"""Background-tunnel detach test for issue #223.

`shed tunnels start -d` is documented to run the tunnel as a detached
daemon: print success, return the terminal, and keep forwarding in a
separate process. Before the fix it blocked in the foreground exactly like
non-background mode. This is the live regression guard for the detach
behavior — the per-connection copy/teardown logic is unit-tested in
`internal/tunnels`.

The behavior under test is client-side and backend-independent; the shed is
only here to give the tunnel a real server + running shed to attach to. One
create-cycle, parameterized over the available backend like the rest of the
suite (it skips cleanly when a backend is unreachable).
"""

import os
import signal
import socket
import subprocess
import time

# A foreground (un-detached) `start` blocks until killed; a detached one
# returns as soon as the worker reports readiness (~0.1s observed). The budget
# is generous against CI variance while staying far below the subprocess
# timeout, so it cleanly separates "detached" from "blocked".
DETACH_RETURN_BUDGET_S = 10.0


def _free_local_port() -> int:
    s = socket.socket()
    try:
        s.bind(("127.0.0.1", 0))
        return s.getsockname()[1]
    finally:
        s.close()


def _pid_alive(pid: int) -> bool:
    try:
        os.kill(pid, 0)
    except ProcessLookupError:
        return False
    except PermissionError:
        return True
    return True


def test_tunnels_background_detaches(shed_server, test_shed_name):
    server = shed_server
    name = test_shed_name
    server.create(name)

    local_port = _free_local_port()
    pid = None
    try:
        t0 = time.monotonic()
        r = subprocess.run(
            ["shed", "-s", server.name, "tunnels", "start", name,
             "-t", f"{local_port}:22", "-d"],
            capture_output=True,
            text=True,
            timeout=30,
        )
        elapsed = time.monotonic() - t0
        assert r.returncode == 0, f"tunnels start -d failed: {r.stdout}\n{r.stderr}"
        # The defining symptom of the bug: -d blocked in the foreground.
        assert elapsed < DETACH_RETURN_BUDGET_S, (
            f"tunnels start -d took {elapsed:.1f}s; it did not detach"
        )

        # The readiness handshake guarantees the worker has bound its listener
        # and written state before the CLI returned, so these are race-free:
        # state records a separate, live daemon process (not the already-exited
        # CLI, not this test).
        listing = server.list_tunnels()
        assert name in listing, f"tunnel not listed after start: {listing}"
        pid = listing[name]["pid"]
        assert pid != os.getpid()
        assert _pid_alive(pid), f"daemon pid {pid} not alive after detach"

        # That daemon owns the local listener: connecting to it succeeds even
        # though the launching CLI has already returned.
        with socket.create_connection(("127.0.0.1", local_port), timeout=5):
            pass

        # `tunnels stop` tears the daemon down cleanly.
        stop = subprocess.run(
            ["shed", "-s", server.name, "tunnels", "stop", name],
            capture_output=True,
            text=True,
            timeout=15,
        )
        assert stop.returncode == 0, f"tunnels stop failed: {stop.stdout}\n{stop.stderr}"

        assert name not in server.list_tunnels()
        deadline = time.monotonic() + 10.0
        while _pid_alive(pid) and time.monotonic() < deadline:
            time.sleep(0.2)
        assert not _pid_alive(pid), "daemon survived `tunnels stop`"
    finally:
        # Best-effort: never leak the daemon, whichever assertion failed —
        # including the case where `stop` returned 0 but the process lingered.
        subprocess.run(
            ["shed", "-s", server.name, "tunnels", "stop", name],
            capture_output=True,
            text=True,
            timeout=15,
        )
        if pid is not None and _pid_alive(pid):
            try:
                os.kill(pid, signal.SIGTERM)
            except ProcessLookupError:
                pass
