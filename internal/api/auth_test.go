package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/charliek/shed/internal/config"
)

// doAuth runs one request through the auth middleware, with an optional
// Authorization header, and returns the status.
func doAuth(s *Server, method, path, authorization string) int {
	h := s.authMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequest(method, path, nil)
	if authorization != "" {
		req.Header.Set("Authorization", authorization)
	}
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr.Code
}

func TestAuthMiddlewareOpenIsPassthrough(t *testing.T) {
	for _, cfg := range []*config.ServerConfig{
		{},
		{Auth: &config.AuthConfig{}},
		{Auth: &config.AuthConfig{Mode: config.AuthModeOpen}},
	} {
		s := &Server{cfg: cfg}
		for _, path := range []string{"/api/sheds", "/api/info", "/api/plugins/listeners"} {
			if got := doAuth(s, "GET", path, ""); got != 200 {
				t.Errorf("open mode %+v %s: got %d, want 200", cfg.Auth, path, got)
			}
		}
	}
}

// TestAuthMiddlewareRefusesRemovedModes: the server has no bearer-token path.
// A config that names the removed token mode cannot load, but one built in
// code must still not fall back to open, and a bearer token must not help:
// every request, the old bootstrap-exempt routes included, is refused.
func TestAuthMiddlewareRefusesRemovedModes(t *testing.T) {
	for _, mode := range []string{config.AuthModeToken, "secure", "bogus"} {
		s := &Server{cfg: &config.ServerConfig{Auth: &config.AuthConfig{Mode: mode}}}
		for _, path := range []string{"/api/sheds", "/api/info", "/api/ssh-host-key", "/api/plugins/listeners"} {
			for _, header := range []string{"", "Bearer shed_control_AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"} {
				if got := doAuth(s, "GET", path, header); got != http.StatusInternalServerError {
					t.Errorf("mode %q %s (Authorization %q): got %d, want 500", mode, path, header, got)
				}
			}
		}
	}
}
