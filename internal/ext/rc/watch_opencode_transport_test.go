package rc

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
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
)

// ---- fake opencode server (httptest double: /event SSE + REST seed endpoints) ----

// fakeOpencode is a programmable stand-in for opencode's embedded HTTP+SSE server. onEvent
// drives one /event connection (called once per connect, with the 1-based connection index);
// the REST bodies are plain JSON strings the seed reads. All fields are safe to set before
// starting the watcher; counters are atomic so a test goroutine can observe progress.
type fakeOpencode struct {
	ts *httptest.Server

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
	beforeMessages func(call int64, ctx context.Context) // optional gate at the start of /message (call is 1-based)
}

func newFakeOpencode(t *testing.T) *fakeOpencode {
	t.Helper()
	f := &fakeOpencode{
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
		// Subtree: /session/{id}/message is history (a longer /session/status match wins
		// its exact path). Anything else under /session/ is unknown → 404.
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
		f.mu.Unlock()
		_, _ = io.WriteString(w, body)
	})
	f.ts = httptest.NewServer(mux)
	t.Cleanup(f.ts.Close)
	return f
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
	w := newOpencodeWatcher(f.port(t), ocFixtureDir, "", clk.now, nil)
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
	w := newOpencodeWatcher(f.port(t), ocFixtureDir, ocFixtureSID, clk.now, nil)
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
	w := newOpencodeWatcher(f.port(t), ocFixtureDir, "", clk.now, nil)
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
	w := newOpencodeWatcher(f.port(t), ocFixtureDir, "", clk.now, nil)
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
	w := newOpencodeWatcher(f.port(t), ocFixtureDir, ocFixtureSID, clk.now, nil)
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
	w := newOpencodeWatcher(f.port(t), ocFixtureDir, ocFixtureSID, clk.now, nil)
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
	w := newOpencodeWatcher(f.port(t), ocFixtureDir, "", clk.now, nil)
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
	w := newOpencodeWatcher(f.port(t), ocFixtureDir, ocFixtureSID, clk.now, nil)
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
	w := newOpencodeWatcher(f.port(t), ocFixtureDir, ocFixtureSID, clk.now, nil)
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
	w := newOpencodeWatcher(port, ocFixtureDir, ocFixtureSID, clk.now, nil)

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
	w := newOpencodeWatcher(f.port(t), ocFixtureDir, ocFixtureSID, clk.now, nil)
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
	w := newOpencodeWatcher(f.port(t), ocFixtureDir, "", clk.now, nil)
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
	w := newOpencodeWatcher(f.port(t), ocFixtureDir, ocFixtureSID, clk.now, nil)
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
	w := newOpencodeWatcher(f.port(t), ocFixtureDir, ocFixtureSID, clk.now, nil)
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
	w := newOpencodeWatcher(f.port(t), ocFixtureDir, ocFixtureSID, clk.now, nil)
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
	w := newOpencodeWatcher(f.port(t), ocFixtureDir, ocFixtureSID, clk.now, nil)
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
	w := newOpencodeWatcher(f.port(t), ocFixtureDir, ocFixtureSID, clk.now, nil)
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
	w := newOpencodeWatcher(f.port(t), ocFixtureDir, ocFixtureSID, clk.now, nil)
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
	w := newOpencodeWatcher(f.port(t), ocFixtureDir, "", clk.now, nil) // no priorID → candidate path
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
	w := newOpencodeWatcher(f.port(t), ocFixtureDir, ocFixtureSID, clk.now, nil)

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
	w := newOpencodeWatcher(f.port(t), ocFixtureDir, ocFixtureSID, clk.now, nil)
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
	w := newOpencodeWatcher(f.port(t), ocFixtureDir, ocFixtureSID, clk.now, nil)
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
	w := newOpencodeWatcher(f.port(t), ocFixtureDir, ocFixtureSID, clk.now, nil)
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
	w := newOpencodeWatcher(f.port(t), ocFixtureDir, ocFixtureSID, clk.now, nil)

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
