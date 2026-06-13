package vmutil

import (
	"log"
	"net"
	"strings"

	"github.com/charliek/shed/internal/config"
)

// mtuDetectTarget is the destination used to discover which host interface
// (and therefore which MTU) egress traffic to the public internet would take.
// No packet is ever sent — Dial on a UDP "connection" only triggers a kernel
// route lookup so a local address is bound. Centralized as a single constant so
// a future `mtu_detection_target` config knob (some corporate networks
// special-route or block Cloudflare) is a one-line change.
const mtuDetectTarget = "1.1.1.1:80"

// ResolveGuestMTU decides the MTU to hand the guest's primary interface at VM
// start. It returns (mtu, true) when a value should be applied via the
// `shed.mtu=` kernel cmdline, or (0, false) to leave the guest at its default
// (1500) and emit no cmdline arg.
//
// Order of precedence:
//  1. An explicit override (already validated to [MinGuestMTU, MaxGuestMTU] by
//     config) wins — detection is skipped.
//  2. Otherwise auto-detect the host egress path MTU and apply it only when it
//     is below 1500 (i.e. a reduced-MTU path such as a VPN/overlay).
//  3. On any detection failure, or when the path is already >= 1500, return
//     (0, false): identical to today's behavior, so there is no regression.
//
// The decision path is logged because VPN/MTU failures are otherwise hard to
// diagnose from the outside.
func ResolveGuestMTU(override int) (int, bool) {
	return resolveGuestMTU(override, DetectEgressMTU)
}

// resolveGuestMTU is the testable core of ResolveGuestMTU with the detector
// injected.
func resolveGuestMTU(override int, detect func() (int, bool)) (int, bool) {
	if override > 0 {
		log.Printf("guest MTU: using configured override guest_mtu=%d", override)
		return override, true
	}
	detected, ok := detect()
	if !ok {
		log.Printf("guest MTU: host egress MTU not detected; leaving guest at default (%d)", config.MaxGuestMTU)
		return 0, false
	}
	if detected >= config.MaxGuestMTU {
		log.Printf("guest MTU: host egress MTU=%d (>= %d); leaving guest at default", detected, config.MaxGuestMTU)
		return 0, false
	}
	clamped := ClampGuestMTU(detected)
	log.Printf("guest MTU: detected reduced host egress MTU=%d; lowering guest to %d", detected, clamped)
	return clamped, true
}

// ClampGuestMTU clamps an auto-detected MTU into the supportable
// [MinGuestMTU, MaxGuestMTU] range. The floor guards against an absurdly small
// tunnel; the ceiling guards against ever raising the guest above the host
// vmnet/TAP MTU.
func ClampGuestMTU(mtu int) int {
	return max(config.MinGuestMTU, min(config.MaxGuestMTU, mtu))
}

// ifaceInfo is the address/MTU view of one host interface. It exists so the
// address-matching logic (egressMTUForAddr) is pure and unit-testable: the
// real net.Interface.Addrs() is a syscall that can't be faked, so DetectEgressMTU
// snapshots it into these structs first.
type ifaceInfo struct {
	name  string
	mtu   int
	addrs []net.IP
}

// DetectEgressMTU reports the MTU of the host interface that egress traffic to
// the public internet would use, and whether detection succeeded. It opens a
// connected — but never written-to — UDP socket to learn the kernel-selected
// local address, then maps that address back to its interface. Read-only: no
// packet is sent.
func DetectEgressMTU() (int, bool) {
	conn, err := net.Dial("udp", mtuDetectTarget)
	if err != nil {
		return 0, false
	}
	defer conn.Close()

	udpAddr, ok := conn.LocalAddr().(*net.UDPAddr)
	if !ok || udpAddr == nil {
		return 0, false
	}

	ifaces, err := net.Interfaces()
	if err != nil {
		return 0, false
	}

	infos := make([]ifaceInfo, 0, len(ifaces))
	for _, iface := range ifaces {
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		ips := make([]net.IP, 0, len(addrs))
		for _, a := range addrs {
			switch v := a.(type) {
			case *net.IPNet:
				ips = append(ips, v.IP)
			case *net.IPAddr:
				ips = append(ips, v.IP)
			}
		}
		infos = append(infos, ifaceInfo{name: iface.Name, mtu: iface.MTU, addrs: ips})
	}

	return egressMTUForAddr(udpAddr.IP, infos)
}

// egressMTUForAddr finds the interface that owns localIP and returns its MTU.
// Pure (no syscalls) for testability. Returns (0, false) when localIP is
// loopback/unspecified, when it belongs only to a shed-managed bridge/tap, when
// no interface owns it, or when the owning interface reports a non-positive MTU.
func egressMTUForAddr(localIP net.IP, ifaces []ifaceInfo) (int, bool) {
	if localIP == nil || localIP.IsLoopback() || localIP.IsUnspecified() {
		return 0, false
	}
	for _, iface := range ifaces {
		if isShedManagedIface(iface.name) {
			continue
		}
		for _, ip := range iface.addrs {
			if ip != nil && ip.Equal(localIP) {
				if iface.mtu <= 0 {
					return 0, false
				}
				return iface.mtu, true
			}
		}
	}
	return 0, false
}

// isShedManagedIface reports whether name is one of shed's own VM-facing
// devices (the Firecracker bridge `shed-br0` and `shed-tap*` devices). Internet
// egress never routes through these, so matching one means the host routing
// table is unusual — skip it rather than read its bridge MTU.
func isShedManagedIface(name string) bool {
	return strings.HasPrefix(name, "shed-")
}
