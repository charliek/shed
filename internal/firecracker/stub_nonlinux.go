//go:build !linux
// +build !linux

package firecracker

import (
	"context"
	"fmt"

	"github.com/charliek/shed/internal/backend"
	"github.com/charliek/shed/internal/config"
)

const nonLinuxErr = "firecracker backend is only supported on linux"

// Client is a stub for non-linux builds.
type Client struct{}

// NewClient returns an error on non-linux platforms.
func NewClient(cfg *config.FirecrackerConfig, serverCfg *config.ServerConfig) (*Client, error) {
	return nil, fmt.Errorf(nonLinuxErr)
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
	return nil, fmt.Errorf(nonLinuxErr)
}

// GetShed returns an error on non-linux platforms.
func (b *FirecrackerBackend) GetShed(ctx context.Context, name string) (*config.Shed, error) {
	return nil, fmt.Errorf(nonLinuxErr)
}

// ListSheds returns an error on non-linux platforms.
func (b *FirecrackerBackend) ListSheds(ctx context.Context) ([]config.Shed, error) {
	return nil, fmt.Errorf(nonLinuxErr)
}

// DeleteShed returns an error on non-linux platforms.
func (b *FirecrackerBackend) DeleteShed(ctx context.Context, name string, keepVolume bool) error {
	return fmt.Errorf(nonLinuxErr)
}

// StartShed returns an error on non-linux platforms.
func (b *FirecrackerBackend) StartShed(ctx context.Context, name string) (*config.Shed, error) {
	return nil, fmt.Errorf(nonLinuxErr)
}

// StopShed returns an error on non-linux platforms.
func (b *FirecrackerBackend) StopShed(ctx context.Context, name string) (*config.Shed, error) {
	return nil, fmt.Errorf(nonLinuxErr)
}

// ListSessions returns an error on non-linux platforms.
func (b *FirecrackerBackend) ListSessions(ctx context.Context, shedName string) ([]config.Session, error) {
	return nil, fmt.Errorf(nonLinuxErr)
}

// KillSession returns an error on non-linux platforms.
func (b *FirecrackerBackend) KillSession(ctx context.Context, shedName, sessionName string) error {
	return fmt.Errorf(nonLinuxErr)
}

// Exec returns an error on non-linux platforms.
func (b *FirecrackerBackend) Exec(ctx context.Context, shedName string, opts backend.ExecOptions) error {
	return fmt.Errorf(nonLinuxErr)
}

// GetNetworkEndpoint returns an error on non-linux platforms.
func (b *FirecrackerBackend) GetNetworkEndpoint(ctx context.Context, shedName string) (string, error) {
	return "", fmt.Errorf(nonLinuxErr)
}
