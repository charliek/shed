package api

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/charliek/shed/internal/backend"
	"github.com/charliek/shed/internal/config"
)

// fakeBackend implements backend.Backend for system handler tests.
// Only DiskUsage and Prune are wired; other methods panic if called.
type fakeBackend struct {
	usage       config.DiskUsage
	err         error
	pruneReport config.PruneReport
	pruneErr    error
	// captured so the test can assert what the handler forwarded.
	lastPruneOpts backend.PruneOptions
}

func (f *fakeBackend) Type() backend.Type { return backend.TypeVZ }
func (f *fakeBackend) Close() error       { return nil }

func (f *fakeBackend) CreateShed(ctx context.Context, req config.CreateShedRequest) (*config.Shed, error) {
	panic("unexpected")
}
func (f *fakeBackend) GetShed(ctx context.Context, name string) (*config.Shed, error) {
	panic("unexpected")
}
func (f *fakeBackend) ListSheds(ctx context.Context) ([]config.Shed, error) { panic("unexpected") }
func (f *fakeBackend) DeleteShed(ctx context.Context, name string, keepVolume bool) error {
	panic("unexpected")
}
func (f *fakeBackend) StartShed(ctx context.Context, name string) (*config.Shed, error) {
	panic("unexpected")
}
func (f *fakeBackend) StopShed(ctx context.Context, name string) (*config.Shed, error) {
	panic("unexpected")
}
func (f *fakeBackend) ResetShed(ctx context.Context, name string) (*config.Shed, error) {
	panic("unexpected")
}
func (f *fakeBackend) ListSessions(ctx context.Context, shedName string) ([]config.Session, error) {
	panic("unexpected")
}
func (f *fakeBackend) KillSession(ctx context.Context, shedName, sessionName string) error {
	panic("unexpected")
}
func (f *fakeBackend) Exec(ctx context.Context, shedName string, opts backend.ExecOptions) error {
	panic("unexpected")
}
func (f *fakeBackend) GetNetworkEndpoint(ctx context.Context, shedName string) (string, error) {
	panic("unexpected")
}
func (f *fakeBackend) DialService(ctx context.Context, shedName string, port uint16) (net.Conn, error) {
	panic("unexpected")
}
func (f *fakeBackend) ListImages(ctx context.Context) ([]config.ImageInfo, error) {
	panic("unexpected")
}
func (f *fakeBackend) InspectImage(ctx context.Context, _ string) (config.ImageInspectResponse, error) {
	panic("unexpected")
}
func (f *fakeBackend) TagImage(ctx context.Context, _, _ string) error { panic("unexpected") }
func (f *fakeBackend) PullImage(ctx context.Context, _, _ string) (string, error) {
	panic("unexpected")
}
func (f *fakeBackend) DeleteImage(ctx context.Context, name string) error { panic("unexpected") }
func (f *fakeBackend) PruneImages(ctx context.Context, dryRun bool) ([]config.ImageInfo, error) {
	panic("unexpected")
}

func (f *fakeBackend) DiskUsage(ctx context.Context) (config.DiskUsage, error) {
	return f.usage, f.err
}

func (f *fakeBackend) Prune(ctx context.Context, opts backend.PruneOptions) (config.PruneReport, error) {
	f.lastPruneOpts = opts
	return f.pruneReport, f.pruneErr
}

func (f *fakeBackend) CreateSnapshot(ctx context.Context, req config.SnapshotCreateRequest) (*config.Snapshot, error) {
	panic("unexpected")
}
func (f *fakeBackend) ListSnapshots(ctx context.Context) ([]config.Snapshot, error) {
	panic("unexpected")
}
func (f *fakeBackend) GetSnapshot(ctx context.Context, name string) (*config.Snapshot, error) {
	panic("unexpected")
}
func (f *fakeBackend) DeleteSnapshot(ctx context.Context, name string) error { panic("unexpected") }

func newSystemTestServer(be backend.Backend) *Server {
	return NewServer(be, &config.ServerConfig{Name: "test-server"}, "", nil, nil)
}

