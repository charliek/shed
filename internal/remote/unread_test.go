package remote

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/charliek/shed/internal/config"
	"github.com/charliek/shed/internal/remote/provider"
	"github.com/charliek/shed/internal/remote/provider/fake"
	"github.com/charliek/shed/internal/remote/state"
)

var errCloudDown = errors.New("503 backend error: the cloud is having a moment")

// waitRef waits for a create in flight to record its instance.
func (e *env) waitRef(name string) string {
	e.t.Helper()
	id := e.settledID(name)
	deadline := time.Now().Add(testTimeout)
	for e.b.snapshot(name, id).Ref == "" {
		if time.Now().After(deadline) {
			e.t.Fatal("the create never recorded its instance")
		}
		time.Sleep(2 * time.Millisecond)
	}
	return id
}

// A create resumed after a restart reads its instance first. When that read
// fails the server knows nothing about the instance, and must decide nothing
// from it: failing the create writes a clean-up marker, and the clean-up
// destroys the VM, which may be a finished shed (labelled ready by the server
// that died). The create is kept, nothing is changed in the cloud, and the
// periodic pass looks again.
func TestResumedCreateIsKeptWhenItsInstanceCannotBeRead(t *testing.T) {
	tests := []struct {
		name string
		// finished: the cloud calls the shed ready and the list does not show
		// it (a server back with another zone), so only the read can tell.
		finished bool
	}{
		{"an unfinished create", false},
		{"a create the cloud calls finished", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := &hookProvider{}
			var e *env
			var id string
			const name = "kept"
			if tt.finished {
				e = newEnv(t, hooked(h))
				e.running(name)
				id = e.uncommit(name)
				h.list = func(context.Context, provider.Selector) ([]provider.Instance, error) { return nil, nil }
			} else {
				e = newEnv(t, manual(), hooked(h))
				e.put(name, config.StatusStarting)
				id = e.waitRef(name)
			}
			creates := e.count(fake.OpCreate)

			// The next server's first read of the instance fails, and so do
			// two more: the cloud is down for a while.
			if err := e.b.Close(); err != nil {
				t.Fatal(err)
			}
			for range 3 {
				e.prov.FailNext(fake.OpGet, errCloudDown)
			}
			e.b = e.open()
			e.idle()

			check := func(when string) {
				t.Helper()
				if n := e.count(fake.OpDestroy); n != 0 {
					t.Fatalf("%s: %d destroy calls, want none", when, n)
				}
				if n := markerCount(t, e.dir); n != 0 {
					t.Fatalf("%s: %d delete markers, want none: nothing may be queued for destruction on a failed read", when, n)
				}
				if e.instances() != 1 {
					t.Fatalf("%s: %d instances, want the one", when, e.instances())
				}
				if n := e.count(fake.OpCreate) - creates; n != 0 {
					t.Fatalf("%s: %d more inserts, want none", when, n)
				}
			}
			check("after the failed read")
			rec := e.b.snapshot(name, id)
			if rec == nil || rec.Shed.Status != config.StatusStarting || rec.Job != state.JobCreate || rec.Shed.StatusReason != config.StatusReasonCloudUnreadable {
				t.Fatalf("after the failed read the record is %+v, want starting (%s) with its create kept", rec, config.StatusReasonCloudUnreadable)
			}

			// It does not spin: one read for each look, and a look only when
			// the periodic pass, a start call or a restart asks for one.
			gets := e.count(fake.OpGet)
			time.Sleep(50 * time.Millisecond)
			if n := e.count(fake.OpGet) - gets; n != 0 {
				t.Fatalf("%d reads with nobody asking, want none", n)
			}
			e.sweep()
			e.idle()
			check("after a periodic pass that still could not read it")
			if n := e.count(fake.OpGet) - gets; n != 1 {
				t.Fatalf("%d reads for one periodic pass, want 1", n)
			}
			if _, err := e.b.StartShed(ctxT(t), name); err != nil {
				t.Fatalf("a start call on a kept create: %v", err)
			}
			e.idle()
			check("after a start call that still could not read it")

			// The cloud answers again: the next pass finishes the job.
			if !tt.finished {
				e.auto()
			}
			e.sweep()
			got := e.settled(name)
			e.idle()
			if got.Status != config.StatusRunning || got.StatusReason != "" || got.ID != id {
				t.Fatalf("came to rest %q (%q), want running", got.Status, got.StatusReason)
			}
			check("once the cloud answered")
			if rec := e.b.snapshot(name, id); !rec.Ready || rec.Job != "" {
				t.Fatalf("record Ready=%v Job=%q, want a finished shed", rec.Ready, rec.Job)
			}
		})
	}
}

