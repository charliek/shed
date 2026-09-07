"""A fake `roost-session` on a Unix socket — the machine suite's far side.

Since plan 013 the Tauri app's **machine** rows come from a `roost-session`
reached over roost's SSH client-bridge. A hermetic harness cannot ssh anywhere,
so the app is launched with the test-mode `SHED_TAURI_ROOST_SOCKETS` seam, which
swaps the bridge for a direct `LocalSession` on a Unix socket — and this is what
listens there. Everything ABOVE the socket is the production path: the real
`shed_core::roost` client, the real `RoostWatcher`, the real decode, the real
row mapping.

**It is a test double, not a server.** No lease, no event stream, no PTY, no
persistence: one thread per connection, read a line, write a line. What it is
faithful about is the WIRE — every reply is built from a vendored copy of roost's
own golden vector (`crates/fixtures/roost-vectors/`, taken at the rev
`roost-ipc` is pinned to), so a shape here cannot drift away from the shape roost
publishes without the copy step noticing. It is the Python twin of
`shed_core::roost::testing::FakeRoost` (`crates/shed-core/src/roost/testing.rs`),
deliberately built from the same vectors so the Rust unit tests and this suite
cannot disagree about what a roost-session says.

Every id on this wire is a **string-int64** (a JavaScript client cannot round an
i64 through a `Number`); the control surface here takes plain ints and does the
stringifying, because a test should never have to remember which side of the
wire it is on.

## The op table

| op | answer |
|---|---|
| `session.identify` | the vendored result, with `session_protocol` / `session_id` / `started_at` from this fake's state (the compatibility gate's input) |
| `identify` | the vendored UI-socket result, verbatim |
| `tab.list` | `{projects, revision}` — the SESSION variant, which is the one that carries a revision |
| `tab.dump` | `{cols, rows, cursor, rows_text}` from this tab's text; `not-found` for an unknown id |
| `tab.open` | appends a tab built from the `tab.open` vector (cwd + an argv-derived title), bumps the revision, answers `{tab}`; the params are recorded for the test |
| `tab.close` | removes the tab and bumps the revision; `not-found` for an unknown id |
| `tab.write` | records the base64-decoded bytes for the tab; `not-found` for an unknown id |
| anything else | `unknown-op` — `session.connect` and `events.subscribe` included, so a client that reaches for the lease fails loudly here rather than passing |
"""

from __future__ import annotations

import base64
import contextlib
import copy
import json
import os
import shutil
import socket
import socketserver
import tempfile
import threading
from pathlib import Path

# .../shed/desktop/tools/shedtest/fake_roost.py → the repo root. The render gate
# recreates the same repo-root-relative layout inside its container (it tars
# `crates desktop/tauri desktop/tools …` into /work), so this resolves there too.
REPO_ROOT = Path(__file__).resolve().parents[3]
VECTORS = REPO_ROOT / "crates" / "fixtures" / "roost-vectors"


def _vector(name: str) -> dict:
    """One vendored roost vector. See the README beside them: never semantically
    edited, re-copied on a `roost-ipc` rev bump."""
    return json.loads((VECTORS / name).read_text())


_SESSION_IDENTIFY = _vector("session.identify.response.json")["result"]
_IDENTIFY = _vector("identify.response.json")["result"]
_TAB_LIST = _vector("tab.list.session.response.json")["result"]
_TAB_OPEN = _vector("tab.open.response.json")["result"]["tab"]
_ERROR = _vector("response.error.json")
_OPENCODE = _vector("shed.tab.list.opencode.finished.json")["result"]


def _first_owned_tab(listing: dict) -> dict:
    """The agent-owned tab in a `tab.list` result — the template for a session
    row. Searched for rather than indexed, so a re-copy that reorders the
    vector's tabs fails here instead of silently handing back a shell."""
    for project in listing["projects"]:
        for tab in project["tabs"]:
            if tab.get("ownership"):
                return tab
    raise AssertionError("no agent-owned tab in the vendored opencode vector")


