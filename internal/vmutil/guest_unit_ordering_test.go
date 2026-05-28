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

// beforeTokens returns the union of every token that appears in a
// `Before=` directive inside the file's [Unit] section. Comment lines
// (starting with `#`) and directives outside [Unit] are ignored.
func beforeTokens(t *testing.T, path string) map[string]bool {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	tokens := map[string]bool{}
	inUnit := false
	for _, raw := range strings.Split(string(data), "\n") {
		line := strings.TrimSpace(raw)
		// Section header switches the inUnit gate.
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			inUnit = line == "[Unit]"
			continue
		}
		if !inUnit || line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if v, ok := strings.CutPrefix(line, "Before="); ok {
			for _, t := range strings.Fields(v) {
				tokens[t] = true
			}
		}
	}
	return tokens
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
