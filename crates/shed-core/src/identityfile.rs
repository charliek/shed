//! identityfile — a server entry's `identity_file`, in Rust (plan 037 §3.3, milestone A8).
//!
//! A server entry in `~/.shed/config.yaml` may name the SSH key it uses
//! (`identity_file`, set by `shed server add --identity-file`). The Go CLI's rules are in
//! `internal/config/identityfile.go` and `sdk/bootstrap/bootstrap.go`. This module is what
//! the Rust readers (shed-core's config reader, the desktop app, and the credential
//! broker's discovery) share of them:
//!
//! * **The value rule** ([`check_value`], Go `ValidateIdentityFile`): an absolute path, no
//!   leading or trailing whitespace (Unicode White_Space), and none of NUL, CR, LF, `"`,
//!   `\`, `#`, `%` or `$`. One rule for every reader, so a value one side accepts the other
//!   never refuses. `crates/fixtures/identityfile/` (written by a Go test) holds both
//!   languages to it.
//! * **The file, at the time of use** ([`check_file`], Go `CheckIdentityFile`): opened
//!   without blocking, and the OPENED descriptor checked to be a regular file. It is
//!   checked each time the app or the broker BUILDS an ssh command line for the entry, and
//!   each time it OPENS (ensures) a tunnel or an exec runner for it, so a key removed after
//!   it was set fails early, with its own typed reason ([`IdentityFileError`]), not as an
//!   ssh that quietly tried other keys and "refused every key". It is not checked again
//!   for a connection an already-open tunnel makes later (roost's bridge opens those
//!   itself), nor for the local commands that only close a shared connection (`-O exit`
//!   and the like); and a terminal command is checked when it is built, not when the
//!   terminal later runs it. The key is a file the person controls: this is an early,
//!   clear error, not a guarantee about every ssh process.
//! * **The ssh options** ([`identity_args`]): `-i <path> -o IdentitiesOnly=yes -o
//!   ControlMaster=no -o ControlPath=none`, the Go CLI's spelling and order, and the four
//!   ssh_config lines ([`identity_config_lines`]) a generated stanza carries.
//!
//! Unlike the Go CLI, the Rust side does not learn the key's public half, so it does not
//! check that a certificate was made for the entry's key: what bounds who the app acts as
//! is the held-user rule (`crate::token::held_user_refusal`), not the key.

/// What a reader holds as `identity_file` when config.yaml's value is not a plain string it
/// can read exactly, in an entry it otherwise reads: a nested mapping (shed-core's reader,
/// whose whitelist refuses every other such form for the whole entry), or (the broker's
/// reader) a list, a map, or a value with a tag or an anchor or over more than one line.
/// Go refuses to load such a file at all, or reads another value than the Rust reader
/// would; a Rust reader keeps the entry and fails closed on it: every check of this value
/// refuses it ([`check_value`]), so it can never read as "no identity file". A NUL cannot
/// be typed in a path, so no real value is this one.
pub const IDENTITY_FILE_UNREADABLE: &str = "\u{0}<identity_file: not a plain string>";

/// What a reader holds as `identity_file` for an entry it does not read at all: in
/// shed-core's reader, one with a line that is not a plain `key: value` it reads exactly
/// (`crate::config`'s whitelist: an explicit key, a flow mapping, a tag, an anchor, a merge
/// key, a block scalar, a continuation line, a repeated key, a list…) or a null `host`; in
/// the broker's, one with a merge key (`<<`) or a tagged key. Such an entry could carry an
/// `identity_file` (or a `user`) the reader does not see, so it is refused whatever it
/// says, like [`IDENTITY_FILE_UNREADABLE`], and the refusal names the entry.
pub const IDENTITY_FILE_ENTRY_UNREADABLE: &str = "\u{0}<identity_file: entry not read exactly>";

