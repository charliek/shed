package plugin

// The credential bus, as this file (the registry: listeners and the messages
// held for them) and bridge.go (the bridge: shed connections and their
// routes) implement it. The code is meant to be checked against this.
//
// What identifies a request. A guest chooses the ID of the request it sends,
// so that ID identifies nothing on the server. When a request is accepted the
// registry mints its own ID for it (random, unguessable, unique among the
// requests it holds) and makes one Message: the request's envelope carrying
// the server's ID in place of the guest's (and, as every envelope a shed
// sends, its shed field replaced by the bridge with the server's own record
// of the shed: name, backend, server), the guest's ID, the user it was
// routed to, its namespace, its shed's name, and the connection generation
// it arrived on. The bridge issues a new generation, never reused, for every
// registration, and the credential manager registers again for every
// transport of every shed incarnation, so the generation names one
// transport of one incarnation. The listener is sent the envelope with the
// server's ID and answers with that ID in in_reply_to. An answer is looked
// up by it, must come from the user, namespace and shed name the Message
// records, and is written into the shed with the guest's ID put back. Two
// requests can never be confused through an ID a guest chose, whatever
// connections, users or sheds they came from, and an answer whose ID is not
// held (answered, discarded, or from before a restart) is refused, in every
// auth mode. Only requests that carry an ID are answerable; any other
// message (an event) is forwarded with nothing changed but its shed field
// (stamped by the bridge, as above) and forgotten once its listener takes
// it.
//
// What becomes of a message on its way. With no listener of its user for
// its namespace it is refused at once and nothing is kept. When the listener
// disconnects while the message waits for room in its queue, a request is
// kept, pending, for that user's next listener (an event is freed). When the
// queue stays full for publishWait (or the publishing channel ends) the
// message is dropped, request or not: it is not kept for re-delivery.
//
// What is checked where. A Message is live while the registry still holds it
// and the route it was published on is still its connection's current route.
// The bridge keeps each connection's route (the user it routes to, or
// nowhere) behind an atomic pointer and stores a new pointer whenever it
// changes: the shed's user is set, its route is suspended or resumed, or the
// connection is replaced or removed. A Message is live only while the
// pointer is the very one read when it was published. The bridge then drops
// the connection's Messages that are no longer live (by generation, never by
// name). Liveness is checked again at every point where a Message has an
// effect, under the lock that effect holds:
//   - when it is made, and each time it is about to be put in a listener's
//     queue, including after waiting for room (r.mu);
//   - when the listener's stream takes it from the queue to send it
//     (Listener.Take, r.mu);
//   - when its answer is about to be written into the shed, after the wait
//     for the transport's write lock and under it (Response.Send).
//
// So a change of route stops queued work, not only new work, from the
// instant the pointer is stored. An effect that passed its check is ordered
// before the change; a frame already being written finishes within its
// deadline. A final answer consumes its Message only once its frame has been
// written (the write returned success, even if its deadline passed just as
// it did), still under the transport's write lock, so no other answer to it
// can be written after it. If the write lock or the deadline is not had in
// time nothing is written and the request stays answerable (retryable); a
// write that fails may have left a frame half written, so the transport is
// closed and nothing more is written to it (not retryable); and if the route
// or the connection changes the request is dropped with it.
//
// Locks, and their order. r.mu (registry) guards the listeners, every
// Message and the counts; b.mu (bridge) guards the registrations and route
// holds. Both are held only across in-memory steps: no I/O, no blocking
// channel operation, and neither is ever taken while the other is held.
// A transport's write lock (the credential manager's, one per shed channel)
// is held for one bounded write; an answer waits for it only until its
// deadline, and takes r.mu under it (admit, consume). The credential
// manager's own locks come before all of these: cm.mu, then a channel's
// link.mu, then b.mu or r.mu. Nothing on the publish or the response path
// waits for a lock without a deadline that anything can hold across I/O.
//
// Limits (fixed). Everything the registry retains is a Message, and a
// Message owns its payload: a listener's queue holds references only, so
// freeing a Message frees its payload wherever it is queued. A connection
// may have maxMessagesPerConn Messages (un-answered requests, and events not
// yet taken) and maxBytesPerConn of them; the server maxBytesTotal. A
// guest-chosen identifier is at most maxIdentifierLen bytes. A publish waits
// for queue room at most publishWait, a re-delivery redeliverWait. An
// un-answered request is answerable for at most pendingRetention: matching
// an answer and admitting it for writing both check its age, and free it
// once it is older, whether or not a sweep has run; otherwise its memory is
// reclaimed by the next sweep (from Publish or Register, at most once per
// sweepEvery). Each registration of a shed (one connection generation, not
// the shed's name across overlapping registrations) takes at most
// maxResponsesInFlight answers at once, each within responseTimeout.
//
// What the limits do not yet cover. A subscription's stream writes to its
// HTTP client with no deadline. Take frees an event, and stops counting it,
// before the stream writes it, so a subscriber that stops reading holds the
// payloads its stalled writes carry beyond the counted limits (and keeps its
// namespace registered) until the stream's write deadline, which a later
// commit adds.

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log"
	"sort"
	"sync"
	"time"
)

