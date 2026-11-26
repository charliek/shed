package remote

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/charliek/shed/internal/backend"
	"github.com/charliek/shed/internal/config"
	"github.com/charliek/shed/internal/delivery"
	"github.com/charliek/shed/internal/directory/refserver"
	"github.com/charliek/shed/internal/remote/guest"
	"github.com/charliek/shed/internal/remote/provider"
	"github.com/charliek/shed/internal/remote/provider/fake"
	"github.com/charliek/shed/internal/remote/provider/service/servicetest"
	"github.com/charliek/shed/internal/remote/state"
)

// The fixes of the A2 review: one test (or more) for each finding.

// callHooks is the fake with switches that lose the replies of calls whose
// work it has done, fail reads, and record the request ids sent.
type callHooks struct {
	*fake.Provider

	loseCreate  atomic.Bool // the create is made; every reply is lost
	loseWait    atomic.Bool // every wait answers unavailable
	loseStart   atomic.Bool // the start is accepted; every reply is lost
	loseDestroy atomic.Bool // the destroy is accepted; every reply is lost
	// startNotFound answers start with NOT_FOUND while the machine stands.
	startNotFound atomic.Bool
	// dropDeleted strips the list's deletions (a deletion past the
	// service's retention).
	dropDeleted atomic.Bool
	// getNotDelivered: every read of a machine never reaches the service.
	getNotDelivered atomic.Bool
	// waitDenied: the read that follows the work is refused at the scope.
	waitDenied atomic.Bool
	// createDeniedAfter: creates after this many are refused at the scope.
	createDeniedAfter atomic.Int32
	createCalls       atomic.Int32
	// blackhole: a create never connects; it ends when its context does,
	// never delivered.
	blackhole atomic.Bool
	// blackholeNext: the next this many creates are blackholed so.
	blackholeNext atomic.Int32
	// createNotMade: every create reply is lost and nothing is made (a
	// send that never got as far as the service's judgement).
	createNotMade atomic.Bool

	mu   sync.Mutex
	rids map[string][]string
}

func (h *callHooks) note(call, rid string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.rids == nil {
		h.rids = map[string][]string{}
	}
	h.rids[call] = append(h.rids[call], rid)
}

func (h *callHooks) sent(call string) []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.rids[call]...)
}

func lost(op string) error { return &provider.Error{Kind: provider.ErrUnavailable, Op: op} }

func (h *callHooks) Create(ctx context.Context, spec provider.Spec) (provider.Pending, provider.Instance, error) {
	h.note("create", spec.RequestID)
	n := h.createCalls.Add(1)
	if h.blackhole.Load() || h.blackholeNext.Add(-1) >= 0 {
		<-ctx.Done()
		return provider.Pending{}, provider.Instance{}, notDeliveredErr("create")
	}
	if h.createNotMade.Load() {
		return provider.Pending{}, provider.Instance{}, lost("create")
	}
	if after := h.createDeniedAfter.Load(); after > 0 && n > after {
		return provider.Pending{}, provider.Instance{}, &provider.Error{Kind: provider.ErrDenied, Op: "create", Code: "INSUFFICIENT_SCOPE"}
	}
	p, i, err := h.Provider.Create(ctx, spec)
	if h.loseCreate.Load() {
		return provider.Pending{}, provider.Instance{}, lost("create")
	}
	return p, i, err
}

func (h *callHooks) Wait(ctx context.Context, p provider.Pending) (provider.Instance, error) {
	if h.loseWait.Load() {
		return provider.Instance{}, lost("wait")
	}
	if h.waitDenied.Load() {
		// What the service client makes of a 403 on the operation read.
		return provider.Instance{}, &provider.Error{Kind: provider.ErrUnavailable, Op: "wait", Detail: "the service refused the read (INSUFFICIENT_SCOPE)"}
	}
	return h.Provider.Wait(ctx, p)
}

func (h *callHooks) Get(ctx context.Context, ref provider.Ref) (provider.Instance, error) {
	if h.getNotDelivered.Load() {
		return provider.Instance{}, notDeliveredErr("get")
	}
	return h.Provider.Get(ctx, ref)
}

func notDeliveredErr(op string) error {
	return &provider.Error{Kind: provider.ErrUnavailable, Op: op, Detail: "connection refused", Err: provider.ErrNotDelivered}
}

func (h *callHooks) Start(ctx context.Context, ref provider.Ref, rid string) (provider.Pending, error) {
	h.note("start", rid)
	if h.startNotFound.Load() {
		return provider.Pending{}, &provider.Error{Kind: provider.ErrNotFound, Op: "start", Code: "NOT_FOUND"}
	}
	p, err := h.Provider.Start(ctx, ref, rid)
	if h.loseStart.Load() {
		return provider.Pending{}, lost("start")
	}
	return p, err
}

func (h *callHooks) Destroy(ctx context.Context, ref provider.Ref, rid string) (provider.Pending, error) {
	h.note("destroy", rid)
	p, err := h.Provider.Destroy(ctx, ref, rid)
	if h.loseDestroy.Load() {
		return provider.Pending{}, lost("destroy")
	}
	return p, err
}

func (h *callHooks) List(ctx context.Context) (provider.Listing, error) {
	l, err := h.Provider.List(ctx)
	if h.dropDeleted.Load() {
		l.Deleted = nil
	}
	return l, err
}

func withHooks(h *callHooks) envOpt {
	return func(e *env, _ *fake.Options) {
		e.wrap = func(p provider.Provider) provider.Provider {
			h.Provider = p.(*fake.Provider)
			return h
		}
	}
}

// --- 1: an unknown create outcome is never cleaned up ---

