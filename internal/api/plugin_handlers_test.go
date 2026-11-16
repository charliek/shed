package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/charliek/shed/internal/authtoken"
	"github.com/charliek/shed/internal/config"
	"github.com/charliek/shed/internal/plugin"
	"github.com/charliek/shed/internal/users/userstest"
)

func newTestServerWithPlugins() *Server {
	reg := plugin.NewRegistry()
	bridge := plugin.NewBridge(reg, false)
	return NewServer(nil, &config.ServerConfig{
		Name:     "test-server",
		HTTPPort: 8080,
	}, "", reg, bridge)
}

// unregisterImplicit ends the implicit user's listener for namespace, as its
// handler's deferred Unregister would when the stream drops.
func unregisterImplicit(srv *Server, namespace string) {
	key := plugin.ListenerKey{Namespace: namespace}
	if l, ok := srv.plugins.Get(key); ok {
		srv.plugins.Unregister(key, l.Token)
	}
}

func TestHandleListListenersEmpty(t *testing.T) {
	srv := newTestServerWithPlugins()
	r := httptest.NewRequest(http.MethodGet, "/api/plugins/listeners", nil)
	w := httptest.NewRecorder()
	srv.Router().ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}

	var resp map[string]json.RawMessage
	json.NewDecoder(w.Body).Decode(&resp)
	if string(resp["listeners"]) != "[]" {
		t.Errorf("expected empty listeners, got %s", resp["listeners"])
	}
}

func TestHandleListListenersWithRegistered(t *testing.T) {
	srv := newTestServerWithPlugins()
	srv.plugins.Register(plugin.ListenerKey{Namespace: "op"})

	r := httptest.NewRequest(http.MethodGet, "/api/plugins/listeners", nil)
	w := httptest.NewRecorder()
	srv.Router().ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}

	var resp struct {
		Listeners []plugin.ListenerInfo `json:"listeners"`
	}
	json.NewDecoder(w.Body).Decode(&resp)
	if len(resp.Listeners) != 1 || resp.Listeners[0].Namespace != "op" {
		t.Errorf("unexpected listeners: %+v", resp.Listeners)
	}
}

func TestHandlePluginSubscribeSystemNamespace(t *testing.T) {
	srv := newTestServerWithPlugins()
	r := httptest.NewRequest(http.MethodGet, "/api/plugins/listeners/system:credentials/messages", nil)
	w := httptest.NewRecorder()
	srv.Router().ServeHTTP(w, r)

	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusForbidden)
	}
}

func TestHandlePluginSubscribeDuplicate(t *testing.T) {
	srv := newTestServerWithPlugins()
	srv.plugins.Register(plugin.ListenerKey{Namespace: "op"})

	r := httptest.NewRequest(http.MethodGet, "/api/plugins/listeners/op/messages", nil)
	w := httptest.NewRecorder()
	srv.Router().ServeHTTP(w, r)

	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusConflict)
	}
}

// streamRecorder is a ResponseWriter for a streaming handler that a test
// reads while the handler is still writing: it is safe for both at once,
// and waitFor blocks until the body holds what the test expects.
type streamRecorder struct {
	mu     sync.Mutex
	header http.Header
	body   strings.Builder
	wrote  chan struct{} // signalled on every write
}

func newStreamRecorder() *streamRecorder {
	return &streamRecorder{header: http.Header{}, wrote: make(chan struct{}, 1)}
}

func (r *streamRecorder) Header() http.Header { return r.header }
func (r *streamRecorder) WriteHeader(int)     {}
func (r *streamRecorder) Flush()              {}

func (r *streamRecorder) Write(p []byte) (int, error) {
	r.mu.Lock()
	r.body.Write(p)
	r.mu.Unlock()
	select {
	case r.wrote <- struct{}{}:
	default:
	}
	return len(p), nil
}

func (r *streamRecorder) String() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.body.String()
}

// waitFor returns once the body satisfies ok.
func (r *streamRecorder) waitFor(t *testing.T, what string, ok func(body string) bool) {
	t.Helper()
	for !ok(r.String()) {
		select {
		case <-r.wrote:
		case <-time.After(10 * time.Second):
			t.Fatalf("the stream never held %s; it holds %q", what, r.String())
		}
	}
}

