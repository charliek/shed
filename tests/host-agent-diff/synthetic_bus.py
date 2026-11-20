"""A synthetic shed-server plugin bus for the surface-B wire tests.

Stands in for shed-server's plugin message-bus HTTP API (`sdk/hostclient.go`) so the
broker's bus client (`crates/shed-broker/src/bus.rs`, run by `shed-broker-harness`) can
subscribe, receive a pushed request Envelope, and POST a response back, entirely on
`127.0.0.1` with no real server. It exposes exactly the three routes a single-server
broker touches:

* `GET /api/plugins/listeners/{ns}/messages` — the SSE subscribe. Records that a
  subscribe happened for `{ns}` (and the `Authorization` header — `None` in open
  mode), holds the stream open, and pushes any queued request Envelope as a
  `data: {json}\\n\\n` frame until teardown.
* `POST /api/plugins/listeners/{ns}/respond` — reads the response Envelope body,
  records it keyed by namespace, and returns **204** (the status the client
  expects; a non-204 is a bus error).
* `GET /api/egress/stream` — the always-on egress-audit consumer's route. Two modes,
  chosen at construction (`SyntheticBus(egress=...)`):
    - `"unavailable"` (default) → **501**, so the broker's egress subscriber backs
      off hard (5m, DEBUG-quiet) — the harness asserts `egress_hits() == 1` within a
      short window to prove that hard backoff (no reconnect).
    - `"events"` → **200** + a held `text/event-stream` that pushes `data:` decision
      frames (`push_egress`) with FIXED timestamps, so the durable audit JSONL line is
      deterministic (`now()` is stamped only for an ABSENT ts).
* Any other path — **404**.

The machine pin (plan 035 §3.4). Every subscribe and respond records the
`X-Shed-Machine-Id` / `X-Shed-Machine-Name` headers it carried (the name
percent-decoded). A namespace can be told, at construction (`refuse={ns: kind}`) or
live (`refuse(ns, kind)`), to refuse its subscribe with one of the pinned shapes, all
in the API's `{"error":{"code","message"}}` body under 1024 bytes:

* `"not-source"` — **403** `SOURCE_NOT_BOUND` + a top-level `source:{machine_id,
  machine_name}` naming the active machine (`NOT_SOURCE_ACTIVE`);
* `"machine-id-required"` — **403** `SOURCE_MACHINE_ID_REQUIRED` (a released broker);
* `"forbidden"` — **403** `FORBIDDEN` (any other 403: a scope failure, a script key);
* `"unparseable-403"` — **403** with a body that is not JSON;
* `"duplicate"` — **409** `SOURCE_DUPLICATE_STREAM` (this machine id already holds it).

`accept(ns)` lifts the refusal so the next retry is served. The bare 409 (terminal) is
the older `conflict=` set. A respond can be told to answer a script of statuses first
(`respond_script(ns, [(status, retry_after), ...])`), each with its pinned body and a
`Retry-After` when one is given. The server sends one only with 503 and 429; a cell may
attach one to a 403 or 413 to prove the status alone stops the retry.

Responds are matched as the server matches them (`internal/api/plugin_handlers.go`,
`internal/plugin/bridge.go`): a request is PENDING from the moment its frame is written
on a stream, an answer must name a pending request of its namespace and shed by
`in_reply_to` (else `403 FORBIDDEN`), and a final answer consumes it. A namespace that
turns `not-source` live (`refuse(ns, "not-source")`: the source moved away) ends its live
streams and drops the requests pending on it, as a move does, and answers its responds
`403 SOURCE_NOT_BOUND`. `restart()` is a server restart: every live stream ends and every
pending request is gone (the server keeps them in memory only). `drop_streams()` ends
every live stream and KEEPS the pending requests. As the server does
(`internal/plugin/registry.go`, `pendingForLocked`), a new stream is first sent every
request still pending on its namespace that was delivered to the SAME machine (the
`X-Shed-Machine-Id` it subscribed with) at the SAME binding generation (bumped by every
move, away or back), in the order they were first delivered: a lost connection's
reconnect, or another process of the same machine (two brokers on a copied state
directory share one id), is handed what was in flight. A stream that has ended (a
move, a restart, `drop_streams`) never delivers a frame it takes after that: the frame
goes back on the queue for the next stream.

`single_holder=True` adds the duplicate rule of a pinning server for this one bound
machine: at most one live stream per namespace, and a subscribe for a held namespace is
`409 SOURCE_DUPLICATE_STREAM` — two processes sharing one machine id (two brokers on a
copied state directory); `holders()` and `attempts()` say who holds and who asked.

Threading model mirrors the plain-`socket`/blocking style of `control_client.py`:
a `ThreadingHTTPServer` on a background thread, one handler thread per connection
(the long-lived SSE GET, the egress GET, and the respond POST each get their own),
and a deterministic, deadline-driven shutdown — an `Event` plus a per-namespace
sentinel unblocks the SSE loops immediately, and every handler thread is joined so
no socket lingers to trip the suite's warnings-as-errors (ResourceWarning).

Robust timing: `wait_for_subscribe` / `await_response` block on a `Condition` with a
deadline (never a fixed sleep), so a slow broker connect can't flake the test.
"""

