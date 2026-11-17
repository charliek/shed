//! The daemon's authoritative self-report (`LiveStatus`) snapshot builder plus the
//! shared RFC3339 time helpers. Ported to be wire-compatible, field-for-field, with
//! the Go `status_server.go` / `status.go` (§4 of the wire catalog).
//!
//! This is the crate-agnostic half: the snapshot **types** + [`build_live_status`]
//! **builder** (which an embedder reuses for its `broker.status` op) and the
//! RFC3339 helpers many core modules share. The read-only status UDS **server** and
//! the `status` CLI **client + renderer** live bin-side (`status_server.rs`) — only
//! the daemon serves that socket.

use std::time::{SystemTime, UNIX_EPOCH};

use serde::{Deserialize, Serialize};

use crate::config::{HostAgentConfig, NS_AWS_CREDENTIALS, NS_DOCKER_CREDENTIALS, NS_SSH_AGENT};
use crate::machine::MachineIdentity;
use crate::sign_check::SignPolicy;
use crate::sockets::desktop_socket_path;
use crate::ssh_backend::OfferCount;

/// The version of the LiveStatus JSON contract. Bumped only on a breaking change;
/// additive fields do not bump it. A mismatch is a hard reject in the client.
pub const STATUS_SCHEMA_VERSION: u32 = 1;

/// The provider order shown in the status report (matches Go's `statusNamespaces`).
/// `pub` so the bin-side renderer walks the same fixed order.
pub const STATUS_NAMESPACES: [&str; 3] = [NS_SSH_AGENT, NS_AWS_CREDENTIALS, NS_DOCKER_CREDENTIALS];

/// `LiveStatus` is the daemon's authoritative self-report, served over the
/// read-only status socket and rendered by `shed-host-agent status`. Field names
/// and order match `status_server.go:25-60` byte-for-byte.
#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct LiveStatus {
    pub schema: u32,
    pub version: String,
    pub pid: i64,
    pub started_at: String,
    pub written_at: String,
    pub config_path: String,
    pub policies: std::collections::BTreeMap<String, String>,
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    pub gate_namespaces: Vec<String>,
    pub approval_channel: ApprovalChannelStatus,
    pub servers: Vec<ServerHealth>,
    /// This machine's identity ([`crate::machine`]). Additive (schema stays 1): absent
    /// from an older daemon's snapshot, and absent when the identity could not be
    /// loaded, in which case [`Self::machine_error`] says why.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub machine: Option<MachineIdentity>,
    /// Why there is no [`Self::machine`]; empty when there is one.
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub machine_error: String,
    /// What this machine signs for a shed (plan 035 §3.5). Additive (schema stays 1):
    /// absent from an older daemon's snapshot.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub ssh: Option<SshSigningStatus>,
}

/// The `ssh` block of [`LiveStatus`] and of the desktop's `broker.status`: the SSH
/// backend's mode, the usernames a login may be signed for (`ssh.allow_users`), whether
/// an `ssh.offer_keys` filter is set, and — once a listing has happened — how many keys
/// the last listing offered to sheds of how many the backend had. The counts are absent
/// until then: the daemon lists local keys at start-up, but never asks a forwarded agent
/// until a shed asks.
#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct SshSigningStatus {
    pub mode: String,
    pub allow_users: Vec<String>,
    pub key_filter: bool,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub keys_offered: Option<usize>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub keys_available: Option<usize>,
}

impl SshSigningStatus {
    /// The block for a backend in `mode` under `policy`, with the last listing's
    /// `counts` (`None` before any).
    pub fn new(mode: &str, policy: &SignPolicy, counts: Option<OfferCount>) -> SshSigningStatus {
        SshSigningStatus {
            mode: mode.to_string(),
            allow_users: policy.allow_users().to_vec(),
            key_filter: policy.offer_keys().is_some(),
            keys_offered: counts.map(|c| c.offered),
            keys_available: counts.map(|c| c.available),
        }
    }
}

/// `ApprovalChannelStatus` describes the approval-channel socket and its current
/// consumer.
#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct ApprovalChannelStatus {
    pub socket_path: String,
    pub consumer_connected: bool,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub client_name: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub client_version: String,
}

