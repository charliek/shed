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
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

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

            def do_POST(self):
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
        if self._httpd is not None:
            self._httpd.shutdown()
            self._httpd.server_close()
            self._httpd = None
        if self._thread is not None:
            self._thread.join(timeout=2)
            self._thread = None
