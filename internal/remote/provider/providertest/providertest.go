// Package providertest is the conformance suite every provider.Provider must
// pass: the in-memory fake, and the compute service's client against the
// reference compute service. A provider's own tests call Run with a Harness
// that builds a fresh world for each subtest and the out-of-band changes the
// suite needs (finishing work, a lost reply, an expired operation, a delete
// at the service, the clock).
//
// The suite checks what the seam in package provider promises: idempotent
// calls by shed id and by request id, the state rules of start and stop, a
// delete that ends the work in progress, refusals on the call and on the
// operation, labels with a condition, leases and extends, recent deletions
// with their causes, and that a Pending stored by one process is waited on
// by another.
package providertest

import (
	"context"
	"encoding/json"
	"errors"
	"net/netip"
	"slices"
	"testing"
	"time"

	"github.com/charliek/shed/internal/config"
	"github.com/charliek/shed/internal/remote/provider"
)

// Harness describes the provider under test.
type Harness struct {
	// New returns a fresh world. It is called once per subtest.
	New func(t *testing.T) *Target
	// Timeout bounds each Wait. Zero means 20 s.
	Timeout time.Duration
}

// Target is one world: a provider and what the suite needs to drive it.
type Target struct {
	// Provider is the provider under test.
	Provider provider.Provider
	// Fresh returns a new provider over the same machines: what a server
	// process started again holds (its own token, nothing in memory).
	Fresh func() provider.Provider
	// Settle finishes the work in progress on a machine. The world's
	// operations finish only by it.
	Settle func(ref provider.Ref)
	// LoseNextReply has the next call of the kind (create, start, stop,
	// delete, labels, extend) take effect and keep its answer, and its
	// reply lost on the way back.
	LoseNextReply func(call string)
	// ExpireOperations has every finished operation forgotten, as one kept
	// past its 24 hours is.
	ExpireOperations func()
	// DeleteOutside deletes a machine at the service (provider.CausePage)
	// or outside it (provider.CauseExternal).
	DeleteOutside func(ref provider.Ref, cause string)
	// Advance moves the world's clock by d and ends every lease that is
	// then due.
	Advance func(d time.Duration)
	// FailNextCreate has the next create accepted and its work fail with
	// the cloud's quota.
	FailNextCreate func()
	// User is a person the provider makes machines for; Client a client
	// registered for the server.
	User, Client string
	// Key is an authorized key line to create with.
	Key string
}

// Run runs the suite.
func Run(t *testing.T, h Harness) {
	timeout := h.Timeout
	if timeout == 0 {
		timeout = 20 * time.Second
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 2*timeout)
			defer cancel()
			w := &world{t: t, Target: h.New(t), ctx: ctx, timeout: timeout}
			tc.run(w)
		})
	}
}

type world struct {
	*Target
	t       *testing.T
	ctx     context.Context
	timeout time.Duration
}

var cases = []struct {
	name string
	run  func(*world)
}{
	{"catalog", testCatalog},
	{"create then wait", testCreate},
	{"create is idempotent by shed id", testCreateIdempotent},
	{"a request id is answered once", testRequestID},
	{"stop and start", testStopStart},
	{"busy", testBusy},
	{"labels", testLabels},
	{"extend", testExtend},
	{"a client's machine", testClient},
	{"destroy", testDestroy},
	{"destroy ends the work in progress", testDestroyEndsWork},
	{"deleted outside the server", testDeletedOutside},
	{"the lease ends", testLeaseEnds},
	{"a refusal that arrives late", testLateRefusal},
	{"unknown machines", testUnknown},
	{"usage", testUsage},
	{"ingress", testIngress},
	{"a lost reply for each mutation", testLostReplies},
	{"an expired operation for each mutation", testExpiredOperations},
	{"a fresh process waits on a stored pending", testFreshProcess},
}

// --- helpers ---

func newShedID() string {
	id, err := config.NewShedID()
	if err != nil {
		panic(err)
	}
	return id
}

func (w *world) spec(name string, owner provider.Owner) provider.Spec {
	w.t.Helper()
	spec, err := provider.NewSpec(newShedID(), name, owner,
		provider.ShedSpec{CreatedAt: time.Now().UTC(), Network: "open", UserOrigin: "directory"},
		[]string{w.Key}, "standard", 0, provider.ImageDefault)
	if err != nil {
		w.t.Fatalf("NewSpec: %v", err)
	}
	return spec
}

// sub is w for a subtest.
func (w *world) sub(t *testing.T) *world {
	cp := *w
	cp.t = t
	return &cp
}

func (w *world) personSpec(name string) provider.Spec {
	return w.spec(name, provider.Owner{User: w.User})
}

func (w *world) wait(p provider.Provider, pend provider.Pending) (provider.Instance, error) {
	ctx, cancel := context.WithTimeout(w.ctx, w.timeout)
	defer cancel()
	return p.Wait(ctx, pend)
}

