package api

import (
	"context"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/charliek/shed/internal/backend"
	"github.com/charliek/shed/internal/config"
)

// clientAllowedList is the service-client allowlist as this test states it.
// It must equal the production table (clientAllowedRoutes).
var clientAllowedList = []string{
	"GET /api/info",
	"GET /api/images",
	"GET /api/sheds",
	"POST /api/sheds",
	"GET /api/sheds/{name}",
	"DELETE /api/sheds/{name}",
	"POST /api/sheds/{name}/start",
	"POST /api/sheds/{name}/stop",
	"POST /api/sheds/{name}/exec",
}

// clientForbiddenList is every other registered route, listed by hand. A
// route added to Router() fails TestClientRouteWalk until it is put on one of
// the two lists.
var clientForbiddenList = []string{
	"GET /api/ssh-host-key",
	"GET /api/sessions",
	"GET /api/overview",
	"DELETE /api/images",
	"GET /api/images/inspect",
	"GET /api/images/inspect/{name}",
	"POST /api/images/prune",
	"POST /api/images/pull",
	"POST /api/images/push",
	"POST /api/images/tag",
	"DELETE /api/images/{name}",
	"GET /api/system/df",
	"POST /api/system/prune",
	"GET /api/snapshots",
	"POST /api/snapshots",
	"GET /api/snapshots/{name}",
	"DELETE /api/snapshots/{name}",
	"GET /api/egress/stream",
	"GET /api/egress/profiles",
	"GET /api/egress/profiles/{name}",
	"PUT /api/egress/profiles/{name}",
	"DELETE /api/egress/profiles/{name}",
	"GET /api/egress/{name}",
	"POST /api/egress/{name}",
	"DELETE /api/egress/{name}",
	"GET /api/plugins/listeners",
	"GET /api/plugins/listeners/{namespace}/messages",
	"POST /api/plugins/listeners/{namespace}/respond",
	"GET /api/plugins/sheds",
	"POST /api/sheds/{name}/reset",
	"GET /api/sheds/{name}/sessions",
	"DELETE /api/sheds/{name}/sessions/{session}",
	"GET /api/sheds/{name}/connect/{port}",
}

// --- a backend with owned sheds ----------------------------------------------

type clientFakeBackend struct {
	routesFakeBackend
	mu      sync.Mutex
	sheds   map[string]config.Shed
	created []config.CreateShedRequest
	calls   []string // "delete x", "start x", "stop x", "exec x"
	// createGate, when set, blocks CreateShed until it is closed.
	createGate chan struct{}
	createdHit chan struct{}
	// publishEarly lists the shed (as a real backend persists its metadata)
	// before waiting on createGate.
	publishEarly bool
	// onLock, when set, runs at the start of delete, start, stop and exec,
	// before the owner check.
	onLock func()
}

// Type matches clientTestServer's config (default_backend: firecracker), so a
// client's pinned isolated profile is enforceable.
func (b *clientFakeBackend) Type() backend.Type { return backend.TypeFirecracker }

func newClientFakeBackend(sheds ...config.Shed) *clientFakeBackend {
	b := &clientFakeBackend{sheds: map[string]config.Shed{}}
	for _, s := range sheds {
		b.sheds[s.Name] = s
	}
	return b
}

func (b *clientFakeBackend) record(call string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.calls = append(b.calls, call)
}

func (b *clientFakeBackend) callsSnapshot() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.calls...)
}

func (b *clientFakeBackend) createdSnapshot() []config.CreateShedRequest {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]config.CreateShedRequest(nil), b.created...)
}

// GetShed, like the real backends, checks a required owner (the context of a
// client's request on a {name} route) against the shed it loads.
func (b *clientFakeBackend) GetShed(ctx context.Context, name string) (*config.Shed, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	s, ok := b.sheds[name]
	if !ok {
		return nil, config.ErrShedNotFoundSentinel
	}
	if err := backend.CheckOwner(ctx, name, s.Owner); err != nil {
		return nil, err
	}
	return &s, nil
}

