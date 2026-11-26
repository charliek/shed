// Package fake is an in-memory provider.Provider for tests and for the
// development driver (remote.provider: fake). It keeps machines in a map,
// runs no VM, and follows the compute contract's rules where the seam
// carries them: request ids answered once, creates idempotent by shed id, a
// delete that ends the work in progress, leases, recent deletions with their
// causes, refusals with the contract's codes.
//
// Knobs:
//   - Options.Manual: operations finish only when the test calls Complete;
//     otherwise they finish before the accepting call returns.
//   - FailNext: the next call of a kind returns an error and changes nothing.
//   - LoseNextReply: the next call of a kind takes effect, keeps its answer
//     under its request id, and returns ErrUnavailable (a lost reply).
//   - FailOperation: the next operation of a kind is accepted, then fails, so
//     Wait returns the error (a refusal that arrives late).
//   - ForgetOperations: every operation is forgotten, as an operation kept
//     past its 24 hours is.
//   - ExternalStop, ExternalDelete, DeleteAs, EndLeases: the machine changes
//     behind the server's back (stopped, deleted outside, deleted at the
//     service, its lease ended).
//   - Seed: put a machine in directly, for adoption tests.
//   - PublishHostKey, SetAccess, SetOwner: what the service reports.
//   - SetIngressState, SetIngressWritable, IngressWrites: the ingress as
//     someone else left it.
//   - Calls, Count, LabelWrites: what was called, in order.
package fake

import (
	"context"
	"fmt"
	"maps"
	"net/netip"
	"slices"
	"sort"
	"sync"
	"time"

	"github.com/charliek/shed/internal/remote/provider"
)

// Op names a provider call, for FailNext, FailOperation and Calls.
type Op string

const (
	OpCatalog    Op = "catalog"
	OpCreate     Op = "create"
	OpStart      Op = "start"
	OpStop       Op = "stop"
	OpDestroy    Op = "destroy"
	OpWait       Op = "wait"
	OpGet        Op = "get"
	OpList       Op = "list"
	OpSetLabels  Op = "set_labels"
	OpExtend     Op = "extend"
	OpUsage      Op = "usage"
	OpIngress    Op = "ingress"
	OpSetIngress Op = "set_ingress"
)

// DefaultIngressSource is the source the fake's ingress starts with unless
// Options.IngressSources says otherwise. It is a documentation address.
const DefaultIngressSource = "192.0.2.1/32"

// DefaultHostKey is the host key every machine's guest publishes unless
// Options.HostKey names another or Options.NoHostKey is set.
const DefaultHostKey = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIGZha2UtaG9zdC1rZXktZmFrZS1ob3N0LWtleS1mYWtl"

// ProviderWord is the word machines carry as their provider.
const ProviderWord = "fake"

// DefaultCatalog is the catalog the fake offers unless Options.Catalog says
// otherwise: the compute contract's world, with generous limits.
func DefaultCatalog() provider.Catalog {
	return provider.Catalog{
		Sizes: []provider.Size{
			{Name: "standard", CPUs: 2, MemoryMB: 8192, Arch: provider.ArchAMD64, DiskGBDefault: 50},
			{Name: "large", CPUs: 8, MemoryMB: 32768, Arch: provider.ArchAMD64, DiskGBDefault: 100},
		},
		DefaultSize:  "standard",
		DefaultImage: "shed-remote-full-fake",
		Aliases:      []string{"base", "cloud"},
		AccessKinds:  []string{provider.AccessSSH},
		Policy: provider.Policy{
			PersonInitialLease: 24 * time.Hour,
			PersonMaxAge:       5 * 24 * time.Hour,
			PersonExtend:       true,
			PresetsH:           []int{6, 12, 24, 48},
			ClientMaxLease:     5 * 24 * time.Hour,
			Warn:               2 * time.Hour,
		},
		Limits:          provider.Limits{PersonMax: 100, OrgMax: 100},
		IngressWritable: true,
	}
}

// Options configures a fake provider. The zero value is usable: operations
// finish before the accepting call returns.
type Options struct {
	// Now is the clock for timestamps and leases. Nil means time.Now.
	Now func() time.Time
	// Manual makes every operation wait for Complete.
	Manual bool
	// Catalog replaces DefaultCatalog.
	Catalog *provider.Catalog
	// OwnerRequired makes the catalog say a machine is made only for a
	// person or a client, as the compute contract does.
	OwnerRequired bool
	// HostKey is the key every guest publishes when its machine first
	// runs; empty means DefaultHostKey. NoHostKey publishes none (a guest
	// that has not published yet: its machine has no access).
	HostKey   string
	NoHostKey bool
	// IngressSources are the sources the fake's ingress starts with.
	// Empty means DefaultIngressSource.
	IngressSources []string
}

// Call records one provider call.
type Call struct {
	Op Op
	// Ref is the machine the call named.
	Ref provider.Ref
}

type operation struct {
	id       string
	kind     Op
	ref      provider.Ref
	prev     provider.State
	failWith error
	done     chan struct{}
	err      error
	// gone: the operation ended with the machine deleted.
	gone bool
}

type machine struct {
	inst    provider.Instance
	spec    provider.Spec
	op      *operation // in flight; nil when settled
	hostKey string
}

