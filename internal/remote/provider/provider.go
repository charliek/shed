// Package provider is the seam between the remote backend and whatever gives
// it one machine per shed: a compute service (provider/service, over the
// compute contract, tests/compute-contract/v1) or the in-memory fake
// (provider/fake, tests). The backend (internal/remote) holds a Provider and
// never talks to a cloud or a service directly; a provider never touches
// shed records, the agent or SSH.
//
// # Accepted, then wait
//
// Create, Start, Stop and Destroy return as soon as the work has been
// accepted, which takes seconds. Each returns a Pending. Wait blocks until
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
// # Scope
//
// A provider sees only this server's machines: the service scopes every call
// by the server's token (its enrolled name), so nothing here checks whose a
// machine is. A machine's owner (its user or its client) is fixed at create
// and is the truth about who owns the shed.
//
// # Request ids
//
// Every call that changes something carries a request id. A provider answers
// a repeated request id with the first answer, so a caller whose reply was
// lost (ErrUnavailable from a mutation) sends the call again with the same
// id, and it takes effect once. NewRequestID makes one.
//
// # Refusals
//
// A service may refuse for quota or capacity either at once (the accepting
// call returns the error) or later (Wait returns it). Callers handle both the
// same way; see ErrQuota and ErrCapacity.
//
// The package imports only internal/remote/guest, so internal/config may
// import it.
package provider

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/netip"
	"slices"
	"time"
)

// Provider names, the values of remote.provider in the server config.
const (
	NameService = "service"
	NameFake    = "fake"
)

// FastFailWindow is how long the create handler waits on a new machine's
// Pending before it answers. A refusal that arrives inside the window is
// returned on the create reply and leaves no record; after it the handler
// answers 201 "starting" and a job keeps waiting.
const FastFailWindow = 15 * time.Second

// Ref identifies one machine to the provider: its shed id. Callers store it
// once the create has been accepted (an empty Ref means "not accepted yet")
// and pass it back. It stays valid across stop and start and across server
// restarts.
type Ref string

// State is where a machine is in its life, as the provider reports it. It is
// not a shed status: the backend derives the status from the State, the
// shed_state label and an agent probe.
type State string

const (
	// StateStarting: the machine exists and is on its way to running (a
	// machine being created is starting too).
	StateStarting State = "starting"
	// StateRunning: the machine runs. The guest may still be booting.
	StateRunning State = "running"
	// StateStopping: the machine is on its way to stopped.
	StateStopping State = "stopping"
	// StateStopped: the machine is off and its disk is kept.
	StateStopped State = "stopped"
	// StateDeleting: the service is deleting the machine (its lease
	// ended, or it was deleted at the service). Nothing but the delete
	// will finish; the backend never answers it with a stop.
	StateDeleting State = "deleting"
	// StateUnknown: the provider reported a state this build does not map.
	// Callers treat it as not settled.
	StateUnknown State = "unknown"
	// StateAbsent is never reported by Get or List. It is only a wait
	// target: Pending.Want for a Destroy.
	StateAbsent State = "absent"
)

// Settled reports whether the state is one a machine rests in. Starting,
// stopping, deleting and unknown are not settled.
func (s State) Settled() bool {
	return s == StateRunning || s == StateStopped || s == StateAbsent
}

// Pending describes work the provider has accepted and not yet finished. It
// is plain data: the backend may store it in a shed record and hand it to
// Wait after a restart, from another process.
type Pending struct {
	// Ref is the machine the work is on.
	Ref Ref `json:"ref"`
	// Operation is the provider's token for the operation. It is empty
	// when there is nothing to follow (the call was a no-op) or the token
	// was lost. Wait then follows the machine's state instead, and cannot
	// report why an operation failed.
	Operation string `json:"operation,omitempty"`
	// Want is the state that means the work is done: StateRunning for
	// Create and Start, StateStopped for Stop, StateAbsent for Destroy.
	// Empty means "whatever settled state it reaches"; see Settle. It is
	// kept with the operation because an operation the provider no longer
	// knows is followed by the machine's state, and only Want says which
	// state is done.
	Want State `json:"want,omitempty"`
}