// beforeLocked runs b.onLock (the window where a real backend waits for its
// per-shed lock) and then checks a required owner and creation time against
// the shed as it is now, as the real backends do once they hold the lock.
func (b *clientFakeBackend) beforeLocked(ctx context.Context, name string) error {
	b.mu.Lock()
	hook := b.onLock
	b.mu.Unlock()
	if hook != nil {
		hook()
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if s, ok := b.sheds[name]; ok {
		if err := backend.CheckOwner(ctx, name, s.Owner); err != nil {
			return err
		}
		return backend.CheckCreatedAt(ctx, name, s.CreatedAt)
	}
	return nil
}

func (b *clientFakeBackend) ListSheds(_ context.Context) ([]config.Shed, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]config.Shed, 0, len(b.sheds))
	for _, s := range b.sheds {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (b *clientFakeBackend) CreateShed(_ context.Context, req config.CreateShedRequest) (*config.Shed, error) {
	b.mu.Lock()
	b.created = append(b.created, req)
	gate, hit := b.createGate, b.createdHit
	s := config.Shed{Name: req.Name, Status: config.StatusRunning, Owner: req.Owner, Mounts: config.EffectiveMountSelection(req.ResolvedMounts)}
	if b.publishEarly {
		b.sheds[req.Name] = s
	}
	b.mu.Unlock()
	if hit != nil {
		hit <- struct{}{}
	}
	if gate != nil {
		<-gate
	}
	b.mu.Lock()
	b.sheds[req.Name] = s
	b.mu.Unlock()
	return &s, nil
}

func (b *clientFakeBackend) DeleteShed(ctx context.Context, name string) error {
	if err := b.beforeLocked(ctx, name); err != nil {
		return err
	}
	b.record("delete " + name)
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.sheds, name)
	return nil
}

func (b *clientFakeBackend) StartShed(ctx context.Context, name string) (*config.Shed, error) {
	if err := b.beforeLocked(ctx, name); err != nil {
		return nil, err
	}
	b.record("start " + name)
	return b.GetShed(ctx, name)
}

func (b *clientFakeBackend) StopShed(ctx context.Context, name string) (*config.Shed, error) {
	if err := b.beforeLocked(ctx, name); err != nil {
		return nil, err
	}
	b.record("stop " + name)
	return b.GetShed(ctx, name)
}

func (b *clientFakeBackend) Exec(ctx context.Context, name string, opts backend.ExecOptions) error {
	if err := b.beforeLocked(ctx, name); err != nil {
		return err
	}
	b.record("exec " + name)
	if opts.OnStarted != nil {
		opts.OnStarted()
	}
	_, err := opts.Stdout.Write([]byte("ok"))
	return err
}

// --- a server in mtls mode with one service client ---------------------------

const testClientName = "nocturn"

var testAdminFP = testFingerprint("admin")

func clientTestServer(t *testing.T, b backend.Backend, policy config.ServiceClientPolicy) (*Server, http.Handler) {
	t.Helper()
	cfg := &config.ServerConfig{
		Name:           "test-server",
		DefaultBackend: config.BackendFirecracker,
		Firecracker: &config.FirecrackerConfig{
			DefaultCPUs:     2,
			DefaultMemoryMB: 4096,
			ImageAliases:    map[string]string{"itest": "ghcr.io/x/itest:v1", "other": "ghcr.io/x/other:v1"},
		},
		Mounts: map[string]config.MountConfig{
			"claude": {Source: "/srv/claude", Target: "/home/shed/.claude"},
			"gh":     {Source: "/srv/gh", Target: "/home/shed/.config/gh"},
		},
		Auth: &config.AuthConfig{
			Mode:    config.AuthModeMTLS,
			Clients: map[string]config.ServiceClientConfig{testClientName: {Policy: policy}},
		},
	}
	s := NewServer(b, cfg, "", nil, nil)
	s.SetClientCertAuthorizer(func(fp string) bool { return fp == testAdminFP })
	return s, s.Router()
}

func defaultTestPolicy() config.ServiceClientPolicy {
	return config.ServiceClientPolicy{
		NamePrefix:   "nocturn-",
		MaxSheds:     3,
		MaxAge:       config.Duration(30 * time.Hour),
		ServerMounts: []string{"gh"},
		Images:       []string{"itest"},
		Caps:         config.ServiceClientCaps{CPUs: 4, MemoryMB: 8192},
	}
}

// serviceLeaf is a well-formed (unsigned) service certificate; chain
// verification is the TLS layer's job, the middleware reads the shape.
func serviceLeaf(name string) *x509.Certificate {
	now := time.Now()
	return &x509.Certificate{
		SerialNumber: big.NewInt(0xbeef),
		Subject:      pkix.Name{CommonName: name, OrganizationalUnit: []string{"service"}},
		URIs:         []*url.URL{{Scheme: "shed-client", Host: name}},
		NotBefore:    now.Add(-time.Minute),
		NotAfter:     now.Add(time.Hour),
	}
}

func adminLeaf() *x509.Certificate { return testClientCert(testAdminFP, "control", time.Hour) }

type apiResp struct {
	code   int
	body   []byte
	header http.Header
}

func (r apiResp) errCode(t *testing.T) string {
	t.Helper()
	var e config.APIError
	if err := json.Unmarshal(r.body, &e); err != nil {
		t.Fatalf("not an API error (status %d): %s", r.code, r.body)
	}
	return e.Error.Code
}