// kept is a request id's first answer.
type kept struct {
	call    string // op and ref the id was used for
	pending provider.Pending
	inst    provider.Instance
	ingress provider.Ingress
}

// Provider is the fake. Create one with New.
type Provider struct {
	opts    Options
	catalog provider.Catalog

	mu        sync.Mutex
	machines  map[provider.Ref]*machine
	ops       map[string]*operation
	requests  map[string]kept
	deleted   []provider.Deletion
	failCall  map[Op][]error
	loseReply map[Op]int
	failOp    map[Op][]error
	calls     []Call
	nextOp    int
	nextAddr  int
	// labelWrites counts the SetLabels calls that changed a label.
	labelWrites int

	ingress provider.Ingress
	// ingressWrites are the source lists SetIngress applied, in order.
	ingressWrites [][]string
}

var _ provider.Provider = (*Provider)(nil)

// New returns an empty fake provider.
func New(opts Options) *Provider {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	cat := DefaultCatalog()
	if opts.Catalog != nil {
		cat = cloneCatalog(*opts.Catalog)
	}
	if opts.OwnerRequired {
		cat.OwnerRequired = true
	}
	p := &Provider{
		opts:      opts,
		catalog:   cat,
		machines:  make(map[provider.Ref]*machine),
		ops:       make(map[string]*operation),
		requests:  make(map[string]kept),
		failCall:  make(map[Op][]error),
		loseReply: make(map[Op]int),
		failOp:    make(map[Op][]error),
	}
	sources := opts.IngressSources
	if len(sources) == 0 {
		sources = []string{DefaultIngressSource}
	}
	p.ingress.Sources = mustPrefixes(sources)
	p.ingress.Exclusive = true
	return p
}

// mustPrefixes parses a test's source list, masked.
func mustPrefixes(sources []string) []netip.Prefix {
	var out []netip.Prefix
	for _, s := range sources {
		out = append(out, netip.MustParsePrefix(s).Masked())
	}
	return out
}

func cloneCatalog(c provider.Catalog) provider.Catalog {
	c.Sizes = slices.Clone(c.Sizes)
	c.Aliases = slices.Clone(c.Aliases)
	c.AccessKinds = slices.Clone(c.AccessKinds)
	c.Policy.PresetsH = slices.Clone(c.Policy.PresetsH)
	return c
}

// errorf builds a provider error carrying a contract code.
func errorf(kind error, op Op, code, format string, args ...any) error {
	return &provider.Error{Kind: kind, Op: string(op), Code: code, Detail: fmt.Sprintf(format, args...)}
}

// --- test hooks ---

// FailNext makes the next call of the given kind return err without
// changing anything. Calls queue: two FailNext calls fail the next two.
func (p *Provider) FailNext(op Op, err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.failCall[op] = append(p.failCall[op], err)
}

// LoseNextReply makes the next call of the given kind (a call that changes
// something) take effect and keep its answer under its request id, then
// return ErrUnavailable, as a reply lost on the way back. The same call sent
// again with the same request id gets the kept answer.
func (p *Provider) LoseNextReply(op Op) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.loseReply[op]++
}

// LabelWrites returns how many SetLabels calls changed a label so far.
func (p *Provider) LabelWrites() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.labelWrites
}

// FailOperation makes the next accepted operation of the given kind
// (OpCreate, OpStart, OpStop or OpDestroy) fail when it finishes: the
// accepting call succeeds and Wait returns err. A failed create removes the
// machine; a failed start, stop or destroy puts it back in the state it was
// in.
func (p *Provider) FailOperation(op Op, err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.failOp[op] = append(p.failOp[op], err)
}

// ForgetOperations forgets every operation, as a provider does an operation
// kept past its time: a Wait on one follows the machine instead. Work in
// progress carries on.
func (p *Provider) ForgetOperations() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.ops = map[string]*operation{}
}

// Complete finishes the operation in flight on a machine. It is how a Manual
// provider moves forward. It reports whether there was an operation to
// finish.
func (p *Provider) Complete(ref provider.Ref) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	m, ok := p.machines[ref]
	if !ok || m.op == nil {
		return false
	}
	p.finishLocked(m.op)
	return true
}

// ExternalStop stops a settled, running machine at once, as something
// outside the server would. It records no call.
func (p *Provider) ExternalStop(ref provider.Ref) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	m, ok := p.machines[ref]
	switch {
	case !ok:
		return errorf(provider.ErrNotFound, "external_stop", "NOT_FOUND", "%s", ref)
	case m.op != nil || m.inst.State != provider.StateRunning:
		return errorf(provider.ErrBusy, "external_stop", "BUSY", "%s is %s", ref, m.inst.State)
	}
	p.markStoppedLocked(m)
	return nil
}

// ExternalDelete removes a machine at once, whatever it is doing, as gone
// from the cloud without the service (cause external). A Wait in progress on
// it returns ErrDeleting.
func (p *Provider) ExternalDelete(ref provider.Ref) {
	p.DeleteAs(ref, provider.CauseExternal)
}

// DeleteAs removes a machine at once, whatever it is doing, and lists the
// deletion with the cause (provider.Cause*). A Wait in progress on it returns
// ErrDeleting.
func (p *Provider) DeleteAs(ref provider.Ref, cause string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if m, ok := p.machines[ref]; ok {
		code := "DELETING"
		kind := provider.ErrDeleting
		if cause == provider.CauseLeaseEnded {
			code, kind = "LEASE_ENDED", provider.ErrLease
		}
		p.removeLocked(m, cause, errorf(kind, OpWait, code, "%s was deleted (%s)", ref, cause))
	}
}

