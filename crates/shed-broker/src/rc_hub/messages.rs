//! The per-session message feed — a port of `internal/ext/rc/hub_messages.go`
//! (the ring, the sanitizers, the feed wire vocabulary, and the producer-side
//! `trimFeedText`), plus the text-hygiene helpers it leans on from
//! `activity.go` (the ANSI-escape stripper, the control-character filter, and
//! the folds' `SanitizeLastMessage` preview sanitizer).
//!
//! These are the strict PRODUCER-side wire shapes; the tolerant client-side
//! twins live in `shed_core::rc` (`RcFeedTool`/`RcFeedApproval`/
//! `RcFeedMessage`/`RcMessagesPage`) — deliberate duplication across the
//! producer/consumer trust boundary.
//!
//! The message feed is the non-TUI view's data source: as the watchers fold
//! their native event streams (codex rollout JSONL, opencode SSE, cursor hook
//! pushes), each also emits normalized conversation messages, which the
//! reconcile loop drains into a per-session ring buffer here.
//! `GET /v1/sessions/{slug}/messages` pages that ring; `message.appended` SSE
//! events notify subscribers a new message landed (the body is fetched from
//! `/messages` — the notification stays tiny).
//!
//! Bounds (the wire contract): each message's text (and a tool block's detail)
//! is sanitized and capped at 8 KiB; the per-session ring holds at most 500
//! messages AND 1 MiB of text total, dropping the oldest first. `seq` is
//! monotonic per hub run, starting at 1 — it restarts from 1 when the hub
//! restarts, so a client that sees a seq lower than one it already holds does a
//! full refetch.

use std::collections::VecDeque;
use std::sync::LazyLock;
use std::sync::Mutex;

use chrono::{DateTime, SecondsFormat, Utc};
use regex::Regex;
use serde::{Deserialize, Serialize};

/// Caps one message's text (and one tool block's detail) after sanitization
/// (`maxFeedMessageBytes`, `hub_messages.go:32`). 8 KiB preserves far more than
/// the 200-rune last_message preview while keeping a single message bounded. A
/// longer value is truncated with [`FEED_TRUNC_MARKER`] appended.
pub const MAX_FEED_MESSAGE_BYTES: usize = 8 << 10;
/// Caps the per-session ring by count, drop-oldest past it
/// (`maxRingMessages`, `hub_messages.go:34`).
pub const MAX_RING_MESSAGES: usize = 500;
/// Caps the per-session ring by total text bytes, drop-oldest past it
/// (`maxRingBytes`, `hub_messages.go:36`).
pub const MAX_RING_BYTES: usize = 1 << 20;
/// Appended to a message text (or tool detail) truncated at the byte cap, so a
/// client can tell a preview from a complete message (`feedTruncMarker`).
pub const FEED_TRUNC_MARKER: &str = "…[truncated]";

/// The `/messages` page bounds (the wire contract: default 100, hard cap 200 —
/// `defaultMessagesLimit`/`maxMessagesLimit`, `hub_messages.go:43-44`).
pub const DEFAULT_MESSAGES_LIMIT: usize = 100;
pub const MAX_MESSAGES_LIMIT: usize = 200;

/// Caps each identifier-shaped approval field (the id and every advertised
/// decision token) at the length of the id's wire grammar
/// (`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$` — the contract's 128-char ceiling).
/// The grammar itself is enforced at the hub's approval handler (where a bad id
/// is a 4xx); the ring only BOUNDS what a producer hands it, so a misbehaving
/// lane adapter can never inflate the ring's byte budget
/// (`maxApprovalTokenBytes`, `hub_messages.go:53`).
pub const MAX_APPROVAL_TOKEN_BYTES: usize = 128;
/// Caps how many advertised decisions one approval row may carry. The decision
/// vocabulary is a fixed, tiny enum; the cap exists so the slice cannot be used
/// as unbounded payload (`maxApprovalDecisions`, `hub_messages.go:57`).
pub const MAX_APPROVAL_DECISIONS: usize = 8;

// Feed message role/type tokens (the wire contract's message shape,
// `hub_messages.go:64-83`). role ∈ {user, assistant, tool, system}; type ∈
// {text, tool_use, tool_result, reasoning, status, approval_request}.
pub const FEED_ROLE_USER: &str = "user";
pub const FEED_ROLE_ASSISTANT: &str = "assistant";
pub const FEED_ROLE_TOOL: &str = "tool";
pub const FEED_ROLE_SYSTEM: &str = "system";

pub const FEED_TYPE_TEXT: &str = "text";
pub const FEED_TYPE_TOOL_USE: &str = "tool_use";
pub const FEED_TYPE_TOOL_RESULT: &str = "tool_result";
pub const FEED_TYPE_REASONING: &str = "reasoning";
pub const FEED_TYPE_STATUS: &str = "status";
/// An approval row: an agent asked for permission to do something. It rides
/// role `tool` with `text` carrying the sanitized human-readable summary,
/// `tool{name,detail}` the call being approved, and `approval` the
/// machine-readable state. A resolution is a SECOND row with the same id and
/// status "resolved" — never an edit of the first (see [`FeedApproval`]).
pub const FEED_TYPE_APPROVAL_REQUEST: &str = "approval_request";

// Approval status / decision tokens (the wire contract's approval vocabulary,
// `hub_messages.go:86-93`).
pub const APPROVAL_STATUS_PENDING: &str = "pending";
pub const APPROVAL_STATUS_RESOLVED: &str = "resolved";

