// This file extends the orchestrator with the backend-agnostic
// StartShed lifecycle that mirrors CreateShed's late half (StartVM →
// FinalizeStartedVM → MountLocalDir → SetupCredentials →
// RunProvisioning) without the early-half steps (image pull, network
// alloc, rootfs alloc, metadata create) which a stopped shed already
// has on disk.
//
// Why a separate sub-interface (BackendStarter) and not a method
// suffix on BackendCreator?
//
//   - CreateShed and StartShed take genuinely different inputs:
//     CreateShed gets a CreateShedRequest, StartShed gets a name +
//     loads metadata. Squeezing both into one interface forces every
//     hook to know which mode it's running in.
//   - The StartShed-specific hooks (LoadMetadata, CheckNotRunning,
//     PersistRunningState, RunStartupHook) have no natural place in
//     CreateShed's flow.
//   - The shared hooks (MountLocalDir, SetupCredentials,
//     ToShedResult) reuse the same closed-over Client state via the
//     backend's per-create wrapper type implementing both interfaces.
//     Each backend's *Starter type wraps the same *Client as the
//     *Creator and delegates to the shared helpers underneath.
//
// Per PR-B1 the migration is VZ-only; PR-B2 mirrors for Firecracker
// and removes the inline StartShed code from both backend clients.

package orchestrator

import (
	"context"

	"github.com/charliek/shed/internal/backend"
	"github.com/charliek/shed/internal/config"
)

// StartRequest is the orchestrator-side input for StartShed. Kept as
// its own type rather than reusing CreateShedRequest because the
// stop→start hot path takes only a name and the rest of the request
// shape is loaded from persisted metadata. Carrying a half-populated
// CreateShedRequest would invite "did the caller forget to set X?"
// bugs in the hooks.
type StartRequest struct {
	Name string
}

// BackendStarter is the per-backend hook contract used by
// orchestrator.StartShed. Methods are listed in the order StartShed
// calls them.
//
// Same cleanup-stack contract as BackendCreator: hooks that acquire
// rollback-able state Register on the passed *backend.Cleanup; the
// orchestrator runs cleanup on failure and commits on success.
type BackendStarter interface {
	// LoadMetadata reads the persisted per-shed metadata from disk
	// and returns it as an orchestrator-opaque handle. Returning the
	// canonical ErrShedNotFoundSentinel from internal/config is the
	// API-level contract for "no such shed."
	LoadMetadata(ctx context.Context, req StartRequest) (MetadataHandle, error)

	// CheckNotRunning verifies the shed isn't already up. Two error
	// paths matter:
	//
	//   - ErrShedAlreadyRunningSentinel: status=Running AND the
	//     recorded PID is genuinely a live VMM. Caller's StartShed
	//     was a no-op against a running shed.
	//   - ErrZombiePresentSentinel: status=Stopped (or stale Running)
	//     but the recorded PID is alive + matches the VMM binary.
	//     We refuse to spawn a second VMM under the same name.
	//
	// If neither fires (PID dead, or status=Stopped with PID=0),
	// returns nil. Implementations are expected to clear stale
	// status/PID fields on the in-memory metadata before returning
	// — those changes ride along the rest of the lifecycle and are
	// persisted by PersistRunningState.
	CheckNotRunning(ctx context.Context, meta MetadataHandle) error

	// StartVM constructs and starts the per-shed VM from the existing
	// metadata. Implementations MUST Register a "stop VM" cleanup on
	// cleanup so a downstream failure unwinds it. The orchestrator
	// emits the "vm" PhaseTimer boundary before calling.
	StartVM(ctx context.Context, meta MetadataHandle, cleanup *backend.Cleanup) (VMHandle, error)

	// PersistRunningState updates the persisted metadata with
	// post-Start state (Status=Running, PID, ...) and registers the
	// "remove from vms map" cleanup so a downstream failure unwinds
	// it. Mirrors BackendCreator.FinalizeStartedVM.
	PersistRunningState(ctx context.Context, meta MetadataHandle, vm VMHandle, cleanup *backend.Cleanup) error

	// RestoreStoppedMetadata rewrites the persisted metadata back to
	// status=Stopped, PID=0. The orchestrator wires it into the
	// cleanup stack BEFORE StartVM (so LIFO runs it LAST, after the
	// vms-map and "stop VM" cleanups have terminated the VMM) and
	// gates the call on PersistRunningState having succeeded — there
	// is nothing to restore otherwise.
	//
	// Implementations should refuse to clear the PID when the recorded
	// process is still alive after the LIFO unwind (i.e. when "stop VM"
	// failed to actually kill it). Same defensive shape as StopShed's
	// pre-flip IsProcessAlive guard: returning an error from this hook
	// leaves the lying-"Running" state on disk for the GetShed
	// lazy-staleness check to repair, which is safer than the lying-
	// "Stopped" state that a force-clear would produce. Errors logged
	// but not propagated.
	RestoreStoppedMetadata(meta MetadataHandle) error

	// MountLocalDir re-mounts the configured `--local-dir` via the
	// backend's transport (VirtioFS on VZ, 9P on FC). Mount state
	// does not persist across VMM restarts, so this runs on every
	// start even when CreateShed already mounted at create time.
	// No-op when the metadata has no local-dir.
	MountLocalDir(ctx context.Context, meta MetadataHandle, vm VMHandle) error

	// SetupCredentials re-mounts any configured credentials into the
	// guest. Best-effort (matches CreateShed's contract).
	SetupCredentials(ctx context.Context, meta MetadataHandle, vm VMHandle)

	// RunStartupHook runs only the `startup` provisioning hook —
	// NOT `install`, which is create-time only. Loads the per-shed
	// provision.yaml inside the guest workspace. Best-effort.
	RunStartupHook(ctx context.Context, meta MetadataHandle, vm VMHandle)

	// ToShedResult returns the *config.Shed value to hand back to
	// the orchestrator's caller. Called once the start is fully
	// committed.
	ToShedResult(meta MetadataHandle) *config.Shed
}

