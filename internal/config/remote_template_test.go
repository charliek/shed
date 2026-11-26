package config

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

const devRemoteTemplate = "../../configs/server.dev-remote.yaml.tmpl"

// devRemoteReplacer is the Make target's render: the reference service on
// loopback with the local transport, or an https service with ssh.
func devRemoteReplacer(t *testing.T, dir, mode, setting string, local bool) *strings.Replacer {
	t.Helper()
	url, dirSetting, transport, transportSetting := "https://accounts.example.test", "# https: no allow_plain_http", "ssh", "allow_test_drivers: false"
	if local {
		url, dirSetting, transport, transportSetting = "http://127.0.0.1:18700", "allow_plain_http: true", "local", "allow_test_drivers: true"
	}
	return strings.NewReplacer("@DEV_REMOTE_DIR@", dir, "@ADMIN_KEY@", userKeyLine(t),
		"@DIRECTORY_URL@", url, "@DIRECTORY_SETTING@", dirSetting, "@TRANSPORT@", transport, "@TRANSPORT_SETTING@", transportSetting,
		"@FIREWALL_MODE@", mode, "@FIREWALL_SETTING@", setting)
}

// TestDevRemoteTemplate keeps configs/server.dev-remote.yaml.tmpl loadable
// in both firewall modes `make dev-server-up-remote` renders it in, against
// an https account service and against the reference service on loopback,
// and free of any address. The replacements are the Make target's own: a
// static render names the variable, an auto render lists
// SHED_REMOTE_ECHO_URLS as a flow sequence.
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
	render := func(mode, setting string, local bool) string {
		return devRemoteReplacer(t, dir, mode, setting, local).Replace(string(raw))
	}
	rendered := render("static", "source_ranges_env: SHED_REMOTE_SOURCE_RANGES", false)
	auto := render("auto", "echo_urls: [https://echo-one.example/, https://echo-two.example/]", false)
	ref := render("static", "source_ranges_env: SHED_REMOTE_SOURCE_RANGES", true)
	for name, y := range map[string]string{"static": rendered, "auto": auto, "reference service": ref} {
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
		{"the reference service on loopback, the local transport", ref,
			RemoteFirewallConfig{Mode: RemoteFirewallStatic, SourceRangesEnv: "SHED_REMOTE_SOURCE_RANGES"}},
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
			if cfg.Remote.Provider != RemoteProviderService || !cfg.DirectoryConfigured() {
				t.Errorf("provider %q, directory %v: want the service and a directory", cfg.Remote.Provider, cfg.DirectoryConfigured())
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

// TestRemoteUnknownKeysRefused: a misspelt key under remote: stops the load.
// Every setting there bounds a billed machine or who may reach it, and the
// typed decode alone would drop the key and use the default.
func TestRemoteUnknownKeysRefused(t *testing.T) {
	raw, err := os.ReadFile(devRemoteTemplate)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	good := devRemoteReplacer(t, dir, "static", "source_ranges_env: SHED_REMOTE_SOURCE_RANGES", false).Replace(string(raw))
	load := func(t *testing.T, y string) error {
		t.Helper()
		path := filepath.Join(t.TempDir(), "server.yaml")
		if err := os.WriteFile(path, []byte(y), 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := LoadServerConfigFromPath(path)
		return err
	}
	if err := load(t, good); err != nil {
		t.Fatalf("the unchanged template does not load: %v", err)
	}

	tests := []struct {
		name, old, new, want string
	}{
		{"a limit at the wrong level", "  agent_install: never\n", "  agent_install: never\n  max_running: 1\n", "max_running"},
		{"the state directory", "  state_dir: " + dir + "/remote\n", "  state_dirs: " + dir + "/remote\n", "state_dirs"},
		{"a firewall setting", "    source_ranges_env: SHED_REMOTE_SOURCE_RANGES\n", "    source_range_env: SHED_REMOTE_SOURCE_RANGES\n", "source_range_env"},
		{"the firewall acknowledgement", "    mode: static\n", "    mode: static\n    acknowledge: false\n", "acknowledge"},
		{"an ssh setting", "    client_key_env: SHED_REMOTE_SSH_KEY\n", "    client_key: SHED_REMOTE_SSH_KEY\n", "client_key"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if strings.Count(good, tt.old) != 1 {
				t.Fatalf("the template must hold %q exactly once", tt.old)
			}
			err := load(t, strings.Replace(good, tt.old, tt.new, 1))
			if err == nil || !strings.Contains(err.Error(), "remote:") || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want a refusal under remote: naming %q", err, tt.want)
			}
		})
	}
}
