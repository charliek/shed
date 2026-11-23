//! `delivery.toml`: which providers deliver, where, and from what.
//!
//! The file lives beside `extensions.yaml`, at `~/.config/shed/delivery.toml`
//! ([`settings_path`]). Every table is optional, and a provider whose table
//! is absent does not deliver. Unknown keys are refused, so a misspelt key
//! fails loudly rather than switching something off.
//!
//! ```toml
//! [ssh-key]
//! enabled = true
//! key_file = "~/.ssh/id_ed25519"      # its public half is key_file + ".pub"
//! sign_commits = true                 # git signs commits with this key
//!
//! [github]
//! enabled = true
//! gh = "/opt/homebrew/bin/gh"         # optional; default "gh" on the PATH
//!
//! [api-keys]
//! enabled = true
//! craze_providers = true              # optional; default true: ~/.craze/native/providers.toml
//! env_files = ["~/.config/shed/keys.env"]   # KEY=VALUE lines, each file mode 0600
//!
//! [envsecrets-login]
//! enabled = true
//! passphrase_file = "~/.config/envsecrets/passphrase"
//! # or: passphrase_command = ["security", "find-generic-password", "-s", "envsecrets", "-w"]
//!
//! [files]
//! enabled = true
//! [[files.file]]
//! from = "~/.npmrc"
//! to = ".npmrc"
//! [[files.file]]
//! from = "~/work/widgets.env"
//! to = ".env"
//! base = "project"                    # "home" (default) or "project": stage 2
//! mode = "0400"                       # "0600" (default) or "0400"
//! only = { repos = ["example/widgets"] }
//!
//! # Any provider (and any files entry) may say where it delivers. Every list
//! # given must match; an absent list does not restrict; a list is never empty.
//! [github.only]
//! servers = ["work"]                  # the server's name in ~/.shed/config.yaml
//! backends = ["remote"]               # "firecracker", "vz", "remote"
//! repos = ["example/*"]               # owner/name, `*` within a part
//! ```
//!
//! A setting that names a file takes an absolute path or one starting `~/`.

use std::fmt;
use std::ops::Range;
use std::path::{Path, PathBuf};

use shed_provision::{check_path, valid_repo_pattern, Backend, Base, Only};
use toml::de::{DeTable, DeValue};
use toml::Spanned;

/// The settings file's name, in the app's config directory.
pub const SETTINGS_FILE_NAME: &str = "delivery.toml";

/// Where the settings file is: `<home>/.config/shed/delivery.toml`.
pub fn settings_path(home: &Path) -> PathBuf {
    home.join(".config").join("shed").join(SETTINGS_FILE_NAME)
}

/// The whole settings file.
#[derive(Debug, Clone, Default, PartialEq, Eq)]
pub struct Settings {
    /// `[ssh-key]`
    pub ssh_key: Option<SshKeySettings>,
    /// `[github]`
    pub github: Option<GithubSettings>,
    /// `[api-keys]`
    pub api_keys: Option<ApiKeysSettings>,
    /// `[envsecrets-login]`
    pub envsecrets_login: Option<EnvsecretsLoginSettings>,
    /// `[files]`
    pub files: Option<FilesSettings>,
}

/// `[ssh-key]`: one SSH key pair.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct SshKeySettings {
    /// Whether it delivers.
    pub enabled: bool,
    /// Where it delivers.
    pub only: Option<Only>,
    /// The private key; its public half is beside it, with `.pub`.
    pub key_file: Option<String>,
    /// Whether git in the shed signs commits with it (default false).
    pub sign_commits: bool,
}

/// `[github]`: the `gh` sign-in.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct GithubSettings {
    /// Whether it delivers.
    pub enabled: bool,
    /// Where it delivers.
    pub only: Option<Only>,
    /// The `gh` program: a name on the PATH, or an absolute path.
    pub gh: Option<String>,
}

