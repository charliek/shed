//go:build !darwin
// +build !darwin

package vz

import (
	"context"
	"fmt"
	"net"

	"github.com/charliek/shed/internal/backend"
	"github.com/charliek/shed/internal/config"
	"github.com/charliek/shed/internal/egress"
	"github.com/charliek/shed/internal/plugin"
)

// errNonDarwin wraps ErrNotSupportedSentinel so the API layer maps
// every VZ-on-Linux call to HTTP 501 (Not Implemented) rather than a
// generic 500 backend error.
var errNonDarwin = fmt.Errorf("%w: vz backend is only supported on macOS", config.ErrNotSupportedSentinel)

// Client is a stub for non-darwin builds.
type Client struct{}

// NewClient returns an error on non-darwin platforms.
func NewClient(cfg *config.VZConfig, serverCfg *config.ServerConfig, _ *plugin.Bridge) (*Client, error) {
	return nil, errNonDarwin
}

// Close is a no-op for the stub client.
func (c *Client) Close() error {
	return nil
}

// SetEgressManager is a no-op for the stub client.
func (c *Client) SetEgressManager(_ *egress.Manager) {}

// VZBackend is a stub for non-darwin builds.
type VZBackend struct{}

// NewBackend creates a stub backend.
func NewBackend(client *Client) *VZBackend {
	return &VZBackend{}
}

// Type returns the backend type identifier.
func (b *VZBackend) Type() backend.Type {
	return backend.TypeVZ
}

// Close releases any resources held by the backend.
func (b *VZBackend) Close() error {
	return nil
}

// CreateShed returns an error on non-darwin platforms.
func (b *VZBackend) CreateShed(ctx context.Context, req config.CreateShedRequest) (*config.Shed, error) {
	return nil, errNonDarwin
}

// GetShed returns an error on non-darwin platforms.
func (b *VZBackend) GetShed(ctx context.Context, name string) (*config.Shed, error) {
	return nil, errNonDarwin
}

// ListSheds returns an error on non-darwin platforms.
func (b *VZBackend) ListSheds(ctx context.Context) ([]config.Shed, error) {
	return nil, errNonDarwin
}

// DeleteShed returns an error on non-darwin platforms.
func (b *VZBackend) DeleteShed(ctx context.Context, name string, keepVolume bool) error {
	return errNonDarwin
}

// StartShed returns an error on non-darwin platforms.
func (b *VZBackend) StartShed(ctx context.Context, name string) (*config.Shed, error) {
	return nil, errNonDarwin
}

// StopShed returns an error on non-darwin platforms.
func (b *VZBackend) StopShed(ctx context.Context, name string) (*config.Shed, error) {
	return nil, errNonDarwin
}

// ResetShed returns an error on non-darwin platforms.
func (b *VZBackend) ResetShed(ctx context.Context, name string) (*config.Shed, error) {
	return nil, errNonDarwin
}

// ListSessions returns an error on non-darwin platforms.
func (b *VZBackend) ListSessions(ctx context.Context, shedName string) ([]config.Session, error) {
	return nil, errNonDarwin
}

// KillSession returns an error on non-darwin platforms.
func (b *VZBackend) KillSession(ctx context.Context, shedName, sessionName string) error {
	return errNonDarwin
}

// Exec returns an error on non-darwin platforms.
func (b *VZBackend) Exec(ctx context.Context, shedName string, opts backend.ExecOptions) error {
	return errNonDarwin
}

// DialService returns an error on non-darwin platforms.
func (b *VZBackend) DialService(ctx context.Context, shedName string, port uint16) (net.Conn, error) {
	return nil, errNonDarwin
}

// ListImages returns an empty list on non-darwin platforms.
func (b *VZBackend) ListImages(_ context.Context) ([]config.ImageInfo, error) {
	return nil, nil
}

// InspectImage returns an error on non-darwin platforms.
func (b *VZBackend) InspectImage(_ context.Context, _ string) (config.ImageInspectResponse, error) {
	return config.ImageInspectResponse{}, fmt.Errorf("inspect image: %w", config.ErrNotSupportedSentinel)
}

// TagImage returns an error on non-darwin platforms.
func (b *VZBackend) TagImage(_ context.Context, _, _ string) error {
	return fmt.Errorf("tag image: %w", config.ErrNotSupportedSentinel)
}

// PullImage returns an error on non-darwin platforms.
func (b *VZBackend) PullImage(_ context.Context, _, _, _ string, _ bool) (string, error) {
	return "", fmt.Errorf("pull image: %w", config.ErrNotSupportedSentinel)
}

// PushImage returns an error on non-darwin platforms.
func (b *VZBackend) PushImage(_ context.Context, _, _ string) error {
	return fmt.Errorf("push image: %w", config.ErrNotSupportedSentinel)
}

// DeleteImage returns an error on non-darwin platforms.
func (b *VZBackend) DeleteImage(_ context.Context, _ string) error {
	return fmt.Errorf("image management: %w", config.ErrNotSupportedSentinel)
}

// PruneImages returns an empty list on non-darwin platforms.
func (b *VZBackend) PruneImages(_ context.Context, _ bool) ([]config.ImageInfo, error) {
	return nil, nil
}

// DiskUsage returns an empty disk-usage report on non-darwin platforms.
// Read-only system queries never error on a non-native backend — they just
// report "no local state," matching the ListImages precedent.
func (b *VZBackend) DiskUsage(_ context.Context) (config.DiskUsage, error) {
	return config.DiskUsage{Backend: "none"}, nil
}

// Prune is a mutating operation; the non-native stub returns the sentinel
// so callers get a clear "not supported" signal (matches DeleteImage).
func (b *VZBackend) Prune(_ context.Context, _ backend.PruneOptions) (config.PruneReport, error) {
	return config.PruneReport{}, fmt.Errorf("prune: %w", config.ErrNotSupportedSentinel)
}

// CreateSnapshot returns an error on non-darwin platforms. Uses the
// not-supported sentinel so the API layer can map it consistently with the
// other unsupported snapshot methods (e.g., to 501 Not Implemented).
func (b *VZBackend) CreateSnapshot(_ context.Context, _ config.SnapshotCreateRequest) (*config.Snapshot, error) {
	return nil, fmt.Errorf("create snapshot: %w", config.ErrNotSupportedSentinel)
}

// ListSnapshots returns an empty list on non-darwin platforms.
func (b *VZBackend) ListSnapshots(_ context.Context) ([]config.Snapshot, error) {
	return nil, nil
}

// GetSnapshot returns an error on non-darwin platforms.
func (b *VZBackend) GetSnapshot(_ context.Context, _ string) (*config.Snapshot, error) {
	return nil, fmt.Errorf("get snapshot: %w", config.ErrNotSupportedSentinel)
}

// DeleteSnapshot returns an error on non-darwin platforms.
func (b *VZBackend) DeleteSnapshot(_ context.Context, _ string) error {
	return fmt.Errorf("delete snapshot: %w", config.ErrNotSupportedSentinel)
}
