package api

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/charliek/shed/internal/authtoken"
	"github.com/charliek/shed/internal/config"
	"github.com/charliek/shed/internal/servertls"
)

// ---------------------------------------------------------------------------
// Synthetic-certificate helpers (middleware-level tests)
//
// The mtls middleware never verifies a signature — chain verification is the
// TLS layer's job, and by the time a request is dispatched the certificate has
// already been verified against the CA. The middleware reads exactly three
// things off the leaf: the validity window, the Subject CN, and the Subject OU.
// So a middleware-level test can hand it a hand-built *x509.Certificate and
// exercise the real decision logic without a handshake. The end-to-end TLS
// tests further down cover what only a real handshake can.
// ---------------------------------------------------------------------------

// testFingerprint renders label as a canonical OpenSSH SHA-256 fingerprint —
// "SHA256:" plus 43 characters of unpadded base64, byte-identical in shape to
// what gossh.FingerprintSHA256 produces for a real key. The CA validates that
// shape at issuance (servertls.ErrCAInvalidSubject), so a test identity has to
// be a real fingerprint and not a readable placeholder.
func testFingerprint(label string) string {
	sum := sha256.Sum256([]byte(label))
	return "SHA256:" + base64.RawStdEncoding.EncodeToString(sum[:])
}

// testClientCert builds an unsigned leaf carrying the identity the middleware
// reads. A negative ttl produces an already-expired certificate.
func testClientCert(cn, scope string, ttl time.Duration) *x509.Certificate {
	now := time.Now()
	cert := &x509.Certificate{
		Subject:   pkix.Name{CommonName: cn},
		NotBefore: now.Add(-time.Minute),
		NotAfter:  now.Add(ttl),
	}
	if scope != "" {
		cert.Subject.OrganizationalUnit = []string{scope}
	}
	return cert
}

// withClientCert attaches peer certificates to r as a completed TLS handshake
// would, so the request looks to the middleware exactly like one arriving on an
// mtls connection.
func withClientCert(r *http.Request, certs ...*x509.Certificate) *http.Request {
	r.TLS = &tls.ConnectionState{HandshakeComplete: true, PeerCertificates: certs}
	return r
}

// mtlsTestServer builds an mtls-mode API server whose allowlist contains
// exactly the fingerprints in authorized, and whose token store holds one valid
// control token (returned) so tests can prove a bearer token is ignored on this
// path.
func mtlsTestServer(t *testing.T, authorized ...string) (s *Server, controlToken string) {
	t.Helper()
	allowed := make(map[string]bool, len(authorized))
	for _, fp := range authorized {
		allowed[fp] = true
	}
	store := authtoken.NewStore()
	controlToken, _, err := store.Mint("SHA256:test", authtoken.ScopeControl, authtoken.ClientCLI, time.Hour)
	if err != nil {
		t.Fatalf("mint control token: %v", err)
	}
	s = &Server{
		cfg:            &config.ServerConfig{Auth: &config.AuthConfig{Mode: config.AuthModeMTLS}},
		tokens:         store,
		certAuthorized: func(fp string) bool { return allowed[fp] },
	}
	return s, controlToken
}

// doCert runs one request through the middleware with the given peer
// certificates (none = a request with no client certificate at all).
func doCert(s *Server, method, path string, certs ...*x509.Certificate) int {
	h := s.authMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequest(method, path, nil)
	if len(certs) > 0 {
		withClientCert(req, certs...)
	}
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr.Code
}

