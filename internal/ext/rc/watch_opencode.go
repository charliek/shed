package rc

import (
	"bytes"
	"encoding/json"
	"strings"
	"time"
)

// opencodeFold folds an opencode session's /event envelope stream into an activity
// verdict AND a normalized message feed (drainMessages), mirroring codexFold's shape.
// Unlike codex (a tailed append-only JSONL file) opencode is a client/server model: the
// hub subscribes to the embedded HTTP+SSE server's /event endpoint as a second client.
// This file is the PURE fold only — no network/transport (that is the opencodeWatcher in
// watch_opencode_transport.go) and no correlation (that is correlateOpencode). The fold
// is SESSION-SCOPED: it assumes every envelope it is handed already belongs to its
// session (the transport filters by sessionID before calling applyLine), so the fold
// itself does NOT filter by sessionID.
//
// Wire shape (verified against ../opencode 1.18.4 source + a live /event capture; see the
// plan's SCHEMA-NOTES.md). Each applyLine receives one decoded SSE `data:` payload:
//
//	{ "id": "evt_…", "type": "<dotted>", "properties": { … } }   (top-level id ignored)
//
// The envelope types the fold reads:
//
//   - session.status {status:{type}}   busy → working; idle → needs_input (settled);
//     retry → working (keep last message).
//   - session.idle {sessionID}         → needs_input (settled).
//   - message.updated {info:{id,role,time:{created,completed}}}  tracks messageID→role and
//     completion so cached text/reasoning parts can be flushed (feed only — not activity).
//   - message.part.updated {part,time}  the part carries the FULL current snapshot (NOT a
//     delta; message.part.delta is ignored):
//       - text part      role from its message; user text emits immediately, assistant text
//         emits when part.time.end is set OR the owning message has time.completed.
//       - reasoning part role assistant, type reasoning, same terminal rule.
//       - tool part      tool_use (name+state.input) at running/completed, tool_result
//         (state.output/state.error) at completed/error; keyed by callID. A pending part
//         (input:{}) tracks the call as open (working) but emits nothing. A tool part whose
//         state.status is none of pending/running/completed/error is tolerantly ignored (no
//         confirm, no pending-set change, no emit).
//   - permission.asked / question.asked  → a display-only `status` feed row (role system);
//     NO activity change. Emitted only when it carries meaningful content (a non-empty
//     permission kind / at least one non-empty question); an empty/null-properties ask is
//     ignored rather than emitting a hollow row.
//
// Everything else — message.part.removed, session.error, session.created/updated, server.*,
// session.next.*, message.part.delta, step-start/step-finish, catalog/plugin, an unknown
// type/part/field, or an unparseable line — is ignored: applyLine returns false and leaves
// state untouched (tolerant parsing, per the activityFold contract).
//
// Feed emission is DEDUPED by (part identity, phase) so a reseed after an SSE reconnect —
// which replays the same history WITHOUT resetting the fold — can never emit a part twice.
// Timestamps: opencode times are epoch-MILLIS integers, converted to UTC RFC3339 for
// feedMessage.TS so seeded history keeps its real ordering instead of being stamped with
// reconcile-now.

// ---- parse structs (all fields optional; tolerant) ----

// ocEnvelope is the generic {id,type,properties} frame every /event payload shares.
type ocEnvelope struct {
	Type       string          `json:"type"`
	Properties json.RawMessage `json:"properties"`
}

// ocProperties is the union of the properties fields the fold reads across event types.
type ocProperties struct {
	SessionID  string         `json:"sessionID"`
	Status     *ocStatus      `json:"status"`     // session.status
	Info       *ocMessageInfo `json:"info"`       // message.updated
	Part       *ocPart        `json:"part"`       // message.part.updated
	Time       int64          `json:"time"`       // message.part.updated update time (epoch ms)
	ID         string         `json:"id"`         // permission.asked (per_…) / question.asked (que_…)
	Permission string         `json:"permission"` // permission.asked kind ("bash", "edit", …)
	Patterns   []string       `json:"patterns"`   // permission.asked matched commands/globs
	Questions  []ocQuestion   `json:"questions"`  // question.asked
}

