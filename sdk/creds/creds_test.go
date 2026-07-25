package creds_test

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"io/fs"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/charliek/shed/sdk/creds"
)

// The store's full behavior — permissions, atomicity, the lock, name escaping,
// staging — is exercised through its CLI binding in
// internal/config/clientcreds_test.go, where the assertions can also pin the
// ~/.shed layout that binding promises. What is asserted HERE is the one thing
// only this package can express, and the reason the store is rooted rather than
// fixed: two holders of client credentials, under two roots, that must not be
// able to reach each other's material.
//
// That is not a hypothetical tidiness concern. The shed CLI holds a
// control-scope certificate and the host-agent holds a credentials-scope one;
// one certificate carries exactly one scope, so if they shared a per-server
// directory each would overwrite the other in place on every rotation, and each
// would then present a certificate the server refuses for the route it is
// calling — intermittently, depending on who rotated last.

func issue(t *testing.T, cn string) (certPEM, keyPEM []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: cn},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
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

func TestStoresAtDifferentRootsAreIndependent(t *testing.T) {
	base := t.TempDir()
	cli := creds.NewStore(filepath.Join(base, "cli"))
	agent := creds.NewStore(filepath.Join(base, "agent", "credentials"))

	cliCert, cliKey := issue(t, "cli")
	agentCert, agentKey := issue(t, "agent")

	// Same server NAME in both stores — the collision that matters.
	if _, _, err := cli.Write("prod", cliCert, cliKey); err != nil {
		t.Fatal(err)
	}
	if _, _, err := agent.Write("prod", agentCert, agentKey); err != nil {
		t.Fatal(err)
	}

	got, err := cli.LoadServer("prod")
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(got.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	if leaf.Subject.CommonName != "cli" {
		t.Errorf("the CLI store loaded CN %q; the agent's write reached it", leaf.Subject.CommonName)
	}

	got, err = agent.LoadServer("prod")
	if err != nil {
		t.Fatal(err)
	}
	if leaf, err = x509.ParseCertificate(got.Certificate[0]); err != nil {
		t.Fatal(err)
	}
	if leaf.Subject.CommonName != "agent" {
		t.Errorf("the agent store loaded CN %q; the CLI's write reached it", leaf.Subject.CommonName)
	}

	// Removal is likewise scoped: forgetting a server in one store leaves the
	// other holder's credential for the same server alone.
	if err := cli.Remove("prod"); err != nil {
		t.Fatal(err)
	}
	if _, err := cli.LoadServer("prod"); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("after Remove the CLI store should hold nothing, got err=%v", err)
	}
	if _, err := agent.LoadServer("prod"); err != nil {
		t.Errorf("the agent's credential was removed by the CLI store: %v", err)
	}
}

// A store whose root does not exist yet creates it on first write, with the
// directory permissions the material requires — construction touches nothing, so
// nothing has to exist before a process decides it has a credential to keep.
func TestWriteCreatesTheRootWithOwnerOnlyPermissions(t *testing.T) {
	root := filepath.Join(t.TempDir(), "deeply", "nested", "creds")
	store := creds.NewStore(root)
	if _, err := os.Stat(root); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("NewStore must not touch the filesystem, got err=%v", err)
	}

	certPEM, keyPEM := issue(t, "x")
	certPath, keyPath, err := store.Write("prod", certPEM, keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{root, store.ServerDir("prod")} {
		fi, err := os.Stat(dir)
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm() != creds.DirPerm {
			t.Errorf("%s mode = %v, want %v", dir, fi.Mode().Perm(), creds.DirPerm)
		}
	}
	for _, p := range []string{certPath, keyPath} {
		fi, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm() != creds.FilePerm {
			t.Errorf("%s mode = %v, want %v", p, fi.Mode().Perm(), creds.FilePerm)
		}
	}
}

// A store with no root at all fails rather than defaulting to somewhere — a
// zero-value Store must never quietly write private keys into the process's
// working directory.
func TestAnEmptyRootIsAnError(t *testing.T) {
	certPEM, keyPEM := issue(t, "x")
	if _, _, err := creds.NewStore("").Write("prod", certPEM, keyPEM); err == nil {
		t.Error("a store with no root must refuse to write")
	}
}
