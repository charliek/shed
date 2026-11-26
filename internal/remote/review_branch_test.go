package remote

import (
	"context"
	"errors"
	"net/http"
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
	"github.com/charliek/shed/internal/directory/refserver"
	"github.com/charliek/shed/internal/remote/provider"
	"github.com/charliek/shed/internal/remote/provider/fake"
	"github.com/charliek/shed/internal/remote/provider/service/servicetest"
	"github.com/charliek/shed/internal/remote/state"
	"github.com/charliek/shed/internal/remote/transport"
)

// The fixes of the whole-branch review of plan 041 part A: one test (or
// more) for each finding.

// withAudit gives every backend the environment opens an audit log, from
// its construction (Options.AuditLog), and returns the log's path.
func withAudit(t *testing.T) (envOpt, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), audit.FileName)
	l, err := audit.Open(path, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	return func(e *env, _ *fake.Options) {
		prev := e.tune
		e.tune = func(o *Options) {
			if prev != nil {
				prev(o)
			}
			o.AuditLog = l
		}
	}, path
}

// auditHas reports whether the audit log at path holds a service-deletion
// event with cause.
func auditHas(t *testing.T, path, cause string) bool {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return strings.Contains(string(data), `"cause":"`+cause+`"`) && strings.Contains(string(data), `"shed.delete"`)
}

// createCounter counts the creates sent to the provider.
type createCounter struct {
	provider.Provider
	n atomic.Int32
}

func (c *createCounter) Create(ctx context.Context, spec provider.Spec) (provider.Pending, provider.Instance, error) {
	c.n.Add(1)
	return c.Provider.Create(ctx, spec)
}

// --- 1: a kept create never makes a second machine for a deleted shed ---

// A create whose guest never answers is kept starting (unreachable). When
// its lease ends at the service, the next pass, or the next server start,
// drops its record with the cause, in the log and the audit log, and never
// sends the create again: no second machine is made.
func TestKeptCreateOfADeletedMachineIsNotMadeAgain(t *testing.T) {
	for _, tc := range []struct {
		name string
		next func(e *env)
	}{
		{"the next pass", func(e *env) { e.sweep() }},
		{"the next server start", func(e *env) { e.restart() }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			counter := &createCounter{}
			auditOpt, path := withAudit(t)
			fast := func(e *env, _ *fake.Options) {
				prev := e.tune
				e.tune = func(o *Options) {
					if prev != nil {
						prev(o)
					}
					o.BootTimeout = 100 * time.Millisecond
				}
			}
			e, w := serviceEnvWith(t, servicetest.Options{}, func(p provider.Provider) provider.Provider {
				counter.Provider = p
				return counter
			}, auditOpt, fast)
			e.fl.failAlways("deaf", transport.ErrUnreachable)
			if _, err := e.b.CreateShed(ctxT(t), config.CreateShedRequest{Name: "deaf", NoProvision: true, User: "linus"}); err != nil {
				t.Fatal(err)
			}
			e.idle()
			if got, why := e.status("deaf"), e.reason("deaf"); got != config.StatusStarting || why != config.StatusReasonUnreachable {
				t.Fatalf("shed is %s (%s), want starting (unreachable)", got, why)
			}
			w.Step("clock.advance", 86400+1)
			tc.next(e)
			e.idle()
			if _, err := e.b.GetShed(ctxT(t), "deaf"); !errors.Is(err, config.ErrShedNotFoundSentinel) {
				t.Fatalf("GetShed = %v, want the record dropped", err)
			}
			if n := counter.n.Load(); n != 1 {
				t.Fatalf("%d creates sent, want the first only", n)
			}
			if l, err := w.Client().List(ctxT(t)); err != nil || len(l.Instances) != 0 {
				t.Fatalf("machines at the service: %+v (%v), want none", l.Instances, err)
			}
			if log := strings.Join(e.logLines(), "\n"); !strings.Contains(log, "deleted its machine (cause lease_ended)") {
				t.Fatalf("no line names the cause:\n%s", log)
			}
			if !auditHas(t, path, provider.CauseLeaseEnded) {
				t.Fatal("no service-deletion event in the audit log")
			}
		})
	}
}

// --- 2: a refusal the service made before acting stays one ---

