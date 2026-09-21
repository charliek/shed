package api

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/charliek/shed/internal/authtoken"
	"github.com/charliek/shed/internal/backend"
	"github.com/charliek/shed/internal/config"
)

// overviewFakeBackend is a Backend stub for the overview + session-listing
// handler tests. Only the methods those paths touch are wired; the rest panic
// so an unexpected call is loud.
type overviewFakeBackend struct {
	sheds    []config.Shed
	sessions map[string][]config.Session // keyed by shed name
	// dfUsage / dfErr back DiskUsage (used by the overview endpoint's df block);
	// zero values give an empty usage with no error.
	dfUsage config.DiskUsage
	dfErr   error
	// listErr injects a per-shed ListSessions failure (keyed by shed name) so the
	// overview session-list-degrade path is testable; nil entries list normally.
	listErr map[string]error
}

func (f *overviewFakeBackend) Type() backend.Type { return backend.TypeVZ }
func (f *overviewFakeBackend) Close() error       { return nil }
func (f *overviewFakeBackend) CreateShed(context.Context, config.CreateShedRequest) (*config.Shed, error) {
	panic("unexpected")
}
func (f *overviewFakeBackend) GetShed(_ context.Context, name string) (*config.Shed, error) {
	return &config.Shed{Name: name, Status: config.StatusRunning}, nil
}
func (f *overviewFakeBackend) ListSheds(context.Context) ([]config.Shed, error) { return f.sheds, nil }
func (f *overviewFakeBackend) DeleteShed(context.Context, string) error         { return nil }
func (f *overviewFakeBackend) StartShed(_ context.Context, name string) (*config.Shed, error) {
	return &config.Shed{Name: name, Status: config.StatusRunning}, nil
}
func (f *overviewFakeBackend) StopShed(_ context.Context, name string) (*config.Shed, error) {
	return &config.Shed{Name: name, Status: config.StatusStopped}, nil
}
func (f *overviewFakeBackend) ResetShed(_ context.Context, name string) (*config.Shed, error) {
	return &config.Shed{Name: name, Status: config.StatusStopped}, nil
}
func (f *overviewFakeBackend) ListSessions(_ context.Context, name string) ([]config.Session, error) {
	if f.listErr != nil {
		if err := f.listErr[name]; err != nil {
			return nil, err
		}
	}
	return f.sessions[name], nil
}
func (f *overviewFakeBackend) KillSession(context.Context, string, string) error { return nil }
func (f *overviewFakeBackend) Exec(context.Context, string, backend.ExecOptions) error {
	panic("unexpected Exec call")
}
func (f *overviewFakeBackend) DialService(context.Context, string, uint16) (net.Conn, error) {
	panic("unexpected DialService call")
}
func (f *overviewFakeBackend) ListImages(context.Context) ([]config.ImageInfo, error) {
	panic("unexpected")
}
func (f *overviewFakeBackend) InspectImage(context.Context, string) (config.ImageInspectResponse, error) {
	panic("unexpected")
}
func (f *overviewFakeBackend) TagImage(context.Context, string, string) error { panic("unexpected") }
func (f *overviewFakeBackend) PullImage(context.Context, string, string, string, bool) (string, error) {
	panic("unexpected")
}
func (f *overviewFakeBackend) PushImage(context.Context, string, string) error { panic("unexpected") }
func (f *overviewFakeBackend) DeleteImage(context.Context, string) error       { panic("unexpected") }
func (f *overviewFakeBackend) PruneImages(context.Context, bool) ([]config.ImageInfo, error) {
	panic("unexpected")
}
func (f *overviewFakeBackend) DiskUsage(context.Context) (config.DiskUsage, error) {
	return f.dfUsage, f.dfErr
}
func (f *overviewFakeBackend) Prune(context.Context, backend.PruneOptions) (config.PruneReport, error) {
	panic("unexpected")
}
func (f *overviewFakeBackend) ListSnapshots(context.Context) ([]config.Snapshot, error) {
	panic("unexpected")
}
func (f *overviewFakeBackend) CreateSnapshot(context.Context, config.SnapshotCreateRequest) (*config.Snapshot, error) {
	panic("unexpected")
}
func (f *overviewFakeBackend) GetSnapshot(context.Context, string) (*config.Snapshot, error) {
	panic("unexpected")
}
func (f *overviewFakeBackend) DeleteSnapshot(context.Context, string) error { panic("unexpected") }

