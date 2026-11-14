package remote

import (
	"testing"

	"github.com/charliek/shed/internal/config"
	"github.com/charliek/shed/internal/remote/state"
)

// A job that has published its result and not yet left must not take an
// action queued for the job that replaced it: the start a client was told is
// queued behind a stop would vanish.
func TestNextStepLeavesTheQueueToTheRegisteredJob(t *testing.T) {
	e := newEnv(t)
	const name = "queue"
	e.running(name)
	id := e.settledID(name)
	old := &job{kind: state.JobStart, name: name, id: id}

	e.b.mu.Lock()
	r := e.b.records[name]
	r.Shed.Status = config.StatusStopping
	r.Job = state.JobStop
	r.Queued = state.QueuedStart
	successor := e.b.newJobLocked(state.JobStop, name, id) // registered, never launched
	e.b.mu.Unlock()
	defer func() {
		successor.cancel()
		e.b.mu.Lock()
		delete(e.b.jobs, name)
		e.b.mu.Unlock()
	}()

	if next := e.b.nextStep(old); next != "" {
		t.Fatalf("the outgoing job took step %q", next)
	}
	e.b.mu.Lock()
	queued, registered := e.b.records[name].Queued, e.b.jobs[name]
	e.b.mu.Unlock()
	if queued != state.QueuedStart {
		t.Fatalf("queued action is %q after the outgoing job looked, want %q", queued, state.QueuedStart)
	}
	if registered != successor {
		t.Fatal("the outgoing job deregistered its successor")
	}
}

// A job with nothing queued gives the shed up in the same critical section as
// the look, so an action queued right after starts its own job.
func TestNextStepWithNothingQueuedDeregistersTheJob(t *testing.T) {
	e := newEnv(t)
	const name = "leave"
	e.running(name)
	id := e.settledID(name)

	e.b.mu.Lock()
	j := e.b.newJobLocked(state.JobProbe, name, id)
	e.b.mu.Unlock()
	defer j.cancel()

	if next := e.b.nextStep(j); next != "" {
		t.Fatalf("next step %q, want none", next)
	}
	e.b.mu.Lock()
	_, still := e.b.jobs[name]
	e.b.mu.Unlock()
	if still {
		t.Fatal("the job is still registered after finding nothing to do")
	}
	// A stop asked for now gets a job of its own.
	if _, err := e.b.StopShed(ctxT(t), name); err != nil {
		t.Fatal(err)
	}
	if got := e.settled(name); got.Status != config.StatusStopped {
		t.Fatalf("settled %q, want stopped", got.Status)
	}
}
