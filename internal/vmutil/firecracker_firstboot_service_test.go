package vmutil

import (
	"os"
	"strings"
	"testing"
)

// TestFirecrackerFirstbootOrdering locks in the security-critical systemd
// ordering for firecracker/shed-firstboot.service. The unit regenerates
// per-shed SSH host keys via `ssh-keygen -A`; if its `Before=ssh.service`
// edge is removed, sshd may start before the keys are regenerated and every
// shed would serve the same baked-in host keys — a security regression.
//
// The test also explicitly bans the directives that were removed in
// PR #126's firstboot reorder (Before=sysinit.target / shed-agent.service /
// network-setup.service). Reintroducing any of them would re-block
// shed-agent on firstboot's crng-blocked `ssh-keygen` — see
// docs/discovery/platform-runtime-optimization.md §14.
//
// Note: this asserts the *ordering edge*, not failure-mode propagation.
// `Before=` makes sshd start AFTER firstboot finishes, regardless of
// firstboot's exit status; if a future change wants sshd to fail-closed
// when firstboot fails, that's a separate `Requires=`/`BindsTo=` edge
// (intentionally not in this PR — same failure mode as shipped).
func TestFirecrackerFirstbootOrdering(t *testing.T) {
	const path = "../../firecracker/shed-firstboot.service"
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}

	var foundBeforeSsh bool
	// Tokens that must NOT appear inside any [Unit] Before= directive on FC.
	// Listing them here ensures any reintroduction shows up as a clear test
	// failure with the rationale link, not a silent perf+correctness regression.
	banned := []string{
		"sysinit.target",
		"shed-agent.service",
		"network-setup.service",
	}

	for _, raw := range strings.Split(string(data), "\n") {
		line := strings.TrimSpace(raw)
		if line == "Before=ssh.service" {
			foundBeforeSsh = true
		}
		if !strings.HasPrefix(line, "Before=") {
			continue
		}
		for _, b := range banned {
			if strings.Contains(line, b) {
				t.Fatalf("%s `Before=` must not include %q (would re-block shed-agent on firstboot; see docs/discovery/platform-runtime-optimization.md §14): %q",
					path, b, line)
			}
		}
	}

	if !foundBeforeSsh {
		t.Fatalf("%s must contain a `Before=ssh.service` directive — preserves the per-shed host-key invariant; see docs/discovery/platform-runtime-optimization.md §14", path)
	}
}
