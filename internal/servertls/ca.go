package servertls

// ca.go adds a tiny internal certificate authority to the servertls package.
// It exists to back client-certificate ("mtls") authentication: the server
// mints short-lived client certs for callers that have already proved their
// identity by another channel (an SSH public key), and the TLS listener then
// trusts exactly the certs this CA issued.
//
// The CA is deliberately minimal — one self-signed ECDSA P-256 root, no
// intermediates (MaxPathLen 0), no CRL/OCSP — and deliberately strict: unlike
// the server leaf cert above, which is regenerated whenever it fails to load,
// the CA is NEVER silently regenerated. Regenerating it would invalidate every
// client certificate already issued from it, fleet-wide, so any partial or
// suspicious on-disk state is a hard error the operator must resolve.

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

const (
	// caValidity is the lifetime of a generated CA. Client certs are minted
	// with hour-scale TTLs, so the root itself is long-lived; rotation is a
	// deliberate operator act (delete both files, re-enroll every client).
	caValidity = 10 * 365 * 24 * time.Hour

	// caCommonName is the Subject CN of the generated root.
	caCommonName = "shed-ca"

	// caMinRemaining is the remaining CA lifetime below which issuance stops.
	// Handing out a cert from a root that is about to expire produces
	// mysterious handshake failures moments later; refusing loudly instead
	// turns that into an actionable "rotate the CA" message.
	caMinRemaining = 48 * time.Hour

	// clientCertBackdate absorbs clock skew between the issuing server and the
	// verifying peer, which may be a different machine.
	clientCertBackdate = 5 * time.Minute

	// maxCSRBytes caps the DER a client may submit. A P-256 CSR is a few
	// hundred bytes; anything near this cap is abuse, not a request.
	maxCSRBytes = 8 << 10 // 8 KiB

	// caNoRegenAdvice is embedded in every terminal load failure. The CA is
	// never silently replaced, so each message has to say so and hand the
	// operator the two ways out.
	caNoRegenAdvice = "refusing to regenerate (a new CA invalidates every issued client certificate)"

	// caLockSuffix names the advisory lock file guarding load-or-generate,
	// created alongside the CA cert.
	caLockSuffix = ".lock"

	// sshFingerprintPrefix and sshFingerprintBodyLen describe the canonical
	// OpenSSH SHA-256 fingerprint form ("SHA256:" + unpadded base64 of 32
	// bytes = 43 characters), which is what the issued Subject CN must be.
	sshFingerprintPrefix   = "SHA256:"
	sshFingerprintBodyLen  = 43
	sshFingerprintKeyBytes = 32
)

// CSR / issuance failures. These strings are part of the enrollment protocol —
// they are surfaced verbatim to the client (over SSH stderr), so keep them
// stable, short, and free of internal detail.
var (
	// ErrCSRTooLarge is returned when the submitted DER exceeds maxCSRBytes.
	ErrCSRTooLarge = errors.New("csr: too large")
	// ErrCSRInvalidDER is returned when the submission is not exactly one
	// well-formed CertificationRequest (including trailing garbage).
	ErrCSRInvalidDER = errors.New("csr: invalid DER")
	// ErrCSRInvalidSignature is returned when the CSR's self-signature does
	// not verify against the key it carries.
	ErrCSRInvalidSignature = errors.New("csr: invalid signature")
	// ErrCSRUnsupportedKey is returned for any key that is not ECDSA P-256.
	ErrCSRUnsupportedKey = errors.New("csr: unsupported key type (need P-256)")
	// ErrCSRWeakSignature is returned when the CSR is not signed with
	// ECDSA-SHA256 or ECDSA-SHA384.
	ErrCSRWeakSignature = errors.New("csr: weak signature algorithm")
	// ErrCAExpiringSoon is returned when the CA has less than caMinRemaining
	// of life left; issuance stops before the root does.
	ErrCAExpiringSoon = errors.New("ca: expiring soon; rotate the CA")
	// ErrCANotInitialized guards the zero CA value.
	ErrCANotInitialized = errors.New("ca: not initialized")
	// ErrCAInvalidTTL rejects a non-positive requested lifetime.
	ErrCAInvalidTTL = errors.New("ca: ttl must be positive")
	// ErrCAEmptySubject rejects issuance with no caller identity to bind.
	ErrCAEmptySubject = errors.New("ca: empty subject fingerprint")
	// ErrCAInvalidSubject rejects a subject fingerprint that is not in the
	// canonical OpenSSH "SHA256:<43 base64 chars>" form. Unlike the csr:
	// errors this is a caller (server-side) bug, not client input: the
	// fingerprint is derived from the authenticated SSH key, so a malformed
	// one means the enrollment path built the identity wrong.
	ErrCAInvalidSubject = errors.New("ca: subject fingerprint is not a canonical SHA256 SSH fingerprint")
)