from __future__ import annotations

import json
import queue
import re
import select
import socket
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from urllib.parse import unquote, urlsplit

# Route matchers for the two namespace-scoped endpoints. `{ns}` is a single path
# segment (no slashes), URL-decoded before use.
_MESSAGES_RE = re.compile(r"^/api/plugins/listeners/([^/]+)/messages$")
_RESPOND_RE = re.compile(r"^/api/plugins/listeners/([^/]+)/respond$")
_EGRESS_PATH = "/api/egress/stream"

# The machine-identity request headers (plan 035 §3.4).
MACHINE_ID_HEADER = "X-Shed-Machine-Id"
MACHINE_NAME_HEADER = "X-Shed-Machine-Name"

# The machine a `not-source` refusal names as the active source (fixed, so a golden
# can pin it unmasked).
NOT_SOURCE_ACTIVE = {
    "machine_id": "fedcba9876543210fedcba9876543210",
    "machine_name": "desk",
}

# The pinned subscribe refusals: kind -> (status, JSON body or raw text body).
_REFUSALS: dict[str, tuple[int, object]] = {
    "not-source": (
        403,
        {
            "error": {
                "code": "SOURCE_NOT_BOUND",
                "message": 'credential source for this server is "desk"; '
                "run 'shed source use <this machine>' to move it",
            },
            "source": NOT_SOURCE_ACTIVE,
        },
    ),
    "machine-id-required": (
        403,
        {
            "error": {
                "code": "SOURCE_MACHINE_ID_REQUIRED",
                "message": "this server needs shed-host-agent and shed-desktop 0.10.0 or newer",
            }
        },
    ),
    "forbidden": (
        403,
        {"error": {"code": "FORBIDDEN", "message": "a script key cannot subscribe"}},
    ),
    "unparseable-403": (403, '{"error": {"code": "SOURCE_NOT_BOUND"'),
    "duplicate": (
        409,
        {
            "error": {
                "code": "SOURCE_DUPLICATE_STREAM",
                "message": 'this machine already holds namespace "ssh-agent"',
            }
        },
    ),
}

# The pinned respond refusal bodies, by status.
_RESPOND_REFUSALS: dict[int, dict] = {
    503: {"error": {"code": "RESPONSE_NOT_DELIVERED", "message": "not delivered; try again"}},
    429: {"error": {"code": "TOO_MANY_RESPONSES", "message": "too many answers in flight"}},
    # The server's own code and words for a respond body over its limit
    # (`plugin_handlers.go`: `config.ErrRequestTooLarge`, `maxRespondBody`).
    413: {"error": {"code": "REQUEST_TOO_LARGE", "message": "response body is over 16777216 bytes"}},
    403: {
        "error": {
            "code": "FORBIDDEN",
            "message": "response does not match a pending request for this namespace",
        }
    },
}

# What the server answers a respond that matches no pending request.
_UNMATCHED = {
    "error": {
        "code": "FORBIDDEN",
        "message": "response does not match a pending request for this namespace",
    }
}

# How often an idle SSE loop re-checks the shutdown flag while waiting to push.
_SSE_POLL = 0.1


# What `SyntheticBus._deliver` decides about a frame a stream is about to write.
DELIVER_WRITE = "write"
DELIVER_SKIP = "skip"
DELIVER_ENDED = "ended"


