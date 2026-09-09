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
    /// **The** directory the gx lane's LOCAL credential reader looks in for its
    /// discovery record and token — already resolved, so the lane layer makes no
    /// decision about it and reads no environment of its own.
    ///
    /// [`shed_gx::gx_home`]'s three candidates, applied here in its order:
    ///
    /// 1. `SHED_TAURI_GX_HOME`, the TEST-ONLY override. It is what lets the
    ///    harness seed a fixture home with a fake token and a fake record and
    ///    have the shipped reader — checks and all — find them. **Test mode
    ///    only**, and the outright winner when it is set: a stray var must never
    ///    point a production lane at somebody else's token.
    /// 2. the user's own `$GROK_HOME`, honoured only OUTSIDE test mode. A
    ///    hermetic run redirects `$HOME` to the harness runtime dir, but nothing
    ///    clears `$GROK_HOME`, so honouring an inherited one would let a cell
    ///    read the developer's real token.
    /// 3. `$HOME/.grok`.
    ///
    /// The gate over 1 and 2 is [`gx_homes`], which is where that promise is
    /// tested. Resolving the whole path here rather than passing the candidates
    /// down is what keeps this the ONLY place the three are weighed — and what
    /// keeps `$HOME` read beside every other env read in this file.
    pub gx_home: PathBuf,
    /// The gx adapter's windows, with `SHED_TAURI_GX_TIMINGS_MS` applied.
    ///
    /// A harness cannot wait out a thirty-second stall window, so
    /// [`shed_gx::GxTimings`] takes its windows as constructor options and this
    /// is where the app's come from. The var is a comma-separated
    /// `<field>=<milliseconds>` list over the four DURATION fields —
    /// `stall`, `resume_window`, `flush_after`, `down_after` — e.g.
    /// `stall=2000,flush_after=300,down_after=6000`. Anything unnamed keeps
    /// [`shed_gx::GxTimings::default`]'s value.
    ///
    /// **Test mode only**, and a malformed pair is DROPPED rather than fatal —
    /// [`Self::roost_sockets`]'s rule, for the same reason: a launch that dies
    /// on a typo is harder to debug than a lane whose window is visibly the
    /// default.
    pub gx_timings: shed_gx::GxTimings,
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
        // The gx credential seam's two test-mode knobs. Same rule as the two
        // above — parsed ONLY in test mode, so a stray `SHED_TAURI_GX_HOME` in
        // a developer's shell can never redirect a production lane's token read.
        // Resolved to one path right here: see [`Env::gx_home`].
        let (gx_seam, grok_home) = gx_homes(test_mode, var("SHED_TAURI_GX_HOME"), var("GROK_HOME"));
        let gx_home = shed_gx::gx_home(
            gx_seam.as_deref(),
            grok_home.as_deref(),
            &std::env::var_os("HOME")
                .map(PathBuf::from)
                .unwrap_or_default(),
        );
        let gx_timings = if test_mode {
            gx_timings_from(var("SHED_TAURI_GX_TIMINGS_MS").as_deref())
        } else {
            shed_gx::GxTimings::default()
        };
        Self {
            test_mode,
            mock_base_url: var("SHED_TAURI_MOCK_BASE_URL"),
            mock_unreachable_hosts,
            roost_sockets,
            gx_home,
            gx_timings,
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

/// Which of the two `$GROK_HOME` overrides survives, given the mode.
///
/// A pure function so the gate is TESTABLE rather than merely visible: it is the
/// whole of the hermeticity promise on [`Env::gx_home`], and "the app must not
/// read a real token in a hermetic run" is not a claim to leave to a reading of
/// `from_process`.
///
/// The two are mutually exclusive by construction. In test mode only the
/// harness's override is honoured and an inherited `GROK_HOME` is DROPPED — a
/// hermetic launch redirects `$HOME` but nothing clears `GROK_HOME`, so
/// honouring it would let a cell read the developer's own `~/.grok`. Outside
/// test mode it is the reverse: `SHED_TAURI_GX_HOME` is a test seam and must
/// never redirect a production lane's token read.
fn gx_homes(
    test_mode: bool,
    gx_home: Option<String>,
    grok_home: Option<String>,
) -> (Option<PathBuf>, Option<PathBuf>) {
    if test_mode {
        (gx_home.map(PathBuf::from), None)
    } else {
        (None, grok_home.map(PathBuf::from))
    }
}

/// Apply `SHED_TAURI_GX_TIMINGS_MS` to [`shed_gx::GxTimings::default`].
///
/// The syntax is `<field>=<milliseconds>`, comma separated, over the four
/// DURATION fields — the only ones a harness needs to shrink, and the only ones
/// whose unit the var's name can promise. `resume_tries`, `seed_limit` and
/// `rest_cap` are not durations and are deliberately not settable here: a knob
/// whose name says `_MS` should not silently accept a count.
///
/// **Every malformed pair is dropped, never fatal** — [`Env::roost_sockets`]'s
/// rule. An unknown field, a non-numeric value or a missing `=` leaves that
/// window at its default, which is a visible symptom (the cell waits out a real
/// window and times out) rather than an app that will not launch.
fn gx_timings_from(spec: Option<&str>) -> shed_gx::GxTimings {
    let mut timings = shed_gx::GxTimings::default();
    let Some(spec) = spec else {
        return timings;
    };
    for pair in spec.split(',') {
        let Some((field, millis)) = pair.split_once('=') else {
            continue;
        };
        let Ok(millis) = millis.trim().parse::<u64>() else {
            continue;
        };
        let value = std::time::Duration::from_millis(millis);
        match field.trim() {
            "stall" => timings.stall = value,
            "resume_window" => timings.resume_window = value,
            "flush_after" => timings.flush_after = value,
            "down_after" => timings.down_after = value,
            _ => {}
        }
    }
    timings
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

/// The uid the app runs as — what the gx token file's owner check compares
/// against ([`shed_gx::read_token_file`]), and what the IPC socket path falls
/// back to.
pub(crate) fn current_uid() -> u32 {
    // getuid() is infallible and has no safety preconditions.
    unsafe { libc::getuid() }
}

#[cfg(test)]
mod tests {
    use super::*;

    /// **A hermetic run cannot reach a real `~/.grok`.**
    ///
    /// The harness redirects `$HOME`, so the `~/.grok` fallback is already
    /// contained — but `$GROK_HOME` is an absolute path nobody clears, and it
    /// used to be read un-gated at the point of use. Honouring it in test mode
    /// would have let any cell that opens a gx lane read the developer's own
    /// token, which is exactly what `Env::gx_home`'s doc promises cannot happen.
    #[test]
    fn test_mode_drops_an_inherited_grok_home_and_production_drops_the_test_seam() {
        let (gx, grok) = gx_homes(
            true,
            Some("/fixture/home".into()),
            Some("/home/dev/.grok".into()),
        );
        assert_eq!(gx, Some(PathBuf::from("/fixture/home")));
        assert_eq!(
            grok, None,
            "an inherited GROK_HOME never reaches a test run"
        );

        // …and with no fixture home set, test mode still refuses it: the reader
        // falls through to `$HOME/.grok`, which the harness has redirected.
        let (gx, grok) = gx_homes(true, None, Some("/home/dev/.grok".into()));
        assert_eq!((gx, grok), (None, None));

        // Production is the mirror image: the test seam is inert, the user's own
        // var is honoured.
        let (gx, grok) = gx_homes(
            false,
            Some("/fixture/home".into()),
            Some("/home/dev/.grok".into()),
        );
        assert_eq!(gx, None, "the test seam never redirects a production lane");
        assert_eq!(grok, Some(PathBuf::from("/home/dev/.grok")));

        let (gx, grok) = gx_homes(false, None, None);
        assert_eq!((gx, grok), (None, None), "neither set: ~/.grok");
    }

    /// The gx window knob parses what it names and drops everything else, so a
    /// typo in a harness launch costs one defaulted window rather than the app.
    #[test]
    fn gx_timings_apply_the_named_windows_and_drop_the_rest() {
        let d = shed_gx::GxTimings::default();
        assert_eq!(gx_timings_from(None), d, "no var, no change");

        let t = gx_timings_from(Some(
            "stall=2000, resume_window=3000,flush_after=250,down_after=6000",
        ));
        assert_eq!(t.stall, std::time::Duration::from_millis(2000));
        assert_eq!(t.resume_window, std::time::Duration::from_millis(3000));
        assert_eq!(t.flush_after, std::time::Duration::from_millis(250));
        assert_eq!(t.down_after, std::time::Duration::from_millis(6000));
        // Untouched: the non-duration fields are not settable here.
        assert_eq!(t.resume_tries, d.resume_tries);
        assert_eq!(t.seed_limit, d.seed_limit);
        assert_eq!(t.rest_cap, d.rest_cap);

        // A bare word, an unknown field, a non-number, and a count field that
        // does not belong here: each is dropped on its own, and `stall` — the
        // one well-formed pair in the line — still lands.
        let t = gx_timings_from(Some(
            "nonsense,unknown=5,stall=1500,flush_after=soon,resume_tries=1",
        ));
        assert_eq!(t.stall, std::time::Duration::from_millis(1500));
        assert_eq!(t.flush_after, d.flush_after);
        assert_eq!(t.resume_tries, d.resume_tries);
    }
}
