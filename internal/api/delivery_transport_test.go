package api

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/charliek/shed/internal/authtoken"
	"github.com/charliek/shed/internal/config"
	"github.com/charliek/shed/internal/delivery"
	"github.com/charliek/shed/internal/delivery/refsource"
	"github.com/charliek/shed/internal/plugin"
	"github.com/charliek/shed/internal/servertls"
	"github.com/charliek/shed/internal/users"
	"github.com/charliek/shed/internal/users/userstest"
)

// The delivery transport, driven over real mtls by the contract's reference
// source (internal/delivery/refsource): the real router, a TLS listener that
// verifies client certificates against a test CA, and certificates the CA
// issues (each with its own serial). The service behind the routes is the
// real one, on the real registry.

// provisionFixtures is the contract's fixture folder.
var provisionFixtures = filepath.Join("..", "..", "tests", "provision-contract", "v1", "fixtures")

// deliveryHarness is an mtls server with the delivery routes, its CA, and
// the users alice, bob and root (an admin).
type deliveryHarness struct {
	t   *testing.T
	s   *Server
	srv *httptest.Server
	ca  servertls.CA
}

func newDeliveryHarness(t *testing.T, timeout time.Duration) *deliveryHarness {
	t.Helper()
	reg := plugin.NewRegistry()
	s := NewServer(nil, &config.ServerConfig{Name: "test-server", Auth: &config.AuthConfig{Mode: config.AuthModeMTLS}}, "", reg, plugin.NewBridge(reg, true))
	s.SetUserLookup(userLookup())
	pinBus(s)
	s.SetDelivery(delivery.NewService(reg, timeout))
	ca := testCA(t)
	srv := httptest.NewUnstartedServer(s.Router())
	srv.TLS = &tls.Config{
		MinVersion: tls.VersionTLS12,
		ClientCAs:  ca.Pool(),
		ClientAuth: tls.RequireAndVerifyClientCert,
		VerifyConnection: servertls.ClientConnectionVerifier(func(leaf *x509.Certificate) bool {
			_, _, ok := CertificateUser(s.users, leaf, time.Now())
			return ok
		}, nil),
	}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return &deliveryHarness{t: t, s: s, srv: srv, ca: ca}
}

// cert is a new credentials certificate for the key fp, issued to user: a
// serial of its own each time.
func (h *deliveryHarness) cert(fp, user, scope string) tls.Certificate {
	return issueClientCertAs(h.t, h.ca, fp, user, scope, time.Hour)
}

// client is an HTTPS client presenting cert, with connections of its own.
func (h *deliveryHarness) client(cert tls.Certificate) *http.Client {
	pool := x509.NewCertPool()
	pool.AddCert(h.srv.Certificate())
	return &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{
		RootCAs: pool, MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{cert},
	}}}
}

// refSource is a reference source running one stream, and what it reports.
type refSource struct {
	t      *testing.T
	events chan refsource.Event
	cancel context.CancelFunc
	done   chan error
	once   sync.Once
	stream string
}

// startSource runs cfg's source for one stream against the harness and
// returns once it has reported the stream open (or refused).
func startSource(t *testing.T, server string, cfg refsource.Config) *refSource {
	t.Helper()
	src := &refSource{t: t, events: make(chan refsource.Event, 512), done: make(chan error, 1)}
	cfg.Server = server
	if cfg.MachineName == "" {
		cfg.MachineName = "ada-laptop"
	}
	cfg.Report = func(e refsource.Event) { src.events <- e }
	rs, err := refsource.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	src.cancel = cancel
	go func() { src.done <- rs.RunStream(ctx) }()
	t.Cleanup(src.stop)
	return src
}

// stop ends the source's stream and waits for it (once).
func (s *refSource) stop() {
	s.once.Do(func() {
		s.cancel()
		select {
		case <-s.done:
		case <-time.After(10 * time.Second):
			s.t.Error("the reference source did not stop")
		}
	})
}

// next returns the next event of kind, skipping others.
func (s *refSource) next(kind string) refsource.Event {
	s.t.Helper()
	timeout := time.After(15 * time.Second)
	for {
		select {
		case e := <-s.events:
			if e.Kind == "stream" {
				s.stream = e.Stream
			}
			if e.Kind == kind {
				return e
			}
		case <-timeout:
			s.t.Fatalf("no %s event from the reference source", kind)
		}
	}
}

