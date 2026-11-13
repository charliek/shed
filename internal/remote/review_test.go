package remote

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/charliek/shed/internal/backend"
	"github.com/charliek/shed/internal/config"
	"github.com/charliek/shed/internal/remote/guest"
	"github.com/charliek/shed/internal/remote/provider"
	"github.com/charliek/shed/internal/remote/provider/fake"
	"github.com/charliek/shed/internal/remote/state"
	"github.com/charliek/shed/internal/remote/transport"
	"github.com/charliek/shed/internal/vmutil"
)

// hookProvider is the fake with single calls replaced.
type hookProvider struct {
	*fake.Provider
	create  func(ctx context.Context, spec provider.Spec) (provider.Pending, error)
	list    func(ctx context.Context, sel provider.Selector) ([]provider.Instance, error)
	resolve func(ctx context.Context, image string) (string, error)
}

func (h *hookProvider) Create(ctx context.Context, spec provider.Spec) (provider.Pending, error) {
	if h.create != nil {
		return h.create(ctx, spec)
	}
	return h.Provider.Create(ctx, spec)
}

func (h *hookProvider) List(ctx context.Context, sel provider.Selector) ([]provider.Instance, error) {
	if h.list != nil {
		return h.list(ctx, sel)
	}
	return h.Provider.List(ctx, sel)
}

func (h *hookProvider) ResolveImage(ctx context.Context, image string) (string, error) {
	if h.resolve != nil {
		return h.resolve(ctx, image)
	}
	return h.Provider.ResolveImage(ctx, image)
}

func hooked(h *hookProvider) envOpt {
	return func(e *env, _ *fake.Options) {
		e.wrap = func(p provider.Provider) provider.Provider {
			h.Provider = p.(*fake.Provider)
			return h
		}
	}
}

func tuned(fn func(*Options)) envOpt {
	return func(e *env, _ *fake.Options) { e.tune = fn }
}

// clock is a settable clock for the fake provider.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func clocked(c *clock) envOpt {
	return func(_ *env, o *fake.Options) { o.Now = c.now }
}

func (e *env) instances() int {
	e.t.Helper()
	insts, err := e.prov.List(context.Background(), provider.Selector{})
	if err != nil {
		e.t.Fatalf("List: %v", err)
	}
	return len(insts)
}

func (e *env) sweep() {
	e.t.Helper()
	if err := e.b.Sweep(ctxT(e.t)); err != nil {
		e.t.Fatalf("Sweep: %v", err)
	}
}

func (e *env) running(name string) {
	e.t.Helper()
	if _, err := e.create(name); err != nil {
		e.t.Fatalf("create %s: %v", name, err)
	}
	if got := e.settled(name); got.Status != config.StatusRunning {
		e.t.Fatalf("shed %s settled %q, want running", name, got.Status)
	}
}

func (e *env) reason(name string) string {
	e.t.Helper()
	shed, err := e.b.GetShed(context.Background(), name)
	if err != nil {
		e.t.Fatalf("GetShed(%s): %v", name, err)
	}
	return shed.StatusReason
}

// --- a failed create never leaks its VM ---

// The destroy after a failed create fails once. The VM must not be forgotten:
// a delete marker stands, and the periodic pass (or a restart) finishes it.
// The shed's record stays, in "error".
func TestFailedCreateDestroyIsRetried(t *testing.T) {
	for _, how := range []string{"periodic pass", "restart"} {
		t.Run(how, func(t *testing.T) {
			e := newEnv(t)
			e.fl.failAlways("late", transport.ErrHostKeyMismatch)
			e.prov.FailNext(fake.OpDestroy, errors.New("the cloud is having a moment"))
			// The failure may land inside the fast-fail window, and the
			// create call then reports it; the shed is recorded in error
			// either way.
			_, _ = e.create("late")
			if got := e.settled("late"); got.Status != config.StatusError || got.StatusReason != config.StatusReasonHostKey {
				t.Fatalf("settled %q (%q), want error (host_key)", got.Status, got.StatusReason)
			}
			e.idle()
			if n := e.instances(); n != 1 {
				t.Fatalf("%d instances after the failed destroy, want the 1 that is still to go", n)
			}
			if n := markerCount(t, e.dir); n != 1 {
				t.Fatalf("%d delete markers after the failed destroy, want 1: nothing would retry it", n)
			}

			if how == "restart" {
				e.restart()
			} else {
				e.sweep()
			}
			e.idle()
			if n := e.instances(); n != 0 {
				t.Fatalf("%d instances after the %s, want 0", n, how)
			}
			if n := markerCount(t, e.dir); n != 0 {
				t.Fatalf("%d delete markers left", n)
			}
			if got := e.status("late"); got != config.StatusError {
				t.Fatalf("the failed shed is %q after the %s, want it still listed in error", got, how)
			}
			if err := e.b.DeleteShed(ctxT(t), "late"); err != nil {
				t.Fatalf("delete of the error shed: %v", err)
			}
			e.idle()
			if n := markerCount(t, e.dir); n != 0 {
				t.Fatalf("%d delete markers left after the shed was deleted", n)
			}
		})
	}
}

