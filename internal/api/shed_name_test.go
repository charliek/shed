package api

import (
	"context"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/charliek/shed/internal/backend"
	"github.com/charliek/shed/internal/config"
)

// The router's one check of a shed name taken from the path
// (requireShedName). foldingBackend stands in for a shed store on a
// case-insensitive volume (the VZ backend on macOS): every name it is asked
// for is folded to lowercase before the lookup, so "BOX" finds the shed
// stored as "box", as a directory lookup there would. It records every call
// that names a shed or a snapshot, lookups included, so a test can assert that
// a name reached no backend at all.
type foldingBackend struct {
	*userFakeBackend
	mu    sync.Mutex
	calls []string
}

func newFoldingBackend() *foldingBackend {
	return &foldingBackend{userFakeBackend: newUserFakeBackend()}
}

// fold records a call and returns the name the store would resolve.
func (b *foldingBackend) fold(op, name string) string {
	b.mu.Lock()
	b.calls = append(b.calls, op+" "+name)
	b.mu.Unlock()
	return strings.ToLower(name)
}

func (b *foldingBackend) callsSnapshot() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.calls...)
}

func (b *foldingBackend) resetCalls() {
	b.mu.Lock()
	b.calls = nil
	b.mu.Unlock()
	b.reset()
}

func (b *foldingBackend) GetShed(ctx context.Context, name string) (*config.Shed, error) {
	return b.userFakeBackend.GetShed(ctx, b.fold("get", name))
}
func (b *foldingBackend) DeleteShed(ctx context.Context, name string) error {
	return b.userFakeBackend.DeleteShed(ctx, b.fold("delete", name))
}
func (b *foldingBackend) StartShed(ctx context.Context, name string) (*config.Shed, error) {
	return b.userFakeBackend.StartShed(ctx, b.fold("start", name))
}
func (b *foldingBackend) StopShed(ctx context.Context, name string) (*config.Shed, error) {
	return b.userFakeBackend.StopShed(ctx, b.fold("stop", name))
}
func (b *foldingBackend) AssignUser(ctx context.Context, name string, req config.AssignShedRequest) (*config.Shed, error) {
	return b.userFakeBackend.AssignUser(ctx, b.fold("assign", name), req)
}
func (b *foldingBackend) ResetShed(ctx context.Context, name string) (*config.Shed, error) {
	return b.userFakeBackend.ResetShed(ctx, b.fold("reset", name))
}
func (b *foldingBackend) ListSessions(ctx context.Context, name string) ([]config.Session, error) {
	return b.userFakeBackend.ListSessions(ctx, b.fold("sessions", name))
}
func (b *foldingBackend) KillSession(ctx context.Context, name, session string) error {
	return b.userFakeBackend.KillSession(ctx, b.fold("kill-session", name), session)
}
func (b *foldingBackend) Exec(ctx context.Context, name string, opts backend.ExecOptions) error {
	return b.userFakeBackend.Exec(ctx, b.fold("exec", name), opts)
}
func (b *foldingBackend) DialService(ctx context.Context, name string, port uint16) (net.Conn, error) {
	return b.userFakeBackend.DialService(ctx, b.fold("dial", name), port)
}
func (b *foldingBackend) SetShedEgress(ctx context.Context, name string, profiles []string) (*config.Shed, error) {
	return b.userFakeBackend.SetShedEgress(ctx, b.fold("egress-set", name), profiles)
}
func (b *foldingBackend) ClearShedEgress(ctx context.Context, name string) (*config.Shed, error) {
	return b.userFakeBackend.ClearShedEgress(ctx, b.fold("egress-clear", name))
}
func (b *foldingBackend) GetSnapshot(ctx context.Context, name string) (*config.Snapshot, error) {
	return b.userFakeBackend.GetSnapshot(ctx, b.fold("get-snapshot", name))
}
func (b *foldingBackend) DeleteSnapshot(ctx context.Context, name string) error {
	return b.userFakeBackend.DeleteSnapshot(ctx, b.fold("delete-snapshot", name))
}
func (b *foldingBackend) CreateSnapshot(ctx context.Context, req config.SnapshotCreateRequest) (*config.Snapshot, error) {
	req.SourceShed = b.fold("snapshot", req.SourceShed)
	return b.userFakeBackend.CreateSnapshot(ctx, req)
}

