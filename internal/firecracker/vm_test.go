//go:build linux
// +build linux

package firecracker

import (
	"io/fs"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"

	firecracker "github.com/firecracker-microvm/firecracker-go-sdk"
	"github.com/firecracker-microvm/firecracker-go-sdk/client/models"
)

func TestGenerateMACAddress(t *testing.T) {
	tests := []struct {
		name string
		cid  uint32
		want string
	}{
		{
			name: "CID 100",
			cid:  100,
			want: "02:FC:00:00:00:64",
		},
		{
			name: "CID 256",
			cid:  256,
			want: "02:FC:00:00:01:00",
		},
		{
			name: "CID 65535",
			cid:  65535,
			want: "02:FC:00:00:FF:FF",
		},
		{
			name: "CID 0",
			cid:  0,
			want: "02:FC:00:00:00:00",
		},
		{
			name: "CID 1000",
			cid:  1000,
			want: "02:FC:00:00:03:E8",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := generateMACAddress(tt.cid)
			if got != tt.want {
				t.Errorf("generateMACAddress(%d) = %v, want %v", tt.cid, got, tt.want)
			}
		})
	}
}

func TestIsRunning(t *testing.T) {
	// spawnSleepChild returns a non-firecracker live pid the caller
	// owns for the duration of the case (cleanup kills + reaps it).
	// os.Getpid() can't substitute here: the test binary's
	// /proc/PID/cmdline ends in `firecracker.test` and substring-
	// matches "firecracker", which would defeat the PID-reuse guard
	// we're trying to test.
	spawnSleepChild := func(t *testing.T) int {
		t.Helper()
		cmd := exec.Command("sleep", "30")
		if err := cmd.Start(); err != nil {
			t.Fatalf("start sleep child: %v", err)
		}
		t.Cleanup(func() {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		})
		return cmd.Process.Pid
	}

	cases := []struct {
		name        string
		setupPID    func(t *testing.T) int
		wantRunning bool
	}{
		{
			name:        "zero PID",
			setupPID:    func(*testing.T) int { return 0 },
			wantRunning: false,
		},
		{
			name:        "negative PID",
			setupPID:    func(*testing.T) int { return -1 },
			wantRunning: false,
		},
		{
			// Tightens the contract added by the PID-reuse guard: a
			// live PID that isn't firecracker must report not-running.
			// Before the guard, this returned true and `shed list`
			// would silently advertise a recycled pid as a running VMM.
			name:        "live PID but not firecracker",
			setupPID:    spawnSleepChild,
			wantRunning: false,
		},
		{
			// 2,000,000,000 is above default Linux pid_max (4,194,304)
			// but well within int32 — kernels return ESRCH.
			name:        "impossibly-large PID",
			setupPID:    func(*testing.T) int { return 2000000000 },
			wantRunning: false,
		},
	}

	dir := mustTempDir(t, "vm-test")
	cfg := testFirecrackerConfig(dir)

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			meta := testMetadata("test-vm")
			meta.PID = tc.setupPID(t)
			vm := &VM{meta: meta, cfg: cfg}

			got, err := vm.IsRunning()
			if err != nil {
				t.Fatalf("IsRunning() error = %v, want nil (a readable pid is never UNKNOWN)", err)
			}
			if got != tc.wantRunning {
				t.Errorf("IsRunning() = %v, want %v (pid=%d)", got, tc.wantRunning, meta.PID)
			}
		})
	}
}

func TestMACAddressFormat(t *testing.T) {
	// Verify MAC addresses are locally administered (bit 1 of first byte set)
	for cid := uint32(0); cid < 1000; cid += 100 {
		mac := generateMACAddress(cid)

		// MAC should start with 02: (locally administered)
		if mac[:2] != "02" {
			t.Errorf("generateMACAddress(%d) = %v, first octet should be 02", cid, mac)
		}

		// MAC should have proper format (17 chars: XX:XX:XX:XX:XX:XX)
		if len(mac) != 17 {
			t.Errorf("generateMACAddress(%d) = %v, length = %d, want 17", cid, mac, len(mac))
		}
	}
}

