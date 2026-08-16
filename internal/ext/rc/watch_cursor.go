package rc

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sync"
	"time"
)

// cursorWatcher is the PUSH-FED sessionWatcher for a cursor-agent session (plan 008
// §3.5). It is the third transport shape in the watcher stack and the only one that
// initiates no I/O at all:
//
//	codex/claude  — pull: tail an append-only JSONL file (fileWatcher).
//	opencode      — pull: subscribe to the agent's embedded HTTP+SSE server.
//	cursor        — PUSH: the agent's own hook scripts POST each event to the hub's
//	                loopback ingest route, which hands it to this watcher.
//
// Why push: cursor exposes no server and its transcript JSONL is thin (user/assistant
// lines only — no tool results, no ids, no timestamps, and it lags mid-turn). Hooks are
// the only channel carrying tool output and turn boundaries, and they only exist because
// the hub preseeds them (PreseedCursorHooks, preseed_cursor.go).
//
// CONCURRENCY MODEL (exercised under -race by TestCursorWatcherConcurrentPushAndRefresh):
// TWO writers, one mutex, no goroutine of its own.
//
//   - pushHookEvent runs on the HTTP handler goroutine (the ingest route) and appends to
//     the bounded inbox under w.mu. It never folds.
//   - refresh(now) is the ONLY method that mutates the fold, and it does so under the SAME
//     mutex: it drains the inbox → fold.applyEvent and reads the verdict out. It is called
//     from TWO goroutines, not one — reconcile on its tick, and the input handler's
//     acceptance re-check (hub.go inputAccepted refreshes before it reads the snapshot), on
//     an HTTP goroutine. That is safe precisely because the mutex, not the goroutine
//     identity, is what serializes it; do not "simplify" this to a single-goroutine
//     assumption.
//
// So the fold — which, like every activityFold, is not concurrency-safe — still has
// exactly one writer at a time, and the watcher needs no background goroutine to own I/O
// (there is none to own). close() therefore has nothing to release beyond flipping the
// terminal flag.
//
// FRESHNESS: this reuses the fileWatcher rule (settled trusted indefinitely, non-settled
// fresh for watcherFreshWindow, working for watcherWorkingGrace) rather than the opencode
// watcher's transport-health rule. That is deliberate: the opencode rule exists because a
// wedged SSE socket can look identical to a quiet one, so its verdict must be revoked when
// the CONNECTION goes unhealthy. A hook relay has no connection to be unhealthy — the hub
// either received an event or it did not, exactly like a file that has or has not grown —
// and a "stop" verdict stays true until the next event says otherwise. Modeling a health
// dimension that does not exist would only invent a way to go dark.
//
// NOT an approval surface: cursor has no approval hook event (verified in the spike — a
// session blocked on the allowlist prompt is indistinguishable, hook-wise, from a long
// tool call). needs_approval for cursor comes from the pane anchor in reconcile
// (AgentSpec.ApprovalAnchor), so this watcher deliberately implements NEITHER
// approvalBlocker NOR approvalPublisher.
type cursorWatcher struct {
	// No clock of its own, deliberately: unlike the opencode watcher (which stamps SSE
	// frame arrivals from a background goroutine), every timestamp this watcher needs is
	// the `now` reconcile hands to refresh/snapshot — so there is no second time source to
	// keep consistent with the hub's.
	logf func(string, ...any)

	mu         sync.Mutex
	fold       *cursorFold
	inbox      []cursorHookEvent
	inboxBytes int
	gapped     bool // an inbox overflow dropped an event: fold.noteGap() on the next refresh
	closed     bool // terminal: push/refresh/snapshot no-op (see close)

	// priorID is the SHED_RC_AGENT_SESSION pin from a previous hub lifetime ("" if none).
	// It suppresses a redundant back-write when the hook stream pins the same id again.
	priorID string
	// confirmedID is a newly pinned agent session id awaiting drainConfirmedAgentID.
	confirmedID string

	// fold-derived verdict (refresh writes; snapshot reads)
	lastEventAt time.Time
	curActivity Activity
	curMessage  string
	curSettled  bool
	pending     []feedMessage
}

