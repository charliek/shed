//go:build darwin
// +build darwin

package vz

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/charliek/shed/internal/config"
	"github.com/charliek/shed/internal/vmimage"
	"github.com/charliek/shed/internal/vmutil"
)

// credentialVirtioFS describes a VirtioFS share for a credential directory.
type credentialVirtioFS struct {
	SourceDir string
	MountTag  string
}

// VM represents a running VZ VM instance managed by a vfkit subprocess.
//
// VM is not safe for concurrent use. Callers (e.g. Client) must serialize
// calls to Start and Stop.
type VM struct {
	meta             *Metadata
	cfg              *config.VZConfig
	cmd              *exec.Cmd
	waitCh           chan struct{} // closed when cmd.Wait() completes (process reaped)
	credentialShares []credentialVirtioFS
}

// CreateVM creates a new VM instance (but does not start it).
func CreateVM(meta *Metadata, cfg *config.VZConfig) *VM {
	return &VM{
		meta: meta,
		cfg:  cfg,
	}
}

// Start starts the VM by launching vfkit as a subprocess.
func (vm *VM) Start(ctx context.Context) error {
	// Guard against a previously interrupted ResetShed (DeleteUpper
	// succeeded, EnsureUpper then failed): without this, vfkit would
	// fail with an opaque "open failed" on the virtio-blk device.
	// Surfacing a clean recovery hint here saves the operator from
	// digging through vfkit logs.
	if _, err := os.Stat(vm.meta.RootfsPath); err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("shed %s has no writable upper at %s; run `shed reset %s` to recreate it (or `shed delete %s` to abandon)", vm.meta.Name, vm.meta.RootfsPath, vm.meta.Name, vm.meta.Name)
		}
		return fmt.Errorf("stat upper at %s: %w", vm.meta.RootfsPath, err)
	}

	// Ensure socket directory exists
	if err := os.MkdirAll(vm.cfg.SocketDir, 0755); err != nil {
		return fmt.Errorf("failed to create socket directory: %w", err)
	}

	// Remove stale sockets for this VM
	vm.cleanupSockets()

	// Build vfkit command-line arguments
	args, err := vm.buildVfkitArgs()
	if err != nil {
		return fmt.Errorf("failed to build vfkit args: %w", err)
	}

	// Bail out early if the caller already cancelled
	if err := ctx.Err(); err != nil {
		return err
	}

	cmd := exec.Command(vm.cfg.VfkitPath, args...)

	// Redirect vfkit output to the console log instead of server stderr.
	// This captures vfkit INFO lines and tcpproxy retry noise for debugging
	// without cluttering the server log.
	consoleLogPath := filepath.Join(vm.cfg.InstanceDir, vm.meta.Name, "console.log")
	consoleLog, err := os.OpenFile(consoleLogPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return fmt.Errorf("failed to open console log: %w", err)
	}
	cmd.Stdout = consoleLog
	cmd.Stderr = consoleLog

	if err := cmd.Start(); err != nil {
		consoleLog.Close()
		return fmt.Errorf("failed to start vfkit: %w", err)
	}

	vm.cmd = cmd
	vm.meta.PID = cmd.Process.Pid

	// Reap the child process in the background to prevent zombies
	waitCh := make(chan struct{})
	vm.waitCh = waitCh
	go func(cmd *exec.Cmd, done chan struct{}, logFile *os.File) {
		err := cmd.Wait()
		logFile.Close()
		if err != nil {
			log.Printf("[%s] vfkit exited unexpectedly: %v", vm.meta.Name, err)
		}
		close(done)
	}(cmd, waitCh, consoleLog)

	// Wait for the agent to be healthy
	dialer := NewVZDialer(vm.cfg.SocketDir, vm.meta.Name)
	agent := vmutil.NewAgentClient(dialer, vm.cfg.ConsolePort, vm.cfg.NotifyPort)
	if err := agent.WaitForHealth(ctx, vm.cfg.StartTimeout.Duration()); err != nil {
		// Try to stop the VM on failure
		if stopErr := vm.Stop(context.Background()); stopErr != nil {
			log.Printf("Warning: failed to stop VM after health check failure: %v", stopErr)
		}
		return fmt.Errorf("agent health check failed: %w", err)
	}

	return nil
}

