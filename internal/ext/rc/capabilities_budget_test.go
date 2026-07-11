package rc

import (
	"testing"
	"time"
)

// TestBuildCapabilities_SlowProbeDegradesWithinBudget: one hung agent probe (a
// slow `--version`) must not stall capabilities assembly — the laggard degrades
// to the fast installed-only result (version omitted), everything else keeps its
// full result, and the whole call returns within the shared probeBudget (plus
// slack), well under the server's ~2s enrichment exec timeout.
func TestBuildCapabilities_SlowProbeDegradesWithinBudget(t *testing.T) {
	release := make(chan struct{})
	defer close(release) // unblock the parked goroutine at test end
	probe := func(bin string) AgentInfo {
		if bin == "codex" {
			<-release // hung --version: outruns the probe budget
			return AgentInfo{Installed: true, Version: "9.9.9"}
		}
		return AgentInfo{Installed: true, Version: "1.0.0"}
	}
	installed := func(bin string) bool { return bin == "codex" }

	start := time.Now()
	caps := BuildCapabilities(probe, installed)
	elapsed := time.Since(start)

	// Fast overall: the budget plus generous slack, never near the server's 2s
	// enrichment timeout.
	if elapsed > probeBudget+500*time.Millisecond {
		t.Fatalf("BuildCapabilities took %v; must return within the %v probe budget", elapsed, probeBudget)
	}

	// The laggard degrades: installed from the fast check, version omitted.
	codex, ok := caps.Agents["codex"]
	if !ok {
		t.Fatal("codex missing from agents")
	}
	if !codex.Installed {
		t.Errorf("laggard codex should report installed=true from the fast check: %+v", codex)
	}
	if codex.Version != "" {
		t.Errorf("laggard codex must omit version, got %q", codex.Version)
	}

	// Prompt probes keep their full result.
	for _, tool := range []string{"claude", "opencode", "cursor"} {
		info, ok := caps.Agents[tool]
		if !ok {
			t.Fatalf("%s missing from agents", tool)
		}
		if !info.Installed || info.Version != "1.0.0" {
			t.Errorf("%s should keep its full probe result, got %+v", tool, info)
		}
	}
}

// TestBuildCapabilities_HungInstalledProbeDegradesWithinBudget: the fast
// installed-only fallback runs concurrently inside the same budgeted flight —
// when BOTH the full probe and the installed check hang (e.g. a stuck login
// shell), assembly still returns within the budget and the agent degrades to
// installed:false. The fallback must never run synchronously after expiry.
func TestBuildCapabilities_HungInstalledProbeDegradesWithinBudget(t *testing.T) {
	release := make(chan struct{})
	defer close(release) // unblock all parked goroutines at test end
	probe := func(bin string) AgentInfo {
		if bin == "codex" {
			<-release // hung --version
			return AgentInfo{Installed: true, Version: "9.9.9"}
		}
		return AgentInfo{Installed: true, Version: "1.0.0"}
	}
	installed := func(bin string) bool {
		if bin == "codex" {
			<-release // hung login shell: the fast check is stuck too
			return true
		}
		return true
	}

	start := time.Now()
	caps := BuildCapabilities(probe, installed)
	elapsed := time.Since(start)

	if elapsed > probeBudget+500*time.Millisecond {
		t.Fatalf("BuildCapabilities took %v; must return within the %v probe budget even with a hung installed probe", elapsed, probeBudget)
	}
	if info := caps.Agents["codex"]; info.Installed || info.Version != "" {
		t.Errorf("hung full+installed probes must degrade to installed:false, got %+v", info)
	}
	// The other agents keep their full results.
	for _, tool := range []string{"claude", "opencode", "cursor"} {
		if info := caps.Agents[tool]; !info.Installed || info.Version != "1.0.0" {
			t.Errorf("%s should keep its full probe result, got %+v", tool, info)
		}
	}
}

// TestBuildCapabilities_NilInstalledFallback: with no fast fallback injected, a
// budget-exhausted agent reports not-installed (never blocks).
func TestBuildCapabilities_NilInstalledFallback(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	probe := func(bin string) AgentInfo {
		if bin == "codex" {
			<-release
		}
		return AgentInfo{Installed: true, Version: "1.0.0"}
	}
	caps := BuildCapabilities(probe, nil)
	if info := caps.Agents["codex"]; info.Installed || info.Version != "" {
		t.Errorf("nil fallback laggard should be zero-valued, got %+v", info)
	}
}