// TestMTLSMiddlewareScopeParity is the mtls twin of TestAuthMiddlewareEnforce:
// the same route/scope table, driven by certificate OU instead of token scope.
// The two tables must agree route-for-route — scope semantics belong to the
// route, not to the credential shape.
func TestMTLSMiddlewareScopeParity(t *testing.T) {
	fp := testFingerprint("allowed")
	s, _ := mtlsTestServer(t, fp)
	ctl := testClientCert(fp, authtoken.ScopeControl, time.Hour)
	cred := testClientCert(fp, authtoken.ScopeCredentials, time.Hour)

	tests := []struct {
		name, method, path string
		cert               *x509.Certificate
		want               int
	}{
		// mtls has NO bootstrap exemptions: the routes token mode leaves open
		// are certificate-gated here like every other route.
		{"bootstrap info, no cert", "GET", "/api/info", nil, 401},
		{"bootstrap host-key, no cert", "GET", "/api/ssh-host-key", nil, 401},
		{"bootstrap info, control cert", "GET", "/api/info", ctl, 200},
		{"bootstrap host-key, control cert", "GET", "/api/ssh-host-key", ctl, 200},

		{"control plane, no cert", "GET", "/api/sheds", nil, 401},
		{"control plane, control cert", "GET", "/api/sheds", ctl, 200},
		{"control plane, credentials cert forbidden", "GET", "/api/sheds", cred, 403},
		{"bus, credentials cert", "GET", "/api/plugins/listeners", cred, 200},
		{"bus, control cert forbidden", "GET", "/api/plugins/listeners", ctl, 403},
		{"connect, control cert", "GET", "/api/sheds/x/connect/22", ctl, 200},
		{"connect, credentials cert", "GET", "/api/sheds/x/connect/22", cred, 200},
		{"connect, no cert", "GET", "/api/sheds/x/connect/22", nil, 401},
		{"shed named connect, control cert", "POST", "/api/sheds/connect/start", ctl, 200},
		{"shed named connect, credentials cert forbidden", "POST", "/api/sheds/connect/start", cred, 403},
		{"connect prefix without port, credentials forbidden", "GET", "/api/sheds/x/connect", cred, 403},
		{"connect with trailing segment, credentials forbidden", "GET", "/api/sheds/x/connect/22/extra", cred, 403},
		{"egress stream, control cert", "GET", "/api/egress/stream", ctl, 200},
		{"egress stream, credentials cert", "GET", "/api/egress/stream", cred, 200},
		{"egress stream, no cert", "GET", "/api/egress/stream", nil, 401},
		{"egress profiles, control cert", "GET", "/api/egress/profiles", ctl, 200},
		{"egress profiles, credentials cert forbidden", "GET", "/api/egress/profiles", cred, 403},
		{"egress per-shed, credentials cert forbidden", "GET", "/api/egress/myshed", cred, 403},
		{"egress stream POST, credentials forbidden", "POST", "/api/egress/stream", cred, 403},
		{"egress stream DELETE, credentials forbidden", "DELETE", "/api/egress/stream", cred, 403},
		{"egress stream POST, control cert", "POST", "/api/egress/stream", ctl, 200},
		{"create, control cert", "POST", "/api/sheds", ctl, 200},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var certs []*x509.Certificate
			if tt.cert != nil {
				certs = []*x509.Certificate{tt.cert}
			}
			if got := doCert(s, tt.method, tt.path, certs...); got != tt.want {
				t.Errorf("got %d, want %d", got, tt.want)
			}
		})
	}
}

// TestMTLSMiddlewareFailsClosed covers every way a request can fail to present
// a usable identity. Each must be refused, and none may fall through to a
// bearer-token or open-mode path.
func TestMTLSMiddlewareFailsClosed(t *testing.T) {
	fp := testFingerprint("allowed")

	t.Run("no TLS at all", func(t *testing.T) {
		s, _ := mtlsTestServer(t, fp)
		if got := doCert(s, "GET", "/api/sheds"); got != 401 {
			t.Errorf("plaintext request: got %d, want 401", got)
		}
	})

	t.Run("TLS with no peer certificate", func(t *testing.T) {
		s, _ := mtlsTestServer(t, fp)
		h := s.authMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		}))
		req := httptest.NewRequest("GET", "/api/sheds", nil)
		req.TLS = &tls.ConnectionState{HandshakeComplete: true}
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		if rr.Code != 401 {
			t.Errorf("empty PeerCertificates: got %d, want 401", rr.Code)
		}
	})

	t.Run("expired certificate", func(t *testing.T) {
		s, _ := mtlsTestServer(t, fp)
		expired := testClientCert(fp, authtoken.ScopeControl, -time.Second)
		if got := doCert(s, "GET", "/api/sheds", expired); got != 401 {
			t.Errorf("expired cert: got %d, want 401", got)
		}
	})

	t.Run("not yet valid certificate", func(t *testing.T) {
		s, _ := mtlsTestServer(t, fp)
		future := testClientCert(fp, authtoken.ScopeControl, time.Hour)
		future.NotBefore = time.Now().Add(time.Hour)
		if got := doCert(s, "GET", "/api/sheds", future); got != 401 {
			t.Errorf("not-yet-valid cert: got %d, want 401", got)
		}
	})

	t.Run("CN not in allowlist", func(t *testing.T) {
		s, _ := mtlsTestServer(t, fp)
		stranger := testClientCert(testFingerprint("stranger"), authtoken.ScopeControl, time.Hour)
		if got := doCert(s, "GET", "/api/sheds", stranger); got != 401 {
			t.Errorf("unlisted CN: got %d, want 401", got)
		}
	})

	t.Run("nil authorizer authorizes nothing", func(t *testing.T) {
		s, _ := mtlsTestServer(t)
		s.certAuthorized = nil
		valid := testClientCert(fp, authtoken.ScopeControl, time.Hour)
		if got := doCert(s, "GET", "/api/sheds", valid); got != 401 {
			t.Errorf("nil authorizer: got %d, want 401", got)
		}
	})

	t.Run("no usable scope", func(t *testing.T) {
		s, _ := mtlsTestServer(t, fp)
		// An OU-less certificate carries no scope, and no route grants one by
		// default: authenticated but unauthorized (403), never defaulted to
		// control.
		noScope := testClientCert(fp, "", time.Hour)
		if got := doCert(s, "GET", "/api/sheds", noScope); got != 403 {
			t.Errorf("no-OU cert on control route: got %d, want 403", got)
		}
		if got := doCert(s, "GET", "/api/plugins/listeners", noScope); got != 403 {
			t.Errorf("no-OU cert on bus: got %d, want 403", got)
		}
		// Two OUs is an ambiguity this CA never issues; picking either one
		// would let a hand-crafted subject hide a broad scope behind a narrow
		// one, so it is treated as no scope at all.
		ambiguous := testClientCert(fp, authtoken.ScopeControl, time.Hour)
		ambiguous.Subject.OrganizationalUnit = []string{authtoken.ScopeCredentials, authtoken.ScopeControl}
		if got := doCert(s, "GET", "/api/sheds", ambiguous); got != 403 {
			t.Errorf("multi-OU cert: got %d, want 403", got)
		}
		// An unknown scope string is not a wildcard.
		bogus := testClientCert(fp, "admin", time.Hour)
		if got := doCert(s, "GET", "/api/sheds", bogus); got != 403 {
			t.Errorf("unknown-scope cert: got %d, want 403", got)
		}
	})
}

