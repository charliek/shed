package directory

import (
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/charliek/shed/internal/config"
	"github.com/charliek/shed/internal/users"
)

// TestUsersStatusInTheAPIsTerms: usersStatus carries every field of Status
// across, a zero instant as none, every user with its source, and every
// skipped entry with its reason in words; nothing else.
func TestUsersStatusInTheAPIsTerms(t *testing.T) {
	at := func(h int) time.Time { return rtT0.Add(time.Duration(h) * time.Hour) }
	ptr := func(tm time.Time) *time.Time { return &tm }
	st := Status{
		Issuer: "https://accounts.example.test", Organization: "example", ClientID: "srv_" + strings.Repeat("0a", 16),
		State: StateExpired, Reason: "the list's trust time ended at x",
		LastGoodPull: at(0), Version: strings.Repeat("ab", 32), TrustedUntil: at(4),
		LastAttempt: at(5), LastError: "the account service did not answer", NextAttempt: at(6),
		Copy: CopyStale, CopyError: "removing it failed",
		Users:   []StatusUser{{Name: "ada", Admin: true, Source: UserDirectory}, {Name: "ops", Admin: true, Source: UserLocal}},
		Skipped: []Skip{{Name: "ops", Reason: SkipLocalAdminName}, {Name: "", Reason: SkipMalformed}},
	}
	want := config.UsersStatusResponse{
		Source: config.UsersSourceDirectory,
		Directory: &config.DirectoryStatus{
			URL: st.Issuer, Organization: "example", ClientID: st.ClientID,
			State: "expired", Reason: st.Reason,
			LastGoodPull: ptr(at(0)), Version: st.Version, TrustedUntil: ptr(at(4)),
			LastAttempt: ptr(at(5)), LastError: st.LastError, NextAttempt: ptr(at(6)),
			Copy: "stale-on-disk", CopyError: "removing it failed",
			Skipped: []config.SkippedEntry{
				{Name: "ops", Reason: "local-admin-name", Description: skipWords(SkipLocalAdminName)},
				{Name: "", Reason: "malformed", Description: skipWords(SkipMalformed)},
			},
		},
		Users: []config.UserStatus{
			{Name: "ada", Admin: true, Source: config.UserFromDirectory},
			{Name: "ops", Admin: true, Source: config.UserFromLocal},
		},
	}
	if got := usersStatus(st); !reflect.DeepEqual(got, want) {
		t.Errorf("usersStatus =\n%+v\nwant\n%+v", got.Directory, want.Directory)
	}

	// Nothing yet: no instants, no users, no skips; the lists are empty, not
	// absent.
	got := usersStatus(Status{Issuer: "https://accounts.example.test", State: StateNotEnrolled, Reason: "no enrolment", Copy: CopyNone})
	d := got.Directory
	if d.LastGoodPull != nil || d.TrustedUntil != nil || d.LastAttempt != nil || d.NextAttempt != nil ||
		got.Users == nil || len(got.Users) != 0 || d.Skipped == nil || len(d.Skipped) != 0 || d.State != "not-enrolled" || d.Copy != "none" {
		t.Errorf("usersStatus of nothing yet = %+v %+v", got, d)
	}
}