// ListenerKey names one listener: a user's subscription to a namespace. User
// is the user's name, "" for the implicit user of a server with no user list.
// Two users may hold the same namespace at once; each is sent only the
// requests of sheds that belong to them.
type ListenerKey struct {
	User      string
	Namespace string
}

// Listener represents an active host-side listener subscribed to a namespace
// for one user.
type Listener struct {
	User      string
	Namespace string
	// Messages is the listener's queue. What it carries are references: the
	// reader passes each to Take, which returns the envelope to send, or nil
	// for a message dropped since it was queued.
	Messages  <-chan *Message
	Done      <-chan struct{} // closed when the listener is unregistered
	CreatedAt time.Time
	// Token is issued by the registry and is unique for its life. Unregister
	// removes a listener only while the key is still held by the listener
	// with this token, so a handler that ends late can never remove the
	// listener that replaced it.
	Token uint64

	reg   *Registry
	queue chan *Message // internal write side of Messages
	// room is closed, and replaced, each time a message is taken from the
	// queue, so whoever waits for room in it looks again. Guarded by reg.mu.
	room      chan struct{}
	done      chan struct{} // internal close side
	closeOnce sync.Once
}

// Key is the listener's key in the registry.
func (l *Listener) Key() ListenerKey {
	return ListenerKey{User: l.User, Namespace: l.Namespace}
}

// close closes the listener's done channel, once.
func (l *Listener) close() {
	l.closeOnce.Do(func() { close(l.done) })
}

// Take turns a message received from Messages into the envelope to send to
// the listener's machine, at the moment it is about to be sent. It returns
// nil, and nothing must be sent, for a message that is no longer live (its
// request was answered or dropped, or its shed's route changed, since it was
// queued) and for a listener that has been unregistered.
func (l *Listener) Take(m *Message) *Envelope {
	r := l.reg
	r.mu.Lock()
	defer r.mu.Unlock()
	close(l.room)
	l.room = make(chan struct{})
	if !r.liveLocked(m) {
		r.freeLocked(m)
		return nil
	}
	env := m.env
	if !m.tracked {
		r.freeLocked(m) // an event is forgotten once taken
	}
	if r.listeners[l.Key()] != l {
		return nil
	}
	return env
}

// Message is one envelope the registry holds: a request not yet answered
// (tracked), kept for /respond and for re-delivery to a reconnecting
// listener of its user, or any other message (an event) on its way to a
// listener. It owns its envelope and payload; queues hold references to it.
// Every field but env and live is fixed when it is made; those two are
// guarded by the registry's lock and are nil once the Message is freed.
type Message struct {
	env *Envelope
	// id is the server's ID for a tracked request, the one its listener is
	// sent and answers with; guestID is the ID the guest chose, put back
	// when the answer is written into the shed.
	id, guestID string
	user        string
	namespace   string
	shed        string
	// conn is the bridge connection generation the message arrived on.
	conn uint64
	// live, when set, reports whether the route the message was published
	// on is still its connection's current route (see Bridge.PublishToHost).
	live    func() bool
	at      time.Time
	seq     uint64 // order of arrival
	size    int    // what it counts against the byte limits
	tracked bool
}