// waitSettled settles the machine's work and waits for it.
func (w *world) settled(pend provider.Pending) provider.Instance {
	w.t.Helper()
	w.Settle(pend.Ref)
	inst, err := w.wait(w.Provider, pend)
	if err != nil {
		w.t.Fatalf("Wait(%+v): %v", pend, err)
	}
	return inst
}

// running creates a person's machine and waits for it to run.
func (w *world) running(name string) provider.Instance {
	w.t.Helper()
	pend, _, err := w.Provider.Create(w.ctx, w.personSpec(name))
	if err != nil {
		w.t.Fatalf("Create(%s): %v", name, err)
	}
	inst := w.settled(pend)
	if inst.State != provider.StateRunning {
		w.t.Fatalf("machine %s is %s after its create, want running", name, inst.State)
	}
	return inst
}

// stopped creates a machine and stops it.
func (w *world) stopped(name string) provider.Instance {
	w.t.Helper()
	inst := w.running(name)
	pend, err := w.Provider.Stop(w.ctx, inst.Ref, provider.NewRequestID())
	if err != nil {
		w.t.Fatalf("Stop: %v", err)
	}
	return w.settled(pend)
}

func (w *world) get(ref provider.Ref) (provider.Instance, error) {
	return w.Provider.Get(w.ctx, ref)
}

func (w *world) list() provider.Listing {
	w.t.Helper()
	l, err := w.Provider.List(w.ctx)
	if err != nil {
		w.t.Fatalf("List: %v", err)
	}
	return l
}

func deletionOf(l provider.Listing, ref provider.Ref) (provider.Deletion, bool) {
	for _, d := range l.Deleted {
		if d.Ref == ref {
			return d, true
		}
	}
	return provider.Deletion{}, false
}

func listed(l provider.Listing, ref provider.Ref) bool {
	for _, i := range l.Instances {
		if i.Ref == ref {
			return true
		}
	}
	return false
}

func wantKind(t *testing.T, what string, err, kind error) {
	t.Helper()
	if !errors.Is(err, kind) {
		t.Fatalf("%s = %v, want %v", what, err, kind)
	}
}

func wantCode(t *testing.T, what string, err error, code string) {
	t.Helper()
	if got := provider.CodeOf(err); got != code {
		t.Fatalf("%s: code %q (%v), want %s", what, got, err, code)
	}
}

// --- cases ---

func testCatalog(w *world) {
	c, err := w.Provider.Catalog(w.ctx)
	if err != nil {
		w.t.Fatalf("Catalog: %v", err)
	}
	if _, ok := c.SizeNamed(c.DefaultSize); !ok {
		w.t.Errorf("the default size %q is not one of the sizes %v", c.DefaultSize, c.Sizes)
	}
	if _, ok := c.SizeNamed("standard"); !ok {
		w.t.Errorf("no size standard: %v", c.Sizes)
	}
	if !c.HasImage(provider.ImageDefault) || slices.Contains(c.Aliases, provider.ImageDefault) {
		w.t.Errorf("images: default %v, aliases %v", c.HasImage(provider.ImageDefault), c.Aliases)
	}
	if !slices.Contains(c.AccessKinds, provider.AccessSSH) {
		w.t.Errorf("access kinds %v lack ssh", c.AccessKinds)
	}
	if c.Policy.PersonMaxAge < c.Policy.PersonInitialLease || c.Policy.PersonInitialLease <= 0 {
		w.t.Errorf("policy %+v", c.Policy)
	}
}

func testCreate(w *world) {
	spec := w.personSpec("widgets")
	pend, inst, err := w.Provider.Create(w.ctx, spec)
	if err != nil {
		w.t.Fatalf("Create: %v", err)
	}
	if pend.Ref != provider.Ref(spec.ShedID) || pend.Want != provider.StateRunning || pend.Operation == "" {
		w.t.Errorf("Pending = %+v, want the shed id, an operation and Want running", pend)
	}
	// The create's answer carries the machine at once.
	if inst.Ref != pend.Ref || inst.Name != "widgets" || inst.User != w.User || inst.State != provider.StateStarting {
		w.t.Errorf("the create's machine = %+v", inst)
	}
	if inst.Size.Name != "standard" || inst.Size.CPUs <= 0 || inst.DiskGB <= 0 || inst.Image != provider.ImageDefault || inst.ImageResolved == "" {
		w.t.Errorf("the create's machine size and image: %+v %d %q %q", inst.Size, inst.DiskGB, inst.Image, inst.ImageResolved)
	}
	if inst.Lease.ExpiresAt.IsZero() || inst.Lease.ExpiresAt.Before(inst.CreatedAt) || inst.Lease.MaxExpiresAt.Before(inst.Lease.ExpiresAt) {
		w.t.Errorf("lease %+v from created %v", inst.Lease, inst.CreatedAt)
	}
	if inst.Labels[provider.LabelState] != string(provider.ShedStateCreating) || inst.Spec != spec.Spec {
		w.t.Errorf("labels %v, spec kept verbatim %v", inst.Labels, inst.Spec == spec.Spec)
	}
	if len(inst.Access) != 0 {
		w.t.Errorf("a machine being created has access %v", inst.Access)
	}
	got := w.settled(pend)
	if got.State != provider.StateRunning || got.StartedAt.IsZero() {
		w.t.Errorf("after the create: %s, started %v", got.State, got.StartedAt)
	}
	if len(got.Access) == 0 || got.Access[0].Kind != provider.AccessSSH || got.Access[0].HostKey == "" || got.Access[0].Port == 0 {
		w.t.Errorf("a running machine's access = %+v", got.Access)
	}
	if g, err := w.get(pend.Ref); err != nil || g.State != provider.StateRunning {
		w.t.Errorf("Get = %+v, %v", g.State, err)
	}
	if !listed(w.list(), pend.Ref) {
		w.t.Errorf("the machine is not listed")
	}
}

