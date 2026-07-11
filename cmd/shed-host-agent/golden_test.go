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
	"reflect"
	"runtime"
	"testing"

	"github.com/charliek/shed/internal/ext/protocol"
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

// goldenTarget is the wire shape the load_discovered_servers golden compares (the same
// keys the Rust runner emits), so ServerTarget (no json tags) can be checked field-wise.
type goldenTarget struct {
	Name           string `json:"name"`
	URL            string `json:"url"`
	Token          string `json:"token"`
	TLSFingerprint string `json:"tls_fingerprint"`
	SSHHost        string `json:"ssh_host"`
	SSHPort        int    `json:"ssh_port"`
}

func TestGoldenLoadDiscoveredServers(t *testing.T) {
	var fx struct {
		ProtocolVersion int `json:"protocol_version"`
		Vectors         []struct {
			Name       string         `json:"name"`
			ConfigYAML string         `json:"config_yaml"`
			Expected   []goldenTarget `json:"expected"`
		} `json:"vectors"`
	}
	readFixture(t, "load_discovered_servers.json", &fx)

	if fx.ProtocolVersion != 1 {
		t.Fatalf("load_discovered_servers.json protocol_version = %d, want 1 (version skew)", fx.ProtocolVersion)
	}
	if len(fx.Vectors) == 0 {
		t.Fatal("load_discovered_servers.json has no vectors")
	}
	for _, v := range fx.Vectors {
		path := filepath.Join(t.TempDir(), "config.yaml")
		if err := os.WriteFile(path, []byte(v.ConfigYAML), 0o644); err != nil {
			t.Fatal(err)
		}
		targets, err := LoadDiscoveredServers(path)
		if err != nil {
			t.Errorf("%s: LoadDiscoveredServers: %v", v.Name, err)
			continue
		}
		got := make([]goldenTarget, 0, len(targets))
		for _, tgt := range targets {
			// ServerTarget and goldenTarget share field names+types (only the json
			// tags differ), so a struct conversion suffices (staticcheck S1016).
			got = append(got, goldenTarget(tgt))
		}
		gj, _ := json.Marshal(got)
		wj, _ := json.Marshal(v.Expected)
		if string(gj) != string(wj) {
			t.Errorf("%s: LoadDiscoveredServers =\n  %s\nwant\n  %s", v.Name, gj, wj)
		}
	}
}

// assertJSONShapeEqual marshals `got` (a built protocol.* struct), then unmarshals
// both the result and the fixture's raw `expected` into interface{} and compares them
// with reflect.DeepEqual — a parsed-value compare (key order insensitive), matching how
// the Rust runner compares serde_json::Value.
func assertJSONShapeEqual(t *testing.T, name string, got any, expected json.RawMessage) {
	t.Helper()
	gj, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("%s: marshal: %v", name, err)
	}
	var gotV, wantV any
	if err := json.Unmarshal(gj, &gotV); err != nil {
		t.Fatalf("%s: unmarshal got: %v", name, err)
	}
	if err := json.Unmarshal(expected, &wantV); err != nil {
		t.Fatalf("%s: unmarshal expected: %v", name, err)
	}
	if !reflect.DeepEqual(gotV, wantV) {
		t.Errorf("%s:\n  got  %s\n  want %s", name, gj, expected)
	}
}

// TestGoldenSSHPayloadShapes pins the four ssh-agent response payload shapes
// (internal/ext/protocol/ssh.go) against the SAME fixture the Rust in-crate runner
// (bus.rs:golden_ssh_payload_shapes) reads: tag names, the b64 pass-through of blobs,
// the always-present rest:"", and that an empty key list marshals as [] not null.
func TestGoldenSSHPayloadShapes(t *testing.T) {
	var fx struct {
		ProtocolVersion int `json:"protocol_version"`
		ListVectors     []struct {
			Name  string `json:"name"`
			Input struct {
				Keys []struct {
					Format  string `json:"format"`
					BlobB64 string `json:"blob_b64"`
					Comment string `json:"comment"`
				} `json:"keys"`
			} `json:"input"`
			Expected json.RawMessage `json:"expected"`
		} `json:"list_vectors"`
		SignVectors []struct {
			Name  string `json:"name"`
			Input struct {
				Format  string `json:"format"`
				BlobB64 string `json:"blob_b64"`
			} `json:"input"`
			Expected json.RawMessage `json:"expected"`
		} `json:"sign_vectors"`
		StatusVectors []struct {
			Name  string `json:"name"`
			Input struct {
				Mode     string `json:"mode"`
				KeyCount int    `json:"key_count"`
			} `json:"input"`
			Expected json.RawMessage `json:"expected"`
		} `json:"status_vectors"`
		ErrorVectors []struct {
			Name  string `json:"name"`
			Input struct {
				Error string `json:"error"`
				Code  string `json:"code"`
			} `json:"input"`
			Expected json.RawMessage `json:"expected"`
		} `json:"error_vectors"`
	}
	readFixture(t, "ssh_payload_shapes.json", &fx)

	if fx.ProtocolVersion != 1 {
		t.Fatalf("ssh_payload_shapes.json protocol_version = %d, want 1 (version skew)", fx.ProtocolVersion)
	}
	if len(fx.ListVectors) == 0 || len(fx.SignVectors) == 0 || len(fx.StatusVectors) == 0 || len(fx.ErrorVectors) == 0 {
		t.Fatal("ssh_payload_shapes.json missing vectors")
	}

	for _, v := range fx.ListVectors {
		keys := make([]protocol.SSHKeyInfo, 0, len(v.Input.Keys))
		for _, k := range v.Input.Keys {
			keys = append(keys, protocol.SSHKeyInfo{Format: k.Format, Blob: k.BlobB64, Comment: k.Comment})
		}
		assertJSONShapeEqual(t, "list/"+v.Name, protocol.SSHListResponse{Keys: keys}, v.Expected)
	}
	for _, v := range fx.SignVectors {
		got := protocol.SSHSignResponse{Format: v.Input.Format, Blob: v.Input.BlobB64, Rest: ""}
		assertJSONShapeEqual(t, "sign/"+v.Name, got, v.Expected)
	}
	for _, v := range fx.StatusVectors {
		got := protocol.SSHStatusResponse{Connected: true, Mode: v.Input.Mode, KeyCount: v.Input.KeyCount}
		assertJSONShapeEqual(t, "status/"+v.Name, got, v.Expected)
	}
	for _, v := range fx.ErrorVectors {
		got := protocol.SSHErrorResponse{Error: v.Input.Error, Code: v.Input.Code}
		assertJSONShapeEqual(t, "error/"+v.Name, got, v.Expected)
	}
}

