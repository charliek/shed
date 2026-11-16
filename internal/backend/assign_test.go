package backend

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/charliek/shed/internal/config"
)

// TestCheckAssignable: only a shed with no user and no service client may be
// assigned, and (unless the start-up pass allows it) only one that carries no
// host directories and no server mounts. A Firecracker or VZ shed stored with
// no selection is reported {mode: all} and counts; the remote backend reports
// none (nil) and does not.
func TestCheckAssignable(t *testing.T) {
	dirs := []config.MountConfig{{Source: "/srv/code", Target: "/home/shed/code"}}
	for _, tc := range []struct {
		name  string
		shed  config.Shed
		allow bool
		want  error
	}{
		{"no user, no mounts", config.Shed{Mounts: config.SelectedMounts(nil)}, false, nil},
		{"no user, remote (no selection)", config.Shed{}, false, nil},
		{"service client", config.Shed{Owner: "nocturn", Mounts: config.SelectedMounts(nil)}, false, config.ErrShedAssignedSentinel},
		{"another user", config.Shed{User: "alice"}, false, config.ErrShedAssignedSentinel},
		{"the same user", config.Shed{User: "bob"}, false, config.ErrShedAssignedSentinel},
		{"unresolved", config.Shed{User: config.UserUnresolved}, false, config.ErrShedAssignedSentinel},
		{"unresolved, host resources allowed", config.Shed{User: config.UserUnresolved}, true, config.ErrShedAssignedSentinel},
		{"host directories", config.Shed{ProjectMounts: dirs, Mounts: config.SelectedMounts(nil)}, false, config.ErrShedHostResourcesSentinel},
		{"every server mount (a legacy shed)", config.Shed{Mounts: config.AllMounts()}, false, config.ErrShedHostResourcesSentinel},
		{"a named server mount", config.Shed{Mounts: config.SelectedMounts([]string{"gh"})}, false, config.ErrShedHostResourcesSentinel},
		{"host directories, allowed", config.Shed{ProjectMounts: dirs, Mounts: config.AllMounts()}, true, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sh := tc.shed
			sh.Name = "box"
			err := CheckAssignable(&sh, config.AssignShedRequest{User: "bob", AllowHostResources: tc.allow})
			switch {
			case tc.want == nil && err != nil:
				t.Fatalf("refused: %v", err)
			case tc.want != nil && !errors.Is(err, tc.want):
				t.Fatalf("got %v, want %v", err, tc.want)
			}
			if errors.Is(err, config.ErrShedHostResourcesSentinel) && !strings.Contains(err.Error(), "recreate it without them") {
				t.Errorf("the host-resources refusal does not say what to do: %v", err)
			}
		})
	}
}

// legacyFixture: alice's "box", a shed with no user and no mounts ("legacy"),
// one with host directories ("hosty"), and a service client's ("svc").
func legacyFixture() (*recBackend, *Guard) {
	b, g := fixture()
	none := config.SelectedMounts(nil)
	b.put(config.Shed{Name: "legacy", CreatedAt: t0, Status: config.StatusStopped, Mounts: none})
	b.put(config.Shed{Name: "hosty", CreatedAt: t0, Mounts: none, ProjectMounts: []config.MountConfig{{Source: "/srv", Target: "/home/shed/srv"}}})
	b.put(config.Shed{Name: "svc", CreatedAt: t0, Owner: "nocturn", Mounts: none})
	return b, g
}