// A create whose every reply was lost may have made the machine: the shed
// is kept starting (cloud_unreadable), nothing is destroyed and no delete
// marker is written, inside the fast-fail window and after it; the next
// pass sends the same create again and finds the machine it made.
func TestUnknownCreateOutcomeIsKept(t *testing.T) {
	for _, tc := range []struct {
		name string
		lose func(h *callHooks, on bool)
	}{
		{"every create reply lost", func(h *callHooks, on bool) { h.loseCreate.Store(on) }},
		{"the wait never answers", func(h *callHooks, on bool) { h.loseWait.Store(on) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := &callHooks{}
			e := newEnv(t, withHooks(h))
			tc.lose(h, true)
			shed, err := e.create("kept")
			if err != nil {
				t.Fatalf("create = %v; an unknown outcome is not a refusal", err)
			}
			e.idle()
			if got := e.reason("kept"); e.status("kept") != config.StatusStarting || got != config.StatusReasonCloudUnreadable {
				t.Fatalf("shed %s (%s), want starting (cloud_unreadable)", e.status("kept"), got)
			}
			if n := markerCount(t, e.dir); n != 0 {
				t.Fatalf("%d delete markers: an unknown outcome was cleaned up", n)
			}
			if n := e.instances(); n != 1 {
				t.Fatalf("%d machines; the one the create made must stand", n)
			}
			if e.prov.Count(fake.OpDestroy) != 0 {
				t.Fatal("a destroy was sent")
			}
			tc.lose(h, false)
			e.sweep()
			if got := e.settled("kept"); got.Status != config.StatusRunning || got.ID != shed.ID {
				t.Fatalf("after the pass: %q (%q)", got.Status, got.StatusReason)
			}
			if n := e.instances(); n != 1 {
				t.Fatalf("%d machines after the resumed create, want the one", n)
			}
			rids := h.sent("create")
			for _, rid := range rids {
				if rid != rids[0] {
					t.Fatalf("the create was sent with request ids %v; want one", rids)
				}
			}
		})
	}
}

// --- 2: R15 catches every earlier record shape ---

func writeRaw(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// Records an earlier server wrote stop the start whatever their shape: one
// the A1 code wrote whose spec no longer decodes (its size an object), one
// with no environment and a shed id for a reference, and a marker with no
// schema. Each is named.
func TestLegacyStateOfAnyShapeRefusesToStart(t *testing.T) {
	const id = "abcdefghijklmnopqrstuvwxyz"
	for _, tc := range []struct {
		name, file, body, names string
	}{
		{"an A1 record whose spec no longer decodes", "sheds/a1.json",
			`{"shed":{"name":"a1","id":"` + id + `","status":"running"},"env":"dev-a","zone":"zone-a","ref":"zone-a/shed-dev-a-x","spec":{"size":{"name":"standard","machine_type":"m"},"labels":{"shed_env":"dev-a"}},"max_run":3600}`,
			"shed a1"},
		{"a record with no environment and no schema", "sheds/plain.json",
			`{"shed":{"name":"plain","id":"` + id + `","status":"stopped"},"ref":"` + id + `"}`,
			"shed plain"},
		{"a marker with no schema", "deleting/" + id + ".json",
			`{"id":"` + id + `","name":"gone","ref":"` + id + `","at":"2026-11-01T00:00:00Z"}`,
			"an unfinished delete of shed gone"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if _, err := state.Open(dir); err != nil {
				t.Fatal(err)
			}
			writeRaw(t, filepath.Join(dir, tc.file), tc.body)
			_, err := New(Options{
				Config: &config.RemoteConfig{Provider: config.RemoteProviderFake, AllowTestDrivers: true}, Provider: fake.New(fake.Options{}),
				Links: newFakeLinks(), StateDir: dir, AuthorizedKeys: []string{testKey}, FirewallSources: []string{fake.DefaultIngressSource},
			})
			if !errors.Is(err, ErrLegacyState) || !strings.Contains(err.Error(), tc.names) {
				t.Fatalf("New = %v, want the legacy refusal naming %q", err, tc.names)
			}
		})
	}
}

// --- 3 and 11: adoption pins at first contact; probes keep their reasons ---

// A stopped, ready machine with no record is adopted, and its first start
// learns its host key (the attested key, compared with what sshd presents):
// never a record that can no longer learn one.
func TestAdoptedStoppedMachineLearnsItsKeyAtFirstStart(t *testing.T) {
	e := newEnv(t)
	e.fl.presented = fake.DefaultHostKey
	if err := e.b.Close(); err != nil {
		t.Fatal(err)
	}
	id := newID(t)
	spec, _ := provider.ShedSpec{CreatedAt: time.Now(), Network: "open", UserOrigin: "local"}.Encode()
	e.prov.Seed(provider.Instance{Ref: provider.Ref(id), Name: "found", State: provider.StateStopped,
		Labels: map[string]string{provider.LabelState: string(provider.ShedStateReady)}, Spec: spec})
	e.b = e.open()
	e.idle()
	rec := e.storedRecord("found")
	if rec.Shed.Status != config.StatusStopped || !rec.Ready || rec.HostKeyPin != "" {
		t.Fatalf("adopted %q ready %v pin %q", rec.Shed.Status, rec.Ready, rec.HostKeyPin)
	}
	if _, err := e.b.StartShed(ctxT(t), "found"); err != nil {
		t.Fatal(err)
	}
	if got := e.settled("found"); got.Status != config.StatusRunning {
		t.Fatalf("first start of the adopted shed: %q (%q)", got.Status, got.StatusReason)
	}
	if pin := e.storedRecord("found").HostKeyPin; pin == "" || !sameHostKey(pin, fake.DefaultHostKey) {
		t.Fatalf("pin %q, want the attested key", pin)
	}
}

