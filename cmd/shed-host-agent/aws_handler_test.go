package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	sdk "github.com/charliek/shed/sdk"

	"github.com/charliek/shed/internal/ext/protocol"
)

func TestAWSHandlerGetCredentials(t *testing.T) {
	backend := &mockAWSBackend{
		creds: &AWSCachedCredentials{
			AccessKeyID:     "ASIAIOSFODNN7EXAMPLE",
			SecretAccessKey: "wJalrXUtnFEMI/K7MDENG",
			SessionToken:    "FwoGZXIvYXdzE...",
			Expiration:      time.Date(2026, 3, 31, 19, 0, 0, 0, time.UTC),
		},
	}

	var responded sdk.Envelope
	respondCalled := make(chan struct{}, 1)

	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			req := protocol.AWSCredentialsRequest{Operation: protocol.AWSOpGetCredentials}
			payload, _ := json.Marshal(req)
			env := sdk.NewEnvelope(protocol.NamespaceAWSCredentials, sdk.MessageTypeRequest, payload)
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

	handler := NewAWSHandler(backend, client, &noopGate{}, audit, "test-server", logger)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	go handler.Run(ctx)

	select {
	case <-respondCalled:
		mu.Lock()
		defer mu.Unlock()

		var credsResp protocol.AWSCredentialsResponse
		if err := json.Unmarshal(responded.Payload, &credsResp); err != nil {
			t.Fatalf("unmarshal response: %v", err)
		}
		if credsResp.AccessKeyID != "ASIAIOSFODNN7EXAMPLE" {
			t.Errorf("AccessKeyID: got %q, want %q", credsResp.AccessKeyID, "ASIAIOSFODNN7EXAMPLE")
		}
		if credsResp.Expiration != "2026-03-31T19:00:00Z" {
			t.Errorf("Expiration: got %q, want %q", credsResp.Expiration, "2026-03-31T19:00:00Z")
		}
	case <-ctx.Done():
		t.Fatal("timed out waiting for response")
	}
}

