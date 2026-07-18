//! rc-events decode + activity fold — the pure half of the live-activity layer.
//!
//! The server-side aggregate rc activity stream (`GET /api/rc/events`, SSE):
//! one stream a client subscribes to for live rc activity across every shed on
//! a host. Each upstream hub envelope has its `shed` field server-filled by the
//! aggregator's frame rewrite, and the synthetic `hub.unavailable` /
//! `shed.stopped` events are server-minted, outside the forwardable allowlist,
//! so a guest hub can't spoof them (`internal/api/rcevents.go:454-503`). The
//! hub-side payload shapes are `internal/ext/rc/hub_events.go:69-95`. Ported
//! from mobile's `rc_events.dart` (decode `:109-158`, fold `:185-277`).
//!
//! SSE here is best-effort notification, not durable delivery: on reconnect the
//! client refetches snapshots (overview / messages) rather than replaying.
//!
//! The forwarded payload fields (slug/activity/seq/…) are guest-controlled and
//! treated as untrusted; the decode is tolerant and NEVER errors — a malformed
//! frame is dropped ([`parse_rc_event`] → `None`) or its wrong-typed fields are
//! nulled, because a decode failure that tore down the stream would turn one
//! bad frame into a perpetual reconnect storm. The HTTP half (the
//! `Client::rc_events` SSE connection) lives in [`crate::http`]; the
//! reconnect-and-refold watcher lives in `shed-app`.

use std::collections::HashMap;

use serde_json::Value;

use crate::models::{clean_display, opt_trimmed};
use crate::rc::{RcActivity, RcState};
use crate::sse::SseEvent;

/// A parsed rc event (`rc_events.dart:18-104`). The `shed` a variant carries is
/// server-corrected; everything else is guest-controlled display data.
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum RcEvent {
    /// `event: activity.changed` — a session's live activity (and lifecycle
    /// state) moved. `data: {shed, slug, activity, activity_at, state}`, plus
    /// an optional `last_message` (the sanitized preview that rides with the
    /// activity dimension) when the hub includes it — decoded tolerantly so a
    /// card's subtitle can patch live alongside the badge instead of waiting
    /// for an overview refetch.
    ActivityChanged {
        shed: String,
        slug: String,
        activity: Option<RcActivity>,
        activity_at: Option<String>,
        state: Option<RcState>,
        last_message: Option<String>,
    },
    /// `event: session.updated` — a session was created/killed or its
    /// lifecycle changed. `data: {shed, slug, session}`. The nested `session`
    /// is the display subset; the activity/state/last_message dimensions are
    /// extracted from it. An absent/null `session` means the session is GONE
    /// (a kill): `removed` is set and the overlay drops its patch entirely
    /// instead of merging stale fields.
    SessionUpdated {
        shed: String,
        slug: String,
        activity: Option<RcActivity>,
        state: Option<RcState>,
        last_message: Option<String>,
        removed: bool,
    },
    /// `event: message.appended` — a new feed message landed. `data: {shed,
    /// slug, seq}`. Notification only: the body comes from a targeted
    /// /messages fetch, so fan-out stays tiny and drop-safe.
    MessageAppended {
        shed: String,
        slug: String,
        seq: u64,
    },
    /// `event: hub.unavailable` — a shed's upstream hub connection dropped;
    /// that host's live activity is stale until it reconnects.
    HubUnavailable { shed: String },
    /// `event: shed.stopped` — a shed left candidacy (stopped/deleted); its
    /// reader tore down.
    ShedStopped { shed: String },
}

impl RcEvent {
    /// The host (shed name) an event pertains to — server-filled on every event.
    pub fn shed(&self) -> &str {
        match self {
            RcEvent::ActivityChanged { shed, .. }
            | RcEvent::SessionUpdated { shed, .. }
            | RcEvent::MessageAppended { shed, .. }
            | RcEvent::HubUnavailable { shed }
            | RcEvent::ShedStopped { shed } => shed,
        }
    }
}