/// Which part of the value rule a value breaks, in the order Go checks them.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum ValueRule {
    /// The value in config.yaml cannot be read exactly ([`IDENTITY_FILE_UNREADABLE`]).
    Unreadable,
    /// The entry cannot be read exactly ([`IDENTITY_FILE_ENTRY_UNREADABLE`]).
    EntryUnreadable,
    Empty,
    WhitespaceOnly,
    Nul,
    Cr,
    Lf,
    DoubleQuote,
    Backslash,
    Hash,
    Percent,
    Dollar,
    LeadingWhitespace,
    TrailingWhitespace,
    NotAbsolute,
}

impl ValueRule {
    /// The fixture's class name (`crates/fixtures/identityfile/README.md`).
    pub fn class(self) -> &'static str {
        match self {
            ValueRule::Unreadable => "unreadable",
            ValueRule::EntryUnreadable => "entry_unreadable",
            ValueRule::Empty => "empty",
            ValueRule::WhitespaceOnly => "whitespace_only",
            ValueRule::Nul => "nul",
            ValueRule::Cr => "cr",
            ValueRule::Lf => "lf",
            ValueRule::DoubleQuote => "double_quote",
            ValueRule::Backslash => "backslash",
            ValueRule::Hash => "hash",
            ValueRule::Percent => "percent",
            ValueRule::Dollar => "dollar",
            ValueRule::LeadingWhitespace => "leading_whitespace",
            ValueRule::TrailingWhitespace => "trailing_whitespace",
            ValueRule::NotAbsolute => "not_absolute",
        }
    }

    /// Why, in words that follow "cannot be used: " (Go's texts).
    pub fn reason(self) -> &'static str {
        match self {
            ValueRule::Unreadable => {
                "its value in config.yaml is not a plain string that can be read exactly (a \
                 list, a map, a quoted value that does not parse, a value with a tag or an \
                 anchor, or a value that runs on to the next line); write it as one plain \
                 or quoted string on its line"
            }
            ValueRule::EntryUnreadable => {
                "shed reads a server entry only when each line in it is a plain `key: value` \
                 it reads exactly, and this one is not (it has an explicit ? key, a {...} or \
                 [...] collection, a tag, an anchor or an alias, a merge key <<, a block \
                 scalar, a value over more than one line, a repeated key or a list, or its \
                 host is null); write its fields one per line, as key: value"
            }
            ValueRule::Empty => "it is empty",
            ValueRule::WhitespaceOnly => "it is only whitespace",
            ValueRule::Nul => "it contains a NUL byte",
            ValueRule::Cr => "it contains a carriage return",
            ValueRule::Lf => "it contains a line feed",
            ValueRule::DoubleQuote => {
                "it contains a double quote (\"), which would end the quoted IdentityFile value"
            }
            ValueRule::Backslash => "it contains a backslash (\\), which ssh unescapes",
            ValueRule::Hash => {
                "it contains #, which a config reader takes as the start of a comment"
            }
            ValueRule::Percent => "it contains %, which ssh expands as a token",
            ValueRule::Dollar => "it contains $, which ssh expands as an environment variable",
            ValueRule::LeadingWhitespace => "it begins with whitespace",
            ValueRule::TrailingWhitespace => "it ends with whitespace",
            ValueRule::NotAbsolute => "it is not an absolute path",
        }
    }
}

/// The characters a value may not hold, in Go's order (`identityFileRefused`).
const REFUSED: [(char, ValueRule); 8] = [
    ('\0', ValueRule::Nul),
    ('\r', ValueRule::Cr),
    ('\n', ValueRule::Lf),
    ('"', ValueRule::DoubleQuote),
    ('\\', ValueRule::Backslash),
    ('#', ValueRule::Hash),
    ('%', ValueRule::Percent),
    ('$', ValueRule::Dollar),
];

