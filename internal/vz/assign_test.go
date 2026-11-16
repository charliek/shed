//go:build darwin

package vz

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/charliek/shed/internal/backend"
	"github.com/charliek/shed/internal/config"
	"github.com/charliek/shed/internal/plugin"
	"github.com/charliek/shed/internal/vmutil"
)

// The VZ twin of internal/firecracker/assign_test.go: the same cases, case
// for case, over VZ's metadata and resume walk.

var (
	asRoot = backend.WithPrincipal(context.Background(), backend.UserPrincipal("root", true))
	asBob  = backend.WithPrincipal(context.Background(), backend.UserPrincipal("bob", false))
)

// legacyVZShed writes a shed made before users existed: no user, no owner,
// and (unless edit says otherwise) an explicitly empty mount selection.
func legacyVZShed(t *testing.T, dir, name string, edit func(*Metadata)) *Metadata {
	t.Helper()
	return saveUserTestShed(t, dir, name, func(m *Metadata) {
		m.MountSelection = config.SelectedMounts(nil)
		if edit != nil {
			edit(m)
		}
	})
}

// A stopped shed with no user is assigned through the guard by an admin and
// becomes the user's: the record says so (same metadata version), the user
// may enter it, and the admin may view it but not enter it.
func TestAssignUserStoppedShed(t *testing.T) {
	dir := t.TempDir()
	c := newUserTestClient(t, dir)
	legacyVZShed(t, dir, "legacy", nil)
	g := backend.NewGuard(NewBackend(c))

	if _, err := g.Check(asBob, "legacy", backend.ActionEnter); !errors.Is(err, config.ErrShedNotFoundSentinel) {
		t.Fatalf("bob before the assignment: %v, want not found", err)
	}
	shed, err := g.AssignUser(asRoot, "legacy", config.AssignShedRequest{User: "bob"})
	if err != nil {
		t.Fatalf("assign: %v", err)
	}
	if shed.User != "bob" || shed.Status != config.StatusStopped {
		t.Errorf("assign returned user %q status %q", shed.User, shed.Status)
	}
	meta, err := LoadMetadata(dir, "legacy")
	if err != nil {
		t.Fatal(err)
	}
	if meta.User != "bob" || meta.Owner != "" || meta.Version != MetadataVersion {
		t.Errorf("stored user %q owner %q version %d", meta.User, meta.Owner, meta.Version)
	}
	if _, err := g.Check(asBob, "legacy", backend.ActionEnter); err != nil {
		t.Errorf("bob entering his shed: %v", err)
	}
	if _, err := g.Check(asRoot, "legacy", backend.ActionEnter); !errors.Is(err, config.ErrForbiddenSentinel) {
		t.Errorf("admin entering bob's shed: %v, want forbidden", err)
	}
	if _, err := g.AssignUser(asRoot, "legacy", config.AssignShedRequest{User: "alice"}); !errors.Is(err, config.ErrShedAssignedSentinel) {
		t.Errorf("assigning it again: %v, want already assigned", err)
	}
}

// runningLegacyVZShed is a running shed "box" with no user on the bus of a
// server with users (the resume walk registered it).
func runningLegacyVZShed(t *testing.T) (*Client, *plugin.Bridge, *Metadata, string) {
	t.Helper()
	tmpDir := t.TempDir()
	cfg := &config.VZConfig{
		InstanceDir: tmpDir,
		SocketDir:   filepath.Join(tmpDir, "sockets"),
		ConsolePort: 1024,
		NotifyPort:  1026,
		VfkitPath:   defaultTestVfkitPath,
	}
	writeResumeInstance(t, tmpDir, "box", config.StatusRunning, 4242)
	meta, err := LoadMetadata(tmpDir, "box")
	if err != nil {
		t.Fatal(err)
	}
	meta.MountSelection = config.SelectedMounts(nil)
	if err := meta.Save(tmpDir); err != nil {
		t.Fatal(err)
	}
	bridge := plugin.NewBridge(plugin.NewRegistry(), true)
	c := &Client{
		cfg:         cfg,
		vms:         make(map[string]*VM),
		credMgr:     vmutil.NewCredentialManager(nil, bridge, string(config.BackendVZ), vmutil.NewHealthTracker()),
		procCmdline: func(context.Context, int) (string, error) { return vfkitCmdline(cfg, "box"), nil },
	}
	t.Cleanup(func() { _ = c.Close() })
	c.ResumeRunningInstances(context.Background())
	if got := resumedNames(bridge); len(got) != 1 || got[0] != "box" {
		t.Fatalf("test set-up: the running shed is not on the bus (%v)", got)
	}
	if u, ok := bridge.ShedUser("box"); ok {
		t.Fatalf("a shed with no user routes to %q before the assignment", u)
	}
	return c, bridge, meta, tmpDir
}

