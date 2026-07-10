package api

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/charliek/shed/internal/backend"
	"github.com/charliek/shed/internal/config"
	"github.com/charliek/shed/internal/ext/rc"
)

// rcFakeBackend is a Backend stub for the rc-enrichment handler tests. Only the
// methods the session-listing and lifecycle paths touch are wired; the rest panic
// so an unexpected call is loud. Exec runs execFn (recording a call) so a test can
// serve a golden `shed-ext-rc list` envelope, an error, or a slow/blocking exec.
type rcFakeBackend struct {
	sheds     []config.Shed
	sessions  map[string][]config.Session // keyed by shed name
	execFn    func(ctx context.Context, shed string, opts backend.ExecOptions) error
	execCalls atomic.Int32
}

func (f *rcFakeBackend) Type() backend.Type { return backend.TypeVZ }
func (f *rcFakeBackend) Close() error       { return nil }
func (f *rcFakeBackend) CreateShed(context.Context, config.CreateShedRequest) (*config.Shed, error) {
	panic("unexpected")
}
func (f *rcFakeBackend) GetShed(_ context.Context, name string) (*config.Shed, error) {
	return &config.Shed{Name: name, Status: config.StatusRunning}, nil
}
func (f *rcFakeBackend) ListSheds(context.Context) ([]config.Shed, error) { return f.sheds, nil }
func (f *rcFakeBackend) DeleteShed(context.Context, string) error         { return nil }
func (f *rcFakeBackend) StartShed(_ context.Context, name string) (*config.Shed, error) {
	return &config.Shed{Name: name, Status: config.StatusRunning}, nil
}
func (f *rcFakeBackend) StopShed(_ context.Context, name string) (*config.Shed, error) {
	return &config.Shed{Name: name, Status: config.StatusStopped}, nil
}
func (f *rcFakeBackend) ResetShed(_ context.Context, name string) (*config.Shed, error) {
	return &config.Shed{Name: name, Status: config.StatusStopped}, nil
}
func (f *rcFakeBackend) ListSessions(_ context.Context, name string) ([]config.Session, error) {
	return f.sessions[name], nil
}
func (f *rcFakeBackend) KillSession(context.Context, string, string) error { return nil }
func (f *rcFakeBackend) Exec(ctx context.Context, shed string, opts backend.ExecOptions) error {
	f.execCalls.Add(1)
	if f.execFn != nil {
		return f.execFn(ctx, shed, opts)
	}
	panic("unexpected Exec call")
}
func (f *rcFakeBackend) DialService(context.Context, string, uint16) (net.Conn, error) {
	panic("unexpected")
}
func (f *rcFakeBackend) ListImages(context.Context) ([]config.ImageInfo, error) { panic("unexpected") }
func (f *rcFakeBackend) InspectImage(context.Context, string) (config.ImageInspectResponse, error) {
	panic("unexpected")
}
func (f *rcFakeBackend) TagImage(context.Context, string, string) error { panic("unexpected") }
func (f *rcFakeBackend) PullImage(context.Context, string, string, string, bool) (string, error) {
	panic("unexpected")
}
func (f *rcFakeBackend) PushImage(context.Context, string, string) error { panic("unexpected") }
func (f *rcFakeBackend) DeleteImage(context.Context, string) error       { panic("unexpected") }
func (f *rcFakeBackend) PruneImages(context.Context, bool) ([]config.ImageInfo, error) {
	panic("unexpected")
}
func (f *rcFakeBackend) DiskUsage(context.Context) (config.DiskUsage, error) { panic("unexpected") }
func (f *rcFakeBackend) Prune(context.Context, backend.PruneOptions) (config.PruneReport, error) {
	panic("unexpected")
}
func (f *rcFakeBackend) ListSnapshots(context.Context) ([]config.Snapshot, error) {
	panic("unexpected")
}
func (f *rcFakeBackend) CreateSnapshot(context.Context, config.SnapshotCreateRequest) (*config.Snapshot, error) {
	panic("unexpected")
}
func (f *rcFakeBackend) GetSnapshot(context.Context, string) (*config.Snapshot, error) {
	panic("unexpected")
}
func (f *rcFakeBackend) DeleteSnapshot(context.Context, string) error { panic("unexpected") }

