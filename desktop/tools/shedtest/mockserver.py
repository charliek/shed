"""A hermetic in-process fake shed-server.

A stdlib ThreadingHTTPServer on 127.0.0.1:<ephemeral> the harness points
the app at (via SHED_DESKTOP_MOCK_BASE_URL in test mode), so E2E runs
without a real shed-server and nothing leaves the box. State is a plain
dict the test mutates directly (server + test share the pytest process),
then forces a poll via `sheds.refresh`.

Covers M0 (info + list) now; the lifecycle + SSE create routes (M1) land
when that phase does.
"""

from __future__ import annotations

import json
import queue
import re
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from urllib.parse import unquote, urlsplit

# A non-zero GET /api/system/df payload (M7) so the System pane renders real numbers.
_GiB = 1024 ** 3
_DF_FIXTURE = {
    "server_name": "mock", "backend": "vz", "generated_at": "2026-06-01T00:00:00Z",
    "images": [{"name": "base", "docker_ref": "ghcr.io/x/base",
                "size": {"logical_bytes": _GiB, "physical_bytes": _GiB // 2}}],
    "sheds": [{"name": "shed-a", "size": {"logical_bytes": 2 * _GiB, "physical_bytes": _GiB}}],
    "orphans": [],
    "totals": {
        "images": {"logical_bytes": _GiB, "physical_bytes": _GiB // 2},
        "sheds": {"logical_bytes": 2 * _GiB, "physical_bytes": _GiB},
        "snapshots": {"logical_bytes": 0, "physical_bytes": 0},
        "orphans": {"logical_bytes": 0, "physical_bytes": 0},
        "all": {"logical_bytes": 3 * _GiB, "physical_bytes": _GiB + _GiB // 2},
    },
}


# GET /api/images (v0.6.1 shape): a default+aliased image, two more aliases
# (one uncached → "not pulled"), and an unnamed user-pulled blob the picker
# should ignore (no alias).
_IMAGES_FIXTURE = {
    "images": [
        {"name": "ghcr.io/x/shed-vz-full:v0.6.0", "docker_ref": "ghcr.io/x/shed-vz-full:v0.6.0",
         "alias": "full", "is_default": True, "cached": True, "source": "config",
         "digest": "sha256:2d9669bcf0cd25ef7dc0638dc72c7380c716e3e9d336c5d234ffa4888f28713a",
         "size_bytes": 2 * _GiB},
        {"name": "ghcr.io/x/shed-vz-base:v0.6.0", "docker_ref": "ghcr.io/x/shed-vz-base:v0.6.0",
         "alias": "base", "cached": True, "source": "config",
         "digest": "sha256:aa11bb22cc33dd44ee55ff66aa11bb22cc33dd44ee55ff66aa11bb22cc33dd44",
         "size_bytes": _GiB},
        {"name": "ghcr.io/x/shed-vz-extensions:v0.6.0", "docker_ref": "ghcr.io/x/shed-vz-extensions:v0.6.0",
         "alias": "extensions", "cached": False, "source": "config", "size_bytes": 0},
        {"name": "sha256:ff8800", "cached": True, "source": "dangling",
         "digest": "sha256:ff8800aa11bb22cc33dd44ee55ff66aa11bb22cc33dd44ee55ff66aa11bb22cc",
         "size_bytes": _GiB // 2},
    ]
}


# GET /api/egress/profiles: a config baseline + a user profile, so the egress
# pane renders and the Rust/Swift backends decode the same shape.
_EGRESS_FIXTURE = [
    {"name": "default", "source": "config",
     "profile": {"mode": "audit", "allow": ["*.github.com"], "deny": ["evil.example.com"]}},
    {"name": "custom", "source": "user", "profile": {"allow": ["api.example.com"]}},
]


DEFAULT_INFO = {
    "name": "mock",
    "version": "0.0.0-mock",
    "ssh_port": 2222,
    "http_port": 8080,
    "backend": "vz",
}

DEFAULT_SHEDS = [
    {
        "name": "hello-world",
        "status": "running",
        "created_at": "2026-05-31T13:33:00.884935839-05:00",
        "container_id": "fc-hello-world",
        "backend": "firecracker",
        "ip_address": "172.30.0.2",
        "cpus": 2,
        "memory_mb": 4096,
        "started_at": "2026-05-31T18:33:02.364547927Z",
        # Created from an aliased image → label + digest (the `<label> (sha256:short)` path).
        "image": "full",
        "image_digest": "sha256:2d9669bcf0cd25ef7dc0638dc72c7380c716e3e9d336c5d234ffa4888f28713a",
    },
    {
        "name": "callbell",
        "status": "stopped",
        "backend": "vz",
        # Created from the server default → no `image`, only `image_digest`
        # (the common v0.6.0 shape: the badge falls back to the short digest).
        "image_digest": "sha256:abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789",
    },
]


# ---------------------------------------------------------------------------
# Plugin message-bus endpoints (leg 3a.2, embedded-broker e2e).
#
# The two namespace-scoped routes the embedded broker's bus client (shed-broker
# `bus.rs`) drives against a shed-server, plus the always-on egress consumer route.
# Wire shapes are derived from the REAL server contract and pinned against two
# in-repo references (the §3.8 anti-drift pin) so the two fakes can't diverge:
#   * `tests/host-agent-diff/synthetic_bus.py` — the Go-vs-Rust differential bus
#     (identical `_MESSAGES_RE`/`_RESPOND_RE`/`_EGRESS_PATH`, `data: {json}\n\n`
#     SSE framing, 204-on-respond, 409-terminal-conflict, 501-egress-unavailable);
#   * `docs/development/host-agent-wire-catalog.md` §0/§9 (the observable channels).
#
# Target-scoped: only the tauri-target embedded-broker cells exercise these. The
# mac target never starts an in-process broker, so it never hits them (the mock
# object is shared, but these routes stay dormant for every non-broker test).
_MESSAGES_RE = re.compile(r"^/api/plugins/listeners/([^/]+)/messages$")
_RESPOND_RE = re.compile(r"^/api/plugins/listeners/([^/]+)/respond$")
_EGRESS_PATH = "/api/egress/stream"

# How often an idle SSE loop re-checks the shutdown flag while waiting to push
# (matches synthetic_bus `_SSE_POLL`).
_SSE_POLL = 0.1


class MockShedServer:
    def __init__(self):
        # The app's background poller reads this state from handler threads
        # concurrently with test setup mutating it, so all access is guarded.
        self._lock = threading.Lock()
        self.info = dict(DEFAULT_INFO)
        # `sheds` may be a list or None (the real server returns
        # `{"sheds": null}` when empty — a decoding path we must exercise).
        self.sheds: list | None = [dict(s) for s in DEFAULT_SHEDS]
        # Create-stream controls.
        self.create_should_fail = False
        self.create_progress = ["resolving image", "starting VM", "provisioning workspace"]
        # The body of the most recent POST /api/sheds, so a test can assert
        # the image picker's chosen alias reached the create request.
        self.last_create_body: dict | None = None
        self._httpd: ThreadingHTTPServer | None = None
        self._thread: threading.Thread | None = None
        # ---- plugin message-bus state (leg 3a.2) ----
        # One Condition guards all cross-thread bus state (the SSE handler threads
        # touch it concurrently with the test thread), mirroring synthetic_bus.
        self._bus_cond = threading.Condition()
        self._bus_shutdown = threading.Event()
        # ns -> Authorization header seen on its (latest) subscribe (None in open mode).
        self._bus_subscribed: dict[str, str | None] = {}
        # ns -> total subscribe attempts seen (latest-wins above loses the count of a
        # 409-terminal namespace never retrying vs. one that legitimately reconnects).
        self._bus_subscribe_counts: dict[str, int] = {}
        # namespaces with a currently-LIVE SSE listener — a second concurrent subscribe
        # gets 409 NAMESPACE_ALREADY_REGISTERED (the real server's one-listener-per-ns
        # invariant), released on disconnect so a later cell can re-subscribe cleanly.
        self._bus_live: set[str] = set()
        # namespaces pre-registered as conflicting → their FIRST subscribe already 409s
        # (a competing listener owns them): the split-namespace 409 race cell.
        self._bus_conflict: set[str] = set()
        # ns -> response Envelopes POSTed back (arrival order).
        self._bus_responses: dict[str, list[dict]] = {}
        # ns -> queue of `data:` frames to push over its SSE stream.
        self._bus_queues: dict[str, queue.Queue] = {}
        self._bus_sse_threads: set[threading.Thread] = set()
        self._egress_hits = 0

    @property
    def base_url(self) -> str:
        assert self._httpd is not None
        host, port = self._httpd.server_address[0], self._httpd.server_address[1]
        return f"http://{host}:{port}"

    def snapshot(self) -> tuple[dict, list | None]:
        # Deep-copy the shed dicts under the lock so a GET never encodes a
        # dict that a concurrent lifecycle POST is mutating.
        with self._lock:
            sheds = None if self.sheds is None else [dict(s) for s in self.sheds]
            return dict(self.info), sheds

    def set_sheds(self, sheds: list | None) -> None:
        with self._lock:
            self.sheds = sheds

    def reset(self) -> None:
        with self._lock:
            self.info = dict(DEFAULT_INFO)
            self.sheds = [dict(s) for s in DEFAULT_SHEDS]
            self.create_should_fail = False
            self.last_create_body = None

    def last_create(self) -> dict | None:
        with self._lock:
            return None if self.last_create_body is None else dict(self.last_create_body)

    def shed(self, name: str) -> dict | None:
        with self._lock:
            for s in (self.sheds or []):
                if s["name"] == name:
                    return dict(s)
        return None

    # -- mutations the request handlers call (under lock) -----------------
    def _set_status(self, name: str, status: str) -> bool:
        with self._lock:
            for s in (self.sheds or []):
                if s["name"] == name:
                    s["status"] = status
                    return True
        return False

    def _delete(self, name: str) -> bool:
        with self._lock:
            if not self.sheds:
                return False
            before = len(self.sheds)
            self.sheds = [s for s in self.sheds if s["name"] != name]
            return len(self.sheds) != before

    def _add(self, shed: dict) -> None:
        with self._lock:
            if self.sheds is None:
                self.sheds = []
            self.sheds.append(shed)

    # -- plugin message-bus: test-facing helpers (leg 3a.2) ---------------
    def pre_register_conflict(self, *namespaces: str) -> None:
        """Mark namespaces as already-owned by a competing listener so their subscribe
        answers 409 NAMESPACE_ALREADY_REGISTERED (terminal `rejected` client-side, no
        retry). Call BEFORE the broker connects. Drives the split-namespace 409 cell."""
        with self._bus_cond:
            self._bus_conflict.update(namespaces)

    def wait_for_subscribe(self, ns: str, timeout: float = 10.0) -> str | None:
        """Block until the broker has subscribed to `{ns}`'s SSE stream (deadline poll,
        not a sleep). Returns the recorded `Authorization` header (None in open mode)."""
        deadline = time.monotonic() + timeout
        with self._bus_cond:
            while ns not in self._bus_subscribed:
                remaining = deadline - time.monotonic()
                if remaining <= 0:
                    raise AssertionError(
                        f"no subscribe for {ns!r} within {timeout}s; "
                        f"subscribed so far={sorted(self._bus_subscribed)}")
                self._bus_cond.wait(remaining)
            return self._bus_subscribed[ns]

    def subscribed_namespaces(self) -> list[str]:
        """A sorted snapshot of the namespaces subscribed so far."""
        with self._bus_cond:
            return sorted(self._bus_subscribed)

    def subscribe_count(self, ns: str) -> int:
        """How many times `{ns}` has been subscribed (each SSE GET, including 409s).
        Used to assert the bus does NOT hot-retry a terminal `rejected` namespace."""
        with self._bus_cond:
            return self._bus_subscribe_counts.get(ns, 0)

    def push_request(self, ns: str, envelope: dict) -> None:
        """Push one request Envelope onto `{ns}`'s SSE stream as a `data:` frame. Call
        after `wait_for_subscribe(ns)` so a drainer is attached (the queue buffers, so
        an early push simply waits)."""
        frame = json.dumps(envelope, separators=(",", ":"))
        self._bus_queue_for(ns).put(frame)

    def await_response(self, ns: str, timeout: float = 10.0) -> dict:
        """Block until the broker POSTs a response Envelope for `{ns}` and return the
        first one (or raise on timeout). Deadline-driven."""
        deadline = time.monotonic() + timeout
        with self._bus_cond:
            while not self._bus_responses.get(ns):
                remaining = deadline - time.monotonic()
                if remaining <= 0:
                    raise AssertionError(
                        f"no response POSTed for {ns!r} within {timeout}s")
                self._bus_cond.wait(remaining)
            return self._bus_responses[ns][0]

    def egress_hits(self) -> int:
        with self._bus_cond:
            return self._egress_hits

    # -- plugin message-bus: internals (called from handler threads) ------
    def _bus_queue_for(self, ns: str) -> queue.Queue:
        with self._bus_cond:
            q = self._bus_queues.get(ns)
            if q is None:
                q = queue.Queue()
                self._bus_queues[ns] = q
            return q

    def _bus_record_subscribe(self, ns: str, auth: str | None) -> None:
        with self._bus_cond:
            self._bus_subscribed[ns] = auth
            self._bus_subscribe_counts[ns] = self._bus_subscribe_counts.get(ns, 0) + 1
            self._bus_cond.notify_all()

    def _bus_try_claim(self, ns: str) -> bool:
        """Claim `{ns}`'s single live-listener slot; False (→ 409) if it's a pre-marked
        conflict or already has a live listener."""
        with self._bus_cond:
            if ns in self._bus_conflict or ns in self._bus_live:
                return False
            self._bus_live.add(ns)
            return True

    def _bus_release(self, ns: str) -> None:
        with self._bus_cond:
            self._bus_live.discard(ns)

    def _bus_record_response(self, ns: str, env: dict) -> None:
        with self._bus_cond:
            self._bus_responses.setdefault(ns, []).append(env)
            self._bus_cond.notify_all()

    def _bus_record_egress(self) -> None:
        with self._bus_cond:
            self._egress_hits += 1
            self._bus_cond.notify_all()

    def _bus_register_sse_thread(self) -> None:
        with self._bus_cond:
            self._bus_sse_threads.add(threading.current_thread())

    def _bus_unregister_sse_thread(self) -> None:
        with self._bus_cond:
            self._bus_sse_threads.discard(threading.current_thread())

    def start(self) -> None:
        state = self

        class Handler(BaseHTTPRequestHandler):
            def log_message(self, *_args):  # silence default stderr logging
                pass

            def _send(self, code: int, body: dict):
                payload = json.dumps(body).encode()
                self.send_response(code)
                self.send_header("Content-Type", "application/json")
                self.send_header("Content-Length", str(len(payload)))
                self.end_headers()
                self.wfile.write(payload)

            def _body(self) -> dict:
                length = int(self.headers.get("Content-Length", 0))
                if not length:
                    return {}
                return json.loads(self.rfile.read(length) or b"{}")

            def do_GET(self):
                path = urlsplit(self.path).path
                # -- plugin message-bus routes (leg 3a.2) --
                if path == _EGRESS_PATH:
                    self._serve_egress()
                    return
                m = _MESSAGES_RE.match(path)
                if m:
                    self._serve_sse(unquote(m.group(1)))
                    return
                info, sheds = state.snapshot()
                if self.path == "/api/info":
                    self._send(200, info)
                elif self.path == "/api/sheds":
                    self._send(200, {"sheds": sheds})
                elif self.path == "/api/system/df":
                    self._send(200, _DF_FIXTURE)
                elif self.path == "/api/images":
                    self._send(200, _IMAGES_FIXTURE)
                elif self.path == "/api/egress/profiles":
                    self._send(200, _EGRESS_FIXTURE)
                else:
                    self._send(404, {"error": "not found"})

            # -- plugin message-bus handlers (leg 3a.2) -------------------
            # Framing derived from synthetic_bus.py / the wire catalog §0/§9 (anti-drift
            # pin): SSE `data: {json}\n\n` request frames, respond → 204, subscribe →
            # 409 NAMESPACE_ALREADY_REGISTERED when the ns is already claimed, egress →
            # 501 (unavailable). HTTP/1.0 (the handler default, as the create SSE below)
            # → the broker's reqwest client reads the streamed body until connection
            # close, exactly as the app backend already reads the create stream.
            def _serve_sse(self, ns: str):
                """Record the subscribe, then either 409 (ns already claimed / a
                pre-marked conflict — terminal `rejected`, no retry) or hold a 200
                `text/event-stream` and push queued request frames until teardown /
                client disconnect."""
                state._bus_record_subscribe(ns, self.headers.get("Authorization"))
                if not state._bus_try_claim(ns):
                    # NAMESPACE_ALREADY_REGISTERED — a second listener is terminal.
                    self._send(409, {"error": "namespace already registered",
                                     "code": "NAMESPACE_ALREADY_REGISTERED"})
                    return
                try:
                    self._stream_sse(state._bus_queue_for(ns))
                finally:
                    state._bus_release(ns)

            def _serve_egress(self):
                """The always-on egress-audit consumer route. Record the hit and 501
                (unavailable) so the broker's egress subscriber backs off — no decision
                stream is needed for these cells (wire catalog §9)."""
                state._bus_record_egress()
                self._send(501, {"error": "egress unavailable"})

            def _stream_sse(self, q):
                """Send the 200 SSE handshake, then hold the connection and drain `q`,
                writing each frame as a `data:` line, until shutdown / a `None` sentinel
                / client disconnect."""
                try:
                    self.send_response(200)
                    self.send_header("Content-Type", "text/event-stream")
                    self.send_header("Cache-Control", "no-cache")
                    self.end_headers()
                    self.wfile.flush()
                except OSError:
                    return
                state._bus_register_sse_thread()
                try:
                    while not state._bus_shutdown.is_set():
                        try:
                            frame = q.get(timeout=_SSE_POLL)
                        except queue.Empty:
                            continue
                        if frame is None:  # teardown sentinel
                            break
                        try:
                            self.wfile.write(("data: " + frame + "\n\n").encode())
                            self.wfile.flush()
                        except OSError:
                            break  # client disconnected
                finally:
                    state._bus_unregister_sse_thread()

            def _serve_respond(self, ns: str):
                """Read a response Envelope body, record it for `ns`, return 204."""
                length = int(self.headers.get("Content-Length", "0") or "0")
                body = self.rfile.read(length) if length > 0 else b""
                try:
                    env = json.loads(body)
                except ValueError:
                    self._send(400, {"error": "bad response body"})
                    return
                state._bus_record_response(ns, env)
                # 204 No Content — the status the client expects (a non-204 is a bus
                # error on both sides). Explicit zero-length, no body.
                self.send_response(204)
                self.send_header("Content-Length", "0")
                self.end_headers()

            def do_POST(self):
                path = urlsplit(self.path).path
                m = _RESPOND_RE.match(path)
                if m:
                    self._serve_respond(unquote(m.group(1)))
                    return
                parts = self.path.strip("/").split("/")  # api/sheds[/name/action]
                if self.path == "/api/sheds":
                    self._create()
                elif len(parts) == 4 and parts[:2] == ["api", "sheds"]:
                    name, action = parts[2], parts[3]
                    status = {"start": "running", "stop": "stopped", "reset": "running"}.get(action)
                    if status and state._set_status(name, status):
                        self._send(200, {"ok": True})
                    else:
                        self._send(404, {"error": "no such shed/action"})
                else:
                    self._send(404, {"error": "not found"})

            def do_DELETE(self):
                parts = self.path.strip("/").split("/")
                if len(parts) == 3 and parts[:2] == ["api", "sheds"]:
                    self._send(200 if state._delete(parts[2]) else 404, {"ok": True})
                else:
                    self._send(404, {"error": "not found"})

            def _create(self):
                body = self._body()
                with state._lock:
                    state.last_create_body = dict(body)
                name = body.get("name", "new-shed")
                # Stream SSE progress, then a complete (or error) event.
                self.send_response(200)
                self.send_header("Content-Type", "text/event-stream")
                self.end_headers()

                def frame(event: str, data: dict):
                    self.wfile.write(f"event: {event}\ndata: {json.dumps(data)}\n\n".encode())
                    self.wfile.flush()

                for msg in state.create_progress:
                    frame("progress", {"message": msg})
                    time.sleep(0.02)
                if state.create_should_fail:
                    frame("error", {"code": "create_failed", "message": f"could not create {name}"})
                    return
                shed = {
                    "name": name, "status": "running",
                    "backend": body.get("backend") or "vz",
                    "cpus": body.get("cpus") or 2,
                    "memory_mb": body.get("memory_mb") or 4096,
                    "started_at": "2026-05-31T18:33:02.364547927Z",
                }
                if body.get("repo"):
                    shed["repo"] = body["repo"]
                if body.get("image"):
                    shed["image"] = body["image"]
                state._add(shed)
                frame("complete", shed)

        self._httpd = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
        self._thread = threading.Thread(target=self._httpd.serve_forever, daemon=True)
        self._thread.start()

    def stop(self) -> None:
        # Unblock + join any live SSE handler threads first (a broker cell's held
        # subscribe), so no socket lingers past shutdown (ResourceWarning-safe),
        # mirroring synthetic_bus.stop().
        self._bus_shutdown.set()
        with self._bus_cond:
            for q in self._bus_queues.values():
                q.put(None)  # per-queue teardown sentinel
            threads = list(self._bus_sse_threads)
        for t in threads:
            t.join(timeout=2)
        if self._httpd is not None:
            self._httpd.shutdown()
            self._httpd.server_close()
            self._httpd = None
        if self._thread is not None:
            self._thread.join(timeout=2)
            self._thread = None
