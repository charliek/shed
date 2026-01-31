package docker

import (
	"bytes"
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/pkg/stdcopy"

	"github.com/charliek/shed/internal/config"
)

// Re-export sentinel errors from config for backward compatibility.
var (
	ErrTmuxNotAvailable = config.ErrTmuxNotAvailableSentinel
	ErrSessionNotFound  = config.ErrSessionNotFoundSentinel
	ErrShedNotRunning   = config.ErrShedNotRunningSentinel
)

// ListSessions returns all tmux sessions in a shed container.
// Returns an empty list if the container has no sessions.
// Returns ErrTmuxNotAvailable if tmux is not installed in the container.
// Returns ErrShedNotRunning if the container is not running.
func (c *Client) ListSessions(ctx context.Context, shedName string) ([]config.Session, error) {
	containerName := config.ContainerName(shedName)

	// First check if the container is running
	shed, err := c.GetShed(ctx, shedName)
	if err != nil {
		return nil, err
	}
	if shed.Status != config.StatusRunning {
		return nil, fmt.Errorf("shed %q: %w", shedName, ErrShedNotRunning)
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

	return parseTmuxSessions(output, shedName), nil
}

// SessionExists checks if a tmux session exists in a shed container.
// Returns (false, nil) when the session doesn't exist (including when tmux server isn't running).
// Returns ErrTmuxNotAvailable if tmux is not installed in the container.
func (c *Client) SessionExists(ctx context.Context, shedName, sessionName string) (bool, error) {
	containerName := config.ContainerName(shedName)

	// First check if the container is running
	shed, err := c.GetShed(ctx, shedName)
	if err != nil {
		return false, err
	}
	if shed.Status != config.StatusRunning {
		return false, fmt.Errorf("shed %q: %w", shedName, ErrShedNotRunning)
	}

	cmd := []string{"tmux", "has-session", "-t", sessionName}

	output, exitCode, err := c.execCommand(ctx, containerName, cmd)
	if err != nil {
		return false, fmt.Errorf("failed to check session: %w", err)
	}

	// Exit code 0 means session exists
	if exitCode == 0 {
		return true, nil
	}

	// Check if tmux is not available
	if strings.Contains(output, "not found") || strings.Contains(output, "command not found") {
		return false, ErrTmuxNotAvailable
	}

	// "no server running" or "can't find session" means session doesn't exist
	if strings.Contains(output, "no server running") || strings.Contains(output, "can't find session") {
		return false, nil
	}

	// Other non-zero exits are unexpected errors
	return false, fmt.Errorf("tmux has-session failed: %s", output)
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
		return fmt.Errorf("shed %q: %w", shedName, ErrShedNotRunning)
	}

	// Kill the session directly and handle "not found" from tmux output
	cmd := []string{"tmux", "kill-session", "-t", sessionName}

	output, exitCode, err := c.execCommand(ctx, containerName, cmd)
	if err != nil {
		return fmt.Errorf("failed to kill session: %w", err)
	}

	if exitCode != 0 {
		// Check if session doesn't exist
		if strings.Contains(output, "can't find session") || strings.Contains(output, "no session") {
			return fmt.Errorf("session %q: %w", sessionName, ErrSessionNotFound)
		}
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

	// Read all output, demultiplexing stdout and stderr
	// When TTY is false (the default), Docker multiplexes the streams with framing bytes
	var stdout, stderr bytes.Buffer
	if _, err := stdcopy.StdCopy(&stdout, &stderr, attachResp.Reader); err != nil {
		return "", -1, fmt.Errorf("failed to read exec output: %w", err)
	}

	// Combine stdout and stderr for the output
	output := stdout.String()
	if stderr.Len() > 0 {
		output += stderr.String()
	}

	// Get exit code
	inspectResp, err := c.docker.ContainerExecInspect(ctx, execResp.ID)
	if err != nil {
		return output, -1, fmt.Errorf("failed to inspect exec: %w", err)
	}

	return output, inspectResp.ExitCode, nil
}

// parseTmuxSessions parses tmux list-sessions output into Session structs.
// Format: name:created_timestamp:attached(0/1):windows
// Malformed lines are silently skipped.
func parseTmuxSessions(output string, shedName string) []config.Session {
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

	return sessions
}
