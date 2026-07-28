package main

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"testing"
	"time"

	"github.com/charliek/shed/internal/config"
)

// TestGetInfoNormalizesWireAuthMode pins the client half of the /api/info
// wire-compat contract: both released pre-rename servers and current
// token-mode servers report the legacy "secure" spelling on the wire
// (config.LegacyWireAuthMode), and GetInfo must normalize it to the canonical
// "token" before any consumer compares it against the AuthMode* constants —
// the HTTP-fallback guard in `shed server add` otherwise treats a
// credentialed server as open and writes an entry with no credential.
func TestGetInfoNormalizesWireAuthMode(t *testing.T) {
	tests := []struct {
		name string
		wire string
		want string
	}{
		{"legacy secure normalizes to token", "secure", config.AuthModeToken},
		{"canonical token passes through", config.AuthModeToken, config.AuthModeToken},
		{"open passes through", config.AuthModeOpen, config.AuthModeOpen},
		{"mtls passes through", config.AuthModeMTLS, config.AuthModeMTLS},
		{"absent stays empty", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/api/info" {
					http.NotFound(w, r)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				body := `{"name":"test-server","version":"v0.0.0-test"`
				if tt.wire != "" {
					body += `,"auth_mode":"` + tt.wire + `"`
				}
				body += `}`
				_, _ = w.Write([]byte(body))
			}))
			defer srv.Close()

			u, err := url.Parse(srv.URL)
			if err != nil {
				t.Fatalf("parse test server URL: %v", err)
			}
			port, err := strconv.Atoi(u.Port())
			if err != nil {
				t.Fatalf("parse test server port: %v", err)
			}
			info, err := NewAPIClient(u.Hostname(), port, time.Second).GetInfo()
			if err != nil {
				t.Fatalf("GetInfo: %v", err)
			}
			if info.AuthMode != tt.want {
				t.Errorf("GetInfo AuthMode = %q, want %q", info.AuthMode, tt.want)
			}
		})
	}
}