func (r apiResp) errMessage(t *testing.T) string {
	t.Helper()
	var e config.APIError
	_ = json.Unmarshal(r.body, &e)
	return e.Error.Message
}

func do(h http.Handler, leaf *x509.Certificate, method, path, body string, hdr ...string) apiResp {
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, rd)
	req.Header.Set("Content-Type", "application/json")
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	if leaf != nil {
		withClientCert(req, leaf)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return apiResp{rec.Code, rec.Body.Bytes(), rec.Header()}
}

// walkedRoutes returns every "METHOD normalized-pattern" Router() registers.
func walkedRoutes(t *testing.T, router chi.Router) []string {
	t.Helper()
	seen := map[string]bool{}
	err := chi.Walk(router, func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		seen[method+" "+normalizeRoutePattern(route)] = true
		return nil
	})
	if err != nil {
		t.Fatalf("chi.Walk: %v", err)
	}
	out := make([]string, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// substitute fills a route pattern's placeholders.
func substitute(pattern string) string {
	r := strings.NewReplacer("{name}", "nocturn-x", "{namespace}", "ssh-agent", "{port}", "22", "{session}", "main")
	return r.Replace(pattern)
}

// TestClientRouteWalk: every registered route is on exactly one of the two
// lists, the allowlist equals the production table, and a service client gets
// 403 FORBIDDEN on every forbidden route (even one naming a shed it owns).
func TestClientRouteWalk(t *testing.T) {
	be := newClientFakeBackend(config.Shed{Name: "nocturn-x", Status: config.StatusRunning, Owner: testClientName})
	s, router := clientTestServer(t, be, defaultTestPolicy())

	allowed := map[string]bool{}
	for _, k := range clientAllowedList {
		allowed[k] = true
	}
	forbidden := map[string]bool{}
	for _, k := range clientForbiddenList {
		if allowed[k] {
			t.Errorf("%s is on both lists", k)
		}
		forbidden[k] = true
	}
	if len(allowed) != len(clientAllowedRoutes) {
		t.Errorf("test allowlist has %d routes, production has %d", len(allowed), len(clientAllowedRoutes))
	}
	for k := range clientAllowedRoutes {
		if !allowed[k] {
			t.Errorf("production allowlist has %s, which the test list does not", k)
		}
	}

	walked := walkedRoutes(t, s.mux)
	walkedSet := map[string]bool{}
	for _, k := range walked {
		walkedSet[k] = true
		if !allowed[k] && !forbidden[k] {
			t.Errorf("route %s is on neither the client allowlist nor the forbidden list: decide whether a service client may reach it", k)
		}
	}
	for k := range allowed {
		if !walkedSet[k] {
			t.Errorf("allowlisted route %s is not registered", k)
		}
	}
	for k := range forbidden {
		if !walkedSet[k] {
			t.Errorf("forbidden-list route %s is not registered (stale list)", k)
		}
	}

	for _, k := range clientForbiddenList {
		method, pattern, _ := strings.Cut(k, " ")
		for _, path := range []string{substitute(pattern), substitute(pattern) + "/"} {
			r := do(router, serviceLeaf(testClientName), method, path, "{}")
			if r.code != http.StatusForbidden || r.errCode(t) != config.ErrForbidden {
				t.Errorf("client %s %s = %d %s, want 403 FORBIDDEN", method, path, r.code, r.body)
			}
		}
	}
	if calls := be.callsSnapshot(); len(calls) != 0 {
		t.Errorf("forbidden routes reached the backend: %v", calls)
	}

	// Unknown paths and wrong methods are forbidden too.
	for _, tc := range [][2]string{
		{"GET", "/api/nope"}, {"PUT", "/api/sheds"}, {"PATCH", "/api/sheds/nocturn-x"},
		{"POST", "/api/sheds/nocturn-x"}, {"GET", "/api/sheds/nocturn-x/exec"}, {"POST", "/api/info"},
		{"GET", "/"}, {"GET", "/api"}, {"GET", "/api/sheds/nocturn-x/start"},
	} {
		r := do(router, serviceLeaf(testClientName), tc[0], tc[1], "")
		if r.code != http.StatusForbidden {
			t.Errorf("client %s %s = %d, want 403", tc[0], tc[1], r.code)
		}
	}
}

// TestClientFindAgreesWithDispatch: whenever the middleware's Find resolves a
// request to an allowlisted route, chi dispatches that same request to that
// same route. (Find runs before routing; this pins that the two agree, for
// every registered route with and without a trailing slash, under every
// method.)
func TestClientFindAgreesWithDispatch(t *testing.T) {
	s := newRoutesTestServer()
	router := s.Router()
	methods := []string{"GET", "POST", "PUT", "DELETE", "PATCH"}
	checked := map[string]bool{}
	for _, k := range walkedRoutes(t, s.mux) {
		_, pattern, _ := strings.Cut(k, " ")
		for _, path := range []string{substitute(pattern), substitute(pattern) + "/", strings.TrimSuffix(substitute(pattern), "/")} {
			for _, m := range methods {
				found := s.mux.Find(chi.NewRouteContext(), m, path)
				key := m + " " + normalizeRoutePattern(found)
				if found == "" || !clientAllowedRoutes[key] {
					continue
				}
				rctx := chi.NewRouteContext()
				req := httptest.NewRequest(m, path, strings.NewReader("{}"))
				req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
				router.ServeHTTP(httptest.NewRecorder(), req)
				if got := normalizeRoutePattern(rctx.RoutePattern()); got != normalizeRoutePattern(found) {
					t.Errorf("%s %s: Find says %q, chi dispatched %q", m, path, found, rctx.RoutePattern())
				}
				checked[key] = true
			}
		}
	}
	// Every allowlisted route was exercised.
	for k := range clientAllowedRoutes {
		if !checked[k] {
			t.Errorf("allowlisted route %s was never resolved by Find", k)
		}
	}
}

func TestClientUnauthorized(t *testing.T) {
	be := newClientFakeBackend()
	_, router := clientTestServer(t, be, defaultTestPolicy())

	unknown := do(router, serviceLeaf("stranger"), "GET", "/api/sheds", "")
	if unknown.code != http.StatusUnauthorized || unknown.header.Get("Connection") != "close" {
		t.Errorf("unknown client = %d (Connection %q), want 401 and close", unknown.code, unknown.header.Get("Connection"))
	}

	malformed := serviceLeaf(testClientName)
	malformed.Subject.OrganizationalUnit = []string{"service", "control"}
	if r := do(router, malformed, "GET", "/api/sheds", ""); r.code != http.StatusUnauthorized {
		t.Errorf("malformed service cert = %d, want 401", r.code)
	}

	// A service cert whose CN is an allowlisted admin fingerprint is still
	// malformed, never an admin.
	posing := serviceLeaf(testClientName)
	posing.Subject.CommonName = testAdminFP
	if r := do(router, posing, "GET", "/api/sheds", ""); r.code != http.StatusUnauthorized {
		t.Errorf("service-shaped cert with an admin CN = %d, want 401", r.code)
	}

	expired := serviceLeaf(testClientName)
	expired.NotAfter = time.Now().Add(-time.Second)
	if r := do(router, expired, "GET", "/api/sheds", ""); r.code != http.StatusUnauthorized {
		t.Errorf("expired service cert = %d, want 401", r.code)
	}
}

func TestClientOwnershipAndListing(t *testing.T) {
	be := newClientFakeBackend(
		config.Shed{Name: "nocturn-mine", Status: config.StatusRunning, Owner: testClientName},
		config.Shed{Name: "nocturn-theirs", Status: config.StatusRunning, Owner: "other"},
		config.Shed{Name: "admin-box", Status: config.StatusRunning},
	)
	_, router := clientTestServer(t, be, defaultTestPolicy())
	client := serviceLeaf(testClientName)

	// The list holds only the client's own shed.
	r := do(router, client, "GET", "/api/sheds", "")
	var list config.ShedsResponse
	if err := json.Unmarshal(r.body, &list); err != nil || r.code != 200 {
		t.Fatalf("list = %d %s", r.code, r.body)
	}
	if len(list.Sheds) != 1 || list.Sheds[0].Name != "nocturn-mine" || list.Sheds[0].Owner != testClientName {
		t.Errorf("client list = %+v, want only nocturn-mine", list.Sheds)
	}

	// Someone else's shed, an admin's shed and a missing shed are the same
	// 404 on every {name} route, and nothing reaches the backend.
	for _, name := range []string{"nocturn-theirs", "admin-box", "nocturn-missing", "Bad_Name"} {
		for _, tc := range [][2]string{
			{"GET", "/api/sheds/" + name}, {"DELETE", "/api/sheds/" + name},
			{"POST", "/api/sheds/" + name + "/start"}, {"POST", "/api/sheds/" + name + "/stop"},
			{"POST", "/api/sheds/" + name + "/exec"},
		} {
			body := ""
			if strings.HasSuffix(tc[1], "/exec") {
				body = `{"argv":["true"]}`
			}
			r := do(router, client, tc[0], tc[1], body)
			if r.code != http.StatusNotFound || r.errCode(t) != config.ErrShedNotFound || r.errMessage(t) != "shed not found: "+name {
				t.Errorf("%s %s = %d %s, want 404 SHED_NOT_FOUND \"shed not found: %s\"", tc[0], tc[1], r.code, r.body, name)
			}
		}
	}
	if calls := be.callsSnapshot(); len(calls) != 0 {
		t.Fatalf("refused requests reached the backend: %v", calls)
	}

	// Its own shed: get, exec, stop, start, delete.
	if r := do(router, client, "GET", "/api/sheds/nocturn-mine", ""); r.code != 200 {
		t.Errorf("get own = %d %s", r.code, r.body)
	}
	if r := do(router, client, "POST", "/api/sheds/nocturn-mine/exec", `{"argv":["echo","ok"]}`); r.code != 200 || !strings.Contains(string(r.body), `"t":"exit"`) {
		t.Errorf("exec own = %d %s", r.code, r.body)
	}
	if r := do(router, client, "POST", "/api/sheds/nocturn-mine/stop", ""); r.code != 200 {
		t.Errorf("stop own = %d %s", r.code, r.body)
	}
	if r := do(router, client, "POST", "/api/sheds/nocturn-mine/start", ""); r.code != 200 {
		t.Errorf("start own = %d %s", r.code, r.body)
	}
	if r := do(router, client, "DELETE", "/api/sheds/nocturn-mine", ""); r.code != http.StatusNoContent {
		t.Errorf("delete own = %d %s", r.code, r.body)
	}
	want := []string{"exec nocturn-mine", "stop nocturn-mine", "start nocturn-mine", "delete nocturn-mine"}
	if got := be.callsSnapshot(); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("backend calls = %v, want %v", got, want)
	}

	// An admin still sees and controls every shed, with owners shown.
	r = do(router, adminLeaf(), "GET", "/api/sheds", "")
	list = config.ShedsResponse{}
	_ = json.Unmarshal(r.body, &list)
	if r.code != 200 || len(list.Sheds) != 2 {
		t.Fatalf("admin list = %d %+v", r.code, list.Sheds)
	}
	for _, sh := range list.Sheds {
		if sh.Name == "nocturn-theirs" && sh.Owner != "other" {
			t.Errorf("admin list lost the owner: %+v", sh)
		}
	}
	if r := do(router, adminLeaf(), "POST", "/api/sheds/nocturn-theirs/exec", `{"argv":["true"]}`); r.code != 200 {
		t.Errorf("admin exec in an owned shed = %d %s", r.code, r.body)
	}
	if r := do(router, adminLeaf(), "GET", "/api/overview", ""); r.code != 200 {
		t.Errorf("admin overview = %d", r.code)
	}
}

