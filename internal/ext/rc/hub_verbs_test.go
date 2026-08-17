package rc

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"sync"
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
	verbSlugClaude  = "vrb004"
	verbSlugCursor  = "vrb005"
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
	f.set("rc-"+verbSlugClaude, "claude\n> ", managedEnv("id-vcl", KindClaudeRC))
	f.set("rc-"+verbSlugCursor, "cursor\n> ", managedEnv("id-vcu", KindCursor))
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

// verbCases is the {turn, interrupt, approvals} triple — one valid request per verb,
// written down ONCE so a fourth verb (or a changed route/body shape) is a single edit
// rather than a hunt through every test that drives "all three".
var verbCases = []struct{ name, path, body string }{
	{"turn", "/turn", `{"text":"do the thing"}`},
	{"interrupt", "/interrupt", ""},
	{"approvals", "/approvals/per_1", `{"decision":"allow"}`},
}

// wantAllVerbs drives every verb against one session and asserts they all answer the
// SAME envelope — the shape of every "this session cannot serve the verbs" case.
func wantAllVerbs(t *testing.T, base string, status int, code string) {
	t.Helper()
	for _, v := range verbCases {
		t.Run(v.name, func(t *testing.T) {
			wantEnvelope(t, doRequest(t, http.MethodPost, base+v.path, v.body), status, code)
		})
	}
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

// EVERY kind except opencode fails every verb's capability check: codex advertises input
// "gated" (not "turn"), interrupt false and approvals "tui"; claude-rc and cursor
// advertise no feed input at all; shell and an unknown kind have no kind_features row,
// which means "no affordances" and must be just as final a rejection as an explicit
// false. (opencode's live lane is covered by the opencode section at the end of this
// file.)
func TestHubVerbsNotSupportedForEveryOtherKind(t *testing.T) {
	srv := newVerbHub(t)

	for _, slug := range []string{verbSlugCodex, verbSlugShell, verbSlugUnknown, verbSlugClaude, verbSlugCursor} {
		t.Run(slug, func(t *testing.T) {
			wantAllVerbs(t, srv.URL+"/v1/sessions/"+slug, http.StatusConflict, errNotSupported)
		})
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

// ApprovalIDRe is the contract grammar (mirrored by the server proxy's path
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
		if got := ApprovalIDRe.MatchString(c.id); got != c.ok {
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
// Nothing publishes it for a codex session (its approvals are answered in the TUI), so
// the wire value stays absent — the test pins BOTH halves of the seam (absent by
// default; copied, not aliased, when a lane publishes into it).
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

	// The per-element Decisions slice must be copied too, not just the outer
	// slice — otherwise a lane rewriting its tracked snapshot's Decisions in place
	// would rewrite a response the handler already built. Exercised against the
	// copy helper directly: the handler's copy is gone by the time the response is
	// readable, so this is the only place the pre-encode row can be observed.
	h.trackMu.Lock()
	snapshot := copyApprovals(h.tracked["pnd001"].pendingApprovals)
	h.tracked["pnd001"].pendingApprovals[0].Decisions[0] = "mutated"
	h.trackMu.Unlock()
	if snapshot[0].Decisions[0] != approvalDecisionAllow {
		t.Errorf("copied row aliases the tracked Decisions slice (got %q) — deep-copy required",
			snapshot[0].Decisions[0])
	}
}

// Reconcile republishes the lane's OPEN approvals into the session's pending_approvals
// snapshot every tick (approvalPublisher). Pending-only by wire contract: a resolved entry
// disappears from the snapshot, and its resolution state is answered by the watcher instead.
// A watcher that does not publish leaves the field alone — it is not a blanket writer.
func TestReconcilePublishesPendingApprovals(t *testing.T) {
	f := newHubTmux()
	clk := &hubClock{t: time.Unix(1_700_000_000, 0).UTC()}
	h := newTestHub(f, clk)
	f.set("rc-apv001", codexReadyPane(), managedEnv("id-a", KindCodex))
	h.reconcile()

	// White-box: attach a publishing watcher the way ensureWatcher commits a real one.
	pub := &stubApprovalWatcher{stubWatcher: stubWatcher{activity: ActivityNeedsApproval, fresh: true}}
	pub.approvals = []FeedApproval{{ID: "per_1", Status: approvalStatusPending,
		Decisions: []string{approvalDecisionAllow, approvalDecisionAllowAlways, approvalDecisionDeny}}}
	h.trackMu.Lock()
	h.tracked["apv001"].watcher = pub
	h.trackMu.Unlock()

	h.reconcile()
	h.trackMu.Lock()
	got := h.tracked["apv001"].pendingApprovals
	h.trackMu.Unlock()
	if len(got) != 1 || got[0].ID != "per_1" || got[0].Status != approvalStatusPending {
		t.Fatalf("pendingApprovals = %+v, want the lane's one open ask", got)
	}

	// It surfaces on the wire through the /v1/sessions overlay.
	srv := httptest.NewServer(h.handler())
	defer srv.Close()
	var body hubSessionsResponse
	getJSON(t, srv.URL+"/v1/sessions", &body)
	if len(body.Sessions) != 1 || len(body.Sessions[0].PendingApprovals) != 1 ||
		body.Sessions[0].PendingApprovals[0].ID != "per_1" {
		t.Fatalf("session row pending_approvals = %+v, want the published snapshot", body.Sessions)
	}

	// Resolution is a REMOVAL from the snapshot (never a resolved entry in it).
	pub.approvals = nil
	h.reconcile()
	h.trackMu.Lock()
	got = h.tracked["apv001"].pendingApprovals
	h.trackMu.Unlock()
	if len(got) != 0 {
		t.Fatalf("pendingApprovals after the lane cleared it = %+v, want empty", got)
	}

	// A non-publishing watcher must not blank a snapshot it does not own.
	h.trackMu.Lock()
	h.tracked["apv001"].watcher = &stubWatcher{activity: ActivityIdle}
	h.tracked["apv001"].pendingApprovals = []FeedApproval{{ID: "pane-1", Status: approvalStatusPending}}
	h.trackMu.Unlock()
	h.reconcile()
	h.trackMu.Lock()
	got = h.tracked["apv001"].pendingApprovals
	h.trackMu.Unlock()
	if len(got) != 1 || got[0].ID != "pane-1" {
		t.Fatalf("pendingApprovals = %+v, want the non-lane entry retained", got)
	}
}

// ---- the opencode lane, end to end over the real mux ----
//
// These drive the verbs through the hub's own handler chain against a tracked opencode
// session whose watcher is a REAL opencodeWatcher talking to the fake embedded server —
// so the success shapes, the rejection matrix, and the WS-B scoping guard (the fake fails
// the test on any unscoped POST) are all exercised on the path a client actually takes.

const verbSlugOpencode = "vrb010"

// opencodeVerbEnv is a managed opencode session's tmux env plus the two keys the watcher
// needs: the create-time embedded-server port, and a prior back-written agent session id
// — the latter so the watcher is PINNED synchronously at construction and a handler test
// never races SSE discovery.
func opencodeVerbEnv(id string, port int, agentSession string) string {
	env := managedEnv(id, KindOpencode) + envOpencodePort + "=" + strconv.Itoa(port) + "\n"
	if agentSession != "" {
		env += envAgentSession + "=" + agentSession + "\n"
	}
	return env
}

// newOpencodeVerbHub reconciles a hub tracking one live opencode session against f and
// returns the served hub plus its committed watcher. agentSession "" leaves the watcher
// UNPINNED (the uncorrelated-TUI case).
func newOpencodeVerbHub(t *testing.T, f *fakeOpencode, agentSession string) (*httptest.Server, *Hub, *opencodeWatcher, *hubClock) {
	t.Helper()
	f.holdOpenSSE()
	tm := newHubTmux()
	clk := opencodeClock()
	h := newTestHub(tm, clk)
	tm.set("rc-"+verbSlugOpencode, opencodeReadyPane(), opencodeVerbEnv("id-vo", f.port(t), agentSession))
	h.reconcile() // ensureWatcher builds + commits the opencode watcher

	h.trackMu.Lock()
	watcher, _ := h.tracked[verbSlugOpencode].watcher.(*opencodeWatcher)
	h.trackMu.Unlock()
	if watcher == nil {
		t.Fatal("reconcile did not commit an opencode watcher for the tracked session")
	}
	t.Cleanup(watcher.close) // this hub never ticks again, so nothing else closes it

	srv := httptest.NewServer(h.handler())
	t.Cleanup(srv.Close)
	return srv, h, watcher, clk
}

// wantJSON asserts a response's status and its EXACT decoded body (key set included).
func wantJSON(t *testing.T, resp *http.Response, status int, want map[string]any) {
	t.Helper()
	defer resp.Body.Close()
	body := readAll(t, resp)
	if resp.StatusCode != status {
		t.Fatalf("status = %d, want %d (body %s)", resp.StatusCode, status, body)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("body is not JSON: %v (%s)", err, body)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("body = %s, want %v", body, want)
	}
}

// waitForAsk drives the watcher until the ask the fake announced (f.streamAsk) has reached
// the fold, so the approvals verb has a PENDING entry to resolve.
func waitForAsk(t *testing.T, w *opencodeWatcher, clk *hubClock, id string) {
	t.Helper()
	refreshUntil(t, w, clk, nil, "the ask reaches the fold", func() bool {
		_, _, ok := w.approvalState(id)
		return ok
	})
}

// The pinned SUCCESS shapes, produced for real: 202 {"turn_id"}, 202
// {"interrupting":true}, 200 {"resolved","decision"} — exactly those keys, nothing else.
func TestHubVerbsOpencodeSuccessShapes(t *testing.T) {
	f := newFakeOpencode(t)
	f.setPinGuard(ocFixtureSID)
	f.streamAsk(ocFixtureSID, "per_1")
	srv, h, w, clk := newOpencodeVerbHub(t, f, ocFixtureSID)
	base := srv.URL + "/v1/sessions/" + verbSlugOpencode

	// turn → 202 with an opaque, non-empty handle (the value itself is deliberately
	// unspecified beyond being a string a client must not parse).
	resp := doRequest(t, http.MethodPost, base+"/turn", `{"text":"run the tests"}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("turn status = %d, want 202 (%s)", resp.StatusCode, readAll(t, resp))
	}
	var turn map[string]any
	if err := json.Unmarshal([]byte(readAll(t, resp)), &turn); err != nil {
		t.Fatalf("turn body: %v", err)
	}
	if len(turn) != 1 {
		t.Fatalf("turn body = %v, want exactly {turn_id}", turn)
	}
	if id, _ := turn["turn_id"].(string); id == "" {
		t.Fatalf("turn_id = %v, want a non-empty opaque handle", turn["turn_id"])
	}

	// interrupt → 202 {"interrupting":true}.
	wantJSON(t, doRequest(t, http.MethodPost, base+"/interrupt", ""),
		http.StatusAccepted, map[string]any{"interrupting": true})

	// approvals → 200 {"resolved":true,"decision":"allow"} once the ask is pending.
	waitForAsk(t, w, clk, "per_1")
	wantJSON(t, doRequest(t, http.MethodPost, base+"/approvals/per_1", `{"decision":"allow"}`),
		http.StatusOK, map[string]any{"resolved": true, "decision": approvalDecisionAllow})

	// The resolution is recorded synchronously on BOTH sides of the seam: the watcher's
	// fold, and the session's pending_approvals snapshot (no wait for the next tick).
	if status, decision, ok := w.approvalState("per_1"); !ok || status != approvalStatusResolved || decision != approvalDecisionAllow {
		t.Errorf("approvalState = (%q,%q,%v), want (resolved,allow,true)", status, decision, ok)
	}
	h.trackMu.Lock()
	pending := h.tracked[verbSlugOpencode].pendingApprovals
	h.trackMu.Unlock()
	for _, a := range pending {
		if a.ID == "per_1" {
			t.Errorf("pending_approvals still lists the resolved ask: %+v", pending)
		}
	}

	// Exactly the three pinned, session-scoped routes were touched (the fake's guard
	// covers the negative half from underneath).
	if got := f.postPaths(); len(got) != 3 {
		t.Errorf("upstream POSTs = %v, want one per verb", got)
	}
}

// The opencode rejection matrix — every branch the handler flow defines, over the mux.
func TestHubVerbsOpencodeRejectionMatrix(t *testing.T) {
	t.Run("unpinned session", func(t *testing.T) {
		f := newFakeOpencode(t) // empty store → the watcher never pins
		srv, _, _, _ := newOpencodeVerbHub(t, f, "")
		base := srv.URL + "/v1/sessions/" + verbSlugOpencode

		for _, c := range verbCases {
			t.Run(c.name, func(t *testing.T) {
				resp := doRequest(t, http.MethodPost, base+c.path, c.body)
				// approvals resolves state BEFORE it would address the session, so an
				// unpinned session answers 404 there (the id is unknown to the fold) and
				// 409 not_accepting for the two delivery verbs.
				want := errNotAccepting
				status := http.StatusConflict
				if c.name == "approvals" {
					want, status = errUnknownApproval, http.StatusNotFound
				}
				wantEnvelope(t, resp, status, want)
			})
		}
		if got := f.postPaths(); len(got) != 0 {
			t.Errorf("an unpinned session produced upstream POSTs %v, want none", got)
		}
	})

	t.Run("unknown approval id", func(t *testing.T) {
		f := newFakeOpencode(t)
		f.setPinGuard(ocFixtureSID)
		srv, _, _, _ := newOpencodeVerbHub(t, f, ocFixtureSID)
		wantEnvelope(t, doRequest(t, http.MethodPost,
			srv.URL+"/v1/sessions/"+verbSlugOpencode+"/approvals/per_nope", `{"decision":"allow"}`),
			http.StatusNotFound, errUnknownApproval)
		if got := f.postPaths(); len(got) != 0 {
			t.Errorf("an unknown id must not reach the agent, got %v", got)
		}
	})

	t.Run("replay", func(t *testing.T) {
		f := newFakeOpencode(t)
		f.setPinGuard(ocFixtureSID)
		f.streamAsk(ocFixtureSID, "per_1")
		srv, _, w, clk := newOpencodeVerbHub(t, f, ocFixtureSID)
		base := srv.URL + "/v1/sessions/" + verbSlugOpencode + "/approvals/per_1"
		waitForAsk(t, w, clk, "per_1")

		wantJSON(t, doRequest(t, http.MethodPost, base, `{"decision":"allow"}`),
			http.StatusOK, map[string]any{"resolved": true, "decision": approvalDecisionAllow})
		posts := len(f.postPaths())

		// SAME decision replayed → 200 idempotent, and — the point of the synchronous
		// bookkeeping — NO second upstream POST.
		wantJSON(t, doRequest(t, http.MethodPost, base, `{"decision":"allow"}`),
			http.StatusOK, map[string]any{"resolved": true, "decision": approvalDecisionAllow})
		if got := len(f.postPaths()); got != posts {
			t.Errorf("a same-decision replay POSTed again (%d → %d)", posts, got)
		}

		// A DIFFERENT decision on the same id → 409 already_resolved, still no POST.
		wantEnvelope(t, doRequest(t, http.MethodPost, base, `{"decision":"deny"}`),
			http.StatusConflict, errAlreadyResolved)
		if got := len(f.postPaths()); got != posts {
			t.Errorf("a conflicting replay POSTed anyway (%d → %d)", posts, got)
		}
	})

	t.Run("upstream failure", func(t *testing.T) {
		f := newFakeOpencode(t)
		f.setPinGuard(ocFixtureSID)
		f.promptStatus = http.StatusInternalServerError
		f.abortStatus = http.StatusInternalServerError
		f.permissionStatus = http.StatusInternalServerError
		f.streamAsk(ocFixtureSID, "per_1")
		srv, _, w, clk := newOpencodeVerbHub(t, f, ocFixtureSID)
		base := srv.URL + "/v1/sessions/" + verbSlugOpencode
		waitForAsk(t, w, clk, "per_1")

		// An upstream failure is a retryable 409 not_accepting on every verb — never a
		// 5xx, never a success.
		wantAllVerbs(t, base, http.StatusConflict, errNotAccepting)

		// A failed resolve leaves the ask OPEN (no local mark on an upstream failure), so
		// the operator can retry or answer in the TUI.
		if status, _, ok := w.approvalState("per_1"); !ok || status != approvalStatusPending {
			t.Errorf("approvalState after a failed resolve = (%q,%v), want it still pending", status, ok)
		}
	})

	t.Run("no watcher yet", func(t *testing.T) {
		// An opencode session whose watcher has not been built (no recorded port — a
		// pre-upgrade session) advertises the verbs by kind but cannot serve them: the
		// defensive no-lane 409, now genuinely reachable.
		tm := newHubTmux()
		clk := opencodeClock()
		h := newTestHub(tm, clk)
		tm.set("rc-vrb011", opencodeReadyPane(), managedEnv("id-vo2", KindOpencode))
		h.reconcile()
		srv := httptest.NewServer(h.handler())
		defer srv.Close()
		wantAllVerbs(t, srv.URL+"/v1/sessions/vrb011", http.StatusConflict, errNotAccepting)
	})
}

// Two concurrent approvals requests for the SAME id, over the real mux: the watcher's
// claim makes the read-then-POST atomic, so exactly ONE upstream POST is made. The loser
// is told the resolution is in flight (409 not_accepting, retryable) rather than
// double-answering the ask. Deterministic: the fake holds the winner's upstream call open
// while the loser runs.
func TestHubVerbsOpencodeConcurrentResolvePostsOnce(t *testing.T) {
	f := newFakeOpencode(t)
	f.setPinGuard(ocFixtureSID)
	f.streamAsk(ocFixtureSID, "per_1")
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	f.beforeMutation = func(string) {
		once.Do(func() { close(entered) })
		<-release
	}
	srv, _, w, clk := newOpencodeVerbHub(t, f, ocFixtureSID)
	base := srv.URL + "/v1/sessions/" + verbSlugOpencode + "/approvals/per_1"
	waitForAsk(t, w, clk, "per_1")

	first := make(chan *http.Response, 1)
	go func() { first <- doRequest(t, http.MethodPost, base, `{"decision":"allow"}`) }()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("the first resolve never reached the upstream POST")
	}

	// The racing request cannot claim the id, so it never reaches the network.
	second := doRequest(t, http.MethodPost, base, `{"decision":"allow"}`)
	body := readAll(t, second)
	second.Body.Close()
	if second.StatusCode != http.StatusConflict || !strings.Contains(body, errNotAccepting) ||
		!strings.Contains(body, "already in progress") {
		t.Fatalf("concurrent resolve = %d %s, want 409 not_accepting (in progress)", second.StatusCode, body)
	}

	close(release)
	wantJSON(t, <-first, http.StatusOK,
		map[string]any{"resolved": true, "decision": approvalDecisionAllow})
	if got := f.postPaths(); len(got) != 1 {
		t.Fatalf("upstream POSTs = %v, want exactly one for the two concurrent requests", got)
	}
	// And the settled ask answers idempotently afterwards, still without a second POST.
	wantJSON(t, doRequest(t, http.MethodPost, base, `{"decision":"allow"}`), http.StatusOK,
		map[string]any{"resolved": true, "decision": approvalDecisionAllow})
	if got := f.postPaths(); len(got) != 1 {
		t.Fatalf("upstream POSTs = %v after the replay, want still exactly one", got)
	}
}

// A lane failure's WIRE message must not carry hub-internal addressing: the upstream URL
// embeds the loopback port and the pinned agent session id, which belong in the hub log,
// not in a client's error envelope. The status code survives (it is what distinguishes
// "the agent said no" from "the agent never answered"), and the sentinels — which leak
// nothing — stay verbatim.
func TestHubVerbsOpencodeLaneErrorsDoNotLeakInternals(t *testing.T) {
	f := newFakeOpencode(t)
	f.setPinGuard(ocFixtureSID)
	f.promptStatus = http.StatusInternalServerError
	srv, _, _, _ := newOpencodeVerbHub(t, f, ocFixtureSID)

	resp := doRequest(t, http.MethodPost, srv.URL+"/v1/sessions/"+verbSlugOpencode+"/turn", `{"text":"hi"}`)
	body := readAll(t, resp)
	resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d, want 409 (%s)", resp.StatusCode, body)
	}
	if !strings.Contains(body, "upstream status 500") {
		t.Errorf("message = %s, want the upstream status summarized", body)
	}
	for _, secret := range []string{ocFixtureSID, "/session/", "127.0.0.1", strconv.Itoa(f.port(t))} {
		if strings.Contains(body, secret) {
			t.Errorf("message leaks hub-internal addressing (%q): %s", secret, body)
		}
	}

	// The unpinned sentinel is operator-facing and stays verbatim.
	f2 := newFakeOpencode(t)
	srv2, _, _, _ := newOpencodeVerbHub(t, f2, "")
	resp2 := doRequest(t, http.MethodPost, srv2.URL+"/v1/sessions/"+verbSlugOpencode+"/turn", `{"text":"hi"}`)
	body2 := readAll(t, resp2)
	resp2.Body.Close()
	if !strings.Contains(body2, "agent session not established yet") {
		t.Errorf("unpinned message = %s, want the remediation sentinel", body2)
	}
}

// The post-resolve republish must never write into an ORPHAN: reconcile can replace the
// tracked entry (a recreate) while the upstream POST is in flight, and the handler still
// holds the old pointer. It republishes only when the entry it copied is still THE tracked
// entry; otherwise the next tick republishes from the live watcher.
func TestRepublishApprovalsSkipsAReplacedEntry(t *testing.T) {
	f := newHubTmux()
	clk := &hubClock{t: time.Unix(1_700_000_000, 0).UTC()}
	h := newTestHub(f, clk)
	f.set("rc-rpb001", opencodeReadyPane(), managedEnv("id-rp", KindOpencode))
	h.reconcile()

	pub := &stubApprovalWatcher{stubWatcher: stubWatcher{activity: ActivityIdle}} // nothing open
	h.trackMu.Lock()
	tr := h.tracked["rpb001"]
	tr.pendingApprovals = []FeedApproval{{ID: "per_1", Status: approvalStatusPending}}
	h.trackMu.Unlock()

	// Still the tracked entry → the snapshot is refreshed from the lane.
	h.republishApprovals("rpb001", tr, pub)
	h.trackMu.Lock()
	got := h.tracked["rpb001"].pendingApprovals
	h.trackMu.Unlock()
	if len(got) != 0 {
		t.Fatalf("pendingApprovals = %+v, want the lane's (empty) snapshot", got)
	}

	// A recreate replaces the entry: the orphan the handler holds must be left alone, and
	// the LIVE entry must not be rewritten from a dead incarnation's watcher either.
	orphan := tr
	orphan.pendingApprovals = []FeedApproval{{ID: "per_orphan", Status: approvalStatusPending}}
	live := h.newTrackedSession(Session{Slug: "rpb001", TmuxSession: "rc-rpb001", Kind: KindOpencode})
	live.pendingApprovals = []FeedApproval{{ID: "per_live", Status: approvalStatusPending}}
	h.trackMu.Lock()
	h.tracked["rpb001"] = live
	h.trackMu.Unlock()

	h.republishApprovals("rpb001", orphan, pub)
	if len(orphan.pendingApprovals) != 1 || orphan.pendingApprovals[0].ID != "per_orphan" {
		t.Errorf("orphan entry was rewritten: %+v", orphan.pendingApprovals)
	}
	h.trackMu.Lock()
	got = h.tracked["rpb001"].pendingApprovals
	h.trackMu.Unlock()
	if len(got) != 1 || got[0].ID != "per_live" {
		t.Errorf("live entry = %+v, want the current incarnation's own snapshot", got)
	}
}
