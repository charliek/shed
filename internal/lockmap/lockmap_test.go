package lockmap

import (
	"sync"
	"testing"
	"time"
)

func TestAcquireSerializesSameName(t *testing.T) {
	tests := []struct {
		name        string
		firstName   string
		secondName  string
		shouldBlock bool
	}{
		{"same name serializes", "snap", "snap", true},
		{"different names do not block", "a", "b", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := New()
			release1 := m.Acquire(tt.firstName)

			acquired := make(chan struct{})
			go func() {
				release2 := m.Acquire(tt.secondName)
				close(acquired)
				release2()
			}()

			if tt.shouldBlock {
				select {
				case <-acquired:
					release1()
					t.Fatal("second Acquire should have blocked")
				case <-time.After(100 * time.Millisecond):
					// expected: still blocked
				}
				release1()
				select {
				case <-acquired:
					// expected: unblocked after release
				case <-time.After(time.Second):
					t.Fatal("second Acquire did not proceed after release")
				}
			} else {
				defer release1()
				select {
				case <-acquired:
					// expected: parallel acquire
				case <-time.After(500 * time.Millisecond):
					t.Fatal("Acquire for different names must not block")
				}
			}
		})
	}
}

func TestZeroValueReady(t *testing.T) {
	// A zero-value NamedMutexMap must be usable without calling New().
	var m NamedMutexMap
	release := m.Acquire("x")
	release()
}

func TestUnlockClosureReleases(t *testing.T) {
	m := New()
	release := m.Acquire("x")
	release()

	// Should be able to acquire again immediately.
	done := make(chan struct{})
	go func() {
		r2 := m.Acquire("x")
		r2()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("re-acquire after unlock blocked")
	}
}

func TestTwoMapsConsistentOrderNoDeadlock(t *testing.T) {
	a := New()
	b := New()

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		ra := a.Acquire("k")
		rb := b.Acquire("k")
		rb()
		ra()
	}()
	go func() {
		defer wg.Done()
		ra := a.Acquire("k")
		rb := b.Acquire("k")
		rb()
		ra()
	}()

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("consistent-order acquires deadlocked")
	}
}