func testCreateIdempotent(w *world) {
	spec := w.personSpec("idem")
	pend, _, err := w.Provider.Create(w.ctx, spec)
	if err != nil {
		w.t.Fatalf("Create: %v", err)
	}
	again := *spec.Clone()
	again.RequestID = provider.NewRequestID()
	p2, inst, err := w.Provider.Create(w.ctx, again)
	if err != nil || p2.Ref != pend.Ref || inst.Ref != pend.Ref {
		w.t.Fatalf("the same create again (another request id) = %+v, %+v, %v; want the same machine", p2, inst.Ref, err)
	}
	w.settled(pend)
	// After the create has finished: the machine, with no operation.
	again.RequestID = provider.NewRequestID()
	p3, inst, err := w.Provider.Create(w.ctx, again)
	if err != nil || p3.Ref != pend.Ref || inst.State != provider.StateRunning {
		w.t.Fatalf("the same create after it finished = %+v, %s, %v", p3, inst.State, err)
	}
	other := *spec.Clone()
	other.RequestID = provider.NewRequestID()
	other.AuthorizedKeys = append(other.AuthorizedKeys, w.Key+" second")
	_, _, err = w.Provider.Create(w.ctx, other)
	wantKind(w.t, "a create of the shed id with another body", err, provider.ErrSpecMismatch)
	wantCode(w.t, "a create of the shed id with another body", err, "SPEC_MISMATCH")
	_, _, err = w.Provider.Create(w.ctx, w.personSpec("idem"))
	wantKind(w.t, "a create of the name for another shed id", err, provider.ErrExists)
	if n := len(w.list().Instances); n != 1 {
		w.t.Errorf("%d machines, want 1", n)
	}
}

func testRequestID(w *world) {
	spec := w.personSpec("rid")
	pend, _, err := w.Provider.Create(w.ctx, spec)
	if err != nil {
		w.t.Fatalf("Create: %v", err)
	}
	again, _, err := w.Provider.Create(w.ctx, spec)
	if err != nil || again != pend {
		w.t.Fatalf("the create's request id again = %+v, %v; want the first answer %+v", again, err, pend)
	}
	w.settled(pend)
	rid := provider.NewRequestID()
	p1, err := w.Provider.Stop(w.ctx, pend.Ref, rid)
	if err != nil {
		w.t.Fatal(err)
	}
	p2, err := w.Provider.Stop(w.ctx, pend.Ref, rid)
	if err != nil || p1 != p2 {
		w.t.Errorf("a stop's request id again = %+v, %v; want %+v", p2, err, p1)
	}
	// An id used for another call is refused.
	_, err = w.Provider.Start(w.ctx, pend.Ref, rid)
	wantKind(w.t, "a request id used for another call", err, provider.ErrInvalid)
}

func testStopStart(w *world) {
	inst := w.running("cycle")
	pend, err := w.Provider.Stop(w.ctx, inst.Ref, provider.NewRequestID())
	if err != nil || pend.Want != provider.StateStopped {
		w.t.Fatalf("Stop = %+v, %v", pend, err)
	}
	if g, _ := w.get(inst.Ref); g.State != provider.StateStopping || len(g.Access) != 0 {
		w.t.Errorf("a stopping machine: %s, access %v", g.State, g.Access)
	}
	got := w.settled(pend)
	if got.State != provider.StateStopped || got.StoppedAt.IsZero() {
		w.t.Errorf("after the stop: %s, stopped %v", got.State, got.StoppedAt)
	}
	if got.Lease.ExpiresAt != inst.Lease.ExpiresAt || got.Lease.MaxExpiresAt != inst.Lease.MaxExpiresAt {
		w.t.Errorf("the stop moved the lease: %+v, was %+v", got.Lease, inst.Lease)
	}
	// Stop on a stopped machine: nothing to follow.
	again, err := w.Provider.Stop(w.ctx, inst.Ref, provider.NewRequestID())
	if err != nil {
		w.t.Fatalf("Stop of a stopped machine: %v", err)
	}
	if g, err := w.wait(w.Provider, again); err != nil || g.State != provider.StateStopped {
		w.t.Errorf("Wait on the repeated stop = %s, %v", g.State, err)
	}
	pend, err = w.Provider.Start(w.ctx, inst.Ref, provider.NewRequestID())
	if err != nil || pend.Want != provider.StateRunning {
		w.t.Fatalf("Start = %+v, %v", pend, err)
	}
	// Start on a starting machine follows the start in progress.
	if p2, err := w.Provider.Start(w.ctx, inst.Ref, provider.NewRequestID()); err != nil || p2.Ref != inst.Ref {
		w.t.Errorf("Start of a starting machine = %+v, %v", p2, err)
	}
	got = w.settled(pend)
	if got.State != provider.StateRunning {
		w.t.Errorf("after the start: %s", got.State)
	}
	if got.Lease.ExpiresAt != inst.Lease.ExpiresAt {
		w.t.Errorf("the start moved the lease")
	}
	if p, err := w.Provider.Start(w.ctx, inst.Ref, provider.NewRequestID()); err != nil {
		w.t.Errorf("Start of a running machine: %v", err)
	} else if g, err := w.wait(w.Provider, p); err != nil || g.State != provider.StateRunning {
		w.t.Errorf("Wait on the start of a running machine = %s, %v", g.State, err)
	}
}

