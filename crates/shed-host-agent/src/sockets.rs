//! The agent's IPC sockets live at fixed, well-known paths — they are the
//! program's public interface (the shed-desktop app, `status`, and future tooling
//! all rendezvous here) so they are deliberately NOT configurable in the YAML
//! config. Only the directory is overridable via `SHED_HOST_AGENT_SOCKET_DIR` (an
//! escape hatch for tests and parallel dev agents). Faithful port of the Go
//! daemon's `sockets.go`.

use std::ffi::OsString;
use std::io;
use std::os::unix::ffi::OsStrExt;
use std::os::unix::fs::{DirBuilderExt, FileTypeExt, PermissionsExt};
use std::os::unix::io::{AsRawFd, FromRawFd, OwnedFd, RawFd};
use std::os::unix::net::UnixStream;
use std::path::{Path, PathBuf};
use std::time::{Duration, Instant};

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

/// Bounds the "is this socket live?" probe. Mirrors the Go daemon's
/// `socketProbeTimeout` (`desktop_server.go:23`, `500ms`): a live local agent
/// accepts in well under this; a stale leftover file fails fast (ECONNREFUSED).
const SOCKET_PROBE_TIMEOUT: Duration = Duration::from_millis(500);

/// Whether a Unix socket at `path` currently has a process accepting connections
/// (vs. a stale leftover file). A connect to a Unix stream socket resolves
/// immediately: it succeeds if a listener is bound, else fails fast (ECONNREFUSED /
/// ENOENT). The connect is bounded by [`SOCKET_PROBE_TIMEOUT`] so a pathological
/// peer with a full accept backlog cannot hang the probe (Go's
/// `net.DialTimeout("unix", path, 500ms)`).
fn socket_is_live(path: &Path) -> bool {
    connect_unix_timeout(path, SOCKET_PROBE_TIMEOUT).is_ok()
}

/// Connect to a Unix stream socket at `path`, bounding the connect to `timeout`.
///
/// Reproduces Go's `net.DialTimeout("unix", …)` bound — the daemon's live-socket
/// stale-probe uses `500ms` (`desktop_server.go:23,28`), the `status` client uses
/// `2s` (`status.go:30`). `std::os::unix::net::UnixStream` has **no**
/// `connect_timeout` (std offers it only for `TcpStream`), and both callers run
/// synchronously *before* any tokio runtime, so there is no async connect to await.
/// This bounds the connect with a `libc` nonblocking `connect(2)` + `poll(2)`:
/// create the socket, set `O_NONBLOCK`, `connect` (`EINPROGRESS` expected for a
/// not-yet-accepted peer), `poll(POLLOUT)` up to `timeout`, then check `SO_ERROR`.
///
/// On success the returned stream is restored to **blocking** mode — the `status`
/// client reads a response after connecting, and the stale-probe just drops it, so
/// blocking is the safe default for both. On timeout / refuse / any error → `Err`,
/// which both callers treat exactly like the current unreachable-peer path ("not
/// live" / "not running"). `cfg(unix)`; `libc` is already a direct dep.
pub(crate) fn connect_unix_timeout(path: &Path, timeout: Duration) -> io::Result<UnixStream> {
    let (addr, addr_len) = sockaddr_un(path)?;

    // SAFETY: socket() with constant domain/type/protocol args; returns an fd or -1.
    let fd = unsafe { libc::socket(libc::AF_UNIX, libc::SOCK_STREAM, 0) };
    if fd < 0 {
        return Err(io::Error::last_os_error());
    }
    // Own the fd immediately so every early return closes it (no leak on error).
    // SAFETY: `fd` is a fresh, exclusively-owned descriptor from socket() above.
    let owned = unsafe { OwnedFd::from_raw_fd(fd) };
    let raw = owned.as_raw_fd();

    set_nonblocking(raw, true)?;

    // SAFETY: `addr`/`addr_len` describe a valid AF_UNIX sockaddr built above; the
    // fd is a live AF_UNIX socket.
    let rc = unsafe {
        libc::connect(
            raw,
            &addr as *const libc::sockaddr_un as *const libc::sockaddr,
            addr_len,
        )
    };
    if rc != 0 {
        let err = io::Error::last_os_error();
        // Only EINPROGRESS means "connecting, poll for completion"; anything else
        // (ECONNREFUSED for a stale file, ENOENT for a missing path, …) is terminal.
        if err.raw_os_error() != Some(libc::EINPROGRESS) {
            return Err(err);
        }
        wait_writable(raw, timeout)?;
        // The connect result is reported via SO_ERROR once POLLOUT fires.
        if let Some(errno) = socket_error(raw)? {
            return Err(io::Error::from_raw_os_error(errno));
        }
    }

    set_nonblocking(raw, false)?; // restore blocking I/O for the caller
    Ok(UnixStream::from(owned))
}

