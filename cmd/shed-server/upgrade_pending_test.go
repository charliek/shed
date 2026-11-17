package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/charliek/shed/internal/backend"
	"github.com/charliek/shed/internal/config"
	"github.com/charliek/shed/internal/source"
)

// The one-time upgrade step's pending marker: a step that began for one user
// and did not finish is resumed only for that same user, while they are still
// the only configured user, and is abandoned (nothing stamped, marked done)
// in every other case, so a retry never changes the recipient.

// snapBackend is a countingListBackend that also keeps snapshots
// (backend.SnapshotStamper, backend.CountingSnapshotLister): hiddenSnaps holds
// the snapshots its listing cannot read yet.
type snapBackend struct {
	*countingListBackend
	snaps        []config.Snapshot
	hiddenSnaps  []config.Snapshot
	snapAssigned map[string]string
}

func newSnapBackend() *snapBackend {
	return &snapBackend{countingListBackend: &countingListBackend{listOnlyBackend: newListOnlyBackend()}, snapAssigned: map[string]string{}}
}

func (b *snapBackend) ListSnapshots(context.Context) ([]config.Snapshot, error) {
	return append([]config.Snapshot(nil), b.snaps...), nil
}

func (b *snapBackend) ListSnapshotsCounting(ctx context.Context) ([]config.Snapshot, int, error) {
	snaps, err := b.ListSnapshots(ctx)
	return snaps, len(b.hiddenSnaps), err
}

func (b *snapBackend) StampSnapshotUser(_ context.Context, name, user string) (*config.Snapshot, error) {
	b.snapAssigned[name] = user
	for i := range b.snaps {
		if b.snaps[i].Name == name {
			b.snaps[i].User = user
		}
	}
	return &config.Snapshot{Name: name, User: user}, nil
}

// reveal makes every hidden shed and snapshot readable.
func (b *snapBackend) reveal() {
	b.sheds = append(b.sheds, b.hidden...)
	b.hidden = nil
	b.snaps = append(b.snaps, b.hiddenSnaps...)
	b.hiddenSnaps = nil
}

// soleUserConfig is a config whose only user is name (an admin, as the only
// user of a user list must be).
func soleUserConfig(t *testing.T, name string) *config.ServerConfig {
	t.Helper()
	line, _ := wiringKey(t)
	return loadWiringConfig(t, "auth:\n  mode: mtls\n  users:\n    "+name+":\n      admin: true\n      keys:\n        - {key: \""+line+"\", label: laptop}\n")
}

// pendingFixture: a server whose first start with a user list (alice alone,
// an admin) stamps the readable shed "legacy" and snapshot "snap-a" but
// cannot read the shed "hosty" (host mounts) nor the snapshot "snap-b", so
// the step is left pending for alice. The implicit user's credential source
// is adopted by alice at that start.
func pendingFixture(t *testing.T) (state string, be *snapBackend, fb *source.FileBindings) {
	t.Helper()
	state = t.TempDir()
	fb = bindingsStore(t, filepath.Join(t.TempDir(), source.FileName))
	admitT(t, fb, source.ImplicitUser, smachine(1))
	be = newSnapBackend()
	be.hidden = []config.Shed{{Name: "hosty", Mounts: config.SelectedMounts(nil), ProjectMounts: []config.MountConfig{{Source: "/srv", Target: "/home/shed/srv"}}}}
	be.snaps = []config.Snapshot{{Name: "snap-a"}}
	be.hiddenSnaps = []config.Snapshot{{Name: "snap-b"}}

	rep, up := stamp(t, "alice", func() (backend.UpgradeReport, *backend.UsersUpgrade, error) {
		return stampUnassignedIn(soleUserConfig(t, "alice"), be, state, fb)
	})
	if !up.FirstUserListStart() || !rep.Incomplete {
		t.Fatalf("test set-up: first %v report %+v; want an unfinished first start", up.FirstUserListStart(), rep)
	}
	if got := readMarker(t, state); got != "pending:alice" {
		t.Fatalf("test set-up: marker %q, want pending for alice", got)
	}
	if !reflect.DeepEqual(be.assigned, map[string]string{"legacy": "alice"}) || !reflect.DeepEqual(be.snapAssigned, map[string]string{"snap-a": "alice"}) {
		t.Fatalf("test set-up: sheds %v snapshots %v; want legacy and snap-a to alice", be.assigned, be.snapAssigned)
	}
	if boundTo(fb, "alice") != smachine(1).ID {
		t.Fatal("test set-up: the implicit binding was not adopted by alice")
	}
	return state, be, fb
}

