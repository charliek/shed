package remote

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/charliek/shed/internal/backend"
	"github.com/charliek/shed/internal/config"
	"github.com/charliek/shed/internal/directory/refserver"
	"github.com/charliek/shed/internal/remote/provider"
	"github.com/charliek/shed/internal/remote/provider/fake"
	"github.com/charliek/shed/internal/remote/provider/service/servicetest"
	"github.com/charliek/shed/internal/remote/state"
)

// A cloud shed's lease and usage through the remote backend and the
// reference compute service (plan 041 criterion 4).

// extendAs extends through the access guard, as the API does, with no
// request id of the caller's.
func extendAs(t *testing.T, e *env, p backend.Principal, name string, hours int) (*config.Shed, error) {
	t.Helper()
	return extendWithID(t, e, p, name, hours, "")
}

// extendWithID is extendAs with the caller's own request id.
func extendWithID(t *testing.T, e *env, p backend.Principal, name string, hours int, callerID string) (*config.Shed, error) {
	t.Helper()
	ex, ok := backend.AsExtender(backend.NewGuard(e.b))
	if !ok {
		t.Fatal("the guard hides the remote backend's extend")
	}
	return ex.ExtendShed(backend.WithPrincipal(ctxT(t), p), name, hours, callerID)
}

// The record carries the machine's cloud facts on every read: the first
// starting record, GetShed and ListSheds, with the catalog's warning time;
// and the JSON a client gets has them under "cloud".
func TestServiceCloudOnTheRecord(t *testing.T) {
	e, w := serviceEnv(t, servicetest.Options{})
	w.Step("policy.set", map[string]any{"warn_s": 5400})
	if err := e.b.refreshCatalog(ctxT(t)); err != nil {
		t.Fatal(err)
	}
	first, err := e.b.CreateShed(ctxT(t), config.CreateShedRequest{Name: "widgets", NoProvision: true, User: "linus"})
	if err != nil {
		t.Fatal(err)
	}
	if first.Cloud == nil || first.Cloud.Size.Name != "standard" || first.Cloud.Lease == nil || first.Cloud.Lease.ExpiresAt.IsZero() {
		t.Fatalf("the first starting record's cloud = %+v", first.Cloud)
	}
	e.settled("widgets")
	got, err := e.b.GetShed(ctxT(t), "widgets")
	if err != nil {
		t.Fatal(err)
	}
	c := got.Cloud
	if c == nil {
		t.Fatal("GetShed: no cloud")
	}
	m, _ := machineOf(t, w, e, "widgets")
	if c.Size != (config.CloudSize{Name: "standard", CPUs: 2, MemoryMB: 8192}) || c.DiskGB != m.DiskGB || c.Image != m.ImageResolved ||
		c.Provider != m.Provider || !c.CreatedAt.Equal(m.CreatedAt) || c.StartedAt == nil || c.StoppedAt != nil ||
		c.Lease == nil || !c.Lease.ExpiresAt.Equal(m.Lease.ExpiresAt) || !c.Lease.MaxExpiresAt.Equal(m.Lease.MaxExpiresAt) ||
		!c.Lease.Extendable || len(c.Lease.PresetsH) == 0 || c.Lease.WarnS == nil || *c.Lease.WarnS != 5400 {
		t.Fatalf("GetShed's cloud = %+v, the machine %+v", c, m)
	}
	list, _ := e.b.ListSheds(ctxT(t))
	if len(list) != 1 || list[0].Cloud == nil || list[0].Cloud.Lease == nil || !list[0].Cloud.Lease.ExpiresAt.Equal(c.Lease.ExpiresAt) {
		t.Fatalf("ListSheds' cloud = %+v", list)
	}
	raw, _ := json.Marshal(got)
	var doc map[string]json.RawMessage
	_ = json.Unmarshal(raw, &doc)
	var cloud map[string]json.RawMessage
	if err := json.Unmarshal(doc["cloud"], &cloud); err != nil {
		t.Fatalf("the record's JSON has no cloud object: %s", raw)
	}
	for _, k := range []string{"provider", "size", "disk_gb", "image", "created_at", "started_at", "lease"} {
		if _, ok := cloud[k]; !ok {
			t.Errorf("cloud has no %q: %s", k, doc["cloud"])
		}
	}
	// The stored record keeps no cloud of its own: it is reported, not
	// stored.
	if rec := e.storedRecord("widgets"); rec.Shed.Cloud != nil {
		t.Fatalf("the stored record holds a cloud: %+v", rec.Shed.Cloud)
	}
}

