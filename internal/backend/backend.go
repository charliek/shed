// Package backend provides the abstraction layer for different execution backends.
package backend

import (
	"context"

	"github.com/charliek/shed/internal/config"
)

// Type identifies the backend implementation.
type Type string

const (
	// TypeDocker is the Docker-based backend.
	TypeDocker Type = "docker"
	// TypeFirecracker is the Firecracker-based backend.
	TypeFirecracker Type = "firecracker"
	// TypeVZ is the Apple Virtualization.framework-based backend (macOS only).
	TypeVZ Type = "vz"
)

// Backend defines the interface for shed execution backends.
// Different backends (Docker, Firecracker, etc.) implement this interface
// to provide shed lifecycle management, session handling, and command execution.
type Backend interface {
	// Type returns the backend type identifier.
	Type() Type

	// Close releases any resources held by the backend.
	Close() error

	// Shed lifecycle operations

	// CreateShed creates a new shed with the given configuration.
	CreateShed(ctx context.Context, req config.CreateShedRequest) (*config.Shed, error)

	// GetShed returns a shed by name, or an error if not found.
	GetShed(ctx context.Context, name string) (*config.Shed, error)

	// ListSheds returns all sheds managed by this backend.
	ListSheds(ctx context.Context) ([]config.Shed, error)

	// DeleteShed removes a shed. If keepVolume is true, the workspace is preserved.
	DeleteShed(ctx context.Context, name string, keepVolume bool) error

	// StartShed starts a stopped shed.
	StartShed(ctx context.Context, name string) (*config.Shed, error)

	// StopShed stops a running shed.
	StopShed(ctx context.Context, name string) (*config.Shed, error)

	// Session operations

	// ListSessions returns all sessions in a shed.
	ListSessions(ctx context.Context, shedName string) ([]config.Session, error)

	// KillSession terminates a session in a shed.
	KillSession(ctx context.Context, shedName, sessionName string) error

	// Execution

	// Exec executes a command in a shed with the given options.
	Exec(ctx context.Context, shedName string, opts ExecOptions) error

	// Network

	// GetNetworkEndpoint returns the network endpoint (IP or hostname) for a shed.
	// This is used for port forwarding and other network operations.
	GetNetworkEndpoint(ctx context.Context, shedName string) (string, error)
}