// TestGoldenAWSResolve is the Go half of the aws_resolve golden. It routes each
// vector's config YAML through the PRODUCTION LoadConfig (so the same DefaultConfig
// defaulting + yaml merge the daemon uses is exercised), then asserts AWS.Resolve /
// AWS.Enabled / the applied load defaults against the shared fixture the Rust runner
// (config.rs:golden_aws_resolve) also reads.
func TestGoldenAWSResolve(t *testing.T) {
	var fx struct {
		ProtocolVersion int `json:"protocol_version"`
		Vectors         []struct {
			Name       string `json:"name"`
			ConfigYAML string `json:"config_yaml"`
			Queries    []struct {
				Server          string `json:"server"`
				Shed            string `json:"shed"`
				Role            string `json:"role"`
				Mode            string `json:"mode"`
				SessionDuration string `json:"session_duration"`
			} `json:"queries"`
			Enabled  bool `json:"enabled"`
			Defaults struct {
				SourceProfile      string `json:"source_profile"`
				SessionDuration    string `json:"session_duration"`
				CacheRefreshBefore string `json:"cache_refresh_before"`
			} `json:"defaults"`
		} `json:"vectors"`
	}
	readFixture(t, "aws_resolve.json", &fx)

	if fx.ProtocolVersion != 1 {
		t.Fatalf("aws_resolve.json protocol_version = %d, want 1 (version skew)", fx.ProtocolVersion)
	}
	if len(fx.Vectors) == 0 {
		t.Fatal("aws_resolve.json has no vectors")
	}
	for _, v := range fx.Vectors {
		path := filepath.Join(t.TempDir(), "config.yaml")
		if err := os.WriteFile(path, []byte(v.ConfigYAML), 0o644); err != nil {
			t.Fatal(err)
		}
		cfg, err := LoadConfig(path)
		if err != nil {
			t.Errorf("%s: LoadConfig: %v", v.Name, err)
			continue
		}
		if got := cfg.AWS.Enabled(); got != v.Enabled {
			t.Errorf("%s: Enabled() = %v, want %v", v.Name, got, v.Enabled)
		}
		if cfg.AWS.SourceProfile != v.Defaults.SourceProfile {
			t.Errorf("%s: source_profile = %q, want %q", v.Name, cfg.AWS.SourceProfile, v.Defaults.SourceProfile)
		}
		if cfg.AWS.SessionDuration != v.Defaults.SessionDuration {
			t.Errorf("%s: session_duration = %q, want %q", v.Name, cfg.AWS.SessionDuration, v.Defaults.SessionDuration)
		}
		if cfg.AWS.CacheRefreshBefore != v.Defaults.CacheRefreshBefore {
			t.Errorf("%s: cache_refresh_before = %q, want %q", v.Name, cfg.AWS.CacheRefreshBefore, v.Defaults.CacheRefreshBefore)
		}
		for _, q := range v.Queries {
			got := cfg.AWS.Resolve(q.Server, q.Shed)
			if got.Role != q.Role || got.Mode != q.Mode || got.SessionDuration != q.SessionDuration {
				t.Errorf("%s: Resolve(%q,%q) = {role:%q mode:%q dur:%q}, want {role:%q mode:%q dur:%q}",
					v.Name, q.Server, q.Shed, got.Role, got.Mode, got.SessionDuration, q.Role, q.Mode, q.SessionDuration)
			}
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
