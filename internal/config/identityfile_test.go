package config

import (
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/charliek/shed/internal/clienttoken"
	"github.com/charliek/shed/internal/servertls"
	sdkbootstrap "github.com/charliek/shed/sdk/bootstrap"
)

// identityKey writes an unencrypted OpenSSH-format ed25519 private key under
// dir and returns its path.
func identityKey(t *testing.T, dir, name string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	writeOpenSSHKey(t, path, nil)
	return path
}

// writeOpenSSHKey writes a new ed25519 private key at path in the OpenSSH
// format, encrypted with passphrase when it is not nil, and returns its
// public key.
func writeOpenSSHKey(t *testing.T, path string, passphrase []byte) ssh.PublicKey {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	var block *pem.Block
	if passphrase == nil {
		block, err = ssh.MarshalPrivateKey(priv, "")
	} else {
		block, err = ssh.MarshalPrivateKeyWithPassphrase(priv, "", passphrase)
	}
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, pem.EncodeToMemory(block), 0o600); err != nil {
		t.Fatal(err)
	}
	sshPub, err := ssh.NewPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	return sshPub
}

// writePub writes pub as an authorized_keys line at path.
func writePub(t *testing.T, path string, pub ssh.PublicKey) {
	t.Helper()
	if err := os.WriteFile(path, ssh.MarshalAuthorizedKey(pub), 0o644); err != nil {
		t.Fatal(err)
	}
}

// newPub is a fresh ed25519 public key.
func newPub(t *testing.T) ssh.PublicKey {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	sshPub, err := ssh.NewPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	return sshPub
}

// writeLegacyEncryptedPEM writes a legacy encrypted PEM private key (the
// `Proc-Type: 4,ENCRYPTED` shape, which carries no public key) at path. Its
// body is not a real key: nothing may read past the encryption header
// without the passphrase.
func writeLegacyEncryptedPEM(t *testing.T, path string) {
	t.Helper()
	block := &pem.Block{Type: "RSA PRIVATE KEY",
		Headers: map[string]string{"Proc-Type": "4,ENCRYPTED", "DEK-Info": "AES-128-CBC,00112233445566778899AABBCCDDEEFF"},
		Bytes:   []byte("encrypted bytes that only the passphrase opens")}
	if err := os.WriteFile(path, pem.EncodeToMemory(block), 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestIdentityFileArgs: no identity file adds nothing; a usable one adds the
// key, IdentitiesOnly and no shared connection; one that cannot be used is an
// *IdentityFileError naming the entry.
func TestIdentityFileArgs(t *testing.T) {
	dir := t.TempDir()
	key := identityKey(t, dir, "id work")

	if args, err := IdentityFileArgs("srv", ""); err != nil || args != nil {
		t.Fatalf(`IdentityFileArgs("") = %q, %v; want nothing`, args, err)
	}
	args, err := IdentityFileArgs("srv", key)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"-i", key, "-o", "IdentitiesOnly=yes", "-o", "ControlMaster=no", "-o", "ControlPath=none"}; !slices.Equal(args, want) {
		t.Errorf("args = %q, want %q", args, want)
	}

	_, err = IdentityFileArgs("srv", filepath.Join(dir, "gone"))
	var ie *IdentityFileError
	if !errors.As(err, &ie) || ie.Entry != "srv" {
		t.Fatalf("err = %v, want an *IdentityFileError for srv", err)
	}
	want := `entry "srv": identity_file "` + filepath.Join(dir, "gone") + `" cannot be used: the file does not exist`
	if err.Error() != want {
		t.Errorf("message =\n %s\nwant\n %s", err, want)
	}
}

// TestHandEditedIdentityFileIsRefused: the value in config.yaml goes through
// the same rule as the flag. Each refused value (written the way a person
// would write it in YAML) is refused at its use, named for its entry; a usable
// one is not.
func TestHandEditedIdentityFileIsRefused(t *testing.T) {
	dir := t.TempDir()
	key := identityKey(t, dir, "id_work")
	cases := []struct {
		name, yaml, reason string
	}{
		{"usable", strconv.Quote(key), ""},
		{"CR", `"` + key + `\r"`, "carriage return"},
		{"LF", `"` + key + `\n"`, "line feed"},
		{"NUL", `"` + key + `\0"`, "NUL"},
		{"double quote", `'` + key + `"'`, "double quote"},
		{"backslash", `'` + key + `\x'`, "backslash"},
		{"hash", `"` + key + `#x"`, "#"},
		{"percent", `"` + key + `%h"`, "%"},
		{"dollar", `"` + key + `$HOME"`, "$"},
		{"leading space", `" ` + key + `"`, "begins with whitespace"},
		{"trailing space", `"` + key + ` "`, "ends with whitespace"},
		{"only whitespace", `"   "`, "only whitespace"},
		{"relative", `id_work`, "not an absolute path"},
		{"a tilde", `~/id_work`, "not an absolute path"},
		{"a directory", strconv.Quote(dir), "directory"},
		{"missing", strconv.Quote(filepath.Join(dir, "gone")), "does not exist"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yaml")
			doc := "servers:\n  srv:\n    host: shed.example.test\n    ssh_port: 2222\n    identity_file: " + tc.yaml + "\n"
			if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
				t.Fatal(err)
			}
			cfg, err := LoadClientConfigFromPath(path)
			if err != nil {
				t.Fatalf("load: %v", err)
			}
			entry := cfg.Servers["srv"]
			args, err := entry.SSHIdentityArgs("srv")
			if tc.reason == "" {
				if err != nil || len(args) != 8 {
					t.Fatalf("SSHIdentityArgs = %q, %v; want the identity args", args, err)
				}
				return
			}
			var ie *IdentityFileError
			if !errors.As(err, &ie) {
				t.Fatalf("SSHIdentityArgs(%q) = %q, %v; want an *IdentityFileError", entry.IdentityFile, args, err)
			}
			if ie.Entry != "srv" || !strings.Contains(ie.Reason, tc.reason) {
				t.Errorf("error = %v, want srv refused for %q", err, tc.reason)
			}
		})
	}
}

