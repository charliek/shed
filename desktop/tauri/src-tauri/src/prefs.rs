//! The app's persisted preferences — currently the terminal preset + custom
//! template that `terminal.open` uses (the in-app Preferences view sets them, the
//! shed-card "Open in Terminal" button reads them). A small JSON file in the app
//! config dir; the mac app uses UserDefaults, this is the windowed-app analog.

use std::collections::HashMap;
use std::io::{self, Write};
use std::path::PathBuf;
use std::sync::{Arc, Mutex};

use serde::{Deserialize, Serialize};
use serde_json::Value;

use shed_core::terminal::TerminalPreset;

#[derive(Debug, Clone, Default, Serialize, Deserialize)]
pub struct Prefs {
    #[serde(default)]
    pub terminal_preset: TerminalPreset,
    /// Used only when `terminal_preset == Custom` (`{cmd}`/`{shed}` template).
    #[serde(default)]
    pub terminal_template: String,
    /// The SSH approval prefs, stored as their wire strings (`method`/`policy`/
    /// `ttl`). Kept as raw strings — not typed enums — so an old file (fields
    /// absent) or a value a downgraded build can't parse never fails the whole-file
    /// decode; the parse-with-fallback happens at hydration (see `lib.rs`).
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub ssh_method: Option<String>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub ssh_policy: Option<String>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub ssh_ttl: Option<String>,
    /// The AWS/Docker provider approval modes, keyed by the FULL namespace string
    /// (`aws-credentials`/`docker-credentials`) → the raw wire decision string
    /// (`approve`/`deny`). Raw strings (not typed enums), like the SSH fields, so an
    /// old file (absent) or a value a downgraded build can't parse never fails the
    /// whole-file decode; the parse-with-fallback-to-deny happens at hydration.
    #[serde(default)]
    pub provider_modes: HashMap<String, String>,
    /// The per-shed policy overrides (the Preferences "Per-shed overrides" list),
    /// stored as raw wire `PolicyRule` JSON. Untyped `Value`s so a rule a downgraded
    /// build can't parse is skipped at hydration rather than failing the whole file.
    #[serde(default)]
    pub extra_rules: Vec<Value>,
    /// Whether the app has applied its launch-at-login default once ([`apply_login_default`]).
    /// It records only that; the login item's own state lives in the OS.
    #[serde(default)]
    pub login_item_defaulted: bool,
}

/// Owns the prefs file path + the in-memory copy; writes through on every set.
pub struct PrefsStore {
    path: PathBuf,
    inner: Mutex<Prefs>,
}

impl PrefsStore {
    /// Load from `path` (a `prefs.json` in the app config dir), defaulting when
    /// absent or unreadable (a corrupt file is not fatal — the app still runs).
    pub fn load(path: PathBuf) -> Self {
        let inner = std::fs::read_to_string(&path)
            .ok()
            .and_then(|s| serde_json::from_str(&s).ok())
            .unwrap_or_default();
        Self {
            path,
            inner: Mutex::new(inner),
        }
    }

    pub fn get(&self) -> Prefs {
        self.inner.lock().map(|p| p.clone()).unwrap_or_default()
    }

    /// Set the terminal preset (+ optional custom template) and persist.
    pub fn set_terminal(&self, preset: TerminalPreset, template: Option<String>) {
        if let Ok(mut prefs) = self.inner.lock() {
            prefs.terminal_preset = preset;
            if let Some(template) = template {
                prefs.terminal_template = template;
            }
            self.save(&prefs);
        }
    }

    /// Persist the SSH approval prefs (method/policy/TTL wire strings), write-through
    /// like `set_terminal`. The caller sources these from the coordinator's current
    /// prefs, so the modal's partial updates compose into a complete snapshot.
    pub fn set_ssh(&self, method: String, policy: String, ttl: String) {
        if let Ok(mut prefs) = self.inner.lock() {
            prefs.ssh_method = Some(method);
            prefs.ssh_policy = Some(policy);
            prefs.ssh_ttl = Some(ttl);
            self.save(&prefs);
        }
    }

    /// Persist the AWS/Docker provider modes (full namespace → wire-decision map),
    /// write-through like `set_ssh`. The caller sources this from the coordinator's
    /// current modes, so it's a complete snapshot, not a partial merge.
    pub fn set_provider_modes(&self, modes: HashMap<String, String>) {
        if let Ok(mut prefs) = self.inner.lock() {
            prefs.provider_modes = modes;
            self.save(&prefs);
        }
    }

