package config

import (
	"os"
	"path/filepath"
	"testing"
)

// TestValidateHTTPPort covers the mode-gated range check: http_port is required
// (1..65535) in open mode and optional (0 = unset) in token mode.
func TestValidateHTTPPort(t *testing.T) {
	tokenAuth := &AuthConfig{Mode: AuthModeToken, SSH: &SSHAuthConfig{GitHubUsers: []string{"charliek"}}}
	tests := []struct {
		name     string
		auth     *AuthConfig
		httpPort int
		wantErr  bool
	}{
		{"open requires http_port (0 invalid)", nil, 0, true},
		{"open valid http_port", nil, 8080, false},
		{"open out of range", nil, 70000, true},
		{"token allows unset http_port (0)", tokenAuth, 0, false},
		{"token allows a set http_port", tokenAuth, 8080, false},
		{"token out of range rejected", tokenAuth, 70000, true},
		{"token negative rejected", tokenAuth, -1, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := &ServerConfig{HTTPPort: tt.httpPort, Auth: tt.auth}
			if err := c.validateHTTPPort(); (err != nil) != tt.wantErr {
				t.Errorf("validateHTTPPort() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

// TestLoadServerConfigHTTPPortByMode verifies the loader defaults http_port to
// 8080 in open mode and drops it to 0 in token mode (where no plain-HTTP
// listener is served), so /api/info and the written client entry omit it. It
// also covers the deprecated "secure" spelling: the loader must normalize it
// to token mode and behave identically to the canonical "token" spelling.
func TestLoadServerConfigHTTPPortByMode(t *testing.T) {
	backend, bcfg := platformTestBackend(t)
	backendYAML := ""
	if bcfg.vz != nil {
		backendYAML = "vz:\n  vfkit_path: vfkit\n  kernel_path: /dev/null\n  default_image: /dev/null\n  instance_dir: /tmp/ti\n  socket_dir: /tmp/ts\n  default_cpus: 2\n  default_memory_mb: 4096\n  default_disk_gb: 20\n  console_port: 1024\n  notify_port: 1026\n"
	}
	if bcfg.fc != nil {
		backendYAML = "firecracker:\n  kernel_path: /dev/null\n  default_image: /dev/null\n  instance_dir: /tmp/ti\n  socket_dir: /tmp/ts\n  default_cpus: 2\n  default_memory_mb: 4096\n  default_disk_gb: 20\n  vsock_base_cid: 100\n  console_port: 1024\n  notify_port: 1026\n  bridge_name: shed-br0\n  bridge_cidr: 172.30.0.1/24\n  tap_prefix: shed-tap\n"
	}
	tokenAuthYAML := "auth:\n  mode: token\n  ssh:\n    github_users: [charliek]\n"
	secureAliasAuthYAML := "auth:\n  mode: secure\n  ssh:\n    github_users: [charliek]\n"
	tests := []struct {
		name         string
		extra        string
		wantHTTPPort int
	}{
		{"open keeps a set http_port", "http_port: 8080\n", 8080},
		{"open defaults http_port when unset", "", 8080},
		{"token zeroes a set http_port", "http_port: 8080\n" + tokenAuthYAML, 0},
		{"token leaves http_port unset", tokenAuthYAML, 0},
		{"deprecated secure alias zeroes a set http_port", "http_port: 8080\n" + secureAliasAuthYAML, 0},
		{"deprecated secure alias leaves http_port unset", secureAliasAuthYAML, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfgYAML := "name: test\ndefault_backend: " + backend + "\n" + tt.extra + backendYAML
			cfgPath := filepath.Join(t.TempDir(), "server.yaml")
			if err := os.WriteFile(cfgPath, []byte(cfgYAML), 0o644); err != nil {
				t.Fatal(err)
			}
			cfg, err := LoadServerConfigFromPath(cfgPath)
			if err != nil {
				t.Fatalf("load: %v", err)
			}
			if cfg.HTTPPort != tt.wantHTTPPort {
				t.Errorf("HTTPPort = %d, want %d", cfg.HTTPPort, tt.wantHTTPPort)
			}
		})
	}
}