// TestMTLSProductionRouterEnforces drives the PRODUCTION router — the exact
// chi.Router shed-server serves, with its real middleware stack — instead of
// calling authMiddleware directly.
//
// This is the regression the middleware-level tests above structurally cannot
// catch: they invoke s.authMiddleware by hand, so they would all still pass if
// someone dropped `r.Use(s.authMiddleware)` from useCommonMiddleware and left
// every route wide open. Here the middleware is reached only because the router
// installs it.
//
// The routes exercised are ones that need no backend: /api/info and
// /api/ssh-host-key are handled from server state alone. Every refusal below is
// produced before dispatch, so no handler runs regardless.
func TestMTLSProductionRouterEnforces(t *testing.T) {
	fp := testFingerprint("allowed")

	newRouterServer := func() *Server {
		srv := NewServer(nil, &config.ServerConfig{
			Name: "test-server",
			Auth: &config.AuthConfig{Mode: config.AuthModeMTLS},
		}, "ssh-ed25519 AAAAtest", nil, nil)
		srv.SetClientCertAuthorizer(func(f string) bool { return f == fp })
		return srv
	}

	do := func(t *testing.T, method, path string, cert *x509.Certificate) int {
		t.Helper()
		r := httptest.NewRequest(method, path, nil)
		if cert != nil {
			withClientCert(r, cert)
		}
		w := httptest.NewRecorder()
		newRouterServer().Router().ServeHTTP(w, r)
		return w.Code
	}

	ctl := testClientCert(fp, authtoken.ScopeControl, time.Hour)
	cred := testClientCert(fp, authtoken.ScopeCredentials, time.Hour)

	tests := []struct {
		name, method, path string
		cert               *x509.Certificate
		want               int
	}{
		// mtls has no bootstrap exemptions: the two routes token mode leaves
		// open are certificate-gated here. If the router stops installing the
		// auth middleware, these two return 200 and this test fails.
		{"info, no certificate", "GET", "/api/info", nil, 401},
		{"ssh-host-key, no certificate", "GET", "/api/ssh-host-key", nil, 401},

		{"info, expired certificate", "GET", "/api/info",
			testClientCert(fp, authtoken.ScopeControl, -time.Second), 401},
		{"info, unlisted CN", "GET", "/api/info",
			testClientCert(testFingerprint("stranger"), authtoken.ScopeControl, time.Hour), 401},
		// Authenticated but wrong scope: /api/info is a control route, so a
		// credentials certificate is authenticated (401 would be wrong) and
		// refused (403).
		{"info, credentials certificate", "GET", "/api/info", cred, 403},
		{"info, no-scope certificate", "GET", "/api/info",
			testClientCert(fp, "", time.Hour), 403},

		// ...and the allow direction, so the test cannot be satisfied by a
		// middleware that simply refuses everything.
		{"info, control certificate", "GET", "/api/info", ctl, 200},
		{"ssh-host-key, control certificate", "GET", "/api/ssh-host-key", ctl, 200},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := do(t, tt.method, tt.path, tt.cert); got != tt.want {
				t.Errorf("%s %s: got %d, want %d", tt.method, tt.path, got, tt.want)
			}
		})
	}
}

