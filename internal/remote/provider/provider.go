// Package provider is the seam between the remote backend and a cloud that
// runs one VM per shed. The backend (internal/remote) holds a Provider and
// never talks to a cloud API directly; a provider never touches shed records,
// the agent or SSH.
//
// # Accepted, then wait
//
// Create, Start, Stop and Destroy return as soon as the cloud has accepted
// the request, which takes seconds. Each returns a Pending. Wait blocks until
// that work has finished, which takes tens of seconds. An HTTP handler calls
// the first and answers; a background job calls Wait on a context the server
// owns.
//
// # Who polls
//
// The provider polls, and only inside Wait. A caller calls Wait once per
// Pending and does not loop over Get to follow an operation. The one other
// periodic read is the backend's reconcile pass, which calls List on its own
// timer.
//
// # Refusals
//
// A cloud may refuse for quota or capacity either at once (the accepting call
// returns the error) or later (Wait returns it). Callers handle both the same
// way; see ErrQuota and ErrCapacity.
//
// The package imports only internal/remote/guest, so internal/config may
// import it.
package provider

import (
	"context"
	"time"
)

// Provider names, the values of remote.provider in the server config.
const (
	NameGCE  = "gce"
	NameFake = "fake"
)

// FastFailWindow is how long the create handler waits on a new instance's
// Pending before it answers. A refusal that arrives inside the window is
// returned on the create reply and leaves no record; after it the handler
// answers 201 "starting" and a job keeps waiting.
const FastFailWindow = 15 * time.Second

// Ref identifies one instance to the provider that issued it. It is opaque:
// callers store it and pass it back, and never build or parse one. A Ref
// stays valid across stop and start and across server restarts. It comes from
// Create (in the Pending) or from List.
type Ref string

// State is where an instance is in its life, as the cloud reports it. It is
// not a shed status: the backend derives the status from the State, the
// shed_state label and an agent probe.
type State string

const (
	// StateStarting: the instance exists and is on its way to running.
	StateStarting State = "starting"
	// StateRunning: the cloud reports the VM running. The guest may still
	// be booting.
	StateRunning State = "running"
	// StateStopping: the instance is on its way to stopped, or to being
	// deleted.
	StateStopping State = "stopping"
	// StateStopped: the VM is off and its disk is kept.
	StateStopped State = "stopped"
	// StateUnknown: the cloud reported a state this provider does not map.
	// Callers treat it as not settled.
	StateUnknown State = "unknown"
	// StateAbsent is never reported by Get or List. It is only a wait
	// target: Pending.Want for a Destroy.
	StateAbsent State = "absent"
)

// Settled reports whether the state is one an instance rests in. Starting,
// stopping and unknown are not settled.
func (s State) Settled() bool {
	return s == StateRunning || s == StateStopped || s == StateAbsent
}

// Pending describes work the cloud has accepted and not yet finished. It is
// plain data: the backend may store it in a shed record and hand it to Wait
// after a restart, from another process.
type Pending struct {
	// Ref is the instance the work is on. It is set whenever the accepting
	// call returned no error, including by Create.
	Ref Ref `json:"ref"`
	// Operation is the provider's token for the cloud operation. It is
	// empty when there is nothing to follow (the call was a no-op) or the
	// token was lost. Wait then follows the instance's state instead, and
	// cannot report why an operation failed.
	Operation string `json:"operation,omitempty"`
	// Want is the state that means the work is done: StateRunning for
	// Create and Start, StateStopped for Stop, StateAbsent for Destroy.
	// Empty means "whatever settled state it reaches"; see Settle.
	Want State `json:"want,omitempty"`
}

// Settle returns a Pending that makes Wait block until the instance has no
// transition in progress, whichever state it ends in. Use it after ErrBusy,
// and after a restart when no Pending was stored.
func Settle(ref Ref) Pending {
	return Pending{Ref: ref}
}

// Capabilities says what a provider does by itself.
type Capabilities struct {
	// EnforcesRunLimit: the cloud stops the VM when Spec.MaxRun has passed
	// since its last start, even when the server is down. When false the
	// server must run its own timer.
	EnforcesRunLimit bool
	// StableAddress: Instance.Address is the same after a stop and start.
	// When false, callers ask again after every start.
	StableAddress bool
}

// StartOptions are the settings a Start may change.
type StartOptions struct {
	// MaxRun, when positive, is the run limit for this session. The
	// provider applies it before starting if it differs from the
	// instance's current limit. Zero leaves the limit as it is.
	MaxRun time.Duration
}

// Selector picks the instances List returns.
type Selector struct {
	// Env, when set, selects instances whose shed_env label equals it.
	// Empty selects every instance in the provider's scope (its project
	// and zone), labelled or not; reconcile uses that only to report.
	Env string
}

