package provider

import (
	"context"
	"net/netip"
	"sort"
)

// Firewall rule directions, as FirewallRule.Direction carries them.
const (
	DirectionIngress = "INGRESS"
	DirectionEgress  = "EGRESS"
)

// MinFirewallSourceBits is the widest source range a FirewallSourceSetter
// accepts: a /24. The backend only ever sends one /32.
const MinFirewallSourceBits = 24

// FirewallRule is one rule of a cloud network's firewall, as the cloud
// reports it. The backend reads rules to check them; it never builds one.
type FirewallRule struct {
	Name string
	// Network is the name of the network the rule is on.
	Network string
	// Direction is DirectionIngress or DirectionEgress.
	Direction string
	Priority  int
	// Disabled rules are stored and not enforced.
	Disabled bool
	// SourceRanges, SourceTags and SourceServiceAccounts select who an
	// ingress rule admits. A packet that matches any one of them matches.
	SourceRanges          []string
	SourceTags            []string
	SourceServiceAccounts []string
	// TargetTags and TargetServiceAccounts select the instances the rule
	// covers; both empty means every instance on the network.
	TargetTags            []string
	TargetServiceAccounts []string
	DestinationRanges     []string
	// Allowed is set on an allow rule and Denied on a deny rule.
	Allowed []FirewallPorts
	Denied  []FirewallPorts
}

// FirewallPorts is one protocol and its ports; no ports means all of them.
type FirewallPorts struct {
	Protocol string
	Ports    []string
}

// NetworkIngress is the network a provider puts its instances on and the
// ingress rules that network has.
type NetworkIngress struct {
	// Network is the network's name.
	Network string
	// Default is true when it is the network a cloud project is born with,
	// whose stock rules admit traffic between instances and SSH from
	// anywhere. Sheds must never be on it.
	Default bool
	// Rules is every ingress rule on the network, enabled or not, allow or
	// deny, ordered by name.
	Rules []FirewallRule
}

// FirewallSourceSetter is an optional interface. A provider implements it
// when it can change the source ranges of the environment's firewall rule,
// which remote.firewall.mode: auto needs so the rule can follow the server's
// public address. The server's cloud role must allow it; the base role does
// not.
type FirewallSourceSetter interface {
	// SetEnvFirewallSources replaces the source ranges of the environment's
	// one rule with ranges and changes nothing else about it. It takes no
	// rule name: the rule is always FirewallRuleName(env) for the
	// provider's own environment, so the call cannot be aimed at another
	// rule. It returns once the cloud has applied the change; a following
	// EnvFirewallRule shows it.
	//
	// ErrInvalid means ranges is empty or holds something other than a
	// canonical IPv4 CIDR no wider than a /MinFirewallSourceBits; nothing
	// was sent. ErrNotFound means the rule does not exist; it is never
	// created.
	SetEnvFirewallSources(ctx context.Context, ranges []string) error
}

// CheckFirewallSources is the argument check every FirewallSourceSetter
// makes before it sends anything.
func CheckFirewallSources(op string, ranges []string) error {
	if len(ranges) == 0 {
		return Errorf(ErrInvalid, op, "no source range given")
	}
	for _, r := range ranges {
		p, err := netip.ParsePrefix(r)
		if err != nil || !p.Addr().Is4() || p.Masked() != p || p.String() != r {
			return Errorf(ErrInvalid, op, "source range %q is not a canonical IPv4 CIDR", r)
		}
		if p.Bits() < MinFirewallSourceBits {
			return Errorf(ErrInvalid, op, "source range %q is wider than a /%d", r, MinFirewallSourceBits)
		}
	}
	return nil
}

// SortFirewallRules orders rules by name, the order NetworkIngress promises.
func SortFirewallRules(rules []FirewallRule) {
	sort.SliceStable(rules, func(i, k int) bool { return rules[i].Name < rules[k].Name })
}