// StartShed runs the backend-agnostic shed-start lifecycle. Each
// backend supplies its platform-specific behavior via `b`.
//
// Caller contract (what the per-backend StartShed wrapper must do
// BEFORE calling orchestrator.StartShed):
//
//   - Acquire the per-shed create-lock (same lock that serializes
//     CreateShed/StopShed/DeleteShed for this name). The
//     orchestrator does not own it.
//
// On any error from a hook, the deferred cleanup.Run unwinds every
// cleanup the hook chain has registered so far, in LIFO order. On
// success, cleanup.Commit zeroes the stack so the defer is a no-op.
func StartShed(ctx context.Context, b BackendStarter, req StartRequest) (*config.Shed, error) {
	cleanup := backend.NewCleanup()
	defer cleanup.Run()

	meta, err := b.LoadMetadata(ctx, req)
	if err != nil {
		return nil, err
	}

	if err := b.CheckNotRunning(ctx, meta); err != nil {
		return nil, err
	}

	// Register the metadata-restore cleanup BEFORE StartVM so that LIFO
	// ordering puts it LAST on unwind — after "stop VM" has terminated
	// the VMM. Restoring disk to Stopped/PID=0 only after the process
	// is confirmed dead matches StopShed's verify-before-clear pattern
	// (see e.g. vz/client.go's `IsProcessAlive` guard) and avoids the
	// CodeRabbit-flagged race where a silent stop-VM failure could leave
	// disk=Stopped/PID=0 + a still-alive VMM, opening the door to a
	// double-spawn on the next start.
	//
	// The closure is gated on `persistedRunning` so that a failure
	// BEFORE PersistRunningState succeeds is a no-op — at that point
	// the metadata never reached Running on disk, so there's nothing
	// to restore, and clobbering whatever CheckNotRunning left would
	// be wrong.
	persistedRunning := false
	cleanup.Register("restore stopped metadata", func() error {
		if !persistedRunning {
			return nil
		}
		return b.RestoreStoppedMetadata(meta)
	})

	backend.Phase(ctx, "vm")
	backend.Status(ctx, "Starting virtual machine...")
	vm, err := b.StartVM(ctx, meta, cleanup)
	if err != nil {
		return nil, err
	}

	if err := b.PersistRunningState(ctx, meta, vm, cleanup); err != nil {
		return nil, err
	}
	persistedRunning = true

	if err := b.MountLocalDir(ctx, meta, vm); err != nil {
		return nil, err
	}

	b.SetupCredentials(ctx, meta, vm)
	b.RunStartupHook(ctx, meta, vm)

	cleanup.Commit()
	return b.ToShedResult(meta), nil
}
