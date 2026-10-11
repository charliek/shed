//go:build linux
// +build linux

package firecracker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/charliek/shed/internal/backend"
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

// --- UNKNOWN pid identity must never be read as "stopped" (#372 follow-up) ---
//
// The predicate is tri-state (see isThisVMsProcess): a live pid whose /proc
// cmdline can't be read is UNKNOWN, not "not ours". These cover the three
// callers that would otherwise act destructively on that answer.

// unknownIdentityClient builds a Client over tmpDir with one running instance
// whose recorded pid is live, and a /proc reader that fails with EACCES.
func unknownIdentityClient(t *testing.T, name string) (*Client, *config.FirecrackerConfig, int) {
	t.Helper()
	tmpDir := t.TempDir()
	cfg := testFirecrackerConfig(tmpDir)
	c, _ := newResumeTestClient(t, cfg, func(context.Context, int) (string, error) {
		return "", os.ErrPermission
	})
	pid := mustSpawnLiveChild(t)
	writeResumeInstance(t, tmpDir, name, config.StatusRunning, pid)
	swapPIDCmdlineReader(t, func(int) ([]byte, error) { return nil, fs.ErrPermission })
	return c, cfg, pid
}

func TestGetShedKeepsStatusWhenIdentityUnknown(t *testing.T) {
	c, cfg, pid := unknownIdentityClient(t, "alpha")

	shed, err := c.GetShed(context.Background(), "alpha")
	if err != nil {
		t.Fatalf("GetShed() error = %v", err)
	}
	if shed.Status != config.StatusRunning {
		t.Errorf("Status = %q, want %q: an unverifiable pid must not be reported as stopped", shed.Status, config.StatusRunning)
	}

	// And the metadata on disk must be untouched — rewriting it to
	// Stopped/PID=0 is what lets the next start spawn a second firecracker.
	meta, err := LoadMetadata(cfg.InstanceDir, "alpha")
	if err != nil {
		t.Fatalf("LoadMetadata() error = %v", err)
	}
	if meta.Status != config.StatusRunning || meta.PID != pid {
		t.Errorf("persisted metadata = (%q, pid %d), want (%q, pid %d)", meta.Status, meta.PID, config.StatusRunning, pid)
	}
}

func TestCheckNotRunningRefusesWhenIdentityUnknown(t *testing.T) {
	c, cfg, _ := unknownIdentityClient(t, "alpha")

	meta, err := LoadMetadata(cfg.InstanceDir, "alpha")
	if err != nil {
		t.Fatalf("LoadMetadata() error = %v", err)
	}
	starter := &fcStarter{c: c}
	err = starter.CheckNotRunning(context.Background(), &fcMetaHandle{meta: meta})
	if err == nil {
		t.Fatal("CheckNotRunning() = nil on an unverifiable pid; it must refuse to spawn a second firecracker over a possibly-live VMM")
	}
	if meta.PID == 0 {
		t.Error("CheckNotRunning() cleared the recorded PID on UNKNOWN")
	}
}

func TestDeleteShedRefusesWhenIdentityUnknown(t *testing.T) {
	c, cfg, _ := unknownIdentityClient(t, "alpha")
	srv := registerShedP9Server(t, c, "alpha")

	err := c.DeleteShed(context.Background(), "alpha")
	if err == nil {
		t.Fatal("DeleteShed() = nil on an unverifiable pid; it must not release the TAP/CID/upper of a possibly-live VMM")
	}
	if _, err := LoadMetadata(cfg.InstanceDir, "alpha"); err != nil {
		t.Errorf("metadata was removed despite the refusal: %v", err)
	}
	// The possibly-live VMM keeps its mounts too (#284).
	assertP9Servers(t, c, "alpha", []*P9Server{srv}, false)
}

// --- 9P servers live exactly as long as their VMM may (#284) ----------------
//
// A shed's 9P servers close only once its VMM is verifiably gone: after
// stop/kill whatever they returned, on every delete path that releases the
// shed, on the start/create rollback, and as a stale sweep when a start finds
// the shed not running. Where the VMM may still be alive they stay registered
// and listening, so a live guest's mounts keep working.