func newOverviewServer(be backend.Backend) *Server {
	return NewServer(be, &config.ServerConfig{Name: "test-server"}, "", nil, nil)
}

// getOverview issues GET path against srv and decodes the 200 response.
func getOverview(t *testing.T, srv *Server, path string) OverviewResponse {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, path, nil)
	w := httptest.NewRecorder()
	srv.Router().ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("GET %s = %d, want 200 (body: %s)", path, w.Code, w.Body.String())
	}
	var resp OverviewResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decoding overview response: %v (body: %s)", err, w.Body.String())
	}
	return resp
}

func findOverviewShed(sheds []OverviewShed, name string) *OverviewShed {
	for i := range sheds {
		if sheds[i].Name == name {
			return &sheds[i]
		}
	}
	return nil
}

func findSession(sessions []config.Session, name string) *config.Session {
	for i := range sessions {
		if sessions[i].Name == name {
			return &sessions[i]
		}
	}
	return nil
}

// sliceHas reports whether any element of ss contains substr. Used for both
// warning assertions and feature-token presence (order-independent).
func sliceHas(ss []string, substr string) bool {
	for _, s := range ss {
		if strings.Contains(s, substr) {
			return true
		}
	}
	return false
}

// TestOverview_HappyPath: a running shed carries its sessions; a stopped shed
// carries an empty sessions slice; the df block and the server feature set are
// present.
func TestOverview_HappyPath(t *testing.T) {
	be := &overviewFakeBackend{
		sheds: []config.Shed{
			{Name: "proj", Status: config.StatusRunning},
			{Name: "asleep", Status: config.StatusStopped},
		},
		sessions: map[string][]config.Session{
			"proj": {{Name: "default", ShedName: "proj"}},
		},
		dfUsage: config.DiskUsage{ServerName: "test-server", Backend: "vz"},
	}
	srv := newOverviewServer(be)

	resp := getOverview(t, srv, "/api/overview")

	// server block
	if resp.Server.Version == "" {
		t.Fatal("server.version empty")
	}
	if !sliceHas(resp.Server.Features, FeatureOverview) {
		t.Fatalf("server.features missing tokens: %v", resp.Server.Features)
	}
	assertNoRetiredRCFeatures(t, "server.features", resp.Server.Features)

	// df block present
	if resp.DF == nil || resp.DF.ServerName != "test-server" {
		t.Fatalf("df block missing/wrong: %+v", resp.DF)
	}

	// running shed: its sessions are listed under it
	proj := findOverviewShed(resp.Sheds, "proj")
	if proj == nil {
		t.Fatal("proj shed absent")
	}
	if findSession(proj.Sessions, "default") == nil {
		t.Fatalf("running shed sessions missing: %+v", proj.Sessions)
	}

	// stopped shed: empty sessions
	asleep := findOverviewShed(resp.Sheds, "asleep")
	if asleep == nil {
		t.Fatal("asleep shed absent")
	}
	if len(asleep.Sessions) != 0 {
		t.Fatalf("stopped shed must have no sessions: %+v", asleep.Sessions)
	}
	if len(resp.Warnings) != 0 {
		t.Fatalf("no warnings expected, got %v", resp.Warnings)
	}
}

