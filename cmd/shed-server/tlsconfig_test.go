package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"path/filepath"
	"testing"
	"time"

	"github.com/charliek/shed/internal/config"
	"github.com/charliek/shed/internal/servertls"
)

// testClientFingerprint is a canonically-shaped OpenSSH SHA-256 fingerprint.
// The CA validates the shape at issuance, so a test identity has to look like a
// real one rather than a readable placeholder.
const testClientFingerprint = "SHA256:1U6JLBiKdyPFRj5o2Vv5uF3d8Kk0Y0mYFDDaC4mHtCk"

// signTestClient runs the real enrollment path (P-256 CSR -> CA.SignClientCSR)
// and returns the issued leaf.
func signTestClient(t *testing.T, ca servertls.CA) *x509.Certificate {
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
	der, err := ca.SignClientCSR(csrDER, testClientFingerprint, "control", "cli", time.Hour)
	if err != nil {
		t.Fatalf("sign client CSR: %v", err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("parse issued cert: %v", err)
	}
	return leaf
}

// TestBuildHTTPSTLSConfig asserts on the REAL production assembly — the same
// function runServe hands to the HTTPS listener. That is the point: a
// middleware-level test in internal/api would still pass if someone deleted the
// ClientAuth lines here, because a hand-built tls.Config in a test would keep
// mirroring the intent long after production stopped honoring it. This test
// fails the moment the listener stops requiring client certificates in mtls
// mode, stops pinning trust to the internal CA, or grows client-auth in a mode
// that must not have it.
func TestBuildHTTPSTLSConfig(t *testing.T) {
	dir := t.TempDir()
	serverCert, _, err := servertls.LoadOrGenerate(
		filepath.Join(dir, "tls.pem"), filepath.Join(dir, "tls.key"), nil)
	if err != nil {
		t.Fatalf("generate server cert: %v", err)
	}
	ca, err := servertls.LoadOrGenerateCA(
		filepath.Join(dir, "ca.pem"), filepath.Join(dir, "ca.key"))
	if err != nil {
		t.Fatalf("generate CA: %v", err)
	}
	// A second, unrelated CA: the mtls pool must NOT accept it.
	otherDir := t.TempDir()
	otherCA, err := servertls.LoadOrGenerateCA(
		filepath.Join(otherDir, "ca.pem"), filepath.Join(otherDir, "ca.key"))
	if err != nil {
		t.Fatalf("generate second CA: %v", err)
	}

	t.Run("mtls mode requires and verifies client certificates", func(t *testing.T) {
		cfg := &config.ServerConfig{Auth: &config.AuthConfig{Mode: config.AuthModeMTLS}}
		got := buildHTTPSTLSConfig(cfg, serverCert, &ca, func(string) bool { return true })

		if got.ClientAuth != tls.RequireAndVerifyClientCert {
			t.Errorf("ClientAuth = %v, want RequireAndVerifyClientCert — "+
				"the HTTPS listener is not requiring a client certificate in mtls mode", got.ClientAuth)
		}
		if got.ClientCAs == nil {
			t.Fatal("ClientCAs is nil in mtls mode — any client CA would be trusted")
		}
		if got.VerifyConnection == nil {
			t.Error("VerifyConnection is nil in mtls mode — the live-allowlist handshake check is not wired")
		}
		if got.MinVersion != tls.VersionTLS12 {
			t.Errorf("MinVersion = %#x, want TLS 1.2", got.MinVersion)
		}

		// The pool contains ONLY the internal CA. Verifying a leaf issued by an
		// unrelated CA against it must fail, and one issued by the internal CA
		// must succeed — a stronger statement than "non-nil".
		mine := signTestClient(t, ca)
		if _, err := mine.Verify(x509.VerifyOptions{Roots: got.ClientCAs, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}); err != nil {
			t.Errorf("internal-CA client cert does not verify against ClientCAs: %v", err)
		}
		foreign := signTestClient(t, otherCA)
		if _, err := foreign.Verify(x509.VerifyOptions{Roots: got.ClientCAs, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}); err == nil {
			t.Error("a foreign CA's client cert verified against ClientCAs — the pool is not restricted to the internal CA")
		}
	})

	t.Run("mtls verifier consults the live allowlist", func(t *testing.T) {
		cfg := &config.ServerConfig{Auth: &config.AuthConfig{Mode: config.AuthModeMTLS}}
		allow := false
		got := buildHTTPSTLSConfig(cfg, serverCert, &ca, func(string) bool { return allow })
		state := tls.ConnectionState{PeerCertificates: []*x509.Certificate{signTestClient(t, ca)}}

		if err := got.VerifyConnection(state); err == nil {
			t.Error("de-authorized identity passed the handshake verifier")
		}
		allow = true
		if err := got.VerifyConnection(state); err != nil {
			t.Errorf("authorized identity rejected by the handshake verifier: %v", err)
		}
	})

	t.Run("token mode has no client auth", func(t *testing.T) {
		cfg := &config.ServerConfig{Auth: &config.AuthConfig{Mode: config.AuthModeToken}}
		// Pass a CA anyway: mode, not the argument, is what decides.
		got := buildHTTPSTLSConfig(cfg, serverCert, &ca, func(string) bool { return true })

		if got.ClientAuth != tls.NoClientCert {
			t.Errorf("ClientAuth = %v, want NoClientCert in token mode", got.ClientAuth)
		}
		if got.ClientCAs != nil {
			t.Error("ClientCAs set in token mode")
		}
		if got.VerifyConnection != nil {
			t.Error("VerifyConnection set in token mode")
		}
		if got.MinVersion != tls.VersionTLS12 {
			t.Errorf("MinVersion = %#x, want TLS 1.2", got.MinVersion)
		}
	})

	t.Run("open mode has no client auth", func(t *testing.T) {
		cfg := &config.ServerConfig{}
		got := buildHTTPSTLSConfig(cfg, serverCert, nil, nil)
		if got.ClientAuth != tls.NoClientCert || got.ClientCAs != nil || got.VerifyConnection != nil {
			t.Errorf("open mode grew client auth: ClientAuth=%v ClientCAs=%v VerifyConnection=%v",
				got.ClientAuth, got.ClientCAs != nil, got.VerifyConnection != nil)
		}
		if got.MinVersion != tls.VersionTLS12 {
			t.Errorf("MinVersion = %#x, want TLS 1.2", got.MinVersion)
		}
	})
}
