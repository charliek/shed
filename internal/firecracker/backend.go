//go:build linux
// +build linux

package firecracker

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/charliek/shed/internal/backend"
	"github.com/charliek/shed/internal/config"
	"github.com/charliek/shed/internal/vmutil"
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

	agent := b.client.newAgentClient(meta.Name)

	var output strings.Builder
	opts := backend.ExecOptions{
		Cmd:    []string{"tmux", "list-sessions", "-F", "#{session_name}:#{session_created}:#{session_attached}:#{session_windows}"},
		Stdout: vmutil.NopWriteCloser(&output),
		TTY:    false,
	}

	if err := agent.Exec(ctx, opts); err != nil {
		if strings.Contains(err.Error(), "no server running") {
			return []config.Session{}, nil
		}
		var exitErr *vmutil.ExitError
		if errors.As(err, &exitErr) && exitErr.Code == 1 {
			return []config.Session{}, nil
		}
		return nil, fmt.Errorf("failed to list sessions: %w", err)
	}

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

	agent := b.client.newAgentClient(meta.Name)

	opts := backend.ExecOptions{
		Cmd: []string{"tmux", "kill-session", "-t", sessionName},
		TTY: false,
	}

	if err := agent.Exec(ctx, opts); err != nil {
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

	cmd := opts.Cmd
	if len(cmd) == 0 {
		cmd = []string{"/bin/bash", "--login"}
	} else {
		cmd = []string{"/bin/bash", "--login", "-c", strings.Join(cmd, " ")}
	}
	opts.Cmd = cmd

	agent := b.client.newAgentClient(meta.Name)
	return agent.Exec(ctx, opts)
}

// GetNetworkEndpoint returns the network endpoint (IP) for a shed.
func (b *FirecrackerBackend) GetNetworkEndpoint(ctx context.Context, shedName string) (string, error) {
	return b.client.GetNetworkEndpoint(ctx, shedName)
}
