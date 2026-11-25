//! What each provider is, in a person's words, and the fields it has: what
//! the desktop's form and an account service's form are both drawn from
//! (plan 040 §3.4). A field exists on a machine (a key of the provider's
//! table in `delivery.toml`), in a service (part of what a person stores
//! there), or both. Every provider also has a switch (`enabled`, on a
//! machine) and a scope (`only`: servers, backends, repos), which are not
//! listed per provider.
//!
//! The settings walk allows exactly the machine fields' keys, and a test
//! holds each to a key the walk reads.

use serde::Serialize;

/// One provider, described.
#[derive(Debug, Clone, PartialEq, Eq, Serialize)]
pub struct Descriptor {
    /// Its name: `ssh-key`, `github`, `api-keys`, `envsecrets-login`, `files`.
    pub name: &'static str,
    /// What a person calls it.
    pub title: &'static str,
    /// One line on what it does for the person.
    pub summary: &'static str,
    /// Where its items land in the shed.
    pub lands: &'static str,
    /// Its fields, in the order a form shows them.
    pub fields: Vec<Field>,
}

/// What a field holds.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize)]
#[serde(rename_all = "lowercase")]
pub enum FieldKind {
    /// One line of text.
    Text,
    /// A value that is stored and never shown back.
    Secret,
    /// A file's content, uploaded.
    File,
    /// A path on the machine: absolute, or starting `~/`.
    Path,
    /// On or off.
    Switch,
    /// A list: of text, or of entries (see [`Field::entry`]).
    List,
}

/// Where a field exists.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize)]
#[serde(rename_all = "lowercase")]
pub enum Place {
    /// In `delivery.toml` on the machine.
    Machine,
    /// In what the person stores in an account service.
    Service,
    /// In both.
    Both,
}

impl Place {
    /// Whether the field exists on a machine.
    pub fn on_machine(self) -> bool {
        matches!(self, Place::Machine | Place::Both)
    }

    /// Whether the field exists in a service.
    pub fn in_service(self) -> bool {
        matches!(self, Place::Service | Place::Both)
    }
}

/// A field's value when it is not set.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize)]
#[serde(untagged)]
pub enum FieldDefault {
    /// A switch's.
    Switch(bool),
    /// A text field's.
    Text(&'static str),
}

/// One field of a provider.
#[derive(Debug, Clone, PartialEq, Eq, Serialize)]
pub struct Field {
    /// Its key: on a machine, the key in the provider's table.
    pub key: &'static str,
    /// What it holds.
    pub kind: FieldKind,
    /// What a person calls it.
    pub title: &'static str,
    /// One line of help.
    pub help: &'static str,
    /// Whether the provider can deliver nothing without it.
    pub required: bool,
    /// Where it exists.
    pub place: Place,
    /// Its value when it is not set (a machine's view always shows such a
    /// field).
    #[serde(skip_serializing_if = "Option::is_none")]
    pub default: Option<FieldDefault>,
    /// The values it may take, when they are few.
    #[serde(skip_serializing_if = "Vec::is_empty")]
    pub choices: Vec<&'static str>,
    /// Never shown back once stored: a view says only that it is set.
    #[serde(skip_serializing_if = "std::ops::Not::not")]
    pub write_only: bool,
    /// For a list of entries, each entry's fields.
    #[serde(skip_serializing_if = "Vec::is_empty")]
    pub entry: Vec<Field>,
    /// For a list of entries, whether each entry has a scope (`only`) of its
    /// own.
    #[serde(skip_serializing_if = "std::ops::Not::not")]
    pub scoped: bool,
}

impl Field {
    fn new(
        key: &'static str,
        kind: FieldKind,
        place: Place,
        title: &'static str,
        help: &'static str,
    ) -> Field {
        Field {
            key,
            kind,
            title,
            help,
            required: false,
            place,
            default: None,
            choices: Vec::new(),
            write_only: false,
            entry: Vec::new(),
            scoped: false,
        }
    }

    fn required(mut self) -> Field {
        self.required = true;
        self
    }

    fn default(mut self, d: FieldDefault) -> Field {
        self.default = Some(d);
        self
    }

