package users

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
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

// has reports whether the lookup resolves key to a user, asked by fingerprint
// as both front doors ask.
func has(l Lookup, key gossh.PublicKey) bool {
	return HasKey(l, gossh.FingerprintSHA256(key))
}

func TestAllowlistOffByDefault(t *testing.T) {
	a, err := NewConfigLookup(ConfigOptions{GitHubCacheDir: t.TempDir()})
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

	a, err := NewConfigLookup(ConfigOptions{SSH: &config.SSHAuthConfig{
		Mode:           config.SSHAuthEnforce,
		AuthorizedKeys: []string{listedLine},
	}, GitHubCacheDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if !has(a, listed) {
		t.Error("listed key should be authorized")
	}
	if has(a, unlisted) {
		t.Error("unlisted key must not be authorized")
	}
}

func TestEnforceEmptyFailsClosed(t *testing.T) {
	// enforce with no resolvable keys must error (fail closed), never
	// silently start accept-all or with an empty allowlist.
	if _, err := NewConfigLookup(ConfigOptions{SSH: &config.SSHAuthConfig{Mode: config.SSHAuthEnforce}, GitHubCacheDir: t.TempDir()}); err == nil {
		t.Fatal("expected error for enforce with no keys")
	}
}

func TestWarnEmptyIsAllowed(t *testing.T) {
	// warn mode tolerates an empty allowlist (it accepts anyway).
	a, err := NewConfigLookup(ConfigOptions{SSH: &config.SSHAuthConfig{Mode: config.SSHAuthWarn}, GitHubCacheDir: t.TempDir()})
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
	a, err := NewConfigLookup(ConfigOptions{SSH: &config.SSHAuthConfig{
		Mode:        config.SSHAuthEnforce,
		GitHubUsers: []string{"octocat"},
	}, GitHubCacheDir: cacheDir})
	if err != nil {
		t.Fatalf("initial fetch should resolve keys: %v", err)
	}
	if !has(a, listed) {
		t.Fatal("fetched github key should be authorized")
	}

	// GitHub now down: a rebuild must keep the cached key (fail closed),
	// not drop it.
	failFetch.Store(true)
	if err := a.rebuild(); err != nil {
		t.Fatalf("rebuild should not error on github outage: %v", err)
	}
	if !has(a, listed) {
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
	if !has(a, listed) {
		t.Error("key should survive github outage + missing cache via in-memory snapshot")
	}
}

func TestStartRefreshNoOpWhenNothingToRefresh(t *testing.T) {
	// The refresh loop only exists to re-resolve GitHub keys. With auth off, or
	// with no GitHub users, StartRefresh must return without spawning a ticker —
	// calling it has to be safe and non-blocking in those cases.
	off, err := NewConfigLookup(ConfigOptions{GitHubCacheDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	off.StartRefresh(context.Background()) // off mode: no-op

	_, line := genKey(t)
	inline, err := NewConfigLookup(ConfigOptions{SSH: &config.SSHAuthConfig{
		Mode:           config.SSHAuthEnforce,
		AuthorizedKeys: []string{line},
	}, GitHubCacheDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	inline.StartRefresh(context.Background()) // enforce but no github users: no-op
}

func TestGitHubInvalidUsernameSkipped(t *testing.T) {
	// A path-traversal-ish username is rejected before any fetch, so enforce
	// resolves no keys and fails closed.
	if _, err := NewConfigLookup(ConfigOptions{SSH: &config.SSHAuthConfig{
		Mode:        config.SSHAuthEnforce,
		GitHubUsers: []string{"../etc/passwd"},
	}, GitHubCacheDir: t.TempDir()}); err == nil {
		t.Fatal("expected fail-closed error: invalid username yields no keys")
	}
}

func TestMaxAuthTriesPassthrough(t *testing.T) {
	a, err := NewConfigLookup(ConfigOptions{SSH: &config.SSHAuthConfig{
		Mode:           config.SSHAuthWarn,
		MaxAuthTries:   3,
		AuthorizedKeys: nil,
	}, GitHubCacheDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if a.MaxAuthTries() != 3 {
		t.Errorf("MaxAuthTries() = %d, want 3", a.MaxAuthTries())
	}
}

// The rebuild tests below pin how a key leaves the allowlist. An mtls server
// re-checks every request's certificate CN (the key fingerprint) against this
// set, so a rebuild that drops a key is what de-authorizes its certificates,
// and a rebuild that wrongly drops one locks a user out. (These replaced the
// revoke-hook tests when the bearer-token store, the hook's only consumer, was
// deleted.)

func TestRebuildDropsARemovedKey(t *testing.T) {
	k1, line1 := genKey(t)
	k2, line2 := genKey(t)
	a, err := NewConfigLookup(ConfigOptions{SSH: &config.SSHAuthConfig{
		Mode:           config.SSHAuthEnforce,
		AuthorizedKeys: []string{line1, line2},
	}, GitHubCacheDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}

	// Drop k2 from the inline source and rebuild → only k2 leaves.
	a.inline = []string{line1}
	if err := a.rebuild(); err != nil {
		t.Fatal(err)
	}
	if !has(a, k1) {
		t.Error("k1 should still be authorized")
	}
	if has(a, k2) {
		t.Error("k2 should no longer be authorized")
	}
}

func TestRebuildKeepsKeysOnFailedRefetch(t *testing.T) {
	// A transient GitHub outage must never drop a key: the fail-closed fallback
	// keeps the key set unchanged.
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

	a, err := NewConfigLookup(ConfigOptions{SSH: &config.SSHAuthConfig{
		Mode:        config.SSHAuthEnforce,
		GitHubUsers: []string{"octocat"},
	}, GitHubCacheDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}

	failFetch.Store(true)
	if err := a.rebuild(); err != nil {
		t.Fatal(err)
	}
	if !has(a, listed) {
		t.Error("key should be retained via last-known-good")
	}
}

func TestRebuildDropsAKeyGitHubNoLongerServes(t *testing.T) {
	// The realistic rotation: the user removes a key from their GitHub account;
	// the next successful fetch returns fewer keys → the dropped key leaves.
	k1, line1 := genKey(t)
	k2, line2 := genKey(t)
	var served atomic.Pointer[string]
	both := line1 + line2
	served.Store(&both)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(*served.Load()))
	}))
	defer srv.Close()
	orig := githubKeysBaseURL
	githubKeysBaseURL = srv.URL
	defer func() { githubKeysBaseURL = orig }()

	a, err := NewConfigLookup(ConfigOptions{SSH: &config.SSHAuthConfig{
		Mode:        config.SSHAuthEnforce,
		GitHubUsers: []string{"octocat"},
	}, GitHubCacheDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if !has(a, k1) || !has(a, k2) {
		t.Fatal("both keys should be authorized initially")
	}

	only1 := line1
	served.Store(&only1)
	if err := a.rebuild(); err != nil {
		t.Fatal(err)
	}
	if !has(a, k1) {
		t.Error("k1 should still be authorized")
	}
	if has(a, k2) {
		t.Error("k2 should have left the allowlist (dropped from github)")
	}
}

func TestRebuildPrefersInMemoryOverStaleDiskCache(t *testing.T) {
	// Regression (Codex 1e): when writeCache fails and the disk cache is staler
	// than the in-memory snapshot, a transient fetch failure must fall back to
	// the in-memory last-known-good, NOT the stale disk — otherwise it would
	// falsely shrink the set and de-authorize a still-valid key.
	k1, line1 := genKey(t)
	k2, line2 := genKey(t)

	cacheDir := t.TempDir()
	cachePath := filepath.Join(cacheDir, "octocat.keys")
	// Stale on-disk cache: only k1, and read-only so writeCache can't refresh it.
	if err := os.WriteFile(cachePath, []byte(line1), 0o400); err != nil {
		t.Fatal(err)
	}

	var failFetch atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if failFetch.Load() {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		_, _ = w.Write([]byte(line1 + line2)) // fresh: k1 + k2
	}))
	defer srv.Close()
	orig := githubKeysBaseURL
	githubKeysBaseURL = srv.URL
	defer func() { githubKeysBaseURL = orig }()

	a, err := NewConfigLookup(ConfigOptions{SSH: &config.SSHAuthConfig{Mode: config.SSHAuthEnforce, GitHubUsers: []string{"octocat"}}, GitHubCacheDir: cacheDir})
	if err != nil {
		t.Fatal(err)
	}
	// First fetch resolved k1+k2 into the in-memory snapshot; writeCache failed
	// (0400 file), so disk is still the stale k1-only.
	if !has(a, k1) || !has(a, k2) {
		t.Fatal("both keys should be authorized after the successful fetch")
	}

	// Transient failure: must use the in-memory last-known-good (k1+k2), not the
	// stale disk cache (k1 only).
	failFetch.Store(true)
	if err := a.rebuild(); err != nil {
		t.Fatal(err)
	}
	if !has(a, k2) {
		t.Error("k2 must be retained via the in-memory snapshot")
	}
}

// TestUserForKeyFingerprint pins the contract the mtls HTTP path depends on:
// the lookup answers for exactly the canonical gossh.FingerprintSHA256 string
// (which is what the bootstrap burns into an issued certificate's Subject CN),
// and it fails closed on anything else.
func TestUserForKeyFingerprint(t *testing.T) {
	listed, listedLine := genKey(t)
	unlisted, _ := genKey(t)

	a, err := NewConfigLookup(ConfigOptions{SSH: &config.SSHAuthConfig{
		Mode:           config.SSHAuthEnforce,
		AuthorizedKeys: []string{listedLine},
	}, GitHubCacheDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}

	listedFP := gossh.FingerprintSHA256(listed)
	if !HasKey(a, listedFP) {
		t.Errorf("listed fingerprint %s should be authorized", listedFP)
	}
	if HasKey(a, gossh.FingerprintSHA256(unlisted)) {
		t.Error("unlisted fingerprint must not be authorized")
	}
	// Fail closed on the empty string and on near-miss spellings — an mtls
	// certificate CN is attacker-visible, and only the exact canonical form
	// may match.
	for _, bogus := range []string{
		"",
		listedFP + "=",                          // padded
		strings.TrimPrefix(listedFP, "SHA256:"), // prefix stripped
		strings.ToLower(listedFP),               // case-folded
		"MD5:" + strings.TrimPrefix(listedFP, "SHA256:"), // wrong hash label
	} {
		if HasKey(a, bogus) {
			t.Errorf("non-canonical fingerprint %q must not be authorized", bogus)
		}
	}

	// The lookup tracks a rebuild: dropping the key from the source
	// de-authorizes it for the HTTP path on the very next call, which is how
	// mtls certificate revocation lands without a CRL.
	_, otherLine := genKey(t)
	a.inline = []string{otherLine}
	if err := a.rebuild(); err != nil {
		t.Fatal(err)
	}
	if HasKey(a, listedFP) {
		t.Error("removed key's fingerprint is still authorized after rebuild")
	}
}

// TestUserForKeyOffModeAuthorizesNothing: in off mode the SSH door is
// accept-all and the key set is empty. The lookup reports membership only, so
// it authorizes nobody — callers that use it as an authorization oracle (the
// mtls middleware) fail closed rather than inheriting SSH's accept-all posture.
func TestUserForKeyOffModeAuthorizesNothing(t *testing.T) {
	key, line := genKey(t)
	a, err := NewConfigLookup(ConfigOptions{SSH: &config.SSHAuthConfig{
		Mode:           config.SSHAuthOff,
		AuthorizedKeys: []string{line},
	}, GitHubCacheDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if HasKey(a, gossh.FingerprintSHA256(key)) {
		t.Error("off mode must not authorize any fingerprint")
	}
}

// TestKeysReportTheirSource: with no user list every key belongs to the
// implicit user, and each one still says where it came from — an inline key,
// the keys file (with its path), or a GitHub login (with the login). A key
// listed in two sources reports the first: inline, then the file, then the
// GitHub logins in config order.
func TestKeysReportTheirSource(t *testing.T) {
	inlineKey, inlineLine := genKey(t)
	fileKey, fileLine := genKey(t)
	octoKey, octoLine := genKey(t)
	hubotKey, hubotLine := genKey(t)
	sharedKey, sharedLine := genKey(t)

	keysFile := filepath.Join(t.TempDir(), "authorized_keys")
	fileData := strings.TrimSuffix(fileLine, "\n") + " laptop\n" + sharedLine
	if err := os.WriteFile(keysFile, []byte(fileData), 0o600); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/octocat.keys":
			_, _ = w.Write([]byte(octoLine + sharedLine))
		case "/hubot.keys":
			_, _ = w.Write([]byte(hubotLine + octoLine))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	orig := githubKeysBaseURL
	githubKeysBaseURL = srv.URL
	defer func() { githubKeysBaseURL = orig }()

	a, err := NewConfigLookup(ConfigOptions{SSH: &config.SSHAuthConfig{
		Mode:               config.SSHAuthEnforce,
		AuthorizedKeys:     []string{inlineLine},
		AuthorizedKeysFile: keysFile,
		GitHubUsers:        []string{"octocat", "hubot"},
	}, GitHubCacheDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name   string
		key    gossh.PublicKey
		source Source
		origin string
		label  string
	}{
		{"inline", inlineKey, SourceInline, "", ""},
		{"file, with its comment", fileKey, SourceFile, keysFile, "laptop"},
		{"github login", octoKey, SourceGitHub, "octocat", ""},
		{"second github login", hubotKey, SourceGitHub, "hubot", ""},
		{"listed in the file and on github: the file wins", sharedKey, SourceFile, keysFile, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fp := gossh.FingerprintSHA256(tc.key)
			u, k, ok := a.UserForKey(fp)
			if !ok {
				t.Fatal("key should resolve")
			}
			if u != Implicit() {
				t.Errorf("user = %+v, want the implicit user", u)
			}
			want := Key{Fingerprint: fp, Source: tc.source, Origin: tc.origin, Label: tc.label}
			if k != want {
				t.Errorf("key = %+v, want %+v", k, want)
			}
		})
	}
}

// TestImplicitUserIsTheOnlyUser: with no user list the lookup knows exactly one
// user, the implicit one, named "", an admin who passes every check — in every
// key-auth mode.
func TestImplicitUserIsTheOnlyUser(t *testing.T) {
	_, line := genKey(t)
	for _, ssh := range []*config.SSHAuthConfig{
		nil,
		{Mode: config.SSHAuthWarn},
		{Mode: config.SSHAuthEnforce, AuthorizedKeys: []string{line}},
	} {
		a, err := NewConfigLookup(ConfigOptions{SSH: ssh, GitHubCacheDir: t.TempDir()})
		if err != nil {
			t.Fatal(err)
		}
		want := User{Name: "", Admin: true, Implicit: true}
		if got := a.Users(); len(got) != 1 || got[0] != want {
			t.Errorf("Users() = %+v, want just %+v", got, want)
		}
		if got, ok := a.User(""); !ok || got != want {
			t.Errorf("User(\"\") = %+v, %v; want %+v", got, ok, want)
		}
		if _, ok := a.User("alice"); ok {
			t.Error("User(alice) should not exist without a user list")
		}
	}
}

// TestRebuildRacesLookups runs rebuilds (the background refresh) against
// lookups (both front doors) and against each other. Under -race it proves the
// live set is swapped, never mutated in place, and that concurrent rebuilds
// serialize on the per-login last-known-good map.
func TestRebuildRacesLookups(t *testing.T) {
	key, line := genKey(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(line))
	}))
	defer srv.Close()
	orig := githubKeysBaseURL
	githubKeysBaseURL = srv.URL
	defer func() { githubKeysBaseURL = orig }()

	a, err := NewConfigLookup(ConfigOptions{SSH: &config.SSHAuthConfig{
		Mode:        config.SSHAuthEnforce,
		GitHubUsers: []string{"octocat"},
	}, GitHubCacheDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	fp := gossh.FingerprintSHA256(key)

	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				if err := a.rebuild(); err != nil {
					t.Error(err)
				}
			}
		}()
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				if !HasKey(a, fp) {
					t.Error("key dropped during a rebuild")
					return
				}
			}
		}()
	}
	wg.Wait()
}
