package sshd

import (
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	gossh "golang.org/x/crypto/ssh"

	"github.com/charliek/shed/internal/config"
)

const (
	defaultGitHubRefresh = time.Hour
	githubFetchTimeout   = 15 * time.Second
	githubKeysMaxBytes   = 1 << 20 // 1 MiB ceiling on a .keys response
)

// githubKeysBaseURL is the GitHub base for `<user>.keys`; overridable in tests.
var githubKeysBaseURL = "https://github.com"

// KeyAllowlist is the SSH public-key allowlist, resolved from inline keys, an
// authorized_keys file, and GitHub users. GitHub keys are cached to disk and
// fail closed to the last-known-good cache when a refetch fails, so a GitHub
// outage never silently empties the allowlist.
type KeyAllowlist struct {
	mode         string
	maxAuthTries int

	inline   []string
	file     string
	users    []string
	cacheDir string
	refresh  time.Duration
	client   *http.Client

	// lastGitHub holds each GitHub user's last successfully-resolved .keys
	// bytes — the ultimate last-known-good fallback when both the live fetch
	// and the disk cache fail, so a refresh can never silently drop keys that
	// were valid moments ago. Only touched by rebuild() (never concurrent).
	lastGitHub map[string][]byte

	mu   sync.RWMutex
	keys map[string]string // marshaled public key -> SHA-256 fingerprint

	// onRemove, when set, is called (outside the lock) with the fingerprints of
	// keys that left the allowlist on a rebuild, so a caller can revoke their
	// HTTP tokens. Set once at startup before StartRefresh; nil on the off path.
	onRemove func(subjects []string)
}

// NewKeyAllowlist builds the allowlist from config and does the initial GitHub
// fetch (falling back to cache). cacheDir is where GitHub .keys are cached.
//
// Returns an error when mode==enforce but no keys resolved — failing closed
// rather than starting a server that would lock the operator out (and must not
// silently fall back to accept-all).
func NewKeyAllowlist(cfg *config.SSHAuthConfig, cacheDir string) (*KeyAllowlist, error) {
	a := &KeyAllowlist{
		mode:       config.SSHAuthOff,
		keys:       map[string]string{},
		lastGitHub: map[string][]byte{},
	}
	if cfg == nil || cfg.Mode == "" || cfg.Mode == config.SSHAuthOff {
		return a, nil // off: accept-all, nothing to resolve
	}

	a.mode = cfg.Mode
	a.maxAuthTries = cfg.MaxAuthTries
	a.inline = cfg.AuthorizedKeys
	a.file = config.ExpandPath(cfg.AuthorizedKeysFile)
	a.users = cfg.GitHubUsers
	a.cacheDir = cacheDir
	a.client = &http.Client{Timeout: githubFetchTimeout}
	a.refresh = time.Duration(cfg.GitHubRefresh)
	if a.refresh <= 0 {
		a.refresh = defaultGitHubRefresh
	}

	if err := a.rebuild(); err != nil {
		return nil, err
	}
	if a.mode == config.SSHAuthEnforce && a.size() == 0 {
		return nil, fmt.Errorf(
			"auth.ssh.mode=enforce but no authorized keys resolved " +
				"(inline/file empty and GitHub fetch failed with no cache); " +
				"use mode=warn for first boot or provide authorized_keys")
	}
	return a, nil
}

// Mode returns the allowlist mode (off/warn/enforce).
func (a *KeyAllowlist) Mode() string { return a.mode }

// MaxAuthTries returns the configured per-connection public-key attempt cap, or
// 0 when unset (the server then applies its own default — see effectiveMaxAuthTries).
func (a *KeyAllowlist) MaxAuthTries() int { return a.maxAuthTries }

// SetRevokeHook installs a callback invoked (outside the lock) with the
// fingerprints of keys that leave the allowlist on a rebuild, so the caller can
// revoke their HTTP tokens. Set once at startup, before StartRefresh.
func (a *KeyAllowlist) SetRevokeHook(fn func(subjects []string)) { a.onRemove = fn }

// IsAuthorized reports whether key is in the allowlist.
func (a *KeyAllowlist) IsAuthorized(key gossh.PublicKey) bool {
	a.mu.RLock()
	defer a.mu.RUnlock()
	_, ok := a.keys[string(key.Marshal())]
	return ok
}

func (a *KeyAllowlist) size() int {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return len(a.keys)
}

