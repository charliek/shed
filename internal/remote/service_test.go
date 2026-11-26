package remote

import (
	"context"
	"errors"
	"net/http"
	"net/netip"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/charliek/shed/internal/config"
	"github.com/charliek/shed/internal/delivery"
	"github.com/charliek/shed/internal/delivery/deliverytest"
	"github.com/charliek/shed/internal/directory/refserver"
	"github.com/charliek/shed/internal/remote/provider"
	"github.com/charliek/shed/internal/remote/provider/fake"
	"github.com/charliek/shed/internal/remote/provider/service/servicetest"
	"github.com/charliek/shed/internal/remote/state"
)

// The remote backend on the compute service's client and the reference
// compute service, in process over loopback (plan 041 criterion 3). The
// agent is the fake links' in-process one: only the service is real.

// The world's people (servicetest's): ada (an admin), linus and turing.
// They are the account service's; anyone else is a local admin.
func directoryOrigin(user string) string {
	switch user {
	case "ada", "linus", "turing":
		return delivery.OriginDirectory
	}
	return delivery.OriginLocal
}

// serviceEnv builds a backend whose provider is the compute service's client
// against a reference service of its own. Every backend opened (a restart)
// gets a client of its own: a new server process.
func serviceEnv(t *testing.T, opts servicetest.Options, more ...envOpt) (*env, *servicetest.World) {
	t.Helper()
	return serviceEnvWith(t, opts, nil, more...)
}

// serviceEnvWith is serviceEnv with each client handed to decorate first.
func serviceEnvWith(t *testing.T, opts servicetest.Options, decorate func(provider.Provider) provider.Provider, more ...envOpt) (*env, *servicetest.World) {
	t.Helper()
	if opts.AutoSettle == 0 {
		opts.AutoSettle = 20 * time.Millisecond
	}
	w := servicetest.Start(t, opts)
	w.Step("policy.set", map[string]any{"person_max": 100, "org_max": 100})
	// The ingress the static mode of the test backend expects.
	if _, err := w.Client().SetIngress(context.Background(), "", []netip.Prefix{netip.MustParsePrefix(fake.DefaultIngressSource)}); err != nil {
		t.Fatalf("SetIngress: %v", err)
	}
	return serviceEnvOn(t, w, decorate, more...), w
}

// serviceEnvOn builds the backend of serviceEnvWith against a world the
// caller set up (its ingress included).
func serviceEnvOn(t *testing.T, w *servicetest.World, decorate func(provider.Provider) provider.Provider, more ...envOpt) *env {
	t.Helper()
	svc := delivery.NewService(deliverytest.NewSource(), 5*time.Second)
	svc.SetOrigin(directoryOrigin)
	base := func(e *env, _ *fake.Options) {
		e.cfg.AllowTestDrivers = true
		e.cfg.DiskGB = 0
		e.wrap = func(provider.Provider) provider.Provider {
			c := provider.Provider(w.Client())
			if decorate != nil {
				c = decorate(c)
			}
			return c
		}
		prev := e.seams
		e.seams = func(b *Backend) {
			if prev != nil {
				prev(b)
			}
			b.SetDelivery(svc)
		}
	}
	return newEnv(t, append([]envOpt{base}, more...)...)
}

// machineOf reads a shed's machine at the service.
func machineOf(t *testing.T, w *servicetest.World, e *env, name string) (provider.Instance, error) {
	t.Helper()
	shed, err := e.b.GetShed(context.Background(), name)
	if err != nil {
		t.Fatalf("GetShed(%s): %v", name, err)
	}
	return w.Client().Get(context.Background(), provider.Ref(shed.ID))
}

func computeRefusal(t *testing.T, err error, status int, code, reason string) {
	t.Helper()
	var cr *config.ComputeRefusal
	if !errors.As(err, &cr) || cr.Status != status || cr.Code != code || cr.Reason != reason {
		t.Fatalf("error %v, want %d %s (reason %q)", err, status, code, reason)
	}
}

