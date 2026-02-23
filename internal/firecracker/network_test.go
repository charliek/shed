//go:build linux
// +build linux

package firecracker

import (
	"errors"
	"net"
	"testing"
)

func TestNewNetworkManager_Valid(t *testing.T) {
	nm, err := NewNetworkManager("fc-br0", "172.30.0.1/24", "fc-tap")
	if err != nil {
		t.Fatalf("NewNetworkManager() error = %v", err)
	}

	if nm.bridgeName != "fc-br0" {
		t.Errorf("bridgeName = %v, want fc-br0", nm.bridgeName)
	}
	if nm.tapPrefix != "fc-tap" {
		t.Errorf("tapPrefix = %v, want fc-tap", nm.tapPrefix)
	}
}

func TestNewNetworkManager_InvalidCIDR(t *testing.T) {
	_, err := NewNetworkManager("fc-br0", "invalid-cidr", "fc-tap")
	if err == nil {
		t.Error("NewNetworkManager() expected error for invalid CIDR")
	}
}

func TestNewNetworkManager_IPv6Rejected(t *testing.T) {
	_, err := NewNetworkManager("fc-br0", "fd00::1/64", "fc-tap")
	if err == nil {
		t.Error("NewNetworkManager() expected error for IPv6 CIDR")
	}
}

func TestTAPDeviceName(t *testing.T) {
	nm, err := NewNetworkManager("fc-br0", "172.30.0.1/24", "fc-tap")
	if err != nil {
		t.Fatalf("NewNetworkManager() error = %v", err)
	}

	tests := []struct {
		index int
		want  string
	}{
		{0, "fc-tap-0"},
		{1, "fc-tap-1"},
		{10, "fc-tap-10"},
		{999, "fc-tap-999"},
	}

	for _, tt := range tests {
		got := nm.TAPDeviceName(tt.index)
		if got != tt.want {
			t.Errorf("TAPDeviceName(%d) = %v, want %v", tt.index, got, tt.want)
		}
	}
}

func TestAllocateIP(t *testing.T) {
	nm, err := NewNetworkManager("fc-br0", "172.30.0.1/24", "fc-tap")
	if err != nil {
		t.Fatalf("NewNetworkManager() error = %v", err)
	}

	tests := []struct {
		name  string
		index int
		want  string
		err   bool
	}{
		{
			name:  "first IP",
			index: 0,
			want:  "172.30.0.2",
		},
		{
			name:  "second IP",
			index: 1,
			want:  "172.30.0.3",
		},
		{
			name:  "tenth IP",
			index: 9,
			want:  "172.30.0.11",
		},
		{
			name:  "near octet boundary",
			index: 252,
			want:  "172.30.0.254",
		},
		{
			name:  "cross octet boundary",
			index: 253,
			want:  "172.30.0.255",
		},
		{
			name:  "just past boundary",
			index: 254,
			err:   true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := nm.AllocateIP(tt.index)
			if tt.err {
				if err == nil {
					t.Fatalf("AllocateIP(%d) expected error, got nil", tt.index)
				}
				return
			}
			if err != nil {
				t.Fatalf("AllocateIP(%d) unexpected error: %v", tt.index, err)
			}
			if got != tt.want {
				t.Errorf("AllocateIP(%d) = %v, want %v", tt.index, got, tt.want)
			}
		})
	}
}

