package bootstrap

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"math/big"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/charliek/shed/sdk"
)

// TestNewClientKeyPairProducesAServerAcceptableCSR asserts the exact shape
// internal/servertls.validateClientCSR demands: standard base64, one
// well-formed CertificationRequest with no trailing bytes, a valid
// self-signature, an ECDSA P-256 key, and ECDSA-SHA256.
func TestNewClientKeyPairProducesAServerAcceptableCSR(t *testing.T) {
	kp, err := newClientKeyPair()
	if err != nil {
		t.Fatalf("newClientKeyPair: %v", err)
	}

	// (a) STANDARD base64, strict — the server decodes with
	// base64.StdEncoding.Strict() and rejects the URL-safe alphabet outright.
	der, err := base64.StdEncoding.Strict().DecodeString(kp.csrB64)
	if err != nil {
		t.Fatalf("csr is not strict standard base64: %v", err)
	}
	if strings.ContainsAny(kp.csrB64, "-_") {
		t.Error("csr uses the URL-safe base64 alphabet; the server rejects it")
	}

	// (b) exactly one DER element, nothing trailing.
	csr, err := x509.ParseCertificateRequest(der)
	if err != nil {
		t.Fatalf("parse CSR: %v", err)
	}
	// (c) proof of possession.
	if err := csr.CheckSignature(); err != nil {
		t.Errorf("CSR self-signature does not verify: %v", err)
	}
	// (d) key type.
	pub, ok := csr.PublicKey.(*ecdsa.PublicKey)
	if !ok {
		t.Fatalf("CSR public key is %T, want *ecdsa.PublicKey", csr.PublicKey)
	}
	if pub.Curve != elliptic.P256() {
		t.Errorf("CSR curve = %v, want P-256", pub.Curve.Params().Name)
	}
	// (e) signature algorithm allowlist.
	if csr.SignatureAlgorithm != x509.ECDSAWithSHA256 {
		t.Errorf("CSR signature algorithm = %v, want ECDSAWithSHA256", csr.SignatureAlgorithm)
	}

	// The key PEM must be the private half of that same public key, in the SEC 1
	// form tls.X509KeyPair accepts.
	block, rest := pem.Decode(kp.keyPEM)
	if block == nil || block.Type != "EC PRIVATE KEY" || len(strings.TrimSpace(string(rest))) != 0 {
		t.Fatalf("key PEM is not a single EC PRIVATE KEY block: %q", kp.keyPEM)
	}
	key, err := x509.ParseECPrivateKey(block.Bytes)
	if err != nil {
		t.Fatalf("parse key PEM: %v", err)
	}
	if !key.PublicKey.Equal(pub) {
		t.Error("the key PEM does not match the CSR's public key")
	}
}

// TestNewClientKeyPairIsFreshEachTime: enrollment must never reuse a key.
func TestNewClientKeyPairIsFreshEachTime(t *testing.T) {
	a, err := newClientKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	b, err := newClientKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	if a.csrB64 == b.csrB64 {
		t.Error("two enrollments produced the same CSR")
	}
	if a.key.Equal(b.key) {
		t.Error("two enrollments produced the same private key")
	}
}

