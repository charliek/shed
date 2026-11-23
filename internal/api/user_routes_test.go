package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/charliek/shed/internal/backend"
	"github.com/charliek/shed/internal/config"
	"github.com/charliek/shed/internal/delivery"
	"github.com/charliek/shed/internal/egress"
	"github.com/charliek/shed/internal/plugin"
	"github.com/charliek/shed/internal/source"
	"github.com/charliek/shed/internal/users"
	"github.com/charliek/shed/internal/users/userstest"
)

// The access guard at the API door, driven with named users through the
// userstest double (config cannot make named users yet; the rule is the
// same). alice and bob are users who are not admins; root is an admin.

var (
	aliceFP = testFingerprint("alice")
	bobFP   = testFingerprint("bob")
	rootFP  = testFingerprint("root")
)

func userLookup() *userstest.Fake {
	f := userstest.New()
	f.AddUser(users.User{Name: "alice"})
	f.AddUser(users.User{Name: "bob"})
	f.AddUser(users.User{Name: "root", Admin: true})
	f.AddKey("alice", users.Key{Fingerprint: aliceFP, Source: users.SourceInline})
	f.AddKey("bob", users.Key{Fingerprint: bobFP, Source: users.SourceInline})
	f.AddKey("root", users.Key{Fingerprint: rootFP, Source: users.SourceInline})
	return f
}

// userFakeBackend holds alice's running shed "box" and her snapshot "snap",
// and records every call that reaches it past the guard's lookups. Like the
// real backends it checks the incarnation required on its context.
type userFakeBackend struct {
	routesFakeBackend
	mu      sync.Mutex
	sheds   map[string]config.Shed
	snaps   map[string]config.Snapshot
	acted   []string
	created []config.CreateShedRequest
}

func newUserFakeBackend() *userFakeBackend {
	return &userFakeBackend{
		sheds: map[string]config.Shed{
			"box": {Name: "box", User: "alice", Status: config.StatusRunning, CreatedAt: time.Date(2026, 11, 14, 9, 0, 0, 0, time.UTC)},
		},
		snaps: map[string]config.Snapshot{"snap": {Name: "snap", User: "alice"}},
	}
}

func (b *userFakeBackend) Type() backend.Type { return backend.TypeFirecracker }

func (b *userFakeBackend) act(ctx context.Context, op, name string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if name != "" {
		s, ok := b.sheds[name]
		if !ok {
			return fmt.Errorf("%w: %s", config.ErrShedNotFoundSentinel, name)
		}
		if err := backend.CheckCreatedAt(ctx, name, s.CreatedAt); err != nil {
			return err
		}
	}
	b.acted = append(b.acted, op+" "+name)
	return nil
}

func (b *userFakeBackend) actedSnapshot() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.acted...)
}

func (b *userFakeBackend) reset() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.acted = nil
	b.created = nil
}

func (b *userFakeBackend) GetShed(ctx context.Context, name string) (*config.Shed, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	s, ok := b.sheds[name]
	if !ok {
		return nil, fmt.Errorf("%w: %s", config.ErrShedNotFoundSentinel, name)
	}
	return &s, nil
}

func (b *userFakeBackend) ListSheds(context.Context) ([]config.Shed, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []config.Shed
	for _, s := range b.sheds {
		out = append(out, s)
	}
	return out, nil
}

func (b *userFakeBackend) CreateShed(ctx context.Context, req config.CreateShedRequest) (*config.Shed, error) {
	if err := b.act(ctx, "create", ""); err != nil {
		return nil, err
	}
	b.mu.Lock()
	b.created = append(b.created, req)
	b.mu.Unlock()
	return &config.Shed{Name: req.Name, User: req.User, Owner: req.Owner}, nil
}

func (b *userFakeBackend) DeleteShed(ctx context.Context, name string) error {
	return b.act(ctx, "delete", name)
}

func (b *userFakeBackend) StartShed(ctx context.Context, name string) (*config.Shed, error) {
	if err := b.act(ctx, "start", name); err != nil {
		return nil, err
	}
	return b.GetShed(ctx, name)
}

func (b *userFakeBackend) StopShed(ctx context.Context, name string) (*config.Shed, error) {
	if err := b.act(ctx, "stop", name); err != nil {
		return nil, err
	}
	return b.GetShed(ctx, name)
}

// AssignUser applies the assignment rule under the fake's lock, as a real
// backend does, to the incarnation required on its context.
func (b *userFakeBackend) AssignUser(ctx context.Context, name string, req config.AssignShedRequest) (*config.Shed, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	s, ok := b.sheds[name]
	if !ok {
		return nil, fmt.Errorf("%w: %s", config.ErrShedNotFoundSentinel, name)
	}
	if err := backend.CheckCreatedAt(ctx, name, s.CreatedAt); err != nil {
		return nil, err
	}
	if err := backend.CheckAssignable(&s, req); err != nil {
		return nil, err
	}
	s.User = req.User
	b.sheds[name] = s
	b.acted = append(b.acted, "assign "+name)
	return &s, nil
}

func (b *userFakeBackend) ResetShed(ctx context.Context, name string) (*config.Shed, error) {
	if err := b.act(ctx, "reset", name); err != nil {
		return nil, err
	}
	return b.GetShed(ctx, name)
}

func (b *userFakeBackend) ListSessions(ctx context.Context, name string) ([]config.Session, error) {
	return []config.Session{}, b.act(ctx, "sessions", name)
}

func (b *userFakeBackend) KillSession(ctx context.Context, name, _ string) error {
	return b.act(ctx, "kill-session", name)
}

func (b *userFakeBackend) Exec(ctx context.Context, name string, opts backend.ExecOptions) error {
	if err := b.act(ctx, "exec", name); err != nil {
		return err
	}
	if opts.OnStarted != nil {
		opts.OnStarted()
	}
	return nil
}

