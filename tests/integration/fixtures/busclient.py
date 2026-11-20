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
credential broker in the desktop app (tested in tests/broker-wire); this suite never
runs it. A test stands in for it with `BusListener`, over pinned mTLS with a
credentials certificate enrolled over `_bootstrap`, and starts requests from
inside a shed with a POST to the shed-agent's `127.0.0.1:498/v1/publish`,
which every image serves (`publish_from_shed`).

The pin (`test_source_pin.py`) needs several machines per user: a
`FakeMachine` is one of them (an identity's credentials certificate plus a
machine id and name), subscribes as that machine and reports exactly how the
server answered (`Subscription`: status, `error.code`, the `source` object a
SOURCE_NOT_BOUND refusal names), holds its streams, and answers requests as
that machine. A user's DEFAULT machine (`FakeMachine(user, ...)`, no tag) is
the very machine `BusListener(name=user)` subscribes as, so the two modules
share it. `ensure_source` and `ensure_known` set a user's binding up through
the routes a user has (`GET /api/sources`, `POST /api/sources/use`), never by
assuming a fresh server: bindings outlive a server restart. Two answers a
correct server may give for a moment are waited out a bounded number of
times, honouring Retry-After: 503 SOURCE_BUSY on `GET /api/sources`
(`sources`, and `cli_retrying_busy` for the `shed source` commands) and 429
SOURCE_RATE_LIMITED on a set-up subscribe.

Nothing here runs at import or needs a server; the stream parser
(`parse_sse`), the machines and the binding helpers have unit tests.
"""

from __future__ import annotations

import hashlib
import http.client
import json
import socket
import ssl
import subprocess
import threading
import time
import urllib.parse
from dataclasses import dataclass
from typing import Callable, Iterable, Mapping, Optional

from .tlsclient import pinned_request


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
    should carry `machine_headers` of the same identity. `headers`, when
    given, are sent instead of the machine headers (`{}` sends none: what a
    broker released before the pin sends)."""

    def __init__(self, host: str, port: int, pin: str, cert_pair: tuple[str, str], namespace: str,
                 *, name: str = "listener", machine: Optional[str] = None,
                 headers: Optional[Mapping[str, str]] = None) -> None:
        self.host, self.port, self.pin = host, int(port), pin
        self.cert_pair = cert_pair
        self.namespace = namespace
        self.name = name
        self.machine = machine if machine is not None else name
        self.headers = dict(machine_headers(self.machine) if headers is None else headers)
        self.events: list[dict] = []
        # When (time.monotonic()) each event and each keepalive comment was
        # read: what `alive_after` looks at.
        self.event_times: list[float] = []
        self.keepalive_times: list[float] = []
        # Set when the stream broke, or ENDED: the server never ends a
        # subscription on its own, so a stream that ends while the test still
        # listens is a listener that hears nothing more, never a quiet one.
        self.error: Optional[str] = None
        # A refused subscribe's Retry-After header (`open`), or None.
        self.retry_after: Optional[str] = None
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
        """Send the subscribe. On anything but a stream that is up (a
        refusal, or an exception: a timeout, a refused handshake, a wrong
        pin) the connection is closed before this returns or raises, so
        nothing is left open that no one tracks. A refusal's `Retry-After`
        header is kept in `retry_after`."""
        self.retry_after = None
        ctx = ssl.SSLContext(ssl.PROTOCOL_TLS_CLIENT)
        ctx.check_hostname = False
        ctx.verify_mode = ssl.CERT_NONE  # checked below, by fingerprint
        ctx.load_cert_chain(*self.cert_pair)
        conn = http.client.HTTPSConnection(self.host, self.port, context=ctx, timeout=timeout)
        sock: Optional[socket.socket] = None
        up = False
        try:
            conn.connect()
            # Kept here: http.client lets go of conn.sock once a response
            # says "Connection: close", and close() needs the socket to end
            # the read.
            sock = conn.sock
            got = "sha256:" + hashlib.sha256(sock.getpeercert(binary_form=True)).hexdigest()
            if got != self.pin:
                raise AssertionError(f"{self.host}:{self.port} presented {got}, pinned {self.pin}")
            conn.request("GET", f"/api/plugins/listeners/{self.namespace}/messages",
                         headers={"Accept": "text/event-stream", **self.headers})
            resp = conn.getresponse()
            if resp.status != 200:
                self.retry_after = resp.getheader("Retry-After")
                raw = resp.read()
                try:
                    return resp.status, json.loads(raw) if raw.strip() else None
                except json.JSONDecodeError:
                    return resp.status, {"raw": raw.decode("utf-8", errors="replace")}
            # The stream idles between events (a keepalive every 25 s): no
            # read timeout; close() shuts the socket down to end the read.
            sock.settimeout(None)
            self._conn, self._resp, self._sock = conn, resp, sock
            self._thread = threading.Thread(target=self._read, name=f"bus-{self.name}", daemon=True)
            self._thread.start()
            up = True
            return 200, None
        finally:
            if not up:
                self._conn = self._resp = self._sock = self._thread = None
                conn.close()
                if sock is not None:
                    try:
                        sock.close()
                    except OSError:
                        pass

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

    def wait_ended(self, timeout: float) -> bool:
        """Wait up to `timeout` seconds for the stream to end or break (the
        server ended the subscription: a move of the binding does that to the
        old machine's streams). True when it did."""
        deadline = time.monotonic() + timeout
        with self._cond:
            while self.error is None:
                left = deadline - time.monotonic()
                if left <= 0:
                    return False
                self._cond.wait(left)
            return True

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


