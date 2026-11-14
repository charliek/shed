package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/charliek/shed/internal/backend"
	"github.com/charliek/shed/internal/config"
)

// asyncFakeBackend is a fake remote backend: create, start and delete save
// the new status and return, and a "job" goroutine finishes the work when the
// test releases it. It implements backend.AsyncLifecycle,
// CapabilityReporter and NetworkProfiles, as the remote backend does.
type asyncFakeBackend struct {
	routesFakeBackend
	mu    sync.Mutex
	sheds map[string]config.Shed
	calls []string
	// createErr is returned by CreateShed when set (a fast refusal).
	createErr error
	// release ends the in-flight job; the job then applies final.
	release chan struct{}
	// final is the status (and reason) a create job settles on.
	final, finalReason string
	// jobCtx is the context the last lifecycle call received.
	jobCtx context.Context
	// settled is closed when a job ends.
	settled chan struct{}
}

func newAsyncFakeBackend(sheds ...config.Shed) *asyncFakeBackend {
	b := &asyncFakeBackend{
		sheds:   map[string]config.Shed{},
		release: make(chan struct{}),
		settled: make(chan struct{}),
		final:   config.StatusRunning,
	}
	for _, s := range sheds {
		b.sheds[s.Name] = s
	}
	return b
}

func (b *asyncFakeBackend) Type() backend.Type { return backend.TypeRemote }

func (b *asyncFakeBackend) Capabilities() backend.Capabilities { return backend.RemoteCapabilities() }

func (b *asyncFakeBackend) SupportsNetworkProfile(p string) bool {
	return p == config.NetworkProfileOpen || p == config.NetworkProfileIsolated
}

func (b *asyncFakeBackend) record(call string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.calls = append(b.calls, call)
}

func (b *asyncFakeBackend) callCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.calls)
}

func (b *asyncFakeBackend) ListSheds(context.Context) ([]config.Shed, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]config.Shed, 0, len(b.sheds))
	for _, s := range b.sheds {
		out = append(out, s)
	}
	return out, nil
}

