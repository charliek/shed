package clienttoken

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"fmt"
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// remoteAlert builds the error shape crypto/tls produces for an alert RECEIVED
// from the peer: a *net.OpError with Op "remote error" wrapping the rendered
// alert. The real alert type is unexported, so the test stands in a plain error
// with the identical text — which is exactly the surface the classifier reads.
func remoteAlert(desc string) error {
	return &net.OpError{Op: "remote error", Err: errors.New("tls: " + desc)}
}

func TestIsAuthFailure(t *testing.T) {
	tests := []struct {
		name   string
		status int
		err    error
		want   bool
	}{
		// --- status-driven -------------------------------------------------
		// One signal, two callers: token mode's expiry, and mtls mode's
		// per-request re-validation of a certificate the handshake accepted.
		{"401 is the token-mode and mtls per-request signal", http.StatusUnauthorized, nil, true},
		{"200 is not a failure", http.StatusOK, nil, false},
		{"403 is authenticated-but-forbidden, re-minting cannot help", http.StatusForbidden, nil, false},
		{"500 is a server fault, not an auth failure", http.StatusInternalServerError, nil, false},
		{"no status and no error", 0, nil, false},

		// --- TLS alerts that mean "your certificate is not acceptable" -----
		{"TLS 1.3 certificate_required", 0, remoteAlert("certificate required"), true},
		{"TLS 1.2 bad certificate", 0, remoteAlert("bad certificate"), true},
		{"expired certificate", 0, remoteAlert("expired certificate"), true},
		{"revoked certificate", 0, remoteAlert("revoked certificate"), true},
		{"unknown certificate", 0, remoteAlert("unknown certificate"), true},
		{"unknown certificate authority", 0, remoteAlert("unknown certificate authority"), true},
		{"unsupported certificate", 0, remoteAlert("unsupported certificate"), true},
		{"access denied", 0, remoteAlert("access denied"), true},

		// Noun-first spellings other stacks (or a future Go) may render.
		{"noun-first: certificate expired", 0, remoteAlert("certificate expired"), true},
		{"noun-first: certificate revoked", 0, remoteAlert("certificate revoked"), true},
		{"noun-first: unknown ca", 0, remoteAlert("unknown ca"), true},

		// --- TLS failures that are NOT about our credential ----------------
		// These matter as much as the positives: classifying them as auth
		// failures would make a permanent misconfiguration re-mint over SSH on
		// every request, forever, and never fix itself.
		{"handshake failure is a negotiation problem", 0, remoteAlert("handshake failure"), false},
		{"protocol version is a negotiation problem", 0, remoteAlert("protocol version not supported"), false},
		{"internal error is the server's fault", 0, remoteAlert("internal error"), false},
		{"close notify is a clean shutdown", 0, remoteAlert("close notify"), false},

		// --- transport failures --------------------------------------------
		{"connection refused", 0, &net.OpError{Op: "dial", Err: errors.New("connect: connection refused")}, false},
		{"DNS failure", 0, &net.DNSError{Err: "no such host", Name: "nope.invalid"}, false},
		{"i/o timeout", 0, &net.OpError{Op: "read", Err: errors.New("i/o timeout")}, false},
		{"EOF", 0, io.EOF, false},
		{"context deadline", 0, context.DeadlineExceeded, false},

		// A LOCAL verification failure must never trigger a re-mint: the pin is
		// wrong (or the server rotated its cert), and a new client certificate
		// cannot fix it. The word "certificate" appearing in the text is exactly
		// the trap the "remote error:" anchor exists to avoid.
		{"pin mismatch is local, not a credential refusal", 0,
			errors.New("tls: failed to verify certificate: TLS cert fingerprint mismatch"), false},
		{"x509 expiry of the SERVER cert is local", 0,
			errors.New("x509: certificate has expired or is not yet valid"), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsAuthFailure(tt.status, tt.err); got != tt.want {
				t.Errorf("IsAuthFailure(%d, %v) = %v, want %v", tt.status, tt.err, got, tt.want)
			}
		})
	}
}

