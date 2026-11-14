package users

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	gossh "golang.org/x/crypto/ssh"

	"github.com/charliek/shed/internal/config"
)

const (
	defaultGitHubRefresh = time.Hour
	githubFetchTimeout   = 15 * time.Second

	// unknownKeyRefetchEvery is the server-wide limit on fetches an unknown
	// key can start: one a minute, whoever offers it.
	unknownKeyRefetchEvery = time.Minute
	// unknownKeyWait is how long the login that started such a fetch waits
	// for it before it is answered with what is known.
	unknownKeyWait = 5 * time.Second
)

// githubKeysBaseURL is the GitHub base for `<user>.keys` (a change detector
// only; see github.go); overridable in tests.
var githubKeysBaseURL = "https://github.com"

// ConfigOptions is what NewConfigLookup reads.
type ConfigOptions struct {
	// SSH is the effective auth.ssh block (config.ServerConfig.EffectiveSSHAuth);
	// nil means off.
	SSH *config.SSHAuthConfig
	// GitHubCacheDir is where each GitHub login's .keys are cached
	// (<login>.keys, the file earlier releases read and write, so a rollback
	// finds it); "" keeps no disk cache.
	GitHubCacheDir string
	// PinsPath is the pin store (<state_dir>/github_pins.json): each GitHub
	// login's account id, pinned at its first good fetch. "" keeps pins in
	// memory only, for tests.
	PinsPath string
	// GitHubIDs pins logins (any case) to account ids from config. A
	// configured id wins over the stored pin.
	GitHubIDs map[string]int64
	// Context bounds every GitHub fetch the lookup makes, the first one
	// included: shed-server passes the context it cancels at shutdown, so a
	// fetch in flight then ends at once. nil is context.Background().
	Context context.Context
}

// githubLogin is one configured GitHub login and whose keys are its.
type githubLogin struct {
	login string // as configured
	key   string // pinKey(login)
	user  string
	// configID is the account id config pins the login to; 0 for none.
	configID int64
}

// ConfigLookup is the Lookup answered from the server's config file: the keys
// in auth.ssh (inline keys, an authorized_keys file and GitHub users), every
// one of which belongs to the implicit user.
//
// A GitHub login is pinned to its numeric account id (see github.go): its keys
// are accepted only from a fetch that shows the login still names that
// account, and the pin is written durably before any key from the fetch is
// accepted. When GitHub cannot be reached the last known keys stay and
// nothing new is accepted, so an outage revokes nobody. When the login names
// another account, its keys are dropped and are never read back from memory
// or the disk cache. Each key keeps where it came from.
type ConfigLookup struct {
	mode         string
	maxAuthTries int

	inline   []string
	file     string
	logins   []githubLogin
	cacheDir string
	pinsPath string
	refresh  time.Duration
	gh       *githubClient
	clock    clock
	// ctx bounds every fetch (ConfigOptions.Context).
	ctx context.Context
	// writePins writes the pin store; tests make it fail.
	writePins func(path string, data []byte) error

	// rebuildMu serializes rebuild and guards the per-login state below.
	rebuildMu sync.Mutex
	// lastGitHub holds each login's last accepted key list, by pin key: the
	// first fallback when a fetch fails, so a refresh never silently drops
	// keys that were valid moments ago. Deleted on a mismatch; an empty,
	// non-nil value means "no keys", and stops the disk cache being read.
	lastGitHub map[string][]byte
	// pins is the pin store as last written (or loaded), by pin key.
	pins map[string]pinEntry
	// pendingPins holds, by pin key, a revocation (a mismatch's cleared
	// digest and rejected fingerprints) that the pin store could not record.
	// It is written again by every later rebuild until a write succeeds.
	pendingPins map[string]pinEntry
	// mismatched marks logins whose last answer was a mismatch: nothing of
	// theirs is read from memory or disk until a fetch confirms them again
	// and that confirmation is stored.
	mismatched map[string]bool
	// observed holds, by pin key, the digest of the .keys body the change
	// detector last saw for a login whose fetch then reached a conclusion (or
	// which matched its accepted keys). A light fetch asks the API about a
	// login only when its .keys differs from this, so a body that has not
	// changed never costs an API request, whatever the API said about it.
	observed map[string]string

	// unknownBudget is the hourly allowance of API requests that fetches
	// started by unknown keys may make (unknownKeyAPIBudget).
	unknownBudget *apiBudget

	mu sync.RWMutex
	// keys is the live key set: fingerprint (gossh.FingerprintSHA256) to the
	// owning user's name and the key's provenance. Swapped whole by rebuild,
	// so a reader never sees a half-built set. Fingerprint-keyed so the mtls
	// HTTP path, which holds a certificate CN and not a key, answers in O(1).
	keys map[string]keyEntry
	// rejected holds the fingerprints dropped for a pin mismatch (to their
	// login's pin key), which may never start an unknown-key fetch. Loaded
	// from the pin store at start-up, so it survives a restart.
	rejected map[string]string

	// flightMu guards running and queued: the fetch in progress and the one
	// that starts after it (see request).
	flightMu sync.Mutex
	running  *flight
	queued   *flight

	// limitMu guards lastUnknownFetch, when an unknown key last started a
	// fetch.
	limitMu          sync.Mutex
	lastUnknownFetch time.Time
}

