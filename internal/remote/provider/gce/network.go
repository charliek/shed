package gce

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/charliek/shed/internal/remote/provider"
)

// Disk is a persistent disk as the API reports it. The backend reads its
// environment's disks to find any a failed create or a crash left behind.
type Disk struct {
	Name   string
	Zone   string
	Labels map[string]string
	SizeGB int
	// Status is CREATING, RESTORING, FAILED, READY or DELETING.
	Status string
	// Users are the instances the disk is attached to; empty means
	// unattached.
	Users        []string
	CreatedAt    time.Time
	LastAttachAt time.Time
	LastDetachAt time.Time
	SourceImage  string
}

type gceDisk struct {
	Name                string            `json:"name"`
	Zone                string            `json:"zone"`
	Labels              map[string]string `json:"labels"`
	SizeGb              int64String       `json:"sizeGb"`
	Status              string            `json:"status"`
	Users               []string          `json:"users"`
	CreationTimestamp   string            `json:"creationTimestamp"`
	LastAttachTimestamp string            `json:"lastAttachTimestamp"`
	LastDetachTimestamp string            `json:"lastDetachTimestamp"`
	SourceImage         string            `json:"sourceImage"`
}

func (g *gceDisk) toDisk() Disk {
	users := make([]string, 0, len(g.Users))
	for _, u := range g.Users {
		users = append(users, lastSegment(u))
	}
	return Disk{
		Name:         g.Name,
		Zone:         lastSegment(g.Zone),
		Labels:       g.Labels,
		SizeGB:       int(g.SizeGb),
		Status:       g.Status,
		Users:        users,
		CreatedAt:    parseTime(g.CreationTimestamp),
		LastAttachAt: parseTime(g.LastAttachTimestamp),
		LastDetachAt: parseTime(g.LastDetachTimestamp),
		SourceImage:  lastSegment(g.SourceImage),
	}
}

