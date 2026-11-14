package remote

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/charliek/shed/internal/backend"
	"github.com/charliek/shed/internal/config"
	"github.com/charliek/shed/internal/remote/provider/fake"
)

// A delete whose destroy failed is not reported as a finished delete: the VM
// still exists (and bills) until the periodic pass destroys it. The waiter
// gets ErrDeleteOwed whether it was waiting on the job or arrived after it.
func TestWaitSettledDoesNotCallAFailedDeleteGone(t *testing.T) {
	e := newEnv(t)
	const name = "stuck"
	e.running(name)
	e.prov.FailNext(fake.OpDestroy, errors.New("the cloud is having a moment"))
	if err := e.b.DeleteShed(ctxT(t), name); err != nil {
		t.Fatal(err)
	}
	if _, err := e.b.WaitSettled(ctxT(t), name); !errors.Is(err, ErrDeleteOwed) || errors.Is(err, config.ErrShedNotFoundSentinel) {
		t.Fatalf("WaitSettled after a failed destroy = %v, want ErrDeleteOwed", err)
	}
	e.idle()
	if e.instances() != 1 {
		t.Fatal("the instance should still exist")
	}
	// After the job has gone, with its marker standing, the answer is the same.
	if _, err := e.b.WaitSettled(ctxT(t), name); !errors.Is(err, ErrDeleteOwed) {
		t.Fatalf("WaitSettled with the marker standing = %v, want ErrDeleteOwed", err)
	}
	// Another client's view of the name is unchanged: nothing there.
	other := backend.WithRequiredOwner(ctxT(t), "someone-else")
	if _, err := e.b.WaitSettled(other, name); !errors.Is(err, config.ErrShedNotFoundSentinel) {
		t.Fatalf("WaitSettled by another client = %v, want not found", err)
	}

	e.sweep()
	e.idle()
	if e.instances() != 0 {
		t.Fatal("the periodic pass did not finish the delete")
	}
	if _, err := e.b.WaitSettled(ctxT(t), name); !errors.Is(err, config.ErrShedNotFoundSentinel) {
		t.Fatalf("WaitSettled after the delete finished = %v, want not found", err)
	}
}

// A wait bound to one incarnation of a name, as the SSE create binds it, ends
// when that shed is deleted and never follows a shed created under the name
// afterwards. The replacement by the same owner is the case only the creation
// time tells apart: the owner check alone lets it through.
func TestWaitSettledBoundToAnIncarnationDoesNotFollowAReplacement(t *testing.T) {
	tests := []struct {
		name        string
		replacement string // the replacement's owner; the first shed is alice's
	}{
		{"replaced by another owner", "bob"},
		{"replaced by the same owner", "alice"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clock := newTestClock()
			e := newEnv(t, manual(), withClock(clock))
			const name = "reused"
			first, err := e.b.CreateShed(ctxT(t), config.CreateShedRequest{Name: name, NoProvision: true, Owner: "alice"})
			if err != nil {
				t.Fatal(err)
			}
			bound := backend.WithRequiredCreatedAt(backend.WithRequiredOwner(ctxT(t), first.Owner), first.CreatedAt)

			if err := e.b.DeleteShed(context.Background(), name); err != nil {
				t.Fatal(err)
			}
			e.auto()
			e.idle()
			e.stopAuto()
			clock.Advance(time.Second)
			second, err := e.b.CreateShed(ctxT(t), config.CreateShedRequest{Name: name, NoProvision: true, Owner: tt.replacement})
			if err != nil {
				t.Fatal(err)
			}
			if second.ID == first.ID {
				t.Fatal("the replacement has the first shed's ID")
			}
			if second.CreatedAt.Equal(first.CreatedAt) {
				t.Fatal("the replacement has the first shed's creation time")
			}
			if shed, err := e.b.WaitSettled(bound, name); !errors.Is(err, config.ErrShedNotFoundSentinel) {
				t.Fatalf("a wait bound to the first shed = %+v, %v; want not found", shed, err)
			}
			// The replacement itself is there for a caller not bound to the
			// first one.
			owner := backend.WithRequiredOwner(ctxT(t), tt.replacement)
			if shed, err := e.b.GetShed(owner, name); err != nil || shed.ID != second.ID {
				t.Fatalf("the replacement's owner reads %+v, %v; want shed %s", shed, err, second.ID)
			}
		})
	}
}