// Settle returns a Pending that makes Wait block until the machine has no
// transition in progress, whichever state it ends in. Use it after ErrBusy,
// and after a restart when no Pending was stored.
func Settle(ref Ref) Pending {
	return Pending{Ref: ref}
}

// Provider creates and controls machines. Every method is safe to call from
// many goroutines. Every method honours ctx: when ctx ends it returns an
// error that wraps ctx.Err(), and the provider-side work, if already
// accepted, carries on.
type Provider interface {
	// Name returns the provider's name: NameService or NameFake.
	Name() string

	// Catalog returns what the provider offers: sizes, images, access
	// kinds, the lease policy and the limits. The backend reads it at
	// start-up and at every periodic pass, and keeps the last one.
	Catalog(ctx context.Context) (Catalog, error)

	// Create asks for a new machine and returns once the request is
	// accepted, with the machine as the accepting answer gave it (state
	// starting while it is being made). It does not wait for the machine
	// to run; pass the Pending to Wait for that.
	//
	// Idempotent on spec.ShedID: a machine with that shed id and the same
	// spec (spec.RequestID aside) is answered as it is, with a Pending
	// for it. The same shed id with another spec is ErrSpecMismatch, and a
	// name held by another shed id is ErrExists. A repeated
	// spec.RequestID returns the first answer.
	//
	// ErrQuota and ErrCapacity may be returned here or by Wait; the
	// service's judgement (ErrComputeLimit, ErrPolicy) only here. ErrInvalid
	// means the spec is malformed and nothing was sent. ErrUnavailable means
	// the outcome is not known: call Create again with the same spec.
	Create(ctx context.Context, spec Spec) (Pending, Instance, error)

	// Start asks for a stopped machine to start and returns once the
	// request is accepted.
	//
	// Idempotent: on a machine that is running, or already starting, it
	// starts nothing and returns a Pending that Wait resolves when the
	// machine runs. On a machine that is stopping, or being created, it
	// returns ErrBusy. ErrNotFound means no such machine; ErrDeleting a
	// machine being deleted; ErrLease a machine whose lease has ended or
	// whose person is no longer active. ErrQuota and ErrCapacity may be
	// returned here or by Wait; after either the machine is left stopped.
	Start(ctx context.Context, ref Ref, requestID string) (Pending, error)

	// Stop asks for a running machine to stop, keeping its disk, and
	// returns once the request is accepted.
	//
	// Idempotent: on a machine that is stopped, or already stopping, it
	// stops nothing and returns a Pending that Wait resolves when the
	// machine is stopped. On a machine that is starting it returns
	// ErrBusy. ErrNotFound means no such machine; ErrDeleting a machine
	// being deleted.
	Stop(ctx context.Context, ref Ref, requestID string) (Pending, error)

	// Destroy asks for the machine and its disk to be deleted and returns
	// once the request is accepted.
	//
	// Not found is success: if the machine does not exist, Destroy returns
	// a Pending and a nil error, and Wait on it returns at once. Destroy
	// never returns ErrNotFound. Calling it again while the delete is in
	// progress is also success, and ends any create, start or stop in
	// progress on the machine.
	Destroy(ctx context.Context, ref Ref, requestID string) (Pending, error)

	// Wait blocks until the work p describes has finished or ctx ends. The
	// provider does the polling.
	//
	// It returns a nil error when the machine has reached p.Want, with a
	// fresh view of the machine (the zero Instance for StateAbsent).
	//
	// When ctx ends first it returns an error wrapping ctx.Err(). Nothing
	// is cancelled, and Wait may be called again with the same Pending,
	// any number of times, from any process.
	//
	// When the operation failed it returns the reason: ErrQuota,
	// ErrCapacity, ErrDeleting, ErrLease, or another error. A failed create
	// leaves no machine; a failed start leaves it stopped.
	//
	// When the machine disappears while p.Want is not StateAbsent it
	// returns ErrNotFound. When the machine settles in a state other than
	// p.Want (it was stopped from outside while starting, for example) it
	// returns ErrUnexpectedState together with the machine as it is.
	//
	// With an empty p.Operation, or one the provider no longer knows, Wait
	// follows the machine's state alone. With an empty p.Want it returns as
	// soon as the machine is settled: nil with the machine, or ErrNotFound
	// if it is gone.
	Wait(ctx context.Context, p Pending) (Instance, error)

	// Get returns the machine as the provider reports it now. ErrNotFound
	// means it does not exist.
	Get(ctx context.Context, ref Ref) (Instance, error)

	// List returns every live machine of this server, in any state,
	// ordered by Ref, and the machines deleted recently (the last 24
	// hours) with why. It returns the complete set or an error, never a
	// partial list; even so, a caller drops a record only after an exact
	// Get of the missing machine answers ErrNotFound.
	List(ctx context.Context) (Listing, error)

	// SetLabels writes the machine's working labels in one atomic write:
	// every key of set is set to its value, or removed when the value is
	// nil. expect, when not empty, is a condition on the labels before the
	// write: a value means "holds exactly this", nil "is absent"; when it
	// does not hold the write is ErrLabelTaken and nothing changes. A
	// reserved label (ReservedLabels) is ErrInvalid. It completes before it
	// returns and returns the machine as written.
	SetLabels(ctx context.Context, ref Ref, requestID string, set, expect map[string]*string) (Instance, error)

	// Extend moves the machine's lease end later by by (whole hours,
	// 1 to 720), never past its cap. ErrLease says why it was refused
	// (extends not allowed, the cap, the lease ended, the person no longer
	// active); ErrDeleting a machine being deleted. It returns the machine
	// with its new lease.
	Extend(ctx context.Context, ref Ref, requestID string, by time.Duration) (Instance, error)

	// Usage returns the machines held against the limits: the
	// organization's, and with user set that person's (nil when the
	// service would refuse the person a machine).
	Usage(ctx context.Context, user string) (Usage, error)

	// Ingress returns the sources this server's machines admit SSH from,
	// as last applied, and whether the provider can say that nothing else
	// reaches them (Ingress.Exclusive).
	Ingress(ctx context.Context) (Ingress, error)

	// SetIngress replaces the sources whole and returns the ingress as
	// applied. ErrIngress means the provider will not apply them: its
	// ingress is managed elsewhere (Error.Code INGRESS_NOT_WRITABLE) or a
	// source is wider than it allows (INGRESS_TOO_WIDE).
	SetIngress(ctx context.Context, requestID string, sources []netip.Prefix) (Ingress, error)
}

