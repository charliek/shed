package orchestrator

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/charliek/shed/internal/backend"
	"github.com/charliek/shed/internal/config"
	"github.com/charliek/shed/internal/delivery"
	"github.com/charliek/shed/internal/delivery/deliverytest"
	"github.com/charliek/shed/internal/lockmap"
)

// ProvisionShed on a local shed, against a fake backend whose shed can be
// stopped, replaced or deleted between steps: one provision at a time, the
// shed must be running, an id that changed ends the run, and a stop waiting
// for the lock is not overtaken by the next step.

type fakeLocal struct {
	locks   lockmap.NamedMutexMap
	halting lockmap.Flags
	mu      sync.Mutex
	shed    *config.Shed
	id      string
	rec     *delivery.Record
	// repo and projectDir are the shed's; writer is its agent.
	repo, projectDir string
	writer           delivery.Agent
	cloneEnv         []string
	gen              uint64
	// saved is every record saved, in order; withheld is each provision
	// clone's server-env rule.
	saved    []*delivery.Record
	withheld []bool
	loads    int
	// onLoad, when set, runs at each load (under the lock), with its count.
	onLoad func(n int)
	// origin is the shed's recorded user origin.
	origin string
}

func (f *fakeLocal) LockShed(name string) func() { return f.locks.Acquire(name) }

// originOrLocal is the shed's recorded origin: local unless the test set
// one, or "none" for a shed that has none.
func (f *fakeLocal) originOrLocal() string {
	switch f.origin {
	case "":
		return delivery.OriginLocal
	case "none":
		return ""
	}
	return f.origin
}

func (f *fakeLocal) Halting(name string) bool { return f.halting.Raised(name) }