// The cloud makes the instance and the reply is lost. The create fails with
// no instance handle, and the instance must still be found and destroyed.
func TestCreateWithLostReplyIsCleanedUp(t *testing.T) {
	tests := []struct {
		name      string
		listFails int
	}{
		{"found by shed id", 0},
		// Every look for the instance fails: the marker must stand.
		{"the cloud cannot be listed", findInsertAttempts},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := &hookProvider{}
			h.create = func(ctx context.Context, spec provider.Spec) (provider.Pending, error) {
				if _, err := h.Provider.Create(ctx, spec); err != nil {
					return provider.Pending{}, err
				}
				for range tt.listFails {
					h.FailNext(fake.OpList, errors.New("list failed"))
				}
				return provider.Pending{}, errors.New("the reply was lost")
			}
			e := newEnv(t, hooked(h))
			_, _ = e.create("lost")
			if got := e.settled("lost"); got.Status != config.StatusError {
				t.Fatalf("settled %q, want error", got.Status)
			}
			e.idle()
			if tt.listFails > 0 {
				if e.instances() != 1 || markerCount(t, e.dir) != 1 {
					t.Fatalf("%d instances and %d markers while the cloud could not be listed, want 1 and 1",
						e.instances(), markerCount(t, e.dir))
				}
				e.sweep()
				e.idle()
			}
			if n := e.instances(); n != 0 {
				t.Fatalf("%d instances left by a create whose reply was lost", n)
			}
			if n := markerCount(t, e.dir); n != 0 {
				t.Fatalf("%d delete markers left", n)
			}
		})
	}
}