#: A plain shell tab — somebody's terminal, and NOT a session row.
_SHELL_TAB = _TAB_LIST["projects"][0]["tabs"][0]
#: An agent-owned tab, from the vector shed recorded off a real opencode run.
_AGENT_TAB = _first_owned_tab(_OPENCODE)
#: The one project every tab lands in. roost nests tabs under projects; shed
#: flattens them, so one project is enough to exercise the flattening.
_PROJECT = {**_TAB_LIST["projects"][0], "tabs": []}

#: Sentinel for "this axis was not passed" — `None` is a meaningful value for
#: `ownership` (it un-owns the tab), so it cannot double as the default.
_UNSET = object()


class RoostWireError(Exception):
    """An `{ok: false}` envelope from a roost socket (fake or real)."""

    def __init__(self, code: str, message: str):
        super().__init__(f"{code}: {message}")
        self.code = code
        self.message = message


class _Refusal(Exception):
    """An op refusing, inside the fake. Becomes an error envelope on the wire."""

    def __init__(self, code: str, message: str):
        super().__init__(message)
        self.code = code
        self.message = message


def _error(request_id, code: str, message: str) -> dict:
    """The refusal envelope, built by overwriting the vendored error vector's own
    fields — so the key names come from roost's wire, not from memory."""
    envelope = copy.deepcopy(_ERROR)
    envelope["id"] = request_id
    envelope["error"]["code"] = code
    envelope["error"]["message"] = message
    return envelope


def _wire_id(value) -> int | None:
    """A string-int64 wire id as an int, or None if it is not one."""
    if isinstance(value, str):
        try:
            return int(value)
        except ValueError:
            return None
    return None


class _Server(socketserver.ThreadingUnixStreamServer):
    """Daemon threads + no block-on-close, so teardown cannot hang: a connection
    the app is holding open must never make `stop()` wait for the app."""

    daemon_threads = True
    block_on_close = False


class _Handler(socketserver.StreamRequestHandler):
    def handle(self) -> None:
        fake: FakeRoost = self.server.fake  # type: ignore[attr-defined]
        fake._opened(self.connection)
        try:
            for raw in self.rfile:
                line = raw.decode("utf-8", "replace").strip()
                if not line:
                    continue
                try:
                    request = json.loads(line)
                except json.JSONDecodeError as e:
                    reply = _error(None, "parse-error", str(e))
                else:
                    reply = fake.dispatch(request)
                # `sendall`, not the buffered `wfile`: an unbuffered `wfile` is a
                # raw SocketIO whose `write` may do a partial write.
                self.connection.sendall((json.dumps(reply) + "\n").encode())
        except OSError:
            # Any closed-socket read/write — a hang-up (`close_all`) or the app
            # going away mid-request. Both are normal ends to a connection.
            pass
        finally:
            fake._closed(self.connection)


