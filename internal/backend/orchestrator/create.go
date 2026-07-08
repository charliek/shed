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
//
// Caller contract (what the per-backend CreateShed wrapper must do
// BEFORE calling orchestrator.CreateShed):
//
//   - Acquire any cross-name locks. VZ + FC use snapshot-lock(source)
//     then create-lock(new shed) — see the lock-order comments in
//     each backend's CreateShed. The orchestrator does not own these.
//   - Reject duplicate-name creates (LoadMetadata check). The
//     orchestrator's hooks assume the shed is new.
//   - Sweep any orphan upper from a previously-crashed create. Same
//     reason — hooks assume a clean slate.
//   - Validate request-only invariants the orchestrator cannot
//     express (cpus/memory bounds, upper-size bounds, --image vs
//     --from-snapshot exclusivity).
//
// These tasks intentionally stay in the per-backend wrapper because
// they need backend-private state (lock maps, instance-dir layout)
// the orchestrator does not own. PreFlight + the subsequent hooks
// take it from there.
package orchestrator

import (
	"context"

	"github.com/charliek/shed/internal/backend"
	"github.com/charliek/shed/internal/config"
)

// PreFlightResult is the orchestrator-opaque output of the backend's
// PreFlight step. Carries early-resolved values (image-source digest,
// snapshot-source path) so the rest of the lifecycle does not
// re-resolve them. Backends type-assert back to their concrete type
// inside subsequent hooks.
type PreFlightResult interface {
	// IsFromSnapshot reports whether the create is sourced from a
	// snapshot rather than a fresh image. The orchestrator uses
	// this to skip steps that snapshot-spawned sheds don't need
	// (e.g., re-running install hooks).
	IsFromSnapshot() bool
}

// UpperInfo is the orchestrator-opaque handle to a just-allocated
// writable upper layer. The orchestrator threads it from
// AllocateUpper through BuildAndPersistMetadata and StartVM as an
// `any`-shaped token; backends type-assert to their concrete value
// when they need the host-side path or size.
//
// (Empty interface by design — see the package doc-comment. The
// orchestrator does not introspect the value; the marker pattern was
// rejected because Go requires unexported markers to be implemented
// in the same package as the interface.)
type UpperInfo any

// NetworkResources is the orchestrator-opaque handle to the per-shed
// network resources acquired by the backend. Backends without
// per-shed network allocation (VZ uses Apple's vmnet shared NAT)
// return a zero-value value of any type that satisfies `any` and
// registers no cleanups. Same opacity rationale as `UpperInfo`.
type NetworkResources any

// MetadataHandle is the orchestrator-opaque handle to the persisted
// per-shed Metadata record. Each backend has its own Metadata type
// today (`vz.Metadata`, `firecracker.Metadata`); the interface keeps
// the orchestrator from depending on either struct's shape.
//
// Only `Name()` is exposed: it's used internally for diagnostic logs
// and to derive the vms-map key. Backends type-assert to their
// concrete type when they need other fields.
type MetadataHandle interface {
	Name() string
}

