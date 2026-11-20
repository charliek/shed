//! **Adding a `machines:` entry to `~/.shed/config.yaml`, without owning it.**
//!
//! That file is written and maintained by hand. It carries comments, a chosen
//! indentation, and sections this crate does not model — the reader
//! ([`crate::config`]) is deliberately partial, so a parse-and-re-serialize
//! round trip would silently delete everything it does not understand. There is
//! no YAML *writer* here and this is not one.
//!
//! So the rule is **insert-only**: every line that was in the file is in the
//! output, byte for byte, in the same order. The edit only ever adds lines.
//! That makes the worst case "a stanza landed somewhere odd" rather than "the
//! config lost its comments", and it means a failed edit can never be
//! destructive.
//!
//! Two shapes, because a config either already has a `machines:` block or does
//! not:
//!
//! * **It does** — insert the new entry as the block's FIRST child, immediately
//!   after the `machines:` line, indented to match the existing children. First
//!   rather than last because finding where a block ends means deciding what a
//!   trailing comment belongs to, and guessing wrong there moves someone's
//!   words.
//! * **It does not** — append a fresh `machines:` block at the end, after a
//!   blank line.

/// Why an insert could not be made. Both are conditions the caller should show
/// the user rather than retry.
#[derive(Debug, PartialEq)]
pub enum EditError {
    /// A machine with this name is already configured. Silently overwriting one
    /// would be an edit to a line the user wrote, which this module never does.
    Duplicate(String),
    /// The name would not survive the round trip — it has to be a plain scalar
    /// key, or the file it lands in stops parsing.
    BadName(String),
    /// The edit was built, read back, and the reader did not see what was asked
    /// for — so it is discarded rather than written.
    ///
    /// The backstop for the fact that the reader is PARTIAL and has limits the
    /// writer must not try to mirror (a quoted value must close on its line, for
    /// one). Rather than model each one and get it subtly wrong, the writer
    /// proposes and the reader disposes — whatever the reader makes of the new
    /// text IS the truth, and if that is not what was asked for, nothing is
    /// written.
    WouldNotRoundTrip(String),
}

impl std::fmt::Display for EditError {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        match self {
            EditError::Duplicate(n) => write!(f, "a machine named {n:?} is already configured"),
            EditError::BadName(n) => write!(
                f,
                "{n:?} is not a usable machine name (letters, digits, dot, dash, underscore)"
            ),
            EditError::WouldNotRoundTrip(why) => write!(f, "{why}"),
        }
    }
}

/// The fields an inserted entry carries. `None` means "leave it out and let the
/// reader's default apply" — which is why adding `mini3` can be one field.
pub struct NewMachine<'a> {
    pub name: &'a str,
    pub host: Option<&'a str>,
    pub user: Option<&'a str>,
    pub ssh_port: Option<u16>,
}