/// `[api-keys]`: craze's API keys and env files.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct ApiKeysSettings {
    /// Whether it delivers.
    pub enabled: bool,
    /// Where it delivers.
    pub only: Option<Only>,
    /// Whether to deliver `~/.craze/native/providers.toml` (default true).
    /// No other file of craze's is ever read.
    pub craze_providers: bool,
    /// Files of `KEY=VALUE` lines, each readable by its owner only.
    pub env_files: Vec<String>,
}

/// `[envsecrets-login]`: the envsecrets config and its passphrase.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct EnvsecretsLoginSettings {
    /// Whether it delivers.
    pub enabled: bool,
    /// Where it delivers.
    pub only: Option<Only>,
    /// A file holding the passphrase (one trailing line end is not part of
    /// it).
    pub passphrase_file: Option<String>,
    /// A command printing the passphrase: an argv, run with no shell.
    pub passphrase_command: Option<Vec<String>>,
}

/// `[files]`: files copied as they are.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct FilesSettings {
    /// Whether it delivers.
    pub enabled: bool,
    /// Where it delivers.
    pub only: Option<Only>,
    /// `[[files.file]]` entries.
    pub files: Vec<FileEntry>,
}

/// One `[[files.file]]` entry.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct FileEntry {
    /// The file on this machine.
    pub from: String,
    /// Where it goes, relative to `base`, by the contract's path rules.
    pub to: String,
    /// `home` (default, stage 1) or `project` (stage 2, after the clone).
    pub base: Base,
    /// `0600` (default) or `0400`.
    pub mode: Option<String>,
    /// Where this entry delivers, beside the provider's own rules.
    pub only: Option<Only>,
}

/// What is wrong with the settings file: a fixed description, never the
/// file's text.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum SettingsProblem {
    /// The file is not TOML.
    NotToml,
    /// A key the schema does not have. Its name is the person's text, so it
    /// is not repeated: only where it is.
    UnknownKey,
    /// A key the schema requires is absent.
    MissingKey,
    /// A value of the wrong type (a number for a string, say).
    WrongType,
    /// A value of the right type that the schema does not allow.
    InvalidValue,
    /// A list that must not be empty is.
    EmptyList,
    /// `envsecrets-login` sets both `passphrase_file` and
    /// `passphrase_command`.
    BothPassphraseSources,
}

impl SettingsProblem {
    /// The problem, for people.
    pub fn as_str(self) -> &'static str {
        match self {
            SettingsProblem::NotToml => "not valid TOML",
            SettingsProblem::UnknownKey => "unknown key",
            SettingsProblem::MissingKey => "missing key",
            SettingsProblem::WrongType => "wrong type",
            SettingsProblem::InvalidValue => "invalid value",
            SettingsProblem::EmptyList => "empty list",
            SettingsProblem::BothPassphraseSources => {
                "both passphrase_file and passphrase_command; set one"
            }
        }
    }
}

/// The settings file could not be used. Every part of it is fixed text or
/// comes from the schema (`key` is a path of the schema's own names and
/// list indexes, `expected` a fixed description): it never holds the
/// file's text, a value or a key name the person typed.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct SettingsError {
    /// The 1-based line.
    pub line: usize,
    /// The 1-based column, in characters.
    pub column: usize,
    /// The schema path of the key (`github.only.backends[0]`), when the
    /// problem is at a key the schema names.
    pub key: Option<String>,
    /// What is wrong.
    pub problem: SettingsProblem,
    /// What the schema allows there, when that helps.
    pub expected: Option<&'static str>,
}

impl fmt::Display for SettingsError {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        write!(
            f,
            "{SETTINGS_FILE_NAME} line {}, column {}: ",
            self.line, self.column
        )?;
        if let Some(k) = &self.key {
            write!(f, "{k}: ")?;
        }
        f.write_str(self.problem.as_str())?;
        if let Some(e) = self.expected {
            write!(f, " (expected {e})")?;
        }
        Ok(())
    }
}

impl std::error::Error for SettingsError {}

const EXPECT_PATH: &str = "an absolute path or one starting with ~/";
const EXPECT_BACKEND: &str = "\"firecracker\", \"vz\" or \"remote\"";

