package api

import (
	"crypto/x509"
	"net/http"
	"strings"
	"time"

	"github.com/charliek/shed/internal/audit"
	"github.com/charliek/shed/internal/authtoken"
	"github.com/charliek/shed/internal/config"
	"github.com/charliek/shed/internal/servertls"
	"github.com/charliek/shed/internal/users"
)

// authMiddleware enforces the server's auth.mode on every /api request. There
// are exactly two shapes:
//
//	open  — pass-through (the default; loopback only)
//	mtls  — a scoped client certificate, validated against the internal CA
//	        at the handshake and RE-validated here on every request
//
// The server holds no bearer-token code: the token mode was removed, and a
// config naming it does not load. An Authorization header is never read.
//
// The enforced branch consults one authorization table (authorizeScope): the
// credential bus (/api/plugins/*) requires credentials scope; the Connect
// tunnel (/api/sheds/*/connect/*) and the egress audit stream
// (GET /api/egress/stream) accept control or credentials; everything else
// requires control. So a leaked control credential can't reach the bus, and a
// credentials credential can't manage the fleet.
//
// mtls has no unauthenticated exemptions at all, /api/info included. It does
// not need them (a client that has not enrolled cannot complete the TLS
// handshake, so an exempt route is unreachable pre-certificate either way), and
// carving one out would mean a code path that skips certificate validation —
// the exact shape of an accidental fail-open. Everything an mtls client needs
// to bootstrap arrives in the SSH `_bootstrap` bundle instead.
//
// Any other mode string refuses every request. Config load never produces one
// (it refuses the removed token mode by name), so this guards a config built
// in code: a mode this server does not implement must not fall back to open.
func (s *Server) authMiddleware(next http.Handler) http.Handler {
	switch mode := s.cfg.AuthModeValue(); {
	case s.cfg.HTTPAuthEnforced():
		return s.certAuthMiddleware(next)
	case mode == config.AuthModeOpen:
		// Open mode: every caller is an admin (no subject).
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			next.ServeHTTP(w, r.WithContext(withCaller(r.Context(), Caller{Kind: CallerAdmin})))
		})
	default:
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			writeError(w, http.StatusInternalServerError, config.ErrInternalError, "server auth mode is not supported; use auth.mode: mtls")
		})
	}
}

// certAuthMiddleware is the mtls-mode branch. It re-derives the caller's
// identity from the presented client certificate on EVERY request rather than
// trusting the handshake, because a TLS peer is verified exactly once per
// connection and HTTP connections are pooled and long-lived:
//
//   - Expiry. A certificate that was valid when the connection was established
//     stays "verified" for the life of that connection as far as crypto/tls is
//     concerned. Re-checking NotBefore/NotAfter here is what actually bounds a
//     certificate's TTL.
//
//   - Authorization. The CA issues no CRL. Removing an SSH key from the
//     allowlist must de-authorize the certificates issued against it, so the
//     Subject CN is re-checked against the LIVE allowlist on every request, so
//     revocation lands on the next request.
//
// The Authorization header is not read on this path, at all. A bearer token can
// neither substitute for a certificate nor widen the scope one carries: the
// server holds no token store to validate against, and letting a header add
// authority to an authenticated connection would make the certificate's scope
// advisory.
//
// Known limitation: these checks run at request
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
// This is deliberate, not an oversight: bounding an already-established
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
			closeIdentityBoundConnection(w, r)
			writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "client certificate required")
			return
		}
		leaf := r.TLS.PeerCertificates[0]

		now := time.Now()
		valid := !now.Before(leaf.NotBefore) && !now.After(leaf.NotAfter)
		refuseInvalid := func(w http.ResponseWriter, r *http.Request) {
			closeIdentityBoundConnection(w, r)
			writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "client certificate is expired or not yet valid")
		}
		// A service certificate (auth.clients) is decided here and never
		// reaches the SSH allowlist; a malformed one, or one for a client that
		// is not configured, is refused rather than treated as a user's.
		//
		// Every service-certificate request, refused ones included, gets one
		// api line in the audit log: the certificate is classified first, and
		// its validity is checked inside the audit wrapper (an expired one
		// reaches here over a connection that opened while it was valid).
		if name, isService, err := servertls.ServiceClientName(leaf); isService {
			id := audit.Identity{Kind: audit.KindClient, Name: name, Serial: certSerial(leaf)}
			if err != nil {
				id.Name = leaf.Subject.CommonName
			}
			s.auditClientRequest(w, r, id, func(w http.ResponseWriter, r *http.Request) {
				if !valid {
					refuseInvalid(w, r)
					return
				}
				cc, known := s.cfg.ServiceClient(name)
				if err != nil || !known {
					closeIdentityBoundConnection(w, r)
					writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "client certificate is not authorized")
					return
				}
				s.serveServiceClient(w, r, next, name, cc, leaf)
			})
			return
		}
		if !valid {
			refuseInvalid(w, r)
			return
		}
		if !s.certSubjectAuthorized(leaf.Subject.CommonName) {
			closeIdentityBoundConnection(w, r)
			writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "client certificate is not authorized")
			return
		}
		if detail, ok := authorizeScope(r.Method, r.URL.Path, certScope(leaf)); !ok {
			writeError(w, http.StatusForbidden, "FORBIDDEN", detail)
			return
		}
		caller := Caller{Kind: CallerAdmin, Name: leaf.Subject.CommonName, Serial: certSerial(leaf)}
		next.ServeHTTP(w, r.WithContext(withCaller(r.Context(), caller)))
	})
}