// buildVfkitArgs constructs the vfkit command-line arguments.
//
// The shed initramfs (per-image, pulled from the blob dir) builds the
// in-guest overlay (writable upper on /dev/vda + read-only lower on
// /dev/vdb) and pivot_roots into the merged tree, so the kernel cmdline
// drops the legacy `root=/dev/vda rw` and instead names the two layers
// via `shed.upper=` / `shed.lower=`.
func (vm *VM) buildVfkitArgs() (args []string, err error) {
	if vm.meta.LowerDigest == "" {
		return nil, fmt.Errorf("vm %s has no lower_digest in metadata; recreate via `shed delete && shed create`", vm.meta.Name)
	}
	if !vmimage.BlobExists(vm.cfg.ImagesDir, vm.meta.LowerDigest) {
		return nil, fmt.Errorf("manifest blob %s is not cached; pull the image (%s) before starting", vmimage.ShortDigest(vm.meta.LowerDigest), vm.meta.LowerImageTag)
	}
	manifest, kernelBlobPath, initrdBlobPath, err := vmimage.NewManager(vm.cfg, nil).ResolveImageBlobs(vm.meta.LowerDigest)
	if err != nil {
		return nil, fmt.Errorf("resolving image blobs: %w", err)
	}
	if initrdBlobPath == "" {
		return nil, fmt.Errorf("image %s has no initrd annotation; rebuild the image", vmimage.ShortDigest(vm.meta.LowerDigest))
	}
	if _, statErr := os.Stat(initrdBlobPath); statErr != nil {
		return nil, fmt.Errorf("initrd blob missing at %s: %w", initrdBlobPath, statErr)
	}

	// Prefer the kernel blob from the manifest annotation. Fall back to
	// the configured `vz.kernel_path` only when the manifest lacks an
	// io.shed.kernel.digest annotation (legacy / hand-built images).
	kernelPath := kernelBlobPath
	if kernelPath == "" {
		if vm.cfg.KernelPath == "" {
			return nil, fmt.Errorf("no kernel for %s: manifest has no kernel annotation and vz.kernel_path is unset", vm.meta.Name)
		}
		kernelPath = vm.cfg.KernelPath
	} else if _, statErr := os.Stat(kernelPath); statErr != nil {
		return nil, fmt.Errorf("kernel blob missing at %s: %w", kernelPath, statErr)
	}

	// Resolve the single flattened lower for the manifest. Upper is
	// /dev/vda (per-shed writable); lower is /dev/vdb (read-only erofs
	// shared across every shed booting from this manifest).
	lowerPath, err := vmimage.NewManager(vm.cfg, nil).ResolveManifestLower(context.Background(), vm.meta.LowerDigest)
	if err != nil {
		return nil, fmt.Errorf("resolving manifest lower: %w", err)
	}
	_ = manifest // reserved for future per-layer annotation use

	kernelArgs := fmt.Sprintf(
		"console=hvc0 init=/sbin/init shed.name=%s shed.upper=/dev/vda shed.lower=/dev/vdb",
		vm.meta.Name,
	)

	// Console log for debugging boot issues (writes guest console to a file)
	consoleLogPath := filepath.Join(vm.cfg.InstanceDir, vm.meta.Name, "console.log")

	args = []string{
		"--cpus", fmt.Sprintf("%d", vm.meta.CPUs),
		"--memory", fmt.Sprintf("%d", vm.meta.MemoryMB),
		"--kernel", kernelPath,
		"--initrd", initrdBlobPath,
		"--kernel-cmdline", kernelArgs,
		// Upper: writable, /dev/vda inside the guest.
		"--device", fmt.Sprintf("virtio-blk,path=%s", vm.meta.RootfsPath),
		// Lower: read-only flattened manifest erofs, /dev/vdb.
		"--device", fmt.Sprintf("virtio-blk,path=%s,readonly", lowerPath),
	}
	args = append(args,
		"--device", "virtio-net,nat",
		"--device", fmt.Sprintf("virtio-serial,logFilePath=%s", consoleLogPath),
	)

	// Add VirtioFS shared directories for project mounts (--local-dir / --add-dir)
	for _, m := range vm.meta.ProjectMounts {
		tag := config.ProjectMountTagForTarget(m.Target)
		args = append(args, "--device", fmt.Sprintf("virtio-fs,sharedDir=%s,mountTag=%s", m.Source, tag))
	}

	// Add VirtioFS shared directories for credential mounts
	for _, share := range vm.credentialShares {
		args = append(args, "--device", fmt.Sprintf("virtio-fs,sharedDir=%s,mountTag=%s", share.SourceDir, share.MountTag))
	}

	// Add vsock devices — one per port.
	// NOTE: SocketDir must not contain spaces. vfkit URL-encodes the socketURL
	// parameter, turning spaces into %20, which causes connection failures.
	ports := []uint32{vm.cfg.ConsolePort, vm.cfg.NotifyPort, vm.cfg.TCPProxyPort}
	for _, port := range ports {
		socketPath := filepath.Join(vm.cfg.SocketDir, fmt.Sprintf("%s-%d.sock", vm.meta.Name, port))
		args = append(args, "--device", fmt.Sprintf("virtio-vsock,port=%d,socketURL=%s,connect", port, socketPath))
	}

	return args, nil
}

