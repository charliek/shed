package rc

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// The opencode session id + workdir the sanitized fixture (testdata/jsonl/opencode_turn.jsonl)
// was captured under. The transport pins on a session.created whose directory matches the
// workdir; canonicalDir falls back to a lexical Clean for a non-existent path, so both sides
// compare equal without the directory needing to exist on the test host.
const (
	ocFixtureSID = "ses_07cbd4370ffeF17Wb3Ius82a2g"
	ocFixtureDir = "/private/tmp/oc-cap-kAr0"

	// ocOtherSID is a SECOND session in the same fake's store — the global-store hazard
	// made concrete: one embedded server lists (and, on a global route, would answer for)
	// sessions belonging to other directories/processes. Every WS-B test that needs a
	// bystander uses this id.
	ocOtherSID = "ses_07cbd4370ffeOTHERsession9zz"
	ocOtherDir = "/private/tmp/oc-cap-other"
)

// ---- fake opencode server (httptest double: /event SSE + REST seed endpoints) ----

// fakeOpencode is a programmable stand-in for opencode's embedded HTTP+SSE server. onEvent
// drives one /event connection (called once per connect, with the 1-based connection index);
// the REST bodies are plain JSON strings the seed reads. All fields are safe to set before
// starting the watcher; counters are atomic so a test goroutine can observe progress.
type fakeOpencode struct {
	ts *httptest.Server
	t  *testing.T // for the scoping-invariant guard, which FAILS the test from the handler

	eventConns   atomic.Int64 // number of /event connections opened
	sessionHits  atomic.Int64 // GET /session calls (candidate lookup)
	messagesHits atomic.Int64 // GET /session/{id}/message calls (seed reads)

	onEvent func(conn int64, w io.Writer, flush func(), ctx context.Context)

	mu             sync.Mutex
	sessionBody    string                                // GET /session
	messagesBody   string                                // GET /session/{id}/message
	statusBody     string                                // GET /session/status
	permissionBody string                                // GET /permission
	questionBody   string                                // GET /question
	eventStatus    int                                   // non-zero → /event replies this status and returns (e.g. 401)
	messagesStatus int                                   // non-zero → /message replies this status (e.g. 500)
	statusStatus   int                                   // non-zero → /session/status replies this status (e.g. 500)
	questionStatus int                                   // non-zero → /question replies this status (e.g. 500)
	beforeMessages func(call int64, ctx context.Context) // optional gate at the start of /message (call is 1-based)

	// ---- verb lane (mutations) ----
	promptStatus     int // non-zero → POST /session/{id}/prompt_async replies this status (default 204)
	abortStatus      int // non-zero → POST /session/{id}/abort replies this status (default 200)
	permissionStatus int // non-zero → POST /session/{id}/permissions/{id} replies this (default 200)

	// beforeMutation, when set, runs at the start of every POST (with its path) — a gate a
	// test uses to hold one verb's upstream call in flight while it drives another.
	beforeMutation func(path string)

	// pinGuard, when set, is the ONLY opencode session id a MUTATION may address: a POST
	// to any other session's scoped route — or to a global route at all — fails the test
	// (see the WS-B invariant guard below).
	pinGuard string
	requests []ocRequest // every request, in order (method + path + body)
}

// ocRequest is one recorded request against the fake.
type ocRequest struct {
	method string
	path   string
	body   string
}

func newFakeOpencode(t *testing.T) *fakeOpencode {
	t.Helper()
	f := &fakeOpencode{
		t:              t,
		sessionBody:    "[]",
		messagesBody:   "[]",
		statusBody:     "{}",
		permissionBody: "[]",
		questionBody:   "[]",
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/event", func(w http.ResponseWriter, r *http.Request) {
		conn := f.eventConns.Add(1)
		f.mu.Lock()
		st := f.eventStatus
		f.mu.Unlock()
		if st != 0 {
			w.WriteHeader(st)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fl, _ := w.(http.Flusher)
		flush := func() {
			if fl != nil {
				fl.Flush()
			}
		}
		if f.onEvent != nil {
			f.onEvent(conn, w, flush, r.Context())
		}
	})
	mux.HandleFunc("/session/status", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		body := f.statusBody
		st := f.statusStatus
		f.mu.Unlock()
		if st != 0 {
			w.WriteHeader(st)
			return
		}
		_, _ = io.WriteString(w, body)
	})
	mux.HandleFunc("/session", func(w http.ResponseWriter, r *http.Request) {
		// /session (exact) is the candidate list.
		f.sessionHits.Add(1)
		f.mu.Lock()
		body := f.sessionBody
		f.mu.Unlock()
		_, _ = io.WriteString(w, body)
	})
	mux.HandleFunc("/session/", func(w http.ResponseWriter, r *http.Request) {
		// Subtree: the three session-scoped MUTATIONS (POST), plus /session/{id}/message
		// history (a longer /session/status match wins its exact path). Anything else
		// under /session/ is unknown → 404.
		if r.Method == http.MethodPost {
			f.serveMutation(w, r)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/message") {
			call := f.messagesHits.Add(1)
			f.mu.Lock()
			body := f.messagesBody
			st := f.messagesStatus
			gate := f.beforeMessages
			f.mu.Unlock()
			if gate != nil {
				gate(call, r.Context())
			}
			if st != 0 {
				w.WriteHeader(st)
				return
			}
			_, _ = io.WriteString(w, body)
			return
		}
		http.NotFound(w, r)
	})
	mux.HandleFunc("/permission", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		body := f.permissionBody
		f.mu.Unlock()
		_, _ = io.WriteString(w, body)
	})
	mux.HandleFunc("/question", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		body := f.questionBody
		st := f.questionStatus
		f.mu.Unlock()
		if st != 0 {
			w.WriteHeader(st)
			return
		}
		_, _ = io.WriteString(w, body)
	})
	f.ts = httptest.NewServer(f.recordAndGuard(mux))
	t.Cleanup(f.ts.Close)
	return f
}

// ocScopedMutationRe is the ONLY shape a hub-initiated mutation may take:
// POST /session/{id}/prompt_async | /abort | /permissions/{permID}. Anything else a POST
// could reach — the global /permission/{id}/reply and /question/{id}/reply|reject write
// routes above all — is an invariant violation by construction (WS-B, plan §3.4).
var ocScopedMutationRe = regexp.MustCompile(`^/session/([^/]+)/(prompt_async|abort|permissions/[^/]+)$`)

// mutationViolation returns why a POST to path breaks the session-scoping invariant
// ("" when it does not). pin, when non-empty, is the session id the caller is pinned to:
// a scoped route addressing any OTHER session is just as much a violation as a global
// one — it steers somebody else's TUI.
func mutationViolation(path, pin string) string {
	m := ocScopedMutationRe.FindStringSubmatch(path)
	if m == nil {
		return "not a session-scoped mutation route"
	}
	if pin != "" && m[1] != pin {
		return "addressed session " + m[1] + ", not the pinned " + pin
	}
	return ""
}

