package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/charliek/shed/internal/authtoken"
	"github.com/charliek/shed/internal/config"
	"github.com/charliek/shed/internal/plugin"
)

func newTestServerWithPlugins() *Server {
	reg := plugin.NewRegistry()
	bridge := plugin.NewBridge(reg)
	return NewServer(nil, &config.ServerConfig{
		Name:     "test-server",
		HTTPPort: 8080,
	}, "", reg, bridge)
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
	srv.plugins.Register("op")

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
	srv.plugins.Register("op")

	r := httptest.NewRequest(http.MethodGet, "/api/plugins/listeners/op/messages", nil)
	w := httptest.NewRecorder()
	srv.Router().ServeHTTP(w, r)

	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusConflict)
	}
}

func TestHandlePluginSubscribeSSE(t *testing.T) {
	srv := newTestServerWithPlugins()

	// Register a shed so we can publish to it
	srv.bridge.RegisterShed("dev", &plugin.ShedConn{
		Name:    "dev",
		Backend: "vz",
		Server:  "test",
		Send:    func(env *plugin.Envelope) error { return nil },
	})

	// Start SSE in background
	done := make(chan struct{})
	var recorder *httptest.ResponseRecorder
	go func() {
		defer close(done)
		r := httptest.NewRequest(http.MethodGet, "/api/plugins/listeners/op/messages", nil)
		r.Header.Set("Accept", "text/event-stream")
		recorder = httptest.NewRecorder()
		srv.Router().ServeHTTP(recorder, r)
	}()

	// Give the handler time to register
	time.Sleep(50 * time.Millisecond)

	// Verify listener is registered
	if _, ok := srv.plugins.Get("op"); !ok {
		t.Fatal("expected listener to be registered")
	}

	// Publish a message
	env := plugin.NewEnvelope("op", plugin.MessageTypeRequest, json.RawMessage(`{"cmd":"test"}`))
	env.Shed = &plugin.ShedInfo{Name: "dev", Backend: "vz", Server: "test"}
	if err := srv.plugins.Publish(env); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	// Give time for the message to be processed then unregister to close the handler
	time.Sleep(50 * time.Millisecond)
	srv.plugins.Unregister("op")

	<-done

	// Verify SSE response
	body := recorder.Body.String()
	if !strings.Contains(body, "event: message") {
		t.Errorf("expected SSE event in body, got: %s", body)
	}
	if !strings.Contains(body, `"namespace":"op"`) {
		t.Errorf("expected namespace in body, got: %s", body)
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

	done := make(chan struct{})
	var recorder *httptest.ResponseRecorder
	go func() {
		defer close(done)
		r := httptest.NewRequest(http.MethodGet, "/api/plugins/listeners/op/messages", nil)
		r.Header.Set("Accept", "text/event-stream")
		recorder = httptest.NewRecorder()
		srv.Router().ServeHTTP(recorder, r)
	}()

	// Stay idle (publish nothing) long enough for multiple keepalive ticks,
	// then close the handler by unregistering.
	time.Sleep(120 * time.Millisecond)
	srv.plugins.Unregister("op")
	<-done

	if n := strings.Count(recorder.Body.String(), ": keepalive"); n < 2 {
		t.Errorf("expected >= 2 idle keepalive pings, got %d in %q", n, recorder.Body.String())
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

func TestHandlePluginRespondShedNotConnected(t *testing.T) {
	srv := newTestServerWithPlugins()
	body := `{"namespace":"op","type":"response","shed":{"name":"unknown"},"payload":{}}`
	r := httptest.NewRequest(http.MethodPost, "/api/plugins/listeners/op/respond", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.Router().ServeHTTP(w, r)

	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusNotFound)
	}
}

func TestHandlePluginRespondSuccess(t *testing.T) {
	srv := newTestServerWithPlugins()

	var sent *plugin.Envelope
	srv.bridge.RegisterShed("dev", &plugin.ShedConn{
		Name: "dev",
		Send: func(env *plugin.Envelope) error {
			sent = env
			return nil
		},
	})

	body := `{"namespace":"op","type":"response","in_reply_to":"abc","final":true,"shed":{"name":"dev"},"payload":{"result":"ok"}}`
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
	if sent.InReplyTo != "abc" {
		t.Errorf("InReplyTo = %q, want %q", sent.InReplyTo, "abc")
	}
}

func TestHandlePluginRespondOwnershipEnforced(t *testing.T) {
	srv := newTestServerWithPlugins()
	store := authtoken.NewStore()
	creds, _, err := store.Mint("SHA256:test", authtoken.ScopeCredentials, authtoken.ClientHostAgent, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	srv.tokens = store
	srv.cfg.Auth = &config.AuthConfig{Mode: config.AuthModeToken}
	srv.plugins.EnableOwnershipTracking() // a server with HTTP auth enforced does this
	srv.bridge.RegisterShed("dev", &plugin.ShedConn{
		Name: "dev",
		Send: func(*plugin.Envelope) error { return nil },
	})
	if _, err := srv.plugins.Register("op"); err != nil {
		t.Fatal(err)
	}

	post := func(inReplyTo string) int {
		body := fmt.Sprintf(`{"namespace":"op","type":"response","in_reply_to":%q,"final":true,"shed":{"name":"dev"}}`, inReplyTo)
		r := httptest.NewRequest(http.MethodPost, "/api/plugins/listeners/op/respond", strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer "+creds)
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
	if err := srv.bridge.PublishToHost("dev", req); err != nil {
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
