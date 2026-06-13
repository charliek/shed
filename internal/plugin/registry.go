package plugin

import (
	"fmt"
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

// Registry tracks active listeners by namespace. One listener per namespace.
type Registry struct {
	mu        sync.RWMutex
	listeners map[string]*Listener
	// pending records requests dispatched to a listener that have not yet been
	// answered. /respond is validated against this set so a credentials-token
	// holder cannot forge a response for a request it did not receive (only the
	// sole registered listener for a namespace is delivered the request, and
	// hence its unguessable requestID). Entries are added on delivery, removed
	// on the final response, and swept when the namespace's listener unregisters.
	pending map[pendingKey]struct{}
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
		pending:   make(map[pendingKey]struct{}),
	}
}

// Register creates a listener for the given namespace. Returns an error if
// the namespace is already registered or is reserved.
func (r *Registry) Register(namespace string) (*Listener, error) {
	if err := ValidateNamespace(namespace); err != nil {
		return nil, err
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	if _, exists := r.listeners[namespace]; exists {
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
	return l, nil
}

// Unregister removes the listener for the given namespace and discards any of
// its still-pending requests, so a response that arrives after the listener is
// gone (or after a different listener reclaims the namespace) is not honored.
func (r *Registry) Unregister(namespace string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if l, exists := r.listeners[namespace]; exists {
		close(l.done)
		delete(r.listeners, namespace)
	}
	// Linear sweep is fine at credential-bus volumes (a handful of in-flight
	// requests); revisit with a per-namespace index only if pending grows large.
	for k := range r.pending {
		if k.namespace == namespace {
			delete(r.pending, k)
		}
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
	// after the lock is released.
	r.mu.Lock()
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
func (r *Registry) trackPendingLocked(env *Envelope) {
	if !r.ownershipTracking {
		return
	}
	if env.Type != MessageTypeRequest || env.ID == "" || env.Shed == nil || env.Shed.Name == "" {
		return
	}
	r.pending[pendingKey{namespace: env.Namespace, shed: env.Shed.Name, requestID: env.ID}] = struct{}{}
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
