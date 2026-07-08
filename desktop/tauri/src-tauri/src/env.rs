//! Runtime configuration resolved from `SHED_TAURI_*` env vars, mirroring the
//! Swift `ShedBackend` hermeticity hooks so the pytest harness can point the Tauri
//! app at an in-process mock + a fixture config without touching real hosts.

use std::path::PathBuf;

#[derive(Debug, Clone)]
pub struct Env {
    /// `SHED_TAURI_TEST_MODE=1` — unlocks test-only behavior + echoed by `identify`.
    pub test_mode: bool,
    /// In test mode, every host client is pointed at this single mock base URL
    /// (`SHED_TAURI_MOCK_BASE_URL`). Echoed by `identify` so the harness can fail
    /// fast if a run isn't actually hermetic.
    pub mock_base_url: Option<String>,
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
        Self {
            test_mode,
            mock_base_url: var("SHED_TAURI_MOCK_BASE_URL"),
            config_path,
            socket_path: var("SHED_TAURI_SOCKET")
                .map(PathBuf::from)
                .unwrap_or_else(default_socket_path),
            host_agent_socket: var("SHED_TAURI_HOST_AGENT_SOCKET")
                .map(PathBuf::from)
                .unwrap_or_else(default_host_agent_socket),
        }
    }
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