// ocStatus is session.status's status union, discriminated by type (busy|idle|retry).
type ocStatus struct {
	Type string `json:"type"`
}

// ocMessageInfo is the message.updated info (User|Assistant by role).
type ocMessageInfo struct {
	ID   string `json:"id"`
	Role string `json:"role"` // user | assistant
	Time ocTime `json:"time"` // {created, completed}
}

// ocTime carries both a message time ({created,completed}) and a part/tool time
// ({start,end}); the unused pair stays zero for whichever shape is present.
type ocTime struct {
	Created   int64 `json:"created"`
	Completed int64 `json:"completed"`
	Start     int64 `json:"start"`
	End       int64 `json:"end"`
}

// ocPart is a message.part.updated part (the full snapshot).
type ocPart struct {
	ID        string       `json:"id"`
	MessageID string       `json:"messageID"`
	Type      string       `json:"type"` // text | reasoning | tool | step-start | step-finish | …
	Text      string       `json:"text"` // text / reasoning
	Time      ocTime       `json:"time"` // text / reasoning: {start,end}
	Synthetic bool         `json:"synthetic"`
	Ignored   bool         `json:"ignored"`
	Tool      string       `json:"tool"`   // tool part: the tool NAME
	CallID    string       `json:"callID"` // tool part: the call id (dedup key)
	State     *ocToolState `json:"state"`  // tool part: the state union
}

// ocToolState is a tool part's state union, discriminated by status.
type ocToolState struct {
	Status string          `json:"status"` // pending | running | completed | error
	Input  json.RawMessage `json:"input"`  // invocation arguments (compact JSON → tool_use detail)
	Output string          `json:"output"` // completed result → tool_result detail
	Error  string          `json:"error"`  // error message → tool_result detail
	Time   ocTime          `json:"time"`   // {start,end}
}

// ocQuestion is one entry of question.asked's questions[] (shape tolerated).
type ocQuestion struct {
	Header string `json:"header"`
	Text   string `json:"text"`
}

// ---- cached parts (text/reasoning awaiting their terminal snapshot or their role) ----

// ocPartCache holds the latest snapshot of a text/reasoning part that has not yet been
// emitted — either because its owning message's role is not known yet (parts can arrive
// before their message.updated) or because its terminal condition (part.time.end / message
// completed) has not been met. It is dropped once the part emits.
type ocPartCache struct {
	partID    string
	messageID string
	kind      string // "text" | "reasoning"
	text      string // latest snapshot text (snapshots go empty→full, never additive)
	hasEnd    bool   // the latest snapshot carried a part.time.end
	tsStart   int64  // part.time.start (epoch ms)
	tsEnd     int64  // part.time.end   (epoch ms)
	updTime   int64  // properties.time (epoch ms) — the update-time fallback
}

// opencodeFold folds the /event envelope stream into activity + feed. It holds cumulative
// state across applyLine calls and is NOT safe for concurrent use (the owning watcher
// serializes access, mirroring the fileWatcher/activityFold contract).
type opencodeFold struct {
	confirmed    bool            // seen ≥1 activity-relevant event (session.status/idle or a tool part)
	sawStatus    bool            // seen ≥1 LIVE session.status/session.idle boundary (REST status is a fallback)
	lastBoundary string          // "busy" | "idle" | "" (retry maps to busy)
	pending      map[string]bool // open tool-call ids (pending/running)
	lastMsg      string          // latest assistant text (sanitized on read)

	msgRole      map[string]string // messageID → role (user|assistant)
	msgCreated   map[string]int64  // messageID → time.created (epoch ms)
	msgCompleted map[string]int64  // messageID → time.completed (epoch ms; 0 == not completed)

	parts     map[string]*ocPartCache // partID → latest un-emitted text/reasoning snapshot
	partOrder []string                // partID insertion order (deterministic flush ordering)

	emitted map[string]bool // dedup: "<id>|<phase>" already emitted (survives reseed + gap)
	msgs    []feedMessage   // produced-but-undrained feed messages
}

