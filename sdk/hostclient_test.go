package sdk

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// waitFor polls pred until true or the deadline, failing the test otherwise.
func waitFor(t *testing.T, what string, pred func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if pred() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func nsState(client *HostClient, ns string) (SubStatus, bool) {
	for _, s := range client.Status() {
		if s.Namespace == ns {
			return s, true
		}
	}
	return SubStatus{}, false
}

func TestSubscribeReceivesEnvelopes(t *testing.T) {
	env1 := NewEnvelope("test-ns", MessageTypeRequest, json.RawMessage(`{"cmd":"read"}`))
	env2 := NewEnvelope("test-ns", MessageTypeRequest, json.RawMessage(`{"cmd":"write"}`))

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(http.StatusOK)

		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Fatal("expected ResponseWriter to be a Flusher")
		}

		for _, env := range []*Envelope{env1, env2} {
			data, _ := json.Marshal(env)
			fmt.Fprintf(w, "data: %s\n\n", data)
			flusher.Flush()
		}
	}))
	defer srv.Close()

	client := NewHostClient(
		WithServerURL(srv.URL),
		WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))),
	)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	ch := client.Subscribe(ctx, "test-ns")

	var received []*Envelope
	for env := range ch {
		received = append(received, env)
		if len(received) == 2 {
			cancel()
		}
	}

	if len(received) != 2 {
		t.Fatalf("expected 2 envelopes, got %d", len(received))
	}
	if received[0].ID != env1.ID {
		t.Errorf("first envelope ID = %q, want %q", received[0].ID, env1.ID)
	}
	if received[1].ID != env2.ID {
		t.Errorf("second envelope ID = %q, want %q", received[1].ID, env2.ID)
	}
}

// TestSubscribeTerminatesOn409 proves a 409 (another listener already owns the
// namespace) is terminal: the channel closes on its own, the server is hit
// exactly once (no hot-loop retry), and Status reports ConnRejected. Guards the
// "second broker is observably rejected" acceptance criterion.
func TestSubscribeTerminatesOn409(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`namespace "ns1" is already registered`))
	}))
	defer srv.Close()

	client := NewHostClient(
		WithServerURL(srv.URL),
		WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))),
	)

	// No ctx cancel: the loop must terminate itself on the 409.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	ch := client.Subscribe(ctx, "ns1")

	select {
	case _, ok := <-ch:
		if ok {
			t.Fatal("expected no envelopes on a 409-rejected subscription")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("subscribe did not terminate on 409 — it is hot-looping the retry")
	}

	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Errorf("server hits = %d, want 1 (a 409 must be terminal, not retried)", got)
	}

	st, ok := nsState(client, "ns1")
	if !ok {
		t.Fatal("no status recorded for ns1")
	}
	if st.State != ConnRejected {
		t.Errorf("state = %q, want %q", st.State, ConnRejected)
	}
	if st.LastError == "" {
		t.Error("expected LastError to carry the 409 reason")
	}
}

func TestSubscribeClosesOnContextCancel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		// Block until client disconnects
		<-r.Context().Done()
	}))
	defer srv.Close()

	client := NewHostClient(
		WithServerURL(srv.URL),
		WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))),
	)

	ctx, cancel := context.WithCancel(context.Background())
	ch := client.Subscribe(ctx, "test-ns")

	// Cancel immediately
	cancel()

	// Channel should close
	timer := time.NewTimer(2 * time.Second)
	defer timer.Stop()
	select {
	case _, ok := <-ch:
		if ok {
			// Might get one envelope, keep draining
			for range ch {
			}
		}
		// Channel closed, good
	case <-timer.C:
		t.Fatal("channel did not close after context cancellation")
	}
}

func TestRespondSuccess(t *testing.T) {
	env := NewResponse("req-123", "test-ns", json.RawMessage(`{"result":"ok"}`))

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %q, want POST", r.Method)
		}
		expectedPath := "/api/plugins/listeners/test-ns/respond"
		if r.URL.Path != expectedPath {
			t.Errorf("path = %q, want %q", r.URL.Path, expectedPath)
		}
		if ct := r.Header.Get("Content-Type"); ct != "application/json" {
			t.Errorf("Content-Type = %q, want application/json", ct)
		}

		var decoded Envelope
		if err := json.NewDecoder(r.Body).Decode(&decoded); err != nil {
			t.Fatalf("decoding body: %v", err)
		}
		if decoded.ID != env.ID {
			t.Errorf("envelope ID = %q, want %q", decoded.ID, env.ID)
		}
		if decoded.InReplyTo != "req-123" {
			t.Errorf("InReplyTo = %q, want %q", decoded.InReplyTo, "req-123")
		}

		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	client := NewHostClient(WithServerURL(srv.URL))
	err := client.Respond(context.Background(), "test-ns", env)
	if err != nil {
		t.Fatalf("Respond: %v", err)
	}
}

