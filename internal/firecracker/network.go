//go:build linux
// +build linux

package firecracker

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"os/exec"
	"syscall"
	"time"

	"github.com/charliek/shed/internal/retry"
	"github.com/vishvananda/netlink"
)

// NetworkManager handles TAP device creation and IP allocation.
type NetworkManager struct {
	bridgeName string
	bridgeCIDR string
	tapPrefix  string
	gateway    net.IP
	network    *net.IPNet

	// ipInUse reports whether an IP is already claimed on the host by
	// something outside shed's own bookkeeping (a host interface holding
	// it, or a live neighbor on the bridge). It is a passive, best-effort
	// check used to skip a colliding index during allocation. Defaults to
	// ipInUseNetlink; overridable in tests.
	ipInUse func(string) bool
}

// NewNetworkManager creates a new NetworkManager.
func NewNetworkManager(bridgeName, bridgeCIDR, tapPrefix string) (*NetworkManager, error) {
	ip, network, err := net.ParseCIDR(bridgeCIDR)
	if err != nil {
		return nil, fmt.Errorf("invalid bridge CIDR %q: %w", bridgeCIDR, err)
	}
	if ip.To4() == nil {
		return nil, fmt.Errorf("bridge CIDR %q must be IPv4", bridgeCIDR)
	}

	nm := &NetworkManager{
		bridgeName: bridgeName,
		bridgeCIDR: bridgeCIDR,
		tapPrefix:  tapPrefix,
		gateway:    ip,
		network:    network,
	}
	nm.ipInUse = nm.ipInUseNetlink
	return nm, nil
}

// TAPDeviceName returns the TAP device name for an instance index.
func (nm *NetworkManager) TAPDeviceName(index int) string {
	return fmt.Sprintf("%s-%d", nm.tapPrefix, index)
}

// AllocateIP returns the IP address for an instance index.
// Index 0 is reserved for the gateway, so instances start at index 1.
func (nm *NetworkManager) AllocateIP(index int) (string, error) {
	// Start from gateway IP and add the index + 1
	// e.g., gateway is 172.30.0.1, index 0 gets 172.30.0.2
	ip := make(net.IP, len(nm.gateway))
	copy(ip, nm.gateway)

	// Convert IP to uint32 for proper arithmetic across octets
	ipInt, err := ipToUint32(ip)
	if err != nil {
		return "", fmt.Errorf("invalid gateway IP: %w", err)
	}
	ipInt += uint32(index + 1)
	allocated := uint32ToIP(ipInt)
	if nm.network != nil && !nm.network.Contains(allocated) {
		return "", fmt.Errorf("allocated IP %s is outside of subnet %s", allocated.String(), nm.network.String())
	}

	return allocated.String(), nil
}

// ErrInvalidIPv4 is returned when an IP address is not a valid IPv4 address.
var ErrInvalidIPv4 = errors.New("not a valid IPv4 address")

// ipToUint32 converts an IPv4 address to a uint32.
func ipToUint32(ip net.IP) (uint32, error) {
	ip = ip.To4()
	if ip == nil {
		return 0, ErrInvalidIPv4
	}
	return uint32(ip[0])<<24 | uint32(ip[1])<<16 | uint32(ip[2])<<8 | uint32(ip[3]), nil
}

// uint32ToIP converts a uint32 to an IPv4 address.
func uint32ToIP(n uint32) net.IP {
	return net.IPv4(byte(n>>24), byte(n>>16), byte(n>>8), byte(n))
}

// Gateway returns the gateway IP address.
func (nm *NetworkManager) Gateway() string {
	return nm.gateway.String()
}

// tapRetryBackoffs bounds the retry of TAP setup on transient netlink
// failures (EAGAIN/EBUSY/EINTR). Two short retries — netlink against a
// local, pre-created bridge rarely stutters, so this adds latency only on
// the rare transient and never on the happy path.
var tapRetryBackoffs = []time.Duration{50 * time.Millisecond, 150 * time.Millisecond}

