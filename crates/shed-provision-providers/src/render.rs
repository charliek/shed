//! Writing settings back: [`Settings::table`] is one provider's table as the settings
//! hold it, and [`Settings::to_toml`] renders a whole `delivery.toml` from the tables
//! that [`Settings::parse`] reads back as the same settings. The desktop's settings op
//! writes the file whole with it, and its JSON view is made from the tables; comments
//! and layout of a hand-written file are not kept.

use std::fmt::Write as _;

use shed_provision::{Base, Only};

use crate::describe::describe;
use crate::settings::{FileEntry, Settings};

/// A TOML basic string: quotes, backslashes and every control character escaped.
fn string(s: &str) -> String {
    let mut out = String::with_capacity(s.len() + 2);
    out.push('"');
    for c in s.chars() {
        match c {
            '"' => out.push_str("\\\""),
            '\\' => out.push_str("\\\\"),
            '\n' => out.push_str("\\n"),
            '\t' => out.push_str("\\t"),
            '\r' => out.push_str("\\r"),
            c if (c as u32) < 0x20 || c == '\u{7f}' => {
                let _ = write!(out, "\\u{:04X}", c as u32);
            }
            c => out.push(c),
        }
    }
    out.push('"');
    out
}

fn list<'a>(items: impl IntoIterator<Item = &'a str>) -> String {
    let parts: Vec<String> = items.into_iter().map(string).collect();
    format!("[{}]", parts.join(", "))
}

/// An `only` rule as an inline table.
fn only(o: &Only) -> String {
    let mut parts = Vec::new();
    if let Some(s) = &o.servers {
        parts.push(format!("servers = {}", list(s.iter().map(String::as_str))));
    }
    if let Some(b) = &o.backends {
        parts.push(format!("backends = {}", list(b.iter().map(|b| b.as_str()))));
    }
    if let Some(r) = &o.repos {
        parts.push(format!("repos = {}", list(r.iter().map(String::as_str))));
    }
    format!("{{ {} }}", parts.join(", "))
}

