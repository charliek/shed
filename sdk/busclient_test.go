package sdk

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestPublishSuccess(t *testing.T) {
	respEnv := NewEnvelope("test-ns", MessageTypeResponse, json.RawMessage(`{"result":"ok"}`))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %q, want POST", r.Method)
		}
		if ct := r.Header.Get("Content-Type"); ct != "application/json" {
			t.Errorf("Content-Type = %q, want application/json", ct)
		}

		var req publishRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decoding request body: %v", err)
		}
		if req.Namespace != "test-ns" {
			t.Errorf("request namespace = %q, want %q", req.Namespace, "test-ns")
		}
		if req.Type != string(MessageTypeRequest) {
			t.Errorf("request type = %q, want %q", req.Type, string(MessageTypeRequest))
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(respEnv)
	}))
	defer srv.Close()

	client := NewBusClient(srv.URL, 5*time.Second)
	payload, err := client.Publish(context.Background(), "test-ns", json.RawMessage(`{"cmd":"read"}`))
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if string(payload) != `{"result":"ok"}` {
		t.Errorf("payload = %s, want %s", payload, `{"result":"ok"}`)
	}
}

func TestPublishServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte("internal server error"))
	}))
	defer srv.Close()

	client := NewBusClient(srv.URL, 5*time.Second)
	_, err := client.Publish(context.Background(), "test-ns", json.RawMessage(`{}`))
	if err == nil {
		t.Fatal("expected error for non-200 response")
	}
}

func TestPingSuccess(t *testing.T) {
	respEnv := NewEnvelope("test-ns", MessageTypeResponse, json.RawMessage(`{"status":"ok"}`))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(respEnv)
	}))
	defer srv.Close()

	client := NewBusClient(srv.URL, 5*time.Second)
	err := client.Ping(context.Background(), "test-ns", 2*time.Second)
	if err != nil {
		t.Fatalf("Ping: %v", err)
	}
}

func TestPingTimeout(t *testing.T) {
	// Use a channel to allow the handler to unblock when the test is done,
	// so the httptest server can shut down cleanly.
	done := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-done:
		case <-r.Context().Done():
		}
	}))
	defer func() {
		close(done)
		srv.Close()
	}()

	client := NewBusClient(srv.URL, 10*time.Second)
	err := client.Ping(context.Background(), "test-ns", 50*time.Millisecond)
	if err == nil {
		t.Fatal("expected error for timeout")
	}
}

func TestNewBusClientSetsFields(t *testing.T) {
	client := NewBusClient("http://example.com/publish", 7*time.Second)

	if client.PublishURL != "http://example.com/publish" {
		t.Errorf("PublishURL = %q, want %q", client.PublishURL, "http://example.com/publish")
	}
	if client.HTTPClient == nil {
		t.Fatal("expected non-nil HTTPClient")
	}
	if client.HTTPClient.Timeout != 7*time.Second {
		t.Errorf("Timeout = %v, want %v", client.HTTPClient.Timeout, 7*time.Second)
	}
}
