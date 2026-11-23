//! `envsecrets-login`: `~/.envsecrets/config.yaml`, copied byte for byte
//! (`0600`), and its passphrase, delivered as the env item the config's
//! `passphrase_env` names. The passphrase comes from this provider's own
//! `passphrase_file` or `passphrase_command`.
//!
//! There is no YAML parser here: the two flat keys it needs,
//! `passphrase_env` and `gcs_credentials`, are read with a strict line
//! pattern (the key at the start of a line, `:`, a plain or quoted scalar,
//! an optional ` # comment`), and anything else about the file is left to
//! envsecrets. A config with no `passphrase_env`, or no `gcs_credentials`
//! (which would leave envsecrets to the person's own Google sign-in, which
//! is never copied), is a problem and nothing is delivered.

use shed_provision::{
    check_value, check_variable, Base, Command, Context, Contribution, Draft, EnvItem, FileItem,
    ItemBody, Mode, Only, ProblemCode, Provider, Reader, Rule, Stage,
};

use crate::common::{one_line, read_setting_file, run};
use crate::settings::EnvsecretsLoginSettings;

/// The provider's name.
pub const NAME: &str = "envsecrets-login";

/// The envsecrets config, under home on both sides.
pub const CONFIG: &str = ".envsecrets/config.yaml";

/// The most the config may hold.
const MAX_CONFIG_BYTES: usize = 64 << 10;

/// The most a passphrase file may hold.
const MAX_PASSPHRASE_BYTES: usize = 16 << 10;

/// The `envsecrets-login` provider.
#[derive(Debug, Clone)]
pub struct EnvsecretsLogin {
    settings: EnvsecretsLoginSettings,
}

impl EnvsecretsLogin {
    /// The provider for these settings.
    pub fn new(settings: EnvsecretsLoginSettings) -> EnvsecretsLogin {
        EnvsecretsLogin { settings }
    }
}

/// A top-level key's value, by the strict pattern: `None` when the key is
/// absent, `Some(Err(()))` when it is there but not in the pattern, or named
/// twice.
fn flat_key<'a>(text: &'a str, key: &str) -> Option<Result<&'a str, ()>> {
    let mut found: Option<Result<&str, ()>> = None;
    for raw in text.split('\n') {
        let line = raw.strip_suffix('\r').unwrap_or(raw);
        let Some(rest) = line.strip_prefix(key) else {
            continue;
        };
        let Some(rest) = rest.strip_prefix(':') else {
            if rest.trim_start().starts_with(':') {
                // `key :` is the same key, in a form this reader refuses.
                found = Some(Err(()));
            }
            continue;
        };
        if found.is_some() {
            return Some(Err(()));
        }
        found = Some(scalar(rest).ok_or(()));
    }
    found
}

/// The scalar after a key's colon: at least one blank, then a word of
/// `[A-Za-z0-9_+/=]` (all a variable name or a base64 value can hold, and
/// no YAML indicator), plain or quoted in `'` or `"`, then blanks and an
/// optional `# comment` after at least one blank.
fn scalar(rest: &str) -> Option<&str> {
    let blank = |c: char| c == ' ' || c == '\t';
    if !rest.starts_with(blank) {
        return None;
    }
    let v = rest.trim_start_matches(blank);
    let (value, after) = match v.chars().next()? {
        q @ ('\'' | '"') => {
            let end = v[1..].find(q)? + 1;
            (&v[1..end], &v[end + 1..])
        }
        _ => {
            let end = v.find(blank).unwrap_or(v.len());
            (&v[..end], &v[end..])
        }
    };
    let tail = after.trim_start_matches(blank);
    let tail_ok = tail.is_empty() || (tail.starts_with('#') && tail.len() < after.len());
    let word = !value.is_empty()
        && value
            .bytes()
            .all(|c| c.is_ascii_alphanumeric() || matches!(c, b'_' | b'+' | b'/' | b'='));
    (tail_ok && word).then_some(value)
}

fn base64_text(s: &str) -> bool {
    !s.is_empty()
        && s.bytes()
            .all(|c| c.is_ascii_alphanumeric() || matches!(c, b'+' | b'/' | b'='))
}

fn value_message(rule: Rule) -> String {
    format!("the passphrase breaks the env rule {rule}; it was not delivered")
}

impl Provider for EnvsecretsLogin {
    fn name(&self) -> &str {
        NAME
    }

    fn only(&self) -> Option<&Only> {
        self.settings.only.as_ref()
    }

