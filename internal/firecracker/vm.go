//go:build linux
// +build linux

package firecracker

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	firecracker "github.com/firecracker-microvm/firecracker-go-sdk"
	"github.com/firecracker-microvm/firecracker-go-sdk/client/models"
	"github.com/sirupsen/logrus"

	"github.com/charliek/shed/internal/config"
	"github.com/charliek/shed/internal/vmimage"
	"github.com/charliek/shed/internal/vmutil"
)

// VM represents a running Firecracker VM instance.
type VM struct {
	meta    *Metadata
	cfg     *config.FirecrackerConfig
	netMgr  *NetworkManager
	machine *firecracker.Machine
}

// CreateVM creates a new VM instance (but does not start it).
func CreateVM(ctx context.Context, meta *Metadata, cfg *config.FirecrackerConfig, netMgr *NetworkManager) (*VM, error) {
	return &VM{
		meta:   meta,
		cfg:    cfg,
		netMgr: netMgr,
	}, nil
}

// Start starts the VM.
func (vm *VM) Start(ctx context.Context) error {
	// Guard against a previously interrupted ResetShed (DeleteUpper
	// succeeded, EnsureUpper then failed): without this, vm.Start
	// would fail deep inside the firecracker SDK with a generic
	// "open failed" on the rootfs drive. Surfacing a clean recovery
	// hint here saves the operator from digging through SDK logs.
	if _, err := os.Stat(vm.meta.RootfsPath); err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("shed %s has no writable upper at %s; run `shed reset %s` to recreate it (or `shed delete %s` to abandon)", vm.meta.Name, vm.meta.RootfsPath, vm.meta.Name, vm.meta.Name)
		}
		return fmt.Errorf("stat upper at %s: %w", vm.meta.RootfsPath, err)
	}

	// Ensure socket directory exists
	socketDir := vm.cfg.SocketDir
	if err := os.MkdirAll(socketDir, 0755); err != nil {
		return fmt.Errorf("failed to create socket directory: %w", err)
	}

	// Socket path for this VM
	socketPath := filepath.Join(socketDir, fmt.Sprintf("%s.sock", vm.meta.Name))

	// Remove old socket if it exists
	os.Remove(socketPath)

	// Remove old vsock socket if it exists (prevents "Address in use" on restart)
	os.Remove(filepath.Join(socketDir, fmt.Sprintf("%s.vsock", vm.meta.Name)))

	// Build firecracker configuration
	// Kernel args include:
	// - IP configuration in kernel autoconf format: ip=<client>:<server>:<gw>:<netmask>:<hostname>:<device>:<autoconf>
	// - cgroup_enable=memory for Docker cgroup support
	// The "off" at the end disables DHCP/BOOTP which can block boot on some kernels
	_, ipNet, err := net.ParseCIDR(vm.cfg.BridgeCIDR)
	if err != nil {
		return fmt.Errorf("invalid bridge CIDR %q: %w", vm.cfg.BridgeCIDR, err)
	}
	netmask := fmt.Sprintf("%d.%d.%d.%d", ipNet.Mask[0], ipNet.Mask[1], ipNet.Mask[2], ipNet.Mask[3])
	// shed.name= is read by the in-guest shed-firstboot service to set the
	// hostname and detect rootfs clones (snapshot spawns). Shed names are
	// validated by config.ValidateShedName so direct concatenation is safe.
	//
	if vm.meta.LowerDigest == "" {
		return fmt.Errorf("vm %s has no lower_digest in metadata; recreate via `shed delete && shed create`", vm.meta.Name)
	}
	if !vmimage.BlobExists(vm.cfg.ImagesDir, vm.meta.LowerDigest) {
		return fmt.Errorf("manifest blob %s is not cached; pull the image (%s) before starting", vmimage.ShortDigest(vm.meta.LowerDigest), vm.meta.LowerImageTag)
	}
	imageMgr := vmimage.NewManager(vm.cfg, nil)
	_, kernelBlob, initrdBlob, err := imageMgr.ResolveImageBlobs(vm.meta.LowerDigest)
	if err != nil {
		return fmt.Errorf("resolving image blobs: %w", err)
	}
	if initrdBlob == "" {
		return fmt.Errorf("image %s has no initrd annotation; rebuild the image", vmimage.ShortDigest(vm.meta.LowerDigest))
	}
	if _, err := os.Stat(initrdBlob); err != nil {
		return fmt.Errorf("initrd blob missing at %s: %w", initrdBlob, err)
	}
	initrdPath := initrdBlob

	// Prefer the kernel blob from the manifest annotation. Fall back to
	// the configured `firecracker.kernel_path` only when the manifest
	// lacks an io.shed.kernel.digest annotation.
	kernelPath := kernelBlob
	if kernelPath == "" {
		if vm.cfg.KernelPath == "" {
			return fmt.Errorf("no kernel for %s: manifest has no kernel annotation and firecracker.kernel_path is unset", vm.meta.Name)
		}
		kernelPath = vm.cfg.KernelPath
	} else if _, err := os.Stat(kernelPath); err != nil {
		return fmt.Errorf("kernel blob missing at %s: %w", kernelPath, err)
	}

	// Resolve the single flattened lower for the manifest. Upper is
	// /dev/vda (per-shed writable); lower is /dev/vdb (read-only erofs
	// shared across every shed booting from this manifest).
	lowerPath, err := imageMgr.ResolveManifestLower(ctx, vm.meta.LowerDigest)
	if err != nil {
		return fmt.Errorf("resolving manifest lower: %w", err)
	}

	kernelArgs := fmt.Sprintf(
		"console=ttyS0 reboot=k panic=1 pci=off init=/sbin/init ip=%s::%s:%s::eth0:off cgroup_enable=memory cgroup_memory=1 shed.name=%s shed.upper=/dev/vda shed.lower=/dev/vdb",
		vm.meta.IPAddress, vm.netMgr.Gateway(), netmask, vm.meta.Name,
	)

	// Pass the resolved guest MTU so network-setup lowers eth0 to match a
	// reduced host egress path (e.g. a VPN/overlay on the FC host). Omitted
	// entirely when detection finds no reduction and no override is set.
	if mtu, ok := vmutil.ResolveGuestMTU(vm.cfg.GuestMTU); ok {
		kernelArgs += fmt.Sprintf(" shed.mtu=%d", mtu)
	}

	drives := []models.Drive{
		// Upper (writable). The initramfs runs mkfs.ext4 on first
		// boot when no FS signature is present.
		{
			DriveID:      firecracker.String("rootfs"),
			PathOnHost:   firecracker.String(vm.meta.RootfsPath),
			IsRootDevice: firecracker.Bool(true),
			IsReadOnly:   firecracker.Bool(false),
		},
		// Lower: read-only flattened manifest erofs, /dev/vdb.
		{
			DriveID:      firecracker.String("lower"),
			PathOnHost:   firecracker.String(lowerPath),
			IsRootDevice: firecracker.Bool(false),
			IsReadOnly:   firecracker.Bool(true),
		},
	}

	fcCfg := firecracker.Config{
		SocketPath:      socketPath,
		KernelImagePath: kernelPath,
		InitrdPath:      initrdPath,
		KernelArgs:      kernelArgs,
		Drives:          drives,
		MachineCfg: models.MachineConfiguration{
			VcpuCount:  firecracker.Int64(int64(vm.meta.CPUs)),
			MemSizeMib: firecracker.Int64(int64(vm.meta.MemoryMB)),
		},
		VsockDevices: []firecracker.VsockDevice{
			{
				Path: filepath.Join(socketDir, fmt.Sprintf("%s.vsock", vm.meta.Name)),
				CID:  uint32(vm.meta.CID),
			},
		},
		NetworkInterfaces: []firecracker.NetworkInterface{
			{
				StaticConfiguration: &firecracker.StaticNetworkConfiguration{
					MacAddress:  generateMACAddress(vm.meta.CID),
					HostDevName: vm.meta.TAPDevice,
				},
			},
		},
	}

	// Use background context for the VM lifecycle so it persists beyond
	// the HTTP request that created it. NewMachine and machine.Start both
	// store this context for ongoing Firecracker API communication, so a
	// request-scoped context would incorrectly cancel the VM when the
	// request completes. Caller-supplied cancellation is still respected
	// via the health-check below which uses ctx with StartTimeout.
	vmCtx := context.Background()

	// Bail out early if the caller already cancelled before we do the
	// (relatively expensive) machine creation.
	if err := ctx.Err(); err != nil {
		return err
	}

	logger := logrus.New()
	logger.SetOutput(os.Stderr)
	logger.SetLevel(logrus.InfoLevel)
	machineOpts := []firecracker.Opt{
		firecracker.WithLogger(logrus.NewEntry(logger)),
	}

	machine, err := firecracker.NewMachine(vmCtx, fcCfg, machineOpts...)
	if err != nil {
		return fmt.Errorf("failed to create firecracker machine: %w", err)
	}

	vm.machine = machine

	// Start the machine
	if err := machine.Start(vmCtx); err != nil {
		vm.cleanupSockets() // Clean up .sock and .vsock files on failure
		return fmt.Errorf("failed to start firecracker machine: %w", err)
	}

	// Get the PID from the running machine
	pid, err := machine.PID()
	if err != nil {
		log.Printf("Warning: failed to get Firecracker PID: %v", err)
	} else {
		vm.meta.PID = pid
	}

	// Wait for the agent to be healthy
	vsockPath := filepath.Join(socketDir, fmt.Sprintf("%s.vsock", vm.meta.Name))
	dialer := NewFirecrackerDialer(vsockPath)
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

