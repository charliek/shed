package remote

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
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

// After a destroy failed and its job ended, the delete marker on disk is the
// only thing that says the delete is unfinished. When the markers cannot be
// read, the server does not know, and "not known" is not "gone": a waiter (a
// streamed delete) must get an error, never the not-found it reads as a
// finished delete while the VM still exists.
func TestWaitSettledDoesNotCallADeleteGoneWhenTheMarkersCannotBeRead(t *testing.T) {
	e := newEnv(t)
	const name = "stuck"
	e.running(name)
	e.prov.FailNext(fake.OpDestroy, errors.New("the cloud is having a moment"))
	if err := e.b.DeleteShed(ctxT(t), name); err != nil {
		t.Fatal(err)
	}
	e.idle()
	if e.instances() != 1 || markerCount(t, e.dir) != 1 {
		t.Fatalf("%d instances and %d markers, want the VM and its marker left by the failed destroy", e.instances(), markerCount(t, e.dir))
	}

	// The marker directory is moved aside: reading it fails, for root too.
	markers, aside := filepath.Join(e.dir, "deleting"), filepath.Join(e.dir, "deleting.aside")
	if err := os.Rename(markers, aside); err != nil {
		t.Fatal(err)
	}
	_, err := e.b.WaitSettled(ctxT(t), name)
	if rerr := os.Rename(aside, markers); rerr != nil {
		t.Fatal(rerr)
	}
	if err == nil || errors.Is(err, config.ErrShedNotFoundSentinel) {
		t.Fatalf("WaitSettled with unreadable markers = %v, want an error that is not not-found: the VM still exists", err)
	}
	if !errors.Is(err, ErrDeleteUnknown) || errors.Is(err, ErrDeleteOwed) {
		t.Fatalf("WaitSettled with unreadable markers = %v, want ErrDeleteUnknown", err)
	}
	if strings.Contains(err.Error(), e.dir) {
		t.Fatalf("the error repeats the server's state path to the caller: %v", err)
	}

	// Readable again, the answers are the usual ones.
	if _, err := e.b.WaitSettled(ctxT(t), name); !errors.Is(err, ErrDeleteOwed) {
		t.Fatalf("WaitSettled with the marker standing = %v, want ErrDeleteOwed", err)
	}
	e.sweep()
	e.idle()
	if _, err := e.b.WaitSettled(ctxT(t), name); !errors.Is(err, config.ErrShedNotFoundSentinel) {
		t.Fatalf("WaitSettled after the delete finished = %v, want not found", err)
	}
}

// One marker file that cannot be parsed is left out by the store without an
// error. It is still the only evidence of its unfinished delete, so the wait
// must not answer not-found on the strength of a list it is missing from.
func TestWaitSettledDoesNotCallADeleteGoneWhenItsMarkerFileCannotBeRead(t *testing.T) {
	e := newEnv(t)
	const name = "stuck"
	e.running(name)
	e.prov.FailNext(fake.OpDestroy, errors.New("the cloud is having a moment"))
	if err := e.b.DeleteShed(ctxT(t), name); err != nil {
		t.Fatal(err)
	}
	e.idle()
	if e.instances() != 1 || markerCount(t, e.dir) != 1 {
		t.Fatalf("%d instances and %d markers, want the VM and its marker left by the failed destroy", e.instances(), markerCount(t, e.dir))
	}

	// The marker file's content is replaced by something that does not
	// parse: unreadable for root too, and the directory still lists.
	entries, err := os.ReadDir(filepath.Join(e.dir, "deleting"))
	if err != nil || len(entries) != 1 {
		t.Fatalf("marker directory: %v, %d entries", err, len(entries))
	}
	path := filepath.Join(e.dir, "deleting", entries[0].Name())
	good, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = e.b.WaitSettled(ctxT(t), name)
	if werr := os.WriteFile(path, good, 0o600); werr != nil {
		t.Fatal(werr)
	}
	if !errors.Is(err, ErrDeleteUnknown) || errors.Is(err, config.ErrShedNotFoundSentinel) {
		t.Fatalf("WaitSettled with an unreadable marker file = %v, want ErrDeleteUnknown: the VM still exists", err)
	}
	if strings.Contains(err.Error(), e.dir) {
		t.Fatalf("the error repeats the server's state path to the caller: %v", err)
	}

	// Readable again, the answers are the usual ones.
	if _, err := e.b.WaitSettled(ctxT(t), name); !errors.Is(err, ErrDeleteOwed) {
		t.Fatalf("WaitSettled with the marker standing = %v, want ErrDeleteOwed", err)
	}
	e.sweep()
	e.idle()
	if _, err := e.b.WaitSettled(ctxT(t), name); !errors.Is(err, config.ErrShedNotFoundSentinel) {
		t.Fatalf("WaitSettled after the delete finished = %v, want not found", err)
	}
}
