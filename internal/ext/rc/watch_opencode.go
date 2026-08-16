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
//   - permission.asked  → an OPEN APPROVAL: a tracked pending entry (which drives the
//     activity verdict to needs_approval) plus an `approval_request` feed row carrying the
//     addressable id and the decisions the lane honors. permission.replied closes the entry
//     and appends the matching resolved row.
//   - question.asked / question.replied / question.rejected  → a display-only `status` feed
//     row (role system) as before, but an open question DOES count toward needs_approval.
//     Questions never enter pending_approvals: their answer vocabulary is not the
//     allow/allow_always/deny decision enum, so they are not addressable by the approvals
//     verb (remote question-answering is a future contract extension). needs_approval with
//     an empty pending_approvals is therefore a legal, expected state.
//
// Both asks are folded only when they carry meaningful content (a non-empty permission kind /
// at least one non-empty question); an empty/null-properties ask is ignored rather than
// emitting a hollow row.
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
	SessionID  string          `json:"sessionID"`
	Status     *ocStatus       `json:"status"`     // session.status
	Info       *ocMessageInfo  `json:"info"`       // message.updated
	Part       *ocPart         `json:"part"`       // message.part.updated
	Time       int64           `json:"time"`       // message.part.updated update time (epoch ms)
	ID         string          `json:"id"`         // permission.asked (per_…) / question.asked (que_…)
	Permission string          `json:"permission"` // permission.asked kind ("bash", "edit", …)
	Patterns   []string        `json:"patterns"`   // permission.asked matched commands/globs
	Metadata   *ocPermMetadata `json:"metadata"`   // permission.asked call detail (metadata.command)
	Questions  []ocQuestion    `json:"questions"`  // question.asked

	// RequestID / Reply are the reply-event fields (permission.replied,
	// question.replied/rejected): opencode addresses the resolved request by requestID and
	// names its native reply ("once"|"always"|"reject"). `id` is accepted as a tolerant
	// fallback address (see replyTarget) — a missed resolution would strand an approval
	// pending forever, which is the one failure mode worth being generous about. The
	// question answers[] payload is deliberately NOT read: the fold only needs to know the
	// question closed.
	RequestID string `json:"requestID"`
	Reply     string `json:"reply"`

	// These four ride the hub-SYNTHESIZED approval-seed marker only (ocApprovalSeedType,
	// pushed by the REST seed — see applyApprovalSeed). No opencode event carries them, and
	// the live-stream routing whitelist (foldRelevantType) does not admit the marker's type,
	// so they can only ever arrive from our own seed path. Each *Known flag says its half's
	// REST read succeeded — only then is that half's id list authoritative.
	PermissionIDs    []string `json:"permissionIDs"`
	QuestionIDs      []string `json:"questionIDs"`
	PermissionsKnown bool     `json:"permissionsKnown"`
	QuestionsKnown   bool     `json:"questionsKnown"`
}

// ocApprovalSeedType is the type of the hub-synthesized envelope the REST seed pushes AFTER
// the individual permission.asked/question.asked replays: it carries the authoritative set of
// asks still open on the server, which is what lets the fold retire an ask that was answered
// while the stream was down (applyApprovalSeed). Namespaced under `shed.` because it is ours,
// not opencode's.
const ocApprovalSeedType = "shed.approval.seed"

// ocPermMetadata is permission.asked's metadata bag. Only the command is read — it is the
// tool detail on the approval row (what the operator is being asked to allow).
type ocPermMetadata struct {
	Command string `json:"command"`
}

// replyTarget is the approval id a replied/rejected event addresses. opencode carries it as
// requestID; `id` is a tolerant fallback so a spelling difference cannot strand a pending
// entry (and with it, a stuck needs_approval verdict).
func (p *ocProperties) replyTarget() string {
	if p.RequestID != "" {
		return p.RequestID
	}
	return p.ID
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

	// pendingPerms tracks permission asks by their opencode id (per_…); permOrder keeps ask
	// order for the pending_approvals snapshot. A RESOLVED entry is RETAINED (not deleted):
	// the approvals verb must distinguish an id it never saw (404 unknown_approval) from one
	// already answered (same-decision replay is idempotent, a different decision is 409
	// already_resolved) — see approvalState. Growth is bounded by the session's total asks,
	// like the emitted dedup set.
	pendingPerms map[string]*ocPendingPerm
	permOrder    []string
	// pendingQuestions holds the OPEN question.asked ids → their summary text. Questions
	// count toward needs_approval but never enter pending_approvals (see the file doc).
	pendingQuestions map[string]string
}