// keyEntry is one accepted key: who owns it and where it came from.
type keyEntry struct {
	user string
	key  Key
}

var (
	_ Lookup              = (*ConfigLookup)(nil)
	_ Refresher           = (*ConfigLookup)(nil)
	_ UnknownKeyRefetcher = (*ConfigLookup)(nil)
)

// NewConfigLookup builds the lookup from config and does the initial GitHub
// fetch (falling back to cache).
//
// Returns an error when mode==enforce but no keys resolved — failing closed
// rather than starting a server that would lock the operator out (and must not
// silently fall back to accept-all) — and when GitHub logins are configured
// and the pin store cannot be read: treating it as empty would pin every login
// again to whatever account it names today, so the server does not start
// until an operator moves the file aside (which re-pins every login at its
// next good fetch).
func NewConfigLookup(opts ConfigOptions) (*ConfigLookup, error) {
	return newConfigLookup(opts, lookupDeps{})
}

// lookupDeps are what tests replace: the clock and the pin store writer.
type lookupDeps struct {
	clock     clock
	writePins func(path string, data []byte) error
}

func newConfigLookup(opts ConfigOptions, deps lookupDeps) (*ConfigLookup, error) {
	clk := deps.clock
	if clk == nil {
		clk = realClock{}
	}
	writePins := deps.writePins
	if writePins == nil {
		writePins = writeFileDurable
	}
	ctx := opts.Context
	if ctx == nil {
		ctx = context.Background()
	}
	l := &ConfigLookup{
		mode:        config.SSHAuthOff,
		keys:        map[string]keyEntry{},
		rejected:    map[string]string{},
		lastGitHub:  map[string][]byte{},
		pins:        map[string]pinEntry{},
		pendingPins: map[string]pinEntry{},
		mismatched:  map[string]bool{},
		observed:    map[string]string{},
		clock:       clk,
		ctx:         ctx,
		writePins:   writePins,
	}
	cfg := opts.SSH
	if cfg == nil || cfg.Mode == "" || cfg.Mode == config.SSHAuthOff {
		return l, nil // off: accept-all, nothing to resolve
	}

	l.mode = cfg.Mode
	l.maxAuthTries = cfg.MaxAuthTries
	l.inline = cfg.AuthorizedKeys
	l.file = config.ExpandPath(cfg.AuthorizedKeysFile)
	ids := map[string]int64{}
	for login, id := range opts.GitHubIDs {
		ids[pinKey(login)] = id
	}
	// GitHub logins are case-insensitive, and two spellings of one login
	// share one pin: they are one login, followed once. (Config load refuses
	// such a list; this holds for any caller.)
	seen := map[string]bool{}
	for _, login := range cfg.GitHubUsers {
		if seen[pinKey(login)] {
			log.Printf("auth.ssh: github login %q is listed more than once (logins are case-insensitive); following it once", login)
			continue
		}
		seen[pinKey(login)] = true
		l.logins = append(l.logins, githubLogin{
			login: login, key: pinKey(login), user: ImplicitName, configID: ids[pinKey(login)],
		})
	}
	l.cacheDir = opts.GitHubCacheDir
	l.pinsPath = opts.PinsPath
	l.gh = &githubClient{
		http:    newGitHubHTTPClient(),
		apiBase: githubAPIBaseURL,
		keyBase: githubKeysBaseURL,
		clock:   clk,
	}
	l.unknownBudget = newAPIBudget(clk, unknownKeyAPIBudget, unknownKeyAPIWindow)
	l.refresh = time.Duration(cfg.GitHubRefresh)
	if l.refresh <= 0 {
		l.refresh = defaultGitHubRefresh
	}

	if len(l.logins) > 0 {
		pins, err := loadPins(l.pinsPath)
		if err != nil {
			return nil, fmt.Errorf("github pin store %s is unreadable (%v); refusing to start rather than pin every GitHub login again. "+
				"Restore it, or move it aside to pin each login to the account it names at its next fetch", l.pinsPath, err)
		}
		l.pins = pins
		for _, g := range l.logins {
			for _, fp := range pins[g.key].Rejected {
				l.rejected[fp] = g.key
			}
		}
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
				f := l.request(fetchFull, false)
				select {
				case <-f.done:
					if f.err != nil {
						log.Printf("auth.ssh: key refresh failed: %v", f.err)
					}
				case <-ctx.Done():
					return
				}
			}
		}
	}()
}