// A running machine is adopted with no pin: the attestation alone pins
// nothing. A probe whose guest presents another key than the attested one
// records host_key (and pins nothing); one whose machine offers only access
// kinds this server does not know records access.
func TestAdoptedProbeKeepsItsReasons(t *testing.T) {
	t.Run("host_key", func(t *testing.T) {
		e := newEnv(t)
		e.fl.presented = testHostKey // not the attested fake.DefaultHostKey
		if err := e.b.Close(); err != nil {
			t.Fatal(err)
		}
		e.seedMachine("other-key", newID(t), provider.ShedStateReady, time.Now())
		e.b = e.open()
		e.idle()
		rec := e.storedRecord("other-key")
		if rec.Shed.StatusReason != config.StatusReasonHostKey || rec.HostKeyPin != "" {
			t.Fatalf("probe: %q (%q), pin %q; want host_key and no pin", rec.Shed.Status, rec.Shed.StatusReason, rec.HostKeyPin)
		}
	})
	t.Run("access", func(t *testing.T) {
		e := newEnv(t)
		e.fl.presented = fake.DefaultHostKey
		if err := e.b.Close(); err != nil {
			t.Fatal(err)
		}
		ref := e.seedMachine("odd-access", newID(t), provider.ShedStateReady, time.Now())
		e.prov.SetAccess(ref, nil, 2)
		e.b = e.open()
		e.idle()
		if got := e.storedRecord("odd-access").Shed.StatusReason; got != config.StatusReasonAccess {
			t.Fatalf("probe reason %q, want access", got)
		}
	})
}

// --- 4: a recreated create starts clean ---

// An unfinished create that was never accepted (every reply lost, no
// machine made) and whose machine the list names no deletion of is created
// again for a new guest (createGone): the old pin, layout and delivery
// history are not carried over, and the new machine's key is learned.
func TestRecreatedCreateStartsClean(t *testing.T) {
	h := &callHooks{}
	e := newEnv(t, manual(), withHooks(h))
	e.fl.presented = fake.DefaultHostKey
	h.createNotMade.Store(true)
	if _, err := e.create("again"); err != nil {
		t.Fatalf("create = %v; a lost reply is not a refusal", err)
	}
	e.idle()
	if got := e.reason("again"); e.status("again") != config.StatusStarting || got != config.StatusReasonCloudUnreadable {
		t.Fatalf("shed %s (%s), want starting (cloud_unreadable)", e.status("again"), got)
	}
	if err := e.b.Close(); err != nil {
		t.Fatal(err)
	}
	st, err := state.Open(e.dir)
	if err != nil {
		t.Fatal(err)
	}
	recs, _, _ := st.Records()
	old := recs[0]
	if old.Ref != "" || old.Machine != nil || old.Pending != nil {
		t.Fatalf("the create was accepted: ref %q machine %v pending %v", old.Ref, old.Machine != nil, old.Pending != nil)
	}
	oldRID := old.Spec.RequestID
	old.HostKeyPin = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOldOldOldOldOldOldOldOldOldOldOldOldOld"
	old.Layout = 99
	old.Delivery = delivery.NewRecord()
	old.Delivery.Steps.Stage1 = delivery.Step{State: delivery.StepDone}
	old.Delivery.Items = []delivery.RecordItem{{Name: "ssh-key"}}
	if err := st.SaveRecord(old); err != nil {
		t.Fatal(err)
	}
	h.createNotMade.Store(false)
	e.auto()
	e.b = e.open()
	if got := e.settled("again"); got.Status != config.StatusRunning {
		t.Fatalf("the recreated create: %q (%q)", got.Status, got.StatusReason)
	}
	rec := e.storedRecord("again")
	if !sameHostKey(rec.HostKeyPin, fake.DefaultHostKey) || rec.Layout != guest.Layout || rec.Spec.RequestID == oldRID {
		t.Fatalf("pin %q layout %d request id kept %v", rec.HostKeyPin, rec.Layout, rec.Spec.RequestID == oldRID)
	}
	if len(rec.Delivery.Items) != 0 {
		t.Fatalf("the gone guest's delivery carried over: %+v", rec.Delivery.Items)
	}
	if n := e.instances(); n != 1 {
		t.Fatalf("%d machines, want the one made", n)
	}
}

// --- 5: one answer for a name held by a machine being deleted ---

// slowNames answers creates and lists slowly (each within the service's own
// bound), and can fail every list.
type slowNames struct {
	*fake.Provider
	createDelay, listDelay time.Duration
	failList               atomic.Bool
}

func (s *slowNames) Create(ctx context.Context, spec provider.Spec) (provider.Pending, provider.Instance, error) {
	time.Sleep(s.createDelay)
	return s.Provider.Create(ctx, spec)
}

func (s *slowNames) List(ctx context.Context) (provider.Listing, error) {
	select {
	case <-time.After(s.listDelay):
	case <-ctx.Done():
		return provider.Listing{}, lost("list")
	}
	if s.failList.Load() {
		return provider.Listing{}, lost("list")
	}
	return s.Provider.List(ctx)
}

