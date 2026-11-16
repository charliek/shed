"""A synthetic credential-bus listener for the integration suite.

The credential bus carries a shed's helper requests (the ssh-agent, the
Docker and AWS helpers) to a machine that answers them. A machine subscribes
with `GET /api/plugins/listeners/{namespace}/messages` (a server-sent event
stream, credentials scope) and answers with
`POST /api/plugins/listeners/{namespace}/respond`. A server pins each user's
requests to one machine, named on every subscribe and respond by the
`X-Shed-Machine-Id` and `X-Shed-Machine-Name` headers (`machine_headers`): the
first machine of a user to subscribe is bound, and the others are refused.
Each synthetic machine here has a stable id of its own, derived from the name
of the identity it stands for, so a listener and its answers name the same
machine and two identities never share one. The real machine is the
Rust `shed-host-agent` (tested in tests/host-agent-diff); this suite never
runs it. A test stands in for it with `BusListener`, over pinned mTLS with a
credentials certificate enrolled over `_bootstrap`, and starts requests from
inside a shed with a POST to the shed-agent's `127.0.0.1:498/v1/publish`,
which every image serves (`publish_from_shed`).

Nothing here runs at import or needs a server; the stream parser
(`parse_sse`) has unit tests.
"""

from __future__ import annotations

import hashlib
import http.client
import json
import socket
import ssl
import threading
import time
import urllib.parse
from typing import Callable, Iterable, Optional


MACHINE_ID_HEADER = "X-Shed-Machine-Id"
MACHINE_NAME_HEADER = "X-Shed-Machine-Name"


def machine_id(identity: str) -> str:
    """The stable machine id (32 lowercase hex digits) of the synthetic
    machine that stands for `identity`."""
    return hashlib.sha256(f"shed-itest-machine:{identity}".encode()).hexdigest()[:32]


def machine_name(identity: str) -> str:
    """The display name of `identity`'s synthetic machine."""
    return f"itest-{identity}"[:63]


def machine_headers(identity: str) -> dict[str, str]:
    """The machine headers a subscribe or respond from `identity`'s synthetic
    machine carries (the name percent-encoded, as a broker sends it)."""
    return {
        MACHINE_ID_HEADER: machine_id(identity),
        MACHINE_NAME_HEADER: urllib.parse.quote(machine_name(identity), safe=""),
    }


def parse_sse(lines: Iterable[str]) -> Iterable[tuple[str, str]]:
    """Yield `(event, data)` for each event in a server-sent event stream:
    `event:` and `data:` fields up to a blank line; `:` comment lines (the
    server's keepalives) are ignored. Several `data:` lines join with a
    newline, as the SSE format says."""
    event, data = "", []
    for raw in lines:
        line = raw.rstrip("\r\n")
        if line == "":
            if data:
                yield event or "message", "\n".join(data)
            event, data = "", []
            continue
        if line.startswith(":"):
            continue
        name, _, value = line.partition(":")
        value = value[1:] if value.startswith(" ") else value
        if name == "event":
            event = value
        elif name == "data":
            data.append(value)