/// One setting's value, as the settings hold it.
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum SettingValue {
    /// `true` or `false`.
    Switch(bool),
    /// A string.
    Text(String),
    /// A list of strings.
    List(Vec<String>),
    /// A list of tables (`[[files.file]]`), each its keys and values in order.
    Entries(Vec<Vec<(&'static str, SettingValue)>>),
    /// An entry's own `only` rule.
    Only(Only),
}

/// One provider's table, as [`Settings::to_toml`] writes it: `enabled`, `only`, and
/// the other keys it holds, in order. A key the settings leave at its default is
/// absent, except a switch, which is always written.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct ProviderTable {
    /// `enabled`.
    pub enabled: bool,
    /// `only`.
    pub only: Option<Only>,
    /// The other keys.
    pub values: Vec<(&'static str, SettingValue)>,
}

impl ProviderTable {
    /// The value of `key`.
    pub fn get(&self, key: &str) -> Option<&SettingValue> {
        self.values.iter().find(|(k, _)| *k == key).map(|(_, v)| v)
    }
}

fn text(v: &Option<String>) -> Option<SettingValue> {
    v.clone().map(SettingValue::Text)
}

fn entry(e: &FileEntry) -> Vec<(&'static str, SettingValue)> {
    let mut out = vec![
        ("from", SettingValue::Text(e.from.clone())),
        ("to", SettingValue::Text(e.to.clone())),
    ];
    if e.base == Base::Project {
        out.push(("base", SettingValue::Text("project".into())));
    }
    if let Some(m) = &e.mode {
        out.push(("mode", SettingValue::Text(m.clone())));
    }
    if let Some(o) = &e.only {
        out.push(("only", SettingValue::Only(o.clone())));
    }
    out
}

impl Settings {
    /// `provider`'s table, or `None` when the settings have none for it (or there is
    /// no such provider).
    pub fn table(&self, provider: &str) -> Option<ProviderTable> {
        let mut values: Vec<(&'static str, Option<SettingValue>)> = Vec::new();
        let (enabled, only) = match provider {
            "ssh-key" => {
                let s = self.ssh_key.as_ref()?;
                values.push(("key_file", text(&s.key_file)));
                values.push(("sign_commits", Some(SettingValue::Switch(s.sign_commits))));
                (s.enabled, s.only.clone())
            }
            "github" => {
                let s = self.github.as_ref()?;
                values.push(("gh", text(&s.gh)));
                (s.enabled, s.only.clone())
            }
            "api-keys" => {
                let s = self.api_keys.as_ref()?;
                values.push((
                    "craze_providers",
                    Some(SettingValue::Switch(s.craze_providers)),
                ));
                values.push((
                    "env_files",
                    (!s.env_files.is_empty()).then(|| SettingValue::List(s.env_files.clone())),
                ));
                (s.enabled, s.only.clone())
            }
            "envsecrets-login" => {
                let s = self.envsecrets_login.as_ref()?;
                values.push(("passphrase_file", text(&s.passphrase_file)));
                values.push((
                    "passphrase_command",
                    s.passphrase_command.clone().map(SettingValue::List),
                ));
                (s.enabled, s.only.clone())
            }
            "files" => {
                let s = self.files.as_ref()?;
                values.push((
                    "file",
                    Some(SettingValue::Entries(s.files.iter().map(entry).collect())),
                ));
                (s.enabled, s.only.clone())
            }
            _ => return None,
        };
        Some(ProviderTable {
            enabled,
            only,
            values: values
                .into_iter()
                .filter_map(|(k, v)| v.map(|v| (k, v)))
                .collect(),
        })
    }

    /// The whole settings file for these settings.
    pub fn to_toml(&self) -> String {
        let mut out = String::new();
        for name in describe().iter().map(|d| d.name) {
            let Some(t) = self.table(name) else {
                continue;
            };
            if !out.is_empty() {
                out.push('\n');
            }
            let _ = writeln!(out, "[{name}]");
            let _ = writeln!(out, "enabled = {}", t.enabled);
            if let Some(o) = &t.only {
                let _ = writeln!(out, "only = {}", only(o));
            }
            for (key, v) in &t.values {
                match v {
                    SettingValue::Entries(entries) => {
                        for e in entries {
                            let _ = writeln!(out, "[[{name}.{key}]]");
                            for (k, v) in e {
                                let _ = writeln!(out, "{k} = {}", value(v));
                            }
                        }
                    }
                    v => {
                        let _ = writeln!(out, "{key} = {}", value(v));
                    }
                }
            }
        }
        out
    }
}

/// A value as TOML (a list of tables is written by its caller).
fn value(v: &SettingValue) -> String {
    match v {
        SettingValue::Switch(b) => b.to_string(),
        SettingValue::Text(s) => string(s),
        SettingValue::List(l) => list(l.iter().map(String::as_str)),
        SettingValue::Only(o) => only(o),
        SettingValue::Entries(_) => unreachable!("a list of tables is written by its caller"),
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn example() -> String {
        include_str!("settings.rs")
            .lines()
            .skip_while(|l| !l.starts_with("//! ```toml"))
            .skip(1)
            .take_while(|l| !l.starts_with("//! ```"))
            .map(|l| {
                l.strip_prefix("//! ")
                    .unwrap_or(l.strip_prefix("//!").unwrap_or(l))
            })
            .collect::<Vec<_>>()
            .join("\n")
    }

    #[test]
    fn rendered_settings_read_back_the_same() {
        let s = Settings::parse(&example()).unwrap();
        let text = s.to_toml();
        assert_eq!(Settings::parse(&text).unwrap(), s, "{text}");
        assert_eq!(Settings::default().to_toml(), "");
        assert_eq!(
            Settings::parse(&Settings::default().to_toml()).unwrap(),
            Settings::default()
        );
    }

    #[test]
    fn strings_with_quotes_and_controls_survive() {
        let text = "[envsecrets-login]\nenabled = false\npassphrase_command = [\"/bin/x\", \"a \\\"b\\\" \\\\ c\", \"tab\\there\", \"é\\u007F\"]\n";
        let s = Settings::parse(text).unwrap();
        let back = Settings::parse(&s.to_toml()).unwrap();
        assert_eq!(back, s);
        assert_eq!(
            back.envsecrets_login.unwrap().passphrase_command.unwrap()[1],
            "a \"b\" \\ c"
        );
    }
}
