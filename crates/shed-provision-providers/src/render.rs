//! Writing settings back: [`Settings::to_toml`] renders a whole `delivery.toml` that
//! [`Settings::parse`] reads back as the same settings. The desktop's settings op writes
//! the file whole with it; comments and layout of a hand-written file are not kept.

use std::fmt::Write as _;

use shed_provision::{Base, Only};

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

fn header(out: &mut String, name: &str, enabled: bool, o: Option<&Only>) {
    if !out.is_empty() {
        out.push('\n');
    }
    let _ = writeln!(out, "[{name}]");
    let _ = writeln!(out, "enabled = {enabled}");
    if let Some(o) = o {
        let _ = writeln!(out, "only = {}", only(o));
    }
}

fn file_entry(out: &mut String, e: &FileEntry) {
    out.push_str("[[files.file]]\n");
    let _ = writeln!(out, "from = {}", string(&e.from));
    let _ = writeln!(out, "to = {}", string(&e.to));
    if e.base == Base::Project {
        out.push_str("base = \"project\"\n");
    }
    if let Some(m) = &e.mode {
        let _ = writeln!(out, "mode = {}", string(m));
    }
    if let Some(o) = &e.only {
        let _ = writeln!(out, "only = {}", only(o));
    }
}

impl Settings {
    /// The whole settings file for these settings.
    pub fn to_toml(&self) -> String {
        let mut out = String::new();
        if let Some(s) = &self.ssh_key {
            header(&mut out, "ssh-key", s.enabled, s.only.as_ref());
            if let Some(k) = &s.key_file {
                let _ = writeln!(out, "key_file = {}", string(k));
            }
            let _ = writeln!(out, "sign_commits = {}", s.sign_commits);
        }
        if let Some(s) = &self.github {
            header(&mut out, "github", s.enabled, s.only.as_ref());
            if let Some(g) = &s.gh {
                let _ = writeln!(out, "gh = {}", string(g));
            }
        }
        if let Some(s) = &self.api_keys {
            header(&mut out, "api-keys", s.enabled, s.only.as_ref());
            let _ = writeln!(out, "craze_providers = {}", s.craze_providers);
            if !s.env_files.is_empty() {
                let _ = writeln!(
                    out,
                    "env_files = {}",
                    list(s.env_files.iter().map(String::as_str))
                );
            }
        }
        if let Some(s) = &self.envsecrets_login {
            header(&mut out, "envsecrets-login", s.enabled, s.only.as_ref());
            if let Some(f) = &s.passphrase_file {
                let _ = writeln!(out, "passphrase_file = {}", string(f));
            }
            if let Some(c) = &s.passphrase_command {
                let _ = writeln!(
                    out,
                    "passphrase_command = {}",
                    list(c.iter().map(String::as_str))
                );
            }
        }
        if let Some(s) = &self.files {
            header(&mut out, "files", s.enabled, s.only.as_ref());
            for e in &s.files {
                file_entry(&mut out, e);
            }
        }
        out
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
