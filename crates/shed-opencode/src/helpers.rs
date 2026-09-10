//! The fold's decode helpers, **copied** out of the rc hub — plus the feed
//! vocabulary and sanitizers, now **re-exported** from `shed_core::lane::feed`.
//!
//! Every item here is a verbatim port of a `shed-broker::rc_hub` helper the
//! opencode fold leans on: the Go-`encoding/json`-shaped serde readers from
//! `rc_hub::watch` (`compact_json`, `first_non_empty`, `json_first_byte`,
//! `null_default`, `null_string_vec`, `object_default`, `object_opt`, `raw_opt`,
//! `vec_objects`).
//!
//! # The feed vocabulary and the sanitizers moved down a layer
//!
//! `FEED_*`/`APPROVAL_*`, `sanitize_feed_text`, `sanitize_feed_token`,
//! `sanitize_last_message`, `trim_feed_text`, `truncate_bytes`, `bound_token`
//! and their caps now live in [`shed_core::lane::feed`]. They moved when the
//! SECOND lane adapter needed them: an adapter-to-adapter dependency
//! (`shed-gx` reaching into `shed-opencode` for a sanitizer) makes one
//! adapter's release schedule the other's. Behaviour is unchanged across the
//! move — every golden under `fixtures/` is byte-identical to what it was
//! before it.
//!
//! **Only the ones this crate still names directly are re-exported here**
//! (`sanitize_last_message`, `trim_feed_text`, and the `FEED_*`/`APPROVAL_*`
//! constants — see the `use` below). The rest — `sanitize_feed_text`,
//! `sanitize_feed_token`, `truncate_bytes`, `bound_token` and the caps — are
//! reached at [`shed_core::lane::feed`] by their remaining callers (the ring,
//! which is shed-core's own now) and are deliberately NOT re-listed here: a
//! re-export nothing uses is a name that goes stale silently.
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

use serde::Deserialize;

// ---- feed vocabulary + sanitizers (now `shed_core::lane::feed`) ----

// Re-exported, not redefined: `shed_core::lane::feed` is the one copy, and the
// `pub(crate)` here keeps `fold.rs`'s call sites spelled exactly as they were
// before the move. (`ring.rs` re-exports the ring straight from shed-core and
// needs none of these; `client.rs` reaches the ring, not the sanitizers.)
//
// Deliberately NOT blanket-`#[allow(unused_imports)]`: this list is the one
// place a name can go dead unnoticed after a move, and the compiler is a better
// guard against that than a test can be — an item reimplemented locally would
// shadow its import and be reported here.
pub(crate) use shed_core::lane::feed::{
    sanitize_last_message, trim_feed_text, APPROVAL_DECISION_ALLOW, APPROVAL_DECISION_ALLOW_ALWAYS,
    APPROVAL_DECISION_DENY, APPROVAL_STATUS_PENDING, APPROVAL_STATUS_RESOLVED, FEED_ROLE_ASSISTANT,
    FEED_ROLE_SYSTEM, FEED_ROLE_TOOL, FEED_ROLE_USER, FEED_TYPE_APPROVAL_REQUEST,
    FEED_TYPE_REASONING, FEED_TYPE_STATUS, FEED_TYPE_TEXT, FEED_TYPE_TOOL_RESULT,
    FEED_TYPE_TOOL_USE,
};

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
}
