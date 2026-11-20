package api

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/charliek/shed/internal/authtoken"
	"github.com/charliek/shed/internal/config"
	"github.com/charliek/shed/internal/servertls"
	"github.com/charliek/shed/internal/users"
	"github.com/charliek/shed/internal/users/userstest"
)

// These tests pin that a user's client certificate authenticates as the user
// it was issued to, and never as another user its key comes to belong to.

// bindingLookup is a lookup with users alice (an admin) and bob, and the key
// k as alice's.
func bindingLookup(k string) *userstest.Fake {
	f := userstest.New()
	f.AddUser(users.User{Name: "alice", Admin: true})
	f.AddUser(users.User{Name: "bob"})
	f.AddKey("alice", users.Key{Fingerprint: k, Source: users.SourceAdHoc})
	return f
}

// callerOf runs one control request with leaf through s's mtls middleware and
// returns the status and the caller the handler saw.
func callerOf(s *Server, leaf *x509.Certificate) (int, Caller) {
	var got Caller
	h := s.authMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = callerFrom(r.Context())
		w.WriteHeader(http.StatusOK)
	}))
	req := withClientCert(httptest.NewRequest(http.MethodGet, "/api/sheds", nil), leaf)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Code, got
}

// handshake runs the HTTPS listener's handshake check, as shed-server wires
// it (servertls.ClientConnectionVerifier over CertificateUser), on leaf.
func handshake(l users.Lookup, leaf *x509.Certificate) error {
	return servertls.ClientConnectionVerifier(func(leaf *x509.Certificate) bool {
		_, _, ok := CertificateUser(l, leaf, time.Now())
		return ok
	}, nil)(tls.ConnectionState{PeerCertificates: []*x509.Certificate{leaf}})
}

func bindingServer(l users.Lookup) *Server {
	return &Server{cfg: &config.ServerConfig{Auth: &config.AuthConfig{Mode: config.AuthModeMTLS}}, users: l}
}

// TestCertificateFollowsNoMovedKey: alice enrols with key K; K then moves to
// bob without a restart (as a GitHub refresh that finds K under bob's login
// does: the live lookup changes under a running server). Alice's certificate,
// still valid, is refused on the API (401) and at the handshake; a new
// enrolment with K yields bob's certificate, which is bob.
func TestCertificateFollowsNoMovedKey(t *testing.T) {
	k := testFingerprint("K")
	l := bindingLookup(k)
	s := bindingServer(l)
	ca := testCA(t)
	alices := issueClientCertAs(t, ca, k, "alice", authtoken.ScopeControl, time.Hour).Leaf

	if code, c := callerOf(s, alices); code != http.StatusOK || c.User != "alice" {
		t.Fatalf("alice before the move: %d %+v", code, c)
	}
	if err := handshake(l, alices); err != nil {
		t.Fatalf("alice's handshake before the move: %v", err)
	}

	l.AddKey("bob", users.Key{Fingerprint: k, Source: users.SourceGitHub, Origin: "bob-gh"})

	if code, c := callerOf(s, alices); code != http.StatusUnauthorized {
		t.Errorf("alice's certificate after K moved to bob: %d as %+v, want 401", code, c)
	}
	if err := handshake(l, alices); err == nil {
		t.Error("alice's certificate passed the handshake after K moved to bob")
	}
	bobs := issueClientCertAs(t, ca, k, "bob", authtoken.ScopeControl, time.Hour).Leaf
	if code, c := callerOf(s, bobs); code != http.StatusOK || c.User != "bob" || c.Admin {
		t.Errorf("the certificate enrolled with K after the move: %d %+v, want bob", code, c)
	}
	if err := handshake(l, bobs); err != nil {
		t.Errorf("bob's handshake: %v", err)
	}
}

// TestCertificateRefusedAfterRestartWithKeyMoved: a config change gives K to
// bob and the server restarts with the same CA. Alice's certificate is refused
// by the new server on the API and at the handshake, over real TLS; bob's,
// enrolled after, is served.
func TestCertificateRefusedAfterRestartWithKeyMoved(t *testing.T) {
	k, other := newKeyPair(t), newKeyPair(t)
	ca := testCA(t)
	before, _, _ := configServer(t, newUserFakeBackend(), "auth:\n  mode: mtls\n  users:\n"+
		"    alice:\n      admin: true\n      keys:\n        - {key: \""+k.line+"\", label: laptop}\n"+
		"    bob:\n      keys:\n        - {key: \""+other.line+"\", label: desk}\n")
	after, _, _ := configServer(t, newUserFakeBackend(), "auth:\n  mode: mtls\n  users:\n"+
		"    alice:\n      admin: true\n      keys:\n        - {key: \""+other.line+"\", label: desk}\n"+
		"    bob:\n      keys:\n        - {key: \""+k.line+"\", label: laptop}\n")

	alices := issueClientCertAs(t, ca, k.fp, "alice", authtoken.ScopeControl, time.Hour)
	if code, c := callerOf(before, alices.Leaf); code != http.StatusOK || c.User != "alice" {
		t.Fatalf("alice before the restart: %d %+v", code, c)
	}

	srv := realTLSServer(t, after, ca)
	if code, err := getWith(t, srv, alices); err == nil {
		t.Errorf("alice's certificate after the restart: handshake passed, status %d", code)
	}
	if code, c := callerOf(after, alices.Leaf); code != http.StatusUnauthorized {
		t.Errorf("alice's certificate after the restart: %d as %+v, want 401", code, c)
	}
	bobs := issueClientCertAs(t, ca, k.fp, "bob", authtoken.ScopeControl, time.Hour)
	if code, err := getWith(t, srv, bobs); err != nil || code != http.StatusOK {
		t.Errorf("bob's certificate after the restart: %d %v, want 200", code, err)
	}
}

