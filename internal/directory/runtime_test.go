package directory

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/charliek/shed/internal/users"
)

// rtT0 is when the runtime tests start, on their fake clock.
var rtT0 = time.Date(2026, 11, 16, 9, 0, 0, 0, time.UTC)

var versionRx = regexp.MustCompile(`^[0-9a-f]{64}$`)

// readCopy decodes users.json in stateDir.
func readCopy(t *testing.T, stateDir string) copyFile {
	t.Helper()
	var f copyFile
	if err := json.Unmarshal(copyBytes(t, stateDir), &f); err != nil {
		t.Fatalf("users.json: %v", err)
	}
	return f
}

// TestRuntimeTimedScenario is the timed scenario the contract asks of a
// client, on a fake clock: a good pull; a removal; failed refreshes (401,
// 403 SERVER_DISABLED, 503, a timeout), each leaving the list and the kept
// copy as they were, across a restart that starts from the copy; the trust
// time passing, after which the copy is refused at the next start; a good
// pull restoring the list. A key with an expiry is refused from its instant
// on throughout, and the local admin logs in at every step.
func TestRuntimeTimedScenario(t *testing.T) {
	global := captureLog(t)
	clock := newFakeClock(rtT0)
	svc := newRTService(t, clock)
	stateDir := filepath.Join(t.TempDir(), "directory")
	e := svc.enrol(stateDir)
	opsLine, _ := newTestKey(t)
	ada, bob, cy := newPerson(t, "ada", true), newPerson(t, "bob", false), newPerson(t, "cy", false)
	bob.expires = rtT0.Add(2*time.Hour + 30*time.Minute)
	svc.dir.setUsers(ada.served(), bob.served(), cy.served())

	srv := startRT(t, svc, stateDir, opsLine, nil)
	if st := srv.rt.Status(); st.State != StateNoCopy || st.Copy != CopyNone || st.ClientID != e.ClientID {
		t.Fatalf("before the first pull: %+v", st)
	}
	opsWorks := func(s *rtServer, step string) {
		t.Helper()
		if s.owner(s.opsFP) != "ops" {
			t.Fatalf("%s: the local admin's key does not resolve", step)
		}
	}
	opsWorks(srv, "start")

	// A good pull.
	row := srv.refresh(t)
	if row.Status != users.DirectoryRefreshGood || row.Users != 3 || row.Skipped != 0 || !versionRx.MatchString(row.Version) || row.Error != "" {
		t.Fatalf("first pull row %+v", row)
	}
	for _, p := range []*person{ada, bob, cy} {
		if got := srv.owner(p.fp); got != p.name {
			t.Fatalf("%s's key resolves to %q", p.name, got)
		}
	}
	if u, k, _ := srv.rt.UserForKey(ada.fp); !u.Admin || k.Source != users.SourceDirectory || k.Label != "laptop" {
		t.Errorf("ada = %+v, key %+v", u, k)
	}
	if _, k, _ := srv.rt.UserForKey(bob.fp); k.Expires == nil || !k.Expires.Equal(bob.expires) {
		t.Errorf("bob's key expiry = %v, want %v", k.Expires, bob.expires)
	}
	st := srv.rt.Status()
	if st.State != StateTrusted || !st.LastGoodPull.Equal(rtT0) || !st.TrustedUntil.Equal(rtT0.Add(rtTrust)) ||
		!st.NextAttempt.Equal(rtT0.Add(rtRefresh)) || st.Copy != CopyKept || st.Version != row.Version {
		t.Fatalf("after the first pull: %+v", st)
	}
	if f := readCopy(t, stateDir); f.Format != 1 || !f.FetchedAt.Equal(rtT0) || f.ClientID != e.ClientID || f.Issuer != svc.issuer || f.Version != row.Version || f.Organization != worldOrg {
		t.Fatalf("the kept copy: %+v", f)
	}
	if fi, err := os.Stat(filepath.Join(stateDir, CopyFileName)); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("users.json: %v %v", fi, err)
	}

	// A removal, an hour later.
	fetched1 := rtT0.Add(time.Hour)
	clock.set(fetched1)
	svc.dir.setUsers(ada.served(), bob.served())
	row = srv.refresh(t)
	if row.Status != users.DirectoryRefreshGood || row.Users != 2 {
		t.Fatalf("removal pull row %+v", row)
	}
	if srv.owner(cy.fp) != "" {
		t.Fatal("cy's key still resolves after cy left the list")
	}
	if !strings.Contains(srv.log.String(), `removed "cy"`) {
		t.Errorf("the good pull's line does not name the removal:\n%s", srv.log)
	}
	version := row.Version
	goodCopy := copyBytes(t, stateDir)
	if f := readCopy(t, stateDir); !f.FetchedAt.Equal(fetched1) {
		t.Fatalf("the copy's fetched_at = %v, want %v", f.FetchedAt, fetched1)
	}

	// Failed refreshes: each leaves the list and the copy as they were. The
	// 403 comes first, while the access token from the removal's pull is
	// still held (a disabled server cannot log in again).
	fault := func(m faultMode) func(on bool) {
		return func(on bool) {
			if on {
				svc.setMode(m, "")
			} else {
				svc.setMode(faultNone, "")
			}
		}
	}
	for _, f := range []struct {
		name    string
		set     func(on bool)
		errText string
		// timeout: the service never answers; the pull ends at its bound
		// (PullTimeout on the fake clock), and the next attempt is counted
		// from then.
		timeout bool
	}{
		{"403 SERVER_DISABLED", func(on bool) { svc.dir.setDisabled(e.ClientID, on) }, `server-disabled (HTTP 403) "SERVER_DISABLED"`, false},
		{"401", fault(fault401), "invalid-token (HTTP 401)", false},
		{"503", svc.dir.setOutage, "unavailable (HTTP 503)", false},
		{"timeout", fault(faultHang), "transport: no answer within the pull's bound of 30s", true},
	} {
		clock.advance(time.Minute)
		at := clock.Now()
		f.set(true)
		var row users.DirectoryRefresh
		failedAt := at
		if f.timeout {
			row = srv.refreshTimingOut(t, svc)
			failedAt = at.Add(PullTimeout)
		} else {
			row = srv.refresh(t)
		}
		f.set(false)
		if row.Status != users.DirectoryRefreshFailed || !strings.Contains(row.Error, f.errText) || row.Version != version || row.Users != 2 {
			t.Fatalf("%s: row %+v; want failed with %q, the list held", f.name, row, f.errText)
		}
		if srv.owner(ada.fp) != "ada" || srv.owner(bob.fp) != "bob" {
			t.Fatalf("%s: the list held changed", f.name)
		}
		if got := copyBytes(t, stateDir); string(got) != string(goodCopy) {
			t.Fatalf("%s: the kept copy changed", f.name)
		}
		st := srv.rt.Status()
		if st.State != StateTrusted || !st.LastAttempt.Equal(at) || !st.NextAttempt.Equal(failedAt.Add(RetryInterval)) ||
			!st.LastGoodPull.Equal(fetched1) || !strings.Contains(st.LastError, f.errText) {
			t.Fatalf("%s: status %+v", f.name, st)
		}
		opsWorks(srv, f.name)
	}
	srv.stop()

	// A restart: the copy is published at once; the service is down.
	svc.dir.setOutage(true)
	again := startRT(t, svc, stateDir, opsLine, nil)
	lines := strings.Join(again.rt.StartupLines(), "\n")
	wantLine := "Users: kept copy from 2026-11-16T10:00:00Z, version " + version + ", 2 users (0 skipped), trusted until 2026-11-16T14:00:00Z"
	if !strings.Contains(lines, wantLine) || !strings.Contains(lines, "client id "+e.ClientID) {
		t.Fatalf("start-up lines:\n%s\nwant %q", lines, wantLine)
	}
	if again.owner(ada.fp) != "ada" || again.owner(bob.fp) != "bob" {
		t.Fatal("after the restart the copy's list is not published")
	}
	opsWorks(again, "restart")
	if row := again.refresh(t); row.Status != users.DirectoryRefreshFailed || row.Version != version {
		t.Fatalf("a failed pull after the restart: %+v", row)
	}

	// Bob's key: accepted until its instant, refused from it on.
	clock.set(bob.expires.Add(-time.Second))
	if _, k, ok := again.rt.UserForKey(bob.fp); !ok || k.Expired(clock.Now()) {
		t.Fatal("bob's key refused before its expiry")
	}
	clock.set(bob.expires)
	if _, k, ok := again.rt.UserForKey(bob.fp); !ok || !k.Expired(clock.Now()) {
		t.Fatal("bob's key not refused at its expiry")
	}
	if _, _, ok := users.ResolveBound(again.rt, bob.fp, users.BindingOf(users.User{Name: "bob"}), clock.Now()); ok {
		t.Fatal("bob's bound credential accepted at his key's expiry")
	}

	// The trust time passes: from its instant only the local admin.
	clock.set(fetched1.Add(rtTrust - time.Second))
	if again.owner(ada.fp) != "ada" {
		t.Fatal("ada refused before the trust time ended")
	}
	again.start()
	clock.set(fetched1.Add(rtTrust))
	if again.owner(ada.fp) != "" {
		t.Fatal("ada still resolves when the trust time has ended")
	}
	opsWorks(again, "trust ended")
	gen := again.rt.State().Gen
	again.ticks <- clock.Now()
	again.ticks <- clock.Now() // the second tick waits for the first to be handled
	if g := again.rt.State().Gen; g != gen+1 {
		t.Fatalf("the end of the trust time was not published (gen %d -> %d)", gen, g)
	}
	// The end of the trust time is made durable: the kept copy is removed;
	// the last good pull and its version stay known.
	if st := again.rt.Status(); st.State != StateExpired || st.Copy != CopyNone ||
		!strings.Contains(st.CopyError, "removed: the trust time of its list ended at 2026-11-16T14:00:00Z") ||
		!st.LastGoodPull.Equal(fetched1) || st.Version != version {
		t.Fatalf("status after the trust time: %+v", st)
	}
	if copyBytes(t, stateDir) != nil {
		t.Fatal("the kept copy was not removed when the trust time ended")
	}
	if n := again.log.count("kept copy removed: the trust time of its list ended at 2026-11-16T14:00:00Z"); n != 1 {
		t.Errorf("the removal was logged %d times:\n%s", n, again.log)
	}
	again.stop()

	// A restart after the trust time: there is no copy.
	late := startRT(t, svc, stateDir, opsLine, nil)
	lines = strings.Join(late.rt.StartupLines(), "\n")
	if !strings.Contains(lines, "Users: no kept copy; only the local admins can log in until the first good pull") {
		t.Fatalf("start-up lines after the trust time:\n%s", lines)
	}
	if st := late.rt.Status(); st.State != StateNoCopy || st.Copy != CopyNone || late.owner(ada.fp) != "" {
		t.Fatalf("after a restart past the trust time: %+v, ada %q", st, late.owner(ada.fp))
	}
	opsWorks(late, "restart past the trust time")

	// The service is back: a good pull restores the list (bob's key has
	// expired, so the service no longer lists him).
	svc.dir.setOutage(false)
	if row := late.refresh(t); row.Status != users.DirectoryRefreshGood || row.Users != 1 {
		t.Fatalf("the restoring pull: %+v", row)
	}
	if late.owner(ada.fp) != "ada" || late.rt.Status().State != StateTrusted {
		t.Fatal("a good pull did not restore the list")
	}
	if late.log.count("good pull: version") != 1 || copyBytes(t, stateDir) == nil {
		t.Errorf("the restoring pull was not logged, or wrote no copy:\n%s", late.log)
	}
	opsWorks(late, "restored")

	if !strings.Contains(global.String(), "TRUST ENDED") {
		t.Errorf("no trust ended line:\n%s", global)
	}
	secrets := svc.secrets()
	if len(secrets) < 4 {
		t.Fatalf("the service recorded %d tokens and assertions", len(secrets))
	}
	all := srv.log.String() + again.log.String() + late.log.String() + global.String() +
		strings.Join(srv.rt.StartupLines(), "\n") + string(copyBytes(t, stateDir))
	noSecretsIn(t, "the logs and the copy", all, secrets)
	if strings.Contains(again.log.String(), "context canceled") {
		t.Errorf("a pull ended by the server stopping was logged as a failure:\n%s", again.log)
	}
}