/// `ServerHealth` is one watched server's connection state.
#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct ServerHealth {
    pub name: String,
    pub url: String,
    pub namespaces: Vec<NamespaceHealth>,
    /// Set when ANY namespace on this server is `not-source`: the server answers this
    /// user's credential requests through another machine, named here. Additive (schema
    /// stays 1) and absent otherwise, so an older reader and the existing goldens see
    /// nothing new.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub active_source: Option<ActiveSource>,
}

/// `NamespaceHealth` is one namespace subscription's state on a server.
#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct NamespaceHealth {
    pub namespace: String,
    pub state: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub last_error: String,
    pub since: String,
    /// The machine the server named when it refused this subscription as `not-source`
    /// (plan 035 §3.4); absent in every other state.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub active_source: Option<ActiveSource>,
}

/// The machine a server reports as the active credential source for this user — the
/// `source` object of its `403 SOURCE_NOT_BOUND` refusal, as the server sent it. Either
/// field may be empty when the server's body left it out.
#[derive(Debug, Clone, Default, PartialEq, Eq, Serialize, Deserialize)]
pub struct ActiveSource {
    #[serde(default)]
    pub machine_id: String,
    #[serde(default)]
    pub machine_name: String,
}

/// `build_live_status` snapshots the daemon's live self-report. `consumer` is the
/// desktop approval channel's current consumer identity (`Some((name, version))`
/// when an app is connected, `None` otherwise) — the desktop server's
/// `consumer_info()`, or always `None` in the headless build with no desktop server.
/// `servers` is the supervisor's per-server connection snapshot (`Supervisor::health()`),
/// populated in BOTH modes (single-server shows one entry, `name:""`). `machine` is the
/// identity loaded at start ([`crate::machine::load_or_create`]) or the reason there is
/// none. `ssh` is the signing block ([`SshSigningStatus`]).
#[allow(clippy::too_many_arguments)]
pub fn build_live_status(
    cfg: &HostAgentConfig,
    config_path: &str,
    started_at: &str,
    version: &str,
    consumer: Option<(String, String)>,
    servers: Vec<ServerHealth>,
    machine: &Result<MachineIdentity, String>,
    ssh: Option<SshSigningStatus>,
) -> LiveStatus {
    // Keyed by the fixed status/gate namespace order; the BTreeMap re-sorts on
    // insert, so the emitted JSON key order is identical regardless.
    let policies = STATUS_NAMESPACES
        .iter()
        .map(|&ns| (ns.to_string(), cfg.effective_policy(ns)))
        .collect();
    // Destructure the optional consumer identity once (clone-free) rather than
    // re-inspecting it per approval-channel field.
    let (consumer_connected, client_name, client_version) = match consumer {
        Some((name, version)) => (true, name, version),
        None => (false, String::new(), String::new()),
    };
    let (machine, machine_error) = match machine {
        Ok(identity) => (Some(identity.clone()), String::new()),
        Err(e) => (None, e.clone()),
    };
    LiveStatus {
        schema: STATUS_SCHEMA_VERSION,
        version: version.to_string(),
        pid: current_pid(),
        started_at: started_at.to_string(),
        written_at: now_rfc3339(),
        config_path: config_path.to_string(),
        policies,
        gate_namespaces: cfg.gate_namespaces(),
        approval_channel: ApprovalChannelStatus {
            socket_path: desktop_socket_path().to_string_lossy().into_owned(),
            consumer_connected,
            client_name,
            client_version,
        },
        servers,
        machine,
        machine_error,
        ssh,
    }
}

fn current_pid() -> i64 {
    // SAFETY: getpid() takes no arguments, touches no memory, and is always safe.
    unsafe { libc::getpid() as i64 }
}

/// `now_rfc3339` formats the current UTC time as RFC3339 with second precision
/// (`2006-01-02T15:04:05Z`), matching the Go daemon's
/// `time.Now().UTC().Format(time.RFC3339)`. No chrono dependency — a std-only
/// civil-date conversion.
pub fn now_rfc3339() -> String {
    rfc3339_utc(now_unix())
}

