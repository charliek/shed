//! Shared UI state the React frontends report to Rust (the `ui_report` command),
//! exposed to the harness over IPC (`ui.current_pane` / `ui.computed_style` /
//! `dashboard.dump` / `tray.dump`).
//!
//! The truth op reads the RENDERED UI state, not a backend re-query: each webview
//! is the source of truth for what IT shows, and Rust relays what it reported.
//! Snapshots are keyed by the reporting WINDOW's label, so the other webviews (the
//! B1b menu-bar popover, label `popover`; the Preferences window, label
//! `preferences`) report under their own keys and can't clobber the dashboard's
//! `main` snapshot (`pane`/`sheds`/`refresh_token`). WITHIN
//! a window the object keys are merged, so a partial reporter (the Agents pane
//! publishing only `agents`) doesn't clobber that window's other keys.
//!
//! The dashboard's full reports are also counted into a small ring
//! ([`ReportRecord`]), so a harness can assert what EVERY report carried rather
//! than what it happened to sample between two of them (`ui.report_history`,
//! test mode only; plan 026 §3.4, shed#400).

use std::collections::{HashMap, VecDeque};
use std::sync::{Arc, Mutex};

use serde::Serialize;
use serde_json::Value;

/// How many of the dashboard's reports the history ring keeps. A render
/// reports at most once, so this is minutes of an idle shell and seconds of a
/// busy one — enough for a cell that clears it, does one thing, and reads it.
pub const REPORT_HISTORY_CAP: usize = 256;

/// What one full `main` report carried, reduced to the counts a seeded report
/// used to zero: the Hosts badge, the dashboard's rows, the sidebar's servers.
#[derive(Clone, Copy, Debug, PartialEq, Eq, Serialize)]
pub struct ReportRecord {
    pub hosts: u64,
    pub rows: usize,
    pub servers: usize,
}

impl ReportRecord {
    /// The record for a report, or `None` unless it is a FULL shell report —
    /// one carrying `badges.hosts`, `sheds[]` and `sidebar.servers[]`. A partial
    /// reporter (`agents`, `lane`, …) says nothing about those three and is not
    /// recorded.
    fn of(report: &Value) -> Option<Self> {
        Some(Self {
            hosts: report.get("badges")?.get("hosts")?.as_u64()?,
            rows: report.get("sheds")?.as_array()?.len(),
            servers: report.get("sidebar")?.get("servers")?.as_array()?.len(),
        })
    }
}

#[derive(Default)]
pub struct UiState {
    /// The last snapshot each window reported, keyed by the window label
    /// (`main` = the dashboard shell; `popover` = the mac menu-bar popover).
    snapshots: HashMap<String, Value>,
    /// The dashboard's last [`REPORT_HISTORY_CAP`] full reports, oldest first.
    history: VecDeque<ReportRecord>,
    /// Records the ring has evicted since it was last cleared — nonzero means
    /// the history no longer starts where the reader cleared it.
    dropped: u64,
}

impl UiState {
    /// Merge a window's reported snapshot into its OWN slot — object keys are merged
    /// (a partial report doesn't clobber that window's other keys, and the shell
    /// re-sends its keys every render, so nothing goes stale). A non-object report
    /// replaces the slot wholesale.
    pub fn merge(&mut self, label: &str, incoming: Value) {
        if label == "main" {
            if let Some(record) = ReportRecord::of(&incoming) {
                if self.history.len() == REPORT_HISTORY_CAP {
                    self.history.pop_front();
                    self.dropped += 1;
                }
                self.history.push_back(record);
            }
        }
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

    /// The dashboard's recorded reports, oldest first, and how many the ring
    /// evicted since the last clear. `clear` empties it after the read, so the
    /// next read holds only reports sent after this one.
    pub fn report_history(&mut self, clear: bool) -> (Vec<ReportRecord>, u64) {
        let out = (self.history.iter().copied().collect(), self.dropped);
        if clear {
            self.history.clear();
            self.dropped = 0;
        }
        out
    }
}

/// Shared between the `ui_report` Tauri command (writer) and the IPC handler
/// (reader), so both see the same rendered state.
pub type SharedUi = Arc<Mutex<UiState>>;

#[cfg(test)]
mod tests {
    use super::*;
    use serde_json::json;

