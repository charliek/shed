//! The SSH argv the porcelain shells out with, for both remote target flavors.
//!
//! **Sheds and machines are deliberately not the same shape.** A shed is reached
//! through shed-server's SSH daemon at an endpoint the shed CLI wrote into
//! `~/.shed/config.yaml`, with the server's host key pinned in
//! `~/.shed/known_hosts` and `StrictHostKeyChecking=yes` — that whole posture
//! already exists as [`shed_core::rc::ssh_argv`] / [`shed_core::terminal::ssh_command`]
//! and is reused verbatim, so `sx` and the desktop dial a shed identically.
//!
//! A **machine** is an ordinary SSH host the operator already manages. Its entry
//! may name a user, a port, a `known_hosts` file — or none of them, in which case
//! `sx` says nothing and lets `~/.ssh/config` and ssh's own defaults decide.
//! Forcing the shed posture onto a machine would break every host the operator
//! reaches through a jump host, an agent, or a per-host identity.

use shed_core::config::MachineEntry;
use shed_core::rc;
use shed_core::terminal;

use shed_app::backend::RcTarget;
use shed_core::rc_agents::shell_quote_always;

/// ssh `ConnectTimeout` for the non-interactive ops — bounds connection setup
/// only, not a hung remote command (same value + rationale as `RcService`).
pub const CONNECT_TIMEOUT_SECS: u32 = 10;

/// Non-interactive ssh to a machine, carrying `remote_argv` as one shell-quoted
/// command line.
///
/// Quoting is [`shell_quote_always`] — the verbatim port of Go's `shellQuote`,
/// the same function the ENGINE uses to build its inner commands — rather than
/// `shed-core`'s conditional `terminal::shell_quote`. One quoter everywhere means
/// a command line reads identically whoever built it, on a mixed Go/Rust fleet;
/// the cost is that a `--print` line is quoted even where it needn't be.
pub fn machine_argv(entry: &MachineEntry, remote_argv: &[String]) -> Vec<String> {
    let mut argv = vec![
        "ssh".to_string(),
        "-o".to_string(),
        "BatchMode=yes".to_string(),
    ];
    argv.extend(host_key_opts(entry));
    argv.push("-o".to_string());
    argv.push(format!("ConnectTimeout={CONNECT_TIMEOUT_SECS}"));
    argv.extend(port_opts(entry));
    argv.push(user_at_host(entry));
    argv.push("--".to_string());
    // The remote command line and a PRINTED one are the same string by
    // construction — one quoter everywhere is the whole point.
    argv.push(display_line(remote_argv));
    argv
}

/// Interactive (TTY) ssh to a machine — `sx attach`'s exec target. No
/// `BatchMode` (an attach may legitimately prompt for a passphrase) and no
/// `ConnectTimeout` (the session is long-lived).
///
/// The remote command is quoted EXACTLY as [`machine_argv`] quotes it. ssh joins
/// a multi-element command with spaces and hands the result to the remote login
/// shell, so bare argv here would let a metacharacter in any element escape into
/// that shell — the same hole [`machine_argv`] already closed. One quoter, both
/// directions.
pub fn machine_tty_argv(entry: &MachineEntry, remote_argv: &[String]) -> Vec<String> {
    let mut argv = vec!["ssh".to_string(), "-t".to_string()];
    argv.extend(host_key_opts(entry));
    argv.extend(port_opts(entry));
    argv.push(user_at_host(entry));
    argv.push("--".to_string());
    argv.push(display_line(remote_argv));
    argv
}

/// `ssh -N -L <local>:127.0.0.1:<remote> <machine>` — the background tunnel
/// `sx watch --on machine:<m>` runs the local hub client through. `-N` runs no
/// remote command; `ExitOnForwardFailure` turns a taken local port into an
/// immediate non-zero exit instead of a tunnel that silently forwards nothing.
pub fn machine_forward_argv(
    entry: &MachineEntry,
    local_port: u16,
    remote_port: u16,
) -> Vec<String> {
    let mut argv = vec![
        "ssh".to_string(),
        "-N".to_string(),
        "-o".to_string(),
        "BatchMode=yes".to_string(),
        "-o".to_string(),
        "ExitOnForwardFailure=yes".to_string(),
    ];
    argv.extend(host_key_opts(entry));
    argv.push("-o".to_string());
    argv.push(format!("ConnectTimeout={CONNECT_TIMEOUT_SECS}"));
    argv.extend(port_opts(entry));
    argv.push("-L".to_string());
    argv.push(format!("{local_port}:127.0.0.1:{remote_port}"));
    argv.push(user_at_host(entry));
    argv
}

