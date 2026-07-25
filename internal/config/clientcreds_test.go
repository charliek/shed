package config

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// withHomeShed points GetClientConfigDir (and therefore the creds store) at a
// temp dir for the duration of a test.
func withHomeShed(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	return filepath.Join(home, ".shed")
}

func TestWriteClientCredentialsPermissions(t *testing.T) {
	shed := withHomeShed(t)

	certPath, keyPath, err := WriteClientCredentials("my-server", []byte("CERT-PEM"), []byte("KEY-PEM"))
	if err != nil {
		t.Fatalf("WriteClientCredentials: %v", err)
	}

	wantDir := filepath.Join(shed, "creds", "my-server")
	if got := filepath.Dir(certPath); got != wantDir {
		t.Errorf("cert dir = %s, want %s", got, wantDir)
	}
	if filepath.Base(certPath) != "client.pem" || filepath.Base(keyPath) != "client.key" {
		t.Errorf("unexpected basenames: %s / %s", certPath, keyPath)
	}

	// The private key must not be readable beyond its owner, and neither must
	// the directory that holds it.
	for _, tc := range []struct {
		path string
		want os.FileMode
	}{
		{wantDir, 0700},
		{certPath, 0600},
		{keyPath, 0600},
	} {
		info, err := os.Stat(tc.path)
		if err != nil {
			t.Fatalf("stat %s: %v", tc.path, err)
		}
		if got := info.Mode().Perm(); got != tc.want {
			t.Errorf("%s has mode %04o, want %04o", tc.path, got, tc.want)
		}
	}

	assertFile(t, certPath, "CERT-PEM")
	assertFile(t, keyPath, "KEY-PEM")
}

// TestWriteClientCredentialsTightensLooseDir: a creds dir left world-readable
// by an older build or a careless restore is corrected, not accepted. MkdirAll
// is a no-op on an existing directory, so without the explicit chmod the loose
// mode would persist forever.
func TestWriteClientCredentialsTightensLooseDir(t *testing.T) {
	shed := withHomeShed(t)
	dir := filepath.Join(shed, "creds", "loose")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0755); err != nil {
		t.Fatal(err)
	}

	if _, _, err := WriteClientCredentials("loose", []byte("C"), []byte("K")); err != nil {
		t.Fatalf("WriteClientCredentials: %v", err)
	}
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0700 {
		t.Errorf("creds dir mode = %04o, want 0700 (a loose dir must be tightened)", got)
	}
}

// TestWriteClientCredentialsRotatesInPlace: a re-mint overwrites the pair and
// leaves no temp files or previous generations behind. Stale private keys
// accumulating in the store would be exactly the kind of quiet residue this
// layout exists to avoid.
func TestWriteClientCredentialsRotatesInPlace(t *testing.T) {
	withHomeShed(t)

	certPath, keyPath, err := WriteClientCredentials("s", []byte("CERT-1"), []byte("KEY-1"))
	if err != nil {
		t.Fatal(err)
	}
	certPath2, keyPath2, err := WriteClientCredentials("s", []byte("CERT-2"), []byte("KEY-2"))
	if err != nil {
		t.Fatal(err)
	}
	if certPath2 != certPath || keyPath2 != keyPath {
		t.Errorf("rotation changed the paths: %s/%s then %s/%s", certPath, keyPath, certPath2, keyPath2)
	}
	assertFile(t, certPath, "CERT-2")
	assertFile(t, keyPath, "KEY-2")

	entries, err := os.ReadDir(filepath.Dir(certPath))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("creds dir holds %v, want exactly client.pem + client.key (no temp or stale files)", names)
	}
}

func TestWriteClientCredentialsRejectsEmptyInput(t *testing.T) {
	withHomeShed(t)
	for _, tc := range []struct {
		name      string
		server    string
		cert, key []byte
	}{
		{"no server name", "", []byte("C"), []byte("K")},
		{"no certificate", "s", nil, []byte("K")},
		{"no key", "s", []byte("C"), nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, _, err := WriteClientCredentials(tc.server, tc.cert, tc.key); err == nil {
				t.Error("expected an error")
			}
		})
	}
}

