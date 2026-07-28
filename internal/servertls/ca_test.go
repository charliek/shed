package servertls

import (
	"bytes"
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// --- helpers ---------------------------------------------------------------

func caPaths(t *testing.T) (dir, certPath, keyPath string) {
	t.Helper()
	dir = t.TempDir()
	return dir, filepath.Join(dir, "ca_cert.pem"), filepath.Join(dir, "ca_key.pem")
}

// mustECDSAKey generates a key on curve or fails the test.
func mustECDSAKey(t *testing.T, curve elliptic.Curve) *ecdsa.PrivateKey {
	t.Helper()
	key, err := ecdsa.GenerateKey(curve, rand.Reader)
	if err != nil {
		t.Fatalf("generate %s key: %v", curveName(curve), err)
	}
	return key
}

// caSpec describes a hand-crafted CA for the strict-load tests. The zero value
// is a valid, current, P-256 CA.
type caSpec struct {
	curve     elliptic.Curve
	notBefore time.Time
	notAfter  time.Time
	notCA     bool
	keyUsage  x509.KeyUsage
}

// writeTestCA mints a CA per spec and writes it to certPath/keyPath with the
// production permissions (0644 / 0600).
func writeTestCA(t *testing.T, certPath, keyPath string, s caSpec) *ecdsa.PrivateKey {
	t.Helper()
	curve := s.curve
	if curve == nil {
		curve = elliptic.P256()
	}
	key := mustECDSAKey(t, curve)
	now := time.Now()
	notBefore, notAfter := s.notBefore, s.notAfter
	if notBefore.IsZero() {
		notBefore = now.Add(-time.Hour)
	}
	if notAfter.IsZero() {
		notAfter = now.Add(365 * 24 * time.Hour)
	}
	keyUsage := s.keyUsage
	if keyUsage == 0 {
		keyUsage = x509.KeyUsageCertSign | x509.KeyUsageCRLSign
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(42),
		Subject:               pkix.Name{CommonName: "test-ca"},
		NotBefore:             notBefore,
		NotAfter:              notAfter,
		KeyUsage:              keyUsage,
		BasicConstraintsValid: true,
		IsCA:                  !s.notCA,
		MaxPathLen:            0,
		MaxPathLenZero:        !s.notCA,
	}
	if s.notCA {
		tmpl.KeyUsage = x509.KeyUsageDigitalSignature
		tmpl.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create test CA cert: %v", err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatalf("marshal test CA key: %v", err)
	}
	writeFile(t, certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0644)
	writeFile(t, keyPath, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0600)
	return key
}

func writeFile(t *testing.T, path string, data []byte, perm os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, data, perm); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	if err := os.Chmod(path, perm); err != nil { // defeat umask
		t.Fatalf("chmod %s: %v", path, err)
	}
}

// symlinkInPlace moves path aside and leaves a symlink to it behind, so the
// content is still reachable but the CA path itself is no longer a real file.
func symlinkInPlace(t *testing.T, path string) {
	t.Helper()
	target := path + ".real"
	if err := os.Rename(path, target); err != nil {
		t.Fatalf("move %s aside: %v", path, err)
	}
	if err := os.Symlink(target, path); err != nil {
		t.Fatalf("symlink %s -> %s: %v", path, target, err)
	}
}

// tamperCACertTBS flips a byte inside the certificate's TBS (a character of the
// encoded subject/issuer name) while leaving the signature bytes alone. The
// result still parses, still carries the right public key, and still claims to
// be a CA — only the self-signature no longer verifies.
func tamperCACertTBS(t *testing.T, certPath string) {
	t.Helper()
	block, _ := pem.Decode(readFile(t, certPath))
	if block == nil {
		t.Fatalf("%s is not PEM", certPath)
	}
	der := bytes.Clone(block.Bytes)
	i := bytes.Index(der, []byte(caCommonName))
	if i < 0 {
		t.Fatalf("subject %q not found in the certificate DER", caCommonName)
	}
	der[i] = 'x'
	if _, err := x509.ParseCertificate(der); err != nil {
		t.Fatalf("tampered certificate no longer parses (test needs a subtler edit): %v", err)
	}
	writeFile(t, certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0644)
}

func readFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return data
}

// loadTestCA writes a CA per spec and loads it through the production loader.
func loadTestCA(t *testing.T, s caSpec) CA {
	t.Helper()
	_, certPath, keyPath := caPaths(t)
	writeTestCA(t, certPath, keyPath, s)
	ca, err := LoadOrGenerateCA(certPath, keyPath)
	if err != nil {
		t.Fatalf("load crafted CA: %v", err)
	}
	return ca
}