// execServes returns an execFn that writes payload to opts.Stdout and exits 0.
func execServes(payload string) func(context.Context, string, backend.ExecOptions) error {
	return func(_ context.Context, _ string, opts backend.ExecOptions) error {
		_, err := opts.Stdout.Write([]byte(payload))
		return err
	}
}

// Golden `shed-ext-rc list` envelopes.
const (
	newEnvelope = `{"rc_sessions":[` +
		`{"slug":"abc234","tmux_session":"rc-abc234","kind":"claude-rc","state":"ready","managed":true,"display_name":"proj/abc234","url":"https://claude.ai/session_x","created_by":"shed/1"}` +
		`],"capabilities":{"rc_version":3,"kinds":["shell","claude-rc"],"agents":{"claude":{"installed":true,"version":"2.1.0"}},"features":["generic-perm"],"kind_features":{}}}`

	oldBareEnvelope = `{"rc_sessions":[` +
		`{"slug":"abc234","tmux_session":"rc-abc234","kind":"codex","state":"needs-auth","managed":true}` +
		`]}`
)

func newRCServer(be backend.Backend) *Server {
	return NewServer(be, &config.ServerConfig{Name: "test-server"}, "", nil, nil)
}

func getSessions(t *testing.T, srv *Server, path string) config.SessionsResponse {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, path, nil)
	w := httptest.NewRecorder()
	srv.Router().ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("GET %s = %d, want 200 (body: %s)", path, w.Code, w.Body.String())
	}
	var resp config.SessionsResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decoding sessions response: %v (body: %s)", err, w.Body.String())
	}
	return resp
}

func findSession(sessions []config.Session, name string) *config.Session {
	for i := range sessions {
		if sessions[i].Name == name {
			return &sessions[i]
		}
	}
	return nil
}

// TestEnrich_NewEnvelope_PopulatesRCAndCachesCaps: an rc-* row is enriched from a
// modern envelope AND the capabilities block is cached.
func TestEnrich_NewEnvelope_PopulatesRCAndCachesCaps(t *testing.T) {
	be := &rcFakeBackend{
		sessions: map[string][]config.Session{
			"proj": {{Name: "rc-abc234", ShedName: "proj"}, {Name: "default", ShedName: "proj"}},
		},
		execFn: execServes(newEnvelope),
	}
	srv := newRCServer(be)

	resp := getSessions(t, srv, "/api/sheds/proj/sessions")
	rcRow := findSession(resp.Sessions, "rc-abc234")
	if rcRow == nil || rcRow.RC == nil {
		t.Fatalf("rc-abc234 not enriched: %+v", resp.Sessions)
	}
	if rcRow.RC.Kind != "claude-rc" || rcRow.RC.State != "ready" || !rcRow.RC.Managed ||
		rcRow.RC.DisplayName != "proj/abc234" || rcRow.RC.URL != "https://claude.ai/session_x" {
		t.Fatalf("rc metadata wrong: %+v", rcRow.RC)
	}
	if plain := findSession(resp.Sessions, "default"); plain == nil || plain.RC != nil {
		t.Fatalf("plain session must not be enriched: %+v", plain)
	}
	if len(resp.Warnings) != 0 {
		t.Fatalf("no warnings expected, got %v", resp.Warnings)
	}
	// Capabilities were cached from the same exec.
	if caps, ok := srv.rcCaps.get("proj"); !ok || caps == nil || caps.RCVersion != 3 {
		t.Fatalf("capabilities not cached: ok=%v caps=%+v", ok, caps)
	}
	if got := be.execCalls.Load(); got != 1 {
		t.Fatalf("exec called %d times, want exactly 1", got)
	}
}

// TestEnrich_OldBareEnvelope enriches from a pre-capabilities binary's bare
// envelope; caps stay uncached (nothing to cache).
func TestEnrich_OldBareEnvelope(t *testing.T) {
	be := &rcFakeBackend{
		sessions: map[string][]config.Session{
			"proj": {{Name: "rc-abc234", ShedName: "proj"}},
		},
		execFn: execServes(oldBareEnvelope),
	}
	srv := newRCServer(be)

	resp := getSessions(t, srv, "/api/sheds/proj/sessions")
	rcRow := findSession(resp.Sessions, "rc-abc234")
	if rcRow == nil || rcRow.RC == nil || rcRow.RC.Kind != "codex" || rcRow.RC.State != "needs-auth" {
		t.Fatalf("old-envelope enrichment wrong: %+v", rcRow)
	}
	if _, ok := srv.rcCaps.get("proj"); ok {
		t.Fatalf("bare envelope must not populate the capabilities cache")
	}
}