// TestGuardAssignUser: assignment is an admin's, refused to anyone else before
// the shed is even looked up. Afterwards the shed is its new user's like any
// other: the user may enter it, the admin who assigned it may view, stop and
// delete it but not enter it, and a later demotion of that admin changes
// nothing (no one records who assigned it). AllowHostResources is never taken
// from a caller.
func TestGuardAssignUser(t *testing.T) {
	b, g := legacyFixture()

	for _, p := range []Principal{alice, bob, nocturn, {Kind: PrincipalUser, User: "bob", Script: true}} {
		// "missing" does not exist: the refusal is the same, so it tells the
		// caller nothing.
		for _, name := range []string{"legacy", "missing"} {
			_, err := g.AssignUser(as(p), name, config.AssignShedRequest{User: "bob"})
			if !errors.Is(err, config.ErrForbiddenSentinel) {
				t.Errorf("%s assigning %s: %v, want forbidden", p, name, err)
			}
		}
	}
	if _, err := g.AssignUser(context.Background(), "legacy", config.AssignShedRequest{User: "bob"}); !errors.Is(err, ErrNoPrincipal) {
		t.Errorf("no principal: %v", err)
	}
	for _, bad := range []string{"", "Bob", config.UserUnresolved} {
		if _, err := g.AssignUser(as(root), "legacy", config.AssignShedRequest{User: bad}); !errors.Is(err, config.ErrInvalidShedRequestSentinel) {
			t.Errorf("target %q: %v, want invalid request", bad, err)
		}
	}
	if _, err := g.AssignUser(as(root), "hosty", config.AssignShedRequest{User: "bob", AllowHostResources: true}); !errors.Is(err, config.ErrShedHostResourcesSentinel) {
		t.Errorf("a caller's AllowHostResources was honoured: %v", err)
	}
	if _, err := g.AssignUser(as(root), "svc", config.AssignShedRequest{User: "bob"}); !errors.Is(err, config.ErrShedAssignedSentinel) {
		t.Errorf("service client's shed: %v", err)
	}
	if _, err := g.AssignUser(as(root), "box", config.AssignShedRequest{User: "bob"}); !errors.Is(err, config.ErrShedAssignedSentinel) {
		t.Errorf("alice's shed: %v", err)
	}
	if _, err := g.AssignUser(as(root), "missing", config.AssignShedRequest{User: "bob"}); !errors.Is(err, config.ErrShedNotFoundSentinel) {
		t.Errorf("missing shed: %v", err)
	}
	if got := b.acted(); len(got) != 0 {
		t.Fatalf("a refused assignment changed something: %v", got)
	}

	// Before: nobody may enter it, an admin may view and stop it.
	if _, err := g.StartShed(as(bob), "legacy"); !errors.Is(err, config.ErrShedNotFoundSentinel) {
		t.Errorf("bob starting the unassigned shed: %v, want not found", err)
	}
	if _, err := g.StartShed(as(root), "legacy"); !errors.Is(err, config.ErrForbiddenSentinel) || !errors.Is(err, ErrUnassigned) ||
		!strings.Contains(err.Error(), "shed assign legacy <user>") {
		t.Errorf("admin starting the unassigned shed: %v, want forbidden, naming shed assign", err)
	}
	if _, err := g.StopShed(as(root), "legacy"); err != nil {
		t.Errorf("admin stopping the unassigned shed: %v", err)
	}
	b.calls = nil

	got, err := g.AssignUser(as(root), "legacy", config.AssignShedRequest{User: "bob"})
	if err != nil {
		t.Fatalf("assign: %v", err)
	}
	if got.User != "bob" {
		t.Errorf("assign returned user %q", got.User)
	}
	if ctx := b.ctxs["AssignUser"]; ctx == nil {
		t.Fatal("the assignment did not reach the backend")
	} else if at, ok := RequiredCreatedAt(ctx); !ok || !at.Equal(t0) {
		t.Errorf("the assignment reached the backend unbound: %v %v", at, ok)
	}
	if _, err := g.AssignUser(as(root), "legacy", config.AssignShedRequest{User: "alice"}); !errors.Is(err, config.ErrShedAssignedSentinel) {
		t.Errorf("assigning it again: %v, want already assigned", err)
	}

	if _, err := g.StartShed(as(bob), "legacy"); err != nil {
		t.Errorf("bob starting his shed: %v", err)
	}
	if _, err := g.StartShed(as(root), "legacy"); !errors.Is(err, config.ErrForbiddenSentinel) || errors.Is(err, ErrUnassigned) {
		t.Errorf("admin starting bob's shed: %v, want forbidden as another user's", err)
	}
	if _, err := g.GetShed(as(root), "legacy"); err != nil {
		t.Errorf("admin viewing bob's shed: %v", err)
	}

	// The admin is demoted: bob's shed stays bob's; the former admin sees
	// nothing of it.
	demoted := UserPrincipal("root", false)
	if _, err := g.GetShed(as(demoted), "legacy"); !errors.Is(err, config.ErrShedNotFoundSentinel) {
		t.Errorf("demoted admin viewing bob's shed: %v, want not found", err)
	}
	if _, err := g.StartShed(as(bob), "legacy"); err != nil {
		t.Errorf("bob starting his shed after the demotion: %v", err)
	}
	if sh, _ := b.GetShed(context.Background(), "legacy"); sh.User != "bob" {
		t.Errorf("stored user after the demotion = %q", sh.User)
	}
}