func (f *fakeLocal) LoadShed(_ context.Context, name string, mintID bool) (*LocalShed, error) {
	f.mu.Lock()
	f.loads++
	n := f.loads
	on := f.onLoad
	f.mu.Unlock()
	if on != nil {
		on(n)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.shed == nil {
		return nil, config.ErrShedNotFoundSentinel
	}
	if f.id == "" && mintID {
		f.id = "minted0000000000000000000a"
	}
	cp := *f.shed
	cp.ID = f.id
	return &LocalShed{
		Shed: &cp, ID: f.id, Record: f.rec.Clone(), ResetGen: f.gen, Origin: f.originOrLocal(),
		Request:  config.DeliveryRequestShed(name, f.id, delivery.BackendFirecracker, f.repo, "", f.projectDir),
		Repo:     f.repo,
		CloneEnv: f.cloneEnv,
		Writer:   f.writer,
		Clone: func(_ context.Context, withheld bool) delivery.Step {
			f.mu.Lock()
			defer f.mu.Unlock()
			f.withheld = append(f.withheld, withheld)
			return delivery.Step{State: delivery.StepDone}
		},
		Hooks: func(context.Context, bool, func(delivery.Step, bool) bool) (delivery.Step, bool) {
			return delivery.Step{State: delivery.StepSkipped, Reason: delivery.ReasonNoHook}, false
		},
		Save: func(rec *delivery.Record) error {
			f.mu.Lock()
			defer f.mu.Unlock()
			f.rec = rec.Clone()
			f.saved = append(f.saved, rec.Clone())
			return nil
		},
	}, nil
}

// provision begins, runs and releases one provision.
func provision(f *fakeLocal, svc *delivery.Service, busy *lockmap.NamedMutexMap) (*config.Shed, error) {
	run, err := BeginProvision(context.Background(), f, svc, busy, "box")
	if err != nil {
		return nil, err
	}
	defer run.Release()
	return run.Run(context.Background())
}

func running() *config.Shed {
	return &config.Shed{Name: "box", Status: config.StatusRunning, CreatedAt: time.Date(2026, 11, 23, 9, 0, 0, 0, time.UTC)}
}

func TestLocalProvisionRules(t *testing.T) {
	t.Run("a shed that is not running", func(t *testing.T) {
		f := &fakeLocal{shed: &config.Shed{Name: "box", Status: config.StatusStopped}, id: "abc"}
		var busy lockmap.NamedMutexMap
		if _, err := provision(f, nil, &busy); !errors.Is(err, config.ErrShedNotRunningSentinel) {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("one at a time", func(t *testing.T) {
		f := &fakeLocal{shed: running(), id: "abc"}
		var busy lockmap.NamedMutexMap
		release := busy.Acquire("box")
		defer release()
		if _, err := provision(f, nil, &busy); !errors.Is(err, config.ErrProvisionBusySentinel) {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("a shed whose user's origin changed is refused before anything runs", func(t *testing.T) {
		sh := running()
		sh.User = "ada"
		f := &fakeLocal{shed: sh, id: "abc", origin: delivery.OriginLocal}
		svc := delivery.NewService(deliverytest.NewSource(), time.Second)
		svc.SetOrigin(func(string) string { return delivery.OriginDirectory })
		var busy lockmap.NamedMutexMap
		if _, err := provision(f, svc, &busy); !errors.Is(err, delivery.ErrShedOriginChanged) {
			t.Fatalf("err = %v, want ErrShedOriginChanged", err)
		}
		if len(f.saved) != 0 {
			t.Fatalf("%d records saved", len(f.saved))
		}
		if release, ok := busy.TryAcquire("box"); !ok {
			t.Fatal("the refused provision kept the shed's slot")
		} else {
			release()
		}
	})
	t.Run("a shed with no recorded origin is refused: it fails closed", func(t *testing.T) {
		f := &fakeLocal{shed: running(), id: "abc", origin: "none"}
		svc := delivery.NewService(deliverytest.NewSource(), time.Second)
		var busy lockmap.NamedMutexMap
		if _, err := provision(f, svc, &busy); !errors.Is(err, delivery.ErrShedOriginUnknown) {
			t.Fatalf("err = %v, want ErrShedOriginUnknown", err)
		}
		if len(f.saved) != 0 {
			t.Fatalf("%d records saved", len(f.saved))
		}
	})
	t.Run("a shed from before delivery gets an id and a record", func(t *testing.T) {
		f := &fakeLocal{shed: running()}
		var busy lockmap.NamedMutexMap
		shed, err := provision(f, nil, &busy)
		if err != nil {
			t.Fatal(err)
		}
		if shed.ID == "" || f.rec == nil || f.rec.State != delivery.StateNotApplicable {
			t.Fatalf("id %q record %+v", shed.ID, f.rec)
		}
	})
	t.Run("the shed is deleted and created again between steps", func(t *testing.T) {
		f := &fakeLocal{shed: running(), id: "abc"}
		f.onLoad = func(n int) {
			if n == 3 { // the first load and the stage 1 step have run
				f.mu.Lock()
				f.id = "other"
				f.mu.Unlock()
			}
		}
		var busy lockmap.NamedMutexMap
		if _, err := provision(f, nil, &busy); !errors.Is(err, config.ErrShedNotFoundSentinel) {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("a stop waiting for the lock is not overtaken", func(t *testing.T) {
		f := &fakeLocal{shed: running(), id: "abc"}
		f.onLoad = func(n int) {
			if n == 3 { // at the clone step, after stage 1: a stop arrives and waits
				lower := f.halting.Raise("box")
				go func() {
					unlock := f.locks.Acquire("box")
					f.mu.Lock()
					f.shed.Status = config.StatusStopped
					f.mu.Unlock()
					unlock()
					lower()
				}()
			}
		}
		var busy lockmap.NamedMutexMap
		if _, err := provision(f, nil, &busy); !errors.Is(err, config.ErrShedNotRunningSentinel) {
			t.Fatalf("err = %v", err)
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.rec == nil || f.rec.Steps.Stage1.State != delivery.StepSkipped || f.rec.Steps.Clone.State != delivery.StepOwed {
			t.Fatalf("record = %+v: the step after the stop's arrival ran", f.rec)
		}
	})
}

// waitRequests waits until src has been sent n requests.
func waitRequests(t *testing.T, src *deliverytest.Source, n int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for len(src.Requests()) < n {
		if time.Now().After(deadline) {
			t.Fatalf("%d requests, want %d", len(src.Requests()), n)
		}
		time.Sleep(time.Millisecond)
	}
}

func gatedSource(t *testing.T) (*deliverytest.Source, *delivery.Service, chan struct{}) {
	t.Helper()
	src := deliverytest.NewSource()
	svc := delivery.NewService(src, 10*time.Second)
	src.Attach(svc)
	src.Set("alice", deliverytest.Answer, func(req *delivery.Request) []byte {
		return deliverytest.AnswerOf(req.ID, []deliverytest.FileItem{
			{Name: "k", Provider: "ssh-key", Stage: 1, Base: delivery.BaseHome, Path: ".ssh/id_ed25519", Mode: "0600", Content: []byte("k")},
		}, nil, nil)
	})
	gate := make(chan struct{})
	src.SetGate("alice", gate)
	return src, svc, gate
}

// TestCreateDeliveryTakesTheLockPerStep: a create's delivery holds no
// lifecycle lock while it waits for its source, so a stop asked for then
// returns at once; the answer that comes after is dropped (the shed
// stopped), and nothing is written. While the create's delivery runs (its
// caller holds the shed's provisioning slot), a provision is refused.
func TestCreateDeliveryTakesTheLockPerStep(t *testing.T) {
	src, svc, gate := gatedSource(t)
	b := newMockBackend()
	agent := deliverytest.NewAgent()
	b.service, b.agent = svc, agent
	req := config.CreateShedRequest{Name: "box", User: "alice"}
	var busy lockmap.NamedMutexMap
	releaseBusy := busy.Acquire("box") // the backend's create holds it
	_, pending, err := CreateShed(context.Background(), b, req)
	if err != nil {
		t.Fatal(err)
	}
	f := b.local(req)
	done := make(chan *config.Shed, 1)
	go func() { done <- pending.Run(context.Background(), f) }()
	waitRequests(t, src, 1)

	if _, err := BeginProvision(context.Background(), f, svc, &busy, "box"); !errors.Is(err, config.ErrProvisionBusySentinel) {
		t.Fatalf("a provision during the create's delivery: %v, want busy", err)
	}
	stopped := make(chan struct{})
	go func() {
		lower := f.halting.Raise("box")
		unlock := f.locks.Acquire("box")
		f.mu.Lock()
		f.shed.Status = config.StatusStopped
		f.mu.Unlock()
		unlock()
		lower()
		close(stopped)
	}()
	select {
	case <-stopped:
	case <-time.After(2 * time.Second):
		t.Fatal("a stop during the create's source wait did not return: the wait holds the lock")
	}
	close(gate)
	<-done
	releaseBusy()
	if len(agent.Writes()) != 0 {
		t.Fatal("the answer for a stopped shed was written")
	}
	if len(f.saved) != 0 || len(delivery.OwedSteps(f.rec)) != 4 {
		t.Fatalf("saved %d, record %+v", len(f.saved), f.rec)
	}
}

// TestProvisionEndsWhenTheShedIsResetMeanwhile: while a provision waits for
// its source, the shed is stopped, reset and started again. Its id and
// creation time are the same, but its record is not the one the provision
// read: the answer is dropped and the reset record stands.
func TestProvisionEndsWhenTheShedIsResetMeanwhile(t *testing.T) {
	src, svc, gate := gatedSource(t)
	agent := deliverytest.NewAgent()
	delivered := delivery.NewRecord()
	delivered.Steps.Clone = delivery.Step{State: delivery.StepDone}
	delivered.Steps.Install = delivery.Step{State: delivery.StepDone}
	delivered.State = delivery.DeriveState(delivered)
	f := &fakeLocal{shed: running(), id: "abc", rec: delivered, writer: agent}
	f.shed.User = "alice"
	var busy lockmap.NamedMutexMap
	done := make(chan error, 1)
	go func() { _, err := provision(f, svc, &busy); done <- err }()
	waitRequests(t, src, 1)

	// Stop, reset and start, each under the lock, as the backend does them.
	unlock := f.locks.Acquire("box")
	f.mu.Lock()
	f.rec = delivery.ResetRecord(f.rec)
	f.mu.Unlock()
	unlock()

	close(gate)
	if err := <-done; !errors.Is(err, config.ErrShedNotRunningSentinel) {
		t.Fatalf("provision after a reset: %v, want it ended", err)
	}
	if len(agent.Writes()) != 0 || len(f.saved) != 0 {
		t.Fatalf("writes %d saves %d: the answer for the wiped disk was used", len(agent.Writes()), len(f.saved))
	}
	if got := delivery.OwedSteps(f.rec); len(got) != 4 {
		t.Fatalf("the reset record was overwritten: owed %v", got)
	}
}

// TestProvisionCloneWithholdsByCaller: the provision's clone gets the
// server's env_file variables only for an admin.
func TestProvisionCloneWithholdsByCaller(t *testing.T) {
	for _, tc := range []struct {
		p    backend.Principal
		want bool
	}{{backend.UserPrincipal("alice", false), true}, {backend.UserPrincipal("alice", true), false}} {
		rec := delivery.NewRecord()
		f := &fakeLocal{shed: running(), id: "abc", rec: rec, repo: "https://git.example.test/example/box.git", projectDir: "/home/shed/box"}
		var busy lockmap.NamedMutexMap
		ctx := backend.WithPrincipal(context.Background(), tc.p)
		run, err := BeginProvision(ctx, f, nil, &busy, "box")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := run.Run(ctx); err != nil {
			t.Fatal(err)
		}
		run.Release()
		if len(f.withheld) != 1 || f.withheld[0] != tc.want {
			t.Fatalf("%v: withheld %v, want [%v]", tc.p, f.withheld, tc.want)
		}
	}
}

// A create's clone runs with the egress env of the shed's metadata as its
// step reads it, not the env of the create's own proxy setup: one set, or
// cleared, between the create and its clone step is the one the clone gets.
func TestCreateCloneTakesTheCurrentEgressEnv(t *testing.T) {
	for _, tc := range []struct {
		name string
		env  []string
	}{
		{"set meanwhile", []string{"HTTPS_PROXY=http://tok@192.0.2.1:41234"}},
		{"cleared meanwhile", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := newMockBackend()
			req := config.CreateShedRequest{Name: "box", User: "alice", Repo: "https://git.example.test/example/box.git"}
			_, pending, err := CreateShed(context.Background(), b, req)
			if err != nil {
				t.Fatal(err)
			}
			f := b.local(req)
			f.cloneEnv = []string{"HTTPS_PROXY=http://old@192.0.2.1:1"} // as created
			f.cloneEnv = tc.env                                         // then changed
			pending.Run(context.Background(), f)
			if len(b.cloneEnvs) != 1 || strings.Join(b.cloneEnvs[0], ",") != strings.Join(tc.env, ",") {
				t.Fatalf("clone env = %v, want %v", b.cloneEnvs, tc.env)
			}
		})
	}
}

// A reset between a create's lock release and its first step, or during
// its source wait, ends its delivery without saving, even when the record
// it leaves is the same all-owed record (only the reset generation tells):
// a snapshot create does not mark every step skipped over a wiped disk.
func TestCreateDeliveryEndsWhenTheShedIsReset(t *testing.T) {
	reset := func(f *fakeLocal) {
		unlock := f.locks.Acquire("box")
		f.mu.Lock()
		f.rec = delivery.ResetRecord(f.rec)
		f.gen++
		f.mu.Unlock()
		unlock()
	}
	t.Run("a snapshot create, reset at the handoff", func(t *testing.T) {
		b := newMockBackend()
		b.fromSnapshot = true
		req := config.CreateShedRequest{Name: "box", User: "alice", FromSnapshot: "snap"}
		_, pending, err := CreateShed(context.Background(), b, req)
		if err != nil {
			t.Fatal(err)
		}
		f := b.local(req)
		reset(f)
		pending.Run(context.Background(), f)
		if len(f.saved) != 0 || slices.Contains(b.calls, "Startup") {
			t.Fatalf("saved %d, calls %v: the restored files' record was kept over a reset", len(f.saved), b.calls)
		}
		if got := delivery.OwedSteps(f.rec); len(got) != 4 {
			t.Fatalf("owed %v", got)
		}
	})
	t.Run("an all-owed create, reset during its source wait", func(t *testing.T) {
		src, svc, gate := gatedSource(t)
		b := newMockBackend()
		agent := deliverytest.NewAgent()
		b.service, b.agent = svc, agent
		req := config.CreateShedRequest{Name: "box", User: "alice"}
		_, pending, err := CreateShed(context.Background(), b, req)
		if err != nil {
			t.Fatal(err)
		}
		f := b.local(req)
		done := make(chan struct{})
		go func() { pending.Run(context.Background(), f); close(done) }()
		waitRequests(t, src, 1)
		reset(f)
		close(gate)
		<-done
		if len(f.saved) != 0 || len(agent.Writes()) != 0 {
			t.Fatalf("saved %d, writes %d after a reset", len(f.saved), len(agent.Writes()))
		}
	})
}

// TestProvisionStoppedBeforeItsWriteIsShedEnded: a directory user's
// provision whose shed is stopped once its answer is in (here, at the load
// its first step makes) writes nothing and reports ErrShedEnded, the cause,
// not a bare cancellation (so the API answers 409, not 500).
func TestProvisionStoppedBeforeItsWriteIsShedEnded(t *testing.T) {
	src := deliverytest.NewSource()
	svc := delivery.NewService(src, 5*time.Second)
	svc.SetOrigin(func(string) string { return delivery.OriginDirectory })
	svc.SetHosted(answeringHosted{})
	agent := deliverytest.NewAgent()
	f := &fakeLocal{shed: running(), id: "abc", writer: agent, projectDir: "/home/shed/box", origin: delivery.OriginDirectory}
	f.onLoad = func(n int) {
		if n == 2 {
			svc.EndShed("box", "abc")
		}
	}
	var busy lockmap.NamedMutexMap
	if _, err := provision(f, svc, &busy); !errors.Is(err, delivery.ErrShedEnded) {
		t.Fatalf("provision = %v, want ErrShedEnded", err)
	}
	if len(agent.Writes()) != 0 || len(f.saved) != 0 {
		t.Fatalf("wrote %d, saved %d", len(agent.Writes()), len(f.saved))
	}
}

// answeringHosted answers every request with an accepted, empty answer.
type answeringHosted struct{}

func (answeringHosted) Deliver(_ context.Context, request []byte) (delivery.HostedStatus, []byte) {
	req, _ := delivery.ParseRequest(request)
	return delivery.HostedAnswer, []byte(`{"version":2,"id":"` + req.ID + `","items":[{"name":"e","provider":"p","kind":"env","variable":"E","value":"v","fingerprint":"f"}],"problems":[]}`)
}

func (answeringHosted) NotDelivering() bool { return false }