func (b *asyncFakeBackend) GetShed(ctx context.Context, name string) (*config.Shed, error) {
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

// runJob finishes a lifecycle call once the test releases it.
func (b *asyncFakeBackend) runJob(name string, apply func(s *config.Shed) bool) {
	go func() {
		<-b.release
		b.mu.Lock()
		s, ok := b.sheds[name]
		if ok {
			if apply(&s) {
				b.sheds[name] = s
			} else {
				delete(b.sheds, name)
			}
		}
		b.mu.Unlock()
		close(b.settled)
	}()
}

func (b *asyncFakeBackend) CreateShed(ctx context.Context, req config.CreateShedRequest) (*config.Shed, error) {
	b.record("create " + req.Name)
	if b.createErr != nil {
		return nil, b.createErr
	}
	b.mu.Lock()
	b.jobCtx = ctx
	s := config.Shed{
		Name: req.Name, ID: req.ID, Owner: req.Owner, Status: config.StatusStarting,
		Backend: config.BackendRemote, CPUs: req.CPUs, MemoryMB: req.MemoryMB, NetworkProfile: req.Network,
	}
	b.sheds[req.Name] = s
	b.mu.Unlock()
	b.runJob(req.Name, func(s *config.Shed) bool {
		s.Status, s.StatusReason = b.final, b.finalReason
		return true
	})
	return &s, nil
}

func (b *asyncFakeBackend) StartShed(ctx context.Context, name string) (*config.Shed, error) {
	b.record("start " + name)
	b.mu.Lock()
	defer b.mu.Unlock()
	b.jobCtx = ctx
	s, ok := b.sheds[name]
	if !ok {
		return nil, config.ErrShedNotFoundSentinel
	}
	s.Status = config.StatusStarting
	b.sheds[name] = s
	return &s, nil
}

func (b *asyncFakeBackend) DeleteShed(ctx context.Context, name string) error {
	b.record("delete " + name)
	b.mu.Lock()
	b.jobCtx = ctx
	if _, ok := b.sheds[name]; !ok {
		b.mu.Unlock()
		return config.ErrShedNotFoundSentinel
	}
	b.mu.Unlock()
	b.runJob(name, func(*config.Shed) bool { return false })
	return nil
}

func (b *asyncFakeBackend) WaitSettled(ctx context.Context, name string) (*config.Shed, error) {
	backend.Status(ctx, "waiting for "+name)
	select {
	case <-b.settled:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	s, ok := b.sheds[name]
	if !ok {
		return nil, config.ErrShedNotFoundSentinel
	}
	return &s, nil
}

// remoteTestServer is an mtls server with default_backend: remote, two sizes
// and a service client with the given policy.
func remoteTestServer(t *testing.T, b backend.Backend, policy config.ServiceClientPolicy) http.Handler {
	t.Helper()
	cfg := &config.ServerConfig{
		Name:           "test-server",
		DefaultBackend: config.BackendRemote,
		Remote: &config.RemoteConfig{
			Environment:  "test",
			DefaultImage: "family/shed-test",
			ImageAliases: map[string]string{"cloud": "family/shed-test"},
			DefaultSize:  "standard",
			Sizes: map[string]config.RemoteSizeConfig{
				"standard": {MachineType: "m-2", CPUs: 2, MemoryMB: 8192},
				"large":    {MachineType: "m-8", CPUs: 8, MemoryMB: 32768},
			},
		},
		Auth: &config.AuthConfig{
			Mode:    config.AuthModeMTLS,
			Clients: map[string]config.ServiceClientConfig{testClientName: {Policy: policy}},
		},
	}
	s := NewServer(b, cfg, "", nil, nil)
	s.SetClientCertAuthorizer(func(fp string) bool { return fp == testAdminFP })
	return s.Router()
}

func remotePolicy() config.ServiceClientPolicy {
	return config.ServiceClientPolicy{
		NamePrefix: "nocturn-",
		MaxSheds:   2,
		MaxAge:     config.Duration(30 * time.Hour),
		Images:     []string{"cloud"},
		Network:    config.NetworkProfileIsolated,
		Caps:       config.ServiceClientCaps{CPUs: 4, MemoryMB: 16384},
	}
}

// doCtx is do with a request context the test controls.
func doCtx(ctx context.Context, h http.Handler, method, path, body string, hdr ...string) apiResp {
	req := httptest.NewRequest(method, path, strings.NewReader(body)).WithContext(ctx)
	req.Header.Set("Content-Type", "application/json")
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	withClientCert(req, adminLeaf())
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return apiResp{rec.Code, rec.Body.Bytes(), rec.Header()}
}

// sseEvents returns the event names of an SSE body, in order, and the data
// of the last one.
func sseEvents(t *testing.T, body []byte) ([]string, string) {
	t.Helper()
	var names []string
	var last string
	for _, line := range strings.Split(string(body), "\n") {
		if n, ok := strings.CutPrefix(line, "event: "); ok {
			names = append(names, n)
		}
		if d, ok := strings.CutPrefix(line, "data: "); ok {
			last = d
		}
	}
	return names, last
}

func TestAsyncCreateJSON(t *testing.T) {
	b := newAsyncFakeBackend()
	h := remoteTestServer(t, b, remotePolicy())
	ctx, cancel := context.WithCancel(context.Background())
	resp := doCtx(ctx, h, http.MethodPost, "/api/sheds", `{"name":"a1"}`)
	cancel() // the request ends; the job must not

	if resp.code != http.StatusCreated {
		t.Fatalf("status = %d, want 201: %s", resp.code, resp.body)
	}
	var sh config.Shed
	if err := json.Unmarshal(resp.body, &sh); err != nil {
		t.Fatal(err)
	}
	if sh.Status != config.StatusStarting {
		t.Errorf("status = %q, want starting", sh.Status)
	}
	if !config.ValidShedID(sh.ID) {
		t.Errorf("id = %q, not a minted shed id", sh.ID)
	}
	if sh.CPUs != 2 || sh.MemoryMB != 8192 {
		t.Errorf("size = %d/%d, want the default size 2/8192", sh.CPUs, sh.MemoryMB)
	}
	if err := b.jobCtx.Err(); err != nil {
		t.Errorf("job context cancelled with the request: %v", err)
	}
	close(b.release)
	<-b.settled
	got, _ := b.GetShed(context.Background(), "a1")
	if got.Status != config.StatusRunning {
		t.Errorf("after the job: %q, want running", got.Status)
	}
}

func TestAsyncCreateSSE(t *testing.T) {
	tests := []struct {
		name, final, reason string
		wantEvent, wantCode string
	}{
		{name: "runs", final: config.StatusRunning, wantEvent: "complete"},
		{name: "capacity", final: config.StatusError, reason: config.StatusReasonCapacity, wantEvent: "error", wantCode: config.ErrZoneCapacityUnavailable},
		{name: "quota", final: config.StatusError, reason: config.StatusReasonQuota, wantEvent: "error", wantCode: config.ErrCloudQuotaExceeded},
		{name: "create failed", final: config.StatusError, reason: config.StatusReasonCreateFailed, wantEvent: "error", wantCode: config.ErrBackendError},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b := newAsyncFakeBackend()
			b.final, b.finalReason = tt.final, tt.reason
			h := remoteTestServer(t, b, remotePolicy())
			close(b.release)
			resp := doCtx(context.Background(), h, http.MethodPost, "/api/sheds", `{"name":"a1"}`, "Accept", "text/event-stream")
			names, data := sseEvents(t, resp.body)
			if len(names) == 0 || names[len(names)-1] != tt.wantEvent {
				t.Fatalf("events = %v, want last %q; body %s", names, tt.wantEvent, resp.body)
			}
			if names[0] != "progress" {
				t.Errorf("events = %v, want WaitSettled's progress first", names)
			}
			if tt.wantEvent == "complete" {
				var sh config.Shed
				if err := json.Unmarshal([]byte(data), &sh); err != nil {
					t.Fatal(err)
				}
				if sh.Status != config.StatusRunning {
					t.Errorf("complete carries %q, want running", sh.Status)
				}
				return
			}
			var e config.APIError
			if err := json.Unmarshal([]byte(data), &e); err != nil {
				t.Fatal(err)
			}
			if e.Error.Code != tt.wantCode {
				t.Errorf("code = %q, want %q", e.Error.Code, tt.wantCode)
			}
		})
	}
}

