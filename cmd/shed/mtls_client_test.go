package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/charliek/shed/internal/clienttoken"
	"github.com/charliek/shed/internal/config"
	"github.com/charliek/shed/internal/servertls"
	"github.com/charliek/shed/internal/tunnels"
	"github.com/charliek/shed/sdk"
)

// ---------------------------------------------------------------------------
// An in-process mtls shed-server.
//
// The listener is wired the way cmd/shed-server wires the real one — the
// internal CA as the ONLY ClientCAs, RequireAndVerifyClientCert, and the
// allowlist connection verifier — so these tests exercise the CLI's transport
// against a genuine handshake rather than a mock. What is asserted here is the
// CLIENT's behavior; internal/api owns the server's.
// ---------------------------------------------------------------------------

// sshFingerprint renders label in the canonical OpenSSH SHA-256 form the CA
// requires as an issued certificate's Subject CN.
func sshFingerprint(label string) string {
	sum := sha256.Sum256([]byte(label))
	return "SHA256:" + base64.RawStdEncoding.EncodeToString(sum[:])
}

type mtlsServer struct {
	srv *httptest.Server
	ca  servertls.CA
	pin string
	// requests counts handler invocations, and records what credential each
	// arrived with, so a test can prove the CLI authenticated by certificate and
	// sent no bearer token.
	requests   atomic.Int64
	lastAuth   atomic.Value // string: the Authorization header seen
	lastCN     atomic.Value // string: the peer certificate's Subject CN
	lastSerial atomic.Value // string: the peer certificate's serial, lower-case hex
	// revoked holds serials the handler refuses with a 401 while still COMPLETING
	// the handshake for them — the shape of the real server's per-request
	// re-validation (internal/api re-checks the peer certificate on every
	// request, because crypto/tls verifies it only once per connection).
	revoked atomic.Value // map[string]bool
}

// revoke makes the server answer 401 for these serials from now on, without
// touching the TLS layer. See TestReauthDropsPooledConnection.
func (m *mtlsServer) revoke(serials ...string) {
	deny := make(map[string]bool, len(serials))
	for _, s := range serials {
		deny[s] = true
	}
	m.revoked.Store(deny)
}

// certTTL values used across these tests. The distinction matters: the
// proactive refresh window is 2h, so a certificate with a shorter remaining
// life re-mints at construction and a test that wants to isolate the REACTIVE
// path must use the long one.
const (
	farFromExpiry = 24 * time.Hour
	// alreadyExpired is expressed as a positive nanosecond TTL because the CA
	// refuses a non-positive one (servertls.ErrCAInvalidTTL). NotBefore is
	// backdated by the CA's clock-skew allowance, so the result is a
	// structurally valid certificate whose validity window has already closed by
	// the time any handshake reaches it — an expired credential with no sleep.
	alreadyExpired = time.Nanosecond
)

