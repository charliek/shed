package delivery_test

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/charliek/shed/internal/delivery"
	"github.com/charliek/shed/internal/delivery/refsource"
)

// v1 is the version 1 contract, from this package's directory.
var v1 = filepath.Join("..", "..", "tests", "provision-contract", "v1")

// jsonFiles lists the .json files directly under v1/<dir>, by their paths
// under v1/, and fails when there are none.
func jsonFiles(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(v1, filepath.FromSlash(dir)))
	if err != nil {
		t.Fatalf("list %s: %v", dir, err)
	}
	var out []string
	for _, e := range entries {
		if !e.IsDir() && filepath.Ext(e.Name()) == ".json" {
			out = append(out, dir+"/"+e.Name())
		}
	}
	if len(out) == 0 {
		t.Fatalf("no .json files in %s", dir)
	}
	return out
}

// loadAnswerFixture reads one answer fixture and checks its name is its
// file's.
func loadAnswerFixture(t *testing.T, rel string) *refsource.AnswerFixture {
	t.Helper()
	f, err := refsource.LoadAnswerFixture(filepath.Join(v1, filepath.FromSlash(rel)))
	if err != nil {
		t.Fatalf("%s: %v", rel, err)
	}
	if want := strings.TrimSuffix(filepath.Base(rel), ".json"); f.Name != want {
		t.Fatalf("%s: name %q, want %q", rel, f.Name, want)
	}
	return f
}

// fixtureRequest is the request an answer fixture is judged against: the
// request fixture it names, whose id must be the fixtures' request id.
func fixtureRequest(t *testing.T, f *refsource.AnswerFixture) *delivery.Request {
	t.Helper()
	var rf struct {
		Name        string          `json:"name"`
		Description string          `json:"description"`
		Request     json.RawMessage `json:"request"`
	}
	strictDecode(t, "fixtures/requests/"+f.Request+".json", &rf)
	req, r := delivery.ParseRequest(rf.Request)
	if r != nil {
		t.Fatalf("request %s: %v", f.Request, r)
	}
	if req.ID != refsource.FixtureRequestID {
		t.Fatalf("request %s has the id %s, not the fixtures' %s", f.Request, req.ID, refsource.FixtureRequestID)
	}
	return req
}

// whereRx is the form of a refusal's location: member names and indexes,
// or nothing.
var whereRx = regexp.MustCompile(`^$|^([a-z0-9_]+|\*)(\[[0-9]+\])?(\.([a-z0-9_]+|\*)(\[[0-9]+\])?)*$`)

// TestAcceptFixtures runs every accept fixture through ParseAnswer: each is
// accepted, and the answer it decodes to encodes to an answer that decodes
// to the same again.
func TestAcceptFixtures(t *testing.T) {
	for _, rel := range jsonFiles(t, "fixtures/answers/accept") {
		t.Run(filepath.Base(rel), func(t *testing.T) {
			f := loadAnswerFixture(t, rel)
			if f.Expect.Result != "accepted" {
				t.Fatalf("expect.result %q in accept/", f.Expect.Result)
			}
			body, err := f.Body(refsource.FixtureRequestID)
			if err != nil {
				t.Fatal(err)
			}
			a, r := delivery.ParseAnswer(body, fixtureRequest(t, f))
			if r != nil {
				t.Fatalf("refused: %v", r)
			}
			again, err := json.Marshal(a)
			if err != nil {
				t.Fatal(err)
			}
			b, r := delivery.ParseAnswer(again, fixtureRequest(t, f))
			if r != nil {
				t.Fatalf("its own encoding is refused: %v", r)
			}
			if !answersEqual(a, b) {
				t.Fatalf("round trip changed the answer")
			}
		})
	}
}

// TestRefuseFixtures runs every refuse fixture through ParseAnswer: each is
// refused under its rule, with its status, at a location that names no
// value. Every rule but body_too_large has a fixture.
func TestRefuseFixtures(t *testing.T) {
	covered := map[delivery.Rule]bool{}
	for _, rel := range jsonFiles(t, "fixtures/answers/refuse") {
		f := loadAnswerFixture(t, rel)
		covered[delivery.Rule(f.Expect.Rule)] = true
		t.Run(filepath.Base(rel), func(t *testing.T) {
			if f.Expect.Result != "refused" {
				t.Fatalf("expect.result %q in refuse/", f.Expect.Result)
			}
			rule := delivery.Rule(f.Expect.Rule)
			if !delivery.ValidRule(rule) {
				t.Fatalf("expect.rule %q is not a rule", rule)
			}
			if f.Expect.Status != rule.Status() {
				t.Fatalf("expect.status %d, but %s is %d", f.Expect.Status, rule, rule.Status())
			}
			body, err := f.Body(refsource.FixtureRequestID)
			if err != nil {
				t.Fatal(err)
			}
			_, r := delivery.ParseAnswer(body, fixtureRequest(t, f))
			if r == nil {
				t.Fatalf("accepted; want refused under %s", rule)
			}
			if r.Rule != rule {
				t.Fatalf("refused under %s (%v); want %s", r.Rule, r, rule)
			}
			if !whereRx.MatchString(r.Where) {
				t.Fatalf("location %q is not member names and indexes", r.Where)
			}
		})
	}
	for _, rule := range delivery.Rules {
		if rule != delivery.RuleBodyTooLarge && !covered[rule] {
			t.Errorf("no refuse fixture for %s", rule)
		}
	}
}

