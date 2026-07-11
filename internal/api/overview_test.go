package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/charliek/shed/internal/authtoken"
	"github.com/charliek/shed/internal/backend"
	"github.com/charliek/shed/internal/config"
)

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

// TestOverview_HappyPath: a running rc-bearing shed carries enriched sessions +
// capabilities; a stopped shed carries an empty sessions slice and no caps; the
// df block and the server feature set are present.
func TestOverview_HappyPath(t *testing.T) {
	be := &rcFakeBackend{
		sheds: []config.Shed{
			{Name: "proj", Status: config.StatusRunning},
			{Name: "asleep", Status: config.StatusStopped},
		},
		sessions: map[string][]config.Session{
			"proj": {{Name: "rc-abc234", ShedName: "proj"}, {Name: "default", ShedName: "proj"}},
		},
		dfUsage: config.DiskUsage{ServerName: "test-server", Backend: "vz"},
		execFn:  execServes(newEnvelope),
	}
	srv := newRCServer(be)

	resp := getOverview(t, srv, "/api/overview")

	// server block
	if resp.Server.Version == "" {
		t.Fatal("server.version empty")
	}
	if !sliceHas(resp.Server.Features, FeatureOverview) ||
		!sliceHas(resp.Server.Features, FeatureRCEnrich) {
		t.Fatalf("server.features missing tokens: %v", resp.Server.Features)
	}

	// df block present
	if resp.DF == nil || resp.DF.ServerName != "test-server" {
		t.Fatalf("df block missing/wrong: %+v", resp.DF)
	}

	// running shed: enriched rc session + caps
	proj := findOverviewShed(resp.Sheds, "proj")
	if proj == nil {
		t.Fatal("proj shed absent")
	}
	rcRow := findSession(proj.Sessions, "rc-abc234")
	if rcRow == nil || rcRow.RC == nil || rcRow.RC.Kind != "claude-rc" || rcRow.RC.State != "ready" {
		t.Fatalf("rc row not enriched under its shed: %+v", rcRow)
	}
	if proj.RCCapabilities == nil || proj.RCCapabilities.RCVersion != 3 {
		t.Fatalf("running shed caps missing: %+v", proj.RCCapabilities)
	}

	// stopped shed: empty sessions, no caps
	asleep := findOverviewShed(resp.Sheds, "asleep")
	if asleep == nil {
		t.Fatal("asleep shed absent")
	}
	if len(asleep.Sessions) != 0 {
		t.Fatalf("stopped shed must have no sessions: %+v", asleep.Sessions)
	}
	if asleep.RCCapabilities != nil {
		t.Fatalf("stopped shed must omit caps: %+v", asleep.RCCapabilities)
	}
	if len(resp.Warnings) != 0 {
		t.Fatalf("no warnings expected, got %v", resp.Warnings)
	}
	// Only the rc-bearing running shed execs; caps served from the enrichment cache.
	if got := be.execCalls.Load(); got != 1 {
		t.Fatalf("exec called %d times, want exactly 1", got)
	}
}