// newMTLSServer starts the listener. Every request answers with a minimal
// /api/info body so the CLI's GetInfo path can drive it.
func newMTLSServer(t *testing.T) *mtlsServer {
	t.Helper()
	dir := t.TempDir()
	ca, err := servertls.LoadOrGenerateCA(filepath.Join(dir, "ca.pem"), filepath.Join(dir, "ca.key"))
	if err != nil {
		t.Fatalf("generate CA: %v", err)
	}
	m := &mtlsServer{ca: ca}
	m.lastAuth.Store("")
	m.lastCN.Store("")
	m.lastSerial.Store("")
	m.revoke()

	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		m.requests.Add(1)
		m.lastAuth.Store(r.Header.Get("Authorization"))
		serial := ""
		if r.TLS != nil && len(r.TLS.PeerCertificates) > 0 {
			m.lastCN.Store(r.TLS.PeerCertificates[0].Subject.CommonName)
			serial = r.TLS.PeerCertificates[0].SerialNumber.Text(16)
		}
		m.lastSerial.Store(serial)
		if m.revoked.Load().(map[string]bool)[serial] {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"name":"srv","http_port":0,"ssh_port":2222,"auth_mode":"mtls"}`)
	}))
	srv.TLS = &tls.Config{
		MinVersion: tls.VersionTLS12,
		ClientCAs:  ca.Pool(),
		ClientAuth: tls.RequireAndVerifyClientCert,
	}
	// A refused handshake is the point of several of these tests, not a surprise
	// worth pages of output.
	srv.Config.ErrorLog = log.New(io.Discard, "", 0)
	srv.StartTLS()
	t.Cleanup(srv.Close)

	m.srv = srv
	m.pin = servertls.Fingerprint(srv.Certificate().Raw)
	return m
}

// newMTLSCredentialIssuer provides the real CA/CSR issuance half of
// newMTLSServer without opening a listener. Persistence-only tests do not need
// an HTTP exchange, and keeping them listener-free makes their failure signal
// independent of the host's networking policy.
func newMTLSCredentialIssuer(t *testing.T) *mtlsServer {
	t.Helper()
	dir := t.TempDir()
	ca, err := servertls.LoadOrGenerateCA(filepath.Join(dir, "ca.pem"), filepath.Join(dir, "ca.key"))
	if err != nil {
		t.Fatalf("generate CA: %v", err)
	}
	return &mtlsServer{
		ca:  ca,
		pin: servertls.Fingerprint([]byte("test-server-certificate")),
	}
}

// issue runs the REAL enrollment path — generate a P-256 key, build a CSR, have
// the CA sign it — and returns what a bootstrap would hand back: the leaf PEM
// and the matching key PEM. Using SignClientCSR rather than a hand-rolled leaf
// keeps these tests honest about the subject and extension shape the server
// actually issues.
func (m *mtlsServer) issue(t *testing.T, label string, ttl time.Duration) (certPEM, keyPEM []byte, serial string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	csrDER, err := x509.CreateCertificateRequest(rand.Reader,
		&x509.CertificateRequest{SignatureAlgorithm: x509.ECDSAWithSHA256}, key)
	if err != nil {
		t.Fatal(err)
	}
	certDER, err := m.ca.SignClientCSR(csrDER, sshFingerprint(label), "control", "cli", ttl)
	if err != nil {
		t.Fatalf("sign client CSR: %v", err)
	}
	leaf, err := x509.ParseCertificate(certDER)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER}),
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}),
		leaf.SerialNumber.Text(16)
}

// credential packages an issued pair as the sdk.Credential a bootstrap returns.
func (m *mtlsServer) credential(t *testing.T, label string, ttl time.Duration) sdk.Credential {
	t.Helper()
	certPEM, keyPEM, serial := m.issue(t, label, ttl)
	return sdk.Credential{
		Bundle: sdk.Bundle{
			AuthMode:           sdk.AuthModeMTLS,
			HTTPSPort:          443,
			TLSCertFingerprint: m.pin,
			ClientCert:         string(certPEM),
			Scope:              "control",
			CertSerial:         serial,
			ExpiresAt:          expiryFor(ttl),
		},
		KeyPEM: keyPEM,
	}
}

// expiryFor is the bundle expiry a server would report for a given issuance
// TTL. alreadyExpired is a nanosecond on the wire but must be RECORDED as a
// past instant, since that is what a server issuing a genuinely expired
// certificate would report — and what the proactive window reads.
func expiryFor(ttl time.Duration) time.Time {
	if ttl == alreadyExpired {
		return time.Now().Add(-time.Hour)
	}
	return time.Now().Add(ttl)
}

// testClientConfig installs an isolated clientConfig + HOME (so the creds store
// writes into a temp dir) and returns the config path.
func testClientConfig(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)

	origCfg, origBF := clientConfig, bootstrapCredentialFn
	t.Cleanup(func() { clientConfig, bootstrapCredentialFn = origCfg, origBF })

	cfgPath := filepath.Join(home, ".shed", "config.yaml")
	cfg, err := config.LoadClientConfigFromPath(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	clientConfig = cfg
	return cfgPath
}

// putServerEntry installs an entry in BOTH the in-memory config and the file on
// disk, which is the state every real command starts from.
//
// It matters because the credential persist re-verifies its target under the
// config lock, against a FRESH read of config.yaml: an entry that exists only
// in this process's memory looks exactly like one another process just removed,
// and the persist correctly refuses to resurrect it.
func putServerEntry(t *testing.T, name string, entry config.ServerEntry) {
	t.Helper()
	if err := clientConfig.Update(func(c *config.ClientConfig) error {
		c.Servers[name] = entry
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// stubBootstrap installs a bootstrap that returns mint()'s credential and
// counts the calls. testClientConfig restores the original on cleanup, so every
// test that stubs the bootstrap must call that first.
//
// The counter is the assertion these tests care about most: "exactly one silent
// re-enrollment" is the behavior, and a second mint would mean an SSH exchange
// per request.
func stubBootstrap(mint func() sdk.Credential) *atomic.Int32 {
	var calls atomic.Int32
	bootstrapCredentialFn = func(string, int, string, string) (sdk.Credential, error) {
		calls.Add(1)
		return mint(), nil
	}
	return &calls
}

// mtlsEntry writes an issued credential to the creds store and returns the
// server entry pointing at it — i.e. the state `shed server add` would leave
// behind for an mtls server.
func mtlsEntry(t *testing.T, m *mtlsServer, name string, cred sdk.Credential) config.ServerEntry {
	t.Helper()
	certPath, keyPath, err := config.WriteClientCredentials(name,
		[]byte(cred.Bundle.ClientCert), cred.KeyPEM)
	if err != nil {
		t.Fatal(err)
	}
	return config.ServerEntry{
		Host:                "127.0.0.1",
		SSHPort:             2222,
		APIURL:              m.srv.URL,
		TLSCertFingerprint:  m.pin,
		AuthMode:            config.AuthModeMTLS,
		ClientCertFile:      certPath,
		ClientKeyFile:       keyPath,
		ClientCertExpiresAt: cred.Bundle.ExpiresAt,
	}
}

// TestMTLSClientAuthenticatesEndToEnd: an entry holding a valid client
// certificate reaches a real mtls listener, over the CLI's own transport, with
// no bearer token anywhere.
func TestMTLSClientAuthenticatesEndToEnd(t *testing.T) {
	testClientConfig(t)
	m := newMTLSServer(t)
	cred := m.credential(t, "cli-key", farFromExpiry)
	entry := mtlsEntry(t, m, "srv", cred)
	putServerEntry(t, "srv", entry)

	mints := stubBootstrap(func() sdk.Credential { return m.credential(t, "cli-key", farFromExpiry) })

	e := clientConfig.Servers["srv"]
	c := NewAPIClientFromEntry(&e, DefaultTimeout)
	if _, err := c.GetInfo(); err != nil {
		t.Fatalf("GetInfo over mtls: %v", err)
	}

	if got := m.requests.Load(); got != 1 {
		t.Errorf("server saw %d requests, want 1 (no retry was needed)", got)
	}
	if got := m.lastAuth.Load().(string); got != "" {
		t.Errorf("the client sent Authorization %q in mtls mode; the credential belongs in the handshake", got)
	}
	if got := m.lastCN.Load().(string); got != sshFingerprint("cli-key") {
		t.Errorf("peer CN = %q, want the enrolled SSH key fingerprint", got)
	}
	if got := mints.Load(); got != 0 {
		t.Errorf("mints = %d, want 0 (the stored certificate was valid and far from expiry)", got)
	}
}

// TestExpiredClientCertTriggersExactlyOneSilentRemint is the headline behavior:
// an expired certificate must recover on its own, once, without the user
// seeing anything.
//
// Both halves are covered because they fire through different mechanisms and
// both are load-bearing.
func TestExpiredClientCertTriggersExactlyOneSilentRemint(t *testing.T) {
	// The reactive half. The entry CLAIMS the certificate is good for another
	// day (a skewed clock, or a certificate revoked early), so nothing proactive
	// fires — the server's TLS rejection is the only signal, and the request
	// must still succeed.
	t.Run("reactive: the server refuses the handshake", func(t *testing.T) {
		testClientConfig(t)
		m := newMTLSServer(t)

		expired := m.credential(t, "cli-key", alreadyExpired)
		entry := mtlsEntry(t, m, "srv", expired)
		entry.ClientCertExpiresAt = time.Now().Add(24 * time.Hour) // a lie: nothing proactive fires
		putServerEntry(t, "srv", entry)

		mints := stubBootstrap(func() sdk.Credential { return m.credential(t, "cli-key", farFromExpiry) })

		e := clientConfig.Servers["srv"]
		c := NewAPIClientFromEntry(&e, DefaultTimeout)
		if got := mints.Load(); got != 0 {
			t.Fatalf("mints = %d before the first request, want 0 (the entry claimed the cert was fresh)", got)
		}

		if _, err := c.GetInfo(); err != nil {
			t.Fatalf("GetInfo should have recovered from the expired certificate: %v", err)
		}
		if got := mints.Load(); got != 1 {
			t.Errorf("mints = %d, want EXACTLY 1 (one silent re-enrollment)", got)
		}
		if got := m.requests.Load(); got != 1 {
			t.Errorf("handler ran %d times, want 1 — the first attempt must never have reached it", got)
		}
	})

	// The proactive half. The entry's recorded expiry is in the past, so the
	// refresh window fires at construction and the request is never made with a
	// dead certificate at all.
	t.Run("proactive: the recorded expiry has passed", func(t *testing.T) {
		testClientConfig(t)
		m := newMTLSServer(t)

		expired := m.credential(t, "cli-key", alreadyExpired)
		putServerEntry(t, "srv", mtlsEntry(t, m, "srv", expired))

		mints := stubBootstrap(func() sdk.Credential { return m.credential(t, "cli-key", farFromExpiry) })

		e := clientConfig.Servers["srv"]
		c := NewAPIClientFromEntry(&e, DefaultTimeout)
		if got := mints.Load(); got != 1 {
			t.Fatalf("mints = %d at construction, want 1 (proactive window)", got)
		}
		if _, err := c.GetInfo(); err != nil {
			t.Fatalf("GetInfo: %v", err)
		}
		if got := mints.Load(); got != 1 {
			t.Errorf("mints = %d, want 1 (the proactive mint was enough)", got)
		}
	})

	// A missing credential file is the same recoverable situation: enroll,
	// don't fail. This is the case the classifier deliberately does NOT rely on
	// a TLS alert for (see authAlertDescriptions).
	t.Run("missing credential files enroll rather than fail", func(t *testing.T) {
		testClientConfig(t)
		m := newMTLSServer(t)

		cred := m.credential(t, "cli-key", farFromExpiry)
		entry := mtlsEntry(t, m, "srv", cred)
		if err := os.Remove(entry.ClientCertFile); err != nil {
			t.Fatal(err)
		}
		putServerEntry(t, "srv", entry)

		mints := stubBootstrap(func() sdk.Credential { return m.credential(t, "cli-key", farFromExpiry) })

		e := clientConfig.Servers["srv"]
		c := NewAPIClientFromEntry(&e, DefaultTimeout)
		if _, err := c.GetInfo(); err != nil {
			t.Fatalf("a missing certificate must re-enroll, not fail: %v", err)
		}
		if got := mints.Load(); got != 1 {
			t.Errorf("mints = %d, want 1", got)
		}
	})
}

// TestTransportSurvivesRotation is the structural claim the adaptive transport
// makes: a re-mint changes what is PRESENTED without changing the transport.
//
// Asserting pointer identity is the point. A design that rebuilt the transport
// on rotation would pass a "does it still work" test while silently discarding
// the connection pool — and, worse, would need every http.Client the APIClient
// hands out (SSE, long-timeout create, prune) to be rebuilt in lockstep.
func TestTransportSurvivesRotation(t *testing.T) {
	testClientConfig(t)
	m := newMTLSServer(t)

	first := m.credential(t, "cli-key", farFromExpiry)
	putServerEntry(t, "srv", mtlsEntry(t, m, "srv", first))

	second := m.credential(t, "cli-key", farFromExpiry)
	if second.Bundle.CertSerial == first.Bundle.CertSerial || second.Bundle.CertSerial == "" {
		t.Fatal("test setup: the two credentials must be distinguishable by serial")
	}
	stubBootstrap(func() sdk.Credential { return second })

	e := clientConfig.Servers["srv"]
	c := NewAPIClientFromEntry(&e, DefaultTimeout)

	transportBefore := c.transport
	httpClientBefore := c.httpClient
	if _, err := c.GetInfo(); err != nil {
		t.Fatalf("GetInfo: %v", err)
	}
	if got := m.lastSerial.Load().(string); got != first.Bundle.CertSerial {
		t.Fatalf("first request presented serial %s, want the stored %s", got, first.Bundle.CertSerial)
	}

	// Force a rotation the way an auth failure would.
	_, gen := c.tokens.Current()
	if _, err := c.tokens.Refresh(gen); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	c.closeIdleConnections()

	if c.transport != transportBefore {
		t.Error("the transport was rebuilt across a rotation; it must be built once and reused")
	}
	if c.httpClient != httpClientBefore {
		t.Error("the http.Client was rebuilt across a rotation")
	}

	// The claim that matters: the SAME transport now presents the NEW
	// certificate on the wire. Asserted by the serial the SERVER observed, not
	// by anything the client believes about itself.
	if _, err := c.GetInfo(); err != nil {
		t.Fatalf("GetInfo after rotation: %v", err)
	}
	if got := m.lastSerial.Load().(string); got != second.Bundle.CertSerial {
		t.Errorf("after rotation the server saw serial %s, want the re-minted %s — "+
			"the unchanged transport did not pick up the new certificate", got, second.Bundle.CertSerial)
	}
	if got := m.lastAuth.Load().(string); got != "" {
		t.Errorf("Authorization header sent after rotation: %q", got)
	}
}

// TestModeFlipTokenToMTLS: an operator switches a server from auth.mode token
// to mtls. The client's stored entry still says token, so it presents no
// certificate, the server refuses the handshake, and the reactive path enrolls
// — rewriting the entry to mtls and writing the credential files. No `shed
// server add`, no user action.
func TestModeFlipTokenToMTLS(t *testing.T) {
	cfgPath := testClientConfig(t)
	m := newMTLSServer(t)

	putServerEntry(t, "srv", config.ServerEntry{
		Host: "127.0.0.1", SSHPort: 2222,
		APIURL:                m.srv.URL,
		TLSCertFingerprint:    m.pin,
		AuthMode:              config.AuthModeToken,
		ControlToken:          "shed_ctl_stale",
		ControlTokenExpiresAt: time.Now().Add(24 * time.Hour), // far from expiry: only the reactive path can fire
	})

	issued := m.credential(t, "cli-key", farFromExpiry)
	mints := stubBootstrap(func() sdk.Credential { return issued })

	e := clientConfig.Servers["srv"]
	c := NewAPIClientFromEntry(&e, DefaultTimeout)
	if _, err := c.GetInfo(); err != nil {
		t.Fatalf("the client should have enrolled and recovered: %v", err)
	}
	if got := mints.Load(); got != 1 {
		t.Errorf("mints = %d, want 1", got)
	}

	// The entry migrated, in memory and on disk.
	for _, cfg := range loadedAndInMemory(t, cfgPath) {
		got := cfg.Servers["srv"]
		if got.AuthMode != config.AuthModeMTLS {
			t.Errorf("auth_mode = %q, want mtls", got.AuthMode)
		}
		if got.ControlToken != "" || !got.ControlTokenExpiresAt.IsZero() {
			t.Errorf("the stale bearer token survived the flip: %+v", got)
		}
		if got.ClientCertFile == "" || got.ClientKeyFile == "" {
			t.Errorf("the entry does not point at the issued credential: %+v", got)
		}
		if !got.ClientCertExpiresAt.Equal(issued.Bundle.ExpiresAt) {
			t.Errorf("client_cert_expires_at = %v, want %v", got.ClientCertExpiresAt, issued.Bundle.ExpiresAt)
		}
	}

	// And the credential really is on disk, loadable as a pair.
	stored := clientConfig.Servers["srv"]
	if _, err := tls.LoadX509KeyPair(stored.ClientCertFile, stored.ClientKeyFile); err != nil {
		t.Errorf("the persisted credential does not load: %v", err)
	}
}

// TestModeFlipMTLSToToken is the other direction: the server goes back to
// token mode, so it no longer requests a certificate and answers the CLI's
// (certificate-less, token-less) request with a 401. The client re-bootstraps,
// gets a bearer token, and — importantly — DELETES the now-useless private key
// rather than leaving it on disk forever.
func TestModeFlipMTLSToToken(t *testing.T) {
	cfgPath := testClientConfig(t)

	var hits int32
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		if r.Header.Get("Authorization") != "Bearer shed_ctl_fresh" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"name":"srv","ssh_port":2222,"auth_mode":"token"}`)
	}))
	defer srv.Close()
	pin := servertls.Fingerprint(srv.Certificate().Raw)

	// Stand up a credential store entry as if the server were still mtls.
	ca := newMTLSServer(t) // borrowed only for its CA / issuance helper
	cred := ca.credential(t, "cli-key", farFromExpiry)
	certPath, keyPath, err := config.WriteClientCredentials("srv",
		[]byte(cred.Bundle.ClientCert), cred.KeyPEM)
	if err != nil {
		t.Fatal(err)
	}
	putServerEntry(t, "srv", config.ServerEntry{
		Host: "127.0.0.1", SSHPort: 2222,
		APIURL:              srv.URL,
		TLSCertFingerprint:  pin,
		AuthMode:            config.AuthModeMTLS,
		ClientCertFile:      certPath,
		ClientKeyFile:       keyPath,
		ClientCertExpiresAt: time.Now().Add(24 * time.Hour), // far from expiry: reactive path only
	})

	newExpiry := time.Now().Add(24 * time.Hour)
	mints := stubBootstrap(func() sdk.Credential {
		return sdk.Credential{Bundle: sdk.Bundle{
			AuthMode: sdk.AuthModeToken, HTTPSPort: 443, TLSCertFingerprint: pin,
			Token: "shed_ctl_fresh", Scope: "control", ExpiresAt: newExpiry,
		}}
	})

	e := clientConfig.Servers["srv"]
	c := NewAPIClientFromEntry(&e, DefaultTimeout)
	if _, err := c.GetInfo(); err != nil {
		t.Fatalf("the client should have recovered a bearer token: %v", err)
	}
	if got := mints.Load(); got != 1 {
		t.Errorf("mints = %d, want 1", got)
	}
	if got := atomic.LoadInt32(&hits); got != 2 {
		t.Errorf("server saw %d requests, want 2 (401 then the authenticated retry)", got)
	}

	for _, cfg := range loadedAndInMemory(t, cfgPath) {
		got := cfg.Servers["srv"]
		if got.AuthMode != config.AuthModeToken {
			t.Errorf("auth_mode = %q, want token", got.AuthMode)
		}
		if got.ControlToken != "shed_ctl_fresh" {
			t.Errorf("control_token = %q, want the fresh one", got.ControlToken)
		}
		if got.ClientCertFile != "" || got.ClientKeyFile != "" || !got.ClientCertExpiresAt.IsZero() {
			t.Errorf("the certificate fields survived the flip back to token mode: %+v", got)
		}
	}
	// The private key must be gone, not merely unreferenced.
	if _, err := os.Stat(keyPath); !os.IsNotExist(err) {
		t.Errorf("the obsolete private key is still on disk at %s (err=%v)", keyPath, err)
	}
}

// TestCredentialLessEntryEnrolls is the upgrade-path recovery: an entry that
// holds NO credential of any kind against a server that demands one.
//
// That state is routine rather than exotic. A pre-mtls client — the brew-
// installed binary a user still has around mid-upgrade — loads config.yaml into
// its own ServerEntry struct and re-saves it, silently dropping every key it
// predates (auth_mode, client_cert_file, client_key_file,
// client_cert_expires_at). What is left is an https entry with a TLS pin and
// nothing to present.
//
// Before the credential-less enrollment path existed, such an entry failed
// PERMANENTLY: it was read as an open server (static, not refreshable), so the
// reactive re-mint was gated off — and it had no credential to be rejected in
// the first place. Every invocation died at the same `remote error: tls:
// certificate required`.
//
// Both server modes are covered, because the client does not (and must not)
// guess which one it is talking to: the bootstrap answers in whatever mode the
// server is actually in, and the entry adopts it.
func TestCredentialLessEntryEnrolls(t *testing.T) {
	t.Run("mtls server: enrolls, adopts mtls, restores the entry", func(t *testing.T) {
		cfgPath := testClientConfig(t)
		m := newMTLSServer(t)

		// Exactly what an older client leaves behind: endpoint + pin, no
		// credential fields at all.
		putServerEntry(t, "srv", config.ServerEntry{
			Host: "127.0.0.1", SSHPort: 2222,
			APIURL:             m.srv.URL,
			TLSCertFingerprint: m.pin,
		})

		issued := m.credential(t, "cli-key", farFromExpiry)
		mints := stubBootstrap(func() sdk.Credential { return issued })

		e := clientConfig.Servers["srv"]
		c := NewAPIClientFromEntry(&e, DefaultTimeout)
		if got := mints.Load(); got != 1 {
			t.Fatalf("mints = %d at construction, want 1 — an entry holding nothing must enroll "+
				"before the first request, not wait for a rejection it cannot provoke", got)
		}
		if _, err := c.GetInfo(); err != nil {
			t.Fatalf("GetInfo after enrollment: %v", err)
		}
		if got := mints.Load(); got != 1 {
			t.Errorf("mints = %d, want 1 (one silent enrollment)", got)
		}
		// The recovery is proactive: the FIRST request already carried the new
		// certificate, so the server saw one request and no refused attempt.
		if got := m.requests.Load(); got != 1 {
			t.Errorf("server saw %d requests, want 1", got)
		}
		if got := m.lastSerial.Load().(string); got != issued.Bundle.CertSerial {
			t.Errorf("the request presented serial %q, want the enrolled %q", got, issued.Bundle.CertSerial)
		}

		// The regression assertion: the fields the older client stripped are back,
		// in memory AND on disk, so the next invocation needs no SSH at all.
		for _, cfg := range loadedAndInMemory(t, cfgPath) {
			got := cfg.Servers["srv"]
			if got.AuthMode != config.AuthModeMTLS {
				t.Errorf("auth_mode = %q, want mtls", got.AuthMode)
			}
			if got.ClientCertFile == "" || got.ClientKeyFile == "" {
				t.Errorf("the entry was not repointed at the issued credential: %+v", got)
			}
			if !got.ClientCertExpiresAt.Equal(issued.Bundle.ExpiresAt) {
				t.Errorf("client_cert_expires_at = %v, want %v", got.ClientCertExpiresAt, issued.Bundle.ExpiresAt)
			}
		}
		stored := clientConfig.Servers["srv"]
		if _, err := tls.LoadX509KeyPair(stored.ClientCertFile, stored.ClientKeyFile); err != nil {
			t.Errorf("the persisted credential does not load: %v", err)
		}

		// And the restored entry stands on its own: a fresh client built from it
		// authenticates with no further bootstrap.
		next := clientConfig.Servers["srv"]
		if _, err := NewAPIClientFromEntry(&next, DefaultTimeout).GetInfo(); err != nil {
			t.Fatalf("the restored entry did not work on its own: %v", err)
		}
		if got := mints.Load(); got != 1 {
			t.Errorf("mints = %d after re-using the restored entry, want 1 (no re-enrollment)", got)
		}
	})

	t.Run("token server: enrolls, adopts token, restores the entry", func(t *testing.T) {
		cfgPath := testClientConfig(t)

		var hits int32
		srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			atomic.AddInt32(&hits, 1)
			if r.Header.Get("Authorization") != "Bearer shed_ctl_fresh" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"name":"srv","ssh_port":2222,"auth_mode":"token"}`)
		}))
		defer srv.Close()
		pin := servertls.Fingerprint(srv.Certificate().Raw)

		putServerEntry(t, "srv", config.ServerEntry{
			Host: "127.0.0.1", SSHPort: 2222,
			APIURL:             srv.URL,
			TLSCertFingerprint: pin,
		})

		newExpiry := time.Now().Add(24 * time.Hour)
		mints := stubBootstrap(func() sdk.Credential {
			return sdk.Credential{Bundle: sdk.Bundle{
				AuthMode: sdk.AuthModeToken, HTTPSPort: 443, TLSCertFingerprint: pin,
				Token: "shed_ctl_fresh", Scope: "control", ExpiresAt: newExpiry,
			}}
		})

		e := clientConfig.Servers["srv"]
		c := NewAPIClientFromEntry(&e, DefaultTimeout)
		if _, err := c.GetInfo(); err != nil {
			t.Fatalf("GetInfo after enrollment: %v", err)
		}
		if got := mints.Load(); got != 1 {
			t.Errorf("mints = %d, want 1", got)
		}
		if got := atomic.LoadInt32(&hits); got != 1 {
			t.Errorf("server saw %d requests, want 1 (the enrollment landed before the first one)", got)
		}

		for _, cfg := range loadedAndInMemory(t, cfgPath) {
			got := cfg.Servers["srv"]
			if got.AuthMode != config.AuthModeToken {
				t.Errorf("auth_mode = %q, want token", got.AuthMode)
			}
			if got.ControlToken != "shed_ctl_fresh" {
				t.Errorf("control_token = %q, want the enrolled one", got.ControlToken)
			}
			if !got.ControlTokenExpiresAt.Equal(newExpiry) {
				t.Errorf("control_token_expires_at = %v, want %v", got.ControlTokenExpiresAt, newExpiry)
			}
		}
	})

	// The other half of the discrimination: an OPEN server also has no
	// credential, and must keep costing zero SSH round-trips. Reading "no
	// credential" as "enroll" unconditionally would put a bootstrap in front of
	// every command against every open server.
	t.Run("open server: no credential, no bootstrap", func(t *testing.T) {
		testClientConfig(t)

		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"name":"srv","ssh_port":2222,"auth_mode":"open"}`)
		}))
		defer srv.Close()

		host, port := hostPortOf(t, srv.URL)
		putServerEntry(t, "srv", config.ServerEntry{Host: host, HTTPPort: port, SSHPort: 2222})

		mints := stubBootstrap(func() sdk.Credential { return sdk.Credential{} })

		e := clientConfig.Servers["srv"]
		c := NewAPIClientFromEntry(&e, DefaultTimeout)
		if _, err := c.GetInfo(); err != nil {
			t.Fatalf("GetInfo against an open server: %v", err)
		}
		if got := mints.Load(); got != 0 {
			t.Errorf("mints = %d against an open server, want 0", got)
		}
	})
}