// hostileCSRTemplate is a CSR that asks for everything it must not get: an
// admin identity, extra names, and a basicConstraints CA:TRUE extension.
func hostileCSRTemplate() *x509.CertificateRequest {
	return &x509.CertificateRequest{
		Subject: pkix.Name{
			CommonName:         "admin",
			Organization:       []string{"superuser"},
			OrganizationalUnit: []string{"root"},
		},
		DNSNames:       []string{"evil.example.com"},
		EmailAddresses: []string{"root@example.com"},
		IPAddresses:    []net.IP{net.IPv4(10, 0, 0, 1)},
		ExtraExtensions: []pkix.Extension{{
			Id:       []int{2, 5, 29, 19}, // basicConstraints
			Critical: true,
			Value:    []byte{0x30, 0x03, 0x01, 0x01, 0xff}, // SEQUENCE { BOOLEAN TRUE }
		}},
		SignatureAlgorithm: x509.ECDSAWithSHA256,
	}
}

func makeCSR(t *testing.T, tmpl *x509.CertificateRequest, key crypto.Signer) []byte {
	t.Helper()
	der, err := x509.CreateCertificateRequest(rand.Reader, tmpl, key)
	if err != nil {
		t.Fatalf("create CSR: %v", err)
	}
	return der
}

func p256CSR(t *testing.T) ([]byte, *ecdsa.PrivateKey) {
	t.Helper()
	key := mustECDSAKey(t, elliptic.P256())
	return makeCSR(t, &x509.CertificateRequest{SignatureAlgorithm: x509.ECDSAWithSHA256}, key), key
}

const testFingerprint = "SHA256:1U6JLBiKdyPFRj5o2Vv5uF3d8Kk0Y0mYFDDaC4mHtCk"

// signOK issues a cert for csrDER with the standard test identity and returns
// it parsed. Signing failures are fatal — callers asserting on failures call
// SignClientCSR directly.
func signOK(t *testing.T, ca CA, csrDER []byte, ttl time.Duration) *x509.Certificate {
	t.Helper()
	der, err := ca.SignClientCSR(csrDER, testFingerprint, "shed", "cli", ttl)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	return leaf(t, der)
}

// --- generation ------------------------------------------------------------

func TestLoadOrGenerateCAGenerates(t *testing.T) {
	_, certPath, keyPath := caPaths(t)

	ca, err := LoadOrGenerateCA(certPath, keyPath)
	if err != nil {
		t.Fatalf("generate CA: %v", err)
	}

	certInfo, err := os.Stat(certPath)
	if err != nil {
		t.Fatalf("stat cert: %v", err)
	}
	if perm := certInfo.Mode().Perm(); perm != 0644 {
		t.Errorf("cert perms = %04o, want 0644", perm)
	}
	keyInfo, err := os.Stat(keyPath)
	if err != nil {
		t.Fatalf("stat key: %v", err)
	}
	if perm := keyInfo.Mode().Perm(); perm != 0600 {
		t.Errorf("key perms = %04o, want 0600", perm)
	}

	cert := ca.Cert()
	if cert == nil {
		t.Fatal("Cert() is nil")
	}
	if !cert.IsCA {
		t.Error("generated cert is not a CA")
	}
	if !cert.BasicConstraintsValid {
		t.Error("BasicConstraintsValid must be set")
	}
	if !cert.MaxPathLenZero || cert.MaxPathLen != 0 {
		t.Errorf("MaxPathLen = %d (zero=%v), want 0 (zero=true)", cert.MaxPathLen, cert.MaxPathLenZero)
	}
	if cert.Subject.CommonName != "shed-ca" {
		t.Errorf("CommonName = %q, want %q", cert.Subject.CommonName, "shed-ca")
	}
	if want := x509.KeyUsageCertSign | x509.KeyUsageCRLSign; cert.KeyUsage != want {
		t.Errorf("KeyUsage = %b, want %b (certSign|crlSign)", cert.KeyUsage, want)
	}
	if len(cert.ExtKeyUsage) != 0 {
		t.Errorf("CA should carry no EKU, got %v", cert.ExtKeyUsage)
	}
	pub, ok := cert.PublicKey.(*ecdsa.PublicKey)
	if !ok {
		t.Fatalf("public key is %T, want *ecdsa.PublicKey", cert.PublicKey)
	}
	if pub.Curve != elliptic.P256() {
		t.Errorf("curve = %s, want P-256", curveName(pub.Curve))
	}
	if got := cert.NotAfter.Sub(cert.NotBefore); got < caValidity || got > caValidity+time.Hour {
		t.Errorf("validity = %s, want ~%s", got, caValidity)
	}
	if !cert.NotBefore.Before(time.Now()) {
		t.Error("NotBefore should be backdated for clock skew")
	}
}

