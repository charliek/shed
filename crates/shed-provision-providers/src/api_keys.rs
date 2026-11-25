//! `api-keys`: craze's `~/.craze/native/providers.toml` (copied, `0600`;
//! never the ChatGPT plan's token file, which this provider does not read)
//! and env files of `KEY=VALUE` lines, one env item per variable. An env
//! file must be readable by its owner only. A line that breaks the env
//! rules is a problem, never dropped in silence.
//!
//! An env file's format: one `KEY=VALUE` per line. The value is everything
//! after the first `=`, byte for byte (no quoting, no expansion, no
//! trimming); one trailing `\r` is a line end. Blank lines, and lines whose
//! first non-blank character is `#`, are skipped.

use std::fmt;

use shed_provision::{
    check_value, check_variable, limits::MAX_ENV_FILE_BYTES, limits::MAX_FILE_BYTES, valid_name,
    Base, Context, Contribution, Mode, ProblemCode, ProblemDraft, Reader, Rule,
};

use crate::common::{desktop_provider, env_draft, file_draft, read_setting_file, Redacted};
use crate::settings::ApiKeysSettings;
use crate::Gathered;

/// The provider's name.
pub const NAME: &str = "api-keys";

/// craze's API key file, under home on both sides.
pub const CRAZE_PROVIDERS: &str = ".craze/native/providers.toml";

/// The craze file's item name.
pub const CRAZE_ITEM: &str = "api-keys.craze";

/// The most line problems one env file reports before it sums up the rest.
const LINE_PROBLEMS: usize = 3;

/// One environment variable to deliver.
#[derive(Clone, PartialEq, Eq)]
pub struct EnvInput {
    /// The item's name.
    pub name: String,
    /// The variable.
    pub variable: String,
    /// Its value.
    pub value: String,
}

impl fmt::Debug for EnvInput {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.debug_struct("EnvInput")
            .field("name", &self.name)
            .field("variable", &self.variable)
            .field("value", &Redacted(self.value.len()))
            .finish()
    }
}

/// What the provider delivers from: craze's `providers.toml`, if any, then
/// the variables, in order.
#[derive(Clone, Default, PartialEq, Eq)]
pub struct Inputs {
    /// craze's `providers.toml`, delivered as [`CRAZE_ITEM`].
    pub craze_providers: Option<Vec<u8>>,
    /// The variables.
    pub env: Vec<EnvInput>,
}

impl fmt::Debug for Inputs {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.debug_struct("Inputs")
            .field(
                "craze_providers",
                &self.craze_providers.as_ref().map(|b| Redacted(b.len())),
            )
            .field("env", &self.env)
            .finish()
    }
}

desktop_provider!(
    /// The `api-keys` provider.
    ApiKeys,
    ApiKeysSettings
);

fn line_problem(file: usize, line: usize, rule: Rule) -> String {
    let what = match rule {
        Rule::BadVariable => "the name is not upper-case letters, digits and underscores",
        Rule::RefusedVariable => "the variable is one a shed may not have set",
        Rule::ValueEmpty => "the value is empty",
        Rule::ValueTooLong => "the value is over 8192 bytes",
        Rule::ValueNotPrintable => "the value holds a character that is not printable",
        Rule::ValueEdgeSpace => "the value starts or ends with a space",
        _ => "the line is not KEY=VALUE",
    };
    format!("api-keys.env_files[{file}] line {line}: {what}; it was not delivered")
}

