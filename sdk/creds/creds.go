// Package creds owns the on-disk client-credential store: the certificate +
// private key an mtls-mode shed-server issues over the SSH bootstrap channel,
// kept out of any config file.
//
// It is a Store rooted at a caller-supplied directory rather than a set of
// package functions bound to one location, because the two processes that hold
// shed client credentials must NOT share a directory:
//
//	shed CLI     control-scope credential   ~/.shed/creds/<server>/
//	host-agent   credentials-scope one      its own state dir
//
// One certificate carries one scope, and two processes rotating independently
// would otherwise overwrite each other's material in place. Parameterizing the
// root is what lets both use this one implementation — the 0700/0600
// permissions, the atomic per-file write, and the per-server advisory lock that
// makes a rotation atomic as a PAIR — instead of a second, weaker copy.
//
// Layout, one directory per server entry:
//
//	<root>/<escaped-name>/client.pem   0600  the issued certificate
//	<root>/<escaped-name>/client.key   0600  the matching EC private key
//	<root>/%lock/<escaped-name>        0600  the per-server advisory lock
//	<root>/%staging/<escaped-name>.*/  0700  not-yet-adopted material
//
// The certificate is public material and 0644 would be defensible, but it is
// written 0600 alongside the key: nothing needs to read it but its owner, and a
// uniform rule is one fewer thing to get wrong when the pair is rewritten on
// every rotation.
package creds

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
	// CertFileName / KeyFileName are the fixed basenames inside a server's
	// credential directory. Fixed rather than serial-stamped: rotation replaces
	// the pair in place, so there is never a second generation to name, and no
	// stale keys accumulate for an attacker to find or an operator to wonder at.
	CertFileName = "client.pem"
	KeyFileName  = "client.key"

	// DirPerm / FilePerm are the permissions enforced on write.
	DirPerm  os.FileMode = 0700
	FilePerm os.FileMode = 0600

	// LockDirName holds one advisory lock file per server, as a SIBLING of the
	// per-server credential directories rather than a file inside them.
	//
	// Sibling placement buys two things. Remove recursively deletes a server's
	// directory, and deleting a lock file out from under a process that holds it
	// would let the next locker create a fresh inode and lock THAT instead — the
	// classic broken-mutex shape. And the per-server directory stays exactly the
	// two files it documents, with nothing to explain to an operator who looks.
	//
	// The "%" prefix is what makes the name collision-proof. Directory names in
	// the root come from EscapeServerName, i.e. url.PathEscape, which emits "%"
	// only as the start of a two-hex-digit escape — "%l" is not one, so no server
	// name, however chosen, can ever escape to "%lock".
	LockDirName = "%lock"

	// StagingDirName holds credential material that has been written but not yet
	// adopted — see Store.Stage.
	//
	// It is a sibling of the per-server directories for the same two reasons as
	// LockDirName: Remove must not be able to delete it out from under a staging
	// write, and a per-server directory stays exactly the two files it documents.
	// The "%" prefix is collision-proof by the same argument ("%s" is not a
	// two-hex-digit escape).
	StagingDirName = "%staging"
)

// ErrPairMismatch reports a stored certificate and private key that do not
// belong together.
//
// It is a RECOVERABLE state, not a corruption to report to the user: a crash
// between the two renames of a rotation leaves exactly this, and so does an
// interrupted restore. Callers treat it as "no credential" and re-enroll — see
// Store.Load.
var ErrPairMismatch = errors.New("client credentials: the stored certificate does not match the stored private key")

// Store is a credential store rooted at one directory. The zero value is not
// usable; construct with NewStore.
type Store struct{ root string }

// NewStore returns the store rooted at dir. The directory is created lazily, on
// the first write, so constructing a Store never touches the filesystem.
func NewStore(root string) *Store { return &Store{root: root} }

// Root returns the store's root directory.
func (s *Store) Root() string { return s.root }

// ServerDir returns the credential directory for a named server entry.
//
// The name is escaped because it is user-chosen and otherwise unconstrained: an
// entry called "../../.ssh" must not be able to steer a 0600 write — or the
// recursive delete in Remove — outside the root. Escaping (rather than
// rejecting) keeps the mapping total, so no name that is legal everywhere else
// in a config becomes unusable here.
func (s *Store) ServerDir(name string) string {
	return filepath.Join(s.root, EscapeServerName(name))
}

// Paths returns the certificate and key paths for a named server, without
// touching the filesystem.
func (s *Store) Paths(name string) (certPath, keyPath string) {
	dir := s.ServerDir(name)
	return filepath.Join(dir, CertFileName), filepath.Join(dir, KeyFileName)
}