// sampleMachineConfig feeds machineConfig one fully-formed set of
// inputs, mirroring the shape (*VM).Start passes it.
func sampleMachineConfig() firecracker.Config {
	return machineConfig(
		"/run/shed/firecracker/test-vm.sock",
		"/var/lib/shed/blobs/sha256/kernel",
		"/var/lib/shed/blobs/sha256/initrd",
		"console=ttyS0 shed.name=test-vm shed.upper=/dev/vda shed.lower=/dev/vdb",
		[]models.Drive{
			{
				DriveID:      firecracker.String("rootfs"),
				PathOnHost:   firecracker.String("/var/lib/shed/firecracker/test-vm/rootfs.ext4"),
				IsRootDevice: firecracker.Bool(true),
				IsReadOnly:   firecracker.Bool(false),
			},
			{
				DriveID:      firecracker.String("lower"),
				PathOnHost:   firecracker.String("/var/lib/shed/blobs/sha256/lower.erofs"),
				IsRootDevice: firecracker.Bool(false),
				IsReadOnly:   firecracker.Bool(true),
			},
		},
		models.MachineConfiguration{
			VcpuCount:  firecracker.Int64(2),
			MemSizeMib: firecracker.Int64(2048),
		},
		[]firecracker.VsockDevice{
			{Path: "/run/shed/firecracker/test-vm.vsock", CID: 123},
		},
		[]firecracker.NetworkInterface{
			{
				StaticConfiguration: &firecracker.StaticNetworkConfiguration{
					MacAddress:  generateMACAddress(123),
					HostDevName: "shed-tap0",
				},
			},
		},
	)
}

// #372: stopping shed-server used to kill every running shed's VM. The
// SDK substitutes its default signal set (INT/QUIT/TERM/HUP/ABRT) when
// ForwardSignals is nil and only skips the handler when the slice is
// empty, so "empty, non-nil" is the whole fix — a nil here silently
// restores the relay.
func TestMachineConfigDisablesSignalForwarding(t *testing.T) {
	cfg := sampleMachineConfig()

	if cfg.ForwardSignals == nil {
		t.Fatal("ForwardSignals is nil; the SDK fills a nil with its default INT/QUIT/TERM/HUP/ABRT set and relays every shed-server signal to the VMM (#372). It must be an EMPTY, NON-NIL slice")
	}
	if len(cfg.ForwardSignals) != 0 {
		t.Errorf("ForwardSignals = %v, want an empty slice; any entry is forwarded to the firecracker child", cfg.ForwardSignals)
	}
}

// Proves the extraction of the config block out of (*VM).Start is
// lossless: every field Start used to set inline still arrives.
func TestMachineConfigPassesThroughInputs(t *testing.T) {
	cfg := sampleMachineConfig()

	tests := []struct {
		name string
		got  any
		want any
	}{
		{"socket path", cfg.SocketPath, "/run/shed/firecracker/test-vm.sock"},
		{"kernel image path", cfg.KernelImagePath, "/var/lib/shed/blobs/sha256/kernel"},
		{"initrd path", cfg.InitrdPath, "/var/lib/shed/blobs/sha256/initrd"},
		{"kernel args", cfg.KernelArgs, "console=ttyS0 shed.name=test-vm shed.upper=/dev/vda shed.lower=/dev/vdb"},
		{"drive count", len(cfg.Drives), 2},
		{"vsock device count", len(cfg.VsockDevices), 1},
		{"network interface count", len(cfg.NetworkInterfaces), 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.got != tt.want {
				t.Errorf("%s = %v, want %v", tt.name, tt.got, tt.want)
			}
		})
	}

	t.Run("upper drive", func(t *testing.T) {
		if len(cfg.Drives) == 0 {
			t.Fatal("no drives")
		}
		d := cfg.Drives[0]
		if got := firecracker.StringValue(d.DriveID); got != "rootfs" {
			t.Errorf("Drives[0].DriveID = %q, want %q", got, "rootfs")
		}
		if got := firecracker.StringValue(d.PathOnHost); got != "/var/lib/shed/firecracker/test-vm/rootfs.ext4" {
			t.Errorf("Drives[0].PathOnHost = %q, want the upper path", got)
		}
		if !firecracker.BoolValue(d.IsRootDevice) || firecracker.BoolValue(d.IsReadOnly) {
			t.Errorf("Drives[0] root=%v readonly=%v, want root=true readonly=false", firecracker.BoolValue(d.IsRootDevice), firecracker.BoolValue(d.IsReadOnly))
		}
	})

	t.Run("machine cfg", func(t *testing.T) {
		if got := firecracker.Int64Value(cfg.MachineCfg.VcpuCount); got != 2 {
			t.Errorf("MachineCfg.VcpuCount = %d, want 2", got)
		}
		if got := firecracker.Int64Value(cfg.MachineCfg.MemSizeMib); got != 2048 {
			t.Errorf("MachineCfg.MemSizeMib = %d, want 2048", got)
		}
	})

	t.Run("vsock device", func(t *testing.T) {
		if cfg.VsockDevices[0].CID != 123 {
			t.Errorf("VsockDevices[0].CID = %d, want 123", cfg.VsockDevices[0].CID)
		}
		if cfg.VsockDevices[0].Path != "/run/shed/firecracker/test-vm.vsock" {
			t.Errorf("VsockDevices[0].Path = %q, want the .vsock path", cfg.VsockDevices[0].Path)
		}
	})

	t.Run("network interface", func(t *testing.T) {
		sc := cfg.NetworkInterfaces[0].StaticConfiguration
		if sc == nil {
			t.Fatal("NetworkInterfaces[0].StaticConfiguration is nil")
		}
		if sc.HostDevName != "shed-tap0" {
			t.Errorf("HostDevName = %q, want %q", sc.HostDevName, "shed-tap0")
		}
		if sc.MacAddress != generateMACAddress(123) {
			t.Errorf("MacAddress = %q, want %q", sc.MacAddress, generateMACAddress(123))
		}
	})
}