// Refresh implements Refresher.
func (l *ConfigLookup) Refresh(ctx context.Context) (RefreshReport, error) {
	if l.mode == config.SSHAuthOff || len(l.logins) == 0 {
		return RefreshReport{Logins: []LoginRefresh{}}, nil
	}
	f := l.request(fetchFull, true)
	select {
	case <-f.done:
		return f.report, f.err
	case <-ctx.Done():
		return RefreshReport{}, ctx.Err()
	}
}

// RefetchForUnknownKey implements UnknownKeyRefetcher.
//
// Only in enforce mode: in warn mode an unknown key is let in anyway, and in
// off mode no key is looked up. A key dropped for a mismatch is refused at
// once (a map lookup), and so is every key while the hour's API budget for
// unknown keys is spent. Then the server-wide limit is taken, atomically,
// before anything else is done: the caller that takes it starts a light
// fetch and waits up to unknownKeyWait for it, and every other caller in
// that minute returns false at once, having done nothing and waited for
// nothing.
func (l *ConfigLookup) RefetchForUnknownKey(ctx context.Context, fp string) bool {
	if fp == "" || l.mode != config.SSHAuthEnforce || len(l.logins) == 0 {
		return false
	}
	// Not logged: whoever holds a dropped key can offer it at connection
	// rate, and the drop itself was logged.
	l.mu.RLock()
	_, rejected := l.rejected[fp]
	l.mu.RUnlock()
	if rejected {
		return false
	}
	// Not logged either: running out was logged once.
	if !l.unknownBudget.available() {
		return false
	}
	if !l.takeUnknownFetchSlot() {
		return false
	}
	log.Printf("auth.ssh: unknown key %s: fetching the GitHub logins again (at most once a minute)", fp)
	f := l.request(fetchLight, false)
	select {
	case <-f.done:
	case <-l.clock.After(unknownKeyWait):
	case <-ctx.Done():
	}
	_, k, ok := l.UserForKey(fp)
	return ok && !k.Expired(l.clock.Now())
}