// ListDisks returns every disk in the client's zone whose shed_env label is
// the client's environment, all pages or an error, in API order.
func (c *Client) ListDisks(ctx context.Context) ([]Disk, error) {
	q := url.Values{"filter": {`labels.` + provider.LabelEnv + ` = "` + c.cfg.Env + `"`}}
	var out []Disk
	err := c.listPages(ctx, "list_disks", c.zonePath(c.cfg.Zone)+"disks", q, func(raw json.RawMessage) error {
		var items []gceDisk
		if err := json.Unmarshal(raw, &items); err != nil {
			return err
		}
		for i := range items {
			if items[i].Labels[provider.LabelEnv] != c.cfg.Env {
				continue
			}
			out = append(out, items[i].toDisk())
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// DeleteDisk deletes one of the environment's disks in the client's zone and
// waits for the delete. A missing disk is success. A disk of another
// environment is ErrForeign; one still attached to an instance is ErrBusy.
func (c *Client) DeleteDisk(ctx context.Context, name string) error {
	if !imageNameRegex.MatchString(name) {
		return provider.Errorf(provider.ErrInvalid, "delete_disk", "disk name %q is not valid", name)
	}
	path := c.zonePath(c.cfg.Zone) + "disks/" + name
	var g gceDisk
	err := c.call(ctx, request{op: "delete_disk", method: http.MethodGet, path: path}, &g)
	if err := asProviderError("delete_disk", err); err != nil {
		if errors.Is(err, provider.ErrNotFound) {
			return nil
		}
		return err
	}
	if env := g.Labels[provider.LabelEnv]; env != c.cfg.Env {
		return provider.Errorf(provider.ErrForeign, "delete_disk", "disk %s has %s=%q, this server is %q", name, provider.LabelEnv, env, c.cfg.Env)
	}
	if len(g.Users) > 0 {
		return provider.Errorf(provider.ErrBusy, "delete_disk", "disk %s is attached to %s", name, lastSegment(g.Users[0]))
	}
	o, err := c.mutate(ctx, "delete_disk", http.MethodDelete, path, nil, nil)
	if errors.Is(err, provider.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := c.runOperation(ctx, "delete_disk", o); err != nil && !errors.Is(err, provider.ErrNotFound) {
		return err
	}
	return nil
}

// Network is a VPC network.
type Network struct {
	Name string
	// AutoCreateSubnetworks is true for an auto-mode network; shed wants a
	// custom-mode one.
	AutoCreateSubnetworks bool
	// Subnetworks are the subnetwork paths, "regions/<r>/subnetworks/<s>".
	Subnetworks []string
	// FirewallPolicy is the network firewall policy attached to it, or "".
	FirewallPolicy string
	SelfLink       string
}

// Subnetwork is one regional subnet.
type Subnetwork struct {
	Name        string
	Region      string
	Network     string // the network's name
	IPCIDRRange string
	// StackType is IPV4_ONLY, IPV4_IPV6 or IPV6_ONLY.
	StackType             string
	PrivateIPGoogleAccess bool
}

// FirewallRule is one VPC firewall rule.
type FirewallRule struct {
	Name      string
	Network   string // the network's name
	Direction string // INGRESS or EGRESS
	Priority  int
	Disabled  bool
	// SourceRanges, SourceTags and SourceServiceAccounts select who an
	// ingress rule admits.
	SourceRanges          []string
	SourceTags            []string
	SourceServiceAccounts []string
	// TargetTags and TargetServiceAccounts select the instances it covers;
	// both empty means every instance on the network.
	TargetTags            []string
	TargetServiceAccounts []string
	DestinationRanges     []string
	Allowed               []FirewallPorts
	Denied                []FirewallPorts
}

// FirewallPorts is one protocol and its ports; no ports means all of them.
type FirewallPorts struct {
	Protocol string
	Ports    []string
}

// GetNetwork reads the client's network.
func (c *Client) GetNetwork(ctx context.Context) (Network, error) {
	var g struct {
		Name                  string   `json:"name"`
		AutoCreateSubnetworks bool     `json:"autoCreateSubnetworks"`
		Subnetworks           []string `json:"subnetworks"`
		FirewallPolicy        string   `json:"firewallPolicy"`
		SelfLink              string   `json:"selfLink"`
	}
	if err := c.call(ctx, request{op: "get_network", method: http.MethodGet, path: c.networkURL}, &g); err != nil {
		return Network{}, asProviderError("get_network", err)
	}
	n := Network{Name: g.Name, AutoCreateSubnetworks: g.AutoCreateSubnetworks, FirewallPolicy: g.FirewallPolicy, SelfLink: g.SelfLink}
	for _, s := range g.Subnetworks {
		n.Subnetworks = append(n.Subnetworks, trimToRegion(s))
	}
	return n, nil
}

// trimToRegion shortens a subnetwork URL to "regions/<r>/subnetworks/<s>".
func trimToRegion(u string) string {
	if i := strings.Index(u, "regions/"); i >= 0 {
		return u[i:]
	}
	return u
}

// GetSubnetwork reads the client's subnetwork. It is ErrInvalid when the
// config names none.
func (c *Client) GetSubnetwork(ctx context.Context) (Subnetwork, error) {
	if c.subnetURL == "" {
		return Subnetwork{}, provider.Errorf(provider.ErrInvalid, "get_subnetwork", "no subnetwork is configured")
	}
	var g struct {
		Name                  string `json:"name"`
		Region                string `json:"region"`
		Network               string `json:"network"`
		IPCidrRange           string `json:"ipCidrRange"`
		StackType             string `json:"stackType"`
		PrivateIPGoogleAccess bool   `json:"privateIpGoogleAccess"`
	}
	if err := c.call(ctx, request{op: "get_subnetwork", method: http.MethodGet, path: c.subnetURL}, &g); err != nil {
		return Subnetwork{}, asProviderError("get_subnetwork", err)
	}
	return Subnetwork{
		Name:                  g.Name,
		Region:                lastSegment(g.Region),
		Network:               lastSegment(g.Network),
		IPCIDRRange:           g.IPCidrRange,
		StackType:             g.StackType,
		PrivateIPGoogleAccess: g.PrivateIPGoogleAccess,
	}, nil
}

// ListFirewalls returns every firewall rule on the client's network, all
// pages or an error, in API order. The project's rules are read and filtered
// here by network, so no filter syntax is involved.
func (c *Client) ListFirewalls(ctx context.Context) ([]FirewallRule, error) {
	network := lastSegment(c.networkURL)
	var out []FirewallRule
	err := c.listPages(ctx, "list_firewalls", c.projectPath()+"global/firewalls", nil, func(raw json.RawMessage) error {
		var items []struct {
			Name                  string   `json:"name"`
			Network               string   `json:"network"`
			Direction             string   `json:"direction"`
			Priority              int      `json:"priority"`
			Disabled              bool     `json:"disabled"`
			SourceRanges          []string `json:"sourceRanges"`
			SourceTags            []string `json:"sourceTags"`
			SourceServiceAccounts []string `json:"sourceServiceAccounts"`
			TargetTags            []string `json:"targetTags"`
			TargetServiceAccounts []string `json:"targetServiceAccounts"`
			DestinationRanges     []string `json:"destinationRanges"`
			Allowed               []struct {
				IPProtocol string   `json:"IPProtocol"`
				Ports      []string `json:"ports"`
			} `json:"allowed"`
			Denied []struct {
				IPProtocol string   `json:"IPProtocol"`
				Ports      []string `json:"ports"`
			} `json:"denied"`
		}
		if err := json.Unmarshal(raw, &items); err != nil {
			return err
		}
		for _, it := range items {
			if lastSegment(it.Network) != network {
				continue
			}
			r := FirewallRule{
				Name:                  it.Name,
				Network:               network,
				Direction:             it.Direction,
				Priority:              it.Priority,
				Disabled:              it.Disabled,
				SourceRanges:          it.SourceRanges,
				SourceTags:            it.SourceTags,
				SourceServiceAccounts: it.SourceServiceAccounts,
				TargetTags:            it.TargetTags,
				TargetServiceAccounts: it.TargetServiceAccounts,
				DestinationRanges:     it.DestinationRanges,
			}
			for _, a := range it.Allowed {
				r.Allowed = append(r.Allowed, FirewallPorts{Protocol: a.IPProtocol, Ports: a.Ports})
			}
			for _, d := range it.Denied {
				r.Denied = append(r.Denied, FirewallPorts{Protocol: d.IPProtocol, Ports: d.Ports})
			}
			out = append(out, r)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}
