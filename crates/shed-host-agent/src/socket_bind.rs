//! The daemon-only socket **bind ceremony** — the half of the Go `sockets.go` that
//! touches the daemon's operational [`Log`](crate::Log) and actually creates the
//! listeners. Only the daemon binds sockets; an embedder never does. The socket
//! **path resolution** + **liveness probes** live in the crate-agnostic
//! [`shed_broker::sockets`] module (shared with the embedder's startup probe).

use std::io;
use std::os::unix::fs::{DirBuilderExt, FileTypeExt, PermissionsExt};
use std::path::Path;

use shed_broker::sockets::socket_is_live;

use crate::Log;

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
/// Go `prepareSocketPath`. The liveness check uses [`socket_is_live`] from the core.
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

// The rc-hub's loopback TCP bind is NOT here: it needs no ceremony (no path to
// prepare, no perms to set — the loopback interface is the trust boundary) and
// its EADDRINUSE is a bind-as-lock *signal* rather than a failure, so it lives
// with the FSM that acts on it, in `shed_broker::rc_hub::role`.

#[cfg(test)]
mod tests {
    use super::*;

    use std::os::unix::net::UnixListener;

    // An AF_UNIX bind path caps at ~104 bytes (macOS) / ~108 (Linux); pytest's
    // nested tmp tree blows past that, so bind under a short /tmp root — same
    // `sun_path`-cap rationale as the Go `shortDir` test helper.
    fn short_tmpdir() -> tempfile::TempDir {
        tempfile::Builder::new()
            .prefix("shed-so")
            .tempdir_in("/tmp")
            .expect("mkdtemp under /tmp")
    }

    // A genuinely-stale socket file: bind then drop the listener. std's UnixListener
    // does NOT unlink the path on drop, so the socket file survives with nothing
    // accepting — a connect to it fails fast (ECONNREFUSED).
    fn make_stale_socket(path: &Path) {
        let ln = UnixListener::bind(path).expect("bind stale socket");
        drop(ln);
        assert!(path.exists(), "stale socket file should remain after drop");
    }

    // --- prepare_socket_path three-way gate (Go `prepareSocketPath`) ---
    // The A1 live cell drives the dir-0700 + stale-rebind halves; the live-refuse and
    // non-socket-refuse halves have no clean equal wire consequence (op-log-only
    // refusal), so they are pinned here as units (named in the harness README).

    #[test]
    fn prepare_allows_absent_path() {
        let dir = short_tmpdir();
        prepare_socket_path(&dir.path().join("nope.sock")).expect("absent path is bindable");
    }

    #[test]
    fn prepare_removes_stale_socket() {
        let dir = short_tmpdir();
        let path = dir.path().join("stale.sock");
        make_stale_socket(&path);
        prepare_socket_path(&path).expect("stale socket should be removable");
        assert!(!path.exists(), "stale socket should have been removed");
    }

    #[test]
    fn prepare_refuses_live_socket() {
        let dir = short_tmpdir();
        let path = dir.path().join("live.sock");
        let _ln = UnixListener::bind(&path).unwrap(); // held live
        prepare_socket_path(&path).expect_err("must refuse to clobber a live socket");
        assert!(path.exists(), "live socket must be left intact");
    }

    #[test]
    fn prepare_refuses_non_socket_file() {
        let dir = short_tmpdir();
        let path = dir.path().join("regular");
        std::fs::write(&path, b"x").unwrap();
        prepare_socket_path(&path).expect_err("must refuse a non-socket file");
        assert!(path.exists(), "non-socket file must be left intact");
    }
}
