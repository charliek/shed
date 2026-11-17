package directory

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/charliek/shed/internal/users"
)

// The kept copy's tests: the order of write and publication, the failures
// around them, the end of the trust time made durable, and the checks of a
// copy found at start.

// pullAsync runs an admin's refresh in the background; the channel gets its
// row.
func pullAsync(s *rtServer) <-chan users.DirectoryRefresh {
	rows := make(chan users.DirectoryRefresh, 1)
	go func() {
		rep, err := s.rt.Refresh(context.Background())
		if err != nil || rep.Directory == nil {
			rows <- users.DirectoryRefresh{Error: "refresh: " + errString(err)}
			return
		}
		rows <- *rep.Directory
	}()
	return rows
}

func errString(err error) string {
	if err == nil {
		return "no row"
	}
	return err.Error()
}

// rtSetup is a state directory enrolled with a fresh service, ada listed,
// and the local admin's key.
func rtSetup(t *testing.T) (*fakeClock, *rtService, string, string, *person) {
	t.Helper()
	clock := newFakeClock(rtT0)
	svc := newRTService(t, clock)
	dir := filepath.Join(t.TempDir(), "directory")
	svc.enrol(dir)
	opsLine, _ := newTestKey(t)
	ada := newPerson(t, "ada", false)
	svc.dir.setUsers(ada.served())
	return clock, svc, dir, opsLine, ada
}

// TestRuntimeCrashBetweenWriteAndPublishLoadsTheCopy pins the INTENDED
// behaviour of the crash window of write-then-publish: a server that stops
// after a good pull's copy is written and before its list is published
// leaves the service's own, fully checked answer on disk, and the next start
// loads it, newly listed person included. (Write, then publish only keeps
// the copy from being OLDER than the list published.)
func TestRuntimeCrashBetweenWriteAndPublishLoadsTheCopy(t *testing.T) {
	_, svc, dir, opsLine, ada := rtSetup(t)
	bob := newPerson(t, "bob", false)
	written, release := make(chan struct{}), make(chan struct{})
	var hold atomic.Bool
	first := startRT(t, svc, dir, opsLine, func(rt *Runtime) {
		write := rt.writeCopy
		rt.writeCopy = func(d *StateDir, data []byte) error {
			err := write(d, data)
			if hold.Load() {
				close(written)
				<-release // the server "dies" here: the list is never published
			}
			return err
		}
	})
	var freed sync.Once
	free := func() { freed.Do(func() { close(release) }) }
	t.Cleanup(free) // runs before the runtime's stop, so a failure cannot hang it
	if row := first.refresh(t); row.Status != users.DirectoryRefreshGood {
		t.Fatalf("set-up pull: %+v", row)
	}
	svc.dir.setUsers(ada.served(), bob.served())
	hold.Store(true)
	rows := pullAsync(first)
	select {
	case <-written:
	case <-time.After(10 * time.Second):
		t.Fatal("the copy was not written")
	}
	if first.owner(bob.fp) != "" {
		t.Fatal("bob was published before the write returned")
	}

	// A fresh start on the same state directory, the service unreachable:
	// it loads the copy written for the list never published.
	svc.dir.setOutage(true)
	next := startRT(t, svc, dir, opsLine, nil)
	if next.owner(bob.fp) != "bob" || next.owner(ada.fp) != "ada" || next.rt.Status().Copy != CopyKept {
		t.Fatalf("the copy written before the crash was not loaded: bob %q ada %q %+v", next.owner(bob.fp), next.owner(ada.fp), next.rt.Status())
	}
	free()
	<-rows
}

