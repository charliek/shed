package remote

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/charliek/shed/internal/api"
	"github.com/charliek/shed/internal/config"
	"github.com/charliek/shed/internal/remote/provider/service/servicetest"
	"github.com/charliek/shed/internal/servertls"
	"github.com/charliek/shed/internal/users"
	"github.com/charliek/shed/internal/users/userstest"
)

// The client fixtures of plan 041 (crates/fixtures/cloud/): what shed-server's
// own API answers for cloud sheds, recorded from the real handlers over the
// remote backend and the reference compute service, so the Rust clients
// decode the server's bytes rather than a hand-written copy. Recorded with
//
//	UPDATE_CLIENT_FIXTURES=1 go test ./internal/remote/ -run TestCloudClientFixtures
//
// Without it the test records a fresh set and checks each committed file has
// the same shape (every object's keys, every array's length, every leaf's
// JSON type): the values (times, ids) differ from run to run, so a changed
// shape is what makes a fixture stale.
const clientFixtureDir = "../../crates/fixtures/cloud"

func TestCloudClientFixtures(t *testing.T) {
	e, _ := serviceEnv(t, servicetest.Options{})
	e.createFor("widgets", "linus")
	e.createFor("gadgets", "linus")
	if _, err := e.b.StopShed(ctxT(t), "gadgets"); err != nil {
		t.Fatal(err)
	}
	if got := e.settled("gadgets").Status; got != config.StatusStopped {
		t.Fatalf("gadgets settled %q, want stopped", got)
	}

	cfg := &config.ServerConfig{
		Name:           servicetest.DefaultServer,
		DefaultBackend: config.BackendRemote,
		Auth:           &config.AuthConfig{Mode: config.AuthModeOpen},
	}
	router := api.NewServer(e.b, cfg, "", nil, nil).Router()
	call := func(method, path, body string, hdr ...string) (int, []byte) {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		for i := 0; i+1 < len(hdr); i += 2 {
			req.Header.Set(hdr[i], hdr[i+1])
		}
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		return rec.Code, rec.Body.Bytes()
	}
	want := func(label string, code, status int, body []byte) []byte {
		t.Helper()
		if code != status {
			t.Fatalf("%s: %d %s, want %d", label, code, body, status)
		}
		return body
	}

	files := map[string][]byte{}
	code, body := call(http.MethodGet, "/api/sheds", "")
	files["sheds.json"] = want("GET /api/sheds", code, http.StatusOK, body)
	code, body = call(http.MethodGet, "/api/overview", "")
	files["overview.json"] = want("GET /api/overview", code, http.StatusOK, body)
	code, body = call(http.MethodPost, "/api/sheds/widgets/extend", `{"hours":6,"request_id":"6f9619ff-8b86-4d01-b42d-00cf4fc964ff"}`)
	files["extend.json"] = want("extend", code, http.StatusOK, body)
	code, body = call(http.MethodPost, "/api/sheds/widgets/extend", `{"hours":720}`)
	files["extend-at-cap.json"] = want("extend past the cap", code, http.StatusConflict, body)
	code, body = call(http.MethodGet, "/api/cloud/usage", "")
	files["usage-org.json"] = want("GET /api/cloud/usage", code, http.StatusOK, body)
	// A person's usage, through the handler for an authenticated person (an
	// mtls server whose user list has linus): the open server's caller above
	// is a local admin, whose answer is the organization's alone.
	code, body = personUsage(t, e)
	files["usage-person.json"] = want("GET /api/cloud/usage (a person)", code, http.StatusOK, body)
	// The open server's caller is a local admin, whom the account service
	// makes no machine for: the create stream's error event carries the
	// reason.
	code, body = call(http.MethodPost, "/api/sheds", `{"name":"refused","no_provision":true}`, "Accept", "text/event-stream")
	files["create-refused.sse"] = want("create (SSE)", code, http.StatusOK, body)
	if !bytes.Contains(body, []byte(`"reason":"person"`)) {
		t.Fatalf("the create stream's error has no reason: %s", body)
	}

	if os.Getenv("UPDATE_CLIENT_FIXTURES") == "1" {
		if err := os.MkdirAll(clientFixtureDir, 0o755); err != nil {
			t.Fatal(err)
		}
		for name, data := range files {
			if err := os.WriteFile(filepath.Join(clientFixtureDir, name), data, 0o644); err != nil {
				t.Fatal(err)
			}
		}
		return
	}
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		have, err := os.ReadFile(filepath.Join(clientFixtureDir, name))
		if err != nil {
			t.Errorf("read %s: %v (UPDATE_CLIENT_FIXTURES=1 go test ./internal/remote/ -run TestCloudClientFixtures writes it)", name, err)
			continue
		}
		got, err := fixtureShape(have)
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		fresh, err := fixtureShape(files[name])
		if err != nil {
			t.Fatalf("%s (fresh): %v", name, err)
		}
		if !reflect.DeepEqual(got, fresh) {
			t.Errorf("crates/fixtures/cloud/%s no longer has the server's shape: run UPDATE_CLIENT_FIXTURES=1 go test ./internal/remote/ -run TestCloudClientFixtures\n have %v\n want %v", name, got, fresh)
		}
	}
}

