package rc

import (
	"regexp"
	"strings"
)

// Activity is a session's live work dimension, orthogonal to the lifecycle State.
// State answers "is the session usable?" (starting/ready/needs-*/dead); Activity
// answers "what is a usable session doing right now?" (working/idle/waiting). It is
// derived live by the rc hub (Phase C) from JSONL tails and the pane-stability
// engine, and rendered only when lifecycle permits (see DisplayActivity). Absent /
// empty when the hub is not running or the kind is unsupported.
type Activity string

const (
	// ActivityWorking — the session is actively producing output (a JSONL turn is
	// streaming, or the pane changed since the last capture).
	ActivityWorking Activity = "working"
	// ActivityNeedsInput — the session is idle at a prompt anchor, waiting for the
	// operator to type (a stable pane matching the kind's PromptAnchor).
	ActivityNeedsInput Activity = "needs_input"
	// ActivityIdle — the session is quiescent with no prompt anchor visible
	// (finished, or a kind with no anchor sitting still).
	ActivityIdle Activity = "idle"
	// ActivityUnknown — a live session whose activity could not be determined yet
	// (e.g. correlation to a JSONL file is still ambiguous). Distinct from empty,
	// which means "no activity dimension at all" (hub absent / lifecycle trumps).
	ActivityUnknown Activity = "unknown"

	// ActivityNeedsApproval is RESERVED in the wire contract for a future
	// approval-gating dimension. No derivation produces it in this phase and no
	// client AC references it — approval handling remains "open the TUI" via the
	// needs-* lifecycle states. It is declared here so the enum's wire values are
	// fixed up front and a later phase can start emitting it without a contract bump.
	ActivityNeedsApproval Activity = "needs_approval"
)

// DisplayActivity applies the lifecycle-trumps-activity precedence rule: when the
// pane-derived State is a blocking lifecycle state (needs-trust / needs-auth / dead),
// the session's activity is suppressed — a dead or gated session has no meaningful
// "working/idle" dimension, and surfacing one would contradict the badge the client
// already shows. For every other state (starting/ready/reconnecting) the activity
// passes through unchanged. Returns the empty Activity ("") to signal "suppress —
// render no activity" so the omitempty DTO field drops out entirely. Suppression
// covers the WHOLE activity dimension: a consumer projecting these fields (e.g. the
// server's toSessionRC) must drop activity_at AND last_message alongside the
// activity — a bare timestamp is meaningless without its activity, and a stale
// last_message on a dead/gated row would present pre-death context as current.
func DisplayActivity(state State, activity Activity) Activity {
	switch state {
	case StateNeedsTrust, StateNeedsAuth, StateDead:
		return ""
	default:
		return activity
	}
}

// maxLastMessageRunes bounds a sanitized last-message preview. 200 runes is a
// one-to-two-line preview on a phone — enough to recognize the message, small enough
// to keep listing/SSE payloads tiny.
const maxLastMessageRunes = 200

// ansiEscapeRe strips terminal escape sequences from captured text: CSI sequences
// (`ESC [ … final`, e.g. colors/cursor moves), OSC sequences (`ESC ] … BEL|ST`, e.g.
// window titles / hyperlinks) — including an UNTERMINATED OSC that runs to the end
// of the string (a chunk-cut capture can truncate the sequence mid-payload; without
// the `\z` arm the raw payload text would leak into the preview) — and the
// standalone two-byte escapes (`ESC` + a single Fe byte, e.g. `ESC M`). Agent panes
// and JSONL previews can carry these; they must never reach a client. Applied before
// control-char stripping so an escape's intermediate bytes are consumed as a unit
// rather than left as stray punctuation.
var ansiEscapeRe = regexp.MustCompile(
	"\x1b\\[[0-?]*[ -/]*[@-~]" + // CSI: ESC [ params intermediates final
		"|\x1b\\][^\x07\x1b]*(?:\x07|\x1b\\\\|\\z)" + // OSC: ESC ] ... (BEL | ST | end-of-string)
		"|\x1b[@-Z\\\\-_]", // single-byte Fe escapes: ESC <0x40-0x5f>
)

// SanitizeLastMessage turns raw agent/JSONL text into a safe, compact one-line
// preview: strip ANSI escape sequences, drop remaining control characters (C0 except
// whitespace, DEL, and the C1 range — the same posture as HasUnsafePromptChars, so a
// smuggled CSI can't survive), collapse every run of whitespace to a single space,
// trim, and truncate to maxLastMessageRunes on a rune boundary (never mid-codepoint,
// so multi-byte text is not corrupted). The result is plain, single-line, bounded.
func SanitizeLastMessage(s string) string {
	s = ansiEscapeRe.ReplaceAllString(s, "")
	s = stripNonWhitespaceControls(s)
	// strings.Fields splits on Unicode whitespace (spaces, tabs, newlines, NBSP…)
	// and rejoins with a single ASCII space — collapse + trim in one step.
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > maxLastMessageRunes {
		return string(r[:maxLastMessageRunes])
	}
	return s
}

// stripNonWhitespaceControls removes control characters that are NOT whitespace,
// leaving whitespace controls (tab/newline/CR/VT/FF) in place for the later
// whitespace-collapse to fold. Dropped: C0 (< 0x20) other than whitespace, DEL
// (0x7f), and C1 (0x80–0x9f, e.g. the 8-bit CSI 0x9b a terminal would honor).
func stripNonWhitespaceControls(s string) string {
	return strings.Map(func(r rune) rune {
		switch r {
		case '\t', '\n', '\v', '\f', '\r':
			return r // whitespace — kept for collapse
		}
		if r < 0x20 || r == 0x7f || (r >= 0x80 && r <= 0x9f) {
			return -1 // drop
		}
		return r
	}, s)
}