// CreateTAPDevice creates a TAP device and attaches it to the bridge,
// retrying the whole sequence on transient netlink errors.
func (nm *NetworkManager) CreateTAPDevice(name string) error {
	// context.Background: the retry waits are tiny and bounded
	// (tapRetryBackoffs), so honoring a caller cancel mid-setup buys
	// nothing, and keeping the signature stable avoids churning every
	// caller for a sub-second robustness retry.
	attempt := 0
	return retry.Do(context.Background(), "create-tap "+name, tapRetryBackoffs, isRetryableNetlink, func() error {
		attempt++
		// Clean up a leftover device only on a RETRY — it can only be one
		// our own previous attempt created. Pre-deleting on the first
		// attempt would clobber a same-name TAP owned by someone else
		// (e.g. a peer shed-server on a shared bridge) that LinkAdd should
		// instead surface as an EEXIST error.
		return nm.createTAPDeviceOnce(name, attempt > 1)
	})
}

// createTAPDeviceOnce performs a single TAP create + attach + up attempt.
// On any failure it best-effort-removes the partially-created device so a
// retry (or the caller) starts from a clean slate.
func (nm *NetworkManager) createTAPDeviceOnce(name string, cleanLeftover bool) error {
	// On a retry, remove a leftover device from our own previous failed
	// attempt (whose rollback LinkDel may itself have failed) so LinkAdd
	// doesn't fail with EEXIST. Never on the first attempt: a pre-existing
	// device there means another process owns the name, which LinkAdd
	// should surface as an error rather than have us delete it.
	if cleanLeftover && nm.TAPExists(name) {
		if err := nm.DeleteTAPDevice(name); err != nil {
			log.Printf("Warning: failed to remove leftover TAP %s before retry: %v", name, err)
		}
	}

	tap := &netlink.Tuntap{
		LinkAttrs: netlink.LinkAttrs{Name: name},
		Mode:      netlink.TUNTAP_MODE_TAP,
	}

	if err := netlink.LinkAdd(tap); err != nil {
		return fmt.Errorf("failed to create TAP device %s: %w", name, err)
	}

	// cleanup removes the device created above on any later failure and —
	// unlike the previous code — surfaces a cleanup failure instead of
	// silently dropping it (a leaked TAP otherwise has no recorded owner).
	cleanup := func() {
		if delErr := netlink.LinkDel(tap); delErr != nil {
			log.Printf("Warning: failed to clean up TAP %s after setup failure: %v", name, delErr)
		}
	}

	// Get the bridge
	bridge, err := netlink.LinkByName(nm.bridgeName)
	if err != nil {
		cleanup()
		return fmt.Errorf("failed to find bridge %s: %w", nm.bridgeName, err)
	}

	// Get the TAP link (need to re-fetch after creation)
	tapLink, err := netlink.LinkByName(name)
	if err != nil {
		cleanup()
		return fmt.Errorf("failed to find created TAP device %s: %w", name, err)
	}

	// Attach TAP to bridge
	if err := netlink.LinkSetMaster(tapLink, bridge); err != nil {
		cleanup()
		return fmt.Errorf("failed to attach TAP %s to bridge %s: %w", name, nm.bridgeName, err)
	}

	// Bring up TAP device
	if err := netlink.LinkSetUp(tapLink); err != nil {
		cleanup()
		return fmt.Errorf("failed to bring up TAP device %s: %w", name, err)
	}

	return nil
}

// isRetryableNetlink reports whether a netlink error is a transient kernel
// condition worth retrying (resource temporarily unavailable / busy /
// interrupted). Permanent errors (bad args, ENODEV, EEXIST) are not retried.
func isRetryableNetlink(err error) bool {
	if err == nil {
		return false
	}
	var errno syscall.Errno
	if errors.As(err, &errno) {
		return errno == syscall.EAGAIN || errno == syscall.EBUSY || errno == syscall.EINTR
	}
	return false
}

