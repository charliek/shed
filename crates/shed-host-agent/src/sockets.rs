//! The agent's IPC sockets live at fixed, well-known paths — they are the
//! program's public interface (the shed-desktop app, `status`, and future tooling
//! all rendezvous here) so they are deliberately NOT configurable in the YAML
//! config. Only the directory is overridable via `SHED_HOST_AGENT_SOCKET_DIR` (an
//! escape hatch for tests and parallel dev agents). Faithful port of the Go
//! daemon's `sockets.go`.

use std::ffi::OsString;
use std::io;
use std::os::unix::fs::{DirBuilderExt, FileTypeExt, PermissionsExt};
use std::path::{Path, PathBuf};

use crate::config::user_home_dir;
use crate::Log;

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

// ---------------------------------------------------------------------------
// Shared bind ceremony (Go `bindUnixSocket`) — used by BOTH the status socket
// and the desktop approval socket. Trust is filesystem perms only: an owner-only
// `0700` parent dir plus a `0600` socket (there is NO peer-UID check, matching the
// Go server). `name` prefixes the operational log lines (e.g. "status socket",
// "desktop"); the operational log is not a differential target.
// ---------------------------------------------------------------------------

/// Bind a fresh blocking std listener at `path`, applying the shared safety
/// ceremony: create + restrict the parent dir to `0700`, refuse to clobber a live
/// or non-socket path (`prepare_socket_path`), bind, then `chmod 0600`. Dir/perm
/// failures are best-effort (logged, non-fatal); a refused or failed bind logs and
/// returns `None`. The std listener is later handed to tokio via
/// `UnixListener::from_std`, so this ceremony (and its logging) runs synchronously
/// before the runtime starts. Mirrors the Go `bindUnixSocket`.
pub(crate) fn bind_unix_socket(
    name: &str,
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
                "{name}: could not create socket dir {}: {e}",
                dir.display()
            ));
        }
        // Owner-only parent dir is the real protection (it covers the brief window
        // before the socket chmod); enforce it even if the dir already existed.
        if let Err(e) = std::fs::set_permissions(dir, std::fs::Permissions::from_mode(0o700)) {
            log.warn(&format!(
                "{name}: could not restrict socket dir perms {}: {e}",
                dir.display()
            ));
        }
    }
    if let Err(e) = prepare_socket_path(path) {
        log.error(&format!("{name}: refusing to bind {}: {e}", path.display()));
        return None;
    }
    let listener = match std::os::unix::net::UnixListener::bind(path) {
        Ok(l) => l,
        Err(e) => {
            log.error(&format!("{name}: failed to listen {}: {e}", path.display()));
            return None;
        }
    };
    if let Err(e) = std::fs::set_permissions(path, std::fs::Permissions::from_mode(0o600)) {
        log.warn(&format!(
            "{name}: could not set socket perms 0600 {}: {e}",
            path.display()
        ));
    }
    log.info(&format!("{name}: socket listening {}", path.display()));
    Some(listener)
}

/// Make `path` bindable for a fresh listener. Errors when the path is a non-socket
/// file (a misconfigured path must never delete an unrelated file) or when another
/// process is still accepting on it (clobbering a live socket would break the
/// running agent). A truly stale socket (nothing accepting) is removed. Mirrors the
/// Go `prepareSocketPath`.
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
/// immediately: it succeeds if a listener is bound, else fails fast (ECONNREFUSED /
/// ENOENT).
fn socket_is_live(path: &Path) -> bool {
    std::os::unix::net::UnixStream::connect(path).is_ok()
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
