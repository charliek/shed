package config

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/charliek/shed/internal/egress"
)

// EgressConfig is the server-level Level-1 (audit-first, cooperative) egress
// policy. Off by default — the common case is unrestricted. See
// docs/reference/egress.md. The egress proxy is a child process of shed-server
// that is launched only when Enabled.
type EgressConfig struct {
	// Enabled is the master switch. When false the proxy child is never started
	// and no guest injection happens.
	Enabled bool `yaml:"enabled"`

	// PortRange is the inclusive "lo-hi" range for per-shed listener ports.
	PortRange string `yaml:"port_range,omitempty"`

	// Default is the profile list applied to sheds created without --egress.
	// ABSENT or [] means no egress (NOT [audit]); a list applies those profiles.
	Default []string `yaml:"default,omitempty"`

	// Profiles are reusable named policy fragments. Names "off", "none", and
	// "default" are reserved.
	Profiles map[string]EgressProfile `yaml:"profiles,omitempty"`
}

// EgressProfile is one named policy fragment. Allow/Deny are domain globs
// ("*.github.com" suffix, "github.com" exact); Rule is a CEL expression (the
// power path); Mode "audit" makes the fall-through allow+log instead of deny.
//
// The json tags are the wire contract for the `rules` map in
// `shed egress show --json` (GET /api/egress/{name}). They are snake_case to
// match the rest of the API (egress.AuditRecord, EgressStatus); omitempty keeps
// the output to the fields a profile actually sets.
type EgressProfile struct {
	Mode  string   `json:"mode,omitempty" yaml:"mode,omitempty"`
	Allow []string `json:"allow,omitempty" yaml:"allow,omitempty"`
	Deny  []string `json:"deny,omitempty" yaml:"deny,omitempty"`
	Rule  string   `json:"rule,omitempty" yaml:"rule,omitempty"`
}

// EgressStatus is the `shed egress show` response: a shed's active egress
// assignment, the resolved definitions of its profiles, and recent decisions.
type EgressStatus struct {
	Shed     string                   `json:"shed"`
	Enabled  bool                     `json:"enabled"`            // server-level egress master switch
	Profiles []string                 `json:"profiles,omitempty"` // effective profiles for this shed
	Port     int                      `json:"port,omitempty"`     // assigned listener port
	Rules    map[string]EgressProfile `json:"rules,omitempty"`    // definitions of the active profiles
	Recent   []egress.AuditRecord     `json:"recent,omitempty"`   // recent egress decisions for this shed
}

// EgressSetRequest is the `POST /api/egress/{name}` body (live `shed egress set`).
type EgressSetRequest struct {
	Profiles []string `json:"profiles"`
}

var egressReservedNames = map[string]bool{"off": true, "none": true, "default": true}

func (p EgressProfile) spec() egress.ProfileSpec {
	return egress.ProfileSpec{Mode: p.Mode, Allow: p.Allow, Deny: p.Deny, Rule: p.Rule}
}

// Validate fails fast at config load on a bad port range, a reserved/dangling
// profile name, or an uncompilable glob/CEL rule.
func (c *EgressConfig) Validate() error {
	if c == nil || !c.Enabled {
		return nil
	}
	if c.PortRange != "" {
		if _, _, err := parseEgressPortRange(c.PortRange); err != nil {
			return fmt.Errorf("egress.port_range: %w", err)
		}
	}
	for name, p := range c.Profiles {
		if egressReservedNames[strings.ToLower(name)] {
			return fmt.Errorf("egress.profiles: name %q is reserved", name)
		}
		if err := egress.ValidateProfile(p.spec()); err != nil {
			return fmt.Errorf("egress.profiles.%s: %w", name, err)
		}
	}
	for _, d := range c.Default {
		if egressReservedNames[strings.ToLower(d)] {
			return fmt.Errorf("egress.default: %q is reserved (use [] for no egress)", d)
		}
		if _, ok := c.Profiles[d]; !ok {
			return fmt.Errorf("egress.default references unknown profile %q", d)
		}
	}
	return nil
}

// PortRangeBounds returns the configured [lo, hi] listener-port range, or a
// sensible default when unset.
func (c *EgressConfig) PortRangeBounds() (int, int) {
	if c != nil && c.PortRange != "" {
		if lo, hi, err := parseEgressPortRange(c.PortRange); err == nil {
			return lo, hi
		}
	}
	return 20000, 30000
}

// ResolveProfiles returns the composed profile specs for a shed's requested
// profile list (empty inherits Default). It returns (nil, nil) when egress is
// disabled or the effective list is empty / "off" / "none" — meaning this shed
// gets no egress proxy at all.
func (c *EgressConfig) ResolveProfiles(req []string) ([]egress.ProfileSpec, error) {
	if c == nil || !c.Enabled {
		return nil, nil
	}
	list := req
	if len(list) == 0 {
		list = c.Default
	}
	if len(list) == 1 && (strings.EqualFold(list[0], "off") || strings.EqualFold(list[0], "none")) {
		return nil, nil
	}
	if len(list) == 0 {
		return nil, nil
	}
	specs := make([]egress.ProfileSpec, 0, len(list))
	for _, name := range list {
		p, ok := c.Profiles[name]
		if !ok {
			return nil, fmt.Errorf("unknown egress profile %q", name)
		}
		specs = append(specs, p.spec())
	}
	return specs, nil
}

func parseEgressPortRange(s string) (int, int, error) {
	parts := strings.SplitN(s, "-", 2)
	if len(parts) != 2 {
		return 0, 0, fmt.Errorf("expected lo-hi, got %q", s)
	}
	lo, err1 := strconv.Atoi(strings.TrimSpace(parts[0]))
	hi, err2 := strconv.Atoi(strings.TrimSpace(parts[1]))
	if err1 != nil || err2 != nil || lo < 1 || hi > 65535 || lo >= hi {
		return 0, 0, fmt.Errorf("invalid range %q", s)
	}
	return lo, hi, nil
}
