// Package fake is the test double for source.Bindings: the same rules as
// source.FileBindings (it is a source.Store), saved to memory, with a way to
// make saves fail. The shared behaviour table in internal/source/sourcetest
// runs against both, so the double cannot drift.
package fake

import (
	"sync"

	"github.com/charliek/shed/internal/source"
)

// Bindings is an in-memory source.Bindings.
type Bindings struct {
	*source.Store

	opts source.Options

	mu          sync.Mutex
	fail        error
	writes      int
	saved       []byte
	beforeWrite func()
}

var _ source.Bindings = (*Bindings)(nil)

// New returns an empty store.
func New(opts source.Options) *Bindings {
	b := &Bindings{opts: opts}
	b.Store = source.NewStore(b.persist, opts)
	return b
}

// FailWrites makes every save fail with err from now on (each change then
// fails and leaves the state as it was); nil makes saves succeed again.
func (b *Bindings) FailWrites(err error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.fail = err
}

// Writes is how many saves have succeeded.
func (b *Bindings) Writes() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.writes
}

// BeforeWrite sets a function called at the start of every save, inside
// the store's write lock (tests use it to hold a save open).
func (b *Bindings) BeforeWrite(fn func()) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.beforeWrite = fn
}

// Reopen returns a new store loaded from what this one last saved, as a
// restart would find it (empty if nothing was saved).
func (b *Bindings) Reopen() (*Bindings, error) {
	b.mu.Lock()
	saved := b.saved
	b.mu.Unlock()
	nb := &Bindings{opts: b.opts}
	if saved == nil {
		nb.Store = source.NewStore(nb.persist, b.opts)
		return nb, nil
	}
	st, err := source.LoadStore(saved, nb.persist, b.opts)
	if err != nil {
		return nil, err
	}
	nb.Store = st
	return nb, nil
}

// persist is the store's save.
func (b *Bindings) persist(data []byte) error {
	b.mu.Lock()
	hook := b.beforeWrite
	b.mu.Unlock()
	if hook != nil {
		hook()
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.fail != nil {
		return b.fail
	}
	b.writes++
	b.saved = append([]byte(nil), data...)
	return nil
}
