//! Runtime configuration resolved from `SHED_TAURI_*` env vars, mirroring the
//! Swift `ShedBackend` hermeticity hooks so the pytest harness can point the Tauri
//! app at an in-process mock + a fixture config without touching real hosts.

use std::collections::{HashMap, HashSet};
use std::path::PathBuf;

#[derive(Debug, Clone)]
pub struct Env {
    /// `SHED_TAURI_TEST_MODE=1` — unlocks test-only behavior + echoed by `identify`.
    pub test_mode: bool,
    /// In test mode, every host client is pointed at this single mock base URL
    /// (`SHED_TAURI_MOCK_BASE_URL`). Echoed by `identify` so the harness can fail
    /// fast if a run isn't actually hermetic.
    pub mock_base_url: Option<String>,
    /// TEST-ONLY per-host down simulation: the comma-separated server NAMES from
    /// `SHED_TAURI_MOCK_UNREACHABLE_HOSTS` (parsed only in test mode) that the
    /// backend points at a closed port instead of the mock, so the harness can
    /// exercise the per-host error row. Empty unless test mode + the var is set.
    pub mock_unreachable_hosts: HashSet<String>,
    /// The shed config to read (`SHED_TAURI_SHED_CONFIG`, else `~/.shed/config.yaml`).
    #[allow(dead_code)] // read by the shed-app backend in A1b
    pub config_path: PathBuf,
    /// The IPC socket path (`SHED_TAURI_SOCKET`, else `$XDG_RUNTIME_DIR/shed-tauri.sock`
    /// with a `/tmp/shed-tauri-<uid>/shed-tauri.sock` fallback — flat, no nested
    /// subdir, to stay under the macOS Unix-socket path limit).
    pub socket_path: PathBuf,
    /// The host-agent approval socket (`SHED_TAURI_HOST_AGENT_SOCKET` in tests →
    /// the fake agent; else the PLATFORM default — see [`default_host_agent_socket`]:
    /// macOS `~/Library/Application Support/shed`, Linux `$XDG_RUNTIME_DIR/shed` or
    /// `~/.local/share/shed`, both under `$SHED_HOST_AGENT_SOCKET_DIR` if set).
    pub host_agent_socket: PathBuf,
    /// TEST-ONLY roost-session override: `SHED_TAURI_ROOST_SOCKETS`, a
    /// comma-separated `<machine>=<socket path>` map. A named machine's
    /// `roost-session` is dialled on that Unix socket directly instead of through
    /// roost's SSH client-bridge. `localhost` (the implicit local host) goes
    /// through the same map.
    ///
    /// This is what makes the machine path testable HERMETICALLY: the harness
    /// stands up a fake `roost-session` on a socket and the app reaches it through
    /// `shed_app::roost::LocalSession`, so the REAL roost client and
    /// `RoostWatcher` run with no ssh, no remote host, and no network.
    ///
    /// **Per-machine, not one socket for all**, so a suite can serve a session for
    /// one machine and leave another unmapped — which is how the everyday "asleep
    /// / off-network" state gets covered without any real machine.
    ///
    /// Non-empty in test mode means NO machine ever spawns ssh: an unmapped entry
    /// is treated as permanently unreachable rather than falling back to a real
    /// bridge, so a hermetic run cannot leak an ssh child.
    ///
    /// Parsed only in test mode, like [`Self::mock_unreachable_hosts`] — a stray
    /// env var must never redirect a real machine's session in production.
    pub roost_sockets: HashMap<String, PathBuf>,
    /// TEST-ONLY `ssh` override: `SHED_TAURI_SSH_BIN`, the binary every roost
    /// transport execs instead of `ssh` (plan 019 §3.6).
    ///
    /// [`Self::roost_sockets`] swaps the transport out entirely, which is
    /// perfect for reading a session that is already there and useless for the
    /// bootstrap, whose whole subject is a host with NO session: the probe's
    /// answer comes from `ssh` exit codes and stderr, and its install runs
    /// roost's scripts through a remote `/bin/sh -s`. So a bootstrap cell keeps
    /// the REAL `SshBridge` and `SshExec` — roost's own classification, roost's
    /// own scripts, this app's own argv — and points them at a fake `ssh` that
    /// runs the far side locally under a throwaway `$HOME`.
    ///
    /// Test mode only, like every seam in this file — **and, unlike every other
    /// seam in this file, dead in a release build whatever the mode says**. See
    /// [`exec_seam`]: a stray `SHED_TAURI_SSH_BIN` in a developer's shell must
    /// never decide which binary a shipped app execs to reach somebody's
    /// machine, and `SHED_TAURI_TEST_MODE=1` is one more thing a shell can
    /// export.
    pub ssh_bin: Option<PathBuf>,
    /// TEST-ONLY: roost's `jail_fs_root` for the bootstrap machines
    /// (`SHED_TAURI_ROOST_JAIL=1`). **`false` in production**, always.
    ///
    /// roost's own bootstrap builders take this bool, and the delta it makes is
    /// exactly one thing: the candidate ladder's ABSOLUTE rungs
    /// (`/usr/bin/roost-session`, the linuxbrew and nix paths) gain a
    /// `${ROOST_BOOTSTRAP_FS_ROOT}` prefix the FAR SIDE expands. That is what
    /// lets a hermetic cell about a cold host be about a cold host: without it,
    /// a developer's own `/usr/bin/roost-session` — protocol 2 on this machine,
    /// absent in CI's container — answers the probe, and the same cell reads
    /// `Mismatch` here and `Missing` there.
    ///
    /// It steers which binary the far side execs, so it is gated exactly as
    /// [`shed_app::roost::SshBridgeOptions`] describes for `ROOST_TEST_MODE`:
    /// never read outside test mode — and never read in a release build either,
    /// for the reason on [`exec_seam`].
    pub roost_jail_fs_root: bool,
    /// The host-agent `extensions.yaml` the EMBEDDED broker loads (`SHED_TAURI_EXTENSIONS_CONFIG`,
    /// else the daemon default `~/.config/shed/extensions.yaml`). Only read in embedded /
    /// headless-coexist mode; external mode never touches it. The harness overrides it to
    /// keep an embedded-mode run hermetic.
    pub broker_extensions_path: PathBuf,
}

