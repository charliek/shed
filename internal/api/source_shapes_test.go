package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/charliek/shed/internal/config"
	"github.com/charliek/shed/internal/source"
)

// The shapes GET /api/sources and the source routes answer for a user the
// account service answers for (plan 040, section 3.2), pinned as fixtures
// that this package, shed-core's SourceDocument test and the desktop
// harness's mock server all read: crates/fixtures/sources/.
var sourceShapes = filepath.Join("..", "..", "crates", "fixtures", "sources")

func readShape(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(sourceShapes, name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// canonical re-encodes JSON with sorted keys and no space.
func canonical(t *testing.T, b []byte) string {
	t.Helper()
	var v any
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	if err := dec.Decode(&v); err != nil {
		t.Fatalf("%s: %v", b, err)
	}
	out, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

// serviceRef is the delivery slot of a directory user's sources document.
type serviceRef struct {
	Kind  string `json:"kind"`
	URL   string `json:"url"`
	State string `json:"state,omitempty"`
}

// TestSourceIsServiceShape: the 409 body the source routes answer for a
// directory user is the API's error shape with ErrSourceIsService and its
// fixed message, exactly as the fixture holds it.
func TestSourceIsServiceShape(t *testing.T) {
	rec := httptest.NewRecorder()
	writeError(rec, http.StatusConflict, config.ErrSourceIsService, config.SourceIsServiceMessage)
	if got, want := canonical(t, rec.Body.Bytes()), canonical(t, readShape(t, "source-is-service.json")); got != want {
		t.Fatalf("the 409 body is\n%s\nthe fixture\n%s", got, want)
	}
}

// TestServiceSourceDocumentShape: a directory user's sources document is
// the existing document with no machine, no signing slot and one delivery
// entry {"kind": "service", "url": ...}, with "state": "not_delivering"
// while the service is not delivering to this server.
func TestServiceSourceDocumentShape(t *testing.T) {
	for name, state := range map[string]string{"service.json": "", "service-not-delivering.json": source.StateNotDelivering} {
		t.Run(name, func(t *testing.T) {
			doc := map[string]any{
				"user": "ada",
				"binding": map[string]any{
					"delivery": []serviceRef{{Kind: source.KindService, URL: "https://accounts.example.test", State: state}},
					"signing":  []source.Ref{},
				},
				"machines": []any{},
			}
			b, err := json.Marshal(doc)
			if err != nil {
				t.Fatal(err)
			}
			if got, want := canonical(t, b), canonical(t, readShape(t, name)); got != want {
				t.Fatalf("the document is\n%s\nthe fixture\n%s", got, want)
			}
		})
	}
}
