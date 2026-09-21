//go:build linux
// +build linux

package firecracker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/charliek/shed/internal/config"
	"github.com/charliek/shed/internal/plugin"
	"github.com/charliek/shed/internal/vmutil"
)

// TestAcquireSnapshotLock mirrors TestAcquireCreateLock for the snapshot-name
// keyspace. Same-name acquires must serialize CreateSnapshot vs DeleteSnapshot
// vs CreateShed-from-snapshot; different-name acquires must run in parallel.
func TestAcquireSnapshotLock(t *testing.T) {
	tests := []struct {
		name        string
		firstName   string
		secondName  string
		shouldBlock bool
	}{
		{"same name serializes", "snap", "snap", true},
		{"different names do not block", "a", "b", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := &Client{}
			release1 := c.acquireSnapshotLock(tt.firstName)

			acquired := make(chan struct{})
			go func() {
				release2 := c.acquireSnapshotLock(tt.secondName)
				close(acquired)
				release2()
			}()

			if tt.shouldBlock {
				select {
				case <-acquired:
					release1()
					t.Fatal("second acquireSnapshotLock should have blocked")
				case <-time.After(100 * time.Millisecond):
				}
				release1()
				select {
				case <-acquired:
				case <-time.After(time.Second):
					t.Fatal("second acquireSnapshotLock did not proceed after release")
				}
			} else {
				defer release1()
				select {
				case <-acquired:
				case <-time.After(500 * time.Millisecond):
					t.Fatal("acquireSnapshotLock for different names must not block")
				}
			}
		})
	}
}

// TestSnapshotAndCreateLockOrderNoDeadlock asserts the documented lock-order
// rule (snapshotLock -> createLock) holds under real contention. Two
// goroutines acquire the SAME snapshot and shed names so the locks actually
// contend; if either took them in the wrong order this would AB-BA deadlock.
func TestSnapshotAndCreateLockOrderNoDeadlock(t *testing.T) {
	c := &Client{}
	const snapName = "shared-snap"
	const shedName = "shared-shed"
	start := make(chan struct{})

	doneA := make(chan struct{})
	go func() {
		<-start
		releaseSnap := c.acquireSnapshotLock(snapName)
		time.Sleep(10 * time.Millisecond)
		releaseCreate := c.acquireCreateLock(shedName)
		releaseCreate()
		releaseSnap()
		close(doneA)
	}()

	doneB := make(chan struct{})
	go func() {
		<-start
		releaseSnap := c.acquireSnapshotLock(snapName)
		releaseCreate := c.acquireCreateLock(shedName)
		releaseCreate()
		releaseSnap()
		close(doneB)
	}()
	close(start)

	select {
	case <-doneA:
	case <-time.After(2 * time.Second):
		t.Fatal("goroutine A deadlocked")
	}
	select {
	case <-doneB:
	case <-time.After(2 * time.Second):
		t.Fatal("goroutine B deadlocked")
	}
}

// TestStopShedLockedDoesNotReacquireLock guards against a regression where
// DeleteShed (which holds the lifecycle lock) calls into the stop path and
// the stop path re-takes the same non-reentrant mutex — a deadlock that
// CodeRabbit flagged on PR #81.
func TestStopShedLockedDoesNotReacquireLock(t *testing.T) {
	c := &Client{}
	defer c.acquireCreateLock("test-shed")()

	done := make(chan struct{})
	go func() {
		_, _ = c.stopShedLocked(context.Background(), &Metadata{
			Name:   "test-shed",
			Status: config.StatusStopped,
		}, stopGraceful)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("stopShedLocked deadlocked while caller held createLock")
	}
}

