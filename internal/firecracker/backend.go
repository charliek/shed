//go:build linux
// +build linux

package firecracker

import (
	"context"
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"github.com/charliek/shed/internal/backend"
	"github.com/charliek/shed/internal/config"
)

// Compile-time check that FirecrackerBackend implements backend.Backend.
var _ backend.Backend = (*FirecrackerBackend)(nil)

// FirecrackerBackend implements backend.Backend using Firecracker VMs.
type FirecrackerBackend struct {
	client *Client
}

// NewBackend creates a new FirecrackerBackend wrapping the given Client.
func NewBackend(client *Client) *FirecrackerBackend {
	return &FirecrackerBackend{client: client}
}

// Type returns the backend type identifier.
func (b *FirecrackerBackend) Type() backend.Type {
	return backend.TypeFirecracker
}

// Close releases any resources held by the backend.
func (b *FirecrackerBackend) Close() error {
	return b.client.Close()
}

// CreateShed creates a new shed with the given configuration.
func (b *FirecrackerBackend) CreateShed(ctx context.Context, req config.CreateShedRequest) (*config.Shed, error) {
	return b.client.CreateShed(ctx, req)
}

// GetShed returns a shed by name, or an error if not found.
func (b *FirecrackerBackend) GetShed(ctx context.Context, name string) (*config.Shed, error) {
	return b.client.GetShed(ctx, name)
}

// ListSheds returns all sheds managed by this backend.
func (b *FirecrackerBackend) ListSheds(ctx context.Context) ([]config.Shed, error) {
	return b.client.ListSheds(ctx)
}

// DeleteShed removes a shed. If keepVolume is true, the workspace is preserved.
// Note: For Firecracker, keepVolume is ignored as the rootfs is always part of the instance.
func (b *FirecrackerBackend) DeleteShed(ctx context.Context, name string, keepVolume bool) error {
	return b.client.DeleteShed(ctx, name, keepVolume)
}

// StartShed starts a stopped shed.
func (b *FirecrackerBackend) StartShed(ctx context.Context, name string) (*config.Shed, error) {
	return b.client.StartShed(ctx, name)
}

// StopShed stops a running shed.
func (b *FirecrackerBackend) StopShed(ctx context.Context, name string) (*config.Shed, error) {
	return b.client.StopShed(ctx, name)
}

// ListSessions returns all tmux sessions in a shed.
func (b *FirecrackerBackend) ListSessions(ctx context.Context, shedName string) ([]config.Session, error) {
	meta, err := LoadMetadata(b.client.cfg.InstanceDir, shedName)
	if err != nil {
		return nil, err
	}

	if meta.Status != config.StatusRunning {
		return nil, config.ErrShedNotRunningSentinel
	}

	// Execute tmux list-sessions via vsock
	vsockPath := filepath.Join(b.client.cfg.SocketDir, fmt.Sprintf("%s.vsock", meta.Name))
	vsockClient := NewVsockClient(vsockPath, b.client.cfg.ConsolePort, b.client.cfg.HealthPort)

	// Create a simple exec to run tmux list-sessions
	// We'll capture output and parse it
	var output strings.Builder
	opts := backend.ExecOptions{
		Cmd:    []string{"tmux", "list-sessions", "-F", "#{session_name}:#{session_created}:#{session_attached}:#{session_windows}"},
		Stdout: &nopWriteCloser{&output},
		TTY:    false,
	}

	if err := vsockClient.Exec(ctx, opts); err != nil {
		// tmux server might not be running
		if strings.Contains(err.Error(), "no server running") || strings.Contains(err.Error(), "exit code") {
			return []config.Session{}, nil // No sessions
		}
		return nil, fmt.Errorf("failed to list sessions: %w", err)
	}

	// Parse output
	sessions := []config.Session{}
	lines := strings.Split(strings.TrimSpace(output.String()), "\n")
	for _, line := range lines {
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, ":", 4)
		if len(parts) < 4 {
			continue
		}

		sessions = append(sessions, config.Session{
			Name:     parts[0],
			ShedName: shedName,
			Attached: parts[2] == "1",
		})
	}

	return sessions, nil
}