// StagingRoot returns the directory holding not-yet-adopted credential material.
func (s *Store) StagingRoot() string { return filepath.Join(s.root, StagingDirName) }

// EscapeServerName maps a server name onto exactly one inert path component.
//
// url.PathEscape does most of it — it encodes "/" and every other separator —
// but it deliberately leaves "." alone, because a dot is a perfectly legal path
// character. That is fine for a name embedded in a longer filename and NOT fine
// here, where the escaped name is the whole component: a server named ".." would
// resolve <root>/.. to the root's parent, and Remove would then recursively
// delete it — for the CLI store, the user's entire shed configuration.
//
// Only the exact components ""/"."/".." carry that meaning; a name like "..foo"
// is already inert. So those three get a leading "%2E", which keeps the result a
// single, ordinary directory name, keeps the mapping injective, and leaves every
// realistic server name untouched.
func EscapeServerName(name string) string {
	esc := url.PathEscape(name)
	switch esc {
	case "", ".", "..":
		return "%2E" + esc
	}
	return esc
}

// ensureRoot creates the store root and tightens it to 0700.
//
// The chmod is not redundant with the MkdirAll: MkdirAll is a no-op on a
// directory that already exists, so a root left group- or world-readable by an
// older build, a careless restore, or a permissive umask would keep those
// permissions forever — and every private key in the store sits under it. The
// same reasoning applies one level down, to the per-server directory.
func (s *Store) ensureRoot() error {
	if s.root == "" {
		return errors.New("client credentials: store has no root directory")
	}
	if err := os.MkdirAll(s.root, DirPerm); err != nil {
		return fmt.Errorf("create credentials dir %s: %w", s.root, err)
	}
	if err := os.Chmod(s.root, DirPerm); err != nil {
		return fmt.Errorf("tighten credentials dir %s: %w", s.root, err)
	}
	return nil
}

// Lock takes the exclusive advisory lock guarding one server's credential pair
// and returns the release function.
//
// It is held across the WHOLE write (both renames) and across the whole load,
// which is what makes a rotation atomic as a PAIR rather than merely per file.
// Each file is already written atomically, but two independent renames are two
// independent commits: without this lock a reader can observe the new
// certificate beside the old key, and two processes refreshing at once can
// interleave into cert-A-with-key-B — a credential that belongs to nobody and
// fails the handshake at some later, unrelated moment.
//
// A lock file is never removed once created (see LockDirName), and an empty name
// means "no identifiable server", which takes no lock at all: a nameless entry is
// never written by the persist path either, so there is nothing to serialize
// against.
func (s *Store) Lock(name string) (func(), error) {
	if name == "" {
		return func() {}, nil
	}
	if err := s.ensureRoot(); err != nil {
		return nil, err
	}
	lockDir := filepath.Join(s.root, LockDirName)
	if err := os.MkdirAll(lockDir, DirPerm); err != nil {
		return nil, fmt.Errorf("create credentials lock dir %s: %w", lockDir, err)
	}
	lockPath := filepath.Join(lockDir, EscapeServerName(name))
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, FilePerm)
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

// Load reads a server's stored certificate + key under that server's credential
// lock and assembles them for the TLS stack.
//
// It returns (nil, err) for EVERY unusable state — absent files, unreadable
// files, malformed PEM, and a certificate that does not match the key — because
// all of them mean the same thing to the caller: there is no credential to
// present, so re-enroll. In particular a mismatched pair is ErrPairMismatch
// rather than a hard failure: it is what a crash between the two renames of a
// rotation leaves behind, and the recovery for it is a fresh enrollment, not an
// operator with a text editor.
//
// name identifies the lock, not the paths: the paths may come from a config
// entry (which the user may have edited) while the lock is keyed on the entry's
// name, matching what Write holds. An empty name skips the lock.
func (s *Store) Load(name, certPath, keyPath string) (*tls.Certificate, error) {
	if certPath == "" || keyPath == "" {
		return nil, errors.New("client credentials: no client certificate on file")
	}
	unlock, err := s.Lock(name)
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
	if err := VerifyPairMatches(certPEM, keyPEM); err != nil {
		return nil, err
	}
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, fmt.Errorf("assemble client credentials: %w", err)
	}
	return &cert, nil
}

// LoadServer is Load for a pair the store itself owns: the paths come from
// Paths(name) rather than from a caller-held config entry. It is the shape every
// non-CLI holder wants — the host-agent has no config file recording where its
// own material lives, so the store is the only authority on the location.
func (s *Store) LoadServer(name string) (*tls.Certificate, error) {
	certPath, keyPath := s.Paths(name)
	return s.Load(name, certPath, keyPath)
}

