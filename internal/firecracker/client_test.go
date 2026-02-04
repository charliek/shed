package firecracker

import (
	"testing"
)

func TestAllocateCID(t *testing.T) {
	dir := mustTempDir(t, "client-test")
	cfg := testFirecrackerConfig(dir)

	client := &Client{
		cfg:      cfg,
		vms:      make(map[string]*VM),
		usedCIDs: make(map[uint32]string),
		usedIPs:  make(map[string]string),
	}

	tests := []struct {
		name     string
		usedCIDs map[uint32]string
		want     uint32
	}{
		{
			name:     "no used CIDs",
			usedCIDs: map[uint32]string{},
			want:     100, // VsockBaseCID
		},
		{
			name:     "first CID used",
			usedCIDs: map[uint32]string{100: "vm-1"},
			want:     101,
		},
		{
			name:     "gap in CIDs",
			usedCIDs: map[uint32]string{100: "vm-1", 101: "vm-2", 103: "vm-3"},
			want:     102,
		},
		{
			name:     "sequential CIDs",
			usedCIDs: map[uint32]string{100: "vm-1", 101: "vm-2", 102: "vm-3"},
			want:     103,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client.usedCIDs = tt.usedCIDs
			got := client.AllocateCID()
			if got != tt.want {
				t.Errorf("AllocateCID() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestRegisterUnregisterInstance(t *testing.T) {
	dir := mustTempDir(t, "client-test")
	cfg := testFirecrackerConfig(dir)

	client := &Client{
		cfg:      cfg,
		vms:      make(map[string]*VM),
		usedCIDs: make(map[uint32]string),
		usedIPs:  make(map[string]string),
	}

	// Register an instance
	client.RegisterInstance("test-vm", 100, "172.30.0.2")

	// Verify it's registered
	if client.usedCIDs[100] != "test-vm" {
		t.Error("CID not registered")
	}
	if client.usedIPs["172.30.0.2"] != "test-vm" {
		t.Error("IP not registered")
	}

	// Unregister
	client.UnregisterInstance("test-vm", 100, "172.30.0.2")

	// Verify it's gone
	if _, exists := client.usedCIDs[100]; exists {
		t.Error("CID still registered after unregister")
	}
	if _, exists := client.usedIPs["172.30.0.2"]; exists {
		t.Error("IP still registered after unregister")
	}
}

func TestAllocateNetwork(t *testing.T) {
	dir := mustTempDir(t, "client-test")
	cfg := testFirecrackerConfig(dir)

	netMgr, err := NewNetworkManager(cfg.BridgeName, cfg.BridgeCIDR, cfg.TAPPrefix)
	if err != nil {
		t.Fatalf("NewNetworkManager() error = %v", err)
	}

	client := &Client{
		cfg:      cfg,
		netMgr:   netMgr,
		vms:      make(map[string]*VM),
		usedCIDs: make(map[uint32]string),
		usedIPs:  make(map[string]string),
	}

	// Allocate first network
	tap1, ip1, err := client.AllocateNetwork("vm-1")
	if err != nil {
		t.Fatalf("AllocateNetwork() error = %v", err)
	}

	if tap1 != "fc-tap-0" {
		t.Errorf("first tap = %v, want fc-tap-0", tap1)
	}
	if ip1 != "172.30.0.2" {
		t.Errorf("first ip = %v, want 172.30.0.2", ip1)
	}

	// Register and allocate second
	client.usedIPs[ip1] = "vm-1"

	tap2, ip2, err := client.AllocateNetwork("vm-2")
	if err != nil {
		t.Fatalf("AllocateNetwork() error = %v", err)
	}

	if tap2 != "fc-tap-1" {
		t.Errorf("second tap = %v, want fc-tap-1", tap2)
	}
	if ip2 != "172.30.0.3" {
		t.Errorf("second ip = %v, want 172.30.0.3", ip2)
	}
}