/// Non-interactive ssh into a shed — [`shed_core::rc::ssh_argv`] verbatim (the
/// shed name is the SSH user; the server's key is pinned).
pub fn shed_argv(shed: &str, target: &RcTarget, remote_argv: &[String]) -> Vec<String> {
    rc::ssh_argv(
        shed,
        &target.ssh_host,
        target.ssh_port,
        &target.known_hosts,
        remote_argv,
        CONNECT_TIMEOUT_SECS,
    )
}

/// Interactive ssh into a shed, attaching `session` —
/// [`shed_core::terminal::ssh_command`] verbatim, so `sx attach --on shed:…`
/// lands exactly where the desktop's "open terminal" does.
pub fn shed_tty_argv(shed: &str, target: &RcTarget, session: &str) -> Vec<String> {
    terminal::ssh_command(
        shed,
        &target.ssh_host,
        target.ssh_port,
        &target.known_hosts,
        Some(session),
    )
    .argv
}

/// Render an argv as one copy-pasteable shell line — the `--print` output, the
/// summaries and errors, AND the single remote-command argument
/// [`machine_argv`] hands to ssh.
pub fn display_line(argv: &[String]) -> String {
    argv.iter()
        .map(|a| shell_quote_always(a))
        .collect::<Vec<_>>()
        .join(" ")
}

/// Pin the host key only when the entry asked for it — see the module doc.
fn host_key_opts(entry: &MachineEntry) -> Vec<String> {
    match entry.known_hosts.as_deref().filter(|s| !s.is_empty()) {
        Some(path) => vec![
            "-o".to_string(),
            "StrictHostKeyChecking=yes".to_string(),
            "-o".to_string(),
            format!("UserKnownHostsFile={path}"),
        ],
        None => Vec::new(),
    }
}

/// `-p <port>` only for a non-default port, so an entry that says nothing lets
/// `~/.ssh/config`'s own `Port` win.
fn port_opts(entry: &MachineEntry) -> Vec<String> {
    if entry.ssh_port == 22 {
        Vec::new()
    } else {
        vec!["-p".to_string(), entry.ssh_port.to_string()]
    }
}

