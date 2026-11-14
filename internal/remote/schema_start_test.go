package remote

import (
	"errors"
	"testing"

	"github.com/charliek/shed/internal/config"
	"github.com/charliek/shed/internal/remote/provider"
	"github.com/charliek/shed/internal/remote/provider/fake"
	"github.com/charliek/shed/internal/remote/state"
)

// schemaStart is a shed whose start job has met an instance labelled by
// another version of the label set, with the cloud calls and guest
// connections counted up to just before it did.
type schemaStart struct {
	id                   string
	starts, stops, dials int
}

// startOnAnotherSchema brings a shed to that point. resumed: the start is one
// a server that died had been working on; otherwise it is a start call on a
// stopped VM. up (resumed only): the VM is already running.
func startOnAnotherSchema(t *testing.T, e *env, sp *schemaProvider, name string, resumed, up, gateFails bool) schemaStart {
	t.Helper()
	e.running(name)
	s := schemaStart{id: e.settledID(name)}
	if !resumed || !up {
		if _, err := e.b.StopShed(ctxT(t), name); err != nil {
			t.Fatal(err)
		}
		if got := e.settled(name); got.Status != config.StatusStopped {
			t.Fatalf("the shed settled %q, want stopped", got.Status)
		}
	}
	e.idle()
	s.starts, s.stops, s.dials = e.count(fake.OpStart), e.count(fake.OpStop), e.fl.dialCount()
	other := func() {
		sp.schema.Store("2")
		sp.listToo.Store(true)
	}
	if !resumed {
		other()
		if _, err := e.b.StartShed(ctxT(t), name); err != nil {
			t.Fatalf("the start call: %v", err)
		}
		e.idle()
		return s
	}
	// The record of a start whose server died: before the start call was
	// sent (the VM is stopped) or after the cloud brought the VM up.
	if !e.b.update(name, s.id, func(r *state.Record) {
		r.Shed.Status = config.StatusStarting
		r.Shed.StatusReason = ""
		r.Job = state.JobStart
	}) {
		t.Fatal("no record")
	}
	if err := e.b.Close(); err != nil {
		t.Fatal(err)
	}
	if gateFails {
		brokenSources(e.prov)
	}
	other()
	e.b = e.open()
	e.idle()
	// The start-up pass leaves an instance with such labels out of the list
	// it works from, so the start gets its job at the first periodic pass.
	e.sweep()
	e.idle()
	return s
}