// Extend moves the lease for the shed's person, an admin of the service
// and a local admin; another person learns nothing; a service client and a
// script key are refused.
func TestServiceExtendWho(t *testing.T) {
	e, w := serviceEnv(t, servicetest.Options{Clients: []string{"nocturn"}})
	e.createFor("widgets", "linus")
	m0, _ := machineOf(t, w, e, "widgets")

	script := backend.UserPrincipal("linus", false)
	script.Script = true
	for _, tc := range []struct {
		name string
		p    backend.Principal
		want error
	}{
		{"another person", backend.UserPrincipal("turing", false), config.ErrShedNotFoundSentinel},
		{"a service client", backend.ClientPrincipal("nocturn"), config.ErrForbiddenSentinel},
		{"a script key", script, config.ErrForbiddenSentinel},
	} {
		if _, err := extendAs(t, e, tc.p, "widgets", 6); !errors.Is(err, tc.want) {
			t.Errorf("%s: %v, want %v", tc.name, err, tc.want)
		}
	}
	if m, _ := machineOf(t, w, e, "widgets"); !m.Lease.ExpiresAt.Equal(m0.Lease.ExpiresAt) {
		t.Fatalf("a refused extend moved the lease: %v -> %v", m0.Lease.ExpiresAt, m.Lease.ExpiresAt)
	}

	want := m0.Lease.ExpiresAt
	for _, tc := range []struct {
		name string
		p    backend.Principal
	}{
		{"the shed's person", backend.UserPrincipal("linus", false)},
		{"an admin of the service", backend.UserPrincipal("ada", true)},
		{"a local admin", backend.UserPrincipal("ops", true)},
	} {
		shed, err := extendAs(t, e, tc.p, "widgets", 6)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		want = want.Add(6 * time.Hour)
		if shed.Cloud == nil || shed.Cloud.Lease == nil || !shed.Cloud.Lease.ExpiresAt.Equal(want) {
			t.Fatalf("%s: the answer's lease = %+v, want it to end at %v", tc.name, shed.Cloud, want)
		}
		if m, _ := machineOf(t, w, e, "widgets"); !m.Lease.ExpiresAt.Equal(want) {
			t.Fatalf("%s: the machine's lease ends at %v, want %v", tc.name, m.Lease.ExpiresAt, want)
		}
	}
	if rec := e.storedRecord("widgets"); rec.Extend != nil || rec.Machine == nil || !rec.Machine.Lease.ExpiresAt.Equal(want) {
		t.Fatalf("the record after the extends: extend %+v, machine %+v", rec.Extend, rec.Machine)
	}
}

// Each of the contract's refusals of an extend reaches the caller with its
// code, and changes nothing.
func TestServiceExtendRefusals(t *testing.T) {
	linus := backend.UserPrincipal("linus", false)

	t.Run("EXTEND_NOT_ALLOWED: a client's machine", func(t *testing.T) {
		e, _ := serviceEnv(t, servicetest.Options{Clients: []string{"nocturn"}})
		if _, err := e.b.CreateShed(ctxT(t), config.CreateShedRequest{Name: "nocturn-1", NoProvision: true, Owner: "nocturn"}); err != nil {
			t.Fatal(err)
		}
		e.settled("nocturn-1")
		_, err := extendAs(t, e, backend.UserPrincipal("ada", true), "nocturn-1", 6)
		computeRefusal(t, err, http.StatusForbidden, config.ErrExtendNotAllowed, "")
	})
	t.Run("EXTEND_NOT_ALLOWED: the policy", func(t *testing.T) {
		e, w := serviceEnv(t, servicetest.Options{})
		e.createFor("widgets", "linus")
		w.Step("policy.set", map[string]any{"extend_allowed": false})
		_, err := extendAs(t, e, linus, "widgets", 6)
		computeRefusal(t, err, http.StatusForbidden, config.ErrExtendNotAllowed, "")
	})
	t.Run("LEASE_AT_CAP", func(t *testing.T) {
		e, w := serviceEnv(t, servicetest.Options{})
		e.createFor("widgets", "linus")
		m0, _ := machineOf(t, w, e, "widgets")
		_, err := extendAs(t, e, linus, "widgets", 720)
		computeRefusal(t, err, http.StatusConflict, config.ErrLeaseAtCap, "")
		if m, _ := machineOf(t, w, e, "widgets"); !m.Lease.ExpiresAt.Equal(m0.Lease.ExpiresAt) {
			t.Fatal("a refused extend moved the lease")
		}
		if rec := e.storedRecord("widgets"); rec.Extend != nil {
			t.Fatalf("a refused extend left its request id: %+v", rec.Extend)
		}
	})
	t.Run("PERSON_INACTIVE", func(t *testing.T) {
		e, w := serviceEnv(t, servicetest.Options{})
		e.createFor("widgets", "linus")
		w.SetPeople([]refserver.User{{Name: "ada", Admin: true, Keys: []refserver.Key{{Key: servicetest.NewKey(t), Label: "laptop"}}}})
		_, err := extendAs(t, e, backend.UserPrincipal("ada", true), "widgets", 6)
		computeRefusal(t, err, http.StatusForbidden, config.ErrPersonInactive, "")
	})
	t.Run("COMPUTE_DELETING", func(t *testing.T) {
		e, w := serviceEnv(t, servicetest.Options{})
		e.createFor("widgets", "linus")
		// The service's delete stays in progress: nothing settles it.
		w.Ref.ComputeSettings(0, w.HostKey, nil)
		id := e.settledID("widgets")
		if _, err := w.Client().Destroy(ctxT(t), provider.Ref(id), provider.NewRequestID()); err != nil {
			t.Fatal(err)
		}
		_, err := extendAs(t, e, linus, "widgets", 6)
		computeRefusal(t, err, http.StatusConflict, config.ErrComputeDeleting, "")
	})
	t.Run("COMPUTE_UNAVAILABLE", func(t *testing.T) {
		e, w := serviceEnv(t, servicetest.Options{})
		e.createFor("widgets", "linus")
		w.Down(true)
		_, err := extendAs(t, e, linus, "widgets", 6)
		computeRefusal(t, err, http.StatusServiceUnavailable, config.ErrComputeUnavailable, "")
	})
	t.Run("LEASE_ENDED", func(t *testing.T) {
		// The reference service's own clock deletes a machine at its
		// lease's end; the fake's stands at the end without deleting.
		clock := newTestClock()
		e := newEnv(t, withClock(clock), func(_ *env, o *fake.Options) { o.Now = clock.Now })
		e.createFor("widgets", "linus")
		rec := e.storedRecord("widgets")
		clock.Advance(rec.Machine.Lease.ExpiresAt.Sub(clock.Now()) + time.Second)
		_, err := extendAs(t, e, linus, "widgets", 6)
		computeRefusal(t, err, http.StatusConflict, config.ErrLeaseEnded, "")
	})
	t.Run("hours out of range", func(t *testing.T) {
		e, _ := serviceEnv(t, servicetest.Options{})
		e.createFor("widgets", "linus")
		for _, h := range []int{0, -1, 721} {
			if _, err := extendAs(t, e, linus, "widgets", h); !errors.Is(err, config.ErrInvalidShedRequestSentinel) {
				t.Errorf("extend by %d: %v, want invalid", h, err)
			}
		}
	})
}

