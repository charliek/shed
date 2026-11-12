// Package netpolicy renders and applies shed-server's own nftables tables,
// which enforce the per-shed network profiles on the Firecracker bridge.
//
// A server owns exactly two tables, `bridge shed_<name>` and
// `inet shed_<name>` (the server's `name:` sanitized), and nothing else: the
// tables only add drops, so the host's existing iptables NAT and FORWARD rules,
// Docker's, Tailscale's and any other server's tables are left alone (a drop
// in any table is final; an accept ends only the chain it is in). Every change
// re-renders both tables in full and applies them with `nft -f -` in one
// transaction, so the kernel never holds a half-built rule set and a stale
// kernel state is corrected by the next successful apply.
//
// The rendering and the state are platform-neutral; only the Runner shells out
// to nft, so the package builds everywhere and does nothing on a host that
// needs no rules.
package netpolicy

import (
	"fmt"

	"github.com/charliek/shed/internal/config"
)

// Profile is a shed's network profile.
type Profile string

const (
	// Open is today's network, unchanged: the shed is not in any set.
	Open Profile = config.NetworkProfileOpen
	// Isolated gives the guest the internet and nothing else, and guards its
	// address against spoofing (a "guarded" shed).
	Isolated Profile = config.NetworkProfileIsolated
)

// Parse returns the profile called s; "" is Open.
func Parse(s string) (Profile, error) {
	switch s {
	case "", config.NetworkProfileOpen:
		return Open, nil
	case config.NetworkProfileIsolated:
		return Isolated, nil
	default:
		return "", fmt.Errorf("unknown network profile %q (must be %s or %s)", s, Open, Isolated)
	}
}

// Guarded reports whether a shed with profile p needs rules (any profile
// other than open).
func (p Profile) Guarded() bool { return p != Open && p != "" }
