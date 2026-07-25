package main

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	sdk "github.com/charliek/shed/sdk"
	"github.com/charliek/shed/sdk/creds"
)

// mtls_test.go covers the host-agent against an auth.mode: mtls server — the
// commit's whole point. The agent has THREE places a credential leaves it (the
// plugin bus, the egress stream, and the desktop socket) and each is wired
// separately, so each is asserted separately.

// issueClientCert mints a self-signed client certificate + its key PEM, standing
// in for what an mtls server's internal CA returns over the bootstrap channel.
func issueClientCert(t *testing.T, cn string, notAfter time.Time) (certPEM, keyPEM []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: cn, OrganizationalUnit: []string{scopeCredentials}},
		NotBefore:    time.Now().Add(-5 * time.Minute),
		NotAfter:     notAfter,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, key.Public(), key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
}

// mtlsMint is the mintResult an mtls-mode server produces.
func mtlsMint(t *testing.T, cn string, notAfter time.Time) mintResult {
	t.Helper()
	certPEM, keyPEM := issueClientCert(t, cn, notAfter)
	return mintResult{cred: sdk.Credential{
		Bundle: sdk.Bundle{
			AuthMode:   sdk.AuthModeMTLS,
			ClientCert: string(certPEM),
			CertSerial: "0a0b0c",
			ExpiresAt:  notAfter,
		},
		KeyPEM: keyPEM,
	}}
}

func mtlsTarget(name string) ServerTarget {
	return ServerTarget{Name: name, URL: "https://s.example:8443", SSHHost: "s.example", SSHPort: 2222, AuthMode: authModeMTLS}
}

// TestCredentialSourceAdoptsACertificate: the source adopts whichever shape the
// server issued, and reports it through the two interfaces the transports use.
// In mtls state Token() is empty AND non-erroring — the credential is real, it
// simply is not a bearer token.
func TestCredentialSourceAdoptsACertificate(t *testing.T) {
	far := time.Now().Add(24 * time.Hour)
	fm := &fakeMinter{results: []mintResult{mtlsMint(t, "SHA256:abc", far)}}
	s := newCredentialSource(context.Background(), fm, mtlsTarget("s"), scopeCredentials, nil, nil)

	tok, err := s.Token()
	if err != nil {
		t.Fatalf("Token: %v", err)
	}
	if tok != "" {
		t.Errorf("Token = %q, want empty in mtls state (the certificate carries the identity)", tok)
	}
	cert := s.ClientCertificate()
	if cert == nil {
		t.Fatal("ClientCertificate = nil, want the issued certificate")
	}
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	if leaf.Subject.CommonName != "SHA256:abc" {
		t.Errorf("presented CN = %q, want SHA256:abc", leaf.Subject.CommonName)
	}
	if got := s.Mode(); got != "mtls" {
		t.Errorf("Mode = %q, want mtls", got)
	}
	if fm.calls != 1 {
		t.Errorf("mint calls = %d, want 1", fm.calls)
	}
}

// TestShouldMintIgnoresAuthMode: the recorded auth_mode must not gate brokering.
//
// Both secure modes are reached over https and mint over the same channel, and the
// mint is CSR-first and mode-agnostic, so the server's answer decides the shape.
// If auth_mode gated the decision, a stale or unrecognized value would silently
// stop the agent from brokering for a perfectly reachable server — the exact
// failure a mode flip is supposed to be incapable of causing.
func TestShouldMintIgnoresAuthMode(t *testing.T) {
	deps := SharedDeps{Minter: NewCredentialMinter("/nonexistent")}
	base := ServerTarget{Name: "s", URL: "https://s:8443", SSHHost: "s", SSHPort: 2222}
	for _, mode := range []string{"", authModeToken, authModeMTLS, "MTLS", "something-new"} {
		target := base
		target.AuthMode = mode
		if !shouldMint(deps, target) {
			t.Errorf("shouldMint with auth_mode %q = false, want true", mode)
		}
	}
	// And it still does not rescue a server that genuinely cannot be minted for.
	open := base
	open.URL, open.AuthMode = "http://s:8080", authModeMTLS
	if shouldMint(deps, open) {
		t.Error("an http server must not be minted for, whatever auth_mode claims")
	}
}

