package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"

	sdk "github.com/charliek/shed/sdk"

	"github.com/charliek/shed/internal/ext/protocol"
)

// mockBackend implements SSHBackend for testing.
type mockBackend struct {
	keys    []*agent.Key
	signFn  func(key ssh.PublicKey, data []byte, flags agent.SignatureFlags) (*ssh.Signature, error)
	listErr error
	signErr error
}

func (m *mockBackend) Mode() string { return "mock" }

func (m *mockBackend) List() ([]*agent.Key, error) {
	if m.listErr != nil {
		return nil, m.listErr
	}
	return m.keys, nil
}

func (m *mockBackend) Sign(key ssh.PublicKey, data []byte, flags agent.SignatureFlags) (*ssh.Signature, error) {
	if m.signErr != nil {
		return nil, m.signErr
	}
	if m.signFn != nil {
		return m.signFn(key, data, flags)
	}
	return &ssh.Signature{Format: "ssh-ed25519", Blob: []byte("mock-signature")}, nil
}

func TestSSHHandlerList(t *testing.T) {
	backend := &mockBackend{
		keys: []*agent.Key{
			{Format: "ssh-ed25519", Blob: []byte("key1"), Comment: "test@host"},
		},
	}

	var responded sdk.Envelope
	respondCalled := make(chan struct{}, 1)

	// Mock shed-server with SSE + respond endpoints
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			// SSE stream — send one list request then keep alive
			listReq := protocol.SSHListRequest{Operation: protocol.SSHOpList}
			payload, _ := json.Marshal(listReq)
			env := sdk.NewEnvelope(protocol.NamespaceSSHAgent, sdk.MessageTypeRequest, payload)
			env.Shed = &sdk.ShedInfo{Name: "test-shed"}
			data, _ := json.Marshal(env)

			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			flusher := w.(http.Flusher)
			fmt.Fprintf(w, "data: %s\n\n", data)
			flusher.Flush()
			<-r.Context().Done()

		case http.MethodPost:
			mu.Lock()
			json.NewDecoder(r.Body).Decode(&responded)
			mu.Unlock()
			w.WriteHeader(http.StatusNoContent)
			respondCalled <- struct{}{}
		}
	}))
	defer srv.Close()

	client := sdk.NewHostClient(sdk.WithServerURL(srv.URL))
	logger := slog.Default()
	audit := &AuditLogger{logger: logger}

	handler := NewSSHHandler(backend, client, &noopGate{}, audit, "test-server", logger)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	go handler.Run(ctx)

	select {
	case <-respondCalled:
		mu.Lock()
		defer mu.Unlock()

		var listResp protocol.SSHListResponse
		if err := json.Unmarshal(responded.Payload, &listResp); err != nil {
			t.Fatalf("unmarshal response: %v", err)
		}
		if len(listResp.Keys) != 1 {
			t.Fatalf("expected 1 key, got %d", len(listResp.Keys))
		}
		if listResp.Keys[0].Comment != "test@host" {
			t.Errorf("comment: got %q, want %q", listResp.Keys[0].Comment, "test@host")
		}
	case <-ctx.Done():
		t.Fatal("timed out waiting for response")
	}
}

func TestSSHHandlerSign(t *testing.T) {
	// Generate a real ed25519 key so the handler can parse and sign
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	sshPub, err := ssh.NewPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}

	backend := &mockBackend{
		signFn: func(_ ssh.PublicKey, _ []byte, _ agent.SignatureFlags) (*ssh.Signature, error) {
			return &ssh.Signature{
				Format: "ssh-ed25519",
				Blob:   []byte("test-signature"),
			}, nil
		},
	}

	respondCalled := make(chan json.RawMessage, 1)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			signReq := protocol.SSHSignRequest{
				Operation: protocol.SSHOpSign,
				PublicKey: base64.StdEncoding.EncodeToString(sshPub.Marshal()),
				Data:      base64.StdEncoding.EncodeToString([]byte("challenge")),
				Flags:     0,
			}
			payload, _ := json.Marshal(signReq)
			env := sdk.NewEnvelope(protocol.NamespaceSSHAgent, sdk.MessageTypeRequest, payload)
			env.Shed = &sdk.ShedInfo{Name: "test-shed"}
			data, _ := json.Marshal(env)

			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			flusher := w.(http.Flusher)
			fmt.Fprintf(w, "data: %s\n\n", data)
			flusher.Flush()
			<-r.Context().Done()

		case http.MethodPost:
			var env sdk.Envelope
			json.NewDecoder(r.Body).Decode(&env)
			w.WriteHeader(http.StatusNoContent)
			respondCalled <- env.Payload
		}
	}))
	defer srv.Close()

	client := sdk.NewHostClient(sdk.WithServerURL(srv.URL))
	logger := slog.Default()
	audit := &AuditLogger{logger: logger}

	handler := NewSSHHandler(backend, client, &noopGate{}, audit, "test-server", logger)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	go handler.Run(ctx)

	select {
	case payload := <-respondCalled:
		var signResp protocol.SSHSignResponse
		if err := json.Unmarshal(payload, &signResp); err != nil {
			t.Fatalf("unmarshal sign response: %v", err)
		}
		if signResp.Format != "ssh-ed25519" {
			t.Errorf("format: got %q, want %q", signResp.Format, "ssh-ed25519")
		}
		if signResp.Blob == "" {
			t.Error("empty signature blob")
		}
	case <-ctx.Done():
		t.Fatal("timed out waiting for response")
	}
}