// CA is a loaded internal certificate authority: the self-signed root, its
// private key, and the derived material callers need (DER for fingerprinting,
// PEM for reporting, a pool for the TLS listener). The zero value is unusable;
// obtain one from LoadOrGenerateCA.
type CA struct {
	cert    *x509.Certificate
	key     *ecdsa.PrivateKey
	der     []byte
	certPEM []byte
}

// LoadOrGenerateCA returns the CA persisted at certPath/keyPath, generating a
// fresh one only when BOTH files are absent.
//
// Every other state is an error. In particular a load failure never falls back
// to generation: a new CA silently replacing the old one would invalidate every
// client certificate already issued, so a corrupt or half-written CA must be
// resolved by an operator. Rejected states are: exactly one of the two files
// present; malformed PEM or DER; a key that does not match the cert; a cert
// that is not a CA; a cert whose signature does not verify against its own key
// (a tampered TBS); an expired (or not-yet-valid) cert; a key or cert on a
// curve other than P-256; and a key file readable beyond its owner.
//
// Neither path may be a symlink or any other non-regular file: the CA is never
// regenerated over one, and never followed.
//
// On success the key file is normalized to 0600 even when it preexisted.
//
// The whole load-or-generate is serialized by an exclusive advisory lock on
// <certPath>.lock, so two servers starting at once on the same directory cannot
// both decide "no CA yet" and mint competing roots — the loser blocks, then
// loads what the winner wrote.
func LoadOrGenerateCA(certPath, keyPath string) (CA, error) {
	unlock, err := lockCA(certPath)
	if err != nil {
		return CA{}, err
	}
	defer unlock()
	return loadOrGenerateCALocked(certPath, keyPath)
}

// lockCA takes the exclusive advisory lock guarding the CA pair and returns the
// release function. The lock file itself is deliberately left on disk: removing
// it would let a later locker create a fresh inode and lock that instead, while
// an in-flight one still holds the old one.
func lockCA(certPath string) (func(), error) {
	lockPath := certPath + caLockSuffix
	if dir := filepath.Dir(lockPath); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0700); err != nil {
			return nil, fmt.Errorf("create CA dir %s: %w", dir, err)
		}
	}
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, fmt.Errorf("open CA lock %s: %w", lockPath, err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("lock CA %s: %w", lockPath, err)
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}, nil
}

// loadOrGenerateCALocked is LoadOrGenerateCA's body, run with the CA lock held.
func loadOrGenerateCALocked(certPath, keyPath string) (CA, error) {
	certExists, err := caFileExists(certPath)
	if err != nil {
		return CA{}, err
	}
	keyExists, err := caFileExists(keyPath)
	if err != nil {
		return CA{}, err
	}

	switch {
	case certExists && keyExists:
		return loadCA(certPath, keyPath)
	case certExists != keyExists:
		present, missing := certPath, keyPath
		if keyExists {
			present, missing = keyPath, certPath
		}
		return CA{}, fmt.Errorf("CA is half-present: %s exists but %s is missing; %s — "+
			"a previous start that crashed or was killed part-way through writing the CA can leave this state; "+
			"restore %s from backup, or delete %s to mint a fresh CA and re-enroll all clients",
			present, missing, caNoRegenAdvice, missing, present)
	default:
		return generateCA(certPath, keyPath)
	}
}

// Fingerprint returns the pin string for the CA certificate, in the same
// "sha256:<hex>" form clients already use for the server leaf.
func (c CA) Fingerprint() string { return Fingerprint(c.der) }

// NotAfter is the CA certificate's expiry, for /api/info reporting.
func (c CA) NotAfter() time.Time {
	if c.cert == nil {
		return time.Time{}
	}
	return c.cert.NotAfter
}

// CertPEM returns the PEM encoding of the CA certificate. Clients pin by
// fingerprint and never need it; it exists for reporting and for operators
// wiring the CA into other tooling.
func (c CA) CertPEM() []byte { return bytes.Clone(c.certPEM) }