// TestCredentialSourceFlipsBetweenModes: one source, one server, two modes. The
// flip is driven entirely by what the server answered — nothing local is
// reconfigured — and it works in both directions.
func TestCredentialSourceFlipsBetweenModes(t *testing.T) {
	far := time.Now().Add(24 * time.Hour)
	fm := &fakeMinter{results: []mintResult{
		tokenMint("tok-1", far),
		mtlsMint(t, "SHA256:abc", far),
		tokenMint("tok-2", far),
	}}
	s := newCredentialSource(context.Background(), fm, mtlsTarget("s"), scopeCredentials, nil, nil)

	if tok, _ := s.Token(); tok != "tok-1" {
		t.Fatalf("Token = %q, want tok-1 (token state)", tok)
	}
	if s.ClientCertificate() != nil {
		t.Error("a token-state source must present no certificate")
	}

	s.Invalidate() // server flipped to mtls; the next mint returns a certificate
	if tok, _ := s.Token(); tok != "" {
		t.Errorf("Token = %q, want empty after the flip to mtls", tok)
	}
	if s.ClientCertificate() == nil {
		t.Error("an mtls-state source must present its certificate")
	}

	s.Invalidate() // and back again
	if tok, _ := s.Token(); tok != "tok-2" {
		t.Errorf("Token = %q, want tok-2 after the flip back to token", tok)
	}
	if s.ClientCertificate() != nil {
		t.Error("a source flipped back to token must stop presenting a certificate")
	}
}

// TestCredentialSourcePersistsAndRehydrates: the agent's credentials-scope
// certificate survives a restart. Without this every restart costs an SSH
// round-trip before the bus can connect, and a server that is briefly
// unreachable over SSH turns a restart into an outage.
func TestCredentialSourcePersistsAndRehydrates(t *testing.T) {
	root := t.TempDir()
	store := creds.NewStore(filepath.Join(root, "creds", scopeCredentials))
	far := time.Now().Add(24 * time.Hour)
	target := mtlsTarget("prod")

	first := &fakeMinter{results: []mintResult{mtlsMint(t, "SHA256:abc", far)}}
	s1 := newCredentialSource(context.Background(), first, target, scopeCredentials, store, nil)
	if _, err := s1.Token(); err != nil {
		t.Fatalf("first Token: %v", err)
	}
	presented := s1.ClientCertificate()
	if presented == nil {
		t.Fatal("no certificate after the first mint")
	}

	certPath, keyPath := store.Paths("prod")
	for _, p := range []string{certPath, keyPath} {
		fi, err := os.Stat(p)
		if err != nil {
			t.Fatalf("stat %s: %v", p, err)
		}
		if fi.Mode().Perm() != 0600 {
			t.Errorf("%s mode = %v, want 0600", p, fi.Mode().Perm())
		}
	}
	if fi, err := os.Stat(store.ServerDir("prod")); err != nil {
		t.Fatal(err)
	} else if fi.Mode().Perm() != 0700 {
		t.Errorf("creds dir mode = %v, want 0700", fi.Mode().Perm())
	}

	// A "restart": a brand-new source over the same store, with a minter that
	// would fail if it were consulted at all.
	second := &fakeMinter{results: []mintResult{{err: errUnexpectedMint}}}
	s2 := newCredentialSource(context.Background(), second, target, scopeCredentials, store, nil)
	rehydrated := s2.ClientCertificate()
	if rehydrated == nil {
		t.Fatal("the persisted certificate was not rehydrated; the agent would re-enroll on every restart")
	}
	if string(rehydrated.Certificate[0]) != string(presented.Certificate[0]) {
		t.Error("rehydrated a different certificate than the one persisted")
	}
	if second.calls != 0 {
		t.Errorf("mint calls after restart = %d, want 0 (the stored credential is still valid)", second.calls)
	}
}

// TestCredentialSourceIgnoresStoredCertForATokenEntry: material for a mode the
// entry does not record must not be loaded. Presenting a stale certificate to a
// server that has been flipped back to token mode is exactly the visible
// breakage a flip is supposed to avoid.
func TestCredentialSourceIgnoresStoredCertForATokenEntry(t *testing.T) {
	store := creds.NewStore(filepath.Join(t.TempDir(), "creds"))
	certPEM, keyPEM := issueClientCert(t, "SHA256:abc", time.Now().Add(24*time.Hour))
	if _, _, err := store.Write("prod", certPEM, keyPEM); err != nil {
		t.Fatal(err)
	}
	target := mtlsTarget("prod")
	target.AuthMode = authModeToken

	s := newCredentialSource(context.Background(), &fakeMinter{results: []mintResult{{err: errUnexpectedMint}}},
		target, scopeCredentials, store, nil)
	if s.ClientCertificate() != nil {
		t.Error("a token-mode entry must not load a stored certificate")
	}
}