/// The value rule (Go `ValidateIdentityFile`), on the value alone: nothing is read.
pub fn check_value(path: &str) -> Result<(), ValueRule> {
    if path == IDENTITY_FILE_UNREADABLE {
        return Err(ValueRule::Unreadable);
    }
    if path == IDENTITY_FILE_ENTRY_UNREADABLE {
        return Err(ValueRule::EntryUnreadable);
    }
    if path.trim().is_empty() {
        return Err(if path.is_empty() {
            ValueRule::Empty
        } else {
            ValueRule::WhitespaceOnly
        });
    }
    if let Some((_, rule)) = REFUSED.iter().find(|(c, _)| path.contains(*c)) {
        return Err(*rule);
    }
    if path.starts_with(char::is_whitespace) {
        return Err(ValueRule::LeadingWhitespace);
    }
    if path.ends_with(char::is_whitespace) {
        return Err(ValueRule::TrailingWhitespace);
    }
    if !path.starts_with('/') {
        return Err(ValueRule::NotAbsolute);
    }
    Ok(())
}

/// An entry's `identity_file` that cannot be used: its value breaks the rule, or the file is
/// missing, not a regular file, or cannot be opened. Every Rust site returns it BEFORE any
/// ssh is spawned for the entry.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct IdentityFileError {
    /// The entry's config name ("" when there is none to name).
    pub entry: String,
    /// The value as config.yaml holds it.
    pub path: String,
    /// Why, in words that follow "cannot be used: ".
    pub reason: String,
    /// The value rule it breaks, when that is why.
    pub rule: Option<ValueRule>,
}

impl IdentityFileError {
    fn new(entry: &str, path: &str, reason: String) -> Self {
        Self {
            entry: entry.to_string(),
            path: path.to_string(),
            reason,
            rule: None,
        }
    }

    fn from_rule(entry: &str, path: &str, rule: ValueRule) -> Self {
        Self {
            rule: Some(rule),
            ..Self::new(entry, path, rule.reason().to_string())
        }
    }

    /// The same error, named for `entry` when it names none yet.
    pub fn named(mut self, entry: &str) -> Self {
        if self.entry.is_empty() {
            self.entry = entry.to_string();
        }
        self
    }
}

/// `entry "<name>": identity_file "<path>" cannot be used: <reason>` — Go's text. The name
/// and the path are shown in quotes with every `"`, `\` and non-printing character escaped
/// (as the other refusals of this lane show a config value), and the reason with every
/// non-printing character escaped ([`safe_text`]), so nothing from a config file or a file
/// system error can start a new line or a terminal sequence. A value the reader could not
/// read exactly is shown as `identity_file` alone; an entry the reader does not read at all
/// as `entry "<name>" cannot be used: <reason>`.
impl std::fmt::Display for IdentityFileError {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        let reason = safe_text(&self.reason);
        if self.path == IDENTITY_FILE_ENTRY_UNREADABLE {
            return if self.entry.is_empty() {
                write!(f, "the entry cannot be used: {reason}")
            } else {
                write!(f, "entry {:?} cannot be used: {reason}", self.entry)
            };
        }
        let value = if self.path == IDENTITY_FILE_UNREADABLE {
            "identity_file".to_string()
        } else {
            format!("identity_file {:?}", self.path)
        };
        if self.entry.is_empty() {
            write!(f, "{value} cannot be used: {reason}")
        } else {
            write!(
                f,
                "entry {:?}: {value} cannot be used: {reason}",
                self.entry
            )
        }
    }
}

impl std::error::Error for IdentityFileError {}