// none checks that no event of kind comes within d.
func (s *refSource) none(kind string, d time.Duration) {
	s.t.Helper()
	timeout := time.After(d)
	for {
		select {
		case e := <-s.events:
			if e.Kind == kind {
				s.t.Fatalf("unexpected %s event: %+v", kind, e)
			}
		case <-timeout:
			return
		}
	}
}

// ask runs a delivery request for alice's shed in the background.
func (h *deliveryHarness) ask(p delivery.Params) <-chan delivery.Result {
	ch := make(chan delivery.Result, 1)
	go func() {
		r, err := h.s.delivery.Request(context.Background(), p)
		if err != nil {
			h.t.Errorf("Request: %v", err)
		}
		ch <- r
	}()
	return ch
}

func aliceShed(name string) delivery.Params {
	return delivery.Params{Reason: delivery.ReasonCreate, User: "alice", Shed: delivery.RequestShed{
		Name: name, ID: "id" + strings.ReplaceAll(name, "-", ""), Backend: delivery.BackendFirecracker,
		Repo: &delivery.RequestRepo{URL: "git@git.example.test:example/" + name + ".git", Name: "example/" + name}, ProjectDir: "/home/shed/" + name,
	}}
}

func waitResult(t *testing.T, ch <-chan delivery.Result) delivery.Result {
	t.Helper()
	select {
	case r := <-ch:
		return r
	case <-time.After(20 * time.Second):
		t.Fatal("the delivery request did not end")
	}
	return delivery.Result{}
}

// post sends body as the answer to id from client, naming machine and
// stream, and returns the status, the error code and rule, and the headers.
func post(t *testing.T, client *http.Client, server, machine, stream, id, body string) (int, string, string, http.Header) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, server+delivery.AnswerPathPrefix+id, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(HeaderMachineID, machine)
	req.Header.Set(HeaderMachineName, "ada-laptop")
	if stream != "" {
		req.Header.Set(delivery.HeaderStream, stream)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("posting an answer: %v", err)
	}
	defer resp.Body.Close()
	var e struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
		Rule string `json:"rule"`
	}
	b, _ := io.ReadAll(resp.Body)
	_ = json.Unmarshal(b, &e)
	return resp.StatusCode, e.Error.Code, e.Rule, resp.Header
}

func emptyAnswer(id string) string {
	return fmt.Sprintf(`{"version":1,"id":%q,"items":[],"problems":[]}`, id)
}