// TestFixturesAnswerAnyRequest posts every answer fixture to another
// request id, as a live harness does: each is judged as it is for the
// fixtures' own request.
func TestFixturesAnswerAnyRequest(t *testing.T) {
	const other = "00112233445566778899aabbccddeeff"
	for _, dir := range []string{"fixtures/answers/accept", "fixtures/answers/refuse"} {
		for _, rel := range jsonFiles(t, dir) {
			f := loadAnswerFixture(t, rel)
			body, err := f.Body(other)
			if err != nil {
				t.Fatalf("%s: %v", rel, err)
			}
			req := *fixtureRequest(t, f)
			req.ID = other
			_, r := delivery.ParseAnswer(body, &req)
			got := "accepted"
			if r != nil {
				got = string(r.Rule)
			}
			want := f.Expect.Rule
			if want == "" {
				want = "accepted"
			}
			if got != want {
				t.Errorf("%s posted to %s: %s, want %s", rel, other, got, want)
			}
		}
	}
}

// TestBodyTooLarge: a body over 6 MiB is refused under body_too_large
// (413), whatever it holds. Generated, not stored.
func TestBodyTooLarge(t *testing.T) {
	body := append([]byte(`{"version":1,"id":"`+refsource.FixtureRequestID+`","items":[],"problems":[],"x":"`),
		bytes.Repeat([]byte("a"), delivery.MaxBodyBytes)...)
	body = append(body, `"}`...)
	req := &delivery.Request{ID: refsource.FixtureRequestID}
	_, r := delivery.ParseAnswer(body, req)
	if r == nil || r.Rule != delivery.RuleBodyTooLarge || r.Rule.Status() != 413 {
		t.Fatalf("got %v, want body_too_large", r)
	}
	// At the limit, the body is read and judged by the other rules.
	body = body[:delivery.MaxBodyBytes]
	if _, r := delivery.ParseAnswer(body, req); r == nil || r.Rule != delivery.RuleNotJSON {
		t.Fatalf("a 6 MiB body cut short: got %v, want not_json", r)
	}
}

// TestRequestFixtures decodes every request fixture, and checks the server's
// encoding of it is the fixture's compact form: the data line a stream
// carries.
func TestRequestFixtures(t *testing.T) {
	for _, rel := range jsonFiles(t, "fixtures/requests") {
		t.Run(filepath.Base(rel), func(t *testing.T) {
			var f struct {
				Name        string          `json:"name"`
				Description string          `json:"description"`
				Request     json.RawMessage `json:"request"`
			}
			strictDecode(t, rel, &f)
			if f.Name != strings.TrimSuffix(filepath.Base(rel), ".json") || f.Description == "" {
				t.Fatalf("name %q, description %q", f.Name, f.Description)
			}
			req, r := delivery.ParseRequest(f.Request)
			if r != nil {
				t.Fatalf("refused: %v", r)
			}
			got, err := req.Marshal()
			if err != nil {
				t.Fatal(err)
			}
			var want bytes.Buffer
			if err := json.Compact(&want, f.Request); err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, want.Bytes()) {
				t.Fatalf("encodes to\n%s\nwant\n%s", got, want.Bytes())
			}
		})
	}
}

// recordFixture is one file of fixtures/records/.
type recordFixture struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Record      json.RawMessage `json:"record,omitempty"`
	RecordText  *string         `json:"record_text,omitempty"`
	Expect      struct {
		Result string `json:"result"`
		Rule   string `json:"rule,omitempty"`
	} `json:"expect"`
}

func loadRecordFixture(t *testing.T, rel string) (*recordFixture, []byte) {
	t.Helper()
	var f recordFixture
	strictDecode(t, rel, &f)
	if f.Name != strings.TrimSuffix(filepath.Base(rel), ".json") || f.Description == "" {
		t.Fatalf("name %q, description %q", f.Name, f.Description)
	}
	if (f.Record == nil) == (f.RecordText == nil) {
		t.Fatalf("exactly one of record and record_text")
	}
	if f.RecordText != nil {
		return &f, []byte(*f.RecordText)
	}
	return &f, f.Record
}

