// Package backend provides the abstraction layer for different execution backends.
package backend

import (
	"context"
	"net"

	"github.com/charliek/shed/internal/config"
)

// Type identifies the backend implementation.
type Type string

const (
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

	// ResetShed deletes the per-shed writable upper layer and recreates
	// it as a fresh empty sparse file. The shed must be stopped. The
	// workspace (mounted post-boot via 9P/VirtioFS from outside the
	// overlay) is not touched. Returns the updated shed metadata.
	ResetShed(ctx context.Context, name string) (*config.Shed, error)

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
	// This is used for API responses and informational purposes.
	GetNetworkEndpoint(ctx context.Context, shedName string) (string, error)

	// DialService opens a TCP connection to a port inside a running shed's VM.
	// For VZ: dials via vsock TCP proxy (port 1028) with CONNECT handshake.
	// For Firecracker: dials the VM's bridge IP directly over TCP.
	// The returned net.Conn is a raw TCP stream.
	DialService(ctx context.Context, shedName string, port uint16) (net.Conn, error)

	// Images

	// ListImages returns available image variants for this backend.
	// Returns an empty list for backends that don't support image variants.
	ListImages(ctx context.Context) ([]config.ImageInfo, error)

	// InspectImage returns full details (info + manifest) for a tag or
	// digest. Returns ErrImageNotFoundSentinel if neither resolves.
	InspectImage(ctx context.Context, tagOrDigest string) (config.ImageInspectResponse, error)

	// TagImage points newTag at the digest currently held by srcTagOrDigest.
	// Equivalent to `docker tag`.
	TagImage(ctx context.Context, srcTagOrDigest, newTag string) error

	// PullImage pulls a Docker reference, converts it to ext4, installs
	// into the blob store, and advances the named tag. Returns the digest.
	PullImage(ctx context.Context, dockerRef, tag string) (string, error)

	// DeleteImage removes a tag (Docker model). The underlying blob is
	// GC'd by PruneImages once nothing references it. Returns
	// ErrImageNotFoundSentinel if the tag doesn't exist, or
	// ErrImageInUseSentinel if the tag is referenced by config.
	DeleteImage(ctx context.Context, name string) error

	// PruneImages removes blobs that have no protective shed/snapshot
	// references. If dryRun is true, returns candidates without deleting.
	PruneImages(ctx context.Context, dryRun bool) ([]config.ImageInfo, error)

	// System

	// DiskUsage returns disk-usage information for everything this backend
	// manages on the local server: image cache, per-instance rootfs copies,
	// console logs, kernel/initrd, and orphan sidecar files. Client-side
	// state under ~/.shed/ is out of scope.
	DiskUsage(ctx context.Context) (config.DiskUsage, error)

	// Prune removes items selected by opts. DryRun returns the report
	// without mutating. Age-based instance pruning uses mtime(metadata.json)
	// as the "last touched" proxy — see opts.Until.
	Prune(ctx context.Context, opts PruneOptions) (config.PruneReport, error)

	// Snapshots

	// CreateSnapshot captures a stopped shed's rootfs as a named, immutable artifact.
	// Returns ErrSnapshotSourceRunningSentinel if the source is running,
	// ErrSnapshotAlreadyExistsSentinel if the name is taken, or ErrShedNotFoundSentinel
	// if the source shed does not exist. Backends emit progress via backend.Progress.
	CreateSnapshot(ctx context.Context, req config.SnapshotCreateRequest) (*config.Snapshot, error)

	// ListSnapshots returns all snapshots managed by this backend.
	ListSnapshots(ctx context.Context) ([]config.Snapshot, error)

	// GetSnapshot returns a snapshot by name.
	// Returns ErrSnapshotNotFoundSentinel if the snapshot does not exist.
	GetSnapshot(ctx context.Context, name string) (*config.Snapshot, error)

	// DeleteSnapshot removes a snapshot. Spawned sheds remain independent
	// (each holds its own rootfs copy at create-from-snapshot time).
	// Returns ErrSnapshotNotFoundSentinel if the snapshot does not exist.
	DeleteSnapshot(ctx context.Context, name string) error
}
