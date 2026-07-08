package sshd

import (
	"crypto/ed25519"
	"crypto/rand"
	"testing"
	"time"

	gossh "golang.org/x/crypto/ssh"

	"github.com/charliek/shed/internal/authtoken"
	"github.com/charliek/shed/internal/config"
)

func testKey(t *testing.T) gossh.PublicKey {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := gossh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	return signer.PublicKey()
}

// enforceServer builds an enforce-mode server whose allowlist authorizes the
// given key, with bootstrap wired to a fresh token store.
func enforceServer(t *testing.T, authorized gossh.PublicKey) *Server {
	t.Helper()
	al, err := NewKeyAllowlist(&config.SSHAuthConfig{
		Mode:           config.SSHAuthEnforce,
		AuthorizedKeys: []string{string(gossh.MarshalAuthorizedKey(authorized))},
	}, "")
	if err != nil {
		t.Fatalf("allowlist: %v", err)
	}
	s := &Server{allowlist: al}
	s.SetBootstrap(authtoken.NewStore(), BootstrapInfo{
		HTTPPort:       8080,
		HTTPSPort:      8443,
		TLSFingerprint: "sha256:abc",
		TokenTTL:       time.Hour,
	})
	return s
}

func TestMintBootstrapAuthorizedKey(t *testing.T) {
	key := testKey(t)
	s := enforceServer(t, key)

	bundle, err := s.mintBootstrap(key, "control")
	if err != nil {
		t.Fatalf("mintBootstrap: %v", err)
	}
	if bundle.Scope != authtoken.ScopeControl {
		t.Errorf("scope = %q, want control", bundle.Scope)
	}
	if bundle.HTTPSPort != 8443 || bundle.TLSCertFingerprint != "sha256:abc" || bundle.HTTPPort != 8080 {
		t.Errorf("bundle metadata not propagated: %+v", bundle)
	}
	if bundle.Token == "" || bundle.TokenID == "" {
		t.Error("empty token or token_id")
	}
	// The minted token validates in the shared store as control, bound to the
	// requesting key's fingerprint.
	rec, ok := s.tokens.Validate(bundle.Token)
	if !ok || rec.Scope != authtoken.ScopeControl {
		t.Errorf("minted token does not validate as control: ok=%v rec=%+v", ok, rec)
	}
	if rec.Subject != gossh.FingerprintSHA256(key) {
		t.Errorf("token subject = %q, want key fingerprint %q", rec.Subject, gossh.FingerprintSHA256(key))
	}
}

func TestMintBootstrapDefaultsToControl(t *testing.T) {
	key := testKey(t)
	s := enforceServer(t, key)
	bundle, err := s.mintBootstrap(key, "")
	if err != nil {
		t.Fatal(err)
	}
	if bundle.Scope != authtoken.ScopeControl {
		t.Errorf("empty request should default to control, got %q", bundle.Scope)
	}
}

func TestMintBootstrapCredentialsScopeAndKind(t *testing.T) {
	key := testKey(t)
	s := enforceServer(t, key)
	bundle, err := s.mintBootstrap(key, "credentials host-agent")
	if err != nil {
		t.Fatal(err)
	}
	if bundle.Scope != authtoken.ScopeCredentials {
		t.Errorf("scope = %q, want credentials", bundle.Scope)
	}
	rec, ok := s.tokens.Validate(bundle.Token)
	if !ok || rec.ClientKind != authtoken.ClientHostAgent {
		t.Errorf("client kind = %q, want host-agent (ok=%v)", rec.ClientKind, ok)
	}
}

func TestMintBootstrapRejectsUnauthorizedKey(t *testing.T) {
	s := enforceServer(t, testKey(t))
	if _, err := s.mintBootstrap(testKey(t), "control"); err == nil {
		t.Fatal("expected error for unauthorized key")
	}
}

func TestMintBootstrapRequiresEnforce(t *testing.T) {
	key := testKey(t)
	// In warn mode an unlisted key is admitted at the transport, so the mint
	// handler must independently refuse outside enforce.
	al, err := NewKeyAllowlist(&config.SSHAuthConfig{
		Mode:           config.SSHAuthWarn,
		AuthorizedKeys: []string{string(gossh.MarshalAuthorizedKey(key))},
	}, "")
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{allowlist: al}
	s.SetBootstrap(authtoken.NewStore(), BootstrapInfo{TokenTTL: time.Hour})
	if _, err := s.mintBootstrap(key, "control"); err == nil {
		t.Fatal("expected error: bootstrap must require enforce mode")
	}
}

func TestMintBootstrapRejectsInvalidScope(t *testing.T) {
	key := testKey(t)
	s := enforceServer(t, key)
	if _, err := s.mintBootstrap(key, "admin"); err == nil {
		t.Fatal("expected error for admin (removed) scope")
	}
}

func TestMintBootstrapRejectsNilKey(t *testing.T) {
	s := enforceServer(t, testKey(t))
	if _, err := s.mintBootstrap(nil, "control"); err == nil {
		t.Fatal("expected error for nil key")
	}
}

func TestMintBootstrapUnconfigured(t *testing.T) {
	key := testKey(t)
	al, _ := NewKeyAllowlist(&config.SSHAuthConfig{
		Mode:           config.SSHAuthEnforce,
		AuthorizedKeys: []string{string(gossh.MarshalAuthorizedKey(key))},
	}, "")
	s := &Server{allowlist: al} // no SetBootstrap
	if _, err := s.mintBootstrap(key, "control"); err == nil {
		t.Fatal("expected error when token issuance not configured")
	}
}