func (b *userFakeBackend) DialService(ctx context.Context, name string, _ uint16) (net.Conn, error) {
	if err := b.act(ctx, "dial", name); err != nil {
		return nil, err
	}
	return nil, config.ErrShedNotRunningSentinel
}

func (b *userFakeBackend) SetShedEgress(ctx context.Context, name string, _ []string) (*config.Shed, error) {
	if err := b.act(ctx, "egress-set", name); err != nil {
		return nil, err
	}
	return b.GetShed(ctx, name)
}

func (b *userFakeBackend) ClearShedEgress(ctx context.Context, name string) (*config.Shed, error) {
	if err := b.act(ctx, "egress-clear", name); err != nil {
		return nil, err
	}
	return b.GetShed(ctx, name)
}

func (b *userFakeBackend) InspectImage(ctx context.Context, _ string) (config.ImageInspectResponse, error) {
	return config.ImageInspectResponse{}, b.act(ctx, "inspect", "")
}
func (b *userFakeBackend) TagImage(ctx context.Context, _, _ string) error {
	return b.act(ctx, "tag", "")
}
func (b *userFakeBackend) PullImage(ctx context.Context, _, _, _ string, _ bool) (string, error) {
	return "sha256:x", b.act(ctx, "pull", "")
}
func (b *userFakeBackend) PushImage(ctx context.Context, _, _ string) error {
	return b.act(ctx, "push", "")
}
func (b *userFakeBackend) DeleteImage(ctx context.Context, _ string) error {
	return b.act(ctx, "image-delete", "")
}
func (b *userFakeBackend) PruneImages(ctx context.Context, _ bool) ([]config.ImageInfo, error) {
	return nil, b.act(ctx, "image-prune", "")
}
func (b *userFakeBackend) Prune(ctx context.Context, _ backend.PruneOptions) (config.PruneReport, error) {
	return config.PruneReport{}, b.act(ctx, "prune", "")
}

func (b *userFakeBackend) CreateSnapshot(ctx context.Context, req config.SnapshotCreateRequest) (*config.Snapshot, error) {
	if err := b.act(ctx, "snapshot", req.SourceShed); err != nil {
		return nil, err
	}
	return &config.Snapshot{Name: req.Name, User: "alice"}, nil
}

func (b *userFakeBackend) ListSnapshots(context.Context) ([]config.Snapshot, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []config.Snapshot
	for _, s := range b.snaps {
		out = append(out, s)
	}
	return out, nil
}

func (b *userFakeBackend) GetSnapshot(_ context.Context, name string) (*config.Snapshot, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	s, ok := b.snaps[name]
	if !ok {
		return nil, fmt.Errorf("%w: %s", config.ErrSnapshotNotFoundSentinel, name)
	}
	return &s, nil
}

func (b *userFakeBackend) DeleteSnapshot(ctx context.Context, name string) error {
	if err := b.act(ctx, "snapshot-delete", ""); err != nil {
		return err
	}
	return backend.CheckSnapshot(ctx, name, &config.Snapshot{Name: name, User: "alice"})
}

// usersTestServer is an mtls server with the three users, egress on (so the
// egress routes reach the guard) and an image alias.
func usersTestServer(t *testing.T, b backend.Backend) (*Server, http.Handler) {
	t.Helper()
	cfg := &config.ServerConfig{
		Name:           "test-server",
		DefaultBackend: config.BackendFirecracker,
		Firecracker: &config.FirecrackerConfig{
			DefaultCPUs: 2, DefaultMemoryMB: 4096,
			ImageAliases: map[string]string{"itest": "ghcr.io/x/itest:v1"},
		},
		Mounts: map[string]config.MountConfig{"gh": {Source: "/srv/gh", Target: "/home/shed/.config/gh"}},
		Egress: &config.EgressConfig{Enabled: true},
		Auth:   &config.AuthConfig{Mode: config.AuthModeMTLS},
	}
	reg := plugin.NewRegistry()
	s := NewServer(b, cfg, "", reg, plugin.NewBridge(reg, true))
	s.SetUserLookup(userLookup())
	pinBus(s)
	s.SetDelivery(delivery.NewService(reg, 10*time.Second))
	store, err := config.OpenUserProfileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s.SetEgressUserStore(store)
	audit, err := egress.OpenAuditLog(filepath.Join(t.TempDir(), "egress.jsonl"), 10)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = audit.Close() })
	s.SetEgressAudit(audit)
	return s, s.Router()
}

func doAs(h http.Handler, fp, method, path, body string) apiResp {
	return do(h, testClientCert(fp, "control", time.Hour), method, path, body)
}

