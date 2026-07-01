package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/charliek/shed/internal/backend"
	"github.com/charliek/shed/internal/config"
)

// newTestServer creates a minimal API server for handler testing.
// It uses a nil backend since validation tests don't reach the backend layer.
func newTestServer() *Server {
	return NewServer(nil, &config.ServerConfig{
		Name:     "test-server",
		HTTPPort: 8080,
	}, "", nil, nil)
}

func postCreateShed(t *testing.T, srv *Server, req config.CreateShedRequest) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("failed to marshal request: %v", err)
	}
	r := httptest.NewRequest(http.MethodPost, "/api/sheds", bytes.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router := srv.Router()
	router.ServeHTTP(w, r)
	return w
}

type apiErrorResponse struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func parseErrorResponse(t *testing.T, w *httptest.ResponseRecorder) apiErrorResponse {
	t.Helper()
	var resp apiErrorResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to parse error response: %v\nbody: %s", err, w.Body.String())
	}
	return resp
}

func TestCreateShed_LocalDirAndRepoMutuallyExclusive(t *testing.T) {
	tmpDir := t.TempDir()
	srv := newTestServer()
	w := postCreateShed(t, srv, config.CreateShedRequest{
		Name:     "test-shed",
		Repo:     "user/repo",
		LocalDir: tmpDir,
	})

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", w.Code, w.Body.String())
	}
	resp := parseErrorResponse(t, w)
	if resp.Error.Code != config.ErrInvalidLocalDir {
		t.Errorf("expected error code %q, got %q", config.ErrInvalidLocalDir, resp.Error.Code)
	}
}

func TestCreateShed_LocalDirNotAbsolute(t *testing.T) {
	srv := newTestServer()
	w := postCreateShed(t, srv, config.CreateShedRequest{
		Name:     "test-shed",
		LocalDir: "relative/path",
	})

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", w.Code, w.Body.String())
	}
	resp := parseErrorResponse(t, w)
	if resp.Error.Code != config.ErrInvalidLocalDir {
		t.Errorf("expected error code %q, got %q", config.ErrInvalidLocalDir, resp.Error.Code)
	}
}

func TestCreateShed_LocalDirNotExist(t *testing.T) {
	srv := newTestServer()
	w := postCreateShed(t, srv, config.CreateShedRequest{
		Name:     "test-shed",
		LocalDir: "/nonexistent/path/should/not/exist",
	})

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", w.Code, w.Body.String())
	}
	resp := parseErrorResponse(t, w)
	if resp.Error.Code != config.ErrInvalidLocalDir {
		t.Errorf("expected error code %q, got %q", config.ErrInvalidLocalDir, resp.Error.Code)
	}
}

func TestCreateShed_LocalDirIsFile(t *testing.T) {
	tmpDir := t.TempDir()
	tmpFile := filepath.Join(tmpDir, "not-a-dir")
	if err := os.WriteFile(tmpFile, []byte("hello"), 0644); err != nil {
		t.Fatalf("failed to create temp file: %v", err)
	}

	srv := newTestServer()
	w := postCreateShed(t, srv, config.CreateShedRequest{
		Name:     "test-shed",
		LocalDir: tmpFile,
	})

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", w.Code, w.Body.String())
	}
	resp := parseErrorResponse(t, w)
	if resp.Error.Code != config.ErrInvalidLocalDir {
		t.Errorf("expected error code %q, got %q", config.ErrInvalidLocalDir, resp.Error.Code)
	}
}

func TestCreateShed_AddDirsRequireLocalDir(t *testing.T) {
	tmpDir := t.TempDir()
	srv := newTestServer()
	w := postCreateShed(t, srv, config.CreateShedRequest{
		Name:    "test-shed",
		AddDirs: []string{tmpDir},
	})

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", w.Code, w.Body.String())
	}
	resp := parseErrorResponse(t, w)
	if resp.Error.Code != config.ErrInvalidLocalDir {
		t.Errorf("expected error code %q, got %q", config.ErrInvalidLocalDir, resp.Error.Code)
	}
}