// TestRuntimeStartUp checks the start from each kind of state directory:
// the copy is used only when every check passes; otherwise only the local
// admins can log in, and the reason is logged and shown.
func TestRuntimeStartUp(t *testing.T) {
	type outcome struct {
		state SourceState
		copy  CopyCondition
		line  string // in the start-up lines
		log   string // in the log ("" none)
		ada   bool   // ada's key resolves
	}
	editJSON := func(t *testing.T, path string, edit func(m map[string]any)) {
		t.Helper()
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		m := map[string]any{}
		if err := json.Unmarshal(b, &m); err != nil {
			t.Fatal(err)
		}
		edit(m)
		out, err := json.Marshal(m)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, out, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		name string
		// prepare changes the state directory after one good pull, and may
		// move the clock.
		prepare func(t *testing.T, dir string, clock *fakeClock)
		want    outcome
	}{
		{"fresh copy", nil, outcome{StateTrusted, CopyKept, "Users: kept copy from 2026-11-16T09:00:00Z", "", true}},
		{"copy past its trust time", func(t *testing.T, dir string, clock *fakeClock) { clock.advance(rtTrust) },
			outcome{StateExpired, CopyNone, "past its trust time (2026-11-16T13:00:00Z)", "kept copy removed: the trust time of its list ended at 2026-11-16T13:00:00Z", false}},
		{"copy just inside its trust time", func(t *testing.T, dir string, clock *fakeClock) { clock.advance(rtTrust - time.Nanosecond) },
			outcome{StateTrusted, CopyKept, "Users: kept copy from", "", true}},
		{"foreign client id", func(t *testing.T, dir string, _ *fakeClock) {
			editJSON(t, filepath.Join(dir, CopyFileName), func(m map[string]any) { m["client_id"] = "srv_" + strings.Repeat("0", 32) })
		}, outcome{StateNoCopy, CopyUnused, "no kept copy", "pulled by the client id", false}},
		{"foreign issuer", func(t *testing.T, dir string, _ *fakeClock) {
			editJSON(t, filepath.Join(dir, CopyFileName), func(m map[string]any) { m["issuer"] = "https://other.example.test" })
		}, outcome{StateNoCopy, CopyUnused, "no kept copy", "pulled from the issuer", false}},
		{"another organization", func(t *testing.T, dir string, _ *fakeClock) {
			editJSON(t, filepath.Join(dir, CopyFileName), func(m map[string]any) { m["organization"] = "other" })
		}, outcome{StateNoCopy, CopyUnused, "no kept copy", "names the organization", false}},
		{"future-dated", func(t *testing.T, dir string, clock *fakeClock) {
			editJSON(t, filepath.Join(dir, CopyFileName), func(m map[string]any) {
				m["fetched_at"] = clock.Now().Add(time.Nanosecond).Format(time.RFC3339Nano)
			})
		}, outcome{StateNoCopy, CopyUnused, "no kept copy", "its fetched_at 2026-11-16T09:00:00.000000001Z is after this server's clock", false}},
		{"a body today's rules refuse", func(t *testing.T, dir string, _ *fakeClock) {
			editJSON(t, filepath.Join(dir, CopyFileName), func(m map[string]any) {
				m["body"] = base64.StdEncoding.EncodeToString([]byte(`{"contract":2,"organization":"example","version":"` + strings.Repeat("a", 64) + `","users":[]}`))
			})
		}, outcome{StateNoCopy, CopyUnused, "no kept copy", "its body is refused", false}},
		{"an unknown member", func(t *testing.T, dir string, _ *fakeClock) {
			editJSON(t, filepath.Join(dir, CopyFileName), func(m map[string]any) { m["extra"] = 1 })
		}, outcome{StateNoCopy, CopyUnused, "no kept copy", `unknown member "extra"`, false}},
		{"symlinked copy", func(t *testing.T, dir string, _ *fakeClock) {
			src := filepath.Join(dir, CopyFileName)
			other := filepath.Join(dir, "elsewhere.json")
			if err := os.Rename(src, other); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(other, src); err != nil {
				t.Fatal(err)
			}
		}, outcome{StateNoCopy, CopyUnused, "no kept copy", "is a symbolic link", false}},
		{"group-readable copy", func(t *testing.T, dir string, _ *fakeClock) {
			if err := os.Chmod(filepath.Join(dir, CopyFileName), 0o640); err != nil {
				t.Fatal(err)
			}
		}, outcome{StateNoCopy, CopyUnused, "no kept copy", "group or others may use it", false}},
		{"no copy", func(t *testing.T, dir string, _ *fakeClock) {
			if err := os.Remove(filepath.Join(dir, CopyFileName)); err != nil {
				t.Fatal(err)
			}
		}, outcome{StateNoCopy, CopyNone, "Users: no kept copy; only the local admins can log in until the first good pull", "", false}},
		{"no enrolment", func(t *testing.T, dir string, _ *fakeClock) {
			if err := os.Remove(filepath.Join(dir, EnrolmentFileName)); err != nil {
				t.Fatal(err)
			}
		}, outcome{StateNotEnrolled, CopyNone, "Users: not enrolled (no enrolment: " /* path follows */, "", false}},
		{"no state directory", func(t *testing.T, dir string, _ *fakeClock) {
			if err := os.RemoveAll(dir); err != nil {
				t.Fatal(err)
			}
		}, outcome{StateNotEnrolled, CopyNone, "does not exist): run 'shed-server directory enroll'; only the local admins can log in", "", false}},
		{"enrolment for another issuer", func(t *testing.T, dir string, _ *fakeClock) {
			editJSON(t, filepath.Join(dir, EnrolmentFileName), func(m map[string]any) {
				m["issuer"] = "https://other.example.test"
				m["token_endpoint"] = "https://other.example.test/oauth2/token"
			})
		}, outcome{StateNotEnrolled, CopyNone, "enrolled with the issuer \"https://other.example.test\"", "", false}},
		{"group-readable enrolment", func(t *testing.T, dir string, _ *fakeClock) {
			if err := os.Chmod(filepath.Join(dir, EnrolmentFileName), 0o640); err != nil {
				t.Fatal(err)
			}
		}, outcome{StateEnrolmentUnusable, CopyNone, "Users: enrolment unusable (" /* path */, "", false}},
		{"enrolment that does not decode", func(t *testing.T, dir string, _ *fakeClock) {
			if err := os.WriteFile(filepath.Join(dir, EnrolmentFileName), []byte(`{"format":1}`+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
		}, outcome{StateEnrolmentUnusable, CopyNone, "enrolment.json: client_id", "", false}},
		{"state directory group-readable", func(t *testing.T, dir string, _ *fakeClock) {
			if err := os.Chmod(dir, 0o750); err != nil {
				t.Fatal(err)
			}
		}, outcome{StateEnrolmentUnusable, CopyNone, "has mode 0750, not 0700", "", false}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clock := newFakeClock(rtT0)
			svc := newRTService(t, clock)
			dir := filepath.Join(t.TempDir(), "directory")
			svc.enrol(dir)
			opsLine, _ := newTestKey(t)
			ada := newPerson(t, "ada", false)
			svc.dir.setUsers(ada.served())
			first := startRT(t, svc, dir, opsLine, nil)
			if row := first.refresh(t); row.Status != users.DirectoryRefreshGood {
				t.Fatalf("set-up pull: %+v", row)
			}
			first.stop()
			if tc.prepare != nil {
				tc.prepare(t, dir, clock)
			}
			requests := svc.directoryCount() + svc.tokenCount()

			srv := startRT(t, svc, dir, opsLine, nil)
			st := srv.rt.Status()
			lines := strings.Join(srv.rt.StartupLines(), "\n")
			if st.State != tc.want.state || st.Copy != tc.want.copy || !strings.Contains(lines, tc.want.line) {
				t.Errorf("state %s, copy %s, lines:\n%s\nwant %s, %s, a line holding %q", st.State, st.Copy, lines, tc.want.state, tc.want.copy, tc.want.line)
			}
			if tc.want.log != "" && !strings.Contains(srv.log.String(), tc.want.log) {
				t.Errorf("log:\n%s\nwant %q", srv.log, tc.want.log)
			}
			if tc.want.log == "" && srv.log.String() != "" {
				t.Errorf("log at start:\n%s\nwant none", srv.log)
			}
			if got := srv.owner(ada.fp) == "ada"; got != tc.want.ada {
				t.Errorf("ada resolves: %v, want %v", got, tc.want.ada)
			}
			if srv.owner(srv.opsFP) != "ops" {
				t.Error("the local admin does not resolve")
			}
			if n := svc.directoryCount() + svc.tokenCount(); n != requests {
				t.Errorf("Load sent %d requests", n-requests)
			}
			// Not enrolled, or an enrolment that cannot be used: a pull
			// sends nothing.
			if tc.want.state == StateNotEnrolled || tc.want.state == StateEnrolmentUnusable {
				row := srv.refresh(t)
				if row.Status != users.DirectoryRefreshFailed || svc.directoryCount()+svc.tokenCount() != requests {
					t.Errorf("a pull without a usable enrolment: %+v, %d requests sent", row, svc.directoryCount()+svc.tokenCount()-requests)
				}
			}
		})
	}
}

// TestRuntimeCopyWriteFails: when the new copy cannot be written the old
// one is removed and the list is published in memory only, so a restart
// starts with only the local admins (it cannot bring back a person the new
// list removed). When the old copy cannot be removed either, status and the
// log at every pull say the copy on disk is stale.
func TestRuntimeCopyWriteFails(t *testing.T) {
	clock := newFakeClock(rtT0)
	svc := newRTService(t, clock)
	dir := filepath.Join(t.TempDir(), "directory")
	svc.enrol(dir)
	opsLine, _ := newTestKey(t)
	ada, bob := newPerson(t, "ada", false), newPerson(t, "bob", false)
	svc.dir.setUsers(ada.served(), bob.served())

	writeErr := errors.New("disk full")
	var failWrite, failRemove bool
	adjust := func(rt *Runtime) {
		write, remove := rt.writeCopy, rt.removeCopy
		rt.writeCopy = func(d *StateDir, data []byte) error {
			if failWrite {
				return writeErr
			}
			return write(d, data)
		}
		rt.removeCopy = func(d *StateDir) error {
			if failRemove {
				return errors.New("read-only file system")
			}
			return remove(d)
		}
	}
	srv := startRT(t, svc, dir, opsLine, adjust)
	if row := srv.refresh(t); row.Status != users.DirectoryRefreshGood || copyBytes(t, dir) == nil {
		t.Fatalf("set-up pull: %+v", row)
	}

	// Bob leaves; the copy cannot be written.
	clock.advance(time.Minute)
	failWrite = true
	svc.dir.setUsers(ada.served())
	if row := srv.refresh(t); row.Status != users.DirectoryRefreshGood || row.Users != 1 {
		t.Fatalf("pull with a failed write: %+v", row)
	}
	if srv.owner(bob.fp) != "" || srv.owner(ada.fp) != "ada" {
		t.Fatal("the new list was not published in memory")
	}
	if copyBytes(t, dir) != nil {
		t.Fatal("the old copy was not removed")
	}
	if st := srv.rt.Status(); st.Copy != CopyNotWritten || !strings.Contains(st.CopyError, "disk full") {
		t.Fatalf("status: copy %s %q", st.Copy, st.CopyError)
	}
	if !strings.Contains(srv.log.String(), "kept copy not written: disk full; a restart starts with only the local admins until a good pull") {
		t.Errorf("log:\n%s", srv.log)
	}
	srv.stop()

	// A restart has only the local admins: neither bob (removed) nor ada.
	again := startRT(t, svc, dir, opsLine, adjust)
	if again.owner(bob.fp) != "" || again.owner(ada.fp) != "" || again.rt.Status().State != StateNoCopy {
		t.Fatalf("after the restart: bob %q ada %q status %+v", again.owner(bob.fp), again.owner(ada.fp), again.rt.Status())
	}
	if again.owner(again.opsFP) != "ops" {
		t.Fatal("the local admin does not resolve")
	}

	// A good pull writes a copy again; then neither the write nor the
	// removal works: the copy on disk is stale, said at every pull.
	failWrite = false
	svc.dir.setUsers(ada.served(), bob.served())
	if row := again.refresh(t); row.Status != users.DirectoryRefreshGood || again.rt.Status().Copy != CopyKept {
		t.Fatalf("pull with the write working again: %+v %s", row, again.rt.Status().Copy)
	}
	stale := copyBytes(t, dir)
	failWrite, failRemove = true, true
	clock.advance(time.Minute)
	svc.dir.setUsers(ada.served())
	if row := again.refresh(t); row.Status != users.DirectoryRefreshGood {
		t.Fatalf("pull: %+v", row)
	}
	if st := again.rt.Status(); st.Copy != CopyStale || !strings.Contains(st.CopyError, "disk full; the copy on disk remains: removing it failed: read-only file system") {
		t.Fatalf("status: copy %s %q", st.Copy, st.CopyError)
	}
	if string(copyBytes(t, dir)) != string(stale) {
		t.Fatal("the stale copy changed")
	}
	svc.dir.setOutage(true)
	again.refresh(t) // a failed pull
	svc.dir.setOutage(false)
	again.refresh(t) // a good pull, the write still failing
	if n := again.log.count("the copy on disk is stale"); n != 3 {
		t.Errorf("the stale copy was logged %d times over three pulls:\n%s", n, again.log)
	}
	again.stop()

	// The one case a restart brings back the old list: the stale copy.
	late := startRT(t, svc, dir, opsLine, adjust)
	if late.owner(bob.fp) != "bob" {
		t.Fatal("the stale copy was not used at the restart (the documented case)")
	}
	failWrite, failRemove = false, false
	if row := late.refresh(t); row.Status != users.DirectoryRefreshGood || late.rt.Status().Copy != CopyKept || late.owner(bob.fp) != "" {
		t.Fatalf("a good pull with the write working: %+v", row)
	}
	if !strings.Contains(late.log.String(), "kept copy written again") && late.log.count("good pull") == 0 {
		t.Errorf("log:\n%s", late.log)
	}
}

// TestRuntimeSingleFlight: one pull at a time. A pull asked for while one
// runs waits for it, and every caller asking meanwhile joins that same
// waiting pull, so an admin's refresh never returns a pull that started
// before the call.
func TestRuntimeSingleFlight(t *testing.T) {
	clock := newFakeClock(rtT0)
	svc := newRTService(t, clock)
	dir := filepath.Join(t.TempDir(), "directory")
	svc.enrol(dir)
	opsLine, _ := newTestKey(t)
	ada, bob := newPerson(t, "ada", false), newPerson(t, "bob", false)
	svc.dir.setUsers(ada.served())
	srv := startRT(t, svc, dir, opsLine, nil)

	gate := make(chan struct{})
	svc.setGate(gate)
	fA := srv.rt.request()
	select {
	case <-svc.arrived:
	case <-time.After(10 * time.Second):
		t.Fatal("the first pull did not reach the service")
	}
	// A's answer is made (ada only) and held. The list changes; B and C ask
	// while A runs: both wait for one pull after A.
	svc.dir.setUsers(ada.served(), bob.served())
	fB := srv.rt.request()
	fC := srv.rt.request()
	svc.setGate(nil)
	var release sync.Once
	defer release.Do(func() { close(gate) })
	if fB != fC || fB == fA {
		t.Fatal("callers during a running pull do not share the one pull after it")
	}
	rowD := make(chan users.DirectoryRefresh, 1)
	go func() { rowD <- srv.refresh(t) }()

	release.Do(func() { close(gate) })
	for _, f := range []*flight{fA, fB} {
		select {
		case <-f.done:
		case <-time.After(10 * time.Second):
			t.Fatal("a pull asked for never ran")
		}
	}
	if fA.row.Users != 1 || fB.row.Users != 2 || fB.row.Version == fA.row.Version {
		t.Fatalf("A %+v, B %+v; want A the old list, B the new", fA.row, fB.row)
	}
	select {
	case d := <-rowD:
		if d.Users != 2 {
			t.Fatalf("a refresh asked during A returned %+v; want a pull started after it", d)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("a refresh asked during A never returned")
	}
	if n := svc.directoryCount(); n != 2 && n != 3 {
		t.Fatalf("%d directory requests; want A's and one more (or two, when the refresh came after B ran)", n)
	}
	if srv.owner(bob.fp) != "bob" {
		t.Fatal("the newer list is not the one published")
	}
}

// TestRuntimeSchedule: the first pull at once; the next at the last good
// pull + refresh, and not before; after a failure, 15 minutes, or the
// Retry-After when later (at most an hour); an admin's refresh ignores a
// pending Retry-After, and the schedule follows the result.
func TestRuntimeSchedule(t *testing.T) {
	clock := newFakeClock(rtT0)
	svc := newRTService(t, clock)
	dir := filepath.Join(t.TempDir(), "directory")
	svc.enrol(dir)
	opsLine, _ := newTestKey(t)
	svc.dir.setUsers(newPerson(t, "ada", false).served())
	attempts := make(chan users.DirectoryRefresh, 64)
	waits := make(chan time.Time, 1024)
	srv := startRT(t, svc, dir, opsLine, func(rt *Runtime) {
		rt.afterAttempt = func(row users.DirectoryRefresh) { attempts <- row }
		rt.beforeWait = func(now time.Time) { waits <- now }
	})
	// idleAt waits until the schedule has read the clock at at and gone
	// back to waiting.
	idleAt := func(at time.Time) {
		t.Helper()
		deadline := time.After(10 * time.Second)
		for {
			select {
			case w := <-waits:
				if w.Equal(at) {
					return
				}
			case <-deadline:
				t.Fatalf("the schedule did not wait at %s", stamp(at))
			}
		}
	}
	next := func() users.DirectoryRefresh {
		t.Helper()
		select {
		case row := <-attempts:
			return row
		case <-time.After(10 * time.Second):
			t.Fatal("no pull")
		}
		return users.DirectoryRefresh{}
	}
	none := func(what string) {
		t.Helper()
		select {
		case row := <-attempts:
			t.Fatalf("%s: an unexpected pull %+v", what, row)
		default:
		}
	}
	wantNext := func(want time.Time) {
		t.Helper()
		if got := srv.rt.Status().NextAttempt; !got.Equal(want) {
			t.Fatalf("next attempt %s, want %s", stamp(got), stamp(want))
		}
	}
	srv.start()
	if row := next(); row.Status != users.DirectoryRefreshGood {
		t.Fatalf("first pull: %+v", row)
	}
	wantNext(rtT0.Add(rtRefresh))
	idleAt(rtT0)

	// Just before it is due: the schedule looks again and waits on.
	clock.set(rtT0.Add(rtRefresh - time.Second))
	idleAt(rtT0.Add(rtRefresh - time.Second))
	none("a second before the refresh")
	clock.set(rtT0.Add(rtRefresh))
	if row := next(); row.Status != users.DirectoryRefreshGood {
		t.Fatalf("the refresh: %+v", row)
	}
	wantNext(rtT0.Add(2 * rtRefresh))
	idleAt(rtT0.Add(rtRefresh))

	// A failure with a Retry-After of half an hour, later than the 15
	// minutes.
	svc.setMode(faultRetryAfter, "1800")
	failAt := rtT0.Add(2 * rtRefresh)
	clock.set(failAt)
	if row := next(); row.Status != users.DirectoryRefreshFailed {
		t.Fatalf("pull: %+v", row)
	}
	wantNext(failAt.Add(30 * time.Minute))
	idleAt(failAt)
	clock.set(failAt.Add(15 * time.Minute))
	idleAt(failAt.Add(15 * time.Minute))
	none("15 minutes into a 30-minute Retry-After")

	// An admin's refresh ignores the pending Retry-After. Its Retry-After
	// of two hours is held to one hour; one of a minute gives way to the
	// 15 minutes.
	svc.setMode(faultRetryAfter, "7200")
	adminAt := clock.Now()
	if row := srv.refresh(t); row.Status != users.DirectoryRefreshFailed {
		t.Fatalf("admin refresh: %+v", row)
	}
	next()
	wantNext(adminAt.Add(time.Hour))
	svc.setMode(fault429, "60")
	if row := srv.refresh(t); row.Status != users.DirectoryRefreshFailed || !strings.Contains(row.Error, "rate-limited") {
		t.Fatalf("admin refresh: %+v", row)
	}
	next()
	wantNext(adminAt.Add(RetryInterval))

	// A good admin refresh: the schedule follows it.
	svc.setMode(faultNone, "")
	clock.advance(time.Minute)
	goodAt := clock.Now()
	if row := srv.refresh(t); row.Status != users.DirectoryRefreshGood {
		t.Fatalf("admin refresh: %+v", row)
	}
	next()
	wantNext(goodAt.Add(rtRefresh))
	clock.set(adminAt.Add(RetryInterval)) // the old next attempt
	idleAt(adminAt.Add(RetryInterval))
	none("at the next attempt the good refresh replaced")
	clock.set(goodAt.Add(rtRefresh))
	if row := next(); row.Status != users.DirectoryRefreshGood {
		t.Fatalf("scheduled pull after the admin's: %+v", row)
	}
}

// TestRuntimeEnrolmentReadBeforeEachPull: the enrolment is read before each
// pull. One for another issuer, one that fails its checks, or none, means
// nothing is sent, the state and reason are kept, the list held is
// withdrawn (closing its users' connections), and it is logged once per
// change. Put right, the next pull is good with no restart.
func TestRuntimeEnrolmentReadBeforeEachPull(t *testing.T) {
	clock := newFakeClock(rtT0)
	svc := newRTService(t, clock)
	dir := filepath.Join(t.TempDir(), "directory")
	e := svc.enrol(dir)
	opsLine, _ := newTestKey(t)
	ada := newPerson(t, "ada", false)
	svc.dir.setUsers(ada.served())
	srv := startRT(t, svc, dir, opsLine, nil)
	var pubs []Publication
	srv.rt.SetPublicationHook(func(p Publication) { pubs = append(pubs, p) })
	if row := srv.refresh(t); row.Status != users.DirectoryRefreshGood {
		t.Fatalf("set-up pull: %+v", row)
	}
	path := filepath.Join(dir, EnrolmentFileName)
	good, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	restore := func() {
		t.Helper()
		if err := os.WriteFile(path, good, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		name   string
		break_ func()
		state  SourceState
		reason string
		line   string
	}{
		{"another issuer", func() {
			other := strings.ReplaceAll(string(good), svc.issuer, "https://other.example.test")
			if err := os.WriteFile(path, []byte(other), 0o600); err != nil {
				t.Fatal(err)
			}
		}, StateNotEnrolled, "enrolled with the issuer", "not enrolled ("},
		{"group-readable", func() {
			if err := os.Chmod(path, 0o644); err != nil {
				t.Fatal(err)
			}
		}, StateEnrolmentUnusable, "has mode 0644: group or others may use it", "ENROLMENT UNUSABLE ("},
		{"removed", func() {
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
		}, StateNotEnrolled, "does not exist", "not enrolled ("},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.break_()
			sent := svc.directoryCount() + svc.tokenCount()
			pubs = nil
			for i := 0; i < 2; i++ {
				row := srv.refresh(t)
				if row.Status != users.DirectoryRefreshFailed || !strings.Contains(row.Error, tc.reason) {
					t.Fatalf("pull %d: %+v", i, row)
				}
			}
			if n := svc.directoryCount() + svc.tokenCount(); n != sent {
				t.Fatalf("%d requests sent without a usable enrolment", n-sent)
			}
			st := srv.rt.Status()
			if st.State != tc.state || !strings.Contains(st.Reason, tc.reason) {
				t.Fatalf("status %s %q", st.State, st.Reason)
			}
			if srv.owner(ada.fp) != "" || srv.owner(srv.opsFP) != "ops" {
				t.Fatal("the list was not withdrawn, or the local admin was")
			}
			if len(pubs) != 1 || pubs[0].Has("ada") || !pubs[0].Has("ops") {
				t.Fatalf("publications %d; want one, without ada", len(pubs))
			}
			if n := srv.log.count(tc.line); n != 1 {
				t.Errorf("%q logged %d times over two pulls:\n%s", tc.line, n, srv.log)
			}
			restore()
			if row := srv.refresh(t); row.Status != users.DirectoryRefreshGood || srv.owner(ada.fp) != "ada" {
				t.Fatalf("after the enrolment was put right: %+v", row)
			}
			if !strings.Contains(srv.log.String(), "enrolled as "+e.ClientID) {
				t.Errorf("no line for the enrolment put right:\n%s", srv.log)
			}
			srv.log.mu.Lock()
			srv.log.lines = nil
			srv.log.mu.Unlock()
		})
	}
}

// TestRuntimeFailureLogLines: each kind of failure is logged as section 3.10
// says: a refused token, an unavailable service or a timeout at every
// attempt; a disabled server (also when its answer arrives cut short), a
// rate limit, and a refused answer once per change of state.
func TestRuntimeFailureLogLines(t *testing.T) {
	clock := newFakeClock(rtT0)
	svc := newRTService(t, clock)
	dir := filepath.Join(t.TempDir(), "directory")
	e := svc.enrol(dir)
	opsLine, _ := newTestKey(t)
	svc.dir.setUsers(newPerson(t, "ada", false).served())
	srv := startRT(t, svc, dir, opsLine, nil)
	srv.refresh(t)

	twice := func(set func(), unset func(), hangs bool) []users.DirectoryRefresh {
		set()
		defer unset()
		if hangs {
			return []users.DirectoryRefresh{srv.refreshTimingOut(t, svc), srv.refreshTimingOut(t, svc)}
		}
		return []users.DirectoryRefresh{srv.refresh(t), srv.refresh(t)}
	}
	mode := func(m faultMode, ra string) func() { return func() { svc.setMode(m, ra) } }
	clearMode := mode(faultNone, "")
	for _, tc := range []struct {
		name  string
		set   func()
		unset func()
		line  string
		times int
	}{
		{"a disabled server's answer cut short", mode(faultCut403, ""), clearMode, "REFUSED: the account service answered HTTP 403 with no error this server could read", 1},
		{"unavailable", func() { svc.dir.setOutage(true) }, func() { svc.dir.setOutage(false) }, "the account service is unavailable or did not answer in time", 2},
		{"a disabled server", func() { svc.dir.setDisabled(e.ClientID, true) }, func() { svc.dir.setDisabled(e.ClientID, false) }, "SERVER DISABLED", 1},
		{"a refused token", mode(fault401, ""), clearMode, "refused this server's access token after a new login", 2},
		{"rate limited", mode(fault429, "120"), clearMode, "rate limited by the account service", 1},
		{"another contract", mode(faultContract2, ""), clearMode, "the account service's answer is refused", 1},
		{"a timeout", mode(faultHang, ""), clearMode, "did not answer in time", 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := srv.log.count(tc.line)
			for _, row := range twice(tc.set, tc.unset, tc.name == "a timeout") {
				if row.Status != users.DirectoryRefreshFailed {
					t.Fatalf("row %+v", row)
				}
			}
			if n := srv.log.count(tc.line) - before; n != tc.times {
				t.Errorf("%q logged %d times over two attempts, want %d:\n%s", tc.line, n, tc.times, srv.log)
			}
			// A good pull between cases, while the token is still held.
			if row := srv.refresh(t); row.Status != users.DirectoryRefreshGood {
				t.Fatalf("good pull after: %+v", row)
			}
		})
	}
	if !strings.Contains(srv.log.String(), "the service's clock") {
		t.Errorf("the refused token's line does not show both clocks:\n%s", srv.log)
	}
	noSecretsIn(t, "the log", srv.log.String(), svc.secrets())
}