// TestIsAuthFailureThroughWrappers: real errors reach the classifier through
// http.Client (*url.Error) and fmt.Errorf wrapping, sometimes losing the
// *net.OpError entirely. Both the structured and the text fallback must work.
func TestIsAuthFailureThroughWrappers(t *testing.T) {
	alert := remoteAlert("certificate required")

	t.Run("url.Error preserves the OpError", func(t *testing.T) {
		err := &url.Error{Op: "Get", URL: "https://h:8443/api/info", Err: alert}
		if !IsAuthFailure(0, err) {
			t.Error("a url.Error-wrapped alert must classify as an auth failure")
		}
	})
	t.Run("fmt.Errorf %w chain", func(t *testing.T) {
		err := fmt.Errorf("failed to connect to server: %w", fmt.Errorf("Get %q: %w", "https://h", alert))
		if !IsAuthFailure(0, err) {
			t.Error("a doubly-wrapped alert must classify as an auth failure")
		}
	})
	t.Run("text-only fallback when the OpError is flattened", func(t *testing.T) {
		// An intermediary that stringified the cause: no OpError survives, only
		// the rendered text.
		err := errors.New(`Get "https://h:8443/api/info": remote error: tls: certificate required`)
		if !IsAuthFailure(0, err) {
			t.Error("the text fallback must still recognize a flattened alert")
		}
	})
	t.Run("text fallback stays anchored on remote error", func(t *testing.T) {
		err := errors.New("local check said: tls: expired certificate")
		if IsAuthFailure(0, err) {
			t.Error("text without the remote-error anchor must not classify as an auth failure")
		}
	})
}

// --------------------------------------------------------------------------
// Live-wire leg.
//
// Everything above asserts against alert strings this test file writes down,
// which is only as good as the guess. This leg asserts against the strings Go
// ACTUALLY produces: a real TLS listener with RequireAndVerifyClientCert, dialed
// by a real client with no certificate (and with a certificate from the wrong
// CA), at both TLS 1.2 and TLS 1.3 — whose rejection timings differ, which is
// the whole reason the classifier cannot key on an HTTP status.
// --------------------------------------------------------------------------

// selfSignedCA returns a CA certificate + key usable for both signing and
// (for the server leaf) serving.
func selfSignedCA(t *testing.T) (*x509.Certificate, *ecdsa.PrivateKey) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "test-ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return cert, key
}

// signedClientCert issues a clientAuth leaf from ca. A negative ttl produces an
// already-expired certificate.
func signedClientCert(t *testing.T, ca *x509.Certificate, caKey *ecdsa.PrivateKey, ttl time.Duration) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: "client"},
		NotBefore:    time.Now().Add(-2 * time.Hour),
		NotAfter:     time.Now().Add(ttl),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca, &key.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

// mtlsListener starts a RequireAndVerifyClientCert server trusting ca, with its
// error log silenced (a refused handshake is the POINT of these tests, not a
// surprise worth 40 lines of output). It returns the server and a pool
// containing its own certificate.
func mtlsListener(t *testing.T, ca *x509.Certificate) (*httptest.Server, *x509.CertPool) {
	t.Helper()
	pool := x509.NewCertPool()
	pool.AddCert(ca)
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	srv.TLS = &tls.Config{
		MinVersion: tls.VersionTLS12,
		ClientCAs:  pool,
		ClientAuth: tls.RequireAndVerifyClientCert,
	}
	srv.Config.ErrorLog = log.New(io.Discard, "", 0)
	srv.StartTLS()
	t.Cleanup(srv.Close)

	serverPool := x509.NewCertPool()
	serverPool.AddCert(srv.Certificate())
	return srv, serverPool
}

// mtlsGet dials srv at exactly version, presenting cert (nil = none), and
// returns the error.
func mtlsGet(t *testing.T, srv *httptest.Server, serverPool *x509.CertPool, version uint16, cert *tls.Certificate) error {
	t.Helper()
	cfg := &tls.Config{RootCAs: serverPool, MinVersion: version, MaxVersion: version}
	if cert != nil {
		cfg.Certificates = []tls.Certificate{*cert}
	}
	c := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{TLSClientConfig: cfg}}
	resp, err := c.Get(srv.URL + "/")
	if err == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		t.Fatalf("expected the handshake to be refused, got HTTP %d", resp.StatusCode)
	}
	return err
}