// Extend's request ids (lead ruling on A3 finding 2): a reply the service
// loses is sent again by the server with its id; a reply the caller loses is
// sent again by the caller with its own id, and moves the lease once even
// after the server cleared its record; two requests are two extends; an
// extend whose outcome is unknown is not answered for by the next request.
func TestServiceExtendRequestIDs(t *testing.T) {
	linus := backend.UserPrincipal("linus", false)
	const callerID = "6f9619ff-8b86-4d01-b42d-00cf4fc964ff"

	t.Run("the service's reply lost once", func(t *testing.T) {
		e, w := serviceEnv(t, servicetest.Options{})
		e.createFor("widgets", "linus")
		m0, _ := machineOf(t, w, e, "widgets")
		w.Step("cloud.fail", []string{"extend", "lost_reply"})
		shed, err := extendAs(t, e, linus, "widgets", 6)
		if err != nil {
			t.Fatal(err)
		}
		want := m0.Lease.ExpiresAt.Add(6 * time.Hour)
		if m, _ := machineOf(t, w, e, "widgets"); !m.Lease.ExpiresAt.Equal(want) || !shed.Cloud.Lease.ExpiresAt.Equal(want) {
			t.Fatalf("after a lost reply the lease ends at %v (answer %v), want %v once", m.Lease.ExpiresAt, shed.Cloud.Lease.ExpiresAt, want)
		}
		if rec := e.storedRecord("widgets"); rec.Extend != nil {
			t.Fatalf("a settled extend left its request id: %+v", rec.Extend)
		}
	})

	t.Run("the caller's reply lost, retried with its id", func(t *testing.T) {
		var ids []string
		e, w := serviceEnvWith(t, servicetest.Options{}, func(p provider.Provider) provider.Provider {
			return &extendSpy{Provider: p, ids: &ids}
		})
		e.createFor("widgets", "linus")
		m0, _ := machineOf(t, w, e, "widgets")
		// The first POST succeeds and the server clears its record; its
		// answer never reaches the caller, who sends the POST again.
		for i := 0; i < 2; i++ {
			if _, err := extendWithID(t, e, linus, "widgets", 6, callerID); err != nil {
				t.Fatal(err)
			}
			if rec := e.storedRecord("widgets"); rec.Extend != nil {
				t.Fatalf("send %d left its request id: %+v", i, rec.Extend)
			}
		}
		want := m0.Lease.ExpiresAt.Add(6 * time.Hour)
		if m, _ := machineOf(t, w, e, "widgets"); !m.Lease.ExpiresAt.Equal(want) {
			t.Fatalf("a retried POST moved the lease %v, want 6h once", m.Lease.ExpiresAt.Sub(m0.Lease.ExpiresAt))
		}
		if len(ids) != 2 || ids[0] != ids[1] || ids[0] == callerID {
			t.Fatalf("the sends carried %v: want one id, derived from the caller's", ids)
		}
		// Another caller with the same id of its own is another extend.
		if _, err := extendWithID(t, e, backend.UserPrincipal("ada", true), "widgets", 6, callerID); err != nil {
			t.Fatal(err)
		}
		if ids[2] == ids[0] {
			t.Fatal("another caller's id was the same request id")
		}
		if m, _ := machineOf(t, w, e, "widgets"); !m.Lease.ExpiresAt.Equal(want.Add(6 * time.Hour)) {
			t.Fatalf("another caller's extend: the lease ends at %v", m.Lease.ExpiresAt)
		}
	})

	t.Run("two requests are two extends", func(t *testing.T) {
		e, w := serviceEnv(t, servicetest.Options{})
		e.createFor("widgets", "linus")
		m0, _ := machineOf(t, w, e, "widgets")
		if _, err := extendWithID(t, e, linus, "widgets", 6, callerID); err != nil {
			t.Fatal(err)
		}
		if _, err := extendWithID(t, e, linus, "widgets", 6, "0b4e7a0e-5f2c-4b3d-9a1e-2c7d8e9f0a1b"); err != nil {
			t.Fatal(err)
		}
		if _, err := extendAs(t, e, linus, "widgets", 6); err != nil {
			t.Fatal(err)
		}
		if m, _ := machineOf(t, w, e, "widgets"); !m.Lease.ExpiresAt.Equal(m0.Lease.ExpiresAt.Add(18 * time.Hour)) {
			t.Fatalf("three requests moved the lease %v, want 18h", m.Lease.ExpiresAt.Sub(m0.Lease.ExpiresAt))
		}
	})

	t.Run("an unknown outcome, then a new request", func(t *testing.T) {
		var ids []string
		e, w := serviceEnvWith(t, servicetest.Options{}, func(p provider.Provider) provider.Provider {
			return &extendSpy{Provider: p, ids: &ids}
		})
		e.createFor("widgets", "linus")
		m0, _ := machineOf(t, w, e, "widgets")
		w.LoseReplies(http.MethodPost)
		_, err := extendAs(t, e, linus, "widgets", 6)
		computeRefusal(t, err, http.StatusServiceUnavailable, config.ErrComputeUnavailable, "")
		w.LoseReplies("")
		rec := e.storedRecord("widgets")
		if rec.Extend == nil || rec.Extend.Hours != 6 || rec.Extend.RequestID == "" {
			t.Fatalf("an extend of unknown outcome left no request id: %+v", rec.Extend)
		}
		first := rec.Extend.RequestID
		for _, id := range ids {
			if id != first {
				t.Fatalf("the server's own retries sent ids %v, want only %s", ids, first)
			}
		}
		n := len(ids)
		if _, err := extendAs(t, e, linus, "widgets", 6); err != nil {
			t.Fatal(err)
		}
		if ids[n] == first {
			t.Fatal("a new request sent the unknown extend's id")
		}
		// The first extend landed at the service; the new one is another.
		if m, _ := machineOf(t, w, e, "widgets"); !m.Lease.ExpiresAt.Equal(m0.Lease.ExpiresAt.Add(12 * time.Hour)) {
			t.Fatalf("the lease moved %v, want 12h", m.Lease.ExpiresAt.Sub(m0.Lease.ExpiresAt))
		}
	})

	t.Run("a request id that is not flushed sends nothing", func(t *testing.T) {
		var ids []string
		e, w := serviceEnvWith(t, servicetest.Options{}, func(p provider.Provider) provider.Provider {
			return &extendSpy{Provider: p, ids: &ids}
		})
		e.createFor("widgets", "linus")
		m0, _ := machineOf(t, w, e, "widgets")
		save := e.b.saveRecord
		e.b.saveRecord = func(r *state.Record) error {
			if err := save(r); err != nil {
				return err
			}
			return state.ErrNotDurable
		}
		_, err := extendWithID(t, e, linus, "widgets", 6, callerID)
		computeRefusal(t, err, http.StatusServiceUnavailable, config.ErrComputeUnavailable, "")
		if len(ids) != 0 {
			t.Fatalf("an extend whose id was not flushed was sent: %v", ids)
		}
		e.b.saveRecord = save
		if _, err := extendWithID(t, e, linus, "widgets", 6, callerID); err != nil {
			t.Fatal(err)
		}
		if m, _ := machineOf(t, w, e, "widgets"); !m.Lease.ExpiresAt.Equal(m0.Lease.ExpiresAt.Add(6 * time.Hour)) {
			t.Fatalf("the retry moved the lease %v, want 6h", m.Lease.ExpiresAt.Sub(m0.Lease.ExpiresAt))
		}
	})

	t.Run("a request id that is not a UUID", func(t *testing.T) {
		e, _ := serviceEnv(t, servicetest.Options{})
		e.createFor("widgets", "linus")
		if _, err := e.b.ExtendShed(backend.WithPrincipal(ctxT(t), linus), "widgets", 6, "not-a-uuid"); !errors.Is(err, config.ErrInvalidShedRequestSentinel) {
			t.Fatalf("a malformed id: %v", err)
		}
	})
}