class FakeRoost:
    """A stand-in `roost-session` listening on a Unix socket.

    Use it as a context manager, or `start()` / `stop()` by hand::

        with FakeRoost() as fake:
            fake.add_tab(4, cwd="/home/shed/work", title="OC", source="opencode")
            ...  # point the app at fake.socket_path

    `socket_path` may be pinned by the caller (the localhost test needs a path
    the app is launched with BEFORE anything listens on it); otherwise the fake
    owns a short throwaway directory under `/tmp` — short because a `sun_path` is
    108 bytes and a socket under a long temp path fails to bind.
    """

    def __init__(self, *, socket_path: str | os.PathLike[str] | None = None):
        self._own_dir = socket_path is None
        if socket_path is None:
            self._dir = Path(tempfile.mkdtemp(prefix="fkroost-", dir="/tmp"))
            self._socket_path = self._dir / "roost.sock"
        else:
            self._socket_path = Path(socket_path)
            self._dir = self._socket_path.parent
        self._lock = threading.Lock()
        self._server: _Server | None = None
        self._thread: threading.Thread | None = None
        self._live: set[socket.socket] = set()

        # -- the state a test pokes ---------------------------------------
        self.session_protocol: int = _SESSION_IDENTIFY["session_protocol"]
        self._session_id: str = _SESSION_IDENTIFY["session_id"]
        self._started_at: str = _SESSION_IDENTIFY["started_at"]
        self._revision: int = _TAB_LIST["revision"]
        self._projects: list[dict] = [copy.deepcopy(_PROJECT)]
        self._writes: dict[int, bytes] = {}
        self._dumps: dict[int, list[str]] = {}
        self._restarts = 0
        #: Every `tab.open` this fake served, params verbatim, in order.
        self.opens: list[dict] = []

    # -- lifecycle ---------------------------------------------------------
    def start(self) -> "FakeRoost":
        self._socket_path.parent.mkdir(parents=True, exist_ok=True)
        # A stale socket from a crashed run would make `bind` fail with EADDRINUSE.
        with contextlib.suppress(FileNotFoundError):
            self._socket_path.unlink()
        server = _Server(str(self._socket_path), _Handler)
        server.fake = self  # type: ignore[attr-defined]
        self._server = server
        self._thread = threading.Thread(target=server.serve_forever, daemon=True)
        self._thread.start()
        return self

    def stop(self) -> None:
        """Hang up, stop listening, and take the socket with us.

        The socket file must GO: a `LocalSession` reach reports "no roost-session
        at <path>" from the path not existing, which is how a test produces the
        durable gone state rather than a transient hang-up."""
        self.close_all()
        if self._server is not None:
            self._server.shutdown()
            self._server.server_close()
            self._server = None
        if self._own_dir:
            shutil.rmtree(self._dir, ignore_errors=True)
        else:
            with contextlib.suppress(FileNotFoundError):
                self._socket_path.unlink()

    def __enter__(self) -> "FakeRoost":
        return self.start()

    def __exit__(self, *_exc) -> None:
        self.stop()

    @property
    def socket_path(self) -> Path:
        return self._socket_path

    # -- the control surface ----------------------------------------------
    @property
    def revision(self) -> int:
        """The commit revision `tab.list` currently reports."""
        with self._lock:
            return self._revision

    @property
    def session_id(self) -> str:
        """The daemon-INSTANCE id `session.identify` currently reports."""
        with self._lock:
            return self._session_id

    def add_tab(self, tab_id: int, *, cwd: str, title: str, source: str | None = None,
                session_id: str = "", lifecycle: str = "inactive", detail: str = "",
                has_notification: bool = False, shell_state: str = "at_prompt") -> dict:
        """Add a tab and commit a revision.

        `source` is roost's open `ownership.source` string (`opencode`, `claude`,
        …). **`None` means a plain shell tab** — no `ownership` key at all, which
        is what roost's wire carries for somebody's terminal, and what makes it
        NOT a session row.
        """
        with self._lock:
            tab = copy.deepcopy(_AGENT_TAB if source else _SHELL_TAB)
            tabs = self._projects[0]["tabs"]
            tab["id"] = str(tab_id)
            tab["project_id"] = self._projects[0]["id"]
            tab["title"] = title
            tab["cwd"] = cwd
            tab["shell_state"] = shell_state
            tab["agent_lifecycle"] = lifecycle
            tab["has_notification"] = has_notification
            tab["position"] = len(tabs)
            if source:
                tab["ownership"] = _ownership(source, session_id, detail,
                                              tab.get("last_active", 0))
            else:
                tab.pop("ownership", None)
            tabs.append(tab)
            self._revision += 1
            return copy.deepcopy(tab)

    def set_axes(self, tab_id: int, *, lifecycle=_UNSET, has_notification=_UNSET,
                 detail=_UNSET, ownership=_UNSET) -> None:
        """Set a tab's agent axes and commit a revision — the whole point of the
        fake for a status test.

        `ownership=None` un-owns the tab (the key is REMOVED, as roost's wire has
        it), which drops it from the row set; a dict replaces it wholesale.
        `detail` edits the current ownership's detail in place.
        """
        with self._lock:
            tab = self._tab(tab_id)
            if tab is None:
                raise KeyError(f"no tab {tab_id} to set axes on")
            if lifecycle is not _UNSET:
                tab["agent_lifecycle"] = lifecycle
            if has_notification is not _UNSET:
                tab["has_notification"] = has_notification
            if ownership is not _UNSET:
                if ownership is None:
                    tab.pop("ownership", None)
                else:
                    tab["ownership"] = copy.deepcopy(ownership)
            if detail is not _UNSET:
                if "ownership" not in tab:
                    raise KeyError(f"tab {tab_id} has no ownership to detail")
                tab["ownership"]["detail"] = detail
            self._revision += 1

    def bump_revision(self) -> None:
        """Commit a revision with no visible change — roost pushes those too."""
        with self._lock:
            self._revision += 1

    def restart(self) -> None:
        """Restart the daemon: a new `session_id`, `revision` back to 1, **tab
        ids unchanged**.

        Both halves are real roost behaviour — ids are persisted, the revision
        counter is in-process — and together they are why a client fences per
        connection, keys rows by tab id, and compares `(session_id, revision)`
        rather than watching the number climb.
        """
        with self._lock:
            self._restarts += 1
            self._session_id = f"{_SESSION_IDENTIFY['session_id']}-restart-{self._restarts}"
            self._revision = 1

    def set_dump(self, tab_id: int, rows_text: list[str]) -> None:
        """What `tab.dump` reports for a tab (one entry per visible row)."""
        with self._lock:
            self._dumps[tab_id] = list(rows_text)

    def written(self, tab_id: int) -> bytes:
        """The bytes `tab.write` delivered to a tab, in order."""
        with self._lock:
            return self._writes.get(tab_id, b"")

    def tab_ids(self) -> list[int]:
        """The tab ids this session currently holds, in list order."""
        with self._lock:
            return [int(t["id"]) for p in self._projects for t in p["tabs"]]

    def close_all(self) -> None:
        """Hang up on every connection that is live right now. Connections
        accepted afterwards are unaffected — this is a hang-up, not a shutdown."""
        with self._lock:
            live = list(self._live)
        for conn in live:
            with contextlib.suppress(OSError):
                conn.shutdown(socket.SHUT_RDWR)

    # -- connection bookkeeping -------------------------------------------
    def _opened(self, conn: socket.socket) -> None:
        with self._lock:
            self._live.add(conn)

    def _closed(self, conn: socket.socket) -> None:
        with self._lock:
            self._live.discard(conn)

    # -- the wire ----------------------------------------------------------
    def dispatch(self, request: dict) -> dict:
        """One request → one reply envelope. The request `id` is echoed EXACTLY:
        a client matches replies by it, and a fake that normalized it would hide
        exactly the bug that matters."""
        request_id = request.get("id")
        op = request.get("op") or ""
        params = request.get("params") or {}
        try:
            result = self._op(op, params)
        except _Refusal as refusal:
            return _error(request_id, refusal.code, refusal.message)
        return {"id": request_id, "ok": True, "result": result}

    def _op(self, op: str, params: dict) -> dict:
        with self._lock:
            if op == "session.identify":
                return {
                    **copy.deepcopy(_SESSION_IDENTIFY),
                    "session_protocol": self.session_protocol,
                    "session_id": self._session_id,
                    "started_at": self._started_at,
                }
            if op == "identify":
                return copy.deepcopy(_IDENTIFY)
            if op == "tab.list":
                return {
                    "projects": copy.deepcopy(self._projects),
                    "revision": self._revision,
                }
            if op == "tab.dump":
                return self._dump(_require_tab_id(params))
            if op == "tab.open":
                return self._open(params)
            if op == "tab.close":
                return self._close(_require_tab_id(params))
            if op == "tab.write":
                return self._write(_require_tab_id(params), params)
            # Everything else, `session.connect` and `events.subscribe`
            # included: this fake serves inventory and one-shots.
            raise _Refusal("unknown-op", f"no such op: {op}")

    # -- op bodies (all called under the lock) -----------------------------
    def _tab(self, tab_id: int) -> dict | None:
        for project in self._projects:
            for tab in project["tabs"]:
                if _wire_id(tab["id"]) == tab_id:
                    return tab
        return None

    def _require(self, tab_id: int) -> dict:
        tab = self._tab(tab_id)
        if tab is None:
            raise _Refusal("not-found", f"no such tab: {tab_id}")
        return tab

    def _dump(self, tab_id: int) -> dict:
        tab = self._require(tab_id)
        rows_text = self._dumps.get(tab_id) or [
            f"tab {tab_id} {tab['title']}",
            f"cwd {tab['cwd']}",
            "",
        ]
        return {
            "cols": 80,
            "rows": len(rows_text),
            "cursor": {"row": 0, "col": 0, "visible": True},
            "rows_text": list(rows_text),
        }

    def _open(self, params: dict) -> dict:
        self.opens.append(copy.deepcopy(params))
        argv = params.get("argv") or []
        cwd = params.get("cwd") or ""
        tab = copy.deepcopy(_TAB_OPEN)
        tab_id = max([int(t["id"]) for p in self._projects for t in p["tabs"]], default=0) + 1
        tabs = self._projects[0]["tabs"]
        tab["id"] = str(tab_id)
        tab["project_id"] = self._projects[0]["id"]
        tab["cwd"] = cwd
        # roost derives an empty title from the tab's command (and then the
        # foreground process renames it); a fresh tab is never agent-OWNED —
        # the adapter's first report is what claims it.
        tab["title"] = params.get("title") or (argv[0] if argv else os.path.basename(cwd))
        tab["position"] = len(tabs)
        tabs.append(tab)
        self._revision += 1
        return {"tab": copy.deepcopy(tab)}

    def _close(self, tab_id: int) -> dict:
        self._require(tab_id)
        for project in self._projects:
            project["tabs"] = [t for t in project["tabs"] if _wire_id(t["id"]) != tab_id]
        self._revision += 1
        return {}

    def _write(self, tab_id: int, params: dict) -> dict:
        self._require(tab_id)
        data = params.get("data")
        if not isinstance(data, str):
            raise _Refusal("invalid-param", "tab.write needs base64 `data`")
        try:
            decoded = base64.b64decode(data, validate=True)
        except ValueError as e:
            raise _Refusal("invalid-param", f"tab.write data: {e}") from e
        self._writes[tab_id] = self._writes.get(tab_id, b"") + decoded
        return {}


