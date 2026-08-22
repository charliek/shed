//! The SSH argv the porcelain shells out with, for both remote target flavors.
//!
//! **Sheds and machines are deliberately not the same shape**, and neither half
//! is defined here any more — this module is the porcelain's thin adapter over
//! two shared builders:
//!
//! - a **shed** is reached through shed-server's SSH daemon at an endpoint the
//!   shed CLI wrote into `~/.shed/config.yaml`, host key pinned in
//!   `~/.shed/known_hosts` — [`shed_core::rc::ssh_argv`] /
//!   [`shed_core::terminal::ssh_command`], reused verbatim so `sx` and the
//!   desktop dial a shed identically;
//! - a **machine** is an ordinary SSH host the operator already manages —
//!   [`shed_core::machine`], graduated out of this file in plan 012 (roadmap R4)
//!   when the desktop and mobile clients became its second and third consumers.
//!
//! The machine builders are re-exported rather than wrapped: `sx` was their only
//! caller for two plans, so keeping the `ssh::machine_*` call sites intact makes
//! the graduation a pure move at every use site, and the tests that pinned their
//! shapes now live with the code in `shed-core`.

pub use shed_core::machine::{
    display_line, ssh_argv as machine_argv, tty_argv as machine_tty_argv, CONNECT_TIMEOUT_SECS,
};

use shed_core::rc;
use shed_core::terminal;

use shed_app::backend::RcTarget;

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

#[cfg(test)]
mod tests {
    use super::*;

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

    /// The graduation is a pure re-export: a machine argv built through this
    /// module and one built through `shed_core::machine` are the same bytes.
    /// Pins that `sx` never grows a second machine posture.
    #[test]
    fn the_machine_builders_are_the_shared_ones() {
        let entry = shed_core::config::MachineEntry {
            name: "mini2".into(),
            host: "mini2.local".into(),
            user: Some("charliek".into()),
            ssh_port: 2022,
            rc_bin: None,
            known_hosts: Some("/kh".into()),
        };
        let argv = ["sx".to_string(), "rc".to_string(), "list".to_string()];
        assert_eq!(
            machine_argv(&entry, &argv),
            shed_core::machine::ssh_argv(&entry, &argv)
        );
        assert_eq!(
            machine_tty_argv(&entry, &argv),
            shed_core::machine::tty_argv(&entry, &argv)
        );
    }
}
