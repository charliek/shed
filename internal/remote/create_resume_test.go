package remote

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/charliek/shed/internal/config"
	"github.com/charliek/shed/internal/remote/provider"
	"github.com/charliek/shed/internal/remote/provider/fake"
	"github.com/charliek/shed/internal/remote/state"
)

// uncommit puts a running shed's record back to where a create leaves it just
// before its last local write: the cloud says shed_state=ready, the record
// still says "create in flight".
func (e *env) uncommit(name string) string {
	e.t.Helper()
	id := e.settledID(name)
	if !e.b.update(name, id, func(r *state.Record) {
		r.Shed.Status = config.StatusStarting
		r.Job = state.JobCreate
		r.Ready = false
	}) {
		e.t.Fatalf("no record for %s", name)
	}
	return id
}

// A server that dies between a create's ready label and its last local write
// leaves a finished shed. The next server must not resume the create: a
// resumed create whose VM has stopped meanwhile fails, and a failed create is
// cleaned up by destroying the VM and its disk.
func TestRestartNeverDestroysAShedTheCloudCallsReady(t *testing.T) {
	tests := []struct {
		name     string
		stop     bool
		unlisted bool
		want     string
	}{
		{"the VM stopped while the server was down", true, false, config.StatusStopped},
		{"the VM still runs", false, false, config.StatusRunning},
		{"the VM stopped and the list does not show it", true, true, config.StatusStopped},
		{"the VM runs and the list does not show it", false, true, config.StatusRunning},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			h := &hookProvider{}
			e := newEnv(t, hooked(h))
			const name = "done"
			e.running(name)
			id := e.uncommit(name)
			ref := e.ref(name)
			if tt.stop {
				if err := e.prov.ExternalStop(ref); err != nil {
					t.Fatal(err)
				}
			}
			if tt.unlisted {
				h.list = func(context.Context, provider.Selector) ([]provider.Instance, error) { return nil, nil }
			}
			e.restart()
			got := e.settled(name)
			e.idle()
			if got.Status != tt.want || got.ID != id {
				t.Fatalf("came to rest %q (%q) id %s, want %q id %s", got.Status, got.StatusReason, got.ID, tt.want, id)
			}
			if n := e.prov.Count(fake.OpDestroy); n != 0 {
				t.Fatalf("%d destroy calls on a finished shed, want none", n)
			}
			if n := e.prov.Count(fake.OpCreate); n != 1 {
				t.Fatalf("%d create calls, want the original one only", n)
			}
			if e.instances() != 1 || markerCount(t, e.dir) != 0 {
				t.Fatalf("%d instances and %d delete markers, want 1 and 0", e.instances(), markerCount(t, e.dir))
			}
			rec := e.b.snapshot(name, id)
			if !rec.Ready || rec.Job != "" {
				t.Fatalf("record Ready=%v Job=%q, want a finished shed with no job", rec.Ready, rec.Job)
			}
		})
	}
}

// A create resumed after a restart goes back to the instance it was given. It
// does not send the insert again while that instance exists: a server that
// came back with another zone would place a second instance there and lose
// the first.
func TestResumedCreateDoesNotInsertASecondInstance(t *testing.T) {
	h := &hookProvider{}
	e := newEnv(t, manual(), hooked(h))
	const name = "resume"
	e.put(name, config.StatusStarting)
	id := e.settledID(name)
	deadline := time.Now().Add(testTimeout)
	for e.b.snapshot(name, id).Ref == "" {
		if time.Now().After(deadline) {
			t.Fatal("the create never recorded its instance")
		}
		time.Sleep(2 * time.Millisecond)
	}
	// The new server lists another zone: the instance is not in the list.
	h.list = func(context.Context, provider.Selector) ([]provider.Instance, error) { return nil, nil }
	h.create = func(context.Context, provider.Spec) (provider.Pending, error) {
		t.Error("the resumed create sent a second insert")
		return provider.Pending{}, errors.New("no second insert")
	}
	e.restart()
	e.auto()
	if got := e.settled(name); got.Status != config.StatusRunning || got.ID != id {
		t.Fatalf("came to rest %q (%q), want running", got.Status, got.StatusReason)
	}
	if n := e.prov.Count(fake.OpDestroy); n != 0 {
		t.Fatalf("%d destroy calls, want none", n)
	}
}

// When the instance of an unfinished create is gone, the resumed create makes
// it again.
func TestResumedCreateInsertsAgainWhenItsInstanceIsGone(t *testing.T) {
	e := newEnv(t, manual())
	const name = "again"
	e.put(name, config.StatusStarting)
	id := e.settledID(name)
	deadline := time.Now().Add(testTimeout)
	for e.b.snapshot(name, id).Ref == "" {
		if time.Now().After(deadline) {
			t.Fatal("the create never recorded its instance")
		}
		time.Sleep(2 * time.Millisecond)
	}
	if err := e.b.Close(); err != nil {
		t.Fatal(err)
	}
	e.prov.ExternalDelete(e.ref(name))
	e.b = e.open()
	e.auto()
	if got := e.settled(name); got.Status != config.StatusRunning || got.ID != id {
		t.Fatalf("came to rest %q (%q), want running", got.Status, got.StatusReason)
	}
	if n := e.prov.Count(fake.OpCreate); n != 2 {
		t.Fatalf("%d create calls, want 2", n)
	}
}