// TestNamedEntryEnrollsWithDuplicateEndpointAlias is the regression for #295:
// endpoint matching is intentionally ambiguous when two aliases point at the
// same server, but the alias selected by the command is not. The issued private
// key must be stored under that selected alias and no other.
func TestNamedEntryEnrollsWithDuplicateEndpointAlias(t *testing.T) {
	cfgPath := testClientConfig(t)
	m := newMTLSCredentialIssuer(t)

	entry := config.ServerEntry{
		Host: "127.0.0.1", SSHPort: 2222,
		APIURL:             "https://localhost:18443",
		TLSCertFingerprint: m.pin,
	}
	putServerEntry(t, "my-server-dev", entry)
	putServerEntry(t, "dev-mtls", entry)

	issued := m.credential(t, "cli-key", farFromExpiry)
	mints := stubBootstrap(func() sdk.Credential { return issued })

	_ = NewAPIClientFromNamedEntry("dev-mtls", &entry, DefaultTimeout)
	if got := mints.Load(); got != 1 {
		t.Fatalf("mints = %d, want 1", got)
	}

	for _, cfg := range loadedAndInMemory(t, cfgPath) {
		got := cfg.Servers["dev-mtls"]
		if got.AuthMode != config.AuthModeMTLS || got.ClientCertFile == "" || got.ClientKeyFile == "" {
			t.Errorf("selected alias was not updated with the issued credential: %+v", got)
		}
		other := cfg.Servers["my-server-dev"]
		if other.AuthMode != "" || other.ClientCertFile != "" || other.ClientKeyFile != "" {
			t.Errorf("unselected alias was modified: %+v", other)
		}
	}

	if _, err := os.Stat(filepath.Join(config.ServerCredsDir("dev-mtls"), "client.pem")); err != nil {
		t.Errorf("selected alias certificate was not persisted: %v", err)
	}
	if _, err := os.Stat(config.ServerCredsDir("my-server-dev")); !os.IsNotExist(err) {
		t.Errorf("credential directory for the unselected alias exists (err=%v)", err)
	}
}