// TestRuntimeRefusedPublicationRemovesTheNewCopy: a good pull whose list the
// lookup refuses to publish (here, the clock stepped back while the pull
// ran) removes the copy it just wrote, so the next start does not prefer it.
// When that removal fails, status says the copy on disk is stale, and the
// log names the new, unpublished copy as the one that remains.
func TestRuntimeRefusedPublicationRemovesTheNewCopy(t *testing.T) {
	for _, removalFails := range []bool{false, true} {
		name := "removed"
		if removalFails {
			name = "the removal fails"
		}
		t.Run(name, func(t *testing.T) {
			clock, svc, dir, opsLine, ada := rtSetup(t)
			bob := newPerson(t, "bob", false)
			var stepBack, failRemove atomic.Bool
			srv := startRT(t, svc, dir, opsLine, func(rt *Runtime) {
				write, remove := rt.writeCopy, rt.removeCopy
				rt.writeCopy = func(d *StateDir, data []byte) error {
					err := write(d, data)
					if stepBack.Load() {
						clock.set(clock.Now().Add(-time.Second))
					}
					return err
				}
				rt.removeCopy = func(d *StateDir) error {
					if failRemove.Load() {
						return errors.New("read-only file system")
					}
					return remove(d)
				}
			})
			first := srv.refresh(t)
			if first.Status != users.DirectoryRefreshGood {
				t.Fatalf("set-up pull: %+v", first)
			}
			clock.advance(time.Minute)
			svc.dir.setUsers(ada.served(), bob.served())
			stepBack.Store(true)
			failRemove.Store(removalFails)
			row := srv.refresh(t)
			stepBack.Store(false)
			if row.Status != users.DirectoryRefreshFailed || !strings.Contains(row.Error, "installing the list") || row.Version != first.Version {
				t.Fatalf("the refused publication's row: %+v", row)
			}
			if srv.owner(bob.fp) != "" || srv.owner(ada.fp) != "ada" {
				t.Fatal("the published list changed")
			}
			st := srv.rt.Status()
			if !removalFails {
				if copyBytes(t, dir) != nil {
					t.Fatal("the new, unpublished copy was not removed")
				}
				if st.Copy != CopyNotWritten || !strings.Contains(st.CopyError, "the list was not published") {
					t.Fatalf("status: copy %s %q", st.Copy, st.CopyError)
				}
				srv.stop()
				clock.advance(time.Minute)
				again := startRT(t, svc, dir, opsLine, nil)
				if again.owner(bob.fp) != "" || again.owner(ada.fp) != "" || again.owner(again.opsFP) != "ops" {
					t.Fatal("after the removal a restart did not start with only the local admins")
				}
				return
			}
			want := "the new, unpublished copy remains on disk: removing it failed: read-only file system"
			if st.Copy != CopyStale || !strings.Contains(st.CopyError, want) {
				t.Fatalf("status: copy %s %q; want stale-on-disk naming the new, unpublished copy", st.Copy, st.CopyError)
			}
			if !strings.Contains(srv.log.String(), "WARNING: the copy on disk is stale (") || !strings.Contains(srv.log.String(), want) {
				t.Errorf("log:\n%s\nwant the stale warning naming the new, unpublished copy", srv.log)
			}
			if strings.Contains(srv.log.String(), "the old copy") {
				t.Errorf("the log calls the copy that remains the old one:\n%s", srv.log)
			}
			if f := readCopy(t, dir); f.Version == first.Version {
				t.Fatal("the copy on disk is not the new, unpublished one")
			}
		})
	}
}

// TestRuntimeCopyIsOnDiskWhenTheListIsPublished: at the instant a good
// pull's list is published, its copy is already on disk (write, then
// publish), for the first pull and for a changed list.
func TestRuntimeCopyIsOnDiskWhenTheListIsPublished(t *testing.T) {
	_, svc, dir, opsLine, ada := rtSetup(t)
	srv := startRT(t, svc, dir, opsLine, nil)
	var mu sync.Mutex
	var onDisk []string
	srv.rt.SetPublicationHook(func(Publication) {
		v := "none"
		if b, err := os.ReadFile(filepath.Join(dir, CopyFileName)); err == nil {
			var f copyFile
			if json.Unmarshal(b, &f) == nil {
				v = f.Version
			}
		}
		mu.Lock()
		onDisk = append(onDisk, v)
		mu.Unlock()
	})
	var published []string
	row := srv.refresh(t)
	published = append(published, row.Version)
	svc.dir.setUsers(ada.served(), newPerson(t, "bob", false).served())
	row = srv.refresh(t)
	published = append(published, row.Version)
	mu.Lock()
	defer mu.Unlock()
	if len(onDisk) != 2 || onDisk[0] != published[0] || onDisk[1] != published[1] {
		t.Fatalf("the copy on disk at each publication: %v; the lists published: %v", onDisk, published)
	}
}

// failSyncs makes the state directory's syncs fail after the rename, link or
// unlink before them has happened: the first n of them when n > 0, every one
// when n < 0.
func failSyncs(t *testing.T, n int) {
	t.Helper()
	var calls atomic.Int32
	hookBeforeDirSync = func() error {
		if c := int(calls.Add(1)); n < 0 || c <= n {
			return errors.New("input/output error")
		}
		return nil
	}
	t.Cleanup(func() { hookBeforeDirSync = nil })
}

