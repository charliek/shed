package config

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// ClientConfig.Update — the locked read-modify-write.
//
// The bug these pin is shed#299: `SaveToPath` serializes the WHOLE document, so
// two `shed` processes that each loaded the config and then saved it do not
// merge — the second rename discards everything the first committed, including
// the fields it never looked at. A credential persist losing to a `shed list`
// cache refresh is how it shows up in practice.
// ---------------------------------------------------------------------------

// newTestConfig returns an empty client config rooted at a fresh temp dir.
func newTestConfig(t *testing.T) *ClientConfig {
	t.Helper()
	cfg, err := LoadClientConfigFromPath(filepath.Join(t.TempDir(), "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

// setEntry is the mutation these tests use most: one field write on one entry.
func setEntry(name string, entry ServerEntry) func(*ClientConfig) error {
	return func(c *ClientConfig) error {
		c.Servers[name] = entry
		return nil
	}
}

// TestUpdateOnAMissingFileStartsFromTheEmptyConfig: the fresh snapshot is
// loaded with LoadClientConfigFromPath's own semantics, where an absent file is
// the empty config rather than an error — otherwise the very first write on a
// clean machine would fail.
func TestUpdateOnAMissingFileStartsFromTheEmptyConfig(t *testing.T) {
	dir := t.TempDir()
	// Two levels deep: the config directory does not exist yet either, which is
	// what a first `shed server add` on a bare home looks like.
	cfg, err := LoadClientConfigFromPath(filepath.Join(dir, ".shed", "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.Update(setEntry("a", ServerEntry{Host: "h"})); err != nil {
		t.Fatalf("Update on a missing file: %v", err)
	}
	if got := reloadFrom(t, cfg).Servers["a"].Host; got != "h" {
		t.Errorf("on-disk host = %q, want h", got)
	}
}

// TestUpdateMergesConcurrentDistinctEntries is the core regression: two writers
// touching DIFFERENT entries must both survive. Under the old
// mutate-then-Save() shape one of them would be erased by the other's stale
// whole-document snapshot.
func TestUpdateMergesConcurrentDistinctEntries(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")

	const writers = 8
	var wg sync.WaitGroup
	errs := make([]error, writers)
	for i := range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// Each writer holds its OWN *ClientConfig, loaded before any of the
			// others wrote — exactly the shape of N concurrent `shed`
			// processes, and the shape the in-process mutex alone cannot fix.
			cfg, err := LoadClientConfigFromPath(path)
			if err != nil {
				errs[i] = err
				return
			}
			name := string(rune('a' + i))
			errs[i] = cfg.Update(setEntry(name, ServerEntry{Host: name + ".example", SSHPort: 2222}))
		}()
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("writer %d: %v", i, err)
		}
	}

	loaded, err := LoadClientConfigFromPath(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.Servers) != writers {
		t.Fatalf("on-disk servers = %d (%v), want all %d — a writer clobbered another's entry",
			len(loaded.Servers), loaded.Servers, writers)
	}
	for i := range writers {
		name := string(rune('a' + i))
		if got := loaded.Servers[name].Host; got != name+".example" {
			t.Errorf("entry %q = %q, want %q", name, got, name+".example")
		}
	}
}

// TestConcurrentSaveToPathDoesNotCollideOnATempFile covers the second half of
// the fix. The old writer built its temp path as `<path>.tmp` — a name every
// writer picks — so two savers interleaved their bytes into ONE file and then
// each renamed it into place. The rename is atomic; the CONTENT was not.
//
// SaveToPath takes no lock (the lock lives in Update), so this drives it
// directly: the assertion is that the result always parses and is one writer's
// whole document, never a blend.
func TestConcurrentSaveToPathDoesNotCollideOnATempFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")

	// Documents of very different sizes: a short one written over a long one's
	// bytes leaves a syntactically valid-looking tail, which is exactly how a
	// shared temp file corrupts silently. The maps are shared (read-only from
	// here on) but every goroutine gets its OWN ClientConfig, since each writer
	// in the real world is a separate process.
	smallServers := map[string]ServerEntry{"s": {Host: "small"}}
	largeServers := map[string]ServerEntry{}
	for i := range 200 {
		largeServers[string(rune('A'+i%26))+string(rune('a'+i/26))] = ServerEntry{
			Host: "a-considerably-longer-hostname.example.invalid", SSHPort: 2222, ControlToken: "tok",
		}
	}

	var wg sync.WaitGroup
	for range 25 {
		for _, servers := range []map[string]ServerEntry{smallServers, largeServers} {
			wg.Add(1)
			go func() {
				defer wg.Done()
				c := &ClientConfig{Servers: servers, Sheds: map[string]ShedCache{}}
				if err := c.SaveToPath(path); err != nil {
					t.Errorf("SaveToPath: %v", err)
				}
			}()
		}
	}
	wg.Wait()

	loaded, err := LoadClientConfigFromPath(path)
	if err != nil {
		t.Fatalf("the saved config does not parse — two writers shared a temp file: %v", err)
	}
	if n := len(loaded.Servers); n != len(smallServers) && n != len(largeServers) {
		t.Errorf("servers = %d, want either %d or %d — the file is a blend of two documents",
			n, len(smallServers), len(largeServers))
	}

	// And nothing is left lying around under the old shared name.
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Errorf("the shared %q temp name is still in use (err=%v)", path+".tmp", err)
	}
}

