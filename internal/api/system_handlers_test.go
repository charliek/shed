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
// Only DiskUsage is wired; other methods panic if called.
type fakeBackend struct {
	usage config.DiskUsage
	err   error
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
func (f *fakeBackend) DeleteImage(ctx context.Context, name string) error { panic("unexpected") }
func (f *fakeBackend) PruneImages(ctx context.Context, dryRun bool) ([]config.ImageInfo, error) {
	panic("unexpected")
}

func (f *fakeBackend) DiskUsage(ctx context.Context) (config.DiskUsage, error) {
	return f.usage, f.err
}

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
