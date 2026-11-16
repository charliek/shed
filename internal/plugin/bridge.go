package plugin

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/charliek/shed/internal/config"
	"github.com/charliek/shed/internal/source"
)

// SendFunc writes one response envelope into a shed's transport. It takes
// the transport's write lock, giving up at deadline (ErrResponseTimeout,
// nothing written); with the lock held it calls admit, and writes nothing if
// admit returns an error (which it returns); then it writes the frame, within
// the same deadline; and, the frame fully written and the lock still held, it
// calls wrote. So what admit checks holds at the moment the write begins, and
// no other response's admit can run between a write and its wrote.
type SendFunc func(env *Envelope, deadline time.Time, admit func() error, wrote func()) error

// ShedConn represents an active message channel to a running shed.
type ShedConn struct {
	Name    string
	Backend string
	Server  string
	// SendWithin writes a response to the shed's transport. The credential
	// manager sets it, bound to one transport.
	SendWithin SendFunc
	// Send is used when SendWithin is not set (tests): a plain write, which
	// the bridge serializes and wraps as a SendFunc. It must return within a
	// bounded time.
	Send func(env *Envelope) error
	// Owned marks a shed a service client owns. The credential bus is off
	// for it, always: the bridge drops its envelopes to the host, so nothing
	// of it is ever pending and nothing can be answered into it, and leaves
	// it out of ListSheds.
	Owned bool
	// User is the shed's user as the server's own record of the shed
	// reports it (config.ShedOwnership, so config.UserUnresolved for a
	// record that does not agree about whose it is). It is never taken from
	// anything the guest sends. On a server with a user list a shed is
	// routed to its user's listeners only; a shed with no user, or an
	// unresolved one, is routed nowhere. On a server with no user list every
	// shed routes as the implicit user, whatever this says.
	User string
	// CreatedAt is the creation time of the shed incarnation the channel
	// was opened for. A shed deleted and created again under the same name
	// is another incarnation.
	CreatedAt time.Time
}

// route is where a registration's requests go: to user's listeners, or
// nowhere (ok false). A route value is never changed once stored; a change
// stores a new one, so the pointer itself says whether anything changed
// since it was read.
type route struct {
	user string
	ok   bool
}

// shedEntry is one registration: a connection generation and the bridge's
// copy of its ShedConn. conn.User, retired and suspended are guarded by the
// bridge's lock; everything else is fixed at registration, but for the two
// atomics.
type shedEntry struct {
	conn      ShedConn
	gen       uint64   // issued by the bridge, never reused
	send      SendFunc // conn.SendWithin, or conn.Send wrapped
	retired   bool     // replaced or removed
	suspended bool     // its route is held (see SuspendRoute)
	// route is the registration's current route. It is stored under the
	// bridge's lock, a new pointer whenever what it says changes (and never
	// otherwise), and read without it wherever a request or a response is
	// about to have an effect.
	route     atomic.Pointer[route]
	responses atomic.Int32
}

// routeHold suspends the route of one shed incarnation (see SuspendRoute).
type routeHold struct {
	createdAt time.Time
	epoch     uint64
}

// ErrUnmatched is returned for a response that answers no pending request of
// the responding user: forged, replayed, already answered, another user's,
// or for a request dropped since (its shed's connection or route gone).
var ErrUnmatched = errors.New("response does not match a pending request")

// ErrTooManyResponses is returned when a shed's registration (its current
// connection generation) already has maxResponsesInFlight responses being
// delivered. It is retryable.
var ErrTooManyResponses = errors.New("too many responses are being delivered to the shed")

// ErrResponseTimeout is returned when a response could not begin its write
// within responseTimeout of its start. Nothing was written, and the request
// is still pending: it is retryable.
var ErrResponseTimeout = errors.New("the response could not be delivered in time")