// extendSpy records the request ids extends are sent with.
type extendSpy struct {
	provider.Provider
	ids *[]string
}

func (s *extendSpy) Extend(ctx context.Context, ref provider.Ref, rid string, by time.Duration) (provider.Instance, error) {
	*s.ids = append(*s.ids, rid)
	return s.Provider.Extend(ctx, ref, rid, by)
}

// A client's own lease reaches the service: its machine is deleted that
// long after it is made.
func TestServiceClientLease(t *testing.T) {
	e, w := serviceEnv(t, servicetest.Options{Clients: []string{"nocturn"}})
	if _, err := e.b.CreateShed(ctxT(t), config.CreateShedRequest{Name: "nocturn-1", NoProvision: true, Owner: "nocturn", ClientLease: 30 * time.Hour}); err != nil {
		t.Fatal(err)
	}
	e.settled("nocturn-1")
	m, err := machineOf(t, w, e, "nocturn-1")
	if err != nil || m.Lease.ExpiresAt.Sub(m.CreatedAt) != 30*time.Hour || !m.Lease.MaxExpiresAt.Equal(m.Lease.ExpiresAt) {
		t.Fatalf("the client's machine: %+v, %v", m.Lease, err)
	}
	// Above the service's longest client lease: the service refuses it.
	w.Step("policy.set", map[string]any{"client_max_lease_s": 3600})
	_, err = e.b.CreateShed(ctxT(t), config.CreateShedRequest{Name: "nocturn-2", NoProvision: true, Owner: "nocturn", ClientLease: 2 * time.Hour})
	computeRefusal(t, err, http.StatusUnprocessableEntity, config.ErrComputePolicyRefused, "lease")
}

