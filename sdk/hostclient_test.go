package sdk

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

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