// A start that reads an instance labelled by another version of the label
// set knows whose the instance is and nothing else it may act on. Failing the
// start records the shed stopped, drops its job and tries to stop the VM,
// which may be running (a start resumed after a restart). The start is kept
// instead, as a create is: nothing is sent to the cloud or the guest, the
// periodic pass looks again without logging again, and once the labels can be
// read the job carries on.
func TestStartIsKeptForAnotherLabelSchema(t *testing.T) {
	tests := []struct {
		name      string
		resumed   bool
		gateFails bool
		// reads is how many reads of the instance one periodic pass makes.
		reads int
	}{
		{name: "a start call on a stopped VM", reads: 1},
		{name: "a resumed start whose VM is running", resumed: true, reads: 1},
		// The read that follows a failing firewall check uses the instance's
		// state only, which is no label: the VM is up, so the start is
		// followed, and the read before the start call then meets the labels.
		{name: "a resumed start whose VM is running, the firewall check failing", resumed: true, gateFails: true, reads: 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sp := &schemaProvider{}
			e := newEnv(t, otherSchema(sp))
			const name = "newer"
			s := startOnAnotherSchema(t, e, sp, name, tt.resumed, true, tt.gateFails)

			check := func(when string) {
				t.Helper()
				rec := e.b.snapshot(name, s.id)
				if rec == nil || rec.Shed.Status != config.StatusStarting || rec.Job != state.JobStart || rec.Shed.StatusReason != config.StatusReasonCloudUnreadable {
					t.Fatalf("%s: the record is %+v, want starting (%s) with its start kept", when, rec, config.StatusReasonCloudUnreadable)
				}
				if n := e.count(fake.OpStart) - s.starts; n != 0 {
					t.Fatalf("%s: %d start calls on an instance of another label schema, want none", when, n)
				}
				if n := e.count(fake.OpStop) - s.stops; n != 0 {
					t.Fatalf("%s: %d stop calls on an instance of another label schema, want none", when, n)
				}
				if n := e.count(fake.OpDestroy) + markerCount(t, e.dir); n != 0 {
					t.Fatalf("%s: %d destroy calls and delete markers, want none", when, n)
				}
				if n := e.fl.dialCount() - s.dials; n != 0 {
					t.Fatalf("%s: %d connections to the guest, want none", when, n)
				}
				if n := e.keptLines(); n != 1 {
					t.Fatalf("%s: %d log lines say the start is kept, want 1: %q", when, n, e.logLines())
				}
			}
			check("after the start read the labels")

			// It does not spin, and each pass costs its reads and no more.
			for pass := 1; pass <= 2; pass++ {
				gets := e.count(fake.OpGet)
				e.sweep()
				e.idle()
				check("after a periodic pass")
				if n := e.count(fake.OpGet) - gets; n != tt.reads {
					t.Fatalf("pass %d: %d reads of the instance, want %d", pass, n, tt.reads)
				}
			}

			// Once the labels can be read, the job carries on to running.
			sp.schema.Store(provider.SchemaVersion)
			sp.listToo.Store(false)
			e.sweep()
			got := e.settled(name)
			e.idle()
			if got.Status != config.StatusRunning || got.StatusReason != "" || got.ID != s.id {
				t.Fatalf("with readable labels came to rest %q (%q), want running", got.Status, got.StatusReason)
			}
			if n := e.count(fake.OpStart) - s.starts; n != 1 {
				t.Fatalf("%d start calls once the labels could be read, want 1", n)
			}
			if n := e.count(fake.OpStop) - s.stops; n != 0 {
				t.Fatalf("%d stop calls, want none", n)
			}
		})
	}
}

// What can be asked of a shed whose start is kept for its labels: it holds
// its run slot (its VM may be running), a stop waits behind the start, and a
// delete ends it at once. None of them stops or destroys the instance while
// its labels are another version's.
func TestKeptStartOnAnotherLabelSchemaStaysUsable(t *testing.T) {
	tests := []struct {
		name string
		run  func(t *testing.T, e *env, sp *schemaProvider, name string, s schemaStart)
	}{
		{"it holds its run slot", func(t *testing.T, e *env, _ *schemaProvider, _ string, _ schemaStart) {
			if _, err := e.create("second"); !errors.Is(err, config.ErrServerLimitReachedSentinel) {
				t.Fatalf("a second shed under max_running 1 = %v, want the limit", err)
			}
		}},
		{"a stop waits behind it and runs once the labels can be read", func(t *testing.T, e *env, sp *schemaProvider, name string, s schemaStart) {
			if _, err := e.b.StopShed(ctxT(t), name); err != nil {
				t.Fatalf("stop on a kept start: %v", err)
			}
			e.idle()
			e.sweep()
			e.idle()
			rec := e.b.snapshot(name, s.id)
			if rec.Shed.Status != config.StatusStarting || rec.Job != state.JobStart || rec.Queued != state.QueuedStop {
				t.Fatalf("record %q job %q queued %q, want the start kept with the stop queued", rec.Shed.Status, rec.Job, rec.Queued)
			}
			if n := e.count(fake.OpStop) - s.stops; n != 0 {
				t.Fatalf("%d stop calls on an instance of another label schema, want none", n)
			}
			sp.schema.Store(provider.SchemaVersion)
			sp.listToo.Store(false)
			e.sweep()
			if got := e.settled(name); got.Status != config.StatusStopped {
				t.Fatalf("came to rest %q (%q), want stopped: the start finished and the queued stop ran", got.Status, got.StatusReason)
			}
			if starts, stops := e.count(fake.OpStart)-s.starts, e.count(fake.OpStop)-s.stops; starts != 1 || stops != 1 {
				t.Fatalf("%d start and %d stop calls, want 1 and 1", starts, stops)
			}
		}},
		{"a delete ends it and destroys nothing", func(t *testing.T, e *env, _ *schemaProvider, name string, s schemaStart) {
			if err := e.b.DeleteShed(ctxT(t), name); err != nil {
				t.Fatalf("delete of a kept start: %v", err)
			}
			e.idle()
			if _, err := e.b.GetShed(ctxT(t), name); !errors.Is(err, config.ErrShedNotFoundSentinel) {
				t.Fatalf("the shed after its delete = %v, want not found", err)
			}
			e.sweep()
			e.idle()
			if n := e.count(fake.OpDestroy) + e.count(fake.OpStop) - s.stops; n != 0 {
				t.Fatalf("%d stop or destroy calls on an instance of another label schema, want none", n)
			}
			// The delete is owed to a server that reads the labels.
			if e.instances() != 1 || markerCount(t, e.dir) != 1 {
				t.Fatalf("%d instances and %d markers, want the instance left and the marker standing", e.instances(), markerCount(t, e.dir))
			}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sp := &schemaProvider{}
			e := newEnv(t, otherSchema(sp), limits(1, 16))
			const name = "newer"
			s := startOnAnotherSchema(t, e, sp, name, false, false, false)
			if rec := e.b.snapshot(name, s.id); rec == nil || rec.Job != state.JobStart || rec.Shed.StatusReason != config.StatusReasonCloudUnreadable {
				t.Fatalf("the record is %+v, want its start kept (%s)", rec, config.StatusReasonCloudUnreadable)
			}
			tt.run(t, e, sp, name, s)
		})
	}
}

