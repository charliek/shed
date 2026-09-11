#!/usr/bin/env python3
"""A control door on the fakes, for a harness in another language (plan 018 §3.6).

Hosts one of `fake_gx.FakeGx` / `fake_opencode.FakeOpencode` and exposes its
knobs over a second, loopback-only HTTP port — so a Dart (or any other
language's) test can seed a session, push a frame, or ask "how many streams are
live" without re-deriving either fake in its own runtime.

```
fake_lane_server.py --agent gx|opencode [--home DIR] [--token T] [--instance-id I]
→ stdout, one line: {"agent":"gx","port":41234,"reported_url":"http://127.0.0.1:41234",
                      "home":"/tmp/…","control":"http://127.0.0.1:41235"}
POST /_/<method>   body: {"args": [...], "kwargs": {...}} → 200 JSON result | 400 {"error":…}
GET  /_/info       → the same JSON as the startup line
POST /_/stop
```

`<method>` is an **explicit per-agent allowlist** (`GX_METHODS` / `OPENCODE_METHODS`
below — a dict, not introspection over the fake's public methods), so a wire a
Dart test depends on can't silently grow or shrink just because someone added a
helper to `fake_gx.py`. The envelope builders (`permission_request`,
`chunk`, `turn_completed`, …) are reached at `POST /_/envelope/<name>`, whose
body IS the kwargs object directly (no `args`/`kwargs` wrapping — there are no
positional-only envelope builders, so the ambiguity doesn't arise there).
Lifecycle (`stop`) is never in the allowlist; it is its own route.

Every result is made JSON-serialisable by construction: allowlisted functions
either return plain dicts/lists/scalars already, or (for the three cases
that don't — gx's `requests()` and `bodies_to()` lists of `RequestRecord`, and
opencode's `post_paths` attribute) are given a small adapter here that shapes
the result before it hits `json.dumps`. A `Path` or a dataclass instance reaching the encoder is
still handled (`_Encoder` below), for any future knob that returns one
directly, but nothing shipped today relies on that fallback.

A **stdin-EOF watchdog** runs as a daemon thread: a test that holds this
process's stdin pipe open controls its lifetime for free, and a `flutter
test`/pytest run that gets killed without an explicit `/_/stop` never leaves an
orphaned fake bound to a loopback port in CI.

stdlib only (`argparse`, `http.server`, `json`, `threading`, …) — this file runs
under a bare `python3`, no virtualenv, no third-party imports, so shed-mobile's
CI needs no `uv`.
"""

from __future__ import annotations

import argparse
import dataclasses
import json
import os
import sys
import threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
from typing import Any, Callable
from urllib.parse import unquote, urlsplit

sys.path.insert(0, str(Path(__file__).resolve().parent))

import fake_gx  # noqa: E402
import fake_opencode  # noqa: E402

#: How long the stdin-EOF watchdog's read loop is allowed to block on any one
#: `readline()` before it re-checks — irrelevant in practice (`readline()`
#: returns immediately on EOF), kept only so the thread is never truly stuck.
_WATCHDOG_POLL = 5.0


class _Encoder(json.JSONEncoder):
    """`Path` → `str`, a dataclass instance → its `dict`. A safety net: every
    allowlisted call today already returns something plain, but a knob added
    later that forgets to shape its own result still serialises."""

    def default(self, obj: Any) -> Any:
        if isinstance(obj, Path):
            return str(obj)
        if dataclasses.is_dataclass(obj) and not isinstance(obj, type):
            return dataclasses.asdict(obj)
        return super().default(obj)


class ServerState:
    """Everything the control handler needs: which fake, which allowlist, and
    the `--home` this process started with (the composite's fallback)."""

    def __init__(self, agent: str, fake: Any, home: Path | None,
                 methods: dict[str, Callable], envelopes: dict[str, Callable]):
        self.agent = agent
        self.fake = fake
        self.home = home
        self.methods = methods
        self.envelopes = envelopes
        self.control_port = 0
        self.stopped = threading.Event()

    def info(self) -> dict:
        reported = self.fake.reported_url if self.agent == "gx" else self.fake.base_url
        return {
            "agent": self.agent,
            "port": self.fake.port,
            "reported_url": reported,
            "home": str(self.home) if self.home else None,
            "control": f"http://127.0.0.1:{self.control_port}",
        }