// TestIdentityFileFromFlag: the flag's value is stored absolute, ~ expanded
// and nothing else rewritten; "" clears; only whitespace is refused, never
// read as "clear"; and the file must be usable now.
func TestIdentityFileFromFlag(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := os.MkdirAll(filepath.Join(home, ".ssh"), 0o700); err != nil {
		t.Fatal(err)
	}
	key := identityKey(t, filepath.Join(home, ".ssh"), "id work")

	for _, tc := range []struct {
		name, value, want, reason string
	}{
		{"empty clears", "", "", ""},
		{"an absolute path is kept as typed", key, key, ""},
		{"~/ is expanded", "~/.ssh/id work", key, ""},
		{"only whitespace", "  ", "", "only whitespace"},
		{"a relative path", ".ssh/id work", "", "not an absolute path"},
		{"~user is not expanded", "~pat/.ssh/id work", "", "not an absolute path"},
		{"a trailing space after ~ expansion", "~/.ssh/id work ", "", "ends with whitespace"},
		{"~ alone is the home directory", "~", "", "directory"},
		{"missing", "~/.ssh/gone", "", "does not exist"},
		{"a refused character", "~/.ssh/id%h", "", "%"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := IdentityFileFromFlag("srv", tc.value)
			if tc.reason == "" {
				if err != nil || got != tc.want {
					t.Fatalf("IdentityFileFromFlag(%q) = %q, %v; want %q", tc.value, got, err, tc.want)
				}
				return
			}
			var ie *IdentityFileError
			if !errors.As(err, &ie) || !strings.Contains(ie.Reason, tc.reason) {
				t.Fatalf("IdentityFileFromFlag(%q) = %q, %v; want refused for %q", tc.value, got, err, tc.reason)
			}
		})
	}
}

