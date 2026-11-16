//go:build darwin

package vz

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/charliek/shed/internal/backend"
	"github.com/charliek/shed/internal/backend/orchestrator"
	"github.com/charliek/shed/internal/config"
	"github.com/charliek/shed/internal/egress"
	"github.com/charliek/shed/internal/plugin"
	"github.com/charliek/shed/internal/vmutil"
)

// newUserTestClient is a Client over a fresh instance directory with what the
// metadata paths below touch, and nothing that needs a VM.
func newUserTestClient(t *testing.T, instanceDir string) *Client {
	t.Helper()
	return &Client{
		cfg:     &config.VZConfig{InstanceDir: instanceDir, SnapshotsDir: filepath.Join(instanceDir, "..", "snapshots")},
		vms:     make(map[string]*VM),
		credMgr: vmutil.NewCredentialManager(nil, plugin.NewBridge(plugin.NewRegistry(), false), string(config.BackendVZ), vmutil.NewHealthTracker()),
	}
}

// A metadata file written before users existed loads with no user, and a
// user is saved and read back as written. The metadata version is unchanged.
func TestMetadataUserRoundTrip(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "old"), 0o755); err != nil {
		t.Fatal(err)
	}
	legacy := `{"version": 3, "name": "old", "status": "stopped", "backend": "vz"}`
	if err := os.WriteFile(MetadataPath(dir, "old"), []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	meta, err := LoadMetadata(dir, "old")
	if err != nil {
		t.Fatalf("LoadMetadata(legacy): %v", err)
	}
	if meta.User != "" || metadataToShed(meta, "").User != "" {
		t.Errorf("legacy metadata user = %q, want none", meta.User)
	}

	mine := &Metadata{Name: "mine", Status: config.StatusStopped, Backend: config.BackendVZ, CreatedAt: time.Now(), User: "alice"}
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
	if shed := metadataToShed(got, ""); shed.User != "alice" || shed.Owner != "" {
		t.Errorf("Shed: owner %q user %q, want none and alice", shed.Owner, shed.User)
	}
}

// A metadata file that names both a service client and a user is invalid and
// fails closed: it is reported with no owner and config.UserUnresolved.
func TestMetadataOwnerAndUserFailsClosed(t *testing.T) {
	meta := &Metadata{Name: "odd", Status: config.StatusStopped, Backend: config.BackendVZ, Owner: "nocturn", User: "alice"}
	shed := metadataToShed(meta, "")
	if shed.Owner != "" || shed.User != config.UserUnresolved {
		t.Errorf("Shed: owner %q user %q, want none and %q", shed.Owner, shed.User, config.UserUnresolved)
	}
}

// The create step that writes a new shed's metadata stores the request's
// user, and the user survives the metadata writes of a start, a start rolled
// back, and the first read after a server restart that finds the VM gone.
func TestUserSurvivesCreateStartAndRestart(t *testing.T) {
	dir := t.TempDir()
	c := newUserTestClient(t, dir)
	ctx := context.Background()
	const name = "mine"
	wantUser := func(when string) {
		t.Helper()
		shed, err := c.GetShed(ctx, name)
		if err != nil {
			t.Fatalf("%s: GetShed: %v", when, err)
		}
		if shed.User != "alice" {
			t.Fatalf("%s: user = %q, want alice", when, shed.User)
		}
		onDisk, err := LoadMetadata(dir, name)
		if err != nil {
			t.Fatal(err)
		}
		if onDisk.User != "alice" {
			t.Fatalf("%s: stored user = %q, want alice", when, onDisk.User)
		}
	}

	cr := &vzCreator{c: c}
	h, err := cr.BuildAndPersistMetadata(ctx, config.CreateShedRequest{Name: name, User: "alice"},
		&vzPreFlight{cpus: 2, memoryMB: 512}, vzUpper{path: filepath.Join(t.TempDir(), "upper.img")}, vzNet{}, backend.NewCleanup())
	if err != nil {
		t.Fatalf("BuildAndPersistMetadata: %v", err)
	}
	if got := cr.ToShedResult(h).User; got != "alice" {
		t.Fatalf("created shed reports user %q, want alice", got)
	}
	wantUser("after create")

	st := &vzStarter{c: c}
	sh, err := st.LoadMetadata(ctx, orchestrator.StartRequest{Name: name})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.CheckNotRunning(ctx, sh); err != nil {
		t.Fatal(err)
	}
	vm := vzVMHandle{vm: &VM{meta: &Metadata{Name: name}, cfg: c.cfg}}
	cleanup := backend.NewCleanup()
	if err := st.PersistRunningState(ctx, sh, vm, cleanup); err != nil {
		t.Fatalf("PersistRunningState: %v", err)
	}
	cleanup.Commit()
	if onDisk, _ := LoadMetadata(dir, name); onDisk.Status != config.StatusRunning {
		t.Fatalf("test set-up: the start did not save running (status %q)", onDisk.Status)
	}
	if err := st.RestoreStoppedMetadata(sh); err != nil {
		t.Fatalf("RestoreStoppedMetadata: %v", err)
	}
	wantUser("after a start rolled back")

	// Restart: a new server process; the shed is recorded running with no
	// VM, and the first read records it stopped.
	cleanup = backend.NewCleanup()
	if err := st.PersistRunningState(ctx, sh, vm, cleanup); err != nil {
		t.Fatal(err)
	}
	cleanup.Commit()
	c = newUserTestClient(t, dir)
	wantUser("after restart")
	if onDisk, _ := LoadMetadata(dir, name); onDisk.Status != config.StatusStopped {
		t.Fatalf("test set-up: the read after the restart did not record the shed stopped (status %q)", onDisk.Status)
	}
}

