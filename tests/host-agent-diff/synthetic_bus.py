"""A synthetic shed-server plugin bus for the surface-B differential tests.

Stands in for shed-server's plugin message-bus HTTP API (`sdk/hostclient.go`) so
BOTH host-agent daemons — the Go `cmd/shed-host-agent` and the Rust
`crates/shed-host-agent` bus client (`bus.rs`) — can subscribe, receive a pushed
request Envelope, and POST a response back, entirely on `127.0.0.1` with no real
server. It exposes exactly the three routes a single-server daemon touches:

* `GET /api/plugins/listeners/{ns}/messages` — the SSE subscribe. Records that a
  subscribe happened for `{ns}` (and the `Authorization` header — `None` in open
  mode), holds the stream open, and pushes any queued request Envelope as a
  `data: {json}\\n\\n` frame until teardown.
* `POST /api/plugins/listeners/{ns}/respond` — reads the response Envelope body,
  records it keyed by namespace, and returns **204** (the status the client
  expects; a non-204 is a bus error on both sides).
* `GET /api/egress/stream` — the always-on egress-audit consumer's route, hit by
  BOTH daemons now (the endpoint sets converged). Two modes, chosen at construction
  (`SyntheticBus(egress=...)`):
    - `"unavailable"` (default) → **501**, so each daemon's egress subscriber backs
      off hard (5m, DEBUG-quiet) — the harness asserts per-impl `egress_hits() == 1`
      within a short window to prove that hard backoff (no reconnect).
    - `"events"` → **200** + a held `text/event-stream` that pushes `data:` decision
      frames (`push_egress`) with FIXED timestamps, so both daemons write the SAME
      durable audit JSONL line (a deterministic differential — Go stamps `now()` only
      for an ABSENT ts).
* Any other path — **404**.

Threading model mirrors the plain-`socket`/blocking style of `desktop_client.py`:
a `ThreadingHTTPServer` on a background thread, one handler thread per connection
(the long-lived SSE GET, the egress GET, and the respond POST each get their own),
and a deterministic, deadline-driven shutdown — an `Event` plus a per-namespace
sentinel unblocks the SSE loops immediately, and every handler thread is joined so
no socket lingers to trip the suite's warnings-as-errors (ResourceWarning).

Robust timing: `wait_for_subscribe` / `await_response` block on a `Condition` with a
deadline (never a fixed sleep), so a slow daemon connect can't flake the test.
"""

from __future__ import annotations

import json
import queue
import re
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from urllib.parse import unquote, urlsplit

# Route matchers for the two namespace-scoped endpoints. `{ns}` is a single path
# segment (no slashes), URL-decoded before use.
_MESSAGES_RE = re.compile(r"^/api/plugins/listeners/([^/]+)/messages$")
_RESPOND_RE = re.compile(r"^/api/plugins/listeners/([^/]+)/respond$")
_EGRESS_PATH = "/api/egress/stream"

# How often an idle SSE loop re-checks the shutdown flag while waiting to push.
_SSE_POLL = 0.1


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

    def subscribed_namespaces(self) -> list[str]:
        """A snapshot of the namespaces subscribed so far (sorted)."""
        with self._cond:
            return sorted(self._subscribed)

    def egress_hits(self) -> int:
        """How many times `GET /api/egress/stream` has been hit by this bus instance's
        daemon (each impl gets its OWN `SyntheticBus`, so this is a per-impl counter).
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

    def _record_subscribe(self, ns: str, auth: str | None) -> None:
        with self._cond:
            self._subscribed[ns] = auth
            self._subscribe_auths.setdefault(ns, []).append(auth)
            self._cond.notify_all()

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
        """The `GET /api/egress/stream` route. Records the hit (both impls now GET it),
        then either 501s (`"unavailable"` mode → the daemon backs off hard) or holds a
        200 `text/event-stream` and pushes queued decision frames (`"events"` mode)."""
        bus = self._bus
        bus._record_egress()
        if bus._egress_mode != "events":
            # Egress disabled → 501; each impl's subscriber backs off the hard 5m.
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
        bus._record_subscribe(ns, self.headers.get("Authorization"))
        if ns in bus._conflict:
            self._send_empty(409)
            return
        if bus._should_401(ns):
            self._send_empty(401)
            return
        self._stream_sse(bus._queue_for(ns))

    def _stream_sse(self, q: queue.Queue) -> None:
        """Send the 200 SSE handshake, then hold the connection and drain `q`, pushing
        each frame as a `data:` line, until shutdown / `None` sentinel / client
        disconnect. The shared streaming body of `_serve_sse` (per-namespace queue) and
        `_serve_egress` events mode (the egress decision queue)."""
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
        try:
            while not bus._shutdown.is_set():
                try:
                    frame = q.get(timeout=_SSE_POLL)
                except queue.Empty:
                    continue
                if frame is None:  # teardown sentinel
                    break
                try:
                    self.wfile.write(("data: " + frame + "\n\n").encode("utf-8"))
                    self.wfile.flush()
                except OSError:
                    break  # client disconnected
        finally:
            bus._unregister_sse_thread()

    def _serve_respond(self, ns: str) -> None:
        """Read a response Envelope body, record it for `ns`, return 204."""
        length = int(self.headers.get("Content-Length", "0") or "0")
        body = self.rfile.read(length) if length > 0 else b""
        try:
            env = json.loads(body)
        except ValueError:
            self._send_empty(400)
            return
        self._bus._record_response(ns, env)
        self._send_empty(204)

    # -- helpers -------------------------------------------------------------

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
        the bus runs in-process, so there's no daemon DEVNULL to swallow it)."""