// TestUpdateWaitsForAForeignLockHolder proves the lock is a REAL advisory file
// lock and not just an in-process mutex: the holder here is a second file
// descriptor flock'd by hand, which is what another `shed` process is.
func TestUpdateWaitsForAForeignLockHolder(t *testing.T) {
	cfg := newTestConfig(t)
	dir := filepath.Dir(cfg.path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}

	holder, err := os.OpenFile(filepath.Join(dir, clientConfigLockName), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Close()
	if err := syscall.Flock(int(holder.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	go func() { done <- cfg.Update(setEntry("a", ServerEntry{Host: "h"})) }()

	// It must NOT complete while the foreign holder has the lock.
	select {
	case err := <-done:
		t.Fatalf("Update completed while another process held the lock (err=%v)", err)
	case <-time.After(150 * time.Millisecond):
	}

	if err := syscall.Flock(int(holder.Fd()), syscall.LOCK_UN); err != nil {
		t.Fatal(err)
	}

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Update after the lock was released: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Update never proceeded after the lock was released")
	}
	if got := reloadFrom(t, cfg).Servers["a"].Host; got != "h" {
		t.Errorf("on-disk host = %q, want h", got)
	}
}

// TestUpdateSurvivesAStaleWholeSnapshotWriter documents the migration's whole
// point — and its limit.
//
// An UNMIGRATED writer (load early, mutate, SaveToPath) takes no lock and
// serializes its whole stale document, so it erases whatever landed in between.
// That is #299, reproduced. Once both writers use Update, the lock orders them
// and both mutations survive — which is why the fix had to reach every writer
// in cmd/shed, not just the credential path.
func TestUpdateSurvivesAStaleWholeSnapshotWriter(t *testing.T) {
	t.Run("an unlocked whole-snapshot save still clobbers", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "config.yaml")
		seed, err := LoadClientConfigFromPath(path)
		if err != nil {
			t.Fatal(err)
		}
		if err := seed.Update(setEntry("srv", ServerEntry{Host: "h", SSHPort: 2222})); err != nil {
			t.Fatal(err)
		}

		// The stale writer's view, taken BEFORE the credential lands.
		stale, err := LoadClientConfigFromPath(path)
		if err != nil {
			t.Fatal(err)
		}

		// A credential persist commits in between.
		if err := seed.Update(func(c *ClientConfig) error {
			e := c.Servers["srv"]
			e.ControlToken = "freshly-minted"
			c.Servers["srv"] = e
			return nil
		}); err != nil {
			t.Fatal(err)
		}

		// The stale writer saves its own whole document, as every pre-migration
		// call site did.
		stale.Sheds["myshed"] = ShedCache{Server: "srv", Status: "running"}
		if err := stale.SaveToPath(path); err != nil {
			t.Fatal(err)
		}

		if got := reloadFrom(t, seed).Servers["srv"].ControlToken; got != "" {
			t.Fatalf("control token = %q; this branch is meant to REPRODUCE the loss", got)
		}
	})

	t.Run("routing the same writer through Update preserves both", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "config.yaml")
		seed, err := LoadClientConfigFromPath(path)
		if err != nil {
			t.Fatal(err)
		}
		if err := seed.Update(setEntry("srv", ServerEntry{Host: "h", SSHPort: 2222})); err != nil {
			t.Fatal(err)
		}

		stale, err := LoadClientConfigFromPath(path)
		if err != nil {
			t.Fatal(err)
		}

		if err := seed.Update(func(c *ClientConfig) error {
			e := c.Servers["srv"]
			e.ControlToken = "freshly-minted"
			c.Servers["srv"] = e
			return nil
		}); err != nil {
			t.Fatal(err)
		}

		if err := stale.Update(func(c *ClientConfig) error {
			c.CacheShed("myshed", "srv", "running")
			return nil
		}); err != nil {
			t.Fatal(err)
		}

		loaded := reloadFrom(t, seed)
		if got := loaded.Servers["srv"].ControlToken; got != "freshly-minted" {
			t.Errorf("control token = %q, want freshly-minted (the stale writer erased it)", got)
		}
		if got := loaded.Sheds["myshed"].Server; got != "srv" {
			t.Errorf("shed cache = %q, want srv (the credential persist erased it)", got)
		}
	})
}