func TestRespondNon204Status(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte("bad request"))
	}))
	defer srv.Close()

	client := NewHostClient(WithServerURL(srv.URL))
	env := NewResponse("req-123", "test-ns", nil)
	err := client.Respond(context.Background(), "test-ns", env)
	if err == nil {
		t.Fatal("expected error for non-204 response")
	}
}

func TestWithTokenSetsBearerHeader(t *testing.T) {
	// The host-agent authenticates to the credential bus with its scoped bearer
	// token. WithToken must attach it as `Authorization: Bearer <token>` on
	// outbound requests; without it, no Authorization header is sent (so the
	// default-off / tailnet path stays unauthenticated).
	const token = "shed_credentials_abcdef0123456789"
	tests := []struct {
		name     string
		opts     []HostClientOption
		wantAuth string
	}{
		{"token attached as bearer", []HostClientOption{WithToken(token)}, "Bearer " + token},
		{"no token, no header", nil, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var gotAuth string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotAuth = r.Header.Get("Authorization")
				w.WriteHeader(http.StatusNoContent)
			}))
			defer srv.Close()

			opts := append([]HostClientOption{WithServerURL(srv.URL)}, tt.opts...)
			client := NewHostClient(opts...)
			if err := client.Respond(context.Background(), "ns", NewResponse("req-1", "ns", nil)); err != nil {
				t.Fatalf("Respond: %v", err)
			}
			if gotAuth != tt.wantAuth {
				t.Errorf("Authorization = %q, want %q", gotAuth, tt.wantAuth)
			}
		})
	}
}

func TestNewHostClientDefaults(t *testing.T) {
	client := NewHostClient()

	if client.serverURL != defaultServerURL {
		t.Errorf("serverURL = %q, want %q", client.serverURL, defaultServerURL)
	}
	if client.httpClient == nil {
		t.Fatal("expected non-nil httpClient")
	}
	if client.logger == nil {
		t.Fatal("expected non-nil logger")
	}
}

func TestStatusReportsConnected(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		<-r.Context().Done() // hold the stream open
	}))
	defer srv.Close()

	client := NewHostClient(WithServerURL(srv.URL), WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_ = client.Subscribe(ctx, "ns1")

	waitFor(t, "ns1 connected", func() bool {
		s, ok := nsState(client, "ns1")
		return ok && s.State == ConnConnected && s.LastError == ""
	})
}

func TestStatusReportsReconnectingWithError(t *testing.T) {
	// A server that's been shut down → connections refused → reconnecting + error.
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	deadURL := dead.URL
	dead.Close()

	client := NewHostClient(WithServerURL(deadURL), WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_ = client.Subscribe(ctx, "ns1")

	waitFor(t, "ns1 reconnecting with error", func() bool {
		s, ok := nsState(client, "ns1")
		return ok && s.State == ConnReconnecting && s.LastError != ""
	})
}

func TestReconnectLogDedup(t *testing.T) {
	// At WARN level the per-retry DEBUG lines are dropped, so a persistently
	// down server logs the "connection lost" WARN exactly once — not once per
	// backoff cycle (the 21 MB-log regression).
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	deadURL := dead.URL
	dead.Close()

	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))
	client := NewHostClient(WithServerURL(deadURL), WithLogger(logger))

	// Long enough for several retries (initialBackoff is 1s), short enough to
	// keep the test quick.
	ctx, cancel := context.WithTimeout(context.Background(), 2500*time.Millisecond)
	defer cancel()
	for range client.Subscribe(ctx, "ns1") {
	}

	if n := strings.Count(buf.String(), "SSE connection lost"); n != 1 {
		t.Fatalf("expected exactly 1 'connection lost' WARN after dedup, got %d:\n%s", n, buf.String())
	}
}

func TestNewHostClientOptionOverrides(t *testing.T) {
	customHTTP := &http.Client{Timeout: 42 * time.Second}
	customLogger := slog.New(slog.NewTextHandler(io.Discard, nil))

	client := NewHostClient(
		WithServerURL("http://custom:9090/"),
		WithHTTPClient(customHTTP),
		WithLogger(customLogger),
	)

	// WithServerURL trims trailing slash
	if client.serverURL != "http://custom:9090" {
		t.Errorf("serverURL = %q, want %q", client.serverURL, "http://custom:9090")
	}
	if client.httpClient != customHTTP {
		t.Error("expected custom HTTP client")
	}
	if client.logger != customLogger {
		t.Error("expected custom logger")
	}
}

// fakeTokenProvider returns tokens in sequence; Invalidate advances to the next
// (simulating a re-mint) and counts how many times it was called.
type fakeTokenProvider struct {
	mu          sync.Mutex
	tokens      []string
	idx         int
	invalidated int
}

func (f *fakeTokenProvider) Token() (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.tokens[f.idx], nil
}

func (f *fakeTokenProvider) Invalidate() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.invalidated++
	if f.idx < len(f.tokens)-1 {
		f.idx++
	}
}

