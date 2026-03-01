//go:build darwin
// +build darwin

package vz

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/charliek/shed/internal/config"
)

func TestBuildVfkitArgs(t *testing.T) {
	meta := &Metadata{
		Name:       "test-vm",
		CPUs:       4,
		MemoryMB:   8192,
		RootfsPath: "/tmp/test-rootfs.ext4",
	}
	cfg := &config.VZConfig{
		VfkitPath:   "vfkit",
		KernelPath:  "/tmp/vmlinux",
		InstanceDir: "",
		SocketDir:   "/tmp/sockets",
		ConsolePort: 1024,
		HealthPort:  1025,
		NotifyPort:  1026,
	}

	vm := &VM{meta: meta, cfg: cfg}
	args := vm.buildVfkitArgs()

	// Check that key args are present
	argsStr := strings.Join(args, " ")

	if !strings.Contains(argsStr, "--cpus 4") {
		t.Errorf("expected --cpus 4 in args, got: %s", argsStr)
	}
	if !strings.Contains(argsStr, "--memory 8192") {
		t.Errorf("expected --memory 8192 in args, got: %s", argsStr)
	}
	if !strings.Contains(argsStr, fmt.Sprintf("kernel=%s", cfg.KernelPath)) {
		t.Errorf("expected kernel path in args, got: %s", argsStr)
	}
	if !strings.Contains(argsStr, fmt.Sprintf("path=%s", meta.RootfsPath)) {
		t.Errorf("expected rootfs path in args, got: %s", argsStr)
	}

	// Check virtio-net NAT device
	if !strings.Contains(argsStr, "virtio-net,nat") {
		t.Errorf("expected virtio-net,nat in args, got: %s", argsStr)
	}

	// Check virtio-serial console log device
	if !strings.Contains(argsStr, "virtio-serial,logFilePath=") {
		t.Errorf("expected virtio-serial console log in args, got: %s", argsStr)
	}

	// Check vsock devices for each port (connect mode, no unix:// prefix)
	for _, port := range []uint32{1024, 1025, 1026} {
		socketPath := filepath.Join(cfg.SocketDir, fmt.Sprintf("test-vm-%d.sock", port))
		expected := fmt.Sprintf("port=%d,socketURL=%s,connect", port, socketPath)
		if !strings.Contains(argsStr, expected) {
			t.Errorf("expected vsock device for port %d in args, got: %s", port, argsStr)
		}
	}

	// Should have exactly 6 --device flags (1 block + 1 net + 1 serial + 3 vsock)
	deviceCount := strings.Count(argsStr, "--device")
	if deviceCount != 6 {
		t.Errorf("expected 6 --device flags (1 block + 1 net + 1 serial + 3 vsock), got %d", deviceCount)
	}
}

func TestBuildVfkitArgsKernelCmdline(t *testing.T) {
	meta := &Metadata{Name: "test-vm", CPUs: 2, MemoryMB: 4096, RootfsPath: "/tmp/rootfs.ext4"}
	cfg := &config.VZConfig{
		KernelPath:  "/tmp/vmlinux",
		InstanceDir: "",
		SocketDir:   "/tmp/sockets",
		ConsolePort: 1024,
		HealthPort:  1025,
		NotifyPort:  1026,
	}

	vm := &VM{meta: meta, cfg: cfg}
	args := vm.buildVfkitArgs()
	argsStr := strings.Join(args, " ")

	if !strings.Contains(argsStr, "console=hvc0") {
		t.Error("expected console=hvc0 in kernel cmdline")
	}
	if !strings.Contains(argsStr, "root=/dev/vda") {
		t.Error("expected root=/dev/vda in kernel cmdline")
	}
	if !strings.Contains(argsStr, "init=/sbin/init") {
		t.Error("expected init=/sbin/init in kernel cmdline")
	}
}

func TestCleanupSockets(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "vz-cleanup-test-*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	meta := &Metadata{Name: "test-vm"}
	cfg := &config.VZConfig{
		SocketDir:   tmpDir,
		ConsolePort: 1024,
		HealthPort:  1025,
		NotifyPort:  1026,
	}

	// Create socket files
	for _, port := range []uint32{1024, 1025, 1026} {
		path := filepath.Join(tmpDir, fmt.Sprintf("test-vm-%d.sock", port))
		if err := os.WriteFile(path, nil, 0600); err != nil {
			t.Fatalf("Failed to create socket file %s: %v", path, err)
		}
	}

	// Also create a socket for a different VM to make sure it's not deleted
	otherSocket := filepath.Join(tmpDir, "other-vm-1024.sock")
	if err := os.WriteFile(otherSocket, nil, 0600); err != nil {
		t.Fatalf("Failed to create socket file %s: %v", otherSocket, err)
	}

	vm := &VM{meta: meta, cfg: cfg}
	vm.cleanupSockets()

	// Check that our sockets are gone
	for _, port := range []uint32{1024, 1025, 1026} {
		path := filepath.Join(tmpDir, fmt.Sprintf("test-vm-%d.sock", port))
		if _, err := os.Stat(path); err == nil {
			t.Errorf("socket %s should have been removed", path)
		}
	}

	// Check that the other VM's socket is still there
	if _, err := os.Stat(otherSocket); err != nil {
		t.Error("other VM's socket should not have been removed")
	}
}

func TestIsVfkitProcessNonexistentPID(t *testing.T) {
	// Use a PID that almost certainly doesn't exist
	if isVfkitProcess(999999999) {
		t.Error("isVfkitProcess should return false for non-existent PID")
	}
}

func TestIsVfkitProcessCurrentProcess(t *testing.T) {
	// Current process is "go" or the test binary, not vfkit
	if isVfkitProcess(os.Getpid()) {
		t.Error("isVfkitProcess should return false for the test process")
	}
}

func TestIsRunningNotStarted(t *testing.T) {
	meta := &Metadata{PID: 0}
	cfg := &config.VZConfig{}
	vm := &VM{meta: meta, cfg: cfg}

	if vm.IsRunning() {
		t.Error("VM with PID 0 should not be running")
	}
}

func TestIsRunningStaleProcess(t *testing.T) {
	meta := &Metadata{PID: 999999999}
	cfg := &config.VZConfig{}
	vm := &VM{meta: meta, cfg: cfg}

	if vm.IsRunning() {
		t.Error("VM with non-existent PID should not be running")
	}
}

func TestIsRunningReapedProcess(t *testing.T) {
	done := make(chan struct{})
	close(done)

	meta := &Metadata{PID: os.Getpid()}
	cfg := &config.VZConfig{}
	vm := &VM{meta: meta, cfg: cfg, waitCh: done}

	if vm.IsRunning() {
		t.Error("VM with closed wait channel should not be running")
	}
}

func TestWaitForProcessExitNonExistent(t *testing.T) {
	// Non-existent process should return true immediately
	if !waitForProcessExit(999999999, 1*time.Second) {
		t.Error("waitForProcessExit should return true for non-existent PID")
	}
}

func TestCreateVM(t *testing.T) {
	meta := &Metadata{Name: "test-vm", CPUs: 2, MemoryMB: 4096}
	cfg := &config.VZConfig{VfkitPath: "vfkit"}

	vm := CreateVM(meta, cfg)
	if vm == nil {
		t.Fatal("CreateVM() returned nil")
	}
	if vm.meta != meta {
		t.Error("VM should reference the provided metadata")
	}
	if vm.cfg != cfg {
		t.Error("VM should reference the provided config")
	}
}