// issueFor signs a certificate for pub, standing in for the server's CA.
func issueFor(t *testing.T, pub any) []byte {
	t.Helper()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(7),
		Subject:      pkix.Name{CommonName: "SHA256:whatever"},
		NotBefore:    time.Now().Add(-time.Minute),
		NotAfter:     time.Now().Add(time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, pub, caKey)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

// TestMatchesIssuedCert: the returned certificate must certify the key we
// generated. A mismatch here would otherwise surface much later as an opaque
// "bad certificate" from a server that looks broken.
func TestMatchesIssuedCert(t *testing.T) {
	kp, err := newClientKeyPair()
	if err != nil {
		t.Fatal(err)
	}

	t.Run("matching pair accepted", func(t *testing.T) {
		if err := kp.matchesIssuedCert(issueFor(t, kp.key.Public())); err != nil {
			t.Errorf("a certificate for our own key must be accepted: %v", err)
		}
	})
	t.Run("certificate for someone else's key rejected", func(t *testing.T) {
		other, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		if err := kp.matchesIssuedCert(issueFor(t, other.Public())); !errors.Is(err, errCertKeyMismatch) {
			t.Errorf("err = %v, want errCertKeyMismatch", err)
		}
	})
	t.Run("non-PEM rejected", func(t *testing.T) {
		if err := kp.matchesIssuedCert([]byte("not a pem")); !errors.Is(err, errNoCertificatePEM) {
			t.Errorf("err = %v, want errNoCertificatePEM", err)
		}
	})
	t.Run("empty rejected", func(t *testing.T) {
		if err := kp.matchesIssuedCert(nil); !errors.Is(err, errNoCertificatePEM) {
			t.Errorf("err = %v, want errNoCertificatePEM", err)
		}
	})
}

// TestValidateCSRArg: the CSR rides in ONE argv element that the server splits
// on the first "=" and then base64-decodes. Whitespace would split it into two
// request arguments (the second read as a client kind); a NUL would truncate it
// at exec. Neither can occur in standard base64, so anything outside that
// alphabet must be refused before reaching exec.
func TestValidateCSRArg(t *testing.T) {
	valid, err := newClientKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name    string
		csr     string
		wantErr bool
	}{
		{"empty (no CSR requested)", "", false},
		{"a real generated CSR", valid.csrB64, false},
		{"padding is allowed", "QUJD==", false},
		{"space splits the argv element", "QUJ D", true},
		{"tab", "QUJ\tD", true},
		{"newline", "QUJ\nD", true},
		{"carriage return", "QUJ\rD", true},
		{"NUL truncates at exec", "QUJ\x00D", true},
		{"URL-safe alphabet is not what the server decodes", "QU-_", true},
		{"a leading dash would look like an ssh option", "-oProxyCommand=x", true},
		{"shell metacharacters", "QUJ;rm -rf /", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateCSRArg(tt.csr)
			if (err != nil) != tt.wantErr {
				t.Errorf("validateCSRArg(%q) err = %v, wantErr %v", tt.csr, err, tt.wantErr)
			}
			// Whatever the verdict, the value itself must not be echoed back.
			if err != nil && strings.Contains(err.Error(), tt.csr) {
				t.Errorf("the error echoed the csr value: %v", err)
			}
		})
	}
}

// TestValidateRejectsBadCSRThroughParams proves the CSR check is on the same
// path as every other argv element — i.e. Run/RunCredential cannot bypass it.
func TestValidateRejectsBadCSRThroughParams(t *testing.T) {
	p := Params{Host: "h", Port: 22, KnownHostsPath: "/kh", Scope: "control", CSRBase64: "bad value"}
	if err := validate(p); err == nil {
		t.Error("validate must reject a CSR that is not a single base64 token")
	}
}

// TestSSHArgsCarryCSR pins the request line the server parses:
// "<scope> [<kind>] [csr=<base64>]", with the CSR order-independent after the
// scope and absent entirely when none was generated.
func TestSSHArgsCarryCSR(t *testing.T) {
	base := Params{Host: "h", Port: 2222, KnownHostsPath: "/kh", Scope: "control"}

	t.Run("no csr", func(t *testing.T) {
		args := sshArgs(base)
		for _, a := range args {
			if strings.HasPrefix(a, "csr=") {
				t.Errorf("csr argument present when none was requested: %v", args)
			}
		}
	})

	t.Run("with kind and csr", func(t *testing.T) {
		p := base
		p.ClientKind = "cli"
		p.CSRBase64 = "QUJD"
		args := sshArgs(p)
		// Positional contract: host, then scope, then kind, then csr.
		i := slices.Index(args, "h")
		if i < 0 || len(args) != i+4 {
			t.Fatalf("unexpected tail shape: %v", args)
		}
		if got := args[i+1 : i+4]; !slices.Equal(got, []string{"control", "cli", "csr=QUJD"}) {
			t.Errorf("request line = %v, want [control cli csr=QUJD]", got)
		}
	})

	t.Run("csr without a kind is a valid line", func(t *testing.T) {
		p := base
		p.CSRBase64 = "QUJD"
		args := sshArgs(p)
		i := slices.Index(args, "h")
		if got := args[i+1:]; !slices.Equal(got, []string{"control", "csr=QUJD"}) {
			t.Errorf("request line = %v, want [control csr=QUJD]", got)
		}
	})

	t.Run("the csr is one argv element", func(t *testing.T) {
		kp, err := newClientKeyPair()
		if err != nil {
			t.Fatal(err)
		}
		p := base
		p.CSRBase64 = kp.csrB64
		count := 0
		for _, a := range sshArgs(p) {
			if strings.HasPrefix(a, "csr=") {
				count++
				if strings.ContainsAny(a, " \t\r\n\x00") {
					t.Error("the csr argv element contains whitespace; the server would parse it as two arguments")
				}
			}
		}
		if count != 1 {
			t.Errorf("csr appears %d times, want exactly 1 (the server rejects duplicates)", count)
		}
	})
}