// TestClientCreatePolicy: each rule is refused with 403 POLICY_VIOLATION
// naming the client and the rule, before the backend (and before SSE).
func TestClientCreatePolicy(t *testing.T) {
	be := newClientFakeBackend()
	_, router := clientTestServer(t, be, defaultTestPolicy())
	client := serviceLeaf(testClientName)

	cases := map[string]struct{ body, rule string }{
		"no prefix":           {`{"name":"other-box"}`, `must start with "nocturn-"`},
		"local_dir":           {`{"name":"nocturn-a","local_dir":"/etc"}`, "local_dir"},
		"local_dir nope":      {`{"name":"nocturn-a","local_dir":"/no/such/dir"}`, "local_dir"},
		"add_dirs":            {`{"name":"nocturn-a","add_dirs":["/etc"]}`, "add_dirs"},
		"from_snapshot":       {`{"name":"nocturn-a","from_snapshot":"snap"}`, "from_snapshot"},
		"image not owned":     {`{"name":"nocturn-a","image":"other"}`, `image "other"`},
		"image ref":           {`{"name":"nocturn-a","image":"ghcr.io/x/itest:v1"}`, "image"},
		"cpus over cap":       {`{"name":"nocturn-a","cpus":5}`, "cpus 5"},
		"memory over cap":     {`{"name":"nocturn-a","memory_mb":9000}`, "memory_mb 9000"},
		"mount not in policy": {`{"name":"nocturn-a","mounts":["claude"]}`, `mount "claude"`},
		"mount unknown":       {`{"name":"nocturn-a","mounts":["ssh"]}`, `mount "ssh"`},
	}
	for name, tc := range cases {
		for _, sse := range []bool{false, true} {
			var hdr []string
			if sse {
				hdr = []string{"Accept", "text/event-stream"}
			}
			// The forged server-set keys must change nothing.
			body := strings.TrimSuffix(tc.body, "}") + `,"owner":"","Owner":"","ResolvedMounts":{"mode":"all"}}`
			r := do(router, client, "POST", "/api/sheds", body, hdr...)
			if r.code != http.StatusForbidden || r.errCode(t) != config.ErrPolicyViolation {
				t.Errorf("%s (sse=%v) = %d %s, want 403 POLICY_VIOLATION", name, sse, r.code, r.body)
				continue
			}
			if msg := r.errMessage(t); !strings.HasPrefix(msg, "client nocturn: ") || !strings.Contains(msg, tc.rule) {
				t.Errorf("%s: message %q, want it to name the client and %q", name, msg, tc.rule)
			}
		}
	}
	if got := be.createdSnapshot(); len(got) != 0 {
		t.Fatalf("refused creates reached the backend: %+v", got)
	}
}

