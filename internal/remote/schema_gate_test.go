package remote

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/charliek/shed/internal/config"
	"github.com/charliek/shed/internal/remote/provider"
	"github.com/charliek/shed/internal/remote/provider/fake"
	"github.com/charliek/shed/internal/remote/state"
)

// schemaProvider is the fake reporting every instance it reads under another
// label schema, once schema is set. (The fake itself refuses to change that
// label, as the cloud's provider does.)
type schemaProvider struct {
	*fake.Provider
	schema atomic.Value
	// listToo makes List report the other schema as well, as the cloud
	// would: the labels are the instance's, whoever reads them.
	listToo atomic.Bool
}

func (s *schemaProvider) List(ctx context.Context, sel provider.Selector) ([]provider.Instance, error) {
	insts, err := s.Provider.List(ctx, sel)
	if v, ok := s.schema.Load().(string); ok && err == nil && s.listToo.Load() {
		for i := range insts {
			insts[i].Labels[provider.LabelSchema] = v
		}
	}
	return insts, err
}

func (s *schemaProvider) Get(ctx context.Context, ref provider.Ref) (provider.Instance, error) {
	inst, err := s.Provider.Get(ctx, ref)
	if v, ok := s.schema.Load().(string); ok && err == nil {
		inst.Labels[provider.LabelSchema] = v
	}
	return inst, err
}

// otherSchema gives the environment a provider whose instances can be shown
// under another label schema (sp.schema, sp.listToo).
func otherSchema(sp *schemaProvider) envOpt {
	return func(e *env, _ *fake.Options) {
		e.wrap = func(p provider.Provider) provider.Provider {
			sp.Provider = p.(*fake.Provider)
			return sp
		}
	}
}

// keptLines counts the log lines that say a job was kept.
func (e *env) keptLines() int {
	n := 0
	for _, l := range e.logLines() {
		if strings.Contains(l, "is kept as it is") {
			n++
		}
	}
	return n
}

// An instance whose labels are another version's is never touched: not
// stopped, and not destroyed by a delete, whose marker stands for a server
// that can read the labels.
func TestUnknownSchemaInstanceIsNeverMutated(t *testing.T) {
	sp := &schemaProvider{}
	e := newEnv(t, func(e *env, _ *fake.Options) {
		e.wrap = func(p provider.Provider) provider.Provider {
			sp.Provider = p.(*fake.Provider)
			return sp
		}
	})
	const name = "newer"
	e.running(name)
	sp.schema.Store("2")

	if _, err := e.b.StopShed(ctxT(t), name); err != nil {
		t.Fatal(err)
	}
	e.idle()
	if n := e.prov.Count(fake.OpStop); n != 0 {
		t.Fatalf("%d stop calls on an instance of another label schema, want none", n)
	}

	if err := e.b.DeleteShed(ctxT(t), name); err != nil {
		t.Fatal(err)
	}
	e.idle()
	if n := e.prov.Count(fake.OpDestroy); n != 0 {
		t.Fatalf("%d destroy calls on an instance of another label schema, want none", n)
	}
	if e.instances() != 1 {
		t.Fatal("the instance is gone")
	}
	if markerCount(t, e.dir) != 1 {
		t.Fatalf("%d delete markers, want 1: the delete is still owed", markerCount(t, e.dir))
	}
	found := false
	for _, l := range e.logLines() {
		if strings.Contains(l, provider.LabelSchema) {
			found = true
		}
	}
	if !found {
		t.Fatal("nothing was logged about the label schema")
	}
}

// lateSchema is the fake showing every instance under another label schema
// from one chosen read on: labels that change while a job is at work.
type lateSchema struct {
	*fake.Provider
	mu   sync.Mutex
	seen int
	from int // the first read to show the other schema, counted from 1; 0 for none
}

func (l *lateSchema) Get(ctx context.Context, ref provider.Ref) (provider.Instance, error) {
	inst, err := l.Provider.Get(ctx, ref)
	l.mu.Lock()
	defer l.mu.Unlock()
	l.seen++
	if err == nil && l.from != 0 && l.seen >= l.from {
		inst.Labels[provider.LabelSchema] = "2"
	}
	return inst, err
}

func (l *lateSchema) arm(from int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.seen, l.from = 0, from
}

func (l *lateSchema) count() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.seen
}

// The read before the ready label can find labels of another version too, if
// they changed while the create was at work. The create is kept there as it
// is at its first read: failing it writes a clean-up marker, and a server
// that reads those labels would destroy a VM that is set up and good.
func TestCreateIsKeptWhenTheLabelsChangeBeforeItsReadyLabel(t *testing.T) {
	l := &lateSchema{}
	e := newEnv(t, func(e *env, _ *fake.Options) {
		e.wrap = func(p provider.Provider) provider.Provider {
			l.Provider = p.(*fake.Provider)
			return l
		}
	})
	// A first create shows how many reads a create makes; the last of them
	// is the one before the label.
	e.running("first")
	reads := l.count()
	if reads == 0 {
		t.Fatal("a create made no read of its instance")
	}

	const name = "second"
	labels := e.count(fake.OpSetLabels)
	l.arm(reads)
	if _, err := e.create(name); err != nil {
		t.Fatal(err)
	}
	e.idle()
	id := e.settledID(name)
	check := func(when string) {
		t.Helper()
		rec := e.b.snapshot(name, id)
		if rec.Shed.Status != config.StatusStarting || rec.Job != state.JobCreate || rec.Shed.StatusReason != config.StatusReasonCloudUnreadable {
			t.Fatalf("%s: the record is %q (%q) job %q, want starting (%s) with its create kept", when, rec.Shed.Status, rec.Shed.StatusReason, rec.Job, config.StatusReasonCloudUnreadable)
		}
		if n := e.count(fake.OpDestroy) + markerCount(t, e.dir); n != 0 {
			t.Fatalf("%s: %d destroy calls and markers, want none", when, n)
		}
		if n := e.count(fake.OpSetLabels) - labels; n != 0 {
			t.Fatalf("%s: %d label writes on an instance of another label schema, want none", when, n)
		}
		if n := e.keptLines(); n != 1 {
			t.Fatalf("%s: %d log lines say the create is kept, want 1", when, n)
		}
	}
	check("after the read before the label")
	e.sweep()
	e.idle()
	check("after a periodic pass")

	l.arm(0)
	e.sweep()
	if got := e.settled(name); got.Status != config.StatusRunning || got.StatusReason != "" {
		t.Fatalf("with readable labels came to rest %q (%q), want running", got.Status, got.StatusReason)
	}
	if e.instances() != 2 || e.count(fake.OpDestroy) != 0 {
		t.Fatalf("%d instances and %d destroy calls, want 2 and 0", e.instances(), e.count(fake.OpDestroy))
	}
}
