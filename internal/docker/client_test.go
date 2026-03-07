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
