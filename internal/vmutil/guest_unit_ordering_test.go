package vmutil

// Boot-ordering invariants for the guest systemd unit files baked into
// VZ and Firecracker rootfs images. These tests are pure file parsing —
// no VM is booted — so they run on standard GHA runners that lack KVM
// (used by the FC backend) and Apple-Silicon virtualization (VZ).
//
// They lock the ordering decisions made in PR #126 (FC firstboot reorder)
// and the *intentional* non-changes (VZ left untouched; FC network-setup
// kept as the agent's static-IP gate). Without these tests, a future
// well-intentioned "make the two platforms uniform" change would silently
// re-create the regressions PR #126 measured and avoided — see
// docs/discovery/platform-runtime-optimization.md §14.
//
// Each test asserts a specific `Before=` directive (presence and absence)
// in a specific guest unit file and points the reader at the doc section
// that records *why* the invariant matters. The doc-link in every failure
// message is the load-bearing piece: the next contributor to touch one of
// these lines should always have somewhere to read before changing it.

import (
	"os"
	"strings"
	"testing"
)

// directiveTokens returns the union of every token that appears in
// `<key>=...` directives inside the named section of a systemd unit file.
// Whole-line comments (lines starting with `#`) and directives outside
// the requested section are ignored.
//
// Caveats (acceptable today — every unit file in this repo uses simple
// whole-line comments and no continuation lines):
//   - Inline comments on the same line as a directive (e.g.
//     `Before=ssh.service # note`) are NOT stripped; the `# note` would be
//     read as another token. Add comment-stripping here if a future unit
//     file starts using that style.
//   - Systemd `\`-continuation lines are NOT joined; a directive split
//     across lines would lose tokens after the first line. Same caveat.
//
// Multiple directives with the same key in the same section are unioned
// (systemd-`After=` and `Before=` are themselves additive by spec).
func directiveTokens(t *testing.T, path, section, key string) map[string]bool {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	tokens := map[string]bool{}
	want := "[" + section + "]"
	prefix := key + "="
	inSection := false
	for _, raw := range strings.Split(string(data), "\n") {
		line := strings.TrimSpace(raw)
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			inSection = line == want
			continue
		}
		if !inSection || line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if v, ok := strings.CutPrefix(line, prefix); ok {
			for _, t := range strings.Fields(v) {
				tokens[t] = true
			}
		}
	}
	return tokens
}

// beforeTokens returns `Before=` tokens from the [Unit] section. Thin
// wrapper for readability at call sites.
func beforeTokens(t *testing.T, path string) map[string]bool {
	t.Helper()
	return directiveTokens(t, path, "Unit", "Before")
}

// afterTokens returns `After=` tokens from the [Unit] section.
func afterTokens(t *testing.T, path string) map[string]bool {
	t.Helper()
	return directiveTokens(t, path, "Unit", "After")
}

// wantedByTokens returns `WantedBy=` tokens from the [Install] section.
// Used to verify a unit is actually enabled into the boot graph (without
// it, the `Before=`/`After=` edges are unreachable code paths).
func wantedByTokens(t *testing.T, path string) map[string]bool {
	t.Helper()
	return directiveTokens(t, path, "Install", "WantedBy")
}

// TestFirecrackerFirstbootOrdering: the load-bearing line of PR #126.
// firecracker/shed-firstboot.service must order Before=ssh.service so
// per-shed SSH host keys are regenerated before sshd starts, and must
// NOT order before sysinit.target / shed-agent.service /
// network-setup.service — those edges were the broad gating that delayed
// shed-agent by firstboot's crng-blocked ssh-keygen and erased the ~20 %
// plain-create win. See docs/discovery/platform-runtime-optimization.md §14.
func TestFirecrackerFirstbootOrdering(t *testing.T) {
	const path = "../../firecracker/shed-firstboot.service"
	before := beforeTokens(t, path)
	if !before["ssh.service"] {
		t.Errorf("%s must order `Before=ssh.service` (preserves keygen-before-sshd; doc §14)", path)
	}
	for _, b := range []string{"sysinit.target", "shed-agent.service", "network-setup.service"} {
		if before[b] {
			t.Errorf("%s must NOT order `Before=%s` (would re-block shed-agent on firstboot; doc §14)", path, b)
		}
	}
}

// TestFirecrackerNetworkSetupGuardsAgent: PR #126 deliberately *kept*
// `Before=shed-agent.service` on FC's network-setup.service — that edge
// is safe because FC networking is static IP (fast, synchronous, no DHCP
// wait), and removing it without adding a host-side network-wait gate
// would re-create the --repo regression we measured on VZ (network
// readiness no longer overlapping boot; the host pays the wait serially
// before clone). See doc §14a / §14b / §14e.
func TestFirecrackerNetworkSetupGuardsAgent(t *testing.T) {
	const path = "../../firecracker/network-setup.service"
	before := beforeTokens(t, path)
	if !before["shed-agent.service"] {
		t.Errorf("%s must order `Before=shed-agent.service` so the agent has the network when clone/provisioning runs (FC static-IP guardrail; doc §14)", path)
	}
}