// Compile-time checks. sessionWatcher is the reconcile/input surface;
// confirmedAgentIDDrainer carries the hook-discovered session id to the tmux back-write;
// cursorIngester is the ingest handler's push seam.
var (
	_ sessionWatcher          = (*cursorWatcher)(nil)
	_ confirmedAgentIDDrainer = (*cursorWatcher)(nil)
	_ cursorIngester          = (*cursorWatcher)(nil)
)

// cursorIngester is the narrow interface the hub's ingest handler pushes through, so the
// handler holds a sessionWatcher (as reconcile does) and type-asserts exactly this one
// capability — the confirmedAgentIDDrainer/approvalPublisher precedent.
type cursorIngester interface {
	// pushHookEvent enqueues one hook event for the next refresh to fold, reporting
	// whether it was accepted (false = the watcher is closed, or the inbox is full and
	// the event was dropped).
	pushHookEvent(ev cursorHookEvent) bool
}

// cursorHookEvent is one hook delivery: the event NAME (the ingest route's ?event=, which
// the preseeded script passes as argv[1]) plus the raw JSON payload cursor wrote to the
// script's stdin. The payload is kept raw — the fold parses it — so the ingest path stays
// a dumb pipe and every shape decision lives in one place.
type cursorHookEvent struct {
	event   string
	payload []byte
}

const (
	// maxCursorInboxItems / maxCursorInboxBytes bound the LIVE watcher's inbox by both
	// count and bytes. The byte bound is the load-bearing one: a single afterShellExecution
	// payload may be up to the ingest cap (256 KiB), so a count-only bound would admit
	// ~64 MiB. On overflow the incoming event is dropped and a gap is recorded (the fold's
	// record-exact state is then dropped — see noteGap).
	maxCursorInboxItems = 256
	maxCursorInboxBytes = 4 << 20 // 4 MiB
)

// newCursorWatcher builds a push-fed watcher for a cursor session. priorID is the
// back-written SHED_RC_AGENT_SESSION from an earlier hub lifetime ("" if none); a
// malformed one is discarded rather than trusted (a tmux env var is operator-writable, and
// the id ends up on the wire and in a back-write). logf may be nil (defaulted).
//
// Unlike the file watchers there is nothing to correlate here and nothing to open: the
// watcher is usable the moment it exists, which is why ensureWatcher constructs it
// immediately with no correlation input and simply drains the hub's pre-watcher inbox into
// it.
func newCursorWatcher(priorID string, logf func(string, ...any)) *cursorWatcher {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	if priorID != "" && !validCursorSessionID(priorID) {
		logf("rc hub: cursor watcher ignoring a malformed %s pin", envAgentSession)
		priorID = ""
	}
	return &cursorWatcher{
		logf:    logf,
		fold:    newCursorFold(priorID),
		priorID: priorID,
	}
}

// ---- sessionWatcher surface ----

// pushHookEvent appends an event to the inbox under the watcher mutex (the ingest
// handler's goroutine). A CLOSED watcher drops silently; an overflowing inbox drops the
// event and records a gap so the next refresh can clear the state that assumed a complete
// stream. Dropping the NEWEST (rather than the oldest) keeps the retained prefix
// contiguous, which is what the fold's turn-boundary reasoning wants.
func (w *cursorWatcher) pushHookEvent(ev cursorHookEvent) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return false
	}
	if len(w.inbox) >= maxCursorInboxItems || w.inboxBytes+len(ev.payload) > maxCursorInboxBytes {
		w.gapped = true
		return false
	}
	w.inbox = append(w.inbox, ev)
	w.inboxBytes += len(ev.payload)
	return true
}

