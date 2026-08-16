package rc

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// Tests for POST /v1/ingest/cursor (plan 008 §3.5) — the hub's one guest-internal route,
// and the push half of the cursor watcher. Everything here goes through the REAL mux, so
// the route's precedence (413 → 400 → 404 → 409) is pinned as a client sees it.

// ingestHub builds a hub tracking one session of the given kind, reconciled once (so the
// watcher exists), plus a live HTTP server on the hub's real handler.
func ingestHub(t *testing.T, kind Kind, reconcile bool) (*Hub, *hubTmux, *httptest.Server, *hubClock) {
	t.Helper()
	f := newHubTmux()
	clk := &hubClock{t: time.Unix(1_700_000_000, 0).UTC()}
	h := newTestHub(f, clk)
	pane := paneFixture(t, "cursor-ready")
	if kind == KindCodex {
		pane = codexReadyPane()
	}
	f.set("rc-ing001", pane, managedEnv("id-ing", kind))
	if reconcile {
		h.reconcile()
	}
	srv := httptest.NewServer(h.handler())
	t.Cleanup(srv.Close)
	return h, f, srv, clk
}

// postHook posts one hook payload the way the preseeded script does.
func postHook(t *testing.T, srv *httptest.Server, slug, event, payload string) *http.Response {
	t.Helper()
	resp, err := http.Post(srv.URL+"/v1/ingest/cursor?slug="+slug+"&event="+event,
		"application/json", strings.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

// cursorWatcherOf returns a tracked session's cursor watcher.
func cursorWatcherOf(t *testing.T, h *Hub, slug string) *cursorWatcher {
	t.Helper()
	h.trackMu.Lock()
	defer h.trackMu.Unlock()
	tr, ok := h.tracked[slug]
	if !ok {
		t.Fatalf("slug %q not tracked", slug)
	}
	w, ok := tr.watcher.(*cursorWatcher)
	if !ok {
		t.Fatalf("watcher = %T, want *cursorWatcher", tr.watcher)
	}
	return w
}

// feedRowsOf returns every feed row in a tracked session's ring, oldest first.
func feedRowsOf(t *testing.T, h *Hub, slug string) []feedMessage {
	t.Helper()
	h.trackMu.Lock()
	tr, ok := h.tracked[slug]
	h.trackMu.Unlock()
	if !ok {
		t.Fatalf("slug %q not tracked", slug)
	}
	msgs, _ := tr.ring.since(0, maxMessagesLimit)
	return msgs
}

// The happy path: a hook event reaches the session's watcher and shows up in the feed on
// the next reconcile tick.
func TestHubIngestCursorReachesWatcherAndFeed(t *testing.T) {
	h, _, srv, _ := ingestHub(t, KindCursor, true)

	resp := postHook(t, srv, "ing001", "beforeSubmitPrompt",
		`{"session_id":"`+cursorTestSessionID+`","prompt":"build the thing"}`)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", resp.StatusCode)
	}
	h.reconcile()

	rows := feedRowsOf(t, h, "ing001")
	if len(rows) != 1 || rows[0].Role != feedRoleUser || rows[0].Text != "build the thing" {
		t.Fatalf("feed = %+v, want the user's prompt row", rows)
	}
	if got := hubActivityOf(t, h, "ing001"); got != ActivityWorking {
		t.Errorf("activity = %q, want working after a submitted prompt", got)
	}
}

// Oversize: the ingest cap is its OWN 256 KiB (not the 16 KiB every other POST shares),
// and a payload past it is a 413 with the event dropped — the feed just misses one event.
func TestHubIngestCursorOversizeIs413(t *testing.T) {
	h, _, srv, _ := ingestHub(t, KindCursor, true)

	// Just UNDER the cap: accepted, which is the whole reason for the larger cap —
	// afterShellExecution.output routinely exceeds the 16 KiB verb cap.
	big := `{"session_id":"` + cursorTestSessionID + `","command":"make","output":"` +
		strings.Repeat("x", 200<<10) + `"}`
	if resp := postHook(t, srv, "ing001", "afterShellExecution", big); resp.StatusCode != http.StatusAccepted {
		t.Fatalf("a 200 KiB payload = %d, want 202 (the 16 KiB verb cap must not apply here)", resp.StatusCode)
	}

	over := `{"session_id":"` + cursorTestSessionID + `","output":"` +
		strings.Repeat("x", hubIngestMaxBodyBytes+1024) + `"}`
	resp := postHook(t, srv, "ing001", "afterShellExecution", over)
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", resp.StatusCode)
	}

	h.reconcile()
	rows := feedRowsOf(t, h, "ing001")
	if len(rows) != 1 {
		t.Fatalf("feed = %d rows, want exactly the accepted one (the oversized event is dropped)", len(rows))
	}
	// The ring still applies its own per-field 8 KiB cap to what DID land.
	if n := len(rows[0].Tool.Detail); n > maxFeedMessageBytes+len(feedTruncMarker) {
		t.Errorf("tool detail = %d bytes, want the ring's 8 KiB cap applied", n)
	}
}

