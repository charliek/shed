"""A shared fake opencode server for the hub family's LANE cells (plan 010
§2.9 family 4) — the harness-side equivalent of the Go/Rust test doubles
(`fakeOpencode`): a programmable `/event` SSE stream, canned REST bodies,
injectable POST statuses, request recording, and the **pinGuard** — any POST to
a global route or to a session other than the pinned one is recorded as a
violation (the WS-B invariant: a hub-initiated mutation may only ever address
the pinned session's three scoped routes).

Each leg gets its OWN instance with an identical script, so the two hubs are
driven by byte-identical upstreams. Stdlib only (http.server + threading), per
the suite's no-dependency posture.
"""

import json
import re
import threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

# The scoped-mutation grammar the pin guard enforces (the Go double's
# `ocScopedMutationRe`).
_SCOPED_RE = re.compile(r"^/session/([^/]+)/(prompt_async|abort|permissions/[^/]+)$")


class FakeOpencode:
    """One leg's fake embedded opencode server, bound to a caller-chosen port
    (the engine allocates the session's port at create; the fake binds it
    afterwards, before the hub's watcher first connects)."""

    def __init__(self, port: int):
        self._lock = threading.Lock()
        self._frames: list = []  # broadcast SSE payload strings (JSON)
        self._stopped = threading.Event()
        self.post_paths: list = []
        self.post_bodies: dict = {}
        self.violations: list = []
        self.pin = ""
        # POST status overrides keyed by route suffix.
        self.post_status: dict = {}
        outer = self

        class Handler(BaseHTTPRequestHandler):
            protocol_version = "HTTP/1.1"
            # Bounds the keep-alive read after a handled request, so no
            # handler thread parks in readline() past stop().
            timeout = 1

            def log_message(self, *_args):  # quiet
                pass

            def _json(self, status: int, body: str):
                raw = body.encode()
                self.send_response(status)
                self.send_header("Content-Type", "application/json")
                self.send_header("Content-Length", str(len(raw)))
                self.end_headers()
                self.wfile.write(raw)

            def do_GET(self):  # noqa: N802 - http.server API
                if self.path == "/event":
                    self.send_response(200)
                    self.send_header("Content-Type", "text/event-stream")
                    # Close-delimited stream: the connection is not reusable,
                    # and the handler thread ends with it at stop().
                    self.send_header("Connection", "close")
                    self.end_headers()
                    cursor = 0
                    try:
                        self.wfile.write(
                            b'data: {"type":"server.connected","properties":{}}\n\n'
                        )
                        self.wfile.flush()
                        while not outer._stopped.is_set():
                            with outer._lock:
                                pending = outer._frames[cursor:]
                                cursor = len(outer._frames)
                            for frame in pending:
                                self.wfile.write(
                                    b"data: " + frame.encode() + b"\n\n"
                                )
                                self.wfile.flush()
                            outer._stopped.wait(0.02)
                    except (BrokenPipeError, ConnectionResetError):
                        pass
                    return
                if self.path == "/session":
                    return self._json(200, "[]")
                if self.path == "/session/status":
                    return self._json(200, "{}")
                if self.path in ("/permission", "/question"):
                    return self._json(200, "[]")
                if re.match(r"^/session/[^/]+/message$", self.path):
                    return self._json(200, "[]")
                return self._json(404, "{}")

            def do_POST(self):  # noqa: N802 - http.server API
                length = int(self.headers.get("Content-Length") or 0)
                body = self.rfile.read(length).decode("utf-8", "replace")
                with outer._lock:
                    outer.post_paths.append(self.path)
                    outer.post_bodies[self.path] = body
                    # The pin guard (WS-B): every hub-initiated mutation must
                    # be one of the three scoped routes on the PINNED session.
                    m = _SCOPED_RE.match(self.path)
                    if not m:
                        outer.violations.append(
                            f"not a session-scoped mutation route: POST {self.path}"
                        )
                    elif outer.pin and m.group(1) != outer.pin:
                        outer.violations.append(
                            f"addressed session {m.group(1)}, not the pinned {outer.pin}"
                        )
                    # Mutation-response fidelity (the Go double's
                    # serveMutation): a VIOLATION answers 500 so the offending
                    # verb can never look successful; prompt_async answers 204
                    # empty; abort/permissions answer 200 `true`.
                    if outer.violations:
                        status, body_out = 500, "{}"
                    elif self.path.endswith("/prompt_async"):
                        status, body_out = 204, ""
                    else:
                        status, body_out = 200, "true"
                    for suffix, code in outer.post_status.items():
                        if self.path.endswith(suffix):
                            status = code
                if status == 204:
                    self.send_response(204)
                    self.send_header("Content-Length", "0")
                    self.end_headers()
                    return None
                return self._json(status, body_out)

        self._server = ThreadingHTTPServer(("127.0.0.1", port), Handler)
        self._thread = threading.Thread(target=self._server.serve_forever, daemon=True)
        self._thread.start()

    def stream(self, payload: dict) -> None:
        """Broadcast one SSE payload to every current (and future) /event
        connection — the harness spelling of the Go double's scripted stream."""
        with self._lock:
            self._frames.append(json.dumps(payload))

    def stream_ask(self, sid: str, ask_id: str) -> None:
        self.stream(
            {
                "type": "permission.asked",
                "properties": {
                    "id": ask_id,
                    "sessionID": sid,
                    "permission": "bash",
                    "patterns": ["ls"],
                    "metadata": {"command": "ls -la"},
                },
            }
        )

    def stream_session_created(self, sid: str, directory: str) -> None:
        """A ROOT session.created whose directory matches the watcher's workdir
        — the §3.3 trusted pin source."""
        self.stream(
            {
                "type": "session.created",
                "properties": {"info": {"id": sid, "directory": directory}},
            }
        )

    def snapshot(self) -> dict:
        with self._lock:
            return {
                "post_paths": list(self.post_paths),
                "violations": list(self.violations),
            }

    def stop(self) -> None:
        self._stopped.set()
        self._server.shutdown()
        self._server.server_close()
        self._thread.join(timeout=5)
