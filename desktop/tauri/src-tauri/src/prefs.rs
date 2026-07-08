//! The app's persisted preferences — currently the terminal preset + custom
//! template that `terminal.open` uses (the in-app Preferences view sets them, the
//! shed-card "Open in Terminal" button reads them). A small JSON file in the app
//! config dir; the mac app uses UserDefaults, this is the windowed-app analog.

use std::path::PathBuf;
use std::sync::{Arc, Mutex};

use serde::{Deserialize, Serialize};

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

    fn save(&self, prefs: &Prefs) {
        if let Some(dir) = self.path.parent() {
            let _ = std::fs::create_dir_all(dir);
        }
        if let Ok(json) = serde_json::to_string_pretty(prefs) {
            let _ = std::fs::write(&self.path, json);
        }
    }
}

pub type SharedPrefs = Arc<PrefsStore>;