/// Current wall-clock time as whole Unix seconds (UTC). `pub` so the desktop server
/// can compute `now + approval_timeout` for `expires_at`.
pub fn now_unix() -> i64 {
    SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .map(|d| d.as_secs() as i64)
        .unwrap_or(0)
}

/// Convert a Unix timestamp (seconds) to an RFC3339 UTC string using Howard
/// Hinnant's days-from-civil inverse (epoch 1970-01-01). `pub` so the desktop server
/// can stamp `approval_request.expires_at` (= now + timeout) with the same formatter.
pub fn rfc3339_utc(unix_secs: i64) -> String {
    let days = unix_secs.div_euclid(86_400);
    let sod = unix_secs.rem_euclid(86_400);
    let (hour, minute, second) = (sod / 3600, (sod % 3600) / 60, sod % 60);

    let z = days + 719_468;
    let era = if z >= 0 { z } else { z - 146_096 } / 146_097;
    let doe = z - era * 146_097; // [0, 146096]
    let yoe = (doe - doe / 1460 + doe / 36_524 - doe / 146_096) / 365; // [0, 399]
    let year_civil = yoe + era * 400;
    let doy = doe - (365 * yoe + yoe / 4 - yoe / 100); // [0, 365]
    let mp = (5 * doy + 2) / 153; // [0, 11]
    let day = doy - (153 * mp + 2) / 5 + 1; // [1, 31]
    let month = if mp < 10 { mp + 3 } else { mp - 9 }; // [1, 12]
    let year = if month <= 2 {
        year_civil + 1
    } else {
        year_civil
    };

    format!("{year:04}-{month:02}-{day:02}T{hour:02}:{minute:02}:{second:02}Z")
}

/// Days from the civil date to the unix epoch (Howard Hinnant's `days_from_civil`;
/// the inverse of the civil-from-days math in [`rfc3339_utc`] above). Lives in this
/// always-on module so both `bootstrap` and `aws_backend` (always-on, bus-side) can
/// reuse one implementation without a cross-gate dependency.
pub(crate) fn days_from_civil(y: i64, m: u32, d: u32) -> i64 {
    let y = if m <= 2 { y - 1 } else { y };
    let era = if y >= 0 { y } else { y - 399 } / 400;
    let yoe = y - era * 400; // [0, 399]
    let mp = if m > 2 { m - 3 } else { m + 9 } as i64; // [0, 11]
    let doy = (153 * mp + 2) / 5 + d as i64 - 1; // [0, 365]
    let doe = yoe * 365 + yoe / 4 - yoe / 100 + doy; // [0, 146096]
    era * 146_097 + doe - 719_468
}

/// Parse an RFC3339 timestamp to unix seconds, matching what Go's `time.Time` collapses
/// to: `Ok(None)` for the zero time (`0001-01-01T00:00:00Z`), `Ok(Some(secs))` for a
/// valid instant (offset applied, sub-second **truncated** as Go's `Format(time.RFC3339)`
/// drops it), `Err(())` for a malformed non-empty value. The inverse of
/// [`rfc3339_utc`]; a round-trip against it is unit-pinned.
///
/// `pub(crate)` and homed in this always-on module so both the mint-bundle deserializer
/// (`bootstrap::de_expires_at`) and the always-on, bus-side egress consumer
/// (`egress::egress_audit_entry`) reuse one implementation without a cross-gate
/// dependency — mirroring how `days_from_civil` is shared. Go's
/// `d.Time.UTC().Format(RFC3339)` is reproduced exactly incl. the zero-time and
/// offset-normalize cases.
pub(crate) fn parse_rfc3339_to_unix(s: &str) -> Result<Option<i64>, ()> {
    // Layout: YYYY-MM-DDTHH:MM:SS[.frac](Z|±HH:MM). Split date/time on 'T'.
    let (date, time_and_zone) = s.split_once('T').ok_or(())?;
    let (y, mo, d) = {
        let mut it = date.split('-');
        let y: i64 = it.next().ok_or(())?.parse().map_err(|_| ())?;
        let mo: u32 = parse_fixed(it.next().ok_or(())?, 2)?;
        let d: u32 = parse_fixed(it.next().ok_or(())?, 2)?;
        if it.next().is_some() {
            return Err(());
        }
        (y, mo, d)
    };
    // Split the zone suffix off the time, then split off any fractional seconds.
    let (time_part, offset_secs) = split_zone(time_and_zone)?;
    let (hms, frac) = match time_part.split_once('.') {
        Some((h, f)) => (h, Some(f)),
        None => (time_part, None),
    };
    if let Some(f) = frac {
        // Sub-second digits are validated (Go's RFC3339Nano parse would) then dropped.
        if f.is_empty() || !f.bytes().all(|b| b.is_ascii_digit()) {
            return Err(());
        }
    }
    let (h, mi, se) = {
        let mut it = hms.split(':');
        let h: u32 = parse_fixed(it.next().ok_or(())?, 2)?;
        let mi: u32 = parse_fixed(it.next().ok_or(())?, 2)?;
        let se: u32 = parse_fixed(it.next().ok_or(())?, 2)?;
        if it.next().is_some() {
            return Err(());
        }
        (h, mi, se)
    };
    if !(1..=12).contains(&mo) || !(1..=31).contains(&d) || h > 23 || mi > 59 || se > 60 {
        return Err(());
    }
    // The Go zero time → None (Go's `IsZero()` → omitted `expires_at`).
    if y == 1 && mo == 1 && d == 1 && h == 0 && mi == 0 && se == 0 && offset_secs == 0 {
        return Ok(None);
    }
    let days = days_from_civil(y, mo, d);
    let secs = days * 86_400 + (h as i64) * 3_600 + (mi as i64) * 60 + (se as i64) - offset_secs;
    Ok(Some(secs))
}

