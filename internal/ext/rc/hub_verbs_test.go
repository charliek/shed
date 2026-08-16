package rc

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Tests for the contract-v2 verbs (POST turn / interrupt / approvals/{id}). They spin a
// real hub over a fake tmux (the package's hub-test idiom) and drive the routes over
// httptest, so the mux's own 404/405 behavior is exercised alongside the handlers.
//
// The dominant assertion is the REJECTION matrix: in this phase every kind fails every
// verb's capability check, so the tests pin the precedence that decides WHICH rejection
// a malformed-and-unsupported request gets, not just the final 409.

// verbSlugs are the sessions newVerbHub tracks — one per capability shape a verb can
// meet: a kind with a kind_features row (codex: feed+gated input, but input != "turn",
// interrupt false, approvals "tui"), a kind deliberately OMITTED from kind_features
// (shell), and a kind this binary has never heard of (a newer client's session, or a
// hand-made tmux session).
const (
	verbSlugCodex   = "vrb001"
	verbSlugShell   = "vrb002"
	verbSlugUnknown = "vrb003"
)

// newVerbHub builds a reconciled hub serving the three verb routes over httptest.
func newVerbHub(t *testing.T) *httptest.Server {
	t.Helper()
	f := newHubTmux()
	clk := &hubClock{t: time.Unix(1_700_000_000, 0).UTC()}
	h := newTestHub(f, clk)
	f.set("rc-"+verbSlugCodex, codexReadyPane(), managedEnv("id-vc", KindCodex))
	f.set("rc-"+verbSlugShell, "$ ", managedEnv("id-vs", KindShell))
	f.set("rc-"+verbSlugUnknown, "whatever", managedEnv("id-vu", Kind("some-future-kind")))
	h.reconcile()
	srv := httptest.NewServer(h.handler())
	t.Cleanup(srv.Close)
	return srv
}

// verbClient never follows redirects, so a mux-level redirect surfaces as itself
// instead of as the redirected route's answer.
var verbClient = &http.Client{
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
}