// TestIsAuthFailureAgainstRealTLSRejection is the leg that keeps the string
// table honest. Everything it asserts is derived from a live Go TLS server, so
// if a Go release re-words an alert this fails and authAlertDescriptions gets
// updated — rather than the classifier silently degrading to "401 only" and
// every mtls client losing its ability to re-enroll.
//
// It covers both TLS versions because their rejection TIMING differs (1.2
// refuses inside the handshake, 1.3 refuses on the first read afterwards) and
// their alert CHOICE differs for the no-certificate case — the two facts that
// shaped the classifier.
func TestIsAuthFailureAgainstRealTLSRejection(t *testing.T) {
	ca, caKey := selfSignedCA(t)
	srv, serverPool := mtlsListener(t, ca)

	otherCA, otherKey := selfSignedCA(t)
	foreign := signedClientCert(t, otherCA, otherKey, time.Hour)
	expired := signedClientCert(t, ca, caKey, -time.Hour)
	// The exact value our own GetClientCertificate returns when it has nothing
	// to present. It must behave as "no certificate" and not abort locally.
	empty := tls.Certificate{}

	cases := []struct {
		name    string
		version uint16
		cert    *tls.Certificate
	}{
		// The no-certificate case, in the version our client and server
		// actually negotiate. This is the token→mtls mode-flip trigger.
		{"TLS1.3 no certificate", tls.VersionTLS13, nil},
		{"TLS1.3 empty certificate (our GetClientCertificate fallback)", tls.VersionTLS13, &empty},
		// Rejection of a certificate we DO hold — unambiguous in both versions.
		{"TLS1.2 foreign CA", tls.VersionTLS12, &foreign},
		{"TLS1.3 foreign CA", tls.VersionTLS13, &foreign},
		{"TLS1.2 expired certificate", tls.VersionTLS12, &expired},
		{"TLS1.3 expired certificate", tls.VersionTLS13, &expired},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := mtlsGet(t, srv, serverPool, tc.version, tc.cert)
			if !IsAuthFailure(0, err) {
				t.Fatalf("a real TLS rejection was NOT classified as an auth failure.\n"+
					"  err: %v\n"+
					"  The alert wording in authAlertDescriptions is out of date — update it.", err)
			}
		})
	}
}

// TestTLS12NoCertificateIsAlert40 pins the ONE documented gap in the
// classifier, so it stays a known, reasoned exclusion rather than quietly
// becoming a bug.
//
// A Go TLS 1.2 server answers "no client certificate" with the generic
// handshake_failure (alert 40), which is indistinguishable from a real
// negotiation failure — so authAlertDescriptions deliberately omits it (see the
// comment there for why that is safe: shed always negotiates 1.3, and the
// no-certificate case is caught proactively before any dial).
//
// If a future Go starts sending a certificate-specific alert here, this test
// fails — and the right response is to DELETE this test and add the 1.2
// no-certificate case to the table above, closing the gap for good.
func TestTLS12NoCertificateIsAlert40(t *testing.T) {
	ca, _ := selfSignedCA(t)
	srv, serverPool := mtlsListener(t, ca)

	err := mtlsGet(t, srv, serverPool, tls.VersionTLS12, nil)
	if !strings.Contains(err.Error(), "remote error: tls: handshake failure") {
		t.Fatalf("TLS 1.2 no-certificate no longer produces handshake_failure — it produced:\n"+
			"  %v\n"+
			"If that is now a certificate-specific alert, add it to authAlertDescriptions "+
			"and delete this test: the documented gap is closed.", err)
	}
	if IsAuthFailure(0, err) {
		t.Error("handshake_failure must not classify as an auth failure: it is also what a " +
			"genuine cipher/version negotiation failure produces, and re-minting cannot fix that")
	}
}

// TestIsAuthFailureNotTriggeredByRealNonAuthErrors is the negative twin: real,
// non-credential failures against real endpoints must not trigger a re-mint.
func TestIsAuthFailureNotTriggeredByRealNonAuthErrors(t *testing.T) {
	t.Run("connection refused", func(t *testing.T) {
		// Bind and immediately close, so the port is (almost certainly) dead.
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Skip("cannot bind a loopback port")
		}
		addr := ln.Addr().String()
		_ = ln.Close()

		c := &http.Client{Timeout: 5 * time.Second}
		resp, err := c.Get("http://" + addr + "/")
		if err == nil {
			_ = resp.Body.Close()
			t.Skip("something answered on the closed port")
		}
		if IsAuthFailure(0, err) {
			t.Errorf("connection refused must NOT trigger a re-mint: %v", err)
		}
	})

	t.Run("server cert pin mismatch", func(t *testing.T) {
		srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
		}))
		defer srv.Close()

		// Trust nothing: the client rejects the server's certificate locally.
		c := &http.Client{
			Timeout: 5 * time.Second,
			Transport: &http.Transport{TLSClientConfig: &tls.Config{
				MinVersion: tls.VersionTLS12,
				RootCAs:    x509.NewCertPool(),
			}},
		}
		resp, err := c.Get(srv.URL + "/")
		if err == nil {
			_ = resp.Body.Close()
			t.Fatal("expected the server certificate to be rejected")
		}
		if IsAuthFailure(0, err) {
			t.Errorf("a local server-cert verification failure must NOT trigger a re-mint: %v", err)
		}
	})
}