// TestCredentialLessEntryWithoutConfigMatchMintsWithoutPersisting covers the
// genuinely unresolvable fallback: a one-off entry can enroll for this process,
// but there is no safe config row to update. Under -v that mint-only result must
// be visible rather than silently causing re-enrollment on every invocation.
func TestCredentialLessEntryWithoutConfigMatchMintsWithoutPersisting(t *testing.T) {
	testClientConfig(t)
	m := newMTLSCredentialIssuer(t)

	entry := config.ServerEntry{
		Host: "127.0.0.1", SSHPort: 2222,
		APIURL:             "https://localhost:18443",
		TLSCertFingerprint: m.pin,
	}
	issued := m.credential(t, "cli-key", farFromExpiry)
	mints := stubBootstrap(func() sdk.Credential { return issued })

	origVerbose := verboseLevel
	verboseLevel = 1
	t.Cleanup(func() { verboseLevel = origVerbose })
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	origStderr := os.Stderr
	os.Stderr = w
	t.Cleanup(func() { os.Stderr = origStderr })

	_ = NewAPIClientFromEntry(&entry, DefaultTimeout)
	_ = w.Close()
	os.Stderr = origStderr
	warning, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	_ = r.Close()

	if got := mints.Load(); got != 1 {
		t.Fatalf("mints = %d, want 1", got)
	}
	if !strings.Contains(string(warning), "was not persisted: no configured server matches this entry") {
		t.Errorf("verbose warning = %q, want an observable no-match persistence skip", warning)
	}
	if len(clientConfig.Servers) != 0 {
		t.Errorf("one-off enrollment unexpectedly changed config: %+v", clientConfig.Servers)
	}
	credsRoot := filepath.Dir(config.ServerCredsDir("unused"))
	if _, err := os.Stat(credsRoot); !os.IsNotExist(err) {
		t.Errorf("one-off enrollment created a credential store at %s (err=%v)", credsRoot, err)
	}
}