// connUsage is what the messages of one connection generation hold, and
// which they are.
type connUsage struct {
	bytes int
	msgs  map[*Message]struct{}
}

// Bounds on what the registry retains. A guest chooses its payloads, and a
// request not yet answered keeps its envelope for up to pendingRetention, so
// without a bound a guest that sends many requests to a machine that never
// answers them could hold any amount of the server's memory. A message
// beyond a bound is dropped (the publish returns an error that the message
// channel logs) and is not delivered. Vars so tests can shrink them.
var (
	// maxMessagesPerConn is how many messages one connection generation (one
	// transport of one shed incarnation) may have held: its un-answered
	// requests, and its other messages not yet taken by a listener. It is
	// counted per connection, never per shed name.
	maxMessagesPerConn = 128
	// maxBytesPerConn bounds what one connection's messages retain.
	maxBytesPerConn = 4 << 20
	// maxBytesTotal bounds what every message retains.
	maxBytesTotal = 64 << 20
)

// maxIdentifierLen is the most bytes a guest-chosen identifier of an
// envelope (its ID, in_reply_to, namespace and type) may have. Longer is
// refused before anything is retained. UUIDs are 36 bytes and namespaces
// are already limited to 128.
const maxIdentifierLen = 128

// ErrIdentifierTooLong is returned for an envelope whose ID, in_reply_to,
// namespace or type is longer than maxIdentifierLen.
var ErrIdentifierTooLong = errors.New("an envelope identifier is too long")

// messageOverhead is what a message is counted for beyond the bytes of its
// strings and payload: the Message, its envelope and timestamp, its map
// slots and its queue slot.
const messageOverhead = 256

// publishWait is how long a publish waits for room in a listener's queue
// before it drops the message. A listener that stops reading must never hold
// up the shed's message channel (Publish runs in its read loop), its
// teardown, or anyone waiting on either. redeliverWait is the same bound for
// re-delivering one request to a reconnected listener. Vars so tests can
// change them.
var (
	publishWait   = time.Second
	redeliverWait = 5 * time.Second
)

// sweepEvery is the least time between two sweeps of stale messages, from
// Publish or Register, so neither scans every retained message each time.
var sweepEvery = time.Minute

// ErrPendingLimit is returned when a message is dropped because its
// connection, or the server, holds as much as it may.
var ErrPendingLimit = errors.New("too many un-answered requests")

// ErrListenerBusy is returned when a message is dropped because its
// listener's queue stayed full for publishWait.
var ErrListenerBusy = errors.New("the listener is not reading its messages")

// errConnGone is returned by a publish whose connection was retired or lost
// its route while the publish was on its way.
var errConnGone = errors.New("the shed's connection was retired or lost its route")

// errListenerGone is returned by enqueue when the listener was unregistered.
var errListenerGone = errors.New("the listener disconnected")

// pendingRetention bounds how long an un-answered request is retained (for
// re-delivery) before it is swept as abandoned. Generous on purpose — far longer
// than any credential request's own client-side timeout, so the sweep only
// reclaims genuine leaks, never a still-valid slow approval. A var so tests can
// shrink it.
var pendingRetention = 1 * time.Hour

// Registry tracks active listeners by user and namespace (one listener per
// user per namespace) and every message held for them. See the note at the
// top of this file.
type Registry struct {
	// mu guards everything below and every Message's env. It is held only
	// across in-memory steps.
	mu        sync.Mutex
	listeners map[ListenerKey]*Listener
	nextToken uint64
	// pending is the requests not yet answered, by the server's ID for each,
	// in every auth mode. /respond is validated against it: a caller can
	// only answer a request its user's listener was sent. Entries are added
	// when a request is accepted, removed once its final answer has been
	// written into its shed or its connection or route goes, kept across a
	// listener disconnect so a reconnecting listener of the same user is
	// re-sent them, and swept once stale.
	pending    map[string]*Message
	conns      map[uint64]*connUsage // every held message, by connection generation
	totalBytes int
	nextSeq    uint64
	lastSweep  time.Time
}

