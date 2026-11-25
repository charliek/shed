package refserver

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/charliek/shed/internal/config"
	"github.com/charliek/shed/internal/delivery"
	"github.com/charliek/shed/internal/directory"
)

// shed-server's hosted source (internal/directory's Runtime.Deliver, under
// internal/delivery's hosted source) against the reference service over
// loopback: the one token request ten first deliveries share, the five-minute
// memory of a refusal of deliveries (at the token request and at the call),
// what clears it, and a delivery cut short by its shed's stop or delete.

// hostedCounts counts what reached the service: delivery token requests and
// calls. tokenDelay holds each delivery token request that long first.
type hostedCounts struct {
	tokens, calls atomic.Int64
	tokenDelay    time.Duration
	// slow, when set, is a client id whose delivery token requests are held
	// slowDelay before they are answered.
	slow      atomic.Value
	slowDelay time.Duration
}

// assertionIssuer is the iss of a token request's client assertion.
func assertionIssuer(form url.Values) string {
	parts := strings.Split(form.Get("client_assertion"), ".")
	if len(parts) != 3 {
		return ""
	}
	claims, _ := base64.RawURLEncoding.DecodeString(parts[1])
	var c struct {
		Iss string `json:"iss"`
	}
	_ = json.Unmarshal(claims, &c)
	return c.Iss
}

func (h *hostedCounts) wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case pathToken:
			body, _ := io.ReadAll(io.LimitReader(r.Body, maxTokenBody+1))
			r.Body = io.NopCloser(bytes.NewReader(body))
			if form, err := url.ParseQuery(string(body)); err == nil && form.Get("scope") == scopeDeliveries {
				h.tokens.Add(1)
				time.Sleep(h.tokenDelay)
				if slow, _ := h.slow.Load().(string); slow != "" && assertionIssuer(form) == slow {
					time.Sleep(h.slowDelay)
				}
			}
		case pathDeliveries:
			h.calls.Add(1)
		}
		next.ServeHTTP(w, r)
	})
}

// hostedHarness is a server enrolled with a reference service, its runtime
// and the delivery service whose directory users the service answers.
type hostedHarness struct {
	t      *testing.T
	c      *controlServer
	counts *hostedCounts
	rt     *directory.Runtime
	svc    *delivery.Service
	id     string
	state  string
	codes  int
}