// The server dies after writing the clean-up marker of a failed create and
// before the status. The restarted server must finish both: the shed is in
// "error", still listed, and its VM is gone.
func TestCleanupMarkerKeepsTheRecordAcrossRestart(t *testing.T) {
	e := newEnv(t, manual())
	e.put("half", config.StatusStarting)
	rec := e.b.snapshot("half", e.settledID("half"))
	if err := e.b.Close(); err != nil {
		t.Fatal(err)
	}
	store, err := state.Open(e.dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SaveMarker(&state.Marker{ID: rec.Shed.ID, Name: "half", Env: testEnv, Ref: rec.Ref, At: time.Now(), KeepRecord: true}); err != nil {
		t.Fatal(err)
	}
	e.b = e.open()
	e.auto()
	e.idle()
	if got := e.status("half"); got != config.StatusError {
		t.Fatalf("shed is %q after the restart, want error", got)
	}
	if n := e.instances(); n != 0 {
		t.Fatalf("%d instances left, want 0", n)
	}
}

// --- the periodic pass ---

func TestSweepRetriesAFailedDelete(t *testing.T) {
	e := newEnv(t)
	e.running("doomed")
	// Every destroy attempt of the delete job fails.
	e.prov.FailNext(fake.OpDestroy, errors.New("destroy refused"))
	if err := e.b.DeleteShed(ctxT(t), "doomed"); err != nil {
		t.Fatal(err)
	}
	e.idle()
	if e.instances() != 1 || markerCount(t, e.dir) != 1 {
		t.Fatalf("%d instances and %d markers after the failed delete, want 1 and 1", e.instances(), markerCount(t, e.dir))
	}
	e.sweep()
	e.idle()
	if e.instances() != 0 || markerCount(t, e.dir) != 0 {
		t.Fatalf("%d instances and %d markers after the periodic pass, want 0 and 0", e.instances(), markerCount(t, e.dir))
	}
}

// A delete that was only just acknowledged has its own job on the way; the
// periodic pass leaves its marker to that job.
func TestSweepLeavesAYoungMarkerToItsOwnJob(t *testing.T) {
	e := newEnv(t, tuned(func(o *Options) { o.DeleteRetryAge = time.Hour }))
	e.running("fresh")
	e.prov.FailNext(fake.OpDestroy, errors.New("destroy refused"))
	if err := e.b.DeleteShed(ctxT(t), "fresh"); err != nil {
		t.Fatal(err)
	}
	e.idle()
	before := e.prov.Count(fake.OpDestroy)
	e.sweep()
	e.idle()
	if got := e.prov.Count(fake.OpDestroy); got != before {
		t.Fatalf("the periodic pass retried a marker younger than DeleteRetryAge (%d destroy calls, was %d)", got, before)
	}
}

func TestSweepCorrectsStatus(t *testing.T) {
	t.Run("stopped from outside", func(t *testing.T) {
		e := newEnv(t)
		e.running("s")
		if err := e.prov.ExternalStop(e.ref("s")); err != nil {
			t.Fatal(err)
		}
		e.sweep()
		if got, why := e.status("s"), e.reason("s"); got != config.StatusStopped || why != config.StatusReasonExternal {
			t.Fatalf("shed is %q (%q), want stopped (external)", got, why)
		}
		// It starts again like any stopped shed.
		if _, err := e.b.StartShed(ctxT(t), "s"); err != nil {
			t.Fatalf("start after an outside stop: %v", err)
		}
		if got := e.settled("s"); got.Status != config.StatusRunning {
			t.Fatalf("settled %q after the start", got.Status)
		}
	})

	t.Run("stopped by the run limit", func(t *testing.T) {
		c := &clock{t: time.Date(2026, 11, 13, 9, 0, 0, 0, time.UTC)}
		e := newEnv(t, clocked(c))
		e.running("s")
		c.advance(24 * time.Hour)
		if err := e.prov.ExternalStop(e.ref("s")); err != nil {
			t.Fatal(err)
		}
		e.sweep()
		if got, why := e.status("s"), e.reason("s"); got != config.StatusStopped || why != config.StatusReasonRunLimit {
			t.Fatalf("shed is %q (%q), want stopped (run_limit)", got, why)
		}
	})

	t.Run("instance gone", func(t *testing.T) {
		e := newEnv(t)
		e.running("s")
		e.prov.ExternalDelete(e.ref("s"))
		e.sweep()
		if _, err := e.b.GetShed(context.Background(), "s"); !errors.Is(err, config.ErrShedNotFoundSentinel) {
			t.Fatalf("GetShed after the instance vanished = %v, want not found", err)
		}
	})

	t.Run("started from outside", func(t *testing.T) {
		e := newEnv(t, limits(1, 4))
		e.running("s")
		if _, err := e.b.StopShed(ctxT(t), "s"); err != nil {
			t.Fatal(err)
		}
		e.settled("s")
		if _, err := e.prov.Start(context.Background(), e.ref("s"), provider.StartOptions{}); err != nil {
			t.Fatal(err)
		}
		e.sweep()
		if got := e.settled("s"); got.Status != config.StatusRunning {
			t.Fatalf("shed is %q, want running: its VM runs", got.Status)
		}
		// The VM holds the one run slot.
		if _, err := e.create("another"); !errors.Is(err, config.ErrServerLimitReachedSentinel) {
			t.Fatalf("create beside a VM started from outside = %v, want the limit", err)
		}
	})

	t.Run("left stopping by a failed cloud call", func(t *testing.T) {
		e := newEnv(t)
		e.running("s")
		// The stop job cannot read the instance, twice: before the stop and
		// when it asks what the instance is doing.
		e.prov.FailNext(fake.OpGet, errors.New("get failed"))
		e.prov.FailNext(fake.OpGet, errors.New("get failed"))
		if _, err := e.b.StopShed(ctxT(t), "s"); err != nil {
			t.Fatal(err)
		}
		e.idle()
		if got := e.status("s"); got != config.StatusStopping {
			t.Fatalf("shed is %q after the failed stop, want stopping", got)
		}
		e.sweep()
		if got := e.settled("s"); got.Status != config.StatusStopped {
			t.Fatalf("shed is %q after the periodic pass, want stopped", got.Status)
		}
	})
}

// The periodic pass destroys and stops nothing on its own: not a half-made
// instance, not another environment's, not a shed with a job in flight.
func TestSweepTouchesNothingWithoutAMarker(t *testing.T) {
	e := newEnv(t, manual())
	seed := func(name, id string, st provider.ShedState, env string) {
		ident := provider.Identity{Env: env, ID: id, Name: name}
		spec, _ := provider.ShedSpec{CreatedAt: time.Now(), Image: "img", Size: "standard", Network: "open"}.Encode()
		e.prov.Seed(provider.Instance{
			Name:     ident.InstanceName(),
			State:    provider.StateRunning,
			Labels:   ident.Labels(st),
			Metadata: map[string]string{provider.MetadataShedSpec: spec},
		})
	}
	seed("half-made", "aaaaaaaaaaaa", provider.ShedStateCreating, testEnv)
	seed("unknown", "dddddddddddd", provider.ShedStateReady, testEnv)
	seed("theirs", "cccccccccccc", provider.ShedStateReady, "other")
	e.put("busy", config.StatusStarting)
	e.put("up", config.StatusRunning)

	for range 3 {
		e.sweep()
	}
	if n := e.prov.Count(fake.OpDestroy) + e.prov.Count(fake.OpStop) + e.prov.Count(fake.OpStart); n != 0 {
		t.Fatalf("the periodic pass made %d destroy, stop or start calls, want none", n)
	}
	if n := e.instances(); n != 5 {
		t.Fatalf("%d instances after the periodic pass, want all 5", n)
	}
	if got := e.status("busy"); got != config.StatusStarting {
		t.Fatalf("the shed being created is %q, want starting", got)
	}
	if got := e.status("up"); got != config.StatusRunning {
		t.Fatalf("the running shed is %q, want running", got)
	}
}

func TestSweeperRunsOnItsInterval(t *testing.T) {
	e := newEnv(t, tuned(func(o *Options) { o.SweepInterval = 10 * time.Millisecond }))
	e.running("s")
	if err := e.prov.ExternalStop(e.ref("s")); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(testTimeout)
	for e.status("s") != config.StatusStopped {
		if time.Now().After(deadline) {
			t.Fatal("the periodic pass never recorded the outside stop")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// --- WaitSettled after a delete ---

// Another client must not learn, from how long WaitSettled takes, that a
// shed it could never see is being destroyed.
func TestWaitSettledAfterDeleteChecksTheOwner(t *testing.T) {
	e := newEnv(t, manual())
	if _, err := e.b.CreateShed(ctxT(t), config.CreateShedRequest{Name: "mine", NoProvision: true, Owner: "nocturn"}); err != nil {
		t.Fatal(err)
	}
	e.finish("mine")
	e.settled("mine")
	owner := backend.WithRequiredOwner(ctxT(t), "nocturn")
	if err := e.b.DeleteShed(owner, "mine"); err != nil {
		t.Fatal(err)
	}

	other, cancel := context.WithTimeout(backend.WithRequiredOwner(context.Background(), "someone-else"), 2*time.Second)
	defer cancel()
	if _, err := e.b.WaitSettled(other, "mine"); !errors.Is(err, config.ErrShedNotFoundSentinel) {
		t.Fatalf("WaitSettled by another client = %v, want not found at once", err)
	}
	short, cancelShort := context.WithTimeout(owner, 50*time.Millisecond)
	defer cancelShort()
	if _, err := e.b.WaitSettled(short, "mine"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("WaitSettled by the owner while the VM exists = %v, want it to block", err)
	}
	e.auto()
	if _, err := e.b.WaitSettled(owner, "mine"); !errors.Is(err, config.ErrShedNotFoundSentinel) {
		t.Fatalf("WaitSettled by the owner = %v, want not found", err)
	}
	if n := e.instances(); n != 0 {
		t.Fatalf("%d instances when WaitSettled reported the shed gone", n)
	}
}

// --- the --repo clone ---

func TestCloneHasATimeoutAndABoundedRetry(t *testing.T) {
	const secret = "s3kr1t-token"
	repo := "https://builder:" + secret + "@git.example.com/acme/widgets.git"
	tests := []struct {
		name      string
		failures  int
		wantCalls int
	}{
		{"first attempt works", 0, 1},
		{"third attempt works", 2, 3},
		{"never works", 99, cloneAttempts},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t, tuned(func(o *Options) { o.CloneTimeout = 7 * time.Minute }))
			var mu sync.Mutex
			calls := 0
			var problems []string
			e.b.clone = func(ctx context.Context, _ *vmutil.AgentClient, _ *config.ServerConfig, got string, _ bool, env []string) error {
				mu.Lock()
				defer mu.Unlock()
				calls++
				if got != repo {
					problems = append(problems, "clone was given "+got)
				}
				if !strings.Contains(strings.Join(env, " "), "GIT_TERMINAL_PROMPT=0") {
					problems = append(problems, "no GIT_TERMINAL_PROMPT=0 in "+strings.Join(env, " "))
				}
				deadline, ok := ctx.Deadline()
				if !ok || time.Until(deadline) > 7*time.Minute || time.Until(deadline) < 6*time.Minute {
					problems = append(problems, "the attempt is not bounded by CloneTimeout")
				}
				if calls <= tt.failures {
					return errors.New("git clone failed: exit status 128")
				}
				return nil
			}
			if _, err := e.b.CreateShed(ctxT(t), config.CreateShedRequest{Name: "cloned", NoProvision: true, Repo: repo}); err != nil {
				t.Fatalf("create: %v", err)
			}
			// A clone that never works is a warning, as on the local
			// backends: the shed still runs.
			if got := e.settled("cloned"); got.Status != config.StatusRunning {
				t.Fatalf("settled %q, want running", got.Status)
			}
			mu.Lock()
			defer mu.Unlock()
			if calls != tt.wantCalls {
				t.Fatalf("%d clone attempts, want %d", calls, tt.wantCalls)
			}
			if len(problems) > 0 {
				t.Fatalf("clone attempts: %s", strings.Join(problems, "; "))
			}
			if logs := strings.Join(e.logLines(), "\n"); strings.Contains(logs, secret) {
				t.Fatalf("the repository password reached the log:\n%s", logs)
			}
			data, err := os.ReadFile(filepath.Join(e.dir, "sheds", "cloned.json"))
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(data), secret) {
				t.Fatalf("the repository password reached the record:\n%s", data)
			}
		})
	}
}

