//go:build linux

package firecracker

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

var (
	asRoot = backend.WithPrincipal(context.Background(), backend.UserPrincipal("root", true))
	asBob  = backend.WithPrincipal(context.Background(), backend.UserPrincipal("bob", false))
)

// legacyShed writes a shed made before users existed: no user, no owner, and
// (unless edit says otherwise) an explicitly empty server-mount selection.
func legacyShed(t *testing.T, c *Client, cfg *config.FirecrackerConfig, name string, edit func(*Metadata)) *Metadata {
	t.Helper()
	return snapshotSource(t, c, cfg, name, func(m *Metadata) {
		m.Status = config.StatusStopped
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
	c, cfg := newSnapshotUserClient(t)
	legacyShed(t, c, cfg, "legacy", nil)
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
	meta, err := LoadMetadata(cfg.InstanceDir, "legacy")
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

// runningLegacyShed is a running shed "box" with no user on the bus of a
// server with users (the resume walk registered it).
func runningLegacyShed(t *testing.T) (*Client, *plugin.Bridge, *Metadata, string) {
	t.Helper()
	tmpDir := t.TempDir()
	cfg := testFirecrackerConfig(tmpDir)
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
		usedCIDs:    make(map[uint32]string),
		usedIPs:     make(map[string]string),
		p9Servers:   make(map[string][]*P9Server),
		credMgr:     vmutil.NewCredentialManager(nil, bridge, string(config.BackendFirecracker), vmutil.NewHealthTracker()),
		procCmdline: func(context.Context, int) (string, error) { return fcCmdline(cfg, "box"), nil },
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
	c, bridge, meta, tmpDir := runningLegacyShed(t)

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
// one that carries host directories, and a shed made before mount selection
// existed (no selection stored, reported {mode: all}).
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
			c, cfg := newSnapshotUserClient(t)
			before := legacyShed(t, c, cfg, "box", tc.edit)
			raw, _ := os.ReadFile(MetadataPath(cfg.InstanceDir, "box"))
			g := backend.NewGuard(NewBackend(c))
			_, err := g.AssignUser(asRoot, "box", config.AssignShedRequest{User: "bob"})
			if !errors.Is(err, tc.want) {
				t.Fatalf("assign: %v, want %v", err, tc.want)
			}
			if after, _ := os.ReadFile(MetadataPath(cfg.InstanceDir, "box")); string(after) != string(raw) {
				t.Errorf("the record changed: %s", after)
			}
			// The raw backend refuses the same, under its lock.
			_, err = NewBackend(c).AssignUser(backend.WithRequiredCreatedAt(context.Background(), before.CreatedAt), "box", config.AssignShedRequest{User: "bob"})
			if !errors.Is(err, tc.want) {
				t.Errorf("raw backend: %v, want %v", err, tc.want)
			}
		})
	}
}

// After the guard has looked up and authorized the unassigned shed, it is
// deleted and created again under the name, still with no user (forced with
// the guard's hook). The backend, under its lock, finds another incarnation
// and refuses; the newcomer stays unassigned.
func TestAssignUserAcrossARecreate(t *testing.T) {
	c, cfg := newSnapshotUserClient(t)
	old := legacyShed(t, c, cfg, "legacy", nil)
	g := backend.NewGuard(NewBackend(c))
	var newcomer *Metadata
	g.SetBeforeAct(func(op, name string) {
		if op != "AssignUser" || newcomer != nil {
			return
		}
		if err := old.Delete(cfg.InstanceDir); err != nil {
			t.Fatal(err)
		}
		newcomer = legacyShed(t, c, cfg, "legacy", func(m *Metadata) { m.CreatedAt = old.CreatedAt.Add(time.Minute) })
	})
	if _, err := g.AssignUser(asRoot, "legacy", config.AssignShedRequest{User: "bob"}); !errors.Is(err, config.ErrShedNotFoundSentinel) {
		t.Fatalf("assign across a recreate: %v, want not found", err)
	}
	if newcomer == nil {
		t.Fatal("the guard never reached its hook")
	}
	now, err := LoadMetadata(cfg.InstanceDir, "legacy")
	if err != nil {
		t.Fatal(err)
	}
	if now.User != "" || !now.CreatedAt.Equal(newcomer.CreatedAt) {
		t.Errorf("the newcomer was assigned: user %q created %s", now.User, now.CreatedAt)
	}
}

