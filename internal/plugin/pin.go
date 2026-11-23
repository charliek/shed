package plugin

// The pin on the bus: admission by machine per user, moves, and the per-user
// gate. The design, the rules and the lock order are in the note at the top
// of registry.go.

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sort"
	"time"

	"github.com/charliek/shed/internal/source"
)

var (
	// ErrMachineRequired is returned by Register on a registry with source
	// bindings: a listener is registered only through Subscribe, which names
	// the machine.
	ErrMachineRequired = errors.New("the credential bus admits a listener only for a named machine")
	// ErrNotPinned is returned by Subscribe and Move on a registry with no
	// source bindings.
	ErrNotPinned = errors.New("the credential bus has no source bindings")
	// ErrDuplicateStream is returned by Subscribe when the user's bound
	// machine already holds a listener for the namespace. Nothing is
	// replaced: the existing stream stays until it ends or its write
	// deadline reaps it, and the machine's next subscribe then succeeds.
	ErrDuplicateStream = errors.New("this machine already holds a subscription to the namespace")
	// ErrBindingStore wraps a failure of the source binding store during an
	// admission: nothing was admitted or registered.
	ErrBindingStore = errors.New("the credential source bindings could not be saved")
)

// NotBoundError is a refusal of a machine that is not its user's credential
// source: a subscribe (Subscribe) or an answer (Bridge.BeginResponseFrom)
// from another machine, or one that was the source before a move.
type NotBoundError struct {
	User string
	// Bound is the user's bound machine (zero if the user has no binding).
	Bound source.Machine
}

func (e *NotBoundError) Error() string {
	if e.Bound.ID == "" {
		return fmt.Sprintf("machine is not the credential source%s: no machine is", forUser(e.User))
	}
	return fmt.Sprintf("machine is not the credential source%s: machine %s (%q) is", forUser(e.User), e.Bound.ID, e.Bound.Name)
}

// ThrottledError is a refusal of an admission that would have written the
// binding store (a new machine id, or a changed name or key fingerprint for
// the bound machine; a known machine that is not the source is refused
// before this and writes nothing) when the user has made too many such
// admissions lately.
// Nothing was asked of the store. It is retryable after RetryAfter.
type ThrottledError struct {
	User       string
	RetryAfter time.Duration
}

func (e *ThrottledError) Error() string {
	return fmt.Sprintf("too many new or changed machines%s; retry after %s", forUser(e.User), e.RetryAfter)
}

// Admission rate limit, per user: a token bucket of admitBurst admissions
// that write the store, one more every admitRefill. An admission that writes
// is a full durable rewrite of the store under its one lock, so without a
// bound one user's misbehaving broker (cycling ids or names) would keep the
// disk, and every other user's admission, waiting. A real machine writes
// once when it first connects and again only when its name or key changes
// while it is the source (a refused machine's retries write nothing), so a
// user's several machines and an upgrade fit well within the burst.
// Vars so tests can change them.
var (
	admitBurst  = 8
	admitRefill = 15 * time.Second
	// pinNow is the clock of the rate limit.
	pinNow = time.Now
)

// maxRefusalsLogged bounds how many machines one user's refusal log
// remembers between two changes of the binding; past it the set starts over
// (so a refused machine may be logged once more).
const maxRefusalsLogged = 2 * source.MaxMachinesPerUser

// userGate is one user's gate: the per-user critical section. sem holds one
// token while the gate is held; it is a channel so that a waiter can give up
// with its request's context or its deadline. Every field below is guarded
// by holding the gate.
type userGate struct {
	sem chan struct{}

	// The admission rate limit.
	tokens     int
	refilledAt time.Time
	throttled  bool // a throttle has been logged since the last admitted write

	// refused is the machines whose refusal has been logged since the
	// binding was last at refusedGen.
	refusedGen uint64
	refused    map[string]bool
}

