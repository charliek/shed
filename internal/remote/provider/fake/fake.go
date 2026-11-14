// Package fake is an in-memory provider.Provider for tests and for the
// development driver (remote.provider: fake). It keeps instances in a map,
// runs no VM, and follows the provider contract strictly: wherever the
// contract says a provider may return provider.ErrBusy, the fake does.
//
// Knobs:
//   - Options.*Delay: how long each kind of operation takes to finish.
//   - Options.Manual: operations finish only when the test calls Complete.
//   - FailNext: the next call of a kind returns an error and changes nothing.
//   - FailOperation: the next operation of a kind is accepted, then fails, so
//     Wait returns the error (a refusal that arrives late).
//   - ExternalStop, ExternalDelete: the cloud changes an instance behind the
//     server's back (a run limit, a console delete).
//   - Seed: put an instance in the cloud directly, for adoption tests.
//   - PublishHostKey: what the guest's boot unit would publish.
//   - ResolveImage: exact names resolve to themselves, families to a fixed name.
//   - SetFirewallRules, EditEnvRule, AddFirewallRule, SetDefaultNetwork: the
//     cloud's firewall as someone else left it. A fake with an environment
//     starts with the one rule the setup script makes.
//   - Calls, Count: what was called, in order.
package fake

import (
	"context"
	"fmt"
	"maps"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/charliek/shed/internal/remote/provider"
)

// Op names a provider call, for FailNext, FailOperation and Calls.
type Op string

const (
	OpCreate       Op = "create"
	OpStart        Op = "start"
	OpStop         Op = "stop"
	OpDestroy      Op = "destroy"
	OpWait         Op = "wait"
	OpGet          Op = "get"
	OpList         Op = "list"
	OpSetLabels    Op = "set_labels"
	OpSetMetadata  Op = "set_metadata"
	OpGuestHostKey Op = "guest_host_key"
	OpResolveImage Op = "resolve_image"

	OpEnvFirewallRule    Op = "env_firewall_rule"
	OpNetworkIngress     Op = "network_ingress"
	OpSetFirewallSources Op = "set_firewall_sources"
)

// Zone is the Instance.Zone the fake reports.
const Zone = "fake-zone-a"

// DefaultNetwork is the network the fake's instances are on unless
// Options.Network names another.
const DefaultNetwork = "fake-net"

// DefaultFirewallSource is the source range of the environment rule a fake
// starts with unless Options.FirewallSources says otherwise. It is a
// documentation address.
const DefaultFirewallSource = "192.0.2.1/32"

// Options configures a fake provider. The zero value is usable: operations
// finish at once and no environment check is made.
type Options struct {
	// Env is the environment the provider serves. When set, a Create whose
	// labels name another environment is ErrInvalid, and mutating calls on
	// another environment's instance are ErrForeign.
	Env string
	// Now is the clock for timestamps. Nil means time.Now.
	Now func() time.Time
	// CreateDelay, StartDelay, StopDelay and DestroyDelay are how long an
	// accepted operation takes to finish, in real time. Zero finishes the
	// operation before the accepting call returns.
	CreateDelay  time.Duration
	StartDelay   time.Duration
	StopDelay    time.Duration
	DestroyDelay time.Duration
	// Manual makes every operation wait for Complete instead of a delay.
	Manual bool
	// Capabilities overrides the default (run limit enforced, address not
	// stable), which matches GCE.
	Capabilities *provider.Capabilities
	// Network is the network's name. Empty means DefaultNetwork.
	Network string
	// FirewallSources are the source ranges of the environment rule the
	// fake starts with (when Env is set). Empty means
	// DefaultFirewallSource.
	FirewallSources []string
}

// Call records one provider call.
type Call struct {
	Op Op
	// Ref is the instance the call named; for Create it is the new Ref.
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
}

type instance struct {
	inst    provider.Instance
	op      *operation // in flight; nil when settled
	hostKey string
}

