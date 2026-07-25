package config

// clientcreds.go owns the on-disk client-certificate store: the material an
// mtls-mode server issues over the SSH bootstrap channel, kept out of
// config.yaml.
//
// Layout, one directory per configured server:
//
//	~/.shed/creds/                 0700
//	~/.shed/creds/<server>/        0700
//	~/.shed/creds/<server>/client.pem   0600  (the issued leaf)
//	~/.shed/creds/<server>/client.key   0600  (the private key)
//	~/.shed/creds/%lock/<server>        0600  (advisory lock, see credsLockDirName)
//
// The certificate is public material and 0644 would be defensible, but it is
// written 0600 alongside the key: nothing needs to read it but this client, and
// a uniform rule is one fewer thing to get wrong when the pair is rewritten on
// every rotation.

import (
	"crypto"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"syscall"
)

const (
	// clientCertFileName / clientKeyFileName are the fixed basenames inside a
	// server's creds dir. Fixed rather than serial-stamped: rotation replaces
	// the pair in place, so there is never a second generation to name, and no
	// stale keys accumulate for an attacker to find or an operator to wonder at.
	clientCertFileName = "client.pem"
	clientKeyFileName  = "client.key"

	// credsDirPerm / credsFilePerm are the permissions enforced on write.
	credsDirPerm  os.FileMode = 0700
	credsFilePerm os.FileMode = 0600

	// credsLockDirName holds one advisory lock file per server, as a SIBLING of
	// the per-server credential directories rather than a file inside them.
	//
	// Sibling placement buys two things. RemoveServerCredentials recursively
	// deletes a server's directory, and deleting a lock file out from under a
	// process that holds it would let the next locker create a fresh inode and
	// lock THAT instead — the classic broken-mutex shape (see the same note on
	// servertls.lockCA). And the per-server directory stays exactly the two
	// files it documents, with nothing to explain to an operator who looks.
	//
	// The "%" prefix is what makes the name collision-proof. Directory names in
	// the creds root come from escapeServerName, i.e. url.PathEscape, which
	// emits "%" only as the start of a two-hex-digit escape — "%l" is not one,
	// so no server name, however chosen, can ever escape to "%lock".
	credsLockDirName = "%lock"
)

// ErrCredentialPairMismatch reports a stored certificate and private key that
// do not belong together.
//
// It is a RECOVERABLE state, not a corruption to report to the user: a crash
// between the two renames of a rotation leaves exactly this, and so does an
// interrupted restore. Callers treat it as "no credential" and re-enroll — see
// LoadClientCredentials.
var ErrCredentialPairMismatch = errors.New("client credentials: the stored certificate does not match the stored private key")

// GetCredsDir returns the root of the client-credential store (~/.shed/creds).
func GetCredsDir() string {
	return filepath.Join(GetClientConfigDir(), "creds")
}

// ServerCredsDir returns the credential directory for a named server entry.
//
// The name is escaped because it is user-chosen and otherwise unconstrained: an
// entry called "../../.ssh" must not be able to steer a 0600 write — or the
// recursive delete in RemoveServerCredentials — outside the creds root.
// Escaping (rather than rejecting) keeps the mapping total, so no name that is
// legal everywhere else in the config becomes unusable here.
func ServerCredsDir(name string) string {
	return filepath.Join(GetCredsDir(), escapeServerName(name))
}

// escapeServerName maps a server name onto exactly one inert path component.
//
// url.PathEscape does most of it — it encodes "/" and every other separator —
// but it deliberately leaves "." alone, because a dot is a perfectly legal path
// character. That is fine for a name embedded in a longer filename (see
// GetTunnelLogPath, which always surrounds it with a prefix and suffix) and NOT
// fine here, where the escaped name is the whole component: a server named ".."
// would resolve ~/.shed/creds/.. to ~/.shed, and RemoveServerCredentials would
// then recursively delete the user's entire shed configuration — known hosts,
// tunnels, every other server's credentials.
//
// Only the exact components ""/"."/".." carry that meaning; a name like "..foo"
// is already inert. So those three get a leading "%2E", which keeps the result a
// single, ordinary directory name, keeps the mapping injective, and leaves every
// realistic server name untouched.
func escapeServerName(name string) string {
	esc := url.PathEscape(name)
	switch esc {
	case "", ".", "..":
		return "%2E" + esc
	}
	return esc
}

// ClientCredentialPaths returns the certificate and key paths for a named
// server, without touching the filesystem.
func ClientCredentialPaths(name string) (certPath, keyPath string) {
	dir := ServerCredsDir(name)
	return filepath.Join(dir, clientCertFileName), filepath.Join(dir, clientKeyFileName)
}

