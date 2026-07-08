package plugin

import (
	"fmt"
	"sort"
	"sync"
	"time"
)

// Listener represents an active host-side listener subscribed to a namespace.
type Listener struct {
	Namespace string
	Messages  <-chan *Envelope // read-only; written by Registry.Publish
	Done      <-chan struct{}  // closed when Unregister is called
	CreatedAt time.Time

	messages chan *Envelope // internal write side
	done     chan struct{}  // internal close side
}

// pendingKey identifies an outstanding request awaiting a response. The
// requestID is the request envelope's (UUIDv7) ID; a response references it via
// InReplyTo. Keying on the shed too ensures a response can only route back to
// the shed that issued the request.
type pendingKey struct {
	namespace string
	shed      string
	requestID string
}

// pendingReq is an un-answered request, retained both for /respond ownership
// validation and for re-delivery to a reconnecting listener. The full envelope
// is kept so Register can re-send it; at bounds the leak via sweepStalePendingLocked.
type pendingReq struct {
	env *Envelope
	at  time.Time
}

// pendingRetention bounds how long an un-answered request is retained (for
// re-delivery) before it is swept as abandoned. Generous on purpose — far longer
// than any credential request's own client-side timeout, so the sweep only
// reclaims genuine leaks, never a still-valid slow approval. A var so tests can
// shrink it.
var pendingRetention = 1 * time.Hour

// Registry tracks active listeners by namespace. One listener per namespace.
type Registry struct {
	mu        sync.RWMutex
	listeners map[string]*Listener
	// pending records requests dispatched to a listener that have not yet been
	// answered. /respond is validated against this set so a credentials-token
	// holder cannot forge a response for a request it did not receive (only the
	// sole registered listener for a namespace is delivered the request, and
	// hence its unguessable requestID). Entries are added on delivery, removed
	// on the final response, RETAINED across a listener disconnect so a
	// reconnecting listener gets them re-delivered, and swept once stale.
	pending map[pendingKey]pendingReq
	// ownershipTracking gates whether pending is populated at all. Off by
	// default, so a server without HTTP auth does zero bookkeeping (the
	// ownership gate is only consulted when auth is enforced); EnableOwnership-
	// Tracking turns it on at startup.
	ownershipTracking bool
}

// NewRegistry creates a new empty registry.
func NewRegistry() *Registry {
	return &Registry{
		listeners: make(map[string]*Listener),
		pending:   make(map[pendingKey]pendingReq),
	}
}

// Register creates a listener for the given namespace. Returns an error if
// the namespace is already registered or is reserved.
func (r *Registry) Register(namespace string) (*Listener, error) {
	if err := ValidateNamespace(namespace); err != nil {
		return nil, err
	}

	r.mu.Lock()
	if _, exists := r.listeners[namespace]; exists {
		r.mu.Unlock()
		return nil, fmt.Errorf("namespace %q is already registered", namespace)
	}

	msgs := make(chan *Envelope, 32)
	done := make(chan struct{})
	l := &Listener{
		Namespace: namespace,
		Messages:  msgs,
		Done:      done,
		CreatedAt: time.Now().UTC(),
		messages:  msgs,
		done:      done,
	}
	r.listeners[namespace] = l

	// A reconnecting listener inherits the namespace's still-un-answered
	// requests, so an approval in flight when the previous connection dropped is
	// not lost. Sweep stale entries first (bounds the retained set), then collect
	// this namespace's pending for re-delivery after the lock is released.
	r.sweepStalePendingLocked()
	redeliver := r.pendingEnvelopesLocked(namespace)
	r.mu.Unlock()

	// Re-deliver ASYNCHRONOUSLY. The caller (handlePluginSubscribe) only starts
	// reading l.Messages after Register returns, so sending inline here would
	// block once the 32-buffer fills — and a backlog larger than the buffer would
	// wedge forever, because nothing closes done until the handler installs its
	// deferred Unregister. The goroutine instead drains into the live stream as
	// the handler reads, and bails if the listener is torn down (done) — which the
	// handler's deferred Unregister guarantees on any exit, so it cannot leak.
	if len(redeliver) > 0 {
		go redeliverPending(msgs, done, redeliver)
	}
	return l, nil
}

// redeliverPending streams a freshly (re)registered listener's retained requests
// into its channel, stopping early if the listener disconnects (done). Run in its
// own goroutine so Register never blocks on a backlog the handler hasn't begun
// reading yet.
func redeliverPending(msgs chan<- *Envelope, done <-chan struct{}, envs []*Envelope) {
	for _, env := range envs {
		select {
		case msgs <- env:
		case <-done:
			return
		}
	}
}

// pendingEnvelopesLocked returns a namespace's un-answered request envelopes,
// oldest first (deterministic re-delivery order). The caller must hold r.mu.
func (r *Registry) pendingEnvelopesLocked(namespace string) []*Envelope {
	var ps []pendingReq
	for k, p := range r.pending {
		if k.namespace == namespace {
			ps = append(ps, p)
		}
	}
	if len(ps) == 0 {
		return nil
	}
	sort.Slice(ps, func(i, j int) bool { return ps[i].at.Before(ps[j].at) })
	envs := make([]*Envelope, len(ps))
	for i, p := range ps {
		envs[i] = p.env
	}
	return envs
}

