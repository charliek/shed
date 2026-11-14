//go:build linux

package firecracker

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/charliek/shed/internal/backend"
	"github.com/charliek/shed/internal/backend/orchestrator"
	"github.com/charliek/shed/internal/config"
	"github.com/charliek/shed/internal/egress"
)

// A metadata file written before users existed loads with no user, and a
// user is saved and read back as written. The metadata version is unchanged.
func TestMetadataUserRoundTrip(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "old"), 0o755); err != nil {
		t.Fatal(err)
	}
	legacy := `{"version": 3, "name": "old", "status": "stopped", "backend": "firecracker"}`
	if err := os.WriteFile(MetadataPath(dir, "old"), []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	meta, err := LoadMetadata(dir, "old")
	if err != nil {
		t.Fatalf("LoadMetadata(legacy): %v", err)
	}
	if meta.User != "" || metadataToShed(meta).User != "" {
		t.Errorf("legacy metadata user = %q, want none", meta.User)
	}

	mine := testMetadata("mine")
	mine.User = "alice"
	if err := mine.Save(dir); err != nil {
		t.Fatal(err)
	}
	got, err := LoadMetadata(dir, "mine")
	if err != nil {
		t.Fatal(err)
	}
	if got.User != "alice" || got.Version != MetadataVersion || MetadataVersion != 3 {
		t.Errorf("reloaded user %q version %d (MetadataVersion %d), want alice at version 3", got.User, got.Version, MetadataVersion)
	}
	if shed := metadataToShed(got); shed.User != "alice" || shed.Owner != "" {
		t.Errorf("Shed: owner %q user %q, want none and alice", shed.Owner, shed.User)
	}
}

// A metadata file that names both a service client and a user is invalid and
// fails closed: it is reported with no owner and config.UserUnresolved.
func TestMetadataOwnerAndUserFailsClosed(t *testing.T) {
	meta := testMetadata("odd")
	meta.Owner = "nocturn"
	meta.User = "alice"
	shed := metadataToShed(meta)
	if shed.Owner != "" || shed.User != config.UserUnresolved {
		t.Errorf("Shed: owner %q user %q, want none and %q", shed.Owner, shed.User, config.UserUnresolved)
	}
}

// The user survives every metadata write of a stop, a start and a server
// restart: the stop's, the start's running state, and the start-up walk's.
func TestUserSurvivesStopStartAndRestart(t *testing.T) {
	c, cfg := newP9TeardownClient(t)
	const name = "mine"
	meta := createTestInstance(t, cfg.InstanceDir, name)
	meta.User = "alice"
	meta.Status = config.StatusRunning
	meta.PID = mustDeadPID(t)
	if err := meta.Save(cfg.InstanceDir); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	wantUser := func(when string) {
		t.Helper()
		shed, err := c.GetShed(ctx, name)
		if err != nil {
			t.Fatalf("%s: GetShed: %v", when, err)
		}
		if shed.User != "alice" {
			t.Fatalf("%s: user = %q, want alice", when, shed.User)
		}
		onDisk, err := LoadMetadata(cfg.InstanceDir, name)
		if err != nil {
			t.Fatal(err)
		}
		if onDisk.User != "alice" {
			t.Fatalf("%s: stored user = %q, want alice", when, onDisk.User)
		}
	}

	// Stop: the VMM is gone; the stop saves the metadata stopped.
	if _, err := c.StopShed(ctx, name); err != nil {
		t.Fatalf("StopShed: %v", err)
	}
	wantUser("after stop")

	// Start: the start hooks that write metadata, with a VM handle standing in
	// for a booted VMM.
	st := &fcStarter{c: c}
	h, err := st.LoadMetadata(ctx, orchestrator.StartRequest{Name: name})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.CheckNotRunning(ctx, h); err != nil {
		t.Fatal(err)
	}
	vm := fcVMHandle{vm: &VM{meta: &Metadata{Name: name, PID: mustDeadPID(t)}, cfg: cfg}}
	cleanup := backend.NewCleanup()
	if err := st.PersistRunningState(ctx, h, vm, cleanup); err != nil {
		t.Fatalf("PersistRunningState: %v", err)
	}
	cleanup.Commit()
	if onDisk, _ := LoadMetadata(cfg.InstanceDir, name); onDisk.Status != config.StatusRunning {
		t.Fatalf("test set-up: the start did not save running (status %q)", onDisk.Status)
	}
	wantUser("after start")
	if err := st.RestoreStoppedMetadata(h); err != nil {
		t.Fatalf("RestoreStoppedMetadata: %v", err)
	}
	wantUser("after a start rolled back")

	// Restart: a new server process over the same state, whose start-up
	// walk finds the shed recorded running with its VMM gone; the first read
	// after it records the shed stopped.
	if err := st.PersistRunningState(ctx, h, vm, backend.NewCleanup()); err != nil {
		t.Fatal(err)
	}
	c, _ = newResumeTestClient(t, cfg, nil)
	c.ResumeRunningInstances(ctx)
	wantUser("after restart")
	if onDisk, _ := LoadMetadata(cfg.InstanceDir, name); onDisk.Status != config.StatusStopped {
		t.Fatalf("test set-up: the read after the restart did not record the shed stopped (status %q)", onDisk.Status)
	}
}