// Provider creates and controls instances. Every method is safe to call from
// many goroutines. Every method honours ctx: when ctx ends it returns an
// error that wraps ctx.Err(), and the cloud-side work, if already accepted,
// carries on.
//
// A provider is built for one environment. Start, Stop, Destroy, SetLabels
// and SetMetadata refuse an instance whose shed_env label names another
// environment with ErrForeign, checked against the cloud's current labels,
// not a cached copy.
type Provider interface {
	// Name returns the provider's name: NameGCE or NameFake.
	Name() string

	// Capabilities returns what the provider does by itself. It is constant
	// for the life of the provider.
	Capabilities() Capabilities

	// Create asks the cloud for a new instance and returns once the request
	// is accepted. It does not wait for the instance to run; pass the
	// Pending to Wait for that.
	//
	// Idempotent on spec.Name: if an instance with that name exists and its
	// shed_id label equals the spec's, Create changes nothing and returns a
	// Pending for it with a nil error, so a resumed job may call Create
	// again. If the name is taken by an instance with another shed_id, the
	// error is ErrExists.
	//
	// ErrQuota and ErrCapacity may be returned here or by Wait. ErrInvalid
	// means the spec is malformed and nothing was sent. When Create returns
	// any other error the outcome is not known: call Create again with the
	// same spec, or find the instance with List.
	Create(ctx context.Context, spec Spec) (Pending, error)

	// Start asks the cloud to start a stopped instance and returns once the
	// request is accepted.
	//
	// Idempotent: on an instance that is running, or already starting, it
	// starts nothing and returns a Pending that Wait resolves when the
	// instance runs. On an instance that is stopping it returns ErrBusy.
	// ErrNotFound means no such instance. ErrQuota and ErrCapacity may be
	// returned here or by Wait; after either the instance is left stopped.
	Start(ctx context.Context, ref Ref, opts StartOptions) (Pending, error)

	// Stop asks the cloud to stop a running instance, keeping its disk, and
	// returns once the request is accepted.
	//
	// Idempotent: on an instance that is stopped, or already stopping, it
	// stops nothing and returns a Pending that Wait resolves when the
	// instance is stopped. On an instance that is starting it returns
	// ErrBusy. ErrNotFound means no such instance.
	Stop(ctx context.Context, ref Ref) (Pending, error)

	// Destroy asks the cloud to delete the instance and its disk and
	// returns once the request is accepted.
	//
	// Not found is success: if the instance does not exist, Destroy returns
	// a Pending and a nil error, and Wait on it returns at once. Destroy
	// never returns ErrNotFound. Calling it again while the delete is in
	// progress is also success. While another transition is in progress
	// (the instance is starting, or stopping for a Stop) it may return
	// ErrBusy.
	Destroy(ctx context.Context, ref Ref) (Pending, error)

	// Wait blocks until the work p describes has finished or ctx ends. The
	// provider does the polling.
	//
	// It returns a nil error when the instance has reached p.Want, with a
	// fresh view of the instance (the zero Instance for StateAbsent).
	//
	// When ctx ends first it returns an error wrapping ctx.Err(). Nothing
	// is cancelled in the cloud, and Wait may be called again with the same
	// Pending, any number of times, from any process.
	//
	// When the operation failed it returns the reason: ErrQuota,
	// ErrCapacity, or another error. A failed create leaves no instance; a
	// failed start leaves it stopped.
	//
	// When the instance disappears while p.Want is not StateAbsent it
	// returns ErrNotFound. When the instance settles in a state other than
	// p.Want (it was stopped from outside while starting, for example) it
	// returns ErrUnexpectedState together with the instance as it is.
	//
	// With an empty p.Operation, or one the cloud no longer knows, Wait
	// follows the instance's state alone. With an empty p.Want it returns
	// as soon as the instance is settled: nil with the instance, or
	// ErrNotFound if it is gone.
	Wait(ctx context.Context, p Pending) (Instance, error)

	// Get returns the instance as the cloud reports it now. ErrNotFound
	// means it does not exist. An instance being deleted is reported, in
	// StateStopping, until the cloud drops it.
	Get(ctx context.Context, ref Ref) (Instance, error)

	// List returns every instance the selector matches, in any state,
	// ordered by Name. It returns the complete set or an error, never a
	// partial list: callers treat a missing instance as deleted.
	List(ctx context.Context, sel Selector) ([]Instance, error)

	// SetLabels sets the given labels on the instance and leaves its other
	// labels alone. An empty value sets the label to the empty string; no
	// label is ever removed. LabelSchema, LabelEnv and LabelID cannot be
	// changed (ErrInvalid). Setting a label to the value it already has is
	// success. It completes before it returns. Disk labels are not
	// changed.
	SetLabels(ctx context.Context, ref Ref, labels map[string]string) error

	// SetMetadata sets the given instance metadata keys and leaves the
	// others alone. An empty value removes the key. It completes before it
	// returns; a following Get shows the new values. Metadata is readable
	// by every process in the guest, so it never holds a secret.
	SetMetadata(ctx context.Context, ref Ref, metadata map[string]string) error
}