    fn full(hosts: u64, rows: usize, servers: usize) -> Value {
        json!({
            "pane": "sheds",
            "badges": { "sheds": rows, "agents": 0, "hosts": hosts, "pending": 0 },
            "sheds": vec![json!({ "name": "s" }); rows],
            "sidebar": { "servers": vec![json!({ "name": "h" }); servers], "machines": [] },
            "refresh_token": 0,
        })
    }

    /// **Only a full `main` report is recorded** — a partial reporter's keys
    /// and another window's report say nothing about the shell's badge.
    #[test]
    fn report_history_records_only_full_main_reports() {
        let mut ui = UiState::default();
        ui.merge("main", full(2, 1, 2));
        // Partial reports: no badges, no sheds, no sidebar servers.
        ui.merge("main", json!({ "agents": [], "agents_empty": null }));
        ui.merge("main", json!({ "badges": { "hosts": 0 }, "sheds": [] }));
        ui.merge(
            "main",
            json!({ "badges": { "hosts": 0 }, "sidebar": { "servers": [] } }),
        );
        ui.merge("main", json!({ "sheds": [], "sidebar": { "servers": [] } }));
        // Ill-typed: a hosts count that is not a number is not a record.
        ui.merge(
            "main",
            json!({ "badges": { "hosts": "2" }, "sheds": [], "sidebar": { "servers": [] } }),
        );
        // Another window, full-shaped or not.
        ui.merge("preferences", full(0, 0, 0));
        ui.merge("popover", full(0, 0, 0));
        // A non-object report replaces the slot but is not a record either.
        ui.merge("main", json!(null));
        ui.merge("main", full(0, 0, 0));

        let (reports, dropped) = ui.report_history(false);
        assert_eq!(
            reports,
            vec![
                ReportRecord {
                    hosts: 2,
                    rows: 1,
                    servers: 2
                },
                ReportRecord {
                    hosts: 0,
                    rows: 0,
                    servers: 0
                },
            ]
        );
        assert_eq!(dropped, 0);
        // A read without `clear` leaves it as it was.
        assert_eq!(ui.report_history(false).0.len(), 2);
    }

    /// **The ring is bounded**: past [`REPORT_HISTORY_CAP`] the oldest go first
    /// and are counted, and a clear starts it — and the count — afresh.
    #[test]
    fn report_history_is_bounded_and_counts_what_it_drops() {
        let mut ui = UiState::default();
        let extra = 40;
        for i in 0..(REPORT_HISTORY_CAP + extra) {
            ui.merge("main", full(i as u64, 0, 0));
        }
        let (reports, dropped) = ui.report_history(true);
        assert_eq!(reports.len(), REPORT_HISTORY_CAP);
        assert_eq!(dropped, extra as u64);
        assert_eq!(
            reports[0].hosts, extra as u64,
            "the oldest must be evicted first"
        );
        assert_eq!(
            reports[REPORT_HISTORY_CAP - 1].hosts,
            (REPORT_HISTORY_CAP + extra - 1) as u64
        );

        let (after_clear, dropped) = ui.report_history(false);
        assert!(after_clear.is_empty(), "clear must empty the ring");
        assert_eq!(dropped, 0, "clear must reset the evicted count");
        ui.merge("main", full(2, 3, 2));
        assert_eq!(
            ui.report_history(false).0,
            vec![ReportRecord {
                hosts: 2,
                rows: 3,
                servers: 2
            }]
        );
        // Recording never changes what the slot itself holds.
        assert_eq!(ui.get("main", "badges").unwrap()["hosts"], 2);
    }
}