/// Split an RFC3339 zone suffix (`Z` or `±HH:MM`) off the end, returning the remaining
/// time text and the offset in seconds to SUBTRACT to reach UTC.
fn split_zone(time_and_zone: &str) -> Result<(&str, i64), ()> {
    if let Some(rest) = time_and_zone.strip_suffix('Z') {
        return Ok((rest, 0));
    }
    // Find the last '+' or '-' that starts the offset (after the time, so index >= 1).
    let bytes = time_and_zone.as_bytes();
    let mut idx = None;
    for (i, &b) in bytes.iter().enumerate() {
        if (b == b'+' || b == b'-') && i > 0 {
            idx = Some(i);
        }
    }
    let i = idx.ok_or(())?;
    let (time_part, off) = time_and_zone.split_at(i);
    // A `+05:00` zone is 5h AHEAD of UTC, so 5h is SUBTRACTED from the local time to
    // reach UTC (positive value to subtract); `-05:00` adds (negative value).
    let sign = if off.as_bytes()[0] == b'+' { 1 } else { -1 };
    let off = &off[1..];
    let (oh, om) = off.split_once(':').ok_or(())?;
    let oh: i64 = parse_fixed::<u32>(oh, 2)? as i64;
    let om: i64 = parse_fixed::<u32>(om, 2)? as i64;
    if oh > 23 || om > 59 {
        return Err(());
    }
    Ok((time_part, sign * (oh * 3_600 + om * 60)))
}