// doRouteAs is doAs for one route of the walk, with a credentials
// certificate on the credential bus. The egress stream and a bus
// subscription do not end on their own, so they are served on a context that
// ends shortly.
func doRouteAs(h http.Handler, fp, route, method, path, body string) apiResp {
	scope := "control"
	if strings.HasPrefix(userRouteClasses[route], "bus") || strings.HasPrefix(userRouteClasses[route], "delivery") {
		scope = "credentials"
	}
	if route != "GET /api/egress/stream" && route != "GET /api/plugins/listeners/{namespace}/messages" && route != "GET /api/delivery/requests" {
		return do(h, testClientCert(fp, scope, time.Hour), method, path, body)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	req := httptest.NewRequest(method, path, nil).WithContext(ctx)
	withClientCert(req, testClientCert(fp, scope, time.Hour))
	withTestMachine(req, fp)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return apiResp{rec.Code, rec.Body.Bytes(), rec.Header()}
}

// userRouteClasses classifies every registered route for the access rule:
//
//	shed:view|manage|enter   an operation on the shed {name} ("box")
//	snapshot:view|manage     an operation on the snapshot {name} ("snap")
//	source:enter             POST /api/snapshots, on the source shed in the body
//	create                   POST /api/sheds
//	admin                    server-wide state with no owner: admin only
//	any                      any user (lists are filtered by the guard)
//	bus:list                 a credential-bus listing: any user, filtered to
//	                         the caller's own rows (an admin sees all)
//	bus:subscribe            a credential-bus subscription: any user, as
//	                         themselves
//	bus:respond              an answer to a request of alice's shed "box":
//	                         alice only (no admin exception), 403 otherwise
//	pin:list|use             a user's credential source: every user reads and
//	                         moves their own; only an admin may name another
//	                         user, 403 otherwise
//	delivery:stream          a delivery stream: any user, as themselves
//	delivery:answer          an answer to a delivery request for alice's
//	                         shed "box": alice only (no admin exception),
//	                         refused otherwise and never consumed
//
// A route added to Router() fails TestUserRouteWalk until it is classified.
var userRouteClasses = map[string]string{
	"GET /api/info":                                   "any",
	"GET /api/ssh-host-key":                           "any",
	"GET /api/sessions":                               "any",
	"GET /api/overview":                               "any",
	"GET /api/images":                                 "any",
	"GET /api/images/inspect":                         "any",
	"GET /api/images/inspect/{name}":                  "any",
	"DELETE /api/images":                              "admin",
	"DELETE /api/images/{name}":                       "admin",
	"POST /api/images/prune":                          "admin",
	"POST /api/images/pull":                           "admin",
	"POST /api/images/push":                           "admin",
	"POST /api/images/tag":                            "admin",
	"GET /api/system/df":                              "any",
	"POST /api/system/prune":                          "admin",
	"GET /api/snapshots":                              "any",
	"POST /api/snapshots":                             "source:enter",
	"GET /api/snapshots/{name}":                       "snapshot:view",
	"DELETE /api/snapshots/{name}":                    "snapshot:manage",
	"GET /api/egress/stream":                          "any",
	"GET /api/egress/profiles":                        "any",
	"GET /api/egress/profiles/{name}":                 "any",
	"PUT /api/egress/profiles/{name}":                 "admin",
	"DELETE /api/egress/profiles/{name}":              "admin",
	"GET /api/egress/{name}":                          "shed:view",
	"POST /api/egress/{name}":                         "shed:enter",
	"DELETE /api/egress/{name}":                       "shed:enter",
	"POST /api/users/refresh":                         "admin",
	"GET /api/users/status":                           "admin",
	"GET /api/sources":                                "pin:list",
	"POST /api/sources/use":                           "pin:use",
	"GET /api/delivery/requests":                      "delivery:stream",
	"POST /api/delivery/requests/{id}":                "delivery:answer",
	"GET /api/plugins/listeners":                      "bus:list",
	"GET /api/plugins/listeners/{namespace}/messages": "bus:subscribe",
	"POST /api/plugins/listeners/{namespace}/respond": "bus:respond",
	"GET /api/plugins/sheds":                          "bus:list",
	"GET /api/sheds":                                  "any",
	"POST /api/sheds":                                 "create",
	"GET /api/sheds/{name}":                           "shed:view",
	"DELETE /api/sheds/{name}":                        "shed:manage",
	"POST /api/sheds/{name}/start":                    "shed:enter",
	"POST /api/sheds/{name}/stop":                     "shed:manage",
	"POST /api/sheds/{name}/assign":                   "admin",
	"POST /api/sheds/{name}/reset":                    "shed:enter",
	"POST /api/sheds/{name}/exec":                     "shed:enter",
	"GET /api/sheds/{name}/sessions":                  "shed:enter",
	"DELETE /api/sheds/{name}/sessions/{session}":     "shed:enter",
	"GET /api/sheds/{name}/connect/{port}":            "shed:enter",
}

// routeRequest is the request that exercises a route against alice's things.
func routeRequest(route string) (method, path, body string) {
	method, pattern, _ := strings.Cut(route, " ")
	name := "box"
	if strings.HasPrefix(pattern, "/api/snapshots/") {
		name = "snap"
	}
	if strings.HasPrefix(pattern, "/api/egress/profiles/") || strings.HasPrefix(pattern, "/api/images/") {
		name = "mine"
	}
	path = strings.NewReplacer("{name}", name, "{namespace}", "ssh-agent", "{port}", "22", "{session}", "main").Replace(pattern)
	switch route {
	case "POST /api/sheds/{name}/exec":
		body = `{"argv":["true"]}`
	case "POST /api/egress/{name}":
		body = `{"profiles":["off"]}`
	case "PUT /api/egress/profiles/{name}":
		body = `{"allow":["example.com"]}`
	case "POST /api/snapshots":
		body = `{"name":"s2","source_shed":"box"}`
	case "POST /api/sheds":
		body = `{"name":"fresh"}`
	case "POST /api/sheds/{name}/assign":
		body = `{"user":"bob"}`
	case "POST /api/images/tag":
		body = `{"source":"a","target":"b"}`
	case "POST /api/images/pull":
		body = `{"docker_ref":"ghcr.io/x/y:v1","tag":"y"}`
	case "POST /api/images/push":
		body = `{"source":"a","destination":"ghcr.io/x/y:v2"}`
	case "DELETE /api/images", "GET /api/images/inspect":
		path += "?ref=a"
	}
	return method, path, body
}

// userRouteAllowed is the status each route answers a caller it allows, with
// the fake backend above: what the walk asserts for every allowed case, so a
// route broken past the access rule (a 500, a 400) fails it rather than
// passing for "not refused". The connect route's fake dial reports the shed
// not running (503): a hijacked stream cannot be served by the recorder, and
// the walk also checks that the dial reached the backend.
var userRouteAllowed = map[string]int{
	"GET /api/info":                                   http.StatusOK,
	"GET /api/ssh-host-key":                           http.StatusOK,
	"GET /api/sessions":                               http.StatusOK,
	"GET /api/overview":                               http.StatusOK,
	"GET /api/images":                                 http.StatusOK,
	"GET /api/images/inspect":                         http.StatusOK,
	"GET /api/images/inspect/{name}":                  http.StatusOK,
	"DELETE /api/images":                              http.StatusNoContent,
	"DELETE /api/images/{name}":                       http.StatusNoContent,
	"POST /api/images/prune":                          http.StatusOK,
	"POST /api/images/pull":                           http.StatusOK,
	"POST /api/images/push":                           http.StatusOK,
	"POST /api/images/tag":                            http.StatusNoContent,
	"GET /api/system/df":                              http.StatusOK,
	"POST /api/system/prune":                          http.StatusOK,
	"GET /api/snapshots":                              http.StatusOK,
	"POST /api/snapshots":                             http.StatusCreated,
	"GET /api/snapshots/{name}":                       http.StatusOK,
	"DELETE /api/snapshots/{name}":                    http.StatusNoContent,
	"GET /api/egress/stream":                          http.StatusOK,
	"GET /api/egress/profiles":                        http.StatusOK,
	"GET /api/egress/profiles/{name}":                 http.StatusOK,
	"PUT /api/egress/profiles/{name}":                 http.StatusOK,
	"DELETE /api/egress/profiles/{name}":              http.StatusOK,
	"GET /api/egress/{name}":                          http.StatusOK,
	"POST /api/egress/{name}":                         http.StatusOK,
	"DELETE /api/egress/{name}":                       http.StatusOK,
	"POST /api/users/refresh":                         http.StatusOK,
	"GET /api/users/status":                           http.StatusOK,
	"GET /api/sources":                                http.StatusOK,
	"POST /api/sources/use":                           http.StatusOK,
	"GET /api/delivery/requests":                      http.StatusOK,
	"POST /api/delivery/requests/{id}":                http.StatusNoContent,
	"GET /api/plugins/listeners":                      http.StatusOK,
	"GET /api/plugins/listeners/{namespace}/messages": http.StatusOK,
	"POST /api/plugins/listeners/{namespace}/respond": http.StatusNoContent,
	"GET /api/plugins/sheds":                          http.StatusOK,
	"GET /api/sheds":                                  http.StatusOK,
	"POST /api/sheds":                                 http.StatusCreated,
	"GET /api/sheds/{name}":                           http.StatusOK,
	"DELETE /api/sheds/{name}":                        http.StatusNoContent,
	"POST /api/sheds/{name}/start":                    http.StatusOK,
	"POST /api/sheds/{name}/stop":                     http.StatusOK,
	// The admin passes the access rule and reaches the assignment rule:
	// "box" is alice's already (TestAssignShedRoute assigns one with none).
	"POST /api/sheds/{name}/assign":               http.StatusConflict,
	"POST /api/sheds/{name}/reset":                http.StatusOK,
	"POST /api/sheds/{name}/exec":                 http.StatusOK,
	"GET /api/sheds/{name}/sessions":              http.StatusOK,
	"DELETE /api/sheds/{name}/sessions/{session}": http.StatusNoContent,
	"GET /api/sheds/{name}/connect/{port}":        http.StatusServiceUnavailable,
}

// TestUserRouteWalk walks every registered route and proves each is guarded
// for named users, or listed as reaching no shed. For each route: bob (not an
// admin) is refused anything of alice's, with 404 (he must not learn the
// name), or 403 on an admin route, and reaches nothing in the backend; root
// (an admin) may view, stop and delete alice's shed but gets 403 entering
// it; alice passes the guard on her own. Every allowed case must answer the
// route's expected status (userRouteAllowed), and one that acts on alice's
// shed must have reached the backend for it.
func TestUserRouteWalk(t *testing.T) {
	be := newUserFakeBackend()
	s, router := usersTestServer(t, be)
	// The profile routes act on the profile "mine": it exists before every
	// request (a DELETE of it by the admin is put back).
	seed := func() {
		if err := s.egressStore.Put("mine", config.EgressProfile{Allow: []string{"example.com"}}); err != nil {
			t.Fatal(err)
		}
	}

	walked := walkedRoutes(t, s.mux)
	walkedSet := map[string]bool{}
	for _, k := range walked {
		walkedSet[k] = true
		if _, ok := userRouteClasses[k]; !ok {
			t.Errorf("route %s is not classified for the access rule: decide who may reach it", k)
		}
	}
	for k := range userRouteClasses {
		if !walkedSet[k] {
			t.Errorf("classified route %s is not registered (stale list)", k)
		}
		if _, ok := userRouteAllowed[k]; !ok {
			t.Errorf("route %s has no expected status for an allowed caller", k)
		}
	}

	// allowed checks an allowed case: the route's expected status and, for
	// one that acts on alice's shed, that the action reached the backend.
	allowed := func(who, route, kind, action string, r apiResp) {
		t.Helper()
		if want := userRouteAllowed[route]; r.code != want {
			t.Errorf("%s %s = %d %s, want allowed with %d", who, route, r.code, r.body, want)
			return
		}
		if (kind == "shed" || kind == "source") && action != "view" {
			reached := false
			for _, op := range be.actedSnapshot() {
				reached = reached || strings.HasSuffix(op, " box")
			}
			if !reached {
				t.Errorf("%s %s was allowed but never reached the backend for alice's shed: %v", who, route, be.actedSnapshot())
			}
		}
	}
	// alice's shed "box" is connected to the credential bus.
	box := &sendCounter{}
	boxGen := s.bridge.RegisterShed("box", &plugin.ShedConn{User: "alice", CreatedAt: be.sheds["box"].CreatedAt, Send: box.send})

	for _, route := range walked {
		class := userRouteClasses[route]
		method, path, body := routeRequest(route)
		kind, action, _ := strings.Cut(class, ":")
		if kind == "bus" {
			walkBusRoute(t, s, router, be, route, action, box, boxGen)
			continue
		}
		if kind == "pin" {
			walkPinRoute(t, s, router, be, route)
			continue
		}
		if kind == "delivery" {
			walkDeliveryRoute(t, s, router, be, route, action)
			continue
		}

		be.reset()
		seed()
		bob := doRouteAs(router, bobFP, route, method, path, body)
		switch kind {
		case "shed", "source":
			if bob.code != http.StatusNotFound || bob.errCode(t) != config.ErrShedNotFound {
				t.Errorf("bob %s = %d %s, want 404 SHED_NOT_FOUND", route, bob.code, bob.body)
			}
		case "snapshot":
			if bob.code != http.StatusNotFound || bob.errCode(t) != config.ErrSnapshotNotFound {
				t.Errorf("bob %s = %d %s, want 404 SNAPSHOT_NOT_FOUND", route, bob.code, bob.body)
			}
		case "admin":
			if bob.code != http.StatusForbidden || bob.errCode(t) != config.ErrForbidden {
				t.Errorf("bob %s = %d %s, want 403 FORBIDDEN", route, bob.code, bob.body)
			}
		case "any", "create":
			allowed("bob", route, kind, action, bob)
		}
		if kind != "any" && kind != "create" {
			if got := be.actedSnapshot(); len(got) != 0 {
				t.Errorf("bob %s reached the backend: %v", route, got)
			}
		}

		be.reset()
		seed()
		root := doRouteAs(router, rootFP, route, method, path, body)
		switch {
		case (kind == "shed" || kind == "source") && action == "enter":
			if root.code != http.StatusForbidden || root.errCode(t) != config.ErrForbidden {
				t.Errorf("admin %s = %d %s, want 403 FORBIDDEN", route, root.code, root.body)
			}
			if got := be.actedSnapshot(); len(got) != 0 {
				t.Errorf("admin %s entered alice's shed: %v", route, got)
			}
		default:
			allowed("admin", route, kind, action, root)
		}

		be.reset()
		seed()
		alice := doRouteAs(router, aliceFP, route, method, path, body)
		switch kind {
		case "admin":
			if alice.code != http.StatusForbidden || alice.errCode(t) != config.ErrForbidden {
				t.Errorf("alice %s = %d %s, want 403 FORBIDDEN", route, alice.code, alice.body)
			}
		default:
			allowed("alice", route, kind, action, alice)
		}
	}
}

// walkBusRoute checks one credential-bus route of the walk for bob, root
// and alice: the bus reaches no shed through the backend; a listing or a
// subscription is allowed to every user, as themselves (a listing shows bob
// nothing of alice's); an answer to a request of alice's shed is alice's
// alone, refused to bob and to the admin with the forged-response 403.
func walkBusRoute(t *testing.T, s *Server, router http.Handler, be *userFakeBackend, route, action string, box *sendCounter, boxGen uint64) {
	t.Helper()
	method, path, body := routeRequest(route)
	if action == "respond" {
		// A request of alice's shed, sent to alice's machine.
		l := listen(t, s, "alice", "ssh-agent", aliceFP)
		defer s.plugins.Unregister(l.Key(), l.Token)
		body = respondBody("box", publishFromShed(t, s, "box", boxGen))
	}
	before := box.count()
	for _, who := range []struct{ name, fp string }{{"bob", bobFP}, {"admin", rootFP}, {"alice", aliceFP}} {
		be.reset()
		r := doRouteAs(router, who.fp, route, method, path, body)
		if got := be.actedSnapshot(); len(got) != 0 {
			t.Errorf("%s %s reached the backend: %v", who.name, route, got)
		}
		if action == "respond" && who.name != "alice" {
			if r.code != http.StatusForbidden || r.errCode(t) != config.ErrForbidden {
				t.Errorf("%s %s = %d %s, want 403 FORBIDDEN", who.name, route, r.code, r.body)
			}
			if n := box.count(); n != before {
				t.Errorf("%s %s delivered a response to alice's shed", who.name, route)
			}
			continue
		}
		if want := userRouteAllowed[route]; r.code != want {
			t.Errorf("%s %s = %d %s, want allowed with %d", who.name, route, r.code, r.body, want)
			continue
		}
		if action == "list" {
			mentions := strings.Contains(string(r.body), `"box"`) || strings.Contains(string(r.body), `"alice"`)
			if who.name == "bob" && mentions {
				t.Errorf("bob %s lists alice's rows: %s", route, r.body)
			}
			if who.name != "bob" && route == "GET /api/plugins/sheds" && !mentions {
				t.Errorf("%s %s does not list alice's shed: %s", who.name, route, r.body)
			}
		}
	}
	if action == "respond" && box.count() != before+1 {
		t.Errorf("alice %s: box was sent %d responses, want 1", route, box.count()-before)
	}
}

// walkPinRoute checks one credential source route of the walk for bob, root
// and alice, with alice's machine bound: it reaches no shed through the
// backend; each reads and "moves" only their own binding (a move to alice's
// machine is a no-op for alice and unknown for anyone else, so the walk
// changes no binding); bob naming alice is refused with 403 and learns
// nothing; the admin naming alice reads and moves hers.
func walkPinRoute(t *testing.T, s *Server, router http.Handler, be *userFakeBackend, route string) {
	t.Helper()
	aliceM := testMachineFor(aliceFP)
	l, err := s.plugins.Subscribe(context.Background(), plugin.ListenerKey{User: "alice", Namespace: "x-walk-pin"}, aliceM)
	if err != nil {
		t.Fatalf("binding alice's machine: %v", err)
	}
	defer s.plugins.Unregister(l.Key(), l.Token)
	type pinCase struct {
		who, fp, named string
		want           int
		wantCode       string
		mentionsAlice  bool
	}
	var cases []pinCase
	if route == "GET /api/sources" {
		cases = []pinCase{
			{"bob", bobFP, "", http.StatusOK, "", false},
			{"bob", bobFP, "alice", http.StatusForbidden, config.ErrForbidden, false},
			{"admin", rootFP, "", http.StatusOK, "", false},
			{"admin", rootFP, "alice", http.StatusOK, "", true},
			{"alice", aliceFP, "", http.StatusOK, "", true},
		}
	} else {
		cases = []pinCase{
			{"bob", bobFP, "", http.StatusNotFound, config.ErrSourceMachineNotFound, false},
			{"bob", bobFP, "alice", http.StatusForbidden, config.ErrForbidden, false},
			{"admin", rootFP, "", http.StatusNotFound, config.ErrSourceMachineNotFound, false},
			{"admin", rootFP, "alice", http.StatusOK, "", true},
			{"alice", aliceFP, "", http.StatusOK, "", true},
		}
	}
	gen := mustBinding(t, s, "alice").Generation
	for _, c := range cases {
		be.reset()
		var r apiResp
		if route == "GET /api/sources" {
			path := "/api/sources"
			if c.named != "" {
				path += "?user=" + c.named
			}
			r = doAs(router, c.fp, http.MethodGet, path, "")
		} else {
			body := fmt.Sprintf(`{"machine_id":%q}`, aliceM.ID)
			if c.named != "" {
				body = fmt.Sprintf(`{"machine_id":%q,"user":%q}`, aliceM.ID, c.named)
			}
			r = doAs(router, c.fp, http.MethodPost, "/api/sources/use", body)
		}
		if got := be.actedSnapshot(); len(got) != 0 {
			t.Errorf("%s %s reached the backend: %v", c.who, route, got)
		}
		if r.code != c.want || (c.wantCode != "" && r.errCode(t) != c.wantCode) {
			t.Errorf("%s %s (naming %q) = %d %s, want %d %s", c.who, route, c.named, r.code, r.body, c.want, c.wantCode)
			continue
		}
		// A refusal may echo the id the caller sent; an answer lists only
		// the target user's machines.
		if mentions := strings.Contains(string(r.body), aliceM.ID); r.code == http.StatusOK && mentions != c.mentionsAlice {
			t.Errorf("%s %s (naming %q) mentions alice's machine: %v, want %v: %s", c.who, route, c.named, mentions, c.mentionsAlice, r.body)
		}
	}
	if bd := mustBinding(t, s, "alice"); bd.MachineID() != aliceM.ID || bd.Generation != gen {
		t.Errorf("the walk of %s changed alice's binding: %+v", route, bd)
	}
}

// walkDeliveryRoute checks one delivery route of the walk for bob, root and
// alice: delivery reaches no shed through the backend; each holds a stream
// as themselves (their own machine, bound for them); an answer to a request
// for alice's shed is alice's alone: bob and the admin are refused (their
// machine is not alice's, and the request is not theirs) and consume
// nothing, and alice's answer is then accepted.
func walkDeliveryRoute(t *testing.T, s *Server, router http.Handler, be *userFakeBackend, route, action string) {
	t.Helper()
	method, path, _ := routeRequest(route)
	if action == "stream" {
		for _, who := range []struct{ name, fp string }{{"bob", bobFP}, {"admin", rootFP}, {"alice", aliceFP}} {
			be.reset()
			r := doRouteAs(router, who.fp, route, method, path, "")
			if r.code != http.StatusOK || len(r.header.Get(delivery.HeaderStream)) != 32 || r.header.Get("Cache-Control") != "no-store" {
				t.Errorf("%s %s = %d %v %s, want 200 naming its stream, no-store", who.name, route, r.code, r.header, r.body)
			}
			if got := be.actedSnapshot(); len(got) != 0 {
				t.Errorf("%s %s reached the backend: %v", who.name, route, got)
			}
		}
		return
	}
	// A request for alice's shed, on alice's stream.
	st, err := s.plugins.SubscribeDelivery(context.Background(), "alice", testMachineFor(aliceFP), "")
	if err != nil {
		t.Fatalf("alice's delivery stream: %v", err)
	}
	defer s.plugins.UnregisterDelivery(st)
	done := make(chan delivery.Result, 1)
	go func() {
		res, err := s.delivery.Request(context.Background(), delivery.Params{
			Reason: delivery.ReasonCreate, User: "alice",
			Shed: delivery.RequestShed{Name: "box", ID: "boxid", Backend: delivery.BackendFirecracker},
		})
		if err != nil {
			t.Errorf("request: %v", err)
		}
		done <- res
	}()
	var out delivery.Outgoing
	select {
	case out = <-st.Requests():
	case <-time.After(5 * time.Second):
		t.Fatal("no request reached alice's stream")
	}
	answerPath := delivery.AnswerPathPrefix + out.ID
	body := fmt.Sprintf(`{"version":1,"id":%q,"items":[],"problems":[]}`, out.ID)
	for _, who := range []struct{ name, fp string }{{"bob", bobFP}, {"admin", rootFP}, {"alice", aliceFP}} {
		be.reset()
		r := do(router, testClientCert(who.fp, "credentials", time.Hour), method, answerPath, body, delivery.HeaderStream, st.ID())
		if got := be.actedSnapshot(); len(got) != 0 {
			t.Errorf("%s %s reached the backend: %v", who.name, route, got)
		}
		if r.header.Get("Cache-Control") != "no-store" {
			t.Errorf("%s %s answered without no-store: %v", who.name, route, r.header)
		}
		if who.name != "alice" {
			code := r.errCode(t)
			unknown := r.code == http.StatusNotFound && code == delivery.CodeUnknownRequest
			notBound := r.code == http.StatusForbidden && code == config.ErrSourceNotBound
			if !unknown && !notBound {
				t.Errorf("%s %s = %d %s, want 404 unknown_request or 403 SOURCE_NOT_BOUND", who.name, route, r.code, r.body)
			}
			continue
		}
		if want := userRouteAllowed[route]; r.code != want {
			t.Errorf("alice %s = %d %s, want %d", route, r.code, r.body, want)
		}
	}
	select {
	case res := <-done:
		if res.Outcome != delivery.OutcomeAccepted {
			t.Errorf("alice's request ended %+v, want accepted", res)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("alice's request did not end")
	}
}

// mustBinding is user's binding; the test fails without one.
func mustBinding(t *testing.T, s *Server, user string) source.Binding {
	t.Helper()
	bd, ok := s.plugins.Bindings().Get(user)
	if !ok {
		t.Fatalf("%s has no binding", user)
	}
	return bd
}

// TestUserCreateRefusal: a user who is not an admin may not ask for what is
// shared on the host, refused before anything reads the host; an image must
// be the default or an alias; omitting mounts gives none. An admin may.
func TestUserCreateRefusal(t *testing.T) {
	be := newUserFakeBackend()
	_, router := usersTestServer(t, be)
	dir := t.TempDir()
	for _, tc := range []struct {
		body, field string
	}{
		{`{"name":"x","local_dir":"/definitely/not/here"}`, "local_dir"},
		{`{"name":"x","local_dir":"` + dir + `","add_dirs":["/nope"]}`, "local_dir"},
		{`{"name":"x","add_dirs":["/nope"]}`, "add_dirs"},
		{`{"name":"x","mounts":["gh"]}`, "mounts"},
		{`{"name":"x","image":"ghcr.io/evil/image:latest"}`, "image"},
		{`{"name":"x","image":"/var/lib/images/thing.erofs"}`, "image"},
	} {
		be.reset()
		r := doAs(router, aliceFP, http.MethodPost, "/api/sheds", tc.body)
		if r.code != http.StatusForbidden || r.errCode(t) != config.ErrForbidden || !strings.Contains(r.errMessage(t), "admin only: "+tc.field) {
			t.Errorf("alice create %s = %d %s, want 403 admin only: %s", tc.body, r.code, r.body, tc.field)
		}
		if len(be.actedSnapshot()) != 0 {
			t.Errorf("refused create reached the backend: %v", be.actedSnapshot())
		}
	}

	for _, body := range []string{`{"name":"x"}`, `{"name":"x","image":"itest"}`, `{"name":"x","mounts":[]}`} {
		be.reset()
		r := doAs(router, aliceFP, http.MethodPost, "/api/sheds", body)
		if r.code != http.StatusCreated {
			t.Fatalf("alice create %s = %d %s", body, r.code, r.body)
		}
		req := be.created[0]
		if req.User != "alice" || req.Owner != "" || !req.NoServerEnv {
			t.Errorf("alice's create reached the backend as user %q owner %q noServerEnv %v", req.User, req.Owner, req.NoServerEnv)
		}
		if m := req.ResolvedMounts; m == nil || m.Mode != config.MountModeSelected || len(m.Names) != 0 {
			t.Errorf("alice's create stores mounts %+v, want none", m)
		}
	}

	be.reset()
	r := doAs(router, rootFP, http.MethodPost, "/api/sheds", `{"name":"x","image":"ghcr.io/any/image:v1"}`)
	if r.code != http.StatusCreated {
		t.Fatalf("admin create = %d %s", r.code, r.body)
	}
	if req := be.created[0]; req.User != "root" || req.NoServerEnv || req.ResolvedMounts == nil || req.ResolvedMounts.Mode != config.MountModeAll {
		t.Errorf("admin's create reached the backend as %+v", req)
	}
}

// TestHandlerWithNoPrincipalIsRefused: a handler reached with no caller on
// the request (a route served without the auth middleware) is refused by the
// guard, and the backend is never reached: no caller is an admin.
func TestHandlerWithNoPrincipalIsRefused(t *testing.T) {
	be := newUserFakeBackend()
	s, _ := usersTestServer(t, be)
	for _, tc := range []struct {
		name    string
		handler http.HandlerFunc
		method  string
	}{
		{"get", s.handleGetShed, http.MethodGet},
		{"stop", s.handleStopShed, http.MethodPost},
		{"delete", s.handleDeleteShed, http.MethodDelete},
		{"list", s.handleListSheds, http.MethodGet},
	} {
		be.reset()
		r := httptest.NewRequest(tc.method, "/api/sheds/box", nil)
		rctx := chi.NewRouteContext()
		rctx.URLParams.Add("name", "box")
		r = r.WithContext(context.WithValue(r.Context(), chi.RouteCtxKey, rctx))
		w := httptest.NewRecorder()
		tc.handler(w, r)
		if w.Code != http.StatusInternalServerError {
			t.Errorf("%s with no principal = %d %s, want 500", tc.name, w.Code, w.Body.String())
		}
		if got := be.actedSnapshot(); len(got) != 0 {
			t.Errorf("%s with no principal reached the backend: %v", tc.name, got)
		}
	}
	if _, err := s.backend.GetShed(context.Background(), "box"); !errors.Is(err, backend.ErrNoPrincipal) {
		t.Errorf("the server's backend without a principal: %v, want ErrNoPrincipal", err)
	}
}

// TestAdminSessionListsSkipSheds: the session aggregates list what the caller
// may enter; an admin sees alice's shed but not its sessions, and no warning.
func TestAdminSessionListsSkipSheds(t *testing.T) {
	be := newUserFakeBackend()
	_, router := usersTestServer(t, be)
	r := doAs(router, rootFP, http.MethodGet, "/api/sessions", "")
	var sessions config.SessionsResponse
	if err := json.Unmarshal(r.body, &sessions); err != nil || r.code != http.StatusOK {
		t.Fatalf("admin /api/sessions = %d %s", r.code, r.body)
	}
	if len(sessions.Warnings) != 0 {
		t.Errorf("admin /api/sessions warns %v", sessions.Warnings)
	}
	r = doAs(router, rootFP, http.MethodGet, "/api/overview", "")
	var ov OverviewResponse
	if err := json.Unmarshal(r.body, &ov); err != nil || r.code != http.StatusOK {
		t.Fatalf("admin /api/overview = %d %s", r.code, r.body)
	}
	if len(ov.Sheds) != 1 || len(ov.Warnings) != 0 {
		t.Errorf("admin overview: sheds %d warnings %v", len(ov.Sheds), ov.Warnings)
	}
	if got := be.actedSnapshot(); len(got) != 0 {
		t.Errorf("the admin's lists entered alice's shed: %v", got)
	}
	r = doAs(router, bobFP, http.MethodGet, "/api/overview", "")
	_ = json.Unmarshal(r.body, &ov)
	if len(ov.Sheds) != 0 {
		t.Errorf("bob's overview lists %v", ov.Sheds)
	}
}

// TestViewersSeeNoRepoCredentials: a shed's repository URL can carry
// credentials anywhere in it (here in its query, which the landing directory
// derived from it repeats). An admin may list, read, stop and wait on another
// user's shed without being allowed into it, so every record that reaches the
// admin carries neither the URL nor the landing directory: get, list,
// overview and the stop response. alice, whose shed it is, sees both as
// stored.
func TestViewersSeeNoRepoCredentials(t *testing.T) {
	const secret = "s3cr3t-T0KEN"
	be := newUserFakeBackend()
	box := be.sheds["box"]
	box.Repo = "https://git.example/private.git?access_token=" + secret
	box.LandingDir = config.HomePath + "/private.git?access_token=" + secret
	be.sheds["box"] = box
	_, router := usersTestServer(t, be)
	for _, rt := range []struct{ method, path string }{
		{http.MethodGet, "/api/sheds/box"},
		{http.MethodGet, "/api/sheds"},
		{http.MethodGet, "/api/overview"},
		{http.MethodPost, "/api/sheds/box/stop"},
	} {
		root := doAs(router, rootFP, rt.method, rt.path, "")
		if root.code != http.StatusOK {
			t.Fatalf("admin %s %s = %d %s", rt.method, rt.path, root.code, root.body)
		}
		if body := string(root.body); strings.Contains(body, secret) || strings.Contains(body, "private.git") ||
			strings.Contains(body, `"repo"`) || strings.Contains(body, `"landing_dir"`) {
			t.Errorf("admin %s %s: %s, want no repo and no landing directory", rt.method, rt.path, root.body)
		}
		alice := doAs(router, aliceFP, rt.method, rt.path, "")
		if alice.code != http.StatusOK || !strings.Contains(string(alice.body), box.Repo) ||
			!strings.Contains(string(alice.body), box.LandingDir) {
			t.Errorf("alice %s %s = %d %s, want her repo and landing directory as stored", rt.method, rt.path, alice.code, alice.body)
		}
	}
}

// staleListBackend lists the sheds as they were a moment ago, while lookups
// see them as they are now: a listing taken just before a shed was deleted
// and made again under its name.
type staleListBackend struct {
	*userFakeBackend
	listed []config.Shed
}

func (b staleListBackend) ListSheds(context.Context) ([]config.Shed, error) {
	return append([]config.Shed(nil), b.listed...), nil
}

// TestProfileRePushPinsTheListedShed: the re-push after a profile edit acts
// on the sheds its listing found. A shed deleted and made again under the
// name (bob's, with its own profiles) between the listing and the push is
// refused, not given alice's old selection.
func TestProfileRePushPinsTheListedShed(t *testing.T) {
	be := newUserFakeBackend()
	alicesBox := be.sheds["box"]
	alicesBox.EgressProfiles = []string{"shared"}
	bobsBox := config.Shed{Name: "box", User: "bob", Status: config.StatusRunning, CreatedAt: alicesBox.CreatedAt.Add(time.Minute), EgressProfiles: []string{"bobs"}}
	be.sheds["box"] = bobsBox
	s, _ := usersTestServer(t, staleListBackend{userFakeBackend: be, listed: []config.Shed{alicesBox}})

	warnings := s.rePushProfile("shared")
	if got := be.actedSnapshot(); len(got) != 0 {
		t.Fatalf("the re-push changed bob's shed, made after the listing: %v", got)
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], "not found") {
		t.Errorf("re-push warnings %v, want box not found", warnings)
	}

	// The listed shed itself is pushed.
	be.sheds["box"] = alicesBox
	if warnings := s.rePushProfile("shared"); len(warnings) != 0 {
		t.Errorf("re-push of the listed shed: %v", warnings)
	}
	if got := be.actedSnapshot(); len(got) != 1 || got[0] != "egress-set box" {
		t.Errorf("re-push of the listed shed acted %v", got)
	}
}

// TestProfileRePushContinuesPastAReplacedShed: a refused shed is a warning, and
// the re-push goes on to the next shed the listing found.
func TestProfileRePushContinuesPastAReplacedShed(t *testing.T) {
	be := newUserFakeBackend()
	alicesBox := be.sheds["box"]
	alicesBox.EgressProfiles = []string{"shared"}
	be.sheds["box"] = config.Shed{Name: "box", User: "bob", Status: config.StatusRunning, CreatedAt: alicesBox.CreatedAt.Add(time.Minute), EgressProfiles: []string{"bobs"}}
	next := config.Shed{Name: "next", User: "alice", Status: config.StatusRunning, CreatedAt: alicesBox.CreatedAt, EgressProfiles: []string{"shared"}}
	be.sheds["next"] = next
	// The replaced shed is listed first.
	s, _ := usersTestServer(t, staleListBackend{userFakeBackend: be, listed: []config.Shed{alicesBox, next}})

	warnings := s.rePushProfile("shared")
	if len(warnings) != 1 || !strings.HasPrefix(warnings[0], "box: ") || !strings.Contains(warnings[0], "not found") {
		t.Errorf("re-push warnings %v, want only box not found", warnings)
	}
	if got := be.actedSnapshot(); len(got) != 1 || got[0] != "egress-set next" {
		t.Errorf("re-push acted %v, want the next shed pushed", got)
	}
}