func testBusy(w *world) {
	inst := w.stopped("busy")
	pend, err := w.Provider.Start(w.ctx, inst.Ref, provider.NewRequestID())
	if err != nil {
		w.t.Fatal(err)
	}
	_, err = w.Provider.Stop(w.ctx, inst.Ref, provider.NewRequestID())
	wantKind(w.t, "Stop of a starting machine", err, provider.ErrBusy)
	wantCode(w.t, "Stop of a starting machine", err, "BUSY")
	w.settled(pend)
	pend, err = w.Provider.Stop(w.ctx, inst.Ref, provider.NewRequestID())
	if err != nil {
		w.t.Fatal(err)
	}
	_, err = w.Provider.Start(w.ctx, inst.Ref, provider.NewRequestID())
	wantKind(w.t, "Start of a stopping machine", err, provider.ErrBusy)
	w.settled(pend)
	// Settle on a machine at rest returns at once.
	if g, err := w.wait(w.Provider, provider.Settle(inst.Ref)); err != nil || g.State != provider.StateStopped {
		w.t.Errorf("Wait(Settle) = %s, %v", g.State, err)
	}
}

func testLabels(w *world) {
	inst := w.running("labels")
	set := map[string]*string{provider.LabelState: provider.Str("ready"), provider.LabelLayout: provider.Str("1")}
	got, err := w.Provider.SetLabels(w.ctx, inst.Ref, provider.NewRequestID(), set, map[string]*string{provider.LabelState: provider.Str("creating")})
	if err != nil || got.ShedState() != provider.ShedStateReady || got.Layout() != 1 {
		w.t.Fatalf("SetLabels = %v %v, %v", got.Labels, got.Layout(), err)
	}
	_, err = w.Provider.SetLabels(w.ctx, inst.Ref, provider.NewRequestID(), map[string]*string{"extra": provider.Str("x")}, map[string]*string{provider.LabelState: nil})
	wantKind(w.t, "a write whose expect does not hold", err, provider.ErrLabelTaken)
	wantCode(w.t, "a write whose expect does not hold", err, "LABEL_TAKEN")
	if g, _ := w.get(inst.Ref); g.Labels["extra"] != "" {
		w.t.Errorf("a refused write wrote: %v", g.Labels)
	}
	got, err = w.Provider.SetLabels(w.ctx, inst.Ref, provider.NewRequestID(), map[string]*string{provider.LabelLayout: nil}, nil)
	if _, has := got.Labels[provider.LabelLayout]; err != nil || has {
		w.t.Errorf("a removal = %v, %v", got.Labels, err)
	}
	_, err = w.Provider.SetLabels(w.ctx, inst.Ref, provider.NewRequestID(), map[string]*string{"shed_user": provider.Str("x")}, nil)
	wantKind(w.t, "a write of a reserved label", err, provider.ErrInvalid)
}

func testExtend(w *world) {
	cat, err := w.Provider.Catalog(w.ctx)
	if err != nil {
		w.t.Fatal(err)
	}
	inst := w.running("extend")
	got, err := w.Provider.Extend(w.ctx, inst.Ref, provider.NewRequestID(), 6*time.Hour)
	if err != nil {
		w.t.Fatalf("Extend: %v", err)
	}
	if d := got.Lease.ExpiresAt.Sub(inst.Lease.ExpiresAt); d != 6*time.Hour {
		w.t.Errorf("the extend moved the lease by %s, want 6h", d)
	}
	if got.Lease.MaxExpiresAt != inst.Lease.MaxExpiresAt {
		w.t.Errorf("the extend moved the cap")
	}
	past := cat.Policy.PersonMaxAge.Round(time.Hour) + time.Hour
	_, err = w.Provider.Extend(w.ctx, inst.Ref, provider.NewRequestID(), past)
	wantKind(w.t, "an extend past the cap", err, provider.ErrLease)
	wantCode(w.t, "an extend past the cap", err, "LEASE_AT_CAP")
	if g, _ := w.get(inst.Ref); g.Lease.ExpiresAt != got.Lease.ExpiresAt {
		w.t.Errorf("a refused extend moved the lease")
	}
	_, err = w.Provider.Extend(w.ctx, inst.Ref, provider.NewRequestID(), 90*time.Minute)
	wantKind(w.t, "an extend by a part of an hour", err, provider.ErrInvalid)
}