/// Parse an exactly-`width`-digit unsigned field (RFC3339 fields are fixed-width).
fn parse_fixed<T: std::str::FromStr>(s: &str, width: usize) -> Result<T, ()> {
    if s.len() != width || !s.bytes().all(|b| b.is_ascii_digit()) {
        return Err(());
    }
    s.parse().map_err(|_| ())
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::collections::BTreeMap;

    fn policies(pairs: &[(&str, &str)]) -> BTreeMap<String, String> {
        pairs
            .iter()
            .map(|(k, v)| (k.to_string(), v.to_string()))
            .collect()
    }

    fn ns(name: &str, state: &str, last_error: &str) -> NamespaceHealth {
        NamespaceHealth {
            namespace: name.to_string(),
            state: state.to_string(),
            last_error: last_error.to_string(),
            since: String::new(),
            active_source: None,
        }
    }

    /// The not-source fields are additive: absent when unset (so an old reader and the
    /// recorded `servers[]` goldens see nothing new), present and round-tripping when set,
    /// and an older daemon's snapshot without them still decodes.
    #[test]
    fn active_source_fields_are_additive() {
        let src = ActiveSource {
            machine_id: "0123456789abcdef0123456789abcdef".into(),
            machine_name: "desk".into(),
        };
        let mut sv = ServerHealth {
            name: "a".into(),
            url: "http://a".into(),
            namespaces: vec![ns("ssh-agent", "connected", "")],
            active_source: None,
        };
        let plain = serde_json::to_value(&sv).unwrap();
        assert!(plain.get("active_source").is_none(), "{plain}");
        assert!(
            plain["namespaces"][0].get("active_source").is_none(),
            "{plain}"
        );

        sv.namespaces[0].state = "not-source".into();
        sv.namespaces[0].active_source = Some(src.clone());
        sv.active_source = Some(src.clone());
        let v = serde_json::to_value(&sv).unwrap();
        assert_eq!(v["active_source"]["machine_name"], "desk");
        assert_eq!(
            v["namespaces"][0]["active_source"]["machine_id"],
            "0123456789abcdef0123456789abcdef"
        );
        let back: ServerHealth = serde_json::from_value(v).unwrap();
        assert_eq!(back, sv);

        let old: ServerHealth = serde_json::from_value(serde_json::json!({
            "name": "a", "url": "http://a",
            "namespaces": [{"namespace": "ssh-agent", "state": "connected", "since": ""}]
        }))
        .unwrap();
        assert_eq!(old.active_source, None);
        assert_eq!(old.namespaces[0].active_source, None);
    }

    #[test]
    fn rfc3339_known_epochs() {
        assert_eq!(rfc3339_utc(0), "1970-01-01T00:00:00Z");
        // The Unix "billennium": 2001-09-09 01:46:40 UTC.
        assert_eq!(rfc3339_utc(1_000_000_000), "2001-09-09T01:46:40Z");
        assert_eq!(rfc3339_utc(1_700_000_000), "2023-11-14T22:13:20Z");
    }

    #[test]
    fn rfc3339_round_trip_against_renderer() {
        // parse_rfc3339_to_unix is the inverse of rfc3339_utc.
        for unix in [0i64, 1_893_456_000, 1_700_000_000, 253_402_300_799] {
            let s = rfc3339_utc(unix);
            assert_eq!(parse_rfc3339_to_unix(&s), Ok(Some(unix)), "round-trip {s}");
        }
    }

    #[test]
    fn rfc3339_offset_and_fraction() {
        // +05:00 offset normalizes to UTC; fractional seconds are truncated.
        assert_eq!(
            parse_rfc3339_to_unix("2030-01-01T05:00:00+05:00"),
            Ok(Some(1_893_456_000))
        );
        assert_eq!(
            parse_rfc3339_to_unix("2030-01-01T00:00:00.500Z"),
            Ok(Some(1_893_456_000))
        );
        assert_eq!(parse_rfc3339_to_unix("garbage"), Err(()));
        assert_eq!(parse_rfc3339_to_unix("2030-13-01T00:00:00Z"), Err(())); // bad month
                                                                            // The Go zero time collapses to None (omitted expires_at / absent egress ts).
        assert_eq!(parse_rfc3339_to_unix("0001-01-01T00:00:00Z"), Ok(None));
    }

    fn sample_status() -> LiveStatus {
        LiveStatus {
            schema: STATUS_SCHEMA_VERSION,
            version: "v9".to_string(),
            pid: 4242,
            started_at: "2026-06-11T00:00:00Z".to_string(),
            written_at: "2026-06-11T00:00:01Z".to_string(),
            config_path: "/opt/homebrew/etc/shed/extensions.yaml".to_string(),
            policies: policies(&[
                ("ssh-agent", "shed-desktop"),
                ("aws-credentials", "deny-all"),
                ("docker-credentials", "approve-all"),
            ]),
            gate_namespaces: vec!["ssh-agent".to_string()],
            approval_channel: ApprovalChannelStatus {
                socket_path: "/Users/x/Library/Application Support/shed/host-agent.sock"
                    .to_string(),
                consumer_connected: true,
                client_name: "ShedDesktop".to_string(),
                client_version: "1.2.0".to_string(),
            },
            servers: vec![ServerHealth {
                name: "mac".to_string(),
                url: "http://localhost:8080".to_string(),
                namespaces: vec![ns("ssh-agent", "connected", "")],
                active_source: None,
            }],
            machine: None,
            machine_error: String::new(),
            ssh: None,
        }
    }

    #[test]
    fn live_status_json_round_trips_with_expected_keys() {
        let ls = sample_status();
        let compact = serde_json::to_string(&ls).unwrap();
        let v: serde_json::Value = serde_json::from_str(&compact).unwrap();

        // Field names / casing / nesting match the Go contract.
        assert_eq!(v["schema"], 1);
        assert_eq!(v["pid"], 4242);
        assert_eq!(v["policies"]["ssh-agent"], "shed-desktop");
        assert_eq!(v["gate_namespaces"][0], "ssh-agent");
        assert_eq!(
            v["approval_channel"]["socket_path"],
            "/Users/x/Library/Application Support/shed/host-agent.sock"
        );
        assert_eq!(v["approval_channel"]["consumer_connected"], true);
        assert_eq!(v["approval_channel"]["client_version"], "1.2.0");
        assert_eq!(v["servers"][0]["namespaces"][0]["state"], "connected");
        // `since` has no omitempty → present even when empty.
        assert_eq!(v["servers"][0]["namespaces"][0]["since"], "");

        // Round-trips back to an identical struct.
        let back: LiveStatus = serde_json::from_str(&compact).unwrap();
        assert_eq!(back, ls);
    }

    #[test]
    fn empty_gate_namespaces_and_client_fields_are_omitted() {
        let ls = LiveStatus {
            schema: STATUS_SCHEMA_VERSION,
            version: "v1".to_string(),
            pid: 1,
            started_at: String::new(),
            written_at: String::new(),
            config_path: String::new(),
            policies: policies(&[("ssh-agent", "deny-all")]),
            gate_namespaces: vec![],
            approval_channel: ApprovalChannelStatus {
                socket_path: "/s".to_string(),
                consumer_connected: false,
                client_name: String::new(),
                client_version: String::new(),
            },
            servers: vec![],
            machine: None,
            machine_error: String::new(),
            ssh: None,
        };
        let compact = serde_json::to_string(&ls).unwrap();
        assert!(!compact.contains("gate_namespaces"), "{compact}");
        assert!(!compact.contains("client_name"), "{compact}");
        assert!(!compact.contains("client_version"), "{compact}");
        // Non-omitempty fields are still present.
        assert!(compact.contains("\"servers\":[]"), "{compact}");
        assert!(
            compact.contains("\"consumer_connected\":false"),
            "{compact}"
        );
    }

    /// Decode-side version skew, now in the OTHER direction (plan 022 S6,
    /// `charliek/shed#328`): `rc_hub` left the snapshot with the RC hub itself,
    /// so a new `status` CLI reading an OLD daemon's snapshot — which still
    /// carries the block — must render the rest instead of exiting 1. There is
    /// no `deny_unknown_fields` anywhere on this wire and this test is what
    /// keeps it that way.
    #[test]
    fn snapshot_carrying_a_retired_rc_hub_block_still_decodes() {
        let ls = LiveStatus {
            schema: STATUS_SCHEMA_VERSION,
            version: "v1".to_string(),
            pid: 1,
            started_at: String::new(),
            written_at: String::new(),
            config_path: String::new(),
            policies: policies(&[("ssh-agent", "deny-all")]),
            gate_namespaces: vec![],
            approval_channel: ApprovalChannelStatus {
                socket_path: "/s".to_string(),
                consumer_connected: false,
                client_name: String::new(),
                client_version: String::new(),
            },
            servers: vec![],
            machine: None,
            machine_error: String::new(),
            ssh: None,
        };
        let mut v: serde_json::Value = serde_json::to_value(&ls).unwrap();
        v.as_object_mut().unwrap().insert(
            "rc_hub".to_string(),
            serde_json::json!({"state": "listening", "addr": "127.0.0.1:1029"}),
        );
        let back: LiveStatus = serde_json::from_value(v).expect("old snapshot decodes");
        assert_eq!(back, ls);
    }

    /// The machine block is additive under schema 1: an older daemon's snapshot (no
    /// `machine`) still decodes, a new one round-trips, and an absent identity is
    /// reported as an error string rather than an empty block.
    #[test]
    fn machine_block_is_additive_and_round_trips() {
        let old = sample_status();
        let json = serde_json::to_string(&old).unwrap();
        assert!(!json.contains("machine"), "{json}");
        let back: LiveStatus = serde_json::from_str(&json).expect("old snapshot decodes");
        assert_eq!(back.machine, None);
        assert_eq!(back.machine_error, "");

        let mut ls = sample_status();
        ls.machine = Some(MachineIdentity {
            id: "0123456789abcdef0123456789abcdef".into(),
            name: "laptop".into(),
        });
        let v = serde_json::to_value(&ls).unwrap();
        assert_eq!(v["machine"]["id"], "0123456789abcdef0123456789abcdef");
        assert_eq!(v["machine"]["name"], "laptop");
        assert!(v.get("machine_error").is_none());
        let back: LiveStatus = serde_json::from_value(v).unwrap();
        assert_eq!(back, ls);
    }

    #[test]
    fn build_live_status_reports_the_machine_or_why_not() {
        let cfg = HostAgentConfig::parse("");
        let identity = MachineIdentity {
            id: "0123456789abcdef0123456789abcdef".into(),
            name: "laptop".into(),
        };
        let ok = build_live_status(
            &cfg,
            "/c",
            "",
            "v",
            None,
            vec![],
            &Ok(identity.clone()),
            None,
        );
        assert_eq!(ok.machine, Some(identity));
        assert_eq!(ok.machine_error, "");

        let err = build_live_status(
            &cfg,
            "/c",
            "",
            "v",
            None,
            vec![],
            &Err("no way".into()),
            None,
        );
        assert_eq!(err.machine, None);
        assert_eq!(err.machine_error, "no way");
        let v = serde_json::to_value(&err).unwrap();
        assert!(v.get("machine").is_none());
        assert_eq!(v["machine_error"], "no way");
    }

    /// The ssh signing block is additive under schema 1: an older snapshot (no `ssh`)
    /// still decodes; a new one round-trips; the counts are omitted until a listing has
    /// happened, and present (zeros included) after one.
    #[test]
    fn ssh_block_is_additive_and_round_trips() {
        let old = sample_status();
        let json = serde_json::to_string(&old).unwrap();
        assert!(!json.contains("\"ssh\""), "{json}");
        let back: LiveStatus = serde_json::from_str(&json).expect("old snapshot decodes");
        assert_eq!(back.ssh, None);

        let policy = SignPolicy::default();
        let mut ls = sample_status();
        ls.ssh = Some(SshSigningStatus::new("agent-forward", &policy, None));
        let v = serde_json::to_value(&ls).unwrap();
        assert_eq!(
            v["ssh"],
            serde_json::json!({"mode": "agent-forward", "allow_users": ["git"], "key_filter": false})
        );
        let back: LiveStatus = serde_json::from_value(v).unwrap();
        assert_eq!(back, ls);

        let mut filter = crate::sign_check::KeyFilter::new();
        filter.insert_key_blob(b"k");
        let policy = SignPolicy::new(["git", "deploy"], Some(filter));
        let counts = OfferCount {
            offered: 0,
            available: 3,
        };
        ls.ssh = Some(SshSigningStatus::new("local-keys", &policy, Some(counts)));
        let v = serde_json::to_value(&ls).unwrap();
        assert_eq!(
            v["ssh"],
            serde_json::json!({
                "mode": "local-keys",
                "allow_users": ["git", "deploy"],
                "key_filter": true,
                "keys_offered": 0,
                "keys_available": 3,
            })
        );
        let back: LiveStatus = serde_json::from_value(v).unwrap();
        assert_eq!(back, ls);
    }

    #[test]
    fn build_live_status_carries_the_ssh_block() {
        let cfg = HostAgentConfig::parse("");
        let ssh = SshSigningStatus::new("local-keys", &cfg.ssh_sign_policy(), None);
        let ls = build_live_status(
            &cfg,
            "/c",
            "",
            "v",
            None,
            vec![],
            &Err("x".into()),
            Some(ssh.clone()),
        );
        assert_eq!(ls.ssh, Some(ssh));
    }
}