// refresh drains the inbox under the mutex and folds each event, mirroring
// fileWatcher.refresh but sourced from pushes rather than a tailer. A CLOSED watcher
// no-ops.
func (w *cursorWatcher) refresh(now time.Time) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return
	}
	if w.gapped {
		// Events were lost to inbox overflow: drop the state that assumes every record was
		// seen (open tool calls), leaving the verdict to the next turn boundary.
		w.fold.noteGap()
		w.gapped = false
	}
	items := w.inbox
	w.inbox = nil
	w.inboxBytes = 0
	for _, ev := range items {
		if w.fold.applyEvent(ev) {
			w.lastEventAt = now
		}
	}
	w.curActivity = w.fold.activity()
	w.curMessage = w.fold.lastMessage()
	w.curSettled = w.fold.settled()
	w.pending = append(w.pending, w.fold.drainMessages()...)
	// A hook-carried session id that differs from what was already back-written is the
	// only pin cursor gives us; surface it for reconcile to stamp (see
	// drainConfirmedAgentID).
	if id := w.fold.drainNewPin(); id != "" && id != w.priorID {
		w.confirmedID = id
		w.priorID = id
	}
}

// snapshot reports the verdict + its authority at now, on the fileWatcher's rule (see the
// FRESHNESS note on cursorWatcher for why the opencode transport-health rule does not
// apply here). An empty/unknown verdict is never fresh.
func (w *cursorWatcher) snapshot(now time.Time) (activity Activity, message string, fresh, expiredWorking bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		// A closed watcher has no authority: force expiredWorking=false so pane stability
		// drives (the opencodeWatcher precedent).
		return w.curActivity, w.curMessage, false, false
	}
	fresh, expiredWorking = watcherFreshness(w.curActivity, w.curSettled, w.lastEventAt, now)
	return w.curActivity, w.curMessage, fresh, expiredWorking
}

// drainPending returns and clears the feed messages folded since the last drain (in
// stream order); reconcile appends them to the session ring.
func (w *cursorWatcher) drainPending() []feedMessage {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.pending) == 0 {
		return nil
	}
	out := w.pending
	w.pending = nil
	return out
}

// hadEvent reports whether at least one activity-relevant hook event has been folded.
func (w *cursorWatcher) hadEvent() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return !w.lastEventAt.IsZero()
}

// drainConfirmedAgentID returns and clears a newly pinned cursor session id for reconcile
// to back-write into SHED_RC_AGENT_SESSION ("" when none / already drained). A RE-PIN (the
// operator switched chats in the TUI) surfaces here again with the new id, which is the
// point: the session's identity follows what its TUI is showing.
func (w *cursorWatcher) drainConfirmedAgentID() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	id := w.confirmedID
	w.confirmedID = ""
	return id
}

// close marks the watcher terminally closed. There is no I/O to unwind (pushes are the
// transport), so this only stops later pushes/refreshes from mutating a discarded
// watcher. Idempotent.
func (w *cursorWatcher) close() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.closed = true
}

// ---- the fold ----

// validCursorSessionID bounds what may be trusted as a cursor session id. Cursor's
// session_id == conversation_id == the transcript directory name — a UUID in every
// captured payload — so the grammar is exactly a UUID. It guards two paths: a hostile
// SHED_RC_AGENT_SESSION in the tmux env, and an id from a hook payload (the payload
// arrives on a loopback route any process in the shed can POST to, and the id is
// back-written into the tmux env and used as a path segment by the transcript backfill).
var cursorSessionIDRe = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

func validCursorSessionID(id string) bool { return cursorSessionIDRe.MatchString(id) }

// cursorHookPayload is the union of the fields the fold reads across cursor's hook
// events. Every field is optional and tolerantly typed — an event that carries none of
// them folds to nothing rather than failing. Shapes verified against the live spike
// capture (~/.claude/plans/shed/008-observatory/spikes/cursor-hooks/runB-tui.jsonl, a full
// TUI session's hook log from cursor-agent 2026.08.11-e8db854).
type cursorHookPayload struct {
	// SessionID is cursor's conversation id; present on every event except workspaceOpen.
	SessionID string `json:"session_id"`
	// Prompt is beforeSubmitPrompt's submitted text.
	Prompt string `json:"prompt"`
	// ToolName / ToolInput ride preToolUse and postToolUseFailure.
	ToolName  string          `json:"tool_name"`
	ToolInput json.RawMessage `json:"tool_input"`
	// Command / Output ride afterShellExecution (Output is the command's combined output —
	// routinely far larger than the 16 KiB verb cap, which is why ingest has its own).
	Command string `json:"command"`
	Output  string `json:"output"`
	// FilePath / Edits ride afterFileEdit.
	FilePath string            `json:"file_path"`
	Edits    []json.RawMessage `json:"edits"`
	// ErrorMessage rides postToolUseFailure.
	ErrorMessage string `json:"error_message"`
	// Text is afterAgentResponse's assistant message.
	Text string `json:"text"`
	// Status is stop's turn outcome — "completed", or "aborted"/"error" for a turn the
	// operator interrupted or that failed (a status row says which; see the stop arm).
	// Reason/FinalStatus ride sessionEnd.
	Status      string `json:"status"`
	Reason      string `json:"reason"`
	FinalStatus string `json:"final_status"`
	// Model rides sessionStart (and most others) — used only for the start status row.
	Model string `json:"model"`
}

