//! The daemon's authoritative self-report (`LiveStatus`), the read-only status
//! UDS server, and the `status` subcommand client + text renderer. Ported to be
//! wire-compatible, field-for-field, with the Go `status_server.go` / `status.go`
//! (§4 of the wire catalog).

use std::io::{self, Read, Write};
use std::os::unix::fs::{DirBuilderExt, FileTypeExt, PermissionsExt};
use std::path::{Path, PathBuf};
use std::sync::Arc;
use std::time::{Duration, SystemTime, UNIX_EPOCH};

use serde::{Deserialize, Serialize};

use crate::config::{HostAgentConfig, NS_AWS_CREDENTIALS, NS_DOCKER_CREDENTIALS, NS_SSH_AGENT};
use crate::sockets::desktop_socket_path;
use crate::Log;

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

/// `build_live_status` snapshots the daemon's live self-report. In slice 0 there
/// is no desktop server yet (so the approval channel reports no consumer) and no
/// supervisor (so `servers` is empty).
pub fn build_live_status(
    cfg: &HostAgentConfig,
    config_path: &str,
    started_at: &str,
    version: &str,
) -> LiveStatus {
    // Keyed by the fixed status/gate namespace order; the BTreeMap re-sorts on
    // insert, so the emitted JSON key order is identical regardless.
    let policies = STATUS_NAMESPACES
        .iter()
        .map(|&ns| (ns.to_string(), cfg.effective_policy(ns)))
        .collect();
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
            consumer_connected: false,
            client_name: String::new(),
            client_version: String::new(),
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
    let secs = SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .map(|d| d.as_secs() as i64)
        .unwrap_or(0);
    rfc3339_utc(secs)
}

/// Convert a Unix timestamp (seconds) to an RFC3339 UTC string using Howard
/// Hinnant's days-from-civil inverse (epoch 1970-01-01).
fn rfc3339_utc(unix_secs: i64) -> String {
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

// ---------------------------------------------------------------------------
// Status socket server (channel 4)
// ---------------------------------------------------------------------------

/// `bind_status_listener` performs the socket ceremony (owner-only `0700` parent
/// dir, refuse-to-clobber-live prepare, `0600` socket) and returns a bound
/// **blocking** std listener, or `None` if the bind is refused/fails (logged).
/// Mirrors the Go `bindUnixSocket`. The std listener is later handed to tokio via
/// `UnixListener::from_std`, so the filesystem ceremony (and its logging) runs
/// synchronously before the runtime starts.
pub(crate) fn bind_status_listener(
    path: &Path,
    log: &mut Log,
) -> Option<std::os::unix::net::UnixListener> {
    if let Some(dir) = path.parent() {
        if let Err(e) = std::fs::DirBuilder::new()
            .recursive(true)
            .mode(0o700)
            .create(dir)
        {
            log.warn(&format!(
                "status socket: could not create socket dir {}: {e}",
                dir.display()
            ));
        }
        // Owner-only parent dir is the real protection; enforce it even if the dir
        // already existed.
        if let Err(e) = std::fs::set_permissions(dir, std::fs::Permissions::from_mode(0o700)) {
            log.warn(&format!(
                "status socket: could not restrict socket dir perms {}: {e}",
                dir.display()
            ));
        }
    }
    if let Err(e) = prepare_socket_path(path) {
        log.error(&format!(
            "status socket: refusing to bind {}: {e}",
            path.display()
        ));
        return None;
    }
    let listener = match std::os::unix::net::UnixListener::bind(path) {
        Ok(l) => l,
        Err(e) => {
            log.error(&format!(
                "status socket: failed to listen {}: {e}",
                path.display()
            ));
            return None;
        }
    };
    if let Err(e) = std::fs::set_permissions(path, std::fs::Permissions::from_mode(0o600)) {
        log.warn(&format!(
            "status socket: could not set socket perms 0600 {}: {e}",
            path.display()
        ));
    }
    log.info(&format!(
        "status socket: socket listening {}",
        path.display()
    ));
    Some(listener)
}

/// Make `path` bindable for a fresh listener. Errors when the path is a non-socket
/// file (a misconfigured path must never delete an unrelated file) or when another
/// process is still accepting on it (clobbering a live socket would break the
/// running agent). A truly stale socket (nothing accepting) is removed. Mirrors
/// the Go `prepareSocketPath`.
fn prepare_socket_path(path: &Path) -> io::Result<()> {
    match std::fs::symlink_metadata(path) {
        Err(e) if e.kind() == io::ErrorKind::NotFound => Ok(()),
        Err(e) => Err(e),
        Ok(meta) => {
            if !meta.file_type().is_socket() {
                return Err(io::Error::other(format!(
                    "path exists but is not a socket: {}",
                    path.display()
                )));
            }
            if socket_is_live(path) {
                return Err(io::Error::other(format!(
                    "socket already in use by another process: {}",
                    path.display()
                )));
            }
            std::fs::remove_file(path)
        }
    }
}

/// Whether a Unix socket at `path` currently has a process accepting connections
/// (vs. a stale leftover file). A connect to a Unix stream socket resolves
/// immediately: it succeeds if a listener is bound, else fails fast (ECONNREFUSED
/// / ENOENT).
fn socket_is_live(path: &Path) -> bool {
    std::os::unix::net::UnixStream::connect(path).is_ok()
}

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
