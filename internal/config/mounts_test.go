package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRepoDirName(t *testing.T) {
	tests := []struct {
		repo    string
		want    string
		wantErr bool
	}{
		{"git@github.com:owner/repo.git", "repo", false},
		{"git@github.com:owner/repo", "repo", false},
		{"https://github.com/owner/repo.git", "repo", false},
		{"https://github.com/owner/repo", "repo", false},
		{"https://github.com/owner/repo/", "repo", false},
		{"ssh://git@host/owner/repo.git", "repo", false},
		{"https://example.com/a/b/c.git", "c", false},
		{"", "", true},
	}
	for _, tt := range tests {
		t.Run(tt.repo, func(t *testing.T) {
			got, err := RepoDirName(tt.repo)
			if tt.wantErr {
				if err == nil {
					t.Errorf("RepoDirName(%q) = %q, want error", tt.repo, got)
				}
				return
			}
			if err != nil {
				t.Errorf("RepoDirName(%q) unexpected error: %v", tt.repo, err)
				return
			}
			if got != tt.want {
				t.Errorf("RepoDirName(%q) = %q, want %q", tt.repo, got, tt.want)
			}
		})
	}
}

func TestProjectMountBasename(t *testing.T) {
	tests := []struct {
		dir     string
		want    string
		wantErr bool
	}{
		{"/Users/me/projects/myapp", "myapp", false},
		{"/Users/me/projects/myapp/", "myapp", false},
		{"/Users/me/.ssh", "", true},    // dotfile-style names rejected
		{"/Users/me/.config", "", true}, // dotfile-style names rejected
		{"/", "", true},
	}
	for _, tt := range tests {
		t.Run(tt.dir, func(t *testing.T) {
			got, err := ProjectMountBasename(tt.dir)
			if tt.wantErr {
				if err == nil {
					t.Errorf("ProjectMountBasename(%q) = %q, want error", tt.dir, got)
				}
				return
			}
			if err != nil {
				t.Errorf("ProjectMountBasename(%q) unexpected error: %v", tt.dir, err)
				return
			}
			if got != tt.want {
				t.Errorf("ProjectMountBasename(%q) = %q, want %q", tt.dir, got, tt.want)
			}
		})
	}
}

func TestBuildProjectMounts(t *testing.T) {
	t.Run("none", func(t *testing.T) {
		mounts, landing, err := BuildProjectMounts("", nil)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if mounts != nil {
			t.Errorf("mounts = %+v, want nil", mounts)
		}
		if landing != HomePath {
			t.Errorf("landing = %q, want %q", landing, HomePath)
		}
	})

	t.Run("add_dir requires local_dir", func(t *testing.T) {
		if _, _, err := BuildProjectMounts("", []string{"/x/ref"}); err == nil {
			t.Error("expected error when add dirs given without local dir")
		}
	})

	t.Run("local dir only", func(t *testing.T) {
		mounts, landing, err := BuildProjectMounts("/Users/me/app", nil)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(mounts) != 1 || mounts[0].Source != "/Users/me/app" || mounts[0].Target != HomePath+"/app" {
			t.Errorf("mounts = %+v", mounts)
		}
		if landing != HomePath+"/app" {
			t.Errorf("landing = %q, want %q", landing, HomePath+"/app")
		}
	})

	t.Run("local dir + add dirs", func(t *testing.T) {
		mounts, landing, err := BuildProjectMounts("/Users/me/app", []string{"/other/ref", "/third/lib"})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(mounts) != 3 {
			t.Fatalf("len(mounts) = %d, want 3", len(mounts))
		}
		if mounts[1].Target != HomePath+"/ref" || mounts[2].Target != HomePath+"/lib" {
			t.Errorf("mounts = %+v", mounts)
		}
		// Landing dir is always the primary (--local-dir) mount.
		if landing != HomePath+"/app" {
			t.Errorf("landing = %q, want %q", landing, HomePath+"/app")
		}
	})

	t.Run("duplicate basename rejected", func(t *testing.T) {
		if _, _, err := BuildProjectMounts("/a/app", []string{"/b/app"}); err == nil {
			t.Error("expected error for duplicate basenames")
		}
	})

	t.Run("dotfile basename rejected", func(t *testing.T) {
		if _, _, err := BuildProjectMounts("/home/me/.ssh", nil); err == nil {
			t.Error("expected error for dotfile basename")
		}
	})
}