/// Return `text` with `machine` inserted, or an [`EditError`].
///
/// Pure: the caller does the reading and writing, so this is testable against
/// real config text without touching a disk.
pub fn insert_machine(text: &str, machine: &NewMachine<'_>) -> Result<String, EditError> {
    if !is_plain_key(machine.name) {
        return Err(EditError::BadName(machine.name.to_string()));
    }
    // A control character or a line break inside a value (a raw CR, a LF, U+2028) is
    // refused rather than written: a YAML reader folds or refuses a line break inside a
    // quoted value, so Go's reader would read back something else than this one does,
    // and no host or user name holds one. So is a character YAML does not allow in a file
    // at all (U+FFFE, U+FFFF; [`yaml_printable`]): Go's reader would refuse the whole file.
    for (field, value) in [("host", machine.host), ("user", machine.user)] {
        if value.is_some_and(|v| v.chars().any(|c| breaks_a_line(c) || !yaml_printable(c))) {
            return Err(EditError::WouldNotRoundTrip(format!(
                "the {field} holds a control character, a line break or a character YAML \
                 does not allow in a file, which the config file cannot carry as written"
            )));
        }
    }
    let before = crate::config::ShedConfig::parse(text);
    // A file the reader does not read at all is not extended: nothing it holds could be
    // checked against the edit.
    if let Some(reason) = &before.refused {
        return Err(EditError::WouldNotRoundTrip(format!(
            "config.yaml is not read as it is ({reason}); fix it before adding a machine"
        )));
    }
    if before.machine(machine.name).is_some() {
        return Err(EditError::Duplicate(machine.name.to_string()));
    }

    // Split keeping track of the terminator, so a CRLF file stays CRLF. The
    // promise is byte-for-byte, and rewriting every line ending breaks it as
    // surely as dropping a comment would.
    let (lines, eol) = split_lines(text);
    let out = match find_machines_block(&lines) {
        Some(at) => {
            // Match the indent the block's existing children use, so the file
            // keeps one style rather than acquiring a second.
            let indent = child_indent(&lines, at).unwrap_or(4);
            let mut out: Vec<String> = lines[..=at].iter().map(|l| (*l).to_string()).collect();
            out.extend(entry_lines(machine, indent));
            out.extend(lines[at + 1..].iter().map(|l| (*l).to_string()));
            out
        }
        None => {
            // No block this writer can see. If the READER nonetheless finds
            // machines, the file states them in a shape we cannot locate — a
            // quoted key, `machines :`, something nested — and appending a
            // second `machines:` would let the reader's map take the later one,
            // making every existing machine vanish. Refuse instead.
            if !before.machines.is_empty() {
                return Err(EditError::WouldNotRoundTrip(
                    "this config already declares machines in a form the editor \
                     cannot safely extend — add the entry by hand"
                        .to_string(),
                ));
            }
            let mut out: Vec<String> = lines.iter().map(|l| (*l).to_string()).collect();
            if out.last().is_some_and(|l| !l.trim().is_empty()) {
                out.push(String::new());
            }
            out.push("machines:".to_string());
            out.extend(entry_lines(machine, 4));
            out
        }
    };
    let after_text = join_like(text, &out, eol);
    verify(&before, &after_text, machine)?;
    Ok(after_text)
}

/// The reader's verdict on the proposed text.
///
/// Two claims, both checked against the reader rather than against intent:
/// nothing that was configured may have changed or disappeared, and the new
/// entry must read back as exactly what was asked for.
fn verify(
    before: &crate::config::ShedConfig,
    after_text: &str,
    intended: &NewMachine<'_>,
) -> Result<(), EditError> {
    let after = crate::config::ShedConfig::parse(after_text);

    for old in &before.machines {
        match after.machine(&old.name) {
            Some(now) if now == old => {}
            Some(_) => {
                return Err(EditError::WouldNotRoundTrip(format!(
                    "the edit would change the existing machine {:?}",
                    old.name
                )))
            }
            None => {
                return Err(EditError::WouldNotRoundTrip(format!(
                    "the edit would drop the existing machine {:?}",
                    old.name
                )))
            }
        }
    }

    let Some(written) = after.machine(intended.name) else {
        return Err(EditError::WouldNotRoundTrip(format!(
            "{:?} was written but the config reader does not see it",
            intended.name
        )));
    };
    if *written != expected_entry(intended) {
        return Err(EditError::WouldNotRoundTrip(format!(
            "{:?} does not read back as written — a value is not representable \
             here (a '#' or a quote in a value is the usual cause)",
            intended.name
        )));
    }
    Ok(())
}

/// What the reader should produce for `m`, given its own defaulting rules.
fn expected_entry(m: &NewMachine<'_>) -> crate::config::MachineEntry {
    crate::config::MachineEntry {
        name: m.name.to_string(),
        host: m
            .host
            .filter(|h| !h.is_empty())
            .unwrap_or(m.name)
            .to_string(),
        user: m.user.filter(|u| !u.is_empty()).map(str::to_string),
        ssh_port: m.ssh_port.unwrap_or(22),
        ..Default::default()
    }
}

/// Lines WITHOUT their terminators, plus the terminator the file uses.
fn split_lines(text: &str) -> (Vec<&str>, &'static str) {
    let eol = if text.contains("\r\n") { "\r\n" } else { "\n" };
    let mut lines: Vec<&str> = text
        .split('\n')
        .map(|l| l.strip_suffix('\r').unwrap_or(l))
        .collect();
    // `split` yields a trailing "" for text ending in a newline; drop it and
    // let `join_like` put the terminator back.
    if text.ends_with('\n') {
        lines.pop();
    }
    (lines, eol)
}