// A running shed's credential-bus route follows the assignment at once,
// without a reconnect: before it, a shed with no user routes nowhere on a
// server with users; after it, to its new user.
func TestAssignUserRunningShedMovesTheBusRoute(t *testing.T) {
	c, bridge, meta, tmpDir := runningLegacyVZShed(t)

	if _, err := NewBackend(c).AssignUser(backend.WithRequiredCreatedAt(context.Background(), meta.CreatedAt), "box", config.AssignShedRequest{User: "bob"}); err != nil {
		t.Fatalf("assign: %v", err)
	}
	if u, ok := bridge.ShedUser("box"); !ok || u != "bob" {
		t.Errorf("after the assignment the shed routes to (%q, %v), want bob", u, ok)
	}
	if u, ok := bridge.IncarnationUser("box", meta.CreatedAt); !ok || u != "bob" {
		t.Errorf("IncarnationUser after the assignment = (%q, %v), want bob", u, ok)
	}
	if got, _ := LoadMetadata(tmpDir, "box"); got.User != "bob" {
		t.Errorf("stored user %q", got.User)
	}
}

// Each refusal leaves the record as it was: a shed that belongs to someone,
// one that carries host directories, a shed made before mount selection
// existed (no selection stored, reported {mode: all}), and one that selects
// every server mount explicitly.
func TestAssignUserRefusals(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(*Metadata)
		want error
	}{
		{"another user's", func(m *Metadata) { m.User = "alice" }, config.ErrShedAssignedSentinel},
		{"a service client's", func(m *Metadata) { m.Owner = "nocturn" }, config.ErrShedAssignedSentinel},
		{"both (unresolved)", func(m *Metadata) { m.Owner, m.User = "nocturn", "alice" }, config.ErrShedAssignedSentinel},
		{"host directories", func(m *Metadata) {
			m.ProjectMounts = []config.MountConfig{{Source: "/srv/code", Target: "/home/shed/code"}}
		}, config.ErrShedHostResourcesSentinel},
		{"no mount selection stored", func(m *Metadata) { m.MountSelection = nil }, config.ErrShedHostResourcesSentinel},
		{"every server mount", func(m *Metadata) { m.MountSelection = config.AllMounts() }, config.ErrShedHostResourcesSentinel},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			c := newUserTestClient(t, dir)
			before := legacyVZShed(t, dir, "box", tc.edit)
			raw, _ := os.ReadFile(MetadataPath(dir, "box"))
			g := backend.NewGuard(NewBackend(c))
			_, err := g.AssignUser(asRoot, "box", config.AssignShedRequest{User: "bob"})
			if !errors.Is(err, tc.want) {
				t.Fatalf("assign: %v, want %v", err, tc.want)
			}
			if after, _ := os.ReadFile(MetadataPath(dir, "box")); string(after) != string(raw) {
				t.Errorf("the record changed: %s", after)
			}
			// The raw backend refuses the same, under its lock.
			_, err = NewBackend(c).AssignUser(backend.WithRequiredCreatedAt(context.Background(), before.CreatedAt), "box", config.AssignShedRequest{User: "bob"})
			if !errors.Is(err, tc.want) {
				t.Errorf("raw backend: %v, want %v", err, tc.want)
			}
			if after, _ := os.ReadFile(MetadataPath(dir, "box")); string(after) != string(raw) {
				t.Errorf("the raw backend changed the record: %s", after)
			}
		})
	}
}