// TestEnrich_ExecError_DegradesWithWarning: a failing exec leaves rows un-enriched
// and appends a warning rather than 500-ing.
func TestEnrich_ExecError_DegradesWithWarning(t *testing.T) {
	be := &rcFakeBackend{
		sessions: map[string][]config.Session{
			"proj": {{Name: "rc-abc234", ShedName: "proj"}},
		},
		execFn: func(context.Context, string, backend.ExecOptions) error {
			return errors.New("exit status 127: shed-ext-rc: command not found")
		},
	}
	srv := newRCServer(be)

	resp := getSessions(t, srv, "/api/sheds/proj/sessions")
	if rcRow := findSession(resp.Sessions, "rc-abc234"); rcRow == nil || rcRow.RC != nil {
		t.Fatalf("degraded row must stay un-enriched: %+v", rcRow)
	}
	if len(resp.Warnings) != 1 {
		t.Fatalf("want 1 warning, got %v", resp.Warnings)
	}
}

// TestEnrich_Timeout_Degrades: an exec that outruns rcEnrichTimeout degrades the
// shed to un-enriched + a warning (does not hang the request).
func TestEnrich_Timeout_Degrades(t *testing.T) {
	be := &rcFakeBackend{
		sessions: map[string][]config.Session{
			"proj": {{Name: "rc-abc234", ShedName: "proj"}},
		},
		execFn: func(ctx context.Context, _ string, _ backend.ExecOptions) error {
			<-ctx.Done() // block until the per-exec timeout fires
			return ctx.Err()
		},
	}
	srv := newRCServer(be)
	// execRCList wraps each exec in context.WithTimeout(rcEnrichTimeout), so the
	// blocking exec unblocks (via ctx.Done) at the deadline and the shed degrades.
	done := make(chan config.SessionsResponse, 1)
	go func() { done <- getSessions(t, srv, "/api/sheds/proj/sessions") }()
	select {
	case resp := <-done:
		if rcRow := findSession(resp.Sessions, "rc-abc234"); rcRow == nil || rcRow.RC != nil {
			t.Fatalf("timed-out shed must stay un-enriched: %+v", rcRow)
		}
		if len(resp.Warnings) != 1 {
			t.Fatalf("want 1 timeout warning, got %v", resp.Warnings)
		}
	case <-time.After(rcEnrichTimeout + 3*time.Second):
		t.Fatal("handler did not return within the enrichment timeout budget")
	}
}

// TestEnrich_NoRCRows_ZeroExec: a listing with no rc-* rows must not exec anything.
func TestEnrich_NoRCRows_ZeroExec(t *testing.T) {
	be := &rcFakeBackend{
		sessions: map[string][]config.Session{
			"proj": {{Name: "default", ShedName: "proj"}, {Name: "work", ShedName: "proj"}},
		},
		// execFn nil -> Exec panics if ever called.
	}
	srv := newRCServer(be)

	resp := getSessions(t, srv, "/api/sheds/proj/sessions")
	for _, s := range resp.Sessions {
		if s.RC != nil {
			t.Fatalf("no session should be enriched: %+v", s)
		}
	}
	if got := be.execCalls.Load(); got != 0 {
		t.Fatalf("exec called %d times on a no-rc listing, want 0", got)
	}
}

// TestEnrich_RC0_SkipsEnrichment: ?rc=0 opts out entirely, even with rc-* rows.
func TestEnrich_RC0_SkipsEnrichment(t *testing.T) {
	be := &rcFakeBackend{
		sessions: map[string][]config.Session{
			"proj": {{Name: "rc-abc234", ShedName: "proj"}},
		},
		// execFn nil -> Exec panics if enrichment runs.
	}
	srv := newRCServer(be)

	resp := getSessions(t, srv, "/api/sheds/proj/sessions?rc=0")
	if rcRow := findSession(resp.Sessions, "rc-abc234"); rcRow == nil || rcRow.RC != nil {
		t.Fatalf("?rc=0 must skip enrichment: %+v", rcRow)
	}
	if got := be.execCalls.Load(); got != 0 {
		t.Fatalf("?rc=0 must issue 0 execs, got %d", got)
	}
}