def _ownership(source: str, session_id: str, detail: str, last_event_at: int) -> dict:
    """A minimal roost `Ownership`, so a test never hand-writes the shape."""
    return {
        "source": source,
        "session_id": session_id,
        "last_event_at": last_event_at,
        "detail": detail,
        "metadata": {},
    }


def _require_tab_id(params: dict) -> int:
    tab_id = _wire_id(params.get("tab_id"))
    if tab_id is None:
        raise _Refusal("invalid-param", "a string-int64 `tab_id` is required")
    return tab_id


def roost_call(socket_path: str | os.PathLike[str], op: str,
               params: dict | None = None, timeout: float = 10.0) -> dict:
    """One request against a roost socket — the fake's, or a REAL daemon's.

    Deliberately its own tiny client rather than a method on `FakeRoost`: the
    `real_roost` smoke drives an actual `roost-session` over the same wire, and a
    helper that only worked against the fake would prove nothing about it.
    """
    conn = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
    conn.settimeout(timeout)
    try:
        conn.connect(str(socket_path))
        conn.sendall((json.dumps({"id": "1", "op": op, "params": params or {}}) + "\n").encode())
        buf = b""
        while b"\n" not in buf:
            chunk = conn.recv(1 << 16)
            if not chunk:
                raise RoostWireError("disconnected", "socket closed mid-response")
            buf += chunk
        reply = json.loads(buf.split(b"\n", 1)[0].decode())
    finally:
        conn.close()
    if not reply.get("ok"):
        err = reply.get("error") or {}
        raise RoostWireError(err.get("code", "unknown"), err.get("message", ""))
    return reply.get("result") or {}
