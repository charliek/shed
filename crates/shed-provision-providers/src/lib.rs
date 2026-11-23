//! # shed-provision-providers
//!
//! The five delivery providers of plan 039 §3.8, and the `delivery.toml`
//! settings that choose them:
//!
//! | Provider | Reads | Items |
//! |---|---|---|
//! | [`ssh_key`] | `key_file` and its `.pub` | `.ssh/id_<type>` `0600`, `.ssh/id_<type>.pub` `0644`; with `sign_commits`, git config |
//! | [`github`] | `gh auth token`, `gh api user` | `.config/gh/hosts.yml` `0600`; git config making `gh` the credential helper |
//! | [`api_keys`] | `~/.craze/native/providers.toml`, `env_files` (`0600`) | `.craze/native/providers.toml` `0600`; one env item per variable |
//! | [`envsecrets`] | `~/.envsecrets/config.yaml`, `passphrase_file` or `passphrase_command` | `.envsecrets/config.yaml` `0600`; the passphrase as the env item `passphrase_env` names |
//! | [`files`] | `{from, to, base, mode, only}` entries | those files; `base = "project"` is stage 2 |
//!
//! A public key is `0644`; everything else is `0600` (or `0400`, a `files`
//! entry's choice). Providers read only through the
//! [`Reader`](shed_provision::Reader) they are given, run commands by argv
//! with no shell, and never put a value, a path from the disk or a
//! command's output in a problem message. The settings schema is in
//! [`settings`].
//!
//! Depends on `shed-provision` only (no other workspace crate), and has no
//! YAML parser.

mod common;

pub mod api_keys;
pub mod envsecrets;
pub mod files;
pub mod github;
pub mod settings;
pub mod ssh_key;

pub use settings::{settings_path, Settings, SettingsError, SETTINGS_FILE_NAME};

use shed_provision::{Built, FingerprintKey, Preview, Provider, Reader, Refusal, Request};

impl Settings {
    /// The enabled providers, in the one order they are asked: `ssh-key`,
    /// `github`, `api-keys`, `envsecrets-login`, `files`.
    pub fn providers(&self) -> Vec<Box<dyn Provider + Send + Sync>> {
        let mut out: Vec<Box<dyn Provider + Send + Sync>> = Vec::new();
        if let Some(s) = self.ssh_key.as_ref().filter(|s| s.enabled) {
            out.push(Box::new(ssh_key::SshKey::new(s.clone())));
        }
        if let Some(s) = self.github.as_ref().filter(|s| s.enabled) {
            out.push(Box::new(github::Github::new(s.clone())));
        }
        if let Some(s) = self.api_keys.as_ref().filter(|s| s.enabled) {
            out.push(Box::new(api_keys::ApiKeys::new(s.clone())));
        }
        if let Some(s) = self.envsecrets_login.as_ref().filter(|s| s.enabled) {
            out.push(Box::new(envsecrets::EnvsecretsLogin::new(s.clone())));
        }
        if let Some(s) = self.files.as_ref().filter(|s| s.enabled) {
            out.push(Box::new(files::Files::new(s.clone())));
        }
        out
    }
}

fn as_dyn(list: &[Box<dyn Provider + Send + Sync>]) -> Vec<&dyn Provider> {
    list.iter().map(|p| p.as_ref() as &dyn Provider).collect()
}

/// The answer to `request` from the server named `server`, built from
/// `settings`' enabled providers: see [`shed_provision::build_answer`].
pub fn build_answer(
    settings: &Settings,
    request: &Request,
    server: &str,
    reader: &dyn Reader,
    key: &FingerprintKey,
) -> Result<Built, Refusal> {
    let list = settings.providers();
    shed_provision::build_answer(request, server, &as_dyn(&list), reader, key)
}

/// What `settings` would deliver, redacted, with no request: see
/// [`shed_provision::preview`].
pub fn preview(settings: &Settings, reader: &dyn Reader, key: &FingerprintKey) -> Preview {
    let list = settings.providers();
    shed_provision::preview(&as_dyn(&list), reader, key)
}

#[cfg(test)]
mod tests;