// VerifyPairMatches reports whether certPEM certifies the public half of keyPEM.
//
// tls.X509KeyPair performs the same comparison internally, but only reports it
// as an opaque error string. Doing it here first is what lets the mismatch be
// classified — as the recoverable ErrPairMismatch — instead of being
// indistinguishable from a genuinely malformed file.
func VerifyPairMatches(certPEM, keyPEM []byte) error {
	block, _ := pem.Decode(certPEM)
	if block == nil || block.Type != "CERTIFICATE" {
		return fmt.Errorf("%w: the certificate file is not a PEM CERTIFICATE block", ErrPairMismatch)
	}
	leaf, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrPairMismatch, err)
	}
	keyPub, err := publicKeyOfPEM(keyPEM)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrPairMismatch, err)
	}
	// crypto.PublicKey is an empty interface, but every standard-library public
	// key type carries this Equal method — the idiomatic way to compare two keys
	// without switching on their concrete types.
	pub, ok := leaf.PublicKey.(interface{ Equal(crypto.PublicKey) bool })
	if !ok || !pub.Equal(keyPub) {
		return ErrPairMismatch
	}
	return nil
}

// publicKeyOfPEM extracts the public half of a PEM-encoded private key. An
// enrollment always writes SEC 1 EC, but the other two standard encodings are
// accepted so a hand-restored or externally-provisioned key is compared rather
// than rejected as unparseable.
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

// Write persists a freshly issued client certificate and its private key for the
// named server, returning the paths written.
//
// The key is written FIRST and the certificate second. Both orders can be
// interrupted, and both leave a mismatched pair that the next load rejects — but
// a key with no certificate is inert, whereas a certificate with no key is the
// shape of a credential whose private half has gone missing.
//
// Each file is written atomically (temp file in the same directory, fsynced,
// renamed) so a concurrent reader never observes a half-written PEM. Atomicity
// per FILE is not enough on its own, though — two renames are two commits — so
// the whole write runs under the server's exclusive credential lock, which is
// what stops two processes rotating at once from interleaving into a cert from
// one and a key from the other. Load takes the same lock, and additionally
// verifies the pair, which covers the one case no lock can: a crash BETWEEN the
// two renames.
func (s *Store) Write(name string, certPEM, keyPEM []byte) (certPath, keyPath string, err error) {
	if name == "" {
		return "", "", errors.New("client credentials: server name required")
	}
	if len(certPEM) == 0 || len(keyPEM) == 0 {
		return "", "", errors.New("client credentials: empty certificate or key")
	}
	if err := s.ensureRoot(); err != nil {
		return "", "", err
	}
	unlock, err := s.Lock(name)
	if err != nil {
		return "", "", err
	}
	defer unlock()

	dir := s.ServerDir(name)
	if err := os.MkdirAll(dir, DirPerm); err != nil {
		return "", "", fmt.Errorf("create credentials dir %s: %w", dir, err)
	}
	// MkdirAll is a no-op on an existing directory, including one created with
	// looser permissions by an older build or a careless restore. Tighten it.
	if err := os.Chmod(dir, DirPerm); err != nil {
		return "", "", fmt.Errorf("tighten credentials dir %s: %w", dir, err)
	}

	certPath, keyPath = s.Paths(name)
	if err := AtomicWriteFile(keyPath, keyPEM, FilePerm); err != nil {
		return "", "", fmt.Errorf("write client key %s: %w", keyPath, err)
	}
	if err := AtomicWriteFile(certPath, certPEM, FilePerm); err != nil {
		// Roll the key back so the next start sees "no credentials" (and
		// re-enrolls) rather than a key that certifies nothing.
		_ = os.Remove(keyPath)
		return "", "", fmt.Errorf("write client certificate %s: %w", certPath, err)
	}
	return certPath, keyPath, nil
}

// Remove deletes a server's credential directory. Leaving a private key behind
// for a server the user has explicitly forgotten is exactly the kind of quiet
// residue that turns up years later. A missing directory is not an error.
func (s *Store) Remove(name string) error {
	if name == "" {
		return nil
	}
	dir := s.ServerDir(name)
	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("remove credentials dir %s: %w", dir, err)
	}
	return nil
}

// Staged is a client certificate + key written to disk but not yet adopted by
// the server it belongs to.
//
// It exists so that persisting an enrollment can be a TRANSACTION rather than a
// sequence of independent mutations. A caller that has to update two things —
// a config file and the credential store — needs the invariant that the config
// on disk always names credential material that exists and matches the auth mode
// recorded beside it. Staging is what lets the caller order the two writes so
// that the DESTRUCTIVE half (overwriting or deleting material the config still
// points at) only ever runs after the config save it belongs to has succeeded.
//
// The staged pair lives in its own directory under StagingRoot, on the same
// filesystem as its destination, so Commit is a pair of renames rather than a
// copy that can half-fail.
type Staged struct {
	store             *Store
	name              string
	dir               string
	stagedCert        string
	stagedKey         string
	certPath, keyPath string
}