func TestProjectMountTag(t *testing.T) {
	tag := ProjectMountTag("myapp")
	if tag != ProjectMountTag("myapp") {
		t.Error("ProjectMountTag is not deterministic")
	}
	if !strings.HasPrefix(tag, "proj-") {
		t.Errorf("tag %q missing proj- prefix", tag)
	}
	if len(tag) > maxMountTagLen {
		t.Errorf("tag %q length %d exceeds %d", tag, len(tag), maxMountTagLen)
	}
	if ProjectMountTag("a") == ProjectMountTag("b") {
		t.Error("distinct basenames produced the same tag")
	}

	// Long basenames are capped, and distinct long basenames stay distinct
	// thanks to the hash suffix.
	long1 := strings.Repeat("x", 100)
	long2 := strings.Repeat("x", 99) + "y"
	if len(ProjectMountTag(long1)) > maxMountTagLen {
		t.Errorf("long tag length %d exceeds %d", len(ProjectMountTag(long1)), maxMountTagLen)
	}
	if ProjectMountTag(long1) == ProjectMountTag(long2) {
		t.Error("distinct long basenames produced the same tag")
	}

	// Special characters are sanitized to a valid tag.
	spaced := ProjectMountTag("my app")
	if strings.Contains(spaced, " ") {
		t.Errorf("tag %q contains a space", spaced)
	}

	// ProjectMountTagForTarget derives the tag from the guest-dir basename.
	if ProjectMountTagForTarget(HomePath+"/myapp") != ProjectMountTag("myapp") {
		t.Error("ProjectMountTagForTarget should match ProjectMountTag of the basename")
	}
}

func TestResolveCreateLayout(t *testing.T) {
	t.Run("repo", func(t *testing.T) {
		mounts, landing, err := ResolveCreateLayout(CreateShedRequest{Repo: "git@github.com:o/r.git"})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if mounts != nil {
			t.Errorf("mounts = %+v, want nil for repo", mounts)
		}
		if landing != HomePath+"/r" {
			t.Errorf("landing = %q, want %q", landing, HomePath+"/r")
		}
	})

	t.Run("local dir", func(t *testing.T) {
		mounts, landing, err := ResolveCreateLayout(CreateShedRequest{LocalDir: "/x/app"})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(mounts) != 1 || landing != HomePath+"/app" {
			t.Errorf("mounts = %+v, landing = %q", mounts, landing)
		}
	})

	t.Run("none", func(t *testing.T) {
		mounts, landing, err := ResolveCreateLayout(CreateShedRequest{})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if mounts != nil || landing != HomePath {
			t.Errorf("mounts = %+v, landing = %q", mounts, landing)
		}
	})
}

// TestServerConfigMountsCredentialsFallback verifies the "mounts" key is read,
// the deprecated "credentials" key still works as a fallback, and "mounts"
// wins when both are present.
func TestServerConfigMountsCredentialsFallback(t *testing.T) {
	srcDir := t.TempDir()
	backend, bcfg := platformTestBackend(t)

	backendYAML := ""
	if bcfg.vz != nil {
		backendYAML = "vz:\n  vfkit_path: vfkit\n  kernel_path: /dev/null\n  default_image: /dev/null\n  instance_dir: /tmp/test-instances\n  socket_dir: /tmp/test-sockets\n  default_cpus: 2\n  default_memory_mb: 4096\n  default_disk_gb: 20\n  console_port: 1024\n  notify_port: 1026\n"
	}
	if bcfg.fc != nil {
		backendYAML = "firecracker:\n  kernel_path: /dev/null\n  default_image: /dev/null\n  instance_dir: /tmp/test-instances\n  socket_dir: /tmp/test-sockets\n  default_cpus: 2\n  default_memory_mb: 4096\n  default_disk_gb: 20\n  vsock_base_cid: 100\n  console_port: 1024\n  notify_port: 1026\n  bridge_name: shed-br0\n  bridge_cidr: 172.30.0.1/24\n  tap_prefix: shed-tap\n"
	}
	base := "name: test-server\ndefault_backend: " + backend + "\n"
	entry := func(key, name, target string) string {
		return key + ":\n  " + name + ":\n    source: " + srcDir + "\n    target: " + target + "\n"
	}

	load := func(t *testing.T, yaml string) *ServerConfig {
		t.Helper()
		p := filepath.Join(t.TempDir(), "server.yaml")
		if err := os.WriteFile(p, []byte(yaml), 0644); err != nil {
			t.Fatal(err)
		}
		cfg, err := LoadServerConfigFromPath(p)
		if err != nil {
			t.Fatalf("load: %v", err)
		}
		return cfg
	}

	t.Run("mounts key", func(t *testing.T) {
		cfg := load(t, base+entry("mounts", "test", "/home/shed/.test")+backendYAML)
		if _, ok := cfg.Mounts["test"]; !ok {
			t.Errorf("Mounts[test] missing; Mounts=%+v", cfg.Mounts)
		}
		if cfg.Credentials != nil {
			t.Errorf("Credentials should be cleared after load, got %+v", cfg.Credentials)
		}
	})

	t.Run("credentials fallback", func(t *testing.T) {
		cfg := load(t, base+entry("credentials", "test", "/home/shed/.test")+backendYAML)
		if _, ok := cfg.Mounts["test"]; !ok {
			t.Errorf("expected credentials to fall back into Mounts; Mounts=%+v", cfg.Mounts)
		}
	})

	t.Run("mounts wins over credentials", func(t *testing.T) {
		cfg := load(t, base+entry("mounts", "fromMounts", "/home/shed/.a")+entry("credentials", "fromCreds", "/home/shed/.b")+backendYAML)
		if _, ok := cfg.Mounts["fromMounts"]; !ok {
			t.Error("expected mounts entry to be present")
		}
		if _, ok := cfg.Mounts["fromCreds"]; ok {
			t.Error("credentials entry should be ignored when mounts is present")
		}
	})

	t.Run("explicit empty mounts does not fall back", func(t *testing.T) {
		// `mounts: {}` is a non-nil empty map and means "no mounts"; it must
		// NOT trigger the deprecated-credentials fallback.
		cfg := load(t, base+"mounts: {}\n"+entry("credentials", "test", "/home/shed/.test")+backendYAML)
		if _, ok := cfg.Mounts["test"]; ok {
			t.Error("explicit empty mounts:{} should not fall back to credentials")
		}
	})
}