// NewRegistry creates a new empty registry.
func NewRegistry() *Registry {
	return &Registry{
		listeners: make(map[ListenerKey]*Listener),
		pending:   make(map[string]*Message),
		conns:     make(map[uint64]*connUsage),
	}
}

// Register creates a listener for key. It returns an error if the user
// already has a listener for the namespace or the namespace is reserved.
func (r *Registry) Register(key ListenerKey) (*Listener, error) {
	if err := ValidateNamespace(key.Namespace); err != nil {
		return nil, err
	}

	r.mu.Lock()
	if _, exists := r.listeners[key]; exists {
		r.mu.Unlock()
		return nil, fmt.Errorf("namespace %q is already registered", key.Namespace)
	}

	r.nextToken++
	queue := make(chan *Message, 32)
	done := make(chan struct{})
	l := &Listener{
		User:      key.User,
		Namespace: key.Namespace,
		Messages:  queue,
		Done:      done,
		CreatedAt: time.Now().UTC(),
		Token:     r.nextToken,
		reg:       r,
		queue:     queue,
		room:      make(chan struct{}),
		done:      done,
	}
	r.listeners[key] = l

	// A reconnecting listener inherits its user's still-un-answered requests
	// for the namespace, so an approval in flight when the previous connection
	// dropped is not lost. Sweep stale entries first (at most once per
	// sweepEvery), then collect them for re-delivery after the lock is
	// released.
	r.maybeSweepLocked(time.Now())
	redeliver := r.pendingForLocked(key)
	r.mu.Unlock()
	if redeliverHook != nil {
		redeliverHook(l, len(redeliver))
	}

	// Re-deliver ASYNCHRONOUSLY. The caller (handlePluginSubscribe) only starts
	// reading l.Messages after Register returns, so a backlog larger than the
	// queue could never be sent inline. The goroutine instead feeds the queue
	// as the handler reads, and ends if the listener is torn down (done) or
	// stops reading for redeliverWait, so it cannot leak.
	if len(redeliver) > 0 {
		go r.redeliverPending(l, redeliver)
	}
	return l, nil
}

// redeliverPending queues a freshly (re)registered listener's retained
// requests for it. Each is queued only while it is still live, checked under
// r.mu at the moment it is queued (enqueue), so a request answered or dropped
// since is never re-sent. It stops if the listener disconnects, or leaves one
// request unqueued for redeliverWait (the rest stay retained for the next
// reconnect). Run in its own goroutine so Register never blocks on a backlog
// the handler hasn't begun reading yet.
func (r *Registry) redeliverPending(l *Listener, msgs []*Message) {
	if redeliverDoneHook != nil {
		defer redeliverDoneHook(l)
	}
	full := false
	onFull := func() { // once per re-delivery
		if !full && redeliverFullHook != nil {
			redeliverFullHook(l)
		}
		full = true
	}
	for _, m := range msgs {
		err := r.enqueue(context.Background(), l, m, redeliverWait, onFull)
		switch {
		case err == nil, errors.Is(err, errConnGone):
			// queued, or answered or dropped since: on to the next
		case errors.Is(err, errListenerGone):
			return
		default:
			log.Printf("credential bus: listener for namespace %q%s is not reading; its retained request(s) are left for its next connection",
				l.Namespace, forUser(l.User))
			return
		}
	}
}

// Test hooks (nil in production). redeliverHook runs in Register with the
// number of retained requests it will re-deliver to l (0: none, and no
// re-delivery goroutine starts); redeliverFullHook runs once per re-delivery
// when it first finds l's queue full; redeliverDoneHook runs when a
// re-delivery goroutine ends. publishFullHook runs when a publish first finds
// its listener's queue full, before it waits for room.
var (
	redeliverHook     func(l *Listener, n int)
	redeliverFullHook func(l *Listener)
	redeliverDoneHook func(l *Listener)
	publishFullHook   func()
)