// Bounds on responses. A response's whole delivery (the wait for the
// transport's write lock, and the write) is bounded by one deadline,
// responseTimeout after it starts, and a shed's registration has at most
// maxResponsesInFlight being delivered at once; more are refused with
// ErrTooManyResponses (the API answers 429). The count is per registration
// (shedEntry), not per shed name: a registration that replaced another
// starts its own count while the old one's deliveries finish. Vars so tests
// can shrink them.
var (
	responseTimeout      = 5 * time.Second
	maxResponsesInFlight = int32(8)
)

// ShedConnInfo is the public representation of a connected shed.
type ShedConnInfo struct {
	Name    string `json:"name"`
	Backend string `json:"backend"`
	Server  string `json:"server"`
	// User is the user the shed's requests are routed to; empty (and
	// omitted) for the implicit user, and for a shed routed to nobody.
	User string `json:"user,omitempty"`
}

// Bridge connects the API layer to per-shed vsock message connections and
// routes messages between host listeners and VM agents.
//
// Every registration gets a connection generation, issued by the bridge and
// never reused; the credential manager registers again, with a new
// generation, every time a shed's transport connects. A request is bound to
// the generation it arrived on and to the route that generation had then: it
// is answered into that connection while that route stands, or dropped,
// never into a later transport or a later shed registered under the same
// name. The note at the top of registry.go describes the whole mechanism,
// the locks and their order.
type Bridge struct {
	registry *Registry
	// usersConfigured is whether the server has a user list. Without one
	// every shed routes as the implicit user.
	usersConfigured bool

	// mu guards sheds, holds, and each entry's conn.User, retired and
	// suspended. It is held only across in-memory steps, and the registry's
	// lock is never taken under it.
	mu      sync.RWMutex
	sheds   map[string]*shedEntry
	nextGen uint64
	holds   map[string]routeHold // by shed name
	holdSeq uint64

	ownedDropLogged   sync.Map // shed name -> true, once a drop was logged
	noRouteDropLogged sync.Map // shed name -> true, once a drop was logged
}

// NewBridge creates a new bridge backed by the given registry. usersConfigured
// is whether the server has a user list: without one, every shed routes as
// the implicit user whatever user its record carries.
func NewBridge(registry *Registry, usersConfigured bool) *Bridge {
	return &Bridge{
		registry:        registry,
		usersConfigured: usersConfigured,
		sheds:           make(map[string]*shedEntry),
		holds:           make(map[string]routeHold),
	}
}

// lookup returns the entry registered under name, or nil.
func (b *Bridge) lookup(name string) *shedEntry {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.sheds[name]
}

// rerouteLocked works out e's route from its state and stores it if it says
// something else than the current one, reporting whether it did. The caller
// holds b.mu (write), and afterwards, outside the lock, drops what the old
// route left behind (dropStale).
func (b *Bridge) rerouteLocked(e *shedEntry) bool {
	var next route
	if !e.retired && !e.suspended {
		next.user, next.ok = b.userRoute(e)
	}
	if cur := e.route.Load(); cur != nil && *cur == next {
		return false
	}
	e.route.Store(&next)
	return true
}

// userRoute is what e's user makes its route: nowhere for a service client's
// shed and, on a server with a user list, for a shed with no user or an
// unresolved one. The caller holds b.mu.
func (b *Bridge) userRoute(e *shedEntry) (string, bool) {
	if e.conn.Owned {
		return "", false
	}
	if !b.usersConfigured {
		return "", true
	}
	if !config.ValidUserName(e.conn.User) {
		return "", false
	}
	return e.conn.User, true
}

// reroutedHook, when set (tests only), runs after a registration's route was
// changed (the new pointer stored, the bridge's lock released) and before
// the messages its old route left behind are dropped.
var reroutedHook func()

// dropStale drops the messages of connection generation gen that its route
// no longer covers. Called with no lock held, after the route was changed.
func (b *Bridge) dropStale(gen uint64) {
	if reroutedHook != nil {
		reroutedHook()
	}
	if b.registry != nil {
		b.registry.prune(gen)
	}
}