// TestCAAccessorsMatchPersistedMaterial checks the derived views the rest of
// the server consumes (DER, PEM, pin, expiry, pool) all describe the CA that
// was actually written to disk.
func TestCAAccessorsMatchPersistedMaterial(t *testing.T) {
	_, certPath, keyPath := caPaths(t)
	ca, err := LoadOrGenerateCA(certPath, keyPath)
	if err != nil {
		t.Fatalf("generate CA: %v", err)
	}
	cert := ca.Cert()

	if !bytes.Equal(ca.CertDER(), cert.Raw) {
		t.Error("CertDER does not match the parsed certificate")
	}
	if !bytes.Equal(ca.CertPEM(), readFile(t, certPath)) {
		t.Error("CertPEM does not match the persisted file")
	}
	if got, want := ca.Fingerprint(), Fingerprint(cert.Raw); got != want {
		t.Errorf("Fingerprint = %s, want %s", got, want)
	}
	if !ca.NotAfter().Equal(cert.NotAfter) {
		t.Errorf("NotAfter = %s, want %s", ca.NotAfter(), cert.NotAfter)
	}
	if len(ca.Pool().Subjects()) != 1 { //nolint:staticcheck // Subjects is fine for a non-system pool
		t.Error("Pool() should contain exactly the CA")
	}
	// Each call hands out a fresh pool, so a caller mutating one cannot widen
	// another's trust.
	if first, second := ca.Pool(), ca.Pool(); first == second {
		t.Error("Pool() must return a fresh pool per call")
	}
}

func TestLoadOrGenerateCAReloadIsIdentical(t *testing.T) {
	_, certPath, keyPath := caPaths(t)

	first, err := LoadOrGenerateCA(certPath, keyPath)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	certBefore := readFile(t, certPath)
	keyBefore := readFile(t, keyPath)

	second, err := LoadOrGenerateCA(certPath, keyPath)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if first.Fingerprint() != second.Fingerprint() {
		t.Errorf("fingerprint changed across reload: %s vs %s", first.Fingerprint(), second.Fingerprint())
	}
	if !bytes.Equal(certBefore, readFile(t, certPath)) || !bytes.Equal(keyBefore, readFile(t, keyPath)) {
		t.Error("reload rewrote the persisted CA material")
	}
	// The reloaded key must be the same key: a cert issued by the reloaded CA
	// verifies against the original CA's pool.
	csr, _ := p256CSR(t)
	issued := signOK(t, second, csr, time.Hour)
	if _, err := issued.Verify(x509.VerifyOptions{
		Roots:     first.Pool(),
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}); err != nil {
		t.Errorf("cert issued after reload does not chain to the original CA: %v", err)
	}
}

// TestLoadOrGenerateCAConcurrentFirstStart is the race the file lock exists
// for: several starters hitting an empty CA directory at once. Without
// serialization each would see "no CA yet", mint its own root, and the last
// writer would win — leaving the others holding a private key that no longer
// matches what is on disk, and any cert they had already issued unverifiable.
// Every caller must come away with the same CA.
func TestLoadOrGenerateCAConcurrentFirstStart(t *testing.T) {
	_, certPath, keyPath := caPaths(t)

	const starters = 4
	var wg sync.WaitGroup
	begin := make(chan struct{})
	cas := make([]CA, starters)
	errs := make([]error, starters)

	for i := range starters {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-begin
			cas[i], errs[i] = LoadOrGenerateCA(certPath, keyPath)
		}(i)
	}
	close(begin)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("starter %d: %v", i, err)
		}
	}
	want := cas[0].Fingerprint()
	for i, ca := range cas {
		if got := ca.Fingerprint(); got != want {
			t.Errorf("starter %d got CA %s, want %s", i, got, want)
		}
	}
	// Same fingerprint is not enough: each starter must hold the *key* for the
	// CA on disk, so a cert it issues chains to the first starter's root.
	csr, _ := p256CSR(t)
	for i, ca := range cas {
		cert := signOK(t, ca, csr, time.Hour)
		if _, err := cert.Verify(x509.VerifyOptions{
			Roots:     cas[0].Pool(),
			KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		}); err != nil {
			t.Errorf("cert from starter %d does not chain to the shared CA: %v", i, err)
		}
	}
	// And the persisted material is that same CA.
	reloaded, err := LoadOrGenerateCA(certPath, keyPath)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if got := reloaded.Fingerprint(); got != want {
		t.Errorf("persisted CA is %s, want %s", got, want)
	}
}

