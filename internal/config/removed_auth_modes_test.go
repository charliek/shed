package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// platformBackendYAML is the backend half of a server.yaml that passes the
// full Validate on this host, for loader tests that must reach Validate.
func platformBackendYAML(t *testing.T) string {
	t.Helper()
	backend, bcfg := platformTestBackend(t)
	out := "default_backend: " + backend + "\n"
	if bcfg.vz != nil {
		out += "vz:\n  vfkit_path: vfkit\n  kernel_path: /dev/null\n  default_image: /dev/null\n  instance_dir: /tmp/ti\n  socket_dir: /tmp/ts\n  default_cpus: 2\n  default_memory_mb: 4096\n  default_disk_gb: 20\n  console_port: 1024\n  notify_port: 1026\n"
	}
	if bcfg.fc != nil {
		out += "firecracker:\n  kernel_path: /dev/null\n  default_image: /dev/null\n  instance_dir: /tmp/ti\n  socket_dir: /tmp/ts\n  default_cpus: 2\n  default_memory_mb: 4096\n  default_disk_gb: 20\n  vsock_base_cid: 100\n  console_port: 1024\n  notify_port: 1026\n  bridge_name: shed-br0\n  bridge_cidr: 172.30.0.1/24\n  tap_prefix: shed-tap\n"
	}
	return out
}

// TestLoadRefusesRemovedAuthShapes is the config-load contract for the auth
// modes this release removed. Every shape below loaded on 0.9.x; each must now
// fail at load, through both loaders (serve's LoadServerConfigFromPath and the
// CLI/preflight LoadServerConfigForCLI), with a nil config and a message that
// names mtls, so no path leaves such a server running, let alone running open.
func TestLoadRefusesRemovedAuthShapes(t *testing.T) {
	keys := "  ssh:\n    github_users: [charliek]\n"
	tests := []struct {
		name string
		body string
		want string // a further substring the error must carry, beside "mtls"
	}{
		{"token mode", "auth:\n  mode: token\n" + keys, `auth.mode: "token" was removed`},
		{"token mode without key sources", "auth:\n  mode: token\n", `auth.mode: "token" was removed`},
		{"secure, the token mode's old alias", "auth:\n  mode: secure\n" + keys, `auth.mode: "secure" was removed`},
		{"token mode bound wide", "bind_address: 0.0.0.0\nauth:\n  mode: token\n" + keys, `auth.mode: "token" was removed`},
		{"token mode with an explicit https_port", "https_port: 8443\nauth:\n  mode: token\n" + keys, `auth.mode: "token" was removed`},
		{"open bound wide with the old acknowledgment", "bind_address: 0.0.0.0\nallow_insecure_exposure: true\n", "allow_insecure_exposure"},
		{"the old acknowledgment on loopback", "allow_insecure_exposure: true\n", "allow_insecure_exposure"},
		{"the old acknowledgment set false", "allow_insecure_exposure: false\n", "allow_insecure_exposure"},
		{"the old acknowledgment beside mtls", "bind_address: 0.0.0.0\nallow_insecure_exposure: true\nauth:\n  mode: mtls\n" + keys, "allow_insecure_exposure"},
		{"open bound to all IPv4", "bind_address: 0.0.0.0\n", "loopback only"},
		{"open bound to star", "bind_address: \"*\"\n", "loopback only"},
		{"open bound to all interfaces", "bind_address: \"::\"\n", "loopback only"},
		{"open bound to a LAN address", "bind_address: 192.168.1.20\n", "loopback only"},
		{"explicit open bound wide", "bind_address: 0.0.0.0\nauth:\n  mode: open\n", "loopback only"},
	}
	loaders := []struct {
		name string
		load func(string) (*ServerConfig, error)
	}{
		{"serve", LoadServerConfigFromPath},
		{"cli", LoadServerConfigForCLI},
	}
	for _, tt := range tests {
		for _, l := range loaders {
			t.Run(tt.name+"/"+l.name, func(t *testing.T) {
				path := filepath.Join(t.TempDir(), "server.yaml")
				body := "name: test\n" + platformBackendYAML(t) + tt.body
				if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
					t.Fatal(err)
				}
				cfg, err := l.load(path)
				if err == nil {
					t.Fatalf("loaded (mode %q, bind %q); want a refusal", cfg.AuthModeValue(), cfg.BindAddress)
				}
				if cfg != nil {
					t.Errorf("a refused load returned a config: %+v", cfg)
				}
				for _, want := range []string{"mtls", tt.want} {
					if !strings.Contains(err.Error(), want) {
						t.Errorf("error %q does not contain %q", err, want)
					}
				}
			})
		}
	}
}