// RegisterShed registers a shed's active message connection with the bridge,
// replacing any earlier registration under the name, and returns the
// connection generation it issued. The bridge keeps its own copy of conn,
// with Name set to the registration key. A replaced registration is retired
// at once, without waiting for anything in progress on it: from that instant
// nothing of it is queued, sent to a listener or written into a shed, and
// its pending requests are dropped. A registration of an incarnation whose
// route is held (SuspendRoute) starts held.
func (b *Bridge) RegisterShed(name string, conn *ShedConn) uint64 {
	e := &shedEntry{conn: *conn}
	e.conn.Name = name
	e.send = conn.SendWithin
	if e.send == nil && conn.Send != nil {
		e.send = serialSend(conn.Send)
	}
	b.mu.Lock()
	old := b.sheds[name]
	b.nextGen++
	e.gen = b.nextGen
	if h, ok := b.holds[name]; ok && h.createdAt.Equal(e.conn.CreatedAt) {
		e.suspended = true
	}
	b.rerouteLocked(e)
	b.sheds[name] = e
	if old != nil {
		old.retired = true
		b.rerouteLocked(old)
	}
	b.mu.Unlock()
	if old != nil {
		b.dropStale(old.gen)
	}
	return e.gen
}

// serialSend makes a SendFunc of a plain write, with a write lock of its
// own.
func serialSend(send func(*Envelope) error) SendFunc {
	lock := make(chan struct{}, 1)
	return func(env *Envelope, deadline time.Time, admit func() error, wrote func()) error {
		wait := time.Until(deadline)
		if wait <= 0 {
			return ErrResponseTimeout
		}
		timer := time.NewTimer(wait)
		defer timer.Stop()
		select {
		case lock <- struct{}{}:
		case <-timer.C:
			return ErrResponseTimeout
		}
		defer func() { <-lock }()
		if err := admit(); err != nil {
			return err
		}
		if err := send(env); err != nil {
			return err
		}
		wrote()
		return nil
	}
}

// remove removes the registration under name when match accepts it, and
// reports whether it did. It does not wait for work in progress on it.
func (b *Bridge) remove(name string, match func(*shedEntry) bool) bool {
	b.mu.Lock()
	e := b.sheds[name]
	if e == nil || !match(e) {
		b.mu.Unlock()
		return false
	}
	delete(b.sheds, name)
	e.retired = true
	b.rerouteLocked(e)
	b.mu.Unlock()
	b.dropStale(e.gen)
	return true
}

// UnregisterShed removes whatever message connection is registered under name.
func (b *Bridge) UnregisterShed(name string) {
	b.remove(name, func(*shedEntry) bool { return true })
}

// UnregisterConn removes the registration under name only while it is still
// the connection of generation gen, and reports whether it did.
func (b *Bridge) UnregisterConn(name string, gen uint64) bool {
	return b.remove(name, func(e *shedEntry) bool { return e.gen == gen })
}

// SetShedUser changes the user the running shed name routes as, when it is
// the incarnation created at createdAt and not a service client's shed, and
// reports whether it did. The admin assign command, and a remote shed whose
// reported ownership changes, use it: the shed's record changes, and its
// route must follow without a restart. The connection generation is kept, so
// the shed's later requests still arrive.
//
// It does not wait for anything. The registration is looked up, checked and
// changed under one hold of the bridge's lock, so it acts on the
// registration that is current, never on one replaced meanwhile. When the
// route changes, nothing published on the old route is queued, sent to a
// listener or answered into the shed from that instant, and those messages
// are then dropped by the connection's generation: a machine of the previous
// user is never re-sent them and cannot answer them. Setting the user the
// shed already has changes nothing. It does not lift a route hold
// (ResumeRoute does).
func (b *Bridge) SetShedUser(name string, createdAt time.Time, user string) bool {
	b.mu.Lock()
	e := b.sheds[name]
	if e == nil || e.conn.Owned || !e.conn.CreatedAt.Equal(createdAt) {
		b.mu.Unlock()
		return false
	}
	e.conn.User = user
	changed := b.rerouteLocked(e)
	gen := e.gen
	b.mu.Unlock()
	if changed {
		b.dropStale(gen)
	}
	b.noRouteDropLogged.Delete(name)
	return true
}