// newP9TeardownClient builds a Client for the 9P-teardown tests: a network
// manager (DeleteShed releases a TAP; a missing one is a no-op), an uppers
// dir inside the test's temp dir, and a short stop_timeout so the graceful
// path's guest calls — which fail fast against a VM that isn't there — and
// stopByPID's wait stay quick. 500 ms still leaves a SIGTERMed child ample
// time to exit and be reaped under -race before stopByPID gives up on it.
func newP9TeardownClient(t *testing.T) (*Client, *config.FirecrackerConfig) {
	t.Helper()
	tmpDir := t.TempDir()
	cfg := testFirecrackerConfig(tmpDir)
	cfg.UppersDir = filepath.Join(tmpDir, "uppers")
	cfg.StopTimeout = config.Duration(500 * time.Millisecond)
	c, _ := newResumeTestClient(t, cfg, nil)
	nm, err := NewNetworkManager(cfg.BridgeName, cfg.BridgeCIDR, cfg.TAPPrefix)
	if err != nil {
		t.Fatalf("NewNetworkManager: %v", err)
	}
	c.netMgr = nm
	return c, cfg
}

// registerShedP9Server starts a real 9P server on 127.0.0.1 and registers it
// under name, as startP9Server does for a mount.
func registerShedP9Server(t *testing.T, c *Client, name string) *P9Server {
	t.Helper()
	srv := newStartedP9Server(t, t.TempDir())
	c.mu.Lock()
	c.p9Servers[name] = append(c.p9Servers[name], srv)
	c.mu.Unlock()
	return srv
}

// registerShedP9Servers registers two servers, as a shed with a project mount
// and a credential mount would have.
func registerShedP9Servers(t *testing.T, c *Client, name string) []*P9Server {
	t.Helper()
	return []*P9Server{registerShedP9Server(t, c, name), registerShedP9Server(t, c, name)}
}

// p9ServerListening reports whether srv still accepts connections. A closed
// listener refuses at once; the generous timeout only guards a starved
// scheduler from turning a live listener into a false "closed".
func p9ServerListening(srv *P9Server) bool {
	conn, err := net.DialTimeout("tcp", srv.Addr(), 2*time.Second)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

// assertP9Servers checks name's servers are closed (unregistered and refusing
// connections) or kept (all still registered and listening).
func assertP9Servers(t *testing.T, c *Client, name string, servers []*P9Server, wantClosed bool) {
	t.Helper()
	c.mu.Lock()
	registered := len(c.p9Servers[name])
	c.mu.Unlock()
	for _, srv := range servers {
		if listening := p9ServerListening(srv); listening == wantClosed {
			t.Errorf("9P server %s listening = %v, want %v", srv.Addr(), listening, !wantClosed)
		}
	}
	switch {
	case wantClosed && registered != 0:
		t.Errorf("%d 9P server(s) still registered for %s, want them closed and unregistered", registered, name)
	case !wantClosed && registered != len(servers):
		t.Errorf("%d 9P server(s) registered for %s, want all %d kept", registered, name, len(servers))
	}
}

// mustDeadPID returns the pid of a child that has already exited and been
// reaped: a VMM that is verifiably gone.
func mustDeadPID(t *testing.T) int {
	t.Helper()
	cmd := exec.Command("true")
	if err := cmd.Run(); err != nil {
		t.Fatalf("run true: %v", err)
	}
	return cmd.Process.Pid
}

// mustSpawnReapedChild returns a live pid that is reaped the moment it exits,
// so once a stop kills it, it is gone from the process table — unlike
// mustSpawnLiveChild's child, which lingers as a zombie (still answering
// signal 0) until the test ends.
func mustSpawnReapedChild(t *testing.T) int {
	t.Helper()
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start sleep child: %v", err)
	}
	exited := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(exited)
	}()
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		<-exited
	})
	return cmd.Process.Pid
}

// readerOwns makes pid read as this shed's firecracker VMM (and every other
// pid as gone).
func readerOwns(cfg *config.FirecrackerConfig, name string, pid int) func(int) ([]byte, error) {
	return func(p int) ([]byte, error) {
		if p == pid {
			return []byte(fcCmdline(cfg, name)), nil
		}
		return nil, fs.ErrNotExist
	}
}

// readerUnknown makes every pid's identity unreadable (UNKNOWN).
func readerUnknown(int) ([]byte, error) { return nil, fs.ErrPermission }

// readerSomeoneElse makes pid read as an unrelated process: a recycled pid.
func readerSomeoneElse(int) ([]byte, error) { return []byte("/usr/bin/sleep\x0030"), nil }