func TestClientCreateSetsOwnerAndMounts(t *testing.T) {
	be := newClientFakeBackend()
	_, router := clientTestServer(t, be, defaultTestPolicy())
	client := serviceLeaf(testClientName)

	// Forged owner/ResolvedMounts keys are ignored: the owner is the client
	// and the selection is its server_mounts.
	r := do(router, client, "POST", "/api/sheds",
		`{"name":"nocturn-a","image":"itest","cpus":4,"memory_mb":8192,"owner":"someone","Owner":"someone","ResolvedMounts":{"mode":"all"},"egress":["off"],"repo":"https://github.com/x/y","no_provision":true}`)
	if r.code != http.StatusCreated {
		t.Fatalf("create = %d %s", r.code, r.body)
	}
	var shed config.Shed
	_ = json.Unmarshal(r.body, &shed)
	if shed.Owner != testClientName || shed.Mounts == nil || shed.Mounts.Mode != config.MountModeSelected {
		t.Errorf("created shed JSON = %s", r.body)
	}
	// An explicit [] is none.
	if r := do(router, client, "POST", "/api/sheds", `{"name":"nocturn-b","mounts":[]}`); r.code != http.StatusCreated {
		t.Fatalf("create with mounts [] = %d %s", r.code, r.body)
	}
	got := be.createdSnapshot()
	if len(got) != 2 {
		t.Fatalf("backend saw %d creates", len(got))
	}
	if got[0].Owner != testClientName || got[0].ResolvedMounts == nil || got[0].ResolvedMounts.Mode != config.MountModeSelected ||
		strings.Join(got[0].ResolvedMounts.Names, ",") != "gh" {
		t.Errorf("first create: owner=%q mounts=%+v, want nocturn and selected [gh]", got[0].Owner, got[0].ResolvedMounts)
	}
	if got[1].ResolvedMounts == nil || got[1].ResolvedMounts.Mode != config.MountModeSelected || len(got[1].ResolvedMounts.Names) != 0 {
		t.Errorf("mounts [] create: %+v, want selected with no names", got[1].ResolvedMounts)
	}
}