// SuspendRoute holds the route of the shed incarnation (name, createdAt) at
// once, without waiting for anything in progress, and returns the hold's
// epoch: from this instant the shed's registrations of that incarnation (the
// current one and any made later) route nowhere, so nothing of the shed is
// queued, sent to a listener or answered, queued work included, and its
// pending requests are dropped. A frame already being written finishes
// within its own bound. The hold lasts until ResumeRoute is called with this
// epoch; a later SuspendRoute replaces it with a new epoch. The remote
// backend calls it as soon as a shed's reported user changes, before it
// applies the new user (SetShedUser) out of line. On a server with no user
// list routes do not depend on users, and it does nothing (epoch 0).
func (b *Bridge) SuspendRoute(name string, createdAt time.Time) uint64 {
	if !b.usersConfigured {
		return 0
	}
	b.mu.Lock()
	b.holdSeq++
	epoch := b.holdSeq
	b.holds[name] = routeHold{createdAt: createdAt, epoch: epoch}
	var gen uint64
	changed := false
	if e := b.sheds[name]; e != nil && e.conn.CreatedAt.Equal(createdAt) {
		e.suspended = true
		changed = b.rerouteLocked(e)
		gen = e.gen
	}
	b.mu.Unlock()
	if changed {
		b.dropStale(gen)
	}
	return epoch
}

// ResumeRoute lifts the route hold of name made with epoch, and reports
// whether it did; a hold replaced since by a later SuspendRoute stays. The
// route the shed gets back is a new one: nothing published before the hold
// comes back with it.
func (b *Bridge) ResumeRoute(name string, epoch uint64) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	h, ok := b.holds[name]
	if !ok || h.epoch != epoch {
		return false
	}
	delete(b.holds, name)
	if e := b.sheds[name]; e != nil && e.conn.CreatedAt.Equal(h.createdAt) {
		e.suspended = false
		b.rerouteLocked(e)
	}
	return true
}

// ShedUser returns the user the connected shed name is routed to. ok is false
// when no shed is connected under the name, or it is routed nowhere.
func (b *Bridge) ShedUser(name string) (user string, ok bool) {
	e := b.lookup(name)
	if e == nil {
		return "", false
	}
	rt := e.route.Load()
	return rt.user, rt.ok
}

// IncarnationUser returns the user the shed incarnation (name, createdAt) is
// routed to: the connected shed of that name, only while it is the one
// created at createdAt. ok is false for a zero createdAt, another
// incarnation, a shed not connected, and a shed routed nowhere. It is read
// afresh on every call, so a shed deleted and created again under its name
// never inherits the previous one's answer.
func (b *Bridge) IncarnationUser(name string, createdAt time.Time) (user string, ok bool) {
	if createdAt.IsZero() {
		return "", false
	}
	e := b.lookup(name)
	if e == nil || !e.conn.CreatedAt.Equal(createdAt) {
		return "", false
	}
	rt := e.route.Load()
	return rt.user, rt.ok
}

// ListSheds returns the sheds with active message channels that are routed
// to user, or every shed but service clients' when all is true, by name.
func (b *Bridge) ListSheds(user string, all bool) []ShedConnInfo {
	b.mu.RLock()
	defer b.mu.RUnlock()

	result := make([]ShedConnInfo, 0, len(b.sheds))
	for _, e := range b.sheds {
		if e.conn.Owned {
			continue
		}
		rt := e.route.Load()
		if !all && (!rt.ok || rt.user != user) {
			continue
		}
		result = append(result, ShedConnInfo{
			Name:    e.conn.Name,
			Backend: e.conn.Backend,
			Server:  e.conn.Server,
			User:    rt.user,
		})
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Name < result[j].Name })
	return result
}