// --- the JSON create's reply time ---

// The image lookup runs before the fast-fail window on the request's own
// context, which the API never cancels. It must have a bound of its own, or a
// slow cloud API holds the create's reply past the time a client waits.
func TestCreateBoundsTheImageLookup(t *testing.T) {
	h := &hookProvider{resolve: func(ctx context.Context, _ string) (string, error) {
		<-ctx.Done()
		return "", ctx.Err()
	}}
	e := newEnv(t, hooked(h), tuned(func(o *Options) { o.ImageResolveTimeout = 50 * time.Millisecond }))
	done := make(chan error, 1)
	go func() {
		_, err := e.b.CreateShed(context.Background(), config.CreateShedRequest{Name: "slow", NoProvision: true})
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("create succeeded although the image could not be resolved")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("create is still waiting on the image lookup")
	}
	if sheds, _ := e.b.ListSheds(context.Background()); len(sheds) != 0 {
		t.Fatalf("%d sheds recorded by a create that never resolved its image", len(sheds))
	}
}

// --- adoption ---

// An adopted VM whose agent this server cannot drive is not reported running.
func TestAdoptedShedWithIncompatibleAgentIsNotRunning(t *testing.T) {
	e := newEnv(t)
	e.running("old")
	if err := e.b.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(e.dir); err != nil {
		t.Fatal(err)
	}
	e.fl.mu.Lock()
	e.fl.info = guest.AgentInfo{Protocol: 0, Layout: guest.Layout}
	e.fl.mu.Unlock()
	e.b = e.open()
	e.idle()
	if got, why := e.status("old"), e.reason("old"); got != config.StatusStarting || why != config.StatusReasonUnreachable {
		t.Fatalf("adopted shed is %q (%q), want starting (unreachable)", got, why)
	}
	if n := e.prov.Count(fake.OpStop) + e.prov.Count(fake.OpDestroy); n != 0 {
		t.Fatalf("adoption made %d stop or destroy calls, want none", n)
	}
}