// Rejections, in the handler's pinned precedence order.
func TestHubIngestCursorRejections(t *testing.T) {
	_, _, srv, _ := ingestHub(t, KindCursor, true)

	cases := []struct {
		name, slug, event string
		want              int
	}{
		{"unknown slug", "nosuch", "stop", http.StatusNotFound},
		{"malformed slug", "not_a_slug!", "stop", http.StatusBadRequest},
		{"missing slug", "", "stop", http.StatusBadRequest},
		{"malformed event", "ing001", "st op!", http.StatusBadRequest},
		{"missing event", "ing001", "", http.StatusBadRequest},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if resp := postHook(t, srv, c.slug, c.event, `{}`); resp.StatusCode != c.want {
				t.Errorf("status = %d, want %d", resp.StatusCode, c.want)
			}
		})
	}

	// A tracked session of ANOTHER kind: 409 not_supported. This payload shape is cursor's,
	// and folding it into a codex session would be a category error.
	_, _, codexSrv, _ := ingestHub(t, KindCodex, true)
	resp := postHook(t, codexSrv, "ing001", "stop", `{}`)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("a non-cursor kind = %d, want 409", resp.StatusCode)
	}
	var env map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		t.Fatal(err)
	}
	if env["error"] != errNotSupported {
		t.Errorf("error = %q, want %q", env["error"], errNotSupported)
	}
}

// THE PRE-WATCHER QUEUE: hook events that land before reconcile builds the watcher (the
// kickoff prompt does, every time) are held and drained into the watcher the moment it is
// constructed — so they fold on that very tick.
func TestHubIngestCursorPreWatcherQueueDrains(t *testing.T) {
	// reconcile=false: the session exists in tmux but the hub has not ticked yet, exactly
	// the create→first-tick window.
	h, _, srv, _ := ingestHub(t, KindCursor, false)
	// Track the session WITHOUT building a watcher, the way the very first tick sees it:
	// reconcile once with the session in a blocking lifecycle state would also do, but the
	// simplest faithful setup is to tick, then drop the watcher back out.
	h.reconcile()
	h.trackMu.Lock()
	h.tracked["ing001"].watcher = nil
	h.trackMu.Unlock()

	for _, ev := range []struct{ event, payload string }{
		{"sessionStart", `{"session_id":"` + cursorTestSessionID + `"}`},
		{"beforeSubmitPrompt", `{"session_id":"` + cursorTestSessionID + `","prompt":"the kickoff prompt"}`},
	} {
		if resp := postHook(t, srv, "ing001", ev.event, ev.payload); resp.StatusCode != http.StatusAccepted {
			t.Fatalf("%s: status = %d, want 202", ev.event, resp.StatusCode)
		}
	}
	h.ingestMu.Lock()
	queued := len(h.preWatcher["ing001"].events)
	h.ingestMu.Unlock()
	if queued != 2 {
		t.Fatalf("queued = %d, want both events held for the not-yet-built watcher", queued)
	}

	// The next tick builds the watcher, drains the queue into it, and folds it.
	h.reconcile()
	h.ingestMu.Lock()
	_, stillQueued := h.preWatcher["ing001"]
	h.ingestMu.Unlock()
	if stillQueued {
		t.Error("the queue must be cleared once the watcher takes it")
	}
	rows := feedRowsOf(t, h, "ing001")
	if len(rows) != 2 || rows[1].Text != "the kickoff prompt" {
		t.Fatalf("feed = %+v, want the queued events folded on the watcher's first tick", rows)
	}
}

