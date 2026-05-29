// Package orchestrator hosts the backend-agnostic CreateShed lifecycle
// that both VZ and Firecracker implement. Per §15 Phase 2 of the
// runtime-opt doc, the goal is to express the create / start / spawn
// lifecycle once, with each backend providing a small set of platform-
// specific hooks via the BackendCreator interface.
//
// Today (PR 2b — scaffolding) this package is interface-only: VZ and
// Firecracker continue to use their existing CreateShed
// implementations in `internal/vz/client.go` and
// `internal/firecracker/client.go`. PR 2c migrates VZ to call the
// orchestrator; 2d migrates FC and removes the duplicated lifecycle
// code from both backends.
//
// Why an interface (and not a base struct or shared closures)?
//
//   - VZ and FC have genuinely divergent platform code at each step
//     (vfkit vs firecracker process management; VirtioFS vs 9P
//     workspace transport; in-blob kernel vs separate kernel file;
//     etc.). A shared CreateShed body that branches on backend at every
//     decision is harder to read than two backends implementing a
//     small contract.
//   - The interface forces every cross-backend assumption to be
//     spelled out as a method signature. When 2c migrates VZ, any
//     leftover shape mismatch between "what the interface promises"
//     and "what VZ actually does" surfaces as a compile error, not a
//     runtime surprise.
//   - The cleanup-stack from PR #137 (`internal/backend.Cleanup`) is
//     the shared mechanism for rollback. Each BackendCreator method
//     accepts the in-progress Cleanup and Registers its own teardown,
//     so the orchestrator never needs to know what resources a
//     backend acquires.
package orchestrator

import (
	"context"

	"github.com/charliek/shed/internal/backend"
	"github.com/charliek/shed/internal/config"
)

// PreFlightResult is the orchestrator-opaque output of the
// backend's PreFlight step. Carries early-resolved values
// (image-source digest, snapshot-source path) so the rest of
// the lifecycle does not re-resolve them.
type PreFlightResult interface {
	// IsFromSnapshot reports whether the create is sourced from a
	// snapshot rather than a fresh image. The orchestrator uses
	// this to skip steps that snapshot-spawned sheds don't need
	// (e.g., re-running install hooks).
	IsFromSnapshot() bool
}

// UpperInfo is the orchestrator-opaque handle to a backend's
// just-allocated writable upper layer.
type UpperInfo interface {
	// Path returns the upper layer's filesystem path on the host
	// (used for diagnostics; the backend internally tracks more).
	Path() string

	// SizeBytes returns the upper layer's logical size, which may
	// differ from the freshly-allocated size when the upper was
	// cloned from a snapshot.
	SizeBytes() int64
}

// NetworkResources is the orchestrator-opaque handle to the per-shed
// network resources acquired by the backend. For backends without
// per-shed network allocation (VZ uses Apple's vmnet shared NAT),
// implementations return a zero-value NetworkResources that satisfies
// the interface and registers no cleanups.
type NetworkResources interface {
	// Summary returns a short human-readable description of the
	// allocated resources, used in diagnostic logs. Implementations
	// may return "" if no per-shed network state was allocated.
	Summary() string
}

// MetadataHandle is the orchestrator-opaque handle to the persisted
// per-shed Metadata record. Each backend has its own Metadata type
// today (`vz.Metadata`, `firecracker.Metadata`); the interface keeps
// the orchestrator from depending on either struct's shape.
type MetadataHandle interface {
	// Name returns the shed name (used as a key in logs and in the
	// vms map). Required to match the request's Name.
	Name() string
}

// VMHandle is the orchestrator-opaque handle to a started VM. The
// orchestrator doesn't operate on it directly (the backend's hooks
// own VM-side work); it threads VMHandle from StartVM to FinalizeVM
// and CommitVM so backends can pass their concrete VM type around.
type VMHandle interface {
	// Backend returns the backend identifier for the started VM
	// (used for logging and to assert no cross-backend mix-ups).
	Backend() string
}

