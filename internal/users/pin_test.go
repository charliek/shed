package users

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	gossh "golang.org/x/crypto/ssh"

	"github.com/charliek/shed/internal/config"
)

// pinFixture is a lookup over one GitHub login, octocat, with its pin store
// and key cache in a temp state dir. It runs in enforce mode with one inline
// key (the operator's), so a start with no GitHub key still succeeds and
// unknown-key fetches run.
type pinFixture struct {
	gh       *fakeGitHub
	clock    *fakeClock
	state    string
	cacheDir string
	pinsPath string
	mode     string
	logins   []string
	ids      map[string]int64
	ctx      context.Context
	operator string // the inline key
	// failWrites makes the pin store writer fail while set.
	failWrites atomic.Bool
	writes     atomic.Int64
}

func newPinFixture(t *testing.T) *pinFixture {
	t.Helper()
	state := t.TempDir()
	_, operator := genKey(t)
	return &pinFixture{
		gh:       newFakeGitHub(t),
		clock:    newFakeClock(),
		state:    state,
		cacheDir: filepath.Join(state, "github_keys"),
		pinsPath: filepath.Join(state, PinsFileName),
		mode:     config.SSHAuthEnforce,
		logins:   []string{"octocat"},
		operator: operator,
	}
}

// open builds a lookup (a server start) over the fixture's state.
func (f *pinFixture) open(t *testing.T) *ConfigLookup {
	t.Helper()
	l, err := f.tryOpen()
	if err != nil {
		t.Fatal(err)
	}
	return l
}

func (f *pinFixture) tryOpen() (*ConfigLookup, error) {
	return newConfigLookup(ConfigOptions{
		SSH: &config.SSHAuthConfig{
			Mode: f.mode, GitHubUsers: f.logins, AuthorizedKeys: []string{f.operator},
		},
		GitHubCacheDir: f.cacheDir,
		PinsPath:       f.pinsPath,
		GitHubIDs:      f.ids,
		Context:        f.ctx,
	}, lookupDeps{clock: f.clock, writePins: func(path string, data []byte) error {
		f.writes.Add(1)
		if f.failWrites.Load() {
			return errors.New("disk full")
		}
		return writeFileDurable(path, data)
	}})
}

// storedPins reads the pin store from disk.
func (f *pinFixture) storedPins(t *testing.T) map[string]pinEntry {
	t.Helper()
	pins, err := loadPins(f.pinsPath)
	if err != nil {
		t.Fatal(err)
	}
	return pins
}

// cacheFile is octocat's key cache.
func (f *pinFixture) cacheFile() string { return filepath.Join(f.cacheDir, "octocat.keys") }

// outage makes every GitHub request fail.
func (f *pinFixture) outage() {
	f.gh.setAPIStatus(http.StatusServiceUnavailable, nil)
	f.gh.setKeysStatus(http.StatusServiceUnavailable)
}

func refreshRow(t *testing.T, l *ConfigLookup) LoginRefresh {
	t.Helper()
	r, err := l.Refresh(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Logins) != 1 {
		t.Fatalf("report rows = %d, want 1", len(r.Logins))
	}
	return r.Logins[0]
}

func fp(k gossh.PublicKey) string { return gossh.FingerprintSHA256(k) }

// waitIdle waits until no fetch is running or queued.
func waitIdle(t *testing.T, l *ConfigLookup) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		l.flightMu.Lock()
		idle := l.running == nil && l.queued == nil
		l.flightMu.Unlock()
		if idle {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("a fetch never finished")
		}
		time.Sleep(time.Millisecond)
	}
}

// TestPinAtFirstFetch: the first good fetch pins the login to the account it
// names, in a 0600 pin store on disk, and records the confirmed keys' digest;
// the key is accepted. A later fetch that names the same account keeps it,
// and a key new on the account is accepted and reported.
func TestPinAtFirstFetch(t *testing.T) {
	k1, line1 := genKey(t)
	k2, line2 := genKey(t)
	f := newPinFixture(t)
	f.gh.set("octocat", 583231, line1)

	l := f.open(t)
	if !has(l, k1) {
		t.Fatal("key of the first fetch not accepted")
	}
	pins := f.storedPins(t)
	if e := pins["octocat"]; e.ID != 583231 || e.KeysSHA256 != keysDigest([]byte(line1)) || e.PinnedAt.IsZero() {
		t.Fatalf("stored pin = %+v, want account 583231 with the keys' digest", e)
	}
	st, err := os.Stat(f.pinsPath)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Errorf("pin store mode = %o, want 600", st.Mode().Perm())
	}
	var raw map[string]any
	data, _ := os.ReadFile(f.pinsPath)
	if err := json.Unmarshal(data, &raw); err != nil || raw["version"] != float64(1) {
		t.Errorf("pin store = %s, want version 1", data)
	}

	f.gh.set("octocat", 583231, line1+line2)
	row := refreshRow(t, l)
	if row.Status != RefreshOK || row.PinnedID != 583231 || row.Keys != 2 {
		t.Errorf("row = %+v, want ok, account 583231, 2 keys", row)
	}
	if len(row.Added) != 1 || row.Added[0] != fp(k2) || len(row.Removed) != 0 {
		t.Errorf("added %v removed %v, want just k2 added", row.Added, row.Removed)
	}
	if !has(l, k2) {
		t.Error("new key on the pinned account not accepted")
	}
	if got := f.storedPins(t)["octocat"]; !got.PinnedAt.Equal(pins["octocat"].PinnedAt) {
		t.Error("pinned_at changed on a fetch of the same account")
	}
}

// TestPinMismatchDropsKeys: a login that names another account than its pin
// has its keys dropped at once, its cache removed and the mismatch reported.
func TestPinMismatchDropsKeys(t *testing.T) {
	k1, line1 := genKey(t)
	squatter, squatterLine := genKey(t)
	f := newPinFixture(t)
	f.gh.set("octocat", 583231, line1)
	l := f.open(t)

	f.gh.set("octocat", 999, squatterLine)
	row := refreshRow(t, l)
	if row.Status != RefreshMismatch || row.PinnedID != 583231 || row.SeenID != 999 || row.Keys != 0 {
		t.Errorf("row = %+v, want mismatch, pinned 583231, seen 999, no keys", row)
	}
	if has(l, k1) || has(l, squatter) {
		t.Error("a mismatched login still contributes keys")
	}
	if _, err := os.Stat(f.cacheFile()); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("cache of a mismatched login not removed: %v", err)
	}
	if e := f.storedPins(t)["octocat"]; e.ID != 583231 || e.KeysSHA256 != "" || !slices.Contains(e.Rejected, fp(k1)) {
		t.Errorf("stored pin = %+v, want the pin kept with no trusted keys and k1 rejected", e)
	}

	// A 404 for a pinned login is a mismatch too.
	f.gh.set("octocat", 583231, line1)
	if row := refreshRow(t, l); row.Status != RefreshOK || !has(l, k1) {
		t.Fatalf("back on the pinned account: %+v", row)
	}
	if e := f.storedPins(t)["octocat"]; len(e.Rejected) != 0 {
		t.Errorf("rejected fingerprints kept after the login named its account again: %v", e.Rejected)
	}
	f.gh.remove("octocat")
	if row := refreshRow(t, l); row.Status != RefreshMismatch || row.SeenID != 0 || has(l, k1) {
		t.Errorf("pinned login gone: row %+v, key kept %v; want mismatch and dropped", row, has(l, k1))
	}
}