// hookedTmux wraps the fake runner so a test can act at a precise point INSIDE reconcile's
// unlocked section — the only way to land a request in the construct→commit window.
type hookedTmux struct {
	*hubTmux
	onRun func(args []string)
}

func (h *hookedTmux) Run(args ...string) Result {
	res := h.hubTmux.Run(args...)
	if h.onRun != nil {
		h.onRun(args) // called with the fake's lock RELEASED, so the hook may re-enter
	}
	return res
}

// THE CONSTRUCT→COMMIT WINDOW: ensureWatcher builds the watcher (and drains the queue)
// early in reconcile's unlocked section, but tr.watcher is not published until the trackMu
// re-acquire at the END of that section — several tmux execs later. A hook arriving in
// between sees tr.watcher == nil and queues, into a queue the construction-time drain has
// already passed. Without the post-commit drain nothing would ever take it: ensureWatcher
// no-ops from then on, so the event would sit until the TTL dropped it — and the event that
// most often lands right there is the kickoff prompt.
func TestHubIngestCursorPreWatcherDrainsAfterCommit(t *testing.T) {
	f := newHubTmux()
	clk := &hubClock{t: time.Unix(1_700_000_000, 0).UTC()}
	hooked := &hookedTmux{hubTmux: f}
	h := newTestHub(hooked, clk)
	f.set("rc-win001", paneFixture(t, "cursor-ready"), managedEnv("id-win", KindCursor))
	srv := httptest.NewServer(h.handler())
	defer srv.Close()

	var (
		captures  int
		fired     bool
		inWindow  bool
		postponed = `{"session_id":"` + cursorTestSessionID + `","prompt":"the kickoff prompt"}`
	)
	hooked.onRun = func(args []string) {
		// The FIRST capture-pane of a tick is the session-listing one (before ensureWatcher
		// runs); the second is the stability tracker's, by which point the watcher exists but
		// is not yet published. Fire once, there.
		if args[0] != "capture-pane" {
			return
		}
		captures++
		if captures != 2 || fired {
			return
		}
		fired = true
		// Self-verification: the window is only interesting while tr.watcher is still nil.
		h.trackMu.Lock()
		inWindow = h.tracked["win001"] != nil && h.tracked["win001"].watcher == nil
		h.trackMu.Unlock()
		postHook(t, srv, "win001", "beforeSubmitPrompt", postponed)
	}

	h.reconcile()
	if !fired || !inWindow {
		t.Fatalf("test premise: the hook must fire inside the construct→commit window (fired=%v inWindow=%v)", fired, inWindow)
	}

	// The post-commit drain took it: nothing is left stranded in the queue…
	h.ingestMu.Lock()
	_, stranded := h.preWatcher["win001"]
	h.ingestMu.Unlock()
	if stranded {
		t.Fatal("an event queued during the construct→commit window was left stranded")
	}
	// …and it folds on the next tick rather than waiting out the 60s TTL.
	h.reconcile()
	rows := feedRowsOf(t, h, "win001")
	if len(rows) != 1 || rows[0].Text != "the kickoff prompt" {
		t.Fatalf("feed = %+v, want the window's event folded", rows)
	}
}

// Once a watcher EXISTS the queue is never used again: a refused push (a closed watcher —
// the session is going away) is DROPPED, not queued. Queueing there would waste the slug's
// budget with nothing to drain it, and a queued event would be handed to the NEXT watcher
// built at the same slug — folding a dead incarnation's turn into a recreated session.
func TestHubIngestCursorRefusedPushIsDroppedNotQueued(t *testing.T) {
	h, _, srv, _ := ingestHub(t, KindCursor, true)

	w := cursorWatcherOf(t, h, "ing001")
	w.close() // the session is on its way out; the watcher refuses everything now

	if resp := postHook(t, srv, "ing001", "beforeSubmitPrompt",
		`{"session_id":"`+cursorTestSessionID+`","prompt":"into the void"}`); resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 (the hook script cannot act on anything else)", resp.StatusCode)
	}
	h.ingestMu.Lock()
	_, queued := h.preWatcher["ing001"]
	h.ingestMu.Unlock()
	if queued {
		t.Error("an event refused by an existing watcher must be dropped, never queued")
	}
}