// EndLeases deletes every machine at or past its lease's end, as the
// service's sweep does, with cause lease_ended. It returns the refs deleted.
func (p *Provider) EndLeases() []provider.Ref {
	p.mu.Lock()
	now := p.opts.Now()
	var due []provider.Ref
	for ref, m := range p.machines {
		if !m.inst.Lease.ExpiresAt.IsZero() && !now.Before(m.inst.Lease.ExpiresAt) {
			due = append(due, ref)
		}
	}
	p.mu.Unlock()
	sort.Slice(due, func(i, k int) bool { return due[i] < due[k] })
	for _, ref := range due {
		p.DeleteAs(ref, provider.CauseLeaseEnded)
	}
	return due
}

// SetDeleting puts a machine in StateDeleting, as a service whose own delete
// of it has begun, without ending it.
func (p *Provider) SetDeleting(ref provider.Ref) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if m, ok := p.machines[ref]; ok {
		m.inst.State = provider.StateDeleting
		m.inst.Access = nil
	}
}

// Seed adds a settled machine as if this server had created it and lost its
// record. State must be running or stopped. A missing Ref is the error of a
// test; a missing size, lease or access is filled in. It returns the Ref.
func (p *Provider) Seed(inst provider.Instance) provider.Ref {
	p.mu.Lock()
	defer p.mu.Unlock()
	if inst.Ref == "" {
		panic("fake.Seed: the instance has no Ref (its shed id)")
	}
	now := p.opts.Now()
	if inst.CreatedAt.IsZero() {
		inst.CreatedAt = now
	}
	if inst.Size.Name == "" {
		inst.Size = p.catalog.Sizes[0]
		inst.Size.DiskGBDefault = 0
	}
	if inst.Provider == "" {
		inst.Provider = ProviderWord
	}
	if inst.Lease.ExpiresAt.IsZero() {
		inst.Lease = provider.Lease{ExpiresAt: inst.CreatedAt.Add(p.catalog.Policy.PersonInitialLease), MaxExpiresAt: inst.CreatedAt.Add(p.catalog.Policy.PersonMaxAge)}
	}
	inst.Labels = maps.Clone(inst.Labels)
	m := &machine{inst: inst, hostKey: p.defaultHostKey()}
	if inst.State == provider.StateRunning && len(inst.Access) == 0 {
		p.giveAccessLocked(m)
	}
	m.inst.Lease = p.leaseLocked(m, now)
	p.machines[inst.Ref] = m
	return inst.Ref
}

// PublishHostKey has the machine's guest publish key: kept only when the
// machine has none yet (the service keeps the first key, write-once).
func (p *Provider) PublishHostKey(ref provider.Ref, key string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if m, ok := p.machines[ref]; ok && m.hostKey == "" {
		m.hostKey = key
		if m.inst.State == provider.StateRunning {
			p.giveAccessLocked(m)
		}
	}
}

// AttestHostKey replaces the host key the service attests for the machine,
// as a service that broke the write-once rule would: its access carries it
// from the next start.
func (p *Provider) AttestHostKey(ref provider.Ref, key string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if m, ok := p.machines[ref]; ok {
		m.hostKey = key
	}
}

// SetAccess replaces what the machine reports as its access options, with
// ignored options of kinds this build does not know.
func (p *Provider) SetAccess(ref provider.Ref, access []provider.Access, ignored int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if m, ok := p.machines[ref]; ok {
		m.inst.Access = slices.Clone(access)
		m.inst.IgnoredAccess = ignored
	}
}

// SetOwner replaces who the machine reports as its owner, as a service that
// answers otherwise than it was asked would.
func (p *Provider) SetOwner(ref provider.Ref, user, client string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if m, ok := p.machines[ref]; ok {
		m.inst.User, m.inst.Client = user, client
	}
}

// SetIngressState replaces the ingress as the provider reports it.
func (p *Provider) SetIngressState(sources []string, exclusive bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.ingress.Sources = mustPrefixes(sources)
	p.ingress.Exclusive = exclusive
}

// SetIngressWritable says whether SetIngress is allowed (false: the
// provider's ingress is managed elsewhere, INGRESS_NOT_WRITABLE).
func (p *Provider) SetIngressWritable(writable bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.catalog.IngressWritable = writable
}

// IngressWrites returns the source lists SetIngress applied, in order.
func (p *Provider) IngressWrites() [][]string {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([][]string, len(p.ingressWrites))
	for i, w := range p.ingressWrites {
		out[i] = slices.Clone(w)
	}
	return out
}

// Calls returns every provider call so far, in order, failed ones included.
func (p *Provider) Calls() []Call {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]Call(nil), p.calls...)
}

// Count returns how many calls of the given kind were made.
func (p *Provider) Count(op Op) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	n := 0
	for _, c := range p.calls {
		if c.Op == op {
			n++
		}
	}
	return n
}

// --- provider.Provider ---

// Name implements provider.Provider.
func (p *Provider) Name() string { return provider.NameFake }