// TestUpdateWritesOnlyItsOwnMutation pins a deliberate behavior change: a
// writer no longer piggybacks whatever else this process happens to have dirtied
// in memory. Before, every Save() serialized the entire in-memory config, so a
// credential persist silently committed an unrelated half-finished cache update
// (and vice versa). Now each mutation stands alone.
func TestUpdateWritesOnlyItsOwnMutation(t *testing.T) {
	cfg := newTestConfig(t)
	if err := cfg.Update(setEntry("srv", ServerEntry{Host: "h", SSHPort: 2222})); err != nil {
		t.Fatal(err)
	}

	// In-memory dirt that no Update was ever asked to commit.
	cfg.Sheds["never-asked-for"] = ShedCache{Server: "srv", Status: "running"}
	cfg.DefaultServer = "not-committed"

	if err := cfg.Update(func(c *ClientConfig) error {
		e := c.Servers["srv"]
		e.ControlToken = "tok"
		c.Servers["srv"] = e
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	loaded := reloadFrom(t, cfg)
	if got := loaded.Servers["srv"].ControlToken; got != "tok" {
		t.Errorf("control token = %q, want tok", got)
	}
	if _, ok := loaded.Sheds["never-asked-for"]; ok {
		t.Error("an unrelated in-memory cache entry rode along into the file")
	}
	if loaded.DefaultServer != "" {
		t.Errorf("default_server = %q, want empty — an unrelated in-memory edit rode along", loaded.DefaultServer)
	}
	// The in-memory copy keeps its dirt; Update re-applies the mutation there,
	// it does not replace the object.
	if _, ok := cfg.Sheds["never-asked-for"]; !ok {
		t.Error("Update replaced the in-memory config wholesale instead of re-applying the mutation")
	}
}

// TestUpdateAbortsOnAFailedMutation: a mutation that says no leaves the file
// and the in-memory config exactly as they were.
func TestUpdateAbortsOnAFailedMutation(t *testing.T) {
	cfg := newTestConfig(t)
	if err := cfg.Update(setEntry("srv", ServerEntry{Host: "h"})); err != nil {
		t.Fatal(err)
	}

	boom := errors.New("injected")
	err := cfg.Update(func(c *ClientConfig) error {
		c.Servers["ghost"] = ServerEntry{Host: "should-not-survive"}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("Update err = %v, want the injected error", err)
	}
	if _, ok := cfg.Servers["ghost"]; ok {
		t.Error("the in-memory config was mutated by an aborted Update")
	}
	if _, ok := reloadFrom(t, cfg).Servers["ghost"]; ok {
		t.Error("the file was written by an aborted Update")
	}
}

// TestUpdateRefusesAPathlessConfig: a config with no path (a hand-built
// literal) has nothing to lock or merge against, so it must say so rather than
// write somewhere arbitrary.
func TestUpdateRefusesAPathlessConfig(t *testing.T) {
	cfg := &ClientConfig{Servers: map[string]ServerEntry{}, Sheds: map[string]ShedCache{}}
	if err := cfg.Update(func(*ClientConfig) error { return nil }); err == nil {
		t.Fatal("Update on a pathless config should fail")
	}
}

// TestConfigLockFileIsNeverRemoved: the lock file's inode has to outlive every
// holder — deleting it lets the next locker create a fresh inode and lock THAT,
// which is two "exclusive" holders. (Same rationale as sdk/creds' per-server
// locks.)
func TestConfigLockFileIsNeverRemoved(t *testing.T) {
	cfg := newTestConfig(t)
	if err := cfg.Update(setEntry("a", ServerEntry{Host: "h"})); err != nil {
		t.Fatal(err)
	}
	lockPath := filepath.Join(filepath.Dir(cfg.path), clientConfigLockName)
	if _, err := os.Stat(lockPath); err != nil {
		t.Fatalf("the config lock file should survive its holder: %v", err)
	}
}

// TestRemoveServerPromotesADeterministicDefault: RemoveServer used to promote
// "whatever the map yields first", which is randomized per iteration in Go.
// Inside an Update — applied once to the on-disk snapshot and once to the
// caller's — that hands the file and the running process different defaults.
func TestRemoveServerPromotesADeterministicDefault(t *testing.T) {
	for range 20 {
		cfg := &ClientConfig{
			Servers: map[string]ServerEntry{
				"zulu": {Host: "z"}, "alpha": {Host: "a"}, "mike": {Host: "m"},
			},
			Sheds:         map[string]ShedCache{},
			DefaultServer: "zulu",
		}
		if err := cfg.RemoveServer("zulu"); err != nil {
			t.Fatal(err)
		}
		if cfg.DefaultServer != "alpha" {
			t.Fatalf("promoted default = %q, want the alphabetically first remaining server", cfg.DefaultServer)
		}
	}
}

// TestUpdateAppliesACacheRowIdentically: the whole reason CacheShedAt exists.
// CacheShed's own time.Now() would stamp the two applications a few
// microseconds apart, so the file and the receiver would disagree on
// updated_at forever after.
func TestUpdateAppliesACacheRowIdentically(t *testing.T) {
	cfg := newTestConfig(t)
	at := time.Now()
	if err := cfg.Update(func(c *ClientConfig) error {
		c.CacheShedAt("myshed", "srv", "running", at)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	want := reloadFrom(t, cfg).Sheds["myshed"]
	got := cfg.Sheds["myshed"]
	if got.Server != want.Server || got.Status != want.Status || !got.UpdatedAt.Equal(want.UpdatedAt) {
		t.Errorf("in-memory row %+v differs from the on-disk row %+v", got, want)
	}
}

// reloadFrom reads the config file cfg points at, as a second process would.
func reloadFrom(t *testing.T, cfg *ClientConfig) *ClientConfig {
	t.Helper()
	loaded, err := LoadClientConfigFromPath(cfg.path)
	if err != nil {
		t.Fatal(err)
	}
	return loaded
}