// TestIdentityPublicKey: the public key of an identity file is learned without
// a passphrase, from the file itself when its format carries it (a public key
// file; an OpenSSH private key, encrypted or not) or else from the .pub beside
// it; a .pub that names another key than the file's own is refused, and so is
// a key whose public half cannot be learned either way — at the flag and at
// every use.
func TestIdentityPublicKey(t *testing.T) {
	fp := func(k ssh.PublicKey) string { return ssh.FingerprintSHA256(k) }
	t.Run("an unencrypted OpenSSH key", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "id_work")
		pub := writeOpenSSHKey(t, path, nil)
		if got, err := IdentityFileKey("srv", path); err != nil || got != fp(pub) {
			t.Fatalf("IdentityFileKey = %q, %v; want %q", got, err, fp(pub))
		}
	})
	t.Run("an encrypted OpenSSH key, without its passphrase", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "id_work")
		pub := writeOpenSSHKey(t, path, []byte("correct horse"))
		if got, err := IdentityFileKey("srv", path); err != nil || got != fp(pub) {
			t.Fatalf("IdentityFileKey = %q, %v; want %q", got, err, fp(pub))
		}
		if got, err := IdentityFileFromFlag("srv", path); err != nil || got != path {
			t.Fatalf("IdentityFileFromFlag = %q, %v; want it accepted", got, err)
		}
	})
	t.Run("a public key file", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "id_work.pub")
		pub := newPub(t)
		writePub(t, path, pub)
		if got, err := IdentityFileKey("srv", path); err != nil || got != fp(pub) {
			t.Fatalf("IdentityFileKey = %q, %v; want %q", got, err, fp(pub))
		}
	})
	t.Run("a legacy encrypted PEM key with its .pub", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "id_rsa")
		writeLegacyEncryptedPEM(t, path)
		pub := newPub(t)
		writePub(t, path+".pub", pub)
		if got, err := IdentityFileKey("srv", path); err != nil || got != fp(pub) {
			t.Fatalf("IdentityFileKey = %q, %v; want the .pub's %q", got, err, fp(pub))
		}
	})
	t.Run("a legacy encrypted PEM key with no .pub is refused, at the flag and at use", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "id_rsa")
		writeLegacyEncryptedPEM(t, path)
		for what, err := range map[string]error{
			"flag": func() error { _, err := IdentityFileFromFlag("srv", path); return err }(),
			"use":  func() error { _, err := IdentityFileArgs("srv", path); return err }(),
		} {
			var ie *IdentityFileError
			if !errors.As(err, &ie) || ie.Entry != "srv" || !strings.Contains(ie.Reason, "public key cannot be learned without a passphrase") ||
				!strings.Contains(ie.Reason, ".pub") {
				t.Errorf("%s: err = %v, want a refusal saying the public key is needed", what, err)
			}
		}
	})
	t.Run("a .pub that names another key is refused", func(t *testing.T) {
		for _, encrypted := range []bool{false, true} {
			path := filepath.Join(t.TempDir(), "id_work")
			var passphrase []byte
			if encrypted {
				passphrase = []byte("pw")
			}
			own := writeOpenSSHKey(t, path, passphrase)
			other := newPub(t)
			writePub(t, path+".pub", other)
			_, err := IdentityFileKey("srv", path)
			var ie *IdentityFileError
			if !errors.As(err, &ie) || !strings.Contains(ie.Reason, fp(own)) || !strings.Contains(ie.Reason, fp(other)) {
				t.Errorf("encrypted=%v: err = %v, want a refusal naming both keys", encrypted, err)
			}
			// The negative control: the .pub of the key itself is fine.
			writePub(t, path+".pub", own)
			if got, err := IdentityFileKey("srv", path); err != nil || got != fp(own) {
				t.Errorf("encrypted=%v: with the matching .pub: %q, %v", encrypted, got, err)
			}
		}
	})
	t.Run("a file that is no key is refused", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "id_work")
		if err := os.WriteFile(path, []byte("not a key at all"), 0o600); err != nil {
			t.Fatal(err)
		}
		var ie *IdentityFileError
		if _, err := IdentityFileKey("srv", path); !errors.As(err, &ie) {
			t.Fatalf("err = %v, want an *IdentityFileError", err)
		}
	})
}