// cursorFold folds the hook event stream into an activity verdict + a normalized message
// feed, the same contract codexFold/opencodeFold implement (activity/lastMessage/settled +
// drainMessages). It holds cumulative state across applyEvent calls and is NOT safe for
// concurrent use — cursorWatcher serializes access under its mutex.
//
// It deliberately does NOT implement activityFold: that interface's unit is a JSONL LINE,
// and a hook event is (name, payload). The watcher calls applyEvent directly; only
// messageProducer's drainMessages is shared verbatim, so reconcile's drain path is
// identical to every other watcher's.
type cursorFold struct {
	confirmed bool   // ≥1 activity-relevant event folded (unknown until then)
	state     string // "" | cursorStateWorking | cursorStateIdle
	pinnedID  string // the cursor session id this fold is following
	newPin    string // a pin awaiting drainNewPin (back-write to SHED_RC_AGENT_SESSION)
	openTools int    // preToolUse seen without its matching postToolUse/postToolUseFailure
	lastMsg   string // latest assistant message text
	msgs      []feedMessage
}

// Fold verdict states. A cursor turn runs from beforeSubmitPrompt to stop; everything in
// between (tool calls, agent responses) is working, and stop is the settled boundary.
const (
	cursorStateWorking = "working"
	cursorStateIdle    = "idle"
)

var _ messageProducer = (*cursorFold)(nil)

func newCursorFold(priorID string) *cursorFold {
	return &cursorFold{pinnedID: priorID}
}

// applyEvent folds one hook event, returning true when it advanced meaningful state. An
// unknown event name, an unparseable payload, or an event whose payload carries nothing
// the fold reads returns false and leaves state untouched — the same tolerant-parsing
// contract activityFold.applyLine states, so a cursor release that adds a field (or an
// event we never wired) is inert rather than fatal.
func (f *cursorFold) applyEvent(ev cursorHookEvent) bool {
	var p cursorHookPayload
	if len(ev.payload) > 0 && json.Unmarshal(ev.payload, &p) != nil {
		return false
	}
	// Pinning first: EVERY id-carrying event participates, because sessionStart is absent
	// on `cursor-agent --resume` and would otherwise be the only way to learn the id.
	repinned := f.notePin(p.SessionID)
	if !f.foldEvent(ev.event, &p) {
		return repinned
	}
	// Exactly one place sets confirmed: "a hook event told us this session is alive". It
	// follows foldEvent so an event that folded NOTHING (unwired, or empty text) leaves the
	// verdict at unknown, which is what lets pane stability keep driving.
	f.confirmed = true
	return true
}