// doRequest issues one request, sending no body for an empty string.
func doRequest(t *testing.T, method, url, body string) *http.Response {
	t.Helper()
	var rdr io.Reader // typed nil would make http.NewRequest see a non-nil body
	if body != "" {
		rdr = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, url, rdr)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := verbClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

// wantEnvelope asserts the response is the hub's {error, message} envelope with the
// given status + error code and a non-empty human message.
func wantEnvelope(t *testing.T, resp *http.Response, status int, code string) {
	t.Helper()
	defer resp.Body.Close()
	var env map[string]string
	body := readAll(t, resp)
	if err := json.Unmarshal([]byte(body), &env); err != nil {
		t.Fatalf("status %d body is not the JSON error envelope: %v (%s)", resp.StatusCode, err, body)
	}
	if resp.StatusCode != status {
		t.Errorf("status = %d, want %d (body %s)", resp.StatusCode, status, body)
	}
	if env["error"] != code {
		t.Errorf("error = %q, want %q (body %s)", env["error"], code, body)
	}
	if env["message"] == "" {
		t.Errorf("envelope must carry a human message: %s", body)
	}
	if len(env) != 2 {
		t.Errorf("envelope must be exactly {error, message}: %s", body)
	}
}

// Every kind fails every verb's capability check in this phase: codex advertises
// input "gated" (not "turn"), interrupt false and approvals "tui"; shell and an unknown
// kind have no kind_features row at all, which means "no affordances" and must be just
// as final a rejection as an explicit false.
func TestHubVerbsNotSupportedForEveryKind(t *testing.T) {
	srv := newVerbHub(t)

	verbs := []struct {
		name string
		path string
		body string
	}{
		{"turn", "/turn", `{"text":"do the thing"}`},
		{"interrupt", "/interrupt", ""},
		{"approvals", "/approvals/call_01", `{"decision":"allow"}`},
	}
	for _, slug := range []string{verbSlugCodex, verbSlugShell, verbSlugUnknown} {
		for _, v := range verbs {
			t.Run(slug+"/"+v.name, func(t *testing.T) {
				resp := doRequest(t, http.MethodPost, srv.URL+"/v1/sessions/"+slug+v.path, v.body)
				wantEnvelope(t, resp, http.StatusConflict, errNotSupported)
			})
		}
	}
}

// The full rejection matrix, in precedence order: body size (413) → body validation
// (400) → approval-id validation (400) → tracked lookup (404) → capability (409).
func TestHubVerbsRejectionMatrix(t *testing.T) {
	srv := newVerbHub(t)
	base := srv.URL + "/v1/sessions/" + verbSlugCodex
	ghost := srv.URL + "/v1/sessions/ghost"
	oversized := `{"text":"` + strings.Repeat("x", 17*1024) + `"}`
	longID := strings.Repeat("a", 129)

	// Every row is a POST (the verbs are POST-only; wrong methods are their own test
	// below), so the method is implicit in the loop rather than repeated 22 times.
	cases := []struct {
		name   string
		url    string
		body   string
		status int
		code   string
	}{
		// --- size, before anything is parsed ---
		{"turn oversized", base + "/turn", oversized, http.StatusRequestEntityTooLarge, "too_large"},
		{"interrupt oversized", base + "/interrupt", oversized, http.StatusRequestEntityTooLarge, "too_large"},
		{"approval oversized", base + "/approvals/call_01", oversized, http.StatusRequestEntityTooLarge, "too_large"},
		// A body over the cap outranks every other defect: unknown slug, unparseable
		// JSON and a bad id all lose to the 413 because nothing is read past the cap.
		{"oversized outranks unknown slug", ghost + "/turn", oversized, http.StatusRequestEntityTooLarge, "too_large"},

		// --- body is exactly ONE JSON value (decodeHubBody drains to EOF) ---
		// A small valid prefix must not smuggle a tail past the precedence: an
		// oversized trailer is a 413 (the drain reads it through the MaxBytesReader),
		// a second value or garbage is a 400, and a whitespace-only tail is fine
		// (falls through to the capability 409 like a clean body).
		{"turn oversized whitespace trailer", base + "/turn", `{"text":"hi"}` + strings.Repeat(" ", 17*1024), http.StatusRequestEntityTooLarge, "too_large"},
		{"turn trailing second value", base + "/turn", `{"text":"hi"}{"text":"again"}`, http.StatusBadRequest, "invalid_json"},
		{"turn trailing garbage", base + "/turn", `{"text":"hi"} not-json`, http.StatusBadRequest, "invalid_json"},
		{"turn whitespace tail ok", base + "/turn", `{"text":"hi"}` + "  \n\t ", http.StatusConflict, "not_supported"},

		// --- body validation ---
		{"turn malformed json", base + "/turn", `{not json`, http.StatusBadRequest, "invalid_json"},
		{"turn empty body", base + "/turn", "", http.StatusBadRequest, "invalid_json"},
		{"turn missing text", base + "/turn", `{"options":{"model":"x"}}`, http.StatusBadRequest, "empty_text"},
		{"turn whitespace text", base + "/turn", `{"text":" \n\t "}`, http.StatusBadRequest, "empty_text"},
		{"approval malformed json", base + "/approvals/call_01", `{`, http.StatusBadRequest, "invalid_json"},
		{"approval missing decision", base + "/approvals/call_01", `{}`, http.StatusBadRequest, "invalid_decision"},
		{"approval unknown decision", base + "/approvals/call_01", `{"decision":"maybe"}`, http.StatusBadRequest, "invalid_decision"},
		{"approval empty decision", base + "/approvals/call_01", `{"decision":""}`, http.StatusBadRequest, "invalid_decision"},
		// Body validation runs BEFORE the id check, so a request wrong in both ways
		// reports the decision.
		{"bad decision outranks bad id", base + "/approvals/.hidden", `{"decision":"maybe"}`, http.StatusBadRequest, "invalid_decision"},

		// --- approval-id grammar (a malformed id is a bad REQUEST, never a 404) ---
		{"id leading dot", base + "/approvals/.hidden", `{"decision":"allow"}`, http.StatusBadRequest, "invalid_approval_id"},
		{"id leading dash", base + "/approvals/-x", `{"decision":"allow"}`, http.StatusBadRequest, "invalid_approval_id"},
		{"id traversal (escaped ..)", base + "/approvals/%2E%2E", `{"decision":"allow"}`, http.StatusBadRequest, "invalid_approval_id"},
		{"id with space", base + "/approvals/a%20b", `{"decision":"allow"}`, http.StatusBadRequest, "invalid_approval_id"},
		{"id too long", base + "/approvals/" + longID, `{"decision":"allow"}`, http.StatusBadRequest, "invalid_approval_id"},
		// The id check outranks the lookup: a bad id on a ghost slug is still a 400.
		{"bad id outranks unknown slug", ghost + "/approvals/.hidden", `{"decision":"allow"}`, http.StatusBadRequest, "invalid_approval_id"},

		// --- tracked lookup ---
		{"turn unknown slug", ghost + "/turn", `{"text":"hi"}`, http.StatusNotFound, "unknown_slug"},
		{"interrupt unknown slug", ghost + "/interrupt", "", http.StatusNotFound, "unknown_slug"},
		{"approval unknown slug", ghost + "/approvals/call_01", `{"decision":"deny"}`, http.StatusNotFound, "unknown_slug"},
		// …but a malformed body outranks it (nothing about which sessions exist leaks
		// through a request the hub never accepted).
		{"malformed body outranks unknown slug", ghost + "/turn", `{nope`, http.StatusBadRequest, "invalid_json"},

		// --- capability, last ---
		{"turn valid but unsupported", base + "/turn", `{"text":"hi","options":{"model":"x"}}`, http.StatusConflict, errNotSupported},
		{"interrupt valid but unsupported", base + "/interrupt", "", http.StatusConflict, errNotSupported},
		{"approval valid but unsupported", base + "/approvals/call_01HQ8Z3K.tool:2-a", `{"decision":"allow_always"}`, http.StatusConflict, errNotSupported},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			wantEnvelope(t, doRequest(t, http.MethodPost, c.url, c.body), c.status, c.code)
		})
	}
}