// CertDER returns the DER encoding of the CA certificate.
func (c CA) CertDER() []byte { return bytes.Clone(c.der) }

// Cert returns the parsed CA certificate. Each call hands back a fresh copy of
// the struct, so a caller writing to a field (or to the returned pointer) cannot
// alter the CA every other caller sees. The copy is shallow — the byte slices it
// shares are treated as immutable throughout this package.
func (c CA) Cert() *x509.Certificate {
	if c.cert == nil {
		return nil
	}
	dup := *c.cert
	return &dup
}

// Pool returns a certificate pool containing just this CA, for use as the TLS
// listener's ClientCAs. Each call returns a fresh pool so a caller mutating it
// cannot widen the trust of another.
func (c CA) Pool() *x509.CertPool {
	pool := x509.NewCertPool()
	if c.cert != nil {
		pool.AddCert(c.cert)
	}
	return pool
}

// SignClientCSR issues a short-lived client certificate for a CSR.
//
// The CSR is treated as a carrier for one thing only: the requester's public
// key. Every subject field, SAN, extension, and attribute it requests is
// ignored — a client cannot influence the identity it is issued. The issued
// identity comes solely from the arguments, which the caller derives from the
// already-authenticated channel:
//
//	Subject.CommonName         = subjectFingerprint (the caller's SSH key fingerprint)
//	Subject.OrganizationalUnit = scope
//	Subject.Organization       = clientKind
//
// subjectFingerprint must be a canonical OpenSSH SHA-256 fingerprint
// ("SHA256:" + 43 base64 characters); scope and clientKind are omitted when
// empty. The issued cert carries no SANs and no extensions beyond
// basicConstraints (CA:FALSE), keyUsage (digitalSignature), extKeyUsage
// (clientAuth), the subjectKeyIdentifier derived from the requested key, and
// the authorityKeyIdentifier naming the CA.
//
// NotAfter is now+ttl, clamped to the CA's own expiry; issuance is refused
// outright when the CA has less than 48h of life left.
func (c CA) SignClientCSR(csrDER []byte, subjectFingerprint, scope, clientKind string, ttl time.Duration) ([]byte, error) {
	if c.cert == nil || c.key == nil {
		return nil, ErrCANotInitialized
	}
	if subjectFingerprint == "" {
		return nil, ErrCAEmptySubject
	}
	if !validSSHFingerprint(subjectFingerprint) {
		return nil, ErrCAInvalidSubject
	}
	if ttl <= 0 {
		return nil, ErrCAInvalidTTL
	}

	pub, err := validateClientCSR(csrDER)
	if err != nil {
		return nil, err
	}
	notBefore, notAfter, err := c.issuanceWindow(ttl)
	if err != nil {
		return nil, err
	}
	serial, err := randomSerial()
	if err != nil {
		return nil, err
	}
	skid, err := subjectKeyID(pub)
	if err != nil {
		return nil, err
	}

	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               clientSubject(subjectFingerprint, scope, clientKind),
		NotBefore:             notBefore,
		NotAfter:              notAfter,
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		BasicConstraintsValid: true,
		IsCA:                  false,
		// The standard library only derives a subjectKeyIdentifier for CA
		// certificates, so leaf SKIDs are computed here; the matching
		// authorityKeyIdentifier is added by CreateCertificate from the CA's
		// own SKID.
		SubjectKeyId: skid,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, c.cert, pub, c.key)
	if err != nil {
		return nil, fmt.Errorf("issue client certificate: %w", err)
	}
	return der, nil
}

// issuanceWindow returns the validity window for a certificate minted now with
// the requested ttl: backdated by clientCertBackdate for clock skew, and never
// outliving the CA that signs it. Issuance is refused once the CA has less than
// caMinRemaining left — a cert from an about-to-expire root would fail
// handshakes moments later for no visible reason.
func (c CA) issuanceWindow(ttl time.Duration) (notBefore, notAfter time.Time, err error) {
	now := time.Now()
	if c.cert.NotAfter.Sub(now) < caMinRemaining {
		return time.Time{}, time.Time{}, ErrCAExpiringSoon
	}
	notAfter = now.Add(ttl)
	if notAfter.After(c.cert.NotAfter) {
		notAfter = c.cert.NotAfter
	}
	return now.Add(-clientCertBackdate), notAfter, nil
}