// realTLSServer serves s's middleware over TLS, with the listener's handshake
// check wired as shed-server wires it.
func realTLSServer(t *testing.T, s *Server, ca servertls.CA) *httptest.Server {
	t.Helper()
	srv := httptest.NewUnstartedServer(s.authMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})))
	srv.TLS = &tls.Config{
		MinVersion: tls.VersionTLS12,
		ClientCAs:  ca.Pool(),
		ClientAuth: tls.RequireAndVerifyClientCert,
		VerifyConnection: servertls.ClientConnectionVerifier(func(leaf *x509.Certificate) bool {
			_, _, ok := CertificateUser(s.users, leaf, time.Now())
			return ok
		}, nil),
	}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return srv
}

// getWith GETs /api/sheds from srv on a new connection presenting cert.
func getWith(t *testing.T, srv *httptest.Server, cert tls.Certificate) (int, error) {
	t.Helper()
	pool := x509.NewCertPool()
	pool.AddCert(srv.Certificate())
	c := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{
		RootCAs: pool, MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{cert},
	}}}
	resp, err := c.Get(srv.URL + "/api/sheds")
	if err != nil {
		return 0, err
	}
	_ = resp.Body.Close()
	return resp.StatusCode, nil
}

// TestCertificateImplicitAndNamedNeverMix: a certificate issued to the
// implicit user is refused once its key is a named user's (a server that
// gained a user list), and one issued to a named user is refused once its key
// is the implicit user's (the user list removed), whatever the names.
func TestCertificateImplicitAndNamedNeverMix(t *testing.T) {
	k := testFingerprint("K")
	ca := testCA(t)
	implicit := issueClientCertAs(t, ca, k, users.ImplicitName, authtoken.ScopeControl, time.Hour).Leaf
	alices := issueClientCertAs(t, ca, k, "alice", authtoken.ScopeControl, time.Hour).Leaf

	named := bindingLookup(k)
	if code, _ := callerOf(bindingServer(named), implicit); code != http.StatusUnauthorized {
		t.Errorf("an implicit user's certificate on a server where K is alice's: %d, want 401", code)
	}
	if err := handshake(named, implicit); err == nil {
		t.Error("an implicit user's certificate passed the handshake where K is alice's")
	}
	none := userstest.Implicit(k)
	if code, _ := callerOf(bindingServer(none), alices); code != http.StatusUnauthorized {
		t.Errorf("alice's certificate on a server with no user list: %d, want 401", code)
	}
	if err := handshake(none, alices); err == nil {
		t.Error("alice's certificate passed the handshake on a server with no user list")
	}
	// Controls: each on its own kind of server.
	if code, c := callerOf(bindingServer(none), implicit); code != http.StatusOK || !c.Implicit {
		t.Errorf("an implicit user's certificate with no user list: %d %+v", code, c)
	}
	if code, c := callerOf(bindingServer(named), alices); code != http.StatusOK || c.User != "alice" {
		t.Errorf("alice's certificate where K is alice's: %d %+v", code, c)
	}
}

// TestCertificateAdminAndScriptStayLive: what is bound is the user; the
// user's admin flag and the key's script mark are read from the lookup on
// every request.
func TestCertificateAdminAndScriptStayLive(t *testing.T) {
	k := testFingerprint("K")
	l := bindingLookup(k)
	s := bindingServer(l)
	alices := issueClientCertAs(t, testCA(t), k, "alice", authtoken.ScopeControl, time.Hour).Leaf

	if _, c := callerOf(s, alices); !c.Admin || c.Script {
		t.Fatalf("alice at first: %+v, want an admin, not a script", c)
	}
	l.AddUser(users.User{Name: "alice"}) // demoted
	if code, c := callerOf(s, alices); code != http.StatusOK || c.Admin {
		t.Errorf("alice after her demotion: %d %+v, want served and not an admin", code, c)
	}
	l.AddKey("alice", users.Key{Fingerprint: k, Source: users.SourceAdHoc, Script: true})
	if code, c := callerOf(s, alices); code != http.StatusOK || !c.Script || c.User != "alice" {
		t.Errorf("alice after K became a script key: %d %+v, want alice with the script mark", code, c)
	}
}

