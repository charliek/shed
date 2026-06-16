package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/charliek/shed/internal/config"
)

// TestHandleGetInfo_HTTPSPort verifies GET /api/info reports https_port only
// when a TLS listener is configured (secure mode), and omits it otherwise so a
// client that predates the field (v0.7.1) decodes a zero value without error.
func TestHandleGetInfo_HTTPSPort(t *testing.T) {
	tests := []struct {
		name      string
		httpsPort int
		wantField bool
		wantValue int
	}{
		{name: "secure reports https_port", httpsPort: 8443, wantField: true, wantValue: 8443},
		{name: "open omits https_port", httpsPort: 0, wantField: false, wantValue: 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := NewServer(nil, &config.ServerConfig{
				Name:      "test-server",
				HTTPPort:  8080,
				HTTPSPort: tt.httpsPort,
			}, "", nil, nil)

			r := httptest.NewRequest(http.MethodGet, "/api/info", nil)
			w := httptest.NewRecorder()
			srv.Router().ServeHTTP(w, r)

			if w.Code != http.StatusOK {
				t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
			}

			// Presence check on the raw JSON — omitempty drops the key at 0.
			var raw map[string]json.RawMessage
			if err := json.Unmarshal(w.Body.Bytes(), &raw); err != nil {
				t.Fatalf("parse /api/info body: %v", err)
			}
			if _, present := raw["https_port"]; present != tt.wantField {
				t.Fatalf("https_port present=%v, want %v (body: %s)", present, tt.wantField, w.Body.String())
			}

			// Typed decode mirrors what a client does; back-compat = zero value.
			var info config.ServerInfo
			if err := json.Unmarshal(w.Body.Bytes(), &info); err != nil {
				t.Fatalf("decode ServerInfo: %v", err)
			}
			if info.HTTPSPort != tt.wantValue {
				t.Fatalf("HTTPSPort=%d, want %d", info.HTTPSPort, tt.wantValue)
			}
		})
	}
}
