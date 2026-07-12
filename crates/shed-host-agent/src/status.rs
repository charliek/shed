//! The daemon's authoritative self-report (`LiveStatus`), the read-only status
//! UDS server, and the `status` subcommand client + text renderer. Ported to be
//! wire-compatible, field-for-field, with the Go `status_server.go` / `status.go`
//! (§4 of the wire catalog).

use std::io::{Read, Write};
use std::path::{Path, PathBuf};
use std::sync::Arc;
use std::time::{Duration, SystemTime, UNIX_EPOCH};

use serde::{Deserialize, Serialize};

use crate::config::{HostAgentConfig, NS_AWS_CREDENTIALS, NS_DOCKER_CREDENTIALS, NS_SSH_AGENT};
use crate::sockets::desktop_socket_path;

/// The version of the LiveStatus JSON contract. Bumped only on a breaking change;
/// additive fields do not bump it. A mismatch is a hard reject in the client.
pub const STATUS_SCHEMA_VERSION: u32 = 1;

/// The provider order shown in the status report (matches Go's `statusNamespaces`).
const STATUS_NAMESPACES: [&str; 3] = [NS_SSH_AGENT, NS_AWS_CREDENTIALS, NS_DOCKER_CREDENTIALS];

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
}

/// `NamespaceHealth` is one namespace subscription's state on a server.
#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct NamespaceHealth {
    pub namespace: String,
    pub state: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub last_error: String,
    pub since: String,
}