impl Env {
    pub fn from_process() -> Self {
        let var = |k: &str| std::env::var(k).ok().filter(|v| !v.is_empty());
        let test_mode = std::env::var("SHED_TAURI_TEST_MODE").as_deref() == Ok("1");
        // Hermeticity: in test mode, never fall back to the developer's real
        // ~/.shed/config.yaml — an unset config path loads an empty config.
        let config_path = var("SHED_TAURI_SHED_CONFIG")
            .map(PathBuf::from)
            .unwrap_or_else(|| {
                if test_mode {
                    PathBuf::new()
                } else {
                    default_config_path()
                }
            });
        // Only consulted in the mock arm; parse it only in test mode so a stray env
        // var can never affect a production run.
        let mock_unreachable_hosts = if test_mode {
            var("SHED_TAURI_MOCK_UNREACHABLE_HOSTS")
                .map(|v| {
                    v.split(',')
                        .map(str::trim)
                        .filter(|s| !s.is_empty())
                        .map(str::to_string)
                        .collect()
                })
                .unwrap_or_default()
        } else {
            HashSet::new()
        };
        // Same rule as the unreachable-hosts seam: test mode only, so a stray env
        // var can never point a real machine's session somewhere else. A malformed
        // pair is dropped rather than failing the launch — the machine then reads
        // as unreachable, which is a visible, debuggable state.
        let roost_sockets = if test_mode {
            var("SHED_TAURI_ROOST_SOCKETS")
                .map(|v| {
                    v.split(',')
                        .filter_map(|pair| {
                            let (name, socket) = pair.split_once('=')?;
                            let (name, socket) = (name.trim(), socket.trim());
                            if name.is_empty() || socket.is_empty() {
                                return None;
                            }
                            Some((name.to_string(), PathBuf::from(socket)))
                        })
                        .collect()
                })
                .unwrap_or_default()
        } else {
            HashMap::new()
        };
        // Both are test-mode-only AND debug-build-only for the reason on
        // [`exec_seam`]: each decides which binary the app (or the far side)
        // execs. The gates are [`exec_seam`] / [`jail_flag`], which is where
        // they are tested; the refusal lines they hand back are printed here,
        // because this is the one place that knows a process is starting.
        let (ssh_bin, ssh_refusal) = exec_seam(
            test_mode,
            RELEASE_BUILD,
            "SHED_TAURI_SSH_BIN",
            var("SHED_TAURI_SSH_BIN"),
        );
        let (roost_jail_fs_root, jail_refusal) = jail_flag(
            test_mode,
            RELEASE_BUILD,
            var("SHED_TAURI_ROOST_JAIL").as_deref(),
        );
        for refusal in [ssh_refusal, jail_refusal].into_iter().flatten() {
            eprintln!("{refusal}");
        }
        Self {
            test_mode,
            mock_base_url: var("SHED_TAURI_MOCK_BASE_URL"),
            mock_unreachable_hosts,
            roost_sockets,
            ssh_bin: ssh_bin.map(PathBuf::from),
            roost_jail_fs_root,
            config_path,
            socket_path: var("SHED_TAURI_SOCKET")
                .map(PathBuf::from)
                .unwrap_or_else(default_socket_path),
            host_agent_socket: var("SHED_TAURI_HOST_AGENT_SOCKET")
                .map(PathBuf::from)
                .unwrap_or_else(default_host_agent_socket),
            broker_extensions_path: var("SHED_TAURI_EXTENSIONS_CONFIG")
                .map(PathBuf::from)
                .unwrap_or_else(default_extensions_path),
        }
    }
}

