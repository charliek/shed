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
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"testing"
	"time"

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

// TestGoldenAWSExpiry is the Go half of the aws_expiry golden. It routes each vector
// through the PRODUCTION expiry scan (parseSessionExpiry -> parseExpiryValue), the
// handler's expiration Format layout ("2006-01-02T15:04:05Z", the literal-Z render),
// and awsExpiryDetail, against the shared fixture the Rust runner
// (aws_backend.rs:golden_aws_expiry) also reads.
func TestGoldenAWSExpiry(t *testing.T) {
	var fx struct {
		ProtocolVersion int `json:"protocol_version"`
		ExpiryVectors   []struct {
			Name         string `json:"name"`
			INI          string `json:"ini"`
			Profile      string `json:"profile"`
			ExpectedUnix *int64 `json:"expected_unix"`
		} `json:"expiry_vectors"`
		LiteralZVectors []struct {
			Unix     int64  `json:"unix"`
			Expected string `json:"expected"`
		} `json:"literal_z_vectors"`
		ExpiryDetailVectors []struct {
			Unix     *int64 `json:"unix"`
			Expected string `json:"expected"`
		} `json:"expiry_detail_vectors"`
	}
	readFixture(t, "aws_expiry.json", &fx)

	if fx.ProtocolVersion != 1 {
		t.Fatalf("aws_expiry.json protocol_version = %d, want 1 (version skew)", fx.ProtocolVersion)
	}
	if len(fx.ExpiryVectors) == 0 || len(fx.LiteralZVectors) == 0 || len(fx.ExpiryDetailVectors) == 0 {
		t.Fatal("aws_expiry.json missing vectors")
	}

	for _, v := range fx.ExpiryVectors {
		path := filepath.Join(t.TempDir(), "credentials")
		if err := os.WriteFile(path, []byte(v.INI), 0o600); err != nil {
			t.Fatal(err)
		}
		got := parseSessionExpiry(path, v.Profile)
		switch {
		case v.ExpectedUnix == nil:
			if !got.IsZero() {
				t.Errorf("%s: expected zero time, got %v", v.Name, got)
			}
		case got.IsZero():
			t.Errorf("%s: expected unix %d, got zero time", v.Name, *v.ExpectedUnix)
		case got.Unix() != *v.ExpectedUnix:
			t.Errorf("%s: got unix %d, want %d", v.Name, got.Unix(), *v.ExpectedUnix)
		}
	}
	for _, v := range fx.LiteralZVectors {
		got := time.Unix(v.Unix, 0).UTC().Format("2006-01-02T15:04:05Z")
		if got != v.Expected {
			t.Errorf("literal_z(%d) = %q, want %q", v.Unix, got, v.Expected)
		}
	}
	for _, v := range fx.ExpiryDetailVectors {
		var exp time.Time // zero value => awsExpiryDetail returns "expires:none"
		if v.Unix != nil {
			exp = time.Unix(*v.Unix, 0).UTC()
		}
		if got := awsExpiryDetail(exp); got != v.Expected {
			t.Errorf("awsExpiryDetail(unix=%v) = %q, want %q", v.Unix, got, v.Expected)
		}
	}
}