func TestCreateShed_AddDirsDuplicateBasename(t *testing.T) {
	base := t.TempDir()
	// Two distinct host directories that share a basename ("app").
	a := filepath.Join(base, "a", "app")
	b := filepath.Join(base, "b", "app")
	for _, d := range []string{a, b} {
		if err := os.MkdirAll(d, 0755); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}

	srv := newTestServer()
	w := postCreateShed(t, srv, config.CreateShedRequest{
		Name:     "test-shed",
		LocalDir: a,
		AddDirs:  []string{b},
	})

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", w.Code, w.Body.String())
	}
	resp := parseErrorResponse(t, w)
	if resp.Error.Code != config.ErrInvalidLocalDir {
		t.Errorf("expected error code %q, got %q", config.ErrInvalidLocalDir, resp.Error.Code)
	}
}

func TestMapBackendError_UnknownImage(t *testing.T) {
	err := fmt.Errorf("%w %q; available variants: base, default", config.ErrUnknownImageSentinel, "rust")
	code, errCode, msg := mapBackendError(err)
	if code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", code)
	}
	if errCode != config.ErrUnknownImage {
		t.Errorf("expected %q, got %q", config.ErrUnknownImage, errCode)
	}
	if !strings.Contains(msg, "unknown image") {
		t.Errorf("expected message to contain 'unknown image', got %q", msg)
	}
}

func TestMapBackendError_GenericPassthrough(t *testing.T) {
	err := fmt.Errorf("disk full: cannot copy rootfs")
	code, _, msg := mapBackendError(err)
	if code != http.StatusInternalServerError {
		t.Errorf("expected 500, got %d", code)
	}
	if msg != "disk full: cannot copy rootfs" {
		t.Errorf("expected passthrough message, got %q", msg)
	}
}

func TestMapBackendError_SentinelErrors(t *testing.T) {
	tests := []struct {
		name     string
		err      error
		wantCode int
		wantErr  string
	}{
		{"not found", fmt.Errorf("%w: mydev", config.ErrShedNotFoundSentinel), http.StatusNotFound, config.ErrShedNotFound},
		{"already exists", fmt.Errorf("%w: mydev", config.ErrShedAlreadyExistsSentinel), http.StatusConflict, config.ErrShedAlreadyExists},
		{"already running", fmt.Errorf("%w: mydev", config.ErrShedAlreadyRunningSentinel), http.StatusConflict, config.ErrShedAlreadyRunning},
		{"not running", fmt.Errorf("%w: mydev", config.ErrShedNotRunningSentinel), http.StatusConflict, config.ErrShedAlreadyStopped},
		{"image not found", fmt.Errorf("%w", config.ErrImageNotFoundSentinel), http.StatusNotFound, config.ErrImageNotFound},
		{"image in use", fmt.Errorf("%w", config.ErrImageInUseSentinel), http.StatusConflict, config.ErrImageInUse},
		{"invalid shed request", fmt.Errorf("%w: --from-snapshot cannot be combined with --image or --repo", config.ErrInvalidShedRequestSentinel), http.StatusBadRequest, config.ErrInvalidRequest},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			code, errCode, _ := mapBackendError(tt.err)
			if code != tt.wantCode {
				t.Errorf("code = %d, want %d", code, tt.wantCode)
			}
			if errCode != tt.wantErr {
				t.Errorf("errCode = %q, want %q", errCode, tt.wantErr)
			}
		})
	}
}

// createShedFakeBackend stubs only what the create/delete SSE handlers touch.
// CreateShed / DeleteShed run the configured fn so tests can emit
// progress/warning events before returning.
type createShedFakeBackend struct {
	createFn func(ctx context.Context, req config.CreateShedRequest) (*config.Shed, error)
	deleteFn func(ctx context.Context, name string) error
}

