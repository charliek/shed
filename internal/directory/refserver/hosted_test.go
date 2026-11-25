package refserver

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/charliek/shed/internal/delivery"
	"github.com/charliek/shed/internal/delivery/refsource"
)

// The hosted source (the provision contract, version 2, section 3b), held
// to that contract's hosted fixtures: every exchange of hosted/exchanges
// runs against the reference service in the directory contract's world, and
// every call reply of hosted/replies the service can be told to send is
// sent and compared. These tests may import shed packages (the judgement of
// an answer is internal/delivery's); the service's own code imports none.

// provisionDir is tests/provision-contract/v2 from this package's directory.
const provisionDir = "../../../tests/provision-contract/v2"

func readProvision(t *testing.T, rel string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(provisionDir, filepath.FromSlash(rel)))
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	return b
}

func provisionFiles(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(provisionDir, filepath.FromSlash(dir)))
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".json" {
			out = append(out, dir+"/"+e.Name())
		}
	}
	if len(out) == 0 {
		t.Fatalf("no .json files in %s", dir)
	}
	sort.Strings(out)
	return out
}

// cannedItems are the items sources.set gives a person: those of the accept
// fixture typical, an answer to a request with a project directory.
func cannedItems(t *testing.T) json.RawMessage {
	t.Helper()
	var f struct {
		Answer struct {
			Items json.RawMessage `json:"items"`
		} `json:"answer"`
	}
	if err := json.Unmarshal(readProvision(t, "fixtures/answers/accept/typical.json"), &f); err != nil {
		t.Fatal(err)
	}
	return f.Answer.Items
}

// serverID is the client id of the world server named name.
func (s *scenario) serverID(name string) string {
	s.srv.mu.Lock()
	defer s.srv.mu.Unlock()
	for id, sv := range s.srv.servers {
		if sv.name == name {
			return id
		}
	}
	s.t.Fatalf("no server %q", name)
	return ""
}

// judgeAnswer applies the response member answer: the 200's body, judged
// under the answer rules against the request this step sent, is accepted.
func (s *scenario) judgeAnswer(where string, want json.RawMessage, got []byte) {
	var spec struct {
		Result string `json:"result"`
	}
	mustDecode(s.t, want, &spec)
	if spec.Result != "accepted" {
		s.t.Fatalf("%s: answer.result %q: only accepted is in the language", where, spec.Result)
	}
	req, r := delivery.ParseRequest(s.sent)
	if r != nil {
		s.t.Fatalf("%s: the request this step sent is refused: %v", where, r)
	}
	if _, r := delivery.ParseAnswer(got, req); r != nil {
		s.t.Errorf("%s: the answer is refused under %s: %v", where, r.Rule, r)
	}
}

// TestHostedExchanges runs every hosted exchange against the reference
// service.
func TestHostedExchanges(t *testing.T) {
	h := newHarness(t)
	canned := cannedItems(t)
	for _, rel := range provisionFiles(t, "fixtures/hosted/exchanges") {
		raw := readProvision(t, rel)
		t.Run(strings.TrimSuffix(filepath.Base(rel), ".json"), func(t *testing.T) {
			var ex exchange
			decodeClosed(t, rel, raw, &ex)
			s := h.newScenario(t, &ex)
			s.hosted, s.canned = true, canned
			s.run(&ex)
		})
	}
}

// hostedReply is one reply of a hosted/replies fixture.
type hostedReply struct {
	Status      *int              `json:"status"`
	Headers     map[string]string `json:"headers"`
	Body        json.RawMessage   `json:"body"`
	Text        *string           `json:"text"`
	Answer      string            `json:"answer"`
	Generate    string            `json:"generate"`
	Silent      bool              `json:"silent"`
	Unreachable bool              `json:"unreachable"`
}

type hostedReplyFixture struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Request     string          `json:"request"`
	Token       json.RawMessage `json:"token"`
	Calls       []hostedReply   `json:"calls"`
	Expect      struct {
		Result string `json:"result"`
		Reason string `json:"reason"`
		Rule   string `json:"rule"`
	} `json:"expect"`
}

