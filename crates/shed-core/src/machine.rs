//! **Machine targets** — reaching a native host that runs `sx` and hosts the RC
//! activity hub. The PURE half: how a machine is addressed, the SSH argv it is
//! reached with, and the RC argv prefix it is invoked through. No process is
//! spawned here and no socket is opened; that is the transport's job
//! (`shed_app::machine`), which differs per client.
//!
//! Graduated out of `crates/sx` in plan 012 (roadmap R4) at its second consumer.
//! Until then `machines:` lived in [`crate::config`] but was READ by nothing
//! except the `sx` porcelain, so the reach sat in the porcelain with it. The
//! desktop and mobile clients are the second and third consumers, and mobile in
//! particular can reach only this crate's pure surface — it runs SSH through
//! `dartssh2` on the Dart side and cannot spawn a child process at all — which
//! is exactly why the pure/transport line is drawn here rather than around a
//! single shared SSH implementation.
//!
//! **Sheds and machines are deliberately not the same shape.** A shed is reached
//! through shed-server's SSH daemon at an endpoint the shed CLI wrote into
//! `~/.shed/config.yaml`, with the server's host key pinned in
//! `~/.shed/known_hosts` and `StrictHostKeyChecking=yes` — that posture is
//! [`crate::rc::ssh_argv`] / [`crate::terminal::ssh_command`] and is reused
//! verbatim by every client, so they all dial a shed identically.
//!
//! A **machine** is an ordinary SSH host the operator already manages. Its entry
//! may name a user, a port, a `known_hosts` file — or none of them, in which case
//! we say nothing and let `~/.ssh/config` and ssh's own defaults decide. Forcing
//! the shed posture onto a machine would break every host the operator reaches
//! through a jump host, an agent, or a per-host identity.

use crate::config::{MachineEntry, ShedConfig};
use crate::rc_agents::shell_quote_always;

/// ssh `ConnectTimeout` for the non-interactive ops — bounds connection setup
/// only, never a hung remote command (same value + rationale as `RcService`).
pub const CONNECT_TIMEOUT_SECS: u32 = 10;

/// The binary a `machine:` target invokes when its entry names none — resolved
/// on the machine's non-login SSH `PATH`.
pub const DEFAULT_MACHINE_BIN: &str = "sx";

/// The `sx` namespace carrying the one-shot engine verbs.
const RC_NAMESPACE: &str = "rc";

/// Resolve a machine by name, or explain what is configured.
///
/// The error text is deliberately actionable in both directions: with machines
/// configured it names them (the usual cause is a typo), and with none at all it
/// points at the missing config section rather than listing an empty set.
pub fn resolve<'a>(config: &'a ShedConfig, name: &str) -> Result<&'a MachineEntry, String> {
    if let Some(entry) = config.machine(name) {
        return Ok(entry);
    }
    let known: Vec<&str> = config.machines.iter().map(|m| m.name.as_str()).collect();
    Err(if known.is_empty() {
        format!(
            "no machine {name:?} in the config: add a `machines:` section to \
             ~/.shed/config.yaml"
        )
    } else {
        format!(
            "no machine {name:?} in the config (have: {})",
            known.join(", ")
        )
    })
}

/// The RC argv prefix to invoke on a machine: `<bin> rc`.
///
/// `machines[].rc_bin` names WHERE the binary lives on that machine — an absolute
/// path when it is not on the non-login `PATH` an SSH exec sees — and the `rc`
/// namespace is always appended.
pub fn rc_prefix(entry: &MachineEntry) -> Vec<String> {
    vec![
        entry
            .rc_bin
            .as_deref()
            .unwrap_or(DEFAULT_MACHINE_BIN)
            .to_string(),
        RC_NAMESPACE.to_string(),
    ]
}