// TestMismatchedKeysNeverComeBack: keys dropped for a mismatch stay dropped
// when GitHub then goes down: not from memory, not from the disk cache, not
// after a restart, and not when an earlier release rewrites the cache file
// after a rollback. Offering one starts no request, before or after a
// restart.
func TestMismatchedKeysNeverComeBack(t *testing.T) {
	k1, line1 := genKey(t)
	f := newPinFixture(t)
	f.gh.set("octocat", 583231, line1)
	l := f.open(t)
	f.gh.set("octocat", 999, "")
	if row := refreshRow(t, l); row.Status != RefreshMismatch {
		t.Fatalf("row = %+v, want mismatch", row)
	}

	// Outage in the same process.
	f.outage()
	if row := refreshRow(t, l); row.Status != RefreshUnreachable || row.Keys != 0 || has(l, k1) {
		t.Errorf("outage after a mismatch: row %+v, key back %v", row, has(l, k1))
	}
	// An unknown-key fetch for the dropped key starts nothing.
	api, keys := f.gh.calls()
	if l.RefetchForUnknownKey(context.Background(), fp(k1)) {
		t.Error("dropped key admitted by an unknown-key fetch")
	}
	if a2, k2 := f.gh.calls(); a2 != api || k2 != keys {
		t.Error("a key dropped for a mismatch started a fetch")
	}

	// Restart during the outage, with the old keys planted back in the
	// cache file as an earlier release would write them.
	if err := os.WriteFile(f.cacheFile(), []byte(line1), 0o600); err != nil {
		t.Fatal(err)
	}
	l2 := f.open(t)
	if has(l2, k1) {
		t.Error("a key dropped for a mismatch came back from the disk cache after a restart")
	}
	api, keys = f.gh.calls()
	if l2.RefetchForUnknownKey(context.Background(), fp(k1)) {
		t.Error("dropped key admitted by an unknown-key fetch after a restart")
	}
	if a2, k2 := f.gh.calls(); a2 != api || k2 != keys {
		t.Error("after a restart, a key dropped for a mismatch started a fetch")
	}
}

// TestMismatchAtStartRejectsCachedKeys: a mismatch found by the first fetch
// after a restart rejects the keys of the login's cache (trusted or not), on
// disk too, so none of them starts an unknown-key fetch.
func TestMismatchAtStartRejectsCachedKeys(t *testing.T) {
	k1, line1 := genKey(t)
	k2, line2 := genKey(t)
	f := newPinFixture(t)
	f.gh.set("octocat", 583231, line1)
	f.open(t)
	// A cache that is not the confirmed one (rewritten after a rollback).
	if err := os.WriteFile(f.cacheFile(), []byte(line1+line2), 0o600); err != nil {
		t.Fatal(err)
	}
	f.gh.set("octocat", 999, "")
	l := f.open(t)
	e := f.storedPins(t)["octocat"]
	if !slices.Contains(e.Rejected, fp(k1)) || !slices.Contains(e.Rejected, fp(k2)) {
		t.Errorf("stored rejected = %v, want k1 and k2 from the cache", e.Rejected)
	}
	api, keys := f.gh.calls()
	for _, k := range []gossh.PublicKey{k1, k2} {
		if l.RefetchForUnknownKey(context.Background(), fp(k)) {
			t.Error("a cached key of a mismatched login was admitted")
		}
	}
	if a2, k2c := f.gh.calls(); a2 != api || k2c != keys {
		t.Error("a cached key of a login found mismatched at start-up started a fetch")
	}
}

// TestRejectedKeysStartNoFetch: a key of the account the login names instead
// of its pin is not accepted, and once a fetch has seen it on the login, it
// never starts another (the limit is not what stops it).
func TestRejectedKeysStartNoFetch(t *testing.T) {
	k1, line1 := genKey(t)
	squatter, squatterLine := genKey(t)
	f := newPinFixture(t)
	f.gh.set("octocat", 583231, line1)
	l := f.open(t)
	f.gh.set("octocat", 999, squatterLine)

	ctx := context.Background()
	if l.RefetchForUnknownKey(ctx, fp(squatter)) {
		t.Fatal("the other account's key was admitted")
	}
	if has(l, k1) {
		t.Error("the pinned account's key kept after the mismatch")
	}
	for _, k := range []gossh.PublicKey{squatter, k1} {
		f.clock.Advance(2 * unknownKeyRefetchEvery) // the limit is free
		api, keys := f.gh.calls()
		if l.RefetchForUnknownKey(ctx, fp(k)) {
			t.Error("a rejected key was admitted")
		}
		if a2, k2 := f.gh.calls(); a2 != api || k2 != keys {
			t.Errorf("a rejected key started %d API and %d .keys requests", a2-api, k2-keys)
		}
	}
}

// TestKeysOnlyFromIDAddressedList: keys are accepted only from the API's
// id-addressed key list, never from github.com/<login>.keys (another host),
// never through a redirect, and on a first fetch they are those of the very
// id that is pinned.
func TestKeysOnlyFromIDAddressedList(t *testing.T) {
	k1, line1 := genKey(t)
	other, otherLine := genKey(t)

	t.Run(".keys lists another key", func(t *testing.T) {
		f := newPinFixture(t)
		f.gh.set("octocat", 583231, line1)
		f.gh.setDotKeys("octocat", line1+otherLine)
		l := f.open(t)
		if !has(l, k1) || has(l, other) {
			t.Fatalf("start: k1 %v (want true), other %v (want false)", has(l, k1), has(l, other))
		}
		api, _ := f.gh.calls()
		if l.RefetchForUnknownKey(context.Background(), fp(other)) {
			t.Error("a key only .keys lists was admitted")
		}
		if a2, _ := f.gh.calls(); a2 == api {
			t.Error("a change on .keys did not lead to an id-addressed fetch")
		}
		if has(l, other) || !has(l, k1) {
			t.Errorf("after the fetch: k1 %v (want true), other %v (want false)", has(l, k1), has(l, other))
		}
	})

	t.Run("redirected key list", func(t *testing.T) {
		f := newPinFixture(t)
		f.gh.set("octocat", 583231, line1)
		l := f.open(t)
		f.gh.setAccountKeys(999, otherLine)
		f.gh.setIDKeys(0, "", "/user/999/keys")
		row := refreshRow(t, l)
		if row.Status != RefreshUnreachable || has(l, other) || !has(l, k1) {
			t.Errorf("row %+v, other %v, k1 %v: want unreachable, the redirect not followed, k1 kept", row, has(l, other), has(l, k1))
		}
	})

	t.Run("login renamed during the first fetch", func(t *testing.T) {
		f := newPinFixture(t)
		f.gh.set("octocat", 583231, line1)
		f.gh.setAccountKeys(999, otherLine)
		var once sync.Once
		f.gh.setOnIDKeys(func(int64) {
			once.Do(func() { f.gh.set("octocat", 999, otherLine) })
		})
		l := f.open(t)
		if !has(l, k1) || has(l, other) {
			t.Errorf("k1 %v (want true), other %v (want false): keys of another id than the pinned one", has(l, k1), has(l, other))
		}
		if e := f.storedPins(t)["octocat"]; e.ID != 583231 || e.KeysSHA256 != keysDigest([]byte(line1)) {
			t.Errorf("stored pin = %+v, want 583231 with its own keys' digest", e)
		}
	})
}

