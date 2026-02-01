package provision

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/client"
	"github.com/docker/docker/pkg/stdcopy"
)

// ErrStateFileNotFound is returned when the provisioning state file doesn't exist.
// This is expected when install hasn't run yet.
var ErrStateFileNotFound = errors.New("state file does not exist")

// State file keys for tracking provisioning state.
const (
	StateKeyInstallRan = "install_ran"
	StateKeyError      = "error"
)

// stateFilePath is where we store provisioning state in the container.
const stateFilePath = "/var/log/shed/.provision_state"

// State provides methods for tracking provisioning state via files in the container.
// We use files instead of container labels because Docker doesn't support
// updating labels on running containers.
type State struct {
	docker *client.Client
}

// NewState creates a new provisioning state tracker.
func NewState(docker *client.Client) *State {
	return &State{docker: docker}
}

// HasInstallRun checks if the install hook has already run for a container.
// Returns false, nil if the state file doesn't exist (install hasn't run).
// Returns an error for unexpected failures (Docker API errors, permission issues, etc.).
func (s *State) HasInstallRun(ctx context.Context, containerID string) (bool, error) {
	state, err := s.readStateFile(ctx, containerID)
	if err != nil {
		if errors.Is(err, ErrStateFileNotFound) {
			// File doesn't exist means install hasn't run - this is expected
			return false, nil
		}
		// Unexpected error - propagate it
		return false, fmt.Errorf("failed to check install state: %w", err)
	}
	return state[StateKeyInstallRan] == "true", nil
}

// MarkInstallComplete marks the install hook as having run successfully.
func (s *State) MarkInstallComplete(ctx context.Context, containerID string) error {
	return s.writeStateFile(ctx, containerID, map[string]string{
		StateKeyInstallRan: "true",
	})
}

// MarkInstallFailed records that the install hook failed with the given error.
func (s *State) MarkInstallFailed(ctx context.Context, containerID string, err error) error {
	return s.writeStateFile(ctx, containerID, map[string]string{
		StateKeyError: err.Error(),
	})
}

// GetError returns the last recorded provisioning error, or empty string if none.
// Returns empty string with nil error if the state file doesn't exist.
// Returns an error for unexpected failures.
func (s *State) GetError(ctx context.Context, containerID string) (string, error) {
	state, err := s.readStateFile(ctx, containerID)
	if err != nil {
		if errors.Is(err, ErrStateFileNotFound) {
			// No state file means no error recorded
			return "", nil
		}
		return "", fmt.Errorf("failed to read error state: %w", err)
	}
	return state[StateKeyError], nil
}

// ClearError clears any recorded provisioning error.
func (s *State) ClearError(ctx context.Context, containerID string) error {
	// Read current state - if file doesn't exist, start with empty state
	state, err := s.readStateFile(ctx, containerID)
	if err != nil {
		if errors.Is(err, ErrStateFileNotFound) {
			state = make(map[string]string)
		} else {
			return fmt.Errorf("failed to read state for clearing error: %w", err)
		}
	}
	delete(state, StateKeyError)
	return s.writeStateFile(ctx, containerID, state)
}

// writeStateFile writes provisioning state to a file in the container.
// Uses atomic write pattern (write to temp file, then rename) to prevent corruption.
func (s *State) writeStateFile(ctx context.Context, containerID string, state map[string]string) error {
	// Build the content
	var content strings.Builder
	for key, value := range state {
		content.WriteString(fmt.Sprintf("%s=%s\n", key, value))
	}

	// Encode content as base64 to safely pass through shell without delimiter issues.
	// This avoids problems where error messages might contain heredoc delimiters.
	encoded := base64.StdEncoding.EncodeToString([]byte(content.String()))

	// Ensure directory exists, decode base64 content, write to temp file, then atomically rename
	tempFile := stateFilePath + ".tmp"
	cmd := []string{
		"bash", "-c",
		fmt.Sprintf("mkdir -p %s && echo %s | base64 -d > %s && mv %s %s",
			LogDir, encoded, tempFile, tempFile, stateFilePath),
	}

	execConfig := container.ExecOptions{
		Cmd:          cmd,
		AttachStdout: true,
		AttachStderr: true,
	}

	execResp, err := s.docker.ContainerExecCreate(ctx, containerID, execConfig)
	if err != nil {
		return fmt.Errorf("failed to create exec for state update: %w", err)
	}

	attachResp, err := s.docker.ContainerExecAttach(ctx, execResp.ID, container.ExecStartOptions{})
	if err != nil {
		return fmt.Errorf("failed to attach for state write: %w", err)
	}
	defer attachResp.Close()

	// Read output to wait for completion
	if _, err := io.Copy(io.Discard, attachResp.Reader); err != nil {
		return fmt.Errorf("failed to write state file: %w", err)
	}

	// Check exit code
	inspectResp, err := s.docker.ContainerExecInspect(ctx, execResp.ID)
	if err != nil {
		return fmt.Errorf("failed to inspect state write: %w", err)
	}
	if inspectResp.ExitCode != 0 {
		return fmt.Errorf("state write failed (exit code %d)", inspectResp.ExitCode)
	}

	return nil
}

