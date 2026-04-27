// Package lockmap provides per-name mutex serialization.
package lockmap

import "sync"

// NamedMutexMap serializes operations keyed by name. Each name gets its own
// sync.Mutex; a single guard mutex protects the underlying map. The per-name
// mutex is leaked on first use — bounded by the number of distinct names ever
// seen in this process, which is effectively negligible for shed.
//
// The zero value is ready to use; New() is offered for callers that prefer
// explicit construction.
type NamedMutexMap struct {
	guard sync.Mutex
	locks map[string]*sync.Mutex
}

// New returns a ready-to-use NamedMutexMap. Equivalent to using the zero value.
func New() *NamedMutexMap {
	return &NamedMutexMap{}
}

// Acquire takes the per-name mutex and returns an unlock closure. Callers
// MUST defer the returned closure.
//
// Lock-order rule when callers hold multiple NamedMutexMaps: the rule is
// documented at the type level by the embedding caller (see e.g.
// internal/vz/client.go's acquireSnapshotLock vs acquireCreateLock comments).
// AB-BA deadlock between distinct NamedMutexMaps is the caller's responsibility.
func (m *NamedMutexMap) Acquire(name string) func() {
	m.guard.Lock()
	if m.locks == nil {
		m.locks = make(map[string]*sync.Mutex)
	}
	mu, ok := m.locks[name]
	if !ok {
		mu = &sync.Mutex{}
		m.locks[name] = mu
	}
	m.guard.Unlock()
	mu.Lock()
	return mu.Unlock
}
