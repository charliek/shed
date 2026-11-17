//! The daemon-only status surfaces: the read-only status UDS **server**
//! (`serveStatusSocket`) and the `status` subcommand **client + text renderer**
//! (`runStatus`). Both are wire-compatible, field-for-field, with the Go
//! `status_server.go` / `status.go` (§4 of the wire catalog). The `LiveStatus`
//! snapshot **types** + builder + RFC3339 helpers live in the crate-agnostic
//! [`shed_broker::status`] module; only the daemon serves this socket.

use std::io::{Read, Write};
use std::path::{Path, PathBuf};
use std::sync::Arc;
use std::time::Duration;

use shed_broker::sockets::{connect_unix_timeout, status_socket_path};
use shed_broker::status::{
    ApprovalChannelStatus, LiveStatus, ServerHealth, SshSigningStatus, STATUS_NAMESPACES,
    STATUS_SCHEMA_VERSION,
};

// ---------------------------------------------------------------------------
// Status socket server (channel 4)
// ---------------------------------------------------------------------------
//
// The bind ceremony (owner-only `0700` dir, refuse-to-clobber-live prepare,
// `0600` socket) is shared with the desktop socket in `crate::socket_bind`
// (`bind_unix_socket`) — the daemon calls it with `"status socket"`.

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
    let sock = status_socket_path();
    // Bounded connect (2s) — the client-side analog of the daemon's 500ms live-probe.
    // Mirrors Go's `net.DialTimeout("unix", sock, 2*time.Second)` (`status.go:30`); a
    // wedged daemon can't hang the CLI forever. std `UnixStream` has no connect_timeout
    // and this path is sync (no runtime), so `connect_unix_timeout` bounds it.
    let mut conn = match connect_unix_timeout(&sock, Duration::from_secs(2)) {
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
    // This machine's identity; an older daemon's snapshot carries neither field and
    // prints no line.
    if let Some(m) = &ls.machine {
        let _ = writeln!(out, "machine:  {} ({})", m.name, m.id);
    } else if !ls.machine_error.is_empty() {
        let _ = writeln!(out, "machine:  unavailable: {}", ls.machine_error);
    }
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

    // What this machine signs for a shed (plan 035 §3.5); an older daemon's snapshot
    // has no block and prints nothing.
    if let Some(ssh) = &ls.ssh {
        render_ssh_signing(out, ssh);
        let _ = writeln!(out);
    }

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
        render_source_hint(out, sv, ls);
    }
}

/// The `SSH signing:` block: the backend mode, the usernames a login is signed for, and
/// the keys offered to sheds — `N offered` without a filter, `N of M offered` with one,
/// `not listed yet` before the first listing (a forwarded agent is not asked until a shed
/// asks).
fn render_ssh_signing(out: &mut dyn Write, ssh: &SshSigningStatus) {
    let users = if ssh.allow_users.is_empty() {
        "(none)".to_string()
    } else {
        ssh.allow_users.join(", ")
    };
    let filter = if ssh.key_filter {
        "(ssh.offer_keys)"
    } else {
        "(no ssh.offer_keys filter)"
    };
    let keys = match (ssh.keys_offered, ssh.keys_available) {
        (Some(offered), Some(available)) if ssh.key_filter => {
            format!("{offered} of {available} offered {filter}")
        }
        (Some(offered), _) => format!("{offered} offered {filter}"),
        (None, _) => format!("not listed yet {filter}"),
    };
    let _ = writeln!(out, "SSH signing:");
    let _ = writeln!(out, "  mode   {}", ssh.mode);
    let _ = writeln!(out, "  users  {users}");
    let _ = writeln!(out, "  keys   {keys}");
}