// subjectKeyID derives a leaf certificate's subjectKeyIdentifier from its
// public key using RFC 7093 §2 method 1: SHA-256 over the SPKI subjectPublicKey
// BIT STRING, truncated to the leftmost 160 bits. (RFC 5280's own method 1 uses
// SHA-1 over the same input, which is what the standard library does for CA
// certs; the identifier is a lookup hint, not a security property, but there is
// no reason to introduce a new SHA-1 use here.)
func subjectKeyID(pub *ecdsa.PublicKey) ([]byte, error) {
	spkiDER, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		return nil, fmt.Errorf("marshal client public key: %w", err)
	}
	var spki struct {
		Algorithm pkix.AlgorithmIdentifier
		PublicKey asn1.BitString
	}
	if _, err := asn1.Unmarshal(spkiDER, &spki); err != nil {
		return nil, fmt.Errorf("parse client public key: %w", err)
	}
	sum := sha256.Sum256(spki.PublicKey.Bytes)
	return sum[:20], nil
}

// validSSHFingerprint reports whether s is a canonical OpenSSH SHA-256
// fingerprint: the literal "SHA256:" followed by exactly 43 characters of
// unpadded standard base64 decoding to 32 bytes. The caller derives this string
// from the already-authenticated SSH key, so anything else is a server-side bug
// (ErrCAInvalidSubject), not hostile client input — but the CN ends up in a
// certificate that authorization decisions are made against, so it is checked
// rather than trusted.
func validSSHFingerprint(s string) bool {
	body, ok := strings.CutPrefix(s, sshFingerprintPrefix)
	if !ok || len(body) != sshFingerprintBodyLen {
		return false
	}
	raw, err := base64.RawStdEncoding.Strict().DecodeString(body)
	return err == nil && len(raw) == sshFingerprintKeyBytes
}

// clientSubject composes the issued Subject from the caller-supplied identity
// alone — the CSR's own subject is never consulted. Empty scope/kind are
// omitted rather than encoded as empty attributes.
func clientSubject(fingerprint, scope, clientKind string) pkix.Name {
	subject := pkix.Name{CommonName: fingerprint}
	if scope != "" {
		subject.OrganizationalUnit = []string{scope}
	}
	if clientKind != "" {
		subject.Organization = []string{clientKind}
	}
	return subject
}

// validateClientCSR runs the fixed validation ladder over a submitted CSR and
// returns the public key to certify. The order is part of the protocol: each
// step has its own stable error, and cheaper/structural checks come first so a
// malformed submission never reaches the crypto.
func validateClientCSR(csrDER []byte) (*ecdsa.PublicKey, error) {
	// (a) size cap.
	if len(csrDER) > maxCSRBytes {
		return nil, ErrCSRTooLarge
	}
	// (b) exactly one DER element, no trailing bytes. ParseCertificateRequest
	// rejects trailing data today; the explicit check keeps that guarantee
	// independent of the standard library's internals.
	//
	// The boundary of this check, precisely: exactness is enforced on the OUTER
	// SEQUENCE only — one CertificationRequest, nothing before or after it. It
	// is not a recursive DER-canonicality audit of the interior, and it is not
	// meant to be one. Go's parser keeps CSR attributes opaque (RawAttributes /
	// Attributes), and issuance never reads them: the requested subject, SANs,
	// extensions, and attributes are all discarded, and the issued identity
	// comes solely from clientSubject's arguments. The single field taken from
	// the CSR is the public key, which the standard library parses and step (d)
	// constrains to P-256. So BER-encoded content smuggled inside an opaque
	// attribute has nothing to influence — it is never decoded and never
	// reaches the issued certificate.
	//
	// Rejecting CSRs that carry any attribute at all is therefore a deliberate
	// deferral rather than a gap: it waits until the in-repo Rust client's CSR
	// output is pinned (plan S10), at which point "no attributes beyond the
	// empty set" becomes a cheap tightening with a known-good client to test
	// against.
	var raw asn1.RawValue
	rest, err := asn1.Unmarshal(csrDER, &raw)
	if err != nil || len(rest) != 0 {
		return nil, ErrCSRInvalidDER
	}
	csr, err := x509.ParseCertificateRequest(csrDER)
	if err != nil {
		return nil, ErrCSRInvalidDER
	}
	// (c) proof of possession.
	if err := csr.CheckSignature(); err != nil {
		return nil, ErrCSRInvalidSignature
	}
	// (d) key type: ECDSA P-256 exactly.
	pub, ok := csr.PublicKey.(*ecdsa.PublicKey)
	if !ok || pub.Curve != elliptic.P256() {
		return nil, ErrCSRUnsupportedKey
	}
	// (e) signature algorithm. CheckSignature above still accepts SHA-1 for
	// CSRs, so the allow-list is enforced separately.
	switch csr.SignatureAlgorithm {
	case x509.ECDSAWithSHA256, x509.ECDSAWithSHA384:
	default:
		return nil, ErrCSRWeakSignature
	}
	return pub, nil
}