// Provider is the fake. Create one with New.
type Provider struct {
	opts Options

	mu        sync.Mutex
	instances map[provider.Ref]*instance
	ops       map[string]*operation
	failCall  map[Op][]error
	failOp    map[Op][]error
	calls     []Call
	nextOp    int
	nextAddr  int

	// rules is the whole cloud firewall: every rule on every network.
	rules      []provider.FirewallRule
	defaultNet bool
	// patches are the source lists SetEnvFirewallSources applied, in order.
	patches [][]string
}

var (
	_ provider.Provider      = (*Provider)(nil)
	_ provider.HostKeyReader = (*Provider)(nil)
	_ provider.ImageResolver = (*Provider)(nil)

	_ provider.FirewallSourceSetter = (*Provider)(nil)
)

// New returns an empty fake provider.
func New(opts Options) *Provider {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Network == "" {
		opts.Network = DefaultNetwork
	}
	p := &Provider{
		opts:      opts,
		instances: make(map[provider.Ref]*instance),
		ops:       make(map[string]*operation),
		failCall:  make(map[Op][]error),
		failOp:    make(map[Op][]error),
	}
	if opts.Env != "" {
		sources := opts.FirewallSources
		if len(sources) == 0 {
			sources = []string{DefaultFirewallSource}
		}
		p.rules = []provider.FirewallRule{EnvRule(opts.Env, opts.Network, sources...)}
	}
	return p
}

// EnvRule returns an environment's firewall rule exactly as the setup script
// makes it: ingress, allow tcp:22, to the environment's network tag, from the
// given sources.
func EnvRule(env, network string, sources ...string) provider.FirewallRule {
	return provider.FirewallRule{
		Name:         provider.FirewallRuleName(env),
		Network:      network,
		Direction:    provider.DirectionIngress,
		Priority:     1000,
		SourceRanges: append([]string(nil), sources...),
		TargetTags:   []string{provider.NetworkTag(env)},
		Allowed:      []provider.FirewallPorts{{Protocol: "tcp", Ports: []string{"22"}}},
	}
}

// SetFirewallRules replaces the cloud's whole firewall. A rule whose Network
// is empty is put on the fake's own network.
func (p *Provider) SetFirewallRules(rules ...provider.FirewallRule) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.rules = nil
	for _, r := range rules {
		p.rules = append(p.rules, p.ownedRule(r))
	}
}

// AddFirewallRule adds one rule to the cloud's firewall, replacing a rule of
// the same name. A rule whose Network is empty is put on the fake's network.
func (p *Provider) AddFirewallRule(rule provider.FirewallRule) {
	p.mu.Lock()
	defer p.mu.Unlock()
	rule = p.ownedRule(rule)
	for i := range p.rules {
		if p.rules[i].Name == rule.Name {
			p.rules[i] = rule
			return
		}
	}
	p.rules = append(p.rules, rule)
}

// RemoveFirewallRule deletes a rule by name; a missing rule is ignored.
func (p *Provider) RemoveFirewallRule(name string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for i := range p.rules {
		if p.rules[i].Name == name {
			p.rules = append(p.rules[:i], p.rules[i+1:]...)
			return
		}
	}
}

// EditEnvRule changes the environment's rule in place, as a person with the
// console would. It reports false when the rule does not exist.
func (p *Provider) EditEnvRule(edit func(*provider.FirewallRule)) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	name := provider.FirewallRuleName(p.opts.Env)
	for i := range p.rules {
		if p.rules[i].Name == name {
			edit(&p.rules[i])
			return true
		}
	}
	return false
}

// SetDefaultNetwork makes the fake report its network as the cloud's default
// one.
func (p *Provider) SetDefaultNetwork(isDefault bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.defaultNet = isDefault
}

// FirewallPatches returns every source list SetEnvFirewallSources applied, in
// order.
func (p *Provider) FirewallPatches() [][]string {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([][]string, len(p.patches))
	for i, ranges := range p.patches {
		out[i] = append([]string(nil), ranges...)
	}
	return out
}

func (p *Provider) ownedRule(r provider.FirewallRule) provider.FirewallRule {
	r = cloneRule(r)
	if r.Network == "" {
		r.Network = p.opts.Network
	}
	if r.Direction == "" {
		r.Direction = provider.DirectionIngress
	}
	return r
}