// waitNoStream waits until user holds no delivery stream.
func waitNoStream(t *testing.T, s *Server, user string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, ok := s.plugins.DeliveryStream(user); !ok {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s's delivery stream did not end", user)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestDeliveryTransportAccepts: the reference source opens its stream, is
// sent the request as one event, and answers it with its canned items; the
// server accepts it (204) and the service returns the bundle.
func TestDeliveryTransportAccepts(t *testing.T) {
	h := newDeliveryHarness(t, 30*time.Second)
	alice := h.client(h.cert(aliceFP, "alice", authtoken.ScopeCredentials))
	machine := refsource.NewMachineID()
	canned, err := refsource.ParseCanned([]byte(`{"items":[` +
		`{"name":"key","provider":"ssh","kind":"file","stage":1,"base":"home","path":".ssh/id_ed25519","mode":"0600","content":"a2V5","fingerprint":"f1"},` +
		`{"name":"dotenv","provider":"env","kind":"file","stage":2,"base":"project","path":".env","mode":"0600","content":"QT0x","fingerprint":"f2"},` +
		`{"name":"tok","provider":"gh","kind":"env","variable":"GH_TOKEN","value":"tok en$'\"","fingerprint":"f3"}],` +
		`"problems":[{"provider":"aws","code":"signed_out","message":"run aws sso login"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	src := startSource(t, h.srv.URL, refsource.Config{Client: alice, MachineID: machine, Canned: canned})
	src.next("stream")
	if !delivery.ValidID(src.stream) {
		t.Fatalf("stream id %q", src.stream)
	}
	ch := h.ask(aliceShed("widgets"))
	req := src.next("request")
	if req.Shed != "widgets" || req.User != "alice" || req.Stream != src.stream {
		t.Fatalf("request event = %+v", req)
	}
	if ans := src.next("answer"); ans.Status != http.StatusNoContent {
		t.Fatalf("answer = %+v, want 204", ans)
	}
	res := waitResult(t, ch)
	if res.Outcome != delivery.OutcomeAccepted || res.RequestID != req.ID || res.Source == nil || res.Source.MachineID != machine {
		t.Fatalf("result = %+v", res)
	}
	items := res.Bundle.Items()
	if len(items) != 3 || items[2].Value != `tok en$'"` || string(items[0].Content) != "key" || len(res.Bundle.Problems()) != 1 {
		t.Fatalf("bundle = %+v", items)
	}
	if v, _ := h.s.plugins.Sources(context.Background(), "alice"); !v.Delivery || v.Connected[machine] == nil {
		t.Fatalf("sources do not count the delivery stream: %+v", v)
	}
}

// loadRequestFixture reads fixtures/requests/<name>.json's request.
func loadRequestFixture(t *testing.T, name string) *delivery.Request {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(provisionFixtures, "requests", name+".json"))
	if err != nil {
		t.Fatal(err)
	}
	var f struct {
		Request json.RawMessage `json:"request"`
	}
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatal(err)
	}
	req, refusal := delivery.ParseRequest(f.Request)
	if refusal != nil {
		t.Fatalf("request fixture %s: %v", name, refusal)
	}
	return req
}

// TestDeliveryTransportRefuseFixtures: every refuse fixture of the contract,
// sent by the reference source as the answer to a live request shaped like
// the fixture's request, is refused with the fixture's status and rule
// (code answer_refused), and the request ends refused: nothing is accepted.
// An answer over 6 MiB is 413 body_too_large.
func TestDeliveryTransportRefuseFixtures(t *testing.T) {
	h := newDeliveryHarness(t, 30*time.Second)
	alice := h.client(h.cert(aliceFP, "alice", authtoken.ScopeCredentials))
	machine := refsource.NewMachineID()
	dir := filepath.Join(provisionFixtures, "answers", "refuse")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".json") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	if len(names) < 100 {
		t.Fatalf("found %d refuse fixtures, want the contract's", len(names))
	}
	huge := `{"version":1,"id":"` + refsource.FixtureRequestID + `","items":[],"problems":[],"pad":"` + strings.Repeat("a", delivery.MaxBodyBytes) + `"}`
	fixtures := map[string]*refsource.AnswerFixture{
		"(transport) body over 6 MiB": {Name: "body-over-6mib", Request: "create-repo", AnswerText: &huge,
			Expect: refsource.Expect{Result: "refused", Status: http.StatusRequestEntityTooLarge, Rule: string(delivery.RuleBodyTooLarge)}},
	}
	order := []string{"(transport) body over 6 MiB"}
	for _, n := range names {
		f, err := refsource.LoadAnswerFixture(filepath.Join(dir, n))
		if err != nil {
			t.Fatalf("%s: %v", n, err)
		}
		fixtures[n] = f
		order = append(order, n)
	}
	for _, n := range order {
		f := fixtures[n]
		req := loadRequestFixture(t, f.Request)
		src := startSource(t, h.srv.URL, refsource.Config{Client: alice, MachineID: machine, Refuse: f})
		src.next("stream")
		ch := h.ask(delivery.Params{Reason: req.Reason, User: "alice", Shed: req.Shed})
		src.next("request")
		ans := src.next("answer")
		res := waitResult(t, ch)
		if ans.Status != f.Expect.Status || ans.Code != delivery.CodeAnswerRefused || ans.Rule != f.Expect.Rule {
			t.Errorf("%s: answered %d %s %s, want %d answer_refused %s", n, ans.Status, ans.Code, ans.Rule, f.Expect.Status, f.Expect.Rule)
		}
		if res.Outcome != delivery.OutcomeRefused || res.Reason != delivery.ReasonAnswerRefused || string(res.Rule) != f.Expect.Rule || res.Bundle != nil {
			t.Errorf("%s: result %+v, want refused under %s with nothing accepted", n, res, f.Expect.Rule)
		}
		src.stop()
		waitNoStream(t, h.s, "alice")
	}
}

