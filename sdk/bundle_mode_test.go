package sdk

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestBundleModeAbsentMeansToken is the single most important compatibility
// rule in the client-side mtls work: every shed-server built before client
// certificates existed emits no auth_mode key at all, and every one of those is
// a token/open server. An empty AuthMode must therefore decode as the bearer
// shape — never as "unknown", and never as mtls.
func TestBundleModeAbsentMeansToken(t *testing.T) {
	tests := []struct {
		name     string
		authMode string
		want     string
	}{
		{"absent (pre-mtls server)", "", AuthModeToken},
		{"explicit token", AuthModeToken, AuthModeToken},
		{"mtls", AuthModeMTLS, AuthModeMTLS},
		// An unrecognized future mode falls through to token rather than mtls:
		// the mtls branch is the one that requires a certificate, so guessing
		// wrong that way fails more confusingly than falling back to the shape
		// whose fields are actually populated.
		{"unknown future mode", "quantum", AuthModeToken},
		{"case does not fuzzy-match", "MTLS", AuthModeToken},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := (Bundle{AuthMode: tt.authMode}).Mode(); got != tt.want {
				t.Errorf("Mode() = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestLegacyTokenBundleDecodesUnchanged pins the exact bytes a pre-mtls
// shed-server emits. If this ever fails, every deployed older server has become
// unreachable from a current client.
func TestLegacyTokenBundleDecodesUnchanged(t *testing.T) {
	const legacy = `{"http_port":8080,"https_port":8443,"tls_cert_fingerprint":"sha256:abc",` +
		`"token":"shed_ctl_xyz","scope":"control","token_id":"tid-1","expires_at":"2030-01-02T03:04:05Z"}`

	var b Bundle
	if err := json.Unmarshal([]byte(legacy), &b); err != nil {
		t.Fatalf("legacy bundle must still decode: %v", err)
	}
	if b.Mode() != AuthModeToken {
		t.Errorf("Mode() = %q, want token (no auth_mode key present)", b.Mode())
	}
	if b.AuthMode != "" {
		t.Errorf("AuthMode = %q, want empty (the key was absent on the wire)", b.AuthMode)
	}
	if b.Token != "shed_ctl_xyz" || b.TokenID != "tid-1" || b.Scope != "control" {
		t.Errorf("credential fields lost: %+v", b)
	}
	if b.HTTPPort != 8080 || b.HTTPSPort != 8443 || b.TLSCertFingerprint != "sha256:abc" {
		t.Errorf("endpoint fields lost: %+v", b)
	}
	if b.ExpiresAt.IsZero() {
		t.Error("expires_at lost")
	}
	// Nothing from the mtls shape may materialize out of a token bundle.
	if b.ClientCert != "" || b.CertSerial != "" {
		t.Errorf("a token bundle must carry no certificate fields: %+v", b)
	}
}

// TestMTLSBundleDecodes pins the mtls wire shape emitted by
// internal/sshd/bootstrap.go's mintClientCert.
func TestMTLSBundleDecodes(t *testing.T) {
	const wire = `{"auth_mode":"mtls","https_port":8443,"tls_cert_fingerprint":"sha256:abc",` +
		`"client_cert":"-----BEGIN CERTIFICATE-----\nMIIB\n-----END CERTIFICATE-----\n",` +
		`"scope":"control","cert_serial":"1a2b3c","expires_at":"2030-01-02T03:04:05Z"}`

	var b Bundle
	if err := json.Unmarshal([]byte(wire), &b); err != nil {
		t.Fatalf("mtls bundle must decode: %v", err)
	}
	if b.Mode() != AuthModeMTLS {
		t.Errorf("Mode() = %q, want mtls", b.Mode())
	}
	if !strings.Contains(b.ClientCert, "BEGIN CERTIFICATE") {
		t.Errorf("client_cert not decoded: %q", b.ClientCert)
	}
	if b.CertSerial != "1a2b3c" || b.Scope != "control" || b.HTTPSPort != 8443 {
		t.Errorf("mtls fields lost: %+v", b)
	}
	// An mtls bundle carries no bearer token and no plain-HTTP port to point at.
	if b.Token != "" || b.TokenID != "" || b.HTTPPort != 0 {
		t.Errorf("an mtls bundle must carry no token/http_port: %+v", b)
	}
}

// TestTokenBundleMarshalsWithoutMTLSKeys: the omitempty tags must keep a token
// bundle from sprouting empty mtls placeholders, so a round-trip through this
// type stays byte-compatible with what older decoders expect.
func TestTokenBundleMarshalsWithoutMTLSKeys(t *testing.T) {
	b := Bundle{
		AuthMode: AuthModeToken, HTTPPort: 8080, HTTPSPort: 8443,
		TLSCertFingerprint: "sha256:abc", Token: "t", Scope: "control", TokenID: "id",
	}
	data, err := json.Marshal(b)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"client_cert", "cert_serial"} {
		if strings.Contains(string(data), key) {
			t.Errorf("token bundle marshalled with %q: %s", key, data)
		}
	}
}

// TestBundleStringRedactsSecrets: a Bundle reaching a log line through %v must
// not print the credential. The Stringer makes that structural rather than a
// rule every call site has to remember.
func TestBundleStringRedactsSecrets(t *testing.T) {
	t.Run("token", func(t *testing.T) {
		s := Bundle{AuthMode: AuthModeToken, Token: "shed_ctl_SUPERSECRET", Scope: "control"}.String()
		if strings.Contains(s, "SUPERSECRET") {
			t.Errorf("String() leaked the bearer token: %s", s)
		}
		if !strings.Contains(s, "control") {
			t.Errorf("String() should still be useful for diagnosis: %s", s)
		}
	})
	t.Run("mtls", func(t *testing.T) {
		b := Bundle{AuthMode: AuthModeMTLS, ClientCert: "-----BEGIN CERTIFICATE-----SECRETISH", CertSerial: "1a2b"}
		s := b.String()
		if strings.Contains(s, "SECRETISH") {
			t.Errorf("String() leaked the certificate body: %s", s)
		}
		if !strings.Contains(s, "1a2b") {
			t.Errorf("String() should name the serial for correlation: %s", s)
		}
	})
	t.Run("credential wraps the bundle and hides the key", func(t *testing.T) {
		c := Credential{
			Bundle: Bundle{AuthMode: AuthModeMTLS, ClientCert: "CERTBODY"},
			KeyPEM: []byte("-----BEGIN EC PRIVATE KEY-----PRIVATEBITS"),
		}
		s := c.String()
		if strings.Contains(s, "PRIVATEBITS") || strings.Contains(s, "CERTBODY") {
			t.Errorf("Credential.String() leaked key material: %s", s)
		}
		if c.Mode() != AuthModeMTLS {
			t.Errorf("Credential.Mode() = %q, want mtls", c.Mode())
		}
	})
}