/// The variables of one env file's text, and its problems.
fn parse_env_file(
    file: usize,
    text: &str,
    env: &mut Vec<EnvInput>,
    problems: &mut Vec<ProblemDraft>,
) {
    let mut bad = 0usize;
    for (n, raw) in text.split('\n').enumerate() {
        let line = raw.strip_suffix('\r').unwrap_or(raw);
        let t = line.trim_start();
        if t.is_empty() || t.starts_with('#') {
            continue;
        }
        let parsed = match line.split_once('=') {
            None => Err(Rule::BadValue),
            Some((key, value)) => check_variable(key)
                .or_else(|| check_value(value))
                .map_or(Ok((key, value)), Err),
        };
        match parsed {
            Ok((key, value)) => {
                let lower = format!("{NAME}.{}", key.to_ascii_lowercase());
                let name = if valid_name(&lower) {
                    lower
                } else {
                    format!("{NAME}.f{file}l{}", n + 1)
                };
                env.push(EnvInput {
                    name,
                    variable: key.to_string(),
                    value: value.to_string(),
                });
            }
            Err(rule) => {
                bad += 1;
                if bad <= LINE_PROBLEMS {
                    problems.push(ProblemDraft::new(
                        ProblemCode::Unreadable,
                        line_problem(file, n + 1, rule),
                    ));
                }
            }
        }
    }
    if bad > LINE_PROBLEMS {
        problems.push(ProblemDraft::new(
            ProblemCode::Unreadable,
            format!(
                "api-keys.env_files[{file}]: {} more lines break the env rules and were not delivered",
                bad - LINE_PROBLEMS
            ),
        ));
    }
}

/// Reads craze's file (when `craze_providers` is on) and each env file, in
/// order. A file that cannot be read, is readable by others or is not text
/// is a problem, and the others are kept; a line that breaks the env rules
/// is a problem and the file's other lines are kept. A variable is named
/// `api-keys.<variable lower-cased>`, or by its file index and line number
/// when that is not a valid name.
pub fn gather(
    settings: &ApiKeysSettings,
    ctx: &Context<'_>,
    reader: &dyn Reader,
) -> Gathered<Inputs> {
    let mut inputs = Inputs::default();
    let mut problems = Vec::new();
    if settings.craze_providers {
        match read_setting_file(
            ctx,
            reader,
            &format!("~/{CRAZE_PROVIDERS}"),
            MAX_FILE_BYTES,
            "~/.craze/native/providers.toml",
        ) {
            Ok(f) => inputs.craze_providers = Some(f.bytes),
            Err(e) => problems.extend(e.problems),
        }
    }
    for (i, path) in settings.env_files.iter().enumerate() {
        let what = format!("api-keys.env_files[{i}]");
        let f = match read_setting_file(ctx, reader, path, MAX_ENV_FILE_BYTES, &what) {
            Ok(f) => f,
            Err(e) => {
                problems.extend(e.problems);
                continue;
            }
        };
        if !f.owner_only() {
            problems.push(ProblemDraft::new(
                ProblemCode::Unreadable,
                format!("{what} can be read by others; set its mode to 0600"),
            ));
            continue;
        }
        let Ok(text) = std::str::from_utf8(&f.bytes) else {
            problems.push(ProblemDraft::new(
                ProblemCode::Unreadable,
                format!("{what} is not UTF-8 text"),
            ));
            continue;
        };
        parse_env_file(i, text, &mut inputs.env, &mut problems);
    }
    Gathered {
        inputs: Some(inputs),
        problems,
    }
}

/// The items `inputs` make: craze's file first, then one env item per
/// variable. Each variable and value is held to the env rules where the
/// answer is assembled, as every env item is.
pub fn build(inputs: &Inputs, _ctx: &Context<'_>) -> Contribution {
    let craze = inputs
        .craze_providers
        .as_deref()
        .map(|b| file_draft(CRAZE_ITEM, Base::Home, CRAZE_PROVIDERS, Mode::M0600, b));
    let env = inputs
        .env
        .iter()
        .map(|e| env_draft(&e.name, &e.variable, &e.value));
    Contribution {
        items: craze.into_iter().chain(env).collect(),
        ..Contribution::default()
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn debug_carries_no_value() {
        let i = Inputs {
            craze_providers: Some(b"SENTINEL-craze".to_vec()),
            env: vec![EnvInput {
                name: "api-keys.k".into(),
                variable: "K".into(),
                value: "SENTINEL-value".into(),
            }],
        };
        let shown = format!("{i:?}");
        assert!(!shown.contains("SENTINEL"), "{shown}");
        assert!(
            shown.contains("K") && shown.contains("<14 bytes>"),
            "{shown}"
        );
    }
}