func cloneRule(r provider.FirewallRule) provider.FirewallRule {
	r.SourceRanges = append([]string(nil), r.SourceRanges...)
	r.SourceTags = append([]string(nil), r.SourceTags...)
	r.SourceServiceAccounts = append([]string(nil), r.SourceServiceAccounts...)
	r.TargetTags = append([]string(nil), r.TargetTags...)
	r.TargetServiceAccounts = append([]string(nil), r.TargetServiceAccounts...)
	r.DestinationRanges = append([]string(nil), r.DestinationRanges...)
	r.Allowed = append([]provider.FirewallPorts(nil), r.Allowed...)
	r.Denied = append([]provider.FirewallPorts(nil), r.Denied...)
	return r
}

// EnvFirewallRule returns the environment's rule.
func (p *Provider) EnvFirewallRule(ctx context.Context) (provider.FirewallRule, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.enterLocked(ctx, OpEnvFirewallRule, ""); err != nil {
		return provider.FirewallRule{}, err
	}
	name := provider.FirewallRuleName(p.opts.Env)
	for _, r := range p.rules {
		if r.Name == name {
			return cloneRule(r), nil
		}
	}
	return provider.FirewallRule{}, provider.Errorf(provider.ErrNotFound, string(OpEnvFirewallRule), "no firewall rule named %s", name)
}

// NetworkIngress returns the fake's network and the ingress rules on it.
func (p *Provider) NetworkIngress(ctx context.Context) (provider.NetworkIngress, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.enterLocked(ctx, OpNetworkIngress, ""); err != nil {
		return provider.NetworkIngress{}, err
	}
	out := provider.NetworkIngress{Network: p.opts.Network, Default: p.defaultNet}
	for _, r := range p.rules {
		if r.Network == p.opts.Network && r.Direction == provider.DirectionIngress {
			out.Rules = append(out.Rules, cloneRule(r))
		}
	}
	provider.SortFirewallRules(out.Rules)
	return out, nil
}

// SetEnvFirewallSources replaces the source ranges of the environment's rule.
func (p *Provider) SetEnvFirewallSources(ctx context.Context, ranges []string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.enterLocked(ctx, OpSetFirewallSources, ""); err != nil {
		return err
	}
	if err := provider.CheckFirewallSources(string(OpSetFirewallSources), ranges); err != nil {
		return err
	}
	name := provider.FirewallRuleName(p.opts.Env)
	for i := range p.rules {
		if p.rules[i].Name == name {
			p.rules[i].SourceRanges = append([]string(nil), ranges...)
			p.patches = append(p.patches, append([]string(nil), ranges...))
			return nil
		}
	}
	return provider.Errorf(provider.ErrNotFound, string(OpSetFirewallSources), "no firewall rule named %s", name)
}

// FailNext makes the next call of the given kind return err without changing
// anything. Calls queue: two FailNext calls fail the next two.
func (p *Provider) FailNext(op Op, err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.failCall[op] = append(p.failCall[op], err)
}

// FailOperation makes the next accepted operation of the given kind
// (OpCreate, OpStart, OpStop or OpDestroy) fail when it finishes: the
// accepting call succeeds and Wait returns err. A failed create removes the
// instance; a failed start, stop or destroy puts it back in the state it was
// in.
func (p *Provider) FailOperation(op Op, err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.failOp[op] = append(p.failOp[op], err)
}

// Complete finishes the operation in flight on an instance. It is how a
// Manual provider moves forward, and it also cuts a delay short. It reports
// whether there was an operation to finish.
func (p *Provider) Complete(ref provider.Ref) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	in, ok := p.instances[ref]
	if !ok || in.op == nil {
		return false
	}
	p.finishLocked(in.op)
	return true
}

// ExternalStop stops a settled, running instance at once, as a run limit
// would. It records no call.
func (p *Provider) ExternalStop(ref provider.Ref) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	in, ok := p.instances[ref]
	switch {
	case !ok:
		return provider.Errorf(provider.ErrNotFound, "external_stop", "%s", ref)
	case in.op != nil || in.inst.State != provider.StateRunning:
		return provider.Errorf(provider.ErrBusy, "external_stop", "%s is %s", ref, in.inst.State)
	}
	p.markStoppedLocked(in)
	return nil
}