// TestRuntimeGoodPullLines: a good pull is logged when its version changes,
// with the names added and removed and the admin flags changed; each
// skipped entry once per version, and an entry with a local admin's name at
// every pull. Names from the service are quoted.
func TestRuntimeGoodPullLines(t *testing.T) {
	clock := newFakeClock(rtT0)
	svc := newRTService(t, clock)
	dir := filepath.Join(t.TempDir(), "directory")
	svc.enrol(dir)
	opsLine, opsFP := newTestKey(t)
	ada, bob, cy := newPerson(t, "ada", false), newPerson(t, "bob", false), newPerson(t, "cy", false)
	impostor := newPerson(t, "ops", true)
	thief := &person{name: "dee", line: opsLine, fp: opsFP}
	svc.dir.setUsers(ada.served(), bob.served(), impostor.served(), thief.served())
	srv := startRT(t, svc, dir, opsLine, nil)

	for i := 0; i < 2; i++ {
		clock.advance(time.Minute)
		if row := srv.refresh(t); row.Status != users.DirectoryRefreshGood || row.Users != 2 || row.Skipped != 2 {
			t.Fatalf("pull %d: %+v", i, row)
		}
	}
	if n := srv.log.count("good pull: version"); n != 1 {
		t.Errorf("an unchanged list logged %d good-pull lines over two pulls", n)
	}
	if n := srv.log.count(`list entry "ops" skipped: its name is a local admin's`); n != 2 {
		t.Errorf("the local admin's name logged %d times over two pulls, want 2:\n%s", n, srv.log)
	}
	if n := srv.log.count(`list entry "dee" skipped: it lists a key a local admin holds`); n != 1 {
		t.Errorf("the local admin's key logged %d times over two pulls of one version, want 1:\n%s", n, srv.log)
	}
	if srv.owner(opsFP) != "ops" {
		t.Fatal("the local admin lost their key")
	}
	if u, _, _ := srv.rt.UserForKey(opsFP); !u.Admin {
		t.Fatal("the local admin changed")
	}
	if want := []Skip{{Name: "dee", Reason: SkipLocalAdminKey}, {Name: "ops", Reason: SkipLocalAdminName}}; !reflect.DeepEqual(srv.rt.Status().Skipped, want) {
		t.Errorf("status skipped %+v, want %+v", srv.rt.Status().Skipped, want)
	}

	bob.admin = true
	svc.dir.setUsers(bob.served(), cy.served())
	clock.advance(time.Minute)
	srv.refresh(t)
	if !strings.Contains(srv.log.String(), `; added "cy"; removed "ada"; now admin "bob"`) {
		t.Errorf("the change was not logged:\n%s", srv.log)
	}
	bob.admin = false
	svc.dir.setUsers(bob.served(), cy.served())
	clock.advance(time.Minute)
	srv.refresh(t)
	if !strings.Contains(srv.log.String(), `; no longer admin "bob"`) {
		t.Errorf("the demotion was not logged:\n%s", srv.log)
	}
}

// TestRuntimeRefreshDeadline: only the caller's deadline is an error; the
// pull carries on and its result applies.
func TestRuntimeRefreshDeadline(t *testing.T) {
	clock := newFakeClock(rtT0)
	svc := newRTService(t, clock)
	dir := filepath.Join(t.TempDir(), "directory")
	svc.enrol(dir)
	opsLine, _ := newTestKey(t)
	ada := newPerson(t, "ada", false)
	svc.dir.setUsers(ada.served())
	srv := startRT(t, svc, dir, opsLine, nil)
	gate := make(chan struct{})
	svc.setGate(gate)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := srv.rt.Refresh(ctx)
		done <- err
	}()
	<-svc.arrived
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("Refresh after its context ended: %v", err)
	}
	svc.setGate(nil)
	close(gate)
	if row := srv.refresh(t); row.Status != users.DirectoryRefreshGood || srv.owner(ada.fp) != "ada" {
		t.Fatalf("the next refresh: %+v", row)
	}
}
