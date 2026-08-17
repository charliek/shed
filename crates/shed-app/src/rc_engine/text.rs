//! The two prompt-text helpers the engine needs from `internal/ext/rc/rc.go`.
//!
//! **Why they live here and not in `shed-core`:** C1 ported `rc.go`'s pure
//! REGISTRY surface (classifiers, metadata, quoting, slugs) plus
//! [`shed_core::rc_agents::has_control_chars`], the `SHED_RC_*` env-value gate.
//! These two are the *delivery-path* gate — the only consumer is this engine's
//! create/prompt validation — so they are engine-local rather than a third
//! control-character predicate in the kernel. (`shed-core` already carries a
//! FOURTH, deliberately-stricter client-side one, [`shed_core::rc::is_safe_rc_value`];
//! keeping the engine's next to its caller is what stops anyone "unifying" them
//! into a single wrong answer.) A follow-up may hoist them into `rc_agents`
//! alongside `has_control_chars`; the Go originals are one file apart too.

/// Collapse CRLF and lone CR to LF (`NormalizeNewlines`, `rc.go:265`), so a
/// multi-line prompt pasted from any platform is uniform before delivery.
///
/// Always runs BEFORE [`has_unsafe_prompt_chars`]: a raw CR is rejected there,
/// so normalizing second would reject every Windows-pasted prompt.
pub fn normalize_newlines(s: &str) -> String {
    s.replace("\r\n", "\n").replace('\r', "\n")
}

/// Whether `s` carries a control character that must not appear in a kickoff
/// prompt (`HasUnsafePromptChars`, `rc.go:276`).
///
/// Newlines and tabs are ALLOWED — a multi-line prompt is delivered through a
/// bracketed paste ([`super::tmux::Tmux::send_block`]) where they are input, not
/// submission. Everything else is rejected:
///
/// - C0 (`<= 0x1f`), notably **ESC**, so a paste cannot smuggle the
///   bracketed-paste end sequence and break out into raw keystrokes;
/// - DEL (`0x7f`);
/// - C1 (`0x80`–`0x9f`), e.g. the 8-bit CSI `0x9b`, which terminals honoring C1
///   would treat as a control sequence introducer.
///
/// Go iterates `for _, r := range s`, i.e. over RUNES, and this iterates over
/// `char` — the same thing for the valid UTF-8 both sides have already
/// guaranteed by this point (Rust `str` cannot be otherwise; Go's callers run an
/// explicit `utf8.ValidString` first, precisely so an invalid byte cannot slip
/// past as `RuneError`).
pub fn has_unsafe_prompt_chars(s: &str) -> bool {
    s.chars().any(|c| {
        if c == '\n' || c == '\t' {
            return false;
        }
        c <= '\u{1f}' || c == '\u{7f}' || ('\u{80}'..='\u{9f}').contains(&c)
    })
}

/// Quote `s` the way Go's `%q` verb (`strconv.Quote`) does, for the error
/// messages that embed a CALLER-CONTROLLED value (bad slug, unknown kind, plan
/// path). Rust's `{:?}` differs from `%q` exactly where these messages are
/// reached — control characters (`{:?}` renders ESC as `\u{1b}`, `%q` as
/// `\x1b`) — and the parity harness diffs the exit-2 class messages, so the
/// spelling is contract. Coverage matches `%q` for: `"`/`\` escapes, the
/// named C0 escapes (`\a\b\f\n\r\t\v`), remaining C0 + DEL as `\xNN`,
/// printable text (ASCII and non-ASCII) verbatim, and other non-printables as
/// `\uNNNN`/`\UNNNNNNNN`. (Go's `IsPrint` table and Rust's notion of printable
/// diverge on a few exotic codepoints — out of contract, documented here.)
pub fn quote_go(s: &str) -> String {
    let mut out = String::with_capacity(s.len() + 2);
    out.push('"');
    for c in s.chars() {
        match c {
            '"' => out.push_str("\\\""),
            '\\' => out.push_str("\\\\"),
            '\u{7}' => out.push_str("\\a"),
            '\u{8}' => out.push_str("\\b"),
            '\u{c}' => out.push_str("\\f"),
            '\n' => out.push_str("\\n"),
            '\r' => out.push_str("\\r"),
            '\t' => out.push_str("\\t"),
            '\u{b}' => out.push_str("\\v"),
            c if (c as u32) < 0x20 || c == '\u{7f}' => {
                out.push_str(&format!("\\x{:02x}", c as u32));
            }
            c if (c as u32) < 0x80 => out.push(c),
            // Non-ASCII: printable verbatim (Go IsPrint), else \u / \U.
            c if !c.is_control() => out.push(c),
            c if (c as u32) <= 0xffff => out.push_str(&format!("\\u{:04x}", c as u32)),
            c => out.push_str(&format!("\\U{:08x}", c as u32)),
        }
    }
    out.push('"');
    out
}

#[cfg(test)]
mod tests {
    use super::*;

    // Pins quote_go against Go `%q` outputs (spot-checked with `go run`).
    #[test]
    fn quote_go_matches_strconv_quote() {
        for (input, want) in [
            ("plain", r#""plain""#),
            ("my-shed/abc", r#""my-shed/abc""#),
            ("a\u{1b}b", r#""a\x1bb""#), // ESC — the reachable {:?} divergence
            ("a\"b\\c", r#""a\"b\\c""#),
            ("tab\there", r#""tab\there""#),
            ("nl\n", r#""nl\n""#),
            ("bel\u{7}", r#""bel\a""#),
            ("del\u{7f}", r#""del\x7f""#),
            ("café", r#""café""#),         // printable non-ASCII verbatim
            ("c1\u{9b}", "\"c1\\u009b\""), // C1 control -> \uNNNN, like Go %q
        ] {
            assert_eq!(quote_go(input), want, "input {input:?}");
        }
    }

    // Mirrors Go TestPromptCharGuards (rc_test.go:513).
    #[test]
    fn unsafe_prompt_chars_allow_newline_and_tab() {
        for ok in ["a\nb", "a\tb", "plain", "multi\nline\nplan", ""] {
            assert!(!has_unsafe_prompt_chars(ok), "{ok:?} should be allowed");
        }
        for bad in [
            "a\u{1b}b",  // ESC — the bracketed-paste breakout
            "a\u{0}b",   // NUL
            "a\rb",      // raw CR (normalize first)
            "bell\u{7}", // BEL
            "a\u{9b}b",  // 8-bit CSI
            "c\u{80}",   // C1 low edge
            "d\u{9f}",   // C1 high edge
            "e\u{7f}",   // DEL
        ] {
            assert!(has_unsafe_prompt_chars(bad), "{bad:?} should be rejected");
        }
        // Non-ASCII printable text is fine (the C1 range is codepoints, not bytes).
        assert!(!has_unsafe_prompt_chars("café — ✅ 日本語"));
    }

    #[test]
    fn normalize_newlines_collapses_cr_and_crlf() {
        assert_eq!(normalize_newlines("a\r\nb\rc"), "a\nb\nc");
        assert_eq!(normalize_newlines("no newlines"), "no newlines");
        assert_eq!(normalize_newlines("\r\n\r\n"), "\n\n");
    }
}