class SyntheticBus:
    """A synthetic shed-server plugin bus, bound to `127.0.0.1:0` (dynamic port).

    Use as a context manager (`with SyntheticBus() as bus:`) — it starts on enter
    and shuts down cleanly on exit. `bus.url` is the base `http://127.0.0.1:PORT`
    to point a daemon's `server:` at.
    """

    def __init__(
        self,
        egress: str = "unavailable",
        conflict: set[str] | None = None,
        tls_cert: str | None = None,
        tls_key: str | None = None,
        unauthorized: set[str] | None = None,
        refuse: dict[str, str] | None = None,
        single_holder: bool = False,
    ) -> None:
        assert egress in ("unavailable", "events"), f"bad egress mode {egress!r}"
        self.url: str | None = None
        self._httpd: ThreadingHTTPServer | None = None
        self._thread: threading.Thread | None = None
        self._shutdown = threading.Event()
        self._egress_mode = egress  # "unavailable" (501) | "events" (200 + decision stream)
        # Namespaces whose SSE subscribe answers 409 Conflict (another listener owns them).
        # The client records the state as terminal `rejected` (no hot-retry) — the
        # `servers[]` 409-rejected differential cell.
        self._conflict: set[str] = set(conflict or ())
        # Namespaces whose FIRST SSE subscribe answers 401 (token rejected → the client
        # invalidates + re-mints, then reconnects). A per-namespace one-shot: the second
        # subscribe succeeds, so the secure-bus 401-invalidate cell can observe the re-mint.
        self._unauthorized: set[str] = set(unauthorized or ())
        self._401_seen: set[str] = set()  # namespaces that have already been 401'd once
        # TLS: when a committed cert/key pair is supplied, the listen socket is wrapped in
        # an ssl.SSLContext (Python's stdlib `ssl` can't GENERATE a cert, so the harness
        # commits a fixed self-signed pair) and `url` is https:// — the secure-bus cells.
        self._tls_cert = tls_cert
        self._tls_key = tls_key
        self._subscribe_auths: dict[str, list[str | None]] = {}  # ns -> every Authorization seen
        # ns -> the pinned refusal kind its subscribe answers (plan 035 §3.4), until
        # `accept(ns)`.
        for kind in (refuse or {}).values():
            assert kind in _REFUSALS, f"unknown refusal kind {kind!r}"
        self._refuse: dict[str, str] = dict(refuse or {})
        # ns -> every (machine id, decoded machine name) a subscribe / respond carried
        # (None for an absent header).
        self._subscribe_identities: dict[str, list[tuple[str | None, str | None]]] = {}
        self._respond_identities: dict[str, list[tuple[str | None, str | None]]] = {}
        # ns -> the statuses its next responds answer, in order ([(status, retry_after)]).
        self._respond_script: dict[str, list[tuple[int, str | None]]] = {}
        # (ns, request id) -> (shed name, frame, machine id, generation, order) of a request
        # written on a stream and not yet answered finally (the server's pending set,
        # stamped with the machine and binding generation it was delivered to).
        self._pending: dict[tuple[str, str], tuple] = {}
        # The binding generation: bumped by every move (away, `refuse(ns, "not-source")`,
        # and back, `accept`). Pending requests are re-delivered only at their own.
        self._generation = 1
        self._delivered_seq = 0
        # The duplicate rule (`single_holder`): ns -> (stream token, machine name, end).
        self._single_holder = single_holder
        self._holders: dict[str, tuple[object, str | None, threading.Event]] = {}
        # Every live namespace stream, in every mode: token -> (ns, end event).
        self._live: dict[object, tuple[str, threading.Event, str | None]] = {}
        # Every subscribe: (ns, machine name, monotonic time, accepted).
        self._attempts: list[tuple[str, str | None, float, bool]] = []

        # One Condition guards all cross-thread state below (subscribe/respond
        # records + the per-namespace push queues + the SSE thread registry).
        self._cond = threading.Condition()
        self._subscribed: dict[str, str | None] = {}  # ns -> Authorization (None in open mode)
        self._responses: dict[str, list[dict]] = {}  # ns -> response Envelopes (arrival order)
        self._queues: dict[str, queue.Queue] = {}  # ns -> frames to push over its SSE stream
        self._egress_hits = 0  # count of GET /api/egress/stream
        self._egress_queue: queue.Queue = queue.Queue()  # decision frames (events mode)
        self._sse_threads: set[threading.Thread] = set()  # live SSE handler threads

    # -- lifecycle -----------------------------------------------------------

    def start(self) -> "SyntheticBus":
        """Bind the server on a fresh port and serve on a background thread. When a
        committed TLS cert/key pair was supplied, the listen socket is wrapped in an
        `ssl.SSLContext` and `url` is https://."""
        self._httpd = ThreadingHTTPServer(("127.0.0.1", 0), _BusHandler)
        # Handler threads are daemonic (won't block interpreter exit); we still
        # join them explicitly in stop() so their sockets close deterministically.
        self._httpd.daemon_threads = True
        self._httpd.bus = self  # type: ignore[attr-defined]  # handlers read self.server.bus
        port = self._httpd.server_address[1]
        scheme = "http"
        if self._tls_cert is not None:
            import ssl

            ctx = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
            ctx.load_cert_chain(certfile=self._tls_cert, keyfile=self._tls_key)
            # Wrap the already-bound listen socket; each accepted connection inherits TLS.
            self._httpd.socket = ctx.wrap_socket(self._httpd.socket, server_side=True)
            scheme = "https"
        self.url = f"{scheme}://127.0.0.1:{port}"
        self._thread = threading.Thread(
            target=self._httpd.serve_forever, name="synthetic-bus", daemon=True
        )
        self._thread.start()
        return self

    def stop(self) -> None:
        """Signal shutdown, unblock + join every SSE handler, stop the accept loop,
        and close all sockets. Idempotent."""
        self._shutdown.set()
        # Unblock any SSE loop parked on its queue with a per-queue sentinel, and
        # snapshot the live handler threads to join.
        with self._cond:
            for q in self._queues.values():
                q.put(None)
            self._egress_queue.put(None)  # unblock a parked egress-events SSE loop
            threads = list(self._sse_threads)
        for t in threads:
            t.join(timeout=2.0)
        if self._httpd is not None:
            self._httpd.shutdown()  # stop serve_forever
            self._httpd.server_close()  # close the listening socket
            self._httpd = None
        if self._thread is not None:
            self._thread.join(timeout=2.0)
            self._thread = None

    def __enter__(self) -> "SyntheticBus":
        return self.start()

    def __exit__(self, *exc) -> None:
        self.stop()

    # -- test-facing helpers -------------------------------------------------

    def wait_for_subscribe(self, ns: str, timeout: float = 5.0) -> str | None:
        """Block until a daemon has subscribed to `{ns}`'s SSE stream (or raise on
        timeout). Returns the recorded `Authorization` header (`None` in open mode).
        A deadline poll, not a sleep — closes the daemon-connect timing window."""
        deadline = time.monotonic() + timeout
        with self._cond:
            while ns not in self._subscribed:
                remaining = deadline - time.monotonic()
                if remaining <= 0:
                    raise AssertionError(
                        f"no subscribe for {ns!r} within {timeout}s; "
                        f"subscribed so far={sorted(self._subscribed)}"
                    )
                self._cond.wait(remaining)
            return self._subscribed[ns]

    def wait_for_subscribe_count(self, ns: str, count: int, timeout: float = 10.0) -> list:
        """Block until `{ns}` has been subscribed at least `count` times (a deadline poll)
        and return every recorded `Authorization`. Lets the 401→re-mint→reconnect cell wait
        for the SECOND subscribe (the fresh token)."""
        deadline = time.monotonic() + timeout
        with self._cond:
            while len(self._subscribe_auths.get(ns, [])) < count:
                remaining = deadline - time.monotonic()
                if remaining <= 0:
                    raise AssertionError(
                        f"fewer than {count} subscribes for {ns!r} within {timeout}s "
                        f"(got {len(self._subscribe_auths.get(ns, []))})"
                    )
                self._cond.wait(remaining)
            return list(self._subscribe_auths[ns])

    def push_request(self, ns: str, envelope: dict) -> None:
        """Push one request Envelope onto `{ns}`'s SSE stream as a `data:` frame.
        Call after `wait_for_subscribe(ns)` so a drainer is attached."""
        frame = json.dumps(envelope, separators=(",", ":"))
        self._queue_for(ns).put(frame)

    def await_response(self, ns: str, timeout: float = 5.0) -> dict:
        """Block until the daemon POSTs a response Envelope for `{ns}` and return the
        first one (or raise on timeout). Deadline-driven."""
        return self.await_response_at(ns, 0, timeout)

    def await_response_at(self, ns: str, index: int, timeout: float = 5.0) -> dict:
        """Block until the daemon has POSTed at least `index + 1` responses for `{ns}`
        and return the one at `index` (arrival order). Deadline-driven. Lets a test that
        drives two sequential requests (e.g. the re-login pickup) read the SECOND
        response without racing the first."""
        deadline = time.monotonic() + timeout
        with self._cond:
            while len(self._responses.get(ns, [])) <= index:
                remaining = deadline - time.monotonic()
                if remaining <= 0:
                    raise AssertionError(
                        f"fewer than {index + 1} responses POSTed for {ns!r} within "
                        f"{timeout}s (got {len(self._responses.get(ns, []))})"
                    )
                self._cond.wait(remaining)
            return self._responses[ns][index]

    # -- the machine pin (plan 035 §3.4) ---------------------------------------

    def refuse(self, ns: str, kind: str) -> None:
        """From now on, answer `{ns}`'s subscribe with the pinned refusal `kind`. A move
        away (`"not-source"`) also drops the requests pending on `{ns}`, as the server's
        move drops those delivered to the old machine."""
        assert kind in _REFUSALS, f"unknown refusal kind {kind!r}"
        with self._cond:
            self._refuse[ns] = kind
            if kind == "not-source":
                self._generation += 1
                self._end_streams_locked(ns)
                for key in [k for k in self._pending if k[0] == ns]:
                    del self._pending[key]
            self._cond.notify_all()

    def accept(self, ns: str) -> None:
        """Lift `{ns}`'s refusal: its next subscribe is served (the move to this
        machine, as the server sees it)."""
        with self._cond:
            if self._refuse.pop(ns, None) == "not-source":
                self._generation += 1

    def respond_script(self, ns: str, script: list[tuple[int, str | None]]) -> None:
        """Answer `{ns}`'s next responds with these `(status, retry_after)` first (a
        `None` retry_after sends no header); each is a pinned refusal body, nothing is
        recorded as delivered. Then 204 as usual."""
        for status, _ in script:
            assert status in _RESPOND_REFUSALS, f"no pinned body for {status}"
        with self._cond:
            self._respond_script[ns] = list(script)

    def subscribe_count(self, ns: str) -> int:
        with self._cond:
            return len(self._subscribe_auths.get(ns, []))

    def live_streams(self) -> int:
        """How many stream handlers are running (a closed client's ends within a poll)."""
        with self._cond:
            return len(self._sse_threads)

    def pending(self) -> list[tuple[str, str]]:
        """The (ns, request id) of every request delivered and not yet answered finally."""
        with self._cond:
            return sorted(self._pending)

    # -- the duplicate rule (`single_holder`) --------------------------------

    def _end_streams_locked(self, ns: str | None = None) -> None:
        """End every live stream (of `ns` only, when given). Caller holds _cond."""
        for token, (s_ns, end, _machine) in list(self._live.items()):
            if ns is None or s_ns == ns:
                end.set()
                del self._live[token]
                held = self._holders.get(s_ns)
                if held is not None and held[0] is token:
                    del self._holders[s_ns]

    def drop_streams(self) -> float:
        """End every live stream at once, the server staying up and KEEPING its pending
        requests (a lost connection: the same machine's reconnect is re-delivered them).
        Returns the monotonic time it happened."""
        with self._cond:
            at = time.monotonic()
            self._end_streams_locked()
            self._cond.notify_all()
            return at

    def restart(self) -> float:
        """A server restart, as the app and the daemon see it: every live stream ends
        and every pending request is gone (the server holds them in memory only), so an
        answer to one delivered before is `403 FORBIDDEN`. Returns the monotonic time."""
        with self._cond:
            at = time.monotonic()
            self._end_streams_locked()
            self._pending.clear()
            self._cond.notify_all()
            return at

    def holders(self) -> dict[str, str | None]:
        """ns -> the machine name holding its live stream."""
        with self._cond:
            return {ns: name for ns, (_t, name, _e) in self._holders.items()}

    def attempts(self, ns: str, since: float = 0.0) -> list[tuple[str | None, bool]]:
        """Every subscribe for `{ns}` since `since`: (machine name, accepted)."""
        with self._cond:
            return [(n, ok) for a_ns, n, at, ok in self._attempts if a_ns == ns and at >= since]

    def attempt_times(self, ns: str, since: float = 0.0) -> list[tuple[float, str | None, bool]]:
        """Every subscribe for `{ns}` since `since`: (monotonic time, machine name,
        accepted)."""
        with self._cond:
            return [(at, n, ok) for a_ns, n, at, ok in self._attempts if a_ns == ns and at >= since]

    def wait_for(self, predicate, what: str, timeout: float) -> None:
        """Block until `predicate(self)` holds (re-checked on every recorded change)."""
        deadline = time.monotonic() + timeout
        with self._cond:
            while not predicate(self):
                remaining = deadline - time.monotonic()
                if remaining <= 0:
                    raise AssertionError(f"never within {timeout}s: {what}")
                self._cond.wait(min(remaining, _SSE_POLL))

    def subscribe_identities(self, ns: str) -> list[tuple[str | None, str | None]]:
        """Every (machine id, decoded machine name) `{ns}`'s subscribes carried."""
        with self._cond:
            return list(self._subscribe_identities.get(ns, []))

    def respond_identities(self, ns: str) -> list[tuple[str | None, str | None]]:
        """Every (machine id, decoded machine name) `{ns}`'s respond POSTs carried,
        refused ones included."""
        with self._cond:
            return list(self._respond_identities.get(ns, []))

    def wait_for_respond_attempts(self, ns: str, count: int, timeout: float = 10.0) -> int:
        """Block until `{ns}` has seen at least `count` respond POSTs (accepted or
        refused) and return how many it has seen."""
        deadline = time.monotonic() + timeout
        with self._cond:
            while len(self._respond_identities.get(ns, [])) < count:
                remaining = deadline - time.monotonic()
                if remaining <= 0:
                    raise AssertionError(
                        f"fewer than {count} respond POSTs for {ns!r} within {timeout}s "
                        f"(got {len(self._respond_identities.get(ns, []))})"
                    )
                self._cond.wait(remaining)
            return len(self._respond_identities[ns])

    def subscribed_namespaces(self) -> list[str]:
        """A snapshot of the namespaces subscribed so far (sorted)."""
        with self._cond:
            return sorted(self._subscribed)

    def egress_hits(self) -> int:
        """How many times `GET /api/egress/stream` has been hit by this bus instance's
        daemon (each daemon gets its OWN `SyntheticBus`, so this is a per-daemon counter).
        Both impls hit it now (the always-on egress subscriber)."""
        with self._cond:
            return self._egress_hits

    def wait_for_egress(self, timeout: float = 5.0) -> None:
        """Block until the daemon has GET-ed `/api/egress/stream` at least once (or raise
        on timeout). A deadline poll, not a sleep — closes the daemon-connect window.
        Proves this impl's daemon reaches the egress route (endpoint-set convergence)."""
        deadline = time.monotonic() + timeout
        with self._cond:
            while self._egress_hits < 1:
                remaining = deadline - time.monotonic()
                if remaining <= 0:
                    raise AssertionError(
                        f"no GET /api/egress/stream within {timeout}s"
                    )
                self._cond.wait(remaining)

    def push_egress(self, decision: dict) -> None:
        """Push one egress `decision` onto the (events-mode) egress SSE stream as a
        `data:` frame. Call after `wait_for_egress()` so the drainer is attached; the
        queue buffers, so an early push simply waits. No-op unless `egress="events"`."""
        frame = json.dumps(decision, separators=(",", ":"))
        self._egress_queue.put(frame)

    # -- internals (called from handler threads) -----------------------------

    def _queue_for(self, ns: str) -> queue.Queue:
        with self._cond:
            q = self._queues.get(ns)
            if q is None:
                q = queue.Queue()
                self._queues[ns] = q
            return q

    def _record_subscribe(
        self, ns: str, auth: str | None, identity: tuple[str | None, str | None]
    ) -> str | None:
        """Record a subscribe; return the refusal kind it must be answered with (or
        None to serve it), read under the same lock so `accept` is never half-seen."""
        with self._cond:
            self._subscribed[ns] = auth
            self._subscribe_auths.setdefault(ns, []).append(auth)
            self._subscribe_identities.setdefault(ns, []).append(identity)
            self._cond.notify_all()
            return self._refuse.get(ns)

    def _claim(
        self, ns: str, name: str | None, machine: str | None = None
    ) -> tuple[object, threading.Event, list[str]] | None:
        """The duplicate rule: claim `{ns}`'s one live stream (its token, end event, and
        the frames to re-deliver on it first), or None when a stream holds it. Without
        `single_holder` every claim succeeds. Every stream claimed is live (`drop_streams`,
        `restart` and a move end it) until it is released. The re-delivered frames are
        the requests pending on `{ns}` that were delivered to `machine` at the current
        generation, in their first delivery's order (`pendingForLocked`)."""
        with self._cond:
            held = self._single_holder and ns in self._holders
            self._attempts.append((ns, name, time.monotonic(), not held))
            self._cond.notify_all()
            if held:
                return None
            token, end = object(), threading.Event()
            if self._single_holder:
                self._holders[ns] = (token, name, end)
            self._live[token] = (ns, end, machine)
            redeliver = sorted(
                (order, frame)
                for (p_ns, _id), (_shed, frame, p_machine, gen, order) in self._pending.items()
                if p_ns == ns and p_machine == machine and gen == self._generation
            )
            return token, end, [frame for _order, frame in redeliver]

    def _release(self, ns: str, token: object) -> None:
        with self._cond:
            self._live.pop(token, None)
            held = self._holders.get(ns)
            if held is not None and held[0] is token:
                del self._holders[ns]
            self._cond.notify_all()

    def _deliver(self, ns: str, token: object, frame: str, q: queue.Queue | None) -> str:
        """A request frame is about to be written on `{ns}`'s stream `token`: it is pending
        now, stamped with the stream's machine and the current generation (a re-delivered
        one keeps its first order). Returns what to do with it, as the server's
        `Listener.Take` decides under its lock:

        * `DELIVER_ENDED` when the stream has ended since it took the frame (a move, a
          restart, `drop_streams`): nothing is written, and a frame taken from the queue
          (`q`) goes back on it for the next stream (Take sends nothing for an
          unregistered listener);
        * `DELIVER_SKIP` for a re-delivered frame (`q` is None: taken from the stream's
          backlog) whose request is no longer pending (answered finally since the backlog
          was copied, or dropped by a move): nothing is written and nothing becomes
          pending again (Take frees a message that is no longer live and sends nothing);
        * `DELIVER_WRITE` otherwise."""
        with self._cond:
            live = self._live.get(token)
            if live is None:
                if q is not None:
                    q.put(frame)
                return DELIVER_ENDED
            machine = live[2]
            try:
                env = json.loads(frame)
            except ValueError:
                return DELIVER_WRITE
            if isinstance(env, dict) and isinstance(env.get("id"), str):
                shed = (env.get("shed") or {}).get("name")
                key = (ns, env["id"])
                old = self._pending.get(key)
                if q is None and old is None:
                    return DELIVER_SKIP
                if old is None:
                    self._delivered_seq += 1
                    order = self._delivered_seq
                else:
                    order = old[4]
                self._pending[key] = (shed, frame, machine, self._generation, order)
            return DELIVER_WRITE

    def _next_respond_answer(
        self, ns: str, identity: tuple[str | None, str | None], env: dict
    ) -> tuple[int, dict, str | None] | None:
        """Record a respond; return `(status, body, retry_after)` to refuse it with, or
        None to deliver it. In the server's order: a body over the limit (a scripted
        413) before anything else; a namespace whose source moved away
        (SOURCE_NOT_BOUND); an answer matching no pending request (403 FORBIDDEN); then
        the scripted retryable refusals; else delivered, a final answer consuming its
        request."""
        with self._cond:
            self._respond_identities.setdefault(ns, []).append(identity)
            self._cond.notify_all()
            script = self._respond_script.get(ns)
            if script and script[0][0] == 413:
                status, retry_after = script.pop(0)
                return status, _RESPOND_REFUSALS[status], retry_after
            if self._refuse.get(ns) == "not-source":
                status, body = _REFUSALS["not-source"]
                return status, body, None
            key = (ns, env.get("in_reply_to"))
            shed = (env.get("shed") or {}).get("name")
            if key not in self._pending or self._pending[key][0] != shed:
                return 403, _UNMATCHED, None
            if script:
                status, retry_after = script.pop(0)
                return status, _RESPOND_REFUSALS[status], retry_after
            if env.get("final"):
                del self._pending[key]
            return None

    def _should_401(self, ns: str) -> bool:
        """Whether this subscribe of `ns` should answer 401 (a per-namespace one-shot: the
        first subscribe of a `unauthorized` namespace 401s, later ones succeed)."""
        with self._cond:
            if ns in self._unauthorized and ns not in self._401_seen:
                self._401_seen.add(ns)
                return True
            return False

    def _record_response(self, ns: str, env: dict) -> None:
        with self._cond:
            self._responses.setdefault(ns, []).append(env)
            self._cond.notify_all()

    def _record_egress(self) -> None:
        with self._cond:
            self._egress_hits += 1
            self._cond.notify_all()  # wake wait_for_egress

    def _register_sse_thread(self) -> None:
        with self._cond:
            self._sse_threads.add(threading.current_thread())

    def _unregister_sse_thread(self) -> None:
        with self._cond:
            self._sse_threads.discard(threading.current_thread())


