package config

// clientcreds.go binds the shed CLI to the shared client-credential store: the
// material an mtls-mode server issues over the SSH bootstrap channel, kept out
// of config.yaml.
//
// The store itself — the ~/.shed/creds layout, the 0700/0600 permissions, the
// atomic per-file write, and the per-server advisory lock that makes a rotation
// atomic as a PAIR — lives in sdk/creds. It lives THERE rather than here
// because the CLI is not its only holder: the host-agent keeps a
// credentials-scope certificate of its own, under its own state dir, and two
// copies of a lock-and-rename discipline is exactly the kind of duplication
// that drifts. This file is the CLI's binding of that store to ~/.shed/creds,
// and nothing else.

import (
	"crypto/tls"
	"os"
	"path/filepath"

	"github.com/charliek/shed/sdk/creds"
)

// ErrCredentialPairMismatch reports a stored certificate and private key that
// do not belong together.
//
// It is a RECOVERABLE state, not a corruption to report to the user: a crash
// between the two renames of a rotation leaves exactly this, and so does an
// interrupted restore. Callers treat it as "no credential" and re-enroll — see
// LoadClientCredentials.
var ErrCredentialPairMismatch = creds.ErrPairMismatch

// credsLockDirName is the store's per-server lock directory name, re-exported
// at package scope because the collision-proofness of the "%" prefix (no
// escaped server name can ever produce it) is asserted by this package's tests.
const credsLockDirName = creds.LockDirName

// clientCredStore is the CLI's store, rooted at ~/.shed/creds. It is resolved
// per call rather than cached in a package variable because GetClientConfigDir
// reads the environment, which the tests reassign between cases.
func clientCredStore() *creds.Store { return creds.NewStore(GetCredsDir()) }

// GetCredsDir returns the root of the client-credential store (~/.shed/creds).
func GetCredsDir() string {
	return filepath.Join(GetClientConfigDir(), "creds")
}

// ServerCredsDir returns the credential directory for a named server entry.
func ServerCredsDir(name string) string { return clientCredStore().ServerDir(name) }

// ClientCredentialPaths returns the certificate and key paths for a named
// server, without touching the filesystem.
func ClientCredentialPaths(name string) (certPath, keyPath string) {
	return clientCredStore().Paths(name)
}

// LoadClientCredentials reads a server's stored certificate + key under that
// server's credential lock and assembles them for the TLS stack.
//
// It returns (nil, err) for EVERY unusable state — absent files, unreadable
// files, malformed PEM, and a certificate that does not match the key — because
// all of them mean the same thing to the caller: there is no credential to
// present, so re-enroll.
//
// name identifies the lock, not the paths: the paths come from the config entry
// (which the user may have edited) while the lock is keyed on the entry's name,
// matching what WriteClientCredentials holds. An empty name skips the lock.
func LoadClientCredentials(name, certPath, keyPath string) (*tls.Certificate, error) {
	return clientCredStore().Load(name, certPath, keyPath)
}

// WriteClientCredentials persists a freshly issued client certificate and its
// private key for the named server, returning the paths written.
func WriteClientCredentials(name string, certPEM, keyPEM []byte) (certPath, keyPath string, err error) {
	return clientCredStore().Write(name, certPEM, keyPEM)
}

// CredsStagingRoot returns the directory holding not-yet-adopted credential
// material (~/.shed/creds/%staging). Exported so a test can make a staging write
// fail without reaching into this package's internals.
func CredsStagingRoot() string { return clientCredStore().StagingRoot() }

// StagedClientCredentials is a client certificate + key written to disk but not
// yet adopted by the server it belongs to — the transaction handle that lets
// `shed server add` order the config save before the destructive half of the
// credential update. See sdk/creds.Staged.
type StagedClientCredentials = creds.Staged

// StageClientCredentials writes an issued certificate + key into the staging
// area for the named server, WITHOUT touching whatever that server currently
// has. Call Commit to adopt the pair, or Discard to throw it away.
func StageClientCredentials(name string, certPEM, keyPEM []byte) (*StagedClientCredentials, error) {
	return clientCredStore().Stage(name, certPEM, keyPEM)
}

// RemoveServerCredentials deletes a server's credential directory. It is called
// by `shed server rm`: leaving a private key behind for a server the user has
// explicitly forgotten is exactly the kind of quiet residue that turns up years
// later. A missing directory is not an error.
func RemoveServerCredentials(name string) error { return clientCredStore().Remove(name) }

// atomicWriteFile writes data to path via a temp file in the same directory,
// fsynced and renamed. The temp file is created with the final permissions
// before any bytes are written, so a key is never briefly world-readable.
//
// It is this package's single atomic writer — the client config saver
// (client.go) and the egress user-profile store (egress_userstore.go) share the
// credential store's implementation rather than keeping a second, weaker copy.
func atomicWriteFile(path string, data []byte, perm os.FileMode) error {
	return creds.AtomicWriteFile(path, data, perm)
}
