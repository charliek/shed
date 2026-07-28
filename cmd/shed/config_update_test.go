package main

import (
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/charliek/shed/internal/config"
	"github.com/charliek/shed/sdk"
)

// ---------------------------------------------------------------------------
// shed#299 — the CLI half.
//
// Every writer in this package now goes through updateClientConfig, which holds
// configMu in-process and ClientConfig.Update's file lock across processes. The
// credential persist adds one thing on top: it re-verifies, under that lock and
// against a FRESH read of config.yaml, that the name it is about to write still
// exists and still points at the endpoint the credential was minted for.
//
// These tests stand in for the second process by mutating the FILE behind the
// in-memory config's back — which is exactly what the in-memory config cannot
// see, and exactly what the old mutate-then-Save() path silently overwrote.
// ---------------------------------------------------------------------------

// srvEndpoint is the entry these tests treat as "the server this credential was
// minted against".
func srvEndpoint() config.ServerEntry {
	return config.ServerEntry{Host: "h.example", SSHPort: 2222, APIURL: "https://h.example:8443"}
}

// asAnotherProcess applies mutate to the config FILE through a second,
// independently-loaded *ClientConfig — the closest thing to a concurrent `shed`
// invocation without spawning one. The in-memory clientConfig is left stale on
// purpose.
func asAnotherProcess(t *testing.T, cfgPath string, mutate func(*config.ClientConfig)) {
	t.Helper()
	other, err := config.LoadClientConfigFromPath(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := other.Update(mutate); err != nil {
		t.Fatal(err)
	}
}

// onDisk reads config.yaml as a third party would.
func onDisk(t *testing.T, cfgPath string) *config.ClientConfig {
	t.Helper()
	loaded, err := config.LoadClientConfigFromPath(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	return loaded
}

// TestCredentialPersistAbortsWhenTheEntryWentAway: minting is an SSH
// round-trip, and a `shed server rm` in another process can land inside it.
// Writing anyway would resurrect the entry as a bare credential row for a
// server the user deleted.
func TestCredentialPersistAbortsWhenTheEntryWentAway(t *testing.T) {
	cfgPath := testClientConfig(t)
	putServerEntry(t, "srv", srvEndpoint())

	asAnotherProcess(t, cfgPath, func(c *config.ClientConfig) {
		delete(c.Servers, "srv")
	})

	err := saveServerEntry("srv", "refreshed token", srvEndpoint(), func(e *config.ServerEntry) {
		e.ControlToken = "resurrection"
	})
	if err == nil {
		t.Fatal("the persist should have aborted: the entry no longer exists on disk")
	}
	if _, ok := onDisk(t, cfgPath).Servers["srv"]; ok {
		t.Error("the removed entry was resurrected by the credential persist")
	}
	if got := clientConfig.Servers["srv"].ControlToken; got != "" {
		t.Errorf("in-memory token = %q, want untouched by an aborted persist", got)
	}
}

// TestCredentialPersistAbortsWhenTheEntryWasRepointed: same window, other
// outcome — `shed server rm` + `shed server add` reusing the name, or a
// hand-edited config. Writing anyway would file a private key issued by one
// server under a name that now means a different one.
func TestCredentialPersistAbortsWhenTheEntryWasRepointed(t *testing.T) {
	cfgPath := testClientConfig(t)
	putServerEntry(t, "srv", srvEndpoint())

	repointed := config.ServerEntry{Host: "somewhere-else.example", SSHPort: 2222, APIURL: "https://somewhere-else.example:8443"}
	asAnotherProcess(t, cfgPath, func(c *config.ClientConfig) {
		c.Servers["srv"] = repointed
	})

	err := saveServerEntry("srv", "client certificate", srvEndpoint(), func(e *config.ServerEntry) {
		e.AuthMode = config.AuthModeMTLS
		e.ClientCertFile = "/tmp/misbound.pem"
	})
	if err == nil {
		t.Fatal("the persist should have aborted: the entry names a different endpoint now")
	}
	stored := onDisk(t, cfgPath).Servers["srv"]
	if stored.Host != repointed.Host || stored.ClientCertFile != "" {
		t.Errorf("the repointed entry was overwritten by the persist: %+v", stored)
	}
}

// TestCredentialPersistDoesNotCommitUnrelatedInMemoryDirt pins the deliberate
// behavior change from the migration.
//
// The old path saved the WHOLE in-memory config, so a credential persist
// committed whatever else this process had dirtied — a half-finished shed-cache
// refresh, say — as a side effect. Now a persist writes the credential and
// nothing else; the cache is committed by its own (equally locked) update.
func TestCredentialPersistDoesNotCommitUnrelatedInMemoryDirt(t *testing.T) {
	cfgPath := testClientConfig(t)
	putServerEntry(t, "srv", srvEndpoint())

	// Dirt nobody asked to persist.
	clientConfig.Sheds["half-finished"] = config.ShedCache{Server: "srv", Status: "running"}

	if err := saveServerEntry("srv", "refreshed token", srvEndpoint(), func(e *config.ServerEntry) {
		e.ControlToken = "tok"
	}); err != nil {
		t.Fatalf("saveServerEntry: %v", err)
	}

	loaded := onDisk(t, cfgPath)
	if got := loaded.Servers["srv"].ControlToken; got != "tok" {
		t.Errorf("on-disk token = %q, want tok", got)
	}
	if _, ok := loaded.Sheds["half-finished"]; ok {
		t.Error("an unrelated in-memory cache entry rode along with the credential persist")
	}
}

// TestCredentialPersistMergesIntoAConcurrentWrite is #299 itself: a credential
// persist and another process's shed-cache refresh must BOTH survive. Under the
// old whole-snapshot save, whichever renamed last erased the other.
func TestCredentialPersistMergesIntoAConcurrentWrite(t *testing.T) {
	cfgPath := testClientConfig(t)
	putServerEntry(t, "srv", srvEndpoint())

	// Another process refreshes its shed cache after this process loaded.
	asAnotherProcess(t, cfgPath, func(c *config.ClientConfig) {
		c.CacheShed("myshed", "srv", "running")
	})

	if err := saveServerEntry("srv", "refreshed token", srvEndpoint(), func(e *config.ServerEntry) {
		e.ControlToken = "freshly-minted"
	}); err != nil {
		t.Fatalf("saveServerEntry: %v", err)
	}

	loaded := onDisk(t, cfgPath)
	if got := loaded.Servers["srv"].ControlToken; got != "freshly-minted" {
		t.Errorf("on-disk token = %q, want freshly-minted", got)
	}
	if got := loaded.Sheds["myshed"].Server; got != "srv" {
		t.Errorf("the other process's shed cache was clobbered: %+v", loaded.Sheds)
	}
}

// TestConcurrentClientConfigWritesAllSurvive drives the two write paths that
// actually run at the same time in this package — the `--all` fan-out's
// credential persists and the shed-cache updates — through goroutines. Every
// mutation must be on disk at the end, and the run must be clean under -race
// (configMu against the map reads, the file lock against the saves).
func TestConcurrentClientConfigWritesAllSurvive(t *testing.T) {
	cfgPath := testClientConfig(t)
	names := []string{"s1", "s2", "s3", "s4", "s5", "s6"}
	for _, n := range names {
		putServerEntry(t, n, config.ServerEntry{Host: n + ".example", SSHPort: 2222, APIURL: "https://" + n + ".example:8443"})
	}

	var wg sync.WaitGroup
	for _, n := range names {
		wg.Add(1)
		go func() {
			defer wg.Done()
			endpoint := config.ServerEntry{Host: n + ".example", SSHPort: 2222, APIURL: "https://" + n + ".example:8443"}
			if err := saveServerEntry(n, "refreshed token", endpoint, func(e *config.ServerEntry) {
				e.ControlToken = "tok-" + n
			}); err != nil {
				t.Errorf("saveServerEntry(%s): %v", n, err)
			}
		}()
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := cacheShedLocation("shed-"+n, n, "running"); err != nil {
				t.Errorf("cacheShedLocation(%s): %v", n, err)
			}
		}()
	}
	wg.Wait()

	loaded := onDisk(t, cfgPath)
	for _, n := range names {
		if got := loaded.Servers[n].ControlToken; got != "tok-"+n {
			t.Errorf("server %s token = %q, want tok-%s", n, got, n)
		}
		if got := loaded.Sheds["shed-"+n].Server; got != n {
			t.Errorf("shed-%s cached server = %q, want %s", n, got, n)
		}
	}
}

// TestTokenMTLSFlipLeavesConfigAndFilesConsistent walks a server through both
// halves of a mode flip and asserts the invariant that matters after each one:
// config.yaml never names credential material that is not on disk.
//
// The two persists take the server's credential lock BEFORE the config lock —
// the pinned ordering — which is what stops the file deletion in one from
// landing between the file write and the config update in the other.
func TestTokenMTLSFlipLeavesConfigAndFilesConsistent(t *testing.T) {
	cfgPath := testClientConfig(t)
	endpoint := srvEndpoint()
	putServerEntry(t, "srv", endpoint)

	issuer := newMTLSCredentialIssuer(t)

	// token → mtls: the certificate is written, then the entry starts naming it.
	persistMTLSCredential("srv", endpoint, issuer.credential(t, "cli-key", farFromExpiry))
	stored := onDisk(t, cfgPath).Servers["srv"]
	if stored.AuthMode != config.AuthModeMTLS || stored.ClientCertFile == "" || stored.ClientKeyFile == "" {
		t.Fatalf("entry did not move to mtls: %+v", stored)
	}
	for _, p := range []string{stored.ClientCertFile, stored.ClientKeyFile} {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("config names %s but it is not on disk: %v", p, err)
		}
	}

	// mtls → token: the entry stops naming the pair BEFORE the pair is deleted.
	persistTokenCredential("srv", endpoint, "fresh-token", time.Now().Add(24*time.Hour))
	stored = onDisk(t, cfgPath).Servers["srv"]
	if stored.AuthMode != config.AuthModeToken || stored.ControlToken != "fresh-token" {
		t.Fatalf("entry did not move back to token: %+v", stored)
	}
	if stored.ClientCertFile != "" || stored.ClientKeyFile != "" {
		t.Errorf("the entry still names certificate files after the flip: %+v", stored)
	}
	if _, err := os.Stat(config.ServerCredsDir("srv")); !os.IsNotExist(err) {
		t.Errorf("the credential directory survived the flip back to token mode (err=%v)", err)
	}
}