impl Env {
    /// The transport choices the roost-host layer makes, bundled — see
    /// [`crate::machines::ReachOptions`].
    ///
    /// Built here rather than read there so this file stays the ONE place the
    /// process environment is consulted, which is what makes the test-mode gates
    /// above a property of the type rather than a convention.
    pub fn reach_options(&self) -> crate::machines::ReachOptions {
        crate::machines::ReachOptions {
            roost_sockets: self.roost_sockets.clone(),
            ssh_bin: self.ssh_bin.clone(),
            test_mode: self.test_mode,
        }
    }
}

/// A knob that exists only in test mode.
///
/// A pure function rather than an inline `if`: "a stray variable in a
/// developer's shell must never steer a shipped app" is a promise, and a
/// promise is worth a test. This one carries the two plan-019
/// seams, both of which decide **which binary gets exec'd** —
/// [`Env::ssh_bin`] locally and [`Env::roost_jail_fs_root`] on the far side.
fn test_only<T>(test_mode: bool, value: Option<T>) -> Option<T> {
    if test_mode {
        value
    } else {
        None
    }
}

/// **Is this a release build?** — the second gate's input, resolved once here so
/// [`exec_seam`] can take it as an argument and therefore be tested on BOTH
/// sides of it (`cfg!` is a compile-time constant, and a `cargo test` build is
/// always the debug side of it).
const RELEASE_BUILD: bool = !cfg!(debug_assertions);

/// A test seam that chooses **which program gets executed**, gated twice: test
/// mode ([`test_only`], plan 019 §3.6's pin) *and* a debug build.
///
/// The second gate is the point. `SHED_TAURI_TEST_MODE` is a pure runtime check,
/// so without it `SHED_TAURI_TEST_MODE=1 SHED_TAURI_SSH_BIN=/somewhere/ssh` on
/// the SHIPPED app is enough to make it exec `/somewhere/ssh` — a variable in an
/// environment deciding which binary runs with the user's ssh keys and the
/// user's `roost-session` install behind it. In a release build both seams are
/// therefore dead **whatever the mode says**, and the refusal is announced
/// rather than silent: a harness that somehow ran against a release binary must
/// read as a loud misconfiguration, not as a mysteriously real `ssh`.
///
/// **Only these two.** The other seams in this file — `SHED_TAURI_MOCK_BASE_URL`,
/// `SHED_TAURI_ROOST_SOCKETS`, `SHED_TAURI_SHED_CONFIG` —
/// redirect an HTTP base, a socket path or a config path: the worst they do is
/// point this process's own reads somewhere unhelpful. These two pick an
/// executable (locally, and on the far side of an ssh), which is the difference
/// that earns the stricter gate. Strictly stricter than §3.6's "only under
/// `SHED_TAURI_TEST_MODE`", so the pin still holds.
///
/// Returns the value and, when one was refused, the line to print. Pure, so the
/// gate AND its message are both testable; the printing lives at the one call
/// site in [`Env::from_process`].
fn exec_seam<T>(
    test_mode: bool,
    release_build: bool,
    name: &str,
    value: Option<T>,
) -> (Option<T>, Option<String>) {
    if release_build {
        // Announced whenever the variable is SET, test mode or not: what the user
        // needs told is "I saw this and ignored it", and which one.
        let refusal = value.is_some().then(|| {
            format!(
                "shed-desktop-tauri: ignoring {name} — a release build never lets \
                 an environment variable choose which program it execs"
            )
        });
        return (None, refusal);
    }
    (test_only(test_mode, value), None)
}