// TestServePendingStepIsNotRedirected is the review's sequence: alice's step
// is pending; the operator replaces alice with bob (an admin, so the host
// resources of hosty would not stop a stamp) and the unreadable records are
// repaired. The restart stamps nothing for bob, records the step as done,
// says the step for alice was abandoned, and gives bob none of the credential
// source that was alice's. A later start with bob still stamps nothing.
func TestServePendingStepIsNotRedirected(t *testing.T) {
	state, be, fb := pendingFixture(t)
	be.reveal()
	logs := captureLog(t)
	bob := soleUserConfig(t, "bob")

	rep, up := stamp(t, "bob", func() (backend.UpgradeReport, *backend.UsersUpgrade, error) {
		return stampUnassignedIn(bob, be, state, fb)
	})
	if up.FirstUserListStart() || !isPendingFor(up, "alice") {
		t.Fatalf("the start with bob did not find alice's pending step")
	}
	if _, ok := be.assigned["hosty"]; ok {
		t.Errorf("the shed alice's step could not read was given to %s", be.assigned["hosty"])
	}
	if _, ok := be.snapAssigned["snap-b"]; ok {
		t.Errorf("the snapshot alice's step could not read was given to %s", be.snapAssigned["snap-b"])
	}
	if !reflect.DeepEqual(rep.Unassigned, []string{"hosty"}) || !reflect.DeepEqual(rep.UnassignedSnapshots, []string{"snap-b"}) {
		t.Errorf("report %+v; want hosty and snap-b left with no user", rep)
	}
	if got := readMarker(t, state); got != "done" {
		t.Errorf("marker %q after the abandoned step, want done", got)
	}
	if !strings.Contains(logs.String(), "the unfinished one-time upgrade step for user alice was abandoned, because the configured users are now bob") ||
		!strings.Contains(logs.String(), "'shed assign <shed> <user>'") {
		t.Errorf("the abandonment was not logged clearly:\n%s", logs.String())
	}
	if boundTo(fb, "bob") != "" || len(fb.Machines("bob")) != 0 {
		t.Error("bob was given the credential source adopted for alice")
	}

	// A later start with bob: the step is done; still nothing stamped.
	stamp(t, "bob again", func() (backend.UpgradeReport, *backend.UsersUpgrade, error) {
		return stampUnassignedIn(bob, be, state, fb)
	})
	if _, ok := be.assigned["hosty"]; ok {
		t.Error("a later start stamped hosty")
	}
}

// TestServePendingStepResumesForTheSameUser: alice's pending step resumes at
// the next start, alice still the only user, once the records can be read:
// the shed and snapshot it could not read become hers, the step is done, and
// her adopted credential source is not changed again.
func TestServePendingStepResumesForTheSameUser(t *testing.T) {
	state, be, fb := pendingFixture(t)
	adopted, _ := fb.Get("alice")
	be.reveal()

	rep, up := stamp(t, "alice again", func() (backend.UpgradeReport, *backend.UsersUpgrade, error) {
		return stampUnassignedIn(soleUserConfig(t, "alice"), be, state, fb)
	})
	if !isPendingFor(up, "alice") || rep.Incomplete {
		t.Fatalf("pending %v report %+v; want a finished resumed step", isPendingFor(up, "alice"), rep)
	}
	if be.assigned["hosty"] != "alice" || be.snapAssigned["snap-b"] != "alice" {
		t.Errorf("sheds %v snapshots %v; want hosty and snap-b to alice", be.assigned, be.snapAssigned)
	}
	if got := readMarker(t, state); got != "done" {
		t.Errorf("marker %q, want done", got)
	}
	if bd, _ := fb.Get("alice"); bd.Generation != adopted.Generation {
		t.Errorf("the resumed step changed alice's adopted binding: %+v, was %+v", bd, adopted)
	}
}

// TestServePendingStepThenSeveralUsers: alice's pending step and a start
// with alice and bob: nothing stamped (not even for alice), the step done,
// and the abandonment logged.
func TestServePendingStepThenSeveralUsers(t *testing.T) {
	state, be, fb := pendingFixture(t)
	be.reveal()
	logs := captureLog(t)
	_, two, _ := wiringConfigs(t)

	stamp(t, "two", func() (backend.UpgradeReport, *backend.UsersUpgrade, error) {
		return stampUnassignedIn(two, be, state, fb)
	})
	if _, ok := be.assigned["hosty"]; ok {
		t.Errorf("hosty stamped with several users: %v", be.assigned)
	}
	if _, ok := be.snapAssigned["snap-b"]; ok {
		t.Errorf("snap-b stamped with several users: %v", be.snapAssigned)
	}
	if got := readMarker(t, state); got != "done" {
		t.Errorf("marker %q, want done", got)
	}
	if !strings.Contains(logs.String(), "the unfinished one-time upgrade step for user alice was abandoned, because the configured users are now alice, bob") {
		t.Errorf("the abandonment was not logged:\n%s", logs.String())
	}
	// Alice keeps the source she adopted; bob gets none of it.
	if boundTo(fb, "alice") != smachine(1).ID || boundTo(fb, "bob") != "" {
		t.Errorf("alice %q bob %q after the abandoned step", boundTo(fb, "alice"), boundTo(fb, "bob"))
	}
}