// recordAndGuard wraps the fake's mux: it records every request (so a test can assert
// exactly which routes a verb touched) and ENFORCES the mutation-scoping invariant —
// a violating POST fails the test immediately and is answered 500, so the offending verb
// can never look successful. Reads are deliberately unguarded: global GETs
// (/session, /session/status, /permission, /question) are legal for seed/discovery and
// are pin-filtered client-side.
func (f *fakeOpencode) recordAndGuard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		_ = r.Body.Close()
		r.Body = io.NopCloser(bytes.NewReader(body))

		f.mu.Lock()
		f.requests = append(f.requests, ocRequest{method: r.Method, path: r.URL.Path, body: string(body)})
		pin := f.pinGuard
		f.mu.Unlock()

		if r.Method == http.MethodPost {
			if v := mutationViolation(r.URL.Path, pin); v != "" {
				// Errorf (not Fatalf): this runs on the server's goroutine.
				f.t.Errorf("session-scoping invariant violated — %s: POST %s", v, r.URL.Path)
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// serveMutation answers the three session-scoped verb routes the way live opencode does
// (prompt_async → 204 empty, abort → 200 `true`, permissions/{id} → 200 `true`), with a
// per-route status override so a test can drive the upstream-failure branch.
func (f *fakeOpencode) serveMutation(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	prompt, abort, perm := f.promptStatus, f.abortStatus, f.permissionStatus
	gate := f.beforeMutation
	f.mu.Unlock()
	if gate != nil {
		gate(r.URL.Path)
	}

	status := http.StatusNotFound
	switch {
	case strings.HasSuffix(r.URL.Path, "/prompt_async"):
		status = cmp.Or(prompt, http.StatusNoContent)
	case strings.HasSuffix(r.URL.Path, "/abort"):
		status = cmp.Or(abort, http.StatusOK)
	case strings.Contains(r.URL.Path, "/permissions/"):
		status = cmp.Or(perm, http.StatusOK)
	}
	w.WriteHeader(status)
	if status == http.StatusOK {
		_, _ = io.WriteString(w, "true")
	}
}

// paths returns the recorded paths for one method, in order.
func (f *fakeOpencode) paths(method string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, req := range f.requests {
		if req.method == method {
			out = append(out, req.path)
		}
	}
	return out
}

// postPaths returns the recorded POST paths (in order).
func (f *fakeOpencode) postPaths() []string { return f.paths(http.MethodPost) }

// postBody returns the body of the single recorded POST whose path ends in suffix (or ""
// when there is none), so a test can assert the exact wire body a verb sent.
func (f *fakeOpencode) postBody(suffix string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, req := range f.requests {
		if req.method == http.MethodPost && strings.HasSuffix(req.path, suffix) {
			return req.body
		}
	}
	return ""
}

// getPaths returns every recorded GET path (in order) — the read side of the invariant:
// session-scoped GETs must address the pin, global ones are legal and filtered.
func (f *fakeOpencode) getPaths() []string { return f.paths(http.MethodGet) }

// holdOpenSSE installs the default /event stream — announce server.connected, then hold
// the connection open until the watcher closes it — unless the test already installed one.
// The shared default for every test that needs a live-but-quiet transport.
func (f *fakeOpencode) holdOpenSSE() {
	if f.onEvent != nil {
		return
	}
	f.onEvent = func(conn int64, w io.Writer, flush func(), ctx context.Context) {
		writeSSE(w, flush, sseServerConnected)
		<-ctx.Done()
	}
}

// streamAsk installs an /event stream that announces server.connected followed by ONE
// permission.asked for sid, then holds open — the "session with one pending ask" fixture
// the approvals verb needs, in a single home rather than re-spelled per test.
func (f *fakeOpencode) streamAsk(sid, id string) {
	ask := fmt.Sprintf(
		`{"type":"permission.asked","properties":{"id":%q,"sessionID":%q,"permission":"bash","patterns":["ls"],"metadata":{"command":"ls -la"}}}`,
		id, sid)
	f.onEvent = func(conn int64, w io.Writer, flush func(), ctx context.Context) {
		writeSSE(w, flush, sseServerConnected)
		writeSSE(w, flush, ask)
		<-ctx.Done()
	}
}

func (f *fakeOpencode) setPinGuard(id string) {
	f.mu.Lock()
	f.pinGuard = id
	f.mu.Unlock()
}

func (f *fakeOpencode) port(t *testing.T) int {
	t.Helper()
	u, err := url.Parse(f.ts.URL)
	if err != nil {
		t.Fatalf("parse fake url: %v", err)
	}
	_, portStr, err := net.SplitHostPort(u.Host)
	if err != nil {
		t.Fatalf("split host port: %v", err)
	}
	p, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatalf("atoi port: %v", err)
	}
	return p
}

func (f *fakeOpencode) setEventStatus(code int) {
	f.mu.Lock()
	f.eventStatus = code
	f.mu.Unlock()
}

func (f *fakeOpencode) setMessagesStatus(code int) {
	f.mu.Lock()
	f.messagesStatus = code
	f.mu.Unlock()
}

func (f *fakeOpencode) setStatusStatus(code int) {
	f.mu.Lock()
	f.statusStatus = code
	f.mu.Unlock()
}

func (f *fakeOpencode) setBeforeMessages(fn func(call int64, ctx context.Context)) {
	f.mu.Lock()
	f.beforeMessages = fn
	f.mu.Unlock()
}

// writeSSE writes one opencode SSE frame (event name is always "message") and flushes.
func writeSSE(w io.Writer, flush func(), jsonPayload string) {
	_, _ = fmt.Fprintf(w, "event: message\ndata: %s\n\n", jsonPayload)
	flush()
}

const sseServerConnected = `{"type":"server.connected","properties":{}}`

// ocRESTMessages is a GET /session/{id}/message body (the sanitized live capture): the same
// turn as the SSE fixture in REST {info,parts}[] shape. The transport synthesizes
// message.updated + message.part.updated envelopes from it, which fold to the same 5 rows —
// so a reconnect reseed against this body dedups against the SSE arc (partID/callID keys).
const ocRESTMessages = `[{"info":{"id":"msg_f8342bca6001AF4ZtucXXAMSBG","sessionID":"ses_07cbd4370ffeF17Wb3Ius82a2g","role":"user","time":{"created":1784613616806},"summary":{"diffs":[]},"agent":"build","model":{"providerID":"zai-coding-plan","modelID":"glm-5.2"}},"parts":[{"id":"prt_f8342bcab001S05uV1SU0MDVyM","sessionID":"ses_07cbd4370ffeF17Wb3Ius82a2g","messageID":"msg_f8342bca6001AF4ZtucXXAMSBG","type":"text","text":"Use the bash tool to run ls in the current directory, then tell me how many .txt files there are. Be brief."}]},{"info":{"id":"msg_f8342bd01001y7c4BWOcifP2Va","sessionID":"ses_07cbd4370ffeF17Wb3Ius82a2g","role":"assistant","time":{"created":1784613616897,"completed":1784613621217},"parentID":"msg_f8342bca6001AF4ZtucXXAMSBG","modelID":"glm-5.2","providerID":"zai-coding-plan","mode":"build","agent":"build","path":{"cwd":"/private/tmp/oc-cap-kAr0","root":"/"},"cost":0,"tokens":{"total":7510,"input":7413,"output":11,"reasoning":22,"cache":{"read":64,"write":0}},"finish":"tool-calls"},"parts":[{"id":"prt_f8342cd9f00143bbhEOq5FsLv0","sessionID":"ses_07cbd4370ffeF17Wb3Ius82a2g","messageID":"msg_f8342bd01001y7c4BWOcifP2Va","type":"step-start"},{"id":"prt_f8342cda30012K99PZ0UzpgPGT","sessionID":"ses_07cbd4370ffeF17Wb3Ius82a2g","messageID":"msg_f8342bd01001y7c4BWOcifP2Va","type":"reasoning","text":"The user wants me to run ls in the current directory and tell them how many .txt files there are.","time":{"start":1784613621155,"end":1784613621161}},{"id":"prt_f8342cdab001rx5DsXIbsWW79j","sessionID":"ses_07cbd4370ffeF17Wb3Ius82a2g","messageID":"msg_f8342bd01001y7c4BWOcifP2Va","type":"tool","callID":"call_4c5b28f16dae4e6183bc6cf1","tool":"bash","state":{"status":"completed","input":{"command":"ls"},"output":"a.txt\nb.txt\nc.txt\n","title":"ls","metadata":{"output":"a.txt\nb.txt\nc.txt\n","exit":0,"truncated":false},"time":{"start":1784613621207,"end":1784613621211}}},{"id":"prt_f8342cdde001wVPhUpApFH7zvn","sessionID":"ses_07cbd4370ffeF17Wb3Ius82a2g","messageID":"msg_f8342bd01001y7c4BWOcifP2Va","type":"step-finish","reason":"tool-calls","cost":0,"tokens":{"total":7510,"input":7413,"output":11,"reasoning":22,"cache":{"read":64,"write":0}}}]},{"info":{"id":"msg_f8342cde3001cV5WzoZm92qWBg","sessionID":"ses_07cbd4370ffeF17Wb3Ius82a2g","role":"assistant","time":{"created":1784613621219,"completed":1784613627684},"parentID":"msg_f8342bca6001AF4ZtucXXAMSBG","modelID":"glm-5.2","providerID":"zai-coding-plan","mode":"build","agent":"build","path":{"cwd":"/private/tmp/oc-cap-kAr0","root":"/"},"cost":0,"tokens":{"total":7530,"input":99,"output":7,"reasoning":0,"cache":{"read":7424,"write":0}},"finish":"stop"},"parts":[{"id":"prt_f8342e71a001L2RZmCk3eMll71","sessionID":"ses_07cbd4370ffeF17Wb3Ius82a2g","messageID":"msg_f8342cde3001cV5WzoZm92qWBg","type":"step-start"},{"id":"prt_f8342e71f001dudzMRadO96Nju","sessionID":"ses_07cbd4370ffeF17Wb3Ius82a2g","messageID":"msg_f8342cde3001cV5WzoZm92qWBg","type":"text","text":"3 .txt files.","time":{"start":1784613627679,"end":1784613627681}},{"id":"prt_f8342e722001MPkFiT810m8dLb","sessionID":"ses_07cbd4370ffeF17Wb3Ius82a2g","messageID":"msg_f8342cde3001cV5WzoZm92qWBg","type":"step-finish","reason":"stop","cost":0,"tokens":{"total":7530,"input":99,"output":7,"reasoning":0,"cache":{"read":7424,"write":0}}}]}]`

// ---- test-side sync helpers (deterministic polling, no fixed-duration sleep-then-assert) ----

// pollUntil polls cond every 5ms up to ~2s, failing the test on timeout. Used to wait on the
// fake's connection counters (no watcher refresh, so it observes pure transport progress).
func pollUntil(t *testing.T, msg string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for: %s", msg)
}

// refreshUntil drives the watcher (refresh on the fake clock) every 5ms up to ~2s until cond
// holds, accumulating drained feed rows into *rows. This is the "call refresh+snapshot in a
// bounded loop" sync point the plan mandates.
func refreshUntil(t *testing.T, w *opencodeWatcher, clk *hubClock, rows *[]feedMessage, msg string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		w.refresh(clk.now())
		if rows != nil {
			*rows = append(*rows, w.drainPending()...)
		}
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for: %s", msg)
}

func opencodeClock() *hubClock {
	return &hubClock{t: time.Unix(1_700_000_000, 0).UTC()}
}

// fixtureFrames returns the sanitized turn fixture as SSE payload strings (one per line).
func fixtureFrames(t *testing.T) []string {
	t.Helper()
	var out []string
	for _, ln := range readJSONL(t, "testdata/jsonl/opencode_turn.jsonl") {
		out = append(out, string(ln))
	}
	return out
}

// ---- tests ----

// Pin from a port-local session.created (root, dir match), then seed → live: the fixture arc
// drives activity working → needs_input, drainPending yields the normalized feed rows, and the
// discovered id is surfaced exactly once via drainConfirmedAgentID.
func TestOpencodeWatcherPinFromSessionCreated(t *testing.T) {
	f := newFakeOpencode(t)
	// A fresh session: the candidate list is empty; status reports busy during the turn.
	f.statusBody = fmt.Sprintf(`{%q:{"type":"busy"}}`, ocFixtureSID)
	frames := fixtureFrames(t)
	f.onEvent = func(conn int64, w io.Writer, flush func(), ctx context.Context) {
		writeSSE(w, flush, sseServerConnected)
		for _, fr := range frames {
			writeSSE(w, flush, fr)
		}
		<-ctx.Done() // hold the connection open (no reconnect churn)
	}

	clk := opencodeClock()
	w := newOpencodeWatcher(f.port(t), ocFixtureDir, "", time.Time{}, clk.now, nil)
	t.Cleanup(w.close)

	var rows []feedMessage
	refreshUntil(t, w, clk, &rows, "activity settles to needs_input", func() bool {
		act, _, _, _ := w.snapshot(clk.now())
		return act == ActivityNeedsInput
	})

	act, msg, fresh, expWorking := w.snapshot(clk.now())
	if act != ActivityNeedsInput {
		t.Fatalf("final activity = %q, want needs_input", act)
	}
	if !fresh || expWorking {
		t.Fatalf("settled+healthy snapshot = fresh:%v expiredWorking:%v, want fresh:true expiredWorking:false", fresh, expWorking)
	}
	if msg != "3 .txt files." {
		t.Fatalf("last_message = %q, want %q", msg, "3 .txt files.")
	}

	// The feed is the normalized turn (order preserved through seed→live).
	want := []opencodeFeedRow{
		{role: feedRoleUser, typ: feedTypeText, textPrefix: "Use the bash tool"},
		{role: feedRoleAssistant, typ: feedTypeReasoning, textPrefix: "The user wants"},
		{role: feedRoleTool, typ: feedTypeToolUse, toolName: "bash", detailHas: "ls"},
		{role: feedRoleTool, typ: feedTypeToolResult, toolName: "bash", detailHas: "a.txt"},
		{role: feedRoleAssistant, typ: feedTypeText, textPrefix: "3 .txt files."},
	}
	assertOpencodeRows(t, rows, want)

	// The discovered id is back-write material exactly once.
	if got := w.drainConfirmedAgentID(); got != ocFixtureSID {
		t.Fatalf("drainConfirmedAgentID = %q, want %q", got, ocFixtureSID)
	}
	if got := w.drainConfirmedAgentID(); got != "" {
		t.Fatalf("second drainConfirmedAgentID = %q, want \"\" (drained once)", got)
	}
}

// A prior back-written agentID is the trusted pin: the watcher seeds purely from REST (no wait
// for session.created), reconstructs the feed, and does NOT re-enqueue the (already-stamped) id.
func TestOpencodeWatcherPinViaPriorAgentID(t *testing.T) {
	f := newFakeOpencode(t)
	f.messagesBody = ocRESTMessages
	f.statusBody = "{}" // idle-omitted → seed synthesizes session.idle → needs_input
	f.onEvent = func(conn int64, w io.Writer, flush func(), ctx context.Context) {
		writeSSE(w, flush, sseServerConnected)
		<-ctx.Done()
	}

	clk := opencodeClock()
	w := newOpencodeWatcher(f.port(t), ocFixtureDir, ocFixtureSID, time.Time{}, clk.now, nil)
	t.Cleanup(w.close)

	var rows []feedMessage
	refreshUntil(t, w, clk, &rows, "REST seed reconstructs needs_input", func() bool {
		act, _, _, _ := w.snapshot(clk.now())
		return act == ActivityNeedsInput
	})

	if len(rows) != 5 {
		t.Fatalf("seeded feed rows = %d, want 5:\n%s", len(rows), formatOpencodeRows(rows))
	}
	if got := w.drainConfirmedAgentID(); got != "" {
		t.Fatalf("drainConfirmedAgentID = %q, want \"\" (prior back-write is not re-confirmed)", got)
	}
}

// A GET /session candidate is FOLLOW-ONLY: the feed may be seeded from it, but the id is NOT
// back-written until a live SSE event on our own stream confirms ownership (§3.3).
func TestOpencodeWatcherRESTCandidateFollowOnly(t *testing.T) {
	f := newFakeOpencode(t)
	f.sessionBody = fmt.Sprintf(`[{"id":%q,"directory":%q,"parentID":""}]`, ocFixtureSID, ocFixtureDir)
	f.messagesBody = ocRESTMessages
	f.statusBody = fmt.Sprintf(`{%q:{"type":"busy"}}`, ocFixtureSID)

	release := make(chan struct{})
	f.onEvent = func(conn int64, w io.Writer, flush func(), ctx context.Context) {
		writeSSE(w, flush, sseServerConnected)
		<-release // hold before emitting the confirming SSE event
		writeSSE(w, flush, fmt.Sprintf(`{"type":"session.status","properties":{"sessionID":%q,"status":{"type":"busy"}}}`, ocFixtureSID))
		<-ctx.Done()
	}

	clk := opencodeClock()
	w := newOpencodeWatcher(f.port(t), ocFixtureDir, "", time.Time{}, clk.now, nil)
	t.Cleanup(w.close)

	// The candidate is established from GET /session, but NOT confirmed: no back-write yet.
	pollUntil(t, "candidate established via GET /session", func() bool {
		return f.sessionHits.Load() >= 1
	})
	// Give the follow-only seed a moment to flow; the id must still be undrained.
	for i := 0; i < 20; i++ {
		w.refresh(clk.now())
		if got := w.drainConfirmedAgentID(); got != "" {
			t.Fatalf("id back-written before SSE confirmation: %q", got)
		}
		time.Sleep(5 * time.Millisecond)
	}

	// Release the confirming SSE event → the candidate is now confirmed.
	close(release)
	pollUntil(t, "candidate confirmed by SSE evidence", func() bool {
		return w.drainConfirmedAgentID() == ocFixtureSID
	})
}

// Events for a different sessionID (sibling) or with a non-empty parentID (child) are filtered
// out — they never reach the fold, so they do not appear in the feed or move activity.
func TestOpencodeWatcherFiltersChildSibling(t *testing.T) {
	f := newFakeOpencode(t)
	f.statusBody = fmt.Sprintf(`{%q:{"type":"busy"}}`, ocFixtureSID)
	f.onEvent = func(conn int64, w io.Writer, flush func(), ctx context.Context) {
		writeSSE(w, flush, sseServerConnected)
		// Pin the root session.
		writeSSE(w, flush, fmt.Sprintf(`{"type":"session.created","properties":{"sessionID":%q,"info":{"id":%q,"directory":%q,"parentID":""}}}`, ocFixtureSID, ocFixtureSID, ocFixtureDir))
		// A SIBLING session's user text (different sessionID) — must be dropped.
		writeSSE(w, flush, `{"type":"message.updated","properties":{"sessionID":"ses_SIBLING","info":{"id":"m_sib","role":"user","time":{"created":1784613616806}}}}`)
		writeSSE(w, flush, `{"type":"message.part.updated","properties":{"sessionID":"ses_SIBLING","part":{"id":"p_sib","messageID":"m_sib","type":"text","text":"sibling prompt"}}}`)
		// A CHILD session event (non-empty parentID + its own id) — must be dropped.
		writeSSE(w, flush, `{"type":"message.part.updated","properties":{"sessionID":"ses_CHILD","part":{"id":"p_child","messageID":"m_child","type":"text","text":"child text"}}}`)
		// Our own session's user text — must be kept.
		writeSSE(w, flush, fmt.Sprintf(`{"type":"message.updated","properties":{"sessionID":%q,"info":{"id":"m_root","role":"user","time":{"created":1784613616806}}}}`, ocFixtureSID))
		writeSSE(w, flush, fmt.Sprintf(`{"type":"message.part.updated","properties":{"sessionID":%q,"part":{"id":"p_root","messageID":"m_root","type":"text","text":"root prompt"}}}`, ocFixtureSID))
		writeSSE(w, flush, fmt.Sprintf(`{"type":"session.idle","properties":{"sessionID":%q}}`, ocFixtureSID))
		<-ctx.Done()
	}

	clk := opencodeClock()
	w := newOpencodeWatcher(f.port(t), ocFixtureDir, "", time.Time{}, clk.now, nil)
	t.Cleanup(w.close)

	var rows []feedMessage
	refreshUntil(t, w, clk, &rows, "root session settles", func() bool {
		act, _, _, _ := w.snapshot(clk.now())
		return act == ActivityNeedsInput
	})

	if len(rows) != 1 {
		t.Fatalf("feed rows = %d, want 1 (only the root session's prompt):\n%s", len(rows), formatOpencodeRows(rows))
	}
	if rows[0].Role != feedRoleUser || !strings.HasPrefix(rows[0].Text, "root prompt") {
		t.Fatalf("row = (%s,%q), want the root prompt", rows[0].Role, rows[0].Text)
	}
}

// Seed-before-subscribe: a live event emitted while the goroutine is busy with the REST seed is
// buffered by the kernel socket and folded after the seed — it is NOT lost.
func TestOpencodeWatcherSeedBeforeSubscribe(t *testing.T) {
	f := newFakeOpencode(t)
	f.messagesBody = ocRESTMessages // 5 rows of history
	f.statusBody = fmt.Sprintf(`{%q:{"type":"busy"}}`, ocFixtureSID)
	f.onEvent = func(conn int64, w io.Writer, flush func(), ctx context.Context) {
		writeSSE(w, flush, sseServerConnected)
		// This idle event is written immediately after server.connected — i.e. during the
		// window the goroutine spends doing the REST seed. It must still be folded.
		writeSSE(w, flush, fmt.Sprintf(`{"type":"session.idle","properties":{"sessionID":%q}}`, ocFixtureSID))
		<-ctx.Done()
	}

	clk := opencodeClock()
	w := newOpencodeWatcher(f.port(t), ocFixtureDir, ocFixtureSID, time.Time{}, clk.now, nil)
	t.Cleanup(w.close)

	var rows []feedMessage
	refreshUntil(t, w, clk, &rows, "the during-seed idle event folds", func() bool {
		act, _, _, _ := w.snapshot(clk.now())
		return act == ActivityNeedsInput
	})
	if len(rows) != 5 {
		t.Fatalf("feed rows = %d, want 5 (seed history intact):\n%s", len(rows), formatOpencodeRows(rows))
	}
}

// Idle reseed: a successful /session/status that LACKS the pinned id → synthesized session.idle
// → needs_input (reconstructs a settled verdict after a hub restart while idle).
func TestOpencodeWatcherIdleReseed(t *testing.T) {
	f := newFakeOpencode(t)
	f.messagesBody = ocRESTMessages
	f.statusBody = "{}" // idle-omitted
	f.onEvent = func(conn int64, w io.Writer, flush func(), ctx context.Context) {
		writeSSE(w, flush, sseServerConnected)
		<-ctx.Done()
	}

	clk := opencodeClock()
	w := newOpencodeWatcher(f.port(t), ocFixtureDir, ocFixtureSID, time.Time{}, clk.now, nil)
	t.Cleanup(w.close)

	refreshUntil(t, w, clk, nil, "synthesized idle → needs_input", func() bool {
		act, _, _, _ := w.snapshot(clk.now())
		return act == ActivityNeedsInput
	})
}

// Reconnect reseed idempotency: drop the SSE stream, reconnect, replay the same history — the
// partID/callID dedup (surviving the reconnect) yields no duplicate feed rows.
func TestOpencodeWatcherReconnectReseedIdempotent(t *testing.T) {
	f := newFakeOpencode(t)
	f.statusBody = fmt.Sprintf(`{%q:{"type":"busy"}}`, ocFixtureSID)
	// On reconnect the history is served via REST too, so the reseed must dedup against it.
	f.messagesBody = ocRESTMessages
	frames := fixtureFrames(t)
	f.onEvent = func(conn int64, w io.Writer, flush func(), ctx context.Context) {
		writeSSE(w, flush, sseServerConnected)
		for _, fr := range frames {
			writeSSE(w, flush, fr)
		}
		if conn == 1 {
			return // first connection ends → watcher reconnects
		}
		<-ctx.Done() // second connection stays open
	}

	clk := opencodeClock()
	w := newOpencodeWatcher(f.port(t), ocFixtureDir, "", time.Time{}, clk.now, nil)
	t.Cleanup(w.close)

	var rows []feedMessage
	refreshUntil(t, w, clk, &rows, "first connection yields the 5-row feed", func() bool {
		return len(rows) >= 5
	})
	if len(rows) != 5 {
		t.Fatalf("after first stream: %d rows, want 5", len(rows))
	}
	// Wait for the reconnect (second /event connection) and let its reseed flow.
	pollUntil(t, "watcher reconnects", func() bool { return f.eventConns.Load() >= 2 })
	for i := 0; i < 40; i++ {
		w.refresh(clk.now())
		rows = append(rows, w.drainPending()...)
		time.Sleep(5 * time.Millisecond)
	}
	if len(rows) != 5 {
		t.Fatalf("after reconnect+reseed: %d rows, want 5 (dedup idempotent):\n%s", len(rows), formatOpencodeRows(rows))
	}
}

// A disconnected/heartbeat-stale watcher must NOT report a fresh verdict: snapshot returns BOTH
// fresh=false AND expiredWorking=false so pane-stability drives (the network≠file rule, §3.6).
func TestOpencodeWatcherStaleFallsToStability(t *testing.T) {
	f := newFakeOpencode(t)
	f.statusBody = fmt.Sprintf(`{%q:{"type":"busy"}}`, ocFixtureSID) // seed → working
	f.onEvent = func(conn int64, w io.Writer, flush func(), ctx context.Context) {
		writeSSE(w, flush, sseServerConnected)
		<-ctx.Done() // no further frames: lastFrameAt freezes at connect time
	}

	clk := opencodeClock()
	w := newOpencodeWatcher(f.port(t), ocFixtureDir, ocFixtureSID, time.Time{}, clk.now, nil)
	t.Cleanup(w.close)

	refreshUntil(t, w, clk, nil, "seed → working, healthy", func() bool {
		act, _, fresh, _ := w.snapshot(clk.now())
		return act == ActivityWorking && fresh
	})

	// Advance past the heartbeat-stale window with no new frame → verdict is no longer trusted.
	clk.advance(ocFrameStaleWindow + time.Second)
	w.refresh(clk.now())
	act, _, fresh, expWorking := w.snapshot(clk.now())
	if act != ActivityWorking {
		t.Fatalf("activity = %q, want working (verdict retained, just untrusted)", act)
	}
	if fresh || expWorking {
		t.Fatalf("heartbeat-stale snapshot = fresh:%v expiredWorking:%v, want both false", fresh, expWorking)
	}
	// mergedActivity must let stability drive (both flags false → stability branch).
	merged, _ := mergedActivity(act, "", fresh, expWorking, ActivityIdle)
	if merged != ActivityIdle {
		t.Fatalf("mergedActivity = %q, want idle (stability drives a stale watcher)", merged)
	}
}

// close() during a blocked SSE read returns promptly (non-blocking) and the goroutine exits —
// no leak. The done channel is closed by run() on exit.
func TestOpencodeWatcherCloseDuringBlockedRead(t *testing.T) {
	f := newFakeOpencode(t)
	f.onEvent = func(conn int64, w io.Writer, flush func(), ctx context.Context) {
		writeSSE(w, flush, sseServerConnected)
		<-ctx.Done() // block: the watcher goroutine sits in the SSE read
	}

	clk := opencodeClock()
	w := newOpencodeWatcher(f.port(t), ocFixtureDir, ocFixtureSID, time.Time{}, clk.now, nil)
	pollUntil(t, "SSE connection established", func() bool { return f.eventConns.Load() >= 1 })

	done := make(chan struct{})
	go func() { w.close(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("close() blocked (must be non-blocking)")
	}
	select {
	case <-w.done:
	case <-time.After(2 * time.Second):
		t.Fatal("goroutine leaked (run did not exit after close)")
	}
	// close is idempotent.
	w.close()
}

// An unreachable port never connects: the watcher stays not-fresh, does not panic, and closes
// cleanly (the goroutine exits despite being mid-backoff).
func TestOpencodeWatcherUnreachablePort(t *testing.T) {
	// A closed loopback listener yields a port nothing is listening on.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()

	clk := opencodeClock()
	w := newOpencodeWatcher(port, ocFixtureDir, ocFixtureSID, time.Time{}, clk.now, nil)

	// Refresh a few times; the verdict must stay unknown/not-fresh (nothing ever seeded).
	for i := 0; i < 20; i++ {
		w.refresh(clk.now())
		_, _, fresh, expWorking := w.snapshot(clk.now())
		if fresh || expWorking {
			t.Fatalf("unreachable watcher reported fresh:%v expiredWorking:%v, want both false", fresh, expWorking)
		}
		time.Sleep(5 * time.Millisecond)
	}

	done := make(chan struct{})
	go func() { w.close(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("close() blocked on an unreachable watcher")
	}
	select {
	case <-w.done:
	case <-time.After(2 * time.Second):
		t.Fatal("goroutine leaked after close")
	}
}

// A 401 on /event (a password somehow reached opencode) degrades to disconnect+backoff — it
// reconnects at a bounded rate (no hot loop) and never reports fresh.
func TestOpencodeWatcher401(t *testing.T) {
	f := newFakeOpencode(t)
	f.setEventStatus(http.StatusUnauthorized)
	f.onEvent = func(conn int64, w io.Writer, flush func(), ctx context.Context) {}

	clk := opencodeClock()
	w := newOpencodeWatcher(f.port(t), ocFixtureDir, ocFixtureSID, time.Time{}, clk.now, nil)
	t.Cleanup(w.close)

	pollUntil(t, "at least one 401 attempt", func() bool { return f.eventConns.Load() >= 1 })
	// Let it run briefly, then confirm it is not hot-looping (backoff caps the attempt rate).
	time.Sleep(300 * time.Millisecond)
	if n := f.eventConns.Load(); n > 30 {
		t.Fatalf("401 hot-loop: %d attempts in ~300ms (backoff should bound this)", n)
	}
	w.refresh(clk.now())
	if _, _, fresh, _ := w.snapshot(clk.now()); fresh {
		t.Fatal("a 401 watcher must never report fresh")
	}
}

// A malformed SSE frame (non-JSON data) is tolerantly skipped: the surrounding valid arc still
// folds to needs_input with no panic.
func TestOpencodeWatcherMalformedFrame(t *testing.T) {
	f := newFakeOpencode(t)
	f.statusBody = fmt.Sprintf(`{%q:{"type":"busy"}}`, ocFixtureSID)
	f.onEvent = func(conn int64, w io.Writer, flush func(), ctx context.Context) {
		writeSSE(w, flush, sseServerConnected)
		writeSSE(w, flush, `{"type":"session.created","properties":{"sessionID":"`+ocFixtureSID+`","info":{"id":"`+ocFixtureSID+`","directory":"`+ocFixtureDir+`","parentID":""}}}`)
		writeSSE(w, flush, `this is not json at all`) // garbage data line
		writeSSE(w, flush, `{"type":`)                // truncated JSON
		writeSSE(w, flush, fmt.Sprintf(`{"type":"session.idle","properties":{"sessionID":%q}}`, ocFixtureSID))
		<-ctx.Done()
	}

	clk := opencodeClock()
	w := newOpencodeWatcher(f.port(t), ocFixtureDir, "", time.Time{}, clk.now, nil)
	t.Cleanup(w.close)

	refreshUntil(t, w, clk, nil, "arc folds despite garbage frames", func() bool {
		act, _, _, _ := w.snapshot(clk.now())
		return act == ActivityNeedsInput
	})
}

// An oversized SSE line trips the scanner's cap → the connection errors → reconnect (no panic,
// no unbounded read). The second connection serves a normal stream.
func TestOpencodeWatcherOversizedFrame(t *testing.T) {
	f := newFakeOpencode(t)
	f.onEvent = func(conn int64, w io.Writer, flush func(), ctx context.Context) {
		if conn == 1 {
			// One data line larger than maxSSELineBytes → bufio.ErrTooLong.
			_, _ = io.WriteString(w, "data: ")
			_, _ = io.WriteString(w, strings.Repeat("x", maxSSELineBytes+1024))
			_, _ = io.WriteString(w, "\n\n")
			flush()
			<-ctx.Done()
			return
		}
		writeSSE(w, flush, sseServerConnected)
		<-ctx.Done()
	}

	clk := opencodeClock()
	w := newOpencodeWatcher(f.port(t), ocFixtureDir, ocFixtureSID, time.Time{}, clk.now, nil)
	t.Cleanup(w.close)

	pollUntil(t, "watcher reconnects past the oversized frame", func() bool {
		return f.eventConns.Load() >= 2
	})
	// Sanity: still alive and refreshable, no panic.
	w.refresh(clk.now())
}

// Inbox overflow (more envelopes than the bound before refresh drains) enqueues an overflowGap
// marker and forces a full reconnect+reseed rather than silently dropping records.
func TestOpencodeWatcherInboxOverflow(t *testing.T) {
	f := newFakeOpencode(t)
	f.statusBody = fmt.Sprintf(`{%q:{"type":"busy"}}`, ocFixtureSID)
	f.onEvent = func(conn int64, w io.Writer, flush func(), ctx context.Context) {
		writeSSE(w, flush, sseServerConnected)
		if conn == 1 {
			// Flood the inbox well past maxInboxItems (the test never refreshes during the
			// flood, so nothing drains) → overflow → forced reconnect.
			for i := 0; i < maxInboxItems+200; i++ {
				writeSSE(w, flush, fmt.Sprintf(`{"type":"session.status","properties":{"sessionID":%q,"status":{"type":"busy"}}}`, ocFixtureSID))
			}
			<-ctx.Done()
			return
		}
		<-ctx.Done()
	}

	clk := opencodeClock()
	w := newOpencodeWatcher(f.port(t), ocFixtureDir, ocFixtureSID, time.Time{}, clk.now, nil)
	t.Cleanup(w.close)

	// The overflow must force a reconnect (a second /event connection).
	pollUntil(t, "overflow forces a reconnect+reseed", func() bool {
		return f.eventConns.Load() >= 2
	})

	// And a subsequent refresh applies the overflowGap marker (noteGap) without panicking.
	w.refresh(clk.now())
}

// ---- fix #2: a superseded connection's seedComplete must not authorize a newer connection ----

// Connection A queues a seedComplete then disconnects before any refresh; connection B connects
// and BEGINS seeding (blocked mid-seed). A refresh must NOT report the watcher authoritative on
// A's stale-generation marker — only B's own (current-generation) seedComplete may (fix #2).
func TestOpencodeWatcherStaleSeedCompleteIgnored(t *testing.T) {
	f := newFakeOpencode(t)
	f.messagesBody = ocRESTMessages
	f.statusBody = fmt.Sprintf(`{%q:{"type":"busy"}}`, ocFixtureSID)

	gate := make(chan struct{})
	// Block connection B's seed (the 2nd /message call) until released, so B is "seeding but not
	// complete" while the test refreshes. Connection A's seed (1st call) runs to completion.
	f.setBeforeMessages(func(call int64, ctx context.Context) {
		if call >= 2 {
			select {
			case <-gate:
			case <-ctx.Done():
			}
		}
	})
	f.onEvent = func(conn int64, w io.Writer, flush func(), ctx context.Context) {
		writeSSE(w, flush, sseServerConnected)
		if conn == 1 {
			return // A: connected + REST-seeds, then the /event connection ENDS → reconnect to B
		}
		<-ctx.Done() // B: stay connected while its seed is gated
	}

	clk := opencodeClock()
	w := newOpencodeWatcher(f.port(t), ocFixtureDir, ocFixtureSID, time.Time{}, clk.now, nil)
	t.Cleanup(w.close)

	// messagesHits>=2 guarantees B's connectAndStream has advanced its generation (beginGeneration
	// runs before the GET, before the seed's /message call) — so A's queued seedComplete is stale.
	pollUntil(t, "connection B begins seeding", func() bool { return f.messagesHits.Load() >= 2 })

	// A refresh drains A's synths + A's stale-generation seedComplete (dropped): the watcher must
	// NOT become authoritative until B folds its OWN seedComplete.
	for i := 0; i < 20; i++ {
		w.refresh(clk.now())
		if _, _, fresh, _ := w.snapshot(clk.now()); fresh {
			t.Fatalf("stale-generation seedComplete made the watcher authoritative before B's own seed")
		}
		time.Sleep(5 * time.Millisecond)
	}

	// Release B's seed → B pushes its own current-generation seedComplete → now authoritative.
	close(gate)
	refreshUntil(t, w, clk, nil, "B's own seed completes → authoritative", func() bool {
		_, _, fresh, _ := w.snapshot(clk.now())
		return fresh
	})
}

// ---- fix #3: live status is authoritative; REST /session/status is a fallback only ----

// A live session.status busy buffered during/after the seed wins over a stale REST idle: the
// final verdict reflects the live stream (working), and the REST idle fallback never flips it.
func TestOpencodeWatcherLiveStatusWinsOverRESTFallback(t *testing.T) {
	f := newFakeOpencode(t)
	f.messagesBody = "[]"
	f.statusBody = "{}" // REST status: idle (the pinned id is absent from the map)
	f.onEvent = func(conn int64, w io.Writer, flush func(), ctx context.Context) {
		writeSSE(w, flush, sseServerConnected)
		// A live busy status (buffered while the goroutine does the REST seed): live is authoritative.
		writeSSE(w, flush, fmt.Sprintf(`{"type":"session.status","properties":{"sessionID":%q,"status":{"type":"busy"}}}`, ocFixtureSID))
		<-ctx.Done()
	}

	clk := opencodeClock()
	w := newOpencodeWatcher(f.port(t), ocFixtureDir, ocFixtureSID, time.Time{}, clk.now, nil)
	t.Cleanup(w.close)

	refreshUntil(t, w, clk, nil, "live busy wins over REST idle", func() bool {
		act, _, _, _ := w.snapshot(clk.now())
		return act == ActivityWorking
	})
	// It stays working: the REST idle fallback (applied at most once, and only when no live status
	// was seen) must never later override the live busy.
	for i := 0; i < 20; i++ {
		w.refresh(clk.now())
		if act, _, _, _ := w.snapshot(clk.now()); act != ActivityWorking {
			t.Fatalf("REST idle fallback spuriously flipped a live-busy session to %q", act)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// A genuinely-idle-at-connect session (no live status events, /session/status lacks the id) still
// synthesizes idle → needs_input via the REST fallback.
func TestOpencodeWatcherRESTIdleFallbackWhenNoLiveStatus(t *testing.T) {
	f := newFakeOpencode(t)
	f.messagesBody = "[]"
	f.statusBody = "{}" // idle: id absent, and no live status events arrive
	f.onEvent = func(conn int64, w io.Writer, flush func(), ctx context.Context) {
		writeSSE(w, flush, sseServerConnected)
		<-ctx.Done()
	}

	clk := opencodeClock()
	w := newOpencodeWatcher(f.port(t), ocFixtureDir, ocFixtureSID, time.Time{}, clk.now, nil)
	t.Cleanup(w.close)

	refreshUntil(t, w, clk, nil, "REST idle fallback → needs_input", func() bool {
		act, _, fresh, _ := w.snapshot(clk.now())
		return act == ActivityNeedsInput && fresh
	})
}

// ---- fix #5: a failed /session/status seed is NOT declared complete ----

// GET /session/status returns 500 → the boundary can't be established → the seed FAILS: the
// watcher never reports fresh from that connection and reconnects to reseed.
func TestOpencodeWatcherStatusSeedFailureReconnects(t *testing.T) {
	f := newFakeOpencode(t)
	f.messagesBody = ocRESTMessages
	f.setStatusStatus(http.StatusInternalServerError)
	f.onEvent = func(conn int64, w io.Writer, flush func(), ctx context.Context) {
		writeSSE(w, flush, sseServerConnected)
		<-ctx.Done()
	}

	clk := opencodeClock()
	w := newOpencodeWatcher(f.port(t), ocFixtureDir, ocFixtureSID, time.Time{}, clk.now, nil)
	t.Cleanup(w.close)

	pollUntil(t, "status-seed failure forces reconnect", func() bool { return f.eventConns.Load() >= 2 })
	for i := 0; i < 30; i++ {
		w.refresh(clk.now())
		if _, _, fresh, expWorking := w.snapshot(clk.now()); fresh || expWorking {
			t.Fatalf("status-seed-failed snapshot = fresh:%v expiredWorking:%v, want both false", fresh, expWorking)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// ---- fix #4: a follow-only candidate seed failure is not swallowed ----

// A GET /session candidate whose message fetch fails during the follow-only seed must NOT be kept
// (a later SSE frame could blindly declare it authoritative over incomplete history): propagate
// the error → reconnect+reseed, never fresh, never back-written.
func TestOpencodeWatcherCandidateSeedFailureReconnects(t *testing.T) {
	f := newFakeOpencode(t)
	f.sessionBody = fmt.Sprintf(`[{"id":%q,"directory":%q,"parentID":""}]`, ocFixtureSID, ocFixtureDir)
	f.setMessagesStatus(http.StatusInternalServerError) // candidate seed's message fetch fails
	f.onEvent = func(conn int64, w io.Writer, flush func(), ctx context.Context) {
		writeSSE(w, flush, sseServerConnected)
		<-ctx.Done()
	}

	clk := opencodeClock()
	w := newOpencodeWatcher(f.port(t), ocFixtureDir, "", time.Time{}, clk.now, nil) // no priorID → candidate path
	t.Cleanup(w.close)

	pollUntil(t, "candidate seed failure forces reconnect", func() bool { return f.eventConns.Load() >= 2 })
	for i := 0; i < 30; i++ {
		w.refresh(clk.now())
		if _, _, fresh, expWorking := w.snapshot(clk.now()); fresh || expWorking {
			t.Fatalf("candidate-seed-failed snapshot = fresh:%v expiredWorking:%v, want both false", fresh, expWorking)
		}
		if got := w.drainConfirmedAgentID(); got != "" {
			t.Fatalf("candidate seed failed but its id was back-written: %q", got)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// ---- fix #6: a closed watcher revokes authority (never fresh) ----

// After close() the watcher must report BOTH fresh=false AND expiredWorking=false even if it was
// healthy+working an instant earlier (close clears connected/seedApplied; snapshot short-circuits).
func TestOpencodeWatcherClosedNotFresh(t *testing.T) {
	f := newFakeOpencode(t)
	f.statusBody = fmt.Sprintf(`{%q:{"type":"busy"}}`, ocFixtureSID) // seed → working
	f.onEvent = func(conn int64, w io.Writer, flush func(), ctx context.Context) {
		writeSSE(w, flush, sseServerConnected)
		<-ctx.Done()
	}

	clk := opencodeClock()
	w := newOpencodeWatcher(f.port(t), ocFixtureDir, ocFixtureSID, time.Time{}, clk.now, nil)

	refreshUntil(t, w, clk, nil, "seed → working, fresh", func() bool {
		act, _, fresh, _ := w.snapshot(clk.now())
		return act == ActivityWorking && fresh
	})

	w.close()
	w.refresh(clk.now()) // a post-close refresh no-ops
	if _, _, fresh, expWorking := w.snapshot(clk.now()); fresh || expWorking {
		t.Fatalf("closed snapshot = fresh:%v expiredWorking:%v, want both false", fresh, expWorking)
	}
	select {
	case <-w.done:
	case <-time.After(2 * time.Second):
		t.Fatal("goroutine leaked after close")
	}
}

// ---- fix #7: inbox overflow atomically revokes authority ----

// Forcing an inbox overflow must make the watcher non-authoritative IMMEDIATELY (seedApplied=false
// under the enqueue lock) and keep it non-authoritative until a reseed completes.
func TestOpencodeWatcherOverflowRevokesAuthorityImmediately(t *testing.T) {
	f := newFakeOpencode(t)
	f.statusBody = fmt.Sprintf(`{%q:{"type":"busy"}}`, ocFixtureSID)
	flood := make(chan struct{})
	f.onEvent = func(conn int64, w io.Writer, flush func(), ctx context.Context) {
		if conn == 1 {
			writeSSE(w, flush, sseServerConnected)
			<-flood // hold until the test has observed a fresh verdict
			for i := 0; i < maxInboxItems+200; i++ {
				writeSSE(w, flush, fmt.Sprintf(`{"type":"session.status","properties":{"sessionID":%q,"status":{"type":"busy"}}}`, ocFixtureSID))
			}
			<-ctx.Done()
			return
		}
		// The forced reconnect stays silent (no server.connected → no reseed) so the watcher must
		// remain non-authoritative "until the forced reseed completes".
		<-ctx.Done()
	}

	clk := opencodeClock()
	w := newOpencodeWatcher(f.port(t), ocFixtureDir, ocFixtureSID, time.Time{}, clk.now, nil)
	t.Cleanup(w.close)

	refreshUntil(t, w, clk, nil, "seed → working, fresh", func() bool {
		act, _, fresh, _ := w.snapshot(clk.now())
		return act == ActivityWorking && fresh
	})

	close(flood) // trigger the flood → inbox overflow
	pollUntil(t, "overflow forces a reconnect+reseed", func() bool { return f.eventConns.Load() >= 2 })
	for i := 0; i < 40; i++ {
		w.refresh(clk.now())
		if _, _, fresh, expWorking := w.snapshot(clk.now()); fresh || expWorking {
			t.Fatalf("post-overflow snapshot = fresh:%v expiredWorking:%v, want both false (authority revoked)", fresh, expWorking)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// ---- fix #8: persistent post-connect failures do NOT reset the reconnect backoff ----

// The pure state machine: only a successful seed resets the floor; a failure grows exponentially
// (capped). Deterministic — no wall-clock dependence.
func TestNextReconnectBackoff(t *testing.T) {
	if got := nextReconnectBackoff(2*time.Second, true); got != ocBackoffBase {
		t.Fatalf("a successful seed must reset to the floor: got %v want %v", got, ocBackoffBase)
	}
	if got := nextReconnectBackoff(ocBackoffBase, false); got <= ocBackoffBase {
		t.Fatalf("a post-connect failure must GROW the backoff, not reset it: got %v", got)
	}
	cur, prev := ocBackoffBase, ocBackoffBase
	for i := 0; i < 20; i++ {
		cur = nextReconnectBackoff(cur, false)
		if cur < prev {
			t.Fatalf("backoff must never shrink on a failure: %v -> %v", prev, cur)
		}
		prev = cur
	}
	if cur != ocBackoffMax {
		t.Fatalf("repeated failures must reach the cap: got %v want %v", cur, ocBackoffMax)
	}
}

// A connection that reaches server.connected but then FAILS its REST seed must keep growing the
// backoff (fix #8) — the pre-fix "reset on server.connected" would peg it at the floor and hot-loop.
func TestOpencodeWatcherSeedFailGrowsBackoff(t *testing.T) {
	f := newFakeOpencode(t)
	f.setMessagesStatus(http.StatusInternalServerError) // reach server.connected, then FAIL the seed
	f.onEvent = func(conn int64, w io.Writer, flush func(), ctx context.Context) {
		writeSSE(w, flush, sseServerConnected) // connected, but the seed's /message read 500s
	}

	clk := opencodeClock()
	w := newOpencodeWatcher(f.port(t), ocFixtureDir, ocFixtureSID, time.Time{}, clk.now, nil)
	t.Cleanup(w.close)

	pollUntil(t, "multiple connect-then-seed-fail attempts", func() bool { return f.eventConns.Load() >= 2 })
	pollUntil(t, "backoff grows beyond the floor after seed failures", func() bool {
		return w.getBackoff() > ocBackoffBase
	})
}

// ---- fix #9: comment/empty-data heartbeats keep the watcher fresh ----

// A stream that, after connect, delivers ONLY `: comment` heartbeat frames (no data:) must keep the
// watcher from going heartbeat-stale — markFrame fires on every scanned line, not only on frames
// that yield a payload.
func TestOpencodeWatcherHeartbeatKeepsFresh(t *testing.T) {
	f := newFakeOpencode(t)
	f.statusBody = fmt.Sprintf(`{%q:{"type":"busy"}}`, ocFixtureSID) // seed → working
	beat := make(chan struct{})
	f.onEvent = func(conn int64, w io.Writer, flush func(), ctx context.Context) {
		writeSSE(w, flush, sseServerConnected)
		for {
			select {
			case <-ctx.Done():
				return
			case <-beat:
				_, _ = io.WriteString(w, ": heartbeat\n\n") // comment-only frame, no data:
				flush()
			}
		}
	}

	clk := opencodeClock()
	w := newOpencodeWatcher(f.port(t), ocFixtureDir, ocFixtureSID, time.Time{}, clk.now, nil)
	t.Cleanup(w.close)

	lastFrame := func() time.Time {
		w.mu.Lock()
		defer w.mu.Unlock()
		return w.lastFrameAt
	}

	refreshUntil(t, w, clk, nil, "seed → working, fresh", func() bool {
		act, _, fresh, _ := w.snapshot(clk.now())
		return act == ActivityWorking && fresh
	})

	// Advance to just under the stale window, then deliver a comment heartbeat.
	clk.advance(ocFrameStaleWindow - time.Second)
	beat <- struct{}{}
	pollUntil(t, "comment heartbeat bumps lastFrameAt", func() bool {
		return !lastFrame().Before(clk.now())
	})

	// Advance past the ORIGINAL stale window but within the heartbeat-refreshed window: the comment
	// heartbeat must have kept the watcher fresh (without fix #9 lastFrameAt would be frozen at connect).
	clk.advance(2 * time.Second)
	w.refresh(clk.now())
	if act, _, fresh, _ := w.snapshot(clk.now()); !fresh {
		t.Fatalf("comment heartbeat should keep the watcher fresh; got act=%q fresh=false", act)
	}
}

// ---- fix #10: REST calls honor the close guard (close during a REST seed does not hang) ----

// close() during an in-flight REST seed call unblocks promptly and the goroutine exits. The
// getJSON close-recheck-after-Do guard (fix #10) covers the raced-success case; request-context
// cancellation (w.ctx) covers the blocked-Do case exercised here.
func TestOpencodeWatcherCloseDuringRESTSeed(t *testing.T) {
	f := newFakeOpencode(t)
	f.messagesBody = ocRESTMessages
	entered := make(chan struct{})
	var once sync.Once
	f.setBeforeMessages(func(call int64, ctx context.Context) {
		once.Do(func() { close(entered) })
		<-ctx.Done() // block the seed's /message read until its request context is cancelled
	})
	f.onEvent = func(conn int64, w io.Writer, flush func(), ctx context.Context) {
		writeSSE(w, flush, sseServerConnected)
		<-ctx.Done()
	}

	clk := opencodeClock()
	w := newOpencodeWatcher(f.port(t), ocFixtureDir, ocFixtureSID, time.Time{}, clk.now, nil)

	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("watcher never reached the REST seed")
	}

	done := make(chan struct{})
	go func() { w.close(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("close() blocked during a REST seed")
	}
	select {
	case <-w.done:
	case <-time.After(2 * time.Second):
		t.Fatal("goroutine leaked (run did not exit after close during REST seed)")
	}
}

// ---- approvals: fold state over the live transport ----

// askThenReply starts a watcher against a busy fake opencode that announces ONE permission ask
// (per_1, bash, metadata.command "ls -la") and then holds its matching permission.replied
// ("once" → allow) until the returned channel is closed — the shared fixture for the two tests
// that need a live ask whose resolution the test times.
func askThenReply(t *testing.T) (*opencodeWatcher, *hubClock, chan struct{}) {
	t.Helper()
	f := newFakeOpencode(t)
	f.statusBody = fmt.Sprintf(`{%q:{"type":"busy"}}`, ocFixtureSID)
	ask := fmt.Sprintf(`{"type":"permission.asked","properties":{"id":"per_1","sessionID":%q,"permission":"bash","patterns":["ls"],"metadata":{"command":"ls -la"}}}`, ocFixtureSID)
	replied := fmt.Sprintf(`{"type":"permission.replied","properties":{"sessionID":%q,"requestID":"per_1","reply":"once"}}`, ocFixtureSID)
	release := make(chan struct{})
	f.onEvent = func(conn int64, w io.Writer, flush func(), ctx context.Context) {
		writeSSE(w, flush, sseServerConnected)
		writeSSE(w, flush, ask)
		select {
		case <-release:
			writeSSE(w, flush, replied)
		case <-ctx.Done():
			return
		}
		<-ctx.Done()
	}
	clk := opencodeClock()
	w := newOpencodeWatcher(f.port(t), ocFixtureDir, ocFixtureSID, time.Time{}, clk.now, nil)
	t.Cleanup(w.close)
	return w, clk, release
}

// A permission.asked on the live stream flips the watcher to needs_approval, publishes the
// pending snapshot, and emits the approval_request row; the permission.replied that follows
// resolves it (one resolved row) and releases the verdict. A needs_approval verdict is SETTLED,
// so it stays authoritative while the transport is healthy.
func TestOpencodeWatcherApprovalArc(t *testing.T) {
	w, clk, release := askThenReply(t)

	var rows []feedMessage
	refreshUntil(t, w, clk, &rows, "the ask blocks the session", func() bool {
		act, _, _, _ := w.snapshot(clk.now())
		return act == ActivityNeedsApproval
	})
	_, _, fresh, expWorking := w.snapshot(clk.now())
	if !fresh || expWorking {
		t.Fatalf("needs_approval snapshot = fresh:%v expiredWorking:%v, want fresh:true (settled)", fresh, expWorking)
	}
	pend := w.pendingApprovals()
	if len(pend) != 1 || pend[0].ID != "per_1" || pend[0].Status != approvalStatusPending || len(pend[0].Decisions) != 3 {
		t.Fatalf("pendingApprovals = %+v, want the one open ask with three decisions", pend)
	}
	if status, decision, ok := w.approvalState("per_1"); !ok || status != approvalStatusPending || decision != "" {
		t.Fatalf("approvalState = (%q,%q,%v), want (pending,\"\",true)", status, decision, ok)
	}
	if len(rows) != 1 || rows[0].Type != feedTypeApprovalRequest || rows[0].Approval == nil ||
		rows[0].Approval.ID != "per_1" || rows[0].Tool == nil || rows[0].Tool.Detail != "ls -la" {
		t.Fatalf("feed rows = %s, want one pending approval_request row with the command detail", formatOpencodeRows(rows))
	}

	close(release)
	refreshUntil(t, w, clk, &rows, "the reply releases the session", func() bool {
		act, _, _, _ := w.snapshot(clk.now())
		return act == ActivityWorking
	})
	if got := w.pendingApprovals(); len(got) != 0 {
		t.Fatalf("pendingApprovals after the reply = %+v, want empty", got)
	}
	if status, decision, ok := w.approvalState("per_1"); !ok || status != approvalStatusResolved || decision != approvalDecisionAllow {
		t.Fatalf("approvalState after the reply = (%q,%q,%v), want (resolved,allow,true)", status, decision, ok)
	}
	if len(rows) != 2 || rows[1].Approval == nil || rows[1].Approval.Status != approvalStatusResolved ||
		rows[1].Approval.Decision != approvalDecisionAllow {
		t.Fatalf("feed rows = %s, want a second, resolved row carrying decision allow", formatOpencodeRows(rows))
	}
}

// The dead-stream demotion, pinned: a needs_approval verdict is settled (trusted indefinitely
// while healthy), but a wedged stream revokes that authority — snapshot reports BOTH flags
// false so pane stability drives. An approval nobody is still watching for must not outlive
// the evidence for it.
func TestOpencodeWatcherNeedsApprovalDemotedOnDeadStream(t *testing.T) {
	f := newFakeOpencode(t)
	f.statusBody = fmt.Sprintf(`{%q:{"type":"busy"}}`, ocFixtureSID)
	ask := fmt.Sprintf(`{"type":"permission.asked","properties":{"id":"per_1","sessionID":%q,"permission":"bash","patterns":["ls"]}}`, ocFixtureSID)
	f.onEvent = func(conn int64, w io.Writer, flush func(), ctx context.Context) {
		writeSSE(w, flush, sseServerConnected)
		writeSSE(w, flush, ask)
		<-ctx.Done() // no further frames (not even heartbeats): the stream goes stale
	}

	clk := opencodeClock()
	w := newOpencodeWatcher(f.port(t), ocFixtureDir, ocFixtureSID, time.Time{}, clk.now, nil)
	t.Cleanup(w.close)

	refreshUntil(t, w, clk, nil, "needs_approval, healthy", func() bool {
		act, _, fresh, _ := w.snapshot(clk.now())
		return act == ActivityNeedsApproval && fresh
	})

	clk.advance(ocFrameStaleWindow + time.Second)
	w.refresh(clk.now())
	act, _, fresh, expWorking := w.snapshot(clk.now())
	if act != ActivityNeedsApproval {
		t.Fatalf("activity = %q, want needs_approval (verdict retained, just untrusted)", act)
	}
	if fresh || expWorking {
		t.Fatalf("heartbeat-stale needs_approval = fresh:%v expiredWorking:%v, want both false", fresh, expWorking)
	}
	if merged, _ := mergedActivity(act, "", fresh, expWorking, ActivityIdle); merged != ActivityIdle {
		t.Fatalf("mergedActivity = %q, want idle (stability drives a dead stream)", merged)
	}
}

// Restart/reconnect rebuild: the REST seed (GET /permission + /question, pin-filtered)
// reconstructs the open-ask state — pending_approvals survives a hub restart as the contract
// requires — and a reseed emits no duplicate rows.
func TestOpencodeWatcherSeedRebuildsApprovals(t *testing.T) {
	f := newFakeOpencode(t)
	f.statusBody = fmt.Sprintf(`{%q:{"type":"busy"}}`, ocFixtureSID)
	f.permissionBody = fmt.Sprintf(
		`[{"id":"per_seed","sessionID":%q,"permission":"bash","patterns":["ls"],"metadata":{"command":"ls"}},`+
			`{"id":"per_other","sessionID":"ses_other","permission":"edit","patterns":["x.go"]}]`, ocFixtureSID)
	f.questionBody = fmt.Sprintf(`[{"id":"que_seed","sessionID":%q,"questions":[{"header":"Which file?"}]}]`, ocFixtureSID)
	f.onEvent = func(conn int64, w io.Writer, flush func(), ctx context.Context) {
		writeSSE(w, flush, sseServerConnected)
		if conn == 1 {
			return // drop the first connection → reconnect + full reseed
		}
		<-ctx.Done()
	}

	clk := opencodeClock()
	w := newOpencodeWatcher(f.port(t), ocFixtureDir, ocFixtureSID, time.Time{}, clk.now, nil)
	t.Cleanup(w.close)

	var rows []feedMessage
	refreshUntil(t, w, clk, &rows, "the seed rebuilds the open asks", func() bool {
		act, _, _, _ := w.snapshot(clk.now())
		return act == ActivityNeedsApproval
	})
	pollUntil(t, "a reconnect reseeds", func() bool { return f.eventConns.Load() >= 2 })
	refreshUntil(t, w, clk, &rows, "the reseed is folded", func() bool {
		return f.messagesHits.Load() >= 2
	})
	// Drain once more so any row the reseed produced is observed before asserting.
	w.refresh(clk.now())
	rows = append(rows, w.drainPending()...)

	pend := w.pendingApprovals()
	if len(pend) != 1 || pend[0].ID != "per_seed" {
		t.Fatalf("pendingApprovals = %+v, want only the pinned session's open ask", pend)
	}
	if _, _, ok := w.approvalState("per_other"); ok {
		t.Error("another session's permission must never enter this watcher's fold (pin filter)")
	}
	var approvals, statusRows int
	for _, m := range rows {
		switch m.Type {
		case feedTypeApprovalRequest:
			approvals++
		case feedTypeStatus:
			statusRows++
		}
	}
	if approvals != 1 || statusRows != 1 {
		t.Fatalf("rows = %s, want exactly one approval row + one question status row across both seeds",
			formatOpencodeRows(rows))
	}
}

// A permission answered while the stream was down is retired by the next reseed: the seed's
// authoritative open-ask set no longer lists it, so the fold closes it (with no decision — the
// TUI answered, the hub cannot know which way) instead of blocking the session forever.
func TestOpencodeWatcherSeedRetiresAnsweredApproval(t *testing.T) {
	f := newFakeOpencode(t)
	f.statusBody = fmt.Sprintf(`{%q:{"type":"busy"}}`, ocFixtureSID)
	f.permissionBody = fmt.Sprintf(`[{"id":"per_1","sessionID":%q,"permission":"bash","patterns":["ls"]}]`, ocFixtureSID)
	drop := make(chan struct{})
	f.onEvent = func(conn int64, w io.Writer, flush func(), ctx context.Context) {
		writeSSE(w, flush, sseServerConnected)
		if conn == 1 {
			<-drop // hold connection 1 open until the test answers the ask out-of-band
			return
		}
		<-ctx.Done()
	}

	clk := opencodeClock()
	w := newOpencodeWatcher(f.port(t), ocFixtureDir, ocFixtureSID, time.Time{}, clk.now, nil)
	t.Cleanup(w.close)

	refreshUntil(t, w, clk, nil, "the seeded ask blocks the session", func() bool {
		act, _, _, _ := w.snapshot(clk.now())
		return act == ActivityNeedsApproval
	})

	// The operator answers in the TUI while our stream is down: the server stops listing it.
	f.mu.Lock()
	f.permissionBody = "[]"
	f.mu.Unlock()
	close(drop)

	refreshUntil(t, w, clk, nil, "the reseed retires the answered ask", func() bool {
		act, _, _, _ := w.snapshot(clk.now())
		return act == ActivityWorking
	})
	if got := w.pendingApprovals(); len(got) != 0 {
		t.Fatalf("pendingApprovals = %+v, want empty after the reseed", got)
	}
	status, decision, ok := w.approvalState("per_1")
	if !ok || status != approvalStatusResolved || decision != "" {
		t.Fatalf("approvalState = (%q,%q,%v), want (resolved,\"\",true)", status, decision, ok)
	}
}

// markApprovalResolved is the verb handler's synchronous bookkeeping: it resolves under the
// watcher mutex, updates the verdict immediately (no wait for the next tick), and dedupes
// against the permission.replied event that follows — exactly one resolved row.
func TestOpencodeWatcherMarkApprovalResolved(t *testing.T) {
	w, clk, release := askThenReply(t)

	var rows []feedMessage
	refreshUntil(t, w, clk, &rows, "the ask blocks the session", func() bool {
		act, _, _, _ := w.snapshot(clk.now())
		return act == ActivityNeedsApproval
	})

	if !w.markApprovalResolved("per_1", approvalDecisionAllow) {
		t.Fatal("markApprovalResolved must resolve the open ask")
	}
	// The verdict moves WITHOUT a refresh — the handler's answer must not lag a tick.
	if act, _, _, _ := w.snapshot(clk.now()); act != ActivityWorking {
		t.Fatalf("activity right after the local mark = %q, want working", act)
	}
	if w.markApprovalResolved("per_1", approvalDecisionAllow) {
		t.Error("a same-decision replay must report false (already resolved, no second POST)")
	}
	// An id this fold never saw asked records a resolved tombstone (the C2 handler answers
	// 404 before it would ever get here; the tombstone is what stops a later ask replay from
	// re-opening an answered permission).
	if !w.markApprovalResolved("per_unknown", approvalDecisionAllow) {
		t.Error("an unseen id must record a tombstone")
	}
	if status, _, ok := w.approvalState("per_unknown"); !ok || status != approvalStatusResolved {
		t.Errorf("tombstone state = (%q,%v), want (resolved,true)", status, ok)
	}

	// opencode's own event for per_1 now lands: still exactly one resolved row FOR per_1.
	close(release)
	pollUntil(t, "the replied frame is delivered", func() bool {
		w.refresh(clk.now())
		rows = append(rows, w.drainPending()...)
		return len(rows) >= 2
	})
	w.refresh(clk.now())
	rows = append(rows, w.drainPending()...)
	resolved := 0
	for _, m := range rows {
		if m.Approval != nil && m.Approval.Status == approvalStatusResolved && m.Approval.ID == "per_1" {
			resolved++
		}
	}
	if resolved != 1 {
		t.Fatalf("per_1 resolved rows = %d, want exactly 1 (local mark + event are one resolution):\n%s",
			resolved, formatOpencodeRows(rows))
	}
}

// The approvals map is the one piece of fold state a handler goroutine may write, so the
// two-writer discipline (stream-fed refresh vs. verb-driven resolve) is exercised directly
// under -race: concurrent markApprovalResolved / approvalState / pendingApprovals calls run
// against a stream that keeps folding, and exactly one of the racers may claim each ask.
func TestOpencodeWatcherApprovalsConcurrentAccess(t *testing.T) {
	const asks = 20
	f := newFakeOpencode(t)
	f.statusBody = fmt.Sprintf(`{%q:{"type":"busy"}}`, ocFixtureSID)
	f.onEvent = func(conn int64, w io.Writer, flush func(), ctx context.Context) {
		writeSSE(w, flush, sseServerConnected)
		for i := 0; i < asks; i++ {
			writeSSE(w, flush, fmt.Sprintf(
				`{"type":"permission.asked","properties":{"id":"per_%d","sessionID":%q,"permission":"bash","patterns":["ls"]}}`,
				i, ocFixtureSID))
		}
		<-ctx.Done()
	}

	clk := opencodeClock()
	w := newOpencodeWatcher(f.port(t), ocFixtureDir, ocFixtureSID, time.Time{}, clk.now, nil)
	t.Cleanup(w.close)

	refreshUntil(t, w, clk, nil, "the asks are folded", func() bool {
		return len(w.pendingApprovals()) == asks
	})

	var wg sync.WaitGroup
	var resolvedCount atomic.Int64
	for g := 0; g < 4; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < asks; i++ {
				id := fmt.Sprintf("per_%d", i)
				if w.markApprovalResolved(id, approvalDecisionAllow) {
					resolvedCount.Add(1)
				}
				_, _, _ = w.approvalState(id)
				_ = w.pendingApprovals()
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 50; i++ {
			w.refresh(clk.now())
			_ = w.drainPending()
		}
	}()
	wg.Wait()

	if got := resolvedCount.Load(); got != asks {
		t.Fatalf("resolutions claimed = %d, want %d (each ask resolves exactly once)", got, asks)
	}
	if got := w.pendingApprovals(); len(got) != 0 {
		t.Fatalf("pendingApprovals = %+v, want empty once every ask is resolved", got)
	}
}

// The seed marker's halves are independent END TO END: with GET /question failing on every
// connection, GET /permission still carries authority, so an approval answered in the TUI is
// retired by the next reseed. (Before the split, one failing read blocked all healing.)
func TestOpencodeWatcherSeedHealsPermissionsWithQuestionReadFailing(t *testing.T) {
	f := newFakeOpencode(t)
	f.statusBody = fmt.Sprintf(`{%q:{"type":"busy"}}`, ocFixtureSID)
	f.permissionBody = fmt.Sprintf(`[{"id":"per_1","sessionID":%q,"permission":"bash","patterns":["ls"]}]`, ocFixtureSID)
	f.questionStatus = http.StatusInternalServerError // /question is down for the whole test
	drop := make(chan struct{})
	f.onEvent = func(conn int64, w io.Writer, flush func(), ctx context.Context) {
		writeSSE(w, flush, sseServerConnected)
		if conn == 1 {
			<-drop
			return
		}
		<-ctx.Done()
	}

	clk := opencodeClock()
	w := newOpencodeWatcher(f.port(t), ocFixtureDir, ocFixtureSID, time.Time{}, clk.now, nil)
	t.Cleanup(w.close)

	refreshUntil(t, w, clk, nil, "the seeded ask blocks the session", func() bool {
		act, _, _, _ := w.snapshot(clk.now())
		return act == ActivityNeedsApproval
	})

	// Answered in the TUI: the server stops listing it. The question read still fails.
	f.mu.Lock()
	f.permissionBody = "[]"
	f.mu.Unlock()
	close(drop)

	refreshUntil(t, w, clk, nil, "the reseed retires the answered ask", func() bool {
		act, _, _, _ := w.snapshot(clk.now())
		return act == ActivityWorking
	})
	if got := w.pendingApprovals(); len(got) != 0 {
		t.Fatalf("pendingApprovals = %+v, want empty — a failing /question must not block permission healing", got)
	}
	if w.hasOpenApprovals() {
		t.Error("hasOpenApprovals must be false once the ask is retired")
	}
}

// ---- WS-B: the session-scoping invariant, adversarially ----
//
// The fake's guard (recordAndGuard) fails the test on ANY POST that is not a
// {pinned}-scoped verb route, so these tests assert the POSITIVE half (the exact routes
// and bodies each verb uses) while the guard covers the negative half (a global
// /permission/{id}/reply, a sibling session's route) from underneath every test in the
// package that ever POSTs.

// pinnedVerbWatcher starts a watcher already pinned (priorID) to sid against a fake whose
// mutation guard admits ONLY sid, with an /event stream that stays open. sid "" is the
// UNPINNED case: no prior pin to correlate to, and no id the guard would admit.
func pinnedVerbWatcher(t *testing.T, f *fakeOpencode, sid string) (*opencodeWatcher, *hubClock) {
	t.Helper()
	f.setPinGuard(sid)
	f.holdOpenSSE()
	clk := opencodeClock()
	w := newOpencodeWatcher(f.port(t), ocFixtureDir, sid, time.Time{}, clk.now, nil)
	t.Cleanup(w.close)
	return w, clk
}

// (a) Every verb addresses the pinned session's scoped v1 route — and nothing else. The
// bodies are pinned too: prompt_async's single text part and the permission reply's
// native vocabulary (allow → once) are the wire contract with opencode.
func TestOpencodeVerbsUseOnlyPinnedSessionRoutes(t *testing.T) {
	f := newFakeOpencode(t)
	w, _ := pinnedVerbWatcher(t, f, ocFixtureSID)

	turnID, err := w.startTurn(context.Background(), "run the tests")
	if err != nil {
		t.Fatalf("startTurn: %v", err)
	}
	if !strings.HasPrefix(turnID, "oc-") || len(turnID) <= len("oc-") {
		t.Errorf("turn id = %q, want a non-empty hub-generated handle", turnID)
	}
	if err := w.interruptTurn(context.Background()); err != nil {
		t.Fatalf("interruptTurn: %v", err)
	}
	if err := w.resolveApproval(context.Background(), "per_1", approvalDecisionAllow); err != nil {
		t.Fatalf("resolveApproval: %v", err)
	}

	want := []string{
		"/session/" + ocFixtureSID + "/prompt_async",
		"/session/" + ocFixtureSID + "/abort",
		"/session/" + ocFixtureSID + "/permissions/per_1",
	}
	if got := f.postPaths(); !slices.Equal(got, want) {
		t.Fatalf("POST paths = %v, want %v", got, want)
	}
	if body := f.postBody("/prompt_async"); body != `{"parts":[{"type":"text","text":"run the tests"}]}` {
		t.Errorf("prompt_async body = %s, want the single text part", body)
	}
	if body := f.postBody("/permissions/per_1"); body != `{"response":"once"}` {
		t.Errorf("permission reply body = %s, want the native `once` reply for allow", body)
	}
}

// The decision mapping in full, each answered on the pinned session's scoped route.
func TestOpencodeResolveApprovalDecisionMapping(t *testing.T) {
	cases := []struct{ decision, reply string }{
		{approvalDecisionAllow, "once"},
		{approvalDecisionAllowAlways, "always"},
		{approvalDecisionDeny, "reject"},
	}
	for _, c := range cases {
		t.Run(c.decision, func(t *testing.T) {
			f := newFakeOpencode(t)
			w, _ := pinnedVerbWatcher(t, f, ocFixtureSID)
			if err := w.resolveApproval(context.Background(), "per_"+c.reply, c.decision); err != nil {
				t.Fatalf("resolveApproval: %v", err)
			}
			if body := f.postBody("/permissions/per_" + c.reply); body != `{"response":"`+c.reply+`"}` {
				t.Fatalf("%s → body %s, want response %q", c.decision, body, c.reply)
			}
		})
	}
	// An out-of-enum decision never reaches the wire (the handler 400s first; this is the
	// belt-and-braces half).
	f := newFakeOpencode(t)
	w, _ := pinnedVerbWatcher(t, f, ocFixtureSID)
	if err := w.resolveApproval(context.Background(), "per_1", "maybe"); err == nil {
		t.Error("an unmapped decision must error, not POST a guess")
	}
	if got := f.postPaths(); len(got) != 0 {
		t.Errorf("unmapped decision produced POSTs %v, want none", got)
	}
}

// (b) Two rc sessions pinned to two different opencode sessions in the SAME store: verbs
// on one leave the other completely untouched. This is the global-store hazard in
// miniature — the thing a global write route would get wrong.
func TestOpencodeVerbsLeaveOtherSessionUntouched(t *testing.T) {
	f := newFakeOpencode(t)
	f.sessionBody = fmt.Sprintf(`[{"id":%q,"directory":%q,"parentID":""},{"id":%q,"directory":%q,"parentID":""}]`,
		ocFixtureSID, ocFixtureDir, ocOtherSID, ocOtherDir)
	f.holdOpenSSE()
	clk := opencodeClock()
	// Both watchers speak to the same embedded server; each is pinned to its own session.
	a := newOpencodeWatcher(f.port(t), ocFixtureDir, ocFixtureSID, time.Time{}, clk.now, nil)
	t.Cleanup(a.close)
	b := newOpencodeWatcher(f.port(t), ocOtherDir, ocOtherSID, time.Time{}, clk.now, nil)
	t.Cleanup(b.close)
	// No pinGuard here (both ids are legitimate for SOME watcher) — the assertion is that
	// A's verbs produced no traffic addressed to B.
	f.setPinGuard("")

	if _, err := a.startTurn(context.Background(), "steer A"); err != nil {
		t.Fatalf("startTurn: %v", err)
	}
	if err := a.interruptTurn(context.Background()); err != nil {
		t.Fatalf("interruptTurn: %v", err)
	}
	if err := a.resolveApproval(context.Background(), "per_a", approvalDecisionDeny); err != nil {
		t.Fatalf("resolveApproval: %v", err)
	}

	for _, p := range f.postPaths() {
		if !strings.HasPrefix(p, "/session/"+ocFixtureSID+"/") {
			t.Errorf("verb on session A touched %s — every mutation must be A-scoped", p)
		}
	}
	if got := len(f.postPaths()); got != 3 {
		t.Errorf("POSTs = %d, want exactly the 3 A-scoped verb calls", got)
	}
}

// (c) A verb on an UNPINNED watcher is a typed 409-shaped rejection with ZERO HTTP
// requests: the hub never guesses "the newest session" for an uncorrelated TUI.
func TestOpencodeVerbsUnpinnedRejectWithoutRequest(t *testing.T) {
	f := newFakeOpencode(t) // empty store: no candidate to follow, so the pin stays ""
	w, _ := pinnedVerbWatcher(t, f, "")

	if _, err := w.startTurn(context.Background(), "hi"); !errors.Is(err, errNoAgentSession) {
		t.Errorf("startTurn on an unpinned watcher = %v, want errNoAgentSession", err)
	}
	if err := w.interruptTurn(context.Background()); !errors.Is(err, errNoAgentSession) {
		t.Errorf("interruptTurn on an unpinned watcher = %v, want errNoAgentSession", err)
	}
	if err := w.resolveApproval(context.Background(), "per_1", approvalDecisionAllow); !errors.Is(err, errNoAgentSession) {
		t.Errorf("resolveApproval on an unpinned watcher = %v, want errNoAgentSession", err)
	}
	if got := f.postPaths(); len(got) != 0 {
		t.Errorf("unpinned verbs sent %v, want no request at all", got)
	}
}

// A CLOSED watcher is gone, not unpinned: the verbs report the closed sentinel (which the
// handler maps to the same retryable 409) and send nothing.
func TestOpencodeVerbsOnClosedWatcher(t *testing.T) {
	f := newFakeOpencode(t)
	w, _ := pinnedVerbWatcher(t, f, ocFixtureSID)
	w.close()

	if _, err := w.startTurn(context.Background(), "hi"); !errors.Is(err, errWatcherClosed) {
		t.Errorf("startTurn on a closed watcher = %v, want errWatcherClosed", err)
	}
	if err := w.interruptTurn(context.Background()); !errors.Is(err, errWatcherClosed) {
		t.Errorf("interruptTurn on a closed watcher = %v, want errWatcherClosed", err)
	}
	if got := f.postPaths(); len(got) != 0 {
		t.Errorf("closed-watcher verbs sent %v, want nothing", got)
	}
}

// An upstream non-2xx surfaces as an error (the handler's 409 not_accepting) rather than
// a silent success — the verb must never report a steer that did not land.
func TestOpencodeVerbsUpstreamFailure(t *testing.T) {
	f := newFakeOpencode(t)
	f.promptStatus = http.StatusInternalServerError
	f.abortStatus = http.StatusInternalServerError
	f.permissionStatus = http.StatusInternalServerError
	w, _ := pinnedVerbWatcher(t, f, ocFixtureSID)

	if _, err := w.startTurn(context.Background(), "hi"); err == nil {
		t.Error("a 500 from prompt_async must surface as an error")
	}
	if err := w.interruptTurn(context.Background()); err == nil {
		t.Error("a 500 from abort must surface as an error")
	}
	if err := w.resolveApproval(context.Background(), "per_1", approvalDecisionAllow); err == nil {
		t.Error("a 500 from the permission reply must surface as an error")
	}
}

// The IDLE-ABORT mapping, pinned: opencode answers an abort on an idle session
// successfully (200 `true`), and the lane PASSES THAT THROUGH — the hub does not
// second-guess the lane with a fabricated "no active turn" rejection.
func TestOpencodeInterruptIdleSessionSucceeds(t *testing.T) {
	f := newFakeOpencode(t)
	f.statusBody = "{}" // idle: the pinned id is absent from the status map
	w, clk := pinnedVerbWatcher(t, f, ocFixtureSID)

	refreshUntil(t, w, clk, nil, "the session settles idle", func() bool {
		act, _, _, _ := w.snapshot(clk.now())
		return act == ActivityNeedsInput
	})
	if err := w.interruptTurn(context.Background()); err != nil {
		t.Fatalf("interrupt on an idle session = %v, want success (200 passthrough)", err)
	}
	if got := f.postPaths(); len(got) != 1 || got[0] != "/session/"+ocFixtureSID+"/abort" {
		t.Fatalf("POST paths = %v, want the pinned session's abort", got)
	}
}

// (d) The READ side of the invariant, pinned as part of it: global GETs are legal for
// seed/discovery (and are used), every SESSION-SCOPED GET addresses the pin, and the
// global lists are filtered to the pin client-side — another session's open permission
// never enters this watcher's fold.
func TestOpencodeSeedGETsArePinFiltered(t *testing.T) {
	f := newFakeOpencode(t)
	f.statusBody = fmt.Sprintf(`{%q:{"type":"busy"}}`, ocFixtureSID)
	f.permissionBody = fmt.Sprintf(
		`[{"id":"per_mine","sessionID":%q,"permission":"bash","patterns":["ls"]},`+
			`{"id":"per_theirs","sessionID":%q,"permission":"edit","patterns":["x.go"]}]`, ocFixtureSID, ocOtherSID)
	f.questionBody = fmt.Sprintf(`[{"id":"que_theirs","sessionID":%q,"questions":[{"header":"Which file?"}]}]`, ocOtherSID)
	w, clk := pinnedVerbWatcher(t, f, ocFixtureSID)

	refreshUntil(t, w, clk, nil, "the seed folds the pinned session's ask", func() bool {
		act, _, _, _ := w.snapshot(clk.now())
		return act == ActivityNeedsApproval
	})

	if pend := w.pendingApprovals(); len(pend) != 1 || pend[0].ID != "per_mine" {
		t.Fatalf("pendingApprovals = %+v, want only the pinned session's ask", pend)
	}
	if _, _, ok := w.approvalState("per_theirs"); ok {
		t.Error("another session's permission entered the fold — the global GET must be pin-filtered")
	}
	if !w.hasOpenApprovals() {
		t.Error("the pinned session's own ask must still block")
	}

	gets := f.getPaths()
	var sawGlobal bool
	for _, p := range gets {
		if p == "/permission" || p == "/question" || p == "/session/status" {
			sawGlobal = true // legal: discovery/seed reads, filtered above
			continue
		}
		if strings.HasPrefix(p, "/session/") && !strings.HasPrefix(p, "/session/"+ocFixtureSID+"/") {
			t.Errorf("session-scoped GET %s addresses a session other than the pin", p)
		}
	}
	if !sawGlobal {
		t.Errorf("expected the seed to use the global discovery GETs; saw %v", gets)
	}
}

// ---- pin hardening: a pin must be a single safe path segment ----

// ocMalformedPins are the shapes that would ESCAPE the session-scoped route if they were
// ever interpolated into a URL: traversal, an embedded segment, a query/fragment splice,
// whitespace, and an over-long value.
var ocMalformedPins = []string{
	"ses_A/../../session/VICTIM",
	"ses_A/x",
	"ses_A?scope=project",
	"ses_A#frag",
	"ses A",
	"ses_A%2fVICTIM",
	"../ses_A",
	strings.Repeat("s", 300),
}

// A prior back-write (SHED_RC_AGENT_SESSION — guest-writable tmux env) that is not a safe
// path segment is DISCARDED: the watcher behaves as UNPINNED, so verbs reject with the
// no-agent-session sentinel and nothing is ever POSTed. Invariant hardening — an in-guest
// process can reach the port directly, but a request arriving over the server proxy must
// never be steerable onto another session.
func TestOpencodeWatcherRejectsMalformedPriorPin(t *testing.T) {
	for _, bad := range ocMalformedPins {
		t.Run(strconv.Quote(bad), func(t *testing.T) {
			f := newFakeOpencode(t)
			f.onEvent = func(conn int64, w io.Writer, flush func(), ctx context.Context) {
				writeSSE(w, flush, sseServerConnected)
				<-ctx.Done()
			}
			clk := opencodeClock()
			w := newOpencodeWatcher(f.port(t), ocFixtureDir, bad, time.Time{}, clk.now, nil)
			t.Cleanup(w.close)

			if got := w.getPinned(); got != "" {
				t.Fatalf("pinned = %q, want \"\" (a malformed pin is no pin)", got)
			}
			if _, err := w.startTurn(context.Background(), "hi"); !errors.Is(err, errNoAgentSession) {
				t.Errorf("startTurn = %v, want errNoAgentSession", err)
			}
			if err := w.resolveApproval(context.Background(), "per_1", approvalDecisionAllow); !errors.Is(err, errNoAgentSession) {
				t.Errorf("resolveApproval = %v, want errNoAgentSession", err)
			}
			if got := f.postPaths(); len(got) != 0 {
				t.Errorf("a malformed pin produced requests %v, want none", got)
			}
		})
	}
}

// The same rule on the DISCOVERY path: a session.created carrying a malformed id (a
// hostile or corrupt embedded server) is not a pin — the watcher keeps searching, never
// back-writes it, and stays unaddressable. A well-formed id on the same stream still pins,
// so the guard rejects the value, not the path.
func TestOpencodeWatcherRejectsMalformedDiscoveredPin(t *testing.T) {
	f := newFakeOpencode(t)
	f.statusBody = fmt.Sprintf(`{%q:{"type":"busy"}}`, ocFixtureSID)
	release := make(chan struct{})
	f.onEvent = func(conn int64, w io.Writer, flush func(), ctx context.Context) {
		writeSSE(w, flush, sseServerConnected)
		writeSSE(w, flush, fmt.Sprintf(
			`{"type":"session.created","properties":{"sessionID":"evil","info":{"id":"ses_A/../../session/VICTIM","directory":%q,"parentID":""}}}`, ocFixtureDir))
		<-release
		writeSSE(w, flush, fmt.Sprintf(
			`{"type":"session.created","properties":{"sessionID":%q,"info":{"id":%q,"directory":%q,"parentID":""}}}`,
			ocFixtureSID, ocFixtureSID, ocFixtureDir))
		<-ctx.Done()
	}
	clk := opencodeClock()
	w := newOpencodeWatcher(f.port(t), ocFixtureDir, "", time.Time{}, clk.now, nil)
	t.Cleanup(w.close)

	for i := 0; i < 20; i++ {
		w.refresh(clk.now())
		if got := w.getPinned(); got != "" {
			t.Fatalf("pinned = %q on a malformed session.created, want no pin", got)
		}
		if got := w.drainConfirmedAgentID(); got != "" {
			t.Fatalf("back-wrote a malformed id: %q", got)
		}
		time.Sleep(5 * time.Millisecond)
	}

	close(release)
	pollUntil(t, "the well-formed id still pins", func() bool { return w.getPinned() == ocFixtureSID })
}

// The SECOND layer, independent of the shape check: every interpolated segment is escaped,
// so even a value that somehow reached the builder stays ONE segment. Pinned on the
// builder directly because the validated pin can no longer carry such a value — and
// because the approval id (whose grammar admits "." and ":") reaches it from the request
// path.
func TestOpencodeSessionPathEscapesEverySegment(t *testing.T) {
	if got, want := ocSessionPath("ses_A/../victim", "permissions", "per/../x"),
		"/session/ses_A%2F..%2Fvictim/permissions/per%2F..%2Fx"; got != want {
		t.Errorf("ocSessionPath = %q, want %q", got, want)
	}
	// The ordinary shapes are untouched (an escaped path must still be the real route).
	if got, want := ocSessionPath(ocFixtureSID, "prompt_async"), "/session/"+ocFixtureSID+"/prompt_async"; got != want {
		t.Errorf("ocSessionPath = %q, want %q", got, want)
	}
	if got, want := ocSessionPath(ocFixtureSID, "permissions", "per_01HQ8Z3K.tool:2"),
		"/session/"+ocFixtureSID+"/permissions/per_01HQ8Z3K.tool:2"; got != want {
		t.Errorf("ocSessionPath = %q, want %q", got, want)
	}
}

// validOpencodeSessionID is the shape rule itself: real ids pass, anything that is not a
// single unreserved segment fails.
func TestValidOpencodeSessionID(t *testing.T) {
	for _, ok := range []string{ocFixtureSID, "ses_07cbd4370ffeF17Wb3Ius82a2g", "abc-123_XYZ", "9"} {
		if !validOpencodeSessionID(ok) {
			t.Errorf("validOpencodeSessionID(%q) = false, want true", ok)
		}
	}
	bad := append([]string{"", "ses.A", "ses:A", "ses/A", "."}, ocMalformedPins...)
	for _, b := range bad {
		if validOpencodeSessionID(b) {
			t.Errorf("validOpencodeSessionID(%q) = true, want false", b)
		}
	}
}

// ---- the resolution claim: one upstream POST per ask, ever ----

// Two concurrent resolves of the SAME id: the claim makes the check-then-act atomic, so
// exactly one request POSTs upstream and the other is told the resolution is in flight.
// Deterministic — the fake holds the first POST in a gate while the second runs.
func TestOpencodeApprovalClaimSerializesConcurrentResolves(t *testing.T) {
	f := newFakeOpencode(t)
	entered := make(chan struct{})
	release := make(chan struct{})
	f.beforeMutation = func(path string) {
		close(entered)
		<-release // hold the upstream call open while the racing request runs
	}
	w, clk := pinnedVerbWatcher(t, f, ocFixtureSID)
	// Fold one pending ask through the (fake) stream so the claim has something to take.
	w.mu.Lock()
	w.fold.applyLine([]byte(fmt.Sprintf(
		`{"type":"permission.asked","properties":{"id":"per_1","sessionID":%q,"permission":"bash","patterns":["ls"]}}`, ocFixtureSID)))
	w.mu.Unlock()
	_ = clk

	if got := w.claimApproval("per_1", approvalDecisionAllow); got != approvalClaimed {
		t.Fatalf("first claim = %v, want approvalClaimed", got)
	}
	done := make(chan error, 1)
	go func() { done <- w.resolveApproval(context.Background(), "per_1", approvalDecisionAllow) }()
	<-entered

	// While the first resolve is in flight, a second request cannot claim (and therefore
	// cannot POST).
	if got := w.claimApproval("per_1", approvalDecisionAllow); got != approvalClaimBusy {
		t.Fatalf("concurrent claim = %v, want approvalClaimBusy (same decision included)", got)
	}
	if got := w.claimApproval("per_1", approvalDecisionDeny); got != approvalClaimBusy {
		t.Fatalf("concurrent conflicting claim = %v, want approvalClaimBusy", got)
	}

	close(release)
	if err := <-done; err != nil {
		t.Fatalf("resolveApproval: %v", err)
	}
	if got := w.commitApproval("per_1", approvalDecisionAllow); got != approvalDecisionAllow {
		t.Fatalf("commitApproval = %q, want the recorded decision %q", got, approvalDecisionAllow)
	}
	// Post-commit the ask is settled, so a further claim reports settled (never busy —
	// the claim was consumed) and the handler answers from the recorded state.
	if got := w.claimApproval("per_1", approvalDecisionAllow); got != approvalClaimSettled {
		t.Fatalf("post-commit claim = %v, want approvalClaimSettled", got)
	}
	if got := f.postPaths(); len(got) != 1 {
		t.Fatalf("upstream POSTs = %v, want exactly one", got)
	}
}

// A released claim (the upstream write failed) leaves the ask answerable again — a failed
// POST resolved nothing, so the operator or a retry must still be able to take it.
func TestOpencodeApprovalClaimReleasedOnFailure(t *testing.T) {
	f := newFakeOpencode(t)
	w, _ := pinnedVerbWatcher(t, f, ocFixtureSID)
	w.mu.Lock()
	w.fold.applyLine([]byte(fmt.Sprintf(
		`{"type":"permission.asked","properties":{"id":"per_1","sessionID":%q,"permission":"bash","patterns":["ls"]}}`, ocFixtureSID)))
	w.mu.Unlock()

	if got := w.claimApproval("per_1", approvalDecisionAllow); got != approvalClaimed {
		t.Fatalf("claim = %v, want approvalClaimed", got)
	}
	w.releaseApproval("per_1")
	if got := w.claimApproval("per_1", approvalDecisionDeny); got != approvalClaimed {
		t.Fatalf("claim after release = %v, want approvalClaimed", got)
	}
	if status, _, ok := w.approvalState("per_1"); !ok || status != approvalStatusPending {
		t.Errorf("approvalState = (%q,%v), want it still pending after a released claim", status, ok)
	}
}

// The stream wins a race it lands first: opencode's own permission.replied resolving the
// id before the commit means the FOLD's decision is the truth, and commitApproval reports
// that recorded value rather than the caller's.
func TestOpencodeCommitApprovalReportsTheRecordedDecision(t *testing.T) {
	f := newFakeOpencode(t)
	w, _ := pinnedVerbWatcher(t, f, ocFixtureSID)
	w.mu.Lock()
	w.fold.applyLine([]byte(fmt.Sprintf(
		`{"type":"permission.asked","properties":{"id":"per_1","sessionID":%q,"permission":"bash","patterns":["ls"]}}`, ocFixtureSID)))
	// The stream's reply lands first, recording allow_always ("always").
	w.fold.applyLine([]byte(fmt.Sprintf(
		`{"type":"permission.replied","properties":{"sessionID":%q,"requestID":"per_1","reply":"always"}}`, ocFixtureSID)))
	w.mu.Unlock()

	if got := w.commitApproval("per_1", approvalDecisionAllow); got != approvalDecisionAllowAlways {
		t.Fatalf("commitApproval = %q, want the stream-recorded %q", got, approvalDecisionAllowAlways)
	}
}