// A create that names both a service client and a user, or a user name that
// is not valid, is refused before anything is touched.
func TestCreateRefusesBadOwnership(t *testing.T) {
	c := &Client{}
	for _, req := range []config.CreateShedRequest{
		{Name: "both", Owner: "nocturn", User: "alice"},
		{Name: "bad-user", User: "Alice"},
		{Name: "marker", User: config.UserUnresolved},
	} {
		if _, err := c.CreateShed(context.Background(), req); !errors.Is(err, config.ErrInvalidShedRequestSentinel) {
			t.Errorf("CreateShed(%s, owner %q, user %q) = %v, want an invalid request", req.Name, req.Owner, req.User, err)
		}
	}
}

// deadVfkitPID returns the pid of a child that has already exited and been
// reaped: a VM that is verifiably gone.
func deadVfkitPID(t *testing.T) int {
	t.Helper()
	cmd := exec.Command("true")
	if err := cmd.Run(); err != nil {
		t.Fatalf("run true: %v", err)
	}
	return cmd.Process.Pid
}

// saveUserTestShed writes a shed's metadata with an upper file.
func saveUserTestShed(t *testing.T, dir, name string, edit func(*Metadata)) *Metadata {
	t.Helper()
	meta := &Metadata{Name: name, Status: config.StatusStopped, Backend: config.BackendVZ, CreatedAt: time.Now(), CPUs: 2, MemoryMB: 512}
	meta.UpperPath = filepath.Join(t.TempDir(), "upper.img")
	meta.RootfsPath = meta.UpperPath
	if err := os.WriteFile(meta.UpperPath, []byte("upper"), 0o600); err != nil {
		t.Fatal(err)
	}
	if edit != nil {
		edit(meta)
	}
	if err := meta.Save(dir); err != nil {
		t.Fatal(err)
	}
	return meta
}

// A snapshot records the user its SOURCE shed is reported with, through
// CreateSnapshot, in snapshot.json, without a schema change. The request
// cannot name one, so a caller cannot stamp a snapshot of another user's shed
// as theirs. A service client's shed gives a snapshot with no user. A
// snapshot.json without the field reads as no user.
func TestSnapshotRecordsItsSourcesUser(t *testing.T) {
	dir := t.TempDir()
	c := newUserTestClient(t, dir)
	ctx := context.Background()
	saveUserTestShed(t, dir, "alices", func(m *Metadata) { m.User = "alice" })
	saveUserTestShed(t, dir, "theirs", func(m *Metadata) { m.Owner = "nocturn" })
	saveUserTestShed(t, dir, "implicit", nil)

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
		got, err := loadSnapshot(c.cfg.SnapshotsDir, "of-"+tt.src)
		if err != nil {
			t.Fatal(err)
		}
		if got.User != tt.want {
			t.Errorf("reloaded snapshot of %s: user %q, want %q", tt.src, got.User, tt.want)
		}
	}

	if err := os.MkdirAll(SnapshotDir(c.cfg.SnapshotsDir, "legacy"), 0o755); err != nil {
		t.Fatal(err)
	}
	legacy := `{"version": 2, "name": "legacy", "backend": "vz", "source_shed": "src"}`
	if err := os.WriteFile(SnapshotMetadataPath(c.cfg.SnapshotsDir, "legacy"), []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	old, err := loadSnapshot(c.cfg.SnapshotsDir, "legacy")
	if err != nil {
		t.Fatal(err)
	}
	if old.User != "" {
		t.Errorf("legacy snapshot user = %q, want none", old.User)
	}
}

