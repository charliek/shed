package delivery_test

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/charliek/shed/internal/delivery"
)

// The hosted fixtures (CONTRACT.md, "Hosted exchanges" and "Hosted
// replies"), held to their closed languages here. The exchanges run against
// the reference account service in internal/directory/refserver; a server's
// classification of the replies is its hosted client's test.

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
	Name        string        `json:"name"`
	Description string        `json:"description"`
	Request     string        `json:"request"`
	Token       *hostedReply  `json:"token"`
	Calls       []hostedReply `json:"calls"`
	Expect      struct {
		Result string `json:"result"`
		Reason string `json:"reason"`
		Rule   string `json:"rule"`
	} `json:"expect"`
}

// checkReply holds one reply to the language: a status, or exactly one of
// silent and unreachable; at most one body form; answer and generate only
// on a call's 200 (answer on any call status, as call-created shows).
func checkReply(t *testing.T, rel, where string, r *hostedReply, call bool) {
	t.Helper()
	if r.Silent || r.Unreachable {
		if r.Status != nil || r.Headers != nil || r.Body != nil || r.Text != nil || r.Answer != "" || r.Generate != "" || (r.Silent && r.Unreachable) {
			t.Errorf("%s %s: a silent or unreachable reply has nothing else", rel, where)
		}
		return
	}
	if r.Status == nil || *r.Status < 100 || *r.Status > 599 || r.Headers == nil {
		t.Errorf("%s %s: no status or headers", rel, where)
		return
	}
	forms := 0
	for _, has := range []bool{r.Body != nil, r.Text != nil, r.Answer != "", r.Generate != ""} {
		if has {
			forms++
		}
	}
	if forms > 1 {
		t.Errorf("%s %s: more than one body form", rel, where)
	}
	if (r.Answer != "" || r.Generate != "") && !call {
		t.Errorf("%s %s: answer and generate are a call's", rel, where)
	}
	if r.Generate != "" && (*r.Status != 200 || !slices.Contains([]string{"oversize", "truncated", "echo_bearer"}, r.Generate)) {
		t.Errorf("%s %s: generate %q on %d", rel, where, r.Generate, *r.Status)
	}
	if r.Answer != "" {
		dir, name, ok := strings.Cut(r.Answer, "/")
		if !ok || (dir != "accept" && dir != "refuse") {
			t.Errorf("%s %s: answer %q is not accept/<name> or refuse/<name>", rel, where, r.Answer)
			return
		}
		if _, err := os.Stat(filepath.Join(v2, "fixtures", "answers", dir, name+".json")); err != nil {
			t.Errorf("%s %s: answer %q: %v", rel, where, r.Answer, err)
		}
	}
}

// TestHostedReplyFixtures holds every hosted/replies fixture to its closed
// language: a request fixture that exists, a token reply and calls in the
// reply language, a second call only after a 401, and an expectation that
// names a result, a failure reason the record knows and, exactly for
// answer_refused, an answer rule.
func TestHostedReplyFixtures(t *testing.T) {
	for _, rel := range jsonFiles(t, "fixtures/hosted/replies") {
		t.Run(filepath.Base(rel), func(t *testing.T) {
			var f hostedReplyFixture
			strictDecode(t, rel, &f)
			if f.Name != strings.TrimSuffix(filepath.Base(rel), ".json") || f.Description == "" {
				t.Fatalf("name %q, description %q", f.Name, f.Description)
			}
			if _, err := os.Stat(filepath.Join(v2, "fixtures", "requests", f.Request+".json")); err != nil {
				t.Fatalf("request %q: %v", f.Request, err)
			}
			if f.Token != nil {
				checkReply(t, rel, "token", f.Token, false)
			}
			if len(f.Calls) > 2 {
				t.Fatalf("%d calls: one, and one more after a 401", len(f.Calls))
			}
			for i := range f.Calls {
				checkReply(t, rel, "call", &f.Calls[i], true)
				if i == 1 && (f.Calls[0].Status == nil || *f.Calls[0].Status != 401) {
					t.Errorf("a second call after a first that is not a 401")
				}
			}
			if len(f.Calls) == 0 && f.Token == nil {
				t.Fatalf("neither a token reply nor a call")
			}
			e := f.Expect
			switch e.Result {
			case "accepted", "not_applicable":
				if e.Reason != "" || e.Rule != "" {
					t.Errorf("expect %+v: a reason or rule on %s", e, e.Result)
				}
			case "failed":
				if !slices.Contains(delivery.FailureReasons, delivery.StepReason(e.Reason)) ||
					delivery.StepReason(e.Reason) == delivery.ReasonWriteFailed || delivery.StepReason(e.Reason) == delivery.ReasonImageTooOld {
					t.Errorf("expect.reason %q is not a source failure", e.Reason)
				}
				if (e.Reason == string(delivery.ReasonAnswerRefused)) != (e.Rule != "") {
					t.Errorf("expect.rule %q with reason %q", e.Rule, e.Reason)
				}
				if e.Rule != "" && !delivery.ValidRule(delivery.Rule(e.Rule)) {
					t.Errorf("expect.rule %q is not a rule", e.Rule)
				}
			default:
				t.Errorf("expect.result %q", e.Result)
			}
		})
	}
}

