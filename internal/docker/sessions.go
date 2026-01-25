package docker

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/docker/docker/api/types/container"

	"github.com/charliek/shed/internal/config"
)

// ErrTmuxNotAvailable is returned when tmux is not installed in the container.
var ErrTmuxNotAvailable = fmt.Errorf("tmux is not available in this container")

// ErrSessionNotFound is returned when a tmux session does not exist.
var ErrSessionNotFound = fmt.Errorf("session not found")

// ListSessions returns all tmux sessions in a shed container.
// Returns an empty list if the container has no sessions or tmux is not available.
func (c *Client) ListSessions(ctx context.Context, shedName string) ([]config.Session, error) {
	containerName := config.ContainerName(shedName)

	// First check if the container is running
	shed, err := c.GetShed(ctx, shedName)
	if err != nil {
		return nil, err
	}
	if shed.Status != config.StatusRunning {
		return nil, fmt.Errorf("shed %q is not running", shedName)
	}

	// tmux list-sessions format: name:created:attached:windows
	// Using -F for custom format
	cmd := []string{"tmux", "list-sessions", "-F", "#{session_name}:#{session_created}:#{session_attached}:#{session_windows}"}

	output, exitCode, err := c.execCommand(ctx, containerName, cmd)
	if err != nil {
		return nil, fmt.Errorf("failed to list sessions: %w", err)
	}

	// Exit code 1 with "no server running" means no sessions exist
	if exitCode != 0 {
		if strings.Contains(output, "no server running") || strings.Contains(output, "no sessions") {
			return []config.Session{}, nil
		}
		// Check if tmux is not available
		if strings.Contains(output, "not found") || strings.Contains(output, "command not found") {
			return nil, ErrTmuxNotAvailable
		}
		return nil, fmt.Errorf("tmux list-sessions failed: %s", output)
	}

	return parseTmuxSessions(output, shedName)
}

// SessionExists checks if a tmux session exists in a shed container.
func (c *Client) SessionExists(ctx context.Context, shedName, sessionName string) (bool, error) {
	containerName := config.ContainerName(shedName)

	// First check if the container is running
	shed, err := c.GetShed(ctx, shedName)
	if err != nil {
		return false, err
	}
	if shed.Status != config.StatusRunning {
		return false, fmt.Errorf("shed %q is not running", shedName)
	}

	cmd := []string{"tmux", "has-session", "-t", sessionName}

	_, exitCode, err := c.execCommand(ctx, containerName, cmd)
	if err != nil {
		return false, fmt.Errorf("failed to check session: %w", err)
	}

	// Exit code 0 means session exists
	return exitCode == 0, nil
}

// KillSession terminates a tmux session in a shed container.
func (c *Client) KillSession(ctx context.Context, shedName, sessionName string) error {
	containerName := config.ContainerName(shedName)

	// First check if the container is running
	shed, err := c.GetShed(ctx, shedName)
	if err != nil {
		return err
	}
	if shed.Status != config.StatusRunning {
		return fmt.Errorf("shed %q is not running", shedName)
	}

	// Check if session exists first
	exists, err := c.SessionExists(ctx, shedName, sessionName)
	if err != nil {
		return err
	}
	if !exists {
		return ErrSessionNotFound
	}

	cmd := []string{"tmux", "kill-session", "-t", sessionName}

	output, exitCode, err := c.execCommand(ctx, containerName, cmd)
	if err != nil {
		return fmt.Errorf("failed to kill session: %w", err)
	}

	if exitCode != 0 {
		return fmt.Errorf("tmux kill-session failed: %s", output)
	}

	return nil
}

// execCommand executes a command in a container and returns the output and exit code.
func (c *Client) execCommand(ctx context.Context, containerName string, cmd []string) (string, int, error) {
	execConfig := container.ExecOptions{
		Cmd:          cmd,
		AttachStdout: true,
		AttachStderr: true,
	}

	execResp, err := c.docker.ContainerExecCreate(ctx, containerName, execConfig)
	if err != nil {
		return "", -1, fmt.Errorf("failed to create exec: %w", err)
	}

	attachResp, err := c.docker.ContainerExecAttach(ctx, execResp.ID, container.ExecStartOptions{})
	if err != nil {
		return "", -1, fmt.Errorf("failed to attach to exec: %w", err)
	}
	defer attachResp.Close()

	// Read all output
	var buf bytes.Buffer
	_, _ = io.Copy(&buf, attachResp.Reader)

	// Get exit code
	inspectResp, err := c.docker.ContainerExecInspect(ctx, execResp.ID)
	if err != nil {
		return buf.String(), -1, fmt.Errorf("failed to inspect exec: %w", err)
	}

	return buf.String(), inspectResp.ExitCode, nil
}

// parseTmuxSessions parses tmux list-sessions output into Session structs.
// Format: name:created_timestamp:attached(0/1):windows
func parseTmuxSessions(output string, shedName string) ([]config.Session, error) {
	lines := strings.Split(strings.TrimSpace(output), "\n")
	sessions := make([]config.Session, 0, len(lines))

	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		parts := strings.Split(line, ":")
		if len(parts) < 4 {
			continue // Skip malformed lines
		}

		name := parts[0]

		// Parse creation timestamp (Unix timestamp)
		createdAt := time.Time{}
		if ts, err := strconv.ParseInt(parts[1], 10, 64); err == nil {
			createdAt = time.Unix(ts, 0)
		}

		// Parse attached status (0 or 1)
		attached := parts[2] == "1"

		// Parse window count
		windowCount := 0
		if wc, err := strconv.Atoi(parts[3]); err == nil {
			windowCount = wc
		}

		sessions = append(sessions, config.Session{
			Name:        name,
			ShedName:    shedName,
			CreatedAt:   createdAt,
			Attached:    attached,
			WindowCount: windowCount,
		})
	}

	return sessions, nil
}