// Create, stop, start and delete reach the right record states, and the
// machine's facts are on the first starting record (filled from the
// create's answer).
func TestServiceLifecycle(t *testing.T) {
	e, w := serviceEnv(t, servicetest.Options{})
	ctx := ctxT(t)
	shed, err := e.b.CreateShed(ctx, config.CreateShedRequest{Name: "widgets", NoProvision: true, User: "ada"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if shed.Status != config.StatusStarting {
		t.Fatalf("create answered %q, want starting", shed.Status)
	}
	rec := e.storedRecord("widgets")
	if rec.Machine == nil || rec.Machine.Ref != provider.Ref(shed.ID) || rec.Machine.Size.Name != "standard" ||
		rec.Machine.Lease.ExpiresAt.IsZero() || rec.Machine.ImageResolved == "" || rec.Machine.User != "ada" {
		t.Fatalf("the first starting record's machine = %+v, want the create's answer", rec.Machine)
	}
	if !rec.Shed.CreatedAt.Equal(shed.CreatedAt) || rec.Spec == nil || rec.Spec.RequestID == "" {
		t.Fatalf("record created_at %v (answer %v), spec %+v", rec.Shed.CreatedAt, shed.CreatedAt, rec.Spec)
	}
	if got := e.settled("widgets"); got.Status != config.StatusRunning || got.CPUs != 2 || got.MemoryMB != 8192 {
		t.Fatalf("settled %q (%q) %d/%d, want running on the standard size", got.Status, got.StatusReason, got.CPUs, got.MemoryMB)
	}
	if m, err := machineOf(t, w, e, "widgets"); err != nil || m.State != provider.StateRunning || m.ShedState() != provider.ShedStateReady {
		t.Fatalf("the machine: %s %q, %v", m.State, m.ShedState(), err)
	}

	if _, err := e.b.StopShed(ctx, "widgets"); err != nil {
		t.Fatal(err)
	}
	if got := e.settled("widgets"); got.Status != config.StatusStopped {
		t.Fatalf("after the stop: %q (%q)", got.Status, got.StatusReason)
	}
	m, _ := machineOf(t, w, e, "widgets")
	if m.State != provider.StateStopped {
		t.Fatalf("the machine after the stop: %s", m.State)
	}
	lease := m.Lease
	if _, err := e.b.StartShed(ctx, "widgets"); err != nil {
		t.Fatal(err)
	}
	if got := e.settled("widgets"); got.Status != config.StatusRunning {
		t.Fatalf("after the start: %q (%q)", got.Status, got.StatusReason)
	}
	if m, _ := machineOf(t, w, e, "widgets"); m.Lease.ExpiresAt != lease.ExpiresAt || m.Lease.MaxExpiresAt != lease.MaxExpiresAt {
		t.Errorf("stop and start moved the lease: %+v, was %+v", m.Lease, lease)
	}

	if err := e.b.DeleteShed(ctx, "widgets"); err != nil {
		t.Fatal(err)
	}
	e.idle()
	if _, err := w.Client().Get(ctx, provider.Ref(shed.ID)); !errors.Is(err, provider.ErrNotFound) {
		t.Fatalf("the machine after the delete: %v, want gone", err)
	}
	if n := markerCount(t, e.dir); n != 0 {
		t.Fatalf("%d delete markers left", n)
	}
}

// A lease that ends deletes the machine at the service, running or
// stopped; the next pass drops each record once an exact read confirms it,
// with a line naming the cause. No server timer deletes anything.
func TestServiceLeaseEnds(t *testing.T) {
	e, w := serviceEnv(t, servicetest.Options{})
	ctx := ctxT(t)
	for _, name := range []string{"runs", "rests"} {
		if _, err := e.b.CreateShed(ctx, config.CreateShedRequest{Name: name, NoProvision: true, User: "linus"}); err != nil {
			t.Fatal(err)
		}
		e.settled(name)
	}
	if _, err := e.b.StopShed(ctx, "rests"); err != nil {
		t.Fatal(err)
	}
	e.settled("rests")
	// Nothing ends before the service says so: passes with the lease
	// running drop nothing.
	e.sweep()
	if sheds, _ := e.b.ListSheds(ctx); len(sheds) != 2 {
		t.Fatalf("%d sheds before the lease's end", len(sheds))
	}
	w.Step("clock.advance", 86400+1)
	e.sweep()
	e.idle()
	if sheds, _ := e.b.ListSheds(ctx); len(sheds) != 0 {
		t.Fatalf("%d sheds after their leases ended, want none", len(sheds))
	}
	log := strings.Join(e.logLines(), "\n")
	for _, name := range []string{"runs", "rests"} {
		if !strings.Contains(log, "shed "+name+" (") || !strings.Contains(log, "deleted its machine (cause lease_ended)") {
			t.Errorf("no line names the lease's end of %s:\n%s", name, log)
		}
	}
}

// listFilter hides machines from the list, as a list that omits them would.
type listFilter struct {
	provider.Provider
	hide func(ref provider.Ref) bool
	fail error
}

func (l *listFilter) List(ctx context.Context) (provider.Listing, error) {
	if l.fail != nil {
		return provider.Listing{}, l.fail
	}
	got, err := l.Provider.List(ctx)
	if err != nil {
		return got, err
	}
	var kept []provider.Instance
	for _, i := range got.Instances {
		if l.hide == nil || !l.hide(i.Ref) {
			kept = append(kept, i)
		}
	}
	got.Instances = kept
	return got, nil
}

// A list that omits a machine drops its record only once an exact read
// answers 404; a list that fails, or that the server refuses (incomplete),
// drops nothing.
func TestServiceListOmissionsAndFailures(t *testing.T) {
	filter := &listFilter{}
	e, w := serviceEnvWith(t, servicetest.Options{}, func(p provider.Provider) provider.Provider {
		filter.Provider = p
		return filter
	})
	ctx := ctxT(t)
	if _, err := e.b.CreateShed(ctx, config.CreateShedRequest{Name: "hidden", NoProvision: true, User: "ada"}); err != nil {
		t.Fatal(err)
	}
	shed := e.settled("hidden")
	ref := provider.Ref(shed.ID)

	filter.hide = func(r provider.Ref) bool { return r == ref }
	e.sweep()
	if got := e.status("hidden"); got != config.StatusRunning {
		t.Fatalf("an omitted machine that a read still finds: shed %q, want running", got)
	}

	filter.hide = nil
	filter.fail = &provider.Error{Kind: provider.ErrUnavailable, Op: "list"}
	if err := e.b.Sweep(ctx); err == nil {
		t.Fatal("a pass whose list failed reported success")
	}
	filter.fail = nil
	w.Ref.ComputeForceReply("list", &refserver.ComputeReply{Status: http.StatusOK, Body: []byte(`{"machines":[],"deleted":[],"complete":false}`)})
	if err := e.b.Sweep(ctx); !errors.Is(err, provider.ErrInvalidReply) {
		t.Fatalf("a pass over an incomplete list = %v, want an invalid reply", err)
	}
	w.Ref.ComputeForceReply("list", nil)
	if got := e.status("hidden"); got != config.StatusRunning {
		t.Fatalf("after failed and incomplete lists: shed %q, want running", got)
	}

	// Gone at the service and omitted: dropped.
	filter.hide = func(r provider.Ref) bool { return r == ref }
	w.Step("machine.delete", []string{w.Server, shed.ID, "page"})
	e.sweep()
	if _, err := e.b.GetShed(ctx, "hidden"); !errors.Is(err, config.ErrShedNotFoundSentinel) {
		t.Fatalf("GetShed after the machine is gone = %v, want not found", err)
	}
}

// A local admin manages cloud sheds and creates none: 422
// COMPUTE_POLICY_REFUSED with reason person, before anything is sent.
func TestServiceLocalAdminCreateRefused(t *testing.T) {
	e, w := serviceEnv(t, servicetest.Options{})
	for _, user := range []string{"ops", ""} {
		_, err := e.b.CreateShed(ctxT(t), config.CreateShedRequest{Name: "mine", NoProvision: true, User: user})
		computeRefusal(t, err, http.StatusUnprocessableEntity, config.ErrComputePolicyRefused, "person")
	}
	if l, _ := w.Client().List(context.Background()); len(l.Instances) != 0 {
		t.Fatalf("%d machines made for a local admin", len(l.Instances))
	}
	if sheds, _ := e.b.ListSheds(context.Background()); len(sheds) != 0 {
		t.Fatalf("%d sheds recorded", len(sheds))
	}
}

// catalogWithout hands out the service's catalog with OwnerRequired cleared,
// as a provider that never set it would.
// It counts the creates sent.
type catalogWithout struct {
	provider.Provider
	creates *atomic.Int32
}

func (c catalogWithout) Create(ctx context.Context, spec provider.Spec) (provider.Pending, provider.Instance, error) {
	c.creates.Add(1)
	return c.Provider.Create(ctx, spec)
}

func (c catalogWithout) Catalog(ctx context.Context) (provider.Catalog, error) {
	cat, err := c.Provider.Catalog(ctx)
	cat.OwnerRequired = false
	return cat, err
}

// On remote.provider: service the local admin's refusal does not hang on
// the catalog: with OwnerRequired cleared it is still 422 reason person,
// and nothing reaches the service.
func TestServiceLocalAdminRefusedWhateverTheCatalog(t *testing.T) {
	var creates atomic.Int32
	e, _ := serviceEnvWith(t, servicetest.Options{}, func(p provider.Provider) provider.Provider { return catalogWithout{p, &creates} },
		func(e *env, _ *fake.Options) { e.cfg.Provider = config.RemoteProviderService })
	if cat, ok := e.b.Catalog(); !ok || cat.OwnerRequired {
		t.Fatalf("the catalog = %+v (read %v), want one without OwnerRequired", cat, ok)
	}
	_, err := e.b.CreateShed(ctxT(t), config.CreateShedRequest{Name: "mine", NoProvision: true, User: "ops"})
	computeRefusal(t, err, http.StatusUnprocessableEntity, config.ErrComputePolicyRefused, "person")
	if n := creates.Load(); n != 0 {
		t.Fatalf("%d creates sent for a local admin, want none", n)
	}
	// A person of the service still creates.
	if _, err := e.b.CreateShed(ctxT(t), config.CreateShedRequest{Name: "hers", NoProvision: true, User: "ada"}); err != nil {
		t.Fatalf("ada's create: %v", err)
	}
	e.settled("hers")
}

// The service provider's catalog always says a machine needs an owner.
func TestServiceCatalogRequiresAnOwner(t *testing.T) {
	w := servicetest.Start(t, servicetest.Options{})
	cat, err := w.Client().Catalog(context.Background())
	if err != nil || !cat.OwnerRequired {
		t.Fatalf("catalog OwnerRequired = %v, %v; want true", cat.OwnerRequired, err)
	}
}

// A client's machine carries the client's lease (policy.max_age), and the
// service's own refusals reach the caller by their code: a person the
// service does not list, a size the policy does not allow.
func TestServiceCreateOwnersAndRefusals(t *testing.T) {
	e, w := serviceEnv(t, servicetest.Options{Clients: []string{"nocturn"}}, tuned(func(o *Options) {
		o.Server = &config.ServerConfig{Auth: &config.AuthConfig{Clients: map[string]config.ServiceClientConfig{
			"nocturn": {Policy: config.ServiceClientPolicy{MaxAge: config.Duration(30 * time.Hour)}},
		}}}
	}))
	ctx := ctxT(t)
	if _, err := e.b.CreateShed(ctx, config.CreateShedRequest{Name: "nocturn-1", NoProvision: true, Owner: "nocturn"}); err != nil {
		t.Fatalf("a client's create: %v", err)
	}
	e.settled("nocturn-1")
	m, err := machineOf(t, w, e, "nocturn-1")
	if err != nil || m.Client != "nocturn" || m.Lease.ExpiresAt.Sub(m.CreatedAt) != 30*time.Hour || m.Lease.Extendable {
		t.Fatalf("the client's machine: %+v, %v", m, err)
	}
	// linus may have only the standard size.
	_, err = e.b.CreateShed(ctx, config.CreateShedRequest{Name: "big", NoProvision: true, User: "linus", CPUs: 8})
	computeRefusal(t, err, http.StatusUnprocessableEntity, config.ErrComputePolicyRefused, "size")
	// A person the service's list does not hold.
	w.SetPeople([]refserver.User{{Name: "ada", Admin: true, Keys: []refserver.Key{{Key: servicetest.NewKey(t), Label: "laptop"}}}})
	_, err = e.b.CreateShed(ctx, config.CreateShedRequest{Name: "gone", NoProvision: true, User: "turing"})
	computeRefusal(t, err, http.StatusUnprocessableEntity, config.ErrComputePolicyRefused, "person")
	// The person limit.
	w.Step("quota.fill", []string{"person", "ada"})
	_, err = e.b.CreateShed(ctx, config.CreateShedRequest{Name: "one-more", NoProvision: true, User: "ada"})
	computeRefusal(t, err, http.StatusServiceUnavailable, config.ErrComputeLimitReached, "person")
	for _, name := range []string{"big", "gone", "one-more"} {
		if _, err := e.b.GetShed(ctx, name); !errors.Is(err, config.ErrShedNotFoundSentinel) {
			t.Errorf("a refused create %s left a record: %v", name, err)
		}
	}
}

// The firewall check reads the service's ingress: sources that differ from
// the mode's refuse creates.
func TestServiceFirewallIngress(t *testing.T) {
	ingressOf := func(t *testing.T, w *servicetest.World) string {
		t.Helper()
		ing, err := w.Client().Ingress(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		return joinPrefixes(ing.Sources)
	}

	// Static mode writes its configured sources when the service's differ
	// (plan 041 R7): against a fresh service, with no ingress yet, the
	// start-up check writes them and creates pass; sources changed at the
	// service are written back, at most once every patch interval.
	t.Run("static mode writes its sources", func(t *testing.T) {
		clock := newTestClock()
		w := servicetest.Start(t, servicetest.Options{AutoSettle: 20 * time.Millisecond})
		w.Step("policy.set", map[string]any{"person_max": 100, "org_max": 100})
		if got := ingressOf(t, w); got != "" {
			t.Fatalf("a fresh service's ingress: [%s]", got)
		}
		e := serviceEnvOn(t, w, nil, withClock(clock))
		if got := ingressOf(t, w); got != fake.DefaultIngressSource {
			t.Fatalf("after start-up the ingress is [%s], want the configured %s", got, fake.DefaultIngressSource)
		}
		if _, err := e.b.CreateShed(ctxT(t), config.CreateShedRequest{Name: "first", NoProvision: true, User: "ada"}); err != nil {
			t.Fatalf("a create once the sources are written: %v", err)
		}
		e.settled("first")
		if _, err := w.Client().SetIngress(context.Background(), "", []netip.Prefix{netip.MustParsePrefix("198.51.100.9/32")}); err != nil {
			t.Fatal(err)
		}
		clock.Advance(FirewallPatchInterval + time.Second)
		if _, err := e.b.CreateShed(ctxT(t), config.CreateShedRequest{Name: "second", NoProvision: true, User: "ada"}); err != nil {
			t.Fatalf("a create after the sources were changed at the service: %v", err)
		}
		if got := ingressOf(t, w); got != fake.DefaultIngressSource {
			t.Fatalf("the ingress was not written back: [%s]", got)
		}
		if !strings.Contains(strings.Join(e.logLines(), "\n"), "remote: firewall: wrote the ingress") {
			t.Fatal("no line says the ingress was written")
		}
	})

	// Against an ingress managed elsewhere static mode compares only: the
	// server starts, nothing is written, and a mismatch refuses creates.
	t.Run("static mode against an ingress it may not write", func(t *testing.T) {
		clock := newTestClock()
		w := servicetest.Start(t, servicetest.Options{AutoSettle: 20 * time.Millisecond})
		w.Step("policy.set", map[string]any{"person_max": 100, "org_max": 100})
		if _, err := w.Client().SetIngress(context.Background(), "", []netip.Prefix{netip.MustParsePrefix("198.51.100.9/32")}); err != nil {
			t.Fatal(err)
		}
		w.Step("policy.set", map[string]any{"ingress_writable": false})
		e := serviceEnvOn(t, w, nil, withClock(clock))
		clock.Advance(FirewallPatchInterval + time.Second)
		_, err := e.b.CreateShed(ctxT(t), config.CreateShedRequest{Name: "refused", NoProvision: true, User: "ada"})
		if !errors.Is(err, config.ErrFirewallCheckFailedSentinel) || !strings.Contains(err.Error(), "198.51.100.9/32") || !strings.Contains(err.Error(), "compared only") {
			t.Fatalf("create with the ingress elsewhere = %v, want FIREWALL_CHECK_FAILED, compared only", err)
		}
		if got := ingressOf(t, w); got != "198.51.100.9/32" {
			t.Fatalf("a read-only ingress was changed: [%s]", got)
		}
	})
}

// With the service down at start the server starts degraded: it lists and
// reads its records, refuses everything that needs the service with 503
// COMPUTE_UNAVAILABLE, and recovers once the service answers.
func TestServiceDegradedStart(t *testing.T) {
	e, w := serviceEnv(t, servicetest.Options{}, tuned(func(o *Options) { o.ListRetry = 20 * time.Millisecond }))
	ctx := ctxT(t)
	for _, name := range []string{"one", "two"} {
		if _, err := e.b.CreateShed(ctx, config.CreateShedRequest{Name: name, NoProvision: true, User: "ada"}); err != nil {
			t.Fatal(err)
		}
		e.settled(name)
	}
	if err := e.b.Close(); err != nil {
		t.Fatal(err)
	}
	w.Down(true)
	b, err := New(Options{
		Config: e.cfg, Provider: w.Client(), Links: e.links, StateDir: e.dir, AuthorizedKeys: []string{testKey},
		FirewallSources: []string{fake.DefaultIngressSource}, SweepInterval: -1, ListRetry: 20 * time.Millisecond,
		ProbeInterval: 5 * time.Millisecond, Logf: e.logf,
	})
	if err != nil {
		t.Fatal(err)
	}
	e.b = b
	go b.Reconcile(ctx)
	short, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	if err := b.WaitReady(short); err == nil {
		t.Fatal("the backend became ready with the service down")
	}
	cancel()

	sheds, err := b.ListSheds(ctx)
	if err != nil || len(sheds) != 2 {
		t.Fatalf("ListSheds while degraded = %d, %v; want the two records", len(sheds), err)
	}
	if shed, err := b.GetShed(ctx, "one"); err != nil || shed.Status != config.StatusRunning {
		t.Fatalf("GetShed while degraded = %+v, %v", shed, err)
	}
	_, err = b.CreateShed(ctx, config.CreateShedRequest{Name: "three", NoProvision: true, User: "ada"})
	computeRefusal(t, err, http.StatusServiceUnavailable, config.ErrComputeUnavailable, "")
	_, err = b.StopShed(ctx, "one")
	computeRefusal(t, err, http.StatusServiceUnavailable, config.ErrComputeUnavailable, "")
	_, err = b.StartShed(ctx, "one")
	computeRefusal(t, err, http.StatusServiceUnavailable, config.ErrComputeUnavailable, "")
	err = b.DeleteShed(ctx, "two")
	computeRefusal(t, err, http.StatusServiceUnavailable, config.ErrComputeUnavailable, "")
	_, err = b.AssignUser(ctx, "one", config.AssignShedRequest{User: "linus"})
	if !errors.Is(err, config.ErrFeatureUnsupportedSentinel) {
		t.Fatalf("assign = %v, want unsupported", err)
	}

	// The service returns: the start-up pass's retry finishes, and
	// everything works.
	w.Down(false)
	if err := b.WaitReady(ctx); err != nil {
		t.Fatalf("WaitReady after the service returned: %v", err)
	}
	if _, err := b.StopShed(ctx, "one"); err != nil {
		t.Fatalf("stop after recovery: %v", err)
	}
	if got := e.settled("one"); got.Status != config.StatusStopped {
		t.Fatalf("after recovery the stop came to %q", got.Status)
	}
}

// A machine whose access options are all of kinds this server does not
// know is marked unreachable with reason access.
func TestAccessWithOnlyUnknownKinds(t *testing.T) {
	e := newEnv(t, manual(), func(e *env, o *fake.Options) { o.NoHostKey = true })
	e.fl.presented = testHostKey
	e.put("odd", config.StatusStarting)
	ref := e.ref("odd")
	e.finish("odd")
	e.prov.SetAccess(ref, nil, 2)
	got := e.settled("odd")
	if got.Status != config.StatusError || got.StatusReason != config.StatusReasonAccess {
		t.Fatalf("settled %q (%q), want error (access)", got.Status, got.StatusReason)
	}
}

// A machine whose access host is outside remote.access.allow is never
// dialled: the shed is unreachable with reason access.
func TestAccessHostOutsideTheAllowList(t *testing.T) {
	e := newEnv(t, func(e *env, _ *fake.Options) { e.cfg.Access.Allow = []string{"198.51.100.0/24"} })
	e.fl.presented = testHostKey
	// Inside the fast-fail window the create's answer is the failure.
	if _, err := e.create("far"); err != nil && !errors.Is(err, errNoAccess) {
		t.Fatalf("create = %v", err)
	}
	got := e.settled("far")
	if got.Status != config.StatusError || got.StatusReason != config.StatusReasonAccess {
		t.Fatalf("settled %q (%q), want error (access)", got.Status, got.StatusReason)
	}
	if !strings.Contains(strings.Join(e.logLines(), "\n"), "outside remote.access.allow; not dialled") {
		t.Errorf("the refusal is not in the log")
	}
}

func TestAccessRuleAllows(t *testing.T) {
	def := accessRule{}
	drivers := accessRule{testDrivers: true}
	listed := accessRule{allow: []netip.Prefix{netip.MustParsePrefix("203.0.113.0/24"), netip.MustParsePrefix("2001:db8::/32")}}
	for _, tc := range []struct {
		rule accessRule
		host string
		want bool
	}{
		{def, "203.0.113.7", true},
		{def, "2001:db8::7", true},
		{def, "127.0.0.1", false},
		{def, "169.254.0.1", false},
		{def, "0.0.0.0", false},
		{def, "224.0.0.1", false},
		{def, "not-an-ip", false},
		{drivers, "127.0.0.1", true},
		{listed, "203.0.113.7", true},
		{listed, "2001:db8::7", true},
		{listed, "198.51.100.7", false},
	} {
		if got := tc.rule.allows(tc.host); got != tc.want {
			t.Errorf("allows(%s) with %+v = %v, want %v", tc.host, tc.rule, got, tc.want)
		}
	}
}

// The service attests a host key other than the one pinned at first
// contact (a service that broke its write-once rule): refused, never
// followed; the start ends with reason host_key.
func TestAttestedHostKeyChangeRefused(t *testing.T) {
	e := newEnv(t)
	e.fl.presented = fake.DefaultHostKey
	e.running("keys")
	if pin := e.storedRecord("keys").HostKeyPin; pin != fake.DefaultHostKey {
		t.Fatalf("pin = %q, want the attested key", pin)
	}
	if _, err := e.b.StopShed(ctxT(t), "keys"); err != nil {
		t.Fatal(err)
	}
	e.settled("keys")
	e.prov.AttestHostKey(e.ref("keys"), testHostKey)
	if _, err := e.b.StartShed(ctxT(t), "keys"); err != nil {
		t.Fatal(err)
	}
	got := e.settled("keys")
	if got.Status != config.StatusStopped || got.StatusReason != config.StatusReasonHostKey {
		t.Fatalf("start after the attested key changed: %q (%q), want stopped (host_key)", got.Status, got.StatusReason)
	}
	if pin := e.storedRecord("keys").HostKeyPin; pin != fake.DefaultHostKey {
		t.Fatalf("the pin changed to %q", pin)
	}
}

// Refusals are mapped by their contract code, never by kind alone.
func TestMapRefusalByCode(t *testing.T) {
	for _, tc := range []struct {
		err          error
		status       int
		code, reason string
		sentinel     error
	}{
		{err: &provider.Error{Kind: provider.ErrDeleting, Code: "DELETING"}, status: 409, code: config.ErrComputeDeleting},
		{err: &provider.Error{Kind: provider.ErrDenied, Code: "INSUFFICIENT_SCOPE"}, status: 403, code: config.ErrComputeDenied},
		{err: &provider.Error{Kind: provider.ErrDenied, Code: "COMPUTE_DENIED"}, status: 403, code: config.ErrComputeDenied},
		{err: &provider.Error{Kind: provider.ErrComputeLimit, Code: "QUOTA_ORG"}, status: 503, code: config.ErrComputeLimitReached, reason: "organization"},
		{err: &provider.Error{Kind: provider.ErrPolicy, Code: "IMAGE_UNKNOWN"}, status: 422, code: config.ErrComputePolicyRefused, reason: "image"},
		{err: &provider.Error{Kind: provider.ErrLease, Code: "LEASE_AT_CAP"}, status: 409, code: "LEASE_AT_CAP"},
		{err: &provider.Error{Kind: provider.ErrUnavailable}, status: 503, code: config.ErrComputeUnavailable},
		{err: &provider.Error{Kind: provider.ErrInvalidReply}, status: 503, code: config.ErrComputeUnavailable},
		{err: &provider.Error{Kind: provider.ErrQuota, Code: "CLOUD_QUOTA"}, sentinel: config.ErrCloudQuotaExceededSentinel},
		{err: &provider.Error{Kind: provider.ErrExists, Code: "EXISTS"}, sentinel: config.ErrShedAlreadyExistsSentinel},
		{err: &provider.Error{Kind: provider.ErrNotFound, Code: "NOT_FOUND"}, sentinel: config.ErrShedNotFoundSentinel},
	} {
		got := mapRefusal(tc.err)
		if tc.sentinel != nil {
			if !errors.Is(got, tc.sentinel) {
				t.Errorf("%v mapped to %v, want %v", tc.err, got, tc.sentinel)
			}
			continue
		}
		computeRefusal(t, got, tc.status, tc.code, tc.reason)
	}
}

// A state directory written by the in-tree cloud provider of earlier
// releases stops the server, naming what to do with the old binary: a
// record whose reference has that provider's shape, one with an
// environment and no machine, and a state directory with only an old
// delete marker (plan 041 R15, criterion 2).
func TestLegacyStateRefusesToStart(t *testing.T) {
	for _, tc := range []struct {
		name  string
		write func(t *testing.T, st *state.Store)
	}{
		{"a record with the old provider's reference", func(t *testing.T, st *state.Store) {
			r := &state.Record{Shed: config.Shed{Name: "old", ID: "abcdefghijklmnopqrstuvwxyz", Status: config.StatusRunning}, Ref: "us-central1-a/shed-dev-abcdefghijklmnopqrstuvwxyz"}
			if err := st.SaveRecord(r); err != nil {
				t.Fatal(err)
			}
		}},
		{"a record with an environment and no machine", func(t *testing.T, st *state.Store) {
			r := &state.Record{Shed: config.Shed{Name: "old", ID: "abcdefghijklmnopqrstuvwxyz", Status: config.StatusStopped}, Env: "dev-home"}
			if err := st.SaveRecord(r); err != nil {
				t.Fatal(err)
			}
		}},
		{"only an old delete marker", func(t *testing.T, st *state.Store) {
			m := &state.Marker{ID: "abcdefghijklmnopqrstuvwxyz", Name: "old", Env: "dev-home", Ref: "us-central1-a/shed-dev-abcdefghijklmnopqrstuvwxyz", At: time.Now()}
			if err := st.SaveMarker(m); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			st, err := state.Open(dir)
			if err != nil {
				t.Fatal(err)
			}
			tc.write(t, st)
			_, err = New(Options{
				Config: &config.RemoteConfig{Provider: config.RemoteProviderFake, AllowTestDrivers: true}, Provider: fake.New(fake.Options{}),
				Links: newFakeLinks(), StateDir: dir, AuthorizedKeys: []string{testKey}, FirewallSources: []string{fake.DefaultIngressSource},
			})
			if !errors.Is(err, ErrLegacyState) {
				t.Fatalf("New = %v, want the legacy state refusal", err)
			}
			for _, want := range []string{"old", "shed delete", "previous shed-server release", dir, config.RemoteUpgradePage} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("the refusal lacks %q: %v", want, err)
				}
			}
		})
	}
	t.Run("this build's state starts", func(t *testing.T) {
		e := newEnv(t)
		e.running("new")
		e.restart()
	})
}

// A name the service holds for a machine of another shed id: EXISTS from a
// live machine is SHED_ALREADY_EXISTS; from one being deleted the create
// waits for it, at most the fast-fail window, and then is 409
// COMPUTE_DELETING with no reason (a client never adopts a shed that is
// going away). A machine that goes within the window lets the create
// through.
// nameWindow is a fast-fail window long enough for a create's name to be
// decided inside it on a loaded machine.
func nameWindow() envOpt {
	return tuned(func(o *Options) { o.FastFailWindow = time.Second })
}

func TestCreateUnderANameTheServiceHolds(t *testing.T) {
	refused := func(t *testing.T, e *env, name string) error {
		t.Helper()
		_, err := e.create(name)
		if _, gerr := e.b.GetShed(ctxT(t), name); !errors.Is(gerr, config.ErrShedNotFoundSentinel) {
			t.Fatalf("the refused create left a record: %v (create: %v) logs: %q", gerr, err, e.logLines())
		}
		return err
	}
	t.Run("a live machine", func(t *testing.T) {
		e := newEnv(t, nameWindow())
		e.seedMachine("taken", newID(t), provider.ShedStateReady, time.Now())
		err := refused(t, e, "taken")
		var cr *config.ComputeRefusal
		if !errors.Is(err, config.ErrShedAlreadyExistsSentinel) || errors.As(err, &cr) {
			t.Fatalf("create = %v, want SHED_ALREADY_EXISTS", err)
		}
	})
	t.Run("a machine being deleted", func(t *testing.T) {
		e := newEnv(t, nameWindow())
		ref := e.seedMachine("taken", newID(t), provider.ShedStateReady, time.Now())
		e.prov.SetDeleting(ref)
		err := refused(t, e, "taken")
		computeRefusal(t, err, http.StatusConflict, config.ErrComputeDeleting, "")
		if errors.Is(err, config.ErrShedAlreadyExistsSentinel) {
			t.Fatalf("create = %v, also SHED_ALREADY_EXISTS", err)
		}
	})
	t.Run("a machine labelled for its delete", func(t *testing.T) {
		e := newEnv(t, nameWindow())
		e.seedMachine("taken", newID(t), provider.ShedStateDeleting, time.Now())
		computeRefusal(t, refused(t, e, "taken"), http.StatusConflict, config.ErrComputeDeleting, "")
	})
	t.Run("gone within the window", func(t *testing.T) {
		e := newEnv(t, tuned(func(o *Options) { o.FastFailWindow = 2 * time.Second }))
		ref := e.seedMachine("taken", newID(t), provider.ShedStateReady, time.Now())
		e.prov.SetDeleting(ref)
		go func() {
			time.Sleep(100 * time.Millisecond)
			e.prov.ExternalDelete(ref)
		}()
		if _, err := e.create("taken"); err != nil {
			t.Fatalf("create once the machine went: %v", err)
		}
		if got := e.settled("taken"); got.Status != config.StatusRunning {
			t.Fatalf("settled %q (%q)", got.Status, got.StatusReason)
		}
	})
}

// With the service stopped (nothing listens: the request is never
// delivered), a create is 503 COMPUTE_UNAVAILABLE and leaves no record, and
// nothing is created once the service is back.
func TestServiceStoppedCreateIsRefused(t *testing.T) {
	e, w := serviceEnv(t, servicetest.Options{})
	w.Stop()
	_, err := e.b.CreateShed(ctxT(t), config.CreateShedRequest{Name: "never", NoProvision: true, User: "ada"})
	computeRefusal(t, err, http.StatusServiceUnavailable, config.ErrComputeUnavailable, "")
	if !errors.Is(err, provider.ErrNotDelivered) {
		t.Fatalf("create = %v, want a request never delivered", err)
	}
	if _, gerr := e.b.GetShed(ctxT(t), "never"); !errors.Is(gerr, config.ErrShedNotFoundSentinel) {
		t.Fatalf("the refused create left a record: %v", gerr)
	}
	if n := markerCount(t, e.dir); n != 0 {
		t.Fatalf("%d delete markers", n)
	}
	w.Restart()
	e.sweep()
	e.idle()
	if l, err := w.Client().List(context.Background()); err != nil || len(l.Instances) != 0 {
		t.Fatalf("machines after the service returned: %d, %v; want none", len(l.Instances), err)
	}
	if sheds, _ := e.b.ListSheds(ctxT(t)); len(sheds) != 0 {
		t.Fatalf("%d sheds recorded", len(sheds))
	}
}

// A service that acts on the create and drops the connection before its
// reply leaves the outcome unknown: the shed is kept starting
// (cloud_unreadable), and the next pass resumes it once, to one machine.
func TestServiceLostCreateReplyIsKeptAndResumedOnce(t *testing.T) {
	e, w := serviceEnv(t, servicetest.Options{})
	w.LoseReplies(http.MethodPost)
	shed, err := e.b.CreateShed(ctxT(t), config.CreateShedRequest{Name: "kept", NoProvision: true, User: "ada"})
	if err != nil {
		t.Fatalf("create = %v; an unknown outcome is not a refusal", err)
	}
	e.idle()
	if st, why := e.status("kept"), e.reason("kept"); st != config.StatusStarting || why != config.StatusReasonCloudUnreadable {
		t.Fatalf("shed %s (%s), want starting (cloud_unreadable)", st, why)
	}
	if n := markerCount(t, e.dir); n != 0 {
		t.Fatalf("%d delete markers: an unknown outcome was cleaned up", n)
	}
	w.LoseReplies("")
	e.sweep()
	if got := e.settled("kept"); got.Status != config.StatusRunning || got.ID != shed.ID {
		t.Fatalf("after the pass: %q (%q)", got.Status, got.StatusReason)
	}
	l, err := w.Client().List(context.Background())
	if err != nil || len(l.Instances) != 1 || string(l.Instances[0].Ref) != shed.ID {
		t.Fatalf("machines: %+v, %v; want the one", l.Instances, err)
	}
}