// Stop stops the VM gracefully.
func (vm *VM) Stop(ctx context.Context) error {
	// Clean up socket files
	defer vm.cleanupSockets()

	if vm.machine == nil {
		// Try to stop by PID if we have one
		if vm.meta.PID > 0 {
			return vm.stopByPID(ctx)
		}
		return nil
	}

	// Try graceful shutdown via API first
	shutdownCtx, cancel := context.WithTimeout(ctx, vm.cfg.StopTimeout.Duration())
	defer cancel()

	shutdownErr := vm.machine.Shutdown(shutdownCtx)
	if shutdownErr != nil {
		log.Printf("Graceful shutdown failed: %v, forcing stop", shutdownErr)
	}

	// Wait for the machine to stop
	waitErr := vm.machine.Wait(shutdownCtx)
	if waitErr != nil && vm.meta.PID > 0 {
		// Graceful shutdown timed out — force kill
		if !isFirecrackerProcess(vm.meta.PID) {
			log.Printf("Warning: PID %d is not a Firecracker process, skipping SIGKILL", vm.meta.PID)
			if shutdownErr != nil {
				return shutdownErr
			}
			return waitErr
		} else if err := syscall.Kill(vm.meta.PID, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
			return fmt.Errorf("failed to kill VM after shutdown timeout: %w", err)
		}
		if !waitForProcessExit(vm.meta.PID, 2*time.Second) {
			log.Printf("Warning: VM %s PID %d did not exit within timeout after SIGKILL", vm.meta.Name, vm.meta.PID)
		}
		log.Printf("VM %s force-killed after graceful shutdown timeout", vm.meta.Name)
		return nil
	}
	if waitErr != nil && vm.meta.PID <= 0 {
		log.Printf("Warning: VM %s wait failed but no PID available for force-kill", vm.meta.Name)
	}

	// Return shutdown error if the API call itself failed (not timeout)
	if shutdownErr != nil {
		return shutdownErr
	}
	return waitErr
}