// personUsage is GET /api/cloud/usage as linus, a person of the account
// service, sends it: over an mtls server whose user list holds linus, with a
// control certificate for linus's key. Its status and body.
func personUsage(t *testing.T, e *env) (int, []byte) {
	t.Helper()
	const fp = "SHA256:linus-fixture-key"
	lookup := userstest.New()
	lookup.AddUser(users.User{Name: "linus"})
	lookup.AddKey("linus", users.Key{Fingerprint: fp, Source: users.SourceInline})
	cfg := &config.ServerConfig{
		Name:           servicetest.DefaultServer,
		DefaultBackend: config.BackendRemote,
		Auth:           &config.AuthConfig{Mode: config.AuthModeMTLS},
	}
	s := api.NewServer(e.b, cfg, "", nil, nil)
	s.SetUserLookup(lookup)
	now := time.Now()
	leaf := &x509.Certificate{
		Subject: pkix.Name{
			CommonName:         fp,
			OrganizationalUnit: []string{"control"},
			Names:              []pkix.AttributeTypeAndValue{{Type: servertls.ClientUserOID, Value: "linus"}},
		},
		NotBefore: now.Add(-time.Minute),
		NotAfter:  now.Add(time.Hour),
	}
	req := httptest.NewRequest(http.MethodGet, "/api/cloud/usage", nil)
	req.TLS = &tls.ConnectionState{HandshakeComplete: true, PeerCertificates: []*x509.Certificate{leaf}}
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	var u config.CloudUsage
	if rec.Code == http.StatusOK && (json.Unmarshal(rec.Body.Bytes(), &u) != nil || u.Person == nil) {
		t.Fatalf("linus's usage has no person: %s", rec.Body.Bytes())
	}
	return rec.Code, rec.Body.Bytes()
}

// fixtureShape is a JSON document's (or an SSE stream's events') shape: each
// object as its keys' shapes, each array as its elements' shapes, and each
// leaf as its JSON type, a number as an integer or not.
func fixtureShape(data []byte) (any, error) {
	if bytes.HasPrefix(data, []byte("event: ")) {
		var events []any
		for _, frame := range strings.Split(strings.TrimSpace(string(data)), "\n\n") {
			name, payload, ok := strings.Cut(frame, "\ndata: ")
			if !ok {
				return nil, fmt.Errorf("an SSE frame with no data: %q", frame)
			}
			shape, err := fixtureShape([]byte(payload))
			if err != nil {
				return nil, err
			}
			events = append(events, []any{name, shape})
		}
		return events, nil
	}
	// Numbers as written, so an integer and a fractional number differ (the
	// Rust clients decode counts as integers).
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var doc any
	if err := dec.Decode(&doc); err != nil {
		return nil, err
	}
	return shapeOf(doc), nil
}

func shapeOf(v any) any {
	switch v := v.(type) {
	case map[string]any:
		out := map[string]any{}
		for k, x := range v {
			out[k] = shapeOf(x)
		}
		return out
	case []any:
		out := make([]any, len(v))
		for i, x := range v {
			out[i] = shapeOf(x)
		}
		return out
	case nil:
		return "null"
	case json.Number:
		if _, err := v.Int64(); err == nil {
			return "integer"
		}
		return "number"
	default:
		return fmt.Sprintf("%T", v)
	}
}

// The shape tells an integer from a fractional number (a count the clients
// decode as an integer), and not one integer from another.
func TestFixtureShapeNumbers(t *testing.T) {
	shape := func(doc string) any {
		t.Helper()
		v, err := fixtureShape([]byte(doc))
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	if !reflect.DeepEqual(shape(`{"org":{"max":100}}`), shape(`{"org":{"max":101}}`)) {
		t.Error("100 and 101 have different shapes")
	}
	if reflect.DeepEqual(shape(`{"org":{"max":100}}`), shape(`{"org":{"max":100.5}}`)) {
		t.Error("100 and 100.5 have the same shape")
	}
	if reflect.DeepEqual(shape(`[1]`), shape(`["1"]`)) {
		t.Error("a number and a string have the same shape")
	}
}