/// Under a server where any namespace is `not-source` (plan 035 §3.4): who the source is,
/// and the exact command that makes THIS machine the source. Nothing for any other
/// server, so an existing report is unchanged.
fn render_source_hint(out: &mut dyn Write, sv: &ServerHealth, ls: &LiveStatus) {
    let source = sv.active_source.clone().or_else(|| {
        sv.namespaces
            .iter()
            .find(|n| n.state == "not-source")
            .map(|n| n.active_source.clone().unwrap_or_default())
    });
    let Some(source) = source else {
        return;
    };
    let who = match (source.machine_name.is_empty(), source.machine_id.is_empty()) {
        (false, _) => source.machine_name.clone(),
        (true, false) => source.machine_id.clone(),
        (true, true) => "another machine".to_string(),
    };
    let _ = writeln!(
        out,
        "    this machine is not the credential source here; the active source is {who}"
    );
    if let Some(m) = &ls.machine {
        let _ = writeln!(
            out,
            "    to make this machine ({}) the source: {}",
            m.name,
            source_use_command(&sv.name, &m.id)
        );
    }
}

/// The command that makes this machine the source on `server` (empty: the unnamed
/// single-server target, no `-s`): `shed [-s <server>] source use -- <id>`. It names the
/// machine by its full id, never its name: two of a user's machines can share a name
/// (the same hostname), and the CLI refuses a name that matches more than one. The `--`
/// ends option parsing.
fn source_use_command(server: &str, machine_id: &str) -> String {
    let server = if server.is_empty() {
        String::new()
    } else {
        format!("-s {} ", shell_word(server))
    };
    format!("shed {server}source use -- {}", shell_word(machine_id))
}