// pathParamRoutes classifies every registered route with a parameter in its
// path by what the parameter names, and so which rule checks it before any
// backend call:
//
//	shed       a shed: the router's requireShedName (config.ValidateShedName),
//	           which answers an invalid name as an unknown shed
//	snapshot   a snapshot: the handler's config.ValidateSnapshotName, 400
//	           INVALID_SNAPSHOT_NAME (unchanged by this check)
//	image      an image tag or digest, resolved by the image store: not a
//	           shed name (admin only for anything but a read)
//	profile    an egress profile, from the config or the profile store: never
//	           a backend lookup
//	namespace  a credential-bus namespace: never a backend lookup
//
// A route added to Router() with a parameter fails TestShedNameRouteWalk
// until it is classified here.
var pathParamRoutes = map[string]string{
	"GET /api/images/inspect/{name}":                  "image",
	"DELETE /api/images/{name}":                       "image",
	"GET /api/snapshots/{name}":                       "snapshot",
	"DELETE /api/snapshots/{name}":                    "snapshot",
	"GET /api/egress/profiles/{name}":                 "profile",
	"PUT /api/egress/profiles/{name}":                 "profile",
	"DELETE /api/egress/profiles/{name}":              "profile",
	"GET /api/egress/{name}":                          "shed",
	"POST /api/egress/{name}":                         "shed",
	"DELETE /api/egress/{name}":                       "shed",
	"GET /api/plugins/listeners/{namespace}/messages": "namespace",
	"POST /api/plugins/listeners/{namespace}/respond": "namespace",
	"GET /api/sheds/{name}":                           "shed",
	"DELETE /api/sheds/{name}":                        "shed",
	"POST /api/sheds/{name}/start":                    "shed",
	"POST /api/sheds/{name}/stop":                     "shed",
	"POST /api/sheds/{name}/assign":                   "shed",
	"POST /api/sheds/{name}/reset":                    "shed",
	"POST /api/sheds/{name}/exec":                     "shed",
	"GET /api/sheds/{name}/sessions":                  "shed",
	"DELETE /api/sheds/{name}/sessions/{session}":     "shed",
	"GET /api/sheds/{name}/connect/{port}":            "shed",
}

// invalidShedNames are path segments that are not valid shed names, each
// with the {name} chi hands the handler (it routes on the escaped path).
var invalidShedNames = []struct{ segment, name string }{
	{"Git", "Git"},
	{"GIT", "GIT"},
	{"BOX", "BOX"}, // folds to alice's shed "box"
	{"Box", "Box"},
	{"my_box", "my_box"},
	{strings.Repeat("a", config.MaxShedNameLength+1), strings.Repeat("a", config.MaxShedNameLength+1)},
	{"box.dev", "box.dev"},
	{"a%2Fb", "a%2Fb"},
	{"box%2F", "box%2F"},
	{"b%C3%B6x", "böx"},
	{"-box", "-box"},
	{"box-", "box-"},
	{"1box", "1box"},
}

// shedRouteRequest is one request on a shed route for the shed seg, with a
// body that passes the route's own checks, and the caller who may perform
// it on alice's shed: the admin for assign, alice for the rest.
func shedRouteRequest(route, seg string) (fp, method, path, body string) {
	method, path, body = routeRequest(route)
	path = strings.Replace(path, "/box", "/"+seg, 1)
	if route == "POST /api/sheds/{name}/assign" {
		return rootFP, method, path, body
	}
	return aliceFP, method, path, body
}