// TestDeliveryTransportBinding: an answer is taken only from the
// certificate, machine and stream its request went out on; a refused answer
// consumes nothing, so the owner's answer is then accepted. A second answer
// is already_answered; a late one deadline_passed.
func TestDeliveryTransportBinding(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  func(h *deliveryHarness) refsource.Config
		// status and code of the misdirected answer
		status int
		code   string
	}{
		{"a second connection", func(h *deliveryHarness) refsource.Config {
			return refsource.Config{SecondConnection: true}
		}, http.StatusForbidden, delivery.CodeNotStreamOwner},
		{"another certificate of the same key", func(h *deliveryHarness) refsource.Config {
			return refsource.Config{AnswerClient: h.client(h.cert(aliceFP, "alice", authtoken.ScopeCredentials))}
		}, http.StatusForbidden, delivery.CodeNotStreamOwner},
		{"another machine", func(h *deliveryHarness) refsource.Config {
			return refsource.Config{AnswerMachineID: refsource.NewMachineID()}
		}, http.StatusForbidden, config.ErrSourceNotBound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newDeliveryHarness(t, 30*time.Second)
			alice := h.client(h.cert(aliceFP, "alice", authtoken.ScopeCredentials))
			machine := refsource.NewMachineID()
			cfg := tc.cfg(h)
			cfg.Client, cfg.MachineID = alice, machine
			src := startSource(t, h.srv.URL, cfg)
			src.next("stream")
			ch := h.ask(aliceShed("widgets"))
			req := src.next("request")
			ans := src.next("answer")
			if ans.Status != tc.status || ans.Code != tc.code {
				t.Fatalf("the misdirected answer = %d %s, want %d %s", ans.Status, ans.Code, tc.status, tc.code)
			}
			// Nothing was consumed: the owner's answer, on its stream, is
			// accepted.
			if st, code, _, _ := post(t, alice, h.srv.URL, machine, src.stream, req.ID, emptyAnswer(req.ID)); st != http.StatusNoContent {
				t.Fatalf("the owner's answer after it = %d %s, want 204", st, code)
			}
			if res := waitResult(t, ch); res.Outcome != delivery.OutcomeAccepted {
				t.Fatalf("result = %+v", res)
			}
		})
	}

	t.Run("a second answer", func(t *testing.T) {
		h := newDeliveryHarness(t, 30*time.Second)
		alice := h.client(h.cert(aliceFP, "alice", authtoken.ScopeCredentials))
		src := startSource(t, h.srv.URL, refsource.Config{Client: alice, MachineID: refsource.NewMachineID(), Twice: true})
		src.next("stream")
		ch := h.ask(aliceShed("widgets"))
		first, second := src.next("answer"), src.next("answer")
		if first.Status != http.StatusNoContent || second.Status != http.StatusConflict || second.Code != delivery.CodeAlreadyAnswered {
			t.Fatalf("answers = %+v, %+v; want 204 then 409 already_answered", first, second)
		}
		if res := waitResult(t, ch); res.Outcome != delivery.OutcomeAccepted {
			t.Fatalf("result = %+v", res)
		}
	})

	t.Run("a late answer", func(t *testing.T) {
		h := newDeliveryHarness(t, time.Second)
		alice := h.client(h.cert(aliceFP, "alice", authtoken.ScopeCredentials))
		src := startSource(t, h.srv.URL, refsource.Config{Client: alice, MachineID: refsource.NewMachineID(), Delay: 2500 * time.Millisecond})
		src.next("stream")
		ch := h.ask(aliceShed("widgets"))
		if res := waitResult(t, ch); res.Outcome != delivery.OutcomeNoAnswer || res.Reason != delivery.ReasonSourceTimeout {
			t.Fatalf("result = %+v, want source_timeout", res)
		}
		if ans := src.next("answer"); ans.Status != http.StatusGone || ans.Code != delivery.CodeDeadlinePassed {
			t.Fatalf("the late answer = %+v, want 410 deadline_passed", ans)
		}
	})
}

// TestDeliveryTransportStreamEnds: a stream that ends fails its pending
// request at once (source_disconnected); its answer is then
// unknown_request; the source's next stream is not sent it again.
func TestDeliveryTransportStreamEnds(t *testing.T) {
	h := newDeliveryHarness(t, time.Minute)
	alice := h.client(h.cert(aliceFP, "alice", authtoken.ScopeCredentials))
	machine := refsource.NewMachineID()
	src := startSource(t, h.srv.URL, refsource.Config{Client: alice, MachineID: machine, Silent: true})
	src.next("stream")
	ch := h.ask(aliceShed("widgets"))
	req := src.next("request")
	start := time.Now()
	src.stop()
	res := waitResult(t, ch)
	if res.Outcome != delivery.OutcomeNoAnswer || res.Reason != delivery.ReasonSourceDisconnected || time.Since(start) > 10*time.Second {
		t.Fatalf("result = %+v after %s, want source_disconnected at once", res, time.Since(start))
	}
	if st, code, _, _ := post(t, alice, h.srv.URL, machine, src.stream, req.ID, emptyAnswer(req.ID)); st != http.StatusNotFound || code != delivery.CodeUnknownRequest {
		t.Fatalf("an answer after the stream ended = %d %s, want 404 unknown_request", st, code)
	}
	waitNoStream(t, h.s, "alice")
	again := startSource(t, h.srv.URL, refsource.Config{Client: alice, MachineID: machine, Silent: true})
	again.next("stream")
	again.none("request", 300*time.Millisecond)
}