// TestServerCredsDirEscapesName: a server name is user-chosen and unconstrained
// elsewhere in the config, so it must not be able to steer a 0600 write — or the
// recursive delete in RemoveServerCredentials — outside the creds root.
//
// The ".." and "." cases are the sharp ones: url.PathEscape leaves a dot
// untouched (it is a legal path character), so without escapeServerName's extra
// step, ServerCredsDir("..") resolves to ~/.shed and `shed server rm ..` would
// recursively delete the user's whole configuration.
func TestServerCredsDirEscapesName(t *testing.T) {
	shed := withHomeShed(t)
	root := filepath.Join(shed, "creds")

	names := []string{"../../.ssh", "a/b", "..", ".", "", "with space", "..foo", `a\b`, "x\x00y"}
	seen := make(map[string]string, len(names))
	for _, name := range names {
		dir := ServerCredsDir(name)
		// The only property that matters: after cleaning, the result is a direct
		// child of the creds root. That catches "creds/.." (which cleans to
		// ~/.shed) and "creds/." (which cleans to the root itself), while
		// accepting a component that merely CONTAINS dots or escaped separators.
		cleaned := filepath.Clean(dir)
		if got := filepath.Dir(cleaned); got != root {
			t.Errorf("server name %q escaped the creds root: %s (parent %s, want %s)", name, cleaned, got, root)
		}
		if base := filepath.Base(cleaned); base == "." || base == ".." {
			t.Errorf("server name %q produced the traversal component %q", name, base)
		}
		if prev, dup := seen[dir]; dup {
			t.Errorf("server names %q and %q collide on %s", prev, name, dir)
		}
		seen[dir] = name
	}
}

// TestRemoveServerCredentialsCannotDeleteTheConfigDir is the consequence of the
// above, asserted end to end rather than by inspecting a path. This is the
// failure that would be unrecoverable, so it gets its own test.
func TestRemoveServerCredentialsCannotDeleteTheConfigDir(t *testing.T) {
	shed := withHomeShed(t)
	if err := os.MkdirAll(shed, 0700); err != nil {
		t.Fatal(err)
	}
	knownHosts := filepath.Join(shed, "known_hosts")
	if err := os.WriteFile(knownHosts, []byte("host key\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := WriteClientCredentials("real", []byte("C"), []byte("K")); err != nil {
		t.Fatal(err)
	}

	for _, name := range []string{"..", ".", "../..", "../../.."} {
		if err := RemoveServerCredentials(name); err != nil {
			t.Fatalf("RemoveServerCredentials(%q): %v", name, err)
		}
		if _, err := os.Stat(knownHosts); err != nil {
			t.Fatalf("RemoveServerCredentials(%q) destroyed ~/.shed/known_hosts: %v", name, err)
		}
		realCert, _ := ClientCredentialPaths("real")
		if _, err := os.Stat(realCert); err != nil {
			t.Fatalf("RemoveServerCredentials(%q) destroyed another server's credentials: %v", name, err)
		}
	}
}

// TestRemoveServerCredentials: `shed server rm` must leave no private key
// behind, and must not fail when there was never one.
func TestRemoveServerCredentials(t *testing.T) {
	withHomeShed(t)

	certPath, keyPath, err := WriteClientCredentials("gone", []byte("C"), []byte("K"))
	if err != nil {
		t.Fatal(err)
	}
	if err := RemoveServerCredentials("gone"); err != nil {
		t.Fatalf("RemoveServerCredentials: %v", err)
	}
	for _, p := range []string{certPath, keyPath, filepath.Dir(certPath)} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("%s still exists after removal (err=%v)", p, err)
		}
	}

	// Idempotent: removing a server that never had credentials is a no-op.
	if err := RemoveServerCredentials("never-existed"); err != nil {
		t.Errorf("removing absent credentials should be a no-op, got %v", err)
	}
	if err := RemoveServerCredentials(""); err != nil {
		t.Errorf("removing with an empty name should be a no-op, got %v", err)
	}
}

// TestRemoveServerCredentialsIsScoped: removing one server's credentials must
// not touch another's.
func TestRemoveServerCredentialsIsScoped(t *testing.T) {
	withHomeShed(t)

	if _, _, err := WriteClientCredentials("keep", []byte("C"), []byte("K")); err != nil {
		t.Fatal(err)
	}
	keepCert, _ := ClientCredentialPaths("keep")
	if _, _, err := WriteClientCredentials("drop", []byte("C"), []byte("K")); err != nil {
		t.Fatal(err)
	}
	if err := RemoveServerCredentials("drop"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(keepCert); err != nil {
		t.Errorf("removing one server's credentials deleted another's: %v", err)
	}
}

func assertFile(t *testing.T, path, want string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if string(data) != want {
		t.Errorf("%s = %q, want %q", path, data, want)
	}
}

// ---------------------------------------------------------------------------
// Pair integrity: the credential is two files, and a rotation is two renames.
// These cover what that costs — across processes, and across a crash.
// ---------------------------------------------------------------------------

// testKeyPair returns a self-signed certificate and its matching key, PEM
// encoded the way the enrollment path writes them.
func testKeyPair(t *testing.T, cn string) (certPEM, keyPEM []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: cn},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, key.Public(), key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
}

// commonNameOf reports the CN of a loaded credential, which is how these tests
// tell one generation of the pair from another.
func commonNameOf(t *testing.T, cert *tls.Certificate) string {
	t.Helper()
	if cert == nil || len(cert.Certificate) == 0 {
		t.Fatal("no certificate")
	}
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	return leaf.Subject.CommonName
}