// TestCredentialSourceRemovesStoredCertOnFlipBackToToken: once the server stops
// issuing certificates, the private key on disk is residue for a mode that no
// longer exists. It is removed rather than left to be loaded by a later start.
func TestCredentialSourceRemovesStoredCertOnFlipBackToToken(t *testing.T) {
	store := creds.NewStore(filepath.Join(t.TempDir(), "creds"))
	far := time.Now().Add(24 * time.Hour)
	fm := &fakeMinter{results: []mintResult{mtlsMint(t, "SHA256:abc", far), tokenMint("tok", far)}}
	s := newCredentialSource(context.Background(), fm, mtlsTarget("prod"), scopeCredentials, store, nil)

	if _, err := s.Token(); err != nil {
		t.Fatalf("Token: %v", err)
	}
	certPath, _ := store.Paths("prod")
	if _, err := os.Stat(certPath); err != nil {
		t.Fatalf("expected the certificate to be persisted: %v", err)
	}

	s.Invalidate() // server flipped back to token
	if tok, _ := s.Token(); tok != "tok" {
		t.Fatalf("Token = %q, want tok", tok)
	}
	if _, err := os.Stat(certPath); !os.IsNotExist(err) {
		t.Errorf("stale certificate still on disk after the flip back to token (err=%v)", err)
	}
}

var errUnexpectedMint = errUnexpected("the minter must not be called")

type errUnexpected string

func (e errUnexpected) Error() string { return string(e) }

// --- transport assertions ---------------------------------------------------

// clientCertRecorder is a TLS test server that requests a client certificate and
// records the CN it was shown (empty when none was presented), plus the
// Authorization header — the pair that proves which credential actually travelled.
type clientCertRecorder struct {
	srv  *httptest.Server
	cn   atomic.Value
	auth atomic.Value
}

func newClientCertRecorder(t *testing.T, handler http.HandlerFunc) *clientCertRecorder {
	t.Helper()
	r := &clientCertRecorder{}
	r.cn.Store("")
	r.auth.Store("")
	r.srv = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		r.auth.Store(req.Header.Get("Authorization"))
		if req.TLS != nil && len(req.TLS.PeerCertificates) > 0 {
			r.cn.Store(req.TLS.PeerCertificates[0].Subject.CommonName)
		}
		handler(w, req)
	}))
	// RequestClientCert (not RequireAndVerify): the point is to observe what the
	// client offers, including offering nothing, rather than to reproduce the
	// server's enforcement — which internal/servertls already covers.
	r.srv.TLS = &tls.Config{MinVersion: tls.VersionTLS12, ClientAuth: tls.RequestClientCert}
	r.srv.StartTLS()
	t.Cleanup(r.srv.Close)
	return r
}