// TestRecordFixtures decodes and validates every accept record fixture:
// its state is the one its steps, last delivery and items give, and it
// survives the server's own encoding. Together they cover every state.
func TestRecordFixtures(t *testing.T) {
	seen := map[delivery.State]bool{}
	for _, rel := range jsonFiles(t, "fixtures/records/accept") {
		t.Run(filepath.Base(rel), func(t *testing.T) {
			f, body := loadRecordFixture(t, rel)
			if f.Expect.Result != "accepted" || f.Expect.Rule != "" {
				t.Fatalf("expect %+v in accept/", f.Expect)
			}
			rec, r := delivery.ParseRecord(body)
			if r != nil {
				t.Fatalf("refused: %v", r)
			}
			if err := rec.Validate(); err != nil {
				t.Fatalf("its own encoding is refused: %v", err)
			}
			seen[rec.State] = true
		})
	}
	for _, s := range delivery.States {
		if !seen[s] {
			t.Errorf("no record fixture in state %s", s)
		}
	}
}

// TestRecordRefuseFixtures: every refuse record fixture is refused under
// its rule, and every record-only rule has one.
func TestRecordRefuseFixtures(t *testing.T) {
	covered := map[delivery.Rule]bool{}
	for _, rel := range jsonFiles(t, "fixtures/records/refuse") {
		f, body := loadRecordFixture(t, rel)
		covered[delivery.Rule(f.Expect.Rule)] = true
		t.Run(filepath.Base(rel), func(t *testing.T) {
			if f.Expect.Result != "refused" || f.Expect.Rule == "" {
				t.Fatalf("expect %+v in refuse/", f.Expect)
			}
			rule := delivery.Rule(f.Expect.Rule)
			if !delivery.ValidRule(rule) && !slices.Contains(delivery.RecordRules, rule) {
				t.Fatalf("expect.rule %q is not a rule", rule)
			}
			_, r := delivery.ParseRecord(body)
			if r == nil {
				t.Fatalf("accepted; want refused under %s", rule)
			}
			if r.Rule != rule {
				t.Fatalf("refused under %s (%v); want %s", r.Rule, r, rule)
			}
			if !whereRx.MatchString(r.Where) {
				t.Fatalf("location %q is not member names and indexes", r.Where)
			}
		})
	}
	for _, rule := range delivery.RecordRules {
		if !covered[rule] {
			t.Errorf("no record refuse fixture for %s", rule)
		}
	}
}

// TestStreamFixtures reads every stream transcript as the contract writes
// a stream: a 200 with its three headers, chunks in time order that are
// each a keepalive comment (on the 15-second beat) or one request event
// whose data is a request on one line, and the requests expect names, in
// order.
func TestStreamFixtures(t *testing.T) {
	for _, rel := range jsonFiles(t, "fixtures/stream") {
		t.Run(filepath.Base(rel), func(t *testing.T) {
			tr, err := refsource.LoadTranscript(filepath.Join(v1, filepath.FromSlash(rel)))
			if err != nil {
				t.Fatal(err)
			}
			if tr.Name != strings.TrimSuffix(filepath.Base(rel), ".json") || tr.Description == "" {
				t.Fatalf("name %q, description %q", tr.Name, tr.Description)
			}
			h := tr.Response.Headers
			if tr.Response.Status != 200 || len(h) != 3 || h["Content-Type"] != "text/event-stream" ||
				h["Cache-Control"] != "no-store" || !delivery.ValidID(h[delivery.HeaderStream]) {
				t.Fatalf("response %d %v", tr.Response.Status, h)
			}
			var ids []string
			last := 0
			for i, c := range tr.Chunks {
				if c.AtMS < last {
					t.Fatalf("chunk %d at %d ms, before the one before it", i, c.AtMS)
				}
				last = c.AtMS
				if c.Text == delivery.KeepaliveComment {
					if c.AtMS == 0 || c.AtMS%int(delivery.KeepaliveInterval.Milliseconds()) != 0 {
						t.Fatalf("keepalive at %d ms is off the 15 s beat", c.AtMS)
					}
					continue
				}
				data, ok := strings.CutPrefix(c.Text, "event: request\ndata: ")
				if !ok || !strings.HasSuffix(data, "\n\n") || strings.Count(data, "\n") != 2 {
					t.Fatalf("chunk %d is neither a keepalive nor one request event: %q", i, c.Text)
				}
				data = strings.TrimSuffix(data, "\n\n")
				req, r := delivery.ParseRequest([]byte(data))
				if r != nil {
					t.Fatalf("chunk %d: request refused: %v", i, r)
				}
				enc, _ := req.Marshal()
				if string(enc) != data {
					t.Fatalf("chunk %d: data is not the request's compact encoding", i)
				}
				ids = append(ids, req.ID)
			}
			if strings.Join(ids, ",") != strings.Join(tr.Expect.Requests, ",") {
				t.Fatalf("requests %v, expect %v", ids, tr.Expect.Requests)
			}
		})
	}
}

// strictDecode decodes a fixture with no member outside v.
func strictDecode(t *testing.T, rel string, v any) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(v1, filepath.FromSlash(rel)))
	if err != nil {
		t.Fatal(err)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		t.Fatalf("%s: %v", rel, err)
	}
}

func answersEqual(a, b *delivery.Answer) bool { return reflect.DeepEqual(a, b) }