// TestCACertIsDefensiveCopy: a caller poking at the returned certificate must
// not be able to reach into the CA every other caller shares.
func TestCACertIsDefensiveCopy(t *testing.T) {
	ca := loadTestCA(t, caSpec{})
	original := ca.Cert()

	mutated := ca.Cert()
	mutated.Subject.CommonName = "attacker"
	mutated.IsCA = false
	mutated.NotAfter = time.Unix(0, 0)

	if again := ca.Cert(); again.Subject.CommonName != original.Subject.CommonName ||
		!again.IsCA || !again.NotAfter.Equal(original.NotAfter) {
		t.Errorf("mutating the returned cert changed the CA: %+v", again.Subject)
	}
	if ca.NotAfter().Equal(time.Unix(0, 0)) {
		t.Error("mutating the returned cert changed NotAfter()")
	}
	if first, second := ca.Cert(), ca.Cert(); first == second {
		t.Error("Cert() must return a fresh copy per call")
	}
	if (CA{}).Cert() != nil {
		t.Error("the zero CA must report a nil certificate")
	}
}

// --- strict load -----------------------------------------------------------

// TestLoadOrGenerateCAStrictFailures covers every corrupt/partial on-disk state.
// Beyond the error itself, each case asserts the loader left the files exactly
// as it found them: silently regenerating would invalidate every client cert
// already issued from the old root.
func TestLoadOrGenerateCAStrictFailures(t *testing.T) {
	tests := []struct {
		name    string
		corrupt func(t *testing.T, certPath, keyPath string)
		wantMsg string
	}{
		{
			name: "key missing",
			corrupt: func(t *testing.T, _, keyPath string) {
				if err := os.Remove(keyPath); err != nil {
					t.Fatal(err)
				}
			},
			wantMsg: "half-present",
		},
		{
			name: "cert missing",
			corrupt: func(t *testing.T, certPath, _ string) {
				if err := os.Remove(certPath); err != nil {
					t.Fatal(err)
				}
			},
			wantMsg: "half-present",
		},
		{
			name: "key truncated",
			corrupt: func(t *testing.T, _, keyPath string) {
				data := readFile(t, keyPath)
				writeFile(t, keyPath, data[:len(data)/2], 0600)
			},
			wantMsg: "CA key",
		},
		{
			name: "key not PEM",
			corrupt: func(t *testing.T, _, keyPath string) {
				writeFile(t, keyPath, []byte("not a pem file\n"), 0600)
			},
			wantMsg: "no PEM block",
		},
		{
			name: "cert not PEM",
			corrupt: func(t *testing.T, certPath, _ string) {
				writeFile(t, certPath, []byte("not a pem file\n"), 0644)
			},
			wantMsg: "no PEM block",
		},
		{
			name: "cert PEM body not DER",
			corrupt: func(t *testing.T, certPath, _ string) {
				writeFile(t, certPath, pem.EncodeToMemory(&pem.Block{
					Type: "CERTIFICATE", Bytes: []byte("garbage"),
				}), 0644)
			},
			wantMsg: "parse certificate",
		},
		{
			name: "key does not match cert",
			corrupt: func(t *testing.T, _, keyPath string) {
				other := filepath.Join(filepath.Dir(keyPath), "other")
				writeTestCA(t, other+"_cert.pem", other+"_key.pem", caSpec{})
				writeFile(t, keyPath, readFile(t, other+"_key.pem"), 0600)
			},
			wantMsg: "does not match",
		},
		{
			name: "cert is not a CA",
			corrupt: func(t *testing.T, certPath, keyPath string) {
				writeTestCA(t, certPath, keyPath, caSpec{notCA: true})
			},
			wantMsg: "not a CA",
		},
		{
			name: "CA expired",
			corrupt: func(t *testing.T, certPath, keyPath string) {
				writeTestCA(t, certPath, keyPath, caSpec{
					notBefore: time.Now().Add(-48 * time.Hour),
					notAfter:  time.Now().Add(-time.Hour),
				})
			},
			wantMsg: "expired",
		},
		{
			name: "CA not yet valid",
			corrupt: func(t *testing.T, certPath, keyPath string) {
				writeTestCA(t, certPath, keyPath, caSpec{
					notBefore: time.Now().Add(24 * time.Hour),
					notAfter:  time.Now().Add(48 * time.Hour),
				})
			},
			wantMsg: "not valid until",
		},
		{
			name: "wrong curve",
			corrupt: func(t *testing.T, certPath, keyPath string) {
				writeTestCA(t, certPath, keyPath, caSpec{curve: elliptic.P384()})
			},
			wantMsg: "want P-256",
		},
		{
			name: "CA lacks certSign",
			corrupt: func(t *testing.T, certPath, keyPath string) {
				writeTestCA(t, certPath, keyPath, caSpec{keyUsage: x509.KeyUsageDigitalSignature})
			},
			wantMsg: "keyCertSign",
		},
		{
			name: "key world readable",
			corrupt: func(t *testing.T, _, keyPath string) {
				if err := os.Chmod(keyPath, 0644); err != nil {
					t.Fatal(err)
				}
			},
			wantMsg: "readable beyond its owner",
		},
		{
			name: "key is a symlink",
			corrupt: func(t *testing.T, _, keyPath string) {
				symlinkInPlace(t, keyPath)
			},
			wantMsg: "is a symlink",
		},
		{
			name: "cert is a symlink",
			corrupt: func(t *testing.T, certPath, _ string) {
				symlinkInPlace(t, certPath)
			},
			wantMsg: "is a symlink",
		},
		{
			name: "cert is a directory",
			corrupt: func(t *testing.T, certPath, _ string) {
				if err := os.Remove(certPath); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(certPath, 0700); err != nil {
					t.Fatal(err)
				}
			},
			wantMsg: "not a regular file",
		},
		{
			// A tampered TBS is the one corruption the other checks cannot see:
			// the key still matches, the constraints still say CA, only the
			// signature disagrees.
			name: "cert TBS tampered",
			corrupt: func(t *testing.T, certPath, _ string) {
				tamperCACertTBS(t, certPath)
			},
			wantMsg: "not validly self-signed",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, certPath, keyPath := caPaths(t)
			if _, err := LoadOrGenerateCA(certPath, keyPath); err != nil {
				t.Fatalf("seed CA: %v", err)
			}
			tt.corrupt(t, certPath, keyPath)

			certBefore, certErr := os.ReadFile(certPath)
			keyBefore, keyErr := os.ReadFile(keyPath)

			ca, err := LoadOrGenerateCA(certPath, keyPath)
			if err == nil {
				t.Fatalf("expected an error, got a CA with fingerprint %s", ca.Fingerprint())
			}
			if !strings.Contains(err.Error(), tt.wantMsg) {
				t.Errorf("error %q does not mention %q", err, tt.wantMsg)
			}

			// The loader must never regenerate over a failed load.
			certAfter, certErrAfter := os.ReadFile(certPath)
			keyAfter, keyErrAfter := os.ReadFile(keyPath)
			if (certErr == nil) != (certErrAfter == nil) || !bytes.Equal(certBefore, certAfter) {
				t.Error("cert file changed after a failed load")
			}
			if (keyErr == nil) != (keyErrAfter == nil) || !bytes.Equal(keyBefore, keyAfter) {
				t.Error("key file changed after a failed load")
			}
		})
	}
}

