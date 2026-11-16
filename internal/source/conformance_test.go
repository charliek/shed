package source_test

import (
	"path/filepath"
	"sync"
	"testing"

	"github.com/charliek/shed/internal/source"
	"github.com/charliek/shed/internal/source/sourcetest"
)

// TestFileBindingsBehaviour runs the shared behaviour table against the
// file store (the fake runs the same table in internal/source/fake).
func TestFileBindingsBehaviour(t *testing.T) {
	sourcetest.Run(t, sourcetest.Harness{
		New: func(t *testing.T, opts source.Options) sourcetest.Instance {
			return openFile(t, filepath.Join(t.TempDir(), source.FileName), opts)
		},
	})
}

// fileControls are the table's controls over one FileBindings.
type fileControls struct {
	mu     sync.Mutex
	fail   error
	writes int
	hook   func()
}

func openFile(t *testing.T, path string, opts source.Options) sourcetest.Instance {
	t.Helper()
	fb, _, err := source.OpenFileBindings(path, opts)
	if err != nil {
		t.Fatalf("OpenFileBindings: %v", err)
	}
	t.Cleanup(func() { _ = fb.Close() })
	c := &fileControls{}
	source.WrapWrite(fb, func(next func(string, []byte) error) func(string, []byte) error {
		return func(p string, data []byte) error {
			c.mu.Lock()
			hook, fail := c.hook, c.fail
			c.mu.Unlock()
			if hook != nil {
				hook()
			}
			if fail != nil {
				return fail
			}
			if err := next(p, data); err != nil {
				return err
			}
			c.mu.Lock()
			c.writes++
			c.mu.Unlock()
			return nil
		}
	})
	return sourcetest.Instance{
		Bindings: fb,
		FailWrites: func(err error) {
			c.mu.Lock()
			defer c.mu.Unlock()
			c.fail = err
		},
		Writes: func() int {
			c.mu.Lock()
			defer c.mu.Unlock()
			return c.writes
		},
		BeforeWrite: func(fn func()) {
			c.mu.Lock()
			defer c.mu.Unlock()
			c.hook = fn
		},
		// A restart: the old store lets go of the file's lock first.
		Reopen: func(t *testing.T) sourcetest.Instance {
			if err := fb.Close(); err != nil {
				t.Fatalf("Close: %v", err)
			}
			return openFile(t, path, opts)
		},
	}
}