// A fresh create every attempt of which the service refused before acting
// on it (a 429 whose Retry-After runs past the deadline, a second 401) is
// answered 503 COMPUTE_UNAVAILABLE inside the window, with no record and no
// machine: it is not a lost reply, kept as an accepted create.
func TestCreateTheServiceDidNotActOnIsRefused(t *testing.T) {
	for _, tc := range []struct {
		name  string
		reply refserver.ComputeReply
	}{
		{"rate limited past the deadline", refserver.ComputeReply{Status: http.StatusTooManyRequests, Headers: map[string]string{"Retry-After": "30"},
			Body: []byte(`{"error":{"code":"RATE_LIMITED","message":"slow down"}}`)}},
		{"a second 401", refserver.ComputeReply{Status: http.StatusUnauthorized,
			Body: []byte(`{"error":{"code":"UNAUTHORIZED","message":"no"}}`)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, w := serviceEnv(t, servicetest.Options{})
			w.Ref.ComputeForceReply("create", &tc.reply)
			_, err := e.b.CreateShed(ctxT(t), config.CreateShedRequest{Name: "box", NoProvision: true, User: "linus"})
			computeRefusal(t, err, http.StatusServiceUnavailable, config.ErrComputeUnavailable, "")
			e.idle()
			if _, gerr := e.b.GetShed(ctxT(t), "box"); !errors.Is(gerr, config.ErrShedNotFoundSentinel) {
				t.Fatalf("GetShed = %v, want no record", gerr)
			}
			w.Ref.ComputeForceReply("create", nil)
			if l, lerr := w.Client().List(ctxT(t)); lerr != nil || len(l.Instances) != 0 {
				t.Fatalf("machines at the service: %+v (%v), want none", l.Instances, lerr)
			}
		})
	}
}

// The service client says so across the provider boundary: not acted on,
// with the 429's wait.
func TestNotActedCrossesTheProvider(t *testing.T) {
	w := servicetest.Start(t, servicetest.Options{})
	w.Ref.ComputeForceReply("start", &refserver.ComputeReply{Status: http.StatusTooManyRequests, Headers: map[string]string{"Retry-After": "30"}})
	_, err := w.Client().Start(ctxT(t), "6f9619ff-8b86-4d01-b42d-00cf4fc964ff", provider.NewRequestID())
	if !errors.Is(err, provider.ErrNotActed) || !errors.Is(err, provider.ErrUnavailable) || provider.RetryAfterOf(err) != 30*time.Second {
		t.Fatalf("a 429 past the deadline = %v (retry after %s), want unavailable, not acted on, 30s", err, provider.RetryAfterOf(err))
	}
	if !notActed(err) || unknownOutcome(err) {
		t.Fatalf("the backend reads %v as unknown", err)
	}
	w.Ref.ComputeForceReply("start", &refserver.ComputeReply{Status: http.StatusUnauthorized})
	if _, err := w.Client().Start(ctxT(t), "6f9619ff-8b86-4d01-b42d-00cf4fc964ff", provider.NewRequestID()); !errors.Is(err, provider.ErrNotActed) || unknownOutcome(err) {
		t.Fatalf("a second 401 = %v, want not acted on", err)
	}
	if !errors.Is(provider.ErrNotDelivered, provider.ErrNotActed) {
		t.Fatal("never delivered is not a case of not acted on")
	}
}

// retryUnknown honours a Retry-After: a call is not sent again before it,
// and not at all when it runs past the caller's deadline.
func TestRetryUnknownHonoursRetryAfter(t *testing.T) {
	e := newEnv(t)
	limited := func(d time.Duration) error {
		return &provider.Error{Kind: provider.ErrUnavailable, Op: "create", Code: "RATE_LIMITED", Err: provider.ErrNotActed, RetryAfter: d}
	}

	t.Run("past the deadline: not sent again", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
		defer cancel()
		calls := 0
		start := time.Now()
		err := e.b.retryUnknown(ctx, "", func(string) error { calls++; return limited(30 * time.Second) })
		if calls != 1 || !errors.Is(err, provider.ErrNotActed) || time.Since(start) > 150*time.Millisecond {
			t.Fatalf("%d calls in %s: %v; want one, at once", calls, time.Since(start), err)
		}
	})

	t.Run("inside the deadline: sent again after it", func(t *testing.T) {
		var at []time.Time
		err := e.b.retryUnknown(ctxT(t), "", func(string) error {
			at = append(at, time.Now())
			if len(at) == 1 {
				return limited(80 * time.Millisecond)
			}
			return nil
		})
		if err != nil || len(at) != 2 || at[1].Sub(at[0]) < 80*time.Millisecond {
			t.Fatalf("%d calls, %v; want the second at least 80ms after the first", len(at), err)
		}
	})
}

