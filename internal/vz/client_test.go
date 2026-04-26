//go:build darwin
// +build darwin

package vz

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/charliek/shed/internal/config"
	"github.com/charliek/shed/internal/vmutil"
)

// newTestCredMgr creates a CredentialManager with nil server config for tests.
func newTestCredMgr() *vmutil.CredentialManager {
	return vmutil.NewCredentialManager(nil, nil, "test", nil)
}

func TestBuildEnvForGit(t *testing.T) {
	serverCfg := &config.ServerConfig{
		EnvVars: map[string]string{
			"GITHUB_TOKEN": "ghp_abc123",
			"GIT_AUTHOR":   "test",
		},
	}

	env := vmutil.BuildEnvForGit(serverCfg)

	if len(env) != 2 {
		t.Fatalf("expected 2 env vars, got %d", len(env))
	}

	envMap := make(map[string]bool)
	for _, e := range env {
		envMap[e] = true
	}

	if !envMap["GITHUB_TOKEN=ghp_abc123"] {
		t.Error("expected GITHUB_TOKEN in env")
	}
	if !envMap["GIT_AUTHOR=test"] {
		t.Error("expected GIT_AUTHOR in env")
	}
}

func TestBuildEnvForGitNilServerCfg(t *testing.T) {
	env := vmutil.BuildEnvForGit(nil)
	if len(env) != 0 {
		t.Errorf("expected empty env for nil serverCfg, got %v", env)
	}
}

func TestBuildEnvForGitNoEnvVars(t *testing.T) {
	serverCfg := &config.ServerConfig{
		EnvVars: map[string]string{},
	}
	env := vmutil.BuildEnvForGit(serverCfg)
	if len(env) != 0 {
		t.Errorf("expected empty env for empty EnvVars, got %v", env)
	}
}

func TestGetNetworkEndpoint(t *testing.T) {
	tmpDir := t.TempDir()

	cfg := &config.VZConfig{
		InstanceDir: tmpDir,
	}

	// Create a valid metadata file
	meta := &Metadata{
		Name:     "test-vm",
		Status:   config.StatusRunning,
		Backend:  config.BackendVZ,
		CPUs:     2,
		MemoryMB: 4096,
	}
	meta.Save(tmpDir)

	client := &Client{
		cfg:     cfg,
		vms:     make(map[string]*VM),
		credMgr: newTestCredMgr(),
	}

	endpoint, err := client.GetNetworkEndpoint(context.Background(), "test-vm")
	if err != nil {
		t.Fatalf("GetNetworkEndpoint() error = %v", err)
	}
	if endpoint != "127.0.0.1" {
		t.Errorf("GetNetworkEndpoint() = %q, want %q", endpoint, "127.0.0.1")
	}
}

func TestGetNetworkEndpointNotFound(t *testing.T) {
	tmpDir := t.TempDir()

	cfg := &config.VZConfig{
		InstanceDir: tmpDir,
	}

	client := &Client{
		cfg:     cfg,
		vms:     make(map[string]*VM),
		credMgr: newTestCredMgr(),
	}

	_, err := client.GetNetworkEndpoint(context.Background(), "nonexistent")
	if err == nil {
		t.Error("GetNetworkEndpoint() expected error for nonexistent shed")
	}
}

func TestDialServiceNotFound(t *testing.T) {
	tmpDir := t.TempDir()
	cfg := &config.VZConfig{
		InstanceDir:  tmpDir,
		TCPProxyPort: 1028,
	}

	client := &Client{
		cfg:     cfg,
		vms:     make(map[string]*VM),
		credMgr: newTestCredMgr(),
	}

	_, err := client.DialService(context.Background(), "nonexistent", 8080)
	if err == nil {
		t.Fatal("DialService() expected error for nonexistent shed")
	}
	expected := fmt.Sprintf("%s: %s", config.ErrShedNotFoundSentinel, "nonexistent")
	if err.Error() != expected {
		t.Errorf("error = %q, want %q", err.Error(), expected)
	}
}

func TestDialServiceNotRunning(t *testing.T) {
	tmpDir := t.TempDir()
	cfg := &config.VZConfig{
		InstanceDir:  tmpDir,
		SocketDir:    tmpDir,
		TCPProxyPort: 1028,
	}

	// Create metadata for a stopped VM
	meta := &Metadata{
		Name:   "stopped-vm",
		Status: config.StatusStopped,
	}
	if err := meta.Save(tmpDir); err != nil {
		t.Fatalf("save metadata: %v", err)
	}

	client := &Client{
		cfg:     cfg,
		vms:     make(map[string]*VM),
		credMgr: newTestCredMgr(),
	}

	_, err := client.DialService(context.Background(), "stopped-vm", 8080)
	if err == nil {
		t.Fatal("DialService() expected error for stopped shed")
	}
	if !strings.Contains(err.Error(), config.ErrShedNotRunningSentinel.Error()) {
		t.Errorf("error = %q, want to contain %q", err.Error(), config.ErrShedNotRunningSentinel.Error())
	}
}

func TestNewClientCreation(t *testing.T) {
	cfg := &config.VZConfig{
		InstanceDir: t.TempDir(),
	}
	serverCfg := &config.ServerConfig{
		Credentials: make(map[string]config.MountConfig),
	}

	client, err := NewClient(cfg, serverCfg, nil)
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	if client == nil {
		t.Fatal("NewClient() returned nil")
	}
	if client.cfg != cfg {
		t.Error("client.cfg should reference the provided config")
	}
	if client.serverCfg != serverCfg {
		t.Error("client.serverCfg should reference the provided server config")
	}
}