func TestAsyncCreateSSEDisconnectLeavesJob(t *testing.T) {
	b := newAsyncFakeBackend()
	h := remoteTestServer(t, b, remotePolicy())
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan apiResp, 1)
	go func() {
		done <- doCtx(ctx, h, http.MethodPost, "/api/sheds", `{"name":"a1"}`, "Accept", "text/event-stream")
	}()
	// Wait until the create has been accepted, then disconnect.
	for i := 0; b.callCount() == 0; i++ {
		if i > 200 {
			t.Fatal("create never reached the backend")
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("handler did not return after the disconnect")
	}
	b.mu.Lock()
	jobCtx := b.jobCtx
	b.mu.Unlock()
	if err := jobCtx.Err(); err != nil {
		t.Errorf("job context cancelled by the disconnect: %v", err)
	}
	close(b.release)
	<-b.settled
	got, _ := b.GetShed(context.Background(), "a1")
	if got.Status != config.StatusRunning {
		t.Errorf("job did not finish: %q", got.Status)
	}
}

func TestAsyncStartAndDelete(t *testing.T) {
	t.Run("start outlives the request", func(t *testing.T) {
		b := newAsyncFakeBackend(config.Shed{Name: "a1", Status: config.StatusStopped})
		h := remoteTestServer(t, b, remotePolicy())
		ctx, cancel := context.WithCancel(context.Background())
		resp := doCtx(ctx, h, http.MethodPost, "/api/sheds/a1/start", "")
		cancel()
		if resp.code != http.StatusOK {
			t.Fatalf("status = %d: %s", resp.code, resp.body)
		}
		if err := b.jobCtx.Err(); err != nil {
			t.Errorf("start context cancelled with the request: %v", err)
		}
	})
	t.Run("plain delete returns 204 at once", func(t *testing.T) {
		b := newAsyncFakeBackend(config.Shed{Name: "a1", Status: config.StatusRunning})
		h := remoteTestServer(t, b, remotePolicy())
		resp := doCtx(context.Background(), h, http.MethodDelete, "/api/sheds/a1", "")
		if resp.code != http.StatusNoContent {
			t.Fatalf("status = %d: %s", resp.code, resp.body)
		}
		select {
		case <-b.settled:
			t.Fatal("delete job finished before release; the handler must not wait")
		default:
		}
		close(b.release)
	})
	t.Run("SSE delete waits until gone", func(t *testing.T) {
		b := newAsyncFakeBackend(config.Shed{Name: "a1", Status: config.StatusRunning})
		h := remoteTestServer(t, b, remotePolicy())
		done := make(chan apiResp, 1)
		go func() {
			done <- doCtx(context.Background(), h, http.MethodDelete, "/api/sheds/a1", "", "Accept", "text/event-stream")
		}()
		select {
		case <-done:
			t.Fatal("SSE delete returned before the job ended")
		case <-time.After(50 * time.Millisecond):
		}
		close(b.release)
		resp := <-done
		names, _ := sseEvents(t, resp.body)
		if len(names) == 0 || names[len(names)-1] != "complete" {
			t.Fatalf("events = %v: %s", names, resp.body)
		}
	})
}