func TestHandleSystemDF_Success(t *testing.T) {
	be := &fakeBackend{
		usage: config.DiskUsage{
			ServerName:  "test-server",
			Backend:     "vz",
			GeneratedAt: time.Now().UTC(),
			Images:      []config.ImageDiskEntry{{Name: "default", Size: config.DiskSize{LogicalBytes: 1024, PhysicalBytes: 512}}},
		},
	}
	srv := newSystemTestServer(be)

	r := httptest.NewRequest(http.MethodGet, "/api/system/df", nil)
	w := httptest.NewRecorder()
	srv.Router().ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
	}
	var got config.DiskUsage
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.ServerName != "test-server" || got.Backend != "vz" {
		t.Errorf("unexpected payload: %+v", got)
	}
	if len(got.Images) != 1 || got.Images[0].Name != "default" {
		t.Errorf("images = %+v", got.Images)
	}
}

func TestHandleSystemDF_EmptySlicesNotNull(t *testing.T) {
	// Verify JSON renders empty slices as `[]`, not `null`, so callers using
	// `jq -e '.images | length'` don't blow up.
	be := &fakeBackend{
		usage: config.DiskUsage{ServerName: "x", Backend: "vz"},
	}
	srv := newSystemTestServer(be)

	r := httptest.NewRequest(http.MethodGet, "/api/system/df", nil)
	w := httptest.NewRecorder()
	srv.Router().ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	// Raw-string contains check: `"images":[]` rather than `"images":null`.
	body := w.Body.String()
	for _, want := range []string{`"images":[]`, `"sheds":[]`, `"orphans":[]`} {
		if !jsonContains(body, want) {
			t.Errorf("expected %s in body, got: %s", want, body)
		}
	}
}

func TestHandleSystemDF_BackendError(t *testing.T) {
	be := &fakeBackend{err: errors.New("io boom")}
	srv := newSystemTestServer(be)

	r := httptest.NewRequest(http.MethodGet, "/api/system/df", nil)
	w := httptest.NewRecorder()
	srv.Router().ServeHTTP(w, r)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500: %s", w.Code, w.Body.String())
	}
}

func TestHandleSystemPrune_ParsesQueryParams(t *testing.T) {
	be := &fakeBackend{
		pruneReport: config.PruneReport{DryRun: true, ServerName: "x"},
	}
	srv := newSystemTestServer(be)

	r := httptest.NewRequest(http.MethodPost, "/api/system/prune?scope=images&scope=instances&dry_run=true&until=72h&log_tail_bytes=1024", nil)
	w := httptest.NewRecorder()
	srv.Router().ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
	}
	opts := be.lastPruneOpts
	if !opts.Images || !opts.Instances {
		t.Errorf("scopes parsed as %+v, want Images+Instances", opts)
	}
	if opts.Logs || opts.Orphans {
		t.Errorf("unexpected scope enabled: %+v", opts)
	}
	if !opts.DryRun {
		t.Errorf("DryRun should be true")
	}
	if opts.Until != 72*time.Hour {
		t.Errorf("Until = %v, want 72h", opts.Until)
	}
	if opts.LogTailBytes != 1024 {
		t.Errorf("LogTailBytes = %d, want 1024", opts.LogTailBytes)
	}
}

func TestHandleSystemPrune_UnknownScope(t *testing.T) {
	be := &fakeBackend{}
	srv := newSystemTestServer(be)

	r := httptest.NewRequest(http.MethodPost, "/api/system/prune?scope=bogus", nil)
	w := httptest.NewRecorder()
	srv.Router().ServeHTTP(w, r)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", w.Code, w.Body.String())
	}
	if !jsonContains(w.Body.String(), "unknown scope") {
		t.Errorf("expected 'unknown scope' in error body, got %s", w.Body.String())
	}
}

func TestHandleSystemPrune_InvalidUntil(t *testing.T) {
	be := &fakeBackend{}
	srv := newSystemTestServer(be)

	r := httptest.NewRequest(http.MethodPost, "/api/system/prune?until=not-a-duration", nil)
	w := httptest.NewRecorder()
	srv.Router().ServeHTTP(w, r)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
}

func TestHandleSystemPrune_InvalidDryRun(t *testing.T) {
	be := &fakeBackend{}
	srv := newSystemTestServer(be)

	r := httptest.NewRequest(http.MethodPost, "/api/system/prune?dry_run=maybe", nil)
	w := httptest.NewRecorder()
	srv.Router().ServeHTTP(w, r)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
}