// TestDeliveryTransportPinMove: while a request waits on machine A's stream,
// the binding moves to machine B: the request fails, A's stream ends, A's
// answer is refused, and B's stream is then admitted and sent the next
// request.
func TestDeliveryTransportPinMove(t *testing.T) {
	h := newDeliveryHarness(t, time.Minute)
	alice := h.client(h.cert(aliceFP, "alice", authtoken.ScopeCredentials))
	machineA, machineB := refsource.NewMachineID(), refsource.NewMachineID()
	a := startSource(t, h.srv.URL, refsource.Config{Client: alice, MachineID: machineA, Silent: true})
	a.next("stream")
	// B presents itself, and is refused: A is the source.
	b := startSource(t, h.srv.URL, refsource.Config{Client: alice, MachineID: machineB, MachineName: "ada-desk"})
	if e := b.next("refused"); e.Status != http.StatusForbidden || e.Code != config.ErrSourceNotBound {
		t.Fatalf("B's stream = %+v, want 403 SOURCE_NOT_BOUND", e)
	}
	ch := h.ask(aliceShed("widgets"))
	req := a.next("request")
	if _, err := h.s.plugins.Move(context.Background(), "alice", machineB, "alice"); err != nil {
		t.Fatal(err)
	}
	if res := waitResult(t, ch); res.Outcome != delivery.OutcomeNoAnswer || res.Reason != delivery.ReasonSourceDisconnected {
		t.Fatalf("result = %+v, want source_disconnected", res)
	}
	a.next("end")
	if st, code, _, _ := post(t, alice, h.srv.URL, machineA, a.stream, req.ID, emptyAnswer(req.ID)); st != http.StatusForbidden || code != config.ErrSourceNotBound {
		t.Fatalf("A's answer after the move = %d %s, want 403 SOURCE_NOT_BOUND", st, code)
	}
	b2 := startSource(t, h.srv.URL, refsource.Config{Client: alice, MachineID: machineB, MachineName: "ada-desk"})
	b2.next("stream")
	ch = h.ask(aliceShed("widgets"))
	if ans := b2.next("answer"); ans.Status != http.StatusNoContent {
		t.Fatalf("B's answer = %+v", ans)
	}
	if res := waitResult(t, ch); res.Outcome != delivery.OutcomeAccepted || res.Source.MachineID != machineB {
		t.Fatalf("result = %+v", res)
	}
}