// ExternalDelete removes an instance at once, whatever it is doing. A Wait in
// progress on it returns provider.ErrNotFound.
func (p *Provider) ExternalDelete(ref provider.Ref) {
	p.mu.Lock()
	defer p.mu.Unlock()
	in, ok := p.instances[ref]
	if !ok {
		return
	}
	delete(p.instances, ref)
	if in.op != nil {
		in.op.err = provider.Errorf(provider.ErrNotFound, string(OpWait), "%s was deleted", ref)
		close(in.op.done)
		in.op = nil
	}
}

// Seed adds a settled instance as if another server had created it. State
// must be running or stopped. A missing Ref, Zone or address is filled in.
// It returns the instance's Ref.
func (p *Provider) Seed(inst provider.Instance) provider.Ref {
	p.mu.Lock()
	defer p.mu.Unlock()
	if inst.Ref == "" {
		inst.Ref = refFor(inst.Name)
	}
	if inst.Zone == "" {
		inst.Zone = Zone
	}
	if inst.CreatedAt.IsZero() {
		inst.CreatedAt = p.opts.Now()
	}
	inst.Labels = maps.Clone(inst.Labels)
	inst.Metadata = maps.Clone(inst.Metadata)
	if inst.State == provider.StateRunning && inst.Address == "" {
		inst.Address = p.newAddrLocked()
	}
	p.instances[inst.Ref] = &instance{inst: inst}
	return inst.Ref
}

