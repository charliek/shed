//! The feed vocabulary and the text sanitizers every lane adapter's fold emits
//! through — the contract-side half of a transcript row.
//!
//! These were `shed-opencode`'s `helpers.rs` (itself a copy of the rc hub's
//! `rc_hub::messages`) until the **second** adapter needed them. An
//! adapter-to-adapter dependency (`shed-gx` reaching into `shed-opencode` for a
//! sanitizer) would make one adapter's release schedule the other's, so the
//! shared half moved down here instead, beside the contract whose rows it
//! bounds. `shed-opencode` re-exports every item, so its own call sites and its
//! goldens are untouched.
//!
//! **`regex` was already a `shed-core` dependency** (`rc.rs`'s claude.ai URL
//! extraction), which is why the ANSI stripper could move without adding a crate
//! to the FFI or Android trees — the invariant plan 017 §3.1 #10 pins.
//!
//! Nothing here is I/O and nothing here is agent-specific: an adapter decides
//! WHICH row to emit, this module decides what a row is allowed to contain.

use std::sync::LazyLock;

use regex::Regex;

// ---- feed vocabulary ----

// Feed message role/type tokens (the wire contract's message shape). role ∈
// {user, assistant, tool, system}; type ∈ {text, tool_use, tool_result,
// reasoning, status, approval_request}.

/// A row the human typed.
pub const FEED_ROLE_USER: &str = "user";
/// A row the agent produced.
pub const FEED_ROLE_ASSISTANT: &str = "assistant";
/// A row about a tool call (the call, or its result).
pub const FEED_ROLE_TOOL: &str = "tool";
/// A row the adapter synthesized about the session itself.
pub const FEED_ROLE_SYSTEM: &str = "system";

/// Prose.
pub const FEED_TYPE_TEXT: &str = "text";
/// A tool invocation.
pub const FEED_TYPE_TOOL_USE: &str = "tool_use";
/// A tool invocation's result.
pub const FEED_TYPE_TOOL_RESULT: &str = "tool_result";
/// The agent's own thinking, when it publishes it separately from its prose.
pub const FEED_TYPE_REASONING: &str = "reasoning";
/// A synthesized status line (a turn ending, a stop reason).
pub const FEED_TYPE_STATUS: &str = "status";
/// An approval row: an agent asked for permission to do something. It rides
/// role `tool` with `text` carrying the sanitized human-readable summary,
/// `tool{name,detail}` the call being approved, and `approval` the
/// machine-readable state. A resolution is a SECOND row with the same id and
/// status "resolved" — never an edit of the first.
pub const FEED_TYPE_APPROVAL_REQUEST: &str = "approval_request";

// Approval status / decision tokens (the wire contract's approval vocabulary).
// These are the FEED row's spelling — [`crate::lane::LaneApprovalStatus`] and
// [`crate::lane::LaneDecision`] are the contract's, and a fold speaks both (a
// feed row carries the former, an approval row the latter).

/// The feed row's spelling for an approval still waiting on the human.
pub const APPROVAL_STATUS_PENDING: &str = "pending";
/// The feed row's spelling for an approval that is over. The feed has no
/// `submitted` — [`crate::lane::LaneApprovalStatus::Submitted`] is the
/// approval DTO's optimistic middle and does not exist on a transcript row.
pub const APPROVAL_STATUS_RESOLVED: &str = "resolved";

/// The feed row's spelling for "allowed this once".
pub const APPROVAL_DECISION_ALLOW: &str = "allow";
/// The feed row's spelling for "allowed this and every match".
pub const APPROVAL_DECISION_ALLOW_ALWAYS: &str = "allow_always";
/// The feed row's spelling for "refused".
pub const APPROVAL_DECISION_DENY: &str = "deny";

// ---- text hygiene ----

/// Strips terminal escape sequences from captured text: CSI sequences
/// (`ESC [ … final`), OSC sequences (`ESC ] … BEL|ST`) — including an
/// UNTERMINATED OSC that runs to the end of the string (a chunk-cut capture can
/// truncate the sequence mid-payload) — and the standalone two-byte escapes
/// (`ESC` + a single Fe byte). Applied before control-char stripping so an
/// escape's intermediate bytes are consumed as a unit rather than left as stray
/// punctuation.
static ANSI_ESCAPE_RE: LazyLock<Regex> = LazyLock::new(|| {
    Regex::new(r"\x1b\[[0-?]*[ -/]*[@-~]|\x1b\][^\x07\x1b]*(?:\x07|\x1b\\|\z)|\x1b[@-Z\\-_]")
        .expect("ANSI escape regex compiles")
});

