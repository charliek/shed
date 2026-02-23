package docker

import (
	"testing"

	"github.com/charliek/shed/internal/backend"
	"github.com/charliek/shed/internal/config"
)

func TestDockerBackendImplementsInterface(t *testing.T) {
	// Compile-time check (also in backend.go, but explicit test is good)
	var _ backend.Backend = (*DockerBackend)(nil)
}

func TestBuildExecConfigWorkingDirDefault(t *testing.T) {
	execConfig := buildExecConfig([]string{"pwd"}, backend.ExecOptions{})
	if execConfig.WorkingDir != config.WorkspacePath {
		t.Fatalf("WorkingDir = %q, want %q", execConfig.WorkingDir, config.WorkspacePath)
	}
}

func TestBuildExecConfigWorkingDirCustom(t *testing.T) {
	execConfig := buildExecConfig([]string{"pwd"}, backend.ExecOptions{
		WorkingDir: "/tmp",
	})
	if execConfig.WorkingDir != "/tmp" {
		t.Fatalf("WorkingDir = %q, want %q", execConfig.WorkingDir, "/tmp")
	}
}
