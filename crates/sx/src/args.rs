//! The flag parser — a hand-rolled reimplementation of the subset of Go's `flag`
//! package the RC CLI contract enumerates (plan 009 §3.2).
//!
//! `shed-machine-rc` parses its subcommand flags with `flag.FlagSet`, and real
//! consumers (shed-core's `create_argv`, the skills, humans) emit a mix of forms.
//! The contract this parser implements — each item covered by a unit case below
//! or a harness cell — is:
//!
//! | form | example |
//! |---|---|
//! | single dash | `-kind shell` |
//! | double dash | `--kind shell` |
//! | inline value | `--kind=shell` |
//! | bare boolean | `--wait` |
//! | explicit boolean | `--wait=false` |
//! | flags strictly before positionals | `create --kind shell x` → `x` is a stray |
//! | `--` terminator | ends flag parsing (the rest are positionals) |
//! | unknown flag | usage error, exit 2 |
//! | empty value | `--target ''` / `--target=` is a VALID value |
//! | repeated flag | last one wins |
//! | set-empty vs absent | `--kind=` is SET to `""` (engine rejects); only an ABSENT `--kind` takes Go's `claude-rc` default |
//!
//! Go-`flag` corner cases outside that table (`-flag=` shorthand clusters,
//! interspersed flags after a positional, the `flag.Value` interface) are
//! explicitly NON-contract: the port matches consumers, not the package.
//!
//! The parser is PURE (`&[String]` in, a `Parsed` map out) so the whole grammar is
//! unit-testable without a process.

use std::collections::{BTreeMap, BTreeSet};

/// Whether a flag takes a value or is a bare boolean.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum FlagKind {
    /// `--wait` / `--wait=true` — never consumes the following argument (Go's
    /// bool flags behave exactly this way, which is why `--wait true` leaves
    /// `true` as a stray positional rather than setting the flag).
    Bool,
    /// `--kind shell` / `--kind=shell`.
    Value,
}

/// One declared flag. Names are stored WITHOUT dashes; both `-name` and `--name`
/// resolve to the same spec, as in Go.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct Spec {
    pub name: &'static str,
    pub kind: FlagKind,
}

/// Declare a value-taking flag.
pub const fn value(name: &'static str) -> Spec {
    Spec {
        name,
        kind: FlagKind::Value,
    }
}

/// Declare a bare boolean flag.
pub const fn boolean(name: &'static str) -> Spec {
    Spec {
        name,
        kind: FlagKind::Bool,
    }
}

/// A parse failure. Rendered by the caller as a usage error (exit 2); the text
/// mirrors Go's `flag` messages so a person reading two transcripts side by side
/// sees the same words (the harness diffs the exit CODE for these — the flag
/// package also dumps a per-flag usage block that this port does not reproduce).
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum ArgError {
    /// `flag provided but not defined: -bogus`
    Unknown(String),
    /// `flag needs an argument: -kind`
    NeedsValue(String),
    /// `invalid boolean value "x" for -wait`
    BadBool { flag: String, value: String },
    /// A token like `---kind` or `-=x` that is not a legal flag syntax at all.
    BadSyntax(String),
}

impl std::fmt::Display for ArgError {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        match self {
            ArgError::Unknown(flag) => write!(f, "flag provided but not defined: {flag}"),
            ArgError::NeedsValue(flag) => write!(f, "flag needs an argument: {flag}"),
            ArgError::BadBool { flag, value } => {
                write!(f, "invalid boolean value {value:?} for {flag}")
            }
            ArgError::BadSyntax(token) => write!(f, "bad flag syntax: {token}"),
        }
    }
}

/// The result of a successful parse: flag values, set booleans, and whatever
/// followed the flags.
#[derive(Debug, Default, Clone, PartialEq, Eq)]
pub struct Parsed {
    values: BTreeMap<String, String>,
    bools: BTreeSet<String>,
    positionals: Vec<String>,
}

impl Parsed {
    /// A value flag's value, or `""` when it was not given — the Go-`flag`
    /// zero-value convention the engine's "empty means absent" options expect.
    ///
    /// Fine for every flag whose Go DEFAULT is also `""` (Go conflates
    /// absent-with-empty for those too). A flag with a NON-empty Go default —
    /// today only `--kind`, default `claude-rc` — must use [`Parsed::value_opt`]
    /// instead: `--kind=` is *set to empty* and must reach the engine as `""`
    /// (rejected, exit 2), not fall back to the default.
    pub fn value(&self, name: &str) -> &str {
        self.values.get(name).map_or("", String::as_str)
    }