pub const APPROVAL_DECISION_ALLOW: &str = "allow";
pub const APPROVAL_DECISION_ALLOW_ALWAYS: &str = "allow_always";
pub const APPROVAL_DECISION_DENY: &str = "deny";

/// A tool call/result's name + a compact detail (the invocation arguments for
/// `tool_use`, the output for `tool_result`). Both are sanitized/capped
/// (`feedTool`, `hub_messages.go:97`).
#[derive(Debug, Clone, Default, PartialEq, Eq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct FeedTool {
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub name: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub detail: String,
}

/// The machine-readable state of an approval request carried by an
/// `approval_request` feed row (and by a session's `pending_approvals`
/// snapshot — part of the Session DTO). Port of `FeedApproval`
/// (`hub_messages.go:112`).
///
/// CLIENT FOLDING RULE: approval rows are an id-keyed, LAST-WRITE-WINS stream.
/// A resolution is a SECOND appended row with the same id and status
/// "resolved" — never an edit of the first. A client must not require having
/// seen the `pending` row before the `resolved` one: ring eviction (or a hub
/// restart) can drop the earlier row entirely, and the session's
/// `pending_approvals` snapshot is the authoritative answer to "what is still
/// open".
#[derive(Debug, Clone, Default, PartialEq, Eq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct FeedApproval {
    /// The lane-assigned approval id — the address the approval verb resolves
    /// (`POST /v1/sessions/{slug}/approvals/{id}`). Grammar:
    /// `^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`.
    pub id: String,
    /// "pending" or "resolved".
    pub status: String,
    /// The decision that resolved it (empty while pending).
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub decision: String,
    /// The decisions this request accepts, advertised per request so a client
    /// renders exactly the buttons the lane will honor (a subset of
    /// allow/allow_always/deny).
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    pub decisions: Vec<String>,
}

impl FeedApproval {
    /// Bounds the approval's fields before it enters the ring: every field is a
    /// single-token value (never prose), so each is run through the token
    /// sanitizer (ANSI + ALL control/whitespace stripping — unlike feed text, a
    /// token has no legitimate newlines or tabs), the id is capped at the
    /// grammar's length ceiling, and the advertised decision list is capped in
    /// count. Validity (the id grammar, the decision enum) is the
    /// producing/handling layer's job; this is the ring's byte-budget guard
    /// (`(*FeedApproval).sanitize`, `hub_messages.go:134`).
    fn sanitize(&mut self) {
        // One bounded token: sanitize, then cap at the grammar's ceiling.
        fn bound_token(s: &str) -> String {
            truncate_bytes(&sanitize_feed_token(s), MAX_APPROVAL_TOKEN_BYTES).to_string()
        }
        self.id = bound_token(&self.id);
        self.status = bound_token(&self.status);
        self.decision = bound_token(&self.decision);
        self.decisions.truncate(MAX_APPROVAL_DECISIONS);
        for d in &mut self.decisions {
            *d = bound_token(d);
        }
    }

    /// The approval's contribution to the ring's byte budget (id + status +
    /// decision + every advertised decision) — `(*FeedApproval).size`,
    /// `hub_messages.go:166`.
    pub fn size(&self) -> usize {
        self.id.len()
            + self.status.len()
            + self.decision.len()
            + self.decisions.iter().map(String::len).sum::<usize>()
    }
}

/// One normalized conversation message in the ring / on the wire
/// (`feedMessage`, `hub_messages.go:175`).
#[derive(Debug, Clone, Default, PartialEq, Eq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct FeedMessage {
    pub seq: u64,
    pub ts: String,
    pub role: String,
    #[serde(rename = "type")]
    pub typ: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub text: String,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub tool: Option<FeedTool>,
    /// Set on (and only on) an `approval_request` row.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub approval: Option<FeedApproval>,
}

impl FeedMessage {
    /// The message's contribution to the ring's byte budget (text + tool +
    /// approval fields — every string that rides the row is accounted, so the
    /// 1 MiB budget stays honest for an approval-heavy feed) —
    /// `(feedMessage).size`, `hub_messages.go:189`.
    pub fn size(&self) -> usize {
        let mut n = self.text.len();
        if let Some(t) = &self.tool {
            n += t.name.len() + t.detail.len();
        }
        if let Some(a) = &self.approval {
            n += a.size();
        }
        n
    }
}

/// One session's bounded, drop-oldest feed (`messageRing`,
/// `hub_messages.go:203`). `seq` is assigned on append (monotonic from 1).
/// Safe for concurrent use: reconcile appends while `/messages` and the SSE
/// notifier read.
#[derive(Default)]
pub struct MessageRing {
    inner: Mutex<RingInner>,
}

#[derive(Default)]
struct RingInner {
    msgs: VecDeque<FeedMessage>,
    /// Last assigned seq.
    seq: u64,
    /// Running sum of msgs' `size()`.
    bytes: usize,
}

impl MessageRing {
    pub fn new() -> MessageRing {
        MessageRing::default()
    }

