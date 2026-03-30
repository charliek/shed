package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

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