// TestRuntimeUsersStatusStates: the runtime's UsersStatus in each state of
// the source, on the fake clock: not enrolled, an unusable enrolment, no
// copy, trusted (with an entry skipped for a local admin's name), and
// expired once the trust time passes; each equal to Status in the API's
// terms, and none holding a key or a label.
func TestRuntimeUsersStatusStates(t *testing.T) {
	check := func(t *testing.T, srv *rtServer, state string, keys ...string) config.UsersStatusResponse {
		t.Helper()
		got := srv.rt.UsersStatus()
		if got.Source != config.UsersSourceDirectory || got.Directory == nil || got.Directory.State != state {
			t.Fatalf("UsersStatus = %+v, want source directory, state %s", got, state)
		}
		if want := usersStatus(srv.rt.Status()); !reflect.DeepEqual(got, want) {
			t.Errorf("UsersStatus = %+v, want Status in the API's terms %+v", got, want)
		}
		text := strings.Join([]string{got.Directory.Reason, got.Directory.LastError, got.Directory.CopyError}, "\n")
		for _, u := range got.Users {
			text += "\n" + u.Name
		}
		for _, k := range append(keys, "laptop", "SHA256:") {
			if strings.Contains(text, k) {
				t.Errorf("UsersStatus holds a key or a label (%q)", k)
			}
		}
		return got
	}

	t.Run("not-enrolled", func(t *testing.T) {
		clock := newFakeClock(rtT0)
		svc := newRTService(t, clock)
		opsLine, _ := newTestKey(t)
		srv := startRT(t, svc, filepath.Join(t.TempDir(), "directory"), opsLine, nil)
		got := check(t, srv, config.DirectoryStateNotEnrolled, opsLine)
		if !strings.Contains(got.Directory.Reason, "does not exist") || got.Directory.ClientID != "" ||
			len(got.Users) != 1 || got.Users[0] != (config.UserStatus{Name: "ops", Admin: true, Source: config.UserFromLocal}) {
			t.Errorf("not enrolled: %+v %+v", got.Directory, got.Users)
		}
	})

	t.Run("enrolment-unusable", func(t *testing.T) {
		clock := newFakeClock(rtT0)
		svc := newRTService(t, clock)
		dir := filepath.Join(t.TempDir(), "directory")
		svc.enrol(dir)
		if err := os.Chmod(filepath.Join(dir, EnrolmentFileName), 0o640); err != nil {
			t.Fatal(err)
		}
		opsLine, _ := newTestKey(t)
		got := check(t, startRT(t, svc, dir, opsLine, nil), config.DirectoryStateEnrolmentUnusable, opsLine)
		if !strings.Contains(got.Directory.Reason, EnrolmentFileName) {
			t.Errorf("unusable enrolment reason %q", got.Directory.Reason)
		}
	})

	t.Run("no-copy, trusted, expired", func(t *testing.T) {
		clock := newFakeClock(rtT0)
		svc := newRTService(t, clock)
		dir := filepath.Join(t.TempDir(), "directory")
		e := svc.enrol(dir)
		opsLine, _ := newTestKey(t)
		ada := newPerson(t, "ada", true)
		shadow := newPerson(t, "ops", true)
		svc.dir.setUsers(ada.served(), shadow.served())
		srv := startRT(t, svc, dir, opsLine, nil)

		got := check(t, srv, config.DirectoryStateNoCopy, opsLine, ada.line, ada.fp, shadow.line, shadow.fp)
		if got.Directory.ClientID != e.ClientID || got.Directory.LastGoodPull != nil || got.Directory.Copy != "none" {
			t.Errorf("before the first pull: %+v", got.Directory)
		}

		if row := srv.refresh(t); row.Status != users.DirectoryRefreshGood {
			t.Fatalf("pull: %+v", row)
		}
		got = check(t, srv, config.DirectoryStateTrusted, opsLine, ada.line, ada.fp, shadow.line, shadow.fp)
		d := got.Directory
		if d.LastGoodPull == nil || !d.LastGoodPull.Equal(rtT0) || d.TrustedUntil == nil || !d.TrustedUntil.Equal(rtT0.Add(rtTrust)) ||
			d.NextAttempt == nil || !d.NextAttempt.Equal(rtT0.Add(rtRefresh)) || d.Copy != "kept" || !versionRx.MatchString(d.Version) || d.LastError != "" {
			t.Errorf("trusted: %+v", d)
		}
		wantUsers := []config.UserStatus{{Name: "ada", Admin: true, Source: config.UserFromDirectory}, {Name: "ops", Admin: true, Source: config.UserFromLocal}}
		if !reflect.DeepEqual(got.Users, wantUsers) {
			t.Errorf("trusted users = %+v, want %+v", got.Users, wantUsers)
		}
		if len(d.Skipped) != 1 || d.Skipped[0] != (config.SkippedEntry{Name: "ops", Reason: "local-admin-name", Description: skipWords(SkipLocalAdminName)}) {
			t.Errorf("skipped = %+v", d.Skipped)
		}

		clock.advance(rtTrust)
		got = check(t, srv, config.DirectoryStateExpired, opsLine, ada.line, ada.fp, shadow.line, shadow.fp)
		if !strings.Contains(got.Directory.Reason, "trust time ended") ||
			len(got.Users) != 1 || got.Users[0].Name != "ops" || got.Users[0].Source != config.UserFromLocal {
			t.Errorf("expired: %+v %+v", got.Directory, got.Users)
		}
	})
}