// deliveryFor is the Delivery that tells the service to send reply, for a
// request whose answer fixtures are read from the contract.
func deliveryFor(t *testing.T, reply hostedReply) Delivery {
	t.Helper()
	status := 0
	if reply.Status != nil {
		status = *reply.Status
	}
	switch {
	case reply.Silent:
		return Delivery{Mode: ModeSilent}
	case reply.Generate == "oversize":
		return Delivery{Mode: ModeOversize}
	case reply.Generate == "truncated":
		return Delivery{Mode: ModeTruncated}
	case reply.Generate == "echo_bearer":
		return Delivery{Mode: ModeEcho}
	case reply.Answer != "":
		f, err := refsource.LoadAnswerFixture(filepath.Join(provisionDir, "fixtures", "answers", filepath.FromSlash(reply.Answer)+".json"))
		if err != nil {
			t.Fatal(err)
		}
		body, err := f.Body(refsource.FixtureRequestID)
		if err != nil {
			t.Fatal(err)
		}
		return Delivery{Mode: ModeBody, Status: status, BodyBase64: base64.StdEncoding.EncodeToString(body)}
	default:
		return Delivery{Mode: ModeStatus, Status: status, Headers: reply.Headers, Body: reply.Body, Text: reply.Text}
	}
}

// TestHostedRepliesTheServiceSends: for every call reply of hosted/replies
// but an unreachable service, the reference service is told (through its
// Delivery inputs, as deliveries.json tells it) to send it, a real HTTP
// call is made with a delivery token, and what arrives is that reply: its
// status, the headers the fixture names, and its body (an answer judged
// under the answer rules as the fixture expects; an oversize body of 6 MiB
// and one byte; a truncated body that ends early; an echo that holds the
// bearer; silence until the client gives up).
func TestHostedRepliesTheServiceSends(t *testing.T) {
	h := newHarness(t)
	n := 0
	for _, rel := range provisionFiles(t, "fixtures/hosted/replies") {
		var f hostedReplyFixture
		decodeClosed(t, rel, readProvision(t, rel), &f)
		if len(f.Calls) == 0 {
			continue // a token reply: the service's token endpoint is held to the exchanges
		}
		for i, reply := range f.Calls {
			if reply.Unreachable {
				continue
			}
			n++
			t.Run(fmt.Sprintf("%s/call-%d", f.Name, i), func(t *testing.T) {
				runReply(t, h, f, reply, i == len(f.Calls)-1)
			})
		}
	}
	if n == 0 {
		t.Fatal("no call replies")
	}
}