// TestRuntimeCopySyncFailures: failures after the file system work. The
// write's rename done and its directory sync failing is a write that failed:
// the copy is removed, so a restart has only the local admins. The removal's
// unlink done and its sync failing is reported, conservatively, as stale on
// disk, though the copy is gone: a restart has only the local admins.
func TestRuntimeCopySyncFailures(t *testing.T) {
	for _, tc := range []struct {
		name     string
		failures int
		copy     CopyCondition
		why      string
	}{
		{"rename done, the directory sync fails", 1, CopyNotWritten, "sync directory "},
		{"unlink done, its sync fails", -1, CopyStale, "the copy on disk remains: removing it failed: sync directory "},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clock, svc, dir, opsLine, ada := rtSetup(t)
			bob := newPerson(t, "bob", false)
			svc.dir.setUsers(ada.served(), bob.served())
			srv := startRT(t, svc, dir, opsLine, nil)
			if row := srv.refresh(t); row.Status != users.DirectoryRefreshGood || copyBytes(t, dir) == nil {
				t.Fatalf("set-up pull: %+v", row)
			}
			clock.advance(time.Minute)
			svc.dir.setUsers(ada.served()) // bob leaves
			failSyncs(t, tc.failures)
			if row := srv.refresh(t); row.Status != users.DirectoryRefreshGood || srv.owner(bob.fp) != "" {
				t.Fatalf("pull: %+v", row)
			}
			hookBeforeDirSync = nil
			if st := srv.rt.Status(); st.Copy != tc.copy || !strings.Contains(st.CopyError, tc.why) {
				t.Fatalf("status: copy %s %q; want %s holding %q", st.Copy, st.CopyError, tc.copy, tc.why)
			}
			if copyBytes(t, dir) != nil {
				t.Fatal("a copy remains on disk")
			}
			srv.stop()
			again := startRT(t, svc, dir, opsLine, nil)
			if again.owner(bob.fp) != "" || again.owner(ada.fp) != "" || again.owner(again.opsFP) != "ops" {
				t.Fatal("a restart did not start with only the local admins")
			}
		})
	}
}

// expire moves the clock to at, past the deadline of the list s holds, and
// has its trust watch check twice (the second tick waits for the first's
// check and removal to end). The service is made unreachable first, so the
// pull the schedule then starts fails and the list is not restored.
func (s *rtServer) expire(svc *rtService, clock *fakeClock, at time.Time) {
	svc.dir.setOutage(true)
	clock.set(at)
	s.ticks <- at
	s.ticks <- at
}

// TestRuntimeTrustEndRemovesTheCopy: the end of the trust time is made
// durable. When it ends the kept copy is removed, so a restart whose clock
// reads before that end (moved back) with the service unreachable starts
// with only the local admins. Status keeps the last good pull and its
// version; a good pull in the same run logs that trust is restored, and the
// next end removes the new copy again.
func TestRuntimeTrustEndRemovesTheCopy(t *testing.T) {
	clock, svc, dir, opsLine, ada := rtSetup(t)
	srv := startRT(t, svc, dir, opsLine, nil)
	first := srv.refresh(t)
	srv.start()
	srv.expire(svc, clock, rtT0.Add(rtTrust))
	st := srv.rt.Status()
	if st.State != StateExpired || st.Copy != CopyNone || !strings.Contains(st.CopyError, "removed: the trust time of its list ended at 2026-11-16T13:00:00Z") ||
		!st.LastGoodPull.Equal(rtT0) || st.Version != first.Version {
		t.Fatalf("status at the end of the trust time: %+v", st)
	}
	if copyBytes(t, dir) != nil {
		t.Fatal("the kept copy was not removed when the trust time ended")
	}

	// A good pull restores the list (and writes a copy); its end removes it.
	restoredAt := rtT0.Add(rtTrust + time.Minute)
	svc.dir.setOutage(false)
	clock.set(restoredAt)
	if row := srv.refresh(t); row.Status != users.DirectoryRefreshGood || copyBytes(t, dir) == nil || srv.owner(ada.fp) != "ada" {
		t.Fatalf("the restoring pull: %+v", row)
	}
	if !strings.Contains(srv.log.String(), "trust restored") {
		t.Errorf("no trust restored line:\n%s", srv.log)
	}
	srv.expire(svc, clock, restoredAt.Add(rtTrust))
	if copyBytes(t, dir) != nil {
		t.Fatal("the second end did not remove the copy")
	}
	if n := srv.log.count("kept copy removed: the trust time of its list ended at"); n != 2 {
		t.Errorf("%d removal lines over two ends:\n%s", n, srv.log)
	}
	srv.stop()

	// The clock moved back before the last end; the service unreachable.
	clock.set(restoredAt.Add(rtTrust - time.Hour))
	svc.dir.setOutage(true)
	again := startRT(t, svc, dir, opsLine, nil)
	if again.owner(ada.fp) != "" || again.owner(again.opsFP) != "ops" {
		t.Fatal("a restart with the clock moved back admitted a list user after the trust time ended")
	}
	if lines := strings.Join(again.rt.StartupLines(), "\n"); !strings.Contains(lines, "Users: no kept copy;") {
		t.Fatalf("start-up lines:\n%s", lines)
	}
}