// ensureCredsRoot creates ~/.shed/creds and tightens it to 0700.
//
// The chmod is not redundant with the MkdirAll: MkdirAll is a no-op on a
// directory that already exists, so a root left group- or world-readable by an
// older build, a careless restore, or a permissive umask would keep those
// permissions forever — and every private key in the store sits under it. The
// same reasoning already applies one level down, to the per-server directory.
func ensureCredsRoot() error {
	root := GetCredsDir()
	if err := os.MkdirAll(root, credsDirPerm); err != nil {
		return fmt.Errorf("create credentials dir %s: %w", root, err)
	}
	if err := os.Chmod(root, credsDirPerm); err != nil {
		return fmt.Errorf("tighten credentials dir %s: %w", root, err)
	}
	return nil
}

// lockServerCreds takes the exclusive advisory lock guarding one server's
// credential pair and returns the release function.
//
// It is held across the WHOLE write (both renames) and across the whole load,
// which is what makes a rotation atomic as a PAIR rather than merely per file.
// Each file is already written atomically, but two independent renames are two
// independent commits: without this lock a reader can observe the new
// certificate beside the old key, and two processes refreshing at once can
// interleave into cert-A-with-key-B — a credential that belongs to nobody and
// fails the handshake at some later, unrelated moment.
//
// A lock file is never removed once created (see credsLockDirName), and an
// empty name means "no identifiable server", which takes no lock at all: a
// nameless entry is never written by the persist path either, so there is
// nothing to serialize against.
func lockServerCreds(name string) (func(), error) {
	if name == "" {
		return func() {}, nil
	}
	if err := ensureCredsRoot(); err != nil {
		return nil, err
	}
	lockDir := filepath.Join(GetCredsDir(), credsLockDirName)
	if err := os.MkdirAll(lockDir, credsDirPerm); err != nil {
		return nil, fmt.Errorf("create credentials lock dir %s: %w", lockDir, err)
	}
	lockPath := filepath.Join(lockDir, escapeServerName(name))
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, credsFilePerm)
	if err != nil {
		return nil, fmt.Errorf("open credentials lock %s: %w", lockPath, err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("lock credentials %s: %w", lockPath, err)
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}, nil
}

// LoadClientCredentials reads a server's stored certificate + key under that
// server's credential lock and assembles them for the TLS stack.
//
// It returns (nil, err) for EVERY unusable state — absent files, unreadable
// files, malformed PEM, and a certificate that does not match the key — because
// all of them mean the same thing to the caller: there is no credential to
// present, so re-enroll. In particular a mismatched pair is
// ErrCredentialPairMismatch rather than a hard failure: it is what a crash
// between the two renames of a rotation leaves behind, and the recovery for it
// is a fresh enrollment, not an operator with a text editor.
//
// name identifies the lock, not the paths: the paths come from the config entry
// (which the user may have edited) while the lock is keyed on the entry's name,
// matching what WriteClientCredentials holds. An empty name skips the lock.
func LoadClientCredentials(name, certPath, keyPath string) (*tls.Certificate, error) {
	if certPath == "" || keyPath == "" {
		return nil, errors.New("client credentials: no client certificate on file")
	}
	unlock, err := lockServerCreds(name)
	if err != nil {
		return nil, err
	}
	defer unlock()

	certPEM, err := os.ReadFile(certPath)
	if err != nil {
		return nil, fmt.Errorf("read client certificate: %w", err)
	}
	keyPEM, err := os.ReadFile(keyPath)
	if err != nil {
		return nil, fmt.Errorf("read client key: %w", err)
	}
	if err := verifyPairMatches(certPEM, keyPEM); err != nil {
		return nil, err
	}
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, fmt.Errorf("assemble client credentials: %w", err)
	}
	return &cert, nil
}

// verifyPairMatches reports whether certPEM certifies the public half of
// keyPEM.
//
// tls.X509KeyPair performs the same comparison internally, but only reports it
// as an opaque error string. Doing it here first is what lets the mismatch be
// classified — as the recoverable ErrCredentialPairMismatch — instead of being
// indistinguishable from a genuinely malformed file.
func verifyPairMatches(certPEM, keyPEM []byte) error {
	block, _ := pem.Decode(certPEM)
	if block == nil || block.Type != "CERTIFICATE" {
		return fmt.Errorf("%w: the certificate file is not a PEM CERTIFICATE block", ErrCredentialPairMismatch)
	}
	leaf, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrCredentialPairMismatch, err)
	}
	keyPub, err := publicKeyOfPEM(keyPEM)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrCredentialPairMismatch, err)
	}
	// crypto.PublicKey is an empty interface, but every standard-library public
	// key type carries this Equal method — the idiomatic way to compare two keys
	// without switching on their concrete types.
	pub, ok := leaf.PublicKey.(interface{ Equal(crypto.PublicKey) bool })
	if !ok || !pub.Equal(keyPub) {
		return ErrCredentialPairMismatch
	}
	return nil
}