// DeleteTAPDevice removes a TAP device.
func (nm *NetworkManager) DeleteTAPDevice(name string) error {
	link, err := netlink.LinkByName(name)
	if err != nil {
		// Device doesn't exist, nothing to do
		var lnfErr netlink.LinkNotFoundError
		if errors.As(err, &lnfErr) {
			return nil
		}
		return fmt.Errorf("failed to find TAP device %s: %w", name, err)
	}

	if err := netlink.LinkDel(link); err != nil {
		return fmt.Errorf("failed to delete TAP device %s: %w", name, err)
	}

	return nil
}

// TAPExists checks if a TAP device exists.
func (nm *NetworkManager) TAPExists(name string) bool {
	_, err := netlink.LinkByName(name)
	return err == nil
}

// ipInUseNetlink is the default conflict check for an AllocateIP result. It
// is passive and best-effort: it never sends an ARP probe (which would add
// latency to the create hot path), so it cannot detect a silent host that
// has never been ARP'd and isn't a host interface. It catches the realistic
// cases — an IP already assigned to a host interface, or a live neighbor on
// the bridge — and fails OPEN (any netlink error returns false) so an
// inability to check never blocks allocation.
func (nm *NetworkManager) ipInUseNetlink(ip string) bool {
	target := net.ParseIP(ip).To4()
	if target == nil {
		return false
	}

	// (1) Already assigned to some host interface?
	if addrs, err := netlink.AddrList(nil, netlink.FAMILY_V4); err == nil {
		for _, a := range addrs {
			if a.IPNet != nil && a.IPNet.IP.Equal(target) {
				return true
			}
		}
	}

	// (2) A live neighbor on the bridge? neighStateInUse filters out
	// entries that aren't evidence of an actually-claimed address
	// (FAILED/INCOMPLETE/NONE, and STALE — which can linger for an IP we
	// ourselves just freed).
	if bridge, err := netlink.LinkByName(nm.bridgeName); err == nil {
		if neighs, err := netlink.NeighList(bridge.Attrs().Index, netlink.FAMILY_V4); err == nil {
			for _, n := range neighs {
				if n.IP != nil && n.IP.Equal(target) && neighStateInUse(n.State) {
					return true
				}
			}
		}
	}

	return false
}

// neighStateInUse reports whether a neighbor-cache state is evidence that an
// IP is actually claimed. STALE is excluded deliberately: a stale entry can
// linger for an IP a just-deleted shed freed, and treating it as in-use
// would make the allocator skip an address its own bookkeeping knows is free.
func neighStateInUse(state int) bool {
	switch state {
	case netlink.NUD_NONE, netlink.NUD_INCOMPLETE, netlink.NUD_FAILED, netlink.NUD_STALE:
		return false
	default:
		return true
	}
}

// EnsureBridgeExists checks if the bridge exists and creates it if needed.
// This is mainly for testing; in production, the bridge should be pre-created.
func (nm *NetworkManager) EnsureBridgeExists() error {
	// Check if bridge exists
	_, err := netlink.LinkByName(nm.bridgeName)
	if err == nil {
		return nil // Bridge exists
	}

	// Create bridge
	bridge := &netlink.Bridge{
		LinkAttrs: netlink.LinkAttrs{
			Name: nm.bridgeName,
		},
	}

	if err := netlink.LinkAdd(bridge); err != nil {
		return fmt.Errorf("failed to create bridge %s: %w", nm.bridgeName, err)
	}

	// Assign IP address
	addr, err := netlink.ParseAddr(nm.bridgeCIDR)
	if err != nil {
		if delErr := netlink.LinkDel(bridge); delErr != nil {
			log.Printf("Warning: failed to delete bridge %s: %v", nm.bridgeName, delErr)
		}
		return fmt.Errorf("failed to parse bridge address: %w", err)
	}

	bridgeLink, err := netlink.LinkByName(nm.bridgeName)
	if err != nil {
		if delErr := netlink.LinkDel(bridge); delErr != nil {
			log.Printf("Warning: failed to delete bridge %s: %v", nm.bridgeName, delErr)
		}
		return fmt.Errorf("failed to find created bridge: %w", err)
	}

	if err := netlink.AddrAdd(bridgeLink, addr); err != nil {
		if delErr := netlink.LinkDel(bridge); delErr != nil {
			log.Printf("Warning: failed to delete bridge %s: %v", nm.bridgeName, delErr)
		}
		return fmt.Errorf("failed to assign address to bridge: %w", err)
	}

	// Bring up bridge
	if err := netlink.LinkSetUp(bridgeLink); err != nil {
		if delErr := netlink.LinkDel(bridge); delErr != nil {
			log.Printf("Warning: failed to delete bridge %s: %v", nm.bridgeName, delErr)
		}
		return fmt.Errorf("failed to bring up bridge: %w", err)
	}

	return nil
}