// KillSession terminates a tmux session in a shed.
func (b *FirecrackerBackend) KillSession(ctx context.Context, shedName, sessionName string) error {
	meta, err := LoadMetadata(b.client.cfg.InstanceDir, shedName)
	if err != nil {
		return err
	}

	if meta.Status != config.StatusRunning {
		return config.ErrShedNotRunningSentinel
	}

	// Execute tmux kill-session via vsock
	vsockPath := filepath.Join(b.client.cfg.SocketDir, fmt.Sprintf("%s.vsock", meta.Name))
	vsockClient := NewVsockClient(vsockPath, b.client.cfg.ConsolePort, b.client.cfg.HealthPort)

	opts := backend.ExecOptions{
		Cmd: []string{"tmux", "kill-session", "-t", sessionName},
		TTY: false,
	}

	if err := vsockClient.Exec(ctx, opts); err != nil {
		if strings.Contains(err.Error(), "no server running") {
			return config.ErrSessionNotFoundSentinel
		}
		if strings.Contains(err.Error(), "session not found") {
			return config.ErrSessionNotFoundSentinel
		}
		return fmt.Errorf("failed to kill session: %w", err)
	}

	return nil
}

// Exec executes a command in a shed with the given options.
func (b *FirecrackerBackend) Exec(ctx context.Context, shedName string, opts backend.ExecOptions) error {
	meta, err := LoadMetadata(b.client.cfg.InstanceDir, shedName)
	if err != nil {
		return err
	}

	if meta.Status != config.StatusRunning {
		return config.ErrShedNotRunningSentinel
	}

	// Build command - if empty, use default login shell
	cmd := opts.Cmd
	if len(cmd) == 0 {
		cmd = []string{"/bin/bash", "--login"}
	} else {
		if containsShellOperators(cmd) {
			cmd = []string{"/bin/sh", "-c", buildShellCommand(cmd)}
		}
	}
	opts.Cmd = cmd

	// Execute via vsock
	vsockPath := filepath.Join(b.client.cfg.SocketDir, fmt.Sprintf("%s.vsock", meta.Name))
	vsockClient := NewVsockClient(vsockPath, b.client.cfg.ConsolePort, b.client.cfg.HealthPort)
	return vsockClient.Exec(ctx, opts)
}

func containsShellOperators(cmd []string) bool {
	for _, token := range cmd {
		if isShellOperatorToken(token) {
			return true
		}
	}
	return false
}

func isShellOperatorToken(token string) bool {
	switch token {
	case "&&", "||", "|", ";", "&", ">", ">>", "<", "2>", "2>>", "|&", "(", ")":
		return true
	default:
		return false
	}
}

func buildShellCommand(cmd []string) string {
	parts := make([]string, 0, len(cmd))
	for _, token := range cmd {
		if isShellOperatorToken(token) {
			parts = append(parts, token)
			continue
		}
		parts = append(parts, shellEscape(token))
	}
	return strings.Join(parts, " ")
}

func shellEscape(arg string) string {
	if arg == "" {
		return "''"
	}
	if !strings.ContainsAny(arg, " \t\n'\"\\$&;|<>`()") {
		return arg
	}
	return "'" + strings.ReplaceAll(arg, "'", `'"'"'`) + "'"
}

// GetNetworkEndpoint returns the network endpoint (IP) for a shed.
func (b *FirecrackerBackend) GetNetworkEndpoint(ctx context.Context, shedName string) (string, error) {
	return b.client.GetNetworkEndpoint(ctx, shedName)
}

// nopWriteCloser wraps an io.Writer to implement io.WriteCloser.
type nopWriteCloser struct {
	w io.Writer
}

func (n *nopWriteCloser) Write(p []byte) (int, error) {
	return n.w.Write(p)
}

func (n *nopWriteCloser) Close() error {
	return nil
}
