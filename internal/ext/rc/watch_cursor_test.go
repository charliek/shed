package rc

import (
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// Tests for the push-fed cursor watcher (plan 008 §3.5). The payload literals here are
// TRIMMED COPIES of the live spike capture
// (~/.claude/plans/shed/008-observatory/spikes/cursor-hooks/runB-tui.jsonl, cursor-agent
// 2026.08.11-e8db854): same field names, same nesting, same value shapes, with the noise
// fields (workspace_roots, user_email, cursor_version, token counts) dropped. If cursor
// renames a field, these stop matching the fold and this file is where it shows.

// cursorTestSessionID is the spike capture's own conversation id (a UUID — the shape
// validCursorSessionID pins).
const cursorTestSessionID = "4113a71f-0a42-4a6d-89b9-483e44b74103"

// hookEv builds one hook delivery.
func hookEv(event, payload string) cursorHookEvent {
	return cursorHookEvent{event: event, payload: []byte(payload)}
}

// sid renders the session_id field every real payload carries.
func sid(id string) string { return `"session_id":"` + id + `"` }

// foldEvents applies a sequence to a fresh fold and returns it (no watcher, no clock).
func foldEvents(t *testing.T, evs ...cursorHookEvent) *cursorFold {
	t.Helper()
	f := newCursorFold("")
	for _, ev := range evs {
		f.applyEvent(ev)
	}
	return f
}

// A whole turn, event by event, as the spike recorded it: prompt → tool → output → edit →
// response → stop. Pins the feed row for each event AND the activity at each boundary.
func TestCursorFoldTurnMapping(t *testing.T) {
	f := newCursorFold("")

	if got := f.activity(); got != ActivityUnknown {
		t.Errorf("a fold with no events = %q, want unknown", got)
	}

	// sessionStart: the TUI is up and parked at its composer.
	f.applyEvent(hookEv("sessionStart", `{`+sid(cursorTestSessionID)+`,"model":"cursor-grok-4.5-high"}`))
	if got := f.activity(); got != ActivityNeedsInput {
		t.Errorf("after sessionStart activity = %q, want needs_input", got)
	}

	// beforeSubmitPrompt: the turn's start boundary + the user's row.
	f.applyEvent(hookEv("beforeSubmitPrompt", `{`+sid(cursorTestSessionID)+`,"prompt":"Run the build","attachments":[]}`))
	if got := f.activity(); got != ActivityWorking {
		t.Errorf("after beforeSubmitPrompt activity = %q, want working", got)
	}

	// preToolUse: a tool_use row whose detail is the SHELL COMMAND out of tool_input.
	f.applyEvent(hookEv("preToolUse", `{`+sid(cursorTestSessionID)+`,"tool_name":"Shell",`+
		`"tool_input":{"command":"echo hello-tui","cwd":"","timeout":30000},"tool_use_id":"e52e7aed"}`))
	// afterShellExecution: the OUTPUT, which exists in no other channel.
	f.applyEvent(hookEv("afterShellExecution", `{`+sid(cursorTestSessionID)+`,"command":"echo hello-tui","output":"hello-tui\n","duration":39300.384}`))
	// afterFileEdit: path + edit count (the diffs themselves are deliberately dropped).
	f.applyEvent(hookEv("afterFileEdit", `{`+sid(cursorTestSessionID)+`,"file_path":"/home/shed/proj/notes.txt",`+
		`"edits":[{"old_string":"","new_string":"hi\n"},{"old_string":"a","new_string":"b"}]}`))
	// afterAgentResponse: the assistant row + last_message.
	f.applyEvent(hookEv("afterAgentResponse", `{`+sid(cursorTestSessionID)+`,"text":"Done — the build passed."}`))
	if got := f.activity(); got != ActivityWorking {
		t.Errorf("a message does not end a turn; activity = %q, want working", got)
	}
	if got := f.lastMessage(); got != "Done — the build passed." {
		t.Errorf("lastMessage = %q", got)
	}

	// stop: the turn's end boundary — settled, so snapshot trusts it indefinitely.
	f.applyEvent(hookEv("stop", `{`+sid(cursorTestSessionID)+`,"status":"completed","loop_count":0}`))
	if got := f.activity(); got != ActivityNeedsInput {
		t.Errorf("after stop activity = %q, want needs_input", got)
	}
	if !f.settled() {
		t.Error("after stop the verdict must be settled")
	}

	msgs := f.drainMessages()
	type want struct{ role, typ, contains string }
	wants := []want{
		{feedRoleSystem, feedTypeStatus, "cursor session started"},
		{feedRoleUser, feedTypeText, "Run the build"},
		{feedRoleTool, feedTypeToolUse, "echo hello-tui"},
		{feedRoleTool, feedTypeToolResult, "hello-tui"},
		{feedRoleTool, feedTypeToolResult, "notes.txt (2 edits)"},
		{feedRoleAssistant, feedTypeText, "Done — the build passed."},
	}
	if len(msgs) != len(wants) {
		t.Fatalf("got %d feed rows, want %d: %+v", len(msgs), len(wants), msgs)
	}
	for i, w := range wants {
		m := msgs[i]
		body := m.Text
		if m.Tool != nil {
			body = m.Tool.Name + " " + m.Tool.Detail
		}
		if m.Role != w.role || m.Type != w.typ || !strings.Contains(body, w.contains) {
			t.Errorf("row %d = {%s %s %q}, want {%s %s ~%q}", i, m.Role, m.Type, body, w.role, w.typ, w.contains)
		}
	}
	// The tool rows name the tool, so a client can render them without parsing the detail.
	if msgs[2].Tool.Name != "Shell" || msgs[4].Tool.Name != "Edit" {
		t.Errorf("tool names = %q/%q, want Shell/Edit", msgs[2].Tool.Name, msgs[4].Tool.Name)
	}
	// stop emits NO row: it is a boundary, and a row per turn end would be pure noise.
	if len(f.drainMessages()) != 0 {
		t.Error("stop must not emit a feed row")
	}
}

// A failed tool: a status row, and the open-tool count released (a failure ends the call
// exactly as an output does).
func TestCursorFoldToolFailureAndOpenToolTracking(t *testing.T) {
	f := foldEvents(t,
		hookEv("beforeSubmitPrompt", `{`+sid(cursorTestSessionID)+`,"prompt":"read notes"}`),
		hookEv("preToolUse", `{`+sid(cursorTestSessionID)+`,"tool_name":"Read","tool_input":{"file_path":"/home/shed/notes.txt"}}`),
	)
	if f.openTools != 1 || f.activity() != ActivityWorking {
		t.Fatalf("an open tool call must read working (openTools=%d, activity=%q)", f.openTools, f.activity())
	}
	f.applyEvent(hookEv("postToolUseFailure", `{`+sid(cursorTestSessionID)+`,"tool_name":"Read",`+
		`"error_message":"File not found: /home/shed/notes.txt","failure_type":"error"}`))
	if f.openTools != 0 {
		t.Errorf("openTools = %d after a failure, want 0", f.openTools)
	}
	msgs := f.drainMessages()
	last := msgs[len(msgs)-1]
	if last.Role != feedRoleSystem || last.Type != feedTypeStatus ||
		!strings.Contains(last.Text, "tool Read failed") || !strings.Contains(last.Text, "File not found") {
		t.Errorf("failure row = %+v, want a system status naming the tool and the error", last)
	}

	// A tool still open when `stop` lands does not hold the session at working: stop is the
	// authoritative boundary and clears the count.
	f.applyEvent(hookEv("preToolUse", `{`+sid(cursorTestSessionID)+`,"tool_name":"Shell","tool_input":{"command":"sleep 1"}}`))
	f.applyEvent(hookEv("stop", `{`+sid(cursorTestSessionID)+`,"status":"completed"}`))
	if got := f.activity(); got != ActivityNeedsInput {
		t.Errorf("stop with a tool still open = %q, want needs_input", got)
	}
}

// THE OPEN-CALL COUNTER, pinned to the hook stream's real pairing: every preToolUse is
// matched by exactly one postToolUse or postToolUseFailure (the spike's turn: 5 ↔ 3+2),
// while afterShellExecution/afterFileEdit ride ALONGSIDE the pair rather than terminating
// it. Getting this wrong in either direction misreports the session: double-decrementing
// forgets a genuinely open call, never decrementing leaves a Read/Grep/Glob-class tool
// "open" until the next turn boundary.
func TestCursorFoldOpenCallCounterPairing(t *testing.T) {
	f := newCursorFold("")

	// A shell call: preToolUse, then its output event, then its postToolUse. Only the last
	// closes it.
	f.applyEvent(hookEv("preToolUse", `{`+sid(cursorTestSessionID)+`,"tool_name":"Shell","tool_input":{"command":"make"}}`))
	if f.openTools != 1 {
		t.Fatalf("openTools = %d after preToolUse, want 1", f.openTools)
	}
	f.applyEvent(hookEv("afterShellExecution", `{`+sid(cursorTestSessionID)+`,"command":"make","output":"ok\n"}`))
	if f.openTools != 1 {
		t.Errorf("openTools = %d: afterShellExecution rides alongside the call, it does not end it", f.openTools)
	}
	f.applyEvent(hookEv("postToolUse", `{`+sid(cursorTestSessionID)+`,"tool_name":"Shell","tool_output":"ok"}`))
	if f.openTools != 0 {
		t.Errorf("openTools = %d after postToolUse, want 0", f.openTools)
	}

	// A tool family with NO after* event at all (Read/Grep/Glob) — the case that used to
	// leak, because only shell/edit/failure decremented.
	f.applyEvent(hookEv("preToolUse", `{`+sid(cursorTestSessionID)+`,"tool_name":"Read","tool_input":{"file_path":"/x"}}`))
	f.applyEvent(hookEv("postToolUse", `{`+sid(cursorTestSessionID)+`,"tool_name":"Read","tool_output":"…"}`))
	if f.openTools != 0 {
		t.Errorf("openTools = %d: a Read call must close on its postToolUse", f.openTools)
	}
	if got := f.activity(); got != ActivityNeedsInput && got != ActivityWorking {
		t.Errorf("unexpected activity %q", got)
	}
	// postToolUse emits no feed row of its own (its output would duplicate the after*
	// events for the families that have them).
	for _, m := range f.drainMessages() {
		if m.Type == feedTypeToolResult && m.Tool != nil && m.Tool.Name == "Read" {
			t.Errorf("postToolUse must not emit a feed row: %+v", m)
		}
	}
	// It IS activity-relevant evidence, though: a fold that has only seen a tool close is
	// confirmed, not unknown.
	if !newCursorFoldAfter(t, hookEv("postToolUse", `{`+sid(cursorTestSessionID)+`,"tool_name":"Read"}`)).confirmed {
		t.Error("postToolUse must confirm the session is alive")
	}
}

// newCursorFoldAfter folds one event into a fresh fold and returns it.
func newCursorFoldAfter(t *testing.T, ev cursorHookEvent) *cursorFold {
	t.Helper()
	return foldEvents(t, ev)
}

// `stop` carries the turn's OUTCOME, and a turn the operator interrupted or that errored
// must not read on a phone like one that finished. A completed turn stays silent (a row per
// normal turn end is noise); anything else says so.
func TestCursorFoldStopStatusRow(t *testing.T) {
	cases := []struct {
		status  string
		wantRow string // "" = no row
	}{
		{"completed", ""},
		{"aborted", "turn ended: aborted"},
		{"error", "turn ended: error"},
		{"", ""}, // an absent status is not an outcome to report
	}
	for _, c := range cases {
		t.Run(c.status, func(t *testing.T) {
			f := foldEvents(t, hookEv("stop", `{`+sid(cursorTestSessionID)+`,"status":"`+c.status+`"}`))
			if got := f.activity(); got != ActivityNeedsInput {
				t.Errorf("activity = %q, want needs_input — every stop settles the turn", got)
			}
			msgs := f.drainMessages()
			if c.wantRow == "" {
				if len(msgs) != 0 {
					t.Errorf("rows = %+v, want none", msgs)
				}
				return
			}
			if len(msgs) != 1 || msgs[0].Role != feedRoleSystem || msgs[0].Text != c.wantRow {
				t.Errorf("rows = %+v, want one system status %q", msgs, c.wantRow)
			}
		})
	}
}

// Tolerant parsing (the activityFold contract, applied to hook events): an unwired/unknown
// event, an unparseable body, and an event with nothing the fold reads are all inert.
func TestCursorFoldToleratesUnknownAndMalformed(t *testing.T) {
	f := newCursorFold("")
	cases := []cursorHookEvent{
		hookEv("afterAgentThought", `{`+sid(cursorTestSessionID)+`,"text":"thinking out loud"}`), // not wired
		hookEv("someFutureEvent", `{`+sid(cursorTestSessionID)+`}`),                              // unknown
		hookEv("beforeSubmitPrompt", `{not json`),                                                // malformed
		hookEv("beforeSubmitPrompt", `{`+sid(cursorTestSessionID)+`,"prompt":"   "}`),            // empty prompt
		hookEv("afterAgentResponse", `{`+sid(cursorTestSessionID)+`,"text":""}`),                 // empty text
	}
	for _, ev := range cases {
		f.applyEvent(ev)
	}
	if got := f.activity(); got != ActivityUnknown {
		t.Errorf("activity = %q, want unknown — none of these events confirms a session", got)
	}
	if msgs := f.drainMessages(); len(msgs) != 0 {
		t.Errorf("no feed rows expected, got %+v", msgs)
	}
	// Pinning runs BEFORE foldEvent (see applyEvent), so it is independent of whether the
	// event tells the fold anything: every case above except the unparseable-JSON one carries
	// a well-formed session_id, so notePin pins it regardless of the event being
	// unwired/unknown/empty. Only a payload that fails to unmarshal (no session_id reaches
	// notePin at all) or an id that fails validCursorSessionID's grammar (see
	// TestCursorFoldPinningAndRepin) leaves the fold unpinned.
	if f.pinnedID != cursorTestSessionID {
		t.Errorf("pinnedID = %q, want %q — a tolerated event with a valid session_id still pins", f.pinnedID, cursorTestSessionID)
	}
}

// PINNING: the first id-carrying event pins (sessionStart is NOT required — it is absent
// on --resume), a hostile/malformed id is refused, and a different id re-pins with a status
// row while dropping the previous chat's carry-over state.
func TestCursorFoldPinningAndRepin(t *testing.T) {
	f := newCursorFold("")
	// No sessionStart: the pin comes off the prompt event, exactly as a --resume session
	// would deliver it.
	f.applyEvent(hookEv("beforeSubmitPrompt", `{`+sid(cursorTestSessionID)+`,"prompt":"hello"}`))
	if f.pinnedID != cursorTestSessionID {
		t.Fatalf("pinnedID = %q, want the first event's id", f.pinnedID)
	}
	if got := f.drainNewPin(); got != cursorTestSessionID {
		t.Fatalf("drainNewPin = %q, want the id to be surfaced for the back-write", got)
	}
	if got := f.drainNewPin(); got != "" {
		t.Errorf("drainNewPin = %q on the second call, want it cleared", got)
	}

	// A hostile id (a tmux env var and a loopback POST are both operator-writable) is
	// refused outright — it never becomes the pin and never reaches a back-write.
	for _, bad := range []string{"../../etc/passwd", "not-a-uuid", strings.Repeat("a", 300)} {
		f.applyEvent(hookEv("afterAgentResponse", `{"session_id":"`+bad+`","text":"x"}`))
		if f.pinnedID != cursorTestSessionID {
			t.Fatalf("a malformed session id (%q) must not re-pin (pinnedID=%q)", bad, f.pinnedID)
		}
		if got := f.drainNewPin(); got != "" {
			t.Fatalf("a malformed session id (%q) must never be surfaced for back-write", bad)
		}
	}

	// A tool is open and a message is remembered — both belong to the OLD chat.
	f.applyEvent(hookEv("preToolUse", `{`+sid(cursorTestSessionID)+`,"tool_name":"Shell","tool_input":{"command":"sleep 30"}}`))
	f.applyEvent(hookEv("afterAgentResponse", `{`+sid(cursorTestSessionID)+`,"text":"old chat message"}`))
	f.drainMessages()

	// The operator switched chats in the TUI: a different valid id re-pins.
	const other = "9129668a-885b-48ef-b61b-d80f981d4d68"
	f.applyEvent(hookEv("beforeSubmitPrompt", `{`+sid(other)+`,"prompt":"new chat"}`))
	if f.pinnedID != other {
		t.Fatalf("pinnedID = %q, want the re-pin to %q", f.pinnedID, other)
	}
	if got := f.drainNewPin(); got != other {
		t.Errorf("drainNewPin = %q, want the new id for the back-write", got)
	}
	if f.openTools != 0 || f.lastMsg == "old chat message" {
		t.Errorf("the previous chat's carry-over state survived the switch (openTools=%d, lastMsg=%q)", f.openTools, f.lastMsg)
	}
	msgs := f.drainMessages()
	if len(msgs) == 0 || !strings.Contains(msgs[0].Text, "switched to another chat") {
		t.Errorf("the switch must be announced with a status row, got %+v", msgs)
	}
}

// A repin whose triggering event does NOT itself put the fold into cursorStateWorking (e.g.
// postToolUse, which only closes a tool call) must not leave activity() reading the OLD
// chat's working state forever — switching away from a mid-turn chat to a parked one should
// read unknown (not working) until the new chat's own event says otherwise, letting pane
// stability drive the gap.
func TestCursorFoldRepinResetsState(t *testing.T) {
	f := newCursorFold("")
	// Pin to the first chat and drive it mid-turn (a submitted prompt with an open tool call).
	f.applyEvent(hookEv("beforeSubmitPrompt", `{`+sid(cursorTestSessionID)+`,"prompt":"go"}`))
	f.applyEvent(hookEv("preToolUse", `{`+sid(cursorTestSessionID)+`,"tool_name":"Shell","tool_input":{"command":"sleep 30"}}`))
	if got := f.activity(); got != ActivityWorking {
		t.Fatalf("setup: activity = %q, want working mid-turn", got)
	}

	// The operator switches to a parked chat via an event that does NOT set working —
	// postToolUse only closes a tool call, exactly the case the fix targets.
	const other = "9129668a-885b-48ef-b61b-d80f981d4d68"
	f.applyEvent(hookEv("postToolUse", `{`+sid(other)+`,"tool_name":"Shell"}`))
	if f.pinnedID != other {
		t.Fatalf("pinnedID = %q, want the repin to %q", f.pinnedID, other)
	}
	if got := f.activity(); got == ActivityWorking {
		t.Errorf("activity = %q after a repin via a non-working event, want NOT stuck working", got)
	}
}

// The watcher's freshness rule (the fileWatcher rule, deliberately — see the cursorWatcher
// doc): a settled verdict is authoritative indefinitely, a working one for the grace, and
// a closed watcher has no authority at all.
func TestCursorWatcherSnapshotFreshness(t *testing.T) {
	clk := &hubClock{t: time.Unix(1_700_000_000, 0).UTC()}
	w := newCursorWatcher("", nil)

	// Nothing folded yet → no verdict ("" before the first refresh, unknown after), never
	// fresh: the merge must fall through to pane stability.
	if act, _, fresh, expired := w.snapshot(clk.now()); (act != "" && act != ActivityUnknown) || fresh || expired {
		t.Errorf("empty snapshot = (%q, fresh=%v, expired=%v), want no verdict and no authority", act, fresh, expired)
	}

	w.pushHookEvent(hookEv("beforeSubmitPrompt", `{`+sid(cursorTestSessionID)+`,"prompt":"go"}`))
	w.refresh(clk.now())
	if act, _, fresh, _ := w.snapshot(clk.now()); act != ActivityWorking || !fresh {
		t.Fatalf("working snapshot = (%q, fresh=%v), want working/true", act, fresh)
	}
	// Past the 30s window but inside the 120s working grace: still fresh.
	clk.advance(watcherFreshWindow + time.Second)
	if _, _, fresh, expired := w.snapshot(clk.now()); !fresh || expired {
		t.Errorf("inside the working grace: fresh=%v expired=%v, want true/false", fresh, expired)
	}
	// Past the grace: demoted to conditional (expiredWorking), exactly like a quiet JSONL.
	clk.advance(watcherWorkingGrace)
	if _, _, fresh, expired := w.snapshot(clk.now()); fresh || !expired {
		t.Errorf("past the working grace: fresh=%v expired=%v, want false/true", fresh, expired)
	}

	// A settled verdict is trusted indefinitely — a waiting cursor emits no hooks at all,
	// so quiet is exactly what "waiting for you" looks like.
	w.pushHookEvent(hookEv("stop", `{`+sid(cursorTestSessionID)+`,"status":"completed"}`))
	w.refresh(clk.now())
	clk.advance(24 * time.Hour)
	act, _, fresh, expired := w.snapshot(clk.now())
	if act != ActivityNeedsInput || !fresh || expired {
		t.Errorf("settled snapshot after a day = (%q, fresh=%v, expired=%v), want needs_input/true/false", act, fresh, expired)
	}

	// close() revokes authority and stops folding (a discarded watcher must not keep
	// mutating state a stale pointer could read).
	w.close()
	if _, _, fresh, expired := w.snapshot(clk.now()); fresh || expired {
		t.Error("a closed watcher must report no authority")
	}
	if w.pushHookEvent(hookEv("stop", `{}`)) {
		t.Error("a closed watcher must refuse pushes")
	}
}

// The watcher does NOT implement the approval interfaces: cursor emits no approval hook,
// so its needs_approval comes from the pane anchor and nothing here may claim otherwise.
func TestCursorWatcherIsNotAnApprovalSurface(t *testing.T) {
	var w sessionWatcher = newCursorWatcher("", nil)
	if _, ok := w.(approvalBlocker); ok {
		t.Error("cursorWatcher must not implement approvalBlocker (no approval hook exists)")
	}
	if _, ok := w.(approvalPublisher); ok {
		t.Error("cursorWatcher must not implement approvalPublisher (no approval hook exists)")
	}
}

// The pin reaches reconcile's back-write through drainConfirmedAgentID — and a pin that
// merely REPEATS what was already back-written is not re-surfaced.
func TestCursorWatcherDrainConfirmedAgentID(t *testing.T) {
	clk := &hubClock{t: time.Unix(1_700_000_000, 0).UTC()}
	w := newCursorWatcher("", nil)
	w.pushHookEvent(hookEv("sessionStart", `{`+sid(cursorTestSessionID)+`}`))
	w.refresh(clk.now())
	if got := w.drainConfirmedAgentID(); got != cursorTestSessionID {
		t.Fatalf("drainConfirmedAgentID = %q, want %q", got, cursorTestSessionID)
	}
	if got := w.drainConfirmedAgentID(); got != "" {
		t.Errorf("drainConfirmedAgentID = %q on the second call, want cleared", got)
	}

	// A watcher rebuilt with the SAME prior pin (a hub restart) must not re-stamp it.
	w2 := newCursorWatcher(cursorTestSessionID, nil)
	w2.pushHookEvent(hookEv("stop", `{`+sid(cursorTestSessionID)+`,"status":"completed"}`))
	w2.refresh(clk.now())
	if got := w2.drainConfirmedAgentID(); got != "" {
		t.Errorf("a repeated pin must not be re-surfaced, got %q", got)
	}

	// A malformed prior pin is discarded at construction rather than trusted.
	w3 := newCursorWatcher("../evil", nil)
	if w3.priorID != "" || w3.fold.pinnedID != "" {
		t.Errorf("a malformed prior pin must be discarded (priorID=%q, pinned=%q)", w3.priorID, w3.fold.pinnedID)
	}
}

// The inbox is bounded by COUNT and by BYTES, and an overflow records a gap that releases
// the open-tool state (a swallowed after* event must not pin the verdict at working).
func TestCursorWatcherInboxBounds(t *testing.T) {
	clk := &hubClock{t: time.Unix(1_700_000_000, 0).UTC()}

	// Count bound.
	w := newCursorWatcher("", nil)
	for i := 0; i < maxCursorInboxItems; i++ {
		if !w.pushHookEvent(hookEv("stop", `{"status":"completed"}`)) {
			t.Fatalf("push %d was refused before the count bound", i)
		}
	}
	if w.pushHookEvent(hookEv("stop", `{"status":"completed"}`)) {
		t.Error("a push past maxCursorInboxItems must be refused")
	}
	if !w.gapped {
		t.Error("an overflow must record a gap")
	}

	// Byte bound: one payload over the byte budget is refused even though the count is 1.
	w2 := newCursorWatcher("", nil)
	big := fmt.Sprintf(`{"output":%q}`, strings.Repeat("x", maxCursorInboxBytes+1))
	if w2.pushHookEvent(hookEv("afterShellExecution", big)) {
		t.Error("a payload over the byte budget must be refused")
	}

	// The gap releases open-tool state on the next refresh.
	w3 := newCursorWatcher("", nil)
	w3.pushHookEvent(hookEv("preToolUse", `{`+sid(cursorTestSessionID)+`,"tool_name":"Shell","tool_input":{"command":"sleep 900"}}`))
	w3.refresh(clk.now())
	if w3.fold.openTools != 1 {
		t.Fatalf("premise: the tool call should be open, got %d", w3.fold.openTools)
	}
	w3.mu.Lock()
	w3.gapped = true
	w3.mu.Unlock()
	w3.refresh(clk.now())
	if w3.fold.openTools != 0 {
		t.Errorf("a gap must drop the open-tool count, got %d", w3.fold.openTools)
	}
}

// THE CONCURRENCY PIN (run under -race): the ingest handler's goroutine pushes while the
// reconcile goroutine refreshes/snapshots/drains. Two writers, one mutex, no goroutine of
// the watcher's own — this is the test that proves the discipline holds.
func TestCursorWatcherConcurrentPushAndRefresh(t *testing.T) {
	clk := &hubClock{t: time.Unix(1_700_000_000, 0).UTC()}
	w := newCursorWatcher("", nil)

	const pushers, perPusher = 4, 200
	var wg sync.WaitGroup
	for p := 0; p < pushers; p++ {
		wg.Add(1)
		go func(p int) {
			defer wg.Done()
			for i := 0; i < perPusher; i++ {
				w.pushHookEvent(hookEv("preToolUse", fmt.Sprintf(
					`{%s,"tool_name":"Shell","tool_input":{"command":"echo %d-%d"}}`, sid(cursorTestSessionID), p, i)))
			}
		}(p)
	}
	// The reconcile side, running concurrently with every push.
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 500; i++ {
			w.refresh(clk.now())
			w.snapshot(clk.now())
			w.drainPending()
			w.drainConfirmedAgentID()
			w.hadEvent()
		}
	}()
	// The INPUT-HANDLER side: inputAccepted refreshes the watcher too, on an HTTP
	// goroutine. refresh is the fold's only mutator but NOT a single-goroutine one — the
	// mutex is what makes that safe, and this arm is what proves it.
	gate := make(chan struct{})
	go func() {
		defer close(gate)
		h := newTestHub(newHubTmux(), clk)
		for i := 0; i < 500; i++ {
			h.inputAccepted(w, ActivityIdle, KindCursor, "", "")
		}
	}()
	wg.Wait()
	<-done
	<-gate

	w.refresh(clk.now())
	w.drainPending()
	if !w.hadEvent() {
		t.Error("events were pushed and folded; hadEvent must be true")
	}
}

// A cursor session is watchable and its watcher is built by ensureWatcher with no
// correlation input — the construction path the plan pins.
func TestCursorWatchableAndEnsureWatcher(t *testing.T) {
	if !watchableKind(KindCursor) {
		t.Fatal("cursor must be a watchable kind")
	}
	f := newHubTmux()
	clk := &hubClock{t: time.Unix(1_700_000_000, 0).UTC()}
	h := newTestHub(f, clk)
	f.set("rc-cur001", paneFixture(t, "cursor-ready"), managedEnv("id-cur", KindCursor))
	h.reconcile()

	h.trackMu.Lock()
	tr := h.tracked["cur001"]
	h.trackMu.Unlock()
	if tr == nil || tr.watcher == nil {
		t.Fatal("reconcile must build a cursor watcher on the first eligible tick")
	}
	if _, ok := tr.watcher.(*cursorWatcher); !ok {
		t.Fatalf("watcher = %T, want *cursorWatcher", tr.watcher)
	}
}