// TestDeliveryTransportStreamRefusals: a duplicate stream is 409
// SOURCE_DUPLICATE_STREAM; a control certificate is refused by scope; the
// stream's 200 names it and is no-store, and its keepalive comes; every
// refusal on both routes is no-store; a bound machine that is not connected
// is source_offline at once.
func TestDeliveryTransportStreamRefusals(t *testing.T) {
	prevKeepalive := deliveryKeepaliveInterval
	deliveryKeepaliveInterval = 50 * time.Millisecond
	t.Cleanup(func() { deliveryKeepaliveInterval = prevKeepalive })
	h := newDeliveryHarness(t, time.Minute)
	alice := h.client(h.cert(aliceFP, "alice", authtoken.ScopeCredentials))
	machine := refsource.NewMachineID()

	// The stream, raw.
	req, _ := http.NewRequest(http.MethodGet, h.srv.URL+delivery.StreamPath, nil)
	req.Header.Set(HeaderMachineID, machine)
	req.Header.Set(HeaderMachineName, "ada-laptop")
	req.Header.Set("Last-Event-ID", "x")
	ctx, cancel := context.WithCancel(context.Background())
	resp, err := alice.Do(req.WithContext(ctx))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "text/event-stream" ||
		resp.Header.Get("Cache-Control") != "no-store" || !delivery.ValidID(resp.Header.Get(delivery.HeaderStream)) {
		t.Fatalf("stream = %d %v", resp.StatusCode, resp.Header)
	}
	line, err := bufio.NewReader(resp.Body).ReadString('\n')
	if err != nil || line != ": keepalive\n" {
		t.Fatalf("first line = %q, %v; want the keepalive", line, err)
	}

	dup := startSource(t, h.srv.URL, refsource.Config{Client: alice, MachineID: machine})
	if e := dup.next("refused"); e.Status != http.StatusConflict || e.Code != config.ErrSourceDuplicateStream {
		t.Fatalf("a second stream = %+v, want 409 SOURCE_DUPLICATE_STREAM", e)
	}
	cancel()
	resp.Body.Close()
	waitNoStream(t, h.s, "alice")

	// Bound, not connected: no request, at once.
	r, err := h.s.delivery.Request(context.Background(), aliceShed("widgets"))
	if err != nil || r.Reason != delivery.ReasonSourceOffline || r.Source.MachineID != machine {
		t.Fatalf("Request with the source offline = %+v, %v", r, err)
	}
	// Bob has no bound machine: nothing, no warning.
	p := aliceShed("bobs")
	p.User = "bob"
	if r, err := h.s.delivery.Request(context.Background(), p); err != nil || r.Reason != delivery.ReasonNotApplicable || r.Failed() {
		t.Fatalf("Request for a user with no source = %+v, %v", r, err)
	}

	control := h.client(h.cert(aliceFP, "alice", authtoken.ScopeControl))
	for _, tc := range []struct {
		name          string
		client        *http.Client
		method, path  string
		machine, body string
		status        int
		code          string
	}{
		{"control scope, stream", control, http.MethodGet, delivery.StreamPath, machine, "", 403, config.ErrForbidden},
		{"control scope, answer", control, http.MethodPost, delivery.AnswerPathPrefix + strings.Repeat("a", 32), machine, "{}", 403, config.ErrForbidden},
		{"no machine headers, stream", alice, http.MethodGet, delivery.StreamPath, "", "", 403, config.ErrSourceMachineIDRequired},
		{"no machine headers, answer", alice, http.MethodPost, delivery.AnswerPathPrefix + strings.Repeat("a", 32), "", "{}", 403, config.ErrSourceMachineIDRequired},
		{"another machine, answer", alice, http.MethodPost, delivery.AnswerPathPrefix + strings.Repeat("a", 32), refsource.NewMachineID(), "{}", 403, config.ErrSourceNotBound},
		{"unknown request", alice, http.MethodPost, delivery.AnswerPathPrefix + strings.Repeat("a", 32), machine, "{}", 404, delivery.CodeUnknownRequest},
		{"not a request id", alice, http.MethodPost, delivery.AnswerPathPrefix + "nope", machine, "{}", 404, delivery.CodeUnknownRequest},
	} {
		req, _ := http.NewRequest(tc.method, h.srv.URL+tc.path, strings.NewReader(tc.body))
		if tc.machine != "" {
			req.Header.Set(HeaderMachineID, tc.machine)
			req.Header.Set(HeaderMachineName, url.PathEscape("ada laptop"))
		}
		resp, err := tc.client.Do(req)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		var e config.APIError
		_ = json.NewDecoder(resp.Body).Decode(&e)
		resp.Body.Close()
		if resp.StatusCode != tc.status || e.Error.Code != tc.code || resp.Header.Get("Cache-Control") != "no-store" {
			t.Errorf("%s = %d %s (Cache-Control %q), want %d %s, no-store", tc.name, resp.StatusCode, e.Error.Code, resp.Header.Get("Cache-Control"), tc.status, tc.code)
		}
	}
}