func newOpencodeFold() *opencodeFold {
	return &opencodeFold{
		pending:      map[string]bool{},
		msgRole:      map[string]string{},
		msgCreated:   map[string]int64{},
		msgCompleted: map[string]int64{},
		parts:        map[string]*ocPartCache{},
		emitted:      map[string]bool{},
	}
}

// Compile-time assertions that opencodeFold satisfies both fold interfaces.
var (
	_ activityFold    = (*opencodeFold)(nil)
	_ messageProducer = (*opencodeFold)(nil)
)

// applyLine folds one decoded {id,type,properties} envelope. It returns true when the
// envelope was recognized and folded (activity change, a cached/emitted feed part, or a
// role/completion update); an ignored family or an unparseable line returns false and
// leaves state untouched.
func (f *opencodeFold) applyLine(line []byte) bool {
	var env ocEnvelope
	if json.Unmarshal(line, &env) != nil || env.Type == "" {
		return false
	}
	var props ocProperties
	if len(env.Properties) > 0 {
		if json.Unmarshal(env.Properties, &props) != nil {
			return false // a malformed properties body: tolerant, state untouched
		}
	}

	switch env.Type {
	case "session.status":
		if props.Status == nil {
			return false
		}
		switch props.Status.Type {
		case "busy", "retry": // retry keeps working (and keeps the prior last message)
			f.confirmed = true
			f.sawStatus = true
			f.lastBoundary = "busy"
			return true
		case "idle":
			f.confirmed = true
			f.sawStatus = true
			f.lastBoundary = "idle"
			return true
		default:
			return false
		}
	case "session.idle":
		f.confirmed = true
		f.sawStatus = true
		f.lastBoundary = "idle"
		return true
	case "message.updated":
		return f.applyMessageUpdated(props.Info)
	case "message.part.updated":
		if props.Part == nil {
			return false
		}
		switch props.Part.Type {
		case "text", "reasoning":
			return f.applyTextPart(props.Part, props.Time)
		case "tool":
			return f.applyToolPart(props.Part, props.Time)
		default:
			return false // step-start, step-finish, file, agent, subtask, snapshot, …
		}
	case "permission.asked":
		// Require a meaningful permission kind: absent/null properties (or an empty kind)
		// must not fabricate a hollow "awaiting approval:" row.
		if props.Permission == "" {
			return false
		}
		text := "awaiting approval: " + props.Permission
		if len(props.Patterns) > 0 {
			text += " — " + strings.Join(props.Patterns, ", ")
		}
		f.emitStatusRow("perm", props.ID, text)
		return true
	case "question.asked":
		// Require at least one question with real text: no questions (or an empty question)
		// must not fabricate a hollow "awaiting answer:" row.
		if len(props.Questions) == 0 {
			return false
		}
		hdr := firstNonEmpty(props.Questions[0].Header, props.Questions[0].Text)
		if hdr == "" {
			return false
		}
		f.emitStatusRow("ques", props.ID, "awaiting answer: "+hdr)
		return true
	default:
		// session.created/updated, session.error, message.part.removed, message.part.delta,
		// session.next.*, server.*, catalog/plugin, and any unknown type: ignored.
		return false
	}
}

// applyMessageUpdated tracks a message's role + completion time (feed bookkeeping — it does
// NOT touch activity) and flushes any cached parts for that message now that its role /
// completion is known. It returns true ONLY when it advanced state — a newly-known role, a
// newly-known completion, or a cached part it flushed. A repeated or id-only snapshot that
// changes nothing returns false (per the applyLine contract: this path is feed-tracking, not
// activity, so a no-op must not count as an event).
func (f *opencodeFold) applyMessageUpdated(info *ocMessageInfo) bool {
	if info == nil || info.ID == "" {
		return false
	}
	advanced := false
	if info.Role != "" && f.msgRole[info.ID] != info.Role {
		f.msgRole[info.ID] = info.Role
		advanced = true
	}
	if info.Time.Created != 0 {
		f.msgCreated[info.ID] = info.Time.Created
	}
	if info.Time.Completed != 0 && f.msgCompleted[info.ID] == 0 {
		f.msgCompleted[info.ID] = info.Time.Completed
		advanced = true
	}
	for _, ent := range f.partsForMessage(info.ID) {
		if f.tryEmitPart(ent) {
			advanced = true
		}
	}
	return advanced
}