// listedIn reports whether st names name as a user of the list.
func listedIn(st Status, name string) bool {
	for _, u := range st.Users {
		if u.Name == name && u.Source == UserDirectory {
			return true
		}
	}
	return false
}

// TestRuntimeStatusOneSnapshotAsTheDeadlinePasses: Status loads the lookup's
// snapshot once and checks its trust time once, so the deadline passing
// while it runs never yields "trusted" beside local-only users (or the
// reverse). Forced: the clock moves past the deadline at any second load of
// the snapshot; and, in the one check, the clock reaches the deadline
// exactly (expired, the equality boundary).
func TestRuntimeStatusOneSnapshotAsTheDeadlinePasses(t *testing.T) {
	clock, svc, dir, opsLine, _ := rtSetup(t)
	srv := startRT(t, svc, dir, opsLine, nil)
	if row := srv.refresh(t); row.Status != users.DirectoryRefreshGood {
		t.Fatalf("pull: %+v", row)
	}
	deadline := rtT0.Add(rtTrust)
	clock.set(deadline.Add(-time.Second))

	loads := 0
	srv.rt.loaded = func() {
		loads++
		if loads == 2 {
			clock.set(deadline.Add(time.Hour))
		}
	}
	st := srv.rt.Status()
	srv.rt.loaded = nil
	if loads != 1 {
		t.Errorf("Status loaded the snapshot %d times, want once", loads)
	}
	if st.State != StateTrusted || !listedIn(st, "ada") || !st.TrustedUntil.Equal(deadline) {
		t.Errorf("status = %s with users %+v; want trusted with ada, coherent", st.State, st.Users)
	}

	// The one check of the trust time finds the clock at the deadline.
	clock.set(deadline.Add(-time.Second))
	srv.rt.latchRead = func() {
		srv.rt.latchRead = nil
		clock.set(deadline)
	}
	st = srv.rt.Status()
	srv.rt.latchRead = nil
	if st.State != StateExpired || listedIn(st, "ada") || len(st.Users) != 1 || !strings.Contains(st.Reason, stamp(deadline)) {
		t.Errorf("status at the deadline = %s (%s) with users %+v; want expired with the local admin only", st.State, st.Reason, st.Users)
	}
}

// TestRuntimeStatusOneSnapshotDuringARemoval: a removal published while
// Status runs never pairs one list's users with another's skip report or
// deadline. Forced: the next list (bob removed, the skipped entry gone) is
// published right after Status has loaded its snapshot.
func TestRuntimeStatusOneSnapshotDuringARemoval(t *testing.T) {
	clock, svc, dir, opsLine, ada := rtSetup(t)
	bob := newPerson(t, "bob", false)
	shadow := newPerson(t, "ops", true)
	svc.dir.setUsers(ada.served(), bob.served(), shadow.served())
	srv := startRT(t, svc, dir, opsLine, nil)
	if row := srv.refresh(t); row.Status != users.DirectoryRefreshGood {
		t.Fatalf("pull: %+v", row)
	}
	clock.advance(time.Minute)
	body := `{"contract":1,"organization":"` + worldOrg + `","version":"` + strings.Repeat("e", 64) + `","users":[` +
		`{"name":"ada","admin":false,"keys":[{"key":` + strconv.Quote(ada.line) + `,"label":"laptop"}]}]}`
	next, err := DecodeList([]byte(body), worldOrg)
	if err != nil {
		t.Fatal(err)
	}
	removedAt := clock.Now()
	srv.rt.loaded = func() {
		srv.rt.loaded = nil
		if ok, err := srv.rt.Install(next, removedAt, rtTrust, 1000); err != nil || !ok {
			t.Errorf("Install = %v, %v", ok, err)
		}
	}
	st := srv.rt.Status()
	srv.rt.loaded = nil
	if srv.owner(bob.fp) != "" {
		t.Fatal("the removal was not published")
	}
	// Status answers from the snapshot it loaded: bob, the entry skipped for
	// the local admin's name, and that list's deadline, together.
	if !listedIn(st, "bob") || len(st.Skipped) != 1 || st.Skipped[0].Name != "ops" || !st.TrustedUntil.Equal(rtT0.Add(rtTrust)) {
		t.Errorf("status during the removal: users %+v, skipped %+v, trusted until %s; want one snapshot's", st.Users, st.Skipped, st.TrustedUntil)
	}
	// The next status is the new list's, whole.
	st = srv.rt.Status()
	if listedIn(st, "bob") || len(st.Skipped) != 0 || !st.TrustedUntil.Equal(removedAt.Add(rtTrust)) {
		t.Errorf("status after the removal: users %+v, skipped %+v, trusted until %s", st.Users, st.Skipped, st.TrustedUntil)
	}
}