// The queue is bounded (a session whose watcher never appears must not grow one) and
// expires: after the TTL the whole queue is dropped, not trimmed.
func TestHubIngestCursorPreWatcherBoundsAndTTL(t *testing.T) {
	h, f, srv, clk := ingestHub(t, KindCursor, false)
	h.reconcile()
	h.trackMu.Lock()
	h.tracked["ing001"].watcher = nil
	h.trackMu.Unlock()

	for i := 0; i < maxPreWatcherEvents+10; i++ {
		postHook(t, srv, "ing001", "stop", `{"status":"completed"}`)
	}
	h.ingestMu.Lock()
	n := len(h.preWatcher["ing001"].events)
	h.ingestMu.Unlock()
	if n != maxPreWatcherEvents {
		t.Errorf("queued = %d, want the count bound %d", n, maxPreWatcherEvents)
	}

	// TTL: the slug still has no watcher after 60s → the whole queue is dropped.
	h.trackMu.Lock()
	h.tracked["ing001"].watcher = nil
	h.trackMu.Unlock()
	clk.advance(preWatcherTTL + time.Second)
	h.prunePreWatcher(clk.now(), map[string]bool{"ing001": true})
	h.ingestMu.Lock()
	_, present := h.preWatcher["ing001"]
	h.ingestMu.Unlock()
	if present {
		t.Error("a queue past the TTL must be dropped wholesale")
	}

	// A queue for a slug that has disappeared is dropped on the next tick regardless of age.
	postHook(t, srv, "ing001", "stop", `{"status":"completed"}`)
	f.remove("rc-ing001")
	h.reconcile()
	h.ingestMu.Lock()
	_, present = h.preWatcher["ing001"]
	h.ingestMu.Unlock()
	if present {
		t.Error("a queue for a vanished slug must be dropped")
	}
}

// The pin reaches the tmux env: reconcile back-writes SHED_RC_AGENT_SESSION from the hook
// stream (drainConfirmedAgentID), so a hub restart re-correlates exactly — and a re-pin
// (the operator switched chats in the TUI) stamps the new id and announces the switch.
func TestHubIngestCursorBackWritesAgentSession(t *testing.T) {
	h, f, srv, _ := ingestHub(t, KindCursor, true)

	postHook(t, srv, "ing001", "sessionStart", `{"session_id":"`+cursorTestSessionID+`"}`)
	h.reconcile()
	if calls := f.setEnvCalls(); len(calls) != 1 || calls[0] != envAgentSession+"="+cursorTestSessionID {
		t.Fatalf("set-environment calls = %v, want one %s back-write", calls, envAgentSession)
	}

	// The same id again is not re-stamped (one back-write per pin).
	postHook(t, srv, "ing001", "stop", `{"session_id":"`+cursorTestSessionID+`","status":"completed"}`)
	h.reconcile()
	if calls := f.setEnvCalls(); len(calls) != 1 {
		t.Errorf("set-environment calls = %v, want the repeated pin not re-stamped", calls)
	}

	// A different chat: re-pin, re-stamp, and a status row so the feed shows the switch.
	const other = "9129668a-885b-48ef-b61b-d80f981d4d68"
	postHook(t, srv, "ing001", "beforeSubmitPrompt", `{"session_id":"`+other+`","prompt":"new chat"}`)
	h.reconcile()
	calls := f.setEnvCalls()
	if len(calls) != 2 || calls[1] != envAgentSession+"="+other {
		t.Fatalf("set-environment calls = %v, want the re-pin stamped", calls)
	}
	var switched bool
	for _, m := range feedRowsOf(t, h, "ing001") {
		if m.Type == feedTypeStatus && strings.Contains(m.Text, "switched to another chat") {
			switched = true
		}
	}
	if !switched {
		t.Error("a chat switch must be announced in the feed")
	}
}

// The ingest route is on the hub's mux with the right method only — a GET is a 405 from
// the mux, never a silent success.
func TestHubIngestCursorMethodGate(t *testing.T) {
	_, _, srv, _ := ingestHub(t, KindCursor, true)
	resp, err := http.Get(srv.URL + "/v1/ingest/cursor?slug=ing001&event=stop")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("GET status = %d, want 405", resp.StatusCode)
	}
}