/// `s` as one shell word: bare only when it is non-empty and made only of ASCII letters,
/// digits, `.`, `_` and `-`; otherwise single-quoted (an embedded `'` closed, escaped
/// and reopened). Anything else is quoted: `=` alone begins a zsh filename expansion
/// (`=sh` becomes `/usr/bin/sh`), and a short allow-list is easier to trust than a list
/// of what each shell treats specially.
fn shell_word(s: &str) -> String {
    let plain = !s.is_empty()
        && s.chars()
            .all(|c| c.is_ascii_alphanumeric() || matches!(c, '.' | '_' | '-'));
    if plain {
        s.to_string()
    } else {
        format!("'{}'", s.replace('\'', "'\\''"))
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
        // Waiting for another machine to stop being the source is not a fault, and nor
        // is standing by while the desktop app's embedded broker answers.
        "stopped" | "not-source" | "yielding" => "-",
        _ => "x",
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use shed_broker::status::{NamespaceHealth, ServerHealth};
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

    fn joined(lines: &[&str]) -> String {
        let mut s = lines.join("\n");
        s.push('\n');
        s
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
                active_source: None,
            }],
            machine: None,
            machine_error: String::new(),
            ssh: None,
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
                    active_source: None,
                },
                ServerHealth {
                    name: "mini2".to_string(),
                    url: "https://mini2:8443".to_string(),
                    namespaces: vec![],
                    active_source: None,
                },
            ],
            machine: None,
            machine_error: String::new(),
            ssh: None,
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

    /// The `SSH signing:` block sits between the policies and the approval channel, one
    /// line per keys state: counted without a filter, counted with one, not listed yet;
    /// an empty allow list reads `(none)`. A snapshot without the block (an older daemon,
    /// the cases above) prints nothing.
    #[test]
    fn render_status_ssh_signing_block_per_keys_state() {
        let ssh = |users: &[&str], filter: bool, counts: Option<(usize, usize)>| SshSigningStatus {
            mode: "agent-forward".to_string(),
            allow_users: users.iter().map(|u| u.to_string()).collect(),
            key_filter: filter,
            keys_offered: counts.map(|c| c.0),
            keys_available: counts.map(|c| c.1),
        };
        let cases = [
            (
                ssh(&["git"], false, Some((12, 12))),
                "  users  git",
                "  keys   12 offered (no ssh.offer_keys filter)",
            ),
            (
                ssh(&["git", "deploy"], true, Some((2, 12))),
                "  users  git, deploy",
                "  keys   2 of 12 offered (ssh.offer_keys)",
            ),
            (
                ssh(&["git"], false, None),
                "  users  git",
                "  keys   not listed yet (no ssh.offer_keys filter)",
            ),
            (
                ssh(&[], true, None),
                "  users  (none)",
                "  keys   not listed yet (ssh.offer_keys)",
            ),
        ];
        for (block, users, keys) in cases {
            let mut ls = sample_status();
            ls.ssh = Some(block);
            let mut out: Vec<u8> = Vec::new();
            render_status(&mut out, &ls);
            let got = String::from_utf8(out).unwrap();
            let lines: Vec<&str> = got.lines().collect();
            let at = lines.iter().position(|l| *l == "SSH signing:").expect(&got);
            assert_eq!(lines[at - 1], "", "{got}");
            assert_eq!(
                lines[at - 2],
                "  docker-credentials  approve-all   ",
                "{got}"
            );
            assert_eq!(
                &lines[at + 1..at + 6],
                [
                    "  mode   agent-forward",
                    users,
                    keys,
                    "",
                    "Approval channel:"
                ],
                "{got}"
            );
        }
    }

    #[test]
    fn render_status_shows_the_machine_after_started() {
        let mut ls = sample_status();
        ls.machine = Some(shed_broker::MachineIdentity {
            id: "0123456789abcdef0123456789abcdef".to_string(),
            name: "laptop (work)".to_string(),
        });
        let mut out: Vec<u8> = Vec::new();
        render_status(&mut out, &ls);
        let got = String::from_utf8(out).unwrap();
        let head: Vec<&str> = got.lines().take(5).collect();
        assert_eq!(
            head,
            [
                "shed-host-agent status (pid 4242, v9)",
                "config:   /opt/homebrew/etc/shed/extensions.yaml",
                "started:  2026-06-11T00:00:00Z",
                "machine:  laptop (work) (0123456789abcdef0123456789abcdef)",
                "",
            ]
        );
    }

    #[test]
    fn render_status_says_why_there_is_no_machine() {
        let mut ls = sample_status();
        ls.machine_error = "read machine identity /x/machine.json: Permission denied".into();
        let mut out: Vec<u8> = Vec::new();
        render_status(&mut out, &ls);
        let got = String::from_utf8(out).unwrap();
        assert_eq!(
            got.lines().nth(3),
            Some("machine:  unavailable: read machine identity /x/machine.json: Permission denied")
        );
    }

    /// A server with a `not-source` namespace reads as not the source: an idle `-` mark,
    /// the active machine in the row and in a hint, and the exact command that moves the
    /// source here — naming this machine by its full id (a name can be shared by two
    /// machines; the CLI refuses an ambiguous one), with the name on the same line so a
    /// person sees which machine it is. A `duplicate` namespace is a fault mark and adds
    /// no hint; a `yielding` one (the desktop app's embedded broker answers) is idle and
    /// adds no hint either.
    #[test]
    fn render_status_not_source_hint_names_the_command() {
        let src = shed_broker::ActiveSource {
            machine_id: "0123456789abcdef0123456789abcdef".into(),
            machine_name: "desk".into(),
        };
        let mut ls = sample_status();
        ls.machine = Some(shed_broker::MachineIdentity {
            id: "fedcba9876543210fedcba9876543210".into(),
            name: "laptop (work)".into(),
        });
        let mut not_source = ns(
            "ssh-agent",
            "not-source",
            "not the credential source for this server; the active source is desk (0123456789abcdef0123456789abcdef)",
        );
        not_source.active_source = Some(src.clone());
        ls.servers = vec![
            ServerHealth {
                name: "alpha".into(),
                url: "https://alpha.example:8443".into(),
                namespaces: vec![ns("docker-credentials", "connected", ""), not_source],
                active_source: Some(src),
            },
            ServerHealth {
                name: String::new(),
                url: "http://localhost:8080".into(),
                namespaces: vec![
                    ns("ssh-agent", "duplicate", "another process"),
                    ns("docker-credentials", "yielding", "the desktop app answers"),
                ],
                active_source: None,
            },
        ];
        let mut out: Vec<u8> = Vec::new();
        render_status(&mut out, &ls);
        let got = String::from_utf8(out).unwrap();
        let tail: Vec<&str> = got
            .lines()
            .skip_while(|l| !l.starts_with("Servers"))
            .collect();
        assert_eq!(
            tail,
            [
                "Servers (2):",
                "  alpha  (https://alpha.example:8443)",
                "    ok  docker-credentials  connected",
                "    -   ssh-agent           not-source: not the credential source for this server; the active source is desk (0123456789abcdef0123456789abcdef)",
                "    this machine is not the credential source here; the active source is desk",
                "    to make this machine (laptop (work)) the source: shed -s alpha source use -- fedcba9876543210fedcba9876543210",
                "  (default)  (http://localhost:8080)",
                "    x  ssh-agent           duplicate: another process",
                "    -  docker-credentials  yielding: the desktop app answers",
            ]
        );
    }

    #[test]
    fn shell_word_quotes_only_when_needed() {
        assert_eq!(shell_word("box-2.lan_x"), "box-2.lan_x");
        assert_eq!(shell_word("-rf"), "-rf");
        for (raw, want) in [
            ("", "''"),
            ("=sh", "'=sh'"),
            ("laptop (work)", "'laptop (work)'"),
            ("it's", "'it'\\''s'"),
            ("a$(id)b", "'a$(id)b'"),
            ("`id`", "'`id`'"),
            ("x;rm", "'x;rm'"),
            ("two\nlines", "'two\nlines'"),
            ("büro", "'büro'"),
            ("~root", "'~root'"),
            ("a@b:c/d", "'a@b:c/d'"),
        ] {
            assert_eq!(shell_word(raw), want, "{raw:?}");
        }
        assert_eq!(
            source_use_command("", "laptop"),
            "shed source use -- laptop"
        );
        assert_eq!(
            source_use_command("=sh", "-rf"),
            "shed -s '=sh' source use -- -rf"
        );
    }

    /// The printed command, run by a real shell, gives `shed` exactly the intended
    /// arguments — every awkward name and server arrives as ONE literal word, `--` before
    /// the name. Run under `sh` and, where installed, `bash` and `zsh` (where a bare
    /// `=sh` would have become `/usr/bin/sh`).
    #[test]
    fn source_use_command_round_trips_through_real_shells() {
        let words = [
            "=sh",
            "-rf",
            "--help",
            "laptop (work)",
            "it's here",
            "\"quoted\"",
            "a$(echo pwned)b",
            "`echo pwned`",
            "x; echo pwned",
            "two\nlines",
            "tab\there",
            "büro 日本",
            "~root",
            "*",
            "!!",
            "",
        ];
        let shells: Vec<&str> = ["/bin/sh", "/bin/bash", "/bin/zsh", "/usr/bin/zsh"]
            .into_iter()
            .filter(|p| std::path::Path::new(p).exists())
            .collect();
        assert!(!shells.is_empty());
        for shell in shells {
            for server in ["", "alpha", "=sh", "-x", "my server"] {
                for name in words {
                    let line = source_use_command(server, name);
                    // `shed` is a shell function printing each argument NUL-terminated.
                    let script = format!("shed() {{ printf '%s\\0' \"$@\"; }}\n{line}\n");
                    let out = std::process::Command::new(shell)
                        .arg("-c")
                        .arg(&script)
                        .output()
                        .unwrap();
                    assert!(out.status.success(), "{shell}: {line:?}: {out:?}");
                    let got: Vec<String> = String::from_utf8(out.stdout)
                        .unwrap()
                        .split_terminator('\0')
                        .map(str::to_string)
                        .collect();
                    let mut want: Vec<String> = Vec::new();
                    if !server.is_empty() {
                        want.extend(["-s".to_string(), server.to_string()]);
                    }
                    want.extend(["source", "use", "--", name].map(str::to_string));
                    assert_eq!(got, want, "{shell}: {line:?}");
                }
            }
        }
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
            machine: None,
            machine_error: String::new(),
            ssh: None,
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
}