    fn provide(&self, _ctx: &Context<'_>, reader: &dyn Reader) -> Contribution {
        let config = match read_setting_file(
            reader,
            &format!("~/{CONFIG}"),
            MAX_CONFIG_BYTES,
            "~/.envsecrets/config.yaml",
        ) {
            Ok(f) => f,
            Err(c) => return c,
        };
        let Ok(text) = std::str::from_utf8(&config.bytes) else {
            return Contribution::problem(
                ProblemCode::Unreadable,
                "the envsecrets config is not UTF-8 text",
            );
        };
        let variable =
            match flat_key(text, "passphrase_env") {
                None => {
                    return Contribution::problem(
                        ProblemCode::MissingInput,
                        "the envsecrets config has no passphrase_env",
                    )
                }
                Some(Err(())) => return Contribution::problem(
                    ProblemCode::Unreadable,
                    "the envsecrets config's passphrase_env is not one plain value on its own line",
                ),
                Some(Ok(v)) => v,
            };
        if let Some(rule) = check_variable(variable) {
            return Contribution::problem(
                ProblemCode::Unreadable,
                format!("the envsecrets config's passphrase_env breaks the variable rule {rule}"),
            );
        }
        match flat_key(text, "gcs_credentials") {
            None => {
                return Contribution::problem(
                    ProblemCode::MissingInput,
                    "the envsecrets config has no gcs_credentials; your own Google sign-in is never copied",
                )
            }
            Some(Ok(v)) if base64_text(v) => {}
            Some(_) => {
                return Contribution::problem(
                    ProblemCode::Unreadable,
                    "the envsecrets config's gcs_credentials is not one base64 value on its own line",
                )
            }
        }

        let passphrase = match (
            &self.settings.passphrase_file,
            &self.settings.passphrase_command,
        ) {
            (Some(file), _) => {
                let f = match read_setting_file(
                    reader,
                    file,
                    MAX_PASSPHRASE_BYTES,
                    "envsecrets-login.passphrase_file",
                ) {
                    Ok(f) => f,
                    Err(c) => return c,
                };
                match one_line(&f.bytes) {
                    Some(p) => p.to_string(),
                    None => {
                        return Contribution::problem(
                            ProblemCode::Unreadable,
                            "envsecrets-login.passphrase_file is not UTF-8 text",
                        )
                    }
                }
            }
            (None, Some(argv)) => {
                let Some((program, args)) = argv.split_first() else {
                    return Contribution::problem(
                        ProblemCode::MissingInput,
                        "envsecrets-login.passphrase_command is empty",
                    );
                };
                let cmd = Command::new(program.clone(), args.iter().cloned());
                let out = match run(reader, &cmd, "envsecrets-login.passphrase_command") {
                    Ok(o) => o,
                    Err(c) => return c,
                };
                if !out.success() {
                    return Contribution::problem(
                        ProblemCode::CommandFailed,
                        "envsecrets-login.passphrase_command failed",
                    );
                }
                match one_line(&out.stdout) {
                    Some(p) => p.to_string(),
                    None => {
                        return Contribution::problem(
                            ProblemCode::CommandFailed,
                            "envsecrets-login.passphrase_command printed something that is not UTF-8 text",
                        )
                    }
                }
            }
            (None, None) => {
                return Contribution::problem(
                    ProblemCode::MissingInput,
                    "set envsecrets-login.passphrase_file or passphrase_command",
                )
            }
        };
        if let Some(rule) = check_value(&passphrase) {
            return Contribution::problem(ProblemCode::Unreadable, value_message(rule));
        }
        let variable = variable.to_string();
        Contribution {
            items: vec![
                Draft {
                    name: format!("{NAME}.config"),
                    body: ItemBody::File(FileItem {
                        stage: Stage::One,
                        base: Base::Home,
                        path: CONFIG.into(),
                        mode: Mode::M0600,
                        content: config.bytes,
                    }),
                    expires_at: None,
                },
                Draft {
                    name: format!("{NAME}.passphrase"),
                    body: ItemBody::Env(EnvItem {
                        variable,
                        value: passphrase,
                    }),
                    expires_at: None,
                },
            ],
            ..Contribution::default()
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn the_strict_line_pattern() {
        let t = "bucket: b\npassphrase_env: ENVSECRETS_PASSPHRASE  # or use passphrase_command\ngcs_credentials: eyJ0eXA=\n";
        assert_eq!(
            flat_key(t, "passphrase_env"),
            Some(Ok("ENVSECRETS_PASSPHRASE"))
        );
        assert_eq!(flat_key(t, "gcs_credentials"), Some(Ok("eyJ0eXA=")));
        assert_eq!(
            flat_key("passphrase_env: \"X_Y\"\n", "passphrase_env"),
            Some(Ok("X_Y"))
        );
        assert_eq!(
            flat_key("passphrase_env: 'X'\r\n", "passphrase_env"),
            Some(Ok("X"))
        );
        assert_eq!(flat_key("other: 1\n", "passphrase_env"), None);
        assert_eq!(flat_key("  passphrase_env: X\n", "passphrase_env"), None);
        assert_eq!(flat_key("passphrase_envx: X\n", "passphrase_env"), None);
        for bad in [
            "passphrase_env:\n",
            "passphrase_env: |\n  X\n",
            "passphrase_env:X\n",
            "passphrase_env: X Y\n",
            "passphrase_env: \"X\"#c\n",
            "passphrase_env: \"X\\\"\"\n",
            "passphrase_env: X\npassphrase_env: Y\n",
            "passphrase_env : X\n",
        ] {
            assert!(
                matches!(flat_key(bad, "passphrase_env"), Some(Err(()))),
                "{bad:?}"
            );
        }
    }
}