/// Decode one SSE record into a typed [`RcEvent`], or `None` for an unknown
/// event name, a non-object data payload, or a payload missing its required
/// keys (so a malformed frame is dropped, never rendered). Mirrors
/// `parseRcEvent` (`rc_events.dart:109-158`): tolerant reads throughout — a
/// wrong-typed optional field is nulled, never an error, because a throw here
/// would kill the SSE stream and turn one bad guest frame into a reconnect
/// storm.
pub fn parse_rc_event(e: &SseEvent) -> Option<RcEvent> {
    let data: Value = serde_json::from_str(&e.data).ok()?;
    let data = data.as_object()?;
    let shed = opt_trimmed(data.get("shed"));
    let slug = opt_trimmed(data.get("slug"));
    match e.event.as_str() {
        "activity.changed" => Some(RcEvent::ActivityChanged {
            shed: shed?,
            slug: slug?,
            activity: activity_of(data.get("activity")),
            activity_at: opt_trimmed(data.get("activity_at")),
            state: state_of(data.get("state")),
            last_message: clean_display(data.get("last_message")),
        }),
        "session.updated" => {
            // No session body (absent, null, or non-object) = the session is
            // gone (killed): signal removal so the overlay drops the patch
            // instead of retaining every stale field.
            let sess = data.get("session").and_then(Value::as_object);
            Some(RcEvent::SessionUpdated {
                shed: shed?,
                slug: slug?,
                activity: sess.and_then(|s| activity_of(s.get("activity"))),
                state: sess.and_then(|s| state_of(s.get("state"))),
                last_message: sess.and_then(|s| clean_display(s.get("last_message"))),
                removed: sess.is_none(),
            })
        }
        "message.appended" => {
            let (shed, slug) = (shed?, slug?);
            let seq = seq_of(data.get("seq")?)?;
            Some(RcEvent::MessageAppended { shed, slug, seq })
        }
        "hub.unavailable" => Some(RcEvent::HubUnavailable { shed: shed? }),
        "shed.stopped" => Some(RcEvent::ShedStopped { shed: shed? }),
        _ => None,
    }
}

/// Dart `seq is! num → drop; seq.toInt()` (`rc_events.dart:144-148`): any
/// JSON number is accepted and truncated toward zero, so a fractional seq
/// keeps the event (`42.9` → `42`) exactly as Dart's `toInt()` does; a
/// non-number drops the event in both. Two narrowings on top of Dart, per the
/// wire's actual type (uint64 — `messageAppendedData.Seq`,
/// `hub_events.go:94`): a negative seq is dropped (Dart's `toInt()` would
/// keep it, but a negative cursor is meaningless — same disposition as the C2
/// feed decoder's non-negative `seq`, `rc.rs::feed_u64`), and a value beyond
/// u64 range is dropped rather than wrapped or saturated.
fn seq_of(v: &Value) -> Option<u64> {
    if let Some(u) = v.as_u64() {
        return Some(u);
    }
    // Not directly a u64: a float, a negative, or not a number at all
    // (`as_f64` → None drops the event, Dart's `is! num`).
    let f = v.as_f64()?;
    // u64::MAX as f64 is exactly 2^64; anything strictly below it truncates
    // into range. serde_json numbers are always finite, but stay explicit.
    if f.is_finite() && f >= 0.0 && f < u64::MAX as f64 {
        Some(f.trunc() as u64)
    } else {
        None
    }
}

/// Dart `RcActivity.fromWire(_str(v))` (`rc_events.dart:124`): non-string /
/// blank → `None` ("no activity dimension at all"); any non-blank string
/// decodes via [`RcActivity::from_wire`]'s unknown→`Unknown` path.
fn activity_of(v: Option<&Value>) -> Option<RcActivity> {
    opt_trimmed(v).map(|s| RcActivity::from_wire(&s))
}

/// Dart `_state` (`rc_events.dart:279-280`): a non-empty string decodes via
/// [`RcState::from_wire`] (whose unknown values map to `Starting`); anything
/// else — absent, null, wrong-typed, empty — is `None`. Deliberately an
/// UNtrimmed non-empty check (unlike the `_str`-based fields), matching the
/// Dart exactly: a whitespace-only state is "non-empty" and decodes to
/// `Starting` through the unknown-value path.
fn state_of(v: Option<&Value>) -> Option<RcState> {
    v.and_then(Value::as_str)
        .filter(|s| !s.is_empty())
        .map(RcState::from_wire)
}