// Catalog implements provider.Provider.
func (p *Provider) Catalog(ctx context.Context) (provider.Catalog, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.enterLocked(ctx, OpCatalog, ""); err != nil {
		return provider.Catalog{}, err
	}
	return cloneCatalog(p.catalog), nil
}

// Create implements provider.Provider.
func (p *Provider) Create(ctx context.Context, spec provider.Spec) (provider.Pending, provider.Instance, error) {
	ref := provider.Ref(spec.ShedID)
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.enterLocked(ctx, OpCreate, ref); err != nil {
		return provider.Pending{}, provider.Instance{}, err
	}
	if err := spec.Validate(); err != nil {
		return provider.Pending{}, provider.Instance{}, err
	}
	if k, ok, err := p.keptLocked(spec.RequestID, OpCreate, ref); ok || err != nil {
		return k.pending, k.inst, err
	}
	if m, ok := p.machines[ref]; ok {
		if !provider.SameCreate(m.spec, spec) {
			return provider.Pending{}, provider.Instance{}, errorf(provider.ErrSpecMismatch, OpCreate, "SPEC_MISMATCH", "%s was created with another body", ref)
		}
		pend := pendingFor(m, provider.StateRunning)
		return p.answerLocked(OpCreate, spec.RequestID, ref, kept{pending: pend, inst: p.viewLocked(m)})
	}
	for _, m := range p.machines {
		if m.inst.Name == spec.Name {
			return provider.Pending{}, provider.Instance{}, errorf(provider.ErrExists, OpCreate, "EXISTS", "%s is held by another machine", spec.Name)
		}
	}
	if err := p.judgeLocked(spec); err != nil {
		return provider.Pending{}, provider.Instance{}, err
	}
	size, _ := p.catalog.SizeNamed(spec.Size)
	disk := spec.DiskGB
	if disk == 0 {
		disk = size.DiskGBDefault
	}
	size.DiskGBDefault = 0
	resolved := p.catalog.DefaultImage
	if spec.Image != provider.ImageDefault {
		resolved = "shed-remote-" + spec.Image + "-fake"
	}
	now := p.stamp()
	m := &machine{spec: *spec.Clone(), hostKey: p.defaultHostKey(), inst: provider.Instance{
		Ref:           ref,
		Name:          spec.Name,
		State:         provider.StateStarting,
		User:          spec.User,
		Client:        spec.Client,
		Size:          size,
		DiskGB:        disk,
		Image:         spec.Image,
		ImageResolved: resolved,
		Provider:      ProviderWord,
		CreatedAt:     now,
		Labels:        maps.Clone(spec.Labels),
		Spec:          spec.Spec,
	}}
	if spec.Client != "" {
		end := now.Add(time.Duration(spec.LeaseS) * time.Second)
		m.inst.Lease = provider.Lease{ExpiresAt: end, MaxExpiresAt: end}
	} else {
		m.inst.Lease = provider.Lease{ExpiresAt: now.Add(p.catalog.Policy.PersonInitialLease), MaxExpiresAt: now.Add(p.catalog.Policy.PersonMaxAge)}
	}
	m.inst.Lease = p.leaseLocked(m, now)
	p.machines[ref] = m
	op := p.beginLocked(m, OpCreate, provider.StateAbsent)
	pend := provider.Pending{Ref: ref, Operation: op.id, Want: provider.StateRunning}
	return p.answerLocked(OpCreate, spec.RequestID, ref, kept{pending: pend, inst: p.viewLocked(m)})
}

// judgeLocked is the create's judgement: the owner, the size, the image, a
// client's lease, the access kinds and the limits.
func (p *Provider) judgeLocked(spec provider.Spec) error {
	c := p.catalog
	if c.OwnerRequired && spec.User == "" && spec.Client == "" {
		return errorf(provider.ErrPolicy, OpCreate, "PERSON_UNKNOWN", "a machine is made only for a person or a client")
	}
	if _, ok := c.SizeNamed(spec.Size); !ok {
		return errorf(provider.ErrPolicy, OpCreate, "SIZE_UNKNOWN", "no size %q", spec.Size)
	}
	if !c.HasImage(spec.Image) {
		return errorf(provider.ErrPolicy, OpCreate, "IMAGE_UNKNOWN", "no image %q", spec.Image)
	}
	if spec.Client != "" && time.Duration(spec.LeaseS)*time.Second > c.Policy.ClientMaxLease {
		return errorf(provider.ErrPolicy, OpCreate, "LEASE_TOO_LONG", "a lease of %ds is over %s", spec.LeaseS, c.Policy.ClientMaxLease)
	}
	if !slices.ContainsFunc(spec.AccessKinds, func(k string) bool { return slices.Contains(c.AccessKinds, k) }) {
		return errorf(provider.ErrPolicy, OpCreate, "ACCESS_UNSUPPORTED", "no access kind of %v is offered", spec.AccessKinds)
	}
	person := 0
	for _, m := range p.machines {
		if spec.User != "" && m.inst.User == spec.User {
			person++
		}
	}
	if spec.User != "" && person >= c.Limits.PersonMax {
		return errorf(provider.ErrComputeLimit, OpCreate, "QUOTA_PERSON", "%s holds %d machines", spec.User, person)
	}
	if len(p.machines) >= c.Limits.OrgMax {
		return errorf(provider.ErrComputeLimit, OpCreate, "QUOTA_ORG", "the organization holds %d machines", len(p.machines))
	}
	return nil
}

