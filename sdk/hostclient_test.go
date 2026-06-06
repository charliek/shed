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