func TestSSHHandlerPing(t *testing.T) {
	backend := &mockBackend{}
	respondCalled := make(chan json.RawMessage, 1)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			pingReq := protocol.SSHPingRequest{Operation: protocol.SSHOpPing}
			payload, _ := json.Marshal(pingReq)
			env := sdk.NewEnvelope(protocol.NamespaceSSHAgent, sdk.MessageTypeRequest, payload)
			data, _ := json.Marshal(env)

			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			flusher := w.(http.Flusher)
			fmt.Fprintf(w, "data: %s\n\n", data)
			flusher.Flush()
			<-r.Context().Done()

		case http.MethodPost:
			var env sdk.Envelope
			json.NewDecoder(r.Body).Decode(&env)
			w.WriteHeader(http.StatusNoContent)
			respondCalled <- env.Payload
		}
	}))
	defer srv.Close()

	client := sdk.NewHostClient(sdk.WithServerURL(srv.URL))
	logger := slog.Default()
	audit := &AuditLogger{logger: logger}

	handler := NewSSHHandler(backend, client, &noopGate{}, audit, "test-server", logger)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	go handler.Run(ctx)

	select {
	case payload := <-respondCalled:
		var pingResp protocol.SSHPingResponse
		if err := json.Unmarshal(payload, &pingResp); err != nil {
			t.Fatalf("unmarshal ping response: %v", err)
		}
		if pingResp.Status != "ok" {
			t.Errorf("ping status: got %q, want %q", pingResp.Status, "ok")
		}
	case <-ctx.Done():
		t.Fatal("timed out waiting for ping response")
	}
}

func TestSSHHandlerStatus(t *testing.T) {
	backend := &mockBackend{
		keys: []*agent.Key{
			{Format: "ssh-ed25519", Blob: []byte("key1"), Comment: "test@host"},
			{Format: "ssh-rsa", Blob: []byte("key2"), Comment: "test2@host"},
		},
	}

	respondCalled := make(chan json.RawMessage, 1)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			statusReq := protocol.SSHStatusRequest{Operation: protocol.SSHOpStatus}
			payload, _ := json.Marshal(statusReq)
			env := sdk.NewEnvelope(protocol.NamespaceSSHAgent, sdk.MessageTypeRequest, payload)
			env.Shed = &sdk.ShedInfo{Name: "test-shed"}
			data, _ := json.Marshal(env)

			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			flusher := w.(http.Flusher)
			fmt.Fprintf(w, "data: %s\n\n", data)
			flusher.Flush()
			<-r.Context().Done()

		case http.MethodPost:
			var env sdk.Envelope
			json.NewDecoder(r.Body).Decode(&env)
			w.WriteHeader(http.StatusNoContent)
			respondCalled <- env.Payload
		}
	}))
	defer srv.Close()

	client := sdk.NewHostClient(sdk.WithServerURL(srv.URL))
	logger := slog.Default()
	audit := &AuditLogger{logger: logger}

	handler := NewSSHHandler(backend, client, &noopGate{}, audit, "test-server", logger)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	go handler.Run(ctx)

	select {
	case payload := <-respondCalled:
		var statusResp protocol.SSHStatusResponse
		if err := json.Unmarshal(payload, &statusResp); err != nil {
			t.Fatalf("unmarshal status response: %v", err)
		}
		if !statusResp.Connected {
			t.Error("expected connected=true")
		}
		if statusResp.Mode != "mock" {
			t.Errorf("mode: got %q, want %q", statusResp.Mode, "mock")
		}
		if statusResp.KeyCount != 2 {
			t.Errorf("key_count: got %d, want 2", statusResp.KeyCount)
		}
	case <-ctx.Done():
		t.Fatal("timed out waiting for status response")
	}
}