/// The live overlay a session card merges over its base overview snapshot: the
/// last activity/state/message the SSE stream reported, plus the latest feed
/// seq (which the watch view watches to trigger a targeted /messages fetch).
/// Absent fields fall through to the base session. Mirrors mobile's
/// `LiveActivity` (`rc_events.dart:167-178`).
#[derive(Debug, Clone, PartialEq, Eq, Default)]
pub struct LiveActivity {
    pub activity: Option<RcActivity>,
    pub state: Option<RcState>,
    pub last_message: Option<String>,
    pub last_seq: Option<u64>,
}

/// A host's live-activity overlay: a map of `(shed, slug)` → [`LiveActivity`],
/// folded from the SSE event stream. Immutable semantics — [`apply`] returns a
/// new overlay (clone-on-write of the map). Degrading events (hub.unavailable /
/// shed.stopped) drop that shed's patches so cards fall back to the last
/// overview snapshot rather than showing stale live badges. Mirrors mobile's
/// `ActivityOverlay` (`rc_events.dart:185-277`) with one deliberate
/// non-port: Dart returns the SAME object (`identical`) when a drop matches
/// nothing — a Riverpod rebuild optimization, not part of the fold contract.
/// Here only the *contents* contract is ported; a no-op apply returns an
/// equal-contents overlay, not necessarily a shared allocation.
///
/// [`apply`]: ActivityOverlay::apply
#[derive(Debug, Clone, PartialEq, Eq, Default)]
pub struct ActivityOverlay {
    patches: HashMap<(String, String), LiveActivity>,
}

impl ActivityOverlay {
    /// The empty overlay (Dart's `ActivityOverlay.empty`).
    pub fn empty() -> ActivityOverlay {
        ActivityOverlay::default()
    }

    pub fn is_empty(&self) -> bool {
        self.patches.is_empty()
    }

    pub fn len(&self) -> usize {
        self.patches.len()
    }

    /// The live patch for a session, keyed by `(shed, slug)`.
    pub fn lookup(&self, shed: &str, slug: &str) -> Option<&LiveActivity> {
        self.patches.get(&(shed.to_string(), slug.to_string()))
    }

    /// Fold one event into the overlay, returning the next overlay
    /// (`rc_events.dart:196-248`):
    ///
    /// * `ActivityChanged` — the payload's activity REPLACES the held one (the
    ///   hub always carries the current activity on this event); state and
    ///   last_message fall back to the held values when absent (a frame
    ///   without a preview must not wipe the held one); `last_seq` is kept.
    ///   The merged patch then passes through the suppression rule.
    /// * `SessionUpdated` with `removed` — a kill drops the patch entirely
    ///   (merging over a gone session would keep stale live fields alive
    ///   forever); otherwise every dimension falls back to the held value,
    ///   then suppression.
    /// * `MessageAppended` — bumps `last_seq` only, preserving the other held
    ///   dimensions; deliberately NOT suppressed (the feed history stays
    ///   readable on a blocked/dead session).
    /// * `HubUnavailable` / `ShedStopped` — drops every patch for that shed so
    ///   cards fall back to the base snapshot.
    pub fn apply(&self, ev: &RcEvent) -> ActivityOverlay {
        match ev {
            RcEvent::ActivityChanged {
                shed,
                slug,
                activity,
                state,
                last_message,
                ..
            } => {
                let key = (shed.clone(), slug.clone());
                let prev = self.patches.get(&key);
                // Activity REPLACES (the hub always carries the current one).
                let next = suppressed(merge_over(prev, *activity, *state, last_message));
                self.with(key, next)
            }
            RcEvent::SessionUpdated {
                shed,
                slug,
                activity,
                state,
                last_message,
                removed,
            } => {
                let key = (shed.clone(), slug.clone());
                // A kill (no session body) removes the patch entirely —
                // merging over a gone session would keep stale live fields
                // alive forever.
                if *removed {
                    return self.drop_key(&key);
                }
                let prev = self.patches.get(&key);
                // Activity falls back to the held value when absent.
                let activity = activity.or_else(|| prev.and_then(|p| p.activity));
                let next = suppressed(merge_over(prev, activity, *state, last_message));
                self.with(key, next)
            }
            RcEvent::MessageAppended { shed, slug, seq } => {
                let key = (shed.clone(), slug.clone());
                // Bump last_seq only; every other held dimension is preserved.
                let mut next = self.patches.get(&key).cloned().unwrap_or_default();
                next.last_seq = Some(*seq);
                self.with(key, next)
            }
            RcEvent::HubUnavailable { shed } | RcEvent::ShedStopped { shed } => {
                self.drop_shed(shed)
            }
        }
    }