// Stage writes an issued certificate + key into the staging area for the named
// server, WITHOUT touching whatever that server currently has. This is where a
// full disk, a bad permission, or an unwritable root surfaces — before the
// caller commits to anything.
//
// Call Commit to adopt the pair, or Discard to throw it away. Neither is
// automatic: a staged pair that is never committed and never discarded is the
// one residue this API can leave, and it is inert (a certificate for a server
// the config does not reference).
func (s *Store) Stage(name string, certPEM, keyPEM []byte) (*Staged, error) {
	if name == "" {
		return nil, errors.New("client credentials: server name required")
	}
	if len(certPEM) == 0 || len(keyPEM) == 0 {
		return nil, errors.New("client credentials: empty certificate or key")
	}
	if err := s.ensureRoot(); err != nil {
		return nil, err
	}
	root := s.StagingRoot()
	if err := os.MkdirAll(root, DirPerm); err != nil {
		return nil, fmt.Errorf("create credentials staging dir %s: %w", root, err)
	}
	if err := os.Chmod(root, DirPerm); err != nil {
		return nil, fmt.Errorf("tighten credentials staging dir %s: %w", root, err)
	}
	dir, err := os.MkdirTemp(root, EscapeServerName(name)+".")
	if err != nil {
		return nil, fmt.Errorf("create credentials staging dir in %s: %w", root, err)
	}
	if err := os.Chmod(dir, DirPerm); err != nil {
		_ = os.RemoveAll(dir)
		return nil, fmt.Errorf("tighten credentials staging dir %s: %w", dir, err)
	}

	st := &Staged{store: s, name: name, dir: dir}
	st.stagedCert = filepath.Join(dir, CertFileName)
	st.stagedKey = filepath.Join(dir, KeyFileName)
	st.certPath, st.keyPath = s.Paths(name)

	if err := AtomicWriteFile(st.stagedKey, keyPEM, FilePerm); err != nil {
		st.Discard()
		return nil, fmt.Errorf("stage client key: %w", err)
	}
	if err := AtomicWriteFile(st.stagedCert, certPEM, FilePerm); err != nil {
		st.Discard()
		return nil, fmt.Errorf("stage client certificate: %w", err)
	}
	return st, nil
}

// Paths returns where this pair will live once committed — the ordinary
// per-server credential paths. They are what a config entry must record: the
// staging location is an implementation detail that no config ever names.
func (s *Staged) Paths() (certPath, keyPath string) { return s.certPath, s.keyPath }

// Commit moves the staged pair into the server's credential directory, under
// that server's exclusive lock (so a concurrent load never sees one file from
// this pair and one from the pair it replaces).
//
// The key is renamed first, for the same reason Write writes it first: an
// interruption between the two renames leaves a key with no matching
// certificate, which Load reports as the recoverable ErrPairMismatch and the
// caller answers with a fresh enrollment.
func (s *Staged) Commit() error {
	unlock, err := s.store.Lock(s.name)
	if err != nil {
		return err
	}
	defer unlock()

	dir := s.store.ServerDir(s.name)
	if err := os.MkdirAll(dir, DirPerm); err != nil {
		return fmt.Errorf("create credentials dir %s: %w", dir, err)
	}
	if err := os.Chmod(dir, DirPerm); err != nil {
		return fmt.Errorf("tighten credentials dir %s: %w", dir, err)
	}
	if err := os.Rename(s.stagedKey, s.keyPath); err != nil {
		return fmt.Errorf("commit client key %s: %w", s.keyPath, err)
	}
	if err := os.Rename(s.stagedCert, s.certPath); err != nil {
		return fmt.Errorf("commit client certificate %s: %w", s.certPath, err)
	}
	// Persist the renames. Best effort: some filesystems reject fsync on a
	// directory handle, and the renames have already succeeded.
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	s.Discard()
	return nil
}

// Discard removes the staged material. It is safe to call twice, and safe to
// call after Commit (which uses it to clean up the empty staging directory).
func (s *Staged) Discard() {
	if s == nil || s.dir == "" {
		return
	}
	_ = os.RemoveAll(s.dir)
	s.dir = ""
}

// AtomicWriteFile writes data to path via a temp file in the same directory,
// fsynced and renamed. The temp file is created with the final permissions
// before any bytes are written, so a key is never briefly world-readable.
func AtomicWriteFile(path string, data []byte, perm os.FileMode) error {
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
