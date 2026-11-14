package users

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

// ConfigOptions is what NewConfigLookup reads.
type ConfigOptions struct {
	// SSH is the effective auth.ssh block (config.ServerConfig.EffectiveSSHAuth);
	// nil means off.
	SSH *config.SSHAuthConfig
	// GitHubCacheDir is where each GitHub login's .keys are cached; "" keeps
	// no disk cache.
	GitHubCacheDir string
}

// ConfigLookup is the Lookup answered from the server's config file: the keys
// in auth.ssh (inline keys, an authorized_keys file and GitHub users), every
// one of which belongs to the implicit user.
//
// GitHub keys are cached to disk and fail closed to the last-known-good keys
// when a refetch fails, so a GitHub outage never silently empties the key set.
// Each key keeps where it came from.
type ConfigLookup struct {
	mode         string
	maxAuthTries int

	inline   []string
	file     string
	logins   []string
	cacheDir string
	refresh  time.Duration
	client   *http.Client

	// rebuildMu serializes rebuild, which reads the sources and owns
	// lastGitHub. The periodic refresh is the only caller after start-up
	// today; the lock keeps that true for any later caller too.
	rebuildMu sync.Mutex
	// lastGitHub holds each GitHub login's last successfully-resolved .keys
	// bytes — the ultimate last-known-good fallback when both the live fetch
	// and the disk cache fail, so a refresh can never silently drop keys that
	// were valid moments ago. Guarded by rebuildMu.
	lastGitHub map[string][]byte

	mu sync.RWMutex
	// keys is the live key set: fingerprint (gossh.FingerprintSHA256) to the
	// owning user's name and the key's provenance. Swapped whole by rebuild,
	// so a reader never sees a half-built set. Fingerprint-keyed so the mtls
	// HTTP path, which holds a certificate CN and not a key, answers in O(1).
	keys map[string]keyEntry
}

// keyEntry is one accepted key: who owns it and where it came from.
type keyEntry struct {
	user string
	key  Key
}

var _ Lookup = (*ConfigLookup)(nil)

// NewConfigLookup builds the lookup from config and does the initial GitHub
// fetch (falling back to cache).
//
// Returns an error when mode==enforce but no keys resolved — failing closed
// rather than starting a server that would lock the operator out (and must not
// silently fall back to accept-all).
func NewConfigLookup(opts ConfigOptions) (*ConfigLookup, error) {
	l := &ConfigLookup{
		mode:       config.SSHAuthOff,
		keys:       map[string]keyEntry{},
		lastGitHub: map[string][]byte{},
	}
	cfg := opts.SSH
	if cfg == nil || cfg.Mode == "" || cfg.Mode == config.SSHAuthOff {
		return l, nil // off: accept-all, nothing to resolve
	}

	l.mode = cfg.Mode
	l.maxAuthTries = cfg.MaxAuthTries
	l.inline = cfg.AuthorizedKeys
	l.file = config.ExpandPath(cfg.AuthorizedKeysFile)
	l.logins = cfg.GitHubUsers
	l.cacheDir = opts.GitHubCacheDir
	l.client = &http.Client{Timeout: githubFetchTimeout}
	l.refresh = time.Duration(cfg.GitHubRefresh)
	if l.refresh <= 0 {
		l.refresh = defaultGitHubRefresh
	}

	if err := l.rebuild(); err != nil {
		return nil, err
	}
	if l.mode == config.SSHAuthEnforce && l.size() == 0 {
		return nil, fmt.Errorf(
			"auth.ssh.mode=enforce but no authorized keys resolved " +
				"(inline/file empty and GitHub fetch failed with no cache); " +
				"use mode=warn for first boot or provide authorized_keys")
	}
	return l, nil
}

// Mode returns the SSH key-auth mode (off/warn/enforce).
func (l *ConfigLookup) Mode() string { return l.mode }

// MaxAuthTries returns the configured per-connection public-key attempt cap,
// or 0 when unset (the SSH server then applies its own default).
func (l *ConfigLookup) MaxAuthTries() int { return l.maxAuthTries }

// UserForKey reports which user owns the key with this fingerprint. fp must be
// in canonical gossh.FingerprintSHA256 form ("SHA256:<43 base64 chars>") — the
// same string the mtls bootstrap burns into an issued client certificate's
// Subject CN; any other spelling is not found.
//
// It reads the LIVE in-memory set, so a key removed by a background refresh
// stops resolving as soon as that rebuild lands: the HTTP mtls middleware asks
// per request, which is how certificate revocation takes effect without a CRL.
// A failed GitHub refetch never shrinks the set, so a transient outage cannot
// revoke a live certificate.
//
// It answers only "is this key accepted, and whose is it", never "is key auth
// being enforced": in off mode the set is empty and this finds nothing, so a
// caller that must fail closed gets that for free, and a caller in a
// non-enforcing posture must not use it as an authorization oracle.
func (l *ConfigLookup) UserForKey(fp string) (User, Key, bool) {
	if fp == "" {
		return User{}, Key{}, false
	}
	l.mu.RLock()
	e, ok := l.keys[fp]
	l.mu.RUnlock()
	if !ok {
		return User{}, Key{}, false
	}
	u, ok := l.User(e.user)
	if !ok {
		return User{}, Key{}, false
	}
	return u, e.key, true
}

// User returns the user with this name. With no user list there is exactly
// one, the implicit user.
func (l *ConfigLookup) User(name string) (User, bool) {
	if name == ImplicitName {
		return Implicit(), true
	}
	return User{}, false
}

