package rc

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func envFunc(vals map[string]string) Getenv {
	return func(k string) string { return vals[k] }
}

func readConfig(t *testing.T, path string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("config is not valid JSON: %v", err)
	}
	return m
}

func projectTrusted(m map[string]any, workdir string) bool {
	projects, _ := m["projects"].(map[string]any)
	proj, _ := projects[workdir].(map[string]any)
	v, _ := proj["hasTrustDialogAccepted"].(bool)
	return v
}

func TestClaudeConfigPath(t *testing.T) {
	cases := []struct {
		env  map[string]string
		want string
	}{
		{map[string]string{"CLAUDE_CONFIG_DIR": "/cfg", "HOME": "/home/x"}, "/cfg/.claude.json"},
		{map[string]string{"CLAUDE_CONFIG_DIR": "", "HOME": "/home/x"}, "/home/x/.claude.json"}, // empty → HOME
		{map[string]string{"HOME": "/home/x"}, "/home/x/.claude.json"},
		{map[string]string{}, ""},
	}
	for _, c := range cases {
		if got := claudeConfigPath(envFunc(c.env)); got != c.want {
			t.Errorf("claudeConfigPath(%v) = %q, want %q", c.env, got, c.want)
		}
	}
}

func TestPreseedClaudeConfigCreatesFile(t *testing.T) {
	home := t.TempDir()
	if err := PreseedClaudeConfig("/home/shed/proj", envFunc(map[string]string{"HOME": home})); err != nil {
		t.Fatal(err)
	}
	m := readConfig(t, filepath.Join(home, ".claude.json"))
	if !projectTrusted(m, "/home/shed/proj") {
		t.Fatalf("workdir not marked trusted: %v", m)
	}
}

func TestPreseedClaudeConfigClearsOnboarding(t *testing.T) {
	home := t.TempDir()
	if err := PreseedClaudeConfig("/home/shed/proj", envFunc(map[string]string{"HOME": home})); err != nil {
		t.Fatal(err)
	}
	m := readConfig(t, filepath.Join(home, ".claude.json"))
	if m["hasCompletedOnboarding"] != true {
		t.Fatalf("hasCompletedOnboarding not set: %v", m["hasCompletedOnboarding"])
	}
	if m["theme"] != "dark" {
		t.Fatalf("default theme not set: %v", m["theme"])
	}
}

func TestPreseedClaudeConfigSuppressesInterstitials(t *testing.T) {
	home := t.TempDir()
	if err := PreseedClaudeConfig("/home/shed/proj", envFunc(map[string]string{"HOME": home})); err != nil {
		t.Fatal(err)
	}
	m := readConfig(t, filepath.Join(home, ".claude.json"))
	if n, _ := m["fullscreenUpsellSeenCount"].(float64); int(n) < fullscreenUpsellFloor {
		t.Fatalf("fullscreenUpsellSeenCount not raised: %v", m["fullscreenUpsellSeenCount"])
	}
	if m["hasSeenAutoModeEntryWarning"] != true {
		t.Fatalf("hasSeenAutoModeEntryWarning not set: %v", m["hasSeenAutoModeEntryWarning"])
	}
}

func TestPreseedClaudeConfigDoesNotLowerUpsellCount(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join(home, ".claude.json")
	data, _ := json.Marshal(map[string]any{"fullscreenUpsellSeenCount": 100000})
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := PreseedClaudeConfig("/home/shed/proj", envFunc(map[string]string{"HOME": home})); err != nil {
		t.Fatal(err)
	}
	if got, _ := readConfig(t, path)["fullscreenUpsellSeenCount"].(float64); int(got) != 100000 {
		t.Fatalf("existing higher count was lowered to %v", got)
	}
}

func TestPreseedClaudeConfigDoesNotClobberTheme(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join(home, ".claude.json")
	data, _ := json.Marshal(map[string]any{"theme": "light"})
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := PreseedClaudeConfig("/home/shed/proj", envFunc(map[string]string{"HOME": home})); err != nil {
		t.Fatal(err)
	}
	m := readConfig(t, path)
	if m["theme"] != "light" {
		t.Fatalf("existing theme clobbered: %v", m["theme"])
	}
	if m["hasCompletedOnboarding"] != true {
		t.Fatal("hasCompletedOnboarding not set")
	}
}