func (f *createShedFakeBackend) Type() backend.Type { return backend.TypeVZ }
func (f *createShedFakeBackend) Close() error       { return nil }
func (f *createShedFakeBackend) CreateShed(ctx context.Context, req config.CreateShedRequest) (*config.Shed, error) {
	return f.createFn(ctx, req)
}
func (f *createShedFakeBackend) GetShed(_ context.Context, _ string) (*config.Shed, error) {
	panic("unexpected")
}
func (f *createShedFakeBackend) ListSheds(_ context.Context) ([]config.Shed, error) {
	panic("unexpected")
}
func (f *createShedFakeBackend) DeleteShed(ctx context.Context, name string) error {
	if f.deleteFn != nil {
		return f.deleteFn(ctx, name)
	}
	panic("unexpected")
}
func (f *createShedFakeBackend) StartShed(_ context.Context, _ string) (*config.Shed, error) {
	panic("unexpected")
}
func (f *createShedFakeBackend) StopShed(_ context.Context, _ string) (*config.Shed, error) {
	panic("unexpected")
}
func (f *createShedFakeBackend) ResetShed(_ context.Context, _ string) (*config.Shed, error) {
	panic("unexpected")
}
func (f *createShedFakeBackend) ListSessions(_ context.Context, _ string) ([]config.Session, error) {
	panic("unexpected")
}
func (f *createShedFakeBackend) KillSession(_ context.Context, _, _ string) error {
	panic("unexpected")
}
func (f *createShedFakeBackend) Exec(_ context.Context, _ string, _ backend.ExecOptions) error {
	panic("unexpected")
}
func (f *createShedFakeBackend) DialService(_ context.Context, _ string, _ uint16) (net.Conn, error) {
	panic("unexpected")
}
func (f *createShedFakeBackend) ListImages(_ context.Context) ([]config.ImageInfo, error) {
	panic("unexpected")
}
func (f *createShedFakeBackend) InspectImage(_ context.Context, _ string) (config.ImageInspectResponse, error) {
	panic("unexpected")
}
func (f *createShedFakeBackend) TagImage(_ context.Context, _, _ string) error {
	panic("unexpected")
}
func (f *createShedFakeBackend) PullImage(_ context.Context, _, _, _ string, _ bool) (string, error) {
	panic("unexpected")
}
func (f *createShedFakeBackend) PushImage(_ context.Context, _, _ string) error {
	panic("unexpected")
}
func (f *createShedFakeBackend) DeleteImage(_ context.Context, _ string) error {
	panic("unexpected")
}
func (f *createShedFakeBackend) PruneImages(_ context.Context, _ bool) ([]config.ImageInfo, error) {
	panic("unexpected")
}
func (f *createShedFakeBackend) DiskUsage(_ context.Context) (config.DiskUsage, error) {
	panic("unexpected")
}
func (f *createShedFakeBackend) Prune(_ context.Context, _ backend.PruneOptions) (config.PruneReport, error) {
	panic("unexpected")
}
func (f *createShedFakeBackend) ListSnapshots(_ context.Context) ([]config.Snapshot, error) {
	panic("unexpected")
}
func (f *createShedFakeBackend) CreateSnapshot(_ context.Context, _ config.SnapshotCreateRequest) (*config.Snapshot, error) {
	panic("unexpected")
}
func (f *createShedFakeBackend) GetSnapshot(_ context.Context, _ string) (*config.Snapshot, error) {
	panic("unexpected")
}
func (f *createShedFakeBackend) DeleteSnapshot(_ context.Context, _ string) error {
	panic("unexpected")
}

// parseSSEEvents parses an SSE stream body into a list of (event, data) pairs.
// Handles only the simple "event:\ndata:\n\n" framing used by the API.
func parseSSEEvents(t *testing.T, body string) []struct{ Event, Data string } {
	t.Helper()
	var events []struct{ Event, Data string }
	for _, block := range strings.Split(body, "\n\n") {
		block = strings.TrimSpace(block)
		if block == "" {
			continue
		}
		var evt, data string
		for _, line := range strings.Split(block, "\n") {
			switch {
			case strings.HasPrefix(line, "event: "):
				evt = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				data = strings.TrimPrefix(line, "data: ")
			}
		}
		events = append(events, struct{ Event, Data string }{evt, data})
	}
	return events
}