// The create step that writes a new shed's metadata stores the request's
// user, and the shed it reports carries it.
func TestCreateMetadataRecordsTheUser(t *testing.T) {
	c, cfg := newP9TeardownClient(t)
	cr := &fcCreator{c: c}
	req := config.CreateShedRequest{Name: "mine", User: "alice"}
	h, err := cr.BuildAndPersistMetadata(context.Background(), req,
		&fcPreFlight{cpus: 2, memoryMB: 512}, fcUpper{path: filepath.Join(t.TempDir(), "upper.ext4")},
		fcNet{cid: 101, ipAddress: "172.30.0.3", tapDevice: "fc-tap-1", profile: "open"}, backend.NewCleanup())
	if err != nil {
		t.Fatalf("BuildAndPersistMetadata: %v", err)
	}
	if got := cr.ToShedResult(h).User; got != "alice" {
		t.Errorf("created shed reports user %q, want alice", got)
	}
	onDisk, err := LoadMetadata(cfg.InstanceDir, "mine")
	if err != nil {
		t.Fatal(err)
	}
	if onDisk.User != "alice" {
		t.Errorf("stored user = %q, want alice", onDisk.User)
	}
}

// A create that names both a service client and a user, or a user name that
// is not valid, is refused before anything is written.
func TestCreateShedRefusesBadOwnership(t *testing.T) {
	c, cfg := newP9TeardownClient(t)
	for _, req := range []config.CreateShedRequest{
		{Name: "both", Owner: "nocturn", User: "alice"},
		{Name: "bad-user", User: "Alice"},
		{Name: "marker", User: config.UserUnresolved},
	} {
		if _, err := c.CreateShed(context.Background(), req); !errors.Is(err, config.ErrInvalidShedRequestSentinel) {
			t.Errorf("CreateShed(%s, owner %q, user %q) = %v, want an invalid request", req.Name, req.Owner, req.User, err)
		}
		if _, err := os.Stat(InstanceDir(cfg.InstanceDir, req.Name)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("CreateShed(%s) left an instance directory (%v)", req.Name, err)
		}
	}
}

// snapshotSource writes a stopped shed with an upper file and a cached
// lower image, ready to be snapshotted and spawned from.
func snapshotSource(t *testing.T, c *Client, cfg *config.FirecrackerConfig, name string, edit func(*Metadata)) *Metadata {
	t.Helper()
	meta := createTestInstance(t, cfg.InstanceDir, name)
	meta.UpperPath = filepath.Join(t.TempDir(), "upper.ext4")
	if err := os.WriteFile(meta.UpperPath, []byte("upper"), 0o600); err != nil {
		t.Fatal(err)
	}
	meta.LowerDigest = installTestBlob(t, cfg.ImagesDir, "", []byte("lower of "+name))
	if edit != nil {
		edit(meta)
	}
	if err := meta.Save(cfg.InstanceDir); err != nil {
		t.Fatal(err)
	}
	return meta
}

