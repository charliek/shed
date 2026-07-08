//! Shared UI state the React frontends report to Rust (the `ui_report` command),
//! exposed to the harness over IPC (`ui.current_pane` / `ui.computed_style` /
//! `dashboard.dump` / `tray.dump`).
//!
//! The truth op reads the RENDERED UI state, not a backend re-query: each webview
//! is the source of truth for what IT shows, and Rust relays what it reported.
//! Snapshots are keyed by the reporting WINDOW's label, so a second webview (the
//! B1b menu-bar popover, label `popover`) reports under its own key and can't
//! clobber the dashboard's `main` snapshot (`pane`/`sheds`/`refresh_token`). WITHIN
//! a window the object keys are merged, so a partial reporter (the Agents pane
//! publishing only `agents`) doesn't clobber that window's other keys.

use std::collections::HashMap;
use std::sync::{Arc, Mutex};

use serde_json::Value;

#[derive(Default)]
pub struct UiState {
    /// The last snapshot each window reported, keyed by the window label
    /// (`main` = the dashboard shell; `popover` = the mac menu-bar popover).
    snapshots: HashMap<String, Value>,
}

impl UiState {
    /// Merge a window's reported snapshot into its OWN slot — object keys are merged
    /// (a partial report doesn't clobber that window's other keys, and the shell
    /// re-sends its keys every render, so nothing goes stale). A non-object report
    /// replaces the slot wholesale.
    pub fn merge(&mut self, label: &str, incoming: Value) {
        let slot = self
            .snapshots
            .entry(label.to_string())
            .or_insert_with(|| Value::Object(Default::default()));
        match (slot, incoming) {
            (Value::Object(existing), Value::Object(incoming)) => {
                existing.extend(incoming);
            }
            (slot, incoming) => *slot = incoming,
        }
    }

    /// Project a key out of a window's reported snapshot (`pane`, `style`, `sheds`,
    /// ...), or `None` if that window hasn't reported / the key is absent.
    pub fn get(&self, label: &str, key: &str) -> Option<Value> {
        self.snapshots.get(label).and_then(|s| s.get(key).cloned())
    }

    /// Whether a window has reported at least once (⟹ its listeners are live — the
    /// readiness invariant `navigate`/`sheds.refresh` rely on).
    pub fn has(&self, label: &str) -> bool {
        self.snapshots.contains_key(label)
    }

    /// The refresh token a window last echoed (0 if it hasn't reported one), so a
    /// synchronous `sheds.refresh` can wait until the frontend re-fetched +
    /// re-reported — mirroring gtk's blocking glib re-render.
    pub fn refresh_token(&self, label: &str) -> u64 {
        self.get(label, "refresh_token")
            .and_then(|v| v.as_u64())
            .unwrap_or(0)
    }
}

/// Shared between the `ui_report` Tauri command (writer) and the IPC handler
/// (reader), so both see the same rendered state.
pub type SharedUi = Arc<Mutex<UiState>>;