// TestShedNameRouteWalk: every route with a parameter is classified; every
// shed route answers an invalid name exactly as it answers an unknown shed
// (404 SHED_NOT_FOUND "shed not found: <name>") with no backend call at all,
// although the case-insensitive store would have found alice's shed for
// "BOX"; the valid lowercase name still reaches the backend.
func TestShedNameRouteWalk(t *testing.T) {
	be := newFoldingBackend()
	s, router := usersTestServer(t, be)

	walked := walkedRoutes(t, s.mux)
	walkedSet := map[string]bool{}
	var shedRoutes []string
	for _, route := range walked {
		walkedSet[route] = true
		_, pattern, _ := strings.Cut(route, " ")
		kind, ok := pathParamRoutes[route]
		// A brace parameter or a wildcard: either hands the handler a
		// caller-chosen path segment.
		if (strings.Contains(pattern, "{") || strings.Contains(pattern, "*")) && !ok {
			t.Errorf("route %s takes a path parameter but is not classified: decide what checks it before the backend", route)
		}
		if kind == "shed" {
			shedRoutes = append(shedRoutes, route)
		}
	}
	for route := range pathParamRoutes {
		if !walkedSet[route] {
			t.Errorf("classified route %s is not registered (stale list)", route)
		}
	}
	if len(shedRoutes) != 13 {
		t.Errorf("found %d shed routes, want 13: %v", len(shedRoutes), shedRoutes)
	}

	for _, route := range shedRoutes {
		// An unknown valid name: the answer an invalid one must match.
		be.resetCalls()
		fp, method, path, body := shedRouteRequest(route, "nosuch")
		unknown := doAs(router, fp, method, path, body)
		if unknown.code != http.StatusNotFound || unknown.errCode(t) != config.ErrShedNotFound || unknown.errMessage(t) != "shed not found: nosuch" {
			t.Errorf("%s unknown shed = %d %s, want 404 SHED_NOT_FOUND", route, unknown.code, unknown.body)
		}

		for _, inv := range invalidShedNames {
			be.resetCalls()
			fp, method, path, body := shedRouteRequest(route, inv.segment)
			r := doAs(router, fp, method, path, body)
			if r.code != unknown.code || r.errCode(t) != config.ErrShedNotFound || r.errMessage(t) != "shed not found: "+inv.name {
				t.Errorf("%s %q = %d %s, want the unknown-shed answer 404 SHED_NOT_FOUND \"shed not found: %s\"", route, inv.segment, r.code, r.body, inv.name)
			}
			if got := be.callsSnapshot(); len(got) != 0 {
				t.Errorf("%s %q reached the backend: %v", route, inv.segment, got)
			}
		}

		// The valid name reaches the backend and is served.
		be.resetCalls()
		fp, method, path, body = shedRouteRequest(route, "box")
		r := doAs(router, fp, method, path, body)
		if want := userRouteAllowed[route]; r.code != want {
			t.Errorf("%s box = %d %s, want %d", route, r.code, r.body, want)
		}
		reached := false
		for _, c := range be.callsSnapshot() {
			reached = reached || strings.HasSuffix(c, " box")
		}
		if !reached {
			t.Errorf("%s box never reached the backend: %v", route, be.callsSnapshot())
		}
	}
}

// TestShedNameEmptySegment: an empty {name} (a doubled slash) is an unknown
// shed on every shed route chi routes it to, and reaches no backend.
func TestShedNameEmptySegment(t *testing.T) {
	be := newFoldingBackend()
	_, router := usersTestServer(t, be)
	for _, tc := range []struct{ route, path string }{
		{"POST /api/sheds/{name}/start", "/api/sheds//start"},
		{"POST /api/sheds/{name}/stop", "/api/sheds//stop"},
		{"POST /api/sheds/{name}/exec", "/api/sheds//exec"},
		{"GET /api/sheds/{name}/sessions", "/api/sheds//sessions"},
		{"DELETE /api/sheds/{name}/sessions/{session}", "/api/sheds//sessions/main"},
		{"GET /api/sheds/{name}/connect/{port}", "/api/sheds//connect/22"},
		{"GET /api/egress/{name}", "/api/egress//"},
		{"DELETE /api/sheds/{name}", "/api/sheds//"},
	} {
		be.resetCalls()
		fp, method, _, body := shedRouteRequest(tc.route, "box")
		r := doAs(router, fp, method, tc.path, body)
		if r.code != http.StatusNotFound || r.errCode(t) != config.ErrShedNotFound || r.errMessage(t) != "shed not found: " {
			t.Errorf("%s %s = %d %s, want 404 SHED_NOT_FOUND \"shed not found: \"", method, tc.path, r.code, r.body)
		}
		if got := be.callsSnapshot(); len(got) != 0 {
			t.Errorf("%s %s reached the backend: %v", method, tc.path, got)
		}
	}
}