func TestStopShedLockedClosesP9ServersOnlyOnceVMMGone(t *testing.T) {
	const name = "alpha"
	tests := []struct {
		name string
		mode stopMode
		// setup returns the recorded pid and may swap the /proc reader.
		setup      func(t *testing.T, cfg *config.FirecrackerConfig) int
		wantErr    error // nil: success; errAnyStopFailure: any error
		wantClosed bool
	}{
		{
			name:       "destroy, no recorded pid",
			mode:       stopDestroy,
			setup:      func(*testing.T, *config.FirecrackerConfig) int { return 0 },
			wantClosed: true,
		},
		{
			name:       "destroy, VMM already dead",
			mode:       stopDestroy,
			setup:      func(t *testing.T, _ *config.FirecrackerConfig) int { return mustDeadPID(t) },
			wantClosed: true,
		},
		{
			// vm.Kill fails (it cannot read the pid's identity), but the
			// pid is not alive: gone, whatever Kill returned.
			name: "destroy, kill error but VMM verifiably dead",
			mode: stopDestroy,
			setup: func(t *testing.T, _ *config.FirecrackerConfig) int {
				pid := mustDeadPID(t)
				swapPIDCmdlineReader(t, readerUnknown)
				return pid
			},
			wantErr:    errAnyStopFailure,
			wantClosed: true,
		},
		{
			name: "graceful, vm.Stop error but VMM verifiably dead",
			mode: stopGraceful,
			setup: func(t *testing.T, _ *config.FirecrackerConfig) int {
				pid := mustDeadPID(t)
				swapPIDCmdlineReader(t, readerUnknown)
				return pid
			},
			wantErr:    errAnyStopFailure,
			wantClosed: true,
		},
		{
			name: "graceful, VMM stops and is reaped",
			mode: stopGraceful,
			setup: func(t *testing.T, cfg *config.FirecrackerConfig) int {
				pid := mustSpawnReapedChild(t)
				swapPIDCmdlineReader(t, readerOwns(cfg, name, pid))
				return pid
			},
			wantClosed: true,
		},
		{
			name: "destroy, recorded pid recycled to another process",
			mode: stopDestroy,
			setup: func(t *testing.T, _ *config.FirecrackerConfig) int {
				pid := mustSpawnLiveChild(t)
				swapPIDCmdlineReader(t, readerSomeoneElse)
				return pid
			},
			wantClosed: true,
		},
		{
			name: "destroy, identity UNKNOWN",
			mode: stopDestroy,
			setup: func(t *testing.T, _ *config.FirecrackerConfig) int {
				pid := mustSpawnLiveChild(t)
				swapPIDCmdlineReader(t, readerUnknown)
				return pid
			},
			wantErr:    errAnyStopFailure,
			wantClosed: false,
		},
		{
			name: "graceful, identity UNKNOWN",
			mode: stopGraceful,
			setup: func(t *testing.T, _ *config.FirecrackerConfig) int {
				pid := mustSpawnLiveChild(t)
				swapPIDCmdlineReader(t, readerUnknown)
				return pid
			},
			wantErr:    errAnyStopFailure,
			wantClosed: false,
		},
		{
			// The stop returns nil but the pid is still this shed's VMM
			// (a zombie still answers signal 0): the existing post-check's
			// ErrStopIncomplete, reached from the same single probe.
			name: "graceful, VMM still ours after the stop",
			mode: stopGraceful,
			setup: func(t *testing.T, cfg *config.FirecrackerConfig) int {
				pid := mustSpawnLiveChild(t)
				swapPIDCmdlineReader(t, readerOwns(cfg, name, pid))
				return pid
			},
			wantErr:    config.ErrStopIncompleteSentinel,
			wantClosed: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, cfg := newP9TeardownClient(t)
			pid := tt.setup(t, cfg)
			writeResumeInstance(t, cfg.InstanceDir, name, config.StatusRunning, pid)
			servers := registerShedP9Servers(t, c, name)
			meta, err := LoadMetadata(cfg.InstanceDir, name)
			if err != nil {
				t.Fatalf("LoadMetadata: %v", err)
			}

			_, err = c.stopShedLocked(context.Background(), meta, tt.mode)
			switch {
			case tt.wantErr == nil && err != nil:
				t.Fatalf("stopShedLocked() error = %v, want nil", err)
			case tt.wantErr != nil && err == nil:
				t.Fatalf("stopShedLocked() = nil, want an error")
			case tt.wantErr != nil && tt.wantErr != errAnyStopFailure && !errors.Is(err, tt.wantErr):
				t.Fatalf("stopShedLocked() error = %v, want %v", err, tt.wantErr)
			}
			assertP9Servers(t, c, name, servers, tt.wantClosed)

			onDisk, err := LoadMetadata(cfg.InstanceDir, name)
			if err != nil {
				t.Fatalf("LoadMetadata after stop: %v", err)
			}
			wantStatus := config.StatusStopped
			if tt.wantErr != nil {
				wantStatus = config.StatusRunning
			}
			if onDisk.Status != wantStatus {
				t.Errorf("persisted status = %q, want %q", onDisk.Status, wantStatus)
			}
		})
	}
}