// --- 3: a request id read back from disk is flushed before it is sent ---

// ridSpy records the request ids of the starts and stops sent.
type ridSpy struct {
	*fake.Provider
	mu   sync.Mutex
	rids map[string][]string
}

func (s *ridSpy) note(call, rid string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.rids == nil {
		s.rids = map[string][]string{}
	}
	s.rids[call] = append(s.rids[call], rid)
}

func (s *ridSpy) sent(call string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.rids[call]...)
}

func (s *ridSpy) Start(ctx context.Context, ref provider.Ref, rid string) (provider.Pending, error) {
	s.note(state.JobStart, rid)
	return s.Provider.Start(ctx, ref, rid)
}

func (s *ridSpy) Stop(ctx context.Context, ref provider.Ref, rid string) (provider.Pending, error) {
	s.note(state.JobStop, rid)
	return s.Provider.Stop(ctx, ref, rid)
}

// A start or stop whose request id the record holds from disk (a save whose
// rename landed and whose flush failed, then the process ended) sends
// nothing until a save holding the id is flushed in this process; then it
// sends that id.
func TestLoadedRequestIDsAreFlushedBeforeSending(t *testing.T) {
	const loaded = "6f9619ff-8b86-4d01-b42d-00cf4fc964ff"
	for _, tc := range []struct {
		call   string
		status string
		before func(e *env, name string)
		want   string
	}{
		{state.JobStart, config.StatusStarting, func(e *env, name string) {
			if _, err := e.b.StopShed(ctxT(e.t), name); err != nil {
				e.t.Fatal(err)
			}
			e.settled(name)
		}, config.StatusRunning},
		{state.JobStop, config.StatusStopping, func(*env, string) {}, config.StatusStopped},
	} {
		t.Run(tc.call, func(t *testing.T) {
			spy := &ridSpy{}
			var failing atomic.Bool
			e := newEnv(t, func(e *env, _ *fake.Options) {
				e.wrap = func(p provider.Provider) provider.Provider {
					spy.Provider = p.(*fake.Provider)
					return spy
				}
				// Every backend opened flushes nothing while failing is set.
				e.seams = func(b *Backend) {
					save := b.saveRecord
					b.saveRecord = func(r *state.Record) error {
						if err := save(r); err != nil {
							return err
						}
						if failing.Load() {
							return state.ErrNotDurable
						}
						return nil
					}
				}
			})
			e.running("box")
			tc.before(e, "box")
			if err := e.b.Close(); err != nil {
				t.Fatal(err)
			}
			// What the earlier process left: the call's id in place, not
			// known to be flushed.
			st, err := state.Open(e.dir)
			if err != nil {
				t.Fatal(err)
			}
			recs, _, err := st.Records()
			if err != nil || len(recs) != 1 {
				t.Fatalf("records: %d, %v", len(recs), err)
			}
			r := recs[0]
			r.Shed.Status, r.Shed.StatusReason, r.Job = tc.status, "", tc.call
			r.Pending = nil
			r.Mutation = &state.Mutation{Call: tc.call, Ref: r.Ref, RequestID: loaded}
			if err := st.SaveRecord(r); err != nil {
				t.Fatal(err)
			}
			before := len(spy.sent(tc.call))
			failing.Store(true)
			e.b = e.open()
			e.idle()
			if n := len(spy.sent(tc.call)) - before; n != 0 {
				t.Fatalf("%d %ss sent under an id read back from disk and not flushed", n, tc.call)
			}
			if got := e.status("box"); got != tc.status {
				t.Fatalf("the shed is %s, want %s and kept", got, tc.status)
			}
			failing.Store(false)
			e.sweep()
			if got := e.settled("box"); got.Status != tc.want {
				t.Fatalf("after the id holds: %s (%s), want %s", got.Status, got.StatusReason, tc.want)
			}
			if sent := spy.sent(tc.call)[before:]; len(sent) != 1 || sent[0] != loaded {
				t.Fatalf("%ss sent: %v, want the loaded id once", tc.call, sent)
			}
		})
	}
}

// --- 4: no writer moves the machine's view back ---

// listGate holds a list's answer, taken from the service, until released:
// a list whose reply is delayed past an extend.
type listGate struct {
	provider.Provider
	armed   atomic.Bool
	taken   chan struct{}
	release chan struct{}
}