    fn with(&self, key: (String, String), v: LiveActivity) -> ActivityOverlay {
        let mut patches = self.patches.clone();
        patches.insert(key, v);
        ActivityOverlay { patches }
    }

    fn drop_key(&self, key: &(String, String)) -> ActivityOverlay {
        let mut patches = self.patches.clone();
        patches.remove(key);
        ActivityOverlay { patches }
    }

    fn drop_shed(&self, shed: &str) -> ActivityOverlay {
        let mut patches = self.patches.clone();
        patches.retain(|(s, _), _| s != shed);
        ActivityOverlay { patches }
    }
}

/// The shared merge half of the `ActivityChanged` / `SessionUpdated` fold
/// arms: `state` falls back to the held value when absent, a payload-carried
/// `last_message` supersedes the held preview (this is what lets a card's
/// subtitle update live with the badge) but an absent one must not wipe it,
/// and `last_seq` is always the held one (only `MessageAppended` moves it).
/// The two events differ only in their activity policy — replace vs fall
/// back — so the caller passes the already-resolved activity.
fn merge_over(
    prev: Option<&LiveActivity>,
    activity: Option<RcActivity>,
    state: Option<RcState>,
    last_message: &Option<String>,
) -> LiveActivity {
    LiveActivity {
        activity,
        state: state.or_else(|| prev.and_then(|p| p.state)),
        last_message: last_message
            .clone()
            .or_else(|| prev.and_then(|p| p.last_message.clone())),
        last_seq: prev.and_then(|p| p.last_seq),
    }
}

/// The whole-dimension suppression rule, mirroring the Go server's
/// `DisplayActivity` + `toSessionRC` and mobile's `_suppressed`
/// (`rc_events.dart:255-259`): a blocking lifecycle state
/// (needs-trust/needs-auth/dead — [`RcState::permits_activity`]) drops
/// activity AND last_message — a stale last_message on a dead/gated row would
/// present pre-death context as current. The state itself (and `last_seq`) is
/// retained.
fn suppressed(v: LiveActivity) -> LiveActivity {
    match v.state {
        Some(st) if !st.permits_activity() => LiveActivity {
            activity: None,
            state: Some(st),
            last_message: None,
            last_seq: v.last_seq,
        },
        _ => v,
    }
}

// Ported from mobile's `test/rc/rc_events_test.dart:9-289` — every case, same
// inputs and assertions.
#[cfg(test)]
mod tests {
    use super::*;

    fn parse(event: &str, data: &str) -> Option<RcEvent> {
        parse_rc_event(&SseEvent {
            event: event.to_string(),
            data: data.to_string(),
        })
    }

    /// An `ActivityChanged` with only the fields the fold tests vary.
    fn activity_changed(
        shed: &str,
        slug: &str,
        activity: Option<RcActivity>,
        state: Option<RcState>,
        last_message: Option<&str>,
    ) -> RcEvent {
        RcEvent::ActivityChanged {
            shed: shed.to_string(),
            slug: slug.to_string(),
            activity,
            activity_at: None,
            state,
            last_message: last_message.map(str::to_string),
        }
    }

    // ---- parse_rc_event (rc_events_test.dart:10-138) ----