// runAWSCredsRequest drives one get_credentials round-trip through the handler
// and returns the raw response payload the host posted back.
func runAWSCredsRequest(t *testing.T, backend AWSBackend, gate ApprovalGate) json.RawMessage {
	t.Helper()
	respondCalled := make(chan json.RawMessage, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			req := protocol.AWSCredentialsRequest{Operation: protocol.AWSOpGetCredentials}
			payload, _ := json.Marshal(req)
			env := sdk.NewEnvelope(protocol.NamespaceAWSCredentials, sdk.MessageTypeRequest, payload)
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
	handler := NewAWSHandler(backend, client, gate, audit, "test-server", logger)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	go handler.Run(ctx)

	select {
	case payload := <-respondCalled:
		return payload
	case <-ctx.Done():
		t.Fatal("timed out waiting for response")
		return nil
	}
}

// A passthrough vend with no expiry hint must omit the expiration field entirely
// (so the guest SDK treats the creds as non-expiring), not send year-0001 or "".
func TestAWSHandlerOmitsExpirationWhenZero(t *testing.T) {
	backend := &mockAWSBackend{
		creds: &AWSCachedCredentials{
			AccessKeyID:     "AK",
			SecretAccessKey: "SK",
			SessionToken:    "TK",
		},
	}
	payload := runAWSCredsRequest(t, backend, &noopGate{})

	var raw map[string]any
	if err := json.Unmarshal(payload, &raw); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, present := raw["expiration"]; present {
		t.Errorf("expiration key should be absent for zero expiry, got payload %s", payload)
	}
	if raw["access_key_id"] != "AK" {
		t.Errorf("access_key_id: got %v", raw["access_key_id"])
	}
}

func TestAWSExpiryDetail(t *testing.T) {
	if got := awsExpiryDetail(time.Time{}); got != "expires:none" {
		t.Errorf("zero expiry: got %q, want expires:none", got)
	}
	exp := time.Date(2026, 3, 31, 19, 5, 0, 0, time.UTC)
	if got := awsExpiryDetail(exp); got != "expires:19:05" {
		t.Errorf("non-zero expiry: got %q, want expires:19:05", got)
	}
}

func TestAWSHandlerPing(t *testing.T) {
	backend := &mockAWSBackend{}
	respondCalled := make(chan json.RawMessage, 1)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			pingReq := protocol.AWSPingRequest{Operation: protocol.AWSOpPing}
			payload, _ := json.Marshal(pingReq)
			env := sdk.NewEnvelope(protocol.NamespaceAWSCredentials, sdk.MessageTypeRequest, payload)
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

	handler := NewAWSHandler(backend, client, &noopGate{}, audit, "test-server", logger)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	go handler.Run(ctx)

	select {
	case payload := <-respondCalled:
		var pingResp protocol.AWSPingResponse
		if err := json.Unmarshal(payload, &pingResp); err != nil {
			t.Fatalf("unmarshal ping response: %v", err)
		}
		if pingResp.Status != "ok" {
			t.Errorf("status: got %q, want %q", pingResp.Status, "ok")
		}
	case <-ctx.Done():
		t.Fatal("timed out waiting for ping response")
	}
}

func TestAWSHandlerError(t *testing.T) {
	backend := &mockAWSBackend{
		err: fmt.Errorf("sts:AssumeRole failed"),
	}

	respondCalled := make(chan json.RawMessage, 1)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			req := protocol.AWSCredentialsRequest{Operation: protocol.AWSOpGetCredentials}
			payload, _ := json.Marshal(req)
			env := sdk.NewEnvelope(protocol.NamespaceAWSCredentials, sdk.MessageTypeRequest, payload)
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

	handler := NewAWSHandler(backend, client, &noopGate{}, audit, "test-server", logger)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	go handler.Run(ctx)

	select {
	case payload := <-respondCalled:
		var errResp protocol.AWSErrorResponse
		if err := json.Unmarshal(payload, &errResp); err != nil {
			t.Fatalf("unmarshal error response: %v", err)
		}
		if errResp.Code != protocol.AWSCodeAssumeRoleFailed {
			t.Errorf("code: got %q, want %q", errResp.Code, protocol.AWSCodeAssumeRoleFailed)
		}
	case <-ctx.Done():
		t.Fatal("timed out waiting for error response")
	}
}

func TestAWSHandlerStatus(t *testing.T) {
	backend := &mockAWSBackend{}
	respondCalled := make(chan json.RawMessage, 1)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			statusReq := protocol.AWSStatusRequest{Operation: protocol.AWSOpStatus}
			payload, _ := json.Marshal(statusReq)
			env := sdk.NewEnvelope(protocol.NamespaceAWSCredentials, sdk.MessageTypeRequest, payload)
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

	handler := NewAWSHandler(backend, client, &noopGate{}, audit, "test-server", logger)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	go handler.Run(ctx)

	select {
	case payload := <-respondCalled:
		var statusResp protocol.AWSStatusResponse
		if err := json.Unmarshal(payload, &statusResp); err != nil {
			t.Fatalf("unmarshal status response: %v", err)
		}
		if !statusResp.Connected {
			t.Error("expected connected=true")
		}
		if statusResp.Role != "arn:aws:iam::123:role/mock" {
			t.Errorf("role: got %q, want mock role", statusResp.Role)
		}
	case <-ctx.Done():
		t.Fatal("timed out waiting for status response")
	}
}

// A deny-all gate rejects the credential request before the backend is hit.
func TestAWSHandlerDeniedByGate(t *testing.T) {
	backend := &mockAWSBackend{
		creds: &AWSCachedCredentials{AccessKeyID: "AK", Expiration: time.Now().Add(time.Hour)},
	}
	respondCalled := make(chan json.RawMessage, 1)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			req := protocol.AWSCredentialsRequest{Operation: protocol.AWSOpGetCredentials}
			payload, _ := json.Marshal(req)
			env := sdk.NewEnvelope(protocol.NamespaceAWSCredentials, sdk.MessageTypeRequest, payload)
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
	handler := NewAWSHandler(backend, client, &denyAllGate{}, audit, "test-server", logger)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	go handler.Run(ctx)

	select {
	case payload := <-respondCalled:
		var errResp protocol.AWSErrorResponse
		if err := json.Unmarshal(payload, &errResp); err != nil {
			t.Fatalf("unmarshal error response: %v", err)
		}
		if errResp.Code != protocol.AWSCodeAssumeRoleFailed {
			t.Errorf("code: got %q, want %q", errResp.Code, protocol.AWSCodeAssumeRoleFailed)
		}
		if backend.calls() != 0 {
			t.Errorf("backend was called %d times, want 0 (denied before vend)", backend.calls())
		}
	case <-ctx.Done():
		t.Fatal("timed out waiting for denied response")
	}
}