// TestMTLSPersistSkipsWhenItsMaterialWentAway: between WriteClientCredentials
// (which takes and releases the store lock internally) and the config update, a
// concurrent flip back to token mode can delete the pair. Recording the paths
// anyway would leave config.yaml pointing at nothing — the one state the client
// cannot recover from on its own.
func TestMTLSPersistSkipsWhenItsMaterialWentAway(t *testing.T) {
	cfgPath := testClientConfig(t)
	endpoint := srvEndpoint()
	putServerEntry(t, "srv", endpoint)

	cred := newMTLSCredentialIssuer(t).credential(t, "cli-key", farFromExpiry)
	if _, _, err := config.WriteClientCredentials("srv", []byte(cred.Bundle.ClientCert), cred.KeyPEM); err != nil {
		t.Fatal(err)
	}
	// Stand in for the concurrent token-mode persist that removed the pair
	// after this one wrote it.
	if err := config.RemoveServerCredentials("srv"); err != nil {
		t.Fatal(err)
	}
	certPath, keyPath := config.ClientCredentialPaths("srv")
	if err := os.MkdirAll(config.ServerCredsDir("srv"), 0700); err != nil {
		t.Fatal(err)
	}
	// Only the key survives: a pair the entry must never be pointed at.
	if err := os.WriteFile(keyPath, cred.KeyPEM, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(certPath); !os.IsNotExist(err) {
		t.Fatal("test setup: the certificate should be gone")
	}

	if err := saveServerEntryIfMaterialPresent(t, "srv", endpoint, certPath, keyPath); err == nil {
		t.Fatal("the persist should have refused to name a certificate that is not there")
	}
	if got := onDisk(t, cfgPath).Servers["srv"].ClientCertFile; got != "" {
		t.Errorf("config now names a missing certificate: %q", got)
	}
}

// TestUpdateClosuresAreDeterministic is the standing guard on the primitive's
// one real requirement: Update applies its mutation TWICE — to the fresh
// on-disk snapshot, then to the receiver — so a closure that is not a
// deterministic function of the config it is handed leaves the file and the
// running process holding different rows.
//
// The two ways to get that wrong are both in this package's history:
// ClientConfig.CacheShed stamps its own time.Now() (two calls, two instants),
// and ClientConfig.AddServer both stamps AddedAt and can return an error — an
// error on the SECOND application would report a failed add for a row already
// durably on disk. Byte-equality of the marshalled config against the file is
// the check that catches either.
func TestUpdateClosuresAreDeterministic(t *testing.T) {
	cfgPath := testClientConfig(t)

	t.Run("server add", func(t *testing.T) {
		if err := saveAddedServer("added", config.ServerEntry{Host: "h.example", SSHPort: 2222}); err != nil {
			t.Fatalf("saveAddedServer: %v", err)
		}
		if clientConfig.Servers["added"].AddedAt.IsZero() {
			t.Error("added_at was never stamped")
		}
		assertConfigMatchesDisk(t, cfgPath)
	})

	t.Run("a duplicate name is refused before anything is written", func(t *testing.T) {
		before := clientConfig.Servers["added"]
		if err := saveAddedServer("added", config.ServerEntry{Host: "other.example", SSHPort: 2222}); err == nil {
			t.Fatal("adding a duplicate name should fail")
		}
		if got := clientConfig.Servers["added"]; got.Host != before.Host || !got.AddedAt.Equal(before.AddedAt) {
			t.Errorf("the existing entry was disturbed by the refused add: %+v", got)
		}
		assertConfigMatchesDisk(t, cfgPath)
	})

	t.Run("shed cache", func(t *testing.T) {
		if err := cacheShedLocation("myshed", "added", "running"); err != nil {
			t.Fatalf("cacheShedLocation: %v", err)
		}
		if clientConfig.Sheds["myshed"].UpdatedAt.IsZero() {
			t.Error("updated_at was never stamped")
		}
		assertConfigMatchesDisk(t, cfgPath)
	})

	t.Run("credential persist", func(t *testing.T) {
		endpoint := config.ServerEntry{Host: "h.example", SSHPort: 2222}
		if err := saveServerEntry("added", "refreshed token", endpoint, func(e *config.ServerEntry) {
			e.ControlToken = "tok"
		}); err != nil {
			t.Fatalf("saveServerEntry: %v", err)
		}
		assertConfigMatchesDisk(t, cfgPath)
	})

	t.Run("set default", func(t *testing.T) {
		if err := updateClientConfig(func(c *config.ClientConfig) {
			c.DefaultServer = "added"
		}); err != nil {
			t.Fatal(err)
		}
		assertConfigMatchesDisk(t, cfgPath)
	})
}

// assertConfigMatchesDisk marshals the in-memory config and compares it byte
// for byte with the file — which IS the fresh snapshot Update saved, mutated by
// the same closure. Any difference is the closure disagreeing with itself.
func assertConfigMatchesDisk(t *testing.T, cfgPath string) {
	t.Helper()
	inMemory, err := yaml.Marshal(clientConfig)
	if err != nil {
		t.Fatal(err)
	}
	stored, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(inMemory) != string(stored) {
		t.Errorf("the mutation was not deterministic — the file and this process diverged.\n--- on disk ---\n%s\n--- in memory ---\n%s", stored, inMemory)
	}
}

// ---------------------------------------------------------------------------
// Preconditions must be tested against the config as it is ON DISK, under the
// lock — not against this process's view of it before the lock is taken.
//
// A check performed before the update reads a snapshot another `shed` can
// invalidate in the interval, which is how two adds of one name both see "free"
// and how a set-default outlives the removal of its target. UpdateChecked moves
// the question inside the locked section; these tests are what say so.
// ---------------------------------------------------------------------------

// TestConcurrentSameNameAddsLeaveExactlyOneWinner. The duplicate check used to
// run against clientConfig before the lock, so every racer saw an empty config
// and every racer "succeeded" — the last rename deciding, silently, which
// server the name actually meant.
func TestConcurrentSameNameAddsLeaveExactlyOneWinner(t *testing.T) {
	cfgPath := testClientConfig(t)

	const racers = 8
	var wg sync.WaitGroup
	errs := make([]error, racers)
	for i := range racers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = saveAddedServer("srv", config.ServerEntry{
				Host: fmt.Sprintf("h%d.example", i), SSHPort: 2222,
			})
		}()
	}
	wg.Wait()

	winners := 0
	for i, err := range errs {
		switch {
		case err == nil:
			winners++
		case strings.Contains(err.Error(), "already exists"):
		default:
			t.Errorf("racer %d: unexpected error %v", i, err)
		}
	}
	if winners != 1 {
		t.Fatalf("%d of %d concurrent adds of one name succeeded, want exactly 1", winners, racers)
	}

	stored := onDisk(t, cfgPath).Servers["srv"]
	if stored.Host == "" || stored.AddedAt.IsZero() {
		t.Fatalf("the surviving row is not a complete entry: %+v", stored)
	}
	if got := clientConfig.Servers["srv"]; got.Host != stored.Host {
		t.Errorf("in-memory host %q disagrees with the file's %q", got.Host, stored.Host)
	}
	assertConfigMatchesDisk(t, cfgPath)
}