// applyTextPart caches a text/reasoning part's latest snapshot and attempts to emit it.
//
//   - A synthetic/ignored snapshot SUPPRESSES the part: any earlier cached partial for the
//     same partID is dropped so it can never be flushed on message-completion (a stale
//     partial must not survive a later suppressing snapshot).
//   - A part with no messageID can never have its role resolved, so it is never cached.
//   - A part already emitted (its body dedup key is set — a reseed replay) is not re-cached:
//     re-appending it to partOrder on every reconnect would leak unboundedly.
func (f *opencodeFold) applyTextPart(p *ocPart, updTime int64) bool {
	if p.ID == "" {
		return false
	}
	if p.Synthetic || p.Ignored {
		f.dropPart(p.ID) // suppressing snapshot: drop any cached partial for this partID
		return false
	}
	if p.MessageID == "" {
		return false // ownerless part: its role is unresolvable, so never cache it
	}
	if f.emitted[bodyKey(p.ID)] {
		return false // already emitted (reseed replay): don't re-cache/re-append
	}
	ent := f.parts[p.ID]
	if ent == nil {
		ent = &ocPartCache{partID: p.ID, kind: p.Type}
		f.parts[p.ID] = ent
		f.partOrder = append(f.partOrder, p.ID)
	}
	ent.messageID = p.MessageID
	ent.text = p.Text
	if p.Time.Start != 0 {
		ent.tsStart = p.Time.Start
	}
	if p.Time.End != 0 {
		ent.tsEnd = p.Time.End
		ent.hasEnd = true
	}
	ent.updTime = updTime
	f.tryEmitPart(ent)
	return true
}

// tryEmitPart emits a cached text/reasoning part if its role is known and its terminal
// condition is met (user text emits immediately; assistant text/reasoning emits at
// part.time.end or message completion). Deduped by partID so a reseed re-fold is a no-op.
// Returns true only when it actually emitted a feed row (advanced feed state).
func (f *opencodeFold) tryEmitPart(ent *ocPartCache) bool {
	key := bodyKey(ent.partID)
	if f.emitted[key] {
		f.dropPart(ent.partID)
		return false
	}
	role := f.msgRole[ent.messageID]
	if role == "" {
		return false // role not known yet: keep cached, flush on the owning message.updated
	}
	// The feed contract carries only user/assistant/tool/system roles. A text/reasoning
	// part whose owning message role is neither user nor assistant can never resolve to a
	// valid feed row, so drop it rather than hold it cached forever.
	if role != feedRoleUser && role != feedRoleAssistant {
		f.dropPart(ent.partID)
		return false
	}
	var typ string
	switch ent.kind {
	case "text":
		typ = feedTypeText
	case "reasoning":
		typ = feedTypeReasoning
	default:
		return false
	}
	terminal := role == feedRoleUser || ent.hasEnd || f.msgCompleted[ent.messageID] != 0
	if !terminal {
		return false
	}
	feedRole := role
	if ent.kind == "reasoning" {
		feedRole = feedRoleAssistant
	}
	ts := opencodeTS(firstNonZero(ent.tsEnd, ent.tsStart, ent.updTime, f.msgCompleted[ent.messageID], f.msgCreated[ent.messageID]))
	if f.emitOnce(key, feedMessage{TS: ts, Role: feedRole, Type: typ, Text: ent.text}) {
		if role == feedRoleAssistant && ent.kind == "text" {
			f.lastMsg = ent.text
		}
		f.dropPart(ent.partID)
		return true
	}
	return false
}

