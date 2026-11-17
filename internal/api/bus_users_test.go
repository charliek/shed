package api

import (
	"bufio"
	"context"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/charliek/shed/internal/authtoken"
	"github.com/charliek/shed/internal/config"
	"github.com/charliek/shed/internal/egress"
	"github.com/charliek/shed/internal/plugin"
	"github.com/charliek/shed/internal/users/userstest"
)

// The credential bus and the egress stream by user, driven with named users
// through the userstest double (alice and bob are users, root is an admin;
// see userLookup).

var (
	busT1 = time.Date(2026, 11, 14, 9, 0, 0, 0, time.UTC)
	busT2 = time.Date(2026, 11, 14, 10, 0, 0, 0, time.UTC)
)

// sendCounter counts the responses one registered shed connection is sent.
type sendCounter struct {
	mu   sync.Mutex
	sent []*plugin.Envelope
}

func (c *sendCounter) send(env *plugin.Envelope) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sent = append(c.sent, env)
	return nil
}

func (c *sendCounter) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.sent)
}

// busUsersServer is an mtls server with a user list, a registry and a bridge
// that routes by user.
func busUsersServer(t *testing.T) *Server {
	t.Helper()
	reg := plugin.NewRegistry()
	s := NewServer(nil, &config.ServerConfig{
		Name:   "test-server",
		Auth:   &config.AuthConfig{Mode: config.AuthModeMTLS},
		Egress: &config.EgressConfig{Enabled: true},
	}, "", reg, plugin.NewBridge(reg, true))
	s.SetUserLookup(userLookup())
	pinBus(s)
	return s
}

func credsAs(fp string) *x509.Certificate {
	return testClientCert(fp, authtoken.ScopeCredentials, time.Hour)
}

// respondBody is a final response to request id of shed.
func respondBody(shed, id string) string {
	return fmt.Sprintf(`{"namespace":"ssh-agent","type":"response","in_reply_to":%q,"final":true,"shed":{"name":%q},"payload":{}}`, id, shed)
}

// publishFromShed publishes a request from the shed's guest through the
// bridge, as its message channel would, and returns the ID the server gave
// it: the one its listener is sent, and answers with.
func publishFromShed(t *testing.T, s *Server, shed string, gen uint64) string {
	t.Helper()
	req := plugin.NewEnvelope("ssh-agent", plugin.MessageTypeRequest, nil)
	if err := s.bridge.PublishToHost(context.Background(), shed, gen, req); err != nil {
		t.Fatalf("PublishToHost(%s): %v", shed, err)
	}
	return req.ID
}

// sseServer serves s over a real listener, presenting the certificate named
// by each request's X-Test-Cert header as its TLS peer (the middleware reads
// only r.TLS), so a test can hold streams open for several identities.
func sseServer(t *testing.T, s *Server, certs map[string]*x509.Certificate) *httptest.Server {
	t.Helper()
	router := s.Router()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if c, ok := certs[r.Header.Get("X-Test-Cert")]; ok {
			withClientCert(r, c)
			withTestMachine(r, c.Subject.CommonName)
		}
		router.ServeHTTP(w, r)
	}))
	t.Cleanup(ts.Close)
	return ts
}

// openStream opens a GET SSE stream at path as who and returns a channel of
// its data lines. The stream ends with the test.
func openStream(t *testing.T, ts *httptest.Server, who, path string) <-chan string {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+path, nil)
	req.Header.Set("X-Test-Cert", who)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s opening %s: %v", who, path, err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("%s opening %s = %d, want 200", who, path, resp.StatusCode)
	}
	lines := make(chan string, 64)
	go func() {
		defer resp.Body.Close()
		defer close(lines)
		br := bufio.NewReader(resp.Body)
		for {
			line, err := br.ReadString('\n')
			if err != nil {
				return
			}
			if data, ok := strings.CutPrefix(line, "data: "); ok {
				lines <- strings.TrimSpace(data)
			}
		}
	}()
	return lines
}

// nextData returns the next data line of a stream.
func nextData(t *testing.T, who string, lines <-chan string) string {
	t.Helper()
	select {
	case l, ok := <-lines:
		if !ok {
			t.Fatalf("%s's stream ended", who)
		}
		return l
	case <-time.After(3 * time.Second):
		t.Fatalf("%s's stream sent nothing", who)
	}
	return ""
}

// waitListener waits until key is registered (a subscribe has passed its
// Register).
func waitListener(t *testing.T, s *Server, key plugin.ListenerKey) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, ok := s.plugins.Get(key); ok {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("listener %+v never registered", key)
}