// enqueue puts m in l's queue, waiting for room at most wait and no longer
// than ctx lives. Every attempt is made under r.mu, after checking there that
// l is still the registered listener (else errListenerGone) and m is still
// live (else errConnGone): a message whose route went while it waited for
// room is not queued when room comes. A queue that stays full returns
// ErrListenerBusy, an ended ctx its error. onFull, when set, runs once, when
// the queue is first found full, before the wait.
func (r *Registry) enqueue(ctx context.Context, l *Listener, m *Message, wait time.Duration, onFull func()) error {
	var timeout <-chan time.Time
	for {
		r.mu.Lock()
		if r.listeners[l.Key()] != l {
			r.mu.Unlock()
			return errListenerGone
		}
		if !r.liveLocked(m) {
			r.mu.Unlock()
			return errConnGone
		}
		select {
		case l.queue <- m:
			r.mu.Unlock()
			return nil
		default:
		}
		room := l.room
		r.mu.Unlock()

		if timeout == nil { // the first time the queue is found full
			if onFull != nil {
				onFull()
			}
			timer := time.NewTimer(wait)
			defer timer.Stop()
			timeout = timer.C
		}
		select {
		case <-room:
		case <-l.done:
		case <-ctx.Done():
			return ctx.Err()
		case <-timeout:
			return ErrListenerBusy
		}
	}
}

// liveLocked reports whether m is still held and the route it was published
// on is still its connection's. The caller holds r.mu.
func (r *Registry) liveLocked(m *Message) bool {
	return m.env != nil && (m.live == nil || m.live())
}

// pendingForLocked returns the live un-answered requests of one user for one
// namespace, in the order they arrived. The caller must hold r.mu.
func (r *Registry) pendingForLocked(key ListenerKey) []*Message {
	var msgs []*Message
	for _, m := range r.pending {
		if m.user == key.User && m.namespace == key.Namespace && r.liveLocked(m) {
			msgs = append(msgs, m)
		}
	}
	sort.Slice(msgs, func(i, j int) bool { return msgs[i].seq < msgs[j].seq })
	return msgs
}

// maybeSweepLocked sweeps stale messages when the last sweep was at least
// sweepEvery ago. The caller must hold r.mu.
func (r *Registry) maybeSweepLocked(now time.Time) {
	if now.Sub(r.lastSweep) >= sweepEvery {
		r.sweepStaleLocked(now)
	}
}

// sweepStaleLocked drops messages older than pendingRetention across all
// namespaces. The caller must hold r.mu. Called, at most once per
// sweepEvery, from Register and Publish — deliberately NOT from a background
// sweeper goroutine: so any ongoing request traffic OR listener churn keeps
// the set bounded, without the goroutine + ctx the Registry doesn't carry.
// The sweep only reclaims memory: whether a request can still be answered
// does not wait for it, because match and admit check the same age and free
// a stale request themselves. The only un-swept case is a server gone fully
// idle (no further Publish anywhere, no Register anywhere, no answer to the
// stale request); that retention is frozen and bounded by the limits above
// (not a growing leak), so a background sweeper isn't warranted.
func (r *Registry) sweepStaleLocked(now time.Time) {
	r.lastSweep = now
	for _, u := range r.conns {
		for m := range u.msgs {
			r.expiredLocked(m, now)
		}
	}
}

// envelopeSize is what an envelope retains: every string and the payload.
func envelopeSize(env *Envelope) int {
	n := len(env.ID) + len(env.Namespace) + len(env.Type) + len(env.InReplyTo) + len(env.Payload)
	if env.Shed != nil {
		n += len(env.Shed.Name) + len(env.Shed.Backend) + len(env.Shed.Server)
	}
	return n
}

