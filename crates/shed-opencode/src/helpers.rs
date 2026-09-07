//! The fold's decode + text-hygiene helpers, **copied** out of the rc hub.
//!
//! Every item here is a verbatim port of a `shed-broker::rc_hub` helper the
//! opencode fold leans on: the Go-`encoding/json`-shaped serde readers from
//! `rc_hub::watch` (`compact_json`, `first_non_empty`, `json_first_byte`,
//! `null_string_vec`, `object_default`, `object_opt`, `raw_opt`, `vec_objects`)
//! and the feed vocabulary + sanitizers from `rc_hub::messages` (`null_default`,
//! `sanitize_last_message`, `trim_feed_text`, the `FEED_*`/`APPROVAL_*`
//! constants).
//!
//! **Copied, not linked, on purpose.** `rc_hub::watch` imports
//! `shed_rc_engine::tmux::Tmux` at its module head (`watch.rs:28`), so a `use
//! shed_broker::rc_hub::watch::…` here would drag the whole RC engine — and the
//! hub — into an adapter that talks to opencode over HTTP and has no tmux in
//! sight. The hub's copy is S6's to delete along with the rest of its opencode
//! watcher; until then the two are duplicates by design, pinned against each
//! other by `fixtures/opencode_turn.golden.json`.
//!
//! The `null_default`/`object_opt`/`vec_objects` family exists because this wire
//! was first read by a Go producer: Go's `json.Unmarshal` treats `null` into a
//! string/slice/struct field as a NO-OP, and rejects the positional seq form a
//! serde derive would happily accept. Keeping those semantics is what makes a
//! real producer line that carries `"patterns":null` fold instead of killing the
//! whole envelope.

use std::sync::LazyLock;

use regex::Regex;
use serde::Deserialize;

// ---- feed vocabulary (rc_hub::messages) ----

// Feed message role/type tokens (the wire contract's message shape). role ∈
// {user, assistant, tool, system}; type ∈ {text, tool_use, tool_result,
// reasoning, status, approval_request}.
pub(crate) const FEED_ROLE_USER: &str = "user";
pub(crate) const FEED_ROLE_ASSISTANT: &str = "assistant";
pub(crate) const FEED_ROLE_TOOL: &str = "tool";
pub(crate) const FEED_ROLE_SYSTEM: &str = "system";

pub(crate) const FEED_TYPE_TEXT: &str = "text";
pub(crate) const FEED_TYPE_TOOL_USE: &str = "tool_use";
pub(crate) const FEED_TYPE_TOOL_RESULT: &str = "tool_result";
pub(crate) const FEED_TYPE_REASONING: &str = "reasoning";
pub(crate) const FEED_TYPE_STATUS: &str = "status";
/// An approval row: an agent asked for permission to do something. It rides
/// role `tool` with `text` carrying the sanitized human-readable summary,
/// `tool{name,detail}` the call being approved, and `approval` the
/// machine-readable state. A resolution is a SECOND row with the same id and
/// status "resolved" — never an edit of the first.
pub(crate) const FEED_TYPE_APPROVAL_REQUEST: &str = "approval_request";

// Approval status / decision tokens (the wire contract's approval vocabulary).
// These are the FEED row's spelling — `shed_core::lane::LaneApprovalStatus` and
// `LaneDecision` are the contract's, and the fold speaks both (a feed row
// carries the former, an approval row the latter).
pub(crate) const APPROVAL_STATUS_PENDING: &str = "pending";
pub(crate) const APPROVAL_STATUS_RESOLVED: &str = "resolved";

pub(crate) const APPROVAL_DECISION_ALLOW: &str = "allow";
pub(crate) const APPROVAL_DECISION_ALLOW_ALWAYS: &str = "allow_always";
pub(crate) const APPROVAL_DECISION_DENY: &str = "deny";

// ---- serde readers with Go `encoding/json` semantics (rc_hub::watch) ----

/// Go-null tolerance: decode an explicit `null` as the default.
///
/// This is THE fold-side tolerance shim: Go's `json.Unmarshal` treats `null`
/// into a string/slice field as a NO-OP with no error, so a producer line
/// carrying `"reply":null` still folds — while a bare serde `String` field
/// treats the same `null` as a type error and kills the whole line. Every
/// decoded string/Vec field on the fold envelopes rides this. A genuinely WRONG
/// type (a number where a string belongs) still errors, exactly like Go.
pub(crate) fn null_default<'de, D, T>(d: D) -> Result<T, D::Error>
where
    D: serde::Deserializer<'de>,
    T: Default + Deserialize<'de>,
{
    Ok(Option::<T>::deserialize(d)?.unwrap_or_default())
}