    fn choices(mut self, c: &[&'static str]) -> Field {
        self.choices = c.to_vec();
        self
    }

    fn write_only(mut self) -> Field {
        self.write_only = true;
        self
    }

    fn entry(mut self, fields: Vec<Field>, scoped: bool) -> Field {
        self.entry = fields;
        self.scoped = scoped;
        self
    }
}

impl Descriptor {
    /// The fields that exist on a machine, in order.
    pub fn machine_fields(&self) -> impl Iterator<Item = &Field> {
        self.fields.iter().filter(|f| f.place.on_machine())
    }

    /// The fields that exist in a service, in order.
    pub fn service_fields(&self) -> impl Iterator<Item = &Field> {
        self.fields.iter().filter(|f| f.place.in_service())
    }
}

/// The five providers, in the one order they are asked.
pub fn describe() -> Vec<Descriptor> {
    use FieldKind::*;
    use Place::*;
    vec![
        Descriptor {
            name: crate::ssh_key::NAME,
            title: "SSH key",
            summary: "Your SSH key, so git in the shed reaches your repositories and can sign your commits.",
            lands: "~/.ssh/id_<type> (0600) and its .pub (0644); signing settings in ~/.config/git/config",
            fields: vec![
                Field::new("key_file", Path, Machine, "Key file",
                    "Your private key, with no passphrase; its public half is the file beside it ending .pub.")
                    .required(),
                Field::new("private_key", Secret, Service, "Private key",
                    "Made for you, or pasted: an OpenSSH, PEM or PKCS#8 key with no passphrase.")
                    .required(),
                Field::new("sign_commits", Switch, Both, "Sign commits",
                    "git in the shed signs your commits with this key.")
                    .default(FieldDefault::Switch(false)),
            ],
        },
        Descriptor {
            name: crate::github::NAME,
            title: "GitHub",
            summary: "Your GitHub sign-in, so gh and git in the shed act as you on github.com.",
            lands: "~/.config/gh/hosts.yml (0600); gh as git's credential helper in ~/.config/git/config",
            fields: vec![
                Field::new("gh", Text, Machine, "gh program",
                    "The gh this machine asks for your token and login: a name on your PATH (gh when unset) or an absolute path."),
                Field::new("token", Secret, Service, "Token",
                    "A fine-grained personal access token.")
                    .required(),
                Field::new("login", Text, Service, "Login",
                    "Your GitHub login: confirmed by GitHub when the token is saved, or typed.")
                    .required(),
            ],
        },
        Descriptor {
            name: crate::api_keys::NAME,
            title: "API keys",
            summary: "Keys for coding agents and tools: craze's providers.toml and environment variables.",
            lands: "~/.craze/native/providers.toml (0600); one environment variable each",
            fields: vec![
                Field::new("craze_providers", Switch, Machine, "craze's providers.toml",
                    "Deliver ~/.craze/native/providers.toml from this machine.")
                    .default(FieldDefault::Switch(true)),
                Field::new("env_files", List, Machine, "Env files",
                    "Files of KEY=VALUE lines on this machine, each readable by you only (0600)."),
                Field::new("env", List, Service, "Variables", "One environment variable each.")
                    .entry(
                        vec![
                            Field::new("variable", Text, Service, "Variable",
                                "Its name: upper-case letters, digits and underscores.")
                                .required(),
                            Field::new("value", Secret, Service, "Value", "Its value.").required(),
                        ],
                        true,
                    ),
            ],
        },
        Descriptor {
            name: crate::envsecrets::NAME,
            title: "envsecrets login",
            summary: "Your envsecrets config and its passphrase, so envsecrets works in the shed.",
            lands: "~/.envsecrets/config.yaml (0600); the passphrase as the variable its passphrase_env names",
            fields: vec![
                Field::new("passphrase_file", Path, Machine, "Passphrase file",
                    "A file holding the passphrase. Set this or a passphrase command."),
                Field::new("passphrase_command", List, Machine, "Passphrase command",
                    "A command that prints the passphrase: its program and arguments, run with no shell. Never shown back.")
                    .write_only(),
                Field::new("config", File, Service, "Config",
                    "Your ~/.envsecrets/config.yaml, with passphrase_env and gcs_credentials.")
                    .required(),
                Field::new("passphrase", Secret, Service, "Passphrase", "The config's passphrase.")
                    .required(),
            ],
        },
        Descriptor {
            name: crate::files::NAME,
            title: "Files",
            summary: "Files copied into the shed as they are.",
            lands: "Each file's target, under your home or the project directory",
            fields: vec![Field::new("file", List, Both, "Files",
                "On a machine, files on its disk; in a service, files you upload.")
                .entry(
                    vec![
                        Field::new("from", Path, Machine, "From", "The file on this machine.")
                            .required(),
                        Field::new("content", File, Service, "File", "The file's content.")
                            .required(),
                        Field::new("to", Text, Both, "To", "Where it goes, relative to its base.")
                            .required(),
                        Field::new("base", Text, Both, "Base",
                            "home, or project: inside the project directory, after the clone.")
                            .default(FieldDefault::Text("home"))
                            .choices(&["home", "project"]),
                        Field::new("mode", Text, Both, "Mode",
                            "0600 when unset; 0400 read-only; 0644 readable by all, in a service only.")
                            .choices(&["0600", "0400", "0644"]),
                    ],
                    true,
                )],
        },
    ]
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::{SettingValue, Settings};

    /// A settings file setting every machine field, each by its kind, is one
    /// the walk reads and the settings' table holds (a switch set away from
    /// its default); the walk allows no other key.
    #[test]
    fn every_machine_field_is_one_the_walk_reads() {
        fn on(f: &Field) -> bool {
            f.default != Some(FieldDefault::Switch(true))
        }
        fn sample(f: &Field) -> String {
            match f.kind {
                FieldKind::Switch => on(f).to_string(),
                FieldKind::Path => "\"~/x\"".into(),
                FieldKind::Text => format!("\"{}\"", f.choices.first().copied().unwrap_or("x")),
                FieldKind::List if f.entry.is_empty() => "[\"~/x\"]".into(),
                FieldKind::List => {
                    let parts: Vec<String> = f
                        .entry
                        .iter()
                        .filter(|e| e.place.on_machine())
                        .map(|e| format!("{} = {}", e.key, sample(e)))
                        .collect();
                    format!(
                        "[{{ {}, only = {{ servers = [\"x\"] }} }}]",
                        parts.join(", ")
                    )
                }
                FieldKind::Secret | FieldKind::File => unreachable!("not on a machine"),
            }
        }
        for d in describe() {
            for f in d.machine_fields() {
                // A passphrase file and a command are one or the other.
                let text = format!("[{}]\nenabled = true\n{} = {}\n", d.name, f.key, sample(f));
                let s = Settings::parse(&text).unwrap_or_else(|e| panic!("{text}: {e}"));
                let held = s.table(d.name).unwrap();
                match held.get(f.key) {
                    Some(SettingValue::Switch(b)) => assert_eq!(*b, on(f), "{text}"),
                    Some(_) => {}
                    None => panic!("{text}: the walk did not keep {}", f.key),
                }
            }
            // A key no descriptor names is refused.
            let text = format!("[{}]\nenabled = true\nnot_a_field = 1\n", d.name);
            assert!(Settings::parse(&text).is_err(), "{text}");
        }
    }

    #[test]
    fn descriptors_serialize_for_forms() {
        let v = serde_json::to_value(describe()).unwrap();
        assert_eq!(v[0]["name"], "ssh-key");
        assert_eq!(v[0]["fields"][0]["kind"], "path");
        assert_eq!(v[0]["fields"][0]["place"], "machine");
        assert_eq!(v[0]["fields"][2]["default"], false);
        assert_eq!(v[3]["fields"][1]["write_only"], true);
        assert_eq!(v[4]["fields"][0]["entry"][3]["default"], "home");
        assert!(v[1]["fields"][0].get("entry").is_none());
        // Every service field a person stores a secret in is a secret or a file.
        for d in describe() {
            for f in d.service_fields() {
                assert!(!f.title.is_empty() && !f.help.is_empty());
            }
        }
    }
}