// TestLoadClientCredentialsRoundTrip: the ordinary case, so the failure cases
// below are read against a known-good baseline.
func TestLoadClientCredentialsRoundTrip(t *testing.T) {
	withHomeShed(t)
	certPEM, keyPEM := testKeyPair(t, "gen-1")

	certPath, keyPath, err := WriteClientCredentials("s", certPEM, keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := LoadClientCredentials("s", certPath, keyPath)
	if err != nil {
		t.Fatalf("LoadClientCredentials: %v", err)
	}
	if got := commonNameOf(t, cert); got != "gen-1" {
		t.Errorf("loaded CN = %q, want gen-1", got)
	}
	if cert.PrivateKey == nil {
		t.Error("the loaded credential carries no private key")
	}
}

// TestLoadClientCredentialsRejectsMismatchedPair is the load-side half of pair
// integrity. A certificate that does not certify the stored key is unusable, and
// the ONLY useful reaction is to treat it as no credential at all so the caller
// re-enrolls — which is why it is a typed, recognizable error rather than an
// opaque failure the user would have to resolve by hand.
func TestLoadClientCredentialsRejectsMismatchedPair(t *testing.T) {
	withHomeShed(t)
	certA, keyA := testKeyPair(t, "cert-A")
	_, keyB := testKeyPair(t, "cert-B")

	certPath, keyPath, err := WriteClientCredentials("s", certA, keyA)
	if err != nil {
		t.Fatal(err)
	}
	// Cert from A, key from B: what an interleaved write or a half-applied
	// restore leaves behind.
	if err := os.WriteFile(keyPath, keyB, 0600); err != nil {
		t.Fatal(err)
	}

	cert, err := LoadClientCredentials("s", certPath, keyPath)
	if cert != nil {
		t.Error("a mismatched pair was loaded as a usable credential")
	}
	if !errors.Is(err, ErrCredentialPairMismatch) {
		t.Errorf("err = %v, want ErrCredentialPairMismatch", err)
	}
}

// TestLoadClientCredentialsAbsentIsNotUsable: every unusable state reports the
// same way to the caller — nil certificate, non-nil error — because they all
// mean "re-enroll".
func TestLoadClientCredentialsAbsentIsNotUsable(t *testing.T) {
	withHomeShed(t)
	certPath, keyPath := ClientCredentialPaths("s")

	for _, tc := range []struct {
		name       string
		cert, key  string
		setup      func(t *testing.T)
		wantIsMism bool
	}{
		{name: "no paths recorded"},
		{name: "files absent", cert: certPath, key: keyPath},
		{
			name: "certificate is not PEM", cert: certPath, key: keyPath, wantIsMism: true,
			setup: func(t *testing.T) {
				_, keyPEM := testKeyPair(t, "x")
				if _, _, err := WriteClientCredentials("s", []byte("not a pem"), keyPEM); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "key is not PEM", cert: certPath, key: keyPath, wantIsMism: true,
			setup: func(t *testing.T) {
				certPEM, _ := testKeyPair(t, "x")
				if _, _, err := WriteClientCredentials("s", certPEM, []byte("not a pem")); err != nil {
					t.Fatal(err)
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.setup != nil {
				tc.setup(t)
			}
			cert, err := LoadClientCredentials("s", tc.cert, tc.key)
			if cert != nil || err == nil {
				t.Fatalf("cert = %v, err = %v; want (nil, non-nil)", cert, err)
			}
			if tc.wantIsMism && !errors.Is(err, ErrCredentialPairMismatch) {
				t.Errorf("err = %v, want it to classify as ErrCredentialPairMismatch", err)
			}
		})
	}
}

// TestCrashBetweenRenamesIsRecoverable simulates the one interleaving no lock
// can prevent: the process dies after the key rename and before the cert
// rename, leaving the NEW key beside the OLD certificate.
//
// The state must be recoverable without operator intervention: the load refuses
// it (so nothing presents a broken credential), and the very next write puts a
// consistent pair back.
func TestCrashBetweenRenamesIsRecoverable(t *testing.T) {
	withHomeShed(t)
	certOld, keyOld := testKeyPair(t, "old")
	certNew, keyNew := testKeyPair(t, "new")

	certPath, keyPath, err := WriteClientCredentials("s", certOld, keyOld)
	if err != nil {
		t.Fatal(err)
	}
	// The crash: the key half of the rotation landed, the cert half did not.
	if err := os.WriteFile(keyPath, keyNew, 0600); err != nil {
		t.Fatal(err)
	}

	if _, err := LoadClientCredentials("s", certPath, keyPath); !errors.Is(err, ErrCredentialPairMismatch) {
		t.Fatalf("a half-applied rotation loaded as err = %v, want ErrCredentialPairMismatch", err)
	}

	// Re-enrollment (what the caller does on that error) restores a usable pair.
	if _, _, err := WriteClientCredentials("s", certNew, keyNew); err != nil {
		t.Fatalf("re-enrollment after a half-applied rotation: %v", err)
	}
	cert, err := LoadClientCredentials("s", certPath, keyPath)
	if err != nil {
		t.Fatalf("the store did not recover: %v", err)
	}
	if got := commonNameOf(t, cert); got != "new" {
		t.Errorf("recovered CN = %q, want new", got)
	}
}

// TestConcurrentWritersNeverInterleave is the cross-process half. Two rotations
// running at once are two pairs of renames; without a lock held across BOTH
// renames they can interleave into cert-from-one beside key-from-the-other — a
// credential belonging to nobody, which fails at some later handshake rather
// than here.
//
// Each writer writes a self-consistent pair, so the only way to end up with a
// mismatch is an interleaving. Loading concurrently is part of the test: a
// reader must never observe the store mid-rotation either.
func TestConcurrentWritersNeverInterleave(t *testing.T) {
	withHomeShed(t)
	certPath, keyPath := ClientCredentialPaths("s")

	const writers, rounds = 4, 25
	pairs := make([][2][]byte, writers)
	for i := range pairs {
		c, k := testKeyPair(t, fmt.Sprintf("writer-%d", i))
		pairs[i] = [2][]byte{c, k}
	}
	// Seed the store so concurrent readers always have something to read.
	if _, _, err := WriteClientCredentials("s", pairs[0][0], pairs[0][1]); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	errs := make(chan error, writers*rounds*2)
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for r := 0; r < rounds; r++ {
				if _, _, err := WriteClientCredentials("s", pairs[i][0], pairs[i][1]); err != nil {
					errs <- fmt.Errorf("writer %d: %w", i, err)
					return
				}
			}
		}(i)
		wg.Add(1)
		go func() {
			defer wg.Done()
			for r := 0; r < rounds; r++ {
				if _, err := LoadClientCredentials("s", certPath, keyPath); err != nil {
					errs <- fmt.Errorf("reader saw a spliced pair: %w", err)
					return
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}

	// And the store is left consistent, holding exactly one writer's pair.
	cert, err := LoadClientCredentials("s", certPath, keyPath)
	if err != nil {
		t.Fatalf("final load: %v", err)
	}
	if cn := commonNameOf(t, cert); !strings.HasPrefix(cn, "writer-") {
		t.Errorf("final CN = %q, want one of the writers' pairs intact", cn)
	}
}

// TestWriteClientCredentialsTightensCredsRoot: FIX 5's twin of the per-server
// case. ~/.shed/creds may predate this code (or a tighter umask), and every
// private key in the store sits under it — MkdirAll would leave a 0755 root
// permissive forever.
func TestWriteClientCredentialsTightensCredsRoot(t *testing.T) {
	shed := withHomeShed(t)
	root := filepath.Join(shed, "creds")
	if err := os.MkdirAll(root, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(root, 0755); err != nil {
		t.Fatal(err)
	}

	if _, _, err := WriteClientCredentials("s", []byte("C"), []byte("K")); err != nil {
		t.Fatalf("WriteClientCredentials: %v", err)
	}
	info, err := os.Stat(root)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0700 {
		t.Errorf("~/.shed/creds mode = %04o, want 0700 (a pre-existing loose root must be tightened)", got)
	}
}

// TestCredsLockDirCannotCollideWithAServerName: the lock files live beside the
// per-server directories, so the one thing that would break the scheme is a
// server name that escapes onto the lock directory's name. url.PathEscape emits
// "%" only as the start of a two-hex-digit escape, so "%lock" is unreachable —
// asserted here rather than left as a comment, since a future change to
// escapeServerName is exactly what would break it.
func TestCredsLockDirCannotCollideWithAServerName(t *testing.T) {
	shed := withHomeShed(t)
	lockDir := filepath.Join(shed, "creds", credsLockDirName)
	for _, name := range []string{credsLockDirName, "%lock", "%25lock", ".", "..", "", "a/b", "%2Flock"} {
		if got := ServerCredsDir(name); got == lockDir {
			t.Errorf("server name %q maps onto the lock directory %s", name, lockDir)
		}
	}
	// And the lock really is a sibling, so `shed server rm` cannot delete a lock
	// another process is holding.
	if _, _, err := WriteClientCredentials("victim", []byte("C"), []byte("K")); err != nil {
		t.Fatal(err)
	}
	if err := RemoveServerCredentials("victim"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(lockDir, "victim")); err != nil {
		t.Errorf("removing a server deleted its lock file: %v", err)
	}
}