// TestMTLSRouterOverRealTLSRejectsMissingCertificate is the end-to-end shape of
// the same argument: a real TLS listener, the real production router behind it,
// and a client with no certificate. The request must die at the handshake — it
// may not become an HTTP request at all, let alone reach a route.
//
// The counter wraps the router from the OUTSIDE, so it registers a hit for any
// request that reaches HTTP dispatch, whether or not a route matches. Zero hits
// is therefore the strong claim: nothing was served.
func TestMTLSRouterOverRealTLSRejectsMissingCertificate(t *testing.T) {
	fp := testFingerprint("allowed")

	for _, tc := range []struct {
		name       string
		maxVersion uint16
	}{
		{"TLS 1.2", tls.VersionTLS12},
		{"TLS 1.3", tls.VersionTLS13},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := &mtlsHarness{ca: testCA(t), hits: &atomic.Int64{}}
			h.setAuthorized(func(f string) bool { return f == fp })

			api := NewServer(nil, &config.ServerConfig{
				Name: "test-server",
				Auth: &config.AuthConfig{Mode: config.AuthModeMTLS},
			}, "ssh-ed25519 AAAAtest", nil, nil)
			api.SetClientCertAuthorizer(h.authorized)
			router := api.Router()

			srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				h.hits.Add(1)
				router.ServeHTTP(w, r)
			}))
			srv.TLS = h.tlsConfig()
			srv.StartTLS()
			t.Cleanup(srv.Close)
			h.srv = srv

			resp, err := h.get(t, h.client(nil, tc.maxVersion), "/api/info")
			if err == nil {
				t.Errorf("no-certificate request reached the server (HTTP %d) — "+
					"the handshake must fail", resp.StatusCode)
			}
			if got := h.hits.Load(); got != 0 {
				t.Errorf("router served %d requests, want 0", got)
			}

			// Control: an allowlisted certificate DOES get through the same
			// listener and router, so the zero above is the client certificate's
			// absence and not a broken harness.
			good := issueClientCert(t, h.ca, fp, authtoken.ScopeControl, time.Hour)
			resp, err = h.get(t, h.client(&good, tc.maxVersion), "/api/info")
			if err != nil {
				t.Fatalf("valid certificate rejected: %v", err)
			}
			if resp.StatusCode != http.StatusOK {
				t.Errorf("valid certificate: got %d, want 200", resp.StatusCode)
			}
			if got := h.hits.Load(); got != 1 {
				t.Errorf("router served %d requests, want 1", got)
			}
		})
	}
}

// TestMTLSIgnoresAuthorizationHeader proves a bearer token can neither
// substitute for a client certificate nor widen the scope one carries. The
// token used here is genuinely valid in the server's store — it is simply never
// consulted on the mtls path.
func TestMTLSIgnoresAuthorizationHeader(t *testing.T) {
	fp := testFingerprint("allowed")
	s, controlToken := mtlsTestServer(t, fp)

	run := func(path string, cert *x509.Certificate) int {
		h := s.authMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		}))
		req := httptest.NewRequest("GET", path, nil)
		req.Header.Set("Authorization", "Bearer "+controlToken)
		if cert != nil {
			withClientCert(req, cert)
		}
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		return rr.Code
	}

	// A valid control token cannot substitute for a certificate.
	if got := run("/api/sheds", nil); got != 401 {
		t.Errorf("valid token, no cert: got %d, want 401", got)
	}
	// A valid control token alongside a credentials-scoped certificate does not
	// grant control: the certificate's scope is the only scope.
	cred := testClientCert(fp, authtoken.ScopeCredentials, time.Hour)
	if got := run("/api/sheds", cred); got != 403 {
		t.Errorf("control token + credentials cert on control route: got %d, want 403", got)
	}
	// ...and in the other direction: a control token does not open the bus to a
	// control-scoped certificate.
	ctl := testClientCert(fp, authtoken.ScopeControl, time.Hour)
	if got := run("/api/plugins/listeners", ctl); got != 403 {
		t.Errorf("control token + control cert on bus: got %d, want 403", got)
	}
	// A de-authorized identity is not rescued by holding a valid token.
	stranger := testClientCert(testFingerprint("stranger"), authtoken.ScopeControl, time.Hour)
	if got := run("/api/sheds", stranger); got != 401 {
		t.Errorf("valid token + unlisted cert: got %d, want 401", got)
	}
}