// PublishHostKey sets the host key GuestHostKey returns for an instance, as
// the guest's boot unit would.
func (p *Provider) PublishHostKey(ref provider.Ref, key string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if in, ok := p.instances[ref]; ok {
		in.hostKey = key
	}
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

// Name implements provider.Provider.
func (p *Provider) Name() string { return provider.NameFake }

// Capabilities implements provider.Provider.
func (p *Provider) Capabilities() provider.Capabilities {
	if p.opts.Capabilities != nil {
		return *p.opts.Capabilities
	}
	return provider.Capabilities{EnforcesRunLimit: true, StableAddress: false}
}

// Create implements provider.Provider.
func (p *Provider) Create(ctx context.Context, spec provider.Spec) (provider.Pending, error) {
	ref := refFor(spec.Name)
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.enterLocked(ctx, OpCreate, ref); err != nil {
		return provider.Pending{}, err
	}
	if err := spec.Validate(); err != nil {
		return provider.Pending{}, err
	}
	if p.opts.Env != "" && spec.Labels[provider.LabelEnv] != p.opts.Env {
		return provider.Pending{}, provider.Errorf(provider.ErrInvalid, string(OpCreate),
			"spec is for environment %q, this provider serves %q", spec.Labels[provider.LabelEnv], p.opts.Env)
	}
	if in, ok := p.instances[ref]; ok {
		if in.inst.Labels[provider.LabelID] != spec.Labels[provider.LabelID] {
			return provider.Pending{}, provider.Errorf(provider.ErrExists, string(OpCreate), "%s", spec.Name)
		}
		return pendingFor(in, provider.StateRunning), nil
	}
	now := p.opts.Now()
	in := &instance{inst: provider.Instance{
		Ref:             ref,
		Name:            spec.Name,
		State:           provider.StateStarting,
		Labels:          maps.Clone(spec.Labels),
		Metadata:        maps.Clone(spec.Metadata),
		InternalAddress: fmt.Sprintf("10.128.0.%d", 2+len(p.instances)%250),
		Zone:            Zone,
		MachineType:     spec.Size.MachineType,
		CPUs:            spec.Size.CPUs,
		MemoryMB:        spec.Size.MemoryMB,
		Arch:            spec.Size.Arch,
		DiskGB:          spec.DiskGB,
		Image:           spec.Image,
		MaxRun:          spec.MaxRun,
		CreatedAt:       now,
	}}
	p.instances[ref] = in
	op := p.beginLocked(in, OpCreate, provider.StateAbsent, p.opts.CreateDelay)
	return provider.Pending{Ref: ref, Operation: op.id, Want: provider.StateRunning}, nil
}

// Start implements provider.Provider.
func (p *Provider) Start(ctx context.Context, ref provider.Ref, opts provider.StartOptions) (provider.Pending, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.enterLocked(ctx, OpStart, ref); err != nil {
		return provider.Pending{}, err
	}
	in, err := p.mineLocked(OpStart, ref)
	if err != nil {
		return provider.Pending{}, err
	}
	switch in.inst.State {
	case provider.StateRunning, provider.StateStarting:
		return pendingFor(in, provider.StateRunning), nil
	case provider.StateStopped:
		if opts.MaxRun > 0 {
			in.inst.MaxRun = opts.MaxRun
		}
		in.inst.State = provider.StateStarting
		op := p.beginLocked(in, OpStart, provider.StateStopped, p.opts.StartDelay)
		return provider.Pending{Ref: ref, Operation: op.id, Want: provider.StateRunning}, nil
	default:
		return provider.Pending{}, provider.Errorf(provider.ErrBusy, string(OpStart), "%s is %s", ref, in.inst.State)
	}
}

// Stop implements provider.Provider.
func (p *Provider) Stop(ctx context.Context, ref provider.Ref) (provider.Pending, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.enterLocked(ctx, OpStop, ref); err != nil {
		return provider.Pending{}, err
	}
	in, err := p.mineLocked(OpStop, ref)
	if err != nil {
		return provider.Pending{}, err
	}
	switch {
	case in.inst.State == provider.StateStopped:
		return pendingFor(in, provider.StateStopped), nil
	case in.op != nil && in.op.kind == OpStop:
		return pendingFor(in, provider.StateStopped), nil
	case in.inst.State == provider.StateRunning:
		in.inst.State = provider.StateStopping
		op := p.beginLocked(in, OpStop, provider.StateRunning, p.opts.StopDelay)
		return provider.Pending{Ref: ref, Operation: op.id, Want: provider.StateStopped}, nil
	default:
		return provider.Pending{}, provider.Errorf(provider.ErrBusy, string(OpStop), "%s is %s", ref, in.inst.State)
	}
}

// Destroy implements provider.Provider.
func (p *Provider) Destroy(ctx context.Context, ref provider.Ref) (provider.Pending, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.enterLocked(ctx, OpDestroy, ref); err != nil {
		return provider.Pending{}, err
	}
	in, ok := p.instances[ref]
	if !ok {
		return provider.Pending{Ref: ref, Want: provider.StateAbsent}, nil
	}
	if err := p.checkEnvLocked(OpDestroy, in); err != nil {
		return provider.Pending{}, err
	}
	switch {
	case in.op != nil && in.op.kind == OpDestroy:
		return pendingFor(in, provider.StateAbsent), nil
	case in.op != nil:
		return provider.Pending{}, provider.Errorf(provider.ErrBusy, string(OpDestroy), "%s is %s", ref, in.inst.State)
	}
	prev := in.inst.State
	in.inst.State = provider.StateStopping
	in.inst.Address = ""
	op := p.beginLocked(in, OpDestroy, prev, p.opts.DestroyDelay)
	return provider.Pending{Ref: ref, Operation: op.id, Want: provider.StateAbsent}, nil
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

	// Then follow the instance until it is settled.
	for {
		p.mu.Lock()
		in, ok := p.instances[pend.Ref]
		if !ok {
			p.mu.Unlock()
			if pend.Want == provider.StateAbsent {
				return provider.Instance{}, nil
			}
			return provider.Instance{}, provider.Errorf(provider.ErrNotFound, string(OpWait), "%s", pend.Ref)
		}
		if in.op == nil {
			view := clone(in.inst)
			p.mu.Unlock()
			if pend.Want == "" || view.State == pend.Want {
				return view, nil
			}
			return view, provider.Errorf(provider.ErrUnexpectedState, string(OpWait),
				"%s is %s, want %s", pend.Ref, view.State, pend.Want)
		}
		done := in.op.done
		p.mu.Unlock()
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
	in, ok := p.instances[ref]
	if !ok {
		return provider.Instance{}, provider.Errorf(provider.ErrNotFound, string(OpGet), "%s", ref)
	}
	return clone(in.inst), nil
}

// List implements provider.Provider.
func (p *Provider) List(ctx context.Context, sel provider.Selector) ([]provider.Instance, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.enterLocked(ctx, OpList, ""); err != nil {
		return nil, err
	}
	out := make([]provider.Instance, 0, len(p.instances))
	for _, in := range p.instances {
		if sel.Env != "" && in.inst.Labels[provider.LabelEnv] != sel.Env {
			continue
		}
		out = append(out, clone(in.inst))
	}
	sort.Slice(out, func(a, b int) bool { return out[a].Name < out[b].Name })
	return out, nil
}

// SetLabels implements provider.Provider.
func (p *Provider) SetLabels(ctx context.Context, ref provider.Ref, labels map[string]string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.enterLocked(ctx, OpSetLabels, ref); err != nil {
		return err
	}
	in, err := p.mineLocked(OpSetLabels, ref)
	if err != nil {
		return err
	}
	for k, v := range labels {
		switch k {
		case provider.LabelSchema, provider.LabelEnv, provider.LabelID:
			if in.inst.Labels[k] != v {
				return provider.Errorf(provider.ErrInvalid, string(OpSetLabels), "label %s cannot be changed", k)
			}
		}
		if !provider.ValidLabelValue(v) {
			return provider.Errorf(provider.ErrInvalid, string(OpSetLabels), "label %s has an invalid value %q", k, v)
		}
	}
	if in.inst.Labels == nil {
		in.inst.Labels = make(map[string]string)
	}
	maps.Copy(in.inst.Labels, labels)
	return nil
}

// SetMetadata implements provider.Provider.
func (p *Provider) SetMetadata(ctx context.Context, ref provider.Ref, metadata map[string]string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.enterLocked(ctx, OpSetMetadata, ref); err != nil {
		return err
	}
	in, err := p.mineLocked(OpSetMetadata, ref)
	if err != nil {
		return err
	}
	for k := range metadata {
		if !strings.HasPrefix(k, "shed-") {
			return provider.Errorf(provider.ErrInvalid, string(OpSetMetadata), "metadata key %q does not begin \"shed-\"", k)
		}
	}
	if in.inst.Metadata == nil {
		in.inst.Metadata = make(map[string]string)
	}
	for k, v := range metadata {
		if v == "" {
			delete(in.inst.Metadata, k)
			continue
		}
		in.inst.Metadata[k] = v
	}
	return nil
}

// GuestHostKey implements provider.HostKeyReader.
func (p *Provider) GuestHostKey(ctx context.Context, ref provider.Ref) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.enterLocked(ctx, OpGuestHostKey, ref); err != nil {
		return "", err
	}
	in, ok := p.instances[ref]
	if !ok {
		return "", provider.Errorf(provider.ErrNotFound, string(OpGuestHostKey), "%s", ref)
	}
	if in.hostKey == "" {
		return "", provider.Errorf(provider.ErrNotFound, string(OpGuestHostKey), "%s has published no host key", ref)
	}
	return in.hostKey, nil
}

// ResolveImage implements provider.ImageResolver. An exact name resolves to
// itself; "family/<name>" resolves to "<name>-fake0001". No image is ever
// missing unless a failure is injected with FailNext(OpResolveImage, ...).
func (p *Provider) ResolveImage(ctx context.Context, image string) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.enterLocked(ctx, OpResolveImage, ""); err != nil {
		return "", err
	}
	if image == "" {
		return "", provider.Errorf(provider.ErrInvalid, string(OpResolveImage), "image is empty")
	}
	if family, ok := strings.CutPrefix(image, provider.ImagePrefixFamily); ok {
		return family + "-fake0001", nil
	}
	return image, nil
}

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