// TestAcquireCreateLock covers the lock that closes the CreateShed /
// CopyRootfs TOCTOU race described in rootfs.go: same-name acquires must
// serialize; different-name acquires must run in parallel.
//
// As of the snapshot feature, this is also the per-shed-name lifecycle lock
// taken by Start/Stop/Delete and by CreateSnapshot of this shed as source.
func TestAcquireCreateLock(t *testing.T) {
	tests := []struct {
		name        string
		firstName   string
		secondName  string
		shouldBlock bool
	}{
		{"same name serializes", "same", "same", true},
		{"different names do not block", "a", "b", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := &Client{}
			release1 := c.acquireCreateLock(tt.firstName)

			acquired := make(chan struct{})
			go func() {
				release2 := c.acquireCreateLock(tt.secondName)
				close(acquired)
				release2()
			}()

			if tt.shouldBlock {
				select {
				case <-acquired:
					release1()
					t.Fatal("second acquireCreateLock should have blocked")
				case <-time.After(100 * time.Millisecond):
					// expected: still blocked on the first holder
				}
				release1()
				select {
				case <-acquired:
					// expected: unblocked after release
				case <-time.After(time.Second):
					t.Fatal("second acquireCreateLock did not proceed after release")
				}
			} else {
				defer release1()
				select {
				case <-acquired:
					// expected: different names run in parallel
				case <-time.After(500 * time.Millisecond):
					t.Fatal("acquireCreateLock for different names must not block")
				}
			}
		})
	}
}