/// Build a pathname `sockaddr_un` for `path`, returning it with the address length
/// to pass to `connect(2)`. Errors (`InvalidInput`) if the path is too long for
/// `sun_path` (mirrors the kernel's `sun_path` cap that `net.Dial` would surface).
fn sockaddr_un(path: &Path) -> io::Result<(libc::sockaddr_un, libc::socklen_t)> {
    // SAFETY: `sockaddr_un` is plain-old-data; an all-zero value is a valid start.
    let mut addr: libc::sockaddr_un = unsafe { std::mem::zeroed() };
    addr.sun_family = libc::AF_UNIX as libc::sa_family_t;
    let bytes = path.as_os_str().as_bytes();
    // Leave room for the trailing NUL that a pathname sun_path requires.
    if bytes.len() >= std::mem::size_of_val(&addr.sun_path) {
        return Err(io::Error::new(
            io::ErrorKind::InvalidInput,
            "socket path too long for sun_path",
        ));
    }
    // sun_path is `[c_char; N]` (c_char is i8) — copy the raw bytes in.
    // SAFETY: `bytes.len()` is checked to fit within sun_path above; src/dst don't
    // overlap (distinct allocations).
    unsafe {
        std::ptr::copy_nonoverlapping(
            bytes.as_ptr(),
            addr.sun_path.as_mut_ptr() as *mut u8,
            bytes.len(),
        );
    }
    Ok((addr, std::mem::size_of::<libc::sockaddr_un>() as libc::socklen_t))
}

/// Set or clear `O_NONBLOCK` on `fd`.
fn set_nonblocking(fd: RawFd, nonblocking: bool) -> io::Result<()> {
    // SAFETY: F_GETFL/F_SETFL take a valid fd and return the flags / -1 on error.
    let flags = unsafe { libc::fcntl(fd, libc::F_GETFL) };
    if flags < 0 {
        return Err(io::Error::last_os_error());
    }
    let new = if nonblocking {
        flags | libc::O_NONBLOCK
    } else {
        flags & !libc::O_NONBLOCK
    };
    // SAFETY: same fd, F_SETFL with the computed flag word.
    if unsafe { libc::fcntl(fd, libc::F_SETFL, new) } < 0 {
        return Err(io::Error::last_os_error());
    }
    Ok(())
}

/// Poll `fd` for writability (connect completion) up to `timeout`. `Err(TimedOut)`
/// when the deadline passes with no readiness (the bound Go's `DialTimeout` gives).
/// An `EINTR` re-poll uses the REMAINING time to an absolute deadline (not the full
/// `timeout` again), so a signal storm can't push the wait past the bound — matching
/// Go's absolute-deadline `DialTimeout` (CodeRabbit review).
fn wait_writable(fd: RawFd, timeout: Duration) -> io::Result<()> {
    let deadline = Instant::now() + timeout;
    loop {
        let remaining = deadline.saturating_duration_since(Instant::now());
        let ms = remaining.as_millis().min(libc::c_int::MAX as u128) as libc::c_int;
        let mut pfd = libc::pollfd {
            fd,
            events: libc::POLLOUT,
            revents: 0,
        };
        // SAFETY: a single valid pollfd, count 1, timeout in ms.
        let rc = unsafe { libc::poll(&mut pfd, 1, ms) };
        if rc < 0 {
            let err = io::Error::last_os_error();
            if err.raw_os_error() == Some(libc::EINTR) {
                continue; // interrupted; re-poll with the remaining deadline
            }
            return Err(err);
        }
        if rc == 0 {
            return Err(io::Error::new(io::ErrorKind::TimedOut, "connect timed out"));
        }
        return Ok(());
    }
}