// TestBusTwoUsersHoldTheSameNamespace: alice's and bob's machines subscribe
// to ssh-agent at once; each is sent only its own user's sheds' requests; a
// second subscription of one user is still 409.
func TestBusTwoUsersHoldTheSameNamespace(t *testing.T) {
	s := busUsersServer(t)
	boxGen := s.bridge.RegisterShed("box", &plugin.ShedConn{User: "alice", CreatedAt: busT1, Send: (&sendCounter{}).send})
	bobsGen := s.bridge.RegisterShed("bobs", &plugin.ShedConn{User: "bob", CreatedAt: busT1, Send: (&sendCounter{}).send})
	ts := sseServer(t, s, map[string]*x509.Certificate{"alice": credsAs(aliceFP), "bob": credsAs(bobFP)})

	const path = "/api/plugins/listeners/ssh-agent/messages"
	alice := openStream(t, ts, "alice", path)
	bob := openStream(t, ts, "bob", path)
	waitListener(t, s, plugin.ListenerKey{User: "alice", Namespace: "ssh-agent"})
	waitListener(t, s, plugin.ListenerKey{User: "bob", Namespace: "ssh-agent"})

	if r := do(s.Router(), credsAs(aliceFP), http.MethodGet, path, ""); r.code != http.StatusConflict || r.errCode(t) != config.ErrSourceDuplicateStream {
		t.Fatalf("alice's second subscription = %d %s, want 409 SOURCE_DUPLICATE_STREAM", r.code, r.body)
	}

	// box's request first, then bobs': bob's first message must be his own,
	// so box's never reached him.
	boxReq := publishFromShed(t, s, "box", boxGen)
	bobsReq := publishFromShed(t, s, "bobs", bobsGen)
	if got := nextData(t, "bob", bob); !strings.Contains(got, bobsReq) || strings.Contains(got, boxReq) {
		t.Fatalf("bob's first message = %s, want only bobs' request %s", got, bobsReq)
	}
	if got := nextData(t, "alice", alice); !strings.Contains(got, boxReq) || !strings.Contains(got, `"name":"box"`) {
		t.Fatalf("alice's first message = %s, want box's request %s", got, boxReq)
	}
	// And bobs' request never reaches alice: a later one of box's is her next.
	boxReq2 := publishFromShed(t, s, "box", boxGen)
	if got := nextData(t, "alice", alice); !strings.Contains(got, boxReq2) {
		t.Fatalf("alice's second message = %s, want box's second request %s (not bobs')", got, boxReq2)
	}
}

// TestBusRespondActsAsTheCallersUser: only the shed's user may answer its
// request. Another user and an admin get the same 403 a forged response or a
// shed that does not exist gets, and the request stays pending for its user.
func TestBusRespondActsAsTheCallersUser(t *testing.T) {
	s := busUsersServer(t)
	router := s.Router()
	box := &sendCounter{}
	gen := s.bridge.RegisterShed("box", &plugin.ShedConn{User: "alice", CreatedAt: busT1, Send: box.send})
	listen(t, s, "alice", "ssh-agent", aliceFP)
	id := publishFromShed(t, s, "box", gen)
	const path = "/api/plugins/listeners/ssh-agent/respond"

	refused := func(who, fp, body string) {
		t.Helper()
		r := do(router, credsAs(fp), http.MethodPost, path, body)
		if r.code != http.StatusForbidden || r.errCode(t) != "FORBIDDEN" ||
			r.errMessage(t) != "response does not match a pending request for this namespace" {
			t.Fatalf("%s = %d %s, want the 403 a forged response gets", who, r.code, r.body)
		}
	}
	refused("bob answering alice's request", bobFP, respondBody("box", id))
	refused("root (an admin) answering alice's request", rootFP, respondBody("box", id))
	refused("bob answering for a shed that does not exist", bobFP, respondBody("nope", id))
	refused("alice answering for a shed that does not exist", aliceFP, respondBody("nope", "made-up"))
	refused("alice forging a request id", aliceFP, respondBody("box", "made-up"))
	if n := box.count(); n != 0 {
		t.Fatalf("box was sent %d responses by refused callers", n)
	}

	if r := do(router, credsAs(aliceFP), http.MethodPost, path, respondBody("box", id)); r.code != http.StatusNoContent {
		t.Fatalf("alice answering her request = %d %s, want 204", r.code, r.body)
	}
	if n := box.count(); n != 1 {
		t.Fatalf("box was sent %d responses, want alice's one", n)
	}
	// Answered once: a replay is refused.
	refused("alice replaying her answer", aliceFP, respondBody("box", id))
}

