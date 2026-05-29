package backend

import (
	"errors"
	"sync"
	"testing"
)

func TestCleanup_RunsInLIFO(t *testing.T) {
	var order []string
	c := NewCleanup()
	c.Register("first", func() error { order = append(order, "first"); return nil })
	c.Register("second", func() error { order = append(order, "second"); return nil })
	c.Register("third", func() error { order = append(order, "third"); return nil })
	c.Run()

	want := []string{"third", "second", "first"}
	if !equal(order, want) {
		t.Errorf("LIFO order = %v, want %v", order, want)
	}
}

func TestCleanup_CommitMakesRunNoOp(t *testing.T) {
	called := false
	c := NewCleanup()
	c.Register("must-not-run", func() error { called = true; return nil })
	c.Commit()
	c.Run()

	if called {
		t.Error("cleanup ran after Commit; should have been a no-op")
	}
}

func TestCleanup_RegisterAfterCommitIgnored(t *testing.T) {
	called := false
	c := NewCleanup()
	c.Commit()
	c.Register("late", func() error { called = true; return nil })
	c.Run()

	if called {
		t.Error("cleanup registered after Commit ran; should have been ignored")
	}
}

func TestCleanup_ContinuesPastFailure(t *testing.T) {
	var order []string
	c := NewCleanup()
	c.Register("first", func() error { order = append(order, "first"); return nil })
	c.Register("failing", func() error { order = append(order, "failing"); return errors.New("boom") })
	c.Register("third", func() error { order = append(order, "third"); return nil })
	c.Run()

	want := []string{"third", "failing", "first"}
	if !equal(order, want) {
		t.Errorf("expected to continue past a failure; order = %v, want %v", order, want)
	}
}

func TestCleanup_RunIdempotent(t *testing.T) {
	calls := 0
	c := NewCleanup()
	c.Register("once", func() error { calls++; return nil })
	c.Run()
	c.Run()
	c.Run()

	if calls != 1 {
		t.Errorf("cleanup ran %d times, want 1 (Run must be idempotent)", calls)
	}
}

func TestCleanup_PanickingStepDoesNotAbortChain(t *testing.T) {
	var order []string
	c := NewCleanup()
	c.Register("first", func() error { order = append(order, "first"); return nil })
	c.Register("panicking", func() error {
		order = append(order, "panicking")
		panic("synthetic panic from test")
	})
	c.Register("third", func() error { order = append(order, "third"); return nil })
	c.Run()

	want := []string{"third", "panicking", "first"}
	if !equal(order, want) {
		t.Errorf("panic recovery broke the LIFO chain; order = %v, want %v", order, want)
	}
}

func TestCleanup_ConcurrentRegisters(t *testing.T) {
	c := NewCleanup()
	var wg sync.WaitGroup
	const n = 50
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c.Register("concurrent", func() error { return nil })
		}()
	}
	wg.Wait()
	c.Run()
	// If the test reaches here without a race-detector trip or a
	// deadlock, Register is safely concurrent.
}

// equal compares two string slices.
func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