// Kill terminates the VM immediately with SIGKILL, skipping BOTH graceful
// sub-paths that Stop uses (the firecracker `machine.Shutdown` when a machine
// handle is live, and the `stopByPID` SIGTERM-then-wait when it isn't — the
// server-restart case). This is the destroy/delete path: the writable upper is
// discarded, so a clean guest shutdown is pointless and the ~stop_timeout wait
// is pure latency. It mirrors Stop's proven force-kill fallback (SIGKILL +
// waitForProcessExit + socket cleanup), so it leaves no more behind than a
// graceful stop whose guest ignored the shutdown.
func (vm *VM) Kill(_ context.Context) error {
	defer vm.cleanupSockets()

	// SIGKILL by PID when we have a usable one — covers both the in-process case
	// and the server-restart case (no live machine handle).
	if vm.meta.PID > 0 && isFirecrackerProcess(vm.meta.PID) {
		if err := syscall.Kill(vm.meta.PID, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
			return fmt.Errorf("failed to SIGKILL VM %s (pid %d): %w", vm.meta.Name, vm.meta.PID, err)
		}
		if !waitForProcessExit(vm.meta.PID, 2*time.Second) {
			log.Printf("Warning: VM %s PID %d did not exit within timeout after SIGKILL", vm.meta.Name, vm.meta.PID)
		}
		return nil
	}

	// No usable PID. machine.PID() can fail at Start (logged, non-fatal),
	// leaving meta.PID unset even though the VMM is live — graceful Stop would
	// still drive it via the SDK handle, so Kill must too, or a delete could
	// orphan a running firecracker while removing its metadata/upper. StopVMM
	// signals the firecracker process and waits for cleanup.
	if vm.machine != nil {
		if err := vm.machine.StopVMM(); err != nil {
			return fmt.Errorf("failed to StopVMM %s: %w", vm.meta.Name, err)
		}
	}
	return nil
}