// errAnyStopFailure marks a table case that expects some error without
// pinning which one.
var errAnyStopFailure = errors.New("any stop failure")

func TestDeleteShedClosesStaleP9ServersOfStoppedShed(t *testing.T) {
	c, cfg := newP9TeardownClient(t)
	writeResumeInstance(t, cfg.InstanceDir, "alpha", config.StatusStopped, 0)
	// Left behind by a VMM exit that GetShed reconciled to Stopped.
	servers := registerShedP9Servers(t, c, "alpha")

	if err := c.DeleteShed(context.Background(), "alpha"); err != nil {
		t.Fatalf("DeleteShed() error = %v", err)
	}
	assertP9Servers(t, c, "alpha", servers, true)
	if _, err := LoadMetadata(cfg.InstanceDir, "alpha"); !errors.Is(err, ErrInstanceNotFound) {
		t.Errorf("metadata still present after delete (err = %v)", err)
	}
}

// The delete fallback used to log a VMM that survived SIGKILL and release its
// TAP, reservations and upper anyway. It now refuses, as it already did for
// UNKNOWN identity, and keeps everything — the 9P servers included.
func TestDeleteShedRefusesWhenVMMSurvivesSIGKILL(t *testing.T) {
	c, cfg := newP9TeardownClient(t)
	// A child that is never reaped during the test: once SIGKILLed it is a
	// zombie, which still answers signal 0 — a VMM that "survived" SIGKILL.
	pid := mustSpawnLiveChild(t)
	writeResumeInstance(t, cfg.InstanceDir, "alpha", config.StatusRunning, pid)
	meta, err := LoadMetadata(cfg.InstanceDir, "alpha")
	if err != nil {
		t.Fatalf("LoadMetadata: %v", err)
	}
	c.RegisterInstance("alpha", meta.CID, meta.IPAddress)
	upperDir := UpperDir(cfg.UppersDir, "alpha")
	if err := os.MkdirAll(upperDir, 0o755); err != nil {
		t.Fatalf("mkdir upper: %v", err)
	}
	servers := registerShedP9Servers(t, c, "alpha")

	// The first identity read fails, so stopShedLocked's kill fails and
	// DeleteShed falls back; every later read says "ours", so the fallback
	// SIGKILLs and then waits on a pid that never goes away.
	owns := readerOwns(cfg, "alpha", pid)
	reads := 0
	swapPIDCmdlineReader(t, func(p int) ([]byte, error) {
		reads++
		if reads == 1 {
			return nil, fs.ErrPermission
		}
		return owns(p)
	})

	err = c.DeleteShed(context.Background(), "alpha")
	if !errors.Is(err, config.ErrStopIncompleteSentinel) {
		t.Fatalf("DeleteShed() error = %v, want ErrStopIncomplete: a VMM that survived SIGKILL must not have its resources released", err)
	}
	if _, err := LoadMetadata(cfg.InstanceDir, "alpha"); err != nil {
		t.Errorf("metadata removed despite the refusal: %v", err)
	}
	if _, err := os.Stat(upperDir); err != nil {
		t.Errorf("upper removed despite the refusal: %v", err)
	}
	c.mu.Lock()
	_, cidHeld := c.usedCIDs[meta.CID]
	_, ipHeld := c.usedIPs[meta.IPAddress]
	c.mu.Unlock()
	if !cidHeld || !ipHeld {
		t.Errorf("reservations released despite the refusal: cid held = %v, ip held = %v", cidHeld, ipHeld)
	}
	assertP9Servers(t, c, "alpha", servers, false)
}

