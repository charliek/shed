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
)

// Feed message role/type tokens (the wire contract's message shape). role ∈
// {user, assistant, tool, system}; type ∈ {text, tool_use, tool_result, reasoning,
// status}. The producers (codexFold) map their native events onto these.
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
)

// feedTool carries a tool call/result's name + a compact detail (the invocation
// arguments for tool_use, the output for tool_result). Both are sanitized/capped.
type feedTool struct {
	Name   string `json:"name,omitempty"`
	Detail string `json:"detail,omitempty"`
}

// feedMessage is one normalized conversation message in the ring / on the wire.
type feedMessage struct {
	Seq  uint64    `json:"seq"`
	TS   string    `json:"ts"`
	Role string    `json:"role"`
	Type string    `json:"type"`
	Text string    `json:"text,omitempty"`
	Tool *feedTool `json:"tool,omitempty"`
}

// size is the message's contribution to the ring's byte budget (text + tool fields).
func (m feedMessage) size() int {
	n := len(m.Text)
	if m.Tool != nil {
		n += len(m.Tool.Name) + len(m.Tool.Detail)
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
	cut := maxFeedMessageBytes
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut-- // back up to a rune boundary so a multi-byte codepoint is never split
	}
	return s[:cut] + feedTruncMarker
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