func TestNewAgentClient(t *testing.T) {
	cfg := &config.VZConfig{
		SocketDir:   "/tmp/test-sockets",
		ConsolePort: 1024,
		NotifyPort:  1026,
	}

	client := &Client{cfg: cfg}
	agent := client.newAgentClient("test-vm")

	if agent == nil {
		t.Fatal("newAgentClient() returned nil")
	}
	if agent.NotifyPort() != 1026 {
		t.Errorf("NotifyPort() = %d, want 1026", agent.NotifyPort())
	}
}

func TestClientClose(t *testing.T) {
	cfg := &config.VZConfig{}
	client := &Client{
		cfg:     cfg,
		vms:     make(map[string]*VM),
		credMgr: newTestCredMgr(),
	}

	err := client.Close()
	if err != nil {
		t.Errorf("Close() error = %v", err)
	}
}

func TestCredentialManagerNoServerCfg(t *testing.T) {
	// Creating a CredentialManager with nil serverCfg should not panic
	credMgr := vmutil.NewCredentialManager(nil, nil, "test", nil)
	// Operations should not panic
	credMgr.StopListener("test")
	credMgr.Close()
}

func TestStopListenerNoOp(t *testing.T) {
	credMgr := vmutil.NewCredentialManager(nil, nil, "test", nil)
	// Should not panic when stopping a non-existent listener
	credMgr.StopListener("nonexistent")
}

func TestGetShedNotFound(t *testing.T) {
	cfg := &config.VZConfig{
		InstanceDir: t.TempDir(),
	}
	client := &Client{
		cfg:     cfg,
		vms:     make(map[string]*VM),
		credMgr: newTestCredMgr(),
	}

	_, err := client.GetShed(context.Background(), "nonexistent")
	if err == nil {
		t.Error("GetShed() expected error for nonexistent shed")
	}
	expected := fmt.Sprintf("%s: %s", config.ErrShedNotFoundSentinel, "nonexistent")
	if err.Error() != expected {
		t.Errorf("GetShed() error = %q, want %q", err.Error(), expected)
	}
}

func TestBuildCredentialShares(t *testing.T) {
	creds := map[string]config.MountConfig{
		"ssh": {Source: "/home/user/.ssh", Target: "/home/shed/.ssh"},
		"gh":  {Source: "/home/user/.config/gh", Target: "/home/shed/.config/gh"},
	}

	shares := buildCredentialShares(creds)

	if len(shares) != 2 {
		t.Fatalf("expected 2 shares, got %d", len(shares))
	}

	// Build a map for order-independent checking
	shareMap := make(map[string]string)
	for _, s := range shares {
		shareMap[s.MountTag] = s.SourceDir
	}

	if shareMap["cred-ssh"] != "/home/user/.ssh" {
		t.Error("expected cred-ssh share with source /home/user/.ssh")
	}
	if shareMap["cred-gh"] != "/home/user/.config/gh" {
		t.Error("expected cred-gh share with source /home/user/.config/gh")
	}
}

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
// rule (snapshotLock -> createLock) is consistent: a holder taking both locks
// in that order does not deadlock against a peer taking either lock alone.
// The actual cross-product is enforced at all callers (CreateShed-from-snapshot
// and CreateSnapshot both acquire snapshot first); this test guards the
// invariant by exercising both halves of a paired acquire under timeout.
func TestSnapshotAndCreateLockOrderNoDeadlock(t *testing.T) {
	c := &Client{}

	// Goroutine A: takes snapshot, then create. Holds briefly.
	doneA := make(chan struct{})
	go func() {
		releaseSnap := c.acquireSnapshotLock("snap-a")
		time.Sleep(10 * time.Millisecond)
		releaseCreate := c.acquireCreateLock("shed-a")
		releaseCreate()
		releaseSnap()
		close(doneA)
	}()

	// Goroutine B: same lock order, different names. Must run in parallel.
	doneB := make(chan struct{})
	go func() {
		releaseSnap := c.acquireSnapshotLock("snap-b")
		releaseCreate := c.acquireCreateLock("shed-b")
		releaseCreate()
		releaseSnap()
		close(doneB)
	}()

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

func TestCreateShedValidatesResources(t *testing.T) {
	tmpDir := t.TempDir()
	baseRootfs := filepath.Join(tmpDir, "base-rootfs.ext4")
	if err := os.WriteFile(baseRootfs, []byte("rootfs"), 0644); err != nil {
		t.Fatalf("failed to write base rootfs: %v", err)
	}

	client := &Client{
		cfg: &config.VZConfig{
			BaseRootfs:  baseRootfs,
			InstanceDir: tmpDir,
		},
		vms:     make(map[string]*VM),
		credMgr: newTestCredMgr(),
	}

	_, err := client.CreateShed(context.Background(), config.CreateShedRequest{
		Name: "too-many-cpus",
		CPUs: config.MaxVZCPUs + 1,
	})
	if err == nil {
		t.Fatal("expected cpu validation error")
	}
	if RootfsExists(tmpDir, "too-many-cpus") {
		t.Fatal("rootfs should not be created for invalid cpu request")
	}
	if _, statErr := os.Stat(InstanceDir(tmpDir, "too-many-cpus")); !os.IsNotExist(statErr) {
		t.Fatalf("instance dir should not exist for invalid cpu request, stat err: %v", statErr)
	}

	_, err = client.CreateShed(context.Background(), config.CreateShedRequest{
		Name:     "too-little-memory",
		MemoryMB: 64,
	})
	if err == nil {
		t.Fatal("expected memory validation error")
	}
	if RootfsExists(tmpDir, "too-little-memory") {
		t.Fatal("rootfs should not be created for invalid memory request")
	}
	if _, statErr := os.Stat(InstanceDir(tmpDir, "too-little-memory")); !os.IsNotExist(statErr) {
		t.Fatalf("instance dir should not exist for invalid memory request, stat err: %v", statErr)
	}
}
