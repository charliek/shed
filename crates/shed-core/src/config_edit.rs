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
    /// The backstop for the fact that the reader is PARTIAL and has quirks the
    /// writer must not try to mirror: it strips everything after a `#` whether
    /// or not the value is quoted, and it never unescapes. Rather than model
    /// each one and get it subtly wrong, the writer proposes and the reader
    /// disposes — whatever the reader makes of the new text IS the truth, and
    /// if that is not what was asked for, nothing is written.
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
    pub rc_bin: Option<&'a str>,
}

/// Return `text` with `machine` inserted, or an [`EditError`].
///
/// Pure: the caller does the reading and writing, so this is testable against
/// real config text without touching a disk.
pub fn insert_machine(text: &str, machine: &NewMachine<'_>) -> Result<String, EditError> {
    if !is_plain_key(machine.name) {
        return Err(EditError::BadName(machine.name.to_string()));
    }
    let before = crate::config::ShedConfig::parse(text);
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
        host: m.host.filter(|h| !h.is_empty()).unwrap_or(m.name).to_string(),
        user: m.user.filter(|u| !u.is_empty()).map(str::to_string),
        ssh_port: m.ssh_port.unwrap_or(22),
        rc_bin: m.rc_bin.filter(|b| !b.is_empty()).map(str::to_string),
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
    out.push(field("host", m.host.filter(|h| !h.is_empty()).unwrap_or(m.name)));
    if let Some(u) = m.user.filter(|u| !u.is_empty()) {
        out.push(field("user", u));
    }
    if let Some(p) = m.ssh_port.filter(|p| *p != 22) {
        out.push(format!("{inner}ssh_port: {p}"));
    }
    if let Some(b) = m.rc_bin.filter(|b| !b.is_empty()) {
        out.push(field("rc_bin", b));
    }
    out
}

/// Quote a value only when leaving it bare would change what it means. The
/// hand-rolled reader takes the text after `: ` verbatim, so this is about the
/// FILE staying readable by other tools, not about the reader.
fn scalar(v: &str) -> String {
    let plain = !v.is_empty()
        && !v.starts_with(' ')
        && !v.ends_with(' ')
        && !v.contains(['#', ':', '\'', '"', '\n'])
        && v.parse::<f64>().is_err()
        && !matches!(v, "true" | "false" | "null" | "yes" | "no");
    if plain {
        v.to_string()
    } else {
        format!("\"{}\"", v.replace('\\', "\\\\").replace('"', "\\\""))
    }
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
        NewMachine { name, host: None, user: None, ssh_port: None, rc_bin: None }
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
        assert_eq!(before.next(), None, "an original line was dropped or reordered");
        // …and the insert actually happened, so "survives" is not satisfied by
        // returning the input untouched.
        assert!(crate::config::ShedConfig::parse(&out).machine("mini4").is_some());
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
        assert!(out.starts_with(orig), "the original must be a prefix: {out}");
        assert!(out.contains("\nmachines:\n"));
        assert!(crate::config::ShedConfig::parse(&out).machine("mini3").is_some());
    }

    #[test]
    fn writes_every_supplied_field_and_omits_the_defaults() {
        let full = NewMachine {
            name: "mini3",
            host: Some("100.64.0.3"),
            user: Some("charliek"),
            ssh_port: Some(2200),
            rc_bin: Some("/home/charliek/.local/bin/sx"),
        };
        let out = insert_machine("", &full).unwrap();
        let cfg = crate::config::ShedConfig::parse(&out);
        let e = cfg.machine("mini3").expect("parses back");
        assert_eq!(e.host, "100.64.0.3");
        assert_eq!(e.user.as_deref(), Some("charliek"));
        assert_eq!(e.ssh_port, 2200);
        assert_eq!(e.rc_bin.as_deref(), Some("/home/charliek/.local/bin/sx"));

        // The default port is left out rather than written — the file should not
        // fill with values that only restate the default.
        let bare = insert_machine("", &m("mini4")).unwrap();
        assert!(!bare.contains("ssh_port"), "{bare}");
        assert_eq!(crate::config::ShedConfig::parse(&bare).machine("mini4").unwrap().ssh_port, 22);
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
        assert!(crate::config::ShedConfig::parse(&out).machine("mini3").is_some());
        assert!(crate::config::ShedConfig::parse(&out).machine("not-ours").is_none());
    }

    #[test]
    fn a_value_needing_quotes_gets_them() {
        let odd = NewMachine {
            name: "mini3",
            host: Some("host: with colon"),
            user: None,
            ssh_port: None,
            rc_bin: None,
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
                crate::config::ShedConfig::parse(weird).machine("mini3").is_some(),
                "precondition: reader sees mini3 in {weird:?}"
            );
            let out = insert_machine(weird, &m("mini4"));
            assert!(
                matches!(out, Err(EditError::WouldNotRoundTrip(_))),
                "accepted a config it could not safely extend: {weird:?} -> {out:?}"
            );
        }
    }

    /// codex review: the reader strips everything after a `#` whether or not
    /// the value is quoted, so `host: "mini#prod"` reads back as `"mini`. The
    /// writer cannot represent it, and must say so rather than configure a
    /// machine pointing somewhere else.
    #[test]
    fn refuses_a_value_the_reader_would_mangle() {
        // A trailing space is NOT in this list: quoting preserves it and the
        // reader's unquote gives it back, so it round-trips and is accepted.
        // The verifier decides that, not a guess about which values look risky.
        for bad_host in ["mini#prod", "has\"quote"] {
            let entry = NewMachine {
                name: "mini3",
                host: Some(bad_host),
                user: None,
                ssh_port: None,
                rc_bin: None,
            };
            let out = insert_machine("", &entry);
            assert!(
                matches!(out, Err(EditError::WouldNotRoundTrip(_))),
                "accepted host {bad_host:?} -> {out:?}"
            );
        }
    }

    /// codex review: `lines()` drops `\r`, and joining with `\n` would rewrite
    /// every terminator in the file — byte-for-byte has to include how each
    /// line ended.
    #[test]
    fn a_crlf_file_stays_crlf() {
        let orig = "servers:\r\n    mac-mini:\r\n        host: localhost\r\n";
        let out = insert_machine(orig, &m("mini3")).unwrap();
        assert!(out.starts_with(orig), "original bytes not preserved: {out:?}");
        assert!(!out.contains("\n\n"), "a bare LF crept in: {out:?}");
        assert_eq!(
            out.matches("\r\n").count(),
            out.matches('\n').count(),
            "every newline should still be a CRLF: {out:?}"
        );
        assert!(crate::config::ShedConfig::parse(&out).machine("mini3").is_some());
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
            rc_bin: Some("/home/charliek/.local/bin/sx"),
        };
        let out = insert_machine("", &entry).unwrap();
        assert_eq!(
            *crate::config::ShedConfig::parse(&out).machine("mini3").unwrap(),
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
