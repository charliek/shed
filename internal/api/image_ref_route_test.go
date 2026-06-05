package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/charliek/shed/internal/config"
)

// capturingBackend records the identifier the image handlers resolve so the
// route tests can assert a slash-bearing Docker ref survives the wire intact.
type capturingBackend struct {
	routesFakeBackend
	deletedIdent string
	inspectIdent string
}

func (b *capturingBackend) DeleteImage(_ context.Context, name string) error {
	b.deletedIdent = name
	return nil
}

func (b *capturingBackend) InspectImage(_ context.Context, name string) (config.ImageInspectResponse, error) {
	b.inspectIdent = name
	return config.ImageInspectResponse{}, nil
}

func newCapturingServer() (*Server, *capturingBackend) {
	be := &capturingBackend{}
	return newRoutesTestServerWith(be), be
}

// TestImageDeleteInspectByRef is the regression guard for the bug where
// `shed image rm <ref>` / `shed image inspect <ref>` 404'd: a Docker ref
// contains slashes, and the old path-only routes (/api/images/{name},
// /api/images/inspect/{name}) couldn't carry a multi-segment ref through chi's
// single-segment {name} param. The ?ref= query form fixes it; the path form
// stays for slash-free identifiers (digests, cosmetic tags).
func TestImageDeleteInspectByRef(t *testing.T) {
	const ref = "ghcr.io/charliek/shed-vz-full:v0.6.0"

	t.Run("delete by ref (query)", func(t *testing.T) {
		srv, be := newCapturingServer()
		target := "/api/images?ref=" + url.QueryEscape(ref)
		w := httptest.NewRecorder()
		srv.Router().ServeHTTP(w, httptest.NewRequest(http.MethodDelete, target, nil))

		if w.Code != http.StatusNoContent {
			t.Fatalf("DELETE %s = %d, want %d", target, w.Code, http.StatusNoContent)
		}
		if be.deletedIdent != ref {
			t.Fatalf("backend received ident %q, want %q (ref mangled in transit)", be.deletedIdent, ref)
		}
	})

	t.Run("inspect by ref (query)", func(t *testing.T) {
		srv, be := newCapturingServer()
		target := "/api/images/inspect?ref=" + url.QueryEscape(ref)
		w := httptest.NewRecorder()
		srv.Router().ServeHTTP(w, httptest.NewRequest(http.MethodGet, target, nil))

		if w.Code != http.StatusOK {
			t.Fatalf("GET %s = %d, want %d", target, w.Code, http.StatusOK)
		}
		if be.inspectIdent != ref {
			t.Fatalf("backend received ident %q, want %q (ref mangled in transit)", be.inspectIdent, ref)
		}
	})

	// Slash-free identifiers (digests, cosmetic tags) keep working via the
	// legacy path form so an older CLI still drives a newer server.
	t.Run("delete by digest (legacy path)", func(t *testing.T) {
		srv, be := newCapturingServer()
		const digest = "sha256:2d9669bcf0cd"
		w := httptest.NewRecorder()
		srv.Router().ServeHTTP(w, httptest.NewRequest(http.MethodDelete, "/api/images/"+digest, nil))

		if w.Code != http.StatusNoContent {
			t.Fatalf("DELETE by digest = %d, want %d", w.Code, http.StatusNoContent)
		}
		if be.deletedIdent != digest {
			t.Fatalf("backend received ident %q, want %q", be.deletedIdent, digest)
		}
	})

	t.Run("inspect by tag (legacy path)", func(t *testing.T) {
		srv, be := newCapturingServer()
		w := httptest.NewRecorder()
		srv.Router().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/images/inspect/full", nil))

		if w.Code != http.StatusOK {
			t.Fatalf("GET inspect by tag = %d, want %d", w.Code, http.StatusOK)
		}
		if be.inspectIdent != "full" {
			t.Fatalf("backend received ident %q, want %q", be.inspectIdent, "full")
		}
	})

	// A missing identifier must be a clean 400, not a delete of "".
	t.Run("delete with no ident is 400", func(t *testing.T) {
		srv, be := newCapturingServer()
		w := httptest.NewRecorder()
		srv.Router().ServeHTTP(w, httptest.NewRequest(http.MethodDelete, "/api/images", nil))

		if w.Code != http.StatusBadRequest {
			t.Fatalf("DELETE with no ref = %d, want %d", w.Code, http.StatusBadRequest)
		}
		if be.deletedIdent != "" {
			t.Fatalf("backend.DeleteImage called with %q, want no call", be.deletedIdent)
		}
	})

	// Document the original bug: a raw slash-bearing ref on the path form does
	// not route. This is precisely why the ?ref= query form exists.
	t.Run("ref on legacy path form 404s", func(t *testing.T) {
		srv, _ := newCapturingServer()
		w := httptest.NewRecorder()
		srv.Router().ServeHTTP(w, httptest.NewRequest(http.MethodDelete, "/api/images/"+ref, nil))

		if w.Code != http.StatusNotFound {
			t.Fatalf("DELETE /api/images/%s = %d, want %d (path form can't carry a ref)", ref, w.Code, http.StatusNotFound)
		}
	})
}