func testClient(w *world) {
	spec := w.spec("client-1", provider.Owner{Client: w.Client, Lease: 30 * time.Hour})
	pend, inst, err := w.Provider.Create(w.ctx, spec)
	if err != nil {
		w.t.Fatalf("Create for a client: %v", err)
	}
	if inst.Client != w.Client || inst.User != "" {
		w.t.Errorf("owner = %q/%q", inst.User, inst.Client)
	}
	if d := inst.Lease.ExpiresAt.Sub(inst.CreatedAt); d != 30*time.Hour || inst.Lease.MaxExpiresAt != inst.Lease.ExpiresAt {
		w.t.Errorf("a client's lease = %+v from %v", inst.Lease, inst.CreatedAt)
	}
	if inst.Lease.Extendable || len(inst.Lease.PresetsH) != 0 {
		w.t.Errorf("a client's machine is extendable: %+v", inst.Lease)
	}
	w.settled(pend)
	_, err = w.Provider.Extend(w.ctx, pend.Ref, provider.NewRequestID(), time.Hour)
	wantKind(w.t, "an extend of a client's machine", err, provider.ErrLease)
	wantCode(w.t, "an extend of a client's machine", err, "EXTEND_NOT_ALLOWED")
	long := w.spec("client-2", provider.Owner{Client: w.Client, Lease: 400 * 24 * time.Hour})
	_, _, err = w.Provider.Create(w.ctx, long)
	wantKind(w.t, "a client's lease over the policy", err, provider.ErrPolicy)
	wantCode(w.t, "a client's lease over the policy", err, "LEASE_TOO_LONG")
}

func testDestroy(w *world) {
	inst := w.running("gone")
	pend, err := w.Provider.Destroy(w.ctx, inst.Ref, provider.NewRequestID())
	if err != nil || pend.Want != provider.StateAbsent {
		w.t.Fatalf("Destroy = %+v, %v", pend, err)
	}
	if g, err := w.get(inst.Ref); err == nil && g.State != provider.StateDeleting {
		w.t.Errorf("a machine being destroyed is %s", g.State)
	}
	w.Settle(pend.Ref)
	if _, err := w.wait(w.Provider, pend); err != nil {
		w.t.Fatalf("Wait(destroy) = %v", err)
	}
	_, err = w.get(inst.Ref)
	wantKind(w.t, "Get after the destroy", err, provider.ErrNotFound)
	l := w.list()
	if listed(l, inst.Ref) {
		w.t.Errorf("a destroyed machine is listed")
	}
	if d, ok := deletionOf(l, inst.Ref); !ok || d.Cause != provider.CauseServer {
		w.t.Errorf("the deletion = %+v (%v), want cause server", d, ok)
	}
	// Again, on nothing: success, and the wait returns at once.
	again, err := w.Provider.Destroy(w.ctx, inst.Ref, provider.NewRequestID())
	if err != nil {
		w.t.Fatalf("Destroy of a gone machine = %v", err)
	}
	if _, err := w.wait(w.Provider, again); err != nil {
		w.t.Errorf("Wait on the destroy of a gone machine = %v", err)
	}
}

func testDestroyEndsWork(w *world) {
	pend, _, err := w.Provider.Create(w.ctx, w.personSpec("cut"))
	if err != nil {
		w.t.Fatal(err)
	}
	del, err := w.Provider.Destroy(w.ctx, pend.Ref, provider.NewRequestID())
	if err != nil {
		w.t.Fatalf("Destroy of a machine being created: %v", err)
	}
	_, err = w.wait(w.Provider, pend)
	wantKind(w.t, "the create a destroy ended", err, provider.ErrDeleting)
	w.Settle(del.Ref)
	if _, err := w.wait(w.Provider, del); err != nil {
		w.t.Errorf("Wait(destroy) = %v", err)
	}
}

func testDeletedOutside(w *world) {
	if w.DeleteOutside == nil {
		w.t.Skip("the world cannot delete outside the server")
	}
	a := w.running("page-del")
	b := w.stopped("ext-del")
	w.DeleteOutside(a.Ref, provider.CausePage)
	w.DeleteOutside(b.Ref, provider.CauseExternal)
	l := w.list()
	for ref, cause := range map[provider.Ref]string{a.Ref: provider.CausePage, b.Ref: provider.CauseExternal} {
		if listed(l, ref) {
			w.t.Errorf("%s is listed after its delete", ref)
		}
		if d, ok := deletionOf(l, ref); !ok || d.Cause != cause {
			w.t.Errorf("deletion of %s = %+v (%v), want cause %s", ref, d, ok, cause)
		}
		_, err := w.get(ref)
		wantKind(w.t, "Get of a machine deleted outside", err, provider.ErrNotFound)
	}
	_, err := w.Provider.Start(w.ctx, b.Ref, provider.NewRequestID())
	wantKind(w.t, "Start of a deleted machine", err, provider.ErrNotFound)
}

