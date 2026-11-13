package remote

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"

	"github.com/charliek/shed/internal/backend"
	"github.com/charliek/shed/internal/config"
	"github.com/charliek/shed/internal/remote/state"
	"github.com/charliek/shed/internal/remote/transport"
	"github.com/charliek/shed/internal/vmutil"
)

// running returns a copy of the record of a shed that is running, with the
// caller's ownership requirements checked against it. Any other status is
// config.ErrShedNotRunningSentinel, which the API answers with 409.
func (b *Backend) running(ctx context.Context, name string) (*state.Record, error) {
	if err := b.checkReady(); err != nil {
		return nil, err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	r, err := b.lookupLocked(ctx, name)
	if err != nil {
		return nil, err
	}
	if r.Shed.Status != config.StatusRunning {
		return nil, fmt.Errorf("%w: %s is %s", config.ErrShedNotRunningSentinel, name, r.Shed.Status)
	}
	return r.Clone(), nil
}

// Exec runs a command in a running shed through its agent.
func (b *Backend) Exec(ctx context.Context, shedName string, opts backend.ExecOptions) error {
	rec, err := b.running(ctx, shedName)
	if err != nil {
		return err
	}
	agent, err := b.agentFor(rec)
	if err != nil {
		return err
	}
	// Empty argv is an interactive login shell, as on the local backends.
	if len(opts.Cmd) == 0 {
		opts.Cmd = []string{"/bin/bash", "--login"}
	}
	return agent.Exec(ctx, opts)
}

// ListSessions returns the tmux sessions in a running shed.
func (b *Backend) ListSessions(ctx context.Context, shedName string) ([]config.Session, error) {
	rec, err := b.running(ctx, shedName)
	if err != nil {
		return nil, err
	}
	agent, err := b.agentFor(rec)
	if err != nil {
		return nil, err
	}
	var output strings.Builder
	err = agent.Exec(ctx, backend.ExecOptions{
		Cmd:    []string{"tmux", "list-sessions", "-F", "#{session_name}:#{session_created}:#{session_attached}:#{session_windows}"},
		Stdout: vmutil.NopWriteCloser(&output),
	})
	if err != nil {
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
	for _, line := range strings.Split(strings.TrimSpace(output.String()), "\n") {
		parts := strings.SplitN(line, ":", 4)
		if len(parts) < 4 {
			continue
		}
		sessions = append(sessions, config.Session{Name: parts[0], ShedName: shedName, Attached: parts[2] == "1"})
	}
	return sessions, nil
}

// KillSession ends a tmux session in a running shed.
func (b *Backend) KillSession(ctx context.Context, shedName, sessionName string) error {
	rec, err := b.running(ctx, shedName)
	if err != nil {
		return err
	}
	agent, err := b.agentFor(rec)
	if err != nil {
		return err
	}
	if err := agent.Exec(ctx, backend.ExecOptions{Cmd: []string{"tmux", "kill-session", "-t", sessionName}}); err != nil {
		if strings.Contains(err.Error(), "no server running") || strings.Contains(err.Error(), "session not found") {
			return config.ErrSessionNotFoundSentinel
		}
		var exitErr *vmutil.ExitError
		if errors.As(err, &exitErr) && exitErr.Code == 1 {
			return config.ErrSessionNotFoundSentinel
		}
		return fmt.Errorf("failed to kill session: %w", err)
	}
	return nil
}

// DialService opens a connection to a TCP port inside a running shed, through
// the agent's proxy port.
func (b *Backend) DialService(ctx context.Context, shedName string, port uint16) (net.Conn, error) {
	rec, err := b.running(ctx, shedName)
	if err != nil {
		return nil, err
	}
	link, err := b.linkFor(rec)
	if err != nil {
		return nil, err
	}
	return vmutil.DialService(ctx, link, transport.PortTCPProxy, port)
}