// TestMTLSInfoReportsCA verifies the operator-visibility fields on /api/info:
// present in mtls mode, absent everywhere else (a token/open server has no
// client CA to report, and an empty string must not appear as a key).
func TestMTLSInfoReportsCA(t *testing.T) {
	fp := testFingerprint("allowed")
	notAfter := time.Date(2031, 4, 5, 6, 7, 8, 0, time.UTC)

	newInfoServer := func(mode string) *Server {
		srv := NewServer(nil, &config.ServerConfig{
			Name: "test-server",
			Auth: &config.AuthConfig{Mode: mode},
		}, "", nil, nil)
		srv.SetClientCertAuthorizer(func(string) bool { return true })
		srv.SetClientCAInfo("sha256:deadbeef", notAfter)
		return srv
	}

	getInfo := func(t *testing.T, srv *Server, cert *x509.Certificate) map[string]json.RawMessage {
		t.Helper()
		r := httptest.NewRequest(http.MethodGet, "/api/info", nil)
		if cert != nil {
			withClientCert(r, cert)
		}
		w := httptest.NewRecorder()
		srv.Router().ServeHTTP(w, r)
		if w.Code != http.StatusOK {
			t.Fatalf("GET /api/info: got %d: %s", w.Code, w.Body.String())
		}
		var raw map[string]json.RawMessage
		if err := json.Unmarshal(w.Body.Bytes(), &raw); err != nil {
			t.Fatalf("parse /api/info: %v", err)
		}
		return raw
	}

	t.Run("mtls reports both fields", func(t *testing.T) {
		raw := getInfo(t, newInfoServer(config.AuthModeMTLS),
			testClientCert(fp, authtoken.ScopeControl, time.Hour))
		var info config.ServerInfo
		body, _ := json.Marshal(raw)
		if err := json.Unmarshal(body, &info); err != nil {
			t.Fatalf("decode ServerInfo: %v", err)
		}
		if info.CAFingerprint != "sha256:deadbeef" {
			t.Errorf("ca_fingerprint=%q, want sha256:deadbeef", info.CAFingerprint)
		}
		if want := notAfter.Format(time.RFC3339); info.CANotAfter != want {
			t.Errorf("ca_not_after=%q, want %q", info.CANotAfter, want)
		}
	})

	t.Run("token mode omits both fields", func(t *testing.T) {
		// Token mode leaves /api/info bootstrap-exempt, so no credential needed.
		raw := getInfo(t, newInfoServer(config.AuthModeToken), nil)
		for _, key := range []string{"ca_fingerprint", "ca_not_after"} {
			if _, present := raw[key]; present {
				t.Errorf("%s present in token mode", key)
			}
		}
	})
}

// ---------------------------------------------------------------------------
// End-to-end TLS tests
//
// These drive a real handshake against a real listener configured the way
// shed-server configures its HTTPS listener in mtls mode: ClientCAs from the
// internal CA, RequireAndVerifyClientCert, and the live-allowlist
// VerifyConnection. They cover what a synthetic request cannot — that a
// rejection happens at the handshake and the handler is never reached, and that
// the per-request re-validation actually fires on a REUSED connection.
//
// The listener's tls.Config here is a mirror of production, not production
// itself; cmd/shed-server's TestBuildHTTPSTLSConfig is what pins the real
// assembly (package main is not importable from here).
// ---------------------------------------------------------------------------

// testCA mints a fresh internal CA in a temp dir.
func testCA(t *testing.T) servertls.CA {
	t.Helper()
	dir := t.TempDir()
	ca, err := servertls.LoadOrGenerateCA(filepath.Join(dir, "ca.pem"), filepath.Join(dir, "ca.key"))
	if err != nil {
		t.Fatalf("generate CA: %v", err)
	}
	return ca
}

// issueClientCert runs the real enrollment path: build a P-256 CSR, have the CA
// sign it with the given identity, and assemble the tls.Certificate a client
// would present. Using SignClientCSR rather than a hand-rolled leaf keeps these
// tests honest about the subject shape the server actually issues.
func issueClientCert(t *testing.T, ca servertls.CA, fingerprint, scope string, ttl time.Duration) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate client key: %v", err)
	}
	csrDER, err := x509.CreateCertificateRequest(rand.Reader,
		&x509.CertificateRequest{SignatureAlgorithm: x509.ECDSAWithSHA256}, key)
	if err != nil {
		t.Fatalf("create CSR: %v", err)
	}
	certDER, err := ca.SignClientCSR(csrDER, fingerprint, scope, authtoken.ClientCLI, ttl)
	if err != nil {
		t.Fatalf("sign client CSR: %v", err)
	}
	leaf, err := x509.ParseCertificate(certDER)
	if err != nil {
		t.Fatalf("parse issued cert: %v", err)
	}
	return tls.Certificate{Certificate: [][]byte{certDER}, PrivateKey: key, Leaf: leaf}
}

// mtlsHarness is a running TLS listener wired like shed-server's mtls HTTPS
// listener, plus the knobs a test needs to observe and mutate it.
type mtlsHarness struct {
	srv *httptest.Server
	ca  servertls.CA
	// hits counts handler invocations. A rejected request must never bump it.
	hits *atomic.Int64
	// authorize is the live allowlist predicate, swappable mid-test to simulate
	// a key leaving the allowlist.
	authorize atomic.Value // func(string) bool
}

