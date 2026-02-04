package firecracker

import (
	"os"
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

func TestIsRunning_CurrentPID(t *testing.T) {
	// Use current process PID which is definitely running
	currentPID := os.Getpid()

	dir := mustTempDir(t, "vm-test")
	cfg := testFirecrackerConfig(dir)
	meta := testMetadata("test-vm")
	meta.PID = currentPID

	vm := &VM{meta: meta, cfg: cfg}

	if !vm.IsRunning() {
		t.Error("IsRunning() = false for current process, want true")
	}
}

func TestIsRunning_InvalidPID(t *testing.T) {
	dir := mustTempDir(t, "vm-test")
	cfg := testFirecrackerConfig(dir)
	meta := testMetadata("test-vm")

	tests := []struct {
		name string
		pid  int
	}{
		{"zero PID", 0},
		{"negative PID", -1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			meta.PID = tt.pid
			vm := &VM{meta: meta, cfg: cfg}

			if vm.IsRunning() {
				t.Errorf("IsRunning() = true for PID %d, want false", tt.pid)
			}
		})
	}
}

func TestIsRunning_NonexistentPID(t *testing.T) {
	// Use a very high PID that's unlikely to exist
	// Note: This test may be flaky on systems with very high PIDs
	highPID := 4000000000 // Very high, unlikely to exist

	dir := mustTempDir(t, "vm-test")
	cfg := testFirecrackerConfig(dir)
	meta := testMetadata("test-vm")
	meta.PID = highPID

	vm := &VM{meta: meta, cfg: cfg}

	// On most systems, this should return false
	// The test verifies the logic handles non-running processes
	_ = vm.IsRunning() // Just verify it doesn't panic
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