// issuedFor issues a client certificate whose subject is the SSH key
// fingerprint subject (as the server writes it), for user pat, as PEM.
func issuedFor(t *testing.T, subject string) []byte {
	t.Helper()
	dir := t.TempDir()
	ca, err := servertls.LoadOrGenerateCA(filepath.Join(dir, "ca.pem"), filepath.Join(dir, "ca.key"))
	if err != nil {
		t.Fatal(err)
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	csr, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{SignatureAlgorithm: x509.ECDSAWithSHA256}, key)
	if err != nil {
		t.Fatal(err)
	}
	der, err := ca.SignClientCSR(csr, subject, "pat", "control", "cli", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

// TestIssuedCertificateKey: with an identity file, a newly issued certificate
// is accepted only when it was made for that key (P-a): one made for another
// key, or checked against no key at all, is refused naming both keys and the
// identity file; without an identity file nothing is checked.
func TestIssuedCertificateKey(t *testing.T) {
	entryKey := ssh.FingerprintSHA256(newPub(t))
	otherKey := ssh.FingerprintSHA256(newPub(t))
	if r := IssuedCertificateKey("srv", "shed.example.test", "/home/pat/.ssh/id_work", entryKey, issuedFor(t, entryKey)); r != nil {
		t.Fatalf("a certificate for the entry's key was refused: %v", r)
	}
	r := IssuedCertificateKey("srv", "shed.example.test", "/home/pat/.ssh/id_work", entryKey, issuedFor(t, otherKey))
	if r == nil || r.Kind != RefusedIssuedOtherKey {
		t.Fatalf("a certificate for another key = %v, want RefusedIssuedOtherKey", r)
	}
	for _, want := range []string{otherKey, entryKey, `"srv"`, `"/home/pat/.ssh/id_work"`,
		"ssh authenticated with a different key than the entry's identity file",
		"IdentityFile your ssh config names for this host", "nothing was saved"} {
		if !strings.Contains(r.Error(), want) {
			t.Errorf("message lacks %q:\n%s", want, r)
		}
	}
	if r := IssuedCertificateKey("srv", "h", "/k", "", issuedFor(t, entryKey)); r == nil || r.Kind != RefusedIssuedOtherKey {
		t.Errorf("no entry key = %v, want refused (fail closed)", r)
	}
	if r := IssuedCertificateKey("srv", "h", "", "", issuedFor(t, otherKey)); r != nil {
		t.Errorf("no identity file = %v, want nothing checked", r)
	}
	if r := IssuedCertificateKey("srv", "h", "/k", entryKey, []byte("junk")); r == nil || r.Kind != RefusedIssuedUnreadable {
		t.Errorf("junk = %v, want RefusedIssuedUnreadable", r)
	}
}

// sdkIdentityError is the SDK's identity-file error, wrapped as its run wraps it.
func sdkIdentityError(path, reason string) error {
	return fmt.Errorf("sdk/bootstrap: %w", &sdkbootstrap.IdentityFileError{Path: path, Reason: reason})
}

// TestNameIdentityFileError: an identity-file error from the SDK, however it
// is wrapped, becomes the one typed refusal named for the entry; its words do
// not matter.
func TestNameIdentityFileError(t *testing.T) {
	sdkErr := sdkIdentityError("/home/pat/.ssh/key not authorized: connection refused", "the file does not exist")
	named, ok := NameIdentityFileError("srv", errors.Join(errors.New("sdk/bootstrap"), sdkErr))
	if !ok || named.Entry != "srv" || named.Reason != "the file does not exist" {
		t.Fatalf("named = %+v, %v", named, ok)
	}
	if _, ok := NameIdentityFileError("srv", errors.New("identity_file cannot be used")); ok {
		t.Error("a plain error with the same words was taken for an identity-file error")
	}
}

// TestIdentityFileErrorIsAnEntryRefusal: an identity file that cannot be used
// is a refusal of the entry (clienttoken.IsRefusal, found by type however it
// is wrapped) that withdraws what a source holds; and a stored control token
// on an entry with an identity file is refused with a message that names the
// entry, its identity file and the refetch.
func TestIdentityFileErrorIsAnEntryRefusal(t *testing.T) {
	err := fmt.Errorf("refresh: %w", &IdentityFileError{Entry: "srv", Path: "/home/pat/.ssh/id_work", Reason: "the file does not exist"})
	if !clienttoken.IsRefusal(err) {
		t.Error("an identity-file error is not a refusal")
	}
	var held interface{ InvalidatesHeld() bool }
	if !errors.As(err, &held) || !held.InvalidatesHeld() {
		t.Error("an identity-file refusal does not withdraw what is held")
	}

	stored := StoredTokenRefusal("srv", &ServerEntry{IdentityFile: "/home/pat/.ssh/id_work"})
	want := `entry "srv" cannot be used: it holds a control token, which does not say which SSH key it was issued for, ` +
		`and the entry uses only its identity_file "/home/pat/.ssh/id_work"; no request was sent. ` +
		`Enrol it again: shed server update srv --refetch`
	if got := stored.Error(); got != want {
		t.Errorf("message:\n got %q\nwant %q", got, want)
	}
	if !clienttoken.IsRefusal(stored) {
		t.Error("the stored-token refusal is not a refusal")
	}
}

// TestIdentityFileRoundTrips: the field is saved and read back as it is, and
// an entry without it saves exactly as it did before the field existed (no
// identity_file key at all).
func TestIdentityFileRoundTrips(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	cfg, err := LoadClientConfigFromPath(path)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Servers["with"] = ServerEntry{Host: "shed.example.test", SSHPort: 2222, IdentityFile: "/home/pat/.ssh/pat's key: work"}
	cfg.Servers["without"] = ServerEntry{Host: "shed.example.test", SSHPort: 2222}
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(data), "identity_file"); n != 1 {
		t.Errorf("config.yaml has identity_file %d time(s), want once (only the entry that sets it):\n%s", n, data)
	}
	loaded, err := LoadClientConfigFromPath(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := loaded.Servers["with"].IdentityFile; got != "/home/pat/.ssh/pat's key: work" {
		t.Errorf("identity_file read back as %q", got)
	}
}