// TestHostedReplyAnswersAreJudged: a reply that carries an answer fixture
// expects what that fixture's own judgement gives (accepted, or refused
// under its rule), so the two folders cannot drift apart.
func TestHostedReplyAnswersAreJudged(t *testing.T) {
	for _, rel := range jsonFiles(t, "fixtures/hosted/replies") {
		var f hostedReplyFixture
		strictDecode(t, rel, &f)
		last := f.Calls
		if len(last) == 0 || last[len(last)-1].Answer == "" || *last[len(last)-1].Status != 200 {
			continue
		}
		af := loadAnswerFixture(t, "fixtures/answers/"+last[len(last)-1].Answer+".json")
		if af.Request != f.Request {
			t.Errorf("%s: the answer fixture answers %s, the reply's request is %s", rel, af.Request, f.Request)
		}
		switch {
		case af.Expect.Result == "accepted" && f.Expect.Result != "accepted":
			t.Errorf("%s: an accepted answer, expect %+v", rel, f.Expect)
		case af.Expect.Result == "refused" && (f.Expect.Reason != string(delivery.ReasonAnswerRefused) || f.Expect.Rule != af.Expect.Rule):
			t.Errorf("%s: an answer refused under %s, expect %+v", rel, af.Expect.Rule, f.Expect)
		}
	}
}

// hostedExchangeMembers are the closed language of hosted/exchanges: the
// directory contract's exchange members, a request's body_bytes and a
// response's answer.
var (
	hostedScenarioMembers = []string{"name", "description", "before", "assertion", "token", "request", "response", "then"}
	hostedStepMembers     = []string{"before", "assertion", "token", "request", "response"}
	hostedRequestMembers  = []string{"method", "path", "headers", "body", "body_bytes"}
	hostedResponseMembers = []string{"status", "headers", "body", "text", "answer"}
	hostedSetUpSteps      = []string{"users.remove", "users.set_admin", "servers.disable", "clock.set", "lookups.fail", "limits.exhaust", "sources.set", "deliveries.deny"}
)

func membersIn(t *testing.T, where string, raw json.RawMessage, allowed []string) map[string]json.RawMessage {
	t.Helper()
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("%s: %v", where, err)
	}
	for k := range m {
		if !slices.Contains(allowed, k) {
			t.Errorf("%s: member %q is not in the language", where, k)
		}
	}
	return m
}

// TestHostedExchangeFixtures holds every hosted/exchanges fixture to its
// closed language (members, set-up steps), and checks every request body
// sent to /v1/deliveries that the server should take is a request of this
// version.
func TestHostedExchangeFixtures(t *testing.T) {
	for _, rel := range jsonFiles(t, "fixtures/hosted/exchanges") {
		t.Run(filepath.Base(rel), func(t *testing.T) {
			raw, err := os.ReadFile(filepath.Join(v2, filepath.FromSlash(rel)))
			if err != nil {
				t.Fatal(err)
			}
			top := membersIn(t, rel, raw, hostedScenarioMembers)
			var name string
			_ = json.Unmarshal(top["name"], &name)
			if name != strings.TrimSuffix(filepath.Base(rel), ".json") {
				t.Errorf("name %q", name)
			}
			steps := []map[string]json.RawMessage{top}
			if then, ok := top["then"]; ok {
				var more []json.RawMessage
				if err := json.Unmarshal(then, &more); err != nil {
					t.Fatal(err)
				}
				for i, st := range more {
					steps = append(steps, membersIn(t, rel+" then", st, hostedStepMembers))
					_ = i
				}
			}
			for _, st := range steps {
				if b, ok := st["before"]; ok {
					var list []map[string]json.RawMessage
					if err := json.Unmarshal(b, &list); err != nil {
						t.Fatal(err)
					}
					for _, s := range list {
						for k := range s {
							if len(s) != 1 || !slices.Contains(hostedSetUpSteps, k) {
								t.Errorf("set-up step %v is not one the language names", s)
							}
						}
					}
				}
				req := membersIn(t, rel+" request", st["request"], hostedRequestMembers)
				resp := membersIn(t, rel+" response", st["response"], hostedResponseMembers)
				var path string
				_ = json.Unmarshal(req["path"], &path)
				if _, ok := resp["answer"]; ok {
					if path != "/v1/deliveries" || !bytes.Equal(resp["status"], []byte("200")) {
						t.Errorf("answer on %s %s", resp["status"], path)
					}
					if _, r := delivery.ParseRequest(req["body"]); r != nil {
						t.Errorf("the request judged against is refused: %v", r)
					}
				}
			}
		})
	}
}
