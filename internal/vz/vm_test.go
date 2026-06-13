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
	"github.com/charliek/shed/internal/vmimage"
)

// setupBlobForTest installs a synthetic OCI image (rootfs layer + kernel +
// initrd blobs) under a temp imagesDir and returns (imagesDir, manifestDigest).
// buildVfkitArgs needs the manifest blob to exist along with an initrd
// annotation before it will produce a usable arg list.
func setupBlobForTest(t *testing.T) (imagesDir, digest string) {
	t.Helper()
	imagesDir = t.TempDir()
	rootfs := []byte("stub-rootfs-content")
	// Distinct kernel/initrd content so the synthetic helper writes
	// separate blobs and annotates the manifest with both digests.
	kernel := []byte("stub-kernel-content")
	initrd := []byte("stub-initrd-content")
	d, err := vmimage.InstallSyntheticImage(imagesDir, "stubimg", "ghcr.io/test/stub:v1", rootfs, kernel, initrd)
	if err != nil {
		t.Fatalf("InstallSyntheticImage: %v", err)
	}
	return imagesDir, d
}

func TestBuildVfkitArgs(t *testing.T) {
	imagesDir, digest := setupBlobForTest(t)
	meta := &Metadata{
		Name:        "test-vm",
		CPUs:        4,
		MemoryMB:    8192,
		RootfsPath:  "/tmp/test-rootfs.ext4",
		LowerDigest: digest,
	}
	cfg := &config.VZConfig{
		VfkitPath:    "vfkit",
		KernelPath:   "/tmp/vmlinux",
		ImagesDir:    imagesDir,
		InstanceDir:  "",
		SocketDir:    "/tmp/sockets",
		ConsolePort:  1024,
		NotifyPort:   1026,
		TCPProxyPort: 1028,
	}

	vm := &VM{meta: meta, cfg: cfg}
	args, err := vm.buildVfkitArgs()
	if err != nil {
		t.Fatalf("buildVfkitArgs: %v", err)
	}

	// Check that key args are present
	argsStr := strings.Join(args, " ")

	if !strings.Contains(argsStr, "--cpus 4") {
		t.Errorf("expected --cpus 4 in args, got: %s", argsStr)
	}
	if !strings.Contains(argsStr, "--memory 8192") {
		t.Errorf("expected --memory 8192 in args, got: %s", argsStr)
	}
	// The manifest carries an io.shed.kernel.digest annotation, so the
	// production code prefers the kernel blob over cfg.KernelPath.
	// Kernel is passed via dedicated --kernel flag (not folded into
	// --bootloader cmdline=) so the kernel cmdline can contain commas.
	if !strings.Contains(argsStr, "--kernel "+filepath.Join(cfg.ImagesDir, "blobs", "sha256")) {
		t.Errorf("expected --kernel <blob path> in args, got: %s", argsStr)
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
	for _, port := range []uint32{1024, 1026, 1028} {
		socketPath := filepath.Join(cfg.SocketDir, fmt.Sprintf("test-vm-%d.sock", port))
		expected := fmt.Sprintf("port=%d,socketURL=%s,connect", port, socketPath)
		if !strings.Contains(argsStr, expected) {
			t.Errorf("expected vsock device for port %d in args, got: %s", port, argsStr)
		}
	}

	// Should have exactly 7 --device flags
	// (2 block: upper+lower, 1 net, 1 serial, 3 vsock).
	deviceCount := strings.Count(argsStr, "--device")
	if deviceCount != 7 {
		t.Errorf("expected 7 --device flags (2 block + 1 net + 1 serial + 3 vsock), got %d", deviceCount)
	}

	// Lower (read-only) virtio-blk should be present.
	if !strings.Contains(argsStr, ",readonly") {
		t.Errorf("expected lower virtio-blk with vfkit `readonly` flag, got: %s", argsStr)
	}

	// No VirtioFS device when LocalDir is empty
	if strings.Contains(argsStr, "virtio-fs") {
		t.Error("should not have virtio-fs device when LocalDir is empty")
	}
}

func TestBuildVfkitArgsWithLocalDir(t *testing.T) {
	imagesDir, digest := setupBlobForTest(t)
	meta := &Metadata{
		Name:       "test-vm",
		CPUs:       2,
		MemoryMB:   4096,
		RootfsPath: "/tmp/rootfs.ext4",
		ProjectMounts: []config.MountConfig{
			{Source: "/Users/charlie/projects/myapp", Target: "/home/shed/myapp"},
		},
		LowerDigest: digest,
	}
	cfg := &config.VZConfig{
		KernelPath:   "/tmp/vmlinux",
		ImagesDir:    imagesDir,
		InstanceDir:  "",
		SocketDir:    "/tmp/sockets",
		ConsolePort:  1024,
		NotifyPort:   1026,
		TCPProxyPort: 1028,
	}

	vm := &VM{meta: meta, cfg: cfg}
	args, err := vm.buildVfkitArgs()
	if err != nil {
		t.Fatalf("buildVfkitArgs: %v", err)
	}
	argsStr := strings.Join(args, " ")

	// Should have VirtioFS device for the project mount
	tag := config.ProjectMountTagForTarget("/home/shed/myapp")
	expected := fmt.Sprintf("virtio-fs,sharedDir=/Users/charlie/projects/myapp,mountTag=%s", tag)
	if !strings.Contains(argsStr, expected) {
		t.Errorf("expected VirtioFS device %q in args, got: %s", expected, argsStr)
	}

	// Should have 8 --device flags (2 block + 1 net + 1 serial + 1 virtio-fs + 3 vsock)
	deviceCount := strings.Count(argsStr, "--device")
	if deviceCount != 8 {
		t.Errorf("expected 8 --device flags, got %d", deviceCount)
	}
}

func TestBuildVfkitArgsWithCredentialShares(t *testing.T) {
	imagesDir, digest := setupBlobForTest(t)
	meta := &Metadata{
		Name:        "test-vm",
		CPUs:        2,
		MemoryMB:    4096,
		RootfsPath:  "/tmp/rootfs.ext4",
		LowerDigest: digest,
	}
	cfg := &config.VZConfig{
		KernelPath:   "/tmp/vmlinux",
		ImagesDir:    imagesDir,
		InstanceDir:  "",
		SocketDir:    "/tmp/sockets",
		ConsolePort:  1024,
		NotifyPort:   1026,
		TCPProxyPort: 1028,
	}

	vm := &VM{
		meta: meta,
		cfg:  cfg,
		credentialShares: []credentialVirtioFS{
			{SourceDir: "/Users/charlie/.ssh", MountTag: "cred-git_ssh"},
			{SourceDir: "/Users/charlie/.config/gh", MountTag: "cred-gh"},
		},
	}
	args, err := vm.buildVfkitArgs()
	if err != nil {
		t.Fatalf("buildVfkitArgs: %v", err)
	}
	argsStr := strings.Join(args, " ")

	// Check credential VirtioFS devices
	if !strings.Contains(argsStr, "virtio-fs,sharedDir=/Users/charlie/.ssh,mountTag=cred-git_ssh") {
		t.Error("expected VirtioFS device for git_ssh credential")
	}
	if !strings.Contains(argsStr, "virtio-fs,sharedDir=/Users/charlie/.config/gh,mountTag=cred-gh") {
		t.Error("expected VirtioFS device for gh credential")
	}

	// Should have 9 --device flags (2 block + 1 net + 1 serial + 2 virtio-fs creds + 3 vsock)
	deviceCount := strings.Count(argsStr, "--device")
	if deviceCount != 9 {
		t.Errorf("expected 9 --device flags, got %d", deviceCount)
	}

	// No project VirtioFS device (no ProjectMounts)
	if strings.Contains(argsStr, "mountTag=proj-") {
		t.Error("should not have a project virtio-fs device when there are no project mounts")
	}
}

func TestBuildVfkitArgsWithCredentialSharesAndLocalDir(t *testing.T) {
	imagesDir, digest := setupBlobForTest(t)
	meta := &Metadata{
		Name:       "test-vm",
		CPUs:       2,
		MemoryMB:   4096,
		RootfsPath: "/tmp/rootfs.ext4",
		ProjectMounts: []config.MountConfig{
			{Source: "/Users/charlie/projects/myapp", Target: "/home/shed/myapp"},
		},
		LowerDigest: digest,
	}
	cfg := &config.VZConfig{
		KernelPath:   "/tmp/vmlinux",
		ImagesDir:    imagesDir,
		InstanceDir:  "",
		SocketDir:    "/tmp/sockets",
		ConsolePort:  1024,
		NotifyPort:   1026,
		TCPProxyPort: 1028,
	}

	vm := &VM{
		meta: meta,
		cfg:  cfg,
		credentialShares: []credentialVirtioFS{
			{SourceDir: "/Users/charlie/.claude", MountTag: "cred-claude"},
		},
	}
	args, err := vm.buildVfkitArgs()
	if err != nil {
		t.Fatalf("buildVfkitArgs: %v", err)
	}
	argsStr := strings.Join(args, " ")

	// Should have project VirtioFS
	if !strings.Contains(argsStr, "mountTag=proj-") {
		t.Error("expected project VirtioFS device")
	}

	// Should have credential VirtioFS
	if !strings.Contains(argsStr, "virtio-fs,sharedDir=/Users/charlie/.claude,mountTag=cred-claude") {
		t.Error("expected VirtioFS device for claude credential")
	}

	// Should have 9 --device flags (2 block + 1 net + 1 serial + 1 workspace virtio-fs + 1 cred virtio-fs + 3 vsock)
	deviceCount := strings.Count(argsStr, "--device")
	if deviceCount != 9 {
		t.Errorf("expected 9 --device flags, got %d", deviceCount)
	}
}

func TestBuildVfkitArgsNoCredentialShares(t *testing.T) {
	imagesDir, digest := setupBlobForTest(t)
	meta := &Metadata{
		Name:        "test-vm",
		CPUs:        2,
		MemoryMB:    4096,
		RootfsPath:  "/tmp/rootfs.ext4",
		LowerDigest: digest,
	}
	cfg := &config.VZConfig{
		KernelPath:   "/tmp/vmlinux",
		ImagesDir:    imagesDir,
		InstanceDir:  "",
		SocketDir:    "/tmp/sockets",
		ConsolePort:  1024,
		NotifyPort:   1026,
		TCPProxyPort: 1028,
	}

	vm := &VM{meta: meta, cfg: cfg, credentialShares: nil}
	args, err := vm.buildVfkitArgs()
	if err != nil {
		t.Fatalf("buildVfkitArgs: %v", err)
	}
	argsStr := strings.Join(args, " ")

	// Should have 7 --device flags (2 block + 1 net + 1 serial + 3 vsock) — no virtio-fs
	deviceCount := strings.Count(argsStr, "--device")
	if deviceCount != 7 {
		t.Errorf("expected 7 --device flags with no credential shares, got %d", deviceCount)
	}

	if strings.Contains(argsStr, "virtio-fs") {
		t.Error("should not have any virtio-fs device with no credentials and no LocalDir")
	}
}

func TestBuildVfkitArgsKernelCmdline(t *testing.T) {
	imagesDir, digest := setupBlobForTest(t)
	meta := &Metadata{Name: "test-vm", CPUs: 2, MemoryMB: 4096, RootfsPath: "/tmp/rootfs.ext4", LowerDigest: digest}
	cfg := &config.VZConfig{
		KernelPath:   "/tmp/vmlinux",
		ImagesDir:    imagesDir,
		InstanceDir:  "",
		SocketDir:    "/tmp/sockets",
		ConsolePort:  1024,
		NotifyPort:   1026,
		TCPProxyPort: 1028,
	}

	vm := &VM{meta: meta, cfg: cfg}
	args, err := vm.buildVfkitArgs()
	if err != nil {
		t.Fatalf("buildVfkitArgs: %v", err)
	}
	argsStr := strings.Join(args, " ")

	if !strings.Contains(argsStr, "console=hvc0") {
		t.Error("expected console=hvc0 in kernel cmdline")
	}
	// The shed initramfs builds the overlay itself, so the legacy
	// `root=/dev/vda rw` cmdline is replaced with shed.upper /
	// shed.lower naming the writable upper and the single
	// content-addressed lower.
	if strings.Contains(argsStr, "root=/dev/vda") {
		t.Errorf("expected no root=/dev/vda in kernel cmdline (initramfs builds overlay): %s", argsStr)
	}
	if !strings.Contains(argsStr, "shed.upper=/dev/vda") {
		t.Errorf("expected shed.upper=/dev/vda in kernel cmdline, got: %s", argsStr)
	}
	if !strings.Contains(argsStr, "shed.lower=/dev/vdb") {
		t.Errorf("expected shed.lower=/dev/vdb in kernel cmdline, got: %s", argsStr)
	}
	if !strings.Contains(argsStr, "init=/sbin/init") {
		t.Error("expected init=/sbin/init in kernel cmdline")
	}
	if !strings.Contains(argsStr, "shed.name=test-vm") {
		t.Errorf("expected shed.name=test-vm in kernel cmdline (read by shed-firstboot for identity regen), got: %s", argsStr)
	}
	// With no guest_mtu override and auto-detection of the host egress MTU,
	// shed.mtu= must be omitted on a normal (1500) path — never shed.mtu=0.
	if strings.Contains(argsStr, "shed.mtu=0") {
		t.Errorf("shed.mtu=0 must never be emitted, got: %s", argsStr)
	}
}

// TestBuildVfkitArgsGuestMTUOverride exercises the shed.mtu= cmdline wiring
// deterministically via the guest_mtu override (which short-circuits host
// detection), independent of whatever MTU the test host's egress interface has.
func TestBuildVfkitArgsGuestMTUOverride(t *testing.T) {
	imagesDir, digest := setupBlobForTest(t)
	meta := &Metadata{Name: "test-vm", CPUs: 2, MemoryMB: 4096, RootfsPath: "/tmp/rootfs.ext4", LowerDigest: digest}
	cfg := &config.VZConfig{
		ImagesDir:    imagesDir,
		SocketDir:    "/tmp/sockets",
		ConsolePort:  1024,
		NotifyPort:   1026,
		TCPProxyPort: 1028,
		GuestMTU:     1400,
	}

	vm := &VM{meta: meta, cfg: cfg}
	args, err := vm.buildVfkitArgs()
	if err != nil {
		t.Fatalf("buildVfkitArgs: %v", err)
	}
	if argsStr := strings.Join(args, " "); !strings.Contains(argsStr, "shed.mtu=1400") {
		t.Errorf("expected shed.mtu=1400 in kernel cmdline with guest_mtu override, got: %s", argsStr)
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
		SocketDir:    tmpDir,
		ConsolePort:  1024,
		NotifyPort:   1026,
		TCPProxyPort: 1028,
	}

	// Create socket files
	for _, port := range []uint32{1024, 1026, 1028} {
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
	for _, port := range []uint32{1024, 1026, 1028} {
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
