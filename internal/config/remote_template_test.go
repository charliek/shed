package config

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

const devRemoteTemplate = "../../configs/server.dev-remote.yaml.tmpl"

// TestDevRemoteTemplate keeps configs/server.dev-remote.yaml.tmpl loadable,
// in the static mode it ships with and with its commented auto mode switched
// on, and free of any address.
func TestDevRemoteTemplate(t *testing.T) {
	raw, err := os.ReadFile(devRemoteTemplate)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	rendered := strings.NewReplacer("@DEV_REMOTE_DIR@", dir, "@SHED_REMOTE_ENV@", "dev-box", "@GITHUB_USER@", "example").Replace(string(raw))

	// Switch the commented auto block on, the way its comment says to.
	auto := rendered
	for _, r := range []struct{ old, new string }{
		{"    mode: static\n", ""},
		{"    source_ranges_env: SHED_REMOTE_SOURCE_RANGES\n", ""},
		{"    # mode: auto\n", "    mode: auto\n"},
		{"    # echo_urls:\n", "    echo_urls:\n"},
		{"    #   - https://<first-echo-service>/\n", "      - https://echo-one.example/\n"},
		{"    #   - https://<second-echo-service>/\n", "      - https://echo-two.example/\n"},
	} {
		if !strings.Contains(auto, r.old) {
			t.Fatalf("the template lacks the line %q", r.old)
		}
		auto = strings.Replace(auto, r.old, r.new, 1)
	}

	tests := []struct {
		name string
		yaml string
		want RemoteFirewallConfig
	}{
		{"as shipped: static, sources from a variable", rendered,
			RemoteFirewallConfig{Mode: RemoteFirewallStatic, SourceRangesEnv: "SHED_REMOTE_SOURCE_RANGES"}},
		{"auto switched on", auto,
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
