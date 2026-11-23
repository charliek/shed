package remote

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/charliek/shed/internal/config"
	"github.com/charliek/shed/internal/delivery"
	"github.com/charliek/shed/internal/delivery/deliverytest"
	"github.com/charliek/shed/internal/remote/provider"
	"github.com/charliek/shed/internal/remote/provider/fake"
	"github.com/charliek/shed/internal/remote/state"
	"github.com/charliek/shed/internal/vmutil"
)

// Delivery on the remote backend, on the fake provider: the create job's
// steps, a create interrupted by a server restart between any two of them
// (criterion 8: the unfinished steps stay owed, no request is made on its
// own, and `shed provision` finishes them), and provision's rules (no job in
// flight, one at a time, a stop waits for the step in hand, nothing is ever
// removed).

const testRepo = "https://git.example.test/example/widgets.git"

// fakeGuest is a fake cloud shed's guest: its project directory, its install
// hook, and the points a test can hold a step at.
type fakeGuest struct {
	mu        sync.Mutex
	agent     *deliverytest.Agent
	cloned    bool
	other     bool // something else stands at the project directory
	installed bool
	clones    int
	installs  int
	removes   int
	// holdAt is the point the next step to reach it is held at, until its
	// context ends (a restart) or release is closed; reached says it got
	// there.
	holdAt  string
	release chan struct{}
	reached chan string
}

func newFakeGuest() *fakeGuest {
	return &fakeGuest{agent: deliverytest.NewAgent(), reached: make(chan string, 4), release: make(chan struct{})}
}

// at holds the step at point when the test asked for it.
func (g *fakeGuest) at(ctx context.Context, point string) error {
	g.mu.Lock()
	hold := g.holdAt == point
	if hold {
		g.holdAt = ""
	}
	g.mu.Unlock()
	if !hold {
		return nil
	}
	g.reached <- point
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-g.release:
		return nil
	}
}

func (g *fakeGuest) hold(point string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.holdAt = point
}

func (g *fakeGuest) waitAt(t *testing.T, point string) {
	t.Helper()
	select {
	case got := <-g.reached:
		if got != point {
			t.Fatalf("held at %s, want %s", got, point)
		}
	case <-time.After(testTimeout):
		t.Fatalf("no step reached %s", point)
	}
}

// writer is the guest's agent, with stage 2 holdable.
type writer struct {
	*deliverytest.Agent
	g *fakeGuest
}

func (w writer) Run(ctx context.Context, argv []string, stdin io.Reader, stdout io.Writer, max int) (int, error) {
	body, _ := io.ReadAll(stdin)
	var in delivery.WriterInput
	_ = json.Unmarshal(body, &in)
	if err := w.g.at(ctx, "stage"+string(rune('0'+in.Stage))); err != nil {
		return 0, err
	}
	return w.Agent.Run(ctx, argv, bytes.NewReader(body), stdout, max)
}

// deliveryEnv is an env whose backend asks src (through svc) and drives g.
func deliveryEnv(t *testing.T, opts ...envOpt) (*env, *fakeGuest, *deliverytest.Source) {
	t.Helper()
	g := newFakeGuest()
	src := deliverytest.NewSource()
	svc := delivery.NewService(src, 5*time.Second)
	src.Attach(svc)
	src.Set("", deliverytest.Answer, cannedAnswer)
	e := newEnv(t, append(opts, func(e *env, _ *fake.Options) {
		e.clone = func(ctx context.Context, _ *vmutil.AgentClient, _ *config.ServerConfig, _ string, _ bool, _ []string) error {
			if err := g.at(ctx, "clone"); err != nil {
				return err
			}
			g.mu.Lock()
			defer g.mu.Unlock()
			g.clones++
			g.cloned = true
			return nil
		}
		e.seams = func(b *Backend) {
			b.SetDelivery(svc)
			b.writer = func(*vmutil.AgentClient) delivery.Agent { return writer{Agent: g.agent, g: g} }
			b.workTree = func(context.Context, *vmutil.AgentClient, string) (vmutil.WorkTree, error) {
				g.mu.Lock()
				defer g.mu.Unlock()
				switch {
				case g.other:
					return vmutil.WorkTreeOther, nil
				case g.cloned:
					return vmutil.WorkTreeRepo, nil
				}
				return vmutil.WorkTreeAbsent, nil
			}
			b.rmPartial = func(context.Context, *vmutil.AgentClient, string) {
				g.mu.Lock()
				defer g.mu.Unlock()
				g.removes++
			}
			b.hooks = func(ctx context.Context, _ *vmutil.AgentClient, _ *state.Record, install bool, startup func(delivery.Step, bool) bool) (delivery.Step, bool) {
				var st delivery.Step
				ran := false
				if install {
					if err := g.at(ctx, "install"); err != nil {
						return delivery.Step{State: delivery.StepFailed, Reason: delivery.ReasonHookFailed}, false
					}
					g.mu.Lock()
					if !g.installed {
						g.installed, ran = true, true
						g.installs++
					}
					g.mu.Unlock()
					st = delivery.Step{State: delivery.StepDone}
				}
				startup(st, ran)
				return st, ran
			}
		}
	})...)
	return e, g, src
}

