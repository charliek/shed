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
        /// The session's lane (contract v2), carried verbatim off the nested
        /// `session` body when present. `None` on a removal, on an older hub
        /// that predates the field, and on a wrong-typed value. Additive: a
        /// consumer that ignores it behaves exactly as before, while one that
        /// reads it sees a future lane transition without an overview refetch.
        lane: Option<String>,
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
    // **`shed` is EMPTY when a hub is read directly** — on a machine or locally
    // there is no shed, and the hub says so (`{"shed":"","slug":…}`, verified
    // against a live machine hub and pinned below). Only the shed path has a
    // name to carry, because the server's aggregate proxy injects it while
    // fanning several guests' hubs into one stream.
    //
    // So `shed` is defaulted, not required, for the three HUB-EMITTED events.
    // Requiring it silently dropped every frame from a machine or local hub —
    // the feed decoded to nothing at all — which went unnoticed for two plans
    // because `sx` was the only reader and the shed transport (where the server
    // fills `shed` in) was the only path anyone watched. Plan 012 makes the
    // desktop and mobile clients read machine hubs directly, so this had to be
    // right before AC4/AC5 could pass.
    //
    // The two SYNTHETIC events below keep requiring it: they are produced only
    // by the server's aggregate stream, and both are meaningless without the
    // shed they pertain to.
    let hub_shed = shed.clone().unwrap_or_default();
    match e.event.as_str() {
        "activity.changed" => Some(RcEvent::ActivityChanged {
            shed: hub_shed,
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
                shed: hub_shed,
                slug: slug?,
                activity: sess.and_then(|s| activity_of(s.get("activity"))),
                state: sess.and_then(|s| state_of(s.get("state"))),
                last_message: sess.and_then(|s| clean_display(s.get("last_message"))),
                lane: sess.and_then(|s| opt_trimmed(s.get("lane"))),
                removed: sess.is_none(),
            })
        }
        "message.appended" => {
            let (shed, slug) = (hub_shed, slug?);
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
                // `lane` is carried on the event for consumers that read the
                // frame directly; the activity overlay folds only the live
                // activity dimensions, so it is not merged here.
                ..
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
    fn needs_approval_survives_decode_and_the_fold() {
        // Contract v2's new activity value must reach a card as itself — the
        // stream is where it will arrive first (no derivation produces it yet),
        // so pin it end to end: frame → typed event → overlay patch.
        let ev = parse(
            "activity.changed",
            r#"{"shed":"proj","slug":"cdx777","activity":"needs_approval","state":"ready"}"#,
        )
        .unwrap();
        let RcEvent::ActivityChanged { activity, .. } = &ev else {
            panic!("wrong variant: {ev:?}");
        };
        assert_eq!(*activity, Some(RcActivity::NeedsApproval));
        let o = ActivityOverlay::empty().apply(&ev);
        assert_eq!(
            o.lookup("proj", "cdx777").unwrap().activity,
            Some(RcActivity::NeedsApproval)
        );
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
                lane: None, // this hub predates contract v2
                removed: false,
            }
        );
    }

    #[test]
    fn session_updated_carries_lane_from_the_session_body() {
        // Contract v2: the lane rides the nested session body, so a future lane
        // transition reaches the client without an overview refetch.
        let ev = parse(
            "session.updated",
            r#"{"shed":"proj","slug":"cdx777","session":
                {"state":"ready","activity":"working","lane":"structured"}}"#,
        )
        .unwrap();
        let RcEvent::SessionUpdated { lane, removed, .. } = &ev else {
            panic!("wrong variant: {ev:?}");
        };
        assert_eq!(lane.as_deref(), Some("structured"));
        assert!(!removed);
        // Wrong-typed / blank / removal → None (never an error, never a guess).
        for data in [
            r#"{"shed":"p","slug":"a","session":{"state":"ready","lane":7}}"#,
            r#"{"shed":"p","slug":"a","session":{"state":"ready","lane":"  "}}"#,
            r#"{"shed":"p","slug":"a","session":null}"#,
        ] {
            let ev = parse("session.updated", data).unwrap();
            let RcEvent::SessionUpdated { lane, .. } = &ev else {
                panic!("wrong variant: {ev:?}");
            };
            assert_eq!(*lane, None, "data: {data}");
        }
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

    /// **A hub read DIRECTLY sends `"shed":""` — those frames must decode.**
    ///
    /// The payloads below are the literal bytes captured off a live machine
    /// hub (`shed-host-agent rc-hub` on mini3, read through an `ssh -L` tunnel,
    /// plan 012 S2). Before the fix, `shed` was required non-empty and every
    /// one of these decoded to `None`, so `sx watch --on machine:<m>` and
    /// `--on local` rendered a permanently silent feed while looking healthy.
    ///
    /// Keep these verbatim: they are the contract with the Rust hub, and the
    /// whole point is that they came off the wire rather than out of a fixture
    /// someone wrote to match the parser.
    #[test]
    fn a_directly_read_hub_sends_an_empty_shed_and_still_decodes() {
        let updated = parse(
            "session.updated",
            r#"{"shed":"","slug":"evtprb","session":{"slug":"evtprb","tmux_session":"rc-evtprb","kind":"shell","state":"ready","managed":true,"lane":"tui","display_name":"evtprobe","workdir":"/home/charliek","created_by":"probe","target_label":"local"}}"#,
        )
        .expect("a machine hub's session.updated must decode");
        match updated {
            RcEvent::SessionUpdated {
                shed,
                slug,
                removed,
                ..
            } => {
                assert_eq!(shed, "", "no shed to name when the hub is read directly");
                assert_eq!(slug, "evtprb");
                assert!(!removed, "a session body present means it is not gone");
            }
            other => panic!("expected SessionUpdated, got {other:?}"),
        }

        let activity = parse(
            "activity.changed",
            r#"{"shed":"","slug":"evtprb","activity":"working","activity_at":"2026-08-22T02:05:30Z","state":"ready"}"#,
        )
        .expect("a machine hub's activity.changed must decode");
        match activity {
            RcEvent::ActivityChanged {
                shed,
                slug,
                activity,
                ..
            } => {
                assert_eq!(shed, "");
                assert_eq!(slug, "evtprb");
                assert_eq!(activity, Some(RcActivity::Working));
            }
            other => panic!("expected ActivityChanged, got {other:?}"),
        }

        // A kill on a directly-read hub, likewise.
        let gone = parse(
            "session.updated",
            r#"{"shed":"","slug":"s2","session":null}"#,
        )
        .expect("must decode");
        assert!(matches!(
            gone,
            RcEvent::SessionUpdated { removed: true, .. }
        ));

        // …and the message notification.
        let appended =
            parse("message.appended", r#"{"shed":"","slug":"s3","seq":7}"#).expect("must decode");
        assert!(matches!(appended, RcEvent::MessageAppended { seq: 7, .. }));
    }

    /// The two SYNTHETIC events keep requiring a shed: only the server's
    /// aggregate stream produces them, and neither means anything without the
    /// shed it refers to.
    #[test]
    fn the_server_synthesized_events_still_require_a_shed() {
        assert_eq!(parse("hub.unavailable", r#"{"shed":""}"#), None);
        assert_eq!(parse("shed.stopped", r#"{"shed":""}"#), None);
        assert!(parse("hub.unavailable", r#"{"shed":"web"}"#).is_some());
        assert!(parse("shed.stopped", r#"{"shed":"web"}"#).is_some());
    }

    /// **The whole `shed`/`slug` presence contract, in one grid.**
    ///
    /// Every event name × every presence class of `shed` × every presence class
    /// of `slug`, asserting decode-or-drop for each cell. The rule the grid
    /// pins is deliberately asymmetric and was, until plan 012, only half
    /// right (see [`parse_rc_event`]'s comment):
    ///
    /// * the three HUB-EMITTED events need a `slug` and DON'T need a `shed` —
    ///   a directly-read hub has no shed to name and sends `""`;
    /// * the two SERVER-SYNTHESIZED events need a `shed` and never read `slug`.
    ///
    /// Written as a table on purpose: with the contract spread over a dozen
    /// hand-picked cases, the empty-shed rule was only covered where someone
    /// happened to think of it, and the hole cost two plans. Any future change
    /// to the rule now has to be a deliberate edit of this table.
    #[test]
    fn the_shed_and_slug_presence_grid_for_every_event() {
        /// A field's presence class. `Blank` is whitespace-only, which
        /// `opt_trimmed` collapses to absent — the same class as `Empty`, but
        /// worth its own row because the collapse is a helper's behaviour, not
        /// this decoder's, and could move underneath it.
        #[derive(Debug, Clone, Copy, PartialEq, Eq)]
        enum Presence {
            Absent,
            Empty,
            Blank,
            Present,
        }
        use Presence::*;

        impl Presence {
            /// The JSON value to insert, or `None` to omit the key entirely.
            fn wire(self, real: &str) -> Option<Value> {
                match self {
                    Absent => None,
                    Empty => Some(Value::String(String::new())),
                    Blank => Some(Value::String("   ".to_string())),
                    Present => Some(Value::String(real.to_string())),
                }
            }
        }

        /// The variant an event decoded to, so a cell proves it got the RIGHT
        /// event and not merely *an* event.
        fn variant_of(ev: &RcEvent) -> &'static str {
            match ev {
                RcEvent::ActivityChanged { .. } => "activity.changed",
                RcEvent::SessionUpdated { .. } => "session.updated",
                RcEvent::MessageAppended { .. } => "message.appended",
                RcEvent::HubUnavailable { .. } => "hub.unavailable",
                RcEvent::ShedStopped { .. } => "shed.stopped",
            }
        }

        /// The slug an event carries; the two synthetic events name a shed and
        /// nothing finer.
        fn slug_of(ev: &RcEvent) -> Option<&str> {
            match ev {
                RcEvent::ActivityChanged { slug, .. }
                | RcEvent::SessionUpdated { slug, .. }
                | RcEvent::MessageAppended { slug, .. } => Some(slug),
                RcEvent::HubUnavailable { .. } | RcEvent::ShedStopped { .. } => None,
            }
        }

        const SHED: &str = "web";
        const SLUG: &str = "cdx777";
        // The hub-emitted three, then the two the aggregate proxy mints.
        const HUB_EMITTED: [&str; 3] = ["activity.changed", "session.updated", "message.appended"];
        const SYNTHETIC: [&str; 2] = ["hub.unavailable", "shed.stopped"];

        let mut decoded = 0usize;
        for event in HUB_EMITTED.iter().chain(SYNTHETIC.iter()).copied() {
            for shed in [Absent, Empty, Blank, Present] {
                for slug in [Absent, Empty, Blank, Present] {
                    // Every OTHER required key is supplied, so a cell can only
                    // fail on the two fields under test.
                    let mut body = serde_json::Map::new();
                    if let Some(v) = shed.wire(SHED) {
                        body.insert("shed".to_string(), v);
                    }
                    if let Some(v) = slug.wire(SLUG) {
                        body.insert("slug".to_string(), v);
                    }
                    if event == "message.appended" {
                        body.insert("seq".to_string(), Value::from(9u64));
                    }
                    if event == "session.updated" {
                        body.insert(
                            "session".to_string(),
                            serde_json::json!({"state": "ready", "activity": "working"}),
                        );
                    }
                    let data = Value::Object(body).to_string();
                    let cell = format!("{event} shed={shed:?} slug={slug:?} data={data}");

                    let want = if HUB_EMITTED.contains(&event) {
                        slug == Present // shed is irrelevant to a hub-emitted event
                    } else {
                        shed == Present // slug is never read on a synthetic one
                    };
                    let got = parse(event, &data);
                    assert_eq!(got.is_some(), want, "{cell}");

                    let Some(ev) = got else { continue };
                    decoded += 1;
                    assert_eq!(variant_of(&ev), event, "{cell}");
                    if HUB_EMITTED.contains(&event) {
                        assert_eq!(slug_of(&ev), Some(SLUG), "{cell}");
                        // The defaulted field: a real shed rides through, and
                        // every not-really-there class flattens to `""` — the
                        // value a directly-read hub's frames carry.
                        let want_shed = if shed == Present { SHED } else { "" };
                        assert_eq!(ev.shed(), want_shed, "{cell}");
                    } else {
                        assert_eq!(slug_of(&ev), None, "{cell}");
                        assert_eq!(ev.shed(), SHED, "{cell}");
                    }
                }
            }
        }
        // 3 hub events × 4 shed classes (all decode) + 2 synthetic × 4 slug
        // classes — a guard on the loop itself, so a cell silently skipped
        // (a `continue` moved, a list edited) fails here rather than passing
        // by never running.
        assert_eq!(decoded, 3 * 4 + 2 * 4);
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
                    lane: None,
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

    /// **Empty-shed events COLLIDE in the overlay — this is why one overlay
    /// must not span two feeds.**
    ///
    /// The overlay keys by `(shed, slug)`, and a directly-read hub sends
    /// `shed: ""` on every frame (pinned above). So two sessions on two
    /// DIFFERENT machines that happen to share a slug land on the same key
    /// `("", slug)` and overwrite each other — mini2's `working` badge would
    /// render on mini3's row, or vanish.
    ///
    /// The shared layer does not have this bug today: `MachineHubWatcher`
    /// emits raw events and folds nothing, so there is one feed per machine
    /// and never a shared overlay. This test PINS the present behaviour rather
    /// than changing it, so the hazard is written down where the next client
    /// to build a unified sessions view will find it: keep **one overlay per
    /// feed**, and key rendered rows by `(origin, slug)` — the origin being
    /// the client's own `local` / `machine:<name>` / `shed:<name>` handle, not
    /// the event's `shed` field, which is empty for everything but the shed
    /// transport.
    #[test]
    fn two_machines_empty_shed_events_collide_on_one_overlay_key() {
        // Logically two machines (mini2, mini3), each running a session that
        // happens to have drawn the same slug. Off the wire the frames are
        // indistinguishable — nothing in the payload names the machine.
        let from_mini2 = parse(
            "activity.changed",
            r#"{"shed":"","slug":"cdx777","activity":"working","state":"ready"}"#,
        )
        .expect("mini2's frame decodes");
        let from_mini3 = parse(
            "activity.changed",
            r#"{"shed":"","slug":"cdx777","activity":"idle","state":"ready"}"#,
        )
        .expect("mini3's frame decodes");

        let o = ActivityOverlay::empty()
            .apply(&from_mini2)
            .apply(&from_mini3);
        assert_eq!(o.len(), 1, "the two machines share one key: {o:?}");
        // Last write wins — mini2's activity is simply gone.
        assert_eq!(
            o.lookup("", "cdx777").unwrap().activity,
            Some(RcActivity::Idle)
        );

        // And a degrading event on the empty shed clears BOTH machines'
        // patches, because `drop_shed("")` matches every directly-read row.
        // Constructed, not decoded — the wire can't carry an empty-shed
        // `hub.unavailable` — but a client that MINTS one locally to mean "my
        // machine's hub dropped" would wipe every machine's rows at once.
        let cleared = o.apply(&RcEvent::HubUnavailable {
            shed: String::new(),
        });
        assert!(cleared.is_empty());

        // Contrast: on the shed transport the server fills `shed` in, so the
        // same slug on two sheds stays two rows. The collision is a property of
        // the empty shed, not of the overlay's keying.
        let named = ActivityOverlay::empty()
            .apply(&activity_changed(
                "alpha",
                "cdx777",
                Some(RcActivity::Working),
                Some(RcState::Ready),
                None,
            ))
            .apply(&activity_changed(
                "beta",
                "cdx777",
                Some(RcActivity::Idle),
                Some(RcState::Ready),
                None,
            ));
        assert_eq!(named.len(), 2);
        assert_eq!(
            named.lookup("alpha", "cdx777").unwrap().activity,
            Some(RcActivity::Working)
        );
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
            lane: None,
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
            lane: None,
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
                lane: None,
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