// TestRuntimeTrustEndRemovalFails covers the removal at the end of the trust
// time failing: status says stale on disk, the loud line is logged once per
// change, the removal is tried again at each tick and each turn of the
// schedule until it works.
func TestRuntimeTrustEndRemovalFails(t *testing.T) {
	t.Run("retried until it works", func(t *testing.T) {
		clock, svc, dir, opsLine, _ := rtSetup(t)
		var fail atomic.Bool
		var tries atomic.Int32
		srv := startRT(t, svc, dir, opsLine, func(rt *Runtime) {
			remove := rt.removeCopy
			rt.removeCopy = func(d *StateDir) error {
				tries.Add(1)
				if fail.Load() {
					return errors.New("read-only file system")
				}
				return remove(d)
			}
		})
		srv.refresh(t)
		fail.Store(true)
		srv.start()
		srv.expire(svc, clock, rtT0.Add(rtTrust))
		srv.ticks <- clock.Now()
		if n := tries.Load(); n < 2 {
			t.Fatalf("the removal was tried %d times over three ticks", n)
		}
		st := srv.rt.Status()
		if st.Copy != CopyStale || !strings.Contains(st.CopyError, "removing it failed: read-only file system") {
			t.Fatalf("status: copy %s %q", st.Copy, st.CopyError)
		}
		if n := srv.log.count("WARNING: the copy on disk is stale (the trust time of its list ended at 2026-11-16T13:00:00Z"); n != 1 {
			t.Errorf("the failed removal was logged %d times, want once:\n%s", n, srv.log)
		}
		if copyBytes(t, dir) == nil {
			t.Fatal("the copy is gone though its removal failed")
		}
		fail.Store(false)
		srv.ticks <- clock.Now()
		srv.ticks <- clock.Now()
		if st := srv.rt.Status(); st.Copy != CopyNone || copyBytes(t, dir) != nil {
			t.Fatalf("the retried removal: copy %s %q", st.Copy, st.CopyError)
		}
		if n := srv.log.count("kept copy removed"); n != 1 {
			t.Errorf("the removal that worked was logged %d times:\n%s", n, srv.log)
		}
	})

	// The documented residual case: when the copy cannot be removed, a
	// restart whose clock reads before the end (moved back) brings its list
	// back for what remains, by that clock, of its trust time.
	t.Run("residual: removal failing, a restart with the clock moved back brings the list back", func(t *testing.T) {
		clock, svc, dir, opsLine, ada := rtSetup(t)
		fail := func(rt *Runtime) {
			rt.removeCopy = func(*StateDir) error { return errors.New("read-only file system") }
		}
		srv := startRT(t, svc, dir, opsLine, fail)
		srv.refresh(t)
		srv.start()
		srv.expire(svc, clock, rtT0.Add(rtTrust))
		if srv.rt.Status().Copy != CopyStale {
			t.Fatalf("status: %+v", srv.rt.Status())
		}
		srv.stop()
		clock.set(rtT0.Add(rtTrust - time.Hour))
		svc.dir.setOutage(true)
		again := startRT(t, svc, dir, opsLine, fail)
		if again.owner(ada.fp) != "ada" {
			t.Fatal("the stale copy was not used (the documented residual case)")
		}
	})
}