// TestLoadOrGenerateCATightensKeyPerms covers the one permission case the
// loader repairs rather than rejects: owner-only but not exactly 0600.
func TestLoadOrGenerateCATightensKeyPerms(t *testing.T) {
	_, certPath, keyPath := caPaths(t)
	writeTestCA(t, certPath, keyPath, caSpec{})
	if err := os.Chmod(keyPath, 0400); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadOrGenerateCA(certPath, keyPath); err != nil {
		t.Fatalf("load: %v", err)
	}
	info, err := os.Stat(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0600 {
		t.Errorf("key perms = %04o, want 0600", perm)
	}
}

// TestGenerateCAFailureLeavesNoFiles is the closest a unit test gets to the
// atomicity guarantee: a generate that cannot write leaves nothing behind for
// the next start to trip over.
func TestGenerateCAFailureLeavesNoFiles(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	dir := t.TempDir()
	sub := filepath.Join(dir, "ca")
	if err := os.Mkdir(sub, 0500); err != nil { // readable, not writable
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(sub, 0700) })

	certPath := filepath.Join(sub, "ca_cert.pem")
	keyPath := filepath.Join(sub, "ca_key.pem")
	if _, err := LoadOrGenerateCA(certPath, keyPath); err == nil {
		t.Fatal("expected generation into an unwritable dir to fail")
	}
	entries, err := os.ReadDir(sub)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("failed generation left files behind: %v", names)
	}
}

// --- signing ---------------------------------------------------------------

