package fake

import (
	"errors"
	"testing"

	"github.com/charliek/shed/internal/source"
	"github.com/charliek/shed/internal/source/sourcetest"
)

// TestFakeBehaviour runs the shared behaviour table against the double (the
// file store runs the same table in internal/source).
func TestFakeBehaviour(t *testing.T) {
	sourcetest.Run(t, sourcetest.Harness{New: func(t *testing.T, opts source.Options) sourcetest.Instance {
		return instance(t, New(opts))
	}})
}

func instance(t *testing.T, b *Bindings) sourcetest.Instance {
	return sourcetest.Instance{
		Bindings:    b,
		FailWrites:  b.FailWrites,
		Writes:      b.Writes,
		BeforeWrite: b.BeforeWrite,
		Reopen: func(t *testing.T) sourcetest.Instance {
			nb, err := b.Reopen()
			if err != nil {
				t.Fatalf("Reopen: %v", err)
			}
			return instance(t, nb)
		},
	}
}

func TestFailWritesFailsAdmitAndBindsNothing(t *testing.T) {
	b := New(source.Options{})
	boom := errors.New("disk full")
	b.FailWrites(boom)
	if _, err := b.Admit("alice", sourcetest.M(1)); !errors.Is(err, boom) {
		t.Fatalf("Admit = %v; want %v", err, boom)
	}
	if _, ok := b.Get("alice"); ok {
		t.Fatal("bound despite the failed write")
	}
	b.FailWrites(nil)
	if d, err := b.Admit("alice", sourcetest.M(1)); err != nil || !d.Admitted {
		t.Fatalf("Admit after clearing = %+v, %v", d, err)
	}
	if b.Writes() != 1 {
		t.Fatalf("writes = %d; want 1", b.Writes())
	}
}
