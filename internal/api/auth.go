package api

import (
	"crypto/x509"
	"net/http"
	"strings"
	"time"

	"github.com/charliek/shed/internal/authtoken"
)

// authMiddleware enforces the server's auth.mode on every /api request. There
// are exactly three shapes:
//
//	open  — pass-through (the default; unchanged legacy tailnet/LAN posture)
//	token — a scoped bearer token, validated against the in-memory store
//	mtls  — a scoped client certificate, validated against the internal CA
//	        at the handshake and RE-validated here on every request
//
// The two enforced branches share one authorization table (authorizeScope):
// the credential bus (/api/plugins/*) requires credentials scope; the Connect
// tunnel (/api/sheds/*/connect/*) and the egress audit stream
// (GET /api/egress/stream) accept control or credentials; everything else
// requires control. So a leaked control credential can't reach the bus, and a
// credentials credential can't manage the fleet — identically in both modes.
//
// Bootstrap exemptions (GET /api/info, GET /api/ssh-host-key) are a TOKEN-MODE
// concept: they let `shed server add` fetch server info + the host key before
// the operator holds a token. mtls mode has no exemptions at all. It does not
// need them (a client that has not enrolled cannot complete the TLS handshake,
// so an exempt route is unreachable pre-certificate either way), and carving
// one out would mean a code path in the mtls branch that skips certificate
// validation — the exact shape of an accidental fail-open. Everything an mtls
// client needs to bootstrap arrives in the SSH `_bootstrap` bundle instead.
func (s *Server) authMiddleware(next http.Handler) http.Handler {
	if !s.cfg.HTTPAuthEnforced() {
		return next
	}
	if s.cfg.MTLSMode() {
		return s.certAuthMiddleware(next)
	}
	return s.tokenAuthMiddleware(next)
}