// takeUnknownFetchSlot takes the server-wide unknown-key fetch slot when no
// unknown key has taken it in the last minute, and reports whether it did.
func (l *ConfigLookup) takeUnknownFetchSlot() bool {
	l.limitMu.Lock()
	defer l.limitMu.Unlock()
	now := l.clock.Now()
	if !l.lastUnknownFetch.IsZero() && now.Sub(l.lastUnknownFetch) < unknownKeyRefetchEvery {
		return false
	}
	l.lastUnknownFetch = now
	return true
}

// --- single flight ------------------------------------------------------------

// fetchKind is how hard a rebuild asks GitHub.
type fetchKind int

const (
	// fetchFull checks every login's account id (start-up, the scheduled
	// refresh, `shed users refresh`).
	fetchFull fetchKind = iota
	// fetchLight reads each login's github.com/<login>.keys, a change
	// detector outside the API's rate limit, and does a full fetch only for
	// a login whose .keys changed since the detector last saw it (an unknown
	// key). Its API requests draw on the unknown-key budget. Nothing is
	// accepted from the detector itself.
	fetchLight
)

// flight is one rebuild that callers share.
type flight struct {
	kind   fetchKind // guarded by flightMu while queued
	done   chan struct{}
	report RefreshReport
	err    error
}

// request returns a rebuild of at least kind to wait on. A rebuild that has
// not started yet is joined (and made full when asked). A running one is
// joined when it is enough (full, or a light one for a light ask) and force
// is false; otherwise one is queued to run after it. Whatever the callers,
// at most one rebuild runs and one waits.
func (l *ConfigLookup) request(kind fetchKind, force bool) *flight {
	l.flightMu.Lock()
	defer l.flightMu.Unlock()
	if q := l.queued; q != nil {
		if kind == fetchFull {
			q.kind = fetchFull
		}
		return q
	}
	if r := l.running; r != nil {
		if !force && (r.kind == fetchFull || kind == fetchLight) {
			return r
		}
		l.queued = &flight{kind: kind, done: make(chan struct{})}
		return l.queued
	}
	l.running = &flight{kind: kind, done: make(chan struct{})}
	go l.fly(l.running)
	return l.running
}

// fly runs f, then whatever was queued behind it.
func (l *ConfigLookup) fly(f *flight) {
	for f != nil {
		l.flightMu.Lock()
		kind := f.kind
		l.flightMu.Unlock()
		f.report, f.err = l.rebuildKind(kind)
		l.flightMu.Lock()
		next := l.queued
		l.queued = nil
		l.running = next
		l.flightMu.Unlock()
		close(f.done)
		f = next
	}
}

// --- rebuild ------------------------------------------------------------------

// rebuild re-resolves the full key set from all sources, checking every
// GitHub login's account. Only a keys-file read error is fatal.
func (l *ConfigLookup) rebuild() error {
	_, err := l.rebuildKind(fetchFull)
	return err
}

// loginPlan is what a rebuild does with one login.
type loginPlan struct {
	g   githubLogin
	res fetchResult
	// prev is the login's last known keys before this rebuild: those this
	// process last accepted, else its trusted disk cache (see lastKnown).
	prev     []byte
	prevFrom string // where prev came from, for the log
	// detected is the .keys body a light fetch read (nil otherwise).
	detected []byte
	data     []byte // keys accepted by this rebuild
	status   string
	// gated marks a confirmed fetch whose acceptance waits on the pin
	// store write.
	gated bool
	// revoked marks a mismatch, whose revocation the pin store records;
	// dropped lists the fingerprints it rejects.
	revoked bool
	dropped []string
}