// TestSaveUpdatedServerDoesNotResurrectARemovedEntry: a --refetch is an SSH
// round-trip, and a `shed server rm` can land inside it. The pre-lock check
// could not see that; the under-lock one can.
func TestSaveUpdatedServerDoesNotResurrectARemovedEntry(t *testing.T) {
	was := srvEndpoint()

	t.Run("removed", func(t *testing.T) {
		cfgPath := testClientConfig(t)
		putServerEntry(t, "srv", was)
		asAnotherProcess(t, cfgPath, func(c *config.ClientConfig) { delete(c.Servers, "srv") })

		updated := was
		updated.TLSCertFingerprint = "sha256:new"
		if err := saveUpdatedServer("srv", was, updated); err == nil {
			t.Fatal("updating a concurrently-removed entry should fail")
		}
		if _, ok := onDisk(t, cfgPath).Servers["srv"]; ok {
			t.Error("the removed entry was resurrected by the update")
		}
	})

	t.Run("repointed", func(t *testing.T) {
		cfgPath := testClientConfig(t)
		putServerEntry(t, "srv", was)
		elsewhere := config.ServerEntry{Host: "elsewhere.example", SSHPort: 2222, APIURL: "https://elsewhere.example:8443"}
		asAnotherProcess(t, cfgPath, func(c *config.ClientConfig) { c.Servers["srv"] = elsewhere })

		updated := was
		updated.TLSCertFingerprint = "sha256:new"
		if err := saveUpdatedServer("srv", was, updated); err == nil {
			t.Fatal("updating a concurrently-repointed entry should fail")
		}
		if got := onDisk(t, cfgPath).Servers["srv"]; got.Host != elsewhere.Host || got.TLSCertFingerprint != "" {
			t.Errorf("the re-added entry was overwritten: %+v", got)
		}
	})
}

