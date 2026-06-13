package api

import (
	"net/http"
	"strings"

	"github.com/charliek/shed/internal/config"
)

// authMiddleware enforces bearer-token auth when auth.http.mode == enforce.
// When auth is off (the default), it is a pass-through, so existing
// deployments are unaffected.
//
// Exemptions: the bootstrap endpoints GET /api/info and GET /api/ssh-host-key
// are always reachable without a token, so `shed server add` can fetch server
// info + the host key before the operator holds a token.
//
// Scope: the credential bus (/api/plugins/*) and the Connect tunnel
// (/api/sheds/*/connect/*) require a credentials-scoped token; every other
// /api route requires a control- or admin-scoped token. So a leaked control
// token can't reach the bus, and a credentials token is bus-only.
func (s *Server) authMiddleware(next http.Handler) http.Handler {
	h := s.cfg.HTTPAuth()
	if h == nil || h.Mode != config.HTTPAuthEnforce {
		return next
	}
	scopes := make(map[string]string, len(h.Tokens)) // token -> scope, built once
	for _, t := range h.Tokens {
		scopes[t.Token] = t.Scope
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isBootstrapExempt(r) {
			next.ServeHTTP(w, r)
			return
		}
		scope, ok := scopes[bearerToken(r)]
		if !ok {
			writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "missing or invalid bearer token")
			return
		}
		if requiresCredentialsScope(r.URL.Path) {
			if scope != config.TokenScopeCredentials {
				writeError(w, http.StatusForbidden, "FORBIDDEN", "credentials scope required")
				return
			}
		} else if scope != config.TokenScopeControl && scope != config.TokenScopeAdmin {
			writeError(w, http.StatusForbidden, "FORBIDDEN", "control or admin scope required")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// isBootstrapExempt reports whether r targets an endpoint reachable without a
// token, so `shed server add` can bootstrap trust before holding one.
func isBootstrapExempt(r *http.Request) bool {
	return r.Method == http.MethodGet &&
		(r.URL.Path == "/api/info" || r.URL.Path == "/api/ssh-host-key")
}

// requiresCredentialsScope reports whether path is a credential-bus or Connect
// route, which require a credentials-scoped token. Shed names can't contain
// slashes, so "/connect/" only ever appears as the Connect tunnel route.
func requiresCredentialsScope(path string) bool {
	if path == "/api/plugins" || strings.HasPrefix(path, "/api/plugins/") {
		return true
	}
	return strings.HasPrefix(path, "/api/sheds/") && strings.Contains(path, "/connect/")
}

// bearerToken extracts the token from an "Authorization: Bearer <token>"
// header, or "" if absent/malformed.
func bearerToken(r *http.Request) string {
	const prefix = "Bearer "
	h := r.Header.Get("Authorization")
	if strings.HasPrefix(h, prefix) {
		return strings.TrimPrefix(h, prefix)
	}
	return ""
}