// A create meeting EXISTS from a machine being deleted has one answer,
// given inside the fast-fail window: never a 201 before it is decided, never
// SHED_ALREADY_EXISTS for a machine that is going away, and never later than
// the window. A list that fails throughout is 503 COMPUTE_UNAVAILABLE.
func TestDeletingNameHasOneAnswer(t *testing.T) {
	for _, tc := range []struct {
		name     string
		failList bool
		status   int
		code     string
	}{
		{"slow create and list", false, http.StatusConflict, config.ErrComputeDeleting},
		{"the list fails", true, http.StatusServiceUnavailable, config.ErrComputeUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// EXISTS comes late in the window and the lists that follow
			// are slow: the decision is still made by the window's end.
			s := &slowNames{createDelay: 150 * time.Millisecond, listDelay: 40 * time.Millisecond}
			e := newEnv(t, tuned(func(o *Options) { o.FastFailWindow = 500 * time.Millisecond }), func(e *env, _ *fake.Options) {
				e.wrap = func(p provider.Provider) provider.Provider { s.Provider = p.(*fake.Provider); return s }
			})
			ref := e.seedMachine("held", newID(t), provider.ShedStateReady, time.Now())
			e.prov.SetDeleting(ref)
			s.failList.Store(tc.failList)
			// The window bounds the decision. The answer also holds two
			// durable writes outside it: the record's save before the
			// window starts and, on a refusal, its removal (a directory
			// fsync) after the decision. On a busy disk each was measured
			// at hundreds of milliseconds (883ms and 728ms), with the
			// decision itself within 100µs of the deadline, so the bound
			// allows for them; a create that waited for the holder's delete
			// (which never ends here) is still caught.
			began := time.Now()
			shed, err := e.create("held")
			if took := time.Since(began); took > 500*time.Millisecond+1500*time.Millisecond {
				t.Fatalf("the answer took %s, past the 500ms window and the allowance for its durable writes", took)
			}
			if shed != nil {
				t.Fatalf("create answered %q before the name was decided", shed.Status)
			}
			computeRefusal(t, err, tc.status, tc.code, "")
			if errors.Is(err, config.ErrShedAlreadyExistsSentinel) {
				t.Fatalf("create = %v: SHED_ALREADY_EXISTS for a machine being deleted", err)
			}
			if _, gerr := e.b.GetShed(ctxT(t), "held"); !errors.Is(gerr, config.ErrShedNotFoundSentinel) {
				t.Fatalf("the refused create left a record: %v", gerr)
			}
		})
	}
}

// --- 6: request ids are stored; resumed jobs wait on their Pending ---

// A start whose replies are lost is kept; the pass that runs it again sends
// the request id stored before the first send.
func TestStartRequestIDIsStoredAndReused(t *testing.T) {
	h := &callHooks{}
	e := newEnv(t, withHooks(h))
	e.running("box")
	if _, err := e.b.StopShed(ctxT(t), "box"); err != nil {
		t.Fatal(err)
	}
	e.settled("box")
	h.loseStart.Store(true)
	if _, err := e.b.StartShed(ctxT(t), "box"); err != nil {
		t.Fatal(err)
	}
	e.idle()
	if m := e.storedRecord("box").Mutation; m == nil || m.Call != state.JobStart || m.RequestID == "" {
		t.Fatalf("the kept start's stored request id: %+v", m)
	}
	// Resumed in a new process, as after a restart.
	h.loseStart.Store(false)
	e.restart()
	if got := e.settled("box"); got.Status != config.StatusRunning {
		t.Fatalf("the resumed start: %q (%q)", got.Status, got.StatusReason)
	}
	rids := h.sent("start")
	if len(rids) < 2 {
		t.Fatalf("starts sent: %v", rids)
	}
	for _, rid := range rids {
		if rid != rids[0] {
			t.Fatalf("the start was sent with request ids %v; want one", rids)
		}
	}
	if m := e.storedRecord("box").Mutation; m != nil {
		t.Fatalf("a finished start keeps its request id: %+v", m)
	}
}

// A start or stop resumed after a restart whose call the service had
// accepted waits on the Pending it stored and sends nothing again.
func TestResumedStartAndStopWaitOnTheirPending(t *testing.T) {
	for _, tc := range []struct {
		name string
		op   fake.Op
		act  func(e *env) error
		want string
	}{
		{"start", fake.OpStart, func(e *env) error { _, err := e.b.StartShed(ctxT(e.t), "box"); return err }, config.StatusRunning},
		{"stop", fake.OpStop, func(e *env) error { _, err := e.b.StopShed(ctxT(e.t), "box"); return err }, config.StatusStopped},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t, manual())
			e.auto()
			e.running("box")
			if tc.op == fake.OpStart {
				if _, err := e.b.StopShed(ctxT(t), "box"); err != nil {
					t.Fatal(err)
				}
				e.settled("box")
			}
			e.stopAuto()
			if err := tc.act(e); err != nil {
				t.Fatal(err)
			}
			waitFor(t, func() bool { r := e.storedRecord("box"); return r.Pending != nil && r.Pending.Operation != "" })
			sent := e.prov.Count(tc.op)
			if err := e.b.Close(); err != nil {
				t.Fatal(err)
			}
			e.auto()
			e.b = e.open()
			if got := e.settled("box"); got.Status != tc.want {
				t.Fatalf("the resumed %s: %q (%q)", tc.name, got.Status, got.StatusReason)
			}
			if n := e.prov.Count(tc.op); n != sent {
				t.Fatalf("%d %s calls after the restart, want none (%d before)", n-sent, tc.name, sent)
			}
		})
	}
}