func (h *mtlsHarness) setAuthorized(fn func(string) bool) { h.authorize.Store(fn) }

func (h *mtlsHarness) authorized(fp string) bool {
	return h.authorize.Load().(func(string) bool)(fp)
}

// tlsConfig mirrors what cmd/shed-server's buildHTTPSTLSConfig produces in mtls
// mode. It is a MIRROR, and deliberately not the authority: the production
// assembly is pinned by TestBuildHTTPSTLSConfig in package main (which internal
// packages cannot import). What these tests own is the behavior that shape
// produces on a live wire.
func (h *mtlsHarness) tlsConfig() *tls.Config {
	return &tls.Config{
		MinVersion:       tls.VersionTLS12,
		ClientCAs:        h.ca.Pool(),
		ClientAuth:       tls.RequireAndVerifyClientCert,
		VerifyConnection: servertls.AllowlistConnectionVerifier(h.authorized),
	}
}

// newMTLSHarness starts the listener. handler, when nil, is a 200-with-body
// endpoint; pass one to test streaming.
func newMTLSHarness(t *testing.T, allowed map[string]bool, handler http.Handler) *mtlsHarness {
	t.Helper()
	h := &mtlsHarness{ca: testCA(t), hits: &atomic.Int64{}}
	h.setAuthorized(func(fp string) bool { return allowed[fp] })

	if handler == nil {
		handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, "ok")
		})
	}
	counting := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.hits.Add(1)
		handler.ServeHTTP(w, r)
	})

	api := &Server{
		cfg:            &config.ServerConfig{Auth: &config.AuthConfig{Mode: config.AuthModeMTLS}},
		certAuthorized: h.authorized,
	}

	srv := httptest.NewUnstartedServer(api.authMiddleware(counting))
	srv.TLS = h.tlsConfig()
	srv.StartTLS()
	t.Cleanup(srv.Close)
	h.srv = srv
	return h
}

// client builds an HTTP client that trusts the harness's server certificate and
// presents cert (nil = none). maxVersion pins the client's TLS ceiling; 0 leaves
// the default.
func (h *mtlsHarness) client(cert *tls.Certificate, maxVersion uint16) *http.Client {
	pool := x509.NewCertPool()
	pool.AddCert(h.srv.Certificate())
	tlsCfg := &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12, MaxVersion: maxVersion}
	if cert != nil {
		tlsCfg.Certificates = []tls.Certificate{*cert}
	}
	return &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{TLSClientConfig: tlsCfg}}
}

func (h *mtlsHarness) get(t *testing.T, c *http.Client, path string) (*http.Response, error) {
	t.Helper()
	resp, err := c.Get(h.srv.URL + path)
	if err == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}
	return resp, err
}