// Usage: a person's own counts and the organization's; a local admin is
// never named to the service and gets the organization's only; answers
// are kept 30 seconds per user.
func TestServiceUsage(t *testing.T) {
	clock := newTestClock()
	var reads atomic.Int32
	e, _ := serviceEnvWith(t, servicetest.Options{}, func(p provider.Provider) provider.Provider {
		return &usageSpy{Provider: p, reads: &reads}
	}, withClock(clock))
	e.createFor("one", "linus")
	e.createFor("two", "linus")
	if _, err := e.b.StopShed(ctxT(t), "two"); err != nil {
		t.Fatal(err)
	}
	e.settled("two")

	u, err := e.b.CloudUsage(ctxT(t), "linus")
	if err != nil {
		t.Fatal(err)
	}
	if u.Person == nil || *u.Person != (config.CloudCounts{Machines: 2, Running: 1, Stopped: 1, Max: 100}) || u.Org.Machines != 2 || u.Org.Max != 100 {
		t.Fatalf("linus's usage = %+v (person %+v)", u, u.Person)
	}
	n := reads.Load()
	if _, err := e.b.CloudUsage(ctxT(t), "linus"); err != nil || reads.Load() != n {
		t.Fatalf("a second read inside 30 s asked the service again (%d -> %d): %v", n, reads.Load(), err)
	}
	clock.Advance(usageTTL + time.Second)
	if _, err := e.b.CloudUsage(ctxT(t), "linus"); err != nil || reads.Load() != n+1 {
		t.Fatalf("a read after 30 s did not ask the service (%d -> %d): %v", n, reads.Load(), err)
	}
	// A local admin: the organization only, and the name never sent.
	u, err = e.b.CloudUsage(ctxT(t), "ops")
	if err != nil || u.Person != nil || u.Org.Machines != 2 {
		t.Fatalf("a local admin's usage = %+v, %v", u, err)
	}
	// The organization alone.
	if u, err := e.b.CloudUsage(ctxT(t), ""); err != nil || u.Person != nil || u.Org.Running != 1 || u.Org.Stopped != 1 {
		t.Fatalf("the organization's usage = %+v, %v", u, err)
	}
}

type usageSpy struct {
	provider.Provider
	reads *atomic.Int32
}

func (s *usageSpy) Usage(ctx context.Context, user string) (provider.Usage, error) {
	s.reads.Add(1)
	if user == "ops" {
		return provider.Usage{}, errors.New("a local admin's name reached the service")
	}
	return s.Provider.Usage(ctx, user)
}

// The service down: usage answers COMPUTE_UNAVAILABLE (the CLI's footer
// leaves the line out).
func TestServiceUsageUnavailable(t *testing.T) {
	e, w := serviceEnv(t, servicetest.Options{})
	w.Down(true)
	_, err := e.b.CloudUsage(ctxT(t), "linus")
	computeRefusal(t, err, http.StatusServiceUnavailable, config.ErrComputeUnavailable, "")
}