// tokenAuthMiddleware is the token-mode branch: an "Authorization: Bearer"
// token, validated against the in-memory store (authtoken.Store), not the token
// string alone — each is a short-lived record minted over the SSH bootstrap
// channel and bound to the minting key. The store is shared with the bootstrap
// handler via SetTokenStore.
func (s *Server) tokenAuthMiddleware(next http.Handler) http.Handler {
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
		if detail, ok := authorizeScope(r.Method, r.URL.Path, rec.Scope); !ok {
			writeError(w, http.StatusForbidden, "FORBIDDEN", detail)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// certAuthMiddleware is the mtls-mode branch. It re-derives the caller's
// identity from the presented client certificate on EVERY request rather than
// trusting the handshake, because a TLS peer is verified exactly once per
// connection and HTTP connections are pooled and long-lived:
//
//   - Expiry. A certificate that was valid when the connection was established
//     stays "verified" for the life of that connection as far as crypto/tls is
//     concerned. Re-checking NotBefore/NotAfter here is what actually bounds a
//     certificate's TTL, and is the direct analogue of the token store dropping
//     an expired token on validate.
//
//   - Authorization. The CA issues no CRL. Removing an SSH key from the
//     allowlist must de-authorize the certificates issued against it, so the
//     Subject CN is re-checked against the LIVE allowlist on every request —
//     giving mtls the same "revocation lands on the next request" property that
//     token mode gets from RevokeBySubject.
//
// The Authorization header is not read on this path, at all. A bearer token can
// neither substitute for a certificate nor widen the scope one carries: in mtls
// mode the server holds no token store to validate against, and even if it did,
// letting a header add authority to an authenticated connection would make the
// certificate's scope advisory.
//
// Known parity limitation, shared with token mode: these checks run at request
// dispatch, so a connection that was authorized when it opened is not torn down
// when its credential expires or is revoked mid-flight. Two route families are
// long-lived enough for that to be observable:
//
//   - SSE streams — the create/pull progress feeds, the plugin bus, the egress
//     audit stream. Each keeps emitting until the client or the server closes it.
//
//   - The Connect tunnel (/api/sheds/{name}/connect/{port}). It is the stronger
//     case, and worth naming rather than leaving implied: handleConnect hijacks
//     the connection and hands it to a raw byte-copy loop, after which there is
//     no HTTP request boundary left for any middleware to run on. An established
//     port-forward therefore survives certificate expiry and allowlist removal
//     for as long as it stays open — potentially indefinitely.
//
// This is deliberate, not an oversight: it is exactly token mode's behavior (an
// expired bearer token does not tear down a live stream or an open tunnel
// either), so mtls introduces no regression. Bounding an already-established
// connection is a separate mechanism from authenticating a request, and is not
// something this middleware can do. Revocation binds on the NEXT request, which
// for the tunnel means the next `shed forward` / reverse-proxy dial.
func (s *Server) certAuthMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Fail closed on anything that is not a TLS request carrying a peer
		// certificate. Under RequireAndVerifyClientCert the handshake would
		// already have failed, so reaching here means a misrouted plaintext
		// listener or a test harness — either way, not authenticated.
		if r.TLS == nil || len(r.TLS.PeerCertificates) == 0 {
			writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "client certificate required")
			return
		}
		leaf := r.TLS.PeerCertificates[0]

		now := time.Now()
		if now.Before(leaf.NotBefore) || now.After(leaf.NotAfter) {
			writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "client certificate is expired or not yet valid")
			return
		}
		if !s.certSubjectAuthorized(leaf.Subject.CommonName) {
			writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "client certificate is not authorized")
			return
		}
		if detail, ok := authorizeScope(r.Method, r.URL.Path, certScope(leaf)); !ok {
			writeError(w, http.StatusForbidden, "FORBIDDEN", detail)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// authorizeScope is the single scope table both enforced modes consult. It
// returns ok=false plus the client-facing detail when scope may not reach this
// route. Keeping one function means a token and a certificate carrying the same
// scope string reach exactly the same route set — the scope semantics are a
// property of the route, not of the credential shape.
func authorizeScope(method, path, scope string) (detail string, ok bool) {
	switch {
	case isCredentialBusPath(path):
		if scope != authtoken.ScopeCredentials {
			return "credentials scope required", false
		}
	case isConnectRoute(path) || isEgressStreamPath(method, path):
		// Neither is the credential bus. The Connect tunnel is a data-plane
		// port-forward (`shed forward` uses control; the host-agent's reverse
		// proxy uses credentials); the egress stream is fleet-global audit
		// metadata (host-agent subscriber uses credentials; a control credential
		// may tail it too). Both accept either scope.
		if scope != authtoken.ScopeControl && scope != authtoken.ScopeCredentials {
			return "control or credentials scope required", false
		}
	default:
		if scope != authtoken.ScopeControl {
			return "control scope required", false
		}
	}
	return "", true
}

// certScope extracts the scope a client certificate carries: the single
// Subject OU the CA burns in at issuance (see servertls.CA.SignClientCSR).
//
// Anything other than exactly one OU yields "" — a scope no route accepts, so
// the request is refused with 403 rather than defaulted. Zero OUs is a
// certificate issued with an empty scope; more than one is an ambiguity this
// CA never produces, and picking the first would let a hand-crafted subject
// hide a broader scope behind a narrower one.
func certScope(leaf *x509.Certificate) string {
	if len(leaf.Subject.OrganizationalUnit) != 1 {
		return ""
	}
	return leaf.Subject.OrganizationalUnit[0]
}

// certSubjectAuthorized reports whether a client certificate's Subject CN (an
// SSH key fingerprint) is still in the live SSH allowlist. A nil authorizer
// (no SetClientCertAuthorizer call) authorizes nothing — fail closed, the same
// way a nil token store validates nothing.
func (s *Server) certSubjectAuthorized(cn string) bool {
	if s.certAuthorized == nil {
		return false
	}
	return s.certAuthorized(cn)
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
// token, so `shed server add` can bootstrap trust before holding one. Consulted
// only on the token-mode path — mtls has no exemptions (see authMiddleware).
func isBootstrapExempt(r *http.Request) bool {
	return r.Method == http.MethodGet &&
		(r.URL.Path == "/api/info" || r.URL.Path == "/api/ssh-host-key")
}

// isCredentialBusPath reports whether path is the credential bus (/api/plugins*),
// which requires a credentials-scoped credential so a leaked control one can't
// subscribe to the live-secret bus.
func isCredentialBusPath(path string) bool {
	return path == "/api/plugins" || strings.HasPrefix(path, "/api/plugins/")
}

// isConnectRoute reports whether path is the Connect tunnel
// (/api/sheds/<name>/connect/<port>), which accepts a control OR credentials
// credential. It matches the exact route shape — three segments after
// /api/sheds/ with "connect" in the middle — rather than a "/connect/"
// substring, so a shed literally named "connect" (e.g. POST
// /api/sheds/connect/start) is NOT misclassified as the tunnel and stays
// control-only.
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
// control-only, so a shed named "stream" must not let a credentials credential
// onto POST/DELETE /api/egress/stream. Only this exact GET is dual-scope; every
// other /api/egress/* route (profiles, per-shed show/set/off) stays control-only.
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