// newCA assembles the loaded CA value from its parsed parts.
func newCA(cert *x509.Certificate, der []byte, key *ecdsa.PrivateKey) CA {
	return CA{
		cert:    cert,
		key:     key,
		der:     der,
		certPEM: encodeCertPEM(der),
	}
}

// loadCA reads and validates a persisted CA. Any failure is terminal — see
// LoadOrGenerateCA — so every error names the file and says what to do.
func loadCA(certPath, keyPath string) (CA, error) {
	keyPEMBytes, perm, err := readCAKey(keyPath)
	if err != nil {
		return CA{}, err
	}

	certPEMBytes, err := os.ReadFile(certPath)
	if err != nil {
		return CA{}, fmt.Errorf("read CA cert %s: %w", certPath, err)
	}

	cert, der, err := parseCACertPEM(certPEMBytes)
	if err != nil {
		return CA{}, fmt.Errorf("CA cert %s: %w", certPath, err)
	}
	key, err := parseCAKeyPEM(keyPEMBytes)
	if err != nil {
		return CA{}, fmt.Errorf("CA key %s: %w", keyPath, err)
	}

	if err := checkCAMaterial(cert, key); err != nil {
		return CA{}, fmt.Errorf("CA at %s/%s: %w; %s — "+
			"restore a good CA from backup, or delete both files to mint a fresh one and re-enroll all clients",
			certPath, keyPath, err, caNoRegenAdvice)
	}

	// Normalize only once the material is known good, so a failed load leaves
	// the on-disk state untouched for the operator to inspect.
	if perm != 0o600 {
		if err := os.Chmod(keyPath, 0o600); err != nil {
			return CA{}, fmt.Errorf("tighten CA key permissions on %s: %w", keyPath, err)
		}
	}
	return newCA(cert, der, key), nil
}

// readCAKey reads the key file and returns its bytes plus its permission bits,
// rejecting a key any other user can read. Owner-only exec/read bits are
// tolerated (the caller normalizes them to 0600 after a successful load);
// group/other bits are not, since by then the key may already have been copied.
//
// The permission check and the read go through one open file handle, so the
// bytes returned are provably the bytes of the file that was checked — a
// stat-then-open pair could be raced by swapping the path in between. O_NOFOLLOW
// makes the "regular file, not a symlink" rule enforced by the kernel at open
// time rather than by the earlier Lstat alone.
func readCAKey(keyPath string) ([]byte, os.FileMode, error) {
	f, err := os.OpenFile(keyPath, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, 0, fmt.Errorf("open CA key %s: %w", keyPath, err)
	}
	defer func() { _ = f.Close() }()

	info, err := f.Stat()
	if err != nil {
		return nil, 0, fmt.Errorf("stat CA key %s: %w", keyPath, err)
	}
	if !info.Mode().IsRegular() {
		return nil, 0, fmt.Errorf("ca: %s is not a regular file (mode %s); CA files must be regular files",
			keyPath, info.Mode().Type())
	}
	perm := info.Mode().Perm()
	if perm&0o077 != 0 {
		return nil, 0, fmt.Errorf("CA key %s has permissions %04o (readable beyond its owner): "+
			"run `chmod 600 %s`, and rotate the CA if the key may have leaked", keyPath, perm, keyPath)
	}
	data, err := io.ReadAll(f)
	if err != nil {
		return nil, 0, fmt.Errorf("read CA key %s: %w", keyPath, err)
	}
	return data, perm, nil
}

