package users

import (
	"context"
	"sync"
	"testing"
	"time"
)

// openLogins builds a second server's lookup over the fixture's state
// directory (the same pin store and key cache) following logins.
func (f *pinFixture) openLogins(t *testing.T, logins ...string) *ConfigLookup {
	t.Helper()
	saved := f.logins
	f.logins = logins
	defer func() { f.logins = saved }()
	return f.open(t)
}

// TestTwoServersKeepEachOthersPins: servers A and B share one state
// directory (as the parallel dev server and the installed one do), each
// following its own login. A pins hubot; B starts, pins octocat; A then
// writes the store again (hubot's keys changed). B's pin survives A's write,
// and a restarted B still finds octocat pinned, so a different account under
// that login is refused.
func TestTwoServersKeepEachOthersPins(t *testing.T) {
	_, hubotLine := genKey(t)
	_, hubotLine2 := genKey(t)
	_, octoLine := genKey(t)
	impostor, impostorLine := genKey(t)
	f := newPinFixture(t)
	f.gh.set("hubot", 1001, hubotLine)
	f.gh.set("octocat", 2002, octoLine)

	a := f.openLogins(t, "hubot")
	_ = f.openLogins(t, "octocat")
	if got := f.storedPins(t); got["hubot"].ID != 1001 || got["octocat"].ID != 2002 {
		t.Fatalf("pins after both starts: %+v", got)
	}

	f.gh.set("hubot", 1001, hubotLine+hubotLine2)
	if _, err := a.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	got := f.storedPins(t)
	if got["octocat"].ID != 2002 {
		t.Fatalf("A's write lost B's pin of octocat: %+v", got)
	}
	if got["hubot"].ID != 1001 || got["hubot"].KeysSHA256 != keysDigest([]byte(hubotLine+hubotLine2)) {
		t.Errorf("A's own change was not written: %+v", got["hubot"])
	}

	// octocat now names another account: B, restarted, refuses it.
	f.gh.set("octocat", 3003, impostorLine)
	b := f.openLogins(t, "octocat")
	if has(b, impostor) {
		t.Error("a restarted B accepted another account's key under octocat")
	}
	if got := f.storedPins(t)["octocat"]; got.ID != 2002 {
		t.Errorf("octocat's pin after B's restart: %+v, want 2002", got)
	}
}

// TestPinConflictIsAMismatchNotLastWriterWins: servers A and B share one
// state directory and both follow octocat, which neither has pinned (GitHub
// was down when they started). A's fetch finds account 2; while it is in
// flight, B's fetch finds account 1 (the login was renamed between the two)
// and pins it. A then decides under the store's lock: the store pins 1, A
// found 2, so A's result is a mismatch, its keys are not accepted, and the
// pin stays 1.
func TestPinConflictIsAMismatchNotLastWriterWins(t *testing.T) {
	k1, line1 := genKey(t)
	k2, line2 := genKey(t)
	f := newPinFixture(t)
	f.outage()
	a := f.openLogins(t, "octocat")
	b := f.openLogins(t, "octocat")
	if len(f.storedPins(t)) != 0 {
		t.Fatalf("pins after starting during an outage: %+v", f.storedPins(t))
	}
	f.gh.setAPIStatus(0, nil)
	f.gh.setKeysStatus(0)
	f.gh.set("octocat", 2, line2)
	f.gh.setAccountKeys(1, line1)

	var once sync.Once
	f.gh.setOnIDKeys(func(id int64) {
		if id != 2 {
			return
		}
		once.Do(func() {
			// B's fetch, between A's account lookup and A's decision.
			f.gh.set("octocat", 1, line1)
			if _, err := b.Refresh(context.Background()); err != nil {
				t.Error(err)
			}
			f.gh.set("octocat", 2, line2)
		})
	})
	rows, err := a.Refresh(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got := f.storedPins(t)["octocat"]; got.ID != 1 {
		t.Fatalf("octocat's pin after both fetches: %+v, want 1 (B's)", got)
	}
	if !has(b, k1) {
		t.Error("B did not accept the key of the account it pinned")
	}
	if len(rows.Logins) != 1 || rows.Logins[0].Status != RefreshMismatch || rows.Logins[0].PinnedID != 1 || rows.Logins[0].SeenID != 2 {
		t.Errorf("A's row: %+v, want a mismatch against pin 1, seen 2", rows.Logins)
	}
	if has(a, k2) {
		t.Error("A accepted the key of the account it found, against the stored pin")
	}
	if _, _, ok := a.UserForKey(fp(k1)); ok {
		t.Error("A accepted a key it never fetched")
	}
}

// TestPinWritersExcludeEachOther: two writers of one pin store really exclude
// each other. Writer A (another server sharing the state directory, as
// lockPinStore, loadPins and the durable write do it) holds the store's lock;
// server B's refresh, which has a changed key list to write for octocat,
// blocks until A releases it. While B waits A writes hubot's new pin; once A
// unlocks, B's write keeps it, because B read the store only after taking
// the lock. Then two bare lock holders are checked the same way.
func TestPinWritersExcludeEachOther(t *testing.T) {
	const wait = 200 * time.Millisecond
	_, octoLine := genKey(t)
	_, octoLine2 := genKey(t)
	f := newPinFixture(t)
	f.gh.set("octocat", 2002, octoLine)
	b := f.openLogins(t, "octocat")
	if got := f.storedPins(t)["octocat"]; got.ID != 2002 {
		t.Fatalf("test set-up: octocat pinned %+v", got)
	}
	f.gh.set("octocat", 2002, octoLine+octoLine2)

	unlockA, err := lockPinStore(f.pinsPath)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := b.Refresh(context.Background())
		done <- err
	}()
	select {
	case err := <-done:
		unlockA()
		t.Fatalf("B's refresh finished (%v) while A held the pin store's lock", err)
	case <-time.After(wait):
	}

	// A's write, under its lock: hubot pinned to 1001.
	pins, err := loadPins(f.pinsPath)
	if err != nil {
		unlockA()
		t.Fatal(err)
	}
	pins["hubot"] = pinEntry{ID: 1001, PinnedAt: time.Now().UTC()}
	data, err := encodePins(pins)
	if err != nil {
		unlockA()
		t.Fatal(err)
	}
	if err := writeFileDurable(f.pinsPath, data); err != nil {
		unlockA()
		t.Fatal(err)
	}
	unlockA()

	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("B's refresh did not finish after A released the lock")
	}
	got := f.storedPins(t)
	if got["hubot"].ID != 1001 {
		t.Errorf("B's write lost the pin A wrote while B waited for the lock: %+v", got)
	}
	if got["octocat"].ID != 2002 || got["octocat"].KeysSHA256 != keysDigest([]byte(octoLine+octoLine2)) {
		t.Errorf("B's own change was not written: %+v", got["octocat"])
	}

	// Two bare holders of the lock (two open file descriptions, as two
	// processes have): the second blocks until the first releases.
	first, err := lockPinStore(f.pinsPath)
	if err != nil {
		t.Fatal(err)
	}
	acquired := make(chan func(), 1)
	go func() {
		second, err := lockPinStore(f.pinsPath)
		if err != nil {
			t.Error(err)
			second = func() {}
		}
		acquired <- second
	}()
	select {
	case second := <-acquired:
		second()
		first()
		t.Fatal("a second lock holder got the lock while the first held it")
	case <-time.After(wait):
	}
	first()
	select {
	case second := <-acquired:
		second()
	case <-time.After(10 * time.Second):
		t.Fatal("the second lock holder did not get the lock once the first released it")
	}
}
