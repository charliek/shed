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

use shed_provision::{
    check_value, check_variable, limits::MAX_ENV_FILE_BYTES, limits::MAX_FILE_BYTES, valid_name,
    Base, Context, Contribution, Draft, EnvItem, FileItem, ItemBody, Mode, Only, ProblemCode,
    ProblemDraft, Provider, Reader, Rule, Stage,
};

use crate::common::read_setting_file;
use crate::settings::ApiKeysSettings;

/// The provider's name.
pub const NAME: &str = "api-keys";

/// craze's API key file, under home on both sides.
pub const CRAZE_PROVIDERS: &str = ".craze/native/providers.toml";

/// The most line problems one env file reports before it sums up the rest.
const LINE_PROBLEMS: usize = 3;

/// The `api-keys` provider.
#[derive(Debug, Clone)]
pub struct ApiKeys {
    settings: ApiKeysSettings,
}

impl ApiKeys {
    /// The provider for these settings.
    pub fn new(settings: ApiKeysSettings) -> ApiKeys {
        ApiKeys { settings }
    }
}

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

/// The env items of one env file's text, and its problems.
fn parse_env_file(file: usize, text: &str, c: &mut Contribution) {
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
                c.items.push(Draft {
                    name,
                    body: ItemBody::Env(EnvItem {
                        variable: key.to_string(),
                        value: value.to_string(),
                    }),
                    expires_at: None,
                });
            }
            Err(rule) => {
                bad += 1;
                if bad <= LINE_PROBLEMS {
                    c.problems.push(ProblemDraft::new(
                        ProblemCode::Unreadable,
                        line_problem(file, n + 1, rule),
                    ));
                }
            }
        }
    }
    if bad > LINE_PROBLEMS {
        c.problems.push(ProblemDraft::new(
            ProblemCode::Unreadable,
            format!(
                "api-keys.env_files[{file}]: {} more lines break the env rules and were not delivered",
                bad - LINE_PROBLEMS
            ),
        ));
    }
}

impl Provider for ApiKeys {
    fn name(&self) -> &str {
        NAME
    }

    fn only(&self) -> Option<&Only> {
        self.settings.only.as_ref()
    }

    fn provide(&self, _ctx: &Context<'_>, reader: &dyn Reader) -> Contribution {
        let mut c = Contribution::default();
        if self.settings.craze_providers {
            match read_setting_file(
                reader,
                &format!("~/{CRAZE_PROVIDERS}"),
                MAX_FILE_BYTES,
                "~/.craze/native/providers.toml",
            ) {
                Ok(f) => c.items.push(Draft {
                    name: format!("{NAME}.craze"),
                    body: ItemBody::File(FileItem {
                        stage: Stage::One,
                        base: Base::Home,
                        path: CRAZE_PROVIDERS.into(),
                        mode: Mode::M0600,
                        content: f.bytes,
                    }),
                    expires_at: None,
                }),
                Err(e) => c.problems.extend(e.problems),
            }
        }
        for (i, path) in self.settings.env_files.iter().enumerate() {
            let what = format!("api-keys.env_files[{i}]");
            let f = match read_setting_file(reader, path, MAX_ENV_FILE_BYTES, &what) {
                Ok(f) => f,
                Err(e) => {
                    c.problems.extend(e.problems);
                    continue;
                }
            };
            if !f.owner_only() {
                c.problems.push(ProblemDraft::new(
                    ProblemCode::Unreadable,
                    format!("{what} can be read by others; set its mode to 0600"),
                ));
                continue;
            }
            let Ok(text) = std::str::from_utf8(&f.bytes) else {
                c.problems.push(ProblemDraft::new(
                    ProblemCode::Unreadable,
                    format!("{what} is not UTF-8 text"),
                ));
                continue;
            };
            parse_env_file(i, text, &mut c);
        }
        c
    }
}