// TestTunnelCredentialSourceRefreshIsMintOnly pins the long-lived daemon
// contract: it can refresh an mtls certificate in memory, but must not write its
// stale entry copy or create credential files.
func TestTunnelCredentialSourceRefreshIsMintOnly(t *testing.T) {
	testClientConfig(t)
	m := newMTLSCredentialIssuer(t)

	initial := m.credential(t, "cli-key", farFromExpiry)
	cert, err := tls.X509KeyPair([]byte(initial.Bundle.ClientCert), initial.KeyPEM)
	if err != nil {
		t.Fatal(err)
	}
	entry := config.ServerEntry{
		Host: "127.0.0.1", SSHPort: 2222,
		APIURL:             "https://localhost:18443",
		TLSCertFingerprint: m.pin,
	}
	putServerEntry(t, "dev-mtls", entry)
	client := newAPIClientWithSource(
		entry.BaseURL(),
		entry.TLSCertFingerprint,
		clienttoken.New(clienttoken.MTLSCredential(&cert, initial.Bundle.ExpiresAt), nil),
		DefaultTimeout,
	)

	issued := m.credential(t, "cli-key", farFromExpiry)
	mints := stubBootstrap(func() sdk.Credential { return issued })
	source := tunnelCredentialSource(client, &entry, tunnels.ConnectTarget{TLSPin: m.pin})
	if source == nil {
		t.Fatal("secure tunnel should have a credential source")
	}
	_, generation := source.Current()
	fresh, err := source.Refresh(generation)
	if err != nil {
		t.Fatalf("tunnel credential refresh: %v", err)
	}
	if got := mints.Load(); got != 1 {
		t.Fatalf("mints = %d, want 1", got)
	}
	if fresh.ClientCertificate() == nil {
		t.Fatal("tunnel did not adopt the freshly minted certificate in memory")
	}
	if got := clientConfig.Servers["dev-mtls"]; got.AuthMode != "" || got.ClientCertFile != "" || got.ClientKeyFile != "" {
		t.Errorf("tunnel refresh rewrote its config entry: %+v", got)
	}
	if _, err := os.Stat(config.ServerCredsDir("dev-mtls")); !os.IsNotExist(err) {
		t.Errorf("tunnel refresh created a credential directory (err=%v)", err)
	}
}