// TestMalformedKeyListIsAnOutage: a 200 key list that is not exactly the
// expected JSON (cut short, too large, an HTML page, null, an object, an
// entry with no key, trailing bytes) is an outage: last known keys stay, and
// it is never read as "no keys".
func TestMalformedKeyListIsAnOutage(t *testing.T) {
	k1, line1 := genKey(t)
	_, line2 := genKey(t)
	full := keyListJSON(line1 + line2)
	for name, body := range map[string]string{
		"cut short":     full[:len(full)/2],
		"too large":     `[` + strings.Repeat(" ", githubKeysMaxBytes) + `]`,
		"html":          "<html><body>Unicorn!</body></html>",
		"null":          "null",
		"object":        `{"keys":[]}`,
		"entry no key":  `[{"id":1}]`,
		"entry null":    `[null]`,
		"trailing data": full + "x",
	} {
		t.Run(name, func(t *testing.T) {
			f := newPinFixture(t)
			f.gh.set("octocat", 583231, line1)
			l := f.open(t)
			stored := f.storedPins(t)["octocat"]
			f.gh.setIDKeys(0, body, "")
			row := refreshRow(t, l)
			if row.Status != RefreshUnreachable || row.Keys != 1 || !has(l, k1) {
				t.Errorf("row = %+v, k1 kept %v: want unreachable with k1 kept", row, has(l, k1))
			}
			if got := f.storedPins(t)["octocat"]; !got.equal(stored) {
				t.Errorf("a malformed key list changed the pin: %+v", got)
			}
		})
	}
	t.Run("empty list is no keys", func(t *testing.T) {
		f := newPinFixture(t)
		f.gh.set("octocat", 583231, line1)
		l := f.open(t)
		f.gh.setIDKeys(0, "[]", "")
		if row := refreshRow(t, l); row.Status != RefreshOK || row.Keys != 0 || has(l, k1) {
			t.Errorf("row = %+v: want ok with no keys", row)
		}
	})
}

// TestKeyListUnavailable: the account resolves but its id-addressed key list
// cannot be had (a 404, a 500): an outage, never "no keys".
func TestKeyListUnavailable(t *testing.T) {
	k1, line1 := genKey(t)
	k2, line2 := genKey(t)
	for _, status := range []int{http.StatusNotFound, http.StatusInternalServerError} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			f := newPinFixture(t)
			f.gh.set("octocat", 583231, line1)
			l := f.open(t)
			f.gh.set("octocat", 583231, line1+line2)
			f.gh.setIDKeys(status, "", "")
			row := refreshRow(t, l)
			if row.Status != RefreshUnreachable || !strings.Contains(row.Error, "key list of account 583231") {
				t.Errorf("row = %+v, want unreachable naming the key list", row)
			}
			if !has(l, k1) || has(l, k2) {
				t.Errorf("k1 kept %v (want true), k2 accepted %v (want false)", has(l, k1), has(l, k2))
			}
		})
	}
}

// TestMalformedAccountIsAnOutage: a 200 user response with the pinned id but
// no login, a null login or an empty one is an outage, not a rename: the
// keys and the pin's trusted digest stay.
func TestMalformedAccountIsAnOutage(t *testing.T) {
	k1, line1 := genKey(t)
	for name, body := range map[string]string{
		"no login":    `{"id":583231}`,
		"null login":  `{"login":null,"id":583231}`,
		"empty login": `{"login":"","id":583231}`,
		"null id":     `{"login":"octocat","id":null}`,
		"string id":   `{"login":"octocat","id":"583231"}`,
	} {
		t.Run(name, func(t *testing.T) {
			f := newPinFixture(t)
			f.gh.set("octocat", 583231, line1)
			l := f.open(t)
			f.gh.setAccountBody("octocat", body)
			row := refreshRow(t, l)
			if row.Status != RefreshUnreachable || !has(l, k1) {
				t.Errorf("row = %+v, k1 kept %v: want unreachable with k1 kept", row, has(l, k1))
			}
			if e := f.storedPins(t)["octocat"]; e.KeysSHA256 != keysDigest([]byte(line1)) {
				t.Errorf("a malformed account response revoked the cache: %+v", e)
			}
		})
	}
}

// TestPinWriteFailure: when the pin store cannot be written nothing new is
// accepted: no key at the first pin, and on a pinned login no key new on
// the account, though a key the account stopped listing still leaves.
func TestPinWriteFailure(t *testing.T) {
	k1, line1 := genKey(t)
	k2, line2 := genKey(t)
	k3, line3 := genKey(t)

	t.Run("first pin", func(t *testing.T) {
		f := newPinFixture(t)
		f.gh.set("octocat", 583231, line1)
		f.failWrites.Store(true)
		l := f.open(t)
		if has(l, k1) {
			t.Error("a key was accepted although its pin was not written")
		}
		row := refreshRow(t, l)
		if row.Status != RefreshPinWriteFailed || row.Keys != 0 || row.PinnedID != 0 {
			t.Errorf("row = %+v, want pin-write-failed with no keys and no pin", row)
		}
		if _, err := os.Stat(f.pinsPath); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("pin store exists after failed writes: %v", err)
		}
		f.failWrites.Store(false)
		if row := refreshRow(t, l); row.Status != RefreshPinned || !has(l, k1) {
			t.Errorf("after the store recovers: %+v", row)
		}
	})

	t.Run("pinned login", func(t *testing.T) {
		f := newPinFixture(t)
		f.gh.set("octocat", 583231, line1+line2)
		l := f.open(t)
		f.failWrites.Store(true)
		f.gh.set("octocat", 583231, line1+line3) // k2 removed, k3 added
		row := refreshRow(t, l)
		if row.Status != RefreshPinWriteFailed {
			t.Errorf("status = %s, want pin-write-failed", row.Status)
		}
		if !has(l, k1) {
			t.Error("a key still listed was dropped")
		}
		if has(l, k3) {
			t.Error("a new key was accepted although the pin store was not written")
		}
		if has(l, k2) {
			t.Error("a key the account stopped listing was kept")
		}
		if got := f.storedPins(t)["octocat"].KeysSHA256; got != keysDigest([]byte(line1+line2)) {
			t.Error("the stored digest changed although the write failed")
		}
	})
}

// TestStartPinWriteFailureKeepsTrustedCache: at a start where GitHub lists
// a new key and the pin store cannot be written, the keys of the trusted
// cache that GitHub still lists are kept; the new one is not accepted.
func TestStartPinWriteFailureKeepsTrustedCache(t *testing.T) {
	k1, line1 := genKey(t)
	k2, line2 := genKey(t)
	f := newPinFixture(t)
	f.gh.set("octocat", 583231, line1)
	f.open(t)

	f.gh.set("octocat", 583231, line1+line2)
	f.failWrites.Store(true)
	l := f.open(t)
	if !has(l, k1) {
		t.Error("a trusted cached key GitHub still lists was dropped at start")
	}
	if has(l, k2) {
		t.Error("a new key was accepted although the pin store was not written")
	}
}

// TestEveryKeyRemovedThenOutage: GitHub stops listing every key of the
// account while the pin store cannot be written. The key leaves, and does not
// come back when GitHub then goes down: not from memory, and not from the
// disk cache after a restart.
func TestEveryKeyRemovedThenOutage(t *testing.T) {
	k1, line1 := genKey(t)
	f := newPinFixture(t)
	f.gh.set("octocat", 583231, line1)
	l := f.open(t)

	f.failWrites.Store(true)
	f.gh.set("octocat", 583231, "")
	if row := refreshRow(t, l); row.Status != RefreshPinWriteFailed || has(l, k1) {
		t.Fatalf("row = %+v, k1 kept %v: want pin-write-failed with k1 gone", row, has(l, k1))
	}
	f.outage()
	if row := refreshRow(t, l); row.Status != RefreshUnreachable || has(l, k1) {
		t.Errorf("row = %+v: a key GitHub stopped listing came back during an outage", row)
	}
	if l2 := f.open(t); has(l2, k1) {
		t.Error("a key GitHub stopped listing came back from the disk cache after a restart")
	}
}

