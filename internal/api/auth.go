package api

import (
	"net/http"
	"strings"

	"github.com/charliek/shed/internal/authtoken"
)

// authMiddleware enforces bearer-token auth in secure mode (auth.mode: secure).
// In open mode (the default) it is a pass-through, so existing deployments are
// unaffected.
//
// Exemptions: the bootstrap endpoints GET /api/info and GET /api/ssh-host-key
// are always reachable without a token, so `shed server add` can fetch server
// info + the host key before the operator holds a token.
//
// Tokens are validated against the in-memory store (authtoken.Store), not the
// token string alone: each is a short-lived record minted over the SSH
// bootstrap channel and bound to the minting key. The store is shared with the
// bootstrap handler via SetTokenStore.
//
// Scope: the credential bus (/api/plugins/*) and the Connect tunnel
// (/api/sheds/*/connect/*) require a credentials-scoped token; every other
// /api route requires a control-scoped token. So a leaked control token can't
// reach the bus, and a credentials token is bus-only.
func (s *Server) authMiddleware(next http.Handler) http.Handler {
	if !s.cfg.HTTPAuthEnforced() {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isBootstrapExempt(r) {
			next.ServeHTTP(w, r)
			return
		}
		rec, ok := s.validateToken(bearerToken(r))
		if !ok {
			writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "missing or invalid bearer token")
			return
		}
		if requiresCredentialsScope(r.URL.Path) {
			if rec.Scope != authtoken.ScopeCredentials {
				writeError(w, http.StatusForbidden, "FORBIDDEN", "credentials scope required")
				return
			}
		} else if rec.Scope != authtoken.ScopeControl {
			writeError(w, http.StatusForbidden, "FORBIDDEN", "control scope required")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// validateToken looks up a bearer token in the store. A nil store (no
// SetTokenStore call) validates nothing — fail closed.
func (s *Server) validateToken(tok string) (authtoken.PublicRecord, bool) {
	if s.tokens == nil {
		return authtoken.PublicRecord{}, false
	}
	return s.tokens.Validate(tok)
}

// busOwnershipEnforced reports whether credential-bus /respond ownership is
// validated against the registry's pending set. It is gated on HTTP auth being
// on, so the default token-less fleet keeps today's bus behavior.
func (s *Server) busOwnershipEnforced() bool {
	return s.cfg.HTTPAuthEnforced()
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
