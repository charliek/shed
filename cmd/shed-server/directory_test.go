package main

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// writeServeConfig writes a config serve's own loader accepts on this
// platform (the backend's paths point at /dev/null), followed by auth, and
// points the --config flag at it for the test.
func writeServeConfig(t *testing.T, auth string) {
	t.Helper()
	var backend string
	switch runtime.GOOS {
	case "darwin":
		backend = "default_backend: vz\nvz:\n  vfkit_path: vfkit\n  kernel_path: /dev/null\n  default_image: /dev/null\n  instance_dir: /tmp/ti\n  socket_dir: /tmp/ts\n  default_cpus: 2\n  default_memory_mb: 4096\n  default_disk_gb: 20\n  console_port: 1024\n  notify_port: 1026\n"
	case "linux":
		backend = "default_backend: firecracker\nfirecracker:\n  kernel_path: /dev/null\n  default_image: /dev/null\n  instance_dir: /tmp/ti\n  socket_dir: /tmp/ts\n  default_cpus: 2\n  default_memory_mb: 4096\n  default_disk_gb: 20\n  vsock_base_cid: 100\n  console_port: 1024\n  notify_port: 1026\n  bridge_name: shed-br0\n  bridge_cidr: 172.30.0.1/24\n  tap_prefix: shed-tap\n"
	default:
		t.Skipf("no backend on %s", runtime.GOOS)
	}
	path := filepath.Join(t.TempDir(), "server.yaml")
	if err := os.WriteFile(path, []byte("name: test\n"+backend+auth), 0o600); err != nil {
		t.Fatal(err)
	}
	prev := configPath
	configPath = path
	t.Cleanup(func() { configPath = prev })
}

// TestServeRefusesTheDirectoryBlock: serve's config load (the first thing
// runServe does, before anything binds) refuses a config with auth.directory,
// which this build cannot serve, saying so; the same config is valid to
// config-validate's loader. Without the block serve's load is unchanged.
func TestServeRefusesTheDirectoryBlock(t *testing.T) {
	adminLine, _ := wiringKey(t)
	admins := "auth:\n  mode: mtls\n  users:\n    admin:\n      admin: true\n      keys:\n        - {key: \"" + adminLine + "\", label: laptop}\n"

	writeServeConfig(t, admins+"  directory:\n    url: https://accounts.example.test\n")
	if cfg, err := loadConfig(); err != nil || !cfg.DirectoryConfigured() {
		t.Fatalf("config-validate's load: %v (directory configured: %v); want it accepted", err, cfg != nil && cfg.DirectoryConfigured())
	}
	cfg, err := loadServeConfig()
	if !errors.Is(err, errDirectoryUnavailable) || cfg != nil {
		t.Fatalf("serve's load = %v, %v; want errDirectoryUnavailable and no config", cfg, err)
	}
	if !strings.Contains(err.Error(), "auth.directory: the directory is not available in this build") {
		t.Errorf("refusal %q does not name the block and say why", err)
	}

	writeServeConfig(t, admins)
	cfg, err = loadServeConfig()
	if err != nil || cfg == nil || cfg.DirectoryConfigured() || !cfg.UsersConfigured() {
		t.Fatalf("serve's load without a directory = %v, %v; want the config", cfg, err)
	}
}

// TestConfigValidateSummarisesTheDirectory: config-validate prints the
// service (issuer, trailing slash dropped), the organization, refresh and
// trust with their defaults, the local admins, and that serve refuses the
// block in this build. Nothing without a directory.
func TestConfigValidateSummarisesTheDirectory(t *testing.T) {
	aLine, _ := wiringKey(t)
	bLine, _ := wiringKey(t)
	admins := "auth:\n  mode: mtls\n  users:\n" +
		"    root:\n      admin: true\n      keys:\n        - {key: \"" + aLine + "\", label: laptop}\n" +
		"    ops:\n      admin: true\n      keys:\n        - {key: \"" + bLine + "\", label: desk}\n"

	cfg := loadWiringConfig(t, admins+"  directory:\n    url: https://accounts.example.test/\n    organization: example\n    refresh: 1h\n")
	want := []string{
		"users: from the directory at https://accounts.example.test (organization example), refresh 1h0m0s, trust 72h0m0s",
		"local admins: ops, root",
		"note: auth.directory: the directory is not available in this build; remove the auth.directory block to start the server",
	}
	if got := directorySummary(cfg); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("summary:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}

	cfg = loadWiringConfig(t, admins+"  directory:\n    url: http://127.0.0.1:18080\n    allow_plain_http: true\n")
	if got := directorySummary(cfg); len(got) != 3 || got[0] != "users: from the directory at http://127.0.0.1:18080 (the organization named at enrolment), refresh 24h0m0s, trust 72h0m0s" {
		t.Errorf("summary with defaults: %q", got)
	}

	if got := directorySummary(loadWiringConfig(t, admins)); got != nil {
		t.Errorf("summary without a directory: %q", got)
	}
}