// TestSetDefaultRefusesAConcurrentlyRemovedServer: default_server naming a row
// nobody has makes every later command fail on a default the user never chose.
func TestSetDefaultRefusesAConcurrentlyRemovedServer(t *testing.T) {
	cfgPath := testClientConfig(t)
	putServerEntry(t, "keep", config.ServerEntry{Host: "keep.example", SSHPort: 2222})
	putServerEntry(t, "doomed", config.ServerEntry{Host: "doomed.example", SSHPort: 2222})

	asAnotherProcess(t, cfgPath, func(c *config.ClientConfig) { delete(c.Servers, "doomed") })

	if err := runServerSetDefault(nil, []string{"doomed"}); err == nil {
		t.Fatal("setting the default to a concurrently-removed server should fail")
	}
	loaded := onDisk(t, cfgPath)
	if loaded.DefaultServer == "doomed" {
		t.Error("default_server was left naming a server that no longer exists")
	}
	if _, _, err := loaded.GetDefaultServer(); loaded.DefaultServer != "" && err != nil {
		t.Errorf("default_server %q does not resolve: %v", loaded.DefaultServer, err)
	}
}

// TestCommittedMaterialOwnership covers the guard that replaces "hold one lock
// across the commit and the save" on the mtls path, where holding one lock is
// impossible: StagedClientCredentials.Commit takes the per-server credential
// lock itself, and flock is not re-entrant.
//
// Instead the transaction commits, THEN locks, then proves the pair under the
// entry's paths is still the one it committed. A loser of that race abandons
// its add rather than saving a config row that describes somebody else's
// certificate.
func TestCommittedMaterialOwnership(t *testing.T) {
	newTxn := func(t *testing.T) (*credentialTxn, sdk.Credential) {
		t.Helper()
		testClientConfig(t)
		cred := newMTLSCredentialIssuer(t).credential(t, "cli-key", farFromExpiry)
		entry := config.ServerEntry{Host: "h.example", SSHPort: 2222}
		txn, err := stageCredentials(&entry, "srv", cred, nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := txn.commit(); err != nil {
			t.Fatal(err)
		}
		return txn, cred
	}

	t.Run("the material this transaction committed is accepted", func(t *testing.T) {
		txn, _ := newTxn(t)
		if err := txn.committedMaterialIsOurs(); err != nil {
			t.Errorf("own material rejected: %v", err)
		}
	})

	t.Run("another enrollment's material is refused", func(t *testing.T) {
		txn, _ := newTxn(t)
		other := newMTLSCredentialIssuer(t).credential(t, "cli-key", farFromExpiry)
		certPath, _ := txn.staged.Paths()
		if err := os.WriteFile(certPath, []byte(other.Bundle.ClientCert), 0600); err != nil {
			t.Fatal(err)
		}
		if err := txn.committedMaterialIsOurs(); err == nil {
			t.Error("a certificate from another enrollment was accepted as ours")
		}
	})

	t.Run("material that went away is refused", func(t *testing.T) {
		txn, _ := newTxn(t)
		if err := config.RemoveServerCredentials("srv"); err != nil {
			t.Fatal(err)
		}
		if err := txn.committedMaterialIsOurs(); err == nil {
			t.Error("a missing certificate was accepted as ours")
		}
	})
}

// saveServerEntryIfMaterialPresent mirrors persistMTLSCredential's guard so the
// check can be exercised without racing the real write. It returns an error
// when the material is gone, which is what the real path turns into a warning +
// an early return.
func saveServerEntryIfMaterialPresent(t *testing.T, name string, endpoint config.ServerEntry, certPath, keyPath string) error {
	t.Helper()
	unlock, err := config.LockServerCredentials(name)
	if err != nil {
		return err
	}
	defer unlock()
	for _, p := range []string{certPath, keyPath} {
		if _, err := os.Stat(p); err != nil {
			return err
		}
	}
	return saveServerEntry(name, "client certificate", endpoint, func(e *config.ServerEntry) {
		e.AuthMode = config.AuthModeMTLS
		e.ClientCertFile, e.ClientKeyFile = certPath, keyPath
	})
}