// A kept create never blocks its name or its VM for good: a delete ends it
// like any other shed, and the VM goes with it.
func TestKeptCreateCanBeDeleted(t *testing.T) {
	e := newEnv(t, manual())
	const name = "kept"
	e.put(name, config.StatusStarting)
	e.waitRef(name)
	if err := e.b.Close(); err != nil {
		t.Fatal(err)
	}
	e.prov.FailNext(fake.OpGet, errCloudDown)
	e.b = e.open()
	e.idle()
	if got := e.reason(name); got != config.StatusReasonCloudUnreadable {
		t.Fatalf("reason %q, want %s", got, config.StatusReasonCloudUnreadable)
	}
	e.auto()
	if err := e.b.DeleteShed(ctxT(t), name); err != nil {
		t.Fatal(err)
	}
	e.idle()
	if e.instances() != 0 || markerCount(t, e.dir) != 0 {
		t.Fatalf("%d instances and %d markers after the delete, want none", e.instances(), markerCount(t, e.dir))
	}
	if _, err := e.create(name); err != nil {
		t.Fatalf("the name is not free after the delete: %v", err)
	}
}

// A stop asked for while a create is kept waits for the create: it must not
// turn the unfinished create into a stopped shed.
func TestStopQueuedOnAKeptCreateWaitsForIt(t *testing.T) {
	e := newEnv(t, manual())
	const name = "kept"
	e.put(name, config.StatusStarting)
	id := e.waitRef(name)
	if err := e.b.Close(); err != nil {
		t.Fatal(err)
	}
	e.prov.FailNext(fake.OpGet, errCloudDown)
	e.prov.FailNext(fake.OpGet, errCloudDown)
	e.b = e.open()
	e.idle()
	if _, err := e.b.StopShed(ctxT(t), name); err != nil {
		t.Fatalf("stop on a kept create: %v", err)
	}
	e.idle()
	if n := e.count(fake.OpStop); n != 0 {
		t.Fatalf("%d stop calls while the create is unfinished, want none", n)
	}
	rec := e.b.snapshot(name, id)
	if rec.Shed.Status != config.StatusStarting || rec.Job != state.JobCreate || rec.Queued != state.QueuedStop {
		t.Fatalf("record %q job %q queued %q, want the create kept with the stop queued", rec.Shed.Status, rec.Job, rec.Queued)
	}
	e.auto()
	e.sweep()
	got := e.settled(name)
	if got.Status != config.StatusStopped {
		t.Fatalf("came to rest %q (%q), want stopped: the create finished and the queued stop ran", got.Status, got.StatusReason)
	}
	if rec := e.b.snapshot(name, id); !rec.Ready {
		t.Fatal("the create did not finish before the stop ran")
	}
}

// A create resumed against an instance whose labels are another version's
// touches nothing: no link to the guest, no credentials, no clone, no label
// write, and no clean-up marker that a later server would act on. A server
// that reads the labels finishes the create.
func TestResumedCreateLeavesAnotherSchemaAlone(t *testing.T) {
	tests := []struct {
		name     string
		finished bool
	}{
		{"an unfinished create", false},
		{"a create the cloud calls finished", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sp := &schemaProvider{}
			wrap := func(e *env, _ *fake.Options) {
				e.wrap = func(p provider.Provider) provider.Provider {
					sp.Provider = p.(*fake.Provider)
					return sp
				}
			}
			var e *env
			var id string
			const name = "newer"
			if tt.finished {
				e = newEnv(t, wrap)
				e.running(name)
				id = e.uncommit(name)
			} else {
				e = newEnv(t, manual(), wrap)
				e.put(name, config.StatusStarting)
				id = e.waitRef(name)
			}
			if err := e.b.Close(); err != nil {
				t.Fatal(err)
			}
			// The start-up pass leaves an instance with such labels alone
			// (it cannot read whose it is), so the record's create is
			// resumed and only its own read of the instance finds it.
			sp.schema.Store("2")
			sp.listToo.Store(true)
			dials := e.fl.dialCount()
			labels, meta := e.count(fake.OpSetLabels), e.count(fake.OpSetMetadata)
			e.auto()
			e.b = e.open()
			e.idle()
			e.sweep()
			e.idle()

			rec := e.b.snapshot(name, id)
			if rec == nil || rec.Shed.Status != config.StatusStarting || rec.Job != state.JobCreate || rec.Shed.StatusReason != config.StatusReasonCloudUnreadable {
				t.Fatalf("the record is %+v, want starting (%s) with its create kept", rec, config.StatusReasonCloudUnreadable)
			}
			if n := e.fl.dialCount() - dials; n != 0 {
				t.Errorf("%d connections to the guest of an instance with another label schema, want none", n)
			}
			if n := e.count(fake.OpSetLabels) - labels; n != 0 {
				t.Errorf("%d label writes, want none", n)
			}
			if n := e.count(fake.OpSetMetadata) - meta; n != 0 {
				t.Errorf("%d metadata writes, want none", n)
			}
			if n := e.count(fake.OpDestroy) + e.count(fake.OpStop); n != 0 {
				t.Errorf("%d stop or destroy calls, want none", n)
			}
			if n := markerCount(t, e.dir); n != 0 {
				t.Errorf("%d delete markers, want none", n)
			}
			if e.instances() != 1 {
				t.Fatalf("%d instances, want 1", e.instances())
			}

			// A server that reads the labels carries on.
			sp.schema.Store(provider.SchemaVersion)
			sp.listToo.Store(false)
			e.sweep()
			if got := e.settled(name); got.Status != config.StatusRunning || got.ID != id {
				t.Fatalf("with readable labels came to rest %q (%q), want running", got.Status, got.StatusReason)
			}
		})
	}
}

