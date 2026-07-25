package sdk

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"math/big"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// pinOf renders the fingerprint form WithTLSPin expects for a test server.
func pinOf(srv *httptest.Server) string {
	sum := sha256.Sum256(srv.Certificate().Raw)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// staticTokenProvider is a TokenProvider that always yields the same token, so
// a test can prove the token path is reachable before proving it is suppressed.
type staticTokenProvider struct{ token string }

func (s staticTokenProvider) Token() (string, error) { return s.token, nil }
func (s staticTokenProvider) Invalidate()            {}

// TestClientCertificatesSuppressTheBearerToken.
//
// WithClientCertificates and the token options are independent knobs, so an
// external caller can set both — from a config that carries a leftover token, or
// simply by wiring every option it knows about. Sending both would put a live
// bearer token on the wire to an endpoint that never reads it: an mtls-mode
// server authenticates from the handshake and ignores the header entirely. The
// certificate wins, and the header is not sent at all.
func TestClientCertificatesSuppressTheBearerToken(t *testing.T) {
	var sawAuth atomic.Value
	sawAuth.Store("")
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawAuth.Store(r.Header.Get("Authorization"))
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	pin := pinOf(srv)

	probe := func(t *testing.T, opts ...HostClientOption) string {
		t.Helper()
		sawAuth.Store("")
		c := NewHostClient(append([]HostClientOption{
			WithServerURL(srv.URL), WithTLSPin(pin),
		}, opts...)...)
		if err := c.Respond(context.Background(), "ns", &Envelope{}); err != nil {
			t.Fatalf("Respond: %v", err)
		}
		return sawAuth.Load().(string)
	}

	// Baseline: without a certificate provider, the token IS sent. Without this
	// the suppression assertions below could pass for the wrong reason.
	t.Run("a token alone is sent", func(t *testing.T) {
		if got := probe(t, WithToken("shed_creds_abc")); got != "Bearer shed_creds_abc" {
			t.Errorf("Authorization = %q, want the bearer token", got)
		}
	})
	t.Run("a token provider alone is sent", func(t *testing.T) {
		if got := probe(t, WithTokenProvider(staticTokenProvider{"shed_creds_xyz"})); got != "Bearer shed_creds_xyz" {
			t.Errorf("Authorization = %q, want the provider's token", got)
		}
	})

	// Suppression is keyed on the provider actually HOLDING a certificate, not on
	// it being wired. An adaptive client wires the provider unconditionally so a
	// server flipping between token and mtls needs no new transport; gating on
	// the wiring would mean such a client never authenticates against a
	// token-mode server at all.
	held := selfSignedClientCert(t)
	certs := ClientCertFunc(func() *tls.Certificate { return held })
	empty := ClientCertFunc(func() *tls.Certificate { return nil })

	t.Run("a static token is suppressed while a certificate is held", func(t *testing.T) {
		if got := probe(t, WithToken("shed_creds_abc"), WithClientCertificates(certs)); got != "" {
			t.Errorf("Authorization = %q, want none: the certificate is the credential", got)
		}
	})
	t.Run("a token provider is suppressed while a certificate is held", func(t *testing.T) {
		if got := probe(t, WithTokenProvider(staticTokenProvider{"shed_creds_xyz"}), WithClientCertificates(certs)); got != "" {
			t.Errorf("Authorization = %q, want none: the certificate is the credential", got)
		}
	})
	// Option order must not matter — applyTLSPin runs after all options.
	t.Run("option order does not matter", func(t *testing.T) {
		if got := probe(t, WithClientCertificates(certs), WithToken("shed_creds_abc")); got != "" {
			t.Errorf("Authorization = %q, want none", got)
		}
	})

	// The token half of the same knob: an empty provider is a client in TOKEN
	// state, and its token must travel. This is the assertion that makes one
	// HostClient usable against a server in either mode.
	t.Run("a static token is sent while the provider holds nothing", func(t *testing.T) {
		if got := probe(t, WithToken("shed_creds_abc"), WithClientCertificates(empty)); got != "Bearer shed_creds_abc" {
			t.Errorf("Authorization = %q, want the token: an empty provider is token state, not mtls", got)
		}
	})
	t.Run("a token provider is sent while the cert provider holds nothing", func(t *testing.T) {
		if got := probe(t, WithTokenProvider(staticTokenProvider{"shed_creds_xyz"}), WithClientCertificates(empty)); got != "Bearer shed_creds_xyz" {
			t.Errorf("Authorization = %q, want the provider's token", got)
		}
	})

	// A live flip, on ONE client: the same instance stops sending the token the
	// moment the provider starts holding a certificate, and resumes when it stops
	// — with no rebuild of the client or its transport.
	t.Run("a mode flip changes the header on a live client", func(t *testing.T) {
		var current atomic.Pointer[tls.Certificate]
		flipping := ClientCertFunc(func() *tls.Certificate { return current.Load() })
		c := NewHostClient(
			WithServerURL(srv.URL), WithTLSPin(pin),
			WithToken("shed_creds_abc"), WithClientCertificates(flipping),
		)
		send := func() string {
			sawAuth.Store("")
			if err := c.Respond(context.Background(), "ns", &Envelope{}); err != nil {
				t.Fatalf("Respond: %v", err)
			}
			return sawAuth.Load().(string)
		}
		if got := send(); got != "Bearer shed_creds_abc" {
			t.Fatalf("token state: Authorization = %q, want the token", got)
		}
		current.Store(held)
		if got := send(); got != "" {
			t.Fatalf("mtls state: Authorization = %q, want none", got)
		}
		current.Store(nil)
		if got := send(); got != "Bearer shed_creds_abc" {
			t.Fatalf("back to token state: Authorization = %q, want the token", got)
		}
	})
}

// TestClientCertificatesWithoutAPinDoNotSuppressTheToken: the provider is only
// WIRED when a pin is set (mtls is served only over the pinned HTTPS listener).
// Suppressing the token for a provider that was never installed would leave the
// client with no credential at all — a silent downgrade to unauthenticated
// rather than a mode change.
func TestClientCertificatesWithoutAPinDoNotSuppressTheToken(t *testing.T) {
	var sawAuth atomic.Value
	sawAuth.Store("")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawAuth.Store(r.Header.Get("Authorization"))
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	c := NewHostClient(
		WithServerURL(srv.URL), // plain http, no pin ⇒ the provider is never installed
		WithToken("shed_creds_abc"),
		WithClientCertificates(ClientCertFunc(func() *tls.Certificate { return nil })),
	)
	if err := c.Respond(context.Background(), "ns", &Envelope{}); err != nil {
		t.Fatalf("Respond: %v", err)
	}
	if got := sawAuth.Load().(string); got != "Bearer shed_creds_abc" {
		t.Errorf("Authorization = %q, want the token: no pin means no certificate was ever wired", got)
	}
}

// TestClientCertificateIsPresentedToAnMTLSServer: the other half of the
// exchange — suppressing the header is only correct if the certificate actually
// authenticates. Asserted from the server's side.
func TestClientCertificateIsPresentedToAnMTLSServer(t *testing.T) {
	cert := selfSignedClientCert(t)

	var sawPeer atomic.Value
	sawPeer.Store("")
	var sawAuth atomic.Value
	sawAuth.Store("")
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawAuth.Store(r.Header.Get("Authorization"))
		if r.TLS != nil && len(r.TLS.PeerCertificates) > 0 {
			sawPeer.Store(r.TLS.PeerCertificates[0].Subject.CommonName)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	srv.TLS = &tls.Config{MinVersion: tls.VersionTLS12, ClientAuth: tls.RequestClientCert}
	srv.StartTLS()
	defer srv.Close()

	c := NewHostClient(
		WithServerURL(srv.URL), WithTLSPin(pinOf(srv)),
		WithToken("shed_creds_abc"),
		WithClientCertificates(ClientCertFunc(func() *tls.Certificate { return cert })),
	)
	if err := c.Respond(context.Background(), "ns", &Envelope{}); err != nil {
		t.Fatalf("Respond: %v", err)
	}
	if got := sawPeer.Load().(string); got != "host-agent" {
		t.Errorf("server saw peer CN %q, want host-agent", got)
	}
	if got := sawAuth.Load().(string); got != "" {
		t.Errorf("the client sent Authorization %q alongside its certificate", got)
	}
}

// selfSignedClientCert builds a minimal client certificate for the assertion
// above; the test server only records what was presented, it does not verify.
func selfSignedClientCert(t *testing.T) *tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "host-agent"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, key.Public(), key)
	if err != nil {
		t.Fatal(err)
	}
	return &tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}