func testLeaseEnds(w *world) {
	if w.Advance == nil {
		w.t.Skip("the world has no clock")
	}
	cat, err := w.Provider.Catalog(w.ctx)
	if err != nil {
		w.t.Fatal(err)
	}
	run := w.running("lease-run")
	stop := w.stopped("lease-stop")
	// Past the lease's end the service deletes the machine, running or
	// stopped, and lists why.
	w.Advance(cat.Policy.PersonInitialLease + time.Second)
	l := w.list()
	for _, ref := range []provider.Ref{run.Ref, stop.Ref} {
		if listed(l, ref) {
			w.t.Errorf("%s outlived its lease", ref)
		}
		if d, ok := deletionOf(l, ref); !ok || d.Cause != provider.CauseLeaseEnded {
			w.t.Errorf("deletion of %s = %+v (%v), want lease_ended", ref, d, ok)
		}
	}
}

func testLateRefusal(w *world) {
	if w.FailNextCreate == nil {
		w.t.Skip("the world cannot fail a create")
	}
	w.FailNextCreate()
	pend, _, err := w.Provider.Create(w.ctx, w.personSpec("late"))
	if err != nil {
		w.t.Fatalf("Create: %v (the refusal must arrive with the operation)", err)
	}
	w.Settle(pend.Ref)
	_, err = w.wait(w.Provider, pend)
	wantKind(w.t, "Wait on a create the cloud refused", err, provider.ErrQuota)
	if !provider.IsRefusal(err) {
		w.t.Errorf("a cloud quota is not a refusal: %v", err)
	}
	_, err = w.get(pend.Ref)
	wantKind(w.t, "Get after a refused create", err, provider.ErrNotFound)
	if _, ok := deletionOf(w.list(), pend.Ref); ok {
		w.t.Errorf("a refused create is listed as a deletion")
	}
}

func testUnknown(w *world) {
	ref := provider.Ref(newShedID())
	_, err := w.get(ref)
	wantKind(w.t, "Get of an unknown machine", err, provider.ErrNotFound)
	_, err = w.Provider.Start(w.ctx, ref, provider.NewRequestID())
	wantKind(w.t, "Start of an unknown machine", err, provider.ErrNotFound)
	_, err = w.Provider.Stop(w.ctx, ref, provider.NewRequestID())
	wantKind(w.t, "Stop of an unknown machine", err, provider.ErrNotFound)
	_, err = w.Provider.Extend(w.ctx, ref, provider.NewRequestID(), time.Hour)
	wantKind(w.t, "Extend of an unknown machine", err, provider.ErrNotFound)
	p, err := w.Provider.Destroy(w.ctx, ref, provider.NewRequestID())
	if err != nil {
		w.t.Fatalf("Destroy of an unknown machine = %v", err)
	}
	if _, err := w.wait(w.Provider, p); err != nil {
		w.t.Errorf("Wait on it = %v", err)
	}
	_, err = w.wait(w.Provider, provider.Pending{Ref: ref, Want: provider.StateRunning})
	wantKind(w.t, "Wait for an unknown machine to run", err, provider.ErrNotFound)
}

func testUsage(w *world) {
	w.running("use-1")
	w.stopped("use-2")
	u, err := w.Provider.Usage(w.ctx, w.User)
	if err != nil {
		w.t.Fatalf("Usage: %v", err)
	}
	if u.Person == nil || u.Person.Machines != 2 || u.Person.Running != 1 || u.Person.Stopped != 1 || u.Person.Max <= 0 {
		w.t.Errorf("person usage = %+v", u.Person)
	}
	if u.Org.Machines < 2 || u.Org.Max <= 0 {
		w.t.Errorf("organization usage = %+v", u.Org)
	}
	if u, err := w.Provider.Usage(w.ctx, ""); err != nil || u.Person != nil {
		w.t.Errorf("Usage with no user = %+v, %v", u, err)
	}
}

func testIngress(w *world) {
	ing, err := w.Provider.Ingress(w.ctx)
	if err != nil {
		w.t.Fatalf("Ingress: %v", err)
	}
	_ = ing
	want := []netip.Prefix{netip.MustParsePrefix("203.0.113.0/24")}
	got, err := w.Provider.SetIngress(w.ctx, provider.NewRequestID(), want)
	if err != nil {
		w.t.Fatalf("SetIngress: %v", err)
	}
	if !slices.Equal(got.Sources, want) || got.AppliedAt.IsZero() {
		w.t.Errorf("SetIngress = %+v", got)
	}
	if read, err := w.Provider.Ingress(w.ctx); err != nil || !slices.Equal(read.Sources, want) || !read.Exclusive {
		w.t.Errorf("Ingress after the write = %+v, %v", read, err)
	}
	_, err = w.Provider.SetIngress(w.ctx, provider.NewRequestID(), []netip.Prefix{netip.MustParsePrefix("198.51.0.0/16")})
	wantKind(w.t, "a source wider than allowed", err, provider.ErrIngress)
	wantCode(w.t, "a source wider than allowed", err, "INGRESS_TOO_WIDE")
}