// rebuildKind re-resolves the full key set from all sources. Only a keys-file
// read error is fatal (a config error); every GitHub failure falls back to
// the last known keys.
//
// When one key appears in more than one source, the first source wins, in the
// order inline keys, the keys file, then GitHub logins in config order. Every
// key belongs to the implicit user, so the order decides only the provenance
// that is reported.
func (l *ConfigLookup) rebuildKind(kind fetchKind) (RefreshReport, error) {
	l.rebuildMu.Lock()
	defer l.rebuildMu.Unlock()

	set := make(map[string]keyEntry)
	addAuthorizedKeys(set, []byte(strings.Join(l.inline, "\n")), ImplicitName, SourceInline, "")
	if l.file != "" {
		data, err := os.ReadFile(l.file)
		if err != nil {
			return RefreshReport{}, fmt.Errorf("auth.ssh.authorized_keys_file %q: %w", l.file, err)
		}
		addAuthorizedKeys(set, data, ImplicitName, SourceFile, l.file)
	}

	plans := l.fetchAll(kind)

	// Decide each login, and the pin store that results: the one on disk,
	// with every revocation an earlier write failed to record applied again.
	newPins := make(map[string]pinEntry, len(l.pins))
	for k, v := range l.pins {
		newPins[k] = v
	}
	pinsDirty := len(l.pendingPins) > 0
	for k, v := range l.pendingPins {
		newPins[k] = v
	}
	for i := range plans {
		p := &plans[i]
		if p.status == RefreshInvalidLogin {
			continue
		}
		switch p.res.outcome {
		case fetchUnchanged:
			p.data, p.status = p.prev, RefreshUnchanged
		case fetchUnavailable:
			p.data, p.status = p.prev, RefreshUnreachable
			l.logLastKnown(p)
		case fetchGone:
			p.data, p.status = nil, RefreshGone
		case fetchMismatch:
			p.data, p.status = nil, RefreshMismatch
			p.revoked = true
			p.dropped = l.droppedFingerprints(p)
			e, had := newPins[p.g.key]
			if !had {
				e = pinEntry{ID: l.effectivePin(p.g), PinnedAt: l.clock.Now().UTC()}
			}
			want := e
			want.KeysSHA256 = ""
			want.Rejected = mergeRejected(e.Rejected, p.dropped)
			if !had || !e.equal(want) {
				newPins[p.g.key] = want
				pinsDirty = true
			}
		case fetchConfirmed:
			p.data, p.status = p.res.keys, RefreshOK
			e, had := newPins[p.g.key]
			want := pinEntry{ID: p.res.id, PinnedAt: e.PinnedAt, KeysSHA256: keysDigest(p.res.keys)}
			if !had || e.ID != p.res.id {
				want.PinnedAt = l.clock.Now().UTC()
				p.status = RefreshPinned
			}
			if !had || !e.equal(want) {
				newPins[p.g.key] = want
				pinsDirty = true
				p.gated = true
			}
		}
	}

	// The pin is durable before any key it vouches for is accepted. When it
	// cannot be written nothing new is accepted: a gated login keeps what it
	// had, less any key GitHub stopped listing. A revocation that cannot be
	// written stays in force in this process and is written again by every
	// later rebuild.
	if pinsDirty {
		if err := l.storePins(newPins); err != nil {
			log.Printf("auth.ssh: writing the github pin store %s failed (%v); accepting no new github key", l.pinsPath, err)
			pending := make(map[string]pinEntry, len(l.pendingPins))
			for k, v := range l.pendingPins {
				pending[k] = v
			}
			for i := range plans {
				p := &plans[i]
				if p.gated {
					p.data = keepListed(p.prev, p.res.keys)
					p.status = RefreshPinWriteFailed
					p.res.err = err
				}
				if p.revoked {
					pending[p.g.key] = newPins[p.g.key]
				}
			}
			for k := range pending {
				log.Printf("auth.ssh: SECURITY: the revocation of github login %q's keys is not recorded in %s (%v). "+
					"Its keys stay revoked while this process runs and the write is tried again at every refresh; "+
					"until it succeeds, a restart while GitHub is unreachable could trust its old key cache. Make the state directory writable.",
					k, l.pinsPath, err)
			}
			l.pendingPins = pending
		} else {
			l.pins = newPins
			l.pendingPins = map[string]pinEntry{}
		}
	}

	// Commit the per-login state, then the key set.
	rejected := make(map[string]string)
	l.mu.RLock()
	for fp, login := range l.rejected {
		rejected[fp] = login
	}
	l.mu.RUnlock()
	report := RefreshReport{Logins: make([]LoginRefresh, 0, len(plans))}
	for i := range plans {
		p := &plans[i]
		row := LoginRefresh{User: p.g.user, Login: p.g.login, Status: p.status, PinnedID: l.effectivePin(p.g)}
		if p.res.err != nil {
			row.Error = p.res.err.Error()
		}
		switch p.status {
		case RefreshInvalidLogin:
			row.Error = "not a valid GitHub login"
			report.Logins = append(report.Logins, row)
			continue
		case RefreshOK, RefreshPinned:
			l.lastGitHub[p.g.key] = p.data
			delete(l.mismatched, p.g.key)
			for fp, login := range rejected {
				if login == p.g.key {
					delete(rejected, fp)
				}
			}
			// Written after the pin, so a cache newer than its pin never
			// exists; a write that fails leaves a cache whose digest is not
			// the pin's, which is then not trusted.
			if err := l.writeCache(p.g.login, p.data); err != nil {
				log.Printf("auth.ssh: failed to cache github keys for %q: %v", p.g.login, err)
			}
		case RefreshMismatch:
			row.SeenID = p.res.id
			for _, fp := range p.dropped {
				rejected[fp] = p.g.key
			}
			delete(l.lastGitHub, p.g.key)
			l.mismatched[p.g.key] = true
			l.removeCache(p.g.login)
			seen := "no account"
			if p.res.id != 0 {
				seen = fmt.Sprintf("account %d", p.res.id)
			}
			log.Printf("auth.ssh: github login %q is pinned to account %d but now names %s; its keys are dropped. "+
				"If the account was renamed, configure its new login; to pin %q again, remove its entry from %s",
				p.g.login, row.PinnedID, seen, p.g.login, l.pinsPath)
		case RefreshGone:
			delete(l.lastGitHub, p.g.key)
			l.removeCache(p.g.login)
			log.Printf("auth.ssh: github login %q names no account; it has no keys", p.g.login)
		case RefreshPinWriteFailed:
			// Recorded even when empty, so a later outage falls back to
			// exactly this and never to a key GitHub stopped listing.
			if p.data == nil {
				p.data = []byte{}
			}
			l.lastGitHub[p.g.key] = p.data
			if _, removed := diffFingerprints(p.prev, p.data); len(removed) > 0 {
				// The stored digest may still vouch for the cache, which
				// holds a key GitHub stopped listing: remove it.
				l.removeCache(p.g.login)
			}
		case RefreshUnreachable, RefreshUnchanged:
			if p.data != nil {
				l.lastGitHub[p.g.key] = p.data
			}
		}
		// The detector's observation moves on only when the fetch it led to
		// reached a conclusion (or there was nothing to ask): after an
		// outage, a budget that ran out or a pin write that failed, the same
		// .keys asks again next time.
		switch p.status {
		case RefreshOK, RefreshPinned, RefreshUnchanged, RefreshMismatch, RefreshGone:
			if p.detected != nil {
				l.observed[p.g.key] = keysDigest(p.detected)
			}
		}
		addAuthorizedKeys(set, p.data, p.g.user, SourceGitHub, p.g.login)
		row.Added, row.Removed = diffFingerprints(p.prev, p.data)
		row.Keys = len(fingerprintsOf(p.data))
		for _, fp := range row.Added {
			log.Printf("auth.ssh: github login %q (account %d): accepted new key %s", p.g.login, row.PinnedID, fp)
		}
		for _, fp := range row.Removed {
			log.Printf("auth.ssh: github login %q: key %s is no longer accepted", p.g.login, fp)
		}
		report.Logins = append(report.Logins, row)
	}

	// A failed GitHub fetch falls back to last-known-good, so the set never
	// shrinks on a transient outage; a key leaves only when its source
	// genuinely stopped listing it or its login no longer names its pinned
	// account. An mtls server re-checks every request's certificate against
	// this set, so that removal de-authorizes the key's certificates on
	// their next request.
	l.mu.Lock()
	l.keys = set
	l.rejected = rejected
	l.mu.Unlock()
	return report, nil
}

