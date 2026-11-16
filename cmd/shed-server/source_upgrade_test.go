package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/charliek/shed/internal/backend"
	"github.com/charliek/shed/internal/source"
	"github.com/charliek/shed/internal/source/fake"
)

// The credential source bindings' start-up step (applySourceUpgrade, run by
// stampUnassignedIn under the user list's one-time marker).

func smachine(n int) source.Machine {
	return source.Machine{ID: fmt.Sprintf("%032x", n), Name: fmt.Sprintf("machine-%d", n)}
}

// bindingsStore opens (or reopens, as a restart would) the store at path.
func bindingsStore(t *testing.T, path string) *source.FileBindings {
	t.Helper()
	fb, _, err := source.OpenFileBindings(path, source.Options{})
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	t.Cleanup(func() { _ = fb.Close() })
	return fb
}

// restart closes fb and opens the store again.
func restart(t *testing.T, fb *source.FileBindings) *source.FileBindings {
	t.Helper()
	if err := fb.Close(); err != nil {
		t.Fatal(err)
	}
	return bindingsStore(t, fb.Path())
}

func admitT(t *testing.T, b source.Bindings, user string, m source.Machine) {
	t.Helper()
	if _, err := b.Admit(user, m); err != nil {
		t.Fatal(err)
	}
}

func boundTo(b source.Bindings, user string) string {
	bd, ok := b.Get(user)
	if !ok {
		return ""
	}
	return bd.MachineID()
}

func markerExists(t *testing.T, state string) bool {
	t.Helper()
	_, err := os.Stat(filepath.Join(state, backend.UsersUpgradeFile))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	return err == nil
}

// stamp runs the start-up steps and fails on an error.
func stamp(t *testing.T, cfgName string, run func() (backend.UpgradeReport, *backend.UsersUpgrade, error)) (backend.UpgradeReport, *backend.UsersUpgrade) {
	t.Helper()
	rep, up, err := run()
	if err != nil {
		t.Fatalf("%s: %v", cfgName, err)
	}
	return rep, up
}

// TestSourceUpgradeOneUserAdoptsTheImplicitBinding: on the first start with
// one configured user the implicit user's binding and machines become that
// user's, before the marker; a later start changes nothing.
func TestSourceUpgradeOneUserAdoptsTheImplicitBinding(t *testing.T) {
	one, _, _ := wiringConfigs(t)
	state := t.TempDir()
	fb := bindingsStore(t, filepath.Join(t.TempDir(), source.FileName))
	admitT(t, fb, source.ImplicitUser, smachine(1))
	admitT(t, fb, source.ImplicitUser, smachine(2))
	be := newListOnlyBackend()

	_, up := stamp(t, "one", func() (backend.UpgradeReport, *backend.UsersUpgrade, error) {
		return stampUnassignedIn(one, be, state, fb)
	})
	if !up.FirstUserListStart() || !markerExists(t, state) {
		t.Fatal("test set-up: not a recorded first start")
	}
	if boundTo(fb, "alice") != smachine(1).ID || len(fb.Machines("alice")) != 2 {
		t.Fatalf("alice = %s with %d machines, want the implicit binding and both machines", boundTo(fb, "alice"), len(fb.Machines("alice")))
	}
	if _, ok := fb.Get(source.ImplicitUser); ok {
		t.Fatal("the implicit binding was kept")
	}
	gen, _ := fb.Get("alice")

	fb = restart(t, fb)
	stamp(t, "one again", func() (backend.UpgradeReport, *backend.UsersUpgrade, error) {
		return stampUnassignedIn(one, be, state, fb)
	})
	if bd, _ := fb.Get("alice"); bd.MachineID() != smachine(1).ID || bd.Generation != gen.Generation {
		t.Fatalf("a later start changed alice's binding: %+v", bd)
	}
}

// TestSourceUpgradeSeveralUsersDropTheImplicitBinding: with several users the
// implicit user's records are dropped; each user starts with none.
func TestSourceUpgradeSeveralUsersDropTheImplicitBinding(t *testing.T) {
	_, two, _ := wiringConfigs(t)
	state := t.TempDir()
	fb := bindingsStore(t, filepath.Join(t.TempDir(), source.FileName))
	admitT(t, fb, source.ImplicitUser, smachine(1))
	stamp(t, "two", func() (backend.UpgradeReport, *backend.UsersUpgrade, error) {
		return stampUnassignedIn(two, newListOnlyBackend(), state, fb)
	})
	for _, u := range []string{source.ImplicitUser, "alice", "bob"} {
		if _, ok := fb.Get(u); ok || len(fb.Machines(u)) != 0 {
			t.Fatalf("user %q has records after a first start with two users", u)
		}
	}
	fb = restart(t, fb)
	if _, ok := fb.Get(source.ImplicitUser); ok {
		t.Fatal("the drop did not survive a restart")
	}
}