func newSnapshotUserClient(t *testing.T) (*Client, *config.FirecrackerConfig) {
	t.Helper()
	c, cfg := newP9TeardownClient(t)
	cfg.SnapshotsDir = filepath.Join(t.TempDir(), "snapshots")
	cfg.ImagesDir = filepath.Join(t.TempDir(), "images")
	return c, cfg
}

// A snapshot records the user its SOURCE shed is reported with, through
// CreateSnapshot, in snapshot.json, without a schema change. The request
// cannot name one (config.SnapshotCreateRequest has no user), so a caller
// cannot stamp a snapshot of another user's shed as theirs. A service
// client's shed gives a snapshot with no user. A snapshot.json without the
// field reads as no user.
func TestSnapshotRecordsItsSourcesUser(t *testing.T) {
	c, cfg := newSnapshotUserClient(t)
	ctx := context.Background()
	snapshotSource(t, c, cfg, "alices", func(m *Metadata) { m.User = "alice" })
	snapshotSource(t, c, cfg, "theirs", func(m *Metadata) { m.Owner = "nocturn" })
	snapshotSource(t, c, cfg, "implicit", nil)

	for _, tt := range []struct{ src, want string }{
		{"alices", "alice"},
		{"theirs", ""},
		{"implicit", ""},
	} {
		snap, err := c.CreateSnapshot(ctx, config.SnapshotCreateRequest{Name: "of-" + tt.src, SourceShed: tt.src})
		if err != nil {
			t.Fatalf("CreateSnapshot of %s: %v", tt.src, err)
		}
		if snap.User != tt.want || snap.Version != config.SnapshotSchemaVersion || config.SnapshotSchemaVersion != 2 {
			t.Errorf("snapshot of %s: user %q version %d, want %q at schema 2", tt.src, snap.User, snap.Version, tt.want)
		}
		got, err := c.GetSnapshot(ctx, "of-"+tt.src)
		if err != nil {
			t.Fatal(err)
		}
		if got.User != tt.want {
			t.Errorf("reloaded snapshot of %s: user %q, want %q", tt.src, got.User, tt.want)
		}
	}

	legacyDir := SnapshotDir(cfg.SnapshotsDir, "legacy")
	if err := os.MkdirAll(legacyDir, 0o755); err != nil {
		t.Fatal(err)
	}
	legacy := `{"version": 2, "name": "legacy", "backend": "firecracker", "source_shed": "src"}`
	if err := os.WriteFile(SnapshotMetadataPath(cfg.SnapshotsDir, "legacy"), []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	old, err := loadSnapshot(cfg.SnapshotsDir, "legacy")
	if err != nil {
		t.Fatal(err)
	}
	if old.User != "" {
		t.Errorf("legacy snapshot user = %q, want none", old.User)
	}
}

// A source whose record names both a service client and a user is reported
// unresolved. It is refused, not snapshotted: a snapshot of it would either
// launder its ownership (to the implicit user, or to one of the two names) or
// store config.UserUnresolved. Nothing is left behind.
func TestSnapshotOfAnUnresolvedSourceIsRefused(t *testing.T) {
	c, cfg := newSnapshotUserClient(t)
	snapshotSource(t, c, cfg, "odd", func(m *Metadata) { m.Owner, m.User = "nocturn", "alice" })
	_, err := c.CreateSnapshot(context.Background(), config.SnapshotCreateRequest{Name: "of-odd", SourceShed: "odd"})
	if !errors.Is(err, config.ErrInvalidShedRequestSentinel) {
		t.Fatalf("CreateSnapshot of an unresolved source = %v, want an invalid request", err)
	}
	if _, err := os.Stat(SnapshotDir(cfg.SnapshotsDir, "of-odd")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the refused snapshot left a directory (%v)", err)
	}
}

// A shed created from a snapshot takes the user of the create request, not
// the snapshot's: the spawn's preflight reads the snapshot for its image and
// upper only.
func TestCreateFromSnapshotTakesTheRequestsUser(t *testing.T) {
	c, cfg := newSnapshotUserClient(t)
	ctx := context.Background()
	snapshotSource(t, c, cfg, "alices", func(m *Metadata) { m.User = "alice" })
	if _, err := c.CreateSnapshot(ctx, config.SnapshotCreateRequest{Name: "snap", SourceShed: "alices"}); err != nil {
		t.Fatal(err)
	}

	cr := &fcCreator{c: c}
	req := config.CreateShedRequest{Name: "bobs", FromSnapshot: "snap", User: "bob"}
	cleanup := backend.NewCleanup()
	pre, err := cr.PreFlight(ctx, req, cleanup)
	if err != nil {
		t.Fatalf("PreFlight from the snapshot: %v", err)
	}
	if !pre.(*fcPreFlight).IsFromSnapshot() {
		t.Fatal("test set-up: the preflight did not read the snapshot")
	}
	h, err := cr.BuildAndPersistMetadata(ctx, req, pre, fcUpper{path: filepath.Join(t.TempDir(), "upper.ext4")},
		fcNet{cid: 102, ipAddress: "172.30.0.4", tapDevice: "fc-tap-2", profile: "open"}, cleanup)
	if err != nil {
		t.Fatalf("BuildAndPersistMetadata: %v", err)
	}
	cleanup.Commit()
	if got := cr.ToShedResult(h).User; got != "bob" {
		t.Errorf("shed created from alice's snapshot for bob reports user %q, want bob", got)
	}
	if onDisk, err := LoadMetadata(cfg.InstanceDir, "bobs"); err != nil || onDisk.User != "bob" || onDisk.FromSnapshot != "snap" {
		t.Errorf("stored: %+v, %v; want user bob from snapshot snap", onDisk, err)
	}
}

// A reset rewrites the shed's metadata with a new upper and keeps its user.
func TestResetKeepsTheUser(t *testing.T) {
	c, cfg := newP9TeardownClient(t)
	cfg.UpperSizeDefault = "5G"
	meta := createTestInstance(t, cfg.InstanceDir, "mine")
	meta.User = "alice"
	meta.UpperSizeBytes = 1 << 30
	upper, err := EnsureUpper(cfg.UppersDir, "mine", meta.UpperSizeBytes)
	if err != nil {
		t.Fatal(err)
	}
	meta.UpperPath, meta.RootfsPath = upper, upper
	if err := meta.Save(cfg.InstanceDir); err != nil {
		t.Fatal(err)
	}
	shed, err := c.ResetShed(context.Background(), "mine")
	if err != nil {
		t.Fatalf("ResetShed: %v", err)
	}
	if shed.User != "alice" {
		t.Errorf("reset shed reports user %q, want alice", shed.User)
	}
	if onDisk, err := LoadMetadata(cfg.InstanceDir, "mine"); err != nil || onDisk.User != "alice" {
		t.Errorf("stored after reset: %+v, %v; want user alice", onDisk, err)
	}
}

// A record that names both a service client and a user is reported with no
// owner, so a request that requires the owner it names (a service client's)
// is refused under the per-shed lock by every operation, not only at the
// API's front door, and the shed is left as it was.
func TestRequiredOwnerOnARecordNamingBoth(t *testing.T) {
	c, cfg := newP9TeardownClient(t)
	meta := createTestInstance(t, cfg.InstanceDir, "odd")
	meta.Owner, meta.User = "nocturn", "alice"
	if err := meta.Save(cfg.InstanceDir); err != nil {
		t.Fatal(err)
	}
	ctx := backend.WithRequiredOwner(context.Background(), "nocturn")
	notFound := func(op string, err error) {
		t.Helper()
		if !errors.Is(err, config.ErrShedNotFoundSentinel) {
			t.Errorf("%s with the record's service client required = %v, want not found", op, err)
		}
	}
	_, err := c.GetShed(ctx, "odd")
	notFound("GetShed", err)
	_, err = c.StartShed(ctx, "odd")
	notFound("StartShed", err)
	_, err = c.StopShed(ctx, "odd")
	notFound("StopShed", err)
	notFound("DeleteShed", c.DeleteShed(ctx, "odd"))
	notFound("Exec", NewBackend(c).Exec(ctx, "odd", backend.ExecOptions{Cmd: []string{"true"}}))

	got, err := LoadMetadata(cfg.InstanceDir, "odd")
	if err != nil {
		t.Fatalf("the shed was removed: %v", err)
	}
	if got.Status != config.StatusStopped || got.Owner != "nocturn" || got.User != "alice" {
		t.Errorf("the shed changed: status %q owner %q user %q", got.Status, got.Owner, got.User)
	}
	// The record is reachable without the requirement (an admin).
	if _, err := c.GetShed(context.Background(), "odd"); err != nil {
		t.Errorf("GetShed with no owner required = %v", err)
	}
}

// Reporting a record that names both with no owner does not turn the
// credential bus on for it: the bus still reads the stored owner. A running
// shed resumed after a restart is registered owned (off the bus); a user's
// shed with no owner is on it.
func TestRecordNamingBothKeepsTheBusOff(t *testing.T) {
	for _, tt := range []struct {
		name, owner, user string
		onBus             bool
	}{
		{"both", "nocturn", "alice", false},
		{"users", "", "alice", true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			tmpDir := t.TempDir()
			cfg := testFirecrackerConfig(tmpDir)
			writeResumeInstance(t, tmpDir, tt.name, config.StatusRunning, 4242)
			meta, err := LoadMetadata(tmpDir, tt.name)
			if err != nil {
				t.Fatal(err)
			}
			meta.Owner, meta.User = tt.owner, tt.user
			if err := meta.Save(tmpDir); err != nil {
				t.Fatal(err)
			}
			inspect := func(context.Context, int) (string, error) { return fcCmdline(cfg, tt.name), nil }
			c, bridge := newResumeTestClient(t, cfg, inspect)
			c.ResumeRunningInstances(context.Background())
			got := resumedNames(bridge)
			if onBus := len(got) == 1 && got[0] == tt.name; onBus != tt.onBus {
				t.Errorf("on the credential bus = %v (bridge %v), want %v", onBus, got, tt.onBus)
			}
		})
	}
}