type Value<'i> = Spanned<DeValue<'i>>;

/// Strings of a list, each with where it is.
type Located = Vec<(String, Range<usize>)>;

/// Reads the document against the schema, with each error located.
struct Walk<'t> {
    text: &'t str,
}

impl Walk<'_> {
    fn err(
        &self,
        span: Range<usize>,
        key: Option<&str>,
        problem: SettingsProblem,
        expected: Option<&'static str>,
    ) -> SettingsError {
        let start = span.start.min(self.text.len());
        let before = &self.text[..start];
        let line = before.matches('\n').count() + 1;
        let column = before.rsplit('\n').next().map_or(0, |l| l.chars().count()) + 1;
        SettingsError {
            line,
            column,
            key: key.map(str::to_string),
            problem,
            expected,
        }
    }

    /// No key of `t` outside `allowed`.
    fn keys(&self, t: &DeTable<'_>, allowed: &[&str]) -> Result<(), SettingsError> {
        for (k, _) in t.iter() {
            if !allowed.contains(&k.get_ref().as_ref()) {
                return Err(self.err(k.span(), None, SettingsProblem::UnknownKey, None));
            }
        }
        Ok(())
    }

    fn table<'a, 'i>(&self, v: &'a Value<'i>, key: &str) -> Result<&'a DeTable<'i>, SettingsError> {
        match v.get_ref() {
            DeValue::Table(t) => Ok(t),
            _ => Err(self.err(
                v.span(),
                Some(key),
                SettingsProblem::WrongType,
                Some("a table"),
            )),
        }
    }

    fn boolean(&self, v: &Value<'_>, key: &str) -> Result<bool, SettingsError> {
        match v.get_ref() {
            DeValue::Boolean(b) => Ok(*b),
            _ => Err(self.err(
                v.span(),
                Some(key),
                SettingsProblem::WrongType,
                Some("true or false"),
            )),
        }
    }

    fn string(&self, v: &Value<'_>, key: &str) -> Result<String, SettingsError> {
        match v.get_ref() {
            DeValue::String(s) if s.contains('\0') => Err(self.err(
                v.span(),
                Some(key),
                SettingsProblem::InvalidValue,
                Some("text with no NUL"),
            )),
            DeValue::String(s) => Ok(s.to_string()),
            _ => Err(self.err(
                v.span(),
                Some(key),
                SettingsProblem::WrongType,
                Some("a string"),
            )),
        }
    }

    /// A list of strings, each with its span.
    fn strings(
        &self,
        v: &Value<'_>,
        key: &str,
    ) -> Result<Vec<(String, Range<usize>)>, SettingsError> {
        let DeValue::Array(a) = v.get_ref() else {
            return Err(self.err(
                v.span(),
                Some(key),
                SettingsProblem::WrongType,
                Some("a list of strings"),
            ));
        };
        a.iter()
            .enumerate()
            .map(|(i, e)| Ok((self.string(e, &format!("{key}[{i}]"))?, e.span())))
            .collect()
    }

    fn path(&self, v: &Value<'_>, key: &str) -> Result<String, SettingsError> {
        let s = self.string(v, key)?;
        if !(s.starts_with('/') || s.starts_with("~/")) {
            return Err(self.err(
                v.span(),
                Some(key),
                SettingsProblem::InvalidValue,
                Some(EXPECT_PATH),
            ));
        }
        Ok(s)
    }

    /// The common part of a provider table: its keys, `enabled` and `only`.
    fn provider(
        &self,
        v: &Value<'_>,
        name: &str,
        own: &[&str],
    ) -> Result<(bool, Option<Only>), SettingsError> {
        let t = self.table(v, name)?;
        let mut allowed = vec!["enabled", "only"];
        allowed.extend_from_slice(own);
        self.keys(t, &allowed)?;
        let enabled = match find(t, "enabled") {
            Some(e) => self.boolean(e, &format!("{name}.enabled"))?,
            None => {
                return Err(self.err(
                    v.span(),
                    Some(&format!("{name}.enabled")),
                    SettingsProblem::MissingKey,
                    Some("true or false"),
                ))
            }
        };
        let only = find(t, "only")
            .map(|o| self.only(o, &format!("{name}.only")))
            .transpose()?;
        Ok((enabled, only))
    }

    fn only(&self, v: &Value<'_>, key: &str) -> Result<Only, SettingsError> {
        let t = self.table(v, key)?;
        self.keys(t, &["servers", "backends", "repos"])?;
        let list = |name: &str| -> Result<Option<Located>, SettingsError> {
            let Some(l) = find(t, name) else {
                return Ok(None);
            };
            let k = format!("{key}.{name}");
            let items = self.strings(l, &k)?;
            if items.is_empty() {
                return Err(self.err(l.span(), Some(&k), SettingsProblem::EmptyList, None));
            }
            // A blank entry matches nothing; read loosely it would be
            // dropped, and the list could end up restricting nothing.
            if let Some(i) = items.iter().position(|(s, _)| s.is_empty()) {
                return Err(self.err(
                    items[i].1.clone(),
                    Some(&format!("{k}[{i}]")),
                    SettingsProblem::InvalidValue,
                    None,
                ));
            }
            Ok(Some(items))
        };
        let servers = list("servers")?.map(|l| l.into_iter().map(|(s, _)| s).collect());
        let backends = match list("backends")? {
            None => None,
            Some(l) => Some(
                l.into_iter()
                    .enumerate()
                    .map(|(i, (s, sp))| {
                        Backend::from_name(&s).ok_or_else(|| {
                            self.err(
                                sp,
                                Some(&format!("{key}.backends[{i}]")),
                                SettingsProblem::InvalidValue,
                                Some(EXPECT_BACKEND),
                            )
                        })
                    })
                    .collect::<Result<Vec<_>, _>>()?,
            ),
        };
        let repos = match list("repos")? {
            None => None,
            Some(l) => {
                for (i, (s, sp)) in l.iter().enumerate() {
                    if !valid_repo_pattern(s) {
                        return Err(self.err(
                            sp.clone(),
                            Some(&format!("{key}.repos[{i}]")),
                            SettingsProblem::InvalidValue,
                            Some("an owner/name pattern"),
                        ));
                    }
                }
                Some(l.into_iter().map(|(s, _)| s).collect())
            }
        };
        Ok(Only {
            servers,
            backends,
            repos,
        })
    }

    fn ssh_key(&self, v: &Value<'_>) -> Result<SshKeySettings, SettingsError> {
        let (enabled, only) = self.provider(v, "ssh-key", &["key_file", "sign_commits"])?;
        let t = self.table(v, "ssh-key")?;
        Ok(SshKeySettings {
            enabled,
            only,
            key_file: find(t, "key_file")
                .map(|k| self.path(k, "ssh-key.key_file"))
                .transpose()?,
            sign_commits: find(t, "sign_commits")
                .map(|b| self.boolean(b, "ssh-key.sign_commits"))
                .transpose()?
                .unwrap_or(false),
        })
    }

    fn github(&self, v: &Value<'_>) -> Result<GithubSettings, SettingsError> {
        let (enabled, only) = self.provider(v, "github", &["gh"])?;
        let t = self.table(v, "github")?;
        let gh = match find(t, "gh") {
            None => None,
            Some(g) => {
                let s = self.string(g, "github.gh")?;
                if s.is_empty() {
                    return Err(self.err(
                        g.span(),
                        Some("github.gh"),
                        SettingsProblem::InvalidValue,
                        Some("a program name or path"),
                    ));
                }
                Some(s)
            }
        };
        Ok(GithubSettings { enabled, only, gh })
    }

    fn api_keys(&self, v: &Value<'_>) -> Result<ApiKeysSettings, SettingsError> {
        let (enabled, only) = self.provider(v, "api-keys", &["craze_providers", "env_files"])?;
        let t = self.table(v, "api-keys")?;
        let mut env_files = Vec::new();
        if let Some(l) = find(t, "env_files") {
            let DeValue::Array(a) = l.get_ref() else {
                return Err(self.err(
                    l.span(),
                    Some("api-keys.env_files"),
                    SettingsProblem::WrongType,
                    Some("a list of paths"),
                ));
            };
            for (i, e) in a.iter().enumerate() {
                env_files.push(self.path(e, &format!("api-keys.env_files[{i}]"))?);
            }
        }
        Ok(ApiKeysSettings {
            enabled,
            only,
            craze_providers: find(t, "craze_providers")
                .map(|b| self.boolean(b, "api-keys.craze_providers"))
                .transpose()?
                .unwrap_or(true),
            env_files,
        })
    }

    fn envsecrets_login(&self, v: &Value<'_>) -> Result<EnvsecretsLoginSettings, SettingsError> {
        const NAME: &str = "envsecrets-login";
        let (enabled, only) = self.provider(v, NAME, &["passphrase_file", "passphrase_command"])?;
        let t = self.table(v, NAME)?;
        let passphrase_file = find(t, "passphrase_file")
            .map(|f| self.path(f, "envsecrets-login.passphrase_file"))
            .transpose()?;
        let passphrase_command = match find(t, "passphrase_command") {
            None => None,
            Some(c) => {
                let key = "envsecrets-login.passphrase_command";
                let argv: Vec<String> = self.strings(c, key)?.into_iter().map(|(s, _)| s).collect();
                if argv.first().is_none_or(String::is_empty) {
                    return Err(self.err(
                        c.span(),
                        Some(key),
                        SettingsProblem::InvalidValue,
                        Some("a list starting with a program"),
                    ));
                }
                Some(argv)
            }
        };
        if passphrase_file.is_some() && passphrase_command.is_some() {
            return Err(self.err(
                v.span(),
                Some(NAME),
                SettingsProblem::BothPassphraseSources,
                None,
            ));
        }
        Ok(EnvsecretsLoginSettings {
            enabled,
            only,
            passphrase_file,
            passphrase_command,
        })
    }

    fn files(&self, v: &Value<'_>) -> Result<FilesSettings, SettingsError> {
        let (enabled, only) = self.provider(v, "files", &["file"])?;
        let t = self.table(v, "files")?;
        let mut files = Vec::new();
        if let Some(l) = find(t, "file") {
            let DeValue::Array(a) = l.get_ref() else {
                return Err(self.err(
                    l.span(),
                    Some("files.file"),
                    SettingsProblem::WrongType,
                    Some("a list of tables"),
                ));
            };
            for (i, e) in a.iter().enumerate() {
                files.push(self.file_entry(e, &format!("files.file[{i}]"))?);
            }
        }
        Ok(FilesSettings {
            enabled,
            only,
            files,
        })
    }

    fn file_entry(&self, v: &Value<'_>, at: &str) -> Result<FileEntry, SettingsError> {
        let t = self.table(v, at)?;
        self.keys(t, &["from", "to", "base", "mode", "only"])?;
        let required = |name: &str| {
            find(t, name).ok_or_else(|| {
                self.err(
                    v.span(),
                    Some(&format!("{at}.{name}")),
                    SettingsProblem::MissingKey,
                    None,
                )
            })
        };
        let from = self.path(required("from")?, &format!("{at}.from"))?;
        let to_value = required("to")?;
        let to = self.string(to_value, &format!("{at}.to"))?;
        if let Some(rule) = check_path(&to) {
            return Err(self.err(
                to_value.span(),
                Some(&format!("{at}.to")),
                SettingsProblem::InvalidValue,
                Some(rule.as_str()),
            ));
        }
        let base = match find(t, "base") {
            None => Base::Home,
            Some(b) => match self.string(b, &format!("{at}.base"))?.as_str() {
                "home" => Base::Home,
                "project" => Base::Project,
                _ => {
                    return Err(self.err(
                        b.span(),
                        Some(&format!("{at}.base")),
                        SettingsProblem::InvalidValue,
                        Some("\"home\" or \"project\""),
                    ))
                }
            },
        };
        let mode = match find(t, "mode") {
            None => None,
            Some(m) => {
                let s = self.string(m, &format!("{at}.mode"))?;
                if s != "0600" && s != "0400" {
                    return Err(self.err(
                        m.span(),
                        Some(&format!("{at}.mode")),
                        SettingsProblem::InvalidValue,
                        Some("\"0600\" or \"0400\""),
                    ));
                }
                Some(s)
            }
        };
        let only = find(t, "only")
            .map(|o| self.only(o, &format!("{at}.only")))
            .transpose()?;
        Ok(FileEntry {
            from,
            to,
            base,
            mode,
            only,
        })
    }
}