// TestUnboundCertificateRefusedOnEveryServer: a user's certificate that
// names no user (one a server older than 0.10.0 issued, before certificates
// named their user) is refused on every server, at the handshake and on the
// API (401): on a server with no user list, where its key is the implicit
// user's, and on one with auth.users, where its key is alice's, as on a
// directory server (directory_bus_test.go). Controls: on each server a
// certificate naming the user the key belongs to passes both, and a service
// client's certificate (auth.clients), which names no user and is told apart
// by its shape before CertificateUser, is still admitted at the handshake and
// served by the middleware.
func TestUnboundCertificateRefusedOnEveryServer(t *testing.T) {
	k := testFingerprint("K")
	ca := testCA(t)
	caCert, caKey := testCAMaterial(t)
	legacy := legacyClientCert(t, caCert, caKey, k, time.Now().Add(time.Hour))
	if _, bound, err := servertls.ClientCertUser(legacy); bound || err != nil {
		t.Fatalf("the legacy certificate names a user: bound %v err %v", bound, err)
	}
	service := serviceLeaf(testClientName)
	if _, bound, err := servertls.ClientCertUser(service); bound || err != nil {
		t.Fatalf("the service certificate names a user: bound %v err %v", bound, err)
	}
	clientKnown := func(name string) bool { return name == testClientName }

	for _, tc := range []struct {
		name   string
		lookup users.Lookup
		own    *x509.Certificate // a certificate naming the user K belongs to
	}{
		{"no user list", userstest.Implicit(k), issueClientCertAs(t, ca, k, users.ImplicitName, authtoken.ScopeControl, time.Hour).Leaf},
		{"auth.users", bindingLookup(k), issueClientCertAs(t, ca, k, "alice", authtoken.ScopeControl, time.Hour).Leaf},
	} {
		s := bindingServer(tc.lookup)
		if code, c := callerOf(s, legacy); code != http.StatusUnauthorized {
			t.Errorf("%s: a certificate that names no user: %d %+v, want 401", tc.name, code, c)
		}
		if err := handshake(tc.lookup, legacy); err == nil {
			t.Errorf("%s: a certificate that names no user passed the handshake", tc.name)
		}
		if u, _, ok := CertificateUser(tc.lookup, legacy, time.Now()); ok {
			t.Errorf("%s: CertificateUser accepted a certificate that names no user as %+v", tc.name, u)
		}
		// Control: the key's own user's certificate.
		if code, _ := callerOf(s, tc.own); code != http.StatusOK {
			t.Errorf("%s: a certificate naming K's user: %d, want 200", tc.name, code)
		}
		if err := handshake(tc.lookup, tc.own); err != nil {
			t.Errorf("%s: a certificate naming K's user failed the handshake: %v", tc.name, err)
		}
		// Control: a service client's certificate.
		verify := servertls.ClientConnectionVerifier(func(leaf *x509.Certificate) bool {
			_, _, ok := CertificateUser(tc.lookup, leaf, time.Now())
			return ok
		}, clientKnown)
		if err := verify(tls.ConnectionState{PeerCertificates: []*x509.Certificate{service}}); err != nil {
			t.Errorf("%s: a service client's certificate failed the handshake: %v", tc.name, err)
		}
		cs, router := clientTestServer(t, newClientFakeBackend(), defaultTestPolicy())
		cs.SetUserLookup(tc.lookup)
		if r := do(router, service, "GET", "/api/sheds", ""); r.code != http.StatusOK {
			t.Errorf("%s: a service client's GET /api/sheds = %d %s, want 200", tc.name, r.code, r.body)
		}
	}
}

func poolOf(c *x509.Certificate) *x509.CertPool {
	p := x509.NewCertPool()
	p.AddCert(c)
	return p
}

// testCAMaterial generates a CA as shed-server does and reads its
// certificate and key back from disk, to sign a certificate of the shape an
// earlier release issued.
func testCAMaterial(t *testing.T) (*x509.Certificate, *ecdsa.PrivateKey) {
	t.Helper()
	dir := t.TempDir()
	certPath, keyPath := filepath.Join(dir, "ca.pem"), filepath.Join(dir, "ca.key")
	if _, err := servertls.LoadOrGenerateCA(certPath, keyPath); err != nil {
		t.Fatal(err)
	}
	certPEM, err := os.ReadFile(certPath)
	if err != nil {
		t.Fatal(err)
	}
	keyPEM, err := os.ReadFile(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	cb, _ := pem.Decode(certPEM)
	kb, _ := pem.Decode(keyPEM)
	if cb == nil || kb == nil {
		t.Fatal("CA files are not PEM")
	}
	cert, err := x509.ParseCertificate(cb.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	key, err := x509.ParseECPrivateKey(kb.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	return cert, key
}

// legacyClientCert is a client certificate of the shape issued before
// certificates named their user: CN the key fingerprint, OU the scope, O the
// client kind, nothing else. It expires at notAfter.
func legacyClientCert(t *testing.T, ca *x509.Certificate, caKey *ecdsa.PrivateKey, fingerprint string, notAfter time.Time) *x509.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject: pkix.Name{
			CommonName:         fingerprint,
			OrganizationalUnit: []string{authtoken.ScopeControl},
			Organization:       []string{authtoken.ClientCLI},
		},
		NotBefore:             notAfter.Add(-2 * time.Hour),
		NotAfter:              notAfter,
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca, &key.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return leaf
}