/// The index of a top-level `machines:` line, if the file has one.
///
/// Top-level ONLY (column zero): a `machines:` nested under something else is a
/// different key that happens to share a name, and inserting into it would put
/// the entry somewhere the reader never looks.
fn find_machines_block(lines: &[&str]) -> Option<usize> {
    lines.iter().position(|l| {
        let no_comment = l.split('#').next().unwrap_or("");
        no_comment == "machines:" || no_comment.trim_end() == "machines:"
    })
}

/// The indent the block's first existing child uses, so a second entry lines up
/// with the first instead of introducing a new convention.
fn child_indent(lines: &[&str], block_at: usize) -> Option<usize> {
    lines[block_at + 1..]
        .iter()
        .find(|l| !l.trim().is_empty() && !l.trim_start().starts_with('#'))
        .and_then(|l| {
            let n = l.len() - l.trim_start().len();
            (n > 0).then_some(n)
        })
}

fn entry_lines(m: &NewMachine<'_>, indent: usize) -> Vec<String> {
    let pad = " ".repeat(indent);
    let inner = " ".repeat(indent * 2);
    let mut out = vec![format!("{pad}{}:", m.name)];
    let field = |k: &str, v: &str| format!("{inner}{k}: {}", scalar(v));
    // `host` is written even when it equals the name: the reader defaults it,
    // but a person reading the file should not have to know that rule.
    out.push(field(
        "host",
        m.host.filter(|h| !h.is_empty()).unwrap_or(m.name),
    ));
    if let Some(u) = m.user.filter(|u| !u.is_empty()) {
        out.push(field("user", u));
    }
    if let Some(p) = m.ssh_port.filter(|p| *p != 22) {
        out.push(format!("{inner}ssh_port: {p}"));
    }
    out
}

/// A value as the file carries it: bare only when it is a token no YAML reader
/// can take for anything but its text; otherwise a double-quoted scalar.
///
/// What this writer emits is read back as the same string by its own reader
/// AND by a YAML parser (the Go CLI's, the broker's), so the bare set is defined
/// narrowly (plan 037 A8): ASCII letters, digits and `. _ - / @ :` only;
/// beginning with a letter, `_` or `/` (never with a digit, so no number, date
/// or time is ever bare, and never with `-`, `.`, `@` or `:`, which YAML gives a
/// meaning there); not ending in `:`; and not a word YAML reads as a boolean or
/// a null in any case (`yes`, `Off`, `NULL`…). Everything else (`&a x`,
/// `!!str x`, `*x`, `| x`, a quote, a space, `: `, ` #`, a leading digit…) is
/// written quoted. Inside the quotes only `\` and `"` need escaping: a control
/// character or a line break never reaches here ([`breaks_a_line`]).
fn scalar(v: &str) -> String {
    let token_char =
        |c: char| c.is_ascii_alphanumeric() || matches!(c, '.' | '_' | '-' | '/' | '@' | ':');
    let typed_word = matches!(
        v.to_ascii_lowercase().as_str(),
        "true" | "false" | "yes" | "no" | "on" | "off" | "y" | "n" | "null"
    );
    let bare = v.chars().all(token_char)
        && v.starts_with(|c: char| c.is_ascii_alphabetic() || c == '_' || c == '/')
        && !v.ends_with(':')
        && !typed_word;
    if bare {
        v.to_string()
    } else {
        format!("\"{}\"", v.replace('\\', "\\\\").replace('"', "\\\""))
    }
}

/// A character the writer never puts in a value: a control character (a CR, a LF, a tab,
/// NEL U+0085 and the rest) or a line or paragraph separator (U+2028, U+2029), each of
/// which a YAML reader may read as a line break.
fn breaks_a_line(c: char) -> bool {
    c.is_control() || matches!(c, '\u{2028}' | '\u{2029}')
}