// TestCreateShed_SSE_SurfacesProgressAndWarning verifies that
// StatusWarning events emitted by the backend during CreateShed reach
// the SSE stream as progress events with `warning=true`. Regression
// test for issue #84 — clone failures used to be journald-only.
//
// Post-§15 1b note: Phase boundaries are intentionally NOT sent over
// the SSE wire (they are server-side PhaseTimer events only). The
// warning message reaches the client as a status-only event with
// `"phase":""`. The CLI renders the message regardless (see
// `cmd/shed/shed.go`'s SSE loop, which only consumes `Message` +
// `Warning`).
func TestCreateShed_SSE_SurfacesProgressAndWarning(t *testing.T) {
	be := &createShedFakeBackend{
		createFn: func(ctx context.Context, req config.CreateShedRequest) (*config.Shed, error) {
			backend.Phase(ctx, "repo")
			backend.Status(ctx, "Cloning repository...")
			// Match the production sanitized message from vz/firecracker
			// client.go — no URL, no wrapped err.
			backend.StatusWarning(ctx, "Failed to clone repository (see server logs for details)")
			return &config.Shed{Name: req.Name, Status: config.StatusRunning, Repo: req.Repo}, nil
		},
	}
	srv := NewServer(be, &config.ServerConfig{Name: "test-server"}, "", nil, nil)

	body, _ := json.Marshal(config.CreateShedRequest{Name: "myshed", Repo: "git@example.com:x/y.git"})
	r := httptest.NewRequest(http.MethodPost, "/api/sheds", bytes.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Accept", "text/event-stream")
	w := httptest.NewRecorder()
	srv.Router().ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200 (body: %s)", w.Code, w.Body.String())
	}

	events := parseSSEEvents(t, w.Body.String())

	// Find the warning event and assert its payload carries warning=true
	// AND the user-visible message. Per §15 1b, phase is no longer
	// included on status-only events.
	var sawWarning, sawComplete bool
	for _, e := range events {
		if e.Event == "progress" && strings.Contains(e.Data, `"warning":true`) {
			sawWarning = true
			if !strings.Contains(e.Data, "Failed to clone repository") {
				t.Errorf("warning event missing failure message: %s", e.Data)
			}
			// SSE warning must NOT leak the repo URL — backends sanitize
			// it because URLs can carry credentials. (See vz/firecracker
			// client.go for the production message.)
			if strings.Contains(e.Data, "git@") || strings.Contains(e.Data, "://") {
				t.Errorf("warning event must not include repo URL: %s", e.Data)
			}
		}
		if e.Event == "complete" {
			sawComplete = true
		}
	}
	if !sawWarning {
		t.Errorf("no progress event with warning=true found in stream:\n%s", w.Body.String())
	}
	if !sawComplete {
		t.Errorf("no complete event found in stream:\n%s", w.Body.String())
	}
}

// TestCreateShed_SSE_PhaseOnlyEventsNotStreamed verifies that
// `backend.Phase` calls (timer boundaries with no user-visible message)
// do NOT cross the SSE wire. Per §15 1b the SSE handler drops events
// with empty Message; the PhaseTimer still consumes them server-side.
// Regression test for the silent-skip in `internal/api/handlers.go`'s
// `sseFn`.
func TestCreateShed_SSE_PhaseOnlyEventsNotStreamed(t *testing.T) {
	be := &createShedFakeBackend{
		createFn: func(ctx context.Context, req config.CreateShedRequest) (*config.Shed, error) {
			// One phase-only event (should NOT appear in SSE), one
			// status event (should appear), then another phase-only.
			backend.Phase(ctx, "vm")
			backend.Status(ctx, "Visible message")
			backend.Phase(ctx, "agent")
			return &config.Shed{Name: req.Name, Status: config.StatusRunning}, nil
		},
	}
	srv := NewServer(be, &config.ServerConfig{Name: "test-server"}, "", nil, nil)

	body, _ := json.Marshal(config.CreateShedRequest{Name: "phase-only"})
	r := httptest.NewRequest(http.MethodPost, "/api/sheds", bytes.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Accept", "text/event-stream")
	w := httptest.NewRecorder()
	srv.Router().ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200 (body: %s)", w.Code, w.Body.String())
	}

	progressEvents := 0
	for _, e := range parseSSEEvents(t, w.Body.String()) {
		if e.Event != "progress" {
			continue
		}
		progressEvents++
		// Any progress event we DO emit must carry a non-empty message.
		// (Phase-only events would serialize as `{"phase":"vm","message":""}`
		// and an SSE consumer treating them as user-visible would render
		// blank lines.)
		if !strings.Contains(e.Data, `"message":"Visible message"`) {
			t.Errorf("unexpected SSE progress event data: %s", e.Data)
		}
	}
	if progressEvents != 1 {
		t.Errorf("got %d progress events, want exactly 1 (the Status call); "+
			"phase-only events must not be streamed", progressEvents)
	}
}

