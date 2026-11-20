package main

import (
	"context"
	"errors"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/charliek/shed/internal/clienttoken"
	"github.com/charliek/shed/internal/config"
	"github.com/charliek/shed/internal/roostprovider"
	"github.com/charliek/shed/sdk"
)

// ---------------------------------------------------------------------------
// The write transaction fails closed (plan 037 §3.1, the review of A5).
//
// The certificate pair and config.yaml are two files with no journal between
// them. A kill between the file commit and the config save leaves the new pair
// beside the old row. These tests stop a transaction exactly there and prove
// the next process never uses that state to send a request as a user the row
// does not record, adopts nothing silently, and recovers with the documented
// command.
// ---------------------------------------------------------------------------

// killed is the panic value that stands in for the process being killed.
type killed struct{}

// killAfterNextCommit makes the next lock-held write or staged commit of the
// credential files complete and then stop the transaction dead: a panic the
// test recovers (runUntilKilled), standing in for a kill. Nothing after the
// file commit runs — no config save, no restoration — except deferred unlocks,
// as a dying process's flocks are released. revive puts the real writers back
// for the next process.
func killAfterNextCommit(t *testing.T) (revive func()) {
	t.Helper()
	origWrite, origCommit := writeCredentialsHeldFn, commitStagedHeldFn
	revive = func() { writeCredentialsHeldFn, commitStagedHeldFn = origWrite, origCommit }
	t.Cleanup(revive)
	writeCredentialsHeldFn = func(n string, certPEM, keyPEM []byte) (string, string, error) {
		certPath, keyPath, err := origWrite(n, certPEM, keyPEM)
		if err == nil {
			panic(killed{})
		}
		return certPath, keyPath, err
	}
	commitStagedHeldFn = func(s *config.StagedClientCredentials) error {
		if err := origCommit(s); err != nil {
			return err
		}
		panic(killed{})
	}
	return revive
}

// runUntilKilled runs f and requires it to be stopped by killAfterNextCommit.
func runUntilKilled(t *testing.T, f func()) {
	t.Helper()
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("the transaction was never killed: it did not reach its file commit")
		}
		if _, ok := r.(killed); !ok {
			panic(r)
		}
	}()
	f()
}

// nextProcess reloads the config from disk, as the next command would.
func nextProcess(t *testing.T, cfgPath string) {
	t.Helper()
	reloaded, err := config.LoadClientConfigFromPath(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	clientConfig = reloaded
}

// addedMTLSServerAs is addedMTLSServer for a given user ("" for the implicit
// user of a server with no user list).
func addedMTLSServerAs(t *testing.T, m *mtlsServer, user string) string {
	t.Helper()
	host, port := hostPort(t, m.srv.URL)
	hostKey, _ := testHostKey(t)
	stubHostKeyScan(t, hostKey, nil)
	cred := m.credentialFor(t, "cli-key", user, farFromExpiry)
	cred.Bundle.HTTPSPort = port
	stubBootstrapResult(cred, nil)
	if err := runServerAdd(nil, []string{host}); err != nil {
		t.Fatalf("add: %v", err)
	}
	return config.GetClientConfigPath()
}

// assertRefusedSendingNothing: the entry is refused for its stored certificate
// by every reader — the CLI and the roost provider's inventory — with the
// message that names an interrupted enrolment and the refetch, and nothing
// runs ssh or connects to the server.
func assertRefusedSendingNothing(t *testing.T, m *mtlsServer, name string) {
	t.Helper()
	mints := stubBootstrap(func() sdk.Credential {
		t.Error("a refused entry ran ssh")
		return sdk.Credential{}
	})
	accepted := m.accepted.Load()

	e := clientConfig.Servers[name]
	_, err := NewAPIClientFromNamedEntry(name, &e, DefaultTimeout).GetInfo()
	asRefusal(t, err, config.RefusedStoredUserMismatch)
	for _, want := range []string{"an interrupted or concurrent enrolment", "shed server update " + name + " --refetch"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("message lacks %q:\n%s", want, err)
		}
	}
	inv := roostprovider.Inventory(context.Background(), clientConfig.Servers, 0)
	if _, ok := config.AsEntryRefused(inv.Refused[name]); !ok || len(inv.Answered) != 0 {
		t.Errorf("the roost inventory used the entry: %#v", inv)
	}
	if got := mints.Load(); got != 0 {
		t.Errorf("bootstrap calls = %d, want 0", got)
	}
	if got := m.accepted.Load() - accepted; got != 0 {
		t.Errorf("the server accepted %d connection(s), want 0", got)
	}
}

