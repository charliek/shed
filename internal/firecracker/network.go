//go:build linux
// +build linux

package firecracker

import (
	"fmt"
	"log"
	"net"
	"os/exec"
	"strings"

	"github.com/vishvananda/netlink"
)

// NetworkManager handles TAP device creation and IP allocation.
type NetworkManager struct {
	bridgeName string
	bridgeCIDR string
	tapPrefix  string
	gateway    net.IP
	network    *net.IPNet
}

// NewNetworkManager creates a new NetworkManager.
func NewNetworkManager(bridgeName, bridgeCIDR, tapPrefix string) (*NetworkManager, error) {
	ip, network, err := net.ParseCIDR(bridgeCIDR)
	if err != nil {
		return nil, fmt.Errorf("invalid bridge CIDR %q: %w", bridgeCIDR, err)
	}

	return &NetworkManager{
		bridgeName: bridgeName,
		bridgeCIDR: bridgeCIDR,
		tapPrefix:  tapPrefix,
		gateway:    ip,
		network:    network,
	}, nil
}

// TAPDeviceName returns the TAP device name for an instance index.
func (nm *NetworkManager) TAPDeviceName(index int) string {
	return fmt.Sprintf("%s-%d", nm.tapPrefix, index)
}

// AllocateIP returns the IP address for an instance index.
// Index 0 is reserved for the gateway, so instances start at index 1.
func (nm *NetworkManager) AllocateIP(index int) string {
	// Start from gateway IP and add the index + 1
	// e.g., gateway is 172.30.0.1, index 0 gets 172.30.0.2
	ip := make(net.IP, len(nm.gateway))
	copy(ip, nm.gateway)

	// Convert IP to uint32 for proper arithmetic across octets
	ipInt := ipToUint32(ip)
	ipInt += uint32(index + 1)

	return uint32ToIP(ipInt).String()
}

// ipToUint32 converts an IPv4 address to a uint32.
func ipToUint32(ip net.IP) uint32 {
	ip = ip.To4()
	if ip == nil {
		return 0
	}
	return uint32(ip[0])<<24 | uint32(ip[1])<<16 | uint32(ip[2])<<8 | uint32(ip[3])
}

// uint32ToIP converts a uint32 to an IPv4 address.
func uint32ToIP(n uint32) net.IP {
	return net.IPv4(byte(n>>24), byte(n>>16), byte(n>>8), byte(n))
}

// Gateway returns the gateway IP address.
func (nm *NetworkManager) Gateway() string {
	return nm.gateway.String()
}

// CreateTAPDevice creates a TAP device and attaches it to the bridge.
func (nm *NetworkManager) CreateTAPDevice(name string) error {
	// Create TAP device using netlink
	tap := &netlink.Tuntap{
		LinkAttrs: netlink.LinkAttrs{
			Name: name,
		},
		Mode: netlink.TUNTAP_MODE_TAP,
	}

	if err := netlink.LinkAdd(tap); err != nil {
		return fmt.Errorf("failed to create TAP device %s: %w", name, err)
	}

	// Get the bridge
	bridge, err := netlink.LinkByName(nm.bridgeName)
	if err != nil {
		// Clean up TAP device on failure
		_ = netlink.LinkDel(tap)
		return fmt.Errorf("failed to find bridge %s: %w", nm.bridgeName, err)
	}

	// Get the TAP link (need to re-fetch after creation)
	tapLink, err := netlink.LinkByName(name)
	if err != nil {
		_ = netlink.LinkDel(tap)
		return fmt.Errorf("failed to find created TAP device %s: %w", name, err)
	}

	// Attach TAP to bridge
	if err := netlink.LinkSetMaster(tapLink, bridge); err != nil {
		_ = netlink.LinkDel(tap)
		return fmt.Errorf("failed to attach TAP %s to bridge %s: %w", name, nm.bridgeName, err)
	}

	// Bring up TAP device
	if err := netlink.LinkSetUp(tapLink); err != nil {
		_ = netlink.LinkDel(tap)
		return fmt.Errorf("failed to bring up TAP device %s: %w", name, err)
	}

	return nil
}

// DeleteTAPDevice removes a TAP device.
func (nm *NetworkManager) DeleteTAPDevice(name string) error {
	link, err := netlink.LinkByName(name)
	if err != nil {
		// Device doesn't exist, nothing to do
		if strings.Contains(err.Error(), "not found") {
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

// FindAvailableTAPIndex finds the next available TAP device index.
func (nm *NetworkManager) FindAvailableTAPIndex(usedIndices map[int]bool) int {
	for i := 0; ; i++ {
		if !usedIndices[i] && !nm.TAPExists(nm.TAPDeviceName(i)) {
			return i
		}
	}
}