# ----------------------------------------------------------------------------
# Synthetic machines and a user's binding (the pin)
# ----------------------------------------------------------------------------

# The pin's refusal codes (internal/config/types.go).
SOURCE_MACHINE_ID_REQUIRED = "SOURCE_MACHINE_ID_REQUIRED"
SOURCE_NOT_BOUND = "SOURCE_NOT_BOUND"
SOURCE_DUPLICATE_STREAM = "SOURCE_DUPLICATE_STREAM"
SOURCE_RATE_LIMITED = "SOURCE_RATE_LIMITED"
SOURCE_MACHINE_NOT_FOUND = "SOURCE_MACHINE_NOT_FOUND"
SOURCE_BUSY = "SOURCE_BUSY"

# Two answers a correct server may give for a moment, which the helpers wait
# out rather than fail on:
#   - GET /api/sources: 503 SOURCE_BUSY with Retry-After when the user's
#     records stayed busy (a move or an admission waiting on the store's
#     disk) past the server's own bounded wait. Retried up to
#     SOURCE_BUSY_ATTEMPTS times in all, honouring Retry-After; the CLI's
#     `shed source` commands read the same route and are retried the same way
#     (`cli_retrying_busy`; the CLI does not show the header, so it waits
#     SOURCE_BUSY_WAIT, the value the server sends).
#   - a set-up subscribe (`ensure_source`, `ensure_known`): 429
#     SOURCE_RATE_LIMITED with Retry-After when the user's admission limit (8,
#     then one per 15 s) is spent. Waited out RATE_LIMIT_WAITS times.
# Each wait is capped at MAX_RETRY_AFTER. Past the bound they fail, saying so.
SOURCE_BUSY_ATTEMPTS = 5
SOURCE_BUSY_WAIT = 1.0
RATE_LIMIT_WAITS = 2
RATE_LIMIT_WAIT = 15.0
MAX_RETRY_AFTER = 30.0
# What the helpers wait with; the unit tests put a recorder in its place.
_sleep = time.sleep


def retry_after_seconds(value: Optional[str], default: float) -> float:
    """A `Retry-After` header in seconds (the server sends whole seconds),
    capped at MAX_RETRY_AFTER; `default` when it is absent or not a number
    of seconds (an HTTP date is not one the server sends)."""
    try:
        secs = float(str(value).strip())
    except (TypeError, ValueError):
        return default
    if secs != secs or secs < 0:  # NaN or negative
        return default
    return min(secs, MAX_RETRY_AFTER)