// TestOverview_DFFailure_Degrades: a df error omits the df block + adds a warning,
// but the rest of the overview still renders (no 500).
func TestOverview_DFFailure_Degrades(t *testing.T) {
	be := &overviewFakeBackend{
		sheds:    []config.Shed{{Name: "proj", Status: config.StatusRunning}},
		sessions: map[string][]config.Session{"proj": {{Name: "default", ShedName: "proj"}}},
		dfErr:    errors.New("df computation failed"),
	}
	srv := newOverviewServer(be)

	resp := getOverview(t, srv, "/api/overview")
	if resp.DF != nil {
		t.Fatalf("df block must be omitted on failure: %+v", resp.DF)
	}
	if !sliceHas(resp.Warnings, "df unavailable") {
		t.Fatalf("want a df warning, got %v", resp.Warnings)
	}
	if findOverviewShed(resp.Sheds, "proj") == nil {
		t.Fatal("sheds must still render when df fails")
	}
}

// TestOverview_SessionListFailure_Degrades: a shed whose ListSessions fails
// degrades to an empty sessions slice + a warning; sibling sheds are unaffected.
func TestOverview_SessionListFailure_Degrades(t *testing.T) {
	be := &overviewFakeBackend{
		sheds: []config.Shed{
			{Name: "bad", Status: config.StatusRunning},
			{Name: "good", Status: config.StatusRunning},
		},
		sessions: map[string][]config.Session{
			"good": {{Name: "default", ShedName: "good"}},
		},
		listErr: map[string]error{"bad": errors.New("tmux server not responding")},
	}
	srv := newOverviewServer(be)

	resp := getOverview(t, srv, "/api/overview")
	bad := findOverviewShed(resp.Sheds, "bad")
	if bad == nil || len(bad.Sessions) != 0 {
		t.Fatalf("failed shed must degrade to empty sessions: %+v", bad)
	}
	if !sliceHas(resp.Warnings, "shed bad: sessions unavailable") {
		t.Fatalf("want a session-list warning, got %v", resp.Warnings)
	}
	good := findOverviewShed(resp.Sheds, "good")
	if good == nil || findSession(good.Sessions, "default") == nil {
		t.Fatalf("sibling shed must be unaffected: %+v", good)
	}
}

// TestOverview_EmptySlicesNotNull: with no sheds the JSON renders `"sheds":[]`
// (never null); a running shed with no sessions renders `"sessions":[]`; the df
// slices render `[]`.
func TestOverview_EmptySlicesNotNull(t *testing.T) {
	// No sheds at all.
	be := &overviewFakeBackend{dfUsage: config.DiskUsage{ServerName: "test-server"}}
	srv := newOverviewServer(be)
	r := httptest.NewRequest(http.MethodGet, "/api/overview", nil)
	w := httptest.NewRecorder()
	srv.Router().ServeHTTP(w, r)
	body := w.Body.String()
	if !strings.Contains(body, `"sheds":[]`) {
		t.Fatalf(`want "sheds":[] literal, body: %s`, body)
	}
	if strings.Contains(body, `"sheds":null`) {
		t.Fatalf("sheds must never be null, body: %s", body)
	}
	// df slices non-null.
	for _, lit := range []string{`"images":[]`, `"sheds":[]`, `"orphans":[]`} {
		if !strings.Contains(body, lit) {
			t.Fatalf("df block missing %s literal, body: %s", lit, body)
		}
	}

	// A running shed with zero sessions renders "sessions":[].
	be2 := &overviewFakeBackend{
		sheds:    []config.Shed{{Name: "empty", Status: config.StatusRunning}},
		sessions: map[string][]config.Session{}, // no rows for "empty"
	}
	srv2 := newOverviewServer(be2)
	r2 := httptest.NewRequest(http.MethodGet, "/api/overview", nil)
	w2 := httptest.NewRecorder()
	srv2.Router().ServeHTTP(w2, r2)
	if !strings.Contains(w2.Body.String(), `"sessions":[]`) {
		t.Fatalf(`want "sessions":[] for a running shed with no rows, body: %s`, w2.Body.String())
	}
	if strings.Contains(w2.Body.String(), `"sessions":null`) {
		t.Fatalf("sessions must never be null, body: %s", w2.Body.String())
	}
}