// Every remote record carries cloud: before the service has answered for
// the machine, what the server asked for, and no lease (unknown).
func TestCloudBeforeTheMachineAnswers(t *testing.T) {
	at := time.Date(2026, 11, 26, 9, 0, 0, 0, time.UTC)
	spec := provider.Spec{Size: "standard", DiskGB: 50, Image: "default"}
	rec := &state.Record{Shed: config.Shed{Name: "w", CPUs: 2, MemoryMB: 8192, CreatedAt: at}, Spec: &spec}
	c := cloudOf(rec, nil)
	if c == nil || c.Size != (config.CloudSize{Name: "standard", CPUs: 2, MemoryMB: 8192}) || c.DiskGB != 50 || c.Image != "default" ||
		!c.CreatedAt.Equal(at) || c.Lease != nil || c.Provider != "" {
		t.Fatalf("cloud before the answer = %+v", c)
	}
	raw, _ := json.Marshal(c)
	if string(raw) != `{"size":{"name":"standard","cpus":2,"memory_mb":8192},"disk_gb":50,"image":"default","created_at":"2026-11-26T09:00:00Z"}` {
		t.Fatalf("cloud before the answer, as JSON: %s", raw)
	}
	// A record with neither machine nor spec still carries one.
	if c := cloudOf(&state.Record{Shed: config.Shed{Name: "w", CreatedAt: at}}, nil); c == nil || c.Lease != nil {
		t.Fatalf("a bare record's cloud = %+v", c)
	}
	// After the answer: the lease, with the catalog's warning time, 0
	// included (no warning), and none when the catalog is not read.
	m := provider.Instance{Ref: "x", Lease: provider.Lease{ExpiresAt: at.Add(time.Hour), MaxExpiresAt: at.Add(2 * time.Hour)}}
	rec.Machine = &m
	zero := time.Duration(0)
	if c := cloudOf(rec, &zero); c.Lease == nil || c.Lease.WarnS == nil || *c.Lease.WarnS != 0 || c.Lease.Soon(at) {
		t.Fatalf("warn_s 0: %+v", c.Lease)
	}
	if c := cloudOf(rec, nil); c.Lease == nil || c.Lease.WarnS != nil || !c.Lease.Soon(at) {
		t.Fatalf("no catalog: %+v", c.Lease)
	}
}

// Usage's ?user= through the access guard: a person of the service who is
// an admin may name another; a local admin may name nobody, not even
// themselves; a person names only themselves.
func TestServiceUsageQuery(t *testing.T) {
	e, _ := serviceEnv(t, servicetest.Options{})
	e.createFor("one", "linus")
	cu, ok := backend.AsCloudUsager(backend.NewGuard(e.b))
	if !ok {
		t.Fatal("the guard hides the remote backend's usage")
	}
	ask := func(p backend.Principal, user string) (config.CloudUsage, error) {
		return cu.CloudUsage(backend.WithPrincipal(ctxT(t), p), user)
	}
	if u, err := ask(backend.UserPrincipal("ada", true), "linus"); err != nil || u.Person == nil || u.Person.Machines != 1 {
		t.Fatalf("an admin of the service naming linus: %+v, %v", u, err)
	}
	for _, asked := range []string{"linus", "ops"} {
		if _, err := ask(backend.UserPrincipal("ops", true), asked); !errors.Is(err, config.ErrForbiddenSentinel) {
			t.Fatalf("a local admin naming %s: %v, want forbidden", asked, err)
		}
	}
	if u, err := ask(backend.UserPrincipal("ops", true), ""); err != nil || u.Person != nil {
		t.Fatalf("a local admin's own: %+v, %v", u, err)
	}
	if u, err := ask(backend.UserPrincipal("linus", false), "linus"); err != nil || u.Person == nil {
		t.Fatalf("a person naming themselves: %+v, %v", u, err)
	}
	if _, err := ask(backend.UserPrincipal("turing", false), "linus"); !errors.Is(err, config.ErrForbiddenSentinel) {
		t.Fatalf("a person naming another: %v", err)
	}
	if _, err := ask(backend.ClientPrincipal("nocturn"), "linus"); !errors.Is(err, config.ErrForbiddenSentinel) {
		t.Fatalf("a client naming a person: %v", err)
	}
}

