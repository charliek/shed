package sshd

import (
	"crypto/ed25519"
	"crypto/rand"
	"path/filepath"
	"testing"
	"time"

	gossh "golang.org/x/crypto/ssh"

	"github.com/charliek/shed/internal/config"
	"github.com/charliek/shed/internal/users"
	"github.com/charliek/shed/internal/users/userstest"
)

func testSigner(t *testing.T) gossh.Signer {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := gossh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	return signer
}

// dialAs completes an SSH handshake with signer and reports whether the server
// accepted the key. No channel is opened, so no backend is needed.
func dialAs(t *testing.T, addr string, signer gossh.Signer) bool {
	t.Helper()
	client, err := gossh.Dial("tcp", addr, &gossh.ClientConfig{
		User:            "some-shed",
		Auth:            []gossh.AuthMethod{gossh.PublicKeys(signer)},
		HostKeyCallback: gossh.InsecureIgnoreHostKey(),
		Timeout:         10 * time.Second,
	})
	if err != nil {
		return false
	}
	_ = client.Close()
	return true
}

// TestPublicKeyAuthOnTheWire is the SSH front door: a key is admitted when the
// user lookup resolves its fingerprint, in enforce mode an unresolved key is
// refused, and off and warn admit every key, as before the lookup existed.
func TestPublicKeyAuthOnTheWire(t *testing.T) {
	listed := testSigner(t)
	stranger := testSigner(t)
	lookup := userstest.Implicit(gossh.FingerprintSHA256(listed.PublicKey()))

	for _, tc := range []struct {
		name         string
		auth         KeyAuth
		wantListed   bool
		wantStranger bool
	}{
		{"zero value is off", KeyAuth{}, true, true},
		{"off", KeyAuth{Mode: config.SSHAuthOff, Users: lookup}, true, true},
		{"warn", KeyAuth{Mode: config.SSHAuthWarn, Users: lookup}, true, true},
		{"enforce", KeyAuth{Mode: config.SSHAuthEnforce, Users: lookup}, true, false},
		{"enforce with no lookup accepts nothing", KeyAuth{Mode: config.SSHAuthEnforce}, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, err := NewServer(nil, filepath.Join(t.TempDir(), "host_key"), "127.0.0.1:0", nil, tc.auth)
			if err != nil {
				t.Fatal(err)
			}
			addr := serveForTest(t, s)
			if got := dialAs(t, addr, listed); got != tc.wantListed {
				t.Errorf("listed key admitted = %v, want %v", got, tc.wantListed)
			}
			if got := dialAs(t, addr, stranger); got != tc.wantStranger {
				t.Errorf("unlisted key admitted = %v, want %v", got, tc.wantStranger)
			}
		})
	}
}

// TestPublicKeyAuthReadsTheLiveLookup: the door asks the lookup on every
// connection, so a key that leaves its source stops opening connections as
// soon as the lookup stops resolving it.
func TestPublicKeyAuthReadsTheLiveLookup(t *testing.T) {
	signer := testSigner(t)
	fp := gossh.FingerprintSHA256(signer.PublicKey())
	lookup := userstest.Implicit(fp)
	s, err := NewServer(nil, filepath.Join(t.TempDir(), "host_key"), "127.0.0.1:0", nil,
		KeyAuth{Mode: config.SSHAuthEnforce, Users: lookup})
	if err != nil {
		t.Fatal(err)
	}
	addr := serveForTest(t, s)
	if !dialAs(t, addr, signer) {
		t.Fatal("listed key refused")
	}
	lookup.RemoveKey(fp)
	if dialAs(t, addr, signer) {
		t.Error("key admitted after it left the lookup")
	}
}

// TestPublicKeyAuthWithTheConfigLookup wires the door to the production lookup
// built from an auth.ssh block, as shed-server does.
func TestPublicKeyAuthWithTheConfigLookup(t *testing.T) {
	listed := testSigner(t)
	stranger := testSigner(t)
	lookup, err := users.NewConfigLookup(users.ConfigOptions{SSH: &config.SSHAuthConfig{
		Mode:           config.SSHAuthEnforce,
		AuthorizedKeys: []string{string(gossh.MarshalAuthorizedKey(listed.PublicKey()))},
	}})
	if err != nil {
		t.Fatal(err)
	}
	s, err := NewServer(nil, filepath.Join(t.TempDir(), "host_key"), "127.0.0.1:0", nil,
		KeyAuth{Mode: lookup.Mode(), MaxAuthTries: lookup.MaxAuthTries(), Users: lookup})
	if err != nil {
		t.Fatal(err)
	}
	addr := serveForTest(t, s)
	if !dialAs(t, addr, listed) {
		t.Error("listed key refused")
	}
	if dialAs(t, addr, stranger) {
		t.Error("unlisted key admitted")
	}
}

// TestEffectiveMaxAuthTries: the configured cap passes through; unset applies
// the shed default.
func TestEffectiveMaxAuthTries(t *testing.T) {
	if got := (&Server{}).effectiveMaxAuthTries(); got != defaultMaxAuthTries {
		t.Errorf("unset: got %d, want %d", got, defaultMaxAuthTries)
	}
	if got := (&Server{keyAuth: KeyAuth{MaxAuthTries: 3}}).effectiveMaxAuthTries(); got != 3 {
		t.Errorf("configured 3: got %d", got)
	}
}
