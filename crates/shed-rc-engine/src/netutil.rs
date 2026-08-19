//! Loopback port allocation — a port of `internal/ext/rc/netutil.go`.

use std::net::TcpListener;

/// An unused TCP port on the loopback interface, for handing to a short-lived
/// child process (opencode's `--port`) that will bind it moments later
/// (`freeLoopbackPort`, `netutil.go:19`).
///
/// Implementation: bind an ephemeral listener on `127.0.0.1:0`, read back the
/// OS-assigned port, and drop the listener immediately so the caller's child is
/// free to bind it in turn.
///
/// This carries an inherent, **accepted** TOCTOU race: between this close and
/// opencode's own bind, another process could grab the port. The window is narrow
/// (opencode is exec'd by the `tmux new-session` call create issues right after
/// allocating) and the failure mode is benign and visible, not silent: a lost race
/// makes opencode's embedded HTTP server fail to bind, so opencode exits, which
/// surfaces as a dead RC session (the `exited_to_shell` classifier catches it) —
/// not a hang, and not a watcher silently attached to the wrong port.
///
/// A failure is **non-fatal** to create (Go: `ops.go:175-180` leaves the port at
/// 0, the session is created and usable, just not SSE-watchable).
pub fn free_loopback_port() -> std::io::Result<u16> {
    let listener = TcpListener::bind("127.0.0.1:0")?;
    let port = listener.local_addr()?.port();
    drop(listener); // best-effort; a close failure would not change the port
    Ok(port)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn allocates_a_usable_loopback_port() {
        let port = free_loopback_port().expect("loopback should be bindable");
        assert_ne!(port, 0, "0 is the wildcard, never a resolved port");
        // The listener really is released: the caller's child must be able to
        // bind the same port right after.
        TcpListener::bind(("127.0.0.1", port)).expect("port should be free again");
    }

    #[test]
    fn successive_allocations_are_distinct() {
        // Not a hard OS guarantee, but the ephemeral allocator does not hand out
        // the same port twice while the first listener is still open.
        let first = TcpListener::bind("127.0.0.1:0").unwrap();
        let second = free_loopback_port().unwrap();
        assert_ne!(first.local_addr().unwrap().port(), second);
    }
}