// TestMismatchWriteFailureStaysRevoked: a mismatch whose pin write fails is
// kept in force in the process and written again at every later refresh,
// outages included; once written, a cache restored from a backup is not
// trusted after a restart.
func TestMismatchWriteFailureStaysRevoked(t *testing.T) {
	k1, line1 := genKey(t)
	f := newPinFixture(t)
	f.gh.set("octocat", 583231, line1)
	l := f.open(t)
	backup, err := os.ReadFile(f.cacheFile())
	if err != nil {
		t.Fatal(err)
	}

	f.failWrites.Store(true)
	f.gh.set("octocat", 999, "")
	if row := refreshRow(t, l); row.Status != RefreshMismatch || has(l, k1) {
		t.Fatalf("row = %+v: want mismatch with k1 dropped", row)
	}
	if e := f.storedPins(t)["octocat"]; e.KeysSHA256 == "" {
		t.Fatal("the failed write cleared the stored digest; the test needs it left")
	}

	// The cache is restored from a backup; GitHub goes down.
	if err := os.WriteFile(f.cacheFile(), backup, 0o600); err != nil {
		t.Fatal(err)
	}
	f.outage()
	writes := f.writes.Load()
	if row := refreshRow(t, l); has(l, k1) || row.Keys != 0 {
		t.Errorf("row = %+v: a revoked key came back in the same process", row)
	}
	if f.writes.Load() == writes {
		t.Error("an outage refresh did not write the pending revocation again")
	}

	// The store recovers: the next refresh, still in the outage, records
	// the revocation.
	f.failWrites.Store(false)
	refreshRow(t, l)
	if e := f.storedPins(t)["octocat"]; e.KeysSHA256 != "" || !slices.Contains(e.Rejected, fp(k1)) {
		t.Fatalf("stored pin = %+v, want no trusted digest and k1 rejected", e)
	}
	if err := os.WriteFile(f.cacheFile(), backup, 0o600); err != nil {
		t.Fatal(err)
	}
	if l2 := f.open(t); has(l2, k1) {
		t.Error("a restored cache was trusted after a restart")
	}
}

// TestCorruptPinStoreFailsClosed: a pin store that cannot be parsed stops the
// lookup from being built (shed-server does not start), and the file is left
// as it was; it is not read at all without GitHub logins.
func TestCorruptPinStoreFailsClosed(t *testing.T) {
	for name, content := range map[string]string{
		"not json":         "{not json",
		"wrong version":    `{"version":2,"pins":{}}`,
		"no id":            `{"version":1,"pins":{"octocat":{"id":0}}}`,
		"upper case":       `{"version":1,"pins":{"OctoCat":{"id":1}}}`,
		"invalid login":    `{"version":1,"pins":{"../x":{"id":1}}}`,
		"bad digest":       `{"version":1,"pins":{"octocat":{"id":1,"keys_sha256":"zz"}}}`,
		"bad rejected":     `{"version":1,"pins":{"octocat":{"id":1,"rejected":["SHA256:x"]}}}`,
		"too many rejects": `{"version":1,"pins":{"octocat":{"id":1,"rejected":[` + manyFingerprints(maxRejectedPerLogin+1) + `]}}}`,
		"truncated file":   `{"version":1,"pins":{"octocat":{"id":58`,
	} {
		t.Run(name, func(t *testing.T) {
			_, line := genKey(t)
			f := newPinFixture(t)
			f.gh.set("octocat", 583231, line)
			if err := os.WriteFile(f.pinsPath, []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := f.tryOpen(); err == nil {
				t.Fatal("a corrupt pin store was accepted")
			}
			if api, keys := f.gh.calls(); api != 0 || keys != 0 {
				t.Error("GitHub was asked although the pin store is corrupt")
			}
			if got, _ := os.ReadFile(f.pinsPath); string(got) != content {
				t.Error("the corrupt pin store was rewritten")
			}
		})
	}

	t.Run("ignored without logins", func(t *testing.T) {
		dir := t.TempDir()
		pins := filepath.Join(dir, PinsFileName)
		if err := os.WriteFile(pins, []byte("{not json"), 0o600); err != nil {
			t.Fatal(err)
		}
		_, line := genKey(t)
		if _, err := NewConfigLookup(ConfigOptions{
			SSH:      &config.SSHAuthConfig{Mode: config.SSHAuthEnforce, AuthorizedKeys: []string{line}},
			PinsPath: pins,
		}); err != nil {
			t.Errorf("a server with no GitHub logins refused a pin store it never reads: %v", err)
		}
	})
}

// manyFingerprints is n distinct well-formed fingerprints, JSON-quoted and
// comma-separated.
func manyFingerprints(n int) string {
	out := make([]string, n)
	for i := range out {
		s := strconv.Itoa(i)
		out[i] = `"SHA256:` + strings.Repeat("A", 43-len(s)) + s + `"`
	}
	return strings.Join(out, ",")
}

// TestRejectedListBounded: the rejected fingerprints a pin keeps are bounded,
// the oldest dropped first.
func TestRejectedListBounded(t *testing.T) {
	var old []string
	for i := 0; i < maxRejectedPerLogin; i++ {
		old = append(old, "old"+strconv.Itoa(i))
	}
	got := mergeRejected(old, []string{"old3", "new1", "new2"})
	if len(got) != maxRejectedPerLogin || got[len(got)-1] != "new2" || got[0] != "old2" {
		t.Errorf("merged = %v, want the newest %d", got, maxRejectedPerLogin)
	}
}

// TestCaseDuplicateLoginsAreOne: two spellings of one login are one login:
// fetched once, one row, one pin.
func TestCaseDuplicateLoginsAreOne(t *testing.T) {
	k1, line1 := genKey(t)
	f := newPinFixture(t)
	f.logins = []string{"octocat", "OctoCat"}
	f.gh.set("octocat", 583231, line1)
	l := f.open(t)
	if !has(l, k1) {
		t.Fatal("key not accepted")
	}
	api, _ := f.gh.calls()
	r, err := l.Refresh(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Logins) != 1 || r.Logins[0].Login != "octocat" {
		t.Errorf("rows = %+v, want one row for octocat", r.Logins)
	}
	if a2, _ := f.gh.calls(); a2-api != 2 {
		t.Errorf("a refresh made %d API requests, want 2 (one login)", a2-api)
	}
	if pins := f.storedPins(t); len(pins) != 1 || pins["octocat"].ID != 583231 {
		t.Errorf("pins = %+v", pins)
	}
}

// TestAPIUnreachableKeysReachable: the REST API cannot be reached but
// github.com/<login>.keys can and lists a new key. The last known keys stay
// and the new key is not accepted: keys are taken only with the account
// confirmed.
func TestAPIUnreachableKeysReachable(t *testing.T) {
	k1, line1 := genKey(t)
	k2, line2 := genKey(t)
	f := newPinFixture(t)
	f.gh.set("octocat", 583231, line1)
	l := f.open(t)

	f.gh.set("octocat", 583231, line1+line2)
	dead := httptest.NewServer(http.NotFoundHandler())
	dead.Close()
	l.gh.apiBase = dead.URL

	row := refreshRow(t, l)
	if row.Status != RefreshUnreachable || row.Error == "" {
		t.Errorf("row = %+v, want unreachable with the error", row)
	}
	if !has(l, k1) {
		t.Error("last known key dropped while the API is unreachable")
	}
	if has(l, k2) {
		t.Error("a new key was accepted without the account being confirmed")
	}
	if l.RefetchForUnknownKey(context.Background(), fp(k2)) {
		t.Error("an unknown-key fetch accepted a key without the account being confirmed")
	}
}

// TestRateLimitIsNotGone: a rate-limit answer (403 with remaining 0, or 429)
// keeps the last known keys and accepts nothing new; it is never read as
// "the login is gone". Until the limit resets no API request is made.
func TestRateLimitIsNotGone(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		header http.Header
	}{
		{"primary 403", http.StatusForbidden, http.Header{"X-Ratelimit-Remaining": {"0"}, "X-Ratelimit-Limit": {"60"}}},
		{"secondary 403", http.StatusForbidden, http.Header{"Retry-After": {"60"}}},
		{"429", http.StatusTooManyRequests, http.Header{"Retry-After": {"30"}}},
		{"plain 403", http.StatusForbidden, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			k1, line1 := genKey(t)
			k2, line2 := genKey(t)
			f := newPinFixture(t)
			f.gh.set("octocat", 583231, line1)
			l := f.open(t)
			f.gh.set("octocat", 583231, line1+line2)
			h := tc.header.Clone()
			if h == nil {
				h = http.Header{}
			}
			if tc.name == "primary 403" {
				h.Set("X-Ratelimit-Reset", strconv.FormatInt(f.clock.Now().Add(10*time.Minute).Unix(), 10))
			}
			f.gh.setAPIStatus(tc.status, h)

			row := refreshRow(t, l)
			if row.Status != RefreshUnreachable {
				t.Errorf("status = %s, want unreachable", row.Status)
			}
			if !has(l, k1) || has(l, k2) {
				t.Errorf("rate limited: k1 kept %v (want true), k2 accepted %v (want false)", has(l, k1), has(l, k2))
			}
			if e := f.storedPins(t)["octocat"]; e.ID != 583231 || e.KeysSHA256 == "" {
				t.Errorf("rate limit changed the pin: %+v", e)
			}
			if tc.name == "plain 403" {
				return
			}
			api, _ := f.gh.calls()
			refreshRow(t, l)
			if a2, _ := f.gh.calls(); a2 != api {
				t.Errorf("%d API requests made before the limit reset", a2-api)
			}
			f.clock.Advance(11 * time.Minute)
			f.gh.setAPIStatus(0, nil)
			if row := refreshRow(t, l); row.Status != RefreshOK || !has(l, k2) {
				t.Errorf("after the limit reset: %+v", row)
			}
		})
	}
}