// BackendCreator is the per-backend hook contract used by
// orchestrator.CreateShed. Methods are listed in the order
// CreateShed calls them.
//
// Every method that may register a cleanup is passed the in-progress
// `*backend.Cleanup`. Implementations Register their own teardown
// closures on success; the orchestrator's deferred `cleanup.Run()`
// invokes them in LIFO order on any error-return. Implementations
// MUST NOT call `cleanup.Run()` or `cleanup.Commit()` themselves —
// those are the orchestrator's responsibility.
type BackendCreator interface {
	// Name returns the backend identifier (used as a PhaseTimer
	// label and in diagnostic logs).
	Name() string

	// PreFlight runs early validation and resolves any
	// image-source or snapshot-source references. Errors here cause
	// an immediate return from the orchestrator — no cleanups have
	// been registered yet.
	PreFlight(ctx context.Context, req config.CreateShedRequest) (PreFlightResult, error)

	// AllocateUpper provisions a writable upper layer for the new
	// shed. Implementations MUST Register a "delete upper layer"
	// cleanup on `cleanup` so a downstream failure unwinds it.
	AllocateUpper(ctx context.Context, req config.CreateShedRequest, pre PreFlightResult, cleanup *backend.Cleanup) (UpperInfo, error)

	// AllocateNetwork performs backend-specific per-shed network
	// resource allocation (FC: CID + IP + TAP; VZ: a no-op
	// returning empty NetworkResources). Implementations MUST
	// Register a cleanup for each acquired resource individually.
	AllocateNetwork(ctx context.Context, req config.CreateShedRequest, cleanup *backend.Cleanup) (NetworkResources, error)

	// BuildAndPersistMetadata constructs the platform-specific
	// metadata record and persists it to disk. Implementations MUST
	// Register the "delete instance dir" cleanup BEFORE writing —
	// `meta.Save` typically creates the directory before writing,
	// so a partial-write failure must still unwind. See PR #137
	// for the regression-fix history on this point.
	BuildAndPersistMetadata(ctx context.Context, req config.CreateShedRequest, pre PreFlightResult, upper UpperInfo, net NetworkResources, cleanup *backend.Cleanup) (MetadataHandle, error)

	// StartVM constructs and starts the per-shed VM. Implementations
	// MUST Register a "stop VM" cleanup on cleanup so a downstream
	// failure unwinds it. The orchestrator emits the "vm" PhaseTimer
	// boundary before calling.
	StartVM(ctx context.Context, meta MetadataHandle, upper UpperInfo, net NetworkResources, cleanup *backend.Cleanup) (VMHandle, error)

	// FinalizeVM updates the persisted metadata with post-Start
	// state (Status=Running, PID, ...) and registers the
	// "remove from vms map" cleanup. Splitting this from StartVM
	// keeps the StartVM contract focused on actually booting the
	// guest.
	FinalizeVM(ctx context.Context, meta MetadataHandle, vm VMHandle, cleanup *backend.Cleanup) error

	// MountWorkspace mounts the requested local directory (if any)
	// into the guest via the backend's transport (VirtioFS on VZ,
	// 9P on FC). A nil-error return is required when no local-dir
	// is configured.
	MountWorkspace(ctx context.Context, req config.CreateShedRequest, meta MetadataHandle, vm VMHandle) error

	// SetupCredentials mounts any configured credentials into the
	// guest. Best-effort: failures are logged but do not return an
	// error (matching today's behavior in
	// `vmutil.CredentialManager.SetupCredentials`).
	SetupCredentials(ctx context.Context, req config.CreateShedRequest, meta MetadataHandle, vm VMHandle)

	// CloneRepo clones the request's --repo into the guest's
	// workspace if specified. Best-effort: failures emit a
	// StatusWarning via the ctx-bound progress and return nil so
	// the create completes (matching today's behavior).
	CloneRepo(ctx context.Context, req config.CreateShedRequest, meta MetadataHandle, vm VMHandle) error

	// RunProvisioning runs the shed's provisioning hooks (loaded
	// from .shed/provision.yaml inside the guest workspace) unless
	// the request opted out via NoProvision. Best-effort.
	RunProvisioning(ctx context.Context, req config.CreateShedRequest, meta MetadataHandle, vm VMHandle)

	// ToShedResult returns the *config.Shed value to hand back to
	// the orchestrator's caller. Called once the create is fully
	// committed.
	ToShedResult(meta MetadataHandle) *config.Shed
}

// CreateShed runs the backend-agnostic shed-creation lifecycle. Each
// backend supplies its platform-specific behavior via `b`; the
// orchestrator owns the call ordering, the PhaseTimer boundaries, the
// cleanup-stack scaffolding, and the success/failure return contract.
//
// On any error from a hook, the deferred cleanup.Run unwinds every
// cleanup the hook chain has registered so far, in LIFO order. On
// success, cleanup.Commit zeroes the stack so the defer is a no-op.
//
// CreateShed does not itself acquire per-shed locks or do request
// validation beyond what a backend's PreFlight does — call sites
// (the backend's existing CreateShed wrapper) keep that
// responsibility.
func CreateShed(ctx context.Context, b BackendCreator, req config.CreateShedRequest) (*config.Shed, error) {
	cleanup := backend.NewCleanup()
	defer cleanup.Run()

	pre, err := b.PreFlight(ctx, req)
	if err != nil {
		return nil, err
	}

	backend.Phase(ctx, "rootfs")
	backend.Status(ctx, "Allocating writable upper layer...")
	upper, err := b.AllocateUpper(ctx, req, pre, cleanup)
	if err != nil {
		return nil, err
	}

	// Phase boundary for "network" is emitted only by backends that
	// actually allocate network resources; VZ's AllocateNetwork is a
	// no-op so emitting "network" there would show a 0-ms entry. The
	// hook is responsible for its own Phase/Status events.
	net, err := b.AllocateNetwork(ctx, req, cleanup)
	if err != nil {
		return nil, err
	}

	meta, err := b.BuildAndPersistMetadata(ctx, req, pre, upper, net, cleanup)
	if err != nil {
		return nil, err
	}

	backend.Phase(ctx, "vm")
	backend.Status(ctx, "Starting virtual machine...")
	vm, err := b.StartVM(ctx, meta, upper, net, cleanup)
	if err != nil {
		return nil, err
	}

	if err := b.FinalizeVM(ctx, meta, vm, cleanup); err != nil {
		return nil, err
	}

	if err := b.MountWorkspace(ctx, req, meta, vm); err != nil {
		return nil, err
	}

	b.SetupCredentials(ctx, req, meta, vm)

	if err := b.CloneRepo(ctx, req, meta, vm); err != nil {
		return nil, err
	}

	b.RunProvisioning(ctx, req, meta, vm)

	cleanup.Commit()
	return b.ToShedResult(meta), nil
}