func quietHostClient(url string, tp TokenProvider) *HostClient {
	return NewHostClient(
		WithServerURL(url),
		WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))),
		WithTokenProvider(tp),
	)
}

func TestRespondRefreshesOn401(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		if r.Header.Get("Authorization") == "Bearer fresh" {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	tp := &fakeTokenProvider{tokens: []string{"stale", "fresh"}}
	client := quietHostClient(srv.URL, tp)

	if err := client.Respond(context.Background(), "ns", NewEnvelope("ns", MessageTypeResponse, nil)); err != nil {
		t.Fatalf("Respond: %v", err)
	}
	if tp.invalidated != 1 {
		t.Errorf("Invalidate called %d times, want 1", tp.invalidated)
	}
	if got := atomic.LoadInt32(&hits); got != 2 {
		t.Errorf("server hits = %d, want 2 (401 then 204)", got)
	}
}

// roundTripFunc adapts a closure into an http.RoundTripper so a test can
// script transport-level outcomes (a TLS alert has no *http.Response at all,
// which no httptest server can express).
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// TestRespondTLSAuthFailureThen401RemintsOnce pins the one-re-mint/two-sends
// bound across BOTH classifier legs: a first send that dies with a TLS
// certificate alert (re-mint, retry), whose retry then 401s. The 401 must
// surface as the error — a second re-mint and third send would present the
// same freshly-minted-and-refused identity again.
func TestRespondTLSAuthFailureThen401RemintsOnce(t *testing.T) {
	var sends int32
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		switch atomic.AddInt32(&sends, 1) {
		case 1:
			// The shape authfail's fallback path classifies: a peer TLS alert
			// naming a certificate problem, surviving only as text.
			return nil, fmt.Errorf("remote error: tls: certificate required")
		default:
			return &http.Response{
				StatusCode: http.StatusUnauthorized,
				Body:       io.NopCloser(strings.NewReader("")),
				Header:     make(http.Header),
				Request:    r,
			}, nil
		}
	})

	tp := &fakeTokenProvider{tokens: []string{"a", "b", "c"}}
	client := NewHostClient(
		WithServerURL("https://irrelevant.invalid"),
		WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))),
		WithTokenProvider(tp),
		WithHTTPClient(&http.Client{Transport: transport}),
	)

	err := client.Respond(context.Background(), "ns", NewEnvelope("ns", MessageTypeResponse, nil))
	if err == nil {
		t.Error("expected the retry's 401 to surface as an error")
	}
	if tp.invalidated != 1 {
		t.Errorf("Invalidate called %d times, want exactly 1 (one re-mint total)", tp.invalidated)
	}
	if got := atomic.LoadInt32(&sends); got != 2 {
		t.Errorf("sends = %d, want 2 (TLS failure + one retry, never a third)", got)
	}
}

// TestRespondTLSAuthFailureRecovers is the happy half: the TLS-classified
// failure re-mints once and the retry succeeds.
func TestRespondTLSAuthFailureRecovers(t *testing.T) {
	var sends int32
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if atomic.AddInt32(&sends, 1) == 1 {
			return nil, fmt.Errorf("remote error: tls: certificate required")
		}
		return &http.Response{
			StatusCode: http.StatusNoContent,
			Body:       io.NopCloser(strings.NewReader("")),
			Header:     make(http.Header),
			Request:    r,
		}, nil
	})

	tp := &fakeTokenProvider{tokens: []string{"stale", "fresh"}}
	client := NewHostClient(
		WithServerURL("https://irrelevant.invalid"),
		WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))),
		WithTokenProvider(tp),
		WithHTTPClient(&http.Client{Transport: transport}),
	)

	if err := client.Respond(context.Background(), "ns", NewEnvelope("ns", MessageTypeResponse, nil)); err != nil {
		t.Fatalf("Respond after re-mint: %v", err)
	}
	if tp.invalidated != 1 {
		t.Errorf("Invalidate called %d times, want 1", tp.invalidated)
	}
	if got := atomic.LoadInt32(&sends); got != 2 {
		t.Errorf("sends = %d, want 2", got)
	}
}

func TestRespondRetriesAtMostOnceOn401(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(http.StatusUnauthorized) // always 401
	}))
	defer srv.Close()

	tp := &fakeTokenProvider{tokens: []string{"a", "b", "c"}}
	client := quietHostClient(srv.URL, tp)

	if err := client.Respond(context.Background(), "ns", NewEnvelope("ns", MessageTypeResponse, nil)); err == nil {
		t.Error("expected an error when the 401 persists")
	}
	if tp.invalidated != 1 {
		t.Errorf("Invalidate called %d times, want exactly 1 (at-most-once retry)", tp.invalidated)
	}
	if got := atomic.LoadInt32(&hits); got != 2 {
		t.Errorf("server hits = %d, want 2 (initial + one retry)", got)
	}
}