func cannedAnswer(req *delivery.Request) []byte {
	return deliverytest.AnswerOf(req.ID, []deliverytest.FileItem{
		{Name: "ssh-key", Provider: "ssh-key", Stage: 1, Base: delivery.BaseHome, Path: ".ssh/id_ed25519", Mode: "0600", Content: []byte("k")},
		{Name: "project-env", Provider: "files", Stage: 2, Base: delivery.BaseProject, Path: ".envrc", Mode: "0600", Content: []byte("e")},
	}, nil, nil)
}

func stepsOf(r *delivery.Record) string {
	if r == nil || r.Steps == nil {
		return "<none>"
	}
	f := func(s delivery.Step) string {
		if s.Reason != "" {
			return string(s.State) + "/" + string(s.Reason)
		}
		return string(s.State)
	}
	return f(r.Steps.Stage1) + " " + f(r.Steps.Clone) + " " + f(r.Steps.Stage2) + " " + f(r.Steps.Install)
}

func (e *env) createWithRepo(name string) {
	e.t.Helper()
	if _, err := e.b.CreateShed(ctxT(e.t), config.CreateShedRequest{Name: name, Repo: testRepo}); err != nil {
		e.t.Fatalf("create %s: %v", name, err)
	}
}

func TestRemoteCreateRunsTheStepsInOrder(t *testing.T) {
	e, g, src := deliveryEnv(t)
	e.createWithRepo("box")
	if got := e.settled("box"); got.Status != config.StatusRunning {
		t.Fatalf("settled %q (%q)", got.Status, got.StatusReason)
	}
	rec := e.storedRecord("box").Delivery
	if got := stepsOf(rec); got != "done done done done" || rec.State != delivery.StateDelivered {
		t.Fatalf("steps %s state %s", got, rec.State)
	}
	if err := rec.Validate(); err != nil {
		t.Fatal(err)
	}
	if w := g.agent.Writes(); len(w) != 2 || w[0].Stage != 1 || w[1].Stage != 2 || w[1].ProjectDir != "/home/shed/widgets" {
		t.Fatalf("writes = %+v", w)
	}
	if n := len(src.Requests()); n != 1 {
		t.Fatalf("%d requests", n)
	}
	if got := e.settled("box").Delivery; got == nil || got.State != delivery.StateDelivered || len(got.Owed) != 0 {
		t.Fatalf("shed document's delivery = %+v", got)
	}
}

