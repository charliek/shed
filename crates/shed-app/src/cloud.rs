//! A cloud shed's row (plan 041 R13): what a client shows for a shed whose server
//! answers with its machine's `cloud` facts, decided here once, toolkit-free, so
//! every shell (the Tauri UI, a later iced one) only draws the struct.
//!
//! Pure: a [`Shed`], the server's [`CloudCaps`] and an injected `now` (unix
//! seconds) in, a [`CloudRow`] out. Times are parsed fail-closed
//! ([`crate::timefmt::parse_instant`]): a lease whose times do not parse is
//! `unknown` and offers no extend.

use chrono::{DateTime, Datelike, Utc};
use serde::Serialize;

use shed_core::http::ApiFailure;
pub use shed_core::models::MAX_EXTEND_HOURS;
use shed_core::models::{
    CloudUsage, Shed, ShedCloud, ShedStatus, DEFAULT_LEASE_WARN_S, FEATURE_LEASE,
};

use crate::timefmt::parse_instant;

/// The tag a cloud row wears.
pub const CLOUD_BADGE: &str = "Cloud";

/// What a server offers for its cloud sheds, from its `/api/info` features.
#[derive(Debug, Clone, Copy, Default, PartialEq, Eq, Serialize)]
pub struct CloudCaps {
    /// The server extends leases (`POST /api/sheds/{name}/extend`; the `lease`
    /// feature).
    pub lease: bool,
}

impl CloudCaps {
    /// The capabilities a server's feature tokens advertise.
    pub fn from_features<S: AsRef<str>>(features: &[S]) -> Self {
        CloudCaps {
            lease: features.iter().any(|f| f.as_ref() == FEATURE_LEASE),
        }
    }
}

/// Where a lease stands at `now`.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize)]
#[serde(rename_all = "lowercase")]
pub enum LeaseState {
    /// More than its warning time left.
    Ok,
    /// Within its warning time of the end (shown amber).
    Soon,
    /// At or past its end: the machine is being deleted.
    Ended,
    /// Not known: the service has not answered for the machine yet, or its time
    /// does not parse.
    Unknown,
}

/// Which actions a cloud row offers. Restart is never one: a cloud shed has none.
#[derive(Debug, Clone, Copy, Default, PartialEq, Eq, Serialize)]
pub struct CloudActions {
    pub start: bool,
    pub stop: bool,
    pub delete: bool,
    pub extend: bool,
}

/// A cloud shed as a client shows it.
#[derive(Debug, Clone, PartialEq, Eq, Serialize)]
pub struct CloudRow {
    /// The tag ([`CLOUD_BADGE`]).
    pub badge: String,
    /// `2 vCPU · 8 GB`; the size's name when it says no numbers; "" when nothing.
    pub size_words: String,
    /// The image the machine was made from ("" when not said).
    pub image: String,
    /// `deleted in 5h 10m`, `lease ended`, or `lease unknown`.
    pub lease_words: String,
    pub lease_state: LeaseState,
    /// `running for 3h 2m` for a running shed whose last start is known.
    pub run_words: Option<String>,
    pub actions: CloudActions,
    /// The hours offered to extend by: the server's presets, each clipped to
    /// [`Self::extend_max_h`] (duplicates dropped, ascending). Empty when no extend
    /// is offered.
    pub extend_presets_h: Vec<i64>,
    /// The most hours an extend may ask for now: the whole hours between the lease's
    /// end and its cap, at most [`MAX_EXTEND_HOURS`]; 0 when no extend is offered.
    pub extend_max_h: i64,
    /// When the machine is deleted (`14:30 UTC` today, `2026-11-27 14:30 UTC` on
    /// another day), when known.
    pub expires_words: Option<String>,
    /// The cap no extend goes past, in the same words, when known.
    pub cap_words: Option<String>,
    /// When the machine was made, in the same words, when known.
    pub created_words: Option<String>,
}