func TestCheckNotRunningSweepsStaleP9Servers(t *testing.T) {
	const name = "alpha"
	tests := []struct {
		name   string
		status string
		// setup returns the recorded pid and may swap the /proc reader.
		setup func(t *testing.T, cfg *config.FirecrackerConfig) int
		// handlePID, when set, gives this server an in-process VM handle
		// (c.vms) for the shed with the returned VMM pid. It runs after
		// setup, so it may swap the /proc reader again.
		handlePID  func(t *testing.T, cfg *config.FirecrackerConfig) int
		wantErr    error // nil: not running; errAnyStopFailure: any error
		wantClosed bool
	}{
		{
			name:       "stopped, no recorded pid",
			status:     config.StatusStopped,
			setup:      func(*testing.T, *config.FirecrackerConfig) int { return 0 },
			wantClosed: true,
		},
		{
			// GetShed never got to reconcile this one; the start does.
			name:       "running record, VMM exited unexpectedly",
			status:     config.StatusRunning,
			setup:      func(t *testing.T, _ *config.FirecrackerConfig) int { return mustDeadPID(t) },
			wantClosed: true,
		},
		{
			name:   "running and still ours",
			status: config.StatusRunning,
			setup: func(t *testing.T, cfg *config.FirecrackerConfig) int {
				pid := mustSpawnLiveChild(t)
				swapPIDCmdlineReader(t, readerOwns(cfg, name, pid))
				return pid
			},
			wantErr:    config.ErrShedAlreadyRunningSentinel,
			wantClosed: false,
		},
		{
			name:   "running, identity UNKNOWN",
			status: config.StatusRunning,
			setup: func(t *testing.T, _ *config.FirecrackerConfig) int {
				pid := mustSpawnLiveChild(t)
				swapPIDCmdlineReader(t, readerUnknown)
				return pid
			},
			wantErr:    errAnyStopFailure,
			wantClosed: false,
		},
		{
			name:   "stopped, but the recorded pid is a live VMM of ours",
			status: config.StatusStopped,
			setup: func(t *testing.T, cfg *config.FirecrackerConfig) int {
				pid := mustSpawnLiveChild(t)
				swapPIDCmdlineReader(t, readerOwns(cfg, name, pid))
				return pid
			},
			wantErr:    config.ErrZombiePresentSentinel,
			wantClosed: false,
		},
		{
			// GetShed's unlocked lost update: the record says Stopped/PID=0,
			// but this server's VM handle is a live VMM of ours. Its mounts
			// must survive the sweep.
			name:   "stale stopped record, in-process VM handle live and ours",
			status: config.StatusStopped,
			setup:  func(*testing.T, *config.FirecrackerConfig) int { return 0 },
			handlePID: func(t *testing.T, cfg *config.FirecrackerConfig) int {
				pid := mustSpawnLiveChild(t)
				swapPIDCmdlineReader(t, readerOwns(cfg, name, pid))
				return pid
			},
			wantClosed: false,
		},
		{
			name:   "stale stopped record, in-process VM handle identity UNKNOWN",
			status: config.StatusStopped,
			setup:  func(*testing.T, *config.FirecrackerConfig) int { return 0 },
			handlePID: func(t *testing.T, _ *config.FirecrackerConfig) int {
				pid := mustSpawnLiveChild(t)
				swapPIDCmdlineReader(t, readerUnknown)
				return pid
			},
			wantClosed: false,
		},
		{
			name:       "stopped, in-process VM handle whose VMM is dead",
			status:     config.StatusStopped,
			setup:      func(*testing.T, *config.FirecrackerConfig) int { return 0 },
			handlePID:  func(t *testing.T, _ *config.FirecrackerConfig) int { return mustDeadPID(t) },
			wantClosed: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, cfg := newP9TeardownClient(t)
			pid := tt.setup(t, cfg)
			writeResumeInstance(t, cfg.InstanceDir, name, tt.status, pid)
			if tt.handlePID != nil {
				handle := testMetadata(name)
				handle.Status = config.StatusRunning
				handle.PID = tt.handlePID(t, cfg)
				c.mu.Lock()
				c.vms[name] = &VM{meta: handle, cfg: cfg}
				c.mu.Unlock()
			}
			servers := registerShedP9Servers(t, c, name)
			meta, err := LoadMetadata(cfg.InstanceDir, name)
			if err != nil {
				t.Fatalf("LoadMetadata: %v", err)
			}

			err = (&fcStarter{c: c}).CheckNotRunning(context.Background(), &fcMetaHandle{meta: meta})
			switch {
			case tt.wantErr == nil && err != nil:
				t.Fatalf("CheckNotRunning() error = %v, want nil", err)
			case tt.wantErr != nil && err == nil:
				t.Fatalf("CheckNotRunning() = nil, want an error")
			case tt.wantErr != nil && tt.wantErr != errAnyStopFailure && !errors.Is(err, tt.wantErr):
				t.Fatalf("CheckNotRunning() error = %v, want %v", err, tt.wantErr)
			}
			assertP9Servers(t, c, name, servers, tt.wantClosed)
		})
	}
}