// The verbs are POST-only; the mux answers every other method with a 405 (an empty body
// — the envelope is the handlers' shape, not the mux's), and an unrouted path 404s.
func TestHubVerbsWrongMethodAndUnknownRoute(t *testing.T) {
	srv := newVerbHub(t)
	base := srv.URL + "/v1/sessions/" + verbSlugCodex

	cases := []struct {
		name   string
		method string
		url    string
		want   int
	}{
		{"GET turn", http.MethodGet, base + "/turn", http.StatusMethodNotAllowed},
		{"PUT turn", http.MethodPut, base + "/turn", http.StatusMethodNotAllowed},
		{"GET interrupt", http.MethodGet, base + "/interrupt", http.StatusMethodNotAllowed},
		{"DELETE approvals", http.MethodDelete, base + "/approvals/call_01", http.StatusMethodNotAllowed},
		{"GET approvals", http.MethodGet, base + "/approvals/call_01", http.StatusMethodNotAllowed},
		// An approvals request with NO id is a different (unregistered) path, not an
		// empty id — the mux 404s it before any handler runs.
		{"approvals without an id", http.MethodPost, base + "/approvals", http.StatusNotFound},
		{"unrouted verb", http.MethodPost, base + "/rewind", http.StatusNotFound},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			resp := doRequest(t, c.method, c.url, "")
			defer resp.Body.Close()
			if resp.StatusCode != c.want {
				t.Errorf("status = %d, want %d", resp.StatusCode, c.want)
			}
		})
	}
}

// A body posted to interrupt is IGNORED (the verb takes no parameters) but still
// size-capped: garbage of any shape gets the same answer as no body at all, while an
// oversized one is a 413 (covered in the matrix above).
func TestHubInterruptIgnoresBody(t *testing.T) {
	srv := newVerbHub(t)
	base := srv.URL + "/v1/sessions/" + verbSlugCodex + "/interrupt"
	for _, body := range []string{"", "{}", `{"text":"ignored"}`, `not json at all`} {
		t.Run(strconv.Quote(body), func(t *testing.T) {
			wantEnvelope(t, doRequest(t, http.MethodPost, base, body), http.StatusConflict, errNotSupported)
		})
	}
}

// rcApprovalIDRe is the contract grammar (mirrored by the server proxy's path
// classifier): it must accept the native id shapes lanes carry and reject anything that
// could be a path segment in disguise.
// One flat table rather than subtests: several ids are (deliberately) whitespace or
// path characters, which make illegible subtest names — the %q in the failure message
// identifies the case precisely.
func TestApprovalIDGrammar(t *testing.T) {
	cases := []struct {
		id string
		ok bool
	}{
		{"a", true},
		{"9", true},
		{"call_01HQ8Z3K", true},
		{"call_01HQ8Z3K.tool:2", true},
		{"req-42_a.b:c", true},
		{strings.Repeat("z", 128), true}, // the ceiling, inclusive

		{"", false},        // empty
		{".", false},       // dot segment
		{"..", false},      // traversal — excluded by the must-start-alphanumeric rule
		{"...", false},     // traversal, longer
		{".hidden", false}, // leading punctuation
		{"-lead", false},
		{"_lead", false},
		{":lead", false},
		{"a/b", false},                    // path separator
		{"a b", false},                    // whitespace
		{"a\tb", false},                   // whitespace
		{"a\nb", false},                   // whitespace
		{"a$b", false},                    // shell metacharacter
		{"a%b", false},                    // percent-escape introducer
		{"a?b", false},                    // query separator
		{"a#b", false},                    // fragment separator
		{strings.Repeat("z", 129), false}, // one past the ceiling
	}
	for _, c := range cases {
		if got := rcApprovalIDRe.MatchString(c.id); got != c.ok {
			t.Errorf("contract grammar accepted %q = %v, want %v", c.id, got, c.ok)
		}
	}
}