// TestShedNameStreamedDelete: a streamed delete (Accept: text/event-stream)
// of an invalid name answers exactly as one of an unknown shed does, a 200
// stream whose only event is the error, and reaches no backend.
func TestShedNameStreamedDelete(t *testing.T) {
	be := newFoldingBackend()
	_, router := usersTestServer(t, be)
	del := func(name string) apiResp {
		return do(router, testClientCert(aliceFP, "control", time.Hour), http.MethodDelete, "/api/sheds/"+name, "", "Accept", "text/event-stream")
	}
	unknown := del("nosuch")
	if unknown.code != http.StatusOK || !strings.Contains(string(unknown.body), "event: error") {
		t.Fatalf("unknown shed streamed delete = %d %q, want a 200 error event", unknown.code, unknown.body)
	}
	be.resetCalls()
	for _, name := range []string{"BOX", "Git"} {
		r := del(name)
		want := strings.ReplaceAll(string(unknown.body), "nosuch", name)
		if r.code != unknown.code || string(r.body) != want || r.header.Get("Content-Type") != unknown.header.Get("Content-Type") {
			t.Errorf("streamed delete %s = %d %q (%s), want %d %q (%s)", name, r.code, r.body, r.header.Get("Content-Type"),
				unknown.code, want, unknown.header.Get("Content-Type"))
		}
	}
	if got := be.callsSnapshot(); len(got) != 0 {
		t.Errorf("streamed delete of an invalid name reached the backend: %v", got)
	}
}

// TestShedNameUnroutedUnchanged: the check runs only on a matched route, so a
// method or path chi does not route below {name} answers as it always has,
// whatever the name.
func TestShedNameUnroutedUnchanged(t *testing.T) {
	be := newFoldingBackend()
	_, router := usersTestServer(t, be)
	for _, tc := range []struct{ method, valid, invalid string }{
		{http.MethodPut, "/api/sheds/nosuch", "/api/sheds/Git"},
		{http.MethodGet, "/api/sheds/nosuch/bogus", "/api/sheds/Git/bogus"},
		{http.MethodPut, "/api/egress/nosuch", "/api/egress/Git"},
	} {
		v := doAs(router, aliceFP, tc.method, tc.valid, "")
		i := doAs(router, aliceFP, tc.method, tc.invalid, "")
		if v.code != i.code || string(v.body) != string(i.body) {
			t.Errorf("%s %s = %d %q, but %s = %d %q", tc.method, tc.invalid, i.code, i.body, tc.valid, v.code, v.body)
		}
		if v.code != http.StatusMethodNotAllowed && v.code != http.StatusNotFound {
			t.Errorf("%s %s = %d, want chi's own 405 or 404", tc.method, tc.valid, v.code)
		}
	}
	if got := be.callsSnapshot(); len(got) != 0 {
		t.Errorf("unrouted requests reached the backend: %v", got)
	}
}

// TestShedNamedGitStillReachable: git is reserved for new sheds only. A shed
// named git made before that stays reachable on every shed route: the check
// is the shed-name rule, not the create rule.
func TestShedNamedGitStillReachable(t *testing.T) {
	be := newFoldingBackend()
	be.sheds["git"] = config.Shed{Name: "git", User: "alice", Status: config.StatusRunning, CreatedAt: time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)}
	_, router := usersTestServer(t, be)
	for route, kind := range pathParamRoutes {
		if kind != "shed" {
			continue
		}
		be.resetCalls()
		fp, method, path, body := shedRouteRequest(route, "git")
		r := doAs(router, fp, method, path, body)
		if want := userRouteAllowed[route]; r.code != want {
			t.Errorf("%s git = %d %s, want %d", route, r.code, r.body, want)
		}
		reached := false
		for _, c := range be.callsSnapshot() {
			reached = reached || strings.HasSuffix(c, " git")
		}
		if !reached {
			t.Errorf("%s git never reached the backend: %v", route, be.callsSnapshot())
		}
	}
	// And it is listed and can be snapshotted.
	r := doAs(router, aliceFP, http.MethodGet, "/api/sheds", "")
	if r.code != http.StatusOK || !strings.Contains(string(r.body), `"name":"git"`) {
		t.Errorf("GET /api/sheds = %d %s, want git listed", r.code, r.body)
	}
	be.resetCalls()
	r = doAs(router, aliceFP, http.MethodPost, "/api/snapshots", `{"name":"s2","source_shed":"git"}`)
	if r.code != http.StatusCreated {
		t.Errorf("POST /api/snapshots from git = %d %s, want 201", r.code, r.body)
	}
	if got := be.callsSnapshot(); !strings.Contains(strings.Join(got, ","), "snapshot git") {
		t.Errorf("snapshot of git never reached the backend: %v", got)
	}
}
