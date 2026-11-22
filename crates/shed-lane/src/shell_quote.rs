//! POSIX single-quote command-line composer shared by remote transports.

/// Wrap `s` in single quotes, escaping embedded single quotes with the POSIX
/// `'\''` trick — a VERBATIM port of the guest's `shellQuote`.
///
/// **Always quotes**, even a token that needs no quoting (`my-shed/abc` →
/// `'my-shed/abc'`). That is deliberate and load-bearing: one quoter everywhere
/// means a remote command line reads identically whoever built it, and the
/// quoted form is written verbatim into interoperable artifacts (roost's
/// agent-hook entries, whose idempotent match compares the literal quoted
/// string).
pub fn shell_quote_always(s: &str) -> String {
    format!("'{}'", s.replace('\'', r"'\''"))
}

/// Render an argv as one copy-pasteable shell line — a `--print` output, a
/// summary, an error, and the single remote-command argument SSH transports
/// hand to the far side's shell.
pub fn display_line(argv: &[String]) -> String {
    argv.iter()
        .map(|a| shell_quote_always(a))
        .collect::<Vec<_>>()
        .join(" ")
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn shell_quote_always_wraps() {
        for (input, want) in [
            ("plain", "'plain'"),
            ("two words", "'two words'"),
            ("it's mine", r"'it'\''s mine'"),
            ("my-shed/abc", "'my-shed/abc'"),
            ("", "''"),
        ] {
            assert_eq!(shell_quote_always(input), want, "input {input:?}");
        }
    }

    #[test]
    fn display_line_is_copy_pasteable() {
        assert_eq!(
            display_line(&["ssh".into(), "a b".into()]),
            "'ssh' 'a b'",
            "always-quoted, like the engine's own transcripts"
        );
    }
}
