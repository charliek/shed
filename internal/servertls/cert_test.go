package servertls

import (
	"crypto/tls"
	"crypto/x509"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func leaf(t *testing.T, der []byte) *x509.Certificate {
	t.Helper()
	c, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("parse cert: %v", err)
	}
	return c
}

func TestGenerateThenReloadIsStable(t *testing.T) {
	dir := t.TempDir()
	certPath := filepath.Join(dir, "tls_cert.pem")
	keyPath := filepath.Join(dir, "tls_key.pem")
	names := []string{"shed.example.com", "10.0.0.5"}

	_, der1, err := LoadOrGenerate(certPath, keyPath, names)
	if err != nil {
		t.Fatalf("first generate: %v", err)
	}
	// A second call with the same names reuses the persisted cert, so the
	// pinned fingerprint is stable across restarts.
	_, der2, err := LoadOrGenerate(certPath, keyPath, names)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if Fingerprint(der1) != Fingerprint(der2) {
		t.Errorf("fingerprint changed across reload: %s vs %s", Fingerprint(der1), Fingerprint(der2))
	}
}

func TestSANsIncludeNamesAndLoopback(t *testing.T) {
	dir := t.TempDir()
	_, der, err := LoadOrGenerate(filepath.Join(dir, "c.pem"), filepath.Join(dir, "k.pem"),
		[]string{"shed.example.com", "192.168.1.10"})
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	c := leaf(t, der)

	wantDNS := map[string]bool{"localhost": false, "shed.example.com": false}
	for _, d := range c.DNSNames {
		if _, ok := wantDNS[d]; ok {
			wantDNS[d] = true
		}
	}
	for name, found := range wantDNS {
		if !found {
			t.Errorf("missing DNS SAN %q (got %v)", name, c.DNSNames)
		}
	}

	wantIP := map[string]bool{"127.0.0.1": false, "::1": false, "192.168.1.10": false}
	for _, ip := range c.IPAddresses {
		if _, ok := wantIP[ip.String()]; ok {
			wantIP[ip.String()] = true
		}
	}
	for ip, found := range wantIP {
		if !found {
			t.Errorf("missing IP SAN %q (got %v)", ip, c.IPAddresses)
		}
	}
}

func TestRegeneratesWhenNamesGrow(t *testing.T) {
	dir := t.TempDir()
	certPath := filepath.Join(dir, "c.pem")
	keyPath := filepath.Join(dir, "k.pem")

	_, der1, err := LoadOrGenerate(certPath, keyPath, []string{"a.example.com"})
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	// Adding a name the persisted cert can't cover forces regeneration so the
	// new SAN takes effect; the fingerprint necessarily changes.
	_, der2, err := LoadOrGenerate(certPath, keyPath, []string{"a.example.com", "b.example.com"})
	if err != nil {
		t.Fatalf("regenerate: %v", err)
	}
	if Fingerprint(der1) == Fingerprint(der2) {
		t.Fatal("expected a new fingerprint after adding a SAN")
	}
	if !covers(leaf(t, der2), []string{"a.example.com", "b.example.com"}, nil) {
		t.Error("regenerated cert does not cover both names")
	}
}

func TestExistingCertReusedWhenNamesSubset(t *testing.T) {
	dir := t.TempDir()
	certPath := filepath.Join(dir, "c.pem")
	keyPath := filepath.Join(dir, "k.pem")

	_, der1, err := LoadOrGenerate(certPath, keyPath, []string{"a.example.com", "b.example.com"})
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	// A narrower name set is already covered, so the cert is reused as-is.
	_, der2, err := LoadOrGenerate(certPath, keyPath, []string{"a.example.com"})
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if Fingerprint(der1) != Fingerprint(der2) {
		t.Error("cert should be reused when configured names are a subset of its SANs")
	}
}

func TestFingerprintFormat(t *testing.T) {
	dir := t.TempDir()
	_, der, err := LoadOrGenerate(filepath.Join(dir, "c.pem"), filepath.Join(dir, "k.pem"), nil)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	fp := Fingerprint(der)
	hexPart, ok := strings.CutPrefix(fp, "sha256:")
	if !ok {
		t.Fatalf("fingerprint missing sha256: prefix: %q", fp)
	}
	if len(hexPart) != 64 {
		t.Errorf("expected 64 hex chars, got %d in %q", len(hexPart), fp)
	}
}

func TestKeyFilePermissions(t *testing.T) {
	dir := t.TempDir()
	certPath := filepath.Join(dir, "c.pem")
	keyPath := filepath.Join(dir, "k.pem")
	if _, _, err := LoadOrGenerate(certPath, keyPath, nil); err != nil {
		t.Fatalf("generate: %v", err)
	}
	info, err := os.Stat(keyPath)
	if err != nil {
		t.Fatalf("stat key: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0600 {
		t.Errorf("key file perms = %o, want 0600", perm)
	}
}

// TestPinnedClientConfig exercises the shared trust primitive every in-repo
// client uses to verify the self-signed server cert. The handshake-time check
// lives in the VerifyPeerCertificate callback, so the test drives that callback
// directly with raw DER instead of standing up a TLS server.
func TestPinnedClientConfig(t *testing.T) {
	dir := t.TempDir()
	// Two independently-generated certs: one we pin to, one that stands in for
	// a MITM / wrong server presenting a different leaf.
	_, goodDER, err := LoadOrGenerate(filepath.Join(dir, "good_c.pem"), filepath.Join(dir, "good_k.pem"), nil)
	if err != nil {
		t.Fatalf("generate good cert: %v", err)
	}
	_, otherDER, err := LoadOrGenerate(filepath.Join(dir, "other_c.pem"), filepath.Join(dir, "other_k.pem"), nil)
	if err != nil {
		t.Fatalf("generate other cert: %v", err)
	}

	cfg := PinnedClientConfig(Fingerprint(goodDER))

	// The config must keep the self-signed trust model intact: skip the default
	// CA/hostname chain check (the cert is its own anchor) but floor the version
	// at TLS 1.2. A regression to either of these silently weakens every client.
	if !cfg.InsecureSkipVerify {
		t.Error("InsecureSkipVerify must be true — the pin replaces chain verification")
	}
	if cfg.MinVersion != tls.VersionTLS12 {
		t.Errorf("MinVersion = %x, want TLS 1.2 (%x)", cfg.MinVersion, tls.VersionTLS12)
	}
	if cfg.VerifyPeerCertificate == nil {
		t.Fatal("VerifyPeerCertificate must be set — it is the entire trust check")
	}

	// The pinned leaf verifies.
	if err := cfg.VerifyPeerCertificate([][]byte{goodDER}, nil); err != nil {
		t.Errorf("pinned cert rejected: %v", err)
	}
	// A different leaf (wrong fingerprint) is refused — this is the MITM defense.
	if err := cfg.VerifyPeerCertificate([][]byte{otherDER}, nil); err == nil {
		t.Error("expected fingerprint mismatch to be rejected")
	}
	// A handshake presenting no certificate at all is refused rather than
	// passing vacuously.
	if err := cfg.VerifyPeerCertificate(nil, nil); err == nil {
		t.Error("expected an empty cert chain to be rejected")
	}
}
