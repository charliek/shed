package sshlink

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/pem"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
)

// testKeys returns OpenSSH private key files: an ed25519 key, the same kind
// with a passphrase, and an RSA key.
func testKeys(t *testing.T) (ed, edEncrypted, rsaPEM []byte, edPublic ssh.PublicKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	block, err := ssh.MarshalPrivateKey(priv, "test")
	if err != nil {
		t.Fatal(err)
	}
	ed = pem.EncodeToMemory(block)

	block, err = ssh.MarshalPrivateKeyWithPassphrase(priv, "test", []byte("secret"))
	if err != nil {
		t.Fatal(err)
	}
	edEncrypted = pem.EncodeToMemory(block)

	rsaKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	block, err = ssh.MarshalPrivateKey(rsaKey, "test")
	if err != nil {
		t.Fatal(err)
	}
	rsaPEM = pem.EncodeToMemory(block)

	edPublic, err = ssh.NewPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	return ed, edEncrypted, rsaPEM, edPublic
}

func TestParseClientKey(t *testing.T) {
	ed, encrypted, rsaPEM, _ := testKeys(t)
	tests := []struct {
		name    string
		pem     []byte
		wantErr string
	}{
		{name: "ed25519", pem: ed},
		{name: "passphrase-protected", pem: encrypted, wantErr: "passphrase"},
		{name: "rsa", pem: rsaPEM, wantErr: "needs an ssh-ed25519 key"},
		{name: "not a key", pem: []byte("hello"), wantErr: "parse SSH client key"},
		{name: "empty", pem: nil, wantErr: "parse SSH client key"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			signer, err := ParseClientKey(tt.pem)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("ParseClientKey: %v", err)
				}
				if signer.PublicKey().Type() != ssh.KeyAlgoED25519 {
					t.Errorf("key type %s", signer.PublicKey().Type())
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("ParseClientKey = %v, want an error saying %q", err, tt.wantErr)
			}
		})
	}
}

func TestLoadClientKey(t *testing.T) {
	ed, _, rsaPEM, edPublic := testKeys(t)
	dir := t.TempDir()
	keyFile := filepath.Join(dir, "key")
	if err := os.WriteFile(keyFile, ed, 0o600); err != nil {
		t.Fatal(err)
	}
	bigFile := filepath.Join(dir, "big")
	if err := os.WriteFile(bigFile, make([]byte, maxKeyFile+1), 0o600); err != nil {
		t.Fatal(err)
	}
	b64 := base64.StdEncoding.EncodeToString(ed)
	wrapped := b64[:40] + "\n" + b64[40:] + "\n" // as "base64 -w 76" prints it

	const keyEnv, fileEnv = "TEST_SSH_KEY", "TEST_SSH_KEY_FILE"
	tests := []struct {
		name    string
		env     map[string]string
		keyEnv  string
		fileEnv string
		wantErr string
	}{
		{name: "inline base64", env: map[string]string{keyEnv: b64}, keyEnv: keyEnv, fileEnv: fileEnv},
		{name: "inline base64 with line breaks", env: map[string]string{keyEnv: wrapped}, keyEnv: keyEnv, fileEnv: fileEnv},
		{name: "file", env: map[string]string{fileEnv: keyFile}, keyEnv: keyEnv, fileEnv: fileEnv},
		{name: "only a file variable configured", env: map[string]string{fileEnv: keyFile}, fileEnv: fileEnv},
		{name: "both set", env: map[string]string{keyEnv: b64, fileEnv: keyFile}, keyEnv: keyEnv, fileEnv: fileEnv, wantErr: "set exactly one"},
		{name: "neither set", env: nil, keyEnv: keyEnv, fileEnv: fileEnv, wantErr: "set TEST_SSH_KEY or TEST_SSH_KEY_FILE"},
		{name: "no variable configured", env: nil, wantErr: "no SSH client key variable"},
		{name: "not base64", env: map[string]string{keyEnv: "!!!"}, keyEnv: keyEnv, fileEnv: fileEnv, wantErr: "TEST_SSH_KEY is not valid base64"},
		{name: "base64 of an RSA key", env: map[string]string{keyEnv: base64.StdEncoding.EncodeToString(rsaPEM)}, keyEnv: keyEnv, fileEnv: fileEnv, wantErr: "TEST_SSH_KEY: the SSH client key is ssh-rsa"},
		{name: "missing file", env: map[string]string{fileEnv: filepath.Join(dir, "absent")}, keyEnv: keyEnv, fileEnv: fileEnv, wantErr: "TEST_SSH_KEY_FILE:"},
		{name: "a directory", env: map[string]string{fileEnv: dir}, keyEnv: keyEnv, fileEnv: fileEnv, wantErr: "not a regular file"},
		{name: "an oversized file", env: map[string]string{fileEnv: bigFile}, keyEnv: keyEnv, fileEnv: fileEnv, wantErr: "a key file is under"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			signer, err := loadClientKey(tt.keyEnv, tt.fileEnv, func(name string) string { return tt.env[name] })
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("loadClientKey = %v, want an error saying %q", err, tt.wantErr)
				}
				// A key error must never quote the key.
				if strings.Contains(err.Error(), b64[:24]) {
					t.Errorf("the error quotes key material: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("loadClientKey: %v", err)
			}
			if !sameKey(signer.PublicKey(), edPublic) {
				t.Error("loaded a different key")
			}
		})
	}

	t.Run("LoadClientKey reads the process environment", func(t *testing.T) {
		t.Setenv(fileEnv, keyFile)
		t.Setenv(keyEnv, "")
		signer, err := LoadClientKey(keyEnv, fileEnv)
		if err != nil || !sameKey(signer.PublicKey(), edPublic) {
			t.Fatalf("LoadClientKey = %v", err)
		}
	})
}

