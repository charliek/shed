package awsproxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/charliek/shed/internal/ext/protocol"
	"github.com/charliek/shed/internal/ext/testutil"
)

func TestHandleCredentials(t *testing.T) {
	creds := protocol.AWSCredentialsResponse{
		AccessKeyID:     "ASIAIOSFODNN7EXAMPLE",
		SecretAccessKey: "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY",
		SessionToken:    "FwoGZXIvYXdzE...",
		Expiration:      "2026-03-31T19:00:00Z",
	}

	publishSrv := newMockPublishServer(t, func(payload json.RawMessage) json.RawMessage {
		var req protocol.AWSCredentialsRequest
		if err := json.Unmarshal(payload, &req); err != nil {
			t.Fatalf("unmarshal request: %v", err)
		}
		if req.Operation != protocol.AWSOpGetCredentials {
			t.Fatalf("expected get_credentials, got %q", req.Operation)
		}
		data, _ := json.Marshal(creds)
		return data
	})
	defer publishSrv.Close()

	proxy := New(WithPublishURL(publishSrv.URL + "/v1/publish"))

	req := httptest.NewRequest(http.MethodGet, "/credentials", nil)
	w := httptest.NewRecorder()
	proxy.HandleCredentials(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	// Save raw bytes before decoding consumes the buffer
	raw := w.Body.Bytes()

	var resp awsSDKResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}

	// Verify PascalCase field names in the SDK response
	if resp.AccessKeyID != creds.AccessKeyID {
		t.Errorf("AccessKeyId: got %q, want %q", resp.AccessKeyID, creds.AccessKeyID)
	}
	if resp.SecretAccessKey != creds.SecretAccessKey {
		t.Errorf("SecretAccessKey mismatch")
	}
	if resp.Token != creds.SessionToken {
		t.Errorf("Token: got %q, want %q", resp.Token, creds.SessionToken)
	}
	if resp.Expiration != creds.Expiration {
		t.Errorf("Expiration: got %q, want %q", resp.Expiration, creds.Expiration)
	}

	// Also verify raw JSON has correct field names (not Go struct field names)
	var rawMap map[string]string
	if err := json.Unmarshal(raw, &rawMap); err != nil {
		t.Fatalf("unmarshal raw JSON: %v", err)
	}
	if _, ok := rawMap["AccessKeyId"]; !ok {
		t.Errorf("response JSON missing PascalCase 'AccessKeyId' field, got keys: %v", keys(rawMap))
	}
	if _, ok := rawMap["Token"]; !ok {
		t.Errorf("response JSON missing 'Token' field, got keys: %v", keys(rawMap))
	}
}

func TestHandleCredentialsOmitsEmptyExpiration(t *testing.T) {
	// Passthrough without an expiry hint sends no expiration; the guest response
	// must omit "Expiration" entirely (an empty string can't be parsed into a
	// time, but an absent field means non-expiring).
	creds := protocol.AWSCredentialsResponse{
		AccessKeyID:     "ASIAIOSFODNN7EXAMPLE",
		SecretAccessKey: "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY",
		SessionToken:    "FwoGZXIvYXdzE...",
	}
	publishSrv := newMockPublishServer(t, func(_ json.RawMessage) json.RawMessage {
		data, _ := json.Marshal(creds)
		return data
	})
	defer publishSrv.Close()

	proxy := New(WithPublishURL(publishSrv.URL + "/v1/publish"))

	req := httptest.NewRequest(http.MethodGet, "/credentials", nil)
	w := httptest.NewRecorder()
	proxy.HandleCredentials(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var rawMap map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &rawMap); err != nil {
		t.Fatalf("unmarshal raw JSON: %v", err)
	}
	if _, present := rawMap["Expiration"]; present {
		t.Errorf("Expiration must be omitted for non-expiring creds, body=%s", w.Body.String())
	}
	if rawMap["AccessKeyId"] != creds.AccessKeyID {
		t.Errorf("AccessKeyId: got %v, want %q", rawMap["AccessKeyId"], creds.AccessKeyID)
	}
}

func TestHandleCredentialsErrorResponse(t *testing.T) {
	publishSrv := newMockPublishServer(t, func(_ json.RawMessage) json.RawMessage {
		resp := protocol.AWSErrorResponse{
			Error: "role not found",
			Code:  protocol.AWSCodeRoleNotFound,
		}
		data, _ := json.Marshal(resp)
		return data
	})
	defer publishSrv.Close()

	proxy := New(WithPublishURL(publishSrv.URL + "/v1/publish"))

	req := httptest.NewRequest(http.MethodGet, "/credentials", nil)
	w := httptest.NewRecorder()
	proxy.HandleCredentials(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d", w.Code)
	}
}

func TestHandleCredentialsTimeout(t *testing.T) {
	// Server that never responds
	publishSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(10 * time.Second)
	}))
	defer publishSrv.Close()

	proxy := New(WithPublishURL(publishSrv.URL + "/v1/publish"))

	req := httptest.NewRequest(http.MethodGet, "/credentials", nil)
	w := httptest.NewRecorder()
	proxy.HandleCredentials(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 on timeout, got %d", w.Code)
	}
}

func TestHandleCredentialsMethodNotAllowed(t *testing.T) {
	proxy := New()

	req := httptest.NewRequest(http.MethodPost, "/credentials", nil)
	w := httptest.NewRecorder()
	proxy.HandleCredentials(w, req)

	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405, got %d", w.Code)
	}
}

func keys(m map[string]string) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	return ks
}

func newMockPublishServer(t *testing.T, handler func(json.RawMessage) json.RawMessage) *httptest.Server {
	return testutil.NewMockPublishServer(t, protocol.NamespaceAWSCredentials, handler)
}