// TestKilledRefreshFailsClosed: the first named user over an implicit pair. A
// re-enrolment of an entry that records no user (its server had no user list)
// is issued pat's certificate and is killed after the files are written and
// before the row records pat. The next process finds pat's pair beside a row
// that records no user: it is refused, sending nothing, and a refetch
// recovers.
func TestKilledRefreshFailsClosed(t *testing.T) {
	testClientConfig(t)
	withCleanAddFlags(t)
	serverAddTrustTOFU = true
	m := newMTLSServer(t)
	cfgPath := addedMTLSServerAs(t, m, "")
	if got := clientConfig.Servers["srv"].User; got != "" {
		t.Fatalf("test setup: an implicit add recorded user %q", got)
	}

	revive := killAfterNextCommit(t)
	stubBootstrap(func() sdk.Credential { return m.credential(t, "cli-key", farFromExpiry) })
	refresh := controlCredentialRefresh(clientConfig.Servers["srv"], "srv", "srv")
	runUntilKilled(t, func() { _, _ = refresh() })
	revive()

	nextProcess(t, cfgPath)
	if got := clientConfig.Servers["srv"].User; got != "" {
		t.Fatalf("test setup: the killed refresh saved its row (user %q)", got)
	}
	assertRefusedSendingNothing(t, m, "srv")

	serverUpdateRefetch = true
	serverUpdateTrustTOFU = true
	stubBootstrapResult(mtlsCredentialFor(t, m), nil)
	var err error
	out := captureStdout(t, func() { err = runServerUpdate(nil, []string{"srv"}) })
	if err != nil {
		t.Fatalf("refetch: %v", err)
	}
	if !strings.Contains(out, "  User: pat\n") {
		t.Errorf("the refetch did not print the user:\n%s", out)
	}
	e := clientConfig.Servers["srv"]
	if _, err := NewAPIClientFromNamedEntry("srv", &e, DefaultTimeout).GetInfo(); err != nil {
		t.Fatalf("the recovered entry does not work: %v", err)
	}
}

// TestKilledUserChangeFailsClosed: `--refetch --allow-user-change` from pat to
// admin, killed after admin's pair is committed and before the row records
// admin. The next process finds admin's pair beside a row that records pat:
// refused, sending nothing; the same refetch run again recovers.
func TestKilledUserChangeFailsClosed(t *testing.T) {
	testClientConfig(t)
	withCleanAddFlags(t)
	serverAddTrustTOFU = true
	m := newMTLSServer(t)
	cfgPath := addedMTLSServerAs(t, m, testUser)

	serverUpdateRefetch = true
	serverUpdateTrustTOFU = true
	serverUpdateAllowUserChange = true
	admin := m.credentialFor(t, "admin-key", "admin", farFromExpiry)
	_, port := hostPort(t, m.srv.URL)
	admin.Bundle.HTTPSPort = port
	stubBootstrapResult(admin, nil)

	revive := killAfterNextCommit(t)
	runUntilKilled(t, func() { _ = runServerUpdate(nil, []string{"srv"}) })
	revive()

	nextProcess(t, cfgPath)
	if got := clientConfig.Servers["srv"].User; got != testUser {
		t.Fatalf("test setup: the killed refetch saved its row (user %q)", got)
	}
	assertRefusedSendingNothing(t, m, "srv")

	stubBootstrapResult(admin, nil)
	if err := runServerUpdate(nil, []string{"srv"}); err != nil {
		t.Fatalf("refetch --allow-user-change again: %v", err)
	}
	if got := onDisk(t, cfgPath).Servers["srv"].User; got != "admin" {
		t.Errorf("user = %q after recovery, want admin", got)
	}
	e := clientConfig.Servers["srv"]
	if _, err := NewAPIClientFromNamedEntry("srv", &e, DefaultTimeout).GetInfo(); err != nil {
		t.Fatalf("the recovered entry does not work: %v", err)
	}
}