class _BusHandler(BaseHTTPRequestHandler):
    """One connection's request handler. Reads its `SyntheticBus` off
    `self.server.bus`. HTTP/1.1 so the short responses keep-alive; the SSE stream
    opts out with `Connection: close` (read-until-EOF body)."""

    protocol_version = "HTTP/1.1"

    @property
    def _bus(self) -> SyntheticBus:
        return self.server.bus  # type: ignore[attr-defined]

    def do_GET(self) -> None:
        path = urlsplit(self.path).path
        if path == _EGRESS_PATH:
            self._serve_egress()
            return
        m = _MESSAGES_RE.match(path)
        if m:
            self._serve_sse(unquote(m.group(1)))
            return
        self._send_empty(404)

    def do_POST(self) -> None:
        path = urlsplit(self.path).path
        m = _RESPOND_RE.match(path)
        if m:
            self._serve_respond(unquote(m.group(1)))
            return
        self._send_empty(404)

    # -- route handlers ------------------------------------------------------

    def _serve_egress(self) -> None:
        """The `GET /api/egress/stream` route. Records the hit,
        then either 501s (`"unavailable"` mode → the daemon backs off hard) or holds a
        200 `text/event-stream` and pushes queued decision frames (`"events"` mode)."""
        bus = self._bus
        bus._record_egress()
        if bus._egress_mode != "events":
            # Egress disabled → 501; the subscriber backs off the hard 5m.
            self._send_empty(501)
            return
        self._stream_sse(bus._egress_queue)

    def _serve_sse(self, ns: str) -> None:
        """Open the SSE stream for `ns`, record the subscribe, then push queued
        request frames until shutdown / client disconnect. A namespace in the bus's
        `conflict` set answers 409 (terminal `rejected`, no retry); one in `unauthorized`
        answers 401 on its FIRST subscribe (token rejected → the client invalidates +
        re-mints, then reconnects and succeeds)."""
        bus = self._bus
        identity = self._identity()
        refusal = bus._record_subscribe(ns, self.headers.get("Authorization"), identity)
        if refusal is not None:
            status, body = _REFUSALS[refusal]
            self._send_body(status, body)
            return
        if ns in bus._conflict:
            self._send_empty(409)
            return
        if bus._should_401(ns):
            self._send_empty(401)
            return
        claim = bus._claim(ns, identity[1], identity[0])
        if claim is None:
            status, body = _REFUSALS["duplicate"]
            self._send_body(status, body)
            return
        token, end, redeliver = claim
        try:
            self._stream_sse(
                bus._queue_for(ns), ns=ns, end=end, token=token, redeliver=redeliver
            )
        finally:
            bus._release(ns, token)

    def _stream_sse(
        self,
        q: queue.Queue,
        ns: str | None = None,
        end: threading.Event | None = None,
        token: object = None,
        redeliver: list[str] | None = None,
    ) -> None:
        """Send the 200 SSE handshake, then hold the connection and drain `q`, pushing
        each frame as a `data:` line, until shutdown / `None` sentinel / client
        disconnect / `end` (the bus dropped the stream). The shared streaming body of
        `_serve_sse` (per-namespace queue; each frame written is a pending request of
        `ns`) and `_serve_egress` events mode (the egress decision queue)."""
        try:
            self.send_response(200)
            self.send_header("Content-Type", "text/event-stream")
            self.send_header("Cache-Control", "no-cache")
            # No Content-Length: the body is a stream delimited by connection close.
            self.send_header("Connection", "close")
            self.end_headers()
            self.wfile.flush()
        except OSError:
            return  # client vanished mid-handshake

        bus = self._bus
        bus._register_sse_thread()
        backlog = list(redeliver or [])
        try:
            while not bus._shutdown.is_set() and not (end is not None and end.is_set()):
                if end is not None and self._peer_gone():
                    break  # the holder went away: its namespace is free again
                if backlog:
                    frame, source = backlog.pop(0), None
                else:
                    try:
                        frame, source = q.get(timeout=_SSE_POLL), q
                    except queue.Empty:
                        continue
                    if frame is None:  # teardown sentinel
                        break
                # Pending from its write on (before it, so an answer can never arrive
                # first) — unless the stream ended while it waited for the frame, or a
                # re-delivered request was answered meanwhile.
                if ns is not None:
                    outcome = bus._deliver(ns, token, frame, source)
                    if outcome == DELIVER_ENDED:
                        break
                    if outcome == DELIVER_SKIP:
                        continue
                try:
                    self.wfile.write(("data: " + frame + "\n\n").encode("utf-8"))
                    self.wfile.flush()
                except OSError:
                    break  # client disconnected
        finally:
            bus._unregister_sse_thread()

    def _peer_gone(self) -> bool:
        """Whether the client closed its side (EOF readable), without consuming data."""
        try:
            readable, _, _ = select.select([self.connection], [], [], 0)
            if not readable:
                return False
            return self.connection.recv(1, socket.MSG_PEEK) == b""
        except (OSError, ValueError):
            return False

    def _serve_respond(self, ns: str) -> None:
        """Read a response Envelope body, record it for `ns`, return 204."""
        length = int(self.headers.get("Content-Length", "0") or "0")
        body = self.rfile.read(length) if length > 0 else b""
        try:
            env = json.loads(body)
        except ValueError:
            self._send_empty(400)
            return
        refused = self._bus._next_respond_answer(
            ns, self._identity(), env if isinstance(env, dict) else {}
        )
        if refused is not None:
            status, body, retry_after = refused
            headers = {"Retry-After": retry_after} if retry_after is not None else {}
            self._send_body(status, body, headers)
            return
        self._bus._record_response(ns, env)
        self._send_empty(204)

    # -- helpers -------------------------------------------------------------

    def _identity(self) -> tuple[str | None, str | None]:
        """The machine id and the percent-DECODED machine name this request carried
        (None for an absent header) — what a pinning server reads."""
        mid = self.headers.get(MACHINE_ID_HEADER)
        name = self.headers.get(MACHINE_NAME_HEADER)
        return mid, (unquote(name, errors="strict") if name is not None else None)

    def _send_body(self, code: int, body: object, headers: dict | None = None) -> None:
        """A refusal with a body: a dict is sent as compact JSON (under 1024 bytes, as
        the server's are), a str as is."""
        raw = (
            json.dumps(body, separators=(",", ":")) if isinstance(body, dict) else str(body)
        ).encode("utf-8")
        assert len(raw) < 1024, f"refusal body over 1024 bytes: {len(raw)}"
        try:
            self.send_response(code)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(raw)))
            for k, v in (headers or {}).items():
                self.send_header(k, v)
            self.end_headers()
            self.wfile.write(raw)
        except OSError:
            pass

    def _send_empty(self, code: int) -> None:
        """A bodyless response with an explicit zero Content-Length (keep-alive safe)."""
        try:
            self.send_response(code)
            self.send_header("Content-Length", "0")
            self.end_headers()
        except OSError:
            pass

    def log_message(self, *args) -> None:
        """Silence the default per-request stderr logging (keeps pytest output clean;
        the bus runs in-process, so there's no harness DEVNULL to swallow it)."""