/// Captures a raw field VERBATIM, `null` included (`Option<Box<RawValue>>`'s
/// stock decode maps `null` to `None`, but Go's `json.RawMessage` holds the four
/// bytes `null` and `compact_json` renders them — a tool input of `null` must
/// produce the detail `"null"`, not `""`).
pub(crate) fn raw_opt<'de, D>(d: D) -> Result<Option<Box<serde_json::value::RawValue>>, D::Error>
where
    D: serde::Deserializer<'de>,
{
    serde::Deserialize::deserialize(d).map(Some)
}

/// Decodes a nested OBJECT field with Go `encoding/json` semantics: an object
/// decodes, `null` (or absent, via `#[serde(default)]`) is `None`, and ANY other
/// JSON shape errors — serde derives would otherwise accept the positional
/// seq/tuple form (`["user",…]`) that Go rejects. Routed through a raw capture
/// so `RawValue` fields inside `T` keep their original bytes.
pub(crate) fn object_opt<'de, D, T>(d: D) -> Result<Option<T>, D::Error>
where
    D: serde::Deserializer<'de>,
    T: serde::de::DeserializeOwned,
{
    let raw: Box<serde_json::value::RawValue> = serde::Deserialize::deserialize(d)?;
    object_from_raw(&raw).map_err(serde::de::Error::custom)
}

/// The shape gate [`object_opt`] applies, factored out so the envelope's
/// raw-captured `properties` can be run through exactly the same rule AFTER the
/// decode rather than during it — the fold keeps those bytes for
/// `LaneApproval::request_json`, and `object_opt` would have consumed them.
pub(crate) fn object_from_raw<T>(
    raw: &serde_json::value::RawValue,
) -> Result<Option<T>, serde_json::Error>
where
    T: serde::de::DeserializeOwned,
{
    let s = raw.get().trim();
    if s.starts_with('{') {
        return serde_json::from_str::<T>(s).map(Some);
    }
    if s == "null" {
        return Ok(None);
    }
    Err(serde::de::Error::custom("expected a JSON object"))
}

/// Decodes an array-of-objects field with Go semantics: `null` is the nil slice
/// (empty), every element must be an OBJECT (Go's whole-array unmarshal errors
/// on a positional-form element where serde derives would accept it), and a
/// non-array errors. Raw-routed so `RawValue` fields inside `T` keep their
/// bytes.
pub(crate) fn vec_objects<'de, D, T>(d: D) -> Result<Vec<T>, D::Error>
where
    D: serde::Deserializer<'de>,
    T: serde::de::DeserializeOwned + Default,
{
    let raws: Option<Vec<Box<serde_json::value::RawValue>>> = serde::Deserialize::deserialize(d)?;
    let Some(raws) = raws else {
        return Ok(Vec::new());
    };
    raws.into_iter()
        .map(|r| {
            let s = r.get().trim();
            if s.starts_with('{') {
                return serde_json::from_str::<T>(s).map_err(serde::de::Error::custom);
            }
            if s == "null" {
                // Go's null-is-a-no-op applies at EVERY level, array elements
                // included: a null element decodes to the zero value.
                return Ok(T::default());
            }
            Err(serde::de::Error::custom("expected a JSON object element"))
        })
        .collect()
}

/// A `Vec<String>` field with FULL Go null semantics: the field itself may be
/// `null` (nil slice → empty) and so may any ELEMENT (Go's `[]string` decodes a
/// null element as `""`); a wrong-typed element still errors, like Go.
pub(crate) fn null_string_vec<'de, D>(d: D) -> Result<Vec<String>, D::Error>
where
    D: serde::Deserializer<'de>,
{
    let v: Option<Vec<Option<String>>> = serde::Deserialize::deserialize(d)?;
    Ok(v.unwrap_or_default()
        .into_iter()
        .map(Option::unwrap_or_default)
        .collect())
}