// subscribeInBackground runs the subscribe handler for namespace on rec and
// returns once its listener is registered; done closes when it returns.
func subscribeInBackground(t *testing.T, srv *Server, namespace string, rec *streamRecorder) (done chan struct{}) {
	t.Helper()
	done = make(chan struct{})
	go func() {
		defer close(done)
		r := httptest.NewRequest(http.MethodGet, "/api/plugins/listeners/"+namespace+"/messages", nil)
		r.Header.Set("Accept", "text/event-stream")
		srv.Router().ServeHTTP(rec, r)
	}()
	waitListener(t, srv, plugin.ListenerKey{Namespace: namespace})
	return done
}

func TestHandlePluginSubscribeSSE(t *testing.T) {
	srv := newTestServerWithPlugins()

	// Register a shed so we can publish to it
	gen := srv.bridge.RegisterShed("dev", &plugin.ShedConn{
		Name:    "dev",
		Backend: "vz",
		Server:  "test",
		Send:    func(env *plugin.Envelope) error { return nil },
	})

	recorder := newStreamRecorder()
	done := subscribeInBackground(t, srv, "op", recorder)

	// Publish a message
	env := plugin.NewEnvelope("op", plugin.MessageTypeRequest, json.RawMessage(`{"cmd":"test"}`))
	env.Shed = &plugin.ShedInfo{Name: "dev", Backend: "vz", Server: "test"}
	if err := srv.plugins.Publish(context.Background(), "", gen, env); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	// Once the stream has sent it, unregister to close the handler.
	recorder.waitFor(t, "the published message", func(body string) bool {
		return strings.Contains(body, "event: message") && strings.Contains(body, `"namespace":"op"`)
	})
	unregisterImplicit(srv, "op")
	<-done

	if !strings.Contains(recorder.String(), `"id":"`+env.ID+`"`) {
		t.Errorf("the stream did not send the request under the server's ID %s: %s", env.ID, recorder.String())
	}
	if recorder.Header().Get("Content-Type") != "text/event-stream" {
		t.Errorf("Content-Type = %q, want text/event-stream", recorder.Header().Get("Content-Type"))
	}
}

func TestHandlePluginSubscribeKeepalive(t *testing.T) {
	// Shorten the keepalive so an idle subscription emits several pings quickly.
	prev := sseKeepaliveInterval
	sseKeepaliveInterval = 20 * time.Millisecond
	defer func() { sseKeepaliveInterval = prev }()

	srv := newTestServerWithPlugins()
	recorder := newStreamRecorder()
	done := subscribeInBackground(t, srv, "op", recorder)

	// Stay idle (publish nothing) until two keepalive pings have been sent,
	// then close the handler by unregistering.
	recorder.waitFor(t, "two idle keepalive pings", func(body string) bool {
		return strings.Count(body, ": keepalive") >= 2
	})
	unregisterImplicit(srv, "op")
	<-done
}

// TestSubscribeStreamSendsNothingDroppedSinceItWasQueued: what a subscription
// sends is checked when its stream takes it from the queue, not when it was
// queued. A request queued for a machine whose stream is busy, whose shed is
// then assigned to another user, is never sent to that machine: the stream's
// next message is the one published after it.
func TestSubscribeStreamSendsNothingDroppedSinceItWasQueued(t *testing.T) {
	s := busUsersServer(t)
	gen := s.bridge.RegisterShed("box", &plugin.ShedConn{User: "alice", CreatedAt: busT1, Send: (&sendCounter{}).send})
	other := s.bridge.RegisterShed("alices", &plugin.ShedConn{User: "alice", CreatedAt: busT1, Send: (&sendCounter{}).send})
	// alice's listener exists, and its stream is not reading yet: what is
	// published now sits in its queue.
	l, err := s.plugins.Register(plugin.ListenerKey{User: "alice", Namespace: "ssh-agent"})
	if err != nil {
		t.Fatal(err)
	}
	dropped := publishFromShed(t, s, "box", gen)
	if !s.bridge.SetShedUser("box", busT1, "bob") {
		t.Fatal("SetShedUser refused")
	}
	kept := publishFromShed(t, s, "alices", other)

	// The stream now reads its queue, as handlePluginSubscribe does.
	var sent []string
	for len(l.Messages) > 0 {
		if env := l.Take(<-l.Messages); env != nil {
			sent = append(sent, env.ID)
		}
	}
	if len(sent) != 1 || sent[0] != kept {
		t.Fatalf("alice's stream sent %v, want only %s (not %s, of the shed now bob's)", sent, kept, dropped)
	}
}

