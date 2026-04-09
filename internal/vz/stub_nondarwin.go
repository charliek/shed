//go:build !darwin
// +build !darwin

package vz

import (
	"context"
	"errors"
	"fmt"
	"net"

	"github.com/charliek/shed/internal/backend"
	"github.com/charliek/shed/internal/config"
	"github.com/charliek/shed/internal/plugin"
)

var errNonDarwin = errors.New("vz backend is only supported on macOS")

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

// GetNetworkEndpoint returns an error on non-darwin platforms.
func (b *VZBackend) GetNetworkEndpoint(ctx context.Context, shedName string) (string, error) {
	return "", errNonDarwin
}

// DialService returns an error on non-darwin platforms.
func (b *VZBackend) DialService(ctx context.Context, shedName string, port uint16) (net.Conn, error) {
	return nil, errNonDarwin
}

// ListImages returns an empty list on non-darwin platforms.
func (b *VZBackend) ListImages(_ context.Context) ([]config.ImageInfo, error) {
	return nil, nil
}

// DeleteImage returns an error on non-darwin platforms.
func (b *VZBackend) DeleteImage(_ context.Context, _ string) error {
	return fmt.Errorf("image management: %w", config.ErrNotSupportedSentinel)
}

// PruneImages returns an empty list on non-darwin platforms.
func (b *VZBackend) PruneImages(_ context.Context, _ bool) ([]config.ImageInfo, error) {
	return nil, nil
}
