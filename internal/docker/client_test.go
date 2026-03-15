package docker

import (
	"testing"

	"github.com/charliek/shed/internal/config"
	"github.com/docker/docker/api/types/mount"
)

func TestBuildMounts_VolumeMount(t *testing.T) {
	c := &Client{
		config: &config.ServerConfig{
			Credentials: map[string]config.MountConfig{},
		},
	}

	mounts := c.buildMounts("myproject", "")

	if len(mounts) != 1 {
		t.Fatalf("expected 1 mount, got %d", len(mounts))
	}
	m := mounts[0]
	if m.Type != mount.TypeVolume {
		t.Errorf("expected volume mount, got %s", m.Type)
	}
	if m.Source != config.VolumeName("myproject") {
		t.Errorf("expected source %q, got %q", config.VolumeName("myproject"), m.Source)
	}
	if m.Target != config.WorkspacePath {
		t.Errorf("expected target %q, got %q", config.WorkspacePath, m.Target)
	}
}

func TestBuildMounts_BindMount(t *testing.T) {
	c := &Client{
		config: &config.ServerConfig{
			Credentials: map[string]config.MountConfig{},
		},
	}

	localDir := "/home/user/projects/myapp"
	mounts := c.buildMounts("myproject", localDir)

	if len(mounts) != 1 {
		t.Fatalf("expected 1 mount, got %d", len(mounts))
	}
	m := mounts[0]
	if m.Type != mount.TypeBind {
		t.Errorf("expected bind mount, got %s", m.Type)
	}
	if m.Source != localDir {
		t.Errorf("expected source %q, got %q", localDir, m.Source)
	}
	if m.Target != config.WorkspacePath {
		t.Errorf("expected target %q, got %q", config.WorkspacePath, m.Target)
	}
}

func TestBuildMounts_WithCredentials(t *testing.T) {
	sshDir := t.TempDir()
	claudeDir := t.TempDir()

	c := &Client{
		config: &config.ServerConfig{
			Credentials: map[string]config.MountConfig{
				"git-ssh": {
					Source:   sshDir,
					Target:   "/home/shed/.ssh",
					ReadOnly: true,
				},
				"claude": {
					Source:   claudeDir,
					Target:   "/home/shed/.claude",
					ReadOnly: false,
				},
			},
		},
	}

	mounts := c.buildMounts("myproject", "")
	if len(mounts) != 3 {
		t.Fatalf("expected 3 mounts (1 workspace + 2 credentials), got %d", len(mounts))
	}

	// Build lookup by target (map iteration order is nondeterministic)
	byTarget := make(map[string]mount.Mount)
	for _, m := range mounts {
		byTarget[m.Target] = m
	}

	ssh := byTarget["/home/shed/.ssh"]
	if ssh.Type != mount.TypeBind {
		t.Errorf("ssh mount type = %s, want bind", ssh.Type)
	}
	if ssh.Source != sshDir {
		t.Errorf("ssh mount source = %q, want %q", ssh.Source, sshDir)
	}
	if !ssh.ReadOnly {
		t.Error("ssh mount should be read-only")
	}

	claude := byTarget["/home/shed/.claude"]
	if claude.Type != mount.TypeBind {
		t.Errorf("claude mount type = %s, want bind", claude.Type)
	}
	if claude.Source != claudeDir {
		t.Errorf("claude mount source = %q, want %q", claude.Source, claudeDir)
	}
	if claude.ReadOnly {
		t.Error("claude mount should not be read-only")
	}
}

func TestBuildMounts_SkipsMissingSource(t *testing.T) {
	c := &Client{
		config: &config.ServerConfig{
			Credentials: map[string]config.MountConfig{
				"missing": {
					Source:   "/nonexistent/path/for/testing",
					Target:   "/home/shed/.missing",
					ReadOnly: true,
				},
			},
		},
	}

	mounts := c.buildMounts("myproject", "")
	if len(mounts) != 1 {
		t.Fatalf("expected 1 mount (workspace only, missing credential skipped), got %d", len(mounts))
	}
	if mounts[0].Target != config.WorkspacePath {
		t.Errorf("expected workspace mount, got target %q", mounts[0].Target)
	}
}