func (r *clientCertRecorder) pin() string {
	sum := sha256.Sum256(r.srv.Certificate().Raw)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func (r *clientCertRecorder) peerCN() string { return r.cn.Load().(string) }
func (r *clientCertRecorder) header() string { return r.auth.Load().(string) }

// TestBusTransportPresentsTheCertificate wires the credential-bus client exactly
// as the supervisor does and asserts, from the SERVER side, that the certificate
// is on the wire and the bearer header is not.
func TestBusTransportPresentsTheCertificate(t *testing.T) {
	rec := newClientCertRecorder(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	far := time.Now().Add(24 * time.Hour)

	t.Run("mtls", func(t *testing.T) {
		fm := &fakeMinter{results: []mintResult{mtlsMint(t, "SHA256:bus", far)}}
		target := ServerTarget{Name: "s", URL: rec.srv.URL, TLSFingerprint: rec.pin(), SSHHost: "h", SSHPort: 2222, AuthMode: authModeMTLS}
		src := newCredentialSource(context.Background(), fm, target, scopeCredentials, nil, nil)
		c := sdk.NewHostClient(busClientOptions(target, src, nil)...)
		if err := c.Respond(context.Background(), "ns", &sdk.Envelope{}); err != nil {
			t.Fatalf("Respond: %v", err)
		}
		if got := rec.peerCN(); got != "SHA256:bus" {
			t.Errorf("server saw peer CN %q, want SHA256:bus — the bus is not presenting the certificate", got)
		}
		if got := rec.header(); got != "" {
			t.Errorf("Authorization = %q, want none alongside the certificate", got)
		}
	})

	t.Run("token", func(t *testing.T) {
		fm := &fakeMinter{results: []mintResult{tokenMint("shed_creds_bus", far)}}
		target := ServerTarget{Name: "s", URL: rec.srv.URL, TLSFingerprint: rec.pin(), SSHHost: "h", SSHPort: 2222}
		src := newCredentialSource(context.Background(), fm, target, scopeCredentials, nil, nil)
		c := sdk.NewHostClient(busClientOptions(target, src, nil)...)
		if err := c.Respond(context.Background(), "ns", &sdk.Envelope{}); err != nil {
			t.Fatalf("Respond: %v", err)
		}
		// The same wiring, with the same certificate provider attached, must still
		// authenticate a token-mode server.
		if got := rec.header(); got != "Bearer shed_creds_bus" {
			t.Errorf("Authorization = %q, want the bearer token", got)
		}
	})
}

// TestEgressTransportPresentsTheCertificate: the egress subscriber builds its
// OWN http.Client, so the bus being correct says nothing about it. This is the
// transport that would otherwise reconnect forever against an mtls server.
func TestEgressTransportPresentsTheCertificate(t *testing.T) {
	rec := newClientCertRecorder(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK) // empty body → the stream returns promptly
	})
	certPEM, keyPEM := issueClientCert(t, "SHA256:egress", time.Now().Add(24*time.Hour))
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatal(err)
	}

	target := ServerTarget{Name: "s", URL: rec.srv.URL, TLSFingerprint: rec.pin()}
	src := &fakeTokenSource{cert: &cert}
	sub := NewEgressSubscriber(target, src, NewAuditLogger(LogConfig{Enabled: false}, testLogger()), testLogger())
	if err := sub.stream(context.Background()); err != nil {
		t.Fatalf("stream: %v", err)
	}
	if got := rec.peerCN(); got != "SHA256:egress" {
		t.Errorf("server saw peer CN %q, want SHA256:egress — the egress stream is not presenting the certificate", got)
	}
	if got := rec.header(); got != "" {
		t.Errorf("Authorization = %q, want none in mtls state", got)
	}
}

// TestEgressReMintsOnACertificateRejection: a refused certificate arrives as a
// transport error, not a 401, so a subscriber that only watched statuses would
// replay the refused credential on every reconnect forever.
func TestEgressReMintsOnACertificateRejection(t *testing.T) {
	// RequireAndVerifyClientCert with an empty pool rejects every certificate,
	// producing the real "unknown certificate authority" alert.
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	srv.TLS = &tls.Config{
		MinVersion: tls.VersionTLS12,
		ClientAuth: tls.RequireAndVerifyClientCert,
		ClientCAs:  x509.NewCertPool(),
	}
	srv.StartTLS()
	defer srv.Close()
	sum := sha256.Sum256(srv.Certificate().Raw)

	certPEM, keyPEM := issueClientCert(t, "SHA256:egress", time.Now().Add(24*time.Hour))
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	src := &fakeTokenSource{cert: &cert}
	target := ServerTarget{Name: "s", URL: srv.URL, TLSFingerprint: "sha256:" + hex.EncodeToString(sum[:])}
	sub := NewEgressSubscriber(target, src, NewAuditLogger(LogConfig{Enabled: false}, testLogger()), testLogger())

	if err := sub.stream(context.Background()); err == nil {
		t.Fatal("expected the rejected certificate to fail the stream")
	}
	if src.invalidated != 1 {
		t.Errorf("Invalidate calls = %d, want 1 (a refused certificate must trigger a re-mint)", src.invalidated)
	}
}

// --- the desktop UDS credential.get protocol --------------------------------

// TestHelloAckAdvertisesCredentialGet: the capability is how a NEW app avoids
// sending credential.get into an OLD agent's read loop, where it would be
// silently dropped and time out. It has to be in the ack.
func TestHelloAckAdvertisesCredentialGet(t *testing.T) {
	ack := helloAckMsg{AgentCapabilities: agentCapabilities()}
	raw, err := json.Marshal(ack)
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	caps, ok := decoded["agent_capabilities"].([]any)
	if !ok || len(caps) == 0 || caps[0] != capCredentialGet {
		t.Fatalf("agent_capabilities = %v, want [%q]", decoded["agent_capabilities"], capCredentialGet)
	}

	// The OLD-agent shape, spelled out: no capabilities means the key is absent
	// entirely, which is the exact condition a new app keys "upgrade
	// shed-host-agent" on. A present-but-empty array would read as "an agent that
	// supports nothing", which is a different (and never emitted) statement.
	raw, err = json.Marshal(helloAckMsg{})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "agent_capabilities") {
		t.Errorf("an ack with no capabilities must omit the key entirely, got %s", raw)
	}
}