// ValidExtend reports whether by is an extend the seam takes: whole hours,
// 1 to 720.
func ValidExtend(by time.Duration) bool {
	return by >= time.Hour && by <= 720*time.Hour && by%time.Hour == 0
}

// Reached judges a machine against what p waits for: done with nil when it
// is in p.Want (or settled, for an empty Want), done with ErrUnexpectedState
// when it settled elsewhere, not done while it moves.
func (p Pending) Reached(inst Instance) (done bool, err error) {
	if !inst.State.Settled() {
		return false, nil
	}
	if p.Want == "" || inst.State == p.Want {
		return true, nil
	}
	return true, Errorf(ErrUnexpectedState, "wait", "%s is %s, want %s", p.Ref, inst.State, p.Want)
}

// NewRequestID returns a new request id: a lower-case random UUID.
func NewRequestID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("provider: no randomness for a request id: " + err.Error())
	}
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	h := hex.EncodeToString(b[:])
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}

// Architectures.
const (
	ArchAMD64 = "amd64"
	ArchARM64 = "arm64"
)

// Size is one size of the catalog: a named machine shape.
type Size struct {
	Name     string `json:"name"`
	CPUs     int    `json:"cpus"`
	MemoryMB int    `json:"memory_mb"`
	Arch     string `json:"arch"`
	// DiskGBDefault is the disk a machine of this size gets when the
	// create names none. Zero in a machine's own size.
	DiskGBDefault int `json:"disk_gb_default,omitempty"`
}