func TestGateway(t *testing.T) {
	tests := []struct {
		cidr string
		want string
	}{
		{"172.30.0.1/24", "172.30.0.1"},
		{"10.0.0.1/16", "10.0.0.1"},
		{"192.168.1.1/24", "192.168.1.1"},
	}

	for _, tt := range tests {
		t.Run(tt.cidr, func(t *testing.T) {
			nm, err := NewNetworkManager("fc-br0", tt.cidr, "fc-tap")
			if err != nil {
				t.Fatalf("NewNetworkManager() error = %v", err)
			}

			got := nm.Gateway()
			if got != tt.want {
				t.Errorf("Gateway() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestIPToUint32(t *testing.T) {
	tests := []struct {
		ip   string
		want uint32
	}{
		{"0.0.0.0", 0},
		{"0.0.0.1", 1},
		{"0.0.1.0", 256},
		{"0.1.0.0", 65536},
		{"1.0.0.0", 16777216},
		{"172.30.0.1", 0xAC1E0001},
		{"255.255.255.255", 0xFFFFFFFF},
	}

	for _, tt := range tests {
		t.Run(tt.ip, func(t *testing.T) {
			ip := net.ParseIP(tt.ip)
			got, err := ipToUint32(ip)
			if err != nil {
				t.Fatalf("ipToUint32(%v) unexpected error: %v", tt.ip, err)
			}
			if got != tt.want {
				t.Errorf("ipToUint32(%v) = %#x, want %#x", tt.ip, got, tt.want)
			}
		})
	}
}

func TestIPToUint32_NilIP(t *testing.T) {
	_, err := ipToUint32(nil)
	if !errors.Is(err, ErrInvalidIPv4) {
		t.Errorf("ipToUint32(nil) error = %v, want ErrInvalidIPv4", err)
	}
}

func TestIPToUint32_InvalidIPv6(t *testing.T) {
	ip := net.ParseIP("::1")
	_, err := ipToUint32(ip)
	if err == nil {
		t.Error("ipToUint32(::1) expected error for IPv6 address")
	}
	if !errors.Is(err, ErrInvalidIPv4) {
		t.Errorf("ipToUint32(::1) error = %v, want ErrInvalidIPv4", err)
	}
}

func TestUint32ToIP(t *testing.T) {
	tests := []struct {
		n    uint32
		want string
	}{
		{0, "0.0.0.0"},
		{1, "0.0.0.1"},
		{256, "0.0.1.0"},
		{65536, "0.1.0.0"},
		{16777216, "1.0.0.0"},
		{0xAC1E0001, "172.30.0.1"},
		{0xFFFFFFFF, "255.255.255.255"},
	}

	for _, tt := range tests {
		t.Run(tt.want, func(t *testing.T) {
			got := uint32ToIP(tt.n).String()
			if got != tt.want {
				t.Errorf("uint32ToIP(%#x) = %v, want %v", tt.n, got, tt.want)
			}
		})
	}
}

func TestIPRoundTrip(t *testing.T) {
	// Test that converting IP to uint32 and back gives the same IP
	ips := []string{
		"172.30.0.1",
		"10.0.0.1",
		"192.168.1.254",
		"8.8.8.8",
		"1.1.1.1",
	}

	for _, ipStr := range ips {
		t.Run(ipStr, func(t *testing.T) {
			ip := net.ParseIP(ipStr)
			n, err := ipToUint32(ip)
			if err != nil {
				t.Fatalf("ipToUint32(%v) unexpected error: %v", ipStr, err)
			}
			back := uint32ToIP(n)

			// Compare as strings since net.IP comparison can be tricky
			if back.String() != ipStr {
				t.Errorf("round trip failed: %v -> %#x -> %v", ipStr, n, back.String())
			}
		})
	}
}

func TestFindAvailableTAPIndex(t *testing.T) {
	nm, err := NewNetworkManager("fc-br0", "172.30.0.1/24", "fc-tap")
	if err != nil {
		t.Fatalf("NewNetworkManager() error = %v", err)
	}

	tests := []struct {
		name        string
		usedIndices map[int]bool
		want        int
	}{
		{
			name:        "no used indices",
			usedIndices: map[int]bool{},
			want:        0,
		},
		{
			name:        "first index used",
			usedIndices: map[int]bool{0: true},
			want:        1,
		},
		{
			name:        "gap in indices",
			usedIndices: map[int]bool{0: true, 1: true, 3: true},
			want:        2,
		},
		{
			name:        "sequential used",
			usedIndices: map[int]bool{0: true, 1: true, 2: true},
			want:        3,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := nm.FindAvailableTAPIndex(tt.usedIndices)
			if err != nil {
				t.Fatalf("FindAvailableTAPIndex() unexpected error = %v", err)
			}
			if got != tt.want {
				t.Errorf("FindAvailableTAPIndex() = %v, want %v", got, tt.want)
			}
		})
	}
}