// TestHugeRateLimitHeaders: a retry-after or reset far beyond any sane value
// holds API requests off for the bound (an hour), never overflowing into a
// deadline in the past that would let the next refresh ask at once.
func TestHugeRateLimitHeaders(t *testing.T) {
	for name, h := range map[string]http.Header{
		"retry-after": {"Retry-After": {"9223372036854775807"}},
		"reset":       {"X-Ratelimit-Remaining": {"0"}, "X-Ratelimit-Reset": {"9223372036854775807"}},
	} {
		t.Run(name, func(t *testing.T) {
			_, line1 := genKey(t)
			f := newPinFixture(t)
			f.gh.set("octocat", 583231, line1)
			l := f.open(t)
			f.gh.setAPIStatus(http.StatusForbidden, h)
			refreshRow(t, l)
			f.gh.setAPIStatus(0, nil)

			api, _ := f.gh.calls()
			f.clock.Advance(59 * time.Minute)
			refreshRow(t, l)
			if a2, _ := f.gh.calls(); a2 != api {
				t.Errorf("%d API requests within the back-off", a2-api)
			}
			f.clock.Advance(2 * time.Minute)
			if row := refreshRow(t, l); row.Status != RefreshOK {
				t.Errorf("after the bound: %+v", row)
			}
		})
	}
}

// TestUnpinnedLoginGone: a login with no pin that names no account has no
// keys and pins nothing.
func TestUnpinnedLoginGone(t *testing.T) {
	f := newPinFixture(t)
	l := f.open(t)
	if row := refreshRow(t, l); row.Status != RefreshGone || row.Keys != 0 {
		t.Errorf("row = %+v, want gone", row)
	}
	if pins := f.storedPins(t); len(pins) != 0 {
		t.Errorf("pinned %v", pins)
	}
}

// TestConfigIDWinsOverStoredPin: an account id from config is the pin, over
// the one stored.
func TestConfigIDWinsOverStoredPin(t *testing.T) {
	k1, line1 := genKey(t)
	f := newPinFixture(t)
	f.gh.set("octocat", 583231, line1)
	f.open(t) // stores 583231

	// The account was deleted and recreated; config names the new one.
	f.gh.set("octocat", 777, line1)
	f.ids = map[string]int64{"OctoCat": 777}
	l := f.open(t)
	if !has(l, k1) {
		t.Error("key of the account config names was not accepted")
	}
	if e := f.storedPins(t)["octocat"]; e.ID != 777 {
		t.Errorf("stored pin = %+v, want config's 777", e)
	}

	// Config names 777 and the login names the old account: a mismatch.
	f.gh.set("octocat", 583231, line1)
	if row := refreshRow(t, l); row.Status != RefreshMismatch || row.PinnedID != 777 || has(l, k1) {
		t.Errorf("row = %+v, want a mismatch against config's 777", row)
	}
}

// TestDiskCacheBoundToPin: at a start with GitHub down, the disk cache is
// used only when it is the cache last confirmed for the pin; a cache that
// was changed (by an earlier release after a rollback, or by hand) is not.
// A login never pinned may use the cache an earlier release wrote.
func TestDiskCacheBoundToPin(t *testing.T) {
	k1, line1 := genKey(t)
	k2, line2 := genKey(t)

	t.Run("confirmed cache", func(t *testing.T) {
		f := newPinFixture(t)
		f.gh.set("octocat", 583231, line1)
		f.open(t)
		f.gh.setAPIStatus(http.StatusBadGateway, nil)
		f.gh.setKeysStatus(http.StatusBadGateway)
		if l := f.open(t); !has(l, k1) {
			t.Error("the confirmed cache was not used during an outage")
		}
	})

	t.Run("changed cache", func(t *testing.T) {
		f := newPinFixture(t)
		f.gh.set("octocat", 583231, line1)
		f.open(t)
		if err := os.WriteFile(f.cacheFile(), []byte(line1+line2), 0o600); err != nil {
			t.Fatal(err)
		}
		f.gh.setAPIStatus(http.StatusBadGateway, nil)
		f.gh.setKeysStatus(http.StatusBadGateway)
		l := f.open(t)
		if has(l, k1) || has(l, k2) {
			t.Error("a cache that is not the confirmed one was used")
		}
	})

	t.Run("never pinned", func(t *testing.T) {
		f := newPinFixture(t)
		if err := os.MkdirAll(f.cacheDir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(f.cacheFile(), []byte(line1), 0o600); err != nil {
			t.Fatal(err)
		}
		f.gh.setAPIStatus(http.StatusBadGateway, nil)
		l := f.open(t)
		if !has(l, k1) {
			t.Error("an unpinned login lost its earlier release's cache during an outage")
		}
		// The first good fetch pins, and the cache stays a plain .keys file
		// an earlier release reads.
		f.gh.setAPIStatus(0, nil)
		f.gh.set("octocat", 583231, line2)
		if row := refreshRow(t, l); row.Status != RefreshPinned || !has(l, k2) || has(l, k1) {
			t.Errorf("row = %+v", row)
		}
		got, _ := os.ReadFile(f.cacheFile())
		if string(got) != line2 {
			t.Errorf("cache file = %q, want the .keys body", got)
		}
	})
}

// TestLightFetchSpendsNoAPI: an unknown key fetches each login's .keys and
// asks the API only when they changed.
func TestLightFetchSpendsNoAPI(t *testing.T) {
	_, line1 := genKey(t)
	stranger, _ := genKey(t)
	f := newPinFixture(t)
	f.gh.set("octocat", 583231, line1)
	l := f.open(t)
	api, keys := f.gh.calls()
	if l.RefetchForUnknownKey(context.Background(), fp(stranger)) {
		t.Error("a stranger's key was admitted")
	}
	a2, k2 := f.gh.calls()
	if a2 != api || k2 != keys+1 {
		t.Errorf("light fetch made %d API and %d .keys requests, want 0 and 1", a2-api, k2-keys)
	}
}

// TestUnknownKeyRefetch: an unknown key starts one fetch and the key added
// upstream is accepted on that same attempt; another within the minute starts
// none (whoever offers it); after the minute one may again.
func TestUnknownKeyRefetch(t *testing.T) {
	_, line1 := genKey(t)
	k2, line2 := genKey(t)
	k3, line3 := genKey(t)
	f := newPinFixture(t)
	f.gh.set("octocat", 583231, line1)
	l := f.open(t)
	ctx := context.Background()

	f.gh.set("octocat", 583231, line1+line2)
	if !l.RefetchForUnknownKey(ctx, fp(k2)) {
		t.Fatal("a key added upstream was not accepted on the triggering attempt")
	}

	// Within the minute: nothing, even for a key that is upstream now.
	f.gh.set("octocat", 583231, line1+line2+line3)
	_, keys := f.gh.calls()
	f.clock.Advance(59 * time.Second)
	if l.RefetchForUnknownKey(ctx, fp(k3)) {
		t.Error("a second fetch ran within the minute")
	}
	if _, k := f.gh.calls(); k != keys {
		t.Error("a second fetch was started within the minute")
	}

	f.clock.Advance(time.Second)
	if !l.RefetchForUnknownKey(ctx, fp(k3)) {
		t.Error("no fetch allowed after the minute")
	}
}

// TestUnknownKeyOnlyInEnforce: in warn mode (the key is let in anyway) an
// unknown key starts no fetch.
func TestUnknownKeyOnlyInEnforce(t *testing.T) {
	_, line1 := genKey(t)
	k2, line2 := genKey(t)
	f := newPinFixture(t)
	f.mode = config.SSHAuthWarn
	f.gh.set("octocat", 583231, line1)
	l := f.open(t)
	f.gh.set("octocat", 583231, line1+line2)
	api, keys := f.gh.calls()
	if l.RefetchForUnknownKey(context.Background(), fp(k2)) {
		t.Error("warn mode admitted a key through a fetch")
	}
	if a2, k2c := f.gh.calls(); a2 != api || k2c != keys {
		t.Error("warn mode started an unknown-key fetch")
	}
}

// TestUnknownKeyOnlySlotHolderWaits: while one unknown key holds the slot and
// waits on a slow fetch, every other unknown key returns at once: no request,
// no wait.
func TestUnknownKeyOnlySlotHolderWaits(t *testing.T) {
	_, line1 := genKey(t)
	f := newPinFixture(t)
	f.gh.set("octocat", 583231, line1)
	l := f.open(t)

	release := make(chan struct{})
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
	entered := make(chan struct{})
	var enteredOnce sync.Once
	f.gh.setOnKeys(func(string) {
		enteredOnce.Do(func() { close(entered) })
		<-release
	})
	_, keys := f.gh.calls()

	first := make(chan bool)
	go func() {
		k, _ := genKey(t)
		first <- l.RefetchForUnknownKey(context.Background(), fp(k))
	}()
	<-f.clock.waiting // the slot holder is waiting
	<-entered         // and its fetch has reached GitHub

	var wg sync.WaitGroup
	results := make(chan bool, 49)
	for i := 0; i < 49; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			k, _ := genKey(t)
			results <- l.RefetchForUnknownKey(context.Background(), fp(k))
		}()
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("other unknown keys waited while the slot holder's fetch was running")
	}
	close(results)
	for r := range results {
		if r {
			t.Error("an unknown key without the slot was admitted")
		}
	}
	select {
	case <-f.clock.waiting:
		t.Error("an unknown key without the slot waited on the clock")
	default:
	}
	if _, k := f.gh.calls(); k-keys != 1 {
		t.Errorf("%d fetches started, want 1", k-keys)
	}
	f.clock.Advance(unknownKeyWait)
	<-first
	f.gh.setOnKeys(nil)
	releaseOnce.Do(func() { close(release) })
	waitIdle(t, l)
}

