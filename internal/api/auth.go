package api

import (
	"net/http"
	"strings"

	"github.com/charliek/shed/internal/authtoken"
)

// authMiddleware enforces bearer-token auth in token mode (auth.mode: token).
// In open mode (the default) it is a pass-through, so existing deployments are
// unaffected. mtls mode has no bearer tokens (it authenticates the client via
// certificate instead), so this middleware is not yet mtls-aware — S6 adds
// that branch.
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
// Scope: the credential bus (/api/plugins/*) requires a credentials-scoped
// token; the Connect tunnel (/api/sheds/*/connect/*) and the egress audit stream
// (/api/egress/stream) accept either a control or a credentials token (the CLI
// holds control; the host-agent's reverse proxy / egress subscriber holds
// credentials); every other /api route requires a control-scoped token. So a
// leaked control token can't reach the bus, and a credentials token can't manage
// the fleet.
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
		switch {
		case isCredentialBusPath(r.URL.Path):
			if rec.Scope != authtoken.ScopeCredentials {
				writeError(w, http.StatusForbidden, "FORBIDDEN", "credentials scope required")
				return
			}
		case isConnectRoute(r.URL.Path) || isEgressStreamPath(r.Method, r.URL.Path):
			// Neither is the credential bus. The Connect tunnel is a data-plane
			// port-forward (`shed forward` uses control; the host-agent's reverse
			// proxy uses credentials); the egress stream is fleet-global audit
			// metadata (host-agent subscriber uses credentials; a control token may
			// tail it too). Both accept either scope.
			if rec.Scope != authtoken.ScopeControl && rec.Scope != authtoken.ScopeCredentials {
				writeError(w, http.StatusForbidden, "FORBIDDEN", "control or credentials scope required")
				return
			}
		default:
			if rec.Scope != authtoken.ScopeControl {
				writeError(w, http.StatusForbidden, "FORBIDDEN", "control scope required")
				return
			}
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
// validated against the registry's pending set. It is gated on auth being
// enforced at all (token or mtls — the gate is about "is this server
// authenticated", not about which credential form carries it), so the default
// open-mode fleet keeps today's bus behavior.
func (s *Server) busOwnershipEnforced() bool {
	return s.cfg.AuthEnforced()
}

// isBootstrapExempt reports whether r targets an endpoint reachable without a
// token, so `shed server add` can bootstrap trust before holding one.
func isBootstrapExempt(r *http.Request) bool {
	return r.Method == http.MethodGet &&
		(r.URL.Path == "/api/info" || r.URL.Path == "/api/ssh-host-key")
}

// isCredentialBusPath reports whether path is the credential bus (/api/plugins*),
// which requires a credentials-scoped token so a leaked control token can't
// subscribe to the live-secret bus.
func isCredentialBusPath(path string) bool {
	return path == "/api/plugins" || strings.HasPrefix(path, "/api/plugins/")
}

// isConnectRoute reports whether path is the Connect tunnel
// (/api/sheds/<name>/connect/<port>), which accepts a control OR credentials
// token. It matches the exact route shape — three segments after /api/sheds/
// with "connect" in the middle — rather than a "/connect/" substring, so a shed
// literally named "connect" (e.g. POST /api/sheds/connect/start) is NOT
// misclassified as the tunnel and stays control-only.
func isConnectRoute(path string) bool {
	rest, ok := strings.CutPrefix(path, "/api/sheds/")
	if !ok {
		return false
	}
	parts := strings.Split(rest, "/")
	return len(parts) == 3 && parts[1] == "connect" && parts[0] != "" && parts[2] != ""
}

// isEgressStreamPath reports whether r targets the fleet-global egress audit SSE
// stream (GET /api/egress/stream), consumed by the host-agent (credentials) and
// tailable by the CLI (control). It is method-scoped to GET on purpose: the
// sibling POST/DELETE /api/egress/{name} mutators (handleEgressSet/Off) are
// control-only, so a shed named "stream" must not let a credentials token onto
// POST/DELETE /api/egress/stream. Only this exact GET is dual-scope; every other
// /api/egress/* route (profiles, per-shed show/set/off) stays control-only.
func isEgressStreamPath(method, path string) bool {
	return method == http.MethodGet && path == "/api/egress/stream"
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