    /// Sanitizes `m`, assigns the next seq, stores it, and drops the oldest
    /// messages until both caps hold. `now` stamps `ts` when the message
    /// carries none. Returns the assigned seq (`(*messageRing).append`,
    /// `hub_messages.go:215`).
    ///
    /// One Go concern dissolves here: Go's append stores the approval as a
    /// COPY (own decisions backing array, count-capped before copying) so a
    /// producer that reuses/mutates its struct can never rewrite a row already
    /// accounted in `bytes`. Rust takes `m` by value — ownership makes
    /// producer aliasing (and the pre-copy cap) unrepresentable; the count cap
    /// lives in `sanitize` alone.
    pub fn append(&self, mut m: FeedMessage, now: DateTime<Utc>) -> u64 {
        let mut r = self
            .inner
            .lock()
            .unwrap_or_else(std::sync::PoisonError::into_inner);

        m.text = sanitize_feed_text(&m.text);
        if let Some(t) = &mut m.tool {
            t.name = sanitize_feed_text(&t.name);
            t.detail = sanitize_feed_text(&t.detail);
        }
        if let Some(a) = &mut m.approval {
            a.sanitize();
        }
        if m.ts.is_empty() {
            m.ts = now.to_rfc3339_opts(SecondsFormat::Secs, true);
        }
        r.seq += 1;
        m.seq = r.seq;
        let seq = m.seq;
        r.bytes += m.size();
        r.msgs.push_back(m);

        // Drop-oldest until within BOTH caps. Never drop the just-appended sole
        // message (a lone message that alone exceeds the byte cap is impossible
        // after the 8 KiB per-field caps, but the guard keeps the ring from
        // going empty regardless).
        while r.msgs.len() > 1 && (r.msgs.len() > MAX_RING_MESSAGES || r.bytes > MAX_RING_BYTES) {
            if let Some(dropped) = r.msgs.pop_front() {
                r.bytes -= dropped.size();
            }
        }
        seq
    }

    /// Returns up to `limit` messages with seq > `since_seq` (exclusive),
    /// oldest first, plus `truncated=true` in two cursor-misalignment cases the
    /// caller must treat as "refetch from scratch"
    /// (`(*messageRing).since`, `hub_messages.go:271`):
    ///
    /// - `since_seq` predates the ring's earliest retained message (drop-oldest
    ///   discarded messages the caller has not seen);
    /// - `since_seq` points BEYOND the ring's latest assigned seq — the cursor
    ///   came from a previous ring incarnation (a hub restart or session
    ///   recreate restarts seq at 1), so a poll-only client would otherwise sit
    ///   on empty pages forever, silently misaligned.
    ///
    /// `limit` is clamped to the page bounds (`<= 0` → the default).
    pub fn since(&self, since_seq: u64, limit: i64) -> (Vec<FeedMessage>, bool) {
        let limit = if limit <= 0 {
            DEFAULT_MESSAGES_LIMIT
        } else {
            (limit as usize).min(MAX_MESSAGES_LIMIT)
        };
        let r = self
            .inner
            .lock()
            .unwrap_or_else(std::sync::PoisonError::into_inner);

        // Beyond-tail cursor. This also absorbs the u64 edge: any since_seq >
        // r.seq (u64::MAX included) is a stale cursor from another incarnation,
        // answered before any arithmetic that could wrap.
        if since_seq > r.seq {
            return (Vec::new(), true);
        }
        let mut truncated = false;
        if let Some(first) = r.msgs.front() {
            // The caller expects since_seq+1 next; if that predates the
            // earliest retained seq, the ring dropped messages between them.
            // Compared as `since_seq < earliest-1` (earliest is always >= 1) so
            // no u64 addition can overflow.
            if since_seq < first.seq - 1 {
                truncated = true;
            }
        }
        let msgs: Vec<FeedMessage> = r
            .msgs
            .iter()
            .filter(|m| m.seq > since_seq)
            .take(limit)
            .cloned()
            .collect();
        (msgs, truncated)
    }
}

/// The `GET /v1/sessions/{slug}/messages` body (`hubMessagesResponse`,
/// `hub_messages.go:340`).
///
/// EMPTY-PAGE WIRE NOTE (resolved at H10): Go's HANDLER coerces the nil
/// slice `since` returns to `[]feedMessage{}` before encoding
/// (`hub.go:440-442`), so the wire is `[]` on both sides — a `Vec` matches it
/// naturally. The `null_default` deserializer below stays as defensive
/// tolerance for any non-handler producer.
#[derive(Debug, Clone, Default, PartialEq, Eq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct HubMessagesResponse {
    /// `null`-tolerant so the DECODE direction accepts the Go hub's own empty
    /// page (`messages: null` — the COMMON case for a caught-up cursor: Go
    /// marshals the nil slice `since` returns), which a bare `Vec` would
    /// reject; the harness round-trips Go bodies through this type.
    #[serde(default, deserialize_with = "null_default")]
    pub messages: Vec<FeedMessage>,
    pub truncated: bool,
}

/// Go-null tolerance: decode an explicit `null` as the default (the local twin
/// of `shed_core::models::null_default`, which is crate-private there).
///
/// This is THE fold-side tolerance shim (H5 review finding, HIGH): Go's
/// `json.Unmarshal` treats `null` into a string/slice field as a NO-OP with no
/// error, so a producer line carrying `"stop_reason":null` (42k+ occurrences
/// in real claude transcripts) still folds — while a bare serde `String` field
/// treats the same `null` as a type error and kills the whole line. Every
/// decoded string/Vec field on the fold envelopes rides this. A genuinely
/// WRONG type (a number where a string belongs) still errors, exactly like Go.
pub(crate) fn null_default<'de, D, T>(d: D) -> Result<T, D::Error>
where
    D: serde::Deserializer<'de>,
    T: Default + Deserialize<'de>,
{
    Ok(Option::<T>::deserialize(d)?.unwrap_or_default())
}