// TestGuardAssignAcrossARecreate: after the guard has looked up and
// authorized the unassigned shed, it is deleted and created again under its
// name, still with no user. The assignment reaches a backend that holds
// another incarnation and refuses it; the newcomer is not assigned.
func TestGuardAssignAcrossARecreate(t *testing.T) {
	b, g := legacyFixture()
	later := t0.Add(time.Minute)
	g.SetBeforeAct(func(op, name string) {
		if op == "AssignUser" && name == "legacy" {
			b.put(config.Shed{Name: "legacy", CreatedAt: later, Mounts: config.SelectedMounts(nil)})
		}
	})
	if _, err := g.AssignUser(as(root), "legacy", config.AssignShedRequest{User: "bob"}); !errors.Is(err, config.ErrShedNotFoundSentinel) {
		t.Fatalf("assign across a recreate: %v, want not found", err)
	}
	sh, _ := b.GetShed(context.Background(), "legacy")
	if sh.User != "" || !sh.CreatedAt.Equal(later) {
		t.Errorf("the newcomer was changed: %+v", sh)
	}
	if got := b.acted(); len(got) != 0 {
		t.Errorf("acted: %v", got)
	}
}

// stampingBackend is a recBackend that stamps snapshots (SnapshotStamper), as
// Firecracker and VZ do, and can list a stale view of its sheds.
type stampingBackend struct {
	*recBackend
	staleList []config.Shed
	listErr   error
}

func (b *stampingBackend) ListSheds(ctx context.Context) ([]config.Shed, error) {
	if b.listErr != nil {
		return nil, b.listErr
	}
	if b.staleList != nil {
		return append([]config.Shed(nil), b.staleList...), nil
	}
	return b.recBackend.ListSheds(ctx)
}

func (b *stampingBackend) StampSnapshotUser(ctx context.Context, name, user string) (*config.Snapshot, error) {
	b.mu.Lock()
	s, ok := b.snaps[name]
	b.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("%w: %s", config.ErrSnapshotNotFoundSentinel, name)
	}
	if err := CheckSnapshot(ctx, name, &s); err != nil {
		return nil, err
	}
	if s.User != "" {
		return nil, fmt.Errorf("%w: snapshot %s", config.ErrShedAssignedSentinel, name)
	}
	s.User = user
	b.putSnap(s)
	b.record(ctx, "StampSnapshotUser "+name)
	return &s, nil
}

// upgradeFixture: every kind of shed and snapshot the start-up pass meets.
func upgradeFixture() *stampingBackend {
	b := newRecBackend()
	none := config.SelectedMounts(nil)
	b.put(config.Shed{Name: "alices", User: "alice", CreatedAt: t0, Mounts: none})
	b.put(config.Shed{Name: "legacy", CreatedAt: t0, Mounts: none})
	b.put(config.Shed{Name: "old-all", CreatedAt: t0, Mounts: config.AllMounts()})
	b.put(config.Shed{Name: "hosty", CreatedAt: t0, Mounts: none, ProjectMounts: []config.MountConfig{{Source: "/srv", Target: "/home/shed/srv"}}})
	b.put(config.Shed{Name: "svc", CreatedAt: t0, Owner: "nocturn", Mounts: none})
	b.put(config.Shed{Name: "odd", CreatedAt: t0, User: config.UserUnresolved, Mounts: none})
	b.putSnap(config.Snapshot{Name: "snap-old", CreatedAt: t0})
	b.putSnap(config.Snapshot{Name: "snap-alice", CreatedAt: t0, User: "alice"})
	return &stampingBackend{recBackend: b}
}