// TestCredentialGetCompatMatrix drives the real DesktopServer over its socket
// across the four legs that matter for two separately released components.
func TestCredentialGetCompatMatrix(t *testing.T) {
	far := time.Now().Add(24 * time.Hour).UTC().Truncate(time.Second)
	csr := base64.StdEncoding.EncodeToString([]byte("a-pkcs10-der"))

	t.Run("new app, new agent, token server", func(t *testing.T) {
		fm := &fakeMinter{results: []mintResult{tokenMint("ctl-tok", far)}}
		resp := driveCredentialGet(t, fm, prodSecureShedConfig, credentialGetMsg{Type: "credential.get", ID: "q1", Server: "prod", CSR: csr})

		if resp.Error != "" {
			t.Fatalf("unexpected error: %s", resp.Error)
		}
		if resp.AuthMode != sdk.AuthModeToken || resp.Token != "ctl-tok" {
			t.Errorf("auth_mode/token = %q/%q, want token/ctl-tok", resp.AuthMode, resp.Token)
		}
		if resp.ClientCert != "" {
			t.Error("a token-mode answer must carry no certificate")
		}
		if resp.InReplyTo != "q1" {
			t.Errorf("in_reply_to = %q, want q1", resp.InReplyTo)
		}
		// The CSR is relayed even to a token server: the app cannot know the mode
		// in advance, and a token-mode server ignores it by design.
		if got := fm.relayedCSRs(); len(got) != 1 || got[0] != csr {
			t.Errorf("relayed CSRs = %v, want exactly [%q]", got, csr)
		}
	})

	t.Run("new app, new agent, mtls server", func(t *testing.T) {
		certPEM, _ := issueClientCert(t, "SHA256:desktop", far)
		fm := &fakeMinter{results: []mintResult{{cred: sdk.Credential{Bundle: sdk.Bundle{
			AuthMode: sdk.AuthModeMTLS, ClientCert: string(certPEM), CertSerial: "0a0b", ExpiresAt: far,
		}}}}}
		resp := driveCredentialGet(t, fm, prodMTLSShedConfig, credentialGetMsg{Type: "credential.get", ID: "q2", Server: "prod", CSR: csr})

		if resp.Error != "" {
			t.Fatalf("unexpected error: %s", resp.Error)
		}
		if resp.AuthMode != sdk.AuthModeMTLS {
			t.Errorf("auth_mode = %q, want mtls", resp.AuthMode)
		}
		if resp.ClientCert != string(certPEM) {
			t.Error("the issued certificate did not reach the app intact")
		}
		if resp.CertSerial != "0a0b" || resp.ExpiresAt != far.Format(time.RFC3339) {
			t.Errorf("serial/expiry = %q/%q, want 0a0b/%s", resp.CertSerial, resp.ExpiresAt, far.Format(time.RFC3339))
		}
		if resp.Token != "" {
			t.Error("an mtls answer must carry no bearer token")
		}
		// The KEY never crosses the socket — only the CSR went in, and only a
		// certificate came back. This is the assertion the ownership table (D6)
		// turns on.
		if strings.Contains(resp.ClientCert, "PRIVATE KEY") {
			t.Error("a private key appeared in the credential.response")
		}
		if got := fm.relayedCSRs(); len(got) != 1 || got[0] != csr {
			t.Errorf("relayed CSRs = %v, want the app's CSR verbatim", got)
		}
	})

	// OLD app, NEW agent: the app only knows token.get. Against an mtls server a
	// certificate cannot be delivered through a token.response, so the answer is
	// an explicit upgrade error rather than an empty token.
	t.Run("old app, new agent, mtls server", func(t *testing.T) {
		fm := &fakeMinter{results: []mintResult{{err: errUnexpectedMint}}}
		resp := driveTokenGet(t, fm, prodMTLSShedConfig, tokenGetMsg{Type: "token.get", ID: "q3", Server: "prod"})

		if resp.Token != "" {
			t.Error("fail closed: no token may accompany the error")
		}
		if !strings.Contains(resp.Error, "upgrade the app") {
			t.Errorf("error = %q, want it to name the component to upgrade", resp.Error)
		}
		if fm.calls != 0 {
			t.Errorf("mint calls = %d, want 0 (the recorded auth_mode answers this without an SSH round-trip)", fm.calls)
		}
	})

	// OLD app, NEW agent, token server: completely unchanged. The capability is
	// additive; nothing about token.get moved.
	t.Run("old app, new agent, token server", func(t *testing.T) {
		fm := &fakeMinter{results: []mintResult{tokenMint("ctl-tok", far)}}
		resp := driveTokenGet(t, fm, prodSecureShedConfig, tokenGetMsg{Type: "token.get", ID: "q4", Server: "prod"})

		if resp.Error != "" {
			t.Fatalf("unexpected error: %s", resp.Error)
		}
		if resp.Token != "ctl-tok" {
			t.Errorf("token = %q, want ctl-tok", resp.Token)
		}
	})

	// A new app that sends no CSR to an mtls server gets the server's own error
	// relayed, not a silent empty answer.
	t.Run("credential.get without a csr against an mtls server", func(t *testing.T) {
		fm := &fakeMinter{results: []mintResult{{err: errUnexpected("this server requires auth.mode: mtls; upgrade shed (client certificate support)")}}}
		resp := driveCredentialGet(t, fm, prodMTLSShedConfig, credentialGetMsg{Type: "credential.get", ID: "q5", Server: "prod"})

		if resp.AuthMode != "" || resp.Token != "" || resp.ClientCert != "" {
			t.Error("fail closed: an errored credential.response must carry no credential fields")
		}
		if !strings.Contains(resp.Error, "requires auth.mode: mtls") {
			t.Errorf("error = %q, want the server's own upgrade message relayed", resp.Error)
		}
	})
}