func TestHandleSystemPrune_DefaultUntil_72h(t *testing.T) {
	// Regression for Codex #2: omitting `until` must default to 72h. The
	// previous handler left opts.Until=0, which the backend interprets
	// as "any age" — directly calling POST /api/system/prune?scope=instances
	// would then delete every stopped instance regardless of how recent.
	be := &fakeBackend{pruneReport: config.PruneReport{ServerName: "x"}}
	srv := newSystemTestServer(be)

	r := httptest.NewRequest(http.MethodPost, "/api/system/prune?scope=instances", nil)
	w := httptest.NewRecorder()
	srv.Router().ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if got, want := be.lastPruneOpts.Until, 72*time.Hour; got != want {
		t.Errorf("default Until = %v, want %v", got, want)
	}
}

func TestHandleSystemPrune_ExplicitUntilZero_AnyAge(t *testing.T) {
	// Explicit until=0s must preserve the "any age" escape hatch.
	be := &fakeBackend{pruneReport: config.PruneReport{ServerName: "x"}}
	srv := newSystemTestServer(be)

	r := httptest.NewRequest(http.MethodPost, "/api/system/prune?scope=instances&until=0s", nil)
	w := httptest.NewRecorder()
	srv.Router().ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if got := be.lastPruneOpts.Until; got != 0 {
		t.Errorf("explicit until=0s: got %v, want 0", got)
	}
}

func TestHandleSystemPrune_LogTailCap(t *testing.T) {
	// Regression for Codex #8: unbounded log_tail_bytes would allow a
	// client to allocate arbitrary memory on the daemon. Cap is 64 MiB.
	be := &fakeBackend{}
	srv := newSystemTestServer(be)

	r := httptest.NewRequest(http.MethodPost, "/api/system/prune?log_tail_bytes=1073741824", nil) // 1 GiB
	w := httptest.NewRecorder()
	srv.Router().ServeHTTP(w, r)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", w.Code, w.Body.String())
	}
}

func TestHandleSystemPrune_UsesInvalidRequestErrCode(t *testing.T) {
	// Regression for CR finding B: 400 responses must use the dedicated
	// ErrInvalidRequest code so clients switching on the error code field
	// can distinguish bad input from backend errors.
	be := &fakeBackend{}
	srv := newSystemTestServer(be)

	r := httptest.NewRequest(http.MethodPost, "/api/system/prune?scope=nonsense", nil)
	w := httptest.NewRecorder()
	srv.Router().ServeHTTP(w, r)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
	if !jsonContains(w.Body.String(), `"code":"`+config.ErrInvalidRequest+`"`) {
		t.Errorf("expected error code %s in body, got: %s", config.ErrInvalidRequest, w.Body.String())
	}
}

func TestHandleSystemPrune_EmptyScope(t *testing.T) {
	// No scope flags — backend gets PruneOptions with all flags false and
	// applies its own defaults.
	be := &fakeBackend{
		pruneReport: config.PruneReport{ServerName: "x"},
	}
	srv := newSystemTestServer(be)

	r := httptest.NewRequest(http.MethodPost, "/api/system/prune", nil)
	w := httptest.NewRecorder()
	srv.Router().ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	opts := be.lastPruneOpts
	if opts.Images || opts.Instances || opts.Logs || opts.Orphans {
		t.Errorf("expected all scope flags false (server applies defaults), got %+v", opts)
	}
}

// jsonContains is a substring check that ignores insignificant whitespace by
// stripping spaces/newlines from both the haystack and needle.
func jsonContains(body, substr string) bool {
	clean := func(s string) string {
		out := make([]byte, 0, len(s))
		for i := 0; i < len(s); i++ {
			c := s[i]
			if c == ' ' || c == '\n' || c == '\t' {
				continue
			}
			out = append(out, c)
		}
		return string(out)
	}
	return contains(clean(body), clean(substr))
}

func contains(haystack, needle string) bool {
	return len(needle) == 0 || indexOf(haystack, needle) >= 0
}

func indexOf(s, sub string) int {
outer:
	for i := 0; i+len(sub) <= len(s); i++ {
		for j := 0; j < len(sub); j++ {
			if s[i+j] != sub[j] {
				continue outer
			}
		}
		return i
	}
	return -1
}
