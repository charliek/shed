"""A fake shed-desktop app for the surface-A differential tests.

Drives the desktop approval channel (`<socketDir>/host-agent.sock`, the always-on
protocol-v2 NDJSON UDS server) exactly as the real shed-desktop app would: connect,
send a `hello`, and read the newline-delimited typed frames the agent sends back
(`hello_ack`, `approval_request`, `event`, `ping`, `token.response`). A background
reader thread records every inbound frame and auto-answers `ping` with `pong` (the
server treats `pong` as liveness-only — Go `desktop_server.go:287`, Rust
`DesktopInbound::Pong`), so a slow test can't strand the keepalive.

Design mirrors `conftest.py`'s plain-`socket`/blocking style. Every client is a
context manager that connects on enter and closes on exit (the socket is always
closed — no `ResourceWarning` under the suite's warnings-as-errors), and two clients
can be driven concurrently for the supersede test.

Frame consumption is a **cursor**: `await_frame(type)` advances an internal cursor
through the recorded frames in arrival order, so two successive `await_frame(...)`
calls return two successive frames (e.g. an accepted `hello_ack` then, after being
superseded, an `accepted:false` one).
"""

from __future__ import annotations

import json
import socket
import threading
import time


class DesktopClient:
    """A single fake shed-desktop connection to a `host-agent.sock`."""

    def __init__(
        self,
        sock_path: str,
        *,
        name: str = "fake-desktop",
        version: str = "0.0.0-test",
        pid: int | None = None,
        capabilities: list[str] | None = None,
        replay_events: int = 0,
    ) -> None:
        self.sock_path = str(sock_path)
        self._client = {
            "name": name,
            "version": version,
            "pid": pid if pid is not None else 4242,
        }
        self._capabilities = list(capabilities) if capabilities is not None else []
        self._replay_events = int(replay_events)

        self._sock: socket.socket | None = None
        self._reader: threading.Thread | None = None
        self._send_lock = threading.Lock()

        # All inbound frames in arrival order + a cursor consumed by await_frame.
        self._cond = threading.Condition()
        self._frames: list[dict] = []
        self._cursor = 0
        self._closed = False  # server closed our connection (read hit EOF)

    # -- connection lifecycle ------------------------------------------------

    def connect(self, timeout: float = 3.0) -> "DesktopClient":
        """Connect to the desktop socket (retrying a not-yet-accepting/absent socket
        until `timeout`) and start the background reader. The daemon fixture waits on
        the *status* socket, which the daemon binds just before the desktop one, so a
        brief connect retry closes that startup-ordering window."""
        deadline = time.monotonic() + timeout
        while True:
            s = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
            try:
                s.connect(self.sock_path)
                break
            except (FileNotFoundError, ConnectionRefusedError, OSError):
                s.close()
                if time.monotonic() >= deadline:
                    raise
                time.sleep(0.02)
        self._sock = s
        self._reader = threading.Thread(target=self._read_loop, args=(s,), daemon=True)
        self._reader.start()
        return self

    def __enter__(self) -> "DesktopClient":
        return self.connect()

    def __exit__(self, *exc) -> None:
        self.close()

    def close(self) -> None:
        """Close the socket (unblocking the reader with EOF) and join the reader
        thread. Idempotent; always leaves the socket closed."""
        s = self._sock
        self._sock = None
        if s is not None:
            try:
                s.shutdown(socket.SHUT_RDWR)
            except OSError:
                pass
            try:
                s.close()
            except OSError:
                pass
        if self._reader is not None:
            self._reader.join(timeout=2.0)
            self._reader = None

    # -- sending -------------------------------------------------------------

    def send(self, frame: dict) -> None:
        """Send one NDJSON frame (a compact line + trailing newline)."""
        s = self._sock
        if s is None:
            raise RuntimeError("desktop client is not connected")
        data = (json.dumps(frame, separators=(",", ":")) + "\n").encode("utf-8")
        with self._send_lock:
            s.sendall(data)

    def send_hello(self) -> None:
        """Send the registration `hello` (app -> agent) that opens the handshake."""
        self.send(
            {
                "type": "hello",
                "client": self._client,
                "capabilities": self._capabilities,
                "replay_events": self._replay_events,
            }
        )

    def send_approval_response(
        self,
        request_id: str,
        decision: str,
        *,
        decided_by: str | None = None,
        scope: str | None = None,
        ttl: str | None = None,
    ) -> None:
        """Answer an `approval_request` (app -> agent). `decision` is `"approve"` or
        anything else (the agent treats non-`"approve"` as deny). `decided_by`/`scope`/
        `ttl` are the app's decision detail; omitted (not sent) when None so the audit
        omits them — matching the `omitempty` on `approvalResponseMsg.{scope,ttl}` and
        the agent's `decided_by` default of `"user"` when left empty."""
        frame = {
            "type": "approval_response",
            "request_id": request_id,
            "decision": decision,
        }
        if decided_by is not None:
            frame["decided_by"] = decided_by
        if scope is not None:
            frame["scope"] = scope
        if ttl is not None:
            frame["ttl"] = ttl
        self.send(frame)

    # -- receiving -----------------------------------------------------------

    def _read_loop(self, s: socket.socket) -> None:
        """Read NDJSON frames until EOF/close. Auto-answers `ping` with `pong`;
        records every inbound frame (ping included) and notifies waiters."""
        buf = b""
        try:
            while True:
                chunk = s.recv(65536)
                if not chunk:
                    break  # EOF: server closed the connection
                buf += chunk
                while b"\n" in buf:
                    line, buf = buf.split(b"\n", 1)
                    line = line.strip()
                    if not line:
                        continue
                    try:
                        frame = json.loads(line)
                    except ValueError:
                        continue  # skip an unparseable line (never seen from either impl)
                    if isinstance(frame, dict) and frame.get("type") == "ping":
                        self._auto_pong(frame)
                    with self._cond:
                        self._frames.append(frame)
                        self._cond.notify_all()
        except OSError:
            pass  # our own close(), or a reset — treated as EOF below
        finally:
            with self._cond:
                self._closed = True
                self._cond.notify_all()

    def _auto_pong(self, ping: dict) -> None:
        """Answer a server `ping` with a `pong` (liveness-only for both impls). Best
        effort: a closed socket just means the keepalive no longer matters."""
        try:
            reply = {"type": "pong", "v": 2}
            if isinstance(ping.get("id"), str):
                reply["id"] = ping["id"]
            self.send(reply)
        except (OSError, RuntimeError):
            pass

    def await_frame(self, frame_type: str, timeout: float = 5.0) -> dict:
        """Advance the cursor through recorded frames until one of `frame_type` is
        found, blocking up to `timeout`. Consumes (skips) any intervening frames.
        Raises `AssertionError` on timeout or if the socket closes with no match."""
        deadline = time.monotonic() + timeout
        with self._cond:
            while True:
                while self._cursor < len(self._frames):
                    frame = self._frames[self._cursor]
                    self._cursor += 1
                    if isinstance(frame, dict) and frame.get("type") == frame_type:
                        return frame
                if self._closed:
                    raise AssertionError(
                        f"socket closed while awaiting {frame_type!r}; "
                        f"frames so far={self._frames!r}"
                    )
                remaining = deadline - time.monotonic()
                if remaining <= 0:
                    raise AssertionError(
                        f"timeout ({timeout}s) awaiting a {frame_type!r} frame; "
                        f"frames so far={self._frames!r}"
                    )
                self._cond.wait(remaining)

    def wait_closed(self, timeout: float = 2.0) -> bool:
        """Return True once the server has closed our connection (read hit EOF)
        within `timeout`, else False. Detects the drop-on-non-hello / superseded
        close without a fixed sleep."""
        deadline = time.monotonic() + timeout
        with self._cond:
            while not self._closed:
                remaining = deadline - time.monotonic()
                if remaining <= 0:
                    return False
                self._cond.wait(remaining)
            return True

    def frames(self) -> list[dict]:
        """A snapshot of every inbound frame recorded so far (arrival order)."""
        with self._cond:
            return list(self._frames)


def wait_for_consumer(daemon_handle, connected: bool = True, timeout: float = 5.0) -> None:
    """Poll the daemon's `status --json` until its approval channel reports the
    desired `consumer_connected` state (or raise on timeout). The robust readiness
    signal for 'client A is now the active consumer' before connecting client B —
    a deadline poll, not a sleep. `daemon_handle` is a `conftest.DaemonHandle`."""
    deadline = time.monotonic() + timeout
    last = None
    while time.monotonic() < deadline:
        r = daemon_handle.status(json=True)
        if r.returncode == 0:
            try:
                obj = json.loads(r.stdout)
                last = obj.get("approval_channel", {}).get("consumer_connected")
                if last is connected:
                    return
            except ValueError:
                pass
        time.sleep(0.02)
    raise AssertionError(
        f"approval channel consumer_connected did not reach {connected} within "
        f"{timeout}s (last={last!r})"
    )
