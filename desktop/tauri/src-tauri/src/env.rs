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
    /// TEST-ONLY machine-hub override: `SHED_TAURI_MACHINE_HUB_PORTS`, a
    /// comma-separated `<machine>=<port>` map. A listed machine's hub is reached
    /// on that loopback port directly instead of through an `ssh -N -L` forward.
    ///
    /// This is what makes the machine path testable HERMETICALLY: the harness
    /// stands up a fake `/v1` hub and the app reaches it through
    /// `shed_app::machine::FixedPort`, so the REAL `HubClient` and
    /// `MachineHubWatcher` run with no ssh, no remote host, and no network. The
    /// seam exists for shed-mobile (which supplies its own Dart-side forward),
    /// and it turns out a hermetic harness needs exactly the same thing.
    ///
    /// **Per-machine, not one port for all**, so a suite can serve a hub for one
    /// machine and point another at a dead port — which is how the everyday
    /// "asleep / off-network" state gets covered without any real machine.
    ///
    /// Non-empty in test mode means NO machine ever spawns ssh: an unlisted
    /// entry is treated as permanently unreachable rather than falling back to a
    /// real forward, so a hermetic run cannot leak an ssh child.
    ///
    /// Parsed only in test mode, like [`Self::mock_unreachable_hosts`] — a stray
    /// env var must never redirect a real machine's hub in production.
    pub machine_hub_ports: HashMap<String, u16>,
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
        // var can never point a real machine's hub somewhere else. A malformed
        // pair is dropped rather than failing the launch — the machine then reads
        // as unreachable, which is a visible, debuggable state.
        let machine_hub_ports = if test_mode {
            var("SHED_TAURI_MACHINE_HUB_PORTS")
                .map(|v| {
                    v.split(',')
                        .filter_map(|pair| {
                            let (name, port) = pair.split_once('=')?;
                            Some((name.trim().to_string(), port.trim().parse::<u16>().ok()?))
                        })
                        .collect()
                })
                .unwrap_or_default()
        } else {
            HashMap::new()
        };
        Self {
            test_mode,
            mock_base_url: var("SHED_TAURI_MOCK_BASE_URL"),
            mock_unreachable_hosts,
            machine_hub_ports,
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

fn current_uid() -> u32 {
    // getuid() is infallible and has no safety preconditions.
    unsafe { libc::getuid() }
}
