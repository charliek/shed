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

// Registry tracks active listeners by namespace. One listener per namespace.
type Registry struct {
	mu        sync.RWMutex
	listeners map[string]*Listener
}

// NewRegistry creates a new empty registry.
func NewRegistry() *Registry {
	return &Registry{
		listeners: make(map[string]*Listener),
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

// Unregister removes the listener for the given namespace.
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
	r.mu.RLock()
	l, ok := r.listeners[env.Namespace]
	r.mu.RUnlock()

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