// Stop stops the VM gracefully via SIGTERM, falling back to SIGKILL.
func (vm *VM) Stop(ctx context.Context) error {
	defer vm.cleanupSockets()
	defer vm.clearProcessHandles()

	if vm.cmd != nil && vm.cmd.Process != nil {
		err := vm.stopProcess(ctx, vm.cmd.Process)
		vm.waitForReap(2 * time.Second)
		return err
	}

	// Try to stop by PID if we have one
	if vm.meta.PID > 0 {
		return vm.stopByPID(ctx)
	}

	return nil
}

func (vm *VM) waitForReap(timeout time.Duration) {
	if vm.waitCh == nil {
		return
	}
	select {
	case <-vm.waitCh:
	case <-time.After(timeout):
		log.Printf("Warning: timed out waiting for vfkit process %d to reap", vm.meta.PID)
	}
}

func (vm *VM) clearProcessHandles() {
	vm.cmd = nil
	vm.waitCh = nil
}

// stopProcess stops a process by sending SIGTERM then SIGKILL.
func (vm *VM) stopProcess(ctx context.Context, process *os.Process) error {
	// Try SIGTERM first
	if err := process.Signal(syscall.SIGTERM); err != nil {
		if errors.Is(err, syscall.ESRCH) || errors.Is(err, os.ErrProcessDone) {
			return nil
		}
		return fmt.Errorf("failed to signal VM: %w", err)
	}

	timeout := vm.cfg.StopTimeout.Duration()
	deadline := time.Now().Add(timeout)
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()

	for {
		if err := process.Signal(syscall.Signal(0)); err != nil {
			// Process exited
			return nil
		}

		if time.Now().After(deadline) {
			// Timeout — force kill
			if !isVfkitProcess(vm.meta.PID) {
				log.Printf("Warning: PID %d is not a vfkit process, skipping SIGKILL", vm.meta.PID)
				return nil
			}
			if err := process.Signal(syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) && !errors.Is(err, os.ErrProcessDone) {
				return fmt.Errorf("failed to kill VM after timeout: %w", err)
			}
			if !waitForProcessExit(vm.meta.PID, 2*time.Second) {
				log.Printf("Warning: VM %s PID %d did not exit within timeout after SIGKILL", vm.meta.Name, vm.meta.PID)
			}
			log.Printf("VM %s force-killed after graceful shutdown timeout", vm.meta.Name)
			return nil
		}

		select {
		case <-ctx.Done():
			if !isVfkitProcess(vm.meta.PID) {
				log.Printf("Warning: PID %d is not a vfkit process, skipping SIGKILL", vm.meta.PID)
				return ctx.Err()
			}
			if err := process.Signal(syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) && !errors.Is(err, os.ErrProcessDone) {
				return fmt.Errorf("context canceled, failed to kill VM: %w", err)
			}
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

// stopByPID stops a VM by its PID when we don't have a process handle.
func (vm *VM) stopByPID(ctx context.Context) error {
	if vm.meta.PID <= 0 {
		return nil
	}

	process, err := os.FindProcess(vm.meta.PID)
	if err != nil {
		return nil
	}

	if !isVfkitProcess(vm.meta.PID) {
		log.Printf("Warning: PID %d is not a vfkit process, skipping signal", vm.meta.PID)
		return nil
	}

	return vm.stopProcess(ctx, process)
}

// cleanupSockets removes vsock socket files for this VM.
func (vm *VM) cleanupSockets() {
	ports := []uint32{vm.cfg.ConsolePort, vm.cfg.NotifyPort, vm.cfg.TCPProxyPort}
	for _, port := range ports {
		socketPath := filepath.Join(vm.cfg.SocketDir, fmt.Sprintf("%s-%d.sock", vm.meta.Name, port))
		os.Remove(socketPath)
	}
}

// IsRunning checks if the VM is currently running.
func (vm *VM) IsRunning() bool {
	if vm.waitCh != nil {
		select {
		case <-vm.waitCh:
			return false
		default:
		}
	}

	if vm.meta.PID <= 0 {
		return false
	}

	process, err := os.FindProcess(vm.meta.PID)
	if err != nil {
		return false
	}

	// Send signal 0 to check if process exists.
	// EPERM means the process exists but we lack permission to signal it.
	err = process.Signal(syscall.Signal(0))
	if err != nil && !errors.Is(err, syscall.EPERM) {
		return false
	}

	// Guard against PID reuse: verify the process is actually vfkit.
	return isVfkitProcess(vm.meta.PID)
}

// waitForProcessExit polls until a process exits or timeout expires.
func waitForProcessExit(pid int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) {
			return true
		}
		time.Sleep(100 * time.Millisecond)
	}
	return false
}

// isVfkitProcess checks if the given PID belongs to a vfkit process.
// macOS lacks /proc, so we use ps to check the process name.
func isVfkitProcess(pid int) bool {
	out, err := exec.Command("ps", "-p", fmt.Sprintf("%d", pid), "-o", "comm=").Output()
	if err != nil {
		return false // Process gone or not readable
	}
	comm := strings.TrimSpace(string(out))
	return strings.Contains(comm, "vfkit")
}