// TestSourceUpgradeNoUserList: with no user list nothing is adopted; the
// implicit user's binding stays and the records of named users (unreachable
// without a user list) are dropped.
func TestSourceUpgradeNoUserList(t *testing.T) {
	_, _, none := wiringConfigs(t)
	state := t.TempDir()
	fb := bindingsStore(t, filepath.Join(t.TempDir(), source.FileName))
	admitT(t, fb, source.ImplicitUser, smachine(1))
	admitT(t, fb, "alice", smachine(2))
	stamp(t, "none", func() (backend.UpgradeReport, *backend.UsersUpgrade, error) {
		return stampUnassignedIn(none, newListOnlyBackend(), state, fb)
	})
	if boundTo(fb, source.ImplicitUser) != smachine(1).ID {
		t.Fatal("the implicit binding was not kept")
	}
	if _, ok := fb.Get("alice"); ok {
		t.Fatal("a named user's binding survived a start with no user list")
	}
	if markerExists(t, state) {
		t.Fatal("a start with no user list left a marker")
	}
}

// TestSourceUpgradeCrashBeforeTheMarker: the adoption is made, then the
// start ends before the marker is written (here: the shed pass did not
// finish, which leaves no marker just as a crash would). The next start is
// again the first with a user list; its adoption finds nothing to adopt and
// leaves alice's binding as it was; the marker is then written.
func TestSourceUpgradeCrashBeforeTheMarker(t *testing.T) {
	one, _, _ := wiringConfigs(t)
	state := t.TempDir()
	fb := bindingsStore(t, filepath.Join(t.TempDir(), source.FileName))
	admitT(t, fb, source.ImplicitUser, smachine(1))
	be := newListOnlyBackend()
	be.failAssign = errors.New("disk full")

	rep, up := stamp(t, "one, interrupted", func() (backend.UpgradeReport, *backend.UsersUpgrade, error) {
		return stampUnassignedIn(one, be, state, fb)
	})
	if !up.FirstUserListStart() || !rep.Incomplete || markerExists(t, state) {
		t.Fatalf("test set-up: first %v, incomplete %v, marker %v; want an unrecorded first start", up.FirstUserListStart(), rep.Incomplete, markerExists(t, state))
	}
	adopted, ok := fb.Get("alice")
	if !ok || adopted.MachineID() != smachine(1).ID {
		t.Fatalf("the adoption was not made before the marker: %+v %v", adopted, ok)
	}

	fb = restart(t, fb)
	be.failAssign = nil
	_, up = stamp(t, "one, after the crash", func() (backend.UpgradeReport, *backend.UsersUpgrade, error) {
		return stampUnassignedIn(one, be, state, fb)
	})
	if !up.FirstUserListStart() || !markerExists(t, state) {
		t.Fatal("the start after the crash was not a recorded first start")
	}
	if bd, _ := fb.Get("alice"); bd.MachineID() != smachine(1).ID || bd.Generation != adopted.Generation {
		t.Fatalf("the second run changed the adopted binding: %+v, was %+v", bd, adopted)
	}
	if _, ok := fb.Get(source.ImplicitUser); ok {
		t.Fatal("an implicit binding came back")
	}
}

// TestSourceUpgradeStoreFailureStopsTheStart: a store that cannot be written
// fails the start before the marker, and the implicit binding is kept for
// the next start, which adopts it.
func TestSourceUpgradeStoreFailureStopsTheStart(t *testing.T) {
	one, _, _ := wiringConfigs(t)
	state := t.TempDir()
	b := fake.New(source.Options{})
	admitT(t, b, source.ImplicitUser, smachine(1))
	be := newListOnlyBackend()
	b.FailWrites(errors.New("disk full"))
	if _, _, err := stampUnassignedIn(one, be, state, b); err == nil {
		t.Fatal("a failing store did not fail the start")
	}
	if markerExists(t, state) || len(be.assigned) != 0 {
		t.Fatalf("a failed start recorded the step (marker %v) or stamped sheds (%v)", markerExists(t, state), be.assigned)
	}
	if boundTo(b, source.ImplicitUser) != smachine(1).ID {
		t.Fatal("the implicit binding was lost")
	}
	b.FailWrites(nil)
	stamp(t, "one", func() (backend.UpgradeReport, *backend.UsersUpgrade, error) {
		return stampUnassignedIn(one, be, state, b)
	})
	if boundTo(b, "alice") != smachine(1).ID || !markerExists(t, state) {
		t.Fatal("the retried start did not adopt and record")
	}
}

// TestSourceUpgradeOnlyOnTheFirstStart: once the step is recorded, a start
// with one user does not adopt implicit records (which a start with no user
// list in between would have made); they are dropped as any unconfigured
// user's records are.
func TestSourceUpgradeOnlyOnTheFirstStart(t *testing.T) {
	one, two, _ := wiringConfigs(t)
	state := t.TempDir()
	b := fake.New(source.Options{})
	stamp(t, "two", func() (backend.UpgradeReport, *backend.UsersUpgrade, error) {
		return stampUnassignedIn(two, newListOnlyBackend(), state, b)
	})
	admitT(t, b, source.ImplicitUser, smachine(1))
	_, up := stamp(t, "one", func() (backend.UpgradeReport, *backend.UsersUpgrade, error) {
		return stampUnassignedIn(one, newListOnlyBackend(), state, b)
	})
	if up.FirstUserListStart() {
		t.Fatal("test set-up: a later start was taken as the first")
	}
	if _, ok := b.Get("alice"); ok {
		t.Fatal("a later start adopted the implicit binding")
	}
	if _, ok := b.Get(source.ImplicitUser); ok {
		t.Fatal("the implicit binding was kept on a server with a user list")
	}
}