// ImageDefault is the image a create names when it names none.
const ImageDefault = "default"

// Policy is the provider's lease policy.
type Policy struct {
	// PersonInitialLease and PersonMaxAge are a person's first lease and
	// its cap, from creation.
	PersonInitialLease time.Duration
	PersonMaxAge       time.Duration
	// PersonExtend says whether a person may extend; PresetsH are the
	// hours offered.
	PersonExtend bool
	PresetsH     []int
	// ClientMaxLease bounds a client's lease.
	ClientMaxLease time.Duration
	// Warn is how long before a lease's end a person is warned.
	Warn time.Duration
}

// Limits are the machines a person and the organization may hold.
type Limits struct {
	PersonMax int
	OrgMax    int
}

// Catalog is what a provider offers.
type Catalog struct {
	Sizes       []Size
	DefaultSize string
	// DefaultImage is what the image "default" resolves to now, for
	// display only.
	DefaultImage string
	// Aliases are the other image names a create may give, never
	// "default".
	Aliases     []string
	AccessKinds []string
	Policy      Policy
	Limits      Limits
	// IngressWritable is false for a provider whose ingress is managed
	// elsewhere: the server may read it, not write it.
	IngressWritable bool
	// OwnerRequired says a machine is made only for a person or a client
	// (the compute contract's rule): a create with neither is refused by
	// the backend before it is sent. It is not on the wire: the service
	// provider always sets it, the fake only when asked (Options), and the
	// backend applies the rule on remote.provider: service whatever this
	// says.
	OwnerRequired bool
}

// SizeNamed returns the size of that name.
func (c Catalog) SizeNamed(name string) (Size, bool) {
	for _, s := range c.Sizes {
		if s.Name == name {
			return s, true
		}
	}
	return Size{}, false
}

// HasImage reports whether a create may name image: empty or ImageDefault,
// or one of the aliases.
func (c Catalog) HasImage(image string) bool {
	return image == "" || image == ImageDefault || slices.Contains(c.Aliases, image)
}

// GuestArch is the architecture every size shares, or "" when the sizes mix
// architectures or there are none.
func (c Catalog) GuestArch() string {
	arch := ""
	for _, s := range c.Sizes {
		if s.Arch == "" || (arch != "" && s.Arch != arch) {
			return ""
		}
		arch = s.Arch
	}
	return arch
}

// Lease is a machine's lease: when the provider deletes it, and how far it
// may be extended.
type Lease struct {
	ExpiresAt    time.Time `json:"expires_at"`
	MaxExpiresAt time.Time `json:"max_expires_at"`
	Extendable   bool      `json:"extendable"`
	PresetsH     []int     `json:"presets_h,omitempty"`
}

// AccessSSH is the one access kind this build supports: direct SSH.
const AccessSSH = "ssh"

// Access is one way to reach a machine, of a kind this build supports: for
// AccessSSH, SSH to Host:Port as User, presenting HostKey. Host is an IP
// literal the provider validated.
type Access struct {
	Kind    string `json:"kind"`
	Host    string `json:"host"`
	Port    int    `json:"port"`
	User    string `json:"user"`
	HostKey string `json:"host_key"`
}