// TestUnknownKeyWaitsAtMostFiveSeconds: the login that starts a fetch waits
// for it up to five seconds, then is answered with what is known; the fetch
// carries on and its own result lands afterwards, with nothing else asked.
func TestUnknownKeyWaitsAtMostFiveSeconds(t *testing.T) {
	_, line1 := genKey(t)
	k2, line2 := genKey(t)
	f := newPinFixture(t)
	f.gh.set("octocat", 583231, line1)
	l := f.open(t)

	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseFetch := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(releaseFetch)
	f.gh.setOnKeys(func(string) { <-release })
	f.gh.set("octocat", 583231, line1+line2)

	result := make(chan bool)
	go func() { result <- l.RefetchForUnknownKey(context.Background(), fp(k2)) }()
	<-f.clock.waiting // the login is waiting on its five seconds
	f.clock.Advance(4 * time.Second)
	select {
	case <-result:
		t.Fatal("answered before five seconds")
	case <-time.After(50 * time.Millisecond):
	}
	f.clock.Advance(time.Second)
	if <-result {
		t.Error("admitted before the fetch ended")
	}
	f.gh.setOnKeys(nil)
	releaseFetch()
	// The slow fetch itself lands its key: nothing else is asked for.
	waitIdle(t, l)
	if !has(l, k2) {
		t.Error("the slow fetch's key never landed")
	}
}

// TestRefreshIsNeverOlderThanTheCall: a forced refresh while a fetch is
// running does not take that fetch's result (it may predate a key the admin
// just added); it waits for one that starts after it.
func TestRefreshIsNeverOlderThanTheCall(t *testing.T) {
	_, line1 := genKey(t)
	k2, line2 := genKey(t)
	f := newPinFixture(t)
	f.gh.set("octocat", 583231, line1)
	l := f.open(t)

	entered := make(chan struct{})
	release := make(chan struct{})
	var once, releaseOnce sync.Once
	releaseFetch := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(releaseFetch) // before the fake's server closes, on any exit
	f.gh.setOnIDKeys(func(int64) {
		once.Do(func() { close(entered); <-release })
	})
	running := l.request(fetchFull, false)
	<-entered // the running fetch is reading the old key list
	f.gh.set("octocat", 583231, line1+line2)

	done := make(chan LoginRefresh)
	go func() {
		r, err := l.Refresh(context.Background())
		if err != nil {
			t.Error(err)
		}
		done <- r.Logins[0]
	}()
	// Release the running fetch only once the forced refresh has asked.
	deadline := time.Now().Add(10 * time.Second)
	for {
		l.flightMu.Lock()
		asked := l.queued != nil
		l.flightMu.Unlock()
		if asked {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the forced refresh never queued a fetch")
		}
		time.Sleep(time.Millisecond)
	}
	releaseFetch()
	<-running.done
	row := <-done
	if len(row.Added) != 1 || row.Added[0] != fp(k2) || !has(l, k2) {
		t.Errorf("forced refresh row = %+v; it returned a fetch older than the call", row)
	}
}

// TestShutdownCancelsFetches: cancelling the lookup's context (shed-server's
// shutdown) ends a fetch in flight at once, as an outage.
func TestShutdownCancelsFetches(t *testing.T) {
	k1, line1 := genKey(t)
	f := newPinFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f.ctx = ctx
	f.gh.set("octocat", 583231, line1)
	l := f.open(t)

	entered := make(chan struct{})
	release := make(chan struct{})
	var once, releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
	f.gh.setOnIDKeys(func(int64) {
		once.Do(func() { close(entered); <-release })
	})
	done := make(chan LoginRefresh, 1)
	go func() {
		r, err := l.Refresh(context.Background())
		if err != nil {
			t.Error(err)
		}
		done <- r.Logins[0]
	}()
	<-entered
	cancel()
	select {
	case row := <-done:
		if row.Status != RefreshUnreachable || !has(l, k1) {
			t.Errorf("row = %+v: want unreachable with k1 kept", row)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a fetch in flight did not end when the lookup's context was cancelled")
	}
}

// TestRefreshAndLookupsRace runs forced refreshes, scheduled-style fetches,
// unknown-key fetches and lookups at once while GitHub's answers change.
// Under -race it proves the per-login state, the pin store and the live set
// are only touched under their locks, and that a key never disappears while
// its account keeps listing it.
func TestRefreshAndLookupsRace(t *testing.T) {
	k1, line1 := genKey(t)
	_, line2 := genKey(t)
	f := newPinFixture(t)
	f.gh.set("octocat", 583231, line1)
	l := f.open(t)

	ctx := context.Background()
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { // GitHub changes its answer
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			if i%2 == 0 {
				f.gh.set("octocat", 583231, line1+line2)
			} else {
				f.gh.set("octocat", 583231, line1)
			}
			f.clock.Advance(time.Minute)
		}
	}()
	var work sync.WaitGroup
	for i := 0; i < 4; i++ {
		work.Add(3)
		go func() {
			defer work.Done()
			for j := 0; j < 10; j++ {
				if _, err := l.Refresh(ctx); err != nil {
					t.Error(err)
				}
			}
		}()
		go func() {
			defer work.Done()
			for j := 0; j < 10; j++ {
				<-l.request(fetchFull, false).done
				k, _ := genKey(t)
				l.RefetchForUnknownKey(ctx, fp(k))
			}
		}()
		go func() {
			defer work.Done()
			for j := 0; j < 300; j++ {
				if !has(l, k1) {
					t.Error("a key dropped while its account lists it")
					return
				}
				_ = l.Users()
			}
		}()
	}
	work.Wait()
	close(stop)
	wg.Wait()
}