class BusListener:
    """One subscription to a credential-bus namespace, read on a background
    thread into `events` (decoded envelopes).

    `open()` sends the subscribe and returns `(status, error body)`: 200 means
    the stream is up. `close()` ends it. Use as a context manager.

    The subscribe names the synthetic machine of `machine` (default: `name`),
    so listeners made for one identity are one machine, and its answers
    should carry `machine_headers` of the same identity."""

    def __init__(self, host: str, port: int, pin: str, cert_pair: tuple[str, str], namespace: str,
                 *, name: str = "listener", machine: Optional[str] = None) -> None:
        self.host, self.port, self.pin = host, int(port), pin
        self.cert_pair = cert_pair
        self.namespace = namespace
        self.name = name
        self.machine = machine if machine is not None else name
        self.events: list[dict] = []
        # When (time.monotonic()) each event and each keepalive comment was
        # read: what `alive_after` looks at.
        self.event_times: list[float] = []
        self.keepalive_times: list[float] = []
        # Set when the stream broke, or ENDED: the server never ends a
        # subscription on its own, so a stream that ends while the test still
        # listens is a listener that hears nothing more, never a quiet one.
        self.error: Optional[str] = None
        self._cond = threading.Condition()
        self._conn: Optional[http.client.HTTPSConnection] = None
        self._resp: Optional[http.client.HTTPResponse] = None
        self._sock: Optional[socket.socket] = None
        self._thread: Optional[threading.Thread] = None
        self._closing = False

    def __enter__(self) -> "BusListener":
        return self

    def __exit__(self, *exc) -> None:
        self.close()

    def open(self, timeout: float = 15) -> tuple[int, Optional[dict]]:
        ctx = ssl.SSLContext(ssl.PROTOCOL_TLS_CLIENT)
        ctx.check_hostname = False
        ctx.verify_mode = ssl.CERT_NONE  # checked below, by fingerprint
        ctx.load_cert_chain(*self.cert_pair)
        conn = http.client.HTTPSConnection(self.host, self.port, context=ctx, timeout=timeout)
        conn.connect()
        got = "sha256:" + hashlib.sha256(conn.sock.getpeercert(binary_form=True)).hexdigest()
        if got != self.pin:
            conn.close()
            raise AssertionError(f"{self.host}:{self.port} presented {got}, pinned {self.pin}")
        conn.request("GET", f"/api/plugins/listeners/{self.namespace}/messages",
                     headers={"Accept": "text/event-stream", **machine_headers(self.machine)})
        # Kept here: http.client lets go of conn.sock once a response says
        # "Connection: close", and close() needs the socket to end the read.
        sock = conn.sock
        resp = conn.getresponse()
        if resp.status != 200:
            raw = resp.read()
            conn.close()
            try:
                return resp.status, json.loads(raw) if raw.strip() else None
            except json.JSONDecodeError:
                return resp.status, {"raw": raw.decode("utf-8", errors="replace")}
        # The stream idles between events (a keepalive every 25 s): no read
        # timeout; close() shuts the socket down to end the read.
        sock.settimeout(None)
        self._conn, self._resp, self._sock = conn, resp, sock
        self._thread = threading.Thread(target=self._read, name=f"bus-{self.name}", daemon=True)
        self._thread.start()
        return 200, None

    def _lines(self):
        assert self._resp is not None
        while True:
            line = self._resp.readline()
            if not line:
                return
            if line.startswith(b":"):
                with self._cond:
                    self.keepalive_times.append(time.monotonic())
                    self._cond.notify_all()
            yield line.decode("utf-8", errors="replace")

    def _read(self) -> None:
        # Every exception is caught: an exception escaping a thread is a
        # warning, and this suite turns warnings into errors.
        try:
            for event, data in parse_sse(self._lines()):
                if event != "message":
                    continue
                try:
                    env = json.loads(data)
                except json.JSONDecodeError:
                    continue
                with self._cond:
                    self.events.append(env)
                    self.event_times.append(time.monotonic())
                    self._cond.notify_all()
            if not self._closing:
                self.error = "the stream ended (the server closed the subscription)"
        except BaseException as e:  # noqa: BLE001 - see above
            if not self._closing:
                self.error = f"{type(e).__name__}: {e}"
        finally:
            with self._cond:
                self._cond.notify_all()

    def wait_for(self, pred: Callable[[dict], bool], timeout: float) -> Optional[dict]:
        """The first event matching `pred`, waiting up to `timeout` seconds;
        None when none came."""
        deadline = time.monotonic() + timeout
        with self._cond:
            while True:
                for env in self.events:
                    if pred(env):
                        return env
                left = deadline - time.monotonic()
                if left <= 0:
                    return None
                self._cond.wait(left)

    def matching(self, pred: Callable[[dict], bool]) -> list[dict]:
        with self._cond:
            return [e for e in self.events if pred(e)]

    def alive_after(self, t: float, timeout: float) -> bool:
        """Wait up to `timeout` seconds for the stream to show it is still
        delivering after `t` (a `time.monotonic()` value): an event or a
        keepalive comment read after it. False when none came or the stream
        broke or ended first (`error`). Anything the server queued for this
        listener before that point has been read by then, so a check of
        `events` made after this returns True covers it."""
        deadline = time.monotonic() + timeout
        with self._cond:
            while True:
                if self.error is not None:
                    return False
                if any(x > t for x in self.keepalive_times) or any(x > t for x in self.event_times):
                    return True
                left = deadline - time.monotonic()
                if left <= 0:
                    return False
                self._cond.wait(left)

    def close(self) -> None:
        self._closing = True
        conn, sock = self._conn, self._sock
        if sock is not None:
            try:
                sock.shutdown(socket.SHUT_RDWR)
            except (OSError, ValueError):
                pass
        if self._thread is not None:
            self._thread.join(timeout=10)
        if self._resp is not None:
            try:
                self._resp.close()
            except Exception:  # noqa: BLE001 - best-effort close of a dead stream
                pass
        if conn is not None:
            conn.close()
        if sock is not None:
            try:
                sock.close()
            except OSError:
                pass
        self._conn = self._resp = self._thread = self._sock = None


def payload_has(env: dict, token: str) -> bool:
    """Whether an envelope's payload carries `token` anywhere."""
    return token in json.dumps(env.get("payload"), sort_keys=True)


def response_body(request_env: dict, payload: dict) -> dict:
    """The body of a final response to `request_env` (an envelope a listener
    received): `in_reply_to` is the id the server gave the request, and the
    shed it came from names the route back."""
    return {
        "namespace": request_env.get("namespace"),
        "type": "response",
        "in_reply_to": request_env["id"],
        "final": True,
        "payload": payload,
        "shed": {"name": (request_env.get("shed") or {}).get("name", "")},
    }


def publish_argv(namespace: str, payload: dict, max_time: int = 40) -> list[str]:
    """The argv, run inside a shed, that sends one bus request through the
    shed-agent's local publish endpoint and prints the answer (or the
    agent's error body)."""
    body = json.dumps({"namespace": namespace, "type": "request", "payload": payload})
    return [
        "curl", "-sS", "-m", str(max_time), "-X", "POST",
        "-H", "Content-Type: application/json", "--data", body,
        "http://127.0.0.1:498/v1/publish",
    ]