// --- restart ---

// A list that does not show a shed's instance is not proof the instance is
// gone: a server restarted with another zone lists none of its sheds. The
// record goes only when a read of that one instance says not found.
func TestRestartKeepsARecordWhoseInstanceIsOnlyUnlisted(t *testing.T) {
	h := &hookProvider{}
	e := newEnv(t, hooked(h))
	e.running("here")
	e.running("gone")
	e.prov.ExternalDelete(e.ref("gone"))
	h.list = func(context.Context, provider.Selector) ([]provider.Instance, error) { return nil, nil }
	e.restart()
	e.idle()
	if got := e.status("here"); got != config.StatusRunning {
		t.Fatalf("the unlisted shed is %q after the restart, want running", got)
	}
	if _, err := e.b.GetShed(context.Background(), "gone"); !errors.Is(err, config.ErrShedNotFoundSentinel) {
		t.Fatalf("GetShed of the shed whose instance is gone = %v, want not found", err)
	}
	// And the periodic pass draws the same line.
	e.sweep()
	if got := e.status("here"); got != config.StatusRunning {
		t.Fatalf("the unlisted shed is %q after the periodic pass, want running", got)
	}
	if n := e.prov.Count(fake.OpDestroy) + e.prov.Count(fake.OpStop); n != 0 {
		t.Fatalf("%d destroy or stop calls, want none", n)
	}
}