// fetchAll fetches every valid login concurrently and returns one plan per
// login in config order. Called under rebuildMu. Every request is bound to
// the lookup's context, so a fetch in flight at shutdown ends at once. A
// light fetch's API requests draw on the unknown-key budget.
func (l *ConfigLookup) fetchAll(kind fetchKind) []loginPlan {
	plans := make([]loginPlan, len(l.logins))
	ctx := l.ctx
	if kind == fetchLight {
		ctx = withBudget(ctx, l.unknownBudget)
	}
	var wg sync.WaitGroup
	for i, g := range l.logins {
		plans[i] = loginPlan{g: g}
		if !config.ValidGitHubUsername(g.login) {
			log.Printf("auth.ssh: skipping invalid github username %q", g.login)
			plans[i].status = RefreshInvalidLogin
			continue
		}
		plans[i].prev, plans[i].prevFrom = l.lastKnown(g)
		pin := l.effectivePin(g)
		mismatched := l.distrusted(g.key)
		observed, hasObserved := l.observed[g.key]
		wg.Add(1)
		go func(p *loginPlan) {
			defer wg.Done()
			if kind == fetchLight {
				data, err := l.gh.dotKeys(ctx, p.g.login)
				if err != nil {
					p.res = fetchResult{outcome: fetchUnavailable, err: err}
					return
				}
				p.detected = data
				// Compared with what the detector last saw, not with the
				// accepted keys: a login whose .keys and accepted keys
				// disagree for good (a mismatch, or the detector and the
				// API differing) asks the API once per change, not once
				// per unknown key. Before a first observation, a .keys that
				// lists exactly the accepted keys is unchanged.
				if hasObserved && observed == keysDigest(data) ||
					!hasObserved && !mismatched && sameFingerprints(data, p.prev) {
					p.res = fetchResult{outcome: fetchUnchanged}
					return
				}
			}
			p.res = l.gh.fetchConfirmed(ctx, p.g.login, pin)
		}(&plans[i])
	}
	wg.Wait()
	return plans
}