// SetupNAT configures NAT for the bridge network using iptables.
// This is mainly for testing; in production, NAT rules should be pre-configured.
func (nm *NetworkManager) SetupNAT() error {
	// Enable IP forwarding
	if err := exec.Command("sysctl", "-w", "net.ipv4.ip_forward=1").Run(); err != nil {
		return fmt.Errorf("failed to enable IP forwarding: %w", err)
	}

	// Add MASQUERADE rule
	networkCIDR := nm.network.String()
	if err := exec.Command("iptables", "-t", "nat", "-C", "POSTROUTING", "-s", networkCIDR, "-j", "MASQUERADE").Run(); err != nil {
		// Rule doesn't exist, add it
		if err := exec.Command("iptables", "-t", "nat", "-A", "POSTROUTING", "-s", networkCIDR, "-j", "MASQUERADE").Run(); err != nil {
			return fmt.Errorf("failed to add NAT rule: %w", err)
		}
	}

	// Add FORWARD rules
	if err := exec.Command("iptables", "-C", "FORWARD", "-i", nm.bridgeName, "-j", "ACCEPT").Run(); err != nil {
		if err := exec.Command("iptables", "-A", "FORWARD", "-i", nm.bridgeName, "-j", "ACCEPT").Run(); err != nil {
			return fmt.Errorf("failed to add forward rule (in): %w", err)
		}
	}

	if err := exec.Command("iptables", "-C", "FORWARD", "-o", nm.bridgeName, "-j", "ACCEPT").Run(); err != nil {
		if err := exec.Command("iptables", "-A", "FORWARD", "-o", nm.bridgeName, "-j", "ACCEPT").Run(); err != nil {
			return fmt.Errorf("failed to add forward rule (out): %w", err)
		}
	}

	return nil
}

// maxTAPIndex is the upper bound on TAP device indices to prevent unbounded iteration.
const maxTAPIndex = 1024

// FindAvailableTAPIndex finds the next available TAP device index whose
// derived IP is free. It skips an index when the index is reserved
// (usedIndices), a TAP device with that name already exists, or the derived
// IP is already claimed on the host (ipInUse — a passive cross-process /
// external-consumer check). Returns an error if no index is available below
// maxTAPIndex or the subnet is exhausted.
func (nm *NetworkManager) FindAvailableTAPIndex(usedIndices map[int]bool) (int, error) {
	for i := 0; i < maxTAPIndex; i++ {
		if usedIndices[i] || nm.TAPExists(nm.TAPDeviceName(i)) {
			continue
		}
		ip, err := nm.AllocateIP(i)
		if err != nil {
			// AllocateIP only fails once the index runs past the subnet;
			// no higher index will fit either, so stop here.
			return 0, fmt.Errorf("no available TAP index with a free IP in %s: %w", nm.network, err)
		}
		if nm.ipInUse != nil && nm.ipInUse(ip) {
			log.Printf("Warning: skipping IP %s (TAP index %d): already in use on host", ip, i)
			continue
		}
		return i, nil
	}
	return 0, fmt.Errorf("no available TAP index found (checked %d indices)", maxTAPIndex)
}