func TestAllocateCID(t *testing.T) {
	dir := mustTempDir(t, "client-test")
	cfg := testFirecrackerConfig(dir)

	client := &Client{
		cfg:      cfg,
		vms:      make(map[string]*VM),
		usedCIDs: make(map[uint32]string),
		usedIPs:  make(map[string]string),
	}

	tests := []struct {
		name     string
		usedCIDs map[uint32]string
		want     uint32
	}{
		{
			name:     "no used CIDs",
			usedCIDs: map[uint32]string{},
			want:     100, // VsockBaseCID
		},
		{
			name:     "first CID used",
			usedCIDs: map[uint32]string{100: "vm-1"},
			want:     101,
		},
		{
			name:     "gap in CIDs",
			usedCIDs: map[uint32]string{100: "vm-1", 101: "vm-2", 103: "vm-3"},
			want:     102,
		},
		{
			name:     "sequential CIDs",
			usedCIDs: map[uint32]string{100: "vm-1", 101: "vm-2", 102: "vm-3"},
			want:     103,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client.usedCIDs = tt.usedCIDs
			got, err := client.AllocateCID("test")
			if err != nil {
				t.Fatalf("AllocateCID() error = %v", err)
			}
			if got != tt.want {
				t.Errorf("AllocateCID() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestAllocateCID_Exhaustion(t *testing.T) {
	dir := mustTempDir(t, "client-test")
	cfg := testFirecrackerConfig(dir)
	cfg.VsockBaseCID = MaxVsockCID - 1 // Start near the end

	client := &Client{
		cfg:      cfg,
		vms:      make(map[string]*VM),
		usedCIDs: make(map[uint32]string),
		usedIPs:  make(map[string]string),
	}

	// Allocate the second-to-last CID
	cid1, err := client.AllocateCID("first")
	if err != nil {
		t.Fatalf("First AllocateCID() error = %v", err)
	}
	if cid1 != MaxVsockCID-1 {
		t.Errorf("First CID = %v, want %v", cid1, MaxVsockCID-1)
	}

	// Allocate the last CID
	cid2, err := client.AllocateCID("second")
	if err != nil {
		t.Fatalf("Second AllocateCID() error = %v", err)
	}
	if cid2 != MaxVsockCID {
		t.Errorf("Second CID = %v, want %v", cid2, MaxVsockCID)
	}

	// Try to allocate when all CIDs are exhausted
	_, err = client.AllocateCID("third")
	if err == nil {
		t.Error("Expected error when CIDs exhausted, got nil")
	}
}

func TestRegisterUnregisterInstance(t *testing.T) {
	dir := mustTempDir(t, "client-test")
	cfg := testFirecrackerConfig(dir)

	client := &Client{
		cfg:      cfg,
		vms:      make(map[string]*VM),
		usedCIDs: make(map[uint32]string),
		usedIPs:  make(map[string]string),
	}

	// Register an instance
	client.RegisterInstance("test-vm", 100, "172.30.0.2")

	// Verify it's registered
	if client.usedCIDs[100] != "test-vm" {
		t.Error("CID not registered")
	}
	if client.usedIPs["172.30.0.2"] != "test-vm" {
		t.Error("IP not registered")
	}

	// Unregister
	client.UnregisterInstance("test-vm", 100, "172.30.0.2")

	// Verify it's gone
	if _, exists := client.usedCIDs[100]; exists {
		t.Error("CID still registered after unregister")
	}
	if _, exists := client.usedIPs["172.30.0.2"]; exists {
		t.Error("IP still registered after unregister")
	}
}

func TestAllocateNetwork(t *testing.T) {
	dir := mustTempDir(t, "client-test")
	cfg := testFirecrackerConfig(dir)

	netMgr, err := NewNetworkManager(cfg.BridgeName, cfg.BridgeCIDR, cfg.TAPPrefix)
	if err != nil {
		t.Fatalf("NewNetworkManager() error = %v", err)
	}

	client := &Client{
		cfg:      cfg,
		netMgr:   netMgr,
		vms:      make(map[string]*VM),
		usedCIDs: make(map[uint32]string),
		usedIPs:  make(map[string]string),
	}

	// Allocate first network
	tap1, ip1, err := client.AllocateNetwork("vm-1")
	if err != nil {
		t.Fatalf("AllocateNetwork() error = %v", err)
	}

	if tap1 != "fc-tap-0" {
		t.Errorf("first tap = %v, want fc-tap-0", tap1)
	}
	if ip1 != "172.30.0.2" {
		t.Errorf("first ip = %v, want 172.30.0.2", ip1)
	}

	// Verify IP is immediately marked as used (race condition fix)
	if client.usedIPs[ip1] != "vm-1" {
		t.Error("IP not immediately marked as used after AllocateNetwork")
	}

	// Allocate second - should get next IP since first is already marked used
	tap2, ip2, err := client.AllocateNetwork("vm-2")
	if err != nil {
		t.Fatalf("AllocateNetwork() error = %v", err)
	}

	if tap2 != "fc-tap-1" {
		t.Errorf("second tap = %v, want fc-tap-1", tap2)
	}
	if ip2 != "172.30.0.3" {
		t.Errorf("second ip = %v, want 172.30.0.3", ip2)
	}

	// Verify second IP is also immediately marked as used
	if client.usedIPs[ip2] != "vm-2" {
		t.Error("Second IP not immediately marked as used after AllocateNetwork")
	}
}

func TestMetadataToShed(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	meta := &Metadata{
		Version:    1,
		Name:       "test-vm",
		Status:     config.StatusRunning,
		CreatedAt:  now,
		Backend:    config.BackendFirecracker,
		CID:        42,
		PID:        12345,
		IPAddress:  "172.30.0.5",
		TAPDevice:  "shed-tap-3",
		CPUs:       4,
		MemoryMB:   8192,
		RootfsPath: "/var/lib/shed/firecracker/instances/test-vm/rootfs.ext4",
		Repo:       "https://github.com/example/repo",
		ProjectMounts: []config.MountConfig{
			{Source: "/home/user/projects/myproject", Target: "/home/shed/myproject"},
		},
		LandingDir: "/home/shed/myproject",
	}

	shed := metadataToShed(meta)

	if shed.Name != meta.Name {
		t.Errorf("Name = %q, want %q", shed.Name, meta.Name)
	}
	if shed.Status != meta.Status {
		t.Errorf("Status = %q, want %q", shed.Status, meta.Status)
	}
	if !shed.CreatedAt.Equal(meta.CreatedAt) {
		t.Errorf("CreatedAt = %v, want %v", shed.CreatedAt, meta.CreatedAt)
	}
	if shed.Repo != meta.Repo {
		t.Errorf("Repo = %q, want %q", shed.Repo, meta.Repo)
	}
	expectedContainerID := fmt.Sprintf("fc-%s", meta.Name)
	if shed.ContainerID != expectedContainerID {
		t.Errorf("ContainerID = %q, want %q", shed.ContainerID, expectedContainerID)
	}
	if shed.Backend != meta.Backend {
		t.Errorf("Backend = %q, want %q", shed.Backend, meta.Backend)
	}
	if shed.IPAddress != meta.IPAddress {
		t.Errorf("IPAddress = %q, want %q", shed.IPAddress, meta.IPAddress)
	}
	if shed.CPUs != meta.CPUs {
		t.Errorf("CPUs = %d, want %d", shed.CPUs, meta.CPUs)
	}
	if shed.MemoryMB != meta.MemoryMB {
		t.Errorf("MemoryMB = %d, want %d", shed.MemoryMB, meta.MemoryMB)
	}
	if shed.PID != meta.PID {
		t.Errorf("PID = %d, want %d", shed.PID, meta.PID)
	}
	if shed.RootfsPath != meta.RootfsPath {
		t.Errorf("RootfsPath = %q, want %q", shed.RootfsPath, meta.RootfsPath)
	}
	if len(shed.ProjectMounts) != 1 || shed.ProjectMounts[0].Source != meta.ProjectMounts[0].Source {
		t.Errorf("ProjectMounts = %+v, want %+v", shed.ProjectMounts, meta.ProjectMounts)
	}
	if shed.LandingDir != meta.LandingDir {
		t.Errorf("LandingDir = %q, want %q", shed.LandingDir, meta.LandingDir)
	}
}

func TestMetadataToShed_NoProjectMounts(t *testing.T) {
	meta := &Metadata{
		Name:      "no-mounts",
		Status:    config.StatusStopped,
		Backend:   config.BackendFirecracker,
		IPAddress: "172.30.0.2",
		CPUs:      2,
		MemoryMB:  1024,
	}

	shed := metadataToShed(meta)

	if len(shed.ProjectMounts) != 0 {
		t.Errorf("ProjectMounts = %+v, want none", shed.ProjectMounts)
	}
	if shed.LandingDir != "" {
		t.Errorf("LandingDir = %q, want empty string", shed.LandingDir)
	}
}

func TestMetadataBackwardCompat(t *testing.T) {
	// Test loading metadata JSON that doesn't include the project_mounts
	// field. ProjectMounts should be empty after loading.
	dir := mustTempDir(t, "metadata-compat")

	instanceDir := filepath.Join(dir, "old-vm")
	if err := os.MkdirAll(instanceDir, 0755); err != nil {
		t.Fatalf("failed to create instance dir: %v", err)
	}

	// Write metadata JSON without local_dir field (pre-9P format)
	raw := `{
  "version": 3,
  "name": "old-vm",
  "status": "stopped",
  "created_at": "2024-06-15T10:00:00Z",
  "backend": "firecracker",
  "cid": 100,
  "ip_address": "172.30.0.2",
  "tap_device": "shed-tap-0",
  "cpus": 2,
  "memory_mb": 4096,
  "rootfs_path": "/var/lib/shed/firecracker/instances/old-vm/rootfs.ext4",
  "repo": "https://github.com/example/repo"
}`
	metaPath := filepath.Join(instanceDir, "metadata.json")
	if err := os.WriteFile(metaPath, []byte(raw), 0644); err != nil {
		t.Fatalf("failed to write metadata: %v", err)
	}

	loaded, err := LoadMetadata(dir, "old-vm")
	if err != nil {
		t.Fatalf("LoadMetadata() error = %v", err)
	}

	// Verify ProjectMounts is empty (zero value)
	if len(loaded.ProjectMounts) != 0 {
		t.Errorf("ProjectMounts = %+v, want none for backward-compat metadata", loaded.ProjectMounts)
	}

	// Verify other fields loaded correctly
	if loaded.Name != "old-vm" {
		t.Errorf("Name = %q, want %q", loaded.Name, "old-vm")
	}
	if loaded.Repo != "https://github.com/example/repo" {
		t.Errorf("Repo = %q, want %q", loaded.Repo, "https://github.com/example/repo")
	}

	// Verify metadataToShed also works with no project mounts
	shed := metadataToShed(loaded)
	if len(shed.ProjectMounts) != 0 {
		t.Errorf("metadataToShed().ProjectMounts = %+v, want none", shed.ProjectMounts)
	}
}

func TestMetadataLoad_WithProjectMounts(t *testing.T) {
	// Verify metadata with project_mounts / landing_dir fields loads correctly
	dir := mustTempDir(t, "metadata-compat")

	instanceDir := filepath.Join(dir, "new-vm")
	if err := os.MkdirAll(instanceDir, 0755); err != nil {
		t.Fatalf("failed to create instance dir: %v", err)
	}

	raw := `{
  "version": 3,
  "name": "new-vm",
  "status": "running",
  "created_at": "2024-06-15T10:00:00Z",
  "backend": "firecracker",
  "cid": 101,
  "pid": 5678,
  "ip_address": "172.30.0.3",
  "tap_device": "shed-tap-1",
  "cpus": 4,
  "memory_mb": 8192,
  "rootfs_path": "/var/lib/shed/firecracker/instances/new-vm/rootfs.ext4",
  "project_mounts": [{"source": "/home/user/projects/myapp", "target": "/home/shed/myapp"}],
  "landing_dir": "/home/shed/myapp"
}`
	metaPath := filepath.Join(instanceDir, "metadata.json")
	if err := os.WriteFile(metaPath, []byte(raw), 0644); err != nil {
		t.Fatalf("failed to write metadata: %v", err)
	}

	loaded, err := LoadMetadata(dir, "new-vm")
	if err != nil {
		t.Fatalf("LoadMetadata() error = %v", err)
	}

	if len(loaded.ProjectMounts) != 1 || loaded.ProjectMounts[0].Source != "/home/user/projects/myapp" {
		t.Errorf("ProjectMounts = %+v, want one entry for /home/user/projects/myapp", loaded.ProjectMounts)
	}
	if loaded.LandingDir != "/home/shed/myapp" {
		t.Errorf("LandingDir = %q, want %q", loaded.LandingDir, "/home/shed/myapp")
	}
}

func TestMetadataProjectMounts_RoundTrip(t *testing.T) {
	// Save metadata with project mounts and verify it round-trips correctly
	dir := mustTempDir(t, "metadata-roundtrip")

	meta := testMetadata("roundtrip-vm")
	meta.ProjectMounts = []config.MountConfig{
		{Source: "/tmp/test-project", Target: "/home/shed/test-project"},
	}
	meta.LandingDir = "/home/shed/test-project"

	if err := meta.Save(dir); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	loaded, err := LoadMetadata(dir, "roundtrip-vm")
	if err != nil {
		t.Fatalf("LoadMetadata() error = %v", err)
	}

	if len(loaded.ProjectMounts) != 1 || loaded.ProjectMounts[0].Source != meta.ProjectMounts[0].Source {
		t.Errorf("ProjectMounts = %+v, want %+v", loaded.ProjectMounts, meta.ProjectMounts)
	}
	if loaded.LandingDir != meta.LandingDir {
		t.Errorf("LandingDir = %q, want %q", loaded.LandingDir, meta.LandingDir)
	}

	// Verify the keys are in the JSON
	data, err := os.ReadFile(MetadataPath(dir, "roundtrip-vm"))
	if err != nil {
		t.Fatalf("failed to read metadata file: %v", err)
	}

	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("failed to parse raw JSON: %v", err)
	}

	if _, ok := raw["project_mounts"]; !ok {
		t.Fatal("project_mounts key missing from JSON output")
	}
	if _, ok := raw["landing_dir"]; !ok {
		t.Fatal("landing_dir key missing from JSON output")
	}
}

func TestCreateShedFromSnapshotMutualExclusionWrapsSentinel(t *testing.T) {
	c := &Client{}

	tests := []struct {
		name string
		req  config.CreateShedRequest
	}{
		{"with_image", config.CreateShedRequest{Name: "n", FromSnapshot: "snap1", Image: "default"}},
		{"with_repo", config.CreateShedRequest{Name: "n", FromSnapshot: "snap1", Repo: "git@github.com:o/r.git"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := c.CreateShed(context.Background(), tt.req)
			if err == nil {
				t.Fatal("expected error")
			}
			if !errors.Is(err, config.ErrInvalidShedRequestSentinel) {
				t.Fatalf("error %v does not wrap ErrInvalidShedRequestSentinel", err)
			}
		})
	}
}

// --- ResumeRunningInstances (#315) ------------------------------------------

// newResumeTestClient builds a Client over tmpDir with a real plugin bridge
// and an injected cmdline reader. Bridge registration is the observable the
// walk tests assert on: it is exactly what a restarted shed-server used to
// lose for every running shed.
func newResumeTestClient(t *testing.T, cfg *config.FirecrackerConfig, inspect func(context.Context, int) (string, error)) (*Client, *plugin.Bridge) {
	t.Helper()
	bridge := plugin.NewBridge(plugin.NewRegistry())
	c := &Client{
		cfg:         cfg,
		vms:         make(map[string]*VM),
		usedCIDs:    make(map[uint32]string),
		usedIPs:     make(map[string]string),
		p9Servers:   make(map[string][]*P9Server),
		credMgr:     vmutil.NewCredentialManager(nil, bridge, string(config.BackendFirecracker), vmutil.NewHealthTracker()),
		procCmdline: inspect,
	}
	t.Cleanup(func() { _ = c.Close() })
	return c, bridge
}

// writeResumeInstance writes metadata for `name` in the state the walk will
// find it in. Built on the shared createTestInstance helper.
func writeResumeInstance(t *testing.T, dir, name, status string, pid int) {
	t.Helper()
	meta := createTestInstance(t, dir, name)
	meta.Status = status
	meta.PID = pid
	if err := meta.Save(dir); err != nil {
		t.Fatalf("save metadata for %q: %v", name, err)
	}
}

// fcCmdline is what a live Firecracker VMM serving `name` looks like on
// /proc: NUL-separated argv naming the binary and this instance's api-sock.
func fcCmdline(cfg *config.FirecrackerConfig, name string) string {
	return strings.Join([]string{
		"/usr/bin/firecracker",
		"--api-sock",
		filepath.Join(cfg.SocketDir, name+".sock"),
		"--id",
		name,
	}, "\x00")
}

// resumedNames is the sorted list of sheds the walk registered on the
// bridge — the observable these tests assert on rather than spying on the
// walk itself.
func resumedNames(b *plugin.Bridge) []string {
	infos := b.ListSheds()
	names := make([]string, 0, len(infos))
	for _, i := range infos {
		names = append(names, i.Name)
	}
	sort.Strings(names)
	return names
}

// TestResumeRunningInstances drives the whole startup walk — the unit under
// test for #315 is the walk, not any one predicate.
func TestResumeRunningInstances(t *testing.T) {
	const shed = "alpha"

	tests := []struct {
		name string
		// status/pid are the metadata the walk finds on disk.
		status string
		pid    int
		// cmdline is what the injected inspector reports for that pid; when
		// inspectErr is set the inspector fails instead (vanished PID).
		cmdline    func(cfg *config.FirecrackerConfig) string
		inspectErr error
		// socketDir overrides the default test socket dir. Only the
		// family-independence cell needs it, to reproduce the SHIPPED
		// default (/var/run/shed/firecracker), whose own path contains the
		// family token. A plain t.TempDir() socket dir does not, so the cell
		// would pass against the buggy whole-command-line check too.
		socketDir  func(tmpDir string) string
		wantResume bool
	}{
		{
			name:       "running_and_alive_resumes",
			status:     config.StatusRunning,
			pid:        4242,
			cmdline:    func(cfg *config.FirecrackerConfig) string { return fcCmdline(cfg, shed) },
			wantResume: true,
		},
		{
			// NEGATIVE CONTROL: metadata still says running but the VMM is
			// gone, so the inspection fails. Deleting the liveness check in
			// resumeInstance must turn this cell red.
			name:       "control_running_but_dead_is_not_resumed",
			status:     config.StatusRunning,
			pid:        4242,
			inspectErr: os.ErrNotExist,
			wantResume: false,
		},
		{
			// The PID was recycled by a DIFFERENT shed's firecracker. A
			// family-only check ("is it firecracker?") would resume a dead
			// record here.
			name:   "running_with_reused_pid_is_not_resumed",
			status: config.StatusRunning,
			pid:    4242,
			cmdline: func(cfg *config.FirecrackerConfig) string {
				return fcCmdline(cfg, "some-other-shed")
			},
			wantResume: false,
		},
		{
			// The family check must be INDEPENDENT evidence from the
			// instance-path check. The default socket dir is
			// /var/run/shed/firecracker, so a whole-command-line
			// Contains("firecracker") is satisfied by the sock path itself —
			// and this cell (a recycled PID merely touching the socket) would
			// resume a dead record. Matching argv[0] is what stops it.
			name:   "control_non_vmm_pid_naming_the_socket_is_not_resumed",
			status: config.StatusRunning,
			pid:    4242,
			socketDir: func(tmpDir string) string {
				// Mirrors config/server.go:1277's /var/run/shed/firecracker.
				return filepath.Join(tmpDir, "run", "shed", "firecracker")
			},
			cmdline: func(cfg *config.FirecrackerConfig) string {
				return strings.Join([]string{
					"/usr/bin/socat",
					"-",
					"UNIX-CONNECT:" + filepath.Join(cfg.SocketDir, shed+".sock"),
				}, "\x00")
			},
			wantResume: false,
		},
		{
			name:   "stopped_is_not_resumed",
			status: config.StatusStopped,
			pid:    0,
			cmdline: func(cfg *config.FirecrackerConfig) string {
				return fcCmdline(cfg, shed)
			},
			wantResume: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tmpDir := t.TempDir()
			cfg := testFirecrackerConfig(tmpDir)
			if tt.socketDir != nil {
				cfg.SocketDir = tt.socketDir(tmpDir)
			}
			writeResumeInstance(t, tmpDir, shed, tt.status, tt.pid)

			inspect := func(_ context.Context, pid int) (string, error) {
				if tt.inspectErr != nil {
					return "", tt.inspectErr
				}
				if pid != tt.pid {
					t.Errorf("inspector called with pid %d, want %d", pid, tt.pid)
				}
				return tt.cmdline(cfg), nil
			}

			c, bridge := newResumeTestClient(t, cfg, inspect)
			c.ResumeRunningInstances(context.Background())

			got := resumedNames(bridge)
			if tt.wantResume {
				if len(got) != 1 || got[0] != shed {
					t.Fatalf("resumed = %v, want [%s]", got, shed)
				}
			} else if len(got) != 0 {
				t.Fatalf("resumed = %v, want nothing", got)
			}
		})
	}

	// One wedged inspection must not stall the walk: the other instances
	// still get resumed. The per-inspection timeout is shortened so the cell
	// costs milliseconds rather than the production 2 s.
	t.Run("wedged_inspection_does_not_stall_the_walk", func(t *testing.T) {
		restore := resumeInspectTimeout
		resumeInspectTimeout = 50 * time.Millisecond
		t.Cleanup(func() { resumeInspectTimeout = restore })

		tmpDir := t.TempDir()
		cfg := testFirecrackerConfig(tmpDir)

		// PIDs are how the injected inspector tells the instances apart.
		pids := map[string]int{"aaa": 101, "wedged": 102, "zzz": 103}
		for name, pid := range pids {
			writeResumeInstance(t, tmpDir, name, config.StatusRunning, pid)
		}

		inspect := func(ctx context.Context, pid int) (string, error) {
			if pid == pids["wedged"] {
				// Bounded well above resumeInspectTimeout but well below the
				// package test timeout: if the per-inspection deadline ever
				// stops being plumbed through, this cell fails on the elapsed
				// assertion below instead of hanging until `go test` gives up.
				select {
				case <-ctx.Done():
					return "", ctx.Err()
				case <-time.After(5 * time.Second):
					return "", errors.New("wedged inspector was never cancelled")
				}
			}
			for name, p := range pids {
				if p == pid {
					return fcCmdline(cfg, name), nil
				}
			}
			return "", os.ErrNotExist
		}

		c, bridge := newResumeTestClient(t, cfg, inspect)

		start := time.Now()
		c.ResumeRunningInstances(context.Background())
		elapsed := time.Since(start)

		got := resumedNames(bridge)
		want := []string{"aaa", "zzz"}
		if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
			t.Fatalf("resumed = %v, want %v", got, want)
		}
		if elapsed > 2*time.Second {
			t.Fatalf("walk took %v — the wedged inspection was not bounded", elapsed)
		}
	})

	t.Run("cancelled_context_stops_the_walk", func(t *testing.T) {
		tmpDir := t.TempDir()
		cfg := testFirecrackerConfig(tmpDir)
		writeResumeInstance(t, tmpDir, shed, config.StatusRunning, 4242)

		inspect := func(_ context.Context, _ int) (string, error) {
			return fcCmdline(cfg, shed), nil
		}
		c, bridge := newResumeTestClient(t, cfg, inspect)

		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		c.ResumeRunningInstances(ctx)

		if got := resumedNames(bridge); len(got) != 0 {
			t.Fatalf("resumed = %v on a cancelled walk, want nothing", got)
		}
	})
}

// TestReadProcCmdline covers the REAL /proc reader, which the walk tests never
// touch: they inject a fake so the liveness predicate can be driven against
// synthetic processes. That left the production reader — the one that has to
// honour the walk's per-inspection timeout on a VMM wedged in uninterruptible
// sleep — with no coverage at all.
func TestReadProcCmdline(t *testing.T) {
	t.Run("reads_a_live_pid", func(t *testing.T) {
		got, err := readProcCmdline(context.Background(), os.Getpid())
		if err != nil {
			t.Fatalf("readProcCmdline(self) failed: %v", err)
		}
		// /proc/<pid>/cmdline is NUL-separated argv. The predicate matches on
		// substrings, so pin that a substring of argv[0] is findable in the
		// raw bytes exactly as vmmServesInstance would look for it.
		if !strings.Contains(got, "firecracker.test") {
			t.Fatalf("self cmdline %q does not contain the test binary name", got)
		}
		if !strings.Contains(got, "\x00") {
			t.Fatalf("expected NUL-separated argv, got %q", got)
		}
	})

	t.Run("vanished_pid_is_an_error", func(t *testing.T) {
		// PID 0 is never a readable /proc entry, so this stands in for the
		// "running metadata, dead VM" case without racing a real reaped pid.
		if _, err := readProcCmdline(context.Background(), 0); err == nil {
			t.Fatal("expected an error for a pid with no /proc entry")
		}
	})

	t.Run("already_cancelled_context_is_refused", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		// Self is guaranteed readable, so a nil error here would mean the
		// reader ignored the context rather than that the read failed.
		if _, err := readProcCmdline(ctx, os.Getpid()); !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want context.Canceled", err)
		}
	})
}