/// The row for `shed` at `now` (unix seconds), or `None` for a shed with no
/// `cloud` (any shed of a server without cloud sheds), which shows as before.
pub fn cloud_row(shed: &Shed, caps: CloudCaps, now: i64) -> Option<CloudRow> {
    let cloud = shed.cloud.as_ref()?;
    let lease = cloud.lease.as_ref();
    let expires = lease.and_then(|l| secs(&l.expires_at));
    let cap = lease.and_then(|l| secs(&l.max_expires_at));
    let (lease_state, lease_words) = match (lease, expires) {
        (Some(_), Some(end)) if end <= now => (LeaseState::Ended, "lease ended".to_string()),
        (Some(l), Some(end)) => {
            let left = end - now;
            let warn = l.warn_s.unwrap_or(DEFAULT_LEASE_WARN_S);
            let state = if warn > 0 && left < warn {
                LeaseState::Soon
            } else {
                LeaseState::Ok
            };
            (state, format!("deleted in {}", format_left(left)))
        }
        _ => (LeaseState::Unknown, "lease unknown".to_string()),
    };
    let ended = lease_state == LeaseState::Ended;
    let running = shed.status == ShedStatus::Running;
    let busy = matches!(shed.status, ShedStatus::Starting | ShedStatus::Stopping);

    // Extend: a running or stopped shed whose server extends, whose lease is known,
    // has not ended and says it can be, with at least an hour left to its cap.
    let (extend_max_h, mut extend_presets_h) = match (lease, expires, cap) {
        (Some(l), Some(end), Some(cap))
            if caps.lease
                && l.extendable
                && !ended
                && matches!(shed.status, ShedStatus::Running | ShedStatus::Stopped) =>
        {
            let max = ((cap - end) / 3600).clamp(0, MAX_EXTEND_HOURS);
            let presets: Vec<i64> = if max > 0 {
                l.presets_h
                    .iter()
                    .filter(|&&h| h >= 1)
                    .map(|&h| h.min(max))
                    .collect()
            } else {
                Vec::new()
            };
            (max, presets)
        }
        _ => (0, Vec::new()),
    };
    extend_presets_h.sort_unstable();
    extend_presets_h.dedup();

    let run_words = cloud
        .started_at
        .as_deref()
        .and_then(secs)
        .filter(|&at| running && at <= now)
        .map(|at| format!("running for {}", format_left(now - at)));

    Some(CloudRow {
        badge: CLOUD_BADGE.to_string(),
        size_words: size_words(cloud),
        image: cloud.image.clone(),
        lease_words,
        lease_state,
        run_words,
        actions: CloudActions {
            start: shed.status == ShedStatus::Stopped && !ended,
            stop: running && !ended,
            delete: !running && !busy,
            extend: extend_max_h > 0,
        },
        extend_presets_h,
        extend_max_h,
        expires_words: expires.map(|t| format_lease_time(t, now)),
        cap_words: cap.map(|t| format_lease_time(t, now)),
        created_words: secs(&cloud.created_at).map(|t| format_lease_time(t, now)),
    })
}

/// The fixed words for one of the account service's refusals, by its code and
/// reason (the CLI's `computeWords`, word for word): `None` for any other code,
/// whose server message is shown instead. The server's message for these codes is
/// never shown.
pub fn refusal_words(code: &str, reason: &str) -> Option<&'static str> {
    Some(match (code, reason) {
        ("COMPUTE_LIMIT_REACHED", "person") => "you hold as many cloud machines as the account service allows a person; delete one of your cloud sheds and try again",
        ("COMPUTE_LIMIT_REACHED", "organization") => "the organization holds as many cloud machines as the account service allows; try again once one is deleted",
        ("COMPUTE_LIMIT_REACHED", _) => "the account service's limit on cloud machines is reached",
        ("COMPUTE_POLICY_REFUSED", "size") => "the account service does not allow this size",
        ("COMPUTE_POLICY_REFUSED", "lease") => "the account service does not allow this lease",
        ("COMPUTE_POLICY_REFUSED", "image") => "the account service does not offer this image",
        ("COMPUTE_POLICY_REFUSED", "access") => "the account service offers no way to reach a machine that this server supports",
        ("COMPUTE_POLICY_REFUSED", "person") => "the account service makes cloud machines only for its own people and for service clients; a local admin can list, stop, delete and extend cloud sheds but not create them",
        ("COMPUTE_POLICY_REFUSED", "client") => "this service client is not registered with the account service for this server",
        ("COMPUTE_POLICY_REFUSED", _) => "the account service's policy refused this",
        ("COMPUTE_DENIED", _) => "the account service does not let this server make cloud machines",
        ("COMPUTE_UNAVAILABLE", _) => "the account service could not be reached, or this server has not finished reading its machines; try again later",
        ("COMPUTE_DELETING", _) => "the account service is deleting this cloud shed's machine (its lease ended, or it was deleted at the service)",
        ("EXTEND_NOT_ALLOWED", _) => "this cloud shed's lease cannot be extended (a service client's shed, or the account service's policy)",
        ("LEASE_AT_CAP", _) => "the extend would take the lease past its cap; extend by fewer hours",
        ("LEASE_ENDED", _) => "this cloud shed's lease has ended; its machine is being deleted",
        ("PERSON_INACTIVE", _) => "the shed's person is no longer active at the account service",
        _ => return None,
    })
}

