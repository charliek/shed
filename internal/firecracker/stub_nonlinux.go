//go:build !linux
// +build !linux

package firecracker

import (
	"context"
	"fmt"
	"net"

	"github.com/charliek/shed/internal/backend"
	"github.com/charliek/shed/internal/config"
	"github.com/charliek/shed/internal/plugin"
	"github.com/charliek/shed/internal/vmimage"
)

// MaterializerHook returns nil on non-linux builds. The firecracker
// backend is linux-only, and even there the host-native mkfs.erofs
// path runs without a VM, so the registered hook is a no-op.
func MaterializerHook(imagesDir string) vmimage.MaterializerFunc {
	return nil
}

// errNonLinux returns a fresh sentinel-wrapped error for every call so
// errors.Is(err, ErrNotSupportedSentinel) catches it and the API layer
// maps it to HTTP 501 (Not Implemented).
func errNonLinux() error {
	return fmt.Errorf("%w: firecracker backend is only supported on linux", config.ErrNotSupportedSentinel)
}

// Client is a stub for non-linux builds.
type Client struct{}

// NewClient returns an error on non-linux platforms.
func NewClient(cfg *config.FirecrackerConfig, serverCfg *config.ServerConfig, _ *plugin.Bridge) (*Client, error) {
	return nil, errNonLinux()
}

// Close is a no-op for the stub client.
func (c *Client) Close() error {
	return nil
}

// FirecrackerBackend is a stub for non-linux builds.
type FirecrackerBackend struct{}

// NewBackend creates a stub backend.
func NewBackend(client *Client) *FirecrackerBackend {
	return &FirecrackerBackend{}
}

// Type returns the backend type identifier.
func (b *FirecrackerBackend) Type() backend.Type {
	return backend.TypeFirecracker
}

// Close releases any resources held by the backend.
func (b *FirecrackerBackend) Close() error {
	return nil
}

// CreateShed returns an error on non-linux platforms.
func (b *FirecrackerBackend) CreateShed(ctx context.Context, req config.CreateShedRequest) (*config.Shed, error) {
	return nil, errNonLinux()
}

// GetShed returns an error on non-linux platforms.
func (b *FirecrackerBackend) GetShed(ctx context.Context, name string) (*config.Shed, error) {
	return nil, errNonLinux()
}

// ListSheds returns an error on non-linux platforms.
func (b *FirecrackerBackend) ListSheds(ctx context.Context) ([]config.Shed, error) {
	return nil, errNonLinux()
}

// DeleteShed returns an error on non-linux platforms.
func (b *FirecrackerBackend) DeleteShed(ctx context.Context, name string, keepVolume bool) error {
	return errNonLinux()
}

// StartShed returns an error on non-linux platforms.
func (b *FirecrackerBackend) StartShed(ctx context.Context, name string) (*config.Shed, error) {
	return nil, errNonLinux()
}

// StopShed returns an error on non-linux platforms.
func (b *FirecrackerBackend) StopShed(ctx context.Context, name string) (*config.Shed, error) {
	return nil, errNonLinux()
}

// ResetShed returns an error on non-linux platforms.
func (b *FirecrackerBackend) ResetShed(ctx context.Context, name string) (*config.Shed, error) {
	return nil, errNonLinux()
}

// ListSessions returns an error on non-linux platforms.
func (b *FirecrackerBackend) ListSessions(ctx context.Context, shedName string) ([]config.Session, error) {
	return nil, errNonLinux()
}

// KillSession returns an error on non-linux platforms.
func (b *FirecrackerBackend) KillSession(ctx context.Context, shedName, sessionName string) error {
	return errNonLinux()
}

// Exec returns an error on non-linux platforms.
func (b *FirecrackerBackend) Exec(ctx context.Context, shedName string, opts backend.ExecOptions) error {
	return errNonLinux()
}

// GetNetworkEndpoint returns an error on non-linux platforms.
func (b *FirecrackerBackend) GetNetworkEndpoint(ctx context.Context, shedName string) (string, error) {
	return "", errNonLinux()
}

// DialService returns an error on non-linux platforms.
func (b *FirecrackerBackend) DialService(ctx context.Context, shedName string, port uint16) (net.Conn, error) {
	return nil, errNonLinux()
}

// ListImages returns an empty list on non-linux platforms.
func (b *FirecrackerBackend) ListImages(_ context.Context) ([]config.ImageInfo, error) {
	return nil, nil
}

// InspectImage returns an error on non-linux platforms.
func (b *FirecrackerBackend) InspectImage(_ context.Context, _ string) (config.ImageInspectResponse, error) {
	return config.ImageInspectResponse{}, fmt.Errorf("inspect image: %w", config.ErrNotSupportedSentinel)
}

// TagImage returns an error on non-linux platforms.
func (b *FirecrackerBackend) TagImage(_ context.Context, _, _ string) error {
	return fmt.Errorf("tag image: %w", config.ErrNotSupportedSentinel)
}

// PullImage returns an error on non-linux platforms.
func (b *FirecrackerBackend) PullImage(_ context.Context, _, _, _ string) (string, error) {
	return "", fmt.Errorf("pull image: %w", config.ErrNotSupportedSentinel)
}

// PushImage returns an error on non-linux platforms.
func (b *FirecrackerBackend) PushImage(_ context.Context, _, _ string) error {
	return fmt.Errorf("push image: %w", config.ErrNotSupportedSentinel)
}

// DeleteImage returns an error on non-linux platforms.
func (b *FirecrackerBackend) DeleteImage(_ context.Context, _ string) error {
	return fmt.Errorf("image management: %w", config.ErrNotSupportedSentinel)
}

// PruneImages returns an empty list on non-linux platforms.
func (b *FirecrackerBackend) PruneImages(_ context.Context, _ bool) ([]config.ImageInfo, error) {
	return nil, nil
}

// DiskUsage returns an empty disk-usage report on non-linux platforms.
// Read-only system queries never error on a non-native backend — they just
// report "no local state," matching the ListImages precedent.
func (b *FirecrackerBackend) DiskUsage(_ context.Context) (config.DiskUsage, error) {
	return config.DiskUsage{Backend: "none"}, nil
}

// Prune is a mutating operation; the non-native stub returns the sentinel.
func (b *FirecrackerBackend) Prune(_ context.Context, _ backend.PruneOptions) (config.PruneReport, error) {
	return config.PruneReport{}, fmt.Errorf("prune: %w", config.ErrNotSupportedSentinel)
}

// CreateSnapshot returns an error on non-linux platforms. Uses the
// not-supported sentinel so the API layer can map it consistently with the
// other unsupported snapshot methods (e.g., to 501 Not Implemented).
func (b *FirecrackerBackend) CreateSnapshot(_ context.Context, _ config.SnapshotCreateRequest) (*config.Snapshot, error) {
	return nil, fmt.Errorf("create snapshot: %w", config.ErrNotSupportedSentinel)
}

// ListSnapshots returns an empty list on non-linux platforms.
func (b *FirecrackerBackend) ListSnapshots(_ context.Context) ([]config.Snapshot, error) {
	return nil, nil
}

// GetSnapshot returns an error on non-linux platforms.
func (b *FirecrackerBackend) GetSnapshot(_ context.Context, _ string) (*config.Snapshot, error) {
	return nil, fmt.Errorf("get snapshot: %w", config.ErrNotSupportedSentinel)
}

// DeleteSnapshot returns an error on non-linux platforms.
func (b *FirecrackerBackend) DeleteSnapshot(_ context.Context, _ string) error {
	return fmt.Errorf("delete snapshot: %w", config.ErrNotSupportedSentinel)
}