// GetShed reads a shed with no lock, finds its recorded VMM gone, and saves
// it stopped. Between the read and the save the shed is deleted and another
// shed is created under the name for another user (forced here by the
// beforeStaleSave hook). The save must not land on the replacement: its
// user, creation time, status and pid stay as created.
func TestGetShedStaleSaveSparesAReplacementShed(t *testing.T) {
	c, cfg := newP9TeardownClient(t)
	ctx := context.Background()
	const name = "mine"
	deadPID := mustDeadPID(t)
	old := createTestInstance(t, cfg.InstanceDir, name)
	old.User = "alice"
	old.Status = config.StatusRunning
	old.PID = deadPID
	old.CreatedAt = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	if err := old.Save(cfg.InstanceDir); err != nil {
		t.Fatal(err)
	}

	var replacement *Metadata
	hooked := 0
	c.beforeStaleSave = func(n string) {
		hooked++
		if hooked > 1 {
			return
		}
		// Another request deletes the shed and creates bob's under the
		// name, each under the per-shed lock as the real calls take it.
		if err := c.DeleteShed(ctx, n); err != nil {
			t.Fatalf("DeleteShed in between: %v", err)
		}
		unlock := c.acquireCreateLock(n)
		h, err := (&fcCreator{c: c}).BuildAndPersistMetadata(ctx, config.CreateShedRequest{Name: n, User: "bob"},
			&fcPreFlight{cpus: 2, memoryMB: 512}, fcUpper{path: filepath.Join(t.TempDir(), "upper.ext4")},
			fcNet{cid: 101, ipAddress: "172.30.0.3", tapDevice: "fc-tap-1", profile: "open"}, backend.NewCleanup())
		if err != nil {
			unlock()
			t.Fatalf("recreate in between: %v", err)
		}
		// The replacement runs, and (the worst case) under the very pid
		// the stale read saw, so only its creation time tells it apart.
		replacement = h.(*fcMetaHandle).meta
		replacement.Status = config.StatusRunning
		replacement.PID = deadPID
		if err := replacement.Save(cfg.InstanceDir); err != nil {
			unlock()
			t.Fatal(err)
		}
		unlock()
	}

	if _, err := c.GetShed(ctx, name); err != nil {
		t.Fatalf("GetShed: %v", err)
	}
	if hooked != 1 || replacement == nil {
		t.Fatalf("test set-up: the hook ran %d times, replacement %v", hooked, replacement != nil)
	}
	onDisk, err := LoadMetadata(cfg.InstanceDir, name)
	if err != nil {
		t.Fatal(err)
	}
	if onDisk.User != "bob" {
		t.Errorf("the replacement's user = %q, want bob", onDisk.User)
	}
	if !onDisk.CreatedAt.Equal(replacement.CreatedAt) {
		t.Errorf("the replacement's creation time = %v, want %v", onDisk.CreatedAt, replacement.CreatedAt)
	}
	if onDisk.Status != config.StatusRunning || onDisk.PID != deadPID {
		t.Errorf("the replacement was saved over: status %q pid %d, want running pid %d", onDisk.Status, onDisk.PID, deadPID)
	}
}

