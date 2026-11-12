package main

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/charliek/shed/internal/config"
	"github.com/charliek/shed/internal/servertls"
)

func clientTestConfig(t *testing.T) *config.ServerConfig {
	t.Helper()
	return &config.ServerConfig{
		Name:      "test",
		HTTPSPort: 18443,
		StateDir:  t.TempDir(),
		Auth: &config.AuthConfig{
			Mode: config.AuthModeMTLS,
			Clients: map[string]config.ServiceClientConfig{
				"nocturn": {
					CertMaxTTL: config.Duration(48 * time.Hour),
					Policy:     config.ServiceClientPolicy{NamePrefix: "nocturn-", MaxSheds: 2, MaxAge: config.Duration(30 * time.Hour)},
				},
			},
		},
	}
}

func TestIssueClientBundle(t *testing.T) {
	cfg := clientTestConfig(t)
	start := time.Now()
	b, err := issueClientBundle(cfg, "nocturn", 0)
	if err != nil {
		t.Fatalf("issueClientBundle: %v", err)
	}
	if b.Client != "nocturn" || b.AuthMode != "mtls" || b.HTTPSPort != 18443 {
		t.Errorf("bundle header = %+v", b)
	}

	// The cert verifies against the server's CA, in the same state dir.
	caCertPath, caKeyPath := caCertPaths(cfg.StateDir)
	ca, err := servertls.LoadOrGenerateCA(caCertPath, caKeyPath)
	if err != nil {
		t.Fatalf("load CA: %v", err)
	}
	certBlock, _ := pem.Decode([]byte(b.ClientCert))
	if certBlock == nil || certBlock.Type != "CERTIFICATE" {
		t.Fatalf("client_cert is not a CERTIFICATE PEM: %q", b.ClientCert)
	}
	leaf, err := x509.ParseCertificate(certBlock.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := leaf.Verify(x509.VerifyOptions{Roots: ca.Pool(), KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}); err != nil {
		t.Errorf("issued cert does not verify against the CA: %v", err)
	}
	if name, isService, err := servertls.ServiceClientName(leaf); name != "nocturn" || !isService || err != nil {
		t.Errorf("ServiceClientName = %q %v %v", name, isService, err)
	}

	// The key is PKCS#8 and matches the certificate.
	keyBlock, _ := pem.Decode([]byte(b.ClientKey))
	if keyBlock == nil || keyBlock.Type != "PRIVATE KEY" {
		t.Fatalf("client_key is not a PRIVATE KEY PEM")
	}
	key, err := x509.ParsePKCS8PrivateKey(keyBlock.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	ec, ok := key.(*ecdsa.PrivateKey)
	if !ok || !ec.PublicKey.Equal(leaf.PublicKey) {
		t.Error("client_key does not match the certificate's public key")
	}

	// The pin is the TLS certificate serve presents.
	certPath, keyPath := tlsCertPaths(cfg, cfg.StateDir)
	_, tlsDER, err := servertls.LoadOrGenerate(certPath, keyPath, cfg.TLSNames)
	if err != nil {
		t.Fatal(err)
	}
	if b.TLSCertFingerprint != servertls.Fingerprint(tlsDER) {
		t.Errorf("tls_cert_fingerprint = %s, want %s", b.TLSCertFingerprint, servertls.Fingerprint(tlsDER))
	}

	if b.CertSerial != strings.ToLower(leaf.SerialNumber.Text(16)) {
		t.Errorf("cert_serial = %s, want %s", b.CertSerial, leaf.SerialNumber.Text(16))
	}
	exp, err := time.Parse(time.RFC3339, b.ExpiresAt)
	if err != nil {
		t.Fatalf("expires_at %q: %v", b.ExpiresAt, err)
	}
	if !strings.HasSuffix(b.ExpiresAt, "Z") {
		t.Errorf("expires_at %q is not UTC", b.ExpiresAt)
	}
	// Default ttl is cert_max_ttl (48h here).
	if d := exp.Sub(start); d < 47*time.Hour || d > 49*time.Hour {
		t.Errorf("default ttl gave expiry %s after issuance, want about 48h", d)
	}

	// The JSON has exactly the contract's keys.
	raw, _ := json.Marshal(b)
	var m map[string]any
	_ = json.Unmarshal(raw, &m)
	for _, k := range []string{"client", "auth_mode", "https_port", "tls_cert_fingerprint", "client_cert", "client_key", "cert_serial", "expires_at"} {
		if _, ok := m[k]; !ok {
			t.Errorf("bundle JSON lacks %q", k)
		}
	}
	if len(m) != 8 {
		t.Errorf("bundle JSON has %d keys, want 8: %v", len(m), m)
	}
}

func TestIssueClientBundleTTL(t *testing.T) {
	cfg := clientTestConfig(t)
	b, err := issueClientBundle(cfg, "nocturn", time.Hour)
	if err != nil {
		t.Fatalf("1h: %v", err)
	}
	exp, _ := time.Parse(time.RFC3339, b.ExpiresAt)
	if d := time.Until(exp); d > 61*time.Minute || d < 58*time.Minute {
		t.Errorf("--ttl 1h expires in %s", d)
	}
	if _, err := issueClientBundle(cfg, "nocturn", 49*time.Hour); err == nil || !strings.Contains(err.Error(), "cert_max_ttl") {
		t.Errorf("ttl above cert_max_ttl = %v, want a refusal naming cert_max_ttl", err)
	}
}

func TestIssueClientBundleRefusals(t *testing.T) {
	cfg := clientTestConfig(t)
	if _, err := issueClientBundle(cfg, "stranger", 0); err == nil {
		t.Error("an unknown client was issued a certificate")
	}

	token := clientTestConfig(t)
	token.Auth.Mode = config.AuthModeToken
	if _, err := issueClientBundle(token, "nocturn", 0); err == nil || !strings.Contains(err.Error(), "mtls") {
		t.Errorf("token mode = %v, want a refusal naming mtls", err)
	}
	// Nothing was written for a refused issue.
	entries, _ := os.ReadDir(token.StateDir)
	if len(entries) != 0 {
		t.Errorf("a refused issue wrote %d files into the state dir", len(entries))
	}
}

func TestWriteNewSecretFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bundle.json")
	if err := writeNewSecretFile(path, []byte("secret")); err != nil {
		t.Fatalf("write: %v", err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("mode = %o, want 600", perm)
	}
	if err := writeNewSecretFile(path, []byte("other")); err == nil {
		t.Error("an existing bundle file was overwritten")
	}
	if got, _ := os.ReadFile(path); string(got) != "secret" {
		t.Errorf("file content changed to %q", got)
	}

	// A symlink planted at the path is not followed.
	target := filepath.Join(dir, "target")
	link := filepath.Join(dir, "link.json")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if err := writeNewSecretFile(link, []byte("secret")); err == nil {
		t.Error("wrote through a symlink")
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Error("the symlink's target was created")
	}
}

func TestPrintClientList(t *testing.T) {
	cfg := clientTestConfig(t)
	var buf bytes.Buffer
	if err := printClientList(&buf, cfg, true); err != nil {
		t.Fatal(err)
	}
	var entries []clientListEntry
	if err := json.Unmarshal(buf.Bytes(), &entries); err != nil {
		t.Fatalf("list JSON: %v\n%s", err, buf.String())
	}
	if len(entries) != 1 || entries[0].Name != "nocturn" || entries[0].NamePrefix != "nocturn-" || entries[0].MaxSheds != 2 || entries[0].CertMaxTTL != "48h0m0s" {
		t.Errorf("entries = %+v", entries)
	}
	buf.Reset()
	if err := printClientList(&buf, cfg, false); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "nocturn-") {
		t.Errorf("table lacks the client: %q", buf.String())
	}
}