// PublishToHost routes an envelope from the shed name's connection of
// generation gen to its user's listener for the envelope's namespace,
// overwriting the envelope's shed field with the bridge's own record. The
// registry takes the envelope over (see Registry.Publish). ctx is the
// publishing message channel's: the publish waits for a full listener queue
// no longer than it lives (and at most a short bound).
//
// It is dropped, with an error, when that connection is no longer the one
// registered under name (a channel that outlived its registration must not
// speak for whatever is registered now). An envelope from a client-owned
// shed, or from a shed routed to nobody, is dropped (nil error, logged once
// per shed). With no listener for the user and namespace it returns the
// registry's error; nothing is sent back to the shed.
//
// The message is published on the route read here, and is live only while
// that route is still the connection's: the registry checks that where it
// makes the message, each time it is about to queue it (after any wait for
// room), and when the listener takes it. A change of the shed's user, a
// route hold or a retirement in between therefore drops it.
func (b *Bridge) PublishToHost(ctx context.Context, name string, gen uint64, env *Envelope) error {
	e := b.lookup(name)
	if e == nil || e.gen != gen {
		return fmt.Errorf("shed %q is not registered on the credential bus; dropping its message", name)
	}
	if e.conn.Owned {
		b.logOwnedDrop(name)
		return nil
	}
	rt := e.route.Load()
	if !rt.ok {
		b.logNoRouteDrop(name)
		return nil
	}
	env.Shed = &ShedInfo{Name: e.conn.Name, Backend: e.conn.Backend, Server: e.conn.Server}
	return b.registry.publish(ctx, rt.user, gen, env, func() bool { return e.route.Load() == rt })
}

// Response is one response being delivered to a shed connection: it holds
// one of the shed's maxResponsesInFlight slots and the deadline of the whole
// delivery. End releases it.
type Response struct {
	b        *Bridge
	e        *shedEntry
	m        *Message
	env      *Envelope
	deadline time.Time
}

// BeginResponse starts delivering user's response env, which answers the
// request whose server ID is env.InReplyTo: it matches the pending request
// (which must have been routed to user, for env's namespace and shed), takes
// one of the shed connection's response slots, and starts the delivery's
// deadline. It fails with ErrUnmatched when there is no such request, and
// with ErrTooManyResponses (retryable) when the shed has
// maxResponsesInFlight being delivered. Nothing is consumed. Only a
// response that matched one of the user's own requests takes a slot, so
// nobody else can use up a shed's slots. The caller calls Send, then End.
func (b *Bridge) BeginResponse(user string, env *Envelope) (*Response, error) {
	return b.BeginResponseFrom(user, "", env)
}

// respondGateHook, when set (tests only), runs in BeginResponseFrom with the
// user's gate held, after the binding was checked and before the request is
// matched.
var respondGateHook func(user string)