// writeLegacySnapshot writes a snapshot.json as a build before schema 2 did:
// no version, no user.
func writeLegacySnapshot(t *testing.T, cfg *config.FirecrackerConfig, name string, created time.Time) {
	t.Helper()
	dir := SnapshotDir(cfg.SnapshotsDir, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := `{"name":"` + name + `","backend":"firecracker","source_shed":"gone","created_at":"` + created.UTC().Format(time.RFC3339Nano) + `"}`
	if err := os.WriteFile(filepath.Join(dir, snapshotMetadataFilename), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// A snapshot with no user is stamped under the snapshot lock, bound to the
// snapshot that was looked up; only the user changes (the schema version it
// was written with is kept), and a snapshot that has a user is refused.
func TestStampSnapshotUser(t *testing.T) {
	c, cfg := newSnapshotUserClient(t)
	at := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	writeLegacySnapshot(t, cfg, "old", at)
	b := NewBackend(c)

	// Bound to another incarnation: refused, nothing written.
	if _, err := b.StampSnapshotUser(backend.WithRequiredSnapshot(context.Background(), at.Add(time.Minute), ""), "old", "bob"); !errors.Is(err, config.ErrSnapshotNotFoundSentinel) {
		t.Fatalf("stamp of a retaken snapshot: %v, want not found", err)
	}
	if snap, _ := loadSnapshot(cfg.SnapshotsDir, "old"); snap.User != "" {
		t.Fatalf("a refused stamp wrote user %q", snap.User)
	}

	snap, err := b.StampSnapshotUser(backend.WithRequiredSnapshot(context.Background(), at, ""), "old", "bob")
	if err != nil {
		t.Fatalf("stamp: %v", err)
	}
	if snap.User != "bob" {
		t.Errorf("stamp returned user %q", snap.User)
	}
	got, err := loadSnapshot(cfg.SnapshotsDir, "old")
	if err != nil {
		t.Fatal(err)
	}
	if got.User != "bob" || got.Version != 1 || got.SourceShed != "gone" || !got.CreatedAt.Equal(at) {
		t.Errorf("stored snapshot %+v, want user bob at version 1, otherwise unchanged", got)
	}
	if _, err := b.StampSnapshotUser(context.Background(), "old", "alice"); !errors.Is(err, config.ErrShedAssignedSentinel) {
		t.Errorf("stamping it again: %v, want already assigned", err)
	}
}

// The start-up pass on Firecracker records: with exactly one user (an admin)
// every shed and snapshot with no user becomes theirs, a shed made before
// mount selection included; another user's shed and a service client's are
// left alone. With two users nothing is stamped.
func TestStampUnassignedOnFirecracker(t *testing.T) {
	setup := func(t *testing.T) (*Client, *config.FirecrackerConfig) {
		c, cfg := newSnapshotUserClient(t)
		legacyShed(t, c, cfg, "legacy", nil)
		legacyShed(t, c, cfg, "old-all", func(m *Metadata) { m.MountSelection = nil })
		legacyShed(t, c, cfg, "alices", func(m *Metadata) { m.User = "alice" })
		legacyShed(t, c, cfg, "svc", func(m *Metadata) { m.Owner = "nocturn" })
		legacyShed(t, c, cfg, "odd", func(m *Metadata) { m.Owner, m.User = "nocturn", "alice" })
		writeLegacySnapshot(t, cfg, "old-snap", time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC))
		return c, cfg
	}
	stored := func(t *testing.T, cfg *config.FirecrackerConfig) map[string]string {
		out := map[string]string{}
		for _, n := range []string{"legacy", "old-all", "alices", "svc", "odd"} {
			m, err := LoadMetadata(cfg.InstanceDir, n)
			if err != nil {
				t.Fatal(err)
			}
			out[n] = m.Owner + "/" + m.User
		}
		s, err := loadSnapshot(cfg.SnapshotsDir, "old-snap")
		if err != nil {
			t.Fatal(err)
		}
		out["old-snap"] = s.User
		return out
	}
	var lines []string
	log := backend.UpgradeLog{Infof: func(f string, a ...any) { lines = append(lines, f) }}

	t.Run("one admin", func(t *testing.T) {
		c, cfg := setup(t)
		rep := backend.StampUnassigned(context.Background(), NewBackend(c), []backend.UpgradeUser{{Name: "root", Admin: true}}, true, log)
		want := map[string]string{"legacy": "/root", "old-all": "/root", "alices": "/alice", "svc": "nocturn/", "odd": "nocturn/alice", "old-snap": "root"}
		if got := stored(t, cfg); !equalMaps(got, want) {
			t.Errorf("stored %v, want %v", got, want)
		}
		if len(rep.StampedSheds) != 2 || len(rep.StampedSnapshots) != 1 || len(rep.Unassigned) != 0 {
			t.Errorf("report %+v", rep)
		}
	})
	t.Run("two users", func(t *testing.T) {
		c, cfg := setup(t)
		before := stored(t, cfg)
		rep := backend.StampUnassigned(context.Background(), NewBackend(c), []backend.UpgradeUser{{Name: "root", Admin: true}, {Name: "bob"}}, true, log)
		if got := stored(t, cfg); !equalMaps(got, before) {
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
	c, cfg := newSnapshotUserClient(t)
	legacyShed(t, c, cfg, "legacy", nil)
	at := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	writeLegacySnapshot(t, cfg, "old", at)
	files, dirs := setSyncs(t, nil, nil)

	meta, err := LoadMetadata(cfg.InstanceDir, "legacy")
	if err != nil {
		t.Fatal(err)
	}
	if err := meta.Save(cfg.InstanceDir); err != nil {
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
		recordUnchanged bool
	}{
		{"file sync", errors.New("file sync: I/O error"), nil, true},
		{"directory sync", nil, errors.New("directory sync: I/O error"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, bridge, meta, tmpDir := runningLegacyShed(t)
			setSyncs(t, tc.fileErr, tc.dirErr)
			_, err := NewBackend(c).AssignUser(backend.WithRequiredCreatedAt(context.Background(), meta.CreatedAt), "box", config.AssignShedRequest{User: "bob"})
			if err == nil || !strings.Contains(err.Error(), "I/O error") {
				t.Fatalf("assign with a failing sync: %v", err)
			}
			if u, ok := bridge.ShedUser("box"); ok {
				t.Errorf("the route moved to %q on a failed assignment", u)
			}
			if got, _ := LoadMetadata(tmpDir, "box"); tc.recordUnchanged && got.User != "" {
				t.Errorf("stored user %q after a failed file sync", got.User)
			}
		})
	}
	t.Run("snapshot stamp", func(t *testing.T) {
		c, cfg := newSnapshotUserClient(t)
		at := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
		writeLegacySnapshot(t, cfg, "old", at)
		setSyncs(t, errors.New("file sync: I/O error"), nil)
		if _, err := NewBackend(c).StampSnapshotUser(backend.WithRequiredSnapshot(context.Background(), at, ""), "old", "bob"); err == nil || !strings.Contains(err.Error(), "I/O error") {
			t.Fatalf("stamp with a failing sync: %v", err)
		}
		if snap, _ := loadSnapshot(cfg.SnapshotsDir, "old"); snap.User != "" {
			t.Errorf("stored user %q after a failed stamp", snap.User)
		}
	})
}

// The listing the start-up pass uses says how many shed records it could
// not read, so the report does not count them as having a user.
func TestListShedsCountsUnreadableRecords(t *testing.T) {
	c, cfg := newSnapshotUserClient(t)
	legacyShed(t, c, cfg, "legacy", nil)
	if err := os.MkdirAll(InstanceDir(cfg.InstanceDir, "broken"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(MetadataPath(cfg.InstanceDir, "broken"), []byte("{"), 0o600); err != nil {
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