// manyKeys returns n fresh keys and their authorized_keys body.
func manyKeys(t *testing.T, n int) ([]gossh.PublicKey, string) {
	t.Helper()
	keys := make([]gossh.PublicKey, n)
	var body strings.Builder
	for i := range keys {
		k, line := genKey(t)
		keys[i] = k
		body.WriteString(line)
	}
	return keys, body.String()
}

// TestKeyListPagination: the id-addressed key list is read the largest page
// at a time, following each next link, so a key past the first page is
// accepted and stays accepted. A failed page, a next link that leaves the
// list, an unreadable Link header or more pages than the bound is an outage:
// the last known keys stay, nothing new is accepted and the pin is
// unchanged.
func TestKeyListPagination(t *testing.T) {
	// elsewhere is another host that answers the same key list path with a
	// list of its own.
	other, otherLine := genKey(t)
	elsewhere := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_, _ = w.Write([]byte(keyListJSON(otherLine)))
	}))
	t.Cleanup(elsewhere.Close)

	t.Run("every page is read", func(t *testing.T) {
		keys, body := manyKeys(t, 250)
		f := newPinFixture(t)
		f.gh.set("octocat", 583231, body)
		l := f.open(t)
		for i, k := range keys {
			if !has(l, k) {
				t.Fatalf("key %d of 250 not accepted", i+1)
			}
		}
		if got := f.gh.perPages(); len(got) != 3 || got[0] != "100" {
			t.Errorf("key list requests per_page = %v, want 3 pages of 100", got)
		}
		if e := f.storedPins(t)["octocat"]; e.KeysSHA256 != keysDigest([]byte(body)) {
			t.Error("the pin's digest is not of the whole list")
		}
		// A later fetch keeps the key on the last page.
		if row := refreshRow(t, l); row.Status != RefreshOK || row.Keys != 250 || len(row.Removed) != 0 || !has(l, keys[249]) {
			t.Errorf("refresh row = %+v, want ok with all 250 keys", row)
		}
	})

	t.Run("a relative next link on the same path", func(t *testing.T) {
		keys, body := manyKeys(t, 2)
		f := newPinFixture(t)
		f.gh.set("octocat", 583231, body)
		f.gh.setPaging(1, nil, relativeLinks)
		l := f.open(t)
		if !has(l, keys[0]) || !has(l, keys[1]) {
			t.Error("a key on the second page was not accepted")
		}
	})

	t.Run("the page bound", func(t *testing.T) {
		keys, body := manyKeys(t, githubKeysMaxPages)
		f := newPinFixture(t)
		f.gh.set("octocat", 583231, body)
		f.gh.setPaging(1, nil, "")
		l := f.open(t)
		if !has(l, keys[githubKeysMaxPages-1]) {
			t.Fatalf("a list of exactly %d pages was not read whole", githubKeysMaxPages)
		}
		stored := f.storedPins(t)["octocat"]
		extra, extraLine := genKey(t)
		f.gh.set("octocat", 583231, body+extraLine)
		row := refreshRow(t, l)
		if row.Status != RefreshUnreachable || !strings.Contains(row.Error, "more than") || has(l, extra) || !has(l, keys[0]) {
			t.Errorf("row = %+v, extra accepted %v: want unreachable over the page bound, the old keys kept", row, has(l, extra))
		}
		if got := f.storedPins(t)["octocat"]; !got.equal(stored) {
			t.Errorf("a list over the page bound changed the pin: %+v", got)
		}
	})

	for name, tc := range map[string]struct {
		status map[int]int
		link   func(srv string) string
	}{
		"a later page fails":    {status: map[int]int{2: http.StatusInternalServerError}},
		"a later page is a 404": {status: map[int]int{2: http.StatusNotFound}},
		"next link to another host": {link: func(string) string {
			return `<` + elsewhere.URL + `/user/583231/keys?per_page=100&page=2>; rel="next"`
		}},
		"next link to another account": {link: func(srv string) string {
			return `<` + srv + `/user/999/keys?per_page=100&page=1>; rel="next"`
		}},
		"two next links": {link: func(srv string) string {
			return `<` + srv + `/user/583231/keys?page=2>; rel="next", <` + srv + `/user/583231/keys?page=3>; rel="next"`
		}},
		"unreadable link header": {link: func(string) string { return `page=2; rel=next` }},
	} {
		t.Run(name, func(t *testing.T) {
			keys, body := manyKeys(t, 150)
			f := newPinFixture(t)
			f.gh.set("octocat", 583231, body)
			l := f.open(t)
			if !has(l, keys[149]) {
				t.Fatal("start: the key on page 2 was not accepted")
			}
			stored := f.storedPins(t)["octocat"]
			added, addedLine := genKey(t)
			f.gh.set("octocat", 583231, body+addedLine)
			f.gh.setAccountKeys(999, otherLine)
			link := ""
			if tc.link != nil {
				link = tc.link(f.gh.srv.URL)
			}
			f.gh.setPaging(0, tc.status, link)
			row := refreshRow(t, l)
			if row.Status != RefreshUnreachable || row.Keys != 150 {
				t.Errorf("row = %+v, want unreachable with the 150 known keys", row)
			}
			if !has(l, keys[149]) || !has(l, keys[0]) {
				t.Error("a known key was dropped by an incomplete key list")
			}
			if has(l, added) || has(l, other) {
				t.Errorf("accepted from an incomplete list: added %v, other account %v", has(l, added), has(l, other))
			}
			if got := f.storedPins(t)["octocat"]; !got.equal(stored) {
				t.Errorf("an incomplete key list changed the pin: %+v", got)
			}
		})
	}
}

// sshString is the SSH wire encoding of a string.
func sshString(s []byte) []byte {
	out := []byte{byte(len(s) >> 24), byte(len(s) >> 16), byte(len(s) >> 8), byte(len(s))}
	return append(out, s...)
}

// keyListBody renders key list entries, each given as its raw JSON object.
func keyListBody(entries ...string) string { return "[" + strings.Join(entries, ",") + "]" }

// jsonString is s as a JSON string.
func jsonString(s string) string {
	data, _ := json.Marshal(s)
	return string(data)
}