/// The value of `t`'s key `name`.
fn find<'a, 'i>(t: &'a DeTable<'i>, name: &str) -> Option<&'a Value<'i>> {
    t.iter()
        .find(|(k, _)| k.get_ref().as_ref() == name)
        .map(|(_, v)| v)
}

impl Settings {
    /// Reads and checks a settings file's text against the schema of this
    /// module. An error says where and what, in fixed words: it never quotes
    /// the file.
    pub fn parse(text: &str) -> Result<Settings, SettingsError> {
        let w = Walk { text };
        let doc = DeTable::parse(text).map_err(|e| {
            w.err(
                e.span().unwrap_or(0..0),
                None,
                SettingsProblem::NotToml,
                None,
            )
        })?;
        let root = doc.get_ref();
        w.keys(
            root,
            &["ssh-key", "github", "api-keys", "envsecrets-login", "files"],
        )?;
        Ok(Settings {
            ssh_key: find(root, "ssh-key").map(|v| w.ssh_key(v)).transpose()?,
            github: find(root, "github").map(|v| w.github(v)).transpose()?,
            api_keys: find(root, "api-keys").map(|v| w.api_keys(v)).transpose()?,
            envsecrets_login: find(root, "envsecrets-login")
                .map(|v| w.envsecrets_login(v))
                .transpose()?,
            files: find(root, "files").map(|v| w.files(v)).transpose()?,
        })
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn the_documented_example_parses() {
        let doc = include_str!("settings.rs");
        let example: String = doc
            .lines()
            .skip_while(|l| !l.starts_with("//! ```toml"))
            .skip(1)
            .take_while(|l| !l.starts_with("//! ```"))
            .map(|l| {
                l.strip_prefix("//! ")
                    .unwrap_or(l.strip_prefix("//!").unwrap_or(l))
            })
            .collect::<Vec<_>>()
            .join("\n");
        let s = Settings::parse(&example).unwrap_or_else(|e| panic!("{e}"));
        assert!(s.ssh_key.as_ref().unwrap().sign_commits);
        assert_eq!(
            s.github.as_ref().unwrap().only.as_ref().unwrap().backends,
            Some(vec![Backend::Remote])
        );
        let files = &s.files.as_ref().unwrap().files;
        assert_eq!(files.len(), 2);
        assert_eq!(files[0].base, Base::Home);
        assert_eq!(files[1].base, Base::Project);
        assert_eq!(files[1].mode.as_deref(), Some("0400"));
        assert!(files[1].only.as_ref().unwrap().repos.is_some());
        let api = s.api_keys.as_ref().unwrap();
        assert!(api.craze_providers);
        assert_eq!(api.env_files.len(), 1);
        assert!(s
            .envsecrets_login
            .as_ref()
            .unwrap()
            .passphrase_file
            .is_some());
        assert_eq!(Settings::parse("").unwrap(), Settings::default());
    }

    /// (text, line, column, key, problem)
    #[test]
    fn errors_are_located_and_fixed() {
        use SettingsProblem::*;
        let cases: &[(&str, usize, usize, Option<&str>, SettingsProblem)] = &[
            ("[ssh-key]\nenabled = true\nkey_fil = \"~/.ssh/id\"\n", 3, 1, None, UnknownKey),
            ("[sshkey]\nenabled = true\n", 1, 2, None, UnknownKey),
            ("[github]\ngh = \"gh\"\n", 1, 1, Some("github.enabled"), MissingKey),
            ("[github]\nenabled = true\n[github.only]\nservers = []\n", 4, 11, Some("github.only.servers"), EmptyList),
            ("[github]\nenabled = true\n[github.only]\nservers = [\"\"]\n", 4, 12, Some("github.only.servers[0]"), InvalidValue),
            ("[github]\nenabled = true\n[github.only]\nrepos = [\"widgets\"]\n", 4, 10, Some("github.only.repos[0]"), InvalidValue),
            ("[github]\nenabled = true\n[github.only]\nbackends = [\"docker\"]\n", 4, 13, Some("github.only.backends[0]"), InvalidValue),
            ("[ssh-key]\nenabled = true\nkey_file = \"id_ed25519\"\n", 3, 12, Some("ssh-key.key_file"), InvalidValue),
            ("[ssh-key]\nenabled = 1\n", 2, 11, Some("ssh-key.enabled"), WrongType),
            ("[files]\nenabled = true\n[[files.file]]\nfrom = \"~/a\"\nto = \"../a\"\n", 5, 6, Some("files.file[0].to"), InvalidValue),
            ("[files]\nenabled = true\n[[files.file]]\nfrom = \"~/a\"\nto = \"a\"\nmode = \"0644\"\n", 6, 8, Some("files.file[0].mode"), InvalidValue),
            ("[files]\nenabled = true\n[[files.file]]\nto = \"a\"\n", 3, 1, Some("files.file[0].from"), MissingKey),
            (
                "[envsecrets-login]\nenabled = true\npassphrase_file = \"~/p\"\npassphrase_command = [\"x\"]\n",
                1,
                1,
                Some("envsecrets-login"),
                BothPassphraseSources,
            ),
            ("[envsecrets-login]\nenabled = true\npassphrase_command = []\n", 3, 22, Some("envsecrets-login.passphrase_command"), InvalidValue),
            ("[github\nenabled = true\n", 1, 8, None, NotToml),
            ("[github]\nenabled = true\nenabled = false\n", 3, 1, None, NotToml),
        ];
        for (text, line, column, key, problem) in cases {
            let e = Settings::parse(text).unwrap_err();
            assert_eq!(
                (e.line, e.column, e.key.as_deref(), e.problem),
                (*line, *column, *key, *problem),
                "{text:?}: {e}"
            );
        }
    }

    /// A value, a number or a key name the person typed never comes back in
    /// an error, whatever the parser would have said about it.
    #[test]
    fn errors_never_repeat_the_file() {
        for text in [
            "[github]\nenabled = true\n[github.only]\nbackends = [\"ghp_SENTINEL\"]\n",
            "[github]\nenabled = \"ghp_SENTINEL\"\n",
            "[github]\nenabled = 424242424242\n",
            "[github]\nenabled = true\ngh = 424242424242\n",
            "[github]\nenabled = true\nghp_SENTINEL = 1\n",
            "[github]\nenabled = true\n\"ghp SENTINEL\" = 1\n",
            "ghp_SENTINEL = true\n",
            "[github]\nenabled = true\n[github.only]\nrepos = [\"ghp_SENTINEL\"]\n",
            "[github]\nenabled = tru ghp_SENTINEL\n",
            "[github]\nenabled = \"unterminated ghp_SENTINEL\n",
            "[ssh-key]\nenabled = true\nkey_file = \"ghp_SENTINEL\"\n",
            "[envsecrets-login]\nenabled = true\npassphrase_command = [424242424242]\n",
            "[files]\nenabled = true\n[[files.file]]\nfrom = \"~/a\"\nto = \"a\"\nbase = \"ghp_SENTINEL\"\n",
            "[files]\nenabled = true\nfile = \"ghp_SENTINEL\"\n",
        ] {
            let e = Settings::parse(text).unwrap_err();
            let shown = format!("{e} {e:?}");
            assert!(!shown.contains("SENTINEL") && !shown.contains("424242"), "{text:?}: {shown}");
        }
    }
}