// certSerial is a certificate's serial as lowercase hex ("" when it has none).
func certSerial(leaf *x509.Certificate) string {
	if leaf.SerialNumber == nil {
		return ""
	}
	return strings.ToLower(leaf.SerialNumber.Text(16))
}

// closeIdentityBoundConnection marks the response so the server tears the
// connection down after writing it. Called from the mtls branch's 401s — and
// only from those.
//
// In mtls mode the caller's identity is a property of the CONNECTION: the
// certificate is presented once, at the handshake, and TLS never re-asks. When
// certAuthMiddleware's per-request re-validation refuses that certificate —
// expired, or no longer allowlisted — the connection's identity is dead for
// every request that will ever arrive on it. Keeping it alive is actively
// harmful: a correct client answers a 401 by re-minting and retrying, and the
// retry goes out over the SAME pooled connection, still presenting the OLD
// certificate, and 401s again. That is a stuck loop the client cannot see its
// way out of (a TLS session is not re-negotiated per request, and neither Go's
// http.Client nor reqwest exposes "drop the connection this response arrived
// on"). Closing hands every client a clean slate for free: the next request
// dials, re-handshakes, and presents whatever credential it holds now.
//
// Scope failures (403) deliberately do NOT close. There the identity is valid
// and current; it simply may not reach this route. A new connection would
// present the same certificate and be refused identically, so closing would cost
// a handshake and fix nothing.
//
// HTTP/1.x only. "Connection" is a hop-by-hop header with no HTTP/2 meaning (h2
// signals with GOAWAY / RST_STREAM instead) and Go's h2 server refuses to emit
// it, so on a non-1.x request this is a no-op and recovery falls back to the
// pre-existing "next connection" behavior. Every shipped shed client negotiates
// HTTP/1.1 against the HTTPS listener.
func closeIdentityBoundConnection(w http.ResponseWriter, r *http.Request) {
	if r.ProtoMajor != 1 {
		return
	}
	// net/http honors a handler-set "Connection: close": (*chunkWriter).writeHeader
	// reads exactly this header value and sets closeAfterReply from it, which is
	// what makes the server close the connection once the response is written.
	// No r.Close fiddling is needed (and would not help — that field is an input
	// parsed from the REQUEST, not consulted after the handler returns).
	w.Header().Set("Connection", "close")
}

// authorizeScope is the scope table the mtls branch consults. It returns
// ok=false plus the client-facing detail when scope may not reach this route.
// The scope semantics are a property of the route, not of the credential.
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
// SSH key fingerprint) still resolves to a user through the live user lookup.
// A nil lookup (no SetUserLookup call) authorizes nothing — fail closed.
func (s *Server) certSubjectAuthorized(cn string) bool {
	return users.HasKey(s.users, cn)
}

// busOwnershipEnforced reports whether credential-bus /respond ownership is
// validated against the registry's pending set. It is gated on auth being
// enforced (mtls), so the default open-mode server keeps today's bus behavior.
func (s *Server) busOwnershipEnforced() bool {
	return s.cfg.AuthEnforced()
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
