//go:build darwin
// +build darwin

package vz

import (
	"context"
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
		Mounts: make(map[string]config.MountConfig),
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
// rule (snapshotLock -> createLock) holds under real contention. Two
// goroutines acquire the SAME snapshot and shed names so the locks actually
// contend; if either took them in the wrong order this would AB-BA deadlock.
//
// A start gate ensures both goroutines start the acquire dance simultaneously
// rather than one finishing before the other begins.
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
// CodeRabbit flagged on PR #81 and that wasn't caught by the live test
// because cleanup stopped sheds before deleting them.
//
// The check: with the lifecycle lock held, calling stopShedLocked must not
// itself try to acquire the same lock. We verify by holding the lock in the
// test goroutine and calling stopShedLocked with a stopped-state metadata
// (which short-circuits inside) and asserting it returns within a deadline.
func TestStopShedLockedDoesNotReacquireLock(t *testing.T) {
	c := &Client{}
	defer c.acquireCreateLock("test-shed")()

	done := make(chan struct{})
	go func() {
		// stopShedLocked on a stopped meta returns ErrShedNotRunningSentinel
		// without doing any work — but it must not block trying to take the
		// lifecycle lock the caller already holds.
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

func TestCreateShedValidatesResources(t *testing.T) {
	tmpDir := t.TempDir()
	baseRootfs := filepath.Join(tmpDir, "base-rootfs.ext4")
	if err := os.WriteFile(baseRootfs, []byte("rootfs"), 0644); err != nil {
		t.Fatalf("failed to write base rootfs: %v", err)
	}

	client := &Client{
		cfg: &config.VZConfig{
			DefaultImage: baseRootfs,
			InstanceDir:  tmpDir,
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

func TestCreateShedFromSnapshotMutualExclusionWrapsSentinel(t *testing.T) {
	client := &Client{
		cfg:     &config.VZConfig{InstanceDir: t.TempDir()},
		vms:     make(map[string]*VM),
		credMgr: newTestCredMgr(),
	}

	tests := []struct {
		name string
		req  config.CreateShedRequest
	}{
		{"with_image", config.CreateShedRequest{Name: "n", FromSnapshot: "snap1", Image: "default"}},
		{"with_repo", config.CreateShedRequest{Name: "n", FromSnapshot: "snap1", Repo: "git@github.com:o/r.git"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := client.CreateShed(context.Background(), tt.req)
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
func newResumeTestClient(t *testing.T, cfg *config.VZConfig, inspect func(context.Context, int) (string, error)) (*Client, *plugin.Bridge) {
	t.Helper()
	bridge := plugin.NewBridge(plugin.NewRegistry())
	c := &Client{
		cfg:         cfg,
		vms:         make(map[string]*VM),
		credMgr:     vmutil.NewCredentialManager(nil, bridge, string(config.BackendVZ), vmutil.NewHealthTracker()),
		procCmdline: inspect,
	}
	t.Cleanup(func() { _ = c.Close() })
	return c, bridge
}

// writeResumeInstance writes metadata for `name` in the state the walk will
// find it in.
func writeResumeInstance(t *testing.T, dir, name, status string, pid int) {
	t.Helper()
	meta := &Metadata{
		Name:       name,
		Status:     status,
		CreatedAt:  time.Now(),
		Backend:    string(config.BackendVZ),
		PID:        pid,
		CPUs:       2,
		MemoryMB:   512,
		RootfsPath: filepath.Join(dir, name, "rootfs.ext4"),
	}
	if err := meta.Save(dir); err != nil {
		t.Fatalf("save metadata for %q: %v", name, err)
	}
}

// defaultTestVfkitPath is the vfkit_path the walk cells configure. The
// resume predicate matches argv[0] against THIS, not a hard-coded "vfkit".
const defaultTestVfkitPath = "/opt/homebrew/bin/vfkit"

// vfkitCmdline is what a live vfkit serving `name` looks like in `ps -ww`
// output: the console log it was given identifies the instance.
func vfkitCmdline(cfg *config.VZConfig, name string) string {
	return vfkitCmdlineFrom(cfg.VfkitPath, cfg, name)
}

// vfkitCmdlineFrom is vfkitCmdline with argv[0] chosen by the caller, so a
// cell can model a recycled PID running something that is NOT the VMM while
// still naming the instance's console log.
func vfkitCmdlineFrom(argv0 string, cfg *config.VZConfig, name string) string {
	return strings.Join([]string{
		argv0,
		"--cpus", "2",
		"--memory", "512",
		"--device", "virtio-serial,logFilePath=" + filepath.Join(cfg.InstanceDir, name, "console.log"),
	}, " ")
}

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
		cmdline    func(cfg *config.VZConfig) string
		inspectErr error
		// vfkitPath overrides the configured VMM binary for this cell.
		vfkitPath string
		// shedName overrides the instance name, so a cell can use a name
		// that collides with the family token.
		shedName   string
		wantResume bool
	}{
		{
			name:       "running_and_alive_resumes",
			status:     config.StatusRunning,
			pid:        4242,
			cmdline:    func(cfg *config.VZConfig) string { return vfkitCmdline(cfg, shed) },
			wantResume: true,
		},
		{
			// NEGATIVE CONTROL: metadata still says running but vfkit is
			// gone, so `ps` fails. Deleting the liveness check in
			// resumeInstance must turn this cell red.
			name:       "control_running_but_dead_is_not_resumed",
			status:     config.StatusRunning,
			pid:        4242,
			inspectErr: os.ErrNotExist,
			wantResume: false,
		},
		{
			// The PID was recycled by a DIFFERENT shed's vfkit. A
			// family-only check ("is it vfkit?") would resume a dead
			// record here.
			name:   "running_with_reused_pid_is_not_resumed",
			status: config.StatusRunning,
			pid:    4242,
			cmdline: func(cfg *config.VZConfig) string {
				return vfkitCmdline(cfg, "some-other-shed")
			},
			wantResume: false,
		},
		{
			// The family check must be INDEPENDENT evidence from the
			// instance-path check. A shed NAMED vfkit has "vfkit" inside its
			// own console-log path, so a whole-command-line
			// Contains("vfkit") is satisfied by the path it is meant to
			// corroborate — and this cell (a recycled PID merely tailing that
			// log) would resume a dead record. Matching argv[0] stops it.
			name:     "control_non_vmm_pid_naming_the_console_log_is_not_resumed",
			status:   config.StatusRunning,
			pid:      4242,
			shedName: "vfkit",
			cmdline: func(cfg *config.VZConfig) string {
				return vfkitCmdlineFrom("/usr/bin/tail", cfg, "vfkit")
			},
			wantResume: false,
		},
		{
			// vfkit_path is configurable (vm.go:82 execs exactly it), so the
			// family check reads the CONFIGURED basename. A hard-coded
			// "vfkit" would refuse to resume this genuinely live shed.
			name:      "custom_vfkit_path_still_resumes",
			status:    config.StatusRunning,
			pid:       4242,
			vfkitPath: "/usr/local/bin/vmm",
			cmdline: func(cfg *config.VZConfig) string {
				return vfkitCmdlineFrom("/usr/local/bin/vmm", cfg, shed)
			},
			wantResume: true,
		},
		{
			name:   "stopped_is_not_resumed",
			status: config.StatusStopped,
			pid:    0,
			cmdline: func(cfg *config.VZConfig) string {
				return vfkitCmdline(cfg, shed)
			},
			wantResume: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tmpDir := t.TempDir()
			cfg := &config.VZConfig{
				InstanceDir: tmpDir,
				SocketDir:   filepath.Join(tmpDir, "sockets"),
				ConsolePort: 1024,
				NotifyPort:  1026,
				VfkitPath:   defaultTestVfkitPath,
			}
			if tt.vfkitPath != "" {
				cfg.VfkitPath = tt.vfkitPath
			}
			name := shed
			if tt.shedName != "" {
				name = tt.shedName
			}
			writeResumeInstance(t, tmpDir, name, tt.status, tt.pid)

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
				if len(got) != 1 || got[0] != name {
					t.Fatalf("resumed = %v, want [%s]", got, name)
				}
			} else if len(got) != 0 {
				t.Fatalf("resumed = %v, want nothing", got)
			}
		})
	}

	// One wedged `ps` must not stall the walk: the other instances still get
	// resumed. The per-inspection timeout is shortened so the cell costs
	// milliseconds rather than the production 2 s.
	t.Run("wedged_inspection_does_not_stall_the_walk", func(t *testing.T) {
		restore := resumeInspectTimeout
		resumeInspectTimeout = 50 * time.Millisecond
		t.Cleanup(func() { resumeInspectTimeout = restore })

		tmpDir := t.TempDir()
		cfg := &config.VZConfig{
			InstanceDir: tmpDir,
			SocketDir:   filepath.Join(tmpDir, "sockets"),
			ConsolePort: 1024,
			NotifyPort:  1026,
			VfkitPath:   defaultTestVfkitPath,
		}

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
					return vfkitCmdline(cfg, name), nil
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
		cfg := &config.VZConfig{
			InstanceDir: tmpDir,
			SocketDir:   filepath.Join(tmpDir, "sockets"),
			ConsolePort: 1024,
			NotifyPort:  1026,
			VfkitPath:   defaultTestVfkitPath,
		}
		writeResumeInstance(t, tmpDir, shed, config.StatusRunning, 4242)

		inspect := func(_ context.Context, _ int) (string, error) {
			return vfkitCmdline(cfg, shed), nil
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