// TestLoadAcceptsTheKeptAuthShapes is the positive half: the shapes that stay
// valid still load through both loaders, so the refusals above are about the
// removed shapes and not about the test's scaffolding.
func TestLoadAcceptsTheKeptAuthShapes(t *testing.T) {
	keys := "  ssh:\n    github_users: [charliek]\n"
	tests := []struct {
		name     string
		body     string
		wantMode string
	}{
		{"no auth block (open, loopback)", "", AuthModeOpen},
		{"explicit open on loopback", "bind_address: 127.0.0.1\nauth:\n  mode: open\n", AuthModeOpen},
		{"open on localhost", "bind_address: localhost\n", AuthModeOpen},
		{"mtls on loopback", "auth:\n  mode: mtls\n" + keys, AuthModeMTLS},
		{"mtls bound to all IPv4", "bind_address: 0.0.0.0\nauth:\n  mode: mtls\n" + keys, AuthModeMTLS},
		{"mtls bound to all interfaces", "bind_address: \"::\"\nauth:\n  mode: mtls\n" + keys, AuthModeMTLS},
	}
	for _, tt := range tests {
		for _, l := range []struct {
			name string
			load func(string) (*ServerConfig, error)
		}{{"serve", LoadServerConfigFromPath}, {"cli", LoadServerConfigForCLI}} {
			t.Run(tt.name+"/"+l.name, func(t *testing.T) {
				path := filepath.Join(t.TempDir(), "server.yaml")
				body := "name: test\n" + platformBackendYAML(t) + tt.body
				if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
					t.Fatal(err)
				}
				cfg, err := l.load(path)
				if err != nil {
					t.Fatalf("refused: %v", err)
				}
				if got := cfg.AuthModeValue(); got != tt.wantMode {
					t.Errorf("AuthModeValue() = %q, want %q", got, tt.wantMode)
				}
			})
		}
	}
}