// Start implements provider.Provider.
func (p *Provider) Start(ctx context.Context, ref provider.Ref, requestID string) (provider.Pending, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.enterLocked(ctx, OpStart, ref); err != nil {
		return provider.Pending{}, err
	}
	if k, ok, err := p.keptLocked(requestID, OpStart, ref); ok || err != nil {
		return k.pending, err
	}
	m, err := p.liveLocked(OpStart, ref)
	if err != nil {
		return provider.Pending{}, err
	}
	if !p.opts.Now().Before(m.inst.Lease.ExpiresAt) {
		return provider.Pending{}, errorf(provider.ErrLease, OpStart, "LEASE_ENDED", "%s: the lease has ended", ref)
	}
	var pend provider.Pending
	switch {
	case m.inst.State == provider.StateRunning:
		pend = provider.Pending{Ref: ref, Want: provider.StateRunning}
	case m.inst.State == provider.StateStarting && m.op != nil && m.op.kind == OpStart:
		pend = pendingFor(m, provider.StateRunning)
	case m.inst.State == provider.StateStopped:
		m.inst.State = provider.StateStarting
		op := p.beginLocked(m, OpStart, provider.StateStopped)
		pend = provider.Pending{Ref: ref, Operation: op.id, Want: provider.StateRunning}
	default:
		return provider.Pending{}, errorf(provider.ErrBusy, OpStart, "BUSY", "%s is %s", ref, m.inst.State)
	}
	pend, _, err = p.answerLocked(OpStart, requestID, ref, kept{pending: pend})
	return pend, err
}

// Stop implements provider.Provider.
func (p *Provider) Stop(ctx context.Context, ref provider.Ref, requestID string) (provider.Pending, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.enterLocked(ctx, OpStop, ref); err != nil {
		return provider.Pending{}, err
	}
	if k, ok, err := p.keptLocked(requestID, OpStop, ref); ok || err != nil {
		return k.pending, err
	}
	m, err := p.liveLocked(OpStop, ref)
	if err != nil {
		return provider.Pending{}, err
	}
	var pend provider.Pending
	switch {
	case m.inst.State == provider.StateStopped:
		pend = provider.Pending{Ref: ref, Want: provider.StateStopped}
	case m.op != nil && m.op.kind == OpStop:
		pend = pendingFor(m, provider.StateStopped)
	case m.inst.State == provider.StateRunning:
		m.inst.State = provider.StateStopping
		m.inst.Access = nil
		op := p.beginLocked(m, OpStop, provider.StateRunning)
		pend = provider.Pending{Ref: ref, Operation: op.id, Want: provider.StateStopped}
	default:
		return provider.Pending{}, errorf(provider.ErrBusy, OpStop, "BUSY", "%s is %s", ref, m.inst.State)
	}
	pend, _, err = p.answerLocked(OpStop, requestID, ref, kept{pending: pend})
	return pend, err
}

// Destroy implements provider.Provider.
func (p *Provider) Destroy(ctx context.Context, ref provider.Ref, requestID string) (provider.Pending, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.enterLocked(ctx, OpDestroy, ref); err != nil {
		return provider.Pending{}, err
	}
	if k, ok, err := p.keptLocked(requestID, OpDestroy, ref); ok || err != nil {
		return k.pending, err
	}
	m, ok := p.machines[ref]
	var pend provider.Pending
	switch {
	case !ok:
		pend = provider.Pending{Ref: ref, Want: provider.StateAbsent}
	case m.op != nil && m.op.kind == OpDestroy:
		pend = pendingFor(m, provider.StateAbsent)
	default:
		if m.op != nil {
			// A delete ends the work in progress.
			old := m.op
			m.op = nil
			old.err = errorf(provider.ErrDeleting, OpWait, "DELETING", "%s is being deleted", ref)
			close(old.done)
		}
		prev := m.inst.State
		m.inst.State = provider.StateDeleting
		m.inst.Access = nil
		op := p.beginLocked(m, OpDestroy, prev)
		pend = provider.Pending{Ref: ref, Operation: op.id, Want: provider.StateAbsent}
	}
	pend, _, err := p.answerLocked(OpDestroy, requestID, ref, kept{pending: pend})
	return pend, err
}

// Wait implements provider.Provider.
func (p *Provider) Wait(ctx context.Context, pend provider.Pending) (provider.Instance, error) {
	p.mu.Lock()
	if err := p.enterLocked(ctx, OpWait, pend.Ref); err != nil {
		p.mu.Unlock()
		return provider.Instance{}, err
	}
	op := p.ops[pend.Operation]
	p.mu.Unlock()

	// A known operation: wait for it and report its own failure.
	if op != nil {
		select {
		case <-op.done:
		case <-ctx.Done():
			return provider.Instance{}, ctxErr(ctx, OpWait)
		}
		if op.err != nil {
			return provider.Instance{}, op.err
		}
	}

	// Then follow the machine until it is settled.
	for {
		p.mu.Lock()
		m, ok := p.machines[pend.Ref]
		if !ok {
			p.mu.Unlock()
			if pend.Want == provider.StateAbsent {
				return provider.Instance{}, nil
			}
			return provider.Instance{}, errorf(provider.ErrNotFound, OpWait, "NOT_FOUND", "%s", pend.Ref)
		}
		if m.op == nil && m.inst.State.Settled() {
			view := p.viewLocked(m)
			p.mu.Unlock()
			_, err := pend.Reached(view)
			return view, err
		}
		var done <-chan struct{}
		if m.op != nil {
			done = m.op.done
		}
		p.mu.Unlock()
		if done == nil {
			// Not settled and nothing in progress here (deleting at the
			// service): look again shortly.
			select {
			case <-time.After(5 * time.Millisecond):
			case <-ctx.Done():
				return provider.Instance{}, ctxErr(ctx, OpWait)
			}
			continue
		}
		select {
		case <-done:
		case <-ctx.Done():
			return provider.Instance{}, ctxErr(ctx, OpWait)
		}
	}
}