// After the guard has looked up and authorized the unassigned shed, it is
// deleted and created again under the name, still with no user (forced with
// the guard's hook). The backend, under its lock, finds another incarnation
// and refuses; the newcomer stays unassigned.
func TestAssignUserAcrossARecreate(t *testing.T) {
	dir := t.TempDir()
	c := newUserTestClient(t, dir)
	old := legacyVZShed(t, dir, "legacy", nil)
	g := backend.NewGuard(NewBackend(c))
	var newcomer *Metadata
	g.SetBeforeAct(func(op, name string) {
		if op != "AssignUser" || newcomer != nil {
			return
		}
		if err := old.Delete(dir); err != nil {
			t.Fatal(err)
		}
		newcomer = legacyVZShed(t, dir, "legacy", func(m *Metadata) { m.CreatedAt = old.CreatedAt.Add(time.Minute) })
	})
	if _, err := g.AssignUser(asRoot, "legacy", config.AssignShedRequest{User: "bob"}); !errors.Is(err, config.ErrShedNotFoundSentinel) {
		t.Fatalf("assign across a recreate: %v, want not found", err)
	}
	if newcomer == nil {
		t.Fatal("the guard never reached its hook")
	}
	now, err := LoadMetadata(dir, "legacy")
	if err != nil {
		t.Fatal(err)
	}
	if now.User != "" || !now.CreatedAt.Equal(newcomer.CreatedAt) {
		t.Errorf("the newcomer was assigned: user %q created %s", now.User, now.CreatedAt)
	}
}