/// Strips terminal escape sequences from captured text (`ansiEscapeRe`,
/// `activity.go:83`): CSI sequences (`ESC [ … final`), OSC sequences
/// (`ESC ] … BEL|ST`) — including an UNTERMINATED OSC that runs to the end of
/// the string (a chunk-cut capture can truncate the sequence mid-payload) —
/// and the standalone two-byte escapes (`ESC` + a single Fe byte). Applied
/// before control-char stripping so an escape's intermediate bytes are
/// consumed as a unit rather than left as stray punctuation.
pub(crate) static ANSI_ESCAPE_RE: LazyLock<Regex> = LazyLock::new(|| {
    Regex::new(r"\x1b\[[0-?]*[ -/]*[@-~]|\x1b\][^\x07\x1b]*(?:\x07|\x1b\\|\z)|\x1b[@-Z\\-_]")
        .expect("ANSI escape regex compiles")
});

/// Removes control characters that are NOT whitespace, leaving whitespace
/// controls (tab/newline/CR/VT/FF) in place. Dropped: C0 (< 0x20) other than
/// whitespace, DEL (0x7f), and C1 (0x80–0x9f, e.g. the 8-bit CSI 0x9b a
/// terminal would honor) — `stripNonWhitespaceControls`, `activity.go:111`.
pub(crate) fn strip_non_whitespace_controls(s: &str) -> String {
    s.chars()
        .filter(|&r| {
            matches!(r, '\t' | '\n' | '\x0b' | '\x0c' | '\r')
                || !(r < '\x20' || r == '\x7f' || ('\u{80}'..='\u{9f}').contains(&r))
        })
        .collect()
}

/// Strips ANSI escapes plus EVERY control and whitespace rune from a
/// single-token approval field (id/status/decision). Feed text keeps newlines
/// and tabs (multi-line prose is content there); a token that contains them is
/// malformed, and preserving them would let a crafted value smuggle separators
/// into a field the contract defines as one token (`sanitizeFeedToken`,
/// `hub_messages.go:151`).
fn sanitize_feed_token(s: &str) -> String {
    if s.is_empty() {
        return String::new();
    }
    ANSI_ESCAPE_RE
        .replace_all(s, "")
        .chars()
        .filter(|&r| !(r <= '\x20' || r == '\x7f' || ('\u{80}'..='\u{9f}').contains(&r)))
        .collect()
}