// TestRuntimeStatusDuringAGoodPull: a status read while a good pull
// publishes never pairs the new list's users with the previous version or
// pull time. Forced: from inside the publication, a status is asked for and
// given time to answer before the pull records the new version; it waits
// for the pull, and answers the new list with its version and time.
func TestRuntimeStatusDuringAGoodPull(t *testing.T) {
	clock, svc, dir, opsLine, ada := rtSetup(t)
	srv := startRT(t, svc, dir, opsLine, nil)
	first := srv.refresh(t)
	if first.Status != users.DirectoryRefreshGood {
		t.Fatalf("pull: %+v", first)
	}
	clock.advance(time.Minute)
	bob := newPerson(t, "bob", false)
	svc.dir.setUsers(ada.served(), bob.served())

	var once sync.Once
	got := make(chan Status, 1)
	srv.rt.SetPublicationHook(func(Publication) {
		once.Do(func() {
			answered := make(chan struct{})
			go func() {
				got <- srv.rt.Status()
				close(answered)
			}()
			select {
			case <-answered:
			case <-time.After(200 * time.Millisecond):
			}
		})
	})
	second := srv.refresh(t)
	srv.rt.SetPublicationHook(nil)
	if second.Status != users.DirectoryRefreshGood || second.Version == first.Version {
		t.Fatalf("second pull: %+v", second)
	}
	var st Status
	select {
	case st = <-got:
	case <-time.After(10 * time.Second):
		t.Fatal("no status")
	}
	if !listedIn(st, "bob") || st.Version != second.Version || !st.LastGoodPull.Equal(clock.Now()) || st.LastError != "" {
		t.Errorf("status during the pull: bob listed %v, version %s (the pull's %s), last good pull %s (want %s); one pull's, not two",
			listedIn(st, "bob"), st.Version, second.Version, st.LastGoodPull, clock.Now())
	}
}

// TestRuntimeStatusDuringAWithdrawal: a status read while a pull withdraws
// the list (the enrolment gone) shows the withdrawal with the state it is
// for, never the list gone under the enrolled state before ("no-copy").
// Forced as for a good pull: from inside the publication.
func TestRuntimeStatusDuringAWithdrawal(t *testing.T) {
	_, svc, dir, opsLine, _ := rtSetup(t)
	srv := startRT(t, svc, dir, opsLine, nil)
	if row := srv.refresh(t); row.Status != users.DirectoryRefreshGood {
		t.Fatalf("pull: %+v", row)
	}
	if err := os.Remove(filepath.Join(dir, EnrolmentFileName)); err != nil {
		t.Fatal(err)
	}
	var once sync.Once
	got := make(chan Status, 1)
	srv.rt.SetPublicationHook(func(Publication) {
		once.Do(func() {
			answered := make(chan struct{})
			go func() {
				got <- srv.rt.Status()
				close(answered)
			}()
			select {
			case <-answered:
			case <-time.After(200 * time.Millisecond):
			}
		})
	})
	row := srv.refresh(t)
	srv.rt.SetPublicationHook(nil)
	if row.List != users.DirectoryListWithdrawn {
		t.Fatalf("the withdrawing pull: %+v", row)
	}
	var st Status
	select {
	case st = <-got:
	case <-time.After(10 * time.Second):
		t.Fatal("no status")
	}
	if st.State != StateNotEnrolled || listedIn(st, "ada") || st.ClientID != "" {
		t.Errorf("status during the withdrawal: %s (%q), client id %q, users %+v; want not-enrolled with the local admin only", st.State, st.Reason, st.ClientID, st.Users)
	}
}

