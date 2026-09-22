//go:build linux
// +build linux

package firecracker

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
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

// machineConfig assembles the firecracker.Config for one VM from
// already-resolved inputs. It is deliberately pure — no receiver state,
// no filesystem, no image resolution — so the ForwardSignals contract
// below is unit-testable without a real upper, image or bridge.
func machineConfig(
	socketPath string,
	kernelPath string,
	initrdPath string,
	kernelArgs string,
	drives []models.Drive,
	machineCfg models.MachineConfiguration,
	vsockDevices []firecracker.VsockDevice,
	netIfaces []firecracker.NetworkInterface,
) firecracker.Config {
	return firecracker.Config{
		SocketPath:        socketPath,
		KernelImagePath:   kernelPath,
		InitrdPath:        initrdPath,
		KernelArgs:        kernelArgs,
		Drives:            drives,
		MachineCfg:        machineCfg,
		VsockDevices:      vsockDevices,
		NetworkInterfaces: netIfaces,
		// #372: an EMPTY, NON-NIL slice is the off switch for the SDK's
		// signal relay, and only the empty slice. A nil ForwardSignals
		// makes NewMachine substitute the SDK default set
		// (INT/QUIT/TERM/HUP/ABRT — machine.go:394-401), and
		// setupSignals then forwards each of those from shed-server
		// straight to this VM's firecracker child; a `systemctl stop
		// shed-server` would take every running shed down with it.
		// setupSignals returns without installing any handler when
		// len(signals) == 0 (machine.go:1110-1114), so the empty slice
		// leaves the VMM entirely unsignalled by the server's own exit.
		// Paired with KillMode=process in packaging/shed-server.service
		// (systemd would otherwise reap the children itself); the
		// surviving VMMs are re-attached by the startup resume walk
		// (#315). VZ never had the problem — vfkit is a plain
		// exec.Command with no signal forwarding.
		ForwardSignals: []os.Signal{},
	}
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
	socketPath := vm.apiSocketPath()

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

	fcCfg := machineConfig(
		socketPath,
		kernelPath,
		initrdPath,
		kernelArgs,
		drives,
		models.MachineConfiguration{
			VcpuCount:  firecracker.Int64(int64(vm.meta.CPUs)),
			MemSizeMib: firecracker.Int64(int64(vm.meta.MemoryMB)),
		},
		[]firecracker.VsockDevice{
			{
				Path: filepath.Join(socketDir, fmt.Sprintf("%s.vsock", vm.meta.Name)),
				CID:  uint32(vm.meta.CID),
			},
		},
		[]firecracker.NetworkInterface{
			{
				StaticConfiguration: &firecracker.StaticNetworkConfiguration{
					MacAddress:  generateMACAddress(vm.meta.CID),
					HostDevName: vm.meta.TAPDevice,
				},
			},
		},
	)

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
		// Graceful shutdown timed out — force kill. The handle is taken
		// BEFORE the identity check so the SIGKILL lands on the process we
		// inspected, not on whatever holds the number by then.
		proc, owns, err := vm.recordedPIDHandle()
		if err != nil {
			return fmt.Errorf("cannot verify pid %d before force-killing %s: %w", vm.meta.PID, vm.meta.Name, err)
		}
		if !owns {
			log.Printf("Warning: PID %d is not %s's firecracker VMM (recycled pid?), skipping SIGKILL", vm.meta.PID, vm.meta.Name)
			if shutdownErr != nil {
				return shutdownErr
			}
			return waitErr
		}
		if err := proc.Signal(syscall.SIGKILL); err != nil && !processGone(err) {
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
	// and the server-restart case (no live machine handle). The pid is pinned
	// before the identity check, and signalled through that same handle.
	proc, owns, err := vm.recordedPIDHandle()
	if err != nil {
		return fmt.Errorf("cannot verify pid %d before killing %s: %w", vm.meta.PID, vm.meta.Name, err)
	}
	if owns {
		if err := proc.Signal(syscall.SIGKILL); err != nil && !processGone(err) {
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

// apiSocketPath is this VM's firecracker API socket — the per-shed path its
// VMM carries in argv, and therefore this VM's identity for pid checks.
func (vm *VM) apiSocketPath() string {
	return instanceAPISocketPath(vm.cfg.SocketDir, vm.meta.Name)
}

// ownsRecordedPID reports whether meta.PID is still THIS shed's firecracker
// VMM. Guards every path that signals a recorded pid: after a shed-server
// restart that pid came off disk with no SDK handle behind it (the normal
// case since #372), so it may have been recycled — possibly by another
// shed's VMM, which a family-only check would happily kill.
//
// A non-nil error is the UNKNOWN case — see isThisVMsProcess. It is never
// "not ours": callers must not tear anything down on it.
func (vm *VM) ownsRecordedPID() (bool, error) {
	return isThisVMsProcess(vm.meta.PID, vm.apiSocketPath())
}

// recordedPIDHandle pins meta.PID with os.FindProcess and THEN checks whose
// process it is. The order matters: on Linux the returned *os.Process carries
// a pidfd (os/exec_unix.go:findProcess → pidfdFind), and Signal on a pidfd
// handle goes to that exact process — so a pid recycled between the check and
// the signal can no longer be hit. Signalling by number (syscall.Kill) after a
// separate /proc read is exactly the race this replaces.
//
// Returns (nil, false, nil) when there is no usable pid, and a non-nil error
// only for the UNKNOWN identity case, which callers must not treat as "not
// ours".
func (vm *VM) recordedPIDHandle() (*os.Process, bool, error) {
	if vm.meta.PID <= 0 {
		return nil, false, nil
	}
	proc, err := os.FindProcess(vm.meta.PID)
	if err != nil {
		// Unix never reports "no such process" here; anything else means we
		// have no handle to signal through.
		return nil, false, nil
	}
	owns, err := vm.ownsRecordedPID()
	if err != nil {
		return nil, false, err
	}
	return proc, owns, nil
}

// processGone reports whether a signal error means "the target is already
// gone" — ESRCH for a pid-number signal, os.ErrProcessDone for a pidfd one.
func processGone(err error) bool {
	return errors.Is(err, syscall.ESRCH) || errors.Is(err, os.ErrProcessDone)
}

// cleanupSockets removes the API and vsock socket files for this VM.
func (vm *VM) cleanupSockets() {
	socketDir := vm.cfg.SocketDir
	// Remove API socket
	os.Remove(vm.apiSocketPath())
	// Remove vsock socket
	vsockSocket := filepath.Join(socketDir, fmt.Sprintf("%s.vsock", vm.meta.Name))
	os.Remove(vsockSocket)
}

// stopByPID stops a VM by its PID when we don't have a machine handle.
func (vm *VM) stopByPID(ctx context.Context) error {
	if vm.meta.PID <= 0 {
		return nil
	}

	// Pin the process FIRST (pidfd-backed on Linux), then establish whose it
	// is: every signal below goes through this handle, so it can never land
	// on a pid recycled after the check.
	process, owns, err := vm.recordedPIDHandle()
	if err != nil {
		return fmt.Errorf("cannot verify pid %d before signalling %s: %w", vm.meta.PID, vm.meta.Name, err)
	}
	if process == nil {
		return nil // Process doesn't exist
	}

	// Try SIGTERM first
	if !owns {
		log.Printf("Warning: PID %d is not %s's firecracker VMM (recycled pid?), skipping signal", vm.meta.PID, vm.meta.Name)
		return nil
	}
	if err := process.Signal(syscall.SIGTERM); err != nil {
		if processGone(err) {
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
			if processGone(err) {
				return nil
			}
			if errors.Is(err, syscall.EPERM) {
				continue
			}
			return fmt.Errorf("failed to check VM process: %w", err)
		}

		if time.Now().After(deadline) {
			owns, err := vm.ownsRecordedPID()
			if err != nil {
				return fmt.Errorf("cannot verify pid %d before force-killing %s: %w", vm.meta.PID, vm.meta.Name, err)
			}
			if !owns {
				log.Printf("Warning: PID %d is not %s's firecracker VMM (recycled pid?), skipping SIGKILL", vm.meta.PID, vm.meta.Name)
			} else if err := process.Signal(syscall.SIGKILL); err != nil && !processGone(err) {
				return fmt.Errorf("failed to kill VM after timeout: %w", err)
			}
			return nil
		}

		select {
		case <-ctx.Done():
			owns, err := vm.ownsRecordedPID()
			if err != nil {
				return fmt.Errorf("cannot verify pid %d before force-killing %s: %w", vm.meta.PID, vm.meta.Name, err)
			}
			if !owns {
				log.Printf("Warning: PID %d is not %s's firecracker VMM (recycled pid?), skipping SIGKILL", vm.meta.PID, vm.meta.Name)
			} else if err := process.Signal(syscall.SIGKILL); err != nil && !processGone(err) {
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

// IsRunning reports whether this shed's VMM is live.
//
// The error is the UNKNOWN case: the pid answers signal 0 but its identity
// could not be established (an unreadable /proc entry, EACCES under a
// hardened kernel). Callers must NOT read that as "stopped" — rewriting
// metadata to Stopped/PID=0 under a live VMM strips its TAP/CID/upper
// reservations and invites a second firecracker under the same name.
func (vm *VM) IsRunning() (bool, error) {
	if vm.meta.PID <= 0 {
		return false, nil
	}

	// Check if process exists
	process, err := os.FindProcess(vm.meta.PID)
	if err != nil {
		return false, nil
	}

	// Send signal 0 to check if process exists.
	// EPERM means the process exists but we lack permission to signal it.
	err = process.Signal(syscall.Signal(0))
	if err != nil && !errors.Is(err, syscall.EPERM) {
		return false, nil
	}

	// Guard against PID reuse: verify the process is actually THIS shed's
	// firecracker. Matches VZ's vfkit check in vz/vm.go:IsRunning. Without
	// it, a recycled PID (host reboot + churn) could keep shed reporting
	// status=running indefinitely.
	return vm.ownsRecordedPID()
}

// instanceAPISocketPath is the `--api-sock` path VM.Start hands the SDK for
// one shed. It is per-shed, which is what makes it a usable IDENTITY for a
// recorded pid — the family ("is this some firecracker?") is not.
func instanceAPISocketPath(socketDir, name string) string {
	return filepath.Join(socketDir, fmt.Sprintf("%s.sock", name))
}

// vmmCmdlineServesInstance reports whether a NUL-separated /proc cmdline is a
// firecracker VMM launched with exactly THIS instance's api-sock argument.
//
// Two independent pieces of evidence, deliberately:
//
//   - argv[0] is the firecracker binary. The SDK is given no custom
//     VMCommandBuilder (see machineConfig's caller), so it execs its default
//     `firecracker` bin. Matching argv[0] rather than the whole command line
//     matters because the default socket dir is /var/run/shed/firecracker
//     (config/server.go), so a whole-line Contains("firecracker") is satisfied
//     by the socket path itself and corroborates nothing.
//   - an exact `--api-sock <this shed's sock>` argv pair. The SDK's
//     VMCommandBuilder emits the flag and its value as two adjacent argv
//     elements (command_builder.go:SocketPath/Build), so the pair is matched
//     positionally instead of by substring — another shed's VMM, or a process
//     that merely mentions the path, can't satisfy it.
//
// Together they answer "is pid still the VMM I started for THIS shed?", which
// is the only safe question to ask before signalling a recorded pid: after a
// server restart the pid came off disk and the OS may have recycled it (#372
// makes surviving-VMM-without-an-SDK-handle the normal path).
func vmmCmdlineServesInstance(cmdline, apiSockPath string) bool {
	if apiSockPath == "" {
		return false
	}
	argv := strings.Split(cmdline, "\x00")
	if len(argv) == 0 || filepath.Base(argv[0]) != "firecracker" {
		return false
	}
	for i := 0; i+1 < len(argv); i++ {
		if argv[i] == "--api-sock" && argv[i+1] == apiSockPath {
			return true
		}
	}
	return false
}

// readPIDCmdline reads a pid's /proc command line. A package-level seam so
// the UNKNOWN branch below (a live pid whose /proc entry can't be read) is
// reachable in tests without a hostile /proc; never reassigned at runtime.
var readPIDCmdline = func(pid int) ([]byte, error) {
	return os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
}

// isThisVMsProcess reports whether pid is the live firecracker VMM serving the
// shed whose api-sock is apiSockPath. Every pid-fallback path (Stop,
// Kill/delete, the zombie and stop-incomplete guards, IsRunning) goes through
// it — a family-only check would let a recycled pid make `shed delete A`
// SIGKILL shed B's VMM.
//
// TRI-STATE, deliberately:
//
//	(true,  nil) — pid is this shed's VMM.
//	(false, nil) — definitively NOT this shed's VMM: the process is gone
//	               (ENOENT/ESRCH) or its command line is a clean mismatch.
//	(false, err) — UNKNOWN: the command line could not be read for some other
//	               reason (EACCES under a hardened /proc, EIO, a transient
//	               failure). NOT the same as "not ours".
//
// Collapsing UNKNOWN into false is what makes this dangerous: GetShed would
// rewrite a live shed to Stopped/PID=0, CheckNotRunning would allow a second
// firecracker under the same name, and stop/delete would release the TAP, the
// CID and the upper out from under a running VM. Callers must handle err.
func isThisVMsProcess(pid int, apiSockPath string) (bool, error) {
	if pid <= 0 {
		return false, nil
	}
	data, err := readPIDCmdline(pid)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ESRCH) {
			return false, nil // process gone ⇒ definitively not ours
		}
		return false, fmt.Errorf("reading cmdline for pid %d: %w", pid, err)
	}
	return vmmCmdlineServesInstance(string(data), apiSockPath), nil
}

// generateMACAddress generates a MAC address based on the CID.
func generateMACAddress(cid uint32) string {
	// Use a locally administered MAC address (second hex digit is 2, 6, A, or E)
	// Format: 02:FC:00:00:XX:XX where XX:XX is derived from CID
	return fmt.Sprintf("02:FC:00:00:%02X:%02X", (cid>>8)&0xFF, cid&0xFF)
}