/// Read the pending `SO_ERROR` on `fd` after a nonblocking connect completes:
/// `Ok(None)` on success, `Ok(Some(errno))` when the connect failed.
fn socket_error(fd: RawFd) -> io::Result<Option<i32>> {
    let mut err: libc::c_int = 0;
    let mut len = std::mem::size_of::<libc::c_int>() as libc::socklen_t;
    // SAFETY: valid fd; `err`/`len` are correctly-sized out params for SO_ERROR.
    let rc = unsafe {
        libc::getsockopt(
            fd,
            libc::SOL_SOCKET,
            libc::SO_ERROR,
            &mut err as *mut libc::c_int as *mut libc::c_void,
            &mut len,
        )
    };
    if rc < 0 {
        return Err(io::Error::last_os_error());
    }
    Ok((err != 0).then_some(err))
}

#[cfg(test)]
mod tests {
    use super::*;

    use std::os::unix::net::UnixListener;
    use std::time::Instant;

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

    #[test]
    fn connect_unix_timeout_connects_to_live_listener() {
        let dir = short_tmpdir();
        let path = dir.path().join("live.sock");
        let _ln = UnixListener::bind(&path).unwrap();
        // A listener is accepting → connect resolves within the bound.
        connect_unix_timeout(&path, Duration::from_secs(2)).expect("connect to live listener");
    }

    #[test]
    fn connect_unix_timeout_refuses_stale_fast() {
        let dir = short_tmpdir();
        let path = dir.path().join("stale.sock");
        make_stale_socket(&path);
        // Even with a generous bound the refuse returns essentially immediately;
        // assert it is far under the timeout so we know it isn't waiting it out.
        let start = Instant::now();
        let err = connect_unix_timeout(&path, Duration::from_secs(5))
            .expect_err("stale socket must refuse");
        assert!(
            start.elapsed() < Duration::from_secs(1),
            "stale refuse took {:?} (should be near-instant)",
            start.elapsed()
        );
        // ECONNREFUSED for a bound-but-unaccepted stale socket.
        assert_eq!(err.raw_os_error(), Some(libc::ECONNREFUSED), "{err}");
    }

    #[test]
    fn wait_writable_times_out_on_never_writable_fd() {
        // Direct coverage of the poll-timeout branch (the fast-refuse tests only hit
        // terminal errnos, never wait_writable's rc==0). A read-half pipe fd is never
        // POLLOUT-writable, so a short bound must return TimedOut promptly (and the
        // absolute-deadline retry keeps it near the bound).
        let mut fds = [0 as libc::c_int; 2];
        // SAFETY: a 2-element array for pipe(2).
        assert_eq!(unsafe { libc::pipe(fds.as_mut_ptr()) }, 0);
        let (read_fd, write_fd) = (fds[0], fds[1]);
        let start = Instant::now();
        let err = wait_writable(read_fd, Duration::from_millis(100)).expect_err("read end never POLLOUT");
        assert_eq!(err.kind(), io::ErrorKind::TimedOut);
        let elapsed = start.elapsed();
        assert!(
            (Duration::from_millis(80)..Duration::from_millis(600)).contains(&elapsed),
            "timed out in {elapsed:?} (expected ~100ms bound)"
        );
        // SAFETY: close both raw fds we opened.
        unsafe {
            libc::close(read_fd);
            libc::close(write_fd);
        }
    }

    #[test]
    fn connect_unix_timeout_missing_path_fails_fast() {
        let dir = short_tmpdir();
        let path = dir.path().join("absent.sock");
        let start = Instant::now();
        connect_unix_timeout(&path, Duration::from_secs(5))
            .expect_err("missing path must fail");
        assert!(start.elapsed() < Duration::from_secs(1));
    }

    #[test]
    fn connect_unix_timeout_rejects_overlong_path() {
        // A path longer than sun_path is rejected before any syscall.
        let path = PathBuf::from(format!("/tmp/{}.sock", "x".repeat(200)));
        let err = connect_unix_timeout(&path, Duration::from_secs(1)).unwrap_err();
        assert_eq!(err.kind(), io::ErrorKind::InvalidInput);
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