// Get implements provider.Provider.
func (p *Provider) Get(ctx context.Context, ref provider.Ref) (provider.Instance, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.enterLocked(ctx, OpGet, ref); err != nil {
		return provider.Instance{}, err
	}
	m, ok := p.machines[ref]
	if !ok {
		return provider.Instance{}, errorf(provider.ErrNotFound, OpGet, "NOT_FOUND", "%s", ref)
	}
	return p.viewLocked(m), nil
}

// List implements provider.Provider.
func (p *Provider) List(ctx context.Context) (provider.Listing, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.enterLocked(ctx, OpList, ""); err != nil {
		return provider.Listing{}, err
	}
	out := provider.Listing{Instances: make([]provider.Instance, 0, len(p.machines))}
	for _, m := range p.machines {
		out.Instances = append(out.Instances, p.viewLocked(m))
	}
	sort.Slice(out.Instances, func(a, b int) bool { return out.Instances[a].Ref < out.Instances[b].Ref })
	cutoff := p.opts.Now().Add(-24 * time.Hour)
	for _, d := range p.deleted {
		if d.At.After(cutoff) {
			out.Deleted = append(out.Deleted, d)
		}
	}
	return out, nil
}

// SetLabels implements provider.Provider.
func (p *Provider) SetLabels(ctx context.Context, ref provider.Ref, requestID string, set, expect map[string]*string) (provider.Instance, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.enterLocked(ctx, OpSetLabels, ref); err != nil {
		return provider.Instance{}, err
	}
	for _, labels := range []map[string]*string{set, expect} {
		if err := provider.CheckLabels(labels); err != nil {
			return provider.Instance{}, errorf(provider.ErrInvalid, OpSetLabels, "RESERVED_LABEL", "%v", err)
		}
	}
	if k, ok, err := p.keptLocked(requestID, OpSetLabels, ref); ok || err != nil {
		return k.inst, err
	}
	m, err := p.liveLocked(OpSetLabels, ref)
	if err != nil {
		return provider.Instance{}, err
	}
	for k, v := range expect {
		cur, has := m.inst.Labels[k]
		if (v == nil && has) || (v != nil && (!has || cur != *v)) {
			return provider.Instance{}, errorf(provider.ErrLabelTaken, OpSetLabels, "LABEL_TAKEN", "%s: label %s is not as expected", ref, k)
		}
	}
	merged := maps.Clone(m.inst.Labels)
	if merged == nil {
		merged = map[string]string{}
	}
	for k, v := range set {
		if v == nil {
			delete(merged, k)
		} else {
			merged[k] = *v
		}
	}
	if len(merged) > provider.MaxLabels {
		return provider.Instance{}, errorf(provider.ErrInvalid, OpSetLabels, "INVALID", "a machine takes at most %d labels", provider.MaxLabels)
	}
	if !maps.Equal(merged, m.inst.Labels) {
		p.labelWrites++
	}
	m.inst.Labels = merged
	_, inst, err := p.answerLocked(OpSetLabels, requestID, ref, kept{inst: p.viewLocked(m)})
	return inst, err
}

// Extend implements provider.Provider.
func (p *Provider) Extend(ctx context.Context, ref provider.Ref, requestID string, by time.Duration) (provider.Instance, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.enterLocked(ctx, OpExtend, ref); err != nil {
		return provider.Instance{}, err
	}
	if !provider.ValidExtend(by) {
		return provider.Instance{}, errorf(provider.ErrInvalid, OpExtend, "INVALID", "extend by whole hours, 1 to 720")
	}
	if k, ok, err := p.keptLocked(requestID, OpExtend, ref); ok || err != nil {
		return k.inst, err
	}
	m, err := p.liveLocked(OpExtend, ref)
	if err != nil {
		return provider.Instance{}, err
	}
	now := p.opts.Now()
	switch {
	case !now.Before(m.inst.Lease.ExpiresAt):
		return provider.Instance{}, errorf(provider.ErrLease, OpExtend, "LEASE_ENDED", "%s: the lease has ended", ref)
	case m.inst.Client != "" || !p.catalog.Policy.PersonExtend:
		return provider.Instance{}, errorf(provider.ErrLease, OpExtend, "EXTEND_NOT_ALLOWED", "%s cannot be extended", ref)
	case m.inst.Lease.ExpiresAt.Add(by).After(m.inst.Lease.MaxExpiresAt):
		return provider.Instance{}, errorf(provider.ErrLease, OpExtend, "LEASE_AT_CAP", "%s: the lease would pass its cap", ref)
	}
	m.inst.Lease.ExpiresAt = m.inst.Lease.ExpiresAt.Add(by)
	m.inst.Lease = p.leaseLocked(m, now)
	_, inst, err := p.answerLocked(OpExtend, requestID, ref, kept{inst: p.viewLocked(m)})
	return inst, err
}