// TestRemoteCreateInterruptedByARestart: a server restart between any two
// steps (and while the source is asked) leaves the steps it had not
// recorded owed; the resumed create makes no request and runs no step;
// `shed provision` then finishes them.
func TestRemoteCreateInterruptedByARestart(t *testing.T) {
	cases := []struct {
		point string
		want  string
	}{
		{"request", "owed owed owed owed"},
		{"clone", "done owed owed owed"},
		{"stage2", "done done owed owed"},
		{"install", "done done done owed"},
		{"label", "done done done done"},
	}
	for _, tc := range cases {
		t.Run("at "+tc.point, func(t *testing.T) {
			h := &hookProvider{}
			e, g, src := deliveryEnv(t, hooked(h))
			switch tc.point {
			case "request":
				src.Set("", deliverytest.Silent, nil)
			case "label":
				h.setLabels = func(ctx context.Context, _ provider.Ref, labels map[string]string) error {
					if labels[provider.LabelState] == string(provider.ShedStateReady) {
						return g.at(ctx, "label")
					}
					return nil
				}
				g.hold("label")
			default:
				g.hold(tc.point)
			}
			e.createWithRepo("box")
			if tc.point == "request" {
				deadline := time.Now().Add(testTimeout)
				for len(src.Requests()) == 0 {
					if time.Now().After(deadline) {
						t.Fatal("no request was sent")
					}
					time.Sleep(2 * time.Millisecond)
				}
			} else {
				g.waitAt(t, tc.point)
			}
			// While the create's job is in flight, provision is refused.
			if _, err := e.b.ProvisionShed(ctxT(t), "box"); !errors.Is(err, config.ErrProvisionBusySentinel) {
				t.Fatalf("provision during the create: %v, want busy", err)
			}

			e.restart()
			src.Set("", deliverytest.Answer, cannedAnswer)
			if got := e.settled("box"); got.Status != config.StatusRunning {
				t.Fatalf("resumed create settled %q (%q)", got.Status, got.StatusReason)
			}
			rec := e.storedRecord("box").Delivery
			if got := stepsOf(rec); got != tc.want {
				t.Fatalf("after the restart: %s, want %s", got, tc.want)
			}
			if err := rec.Validate(); err != nil {
				t.Fatal(err)
			}
			if n := len(src.Requests()); n != 1 {
				t.Fatalf("%d requests after the restart, want only the create's", n)
			}

			if _, err := e.b.ProvisionShed(ctxT(t), "box"); err != nil {
				t.Fatalf("provision: %v", err)
			}
			rec = e.storedRecord("box").Delivery
			if got := stepsOf(rec); got != "done done done done" || rec.State != delivery.StateDelivered {
				t.Fatalf("after provision: %s %s", got, rec.State)
			}
			if n := len(src.Requests()); n != 2 {
				t.Fatalf("%d requests, want the create's and the provision's", n)
			}
			g.mu.Lock()
			defer g.mu.Unlock()
			if g.clones != 1 || g.installs != 1 || g.removes != 0 {
				t.Fatalf("clones %d, installs %d, removes %d: want one each, nothing removed", g.clones, g.installs, g.removes)
			}
		})
	}
}

// TestRemoteRecordFromBeforeDelivery: a record an older server wrote reads
// cloned and provisioned as the clone and install hook done; a finished one
// reports delivery none until provisioned.
func TestRemoteRecordFromBeforeDelivery(t *testing.T) {
	e, g, src := deliveryEnv(t)
	e.createWithRepo("box")
	id := e.settled("box").ID
	e.b.update("box", id, func(r *state.Record) { r.Delivery, r.Cloned, r.Provisioned = nil, true, true })
	if d := e.settled("box").Delivery; d == nil || d.State != delivery.StateNone || d.Steps != nil {
		t.Fatalf("an old shed reports %+v, want none", d)
	}
	g.mu.Lock()
	g.clones, g.installs = 0, 0
	g.mu.Unlock()
	if _, err := e.b.ProvisionShed(ctxT(t), "box"); err != nil {
		t.Fatal(err)
	}
	rec := e.storedRecord("box").Delivery
	if got := stepsOf(rec); got != "done done done done" {
		t.Fatalf("steps = %s", got)
	}
	if g.clones != 0 || g.installs != 0 || len(src.Requests()) != 2 {
		t.Fatalf("clones %d installs %d requests %d", g.clones, g.installs, len(src.Requests()))
	}
}