// TestRuntimePastTrustCopyAtStart: a kept copy found past its trust time at
// start is not used and is removed; the last good pull and its version are
// still reported. A later restart with the clock moved back has only the
// local admins. When the removal fails, status says so.
func TestRuntimePastTrustCopyAtStart(t *testing.T) {
	for _, removalFails := range []bool{false, true} {
		name := "removed"
		if removalFails {
			name = "the removal fails"
		}
		t.Run(name, func(t *testing.T) {
			clock, svc, dir, opsLine, ada := rtSetup(t)
			first := startRT(t, svc, dir, opsLine, nil)
			row := first.refresh(t)
			first.stop()
			clock.set(rtT0.Add(rtTrust + time.Minute))
			svc.dir.setOutage(true)
			var adjust func(*Runtime)
			if removalFails {
				adjust = func(rt *Runtime) {
					rt.removeCopy = func(*StateDir) error { return errors.New("read-only file system") }
				}
			}
			srv := startRT(t, svc, dir, opsLine, adjust)
			st := srv.rt.Status()
			if st.State != StateExpired || !st.LastGoodPull.Equal(rtT0) || st.Version != row.Version || !st.TrustedUntil.Equal(rtT0.Add(rtTrust)) || srv.owner(ada.fp) != "" {
				t.Fatalf("status at start: %+v", st)
			}
			if removalFails {
				if st.Copy != CopyStale || copyBytes(t, dir) == nil || srv.log.count("WARNING: the copy on disk is stale") != 1 {
					t.Fatalf("copy %s %q, log:\n%s", st.Copy, st.CopyError, srv.log)
				}
				return
			}
			if st.Copy != CopyNone || copyBytes(t, dir) != nil {
				t.Fatalf("the past-trust copy was not removed: %s %q", st.Copy, st.CopyError)
			}
			srv.stop()
			clock.set(rtT0.Add(rtTrust - time.Hour))
			again := startRT(t, svc, dir, opsLine, nil)
			if again.owner(ada.fp) != "" || again.owner(again.opsFP) != "ops" {
				t.Fatal("a restart with the clock moved back admitted a list user")
			}
		})
	}
}

// TestRuntimeRestartUnderAnotherTrust: at start the copy's trust time is
// counted with today's trust setting, shorter or longer than the one it was
// written under.
func TestRuntimeRestartUnderAnotherTrust(t *testing.T) {
	for _, tc := range []struct {
		name    string
		trust   time.Duration
		after   time.Duration
		trusted bool
	}{
		{"shorter: past it", 2 * time.Hour, 3 * time.Hour, false},
		{"shorter: inside it", 2 * time.Hour, time.Hour, true},
		{"longer: inside it", 8 * time.Hour, 5 * time.Hour, true},
		{"longer: past it", 8 * time.Hour, 8 * time.Hour, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clock, svc, dir, opsLine, ada := rtSetup(t)
			first := startRT(t, svc, dir, opsLine, nil)
			first.refresh(t)
			first.stop()
			clock.set(rtT0.Add(tc.after))
			svc.dir.setOutage(true)
			srv := startRT(t, svc, dir, opsLine, func(rt *Runtime) { rt.trust = tc.trust })
			st := srv.rt.Status()
			if got := srv.owner(ada.fp) == "ada"; got != tc.trusted {
				t.Fatalf("ada resolves %v, want %v (%+v)", got, tc.trusted, st)
			}
			if !st.TrustedUntil.Equal(rtT0.Add(tc.trust)) {
				t.Fatalf("trusted until %s, want %s", stamp(st.TrustedUntil), stamp(rtT0.Add(tc.trust)))
			}
			if want := StateTrusted; tc.trusted && st.State != want || !tc.trusted && st.State != StateExpired {
				t.Fatalf("state %s", st.State)
			}
		})
	}
}