// TestKilledAddFailsClosed: a fresh add killed after its pair is committed and
// before its row is saved leaves files that no row names. The next process has
// no entry: nothing reads them, nothing is sent. A second add replaces them.
func TestKilledAddFailsClosed(t *testing.T) {
	testClientConfig(t)
	withCleanAddFlags(t)
	serverAddTrustTOFU = true
	m := newMTLSServer(t)
	host, port := hostPort(t, m.srv.URL)
	hostKey, _ := testHostKey(t)
	stubHostKeyScan(t, hostKey, nil)
	first := m.credential(t, "cli-key", farFromExpiry)
	first.Bundle.HTTPSPort = port
	stubBootstrapResult(first, nil)

	revive := killAfterNextCommit(t)
	runUntilKilled(t, func() { _ = runServerAdd(nil, []string{host}) })
	revive()

	cfgPath := config.GetClientConfigPath()
	nextProcess(t, cfgPath)
	if _, ok := clientConfig.Servers["srv"]; ok {
		t.Fatal("test setup: the killed add saved its row")
	}
	certPath, _ := config.ClientCredentialPaths("srv")
	if got, _ := os.ReadFile(certPath); string(got) != first.Bundle.ClientCert {
		t.Fatal("test setup: the killed add did not leave its pair behind")
	}
	accepted := m.accepted.Load()
	withServerFlag(t, "srv")
	if _, _, err := findShedServer("box"); err == nil {
		t.Error("a command found an entry the killed add never saved")
	}
	if inv := roostprovider.Inventory(context.Background(), clientConfig.Servers, 0); len(inv.Tried) != 0 {
		t.Errorf("the roost inventory tried %v", inv.Tried)
	}
	if got := m.accepted.Load() - accepted; got != 0 {
		t.Errorf("the server accepted %d connection(s), want 0", got)
	}

	second := m.credential(t, "cli-key", farFromExpiry)
	second.Bundle.HTTPSPort = port
	stubBootstrapResult(second, nil)
	if err := runServerAdd(nil, []string{host}); err != nil {
		t.Fatalf("the second add: %v", err)
	}
	if got, _ := os.ReadFile(certPath); string(got) != second.Bundle.ClientCert {
		t.Error("the second add did not replace the orphaned pair")
	}
	e := clientConfig.Servers["srv"]
	if _, err := NewAPIClientFromNamedEntry("srv", &e, DefaultTimeout).GetInfo(); err != nil {
		t.Fatalf("the added entry does not work: %v", err)
	}
}

// ---------------------------------------------------------------------------
// a failed restoration is an error, and an unreadable snapshot writes nothing
// ---------------------------------------------------------------------------

// failWritesAfter lets the first n lock-held writes through and fails every
// later one (a restoration, when n counts the transaction's own write). A
// restoration writes through putBackCredentialFileFn, one file at a time; it
// counts as a write here too.
func failWritesAfter(t *testing.T, n int32) {
	t.Helper()
	orig, origPutBack := writeCredentialsHeldFn, putBackCredentialFileFn
	t.Cleanup(func() { writeCredentialsHeldFn, putBackCredentialFileFn = orig, origPutBack })
	var calls atomic.Int32
	writeCredentialsHeldFn = func(name string, certPEM, keyPEM []byte) (string, string, error) {
		if calls.Add(1) > n {
			return "", "", errors.New("injected: no space left on device")
		}
		return orig(name, certPEM, keyPEM)
	}
	putBackCredentialFileFn = func(path string, data []byte) error {
		if calls.Add(1) > n {
			return errors.New("injected: no space left on device")
		}
		return origPutBack(path, data)
	}
}