// TestDeliveryRefusesServiceClientsAndScriptKeys: a service client and a
// script key reach neither route (403 FORBIDDEN), and their refusals are
// no-store.
func TestDeliveryRefusesServiceClientsAndScriptKeys(t *testing.T) {
	_, router := clientTestServer(t, newClientFakeBackend(), defaultTestPolicy())
	id := strings.Repeat("a", 32)
	for _, rt := range [][2]string{{http.MethodGet, delivery.StreamPath}, {http.MethodPost, delivery.AnswerPathPrefix + id}} {
		r := do(router, serviceLeaf(testClientName), rt[0], rt[1], emptyAnswer(id))
		if r.code != http.StatusForbidden || r.errCode(t) != config.ErrForbidden || r.header.Get("Cache-Control") != "no-store" {
			t.Errorf("service client %s %s = %d %s %v", rt[0], rt[1], r.code, r.body, r.header)
		}
	}

	scriptFP := testFingerprint("alice-ci")
	lookup := userLookup()
	lookup.AddKey("alice", users.Key{Fingerprint: scriptFP, Source: users.SourceInline, Script: true})
	reg := plugin.NewRegistry()
	s := NewServer(nil, &config.ServerConfig{Name: "test-server", Auth: &config.AuthConfig{Mode: config.AuthModeMTLS}}, "", reg, plugin.NewBridge(reg, true))
	s.SetUserLookup(lookup)
	pinBus(s)
	s.SetDelivery(delivery.NewService(reg, time.Minute))
	router = s.Router()
	script := clientCertNaming(scriptFP, "alice", authtoken.ScopeCredentials, time.Hour)
	for _, rt := range [][2]string{{http.MethodGet, delivery.StreamPath}, {http.MethodPost, delivery.AnswerPathPrefix + id}} {
		r := do(router, script, rt[0], rt[1], emptyAnswer(id))
		if r.code != http.StatusForbidden || r.errCode(t) != config.ErrForbidden || r.header.Get("Cache-Control") != "no-store" {
			t.Errorf("script key %s %s = %d %s", rt[0], rt[1], r.code, r.body)
		}
	}
	if _, ok := s.plugins.Bindings().Get("alice"); ok {
		t.Error("a script key's stream bound a machine")
	}
	// The same user's own key is admitted (control for the test).
	if r := subscribeOnce(router, testClientCert(aliceFP, authtoken.ScopeCredentials, time.Hour), delivery.StreamPath); r.code != http.StatusOK {
		t.Errorf("alice's own key = %d %s", r.code, r.body)
	}
}

// TestDeliveryStreamEndsWhenTheCallerLeavesTheList: on a directory server
// alice's delivery stream is sent a request; a list without alice is
// published; her next request is not sent, her stream ends, and the request
// fails as source_disconnected; her answer is then refused at the door
// (401), the caller being checked afresh at each answer.
func TestDeliveryStreamEndsWhenTheCallerLeavesTheList(t *testing.T) {
	rootLine, _ := directoryTestKey(t)
	_, aliceDirFP := directoryTestKey(t)
	_, bobDirFP := directoryTestKey(t)
	lookup := directoryLookupFor(t, rootLine, map[string]string{"alice": aliceDirFP, "bob": bobDirFP})
	s := busUsersServer(t)
	s.SetUserLookup(lookup)
	s.SetDelivery(delivery.NewService(s.plugins, time.Minute))
	creds := boundCreds(aliceDirFP, "alice")
	ts := sseServer(t, s, map[string]*x509.Certificate{"alice": creds})

	lines := openStream(t, ts, "alice", delivery.StreamPath)
	var st delivery.Stream
	deadline := time.Now().Add(5 * time.Second)
	for st == nil {
		st, _ = s.plugins.DeliveryStream("alice")
		if time.Now().After(deadline) {
			t.Fatal("alice's delivery stream never registered")
		}
		time.Sleep(5 * time.Millisecond)
	}
	first := make(chan delivery.Result, 1)
	go func() { r, _ := s.delivery.Request(context.Background(), aliceShed("box")); first <- r }()
	got := nextData(t, "alice", lines)
	var req delivery.Request
	if err := json.Unmarshal([]byte(got), &req); err != nil || req.Shed.Name != "box" {
		t.Fatalf("alice's first event = %s", got)
	}
	answer := func() apiResp {
		return do(s.Router(), creds, http.MethodPost, delivery.AnswerPathPrefix+req.ID, emptyAnswer(req.ID), delivery.HeaderStream, st.ID())
	}

	installUsers(t, lookup, 2, map[string]string{"bob": bobDirFP})
	second := make(chan delivery.Result, 1)
	go func() { r, _ := s.delivery.Request(context.Background(), aliceShed("box2")); second <- r }()
	if sent := streamEnds(t, "alice", lines); len(sent) != 0 {
		t.Errorf("alice's stream sent %v after she left the list, want nothing", sent)
	}
	for _, ch := range []chan delivery.Result{first, second} {
		if r := waitResult(t, ch); r.Reason != delivery.ReasonSourceDisconnected {
			t.Errorf("a request of the ended stream = %+v, want source_disconnected", r)
		}
	}
	if r := answer(); r.code != http.StatusUnauthorized {
		t.Errorf("alice's answer after she left the list = %d %s, want 401", r.code, r.body)
	}
	if r := do(s.Router(), creds, http.MethodGet, delivery.StreamPath, ""); r.code != http.StatusUnauthorized {
		t.Errorf("alice's new stream after she left the list = %d %s, want 401", r.code, r.body)
	}
}