func TestAdminCreateMounts(t *testing.T) {
	be := newClientFakeBackend()
	_, router := clientTestServer(t, be, defaultTestPolicy())
	admin := adminLeaf()

	// A forged owner on an admin create is ignored; omitted mounts is all.
	if r := do(router, admin, "POST", "/api/sheds", `{"name":"box-a","owner":"nocturn","Owner":"nocturn"}`); r.code != http.StatusCreated {
		t.Fatalf("admin create = %d %s", r.code, r.body)
	}
	if r := do(router, admin, "POST", "/api/sheds", `{"name":"box-b","mounts":[]}`); r.code != http.StatusCreated {
		t.Fatalf("admin create mounts [] = %d %s", r.code, r.body)
	}
	if r := do(router, admin, "POST", "/api/sheds", `{"name":"box-c","mounts":["claude"]}`); r.code != http.StatusCreated {
		t.Fatalf("admin create mounts [claude] = %d %s", r.code, r.body)
	}
	r := do(router, admin, "POST", "/api/sheds", `{"name":"box-d","mounts":["nope"]}`)
	if r.code != http.StatusBadRequest || r.errCode(t) != config.ErrInvalidRequest {
		t.Errorf("admin mounts [nope] = %d %s, want 400 INVALID_REQUEST", r.code, r.body)
	}
	got := be.createdSnapshot()
	if len(got) != 3 {
		t.Fatalf("backend saw %d creates, want 3", len(got))
	}
	if got[0].Owner != "" || got[0].ResolvedMounts == nil || got[0].ResolvedMounts.Mode != config.MountModeAll {
		t.Errorf("admin default: owner=%q mounts=%+v, want no owner and all", got[0].Owner, got[0].ResolvedMounts)
	}
	if got[1].ResolvedMounts.Mode != config.MountModeSelected || len(got[1].ResolvedMounts.Names) != 0 {
		t.Errorf("admin mounts []: %+v, want selected none", got[1].ResolvedMounts)
	}
	if got[2].ResolvedMounts.Mode != config.MountModeSelected || strings.Join(got[2].ResolvedMounts.Names, ",") != "claude" {
		t.Errorf("admin mounts [claude]: %+v", got[2].ResolvedMounts)
	}
}