// TestRuntimeCopyEnvelopeRefused: a kept copy whose envelope or body fails a
// check is not used at start, and the check is named in the log.
func TestRuntimeCopyEnvelopeRefused(t *testing.T) {
	clock, svc, dir, opsLine, ada := rtSetup(t)
	first := startRT(t, svc, dir, opsLine, nil)
	first.refresh(t)
	first.stop()
	good := copyBytes(t, dir)
	var env map[string]any
	if err := json.Unmarshal(good, &env); err != nil {
		t.Fatal(err)
	}
	body, err := base64.StdEncoding.DecodeString(env["body"].(string))
	if err != nil {
		t.Fatal(err)
	}
	with := func(edit func(m map[string]any)) []byte {
		m := map[string]any{}
		for k, v := range env {
			m[k] = v
		}
		edit(m)
		out, err := json.Marshal(m)
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	bodyWith := func(from, to string) string {
		if !strings.Contains(string(body), from) {
			t.Fatalf("the body holds no %q", from)
		}
		return base64.StdEncoding.EncodeToString([]byte(strings.Replace(string(body), from, to, 1)))
	}
	clock.advance(time.Minute)
	for _, tc := range []struct {
		name string
		data []byte
		why  string
	}{
		{"wrong format", with(func(m map[string]any) { m["format"] = 2 }), "format 2 is not 1"},
		{"no fetched_at member", with(func(m map[string]any) { delete(m, "fetched_at") }), "it has no fetched_at"},
		{"a zero fetched_at", with(func(m map[string]any) { m["fetched_at"] = time.Time{} }), "it has no fetched_at"},
		{"a member named twice", []byte(strings.Replace(string(good), `"format": 1,`, `"format": 1, "format": 1,`, 1)), "not a kept copy"},
		{"malformed base64", with(func(m map[string]any) { m["body"] = "!!not base64!!" }), "its body is not base64"},
		{"non-canonical base64", with(func(m map[string]any) { m["body"] = "QR==" }), "its body is not base64"},
		{"a decoded body over the size limit", with(func(m map[string]any) {
			m["body"] = base64.StdEncoding.EncodeToString(make([]byte, MaxDirectoryBody+1))
		}), "its body is over the"},
		{"envelope and body versions disagree", with(func(m map[string]any) { m["version"] = strings.Repeat("b", 64) }), "its body's version is not the copy's"},
		{"the body names another organization", with(func(m map[string]any) {
			m["body"] = bodyWith(`"organization":"`+worldOrg+`"`, `"organization":"other"`)
		}), "its body is refused"},
		{"the envelope names another organization", with(func(m map[string]any) { m["organization"] = "other" }), "names the organization"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := os.WriteFile(filepath.Join(dir, CopyFileName), tc.data, 0o600); err != nil {
				t.Fatal(err)
			}
			srv := startRT(t, svc, dir, opsLine, nil)
			st := srv.rt.Status()
			if st.Copy != CopyUnused || srv.owner(ada.fp) != "" || !strings.Contains(st.CopyError, tc.why) {
				t.Fatalf("copy %s %q, ada %q; want unused because %q", st.Copy, st.CopyError, srv.owner(ada.fp), tc.why)
			}
			if !strings.Contains(srv.log.String(), "kept copy not used: ") || !strings.Contains(srv.log.String(), tc.why) {
				t.Errorf("log:\n%s", srv.log)
			}
		})
	}
	// The unedited copy is used: the refusals above are the edits'.
	if err := os.WriteFile(filepath.Join(dir, CopyFileName), good, 0o600); err != nil {
		t.Fatal(err)
	}
	if srv := startRT(t, svc, dir, opsLine, nil); srv.owner(ada.fp) != "ada" {
		t.Fatal("the unedited copy was not used")
	}
}

// TestRuntimeEnrolmentForAnotherOrganization: an enrolment naming another
// organization than the configured one is not enrolled for this server:
// nothing is sent, and only the local admins can log in.
func TestRuntimeEnrolmentForAnotherOrganization(t *testing.T) {
	_, svc, dir, opsLine, ada := rtSetup(t)
	first := startRT(t, svc, dir, opsLine, nil)
	first.refresh(t)
	first.stop()
	sent := svc.directoryCount() + svc.tokenCount()
	srv := startRT(t, svc, dir, opsLine, func(rt *Runtime) { rt.organization = "other" })
	st := srv.rt.Status()
	if st.State != StateNotEnrolled || !strings.Contains(st.Reason, `enrolled in the organization "`+worldOrg+`", not the configured "other"`) {
		t.Fatalf("status: %s %q", st.State, st.Reason)
	}
	if srv.owner(ada.fp) != "" || srv.owner(srv.opsFP) != "ops" {
		t.Fatal("the copy was used, or the local admin refused")
	}
	if row := srv.refresh(t); row.Status != users.DirectoryRefreshFailed || !strings.Contains(row.Error, "not enrolled") {
		t.Fatalf("pull: %+v", row)
	}
	if n := svc.directoryCount() + svc.tokenCount(); n != sent {
		t.Fatalf("%d requests sent under an enrolment for another organization", n-sent)
	}
}