    #[test]
    fn activity_changed_decodes_typed_patch() {
        let ev = parse(
            "activity.changed",
            r#"{"shed":"proj","slug":"cdx777","activity":"needs_input",
                "activity_at":"2026-06-19T18:54:12Z","state":"ready"}"#,
        )
        .unwrap();
        assert_eq!(
            ev,
            RcEvent::ActivityChanged {
                shed: "proj".into(),
                slug: "cdx777".into(),
                activity: Some(RcActivity::NeedsInput),
                activity_at: Some("2026-06-19T18:54:12Z".into()),
                state: Some(RcState::Ready),
                last_message: None, // not carried on this frame
            }
        );
        assert_eq!(ev.shed(), "proj");
    }

    #[test]
    fn activity_changed_payload_last_message_supersedes_held() {
        let ev = parse(
            "activity.changed",
            r#"{"shed":"proj","slug":"cdx777","activity":"working",
                "state":"ready","last_message":"Running tests."}"#,
        )
        .unwrap();
        match &ev {
            RcEvent::ActivityChanged { last_message, .. } => {
                assert_eq!(last_message.as_deref(), Some("Running tests."));
            }
            other => panic!("wrong variant: {other:?}"),
        }

        let mut o = ActivityOverlay::empty().apply(&activity_changed(
            "proj",
            "cdx777",
            Some(RcActivity::Working),
            Some(RcState::Ready),
            Some("older preview"),
        ));
        o = o.apply(&ev);
        assert_eq!(
            o.lookup("proj", "cdx777").unwrap().last_message.as_deref(),
            Some("Running tests.")
        );

        // A frame WITHOUT last_message keeps the held preview (no wipe).
        o = o.apply(&activity_changed(
            "proj",
            "cdx777",
            Some(RcActivity::NeedsInput),
            Some(RcState::Ready),
            None,
        ));
        assert_eq!(
            o.lookup("proj", "cdx777").unwrap().last_message.as_deref(),
            Some("Running tests.")
        );
    }

    #[test]
    fn session_updated_extracts_fields_from_session() {
        let ev = parse(
            "session.updated",
            r#"{"shed":"proj","slug":"cdx777","session":
                {"state":"dead","activity":"idle","last_message":"bye"}}"#,
        )
        .unwrap();
        assert_eq!(
            ev,
            RcEvent::SessionUpdated {
                shed: "proj".into(),
                slug: "cdx777".into(),
                activity: Some(RcActivity::Idle),
                state: Some(RcState::Dead),
                last_message: Some("bye".into()),
                removed: false,
            }
        );
    }

    #[test]
    fn message_appended_decodes_seq() {
        assert_eq!(
            parse("message.appended", r#"{"shed":"p","slug":"s","seq":42}"#),
            Some(RcEvent::MessageAppended {
                shed: "p".into(),
                slug: "s".into(),
                seq: 42,
            })
        );
    }

    #[test]
    fn message_appended_seq_accepts_any_json_number_truncating() {
        // Dart `seq.toInt()` truncation parity: a fractional seq keeps the
        // event, truncated toward zero.
        assert_eq!(
            parse("message.appended", r#"{"shed":"p","slug":"s","seq":42.9}"#),
            Some(RcEvent::MessageAppended {
                shed: "p".into(),
                slug: "s".into(),
                seq: 42,
            })
        );
        // Narrowings beyond Dart (documented on `seq_of`): a negative seq and
        // a value beyond u64 range are dropped, never truncated in or wrapped.
        assert_eq!(
            parse("message.appended", r#"{"shed":"p","slug":"s","seq":-1}"#),
            None
        );
        assert_eq!(
            parse("message.appended", r#"{"shed":"p","slug":"s","seq":1e300}"#),
            None
        );
    }

    #[test]
    fn bom_padded_values_trim_on_darts_set() {
        // Dart's `String.trim` strips U+FEFF; Rust's `str::trim` does not —
        // the shared helpers trim on Dart's set (`models.rs::dart_trim`), so a
        // BOM-only shed is blank (required key missing → event dropped) and a
        // BOM-padded activity decodes clean. JSON \u escapes, decoded by
        // serde_json.
        assert_eq!(parse("shed.stopped", r#"{"shed":"\uFEFF"}"#), None);
        let ev = parse(
            "activity.changed",
            r#"{"shed":"p","slug":"a","activity":"\uFEFFworking\uFEFF"}"#,
        )
        .unwrap();
        match &ev {
            RcEvent::ActivityChanged { activity, .. } => {
                assert_eq!(*activity, Some(RcActivity::Working));
            }
            other => panic!("wrong variant: {other:?}"),
        }
    }

    #[test]
    fn synthetic_hub_unavailable_and_shed_stopped_decode() {
        assert_eq!(
            parse("hub.unavailable", r#"{"shed":"p"}"#),
            Some(RcEvent::HubUnavailable { shed: "p".into() })
        );
        assert_eq!(
            parse("shed.stopped", r#"{"shed":"p"}"#),
            Some(RcEvent::ShedStopped { shed: "p".into() })
        );
    }

    #[test]
    fn drops_unknown_events_non_object_data_and_missing_keys() {
        assert_eq!(parse("heartbeat", r#"{"shed":"p"}"#), None);
        assert_eq!(parse("activity.changed", "not-json"), None);
        assert_eq!(parse("activity.changed", r#"{"shed":"p"}"#), None); // no slug
        assert_eq!(
            parse("message.appended", r#"{"shed":"p","slug":"s"}"#),
            None
        ); // no seq
    }

    #[test]
    fn non_string_field_values_are_tolerated_nulled_never_errors() {
        // Guest-controlled frames: a wrong-typed value must be dropped/nulled —
        // an error here would kill the SSE stream and turn one malformed frame
        // into a perpetual reconnect storm.
        let ev = parse(
            "activity.changed",
            r#"{"shed":"p","slug":"a","activity":42,"activity_at":[],"state":7}"#,
        )
        .unwrap();
        assert_eq!(
            ev,
            RcEvent::ActivityChanged {
                shed: "p".into(),
                slug: "a".into(),
                activity: None,
                activity_at: None,
                state: None,
                last_message: None,
            }
        );
    }

    #[test]
    fn session_updated_null_or_absent_session_signals_removal() {
        for data in [
            r#"{"shed":"p","slug":"a","session":null}"#,
            r#"{"shed":"p","slug":"a"}"#,
        ] {
            let ev = parse("session.updated", data).unwrap();
            assert_eq!(
                ev,
                RcEvent::SessionUpdated {
                    shed: "p".into(),
                    slug: "a".into(),
                    activity: None,
                    state: None,
                    last_message: None,
                    removed: true,
                },
                "data: {data}"
            );
        }
    }

    #[test]
    fn session_updated_last_message_strips_unicode_format_chars() {
        // JSON \u escapes: RLO (U+202E) + zero-width space (U+200B). The raw
        // string keeps the backslashes literal, so serde_json — not Rust —
        // decodes the escapes, exactly like the Dart original.
        let ev = parse(
            "session.updated",
            r#"{"shed":"p","slug":"a","session":
                {"state":"ready","last_message":"a\u202erev\u200bersed"}}"#,
        )
        .unwrap();
        match &ev {
            RcEvent::SessionUpdated { last_message, .. } => {
                assert_eq!(last_message.as_deref(), Some("areversed"));
            }
            other => panic!("wrong variant: {other:?}"),
        }
    }

    // ---- ActivityOverlay::apply (rc_events_test.dart:140-288) ----

    #[test]
    fn activity_changed_patches_exactly_one_row() {
        let o = ActivityOverlay::empty()
            .apply(&activity_changed(
                "p",
                "a",
                Some(RcActivity::Working),
                Some(RcState::Ready),
                None,
            ))
            .apply(&activity_changed(
                "p",
                "b",
                Some(RcActivity::Idle),
                Some(RcState::Ready),
                None,
            ));
        assert_eq!(
            o.lookup("p", "a").unwrap().activity,
            Some(RcActivity::Working)
        );
        assert_eq!(o.lookup("p", "b").unwrap().activity, Some(RcActivity::Idle));

        let next = o.apply(&activity_changed(
            "p",
            "a",
            Some(RcActivity::NeedsInput),
            Some(RcState::Ready),
            None,
        ));
        // Only row a moved; b is untouched; the source overlay is not mutated.
        assert_eq!(
            next.lookup("p", "a").unwrap().activity,
            Some(RcActivity::NeedsInput)
        );
        assert_eq!(
            next.lookup("p", "b").unwrap().activity,
            Some(RcActivity::Idle)
        );
        assert_eq!(
            o.lookup("p", "a").unwrap().activity,
            Some(RcActivity::Working)
        );
        assert_ne!(next, o);
    }

    #[test]
    fn message_appended_bumps_last_seq_without_wiping_activity() {
        let mut o = ActivityOverlay::empty().apply(&activity_changed(
            "p",
            "a",
            Some(RcActivity::Working),
            Some(RcState::Ready),
            None,
        ));
        o = o.apply(&RcEvent::MessageAppended {
            shed: "p".into(),
            slug: "a".into(),
            seq: 7,
        });
        assert_eq!(o.lookup("p", "a").unwrap().last_seq, Some(7));
        assert_eq!(
            o.lookup("p", "a").unwrap().activity,
            Some(RcActivity::Working)
        ); // preserved
    }

    #[test]
    fn hub_unavailable_and_shed_stopped_drop_that_shed_only() {
        let o = ActivityOverlay::empty()
            .apply(&activity_changed(
                "p",
                "a",
                Some(RcActivity::Working),
                None,
                None,
            ))
            .apply(&activity_changed(
                "q",
                "b",
                Some(RcActivity::Idle),
                None,
                None,
            ));
        let after_hub = o.apply(&RcEvent::HubUnavailable { shed: "p".into() });
        assert!(after_hub.lookup("p", "a").is_none()); // degraded → falls back to base
        assert!(after_hub.lookup("q", "b").is_some());

        let after_stop = after_hub.apply(&RcEvent::ShedStopped { shed: "q".into() });
        assert!(after_stop.is_empty());
        // Dropping a shed with no patches is a no-op with equal contents (Dart
        // asserts object identity — a Riverpod optimization not ported).
        assert_eq!(
            after_stop.apply(&RcEvent::HubUnavailable { shed: "p".into() }),
            after_stop
        );
    }

    #[test]
    fn lookup_keys_by_shed_and_slug() {
        let o = ActivityOverlay::empty().apply(&activity_changed(
            "p",
            "a",
            Some(RcActivity::Working),
            None,
            None,
        ));
        assert!(o.lookup("p", "a").is_some());
        assert!(o.lookup("p", "z").is_none());
        assert!(o.lookup("q", "a").is_none()); // shed is part of the key
        assert_eq!(o.len(), 1);
    }

    #[test]
    fn session_updated_removal_drops_the_patch_entirely() {
        let mut o = ActivityOverlay::empty()
            .apply(&activity_changed(
                "p",
                "a",
                Some(RcActivity::Working),
                Some(RcState::Ready),
                None,
            ))
            .apply(&activity_changed(
                "p",
                "b",
                Some(RcActivity::Idle),
                None,
                None,
            ));
        // A kill (session:null) must REMOVE the patch — retaining the previous
        // fields would keep a dead session's live badge alive forever.
        o = o.apply(&RcEvent::SessionUpdated {
            shed: "p".into(),
            slug: "a".into(),
            activity: None,
            state: None,
            last_message: None,
            removed: true,
        });
        assert!(o.lookup("p", "a").is_none());
        assert!(o.lookup("p", "b").is_some()); // others untouched
    }

    #[test]
    fn blocking_state_suppresses_activity_and_last_message() {
        // Whole-dimension suppression, mirroring the Go server.
        let mut o = ActivityOverlay::empty().apply(&RcEvent::SessionUpdated {
            shed: "p".into(),
            slug: "a".into(),
            activity: Some(RcActivity::Working),
            state: Some(RcState::Ready),
            last_message: Some("pre-death context".into()),
            removed: false,
        });
        o = o.apply(&RcEvent::MessageAppended {
            shed: "p".into(),
            slug: "a".into(),
            seq: 5,
        });
        // The session dies: the patch keeps the state (and last_seq) but must
        // clear activity and last_message — stale context is not current context.
        o = o.apply(&activity_changed(
            "p",
            "a",
            Some(RcActivity::Working),
            Some(RcState::Dead),
            None,
        ));
        let p = o.lookup("p", "a").unwrap();
        assert_eq!(p.state, Some(RcState::Dead));
        assert_eq!(p.activity, None);
        assert_eq!(p.last_message, None);
        assert_eq!(p.last_seq, Some(5)); // retained — the feed history stays readable
    }

    #[test]
    fn rc_event_shed_accessor_covers_every_variant() {
        let events = [
            activity_changed("s1", "a", None, None, None),
            RcEvent::SessionUpdated {
                shed: "s2".into(),
                slug: "a".into(),
                activity: None,
                state: None,
                last_message: None,
                removed: true,
            },
            RcEvent::MessageAppended {
                shed: "s3".into(),
                slug: "a".into(),
                seq: 1,
            },
            RcEvent::HubUnavailable { shed: "s4".into() },
            RcEvent::ShedStopped { shed: "s5".into() },
        ];
        let sheds: Vec<&str> = events.iter().map(RcEvent::shed).collect();
        assert_eq!(sheds, ["s1", "s2", "s3", "s4", "s5"]);
    }
}