// TestEnrich_AllSessions_AcrossSheds: GET /api/sessions enriches rc-* rows across
// multiple running sheds and skips stopped ones.
func TestEnrich_AllSessions_AcrossSheds(t *testing.T) {
	be := &rcFakeBackend{
		sheds: []config.Shed{
			{Name: "proj", Status: config.StatusRunning},
			{Name: "other", Status: config.StatusRunning},
			{Name: "asleep", Status: config.StatusStopped},
		},
		sessions: map[string][]config.Session{
			"proj":  {{Name: "rc-abc234", ShedName: "proj"}},
			"other": {{Name: "default", ShedName: "other"}},
		},
		execFn: execServes(newEnvelope),
	}
	srv := newRCServer(be)

	resp := getSessions(t, srv, "/api/sessions")
	if rcRow := findSession(resp.Sessions, "rc-abc234"); rcRow == nil || rcRow.RC == nil {
		t.Fatalf("rc row across sheds not enriched: %+v", rcRow)
	}
	// Only the shed with an rc-* row execs.
	if got := be.execCalls.Load(); got != 1 {
		t.Fatalf("exec called %d times, want exactly 1 (only the rc-bearing shed)", got)
	}
}

// TestRCCaps_TTLExpiry re-probes once the cached entry ages past rcCapsTTL.
func TestRCCaps_TTLExpiry(t *testing.T) {
	be := &rcFakeBackend{execFn: execServes(newEnvelope)}
	srv := newRCServer(be)
	clock := time.Now()
	srv.rcCaps.now = func() time.Time { return clock }

	if _, err := srv.RCCapabilities(context.Background(), "proj", false); err != nil {
		t.Fatalf("first probe: %v", err)
	}
	if got := be.execCalls.Load(); got != 1 {
		t.Fatalf("first probe should exec once, got %d", got)
	}
	// Within TTL: served from cache, no new exec.
	clock = clock.Add(rcCapsTTL - time.Second)
	if _, err := srv.RCCapabilities(context.Background(), "proj", false); err != nil {
		t.Fatal(err)
	}
	if got := be.execCalls.Load(); got != 1 {
		t.Fatalf("within TTL should not re-exec, got %d execs", got)
	}
	// Past TTL: re-probe.
	clock = clock.Add(2 * time.Second)
	if _, err := srv.RCCapabilities(context.Background(), "proj", false); err != nil {
		t.Fatal(err)
	}
	if got := be.execCalls.Load(); got != 2 {
		t.Fatalf("past TTL should re-exec, got %d execs", got)
	}
}

// TestRCCaps_Fresh forces a re-probe even within TTL.
func TestRCCaps_Fresh(t *testing.T) {
	be := &rcFakeBackend{execFn: execServes(newEnvelope)}
	srv := newRCServer(be)

	if _, err := srv.RCCapabilities(context.Background(), "proj", false); err != nil {
		t.Fatal(err)
	}
	if _, err := srv.RCCapabilities(context.Background(), "proj", true); err != nil {
		t.Fatal(err)
	}
	if got := be.execCalls.Load(); got != 2 {
		t.Fatalf("fresh=1 must re-exec: got %d execs, want 2", got)
	}
}