// TestRemoteProvisionNeverRemovesAnything: a project directory that is not
// the repository's work tree fails the clone (project_dir_exists) and is
// left alone; a failed attempt under provision is not cleaned up.
func TestRemoteProvisionNeverRemovesAnything(t *testing.T) {
	e, g, _ := deliveryEnv(t)
	e.createWithRepo("box")
	id := e.settled("box").ID
	e.b.update("box", id, func(r *state.Record) {
		r.Delivery.Steps.Clone = delivery.Step{State: delivery.StepOwed}
		r.Delivery.Steps.Install = delivery.Step{State: delivery.StepOwed}
		r.Delivery.State = delivery.DeriveState(r.Delivery)
	})
	g.mu.Lock()
	g.other, g.clones = true, 0
	g.mu.Unlock()
	if _, err := e.b.ProvisionShed(ctxT(t), "box"); err != nil {
		t.Fatal(err)
	}
	rec := e.storedRecord("box").Delivery
	if got := stepsOf(rec); got != "done failed/project_dir_exists owed owed" {
		t.Fatalf("steps = %s", got)
	}
	if g.clones != 0 || g.removes != 0 {
		t.Fatalf("clones %d removes %d", g.clones, g.removes)
	}

	// A failed attempt with something left behind: no rm, no retry.
	g.mu.Lock()
	g.other, g.cloned = false, false
	g.mu.Unlock()
	e.b.clone = func(context.Context, *vmutil.AgentClient, *config.ServerConfig, string, bool, []string) error {
		g.mu.Lock()
		defer g.mu.Unlock()
		g.clones++
		g.other = true // a partial clone stands there now
		return errors.New("clone failed")
	}
	if _, err := e.b.ProvisionShed(ctxT(t), "box"); err != nil {
		t.Fatal(err)
	}
	if got := stepsOf(e.storedRecord("box").Delivery); got != "done failed/clone_failed owed owed" {
		t.Fatalf("steps = %s", got)
	}
	if g.clones != 1 || g.removes != 0 {
		t.Fatalf("clones %d removes %d: want one attempt and nothing removed", g.clones, g.removes)
	}
}

// TestRemoteProvisionOneAtATime: a second provision of the shed is refused
// while one runs; a stop waits for the step in hand, and the provision's
// next step then finds the shed stopping and ends.
func TestRemoteProvisionOneAtATime(t *testing.T) {
	e, g, _ := deliveryEnv(t)
	e.createWithRepo("box")
	id := e.settled("box").ID
	e.b.update("box", id, func(r *state.Record) {
		r.Delivery.Steps.Clone = delivery.Step{State: delivery.StepOwed}
		r.Delivery.State = delivery.DeriveState(r.Delivery)
	})
	g.mu.Lock()
	g.cloned = false
	g.mu.Unlock()
	g.hold("clone")
	done := make(chan error, 1)
	go func() { _, err := e.b.ProvisionShed(ctxT(t), "box"); done <- err }()
	g.waitAt(t, "clone")

	if _, err := e.b.ProvisionShed(ctxT(t), "box"); !errors.Is(err, config.ErrProvisionBusySentinel) {
		t.Fatalf("second provision: %v, want busy", err)
	}
	stopped := make(chan error, 1)
	go func() { _, err := e.b.StopShed(ctxT(t), "box"); stopped <- err }()
	select {
	case err := <-stopped:
		t.Fatalf("the stop did not wait for the step in hand: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	close(g.release)
	if err := <-stopped; err != nil {
		t.Fatalf("stop: %v", err)
	}
	if err := <-done; !errors.Is(err, config.ErrShedNotRunningSentinel) {
		t.Logf("record: %s; logs: %q", stepsOf(e.storedRecord("box").Delivery), e.logLines())
		t.Fatalf("provision after the stop: %v, want not running", err)
	}
	// The step in hand was recorded; the next was not reached.
	if got := stepsOf(e.storedRecord("box").Delivery); got != "done done done done" {
		t.Fatalf("steps = %s", got)
	}
}

// A step's record write that fails ends the run: the steps after it do not
// run, and the record is left as the last write that was made. A write
// that put the record in place but could not flush it ends the run too,
// with the record as the disk has it.
func TestRemoteStepWriteFailureEndsTheRun(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want string
	}{
		{"the write fails", errors.New("disk full"), "done owed owed owed"},
		{"the flush fails", fmt.Errorf("x: %w", state.ErrNotDurable), "done done owed owed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, g, _ := deliveryEnv(t)
			var mu sync.Mutex
			writes := 0
			// Only a delivery step's write goes through saveRecord: the
			// first is stage 1's, the second the clone's.
			e.b.saveRecord = func(r *state.Record) error {
				mu.Lock()
				writes++
				n := writes
				mu.Unlock()
				if n != 2 {
					return e.b.store.SaveRecord(r)
				}
				if errors.Is(tc.err, state.ErrNotDurable) {
					if err := e.b.store.SaveRecord(r); err != nil {
						return err
					}
				}
				return tc.err
			}
			e.createWithRepo("box")
			if got := e.settled("box"); got.Status != config.StatusRunning {
				t.Fatalf("settled %q", got.Status)
			}
			if got := stepsOf(e.storedRecord("box").Delivery); got != tc.want {
				t.Fatalf("stored steps = %s, want %s", got, tc.want)
			}
			if got := stepsOf(e.b.snapshot("box", e.settled("box").ID).Delivery); got != tc.want {
				t.Fatalf("in-memory steps = %s, want %s", got, tc.want)
			}
			g.mu.Lock()
			defer g.mu.Unlock()
			if g.installs != 0 || len(g.agent.Writes()) != 1 {
				t.Fatalf("installs %d, writes %d: steps ran after the failed write", g.installs, len(g.agent.Writes()))
			}
		})
	}
}