// readStateFile reads provisioning state from the container.
func (s *State) readStateFile(ctx context.Context, containerID string) (map[string]string, error) {
	cmd := []string{"cat", stateFilePath}

	execConfig := container.ExecOptions{
		Cmd:          cmd,
		AttachStdout: true,
		AttachStderr: true,
	}

	execResp, err := s.docker.ContainerExecCreate(ctx, containerID, execConfig)
	if err != nil {
		return nil, fmt.Errorf("failed to create exec for state read: %w", err)
	}

	attachResp, err := s.docker.ContainerExecAttach(ctx, execResp.ID, container.ExecStartOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to attach for state read: %w", err)
	}
	defer attachResp.Close()

	// Docker multiplexes stdout/stderr with headers, use stdcopy to demux
	var stdout, stderr bytes.Buffer
	if _, err := stdcopy.StdCopy(&stdout, &stderr, attachResp.Reader); err != nil {
		return nil, fmt.Errorf("failed to read state file: %w", err)
	}

	// Check exit code - if cat failed, determine why
	inspectResp, err := s.docker.ContainerExecInspect(ctx, execResp.ID)
	if err != nil {
		return nil, fmt.Errorf("failed to inspect exec: %w", err)
	}
	if inspectResp.ExitCode != 0 {
		stderrStr := stderr.String()
		// cat returns exit 1 for "No such file", check stderr to be sure
		if strings.Contains(stderrStr, "No such file") {
			return nil, ErrStateFileNotFound
		}
		return nil, fmt.Errorf("failed to read state file (exit code %d): %s", inspectResp.ExitCode, stderrStr)
	}

	// Parse key=value pairs
	state := make(map[string]string)
	lines := strings.Split(stdout.String(), "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		// Find first = to split key and value
		if idx := strings.Index(line, "="); idx > 0 {
			key := line[:idx]
			value := line[idx+1:]
			state[key] = value
		}
	}

	return state, nil
}

// ReadInstallLog reads the install hook log file from the container.
func (s *State) ReadInstallLog(ctx context.Context, containerID string) (string, error) {
	return s.readLogFile(ctx, containerID, InstallLog)
}

// ReadStartupLog reads the startup hook log file from the container.
func (s *State) ReadStartupLog(ctx context.Context, containerID string) (string, error) {
	return s.readLogFile(ctx, containerID, StartupLog)
}

// readLogFile reads a log file from the container.
func (s *State) readLogFile(ctx context.Context, containerID string, logPath string) (string, error) {
	cmd := []string{"cat", logPath}

	execConfig := container.ExecOptions{
		Cmd:          cmd,
		AttachStdout: true,
		AttachStderr: true,
	}

	execResp, err := s.docker.ContainerExecCreate(ctx, containerID, execConfig)
	if err != nil {
		return "", fmt.Errorf("failed to create exec: %w", err)
	}

	attachResp, err := s.docker.ContainerExecAttach(ctx, execResp.ID, container.ExecStartOptions{})
	if err != nil {
		return "", fmt.Errorf("failed to attach: %w", err)
	}
	defer attachResp.Close()

	// Docker multiplexes stdout/stderr with headers, capture stderr for error messages
	var stdout, stderr bytes.Buffer
	if _, err := stdcopy.StdCopy(&stdout, &stderr, attachResp.Reader); err != nil {
		return "", fmt.Errorf("failed to read log: %w", err)
	}

	// Check exit code
	inspectResp, err := s.docker.ContainerExecInspect(ctx, execResp.ID)
	if err != nil {
		return "", fmt.Errorf("failed to inspect exec: %w", err)
	}
	if inspectResp.ExitCode != 0 {
		stderrStr := stderr.String()
		// If file doesn't exist, return empty string (expected when hook hasn't run)
		if strings.Contains(stderrStr, "No such file") {
			return "", nil
		}
		return "", fmt.Errorf("failed to read log file (exit code %d): %s", inspectResp.ExitCode, stderrStr)
	}

	return stdout.String(), nil
}
