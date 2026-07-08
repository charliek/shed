package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/crypto/ssh"
)

func TestLocalKeysBackendLoadAndSign(t *testing.T) {
	dir := t.TempDir()

	// Generate an ed25519 key and write it to the temp dir
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}

	sshPub, err := ssh.NewPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}

	// Marshal private key to PEM
	privBytes, err := ssh.MarshalPrivateKey(priv, "")
	if err != nil {
		t.Fatal(err)
	}
	pemData := pem.EncodeToMemory(privBytes)

	keyPath := filepath.Join(dir, "id_ed25519")
	if err := os.WriteFile(keyPath, pemData, 0600); err != nil {
		t.Fatal(err)
	}

	// Override home dir for the test by using the backend directly
	logger := slog.Default()
	b := &localKeysBackend{logger: logger}

	// Load the key manually (since newLocalKeysBackend uses os.UserHomeDir)
	signer, err := ssh.ParsePrivateKey(pemData)
	if err != nil {
		t.Fatal(err)
	}
	b.keys = []localKey{
		{pubKey: signer.PublicKey(), signer: signer, comment: "id_ed25519"},
	}

	// Test List
	keys, err := b.List()
	if err != nil {
		t.Fatalf("List failed: %v", err)
	}
	if len(keys) != 1 {
		t.Fatalf("expected 1 key, got %d", len(keys))
	}
	if keys[0].Format != "ssh-ed25519" {
		t.Errorf("format: got %q, want ssh-ed25519", keys[0].Format)
	}

	// Test Sign
	data := []byte("test challenge data")
	sig, err := b.Sign(sshPub, data, 0)
	if err != nil {
		t.Fatalf("Sign failed: %v", err)
	}
	if sig.Format != "ssh-ed25519" {
		t.Errorf("sig format: got %q, want ssh-ed25519", sig.Format)
	}

	// Verify the signature
	if err := sshPub.Verify(data, sig); err != nil {
		t.Errorf("signature verification failed: %v", err)
	}

	// Test Mode
	if b.Mode() != "local-keys" {
		t.Errorf("mode: got %q, want local-keys", b.Mode())
	}
}

func TestLocalKeysBackendSignKeyNotFound(t *testing.T) {
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	sshPub, err := ssh.NewPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}

	b := &localKeysBackend{logger: slog.Default()}

	_, err = b.Sign(sshPub, []byte("data"), 0)
	if err == nil {
		t.Fatal("expected error for key not found")
	}
}