// The start/create rollback registers "stop 9P servers" before "stop VM", so
// the LIFO unwind decides about the servers only after the VM stop — and
// closes them only if the VMM is then verifiably gone.
func TestRegisterStopVMCleanupsClosesP9ServersOnlyOnceVMMGone(t *testing.T) {
	const name = "alpha"
	tests := []struct {
		name string
		// setup returns the VM's pid and may swap the /proc reader.
		setup      func(t *testing.T, cfg *config.FirecrackerConfig) int
		wantClosed bool
	}{
		{
			name:       "no recorded pid, stop succeeds",
			setup:      func(*testing.T, *config.FirecrackerConfig) int { return 0 },
			wantClosed: true,
		},
		{
			name:       "VMM already dead",
			setup:      func(t *testing.T, _ *config.FirecrackerConfig) int { return mustDeadPID(t) },
			wantClosed: true,
		},
		{
			// Alive and ours until "stop VM" kills it. Had "stop 9P
			// servers" run first, it would have found a live VMM and kept
			// the servers.
			name: "VMM ours until the stop kills it",
			setup: func(t *testing.T, cfg *config.FirecrackerConfig) int {
				pid := mustSpawnReapedChild(t)
				swapPIDCmdlineReader(t, readerOwns(cfg, name, pid))
				return pid
			},
			wantClosed: true,
		},
		{
			name: "identity UNKNOWN",
			setup: func(t *testing.T, _ *config.FirecrackerConfig) int {
				pid := mustSpawnLiveChild(t)
				swapPIDCmdlineReader(t, readerUnknown)
				return pid
			},
			wantClosed: false,
		},
		{
			name: "VMM survives the stop",
			setup: func(t *testing.T, cfg *config.FirecrackerConfig) int {
				pid := mustSpawnLiveChild(t) // lingers as a zombie: still answers signal 0
				swapPIDCmdlineReader(t, readerOwns(cfg, name, pid))
				return pid
			},
			wantClosed: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, cfg := newP9TeardownClient(t)
			meta := testMetadata(name)
			meta.Status = config.StatusRunning
			meta.PID = tt.setup(t, cfg)
			vm := &VM{meta: meta, cfg: cfg}
			servers := registerShedP9Servers(t, c, name)

			cleanup := backend.NewCleanup()
			c.registerStopVMCleanups(cleanup, vm)
			cleanup.Run() // a failed start/create unwinding

			assertP9Servers(t, c, name, servers, tt.wantClosed)
		})
	}
}

// Daemon exit closes every shed's servers, in parallel, without waiting for
// any guest to hang up — the #369 exception.
func TestClientCloseClosesEveryShedsP9Servers(t *testing.T) {
	c, _ := newP9TeardownClient(t)
	var servers []*P9Server
	var reads []<-chan error
	for _, name := range []string{"alpha", "beta"} {
		for _, srv := range registerShedP9Servers(t, c, name) {
			servers = append(servers, srv)
			reads = append(reads, readUntilHangup(dialMidTversion(t, srv)))
		}
	}

	start := time.Now()
	if err := c.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if elapsed := time.Since(start); elapsed >= closeBound {
		t.Errorf("Client.Close() took %v with a guest connected to each server, want < %v (the 5 s backstop means it waited for the guests)", elapsed, closeBound)
	}
	for _, read := range reads {
		assertHungUp(t, <-read)
	}
	for _, name := range []string{"alpha", "beta"} {
		assertP9Servers(t, c, name, nil, true)
	}
	for _, srv := range servers {
		if p9ServerListening(srv) {
			t.Errorf("9P server %s still listening after Client.Close", srv.Addr())
		}
	}
}
