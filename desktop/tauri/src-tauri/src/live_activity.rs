//! **Live activity for SHED sessions.**
//!
//! A machine session arrives with its activity already on it, because the app
//! reads that machine's hub directly (`/v1/sessions`) and activity is something
//! the hub knows. A SHED session does not: the app lists sheds by running the
//! one-shot `shed-ext-rc list` over ssh, and activity is a **hub-layer
//! overlay** that the one-shot deliberately never sets —
//! `internal/ext/rc/hub.go` says so outright ("the one-shot List above never
//! sets it").
//!
//! So without this module the desktop shows every shed session as having no
//! activity at all, while the machine beside it reports `needs input`. Same
//! app, same list, two different amounts of truth — an artifact of how each was
//! fetched, which is exactly the kind of thing a user should never be able to
//! see.
//!
//! The missing piece is the host's aggregate SSE stream, `GET /api/rc/events`,
//! which is what shed-mobile has always subscribed to. [`RcEventsWatcher`]
//! already drives it and folds each event into an [`ActivityOverlay`] — it was
//! built as the deliberate twin of the machine watcher. This module is only the
//! wiring: one watcher per configured host, the overlays held together, and a
//! lookup the session payload applies as it is built.
//!
//! Deliberately shaped like [`crate::machines`], down to the `on_change`
//! callback, because they are the same problem twice — a per-target reconnect
//! loop whose output the UI renders. Divergences are noted where they occur.

use std::collections::BTreeMap;
use std::sync::{Arc, Mutex};

use serde_json::Value;
use shed_app::{Backend, RcEventsWatcher, RcWatcherUpdate};
use shed_core::rc_events::ActivityOverlay;

use crate::machines::OnChange;

/// One watcher per host, and the overlay each has folded so far.
pub struct LiveActivityLayer {
    /// Keyed by SERVER NAME — the same key `RcSession.host` carries, so a
    /// lookup needs no translation.
    overlays: Arc<Mutex<BTreeMap<String, ActivityOverlay>>>,
    /// Held so the watchers live as long as the app does; dropping one ends its
    /// reconnect loop.
    _watchers: Vec<RcEventsWatcher>,
}

impl LiveActivityLayer {
    /// Subscribe to every configured host's event stream.
    ///
    /// Never fails: a host that cannot be reached simply never produces an
    /// overlay, and its sessions render exactly as they do today. That is the
    /// whole failure mode — this layer only ever ADDS information, so nothing
    /// it does can make the list worse than it was before it existed.
    pub fn start(
        handle: &tokio::runtime::Handle,
        backend: &Backend,
        on_change: OnChange,
    ) -> LiveActivityLayer {
        let overlays = Arc::new(Mutex::new(BTreeMap::new()));
        let mut watchers = Vec::new();

        for (name, client) in backend.host_clients() {
            let (watcher, mut rx) = RcEventsWatcher::spawn(handle, client, name.clone());
            watchers.push(watcher);

            let overlays = Arc::clone(&overlays);
            let on_change = on_change.clone();
            handle.spawn(async move {
                while let Some(update) = rx.recv().await {
                    match update {
                        // The overlay AFTER the fold — the watcher owns the
                        // folding, this layer only holds the result.
                        RcWatcherUpdate::Event { overlay, .. } => {
                            overlays
                                .lock()
                                .unwrap_or_else(|e| e.into_inner())
                                .insert(name.clone(), overlay);
                            on_change();
                        }
                        // A reconnect cleared the held overlay: drop ours too,
                        // so a stale badge cannot outlive the connection that
                        // justified it. The rows fall back to their base
                        // snapshot, which is the honest thing to show.
                        RcWatcherUpdate::Resynced => {
                            overlays
                                .lock()
                                .unwrap_or_else(|e| e.into_inner())
                                .remove(&name);
                            on_change();
                        }
                        // A disconnect does NOT clear: the last snapshot stays
                        // on screen until the reconnect replaces it, matching
                        // how the machine layer treats a blip.
                        RcWatcherUpdate::Down { .. } => {}
                    }
                }
            });
        }

        Self::seed(handle, backend, Arc::clone(&overlays), on_change);

        LiveActivityLayer {
            overlays,
            _watchers: watchers,
        }
    }

    /// Seed each host's overlay from a SNAPSHOT, once, at startup.
    ///
    /// The event stream is a CHANGE feed: it says what moved, not what is. A
    /// session that has been sitting at `needs input` since before the app
    /// launched generates no event, so a stream-only layer shows nothing until
    /// something happens — which for an idle machine could be never.
    ///
    /// `GET /api/overview` is the snapshot with the same dimension on it: the
    /// server consults the shed's hub while enriching (`rcenrich.go`, a 200 ms
    /// best-effort dial), so its rows carry activity where the one-shot ssh
    /// listing cannot. Seeding from it and then folding events on top gives the
    /// correct value immediately AND keeps it correct.
    ///
    /// Best-effort by construction: a host that does not answer, or answers
    /// without the dimension, simply leaves its rows as the base listing had
    /// them.
    fn seed(
        handle: &tokio::runtime::Handle,
        backend: &Backend,
        overlays: Arc<Mutex<BTreeMap<String, ActivityOverlay>>>,
        on_change: OnChange,
    ) {
        for (name, client) in backend.host_clients() {
            let overlays = Arc::clone(&overlays);
            let on_change = on_change.clone();
            handle.spawn(async move {
                let Ok(overview) = client.overview().await else {
                    return;
                };
                let mut seeded = ActivityOverlay::empty();
                for shed in &overview.sheds {
                    for s in &shed.sessions {
                        let Some(activity) = s.activity else { continue };
                        seeded = seeded.apply(&shed_core::rc_events::RcEvent::ActivityChanged {
                            shed: shed.shed.name.clone(),
                            slug: s.slug.clone(),
                            activity: Some(activity),
                            activity_at: s.activity_at.clone(),
                            state: Some(s.state),
                            last_message: s.last_message.clone(),
                        });
                    }
                }
                if seeded.is_empty() {
                    return;
                }
                {
                    let mut guard = overlays.lock().unwrap_or_else(|e| e.into_inner());
                    // Only if the stream has not already spoken for this host: a
                    // live fold is newer than a startup snapshot, and a seed
                    // landing late must not undo it.
                    guard.entry(name).or_insert(seeded);
                }
                on_change();
            });
        }
    }