func TestAuthorizedKeyAndHostKeyForms(t *testing.T) {
	ed, _, rsaPEM, edPublic := testKeys(t)
	signer, err := ParseClientKey(ed)
	if err != nil {
		t.Fatal(err)
	}
	line := AuthorizedKey(signer)
	if !strings.HasPrefix(line, "ssh-ed25519 AAAA") || strings.ContainsAny(line, "\n\r") || len(strings.Fields(line)) != 2 {
		t.Errorf("AuthorizedKey = %q, want one \"ssh-ed25519 AAAA...\" line with no comment", line)
	}
	if FormatHostKey(edPublic) != line {
		t.Errorf("FormatHostKey and AuthorizedKey disagree on the same key")
	}

	rsaSigner, err := ssh.ParsePrivateKey(rsaPEM)
	if err != nil {
		t.Fatal(err)
	}
	rsaLine := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(rsaSigner.PublicKey())))

	tests := []struct {
		name    string
		line    string
		wantErr string
	}{
		{name: "pin form", line: line},
		{name: "with a comment", line: line + " root@guest"},
		{name: "with a newline", line: line + "\n"},
		{name: "empty", line: "  ", wantErr: "empty"},
		{name: "rsa", line: rsaLine, wantErr: "only ssh-ed25519"},
		{name: "two keys", line: line + "\n" + line, wantErr: "more than one line"},
		{name: "with options", line: `command="id" ` + line, wantErr: "options"},
		{name: "garbage", line: "ssh-ed25519 AAAA", wantErr: "parse host key"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			key, err := ParseHostKey(tt.line)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("ParseHostKey = %v, want an error saying %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseHostKey: %v", err)
			}
			if !sameKey(key, edPublic) {
				t.Error("parsed a different key")
			}
		})
	}
}

func TestPinnedHostKey(t *testing.T) {
	store := PinnedHostKey("  ssh-ed25519 AAAAexample \n")
	pin, err := store.Pin(context.Background())
	if err != nil || pin != "ssh-ed25519 AAAAexample" {
		t.Errorf("Pin = %q, %v", pin, err)
	}
	if err := store.Learn(context.Background(), "ssh-ed25519 AAAAother"); err == nil {
		t.Error("a fixed pin learned another key")
	}
}
