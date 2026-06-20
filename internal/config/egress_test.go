package config

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func mkEgress() *EgressConfig {
	return &EgressConfig{
		Enabled:   true,
		PortRange: "20000-30000",
		Default:   []string{"audit"},
		Profiles: map[string]EgressProfile{
			"audit":  {Mode: "audit"},
			"github": {Allow: []string{"*.github.com"}},
			"corp":   {Rule: `host.endsWith(".corp.internal")`},
		},
	}
}

func TestEgressConfigValidate(t *testing.T) {
	if err := (*EgressConfig)(nil).Validate(); err != nil {
		t.Errorf("nil egress should validate: %v", err)
	}
	if err := (&EgressConfig{Enabled: false, Profiles: map[string]EgressProfile{"x": {Rule: "garbage ++"}}}).Validate(); err != nil {
		t.Errorf("disabled egress should skip validation: %v", err)
	}
	if err := mkEgress().Validate(); err != nil {
		t.Errorf("valid egress failed: %v", err)
	}
	bad := []struct {
		name string
		cfg  *EgressConfig
	}{
		{"reserved name", &EgressConfig{Enabled: true, Profiles: map[string]EgressProfile{"off": {Mode: "audit"}}}},
		{"unknown default", &EgressConfig{Enabled: true, Default: []string{"nope"}, Profiles: map[string]EgressProfile{}}},
		{"bad cel", &EgressConfig{Enabled: true, Profiles: map[string]EgressProfile{"x": {Rule: "host ++ &&"}}}},
		{"bad port range", &EgressConfig{Enabled: true, PortRange: "abc"}},
		{"reserved default", &EgressConfig{Enabled: true, Default: []string{"off"}, Profiles: map[string]EgressProfile{}}},
	}
	for _, b := range bad {
		if err := b.cfg.Validate(); err == nil {
			t.Errorf("%s: expected validation error", b.name)
		}
	}
}

func TestEgressResolveProfiles(t *testing.T) {
	c := mkEgress()
	// disabled → nil
	if specs, _ := (&EgressConfig{Enabled: false}).ResolveProfiles(nil); specs != nil {
		t.Error("disabled should resolve to nil")
	}
	// empty req → default ([audit])
	specs, err := c.ResolveProfiles(nil)
	if err != nil || len(specs) != 1 || specs[0].Mode != "audit" {
		t.Errorf("default resolve = %v, %v", specs, err)
	}
	// explicit list composes
	specs, err = c.ResolveProfiles([]string{"github", "corp"})
	if err != nil || len(specs) != 2 {
		t.Errorf("compose resolve = %v, %v", specs, err)
	}
	// "off" disables
	if specs, _ := c.ResolveProfiles([]string{"off"}); specs != nil {
		t.Error(`["off"] should resolve to nil`)
	}
	// unknown profile errors
	if _, err := c.ResolveProfiles([]string{"ghost"}); err == nil {
		t.Error("unknown profile should error")
	}
	// absent default (empty) → nil
	if specs, _ := (&EgressConfig{Enabled: true}).ResolveProfiles(nil); specs != nil {
		t.Error("empty default should resolve to nil (no egress)")
	}
}

func TestEgressPortRangeBounds(t *testing.T) {
	lo, hi := mkEgress().PortRangeBounds()
	if lo != 20000 || hi != 30000 {
		t.Errorf("bounds = %d-%d", lo, hi)
	}
	lo, hi = (&EgressConfig{}).PortRangeBounds()
	if lo != 20000 || hi != 30000 {
		t.Errorf("default bounds = %d-%d", lo, hi)
	}
}

// TestEgressProfileJSONContract pins the `shed egress show --json` rules-map wire
// shape: snake_case keys, omitempty dropping unset fields. The raw-bytes
// assertions are deliberate — a struct round-trip would pass even without the
// json tags because encoding/json matches field names case-insensitively.
func TestEgressProfileJSONContract(t *testing.T) {
	cases := []struct {
		name string
		p    EgressProfile
		want string
	}{
		{"allow only", EgressProfile{Allow: []string{"*.github.com", "github.com"}}, `{"allow":["*.github.com","github.com"]}`},
		{"audit mode", EgressProfile{Mode: "audit"}, `{"mode":"audit"}`},
		{"rule only", EgressProfile{Rule: "port == 443"}, `{"rule":"port == 443"}`},
		{"deny+allow", EgressProfile{Deny: []string{"evil.com"}, Allow: []string{"good.com"}}, `{"allow":["good.com"],"deny":["evil.com"]}`},
	}
	for _, c := range cases {
		b, err := json.Marshal(c.p)
		if err != nil {
			t.Fatalf("%s: marshal: %v", c.name, err)
		}
		if string(b) != c.want {
			t.Errorf("%s: marshal = %s, want %s", c.name, b, c.want)
		}
	}

	// No PascalCase key leaks (this is what the json tags actually fix).
	b, _ := json.Marshal(EgressProfile{Allow: []string{"x"}})
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"Mode", "Allow", "Deny", "Rule"} {
		if _, ok := m[bad]; ok {
			t.Errorf("PascalCase key %q leaked into %s", bad, b)
		}
	}
	if _, ok := m["allow"]; !ok {
		t.Errorf("missing snake_case key 'allow' in %s", b)
	}

	// Backward-compat: a legacy PascalCase document still unmarshals, so a new CLI
	// reading an old server's response (and vice-versa) keeps working.
	var p EgressProfile
	if err := json.Unmarshal([]byte(`{"Mode":"audit","Allow":["a.com"]}`), &p); err != nil {
		t.Fatal(err)
	}
	if p.Mode != "audit" || len(p.Allow) != 1 || p.Allow[0] != "a.com" {
		t.Errorf("legacy unmarshal = %+v", p)
	}
}

// TestEgressStatusJSONKeys guards the full GET /api/egress/{name} response shape:
// the nested rules-map profile values must be snake_case, like the rest of it.
func TestEgressStatusJSONKeys(t *testing.T) {
	st := EgressStatus{
		Shed:     "web",
		Enabled:  true,
		Profiles: []string{"github"},
		Port:     20001,
		Rules:    map[string]EgressProfile{"github": {Allow: []string{"github.com"}}},
	}
	b, err := json.Marshal(st)
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	for _, want := range []string{`"shed":"web"`, `"rules":{"github":{"allow":["github.com"]}}`} {
		if !strings.Contains(s, want) {
			t.Errorf("EgressStatus json missing %s in %s", want, s)
		}
	}
	if strings.Contains(s, `"Allow"`) || strings.Contains(s, `"Mode"`) {
		t.Errorf("PascalCase profile key leaked: %s", s)
	}
}

// TestExampleConfigEgressValidates guards configs/server.example.yaml from
// rotting: its starter egress profiles must compile (globs/CEL) so copy-paste
// users get a config that loads. The example ships enabled:false, so validate a
// force-enabled copy to actually exercise profile compilation (Validate is a
// no-op when disabled).
func TestExampleConfigEgressValidates(t *testing.T) {
	data, err := os.ReadFile("../../configs/server.example.yaml")
	if err != nil {
		t.Fatalf("read example config: %v", err)
	}
	var cfg ServerConfig
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		t.Fatalf("parse example config: %v", err)
	}
	if cfg.Egress == nil {
		t.Fatal("example config has no egress block (expected the documented starter profiles)")
	}
	e := *cfg.Egress
	e.Enabled = true
	if err := e.Validate(); err != nil {
		t.Errorf("example egress profiles do not validate: %v", err)
	}
}