// Instance is one machine as the provider reports it. Values returned by a
// provider are copies; changing them changes nothing. It is plain data, and
// the backend keeps the last one it saw in the shed's record.
type Instance struct {
	// Ref is the handle for every later call: the shed id.
	Ref Ref `json:"ref"`
	// Name is the shed's name.
	Name  string `json:"name"`
	State State  `json:"state"`
	// User and Client are who owns the machine, fixed at create: a
	// person's handle or a client's name, at most one set.
	User   string `json:"user,omitempty"`
	Client string `json:"client,omitempty"`
	// Size is the machine's size (DiskGBDefault unset).
	Size   Size `json:"size"`
	DiskGB int  `json:"disk_gb"`
	// Image is the image the create asked for ("default" or an alias);
	// ImageResolved what it resolved to.
	Image         string `json:"image"`
	ImageResolved string `json:"image_resolved"`
	// Provider is a word for display: which cloud, say.
	Provider string `json:"provider"`
	// CreatedAt is when the provider made the machine; StartedAt and
	// StoppedAt the last start and the last stop (zero: not known, or not
	// since).
	CreatedAt time.Time `json:"created_at"`
	StartedAt time.Time `json:"started_at,omitempty"`
	StoppedAt time.Time `json:"stopped_at,omitempty"`
	Lease     Lease     `json:"lease"`
	// Access are the ways to reach the machine of kinds this build
	// supports, in the provider's order; IgnoredAccess counts the options
	// of other kinds that were left out. Empty until the machine runs and
	// its host key is known.
	Access        []Access `json:"access,omitempty"`
	IgnoredAccess int      `json:"ignored_access,omitempty"`
	// Labels are the server's working labels (LabelState, LabelLayout).
	Labels map[string]string `json:"labels,omitempty"`
	// Spec is the create's spec, verbatim: the encoded ShedSpec.
	Spec string `json:"spec,omitempty"`
}

// ShedState returns the machine's shed_state label, or "" when unset.
func (i Instance) ShedState() ShedState {
	return ShedState(i.Labels[LabelState])
}

// Layout returns the guest layout recorded in the shed_layout label, or 0
// when the label is unset or not a number.
func (i Instance) Layout() int {
	return parseLayout(i.Labels[LabelLayout])
}

// ShedSpec decodes the machine's spec.
func (i Instance) ShedSpec() (ShedSpec, error) {
	return DecodeShedSpec(i.Spec)
}

// Clone returns a deep copy.
func (i Instance) Clone() Instance {
	out := i
	out.Lease.PresetsH = slices.Clone(i.Lease.PresetsH)
	out.Access = slices.Clone(i.Access)
	if i.Labels != nil {
		out.Labels = make(map[string]string, len(i.Labels))
		for k, v := range i.Labels {
			out.Labels[k] = v
		}
	}
	return out
}

// Deletion causes, as a listing reports them.
const (
	CauseLeaseEnded = "lease_ended"
	CausePage       = "page"
	CauseExternal   = "external"
	CauseServer     = "server"
)

// Deletion is a machine deleted recently, and why.
type Deletion struct {
	Ref   Ref
	At    time.Time
	Cause string
}

// Listing is List's answer.
type Listing struct {
	Instances []Instance
	Deleted   []Deletion
}

// Counts is one line of usage.
type Counts struct {
	Machines int
	Running  int
	Stopped  int
	Max      int
}

// Usage is the machines held against the limits.
type Usage struct {
	// Person is nil when no person was asked about, or the provider would
	// refuse that person a machine.
	Person *Counts
	Org    Counts
}

// Ingress is where this server's machines admit SSH from.
type Ingress struct {
	// Sources are the prefixes, as last applied, in canonical form.
	Sources []netip.Prefix
	// Exclusive says the provider read every rule it needs to say that the
	// machines admit nothing else, cannot reach each other and share no
	// network with anything else that admits inbound traffic.
	Exclusive bool
	// AppliedAt is when the sources were last applied, zero before any
	// write.
	AppliedAt time.Time
}
