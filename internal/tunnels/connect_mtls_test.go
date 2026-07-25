package tunnels

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/charliek/shed/internal/clienttoken"
	"github.com/charliek/shed/internal/servertls"
)

// A tunnel is the one long-lived thing in the CLI: a background `shed forward`
// can outlive several certificate TTLs. These tests cover the tunnel's own dial
// and probe paths against a real mtls listener — the CLI control plane's are in
// cmd/shed.

// issueTunnelCert runs the real enrollment path against ca and returns a
// ready-to-present certificate plus its serial.
func issueTunnelCert(t *testing.T, ca servertls.CA, label string, ttl time.Duration) (*tls.Certificate, string) {
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
	sum := sha256.Sum256([]byte(label))
	fp := "SHA256:" + base64.RawStdEncoding.EncodeToString(sum[:])
	certDER, err := ca.SignClientCSR(csrDER, fp, "control", "cli", ttl)
	if err != nil {
		t.Fatalf("sign client CSR: %v", err)
	}
	leaf, err := x509.ParseCertificate(certDER)
	if err != nil {
		t.Fatal(err)
	}
	return &tls.Certificate{Certificate: [][]byte{certDER}, PrivateKey: key, Leaf: leaf},
		leaf.SerialNumber.Text(16)
}

// mtlsConnectServer starts a Connect-API listener requiring a client
// certificate, and records the serial each request presented.
func mtlsConnectServer(t *testing.T, handler http.Handler) (srv *httptest.Server, ca servertls.CA, pin string, lastSerial *atomic.Value) {
	return mtlsConnectServerHook(t, handler, nil)
}

// mtlsConnectServerHook is mtlsConnectServer plus a callback run mid-handshake,
// right after the ClientHello and BEFORE the client chooses which certificate
// to present. That ordering is what makes it a deterministic injection point
// for "a refresh lands between the capture and the handshake" — the race the
// per-request credential pin exists to close.
func mtlsConnectServerHook(t *testing.T, handler http.Handler, onClientHello func()) (srv *httptest.Server, ca servertls.CA, pin string, lastSerial *atomic.Value) {
	t.Helper()
	dir := t.TempDir()
	ca, err := servertls.LoadOrGenerateCA(filepath.Join(dir, "ca.pem"), filepath.Join(dir, "ca.key"))
	if err != nil {
		t.Fatal(err)
	}
	lastSerial = &atomic.Value{}
	lastSerial.Store("")

	recording := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.TLS != nil && len(r.TLS.PeerCertificates) > 0 {
			lastSerial.Store(r.TLS.PeerCertificates[0].SerialNumber.Text(16))
		}
		handler.ServeHTTP(w, r)
	})
	srv = httptest.NewUnstartedServer(recording)
	srv.TLS = &tls.Config{
		MinVersion: tls.VersionTLS12,
		ClientCAs:  ca.Pool(),
		ClientAuth: tls.RequireAndVerifyClientCert,
	}
	if onClientHello != nil {
		srv.TLS.GetConfigForClient = func(*tls.ClientHelloInfo) (*tls.Config, error) {
			onClientHello()
			return nil, nil // nil ⇒ keep the listener's config
		}
	}
	srv.Config.ErrorLog = log.New(io.Discard, "", 0)
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return srv, ca, servertls.Fingerprint(srv.Certificate().Raw), lastSerial
}

// TestConnectDialPresentsClientCertificate: the tunnel's raw TLS dial (which
// bypasses http.Transport entirely) must authenticate by certificate, and must
// send no bearer token while doing so.
func TestConnectDialPresentsClientCertificate(t *testing.T) {
	var sawAuth atomic.Value
	sawAuth.Store("")
	srv, ca, pin, lastSerial := mtlsConnectServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawAuth.Store(r.Header.Get("Authorization"))
		upgradeEchoHandler(t, "")(w, r)
	}))

	cert, serial := issueTunnelCert(t, ca, "tunnel-key", 24*time.Hour)
	src := clienttoken.New(clienttoken.MTLSCredential(cert, time.Now().Add(24*time.Hour)), nil)
	client := NewConnectClient(ConnectTarget{Addr: addrOf(srv), TLSPin: pin}, src)

	conn, err := client.Dial(context.Background(), "myshed", 8080)
	if err != nil {
		t.Fatalf("Dial over mtls: %v", err)
	}
	defer conn.Close()
	roundTrip(t, conn)

	if got := lastSerial.Load().(string); got != serial {
		t.Errorf("server saw certificate serial %s, want %s", got, serial)
	}
	if got := sawAuth.Load().(string); got != "" {
		t.Errorf("the tunnel sent Authorization %q in mtls mode; the credential belongs in the handshake", got)
	}
}

