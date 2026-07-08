// Package testutil provides shared test helpers for shed-extensions.
package testutil

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	sdk "github.com/charliek/shed/sdk"
)

// PublishRequest mirrors the bus publish request for test decoding.
type PublishRequest struct {
	Namespace string          `json:"namespace"`
	Type      string          `json:"type"`
	Payload   json.RawMessage `json:"payload"`
}

// NewMockPublishServer creates a test server simulating the shed-agent
// /v1/publish endpoint. The handler receives the request payload and returns
// a response payload.
func NewMockPublishServer(t *testing.T, expectedNamespace string, handler func(json.RawMessage) json.RawMessage) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/publish" {
			t.Errorf("unexpected path: %s", r.URL.Path)
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		if r.Method != http.MethodPost {
			t.Errorf("unexpected method: %s, want POST", r.Method)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		var pubReq PublishRequest
		if err := json.NewDecoder(r.Body).Decode(&pubReq); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		if pubReq.Namespace != expectedNamespace {
			t.Errorf("namespace: got %q, want %q", pubReq.Namespace, expectedNamespace)
		}

		respPayload := handler(pubReq.Payload)
		env := sdk.NewResponse("mock-req", pubReq.Namespace, respPayload)
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(env); err != nil {
			t.Errorf("failed to encode response: %v", err)
		}
	}))
}
