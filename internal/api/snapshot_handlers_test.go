package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/charliek/shed/internal/backend"
	"github.com/charliek/shed/internal/config"
)

// snapshotFakeBackend is a minimal backend.Backend that exercises only the
// snapshot-related routes. Other methods panic so accidental coupling shows up.
//
// CreateShed is overridable via createShedFn for tests that need to exercise
// the API handler's create path (e.g., the from-snapshot mutual-exclusion case
// after the API-layer mutex check moved to the backend sentinel).
type snapshotFakeBackend struct {
	listResp     []config.Snapshot
	listErr      error
	createResp   *config.Snapshot
	createErr    error
	getResp      *config.Snapshot
	getErr       error
	deleteErr    error
	createCalled config.SnapshotCreateRequest
	createShedFn func(context.Context, config.CreateShedRequest) (*config.Shed, error)
}

func (f *snapshotFakeBackend) Type() backend.Type { return backend.TypeVZ }
func (f *snapshotFakeBackend) Close() error       { return nil }
func (f *snapshotFakeBackend) CreateShed(ctx context.Context, req config.CreateShedRequest) (*config.Shed, error) {
	if f.createShedFn != nil {
		return f.createShedFn(ctx, req)
	}
	panic("unexpected")
}
func (f *snapshotFakeBackend) GetShed(_ context.Context, _ string) (*config.Shed, error) {
	panic("unexpected")
}
func (f *snapshotFakeBackend) ListSheds(_ context.Context) ([]config.Shed, error) {
	panic("unexpected")
}
func (f *snapshotFakeBackend) DeleteShed(_ context.Context, _ string, _ bool) error {
	panic("unexpected")
}
func (f *snapshotFakeBackend) StartShed(_ context.Context, _ string) (*config.Shed, error) {
	panic("unexpected")
}
func (f *snapshotFakeBackend) StopShed(_ context.Context, _ string) (*config.Shed, error) {
	panic("unexpected")
}
func (f *snapshotFakeBackend) ResetShed(_ context.Context, _ string) (*config.Shed, error) {
	panic("unexpected")
}
func (f *snapshotFakeBackend) ListSessions(_ context.Context, _ string) ([]config.Session, error) {
	panic("unexpected")
}
func (f *snapshotFakeBackend) KillSession(_ context.Context, _, _ string) error { panic("unexpected") }
func (f *snapshotFakeBackend) Exec(_ context.Context, _ string, _ backend.ExecOptions) error {
	panic("unexpected")
}
func (f *snapshotFakeBackend) GetNetworkEndpoint(_ context.Context, _ string) (string, error) {
	panic("unexpected")
}
func (f *snapshotFakeBackend) DialService(_ context.Context, _ string, _ uint16) (net.Conn, error) {
	panic("unexpected")
}
func (f *snapshotFakeBackend) ListImages(_ context.Context) ([]config.ImageInfo, error) {
	panic("unexpected")
}
func (f *snapshotFakeBackend) InspectImage(_ context.Context, _ string) (config.ImageInspectResponse, error) {
	panic("unexpected")
}
func (f *snapshotFakeBackend) TagImage(_ context.Context, _, _ string) error { panic("unexpected") }
func (f *snapshotFakeBackend) PullImage(_ context.Context, _, _, _ string) (string, error) {
	panic("unexpected")
}
func (f *snapshotFakeBackend) PushImage(_ context.Context, _, _ string) error {
	panic("unexpected")
}
func (f *snapshotFakeBackend) DeleteImage(_ context.Context, _ string) error { panic("unexpected") }
func (f *snapshotFakeBackend) PruneImages(_ context.Context, _ bool) ([]config.ImageInfo, error) {
	panic("unexpected")
}
func (f *snapshotFakeBackend) DiskUsage(_ context.Context) (config.DiskUsage, error) {
	panic("unexpected")
}
func (f *snapshotFakeBackend) Prune(_ context.Context, _ backend.PruneOptions) (config.PruneReport, error) {
	panic("unexpected")
}
func (f *snapshotFakeBackend) ListSnapshots(_ context.Context) ([]config.Snapshot, error) {
	return f.listResp, f.listErr
}
func (f *snapshotFakeBackend) CreateSnapshot(_ context.Context, req config.SnapshotCreateRequest) (*config.Snapshot, error) {
	f.createCalled = req
	return f.createResp, f.createErr
}
func (f *snapshotFakeBackend) GetSnapshot(_ context.Context, _ string) (*config.Snapshot, error) {
	return f.getResp, f.getErr
}
func (f *snapshotFakeBackend) DeleteSnapshot(_ context.Context, _ string) error {
	return f.deleteErr
}