func runReply(t *testing.T, h *harness, f hostedReplyFixture, reply hostedReply, final bool) {
	var ex exchange
	ex.Name = f.Name
	s := h.newScenario(t, &ex)
	s.hosted = true
	s.in.sources["ada"] = deliveryFor(t, reply)
	tok := s.accessToken(json.RawMessage(`{"client_id":"srv_0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a","scope":["deliveries:request"]}`))

	var rf struct {
		Request json.RawMessage `json:"request"`
	}
	if err := json.Unmarshal(readProvision(t, "fixtures/requests/"+f.Request+".json"), &rf); err != nil {
		t.Fatal(err)
	}
	req, r := delivery.ParseRequest(rf.Request)
	if r != nil {
		t.Fatal(r)
	}
	// Another id than the fixtures' own, so a reply that does not answer
	// this request is seen; the world's clock is fixed, and the request's
	// deadline must be after it.
	req.ID = "0f1e2d3c4b5a69788796a5b4c3d2e1f0"
	req.Deadline = s.clock.Add(time.Minute).UTC().Format(time.RFC3339)
	body, err := req.Marshal()
	if err != nil {
		t.Fatal(err)
	}

	ts := httptest.NewServer(s.srv)
	defer ts.Close()
	client := ts.Client()
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	if reply.Silent {
		client.Timeout = 300 * time.Millisecond
	}
	hr, _ := http.NewRequest(http.MethodPost, ts.URL+pathDeliveries, bytes.NewReader(body))
	hr.Header.Set("Content-Type", "application/json")
	hr.Header.Set("Authorization", "Bearer "+tok)
	res, err := client.Do(hr)
	if reply.Silent {
		if err == nil {
			res.Body.Close()
			t.Fatalf("a silent reply answered %d", res.StatusCode)
		}
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	got, rerr := io.ReadAll(res.Body)
	if res.StatusCode != *reply.Status {
		t.Fatalf("status %d, want %d (%s)", res.StatusCode, *reply.Status, got)
	}
	for k, v := range reply.Headers {
		if g := res.Header.Get(k); g != v {
			t.Errorf("%s: %q, want %q", k, g, v)
		}
	}
	switch {
	case reply.Generate == "truncated":
		if !errors.Is(rerr, io.ErrUnexpectedEOF) {
			t.Fatalf("a truncated body read to its end: %v, %d bytes", rerr, len(got))
		}
		return
	case rerr != nil:
		t.Fatal(rerr)
	case reply.Generate == "oversize":
		if len(got) != delivery.MaxBodyBytes+1 {
			t.Fatalf("an oversize body of %d bytes, want %d", len(got), delivery.MaxBodyBytes+1)
		}
	case reply.Generate == "echo_bearer":
		if !bytes.Contains(got, []byte(tok)) {
			t.Fatal("the echo does not hold the bearer")
		}
		if _, r := delivery.ParseAnswer(got, req); r != nil {
			t.Fatalf("the echo is not otherwise a valid answer: %v", r)
		}
	case reply.Text != nil:
		if string(got) != *reply.Text {
			t.Fatalf("body %q, want %q", got, *reply.Text)
		}
	case reply.Body != nil:
		var a, b any
		if json.Unmarshal(got, &a) != nil || json.Unmarshal(reply.Body, &b) != nil || !reflect.DeepEqual(a, b) {
			t.Fatalf("body %s, want %s", got, reply.Body)
		}
	}
	// An answer the server judges: it is judged as the fixture expects.
	if reply.Answer != "" && *reply.Status == http.StatusOK && final {
		_, r := delivery.ParseAnswer(got, req)
		switch f.Expect.Result {
		case "accepted":
			if r != nil {
				t.Fatalf("refused under %s, want accepted", r.Rule)
			}
		default:
			if r == nil || string(r.Rule) != f.Expect.Rule {
				t.Fatalf("judged %v, want refused under %s", r, f.Expect.Rule)
			}
		}
	}
}

// TestDeliveriesControlFile: deliveries.json and no-deliveries in a control
// directory are read on every request, strictly.
func TestDeliveriesControlFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	c, err := OpenControlDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if _, found, err := c.Delivery("ada"); err != nil || found {
		t.Fatalf("no deliveries.json: found %v, err %v", found, err)
	}
	if denied, err := c.DeliveriesDenied("srv_0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a"); err != nil || denied {
		t.Fatalf("no no-deliveries: denied %v, err %v (every server may ask by default)", denied, err)
	}
	write("deliveries.json", `{"ada":{"items":[]},"linus":"NO_SOURCE","turing":{"mode":"status","status":503,"body":{"error":{"code":"UNAVAILABLE","message":"x"}}}}`)
	if d, found, err := c.Delivery("ada"); err != nil || !found || d.Mode != ModeAnswer {
		t.Fatalf("ada: %+v %v %v", d, found, err)
	}
	if _, found, err := c.Delivery("linus"); err != nil || found {
		t.Fatalf("linus (NO_SOURCE): found %v, err %v", found, err)
	}
	if d, found, err := c.Delivery("turing"); err != nil || !found || d.Status != 503 {
		t.Fatalf("turing: %+v %v %v", d, found, err)
	}
	write("no-deliveries", "srv_0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a\n")
	if denied, err := c.DeliveriesDenied("srv_0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a"); err != nil || !denied {
		t.Fatalf("listed: denied %v, err %v", denied, err)
	}
	for _, bad := range []string{
		`{"ada":{"mode":"shout"}}`,
		`{"ada":{"items":[],"x":1}}`,
		`{"ada":"nothing"}`,
		`{"ada":{"mode":"status"}}`,
		`{"ada":{"items":[]},"ada":{"items":[]}}`,
	} {
		write("deliveries.json", bad)
		if _, _, err := c.Delivery("ada"); err == nil {
			t.Errorf("%s: accepted", bad)
		}
	}
}