// The same incarnation, read and saved with nothing in between, is recorded
// stopped. While another operation holds the shed's lock the read neither
// waits for it nor saves: it reports the shed stopped and leaves the record
// to the lock holder, and the next read saves it.
func TestGetShedStaleSaveUnderTheLock(t *testing.T) {
	c, cfg := newP9TeardownClient(t)
	const name = "mine"
	meta := createTestInstance(t, cfg.InstanceDir, name)
	meta.User = "alice"
	meta.Status = config.StatusRunning
	meta.PID = mustDeadPID(t)
	if err := meta.Save(cfg.InstanceDir); err != nil {
		t.Fatal(err)
	}

	unlock := c.acquireCreateLock(name)
	type result struct {
		shed *config.Shed
		err  error
	}
	done := make(chan result, 1)
	go func() {
		shed, err := c.GetShed(context.Background(), name)
		done <- result{shed, err}
	}()
	var r result
	select {
	case r = <-done:
	case <-time.After(10 * time.Second):
		unlock()
		t.Fatal("GetShed waited for the shed's lock")
	}
	if r.err != nil || r.shed.Status != config.StatusStopped || r.shed.PID != 0 {
		t.Fatalf("GetShed with the lock held = %+v, %v; want stopped with no pid", r.shed, r.err)
	}
	if onDisk, _ := LoadMetadata(cfg.InstanceDir, name); onDisk.Status != config.StatusRunning {
		t.Fatalf("GetShed saved while another operation held the lock (status %q)", onDisk.Status)
	}
	unlock()

	if _, err := c.GetShed(context.Background(), name); err != nil {
		t.Fatal(err)
	}
	onDisk, err := LoadMetadata(cfg.InstanceDir, name)
	if err != nil {
		t.Fatal(err)
	}
	if onDisk.Status != config.StatusStopped || onDisk.PID != 0 || onDisk.User != "alice" || !onDisk.CreatedAt.Equal(meta.CreatedAt) {
		t.Errorf("after an unhindered read: status %q pid %d user %q created %v; want stopped, no pid, alice, %v",
			onDisk.Status, onDisk.PID, onDisk.User, onDisk.CreatedAt, meta.CreatedAt)
	}
}

