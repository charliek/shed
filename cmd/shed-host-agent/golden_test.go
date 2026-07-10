package main

// golden_test.go is the Go half of mechanism 2 (language-neutral golden fixtures)
// of the host-agent differential harness. It reads the SAME JSON vectors the Rust
// runner reads (crates/shed-host-agent/tests/golden.rs), builds the Go config /
// approval types, and asserts the pure decision functions (EffectivePolicy,
// desktopGateNamespaces) match every vector. The Go and Rust runners agreeing with
// a committed fixture is the drift guard the live differential cannot give — it
// catches "both impls wrong the same way".
//
// The fixtures live outside this package (tests/host-agent-diff/fixtures) on purpose
// so the neutral home is shared with the Rust runner; reading a data file outside
// the package is fine for a Go test and does not affect `go list ./...`.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// fixturesDir resolves the shared fixture directory relative to THIS source file,
// so the test works regardless of the process working directory.
func fixturesDir(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller(0) failed")
	}
	// cmd/shed-host-agent/golden_test.go -> repo root -> tests/host-agent-diff/fixtures
	root := filepath.Join(filepath.Dir(thisFile), "..", "..")
	return filepath.Join(root, "tests", "host-agent-diff", "fixtures")
}

func readFixture(t *testing.T, name string, into any) {
	t.Helper()
	path := filepath.Join(fixturesDir(t), name)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read fixture %s: %v", path, err)
	}
	if err := json.Unmarshal(data, into); err != nil {
		t.Fatalf("unmarshal fixture %s: %v", path, err)
	}
}

func equalStrings(a, b []string) bool {
	// Treat a nil slice and an empty slice as equal (desktopGateNamespaces returns
	// nil for the no-gate case; the fixture decodes `[]` to a non-nil empty slice).
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestGoldenEffectivePolicy(t *testing.T) {
	var fx struct {
		ProtocolVersion int `json:"protocol_version"`
		Vectors         []struct {
			Raw       string `json:"raw"`
			Effective string `json:"effective"`
		} `json:"vectors"`
	}
	readFixture(t, "effective_policy.json", &fx)

	if fx.ProtocolVersion != 2 {
		t.Fatalf("effective_policy.json protocol_version = %d, want 2 (version skew)", fx.ProtocolVersion)
	}
	if len(fx.Vectors) == 0 {
		t.Fatal("effective_policy.json has no vectors")
	}
	for _, v := range fx.Vectors {
		got := ApprovalConfig{Policy: v.Raw}.EffectivePolicy()
		if got != v.Effective {
			t.Errorf("EffectivePolicy(raw=%q) = %q, want %q", v.Raw, got, v.Effective)
		}
	}
}

func TestGoldenGateNamespaces(t *testing.T) {
	var fx struct {
		ProtocolVersion int `json:"protocol_version"`
		Vectors         []struct {
			SSH    string   `json:"ssh"`
			AWS    string   `json:"aws"`
			Docker string   `json:"docker"`
			Gate   []string `json:"gate"`
		} `json:"vectors"`
	}
	readFixture(t, "gate_namespaces.json", &fx)

	if fx.ProtocolVersion != 2 {
		t.Fatalf("gate_namespaces.json protocol_version = %d, want 2 (version skew)", fx.ProtocolVersion)
	}
	if len(fx.Vectors) == 0 {
		t.Fatal("gate_namespaces.json has no vectors")
	}
	for _, v := range fx.Vectors {
		cfg := Config{
			SSH:    SSHConfig{Approval: ApprovalConfig{Policy: v.SSH}},
			AWS:    AWSConfig{Approval: ApprovalConfig{Policy: v.AWS}},
			Docker: DockerConfig{Approval: ApprovalConfig{Policy: v.Docker}},
		}
		got := desktopGateNamespaces(cfg)
		if !equalStrings(got, v.Gate) {
			t.Errorf("desktopGateNamespaces(ssh=%q,aws=%q,docker=%q) = %v, want %v",
				v.SSH, v.AWS, v.Docker, got, v.Gate)
		}
	}
}