// A request id whose write is in place but not flushed (state.ErrNotDurable)
// is never sent (lead ruling on A3 finding 3): a start keeps its shed
// starting and sends nothing until a later run writes an id that holds; a
// fresh create is refused with nothing made.
func TestNotFlushedRequestIDsSendNothing(t *testing.T) {
	notFlushed := func(e *env, call string) func() {
		save := e.b.saveRecord
		e.b.saveRecord = func(r *state.Record) error {
			if err := save(r); err != nil {
				return err
			}
			if r.Mutation != nil && r.Mutation.Call == call {
				return state.ErrNotDurable
			}
			return nil
		}
		return func() { e.b.saveRecord = save }
	}

	t.Run("start", func(t *testing.T) {
		e := newEnv(t)
		e.createFor("box", "alice")
		if _, err := e.b.StopShed(ctxT(t), "box"); err != nil {
			t.Fatal(err)
		}
		e.settled("box")
		restore := notFlushed(e, state.JobStart)
		before := e.count(fake.OpStart)
		if _, err := e.b.StartShed(ctxT(t), "box"); err != nil {
			t.Fatal(err)
		}
		e.idle()
		got, err := e.b.GetShed(ctxT(t), "box")
		if err != nil {
			t.Fatal(err)
		}
		if e.count(fake.OpStart) != before {
			t.Fatal("a start whose request id was not flushed was sent")
		}
		if got.Status != config.StatusStarting || got.StatusReason != config.StatusReasonCloudUnreadable {
			t.Fatalf("the shed is %s (%s), want starting and kept", got.Status, got.StatusReason)
		}
		restore()
		e.sweep()
		if got := e.settled("box"); got.Status != config.StatusRunning {
			t.Fatalf("after the write holds: %s (%s)", got.Status, got.StatusReason)
		}
		if e.count(fake.OpStart) != before+1 {
			t.Fatalf("starts sent: %d, want one", e.count(fake.OpStart)-before)
		}
	})

	t.Run("a fresh create", func(t *testing.T) {
		e := newEnv(t)
		notFlushed(e, state.JobCreate)
		_, err := e.b.CreateShed(ctxT(t), config.CreateShedRequest{Name: "box", NoProvision: true, User: "alice"})
		if err == nil {
			if got := e.settled("box"); got.Status == config.StatusRunning {
				t.Fatal("a create whose note was not flushed ran")
			}
		}
		if n := e.count(fake.OpCreate); n != 0 {
			t.Fatalf("%d creates sent", n)
		}
	})
}

// A delete marker's request id is sent only once a save holding it has been
// flushed in this process (re-review finding 1): a new id whose save is not
// flushed is taken back; an id read back from disk (a save whose rename
// landed and whose flush failed) is saved again, durably, before it is sent.
func TestMarkerRequestIDsAreFlushedBeforeSending(t *testing.T) {
	t.Run("the request ids", func(t *testing.T) {
		e := newEnv(t)
		saves, fail := 0, true
		e.b.saveMarker = func(m *state.Marker) error {
			saves++
			if fail {
				return state.ErrNotDurable
			}
			return nil
		}
		// A new id, not flushed: taken back.
		m := &state.Marker{ID: "s1", Name: "box"}
		if _, err := e.b.markerRequestID(m, &m.DestroyID); !errors.Is(err, state.ErrNotDurable) || m.DestroyID != "" {
			t.Fatalf("a new id not flushed: %v, field %q", err, m.DestroyID)
		}
		// An id read back from disk: not sent until a save of it holds.
		const loaded = "6f9619ff-8b86-4d01-b42d-00cf4fc964ff"
		m = &state.Marker{ID: "s2", Name: "box", DestroyID: loaded}
		if _, err := e.b.markerRequestID(m, &m.DestroyID); !errors.Is(err, state.ErrNotDurable) || m.DestroyID != loaded {
			t.Fatalf("a loaded id not flushed: %v, field %q", err, m.DestroyID)
		}
		fail = false
		n := saves
		if rid, err := e.b.markerRequestID(m, &m.DestroyID); err != nil || rid != loaded || saves != n+1 {
			t.Fatalf("a loaded id once flushed: %q, %v, %d saves", rid, err, saves-n)
		}
		if rid, err := e.b.markerRequestID(m, &m.DestroyID); err != nil || rid != loaded || saves != n+1 {
			t.Fatalf("a flushed id saved again: %q, %v, %d saves", rid, err, saves-n)
		}
	})

	t.Run("a delete sends no destroy until its id holds", func(t *testing.T) {
		e := newEnv(t)
		e.createFor("box", "alice")
		save := e.b.saveMarker
		var failing atomic.Bool
		failing.Store(true)
		e.b.saveMarker = func(m *state.Marker) error {
			if err := save(m); err != nil {
				return err
			}
			if failing.Load() && m.DestroyID != "" {
				return state.ErrNotDurable
			}
			return nil
		}
		if err := e.b.DeleteShed(ctxT(t), "box"); err != nil {
			t.Fatal(err)
		}
		e.idle()
		if n := e.count(fake.OpDestroy); n != 0 {
			t.Fatalf("%d destroys sent under an id that was not flushed", n)
		}
		failing.Store(false)
		e.sweep()
		e.idle()
		if n := e.count(fake.OpDestroy); n != 1 || e.instances() != 0 {
			t.Fatalf("after the id holds: %d destroys, %d machines", n, e.instances())
		}
	})
}