// publicKeyOfPEM extracts the public half of a PEM-encoded private key. The
// client's own enrollment always writes SEC 1 EC, but the other two standard
// encodings are accepted so a hand-restored or externally-provisioned key is
// compared rather than rejected as unparseable.
func publicKeyOfPEM(keyPEM []byte) (crypto.PublicKey, error) {
	block, _ := pem.Decode(keyPEM)
	if block == nil {
		return nil, errors.New("the key file is not PEM")
	}
	if k, err := x509.ParseECPrivateKey(block.Bytes); err == nil {
		return k.Public(), nil
	}
	if k, err := x509.ParsePKCS8PrivateKey(block.Bytes); err == nil {
		if signer, ok := k.(crypto.Signer); ok {
			return signer.Public(), nil
		}
	}
	if k, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return k.Public(), nil
	}
	return nil, errors.New("unrecognized private key encoding")
}

// WriteClientCredentials persists a freshly issued client certificate and its
// private key for the named server, returning the paths written.
//
// The key is written FIRST and the certificate second. Both orders can be
// interrupted, and both leave a mismatched pair that the next load rejects —
// but a key with no certificate is inert, whereas a certificate with no key is
// the shape of a credential whose private half has gone missing. (Same
// reasoning as servertls.persistCA, deliberately.)
//
// Each file is written atomically (temp file in the same directory, fsynced,
// renamed) so a concurrent reader never observes a half-written PEM. Atomicity
// per FILE is not enough on its own, though — two renames are two commits — so
// the whole write runs under the server's exclusive credential lock, which is
// what stops two processes rotating at once from interleaving into a cert from
// one and a key from the other. LoadClientCredentials takes the same lock, and
// additionally verifies the pair, which covers the one case no lock can: a
// crash BETWEEN the two renames.
func WriteClientCredentials(name string, certPEM, keyPEM []byte) (certPath, keyPath string, err error) {
	if name == "" {
		return "", "", errors.New("client credentials: server name required")
	}
	if len(certPEM) == 0 || len(keyPEM) == 0 {
		return "", "", errors.New("client credentials: empty certificate or key")
	}
	if err := ensureCredsRoot(); err != nil {
		return "", "", err
	}
	unlock, err := lockServerCreds(name)
	if err != nil {
		return "", "", err
	}
	defer unlock()

	dir := ServerCredsDir(name)
	if err := os.MkdirAll(dir, credsDirPerm); err != nil {
		return "", "", fmt.Errorf("create credentials dir %s: %w", dir, err)
	}
	// MkdirAll is a no-op on an existing directory, including one created with
	// looser permissions by an older build or a careless restore. Tighten it.
	if err := os.Chmod(dir, credsDirPerm); err != nil {
		return "", "", fmt.Errorf("tighten credentials dir %s: %w", dir, err)
	}

	certPath, keyPath = ClientCredentialPaths(name)
	if err := atomicWriteFile(keyPath, keyPEM, credsFilePerm); err != nil {
		return "", "", fmt.Errorf("write client key %s: %w", keyPath, err)
	}
	if err := atomicWriteFile(certPath, certPEM, credsFilePerm); err != nil {
		// Roll the key back so the next start sees "no credentials" (and
		// re-enrolls) rather than a key that certifies nothing.
		_ = os.Remove(keyPath)
		return "", "", fmt.Errorf("write client certificate %s: %w", certPath, err)
	}
	return certPath, keyPath, nil
}

// RemoveServerCredentials deletes a server's credential directory. It is called
// by `shed server rm`: leaving a private key behind for a server the user has
// explicitly forgotten is exactly the kind of quiet residue that turns up years
// later. A missing directory is not an error.
func RemoveServerCredentials(name string) error {
	if name == "" {
		return nil
	}
	dir := ServerCredsDir(name)
	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("remove credentials dir %s: %w", dir, err)
	}
	return nil
}

// atomicWriteFile writes data to path via a temp file in the same directory,
// fsynced and renamed. The temp file is created with the final permissions
// before any bytes are written, so the key is never briefly world-readable.
//
// It is the package's single atomic writer — the egress user-profile store
// (egress_userstore.go) shares it rather than keeping a second, weaker copy.
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

	// Persist the rename itself. Best effort: some filesystems reject fsync on a
	// directory handle, and the rename has already succeeded.
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}