// Users lists every user: with no user list, the implicit user alone.
func (l *ConfigLookup) Users() []User {
	return []User{Implicit()}
}

func (l *ConfigLookup) size() int {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return len(l.keys)
}

// StartRefresh launches a background goroutine that periodically re-resolves
// the GitHub-seeded keys, until ctx is cancelled. No-op when there are no
// GitHub users to refresh.
func (l *ConfigLookup) StartRefresh(ctx context.Context) {
	if l.mode == config.SSHAuthOff || len(l.logins) == 0 {
		return
	}
	go func() {
		t := time.NewTicker(l.refresh)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if err := l.rebuild(); err != nil {
					log.Printf("auth.ssh: key refresh failed: %v", err)
				}
			}
		}
	}()
}

// rebuild re-resolves the full key set from all sources. Only a keys-file read
// error is fatal (a config error); GitHub fetch failures fall back to cache.
//
// When one key appears in more than one source, the first source wins, in the
// order inline keys, the keys file, then GitHub logins in config order. Every
// key belongs to the implicit user, so the order decides only the provenance
// that is reported.
func (l *ConfigLookup) rebuild() error {
	l.rebuildMu.Lock()
	defer l.rebuildMu.Unlock()

	set := make(map[string]keyEntry)
	addAuthorizedKeys(set, []byte(strings.Join(l.inline, "\n")), ImplicitName, SourceInline, "")

	if l.file != "" {
		data, err := os.ReadFile(l.file)
		if err != nil {
			return fmt.Errorf("auth.ssh.authorized_keys_file %q: %w", l.file, err)
		}
		addAuthorizedKeys(set, data, ImplicitName, SourceFile, l.file)
	}

	for _, login := range l.logins {
		l.addGitHubUser(set, login, ImplicitName)
	}

	// A failed GitHub fetch falls back to last-known-good, so the set never
	// shrinks on a transient outage; a key leaves only when its source
	// genuinely stopped listing it. An mtls server re-checks every request's
	// certificate against this set, so that removal de-authorizes the key's
	// certificates on their next request.
	l.mu.Lock()
	l.keys = set
	l.mu.Unlock()
	return nil
}

// addGitHubUser fetches a GitHub login's keys (failing closed to cache) and
// adds them to set as user's. Called only by rebuild, under rebuildMu.
func (l *ConfigLookup) addGitHubUser(set map[string]keyEntry, login, user string) {
	if !config.ValidGitHubUsername(login) {
		log.Printf("auth.ssh: skipping invalid github username %q", login)
		return
	}
	data, err := l.fetchGitHub(login)
	if err != nil {
		// Fail closed to last-known-good. Prefer the in-memory snapshot (the
		// freshest keys this process actually resolved) over the disk cache: a
		// previous writeCache failure can leave the disk staler than memory, and
		// falling back to a stale disk would shrink the set and de-authorize a
		// still-valid key (and, in mtls mode, its certificates). Disk is only the
		// cold-start fallback, before this process has resolved the login.
		switch {
		case l.lastGitHub[login] != nil:
			log.Printf("auth.ssh: github fetch for %q failed (%v); using in-memory last-known-good keys", login, err)
			data = l.lastGitHub[login]
		default:
			cached, cerr := os.ReadFile(l.cachePath(login))
			if cerr != nil {
				log.Printf("auth.ssh: github fetch for %q failed (%v) and no cache; keys unavailable", login, err)
				return
			}
			log.Printf("auth.ssh: github fetch for %q failed (%v); using disk-cached keys", login, err)
			data = cached
		}
	} else if werr := l.writeCache(login, data); werr != nil {
		log.Printf("auth.ssh: failed to cache github keys for %q: %v", login, werr)
	}
	l.lastGitHub[login] = data
	addAuthorizedKeys(set, data, user, SourceGitHub, login)
}

func (l *ConfigLookup) fetchGitHub(login string) ([]byte, error) {
	url := fmt.Sprintf("%s/%s.keys", githubKeysBaseURL, login)
	resp, err := l.client.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("github returned %d", resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, githubKeysMaxBytes))
}

func (l *ConfigLookup) cachePath(login string) string {
	return filepath.Join(l.cacheDir, login+".keys")
}

func (l *ConfigLookup) writeCache(login string, data []byte) error {
	if l.cacheDir == "" {
		return nil
	}
	if err := os.MkdirAll(l.cacheDir, 0o700); err != nil {
		return err
	}
	return os.WriteFile(l.cachePath(login), data, 0o600)
}

// addAuthorizedKeys parses authorized_keys-format data and adds each key to
// set as user's, recording source and origin. A key already in set keeps its
// first owner and provenance. Best-effort: stops at the first unparseable
// trailing content (the enforce+empty startup check catches an all-garbage
// source).
func addAuthorizedKeys(set map[string]keyEntry, data []byte, user string, source Source, origin string) {
	rest := data
	for len(rest) > 0 {
		key, comment, _, next, err := gossh.ParseAuthorizedKey(rest)
		if err != nil {
			return
		}
		fp := gossh.FingerprintSHA256(key)
		if _, seen := set[fp]; !seen {
			set[fp] = keyEntry{user: user, key: Key{
				Fingerprint: fp,
				Source:      source,
				Origin:      origin,
				Label:       comment,
			}}
		}
		rest = next
	}
}