// GetShed reads a shed whose recorded VMM is gone. Between the read and the
// save the same shed is stopped and started again, and the OS hands its new
// VMM the very pid the read saw dead (forced by the beforeStaleSave hook and
// the /proc seam). The creation time, the status and the pid all match what
// was read, so only the liveness check under the lock tells the restart
// apart: the save must not strip the live VMM's pid.
func TestGetShedStaleSaveSparesARestartOnTheSamePID(t *testing.T) {
	c, cfg := newP9TeardownClient(t)
	ctx := context.Background()
	const name = "mine"
	pid := mustSpawnLiveChild(t)
	meta := createTestInstance(t, cfg.InstanceDir, name)
	meta.User = "alice"
	meta.Status = config.StatusRunning
	meta.PID = pid
	if err := meta.Save(cfg.InstanceDir); err != nil {
		t.Fatal(err)
	}
	// The read sees the recorded VMM gone.
	swapPIDCmdlineReader(t, func(int) ([]byte, error) { return nil, fs.ErrNotExist })

	hooked := 0
	c.beforeStaleSave = func(n string) {
		hooked++
		if hooked > 1 {
			return
		}
		// A stop and a start of the same shed, each under the shed's lock
		// as the real calls take it; the new VMM gets the same pid.
		unlock := c.acquireCreateLock(n)
		defer unlock()
		cur, err := LoadMetadata(cfg.InstanceDir, n)
		if err != nil {
			t.Fatal(err)
		}
		cur.Status, cur.PID = config.StatusStopped, 0
		if err := cur.Save(cfg.InstanceDir); err != nil {
			t.Fatal(err)
		}
		readPIDCmdline = readerOwns(cfg, n, pid)
		cur.Status, cur.PID = config.StatusRunning, pid
		if err := cur.Save(cfg.InstanceDir); err != nil {
			t.Fatal(err)
		}
	}

	if _, err := c.GetShed(ctx, name); err != nil {
		t.Fatalf("GetShed: %v", err)
	}
	if hooked != 1 {
		t.Fatalf("test set-up: the hook ran %d times", hooked)
	}
	onDisk, err := LoadMetadata(cfg.InstanceDir, name)
	if err != nil {
		t.Fatal(err)
	}
	if onDisk.Status != config.StatusRunning || onDisk.PID != pid {
		t.Errorf("the restarted shed was saved over: status %q pid %d, want running pid %d", onDisk.Status, onDisk.PID, pid)
	}
	if onDisk.User != "alice" || !onDisk.CreatedAt.Equal(meta.CreatedAt) {
		t.Errorf("the restarted shed: user %q created %v, want alice and %v", onDisk.User, onDisk.CreatedAt, meta.CreatedAt)
	}
}