// ocPendingPerm is one permission ask: the summary text it produced (so the resolved row can
// repeat it) plus its resolution state. Keyed by the opencode request id (per_… — the approvals
// verb's address); the ask's kind/detail ride the pending row only and are not retained.
type ocPendingPerm struct {
	text     string // the sanitized human-readable summary carried by both rows
	resolved bool
	decision string // the contract decision that resolved it ("" when resolved outside the hub)
}

func newOpencodeFold() *opencodeFold {
	return &opencodeFold{
		pending:          map[string]bool{},
		msgRole:          map[string]string{},
		msgCreated:       map[string]int64{},
		msgCompleted:     map[string]int64{},
		parts:            map[string]*ocPartCache{},
		emitted:          map[string]bool{},
		pendingPerms:     map[string]*ocPendingPerm{},
		pendingQuestions: map[string]string{},
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
		return f.applyPermissionAsked(&props)
	case "permission.replied":
		// Map opencode's native reply back onto the contract's decision enum. An
		// unrecognized/absent reply still CLOSES the ask (with no decision): the request is
		// demonstrably answered, and a stuck entry would pin needs_approval forever.
		return f.resolvePermission(props.replyTarget(), opencodeDecisionFromReply(props.Reply))
	case "question.asked":
		return f.applyQuestionAsked(&props)
	case "question.replied", "question.rejected":
		return f.clearQuestion(props.replyTarget())
	case ocApprovalSeedType:
		return f.applyApprovalSeed(&props)
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

// ---- approvals (permission asks + questions) ----

// applyPermissionAsked tracks a permission ask and emits its PENDING approval_request row.
//
// An ask with NO id stays on the pre-approvals display-only status-row path: the id is both
// the row's wire address (FeedApproval.ID, which the approvals verb resolves) and the key a
// permission.replied clears, so an id-less ask could never be answered remotely NOR retired —
// tracking it would pin needs_approval forever on a session nothing can unblock.
//
// A re-ask of an id already tracked is usually a reseed replay and changes nothing — with ONE
// exception, the REOPEN rule: an entry resolved with an EMPTY decision was closed by
// applyApprovalSeed (or by a reply whose vocabulary we could not read), i.e. it was retired on
// the evidence "the server no longer lists it". A later ask for that same id is NEWER, stronger
// evidence that it IS open — a stale/racing REST snapshot retired it wrongly — so the entry is
// reopened and its rows re-announced (both dedup slots are cleared, since the client needs the
// pending row again to render the buttons). An entry resolved with a KNOWN decision stays
// closed: a real reply was observed for it, and no ask replay outranks that.
func (f *opencodeFold) applyPermissionAsked(props *ocProperties) bool {
	// Require a meaningful permission kind: absent/null properties (or an empty kind) must
	// not fabricate a hollow "awaiting approval:" row.
	if props.Permission == "" {
		return false
	}
	text := "awaiting approval: " + props.Permission
	if len(props.Patterns) > 0 {
		text += " — " + strings.Join(props.Patterns, ", ")
	}
	if props.ID == "" {
		f.emitStatusRow("perm", "", text)
		return true
	}
	if ent, seen := f.pendingPerms[props.ID]; seen {
		if !ent.resolved || ent.decision != "" {
			return false // still open, or genuinely answered: a replay is not a state change
		}
		// REOPEN (see the doc): drop the resolution and both dedup slots so the pending row is
		// re-announced and a later real resolution can emit its own row.
		ent.resolved = false
		ent.text = text
		delete(f.emitted, permKey(props.ID, approvalStatusPending))
		delete(f.emitted, permKey(props.ID, approvalStatusResolved))
	} else {
		f.pendingPerms[props.ID] = &ocPendingPerm{text: text}
		f.permOrder = append(f.permOrder, props.ID)
	}
	var detail string
	if props.Metadata != nil {
		detail = props.Metadata.Command
	}
	// An open approval IS the activity verdict now (needs_approval), so — unlike the old
	// display-only row — an ask is activity-relevant evidence and confirms the fold.
	f.confirmed = true
	f.emitOnce(permKey(props.ID, approvalStatusPending), feedMessage{
		Role: feedRoleTool,
		Type: feedTypeApprovalRequest,
		Text: text,
		Tool: &feedTool{Name: props.Permission, Detail: detail},
		Approval: &FeedApproval{
			ID:        props.ID,
			Status:    approvalStatusPending,
			Decisions: opencodeDecisions(),
		},
	})
	return true
}

// resolvePermission marks an open ask resolved with decision (which may be "" when the answer
// happened outside the hub) and appends the RESOLVED approval row.
//
// IDEMPOTENT BY DESIGN, and that is load-bearing: the approvals verb marks an entry resolved
// synchronously the moment its POST succeeds (so a same-decision replay cannot re-POST), and
// opencode's own permission.replied event for the same id arrives on the stream moments later.
// Exactly one resolved row must reach the feed, so the second call is a no-op.
//
// A reply for an id this fold never saw asked (the ask predates the watcher, or a gap swallowed
// it) records a resolved TOMBSTONE rather than being dropped: without it a later ask replay —
// a reseed racing the reply, or history the server had not yet dropped — would open a PENDING
// entry for a permission that is demonstrably answered, stranding the session at
// needs_approval. The tombstone makes that replay hit the resolved-with-a-known-decision no-op
// above. Its resolved row still goes out (the client folding rule explicitly allows a resolved
// row with no pending row before it).
//
// Accepted, not fixed: an entry the approvals verb resolved that a reconnect's GET /permission
// still lists stays resolved here — the ask replay's reopen rule does not apply to it because
// its decision is known. That contradicts a reply the server acknowledged, and it self-heals
// the moment the TUI drops the request from its store.
func (f *opencodeFold) resolvePermission(id, decision string) bool {
	if id == "" {
		return false
	}
	ent := f.pendingPerms[id]
	switch {
	case ent == nil:
		ent = &ocPendingPerm{text: "approval resolved"}
		f.pendingPerms[id] = ent
		f.permOrder = append(f.permOrder, id)
	case ent.resolved:
		return false
	}
	ent.resolved = true
	ent.decision = decision
	f.emitOnce(permKey(id, approvalStatusResolved), feedMessage{
		Role:     feedRoleTool,
		Type:     feedTypeApprovalRequest,
		Text:     ent.text,
		Approval: &FeedApproval{ID: id, Status: approvalStatusResolved, Decision: decision},
	})
	return true
}

// applyQuestionAsked emits the display-only status row for a question and, when the question
// is addressable (it carries an id), tracks it as open so it counts toward needs_approval. An
// id-less question is display-only for the same reason an id-less permission is: nothing could
// ever clear it.
func (f *opencodeFold) applyQuestionAsked(props *ocProperties) bool {
	// Require at least one question with real text: no questions (or an empty question) must
	// not fabricate a hollow "awaiting answer:" row.
	if len(props.Questions) == 0 {
		return false
	}
	hdr := firstNonEmpty(props.Questions[0].Header, props.Questions[0].Text)
	if hdr == "" {
		return false
	}
	text := "awaiting answer: " + hdr
	f.emitStatusRow("ques", props.ID, text)
	if props.ID != "" {
		if _, seen := f.pendingQuestions[props.ID]; !seen {
			f.pendingQuestions[props.ID] = text
			f.confirmed = true // an open question is the needs_approval verdict (see applyPermissionAsked)
		}
	}
	return true
}

// clearQuestion retires an open question (question.replied / question.rejected). No feed row:
// the ask's row was display-only, and questions carry no approval state to resolve.
func (f *opencodeFold) clearQuestion(id string) bool {
	if id == "" {
		return false
	}
	if _, open := f.pendingQuestions[id]; !open {
		return false
	}
	delete(f.pendingQuestions, id)
	return true
}

// applyApprovalSeed reconciles the fold's open asks against the REST seed's AUTHORITATIVE view
// of what is still open on the server (GET /permission + GET /question, pin-filtered). Every
// reconnect reseeds, which is what makes this the SELF-HEALING path: a permission.replied or
// question.replied lost to a disconnect or an inbox gap leaves a locally-open entry the server
// no longer lists, and it is retired here. Such a permission is closed with NO decision — the
// answer was given in the TUI, and the hub cannot know which way it went.
//
// The two halves carry INDEPENDENT authority (permsKnown / questionsKnown): a failed REST read
// must never be mistaken for "nothing is open", which would retire live approvals — but neither
// may it block the other's healing. GET /permission succeeding while GET /question fails still
// retires answered permissions, and vice versa.
func (f *opencodeFold) applyApprovalSeed(props *ocProperties) bool {
	changed := false
	if props.PermissionsKnown {
		openPerms := idSet(props.PermissionIDs)
		for _, id := range f.permOrder {
			// resolvePermission is the guard: an entry already resolved reports false.
			if !openPerms[id] && f.resolvePermission(id, "") {
				changed = true
			}
		}
	}
	if props.QuestionsKnown {
		openQuestions := idSet(props.QuestionIDs)
		for id := range f.pendingQuestions {
			if !openQuestions[id] {
				delete(f.pendingQuestions, id)
				changed = true
			}
		}
	}
	return changed
}

// idSet lifts an id list to a membership set.
func idSet(ids []string) map[string]bool {
	set := make(map[string]bool, len(ids))
	for _, id := range ids {
		set[id] = true
	}
	return set
}

// openApprovals counts what the session is BLOCKED on: unresolved permission asks plus open
// questions. Both drive activity needs_approval; only the permissions are addressable.
func (f *opencodeFold) openApprovals() int {
	n := len(f.pendingQuestions)
	for _, ent := range f.pendingPerms {
		if !ent.resolved {
			n++
		}
	}
	return n
}

// approvalState reports a tracked ask's status/decision. ok=false means the id is unknown to
// this fold — the approvals verb's 404 unknown_approval. This, NOT the pending_approvals
// snapshot, is the resolution-state oracle: the snapshot is pending-only by wire contract.
func (f *opencodeFold) approvalState(id string) (status, decision string, ok bool) {
	ent := f.pendingPerms[id]
	if ent == nil {
		return "", "", false
	}
	if ent.resolved {
		return approvalStatusResolved, ent.decision, true
	}
	return approvalStatusPending, "", true
}

// pendingApprovals renders the still-open permission asks as the wire's pending_approvals
// snapshot, in ask order. Freshly allocated on every call (the decisions slice included) so a
// consumer can hand it straight to the session DTO without aliasing fold state.
func (f *opencodeFold) pendingApprovals() []FeedApproval {
	var out []FeedApproval
	for _, id := range f.permOrder {
		ent := f.pendingPerms[id]
		if ent == nil || ent.resolved {
			continue
		}
		out = append(out, FeedApproval{
			ID:        id,
			Status:    approvalStatusPending,
			Decisions: opencodeDecisions(),
		})
	}
	return out
}

// opencodeDecisions is the decision set opencode's session-scoped reply route honors
// (once/always/reject), in contract spelling. A FRESH slice per call: every copy of it ends up
// on the wire in a row or a snapshot that must not alias another's.
func opencodeDecisions() []string {
	return []string{approvalDecisionAllow, approvalDecisionAllowAlways, approvalDecisionDeny}
}

// permKey is the emit/dedup key for one phase (pending|resolved) of an approval row. Distinct
// from emitStatusRow's "perm|id:<id>" key so the two paths can never collide.
func permKey(id, phase string) string { return "perm|id:" + id + "|" + phase }

// opencodeDecisionFromReply maps opencode's native permission reply vocabulary onto the
// contract's decision enum (the inverse of what the approvals verb sends: allow→once,
// allow_always→always, deny→reject). An unrecognized/absent reply maps to "" — "resolved,
// decision unknown", which still closes the ask (see the permission.replied arm).
func opencodeDecisionFromReply(reply string) string {
	switch reply {
	case "once":
		return approvalDecisionAllow
	case "always":
		return approvalDecisionAllowAlways
	case "reject":
		return approvalDecisionDeny
	default:
		return ""
	}
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
	// needs_approval is checked BEFORE the pending-tool arm: the tool call that triggered the
	// ask is still open (opencode holds it) so the pending set says "working", but the session
	// is blocked on the operator, not on the model. The block is what the client must see.
	if f.openApprovals() > 0 {
		return ActivityNeedsApproval
	}
	if len(f.pending) > 0 {
		return ActivityWorking
	}
	if f.lastBoundary == "idle" {
		return ActivityNeedsInput
	}
	return ActivityWorking
}

// settled reports an EVENT-BOUNDED end state — one that stays true until an event changes it,
// so the transport trusts it indefinitely while the stream is healthy (snapshot's freshness
// rule) instead of expiring it after the 30 s window. needs_approval qualifies alongside
// needs_input: an open ask is cleared only by a replied/rejected event or a reseed, never by
// the passage of time. (A DEAD stream is the deliberate exception — see snapshot: an unhealthy
// transport reports not-fresh and pane stability drives, so a needs_approval derived from a
// wedged connection cannot outlive the evidence for it.)
func (f *opencodeFold) settled() bool {
	switch f.activity() {
	case ActivityNeedsInput, ActivityNeedsApproval:
		return true
	default:
		return false
	}
}

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
//
// The open-approval state is deliberately KEPT too: unlike a tool call, an approval that the
// gap may have swallowed a reply for is retired authoritatively by the reseed's approval-seed
// marker (applyApprovalSeed), so clearing it here would only blink needs_approval off and back
// on for the sessions whose asks are genuinely still open.
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