// effectivePin is the account id login must name: config's, else the stored
// pin, else 0 (none yet).
func (l *ConfigLookup) effectivePin(g githubLogin) int64 {
	if g.configID != 0 {
		return g.configID
	}
	return l.pins[g.key].ID
}

// distrusted reports whether a login's keys may not be read back from
// anywhere: its last answer was a mismatch, or a revocation of it is not yet
// recorded in the pin store.
func (l *ConfigLookup) distrusted(key string) bool {
	_, pending := l.pendingPins[key]
	return l.mismatched[key] || pending
}

// Where lastKnown found a login's keys.
const (
	fromMemory = "memory"
	fromCache  = "the disk cache"
	fromLegacy = "the disk cache (the login is not pinned yet)"
)

// lastKnown is a login's last known keys, which a fetch that cannot vouch for
// new ones falls back to and a pin write that fails keeps: the bytes this
// process last accepted, else the disk cache, read only when it is the cache
// last confirmed for the pin (its hash is the pin's) or the login has never
// been pinned (a cache an earlier release wrote). Nothing is read from disk
// for a distrusted login.
func (l *ConfigLookup) lastKnown(g githubLogin) ([]byte, string) {
	if data := l.lastGitHub[g.key]; data != nil {
		return data, fromMemory
	}
	if l.distrusted(g.key) || l.cacheDir == "" {
		return nil, ""
	}
	cached, err := os.ReadFile(l.cachePath(g.login))
	if err != nil {
		return nil, ""
	}
	pin := l.effectivePin(g)
	if pin == 0 {
		return cached, fromLegacy
	}
	if e, ok := l.pins[g.key]; ok && e.ID == pin && e.KeysSHA256 != "" && e.KeysSHA256 == keysDigest(cached) {
		return cached, fromCache
	}
	log.Printf("auth.ssh: the disk cache of github login %q is not the one confirmed for account %d, so it is not used", g.login, pin)
	return nil, ""
}