// cleanupSockets removes the API and vsock socket files for this VM.
func (vm *VM) cleanupSockets() {
	socketDir := vm.cfg.SocketDir
	// Remove API socket
	apiSocket := filepath.Join(socketDir, fmt.Sprintf("%s.sock", vm.meta.Name))
	os.Remove(apiSocket)
	// Remove vsock socket
	vsockSocket := filepath.Join(socketDir, fmt.Sprintf("%s.vsock", vm.meta.Name))
	os.Remove(vsockSocket)
}

// stopByPID stops a VM by its PID when we don't have a machine handle.
func (vm *VM) stopByPID(ctx context.Context) error {
	if vm.meta.PID <= 0 {
		return nil
	}

	// Check if process exists
	process, err := os.FindProcess(vm.meta.PID)
	if err != nil {
		return nil // Process doesn't exist
	}

	// Try SIGTERM first
	if !isFirecrackerProcess(vm.meta.PID) {
		log.Printf("Warning: PID %d is not a Firecracker process, skipping signal", vm.meta.PID)
		return nil
	}
	if err := process.Signal(syscall.SIGTERM); err != nil {
		if errors.Is(err, syscall.ESRCH) {
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
			if errors.Is(err, syscall.ESRCH) {
				return nil
			}
			if errors.Is(err, syscall.EPERM) {
				continue
			}
			return fmt.Errorf("failed to check VM process: %w", err)
		}

		if time.Now().After(deadline) {
			if !isFirecrackerProcess(vm.meta.PID) {
				log.Printf("Warning: PID %d is not a Firecracker process, skipping SIGKILL", vm.meta.PID)
			} else if err := process.Signal(syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
				return fmt.Errorf("failed to kill VM after timeout: %w", err)
			}
			return nil
		}

		select {
		case <-ctx.Done():
			if !isFirecrackerProcess(vm.meta.PID) {
				log.Printf("Warning: PID %d is not a Firecracker process, skipping SIGKILL", vm.meta.PID)
			} else if err := process.Signal(syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
				return fmt.Errorf("context canceled, failed to kill VM: %w", err)
			}
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

// waitForProcessExit polls until a process exits or timeout expires.
// Returns true if the process exited, false if the timeout was reached.
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

// IsRunning checks if the VM is currently running.
func (vm *VM) IsRunning() bool {
	if vm.meta.PID <= 0 {
		return false
	}

	// Check if process exists
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

	// Guard against PID reuse: verify the process is actually
	// firecracker. Matches VZ's vfkit check in vz/vm.go:IsRunning.
	// Without this, a recycled PID (host reboot + churn) could keep
	// shed reporting status=running indefinitely.
	return isFirecrackerProcess(vm.meta.PID)
}

// isFirecrackerProcess checks if the given PID belongs to a Firecracker process
// by reading /proc/<pid>/cmdline. Returns false if the process doesn't exist
// or doesn't look like a Firecracker process (indicating PID reuse).
func isFirecrackerProcess(pid int) bool {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
	if err != nil {
		return false // Process gone or not readable
	}
	return strings.Contains(string(data), "firecracker")
}

// generateMACAddress generates a MAC address based on the CID.
func generateMACAddress(cid uint32) string {
	// Use a locally administered MAC address (second hex digit is 2, 6, A, or E)
	// Format: 02:FC:00:00:XX:XX where XX:XX is derived from CID
	return fmt.Sprintf("02:FC:00:00:%02X:%02X", (cid>>8)&0xFF, cid&0xFF)
}