// openTransportServer is an open-mode server (loopback, no certificate:
// every caller is the implicit user) with the delivery routes.
func openTransportServer(t *testing.T, timeout time.Duration) (*Server, *httptest.Server) {
	t.Helper()
	reg := plugin.NewRegistry()
	s := NewServer(nil, &config.ServerConfig{Name: "test-server", Auth: &config.AuthConfig{Mode: config.AuthModeOpen}}, "", reg, plugin.NewBridge(reg, true))
	s.SetUserLookup(userstest.Implicit())
	pinBus(s)
	s.SetDelivery(delivery.NewService(reg, timeout))
	ts := httptest.NewServer(s.Router())
	t.Cleanup(ts.Close)
	return s, ts
}

// TestDeliveryTransportOpenMode: in open mode the machine alone binds (no
// certificate, so no serial), and the stream still does: an answer from a
// second connection is not_stream_owner; the stream's own is accepted.
func TestDeliveryTransportOpenMode(t *testing.T) {
	s, ts := openTransportServer(t, 30*time.Second)
	machine := refsource.NewMachineID()
	src := startSource(t, ts.URL, refsource.Config{MachineID: machine, SecondConnection: true})
	src.next("stream")
	ch := make(chan delivery.Result, 1)
	p := aliceShed("widgets")
	p.User = users.ImplicitName
	go func() { r, _ := s.delivery.Request(context.Background(), p); ch <- r }()
	req := src.next("request")
	if req.User != "" {
		t.Fatalf("request for the implicit user names %q", req.User)
	}
	if ans := src.next("answer"); ans.Status != http.StatusForbidden || ans.Code != delivery.CodeNotStreamOwner {
		t.Fatalf("a second connection's answer = %+v, want 403 not_stream_owner", ans)
	}
	if st, code, _, h := post(t, http.DefaultClient, ts.URL, machine, src.stream, req.ID, emptyAnswer(req.ID)); st != http.StatusNoContent || h.Get("Cache-Control") != "no-store" {
		t.Fatalf("the stream's own answer = %d %s %v", st, code, h)
	}
	if r := waitResult(t, ch); r.Outcome != delivery.OutcomeAccepted {
		t.Fatalf("result = %+v", r)
	}
}

// TestDeliveryTransportClaimedAnswerLoses: an answer is claimed and its
// body not yet read (the handler paused between the claim and the body)
// when the binding moves or the stream ends: the body is then refused as
// unknown_request, nothing is accepted, and the request ends
// source_disconnected, never with that answer's bundle.
func TestDeliveryTransportClaimedAnswerLoses(t *testing.T) {
	for _, tc := range []struct {
		name string
		end  func(h *deliveryHarness, machineB string)
	}{
		{"the binding moves", func(h *deliveryHarness, machineB string) {
			if _, err := h.s.plugins.Move(context.Background(), "alice", machineB, "alice"); err != nil {
				t.Errorf("move: %v", err)
			}
		}},
		{"the stream ends", func(h *deliveryHarness, _ string) {
			st, ok := h.s.plugins.DeliveryStream("alice")
			if !ok {
				t.Error("alice has no stream to end")
				return
			}
			h.s.plugins.UnregisterDelivery(st.(*plugin.DeliveryStream))
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newDeliveryHarness(t, time.Minute)
			alice := h.client(h.cert(aliceFP, "alice", authtoken.ScopeCredentials))
			machineA, machineB := refsource.NewMachineID(), refsource.NewMachineID()
			a := startSource(t, h.srv.URL, refsource.Config{Client: alice, MachineID: machineA})
			a.next("stream")
			b := startSource(t, h.srv.URL, refsource.Config{Client: alice, MachineID: machineB, MachineName: "ada-desk"})
			b.next("refused")

			var once sync.Once
			deliveryClaimedHook = func() { once.Do(func() { tc.end(h, machineB) }) }
			t.Cleanup(func() { deliveryClaimedHook = nil })

			ch := h.ask(aliceShed("widgets"))
			if ans := a.next("answer"); ans.Status != http.StatusNotFound || ans.Code != delivery.CodeUnknownRequest {
				t.Fatalf("the answer whose request ended while its body was read = %+v, want 404 unknown_request", ans)
			}
			if res := waitResult(t, ch); res.Outcome != delivery.OutcomeNoAnswer || res.Reason != delivery.ReasonSourceDisconnected || res.Bundle != nil {
				t.Fatalf("result = %+v, want source_disconnected", res)
			}
		})
	}
}
