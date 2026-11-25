package directory

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/charliek/shed/internal/delivery"
	"github.com/charliek/shed/internal/delivery/refsource"
)

// The server's half of the provision contract's hosted replies
// (tests/provision-contract/v2/fixtures/hosted/replies): every fixture is
// played by a local service at this package's client, through the delivery
// service's hosted source, and the outcome is held to the fixture's
// expectation. This is section 3b's two tables as the server classifies
// them: the client (status, the code, the size, the echo) and the hosted
// source (the answer rules, the id).

var providedV2 = filepath.Join("..", "..", "tests", "provision-contract", "v2")

// hostedReply is one reply of a fixture.
type hostedReply struct {
	Status      *int              `json:"status"`
	Headers     map[string]string `json:"headers"`
	Body        json.RawMessage   `json:"body"`
	Text        *string           `json:"text"`
	Answer      string            `json:"answer"`
	Generate    string            `json:"generate"`
	Silent      bool              `json:"silent"`
	Unreachable bool              `json:"unreachable"`
}

type hostedReplyFixture struct {
	Name    string        `json:"name"`
	Request string        `json:"request"`
	Token   *hostedReply  `json:"token"`
	Calls   []hostedReply `json:"calls"`
	Expect  struct {
		Result string `json:"result"`
		Reason string `json:"reason"`
		Rule   string `json:"rule"`
	} `json:"expect"`
}

// noPin is a pin with no machine: the hosted source is the only one asked.
type noPin struct{}

func (noPin) DeliveryBound(string) (delivery.SourceMachine, bool) {
	return delivery.SourceMachine{}, false
}
func (noPin) DeliveryStream(string) (delivery.Stream, bool) { return nil, false }
func (noPin) DeliveryCurrent(string, string, uint64) bool   { return false }

// clientHosted is a Client as the hosted source's handle, with no memory.
type clientHosted struct{ c *Client }

func (h clientHosted) Deliver(ctx context.Context, request []byte) (delivery.HostedStatus, []byte) {
	st, body, _ := h.c.Deliver(ctx, request)
	return st, body
}

func (clientHosted) NotDelivering() bool { return false }

// unreachable refuses a dial to the paths it names, as a closed port does,
// and sends every other request on.
type unreachable struct {
	paths map[string]bool
	next  http.RoundTripper
}

func (u *unreachable) RoundTrip(req *http.Request) (*http.Response, error) {
	if u.paths[req.URL.Path] {
		if req.Body != nil {
			_ = req.Body.Close()
		}
		return nil, &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED}
	}
	return u.next.RoundTrip(req)
}

// requestParams is a request fixture's document as delivery parameters.
func requestParams(t *testing.T, name string) delivery.Params {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(providedV2, "fixtures", "requests", name+".json"))
	if err != nil {
		t.Fatal(err)
	}
	var rf struct {
		Request json.RawMessage `json:"request"`
	}
	if err := json.Unmarshal(raw, &rf); err != nil {
		t.Fatal(err)
	}
	req, refusal := delivery.ParseRequest(rf.Request)
	if refusal != nil {
		t.Fatalf("request %s: %v", name, refusal)
	}
	return delivery.Params{Reason: req.Reason, User: req.User, Shed: req.Shed}
}