// TestRCCaps_InvalidateOnLifecycle: stop/start/reset/delete drop the cached caps.
func TestRCCaps_InvalidateOnLifecycle(t *testing.T) {
	for _, tc := range []struct {
		name   string
		method string
		path   string
	}{
		{"stop", http.MethodPost, "/api/sheds/proj/stop"},
		{"start", http.MethodPost, "/api/sheds/proj/start"},
		{"reset", http.MethodPost, "/api/sheds/proj/reset"},
		{"delete", http.MethodDelete, "/api/sheds/proj"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			be := &rcFakeBackend{execFn: execServes(newEnvelope)}
			srv := newRCServer(be)
			// Seed the cache.
			if _, err := srv.RCCapabilities(context.Background(), "proj", false); err != nil {
				t.Fatal(err)
			}
			if _, ok := srv.rcCaps.get("proj"); !ok {
				t.Fatal("precondition: caps should be cached")
			}
			r := httptest.NewRequest(tc.method, tc.path, nil)
			w := httptest.NewRecorder()
			srv.Router().ServeHTTP(w, r)
			if w.Code != http.StatusOK && w.Code != http.StatusNoContent {
				t.Fatalf("%s %s = %d", tc.method, tc.path, w.Code)
			}
			if _, ok := srv.rcCaps.get("proj"); ok {
				t.Fatalf("%s must invalidate the cached caps", tc.name)
			}
		})
	}
}

// TestRCCaps_StaleGenerationPutDropped pins the invalidate-vs-in-flight-put
// interleaving at the cache level: a put whose generation was snapshotted before
// an invalidate must be dropped; a put at the current generation must land.
func TestRCCaps_StaleGenerationPutDropped(t *testing.T) {
	c := newRCCapsCache()
	caps := &rc.Capabilities{RCVersion: 3}

	// Listing starts (snapshots gen) → lifecycle invalidation → old put arrives.
	gen := c.generation("proj")
	c.invalidate("proj")
	c.put("proj", gen, caps)
	if _, ok := c.get("proj"); ok {
		t.Fatal("stale-generation put must be dropped (invalidate raced the probe)")
	}

	// A probe started after the invalidation lands normally.
	gen2 := c.generation("proj")
	c.put("proj", gen2, caps)
	if got, ok := c.get("proj"); !ok || got.RCVersion != 3 {
		t.Fatalf("current-generation put must land: ok=%v caps=%+v", ok, got)
	}
}

// TestEnrich_InvalidationDuringExecDropsStalePut drives the same interleaving
// through the handler: a lifecycle invalidation lands while the enrichment exec
// is in flight, so the exec's capabilities put must be dropped — the rows still
// enrich (session data is fresh), but the pre-transition caps never resurrect.
func TestEnrich_InvalidationDuringExecDropsStalePut(t *testing.T) {
	be := &rcFakeBackend{
		sessions: map[string][]config.Session{
			"proj": {{Name: "rc-abc234", ShedName: "proj"}},
		},
	}
	srv := newRCServer(be)
	be.execFn = func(_ context.Context, _ string, opts backend.ExecOptions) error {
		// The generation was snapshotted before this exec started; a lifecycle
		// transition (stop/start/delete/reset) invalidates mid-flight.
		srv.rcCaps.invalidate("proj")
		_, err := opts.Stdout.Write([]byte(newEnvelope))
		return err
	}

	resp := getSessions(t, srv, "/api/sheds/proj/sessions")
	if rcRow := findSession(resp.Sessions, "rc-abc234"); rcRow == nil || rcRow.RC == nil {
		t.Fatalf("rows should still enrich from the fresh exec: %+v", rcRow)
	}
	if _, ok := srv.rcCaps.get("proj"); ok {
		t.Fatal("in-flight put must lose to the invalidation (stale caps resurrected)")
	}
}

// TestEnrich_ConcurrentRequests exercises the enrichment + cache under parallel
// load; run with -race to catch data races on the shared cache and session slices.
func TestEnrich_ConcurrentRequests(t *testing.T) {
	be := &rcFakeBackend{
		sheds: []config.Shed{
			{Name: "proj", Status: config.StatusRunning},
			{Name: "other", Status: config.StatusRunning},
		},
		sessions: map[string][]config.Session{
			"proj":  {{Name: "rc-abc234", ShedName: "proj"}, {Name: "default", ShedName: "proj"}},
			"other": {{Name: "rc-def567", ShedName: "other"}},
		},
		execFn: execServes(newEnvelope),
	}
	srv := newRCServer(be)

	var wg sync.WaitGroup
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := httptest.NewRequest(http.MethodGet, "/api/sessions", nil)
			w := httptest.NewRecorder()
			srv.Router().ServeHTTP(w, r)
			if w.Code != http.StatusOK {
				t.Errorf("status = %d", w.Code)
			}
		}()
	}
	wg.Wait()
}