// logLastKnown logs what a login whose fetch failed falls back to.
func (l *ConfigLookup) logLastKnown(p *loginPlan) {
	if p.prevFrom == "" {
		log.Printf("auth.ssh: github fetch for %q failed (%v) and there are no last known keys; keys unavailable", p.g.login, p.res.err)
		return
	}
	log.Printf("auth.ssh: github fetch for %q failed (%v); using the last known keys from %s", p.g.login, p.res.err, p.prevFrom)
}

// droppedFingerprints is every fingerprint a mismatch drops for a login: its
// last known keys, those its .keys lists now (the other account's), and
// those in its disk cache whatever it holds, so a key from any of them never
// starts an unknown-key fetch.
func (l *ConfigLookup) droppedFingerprints(p *loginPlan) []string {
	var all []string
	all = append(all, fingerprintsOf(p.prev)...)
	all = append(all, fingerprintsOf(p.detected)...)
	if l.cacheDir != "" {
		if cached, err := os.ReadFile(l.cachePath(p.g.login)); err == nil {
			all = append(all, fingerprintsOf(cached)...)
		}
	}
	sort.Strings(all)
	return slices.Compact(all)
}

// storePins writes pins durably to the pin store (a no-op with no path).
func (l *ConfigLookup) storePins(pins map[string]pinEntry) error {
	if l.pinsPath == "" {
		return nil
	}
	data, err := encodePins(pins)
	if err != nil {
		return err
	}
	return l.writePins(l.pinsPath, data)
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

// removeCache deletes a login's disk cache, so keys GitHub no longer vouches
// for are not read back from it (by this release or an earlier one).
func (l *ConfigLookup) removeCache(login string) {
	if l.cacheDir == "" {
		return
	}
	if err := os.Remove(l.cachePath(login)); err != nil && !errors.Is(err, os.ErrNotExist) {
		log.Printf("auth.ssh: removing the github key cache for %q failed: %v", login, err)
	}
}

// keepListed returns the keys of prev that listed also has: what a login
// keeps when nothing new may be accepted.
func keepListed(prev, listed []byte) []byte {
	keep := map[string]bool{}
	for _, fp := range fingerprintsOf(listed) {
		keep[fp] = true
	}
	var out []byte
	rest := prev
	for len(rest) > 0 {
		key, comment, _, next, err := gossh.ParseAuthorizedKey(rest)
		if err != nil {
			break
		}
		if keep[gossh.FingerprintSHA256(key)] {
			line := bytes.TrimSuffix(gossh.MarshalAuthorizedKey(key), []byte("\n"))
			if comment != "" {
				line = append(append(line, ' '), comment...)
			}
			out = append(append(out, line...), '\n')
		}
		rest = next
	}
	return out
}

// diffFingerprints lists the fingerprints in after and not before (added) and
// in before and not after (removed).
func diffFingerprints(before, after []byte) (added, removed []string) {
	was := map[string]bool{}
	for _, fp := range fingerprintsOf(before) {
		was[fp] = true
	}
	for _, fp := range fingerprintsOf(after) {
		if !was[fp] {
			added = append(added, fp)
		}
		delete(was, fp)
	}
	for _, fp := range fingerprintsOf(before) {
		if was[fp] {
			removed = append(removed, fp)
		}
	}
	return added, removed
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