def _api_request(api, method: str, path: str, body: Optional[dict] = None) -> tuple:
    """`(status, body, headers)` through `api`: `request_full` when it has one
    (`tlsclient.Pinned`), else `request` with no headers."""
    full = getattr(api, "request_full", None)
    if full is not None:
        return full(method, path, body)
    status, resp = api.request(method, path, body)
    return status, resp, {}


def cli_retrying_busy(run: Callable[[], subprocess.CompletedProcess], what: str) -> subprocess.CompletedProcess:
    """Run a `shed source` command (`run()` returns its CompletedProcess)
    again while it fails on 503 SOURCE_BUSY, SOURCE_BUSY_ATTEMPTS times in
    all, SOURCE_BUSY_WAIT apart; any other result is returned as it is. Safe
    for `shed source use` too: it reads the sources before it moves anything,
    and SOURCE_BUSY comes only from that read."""
    for attempt in range(1, SOURCE_BUSY_ATTEMPTS + 1):
        r = run()
        if r.returncode == 0 or SOURCE_BUSY not in (r.stdout or "") + (r.stderr or ""):
            return r
        if attempt < SOURCE_BUSY_ATTEMPTS:
            _sleep(SOURCE_BUSY_WAIT)
    raise AssertionError(
        f"{what}: the server answered 503 {SOURCE_BUSY} {SOURCE_BUSY_ATTEMPTS} times in a row, "
        f"{SOURCE_BUSY_WAIT:.0f} s apart (the user's credential source records stayed busy); last: "
        f"{r.stdout!r} {r.stderr!r}"
    )


@dataclass
class Subscription:
    """What one subscribe got: `status`, the error `body` (None on 200) and,
    on 200, the open `listener` (held by its machine until closed).
    `retry_after` is a refusal's Retry-After header, or None."""

    machine: str
    namespace: str
    status: int
    body: Optional[dict]
    listener: Optional[BusListener]
    retry_after: Optional[str] = None

    @property
    def code(self) -> Optional[str]:
        return ((self.body or {}).get("error") or {}).get("code")

    @property
    def message(self) -> str:
        return str(((self.body or {}).get("error") or {}).get("message") or "")

    @property
    def source(self) -> Optional[dict]:
        """The `source` object of a SOURCE_NOT_BOUND refusal (the bound
        machine's `machine_id` and `machine_name`), or None."""
        s = (self.body or {}).get("source")
        return s if isinstance(s, dict) else None

    def close(self) -> None:
        if self.listener is not None:
            self.listener.close()

    def __str__(self) -> str:
        return f"machine {self.machine!r} subscribing to {self.namespace!r}: {self.status} {self.body!r}"