// TestAdminCreateOpenModeUnchanged: in open mode an admin create with no
// mounts field stores "all", exactly as before mount selection.
func TestAdminCreateOpenModeUnchanged(t *testing.T) {
	be := newClientFakeBackend()
	s := NewServer(be, &config.ServerConfig{Name: "t", HTTPPort: 8080}, "", nil, nil)
	r := do(s.Router(), nil, "POST", "/api/sheds", `{"name":"box"}`)
	if r.code != http.StatusCreated {
		t.Fatalf("create = %d %s", r.code, r.body)
	}
	got := be.createdSnapshot()
	if len(got) != 1 || got[0].Owner != "" || got[0].ResolvedMounts == nil || got[0].ResolvedMounts.Mode != config.MountModeAll {
		t.Errorf("open-mode create: %+v", got)
	}
}

func TestClientMaxSheds(t *testing.T) {
	policy := defaultTestPolicy()
	policy.MaxSheds = 2
	be := newClientFakeBackend(
		config.Shed{Name: "nocturn-old", Status: config.StatusStopped, Owner: testClientName},
		config.Shed{Name: "nocturn-theirs", Status: config.StatusRunning, Owner: "other"},
		config.Shed{Name: "admin-box", Status: config.StatusRunning},
	)
	_, router := clientTestServer(t, be, policy)
	client := serviceLeaf(testClientName)

	// One owned (stopped counts); a create in flight takes the second slot.
	be.createGate = make(chan struct{})
	be.createdHit = make(chan struct{}, 1)
	done := make(chan apiResp, 1)
	go func() { done <- do(router, client, "POST", "/api/sheds", `{"name":"nocturn-new"}`) }()
	<-be.createdHit

	r := do(router, client, "POST", "/api/sheds", `{"name":"nocturn-third"}`)
	if r.code != http.StatusForbidden || r.errCode(t) != config.ErrPolicyViolation || !strings.Contains(r.errMessage(t), "max_sheds") {
		t.Errorf("create over max_sheds while one is in flight = %d %s", r.code, r.body)
	}
	close(be.createGate)
	if first := <-done; first.code != http.StatusCreated {
		t.Fatalf("in-flight create = %d %s", first.code, first.body)
	}
	be.mu.Lock()
	be.createGate, be.createdHit = nil, nil
	be.mu.Unlock()

	// Now two are owned: still refused.
	r = do(router, client, "POST", "/api/sheds", `{"name":"nocturn-third"}`)
	if r.code != http.StatusForbidden || !strings.Contains(r.errMessage(t), "max_sheds") {
		t.Errorf("create at max_sheds = %d %s", r.code, r.body)
	}
	// Deleting one frees a slot.
	if r := do(router, client, "DELETE", "/api/sheds/nocturn-old", ""); r.code != http.StatusNoContent {
		t.Fatalf("delete = %d", r.code)
	}
	if r := do(router, client, "POST", "/api/sheds", `{"name":"nocturn-third"}`); r.code != http.StatusCreated {
		t.Errorf("create after a delete = %d %s", r.code, r.body)
	}
}

// swapOnRead is a request body that, on its first Read (after the auth
// middleware has authorized the request), runs swap.
type swapOnRead struct {
	r    io.Reader
	swap func()
	once sync.Once
}

func (b *swapOnRead) Read(p []byte) (int, error) {
	b.once.Do(b.swap)
	return b.r.Read(p)
}

// replaceWithAdminShed is a delete and recreate by an admin under the same
// name: the shed is there, running, with no owner.
func (b *clientFakeBackend) replaceWithAdminShed(name string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.sheds[name] = config.Shed{Name: name, Status: config.StatusRunning}
}