// Run's own CSR-clearing is asserted in integration_test.go
// (TestRunNeverSendsACSRButRunCredentialDoes), against a real ssh client and a
// real server that records the request line it received.
//
// It deliberately does NOT live here. The obvious unit test — clear CSRBase64
// by hand, then inspect sshArgs — asserts only that sshArgs relays what it is
// given, which TestSSHArgsCarryCSR already covers. It would keep passing if Run
// stopped clearing the field, which is the single thing it exists to protect.

// TestDecodeBundleModeDispatch covers the validation ladder each mode gets, and
// most importantly that a LEGACY bundle (no auth_mode) still takes the token
// path exactly as it always did.
func TestDecodeBundleModeDispatch(t *testing.T) {
	const legacy = `{"http_port":8080,"https_port":8443,"tls_cert_fingerprint":"sha256:a","token":"t","scope":"control","token_id":"i","expires_at":"2030-01-02T03:04:05Z"}`
	const mtls = `{"auth_mode":"mtls","https_port":8443,"tls_cert_fingerprint":"sha256:a","client_cert":"-----BEGIN CERTIFICATE-----\nX\n-----END CERTIFICATE-----\n","scope":"control","cert_serial":"1a","expires_at":"2030-01-02T03:04:05Z"}`

	tests := []struct {
		name     string
		json     string
		wantErr  string
		wantMode string
	}{
		{"legacy token bundle (no auth_mode)", legacy, "", sdk.AuthModeToken},
		{"explicit token bundle", `{"auth_mode":"token",` + legacy[1:], "", sdk.AuthModeToken},
		{"mtls bundle", mtls, "", sdk.AuthModeMTLS},

		// Token-mode ladder, unchanged from before mtls existed.
		{"token bundle with no token", `{"https_port":8443,"tls_cert_fingerprint":"sha256:a","scope":"control"}`, "empty token", ""},
		{"token bundle with no port", `{"token":"t","scope":"control"}`, "no usable API port", ""},
		{"token bundle with https but no pin", `{"https_port":8443,"token":"t","scope":"control"}`, "without a TLS fingerprint", ""},

		// mtls ladder: strictly tighter, because the certificate IS the
		// credential and mtls exists only on the TLS listener.
		{"mtls bundle with no certificate", `{"auth_mode":"mtls","https_port":8443,"tls_cert_fingerprint":"sha256:a","scope":"control"}`, "no client certificate", ""},
		{"mtls bundle with no https port", `{"auth_mode":"mtls","client_cert":"x","tls_cert_fingerprint":"sha256:a","scope":"control"}`, "no HTTPS port", ""},
		{"mtls bundle with no pin", `{"auth_mode":"mtls","https_port":8443,"client_cert":"x","scope":"control"}`, "without a TLS fingerprint", ""},
		{"mtls bundle carrying a bearer token", `{"auth_mode":"mtls","https_port":8443,"tls_cert_fingerprint":"sha256:a","client_cert":"x","token":"t","scope":"control"}`, "unexpectedly carries a bearer token", ""},

		// Shared framing rules.
		{"trailing garbage", legacy + `{"more":1}`, "trailing data", ""},
		{"not json", `nope`, "no valid bootstrap bundle", ""},
		{"scope mismatch", `{"https_port":8443,"tls_cert_fingerprint":"sha256:a","token":"t","scope":"credentials"}`, "scope mismatch", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b, err := decodeBundle([]byte(tt.json), "control")
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want one containing %q", err, tt.wantErr)
				}
				// A decode error must never quote stdout — it carries the credential.
				if strings.Contains(err.Error(), "BEGIN CERTIFICATE") || strings.Contains(err.Error(), `"t"`) {
					t.Errorf("the error leaked bundle contents: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("decodeBundle: %v", err)
			}
			if b.Mode() != tt.wantMode {
				t.Errorf("Mode() = %q, want %q", b.Mode(), tt.wantMode)
			}
		})
	}
}