func TestHandlePluginRespondMissingShed(t *testing.T) {
	srv := newTestServerWithPlugins()
	body := `{"namespace":"op","type":"response","payload":{}}`
	r := httptest.NewRequest(http.MethodPost, "/api/plugins/listeners/op/respond", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.Router().ServeHTTP(w, r)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusBadRequest)
	}
}

// TestHandlePluginRespondUnmatchedRefusedInOpenMode: on an open-mode server a
// response that matches no pending request is refused, exactly as on an
// enforced one, whether or not a shed is connected under the name: it is
// never delivered by shed name. (Before, open mode delivered it to whatever
// held the name, so retrying a stale answer reached a replacement shed.)
func TestHandlePluginRespondUnmatchedRefusedInOpenMode(t *testing.T) {
	srv := newTestServerWithPlugins()
	sent := 0
	srv.bridge.RegisterShed("dev", &plugin.ShedConn{Send: func(*plugin.Envelope) error { sent++; return nil }})
	for _, shed := range []string{"unknown", "dev"} {
		body := fmt.Sprintf(`{"namespace":"op","type":"response","in_reply_to":"abc","final":true,"shed":{"name":%q},"payload":{}}`, shed)
		r := httptest.NewRequest(http.MethodPost, "/api/plugins/listeners/op/respond", strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		srv.Router().ServeHTTP(w, r)
		if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), `"FORBIDDEN"`) {
			t.Fatalf("unmatched response to %s = %d %s, want 403 FORBIDDEN", shed, w.Code, w.Body.String())
		}
	}
	if sent != 0 {
		t.Fatalf("an unmatched response was delivered %d times", sent)
	}
}

// TestHandlePluginRespondSuccess: in open mode, the answer to a request the
// implicit user's listener was sent is delivered to the shed and its payload
// is unchanged.
func TestHandlePluginRespondSuccess(t *testing.T) {
	srv := newTestServerWithPlugins()

	var sent *plugin.Envelope
	gen := srv.bridge.RegisterShed("dev", &plugin.ShedConn{
		Name: "dev",
		Send: func(env *plugin.Envelope) error {
			sent = env
			return nil
		},
	})
	if _, err := srv.plugins.Register(plugin.ListenerKey{Namespace: "op"}); err != nil {
		t.Fatal(err)
	}
	req := plugin.NewEnvelope("op", plugin.MessageTypeRequest, nil)
	guestID := req.ID
	if err := srv.bridge.PublishToHost(context.Background(), "dev", gen, req); err != nil {
		t.Fatal(err)
	}

	// The listener answers with the ID it was sent (the server's, now
	// req.ID); the shed is written the answer to the ID its guest chose.
	body := fmt.Sprintf(`{"namespace":"op","type":"response","in_reply_to":%q,"final":true,"shed":{"name":"dev"},"payload":{"result":"ok"}}`, req.ID)
	r := httptest.NewRequest(http.MethodPost, "/api/plugins/listeners/op/respond", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.Router().ServeHTTP(w, r)

	if w.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want %d; body = %s", w.Code, http.StatusNoContent, w.Body.String())
	}

	if sent == nil {
		t.Fatal("expected message to be sent to shed")
	}
	if sent.InReplyTo != guestID || req.ID == guestID || string(sent.Payload) != `{"result":"ok"}` {
		t.Errorf("sent %+v, want the answer to the guest's %s (the listener answered %s)", sent, guestID, req.ID)
	}
}

func TestHandlePluginRespondOwnershipEnforced(t *testing.T) {
	srv := newTestServerWithPlugins()
	fp := testFingerprint("bus")
	srv.cfg.Auth = &config.AuthConfig{Mode: config.AuthModeMTLS}
	srv.SetUserLookup(userstest.Implicit(fp))
	creds := testClientCert(fp, authtoken.ScopeCredentials, time.Hour)
	gen := srv.bridge.RegisterShed("dev", &plugin.ShedConn{
		Name: "dev",
		Send: func(*plugin.Envelope) error { return nil },
	})
	if _, err := srv.plugins.Register(plugin.ListenerKey{Namespace: "op"}); err != nil {
		t.Fatal(err)
	}

	post := func(inReplyTo string) int {
		body := fmt.Sprintf(`{"namespace":"op","type":"response","in_reply_to":%q,"final":true,"shed":{"name":"dev"}}`, inReplyTo)
		r := httptest.NewRequest(http.MethodPost, "/api/plugins/listeners/op/respond", strings.NewReader(body))
		withClientCert(r, creds)
		w := httptest.NewRecorder()
		srv.Router().ServeHTTP(w, r)
		return w.Code
	}

	// A forged response (no pending request was dispatched) is rejected.
	if code := post("forged-id"); code != http.StatusForbidden {
		t.Errorf("forged respond under enforce = %d, want 403", code)
	}

	// After a real request is dispatched (records pending), the matching
	// response is accepted.
	req := plugin.NewEnvelope("op", plugin.MessageTypeRequest, nil)
	if err := srv.bridge.PublishToHost(context.Background(), "dev", gen, req); err != nil {
		t.Fatalf("PublishToHost: %v", err)
	}
	if code := post(req.ID); code != http.StatusNoContent {
		t.Errorf("owned respond under enforce = %d, want 204", code)
	}
}

