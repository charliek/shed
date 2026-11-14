package config

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

const devRemoteTemplate = "../../configs/server.dev-remote.yaml.tmpl"

// TestDevRemoteTemplate keeps configs/server.dev-remote.yaml.tmpl loadable
// in both firewall modes `make dev-server-up-remote` renders it in, and free
// of any address. The replacements are the Make target's own: a static
// render names the variable, an auto render lists SHED_REMOTE_ECHO_URLS as a
// flow sequence.
func TestDevRemoteTemplate(t *testing.T) {
	raw, err := os.ReadFile(devRemoteTemplate)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	for _, placeholder := range []string{"    mode: @FIREWALL_MODE@\n", "    @FIREWALL_SETTING@\n"} {
		if strings.Count(string(raw), placeholder) != 1 {
			t.Fatalf("the template must hold the line %q exactly once", placeholder)
		}
	}
	render := func(mode, setting string) string {
		return strings.NewReplacer("@DEV_REMOTE_DIR@", dir, "@SHED_REMOTE_ENV@", "dev-box", "@GITHUB_USER@", "example",
			"@FIREWALL_MODE@", mode, "@FIREWALL_SETTING@", setting).Replace(string(raw))
	}
	rendered := render("static", "source_ranges_env: SHED_REMOTE_SOURCE_RANGES")
	auto := render("auto", "echo_urls: [https://echo-one.example/, https://echo-two.example/]")
	for name, y := range map[string]string{"static": rendered, "auto": auto} {
		if strings.Contains(y, "@") && regexp.MustCompile(`@[A-Z_]+@`).MatchString(y) {
			t.Fatalf("the %s render still holds a placeholder", name)
		}
	}

	tests := []struct {
		name string
		yaml string
		want RemoteFirewallConfig
	}{
		{"static, the default: sources from a variable", rendered,
			RemoteFirewallConfig{Mode: RemoteFirewallStatic, SourceRangesEnv: "SHED_REMOTE_SOURCE_RANGES"}},
		{"auto: echo services from the env file", auto,
			RemoteFirewallConfig{Mode: RemoteFirewallAuto, EchoURLs: []string{"https://echo-one.example/", "https://echo-two.example/"}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "server.yaml")
			if err := os.WriteFile(path, []byte(tt.yaml), 0o600); err != nil {
				t.Fatal(err)
			}
			cfg, err := LoadServerConfigFromPath(path)
			if err != nil {
				t.Fatalf("the rendered template does not load: %v", err)
			}
			got := cfg.Remote.Firewall
			if got.Mode != tt.want.Mode || got.SourceRangesEnv != tt.want.SourceRangesEnv || len(got.SourceRanges) != 0 ||
				got.AcknowledgeAny || strings.Join(got.EchoURLs, ",") != strings.Join(tt.want.EchoURLs, ",") {
				t.Errorf("firewall = %+v, want %+v", got, tt.want)
			}
		})
	}

	t.Run("holds no address", func(t *testing.T) {
		for _, m := range regexp.MustCompile(`\b\d{1,3}\.\d{1,3}\.\d{1,3}\.\d{1,3}\b`).FindAllString(string(raw), -1) {
			if m != "127.0.0.1" {
				t.Errorf("the template holds the address %s; addresses belong in the private env file", m)
			}
		}
		if DefaultRemoteFirewallSourceRangesEnv != "SHED_REMOTE_SOURCE_RANGES" {
			t.Errorf("the default variable is %s; the template and the docs name SHED_REMOTE_SOURCE_RANGES", DefaultRemoteFirewallSourceRangesEnv)
		}
	})
}