func testLostReplies(w *world) {
	if w.LoseNextReply == nil {
		w.t.Skip("the world cannot lose a reply")
	}
	p := w.Provider
	parent := w
	w.t.Run("create", func(t *testing.T) {
		w := parent.sub(t)
		spec := w.personSpec("lost-create")
		w.LoseNextReply("create")
		_, _, err := p.Create(w.ctx, spec)
		wantKind(t, "a create whose reply is lost", err, provider.ErrUnavailable)
		pend, inst, err := p.Create(w.ctx, spec)
		if err != nil || inst.Ref != provider.Ref(spec.ShedID) {
			t.Fatalf("the create sent again = %+v, %v", pend, err)
		}
		w.settled(pend)
		n := 0
		for _, i := range w.list().Instances {
			if i.Name == "lost-create" {
				n++
			}
		}
		if n != 1 {
			t.Errorf("%d machines after a create sent twice, want 1", n)
		}
	})
	w.t.Run("stop and start", func(t *testing.T) {
		w := parent.sub(t)
		inst := w.running("lost-cycle")
		for _, c := range []struct {
			call  string
			do    func(rid string) (provider.Pending, error)
			state provider.State
		}{
			{"stop", func(rid string) (provider.Pending, error) { return p.Stop(w.ctx, inst.Ref, rid) }, provider.StateStopped},
			{"start", func(rid string) (provider.Pending, error) { return p.Start(w.ctx, inst.Ref, rid) }, provider.StateRunning},
		} {
			rid := provider.NewRequestID()
			w.LoseNextReply(c.call)
			_, err := c.do(rid)
			wantKind(t, "a "+c.call+" whose reply is lost", err, provider.ErrUnavailable)
			pend, err := c.do(rid)
			if err != nil {
				t.Fatalf("the %s sent again = %v", c.call, err)
			}
			if g := w.settled(pend); g.State != c.state {
				t.Errorf("after the %s sent again: %s", c.call, g.State)
			}
		}
	})
	w.t.Run("labels", func(t *testing.T) {
		w := parent.sub(t)
		inst := w.running("lost-labels")
		rid := provider.NewRequestID()
		set := map[string]*string{"probe": provider.Str("one")}
		expect := map[string]*string{"probe": nil}
		w.LoseNextReply("labels")
		_, err := p.SetLabels(w.ctx, inst.Ref, rid, set, expect)
		wantKind(t, "a labels write whose reply is lost", err, provider.ErrUnavailable)
		// The condition no longer holds; the kept answer is given all the
		// same: the write took effect once.
		got, err := p.SetLabels(w.ctx, inst.Ref, rid, set, expect)
		if err != nil || got.Labels["probe"] != "one" {
			t.Fatalf("the labels write sent again = %v, %v", got.Labels, err)
		}
	})
	w.t.Run("extend", func(t *testing.T) {
		w := parent.sub(t)
		inst := w.running("lost-extend")
		rid := provider.NewRequestID()
		w.LoseNextReply("extend")
		_, err := p.Extend(w.ctx, inst.Ref, rid, 6*time.Hour)
		wantKind(t, "an extend whose reply is lost", err, provider.ErrUnavailable)
		got, err := p.Extend(w.ctx, inst.Ref, rid, 6*time.Hour)
		if err != nil {
			t.Fatalf("the extend sent again = %v", err)
		}
		if d := got.Lease.ExpiresAt.Sub(inst.Lease.ExpiresAt); d != 6*time.Hour {
			t.Errorf("an extend sent twice moved the lease by %s, want 6h once", d)
		}
	})
	w.t.Run("delete", func(t *testing.T) {
		w := parent.sub(t)
		inst := w.running("lost-delete")
		rid := provider.NewRequestID()
		w.LoseNextReply("delete")
		_, err := p.Destroy(w.ctx, inst.Ref, rid)
		wantKind(t, "a delete whose reply is lost", err, provider.ErrUnavailable)
		pend, err := p.Destroy(w.ctx, inst.Ref, rid)
		if err != nil {
			t.Fatalf("the delete sent again = %v", err)
		}
		w.Settle(pend.Ref)
		if _, err := w.wait(p, pend); err != nil {
			t.Errorf("Wait on the delete sent again = %v", err)
		}
		_, err = w.get(inst.Ref)
		wantKind(t, "Get after the delete", err, provider.ErrNotFound)
	})
}