/// `build_live_status` snapshots the daemon's live self-report. `consumer` is the
/// desktop approval channel's current consumer identity (`Some((name, version))`
/// when an app is connected, `None` otherwise) — the desktop server's
/// `consumer_info()` in slice 1, or always `None` in the headless build with no
/// desktop server. There is still no supervisor, so `servers` is empty.
pub fn build_live_status(
    cfg: &HostAgentConfig,
    config_path: &str,
    started_at: &str,
    version: &str,
    consumer: Option<(String, String)>,
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
        servers: Vec::new(),
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

/// Current wall-clock time as whole Unix seconds (UTC). `pub(crate)` so the
/// desktop server can compute `now + approval_timeout` for `expires_at`.
pub(crate) fn now_unix() -> i64 {
    SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .map(|d| d.as_secs() as i64)
        .unwrap_or(0)
}

/// Convert a Unix timestamp (seconds) to an RFC3339 UTC string using Howard
/// Hinnant's days-from-civil inverse (epoch 1970-01-01). `pub(crate)` so the
/// desktop server can stamp `approval_request.expires_at` (= now + timeout) with
/// the same formatter.
pub(crate) fn rfc3339_utc(unix_secs: i64) -> String {
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
/// always-on module so both `bootstrap` (desktop-gated) and `aws_backend` (always-on,
/// bus-side) can reuse one implementation without a cross-gate dependency.
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
/// `pub(crate)` and homed in this always-on module (not the `desktop-forwarding`-gated
/// `bootstrap`) so both the mint-bundle deserializer (`bootstrap::de_expires_at`, gated)
/// and the always-on, bus-side egress consumer (`egress::egress_audit_entry`) reuse one
/// implementation without a cross-gate dependency — mirroring how `days_from_civil` is
/// shared. Go's `d.Time.UTC().Format(RFC3339)` is reproduced exactly incl. the zero-time
/// and offset-normalize cases.
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

// ---------------------------------------------------------------------------
// Status socket server (channel 4)
// ---------------------------------------------------------------------------
//
// The bind ceremony (owner-only `0700` dir, refuse-to-clobber-live prepare,
// `0600` socket) is shared with the desktop socket in `crate::sockets`
// (`bind_unix_socket`) — the daemon calls it with `"status socket"` here.

/// Serve a read-only status UDS: on each connection, write the current LiveStatus
/// JSON (5s write deadline) and close. `health` is called per connection to
/// snapshot live state. Serves until `shutdown` resolves, then unlinks the socket.
/// Mirrors the Go `serveStatusSocket`.
pub async fn serve_status_socket<F>(
    listener: tokio::net::UnixListener,
    path: PathBuf,
    health: Arc<dyn Fn() -> LiveStatus + Send + Sync>,
    shutdown: F,
) where
    F: std::future::Future<Output = ()>,
{
    tokio::pin!(shutdown);
    loop {
        tokio::select! {
            _ = &mut shutdown => break,
            accepted = listener.accept() => {
                match accepted {
                    Ok((conn, _addr)) => {
                        let health = Arc::clone(&health);
                        tokio::spawn(async move {
                            let status = health();
                            write_status(conn, &status).await;
                        });
                    }
                    // Listener closed / fatal accept error → stop serving.
                    Err(_) => break,
                }
            }
        }
    }
    // tokio's UnixListener does not unlink the socket file on drop; remove it so a
    // restart can rebind (the Go daemon's `ln.Close()` unlinks its own socket).
    let _ = std::fs::remove_file(&path);
}

async fn write_status(mut conn: tokio::net::UnixStream, status: &LiveStatus) {
    use tokio::io::AsyncWriteExt;
    let Ok(mut buf) = serde_json::to_vec(status) else {
        return;
    };
    buf.push(b'\n'); // match Go's json.Encoder trailing newline
    let _ = tokio::time::timeout(Duration::from_secs(5), conn.write_all(&buf)).await;
    // `conn` is dropped here → the connection is closed.
}

// ---------------------------------------------------------------------------
// `status` subcommand client (channel 5)
// ---------------------------------------------------------------------------

/// The three-line "not running" message written to stderr when nothing is
/// listening at the status socket (matches `status.go:32-34`).
fn not_running_message(sock: &Path) -> String {
    let mut s = String::new();
    s.push_str(&format!(
        "shed-host-agent is not running — nothing is listening at {}\n",
        sock.display()
    ));
    s.push_str("Start it (Homebrew): brew services start shed-host-agent\n");
    s.push_str("  or run it directly: shed-host-agent -config <path>\n");
    s
}

/// Query the running daemon over the read-only status socket and print its
/// authoritative self-report. Returns a process exit code: 0 on a report, 1 when
/// the agent isn't running or the payload can't be trusted. Mirrors the Go
/// `runStatus`.
pub fn run_status(json_out: bool, out: &mut dyn Write) -> i32 {
    let sock = crate::sockets::status_socket_path();
    let mut conn = match std::os::unix::net::UnixStream::connect(&sock) {
        Ok(c) => c,
        Err(_) => {
            eprint!("{}", not_running_message(&sock));
            return 1;
        }
    };
    let _ = conn.set_read_timeout(Some(Duration::from_secs(5)));

    let mut raw = Vec::new();
    if let Err(e) = conn.read_to_end(&mut raw) {
        eprintln!("status: reading from the agent: {e}");
        return 1;
    }
    let ls: LiveStatus = match serde_json::from_slice(&raw) {
        Ok(v) => v,
        Err(e) => {
            eprintln!("status: reading from the agent: {e}");
            return 1;
        }
    };

    // Schema bumps only on a breaking change, so any mismatch means the payload
    // can't be trusted: refuse rather than render a possibly-misleading report.
    if ls.schema != STATUS_SCHEMA_VERSION {
        eprintln!(
            "status: agent status schema is {}, this CLI expects {} (version skew — match the shed-host-agent binary and CLI versions)",
            ls.schema, STATUS_SCHEMA_VERSION
        );
        return 1;
    }

    if json_out {
        match serde_json::to_string_pretty(&ls) {
            Ok(s) => {
                let _ = writeln!(out, "{s}");
                0
            }
            Err(e) => {
                eprintln!("status: encode: {e}");
                1
            }
        }
    } else {
        render_status(out, &ls);
        0
    }
}

/// Write the human-readable self-report. Column layout matches Go's
/// `text/tabwriter` (minwidth 0, tabwidth 2, padding 2, padchar ' ', flags 0) —
/// see `render_status` in the Go `status.go`.
pub fn render_status(out: &mut dyn Write, ls: &LiveStatus) {
    let _ = writeln!(
        out,
        "shed-host-agent status (pid {}, {})",
        ls.pid, ls.version
    );
    if !ls.config_path.is_empty() {
        let _ = writeln!(out, "config:   {}", ls.config_path);
    }
    let _ = writeln!(out, "started:  {}", ls.started_at);
    let _ = writeln!(out);

    let gated: std::collections::HashSet<&str> =
        ls.gate_namespaces.iter().map(String::as_str).collect();
    let _ = writeln!(out, "Approval policies:");
    let mut rows = Vec::new();
    for ns in STATUS_NAMESPACES {
        let note = if gated.contains(ns) {
            "(decided in shed-desktop)"
        } else {
            ""
        };
        let policy = ls.policies.get(ns).map(String::as_str).unwrap_or("");
        rows.push(vec![
            format!("  {ns}"),
            policy.to_string(),
            note.to_string(),
        ]);
    }
    tabwrite(out, &rows);
    let _ = writeln!(out);

    let _ = writeln!(out, "Approval channel:");
    let _ = writeln!(out, "  socket    {}", ls.approval_channel.socket_path);
    if ls.approval_channel.consumer_connected {
        let _ = writeln!(
            out,
            "  consumer  connected{}",
            client_suffix(&ls.approval_channel)
        );
    } else {
        let _ = writeln!(
            out,
            "  consumer  none connected (shed-desktop-policy requests fail closed)"
        );
    }
    let _ = writeln!(out);

    let _ = writeln!(out, "Servers ({}):", ls.servers.len());
    if ls.servers.is_empty() {
        let _ = writeln!(out, "  (none being watched)");
        return;
    }
    for sv in &ls.servers {
        let _ = writeln!(out, "  {}  ({})", server_label(&sv.name), sv.url);
        if sv.namespaces.is_empty() {
            let _ = writeln!(out, "    (no subscriptions yet)");
            continue;
        }
        let mut srows = Vec::new();
        for ns in &sv.namespaces {
            let mut detail = ns.state.clone();
            if !ns.last_error.is_empty() {
                detail.push_str(": ");
                detail.push_str(&ns.last_error);
            }
            srows.push(vec![
                format!("    {}", conn_mark(&ns.state)),
                ns.namespace.clone(),
                detail,
            ]);
        }
        tabwrite(out, &srows);
    }
}

/// Render rows as aligned columns matching Go's `text/tabwriter` with padding 2
/// and padchar ' ': every column except the trailing one is left-padded with
/// spaces to (max cell width in that column) + 2; the trailing cell is written
/// as-is. Cell width is counted in Unicode scalar values (like tabwriter's runes).
fn tabwrite(out: &mut dyn Write, rows: &[Vec<String>]) {
    if rows.is_empty() {
        return;
    }
    let ncols = rows.iter().map(Vec::len).max().unwrap_or(0);
    let mut widths = vec![0usize; ncols];
    for r in rows {
        for (i, cell) in r.iter().enumerate() {
            // Only tab-terminated cells (every cell except the row's trailing one)
            // participate in column widths.
            if i + 1 < r.len() {
                widths[i] = widths[i].max(cell.chars().count());
            }
        }
    }
    for r in rows {
        let last = r.len().saturating_sub(1);
        let mut line = String::new();
        for (i, cell) in r.iter().enumerate() {
            line.push_str(cell);
            if i != last {
                let target = widths[i] + 2;
                for _ in 0..target.saturating_sub(cell.chars().count()) {
                    line.push(' ');
                }
            }
        }
        line.push('\n');
        let _ = out.write_all(line.as_bytes());
    }
}

/// The connected consumer's identity suffix, e.g. " (ShedDesktop 1.2.0)".
fn client_suffix(ac: &ApprovalChannelStatus) -> String {
    if ac.client_name.is_empty() {
        String::new()
    } else if ac.client_version.is_empty() {
        format!(" ({})", ac.client_name)
    } else {
        format!(" ({} {})", ac.client_name, ac.client_version)
    }
}

fn server_label(name: &str) -> &str {
    if name.is_empty() {
        "(default)"
    } else {
        name
    }
}

fn conn_mark(state: &str) -> &'static str {
    match state {
        "connected" => "ok",
        "stopped" => "-",
        _ => "x",
    }
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
        }
    }

    fn joined(lines: &[&str]) -> String {
        let mut s = lines.join("\n");
        s.push('\n');
        s
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

    #[test]
    fn not_running_message_is_exact_three_lines() {
        let msg = not_running_message(Path::new("/tmp/s.sock"));
        assert_eq!(
            msg,
            "shed-host-agent is not running — nothing is listening at /tmp/s.sock\n\
             Start it (Homebrew): brew services start shed-host-agent\n  \
             or run it directly: shed-host-agent -config <path>\n"
        );
    }

    #[test]
    fn run_status_not_running_returns_1() {
        let _guard = crate::env_lock();
        let dir = tempfile::tempdir().unwrap();
        std::env::set_var("SHED_HOST_AGENT_SOCKET_DIR", dir.path());
        let mut out: Vec<u8> = Vec::new();
        let code = run_status(false, &mut out);
        std::env::remove_var("SHED_HOST_AGENT_SOCKET_DIR");
        assert_eq!(code, 1);
        // The report goes to stderr; stdout stays empty on the not-running path.
        assert!(out.is_empty());
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
            }],
        }
    }

    #[test]
    fn render_status_matches_go_tabwriter_case_a() {
        let mut out: Vec<u8> = Vec::new();
        render_status(&mut out, &sample_status());
        let got = String::from_utf8(out).unwrap();
        let want = joined(&[
            "shed-host-agent status (pid 4242, v9)",
            "config:   /opt/homebrew/etc/shed/extensions.yaml",
            "started:  2026-06-11T00:00:00Z",
            "",
            "Approval policies:",
            "  ssh-agent           shed-desktop  (decided in shed-desktop)",
            "  aws-credentials     deny-all      ",
            "  docker-credentials  approve-all   ",
            "",
            "Approval channel:",
            "  socket    /Users/x/Library/Application Support/shed/host-agent.sock",
            "  consumer  connected (ShedDesktop 1.2.0)",
            "",
            "Servers (1):",
            "  mac  (http://localhost:8080)",
            "    ok  ssh-agent  connected",
        ]);
        assert_eq!(got, want);
    }

    #[test]
    fn render_status_matches_go_tabwriter_case_c() {
        let ls = LiveStatus {
            schema: STATUS_SCHEMA_VERSION,
            version: "v2".to_string(),
            pid: 55,
            started_at: "2026-01-02T03:04:05Z".to_string(),
            written_at: String::new(),
            config_path: String::new(),
            policies: policies(&[
                ("ssh-agent", "approve-all"),
                ("aws-credentials", "shed-desktop"),
                ("docker-credentials", "deny-all"),
            ]),
            gate_namespaces: vec!["aws-credentials".to_string()],
            approval_channel: ApprovalChannelStatus {
                socket_path: "/s.sock".to_string(),
                consumer_connected: true,
                client_name: "App".to_string(),
                client_version: String::new(),
            },
            servers: vec![
                ServerHealth {
                    name: String::new(),
                    url: "http://localhost:8080".to_string(),
                    namespaces: vec![
                        ns("ssh-agent", "connected", ""),
                        ns(
                            "aws-credentials",
                            "reconnecting",
                            "dial tcp: connection refused",
                        ),
                        ns("docker-credentials", "stopped", ""),
                    ],
                },
                ServerHealth {
                    name: "mini2".to_string(),
                    url: "https://mini2:8443".to_string(),
                    namespaces: vec![],
                },
            ],
        };
        let mut out: Vec<u8> = Vec::new();
        render_status(&mut out, &ls);
        let got = String::from_utf8(out).unwrap();
        let want = joined(&[
            "shed-host-agent status (pid 55, v2)",
            "started:  2026-01-02T03:04:05Z",
            "",
            "Approval policies:",
            "  ssh-agent           approve-all   ",
            "  aws-credentials     shed-desktop  (decided in shed-desktop)",
            "  docker-credentials  deny-all      ",
            "",
            "Approval channel:",
            "  socket    /s.sock",
            "  consumer  connected (App)",
            "",
            "Servers (2):",
            "  (default)  (http://localhost:8080)",
            "    ok  ssh-agent           connected",
            "    x   aws-credentials     reconnecting: dial tcp: connection refused",
            "    -   docker-credentials  stopped",
            "  mini2  (https://mini2:8443)",
            "    (no subscriptions yet)",
        ]);
        assert_eq!(got, want);
    }

    #[test]
    fn render_status_no_consumer_landmarks() {
        let ls = LiveStatus {
            schema: STATUS_SCHEMA_VERSION,
            version: "v1".to_string(),
            pid: 1,
            started_at: String::new(),
            written_at: String::new(),
            config_path: String::new(),
            policies: policies(&[
                ("ssh-agent", "deny-all"),
                ("aws-credentials", "deny-all"),
                ("docker-credentials", "deny-all"),
            ]),
            gate_namespaces: vec![],
            approval_channel: ApprovalChannelStatus {
                socket_path: "/x/host-agent.sock".to_string(),
                consumer_connected: false,
                client_name: String::new(),
                client_version: String::new(),
            },
            servers: vec![],
        };
        let mut out: Vec<u8> = Vec::new();
        render_status(&mut out, &ls);
        let got = String::from_utf8(out).unwrap();
        for landmark in [
            "shed-host-agent status (pid 1, v1)",
            "Approval policies:",
            "none connected",
            "fail closed",
            "Servers (0):",
            "(none being watched)",
        ] {
            assert!(got.contains(landmark), "missing {landmark:?} in:\n{got}");
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
}