// hostPortOf splits a test server's URL into the host and numeric port a legacy
// (plain-HTTP) config entry stores.
func hostPortOf(t *testing.T, rawURL string) (string, int) {
	t.Helper()
	u, err := url.Parse(rawURL)
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil {
		t.Fatalf("parse port of %s: %v", rawURL, err)
	}
	return u.Hostname(), port
}

// loadedAndInMemory returns the in-memory config and the one re-read from disk,
// so an assertion can be made against both without duplicating it.
func loadedAndInMemory(t *testing.T, cfgPath string) []*config.ClientConfig {
	t.Helper()
	reloaded, err := config.LoadClientConfigFromPath(cfgPath)
	if err != nil {
		t.Fatalf("reload config: %v", err)
	}
	return []*config.ClientConfig{clientConfig, reloaded}
}

// TestReauthDropsPooledConnection covers the case the handshake-rejection tests
// structurally cannot: a certificate that is accepted at the TLS handshake and
// then refused PER REQUEST.
//
// That is the shed server's actual mtls posture (internal/api re-validates the
// peer certificate on every request, precisely because crypto/tls verifies it
// only once per connection). So a certificate that expires — or an identity
// de-authorized — while a keep-alive connection is open produces a 401 on a
// connection that is otherwise healthy and pooled.
//
// Re-minting alone does not fix that. The pooled connection still carries the
// OLD certificate in its completed handshake, so the retry would replay exactly
// the credential the server just rejected. Dropping idle connections is what
// forces a fresh handshake — and therefore a fresh GetClientCertificate call —
// which is what makes "re-mint, then retry once" actually retry with the new
// credential.
func TestReauthDropsPooledConnection(t *testing.T) {
	testClientConfig(t)
	m := newMTLSServer(t)

	first := m.credential(t, "cli-key", farFromExpiry)
	putServerEntry(t, "srv", mtlsEntry(t, m, "srv", first))

	second := m.credential(t, "cli-key", farFromExpiry)
	mints := stubBootstrap(func() sdk.Credential { return second })

	e := clientConfig.Servers["srv"]
	c := NewAPIClientFromEntry(&e, DefaultTimeout)

	// First request succeeds and leaves a pooled, already-handshaken connection
	// behind — the precondition this test exists to create.
	if _, err := c.GetInfo(); err != nil {
		t.Fatalf("first GetInfo: %v", err)
	}
	if got := m.lastSerial.Load().(string); got != first.Bundle.CertSerial {
		t.Fatalf("first request presented %s, want %s", got, first.Bundle.CertSerial)
	}

	// The identity is de-authorized between requests. The connection stays open.
	m.revoke(first.Bundle.CertSerial)

	if _, err := c.GetInfo(); err != nil {
		t.Fatalf("the client should have re-minted and retried on the per-request 401: %v", err)
	}
	if got := mints.Load(); got != 1 {
		t.Errorf("mints = %d, want exactly 1", got)
	}
	if got := m.lastSerial.Load().(string); got != second.Bundle.CertSerial {
		t.Errorf("the retry presented serial %s, want the re-minted %s — the pooled connection "+
			"was reused and replayed the rejected certificate", got, second.Bundle.CertSerial)
	}
}