// TestServePendingStepThenNoUserList: a start with no user list removes a
// pending marker as it does a done one, and stamps nothing.
func TestServePendingStepThenNoUserList(t *testing.T) {
	state, be, fb := pendingFixture(t)
	be.reveal()
	_, _, none := wiringConfigs(t)
	stamp(t, "none", func() (backend.UpgradeReport, *backend.UsersUpgrade, error) {
		return stampUnassignedIn(none, be, state, fb)
	})
	if got := readMarker(t, state); got != "" {
		t.Errorf("marker %q after a start with no user list, want none", got)
	}
	if _, ok := be.assigned["hosty"]; ok {
		t.Error("a start with no user list stamped hosty")
	}
}

// TestServeCrashAfterThePendingMarker: a start that ends right after writing
// the pending marker for alice (before the source adoption and the first
// stamp). The next start with alice resumes and does both for alice; a next
// start with bob instead does neither, for anyone.
func TestServeCrashAfterThePendingMarker(t *testing.T) {
	crashed := func(t *testing.T) (string, *snapBackend, *source.FileBindings) {
		t.Helper()
		state := t.TempDir()
		fb := bindingsStore(t, filepath.Join(t.TempDir(), source.FileName))
		admitT(t, fb, source.ImplicitUser, smachine(1))
		up := backend.OpenUsersUpgrade(state, true, nil)
		if plan := up.Begin([]backend.UpgradeUser{{Name: "alice", Admin: true}}, time.Now()); plan.StampFor == nil || plan.StampFor.Name != "alice" {
			t.Fatalf("test set-up: plan %+v", plan)
		}
		// The crash: nothing after the marker ran.
		if got := readMarker(t, state); got != "pending:alice" {
			t.Fatalf("test set-up: marker %q", got)
		}
		return state, newSnapBackend(), fb
	}

	t.Run("same user", func(t *testing.T) {
		state, be, fb := crashed(t)
		stamp(t, "alice", func() (backend.UpgradeReport, *backend.UsersUpgrade, error) {
			return stampUnassignedIn(soleUserConfig(t, "alice"), be, state, fb)
		})
		if be.assigned["legacy"] != "alice" {
			t.Errorf("assigned %v, want legacy to alice", be.assigned)
		}
		if boundTo(fb, "alice") != smachine(1).ID {
			t.Error("the resumed step did not adopt the implicit binding for alice")
		}
		if got := readMarker(t, state); got != "done" {
			t.Errorf("marker %q, want done", got)
		}
	})
	t.Run("another user", func(t *testing.T) {
		state, be, fb := crashed(t)
		stamp(t, "bob", func() (backend.UpgradeReport, *backend.UsersUpgrade, error) {
			return stampUnassignedIn(soleUserConfig(t, "bob"), be, state, fb)
		})
		if len(be.assigned) != 0 {
			t.Errorf("assigned %v after alice's step was abandoned, want nothing", be.assigned)
		}
		if boundTo(fb, "bob") != "" || len(fb.Machines("bob")) != 0 {
			t.Error("bob was given the implicit user's credential source")
		}
		if _, ok := fb.Get(source.ImplicitUser); ok {
			t.Error("the implicit binding was kept on a server with a user list")
		}
		if got := readMarker(t, state); got != "done" {
			t.Errorf("marker %q, want done", got)
		}
	})
}

// TestServeOlderDoneMarkerIsDone: a marker written before the pending form
// existed (version 1 and completed_at only) still reads as done: a start with
// one user stamps nothing, adopts nothing, and leaves the marker as it was.
func TestServeOlderDoneMarkerIsDone(t *testing.T) {
	state := t.TempDir()
	old := []byte("{\n  \"version\": 1,\n  \"completed_at\": \"2026-11-15T12:00:00Z\"\n}\n")
	path := filepath.Join(state, backend.UsersUpgradeFile)
	if err := os.WriteFile(path, old, 0o600); err != nil {
		t.Fatal(err)
	}
	fb := bindingsStore(t, filepath.Join(t.TempDir(), source.FileName))
	admitT(t, fb, source.ImplicitUser, smachine(1))
	be := newSnapBackend()
	be.snaps = []config.Snapshot{{Name: "snap-a"}}

	_, up := stamp(t, "alice", func() (backend.UpgradeReport, *backend.UsersUpgrade, error) {
		return stampUnassignedIn(soleUserConfig(t, "alice"), be, state, fb)
	})
	if up.FirstUserListStart() {
		t.Fatal("an older done marker was read as a first start")
	}
	if _, pending := up.PendingFor(); pending {
		t.Fatal("an older done marker was read as pending")
	}
	if len(be.assigned) != 0 || len(be.snapAssigned) != 0 {
		t.Errorf("stamped sheds %v snapshots %v under an older done marker", be.assigned, be.snapAssigned)
	}
	if boundTo(fb, "alice") != "" {
		t.Error("an older done marker let alice adopt the implicit binding")
	}
	if got, _ := os.ReadFile(path); !bytes.Equal(got, old) {
		t.Errorf("the older marker was rewritten: %s", got)
	}
}