    /// Persist the per-shed override rules (raw wire `PolicyRule` JSON), write-through.
    /// The caller sources these from the coordinator's current shed rules.
    pub fn set_extra_rules(&self, rules: Vec<Value>) {
        if let Ok(mut prefs) = self.inner.lock() {
            prefs.extra_rules = rules;
            self.save(&prefs);
        }
    }

    /// Record that the launch-at-login default was applied, and persist it with
    /// [`save_checked`](Self::save_checked). On a failed save the in-memory marker is
    /// put back, so a later setter's save cannot record a default that was never applied.
    pub fn mark_login_item_defaulted(&self) -> io::Result<()> {
        let mut prefs = self
            .inner
            .lock()
            .map_err(|_| io::Error::other("prefs lock poisoned"))?;
        prefs.login_item_defaulted = true;
        let saved = self.save_checked(&prefs);
        if saved.is_err() {
            prefs.login_item_defaulted = false;
        }
        saved
    }

    /// Best-effort write-through for the setters: a failed write is dropped (the app
    /// keeps running on its in-memory prefs).
    fn save(&self, prefs: &Prefs) {
        let _ = self.save_checked(prefs);
    }

    /// Write `prefs` to the file, creating its directory, and report any failure.
    ///
    /// The bytes go to a file beside it, are synced, and are renamed over it, so a
    /// write that fails partway (a full disk) leaves the old file in place. A
    /// truncated `prefs.json` would load as defaults, and the launch-at-login default
    /// would then be applied a second time, against a person who had switched it off.
    pub fn save_checked(&self, prefs: &Prefs) -> io::Result<()> {
        if let Some(dir) = self.path.parent() {
            std::fs::create_dir_all(dir)?;
        }
        let json = serde_json::to_string_pretty(prefs).map_err(io::Error::other)?;
        let mut tmp = self.path.clone().into_os_string();
        tmp.push(".tmp");
        let tmp = PathBuf::from(tmp);
        let written = (|| {
            let mut file = std::fs::File::create(&tmp)?;
            file.write_all(json.as_bytes())?;
            file.sync_all()?;
            std::fs::rename(&tmp, &self.path)
        })();
        if written.is_err() {
            let _ = std::fs::remove_file(&tmp);
            return written;
        }
        // The rename is durable once its directory is synced; best effort, since not
        // every platform lets a directory be opened for that.
        if let Some(dir) = self.path.parent() {
            if let Ok(dir) = std::fs::File::open(dir) {
                let _ = dir.sync_all();
            }
        }
        Ok(())
    }
}

pub type SharedPrefs = Arc<PrefsStore>;

/// What [`apply_login_default`] did.
#[derive(Debug)]
pub enum LoginDefault {
    /// The marker was already set: the default was applied on an earlier launch.
    AlreadyApplied,
    /// The marker was saved and launch at login turned on.
    TurnedOn,
    /// The marker could not be saved, so launch at login was left alone.
    NotRecorded(io::Error),
    /// The marker was saved but the OS refused the login item; not retried.
    SetFailed(String),
}

