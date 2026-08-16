package rc

import (
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// The message feed is the non-TUI view's data source: as the codex rollout watcher
// folds the JSONL turn stream (see watch_codex.go's codexFold) and the opencode
// watcher folds its HTTP/SSE event stream (see watch_opencode.go's opencodeFold), each
// also emits normalized conversation messages, which the reconcile loop drains into a
// per-session ring buffer here. GET /v1/sessions/{slug}/messages pages that ring;
// message.appended SSE events notify subscribers a new message landed (the body is
// fetched from /messages — the notification stays tiny). claude feeds activity only
// in this phase, so its sessions have a ring that simply never fills.
//
// Bounds (the wire contract): each message's text (and a tool block's detail) is
// sanitized and capped at 8 KiB; the per-session ring holds at most 500 messages AND
// 1 MiB of text total, dropping the oldest first. seq is monotonic per hub run,
// starting at 1 — it restarts from 1 when the hub restarts, so a client that sees a
// seq lower than one it already holds does a full refetch.

const (
	// maxFeedMessageBytes caps one message's text (and one tool block's detail) after
	// sanitization. 8 KiB preserves far more than the 200-rune last_message preview
	// (a full code block or tool output reads intact) while keeping a single message
	// bounded. A longer value is truncated with feedTruncMarker appended.
	maxFeedMessageBytes = 8 << 10 // 8 KiB
	// maxRingMessages caps the per-session ring by count (drop-oldest past it).
	maxRingMessages = 500
	// maxRingBytes caps the per-session ring by total text bytes (drop-oldest past it).
	maxRingBytes = 1 << 20 // 1 MiB
	// feedTruncMarker is appended to a message text (or tool detail) truncated at the
	// byte cap, so a client can tell a preview from a complete message.
	feedTruncMarker = "…[truncated]"

	// defaultMessagesLimit / maxMessagesLimit bound a /messages page (the wire
	// contract: default 100, hard cap 200).
	defaultMessagesLimit = 100
	maxMessagesLimit     = 200

	// maxApprovalTokenBytes caps each identifier-shaped approval field (the id and
	// every advertised decision token) at the length of the id's wire grammar
	// (^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$ — the contract's 128-char ceiling, wide
	// enough for the native ids lanes carry: codex call ids, ACP/opencode request
	// ids). The grammar itself is enforced at the hub's approval handler (where a
	// bad id is a 4xx); the ring only BOUNDS what a producer hands it, so a
	// misbehaving lane adapter can never inflate the ring's byte budget.
	maxApprovalTokenBytes = 128
	// maxApprovalDecisions caps how many advertised decisions one approval row may
	// carry. The decision vocabulary is a fixed, tiny enum (allow/allow_always/
	// deny); the cap exists so the slice cannot be used as unbounded payload.
	maxApprovalDecisions = 8
)

// Feed message role/type tokens (the wire contract's message shape). role ∈
// {user, assistant, tool, system}; type ∈ {text, tool_use, tool_result, reasoning,
// status, approval_request}. The producers (codexFold) map their native events onto
// these.
const (
	feedRoleUser      = "user"
	feedRoleAssistant = "assistant"
	feedRoleTool      = "tool"
	feedRoleSystem    = "system"

	feedTypeText       = "text"
	feedTypeToolUse    = "tool_use"
	feedTypeToolResult = "tool_result"
	feedTypeReasoning  = "reasoning"
	feedTypeStatus     = "status"
	// feedTypeApprovalRequest is an approval row: an agent asked for permission to
	// do something. It rides role `tool` with `text` carrying the sanitized
	// human-readable summary, `tool{name,detail}` the call being approved, and
	// `approval` the machine-readable state. Nothing produces it in this phase (no
	// lane emits approvals yet) — the shape is contracted now so a lane can start
	// emitting it without recontracting clients.
	feedTypeApprovalRequest = "approval_request"
)

// Approval status / decision tokens (the wire contract's approval vocabulary).
const (
	approvalStatusPending  = "pending"
	approvalStatusResolved = "resolved"

	approvalDecisionAllow       = "allow"
	approvalDecisionAllowAlways = "allow_always"
	approvalDecisionDeny        = "deny"
)

// feedTool carries a tool call/result's name + a compact detail (the invocation
// arguments for tool_use, the output for tool_result). Both are sanitized/capped.
type feedTool struct {
	Name   string `json:"name,omitempty"`
	Detail string `json:"detail,omitempty"`
}

// FeedApproval is the machine-readable state of an approval request carried by an
// `approval_request` feed row (and, once a lane produces them, by a session's
// pending_approvals snapshot — hence exported: it is part of the Session DTO).
//
// CLIENT FOLDING RULE: approval rows are an id-keyed, LAST-WRITE-WINS stream. A
// resolution is a SECOND appended row with the same id and status "resolved" — never
// an edit of the first. A client must not require having seen the `pending` row
// before the `resolved` one: ring eviction (or a hub restart) can drop the earlier
// row entirely, and the session's pending_approvals snapshot is the authoritative
// answer to "what is still open".
type FeedApproval struct {
	// ID is the lane-assigned approval id — the address the approval verb resolves
	// (POST /v1/sessions/{slug}/approvals/{id}). Grammar:
	// ^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$.
	ID string `json:"id"`
	// Status is "pending" or "resolved".
	Status string `json:"status"`
	// Decision is the decision that resolved it (empty while pending).
	Decision string `json:"decision,omitempty"`
	// Decisions are the decisions this request accepts, advertised per request so a
	// client renders exactly the buttons the lane will honor (a subset of
	// allow/allow_always/deny).
	Decisions []string `json:"decisions,omitempty"`
}

// sanitize bounds an approval's fields before it enters the ring: every field is a
// single-token value (never prose), so each is run through the token sanitizer
// (ANSI + ALL control/whitespace stripping — unlike feed text, a token has no
// legitimate newlines or tabs), the id is capped at the grammar's length ceiling,
// and the advertised decision list is capped in count. Validity (the id grammar,
// the decision enum) is the producing/handling layer's job; this is the ring's
// byte-budget guard, so no producer can inflate the ring past its accounted size.
func (a *FeedApproval) sanitize() {
	a.ID = truncateBytes(sanitizeFeedToken(a.ID), maxApprovalTokenBytes)
	a.Status = truncateBytes(sanitizeFeedToken(a.Status), maxApprovalTokenBytes)
	a.Decision = truncateBytes(sanitizeFeedToken(a.Decision), maxApprovalTokenBytes)
	if len(a.Decisions) > maxApprovalDecisions {
		a.Decisions = a.Decisions[:maxApprovalDecisions]
	}
	for i := range a.Decisions {
		a.Decisions[i] = truncateBytes(sanitizeFeedToken(a.Decisions[i]), maxApprovalTokenBytes)
	}
}

// sanitizeFeedToken strips ANSI escapes plus EVERY control and whitespace rune from
// a single-token approval field (id/status/decision). Feed text keeps newlines and
// tabs (multi-line prose is content there); a token that contains them is malformed,
// and preserving them would let a crafted value smuggle separators into a field the
// contract defines as one token.
func sanitizeFeedToken(s string) string {
	if s == "" {
		return ""
	}
	s = ansiEscapeRe.ReplaceAllString(s, "")
	return strings.Map(func(r rune) rune {
		if r <= 0x20 || r == 0x7f || (r >= 0x80 && r <= 0x9f) {
			return -1
		}
		return r
	}, s)
}

// size is the approval's contribution to the ring's byte budget (id + status +
// decision + every advertised decision).
func (a *FeedApproval) size() int {
	n := len(a.ID) + len(a.Status) + len(a.Decision)
	for _, d := range a.Decisions {
		n += len(d)
	}
	return n
}

// feedMessage is one normalized conversation message in the ring / on the wire.
type feedMessage struct {
	Seq  uint64    `json:"seq"`
	TS   string    `json:"ts"`
	Role string    `json:"role"`
	Type string    `json:"type"`
	Text string    `json:"text,omitempty"`
	Tool *feedTool `json:"tool,omitempty"`
	// Approval is set on (and only on) an `approval_request` row.
	Approval *FeedApproval `json:"approval,omitempty"`
}

// size is the message's contribution to the ring's byte budget (text + tool +
// approval fields — every string that rides the row is accounted, so the 1 MiB
// budget stays honest for an approval-heavy feed).
func (m feedMessage) size() int {
	n := len(m.Text)
	if m.Tool != nil {
		n += len(m.Tool.Name) + len(m.Tool.Detail)
	}
	if m.Approval != nil {
		n += m.Approval.size()
	}
	return n
}

// messageRing is one session's bounded, drop-oldest feed. seq is assigned on append
// (monotonic from 1). Safe for concurrent use: reconcile appends while /messages and
// the SSE notifier read.
type messageRing struct {
	mu    sync.Mutex
	msgs  []feedMessage
	seq   uint64 // last assigned seq
	bytes int    // running sum of msgs' size()
}

func newMessageRing() *messageRing { return &messageRing{} }

// append sanitizes m, assigns the next seq, stores it, and drops the oldest messages
// until both caps hold. now stamps ts when the message carries none. Returns the
// assigned seq.
func (r *messageRing) append(m feedMessage, now time.Time) uint64 {
	r.mu.Lock()
	defer r.mu.Unlock()

	m.Text = sanitizeFeedText(m.Text)
	if m.Tool != nil {
		m.Tool.Name = sanitizeFeedText(m.Tool.Name)
		m.Tool.Detail = sanitizeFeedText(m.Tool.Detail)
	}
	if m.Approval != nil {
		// Stored as a COPY (with its own decisions slice) rather than the producer's
		// pointer: an approval's state changes by appending a second row, so a
		// producer that reuses/mutates its struct must never be able to rewrite a row
		// already accounted in r.bytes.
		a := *m.Approval
		// Cap the decision COUNT before copying: copying first would allocate (and
		// retain, via the stored row) a producer-sized backing array that the ring's
		// byte budget never accounts for.
		src := a.Decisions
		if len(src) > maxApprovalDecisions {
			src = src[:maxApprovalDecisions]
		}
		a.Decisions = append([]string(nil), src...)
		a.sanitize()
		m.Approval = &a
	}
	if m.TS == "" {
		m.TS = now.UTC().Format(time.RFC3339)
	}
	r.seq++
	m.Seq = r.seq
	r.msgs = append(r.msgs, m)
	r.bytes += m.size()

	// Drop-oldest until within BOTH caps. Never drop the just-appended sole message
	// (a lone message that alone exceeds the byte cap is impossible after the 8 KiB
	// per-field caps, but the guard keeps the ring from going empty regardless).
	for len(r.msgs) > 1 && (len(r.msgs) > maxRingMessages || r.bytes > maxRingBytes) {
		r.bytes -= r.msgs[0].size()
		r.msgs = r.msgs[1:]
	}
	return m.Seq
}

// since returns up to limit messages with seq > sinceSeq (exclusive), oldest first,
// plus truncated=true in two cursor-misalignment cases the caller must treat as
// "refetch from scratch":
//
//   - sinceSeq predates the ring's earliest retained message (drop-oldest discarded
//     messages the caller has not seen);
//   - sinceSeq points BEYOND the ring's latest assigned seq — the cursor came from a
//     previous ring incarnation (a hub restart or session recreate restarts seq at 1),
//     so a poll-only client would otherwise sit on empty pages forever, silently
//     misaligned.
//
// limit is clamped to the page bounds.
func (r *messageRing) since(sinceSeq uint64, limit int) (msgs []feedMessage, truncated bool) {
	if limit <= 0 {
		limit = defaultMessagesLimit
	}
	if limit > maxMessagesLimit {
		limit = maxMessagesLimit
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	// Beyond-tail cursor. This also absorbs the uint64 edge: any sinceSeq > r.seq
	// (MaxUint64 included) is a stale cursor from another incarnation, answered
	// before any arithmetic that could wrap.
	if sinceSeq > r.seq {
		return nil, true
	}
	if len(r.msgs) > 0 {
		earliest := r.msgs[0].Seq
		// The caller expects sinceSeq+1 next; if that predates the earliest retained
		// seq, the ring dropped messages between them. Compared as `sinceSeq <
		// earliest-1` (earliest is always >= 1) so no uint64 addition can overflow.
		if sinceSeq < earliest-1 {
			truncated = true
		}
	}
	for _, m := range r.msgs {
		if m.Seq <= sinceSeq {
			continue
		}
		msgs = append(msgs, m)
		if len(msgs) >= limit {
			break
		}
	}
	return msgs, truncated
}

// sanitizeFeedText strips ANSI escape sequences and non-whitespace control characters
// from raw agent/JSONL text, then caps it at maxFeedMessageBytes on a rune boundary
// (appending feedTruncMarker when it truncates). Unlike SanitizeLastMessage it
// PRESERVES newlines and internal whitespace — a feed message keeps its structure (a
// code block, multi-line tool output) rather than collapsing to a one-line preview.
func sanitizeFeedText(s string) string {
	if s == "" {
		return ""
	}
	s = ansiEscapeRe.ReplaceAllString(s, "")
	s = stripNonWhitespaceControls(s)
	if len(s) <= maxFeedMessageBytes {
		return s
	}
	return truncateBytes(s, maxFeedMessageBytes) + feedTruncMarker
}

// truncateBytes caps s at n bytes on a rune boundary (never mid-codepoint). It appends
// no marker of its own: sanitizeFeedText adds one for prose, while the identifier
// fields (approval ids, enum tokens) it also guards would only be muddied by a marker
// inside the value.
func truncateBytes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n-- // back up to a rune boundary so a multi-byte codepoint is never split
	}
	return s[:n]
}

// hubMessagesResponse is the GET /v1/sessions/{slug}/messages body.
type hubMessagesResponse struct {
	Messages  []feedMessage `json:"messages"`
	Truncated bool          `json:"truncated"`
}

// inputGatedKind reports whether a kind exposes the gated feed-input surface
// (kind_features.input == "gated"). Only codex and opencode in this phase — the
// message feed + POST /input cover those two kinds; other kinds keep TUI-only input
// (`post_input`).
func inputGatedKind(k Kind) bool {
	return k == KindCodex || k == KindOpencode
}

// trimFeedText is a small helper used by producers to drop leading/trailing
// whitespace on a captured message before it enters the ring (the sanitizer keeps
// internal newlines; producers just tidy the ends).
func trimFeedText(s string) string {
	return strings.Trim(s, " \t\n\r")
}