/// [`Env::roost_jail_fs_root`]: `1` in test mode, in a debug build, and nothing
/// else, ever.
///
/// Strictly `"1"` rather than "any non-empty value", so the variable cannot be
/// turned on by a shell that exports it as `0` or `false` to mean off.
fn jail_flag(test_mode: bool, release_build: bool, raw: Option<&str>) -> (bool, Option<String>) {
    let (value, refusal) = exec_seam(test_mode, release_build, "SHED_TAURI_ROOST_JAIL", raw);
    (value == Some("1"), refusal)
}

/// The embedded broker's `extensions.yaml`, matching where `shed-host-agent` reads it
/// (`~/.config/shed/extensions.yaml`). Pre-expanded off `$HOME` so the broker's own
/// tilde-expansion is a no-op; a missing file is not fatal here (the bridge synthesizes
/// the fresh-install default).
fn default_extensions_path() -> PathBuf {
    let home = std::env::var_os("HOME")
        .map(PathBuf::from)
        .unwrap_or_default();
    home.join(".config/shed/extensions.yaml")
}

/// The host agent's approval socket, matching where `shed-host-agent` (and the
/// Swift app, `ShedBackend`) place it PER PLATFORM: an explicit
/// `$SHED_HOST_AGENT_SOCKET_DIR` wins everywhere; else **macOS** uses the native
/// `~/Library/Application Support/shed`, and **Linux** the XDG convention
/// (`$XDG_RUNTIME_DIR/shed`, else `~/.local/share/shed`) — plus `host-agent.sock`.
///
/// The macOS branch is load-bearing: without it the mac app resolves the Linux
/// path (`~/.local/share/shed`), never reaches the agent that actually listens on
/// `~/Library/Application Support/shed/host-agent.sock`, and every secure server
/// then 401s (no control-token minting) with approvals silently unavailable.
fn default_host_agent_socket() -> PathBuf {
    let home = || {
        std::env::var_os("HOME")
            .map(PathBuf::from)
            .unwrap_or_default()
    };
    let dir = if let Some(explicit) = std::env::var_os("SHED_HOST_AGENT_SOCKET_DIR") {
        PathBuf::from(explicit)
    } else if cfg!(target_os = "macos") {
        home().join("Library/Application Support/shed")
    } else if let Some(xdg) = std::env::var_os("XDG_RUNTIME_DIR").filter(|x| !x.is_empty()) {
        PathBuf::from(xdg).join("shed")
    } else {
        home().join(".local/share/shed")
    };
    dir.join("host-agent.sock")
}

fn default_config_path() -> PathBuf {
    let home = std::env::var_os("HOME")
        .map(PathBuf::from)
        .unwrap_or_default();
    home.join(".shed/config.yaml")
}

/// `$XDG_RUNTIME_DIR/shed-tauri.sock`, falling back to `/tmp/shed-tauri-<uid>/
/// shed-tauri.sock` when `XDG_RUNTIME_DIR` is unset. Flat, no nested subdir: a
/// throwaway `XDG_RUNTIME_DIR` under macOS's long TMPDIR can otherwise overrun the
/// Unix-socket path limit (`SUN_LEN`, ~104 bytes).
fn default_socket_path() -> PathBuf {
    let dir = match std::env::var_os("XDG_RUNTIME_DIR") {
        Some(x) if !x.is_empty() => PathBuf::from(x),
        _ => PathBuf::from(format!("/tmp/shed-tauri-{}", current_uid())),
    };
    dir.join("shed-tauri.sock")
}

/// The uid the app runs as — what the IPC socket path falls back to.
pub(crate) fn current_uid() -> u32 {
    // getuid() is infallible and has no safety preconditions.
    unsafe { libc::getuid() }
}

#[cfg(test)]
mod tests {
    use super::*;