// TestCorruptKeyEntryIsAnOutage: a key list entry whose key string is not a
// valid key of a recognised algorithm, or that names a member twice, is an
// outage: the last known keys stay, nothing is revoked and the pin is
// unchanged. Only a well-formed key of an algorithm this server does not
// know is skipped.
func TestCorruptKeyEntryIsAnOutage(t *testing.T) {
	k1, line1 := genKey(t)
	k2, line2 := genKey(t)
	one := strings.TrimSpace(line1)
	two := strings.TrimSpace(line2)
	edBlob := strings.Fields(one)[1]
	rawBlob, _ := base64.StdEncoding.DecodeString(edBlob)
	future := "ssh-future@example.com " + base64.StdEncoding.EncodeToString(
		append(sshString([]byte("ssh-future@example.com")), sshString(make([]byte, 32))...))
	valid := `{"id":1,"key":` + jsonString(one) + `}`

	for name, body := range map[string]string{
		"invalid base64":               keyListBody(`{"id":1,"key":"ssh-ed25519 !!!"}`),
		"truncated blob":               keyListBody(`{"id":1,"key":` + jsonString("ssh-ed25519 "+base64.StdEncoding.EncodeToString(rawBlob[:len(rawBlob)-5])) + `}`),
		"blob of another algorithm":    keyListBody(`{"id":1,"key":` + jsonString("ssh-rsa "+edBlob) + `}`),
		"only an algorithm":            keyListBody(`{"id":1,"key":"ssh-ed25519"}`),
		"two keys in one string":       keyListBody(`{"id":1,"key":` + jsonString(one+"\n"+two) + `}`),
		"options before the key":       keyListBody(`{"id":1,"key":` + jsonString("no-pty "+one) + `}`),
		"unknown name, foreign blob":   keyListBody(`{"id":1,"key":` + jsonString("ssh-future@example.com "+edBlob) + `}`),
		"unknown name, short blob":     keyListBody(`{"id":1,"key":"ssh-future@example.com AAAA"}`),
		"key not a string":             keyListBody(`{"id":1,"key":5}`),
		"duplicate key member":         keyListBody(`{"id":1,"key":` + jsonString(one) + `,"key":"garbage"}`),
		"duplicate equal key member":   keyListBody(`{"id":1,"key":` + jsonString(one) + `,"key":` + jsonString(one) + `}`),
		"member named in another case": keyListBody(`{"id":1,"key":` + jsonString(one) + `,"Key":"garbage"}`),
		"duplicate id member":          keyListBody(`{"id":1,"id":2,"key":` + jsonString(one) + `}`),
		"one corrupt among valid":      keyListBody(valid, `{"id":2,"key":"ssh-ed25519 !!!"}`),
	} {
		t.Run(name, func(t *testing.T) {
			f := newPinFixture(t)
			f.gh.set("octocat", 583231, line1)
			l := f.open(t)
			stored := f.storedPins(t)["octocat"]
			f.gh.setIDKeys(0, body, "")
			row := refreshRow(t, l)
			if row.Status != RefreshUnreachable || row.Keys != 1 || !has(l, k1) {
				t.Errorf("row = %+v, k1 kept %v: want unreachable with k1 kept", row, has(l, k1))
			}
			if got := f.storedPins(t)["octocat"]; !got.equal(stored) {
				t.Errorf("a corrupt key list changed the pin: %+v", got)
			}
		})
	}

	t.Run("an unsupported algorithm is skipped", func(t *testing.T) {
		f := newPinFixture(t)
		f.gh.set("octocat", 583231, line1)
		l := f.open(t)
		f.gh.setIDKeys(0, keyListBody(`{"id":1,"key":`+jsonString(future)+`}`, `{"id":2,"key":`+jsonString(two+" a comment")+`}`), "")
		row := refreshRow(t, l)
		if row.Status != RefreshOK || row.Keys != 1 || !has(l, k2) || has(l, k1) {
			t.Errorf("row = %+v: want ok with k2 alone", row)
		}
	})
}

// TestUnknownKeysCannotSpendTheAPIAllowance: a stranger offering an unknown
// key every minute for hours, against two logins whose pins no longer match
// and one legitimate login, spends at most the unknown-key budget in any
// hour and never meets GitHub's rate limit (60 an hour, unauthenticated), so
// the scheduled refresh and `shed users refresh` still confirm a key the
// legitimate login adds. With the mismatched logins' .keys unchanged it
// spends nothing after the first look.
func TestUnknownKeysCannotSpendTheAPIAllowance(t *testing.T) {
	for _, rotate := range []bool{false, true} {
		name := "unchanged .keys"
		if rotate {
			name = "the squatters change their keys every minute"
		}
		t.Run(name, func(t *testing.T) {
			_, lineA := genKey(t)
			_, lineB := genKey(t)
			_, lineC := genKey(t)
			f := newPinFixture(t)
			f.logins = []string{"alpha", "bravo", "carol"}
			f.gh.set("alpha", 101, lineA)
			f.gh.set("bravo", 102, lineB)
			f.gh.set("carol", 103, lineC)
			l := f.open(t)
			ctx := context.Background()

			_, sqA := genKey(t)
			_, sqB := genKey(t)
			f.gh.set("alpha", 901, sqA)
			f.gh.set("bravo", 902, sqB)
			r, err := l.Refresh(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if r.Logins[0].Status != RefreshMismatch || r.Logins[1].Status != RefreshMismatch || r.Logins[2].Status != RefreshOK {
				t.Fatalf("setup rows = %+v, want two mismatches and one ok", r.Logins)
			}
			f.gh.setRateLimit(f.clock, 60)

			stranger, _ := genKey(t)
			start, _ := f.gh.calls()
			hourStart := start
			for minute := 1; minute <= 3*60; minute++ {
				f.clock.Advance(unknownKeyRefetchEvery)
				if rotate {
					_, a := genKey(t)
					_, b := genKey(t)
					f.gh.setAccountKeys(901, a)
					f.gh.setAccountKeys(902, b)
				}
				if l.RefetchForUnknownKey(ctx, fp(stranger)) {
					t.Fatal("the stranger's key was admitted")
				}
				waitIdle(t, l)
				if minute%60 == 0 {
					api, _ := f.gh.calls()
					if api-hourStart > unknownKeyAPIBudget {
						t.Errorf("hour %d: unknown keys made %d API requests, over the budget of %d", minute/60, api-hourStart, unknownKeyAPIBudget)
					}
					hourStart = api
				}
			}
			end, _ := f.gh.calls()
			if !rotate && end-start > 2 {
				t.Errorf("unchanged .keys bodies cost %d API requests over three hours, want at most 2 (the first look at each mismatched login)", end-start)
			}
			if n := f.gh.refused(); n != 0 {
				t.Errorf("GitHub refused %d API requests for its rate limit", n)
			}

			// The legitimate login adds a key: the scheduled refresh confirms it.
			added, addedLine := genKey(t)
			f.gh.setAccountKeys(103, lineC+addedLine)
			<-l.request(fetchFull, false).done
			if !has(l, added) {
				t.Error("the scheduled refresh did not confirm a newly added key")
			}
			// And again: `shed users refresh` confirms it.
			forced, forcedLine := genKey(t)
			f.gh.setAccountKeys(103, lineC+addedLine+forcedLine)
			r, err = l.Refresh(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if row := r.Logins[2]; row.Status != RefreshOK || len(row.Added) != 1 || !has(l, forced) {
				t.Errorf("forced refresh row = %+v: want ok with the new key", row)
			}
		})
	}
}

// TestDetectorAsksAgainAfterAnOutage: a .keys change whose API fetch failed
// is not taken as seen, so the next unknown key asks again and the key the
// login added is accepted then.
func TestDetectorAsksAgainAfterAnOutage(t *testing.T) {
	_, line1 := genKey(t)
	k2, line2 := genKey(t)
	f := newPinFixture(t)
	f.gh.set("octocat", 583231, line1)
	l := f.open(t)
	ctx := context.Background()

	f.gh.set("octocat", 583231, line1+line2)
	f.gh.setAPIStatus(http.StatusServiceUnavailable, nil)
	if l.RefetchForUnknownKey(ctx, fp(k2)) {
		t.Fatal("accepted while the API is down")
	}
	f.gh.setAPIStatus(0, nil)
	f.clock.Advance(unknownKeyRefetchEvery)
	if !l.RefetchForUnknownKey(ctx, fp(k2)) {
		t.Error("an unchanged .keys whose last fetch failed was not asked about again")
	}
}
