package sshd

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	gossh "golang.org/x/crypto/ssh"

	"github.com/charliek/shed/internal/config"
)

// genKey returns a fresh ed25519 SSH public key and its authorized_keys line.
func genKey(t *testing.T) (gossh.PublicKey, string) {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := gossh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	pub := signer.PublicKey()
	return pub, string(gossh.MarshalAuthorizedKey(pub))
}

func TestAllowlistOffByDefault(t *testing.T) {
	a, err := NewKeyAllowlist(nil, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if a.Mode() != config.SSHAuthOff {
		t.Errorf("Mode() = %q, want off", a.Mode())
	}
}

func TestEnforceMatchesInlineKeys(t *testing.T) {
	listed, listedLine := genKey(t)
	unlisted, _ := genKey(t)

	a, err := NewKeyAllowlist(&config.SSHAuthConfig{
		Mode:           config.SSHAuthEnforce,
		AuthorizedKeys: []string{listedLine},
	}, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if !a.IsAuthorized(listed) {
		t.Error("listed key should be authorized")
	}
	if a.IsAuthorized(unlisted) {
		t.Error("unlisted key must not be authorized")
	}
}

func TestEnforceEmptyFailsClosed(t *testing.T) {
	// enforce with no resolvable keys must error (fail closed), never
	// silently start accept-all or with an empty allowlist.
	if _, err := NewKeyAllowlist(&config.SSHAuthConfig{Mode: config.SSHAuthEnforce}, t.TempDir()); err == nil {
		t.Fatal("expected error for enforce with no keys")
	}
}

func TestWarnEmptyIsAllowed(t *testing.T) {
	// warn mode tolerates an empty allowlist (it accepts anyway).
	a, err := NewKeyAllowlist(&config.SSHAuthConfig{Mode: config.SSHAuthWarn}, t.TempDir())
	if err != nil {
		t.Fatalf("warn with empty keys should not error: %v", err)
	}
	if a.Mode() != config.SSHAuthWarn {
		t.Errorf("Mode() = %q, want warn", a.Mode())
	}
}

func TestGitHubSeedAndFailClosedToCache(t *testing.T) {
	listed, listedLine := genKey(t)

	var failFetch atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if failFetch.Load() {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		_, _ = w.Write([]byte(listedLine))
	}))
	defer srv.Close()

	orig := githubKeysBaseURL
	githubKeysBaseURL = srv.URL
	defer func() { githubKeysBaseURL = orig }()

	cacheDir := t.TempDir()
	a, err := NewKeyAllowlist(&config.SSHAuthConfig{
		Mode:        config.SSHAuthEnforce,
		GitHubUsers: []string{"octocat"},
	}, cacheDir)
	if err != nil {
		t.Fatalf("initial fetch should resolve keys: %v", err)
	}
	if !a.IsAuthorized(listed) {
		t.Fatal("fetched github key should be authorized")
	}

	// GitHub now down: a rebuild must keep the cached key (fail closed),
	// not drop it.
	failFetch.Store(true)
	if err := a.rebuild(); err != nil {
		t.Fatalf("rebuild should not error on github outage: %v", err)
	}
	if !a.IsAuthorized(listed) {
		t.Error("key should survive a github outage via the cache")
	}

	// Disk cache gone *and* fetch still failing: the in-memory last-known-good
	// must keep the key (a refresh must never silently drop authorized keys).
	if err := os.Remove(filepath.Join(cacheDir, "octocat.keys")); err != nil {
		t.Fatal(err)
	}
	if err := a.rebuild(); err != nil {
		t.Fatalf("rebuild should not error: %v", err)
	}
	if !a.IsAuthorized(listed) {
		t.Error("key should survive github outage + missing cache via in-memory snapshot")
	}
}

func TestStartRefreshNoOpWhenNothingToRefresh(t *testing.T) {
	// The refresh loop only exists to re-resolve GitHub keys. With auth off, or
	// with no GitHub users, StartRefresh must return without spawning a ticker —
	// calling it has to be safe and non-blocking in those cases.
	off, err := NewKeyAllowlist(nil, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	off.StartRefresh(context.Background()) // off mode: no-op

	_, line := genKey(t)
	inline, err := NewKeyAllowlist(&config.SSHAuthConfig{
		Mode:           config.SSHAuthEnforce,
		AuthorizedKeys: []string{line},
	}, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	inline.StartRefresh(context.Background()) // enforce but no github users: no-op
}

func TestGitHubInvalidUsernameSkipped(t *testing.T) {
	// A path-traversal-ish username is rejected before any fetch, so enforce
	// resolves no keys and fails closed.
	if _, err := NewKeyAllowlist(&config.SSHAuthConfig{
		Mode:        config.SSHAuthEnforce,
		GitHubUsers: []string{"../etc/passwd"},
	}, t.TempDir()); err == nil {
		t.Fatal("expected fail-closed error: invalid username yields no keys")
	}
}

func TestMaxAuthTriesPassthrough(t *testing.T) {
	a, err := NewKeyAllowlist(&config.SSHAuthConfig{
		Mode:           config.SSHAuthWarn,
		MaxAuthTries:   3,
		AuthorizedKeys: nil,
	}, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if a.MaxAuthTries() != 3 {
		t.Errorf("MaxAuthTries() = %d, want 3", a.MaxAuthTries())
	}
}