// TestDeleteShed_SSE_StreamsProgressAndComplete verifies that a delete requested
// with Accept: text/event-stream streams the backend's Status phases as progress
// events and finishes with a terminal `complete` event (#232).
func TestDeleteShed_SSE_StreamsProgressAndComplete(t *testing.T) {
	be := &createShedFakeBackend{
		deleteFn: func(ctx context.Context, _ string) error {
			backend.Status(ctx, "Terminating virtual machine...")
			backend.Status(ctx, "Removing volume...")
			return nil
		},
	}
	srv := NewServer(be, &config.ServerConfig{Name: "test-server"}, "", nil, nil)

	r := httptest.NewRequest(http.MethodDelete, "/api/sheds/myshed", nil)
	r.Header.Set("Accept", "text/event-stream")
	w := httptest.NewRecorder()
	srv.Router().ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200 (body: %s)", w.Code, w.Body.String())
	}
	var progress, complete int
	for _, e := range parseSSEEvents(t, w.Body.String()) {
		switch e.Event {
		case "progress":
			progress++
		case "complete":
			complete++
		}
	}
	if progress < 1 {
		t.Errorf("expected >=1 progress event, got %d:\n%s", progress, w.Body.String())
	}
	if complete != 1 {
		t.Errorf("expected exactly 1 complete event, got %d:\n%s", complete, w.Body.String())
	}
}

// TestDeleteShed_PlainReturns204 verifies that a delete WITHOUT the SSE Accept
// header keeps the plain 204 No Content behavior (back-compat for non-streaming
// clients and old CLIs).
func TestDeleteShed_PlainReturns204(t *testing.T) {
	be := &createShedFakeBackend{
		deleteFn: func(context.Context, string) error { return nil },
	}
	srv := NewServer(be, &config.ServerConfig{Name: "test-server"}, "", nil, nil)

	r := httptest.NewRequest(http.MethodDelete, "/api/sheds/myshed", nil)
	w := httptest.NewRecorder()
	srv.Router().ServeHTTP(w, r)

	if w.Code != http.StatusNoContent {
		t.Fatalf("status = %d; want 204 (body: %s)", w.Code, w.Body.String())
	}
	if w.Body.Len() != 0 {
		t.Errorf("expected empty 204 body, got: %s", w.Body.String())
	}
}

// TestDeleteShed_SSE_SurfacesError verifies a backend delete failure is surfaced
// as a terminal SSE `error` event (the stream still opens with 200 first).
func TestDeleteShed_SSE_SurfacesError(t *testing.T) {
	be := &createShedFakeBackend{
		deleteFn: func(context.Context, string) error { return config.ErrShedNotFoundSentinel },
	}
	srv := NewServer(be, &config.ServerConfig{Name: "test-server"}, "", nil, nil)

	r := httptest.NewRequest(http.MethodDelete, "/api/sheds/ghost", nil)
	r.Header.Set("Accept", "text/event-stream")
	w := httptest.NewRecorder()
	srv.Router().ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200 (SSE opens before the error), body: %s", w.Code, w.Body.String())
	}
	var sawError bool
	for _, e := range parseSSEEvents(t, w.Body.String()) {
		if e.Event == "error" {
			sawError = true
		}
	}
	if !sawError {
		t.Errorf("no error event in stream:\n%s", w.Body.String())
	}
}