func TestSignClientCSRIgnoresRequestedIdentity(t *testing.T) {
	_, certPath, keyPath := caPaths(t)
	ca, err := LoadOrGenerateCA(certPath, keyPath)
	if err != nil {
		t.Fatalf("generate CA: %v", err)
	}

	key := mustECDSAKey(t, elliptic.P256())
	csr := makeCSR(t, hostileCSRTemplate(), key)

	const ttl = 2 * time.Hour
	before := time.Now()
	der, err := ca.SignClientCSR(csr, testFingerprint, "shed:my-shed", "cli", ttl)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	after := time.Now()
	cert := leaf(t, der)

	// Identity comes from the arguments, never from the CSR.
	if cert.Subject.CommonName != testFingerprint {
		t.Errorf("CommonName = %q, want %q", cert.Subject.CommonName, testFingerprint)
	}
	if len(cert.Subject.OrganizationalUnit) != 1 || cert.Subject.OrganizationalUnit[0] != "shed:my-shed" {
		t.Errorf("OU = %v, want [shed:my-shed]", cert.Subject.OrganizationalUnit)
	}
	if len(cert.Subject.Organization) != 1 || cert.Subject.Organization[0] != "cli" {
		t.Errorf("O = %v, want [cli]", cert.Subject.Organization)
	}
	if len(cert.DNSNames) != 0 || len(cert.EmailAddresses) != 0 || len(cert.IPAddresses) != 0 || len(cert.URIs) != 0 {
		t.Errorf("issued cert carries SANs: dns=%v email=%v ip=%v uri=%v",
			cert.DNSNames, cert.EmailAddresses, cert.IPAddresses, cert.URIs)
	}
	if cert.IsCA {
		t.Error("issued cert must not be a CA (the CSR asked for CA:TRUE)")
	}

	// Usage, serial, validity.
	if cert.KeyUsage != x509.KeyUsageDigitalSignature {
		t.Errorf("KeyUsage = %b, want digitalSignature only", cert.KeyUsage)
	}
	if len(cert.ExtKeyUsage) != 1 || cert.ExtKeyUsage[0] != x509.ExtKeyUsageClientAuth {
		t.Errorf("ExtKeyUsage = %v, want [clientAuth]", cert.ExtKeyUsage)
	}
	if cert.SerialNumber == nil || cert.SerialNumber.Sign() <= 0 {
		t.Errorf("serial = %v, want a positive random value", cert.SerialNumber)
	}
	if cert.SerialNumber.BitLen() < 96 {
		t.Errorf("serial has %d bits, want ~128 of entropy", cert.SerialNumber.BitLen())
	}
	skew := before.Add(-clientCertBackdate)
	if cert.NotBefore.After(skew.Add(time.Minute)) || cert.NotBefore.Before(skew.Add(-time.Minute)) {
		t.Errorf("NotBefore = %s, want ~%s (now - 5m)", cert.NotBefore, skew)
	}
	// x509 stores times at second granularity, hence the small tolerance.
	if cert.NotAfter.Before(before.Add(ttl).Add(-time.Minute)) || cert.NotAfter.After(after.Add(ttl+time.Minute)) {
		t.Errorf("NotAfter = %s, want ~%s (now + ttl)", cert.NotAfter, before.Add(ttl))
	}

	// The certified key is the CSR's key.
	pub, ok := cert.PublicKey.(*ecdsa.PublicKey)
	if !ok || !pub.Equal(key.Public()) {
		t.Error("issued cert does not certify the CSR's public key")
	}

	// No extensions beyond the five the issuance policy allows.
	allowed := map[string]bool{
		"2.5.29.19": true, // basicConstraints
		"2.5.29.15": true, // keyUsage
		"2.5.29.37": true, // extKeyUsage
		"2.5.29.35": true, // authorityKeyIdentifier
		"2.5.29.14": true, // subjectKeyIdentifier
	}
	seen := make(map[string]bool, len(cert.Extensions))
	for _, ext := range cert.Extensions {
		if !allowed[ext.Id.String()] {
			t.Errorf("unexpected extension %s in issued cert", ext.Id)
		}
		seen[ext.Id.String()] = true
	}
	// The two key identifiers are not optional: chain builders and operators
	// use them to tie a presented client cert back to this CA.
	for _, oid := range []string{"2.5.29.14", "2.5.29.35"} {
		if !seen[oid] {
			t.Errorf("issued cert is missing extension %s", oid)
		}
	}

	// And it chains to the CA as a client cert.
	if _, err := cert.Verify(x509.VerifyOptions{
		Roots:     ca.Pool(),
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}); err != nil {
		t.Errorf("issued cert does not verify against the CA pool: %v", err)
	}
}