/// YAML 1.2's printable set (`c-printable`): the characters a YAML file may hold. Every
/// other character is refused by a YAML reader wherever it appears, quoted or not
/// (gopkg.in/yaml.v3 refuses the whole file), so the writer never writes one. Its
/// complement is the C0 controls but tab, LF and CR; DEL; the C1 controls but NEL; the
/// surrogates (which a Rust `char` cannot hold); and U+FFFE and U+FFFF.
fn yaml_printable(c: char) -> bool {
    matches!(c,
        '\t' | '\n' | '\r' | '\u{20}'..='\u{7e}' | '\u{85}' | '\u{a0}'..='\u{d7ff}'
            | '\u{e000}'..='\u{fffd}' | '\u{10000}'..='\u{10ffff}')
}

/// A key that survives the reader and stays a plain scalar in the file.
fn is_plain_key(name: &str) -> bool {
    !name.is_empty()
        && name
            .chars()
            .all(|c| c.is_ascii_alphanumeric() || matches!(c, '.' | '-' | '_'))
}

/// Rejoin, preserving whether the original ended with a newline — so a diff of
/// the file shows the inserted lines and nothing else.
fn join_like(original: &str, lines: &[String], eol: &str) -> String {
    let mut s = lines.join(eol);
    if original.ends_with('\n') || original.is_empty() {
        s.push_str(eol);
    }
    s
}

#[cfg(test)]
mod tests {
    use super::*;