/// A failed extend or usage read as a shell shows it: the API error's `code`
/// (`action_failed` when no server answered), its `reason`, the `status`, and
/// `words`: the fixed words for an account-service refusal ([`refusal_words`]), else
/// the error's own (cleaned) message.
#[derive(Debug, Clone, PartialEq, Eq, Serialize)]
pub struct CloudFailure {
    pub code: String,
    pub reason: String,
    pub status: Option<u16>,
    pub words: String,
}

impl CloudFailure {
    pub fn from_api(e: &ApiFailure) -> Self {
        let code = if e.code.is_empty() {
            "action_failed"
        } else {
            e.code.as_str()
        };
        CloudFailure {
            code: code.to_string(),
            reason: e.reason.clone(),
            status: e.status,
            words: refusal_words(&e.code, &e.reason)
                .map_or_else(|| e.message.clone(), str::to_string),
        }
    }
}

/// The usage line under a server's heading (the CLI's footer, without its
/// `Cloud:` head): `3 of 4 machines (2 running, 1 stopped) · organization 7 of 10`,
/// or `organization 7 of 10` for a caller who is not a person of the service.
pub fn usage_words(u: &CloudUsage) -> String {
    let org = format!("organization {} of {}", u.org.machines, u.org.max);
    match u.person {
        Some(p) => format!(
            "{} of {} machines ({} running, {} stopped) · {org}",
            p.machines, p.max, p.running, p.stopped
        ),
        None => org,
    }
}

/// An instant's unix seconds, fail-closed.
fn secs(raw: &str) -> Option<i64> {
    parse_instant(raw).map(|(s, _)| s)
}

/// `2 vCPU · 8 GB` (`1 vCPU`, `1.5 GB`); the size's name when it says neither.
fn size_words(cloud: &ShedCloud) -> String {
    let size = &cloud.size;
    let mut parts = Vec::new();
    if size.cpus > 0 {
        parts.push(format!("{} vCPU", size.cpus));
    }
    if size.memory_mb > 0 {
        let gb = size.memory_mb as f64 / 1024.0;
        parts.push(if size.memory_mb % 1024 == 0 {
            format!("{gb:.0} GB")
        } else {
            format!("{gb:.1} GB")
        });
    }
    if parts.is_empty() {
        return size.name.clone();
    }
    parts.join(" · ")
}

/// A span of seconds as the lease words say it: `1d 2h`, `1h 20m`, `20m` (minutes
/// rounded down, never negative) — the server's `config.FormatLeft`.
pub fn format_left(secs: i64) -> String {
    let secs = secs.max(0);
    let (days, hours, minutes) = (secs / 86_400, secs / 3600 % 24, secs / 60 % 60);
    if days > 0 {
        format!("{days}d {hours}h")
    } else if hours > 0 {
        format!("{hours}h {minutes}m")
    } else {
        format!("{minutes}m")
    }
}