// TestBusRespondForAShedReplacedAfterValidation: a response matched to a
// request of one connection of a name is dropped when the shed is deleted
// and created again under the name between the match and the delivery
// (forced with the handler's hook), whoever the new shed belongs to: the
// request went with its connection, so the response is refused as any
// response that matches nothing is.
func TestBusRespondForAShedReplacedAfterValidation(t *testing.T) {
	for _, newUser := range []string{"alice", "bob"} {
		t.Run("recreated_by_"+newUser, func(t *testing.T) {
			s := busUsersServer(t)
			old := &sendCounter{}
			replacement := &sendCounter{}
			gen := s.bridge.RegisterShed("box", &plugin.ShedConn{User: "alice", CreatedAt: busT1, Send: old.send})
			listen(t, s, "alice", "ssh-agent", aliceFP)
			id := publishFromShed(t, s, "box", gen)

			hooked := false
			respondValidatedHook = func() {
				hooked = true
				s.bridge.UnregisterShed("box")
				s.bridge.RegisterShed("box", &plugin.ShedConn{User: newUser, CreatedAt: busT2, Send: replacement.send})
			}
			t.Cleanup(func() { respondValidatedHook = nil })

			r := do(s.Router(), credsAs(aliceFP), http.MethodPost, "/api/plugins/listeners/ssh-agent/respond", respondBody("box", id))
			if !hooked {
				t.Fatal("the response was never validated (hook not reached)")
			}
			if r.code != http.StatusForbidden || r.errCode(t) != "FORBIDDEN" {
				t.Fatalf("respond = %d %s, want 403 FORBIDDEN", r.code, r.body)
			}
			if n := replacement.count(); n != 0 {
				t.Fatalf("the replacement shed was sent %d responses meant for the old one", n)
			}
			if n := old.count(); n != 0 {
				t.Fatalf("the unregistered connection was sent %d responses", n)
			}
		})
	}
}

// TestBusListingsActAsTheCallersUser: the shed and listener listings show a
// user their own rows and an admin every row.
func TestBusListingsActAsTheCallersUser(t *testing.T) {
	s := busUsersServer(t)
	router := s.Router()
	s.bridge.RegisterShed("box", &plugin.ShedConn{User: "alice", CreatedAt: busT1, Send: (&sendCounter{}).send})
	s.bridge.RegisterShed("bobs", &plugin.ShedConn{User: "bob", CreatedAt: busT1, Send: (&sendCounter{}).send})
	s.bridge.RegisterShed("loose", &plugin.ShedConn{CreatedAt: busT1, Send: (&sendCounter{}).send})
	listen(t, s, "alice", "ssh-agent", aliceFP)
	listen(t, s, "bob", "ssh-agent", bobFP)

	sheds := func(fp string) string {
		t.Helper()
		r := do(router, credsAs(fp), http.MethodGet, "/api/plugins/sheds", "")
		if r.code != http.StatusOK {
			t.Fatalf("sheds = %d %s", r.code, r.body)
		}
		var resp struct{ Sheds []plugin.ShedConnInfo }
		if err := json.Unmarshal(r.body, &resp); err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, sh := range resp.Sheds {
			out = append(out, sh.Name+"="+sh.User)
		}
		return strings.Join(out, ",")
	}
	listeners := func(fp string) string {
		t.Helper()
		r := do(router, credsAs(fp), http.MethodGet, "/api/plugins/listeners", "")
		if r.code != http.StatusOK {
			t.Fatalf("listeners = %d %s", r.code, r.body)
		}
		var resp struct{ Listeners []plugin.ListenerInfo }
		if err := json.Unmarshal(r.body, &resp); err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, l := range resp.Listeners {
			out = append(out, l.User+"/"+l.Namespace)
		}
		return strings.Join(out, ",")
	}
	for _, tc := range []struct{ who, fp, sheds, listeners string }{
		{"alice", aliceFP, "box=alice", "alice/ssh-agent"},
		{"bob", bobFP, "bobs=bob", "bob/ssh-agent"},
		{"root", rootFP, "bobs=bob,box=alice,loose=", "alice/ssh-agent,bob/ssh-agent"},
	} {
		if got := sheds(tc.fp); got != tc.sheds {
			t.Errorf("%s's sheds = %q, want %q", tc.who, got, tc.sheds)
		}
		if got := listeners(tc.fp); got != tc.listeners {
			t.Errorf("%s's listeners = %q, want %q", tc.who, got, tc.listeners)
		}
	}
}