// BeginResponseFrom is BeginResponse for an answer sent by machine
// machineID. On a registry with source bindings the answer is accepted only
// from the user's bound machine, and matches only a request delivered to that
// machine at the binding's current generation: it is checked, and the request
// matched and its slot taken, under the user's gate, which a move holds
// across its change of the binding and its clean-up. A machine that is not
// the user's bound one gets a *NotBoundError; when the gate is not had by the
// response's deadline, ErrResponseTimeout (retryable; nothing was matched).
// With no source bindings machineID must be "" (the stamp every request has
// there). Send then writes the answer only while the request's stamp is
// still current (Response.Send).
func (b *Bridge) BeginResponseFrom(user, machineID string, env *Envelope) (*Response, error) {
	deadline := time.Now().Add(responseTimeout)
	if b.registry == nil || env.Shed == nil {
		return nil, ErrUnmatched
	}
	st := stamp{machine: machineID}
	if pin := b.registry.pin(); pin != nil {
		g := b.registry.gate(user)
		if !g.lockBy(deadline) {
			return nil, ErrResponseTimeout
		}
		defer g.unlock()
		bd, ok := pin.Get(user)
		if !ok {
			// No machine is the user's source, so none was sent a request:
			// nothing can match.
			return nil, ErrUnmatched
		}
		if bd.MachineID() != machineID {
			bound := boundMachine(pin, user)
			g.logRefusal(user, bd, source.Machine{ID: machineID}, "an answer", bound)
			return nil, &NotBoundError{User: user, Bound: bound}
		}
		st.gen = bd.Generation
		if respondGateHook != nil {
			respondGateHook(user)
		}
	}
	m := b.registry.matchFrom(user, st, env.Namespace, env.Shed.Name, env.InReplyTo)
	if m == nil {
		return nil, ErrUnmatched
	}
	e := b.lookup(m.shed)
	if e == nil || e.gen != m.conn || e.send == nil {
		// The request's connection is no longer registered: the request is
		// being dropped with it.
		return nil, ErrUnmatched
	}
	if e.responses.Add(1) > maxResponsesInFlight {
		e.responses.Add(-1)
		return nil, fmt.Errorf("shed %q: %w", m.shed, ErrTooManyResponses)
	}
	// What is written into the shed answers the ID its guest chose.
	out := *env
	out.InReplyTo = m.guestID
	return &Response{b: b, e: e, m: m, env: &out, deadline: deadline}, nil
}

// End releases the response's slot.
func (r *Response) End() { r.e.responses.Add(-1) }

// Send writes the response into the connection its request arrived on. The
// request is checked once more with the transport's write lock held,
// immediately before the write: it must still be pending, and the route it
// was published on must still be its connection's, so a response that waited
// for the write lock is not written after the shed's user changed, its route
// was held or its connection was retired (ErrUnmatched); and with source
// bindings the machine it was delivered to must still be the user's binding
// at the same generation, so an answer is never written after a move
// (*NotBoundError). A final response
// consumes its request once its frame has been written, and not before: when
// the write lock is not had by the deadline (ErrResponseTimeout) nothing was
// written and the request is still pending, so the response can be retried.
// A write that fails is not retryable: it may have begun, so the transport
// is closed, and the request goes when its connection does.
func (r *Response) Send() error {
	reg, m := r.b.registry, r.m
	return r.e.send(r.env, r.deadline,
		func() error {
			if !reg.admit(m) {
				// A move of the binding since the match: the answering
				// machine is no longer the user's source.
				if pin := reg.pin(); pin != nil && !pin.Current(m.user, m.machine, m.gen) {
					return &NotBoundError{User: m.user, Bound: boundMachine(pin, m.user)}
				}
				return fmt.Errorf("shed %q: %w", m.shed, ErrUnmatched)
			}
			return nil
		},
		func() {
			if r.env.Final {
				reg.free(m)
			}
		})
}

// Respond delivers user's response env: BeginResponse, Send and End in one
// call.
func (b *Bridge) Respond(user string, env *Envelope) error {
	resp, err := b.BeginResponse(user, env)
	if err != nil {
		return err
	}
	defer resp.End()
	return resp.Send()
}

// logOwnedDrop logs, once per shed name, that an owned shed's envelope was
// dropped.
func (b *Bridge) logOwnedDrop(shedName string) {
	if _, loaded := b.ownedDropLogged.LoadOrStore(shedName, true); !loaded {
		log.Printf("[%s] credential bus is disabled for client-owned sheds; dropping its plugin messages", shedName)
	}
}

// logNoRouteDrop logs, once per shed name, that the envelope of a shed with
// no user (or an unresolved one) was dropped.
func (b *Bridge) logNoRouteDrop(shedName string) {
	if _, loaded := b.noRouteDropLogged.LoadOrStore(shedName, true); !loaded {
		log.Printf("[%s] shed routes to no user (it has none, its user is unresolved, or a change of its user is in progress); dropping its plugin messages", shedName)
	}
}