// --- limits ---

// While destroys fail, the VM of each deleted shed still runs. Once its
// delete is older than DeleteRetryAge it holds a run slot, so max_running
// still caps what the server has running.
func TestStuckDeleteHoldsARunSlot(t *testing.T) {
	e := newEnv(t, limits(1, 4))
	e.running("one")
	e.prov.FailNext(fake.OpDestroy, errors.New("destroy refused"))
	if err := e.b.DeleteShed(ctxT(t), "one"); err != nil {
		t.Fatal(err)
	}
	e.idle()
	if n := e.instances(); n != 1 {
		t.Fatalf("%d instances after the failed destroy, want 1", n)
	}
	_, err := e.create("two")
	if !errors.Is(err, config.ErrServerLimitReachedSentinel) {
		t.Fatalf("create beside a VM that could not be destroyed = %v, want the limit", err)
	}
	if !strings.Contains(err.Error(), "could not be destroyed") {
		t.Fatalf("the refusal does not say why: %v", err)
	}
	e.sweep()
	e.idle()
	if n := e.instances(); n != 0 {
		t.Fatalf("%d instances after the periodic pass, want 0", n)
	}
	e.running("two")
}

// A delete that is going normally frees its run slot at once.
func TestDeleteInFlightFreesItsRunSlot(t *testing.T) {
	e := newEnv(t, manual(), limits(1, 4), tuned(func(o *Options) { o.DeleteRetryAge = time.Hour }))
	e.put("one", config.StatusRunning)
	if err := e.b.DeleteShed(ctxT(t), "one"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.create("two"); err != nil {
		t.Fatalf("create while the deleted shed's VM is going: %v", err)
	}
	e.auto()
	if got := e.settled("two"); got.Status != config.StatusRunning {
		t.Fatalf("settled %q", got.Status)
	}
}

// --- secrets and guest output ---

// A repository URL with no path makes git's directory name out of the URL's
// authority, credentials included. It must never become the landing
// directory (which is stored and written to instance metadata) or be echoed.
func TestCreateRefusesARepoWhoseDirectoryNameWouldHoldCredentials(t *testing.T) {
	const secret = "s3kr1t-token"
	for _, repo := range []string{"https://builder:" + secret + "@git.example.com", "https://" + secret + "@git.example.com/"} {
		e := newEnv(t)
		_, err := e.b.CreateShed(ctxT(t), config.CreateShedRequest{Name: "leaky", NoProvision: true, Repo: repo})
		if !errors.Is(err, config.ErrInvalidShedRequestSentinel) {
			t.Fatalf("create with repo %q = %v, want an invalid request", strings.ReplaceAll(repo, secret, "<secret>"), err)
		}
		if strings.Contains(err.Error(), secret) {
			t.Fatalf("the refusal echoes the credentials: %v", err)
		}
		if n := e.instances(); n != 0 {
			t.Fatalf("%d instances created", n)
		}
	}
}

func TestCapWriterKeepsOnlyTheFirstBytes(t *testing.T) {
	w := &capWriter{max: 10}
	for _, chunk := range []string{"0123", "4567", "89ab", "cdef"} {
		if n, err := w.Write([]byte(chunk)); n != len(chunk) || err != nil {
			t.Fatalf("Write(%q) = %d, %v; want the whole chunk accepted", chunk, n, err)
		}
	}
	if got := w.String(); got != "0123456789" {
		t.Fatalf("kept %q, want the first 10 bytes", got)
	}
}