// applyToolPart tracks a tool call's pending state and emits tool_use / tool_result rows,
// each once, keyed by callID. tool_use emits on the first running/completed/error snapshot
// carrying non-empty input; tool_result emits on completed (output) / error (error). A
// completed snapshot seen with neither yet emitted emits BOTH (tool_use then tool_result).
func (f *opencodeFold) applyToolPart(p *ocPart, updTime int64) bool {
	if p.CallID == "" || p.State == nil {
		return false
	}
	st := p.State
	switch st.Status {
	case "pending", "running":
		f.pending[p.CallID] = true
	case "completed", "error":
		delete(f.pending, p.CallID)
	default:
		// An unrecognized tool state is tolerantly ignored: it must NOT confirm activity,
		// mutate the pending set, or emit — a bogus/unknown status is noise, not a call.
		return false
	}
	f.confirmed = true // a recognized tool part is activity-relevant (pending/running → working)

	detail := compactJSON(st.Input)
	hasInput := detail != "" && detail != "{}" && detail != "null"
	if hasInput && (st.Status == "running" || st.Status == "completed" || st.Status == "error") {
		ts := opencodeTS(firstNonZero(st.Time.Start, updTime))
		f.emitOnce(p.CallID+"|use", feedMessage{TS: ts, Role: feedRoleTool, Type: feedTypeToolUse, Tool: &feedTool{Name: p.Tool, Detail: detail}})
	}
	if st.Status == "completed" || st.Status == "error" {
		resultDetail := st.Output
		if st.Status == "error" {
			resultDetail = st.Error
		}
		ts := opencodeTS(firstNonZero(st.Time.End, st.Time.Start, updTime))
		f.emitOnce(p.CallID+"|result", feedMessage{TS: ts, Role: feedRoleTool, Type: feedTypeToolResult, Tool: &feedTool{Name: p.Tool, Detail: resultDetail}})
	}
	return true
}

// emitOnce queues a feed message unless its dedup key was already emitted; empty-text
// non-tool messages are dropped (and do NOT consume the dedup slot, so a later non-empty
// snapshot of the same part can still emit). Returns true when a message was queued.
func (f *opencodeFold) emitOnce(key string, m feedMessage) bool {
	if key != "" && f.emitted[key] {
		return false
	}
	if m.Tool == nil && trimFeedText(m.Text) == "" {
		return false
	}
	if key != "" {
		f.emitted[key] = true
	}
	f.msgs = append(f.msgs, m)
	return true
}

// emitStatusRow queues a display-only system/status feed row (permission.asked /
// question.asked), deduped by request id. When the wire carries no id, the dedup key is
// derived from the row's CONTENT instead — otherwise a reseed replay (which has no id to key
// on) would emit an identical status row on every reconnect. The row is informational: it
// never changes the activity verdict.
func (f *opencodeFold) emitStatusRow(prefix, id, text string) {
	key := prefix + "|"
	if id != "" {
		key += "id:" + id
	} else {
		key += "text:" + text
	}
	f.emitOnce(key, feedMessage{Role: feedRoleSystem, Type: feedTypeStatus, Text: text})
}

// partsForMessage returns the cached parts owned by msgID in insertion order (deterministic
// flush ordering — map iteration order is not stable).
func (f *opencodeFold) partsForMessage(msgID string) []*ocPartCache {
	var out []*ocPartCache
	for _, id := range f.partOrder {
		if ent := f.parts[id]; ent != nil && ent.messageID == msgID {
			out = append(out, ent)
		}
	}
	return out
}

// dropPart removes an emitted/duplicate cached part from the parts map. Its partOrder slot
// is left behind and skipped by later reads (the nil parts lookup in partsForMessage);
// partOrder growth is bounded by the session's total parts, like the emitted dedup set.
func (f *opencodeFold) dropPart(partID string) {
	delete(f.parts, partID)
}

