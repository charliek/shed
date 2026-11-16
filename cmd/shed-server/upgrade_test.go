package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/charliek/shed/internal/backend"
	"github.com/charliek/shed/internal/config"
)

// listOnlyBackend is a Backend whose only working calls are the listings and
// the assignment the start-up pass makes; it records the assignments and
// lists the sheds as they are after them. failAssign makes every assignment
// fail, failList every listing.
type listOnlyBackend struct {
	backend.Backend
	sheds      []config.Shed
	assigned   map[string]string
	failAssign error
	failList   error
}

func (b *listOnlyBackend) ListSheds(context.Context) ([]config.Shed, error) {
	if b.failList != nil {
		return nil, b.failList
	}
	return append([]config.Shed(nil), b.sheds...), nil
}

func (b *listOnlyBackend) AssignUser(_ context.Context, name string, req config.AssignShedRequest) (*config.Shed, error) {
	if b.failAssign != nil {
		return nil, b.failAssign
	}
	b.assigned[name] = req.User
	for i := range b.sheds {
		if b.sheds[i].Name == name {
			b.sheds[i].User = req.User
		}
	}
	return &config.Shed{Name: name, User: req.User}, nil
}

func newListOnlyBackend() *listOnlyBackend {
	return &listOnlyBackend{sheds: []config.Shed{{Name: "legacy", Mounts: config.SelectedMounts(nil)}, {Name: "mine", User: "alice"}}, assigned: map[string]string{}}
}

// wiringConfigs: one user (alice, an admin), two users (alice and bob), and no
// user list.
func wiringConfigs(t *testing.T) (one, two, none *config.ServerConfig) {
	t.Helper()
	aliceLine, _ := wiringKey(t)
	bobLine, _ := wiringKey(t)
	one = loadWiringConfig(t, "auth:\n  mode: mtls\n  users:\n    alice:\n      admin: true\n      keys:\n        - {key: \""+aliceLine+"\", label: laptop}\n")
	two = loadWiringConfig(t, "auth:\n  mode: mtls\n  users:\n    alice:\n      admin: true\n      keys:\n        - {key: \""+aliceLine+"\", label: laptop}\n    bob:\n      keys:\n        - {key: \""+bobLine+"\", label: laptop}\n")
	line, _ := wiringKey(t)
	none = loadWiringConfig(t, "auth:\n  mode: mtls\n  ssh:\n    authorized_keys: [\""+line+"\"]\n")
	return one, two, none
}

// TestServeStampsUnassignedSheds: shed-server hands the start-up pass the
// configured users with their admin flags; with no user list the pass does
// nothing, on the first start with one user it stamps, with two it does not.
func TestServeStampsUnassignedSheds(t *testing.T) {
	one, two, none := wiringConfigs(t)

	if got := upgradeUsers(two); !reflect.DeepEqual(got, []backend.UpgradeUser{{Name: "alice", Admin: true}, {Name: "bob"}}) {
		t.Errorf("upgradeUsers = %+v", got)
	}
	if got := upgradeUsers(none); got != nil {
		t.Errorf("upgradeUsers with no user list = %+v", got)
	}

	for _, tc := range []struct {
		name string
		cfg  *config.ServerConfig
		want map[string]string
	}{
		{"no user list", none, map[string]string{}},
		{"one user", one, map[string]string{"legacy": "alice"}},
		{"two users", two, map[string]string{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			be := newListOnlyBackend()
			rep, _ := stampUnassignedIn(tc.cfg, be, t.TempDir())
			if !reflect.DeepEqual(be.assigned, tc.want) {
				t.Errorf("assigned %v, want %v (report %+v)", be.assigned, tc.want, rep)
			}
		})
	}
}

// TestServeStampIsAOneTimeStep: the automatic stamp runs only on the first
// start with a user list. A server started with two users, its legacy shed
// left unassigned on purpose, that then drops to one user does not give the
// shed to the one left; it needs 'shed assign'. Going back to no user list
// removes the marker, so the next user list is a first start again. A pass
// that does not finish writes no marker and runs again at the next start.
func TestServeStampIsAOneTimeStep(t *testing.T) {
	one, two, none := wiringConfigs(t)
	state := t.TempDir()
	marker := filepath.Join(state, backend.UsersUpgradeFile)
	be := newListOnlyBackend()

	// First start with a user list: two users. Nothing is stamped; the step
	// is recorded.
	if _, up := stampUnassignedIn(two, be, state); !up.FirstUserListStart() {
		t.Fatal("the first start with a user list was not taken as one")
	}
	if len(be.assigned) != 0 {
		t.Fatalf("two users: assigned %v", be.assigned)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("no marker after the first start with a user list: %v", err)
	}

	// Bob is removed: one user left. The legacy shed stays unassigned.
	rep, up := stampUnassignedIn(one, be, state)
	if up.FirstUserListStart() {
		t.Error("a later start was taken as the first with a user list")
	}
	if len(be.assigned) != 0 || !reflect.DeepEqual(rep.Unassigned, []string{"legacy"}) {
		t.Fatalf("one user left: assigned %v, report %+v", be.assigned, rep)
	}

	// The user list is removed, then the one user added again: the step
	// runs again, and this time stamps.
	if _, up := stampUnassignedIn(none, be, state); up.FirstUserListStart() {
		t.Error("a start with no user list was taken as a first start")
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the marker survived a start with no user list: %v", err)
	}

	// A pass that fails writes no marker; the next start runs it again.
	be.failAssign = errors.New("disk full")
	if rep, _ := stampUnassignedIn(one, be, state); !rep.Incomplete {
		t.Fatalf("the failing pass reported %+v", rep)
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a pass that did not finish wrote the marker: %v", err)
	}
	be.failAssign = nil
	if _, up := stampUnassignedIn(one, be, state); !up.FirstUserListStart() {
		t.Error("the start after an unfinished pass was not a first start")
	}
	if !reflect.DeepEqual(be.assigned, map[string]string{"legacy": "alice"}) {
		t.Errorf("assigned %v, want legacy to alice", be.assigned)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Errorf("no marker after the finished pass: %v", err)
	}
}

// TestServeSeveralUsersRecordedBeforeListing: with several users nothing is
// ever stamped, so the one-time step is recorded before anything is listed. A
// first start with two users whose listing fails still records it; a later
// start with one user stamps nothing, the legacy shed left for 'shed assign'.
func TestServeSeveralUsersRecordedBeforeListing(t *testing.T) {
	one, two, _ := wiringConfigs(t)
	state := t.TempDir()
	marker := filepath.Join(state, backend.UsersUpgradeFile)
	be := newListOnlyBackend()

	be.failList = errors.New("instance directory unreadable")
	rep, up := stampUnassignedIn(two, be, state)
	if !up.FirstUserListStart() || !rep.Incomplete {
		t.Fatalf("test set-up: first start %v, report %+v; want a first start whose pass did not finish", up.FirstUserListStart(), rep)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("a first start with two users whose listing failed left no marker: %v", err)
	}

	be.failList = nil
	rep, up = stampUnassignedIn(one, be, state)
	if up.FirstUserListStart() {
		t.Error("the start after a two-user first start was taken as a first start")
	}
	if len(be.assigned) != 0 || !reflect.DeepEqual(rep.Unassigned, []string{"legacy"}) {
		t.Errorf("one user after an interrupted two-user start: assigned %v, report %+v; want nothing stamped", be.assigned, rep)
	}
}