// A then B then A again (re-review finding 2): the service answers the
// repeated A with A's first answer, a lease older than the machine's; the
// record and the answer keep the machine's newest view.
func TestExtendReplayKeepsTheNewestMachine(t *testing.T) {
	const a, bID = "6f9619ff-8b86-4d01-b42d-00cf4fc964ff", "0b4e7a0e-5f2c-4b3d-9a1e-2c7d8e9f0a1b"
	linus := backend.UserPrincipal("linus", false)

	check := func(t *testing.T, e *env, w *servicetest.World, want time.Time, shed *config.Shed) {
		t.Helper()
		if m, _ := machineOf(t, w, e, "widgets"); !m.Lease.ExpiresAt.Equal(want) {
			t.Fatalf("the machine's lease ends at %v, want %v", m.Lease.ExpiresAt, want)
		}
		if shed.Cloud == nil || shed.Cloud.Lease == nil || !shed.Cloud.Lease.ExpiresAt.Equal(want) {
			t.Fatalf("the answer's lease: %+v, want %v", shed.Cloud, want)
		}
		got, _ := e.b.GetShed(ctxT(t), "widgets")
		if rec := e.storedRecord("widgets"); !got.Cloud.Lease.ExpiresAt.Equal(want) || !rec.Machine.Lease.ExpiresAt.Equal(want) {
			t.Fatalf("the record's lease ends at %v (stored %v), want %v", got.Cloud.Lease.ExpiresAt, rec.Machine.Lease.ExpiresAt, want)
		}
	}

	t.Run("the machine read afresh", func(t *testing.T) {
		e, w := serviceEnv(t, servicetest.Options{})
		e.createFor("widgets", "linus")
		m0, _ := machineOf(t, w, e, "widgets")
		if _, err := extendWithID(t, e, linus, "widgets", 6, a); err != nil {
			t.Fatal(err)
		}
		if _, err := extendWithID(t, e, linus, "widgets", 6, bID); err != nil {
			t.Fatal(err)
		}
		shed, err := extendWithID(t, e, linus, "widgets", 6, a)
		if err != nil {
			t.Fatal(err)
		}
		check(t, e, w, m0.Lease.ExpiresAt.Add(12*time.Hour), shed)
	})

	t.Run("the read fails: the older answer is not kept", func(t *testing.T) {
		var failGet atomic.Bool
		e, w := serviceEnvWith(t, servicetest.Options{}, func(p provider.Provider) provider.Provider {
			return &getFailer{Provider: p, fail: &failGet}
		})
		e.createFor("widgets", "linus")
		m0, _ := machineOf(t, w, e, "widgets")
		for _, id := range []string{a, bID} {
			if _, err := extendWithID(t, e, linus, "widgets", 6, id); err != nil {
				t.Fatal(err)
			}
		}
		failGet.Store(true)
		shed, err := extendWithID(t, e, linus, "widgets", 6, a)
		failGet.Store(false)
		if err != nil {
			t.Fatal(err)
		}
		check(t, e, w, m0.Lease.ExpiresAt.Add(12*time.Hour), shed)
	})
}

// getFailer fails Get while fail is set.
type getFailer struct {
	provider.Provider
	fail *atomic.Bool
}

func (g *getFailer) Get(ctx context.Context, ref provider.Ref) (provider.Instance, error) {
	if g.fail.Load() {
		return provider.Instance{}, provider.Errorf(provider.ErrUnavailable, "get", "down")
	}
	return g.Provider.Get(ctx, ref)
}

func TestNotOlder(t *testing.T) {
	at := time.Date(2026, 11, 26, 9, 0, 0, 0, time.UTC)
	held := provider.Instance{StartedAt: at, StoppedAt: at.Add(time.Hour), Lease: provider.Lease{ExpiresAt: at.Add(24 * time.Hour)}}
	for _, tc := range []struct {
		name string
		inst provider.Instance
		want bool
	}{
		{"the same", held, true},
		{"a later lease", provider.Instance{StartedAt: at, StoppedAt: at.Add(time.Hour), Lease: provider.Lease{ExpiresAt: at.Add(30 * time.Hour)}}, true},
		{"an earlier lease", provider.Instance{StartedAt: at, StoppedAt: at.Add(time.Hour), Lease: provider.Lease{ExpiresAt: at.Add(18 * time.Hour)}}, false},
		{"started since (stopped_at cleared)", provider.Instance{StartedAt: at.Add(2 * time.Hour), Lease: provider.Lease{ExpiresAt: at.Add(24 * time.Hour)}}, true},
		{"from before the stop", provider.Instance{StartedAt: at, Lease: provider.Lease{ExpiresAt: at.Add(24 * time.Hour)}}, false},
	} {
		if got := notOlder(tc.inst, &held); got != tc.want {
			t.Errorf("%s: %v, want %v", tc.name, got, tc.want)
		}
	}
	if !notOlder(held, nil) {
		t.Error("no machine held")
	}
}
