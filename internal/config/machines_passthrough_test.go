package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// TestMachinesSectionSurvivesUpdate pins the passthrough contract: a `machines:`
// section (owned by the Rust porcelain, opaque to Go) must survive the
// whole-document rewrite that Update/SaveToPath perform. Before the passthrough
// field existed, any config-updating `shed` command silently DELETED the section
// — user data loss, not a hypothetical.
func TestMachinesSectionSurvivesUpdate(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	original := `servers:
    my-server:
        host: localhost
        http_port: 8080
        ssh_port: 2222
default_server: my-server
machines:
    mini2:
        host: mini2.local
        user: charliek
        ssh_port: 22
        rc_bin: /opt/homebrew/bin/shed-machine-rc
    laptop:
        host: laptop.tailnet.ts.net
`
	if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadClientConfigFromPath(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	// A representative config-updating operation: mutate an unrelated field the
	// way `shed`'s cache refresh / server add paths do.
	if err := cfg.Update(func(c *ClientConfig) {
		c.DefaultServer = "my-server"
	}); err != nil {
		t.Fatalf("update: %v", err)
	}

	rewritten, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := yaml.Unmarshal(rewritten, &got); err != nil {
		t.Fatalf("re-parse rewritten config: %v", err)
	}
	var want map[string]any
	if err := yaml.Unmarshal([]byte(original), &want); err != nil {
		t.Fatal(err)
	}
	if got["machines"] == nil {
		t.Fatalf("machines section deleted by Update; rewritten config:\n%s", rewritten)
	}
	if !reflect.DeepEqual(got["machines"], want["machines"]) {
		t.Fatalf("machines subtree changed across Update:\n got: %#v\nwant: %#v", got["machines"], want["machines"])
	}
}

// TestNoMachinesSectionStaysAbsent pins the omitempty half: a config that never
// had a `machines:` key must not grow an empty/null one from the passthrough
// field's zero value.
func TestNoMachinesSectionStaysAbsent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("servers:\n    a:\n        host: h\ndefault_server: a\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadClientConfigFromPath(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if err := cfg.Update(func(c *ClientConfig) { c.DefaultServer = "a" }); err != nil {
		t.Fatalf("update: %v", err)
	}
	rewritten, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(rewritten), "machines") {
		t.Fatalf("machines key materialized from zero value:\n%s", rewritten)
	}
}