class FakeMachine:
    """One synthetic machine of one user: the user's credentials certificate
    (`cert`, a `devcontrol.BootstrapCert`) plus a machine id and name.

    With no `tag` it is the user's DEFAULT machine: the id and name
    `machine_headers(user)` gives, so the same machine `BusListener(name=user)`
    subscribes as. A `tag` makes another machine of the same user (id from
    `"<user>-<tag>"`, name `itest-<user>-<tag>`): several machines share one
    certificate, as a person's two computers may enrol with one key."""

    def __init__(self, user: str, host: str, cert, *, tag: Optional[str] = None) -> None:
        self.user, self.host, self.cert, self.tag = user, host, cert, tag
        self.key = user if tag is None else f"{user}-{tag}"
        self.id = machine_id(self.key)
        self.name = machine_name(self.key)
        self.streams: list[BusListener] = []

    def __repr__(self) -> str:
        return f"FakeMachine({self.name!r}, {self.id[:8]}, user={self.user!r})"

    @property
    def headers(self) -> dict[str, str]:
        return machine_headers(self.key)

    def subscribe(self, namespace: str, *, headers: Optional[Mapping[str, str]] = None,
                  timeout: float = 15) -> Subscription:
        """Subscribe to `namespace` as this machine (or with `headers`
        instead of the machine headers; `{}` sends none). On 200 the stream is
        held (and closed by `close()`); otherwise nothing is kept."""
        listener = BusListener(self.host, self.cert.https_port, self.cert.pin, self.cert.pair, namespace,
                               name=self.name, machine=self.key, headers=headers)
        status, body = listener.open(timeout)
        if status != 200:
            listener.close()
            return Subscription(self.name, namespace, status, body, None, listener.retry_after)
        self.streams.append(listener)
        return Subscription(self.name, namespace, status, None, listener)

    def answer(self, request_env: dict, payload: dict, *, timeout: float = 5,
               headers: Optional[Mapping[str, str]] = None) -> tuple[Optional[int], Optional[dict]]:
        """Answer `request_env` (an envelope a listener received) as this
        machine: a final response carrying `payload`. Returns `(status, JSON
        body)` (204 and None when it was accepted)."""
        ns = request_env.get("namespace") or ""
        return pinned_request(
            self.host, self.cert.https_port, f"/api/plugins/listeners/{ns}/respond", self.cert.pin, self.cert.pair,
            method="POST", body=response_body(request_env, payload), timeout=timeout,
            headers=dict(self.headers if headers is None else headers),
        )

    def close(self) -> None:
        """Close every stream this machine holds."""
        streams, self.streams = self.streams, []
        for listener in streams:
            listener.close()


def sources(api, user: Optional[str] = None) -> dict:
    """`GET /api/sources` (the caller's own, or `user`'s for an admin) through
    `api` (anything with `request(method, path, body=None)`, such as
    `Identity.api()`); raises AssertionError unless it answers 200. A 503
    SOURCE_BUSY is asked again, honouring its Retry-After, up to
    SOURCE_BUSY_ATTEMPTS times in all."""
    path = "/api/sources" + (f"?user={urllib.parse.quote(user, safe='')}" if user else "")
    for attempt in range(1, SOURCE_BUSY_ATTEMPTS + 1):
        status, body, headers = _api_request(api, "GET", path)
        if not (status == 503 and ((body or {}).get("error") or {}).get("code") == SOURCE_BUSY):
            break
        if attempt == SOURCE_BUSY_ATTEMPTS:
            raise AssertionError(
                f"GET {path}: 503 {SOURCE_BUSY} {SOURCE_BUSY_ATTEMPTS} times in a row, each Retry-After honoured "
                f"(the user's credential source records stayed busy); last: {body!r}"
            )
        _sleep(retry_after_seconds((headers or {}).get("retry-after"), SOURCE_BUSY_WAIT))
    if status != 200 or not isinstance(body, dict):
        raise AssertionError(f"GET {path}: {status} {body!r}")
    return body


def bound_id(view: dict) -> Optional[str]:
    """The bound machine's id in a sources view, or None when there is no
    binding. Delivery and signing always name the same machine in this cut;
    a view where they differ is an error."""
    binding = view.get("binding")
    if not binding:
        return None
    delivery = [r.get("id") for r in binding.get("delivery") or []]
    signing = [r.get("id") for r in binding.get("signing") or []]
    if delivery != signing or len(signing) != 1:
        raise AssertionError(f"a binding whose delivery and signing are not one machine: {binding!r}")
    return signing[0]


def known_ids(view: dict) -> set[str]:
    """The ids of the machines a sources view lists."""
    return {m.get("id") for m in view.get("machines") or []}


def describe_sources(view: dict) -> str:
    """A sources view in one line, for a failure message."""
    binding = view.get("binding") or {}
    machines = ", ".join(
        f"{m.get('name')!r} {str(m.get('id'))[:8]}" + (f" connected {m.get('namespaces')}" if m.get("connected") else "")
        for m in view.get("machines") or []
    )
    return (f"user {view.get('user')!r}: bound {str(bound_id(view))[:8]} "
            f"(generation {binding.get('generation')}, by {binding.get('bound_by')!r}); machines: {machines or 'none'}")