// assertRestoreFailed: the command failed with the restoration error, which
// says the files and the recorded user may disagree and names the refetch.
func assertRestoreFailed(t *testing.T, err error) {
	t.Helper()
	asRefusal(t, err, config.RefusedRestoreFailed)
	for _, want := range []string{"could not be put back", "may now disagree", "shed server update srv --refetch", "no space left on device"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("message lacks %q:\n%s", want, err)
		}
	}
}

// TestFailedRestorationFailsTheRefresh: a re-enrolment whose config save fails
// puts the previous pair back; when that fails too, the files and the row may
// disagree, so the command fails with that error (and its client is ended:
// nothing is sent), never a warning on a command that otherwise succeeds.
func TestFailedRestorationFailsTheRefresh(t *testing.T) {
	testClientConfig(t)
	m := newMTLSServer(t)
	entry := mtlsEntry(t, m, "srv", m.credential(t, "cli-key", farFromExpiry))
	entry.ClientCertExpiresAt = time.Now().Add(-time.Hour) // re-enrols at build
	putServerEntry(t, "srv", entry)
	breakConfigSave(t)
	failWritesAfter(t, 1)
	stubBootstrap(func() sdk.Credential { return m.credential(t, "cli-key", farFromExpiry) })

	e := clientConfig.Servers["srv"]
	_, err := NewAPIClientFromNamedEntry("srv", &e, DefaultTimeout).GetInfo()
	assertRestoreFailed(t, err)
	if got := m.accepted.Load(); got != 0 {
		t.Errorf("the server accepted %d connection(s), want 0", got)
	}
}

// TestFailedRestorationFailsTheRefetch: the same for --refetch.
func TestFailedRestorationFailsTheRefetch(t *testing.T) {
	testClientConfig(t)
	withCleanAddFlags(t)
	serverAddTrustTOFU = true
	m := newMTLSServer(t)
	addedMTLSServer(t, m)
	serverUpdateRefetch = true
	serverUpdateTrustTOFU = true
	stubBootstrapResult(mtlsCredentialFor(t, m), nil)
	breakConfigSave(t)
	failWritesAfter(t, 0)

	assertRestoreFailed(t, runServerUpdate(nil, []string{"srv"}))
}

// TestFailedRestorationFailsTheAdd: a fresh add whose save fails removes the
// pair it committed; when that fails, the add fails saying so.
func TestFailedRestorationFailsTheAdd(t *testing.T) {
	testClientConfig(t)
	withCleanAddFlags(t)
	serverAddTrustTOFU = true
	m := newMTLSServer(t)
	host, _ := hostPort(t, m.srv.URL)
	hostKey, _ := testHostKey(t)
	stubHostKeyScan(t, hostKey, nil)
	stubBootstrapResult(mtlsCredentialFor(t, m), nil)
	breakConfigSave(t)
	orig := removeCredentialsHeldFn
	t.Cleanup(func() { removeCredentialsHeldFn = orig })
	removeCredentialsHeldFn = func(string) error { return errors.New("injected: read-only file system") }

	err := runServerAdd(nil, []string{host})
	if err == nil || !strings.Contains(err.Error(), "could not put back the client credential files") ||
		!strings.Contains(err.Error(), "read-only file system") {
		t.Fatalf("err = %v, want the restoration failure", err)
	}
}

// failSnapshotReads makes reading the current pair before a write fail with a
// permission error (not "does not exist").
func failSnapshotReads(t *testing.T) {
	t.Helper()
	orig := readCredentialFileFn
	t.Cleanup(func() { readCredentialFileFn = orig })
	readCredentialFileFn = func(string) ([]byte, error) { return nil, fs.ErrPermission }
}