type capturedLog struct{ info, debug []string }

func (c *capturedLog) log() UpgradeLog {
	return UpgradeLog{
		Infof:  func(f string, a ...any) { c.info = append(c.info, fmt.Sprintf(f, a...)) },
		Debugf: func(f string, a ...any) { c.debug = append(c.debug, fmt.Sprintf(f, a...)) },
	}
}

func usersOf(b *stampingBackend) map[string]string {
	out := map[string]string{}
	sheds, _ := b.recBackend.ListSheds(context.Background())
	for _, s := range sheds {
		out[s.Name] = s.User
	}
	snaps, _ := b.ListSnapshots(context.Background())
	for _, s := range snaps {
		out["snapshot "+s.Name] = s.User
	}
	return out
}

// TestStampUnassigned covers the start-up pass for each size of user list.
func TestStampUnassigned(t *testing.T) {
	before := usersOf(upgradeFixture())

	t.Run("no user list: nothing at all", func(t *testing.T) {
		b := upgradeFixture()
		var l capturedLog
		rep := StampUnassigned(context.Background(), b, nil, true, l.log())
		if !reflect.DeepEqual(rep, UpgradeReport{}) || len(l.info)+len(l.debug) != 0 || len(b.acted()) != 0 {
			t.Errorf("report %+v, logs %q %q, calls %v", rep, l.info, l.debug, b.acted())
		}
		if got := usersOf(b); !reflect.DeepEqual(got, before) {
			t.Errorf("users changed: %v", got)
		}
	})

	t.Run("one admin: every shed and snapshot with no user is theirs", func(t *testing.T) {
		b := upgradeFixture()
		var l capturedLog
		rep := StampUnassigned(context.Background(), b, []UpgradeUser{{Name: "alice", Admin: true}}, true, l.log())
		want := map[string]string{
			"alices": "alice", "legacy": "alice", "old-all": "alice", "hosty": "alice",
			"svc": "", "odd": config.UserUnresolved,
			"snapshot snap-old": "alice", "snapshot snap-alice": "alice",
		}
		if got := usersOf(b); !reflect.DeepEqual(got, want) {
			t.Errorf("users = %v, want %v", got, want)
		}
		if !reflect.DeepEqual(rep.StampedSheds, []string{"hosty", "legacy", "old-all"}) || !reflect.DeepEqual(rep.StampedSnapshots, []string{"snap-old"}) || len(rep.Unassigned) != 0 {
			t.Errorf("report %+v", rep)
		}
		for _, name := range []string{"shed hosty", "shed legacy", "shed old-all", "snapshot snap-old"} {
			if !containsLine(l.info, name+" had no user; it is now user alice's") {
				t.Errorf("the stamp of %s was not logged: %q", name, l.info)
			}
		}
		if !containsLine(l.info, "found no shed without a user or a service client") || rep.Incomplete {
			t.Errorf("no report line: %q", l.info)
		}
		// Each stamp was bound to the incarnation the listing found.
		if at, ok := RequiredCreatedAt(b.ctxs["AssignUser"]); !ok || !at.Equal(t0) {
			t.Errorf("a stamp was not bound to its incarnation: %v %v", at, ok)
		}

		// The pass is idempotent.
		l = capturedLog{}
		b.calls = nil
		rep = StampUnassigned(context.Background(), b, []UpgradeUser{{Name: "alice", Admin: true}}, true, l.log())
		if len(rep.StampedSheds)+len(rep.StampedSnapshots) != 0 || len(b.acted()) != 0 {
			t.Errorf("second pass stamped %+v (calls %v)", rep, b.acted())
		}
	})

	t.Run("one user who is not an admin: host resources are not handed over", func(t *testing.T) {
		b := upgradeFixture()
		var l capturedLog
		rep := StampUnassigned(context.Background(), b, []UpgradeUser{{Name: "alice"}}, true, l.log())
		got := usersOf(b)
		if got["legacy"] != "alice" || got["hosty"] != "" || got["old-all"] != "" || got["snapshot snap-old"] != "alice" {
			t.Errorf("users = %v", got)
		}
		if !reflect.DeepEqual(rep.Unassigned, []string{"hosty", "old-all"}) || !reflect.DeepEqual(rep.Blocked, []string{"hosty", "old-all"}) {
			t.Errorf("report %+v", rep)
		}
	})

	t.Run("several users: nothing is stamped, and the rest is reported", func(t *testing.T) {
		b := upgradeFixture()
		var l capturedLog
		rep := StampUnassigned(context.Background(), b, []UpgradeUser{{Name: "alice", Admin: true}, {Name: "bob"}}, true, l.log())
		if got := usersOf(b); !reflect.DeepEqual(got, before) {
			t.Errorf("users changed: %v", got)
		}
		if len(b.acted()) != 0 {
			t.Errorf("calls %v", b.acted())
		}
		if !reflect.DeepEqual(rep.Unassigned, []string{"hosty", "legacy", "old-all"}) || !reflect.DeepEqual(rep.Blocked, []string{"hosty", "old-all"}) || !reflect.DeepEqual(rep.UnassignedSnapshots, []string{"snap-old"}) {
			t.Errorf("report %+v", rep)
		}
		if !containsLine(l.info, "found 3 shed(s) with no user (2 of them carry host directories or server mounts") || rep.Incomplete {
			t.Errorf("report line: %q", l.info)
		}
		if !containsLine(l.info, "found 1 snapshot(s) with no user") {
			t.Errorf("snapshot line: %q", l.info)
		}
		if !containsLine(l.debug, "sheds with no user: hosty, legacy, old-all") || !containsLine(l.debug, "carry host resources: hosty, old-all") {
			t.Errorf("debug lines: %q", l.debug)
		}
		for _, line := range l.info {
			if strings.Contains(line, "legacy") {
				t.Errorf("a shed is named outside debug: %q", line)
			}
		}
	})

	t.Run("a shed made again since the listing is not stamped", func(t *testing.T) {
		b := upgradeFixture()
		listed, _ := b.recBackend.ListSheds(context.Background())
		b.staleList = listed
		b.put(config.Shed{Name: "legacy", CreatedAt: t0.Add(time.Minute), Mounts: config.SelectedMounts(nil)})
		var l capturedLog
		rep := StampUnassigned(context.Background(), b, []UpgradeUser{{Name: "alice", Admin: true}}, true, l.log())
		if got := usersOf(b)["legacy"]; got != "" {
			t.Errorf("the newcomer was stamped as %q", got)
		}
		if !reflect.DeepEqual(rep.Unassigned, []string{"legacy"}) || !containsLine(l.info, "shed legacy has no user and could not be given to user alice") {
			t.Errorf("report %+v, log %q", rep, l.info)
		}
	})
}

