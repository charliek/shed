package vmutil

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/charliek/shed/internal/backend"
	"github.com/charliek/shed/internal/config"
)

// CloneRepo clones a git repository into the VM's workspace.
func CloneRepo(ctx context.Context, agent *AgentClient, serverCfg *config.ServerConfig, repo string) error {
	env := BuildEnvForGit(serverCfg)

	var output strings.Builder
	opts := backend.ExecOptions{
		Cmd:        []string{"git", "clone", repo, "."},
		Env:        env,
		Stdout:     NopWriteCloser(io.MultiWriter(&output, os.Stdout)),
		Stderr:     NopWriteCloser(io.MultiWriter(&output, os.Stderr)),
		WorkingDir: config.WorkspacePath,
		TTY:        false,
	}

	if err := agent.Exec(ctx, opts); err != nil {
		return fmt.Errorf("git clone failed: %w", err)
	}

	return nil
}

// BuildEnvForGit builds environment variables for git operations from server config.
func BuildEnvForGit(serverCfg *config.ServerConfig) []string {
	var env []string

	if serverCfg == nil {
		return env
	}

	for key, value := range serverCfg.EnvVars {
		env = append(env, fmt.Sprintf("%s=%s", key, value))
	}

	return env
}
