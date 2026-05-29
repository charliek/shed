//go:build linux
// +build linux

package firecracker

import (
	"os/exec"
	"testing"
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
		name       string
		setupPID   func(t *testing.T) int
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

			if got := vm.IsRunning(); got != tc.wantRunning {
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
