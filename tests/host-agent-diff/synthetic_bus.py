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
* `GET /api/egress/stream` — returns **501** so the Go daemon's always-on egress
  subscriber backs off hard (5m, DEBUG-quiet) instead of erroring. The Rust
  slice-1b daemon never hits this route (egress is a later slice); the bus
  tolerates the asymmetry — the differential compares only the ssh-agent response.
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

    def __init__(self) -> None:
        self.url: str | None = None
        self._httpd: ThreadingHTTPServer | None = None
        self._thread: threading.Thread | None = None
        self._shutdown = threading.Event()

        # One Condition guards all cross-thread state below (subscribe/respond
        # records + the per-namespace push queues + the SSE thread registry).
        self._cond = threading.Condition()
        self._subscribed: dict[str, str | None] = {}  # ns -> Authorization (None in open mode)
        self._responses: dict[str, list[dict]] = {}  # ns -> response Envelopes (arrival order)
        self._queues: dict[str, queue.Queue] = {}  # ns -> frames to push over its SSE stream
        self._egress_hits = 0  # count of GET /api/egress/stream
        self._sse_threads: set[threading.Thread] = set()  # live SSE handler threads

    # -- lifecycle -----------------------------------------------------------

    def start(self) -> "SyntheticBus":
        """Bind the server on a fresh port and serve on a background thread."""
        self._httpd = ThreadingHTTPServer(("127.0.0.1", 0), _BusHandler)
        # Handler threads are daemonic (won't block interpreter exit); we still
        # join them explicitly in stop() so their sockets close deterministically.
        self._httpd.daemon_threads = True
        self._httpd.bus = self  # type: ignore[attr-defined]  # handlers read self.server.bus
        port = self._httpd.server_address[1]
        self.url = f"http://127.0.0.1:{port}"
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

    def push_request(self, ns: str, envelope: dict) -> None:
        """Push one request Envelope onto `{ns}`'s SSE stream as a `data:` frame.
        Call after `wait_for_subscribe(ns)` so a drainer is attached."""
        frame = json.dumps(envelope, separators=(",", ":"))
        self._queue_for(ns).put(frame)

    def await_response(self, ns: str, timeout: float = 5.0) -> dict:
        """Block until the daemon POSTs a response Envelope for `{ns}` and return the
        first one (or raise on timeout). Deadline-driven."""
        deadline = time.monotonic() + timeout
        with self._cond:
            while not self._responses.get(ns):
                remaining = deadline - time.monotonic()
                if remaining <= 0:
                    raise AssertionError(
                        f"no response POSTed for {ns!r} within {timeout}s"
                    )
                self._cond.wait(remaining)
            return self._responses[ns][0]

    def subscribed_namespaces(self) -> list[str]:
        """A snapshot of the namespaces subscribed so far (sorted)."""
        with self._cond:
            return sorted(self._subscribed)

    def egress_hits(self) -> int:
        """How many times `GET /api/egress/stream` has been hit (Go always; Rust
        never, this slice)."""
        with self._cond:
            return self._egress_hits

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
            self._cond.notify_all()

    def _record_response(self, ns: str, env: dict) -> None:
        with self._cond:
            self._responses.setdefault(ns, []).append(env)
            self._cond.notify_all()

    def _record_egress(self) -> None:
        with self._cond:
            self._egress_hits += 1

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
            # Egress disabled → 501; the Go egress subscriber backs off hard.
            self._bus._record_egress()
            self._send_empty(501)
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

    def _serve_sse(self, ns: str) -> None:
        """Open the SSE stream for `ns`, record the subscribe, then push queued
        request frames until shutdown / client disconnect."""
        self._bus._record_subscribe(ns, self.headers.get("Authorization"))
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

        q = self._bus._queue_for(ns)
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