// TestRuntimeRefreshRowListInForce: the refresh row describes the list in
// force after the pull, users admitted: a failed pull inside the trust time
// keeps the list held (its version and counts); once the trust time has
// ended a failed pull's row presents no version and no users and says the
// trust time ended; the pull that withdraws the list says so, and the next
// says none is held; a kept copy refused at start for its trust time is
// trust-ended too. Status agrees each time.
func TestRuntimeRefreshRowListInForce(t *testing.T) {
	t.Run("failed inside, then past, the trust time", func(t *testing.T) {
		clock, svc, dir, opsLine, ada := rtSetup(t)
		shadow := newPerson(t, "ops", true)
		svc.dir.setUsers(ada.served(), shadow.served())
		srv := startRT(t, svc, dir, opsLine, nil)
		good := srv.refresh(t)
		if good.Status != users.DirectoryRefreshGood || good.List != users.DirectoryListInForce || good.Version == "" || good.Users != 1 || good.Skipped != 1 {
			t.Fatalf("good pull: %+v", good)
		}
		svc.dir.setOutage(true)
		clock.advance(rtTrust - time.Minute)
		row := srv.refresh(t)
		if row.Status != users.DirectoryRefreshFailed || row.List != users.DirectoryListInForce || row.Version != good.Version || row.Users != 1 || row.Skipped != 1 {
			t.Errorf("failed inside the trust time: %+v", row)
		}
		clock.set(rtT0.Add(rtTrust))
		row = srv.refresh(t)
		want := users.DirectoryRefresh{Status: users.DirectoryRefreshFailed, List: users.DirectoryListTrustEnded, Error: row.Error}
		if row != want || row.Error == "" {
			t.Errorf("failed past the trust time: %+v, want %+v", row, want)
		}
		if st := srv.rt.Status(); st.State != StateExpired || listedIn(st, "ada") {
			t.Errorf("status past the trust time: %s %+v", st.State, st.Users)
		}
		// A good pull brings a list in force again.
		svc.dir.setOutage(false)
		if row := srv.refresh(t); row.List != users.DirectoryListInForce || row.Users != 1 {
			t.Errorf("good pull after the trust time ended: %+v", row)
		}
	})

	t.Run("withdrawn, then none", func(t *testing.T) {
		_, svc, dir, opsLine, _ := rtSetup(t)
		srv := startRT(t, svc, dir, opsLine, nil)
		if row := srv.refresh(t); row.List != users.DirectoryListInForce {
			t.Fatalf("good pull: %+v", row)
		}
		if err := os.Remove(filepath.Join(dir, EnrolmentFileName)); err != nil {
			t.Fatal(err)
		}
		row := srv.refresh(t)
		if row.Status != users.DirectoryRefreshFailed || row.List != users.DirectoryListWithdrawn || row.Version != "" || row.Users != 0 || row.Skipped != 0 {
			t.Errorf("the pull that withdrew the list: %+v", row)
		}
		row = srv.refresh(t)
		if row.List != users.DirectoryListNone || row.Version != "" || row.Users != 0 {
			t.Errorf("the next pull: %+v", row)
		}
		if st := srv.rt.Status(); st.State != StateNotEnrolled || len(st.Users) != 1 {
			t.Errorf("status: %s %+v", st.State, st.Users)
		}
	})

	t.Run("a kept copy past its trust time", func(t *testing.T) {
		clock, svc, dir, opsLine, _ := rtSetup(t)
		first := startRT(t, svc, dir, opsLine, nil)
		if row := first.refresh(t); row.List != users.DirectoryListInForce {
			t.Fatalf("good pull: %+v", row)
		}
		first.stop()
		clock.advance(rtTrust + time.Minute)
		svc.dir.setOutage(true)
		srv := startRT(t, svc, dir, opsLine, nil)
		row := srv.refresh(t)
		if row.Status != users.DirectoryRefreshFailed || row.List != users.DirectoryListTrustEnded || row.Version != "" || row.Users != 0 {
			t.Errorf("a failed pull after a copy refused for its trust time: %+v", row)
		}
		if st := srv.rt.Status(); st.State != StateExpired {
			t.Errorf("status: %s", st.State)
		}
	})
}
