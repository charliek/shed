package gce

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/charliek/shed/internal/remote/provider"
)

// DefaultNetworkName is the network every project is born with. Its stock
// rules admit traffic between instances and SSH from anywhere.
const DefaultNetworkName = "default"

type gceFirewallPorts struct {
	IPProtocol string   `json:"IPProtocol"`
	Ports      []string `json:"ports"`
}

type gceFirewall struct {
	Name                  string             `json:"name"`
	Network               string             `json:"network"`
	Direction             string             `json:"direction"`
	Priority              int                `json:"priority"`
	Disabled              bool               `json:"disabled"`
	SourceRanges          []string           `json:"sourceRanges"`
	SourceTags            []string           `json:"sourceTags"`
	SourceServiceAccounts []string           `json:"sourceServiceAccounts"`
	TargetTags            []string           `json:"targetTags"`
	TargetServiceAccounts []string           `json:"targetServiceAccounts"`
	DestinationRanges     []string           `json:"destinationRanges"`
	Allowed               []gceFirewallPorts `json:"allowed"`
	Denied                []gceFirewallPorts `json:"denied"`
}

func (g *gceFirewall) toRule() provider.FirewallRule {
	r := provider.FirewallRule{
		Name:                  g.Name,
		Network:               lastSegment(g.Network),
		Direction:             g.Direction,
		Priority:              g.Priority,
		Disabled:              g.Disabled,
		SourceRanges:          g.SourceRanges,
		SourceTags:            g.SourceTags,
		SourceServiceAccounts: g.SourceServiceAccounts,
		TargetTags:            g.TargetTags,
		TargetServiceAccounts: g.TargetServiceAccounts,
		DestinationRanges:     g.DestinationRanges,
	}
	if r.Direction == "" {
		// The API's default when a rule was made without one.
		r.Direction = provider.DirectionIngress
	}
	for _, a := range g.Allowed {
		r.Allowed = append(r.Allowed, provider.FirewallPorts{Protocol: a.IPProtocol, Ports: a.Ports})
	}
	for _, d := range g.Denied {
		r.Denied = append(r.Denied, provider.FirewallPorts{Protocol: d.IPProtocol, Ports: d.Ports})
	}
	return r
}

// firewalls reads every firewall rule of the project, all pages or an error,
// and returns the ones keep accepts, in API order. Rules are filtered here,
// so no filter syntax is involved and the one permission it needs is
// compute.firewalls.list.
func (c *Client) firewalls(ctx context.Context, op string, keep func(provider.FirewallRule) bool) ([]provider.FirewallRule, error) {
	var out []provider.FirewallRule
	err := c.listPages(ctx, op, c.projectPath()+"global/firewalls", nil, func(raw json.RawMessage) error {
		var items []gceFirewall
		if err := json.Unmarshal(raw, &items); err != nil {
			return err
		}
		for i := range items {
			if r := items[i].toRule(); keep(r) {
				out = append(out, r)
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// ListFirewalls returns every firewall rule on the client's network, ingress
// and egress, all pages or an error, in API order.
func (c *Client) ListFirewalls(ctx context.Context) ([]provider.FirewallRule, error) {
	network := lastSegment(c.networkURL)
	return c.firewalls(ctx, "list_firewalls", func(r provider.FirewallRule) bool { return r.Network == network })
}

// EnvFirewallRule finds the environment's rule by name among the project's
// rules, on whichever network it is. A rule name is unique in a project.
func (c *Client) EnvFirewallRule(ctx context.Context) (provider.FirewallRule, error) {
	const op = "env_firewall_rule"
	name := provider.FirewallRuleName(c.cfg.Env)
	rules, err := c.firewalls(ctx, op, func(r provider.FirewallRule) bool { return r.Name == name })
	if err != nil {
		return provider.FirewallRule{}, err
	}
	if len(rules) == 0 {
		return provider.FirewallRule{}, provider.Errorf(provider.ErrNotFound, op, "no firewall rule named %s", name)
	}
	return rules[0], nil
}

// NetworkIngress returns the client's network and its ingress rules.
func (c *Client) NetworkIngress(ctx context.Context) (provider.NetworkIngress, error) {
	network := lastSegment(c.networkURL)
	rules, err := c.firewalls(ctx, "network_ingress", func(r provider.FirewallRule) bool {
		return r.Network == network && r.Direction == provider.DirectionIngress
	})
	if err != nil {
		return provider.NetworkIngress{}, err
	}
	provider.SortFirewallRules(rules)
	return provider.NetworkIngress{Network: network, Default: network == DefaultNetworkName, Rules: rules}, nil
}

// SetEnvFirewallSources patches the source ranges of the environment's rule
// and waits for the change. The request is firewalls.patch on the one derived
// rule name with a body that holds sourceRanges and nothing else, so no other
// rule and no other field can be changed by it. It needs the permissions in
// FirewallAutoPermissions, which the base role does not hold.
func (c *Client) SetEnvFirewallSources(ctx context.Context, ranges []string) error {
	const op = "set_firewall_sources"
	if err := provider.CheckFirewallSources(op, ranges); err != nil {
		return err
	}
	path := c.projectPath() + "global/firewalls/" + provider.FirewallRuleName(c.cfg.Env)
	body := struct {
		SourceRanges []string `json:"sourceRanges"`
	}{SourceRanges: ranges}
	o, err := c.mutate(ctx, op, http.MethodPatch, path, nil, body)
	if err != nil {
		return err
	}
	return c.runGlobalOperation(ctx, op, o)
}
