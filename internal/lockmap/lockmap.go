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
	mu := m.mutex(name)
	mu.Lock()
	return mu.Unlock
}

// TryAcquire takes the per-name mutex only if no one holds it, and reports
// whether it did; it never waits. On success the caller MUST call the
// returned unlock closure. It is for work that is worth doing only when it
// costs no wait and cannot deadlock against a caller that already holds the
// lock, such as a read path's opportunistic repair of a stale record.
func (m *NamedMutexMap) TryAcquire(name string) (func(), bool) {
	mu := m.mutex(name)
	if !mu.TryLock() {
		return nil, false
	}
	return mu.Unlock, true
}

// mutex returns name's mutex, making it on first use.
func (m *NamedMutexMap) mutex(name string) *sync.Mutex {
	m.guard.Lock()
	defer m.guard.Unlock()
	if m.locks == nil {
		m.locks = make(map[string]*sync.Mutex)
	}
	mu, ok := m.locks[name]
	if !ok {
		mu = &sync.Mutex{}
		m.locks[name] = mu
	}
	return mu
}

// Flags counts, per name, the callers that have raised a flag and not yet
// lowered it. A long operation that runs in steps under a NamedMutexMap
// lock checks it between steps, so a caller waiting for that lock (a stop
// or a delete of the shed) is not overtaken by the operation's next step:
// a waiter raises the flag before it waits, and the operation gives way.
// The zero value is ready to use.
type Flags struct {
	mu sync.Mutex
	n  map[string]int
}

// Raise raises name's flag and returns the closure that lowers it again.
func (f *Flags) Raise(name string) func() {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.n == nil {
		f.n = make(map[string]int)
	}
	f.n[name]++
	var once sync.Once
	return func() {
		once.Do(func() {
			f.mu.Lock()
			defer f.mu.Unlock()
			if f.n[name]--; f.n[name] <= 0 {
				delete(f.n, name)
			}
		})
	}
}

// Raised reports whether anyone holds name's flag raised.
func (f *Flags) Raised(name string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.n[name] > 0
}