// A source whose record names both a service client and a user is reported
// unresolved, and is refused rather than snapshotted; nothing is left behind.
func TestSnapshotOfAnUnresolvedSourceIsRefused(t *testing.T) {
	dir := t.TempDir()
	c := newUserTestClient(t, dir)
	saveUserTestShed(t, dir, "odd", func(m *Metadata) { m.Owner, m.User = "nocturn", "alice" })
	_, err := c.CreateSnapshot(context.Background(), config.SnapshotCreateRequest{Name: "of-odd", SourceShed: "odd"})
	if !errors.Is(err, config.ErrInvalidShedRequestSentinel) {
		t.Fatalf("CreateSnapshot of an unresolved source = %v, want an invalid request", err)
	}
	if _, err := os.Stat(SnapshotDir(c.cfg.SnapshotsDir, "of-odd")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the refused snapshot left a directory (%v)", err)
	}
}

// The real stop path saves the shed stopped and keeps its user.
func TestStopKeepsTheUser(t *testing.T) {
	dir := t.TempDir()
	c := newUserTestClient(t, dir)
	c.cfg.StopTimeout = config.Duration(100 * time.Millisecond)
	c.cfg.SocketDir = filepath.Join(t.TempDir(), "sockets")
	saveUserTestShed(t, dir, "mine", func(m *Metadata) {
		m.User = "alice"
		m.Status = config.StatusRunning
	})
	shed, err := c.StopShed(context.Background(), "mine")
	if err != nil {
		t.Fatalf("StopShed: %v", err)
	}
	if shed.User != "alice" {
		t.Errorf("stopped shed reports user %q, want alice", shed.User)
	}
	onDisk, err := LoadMetadata(dir, "mine")
	if err != nil {
		t.Fatal(err)
	}
	if onDisk.Status != config.StatusStopped || onDisk.User != "alice" {
		t.Errorf("stored after stop: status %q user %q, want stopped and alice", onDisk.Status, onDisk.User)
	}
}

// A record that names both a service client and a user is reported with no
// owner, so a request that requires the owner it names is refused under the
// per-shed lock by every operation, and the shed is left as it was.
func TestRequiredOwnerOnARecordNamingBoth(t *testing.T) {
	dir := t.TempDir()
	c := newUserTestClient(t, dir)
	saveUserTestShed(t, dir, "odd", func(m *Metadata) { m.Owner, m.User = "nocturn", "alice" })
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

	got, err := LoadMetadata(dir, "odd")
	if err != nil {
		t.Fatalf("the shed was removed: %v", err)
	}
	if got.Status != config.StatusStopped || got.Owner != "nocturn" || got.User != "alice" {
		t.Errorf("the shed changed: status %q owner %q user %q", got.Status, got.Owner, got.User)
	}
}

// GetShed reads a shed with no lock, finds its recorded VM gone, and saves it
// stopped. Between the read and the save the shed is deleted and another
// shed is created under the name for another user (forced by the
// beforeStaleSave hook). The save must not land on the replacement.
func TestGetShedStaleSaveSparesAReplacementShed(t *testing.T) {
	dir := t.TempDir()
	c := newUserTestClient(t, dir)
	ctx := context.Background()
	const name = "mine"
	deadPID := deadVfkitPID(t)
	saveUserTestShed(t, dir, name, func(m *Metadata) {
		m.User = "alice"
		m.Status = config.StatusRunning
		m.PID = deadPID
		m.CreatedAt = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	})

	var replacement *Metadata
	hooked := 0
	c.beforeStaleSave = func(n string) {
		hooked++
		if hooked > 1 {
			return
		}
		unlock := c.acquireCreateLock(n)
		defer unlock()
		if err := os.RemoveAll(InstanceDir(dir, n)); err != nil {
			t.Fatal(err)
		}
		replacement = saveUserTestShed(t, dir, n, func(m *Metadata) {
			m.User = "bob"
			m.Status = config.StatusRunning
			m.PID = deadPID // the worst case: only the creation time differs
		})
	}

	if _, err := c.GetShed(ctx, name); err != nil {
		t.Fatalf("GetShed: %v", err)
	}
	if hooked != 1 || replacement == nil {
		t.Fatalf("test set-up: the hook ran %d times", hooked)
	}
	onDisk, err := LoadMetadata(dir, name)
	if err != nil {
		t.Fatal(err)
	}
	if onDisk.User != "bob" || !onDisk.CreatedAt.Equal(replacement.CreatedAt) {
		t.Errorf("the replacement: user %q created %v, want bob and %v", onDisk.User, onDisk.CreatedAt, replacement.CreatedAt)
	}
	if onDisk.Status != config.StatusRunning || onDisk.PID != deadPID {
		t.Errorf("the replacement was saved over: status %q pid %d, want running pid %d", onDisk.Status, onDisk.PID, deadPID)
	}
}

