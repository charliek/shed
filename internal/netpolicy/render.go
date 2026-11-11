package netpolicy

import (
	"fmt"
	"net"
	"regexp"
	"sort"
	"strings"
)

// Shed is one shed's entry in the rule set. Only guarded sheds (a non-open
// Profile) are rendered; an open shed is in no set.
type Shed struct {
	// Name is the shed name (bookkeeping only; never rendered).
	Name string
	// TAP is the host-side interface the guest's NIC is attached to.
	TAP string
	// MAC is the guest NIC's MAC address.
	MAC string
	// IP is the guest's IPv4 address on the bridge.
	IP string
	// Profile is the shed's network profile.
	Profile Profile
	// EgressPort is the shed's own egress-proxy port on the gateway (0 when
	// it has none).
	EgressPort uint16
	// P9Ports are the shed's own 9P server ports on the gateway (its server
	// mounts and project mounts, while they are open).
	P9Ports []uint16
}

// HostPorts is the set of gateway TCP ports the shed may reach: its egress
// port and its 9P ports, sorted and without duplicates.
func (s Shed) HostPorts() []uint16 {
	seen := map[uint16]bool{}
	var out []uint16
	add := func(p uint16) {
		if p != 0 && !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	add(s.EgressPort)
	for _, p := range s.P9Ports {
		add(p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// Config is the server-level input to the rule set.
type Config struct {
	// ServerName is the server's `name:`; it names the tables.
	ServerName string
	// Bridge is the Linux bridge the TAPs are attached to.
	Bridge string
	// Gateway is the bridge's own IPv4 address (the guests' gateway).
	Gateway string
	// AllowCIDRs are IPv4 ranges a guarded shed may reach although they are
	// in the blocked ranges (network.allow_cidrs). Forwarded traffic only.
	AllowCIDRs []string
}

// BlockedCIDRs are the IPv4 ranges a guarded shed may not reach through the
// host: "this network", RFC 1918, CGNAT (and so the tailnet), loopback,
// link-local (cloud metadata included), IETF protocol assignments,
// benchmarking, and multicast plus the reserved 240/4 (with broadcast).
var BlockedCIDRs = []string{
	"0.0.0.0/8",
	"10.0.0.0/8",
	"100.64.0.0/10",
	"127.0.0.0/8",
	"169.254.0.0/16",
	"172.16.0.0/12",
	"192.0.0.0/24",
	"192.168.0.0/16",
	"198.18.0.0/15",
	"224.0.0.0/3",
}

var nonTableByte = regexp.MustCompile(`[^A-Za-z0-9_]`)

// TableName is the name of both of a server's tables: "shed_" plus the
// server name with every byte outside [A-Za-z0-9_] replaced by "_"
// ("mini3-dev" gives "shed_mini3_dev").
func TableName(serverName string) string {
	return "shed_" + nonTableByte.ReplaceAllString(serverName, "_")
}

// ifnamePattern is what an interface name rendered into the rule set may look
// like (Linux allows at most 15 bytes; the rest keeps the script well-formed).
var ifnamePattern = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,15}$`)

// macPattern is a colon-separated 48-bit MAC address.
var macPattern = regexp.MustCompile(`^[0-9A-Fa-f]{2}(:[0-9A-Fa-f]{2}){5}$`)

func ipv4(s, what string) (string, error) {
	ip := net.ParseIP(strings.TrimSpace(s))
	if ip == nil || ip.To4() == nil {
		return "", fmt.Errorf("netpolicy: %s %q is not an IPv4 address", what, s)
	}
	return ip.To4().String(), nil
}

// guardedSheds validates and returns the guarded sheds, sorted by name.
func guardedSheds(sheds []Shed) ([]Shed, error) {
	var out []Shed
	for _, s := range sheds {
		if !s.Profile.Guarded() {
			continue
		}
		if !ifnamePattern.MatchString(s.TAP) {
			return nil, fmt.Errorf("netpolicy: shed %q: invalid TAP name %q", s.Name, s.TAP)
		}
		if !macPattern.MatchString(s.MAC) {
			return nil, fmt.Errorf("netpolicy: shed %q: invalid MAC %q", s.Name, s.MAC)
		}
		ip, err := ipv4(s.IP, "shed "+s.Name+" IP")
		if err != nil {
			return nil, err
		}
		s.IP = ip
		s.MAC = strings.ToLower(s.MAC)
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// elements renders a set's elements clause, or nothing for an empty set (nft
// refuses `elements = { }`).
func elements(b *strings.Builder, indent string, elems []string) {
	if len(elems) == 0 {
		return
	}
	fmt.Fprintf(b, "%selements = { %s }\n", indent, strings.Join(elems, ", "))
}

// Render returns one `nft -f -` script that atomically replaces both of the
// server's tables with the rule set for sheds (each table is added, then
// deleted, then defined: the add makes the delete safe when the table is
// absent, and the whole script is one transaction). Open sheds are ignored.
func Render(cfg Config, sheds []Shed) (string, error) {
	if !ifnamePattern.MatchString(cfg.Bridge) {
		return "", fmt.Errorf("netpolicy: invalid bridge name %q", cfg.Bridge)
	}
	gw, err := ipv4(cfg.Gateway, "gateway")
	if err != nil {
		return "", err
	}
	var allow []string
	for _, c := range cfg.AllowCIDRs {
		ip, n, err := net.ParseCIDR(strings.TrimSpace(c))
		if err != nil || ip.To4() == nil {
			return "", fmt.Errorf("netpolicy: allow_cidrs entry %q is not an IPv4 CIDR", c)
		}
		allow = append(allow, n.String())
	}
	guarded, err := guardedSheds(sheds)
	if err != nil {
		return "", err
	}

	var ports, ips, addr4, hostAllow []string
	for _, s := range guarded {
		ports = append(ports, fmt.Sprintf("%q", s.TAP))
		ips = append(ips, s.IP)
		addr4 = append(addr4, fmt.Sprintf("%q . %s . %s", s.TAP, s.MAC, s.IP))
		for _, p := range s.HostPorts() {
			hostAllow = append(hostAllow, fmt.Sprintf("%s . %d", s.IP, p))
		}
	}

	table := TableName(cfg.ServerName)
	br := fmt.Sprintf("%q", cfg.Bridge)
	var b strings.Builder
	fmt.Fprintf(&b, "# shed-server network policy for server %q (generated; do not edit)\n", cfg.ServerName)
	fmt.Fprintf(&b, "add table bridge %s\ndelete table bridge %s\n", table, table)
	fmt.Fprintf(&b, "add table inet %s\ndelete table inet %s\n\n", table, table)

	// L2: anti-spoofing keyed on the claimed address, and guest-to-guest.
	fmt.Fprintf(&b, "table bridge %s {\n", table)
	b.WriteString("\tset guarded_ports {\n\t\ttype ifname\n")
	elements(&b, "\t\t", ports)
	b.WriteString("\t}\n\tset guarded_ip4 {\n\t\ttype ipv4_addr\n")
	elements(&b, "\t\t", ips)
	b.WriteString("\t}\n\tset addr4 {\n\t\ttype ifname . ether_addr . ipv4_addr\n")
	elements(&b, "\t\t", addr4)
	b.WriteString("\t}\n")
	b.WriteString("\tchain pre {\n\t\ttype filter hook prerouting priority filter; policy accept;\n")
	fmt.Fprintf(&b, "\t\tmeta ibrname != %s accept\n", br)
	b.WriteString("\t\tiifname @guarded_ports jump from_guarded\n")
	b.WriteString("\t\tether type arp arp saddr ip @guarded_ip4 jump verify_arp\n")
	b.WriteString("\t\tether type ip ip saddr @guarded_ip4 jump verify_ip\n")
	b.WriteString("\t}\n")
	b.WriteString("\tchain from_guarded {\n")
	b.WriteString("\t\tether type arp jump verify_arp\n")
	b.WriteString("\t\tether type ip jump verify_ip\n")
	b.WriteString("\t\tdrop\n")
	b.WriteString("\t}\n")
	b.WriteString("\tchain verify_arp {\n\t\tiifname . ether saddr . arp saddr ip @addr4 accept\n\t\tdrop\n\t}\n")
	b.WriteString("\tchain verify_ip {\n\t\tiifname . ether saddr . ip saddr @addr4 accept\n\t\tdrop\n\t}\n")
	b.WriteString("\tchain bridged {\n\t\ttype filter hook forward priority filter; policy accept;\n")
	fmt.Fprintf(&b, "\t\tmeta ibrname != %s accept\n", br)
	b.WriteString("\t\tiifname @guarded_ports drop\n")
	b.WriteString("\t\toifname @guarded_ports drop\n")
	b.WriteString("\t}\n}\n\n")

	// L3: host input and forwarding from guarded addresses.
	fmt.Fprintf(&b, "table inet %s {\n", table)
	b.WriteString("\tset guarded4 {\n\t\ttype ipv4_addr\n")
	elements(&b, "\t\t", ips)
	b.WriteString("\t}\n\tset host_allow {\n\t\ttype ipv4_addr . inet_service\n")
	elements(&b, "\t\t", hostAllow)
	b.WriteString("\t}\n\tset allow4 {\n\t\ttype ipv4_addr\n\t\tflags interval\n")
	elements(&b, "\t\t", allow)
	b.WriteString("\t}\n\tset blocked4 {\n\t\ttype ipv4_addr\n\t\tflags interval\n")
	elements(&b, "\t\t", BlockedCIDRs)
	b.WriteString("\t}\n")
	b.WriteString("\tchain input {\n\t\ttype filter hook input priority filter; policy accept;\n")
	fmt.Fprintf(&b, "\t\tiifname %s ip saddr @guarded4 jump guarded_in\n", br)
	b.WriteString("\t}\n")
	b.WriteString("\tchain guarded_in {\n")
	b.WriteString("\t\tct state established,related accept\n")
	fmt.Fprintf(&b, "\t\tip daddr %s ip saddr . tcp dport @host_allow accept\n", gw)
	b.WriteString("\t\tdrop\n")
	b.WriteString("\t}\n")
	b.WriteString("\tchain forward {\n\t\ttype filter hook forward priority filter; policy accept;\n")
	b.WriteString("\t\tip daddr @guarded4 ct state established,related accept\n")
	b.WriteString("\t\tip daddr @guarded4 drop\n")
	fmt.Fprintf(&b, "\t\tip saddr @guarded4 oifname %s drop\n", br)
	b.WriteString("\t\tip saddr @guarded4 ip daddr @allow4 accept\n")
	b.WriteString("\t\tip saddr @guarded4 ip daddr @blocked4 drop\n")
	b.WriteString("\t}\n}\n")
	return b.String(), nil
}

// RenderTeardown returns one `nft -f -` script that deletes both of the
// server's tables, and succeeds whether or not they exist. It names no other
// table.
func RenderTeardown(serverName string) string {
	table := TableName(serverName)
	return fmt.Sprintf("add table bridge %s\ndelete table bridge %s\nadd table inet %s\ndelete table inet %s\n", table, table, table, table)
}