// snapshotFakeBackendWarner embeds snapshotFakeBackend and overrides
// CreateSnapshot to emit warnings via backend.StatusWarning so the test
// exercises the warning-collection path through the handler.
type snapshotFakeBackendWarner struct {
	snapshotFakeBackend
	createResp *config.Snapshot
	createErr  error
	warnings   []string
}

func (f *snapshotFakeBackendWarner) CreateSnapshot(ctx context.Context, _ config.SnapshotCreateRequest) (*config.Snapshot, error) {
	for _, msg := range f.warnings {
		backend.Phase(ctx, "test")
		backend.StatusWarning(ctx, msg)
	}
	return f.createResp, f.createErr
}

func newSnapshotTestServer(be backend.Backend) *Server {
	return NewServer(be, &config.ServerConfig{Name: "test-server"}, "", nil, nil)
}

func TestHandleListSnapshots_Empty(t *testing.T) {
	be := &snapshotFakeBackend{}
	srv := newSnapshotTestServer(be)

	r := httptest.NewRequest(http.MethodGet, "/api/snapshots", nil)
	w := httptest.NewRecorder()
	srv.Router().ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200", w.Code)
	}
	var resp config.SnapshotsResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Snapshots) != 0 {
		t.Errorf("expected empty list, got %d items", len(resp.Snapshots))
	}
}

func TestHandleCreateSnapshot_Validation(t *testing.T) {
	tests := []struct {
		name     string
		body     string
		wantCode int
	}{
		{"invalid_json", `{not json`, http.StatusBadRequest},
		{"missing_name", `{"source_shed":"foo"}`, http.StatusBadRequest},
		{"invalid_name", `{"name":"BAD","source_shed":"foo"}`, http.StatusBadRequest},
		{"missing_source", `{"name":"snap"}`, http.StatusBadRequest},
		{"invalid_source", `{"name":"snap","source_shed":"BAD"}`, http.StatusBadRequest},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			be := &snapshotFakeBackend{}
			srv := newSnapshotTestServer(be)
			r := httptest.NewRequest(http.MethodPost, "/api/snapshots", strings.NewReader(tt.body))
			w := httptest.NewRecorder()
			srv.Router().ServeHTTP(w, r)
			if w.Code != tt.wantCode {
				t.Errorf("status = %d; want %d (body: %s)", w.Code, tt.wantCode, w.Body.String())
			}
		})
	}
}

func TestHandleCreateSnapshot_Success(t *testing.T) {
	want := &config.Snapshot{Version: 1, Name: "snap1", Backend: "vz", SourceShed: "src"}
	be := &snapshotFakeBackend{createResp: want}
	srv := newSnapshotTestServer(be)

	body := `{"name":"snap1","source_shed":"src","comment":"hi"}`
	r := httptest.NewRequest(http.MethodPost, "/api/snapshots", strings.NewReader(body))
	w := httptest.NewRecorder()
	srv.Router().ServeHTTP(w, r)

	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d; want 201 (body: %s)", w.Code, w.Body.String())
	}
	if be.createCalled.Comment != "hi" {
		t.Errorf("comment forwarded = %q; want %q", be.createCalled.Comment, "hi")
	}
	var got config.SnapshotCreateResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Snapshot == nil || got.Snapshot.Name != want.Name {
		t.Errorf("snapshot = %+v; want name=%q", got.Snapshot, want.Name)
	}
	if len(got.Warnings) != 0 {
		t.Errorf("warnings = %v; want empty", got.Warnings)
	}
}