// validAnswer is accept/typical's body for request id.
func validAnswer(t *testing.T, id string) []byte {
	t.Helper()
	f, err := refsource.LoadAnswerFixture(filepath.Join(providedV2, "fixtures", "answers", "accept", "typical.json"))
	if err != nil {
		t.Fatal(err)
	}
	b, err := f.Body(id)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// play writes reply r for a request whose id is id and that presented
// bearer.
func play(t *testing.T, w http.ResponseWriter, r *http.Request, rep hostedReply, id, bearer string) {
	if rep.Silent {
		<-r.Context().Done()
		return
	}
	for k, v := range rep.Headers {
		w.Header().Set(k, v)
	}
	status := *rep.Status
	switch {
	case rep.Body != nil:
		w.WriteHeader(status)
		_, _ = w.Write(rep.Body)
	case rep.Text != nil:
		w.WriteHeader(status)
		_, _ = w.Write([]byte(*rep.Text))
	case rep.Answer != "":
		f, err := refsource.LoadAnswerFixture(filepath.Join(providedV2, "fixtures", "answers", rep.Answer+".json"))
		if err != nil {
			t.Error(err)
			return
		}
		b, err := f.Body(id)
		if err != nil {
			t.Error(err)
			return
		}
		w.WriteHeader(status)
		_, _ = w.Write(b)
	case rep.Generate == "oversize":
		head := `{"version":2,"id":"` + id + `","items":[],"problems":[],"pad":"`
		pad := delivery.MaxBodyBytes + 1 - len(head) - 2
		w.WriteHeader(status)
		_, _ = w.Write([]byte(head + strings.Repeat("a", pad) + `"}`))
	case rep.Generate == "truncated":
		b := validAnswer(t, id)
		w.Header().Set("Content-Length", fmt.Sprint(len(b)))
		w.WriteHeader(status)
		_, _ = w.Write(b[:len(b)/2])
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		panic(http.ErrAbortHandler)
	case rep.Generate == "echo_bearer":
		item, _ := json.Marshal(map[string]string{"name": "echo", "provider": "echo", "kind": "env", "variable": "ECHO_TOKEN", "value": bearer, "fingerprint": "echo"})
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"version":2,"id":"` + id + `","items":[` + string(item) + `],"problems":[]}`))
	default:
		w.WriteHeader(status)
	}
}

// replyService plays one fixture: its token reply (or a granted delivery
// token, a new one each time) and its calls in order.
type replyService struct {
	t      *testing.T
	f      hostedReplyFixture
	mu     sync.Mutex
	tokens int
	calls  int
}

func (s *replyService) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/oauth2/token":
		s.mu.Lock()
		s.tokens++
		n := s.tokens
		s.mu.Unlock()
		if s.f.Token != nil {
			play(s.t, w, r, *s.f.Token, "", "")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		fmt.Fprintf(w, `{"access_token":"delivery-token-%d","token_type":"bearer","expires_in":600,"scope":"deliveries:request"}`, n)
	case "/v1/deliveries":
		s.mu.Lock()
		i := s.calls
		s.calls++
		s.mu.Unlock()
		var req struct {
			ID string `json:"id"`
		}
		b := new(bytes.Buffer)
		_, _ = b.ReadFrom(r.Body)
		_ = json.Unmarshal(b.Bytes(), &req)
		if i >= len(s.f.Calls) {
			s.t.Errorf("call %d, the fixture has %d", i+1, len(s.f.Calls))
			w.WriteHeader(http.StatusTeapot)
			return
		}
		_, bearer, _ := strings.Cut(r.Header.Get("Authorization"), " ")
		play(s.t, w, r, s.f.Calls[i], req.ID, bearer)
	default:
		http.NotFound(w, r)
	}
}

// TestHostedRepliesClassified runs every hosted reply fixture through the
// client and the hosted source, and holds the outcome to its expectation:
// the result, the failure reason and the rule, the source recorded (the
// service for every request that was made), and how many calls were sent
// (one, and one more after a 401).
func TestHostedRepliesClassified(t *testing.T) {
	dir := filepath.Join(providedV2, "fixtures", "hosted", "replies")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		n++
		raw, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		var f hostedReplyFixture
		if err := json.Unmarshal(raw, &f); err != nil {
			t.Fatalf("%s: %v", e.Name(), err)
		}
		t.Run(f.Name, func(t *testing.T) {
			t.Parallel()
			svc := &replyService{t: t, f: f}
			srv := httptest.NewServer(svc)
			t.Cleanup(srv.Close)
			paths := map[string]bool{}
			timeout := 10 * time.Second
			if f.Token != nil && f.Token.Unreachable {
				paths["/oauth2/token"] = true
			}
			for _, c := range f.Calls {
				if c.Unreachable {
					paths["/v1/deliveries"] = true
				}
				if c.Silent {
					timeout = time.Second
				}
			}
			c, err := NewClient(ClientConfig{
				Enrolment: worldEnrolment(t, srv.URL), AllowPlainHTTP: true,
				baseTransport: &unreachable{paths: paths, next: http.DefaultTransport},
			})
			if err != nil {
				t.Fatal(err)
			}
			ds := delivery.NewService(noPin{}, timeout)
			ds.SetOrigin(func(string) string { return delivery.OriginDirectory })
			ds.SetHosted(clientHosted{c})
			res, err := ds.Request(context.Background(), requestParams(t, f.Request))
			if err != nil {
				t.Fatalf("Request: %v", err)
			}
			got := ""
			switch {
			case res.Outcome == delivery.OutcomeAccepted:
				got = "accepted"
			case res.Outcome == delivery.OutcomeNotRequested && res.Reason == delivery.ReasonNotApplicable:
				got = "not_applicable"
			default:
				got = "failed " + string(res.Reason) + " " + string(res.Rule)
			}
			want := f.Expect.Result
			if want == "failed" {
				want = "failed " + f.Expect.Reason + " " + f.Expect.Rule
			}
			if got != want {
				t.Fatalf("%s: %s, want %s", f.Name, got, want)
			}
			if res.Outcome != delivery.OutcomeNotRequested && (res.Source == nil || res.Source.Kind != delivery.SourceKindService) {
				t.Fatalf("source recorded %+v, want the service", res.Source)
			}
			reached := 0
			for _, c := range f.Calls {
				if !c.Unreachable {
					reached++
				}
			}
			if svc.calls != reached {
				t.Fatalf("%d calls reached the service, want %d", svc.calls, reached)
			}
		})
	}
	if n == 0 {
		t.Fatal("no hosted reply fixture")
	}
}