/// The [`IdentityFileError`] behind `err`, when that is why it failed: `err` itself, or
/// any error on its [`source`](std::error::Error::source) chain, or the error an
/// [`std::io::Error`] on that chain carries. The adapters that pass the refusal through
/// another error type (an exec runner's `io::Error`, a roost reach's `ReachError`, a
/// forward's `ForwardError`) keep it there, so a caller can tell "this entry's key file
/// cannot be used" from "the host is unreachable" without reading the text.
pub fn refusal_in<'a>(err: &'a (dyn std::error::Error + 'static)) -> Option<&'a IdentityFileError> {
    let mut next = Some(err);
    while let Some(e) = next {
        if let Some(refusal) = e.downcast_ref::<IdentityFileError>() {
            return Some(refusal);
        }
        if let Some(inner) = e
            .downcast_ref::<std::io::Error>()
            .and_then(std::io::Error::get_ref)
        {
            if let Some(refusal) = inner.downcast_ref::<IdentityFileError>() {
                return Some(refusal);
            }
        }
        next = e.source();
    }
    None
}

/// `s` with every character that is not printable written as an escape (Go `SafeText`: a
/// control character as `\n`, `\x1b` and the like, another non-printing one as `\uXXXX` or
/// `\UXXXXXXXX`). Printable text, spaces included, is kept as it is.
pub fn safe_text(s: &str) -> String {
    let mut out = String::with_capacity(s.len());
    for c in s.chars() {
        if c == ' ' || is_print(c) {
            out.push(c);
            continue;
        }
        match c {
            '\u{7}' => out.push_str("\\a"),
            '\u{8}' => out.push_str("\\b"),
            '\u{c}' => out.push_str("\\f"),
            '\n' => out.push_str("\\n"),
            '\r' => out.push_str("\\r"),
            '\t' => out.push_str("\\t"),
            '\u{b}' => out.push_str("\\v"),
            c if (c as u32) < 0x80 => out.push_str(&format!("\\x{:02x}", c as u32)),
            c if (c as u32) < 0x10000 => out.push_str(&format!("\\u{:04x}", c as u32)),
            c => out.push_str(&format!("\\U{:08x}", c as u32)),
        }
    }
    out
}

/// Printable as Go's `unicode.IsPrint` reads it, near enough: not a control or whitespace
/// character, and one Rust's own debug escaping keeps as it is.
fn is_print(c: char) -> bool {
    if matches!(c, '\'' | '"' | '\\') {
        return true;
    }
    !c.is_control() && !c.is_whitespace() && c.escape_debug().count() == 1
}

/// An io error's own words, as Go prints a syscall error: `permission denied`.
fn io_reason(e: &std::io::Error) -> String {
    let text = e.to_string();
    let text = match text.find(" (os error ") {
        Some(i) => text[..i].to_string(),
        None => text,
    };
    let mut chars = text.chars();
    match chars.next() {
        Some(first) => first.to_lowercase().chain(chars).collect(),
        None => text,
    }
}

/// The value rule plus the file itself, at the time of use (Go `CheckIdentityFile`): the
/// file is opened for reading without blocking (a FIFO or a device swapped in for the key
/// is opened at once, then refused), and the type is checked on the OPENED descriptor, so a
/// swap between a check and the open cannot get anything but a regular file past it. A
/// link to a regular file is followed; a public key file passes. The error names no entry
/// ([`IdentityFileError::named`]).
pub fn check_file(path: &str) -> Result<(), IdentityFileError> {
    use std::os::unix::fs::OpenOptionsExt as _;

    if let Err(rule) = check_value(path) {
        return Err(IdentityFileError::from_rule("", path, rule));
    }
    let refuse = |reason: String| IdentityFileError::new("", path, reason);
    let file = match std::fs::OpenOptions::new()
        .read(true)
        .custom_flags(libc::O_NONBLOCK)
        .open(path)
    {
        Ok(file) => file,
        Err(e) if e.kind() == std::io::ErrorKind::NotFound => {
            return Err(refuse("the file does not exist".to_string()))
        }
        Err(e) => {
            return Err(refuse(format!(
                "the file cannot be opened ({})",
                io_reason(&e)
            )))
        }
    };
    let meta = file
        .metadata()
        .map_err(|e| refuse(format!("the file cannot be read ({})", io_reason(&e))))?;
    if meta.is_dir() {
        return Err(refuse("it is a directory, not a key file".to_string()));
    }
    if !meta.is_file() {
        return Err(refuse("it is not a regular file".to_string()));
    }
    Ok(())
}

/// The value rule for the `identity_file` of the entry named `entry`: `Ok` for `""` (no
/// identity file) and for a value that passes; nothing is read.
pub fn check_entry_value(entry: &str, path: &str) -> Result<(), IdentityFileError> {
    if path.is_empty() {
        return Ok(());
    }
    check_value(path).map_err(|rule| IdentityFileError::from_rule(entry, path, rule))
}

/// [`check_file`] for the `identity_file` of the entry named `entry`, now: `Ok` for `""`
/// (no identity file: nothing is read).
pub fn check_entry_file(entry: &str, path: &str) -> Result<(), IdentityFileError> {
    if path.is_empty() {
        return Ok(());
    }
    check_file(path).map_err(|e| e.named(entry))
}

/// The ssh options an identity file adds, `-i <path> -o IdentitiesOnly=yes -o
/// ControlMaster=no -o ControlPath=none` (Go `IdentityArgs`), or nothing for `""`. It checks
/// nothing: callers check first ([`identity_file_args`]).
pub fn identity_args(path: &str) -> Vec<String> {
    if path.is_empty() {
        return Vec::new();
    }
    [
        "-i",
        path,
        "-o",
        "IdentitiesOnly=yes",
        "-o",
        "ControlMaster=no",
        "-o",
        "ControlPath=none",
    ]
    .map(String::from)
    .to_vec()
}

/// [`identity_args`] for the `identity_file` of the entry named `entry`, once the file
/// passes [`check_entry_file`] now; nothing for `""`. A site that builds an ssh argv for an
/// entry calls it right before (Go `IdentityFileArgs`).
pub fn identity_file_args(entry: &str, path: &str) -> Result<Vec<String>, IdentityFileError> {
    check_entry_file(entry, path)?;
    Ok(identity_args(path))
}

/// The ssh_config lines an identity file adds to a `Host` stanza (Go
/// `sshconfig.IdentityLines`), each after `indent` and ending in a newline: `IdentityFile
/// "<path>"`, `IdentitiesOnly yes`, `ControlMaster no` and `ControlPath none`. Empty for
/// `""`. The path is written as it is: the value rule refuses what a quoted ssh_config value
/// cannot carry.
pub fn identity_config_lines(path: &str, indent: &str) -> String {
    if path.is_empty() {
        return String::new();
    }
    format!(
        "{indent}IdentityFile \"{path}\"\n{indent}IdentitiesOnly yes\n{indent}ControlMaster no\n\
         {indent}ControlPath none\n"
    )
}

/// A stored control token on an entry with an identity file (Go
/// `RefusedStoredTokenForKey`): never presented. `refetch` is the entry's name as one word
/// of the suggested command, or `None` when no pasted word is safe (the advice is then
/// given in words). Go's text says "no request was sent"; the app's client still sends its
/// request with no credential (its rule for every failed credential), so this text says
/// what is true there: the token was not sent.
pub fn stored_token_message(entry: &str, path: &str, refetch: Option<&str>) -> String {
    let fix = match refetch {
        Some(word) => format!("Enrol it again: shed server update {word} --refetch"),
        None => "Enrol it again with shed server update and --refetch for this entry".to_string(),
    };
    format!(
        "entry {entry:?} cannot be used: it holds a control token, which does not say which SSH \
         key it was issued for, and the entry uses only its identity_file {path:?}; the token \
         was not sent. {fix}"
    )
}

#[cfg(test)]
mod tests {
    use super::*;

    /// A private scratch directory under the system temp dir, removed on drop (shed-core
    /// takes no `tempfile` dependency).
    struct Scratch(std::path::PathBuf);

    impl Scratch {
        fn new(tag: &str) -> Self {
            use std::sync::atomic::{AtomicU64, Ordering};
            static NEXT: AtomicU64 = AtomicU64::new(0);
            let dir = std::env::temp_dir().join(format!(
                "shed-core-identityfile-{tag}-{}-{}",
                std::process::id(),
                NEXT.fetch_add(1, Ordering::Relaxed)
            ));
            let _ = std::fs::remove_dir_all(&dir);
            std::fs::create_dir_all(&dir).unwrap();
            Self(dir)
        }

        fn write(&self, name: &str, contents: &[u8]) -> String {
            let path = self.0.join(name);
            std::fs::write(&path, contents).unwrap();
            path.to_str().unwrap().to_string()
        }
    }

    impl Drop for Scratch {
        fn drop(&mut self) {
            let _ = std::fs::remove_dir_all(&self.0);
        }
    }

    #[test]
    fn the_value_rule_is_go_s_in_go_s_order() {
        for (value, want) in [
            ("/home/pat/.ssh/id_work", None),
            ("/home/pat/.ssh/id work", None),
            ("/home/pat/.ssh/id\twork", None),
            ("/home/pat/.ssh/pat's key", None),
            ("", Some(ValueRule::Empty)),
            ("  ", Some(ValueRule::WhitespaceOnly)),
            ("/a\0b", Some(ValueRule::Nul)),
            ("/a\rb", Some(ValueRule::Cr)),
            ("/a\nb", Some(ValueRule::Lf)),
            // Two broken rules: the first in Go's order wins.
            ("/a\"b#c", Some(ValueRule::DoubleQuote)),
            ("/a\\b", Some(ValueRule::Backslash)),
            ("/a#b", Some(ValueRule::Hash)),
            ("/a%h", Some(ValueRule::Percent)),
            ("/${HOME}", Some(ValueRule::Dollar)),
            (" /a", Some(ValueRule::LeadingWhitespace)),
            ("\u{a0}/a", Some(ValueRule::LeadingWhitespace)),
            ("/a\u{2028}", Some(ValueRule::TrailingWhitespace)),
            ("~/.ssh/id", Some(ValueRule::NotAbsolute)),
            ("relative", Some(ValueRule::NotAbsolute)),
            (IDENTITY_FILE_UNREADABLE, Some(ValueRule::Unreadable)),
            (
                IDENTITY_FILE_ENTRY_UNREADABLE,
                Some(ValueRule::EntryUnreadable),
            ),
        ] {
            assert_eq!(check_value(value).err(), want, "{value:?}");
        }
    }

    #[test]
    fn the_args_and_the_config_lines_are_go_s() {
        assert_eq!(
            identity_args("/home/pat/.ssh/id_work"),
            [
                "-i",
                "/home/pat/.ssh/id_work",
                "-o",
                "IdentitiesOnly=yes",
                "-o",
                "ControlMaster=no",
                "-o",
                "ControlPath=none"
            ]
        );
        assert!(identity_args("").is_empty());
        assert_eq!(
            identity_config_lines("/k", "  "),
            "  IdentityFile \"/k\"\n  IdentitiesOnly yes\n  ControlMaster no\n  ControlPath none\n"
        );
        assert_eq!(identity_config_lines("", "  "), "");
    }

    /// The file check at the time of use: a regular file passes whatever it holds (a key
    /// file is not parsed), and the rest is refused with its own reason, named for the
    /// entry.
    #[test]
    fn a_file_that_cannot_be_used_is_refused_with_its_reason() {
        let dir = Scratch::new("refuse");
        let key = dir.write("id work", b"any bytes: the key is not read\n");
        assert_eq!(
            identity_file_args("pat", &key).unwrap(),
            identity_args(&key)
        );
        assert_eq!(identity_file_args("pat", "").unwrap(), Vec::<String>::new());

        let missing = format!("{}/missing", dir.0.display());
        let err = identity_file_args("pat", &missing).unwrap_err();
        assert_eq!(err.reason, "the file does not exist");
        assert_eq!(err.rule, None);
        assert_eq!(
            err.to_string(),
            format!(
                "entry \"pat\": identity_file {missing:?} cannot be used: the file does not exist"
            )
        );
        let as_dir = dir.0.to_str().unwrap().to_string();
        assert_eq!(
            check_entry_file("pat", &as_dir).unwrap_err().reason,
            "it is a directory, not a key file"
        );
        assert_eq!(
            check_entry_file("pat", "/a#b").unwrap_err().rule,
            Some(ValueRule::Hash)
        );
        // A dangling link is a missing file.
        let link = dir.0.join("link");
        std::os::unix::fs::symlink(&missing, &link).unwrap();
        assert_eq!(
            check_file(link.to_str().unwrap()).unwrap_err().reason,
            "the file does not exist"
        );
    }

    /// A FIFO swapped in for the key is opened without blocking and refused by its type.
    #[test]
    fn a_fifo_is_refused_without_blocking() {
        let dir = Scratch::new("fifo");
        let path = dir.0.join("fifo");
        let c = std::ffi::CString::new(path.to_str().unwrap()).unwrap();
        // SAFETY: a valid NUL-terminated path; mkfifo only creates the node.
        assert_eq!(unsafe { libc::mkfifo(c.as_ptr(), 0o600) }, 0);
        let err = check_file(path.to_str().unwrap()).unwrap_err();
        assert_eq!(err.reason, "it is not a regular file");
    }

    #[test]
    fn safe_text_escapes_what_cannot_be_shown() {
        assert_eq!(safe_text("a b"), "a b");
        assert_eq!(safe_text("a\nb\x1b[31m"), "a\\nb\\x1b[31m");
        assert_eq!(safe_text("\u{2028}\u{a0}é"), "\\u2028\\u00a0é");
        assert_eq!(safe_text("\u{e0001}"), "\\U000e0001");
    }

    /// A value, or an entry, the reader could not read exactly is a refusal that shows no
    /// path (the marker is no path) and says what to write instead.
    #[test]
    fn an_unreadable_value_reads_as_a_refusal_with_no_path() {
        let err = check_entry_file("pat", IDENTITY_FILE_UNREADABLE).unwrap_err();
        assert_eq!(err.rule, Some(ValueRule::Unreadable));
        assert!(
            err.to_string().starts_with(
                "entry \"pat\": identity_file cannot be used: its value in config.yaml"
            ),
            "{err}"
        );
        let err = check_entry_file("pat", IDENTITY_FILE_ENTRY_UNREADABLE).unwrap_err();
        assert_eq!(err.rule, Some(ValueRule::EntryUnreadable));
        assert!(
            err.to_string()
                .starts_with("entry \"pat\" cannot be used: shed reads a server entry only"),
            "{err}"
        );
        assert!(err
            .to_string()
            .ends_with("write its fields one per line, as key: value"));
        // The name is shown escaped: it cannot start a new line or a terminal sequence.
        let err = check_entry_file("p\n\x1b[31m", IDENTITY_FILE_ENTRY_UNREADABLE).unwrap_err();
        assert!(
            err.to_string()
                .starts_with("entry \"p\\n\\u{1b}[31m\" cannot be used:"),
            "{err}"
        );
    }

    /// The typed refusal stays findable through the errors that carry it: itself, an
    /// error whose source it is, and an `io::Error` built from it, however deep; an error
    /// that is not one is not mistaken for one.
    #[test]
    fn a_refusal_is_found_through_the_errors_that_carry_it() {
        #[derive(Debug)]
        struct Wrapper(Box<dyn std::error::Error + 'static>);
        impl std::fmt::Display for Wrapper {
            fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
                f.write_str("wrapped")
            }
        }
        impl std::error::Error for Wrapper {
            fn source(&self) -> Option<&(dyn std::error::Error + 'static)> {
                Some(self.0.as_ref())
            }
        }
        let refusal = check_entry_file("pat", "relative").unwrap_err();
        assert_eq!(refusal_in(&refusal), Some(&refusal));
        let io = std::io::Error::other(refusal.clone());
        assert_eq!(refusal_in(&io), Some(&refusal));
        let deep = Wrapper(Box::new(Wrapper(Box::new(std::io::Error::other(
            refusal.clone(),
        )))));
        assert_eq!(refusal_in(&deep), Some(&refusal));
        let unrelated = std::io::Error::other("connection refused");
        assert_eq!(refusal_in(&unrelated), None);
        assert_eq!(
            refusal_in(&Wrapper(Box::new(std::io::Error::from(
                std::io::ErrorKind::NotFound
            )))),
            None
        );
    }
}