// checkIdentifiers refuses an envelope whose guest-chosen identifiers are
// longer than maxIdentifierLen. The error names the field, never its value.
func checkIdentifiers(env *Envelope) error {
	for _, f := range []struct {
		name  string
		value string
	}{{"id", env.ID}, {"in_reply_to", env.InReplyTo}, {"namespace", env.Namespace}, {"type", string(env.Type)}} {
		if len(f.value) > maxIdentifierLen {
			return fmt.Errorf("envelope %s is %d bytes (at most %d): %w; dropping it", f.name, len(f.value), maxIdentifierLen, ErrIdentifierTooLong)
		}
	}
	return nil
}

// maxQuoted is the most bytes of a guest-chosen string an error of this
// package quotes.
const maxQuoted = 48

// clip shortens a guest-chosen string for an error, which never carries more
// than maxQuoted bytes of one.
func clip(s string) string {
	if len(s) <= maxQuoted {
		return s
	}
	return s[:maxQuoted] + "..."
}

// freeLocked releases m: its envelope and payload, its place among the
// pending requests, and what it counted. A queue that still holds a
// reference to it holds nothing but the reference. It does nothing for a
// message already freed. The caller must hold r.mu.
func (r *Registry) freeLocked(m *Message) {
	if m.env == nil {
		return
	}
	m.env = nil
	m.live = nil // and with it the registration it was published on
	if m.tracked {
		delete(r.pending, m.id)
	}
	r.totalBytes -= m.size
	if u := r.conns[m.conn]; u != nil {
		u.bytes -= m.size
		delete(u.msgs, m)
		if len(u.msgs) == 0 {
			delete(r.conns, m.conn)
		}
	}
}

// free is freeLocked for a caller that does not hold r.mu.
func (r *Registry) free(m *Message) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.freeLocked(m)
}

// prune frees the messages of connection generation conn that are no longer
// live. The bridge calls it after it has changed that connection's route
// (the shed's user set, the route suspended, the registration replaced or
// removed): the route's messages are dropped, so a machine of the user it no
// longer routes to is never sent them and cannot answer them. It acts on the
// one generation only, so it can never touch the requests of a shed
// registered under the same name since, and it keeps whatever the connection
// published on its current route.
func (r *Registry) prune(conn uint64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	u := r.conns[conn]
	if u == nil {
		return
	}
	for m := range u.msgs {
		if !r.liveLocked(m) {
			r.freeLocked(m)
		}
	}
}

// Unregister removes the listener for key if it is still the listener with
// this token, closes its Done channel, and reports whether it did. A listener
// that has since been replaced under the key is left alone, so a handler
// that ends late cannot remove its successor.
//
// The user's still-un-answered requests are RETAINED (not discarded) so a
// reconnecting listener of the same user has them re-delivered on the next
// Register; they are bounded by the sweep and the limits, and cleared by the
// final response. Whatever else was still in the listener's queue (events
// nobody took) is freed.
func (r *Registry) Unregister(key ListenerKey, token uint64) bool {
	r.mu.Lock()
	defer r.mu.Unlock()

	l, exists := r.listeners[key]
	if !exists || l.Token != token {
		return false
	}
	delete(r.listeners, key)
	l.close()
	// Nothing is queued for l from here (enqueue checks the listener under
	// r.mu), so this empties the queue for good.
	for {
		select {
		case m := <-l.queue:
			if !m.tracked {
				r.freeLocked(m)
			}
		default:
			return true
		}
	}
}

// Get returns the listener for key, if any.
func (r *Registry) Get(key ListenerKey) (*Listener, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	l, ok := r.listeners[key]
	return l, ok
}

// List returns the active listeners of user, or every user's when all is
// true, ordered by user and namespace.
func (r *Registry) List(user string, all bool) []ListenerInfo {
	r.mu.Lock()
	defer r.mu.Unlock()

	result := make([]ListenerInfo, 0, len(r.listeners))
	for _, l := range r.listeners {
		if !all && l.User != user {
			continue
		}
		result = append(result, ListenerInfo{
			Namespace: l.Namespace,
			User:      l.User,
			CreatedAt: l.CreatedAt,
		})
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].User != result[j].User {
			return result[i].User < result[j].User
		}
		return result[i].Namespace < result[j].Namespace
	})
	return result
}