// newTestEgressManager is an egress manager over an in-process proxy, so the
// egress set and clear paths run with no proxy binary.
func newTestEgressManager(t *testing.T) *egress.Manager {
	t.Helper()
	dir, err := os.MkdirTemp("", "egress")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "control.sock")
	ln, err := egress.ListenControl(sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	proxy := egress.NewProxyServer()
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() { _ = proxy.Serve(conn) }()
		}
	}()
	// The manager supervises a child; `true` stands in for the proxy binary
	// and exits at once, while the control socket is served above.
	mgr, err := egress.StartManager(sock, "true", 41000, 41009, nil)
	if err != nil {
		t.Fatalf("StartManager: %v", err)
	}
	t.Cleanup(func() { _ = mgr.Close() })
	return mgr
}

// Egress set and clear hold the shed's lock and end with a read of the shed.
// When that read finds the recorded VMM gone it records the shed stopped
// under the lock the operation holds, so a snapshot made straight after is
// not refused as a snapshot of a running shed.
func TestEgressOnADeadShedRecordsItStopped(t *testing.T) {
	for _, op := range []string{"clear", "set off"} {
		t.Run(op, func(t *testing.T) {
			c, cfg := newSnapshotUserClient(t)
			if c.serverCfg == nil {
				c.serverCfg = &config.ServerConfig{}
			}
			c.SetEgressManager(newTestEgressManager(t))
			ctx := context.Background()
			const name = "mine"
			snapshotSource(t, c, cfg, name, func(m *Metadata) {
				m.User = "alice"
				m.Status = config.StatusRunning
				m.PID = mustDeadPID(t)
				m.EgressProfiles = []string{"open"}
			})

			var shed *config.Shed
			var err error
			if op == "clear" {
				shed, err = c.ClearShedEgress(ctx, name)
			} else {
				shed, err = c.SetShedEgress(ctx, name, []string{"off"})
			}
			if err != nil {
				t.Fatalf("egress %s: %v", op, err)
			}
			if shed.Status != config.StatusStopped {
				t.Errorf("egress %s reports status %q, want stopped", op, shed.Status)
			}
			if onDisk, err := LoadMetadata(cfg.InstanceDir, name); err != nil || onDisk.Status != config.StatusStopped || onDisk.PID != 0 {
				t.Errorf("after egress %s: %+v, %v; want recorded stopped with no pid", op, onDisk, err)
			}
			snap, err := c.CreateSnapshot(ctx, config.SnapshotCreateRequest{Name: "snap", SourceShed: name})
			if err != nil {
				t.Fatalf("CreateSnapshot straight after egress %s: %v", op, err)
			}
			if snap.User != "alice" {
				t.Errorf("snapshot user %q, want alice", snap.User)
			}
		})
	}
}