func (g *listGate) List(ctx context.Context) (provider.Listing, error) {
	l, err := g.Provider.List(ctx)
	if g.armed.CompareAndSwap(true, false) {
		close(g.taken)
		<-g.release
	}
	return l, err
}

// A pass whose list was answered before an extend and arrives after it
// keeps the extended lease: on the record, stored, and as the shed reports
// it.
func TestDelayedListAfterAnExtendKeepsTheLease(t *testing.T) {
	gate := &listGate{taken: make(chan struct{}), release: make(chan struct{})}
	e, w := serviceEnvWith(t, servicetest.Options{}, func(p provider.Provider) provider.Provider {
		gate.Provider = p
		return gate
	})
	e.createFor("widgets", "linus")
	m0, _ := machineOf(t, w, e, "widgets")
	gate.armed.Store(true)
	done := make(chan error, 1)
	go func() { done <- e.b.Sweep(ctxT(t)) }()
	<-gate.taken
	shed, err := extendAs(t, e, backend.UserPrincipal("linus", false), "widgets", 6)
	if err != nil {
		t.Fatal(err)
	}
	want := m0.Lease.ExpiresAt.Add(6 * time.Hour)
	if shed.Cloud == nil || shed.Cloud.Lease == nil || !shed.Cloud.Lease.ExpiresAt.Equal(want) {
		t.Fatalf("the extend's answer: %+v, want a lease ending at %v", shed.Cloud, want)
	}
	close(gate.release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	got, err := e.b.GetShed(ctxT(t), "widgets")
	if err != nil {
		t.Fatal(err)
	}
	if rec := e.storedRecord("widgets"); !got.Cloud.Lease.ExpiresAt.Equal(want) || !rec.Machine.Lease.ExpiresAt.Equal(want) {
		t.Fatalf("after the delayed list the lease ends at %v (stored %v), want %v", got.Cloud.Lease.ExpiresAt, rec.Machine.Lease.ExpiresAt, want)
	}
}

// noteMachineLocked keeps the newer view whatever writer hands it an older
// one.
func TestNoteMachineKeepsTheNewerView(t *testing.T) {
	e := newEnv(t)
	at := time.Date(2026, 11, 26, 9, 0, 0, 0, time.UTC)
	newer := provider.Instance{Ref: "s1", StartedAt: at, Lease: provider.Lease{ExpiresAt: at.Add(30 * time.Hour)}}
	older := provider.Instance{Ref: "s1", StartedAt: at, Lease: provider.Lease{ExpiresAt: at.Add(24 * time.Hour)}}
	r := &state.Record{}
	e.b.noteMachineLocked(r, newer)
	e.b.noteMachineLocked(r, older)
	if !r.Machine.Lease.ExpiresAt.Equal(newer.Lease.ExpiresAt) {
		t.Fatalf("the record holds a lease ending at %v, want the newer %v", r.Machine.Lease.ExpiresAt, newer.Lease.ExpiresAt)
	}
	stopped := newer
	stopped.StoppedAt = at.Add(time.Hour)
	e.b.noteMachineLocked(r, stopped)
	e.b.noteMachineLocked(r, newer) // from before the stop
	if !r.Machine.StoppedAt.Equal(stopped.StoppedAt) {
		t.Fatal("a view from before the stop replaced the stopped one")
	}
}

// --- 5: the start-up pass records the deletions it finds ---

// A machine the service deleted while the server was stopped is dropped by
// the start-up pass, with its service-deletion event in the audit log: the
// log is the backend's from its construction, before the pass starts.
func TestStartUpDeletionsAreAudited(t *testing.T) {
	auditOpt, path := withAudit(t)
	e := newEnv(t, auditOpt)
	e.running("box")
	ref := e.ref("box")
	if err := e.b.Close(); err != nil {
		t.Fatal(err)
	}
	e.prov.DeleteAs(ref, provider.CauseLeaseEnded)
	e.b = e.open()
	e.idle()
	if _, err := e.b.GetShed(ctxT(t), "box"); !errors.Is(err, config.ErrShedNotFoundSentinel) {
		t.Fatalf("GetShed = %v, want dropped", err)
	}
	if !auditHas(t, path, provider.CauseLeaseEnded) {
		t.Fatal("the start-up pass dropped the shed and recorded no event")
	}
}

// --- live run: an ingress the service cannot be asked for is the service
// being unavailable ---

// When the firewall check failed because the service's ingress could not be
// read (the service unreachable or hanging), a create and a start answer 503
// COMPUTE_UNAVAILABLE; a check that found the ingress wrong stays 503
// FIREWALL_CHECK_FAILED.
func TestUnreadableIngressIsComputeUnavailable(t *testing.T) {
	e := newEnv(t)
	e.running("box")
	if _, err := e.b.StopShed(ctxT(t), "box"); err != nil {
		t.Fatal(err)
	}
	e.settled("box")

	e.prov.FailNext(fake.OpIngress, &provider.Error{Kind: provider.ErrUnavailable, Op: "ingress_get", Detail: "no reply arrived whole"})
	if st := e.b.CheckFirewall(ctxT(t)); st.OK || st.Failure == nil || st.Failure.Reason != FirewallUnreadable {
		t.Fatalf("check = %+v, want unreadable", st)
	}
	_, err := e.create("new")
	computeRefusal(t, err, http.StatusServiceUnavailable, config.ErrComputeUnavailable, "")
	if errors.Is(err, config.ErrFirewallCheckFailedSentinel) {
		t.Fatalf("create = %v, also FIREWALL_CHECK_FAILED", err)
	}
	_, err = e.b.StartShed(ctxT(t), "box")
	computeRefusal(t, err, http.StatusServiceUnavailable, config.ErrComputeUnavailable, "")
	if n := e.count(fake.OpCreate); n != 1 {
		t.Fatalf("%d creates sent, want the first only", n)
	}

	// An ingress managed elsewhere, with other sources: static mode
	// compares only, and the check finds them wrong.
	e.prov.SetIngressWritable(false)
	e.prov.SetIngressState([]string{"198.51.100.9/32"}, true)
	if st := e.b.CheckFirewall(ctxT(t)); st.OK || st.Failure.Reason != FirewallSources {
		t.Fatalf("check = %+v, want sources", st)
	}
	if _, err := e.create("new"); !errors.Is(err, config.ErrFirewallCheckFailedSentinel) {
		t.Fatalf("create with the wrong ingress = %v, want FIREWALL_CHECK_FAILED", err)
	}
	if _, err := e.b.StartShed(ctxT(t), "box"); !errors.Is(err, config.ErrFirewallCheckFailedSentinel) {
		t.Fatalf("start with the wrong ingress = %v, want FIREWALL_CHECK_FAILED", err)
	}
}

// An extend the service rate-limits with a Retry-After past the fast-fail
// window (lead ruling): the wait is not taken, the call answers 503
// COMPUTE_UNAVAILABLE inside the window, and nothing was extended.
func TestExtendRateLimitedPastTheWindow(t *testing.T) {
	e, w := serviceEnv(t, servicetest.Options{})
	e.createFor("widgets", "linus")
	m0, _ := machineOf(t, w, e, "widgets")
	w.Ref.ComputeForceReply("extend", &refserver.ComputeReply{Status: http.StatusTooManyRequests, Headers: map[string]string{"Retry-After": "30"}})
	ex, ok := backend.AsExtender(backend.NewGuard(e.b))
	if !ok {
		t.Fatal("the guard hides the remote backend's extend")
	}
	start := time.Now()
	// No deadline of the caller's, as an API request has none: the bound is
	// the backend's own.
	_, err := ex.ExtendShed(backend.WithPrincipal(context.Background(), backend.UserPrincipal("linus", false)), "widgets", 6, "")
	took := time.Since(start)
	w.Ref.ComputeForceReply("extend", nil)
	computeRefusal(t, err, http.StatusServiceUnavailable, config.ErrComputeUnavailable, "")
	if window := 250 * time.Millisecond; took > window+time.Second {
		t.Fatalf("the extend answered after %s; the window is %s", took, window)
	}
	if m, _ := machineOf(t, w, e, "widgets"); !m.Lease.ExpiresAt.Equal(m0.Lease.ExpiresAt) {
		t.Fatalf("the lease moved to %v, want %v", m.Lease.ExpiresAt, m0.Lease.ExpiresAt)
	}
	if rec := e.storedRecord("widgets"); rec.Extend != nil || !rec.Machine.Lease.ExpiresAt.Equal(m0.Lease.ExpiresAt) {
		t.Fatalf("the record: extend %+v, lease %v", rec.Extend, rec.Machine.Lease.ExpiresAt)
	}
}

// --- A5 review ---

// An extend whose every reply was lost keeps its request id on the record.
// The caller sends the same id again and that send is refused before the
// service judged it (a 429 past the window, a second 401): the earlier
// send may have landed, so the answer is 503 and the id stays, until a send
// the service answers settles it, once.
func TestRetriedUnknownExtendStaysUnknown(t *testing.T) {
	const rid = "6f9619ff-8b86-4d01-b42d-00cf4fc964ff"
	linus := backend.UserPrincipal("linus", false)
	for _, tc := range []struct {
		name  string
		reply refserver.ComputeReply
	}{
		{"rate limited", refserver.ComputeReply{Status: http.StatusTooManyRequests, Headers: map[string]string{"Retry-After": "30"}}},
		{"a second 401", refserver.ComputeReply{Status: http.StatusUnauthorized}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, w := serviceEnv(t, servicetest.Options{})
			e.createFor("widgets", "linus")
			m0, _ := machineOf(t, w, e, "widgets")
			want := m0.Lease.ExpiresAt.Add(6 * time.Hour)

			w.LoseReplies(http.MethodPost)
			_, err := extendWithID(t, e, linus, "widgets", 6, rid)
			w.LoseReplies("")
			computeRefusal(t, err, http.StatusServiceUnavailable, config.ErrComputeUnavailable, "")
			// The id the guard derived from the caller's, kept.
			kept := e.storedRecord("widgets").Extend
			if kept == nil || kept.RequestID == "" {
				t.Fatal("after the lost replies the record keeps no extend")
			}
			if m, _ := machineOf(t, w, e, "widgets"); !m.Lease.ExpiresAt.Equal(want) {
				t.Fatalf("the service did not take the extend: lease %v, want %v", m.Lease.ExpiresAt, want)
			}

			w.Ref.ComputeForceReply("extend", &tc.reply)
			_, err = extendWithID(t, e, linus, "widgets", 6, rid)
			w.Ref.ComputeForceReply("extend", nil)
			computeRefusal(t, err, http.StatusServiceUnavailable, config.ErrComputeUnavailable, "")
			if rec := e.storedRecord("widgets"); rec.Extend == nil || rec.Extend.RequestID != kept.RequestID {
				t.Fatalf("a refusal before judgement cleared the unknown extend: %+v, want %s kept", rec.Extend, kept.RequestID)
			}

			shed, err := extendWithID(t, e, linus, "widgets", 6, rid)
			if err != nil {
				t.Fatal(err)
			}
			if shed.Cloud == nil || shed.Cloud.Lease == nil || !shed.Cloud.Lease.ExpiresAt.Equal(want) {
				t.Fatalf("the settled extend: %+v, want the lease once at %v", shed.Cloud, want)
			}
			if rec := e.storedRecord("widgets"); rec.Extend != nil {
				t.Fatalf("the settled extend left %+v on the record", rec.Extend)
			}
		})
	}
}