/// Removes control characters that are NOT whitespace, leaving whitespace
/// controls (tab/newline/CR/VT/FF) in place. Dropped: C0 (< 0x20) other than
/// whitespace, DEL (0x7f), and C1 (0x80–0x9f, e.g. the 8-bit CSI 0x9b a terminal
/// would honor).
fn strip_non_whitespace_controls(s: &str) -> String {
    // `with_capacity` + `extend` rather than `collect`: a filtered iterator's
    // `size_hint` lower bound is 0, so `collect` grows the String by doubling
    // (~13 reallocations for an 8 KiB row). This runs on every feed row's text
    // and on both tool fields, so the exact upper bound is worth taking.
    let mut out = String::with_capacity(s.len());
    out.extend(s.chars().filter(|&r| {
        matches!(r, '\t' | '\n' | '\x0b' | '\x0c' | '\r')
            || !(r < '\x20' || r == '\x7f' || ('\u{80}'..='\u{9f}').contains(&r))
    }));
    out
}

/// Bounds a sanitized last-message preview. 200 runes is a one-to-two-line
/// preview on a phone — enough to recognize the message, small enough to keep
/// listing/SSE payloads tiny.
const MAX_LAST_MESSAGE_RUNES: usize = 200;

/// Caps one message's text (and one tool block's name/detail) after
/// sanitization. 8 KiB preserves far more than the 200-rune last_message
/// preview while keeping a single row bounded; a longer value is truncated with
/// [`FEED_TRUNC_MARKER`] appended.
pub const MAX_FEED_MESSAGE_BYTES: usize = 8 << 10;

/// Appended to a text (or tool detail) truncated at the byte cap, so a client
/// can tell a preview from a complete message.
pub const FEED_TRUNC_MARKER: &str = "…[truncated]";

/// Caps each identifier-shaped approval field (the id, the status, and every
/// advertised decision token) at the length of the id's wire grammar
/// (`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`). The grammar itself is the
/// producing layer's business; the ring only BOUNDS what it is handed, so a
/// misbehaving producer cannot inflate the byte budget.
pub const MAX_APPROVAL_TOKEN_BYTES: usize = 128;

/// Caps how many advertised decisions one approval row may carry. The decision
/// vocabulary is a fixed, tiny enum; the cap exists so the slice cannot be used
/// as unbounded payload.
pub const MAX_APPROVAL_DECISIONS: usize = 8;

/// Turns raw agent text into a safe, compact one-line preview: strip ANSI escape
/// sequences, drop remaining control characters (C0 except whitespace, DEL, and
/// the C1 range — so a smuggled CSI can't survive), collapse every run of
/// whitespace to a single space, trim, and truncate to
/// [`MAX_LAST_MESSAGE_RUNES`] on a char boundary (never mid-codepoint). The
/// result is plain, single-line, bounded.
pub fn sanitize_last_message(s: &str) -> String {
    let s = ANSI_ESCAPE_RE.replace_all(s, "");
    let s = strip_non_whitespace_controls(&s);
    // Go's strings.Fields splits on Unicode whitespace and rejoins with a single
    // ASCII space — split_whitespace is the same set (char::is_whitespace ==
    // unicode.IsSpace); collapse + trim in one step.
    let s = s.split_whitespace().collect::<Vec<_>>().join(" ");
    match s.char_indices().nth(MAX_LAST_MESSAGE_RUNES) {
        Some((byte_idx, _)) => s[..byte_idx].to_string(),
        None => s,
    }
}

/// Drops leading/trailing whitespace on a captured message before it enters the
/// ring — the sanitizer keeps internal newlines; producers just tidy the ends.
pub fn trim_feed_text(s: &str) -> &str {
    s.trim_matches([' ', '\t', '\n', '\r'])
}

/// Strips ANSI escape sequences and non-whitespace control characters from raw
/// agent text, then caps it at [`MAX_FEED_MESSAGE_BYTES`] on a char boundary
/// (appending [`FEED_TRUNC_MARKER`] when it truncates). Unlike
/// [`sanitize_last_message`] it PRESERVES newlines and internal whitespace — a
/// feed row keeps its structure (a code block, multi-line tool output) rather
/// than collapsing to a one-line preview.
pub fn sanitize_feed_text(s: &str) -> String {
    if s.is_empty() {
        return String::new();
    }
    let s = ANSI_ESCAPE_RE.replace_all(s, "");
    let s = strip_non_whitespace_controls(&s);
    if s.len() <= MAX_FEED_MESSAGE_BYTES {
        return s;
    }
    let mut out = truncate_bytes(&s, MAX_FEED_MESSAGE_BYTES).to_string();
    out.push_str(FEED_TRUNC_MARKER);
    out
}

/// Strips ANSI escapes plus EVERY control and whitespace rune from a
/// single-token approval field (id/status/decision). Feed text keeps newlines
/// and tabs (multi-line prose is content there); a token that contains them is
/// malformed, and preserving them would let a crafted value smuggle separators
/// into a field the contract defines as one token.
pub fn sanitize_feed_token(s: &str) -> String {
    if s.is_empty() {
        return String::new();
    }
    ANSI_ESCAPE_RE
        .replace_all(s, "")
        .chars()
        .filter(|&r| !(r <= '\x20' || r == '\x7f' || ('\u{80}'..='\u{9f}').contains(&r)))
        .collect()
}