func TestPreseedClaudeConfigPreservesUnrelatedKeys(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join(home, ".claude.json")
	// Pre-existing config with OAuth/MCP-like state and another project.
	seed := map[string]any{
		"oauthAccount": map[string]any{"emailAddress": "x@y.z"},
		"mcpServers":   map[string]any{"foo": map[string]any{"command": "bar"}},
		"projects": map[string]any{
			"/other": map[string]any{"hasTrustDialogAccepted": true, "customKey": 7.0},
		},
	}
	data, _ := json.Marshal(seed)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}

	if err := PreseedClaudeConfig("/home/shed/proj", envFunc(map[string]string{"HOME": home})); err != nil {
		t.Fatal(err)
	}

	m := readConfig(t, path)
	if _, ok := m["oauthAccount"]; !ok {
		t.Error("oauthAccount dropped")
	}
	if _, ok := m["mcpServers"]; !ok {
		t.Error("mcpServers dropped")
	}
	if !projectTrusted(m, "/other") {
		t.Error("existing /other project trust dropped")
	}
	if !projectTrusted(m, "/home/shed/proj") {
		t.Error("new project not trusted")
	}
	// The unrelated nested key under /other survives.
	projects, _ := m["projects"].(map[string]any)
	other, _ := projects["/other"].(map[string]any)
	if other["customKey"] != 7.0 {
		t.Errorf("nested customKey not preserved: %v", other["customKey"])
	}
}

func TestPreseedClaudeConfigLeavesMalformedUntouched(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join(home, ".claude.json")
	garbage := []byte("{ this is not json")
	if err := os.WriteFile(path, garbage, 0o600); err != nil {
		t.Fatal(err)
	}
	// Best-effort: returns an error but MUST NOT clobber the file.
	if err := PreseedClaudeConfig("/home/shed/proj", envFunc(map[string]string{"HOME": home})); err == nil {
		t.Fatal("expected an error for malformed config")
	}
	data, _ := os.ReadFile(path)
	if string(data) != string(garbage) {
		t.Fatalf("malformed config was modified: %q", data)
	}
}

func TestPreseedClaudeConfigConcurrent(t *testing.T) {
	home := t.TempDir()
	env := envFunc(map[string]string{"HOME": home})
	workdirs := []string{"/a", "/b", "/c", "/d", "/e"}
	var wg sync.WaitGroup
	for _, wd := range workdirs {
		wg.Add(1)
		go func(d string) {
			defer wg.Done()
			if err := PreseedClaudeConfig(d, env); err != nil {
				t.Errorf("preseed %s: %v", d, err)
			}
		}(wd)
	}
	wg.Wait()

	m := readConfig(t, filepath.Join(home, ".claude.json"))
	for _, wd := range workdirs {
		if !projectTrusted(m, wd) {
			t.Errorf("concurrent insert lost workdir %s: %v", wd, m["projects"])
		}
	}
}

func TestPreseedClaudeConfigNullConfigDoesNotPanic(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join(home, ".claude.json")
	if err := os.WriteFile(path, []byte("null"), 0o600); err != nil {
		t.Fatal(err)
	}
	// A literal `null` decodes a map to nil; preseed must re-seed an object, not panic.
	if err := PreseedClaudeConfig("/home/shed/proj", envFunc(map[string]string{"HOME": home})); err != nil {
		t.Fatal(err)
	}
	if !projectTrusted(readConfig(t, path), "/home/shed/proj") {
		t.Fatal("workdir not trusted after null-config preseed")
	}
}

func TestPreseedClaudeConfigPreservesLargeInteger(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join(home, ".claude.json")
	// 2^53+1 is not exactly representable as float64 — a plain decode would corrupt it.
	if err := os.WriteFile(path, []byte(`{"bigNum":9007199254740993,"projects":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := PreseedClaudeConfig("/p", envFunc(map[string]string{"HOME": home})); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	if !strings.Contains(string(data), "9007199254740993") {
		t.Fatalf("large integer was corrupted on round-trip: %s", data)
	}
}

func TestPreseedClaudeConfigNoHomeIsNoOp(t *testing.T) {
	if err := PreseedClaudeConfig("/x", envFunc(map[string]string{})); err == nil {
		t.Fatal("expected an error (skipped) when neither CLAUDE_CONFIG_DIR nor HOME is set")
	}
}