// TestMTLSHandshakeRejections: every credential a real client could present
// that is not a live, CA-issued certificate must be refused before the request
// reaches a handler. Handler-invocation count is the assertion that matters —
// a 401 body would mean the request WAS dispatched.
func TestMTLSHandshakeRejections(t *testing.T) {
	fp := testFingerprint("allowed")

	// mustFailAtTransport asserts the strongest available property: the request
	// never became an HTTP exchange at all. A weaker "status != 200" assertion
	// would still pass if the listener were switched to tls.VerifyClientCertIfGiven
	// — the request would be dispatched and collect a middleware 401, which looks
	// like a rejection but is a materially different (and worse) posture.
	mustFailAtTransport := func(t *testing.T, h *mtlsHarness, resp *http.Response, err error, what string) {
		t.Helper()
		if err == nil {
			status := 0
			if resp != nil {
				status = resp.StatusCode
			}
			t.Errorf("%s: request completed at the transport level (HTTP %d) — "+
				"the handshake must fail, not dispatch a request", what, status)
		}
		if got := h.hits.Load(); got != 0 {
			t.Errorf("%s: handler invoked %d times, want 0", what, got)
		}
	}

	t.Run("no client certificate", func(t *testing.T) {
		// TLS 1.2 and 1.3 surface a missing client certificate differently
		// (1.2 fails inside the handshake; 1.3 sends the client's certificate
		// after the server has already finished, so the alert arrives on the
		// first read). Both must fail the client's request outright, and
		// neither may dispatch.
		for _, tc := range []struct {
			name       string
			maxVersion uint16
		}{
			{"TLS 1.2", tls.VersionTLS12},
			{"TLS 1.3", tls.VersionTLS13},
		} {
			t.Run(tc.name, func(t *testing.T) {
				h := newMTLSHarness(t, map[string]bool{fp: true}, nil)
				resp, err := h.get(t, h.client(nil, tc.maxVersion), "/api/sheds")
				mustFailAtTransport(t, h, resp, err, "no client certificate")
			})
		}
	})

	t.Run("certificate from a different CA", func(t *testing.T) {
		for _, tc := range []struct {
			name       string
			maxVersion uint16
		}{
			{"TLS 1.2", tls.VersionTLS12},
			{"TLS 1.3", tls.VersionTLS13},
		} {
			t.Run(tc.name, func(t *testing.T) {
				h := newMTLSHarness(t, map[string]bool{fp: true}, nil)
				// A perfectly well-formed certificate for an ALLOWLISTED identity
				// — it simply chains to a CA this server does not trust.
				foreign := issueClientCert(t, testCA(t), fp, authtoken.ScopeControl, time.Hour)
				resp, err := h.get(t, h.client(&foreign, tc.maxVersion), "/api/sheds")
				mustFailAtTransport(t, h, resp, err, "foreign-CA certificate")
			})
		}
	})

	t.Run("expired certificate", func(t *testing.T) {
		h := newMTLSHarness(t, map[string]bool{fp: true}, nil)
		expired := issueClientCert(t, h.ca, fp, authtoken.ScopeControl, time.Nanosecond)
		if !time.Now().After(expired.Leaf.NotAfter) {
			t.Fatalf("test setup: certificate is not expired (NotAfter %s)", expired.Leaf.NotAfter)
		}
		resp, err := h.get(t, h.client(&expired, 0), "/api/sheds")
		mustFailAtTransport(t, h, resp, err, "expired certificate")
	})

	t.Run("CN not in the allowlist", func(t *testing.T) {
		h := newMTLSHarness(t, map[string]bool{fp: true}, nil)
		// Signed by the right CA, inside its validity window — but the SSH key
		// it was issued against is not allowlisted, so the VerifyConnection
		// allowlist check refuses the handshake.
		stranger := issueClientCert(t, h.ca, testFingerprint("stranger"), authtoken.ScopeControl, time.Hour)
		resp, err := h.get(t, h.client(&stranger, 0), "/api/sheds")
		mustFailAtTransport(t, h, resp, err, "unlisted-CN certificate")
	})

	t.Run("valid certificate is accepted", func(t *testing.T) {
		h := newMTLSHarness(t, map[string]bool{fp: true}, nil)
		good := issueClientCert(t, h.ca, fp, authtoken.ScopeControl, time.Hour)
		resp, err := h.get(t, h.client(&good, 0), "/api/sheds")
		if err != nil {
			t.Fatalf("valid certificate rejected: %v", err)
		}
		if resp.StatusCode != http.StatusOK {
			t.Errorf("got %d, want 200", resp.StatusCode)
		}
		if got := h.hits.Load(); got != 1 {
			t.Errorf("handler invoked %d times, want 1", got)
		}
	})
}

// reusedGet issues a GET and reports whether the connection it used was a
// reused (pooled keep-alive) one. That distinction is the whole point of the
// per-request re-validation tests: a rejection on a FRESH connection proves
// only that the handshake check works.
func reusedGet(t *testing.T, c *http.Client, url string) (status int, reused bool, err error) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return 0, false, err
	}
	var gotReused atomic.Bool
	trace := &httptrace.ClientTrace{
		GotConn: func(info httptrace.GotConnInfo) { gotReused.Store(info.Reused) },
	}
	req = req.WithContext(httptrace.WithClientTrace(req.Context(), trace))
	resp, err := c.Do(req)
	if err != nil {
		return 0, gotReused.Load(), err
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	return resp.StatusCode, gotReused.Load(), nil
}

// TestMTLSRevocationOnPooledConnection is the central correctness test for this
// commit. TLS verifies a peer exactly ONCE per connection, so a client that
// keeps a connection alive would keep its authorization forever if the
// handshake were the only check. Here: one connection, first request succeeds,
// the identity is removed from the allowlist, and the SECOND request — proven
// to be on the same pooled connection — is refused.
func TestMTLSRevocationOnPooledConnection(t *testing.T) {
	fp := testFingerprint("allowed")
	h := newMTLSHarness(t, map[string]bool{fp: true}, nil)
	cert := issueClientCert(t, h.ca, fp, authtoken.ScopeControl, time.Hour)
	c := h.client(&cert, 0)
	url := h.srv.URL + "/api/sheds"

	status, _, err := reusedGet(t, c, url)
	if err != nil || status != http.StatusOK {
		t.Fatalf("first request: status %d, err %v; want 200", status, err)
	}

	// The key leaves the allowlist (removed from github_users, or the user
	// deleted it on GitHub and a refresh landed).
	h.setAuthorized(func(string) bool { return false })

	status, reused, err := reusedGet(t, c, url)
	if err != nil {
		t.Fatalf("second request errored instead of returning 401: %v", err)
	}
	if !reused {
		t.Fatal("second request opened a NEW connection — the test cannot prove per-request re-validation")
	}
	if status != http.StatusUnauthorized {
		t.Errorf("second request on the same connection: got %d, want 401", status)
	}
	if got := h.hits.Load(); got != 1 {
		t.Errorf("handler invoked %d times, want 1 (only the first request)", got)
	}
}