// TestDeliveryRequestParseAgreesOnMembers: a request the contract's parser
// refuses for a missing or wrongly typed member, or an unknown one, is a
// bad request to the reference service too; every request fixture is
// taken, but the implicit user's (a server never sends a hosted source an
// empty user, section 3b).
func TestDeliveryRequestParseAgreesOnMembers(t *testing.T) {
	var rf struct {
		Request json.RawMessage `json:"request"`
	}
	for _, rel := range provisionFiles(t, "fixtures/requests") {
		if err := json.Unmarshal(readProvision(t, rel), &rf); err != nil {
			t.Fatal(err)
		}
		var u struct {
			User string `json:"user"`
		}
		_ = json.Unmarshal(rf.Request, &u)
		_, err := parseDeliveryRequest(rf.Request)
		switch {
		case u.User == "" && err == nil:
			t.Errorf("%s: an empty user (never sent to a hosted source) is taken", rel)
		case u.User != "" && err != nil:
			t.Errorf("%s: refused: %v", rel, err)
		}
	}
	base := string(rf.Request)
	cases := map[string]string{
		"shed empty":          `"shed":{}`,
		"shed no name":        `"shed":{"id":"q6c3","backend":"vz"}`,
		"shed no id":          `"shed":{"name":"w","backend":"vz"}`,
		"shed no backend":     `"shed":{"name":"w","id":"q6c3"}`,
		"shed name a number":  `"shed":{"name":7,"id":"q6c3","backend":"vz"}`,
		"shed id a number":    `"shed":{"name":"w","id":7,"backend":"vz"}`,
		"shed backend number": `"shed":{"name":"w","id":"q6c3","backend":7}`,
		"shed backend other":  `"shed":{"name":"w","id":"q6c3","backend":"qemu"}`,
		"shed unknown member": `"shed":{"name":"w","id":"q6c3","backend":"vz","sk-secret":1}`,
		"shed repo a string":  `"shed":{"name":"w","id":"q6c3","backend":"vz","repo":"x"}`,
		"shed repo no url":    `"shed":{"name":"w","id":"q6c3","backend":"vz","repo":{"name":"a/b"}}`,
		"shed local_dir num":  `"shed":{"name":"w","id":"q6c3","backend":"vz","local_dir":3}`,
		"shed a string":       `"shed":"widgets"`,
		// The value rules request.go applies.
		"shed id upper case":     `"shed":{"name":"w","id":"BAD","backend":"vz"}`,
		"shed id a slash":        `"shed":{"name":"w","id":"bad/id","backend":"vz"}`,
		"shed id too long":       `"shed":{"name":"w","id":"` + strings.Repeat("a", 65) + `","backend":"vz"}`,
		"shed name empty":        `"shed":{"name":"","id":"q6c3","backend":"vz"}`,
		"shed name control":      `"shed":{"name":"a\nb","id":"q6c3","backend":"vz"}`,
		"repo name one part":     `"shed":{"name":"w","id":"q6c3","backend":"vz","repo":{"url":"u","name":"owner"}}`,
		"repo name three parts":  `"shed":{"name":"w","id":"q6c3","backend":"vz","repo":{"url":"u","name":"a/b/c"}}`,
		"repo name empty owner":  `"shed":{"name":"w","id":"q6c3","backend":"vz","repo":{"url":"u","name":"/b"}}`,
		"repo url empty":         `"shed":{"name":"w","id":"q6c3","backend":"vz","repo":{"url":""}}`,
		"local_dir dot dot":      `"shed":{"name":"w","id":"q6c3","backend":"vz","local_dir":".."}`,
		"local_dir a slash":      `"shed":{"name":"w","id":"q6c3","backend":"vz","local_dir":"a/b"}`,
		"project_dir relative":   `"shed":{"name":"w","id":"q6c3","backend":"vz","project_dir":"relative"}`,
		"project_dir dot dot":    `"shed":{"name":"w","id":"q6c3","backend":"vz","project_dir":"/home/../etc"}`,
		"project_dir trailing /": `"shed":{"name":"w","id":"q6c3","backend":"vz","project_dir":"/home/shed/"}`,
		"project_dir backslash":  `"shed":{"name":"w","id":"q6c3","backend":"vz","project_dir":"/home\\shed"}`,
		// Malformed text, refused and never repaired (the decoder would
		// make each U+FFFD).
		"name unpaired high surrogate": `"shed":{"name":"\ud800","id":"q6c3","backend":"vz"}`,
		"name lone low surrogate":      `"shed":{"name":"a\udc00","id":"q6c3","backend":"vz"}`,
		"name surrogates reversed":     `"shed":{"name":"\udc00\ud800","id":"q6c3","backend":"vz"}`,
		"name high then another":       `"shed":{"name":"\ud800\u0041","id":"q6c3","backend":"vz"}`,
	}
	shedAt := strings.Index(base, `"shed":`)
	// A pair, and an escaped backslash before a u, are text: taken.
	for _, shed := range []string{`"shed":{"name":"\ud83d\ude00","id":"q6c3","backend":"vz"}`, `"shed":{"name":"a\\ud800","id":"q6c3","backend":"vz"}`} {
		body := []byte(base[:shedAt] + shed + "}")
		if _, r := delivery.ParseRequest(body); r != nil {
			t.Fatalf("%s: the contract's parser refuses it: %v", shed, r)
		}
		if _, err := parseDeliveryRequest(body); err != nil {
			t.Errorf("%s: refused: %v", shed, err)
		}
	}
	for name, shed := range cases {
		body := []byte(base[:shedAt] + shed + "}")
		if _, r := delivery.ParseRequest(body); r == nil {
			t.Fatalf("%s: the contract's parser takes it; the case proves nothing", name)
		}
		if _, err := parseDeliveryRequest(body); err == nil {
			t.Errorf("%s: taken", name)
		} else if err != errNotARequest {
			t.Errorf("%s: %v, not the one fixed classification", name, err)
		}
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(rf.Request, &top); err != nil {
		t.Fatal(err)
	}
	for _, m := range []string{"version", "id", "reason", "deadline", "user", "shed"} {
		for _, mutate := range []string{"absent", "number"} {
			doc := map[string]json.RawMessage{}
			for k, v := range top {
				doc[k] = v
			}
			if mutate == "absent" {
				delete(doc, m)
			} else {
				doc[m] = json.RawMessage(`[1]`)
			}
			body, _ := json.Marshal(doc)
			if _, err := parseDeliveryRequest(body); err == nil {
				t.Errorf("%s %s: taken", m, mutate)
			}
		}
	}
}

// TestDeliveryDeadlineAfterTheBody: a request whose body arrives after its
// deadline is 410 DEADLINE_PASSED, though it began before the deadline:
// the deadline is judged on the clock read after the body, and again just
// before the reply.
func TestDeliveryDeadlineAfterTheBody(t *testing.T) {
	h := newHarness(t)
	var ex exchange
	ex.Name = "deadline-after-the-body"
	s := h.newScenario(t, &ex)
	s.hosted = true
	s.in.sources["ada"] = Delivery{Items: cannedItems(t)}
	tok := s.accessToken(json.RawMessage(`{"client_id":"srv_0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a","scope":["deliveries:request"]}`))
	start := s.clock
	for _, c := range []struct {
		name     string
		after    int // the clock calls that still read the start
		noSource bool
		want     int
	}{
		{"in time", 1 << 30, false, http.StatusOK},
		{"body after the deadline", 2, false, http.StatusGone},  // the bearer and the limit read the start
		{"reply after the deadline", 3, false, http.StatusGone}, // the body arrived in time
		{"no source, in time", 1 << 30, true, http.StatusNotFound},
		// The body arrived in time; the lookup that found no source ended
		// after the deadline: not 404 NO_SOURCE but 410.
		{"no source, lookup after the deadline", 3, true, http.StatusGone},
	} {
		t.Run(c.name, func(t *testing.T) {
			s.in.mu.Lock()
			if c.noSource {
				delete(s.in.sources, "ada")
			} else {
				s.in.sources["ada"] = Delivery{Items: cannedItems(t)}
			}
			s.in.mu.Unlock()
			calls := 0
			s.srv.now = func() time.Time {
				calls++
				if calls > c.after {
					return start.Add(time.Minute)
				}
				return start
			}
			body := `{"version":2,"id":"0f1e2d3c4b5a69788796a5b4c3d2e1f0","reason":"create","deadline":"` +
				start.Add(30*time.Second).UTC().Format(time.RFC3339) +
				`","user":"ada","shed":{"name":"widgets","id":"q6c3","backend":"vz"}}`
			req := httptest.NewRequest(http.MethodPost, pathDeliveries, strings.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Authorization", "Bearer "+tok)
			rec := httptest.NewRecorder()
			s.srv.ServeHTTP(rec, req)
			if rec.Code != c.want {
				t.Fatalf("%d %s, want %d", rec.Code, rec.Body, c.want)
			}
		})
	}
}

// TestDeliveryCallIsLogged: every POST /v1/deliveries logs DeliveryCallLine
// once, a call refused at its token included, and nothing else does (the
// integration suite counts the line).
func TestDeliveryCallIsLogged(t *testing.T) {
	s := newWorld(t)
	var logged []string
	s.srv.logf = func(format string, args ...any) { logged = append(logged, fmt.Sprintf(format, args...)) }
	count := func() int {
		n := 0
		for _, l := range logged {
			if l == DeliveryCallLine {
				n++
			}
		}
		return n
	}
	if rec := s.post(pathDeliveries, "application/json", "{}"); rec.Code != http.StatusUnauthorized {
		t.Fatalf("a call with no token: %d", rec.Code)
	}
	if n := count(); n != 1 {
		t.Fatalf("one call logged %d delivery call lines: %q", n, logged)
	}
	s.directory("Bearer " + s.signBearer(s.bearerParts(srvA)))
	if n := count(); n != 1 {
		t.Fatalf("a directory request logged a delivery call line: %q", logged)
	}
}
