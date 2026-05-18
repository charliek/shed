//go:build darwin
// +build darwin

package vz

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"

	"github.com/charliek/shed/internal/backend"
	"github.com/charliek/shed/internal/config"
	"github.com/charliek/shed/internal/vmutil"
)

// Compile-time check that VZBackend implements backend.Backend.
var _ backend.Backend = (*VZBackend)(nil)

// VZBackend implements backend.Backend using Apple Virtualization.framework VMs via vfkit.
type VZBackend struct {
	client *Client
}

// NewBackend creates a new VZBackend wrapping the given Client.
func NewBackend(client *Client) *VZBackend {
	return &VZBackend{client: client}
}

// Type returns the backend type identifier.
func (b *VZBackend) Type() backend.Type {
	return backend.TypeVZ
}

// Close releases any resources held by the backend.
func (b *VZBackend) Close() error {
	return b.client.Close()
}

// CreateShed creates a new shed with the given configuration.
func (b *VZBackend) CreateShed(ctx context.Context, req config.CreateShedRequest) (*config.Shed, error) {
	return b.client.CreateShed(ctx, req)
}

// GetShed returns a shed by name, or an error if not found.
func (b *VZBackend) GetShed(ctx context.Context, name string) (*config.Shed, error) {
	return b.client.GetShed(ctx, name)
}

// ListSheds returns all sheds managed by this backend.
func (b *VZBackend) ListSheds(ctx context.Context) ([]config.Shed, error) {
	return b.client.ListSheds(ctx)
}

// DeleteShed removes a shed.
func (b *VZBackend) DeleteShed(ctx context.Context, name string, keepVolume bool) error {
	return b.client.DeleteShed(ctx, name, keepVolume)
}

// StartShed starts a stopped shed.
func (b *VZBackend) StartShed(ctx context.Context, name string) (*config.Shed, error) {
	return b.client.StartShed(ctx, name)
}

// StopShed stops a running shed.
func (b *VZBackend) StopShed(ctx context.Context, name string) (*config.Shed, error) {
	return b.client.StopShed(ctx, name)
}

// ResetShed wipes and recreates a stopped shed's writable upper layer.
func (b *VZBackend) ResetShed(ctx context.Context, name string) (*config.Shed, error) {
	return b.client.ResetShed(ctx, name)
}

// newAgentClient creates a vmutil.AgentClient for the given instance name.
func (b *VZBackend) newAgentClient(name string) *vmutil.AgentClient {
	dialer := NewVZDialer(b.client.cfg.SocketDir, name)
	return vmutil.NewAgentClient(dialer, b.client.cfg.ConsolePort, b.client.cfg.NotifyPort)
}

// ListSessions returns all tmux sessions in a shed.
func (b *VZBackend) ListSessions(ctx context.Context, shedName string) ([]config.Session, error) {
	meta, err := LoadMetadata(b.client.cfg.InstanceDir, shedName)
	if err != nil {
		return nil, err
	}

	if meta.Status != config.StatusRunning {
		return nil, config.ErrShedNotRunningSentinel
	}

	agent := b.newAgentClient(meta.Name)

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
func (b *VZBackend) KillSession(ctx context.Context, shedName, sessionName string) error {
	meta, err := LoadMetadata(b.client.cfg.InstanceDir, shedName)
	if err != nil {
		return err
	}

	if meta.Status != config.StatusRunning {
		return config.ErrShedNotRunningSentinel
	}

	agent := b.newAgentClient(meta.Name)

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
func (b *VZBackend) Exec(ctx context.Context, shedName string, opts backend.ExecOptions) error {
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

	agent := b.newAgentClient(meta.Name)
	return agent.Exec(ctx, opts)
}

// GetNetworkEndpoint returns the network endpoint for a shed.
func (b *VZBackend) GetNetworkEndpoint(ctx context.Context, shedName string) (string, error) {
	return b.client.GetNetworkEndpoint(ctx, shedName)
}

// DialService opens a TCP connection to a port inside a running shed's VM.
func (b *VZBackend) DialService(ctx context.Context, shedName string, port uint16) (net.Conn, error) {
	return b.client.DialService(ctx, shedName, port)
}

// ListImages returns available VZ image variants from config and the blob store.
func (b *VZBackend) ListImages(_ context.Context) ([]config.ImageInfo, error) {
	return b.client.ListImages()
}

// InspectImage returns full details for a tag or digest.
func (b *VZBackend) InspectImage(_ context.Context, tagOrDigest string) (config.ImageInspectResponse, error) {
	return b.client.InspectImage(tagOrDigest)
}

// TagImage points newTag at the digest currently held by srcTagOrDigest.
func (b *VZBackend) TagImage(_ context.Context, src, dst string) error {
	return b.client.TagImage(src, dst)
}

// PullImage pulls a Docker reference into the blob store under the named tag.
func (b *VZBackend) PullImage(ctx context.Context, dockerRef, tag, platform string) (string, error) {
	return b.client.PullImage(ctx, dockerRef, tag, platform)
}

// PushImage uploads the manifest held by tagOrDigest to dstRef.
func (b *VZBackend) PushImage(ctx context.Context, tagOrDigest, dstRef string) error {
	return b.client.PushImage(ctx, tagOrDigest, dstRef)
}

// DeleteImage removes a tag.
func (b *VZBackend) DeleteImage(_ context.Context, name string) error {
	return b.client.DeleteImage(name)
}

// PruneImages removes blobs not protected by any shed/snapshot.
func (b *VZBackend) PruneImages(_ context.Context, dryRun bool) ([]config.ImageInfo, error) {
	return b.client.PruneImages(dryRun)
}

// DiskUsage returns disk-usage information for the VZ server.
func (b *VZBackend) DiskUsage(ctx context.Context) (config.DiskUsage, error) {
	return b.client.DiskUsage(ctx)
}

// Prune runs the disk cleanup pass. See backend.PruneOptions for semantics.
func (b *VZBackend) Prune(ctx context.Context, opts backend.PruneOptions) (config.PruneReport, error) {
	return b.client.Prune(ctx, opts)
}

// CreateSnapshot captures a stopped shed's rootfs as a named, immutable artifact.
func (b *VZBackend) CreateSnapshot(ctx context.Context, req config.SnapshotCreateRequest) (*config.Snapshot, error) {
	return b.client.CreateSnapshot(ctx, req)
}

// ListSnapshots returns all snapshots stored on this server.
func (b *VZBackend) ListSnapshots(ctx context.Context) ([]config.Snapshot, error) {
	return b.client.ListSnapshots(ctx)
}

// GetSnapshot returns a snapshot by name.
func (b *VZBackend) GetSnapshot(ctx context.Context, name string) (*config.Snapshot, error) {
	return b.client.GetSnapshot(ctx, name)
}

// DeleteSnapshot removes a snapshot.
func (b *VZBackend) DeleteSnapshot(ctx context.Context, name string) error {
	return b.client.DeleteSnapshot(ctx, name)
}