// TestMTLSExpiryOnPooledConnection is the same argument for expiry: a
// short-lived certificate that was valid at handshake time must stop working on
// the very connection it established once it expires.
func TestMTLSExpiryOnPooledConnection(t *testing.T) {
	fp := testFingerprint("allowed")
	// x509 encodes validity at SECOND granularity, so a sub-second TTL truncates
	// down and can be born already-expired. 1.5s leaves between 0.5s and 1.5s of
	// real validity — comfortably enough for one request, short enough to wait out.
	const ttl = 1500 * time.Millisecond
	h := newMTLSHarness(t, map[string]bool{fp: true}, nil)
	cert := issueClientCert(t, h.ca, fp, authtoken.ScopeControl, ttl)
	c := h.client(&cert, 0)
	url := h.srv.URL + "/api/sheds"

	status, _, err := reusedGet(t, c, url)
	if err != nil || status != http.StatusOK {
		t.Fatalf("first request: status %d, err %v; want 200", status, err)
	}

	time.Sleep(time.Until(cert.Leaf.NotAfter) + 100*time.Millisecond)

	status, reused, err := reusedGet(t, c, url)
	if err != nil {
		t.Fatalf("second request errored instead of returning 401: %v", err)
	}
	if !reused {
		t.Fatal("second request opened a NEW connection — the test cannot prove per-request expiry checking")
	}
	if status != http.StatusUnauthorized {
		t.Errorf("expired cert on the same connection: got %d, want 401", status)
	}
	if got := h.hits.Load(); got != 1 {
		t.Errorf("handler invoked %d times, want 1 (only the first request)", got)
	}
}

// TestMTLSEstablishedStreamSurvivesExpiry pins the documented parity limitation:
// authorization is checked at request dispatch, so an SSE stream that was
// authorized when it opened keeps streaming after its certificate expires. It
// is exactly token mode's behavior (an expired token does not tear down a live
// stream either), and it is asserted rather than left implicit so a future
// change to it is a deliberate, visible one.
func TestMTLSEstablishedStreamSurvivesExpiry(t *testing.T) {
	fp := testFingerprint("allowed")
	// x509 encodes validity at SECOND granularity, so a sub-second TTL truncates
	// down and can be born already-expired. 1.5s leaves between 0.5s and 1.5s of
	// real validity — comfortably enough for one request, short enough to wait out.
	const ttl = 1500 * time.Millisecond

	tick := make(chan struct{})
	done := make(chan struct{})
	stream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Error("ResponseWriter is not a Flusher")
			return
		}
		flusher.Flush()
		for {
			select {
			case <-done:
				return
			case <-tick:
				_, _ = io.WriteString(w, "data: still-here\n\n")
				flusher.Flush()
			case <-r.Context().Done():
				return
			}
		}
	})

	h := newMTLSHarness(t, map[string]bool{fp: true}, stream)
	cert := issueClientCert(t, h.ca, fp, authtoken.ScopeControl, ttl)
	c := h.client(&cert, 0)

	resp, err := c.Get(h.srv.URL + "/api/egress/stream")
	if err != nil {
		t.Fatalf("open stream: %v", err)
	}
	defer func() { close(done); _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("open stream: got %d, want 200", resp.StatusCode)
	}

	// Let the certificate expire while the stream is open.
	time.Sleep(time.Until(cert.Leaf.NotAfter) + 100*time.Millisecond)

	// The stream is still live: a post-expiry event reaches the client.
	tick <- struct{}{}
	buf := make([]byte, 64)
	n, err := resp.Body.Read(buf)
	if err != nil {
		t.Fatalf("established stream died after cert expiry (documented behavior is that it survives): %v", err)
	}
	if !strings.Contains(string(buf[:n]), "still-here") {
		t.Errorf("unexpected stream payload %q", string(buf[:n]))
	}

	// ...but a NEW request with the same expired certificate is refused, so the
	// limitation is scoped to the already-established stream and nothing else.
	// The stream is still holding its connection, so this one opens a fresh
	// connection and dies at the handshake rather than reaching the middleware
	// for a 401 — either rejection is correct; what matters is that it is
	// rejected and never dispatched.
	before := h.hits.Load()
	status, _, err := reusedGet(t, c, h.srv.URL+"/api/sheds")
	if err == nil && status == http.StatusOK {
		t.Error("new request with an expired certificate succeeded")
	}
	if got := h.hits.Load(); got != before {
		t.Errorf("handler invoked for the post-expiry request (hits %d -> %d)", before, got)
	}
}