// A start resumed with its VM already up is followed even while the firewall
// check fails. When the read that would tell (is it up?) fails as well, the
// server knows nothing, and must not treat the start as failed: a failed
// start stops the VM.
func TestResumedStartIsKeptWhenItsInstanceCannotBeRead(t *testing.T) {
	tests := []struct {
		name string
		// gate: the firewall check fails, so the follow-up read is the one
		// that fails. Otherwise it is the read before the start call.
		gate bool
	}{
		{"the firewall check fails and the follow-up read fails", true},
		{"the read before the start call fails", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t)
			const name = "up"
			e.running(name)
			id := e.settledID(name)
			// The record of a start that died after the cloud brought the VM up.
			if !e.b.update(name, id, func(r *state.Record) {
				r.Shed.Status = config.StatusStarting
				r.Job = state.JobStart
			}) {
				t.Fatal("no record")
			}
			if err := e.b.Close(); err != nil {
				t.Fatal(err)
			}
			if tt.gate {
				brokenSources(e.prov)
			}
			// One read fails; the next (the one a failed start's clean-up
			// makes before it stops the VM) would succeed.
			e.prov.FailNext(fake.OpGet, errCloudDown)
			e.b = e.open()
			e.idle()

			if n := e.count(fake.OpStop); n != 0 {
				t.Fatalf("%d stop calls on a running VM after a failed read, want none", n)
			}
			rec := e.b.snapshot(name, id)
			if rec == nil || rec.Shed.Status != config.StatusStarting || rec.Job != state.JobStart || rec.Shed.StatusReason != config.StatusReasonCloudUnreadable {
				t.Fatalf("the record is %+v, want starting (%s) with its start kept", rec, config.StatusReasonCloudUnreadable)
			}
			e.sweep()
			got := e.settled(name)
			e.idle()
			if got.Status != config.StatusRunning || got.StatusReason != "" {
				t.Fatalf("came to rest %q (%q), want running", got.Status, got.StatusReason)
			}
			if n := e.count(fake.OpStop); n != 0 {
				t.Fatalf("%d stop calls, want none", n)
			}
		})
	}
}

// nthGet is the fake with one chosen read of an instance made to fail.
type nthGet struct {
	*fake.Provider
	mu   sync.Mutex
	seen int
	fail int // the read to fail, counted from 1; 0 for none
}

func (n *nthGet) Get(ctx context.Context, ref provider.Ref) (provider.Instance, error) {
	n.mu.Lock()
	n.seen++
	fail := n.fail != 0 && n.seen == n.fail
	n.mu.Unlock()
	if fail {
		return provider.Instance{}, errCloudDown
	}
	return n.Provider.Get(ctx, ref)
}

func (n *nthGet) arm(fail int) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.seen, n.fail = 0, fail
}

func (n *nthGet) count() int {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.seen
}

// The last thing a create does in the cloud is write the ready label, after a
// read that checks the instance is still the shed's. When that read fails,
// everything else is done and the VM is good: the create is kept and
// finished at the next look, not failed and its VM destroyed.
func TestCreateIsKeptWhenTheReadBeforeItsReadyLabelFails(t *testing.T) {
	g := &nthGet{}
	e := newEnv(t, func(e *env, _ *fake.Options) {
		e.wrap = func(p provider.Provider) provider.Provider {
			g.Provider = p.(*fake.Provider)
			return g
		}
	})
	// A first create shows how many reads a create makes; the last of them
	// is the one before the label.
	g.arm(0)
	e.running("first")
	reads := g.count()
	if reads == 0 {
		t.Fatal("a create made no read of its instance")
	}

	const name = "second"
	g.arm(reads)
	if _, err := e.create(name); err != nil {
		t.Fatal(err)
	}
	e.idle()
	id := e.settledID(name)
	rec := e.b.snapshot(name, id)
	if rec.Shed.Status != config.StatusStarting || rec.Job != state.JobCreate || rec.Shed.StatusReason != config.StatusReasonCloudUnreadable {
		t.Fatalf("the record is %q (%q) job %q, want starting (%s) with its create kept", rec.Shed.Status, rec.Shed.StatusReason, rec.Job, config.StatusReasonCloudUnreadable)
	}
	if n := e.count(fake.OpDestroy) + markerCount(t, e.dir); n != 0 {
		t.Fatalf("%d destroy calls and markers, want none", n)
	}
	e.sweep()
	if got := e.settled(name); got.Status != config.StatusRunning || got.StatusReason != "" {
		t.Fatalf("came to rest %q (%q), want running", got.Status, got.StatusReason)
	}
	if e.instances() != 2 || e.count(fake.OpDestroy) != 0 {
		t.Fatalf("%d instances and %d destroy calls, want 2 and 0", e.instances(), e.count(fake.OpDestroy))
	}
}