    /// An empty layer, for tests. Production never needs one: `start` with no
    /// hosts already produces exactly this.
    #[cfg(test)]
    pub fn inert() -> LiveActivityLayer {
        LiveActivityLayer {
            overlays: Arc::new(Mutex::new(BTreeMap::new())),
            _watchers: Vec::new(),
        }
    }

    /// Fold the live activity for `(host, shed, slug)` onto a session row.
    ///
    /// Only ever ADDS: a key the overlay has no opinion about is left exactly as
    /// the base snapshot had it. So a host with no stream, or a session the
    /// stream has not mentioned, renders as it always did.
    pub fn apply(&self, row: &mut Value, host: &str, shed: &str, slug: &str) {
        let guard = self.overlays.lock().unwrap_or_else(|e| e.into_inner());
        let Some(patch) = guard.get(host).and_then(|o| o.lookup(shed, slug)) else {
            return;
        };
        let Some(obj) = row.as_object_mut() else { return };
        if let Some(a) = patch.activity {
            obj.insert("activity".into(), Value::String(a.as_str().to_string()));
        }
        if let Some(s) = patch.state {
            obj.insert("state".into(), Value::String(s.as_str().to_string()));
        }
        if let Some(m) = patch.last_message.as_deref() {
            obj.insert("last_message".into(), Value::String(m.to_string()));
        }
        if let Some(seq) = patch.last_seq {
            obj.insert("last_seq".into(), Value::from(seq));
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use serde_json::json;
    use shed_core::rc::RcActivity;
    use shed_core::rc_events::RcEvent;

    fn layer_with(host: &str, overlay: ActivityOverlay) -> LiveActivityLayer {
        let mut map = BTreeMap::new();
        map.insert(host.to_string(), overlay);
        LiveActivityLayer {
            overlays: Arc::new(Mutex::new(map)),
            _watchers: Vec::new(),
        }
    }

    fn overlay_with(shed: &str, slug: &str, activity: RcActivity) -> ActivityOverlay {
        ActivityOverlay::empty().apply(&RcEvent::ActivityChanged {
            shed: shed.to_string(),
            slug: slug.to_string(),
            activity: Some(activity),
            activity_at: None,
            state: None,
            last_message: None,
        })
    }

    /// The gap this module closes: a shed session listed by the one-shot has no
    /// activity, and the stream is the only thing that can supply it.
    #[test]
    fn a_streamed_activity_lands_on_the_row() {
        let layer = layer_with("mac-mini", overlay_with("prox", "abc123", RcActivity::NeedsInput));
        let mut row = json!({"slug": "abc123", "state": "ready"});
        layer.apply(&mut row, "mac-mini", "prox", "abc123");
        assert_eq!(row["activity"], json!("needs_input"));
    }

    /// Additive, never subtractive: an untouched row is byte-identical, so a
    /// host with no stream is exactly as good as before this existed.
    #[test]
    fn a_row_the_overlay_has_no_opinion_about_is_untouched() {
        let layer = layer_with("mac-mini", overlay_with("prox", "abc123", RcActivity::Working));
        let before = json!({"slug": "other", "state": "ready", "activity": "idle"});
        let mut row = before.clone();
        // Same host and shed, different slug.
        layer.apply(&mut row, "mac-mini", "prox", "other");
        assert_eq!(row, before);
        // …and an unknown host likewise.
        let mut row2 = before.clone();
        layer.apply(&mut row2, "elsewhere", "prox", "abc123");
        assert_eq!(row2, before);
    }

    /// Keyed by `(shed, slug)`, so two sheds on one host that happen to share a
    /// slug do not read each other's activity.
    #[test]
    fn two_sheds_sharing_a_slug_do_not_collide() {
        let layer = layer_with("mac-mini", overlay_with("a", "same", RcActivity::NeedsApproval));
        let mut mine = json!({"slug": "same"});
        layer.apply(&mut mine, "mac-mini", "a", "same");
        assert_eq!(mine["activity"], json!("needs_approval"));

        let mut theirs = json!({"slug": "same"});
        layer.apply(&mut theirs, "mac-mini", "b", "same");
        assert_eq!(theirs.get("activity"), None, "shed b read shed a's activity");
    }

    #[test]
    fn an_inert_layer_changes_nothing() {
        let before = json!({"slug": "abc123", "state": "ready"});
        let mut row = before.clone();
        LiveActivityLayer::inert().apply(&mut row, "mac-mini", "prox", "abc123");
        assert_eq!(row, before);
    }
}