// TestBusImplicitUserPinned: on an mtls server with no user list every key is
// the implicit user, and the implicit user has one credential source like
// any other: the first machine to subscribe is bound, a second machine is
// refused naming it (on subscribe and on respond, whichever key it uses), a
// response is accepted from the bound machine whichever of the user's keys
// it comes with (machines match on the id only), and the listings show every
// row.
func TestBusImplicitUserPinned(t *testing.T) {
	reg := plugin.NewRegistry()
	s := NewServer(nil, &config.ServerConfig{Name: "t", Auth: &config.AuthConfig{Mode: config.AuthModeMTLS}}, "", reg, plugin.NewBridge(reg, false))
	fp1, fp2 := testFingerprint("one"), testFingerprint("two")
	s.SetUserLookup(userstest.Implicit(fp1, fp2))
	pinBus(s)
	router := s.Router()
	box := &sendCounter{}
	gen := s.bridge.RegisterShed("box", &plugin.ShedConn{User: "alice", CreatedAt: busT1, Send: box.send}) // a record from a removed user list
	l := listen(t, s, "", "ssh-agent", fp1)

	if r := do(router, credsAs(fp2), http.MethodGet, "/api/plugins/listeners/ssh-agent/messages", ""); r.code != http.StatusForbidden || r.errCode(t) != config.ErrSourceNotBound ||
		!strings.Contains(string(r.body), testMachineFor(fp1).ID) {
		t.Fatalf("a second machine's subscription = %d %s, want 403 SOURCE_NOT_BOUND naming the first", r.code, r.body)
	}
	id := publishFromShed(t, s, "box", gen)
	select {
	case m := <-l.Messages:
		if got := l.Take(m); got == nil || got.ID != id {
			t.Fatalf("the implicit listener received %+v, want request %s", got, id)
		}
	default:
		t.Fatalf("the implicit listener received nothing, want request %s", id)
	}
	if r := do(router, credsAs(fp2), http.MethodPost, "/api/plugins/listeners/ssh-agent/respond", respondBody("box", id)); r.code != http.StatusForbidden || r.errCode(t) != config.ErrSourceNotBound || box.count() != 0 {
		t.Fatalf("respond from the second machine = %d %s (sent %d), want 403 SOURCE_NOT_BOUND and nothing delivered", r.code, r.body, box.count())
	}
	// The bound machine, presenting the other key: accepted.
	m1 := testMachineFor(fp1)
	if r := do(router, credsAs(fp2), http.MethodPost, "/api/plugins/listeners/ssh-agent/respond", respondBody("box", id),
		HeaderMachineID, m1.ID, HeaderMachineName, m1.Name); r.code != http.StatusNoContent || box.count() != 1 {
		t.Fatalf("respond from the bound machine = %d %s (sent %d), want 204 and one delivery", r.code, r.body, box.count())
	}
	if r := do(router, credsAs(fp1), http.MethodGet, "/api/plugins/sheds", ""); !strings.Contains(string(r.body), `"name":"box"`) || strings.Contains(string(r.body), `"user"`) {
		t.Fatalf("implicit sheds listing = %s, want box with no user field", r.body)
	}
}