    /// A value flag's value, distinguishing ABSENT (`None`) from
    /// set-but-empty (`Some("")`) — Go's `flag` keeps its default only when the
    /// flag was never given.
    pub fn value_opt(&self, name: &str) -> Option<&str> {
        self.values.get(name).map(String::as_str)
    }

    /// Whether a boolean flag was set (`--flag=false` counts as NOT set).
    pub fn flag(&self, name: &str) -> bool {
        self.bools.contains(name)
    }

    /// Everything after the flags. A non-empty slice is a caller mistake for
    /// every subcommand in the RC surface (none takes a positional).
    pub fn positionals(&self) -> &[String] {
        &self.positionals
    }
}

/// Parse `args` against `specs`.
///
/// Mirrors Go's `parseOne` loop: a token that is not `-x`-shaped ENDS flag
/// parsing (it and everything after it are positionals), so flags must precede
/// positionals.
pub fn parse(specs: &[Spec], args: &[String]) -> Result<Parsed, ArgError> {
    let mut out = Parsed::default();
    let mut i = 0;
    while i < args.len() {
        let token = args[i].as_str();
        // "" , "-" and any non-dash token are positionals and stop the parse
        // (Go: `if len(s) < 2 || s[0] != '-' { return false, nil }`).
        if token.len() < 2 || !token.starts_with('-') {
            break;
        }
        let mut name = &token[1..];
        if let Some(stripped) = name.strip_prefix('-') {
            if stripped.is_empty() {
                // A bare "--" terminates flag parsing and is consumed.
                i += 1;
                break;
            }
            name = stripped;
        }
        if name.starts_with('-') || name.starts_with('=') {
            return Err(ArgError::BadSyntax(token.to_string()));
        }
        let (name, inline) = match name.split_once('=') {
            Some((n, v)) => (n, Some(v.to_string())),
            None => (name, None),
        };
        let dashed = format!("-{name}");
        let Some(spec) = specs.iter().find(|s| s.name == name) else {
            return Err(ArgError::Unknown(dashed));
        };
        i += 1;
        match spec.kind {
            FlagKind::Bool => {
                let set = match inline {
                    None => true,
                    Some(raw) => parse_bool(&raw).ok_or(ArgError::BadBool {
                        flag: dashed,
                        value: raw,
                    })?,
                };
                if set {
                    out.bools.insert(name.to_string());
                } else {
                    out.bools.remove(name);
                }
            }
            FlagKind::Value => {
                let v = match inline {
                    Some(v) => v,
                    None => {
                        let Some(next) = args.get(i) else {
                            return Err(ArgError::NeedsValue(dashed));
                        };
                        i += 1;
                        next.clone()
                    }
                };
                out.values.insert(name.to_string(), v);
            }
        }
    }
    out.positionals = args[i..].to_vec();
    Ok(out)
}