// The extend's budget covers its wait for another extend of the shed: one
// that waits behind an extend that does not end answers 503
// COMPUTE_UNAVAILABLE inside the window, and one whose caller leaves ends at
// once.
func TestExtendWaitIsInTheBudget(t *testing.T) {
	e, _ := serviceEnv(t, servicetest.Options{})
	e.createFor("widgets", "linus")
	shed, err := e.b.GetShed(ctxT(t), "widgets")
	if err != nil {
		t.Fatal(err)
	}
	release, err := e.b.extending.acquire(ctxT(t), shed.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	ex, _ := backend.AsExtender(backend.NewGuard(e.b))
	linus := backend.UserPrincipal("linus", false)
	call := func(ctx context.Context) (chan error, time.Time) {
		got := make(chan error, 1)
		start := time.Now()
		go func() {
			_, err := ex.ExtendShed(backend.WithPrincipal(ctx, linus), "widgets", 6, "")
			got <- err
		}()
		return got, start
	}

	got, start := call(context.Background())
	select {
	case err := <-got:
		computeRefusal(t, err, http.StatusServiceUnavailable, config.ErrComputeUnavailable, "")
		if took := time.Since(start); took > time.Second {
			t.Fatalf("the extend answered after %s; the window is 250ms", took)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("an extend behind one that does not end still waits after 5 s")
	}

	ctx, cancel := context.WithCancel(context.Background())
	got, _ = call(ctx)
	time.Sleep(20 * time.Millisecond)
	cancel()
	select {
	case err := <-got:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("an extend whose caller left = %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("an extend whose caller left still waits")
	}
}