// foldEvent applies one recognized hook event to the fold's state, returning whether it
// advanced anything. See applyEvent for the pinning + confirmed handling wrapped around it.
func (f *cursorFold) foldEvent(event string, p *cursorHookPayload) bool {
	switch event {
	case "sessionStart":
		f.state = cursorStateIdle // a fresh TUI is parked at its composer
		text := "cursor session started"
		if p.Model != "" {
			text += " (" + p.Model + ")"
		}
		f.emitStatus(text)
		return true
	case "beforeSubmitPrompt":
		if trimFeedText(p.Prompt) == "" {
			return false
		}
		f.state = cursorStateWorking
		f.emit(feedMessage{Role: feedRoleUser, Type: feedTypeText, Text: p.Prompt})
		return true
	case "preToolUse":
		f.state = cursorStateWorking
		f.openTools++
		f.emit(feedMessage{
			Role: feedRoleTool,
			Type: feedTypeToolUse,
			Tool: &feedTool{Name: p.ToolName, Detail: cursorToolDetail(p)},
		})
		return true
	case "afterShellExecution":
		// Deliberately NOT a closeTool: this is an EXTRA event riding alongside the call's
		// own postToolUse, not its terminator (the spike's turn: 5 preToolUse ↔ 3 postToolUse
		// + 2 postToolUseFailure, with afterShellExecution/afterFileEdit on top of those).
		// Decrementing here as well would double-count and forget a genuinely open call.
		//
		// The ring sanitizes + caps the detail at 8 KiB (sanitizeFeedText), so the raw
		// output is handed over untouched: truncation policy lives in exactly one place.
		f.emit(feedMessage{
			Role: feedRoleTool,
			Type: feedTypeToolResult,
			Tool: &feedTool{Name: "Shell", Detail: firstNonEmpty(p.Output, p.Command)},
		})
		return true
	case "afterFileEdit":
		// Not a closeTool either — same reason as afterShellExecution above.
		f.emit(feedMessage{
			Role: feedRoleTool,
			Type: feedTypeToolResult,
			Tool: &feedTool{Name: "Edit", Detail: cursorEditDetail(p)},
		})
		return true
	case "postToolUse":
		// The counter's other half, and the ONLY thing this event is wired for: every
		// preToolUse is matched by exactly one postToolUse or postToolUseFailure, including
		// for the Read/Grep/Glob-class tools that emit no after* event at all. No feed row —
		// its tool_output would duplicate afterShellExecution/afterFileEdit for the two
		// families that DO emit one, and those carry the better-shaped detail.
		f.closeTool()
		return true
	case "postToolUseFailure":
		f.closeTool()
		text := "tool failed"
		if p.ToolName != "" {
			text = "tool " + p.ToolName + " failed"
		}
		if p.ErrorMessage != "" {
			text += ": " + p.ErrorMessage
		}
		f.emitStatus(text)
		return true
	case "afterAgentResponse":
		if trimFeedText(p.Text) == "" {
			return false
		}
		f.state = cursorStateWorking // the turn ends at `stop`, not at a message
		f.lastMsg = p.Text
		f.emit(feedMessage{Role: feedRoleAssistant, Type: feedTypeText, Text: p.Text})
		return true
	case "stop":
		// The turn is over and cursor is back at its composer: the SETTLED boundary, which
		// snapshot then trusts indefinitely (a waiting agent emits nothing at all).
		f.state = cursorStateIdle
		f.openTools = 0
		// A turn that ended any way OTHER than "completed" (the spike shows "aborted" and
		// "error") settles identically — the composer is live either way — but the operator
		// needs to know WHY the work stopped, or an interrupted turn reads on a phone as a
		// finished one. Only the non-completed case emits: a row per normal turn end would
		// be pure noise.
		if p.Status != "" && p.Status != "completed" {
			f.emitStatus("turn ended: " + p.Status)
		}
		return true
	case "sessionEnd":
		f.state = cursorStateIdle
		f.openTools = 0
		text := "cursor session ended"
		if s := firstNonEmpty(p.FinalStatus, p.Reason); s != "" {
			text += " (" + s + ")"
		}
		f.emitStatus(text)
		return true
	default:
		// Unwired/unknown events (afterAgentThought, beforeShellExecution, workspaceOpen, a
		// future addition): dropped silently. A repin carried by such an event still counts.
		return false
	}
}

// notePin adopts the session id an event carries. The FIRST valid id pins the fold; a
// DIFFERENT valid id later means the operator switched chats in the TUI (cursor keeps one
// process across conversations), so the fold RE-PINS — a session is scoped to whatever its
// TUI is showing — announcing the switch with a status row and dropping the previous
// chat's carry-over state (its open tool calls and last message describe a conversation
// that is no longer on screen). An invalid/absent id is ignored entirely.
func (f *cursorFold) notePin(id string) bool {
	if id == "" || !validCursorSessionID(id) {
		return false
	}
	if f.pinnedID == id {
		return false
	}
	switched := f.pinnedID != ""
	f.pinnedID = id
	f.newPin = id
	if switched {
		f.openTools = 0
		f.lastMsg = ""
		f.emitStatus("cursor switched to another chat (" + id + ")")
	}
	return true
}