// TestEgressStreamFollowsTheShedIncarnation: the live egress stream shows a
// user the records of their own sheds only, by the incarnation each record
// came from. While both streams stay open, alice's shed "box" is deleted and
// bob creates a shed under the name: the new shed's records reach bob and
// not alice, and a late record of alice's old shed reaches neither user. An
// admin sees every record.
func TestEgressStreamFollowsTheShedIncarnation(t *testing.T) {
	s := busUsersServer(t)
	audit, err := egress.OpenAuditLog(filepath.Join(t.TempDir(), "egress.jsonl"), 100)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = audit.Close() })
	s.SetEgressAudit(audit)
	s.bridge.RegisterShed("box", &plugin.ShedConn{User: "alice", CreatedAt: busT1, Send: (&sendCounter{}).send})
	s.bridge.RegisterShed("alices", &plugin.ShedConn{User: "alice", CreatedAt: busT1, Send: (&sendCounter{}).send})
	s.bridge.RegisterShed("bobs", &plugin.ShedConn{User: "bob", CreatedAt: busT1, Send: (&sendCounter{}).send})
	ts := sseServer(t, s, map[string]*x509.Certificate{
		"alice": credsAs(aliceFP),
		"bob":   testClientCert(bobFP, authtoken.ScopeControl, time.Hour),
		"root":  credsAs(rootFP),
	})
	alice := openStream(t, ts, "alice", "/api/egress/stream")
	bob := openStream(t, ts, "bob", "/api/egress/stream")
	root := openStream(t, ts, "root", "/api/egress/stream")

	rec := func(shed string, at time.Time, host string) {
		audit.Record(egress.AuditRecord{Time: time.Now(), Shed: shed, ShedCreatedAt: at, Host: host, Verdict: "allow"})
	}
	// expect reads who's next records and checks they are exactly hosts.
	expect := func(who string, lines <-chan string, hosts ...string) {
		t.Helper()
		for _, h := range hosts {
			if got := nextData(t, who, lines); !strings.Contains(got, `"host":"`+h+`"`) {
				t.Fatalf("%s's next record = %s, want host %s", who, got, h)
			}
		}
	}

	rec("box", busT1, "alice-1.example")
	rec("bobs", busT1, "bob-1.example")
	rec("box", time.Time{}, "no-incarnation.example") // admins only
	expect("alice", alice, "alice-1.example")
	expect("bob", bob, "bob-1.example")

	// Name reuse while the streams stay open.
	s.bridge.UnregisterShed("box")
	s.bridge.RegisterShed("box", &plugin.ShedConn{User: "bob", CreatedAt: busT2, Send: (&sendCounter{}).send})
	rec("box", busT2, "bob-2.example")
	rec("box", busT1, "late-old-box.example") // the old incarnation: admins only
	rec("alices", busT1, "alice-2.example")   // marker: alice's next record
	rec("bobs", busT1, "bob-3.example")       // marker: bob's next record after his own
	expect("alice", alice, "alice-2.example")
	expect("bob", bob, "bob-2.example", "bob-3.example")
	expect("root", root, "alice-1.example", "bob-1.example", "no-incarnation.example",
		"bob-2.example", "late-old-box.example", "alice-2.example", "bob-3.example")
}

// TestBusRespondAfterTheShedIsAssignedAway: a response validated for alice
// is refused, not delivered, when her shed is assigned to bob between
// validation and delivery (forced with the handler's hook).
func TestBusRespondAfterTheShedIsAssignedAway(t *testing.T) {
	s := busUsersServer(t)
	box := &sendCounter{}
	gen := s.bridge.RegisterShed("box", &plugin.ShedConn{User: "alice", CreatedAt: busT1, Send: box.send})
	listen(t, s, "alice", "ssh-agent", aliceFP)
	id := publishFromShed(t, s, "box", gen)
	respondValidatedHook = func() {
		if !s.bridge.SetShedUser("box", busT1, "bob") {
			t.Error("SetShedUser refused")
		}
	}
	t.Cleanup(func() { respondValidatedHook = nil })

	r := do(s.Router(), credsAs(aliceFP), http.MethodPost, "/api/plugins/listeners/ssh-agent/respond", respondBody("box", id))
	if r.code != http.StatusForbidden || r.errCode(t) != "FORBIDDEN" {
		t.Fatalf("respond = %d %s, want 403 FORBIDDEN", r.code, r.body)
	}
	if n := box.count(); n != 0 {
		t.Fatalf("alice's answer was delivered to the shed now bob's (%d sends)", n)
	}
}

// TestBusCaller: who a request acts as on the bus.
func TestBusCaller(t *testing.T) {
	tests := []struct {
		name    string
		caller  *Caller
		user    string
		all, ok bool
	}{
		{"no caller", nil, "", false, false},
		{"service client", &Caller{Kind: CallerClient, Name: "nocturn"}, "", false, false},
		{"user with no name", &Caller{Kind: CallerUser}, "", false, false},
		{"implicit user", &Caller{Kind: CallerUser, Admin: true, Implicit: true}, "", true, true},
		{"user", &Caller{Kind: CallerUser, User: "alice"}, "alice", false, true},
		{"admin", &Caller{Kind: CallerUser, User: "root", Admin: true}, "root", true, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/api/plugins/sheds", nil)
			if tc.caller != nil {
				r = r.WithContext(withCaller(r.Context(), *tc.caller))
			}
			user, all, ok := busCaller(r)
			if user != tc.user || all != tc.all || ok != tc.ok {
				t.Fatalf("busCaller = (%q, %v, %v), want (%q, %v, %v)", user, all, ok, tc.user, tc.all, tc.ok)
			}
		})
	}
}