// TestHandleCreateSnapshot_Warnings verifies that backend.StatusWarning
// emitted during CreateSnapshot is surfaced in the response payload.
func TestHandleCreateSnapshot_Warnings(t *testing.T) {
	be := &snapshotFakeBackendWarner{
		createResp: &config.Snapshot{Version: 1, Name: "snap1", Backend: "vz", SourceShed: "src"},
		warnings:   []string{"workspace not captured: /tmp/proj"},
	}
	srv := newSnapshotTestServer(be)

	body := `{"name":"snap1","source_shed":"src"}`
	r := httptest.NewRequest(http.MethodPost, "/api/snapshots", strings.NewReader(body))
	w := httptest.NewRecorder()
	srv.Router().ServeHTTP(w, r)

	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d; want 201 (body: %s)", w.Code, w.Body.String())
	}
	var got config.SnapshotCreateResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.Warnings) != 1 || got.Warnings[0] != "workspace not captured: /tmp/proj" {
		t.Errorf("warnings = %v; want [workspace not captured: /tmp/proj]", got.Warnings)
	}
}

func TestHandleCreateSnapshot_SourceRunning(t *testing.T) {
	be := &snapshotFakeBackend{
		createErr: config.ErrSnapshotSourceRunningSentinel,
	}
	srv := newSnapshotTestServer(be)
	body := `{"name":"snap1","source_shed":"src"}`
	r := httptest.NewRequest(http.MethodPost, "/api/snapshots", strings.NewReader(body))
	w := httptest.NewRecorder()
	srv.Router().ServeHTTP(w, r)

	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d; want 409", w.Code)
	}
	var apiErr config.APIError
	json.Unmarshal(w.Body.Bytes(), &apiErr)
	if apiErr.Error.Code != config.ErrSnapshotSourceRunning {
		t.Errorf("code = %q; want %q", apiErr.Error.Code, config.ErrSnapshotSourceRunning)
	}
}

func TestHandleGetSnapshot_NotFound(t *testing.T) {
	be := &snapshotFakeBackend{getErr: config.ErrSnapshotNotFoundSentinel}
	srv := newSnapshotTestServer(be)
	r := httptest.NewRequest(http.MethodGet, "/api/snapshots/foo", nil)
	w := httptest.NewRecorder()
	srv.Router().ServeHTTP(w, r)
	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d; want 404", w.Code)
	}
}

func TestHandleDeleteSnapshot_NotFound(t *testing.T) {
	be := &snapshotFakeBackend{deleteErr: config.ErrSnapshotNotFoundSentinel}
	srv := newSnapshotTestServer(be)
	r := httptest.NewRequest(http.MethodDelete, "/api/snapshots/foo", nil)
	w := httptest.NewRecorder()
	srv.Router().ServeHTTP(w, r)
	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d; want 404", w.Code)
	}
}

func TestHandleDeleteSnapshot_Success(t *testing.T) {
	be := &snapshotFakeBackend{}
	srv := newSnapshotTestServer(be)
	r := httptest.NewRequest(http.MethodDelete, "/api/snapshots/snap1", nil)
	w := httptest.NewRecorder()
	srv.Router().ServeHTTP(w, r)
	if w.Code != http.StatusNoContent {
		t.Errorf("status = %d; want 204", w.Code)
	}
}

func TestHandleCreateShed_FromSnapshotMutualExclusion(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{
			"with_image",
			`{"name":"new","from_snapshot":"snap1","image":"experimental"}`,
		},
		{
			"with_repo",
			`{"name":"new","from_snapshot":"snap1","repo":"git@github.com:o/r.git"}`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			be := &snapshotFakeBackend{
				createShedFn: func(_ context.Context, _ config.CreateShedRequest) (*config.Shed, error) {
					return nil, fmt.Errorf("%w: --from-snapshot cannot be combined with --image or --repo", config.ErrInvalidShedRequestSentinel)
				},
			}
			srv := NewServer(be, &config.ServerConfig{Name: "t", DefaultBackend: config.BackendVZ}, "", nil, nil)
			r := httptest.NewRequest(http.MethodPost, "/api/sheds", bytes.NewBufferString(tt.body))
			w := httptest.NewRecorder()
			srv.Router().ServeHTTP(w, r)
			if w.Code != http.StatusBadRequest {
				t.Errorf("status = %d; want 400 (body: %s)", w.Code, w.Body.String())
			}
			var apiErr config.APIError
			if err := json.Unmarshal(w.Body.Bytes(), &apiErr); err != nil {
				t.Fatal(err)
			}
			if apiErr.Error.Code != config.ErrInvalidRequest {
				t.Errorf("code = %q; want %q", apiErr.Error.Code, config.ErrInvalidRequest)
			}
		})
	}
}