    fn m(name: &str) -> NewMachine<'_> {
        NewMachine {
            name,
            host: None,
            user: None,
            ssh_port: None,
        }
    }

    /// The property the whole module exists for: every original line survives,
    /// in order, byte for byte. Comments, spacing, and unmodelled keys included.
    #[test]
    fn every_original_line_survives_in_order() {
        let orig = "\
# my config, hand written
servers:
    mac-mini:
        host: localhost   # trailing comment
        ssh_port: 2222

# machines I reach over ssh
machines:
    mini3:
        host: mini3

some_key_this_crate_does_not_model:
    nested: true
";
        let out = insert_machine(orig, &m("mini4")).unwrap();
        let mut before = orig.lines();
        // Every original line appears, in order, somewhere in the output.
        for line in out.lines() {
            if let Some(peek) = before.clone().next() {
                if line == peek {
                    before.next();
                }
            }
        }
        assert_eq!(
            before.next(),
            None,
            "an original line was dropped or reordered"
        );
        // …and the insert actually happened, so "survives" is not satisfied by
        // returning the input untouched.
        assert!(crate::config::ShedConfig::parse(&out)
            .machine("mini4")
            .is_some());
        assert_eq!(
            out.lines().count(),
            orig.lines().count() + 2,
            "exactly the two new lines, nothing else: {out}"
        );
        assert!(out.contains("some_key_this_crate_does_not_model"));
        assert!(out.contains("# trailing comment"));
        assert!(out.contains("# machines I reach over ssh"));
    }

    #[test]
    fn inserts_into_an_existing_block_at_its_own_indent() {
        let orig = "machines:\n  mini3:\n    host: mini3\n";
        let out = insert_machine(orig, &m("mini4")).unwrap();
        // Two-space children in, two-space children out.
        assert!(out.contains("\n  mini4:\n    host: mini4\n"), "{out}");
        assert!(out.contains("  mini3:"));
        // …and it parses back with BOTH.
        let cfg = crate::config::ShedConfig::parse(&out);
        assert!(cfg.machine("mini3").is_some() && cfg.machine("mini4").is_some());
    }

    #[test]
    fn creates_the_block_when_there_is_none() {
        let orig = "servers:\n    mac-mini:\n        host: localhost\n";
        let out = insert_machine(orig, &m("mini3")).unwrap();
        assert!(
            out.starts_with(orig),
            "the original must be a prefix: {out}"
        );
        assert!(out.contains("\nmachines:\n"));
        assert!(crate::config::ShedConfig::parse(&out)
            .machine("mini3")
            .is_some());
    }

    #[test]
    fn writes_every_supplied_field_and_omits_the_defaults() {
        let full = NewMachine {
            name: "mini3",
            host: Some("100.64.0.3"),
            user: Some("charliek"),
            ssh_port: Some(2200),
        };
        let out = insert_machine("", &full).unwrap();
        let cfg = crate::config::ShedConfig::parse(&out);
        let e = cfg.machine("mini3").expect("parses back");
        assert_eq!(e.host, "100.64.0.3");
        assert_eq!(e.user.as_deref(), Some("charliek"));
        assert_eq!(e.ssh_port, 2200);

        // The default port is left out rather than written — the file should not
        // fill with values that only restate the default.
        let bare = insert_machine("", &m("mini4")).unwrap();
        assert!(!bare.contains("ssh_port"), "{bare}");
        assert_eq!(
            crate::config::ShedConfig::parse(&bare)
                .machine("mini4")
                .unwrap()
                .ssh_port,
            22
        );
    }

    #[test]
    fn refuses_a_duplicate_rather_than_overwriting() {
        let orig = "machines:\n    mini3:\n        host: mini3\n";
        assert_eq!(
            insert_machine(orig, &m("mini3")),
            Err(EditError::Duplicate("mini3".into()))
        );
    }

    #[test]
    fn refuses_a_name_that_would_not_survive_the_round_trip() {
        for bad in ["", "a: b", "has space", "quote\"d", "new\nline", "#hash"] {
            assert!(
                matches!(insert_machine("", &m(bad)), Err(EditError::BadName(_))),
                "accepted {bad:?}"
            );
        }
    }

    /// A `machines:` nested under another key is a different key that happens to
    /// share a name — inserting there would put the entry where the reader never
    /// looks, so the top-level block is created instead.
    #[test]
    fn ignores_a_nested_machines_key() {
        let orig = "something:\n    machines:\n        not-ours: 1\n";
        let out = insert_machine(orig, &m("mini3")).unwrap();
        assert!(out.starts_with(orig));
        assert!(out.contains("\nmachines:\n"));
        assert!(crate::config::ShedConfig::parse(&out)
            .machine("mini3")
            .is_some());
        assert!(crate::config::ShedConfig::parse(&out)
            .machine("not-ours")
            .is_none());
    }

    #[test]
    fn a_value_needing_quotes_gets_them() {
        let odd = NewMachine {
            name: "mini3",
            host: Some("host: with colon"),
            user: None,
            ssh_port: None,
        };
        let out = insert_machine("", &odd).unwrap();
        assert!(out.contains("\"host: with colon\""), "{out}");
    }

    /// codex review: the reader accepts key forms this writer cannot locate
    /// (`"machines":`, `machines :`). Appending a SECOND `machines:` would let
    /// the reader's map take the later block and every existing machine would
    /// silently vanish. Refusing is the only safe answer.
    #[test]
    fn refuses_rather_than_shadowing_a_block_it_cannot_see() {
        for weird in [
            "\"machines\":\n    mini3:\n        host: mini3\n",
            "machines :\n    mini3:\n        host: mini3\n",
        ] {
            // Precondition: the reader really does see a machine here, which is
            // what makes appending dangerous.
            assert!(
                crate::config::ShedConfig::parse(weird)
                    .machine("mini3")
                    .is_some(),
                "precondition: reader sees mini3 in {weird:?}"
            );
            let out = insert_machine(weird, &m("mini4"));
            assert!(
                matches!(out, Err(EditError::WouldNotRoundTrip(_))),
                "accepted a config it could not safely extend: {weird:?} -> {out:?}"
            );
        }
    }

    /// codex review: a value the reader would read back as something else must
    /// be refused rather than configure a machine pointing somewhere else. The
    /// verifier decides that, not a guess about which values look risky.
    ///
    /// Since plan 037 A8 the reader reads a quoted value as YAML does (a `#`
    /// inside quotes is text, `\"` is a quote), so `"mini#prod"` and
    /// `"has\"quote"`, which it used to mangle, round-trip and are accepted, as
    /// written; a value whose quoting spans a line still does not, and is refused.
    #[test]
    fn refuses_a_value_the_reader_would_mangle() {
        for good_host in ["mini#prod", "has\"quote", "back\\slash"] {
            let entry = NewMachine {
                name: "mini3",
                host: Some(good_host),
                user: None,
                ssh_port: None,
            };
            let out = insert_machine("", &entry).expect(good_host);
            let read = crate::config::ShedConfig::parse(&out);
            assert_eq!(read.machine("mini3").unwrap().host, good_host);
        }
        // A control character or a line break inside a value is refused, never written:
        // `a#b\rc` is quoted for its `#`, and a raw CR inside the quotes reads back intact
        // here but folded by a YAML reader such as Go's.
        for bad in [
            "a#b\rc",
            "two\nlines",
            "tab\there",
            "nel\u{85}x",
            "ls\u{2028}x",
        ] {
            for (host, user) in [(Some(bad), None), (None, Some(bad))] {
                let entry = NewMachine {
                    name: "mini3",
                    host,
                    user,
                    ssh_port: None,
                };
                let out = insert_machine("", &entry);
                assert!(
                    matches!(out, Err(EditError::WouldNotRoundTrip(_))),
                    "accepted {bad:?} -> {out:?}"
                );
            }
        }
    }

    /// A character outside YAML's printable set is refused, never written (plan 037 A8,
    /// third review: `a\u{ffff}b` was written quoted, read back by this reader and the
    /// broker's, and made Go's reader refuse the whole file). `char::is_control` does not
    /// cover U+FFFE and U+FFFF; the printable characters around them are still written.
    #[test]
    fn refuses_a_character_yaml_does_not_allow() {
        for bad in [
            "a\u{ffff}b",
            "a\u{fffe}b",
            "a\u{7f}b",
            "a\u{9f}b",
            "a\u{1}b",
        ] {
            for (host, user) in [(Some(bad), None), (None, Some(bad))] {
                let out = insert_machine(
                    "",
                    &NewMachine {
                        name: "box",
                        host,
                        user,
                        ssh_port: None,
                    },
                );
                assert!(
                    matches!(&out, Err(EditError::WouldNotRoundTrip(why)) if why.contains("YAML does not allow")),
                    "accepted {bad:?} -> {out:?}"
                );
            }
        }
        // The edges of the set: each is printable to YAML, and round-trips.
        for good in [
            "a\u{a0}b",
            "a\u{d7ff}b",
            "a\u{e000}b",
            "a\u{fffd}b",
            "a\u{feff}b",
            "a\u{10000}b",
            "a\u{10ffff}b",
        ] {
            let out = insert_machine(
                "",
                &NewMachine {
                    name: "box",
                    host: Some(good),
                    user: None,
                    ssh_port: None,
                },
            )
            .unwrap_or_else(|e| panic!("{good:?}: {e}"));
            let read = crate::config::ShedConfig::parse(&out);
            assert_eq!(read.machine("box").unwrap().host, good);
        }
        // The set is exactly YAML 1.2's `c-printable`, character by character.
        for code in 0..=0x10ffffu32 {
            let Some(c) = char::from_u32(code) else {
                continue;
            };
            let printable = matches!(code, 0x9 | 0xa | 0xd | 0x85)
                || (0x20..=0x7e).contains(&code)
                || (0xa0..=0xd7ff).contains(&code)
                || (0xe000..=0xfffd).contains(&code)
                || (0x10000..=0x10ffff).contains(&code);
            assert_eq!(yaml_printable(c), printable, "U+{code:04X}");
        }
    }

    /// A file the reader refuses whole (a raw NEL in a comment, here) is not extended:
    /// the edit is refused, saying why.
    #[test]
    fn a_file_the_reader_refuses_is_not_extended() {
        let text = "servers:\n    prod:\n        host: prod.example # a\u{85}b\n";
        let out = insert_machine(text, &m("mini3"));
        assert!(
            matches!(&out, Err(EditError::WouldNotRoundTrip(why)) if why.contains("U+0085")),
            "{out:?}"
        );
    }

    /// The values YAML gives another meaning than their text when bare (plan 037 A8
    /// review: `&a other.example` is `other.example` to Go, `!!str other.example`
    /// too) are each written as a double-quoted scalar, and read back as the same
    /// string. The broker's YAML parser reads the same bytes the same way
    /// (`shed_broker::discovery`'s test of this writer).
    #[test]
    fn a_value_yaml_would_read_otherwise_is_written_double_quoted() {
        for &value in QUOTED_VALUES {
            for (host, user) in [(Some(value), None), (None, Some(value))] {
                let entry = NewMachine {
                    name: "box",
                    host,
                    user,
                    ssh_port: None,
                };
                let out = insert_machine("", &entry).unwrap_or_else(|e| panic!("{value:?}: {e}"));
                let field = if host.is_some() { "host" } else { "user" };
                let quoted = format!("\"{}\"", value.replace('\\', "\\\\").replace('"', "\\\""));
                assert!(
                    out.lines()
                        .any(|l| l.trim_start() == format!("{field}: {quoted}")),
                    "{value:?} is not written as a double-quoted scalar: {out}"
                );
                let read = crate::config::ShedConfig::parse(&out);
                let machine = read.machine("box").unwrap();
                match host {
                    Some(_) => assert_eq!(machine.host, value),
                    None => assert_eq!(machine.user.as_deref(), Some(value)),
                }
            }
        }
        // A plain token stays bare.
        for value in ["mini3", "pat", "host.example.test", "/srv/x", "a_b-c@d:e"] {
            let out = insert_machine(
                "",
                &NewMachine {
                    name: "box",
                    host: Some(value),
                    user: None,
                    ssh_port: None,
                },
            )
            .unwrap();
            assert!(out.contains(&format!("host: {value}\n")), "{out}");
        }
    }

    /// The values [`a_value_yaml_would_read_otherwise_is_written_double_quoted`] holds the
    /// writer to (the broker's test of the same bytes carries its own copy).
    const QUOTED_VALUES: &[&str] = &[
        "&a other.example",
        "!!str other.example",
        "*alias",
        "|literal",
        ">folded",
        "-dash",
        "- item",
        "?question",
        "{a: b}",
        "[a]",
        "#hash",
        "%percent",
        "@at",
        "`tick",
        "\"quote",
        "'apostrophe",
        "a: b",
        "a #b",
        "a:",
        ":a",
        ".dot",
        "100.64.0.3",
        "2026-01-02",
        "12:30",
        "0x1F",
        "yes",
        "Off",
        "NULL",
        "~",
        "back\\slash",
        "two words",
        "clé",
    ];

    /// codex review: `lines()` drops `\r`, and joining with `\n` would rewrite
    /// every terminator in the file — byte-for-byte has to include how each
    /// line ended.
    #[test]
    fn a_crlf_file_stays_crlf() {
        let orig = "servers:\r\n    mac-mini:\r\n        host: localhost\r\n";
        let out = insert_machine(orig, &m("mini3")).unwrap();
        assert!(
            out.starts_with(orig),
            "original bytes not preserved: {out:?}"
        );
        assert!(!out.contains("\n\n"), "a bare LF crept in: {out:?}");
        assert_eq!(
            out.matches("\r\n").count(),
            out.matches('\n').count(),
            "every newline should still be a CRLF: {out:?}"
        );
        assert!(crate::config::ShedConfig::parse(&out)
            .machine("mini3")
            .is_some());
    }

    /// The whole entry must read back, not merely the name — otherwise a value
    /// the reader mangles would configure a DIFFERENT machine than the one the
    /// user described.
    #[test]
    fn the_written_entry_reads_back_field_for_field() {
        let entry = NewMachine {
            name: "mini3",
            host: Some("100.64.0.3"),
            user: Some("charliek"),
            ssh_port: Some(2200),
        };
        let out = insert_machine("", &entry).unwrap();
        assert_eq!(
            *crate::config::ShedConfig::parse(&out)
                .machine("mini3")
                .unwrap(),
            expected_entry(&entry)
        );
    }

    #[test]
    fn a_file_without_a_trailing_newline_does_not_gain_one_spuriously() {
        let out = insert_machine("servers: {}", &m("mini3")).unwrap();
        assert!(!out.ends_with('\n'), "{out:?}");
        assert!(out.starts_with("servers: {}"));
    }
}