// A stop asked for while a create waits for its source returns at once
// (the wait holds no step lock); the create's delivery then ends before
// its next step, the create finishes, and the queued stop runs.
func TestRemoteStopDuringTheCreatesSourceWait(t *testing.T) {
	e, g, src := deliveryEnv(t)
	gate := make(chan struct{})
	src.SetGate("", gate)
	e.createWithRepo("box")
	deadline := time.Now().Add(testTimeout)
	for len(src.Requests()) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("no request")
		}
		time.Sleep(time.Millisecond)
	}
	stopped := make(chan error, 1)
	go func() { _, err := e.b.StopShed(ctxT(t), "box"); stopped <- err }()
	select {
	case err := <-stopped:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the stop waited for the create's source wait")
	}
	close(gate)
	if got := e.settled("box"); got.Status != config.StatusStopped {
		t.Fatalf("settled %q (%q), want stopped", got.Status, got.StatusReason)
	}
	if got := stepsOf(e.storedRecord("box").Delivery); got != "owed owed owed owed" {
		t.Fatalf("steps = %s", got)
	}
	if len(g.agent.Writes()) != 0 {
		t.Fatal("the answer was written after the stop")
	}
}

// A reconcile that finds the shed's user changed while a provision waits
// for its source ends the provision: the answer, for the user who was
// authorized, is dropped.
func TestRemoteProvisionEndsWhenTheUserChanges(t *testing.T) {
	e, g, src := deliveryEnv(t)
	e.createWithRepo("box")
	id := e.settled("box").ID
	gate := make(chan struct{})
	src.SetGate("", gate)
	writes := len(g.agent.Writes())
	done := make(chan error, 1)
	go func() { _, err := e.b.ProvisionShed(ctxT(t), "box"); done <- err }()
	deadline := time.Now().Add(testTimeout)
	for len(src.Requests()) < 2 {
		if time.Now().After(deadline) {
			t.Fatal("no provision request")
		}
		time.Sleep(time.Millisecond)
	}
	e.b.update("box", id, func(r *state.Record) { r.UserConflict = "mallory" })
	close(gate)
	if err := <-done; !errors.Is(err, config.ErrShedNotRunningSentinel) {
		t.Fatalf("provision: %v, want it ended", err)
	}
	if len(g.agent.Writes()) != writes {
		t.Fatal("the answer was written for a shed that changed hands")
	}
}