func TestAsyncErrorMapping(t *testing.T) {
	tests := []struct {
		err      error
		status   int
		wantCode string
	}{
		{config.ErrServerLimitReachedSentinel, http.StatusServiceUnavailable, config.ErrServerLimitReached},
		{config.ErrCloudQuotaExceededSentinel, http.StatusServiceUnavailable, config.ErrCloudQuotaExceeded},
		{config.ErrZoneCapacityUnavailableSentinel, http.StatusServiceUnavailable, config.ErrZoneCapacityUnavailable},
		{config.ErrFirewallCheckFailedSentinel, http.StatusServiceUnavailable, config.ErrFirewallCheckFailed},
		{config.ErrFeatureUnsupportedSentinel, http.StatusNotImplemented, config.ErrFeatureUnsupported},
		{config.ErrShedAlreadyExistsSentinel, http.StatusConflict, config.ErrShedAlreadyExists},
	}
	for _, tt := range tests {
		t.Run(tt.wantCode, func(t *testing.T) {
			b := newAsyncFakeBackend()
			b.createErr = errors.Join(errors.New("wrapped"), tt.err)
			h := remoteTestServer(t, b, remotePolicy())
			resp := doCtx(context.Background(), h, http.MethodPost, "/api/sheds", `{"name":"a1"}`)
			if resp.code != tt.status || resp.errCode(t) != tt.wantCode {
				t.Errorf("got %d %s, want %d %s", resp.code, resp.errCode(t), tt.status, tt.wantCode)
			}
		})
	}
}