func TestHandleListPluginShedsEmpty(t *testing.T) {
	srv := newTestServerWithPlugins()
	r := httptest.NewRequest(http.MethodGet, "/api/plugins/sheds", nil)
	w := httptest.NewRecorder()
	srv.Router().ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}
}

func TestHandleListPluginShedsWithConnected(t *testing.T) {
	srv := newTestServerWithPlugins()
	srv.bridge.RegisterShed("dev", &plugin.ShedConn{
		Name:    "dev",
		Backend: "vz",
		Server:  "mini",
		Send:    func(env *plugin.Envelope) error { return nil },
	})

	r := httptest.NewRequest(http.MethodGet, "/api/plugins/sheds", nil)
	w := httptest.NewRecorder()
	srv.Router().ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}

	var resp struct {
		Sheds []plugin.ShedConnInfo `json:"sheds"`
	}
	json.NewDecoder(w.Body).Decode(&resp)
	if len(resp.Sheds) != 1 || resp.Sheds[0].Name != "dev" {
		t.Errorf("unexpected sheds: %+v", resp.Sheds)
	}
}

// TestOpenModeTracksPendingRequests: an open-mode server tracks pending
// requests too, so a response to one is bound to the shed connection the
// request came from: a shed replaced under its name between the match and
// the delivery is not sent it, where an untracked response would have gone
// to whatever holds the name. The request went with its connection, so the
// response is refused as unmatched, the first time and on a retry.
func TestOpenModeTracksPendingRequests(t *testing.T) {
	srv := newTestServerWithPlugins()
	gen := srv.bridge.RegisterShed("dev", &plugin.ShedConn{Send: func(*plugin.Envelope) error { return nil }})
	if _, err := srv.plugins.Register(plugin.ListenerKey{Namespace: "op"}); err != nil {
		t.Fatal(err)
	}
	req := plugin.NewEnvelope("op", plugin.MessageTypeRequest, nil)
	if err := srv.bridge.PublishToHost(context.Background(), "dev", gen, req); err != nil {
		t.Fatal(err)
	}
	replacementSent := 0
	respondValidatedHook = func() {
		srv.bridge.UnregisterShed("dev")
		srv.bridge.RegisterShed("dev", &plugin.ShedConn{Send: func(*plugin.Envelope) error { replacementSent++; return nil }})
	}
	t.Cleanup(func() { respondValidatedHook = nil })

	body := fmt.Sprintf(`{"namespace":"op","type":"response","in_reply_to":%q,"final":true,"shed":{"name":"dev"}}`, req.ID)
	r := httptest.NewRequest(http.MethodPost, "/api/plugins/listeners/op/respond", strings.NewReader(body))
	w := httptest.NewRecorder()
	srv.Router().ServeHTTP(w, r)
	if w.Code != http.StatusForbidden {
		t.Fatalf("respond = %d %s, want 403 (its request went with its connection)", w.Code, w.Body.String())
	}
	if replacementSent != 0 {
		t.Fatalf("the replacement shed was sent the response %d times", replacementSent)
	}

	// Retrying the identical answer: it matches no pending request now, and
	// an open-mode server refuses it as an enforced one does, rather than
	// delivering it by name to the replacement.
	respondValidatedHook = nil
	r = httptest.NewRequest(http.MethodPost, "/api/plugins/listeners/op/respond", strings.NewReader(body))
	w = httptest.NewRecorder()
	srv.Router().ServeHTTP(w, r)
	if w.Code != http.StatusForbidden {
		t.Fatalf("retried respond = %d %s, want 403", w.Code, w.Body.String())
	}
	if replacementSent != 0 {
		t.Fatalf("the retried stale answer reached the replacement shed %d times", replacementSent)
	}
}