/// [`object_opt`] for a NON-pointer nested struct field (Go's value-typed nested
/// structs, e.g. a part's `time`): an object decodes, `null` no-ops to the zero
/// value, any other shape errors.
pub(crate) fn object_default<'de, D, T>(d: D) -> Result<T, D::Error>
where
    D: serde::Deserializer<'de>,
    T: serde::de::DeserializeOwned + Default,
{
    Ok(object_opt(d)?.unwrap_or_default())
}

/// The first non-whitespace byte of a JSON document (Go's decoder skips exactly
/// space/tab/newline/CR before the value). The fold gates its top-level struct
/// decode on `Some(b'{')` — Go's `Unmarshal` into a struct rejects any other
/// shape (and a top-level `null` no-ops into the zero value, which routes to the
/// same "not folded" outcome).
pub(crate) fn json_first_byte(line: &[u8]) -> Option<u8> {
    line.iter()
        .copied()
        .find(|b| !matches!(b, b' ' | b'\t' | b'\n' | b'\r'))
}

/// `firstNonEmpty` for the two-candidate case every fold call site has.
pub(crate) fn first_non_empty<'a>(a: &'a str, b: &'a str) -> &'a str {
    if !a.is_empty() {
        a
    } else {
        b
    }
}

/// Renders a raw JSON value as compact (whitespace-stripped) text — used for a
/// tool_use's input detail. Mirrors Go's `json.Compact`: the ORIGINAL bytes
/// minus inter-token whitespace — no reordering, no number reformatting —
/// falling back to the trimmed raw text when the value is not valid JSON.
pub(crate) fn compact_json(raw: &str) -> String {
    if raw.is_empty() {
        return String::new();
    }
    if serde_json::from_str::<serde::de::IgnoredAny>(raw).is_err() {
        return raw.trim().to_string();
    }
    // Strip whitespace outside string literals, byte-preserving inside them.
    let mut out = String::with_capacity(raw.len());
    let mut in_str = false;
    let mut escaped = false;
    for c in raw.chars() {
        if in_str {
            out.push(c);
            if escaped {
                escaped = false;
            } else if c == '\\' {
                escaped = true;
            } else if c == '"' {
                in_str = false;
            }
            continue;
        }
        match c {
            '"' => {
                in_str = true;
                out.push(c);
            }
            ' ' | '\t' | '\n' | '\r' => {}
            _ => out.push(c),
        }
    }
    out
}

// ---- text hygiene (rc_hub::messages) ----

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
    s.chars()
        .filter(|&r| {
            matches!(r, '\t' | '\n' | '\x0b' | '\x0c' | '\r')
                || !(r < '\x20' || r == '\x7f' || ('\u{80}'..='\u{9f}').contains(&r))
        })
        .collect()
}

/// Bounds a sanitized last-message preview. 200 runes is a one-to-two-line
/// preview on a phone — enough to recognize the message, small enough to keep
/// listing/SSE payloads tiny.
const MAX_LAST_MESSAGE_RUNES: usize = 200;

/// Turns raw agent text into a safe, compact one-line preview: strip ANSI escape
/// sequences, drop remaining control characters (C0 except whitespace, DEL, and
/// the C1 range — so a smuggled CSI can't survive), collapse every run of
/// whitespace to a single space, trim, and truncate to
/// [`MAX_LAST_MESSAGE_RUNES`] on a char boundary (never mid-codepoint). The
/// result is plain, single-line, bounded.
pub(crate) fn sanitize_last_message(s: &str) -> String {
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
pub(crate) fn trim_feed_text(s: &str) -> &str {
    s.trim_matches([' ', '\t', '\n', '\r'])
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn compact_json_preserves_key_order_and_strips_only_outside_strings() {
        assert_eq!(
            compact_json(r#"{ "b" : 1 , "a" : "x y" }"#),
            r#"{"b":1,"a":"x y"}"#
        );
        // Not valid JSON: the trimmed raw text is the fallback.
        assert_eq!(compact_json("  not json  "), "not json");
        assert_eq!(compact_json(""), "");
        // Go's RawMessage holds the four bytes `null`, and they render.
        assert_eq!(compact_json("null"), "null");
    }

    #[test]
    fn json_first_byte_skips_go_whitespace_only() {
        assert_eq!(json_first_byte(b"  \t\r\n{\"a\":1}"), Some(b'{'));
        assert_eq!(json_first_byte(b"[1]"), Some(b'['));
        assert_eq!(json_first_byte(b"   "), None);
    }

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
}