// #372 follow-up: after a server restart every stop/delete path signals a pid
// that came off disk with no SDK handle behind it, so the pid check is the
// only thing standing between `shed delete A` and shed B's VMM if the OS
// recycled A's pid. The check must therefore be INSTANCE-specific (this
// shed's `--api-sock` argument), not family-specific ("some firecracker").
func TestVMMCmdlineServesInstance(t *testing.T) {
	const sockDir = "/var/run/shed/firecracker"
	ourSock := instanceAPISocketPath(sockDir, "alpha")

	cmdline := func(argv ...string) string { return strings.Join(argv, "\x00") }

	tests := []struct {
		name    string
		cmdline string
		want    bool
	}{
		{
			name:    "this VM's VMM",
			cmdline: cmdline("/usr/bin/firecracker", "--api-sock", ourSock, "--id", "alpha"),
			want:    true,
		},
		{
			// The regression this predicate exists for: same binary,
			// same flag, another shed. A family-only check kills it.
			name:    "another shed's firecracker",
			cmdline: cmdline("/usr/bin/firecracker", "--api-sock", instanceAPISocketPath(sockDir, "beta"), "--id", "beta"),
			want:    false,
		},
		{
			// The old check was strings.Contains(cmdline, "firecracker"),
			// which this row satisfies twice over.
			name:    "unrelated process whose argv mentions firecracker",
			cmdline: cmdline("/usr/bin/grep", "firecracker", "/var/log/shed-server.log"),
			want:    false,
		},
		{
			// The socket dir itself is /var/run/shed/firecracker, so a
			// whole-line match on either token is worthless: the path has
			// to appear as the VALUE of --api-sock on a firecracker argv[0].
			name:    "non-VMM process holding our socket",
			cmdline: cmdline("/usr/bin/socat", "-", "UNIX-CONNECT:"+ourSock),
			want:    false,
		},
		{
			// F6 control row: this one carries the EXACT `--api-sock
			// <ours>` pair, so only the argv[0] basename check can reject
			// it. Without this row the basename check could be deleted and
			// every other negative row would still pass.
			name:    "non-VMM argv[0] with our exact api-sock pair",
			cmdline: cmdline("/usr/bin/socat", "--api-sock", ourSock),
			want:    false,
		},
		{
			name:    "our sock present but not as the api-sock value",
			cmdline: cmdline("/usr/bin/firecracker", "--log-path", ourSock, "--api-sock", instanceAPISocketPath(sockDir, "beta")),
			want:    false,
		},
		{
			name:    "api-sock flag with no value",
			cmdline: cmdline("/usr/bin/firecracker", "--api-sock"),
			want:    false,
		},
		{
			name:    "empty cmdline",
			cmdline: "",
			want:    false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := vmmCmdlineServesInstance(tt.cmdline, ourSock); got != tt.want {
				t.Errorf("vmmCmdlineServesInstance(%q, %q) = %v, want %v", tt.cmdline, ourSock, got, tt.want)
			}
		})
	}

	t.Run("empty api-sock path never matches", func(t *testing.T) {
		if vmmCmdlineServesInstance(cmdline("/usr/bin/firecracker", "--api-sock", ""), "") {
			t.Error("an empty api-sock path must never identify a VM")
		}
	})
}

