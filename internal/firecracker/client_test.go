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
	"testing"
	"time"

	"github.com/charliek/shed/internal/config"
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
		})
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
		LocalDir:   "/home/user/projects/myproject",
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
	if shed.LocalDir != meta.LocalDir {
		t.Errorf("LocalDir = %q, want %q", shed.LocalDir, meta.LocalDir)
	}
}

func TestMetadataToShed_EmptyLocalDir(t *testing.T) {
	meta := &Metadata{
		Name:      "no-localdir",
		Status:    config.StatusStopped,
		Backend:   config.BackendFirecracker,
		IPAddress: "172.30.0.2",
		CPUs:      2,
		MemoryMB:  1024,
	}

	shed := metadataToShed(meta)

	if shed.LocalDir != "" {
		t.Errorf("LocalDir = %q, want empty string", shed.LocalDir)
	}
}

func TestMetadataBackwardCompat(t *testing.T) {
	// Test loading metadata JSON that doesn't include the local_dir field
	// (written before 9P support was added). The LocalDir field should be
	// empty after loading.
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

	// Verify LocalDir is empty (zero value)
	if loaded.LocalDir != "" {
		t.Errorf("LocalDir = %q, want empty string for backward-compat metadata", loaded.LocalDir)
	}

	// Verify other fields loaded correctly
	if loaded.Name != "old-vm" {
		t.Errorf("Name = %q, want %q", loaded.Name, "old-vm")
	}
	if loaded.Repo != "https://github.com/example/repo" {
		t.Errorf("Repo = %q, want %q", loaded.Repo, "https://github.com/example/repo")
	}

	// Verify metadataToShed also works with empty LocalDir
	shed := metadataToShed(loaded)
	if shed.LocalDir != "" {
		t.Errorf("metadataToShed().LocalDir = %q, want empty string", shed.LocalDir)
	}
}

func TestMetadataBackwardCompat_WithLocalDir(t *testing.T) {
	// Verify metadata with local_dir field loads correctly
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
  "local_dir": "/home/user/projects/myapp"
}`
	metaPath := filepath.Join(instanceDir, "metadata.json")
	if err := os.WriteFile(metaPath, []byte(raw), 0644); err != nil {
		t.Fatalf("failed to write metadata: %v", err)
	}

	loaded, err := LoadMetadata(dir, "new-vm")
	if err != nil {
		t.Fatalf("LoadMetadata() error = %v", err)
	}

	if loaded.LocalDir != "/home/user/projects/myapp" {
		t.Errorf("LocalDir = %q, want %q", loaded.LocalDir, "/home/user/projects/myapp")
	}
}

func TestMetadataLocalDir_RoundTrip(t *testing.T) {
	// Save metadata with LocalDir and verify it round-trips correctly
	dir := mustTempDir(t, "metadata-roundtrip")

	meta := testMetadata("roundtrip-vm")
	meta.LocalDir = "/tmp/test-project"

	if err := meta.Save(dir); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	loaded, err := LoadMetadata(dir, "roundtrip-vm")
	if err != nil {
		t.Fatalf("LoadMetadata() error = %v", err)
	}

	if loaded.LocalDir != meta.LocalDir {
		t.Errorf("LocalDir = %q, want %q", loaded.LocalDir, meta.LocalDir)
	}

	// Verify it's in the JSON
	data, err := os.ReadFile(MetadataPath(dir, "roundtrip-vm"))
	if err != nil {
		t.Fatalf("failed to read metadata file: %v", err)
	}

	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("failed to parse raw JSON: %v", err)
	}

	localDirRaw, ok := raw["local_dir"]
	if !ok {
		t.Fatal("local_dir key missing from JSON output")
	}

	var localDir string
	if err := json.Unmarshal(localDirRaw, &localDir); err != nil {
		t.Fatalf("failed to parse local_dir value: %v", err)
	}

	if localDir != "/tmp/test-project" {
		t.Errorf("local_dir in JSON = %q, want %q", localDir, "/tmp/test-project")
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
