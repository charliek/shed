package directory

import (
	"bytes"
	"crypto/ed25519"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

// The fixtures' world: its issuer, organization, clock and live server.
const (
	worldIssuer   = "https://accounts.example.test"
	worldOrg      = "example"
	worldClientID = "srv_0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a"
	// enrolledID is the client id a fake 201 names ({{client_id}} filled).
	enrolledID = "srv_0c0c0c0c0c0c0c0c0c0c0c0c0c0c0c0c"
)

var worldClock = time.Date(2026, 11, 16, 6, 0, 0, 0, time.UTC)

// fixedClock returns a clock reading *t.
func fixedClock(t *time.Time) func() time.Time { return func() time.Time { return *t } }

// fixtureKey is the Ed25519 private key of test-keys/<stem>.pem.
func fixtureKey(t *testing.T, stem string) ed25519.PrivateKey {
	t.Helper()
	k, ok := parsePKCS8(t, "fixtures/test-keys/"+stem+".pem").(ed25519.PrivateKey)
	if !ok {
		t.Fatalf("%s.pem is not an Ed25519 key", stem)
	}
	return k
}

// worldEnrolment is srv-a's enrolment with the given issuer.
func worldEnrolment(t *testing.T, issuer string) *Enrolment {
	t.Helper()
	return &Enrolment{
		ClientID:      worldClientID,
		Issuer:        issuer,
		TokenEndpoint: issuer + "/oauth2/token",
		Organization:  worldOrg,
		Name:          "build-1.example.test",
		EnrolledAt:    worldClock,
		PrivateKey:    fixtureKey(t, "server-ed25519"),
	}
}

// served is one canned HTTP answer.
type served struct {
	status int
	header http.Header
	body   []byte
}

func (s served) write(w http.ResponseWriter) {
	for k, vs := range s.header {
		w.Header()[k] = append([]string(nil), vs...)
	}
	w.WriteHeader(s.status)
	_, _ = w.Write(s.body)
}

// goodToken is a token endpoint's 200.
var goodToken = served{
	status: http.StatusOK,
	header: http.Header{"Content-Type": {"application/json"}, "Cache-Control": {"no-store"}},
	body:   []byte(`{"access_token":"opaque-token","token_type":"bearer","expires_in":600,"scope":"directory:read"}`),
}

// fakeService answers the token endpoint and the directory with canned
// answers (in turn, the last repeated), and records what it was sent.
type fakeService struct {
	t   *testing.T
	srv *httptest.Server

	mu         sync.Mutex
	tokens     []served
	directory  []served
	enroll     []served
	assertions []string // every client_assertion received
	bearers    []string // every Authorization header on the directory
	enrolBody  [][]byte
	paths      []string
}

func newFakeService(t *testing.T) *fakeService {
	t.Helper()
	fs := &fakeService{t: t, tokens: []served{goodToken}}
	fs.srv = httptest.NewServer(http.HandlerFunc(fs.serve))
	t.Cleanup(fs.srv.Close)
	return fs
}

func take(list *[]served) (served, bool) {
	if len(*list) == 0 {
		return served{}, false
	}
	s := (*list)[0]
	if len(*list) > 1 {
		*list = (*list)[1:]
	}
	return s, true
}

func (fs *fakeService) serve(w http.ResponseWriter, r *http.Request) {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	fs.paths = append(fs.paths, r.Method+" "+r.URL.Path)
	var s served
	var ok bool
	switch r.Method + " " + r.URL.Path {
	case "POST /oauth2/token":
		body, _ := io.ReadAll(r.Body)
		form, err := url.ParseQuery(string(body))
		if err != nil {
			fs.t.Errorf("the token request is not a form: %v", err)
		}
		fs.assertions = append(fs.assertions, form.Get("client_assertion"))
		s, ok = take(&fs.tokens)
	case "GET /v1/directory":
		fs.bearers = append(fs.bearers, r.Header.Get("Authorization"))
		s, ok = take(&fs.directory)
	case "POST /v1/servers/enroll":
		body, _ := io.ReadAll(r.Body)
		fs.enrolBody = append(fs.enrolBody, body)
		s, ok = take(&fs.enroll)
	}
	if !ok {
		fs.t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusNotFound)
		return
	}
	s.write(w)
}

// count returns how many requests were made for "METHOD /path".
func (fs *fakeService) count(req string) int {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	n := 0
	for _, p := range fs.paths {
		if p == req {
			n++
		}
	}
	return n
}

// client is a Client for this fake service through the production HTTP
// client's plain HTTP rules (the service is on loopback, and
// AllowPlainHTTP is set), at the world's clock.
func (fs *fakeService) client(t *testing.T, org string) *Client {
	t.Helper()
	clock := worldClock
	return fs.clientAt(t, org, &clock)
}

// clientAt is client with its clock reading *clock.
func (fs *fakeService) clientAt(t *testing.T, org string, clock *time.Time) *Client {
	t.Helper()
	c, err := NewClient(ClientConfig{Enrolment: worldEnrolment(t, fs.srv.URL), Organization: org, AllowPlainHTTP: true, Now: fixedClock(clock)})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// toServer sends every request to target over plain HTTP, whatever its URL
// names, so the world's https issuer can be served by a local test server.
type toServer struct {
	target *url.URL
	next   http.RoundTripper
}

func (t *toServer) RoundTrip(req *http.Request) (*http.Response, error) {
	r := req.Clone(req.Context())
	r.URL.Scheme, r.URL.Host = t.target.Scheme, t.target.Host
	return t.next.RoundTrip(r)
}

// worldTransport is a transport (the package's test hook, under the issuer
// and redirect rules) that reaches srv for the world's issuer.
func worldTransport(t *testing.T, srv *httptest.Server) http.RoundTripper {
	t.Helper()
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	return &toServer{target: u, next: http.DefaultTransport}
}

// asFailure returns err as a *Failure, failing the test otherwise.
func asFailure(t *testing.T, err error) *Failure {
	t.Helper()
	f, ok := err.(*Failure)
	if !ok {
		t.Fatalf("error %v (%T) is not a *Failure", err, err)
	}
	return f
}

// fillPlaceholders replaces each response placeholder with a value of its
// rule, as CONTRACT.md says a client's tests may.
func fillPlaceholders(raw []byte) []byte {
	r := strings.NewReplacer(
		`"{{version}}"`, `"`+strings.Repeat("0123456789abcdef", 4)+`"`,
		`"{{message}}"`, `"a message"`,
		`"{{description}}"`, `"a description"`,
		`"{{client_id}}"`, `"`+enrolledID+`"`,
		`"{{access_token}}"`, `"opaque-token"`,
	)
	return []byte(r.Replace(string(raw)))
}

// compactJSON is raw with insignificant whitespace removed.
func compactJSON(t *testing.T, raw []byte) []byte {
	t.Helper()
	var b bytes.Buffer
	if err := json.Compact(&b, raw); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}