// TestUnreadableSnapshotWritesNothing: a transaction that cannot read the pair
// it may have to put back writes nothing at all. A --refetch fails; a
// re-enrolment that renews the user the row already records uses its new
// certificate in memory only, as for any other failed write of a renewal (one
// that would record a first user fails instead:
// TestUnrecordedFirstUserOnEveryWriteFailure).
func TestUnreadableSnapshotWritesNothing(t *testing.T) {
	t.Run("refetch", func(t *testing.T) {
		testClientConfig(t)
		withCleanAddFlags(t)
		serverAddTrustTOFU = true
		m := newMTLSServer(t)
		cfgPath, _, _ := addedMTLSServer(t, m)
		certPath, keyPath := config.ClientCredentialPaths("srv")
		before := snapshotFiles(t, cfgPath, certPath, keyPath)
		serverUpdateRefetch = true
		serverUpdateTrustTOFU = true
		stubBootstrapResult(mtlsCredentialFor(t, m), nil)
		failSnapshotReads(t)
		freezeCreds(t, "srv")

		err := runServerUpdate(nil, []string{"srv"})
		if err == nil || !errors.Is(err, fs.ErrPermission) {
			t.Fatalf("err = %v, want the snapshot read failure", err)
		}
		before.assertUnchanged(t)
	})

	t.Run("refresh", func(t *testing.T) {
		cfgPath := testClientConfig(t)
		m := newMTLSServer(t)
		entry := mtlsEntry(t, m, "srv", m.credential(t, "cli-key", farFromExpiry))
		entry.ClientCertExpiresAt = time.Now().Add(-time.Hour)
		putServerEntry(t, "srv", entry)
		certPath, keyPath := config.ClientCredentialPaths("srv")
		before := snapshotFiles(t, cfgPath, certPath, keyPath)
		issued := m.credential(t, "cli-key", farFromExpiry)
		stubBootstrap(func() sdk.Credential { return issued })
		failSnapshotReads(t)
		freezeCreds(t, "srv")

		e := clientConfig.Servers["srv"]
		if _, err := NewAPIClientFromNamedEntry("srv", &e, DefaultTimeout).GetInfo(); err != nil {
			t.Fatalf("GetInfo: %v", err)
		}
		if got := m.lastSerial.Load().(string); got != issued.Bundle.CertSerial {
			t.Errorf("presented %s, want the new certificate (in memory) %s", got, issued.Bundle.CertSerial)
		}
		before.assertUnchanged(t)
	})
}

// ---------------------------------------------------------------------------
// every reader checks the pair against the row it holds
// ---------------------------------------------------------------------------