// checkCAMaterial verifies the loaded pair is a usable P-256 CA: matching key,
// CA basic constraints, cert-signing key usage, and currently within validity.
func checkCAMaterial(cert *x509.Certificate, key *ecdsa.PrivateKey) error {
	pub, ok := cert.PublicKey.(*ecdsa.PublicKey)
	if !ok {
		return fmt.Errorf("public key is %T, want ECDSA P-256", cert.PublicKey)
	}
	if pub.Curve != elliptic.P256() {
		return fmt.Errorf("certificate uses curve %s, want P-256", curveName(pub.Curve))
	}
	if key.Curve != elliptic.P256() {
		return fmt.Errorf("private key uses curve %s, want P-256", curveName(key.Curve))
	}
	if !pub.Equal(key.Public()) {
		return errors.New("private key does not match the certificate")
	}
	if !cert.BasicConstraintsValid || !cert.IsCA {
		return errors.New("certificate is not a CA (basicConstraints CA:TRUE missing)")
	}
	if cert.KeyUsage&x509.KeyUsageCertSign == 0 {
		return errors.New("certificate lacks the keyCertSign key usage")
	}
	// This root is self-signed by construction and has no issuer above it, so
	// nothing else would ever check its signature: a tampered TBS (a widened
	// subject, a stretched NotAfter) would otherwise load happily. Verifying it
	// against itself makes the certificate's own contents self-authenticating.
	if err := cert.CheckSignatureFrom(cert); err != nil {
		return fmt.Errorf("certificate is not validly self-signed: %w", err)
	}
	now := time.Now()
	if now.After(cert.NotAfter) {
		return fmt.Errorf("certificate expired at %s", cert.NotAfter.UTC().Format(time.RFC3339))
	}
	if now.Before(cert.NotBefore) {
		return fmt.Errorf("certificate is not valid until %s", cert.NotBefore.UTC().Format(time.RFC3339))
	}
	return nil
}

// curveName renders a curve for error messages without panicking on nil.
func curveName(c elliptic.Curve) string {
	if c == nil || c.Params() == nil {
		return "unknown"
	}
	return c.Params().Name
}

// parseCACertPEM decodes exactly one CERTIFICATE block and parses it.
func parseCACertPEM(data []byte) (*x509.Certificate, []byte, error) {
	block, rest := pem.Decode(data)
	if block == nil {
		return nil, nil, errors.New("no PEM block found")
	}
	if block.Type != "CERTIFICATE" {
		return nil, nil, fmt.Errorf("unexpected PEM block %q, want CERTIFICATE", block.Type)
	}
	if len(bytes.TrimSpace(rest)) != 0 {
		return nil, nil, errors.New("trailing data after the CERTIFICATE block")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, nil, fmt.Errorf("parse certificate: %w", err)
	}
	return cert, block.Bytes, nil
}

// parseCAKeyPEM decodes exactly one EC private key block, accepting both the
// SEC 1 form this package writes and PKCS#8 (which an operator-supplied key or
// another tool may produce).
func parseCAKeyPEM(data []byte) (*ecdsa.PrivateKey, error) {
	block, rest := pem.Decode(data)
	if block == nil {
		return nil, errors.New("no PEM block found")
	}
	if len(bytes.TrimSpace(rest)) != 0 {
		return nil, errors.New("trailing data after the private key block")
	}
	switch block.Type {
	case "EC PRIVATE KEY":
		key, err := x509.ParseECPrivateKey(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("parse EC private key: %w", err)
		}
		return key, nil
	case "PRIVATE KEY":
		parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("parse PKCS#8 private key: %w", err)
		}
		key, ok := parsed.(*ecdsa.PrivateKey)
		if !ok {
			return nil, fmt.Errorf("private key is %T, want ECDSA P-256", parsed)
		}
		return key, nil
	default:
		return nil, fmt.Errorf("unexpected PEM block %q, want EC PRIVATE KEY", block.Type)
	}
}

// generateCA mints and persists a fresh self-signed P-256 root.
func generateCA(certPath, keyPath string) (CA, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return CA{}, fmt.Errorf("generate CA key: %w", err)
	}
	serial, err := randomSerial()
	if err != nil {
		return CA{}, err
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: caCommonName},
		NotBefore:    now.Add(-clientCertBackdate),
		NotAfter:     now.Add(caValidity),
		// CRLSign is not used today; including it keeps a future revocation
		// list issuable from the same root without re-enrolling every client.
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
		// Leaf-only: this root may not issue intermediates.
		MaxPathLen:     0,
		MaxPathLenZero: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return CA{}, fmt.Errorf("create CA certificate: %w", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return CA{}, fmt.Errorf("parse generated CA certificate: %w", err)
	}
	if err := persistCA(certPath, keyPath, der, key); err != nil {
		return CA{}, err
	}
	return newCA(cert, der, key), nil
}