// mineLocked returns the instance for a mutating call: ErrNotFound when it is
// absent, ErrForeign when it belongs to another environment.
func (p *Provider) mineLocked(op Op, ref provider.Ref) (*instance, error) {
	in, ok := p.instances[ref]
	if !ok {
		return nil, provider.Errorf(provider.ErrNotFound, string(op), "%s", ref)
	}
	if err := p.checkEnvLocked(op, in); err != nil {
		return nil, err
	}
	return in, nil
}

func (p *Provider) checkEnvLocked(op Op, in *instance) error {
	if p.opts.Env == "" {
		return nil
	}
	if env := in.inst.Labels[provider.LabelEnv]; env != p.opts.Env {
		return provider.Errorf(provider.ErrForeign, string(op), "%s has %s=%q", in.inst.Ref, provider.LabelEnv, env)
	}
	return nil
}

// beginLocked registers a new operation on an instance and arranges for it to
// finish: now, after a delay, or on Complete. prev is the state the instance
// returns to if the operation fails.
func (p *Provider) beginLocked(in *instance, kind Op, prev provider.State, delay time.Duration) *operation {
	p.nextOp++
	op := &operation{
		id:   fmt.Sprintf("fake-op-%d", p.nextOp),
		kind: kind,
		ref:  in.inst.Ref,
		prev: prev,
		done: make(chan struct{}),
	}
	if queue := p.failOp[kind]; len(queue) > 0 {
		p.failOp[kind] = queue[1:]
		op.failWith = queue[0]
	}
	in.op = op
	p.ops[op.id] = op
	switch {
	case p.opts.Manual:
	case delay <= 0:
		p.finishLocked(op)
	default:
		time.AfterFunc(delay, func() {
			p.mu.Lock()
			defer p.mu.Unlock()
			p.finishLocked(op)
		})
	}
	return op
}