// TestSignClientCSRKeyIdentifiers pins the subject/authority key identifiers on
// an issued cert: both present, and the AKID naming the CA's own SKID so the
// leaf points at the root that signed it.
func TestSignClientCSRKeyIdentifiers(t *testing.T) {
	_, certPath, keyPath := caPaths(t)
	ca, err := LoadOrGenerateCA(certPath, keyPath)
	if err != nil {
		t.Fatalf("generate CA: %v", err)
	}
	csr, key := p256CSR(t)
	cert := signOK(t, ca, csr, time.Hour)

	if len(cert.SubjectKeyId) == 0 {
		t.Error("issued cert has no subjectKeyIdentifier")
	}
	if len(cert.AuthorityKeyId) == 0 {
		t.Fatal("issued cert has no authorityKeyIdentifier")
	}
	caSKID := ca.Cert().SubjectKeyId
	if len(caSKID) == 0 {
		t.Fatal("CA cert has no subjectKeyIdentifier to point at")
	}
	if !bytes.Equal(cert.AuthorityKeyId, caSKID) {
		t.Errorf("AKID = %x, want the CA's SKID %x", cert.AuthorityKeyId, caSKID)
	}
	if bytes.Equal(cert.SubjectKeyId, caSKID) {
		t.Error("leaf SKID must be derived from the leaf's own key, not the CA's")
	}

	// The SKID is RFC 7093 method 1: SHA-256 over the SPKI BIT STRING,
	// truncated to 160 bits. Recompute it independently from the CSR's key.
	want, err := subjectKeyID(&key.PublicKey)
	if err != nil {
		t.Fatalf("subjectKeyID: %v", err)
	}
	if len(want) != 20 {
		t.Errorf("SKID is %d bytes, want 20 (160 bits)", len(want))
	}
	if !bytes.Equal(cert.SubjectKeyId, want) {
		t.Errorf("SKID = %x, want %x", cert.SubjectKeyId, want)
	}

	// Two different keys get two different identifiers.
	otherCSR, _ := p256CSR(t)
	other := signOK(t, ca, otherCSR, time.Hour)
	if bytes.Equal(other.SubjectKeyId, cert.SubjectKeyId) {
		t.Error("distinct client keys produced the same SKID")
	}
}

func TestSignClientCSROmitsEmptySubjectFields(t *testing.T) {
	ca := loadTestCA(t, caSpec{})
	csr, _ := p256CSR(t)
	der, err := ca.SignClientCSR(csr, testFingerprint, "", "", time.Hour)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	cert := leaf(t, der)
	if len(cert.Subject.OrganizationalUnit) != 0 || len(cert.Subject.Organization) != 0 {
		t.Errorf("empty scope/kind should be omitted, got OU=%v O=%v",
			cert.Subject.OrganizationalUnit, cert.Subject.Organization)
	}
}

func TestSignClientCSRValidation(t *testing.T) {
	ca := loadTestCA(t, caSpec{})

	good, _ := p256CSR(t)

	tampered := bytes.Clone(good)
	tampered[len(tampered)-1] ^= 0xff

	rsaKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	_, edKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	p384Key := mustECDSAKey(t, elliptic.P384())
	sha1Key := mustECDSAKey(t, elliptic.P256())

	tests := []struct {
		name    string
		csr     []byte
		wantErr error
	}{
		{"oversized", bytes.Repeat([]byte{0x30}, maxCSRBytes+1), ErrCSRTooLarge},
		{"empty", nil, ErrCSRInvalidDER},
		{"garbage", []byte{0xff, 0xff, 0xff}, ErrCSRInvalidDER},
		{"trailing bytes", append(bytes.Clone(good), 0x00), ErrCSRInvalidDER},
		{"tampered signature", tampered, ErrCSRInvalidSignature},
		{"rsa key", makeCSR(t, &x509.CertificateRequest{}, rsaKey), ErrCSRUnsupportedKey},
		{"p384 key", makeCSR(t, &x509.CertificateRequest{}, p384Key), ErrCSRUnsupportedKey},
		{"ed25519 key", makeCSR(t, &x509.CertificateRequest{}, edKey), ErrCSRUnsupportedKey},
		{
			"sha1 signature",
			makeCSR(t, &x509.CertificateRequest{SignatureAlgorithm: x509.ECDSAWithSHA1}, sha1Key),
			ErrCSRWeakSignature,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			der, err := ca.SignClientCSR(tt.csr, testFingerprint, "shed", "cli", time.Hour)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("error = %v, want %v", err, tt.wantErr)
			}
			if der != nil {
				t.Error("no certificate may be returned alongside an error")
			}
		})
	}
}

// TestCSRErrorStrings pins the wire text of every error the enrollment path can
// hand back: clients see these verbatim over SSH stderr, so they are protocol,
// not diagnostics.
func TestCSRErrorStrings(t *testing.T) {
	for err, want := range map[error]string{
		ErrCSRTooLarge:         "csr: too large",
		ErrCSRInvalidDER:       "csr: invalid DER",
		ErrCSRInvalidSignature: "csr: invalid signature",
		ErrCSRUnsupportedKey:   "csr: unsupported key type (need P-256)",
		ErrCSRWeakSignature:    "csr: weak signature algorithm",
		ErrCAExpiringSoon:      "ca: expiring soon; rotate the CA",
	} {
		if err.Error() != want {
			t.Errorf("error string %q changed; want %q", err, want)
		}
	}
}