// Usage implements provider.Provider.
func (p *Provider) Usage(ctx context.Context, user string) (provider.Usage, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.enterLocked(ctx, OpUsage, ""); err != nil {
		return provider.Usage{}, err
	}
	count := func(match func(*machine) bool, limit int) provider.Counts {
		c := provider.Counts{Max: limit}
		for _, m := range p.machines {
			if !match(m) {
				continue
			}
			c.Machines++
			switch m.inst.State {
			case provider.StateStarting, provider.StateRunning:
				c.Running++
			case provider.StateStopping, provider.StateStopped:
				c.Stopped++
			}
		}
		return c
	}
	u := provider.Usage{Org: count(func(*machine) bool { return true }, p.catalog.Limits.OrgMax)}
	if user != "" {
		pc := count(func(m *machine) bool { return m.inst.User == user }, p.catalog.Limits.PersonMax)
		u.Person = &pc
	}
	return u, nil
}

// Ingress implements provider.Provider.
func (p *Provider) Ingress(ctx context.Context) (provider.Ingress, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.enterLocked(ctx, OpIngress, ""); err != nil {
		return provider.Ingress{}, err
	}
	return cloneIngress(p.ingress), nil
}

// SetIngress implements provider.Provider.
func (p *Provider) SetIngress(ctx context.Context, requestID string, sources []netip.Prefix) (provider.Ingress, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.enterLocked(ctx, OpSetIngress, ""); err != nil {
		return provider.Ingress{}, err
	}
	if k, ok, err := p.keptLocked(requestID, OpSetIngress, ""); ok || err != nil {
		return k.ingress, err
	}
	if !p.catalog.IngressWritable {
		return provider.Ingress{}, errorf(provider.ErrIngress, OpSetIngress, "INGRESS_NOT_WRITABLE", "the ingress is managed elsewhere")
	}
	var applied []string
	var canon []netip.Prefix
	for _, s := range sources {
		if !s.IsValid() || s.Masked() != s {
			return provider.Ingress{}, errorf(provider.ErrInvalid, OpSetIngress, "INVALID", "source %s is not canonical", s)
		}
		if (s.Addr().Is4() && s.Bits() < 24) || (s.Addr().Is6() && s.Bits() < 48) {
			return provider.Ingress{}, errorf(provider.ErrIngress, OpSetIngress, "INGRESS_TOO_WIDE", "source %s is wider than allowed", s)
		}
		canon = append(canon, s)
		applied = append(applied, s.String())
	}
	p.ingress.Sources = canon
	p.ingress.AppliedAt = p.stamp()
	p.ingressWrites = append(p.ingressWrites, applied)
	k := kept{ingress: cloneIngress(p.ingress)}
	if _, _, err := p.answerLocked(OpSetIngress, requestID, "", k); err != nil {
		return provider.Ingress{}, err
	}
	return k.ingress, nil
}

// --- internals ---

// enterLocked records the call, then returns the context's error or an
// injected failure, if there is one.
func (p *Provider) enterLocked(ctx context.Context, op Op, ref provider.Ref) error {
	p.calls = append(p.calls, Call{Op: op, Ref: ref})
	if ctx.Err() != nil {
		return ctxErr(ctx, op)
	}
	if queue := p.failCall[op]; len(queue) > 0 {
		p.failCall[op] = queue[1:]
		return queue[0]
	}
	return nil
}

// keptLocked returns the kept answer of a request id used before for the
// same call, ok false when it is new; an id used for another call is
// ErrInvalid.
func (p *Provider) keptLocked(requestID string, op Op, ref provider.Ref) (kept, bool, error) {
	if requestID == "" {
		return kept{}, false, nil
	}
	k, ok := p.requests[requestID]
	if !ok {
		return kept{}, false, nil
	}
	if k.call != string(op)+" "+string(ref) {
		return kept{}, false, errorf(provider.ErrInvalid, op, "INVALID", "request id %s was used for another call", requestID)
	}
	return k, true, nil
}

// answerLocked keeps a successful answer under its request id and returns
// it, or, when a lost reply was asked for, ErrUnavailable.
func (p *Provider) answerLocked(op Op, requestID string, ref provider.Ref, k kept) (provider.Pending, provider.Instance, error) {
	if requestID != "" {
		k.call = string(op) + " " + string(ref)
		p.requests[requestID] = k
	}
	if p.loseReply[op] > 0 {
		p.loseReply[op]--
		return provider.Pending{}, provider.Instance{}, errorf(provider.ErrUnavailable, op, "", "the reply was lost")
	}
	return k.pending, k.inst, nil
}

// liveLocked returns the machine for a call on it: ErrNotFound when it is
// absent, ErrDeleting while it is being deleted.
func (p *Provider) liveLocked(op Op, ref provider.Ref) (*machine, error) {
	m, ok := p.machines[ref]
	if !ok {
		return nil, errorf(provider.ErrNotFound, op, "NOT_FOUND", "%s", ref)
	}
	if m.inst.State == provider.StateDeleting {
		return nil, errorf(provider.ErrDeleting, op, "DELETING", "%s is being deleted", ref)
	}
	return m, nil
}

