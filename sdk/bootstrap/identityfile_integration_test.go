package bootstrap_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"encoding/pem"
	"errors"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/charliek/shed/sdk"
	bootstrap "github.com/charliek/shed/sdk/bootstrap"
	"golang.org/x/crypto/ssh"
)

// writePrivateKey writes priv in OpenSSH form at path, 0600 (ssh refuses a
// looser key file).
func writePrivateKey(t *testing.T, path string, priv ed25519.PrivateKey) {
	t.Helper()
	block, err := ssh.MarshalPrivateKey(priv, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, pem.EncodeToMemory(block), 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestRunOffersTheIdentityFile drives the real ssh client against a server
// that admits ONE key. With that key's file as Params.IdentityFile (at a path
// with a space and a non-ASCII letter, handed to ssh as one argv element) the
// exchange succeeds; with another key's file it is refused: ssh offered the
// identity file, and nothing the agent holds (there is none here).
func TestRunOffersTheIdentityFile(t *testing.T) {
	requireSSH(t)
	bundleJSON, err := json.Marshal(sdk.Bundle{HTTPSPort: 8443, TLSCertFingerprint: "sha256:abc", Token: "shed_control_xyz", Scope: "control"})
	if err != nil {
		t.Fatal(err)
	}

	admitted, admittedPriv := newSigner(t)
	_, otherPriv := newSigner(t)
	hostKey, _ := newSigner(t)
	cfg := &ssh.ServerConfig{
		PublicKeyCallback: func(_ ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
			if bytes.Equal(key.Marshal(), admitted.PublicKey().Marshal()) {
				return &ssh.Permissions{}, nil
			}
			return nil, errors.New("not this key")
		},
	}
	cfg.AddHostKey(hostKey)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go serveOne(conn, cfg, string(bundleJSON), nil)
		}
	}()
	port := ln.Addr().(*net.TCPAddr).Port

	home := t.TempDir()
	kh := writeKnownHosts(t, home, port, hostKey.PublicKey())
	keys := filepath.Join(home, "keys")
	if err := os.MkdirAll(keys, 0o700); err != nil {
		t.Fatal(err)
	}
	admittedPath := filepath.Join(keys, "clé de travail")
	otherPath := filepath.Join(keys, "other key")
	writePrivateKey(t, admittedPath, admittedPriv)
	writePrivateKey(t, otherPath, otherPriv)
	t.Setenv("HOME", home)
	t.Setenv("SSH_AUTH_SOCK", "")

	run := func(identity string) error {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		_, err := bootstrap.Run(ctx, bootstrap.Params{Host: "127.0.0.1", Port: port, KnownHostsPath: kh,
			Scope: "control", ClientKind: "test", IdentityFile: identity})
		return err
	}
	if err := run(admittedPath); err != nil {
		t.Fatalf("Run with the admitted key's file: %v", err)
	}
	if err := run(otherPath); !errors.Is(err, bootstrap.ErrNoSSHIdentities) {
		t.Fatalf("Run with another key's file = %v, want ErrNoSSHIdentities", err)
	}
}
