package vmutil

import (
	"net"
	"testing"

	"github.com/charliek/shed/internal/config"
)

func TestClampGuestMTU(t *testing.T) {
	tests := []struct {
		name string
		in   int
		want int
	}{
		{"below floor", 900, config.MinGuestMTU},
		{"at floor", config.MinGuestMTU, config.MinGuestMTU},
		{"in range", 1400, 1400},
		{"at ceiling", config.MaxGuestMTU, config.MaxGuestMTU},
		{"above ceiling", 9000, config.MaxGuestMTU},
		{"zero", 0, config.MinGuestMTU},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ClampGuestMTU(tt.in); got != tt.want {
				t.Errorf("ClampGuestMTU(%d) = %d, want %d", tt.in, got, tt.want)
			}
		})
	}
}

func TestResolveGuestMTU(t *testing.T) {
	detect := func(mtu int, ok bool) func() (int, bool) {
		return func() (int, bool) { return mtu, ok }
	}
	tests := []struct {
		name     string
		override int
		detect   func() (int, bool)
		wantMTU  int
		wantOK   bool
	}{
		{"override wins, skips detection", 1400, detect(0, false), 1400, true},
		{"override below 1500 trusted as-is", 1300, detect(1450, true), 1300, true},
		{"detect reduced -> clamped", 0, detect(1400, true), 1400, true},
		{"detect tiny -> clamped to floor", 0, detect(900, true), config.MinGuestMTU, true},
		{"detect 1500 -> none", 0, detect(1500, true), 0, false},
		{"detect above 1500 -> none", 0, detect(9000, true), 0, false},
		{"detection fails -> none", 0, detect(0, false), 0, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotMTU, gotOK := resolveGuestMTU(tt.override, tt.detect)
			if gotMTU != tt.wantMTU || gotOK != tt.wantOK {
				t.Errorf("resolveGuestMTU(%d) = (%d, %v), want (%d, %v)",
					tt.override, gotMTU, gotOK, tt.wantMTU, tt.wantOK)
			}
		})
	}
}

func TestEgressMTUForAddr(t *testing.T) {
	ip := func(s string) net.IP { return net.ParseIP(s) }
	ifaces := []ifaceInfo{
		{name: "lo0", mtu: 16384, addrs: []net.IP{ip("127.0.0.1")}},
		{name: "en0", mtu: 1500, addrs: []net.IP{ip("192.168.1.20"), ip("fe80::1")}},
		{name: "utun4", mtu: 1400, addrs: []net.IP{ip("10.8.0.2")}},
		{name: "shed-br0", mtu: 1500, addrs: []net.IP{ip("172.30.0.1")}},
	}

	tests := []struct {
		name    string
		local   net.IP
		wantMTU int
		wantOK  bool
	}{
		{"matches en0 (no VPN)", ip("192.168.1.20"), 1500, true},
		{"matches utun4 (VPN)", ip("10.8.0.2"), 1400, true},
		{"matches en0 v6 alias", ip("fe80::1"), 1500, true},
		{"loopback rejected", ip("127.0.0.1"), 0, false},
		{"unspecified rejected", ip("0.0.0.0"), 0, false},
		{"nil rejected", nil, 0, false},
		{"shed-managed iface skipped", ip("172.30.0.1"), 0, false},
		{"no owning interface", ip("203.0.113.5"), 0, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotMTU, gotOK := egressMTUForAddr(tt.local, ifaces)
			if gotMTU != tt.wantMTU || gotOK != tt.wantOK {
				t.Errorf("egressMTUForAddr(%v) = (%d, %v), want (%d, %v)",
					tt.local, gotMTU, gotOK, tt.wantMTU, tt.wantOK)
			}
		})
	}
}

func TestEgressMTUForAddrNonPositiveMTU(t *testing.T) {
	ifaces := []ifaceInfo{{name: "weird0", mtu: 0, addrs: []net.IP{net.ParseIP("192.0.2.7")}}}
	if mtu, ok := egressMTUForAddr(net.ParseIP("192.0.2.7"), ifaces); ok || mtu != 0 {
		t.Errorf("egressMTUForAddr with mtu=0 = (%d, %v), want (0, false)", mtu, ok)
	}
}