/// Caps `s` at `n` bytes on a char boundary (never mid-codepoint). It appends
/// no marker of its own: [`sanitize_feed_text`] adds one for prose, while the
/// identifier fields it also guards would only be muddied by a marker inside
/// the value.
pub fn truncate_bytes(s: &str, mut n: usize) -> &str {
    if s.len() <= n {
        return s;
    }
    while n > 0 && !s.is_char_boundary(n) {
        n -= 1; // back up to a char boundary so a multi-byte codepoint is never split
    }
    &s[..n]
}

/// One bounded identifier token: sanitize, then cap at the grammar's ceiling.
///
/// The pair [`sanitize_feed_token`] + [`MAX_APPROVAL_TOKEN_BYTES`] is what every
/// approval id/status/decision on a transcript row goes through, so it is named
/// rather than open-coded at each of [`crate::lane::ring::MessageRing`]'s four
/// call sites.
pub fn bound_token(s: &str) -> String {
    // Truncated in place rather than `truncate_bytes(&sanitized).to_string()`,
    // which allocates the sanitized String and then a second copy of its head.
    let mut t = sanitize_feed_token(s);
    let n = truncate_bytes(&t, MAX_APPROVAL_TOKEN_BYTES).len();
    t.truncate(n);
    t
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn sanitize_last_message_strips_ansi_and_collapses_whitespace() {
        assert_eq!(
            sanitize_last_message("\u{1b}[31mred\u{1b}[0m\n  text\t\there"),
            "red text here"
        );
        // C1 CSI (0x9b) is dropped along with the C0 controls.
        assert_eq!(sanitize_last_message("a\u{9b}31mb"), "a31mb");
        // Bounded at 200 runes, on a char boundary.
        let long = "é".repeat(300);
        assert_eq!(sanitize_last_message(&long).chars().count(), 200);
    }

    #[test]
    fn trim_feed_text_trims_ends_only() {
        assert_eq!(trim_feed_text("  a\n b  \n"), "a\n b");
    }

    #[test]
    fn sanitize_feed_text_keeps_structure_and_marks_truncation() {
        // Newlines and internal runs survive; escapes and controls do not.
        assert_eq!(
            sanitize_feed_text("\u{1b}[31mred\u{1b}[0m\n  kept\u{7f}"),
            "red\n  kept"
        );
        assert_eq!(sanitize_feed_text(""), "");

        let long = "x".repeat(MAX_FEED_MESSAGE_BYTES + 100);
        let got = sanitize_feed_text(&long);
        assert!(got.ends_with(FEED_TRUNC_MARKER));
        assert_eq!(got.len(), MAX_FEED_MESSAGE_BYTES + FEED_TRUNC_MARKER.len());
    }

    #[test]
    fn sanitize_feed_token_drops_every_control_and_whitespace_rune() {
        assert_eq!(sanitize_feed_token("pend\ning"), "pending");
        assert_eq!(sanitize_feed_token("a b\tc"), "abc");
        assert_eq!(sanitize_feed_token("\u{1b}[31mid\u{1b}[0m"), "id");
        assert_eq!(sanitize_feed_token(""), "");
    }

    #[test]
    fn truncate_bytes_never_splits_a_codepoint() {
        // "é" is two bytes: a cut at 1 backs up to 0, at 3 backs up to 2.
        assert_eq!(truncate_bytes("éé", 1), "");
        assert_eq!(truncate_bytes("éé", 3), "é");
        assert_eq!(truncate_bytes("éé", 4), "éé");
        assert_eq!(truncate_bytes("abc", 99), "abc");
    }

    #[test]
    fn bound_token_sanitizes_then_caps() {
        assert_eq!(bound_token("pend\ning"), "pending");
        let long = "a".repeat(MAX_APPROVAL_TOKEN_BYTES + 40);
        assert_eq!(bound_token(&long).len(), MAX_APPROVAL_TOKEN_BYTES);
    }

    /// The vocabulary is wire contract: a token renamed here is a client that
    /// stops rendering a row, so the strings are pinned literally.
    #[test]
    fn the_feed_vocabulary_is_the_wire_spelling() {
        let roles = [
            FEED_ROLE_USER,
            FEED_ROLE_ASSISTANT,
            FEED_ROLE_TOOL,
            FEED_ROLE_SYSTEM,
        ];
        assert_eq!(roles, ["user", "assistant", "tool", "system"]);
        let types = [
            FEED_TYPE_TEXT,
            FEED_TYPE_TOOL_USE,
            FEED_TYPE_TOOL_RESULT,
            FEED_TYPE_REASONING,
            FEED_TYPE_STATUS,
            FEED_TYPE_APPROVAL_REQUEST,
        ];
        assert_eq!(
            types,
            [
                "text",
                "tool_use",
                "tool_result",
                "reasoning",
                "status",
                "approval_request",
            ]
        );
        assert_eq!(
            [APPROVAL_STATUS_PENDING, APPROVAL_STATUS_RESOLVED],
            ["pending", "resolved"]
        );
        assert_eq!(
            [
                APPROVAL_DECISION_ALLOW,
                APPROVAL_DECISION_ALLOW_ALWAYS,
                APPROVAL_DECISION_DENY,
            ],
            ["allow", "allow_always", "deny"]
        );
    }
}
