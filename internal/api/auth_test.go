package api

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/charliek/shed/internal/authtoken"
	"github.com/charliek/shed/internal/config"
)

// authTestServer builds a server whose HTTP auth enforcement matches tokenMode
// (token mode enforces bearer tokens; open mode is pass-through). Its token
// store holds one control and one credentials token, returned as plaintext for
// the table to exercise.
func authTestServer(t *testing.T, tokenMode bool) (s *Server, control, credentials string) {
	t.Helper()
	store := authtoken.NewStore()
	control, _, err := store.Mint("SHA256:test", authtoken.ScopeControl, authtoken.ClientCLI, time.Hour)
	if err != nil {
		t.Fatalf("mint control: %v", err)
	}
	credentials, _, err = store.Mint("SHA256:test", authtoken.ScopeCredentials, authtoken.ClientHostAgent, time.Hour)
	if err != nil {
		t.Fatalf("mint credentials: %v", err)
	}
	mode := config.AuthModeOpen
	if tokenMode {
		mode = config.AuthModeToken
	}
	s = &Server{
		cfg:    &config.ServerConfig{Auth: &config.AuthConfig{Mode: mode}},
		tokens: store,
	}
	return s, control, credentials
}

func doAuth(s *Server, method, path, token string) int {
	h := s.authMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequest(method, path, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr.Code
}

func TestAuthMiddlewareEnforce(t *testing.T) {
	s, ctl, cred := authTestServer(t, true)
	tests := []struct {
		name, method, path, token string
		want                      int
	}{
		{"bootstrap info, no token", "GET", "/api/info", "", 200},
		{"bootstrap host-key, no token", "GET", "/api/ssh-host-key", "", 200},
		{"control plane, no token", "GET", "/api/sheds", "", 401},
		{"control plane, unknown token", "GET", "/api/sheds", "shed_control_nope", 401},
		{"control plane, control token", "GET", "/api/sheds", ctl, 200},
		{"control plane, credentials token forbidden", "GET", "/api/sheds", cred, 403},
		{"bus, credentials token", "GET", "/api/plugins/listeners", cred, 200},
		{"bus, control token forbidden", "GET", "/api/plugins/listeners", ctl, 403},
		{"connect, control token", "GET", "/api/sheds/x/connect/22", ctl, 200},
		{"connect, credentials token", "GET", "/api/sheds/x/connect/22", cred, 200},
		{"connect, no token", "GET", "/api/sheds/x/connect/22", "", 401},
		// A shed literally named "connect" must NOT slip a credentials token onto
		// a control-only lifecycle route via a loose "/connect/" substring match.
		{"shed named connect, control token", "POST", "/api/sheds/connect/start", ctl, 200},
		{"shed named connect, credentials token forbidden", "POST", "/api/sheds/connect/start", cred, 403},
		// Connect classification is route-shape exact, not a substring: neither a
		// missing port nor an extra segment counts as the tunnel (→ control-only).
		{"connect prefix without port, credentials forbidden", "GET", "/api/sheds/x/connect", cred, 403},
		{"connect with trailing segment, credentials forbidden", "GET", "/api/sheds/x/connect/22/extra", cred, 403},
		// The fleet-global egress audit stream accepts either scope (host-agent
		// subscriber holds credentials; a control token may tail it).
		{"egress stream, control token", "GET", "/api/egress/stream", ctl, 200},
		{"egress stream, credentials token", "GET", "/api/egress/stream", cred, 200},
		{"egress stream, no token", "GET", "/api/egress/stream", "", 401},
		// Only GET /stream is dual-scope; other egress routes stay control-only.
		{"egress profiles, control token", "GET", "/api/egress/profiles", ctl, 200},
		{"egress profiles, credentials token forbidden", "GET", "/api/egress/profiles", cred, 403},
		{"egress per-shed, credentials token forbidden", "GET", "/api/egress/myshed", cred, 403},
		// A shed named "stream" must NOT let a credentials token onto the
		// control-only POST/DELETE egress mutators via the /stream path.
		{"egress stream POST, credentials forbidden", "POST", "/api/egress/stream", cred, 403},
		{"egress stream DELETE, credentials forbidden", "DELETE", "/api/egress/stream", cred, 403},
		{"egress stream POST, control token", "POST", "/api/egress/stream", ctl, 200},
		{"create, control token", "POST", "/api/sheds", ctl, 200},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := doAuth(s, tt.method, tt.path, tt.token); got != tt.want {
				t.Errorf("got %d, want %d", got, tt.want)
			}
		})
	}
}

func TestAuthMiddlewareOffIsPassthrough(t *testing.T) {
	s, _, _ := authTestServer(t, false)
	if got := doAuth(s, "GET", "/api/sheds", ""); got != 200 {
		t.Errorf("off mode should pass through, got %d", got)
	}
	// No auth config (and no token store) also passes through.
	if got := doAuth(&Server{cfg: &config.ServerConfig{}}, "GET", "/api/sheds", ""); got != 200 {
		t.Errorf("nil auth should pass through, got %d", got)
	}
}

// TestAuthMiddlewareExpiredToken: an expired token fails closed (401), proving
// the middleware honors the store's TTL rather than the token string alone.
func TestAuthMiddlewareExpiredToken(t *testing.T) {
	store := authtoken.NewStore()
	tok, _, err := store.Mint("SHA256:test", authtoken.ScopeControl, authtoken.ClientCLI, time.Nanosecond)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(time.Millisecond)
	s := &Server{
		cfg:    &config.ServerConfig{Auth: &config.AuthConfig{Mode: config.AuthModeToken}},
		tokens: store,
	}
	if got := doAuth(s, "GET", "/api/sheds", tok); got != 401 {
		t.Errorf("expired token: got %d, want 401", got)
	}
}