// A start that is not sent because the firewall check fails reads the
// instance for its state alone, and the state is the cloud's own field, not a
// label. So a stopped VM of another label schema is refused like any other:
// the shed goes back to stopped (firewall), which is true, and holds no run
// slot for a start that could not be sent in any case.
func TestStartOfAStoppedVMOfAnotherLabelSchemaIsRefusedByTheFirewallCheck(t *testing.T) {
	sp := &schemaProvider{}
	e := newEnv(t, otherSchema(sp))
	const name = "newer"
	s := startOnAnotherSchema(t, e, sp, name, true, false, true)
	rec := e.b.snapshot(name, s.id)
	if rec == nil || rec.Shed.Status != config.StatusStopped || rec.Shed.StatusReason != config.StatusReasonFirewall || rec.Job != "" {
		t.Fatalf("the record is %+v, want stopped (%s)", rec, config.StatusReasonFirewall)
	}
	if n := e.count(fake.OpStart) - s.starts + e.count(fake.OpStop) - s.stops + e.count(fake.OpDestroy); n != 0 {
		t.Fatalf("%d start, stop or destroy calls, want none", n)
	}
	if n := e.fl.dialCount() - s.dials; n != 0 {
		t.Fatalf("%d connections to the guest, want none", n)
	}
}

// Only labels of another version keep a start. A start the cloud turns down
// as invalid is the same kind of error (provider.ErrInvalid) and is still a
// failed start: the shed goes back to stopped.
func TestStartTheCloudCallsInvalidIsNotKept(t *testing.T) {
	e := newEnv(t)
	const name = "refused"
	e.running(name)
	if _, err := e.b.StopShed(ctxT(t), name); err != nil {
		t.Fatal(err)
	}
	e.settled(name)
	e.prov.FailNext(fake.OpStart, provider.Errorf(provider.ErrInvalid, "start", "the machine type is no longer offered"))
	if _, err := e.b.StartShed(ctxT(t), name); err != nil {
		t.Fatal(err)
	}
	got := e.settled(name)
	e.idle()
	if got.Status != config.StatusStopped || got.StatusReason == config.StatusReasonCloudUnreadable {
		t.Fatalf("came to rest %q (%q), want stopped: the start failed", got.Status, got.StatusReason)
	}
	if n := e.keptLines(); n != 0 {
		t.Fatalf("%d log lines say the start is kept, want none", n)
	}
}