const prodMTLSShedConfig = `
servers:
  prod:
    api_url: https://prod.example:8443
    host: prod.example
    ssh_port: 2222
    auth_mode: mtls
`

// driveCredentialGet runs one credential.get against a live DesktopServer and
// returns the decoded reply.
func driveCredentialGet(t *testing.T, m relayMinter, shedConfig string, req credentialGetMsg) credentialResponseMsg {
	t.Helper()
	var resp credentialResponseMsg
	driveDesktopRequest(t, m, shedConfig, req, "credential.response", &resp)
	return resp
}

func driveTokenGet(t *testing.T, m relayMinter, shedConfig string, req tokenGetMsg) tokenResponseMsg {
	t.Helper()
	var resp tokenResponseMsg
	driveDesktopRequest(t, m, shedConfig, req, "token.response", &resp)
	return resp
}

// driveDesktopRequest stands up a real DesktopServer on a real socket, completes
// the handshake, sends one request frame, and decodes the reply of wantType into
// out. It drives the actual server rather than calling the handler directly, so
// the JSON tags, the read-loop dispatch, and the capability advertisement are all
// exercised as the app will see them.
func driveDesktopRequest(t *testing.T, m relayMinter, shedConfig string, req any, wantType string, out any) {
	t.Helper()
	cfg := writeShedConfig(t, shedConfig)
	sock := shortSocketPath(t)
	audit := NewAuditLogger(LogConfig{Enabled: false}, testLogger())
	s := NewDesktopServer(sock, 100*time.Millisecond, audit, "test", nil, testLogger())
	s.SetControlTokens(newControlTokenProvider(context.Background(), m, cfg)) // before Listen
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.Listen(ctx)
	waitForSocket(t, sock)

	conn, r := dialHello(t, s, sock)
	defer conn.Close()

	line, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Write(append(line, '\n')); err != nil {
		t.Fatalf("write %T: %v", req, err)
	}
	frame := readType(t, conn, r, wantType)
	raw, err := json.Marshal(frame)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, out); err != nil {
		t.Fatalf("decode %s: %v", wantType, err)
	}
}

// TestDialHelloSeesTheCapability pins that the capability actually reaches an app
// over the socket — the ack is built by the server, not by the test's own struct.
func TestDialHelloSeesTheCapability(t *testing.T) {
	s, _, cancel, sock := startTestServer(t, 100)
	defer cancel()
	conn, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	if _, err := conn.Write([]byte(`{"type":"hello","client":{"name":"t","version":"1","pid":1}}` + "\n")); err != nil {
		t.Fatal(err)
	}
	frame := readType(t, conn, bufio.NewReader(conn), "hello_ack")
	caps, _ := frame["agent_capabilities"].([]any)
	found := false
	for _, c := range caps {
		if c == capCredentialGet {
			found = true
		}
	}
	if !found {
		t.Errorf("hello_ack agent_capabilities = %v, want it to contain %q", frame["agent_capabilities"], capCredentialGet)
	}
	_ = s
}
