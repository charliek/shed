package remote

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/charliek/shed/internal/audit"
	"github.com/charliek/shed/internal/backend"
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
	failClone bool // every clone attempt fails
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
			if g.failClone {
				return errors.New("clone failed")
			}
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
// recorded owed; the resumed create makes no request and reads its record
// only (plan 040, section 3.3): before stage 1 was recorded nothing runs;
// after it, the clone runs if it was not recorded and the install hook runs
// once everything before it is settled, while stage 2, which needs the
// bundle, stays owed. `shed provision` then finishes them.
func TestRemoteCreateInterruptedByARestart(t *testing.T) {
	cases := []struct {
		point string
		want  string
	}{
		{"request", "owed owed owed owed"},
		{"clone", "done done owed owed"},
		{"stage2", "done done owed owed"},
		{"install", "done done done done"},
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

// noSource is a create for which no source would be asked: a service
// client's shed, a script key's create, a user with no bound machine.
type noSource struct {
	name string
	req  func(r *config.CreateShedRequest)
	src  func(src *deliverytest.Source)
}

var noSources = []noSource{
	{"a service client's shed", func(r *config.CreateShedRequest) { r.Owner = "nocturn" }, nil},
	{"a script key's create", func(r *config.CreateShedRequest) { r.ScriptKey = true }, nil},
	{"a user with no bound machine", nil, func(src *deliverytest.Source) { src.Set("", deliverytest.Unbound, nil) }},
}

func (e *env) createWithNoSource(ns noSource, src *deliverytest.Source, name string) {
	e.t.Helper()
	if ns.src != nil {
		ns.src(src)
	}
	req := config.CreateShedRequest{Name: name, Repo: testRepo}
	if ns.req != nil {
		ns.req(&req)
	}
	if _, err := e.b.CreateShed(ctxT(e.t), req); err != nil {
		e.t.Fatalf("create %s: %v", name, err)
	}
}

// TestRemoteResumedCreateWithNoSourceRunsItsSteps: a create for which no
// source would be asked, interrupted by a restart (and, once back, by a cloud
// read that fails, which the periodic pass heals) after its stage 1 was
// recorded not applicable, runs its owed steps when it resumes: stage 2 is
// skipped not_applicable and the repository is cloned and its install hook
// run, with no request and no `shed provision`. Rewound to before stage 1
// was recorded, it runs nothing.
// Its counterpart, where a source would be asked, is
// TestRemoteCreateInterruptedByARestart.
func TestRemoteResumedCreateWithNoSourceRunsItsSteps(t *testing.T) {
	points := []struct {
		point string
		// prepare changes the record while the create is held, to stand for
		// an earlier interruption: before stage 1 was decided, or a record
		// a server from before delivery wrote.
		prepare func(r *state.Record)
		// heal: the next server's first read of the instance fails, and the
		// create is kept until a periodic pass.
		heal bool
	}{
		{point: "clone"},
		{point: "clone", heal: true},
		{point: "install"},
		{point: "install", heal: true},
		{point: "label"},
		{point: "clone", prepare: func(r *state.Record) { r.Delivery = delivery.NewRecord() }},
		{point: "clone", prepare: func(r *state.Record) { r.Delivery, r.Cloned, r.Provisioned = nil, false, false }},
	}
	for _, ns := range noSources {
		for _, pt := range points {
			label := pt.point
			switch {
			case pt.heal:
				label += ", then a failed read"
			case pt.prepare != nil:
				label += ", record rewound"
			}
			t.Run(ns.name+" at "+label, func(t *testing.T) {
				h := &hookProvider{}
				e, g, src := deliveryEnv(t, hooked(h))
				if pt.point == "label" {
					h.setLabels = func(ctx context.Context, _ provider.Ref, labels map[string]string) error {
						if labels[provider.LabelState] == string(provider.ShedStateReady) {
							return g.at(ctx, "label")
						}
						return nil
					}
				}
				g.hold(pt.point)
				e.createWithNoSource(ns, src, "box")
				g.waitAt(t, pt.point)
				if pt.prepare != nil {
					id := e.settledID("box")
					e.b.update("box", id, pt.prepare)
				}
				if pt.heal {
					if err := e.b.Close(); err != nil {
						t.Fatal(err)
					}
					e.prov.FailNext(fake.OpGet, errCloudDown)
					e.b = e.open()
					e.idle()
					if r := e.storedRecord("box"); r.Shed.StatusReason != config.StatusReasonCloudUnreadable || r.Job != state.JobCreate {
						t.Fatalf("after the failed read: %q job %q, want the create kept", r.Shed.StatusReason, r.Job)
					}
					e.sweep()
				} else {
					e.restart()
				}
				if got := e.settled("box"); got.Status != config.StatusRunning {
					t.Fatalf("resumed create settled %q (%q)", got.Status, got.StatusReason)
				}
				e.idle()
				// Rewound to before stage 1 was recorded, nothing runs (every
				// step owed, for `shed provision`); otherwise stage 1 is
				// recorded not applicable and the clone and hook run.
				want, clones := "skipped/not_applicable done skipped/not_applicable done", 1
				if pt.prepare != nil {
					want, clones = "owed owed owed owed", 0
				}
				rec := e.storedRecord("box").Delivery
				if got := stepsOf(rec); got != want {
					t.Fatalf("after the resume: %s %s, want %s", got, rec.State, want)
				}
				if err := rec.Validate(); err != nil {
					t.Fatal(err)
				}
				if n := len(src.Requests()); n != 0 {
					t.Fatalf("%d requests, want none", n)
				}
				g.mu.Lock()
				defer g.mu.Unlock()
				if g.clones != clones || g.installs != clones {
					t.Fatalf("clones %d, installs %d: want %d each", g.clones, g.installs, clones)
				}
			})
		}
	}
}

// TestRemoteResumedCreateNeverRetriesAFailedStep: a create for which no
// source would be asked, whose clone failed and that is interrupted before
// its ready label, does not clone again when it resumes; the failed clone
// (and the install hook it holds back) are left for `shed provision`.
func TestRemoteResumedCreateNeverRetriesAFailedStep(t *testing.T) {
	for _, ns := range noSources {
		t.Run(ns.name, func(t *testing.T) {
			h := &hookProvider{}
			e, g, src := deliveryEnv(t, hooked(h))
			h.setLabels = func(ctx context.Context, _ provider.Ref, labels map[string]string) error {
				if labels[provider.LabelState] == string(provider.ShedStateReady) {
					return g.at(ctx, "label")
				}
				return nil
			}
			g.mu.Lock()
			g.failClone = true
			g.mu.Unlock()
			g.hold("label")
			e.createWithNoSource(ns, src, "box")
			g.waitAt(t, "label")
			g.mu.Lock()
			clones := g.clones
			g.mu.Unlock()
			e.restart()
			if got := e.settled("box"); got.Status != config.StatusRunning {
				t.Fatalf("resumed create settled %q (%q)", got.Status, got.StatusReason)
			}
			e.idle()
			rec := e.storedRecord("box").Delivery
			if got := stepsOf(rec); got != "skipped/not_applicable failed/clone_failed skipped/not_applicable owed" {
				t.Fatalf("after the resume: %s", got)
			}
			g.mu.Lock()
			defer g.mu.Unlock()
			if g.clones != clones || g.installs != 0 || len(src.Requests()) != 0 {
				t.Fatalf("clones %d (was %d), installs %d, requests %d: want no retry", g.clones, clones, g.installs, len(src.Requests()))
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

// TestRemoteCreateDeliveryEventNamesTheCreator: the create job runs after
// the request has returned, on the backend's own context; its delivery event
// still names who made the create (the actor the guard put on the request)
// and the shed by its id.
func TestRemoteCreateDeliveryEventNamesTheCreator(t *testing.T) {
	e, _, _ := deliveryEnv(t)
	path := filepath.Join(t.TempDir(), audit.FileName)
	l, err := audit.Open(path, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	e.b.deliverySvc.SetAuditLog(l)
	creator := audit.UserIdentity("", true, true, false, "SHA256:k", "2a")
	if _, err := e.b.CreateShed(audit.WithActor(ctxT(t), creator), config.CreateShedRequest{Name: "box", Repo: testRepo}); err != nil {
		t.Fatal(err)
	}
	sh := e.settled("box")
	if sh.Status != config.StatusRunning {
		t.Fatalf("settled %q", sh.Status)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var ev audit.DeliveryEvent
	if err := json.Unmarshal(bytes.TrimSpace(raw), &ev); err != nil {
		t.Fatalf("%v: %s", err, raw)
	}
	if ev.Event != audit.EventDelivery || !ev.Identity.Implicit || ev.Identity.Subject != "SHA256:k" || ev.ShedID != sh.ID || ev.Shed != "box" ||
		ev.Delivery.Reason != "create" || ev.Delivery.State != string(delivery.StateDelivered) {
		t.Errorf("event = %+v", ev)
	}
}

// TestRemoteUserOrigin: a create stamps the shed with where its user came
// from; a provision after the name's origin changed is refused
// (ErrShedOriginChanged, 409 SHED_ORIGIN_CHANGED) before anything is asked
// or recorded; the start-up stamp gives a shed with no origin the current
// one, once.
func TestRemoteUserOrigin(t *testing.T) {
	e, _, src := deliveryEnv(t)
	var origin atomic.Value
	origin.Store(delivery.OriginLocal)
	e.b.deliverySvc.SetOrigin(func(string) string { return origin.Load().(string) })
	e.createWithRepo("box")
	if got := e.settled("box"); got.Status != config.StatusRunning {
		t.Fatalf("settled %q", got.Status)
	}
	if got := e.storedRecord("box").UserOrigin; got != delivery.OriginLocal {
		t.Fatalf("user_origin %q, want local", got)
	}
	asked, before := len(src.Requests()), e.storedRecord("box").Delivery

	origin.Store(delivery.OriginDirectory)
	if _, err := e.b.ProvisionShed(ctxT(t), "box"); !errors.Is(err, delivery.ErrShedOriginChanged) {
		t.Fatalf("provision after the origin changed = %v, want ErrShedOriginChanged", err)
	}
	if len(src.Requests()) != asked || !delivery.SameRecord(e.storedRecord("box").Delivery, before) {
		t.Fatal("a refused provision asked a source or changed the record")
	}

	// A shed from before user_origin is refused (it fails closed) until a
	// start stamps it, and a stamp whose save fails (in place but not
	// durable, or not written) leaves it unstamped and refused.
	id := e.settledID("box")
	e.b.update("box", id, func(r *state.Record) { r.UserOrigin = "" })
	if _, err := e.b.ProvisionShed(ctxT(t), "box"); !errors.Is(err, delivery.ErrShedOriginUnknown) {
		t.Fatalf("provision of an unstamped shed = %v, want ErrShedOriginUnknown", err)
	}
	save := e.b.saveRecord
	for _, fail := range []error{state.ErrNotDurable, errors.New("disk full")} {
		e.b.saveRecord = func(r *state.Record) error {
			if fail == state.ErrNotDurable {
				_ = save(r)
			}
			return fail
		}
		stamped, err := e.b.StampUserOrigins(ctxT(t), func(backend.OriginShed) (string, bool) { return delivery.OriginDirectory, true })
		e.b.saveRecord = save
		if err == nil || len(stamped) != 0 || !strings.Contains(err.Error(), "box") {
			t.Fatalf("a failed stamp = %v, %v; want an error naming the shed and nothing stamped", stamped, err)
		}
		if e.storedRecord("box").UserOrigin != "" {
			t.Fatalf("a failed stamp (%v) left the origin %q in memory", fail, e.storedRecord("box").UserOrigin)
		}
		if _, err := e.b.ProvisionShed(ctxT(t), "box"); !errors.Is(err, delivery.ErrShedOriginUnknown) {
			t.Fatalf("provision after a failed stamp = %v, want ErrShedOriginUnknown", err)
		}
	}
	// The next start stamps it.
	stamped, err := e.b.StampUserOrigins(ctxT(t), func(backend.OriginShed) (string, bool) { return delivery.OriginDirectory, true })
	if err != nil || len(stamped) != 1 || stamped[0] != "box" || e.storedRecord("box").UserOrigin != delivery.OriginDirectory {
		t.Fatalf("StampUserOrigins = %v, %v; origin %q", stamped, err, e.storedRecord("box").UserOrigin)
	}
	if again, _ := e.b.StampUserOrigins(ctxT(t), func(backend.OriginShed) (string, bool) { return delivery.OriginLocal, true }); len(again) != 0 || e.storedRecord("box").UserOrigin != delivery.OriginDirectory {
		t.Fatalf("a second stamp changed a stamped shed: %v", again)
	}
}

// countHosted is an account service that answers every request accepted
// with no items, and counts them.
type countHosted struct{ n atomic.Int64 }

func (c *countHosted) Deliver(_ context.Context, request []byte) (delivery.HostedStatus, []byte) {
	c.n.Add(1)
	req, _ := delivery.ParseRequest(request)
	return delivery.HostedAnswer, []byte(`{"version":2,"id":"` + req.ID + `","items":[],"problems":[]}`)
}

func (c *countHosted) NotDelivering() bool { return false }

// TestRemoteCreateStoppedDuringBootAsksNothing: a directory user's create
// takes its delivery's admission with the job, so a stop while the cloud is
// still provisioning the VM ends the delivery before it asks: the account
// service gets no request, and the queued stop is not held up by one.
func TestRemoteCreateStoppedDuringBootAsksNothing(t *testing.T) {
	entered, gate := make(chan struct{}), make(chan struct{})
	h := &hookProvider{}
	h.create = func(ctx context.Context, spec provider.Spec) (provider.Pending, error) {
		close(entered)
		<-gate
		return h.Provider.Create(ctx, spec)
	}
	e, _, _ := deliveryEnv(t, hooked(h))
	hosted := &countHosted{}
	e.b.deliverySvc.SetOrigin(func(string) string { return delivery.OriginDirectory })
	e.b.deliverySvc.SetHosted(hosted)
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = e.b.CreateShed(ctxT(t), config.CreateShedRequest{Name: "box", Repo: testRepo})
	}()
	<-entered
	if _, err := e.b.StopShed(ctxT(t), "box"); err != nil {
		t.Fatalf("stop during the boot: %v", err)
	}
	close(gate)
	<-done
	e.settled("box")
	e.idle()
	if n := hosted.n.Load(); n != 0 {
		t.Fatalf("the account service was asked %d times for a shed stopped during its boot", n)
	}
}

// TestRemoteAdoptionRecoversOriginOnlyFromEvidence: a shed whose record is
// lost is adopted with the user origin its create wrote on the instance
// (shed_user_origin), never with the configuration of the day: pat's shed,
// made while pat was a directory user, stays directory after pat became a
// local admin (so a provision is refused, SHED_ORIGIN_CHANGED); a shed whose
// instance carries no origin (made before the label) is not known
// (SHED_ORIGIN_UNKNOWN). A record that lost its user takes the label's user
// with the label's origin, or none.
func TestRemoteAdoptionRecoversOriginOnlyFromEvidence(t *testing.T) {
	e, _, _ := deliveryEnv(t)
	var patLocal atomic.Bool
	e.b.deliverySvc.SetOrigin(func(u string) string {
		if u == "pat" && !patLocal.Load() {
			return delivery.OriginDirectory
		}
		return delivery.OriginLocal
	})
	e.createFor("pats", "pat")
	e.createFor("legacy", "bob")
	e.createFor("forged", "carol")
	if got, _ := e.labelOf("pats", provider.LabelUserOrigin); got != delivery.OriginDirectory {
		t.Fatalf("the create labelled the origin %q, want directory", got)
	}
	legacy := e.ref("legacy")
	e.prov.Seed(func() provider.Instance {
		inst, err := e.prov.Get(context.Background(), legacy)
		if err != nil {
			t.Fatal(err)
		}
		delete(inst.Labels, provider.LabelUserOrigin)
		return inst
	}())
	forged := e.ref("forged")
	e.prov.Seed(func() provider.Instance {
		inst, err := e.prov.Get(context.Background(), forged)
		if err != nil {
			t.Fatal(err)
		}
		inst.Labels[provider.LabelUserOrigin] = "bogus"
		return inst
	}())

	// The record store is lost, and pat is now a local admin.
	if err := e.b.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(e.dir); err != nil {
		t.Fatal(err)
	}
	patLocal.Store(true)
	e.b = e.open()
	e.settled("pats")
	e.settled("legacy")
	e.settled("forged")
	// Then, as serve does after the first cloud list, the origin migration
	// on a fresh state directory (no recorded decision yet): it fills only
	// the empty origins of v0.11 records, never an unknown one.
	path := filepath.Join(t.TempDir(), backend.OriginMigrationDir, backend.OriginMigrationFile)
	if _, err := backend.MigrateUserOrigins(ctxT(t), e.b, path, func(string) string { return delivery.OriginLocal }, time.Now(), t.Logf); err != nil {
		t.Fatalf("migration: %v", err)
	}
	if got := e.storedRecord("pats").UserOrigin; got != delivery.OriginDirectory {
		t.Fatalf("adopted origin of pat's shed = %q, want directory (its label's)", got)
	}
	for _, name := range []string{"legacy", "forged"} {
		if got := e.storedRecord(name).UserOrigin; got != delivery.OriginUnknown {
			t.Fatalf("adopted origin of %s (no or a bad label) after the migration = %q, want unknown", name, got)
		}
	}
	if _, err := e.b.ProvisionShed(ctxT(t), "forged"); !errors.Is(err, delivery.ErrShedOriginUnknown) {
		t.Fatalf("provision of a shed with a bad origin label = %v, want ErrShedOriginUnknown", err)
	}
	if _, err := e.b.ProvisionShed(ctxT(t), "pats"); !errors.Is(err, delivery.ErrShedOriginChanged) {
		t.Fatalf("provision of pat's old shed = %v, want ErrShedOriginChanged", err)
	}
	if _, err := e.b.ProvisionShed(ctxT(t), "legacy"); !errors.Is(err, delivery.ErrShedOriginUnknown) {
		t.Fatalf("provision of a shed with no evidence = %v, want ErrShedOriginUnknown", err)
	}

	// A record that lost its user: the user and its origin come back from
	// the labels, not from today's configuration.
	id := e.settledID("pats")
	e.b.update("pats", id, func(r *state.Record) { r.Shed.User, r.UserOrigin = "", delivery.OriginLocal })
	if err := e.b.Close(); err != nil {
		t.Fatal(err)
	}
	e.b = e.open()
	e.settled("pats")
	if r := e.storedRecord("pats"); r.Shed.User != "pat" || r.UserOrigin != delivery.OriginDirectory {
		t.Fatalf("restored user %q origin %q, want pat and directory", r.Shed.User, r.UserOrigin)
	}
}

// TestRemoteResumeWaitsForTheOriginMigration: the upgrade race. A create a
// v0.11 server left with stage 1 recorded not applicable and its clone owed
// (and no user_origin) is resumed by the new server's start-up pass; it is
// not run, nor the shed marked ready, before the origin migration has
// stamped it; once it has, the resumed create finishes its owed clone and
// install hook.
func TestRemoteResumeWaitsForTheOriginMigration(t *testing.T) {
	e, g, src := deliveryEnv(t)
	g.hold("clone")
	go func() {
		_, _ = e.b.CreateShed(ctxT(t), config.CreateShedRequest{Name: "box", Repo: testRepo, User: "bob"})
	}()
	g.waitAt(t, "clone")
	id := e.settledID("box")
	// What v0.11 left: stage 1 not applicable, the rest owed, no origin.
	e.b.update("box", id, func(r *state.Record) {
		d := delivery.NewRecord()
		d.Steps.Stage1 = delivery.Step{State: delivery.StepSkipped, Reason: delivery.ReasonNotApplicable}
		d.Last = &delivery.Last{At: delivery.FormatTime(time.Now()), Reason: delivery.ReasonCreate, Outcome: delivery.OutcomeNotRequested}
		d.State = delivery.DeriveState(d)
		r.Delivery, r.UserOrigin = d, ""
	})
	prev := e.seams
	e.seams = func(b *Backend) { prev(b); b.HoldResumes() }
	e.restart()

	time.Sleep(200 * time.Millisecond)
	if r := e.storedRecord("box"); r.Job == "" || r.Shed.Status == config.StatusRunning || r.Delivery.Steps.Clone.State != delivery.StepOwed {
		t.Fatalf("before the migration: job %q status %s clone %s; want the create held", r.Job, r.Shed.Status, r.Delivery.Steps.Clone.State)
	}
	if _, err := e.b.StampUserOrigins(ctxT(t), func(backend.OriginShed) (string, bool) { return delivery.OriginLocal, true }); err != nil {
		t.Fatal(err)
	}
	e.b.ReleaseResumes()
	if got := e.settled("box"); got.Status != config.StatusRunning {
		t.Fatalf("settled %q", got.Status)
	}
	e.idle()
	if got := stepsOf(e.storedRecord("box").Delivery); got != "skipped/not_applicable done skipped/not_applicable done" {
		t.Fatalf("after the migration: %s", got)
	}
	if len(src.Requests()) != 0 {
		t.Fatal("a resumed create asked a source")
	}
}