// gate returns user's gate, making it on first use. Users are the server's
// configured names (and the implicit user), so the map is bounded by the
// configuration.
func (r *Registry) gate(user string) *userGate {
	r.gatesMu.Lock()
	defer r.gatesMu.Unlock()
	g := r.gates[user]
	if g == nil {
		g = &userGate{sem: make(chan struct{}, 1), tokens: admitBurst, refilledAt: pinNow()}
		r.gates[user] = g
	}
	return g
}

// lock takes the gate, waiting no longer than ctx lives.
func (g *userGate) lock(ctx context.Context) error {
	select {
	case g.sem <- struct{}{}:
		return nil
	default:
	}
	if gateWaitHook != nil {
		gateWaitHook()
	}
	select {
	case g.sem <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// lockBy takes the gate, waiting no later than deadline; it reports whether
// it did.
func (g *userGate) lockBy(deadline time.Time) bool {
	select {
	case g.sem <- struct{}{}:
		return true
	default:
	}
	if gateWaitHook != nil {
		gateWaitHook()
	}
	wait := time.Until(deadline)
	if wait <= 0 {
		select {
		case g.sem <- struct{}{}:
			return true
		default:
			return false
		}
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case g.sem <- struct{}{}:
		return true
	case <-timer.C:
		return false
	}
}

func (g *userGate) unlock() { <-g.sem }

// take spends one admission that writes, and reports how long to wait when
// there is none to spend. The caller holds the gate.
func (g *userGate) take(now time.Time) (wait time.Duration, ok bool) {
	if admitRefill > 0 {
		if n := int(now.Sub(g.refilledAt) / admitRefill); n > 0 {
			g.tokens += n
			g.refilledAt = g.refilledAt.Add(time.Duration(n) * admitRefill)
		}
	}
	if g.tokens >= admitBurst {
		g.tokens = admitBurst
		g.refilledAt = now
	}
	if g.tokens > 0 {
		g.tokens--
		g.throttled = false
		return 0, true
	}
	wait = admitRefill - now.Sub(g.refilledAt)
	if wait <= 0 {
		wait = time.Second
	}
	return wait, false
}

// logRefusal logs that user's machine m was refused, once per machine until
// the user's binding changes (a refused broker retries every few seconds).
// The caller holds the gate.
func (g *userGate) logRefusal(user string, b source.Binding, m source.Machine, what string, bound source.Machine) {
	if g.refused == nil || g.refusedGen != b.Generation {
		g.refused = map[string]bool{}
		g.refusedGen = b.Generation
	}
	if g.refused[m.ID] {
		return
	}
	if len(g.refused) >= maxRefusalsLogged {
		g.refused = map[string]bool{}
	}
	g.refused[m.ID] = true
	log.Printf("credential bus: refused %s from %s%s: the user's credential source is %s; logged once until the binding changes",
		what, machineLabel(m), forUser(user), machineLabel(bound))
}

// machineLabel names m in a log line: its id, and its name when known.
func machineLabel(m source.Machine) string {
	if m.Name == "" {
		return "machine " + m.ID
	}
	return fmt.Sprintf("machine %s (%q)", m.ID, m.Name)
}

// knownMachine is user's machine id as the store remembers it (with its
// name), or just the id when the store has no record of it. The store's
// reads take no lock, so it may be called under the user's gate or r.mu.
func knownMachine(b source.Bindings, user, id string) source.Machine {
	for _, m := range b.Machines(user) {
		if m.ID == id {
			return m
		}
	}
	return source.Machine{ID: id}
}

// SetBindings gives the registry its source bindings: from then on a
// listener is registered only through Subscribe, every message is stamped
// with the machine it is delivered to, and an answer is accepted only from
// its user's bound machine. shed-server calls it once at start-up, before
// any listener can register.
func (r *Registry) SetBindings(b source.Bindings) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.bindings = b
}

// pin returns the source bindings, nil when there are none.
func (r *Registry) pin() source.Bindings {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.bindings
}

// Bindings returns the registry's source bindings, nil when there are none.
func (r *Registry) Bindings() source.Bindings { return r.pin() }

// notBoundLocked is the refusal naming user's binding as it is now. The
// store's reads take no lock, so it may be called under r.mu.
func (r *Registry) notBoundLocked(user string) error {
	return &NotBoundError{User: user, Bound: boundMachine(r.bindings, user)}
}

// boundMachine is user's bound machine as the store remembers it (zero when
// the user has no binding; only the id when the record is gone).
func boundMachine(b source.Bindings, user string) source.Machine {
	bd, ok := b.Get(user)
	if !ok {
		return source.Machine{}
	}
	return knownMachine(b, user, bd.MachineID())
}

// wouldWrite reports whether admitting m for user writes the store: the user
// has no binding, m's id is new for the user, or its name or key fingerprint
// changed. Exact when called under user's gate, which every change of the
// user's records (but start-up steps) runs under.
func wouldWrite(b source.Bindings, user string, m source.Machine) bool {
	if _, ok := b.Get(user); !ok {
		return true
	}
	for _, k := range b.Machines(user) {
		if k.ID == m.ID {
			return k.Name != m.Name || k.KeyFingerprint != m.KeyFingerprint
		}
	}
	return true
}

// Test hooks (nil in production). subscribeAdmittedHook runs in Subscribe
// once the store has admitted the machine, with the user's gate still held
// and before the listener is registered; moveStoredHook runs in Move once
// the store has moved the binding, with the gate still held and before the
// other machines' listeners are closed.
// gateWaitHook runs when a caller finds a user's gate held and is about to
// wait for it.
var (
	subscribeAdmittedHook func(key ListenerKey, m source.Machine)
	moveStoredHook        func(user string)
	gateWaitHook          func()
)

// Subscribe admits machine m for the caller's user (key.User) and registers
// it as the user's listener for key.Namespace. m.ID must be a valid machine
// id and m.Name a valid name (the caller validates the request's headers);
// m.KeyFingerprint is the caller's key, recorded and shown, never matched.
//
// Under the user's gate: the bound machine already holding the namespace (a
// listener of it at the binding's current generation) is refused as a
// duplicate before anything else, with nothing asked of the store or spent
// from the rate limit; a machine the store already remembers for the user,
// that is not the bound one, is then refused as not bound, also with nothing
// spent and nothing written (the store only notes, in memory, that it was
// seen); an admission that would write the store is then spent from the
// user's rate limit (*ThrottledError when there is none); the store admits
// or refuses the machine (binding it when the user has none, remembering it
// when it is new); and an admitted machine's listener is registered, under
// r.mu, only while its binding is still current. It returns *NotBoundError
// for a machine that is not the user's source, ErrDuplicateStream when the
// machine already holds the namespace, an error wrapping ErrBindingStore when
// the store failed (nothing is registered), ErrBusClosed after Close,
// ErrNotPinned with no source bindings, and ctx's error when the gate was not
// had before ctx ended.
//
// What the recorded key fingerprint (and name) of a machine means: what the
// machine presented in the last of its subscribes that was accepted, or, for
// a machine that has never been accepted, in its first subscribe (the one
// that made the store remember it, refused as not bound). A machine that is
// not the source keeps what it was recorded with while it is refused,
// whatever it presents on its retries; its record is brought up to date only
// when it is next accepted, after the binding is moved to it. A subscribe
// refused as a duplicate, throttled, or refused before admission changes
// neither. One machine id can legitimately present two keys: its daemon and
// its desktop app share the machine's id and may have enrolled separately.
// The recorded key of the bound machine is then the key of the last of its
// subscriptions that was accepted, and it changes (and the change is logged)
// only when the other process is accepted for a namespace the machine did
// not hold.
func (r *Registry) Subscribe(ctx context.Context, key ListenerKey, m source.Machine) (*Listener, error) {
	if err := ValidateNamespace(key.Namespace); err != nil {
		return nil, err
	}
	b := r.pin()
	if b == nil {
		return nil, ErrNotPinned
	}
	g := r.gate(key.User)
	if err := g.lock(ctx); err != nil {
		return nil, err
	}
	defer g.unlock()

	// The bound machine already holding the namespace is a duplicate, and is
	// answered as one before anything is asked of the store or spent from the
	// rate limit: a refused duplicate changes no record. (Two processes of one
	// machine, its daemon and its desktop app, that enrolled with different
	// keys and each hold a namespace would otherwise rewrite the recorded key
	// on every retry of the other's namespace, and spend the user's budget on
	// it.) Exact under the gate: only Subscribe registers a listener on a
	// registry with bindings, and only under its user's gate.
	if r.holdsCurrent(key, m.ID) {
		return nil, fmt.Errorf("namespace %q: %w", key.Namespace, ErrDuplicateStream)
	}
	gen, err := r.admitLocked(g, b, key.User, m, fmt.Sprintf("a subscription to %q", key.Namespace))
	if err != nil {
		return nil, err
	}
	if subscribeAdmittedHook != nil {
		subscribeAdmittedHook(key, m)
	}
	return r.register(key, stamp{machine: m.ID, name: m.Name, gen: gen}, func() bool { return b.Current(key.User, m.ID, gen) })
}

// admitLocked is the admission Subscribe and SubscribeDelivery share, run
// under user's gate g once the caller has refused a duplicate: a machine the
// store already remembers for the user that is not the bound one is refused
// before anything is spent from the rate limit; an admission that would
// write the store is spent from the user's rate limit (*ThrottledError when
// there is none); the store admits or refuses the machine. It returns the
// binding generation the machine was admitted under. what names the request
// in a refusal's log line.
func (r *Registry) admitLocked(g *userGate, b source.Bindings, user string, m source.Machine, what string) (uint64, error) {
	// A machine the store already remembers for the user that is not the
	// bound one is refused here, also before anything is spent from the rate
	// limit and without Admit: what it presents now is not recorded, and only
	// its in-memory last-seen time is kept fresh (so a machine that keeps
	// retrying is not the one forgotten at the machine limit). Otherwise two
	// processes of a refused machine that enrolled with different keys would
	// rewrite its recorded key on every retry, each a full durable write that
	// spends the user's budget. Exact under the gate: the binding and the
	// user's machines change only under it (or at start-up).
	if bd, ok := b.Get(user); ok && bd.MachineID() != m.ID && b.Seen(user, m.ID) {
		bound := boundMachine(b, user)
		g.logRefusal(user, bd, m, what, bound)
		return 0, &NotBoundError{User: user, Bound: bound}
	}
	if wouldWrite(b, user, m) {
		if wait, ok := g.take(pinNow()); !ok {
			if !g.throttled {
				g.throttled = true
				log.Printf("credential bus: admissions that change the source records%s are being throttled (machine %s, %q, and any other new or changed machine; retry after %s); logged once until one is admitted",
					forUser(user), m.ID, m.Name, wait.Round(time.Second))
			}
			return 0, &ThrottledError{User: user, RetryAfter: wait}
		}
	}
	d, err := b.Admit(user, m)
	if err != nil {
		return 0, fmt.Errorf("%w: %v", ErrBindingStore, err)
	}
	logAdmission(user, d)
	if !d.Admitted {
		g.logRefusal(user, d.Binding, d.Machine, what, d.Bound)
		return 0, &NotBoundError{User: user, Bound: d.Bound}
	}
	return d.Binding.Generation, nil
}

// holdsCurrent reports whether key is held by a listener of machine that is
// still its user's binding at the generation the listener was admitted under.
func (r *Registry) holdsCurrent(key ListenerKey, machine string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	l := r.listeners[key]
	return l != nil && l.MachineID == machine && r.stampCurrentLocked(key.User, l.MachineID, l.Generation)
}

// logAdmission logs what an admission changed: a new binding, a new machine
// remembered, and a changed name or key fingerprint for a known machine.
// Names are quoted, so a name holding quotes or control-like text cannot
// forge a log line.
func logAdmission(user string, d source.Decision) {
	m := d.Machine
	if d.NewlyBound {
		log.Printf("credential bus: machine %s (%q, key %q) is now the credential source%s (the first machine to connect)", m.ID, m.Name, m.KeyFingerprint, forUser(user))
	} else if d.NewMachine {
		log.Printf("credential bus: new machine %s (%q, key %q) presented itself%s; it is not the source (machine %s, %q, is)", m.ID, m.Name, m.KeyFingerprint, forUser(user), d.Bound.ID, d.Bound.Name)
	}
	if d.NameChanged {
		log.Printf("credential bus: machine %s%s changed its name from %q to %q", m.ID, forUser(user), d.PreviousName, m.Name)
	}
	if d.KeyFingerprintChanged {
		log.Printf("credential bus: machine %s (%q)%s now connects with key %q (was %q)", m.ID, m.Name, forUser(user), m.KeyFingerprint, d.PreviousKeyFingerprint)
	}
	for _, e := range d.Evicted {
		log.Printf("credential bus: forgot machine %s (%q)%s, the least recently seen, to stay within %d machines", e.ID, e.Name, forUser(user), source.MaxMachinesPerUser)
	}
}

// SourcesView is one user's credential sources as one moment shows them: the
// binding, the machines the store remembers for the user, and which of them
// hold listeners. It is read under the user's gate (Sources, and Move after
// its clean-up), so the binding and the listeners are of the same moment, and
// only the bound machine, at the binding's generation, is ever listed as
// connected.
type SourcesView struct {
	User string
	// Binding is nil when the user has no binding.
	Binding *source.Binding
	// Machines are the machines the store remembers for the user.
	Machines []source.Machine
	// Connected maps the id of a machine holding listeners of the user, or
	// the user's delivery stream, to the namespaces (request types) it
	// holds, sorted (empty for a machine that holds only the delivery
	// stream). It never names a machine but the bound one.
	Connected map[string][]string
	// Delivery reports whether the bound machine holds the user's delivery
	// stream.
	Delivery bool
}

// MoveResult is what Move did and the user's sources as the move left them,
// read inside the same hold of the user's gate.
type MoveResult struct {
	source.Move
	View SourcesView
}

// Sources returns user's sources, read under the user's gate: the per-user
// critical section that admissions, registrations and moves run in, so the
// answer never mixes the binding of one side of a move with the listeners of
// the other. ctx bounds the wait for the gate (an admission or a move holding
// it may be waiting for the store's disk); its error is returned when the
// gate was not had in time. ErrNotPinned with no source bindings.
func (r *Registry) Sources(ctx context.Context, user string) (SourcesView, error) {
	b := r.pin()
	if b == nil {
		return SourcesView{}, ErrNotPinned
	}
	g := r.gate(user)
	if err := g.lock(ctx); err != nil {
		return SourcesView{}, err
	}
	defer g.unlock()
	return r.view(b, user), nil
}

// view reads user's sources. The caller holds user's gate. Only listeners
// stamped with the binding's machine and generation count as connected
// (under the gate there are no others; the check keeps the view's promise
// even so).
func (r *Registry) view(b source.Bindings, user string) SourcesView {
	v := SourcesView{User: user, Machines: b.Machines(user), Connected: map[string][]string{}}
	bd, ok := b.Get(user)
	if !ok {
		return v
	}
	v.Binding = &bd
	id := bd.MachineID()
	r.mu.Lock()
	for _, l := range r.listeners {
		if l.User == user && l.MachineID == id && l.Generation == bd.Generation {
			v.Connected[id] = append(v.Connected[id], l.Namespace)
		}
	}
	if d := r.deliveries[user]; d != nil && d.machine.MachineID == id && d.gen == bd.Generation {
		v.Delivery = true
		if v.Connected[id] == nil {
			v.Connected[id] = []string{}
		}
	}
	r.mu.Unlock()
	for _, ns := range v.Connected {
		sort.Strings(ns)
	}
	return v
}

// Move moves user's binding to machineID, a machine the store remembers for
// the user (source.ErrUnknownMachine otherwise), recording by as the mover,
// and then, still under the user's gate, closes every listener of the user
// that is not the new binding's (its handler's stream ends) and drops every
// request of the user delivered to another machine or at another
// generation: the old machine can neither be sent them again nor answer
// them, and no other machine inherits them. Another user's listeners and
// requests are untouched. Moving to the machine already bound changes
// nothing. The result's View is the user's sources read after that clean-up,
// before the gate is released: the state this move produced, never a later
// one. The store's errors are returned as they are. ctx bounds the wait for
// the gate.
//
// The routes that call it (and decide who may move) are not part of this
// package.
func (r *Registry) Move(ctx context.Context, user, machineID, by string) (MoveResult, error) {
	b := r.pin()
	if b == nil {
		return MoveResult{}, ErrNotPinned
	}
	g := r.gate(user)
	if err := g.lock(ctx); err != nil {
		return MoveResult{}, err
	}
	defer g.unlock()

	mv, err := b.Use(user, machineID, by)
	if err != nil {
		return MoveResult{}, err
	}
	if moveStoredHook != nil {
		moveStoredHook(user)
	}
	closed, dropped := r.retainOnly(user, mv.Binding.MachineID(), mv.Binding.Generation)
	g.refused = nil
	if mv.Moved {
		log.Printf("credential bus: the credential source%s moved from machine %s (%q) to machine %s (%q) by %q; closed %d stream(s) and dropped %d request(s) of other machines",
			forUser(user), mv.From.ID, mv.From.Name, mv.To.ID, mv.To.Name, by, closed, dropped)
	}
	return MoveResult{Move: mv, View: r.view(b, user)}, nil
}

// retainOnly closes every listener and the delivery stream of user not
// stamped (machine, gen) and frees every pending request of user not stamped
// with them, and reports how many streams it closed and requests it freed.
func (r *Registry) retainOnly(user, machine string, gen uint64) (closed, dropped int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, l := range r.listeners {
		if l.User == user && (l.MachineID != machine || l.Generation != gen) {
			r.removeLocked(l)
			closed++
		}
	}
	for _, m := range r.pending {
		if m.user == user && (m.machine != machine || m.gen != gen) {
			r.freeLocked(m)
			dropped++
		}
	}
	// The delivery stream of another machine or generation is closed too:
	// its pending delivery requests fail with it (they are bound to it).
	if d := r.deliveries[user]; d != nil && (d.machine.MachineID != machine || d.gen != gen) {
		r.removeDeliveryLocked(d)
		closed++
	}
	return closed, dropped
}

// ConnectedMachine is one machine holding listeners of a user.
type ConnectedMachine struct {
	ID   string
	Name string
	// Namespaces are the request types (namespaces) it holds, sorted.
	Namespaces []string
}

// Connected returns the machines that hold listeners of user, by id, each
// with the namespaces it holds: what a status route shows.
func (r *Registry) Connected(user string) []ConnectedMachine {
	r.mu.Lock()
	byID := map[string]*ConnectedMachine{}
	for _, l := range r.listeners {
		if l.User != user {
			continue
		}
		c := byID[l.MachineID]
		if c == nil {
			c = &ConnectedMachine{ID: l.MachineID, Name: l.MachineName}
			byID[l.MachineID] = c
		}
		c.Namespaces = append(c.Namespaces, l.Namespace)
	}
	r.mu.Unlock()
	out := make([]ConnectedMachine, 0, len(byID))
	for _, c := range byID {
		sort.Strings(c.Namespaces)
		out = append(out, *c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// Close ends the bus for shutdown: every listener and delivery stream is
// removed (its handler's stream ends) and nothing registers from then on (ErrBusClosed). Pending
// requests are left to go with the process.
func (r *Registry) Close() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.closed = true
	for _, l := range r.listeners {
		r.removeLocked(l)
	}
	for _, d := range r.deliveries {
		r.removeDeliveryLocked(d)
	}
}