# ---------------------------------------------------------------------------
# the allowlists — explicit dicts, never introspection
# ---------------------------------------------------------------------------


def _call(name: str) -> Callable[[ServerState, list, dict], Any]:
    """A knob that is already exactly `fake.<name>(*args, **kwargs)`."""

    def _fn(state: ServerState, args: list, kwargs: dict) -> Any:
        return getattr(state.fake, name)(*args, **kwargs)

    return _fn


def _gx_requests(state: ServerState, _args: list, _kwargs: dict) -> list[dict]:
    """`requests()` returns `RequestRecord` (a `__slots__` class, not a
    dataclass) — shaped here into the wire the plan pins: `{method, path,
    query, had_bearer, bearer_ok}`. `last_event_id`/`body` are dropped, and the
    token itself was never in the ledger to begin with.

    Bodies are reachable through `bodies_to` rather than here: this shape is
    pinned (a cell asserts the key set exactly), and widening it to carry every
    request's body would put a posted token-bearing body into the one ledger a
    test prints on failure."""
    return [
        {
            "method": r.method,
            "path": r.path,
            "query": r.query,
            "had_bearer": r.had_bearer,
            "bearer_ok": r.bearer_ok,
        }
        for r in state.fake.requests()
    ]


def _gx_bodies_to(state: ServerState, args: list, kwargs: dict) -> list[str]:
    """Every recorded body whose path ends with `suffix`, in order served.

    A list, not `FakeGx.body_of`'s first match: the cell that proves
    `mode: "interject"` posts to `…/messages` twice — a queued send first, then
    the interject — and asserting the FIRST body there would assert the wrong
    one and pass.
    """
    suffix = kwargs.get("suffix")
    if suffix is None and len(args) > 0:
        suffix = args[0]
    # A nonempty STRING, not merely "not None": `"".endswith` is true of every
    # path, so an empty suffix would quietly hand back the whole ledger — the
    # one answer a body assertion must never silently receive.
    if not isinstance(suffix, str) or not suffix:
        raise ValueError("bodies_to requires a nonempty string suffix")
    return [r.body for r in state.fake.requests_to(suffix)]


def _restart_leader_and_rewrite_home(state: ServerState, args: list, kwargs: dict) -> dict:
    """`restart_leader` then `write_home` — a composite because `restart_leader`
    deliberately does not rewrite the discovery record itself
    (`fake_gx.py:restart_leader`'s own docstring). `home` defaults to whatever
    `--home` this process started with, so the common case
    (`{"args": ["new-instance-id"]}`) needs no caller-side path plumbing."""
    instance_id = kwargs.get("instance_id")
    if instance_id is None and len(args) > 0:
        instance_id = args[0]
    if instance_id is None:
        raise ValueError("restart_leader_and_rewrite_home requires instance_id")

    home = kwargs.get("home")
    if home is None and len(args) > 1:
        home = args[1]
    if home is None:
        home = state.home
    if home is None:
        raise ValueError(
            "restart_leader_and_rewrite_home requires a home (pass one, or "
            "start fake_lane_server.py with --home)")

    state.fake.restart_leader(instance_id)
    written = state.fake.write_home(home)
    return {"instance_id": instance_id, "home": str(written)}


def _opencode_post_paths(state: ServerState, _args: list, _kwargs: dict) -> list[str]:
    """`post_paths` is a plain list attribute on `FakeOpencode`, not a method —
    exposed here as a zero-arg read."""
    return list(state.fake.post_paths)