// The pinned SUCCESS shapes. Nothing emits them in this phase — this is the schema pin
// that stops a lane implementation from recontracting them later: the JSON a client is
// written against today is exactly the JSON it will receive.
func TestVerbSuccessShapesRoundTrip(t *testing.T) {
	pinShape(t, "turn", turnResponse{TurnID: "trn_01HQ8Z3K"}, `{"turn_id":"trn_01HQ8Z3K"}`)
	pinShape(t, "interrupt", interruptResponse{Interrupting: true}, `{"interrupting":true}`)
	pinShape(t, "approval", approvalResponse{Resolved: true, Decision: approvalDecisionAllow},
		`{"resolved":true,"decision":"allow"}`)
}

// pinShape asserts v marshals to exactly want, and that want decodes back into a T
// strictly (DisallowUnknownFields: no key may be renamed or dropped on the way in) and
// re-marshals unchanged.
func pinShape[T any](t *testing.T, name string, v T, want string) {
	t.Helper()
	t.Run(name, func(t *testing.T) {
		out, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		if string(out) != want {
			t.Errorf("marshal = %s, want %s", out, want)
		}
		back := new(T)
		dec := json.NewDecoder(strings.NewReader(want))
		dec.DisallowUnknownFields()
		if err := dec.Decode(back); err != nil {
			t.Fatalf("decoding the pinned shape failed: %v", err)
		}
		reout, err := json.Marshal(back)
		if err != nil {
			t.Fatal(err)
		}
		if string(reout) != want {
			t.Errorf("round-trip = %s, want %s", reout, want)
		}
	})
}

// The reserved rejection codes are pinned alongside the success shapes: a lane
// implementing a verb answers with THESE tokens (409 not_accepting / 409
// already_resolved / 404 unknown_approval), never a new spelling.
func TestVerbErrorCodeSpellings(t *testing.T) {
	for name, code := range map[string]string{
		"not_supported": errNotSupported, "not_accepting": errNotAccepting,
		"already_resolved": errAlreadyResolved, "unknown_approval": errUnknownApproval,
	} {
		if name != code {
			t.Errorf("error code constant for %s = %q", name, code)
		}
	}
}

// The pending_approvals overlay: a hub-layer field the one-shot list path never sets.
// No producer fills it in this phase, so the wire value is always absent — the test
// pins BOTH halves of that seam (absent by default; copied, not aliased, when a future
// lane publishes into it).
func TestHubSessionsPendingApprovalsOverlay(t *testing.T) {
	f := newHubTmux()
	clk := &hubClock{t: time.Unix(1_700_000_000, 0).UTC()}
	h := newTestHub(f, clk)
	f.set("rc-pnd001", codexReadyPane(), managedEnv("id-p", KindCodex))
	h.reconcile()

	srv := httptest.NewServer(h.handler())
	defer srv.Close()

	// Default: nothing produces approvals, so the key is absent from the wire entirely.
	resp := doRequest(t, http.MethodGet, srv.URL+"/v1/sessions", "")
	raw := readAll(t, resp)
	resp.Body.Close()
	if strings.Contains(raw, "pending_approvals") {
		t.Errorf("no producer sets pending_approvals in this phase: %s", raw)
	}

	// White-box: publish into the seam the way a lane adapter will, and confirm the
	// overlay surfaces it on the session row.
	pending := []FeedApproval{{ID: "call_01", Status: approvalStatusPending,
		Decisions: []string{approvalDecisionAllow, approvalDecisionDeny}}}
	h.trackMu.Lock()
	h.tracked["pnd001"].pendingApprovals = pending
	h.trackMu.Unlock()

	var body hubSessionsResponse
	getJSON(t, srv.URL+"/v1/sessions", &body)
	if len(body.Sessions) != 1 {
		t.Fatalf("want 1 session, got %d", len(body.Sessions))
	}
	got := body.Sessions[0].PendingApprovals
	if len(got) != 1 || got[0].ID != "call_01" || got[0].Status != approvalStatusPending ||
		len(got[0].Decisions) != 2 {
		t.Fatalf("pending_approvals overlay = %+v, want the tracked snapshot", got)
	}

	// The response row must not alias hub state (a marshaled row is a snapshot).
	h.trackMu.Lock()
	aliased := &h.tracked["pnd001"].pendingApprovals[0] == &pending[0]
	h.trackMu.Unlock()
	if !aliased {
		t.Fatal("test setup: the tracked entry should hold the published slice")
	}
	var again hubSessionsResponse
	getJSON(t, srv.URL+"/v1/sessions", &again)
	again.Sessions[0].PendingApprovals[0].ID = "mutated"
	h.trackMu.Lock()
	still := h.tracked["pnd001"].pendingApprovals[0].ID
	h.trackMu.Unlock()
	if still != "call_01" {
		t.Errorf("mutating a response row rewrote hub state (%q) — the overlay must copy", still)
	}
}