// TestEveryReaderChecksThePairAgainstItsRow: the CLI's loader, with the
// entry's name (locked) and without one (unlocked, as for an ambiguous entry),
// and the roost provider's inventory each refuse to present a mixed pair (one
// enrolment's certificate beside another's key, what an interleaved or
// interrupted write can leave) and a complete pair for a user the row does not
// record. The mixed pair is no refusal (the entry re-enrols under the held
// user rule); the mismatch is, and sends nothing.
func TestEveryReaderChecksThePairAgainstItsRow(t *testing.T) {
	cases := []struct {
		name    string
		write   func(t *testing.T, m *mtlsServer)
		user    string
		refused bool
	}{
		{"a mixed pair", func(t *testing.T, m *mtlsServer) {
			a := m.credential(t, "cli-key", farFromExpiry)
			b := m.credential(t, "cli-key", farFromExpiry)
			certPath, keyPath := config.ClientCredentialPaths("srv")
			if err := os.WriteFile(certPath, []byte(a.Bundle.ClientCert), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(keyPath, b.KeyPEM, 0o600); err != nil {
				t.Fatal(err)
			}
		}, testUser, false},
		{"a complete pair for another user", func(t *testing.T, m *mtlsServer) {
			admin := m.credentialFor(t, "admin-key", "admin", farFromExpiry)
			if _, _, err := config.WriteClientCredentials("srv", []byte(admin.Bundle.ClientCert), admin.KeyPEM); err != nil {
				t.Fatal(err)
			}
		}, testUser, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			testClientConfig(t)
			m := newMTLSServer(t)
			entry := mtlsEntry(t, m, "srv", m.credential(t, "cli-key", farFromExpiry))
			entry.User = tc.user
			putServerEntry(t, "srv", entry)
			tc.write(t, m)

			for _, name := range []string{"srv", ""} {
				cert, err := loadClientCert(&entry, name)
				_, refused := config.AsEntryRefused(err)
				if cert != nil || err == nil || refused != tc.refused {
					t.Errorf("loader (name %q): got (%v, %v), want nothing presented, refused=%v", name, cert, err, tc.refused)
				}
			}
			inv := roostprovider.Inventory(context.Background(), clientConfig.Servers, 0)
			if len(inv.Answered) != 0 {
				t.Errorf("the roost inventory presented the pair: %#v", inv)
			}
			if _, refused := inv.Refused["srv"]; refused != tc.refused {
				t.Errorf("the roost inventory's refusal = %v, want refused=%v", inv.Refused, tc.refused)
			}
			if got := m.accepted.Load(); got != 0 {
				t.Errorf("the server accepted %d connection(s), want 0", got)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// the transport backstop is on every client
// ---------------------------------------------------------------------------

// TestUnpinnedClientRefusedLaterNeverDials: a plain-HTTP client whose source
// is refused after it was built (a refresh refused mid-command) cannot reach
// its server through any http.Client it hands out: the refusal gate wraps the
// default transport too.
func TestUnpinnedClientRefusedLaterNeverDials(t *testing.T) {
	var accepted atomic.Int32
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	srv.Listener = acceptCounter{Listener: srv.Listener, n: &accepted}
	srv.Start()
	t.Cleanup(srv.Close)

	refusal := &config.EntryRefusedError{Entry: "srv", Kind: config.RefusedIssuedUserChange, CertUser: "admin", EntryUser: "pat"}
	src := clienttoken.New(clienttoken.TokenCredential("tok", time.Now().Add(24*time.Hour)),
		func() (clienttoken.Credential, error) { return clienttoken.Credential{}, refusal })
	c := newAPIClientWithSource(srv.URL, "", src, DefaultTimeout)
	_, gen := src.Current()
	if _, err := src.Refresh(gen); err != refusal {
		t.Fatalf("Refresh = %v, want the refusal", err)
	}

	for what, client := range map[string]*http.Client{"httpClient": c.httpClient, "newHTTPClient": c.newHTTPClient(0)} {
		_, err := client.Get(srv.URL + "/api/info")
		if _, ok := config.AsEntryRefused(err); !ok {
			t.Errorf("%s: err = %v, want the refusal", what, err)
		}
	}
	if got := accepted.Load(); got != 0 {
		t.Errorf("the server accepted %d connection(s), want 0", got)
	}
}

// ---------------------------------------------------------------------------
// the refusal surfaces, unwrapped, through every command path
// ---------------------------------------------------------------------------

// refusedServerEntry installs name as an entry whose stored certificate is
// pat's while the entry records admin: refused for its stored certificate.
func refusedServerEntry(t *testing.T, m *mtlsServer, name string) {
	t.Helper()
	entry := mtlsEntry(t, m, name, m.credential(t, "cli-key", farFromExpiry))
	entry.User = "admin"
	clientConfig.Servers[name] = entry
}

// TestRefusalSurfacesThroughDiscovery: a refused entry's message, unwrapped,
// is what a command that looks a shed up reports — not "shed not found", and
// not a wrapped "failed to get shed status" — and the roost provider's
// activation shows it instead of "did not answer". A refused entry that is
// not where the shed lives does not stop a command that finds it elsewhere.
func TestRefusalSurfacesThroughDiscovery(t *testing.T) {
	t.Run("--server names a refused entry", func(t *testing.T) {
		testClientConfig(t)
		m := newMTLSServer(t)
		refusedServerEntry(t, m, "srv")
		withServerFlag(t, "srv")
		var err error
		stderr := captureStderr(t, func() { _, _, err = findShedServer("box") })
		asRefusal(t, err, config.RefusedStoredUserMismatch)
		if strings.Contains(stderr, "not found") {
			t.Errorf("a refusal was reported as not found:\n%s", stderr)
		}
		if got := m.accepted.Load(); got != 0 {
			t.Errorf("the server accepted %d connection(s), want 0", got)
		}
	})

	t.Run("the default server is refused and no other has the shed", func(t *testing.T) {
		testClientConfig(t)
		m := newMTLSServer(t)
		refusedServerEntry(t, m, "srv")
		clientConfig.DefaultServer = "srv"
		addFakeServer("b", newFakeShedServer(t))
		withServerFlag(t, "")
		_, _, err := findShedServer("box")
		asRefusal(t, err, config.RefusedStoredUserMismatch)
	})

	t.Run("the cached server is refused", func(t *testing.T) {
		testClientConfig(t)
		m := newMTLSServer(t)
		refusedServerEntry(t, m, "srv")
		addFakeServer("b", newFakeShedServer(t, "box"))
		clientConfig.Sheds["box"] = config.ShedCache{Server: "srv"}
		withServerFlag(t, "")
		_, _, err := findShedServer("box")
		asRefusal(t, err, config.RefusedStoredUserMismatch)
		if got, _ := clientConfig.GetShedServer("box"); got != "srv" {
			t.Errorf("the cache row was dropped (now %q)", got)
		}
	})

	t.Run("a refused server does not hide a shed found elsewhere", func(t *testing.T) {
		testClientConfig(t)
		m := newMTLSServer(t)
		refusedServerEntry(t, m, "srv")
		clientConfig.DefaultServer = "srv"
		addFakeServer("b", newFakeShedServer(t, "box"))
		withServerFlag(t, "")
		name, _, err := findShedServer("box")
		if err != nil || name != "b" {
			t.Fatalf("findShedServer = (%q, %v), want b", name, err)
		}
	})

	t.Run("ensureRunningShed", func(t *testing.T) {
		testClientConfig(t)
		m := newMTLSServer(t)
		refusedServerEntry(t, m, "srv")
		e := clientConfig.Servers["srv"]
		_, err := ensureRunningShed(NewAPIClientFromNamedEntry("srv", &e, DefaultTimeout), "box")
		asRefusal(t, err, config.RefusedStoredUserMismatch)
	})

	t.Run("the roost provider's activation", func(t *testing.T) {
		testClientConfig(t)
		m := newMTLSServer(t)
		refusedServerEntry(t, m, "srv")
		_, _, row := resolveTarget(t.Context(), roostprovider.Token{Shed: "box", Server: "srv"})
		if row == nil || !strings.Contains(row.Subtitle, "shed server update srv --refetch") {
			t.Fatalf("row = %+v, want the refusal's message", row)
		}
		if got := m.accepted.Load(); got != 0 {
			t.Errorf("the server accepted %d connection(s), want 0", got)
		}
	})
}

// ---------------------------------------------------------------------------
// the tunnel daemon's hand-off
// ---------------------------------------------------------------------------

// TestTunnelSourceHeldToTheUserTheClientAdopted: the tunnel daemon's mint-only
// source is seeded from the API client after the client adopted its first
// named user. The caller's copy of the entry is from before that and records
// no user; the source is held to the user the client holds (pat), so a later
// certificate for admin is refused rather than taken as a "first" user.
func TestTunnelSourceHeldToTheUserTheClientAdopted(t *testing.T) {
	testClientConfig(t)
	m := newMTLSServer(t)
	putServerEntry(t, "srv", config.ServerEntry{
		Host: "127.0.0.1", SSHPort: 2222, APIURL: m.srv.URL, TLSCertFingerprint: m.pin,
	})
	next := m.credential(t, "cli-key", farFromExpiry)
	stubBootstrap(func() sdk.Credential { return next })

	stale := clientConfig.Servers["srv"] // read before the client enrolled
	client := NewAPIClientFromNamedEntry("srv", &stale, DefaultTimeout)
	if got := client.heldUserName(""); got != testUser {
		t.Fatalf("test setup: the client holds %q, want pat", got)
	}
	if stale.User != "" {
		t.Fatal("test setup: the caller's copy is supposed to be stale")
	}

	target, err := connectTargetFromEntry(&stale, "")
	if err != nil {
		t.Fatal(err)
	}
	src := tunnelCredentialSource(client, "srv", &stale, target)
	next = m.credentialFor(t, "admin-key", "admin", farFromExpiry)
	_, gen := src.Current()
	_, err = src.Refresh(gen)
	r := asRefusal(t, err, config.RefusedIssuedUserChange)
	if r.EntryUser != testUser {
		t.Errorf("the tunnel source was held to %q, want the client's pat", r.EntryUser)
	}
}