/// Strips ANSI escape sequences and non-whitespace control characters from raw
/// agent/JSONL text, then caps it at [`MAX_FEED_MESSAGE_BYTES`] on a char
/// boundary (appending [`FEED_TRUNC_MARKER`] when it truncates). Unlike the
/// last-message preview sanitizer it PRESERVES newlines and internal
/// whitespace — a feed message keeps its structure (a code block, multi-line
/// tool output) rather than collapsing to a one-line preview
/// (`sanitizeFeedText`, `hub_messages.go:313`).
pub(crate) fn sanitize_feed_text(s: &str) -> String {
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

/// Caps `s` at `n` bytes on a char boundary (never mid-codepoint). It appends
/// no marker of its own: [`sanitize_feed_text`] adds one for prose, while the
/// identifier fields (approval ids, enum tokens) it also guards would only be
/// muddied by a marker inside the value (`truncateBytes`,
/// `hub_messages.go:329`).
fn truncate_bytes(s: &str, mut n: usize) -> &str {
    if s.len() <= n {
        return s;
    }
    while n > 0 && !s.is_char_boundary(n) {
        n -= 1; // back up to a char boundary so a multi-byte codepoint is never split
    }
    &s[..n]
}

/// Bounds a sanitized last-message preview (`maxLastMessageRunes`,
/// `activity.go:72`). 200 runes is a one-to-two-line preview on a phone —
/// enough to recognize the message, small enough to keep listing/SSE payloads
/// tiny.
const MAX_LAST_MESSAGE_RUNES: usize = 200;

/// Turns raw agent/JSONL text into a safe, compact one-line preview
/// (`SanitizeLastMessage`, `activity.go:95`): strip ANSI escape sequences,
/// drop remaining control characters (C0 except whitespace, DEL, and the C1
/// range — so a smuggled CSI can't survive), collapse every run of whitespace
/// to a single space, trim, and truncate to [`MAX_LAST_MESSAGE_RUNES`] on a
/// char boundary (never mid-codepoint). The result is plain, single-line,
/// bounded. Every fold's `last_message` runs through this.
pub(crate) fn sanitize_last_message(s: &str) -> String {
    let s = ANSI_ESCAPE_RE.replace_all(s, "");
    let s = strip_non_whitespace_controls(&s);
    // Go's strings.Fields splits on Unicode whitespace and rejoins with a
    // single ASCII space — split_whitespace is the same set (char::is_whitespace
    // == unicode.IsSpace); collapse + trim in one step.
    let s = s.split_whitespace().collect::<Vec<_>>().join(" ");
    match s.char_indices().nth(MAX_LAST_MESSAGE_RUNES) {
        Some((byte_idx, _)) => s[..byte_idx].to_string(),
        None => s,
    }
}

/// Drops leading/trailing whitespace on a captured message before it enters
/// the ring — the sanitizer keeps internal newlines; producers just tidy the
/// ends (`trimFeedText`, `hub_messages.go:348`).
pub(crate) fn trim_feed_text(s: &str) -> &str {
    s.trim_matches([' ', '\t', '\n', '\r'])
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::sync::Arc;

    /// The fixed clock every ring test stamps with (`ringClock`,
    /// `hub_messages_test.go:10`).
    fn ring_clock() -> DateTime<Utc> {
        DateTime::from_timestamp(1_700_000_000, 0).expect("valid epoch")
    }

    fn text_msg(text: &str) -> FeedMessage {
        FeedMessage {
            role: FEED_ROLE_ASSISTANT.into(),
            typ: FEED_TYPE_TEXT.into(),
            text: text.into(),
            ..FeedMessage::default()
        }
    }

    /// Builds a pending approval_request row (`approvalMsg`,
    /// `hub_messages_test.go:129`).
    fn approval_msg(id: &str, text: &str) -> FeedMessage {
        FeedMessage {
            role: FEED_ROLE_TOOL.into(),
            typ: FEED_TYPE_APPROVAL_REQUEST.into(),
            text: text.into(),
            approval: Some(FeedApproval {
                id: id.into(),
                status: APPROVAL_STATUS_PENDING.into(),
                decisions: vec![
                    APPROVAL_DECISION_ALLOW.into(),
                    APPROVAL_DECISION_ALLOW_ALWAYS.into(),
                    APPROVAL_DECISION_DENY.into(),
                ],
                ..FeedApproval::default()
            }),
            ..FeedMessage::default()
        }
    }

    fn seqs_of(msgs: &[FeedMessage]) -> Vec<u64> {
        msgs.iter().map(|m| m.seq).collect()
    }

    // Mirrors TestMessageRingSeqMonotonicAndSincePaging.
    #[test]
    fn seq_monotonic_and_since_paging() {
        let r = MessageRing::new();
        for i in 0..5u64 {
            let seq = r.append(text_msg("m"), ring_clock());
            assert_eq!(seq, i + 1, "append assigns monotonic seq from 1");
        }

        // since is EXCLUSIVE: since=2 returns seq 3,4,5.
        let (got, truncated) = r.since(2, 100);
        assert!(
            !truncated,
            "since within the ring must not report truncated"
        );
        assert_eq!(seqs_of(&got), vec![3, 4, 5]);

        // limit caps the page; the client pages again from the last seq it saw.
        let (page, _) = r.since(0, 2);
        assert_eq!(seqs_of(&page), vec![1, 2]);

        // ts is stamped from the clock when the message carries none.
        assert_eq!(page[0].ts, "2023-11-14T22:13:20Z");
        assert_eq!(
            page[0].ts,
            ring_clock().to_rfc3339_opts(SecondsFormat::Secs, true)
        );
    }

    // Mirrors TestMessageRingLimitClampAndDefault.
    #[test]
    fn limit_clamp_and_default() {
        let r = MessageRing::new();
        for _ in 0..250 {
            r.append(text_msg("m"), ring_clock());
        }
        // default (limit<=0) is 100.
        assert_eq!(r.since(0, 0).0.len(), DEFAULT_MESSAGES_LIMIT);
        assert_eq!(r.since(0, -3).0.len(), DEFAULT_MESSAGES_LIMIT);
        // clamp to the hard cap.
        assert_eq!(r.since(0, 10_000).0.len(), MAX_MESSAGES_LIMIT);
    }

    // Mirrors TestMessageRingDropOldestByCount.
    #[test]
    fn drop_oldest_by_count() {
        let r = MessageRing::new();
        let total = MAX_RING_MESSAGES + 50;
        for _ in 0..total {
            r.append(text_msg("m"), ring_clock());
        }
        let (got, _) = r.since(0, MAX_MESSAGES_LIMIT as i64);
        // The earliest retained seq is total-MAX_RING_MESSAGES+1 (the first 50 dropped).
        let want_earliest = (total - MAX_RING_MESSAGES + 1) as u64;
        assert_eq!(got[0].seq, want_earliest, "drop-oldest by count");

        // A fresh client (since=0) whose expected next seq predates the ring gets truncated.
        let (_, truncated) = r.since(0, MAX_MESSAGES_LIMIT as i64);
        assert!(truncated, "since=0 against a ring that dropped its head");
        // A since AT the earliest-1 boundary is contiguous → not truncated.
        assert!(
            !r.since(want_earliest - 1, 10).1,
            "earliest-1 is contiguous"
        );
        // A since one below the boundary IS truncated (a gap was dropped).
        assert!(
            r.since(want_earliest - 2, 10).1,
            "below earliest-1 truncates"
        );
    }

    // Mirrors TestMessageRingDropOldestByBytes.
    #[test]
    fn drop_oldest_by_bytes() {
        let r = MessageRing::new();
        let big = "x".repeat(100 * 1024); // 100 KiB each (well over 8 KiB → truncated)
        const N: usize = 400;
        for _ in 0..N {
            r.append(text_msg(&big), ring_clock());
        }
        let (got, _) = r.since(0, MAX_MESSAGES_LIMIT as i64);
        let sum: usize = got.iter().map(FeedMessage::size).sum();
        assert!(sum <= MAX_RING_BYTES, "retained bytes within the byte cap");
        assert!(got.len() < N, "the byte cap must drop oversized messages");
        // Each retained message was truncated to the 8 KiB cap with the marker.
        assert!(got[0].text.ends_with(FEED_TRUNC_MARKER));
        assert_eq!(
            got[0].text.len() - FEED_TRUNC_MARKER.len(),
            MAX_FEED_MESSAGE_BYTES
        );
    }

    // Mirrors TestMessageRingApprovalBytesCounted: an approval row's payload
    // counts toward the ring's byte budget — size() sums id + status + decision
    // + every advertised decision alongside text/tool.
    #[test]
    fn approval_bytes_counted() {
        let m = approval_msg("call_01HQ8Z3K.tool:2", "Allow running `rm -rf build/`?");
        let a = m.approval.as_ref().expect("approval set");
        let want_approval = a.id.len()
            + APPROVAL_STATUS_PENDING.len()
            + APPROVAL_DECISION_ALLOW.len()
            + APPROVAL_DECISION_ALLOW_ALWAYS.len()
            + APPROVAL_DECISION_DENY.len();
        assert_eq!(a.size(), want_approval);
        // The row's size is its text PLUS the whole approval payload.
        assert_eq!(m.size(), m.text.len() + want_approval);

        // The ring's running total agrees with the per-message accounting.
        let want_size = m.size();
        let r = MessageRing::new();
        r.append(m, ring_clock());
        assert_eq!(r.inner.lock().unwrap().bytes, want_size);
    }

    // Mirrors TestMessageRingApprovalRoundTrip: an approval_request row survives
    // append → since unchanged, and its resolution is a SECOND row carrying the
    // same id — the id-keyed, last-write-wins stream clients fold.
    #[test]
    fn approval_round_trip() {
        let r = MessageRing::new();
        const ID: &str = "call_01HQ8Z3K.tool:2";
        r.append(
            approval_msg(ID, "Allow running `rm -rf build/`?"),
            ring_clock(),
        );
        let mut resolved = approval_msg(ID, "Allow running `rm -rf build/`?");
        {
            let a = resolved.approval.as_mut().expect("approval set");
            a.status = APPROVAL_STATUS_RESOLVED.into();
            a.decision = APPROVAL_DECISION_ALLOW.into();
            a.decisions = Vec::new();
        }
        r.append(resolved, ring_clock());

        let (got, truncated) = r.since(0, 10);
        assert!(!truncated, "a contiguous page must not report truncated");
        assert_eq!(got.len(), 2, "want both approval rows");
        let (pending, done) = (&got[0], &got[1]);
        assert_eq!(pending.typ, FEED_TYPE_APPROVAL_REQUEST);
        assert_eq!(pending.role, FEED_ROLE_TOOL);
        let pa = pending.approval.as_ref().expect("pending approval");
        assert_eq!(pa.id, ID);
        assert_eq!(pa.status, APPROVAL_STATUS_PENDING);
        assert_eq!(pa.decisions.len(), 3);
        let da = done.approval.as_ref().expect("resolved approval");
        assert_eq!(da.id, ID);
        assert_eq!(da.status, APPROVAL_STATUS_RESOLVED);
        assert_eq!(da.decision, APPROVAL_DECISION_ALLOW);
        assert!(
            done.seq > pending.seq,
            "resolution appended after the request"
        );
    }

    // Mirrors TestMessageRingApprovalSanitizedAndBounded: approval fields are
    // sanitized and BOUNDED on the way into the ring (ANSI/control stripping,
    // the grammar's 128-byte id ceiling, a capped decision list) so a
    // misbehaving producer cannot inflate the ring past its accounted byte
    // budget. (Go's final arm — the producer mutating its struct after append —
    // is unrepresentable here: `append` takes the row by value.)
    #[test]
    fn approval_sanitized_and_bounded() {
        let r = MessageRing::new();
        // The id smuggles whitespace/control separators a feed TEXT sanitizer
        // would keep (tab, newline, DEL, a C1 CSI) — the token sanitizer must
        // drop them all, then the length cap applies.
        let id = format!(
            "a\tb\nc\x7fd\u{9b}e{}",
            "x".repeat(MAX_APPROVAL_TOKEN_BYTES + 50)
        );
        let m = FeedMessage {
            role: FEED_ROLE_TOOL.into(),
            typ: FEED_TYPE_APPROVAL_REQUEST.into(),
            approval: Some(FeedApproval {
                id,
                status: "\x1b[31mpen ding\x1b[0m".into(),
                decisions: vec![APPROVAL_DECISION_ALLOW.into(); MAX_APPROVAL_DECISIONS + 5],
                ..FeedApproval::default()
            }),
            ..FeedMessage::default()
        };
        r.append(m, ring_clock());

        let (got, _) = r.since(0, 10);
        let a = got[0].approval.as_ref().expect("approval survives");
        assert_eq!(
            a.id.len(),
            MAX_APPROVAL_TOKEN_BYTES,
            "id capped at the ceiling"
        );
        assert_eq!(a.status, APPROVAL_STATUS_PENDING, "ANSI + space stripped");
        assert_eq!(
            a.decisions.len(),
            MAX_APPROVAL_DECISIONS,
            "decisions capped"
        );
        assert_eq!(
            r.inner.lock().unwrap().bytes,
            got[0].size(),
            "ring bytes account the sanitized size"
        );
    }

    // Mirrors TestSanitizeFeedText: strip + preserve newlines + rune-safe
    // truncation.
    #[test]
    fn sanitize_feed_text_strips_and_truncates() {
        // ANSI + control stripped, newlines PRESERVED (unlike the last-message
        // preview sanitizer).
        let got = sanitize_feed_text("line1\n\x1b[31mred\x1b[0m\x07line2");
        assert_eq!(got, "line1\nredline2");

        // Multi-byte truncation never splits a codepoint.
        let long = "é".repeat(MAX_FEED_MESSAGE_BYTES); // 2 bytes each → 16 KiB
        let out = sanitize_feed_text(&long);
        assert!(
            out.ends_with(FEED_TRUNC_MARKER),
            "over-cap text carries the marker"
        );
        let body = out.strip_suffix(FEED_TRUNC_MARKER).unwrap();
        assert!(body.len() <= MAX_FEED_MESSAGE_BYTES);
        // A str is UTF-8 by construction (truncate_bytes backed up to a
        // boundary or slicing would have panicked); the content check proves no
        // codepoint was mangled.
        assert!(body.chars().all(|c| c == 'é'), "no split codepoint");

        // An ODD cap against 2-byte codepoints forces the boundary back-up arm.
        assert_eq!(truncate_bytes("ééé", 3), "é");
    }

    // Mirrors TestSanitizeLastMessage (activity_test.go:9) — the full strip +
    // collapse table.
    #[test]
    fn sanitize_last_message_table() {
        let cases: [(&str, &str, &str); 18] = [
            ("plain passes through", "hello world", "hello world"),
            (
                "collapses runs of whitespace",
                "hello   \t\n  world",
                "hello world",
            ),
            (
                "trims leading and trailing whitespace",
                "  \tpadded\n ",
                "padded",
            ),
            (
                "strips CSI color codes",
                "\x1b[31mred\x1b[0m and \x1b[1;32mgreen\x1b[0m",
                "red and green",
            ),
            (
                "strips CSI with colon params (ISO 8613-6 truecolor)",
                "\x1b[38:2:255:0:0mred\x1b[0m",
                "red",
            ),
            (
                "strips CSI with > private param",
                "\x1b[>4;2mtext\x1b[0m",
                "text",
            ),
            ("strips CSI with = private param", "\x1b[=3htext", "text"),
            (
                "strips OSC hyperlink (BEL-terminated)",
                "\x1b]8;;https://example.com\x07link text\x1b]8;;\x07",
                "link text",
            ),
            (
                "strips OSC (ST-terminated)",
                "\x1b]0;window title\x1b\\body",
                "body",
            ),
            (
                "strips unterminated OSC hyperlink (chunk-cut capture)",
                "before \x1b]8;;https://example.com/truncat",
                "before",
            ),
            (
                "strips lone two-byte escape",
                "before\x1bMafter",
                "beforeafter",
            ),
            ("drops NUL and BEL control chars", "a\x00b\x07c", "abc"),
            ("drops DEL", "x\x7fy", "xy"),
            ("drops C1 controls (8-bit CSI)", "a\u{9b}c", "ac"),
            (
                "keeps multibyte content intact",
                "café résumé — 日本語",
                "café résumé — 日本語",
            ),
            (
                "newline between words collapses to space",
                "line one\nline two",
                "line one line two",
            ),
            ("empty stays empty", "", ""),
            ("whitespace-only becomes empty", "   \n\t  ", ""),
        ];
        for (name, input, want) in cases {
            assert_eq!(sanitize_last_message(input), want, "{name}");
        }
    }

    // Mirrors TestSanitizeLastMessageTruncatesOnRuneBoundary
    // (activity_test.go:47).
    #[test]
    fn sanitize_last_message_truncates_on_char_boundary() {
        // ascii truncates to 200 runes.
        assert_eq!(
            sanitize_last_message(&"a".repeat(250)).chars().count(),
            MAX_LAST_MESSAGE_RUNES
        );
        // multibyte truncates on a codepoint boundary (never mid-rune).
        let got = sanitize_last_message(&"世".repeat(250));
        assert_eq!(got.chars().count(), MAX_LAST_MESSAGE_RUNES);
        assert_eq!(got.len(), MAX_LAST_MESSAGE_RUNES * 3, "200 3-byte runes");
        // a run at exactly 200 runes is unchanged.
        let exact = "x".repeat(MAX_LAST_MESSAGE_RUNES);
        assert_eq!(sanitize_last_message(&exact), exact);
        // escape sequences do not count toward the length budget.
        let body = "z".repeat(MAX_LAST_MESSAGE_RUNES);
        assert_eq!(
            sanitize_last_message(&format!("\x1b[31m{body}\x1b[0m")),
            body
        );
    }

    #[test]
    fn trim_feed_text_trims_ends_only() {
        assert_eq!(
            trim_feed_text(" \t\na body\nkeeps\ninner\n \r"),
            "a body\nkeeps\ninner"
        );
    }

    // The Go hub's EMPTY page marshals `messages:null` (nil slice, no
    // omitempty) — the common caught-up-cursor body. The decode direction must
    // accept it (H4 review finding: a bare Vec rejects both null and absence).
    #[test]
    fn empty_page_null_messages_decodes() {
        let resp: HubMessagesResponse =
            serde_json::from_str(r#"{"messages":null,"truncated":false}"#)
                .expect("Go's null empty page decodes");
        assert!(resp.messages.is_empty() && !resp.truncated);
        let resp: HubMessagesResponse =
            serde_json::from_str(r#"{"truncated":true}"#).expect("absent field decodes");
        assert!(resp.messages.is_empty() && resp.truncated);
    }

    // Mirrors TestMessageRingConcurrentAppendRead (the Go test runs under
    // -race; here the Mutex makes the equivalent claim and the test exercises
    // real contention).
    #[test]
    fn concurrent_append_read() {
        let r = Arc::new(MessageRing::new());
        let writer = {
            let r = Arc::clone(&r);
            std::thread::spawn(move || {
                for _ in 0..2000 {
                    r.append(text_msg("m"), ring_clock());
                }
            })
        };
        let reader = {
            let r = Arc::clone(&r);
            std::thread::spawn(move || {
                for i in 0..2000u64 {
                    let _ = r.since(i, 50);
                }
            })
        };
        writer.join().expect("writer thread");
        reader.join().expect("reader thread");
    }

    // Mirrors TestMessageRingSinceBeyondTailTruncated: a since cursor beyond
    // the ring's latest seq (a previous incarnation's cursor) must report
    // truncated so a poll-only client refetches instead of idling on empty
    // pages forever. The u64 edge (MAX, where since+1 would wrap) is the same
    // beyond-tail case.
    #[test]
    fn since_beyond_tail_truncated() {
        let r = MessageRing::new();
        for _ in 0..3 {
            r.append(text_msg("m"), ring_clock());
        }

        let (msgs, truncated) = r.since(10, 10); // cursor from a bigger, previous ring
        assert!(truncated, "since beyond the tail must report truncated");
        assert!(msgs.is_empty(), "beyond-tail page must be empty");
        // Exactly at the tail: an up-to-date cursor — empty page, NOT truncated.
        assert!(!r.since(3, 10).1, "since == latest seq is up to date");
        // The wrap edge.
        assert!(
            r.since(u64::MAX, 10).1,
            "since=MAX is a stale cursor, not a wrap"
        );
        // An empty ring (seq 0) with any positive cursor is also beyond-tail.
        let empty = MessageRing::new();
        assert!(
            empty.since(1, 10).1,
            "positive cursor against an empty ring"
        );
    }

    // The wire golden (`feedMessage.golden.json`, byte-locked to the Go
    // canonical by the golden_parity_test.go sweep): decodes strictly
    // (deny_unknown_fields — Go's DisallowUnknownFields), the rows carry the
    // expected role/type/approval shapes (mirrors
    // TestFeedMessageGoldenDecodes), and re-serialization is value-identical —
    // which pins the producer-side omitempty rules (no `decision` on a pending
    // row, no `decisions` on a resolved one, no empty `text`/`tool`).
    #[test]
    fn feed_message_golden_round_trips() {
        const GOLDEN: &str = include_str!("../../../fixtures/feedMessage.golden.json");
        let resp: HubMessagesResponse =
            serde_json::from_str(GOLDEN).expect("golden decodes strictly");
        assert!(!resp.truncated, "the golden page is contiguous");
        assert_eq!(resp.messages.len(), 4);

        const APPROVAL_ID: &str = "call_01HQ8Z3K.tool:2";
        let wants: [(&str, &str, Option<FeedApproval>); 4] = [
            (FEED_ROLE_USER, FEED_TYPE_TEXT, None),
            (FEED_ROLE_TOOL, FEED_TYPE_TOOL_USE, None),
            (
                FEED_ROLE_TOOL,
                FEED_TYPE_APPROVAL_REQUEST,
                Some(FeedApproval {
                    id: APPROVAL_ID.into(),
                    status: APPROVAL_STATUS_PENDING.into(),
                    decisions: vec![
                        APPROVAL_DECISION_ALLOW.into(),
                        APPROVAL_DECISION_ALLOW_ALWAYS.into(),
                        APPROVAL_DECISION_DENY.into(),
                    ],
                    ..FeedApproval::default()
                }),
            ),
            (
                FEED_ROLE_TOOL,
                FEED_TYPE_APPROVAL_REQUEST,
                Some(FeedApproval {
                    id: APPROVAL_ID.into(),
                    status: APPROVAL_STATUS_RESOLVED.into(),
                    decision: APPROVAL_DECISION_ALLOW.into(),
                    ..FeedApproval::default()
                }),
            ),
        ];
        for (i, (role, typ, approval)) in wants.iter().enumerate() {
            let m = &resp.messages[i];
            assert_eq!(m.seq, (i + 1) as u64, "seq monotonic from 1");
            assert!(!m.ts.is_empty(), "ts present on every row");
            assert_eq!((m.role.as_str(), m.typ.as_str()), (*role, *typ));
            assert_eq!(&m.approval, approval, "row {i} approval block");
        }

        // Round trip: the producer-side serialization emits exactly the
        // golden's shape (field presence included — the omitempty pins).
        let reserialized = serde_json::to_value(&resp).expect("serializes");
        let golden: serde_json::Value = serde_json::from_str(GOLDEN).expect("golden parses");
        assert_eq!(
            reserialized, golden,
            "serialization matches the wire golden"
        );
    }
}
