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

// CloneRepo clones a git repository into the VM's home directory at
// ~/<reponame> (the same directory name `git clone` would choose).
//
// For SSH-form URLs (git@host:path or ssh://...), the in-VM
// ~/.ssh/known_hosts is seeded first so OpenSSH can verify the remote host
// key. Built-in defaults cover GitHub; extras come from
// ServerConfig.Git.ExtraKnownHosts.
// extraEnv (e.g. egress proxy variables) is appended to the git environment so
// the clone is routed through (and audited by) anything those vars configure.
func CloneRepo(ctx context.Context, agent *AgentClient, serverCfg *config.ServerConfig, repo string, extraEnv []string) error {
	backend.Phase(ctx, "clone")
	backend.Status(ctx, "Cloning repository...")

	dirName, err := config.RepoDirName(repo)
	if err != nil {
		return fmt.Errorf("invalid repository URL: %w", err)
	}

	if config.IsSSHRepoURL(repo) {
		if err := WriteKnownHosts(ctx, agent, BuildKnownHosts(serverCfg)); err != nil {
			return fmt.Errorf("failed to seed known_hosts: %w", err)
		}
	}

	env := append(BuildEnvForGit(serverCfg), extraEnv...)

	var output strings.Builder
	opts := backend.ExecOptions{
		Cmd:        []string{"git", "clone", repo, dirName},
		Env:        env,
		Stdout:     NopWriteCloser(io.MultiWriter(&output, os.Stdout)),
		Stderr:     NopWriteCloser(io.MultiWriter(&output, os.Stderr)),
		WorkingDir: config.HomePath,
		TTY:        false,
	}

	if err := agent.Exec(ctx, opts); err != nil {
		return fmt.Errorf("git clone failed: %w", err)
	}

	return nil
}

// WriteKnownHosts writes content into the in-VM ~/.ssh/known_hosts file via
// the agent. The file is created with mode 0600 (umask 077) and owned by
// whatever user the agent runs commands as (the `shed` user — see
// cmd/shed-agent/server.go).
//
// Overwrites any existing file. ~/.ssh is pre-created in the VM image so we
// don't need to mkdir it here.
func WriteKnownHosts(ctx context.Context, agent *AgentClient, content string) error {
	opts := backend.ExecOptions{
		Cmd:    []string{"sh", "-c", "umask 077 && cat > ~/.ssh/known_hosts"},
		Stdin:  io.NopCloser(strings.NewReader(content)),
		Stdout: NopWriteCloser(io.Discard),
		Stderr: NopWriteCloser(io.Discard),
		TTY:    false,
	}
	return agent.Exec(ctx, opts)
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