/// An instant in UTC: `14:30 UTC` on now's day, `2026-11-27 14:30 UTC` on any
/// other — the server's `config.FormatLeaseTime`.
pub fn format_lease_time(at: i64, now: i64) -> String {
    let (Some(t), Some(n)) = (
        DateTime::<Utc>::from_timestamp(at, 0),
        DateTime::<Utc>::from_timestamp(now, 0),
    ) else {
        return String::new();
    };
    if t.year() == n.year() && t.ordinal() == n.ordinal() {
        t.format("%H:%M UTC").to_string()
    } else {
        t.format("%Y-%m-%d %H:%M UTC").to_string()
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use serde_json::json;

    /// 2026-11-26T09:00:00Z.
    const NOW: i64 = 1_795_683_600;

    fn at(offset_s: i64) -> String {
        crate::timefmt::format_iso8601(NOW + offset_s)
    }

    const H: i64 = 3600;

    fn shed(status: &str, cloud: serde_json::Value) -> Shed {
        let mut v = json!({
            "host": "cloud-a", "name": "widgets", "status": status, "backend": "remote",
        });
        v["cloud"] = cloud;
        serde_json::from_value(v).unwrap()
    }

    /// A cloud with a lease ending `left` seconds from now and its cap `cap` seconds
    /// from now; `lease` members override.
    fn cloud(left: i64, cap: i64, lease: serde_json::Value) -> serde_json::Value {
        let mut l = json!({
            "expires_at": at(left), "max_expires_at": at(cap), "extendable": true,
            "presets_h": [6, 12, 24, 48], "warn_s": 7200,
        });
        let serde_json::Value::Object(members) = lease else {
            panic!("lease overrides are an object");
        };
        for (k, v) in members {
            l[k.as_str()] = v;
        }
        json!({
            "provider": "gce", "size": {"name": "standard", "cpus": 2, "memory_mb": 8192},
            "disk_gb": 50, "image": "shed-full-1", "created_at": at(-3 * H),
            "started_at": at(-(3 * H + 120)), "lease": l,
        })
    }

    const CAPS: CloudCaps = CloudCaps { lease: true };

    fn row(status: &str, c: serde_json::Value) -> CloudRow {
        cloud_row(&shed(status, c), CAPS, NOW).expect("a cloud row")
    }

    #[test]
    fn a_running_cloud_shed() {
        let r = row("running", cloud(5 * H + 600, 100 * H, json!({})));
        assert_eq!(r.badge, "Cloud");
        assert_eq!(r.size_words, "2 vCPU · 8 GB");
        assert_eq!(r.image, "shed-full-1");
        assert_eq!(r.lease_words, "deleted in 5h 10m");
        assert_eq!(r.lease_state, LeaseState::Ok);
        assert_eq!(r.run_words.as_deref(), Some("running for 3h 2m"));
        assert_eq!(
            r.actions,
            CloudActions {
                start: false,
                stop: true,
                delete: false,
                extend: true
            }
        );
        assert_eq!(r.extend_presets_h, vec![6, 12, 24, 48]);
        assert_eq!(r.extend_max_h, 94);
        assert_eq!(r.expires_words.as_deref(), Some("14:10 UTC"));
        assert_eq!(r.cap_words.as_deref(), Some("2026-11-30 13:00 UTC"));
        assert_eq!(r.created_words.as_deref(), Some("06:00 UTC"));
    }

    #[test]
    fn a_stopped_cloud_shed_starts_deletes_and_extends() {
        let r = row("stopped", cloud(30 * H, 100 * H, json!({})));
        assert_eq!(
            r.actions,
            CloudActions {
                start: true,
                stop: false,
                delete: true,
                extend: true
            }
        );
        assert_eq!(r.run_words, None, "not running");
        assert_eq!(r.lease_words, "deleted in 1d 6h");
        for status in ["starting", "stopping"] {
            let r = row(status, cloud(30 * H, 100 * H, json!({})));
            assert_eq!(r.actions, CloudActions::default(), "{status}");
        }
        let r = row("error", cloud(30 * H, 100 * H, json!({})));
        assert_eq!(
            r.actions,
            CloudActions {
                delete: true,
                ..CloudActions::default()
            }
        );
    }

    /// Presets past the cap are clipped to it (duplicates dropped); a lease at its
    /// cap offers no extend; the custom field's max is never past 720.
    #[test]
    fn presets_are_clipped_to_the_cap() {
        let r = row("running", cloud(2 * H, 2 * H + 20 * H + 1800, json!({})));
        assert_eq!(r.extend_max_h, 20);
        assert_eq!(r.extend_presets_h, vec![6, 12, 20]);
        let r = row(
            "running",
            cloud(2 * H, 2 * H + 3 * H, json!({"presets_h": [6, 12, 0, -1]})),
        );
        assert_eq!(r.extend_presets_h, vec![3]);
        let r = row("running", cloud(2 * H, 2 * H + 1800, json!({})));
        assert_eq!((r.extend_max_h, r.actions.extend), (0, false));
        assert!(r.extend_presets_h.is_empty());
        let r = row(
            "running",
            cloud(2 * H, 2 * H + 2000 * H, json!({"presets_h": [1000]})),
        );
        assert_eq!(r.extend_max_h, MAX_EXTEND_HOURS);
        assert_eq!(r.extend_presets_h, vec![720]);
        let r = row("running", cloud(2 * H, 100 * H, json!({"presets_h": []})));
        assert!(
            r.actions.extend && r.extend_presets_h.is_empty(),
            "custom hours only"
        );
    }

    /// Soon under the warning time; `warn_s` absent is 2 h, 0 never warns.
    #[test]
    fn the_warning_threshold() {
        let state = |left: i64, warn: serde_json::Value| {
            let mut c = cloud(left, 100 * H, json!({}));
            match warn {
                serde_json::Value::Null => {
                    c["lease"].as_object_mut().unwrap().remove("warn_s");
                }
                w => c["lease"]["warn_s"] = w,
            }
            row("running", c).lease_state
        };
        assert_eq!(state(2 * H - 1, json!(7200)), LeaseState::Soon);
        assert_eq!(state(2 * H, json!(7200)), LeaseState::Ok);
        assert_eq!(
            state(2 * H - 1, json!(null)),
            LeaseState::Soon,
            "absent: 2 h"
        );
        assert_eq!(state(2 * H + 1, json!(null)), LeaseState::Ok);
        assert_eq!(state(60, json!(0)), LeaseState::Ok, "0: never");
        assert_eq!(state(30 * 60 - 1, json!(1800)), LeaseState::Soon);
        assert_eq!(state(30 * 60 + 1, json!(1800)), LeaseState::Ok);
    }

    #[test]
    fn a_past_lease_has_ended() {
        for left in [0, -5 * H] {
            let r = row("running", cloud(left, 100 * H, json!({})));
            assert_eq!(r.lease_state, LeaseState::Ended);
            assert_eq!(r.lease_words, "lease ended");
            assert_eq!(r.actions, CloudActions::default(), "{left}");
            assert_eq!((r.extend_max_h, r.extend_presets_h.len()), (0, 0));
            let r = row("stopped", cloud(left, 100 * H, json!({})));
            assert_eq!(
                r.actions,
                CloudActions {
                    delete: true,
                    ..CloudActions::default()
                }
            );
        }
    }

    /// A time that does not parse is not a lease: unknown, and no extend.
    #[test]
    fn an_unparseable_time_offers_no_extend() {
        let r = row(
            "running",
            cloud(5 * H, 100 * H, json!({"expires_at": "tomorrow"})),
        );
        assert_eq!(r.lease_state, LeaseState::Unknown);
        assert_eq!(r.lease_words, "lease unknown");
        assert!(!r.actions.extend && r.actions.stop);
        assert_eq!(r.expires_words, None);
        let r = row(
            "running",
            cloud(5 * H, 100 * H, json!({"max_expires_at": ""})),
        );
        assert_eq!(r.lease_state, LeaseState::Ok, "the end is known");
        assert!(!r.actions.extend, "the cap is not");
        assert_eq!(r.cap_words, None);
        let mut c = cloud(5 * H, 100 * H, json!({}));
        c["started_at"] = json!("0001-01-01T00:00:00Z");
        c["created_at"] = json!("then");
        let r = row("running", c);
        assert_eq!((r.run_words, r.created_words), (None, None));
    }

    /// A service client's machine is not extendable: shown, never extended.
    #[test]
    fn a_clients_machine_offers_no_extend() {
        let r = row(
            "running",
            cloud(
                20 * H,
                30 * H,
                json!({"extendable": false, "presets_h": []}),
            ),
        );
        assert_eq!(r.lease_words, "deleted in 20h 0m");
        assert!(!r.actions.extend && r.actions.stop);
        assert!(r.extend_presets_h.is_empty());
        assert_eq!(r.extend_max_h, 0);
    }

    /// A server that does not extend leases offers no extend whatever the lease says.
    #[test]
    fn a_server_without_leases_offers_no_extend() {
        let s = shed("running", cloud(5 * H, 100 * H, json!({})));
        let r = cloud_row(&s, CloudCaps::default(), NOW).unwrap();
        assert!(!r.actions.extend && r.extend_presets_h.is_empty());
        assert_eq!(
            CloudCaps::from_features(&["overview", "lease"]),
            CloudCaps { lease: true }
        );
        assert_eq!(CloudCaps::from_features::<&str>(&[]), CloudCaps::default());
    }

    /// The lease is unknown before the service answered for the machine.
    #[test]
    fn a_lease_not_known_yet() {
        let mut c = cloud(5 * H, 100 * H, json!({}));
        c.as_object_mut().unwrap().remove("lease");
        let r = row("starting", c);
        assert_eq!(r.lease_state, LeaseState::Unknown);
        assert_eq!(r.lease_words, "lease unknown");
        assert_eq!((r.expires_words, r.cap_words), (None, None));
        assert_eq!(r.actions, CloudActions::default());
        let mut c = cloud(5 * H, 100 * H, json!({}));
        c.as_object_mut().unwrap().remove("lease");
        let r = row("stopped", c);
        assert!(r.actions.start && r.actions.delete && !r.actions.extend);
    }

    /// A shed with no `cloud` has no row: it shows as before.
    #[test]
    fn a_shed_with_no_cloud_has_no_row() {
        let s: Shed =
            serde_json::from_value(json!({"name": "x", "status": "running", "backend": "vz"}))
                .unwrap();
        assert_eq!(cloud_row(&s, CAPS, NOW), None);
        let list: shed_core::models::ShedList =
            serde_json::from_str(include_str!("../../fixtures/cloud/sheds.json")).unwrap();
        assert!(list.sheds.iter().all(|s| cloud_row(s, CAPS, NOW).is_some()));
    }

    #[test]
    fn size_words_cover_odd_sizes() {
        let words = |size: serde_json::Value| {
            let mut c = cloud(5 * H, 100 * H, json!({}));
            c["size"] = size;
            row("running", c).size_words
        };
        assert_eq!(
            words(json!({"name": "tiny", "cpus": 1, "memory_mb": 1536})),
            "1 vCPU · 1.5 GB"
        );
        assert_eq!(words(json!({"name": "odd"})), "odd");
        assert_eq!(words(json!(null)), "");
    }

    #[test]
    fn refusals_have_fixed_words_by_code_and_reason() {
        for code in [
            "EXTEND_NOT_ALLOWED",
            "LEASE_AT_CAP",
            "LEASE_ENDED",
            "PERSON_INACTIVE",
            "COMPUTE_DELETING",
            "COMPUTE_UNAVAILABLE",
            "COMPUTE_DENIED",
        ] {
            assert!(refusal_words(code, "").is_some(), "{code}");
        }
        assert_ne!(
            refusal_words("COMPUTE_LIMIT_REACHED", "person"),
            refusal_words("COMPUTE_LIMIT_REACHED", "organization")
        );
        assert_eq!(
            refusal_words("COMPUTE_POLICY_REFUSED", "new-reason"),
            refusal_words("COMPUTE_POLICY_REFUSED", "")
        );
        assert_eq!(refusal_words("SHED_NOT_FOUND", ""), None);
    }

    #[test]
    fn usage_words_match_the_cli_footer() {
        let u: CloudUsage =
            serde_json::from_str(include_str!("../../fixtures/cloud/usage-person.json")).unwrap();
        assert_eq!(
            usage_words(&u),
            "2 of 100 machines (1 running, 1 stopped) · organization 2 of 100"
        );
        let u: CloudUsage =
            serde_json::from_str(include_str!("../../fixtures/cloud/usage-org.json")).unwrap();
        assert_eq!(usage_words(&u), "organization 2 of 100");
    }

    #[test]
    fn the_lease_words_match_the_cli() {
        assert_eq!(format_left(-5), "0m");
        assert_eq!(format_left(59), "0m");
        assert_eq!(format_left(20 * 60), "20m");
        assert_eq!(format_left(H + 20 * 60 + 59), "1h 20m");
        assert_eq!(format_left(26 * H + 59 * 60), "1d 2h");
        assert_eq!(format_lease_time(NOW + 5 * H + 30 * 60, NOW), "14:30 UTC");
        assert_eq!(format_lease_time(NOW + 24 * H, NOW), "2026-11-27 09:00 UTC");
    }
}