// sweepStalePendingLocked drops pending entries older than pendingRetention
// across all namespaces. The caller must hold r.mu. Called on every Publish AND
// every Register — i.e. on every pending mutation — deliberately NOT from a
// background sweeper (unlike authtoken.StartSweeper): so any ongoing request
// traffic OR listener churn keeps the set bounded and the effective
// response-acceptance window at ~pendingRetention, without the goroutine + ctx
// the Registry doesn't carry. The only un-swept case is a namespace gone fully
// idle (no further Publish anywhere, no Register anywhere); that retention is
// frozen and bounded by its last burst (not a growing leak), so a background
// sweeper isn't warranted.
func (r *Registry) sweepStalePendingLocked() {
	cutoff := time.Now().Add(-pendingRetention)
	for k, p := range r.pending {
		if p.at.Before(cutoff) {
			delete(r.pending, k)
		}
	}
}

// Unregister removes the listener for the given namespace. Its still-un-answered
// pending requests are RETAINED (not discarded) so a reconnecting listener has
// them re-delivered on the next Register; they are bounded by
// sweepStalePendingLocked and cleared by the final response.
//
// Honoring a response after the original listener is gone is safe: ConsumeResponse
// still matches only the unguessable per-request ID, and one-listener-per-namespace
// (the 409 on a second Register) means whoever reclaims the namespace already holds
// its credentials token — i.e. is already authorized to answer its requests. This
// is an intentional relaxation of the prior discard-on-disconnect behavior (a
// reconnecting credentials holder can now complete an approval that was in flight
// when its connection dropped), surfaced for the PR review.
func (r *Registry) Unregister(namespace string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if l, exists := r.listeners[namespace]; exists {
		close(l.done)
		delete(r.listeners, namespace)
	}
}

// Get returns the listener for the given namespace, if any.
func (r *Registry) Get(namespace string) (*Listener, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	l, ok := r.listeners[namespace]
	return l, ok
}

// List returns info about all active listeners.
func (r *Registry) List() []ListenerInfo {
	r.mu.RLock()
	defer r.mu.RUnlock()

	result := make([]ListenerInfo, 0, len(r.listeners))
	for _, l := range r.listeners {
		result = append(result, ListenerInfo{
			Namespace: l.Namespace,
			CreatedAt: l.CreatedAt,
		})
	}
	return result
}

// Publish delivers an envelope to the listener registered for the envelope's
// namespace. Returns an error if no listener is registered.
func (r *Registry) Publish(env *Envelope) error {
	// Look up the listener and record the pending request under one lock, so an
	// Unregister can't interleave between them and orphan a pending entry that a
	// reconnecting listener could then answer. The blocking channel send happens
	// after the lock is released. Sweep here too (not only on Register): request
	// traffic is the most frequent pending mutation, so it keeps the retained set
	// (and the effective response-acceptance TTL) bounded even for a long-lived
	// listener that never reconnects.
	r.mu.Lock()
	r.sweepStalePendingLocked()
	l, ok := r.listeners[env.Namespace]
	if ok {
		r.trackPendingLocked(env)
	}
	r.mu.Unlock()

	if !ok {
		return fmt.Errorf("no listener registered for namespace %q", env.Namespace)
	}

	// Check disconnect first to avoid delivering to a closing listener.
	select {
	case <-l.done:
		return fmt.Errorf("listener for namespace %q disconnected", env.Namespace)
	default:
	}

	select {
	case l.messages <- env:
		return nil
	case <-l.done:
		return fmt.Errorf("listener for namespace %q disconnected", env.Namespace)
	}
}

// EnableOwnershipTracking turns on pending-request tracking so /respond can be
// validated against it. Off by default — a server without HTTP auth does no
// bookkeeping. Call once at startup, before the registry handles traffic.
func (r *Registry) EnableOwnershipTracking() {
	r.mu.Lock()
	r.ownershipTracking = true
	r.mu.Unlock()
}

// trackPendingLocked records a delivered request as awaiting a response; the
// caller must hold r.mu. No-op unless ownership tracking is enabled. Only
// requests with a shed + ID are tracked (events and ID-less messages get no
// response).
//
// Re-delivery on reconnect (Register) rides on this same pending set, so it
// inherits the ownership gate: an open-mode server (no HTTP auth → no tracking)
// does not re-deliver in-flight requests on reconnect. That durability gap is an
// accepted MVP consequence of the deliberate "no bookkeeping without auth"
// design, not an oversight — decoupling re-delivery from the auth gate is a
// future revisit.
func (r *Registry) trackPendingLocked(env *Envelope) {
	if !r.ownershipTracking {
		return
	}
	if env.Type != MessageTypeRequest || env.ID == "" || env.Shed == nil || env.Shed.Name == "" {
		return
	}
	r.pending[pendingKey{namespace: env.Namespace, shed: env.Shed.Name, requestID: env.ID}] = pendingReq{
		env: env,
		at:  time.Now().UTC(),
	}
}

// ConsumeResponse reports whether (namespace, shed, requestID) matches an
// outstanding request dispatched to the current listener. When final, the
// pending entry is consumed so the same request cannot be answered twice.
// requestID is the response's InReplyTo. A false result means the response does
// not correspond to any request this listener was asked to handle (forged,
// replayed, or for a request whose listener has since been replaced).
func (r *Registry) ConsumeResponse(namespace, shed, requestID string, final bool) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	key := pendingKey{namespace: namespace, shed: shed, requestID: requestID}
	if _, ok := r.pending[key]; !ok {
		return false
	}
	if final {
		delete(r.pending, key)
	}
	return true
}