// VMHandle is the orchestrator-opaque handle to a started VM. The
// orchestrator threads it from StartVM through FinalizeStartedVM,
// MountLocalDir, SetupCredentials, CloneRepo, and RunProvisioning
// so backends can pass their concrete VM type around without the
// orchestrator depending on it. Same opacity rationale as
// `UpperInfo`.
type VMHandle any

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
//
// Hooks that emit user-visible status messages should use
// `backend.Phase(ctx, name)` + `backend.Status(ctx, message)` from
// `internal/backend` (split per PR #135). Hooks that DON'T have
// status text should still emit their own Phase boundary if they
// represent a distinct cost the operator should see in the
// PhaseTimer log.
type BackendCreator interface {
	// PreFlight runs early validation and resolves any
	// image-source or snapshot-source references, and registers any
	// protective cleanups that must hold for the rest of the
	// lifecycle (e.g., the `.creating` marker that protects the
	// lower-digest blob from a concurrent `shed image prune`).
	//
	// Errors here cause an immediate return from the orchestrator.
	// Any cleanups registered before the failure WILL still run via
	// the deferred `cleanup.Run()`, so implementations must register
	// each protective bit individually rather than as a single
	// "undo PreFlight" block.
	PreFlight(ctx context.Context, req config.CreateShedRequest, cleanup *backend.Cleanup) (PreFlightResult, error)

	// AllocateNetwork performs backend-specific per-shed network
	// resource allocation. FC: CID + IP + TAP. VZ: no-op returning
	// an empty NetworkResources. Implementations MUST Register a
	// cleanup for each acquired resource individually so a later
	// failure unwinds them in registration-reverse order.
	//
	// Called BEFORE AllocateUpper because per-shed network state is
	// cheap to fail (CID/IP exhaustion) and the upper layer is
	// expensive to undo (large sparse file). Matches FC's existing
	// CreateShed order; preserves measured timing during 2c.
	AllocateNetwork(ctx context.Context, req config.CreateShedRequest, cleanup *backend.Cleanup) (NetworkResources, error)

	// AllocateUpper provisions a writable upper layer for the new
	// shed (including any snapshot-clone or template-mint
	// substeps). Implementations MUST Register a "delete upper
	// layer" cleanup on `cleanup` so a downstream failure unwinds
	// it.
	AllocateUpper(ctx context.Context, req config.CreateShedRequest, pre PreFlightResult, cleanup *backend.Cleanup) (UpperInfo, error)

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

	// FinalizeStartedVM updates the persisted metadata with
	// post-Start state (Status=Running, PID, ...) and registers
	// the "remove from vms map" cleanup. The name is honest about
	// the two things it does — the second meta.Save plus the
	// vms-map registration — both of which must commit-or-rollback
	// as a unit.
	FinalizeStartedVM(ctx context.Context, meta MetadataHandle, vm VMHandle, cleanup *backend.Cleanup) error

	// MountLocalDir mounts the requested project directories
	// (--local-dir / --add-dir) under the guest's home directory via
	// the backend's transport (VirtioFS on VZ, 9P on FC). No-op
	// return nil when there are no project mounts.
	MountLocalDir(ctx context.Context, req config.CreateShedRequest, meta MetadataHandle, vm VMHandle) error

	// SetupCredentials mounts any configured credentials into the
	// guest. Best-effort: failures are logged inside the hook but
	// do not return an error (matching today's behavior in
	// `vmutil.CredentialManager.SetupCredentials`).
	SetupCredentials(ctx context.Context, req config.CreateShedRequest, meta MetadataHandle, vm VMHandle)

	// ConfigureEgressProxy opens (or reuses) this shed's egress-filtering
	// proxy listener and injects the proxy environment into the persistent
	// upper, when egress control is enabled in the server config. Unlike
	// the best-effort credential/clone/provision hooks this is FAILABLE: a
	// configuration failure aborts the create and unwinds (the hook
	// registers its listener teardown on `cleanup`, so a failure inside the
	// hook tears the listener back down). A no-op returning nil when egress
	// is disabled. Runs AFTER SetupCredentials and BEFORE CloneRepo so the
	// clone is itself routed through (and audited by) the proxy.
	ConfigureEgressProxy(ctx context.Context, req config.CreateShedRequest, meta MetadataHandle, vm VMHandle, cleanup *backend.Cleanup) error

	// CloneRepo clones `req.Repo` into the guest's workspace if
	// specified. Best-effort: hook implementations log + emit
	// `backend.StatusWarning` on failure and return nothing — the
	// orchestrator continues to the next step regardless. Matches
	// today's behavior in both backends.
	CloneRepo(ctx context.Context, req config.CreateShedRequest, meta MetadataHandle, vm VMHandle)

	// RunProvisioning runs the shed's provisioning hooks (loaded
	// from .shed/provision.yaml inside the guest workspace) unless
	// the request opted out via NoProvision. Best-effort — same
	// contract as SetupCredentials.
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
// validation beyond what a backend's PreFlight does — call sites (the
// backend's existing CreateShed wrapper) keep that responsibility
// per the package doc-comment contract.
func CreateShed(ctx context.Context, b BackendCreator, req config.CreateShedRequest) (*config.Shed, error) {
	cleanup := backend.NewCleanup()
	defer cleanup.Run()

	pre, err := b.PreFlight(ctx, req, cleanup)
	if err != nil {
		return nil, err
	}

	// Network allocation runs first (fail-cheap; matches FC's
	// existing order). VZ's hook is a no-op and registers nothing.
	net, err := b.AllocateNetwork(ctx, req, cleanup)
	if err != nil {
		return nil, err
	}

	backend.Phase(ctx, "rootfs")
	backend.Status(ctx, "Allocating writable upper layer...")
	upper, err := b.AllocateUpper(ctx, req, pre, cleanup)
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

	if err := b.FinalizeStartedVM(ctx, meta, vm, cleanup); err != nil {
		return nil, err
	}

	if err := b.MountLocalDir(ctx, req, meta, vm); err != nil {
		return nil, err
	}

	b.SetupCredentials(ctx, req, meta, vm)
	if err := b.ConfigureEgressProxy(ctx, req, meta, vm, cleanup); err != nil {
		return nil, err
	}
	b.CloneRepo(ctx, req, meta, vm)
	b.RunProvisioning(ctx, req, meta, vm)

	cleanup.Commit()
	return b.ToShedResult(meta), nil
}