// persistCA writes the CA key (0600) and cert (0644) atomically. A failure
// part-way leaves no half-written pair behind: each file is staged in the same
// directory, fsynced, and renamed into place, and the key is rolled back if the
// cert cannot be written.
//
// The key is committed first on purpose. Both orders can be interrupted between
// the two renames, and both leave a half-present state that the loader rejects
// — but a key with no certificate is inert (nothing can be issued or verified
// from it), whereas a certificate with no key is the shape of a live CA whose
// private key has gone missing, which is a far more alarming thing to find and
// a far easier one to mistake for a compromise. Under the LoadOrGenerateCA
// lock the rollback below can no longer race a concurrent starter.
func persistCA(certPath, keyPath string, der []byte, key *ecdsa.PrivateKey) error {
	for _, dir := range []string{filepath.Dir(certPath), filepath.Dir(keyPath)} {
		if dir == "" || dir == "." {
			continue
		}
		if err := os.MkdirAll(dir, 0700); err != nil {
			return fmt.Errorf("create CA dir %s: %w", dir, err)
		}
	}

	keyPEM, err := encodeECKeyPEM(key)
	if err != nil {
		return err
	}

	if err := atomicWriteFile(keyPath, keyPEM, 0600); err != nil {
		return fmt.Errorf("write CA key %s: %w", keyPath, err)
	}
	if err := atomicWriteFile(certPath, encodeCertPEM(der), 0644); err != nil {
		// Roll the key back so the next start sees "no CA" (and regenerates)
		// rather than the half-present state, which is a hard error.
		_ = os.Remove(keyPath)
		return fmt.Errorf("write CA cert %s: %w", certPath, err)
	}
	return nil
}

// atomicWriteFile writes data to path via a temp file in the same directory,
// fsynced and renamed, so a reader never observes a partial file and a crash
// leaves either the old contents or the new ones.
func atomicWriteFile(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp*")
	if err != nil {
		return fmt.Errorf("create temp file in %s: %w", dir, err)
	}
	tmp := f.Name()
	committed := false
	defer func() {
		if !committed {
			_ = f.Close()
			_ = os.Remove(tmp)
		}
	}()

	if err := f.Chmod(perm); err != nil {
		return fmt.Errorf("chmod temp file: %w", err)
	}
	if _, err := f.Write(data); err != nil {
		return fmt.Errorf("write temp file: %w", err)
	}
	if err := f.Sync(); err != nil {
		return fmt.Errorf("fsync temp file: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close temp file: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("rename temp file into place: %w", err)
	}
	committed = true

	// Persist the rename itself. Best effort: some filesystems reject fsync on
	// a directory handle, and the rename has already succeeded.
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}

// randomSerial returns a cryptographically random serial in [1, 2^128-1].
func randomSerial() (*big.Int, error) {
	limit := new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 128), big.NewInt(1))
	n, err := rand.Int(rand.Reader, limit)
	if err != nil {
		return nil, fmt.Errorf("generate serial: %w", err)
	}
	return n.Add(n, big.NewInt(1)), nil
}

// caFileExists reports whether path exists as a regular file. A stat error
// other than "not exist" is returned rather than treated as absence, so an
// unreadable directory can never be mistaken for "no CA yet" and trigger
// regeneration.
//
// The stat is an Lstat, and a symlink is a hard error rather than a followed
// link: a symlinked CA path is either an operator surprise or an attempt to
// redirect where the private key is read from or written to, and neither is
// worth guessing about when the cost of guessing wrong is regenerating (or
// leaking) the root. Directories, sockets, devices and the rest are rejected by
// the same regular-file rule.
func caFileExists(path string) (bool, error) {
	info, err := os.Lstat(path)
	switch {
	case err == nil:
		mode := info.Mode()
		if mode&os.ModeSymlink != 0 {
			return false, fmt.Errorf("ca: %s is a symlink; CA files must be regular files", path)
		}
		if !mode.IsRegular() {
			return false, fmt.Errorf("ca: %s is not a regular file (mode %s); CA files must be regular files",
				path, mode.Type())
		}
		return true, nil
	case errors.Is(err, os.ErrNotExist):
		return false, nil
	default:
		return false, fmt.Errorf("stat %s: %w", path, err)
	}
}