func TestAsyncCreateFieldRefusals(t *testing.T) {
	tests := []struct{ name, body string }{
		{"local_dir", `{"name":"a1","local_dir":"/tmp"}`},
		{"add_dirs", `{"name":"a1","add_dirs":["/tmp"]}`},
		{"mounts", `{"name":"a1","mounts":["gh"]}`},
		{"from_snapshot", `{"name":"a1","from_snapshot":"snap"}`},
		{"egress", `{"name":"a1","egress":["github"]}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b := newAsyncFakeBackend()
			h := remoteTestServer(t, b, remotePolicy())
			resp := doCtx(context.Background(), h, http.MethodPost, "/api/sheds", tt.body)
			if resp.code != http.StatusUnprocessableEntity || resp.errCode(t) != config.ErrFeatureUnsupported {
				t.Fatalf("got %d %s, want 422 FEATURE_UNSUPPORTED", resp.code, resp.body)
			}
			if !strings.Contains(resp.errMessage(t), tt.name) {
				t.Errorf("message %q does not name %s", resp.errMessage(t), tt.name)
			}
			if n := b.callCount(); n != 0 {
				t.Errorf("backend calls = %d, want 0", n)
			}
		})
	}
	t.Run("empty mounts and egress off are accepted", func(t *testing.T) {
		b := newAsyncFakeBackend()
		h := remoteTestServer(t, b, remotePolicy())
		resp := doCtx(context.Background(), h, http.MethodPost, "/api/sheds", `{"name":"a1","mounts":[],"egress":["off"],"network":"isolated"}`)
		if resp.code != http.StatusCreated {
			t.Fatalf("got %d %s", resp.code, resp.body)
		}
	})
}

func TestAsyncRouteRefusals(t *testing.T) {
	routes := []struct{ method, path, body string }{
		{http.MethodPost, "/api/snapshots", `{"shed":"a1","name":"s"}`},
		{http.MethodGet, "/api/snapshots/s", ""},
		{http.MethodDelete, "/api/snapshots/s", ""},
		{http.MethodPost, "/api/sheds/a1/reset", ""},
		{http.MethodPost, "/api/images/tag", `{"source":"a","target":"b"}`},
		{http.MethodPost, "/api/images/pull", `{"docker_ref":"a","tag":"b"}`},
		{http.MethodPost, "/api/images/push", `{"source":"a","destination":"b"}`},
		{http.MethodPost, "/api/images/prune", ""},
		{http.MethodDelete, "/api/images?ref=a", ""},
		{http.MethodDelete, "/api/images/a", ""},
		{http.MethodGet, "/api/images/inspect?ref=a", ""},
		{http.MethodPost, "/api/egress/a1", `{"profiles":["x"]}`},
		{http.MethodDelete, "/api/egress/a1", ""},
		{http.MethodPut, "/api/egress/profiles/p", `{}`},
		{http.MethodDelete, "/api/egress/profiles/p", ""},
	}
	for _, rt := range routes {
		t.Run(rt.method+" "+rt.path, func(t *testing.T) {
			b := newAsyncFakeBackend(config.Shed{Name: "a1", Status: config.StatusStopped})
			h := remoteTestServer(t, b, remotePolicy())
			resp := doCtx(context.Background(), h, rt.method, rt.path, rt.body)
			if resp.code != http.StatusNotImplemented || resp.errCode(t) != config.ErrFeatureUnsupported {
				t.Errorf("got %d %s, want 501 FEATURE_UNSUPPORTED", resp.code, resp.body)
			}
			if n := b.callCount(); n != 0 {
				t.Errorf("backend calls = %d, want 0", n)
			}
		})
	}
}

func TestAsyncInfo(t *testing.T) {
	b := newAsyncFakeBackend()
	h := remoteTestServer(t, b, remotePolicy())
	resp := doCtx(context.Background(), h, http.MethodGet, "/api/info", "")
	if resp.code != http.StatusOK {
		t.Fatalf("status %d", resp.code)
	}
	var info config.ServerInfo
	if err := json.Unmarshal(resp.body, &info); err != nil {
		t.Fatal(err)
	}
	if info.Backend != config.BackendRemote || info.DefaultImage != "family/shed-test" || info.Isolation != backend.IsolationTopology {
		t.Errorf("info = backend %q image %q isolation %q", info.Backend, info.DefaultImage, info.Isolation)
	}
	for _, f := range []string{FeatureAsyncLifecycle, FeatureNetworkProfiles, FeatureServiceClients, FeatureHTTPExec} {
		found := false
		for _, g := range info.Features {
			found = found || g == f
		}
		if !found {
			t.Errorf("features %v lack %q", info.Features, f)
		}
	}
}

func TestAsyncClientPolicy(t *testing.T) {
	t.Run("max_sheds counts a starting shed", func(t *testing.T) {
		b := newAsyncFakeBackend(
			config.Shed{Name: "nocturn-1", Owner: testClientName, Status: config.StatusStarting},
			config.Shed{Name: "nocturn-2", Owner: testClientName, Status: config.StatusStopping},
		)
		h := remoteTestServer(t, b, remotePolicy())
		resp := do(h, serviceLeaf(testClientName), http.MethodPost, "/api/sheds", `{"name":"nocturn-3"}`)
		if resp.code != http.StatusForbidden || !strings.Contains(resp.errMessage(t), "max_sheds") {
			t.Fatalf("got %d %s, want 403 max_sheds", resp.code, resp.body)
		}
	})
	t.Run("caps apply to the resolved size", func(t *testing.T) {
		b := newAsyncFakeBackend()
		h := remoteTestServer(t, b, remotePolicy())
		// 3 cpus fits under the cap of 4, but resolves to the 8-cpu size.
		resp := do(h, serviceLeaf(testClientName), http.MethodPost, "/api/sheds", `{"name":"nocturn-1","cpus":3}`)
		if resp.code != http.StatusForbidden || !strings.Contains(resp.errMessage(t), "cpus 8") {
			t.Fatalf("got %d %s, want 403 naming cpus 8", resp.code, resp.body)
		}
	})
	t.Run("no size fits", func(t *testing.T) {
		b := newAsyncFakeBackend()
		h := remoteTestServer(t, b, remotePolicy())
		resp := doCtx(context.Background(), h, http.MethodPost, "/api/sheds", `{"name":"a1","cpus":64}`)
		if resp.code != http.StatusBadRequest || resp.errCode(t) != config.ErrInvalidRequest {
			t.Fatalf("got %d %s, want 400 INVALID_REQUEST", resp.code, resp.body)
		}
	})
	t.Run("client create is isolated, owned and minted", func(t *testing.T) {
		b := newAsyncFakeBackend()
		h := remoteTestServer(t, b, remotePolicy())
		resp := do(h, serviceLeaf(testClientName), http.MethodPost, "/api/sheds", `{"name":"nocturn-1","image":"cloud","network":"isolated","mounts":[]}`)
		if resp.code != http.StatusCreated {
			t.Fatalf("got %d %s", resp.code, resp.body)
		}
		var sh config.Shed
		_ = json.Unmarshal(resp.body, &sh)
		if sh.Owner != testClientName || sh.NetworkProfile != config.NetworkProfileIsolated || sh.Status != config.StatusStarting || sh.ID == "" {
			t.Errorf("shed = %+v", sh)
		}
	})
}

func TestAsyncNotRunningConflicts(t *testing.T) {
	for _, status := range []string{config.StatusStarting, config.StatusStopping} {
		t.Run("exec "+status, func(t *testing.T) {
			b := newAsyncFakeBackend(config.Shed{Name: "a1", Status: status})
			h := remoteTestServer(t, b, remotePolicy())
			resp := doCtx(context.Background(), h, http.MethodPost, "/api/sheds/a1/exec", `{"argv":["true"]}`)
			if resp.code != http.StatusConflict || resp.errCode(t) != config.ErrShedNotRunning {
				t.Errorf("got %d %s, want 409 SHED_NOT_RUNNING", resp.code, resp.body)
			}
		})
	}
}

func (b *asyncFakeBackend) DialService(_ context.Context, name string, _ uint16) (net.Conn, error) {
	return nil, fmt.Errorf("%w: %s is starting", config.ErrShedNotRunningSentinel, name)
}

func TestAsyncConnectNotRunning(t *testing.T) {
	b := newAsyncFakeBackend(config.Shed{Name: "a1", Status: config.StatusStarting})
	h := remoteTestServer(t, b, remotePolicy())
	resp := doCtx(context.Background(), h, http.MethodGet, "/api/sheds/a1/connect/22", "")
	if resp.code != http.StatusConflict || resp.errCode(t) != config.ErrShedNotRunning {
		t.Errorf("got %d %s, want 409 SHED_NOT_RUNNING", resp.code, resp.body)
	}
}