// Publish queues an envelope for user's listener for the envelope's
// namespace, as a message of the bridge connection generation conn. The
// registry takes the envelope over: a request that carries an ID becomes a
// pending request and its ID is replaced, in place, with the ID the server
// minted for it (the one its answer must carry in in_reply_to). It returns
// an error, and keeps nothing, if user has no listener for the namespace
// (the message is dropped) or it would pass a limit.
//
// It never blocks without bound: when the listener's queue is full it waits
// at most publishWait, or until ctx (the publishing shed's message channel)
// ends, and then drops the message. A request whose listener disconnects
// while it waits is retained for the listener's next connection, as one sent
// to a listener that then disconnects is.
func (r *Registry) Publish(ctx context.Context, user string, conn uint64, env *Envelope) error {
	return r.publish(ctx, user, conn, env, nil)
}

// publish is Publish with live, which (when set) reports whether the route
// the message is published on is still the connection's current route. It
// is checked under r.mu where the message is made and every time the message
// has an effect afterwards (see the note at the top of this file).
func (r *Registry) publish(ctx context.Context, user string, conn uint64, env *Envelope, live func() bool) error {
	// Nothing is retained for an envelope with an over-long identifier.
	if err := checkIdentifiers(env); err != nil {
		return err
	}
	namespace := env.Namespace
	// Look up the listener and make the message under one lock, so an
	// Unregister can't interleave between them. Stale messages are swept from
	// here too (not only on Register), at most once per sweepEvery, so a
	// long-lived listener that never reconnects still keeps the retained set
	// bounded without every publish scanning it.
	r.mu.Lock()
	r.maybeSweepLocked(time.Now())
	l, ok := r.listeners[ListenerKey{User: user, Namespace: namespace}]
	if !ok {
		r.mu.Unlock()
		return fmt.Errorf("no listener registered for namespace %q%s", clip(namespace), forUser(user))
	}
	if live != nil && !live() {
		r.mu.Unlock()
		return fmt.Errorf("request to namespace %q%s: %w; dropping it", clip(namespace), forUser(user), errConnGone)
	}
	m, err := r.holdLocked(user, conn, env, live)
	r.mu.Unlock()
	if err != nil {
		return err
	}

	err = r.enqueue(ctx, l, m, publishWait, publishFullHook)
	switch {
	case err == nil:
		return nil
	case errors.Is(err, errListenerGone):
		// A request stays pending for the listener's next connection.
		if !m.tracked {
			r.free(m)
		}
		return fmt.Errorf("listener for namespace %q%s disconnected", clip(namespace), forUser(user))
	case errors.Is(err, errConnGone):
		r.free(m)
		return fmt.Errorf("request to namespace %q%s: %w; dropping it", clip(namespace), forUser(user), errConnGone)
	case errors.Is(err, ErrListenerBusy):
		r.free(m)
		return fmt.Errorf("request to namespace %q%s dropped after %s: %w", clip(namespace), forUser(user), publishWait, ErrListenerBusy)
	default:
		r.free(m)
		return fmt.Errorf("request to namespace %q%s dropped: %w", clip(namespace), forUser(user), err)
	}
}

// forUser names a named user in an error; the implicit user is not named, so
// a server with no user list words its errors as it always has.
func forUser(user string) string {
	if user == "" {
		return ""
	}
	return fmt.Sprintf(" for user %q", user)
}