// writeServerYAML writes a server.yaml of "name: test", the host's backend
// block and body, and returns its path.
func writeServerYAML(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "server.yaml")
	if err := os.WriteFile(path, []byte("name: test\n"+platformBackendYAML(t)+body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestLoadRefusesUnrecognisedAuthKeys: YAML keys are case-sensitive and the
// typed unmarshal ignores what it does not know, so before this check a
// re-cased or misspelt auth key was dropped and the server started open with
// no allowlist, whatever mode the operator wrote. Each shape below loaded on
// 0.9.x (and, for every row but the token ones, said open); each must now
// fail through both loaders with a nil config and an error naming the key.
func TestLoadRefusesUnrecognisedAuthKeys(t *testing.T) {
	keys := "  ssh:\n    github_users: [charliek]\n"
	tests := []struct {
		name string
		body string
		want string // the error names the offending key
	}{
		{"auth.Mode token (flow style)", "auth: {Mode: token}\n", "Mode"},
		{"auth.Mode mtls", "auth:\n  Mode: mtls\n" + keys, "Mode"},
		{"auth.MODE mtls", "auth:\n  MODE: mtls\n" + keys, "MODE"},
		{"auth misspelt key", "auth:\n  mdoe: mtls\n" + keys, "mdoe"},
		{"auth.SSH re-cased", "auth:\n  mode: mtls\n  SSH:\n    github_users: [charliek]\n", "SSH"},
		{"auth.Token_TTL re-cased", "auth:\n  mode: mtls\n  Token_TTL: 1h\n" + keys, "Token_TTL"},
		{"auth.ssh.Mode re-cased", "auth:\n  mode: mtls\n  ssh:\n    Mode: enforce\n    github_users: [charliek]\n", "Mode"},
		{"auth.ssh misspelt key", "auth:\n  mode: mtls\n  ssh:\n    github_users: [charliek]\n    authorised_keys: [x]\n", "authorised_keys"},
		{"auth.ssh.GitHub_Users re-cased", "auth:\n  mode: mtls\n  ssh:\n    GitHub_Users: [charliek]\n", "GitHub_Users"},
		{"auth.clients nested re-cased", "auth:\n  mode: mtls\n" + keys + "  clients:\n    ci:\n      Policy:\n        name_prefix: ci-\n", "Policy"},
		{"auth.clients.policy nested misspelt", "auth:\n  mode: mtls\n" + keys + "  clients:\n    ci:\n      policy:\n        name_prefix: ci-\n        max_shed: 2\n        max_age: 1h\n", "max_shed"},
		{"Auth re-cased at the top", "Auth:\n  mode: mtls\n" + keys, `"Auth"`},
		{"AUTH re-cased at the top", "AUTH:\n  mode: mtls\n" + keys, `"AUTH"`},
		{"Auth beside a lower-case auth", "auth:\n  mode: mtls\n" + keys + "Auth:\n  mode: open\n", `"Auth"`},
		{"Allow_Insecure_Exposure re-cased", "Allow_Insecure_Exposure: true\n", `"Allow_Insecure_Exposure"`},
		{"Bind_Address re-cased", "Bind_Address: 0.0.0.0\n", `"Bind_Address"`},
		{"Public_Exposure re-cased", "Public_Exposure: true\n", `"Public_Exposure"`},
	}
	for _, tt := range tests {
		for _, l := range []struct {
			name string
			load func(string) (*ServerConfig, error)
		}{{"serve", LoadServerConfigFromPath}, {"cli", LoadServerConfigForCLI}} {
			t.Run(tt.name+"/"+l.name, func(t *testing.T) {
				cfg, err := l.load(writeServerYAML(t, tt.body))
				if err == nil {
					t.Fatalf("loaded (mode %q, bind %q); want a refusal", cfg.AuthModeValue(), cfg.BindAddress)
				}
				if cfg != nil {
					t.Errorf("a refused load returned a config: %+v", cfg)
				}
				if !strings.Contains(err.Error(), tt.want) {
					t.Errorf("error %q does not name %s", err, tt.want)
				}
			})
		}
	}
}

// TestLoadKeepsAcceptingWhatItAlwaysDid is the control for the refusals
// above: every auth key spelled as documented still loads, and a top-level
// key that matches no known key in any casing is still ignored (refusing
// every unknown top-level key would break existing configs).
func TestLoadKeepsAcceptingWhatItAlwaysDid(t *testing.T) {
	tests := []struct {
		name     string
		body     string
		wantMode string
	}{
		{"every auth and auth.ssh key spelled right",
			"auth:\n  mode: mtls\n  token_ttl: 2h\n  ssh:\n    github_users: [charliek]\n    authorized_keys: []\n    github_refresh: 30m\n    max_auth_tries: 20\n",
			AuthModeMTLS},
		{"an empty auth block", "auth: {}\n", AuthModeOpen},
		{"a null auth block", "auth:\n", AuthModeOpen},
		{"an unrelated unknown top-level key", "my_site_note: kept for the operator\nX-Owner: ops\n", AuthModeOpen},
	}
	for _, tt := range tests {
		for _, l := range []struct {
			name string
			load func(string) (*ServerConfig, error)
		}{{"serve", LoadServerConfigFromPath}, {"cli", LoadServerConfigForCLI}} {
			t.Run(tt.name+"/"+l.name, func(t *testing.T) {
				cfg, err := l.load(writeServerYAML(t, tt.body))
				if err != nil {
					t.Fatalf("refused: %v", err)
				}
				if got := cfg.AuthModeValue(); got != tt.wantMode {
					t.Errorf("AuthModeValue() = %q, want %q", got, tt.wantMode)
				}
			})
		}
	}
}

// TestCommittedConfigsPassTheKeyScans: every server config this repo ships or
// documents passes the removed-, unknown- and re-cased-key scans, so the
// strict auth check breaks none of them.
func TestCommittedConfigsPassTheKeyScans(t *testing.T) {
	paths, err := filepath.Glob(filepath.Join("..", "..", "configs", "server*.yaml*"))
	if err != nil {
		t.Fatal(err)
	}
	paths = append(paths, filepath.Join("..", "..", "packaging", "server.yaml.tmpl"))
	if len(paths) < 5 {
		t.Fatalf("found only %d configs: %v", len(paths), paths)
	}
	for _, p := range paths {
		t.Run(filepath.Base(p), func(t *testing.T) {
			data, err := os.ReadFile(p)
			if err != nil {
				t.Fatal(err)
			}
			if err := rejectRemovedKeys(data); err != nil {
				t.Errorf("%s: %v", p, err)
			}
		})
	}
}

// TestYAMLKeysCoverServerConfig pins the reflection the re-cased-key scan
// relies on: it sees the keys the typed unmarshal reads, and none it skips.
func TestYAMLKeysCoverServerConfig(t *testing.T) {
	got := map[string]bool{}
	for _, k := range yamlKeys(reflect.TypeOf(ServerConfig{})) {
		got[k] = true
	}
	for _, want := range []string{"name", "auth", "bind_address", "https_port", "remote", "network", "credentials"} {
		if !got[want] {
			t.Errorf("yamlKeys(ServerConfig) lacks %q", want)
		}
	}
	if got["-"] || got["envvars"] {
		t.Errorf("yamlKeys(ServerConfig) includes a skipped field: %v", got)
	}
}