// TestGoldenDockerResolve is the Go half of the docker_resolve golden. It routes each
// vector's config YAML through the PRODUCTION LoadConfig (so the same DefaultConfig
// defaulting + yaml merge the daemon uses is exercised — Docker gets NO load-defaults,
// unlike AWS), then asserts Docker.Resolve's allow_all + registries + registry_count
// against the shared fixture the Rust runner (config.rs:golden_docker_resolve) also
// reads. Go has no Resolve layering test, so this golden is the drift guard for the
// Option<Vec<String>> replace / Option<bool> force / flow-list-parse semantics.
func TestGoldenDockerResolve(t *testing.T) {
	var fx struct {
		ProtocolVersion int `json:"protocol_version"`
		Vectors         []struct {
			Name       string `json:"name"`
			ConfigYAML string `json:"config_yaml"`
			Queries    []struct {
				Server        string   `json:"server"`
				Shed          string   `json:"shed"`
				AllowAll      bool     `json:"allow_all"`
				Registries    []string `json:"registries"`
				RegistryCount int      `json:"registry_count"`
			} `json:"queries"`
		} `json:"vectors"`
	}
	readFixture(t, "docker_resolve.json", &fx)

	if fx.ProtocolVersion != 1 {
		t.Fatalf("docker_resolve.json protocol_version = %d, want 1 (version skew)", fx.ProtocolVersion)
	}
	if len(fx.Vectors) == 0 {
		t.Fatal("docker_resolve.json has no vectors")
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
		for _, q := range v.Queries {
			got := cfg.Docker.Resolve(q.Server, q.Shed)
			if got.AllowAll != q.AllowAll {
				t.Errorf("%s: Resolve(%q,%q).AllowAll = %v, want %v",
					v.Name, q.Server, q.Shed, got.AllowAll, q.AllowAll)
			}
			// equalStrings treats a nil slice and an empty slice as equal — Go's
			// Resolve returns a nil Registries for the inherited-empty case, the
			// fixture decodes `[]` to a non-nil empty slice.
			if !equalStrings(got.Registries, q.Registries) {
				t.Errorf("%s: Resolve(%q,%q).Registries = %v, want %v",
					v.Name, q.Server, q.Shed, got.Registries, q.Registries)
			}
			if len(got.Registries) != q.RegistryCount {
				t.Errorf("%s: Resolve(%q,%q) registry_count = %d, want %d",
					v.Name, q.Server, q.Shed, len(got.Registries), q.RegistryCount)
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

// TestGoldenDockerNormalize is the Go half of the docker_normalize golden. It routes
// each vector through the PRODUCTION normalizeRegistry + lookupConfigMap (the same
// canonicalization + 3-way config-map search the backend uses), against the shared
// fixture the Rust runner (docker_backend.rs:tests::golden_docker_normalize) also
// reads, so the one-occurrence strip / strip-order / raw->normalized->scan lookup
// semantics cannot drift together.
func TestGoldenDockerNormalize(t *testing.T) {
	var fx struct {
		ProtocolVersion  int `json:"protocol_version"`
		NormalizeVectors []struct {
			Input    string `json:"input"`
			Expected string `json:"expected"`
		} `json:"normalize_vectors"`
		LookupVectors []struct {
			Name     string            `json:"name"`
			Map      map[string]string `json:"map"`
			Raw      string            `json:"raw"`
			Expected *string           `json:"expected"`
		} `json:"lookup_vectors"`
	}
	readFixture(t, "docker_normalize.json", &fx)

	if fx.ProtocolVersion != 1 {
		t.Fatalf("docker_normalize.json protocol_version = %d, want 1 (version skew)", fx.ProtocolVersion)
	}
	if len(fx.NormalizeVectors) == 0 || len(fx.LookupVectors) == 0 {
		t.Fatal("docker_normalize.json missing vectors")
	}

	for _, v := range fx.NormalizeVectors {
		if got := normalizeRegistry(v.Input); got != v.Expected {
			t.Errorf("normalizeRegistry(%q) = %q, want %q", v.Input, got, v.Expected)
		}
	}
	for _, v := range fx.LookupVectors {
		normalized := normalizeRegistry(v.Raw)
		got, ok := lookupConfigMap(v.Map, v.Raw, normalized)
		switch {
		case v.Expected == nil:
			if ok {
				t.Errorf("%s: lookupConfigMap(raw=%q) = %q, want miss", v.Name, v.Raw, got)
			}
		case !ok:
			t.Errorf("%s: lookupConfigMap(raw=%q) = miss, want %q", v.Name, v.Raw, *v.Expected)
		case got != *v.Expected:
			t.Errorf("%s: lookupConfigMap(raw=%q) = %q, want %q", v.Name, v.Raw, got, *v.Expected)
		}
	}
}

// TestGoldenDockerInlineAuth is the Go half of the docker_inline_auth golden. It
// base64-STANDARD-encodes each vector's `plain` (matching the Rust runner's own
// STANDARD encode), routes it through the PRODUCTION decodeInlineAuth, and asserts the
// username/secret split (valid) or the exact error string (invalid). The
// malformed-base64 case is intentionally absent (its runtime suffix is impl-dependent
// — see the fixture comment).
func TestGoldenDockerInlineAuth(t *testing.T) {
	var fx struct {
		ProtocolVersion int `json:"protocol_version"`
		ValidVectors    []struct {
			Name      string `json:"name"`
			ServerURL string `json:"server_url"`
			Plain     string `json:"plain"`
			Username  string `json:"username"`
			Secret    string `json:"secret"`
		} `json:"valid_vectors"`
		InvalidVectors []struct {
			Name          string `json:"name"`
			ServerURL     string `json:"server_url"`
			Plain         string `json:"plain"`
			ExpectedError string `json:"expected_error"`
		} `json:"invalid_vectors"`
	}
	readFixture(t, "docker_inline_auth.json", &fx)

	if fx.ProtocolVersion != 1 {
		t.Fatalf("docker_inline_auth.json protocol_version = %d, want 1 (version skew)", fx.ProtocolVersion)
	}
	if len(fx.ValidVectors) == 0 || len(fx.InvalidVectors) == 0 {
		t.Fatal("docker_inline_auth.json missing vectors")
	}

	for _, v := range fx.ValidVectors {
		encoded := base64.StdEncoding.EncodeToString([]byte(v.Plain))
		cred, err := decodeInlineAuth(v.ServerURL, encoded)
		if err != nil {
			t.Errorf("%s: unexpected error: %v", v.Name, err)
			continue
		}
		if cred.Username != v.Username || cred.Secret != v.Secret {
			t.Errorf("%s: decodeInlineAuth = {user:%q secret:%q}, want {user:%q secret:%q}",
				v.Name, cred.Username, cred.Secret, v.Username, v.Secret)
		}
		if cred.ServerURL != v.ServerURL {
			t.Errorf("%s: ServerURL = %q, want %q", v.Name, cred.ServerURL, v.ServerURL)
		}
	}
	for _, v := range fx.InvalidVectors {
		encoded := base64.StdEncoding.EncodeToString([]byte(v.Plain))
		_, err := decodeInlineAuth(v.ServerURL, encoded)
		if err == nil {
			t.Errorf("%s: expected error, got nil", v.Name)
			continue
		}
		if err.Error() != v.ExpectedError {
			t.Errorf("%s: error = %q, want %q", v.Name, err.Error(), v.ExpectedError)
		}
	}
}

// TestGoldenDockerPathAugment is the Go half of the docker_path_augment golden. It
// routes each vector through the PRODUCTION augmentPATH (over os.Environ()-shaped
// []string) and extracts the effective (last-wins) PATH value via pathValue, against
// the shared fixture the Rust runner (docker_backend.rs:tests::golden_docker_path_augment)
// also reads. go_only vectors (multiple PATH= entries / no PATH= entry) exercise the
// Go []string-env branches the Rust map-env has no equivalent for; the Rust runner
// skips them.
func TestGoldenDockerPathAugment(t *testing.T) {
	var fx struct {
		ProtocolVersion int `json:"protocol_version"`
		Vectors         []struct {
			Name         string   `json:"name"`
			GoOnly       bool     `json:"go_only"`
			Path         string   `json:"path"`
			Env          []string `json:"env"`
			ExtraDirs    []string `json:"extra_dirs"`
			ExpectedPath string   `json:"expected_path"`
		} `json:"vectors"`
	}
	readFixture(t, "docker_path_augment.json", &fx)

	if fx.ProtocolVersion != 1 {
		t.Fatalf("docker_path_augment.json protocol_version = %d, want 1 (version skew)", fx.ProtocolVersion)
	}
	if len(fx.Vectors) == 0 {
		t.Fatal("docker_path_augment.json has no vectors")
	}

	for _, v := range fx.Vectors {
		env := v.Env
		if env == nil {
			env = []string{"PATH=" + v.Path}
		}
		got := augmentPATH(env, v.ExtraDirs)
		if pv := pathValue(t, got); pv != v.ExpectedPath {
			t.Errorf("%s: augmentPATH PATH = %q, want %q", v.Name, pv, v.ExpectedPath)
		}
	}
}