/// Non-interactive ssh to a machine, carrying `remote_argv` as one shell-quoted
/// command line.
///
/// Quoting is [`shell_quote_always`] — the verbatim port of Go's `shellQuote`,
/// the same function the ENGINE uses to build its inner commands — rather than
/// the conditional `terminal::shell_quote`. One quoter everywhere means a
/// command line reads identically whoever built it, on a mixed Go/Rust fleet;
/// the cost is that a printed line is quoted even where it needn't be.
pub fn ssh_argv(entry: &MachineEntry, remote_argv: &[String]) -> Vec<String> {
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

/// Interactive (TTY) ssh to a machine — an `attach`'s exec target. No
/// `BatchMode` (an attach may legitimately prompt for a passphrase) and no
/// `ConnectTimeout` (the session is long-lived).
///
/// The remote command is quoted EXACTLY as [`ssh_argv`] quotes it. ssh joins a
/// multi-element command with spaces and hands the result to the remote login
/// shell, so bare argv here would let a metacharacter in any element escape into
/// that shell — the same hole [`ssh_argv`] already closed. One quoter, both
/// directions.
pub fn tty_argv(entry: &MachineEntry, remote_argv: &[String]) -> Vec<String> {
    let mut argv = vec!["ssh".to_string(), "-t".to_string()];
    argv.extend(host_key_opts(entry));
    argv.extend(port_opts(entry));
    argv.push(user_at_host(entry));
    argv.push("--".to_string());
    argv.push(display_line(remote_argv));
    argv
}

/// `ssh -N -L <local>:127.0.0.1:<remote> <machine>` — the background tunnel a
/// client runs its hub reads through. `-N` runs no remote command;
/// `ExitOnForwardFailure` turns a taken local port into an immediate non-zero
/// exit instead of a tunnel that silently forwards nothing.
pub fn forward_argv(entry: &MachineEntry, local_port: u16, remote_port: u16) -> Vec<String> {
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

/// Render an argv as one copy-pasteable shell line — a `--print` output, a
/// summary, an error, AND the single remote-command argument [`ssh_argv`] hands
/// to ssh.
///
/// **This is the string a non-`ssh`-binary transport wants too.** A client that
/// speaks SSH itself (mobile's `dartssh2`) has no argv API — it sends one command
/// string the remote shell re-parses — so it should build that string HERE
/// rather than with its own quoter, or the two transports disagree about the
/// bytes on the wire for identical argv.
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

/// The `[user@]host` destination — no user when the entry names none, so
/// `~/.ssh/config` (or the current login) decides.
pub fn user_at_host(entry: &MachineEntry) -> String {
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
        let argv = ssh_argv(&full(), &["sx".into(), "rc".into(), "list".into()]);
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
        let argv = ssh_argv(&bare(), &["sx".into(), "rc".into(), "list".into()]);
        // No host-key override, no -p: ~/.ssh/config keeps its say.
        assert!(!argv.iter().any(|a| a.starts_with("UserKnownHostsFile")));
        assert!(!argv.contains(&"StrictHostKeyChecking=yes".to_string()));
        assert!(!argv.contains(&"-p".to_string()));
        assert!(argv.contains(&"plain".to_string()));
        assert!(!argv.iter().any(|a| a.contains('@')));
    }

    #[test]
    fn tty_argv_asks_for_a_terminal_and_never_batches() {
        let argv = tty_argv(&full(), &["tmux".into(), "attach".into()]);
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
        let tty = tty_argv(&full(), &evil);
        let batch = ssh_argv(&full(), &evil);
        assert_eq!(tty.last().unwrap(), batch.last().unwrap());
        assert_eq!(tty.last().unwrap(), "'tmux' 'attach; touch /tmp/pwn'");
    }

    #[test]
    fn forward_argv_binds_the_loopback_hub_port_and_fails_loudly() {
        let argv = forward_argv(&full(), 40123, crate::hub_client::HUB_PORT);
        assert!(argv.contains(&"-N".to_string()));
        assert!(argv.contains(&"ExitOnForwardFailure=yes".to_string()));
        assert!(argv.windows(2).any(|w| w == ["-L", "40123:127.0.0.1:1029"]));
        // The destination is last — nothing to run remotely.
        assert_eq!(argv.last().unwrap(), "charliek@mini2.local");
    }

    #[test]
    fn display_line_is_copy_pasteable() {
        assert_eq!(
            display_line(&["ssh".into(), "a b".into()]),
            "'ssh' 'a b'",
            "always-quoted, like the engine's own transcripts"
        );
    }

    fn config() -> ShedConfig {
        ShedConfig::parse(
            "\
machines:
    mini2:
        host: mini2.local
        rc_bin: /opt/homebrew/bin/sx
    plain: {}
",
        )
    }

    #[test]
    fn resolving_a_machine_reads_its_entry_and_its_rc_prefix() {
        let cfg = config();
        let entry = resolve(&cfg, "mini2").unwrap();
        assert_eq!(entry.host, "mini2.local");
        // An override says WHERE the binary lives; the `rc` namespace is still
        // appended.
        assert_eq!(
            rc_prefix(entry),
            vec!["/opt/homebrew/bin/sx".to_string(), "rc".to_string()]
        );

        let entry = resolve(&cfg, "plain").unwrap();
        assert_eq!(entry.host, "plain");
        assert_eq!(
            rc_prefix(entry),
            vec![DEFAULT_MACHINE_BIN.to_string(), "rc".to_string()]
        );
    }

    #[test]
    fn an_unknown_machine_names_the_configured_ones() {
        let err = resolve(&config(), "ghost").unwrap_err();
        assert!(err.contains("ghost"), "{err}");
        assert!(err.contains("mini2") && err.contains("plain"), "{err}");

        // With no machines at all the error points at the fix instead.
        let err = resolve(&ShedConfig::default(), "ghost").unwrap_err();
        assert!(err.contains("machines:"), "{err}");
    }
}