// TestOverview_DFFailure_Degrades: a df error omits the df block + adds a warning,
// but the rest of the overview still renders (no 500).
func TestOverview_DFFailure_Degrades(t *testing.T) {
	be := &rcFakeBackend{
		sheds:    []config.Shed{{Name: "proj", Status: config.StatusRunning}},
		sessions: map[string][]config.Session{"proj": {{Name: "default", ShedName: "proj"}}},
		dfErr:    errors.New("df computation failed"),
		// A running shed still gets a capabilities probe (create-chip discovery).
		execFn: execServes(newEnvelope),
	}
	srv := newRCServer(be)

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
	be := &rcFakeBackend{
		sheds: []config.Shed{
			{Name: "bad", Status: config.StatusRunning},
			{Name: "good", Status: config.StatusRunning},
		},
		sessions: map[string][]config.Session{
			"good": {{Name: "default", ShedName: "good"}},
		},
		listErr: map[string]error{"bad": errors.New("tmux server not responding")},
		// Running sheds are still capability-probed (independent of ListSessions).
		execFn: execServes(newEnvelope),
	}
	srv := newRCServer(be)

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

// TestOverview_EnrichmentFailure_Degrades: an rc exec error leaves rc-* rows
// un-enriched + a warning; the call still succeeds.
func TestOverview_EnrichmentFailure_Degrades(t *testing.T) {
	be := &rcFakeBackend{
		sheds:    []config.Shed{{Name: "proj", Status: config.StatusRunning}},
		sessions: map[string][]config.Session{"proj": {{Name: "rc-abc234", ShedName: "proj"}}},
		execFn: func(context.Context, string, backend.ExecOptions) error {
			return errors.New("exit status 127: shed-ext-rc: command not found")
		},
	}
	srv := newRCServer(be)

	resp := getOverview(t, srv, "/api/overview")
	proj := findOverviewShed(resp.Sheds, "proj")
	if proj == nil {
		t.Fatal("proj shed absent")
	}
	if rcRow := findSession(proj.Sessions, "rc-abc234"); rcRow == nil || rcRow.RC != nil {
		t.Fatalf("degraded rc row must stay un-enriched: %+v", rcRow)
	}
	if !sliceHas(resp.Warnings, "RC metadata unavailable") &&
		!sliceHas(resp.Warnings, "RC capabilities unavailable") {
		t.Fatalf("want an rc-degrade warning, got %v", resp.Warnings)
	}
}

// TestOverview_EmptySlicesNotNull: with no sheds the JSON renders `"sheds":[]`
// (never null); a running shed with no sessions renders `"sessions":[]`; the df
// slices render `[]`.
func TestOverview_EmptySlicesNotNull(t *testing.T) {
	// No sheds at all.
	be := &rcFakeBackend{dfUsage: config.DiskUsage{ServerName: "test-server"}}
	srv := newRCServer(be)
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
	be2 := &rcFakeBackend{
		sheds:    []config.Shed{{Name: "empty", Status: config.StatusRunning}},
		sessions: map[string][]config.Session{}, // no rows for "empty"
		execFn:   execServes(newEnvelope),       // running shed still capability-probed
	}
	srv2 := newRCServer(be2)
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

// TestOverview_StoppedShedShape: a stopped shed omits the rc_capabilities key
// entirely and carries an empty sessions slice.
func TestOverview_StoppedShedShape(t *testing.T) {
	be := &rcFakeBackend{
		sheds: []config.Shed{{Name: "asleep", Status: config.StatusStopped}},
	}
	srv := newRCServer(be)
	r := httptest.NewRequest(http.MethodGet, "/api/overview", nil)
	w := httptest.NewRecorder()
	srv.Router().ServeHTTP(w, r)

	// Raw-JSON key presence: rc_capabilities must be absent on a stopped shed.
	var raw struct {
		Sheds []map[string]json.RawMessage `json:"sheds"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &raw); err != nil {
		t.Fatalf("parse overview: %v", err)
	}
	if len(raw.Sheds) != 1 {
		t.Fatalf("want 1 shed, got %d", len(raw.Sheds))
	}
	if _, present := raw.Sheds[0]["rc_capabilities"]; present {
		t.Fatalf("stopped shed must omit rc_capabilities, got: %v", raw.Sheds[0])
	}
	sess, ok := raw.Sheds[0]["sessions"]
	if !ok || string(sess) != "[]" {
		t.Fatalf(`stopped shed sessions must be [], got: %s`, string(sess))
	}
}

// TestOverview_RC0_SkipsEnrichment: ?rc=0 issues zero guest execs — sessions are
// present but un-enriched, and no capabilities are probed.
func TestOverview_RC0_SkipsEnrichment(t *testing.T) {
	be := &rcFakeBackend{
		sheds:    []config.Shed{{Name: "proj", Status: config.StatusRunning}},
		sessions: map[string][]config.Session{"proj": {{Name: "rc-abc234", ShedName: "proj"}}},
		// execFn nil -> Exec panics if any enrichment/capability exec runs.
	}
	srv := newRCServer(be)

	resp := getOverview(t, srv, "/api/overview?rc=0")
	proj := findOverviewShed(resp.Sheds, "proj")
	if proj == nil {
		t.Fatal("proj shed absent")
	}
	if rcRow := findSession(proj.Sessions, "rc-abc234"); rcRow == nil || rcRow.RC != nil {
		t.Fatalf("?rc=0 must not enrich: %+v", rcRow)
	}
	if proj.RCCapabilities != nil {
		t.Fatalf("?rc=0 must not probe caps: %+v", proj.RCCapabilities)
	}
	if got := be.execCalls.Load(); got != 0 {
		t.Fatalf("?rc=0 must issue 0 execs, got %d", got)
	}
}

// newSecureRCServer builds a secure-mode server (bearer tokens enforced) backed
// by be, with a control and a credentials token minted for scope assertions.
func newSecureRCServer(t *testing.T, be *rcFakeBackend) (srv *Server, control, credentials string) {
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
	srv = NewServer(be, &config.ServerConfig{Name: "test-server", Auth: &config.AuthConfig{Mode: config.AuthModeSecure}}, "", nil, nil)
	srv.SetTokenStore(store)
	return srv, control, credentials
}

// TestOverview_Scope: control scope required (the #237/#239 dual-scope carve-outs
// do NOT apply). A credentials token is rejected (403); a control token is
// accepted (200); an unauthenticated request is 401.
func TestOverview_Scope(t *testing.T) {
	be := &rcFakeBackend{} // empty: handler returns 200 with no sheds
	srv, control, credentials := newSecureRCServer(t, be)

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
	be := &rcFakeBackend{}
	srv := newRCServer(be)
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
	if !sliceHas(info.Features, FeatureOverview) ||
		!sliceHas(info.Features, FeatureRCEnrich) {
		t.Fatalf("/api/info features missing tokens: %v", info.Features)
	}
}
