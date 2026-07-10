//! The agent's IPC sockets live at fixed, well-known paths — they are the
//! program's public interface (the shed-desktop app, `status`, and future tooling
//! all rendezvous here) so they are deliberately NOT configurable in the YAML
//! config. Only the directory is overridable via `SHED_HOST_AGENT_SOCKET_DIR` (an
//! escape hatch for tests and parallel dev agents). Faithful port of the Go
//! daemon's `sockets.go`.

use std::ffi::OsString;
use std::path::PathBuf;

use crate::config::user_home_dir;

/// `socket_dir` returns the fixed directory the agent's sockets live in. Order:
/// the `SHED_HOST_AGENT_SOCKET_DIR` override, else macOS
/// `~/Library/Application Support/shed`, else `$XDG_RUNTIME_DIR/shed`, else
/// `~/.local/share/shed`.
pub fn socket_dir() -> PathBuf {
    if let Some(d) = env_nonempty("SHED_HOST_AGENT_SOCKET_DIR") {
        return PathBuf::from(d);
    }
    if cfg!(target_os = "macos") {
        return user_home_dir()
            .join("Library")
            .join("Application Support")
            .join("shed");
    }
    // Future Linux hosts: the user runtime dir, else a stable home path.
    if let Some(d) = env_nonempty("XDG_RUNTIME_DIR") {
        return PathBuf::from(d).join("shed");
    }
    user_home_dir().join(".local").join("share").join("shed")
}

/// `desktop_socket_path` is the stateful approval channel: a single consumer
/// (normally the shed-desktop app) receiving the audit/event stream and deciding
/// shed-desktop-policy approvals.
pub fn desktop_socket_path() -> PathBuf {
    socket_dir().join("host-agent.sock")
}

/// `status_socket_path` is the read-only status socket: any client gets the
/// daemon's LiveStatus JSON self-report per connection.
pub fn status_socket_path() -> PathBuf {
    socket_dir().join("host-agent-status.sock")
}

/// The value of an env var, or `None` when unset or empty (mirrors Go's
/// `os.Getenv(x) != ""`). Uses the OS string so a non-UTF-8 path still works.
fn env_nonempty(key: &str) -> Option<OsString> {
    match std::env::var_os(key) {
        Some(v) if !v.is_empty() => Some(v),
        _ => None,
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn socket_dir_honors_env_override() {
        let _guard = crate::env_lock();
        std::env::set_var("SHED_HOST_AGENT_SOCKET_DIR", "/custom/shed/dir");
        assert_eq!(socket_dir(), PathBuf::from("/custom/shed/dir"));
        assert_eq!(
            desktop_socket_path(),
            PathBuf::from("/custom/shed/dir/host-agent.sock")
        );
        assert_eq!(
            status_socket_path(),
            PathBuf::from("/custom/shed/dir/host-agent-status.sock")
        );
        std::env::remove_var("SHED_HOST_AGENT_SOCKET_DIR");
    }
}