// StartRefresh launches a background goroutine that periodically re-resolves
// the GitHub-seeded keys, until ctx is cancelled. No-op when there are no
// GitHub users to refresh.
func (a *KeyAllowlist) StartRefresh(ctx context.Context) {
	if a.mode == config.SSHAuthOff || len(a.users) == 0 {
		return
	}
	go func() {
		t := time.NewTicker(a.refresh)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if err := a.rebuild(); err != nil {
					log.Printf("auth.ssh: key refresh failed: %v", err)
				}
			}
		}
	}()
}

// rebuild re-resolves the full key set from all sources. Only a keys-file read
// error is fatal (a config error); GitHub fetch failures fall back to cache.
func (a *KeyAllowlist) rebuild() error {
	set := make(map[string]string)
	addAuthorizedKeys(set, []byte(strings.Join(a.inline, "\n")))

	if a.file != "" {
		data, err := os.ReadFile(a.file)
		if err != nil {
			return fmt.Errorf("auth.ssh.authorized_keys_file %q: %w", a.file, err)
		}
		addAuthorizedKeys(set, data)
	}

	for _, user := range a.users {
		a.addGitHubUser(set, user)
	}

	a.mu.Lock()
	old := a.keys
	a.keys = set
	a.mu.Unlock()

	// Authoritative diff: a key in the old set but absent from the new one was
	// genuinely removed. A failed GitHub fetch falls back to last-known-good (so
	// the set never shrinks on a transient outage), which makes this diff safe —
	// a transient failure produces no removals. The initial build has an empty
	// old set, so nothing fires at startup.
	if a.onRemove != nil {
		var removed []string
		for marshaled, fingerprint := range old {
			if _, present := set[marshaled]; !present {
				removed = append(removed, fingerprint)
			}
		}
		if len(removed) > 0 {
			a.onRemove(removed)
		}
	}
	return nil
}

// addGitHubUser fetches a user's keys (failing closed to cache) and adds them.
func (a *KeyAllowlist) addGitHubUser(set map[string]string, user string) {
	if !config.ValidGitHubUsername(user) {
		log.Printf("auth.ssh: skipping invalid github username %q", user)
		return
	}
	data, err := a.fetchGitHub(user)
	if err != nil {
		// Fail closed to last-known-good. Prefer the in-memory snapshot (the
		// freshest keys this process actually resolved) over the disk cache: a
		// previous writeCache failure can leave the disk staler than memory, and
		// with token revocation wired in, falling back to a stale disk would
		// shrink the set and revoke a still-valid key. Disk is only the
		// cold-start fallback, before this process has resolved the user.
		switch {
		case a.lastGitHub[user] != nil:
			log.Printf("auth.ssh: github fetch for %q failed (%v); using in-memory last-known-good keys", user, err)
			data = a.lastGitHub[user]
		default:
			cached, cerr := os.ReadFile(a.cachePath(user))
			if cerr != nil {
				log.Printf("auth.ssh: github fetch for %q failed (%v) and no cache; keys unavailable", user, err)
				return
			}
			log.Printf("auth.ssh: github fetch for %q failed (%v); using disk-cached keys", user, err)
			data = cached
		}
	} else if werr := a.writeCache(user, data); werr != nil {
		log.Printf("auth.ssh: failed to cache github keys for %q: %v", user, werr)
	}
	a.lastGitHub[user] = data
	addAuthorizedKeys(set, data)
}

func (a *KeyAllowlist) fetchGitHub(user string) ([]byte, error) {
	url := fmt.Sprintf("%s/%s.keys", githubKeysBaseURL, user)
	resp, err := a.client.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("github returned %d", resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, githubKeysMaxBytes))
}

func (a *KeyAllowlist) cachePath(user string) string {
	return filepath.Join(a.cacheDir, user+".keys")
}

func (a *KeyAllowlist) writeCache(user string, data []byte) error {
	if a.cacheDir == "" {
		return nil
	}
	if err := os.MkdirAll(a.cacheDir, 0o700); err != nil {
		return err
	}
	return os.WriteFile(a.cachePath(user), data, 0o600)
}

// addAuthorizedKeys parses authorized_keys-format data and adds each key's
// marshaled form to set. Best-effort: stops at the first unparseable trailing
// content (the enforce+empty startup check catches an all-garbage source).
func addAuthorizedKeys(set map[string]string, data []byte) {
	rest := data
	for len(rest) > 0 {
		key, _, _, next, err := gossh.ParseAuthorizedKey(rest)
		if err != nil {
			return
		}
		set[string(key.Marshal())] = gossh.FingerprintSHA256(key)
		rest = next
	}
}