// isThisVMsProcess is the /proc-reading wrapper; the table above covers the
// matching, so these cases only pin the pid edges (no real VMM needed).
func TestIsThisVMsProcess(t *testing.T) {
	sock := instanceAPISocketPath("/var/run/shed/firecracker", "alpha")

	tests := []struct {
		name string
		pid  int
	}{
		{"zero pid", 0},
		{"negative pid", -1},
		// Above default Linux pid_max: the /proc read fails ⇒ not ours.
		{"pid gone", 2000000000},
		// Live, readable, and its argv contains the substring
		// "firecracker" (the test binary is firecracker.test) — the old
		// family check said true for exactly this shape.
		{"this test binary", os.Getpid()},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			owns, err := isThisVMsProcess(tt.pid, sock)
			if err != nil {
				t.Fatalf("isThisVMsProcess(%d, %q) error = %v, want nil (a gone or readable pid is never UNKNOWN)", tt.pid, sock, err)
			}
			if owns {
				t.Errorf("isThisVMsProcess(%d, %q) = true, want false", tt.pid, sock)
			}
		})
	}
}

// swapPIDCmdlineReader points the pid-identity predicate at a fake /proc for
// the duration of one test.
func swapPIDCmdlineReader(t *testing.T, read func(pid int) ([]byte, error)) {
	t.Helper()
	prev := readPIDCmdline
	readPIDCmdline = read
	t.Cleanup(func() { readPIDCmdline = prev })
}

// mustSpawnLiveChild returns a live, signal-0-answerable pid the caller owns
// for the duration of the test.
func mustSpawnLiveChild(t *testing.T) int {
	t.Helper()
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start sleep child: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	return cmd.Process.Pid
}

// The tri-state contract: an unreadable-but-live /proc entry is UNKNOWN, not
// "not ours". Collapsing the two is what lets a live shed be rewritten to
// Stopped, double-spawned, or stripped of its TAP/CID/upper.
func TestIsThisVMsProcessTriState(t *testing.T) {
	sock := instanceAPISocketPath("/var/run/shed/firecracker", "alpha")

	tests := []struct {
		name     string
		read     func(pid int) ([]byte, error)
		wantOwns bool
		wantErr  bool
	}{
		{
			name:     "ours",
			read:     func(int) ([]byte, error) { return []byte("/usr/bin/firecracker\x00--api-sock\x00" + sock), nil },
			wantOwns: true,
		},
		{
			name:     "clean mismatch is definitive",
			read:     func(int) ([]byte, error) { return []byte("/usr/bin/sleep\x0030"), nil },
			wantOwns: false,
		},
		{
			// The process is gone: that IS a definitive "not ours".
			name:     "ENOENT is definitive",
			read:     func(int) ([]byte, error) { return nil, fs.ErrNotExist },
			wantOwns: false,
		},
		{
			name:     "ESRCH is definitive",
			read:     func(int) ([]byte, error) { return nil, syscall.ESRCH },
			wantOwns: false,
		},
		{
			// A hardened /proc, an EIO, a transient failure: we do not know.
			name:     "EACCES is UNKNOWN",
			read:     func(int) ([]byte, error) { return nil, fs.ErrPermission },
			wantOwns: false,
			wantErr:  true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			swapPIDCmdlineReader(t, tt.read)
			owns, err := isThisVMsProcess(4242, sock)
			if owns != tt.wantOwns {
				t.Errorf("owns = %v, want %v", owns, tt.wantOwns)
			}
			if (err != nil) != tt.wantErr {
				t.Errorf("err = %v, want error: %v", err, tt.wantErr)
			}
		})
	}
}

// IsRunning surfaces UNKNOWN rather than reporting a live-but-unverifiable
// VMM as stopped — the value GetShed and CheckNotRunning branch on.
func TestIsRunningReportsUnknownIdentity(t *testing.T) {
	dir := mustTempDir(t, "vm-unknown")
	meta := testMetadata("alpha")
	meta.PID = mustSpawnLiveChild(t)
	vm := &VM{meta: meta, cfg: testFirecrackerConfig(dir)}

	swapPIDCmdlineReader(t, func(int) ([]byte, error) { return nil, fs.ErrPermission })

	running, err := vm.IsRunning()
	if err == nil {
		t.Fatal("IsRunning() returned no error for a live pid whose identity is unreadable; UNKNOWN must not read as stopped")
	}
	if running {
		t.Errorf("IsRunning() = true on UNKNOWN, want false alongside the error")
	}
}