func TestSignClientCSRArgumentGuards(t *testing.T) {
	loaded := loadTestCA(t, caSpec{})
	csr, _ := p256CSR(t)

	tests := []struct {
		name        string
		ca          CA
		fingerprint string
		ttl         time.Duration
		wantErr     error
	}{
		{"empty fingerprint", loaded, "", time.Hour, ErrCAEmptySubject},
		{"zero ttl", loaded, testFingerprint, 0, ErrCAInvalidTTL},
		{"negative ttl", loaded, testFingerprint, -time.Hour, ErrCAInvalidTTL},
		{"zero CA", CA{}, testFingerprint, time.Hour, ErrCANotInitialized},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			der, err := tt.ca.SignClientCSR(csr, tt.fingerprint, "shed", "cli", tt.ttl)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("error = %v, want %v", err, tt.wantErr)
			}
			if der != nil {
				t.Error("no certificate may be returned alongside an error")
			}
		})
	}
}

// TestSignClientCSRSubjectFingerprintForm pins the CN guard. The fingerprint
// reaches issuance from the authenticated SSH channel, never from the client,
// so a malformed one is a server-side bug — but the CN is what authorization
// later matches on, so the shape is enforced rather than assumed.
func TestSignClientCSRSubjectFingerprintForm(t *testing.T) {
	ca := loadTestCA(t, caSpec{})
	csr, _ := p256CSR(t)

	b64 := func(b []byte) string { return base64.RawStdEncoding.EncodeToString(b) }
	allSlashes := b64(bytes.Repeat([]byte{0xff}, 32)) // exercises the '/' alphabet
	allZeros := b64(make([]byte, 32))                 // 43 'A's

	t.Run("accepted", func(t *testing.T) {
		for _, fp := range []string{
			testFingerprint,
			sshFingerprintPrefix + allSlashes,
			sshFingerprintPrefix + allZeros,
		} {
			if _, err := ca.SignClientCSR(csr, fp, "shed", "cli", time.Hour); err != nil {
				t.Errorf("fingerprint %q rejected: %v", fp, err)
			}
		}
	})

	tests := []struct {
		name        string
		fingerprint string
	}{
		{"lowercase prefix", "sha256:" + allZeros},
		{"no prefix", allZeros},
		{"md5 form", "MD5:1f:0c:1a:2b:3c:4d:5e:6f:70:81:92:a3:b4:c5:d6:e7"},
		{"one char short", sshFingerprintPrefix + allZeros[:42]},
		{"one char long", sshFingerprintPrefix + allZeros + "A"},
		{"padded base64", sshFingerprintPrefix + base64.StdEncoding.EncodeToString(make([]byte, 32))},
		{"url-safe alphabet", sshFingerprintPrefix + strings.Repeat("-", 43)},
		{"non-base64 character", sshFingerprintPrefix + strings.Repeat("A", 42) + "!"},
		{"non-canonical trailing bits", sshFingerprintPrefix + strings.Repeat("A", 42) + "B"},
		{"whitespace padded", " " + testFingerprint},
		{"prefix only", sshFingerprintPrefix},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			der, err := ca.SignClientCSR(csr, tt.fingerprint, "shed", "cli", time.Hour)
			if !errors.Is(err, ErrCAInvalidSubject) {
				t.Fatalf("error = %v, want %v", err, ErrCAInvalidSubject)
			}
			if der != nil {
				t.Error("no certificate may be returned alongside an error")
			}
		})
	}
}

func TestSignClientCSRLifetimeBounds(t *testing.T) {
	csr, _ := p256CSR(t)

	t.Run("clamped to CA expiry", func(t *testing.T) {
		ca := loadTestCA(t, caSpec{notAfter: time.Now().Add(72 * time.Hour)})
		cert := signOK(t, ca, csr, 7*24*time.Hour)
		if !cert.NotAfter.Equal(ca.NotAfter()) {
			t.Errorf("NotAfter = %s, want the CA's %s", cert.NotAfter, ca.NotAfter())
		}
	})

	t.Run("ttl inside CA lifetime is untouched", func(t *testing.T) {
		ca := loadTestCA(t, caSpec{notAfter: time.Now().Add(72 * time.Hour)})
		cert := signOK(t, ca, csr, time.Hour)
		if cert.NotAfter.After(time.Now().Add(time.Hour + time.Minute)) {
			t.Errorf("NotAfter = %s, want ~now+1h", cert.NotAfter)
		}
	})

	t.Run("refuses when the CA expires soon", func(t *testing.T) {
		ca := loadTestCA(t, caSpec{notAfter: time.Now().Add(24 * time.Hour)})
		if _, err := ca.SignClientCSR(csr, testFingerprint, "shed", "cli", time.Hour); !errors.Is(err, ErrCAExpiringSoon) {
			t.Errorf("err = %v, want %v", err, ErrCAExpiringSoon)
		}
	})

	t.Run("issues just above the refusal boundary", func(t *testing.T) {
		ca := loadTestCA(t, caSpec{notAfter: time.Now().Add(caMinRemaining + time.Hour)})
		signOK(t, ca, csr, time.Hour) // fatal if issuance is refused

	})
}