    /// **Neither plan-019 seam exists outside test mode** — and each of them
    /// decides which binary something execs, which is why they are gated at all
    /// (the fake `ssh` locally, and roost's jailed candidate ladder on the far
    /// side). A developer with either exported must get a shipped app that
    /// ignores both.
    #[test]
    fn the_bootstrap_seams_are_test_mode_only() {
        const DEBUG: bool = false; // the build the harness drives

        assert_eq!(
            test_only(true, Some("/fake/ssh")),
            Some("/fake/ssh"),
            "a harness run honours the fake ssh"
        );
        assert_eq!(
            test_only(false, Some("/fake/ssh")),
            None,
            "a shipped app execs `ssh`, whatever the shell says"
        );
        assert_eq!(test_only::<&str>(true, None), None);

        assert_eq!(
            exec_seam(true, DEBUG, "SHED_TAURI_SSH_BIN", Some("/fake/ssh")),
            (Some("/fake/ssh"), None),
            "a harness run honours the fake ssh"
        );
        assert_eq!(
            exec_seam(false, DEBUG, "SHED_TAURI_SSH_BIN", Some("/fake/ssh")),
            (None, None),
            "outside test mode a debug build execs `ssh`, silently"
        );

        assert_eq!(jail_flag(true, DEBUG, Some("1")), (true, None));
        assert_eq!(
            jail_flag(false, DEBUG, Some("1")),
            (false, None),
            "not outside test mode"
        );
        assert_eq!(jail_flag(true, DEBUG, None), (false, None));
        for off in ["0", "false", "", "yes", "true"] {
            assert_eq!(
                jail_flag(true, DEBUG, Some(off)),
                (false, None),
                "{off:?} is not how the jail is turned on"
            );
        }
    }

    /// **A RELEASE build ignores both exec-choosing seams even in test mode, and
    /// says so.**
    ///
    /// The gate above is a runtime check over a variable, and
    /// `SHED_TAURI_TEST_MODE=1` is one more variable a shell can export — so on
    /// its own it leaves `SHED_TAURI_TEST_MODE=1 SHED_TAURI_SSH_BIN=…` able to
    /// tell the SHIPPED app which binary to exec on the way to somebody's
    /// machine. [`exec_seam`]'s second gate is what closes that, and it is
    /// asserted here on both sides of `release_build` because `cfg!` is fixed at
    /// compile time and a `cargo test` build is always the debug side of it.
    ///
    /// Both legs of the harness drive a DEBUG binary
    /// (`tauri/src-tauri/target/debug/shed-desktop-tauri`, and
    /// `SHED_TAURI_BIN=/target/debug/…` in the Docker render gate), so this gate
    /// is invisible to it.
    #[test]
    fn a_release_build_ignores_the_exec_seams_whatever_the_mode_says() {
        const RELEASE: bool = true;

        for test_mode in [true, false] {
            let (value, refusal) =
                exec_seam(test_mode, RELEASE, "SHED_TAURI_SSH_BIN", Some("/fake/ssh"));
            assert_eq!(
                value, None,
                "a release build execs `ssh` even with test mode on"
            );
            let refusal = refusal.expect("the refusal is announced, not silent");
            assert!(
                refusal.contains("SHED_TAURI_SSH_BIN"),
                "the line names the variable it ignored: {refusal:?}"
            );

            let (jailed, refusal) = jail_flag(test_mode, RELEASE, Some("1"));
            assert!(!jailed, "and the jail is never on in a release build");
            let refusal = refusal.expect("the refusal is announced, not silent");
            assert!(
                refusal.contains("SHED_TAURI_ROOST_JAIL"),
                "the line names the variable it ignored: {refusal:?}"
            );
        }

        // Nothing set, nothing said: the line is about a variable that was SEEN.
        assert_eq!(
            exec_seam::<&str>(true, RELEASE, "SHED_TAURI_SSH_BIN", None),
            (None, None)
        );
        assert_eq!(jail_flag(true, RELEASE, None), (false, None));
    }

    /// **With the gate wired exactly as [`Env::from_process`] wires it, a harness
    /// run still gets its fake `ssh`.**
    ///
    /// The two tests above pass `release_build` by hand, which proves the gate
    /// and proves nothing about the constant the app feeds it. This one uses
    /// [`RELEASE_BUILD`] itself: a `cargo test` build is a debug build, so the
    /// seam must survive — a constant that went the other way would make every
    /// hermetic bootstrap cell silently exec the real `ssh`.
    #[test]
    fn a_test_build_is_the_debug_side_of_the_release_gate() {
        assert_eq!(
            exec_seam(true, RELEASE_BUILD, "SHED_TAURI_SSH_BIN", Some("/fake/ssh")),
            (Some("/fake/ssh"), None),
            "a test build honours the seam the harness sets"
        );
        assert_eq!(jail_flag(true, RELEASE_BUILD, Some("1")), (true, None));
    }
}
