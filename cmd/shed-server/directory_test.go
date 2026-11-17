package main

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/charliek/shed/internal/config"
	"github.com/charliek/shed/internal/directory"
	"github.com/charliek/shed/internal/users"
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

// TestServeBuildsTheLookupByConfig: serve's config load (loadConfig) takes a
// config with auth.directory, and serve's lookup is then the directory's
// (its local admin a user, no implicit user, every connection tracked by its
// user, unbound certificates refused, an unknown key's login a pull). Without
// the block it is the config
// lookup, as before.
func TestServeBuildsTheLookupByConfig(t *testing.T) {
	adminLine, adminFP := wiringKey(t)
	admins := "auth:\n  mode: mtls\n  users:\n    admin:\n      admin: true\n      keys:\n        - {key: \"" + adminLine + "\", label: laptop}\n"

	writeServeConfig(t, admins+"  directory:\n    url: https://accounts.example.test\n")
	cfg, err := loadConfig()
	if err != nil || !cfg.DirectoryConfigured() {
		t.Fatalf("serve's load: %v (directory configured: %v); want it accepted", err, cfg != nil && cfg.DirectoryConfigured())
	}
	l, err := newUserLookup(context.Background(), cfg, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	rt, ok := l.(*directory.Runtime)
	if !ok {
		t.Fatalf("with auth.directory serve's lookup is a %T, want *directory.Runtime", l)
	}
	if _, bound := l.(users.BoundCredentialsOnly); !bound {
		t.Error("the directory's lookup does not refuse unbound certificates")
	}
	if _, refetch := l.(users.UnknownKeyRefetcher); !refetch {
		t.Error("the directory's lookup does not pull for an unknown key (a new person's first SSH login)")
	}
	if u, k, ok := l.UserForKey(adminFP); !ok || u.Name != "admin" || !u.Admin || k.Source != users.SourceAdHoc || k.Label != "laptop" {
		t.Errorf("the local admin's key resolves to %+v %+v %v", u, k, ok)
	}
	if _, ok := l.User(users.ImplicitName); ok || l.Mode() != config.SSHAuthEnforce {
		t.Errorf("implicit user %v, mode %s; want none and enforce", ok, l.Mode())
	}
	if st := rt.Status(); st.State != directory.StateNotEnrolled {
		t.Errorf("a fresh state directory: state %s, want not-enrolled", st.State)
	}

	writeServeConfig(t, admins)
	cfg, err = loadConfig()
	if err != nil || cfg.DirectoryConfigured() || !cfg.UsersConfigured() {
		t.Fatalf("serve's load without a directory = %v, %v; want the config", cfg, err)
	}
	l, err = newUserLookup(context.Background(), cfg, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := l.(*users.ConfigLookup); !ok {
		t.Fatalf("without auth.directory serve's lookup is a %T, want *users.ConfigLookup", l)
	}
	if _, bound := l.(users.BoundCredentialsOnly); bound {
		t.Error("the config lookup refuses unbound certificates")
	}
	if ka := sshKeyAuth(l); ka.TrackConnections || ka.Users != l {
		t.Errorf("without auth.directory the SSH key auth tracks connections (%v)", ka.TrackConnections)
	}
	if ka := sshKeyAuth(rt); !ka.TrackConnections || ka.Mode != config.SSHAuthEnforce {
		t.Errorf("with auth.directory the SSH key auth = %+v; want connections tracked, enforce", ka)
	}
}

// TestConfigValidateSummarisesTheDirectory: config-validate prints the
// service (issuer, trailing slash dropped), the organization, refresh and
// trust with their defaults, and the local admins. Nothing without a
// directory.
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
	}
	if got := directorySummary(cfg); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("summary:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}

	cfg = loadWiringConfig(t, admins+"  directory:\n    url: http://127.0.0.1:18080\n    allow_plain_http: true\n")
	if got := directorySummary(cfg); len(got) != 2 || got[0] != "users: from the directory at http://127.0.0.1:18080 (the organization named at enrolment), refresh 24h0m0s, trust 72h0m0s" {
		t.Errorf("summary with defaults: %q", got)
	}

	if got := directorySummary(loadWiringConfig(t, admins)); got != nil {
		t.Errorf("summary without a directory: %q", got)
	}
}