// TestConnectDialReMintsOnCertificateRejection: a tunnel outlives its
// certificate. The rejection arrives as a TLS error with no HTTP status at all,
// so the retry cannot be keyed on a 401 — this is the tunnel-side twin of the
// control plane's reactive path.
func TestConnectDialReMintsOnCertificateRejection(t *testing.T) {
	srv, ca, pin, lastSerial := mtlsConnectServer(t, upgradeEchoHandler(t, ""))

	expired, _ := issueTunnelCert(t, ca, "tunnel-key", time.Nanosecond)
	fresh, freshSerial := issueTunnelCert(t, ca, "tunnel-key", 24*time.Hour)

	var mints int32
	src := clienttoken.New(
		// A far-future recorded expiry, so nothing proactive fires and the TLS
		// rejection is the only thing that can drive the re-mint.
		clienttoken.MTLSCredential(expired, time.Now().Add(24*time.Hour)),
		func() (clienttoken.Credential, error) {
			atomic.AddInt32(&mints, 1)
			return clienttoken.MTLSCredential(fresh, time.Now().Add(24*time.Hour)), nil
		})
	client := NewConnectClient(ConnectTarget{Addr: addrOf(srv), TLSPin: pin}, src)

	conn, err := client.Dial(context.Background(), "myshed", 8080)
	if err != nil {
		t.Fatalf("Dial should have recovered from the expired certificate: %v", err)
	}
	defer conn.Close()
	roundTrip(t, conn)

	if got := atomic.LoadInt32(&mints); got != 1 {
		t.Errorf("mints = %d, want exactly 1", got)
	}
	if got := lastSerial.Load().(string); got != freshSerial {
		t.Errorf("the re-dial presented serial %s, want the re-minted %s", got, freshSerial)
	}
}

// TestConnectProbePresentsClientCertificate: the startup probe uses an
// http.Client rather than the raw dial, so it is a separate path that must
// authenticate identically — otherwise a healthy mtls tunnel would fail at
// startup with a misleading error.
func TestConnectProbePresentsClientCertificate(t *testing.T) {
	srv, ca, pin, lastSerial := mtlsConnectServer(t, probeStatusHandler(t, "", http.StatusBadRequest))

	cert, serial := issueTunnelCert(t, ca, "tunnel-key", 24*time.Hour)
	src := clienttoken.New(clienttoken.MTLSCredential(cert, time.Now().Add(24*time.Hour)), nil)
	client := NewConnectClient(ConnectTarget{Addr: addrOf(srv), TLSPin: pin}, src)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := client.Probe(ctx, "myshed"); err != nil {
		t.Fatalf("Probe over mtls: %v", err)
	}
	if got := lastSerial.Load().(string); got != serial {
		t.Errorf("probe presented certificate serial %s, want %s", got, serial)
	}
}

// TestConnectProbeReportsCertificateRejectionClearly: when the probe cannot
// authenticate, the operator must be told it is a credential problem, not sent
// hunting for a network fault by a generic transport error.
func TestConnectProbeReportsCertificateRejectionClearly(t *testing.T) {
	srv, ca, pin, _ := mtlsConnectServer(t, probeStatusHandler(t, "", http.StatusBadRequest))

	expired, _ := issueTunnelCert(t, ca, "tunnel-key", time.Nanosecond)
	src := clienttoken.New(clienttoken.MTLSCredential(expired, time.Now().Add(24*time.Hour)), nil)
	client := NewConnectClient(ConnectTarget{Addr: addrOf(srv), TLSPin: pin}, src)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err := client.Probe(ctx, "myshed")
	if err == nil {
		t.Fatal("Probe should fail with an expired client certificate")
	}
	if !strings.Contains(err.Error(), "client certificate") {
		t.Errorf("probe error should name the credential problem, got: %v", err)
	}
}

// TestPlainTunnelNeverPresentsACertificate: the legacy plain-TCP path has no
// verified peer, so a client certificate must never be offered on it — the same
// rule the bearer token already follows.
func TestPlainTunnelNeverPresentsACertificate(t *testing.T) {
	dir := t.TempDir()
	ca, err := servertls.LoadOrGenerateCA(filepath.Join(dir, "ca.pem"), filepath.Join(dir, "ca.key"))
	if err != nil {
		t.Fatal(err)
	}
	cert, _ := issueTunnelCert(t, ca, "tunnel-key", 24*time.Hour)
	src := clienttoken.New(clienttoken.MTLSCredential(cert, time.Now().Add(24*time.Hour)), nil)

	client := NewConnectClient(ConnectTarget{Addr: "127.0.0.1:1"}, src) // no TLSPin ⇒ plain
	cred, _ := client.capture()
	if got := client.clientCertificate(clienttoken.WithPinned(context.Background(), cred)); got != nil {
		t.Error("a client certificate was offered on an unpinned (plaintext) target")
	}
	if got := client.authToken(cred); got != "" {
		t.Errorf("authToken on a plain target = %q, want empty", got)
	}
}