// HostKeyReader is an optional interface. A provider implements it when the
// guest can publish its SSH host key through the cloud, outside the SSH
// connection itself.
type HostKeyReader interface {
	// GuestHostKey returns the host key the guest's boot unit published
	// (guest attribute guest.HostKeyAttrNamespace/guest.HostKeyAttrKey), in
	// authorized_keys form with no comment. ErrNotFound means the guest has
	// not published one yet; the caller retries while the instance boots.
	//
	// The value is a claim: any process in the guest can write it. The
	// caller compares it with the key sshd presents at first contact, pins
	// that key with SetMetadata under MetadataHostKeyPin, reads the pin
	// back, and never calls GuestHostKey for that instance again.
	GuestHostKey(ctx context.Context, ref Ref) (string, error)
}

// ImagePrefixFamily marks a configured image reference as a family rather
// than an exact name: "family/shed-extensions-amd64". A family hands out its
// newest image, so production configs name an exact image instead.
const ImagePrefixFamily = "family/"

// ImageResolver is an optional interface. A provider implements it when a
// configured image reference may need a lookup before it can be booted.
type ImageResolver interface {
	// ResolveImage turns a configured reference (remote.default_image or
	// an alias's value) into the exact image name to pass in Spec.Image and
	// store in ShedSpec.Image. A reference that begins ImagePrefixFamily
	// resolves to the family's newest image; any other reference is an
	// exact name and is checked to exist. ErrNotFound means there is no
	// such image or family.
	//
	// The backend resolves once per create, before Create, and never
	// again: a stopped shed keeps the image it was made from.
	ResolveImage(ctx context.Context, image string) (string, error)
}

// Instance is one VM as the cloud reports it. Values returned by a provider
// are copies; changing them changes nothing.
type Instance struct {
	// Ref is the handle for every later call.
	Ref Ref
	// Name is the cloud's name for the instance: InstanceName(env, id) for
	// one a shed-server made.
	Name string
	// State is the cloud's view. See State.
	State State
	// Labels are the instance's labels, shed's and any others.
	Labels map[string]string
	// Metadata holds the instance metadata keys that begin "shed-". Other
	// keys are left out.
	Metadata map[string]string
	// Address is the public IPv4 address the server connects to, or "" when
	// the instance has none (it is stopped, or still being created). Unless
	// Capabilities.StableAddress, it changes at every start.
	Address string
	// InternalAddress is the address on the shed network. Informational.
	InternalAddress string
	// Zone is where the instance runs, in the provider's terms.
	Zone string
	// MachineType is the cloud's machine type name; CPUs, MemoryMB and Arch
	// describe it.
	MachineType string
	CPUs        int
	MemoryMB    int
	Arch        string
	// DiskGB is the boot disk size.
	DiskGB int
	// Image is the image the boot disk was made from, when the cloud still
	// reports it.
	Image string
	// MaxRun is the run limit in force, or zero when there is none.
	MaxRun time.Duration
	// CreatedAt is when the cloud created the instance. The shed's own
	// creation time is in ShedSpec.CreatedAt.
	CreatedAt time.Time
	// StartedAt and StoppedAt are the last start and the last stop. Zero
	// when not known.
	StartedAt time.Time
	StoppedAt time.Time
	// PlannedStopAt is when the cloud will stop the instance for its run
	// limit. Zero when it is not running or has no limit.
	PlannedStopAt time.Time
}

// Identity returns who the instance says it is, from its labels.
func (i Instance) Identity() (Identity, error) {
	return IdentityFromLabels(i.Labels)
}

// ShedState returns the instance's shed_state label, or "" when unset.
func (i Instance) ShedState() ShedState {
	return ShedState(i.Labels[LabelState])
}

// Layout returns the guest layout recorded in the shed_layout label, or 0
// when the label is unset or not a number.
func (i Instance) Layout() int {
	return parseLayout(i.Labels[LabelLayout])
}

// HostKeyPin returns the pinned SSH host key from metadata, or "" when the
// instance has no pin.
func (i Instance) HostKeyPin() string {
	return i.Metadata[MetadataHostKeyPin]
}

// ShedSpec decodes the instance's shed-spec metadata.
func (i Instance) ShedSpec() (ShedSpec, error) {
	return DecodeShedSpec(i.Metadata[MetadataShedSpec])
}
