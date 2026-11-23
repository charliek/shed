//! Dart-parity string trimming and display sanitization helpers.
//!
//! Shared by `shed-lane`'s RC/feed decoders and `shed-core`'s wire model
//! decoders so both sides trim and sanitize identically to Dart's
//! `String.trim()` + `_cleanDisplay`.

use serde_json::Value;

use crate::rc::strip_format_chars;

/// Trim on Dart's `String.trim()` set: Unicode White_Space PLUS U+FEFF (BOM).
/// Rust's `str::trim` uses only the White_Space property, which U+FEFF lost in
/// Unicode 6.3 — Dart keeps it in its trim set for legacy reasons (documented
/// on `String.trim`). Every `_str`/`_nonEmpty`-parity helper below must trim
/// on this set, or a BOM-padded guest value decodes differently here than on
/// mobile (e.g. a `"\u{FEFF}"` shed name would survive as non-empty). FEFF is
/// the only practical divergence — both languages otherwise share the Unicode
/// whitespace set.
pub fn dart_trim(s: &str) -> &str {
    s.trim_matches(|c: char| c.is_whitespace() || c == '\u{FEFF}')
}

/// Trim a maybe-string to a non-empty value (Dart's `_nonEmpty`,
/// `shed_dtos.dart:421-422`, and `_str`, `rc_models.dart:278-282` — identical
/// semantics): non-string, absent, or blank → `None`. Trims on Dart's set via
/// [`dart_trim`].
pub fn non_empty_str(v: Option<&Value>) -> Option<&str> {
    v.and_then(Value::as_str)
        .map(dart_trim)
        .filter(|s| !s.is_empty())
}

/// [`non_empty_str`], owned (the Dart `_str` used for the session's optional
/// string fields; `rc_feed.dart:85-89` carries a byte-identical `_str`, so
/// the feed decoders in [`crate::rc`] share this helper).
pub fn opt_trimmed(v: Option<&Value>) -> Option<String> {
    non_empty_str(v).map(str::to_string)
}

/// Dart `_cleanDisplay` (`rc_models.dart:284-290`): [`opt_trimmed`] plus a
/// strip of Unicode format characters (category Cf) — for guest-controlled
/// display text that could carry bidi-override spoofers. A value that is ONLY
/// format characters degrades to `None`. `rc_feed.dart:93-98` carries a
/// byte-identical `_text`, so the feed decoders in [`crate::rc`] share this
/// helper too.
pub fn clean_display(v: Option<&Value>) -> Option<String> {
    let stripped = strip_format_chars(non_empty_str(v)?);
    // Dart-set trim for parity ([`dart_trim`]) — though FEFF is Cf, so the
    // strip above already removed it and this pass only sees whitespace.
    let trimmed = dart_trim(&stripped);
    if trimmed.is_empty() {
        None
    } else {
        Some(trimmed.to_string())
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use serde_json::json;

    #[test]
    fn opt_trimmed_trims_on_darts_set_including_bom() {
        let v = json!("\u{FEFF} x \u{FEFF}");
        assert_eq!(opt_trimmed(Some(&v)).as_deref(), Some("x"));
        let bom_only = json!("\u{FEFF} \u{FEFF}");
        assert_eq!(opt_trimmed(Some(&bom_only)), None);
        assert_eq!(clean_display(Some(&bom_only)), None);
        let ws = json!("\u{00A0}\u{2003}hi\u{3000}");
        assert_eq!(opt_trimmed(Some(&ws)).as_deref(), Some("hi"));
    }
}