GX_METHODS: dict[str, Callable[[ServerState, list, dict], Any]] = {
    "add_session": _call("add_session"),
    "set_history": _call("set_history"),
    "push_update": _call("push_update"),
    "stage_update": _call("stage_update"),
    "push_session_frame": _call("push_session_frame"),
    "push_session_removed": _call("push_session_removed"),
    "push_approval_frame": _call("push_approval_frame"),
    "push_reset": _call("push_reset"),
    "push_keepalive": _call("push_keepalive"),
    "close_streams": _call("close_streams"),
    "stream_count": _call("stream_count"),
    "add_approval": _call("add_approval"),
    "add_placeholder_approval": _call("add_placeholder_approval"),
    "resolve_approval": _call("resolve_approval"),
    "answered_with": _call("answered_with"),
    "fail": _call("fail"),
    "clear_failures": _call("clear_failures"),
    "set_instance_id": _call("set_instance_id"),
    "restart_leader_and_rewrite_home": _restart_leader_and_rewrite_home,
    "requests": _gx_requests,
    "bodies_to": _gx_bodies_to,
    "clear_requests": _call("clear_requests"),
    "hold_seed": _call("hold_seed"),
    "release_seed": _call("release_seed"),
}

#: `POST /_/envelope/<name>` — the wire vocabulary a Dart test never re-derives.
#: opencode has no equivalent route: its `stream_*` methods already build AND
#: broadcast their own envelopes in one call.
GX_ENVELOPES: dict[str, Callable[..., dict]] = {
    "chunk": fake_gx.chunk,
    "turn_completed": fake_gx.turn_completed,
    "hook": fake_gx.hook,
    "tool_call": fake_gx.tool_call,
    "tool_call_update": fake_gx.tool_call_update,
    "permission_request": fake_gx.permission_request,
    "opaque_permission_request": fake_gx.opaque_permission_request,
    "question_request": fake_gx.question_request,
    "plan_request": fake_gx.plan_request,
    "elicitation_request": fake_gx.elicitation_request,
}

OPENCODE_METHODS: dict[str, Callable[[ServerState, list, dict], Any]] = {
    "add_session": _call("add_session"),
    "remove_session": _call("remove_session"),
    "set_status": _call("set_status"),
    "set_messages": _call("set_messages"),
    "set_simple_transcript": _call("set_simple_transcript"),
    "add_permission": _call("add_permission"),
    "add_question": _call("add_question"),
    "remove_request": _call("remove_request"),
    "stream_part": _call("stream_part"),
    "stream_permission_asked": _call("stream_permission_asked"),
    "stream_permission_replied": _call("stream_permission_replied"),
    "stream_question_asked": _call("stream_question_asked"),
    "stream_idle": _call("stream_idle"),
    "stream_session_created": _call("stream_session_created"),
    "close_streams": _call("close_streams"),
    "stream_count": _call("stream_count"),
    "fail_post": _call("fail_post"),
    "fail_get": _call("fail_get"),
    "post_body": _call("post_body"),
    "post_paths": _opencode_post_paths,
    "snapshot": _call("snapshot"),
    "hold_seed": _call("hold_seed"),
    "release_seed": _call("release_seed"),
}


# ---------------------------------------------------------------------------
# the control HTTP door
# ---------------------------------------------------------------------------