func testExpiredOperations(w *world) {
	if w.ExpireOperations == nil {
		w.t.Skip("the world cannot expire an operation")
	}
	p := w.Provider
	// Each operation finishes, is forgotten, and its stored Pending is
	// waited on: the machine is followed by what the Pending waits for.
	create, _, err := p.Create(w.ctx, w.personSpec("expire"))
	if err != nil {
		w.t.Fatal(err)
	}
	ref := create.Ref
	steps := []struct {
		name string
		do   func() provider.Pending
		want provider.State
	}{
		{"create", func() provider.Pending { return create }, provider.StateRunning},
		{"stop", func() provider.Pending {
			pend, err := p.Stop(w.ctx, ref, provider.NewRequestID())
			if err != nil {
				w.t.Fatal(err)
			}
			return pend
		}, provider.StateStopped},
		{"start", func() provider.Pending {
			pend, err := p.Start(w.ctx, ref, provider.NewRequestID())
			if err != nil {
				w.t.Fatal(err)
			}
			return pend
		}, provider.StateRunning},
	}
	for _, s := range steps {
		pend := s.do()
		if pend.Operation == "" {
			w.t.Fatalf("%s gave no operation", s.name)
		}
		w.Settle(ref)
		w.ExpireOperations()
		got, err := w.wait(p, pend)
		if err != nil || got.State != s.want {
			w.t.Errorf("Wait on an expired %s = %s, %v; want %s", s.name, got.State, err, s.want)
		}
	}
	del, err := p.Destroy(w.ctx, ref, provider.NewRequestID())
	if err != nil {
		w.t.Fatal(err)
	}
	w.Settle(ref)
	w.ExpireOperations()
	if _, err := w.wait(p, del); err != nil {
		w.t.Errorf("Wait on an expired delete = %v", err)
	}
	// A Pending stored with its operation unknown, for a machine still
	// running, is waited on by its Want.
	inst := w.running("expire-2")
	w.ExpireOperations()
	got, err := w.wait(p, provider.Pending{Ref: inst.Ref, Operation: "an-operation-never-made", Want: provider.StateRunning})
	if err != nil || got.State != provider.StateRunning {
		w.t.Errorf("Wait on an unknown operation = %s, %v", got.State, err)
	}
	_, err = w.wait(p, provider.Pending{Ref: inst.Ref, Operation: "an-operation-never-made", Want: provider.StateStopped})
	wantKind(w.t, "Wait for stopped on a running machine", err, provider.ErrUnexpectedState)
}

func testFreshProcess(w *world) {
	if w.Fresh == nil {
		w.t.Skip("the world has one process")
	}
	// Each Pending is stored as JSON by one process and waited on by
	// another, which knows nothing of it but what was stored.
	store := func(p provider.Pending) []byte {
		b, err := json.Marshal(p)
		if err != nil {
			w.t.Fatal(err)
		}
		return b
	}
	load := func(b []byte) provider.Pending {
		var p provider.Pending
		if err := json.Unmarshal(b, &p); err != nil {
			w.t.Fatal(err)
		}
		return p
	}
	spec := w.personSpec("fresh")
	pend, _, err := w.Provider.Create(w.ctx, spec)
	if err != nil {
		w.t.Fatal(err)
	}
	stored := store(pend)
	fresh := w.Fresh()
	loaded := load(stored)
	if loaded != pend || loaded.Want != provider.StateRunning {
		w.t.Fatalf("the stored Pending reads back as %+v, want %+v", loaded, pend)
	}
	w.Settle(loaded.Ref)
	got, err := w.wait(fresh, loaded)
	if err != nil || got.State != provider.StateRunning {
		w.t.Fatalf("a fresh process waiting on the stored create = %s, %v", got.State, err)
	}
	// The create replayed verbatim by the fresh process: the same machine.
	if p2, _, err := fresh.Create(w.ctx, spec); err != nil || p2.Ref != pend.Ref {
		w.t.Errorf("the stored create replayed by a fresh process = %+v, %v", p2, err)
	}
	stop, err := w.Provider.Stop(w.ctx, pend.Ref, provider.NewRequestID())
	if err != nil {
		w.t.Fatal(err)
	}
	stored = store(stop)
	fresh = w.Fresh()
	w.Settle(pend.Ref)
	if got, err := w.wait(fresh, load(stored)); err != nil || got.State != provider.StateStopped {
		w.t.Errorf("a fresh process waiting on the stored stop = %s, %v", got.State, err)
	}
	del, err := w.Provider.Destroy(w.ctx, pend.Ref, provider.NewRequestID())
	if err != nil {
		w.t.Fatal(err)
	}
	stored = store(del)
	fresh = w.Fresh()
	w.Settle(pend.Ref)
	if _, err := w.wait(fresh, load(stored)); err != nil {
		w.t.Errorf("a fresh process waiting on the stored delete = %v", err)
	}
	_, err = fresh.Get(w.ctx, pend.Ref)
	wantKind(w.t, "Get after the delete", err, provider.ErrNotFound)
}
