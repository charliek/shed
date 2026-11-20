package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/charliek/shed/internal/backend"
	"github.com/charliek/shed/internal/config"
	"github.com/charliek/shed/internal/source"
)

// The one-time upgrade step's pending marker: a step that began for one user
// and did not finish is resumed only for that same user, while they are still
// the only configured user. In every other case the start is refused (nothing
// stamped, adopted or recorded) unless the operator abandons the step on
// purpose (--abandon-user-upgrade: nothing stamped, marked done), so a retry
// never changes the recipient.

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
// repaired. The restart, with --abandon-user-upgrade, stamps nothing for bob,
// records the step as done, says the step for alice was abandoned, and gives
// bob none of the credential source that was alice's. A later start with bob
// (without the flag) still stamps nothing. (Without the flag the restart is
// refused: TestServeRefusesChangedUsers.)
func TestServePendingStepIsNotRedirected(t *testing.T) {
	state, be, fb := pendingFixture(t)
	be.reveal()
	logs := captureLog(t)
	bob := soleUserConfig(t, "bob")

	rep, up := stamp(t, "bob", func() (backend.UpgradeReport, *backend.UsersUpgrade, error) {
		return stampUnassignedWith(bob, be, state, fb, true)
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
// with alice and bob and --abandon-user-upgrade: nothing stamped (not even
// for alice), the step done, and the abandonment logged.
func TestServePendingStepThenSeveralUsers(t *testing.T) {
	state, be, fb := pendingFixture(t)
	be.reveal()
	logs := captureLog(t)
	_, two, _ := wiringConfigs(t)

	stamp(t, "two", func() (backend.UpgradeReport, *backend.UsersUpgrade, error) {
		return stampUnassignedWith(two, be, state, fb, true)
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
// start with bob instead is refused, and with --abandon-user-upgrade does
// neither, for anyone.
func TestServeCrashAfterThePendingMarker(t *testing.T) {
	crashed := func(t *testing.T) (string, *snapBackend, *source.FileBindings) {
		t.Helper()
		state := t.TempDir()
		fb := bindingsStore(t, filepath.Join(t.TempDir(), source.FileName))
		admitT(t, fb, source.ImplicitUser, smachine(1))
		up := backend.OpenUsersUpgrade(state, true, nil)
		if plan, err := up.Begin([]backend.UpgradeUser{{Name: "alice", Admin: true}}, false, time.Now()); err != nil || plan.StampFor == nil || plan.StampFor.Name != "alice" {
			t.Fatalf("test set-up: plan %+v err %v", plan, err)
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
		if _, _, err := stampUnassignedIn(soleUserConfig(t, "bob"), be, state, fb); !isChangedRefusal(err, "alice") {
			t.Fatalf("the start with bob: %v; want it refused for alice's step", err)
		}
		if len(be.assigned) != 0 || boundTo(fb, "bob") != "" || readMarker(t, state) != "pending:alice" {
			t.Fatalf("the refused start changed something: assigned %v, marker %q", be.assigned, readMarker(t, state))
		}
		if _, ok := fb.Get(source.ImplicitUser); !ok {
			t.Fatal("the refused start dropped the implicit binding")
		}
		stamp(t, "bob, abandoning", func() (backend.UpgradeReport, *backend.UsersUpgrade, error) {
			return stampUnassignedWith(soleUserConfig(t, "bob"), be, state, fb, true)
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

// isChangedRefusal reports whether err is the refusal of a start whose users
// changed while the step for pending is unfinished.
func isChangedRefusal(err error, pending string) bool {
	var changed *backend.UsersUpgradeChangedError
	return errors.As(err, &changed) && changed.Pending.Name == pending
}

// isAbandonRefusal reports whether err is the refusal of the abandon flag
// with no step to abandon.
func isAbandonRefusal(err error) bool {
	var refused *backend.UsersUpgradeAbandonError
	return errors.As(err, &refused)
}

// directoryAdminConfig is a directory server whose only local admin is name;
// nothing is enrolled in any state directory a test gives it, so its list is
// not in force (only the local admins can log in).
func directoryAdminConfig(t *testing.T, name string) *config.ServerConfig {
	t.Helper()
	line, _ := wiringKey(t)
	return loadWiringConfig(t, "auth:\n  mode: mtls\n  users:\n    "+name+":\n      admin: true\n      keys:\n        - {key: \""+line+"\", label: laptop}\n  directory:\n    url: https://accounts.example.test\n")
}

// serverCommandStarts are the words a command suggested in a shed-server
// message starts with.
var serverCommandStarts = []string{"shed-server ", "shed ", "sudo ", "ssh ", "ssh-", "systemctl ", "brew "}

// suggestedServerCommands returns each command msg suggests: a line, or the
// text after ": " to the line's end, that starts with one of
// serverCommandStarts. A command in single quotes inside a sentence ('shed
// assign <shed> <user>') is quoted for the shell as a whole and so holds no
// shell syntax outside quotes. It is cmd/shed's suggested-command rule
// (cmd/shed/suggested_commands_test.go) for this package's start-up
// refusals, which that walker does not read.
func suggestedServerCommands(msg string) []string {
	var out []string
	keep := func(seg string) {
		seg = strings.TrimSpace(seg)
		for _, w := range serverCommandStarts {
			if strings.HasPrefix(seg, w) {
				out = append(out, seg)
				return
			}
		}
	}
	for _, line := range strings.Split(msg, "\n") {
		keep(line)
		for rest := line; ; {
			i := strings.Index(rest, ": ")
			if i < 0 {
				break
			}
			rest = rest[i+2:]
			keep(rest)
		}
	}
	return out
}

// checkSuggestedServerCommands holds msg to the suggested-command rule: no
// shell metacharacter outside single quotes in a suggested command, and none
// of values (read from the config or the state directory) in one.
func checkSuggestedServerCommands(t *testing.T, msg string, values ...string) {
	t.Helper()
	for _, cmd := range suggestedServerCommands(msg) {
		quoted := false
		for _, c := range cmd {
			switch {
			case c == '\'':
				quoted = !quoted
			case !quoted && strings.ContainsRune("<>|&;$`", c):
				t.Errorf("the suggested command %q holds %q outside single quotes", cmd, c)
			}
		}
		for _, v := range values {
			if v != "" && strings.Contains(cmd, v) {
				t.Errorf("the suggested command %q holds %q, a value read from the config or the state directory", cmd, v)
			}
		}
	}
}

// TestSuggestedServerCommandsCheck: the check sees a command on its own line
// and after a colon, and catches a metacharacter or a value in one (its own
// negative control).
func TestSuggestedServerCommandsCheck(t *testing.T) {
	got := suggestedServerCommands("first: shed-server serve --x\n    shed assign <shed>\nand 'shed list' here\nthe server's shed")
	want := []string{"shed-server serve --x", "shed assign <shed>"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("suggestedServerCommands = %q, want %q", got, want)
	}
	for _, tc := range []struct {
		msg   string
		value string
		bad   bool
	}{
		{"run: shed-server serve --config /path/to/server.yaml", "alice", false},
		{"run: shed assign <shed> <user>", "", true},
		{"run: shed assign web alice", "alice", true},
		{"run 'shed assign <shed> <user>' later", "", false},
	} {
		bad := false
		for _, cmd := range suggestedServerCommands(tc.msg) {
			quoted := false
			for _, c := range cmd {
				switch {
				case c == '\'':
					quoted = !quoted
				case !quoted && strings.ContainsRune("<>|&;$`", c):
					bad = true
				}
			}
			if tc.value != "" && strings.Contains(cmd, tc.value) {
				bad = true
			}
		}
		if bad != tc.bad {
			t.Errorf("%q: flagged %v, want %v", tc.msg, bad, tc.bad)
		}
	}
}

// TestServeRefusesChangedUsers: alice's step is pending (pendingFixture: the
// shed hosty and the snapshot snap-b not reached, the implicit credential
// source adopted by alice) and the records are readable again. For each
// configured user list and the abandon flag, the start is refused with
// nothing stamped, adopted or recorded (the marker and the bindings
// byte-identical), or abandons the step as before (marker done, the log
// line, nothing stamped), or resumes it for alice, or refuses the flag.
// Beside auth.directory the users compared are the local admins only, and a
// list that is not in force (nothing enrolled) changes nothing.
func TestServeRefusesChangedUsers(t *testing.T) {
	_, two, none := wiringConfigs(t)
	for _, tc := range []struct {
		name    string
		cfg     *config.ServerConfig
		abandon bool
		want    string // refused, abandoned, resumed, flag-refused, removed
	}{
		{"bob alone", soleUserConfig(t, "bob"), false, "refused"},
		{"alice and bob", two, false, "refused"},
		{"directory, local admin ops", directoryAdminConfig(t, "ops"), false, "refused"},
		{"bob alone, abandoning", soleUserConfig(t, "bob"), true, "abandoned"},
		{"alice and bob, abandoning", two, true, "abandoned"},
		{"directory, local admin ops, abandoning", directoryAdminConfig(t, "ops"), true, "abandoned"},
		{"alice again", soleUserConfig(t, "alice"), false, "resumed"},
		{"directory not enrolled, local admin alice", directoryAdminConfig(t, "alice"), false, "resumed"},
		{"alice again, abandoning", soleUserConfig(t, "alice"), true, "flag-refused"},
		{"directory not enrolled, local admin alice, abandoning", directoryAdminConfig(t, "alice"), true, "flag-refused"},
		{"no user list", none, false, "removed"},
		{"no user list, abandoning", none, true, "flag-refused"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			state, be, fb := pendingFixture(t)
			be.reveal()
			marker := filepath.Join(state, backend.UsersUpgradeFile)
			markerBefore, err := os.ReadFile(marker)
			if err != nil {
				t.Fatal(err)
			}
			fb = restart(t, fb)
			bindingsBefore, err := os.ReadFile(fb.Path())
			if err != nil {
				t.Fatal(err)
			}
			logs := captureLog(t)

			// The check serve makes before anything opens, and the start-up
			// step itself, agree.
			checkErr := checkUsersUpgrade(tc.cfg, state, tc.abandon)
			rep, _, err := stampUnassignedWith(tc.cfg, be, state, fb, tc.abandon)
			if (checkErr == nil) != (err == nil) {
				t.Errorf("checkUsersUpgrade = %v but the start-up step = %v", checkErr, err)
			}
			unchanged := func() {
				t.Helper()
				if got, _ := os.ReadFile(marker); !bytes.Equal(got, markerBefore) {
					t.Errorf("the marker changed: %s, was %s", got, markerBefore)
				}
				if got, _ := os.ReadFile(fb.Path()); !bytes.Equal(got, bindingsBefore) {
					t.Errorf("the credential source bindings changed: %s, was %s", got, bindingsBefore)
				}
				if !reflect.DeepEqual(be.assigned, map[string]string{"legacy": "alice"}) || !reflect.DeepEqual(be.snapAssigned, map[string]string{"snap-a": "alice"}) {
					t.Errorf("stamped sheds %v snapshots %v; want only the set-up's", be.assigned, be.snapAssigned)
				}
				if strings.Contains(logs.String(), "Users: found") {
					t.Errorf("the pass over sheds ran:\n%s", logs.String())
				}
			}
			switch tc.want {
			case "refused":
				if !isChangedRefusal(err, "alice") {
					t.Fatalf("err %v; want the start refused for alice's step", err)
				}
				unchanged()
				msg := err.Error()
				for _, part := range []string{
					"the server does not start",
					"began for user alice and did not finish",
					"the sheds and snapshots it has not reached would keep no user",
					"put user alice back as the only user in auth.users",
					"until its log says the one-time upgrade step for the user list is done and recorded",
					"\n    shed-server serve --config /path/to/server.yaml\n",
					"abandon it on purpose",
					"\n    shed-server serve --config /path/to/server.yaml --abandon-user-upgrade\n",
					"stay admin-only",
				} {
					if !strings.Contains(msg, part) {
						t.Errorf("the refusal does not say %q:\n%s", part, msg)
					}
				}
				var names []string
				for _, u := range upgradeUsers(tc.cfg) {
					names = append(names, u.Name)
				}
				if !strings.Contains(msg, "the users in auth.users are now "+strings.Join(names, ", ")) {
					t.Errorf("the refusal does not name the users now (%v):\n%s", names, msg)
				}
				if len(suggestedServerCommands(msg)) < 2 {
					t.Errorf("the refusal suggests no command: %q", suggestedServerCommands(msg))
				}
				checkSuggestedServerCommands(t, msg, append(names, "alice", state, marker)...)
			case "abandoned":
				if err != nil {
					t.Fatal(err)
				}
				if got := readMarker(t, state); got != "done" {
					t.Errorf("marker %q, want done", got)
				}
				if !strings.Contains(logs.String(), "the unfinished one-time upgrade step for user alice was abandoned, because the configured users are now") {
					t.Errorf("the abandonment was not logged:\n%s", logs.String())
				}
				if _, ok := be.assigned["hosty"]; ok {
					t.Errorf("hosty stamped: %v", be.assigned)
				}
				if _, ok := be.snapAssigned["snap-b"]; ok {
					t.Errorf("snap-b stamped: %v", be.snapAssigned)
				}
				if !reflect.DeepEqual(rep.Unassigned, []string{"hosty"}) {
					t.Errorf("report %+v; want hosty left with no user", rep)
				}
			case "resumed":
				if err != nil {
					t.Fatal(err)
				}
				if be.assigned["hosty"] != "alice" || be.snapAssigned["snap-b"] != "alice" {
					t.Errorf("sheds %v snapshots %v; want hosty and snap-b to alice", be.assigned, be.snapAssigned)
				}
				if got := readMarker(t, state); got != "done" {
					t.Errorf("marker %q, want done", got)
				}
				if !strings.Contains(logs.String(), "Users: resuming the unfinished one-time upgrade step for user alice") {
					t.Errorf("the resumption was not logged:\n%s", logs.String())
				}
			case "flag-refused":
				if !isAbandonRefusal(err) {
					t.Fatalf("err %v; want the flag refused", err)
				}
				if strings.Contains(err.Error(), "\n") {
					t.Errorf("the flag's refusal is more than one line: %q", err)
				}
				unchanged()
				checkSuggestedServerCommands(t, err.Error(), "alice", state, marker)
			case "removed":
				if err != nil {
					t.Fatal(err)
				}
				if got := readMarker(t, state); got != "" {
					t.Errorf("marker %q after a start with no user list, want none", got)
				}
				if _, ok := be.assigned["hosty"]; ok {
					t.Error("a start with no user list stamped hosty")
				}
			}
		})
	}
}

// TestServeAbandonFlagWithNoPendingStep: the flag on a first start with a
// user list, or after the step is done, is refused and writes no marker
// (or leaves the done one as it is).
func TestServeAbandonFlagWithNoPendingStep(t *testing.T) {
	one, _, _ := wiringConfigs(t)
	be := newListOnlyBackend()

	state := t.TempDir()
	if _, _, err := stampUnassignedWith(one, be, state, nil, true); !isAbandonRefusal(err) || !strings.Contains(err.Error(), "first start with a user list") {
		t.Fatalf("a first start with the flag: %v; want the flag refused", err)
	}
	if got := readMarker(t, state); got != "" || len(be.assigned) != 0 {
		t.Fatalf("the refused first start wrote marker %q, stamped %v", got, be.assigned)
	}

	stampNoBindings(t, one, be, state)
	if got := readMarker(t, state); got != "done" {
		t.Fatalf("test set-up: marker %q, want done", got)
	}
	before, _ := os.ReadFile(filepath.Join(state, backend.UsersUpgradeFile))
	if _, _, err := stampUnassignedWith(one, be, state, nil, true); !isAbandonRefusal(err) || !strings.Contains(err.Error(), "is done") {
		t.Fatalf("a start with the flag after the step is done: %v; want the flag refused", err)
	}
	if after, _ := os.ReadFile(filepath.Join(state, backend.UsersUpgradeFile)); !bytes.Equal(after, before) {
		t.Errorf("the refused start rewrote the done marker: %s", after)
	}
}

// writeServeConfigIn is writeServeConfig with every backend path, and the
// server's state directory, under dir, followed by auth.
func writeServeConfigIn(t *testing.T, dir, auth string) {
	t.Helper()
	var be string
	switch runtime.GOOS {
	case "darwin":
		be = "default_backend: vz\nvz:\n  vfkit_path: vfkit\n  kernel_path: /dev/null\n  default_image: /dev/null\n  instance_dir: " + dir + "/vm/instances\n  socket_dir: " + dir + "/vm/sockets\n  images_dir: " + dir + "/vm/images\n  snapshots_dir: " + dir + "/vm/snapshots\n  uppers_dir: " + dir + "/vm/uppers\n  default_cpus: 2\n  default_memory_mb: 4096\n  default_disk_gb: 20\n  console_port: 1024\n  notify_port: 1026\n"
	case "linux":
		be = "default_backend: firecracker\nfirecracker:\n  kernel_path: /dev/null\n  default_image: /dev/null\n  instance_dir: " + dir + "/vm/instances\n  socket_dir: " + dir + "/vm/sockets\n  images_dir: " + dir + "/vm/images\n  snapshots_dir: " + dir + "/vm/snapshots\n  uppers_dir: " + dir + "/vm/uppers\n  default_cpus: 2\n  default_memory_mb: 4096\n  default_disk_gb: 20\n  vsock_base_cid: 100\n  console_port: 1024\n  notify_port: 1026\n  bridge_name: shed-br0\n  bridge_cidr: 172.30.0.1/24\n  tap_prefix: shed-tap\n"
	default:
		t.Skipf("no backend on %s", runtime.GOOS)
	}
	path := filepath.Join(dir, "server.yaml")
	body := "name: test\nstate_dir: " + filepath.Join(dir, "state") + "\nssh_port: 1\nhttps_port: 2\n" + be + auth
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	prev := configPath
	configPath = path
	t.Cleanup(func() { configPath = prev })
}

// TestServeRefusesBeforeAnythingOpens: serve itself, with alice's step
// pending and bob configured, returns the refusal before it opens a backend
// or a store: no backend directory and no credential source bindings exist
// after it, so no listener (opened long after both) was reached. The SSH and
// HTTPS ports are ones an unprivileged test cannot bind, so a start that went
// on to its listeners could not pass as this refusal either. With the flag
// given and nothing pending, serve refuses the flag the same way.
func TestServeRefusesBeforeAnythingOpens(t *testing.T) {
	for _, tc := range []struct {
		name    string
		pending bool
		abandon bool
		refused func(error) bool
	}{
		{"pending for alice, bob configured", true, false, func(err error) bool { return isChangedRefusal(err, "alice") }},
		{"nothing pending, the flag given", false, true, isAbandonRefusal},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			bobLine, _ := wiringKey(t)
			writeServeConfigIn(t, dir, "auth:\n  mode: mtls\n  users:\n    bob:\n      admin: true\n      keys:\n        - {key: \""+bobLine+"\", label: laptop}\n")
			state := filepath.Join(dir, "state")
			var markerBefore []byte
			if tc.pending {
				up := backend.OpenUsersUpgrade(state, true, nil)
				if plan, err := up.Begin([]backend.UpgradeUser{{Name: "alice", Admin: true}}, false, time.Now()); err != nil || plan.StampFor == nil {
					t.Fatalf("test set-up: plan %+v err %v", plan, err)
				}
				markerBefore, _ = os.ReadFile(filepath.Join(state, backend.UsersUpgradeFile))
			}
			prev := serveAbandonUserUpgrade
			serveAbandonUserUpgrade = tc.abandon
			t.Cleanup(func() { serveAbandonUserUpgrade = prev })
			captureLog(t)

			err := runServe(serveCmd, nil)
			if !tc.refused(err) {
				t.Fatalf("serve returned %v; want the start refused", err)
			}
			for _, p := range []string{filepath.Join(dir, "vm"), filepath.Join(dir, "vm", source.FileName)} {
				if _, err := os.Lstat(p); !errors.Is(err, os.ErrNotExist) {
					t.Errorf("%s exists after the refused start (%v): the start went past the backend", p, err)
				}
			}
			markerAfter, _ := os.ReadFile(filepath.Join(state, backend.UsersUpgradeFile))
			if !bytes.Equal(markerAfter, markerBefore) {
				t.Errorf("the marker changed: %q, was %q", markerAfter, markerBefore)
			}
		})
	}
}

// TestUpgradeRefusalsEscapeTheMarkerPath: state_dir may be any absolute path,
// a name with a line break included; the refusals show the marker's path
// quoted, so it cannot add a line to them.
func TestUpgradeRefusalsEscapeTheMarkerPath(t *testing.T) {
	state := filepath.Join(t.TempDir(), "st\nate")
	if err := os.MkdirAll(state, 0o700); err != nil {
		t.Fatal(err)
	}
	// A first start with the flag: refused, naming the marker's path.
	err := backend.CheckUsersUpgrade(state, []backend.UpgradeUser{{Name: "alice"}}, true)
	if err == nil {
		t.Fatal("the flag with nothing pending was not refused")
	}
	if msg := err.Error(); strings.Contains(msg, "st\nate") || !strings.Contains(msg, `st\nate`) {
		t.Errorf("the marker path is not shown escaped: %q", msg)
	}
}