// A delete's destroy whose replies are lost is retried with the request id
// its marker stored.
func TestDestroyRequestIDIsStoredInTheMarker(t *testing.T) {
	h := &callHooks{}
	e := newEnv(t, withHooks(h))
	e.running("box")
	h.loseDestroy.Store(true)
	if err := e.b.DeleteShed(ctxT(t), "box"); err != nil {
		t.Fatal(err)
	}
	e.idle()
	st, _ := state.Open(e.dir)
	markers, _ := st.Markers()
	if len(markers) != 1 || markers[0].DestroyID == "" || markers[0].LabelID == "" {
		t.Fatalf("markers %+v, want one holding its request ids", markers)
	}
	h.loseDestroy.Store(false)
	e.restart()
	e.idle()
	rids := h.sent("destroy")
	if len(rids) < 2 {
		t.Fatalf("destroys sent: %v", rids)
	}
	for _, rid := range rids {
		if rid != markers[0].DestroyID {
			t.Fatalf("destroys sent with %v; want the stored %s", rids, markers[0].DestroyID)
		}
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(testTimeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("the condition never held")
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// --- 8: degraded start reaches running sheds ---

// With the service down at start, a running shed's guest is reached over
// the access and pin its record holds: exec gets to the guest (the fake
// agent has no console) and is never 503 COMPUTE_UNAVAILABLE.
func TestDegradedExecReachesARunningShed(t *testing.T) {
	e := newEnv(t)
	e.running("live")
	if err := e.b.Close(); err != nil {
		t.Fatal(err)
	}
	e.prov.FailNext(fake.OpCatalog, lost("catalog"))
	b, err := New(Options{Config: e.cfg, Provider: e.prov, Links: e.links, StateDir: e.dir, AuthorizedKeys: []string{testKey},
		FirewallSources: []string{fake.DefaultIngressSource}, SweepInterval: -1, ListRetry: time.Hour, Logf: e.logf})
	if err != nil {
		t.Fatal(err)
	}
	e.b = b
	go b.Reconcile(ctxT(t))
	err = b.Exec(ctxT(t), "live", backend.ExecOptions{Cmd: []string{"true"}})
	var cr *config.ComputeRefusal
	if errors.As(err, &cr) || errors.Is(err, ErrNotReady) || err == nil || !strings.Contains(err.Error(), "no console") {
		t.Fatalf("degraded exec = %v, want it to reach the guest", err)
	}
	if _, err := b.StopShed(ctxT(t), "live"); !errors.As(err, &cr) || cr.Code != config.ErrComputeUnavailable {
		t.Fatalf("degraded stop = %v, want COMPUTE_UNAVAILABLE", err)
	}
}

// --- 9: the size a create gets is the one the caps were checked against ---

// catalogSwap answers the catalog it holds, once one is set.
type catalogSwap struct {
	*fake.Provider
	cat atomic.Pointer[provider.Catalog]
}

func (c *catalogSwap) Catalog(ctx context.Context) (provider.Catalog, error) {
	if cat := c.cat.Load(); cat != nil {
		return *cat, nil
	}
	return c.Provider.Catalog(ctx)
}

func TestCreateUsesTheSizeTheAPIChecked(t *testing.T) {
	swap := &catalogSwap{}
	e := newEnv(t, func(e *env, _ *fake.Options) {
		e.wrap = func(p provider.Provider) provider.Provider { swap.Provider = p.(*fake.Provider); return swap }
	})
	req := config.CreateShedRequest{Name: "sized", NoProvision: true, CPUs: 2, MemoryMB: 8192, SizeName: "standard"}
	if _, err := e.b.CreateShed(ctxT(t), req); err != nil {
		t.Fatalf("create of the checked size: %v", err)
	}
	if m := e.storedRecord("sized").Spec; m == nil || m.Size != "standard" {
		t.Fatalf("spec size %+v", m)
	}
	// The catalog changes before the next create runs: standard now has
	// four CPUs, which nobody checked.
	cat := fake.DefaultCatalog()
	cat.Sizes[0].CPUs = 4
	swap.cat.Store(&cat)
	e.sweep()
	req.Name = "resized"
	_, err := e.b.CreateShed(ctxT(t), req)
	if !errors.Is(err, config.ErrSizeUnavailableSentinel) {
		t.Fatalf("create after the catalog changed = %v, want SIZE_UNAVAILABLE", err)
	}
}

// --- 10: a lifecycle call's 404 drops nothing the list still names ---

// A start answered NOT_FOUND while the list still names the machine (and
// an exact read finds it) records the shed stopped and keeps it: records
// are dropped only on omission from the list and a 404 read.
func TestStartNotFoundDropsNothingTheListNames(t *testing.T) {
	h := &callHooks{}
	e := newEnv(t, withHooks(h))
	e.running("box")
	if _, err := e.b.StopShed(ctxT(t), "box"); err != nil {
		t.Fatal(err)
	}
	e.settled("box")
	h.startNotFound.Store(true)
	if _, err := e.b.StartShed(ctxT(t), "box"); err != nil {
		t.Fatal(err)
	}
	if got := e.settled("box"); got.Status != config.StatusStopped {
		t.Fatalf("after a NOT_FOUND start: %q", got.Status)
	}
	// Gone for real: the list omits it and the read answers 404.
	h.startNotFound.Store(false)
	e.prov.ExternalDelete(e.ref("box"))
	h.startNotFound.Store(true)
	if _, err := e.b.StartShed(ctxT(t), "box"); err != nil {
		t.Fatal(err)
	}
	e.idle()
	if _, err := e.b.GetShed(ctxT(t), "box"); !errors.Is(err, config.ErrShedNotFoundSentinel) {
		t.Fatalf("a shed whose machine is confirmed gone: %v, want dropped", err)
	}
}

// --- 12: a 413 is the server's bug ---

// A create the service answers 413 reaches the caller as 500 BACKEND_ERROR,
// never as the caller's invalid request.
func TestPayloadTooLargeIsABackendError(t *testing.T) {
	w := servicetest.Start(t, servicetest.Options{})
	w.Ref.ComputeForceReply("create", &refserver.ComputeReply{Status: http.StatusRequestEntityTooLarge})
	spec, err := provider.NewSpec(newID(t), "big", provider.Owner{User: "ada"}, provider.ShedSpec{CreatedAt: time.Now(), Network: "open"}, []string{testKey}, "standard", 0, provider.ImageDefault)
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = w.Client().Create(context.Background(), spec)
	if err == nil {
		t.Fatal("a 413 create succeeded")
	}
	mapped := mapRefusal(err)
	if errors.Is(mapped, config.ErrInvalidShedRequestSentinel) {
		t.Fatalf("413 mapped to the caller's invalid request: %v", mapped)
	}
	computeRefusal(t, mapped, http.StatusInternalServerError, config.ErrBackendError, "")
}

// --- 13: a reconnect tries the cached access first ---

// The address the record holds is given first, after a connection that
// worked and dropped too; the machine is read again only after a dial to
// it failed.
func TestReconnectTriesTheCachedAccessFirst(t *testing.T) {
	e := newEnv(t)
	e.running("box")
	rec := e.storedRecord("box")
	h := &hostKeys{b: e.b, name: "box", id: rec.Shed.ID}
	gets := func() int { return e.prov.Count(fake.OpGet) }

	n := gets()
	addr, err := h.addr(ctxT(t))
	if err != nil || addr == "" || gets() != n {
		t.Fatalf("first address %q, %v, %d reads; want the cached one and no read", addr, err, gets()-n)
	}
	h.connected()
	addr, err = h.addr(ctxT(t))
	if err != nil || addr == "" || gets() != n {
		t.Fatalf("after a connection that dropped: %q, %v, %d reads; want the cached one first", addr, err, gets()-n)
	}
	// No Connected: the last dial failed. Now the machine is read.
	if _, err := h.addr(ctxT(t)); err != nil || gets() != n+1 {
		t.Fatalf("after a failed dial: %v, %d reads; want one", err, gets()-n)
	}
}

// The JSON of a record carries its schema, so a later build can tell its
// own records from an earlier server's before decoding them.
func TestSavedRecordsCarryTheSchema(t *testing.T) {
	e := newEnv(t)
	e.running("box")
	data, err := os.ReadFile(filepath.Join(e.dir, "sheds", "box.json"))
	if err != nil {
		t.Fatal(err)
	}
	var probe struct {
		Schema int `json:"schema"`
	}
	if err := json.Unmarshal(data, &probe); err != nil || probe.Schema != state.Schema {
		t.Fatalf("schema %d, %v", probe.Schema, err)
	}
}

// --- re-review 1: a never-delivered access read never cleans up a create ---

// The create was accepted and its machine runs with no access yet; every
// read of it then never reaches the service. The guest's wait ends with no
// answer: the create is kept for the next pass (unreachable), and nothing is
// cleaned up. The access read's ErrNotDelivered never passes for the
// create's.
func TestAccessReadNotDeliveredKeepsTheCreate(t *testing.T) {
	h := &callHooks{}
	e := newEnv(t, withHooks(h), func(e *env, o *fake.Options) { o.NoHostKey = true },
		tuned(func(o *Options) { o.BootTimeout = 300 * time.Millisecond }))
	e.fl.presented = fake.DefaultHostKey
	shed, err := e.create("noaccess")
	if err != nil {
		t.Fatal(err)
	}
	h.getNotDelivered.Store(true)
	e.idle()
	rec := e.storedRecord("noaccess")
	if rec.Shed.Status != config.StatusStarting || rec.Shed.StatusReason != config.StatusReasonUnreachable || rec.Ref != provider.Ref(shed.ID) {
		t.Fatalf("shed %q (%q), want starting (unreachable)", rec.Shed.Status, rec.Shed.StatusReason)
	}
	if n, m := e.instances(), markerCount(t, e.dir); n != 1 || m != 0 || e.prov.Count(fake.OpDestroy) != 0 {
		t.Fatalf("%d machines, %d markers, %d destroys: the accepted create was cleaned up", n, m, e.prov.Count(fake.OpDestroy))
	}
	// The service answers again and the guest publishes its key: the next
	// pass finishes the create.
	h.getNotDelivered.Store(false)
	e.prov.PublishHostKey(provider.Ref(shed.ID), fake.DefaultHostKey)
	e.sweep()
	if got := e.settled("noaccess"); got.Status != config.StatusRunning {
		t.Fatalf("after the pass: %q (%q)", got.Status, got.StatusReason)
	}
}

// The access read that never reached the service is told apart from the
// create's own non-delivery.
func TestHostKeysRefreshStripsNotDelivered(t *testing.T) {
	h := &callHooks{}
	e := newEnv(t, withHooks(h))
	e.running("box")
	rec := e.storedRecord("box")
	h.getNotDelivered.Store(true)
	hk := &hostKeys{b: e.b, name: "box", id: rec.Shed.ID}
	if _, err := hk.refresh(ctxT(t), rec); err == nil || errors.Is(err, provider.ErrNotDelivered) || !errors.Is(err, provider.ErrUnavailable) {
		t.Fatalf("refresh = %v, want unavailable and not ErrNotDelivered", err)
	}
}

// --- re-review 2: a kept create resolves its earlier send before any gate ---

// The service made the machine and every create reply was lost; before the
// next pass the firewall check starts failing. The pass finds the machine by
// its shed id first and finishes the create on it: the failing check never
// turns a machine that was made into a refused create to clean up.
func TestKeptCreateResolvesBeforeTheFirewallGate(t *testing.T) {
	h := &callHooks{}
	e := newEnv(t, withHooks(h))
	h.loseCreate.Store(true)
	shed, err := e.create("kept")
	if err != nil {
		t.Fatal(err)
	}
	e.idle()
	h.loseCreate.Store(false)
	brokenSources(e.prov)
	e.sweep()
	if got := e.settled("kept"); got.Status != config.StatusRunning || got.ID != shed.ID {
		t.Fatalf("after the pass: %q (%q)", got.Status, got.StatusReason)
	}
	if n, m := e.instances(), markerCount(t, e.dir); n != 1 || m != 0 || e.prov.Count(fake.OpDestroy) != 0 {
		t.Fatalf("%d machines, %d markers, %d destroys", n, m, e.prov.Count(fake.OpDestroy))
	}
	if n := len(h.sent("create")); n != unknownRetries+1 {
		t.Fatalf("%d creates sent, want only the first run's %d", n, unknownRetries+1)
	}
}

// --- re-review 3: a later authorization refusal is not a refusal of the create ---

func TestLaterDenialKeepsAnAcceptedCreate(t *testing.T) {
	t.Run("the retry is refused at the scope", func(t *testing.T) {
		h := &callHooks{}
		e := newEnv(t, withHooks(h))
		h.loseCreate.Store(true)
		h.createDeniedAfter.Store(1)
		if _, err := e.create("denied"); err != nil {
			t.Fatalf("create = %v; the first send's outcome is unknown, so the create is kept", err)
		}
		e.idle()
		if st, why := e.status("denied"), e.reason("denied"); st != config.StatusStarting || why != config.StatusReasonCloudUnreadable {
			t.Fatalf("shed %s (%s), want starting (cloud_unreadable)", st, why)
		}
		if n, m := e.instances(), markerCount(t, e.dir); n != 1 || m != 0 {
			t.Fatalf("%d machines and %d markers, want the machine and none", n, m)
		}
	})
	t.Run("the operation read is refused at the scope", func(t *testing.T) {
		h := &callHooks{}
		e := newEnv(t, withHooks(h))
		h.waitDenied.Store(true)
		if _, err := e.create("denied"); err != nil {
			t.Fatalf("create = %v", err)
		}
		e.idle()
		if st := e.status("denied"); st != config.StatusStarting {
			t.Fatalf("shed %s, want kept starting", st)
		}
		if n, m := e.instances(), markerCount(t, e.dir); n != 1 || m != 0 || e.prov.Count(fake.OpDestroy) != 0 {
			t.Fatalf("%d machines, %d markers: an accepted create was cleaned up", n, m)
		}
		h.waitDenied.Store(false)
		e.sweep()
		if got := e.settled("denied"); got.Status != config.StatusRunning {
			t.Fatalf("after the pass: %q", got.Status)
		}
	})
	t.Run("the first send is refused at the scope", func(t *testing.T) {
		// Nothing was sent before it: a definite refusal, no record.
		e := newEnv(t)
		e.prov.FailNext(fake.OpCreate, &provider.Error{Kind: provider.ErrDenied, Op: "create", Code: "COMPUTE_DENIED"})
		_, err := e.create("denied")
		computeRefusal(t, mapRefusal(err), http.StatusForbidden, config.ErrComputeDenied, "")
		if _, gerr := e.b.GetShed(ctxT(t), "denied"); !errors.Is(gerr, config.ErrShedNotFoundSentinel) {
			t.Fatalf("the refused create left a record: %v", gerr)
		}
	})
}

// --- re-review 4: never delivered is decided inside the window ---

// A create whose connection never comes up (each attempt ends with its
// context, never delivered) is answered 503 COMPUTE_UNAVAILABLE inside the
// fast-fail window, with no record: never a 201 followed by a failure.
func TestBlackholedCreateAnswersInsideTheWindow(t *testing.T) {
	h := &callHooks{}
	const window = 300 * time.Millisecond
	e := newEnv(t, withHooks(h), tuned(func(o *Options) { o.FastFailWindow = window }))
	h.blackhole.Store(true)
	// The window bounds the decision. The answer also holds durable writes
	// outside it (the record's save before the window starts and, on the
	// refusal, its removal after the decision), which a busy disk was seen
	// to take hundreds of milliseconds over (438ms in all for a 300ms
	// window), so the bound allows for them as TestDeletingNameHasOneAnswer
	// does. A create that waited on the blackholed connection without end
	// is still caught, and the answer must be the window's refusal below.
	began := time.Now()
	shed, err := e.create("dark")
	if took := time.Since(began); took > window+1500*time.Millisecond {
		t.Fatalf("the answer took %s, past the %s window and the allowance for its durable writes", took, window)
	}
	if shed != nil {
		t.Fatalf("create answered %q", shed.Status)
	}
	computeRefusal(t, mapRefusal(err), http.StatusServiceUnavailable, config.ErrComputeUnavailable, "")
	e.idle()
	if _, gerr := e.b.GetShed(ctxT(t), "dark"); !errors.Is(gerr, config.ErrShedNotFoundSentinel) {
		t.Fatalf("a create never delivered left a record: %v", gerr)
	}
}

// The setup helper createFor makes a create again when its send never left
// within the window (a loaded machine): once, here, after the first is
// blackholed; the shed then settles running, made once.
func TestSetupCreateIsMadeAgainWhenNeverDelivered(t *testing.T) {
	h := &callHooks{}
	e := newEnv(t, withHooks(h), tuned(func(o *Options) { o.FastFailWindow = 200 * time.Millisecond }))
	h.blackholeNext.Store(1)
	e.createFor("setup", "")
	if n := len(h.sent("create")); n != 2 {
		t.Fatalf("%d creates sent, want 2 (one never delivered, one made)", n)
	}
	if got := e.instances(); got != 1 {
		t.Fatalf("%d machines, want 1", got)
	}
}

// --- re-review 5: a request id that cannot be written sends nothing ---

func TestRequestIDWriteFailureSendsNothing(t *testing.T) {
	t.Run("start", func(t *testing.T) {
		e := newEnv(t)
		e.running("box")
		if _, err := e.b.StopShed(ctxT(t), "box"); err != nil {
			t.Fatal(err)
		}
		e.settled("box")
		var fail atomic.Bool
		fail.Store(true)
		e.b.saveRecord = func(r *state.Record) error {
			if fail.Load() && r.Mutation != nil && r.Mutation.Call == state.JobStart {
				return errors.New("disk full")
			}
			return e.b.store.SaveRecord(r)
		}
		starts := e.prov.Count(fake.OpStart)
		if _, err := e.b.StartShed(ctxT(t), "box"); err != nil {
			t.Fatal(err)
		}
		e.idle()
		if n := e.prov.Count(fake.OpStart) - starts; n != 0 {
			t.Fatalf("%d starts sent with no request id on disk", n)
		}
		if rec := e.storedRecord("box"); rec.Shed.Status != config.StatusStarting || rec.Job != state.JobStart || rec.Mutation != nil {
			t.Fatalf("record %q job %q mutation %+v, want the start kept and no id", rec.Shed.Status, rec.Job, rec.Mutation)
		}
		fail.Store(false)
		e.sweep()
		if got := e.settled("box"); got.Status != config.StatusRunning {
			t.Fatalf("after the pass: %q", got.Status)
		}
	})
	t.Run("create", func(t *testing.T) {
		e := newEnv(t)
		e.b.saveRecord = func(r *state.Record) error {
			if r.Mutation != nil && r.Mutation.Call == state.JobCreate {
				return errors.New("disk full")
			}
			return e.b.store.SaveRecord(r)
		}
		_, err := e.create("box")
		if !errors.Is(err, errNotRecorded) || e.prov.Count(fake.OpCreate) != 0 {
			t.Fatalf("create = %v with %d creates sent; want refused before any send", err, e.prov.Count(fake.OpCreate))
		}
		if _, gerr := e.b.GetShed(ctxT(t), "box"); !errors.Is(gerr, config.ErrShedNotFoundSentinel) {
			t.Fatalf("record left: %v", gerr)
		}
	})
}

// --- re-review 6: an unresolved stop keeps its state and the queued start ---

// A stop that was accepted and whose wait never answers is kept as it is:
// its Pending, its request id and a start queued behind it. The next pass
// waits on the stored Pending (no second stop is sent) and then runs the
// queued start.
func TestUnresolvedStopKeepsItsQueuedStart(t *testing.T) {
	h := &callHooks{}
	e := newEnv(t, withHooks(h))
	e.running("box")
	h.loseWait.Store(true)
	if _, err := e.b.StopShed(ctxT(t), "box"); err != nil {
		t.Fatal(err)
	}
	e.idle()
	if _, err := e.b.StartShed(ctxT(t), "box"); err != nil {
		t.Fatal(err)
	}
	e.idle()
	rec := e.storedRecord("box")
	if rec.Shed.Status != config.StatusStopping || rec.Pending == nil || rec.Pending.Want != provider.StateStopped ||
		rec.Mutation == nil || rec.Mutation.Call != state.JobStop || rec.Queued != state.QueuedStart {
		t.Fatalf("record %q pending %+v mutation %+v queued %q", rec.Shed.Status, rec.Pending, rec.Mutation, rec.Queued)
	}
	stops := e.prov.Count(fake.OpStop)
	h.loseWait.Store(false)
	e.sweep()
	if got := e.settled("box"); got.Status != config.StatusRunning {
		t.Fatalf("after the pass: %q (%q), want running (the queued start)", got.Status, got.StatusReason)
	}
	if n := e.prov.Count(fake.OpStop) - stops; n != 0 {
		t.Fatalf("%d stops sent again, want none", n)
	}
}

// --- re-review 7: a job's drop carries the deletion's cause ---

// A start answered NOT_FOUND for a machine the service deleted at its
// lease's end drops the record with the cause, in the log and as the
// service-deletion audit event, as the periodic pass would.
func TestJobDropCarriesTheDeletionCause(t *testing.T) {
	h := &callHooks{}
	auditOpt, path := withAudit(t)
	e := newEnv(t, withHooks(h), auditOpt)
	e.running("box")
	if _, err := e.b.StopShed(ctxT(t), "box"); err != nil {
		t.Fatal(err)
	}
	e.settled("box")
	e.prov.DeleteAs(e.ref("box"), provider.CauseLeaseEnded)
	h.startNotFound.Store(true)
	if _, err := e.b.StartShed(ctxT(t), "box"); err != nil {
		t.Fatal(err)
	}
	e.idle()
	if _, err := e.b.GetShed(ctxT(t), "box"); !errors.Is(err, config.ErrShedNotFoundSentinel) {
		t.Fatalf("GetShed = %v, want dropped", err)
	}
	if log := strings.Join(e.logLines(), "\n"); !strings.Contains(log, "deleted its machine (cause lease_ended)") {
		t.Fatalf("no line names the cause:\n%s", log)
	}
	if !auditHas(t, path, provider.CauseLeaseEnded) {
		t.Fatal("no service-deletion event in the audit log")
	}
}
