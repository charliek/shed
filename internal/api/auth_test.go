package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/charliek/shed/internal/config"
)

func authTestServer(mode string) *Server {
	return &Server{cfg: &config.ServerConfig{Auth: &config.AuthConfig{HTTP: &config.HTTPAuthConfig{
		Mode: mode,
		Tokens: []config.HTTPToken{
			{Scope: config.TokenScopeControl, Token: "shed_control_ctl"},
			{Scope: config.TokenScopeCredentials, Token: "shed_credentials_cred"},
		},
	}}}}
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
	s := authTestServer(config.HTTPAuthEnforce)
	tests := []struct {
		name, method, path, token string
		want                      int
	}{
		{"bootstrap info, no token", "GET", "/api/info", "", 200},
		{"bootstrap host-key, no token", "GET", "/api/ssh-host-key", "", 200},
		{"control plane, no token", "GET", "/api/sheds", "", 401},
		{"control plane, unknown token", "GET", "/api/sheds", "shed_control_nope", 401},
		{"control plane, control token", "GET", "/api/sheds", "shed_control_ctl", 200},
		{"control plane, credentials token forbidden", "GET", "/api/sheds", "shed_credentials_cred", 403},
		{"bus, credentials token", "GET", "/api/plugins/listeners", "shed_credentials_cred", 200},
		{"bus, control token forbidden", "GET", "/api/plugins/listeners", "shed_control_ctl", 403},
		{"connect, credentials token", "GET", "/api/sheds/x/connect/22", "shed_credentials_cred", 200},
		{"connect, control token forbidden", "GET", "/api/sheds/x/connect/22", "shed_control_ctl", 403},
		{"create, control token", "POST", "/api/sheds", "shed_control_ctl", 200},
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
	if got := doAuth(authTestServer(config.HTTPAuthOff), "GET", "/api/sheds", ""); got != 200 {
		t.Errorf("off mode should pass through, got %d", got)
	}
	// No auth config at all also passes through.
	if got := doAuth(&Server{cfg: &config.ServerConfig{}}, "GET", "/api/sheds", ""); got != 200 {
		t.Errorf("nil auth should pass through, got %d", got)
	}
}