func newHostedHarness(t *testing.T, timeout time.Duration, tokenDelay time.Duration) *hostedHarness {
	t.Helper()
	h := &hostedHarness{t: t, counts: &hostedCounts{tokenDelay: tokenDelay}, state: filepath.Join(t.TempDir(), directory.StateDirName)}
	h.c = newControlServerWith(t, h.counts.wrap)
	h.c.write("users.json", "[]")
	h.enrol()
	lookup, err := directory.NewLookup(directory.LookupOptions{LocalAdmins: map[string]config.UserConfig{
		"ops": {Admin: true, Keys: []config.UserKeyConfig{{Key: newSSHKey(t), Label: "desk"}}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	h.rt, err = directory.NewRuntime(directory.RuntimeOptions{
		Lookup: lookup, StateDir: h.state, Issuer: h.c.srv.Issuer(), AllowPlainHTTP: true,
		Refresh: time.Hour, Trust: 2 * time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	h.rt.Load()
	h.svc = delivery.NewService(emptyPin{}, timeout)
	h.svc.SetOrigin(func(string) string { return delivery.OriginDirectory })
	h.svc.SetHosted(h.rt)
	return h
}

// enrol enrols the server (again) with a new code, and writes its enrolment.
func (h *hostedHarness) enrol() {
	h.t.Helper()
	h.codes++
	code := fmt.Sprintf("code-%d", h.codes)
	h.c.write("codes", fmt.Sprintf("srv-%d %s\n", h.codes, code))
	e, err := directory.Enroll(context.Background(), directory.EnrolRequest{
		Issuer: h.c.srv.Issuer(), Code: code, Name: "build-1.example.test", AllowPlainHTTP: true,
	})
	if err != nil {
		h.t.Fatal(err)
	}
	data, err := e.MarshalFile()
	if err != nil {
		h.t.Fatal(err)
	}
	if err := directory.EnsureStateDir(h.state); err != nil {
		h.t.Fatal(err)
	}
	_ = directory.RemoveStateFile(h.state, directory.EnrolmentFileName)
	if err := directory.WriteStateFile(h.state, directory.EnrolmentFileName, data); err != nil {
		h.t.Fatal(err)
	}
	h.id = e.ClientID
}

// sources writes deliveries.json: each user with one env item.
func (h *hostedHarness) sources(users ...string) {
	h.t.Helper()
	var parts []string
	for _, u := range users {
		parts = append(parts, fmt.Sprintf(`%q:{"items":[{"name":"tok","provider":"api-keys","kind":"env","variable":"EXAMPLE_TOKEN","value":"v-%s","fingerprint":"f"}]}`, u, u))
	}
	h.c.write("deliveries.json", "{"+strings.Join(parts, ",")+"}")
}

func (h *hostedHarness) request(ctx context.Context, user, shed string) (delivery.Result, error) {
	return h.svc.Request(ctx, delivery.Params{Reason: delivery.ReasonCreate, User: user,
		Shed: delivery.RequestShed{Name: shed, ID: "id" + strings.ReplaceAll(shed, "-", ""), Backend: delivery.BackendRemote}})
}

// emptyPin binds no machine.
type emptyPin struct{}

func (emptyPin) DeliveryBound(string) (delivery.SourceMachine, bool) {
	return delivery.SourceMachine{}, false
}
func (emptyPin) DeliveryStream(string) (delivery.Stream, bool) { return nil, false }
func (emptyPin) DeliveryCurrent(string, string, uint64) bool   { return false }

// TestHostedFirstDeliveriesShareOneToken: ten deliveries that start together
// on a server with no delivery token make one token request between them,
// and every one is answered.
func TestHostedFirstDeliveriesShareOneToken(t *testing.T) {
	h := newHostedHarness(t, 10*time.Second, 300*time.Millisecond)
	var users []string
	for i := range 10 {
		users = append(users, fmt.Sprintf("p%d", i))
	}
	h.sources(users...)
	var wg sync.WaitGroup
	results := make([]delivery.Result, len(users))
	for i, u := range users {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, err := h.request(context.Background(), u, "box-"+u)
			if err != nil {
				t.Error(err)
			}
			results[i] = r
		}()
	}
	wg.Wait()
	for i, r := range results {
		if r.Outcome != delivery.OutcomeAccepted || r.Source == nil || r.Source.Kind != delivery.SourceKindService {
			t.Fatalf("delivery %d = %+v", i, r)
		}
	}
	if n := h.counts.tokens.Load(); n != 1 {
		t.Fatalf("%d delivery token requests for ten first deliveries, want 1", n)
	}
	if n := h.counts.calls.Load(); n != 10 {
		t.Fatalf("%d calls, want 10", n)
	}
}

// TestHostedNotDeliveringIsRemembered: a server the service does not let ask
// (invalid_scope on the token request; ten first deliveries share that one
// request) gets not_applicable, and for five minutes makes no request at
// all; `shed users refresh` (Refresh) and a re-enrolment each end the
// memory. The same after INSUFFICIENT_SCOPE on the call (the grant removed
// after the token was issued).
func TestHostedNotDeliveringIsRemembered(t *testing.T) {
	t.Run("invalid_scope", func(t *testing.T) {
		h := newHostedHarness(t, 10*time.Second, 300*time.Millisecond)
		h.sources("ada")
		h.c.write("no-deliveries", h.id+"\n")
		var wg sync.WaitGroup
		for i := range 10 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				r, err := h.request(context.Background(), "ada", fmt.Sprintf("box%d", i))
				if err != nil || r.Outcome != delivery.OutcomeNotRequested || r.Reason != delivery.ReasonNotApplicable || r.Failed() {
					t.Errorf("a refused server's delivery = %+v, %v", r, err)
				}
			}()
		}
		wg.Wait()
		if n := h.counts.tokens.Load(); n != 1 {
			t.Fatalf("%d token requests, want 1", n)
		}
		if !h.rt.NotDelivering() || !h.svc.NotDelivering() {
			t.Fatal("the refusal is not remembered")
		}
		if r, _ := h.request(context.Background(), "ada", "again"); r.Reason != delivery.ReasonNotApplicable {
			t.Fatalf("within the memory = %+v", r)
		}
		if h.counts.tokens.Load() != 1 || h.counts.calls.Load() != 0 {
			t.Fatalf("a request was made within the memory: %d tokens, %d calls", h.counts.tokens.Load(), h.counts.calls.Load())
		}

		// `shed users refresh` forgets it.
		h.c.write("no-deliveries", "")
		if _, err := h.rt.Refresh(context.Background()); err != nil {
			t.Fatal(err)
		}
		if h.rt.NotDelivering() {
			t.Fatal("Refresh did not clear the memory")
		}
		if r, _ := h.request(context.Background(), "ada", "after-refresh"); r.Outcome != delivery.OutcomeAccepted {
			t.Fatalf("after the refresh = %+v", r)
		}

		// A re-enrolment ends it too: the memory belongs to the client id.
		h.c.write("no-deliveries", h.id+"\n")
		h.enrolFresh(t)
	})

	t.Run("insufficient_scope", func(t *testing.T) {
		h := newHostedHarness(t, 10*time.Second, 0)
		h.sources("ada")
		if r, _ := h.request(context.Background(), "ada", "first"); r.Outcome != delivery.OutcomeAccepted {
			t.Fatalf("first = %+v", r)
		}
		// The grant goes after the token was issued: the next call is 403.
		h.c.write("no-deliveries", h.id+"\n")
		if r, _ := h.request(context.Background(), "ada", "second"); r.Outcome != delivery.OutcomeNotRequested || r.Reason != delivery.ReasonNotApplicable {
			t.Fatalf("after the grant went = %+v", r)
		}
		calls := h.counts.calls.Load()
		if !h.rt.NotDelivering() {
			t.Fatal("INSUFFICIENT_SCOPE is not remembered")
		}
		if r, _ := h.request(context.Background(), "ada", "third"); r.Reason != delivery.ReasonNotApplicable || h.counts.calls.Load() != calls || h.counts.tokens.Load() != 1 {
			t.Fatalf("within the memory = %+v (%d calls, %d tokens)", r, h.counts.calls.Load(), h.counts.tokens.Load())
		}
	})
}

// enrolFresh is the re-enrolment half of the memory test: with the memory
// held for the old client id, the server enrols under a new one; the next
// delivery reads the new enrolment and asks again.
func (h *hostedHarness) enrolFresh(t *testing.T) {
	t.Helper()
	if r, _ := h.request(context.Background(), "ada", "refused"); r.Reason != delivery.ReasonNotApplicable || !h.rt.NotDelivering() {
		t.Fatalf("set-up: the refusal = %+v", r)
	}
	tokens := h.counts.tokens.Load()
	h.enrol()
	if r, _ := h.request(context.Background(), "ada", "after-enrol"); r.Outcome != delivery.OutcomeAccepted {
		t.Fatalf("after a re-enrolment = %+v", r)
	}
	if h.counts.tokens.Load() != tokens+1 {
		t.Fatalf("the re-enrolled server did not ask for a token: %d, was %d", h.counts.tokens.Load(), tokens)
	}
}

// TestHostedDeliveryEndsWithItsShed: a shed stopped or deleted during a slow
// reply ends its delivery at once (ErrShedEnded): the call is aborted, the
// shed's slot is free for the next request at once, and the late reply is
// never read. Another shed's request is not touched.
func TestHostedDeliveryEndsWithItsShed(t *testing.T) {
	h := newHostedHarness(t, 5*time.Second, 0)
	h.c.write("deliveries.json", `{"ada":{"mode":"late","items":[]}}`)
	errc := make(chan error, 1)
	start := time.Now()
	go func() {
		_, err := h.request(context.Background(), "ada", "slow")
		errc <- err
	}()
	for h.counts.calls.Load() == 0 {
		if time.Since(start) > 5*time.Second {
			t.Fatal("the call never reached the service")
		}
		time.Sleep(5 * time.Millisecond)
	}
	h.svc.EndShed("slow", "idslow")
	select {
	case err := <-errc:
		if !errors.Is(err, delivery.ErrShedEnded) {
			t.Fatalf("Request = %v, want ErrShedEnded", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the delivery did not end with its shed")
	}
	if time.Since(start) > 4*time.Second {
		t.Fatalf("the delivery ended only after %s", time.Since(start))
	}
	// The slot is free: the same shed is asked for again at once.
	h.sources("ada")
	if r, err := h.request(context.Background(), "ada", "slow"); err != nil || r.Outcome != delivery.OutcomeAccepted {
		t.Fatalf("the next request for the shed = %+v, %v", r, err)
	}
	// After Close, nothing is asked.
	h.svc.Close()
	calls := h.counts.calls.Load()
	if _, err := h.request(context.Background(), "ada", "late"); !errors.Is(err, delivery.ErrServiceClosed) || h.counts.calls.Load() != calls {
		t.Fatalf("after Close = %v (%d calls, was %d)", err, h.counts.calls.Load(), calls)
	}
}

// TestHostedStatusReadKeepsTheMemory: a status read (the sources document's
// not_delivering) between two refused deliveries reports the refusal and
// does not clear it, also when the server enrolled again since its last
// pull (the runtime's last read of the enrolment is older than the one a
// delivery reads): the second delivery still makes no request.
func TestHostedStatusReadKeepsTheMemory(t *testing.T) {
	h := newHostedHarness(t, 10*time.Second, 0)
	h.sources("ada")
	h.enrol() // a new client id, which no pull has read yet
	h.c.write("no-deliveries", h.id+"\n")
	if r, _ := h.request(context.Background(), "ada", "first"); r.Reason != delivery.ReasonNotApplicable {
		t.Fatalf("the refused delivery = %+v", r)
	}
	for i := range 3 {
		if !h.rt.NotDelivering() || !h.svc.NotDelivering() {
			t.Fatalf("status read %d does not report the refusal", i)
		}
	}
	tokens, calls := h.counts.tokens.Load(), h.counts.calls.Load()
	if r, _ := h.request(context.Background(), "ada", "second"); r.Reason != delivery.ReasonNotApplicable ||
		h.counts.tokens.Load() != tokens || h.counts.calls.Load() != calls {
		t.Fatalf("after the status reads = %+v (%d token requests, was %d)", r, h.counts.tokens.Load(), tokens)
	}
	if !h.rt.NotDelivering() {
		t.Fatal("the memory was lost")
	}
}

// TestHostedLateRefusalFromAnOldEnrolment: a refusal that lands after the
// server has enrolled again, from a delivery made under the old enrolment,
// does not replace the new enrolment's refusal memory: the status still
// reports it, and the next delivery still makes no request.
func TestHostedLateRefusalFromAnOldEnrolment(t *testing.T) {
	h := newHostedHarness(t, 10*time.Second, 0)
	h.sources("ada")
	old := h.id
	h.counts.slowDelay = 600 * time.Millisecond
	h.counts.slow.Store(old)
	h.c.write("no-deliveries", old+"\n")
	late := make(chan delivery.Result, 1)
	go func() {
		r, _ := h.request(context.Background(), "ada", "under-old")
		late <- r
	}()
	for h.counts.tokens.Load() == 0 {
		time.Sleep(time.Millisecond)
	}
	h.enrol() // the server enrols again while the old request waits
	h.c.write("no-deliveries", old+"\n"+h.id+"\n")
	if r, _ := h.request(context.Background(), "ada", "under-new"); r.Reason != delivery.ReasonNotApplicable || !h.rt.NotDelivering() {
		t.Fatalf("the new enrolment's refusal = %+v", r)
	}
	if r := <-late; r.Reason != delivery.ReasonNotApplicable {
		t.Fatalf("the old enrolment's late refusal = %+v", r)
	}
	if !h.rt.NotDelivering() {
		t.Fatal("the late refusal of the old enrolment replaced the new one's memory")
	}
	tokens := h.counts.tokens.Load()
	if r, _ := h.request(context.Background(), "ada", "again"); r.Reason != delivery.ReasonNotApplicable || h.counts.tokens.Load() != tokens {
		t.Fatalf("within the new memory = %+v (%d token requests, was %d)", r, h.counts.tokens.Load(), tokens)
	}
}