/// Go's `strconv.ParseBool` accept-set, for `--flag=<v>`.
fn parse_bool(raw: &str) -> Option<bool> {
    match raw {
        "1" | "t" | "T" | "TRUE" | "true" | "True" => Some(true),
        "0" | "f" | "F" | "FALSE" | "false" | "False" => Some(false),
        _ => None,
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    const SPECS: &[Spec] = &[
        value("kind"),
        value("slug"),
        value("target"),
        boolean("wait"),
        boolean("skip"),
    ];

    fn argv(items: &[&str]) -> Vec<String> {
        items.iter().map(|s| (*s).to_string()).collect()
    }

    fn ok(items: &[&str]) -> Parsed {
        parse(SPECS, &argv(items)).expect("parse")
    }

    #[test]
    fn both_dash_forms_and_inline_values_agree() {
        for form in [
            &["--kind", "shell"][..],
            &["-kind", "shell"][..],
            &["--kind=shell"][..],
            &["-kind=shell"][..],
        ] {
            let p = ok(form);
            assert_eq!(p.value("kind"), "shell", "form {form:?}");
            assert!(p.positionals().is_empty(), "form {form:?}");
        }
    }

    #[test]
    fn bare_bools_set_and_explicit_false_clears() {
        assert!(ok(&["--wait"]).flag("wait"));
        assert!(ok(&["-wait"]).flag("wait"));
        assert!(ok(&["--wait=true"]).flag("wait"));
        assert!(!ok(&["--wait=false"]).flag("wait"));
        assert!(!ok(&["--kind", "shell"]).flag("wait"));
    }

    #[test]
    fn bool_does_not_consume_the_next_argument() {
        // Go's bool flags never take a following value — `true` stays a stray
        // positional, which is what makes `--wait true` a usage error.
        let p = ok(&["--wait", "true"]);
        assert!(p.flag("wait"));
        assert_eq!(p.positionals(), ["true"]);
    }

    #[test]
    fn empty_target_is_a_value_not_an_absence_error() {
        // shed-core's create_argv always passes --target, even empty — in both
        // the separate-arg and inline (=) spellings.
        let p = ok(&["--target", "", "--kind", "shell"]);
        assert_eq!(p.value("target"), "");
        assert_eq!(p.value("kind"), "shell");
        assert!(p.positionals().is_empty());
        let p = ok(&["--target=", "--kind", "shell"]);
        assert_eq!(p.value("target"), "");
        assert_eq!(p.value_opt("target"), Some(""));
    }

    #[test]
    fn repeated_flag_last_wins() {
        // Go's flag.Set overwrites on every occurrence.
        let p = ok(&["--kind", "claude-rc", "--kind", "shell"]);
        assert_eq!(p.value("kind"), "shell");
    }

    #[test]
    fn value_opt_distinguishes_absent_from_set_empty() {
        // The axis Go's non-empty defaults hinge on: absent keeps the default,
        // `--kind=` overrides it with "" (which the engine then rejects).
        let p = ok(&["--slug", "aa1111"]);
        assert_eq!(p.value_opt("kind"), None);
        let p = ok(&["--kind=", "--slug", "aa1111"]);
        assert_eq!(p.value_opt("kind"), Some(""));
    }

    #[test]
    fn flags_stop_at_the_first_positional() {
        let p = ok(&["--kind", "shell", "stray", "--wait"]);
        assert_eq!(p.value("kind"), "shell");
        // Everything from the stray on is positional — `--wait` after it is NOT a flag.
        assert!(!p.flag("wait"));
        assert_eq!(p.positionals(), ["stray", "--wait"]);
    }

    #[test]
    fn double_dash_terminates_flag_parsing() {
        let p = ok(&["--kind", "shell", "--", "--wait"]);
        assert_eq!(p.value("kind"), "shell");
        assert!(!p.flag("wait"));
        assert_eq!(p.positionals(), ["--wait"]);
    }

    #[test]
    fn unknown_flag_is_an_error() {
        assert_eq!(
            parse(SPECS, &argv(&["--bogus"])).unwrap_err(),
            ArgError::Unknown("-bogus".to_string())
        );
        assert_eq!(
            parse(SPECS, &argv(&["-bogus=1"])).unwrap_err(),
            ArgError::Unknown("-bogus".to_string())
        );
    }

    #[test]
    fn value_flag_without_a_value_is_an_error() {
        assert_eq!(
            parse(SPECS, &argv(&["--kind"])).unwrap_err(),
            ArgError::NeedsValue("-kind".to_string())
        );
    }

    #[test]
    fn bad_boolean_value_is_an_error() {
        assert_eq!(
            parse(SPECS, &argv(&["--wait=maybe"])).unwrap_err(),
            ArgError::BadBool {
                flag: "-wait".to_string(),
                value: "maybe".to_string()
            }
        );
    }

    #[test]
    fn lone_dash_and_bad_syntax() {
        // "-" is a positional (Go treats it as a non-flag), "---x" is bad syntax.
        assert_eq!(ok(&["-"]).positionals(), ["-"]);
        assert_eq!(
            parse(SPECS, &argv(&["---kind"])).unwrap_err(),
            ArgError::BadSyntax("---kind".to_string())
        );
    }

    #[test]
    fn absent_flags_read_as_empty_and_false() {
        let p = ok(&[]);
        assert_eq!(p.value("kind"), "");
        assert!(!p.flag("wait"));
        assert!(p.positionals().is_empty());
    }
}