// TestOverview_StoppedShedShape: a stopped shed carries an empty sessions slice.
func TestOverview_StoppedShedShape(t *testing.T) {
	be := &overviewFakeBackend{
		sheds: []config.Shed{{Name: "asleep", Status: config.StatusStopped}},
	}
	srv := newOverviewServer(be)
	r := httptest.NewRequest(http.MethodGet, "/api/overview", nil)
	w := httptest.NewRecorder()
	srv.Router().ServeHTTP(w, r)

	var raw struct {
		Sheds []map[string]json.RawMessage `json:"sheds"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &raw); err != nil {
		t.Fatalf("parse overview: %v", err)
	}
	if len(raw.Sheds) != 1 {
		t.Fatalf("want 1 shed, got %d", len(raw.Sheds))
	}
	sess, ok := raw.Sheds[0]["sessions"]
	if !ok || string(sess) != "[]" {
		t.Fatalf(`stopped shed sessions must be [], got: %s`, string(sess))
	}
}

// newTokenModeOverviewServer builds a token-mode server (bearer tokens enforced)
// backed by be, with a control and a credentials token minted for scope
// assertions.
func newTokenModeOverviewServer(t *testing.T, be *overviewFakeBackend) (srv *Server, control, credentials string) {
	t.Helper()
	store := authtoken.NewStore()
	control, _, err := store.Mint("SHA256:test", authtoken.ScopeControl, authtoken.ClientCLI, time.Hour)
	if err != nil {
		t.Fatalf("mint control: %v", err)
	}
	credentials, _, err = store.Mint("SHA256:test", authtoken.ScopeCredentials, authtoken.ClientHostAgent, time.Hour)
	if err != nil {
		t.Fatalf("mint credentials: %v", err)
	}
	srv = NewServer(be, &config.ServerConfig{Name: "test-server", Auth: &config.AuthConfig{Mode: config.AuthModeToken}}, "", nil, nil)
	srv.SetTokenStore(store)
	return srv, control, credentials
}

// TestOverview_Scope: control scope required (the #237/#239 dual-scope carve-outs
// do NOT apply). A credentials token is rejected (403); a control token is
// accepted (200); an unauthenticated request is 401.
func TestOverview_Scope(t *testing.T) {
	be := &overviewFakeBackend{} // empty: handler returns 200 with no sheds
	srv, control, credentials := newTokenModeOverviewServer(t, be)

	call := func(token string) int {
		r := httptest.NewRequest(http.MethodGet, "/api/overview", nil)
		if token != "" {
			r.Header.Set("Authorization", "Bearer "+token)
		}
		w := httptest.NewRecorder()
		srv.Router().ServeHTTP(w, r)
		return w.Code
	}

	if got := call(""); got != http.StatusUnauthorized {
		t.Errorf("no token: got %d, want 401", got)
	}
	if got := call(credentials); got != http.StatusForbidden {
		t.Errorf("credentials token: got %d, want 403", got)
	}
	if got := call(control); got != http.StatusOK {
		t.Errorf("control token: got %d, want 200", got)
	}
}

// TestOverview_MethodGuard: /api/overview is GET-only; a POST is rejected by the
// router (405 Method Not Allowed). Run in open mode so the request reaches the
// router rather than tripping the auth gate first.
func TestOverview_MethodGuard(t *testing.T) {
	be := &overviewFakeBackend{}
	srv := newOverviewServer(be)
	r := httptest.NewRequest(http.MethodPost, "/api/overview", nil)
	w := httptest.NewRecorder()
	srv.Router().ServeHTTP(w, r)
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST /api/overview = %d, want 405", w.Code)
	}
}

// TestInfo_Features: GET /api/info advertises the feature-token set for endpoint
// discovery (the same set mirrored in the overview server block).
func TestInfo_Features(t *testing.T) {
	srv := NewServer(nil, &config.ServerConfig{Name: "test-server"}, "", nil, nil)
	r := httptest.NewRequest(http.MethodGet, "/api/info", nil)
	w := httptest.NewRecorder()
	srv.Router().ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("GET /api/info = %d, want 200", w.Code)
	}
	var info config.ServerInfo
	if err := json.Unmarshal(w.Body.Bytes(), &info); err != nil {
		t.Fatalf("decode ServerInfo: %v", err)
	}
	if !sliceHas(info.Features, FeatureOverview) {
		t.Fatalf("/api/info features missing tokens: %v", info.Features)
	}
	assertNoRetiredRCFeatures(t, "/api/info features", info.Features)
}

// TestSessions_ListShed: GET /api/sheds/{name}/sessions returns the backend's
// rows unchanged (no enrichment layer any more).
func TestSessions_ListShed(t *testing.T) {
	be := &overviewFakeBackend{
		sheds:    []config.Shed{{Name: "proj", Status: config.StatusRunning}},
		sessions: map[string][]config.Session{"proj": {{Name: "default", ShedName: "proj"}}},
	}
	srv := newOverviewServer(be)

	r := httptest.NewRequest(http.MethodGet, "/api/sheds/proj/sessions", nil)
	w := httptest.NewRecorder()
	srv.Router().ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("GET sessions = %d, want 200 (body: %s)", w.Code, w.Body.String())
	}
	var resp config.SessionsResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode sessions: %v", err)
	}
	if findSession(resp.Sessions, "default") == nil {
		t.Fatalf("session row missing: %+v", resp.Sessions)
	}
	if len(resp.Warnings) != 0 {
		t.Fatalf("no warnings expected, got %v", resp.Warnings)
	}
}

// TestSessions_ListAll: GET /api/sessions flattens every running shed's rows and
// skips stopped sheds.
func TestSessions_ListAll(t *testing.T) {
	be := &overviewFakeBackend{
		sheds: []config.Shed{
			{Name: "proj", Status: config.StatusRunning},
			{Name: "asleep", Status: config.StatusStopped},
		},
		sessions: map[string][]config.Session{
			"proj":   {{Name: "default", ShedName: "proj"}},
			"asleep": {{Name: "ghost", ShedName: "asleep"}},
		},
	}
	srv := newOverviewServer(be)

	r := httptest.NewRequest(http.MethodGet, "/api/sessions", nil)
	w := httptest.NewRecorder()
	srv.Router().ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("GET /api/sessions = %d, want 200 (body: %s)", w.Code, w.Body.String())
	}
	var resp config.SessionsResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode sessions: %v", err)
	}
	if findSession(resp.Sessions, "default") == nil {
		t.Fatalf("running shed row missing: %+v", resp.Sessions)
	}
	if findSession(resp.Sessions, "ghost") != nil {
		t.Fatalf("stopped shed rows must not be listed: %+v", resp.Sessions)
	}
}

// retiredRCFeatureTokens are the feature tokens the S6 RC-hub demolition
// removed (plan 022 C4). The server can no longer serve any of the behaviour
// they advertised, so re-adding one would send clients probing deleted
// routes. Asserting only that "overview" is PRESENT would not catch that —
// hence the absence check below.
var retiredRCFeatureTokens = []string{"rc-enrich", "rc-events", "rc-proxy"}

// assertNoRetiredRCFeatures fails if any retired RC token reappears in a
// feature list.
func assertNoRetiredRCFeatures(t *testing.T, where string, features []string) {
	t.Helper()
	for _, tok := range retiredRCFeatureTokens {
		if sliceHas(features, tok) {
			t.Errorf("%s re-advertises retired RC feature %q: %v", where, tok, features)
		}
	}
}