// While another operation holds the shed's lock, GetShed neither waits for it
// nor saves; the next read with the lock free records the shed stopped.
func TestGetShedStaleSaveUnderTheLock(t *testing.T) {
	dir := t.TempDir()
	c := newUserTestClient(t, dir)
	const name = "mine"
	meta := saveUserTestShed(t, dir, name, func(m *Metadata) {
		m.User = "alice"
		m.Status = config.StatusRunning
		m.PID = deadVfkitPID(t)
	})

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
	if onDisk, _ := LoadMetadata(dir, name); onDisk.Status != config.StatusRunning {
		t.Fatalf("GetShed saved while another operation held the lock (status %q)", onDisk.Status)
	}
	unlock()

	if _, err := c.GetShed(context.Background(), name); err != nil {
		t.Fatal(err)
	}
	onDisk, err := LoadMetadata(dir, name)
	if err != nil {
		t.Fatal(err)
	}
	if onDisk.Status != config.StatusStopped || onDisk.PID != 0 || onDisk.User != "alice" || !onDisk.CreatedAt.Equal(meta.CreatedAt) {
		t.Errorf("after an unhindered read: status %q pid %d user %q; want stopped, no pid, alice", onDisk.Status, onDisk.PID, onDisk.User)
	}
}

// GetShed reads a shed whose recorded VM is gone. Between the read and the
// save the same shed is stopped and started again, and the OS hands its new
// vfkit the very pid the read saw dead (forced by the beforeStaleSave hook and
// the runningVfkit seam). The creation time, the status and the pid all match
// what was read, so only the liveness check under the lock tells the restart
// apart: the save must not strip the live VM's pid.
func TestGetShedStaleSaveSparesARestartOnTheSamePID(t *testing.T) {
	dir := t.TempDir()
	c := newUserTestClient(t, dir)
	ctx := context.Background()
	const name = "mine"
	pid := os.Getpid() // live, so it answers signal 0
	meta := saveUserTestShed(t, dir, name, func(m *Metadata) {
		m.User = "alice"
		m.Status = config.StatusRunning
		m.PID = pid
	})
	prev := runningVfkit
	t.Cleanup(func() { runningVfkit = prev })
	runningVfkit = func(int) bool { return false } // the read sees the VM gone

	hooked := 0
	c.beforeStaleSave = func(n string) {
		hooked++
		if hooked > 1 {
			return
		}
		// A stop and a start of the same shed under the shed's lock; the
		// new vfkit gets the same pid.
		unlock := c.acquireCreateLock(n)
		defer unlock()
		cur, err := LoadMetadata(dir, n)
		if err != nil {
			t.Fatal(err)
		}
		cur.Status, cur.PID = config.StatusStopped, 0
		if err := cur.Save(dir); err != nil {
			t.Fatal(err)
		}
		runningVfkit = func(p int) bool { return p == pid }
		cur.Status, cur.PID = config.StatusRunning, pid
		if err := cur.Save(dir); err != nil {
			t.Fatal(err)
		}
	}

	if _, err := c.GetShed(ctx, name); err != nil {
		t.Fatalf("GetShed: %v", err)
	}
	if hooked != 1 {
		t.Fatalf("test set-up: the hook ran %d times", hooked)
	}
	onDisk, err := LoadMetadata(dir, name)
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
	sockDir, err := os.MkdirTemp("", "egress")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(sockDir) })
	sock := filepath.Join(sockDir, "control.sock")
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
// When that read finds the recorded VM gone it records the shed stopped under
// the lock the operation holds, so a snapshot made straight after is not
// refused as a snapshot of a running shed.
func TestEgressOnADeadShedRecordsItStopped(t *testing.T) {
	for _, op := range []string{"clear", "set off"} {
		t.Run(op, func(t *testing.T) {
			dir := t.TempDir()
			c := newUserTestClient(t, dir)
			c.cfg.SocketDir = t.TempDir()
			c.serverCfg = &config.ServerConfig{}
			c.SetEgressManager(newTestEgressManager(t))
			ctx := context.Background()
			const name = "mine"
			saveUserTestShed(t, dir, name, func(m *Metadata) {
				m.User = "alice"
				m.Status = config.StatusRunning
				m.PID = deadVfkitPID(t)
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
			if onDisk, err := LoadMetadata(dir, name); err != nil || onDisk.Status != config.StatusStopped || onDisk.PID != 0 {
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