func containsLine(lines []string, sub string) bool {
	for _, l := range lines {
		if strings.Contains(l, sub) {
			return true
		}
	}
	return false
}

// failingStamper is a stampingBackend whose shed assignments fail with err
// (a record that cannot be saved, a cloud write that does not finish), and
// whose listing, when unreadable is set, reports that many records it could
// not read (CountingLister).
type failingStamper struct {
	*stampingBackend
	err        error
	unreadable int
}

func (b *failingStamper) AssignUser(ctx context.Context, name string, req config.AssignShedRequest) (*config.Shed, error) {
	if b.err != nil {
		return nil, b.err
	}
	return b.stampingBackend.AssignUser(ctx, name, req)
}

func (b *failingStamper) ListShedsCounting(ctx context.Context) ([]config.Shed, int, error) {
	sheds, err := b.ListSheds(ctx)
	return sheds, b.unreadable, err
}

// TestStampUnassignedOneTimeStep: the pass stamps only on the server's first
// start with a user list; it says when it did not finish, so that start is
// not recorded as done; and a record that came to belong to someone since the
// listing is left alone without making the pass unfinished.
func TestStampUnassignedOneTimeStep(t *testing.T) {
	alice := []UpgradeUser{{Name: "alice", Admin: true}}

	t.Run("not the first start: nothing is stamped, even with one user", func(t *testing.T) {
		b := upgradeFixture()
		before := usersOf(b)
		var l capturedLog
		rep := StampUnassigned(context.Background(), b, alice, false, l.log())
		if got := usersOf(b); !reflect.DeepEqual(got, before) || len(b.acted()) != 0 {
			t.Errorf("users changed: %v (calls %v)", got, b.acted())
		}
		if !reflect.DeepEqual(rep.Unassigned, []string{"hosty", "legacy", "old-all"}) || !reflect.DeepEqual(rep.UnassignedSnapshots, []string{"snap-old"}) || rep.Incomplete {
			t.Errorf("report %+v", rep)
		}
	})

	t.Run("a record that comes to disagree after the listing is not stamped", func(t *testing.T) {
		b := upgradeFixture()
		listed, _ := b.recBackend.ListSheds(context.Background())
		b.staleList = listed
		b.put(config.Shed{Name: "legacy", CreatedAt: t0, User: config.UserUnresolved, Mounts: config.SelectedMounts(nil)})
		var l capturedLog
		rep := StampUnassigned(context.Background(), b, alice, true, l.log())
		if got := usersOf(b)["legacy"]; got != config.UserUnresolved {
			t.Errorf("the shed was stamped: user %q", got)
		}
		if slices.Contains(rep.Unassigned, "legacy") || slices.Contains(rep.StampedSheds, "legacy") {
			t.Errorf("report %+v", rep)
		}
		if rep.Incomplete {
			t.Errorf("a shed that belongs to someone now made the pass unfinished: %+v", rep)
		}
		if !containsLine(l.info, "shed legacy was found with no user but belongs to someone now") {
			t.Errorf("log %q", l.info)
		}
		// The others were stamped as usual.
		if got := usersOf(b); got["hosty"] != "alice" || got["old-all"] != "alice" {
			t.Errorf("users = %v", got)
		}
	})

	t.Run("a stamp that fails leaves the pass unfinished", func(t *testing.T) {
		b := &failingStamper{stampingBackend: upgradeFixture(), err: errors.New("disk full")}
		var l capturedLog
		rep := StampUnassigned(context.Background(), b, alice, true, l.log())
		if !rep.Incomplete {
			t.Errorf("report %+v, want unfinished", rep)
		}
		if !reflect.DeepEqual(rep.Unassigned, []string{"hosty", "legacy", "old-all"}) {
			t.Errorf("report %+v", rep)
		}
	})

	t.Run("a listing that fails leaves the pass unfinished", func(t *testing.T) {
		b := upgradeFixture()
		b.listErr = errors.New("the store is unreadable")
		var l capturedLog
		if rep := StampUnassigned(context.Background(), b, alice, true, l.log()); !rep.Incomplete {
			t.Errorf("report %+v, want unfinished", rep)
		}
	})

	t.Run("records the listing could not read are reported, not counted as having a user", func(t *testing.T) {
		b := &failingStamper{stampingBackend: upgradeFixture(), unreadable: 2}
		var l capturedLog
		rep := StampUnassigned(context.Background(), b, alice, true, l.log())
		if rep.UnreadableSheds != 2 || rep.Incomplete {
			t.Errorf("report %+v", rep)
		}
		if !containsLine(l.info, "found no shed without a user or a service client; 2 shed record(s) could not be read and are not counted") {
			t.Errorf("report line: %q", l.info)
		}
		for _, line := range l.info {
			if strings.Contains(line, "every shed") {
				t.Errorf("the report claims every shed: %q", line)
			}
		}
	})
}