// holdLocked makes the Message for an accepted envelope and counts it; the
// caller must hold r.mu. A request with a shed and an ID is tracked: it gets
// the server's ID (written into env in place of the guest's, which the
// Message keeps) and is pending until answered. A message that would pass a
// limit is refused and nothing is kept.
func (r *Registry) holdLocked(user string, conn uint64, env *Envelope, live func() bool) (*Message, error) {
	m := &Message{
		env:       env,
		user:      user,
		namespace: env.Namespace,
		conn:      conn,
		live:      live,
		tracked:   env.Type == MessageTypeRequest && env.ID != "" && env.Shed != nil && env.Shed.Name != "",
	}
	if env.Shed != nil {
		m.shed = env.Shed.Name
	}
	// Counted: the envelope as it arrived, the guest's ID once more (the
	// Message keeps it) and the server's ID that takes its place.
	size := envelopeSize(env) + len(env.ID) + requestIDLen + messageOverhead
	u := r.conns[conn]
	if u == nil {
		u = &connUsage{msgs: make(map[*Message]struct{})}
	}
	shed := clip(m.shed)
	switch {
	case len(u.msgs)+1 > maxMessagesPerConn:
		return nil, fmt.Errorf("shed %q has %d messages held on its connection (the most it may): %w; dropping its message", shed, len(u.msgs), ErrPendingLimit)
	case u.bytes+size > maxBytesPerConn:
		return nil, fmt.Errorf("shed %q holds %d bytes of messages on its connection and this one is %d (at most %d): %w; dropping it", shed, u.bytes, size, maxBytesPerConn, ErrPendingLimit)
	case r.totalBytes+size > maxBytesTotal:
		return nil, fmt.Errorf("the server holds %d bytes of messages (at most %d): %w; dropping shed %q's message", r.totalBytes, maxBytesTotal, ErrPendingLimit, shed)
	}
	if m.tracked {
		m.guestID = env.ID
		m.id = r.newRequestIDLocked()
		env.ID = m.id
		r.pending[m.id] = m
	}
	r.nextSeq++
	m.seq = r.nextSeq
	m.at = time.Now().UTC()
	m.size = size
	u.bytes += size
	u.msgs[m] = struct{}{}
	r.conns[conn] = u
	r.totalBytes += size
	return m, nil
}

// requestIDLen is the length of a server-minted request ID.
const requestIDLen = 36

// newRequestIDLocked mints the server's ID for a request: random (so it
// cannot be guessed, and an ID from before a restart never matches a new
// request) and unique among the pending requests. It has the form of a
// UUID, as the IDs listeners were sent always had. The caller holds r.mu.
func (r *Registry) newRequestIDLocked() string {
	for {
		var b [16]byte
		if _, err := rand.Read(b[:]); err != nil {
			panic("credential bus: no randomness for a request ID: " + err.Error())
		}
		b[6] = b[6]&0x0f | 0x40 // version 4
		b[8] = b[8]&0x3f | 0x80 // variant 10
		id := fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
		if _, taken := r.pending[id]; !taken {
			return id
		}
	}
}

// match returns the pending request that an answer from user, for namespace
// and shed, carrying id in in_reply_to, answers; nil when there is none: the
// ID is not a pending request's (forged, replayed, already answered,
// dropped, or from before a restart), or the request was routed to another
// user, namespace or shed. Nothing is consumed.
//
// A request older than pendingRetention is past answering: it is freed here,
// whether or not a sweep has run since it went stale, and nil is returned.
func (r *Registry) match(user, namespace, shed, id string) *Message {
	r.mu.Lock()
	defer r.mu.Unlock()
	m := r.pending[id]
	if m == nil || r.expiredLocked(m, time.Now()) {
		return nil
	}
	if m.user != user || m.namespace != namespace || m.shed != shed {
		return nil
	}
	return m
}

// admit reports whether m may still have its answer written: it is still
// pending, no older than pendingRetention (an older one is freed here), and
// the route it was published on is still its connection's. The bridge calls
// it with the transport's write lock held, immediately before the write, so
// a request answered over and over with non-final answers still stops being
// answerable once its retention has passed.
func (r *Registry) admit(m *Message) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.liveLocked(m) && !r.expiredLocked(m, time.Now())
}

// expiredLocked frees m and reports true when m is older than
// pendingRetention, the age at which a sweep would free it. The caller must
// hold r.mu.
func (r *Registry) expiredLocked(m *Message, now time.Time) bool {
	if !m.at.Before(now.Add(-pendingRetention)) {
		return false
	}
	r.freeLocked(m)
	return true
}
