package api

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/charliek/shed/internal/backend"
	"github.com/charliek/shed/internal/config"
)

// routesFakeBackend is a no-op stub backend used by routing tests. Each
// method returns a benign zero value or sentinel error so requests reach
// the real handlers (and exercise routing) without panicking the way a nil
// backend would. We don't care which status the handler chooses — the
// routing tests only assert that dispatch happened (i.e., we never see
// 405 Method Not Allowed).
type routesFakeBackend struct{}

func (routesFakeBackend) Type() backend.Type { return backend.TypeVZ }
func (routesFakeBackend) Close() error       { return nil }
func (routesFakeBackend) CreateShed(_ context.Context, _ config.CreateShedRequest) (*config.Shed, error) {
	return nil, config.ErrShedAlreadyExistsSentinel
}
func (routesFakeBackend) GetShed(_ context.Context, _ string) (*config.Shed, error) {
	return nil, config.ErrShedNotFoundSentinel
}
func (routesFakeBackend) ListSheds(_ context.Context) ([]config.Shed, error) { return nil, nil }
func (routesFakeBackend) DeleteShed(_ context.Context, _ string) error {
	return config.ErrShedNotFoundSentinel
}
func (routesFakeBackend) StartShed(_ context.Context, _ string) (*config.Shed, error) {
	return nil, config.ErrShedNotFoundSentinel
}
func (routesFakeBackend) StopShed(_ context.Context, _ string) (*config.Shed, error) {
	return nil, config.ErrShedNotFoundSentinel
}
func (routesFakeBackend) ResetShed(_ context.Context, _ string) (*config.Shed, error) {
	return nil, config.ErrShedNotFoundSentinel
}
func (routesFakeBackend) ListSessions(_ context.Context, _ string) ([]config.Session, error) {
	return nil, nil
}
func (routesFakeBackend) KillSession(_ context.Context, _, _ string) error { return nil }
func (routesFakeBackend) Exec(_ context.Context, _ string, _ backend.ExecOptions) error {
	return nil
}
func (routesFakeBackend) DialService(_ context.Context, _ string, _ uint16) (net.Conn, error) {
	return nil, config.ErrShedNotFoundSentinel
}
func (routesFakeBackend) ListImages(_ context.Context) ([]config.ImageInfo, error) { return nil, nil }
func (routesFakeBackend) InspectImage(_ context.Context, _ string) (config.ImageInspectResponse, error) {
	return config.ImageInspectResponse{}, config.ErrImageNotFoundSentinel
}
func (routesFakeBackend) TagImage(_ context.Context, _, _ string) error { return nil }
func (routesFakeBackend) PullImage(_ context.Context, _, _, _ string, _ bool) (string, error) {
	return "", nil
}
func (routesFakeBackend) PushImage(_ context.Context, _, _ string) error { return nil }
func (routesFakeBackend) DeleteImage(_ context.Context, _ string) error  { return nil }
func (routesFakeBackend) PruneImages(_ context.Context, _ bool) ([]config.ImageInfo, error) {
	return nil, nil
}
func (routesFakeBackend) DiskUsage(_ context.Context) (config.DiskUsage, error) {
	return config.DiskUsage{}, nil
}
func (routesFakeBackend) Prune(_ context.Context, _ backend.PruneOptions) (config.PruneReport, error) {
	return config.PruneReport{}, nil
}
func (routesFakeBackend) ListSnapshots(_ context.Context) ([]config.Snapshot, error) { return nil, nil }
func (routesFakeBackend) CreateSnapshot(_ context.Context, _ config.SnapshotCreateRequest) (*config.Snapshot, error) {
	return nil, config.ErrInvalidShedRequestSentinel
}
func (routesFakeBackend) GetSnapshot(_ context.Context, _ string) (*config.Snapshot, error) {
	return nil, config.ErrShedNotFoundSentinel
}
func (routesFakeBackend) DeleteSnapshot(_ context.Context, _ string) error { return nil }

func newRoutesTestServerWith(b backend.Backend) *Server {
	return NewServer(b, &config.ServerConfig{
		Name:     "test-server",
		HTTPPort: 8080,
	}, "", nil, nil)
}

func newRoutesTestServer() *Server {
	return newRoutesTestServerWith(routesFakeBackend{})
}

// TestRouteImagesNo405 is a regression test for a chi trie precedence bug
// where r.Delete("/{name}", ...) registered as a sibling of literal POST
// routes (e.g. /pull, /push, /prune, /tag) would shadow them, causing
// requests like `POST /api/images/pull` to return 405 Method Not Allowed
// with `Allow: DELETE` instead of dispatching to the literal handler.
//
// We don't care about handler behavior here — bodies are intentionally
// empty/minimal. The bar is: routing reaches a handler (any status other
// than 405), and we never see Allow: DELETE on a POST route.
func TestRouteImagesNo405(t *testing.T) {
	srv := newRoutesTestServer()
	router := srv.Router()

	tests := []struct {
		name   string
		method string
		path   string
	}{
		{"post pull", http.MethodPost, "/api/images/pull"},
		{"post push", http.MethodPost, "/api/images/push"},
		{"post prune", http.MethodPost, "/api/images/prune"},
		{"post tag", http.MethodPost, "/api/images/tag"},
		{"get inspect", http.MethodGet, "/api/images/inspect/some-tag"},
		{"delete by name", http.MethodDelete, "/api/images/some-tag"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := httptest.NewRequest(tt.method, tt.path, strings.NewReader(""))
			r.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			router.ServeHTTP(w, r)

			if w.Code == http.StatusMethodNotAllowed {
				t.Fatalf("%s %s returned 405 Method Not Allowed (Allow: %q) — routing bug, expected dispatch to a handler",
					tt.method, tt.path, w.Header().Get("Allow"))
			}
			// Belt-and-suspenders: even if some future change started
			// returning 405 with a different Allow header, we shouldn't see
			// Allow: DELETE on a POST route.
			if tt.method == http.MethodPost {
				if allow := w.Header().Get("Allow"); strings.Contains(allow, "DELETE") {
					t.Errorf("%s %s response has Allow header containing DELETE (%q) — sibling DELETE route is shadowing this POST",
						tt.method, tt.path, allow)
				}
			}
		})
	}
}

// TestRouteSnapshotsNo405 mirrors the /images regression test for
// /api/snapshots. Currently there are no literal sibling collisions, but
// the parametric /{name} route is wrapped defensively as drift insurance.
func TestRouteSnapshotsNo405(t *testing.T) {
	srv := newRoutesTestServer()
	router := srv.Router()

	tests := []struct {
		name   string
		method string
		path   string
	}{
		{"get list", http.MethodGet, "/api/snapshots/"},
		{"get by name", http.MethodGet, "/api/snapshots/some-snap"},
		{"delete by name", http.MethodDelete, "/api/snapshots/some-snap"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := httptest.NewRequest(tt.method, tt.path, strings.NewReader(""))
			r.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			router.ServeHTTP(w, r)

			if w.Code == http.StatusMethodNotAllowed {
				t.Fatalf("%s %s returned 405 Method Not Allowed (Allow: %q) — routing bug, expected dispatch to a handler",
					tt.method, tt.path, w.Header().Get("Allow"))
			}
		})
	}
}
