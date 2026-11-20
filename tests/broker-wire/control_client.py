"""The harness's client for `shed-broker-harness`'s control socket.

The harness binary hosts the desktop app's embedded broker and stands a control socket
where the app's Coordinator sits (`crates/shed-broker-harness/src/control.rs`): a Unix
socket of newline-delimited JSON, any number of connections at once, each request
answered on its own connection.

* `request(sock, obj)` sends one request on a connection of its own and returns the
  reply: `{"op":"status"}` (the status JSON) or `{"op":"decide",...}`.
* `ControlClient` is the frame subscriber: on enter it connects and sends
  `{"op":"subscribe"}`, and a reader thread records every frame the bridge sends the app
  (`approval_request`, `event`) from the broker's spawn on: frames sent before the
  subscribe are queued by the harness and arrive first, in order. There is no promotion
  step: the app's broker always has its consumer, so a request waits for a decision
  (or the config's approval timeout) whether or not anything is subscribed.

Frame consumption is a **cursor**: `await_frame(type)` advances through the recorded
frames in arrival order, so two successive calls return two successive frames.
"""

from __future__ import annotations

import json
import socket
import threading
import time


def _connect(sock_path, timeout: float) -> socket.socket:
    """Connect to the control socket, retrying a not-yet-bound one until `timeout`."""
    deadline = time.monotonic() + timeout
    while True:
        s = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        try:
            s.connect(str(sock_path))
            return s
        except OSError:
            s.close()
            if time.monotonic() >= deadline:
                raise
            time.sleep(0.02)


def _send(s: socket.socket, obj: dict) -> None:
    s.sendall((json.dumps(obj, separators=(",", ":")) + "\n").encode("utf-8"))


def _read_line(s: socket.socket, buf: bytearray, timeout: float) -> bytes:
    """Read one newline-terminated line (consuming it from `buf`)."""
    s.settimeout(timeout)
    while b"\n" not in buf:
        chunk = s.recv(65536)
        if not chunk:
            raise AssertionError("control socket closed before a full reply")
        buf.extend(chunk)
    line, _, rest = bytes(buf).partition(b"\n")
    buf[:] = rest
    return line


def request(sock_path, obj: dict, timeout: float = 5.0) -> dict:
    """Send one request on a connection of its own and return its parsed reply."""
    s = _connect(sock_path, timeout)
    try:
        _send(s, obj)
        return json.loads(_read_line(s, bytearray(), timeout))
    finally:
        s.close()


def decide(sock_path, request_id: str, decision: str) -> None:
    """Hand a person's decision (`"approve"` or `"deny"`) to the broker."""
    reply = request(sock_path, {"op": "decide", "id": request_id, "decision": decision})
    assert reply == {"ok": True}, f"decide {request_id}: {reply!r}"


class ControlClient:
    """The frame subscriber on a `shed-broker-harness` control socket."""

    def __init__(self, sock_path) -> None:
        self.sock_path = str(sock_path)
        self._sock: socket.socket | None = None
        self._reader: threading.Thread | None = None
        self._cond = threading.Condition()
        self._frames: list[dict] = []
        self._cursor = 0
        self._closed = False

    def __enter__(self) -> "ControlClient":
        return self.subscribe()

    def __exit__(self, *exc) -> None:
        self.close()

    def subscribe(self, timeout: float = 5.0) -> "ControlClient":
        """Connect, subscribe (asserting the harness took it), and start the reader."""
        s = _connect(self.sock_path, timeout)
        buf = bytearray()
        try:
            _send(s, {"op": "subscribe"})
            reply = json.loads(_read_line(s, buf, timeout))
        except BaseException:
            s.close()
            raise
        if reply != {"ok": True}:
            s.close()
            raise AssertionError(f"subscribe refused: {reply!r}")
        s.settimeout(None)
        self._sock = s
        self._reader = threading.Thread(target=self._read_loop, args=(s, buf), daemon=True)
        self._reader.start()
        return self

    def close(self) -> None:
        """Close the connection (the reader sees EOF) and join the reader. Idempotent."""
        s = self._sock
        self._sock = None
        if s is not None:
            try:
                s.shutdown(socket.SHUT_RDWR)
            except OSError:
                pass
            s.close()
        if self._reader is not None:
            self._reader.join(timeout=2.0)
            self._reader = None

    def decide(self, request_id: str, decision: str) -> None:
        """Decide on a request this subscriber was shown (a connection of its own)."""
        decide(self.sock_path, request_id, decision)

    def _read_loop(self, s: socket.socket, buf: bytearray) -> None:
        data = bytes(buf)
        try:
            while True:
                while b"\n" in data:
                    line, data = data.split(b"\n", 1)
                    if line.strip():
                        frame = json.loads(line)
                        with self._cond:
                            self._frames.append(frame)
                            self._cond.notify_all()
                chunk = s.recv(65536)
                if not chunk:
                    break
                data += chunk
        except OSError:
            pass
        finally:
            with self._cond:
                self._closed = True
                self._cond.notify_all()

    def await_frame(self, frame_type: str, timeout: float = 5.0) -> dict:
        """Advance the cursor until a frame of `frame_type` is found (blocking up to
        `timeout`), skipping any frame in between. Raises on timeout or EOF."""
        deadline = time.monotonic() + timeout
        with self._cond:
            while True:
                while self._cursor < len(self._frames):
                    frame = self._frames[self._cursor]
                    self._cursor += 1
                    if frame.get("type") == frame_type:
                        return frame
                if self._closed:
                    raise AssertionError(
                        f"control socket closed while awaiting {frame_type!r}; "
                        f"frames so far={self._frames!r}"
                    )
                remaining = deadline - time.monotonic()
                if remaining <= 0:
                    raise AssertionError(
                        f"timeout ({timeout}s) awaiting a {frame_type!r} frame; "
                        f"frames so far={self._frames!r}"
                    )
                self._cond.wait(remaining)

    def frames(self) -> list[dict]:
        """Every frame recorded so far, in arrival order."""
        with self._cond:
            return list(self._frames)
