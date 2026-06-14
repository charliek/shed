package config

import "testing"

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