func TestProjectAddDirTargets(t *testing.T) {
	tests := []struct {
		name    string
		mounts  []MountConfig
		landing string
		want    []string
	}{
		{name: "nil mounts (bare or --repo)", mounts: nil, landing: HomePath, want: nil},
		{
			name:    "--local-dir only has no add-dirs",
			mounts:  []MountConfig{{Target: HomePath + "/proj"}},
			landing: HomePath + "/proj",
			want:    nil,
		},
		{
			name: "--add-dirs are every mount but the landing one, in order",
			mounts: []MountConfig{
				{Target: HomePath + "/proj"}, // --local-dir (landing)
				{Target: HomePath + "/sibling"},
				{Target: HomePath + "/other"},
			},
			landing: HomePath + "/proj",
			want:    []string{HomePath + "/sibling", HomePath + "/other"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ProjectAddDirTargets(tt.mounts, tt.landing)
			if len(got) != len(tt.want) {
				t.Fatalf("ProjectAddDirTargets() = %v, want %v", got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("index %d: got %q, want %q", i, got[i], tt.want[i])
				}
			}
		})
	}
}

// TestHasWritableHostMount guards the #232 fast-delete sync decision: a running
// shed with ANY writable host-backed mount (project OR configured server
// `mounts:`) must be flushed before the destroy SIGKILL, or unsynced guest
// writes to host data are lost. A shed with only readonly / no host mounts takes
// the no-sync fast path.
func TestHasWritableHostMount(t *testing.T) {
	ro := MountConfig{Source: "/h/ro", Target: "/t/ro", ReadOnly: true}
	rw := MountConfig{Source: "/h/rw", Target: "/t/rw", ReadOnly: false}

	tests := []struct {
		name          string
		projectMounts []MountConfig
		serverCfg     *ServerConfig
		want          bool
	}{
		{"no mounts, nil serverCfg", nil, nil, false},
		{"only readonly project mount", []MountConfig{ro}, nil, false},
		{"writable project mount", []MountConfig{ro, rw}, nil, true},
		{"writable configured mount only", nil, &ServerConfig{Mounts: map[string]MountConfig{"a": rw}}, true},
		{"readonly configured mount only", nil, &ServerConfig{Mounts: map[string]MountConfig{"a": ro}}, false},
		{"all readonly across both", []MountConfig{ro}, &ServerConfig{Mounts: map[string]MountConfig{"a": ro}}, false},
		{"empty serverCfg mounts", nil, &ServerConfig{}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := HasWritableHostMount(tt.projectMounts, tt.serverCfg); got != tt.want {
				t.Errorf("HasWritableHostMount() = %v, want %v", got, tt.want)
			}
		})
	}
}