// writeLegacyVZSnapshot writes a snapshot.json as a build before schema 2
// did: no version, no user.
func writeLegacyVZSnapshot(t *testing.T, snapshotsDir, name string, created time.Time) {
	t.Helper()
	snapDir := SnapshotDir(snapshotsDir, name)
	if err := os.MkdirAll(snapDir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := `{"name":"` + name + `","backend":"vz","created_at":"` + created.UTC().Format(time.RFC3339Nano) + `"}`
	if err := os.WriteFile(filepath.Join(snapDir, snapshotMetadataFilename), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// A snapshot with no user is stamped under the snapshot lock, bound to the
// snapshot that was looked up; only the user changes (the schema version it
// was written with is kept), and a snapshot that has a user is refused.
func TestStampSnapshotUser(t *testing.T) {
	dir := t.TempDir()
	c := newUserTestClient(t, dir)
	at := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	writeLegacyVZSnapshot(t, c.cfg.SnapshotsDir, "old", at)
	b := NewBackend(c)
	if _, err := b.StampSnapshotUser(backend.WithRequiredSnapshot(context.Background(), at.Add(time.Minute), ""), "old", "bob"); !errors.Is(err, config.ErrSnapshotNotFoundSentinel) {
		t.Fatalf("stamp of a retaken snapshot: %v", err)
	}
	if snap, _ := loadSnapshot(c.cfg.SnapshotsDir, "old"); snap.User != "" {
		t.Fatalf("a refused stamp wrote user %q", snap.User)
	}
	if _, err := b.StampSnapshotUser(backend.WithRequiredSnapshot(context.Background(), at, ""), "old", "bob"); err != nil {
		t.Fatalf("stamp: %v", err)
	}
	got, err := loadSnapshot(c.cfg.SnapshotsDir, "old")
	if err != nil || got.User != "bob" || got.Version != 1 || !got.CreatedAt.Equal(at) {
		t.Fatalf("stored snapshot %+v %v, want user bob at version 1, otherwise unchanged", got, err)
	}
	if _, err := b.StampSnapshotUser(context.Background(), "old", "alice"); !errors.Is(err, config.ErrShedAssignedSentinel) {
		t.Errorf("stamping it again: %v", err)
	}
}

// The start-up pass on VZ records: with exactly one user (an admin) every
// shed and snapshot with no user becomes theirs, a shed made before mount
// selection included; another user's shed and a service client's are left
// alone. With two users nothing is stamped.
func TestStampUnassignedOnVZ(t *testing.T) {
	setup := func(t *testing.T) (*Client, string) {
		dir := t.TempDir()
		c := newUserTestClient(t, dir)
		legacyVZShed(t, dir, "legacy", nil)
		legacyVZShed(t, dir, "old-all", func(m *Metadata) { m.MountSelection = nil })
		legacyVZShed(t, dir, "alices", func(m *Metadata) { m.User = "alice" })
		legacyVZShed(t, dir, "svc", func(m *Metadata) { m.Owner = "nocturn" })
		legacyVZShed(t, dir, "odd", func(m *Metadata) { m.Owner, m.User = "nocturn", "alice" })
		writeLegacyVZSnapshot(t, c.cfg.SnapshotsDir, "old-snap", time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC))
		return c, dir
	}
	stored := func(t *testing.T, c *Client, dir string) map[string]string {
		out := map[string]string{}
		for _, n := range []string{"legacy", "old-all", "alices", "svc", "odd"} {
			m, err := LoadMetadata(dir, n)
			if err != nil {
				t.Fatal(err)
			}
			out[n] = m.Owner + "/" + m.User
		}
		s, err := loadSnapshot(c.cfg.SnapshotsDir, "old-snap")
		if err != nil {
			t.Fatal(err)
		}
		out["old-snap"] = s.User
		return out
	}
	log := backend.UpgradeLog{}

	t.Run("one admin", func(t *testing.T) {
		c, dir := setup(t)
		rep := backend.StampUnassigned(context.Background(), NewBackend(c), []backend.UpgradeUser{{Name: "root", Admin: true}}, true, log)
		want := map[string]string{"legacy": "/root", "old-all": "/root", "alices": "/alice", "svc": "nocturn/", "odd": "nocturn/alice", "old-snap": "root"}
		if got := stored(t, c, dir); !equalMaps(got, want) {
			t.Errorf("stored %v, want %v", got, want)
		}
		if len(rep.StampedSheds) != 2 || len(rep.StampedSnapshots) != 1 || len(rep.Unassigned) != 0 || rep.Incomplete {
			t.Errorf("report %+v", rep)
		}
	})
	t.Run("two users", func(t *testing.T) {
		c, dir := setup(t)
		before := stored(t, c, dir)
		rep := backend.StampUnassigned(context.Background(), NewBackend(c), []backend.UpgradeUser{{Name: "root", Admin: true}, {Name: "bob"}}, true, log)
		if got := stored(t, c, dir); !equalMaps(got, before) {
			t.Errorf("stored %v, want unchanged %v", got, before)
		}
		if strings.Join(rep.Unassigned, ",") != "legacy,old-all" || strings.Join(rep.Blocked, ",") != "old-all" {
			t.Errorf("report %+v", rep)
		}
	})
}

func equalMaps(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

// setSyncs replaces the syncs Metadata.SaveDurable and writeSnapshotDurable
// make for the test, counting them, and makes them fail with the errors
// given (nil: succeed).
func setSyncs(t *testing.T, fileErr, dirErr error) (files, dirs *int) {
	t.Helper()
	files, dirs = new(int), new(int)
	oldFile, oldDir := metadataFileSync, metadataDirSync
	metadataFileSync = func(f *os.File) error {
		*files++
		if fileErr != nil {
			return fileErr
		}
		return oldFile(f)
	}
	metadataDirSync = func(dir string) error {
		*dirs++
		if dirErr != nil {
			return dirErr
		}
		return oldDir(dir)
	}
	t.Cleanup(func() { metadataFileSync, metadataDirSync = oldFile, oldDir })
	return files, dirs
}

// An assignment and a snapshot stamp are durable before they are
// acknowledged: the file is synced before its rename and the directory after
// it. The general metadata writer (create, start, stop) is not synced.
func TestAssignmentIsDurable(t *testing.T) {
	dir := t.TempDir()
	c := newUserTestClient(t, dir)
	meta := legacyVZShed(t, dir, "legacy", nil)
	at := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	writeLegacyVZSnapshot(t, c.cfg.SnapshotsDir, "old", at)
	files, dirs := setSyncs(t, nil, nil)

	if err := meta.Save(dir); err != nil {
		t.Fatal(err)
	}
	if *files != 0 || *dirs != 0 {
		t.Fatalf("the general metadata writer synced (%d file, %d directory syncs)", *files, *dirs)
	}
	if _, err := NewBackend(c).AssignUser(backend.WithRequiredCreatedAt(context.Background(), meta.CreatedAt), "legacy", config.AssignShedRequest{User: "bob"}); err != nil {
		t.Fatalf("assign: %v", err)
	}
	if *files != 1 || *dirs != 1 {
		t.Errorf("the assignment made %d file and %d directory syncs, want 1 and 1", *files, *dirs)
	}
	if _, err := NewBackend(c).StampSnapshotUser(backend.WithRequiredSnapshot(context.Background(), at, ""), "old", "bob"); err != nil {
		t.Fatalf("stamp: %v", err)
	}
	if *files != 2 || *dirs != 2 {
		t.Errorf("the stamp made %d file and %d directory syncs in all, want 2 and 2", *files, *dirs)
	}
}

// A sync that fails fails the assignment, and a running shed's bus route
// does not move: a failed file sync leaves the record as it was; a failed
// directory sync (after the rename) is reported as a failure too. A snapshot
// stamp whose sync fails is refused the same way.
func TestAssignmentSyncFailure(t *testing.T) {
	for _, tc := range []struct {
		name            string
		fileErr, dirErr error
		// wantStored is the user visible on disk after the failure: none
		// after a failed file sync (the rename never happened), the new
		// user after a failed directory sync (the rename did).
		wantStored string
	}{
		{"file sync", errors.New("file sync: I/O error"), nil, ""},
		{"directory sync", nil, errors.New("directory sync: I/O error"), "bob"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, bridge, meta, tmpDir := runningLegacyVZShed(t)
			setSyncs(t, tc.fileErr, tc.dirErr)
			_, err := NewBackend(c).AssignUser(backend.WithRequiredCreatedAt(context.Background(), meta.CreatedAt), "box", config.AssignShedRequest{User: "bob"})
			if err == nil || !strings.Contains(err.Error(), "I/O error") {
				t.Fatalf("assign with a failing sync: %v", err)
			}
			if u, ok := bridge.ShedUser("box"); ok {
				t.Errorf("the route moved to %q on a failed assignment", u)
			}
			got, err := LoadMetadata(tmpDir, "box")
			if err != nil {
				t.Fatal(err)
			}
			if got.User != tc.wantStored {
				t.Errorf("stored user %q after a failed %s, want %q", got.User, tc.name, tc.wantStored)
			}
			if tc.wantStored == "" {
				return
			}
			// The owner visible on disk holds: an assignment to a different
			// user is refused, under the lock, and writes nothing.
			_, err = NewBackend(c).AssignUser(backend.WithRequiredCreatedAt(context.Background(), meta.CreatedAt), "box", config.AssignShedRequest{User: "alice"})
			if !errors.Is(err, config.ErrShedAssignedSentinel) {
				t.Errorf("assign to alice after a failed %s: %v, want already assigned", tc.name, err)
			}
			if got, _ := LoadMetadata(tmpDir, "box"); got.User != tc.wantStored {
				t.Errorf("stored user %q after the refused assignment, want %q", got.User, tc.wantStored)
			}
			if u, ok := bridge.ShedUser("box"); ok {
				t.Errorf("the route moved to %q on a refused assignment", u)
			}
		})
	}
	t.Run("snapshot stamp", func(t *testing.T) {
		dir := t.TempDir()
		c := newUserTestClient(t, dir)
		at := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
		writeLegacyVZSnapshot(t, c.cfg.SnapshotsDir, "old", at)
		setSyncs(t, errors.New("file sync: I/O error"), nil)
		if _, err := NewBackend(c).StampSnapshotUser(backend.WithRequiredSnapshot(context.Background(), at, ""), "old", "bob"); err == nil || !strings.Contains(err.Error(), "I/O error") {
			t.Fatalf("stamp with a failing sync: %v", err)
		}
		if snap, _ := loadSnapshot(c.cfg.SnapshotsDir, "old"); snap.User != "" {
			t.Errorf("stored user %q after a failed stamp", snap.User)
		}
	})
}

// The listing the start-up pass uses says how many shed records it could
// not read, so the report does not count them as having a user.
func TestListShedsCountsUnreadableRecords(t *testing.T) {
	dir := t.TempDir()
	c := newUserTestClient(t, dir)
	legacyVZShed(t, dir, "legacy", nil)
	if err := os.MkdirAll(InstanceDir(dir, "broken"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(MetadataPath(dir, "broken"), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	sheds, unreadable, err := NewBackend(c).ListShedsCounting(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(sheds) != 1 || sheds[0].Name != "legacy" || unreadable != 1 {
		t.Errorf("listed %d sheds, %d unreadable; want legacy and 1", len(sheds), unreadable)
	}
	var lines []string
	rep := backend.StampUnassigned(context.Background(), NewBackend(c), []backend.UpgradeUser{{Name: "root", Admin: true}, {Name: "bob"}}, true,
		backend.UpgradeLog{Infof: func(f string, a ...any) { lines = append(lines, fmt.Sprintf(f, a...)) }})
	if rep.UnreadableSheds != 1 || !strings.Contains(strings.Join(lines, "\n"), "1 shed record(s) could not be read") {
		t.Errorf("report %+v, log %q", rep, lines)
	}
}

// A shed directory whose metadata file cannot even be looked at (here: no
// search permission on the directory, so its Stat fails with something other
// than not-exist) is counted as unreadable by the listing the start-up pass
// uses, not dropped. A directory with no metadata file is still not a shed
// and is not counted.
func TestListShedsCountsUnstattableRecords(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root is not held back by directory permissions")
	}
	dir := t.TempDir()
	c := newUserTestClient(t, dir)
	legacyVZShed(t, dir, "legacy", nil)
	legacyVZShed(t, dir, "locked", nil)
	if err := os.MkdirAll(InstanceDir(dir, "no-metadata"), 0o755); err != nil {
		t.Fatal(err)
	}
	locked := InstanceDir(dir, "locked")
	if err := os.Chmod(locked, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })
	if _, err := os.Stat(MetadataPath(dir, "locked")); err == nil || errors.Is(err, os.ErrNotExist) {
		t.Fatalf("test set-up: the locked shed's metadata Stat gave %v, want a failure other than not-exist", err)
	}

	sheds, unreadable, err := NewBackend(c).ListShedsCounting(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(sheds) != 1 || sheds[0].Name != "legacy" || unreadable != 1 {
		t.Errorf("listed %d sheds, %d unreadable; want legacy and 1 (the locked shed)", len(sheds), unreadable)
	}
	var lines []string
	rep := backend.StampUnassigned(context.Background(), NewBackend(c), []backend.UpgradeUser{{Name: "root", Admin: true}, {Name: "bob"}}, true,
		backend.UpgradeLog{Infof: func(f string, a ...any) { lines = append(lines, fmt.Sprintf(f, a...)) }})
	if rep.UnreadableSheds != 1 || !strings.Contains(strings.Join(lines, "\n"), "1 shed record(s) could not be read") {
		t.Errorf("report %+v, log %q", rep, lines)
	}
}