fn user_at_host(entry: &MachineEntry) -> String {
    match entry.user.as_deref().filter(|s| !s.is_empty()) {
        Some(user) => format!("{user}@{}", entry.host),
        None => entry.host.clone(),
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn full() -> MachineEntry {
        MachineEntry {
            name: "mini2".into(),
            host: "mini2.local".into(),
            user: Some("charliek".into()),
            ssh_port: 2022,
            rc_bin: Some("/opt/bin/sx".into()),
            known_hosts: Some("/kh".into()),
        }
    }

    fn bare() -> MachineEntry {
        MachineEntry {
            name: "plain".into(),
            host: "plain".into(),
            ssh_port: 22,
            ..Default::default()
        }
    }

    #[test]
    fn a_full_machine_entry_pins_user_port_and_host_key() {
        let argv = machine_argv(&full(), &["sx".into(), "rc".into(), "list".into()]);
        assert_eq!(argv[0], "ssh");
        assert!(argv.contains(&"BatchMode=yes".to_string()));
        assert!(argv.contains(&"StrictHostKeyChecking=yes".to_string()));
        assert!(argv.contains(&"UserKnownHostsFile=/kh".to_string()));
        assert!(argv.windows(2).any(|w| w == ["-p", "2022"]));
        assert!(argv.contains(&"charliek@mini2.local".to_string()));
        // The remote command is one always-quoted line after the `--` terminator.
        assert_eq!(argv[argv.len() - 2], "--");
        assert_eq!(argv[argv.len() - 1], "'sx' 'rc' 'list'");
    }

    #[test]
    fn a_bare_machine_entry_says_nothing_ssh_config_can_decide() {
        let argv = machine_argv(&bare(), &["sx".into(), "rc".into(), "list".into()]);
        // No host-key override, no -p: ~/.ssh/config keeps its say.
        assert!(!argv.iter().any(|a| a.starts_with("UserKnownHostsFile")));
        assert!(!argv.contains(&"StrictHostKeyChecking=yes".to_string()));
        assert!(!argv.contains(&"-p".to_string()));
        assert!(argv.contains(&"plain".to_string()));
        assert!(!argv.iter().any(|a| a.contains('@')));
    }

    #[test]
    fn tty_argv_asks_for_a_terminal_and_never_batches() {
        let argv = machine_tty_argv(&full(), &["tmux".into(), "attach".into()]);
        assert_eq!(&argv[..2], ["ssh", "-t"]);
        assert!(!argv.contains(&"BatchMode=yes".to_string()));
        // …and carries the remote command as ONE always-quoted line after `--`,
        // exactly like the non-interactive builder.
        assert_eq!(argv[argv.len() - 2], "--");
        assert_eq!(argv[argv.len() - 1], "'tmux' 'attach'");
    }

    /// The asymmetry that used to exist here was a shell-injection hole: an
    /// element with a metacharacter reached the remote login shell unquoted.
    #[test]
    fn tty_argv_quotes_every_remote_element_like_the_batch_builder() {
        let evil = ["tmux".to_string(), "attach; touch /tmp/pwn".to_string()];
        let tty = machine_tty_argv(&full(), &evil);
        let batch = machine_argv(&full(), &evil);
        assert_eq!(tty.last().unwrap(), batch.last().unwrap());
        assert_eq!(tty.last().unwrap(), "'tmux' 'attach; touch /tmp/pwn'");
    }

    #[test]
    fn forward_argv_binds_the_loopback_hub_port_and_fails_loudly() {
        let argv = machine_forward_argv(&full(), 40123, 1029);
        assert!(argv.contains(&"-N".to_string()));
        assert!(argv.contains(&"ExitOnForwardFailure=yes".to_string()));
        assert!(argv.windows(2).any(|w| w == ["-L", "40123:127.0.0.1:1029"]));
        // The destination is last — nothing to run remotely.
        assert_eq!(argv.last().unwrap(), "charliek@mini2.local");
    }

    fn shed_target() -> RcTarget {
        RcTarget {
            server_name: "mini3".into(),
            ssh_host: "10.0.0.5".into(),
            ssh_port: 2222,
            known_hosts: "/Users/dev/.shed/known_hosts".into(),
        }
    }

    #[test]
    fn shed_argv_keeps_the_shed_posture_verbatim() {
        let argv = shed_argv(
            "web",
            &shed_target(),
            &["shed-ext-rc".into(), "list".into()],
        );
        assert_eq!(
            argv,
            shed_core::rc::ssh_argv(
                "web",
                "10.0.0.5",
                2222,
                "/Users/dev/.shed/known_hosts",
                &["shed-ext-rc".to_string(), "list".to_string()],
                CONNECT_TIMEOUT_SECS,
            )
        );
        // The shed name is the SSH user, and the host key is always pinned.
        assert!(argv.contains(&"web@10.0.0.5".to_string()));
        assert!(argv.contains(&"StrictHostKeyChecking=yes".to_string()));
    }

    #[test]
    fn shed_tty_argv_attaches_through_the_shared_terminal_builder() {
        let argv = shed_tty_argv("web", &shed_target(), "rc-abc234");
        assert_eq!(&argv[..2], ["ssh", "-t"]);
        assert_eq!(
            &argv[argv.len() - 4..],
            ["tmux", "attach", "-t", "rc-abc234"]
        );
    }

    #[test]
    fn display_line_is_copy_pasteable() {
        assert_eq!(
            display_line(&["ssh".into(), "a b".into()]),
            "'ssh' 'a b'",
            "always-quoted, like the engine's own transcripts"
        );
    }
}