func (p *Provider) defaultHostKey() string {
	switch {
	case p.opts.NoHostKey:
		return ""
	case p.opts.HostKey != "":
		return p.opts.HostKey
	}
	return DefaultHostKey
}

// giveAccessLocked gives a running machine its access option at a fresh
// address, when its guest has published a host key.
func (p *Provider) giveAccessLocked(m *machine) {
	if m.hostKey == "" {
		m.inst.Access = nil
		return
	}
	p.nextAddr++
	host := fmt.Sprintf("203.0.113.%d", 1+(p.nextAddr-1)%254)
	m.inst.Access = []provider.Access{{Kind: provider.AccessSSH, Host: host, Port: 22, User: "shed-link", HostKey: m.hostKey}}
	m.inst.IgnoredAccess = 0
}

// leaseLocked returns m's lease with extendable and the presets computed
// now.
func (p *Provider) leaseLocked(m *machine, now time.Time) provider.Lease {
	l := m.inst.Lease
	l.PresetsH = nil
	l.Extendable = false
	if m.inst.Client == "" {
		l.PresetsH = slices.Clone(p.catalog.Policy.PresetsH)
		l.Extendable = p.catalog.Policy.PersonExtend && m.inst.State != provider.StateDeleting &&
			now.Before(l.ExpiresAt) && !l.ExpiresAt.Add(time.Hour).After(l.MaxExpiresAt)
	}
	return l
}

func (p *Provider) viewLocked(m *machine) provider.Instance {
	v := m.inst.Clone()
	v.Lease = p.leaseLocked(m, p.opts.Now())
	return v
}

// removeLocked deletes a machine with a cause, ending its work in progress
// with err.
func (p *Provider) removeLocked(m *machine, cause string, err error) {
	delete(p.machines, m.inst.Ref)
	p.deleted = append(p.deleted, provider.Deletion{Ref: m.inst.Ref, At: p.stamp(), Cause: cause})
	if m.op != nil {
		op := m.op
		m.op = nil
		if op.kind == OpDestroy {
			op.gone = true
		} else {
			op.err = err
		}
		close(op.done)
	}
}

// beginLocked registers a new operation on a machine and arranges for it to
// finish: now, or on Complete when Manual. prev is the state the machine
// returns to if the operation fails.
func (p *Provider) beginLocked(m *machine, kind Op, prev provider.State) *operation {
	p.nextOp++
	op := &operation{
		id:   fmt.Sprintf("fake-op-%d", p.nextOp),
		kind: kind,
		ref:  m.inst.Ref,
		prev: prev,
		done: make(chan struct{}),
	}
	if queue := p.failOp[kind]; len(queue) > 0 {
		p.failOp[kind] = queue[1:]
		op.failWith = queue[0]
	}
	m.op = op
	p.ops[op.id] = op
	if !p.opts.Manual {
		p.finishLocked(op)
	}
	return op
}

// finishLocked applies an operation's result. It does nothing when the
// operation already finished or its machine was deleted.
func (p *Provider) finishLocked(op *operation) {
	m, ok := p.machines[op.ref]
	if !ok || m.op != op {
		return
	}
	m.op = nil
	defer close(op.done)

	if op.failWith != nil {
		op.err = op.failWith
		if op.kind == OpCreate {
			// A create the cloud refused is no deletion.
			delete(p.machines, op.ref)
			return
		}
		m.inst.State = op.prev
		if op.prev == provider.StateRunning {
			p.giveAccessLocked(m)
		}
		return
	}
	switch op.kind {
	case OpCreate, OpStart:
		m.inst.State = provider.StateRunning
		m.inst.StartedAt = p.stamp()
		m.inst.StoppedAt = time.Time{}
		p.giveAccessLocked(m)
	case OpStop:
		p.markStoppedLocked(m)
	case OpDestroy:
		op.gone = true
		delete(p.machines, op.ref)
		p.deleted = append(p.deleted, provider.Deletion{Ref: op.ref, At: p.stamp(), Cause: provider.CauseServer})
	}
}

// stamp is the clock's time as the service writes a timestamp: UTC, whole
// seconds.
func (p *Provider) stamp() time.Time { return p.opts.Now().UTC().Truncate(time.Second) }

func (p *Provider) markStoppedLocked(m *machine) {
	m.inst.State = provider.StateStopped
	m.inst.Access = nil
	m.inst.StoppedAt = p.stamp()
}

// pendingFor describes a machine's work in flight, or none, for an
// idempotent repeat of a call.
func pendingFor(m *machine, want provider.State) provider.Pending {
	pend := provider.Pending{Ref: m.inst.Ref, Want: want}
	if m.op != nil {
		pend.Operation = m.op.id
	}
	return pend
}

func cloneIngress(in provider.Ingress) provider.Ingress {
	in.Sources = slices.Clone(in.Sources)
	return in
}

// ctxErr is a call whose context ended before the fake took it: like a
// request the service client never wrote, it was never delivered and did
// nothing.
func ctxErr(ctx context.Context, op Op) error {
	return &provider.Error{Kind: provider.ErrUnavailable, Op: string(op), Detail: "the call's context ended before it was sent: " + ctx.Err().Error(),
		Err: provider.ErrNotDelivered}
}