// finishLocked applies an operation's result. It does nothing when the
// operation already finished or its instance was deleted from outside.
func (p *Provider) finishLocked(op *operation) {
	in, ok := p.instances[op.ref]
	if !ok || in.op != op {
		return
	}
	in.op = nil
	defer close(op.done)

	if op.failWith != nil {
		op.err = op.failWith
		if op.kind == OpCreate {
			delete(p.instances, op.ref)
			return
		}
		in.inst.State = op.prev
		if op.prev == provider.StateRunning && in.inst.Address == "" {
			in.inst.Address = p.newAddrLocked()
		}
		return
	}
	switch op.kind {
	case OpCreate, OpStart:
		now := p.opts.Now()
		in.inst.State = provider.StateRunning
		in.inst.Address = p.newAddrLocked()
		in.inst.StartedAt = now
		in.inst.PlannedStopAt = time.Time{}
		if in.inst.MaxRun > 0 {
			in.inst.PlannedStopAt = now.Add(in.inst.MaxRun)
		}
	case OpStop:
		p.markStoppedLocked(in)
	case OpDestroy:
		delete(p.instances, op.ref)
	}
}

func (p *Provider) markStoppedLocked(in *instance) {
	in.inst.State = provider.StateStopped
	in.inst.Address = ""
	in.inst.StoppedAt = p.opts.Now()
	in.inst.PlannedStopAt = time.Time{}
}

// newAddrLocked hands out a fresh public address from 203.0.113.0/24 (a
// documentation range), so every start gets a different one.
func (p *Provider) newAddrLocked() string {
	p.nextAddr++
	return fmt.Sprintf("203.0.113.%d", 1+(p.nextAddr-1)%254)
}

// pendingFor describes an instance's work in flight, or none, for an
// idempotent repeat of a call.
func pendingFor(in *instance, want provider.State) provider.Pending {
	pend := provider.Pending{Ref: in.inst.Ref, Want: want}
	if in.op != nil {
		pend.Operation = in.op.id
	}
	return pend
}

func refFor(name string) provider.Ref { return provider.Ref("fake/" + name) }

func clone(inst provider.Instance) provider.Instance {
	inst.Labels = maps.Clone(inst.Labels)
	inst.Metadata = maps.Clone(inst.Metadata)
	return inst
}

func ctxErr(ctx context.Context, op Op) error {
	return &provider.Error{Op: string(op), Err: ctx.Err()}
}