// TestVZFirstbootOrderingPreserved: PR #126 intentionally did NOT mirror
// the FC firstboot reorder to VZ — the same change on VZ exposes the
// DHCP wait (~1 s) on the --repo critical path and a) buys only ~150 ms
// on plain creates (fixed VMM/kernel overhead dominates) and b)
// regresses --repo creates by ~450 ms (network readiness no longer
// overlapping boot). A future "make platforms uniform" change must be
// rejected here. See doc §14a / §14c.
func TestVZFirstbootOrderingPreserved(t *testing.T) {
	const path = "../../vz/shed-firstboot.service"
	before := beforeTokens(t, path)
	for _, r := range []string{"sysinit.target", "ssh.service", "shed-agent.service", "network-setup.service"} {
		if !before[r] {
			t.Errorf("%s must order `Before=%s` (VZ retains broad firstboot gating; mirroring FC's PR #126 reorder here was measured to NOT help on VZ — doc §14a)", path, r)
		}
	}
}

// TestVZNetworkSetupGuardsAgent: PR #126 also rejected 3c (decoupling
// network-setup from shed-agent) on VZ — the doc §14a measurement showed
// it buys 0 ms (firstboot is the real gate, not DHCP) and regresses
// --repo. Lock the edge.
func TestVZNetworkSetupGuardsAgent(t *testing.T) {
	const path = "../../vz/network-setup.service"
	before := beforeTokens(t, path)
	if !before["shed-agent.service"] {
		t.Errorf("%s must order `Before=shed-agent.service` (PR #126 measured that removing this regresses --repo creates by ~450 ms and yields no plain-create win — doc §14a)", path)
	}
}

// TestShedAgentNotAfterFirstboot: shed-agent's only `After=` should be
// the passive `network.target`. Re-introducing `After=shed-firstboot.service`
// (or `After=network-online.target`) would re-block the agent on
// firstboot's crng-blocked ssh-keygen on FC (erasing PR #126's ~20 %
// win) or on systemd-networkd-wait-online (a different kind of gate
// that's even harder to debug). Each backend's agent unit is checked
// separately because their shapes are intentionally allowed to diverge.
// See docs/discovery/platform-runtime-optimization.md §14a / §14b.
func TestShedAgentNotAfterFirstboot(t *testing.T) {
	for _, tc := range []struct {
		name string
		path string
	}{
		{"firecracker", "../../firecracker/shed-agent.service"},
		{"vz", "../../vz/shed-agent.service"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			after := afterTokens(t, tc.path)
			for _, banned := range []string{"shed-firstboot.service", "network-online.target"} {
				if after[banned] {
					t.Errorf("%s must NOT order `After=%s` (would re-gate the agent on an earlier-boot unit; see docs/discovery/platform-runtime-optimization.md §14)", tc.path, banned)
				}
			}
		})
	}
}

// TestFirstbootInstalledIntoBootGraph: the firstboot units only enforce
// their `Before=` edges if they are actually pulled into the boot
// transaction. Both backends rely on `WantedBy=sysinit.target` for that.
// If a future change disables firstboot from the boot graph (e.g. by
// removing the [Install] section or changing WantedBy=), the per-shed
// host-key regeneration silently stops happening — a security regression
// far worse than the boot-ordering ones the rest of this file locks.
func TestFirstbootInstalledIntoBootGraph(t *testing.T) {
	for _, tc := range []struct {
		name string
		path string
	}{
		{"firecracker", "../../firecracker/shed-firstboot.service"},
		{"vz", "../../vz/shed-firstboot.service"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			wantedBy := wantedByTokens(t, tc.path)
			if !wantedBy["sysinit.target"] {
				t.Errorf("%s must declare `WantedBy=sysinit.target` in [Install] so the unit is actually enabled at boot; without it, per-shed SSH host keys would never be regenerated — every shed would serve the baked-in keys (see docs/discovery/platform-runtime-optimization.md §14e)", tc.path)
			}
		})
	}
}

// TestNetworkSetupInstalledIntoBootGraph: same logic as
// TestFirstbootInstalledIntoBootGraph but for network-setup. The
// `Before=shed-agent.service` guardrail this file locks is only
// meaningful when network-setup is pulled into the boot transaction;
// both backends do that via `WantedBy=multi-user.target`.
func TestNetworkSetupInstalledIntoBootGraph(t *testing.T) {
	for _, tc := range []struct {
		name string
		path string
	}{
		{"firecracker", "../../firecracker/network-setup.service"},
		{"vz", "../../vz/network-setup.service"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			wantedBy := wantedByTokens(t, tc.path)
			if !wantedBy["multi-user.target"] {
				t.Errorf("%s must declare `WantedBy=multi-user.target` in [Install] so the unit is actually enabled at boot; without it, the `Before=shed-agent.service` guardrail is unreachable and the agent could start before the network is configured (see docs/discovery/platform-runtime-optimization.md §14)", tc.path)
			}
		})
	}
}