// TestUsersUpgradeMarker: the first start with a user list is the one with no
// marker; MarkDone records it durably; a start with no user list removes the
// marker, so adding a list later is a first start again; a marker that cannot
// be read, is damaged or is from a newer build counts as done, and is kept.
func TestUsersUpgradeMarker(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, UsersUpgradeFile)
	var lines []string
	logf := func(f string, a ...any) { lines = append(lines, fmt.Sprintf(f, a...)) }
	now := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)

	u := OpenUsersUpgrade(dir, true, logf)
	if !u.FirstUserListStart() {
		t.Fatal("no marker: not a first start")
	}
	if err := u.MarkDone(now); err != nil {
		t.Fatal(err)
	}
	if !u.FirstUserListStart() {
		t.Error("MarkDone changed this start's decision")
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Fatalf("no marker after MarkDone: %v", err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Errorf("marker mode %v", st.Mode().Perm())
	}
	var m map[string]any
	data, _ := os.ReadFile(path)
	if err := json.Unmarshal(data, &m); err != nil || m["version"] != float64(1) || m["completed_at"] != "2026-11-15T12:00:00Z" {
		t.Errorf("marker %s (%v)", data, err)
	}

	if u := OpenUsersUpgrade(dir, true, logf); u.FirstUserListStart() {
		t.Error("with the marker: a first start")
	}

	// No user list: the marker goes, and the next list is a first start.
	lines = nil
	if u := OpenUsersUpgrade(dir, false, logf); u.FirstUserListStart() {
		t.Error("no user list: a first start")
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the marker was not removed: %v", err)
	}
	if !containsLine(lines, "removed the user-list upgrade marker") {
		t.Errorf("the removal was not logged: %q", lines)
	}
	if u := OpenUsersUpgrade(dir, true, logf); !u.FirstUserListStart() {
		t.Error("a user list added again: not a first start")
	}

	for _, tc := range []struct {
		name, body, line string
	}{
		{"damaged", "{", "is damaged"},
		{"newer", `{"version": 2, "completed_at": "2026-11-15T12:00:00Z"}`, "has schema version 2"},
		{"no version", `{}`, "has schema version 0"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := os.WriteFile(path, []byte(tc.body), 0o600); err != nil {
				t.Fatal(err)
			}
			lines = nil
			u := OpenUsersUpgrade(dir, true, logf)
			if u.FirstUserListStart() {
				t.Error("taken as a first start")
			}
			if !containsLine(lines, tc.line) || !containsLine(lines, "taken as done") {
				t.Errorf("log %q", lines)
			}
			if err := u.MarkDone(now); err != nil {
				t.Fatal(err)
			}
			if got, _ := os.ReadFile(path); string(got) != tc.body {
				t.Errorf("the marker was rewritten: %s", got)
			}
		})
	}

	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}

	// A directory entry of any kind at the marker's path is a marker that is
	// present: it is never read as a first start, even when it cannot be
	// read or names nothing (a symlink whose target is gone).
	for _, tc := range []struct {
		name, line string
		make       func(t *testing.T)
	}{
		{"dangling symlink", "is not a regular file", func(t *testing.T) {
			if err := os.Symlink(filepath.Join(dir, "gone"), path); err != nil {
				t.Fatal(err)
			}
		}},
		{"directory", "is not a regular file", func(t *testing.T) {
			if err := os.Mkdir(path, 0o700); err != nil {
				t.Fatal(err)
			}
		}},
		{"special file", "is not a regular file", func(t *testing.T) {
			if err := syscall.Mkfifo(path, 0o600); err != nil {
				t.Skipf("cannot make a FIFO here: %v", err)
			}
		}},
		{"unreadable file", "cannot be read", func(t *testing.T) {
			if os.Geteuid() == 0 {
				t.Skip("root reads a file whatever its mode")
			}
			if err := os.WriteFile(path, []byte(`{"version": 1}`), 0o000); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Cleanup(func() { _ = os.Remove(path) })
			tc.make(t)
			lines = nil
			if u := OpenUsersUpgrade(dir, true, logf); u.FirstUserListStart() {
				t.Errorf("a marker that is %s taken as a first start", tc.name)
			}
			if !containsLine(lines, tc.line) || !containsLine(lines, "taken as done") {
				t.Errorf("log %q", lines)
			}
			if _, err := os.Lstat(path); err != nil {
				t.Errorf("the marker entry is gone: %v", err)
			}
		})
	}
}