def _make_control_handler(state: ServerState) -> type[BaseHTTPRequestHandler]:
    class ControlHandler(BaseHTTPRequestHandler):
        protocol_version = "HTTP/1.1"
        timeout = 5

        def log_message(self, *_args):  # quiet
            pass

        def _reply(self, status: int, payload: Any) -> None:
            raw = json.dumps(payload, cls=_Encoder).encode()
            self.send_response(status)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(raw)))
            self.end_headers()
            self.wfile.write(raw)

        def do_GET(self):  # noqa: N802 - http.server API
            path = unquote(urlsplit(self.path).path)
            if path == "/_/info":
                return self._reply(200, state.info())
            return self._reply(404, {"error": f"unknown route {path!r}"})

        def do_POST(self):  # noqa: N802 - http.server API
            length = int(self.headers.get("Content-Length") or 0)
            raw = self.rfile.read(length) if length else b""
            path = unquote(urlsplit(self.path).path)
            try:
                body = json.loads(raw.decode("utf-8")) if raw else {}
            except ValueError:
                return self._reply(400, {"error": "body is not valid JSON"})

            if path == "/_/stop":
                self._reply(200, {"stopped": True})
                state.stopped.set()
                return

            if path.startswith("/_/envelope/"):
                name = path[len("/_/envelope/"):]
                builder = state.envelopes.get(name)
                if builder is None:
                    return self._reply(400, {"error": f"unknown envelope {name!r}"})
                kwargs = body if isinstance(body, dict) else {}
                try:
                    result = builder(**kwargs)
                except Exception as exc:  # noqa: BLE001 - report to the caller
                    return self._reply(400, {"error": str(exc)})
                return self._reply(200, result)

            if path.startswith("/_/"):
                name = path[len("/_/"):]
                fn = state.methods.get(name)
                if fn is None:
                    return self._reply(400, {"error": f"unknown method {name!r}"})
                obj = body if isinstance(body, dict) else {}
                # `.get(k, default)`, not `.get(k) or default`: a caller that
                # sends `"args": ""` or `"args": 0` has sent something wrong and
                # must get a 400, not have it silently swapped for an empty list
                # by a falsy check.
                args = obj.get("args", [])
                kwargs = obj.get("kwargs", {})
                if not isinstance(args, list) or not isinstance(kwargs, dict):
                    return self._reply(
                        400, {"error": "'args' must be a list, 'kwargs' an object"})
                try:
                    result = fn(state, args, kwargs)
                except Exception as exc:  # noqa: BLE001 - report to the caller
                    return self._reply(400, {"error": str(exc)})
                return self._reply(200, result)

            return self._reply(404, {"error": f"unknown route {path!r}"})

    return ControlHandler


def _watchdog(state: ServerState) -> None:
    """Exit the process once stdin hits EOF — the parent (a Dart or Python
    test) holds this pipe open for exactly as long as it wants this fake
    alive, so a killed test run never leaves an orphan bound to a port."""
    try:
        while True:
            line = sys.stdin.readline()
            if line == "":
                break
    except Exception:  # noqa: BLE001 - EOF is the only signal that matters
        pass
    try:
        state.fake.stop()
    except Exception:  # noqa: BLE001 - best-effort only
        pass
    os._exit(0)


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(prog="fake_lane_server.py")
    parser.add_argument("--agent", required=True, choices=("gx", "opencode"))
    parser.add_argument("--home", default=None,
                        help="gx: write $GROK_HOME here at startup (write_home)")
    parser.add_argument("--token", default=None, help="gx only")
    parser.add_argument("--instance-id", default=None, help="gx only")
    args = parser.parse_args(argv)

    if args.agent == "gx":
        kwargs: dict[str, Any] = {}
        if args.token is not None:
            kwargs["token"] = args.token
        if args.instance_id is not None:
            kwargs["instance_id"] = args.instance_id
        fake: Any = fake_gx.FakeGx(**kwargs)
        methods, envelopes = GX_METHODS, GX_ENVELOPES
    else:
        fake = fake_opencode.FakeOpencode()
        methods, envelopes = OPENCODE_METHODS, {}

    home = Path(args.home) if args.home else None
    if home is not None and args.agent == "gx":
        fake.write_home(home)

    state = ServerState(args.agent, fake, home, methods, envelopes)

    control = ThreadingHTTPServer(("127.0.0.1", 0), _make_control_handler(state))
    state.control_port = control.server_address[1]
    control_thread = threading.Thread(target=control.serve_forever, daemon=True)
    control_thread.start()

    threading.Thread(target=_watchdog, args=(state,), daemon=True).start()

    print(json.dumps(state.info()), flush=True)

    state.stopped.wait()

    control.shutdown()
    control.server_close()
    try:
        fake.stop()
    except Exception:  # noqa: BLE001 - best-effort only
        pass
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