// drainMessages returns and clears the produced-but-undrained feed messages.
func (f *opencodeFold) drainMessages() []feedMessage {
	if len(f.msgs) == 0 {
		return nil
	}
	out := f.msgs
	f.msgs = nil
	return out
}

func (f *opencodeFold) activity() Activity {
	if !f.confirmed {
		return ActivityUnknown
	}
	if len(f.pending) > 0 {
		return ActivityWorking
	}
	if f.lastBoundary == "idle" {
		return ActivityNeedsInput
	}
	return ActivityWorking
}

func (f *opencodeFold) settled() bool { return f.activity() == ActivityNeedsInput }

func (f *opencodeFold) lastMessage() string { return SanitizeLastMessage(f.lastMsg) }

// applyStatusFallback applies a REST-seed-derived status boundary as a FALLBACK: it establishes
// the activity boundary ONLY when the fold has NOT observed a live session.status/session.idle
// event (sawStatus). The live /event stream is authoritative and ordered; the REST /session/status
// snapshot is only for reconstructing a session that was quiescent at connect (§3.4) — it must
// never override a newer buffered live busy/idle. idle=true → needs_input; idle=false → working.
// Returns whether it actually set the boundary (so the caller can count it as a seed event).
func (f *opencodeFold) applyStatusFallback(idle bool) bool {
	if f.sawStatus {
		return false // a live status boundary was present: the live stream wins
	}
	f.confirmed = true
	if idle {
		f.lastBoundary = "idle"
	} else {
		f.lastBoundary = "busy"
	}
	return true
}

// noteGap drops the pending tool-call set — a gap (an SSE inbox overflow / dropped frame)
// may have swallowed a completed/error part, and a forever-pending callID would pin the
// verdict at working. It KEEPS the emitted-part dedup set (and the cached snapshots) so
// reseed idempotency survives a gap: after the transport forces a full reconnect+reseed,
// the replayed history must not re-emit rows it already produced.
func (f *opencodeFold) noteGap() {
	f.pending = map[string]bool{}
}

// reset clears ALL fold state. It exists to satisfy the activityFold contract (the file
// watchers call it on a truncation/rotation). The opencode SSE transport does NOT call
// reset on reconnect — it relies on the dedup set surviving so a re-seed emits no
// duplicate rows (a reconnect is a reseed, not a fresh session).
func (f *opencodeFold) reset() {
	*f = *newOpencodeFold()
}

// ---- helpers ----

// opencodeTS converts an opencode epoch-millis timestamp to a UTC RFC3339 string, or ""
// when there is no usable time (the ring then stamps it with reconcile-now on append). A
// non-positive value, or one whose expanded year falls outside RFC3339's 0001..9999 range,
// yields "" — an out-of-range year formats as a non-RFC3339 expanded-year string ("+12345-…"),
// which would corrupt the wire contract, so it is treated as "no usable time" instead.
func opencodeTS(ms int64) string {
	if ms <= 0 {
		return ""
	}
	t := time.UnixMilli(ms).UTC()
	if y := t.Year(); y < 1 || y > 9999 {
		return ""
	}
	return t.Format(time.RFC3339)
}

// bodyKey is the emit/dedup key for a text/reasoning part's body row. Shared by the cache
// (applyTextPart's already-emitted skip) and the emitter (tryEmitPart) so the two never drift.
func bodyKey(partID string) string { return partID + "|body" }

// firstNonZero returns the first non-zero value (0 when all are zero).
func firstNonZero(vals ...int64) int64 {
	for _, v := range vals {
		if v != 0 {
			return v
		}
	}
	return 0
}

// compactJSON renders a raw JSON value as compact (whitespace-stripped) text — used for a
// tool_use's state.input detail. Returns "" for an empty value; falls back to the trimmed
// raw bytes if the value is somehow not re-compactable.
func compactJSON(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var buf bytes.Buffer
	if json.Compact(&buf, raw) != nil {
		return strings.TrimSpace(string(raw))
	}
	return buf.String()
}