/// Turn launch at login on once (§3.1.4). The marker is saved FIRST and the login
/// item is set only if that save succeeded: an unwritable prefs file would otherwise
/// turn launch at login on at every launch, including after the person switched it
/// off. `set` is the guarded login-item setter (`lib.rs:login_item_set`), a closure so
/// the order can be tested without touching the OS.
pub fn apply_login_default(
    store: &PrefsStore,
    set: impl FnOnce() -> Result<(), String>,
) -> LoginDefault {
    if store.get().login_item_defaulted {
        return LoginDefault::AlreadyApplied;
    }
    if let Err(e) = store.mark_login_item_defaulted() {
        return LoginDefault::NotRecorded(e);
    }
    match set() {
        Ok(()) => LoginDefault::TurnedOn,
        Err(e) => LoginDefault::SetFailed(e),
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::cell::Cell;

    /// A path whose parent is a regular file: every write under it fails, for root too
    /// (a read-only directory does not stop root, and the Docker legs run as root).
    fn unwritable(dir: &tempfile::TempDir) -> PathBuf {
        let blocker = dir.path().join("not-a-dir");
        std::fs::write(&blocker, "").unwrap();
        blocker.join("prefs.json")
    }

    fn on_disk(path: &std::path::Path) -> Prefs {
        serde_json::from_str(&std::fs::read_to_string(path).unwrap()).unwrap()
    }

    #[test]
    fn login_item_defaulted_is_false_when_absent_and_round_trips() {
        let dir = tempfile::tempdir().unwrap();
        let path = dir.path().join("prefs.json");
        // A 0.9.x file: no marker, and a stored broker mode the app no longer reads.
        std::fs::write(
            &path,
            r#"{"terminal_template":"x","broker_mode":"external"}"#,
        )
        .unwrap();
        let store = PrefsStore::load(path.clone());
        assert!(!store.get().login_item_defaulted);
        assert_eq!(store.get().terminal_template, "x");

        store.mark_login_item_defaulted().unwrap();
        let reloaded = PrefsStore::load(path.clone());
        assert!(reloaded.get().login_item_defaulted);
        assert_eq!(reloaded.get().terminal_template, "x");
        assert!(!std::fs::read_to_string(&path)
            .unwrap()
            .contains("broker_mode"));
    }

    #[test]
    fn a_save_that_fails_leaves_the_old_file_as_it_was() {
        let dir = tempfile::tempdir().unwrap();
        let path = dir.path().join("prefs.json");
        let store = PrefsStore::load(path.clone());
        store.mark_login_item_defaulted().unwrap();
        let before = std::fs::read(&path).unwrap();
        // The file the bytes are written to first cannot be created.
        std::fs::create_dir(dir.path().join("prefs.json.tmp")).unwrap();
        let mut changed = store.get();
        changed.login_item_defaulted = false;
        assert!(store.save_checked(&changed).is_err());
        assert_eq!(std::fs::read(&path).unwrap(), before);
        assert!(PrefsStore::load(path).get().login_item_defaulted);
    }

    #[test]
    fn save_checked_returns_the_error_save_drops() {
        let dir = tempfile::tempdir().unwrap();
        let store = PrefsStore::load(unwritable(&dir));
        let prefs = store.get();
        assert!(store.save_checked(&prefs).is_err());
        store.save(&prefs); // the best-effort path drops the same error
        store.set_terminal(TerminalPreset::default(), Some("y".into()));
        assert_eq!(store.get().terminal_template, "y");
    }

    #[test]
    fn writable_store_without_marker_sets_once_and_records_it() {
        let dir = tempfile::tempdir().unwrap();
        let path = dir.path().join("cfg").join("prefs.json");
        let store = PrefsStore::load(path.clone());
        let calls = Cell::new(0);
        let r = apply_login_default(&store, || {
            calls.set(calls.get() + 1);
            Ok(())
        });
        assert!(matches!(r, LoginDefault::TurnedOn), "{r:?}");
        assert_eq!(calls.get(), 1);
        assert!(on_disk(&path).login_item_defaulted);
    }

    #[test]
    fn marker_already_set_never_runs_the_setter() {
        let dir = tempfile::tempdir().unwrap();
        let path = dir.path().join("prefs.json");
        std::fs::write(&path, r#"{"login_item_defaulted":true}"#).unwrap();
        let store = PrefsStore::load(path);
        let calls = Cell::new(0);
        let r = apply_login_default(&store, || {
            calls.set(calls.get() + 1);
            Ok(())
        });
        assert!(matches!(r, LoginDefault::AlreadyApplied), "{r:?}");
        assert_eq!(calls.get(), 0);
    }

    #[test]
    fn unwritable_store_never_runs_the_setter() {
        let dir = tempfile::tempdir().unwrap();
        let store = PrefsStore::load(unwritable(&dir));
        let calls = Cell::new(0);
        for _ in 0..2 {
            let r = apply_login_default(&store, || {
                calls.set(calls.get() + 1);
                Ok(())
            });
            assert!(matches!(r, LoginDefault::NotRecorded(_)), "{r:?}");
        }
        assert_eq!(
            calls.get(),
            0,
            "launch at login turned on with no marker saved"
        );
        assert!(
            !store.get().login_item_defaulted,
            "the marker was kept in memory"
        );
    }

    #[test]
    fn failed_setter_keeps_the_marker_and_is_not_retried() {
        let dir = tempfile::tempdir().unwrap();
        let path = dir.path().join("prefs.json");
        let store = PrefsStore::load(path.clone());
        let calls = Cell::new(0);
        let r = apply_login_default(&store, || {
            calls.set(calls.get() + 1);
            Err("the OS said no".into())
        });
        assert!(
            matches!(r, LoginDefault::SetFailed(ref e) if e == "the OS said no"),
            "{r:?}"
        );
        assert!(on_disk(&path).login_item_defaulted);
        let r = apply_login_default(&store, || {
            calls.set(calls.get() + 1);
            Ok(())
        });
        assert!(matches!(r, LoginDefault::AlreadyApplied), "{r:?}");
        assert_eq!(calls.get(), 1);
    }
}