def _present(machine: FakeMachine, namespace: str) -> Subscription:
    """One set-up subscribe of `machine` to `namespace`, its stream (if any)
    closed at once. A 429 SOURCE_RATE_LIMITED (the user's admission limit) is
    waited out, honouring Retry-After, RATE_LIMIT_WAITS times before it
    fails, saying so."""
    for waits in range(RATE_LIMIT_WAITS + 1):
        sub = machine.subscribe(namespace)
        sub.close()
        if (sub.status, sub.code) != (429, SOURCE_RATE_LIMITED):
            return sub
        if waits < RATE_LIMIT_WAITS:
            _sleep(retry_after_seconds(sub.retry_after, RATE_LIMIT_WAIT))
    raise AssertionError(
        f"presenting {machine!r} to the server: still 429 {SOURCE_RATE_LIMITED} after waiting out its Retry-After "
        f"{RATE_LIMIT_WAITS} times; the user's admission limit (8, then one per 15 s) is spent, so something "
        f"presented many new or changed machines for this user: {sub}"
    )


def _refused_naming(sub: Subscription, view: dict, bound: Optional[str]) -> bool:
    """Whether `sub` is the SOURCE_NOT_BOUND refusal that names the machine
    `view` (the user's sources just before) says is bound: its id, and the
    name the server lists for it."""
    if bound is None or (sub.status, sub.code) != (403, SOURCE_NOT_BOUND):
        return False
    names = [m.get("name") for m in view.get("machines") or [] if m.get("id") == bound]
    return sub.source == {"machine_id": bound, "machine_name": names[0] if len(names) == 1 else None}


def ensure_known(api, machine: FakeMachine, namespace: str) -> str:
    """Make sure the server remembers `machine` for its user WITHOUT changing
    which machine is the source: if it is not listed, it subscribes once to
    `namespace`, which the server refuses (SOURCE_NOT_BOUND, naming the
    source) and records the machine as first seen. The user must already
    have a source (call `ensure_source` for it first): a user with none would
    bind this machine. Returns "known" or "presented"."""
    view = sources(api)
    if machine.id in known_ids(view):
        return "known"
    bound = bound_id(view)
    if bound is None:
        raise AssertionError(f"{machine!r}: its user has no source yet, so presenting it would bind it "
                             f"({describe_sources(view)})")
    sub = _present(machine, namespace)
    if not _refused_naming(sub, view, bound):
        raise AssertionError(f"presenting {machine!r} to the server ({describe_sources(view)}): {sub}; want 403 "
                             f"{SOURCE_NOT_BOUND} naming the bound machine")
    return "presented"


def ensure_source(api, machine: FakeMachine, namespace: str) -> str:
    """Make `machine` its user's source through the routes the user has, from
    whatever state the server is in (a fresh one, or one that kept bindings
    from an earlier run). A machine the server does not know yet subscribes
    once to `namespace` first: with no binding that binds it (the first
    machine to connect), otherwise it is refused and recorded; then
    `POST /api/sources/use` moves the binding to it. Returns how: "already",
    "first-connect" or "moved". The stream of that one subscribe is closed;
    the caller subscribes for real afterwards."""
    view = sources(api)
    bound = bound_id(view)
    if bound == machine.id:
        return "already"
    how = "moved"
    if machine.id not in known_ids(view):
        sub = _present(machine, namespace)
        if sub.status == 200 and bound is None:
            how = "first-connect"
        elif not _refused_naming(sub, view, bound):
            raise AssertionError(f"presenting {machine!r} to the server ({describe_sources(view)}): {sub}; want "
                                 f"{'200 (the first machine is bound)' if bound is None else '403 ' + SOURCE_NOT_BOUND + ' naming the bound machine'}")
    if how == "moved":
        status, body = api.request("POST", "/api/sources/use", {"machine_id": machine.id})
        if status != 200:
            raise AssertionError(f"moving the source to {machine!r}: {status} {body!r}")
    view = sources(api)
    if bound_id(view) != machine.id:
        raise AssertionError(f"{machine!r} is not the source after {how}: {describe_sources(view)}")
    return how