// TestClientOwnershipBoundToTheShedOperatedOn: ownership is checked against
// the shed an operation acts on, not only by the middleware's lookup. A shed
// the client owned when the request was authorized, deleted and recreated by
// an admin under the same name before the operation runs (exec: while the
// client withholds its body; lifecycle: while the operation waits for the
// backend's per-shed lock), is 404 and the admin's shed is not touched.
func TestClientOwnershipBoundToTheShedOperatedOn(t *testing.T) {
	owned := config.Shed{Name: "nocturn-a", Status: config.StatusRunning, Owner: testClientName}
	client := serviceLeaf(testClientName)

	t.Run("exec", func(t *testing.T) {
		be := newClientFakeBackend(owned)
		_, router := clientTestServer(t, be, defaultTestPolicy())
		body := &swapOnRead{r: strings.NewReader(`{"argv":["true"]}`), swap: func() { be.replaceWithAdminShed("nocturn-a") }}
		req := httptest.NewRequest("POST", "/api/sheds/nocturn-a/exec", body)
		req.Header.Set("Content-Type", "application/json")
		withClientCert(req, client)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		r := apiResp{rec.Code, rec.Body.Bytes(), rec.Header()}
		if r.code != http.StatusNotFound || r.errCode(t) != config.ErrShedNotFound {
			t.Errorf("exec into a replaced shed = %d %s, want 404 SHED_NOT_FOUND", r.code, r.body)
		}
		if calls := be.callsSnapshot(); len(calls) != 0 {
			t.Errorf("the replacement was operated on: %v", calls)
		}
	})

	for _, tc := range [][2]string{
		{"DELETE", "/api/sheds/nocturn-a"},
		{"POST", "/api/sheds/nocturn-a/stop"},
		{"POST", "/api/sheds/nocturn-a/start"},
	} {
		t.Run(tc[0]+" "+tc[1], func(t *testing.T) {
			be := newClientFakeBackend(owned)
			be.onLock = func() { be.replaceWithAdminShed("nocturn-a") }
			_, router := clientTestServer(t, be, defaultTestPolicy())
			r := do(router, client, tc[0], tc[1], "")
			if r.code != http.StatusNotFound || r.errCode(t) != config.ErrShedNotFound {
				t.Errorf("%s on a replaced shed = %d %s, want 404 SHED_NOT_FOUND", tc[0], r.code, r.body)
			}
			if calls := be.callsSnapshot(); len(calls) != 0 {
				t.Errorf("the replacement was operated on: %v", calls)
			}
			if sh, err := be.GetShed(context.Background(), "nocturn-a"); err != nil || sh.Owner != "" {
				t.Errorf("the admin's shed = %+v, %v", sh, err)
			}
		})
	}
}

// TestClientMaxShedsCountsAnInFlightCreateOnce: a real backend persists an
// owned shed's metadata before it boots, so a create in flight is listed as
// owned too. It still takes one slot, not two.
func TestClientMaxShedsCountsAnInFlightCreateOnce(t *testing.T) {
	policy := defaultTestPolicy()
	policy.MaxSheds = 2
	be := newClientFakeBackend()
	be.publishEarly = true
	_, router := clientTestServer(t, be, policy)
	client := serviceLeaf(testClientName)

	gate := make(chan struct{})
	be.createGate = gate
	be.createdHit = make(chan struct{}, 1)
	done := make(chan apiResp, 1)
	go func() { done <- do(router, client, "POST", "/api/sheds", `{"name":"nocturn-a"}`) }()
	<-be.createdHit // nocturn-a is listed as owned and still in flight
	be.mu.Lock()
	be.createGate, be.createdHit = nil, nil
	be.mu.Unlock()

	if r := do(router, client, "POST", "/api/sheds", `{"name":"nocturn-b"}`); r.code != http.StatusCreated {
		t.Errorf("second create while the first is listed and in flight = %d %s", r.code, r.body)
	}
	if r := do(router, client, "POST", "/api/sheds", `{"name":"nocturn-c"}`); r.code != http.StatusForbidden || !strings.Contains(r.errMessage(t), "max_sheds") {
		t.Errorf("third create = %d %s", r.code, r.body)
	}
	close(gate)
	if first := <-done; first.code != http.StatusCreated {
		t.Fatalf("first create = %d %s", first.code, first.body)
	}
	// Both done and owned: still at the limit.
	if r := do(router, client, "POST", "/api/sheds", `{"name":"nocturn-c"}`); r.code != http.StatusForbidden {
		t.Errorf("create at max_sheds = %d %s", r.code, r.body)
	}
}

func TestServerFeaturesServiceClients(t *testing.T) {
	_, router := clientTestServer(t, newClientFakeBackend(), defaultTestPolicy())
	r := do(router, serviceLeaf(testClientName), "GET", "/api/info", "")
	var info config.ServerInfo
	if err := json.Unmarshal(r.body, &info); err != nil || r.code != 200 {
		t.Fatalf("info = %d %s", r.code, r.body)
	}
	for _, f := range []string{FeatureHTTPExec, FeatureMountSelection, FeatureServiceClients} {
		found := false
		for _, g := range info.Features {
			found = found || g == f
		}
		if !found {
			t.Errorf("features %v lack %s", info.Features, f)
		}
	}
	// Without clients: mount_selection, but no service_clients.
	plain := NewServer(newClientFakeBackend(), &config.ServerConfig{Name: "t", HTTPPort: 8080}, "", nil, nil)
	feats := strings.Join(plain.serverFeatures(), ",")
	if !strings.Contains(feats, FeatureMountSelection) || strings.Contains(feats, FeatureServiceClients) {
		t.Errorf("open-mode features = %s", feats)
	}
}