// drainNewPin returns and clears a newly adopted session id ("" when unchanged since the
// last drain). The watcher forwards it to reconcile's SHED_RC_AGENT_SESSION back-write.
func (f *cursorFold) drainNewPin() string {
	id := f.newPin
	f.newPin = ""
	return id
}

// closeTool retires one open tool call (its postToolUse/postToolUseFailure arrived — NOT
// the after* events, which ride alongside rather than terminate). Floored at zero:
// hooks are a lossy channel by construction (a hub restart mid-turn, an inbox gap), so a
// result whose preToolUse was never seen must not push the counter negative.
func (f *cursorFold) closeTool() {
	if f.openTools > 0 {
		f.openTools--
	}
}

func (f *cursorFold) emit(m feedMessage) { f.msgs = append(f.msgs, m) }
func (f *cursorFold) emitStatus(text string) {
	f.emit(feedMessage{Role: feedRoleSystem, Type: feedTypeStatus, Text: text})
}
func (f *cursorFold) lastMessage() string { return SanitizeLastMessage(f.lastMsg) }
func (f *cursorFold) settled() bool       { return f.confirmed && f.state == cursorStateIdle }

// activity is the fold's verdict: unknown until a hook confirms the session is alive, then
// working while a turn (or a tool call) is in flight and needs_input once `stop` lands.
// needs_approval is deliberately absent — cursor emits NO approval hook, so that verdict
// comes from the pane anchor in reconcile (see the cursorWatcher doc).
func (f *cursorFold) activity() Activity {
	if !f.confirmed {
		return ActivityUnknown
	}
	if f.openTools > 0 {
		return ActivityWorking
	}
	switch f.state {
	case cursorStateIdle:
		return ActivityNeedsInput
	default:
		return ActivityWorking
	}
}

// drainMessages returns and clears the produced-but-undrained feed messages
// (messageProducer).
func (f *cursorFold) drainMessages() []feedMessage {
	if len(f.msgs) == 0 {
		return nil
	}
	out := f.msgs
	f.msgs = nil
	return out
}

// noteGap drops the open-tool count after an inbox overflow: a swallowed
// afterShellExecution/afterFileEdit/postToolUseFailure would otherwise pin the verdict at
// working until the next turn boundary. The turn state itself is KEPT — `stop` is what
// settles a turn, and a gap is no evidence that it arrived.
func (f *cursorFold) noteGap() { f.openTools = 0 }

// cursorToolDetail renders a preToolUse's compact detail: the shell command or the file
// path when the tool_input carries one (by far the most common shapes — Shell/Read/Write/
// Edit), else the whole compacted input object. Falls back to "" for an absent input,
// which the feed simply omits.
func cursorToolDetail(p *cursorHookPayload) string {
	var in struct {
		Command  string `json:"command"`
		FilePath string `json:"file_path"`
	}
	if len(p.ToolInput) > 0 && json.Unmarshal(p.ToolInput, &in) == nil {
		if in.Command != "" {
			return in.Command
		}
		if in.FilePath != "" {
			return in.FilePath
		}
	}
	return compactJSON(p.ToolInput)
}

// cursorEditDetail renders an afterFileEdit's detail: the edited path plus how many edits
// landed. The edit BODIES are deliberately not included — they are diffs of arbitrary
// size, and the path + count is what a phone-sized feed row can use.
func cursorEditDetail(p *cursorHookPayload) string {
	path := p.FilePath
	if path == "" {
		path = "(unknown file)"
	}
	if n := len(p.Edits); n > 0 {
		unit := "edits"
		if n == 1 {
			unit = "edit"
		}
		return fmt.Sprintf("%s (%d %s)", path, n, unit)
	}
	return path
}
