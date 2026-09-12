"""A fake `roost-session` on a Unix socket — the machine suite's far side.

Since plan 013 the Tauri app's **machine** rows come from a `roost-session`
reached over roost's SSH client-bridge. A hermetic harness cannot ssh anywhere,
so the app is launched with the test-mode `SHED_TAURI_ROOST_SOCKETS` seam, which
swaps the bridge for a direct `LocalSession` on a Unix socket — and this is what
listens there. Everything ABOVE the socket is the production path: the real
`shed_core::roost` client, the real `RoostWatcher`, the real decode, the real
row mapping.

**It is a test double, not a server.** No PTY, no persistence: one thread per
connection, read a line and write a line — until `events.subscribe` flips the
connection into a one-way push stream, which is the one place the shape genuinely
differs. What it is faithful about is the WIRE — every reply and every pushed
envelope is built from a vendored copy of roost's own golden vector
(`crates/fixtures/roost-vectors/`, taken at the rev `roost-ipc` is pinned to), so
a shape here cannot drift away from the shape roost publishes without the copy
step noticing.

It is the Python twin of `shed_core::roost::testing::FakeRoost`
(`crates/shed-core/src/roost/testing.rs`) — the same controls, the same
semantics, deliberately built from the same vectors so the Rust unit tests and
this suite cannot disagree about what a roost-session says. **When you change one
fake, change the other**, and pin the change in both `testing.rs`'s tests and
`test_fake_roost.py` (the wire self-test that exists precisely because the Tauri
app never asks for a filtered subscribe or drives two connections at
`session.set_agent_hooks` concurrently, so nothing else here would catch this
file drifting).

At session protocol 5 the wire it speaks is unowned — the lease, its takeover
table and its one tombstone retired with generation 4 (roost#477) — so what is
left to be faithful about is the stream:

* **`events.subscribe` registers, it does not classify.** The ack's `revision`
  and `session_id` and the subscriber's registration are taken under **one**
  lock, so no mutation can commit between the number a client fences on and the
  first frame it is eligible to receive — a fake that let one slip through
  would manufacture the very gap the resync tests exist to distinguish from a
  real one.
* **Every mutation commits exactly one batch** at the new revision, built from
  the vendored envelope vectors. Empty commits are pushed too, because that is
  what makes a skipped revision mean loss and nothing else.
* **Every write op is open to every connection.** `tab.write` and
  `session.set_agent_hooks` take no authority, two connections can both drive
  the same tab, and the last writer wins — which is the one behaviour
  generation 5 actually changed and the one the deleted refusal table was
  standing in front of.

Every id on this wire is a **string-int64** (a JavaScript client cannot round an
i64 through a `Number`); the control surface here takes plain ints and does the
stringifying, because a test should never have to remember which side of the
wire it is on.

## The op table

| op | answer |
|---|---|
| `session.identify` | the vendored `.v5` result, with `session_protocol` / `session_id` / `started_at` from this fake's state (the compatibility gate's input); `unknown-op` on a UI socket |
| `identify` | the vendored UI-socket result, verbatim |
| `tab.list` | `{projects, revision}` — the SESSION variant; a UI socket omits `revision` ENTIRELY (it serves no stream, so it publishes no fence) |
| `tab.dump` | `{cols, rows, cursor, rows_text}` from this tab's text; `not-found` for an unknown id |
| `tab.open` | appends a tab built from the `tab.open` vector, commits a `tab.opened` batch, answers `{tab}`; the params are recorded for the test |
| `tab.close` | removes the tab and commits a `tab.closed` batch; `not-found` for an unknown id |
| `tab.write` | decode, then the tab (`not-found`); open to every connection, no authority to check; records the base64-decoded bytes |
| `session.set_agent_hooks` | open to every connection, last writer wins; `unknown-op` on a UI socket |
| `events.subscribe` | the ack from the vendored vector with this fake's revision and `session_id`, then a one-way push stream; `invalid-param` for a non-zero `tab_id_filter`; `not-implemented` on a UI socket |
| anything else | `unknown-op` |

## Lifecycle vs. the protocol

`stop()` is roost's **`session.stopping`** control (mirroring the Rust fake), not
the teardown — it tells every stream why, hangs up, and latches the fake
unavailable until `restart()`. Tearing the fake down is `shutdown()`, which stops
listening and takes the socket file with it.
"""

from __future__ import annotations

import base64
import contextlib
import copy
import json
import os
import queue
import select
import shutil
import socket
import socketserver
import tempfile
import threading
import time
from pathlib import Path

# .../shed/desktop/tools/shedtest/fake_roost.py → the repo root. The render gate
# recreates the same repo-root-relative layout inside its container (it tars
# `crates desktop/tauri desktop/tools …` into /work), so this resolves there too.
REPO_ROOT = Path(__file__).resolve().parents[3]
VECTORS = REPO_ROOT / "crates" / "fixtures" / "roost-vectors"

#: How long one pushed frame may take to reach a peer before that subscriber is
#: treated as gone. Generous next to any local write and far shorter than a
#: test's own timeouts, so a peer that has stopped reading surfaces as a CLOSED
#: stream — the answer roost's stall budget gives it — rather than as a wedged
#: handler that `stop()` and `shutdown()` can then never end.
STREAM_WRITE_DEADLINE = 2.0

#: The per-subscriber fan-out depth. Small on purpose (roost's own relay queue is
#: shallow) but far larger than any test's burst; a subscriber that overruns it
#: is CLOSED, not thinned — roost's rule, and the bare EOF is the client's
#: resync signal.
FRAME_CAPACITY = 64

#: How long the push loop parks before re-checking whether the peer went away.
#: It is NOT delivery latency: a queued frame wakes `Queue.get` immediately.
_STREAM_IDLE_TICK = 0.05

#: The granularity a blocked `send` is retried at. Small enough that a hang-up
#: beats an in-flight write about as promptly as the Rust fake's `select!` does,
#: and large enough not to spin.
_STREAM_SEND_TICK = 0.05


def _vector(name: str) -> dict:
    """One vendored roost vector. See the README beside them: never semantically
    edited, re-copied on a `roost-ipc` rev bump."""
    return json.loads((VECTORS / name).read_text())


# roost keeps ONE `session.identify` vector per generation and shed vendors only
# the current one — a protocol-2 or protocol-4 daemon is a CONTROL here
# (`set_session_protocol`), not a second vector to keep in step.
_SESSION_IDENTIFY = _vector("session.identify.response.v5.json")["result"]
_IDENTIFY = _vector("identify.response.json")["result"]
_TAB_LIST = _vector("tab.list.session.response.json")["result"]
_TAB_OPEN = _vector("tab.open.response.json")["result"]["tab"]
_ERROR = _vector("response.error.json")
_SET_AGENT_HOOKS = _vector("session.set_agent_hooks.response.json")["result"]
_EVENTS_SUBSCRIBE = _vector("events.subscribe.response.json")["result"]
_EVENTS_BATCH = _vector("events.batch.json")
_TAB_OPENED = _vector("tab.opened.event.json")
_TAB_CLOSED = _vector("shed.tab.closed.event.json")
_TAB_NOTIFICATION = _vector("shed.tab.notification.event.json")
_AGENT_REPORT_CHANGED = _vector("agent_report.changed.event.json")
_SESSION_STOPPING = _vector("session.stopping.event.json")
_TABS_REORDERED = _vector("tabs.reordered.event.json")
_PROJECTS_REORDERED = _vector("projects.reordered.event.json")
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

#: Queued after a stream's last frame to end it. A sentinel rather than a socket
#: shutdown so a goodbye that was pushed FIRST (`session.stopping`) is written
#: before the connection goes — the Python answer to the Rust fake's `biased`
#: select, and the reason a client can tell a stop from a backpressure close.
_CLOSE = object()


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


class _Conn:
    """One live connection, and what the fake knows about it."""

    __slots__ = ("id", "sock", "stream", "hangup")

    def __init__(self, conn_id: int, sock: socket.socket):
        self.id = conn_id
        self.sock = sock
        #: The frame queue, once `events.subscribe` flipped it into a stream.
        self.stream: queue.Queue | None = None
        #: Set by a hang-up. Only a blocked `send` reads it — the queued `_CLOSE`
        #: is what ends an idle stream, in order behind any goodbye — so that a
        #: peer which has stopped reading cannot hold the handler for the whole
        #: write deadline after `stop()`/`restart()`/`close_all()`.
        self.hangup = threading.Event()


class TabListHook:
    """What a [`FakeRoost.before_tab_list`] hook may do while the state lock is
    held. Deliberately tiny (the Rust twin's `TabListHook`): the seam exists to
    commit a mutation *between* a subscribe ack and the `tab.list` reply it is
    fenced against, and anything wider would be a second way to drive the fake.
    """

    def __init__(self, fake: "FakeRoost"):
        self._fake = fake

    def bump_revision(self) -> None:
        """Commit a revision with no visible change — the cheapest mutation that
        still moves the fence."""
        self._fake._commit([])

    @property
    def revision(self) -> int:
        """The revision this hook is about to let `tab.list` report."""
        return self._fake._revision

    def stream_count(self) -> int:
        """Registered streams, read from INSIDE the `tab.list` lock.

        This is how a test proves the ORDERING of a client's prologue rather than
        only its arithmetic: a client that subscribed before listing has a stream
        registered by the time this runs, and one that listed first has none.
        """
        return self._fake._count_streams()


class _Server(socketserver.ThreadingUnixStreamServer):
    """Daemon threads + no block-on-close, so teardown cannot hang: a connection
    the app is holding open must never make `shutdown()` wait for the app."""

    daemon_threads = True
    block_on_close = False


class _Handler(socketserver.StreamRequestHandler):
    def handle(self) -> None:
        fake: FakeRoost = self.server.fake  # type: ignore[attr-defined]
        conn = fake._accept(self.connection)
        if conn is None:
            # A stopped daemon accepts nothing. The dial succeeds and the
            # connection dies at once — which is what a client sees from a
            # daemon whose socket file outlived it.
            return
        try:
            while True:
                raw = self.rfile.readline()
                if not raw:
                    return
                line = raw.decode("utf-8", "replace").strip()
                if not line:
                    continue
                try:
                    request = json.loads(line)
                except json.JSONDecodeError as e:
                    self._send(_error(None, "parse-error", str(e)))
                    continue
                # `events.subscribe` is the one op whose reply is not the end of
                # the exchange: everything after the ack is a push.
                if (request.get("op") or "") == "events.subscribe":
                    try:
                        ack = fake._register_stream(conn, request.get("params") or {})
                    except _Refusal as refusal:
                        self._send(_error(request.get("id"), refusal.code, refusal.message))
                        continue
                    self._send({"id": request.get("id"), "ok": True, "result": ack})
                    self._push_frames(conn)
                    return
                self._send(fake.dispatch(request))
        except OSError:
            # Any closed-socket read/write — a hang-up (`close_all`) or the app
            # going away mid-request. Both are normal ends to a connection.
            pass
        finally:
            fake._closed(conn)

    def _send(self, reply: dict) -> None:
        # `sendall`, not the buffered `wfile`: an unbuffered `wfile` is a raw
        # SocketIO whose `write` may do a partial write.
        self.connection.sendall((json.dumps(reply) + "\n").encode())

    def _push_frames(self, conn: _Conn) -> None:
        """The push half of a subscribed connection.

        Three things end it: the queued `_CLOSE` (a hang-up, after any frames
        already queued ahead of it), the peer going away, and a **write that will
        not complete**. The last one matters even though a real daemon would
        rarely hit it: a peer that has stopped reading eventually fills its
        socket buffer, and a `sendall` with no way out would park this thread
        forever — holding the stream registered, so a test's `stop()` would never
        be honoured and the test would HANG instead of fail. `_send_frame`'s
        deadline keeps a wedged peer a closed connection, which is the same
        answer roost's stall budget gives it.
        """
        sock = self.connection
        stream = conn.stream
        assert stream is not None
        # A short timeout, NOT the write deadline: `_send_frame` re-arms it and
        # counts the ticks itself, so a hang-up can beat a blocked write instead
        # of queueing behind it. `select` keeps the recv below from blocking on
        # it either way.
        sock.settimeout(_STREAM_SEND_TICK)
        while True:
            try:
                frame = stream.get(timeout=_STREAM_IDLE_TICK)
            except queue.Empty:
                frame = None
            if frame is _CLOSE:
                return
            if frame is not None and not self._send_frame(conn, frame):
                return
            # roost keeps reading a subscribed connection so it notices a peer
            # that went away, and never dispatches or replies to what it reads.
            try:
                ready, _, _ = select.select([sock], [], [], 0)
            except OSError:
                return
            if ready:
                try:
                    data = sock.recv(1 << 16)
                except OSError:
                    return
                if not data:
                    return

    def _send_frame(self, conn: _Conn, frame: str) -> bool:
        """Write one frame, bounded by `STREAM_WRITE_DEADLINE` and interruptible
        by a hang-up. False means this subscriber is gone.

        The Python answer to the Rust fake's `select! { biased; write, hangup }`.
        A `sendall` on a peer that has stopped reading blocks until its buffer
        drains, which may be never — so this sends in `_STREAM_SEND_TICK` slices
        and, on each stall, asks the two questions that end it: has somebody hung
        up (a hang-up beats an in-flight write — the peer is not going to read it
        anyway), and has the whole deadline gone.
        """
        sock = self.connection
        view = memoryview((frame + "\n").encode())
        deadline = time.monotonic() + STREAM_WRITE_DEADLINE
        while view:
            try:
                view = view[sock.send(view):]
            except socket.timeout:
                if conn.hangup.is_set() or time.monotonic() >= deadline:
                    return False
            except OSError:
                return False
        return True


class FakeRoost:
    """A stand-in `roost-session` listening on a Unix socket.

    Use it as a context manager, or `start()` / `shutdown()` by hand::

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
        # Re-entrant: a control (a `before_tab_list` hook) commits through the
        # same `_commit`/`_push` helpers the op bodies use, and every one of them
        # expects to already hold the lock.
        self._lock = threading.RLock()
        self._server: _Server | None = None
        self._thread: threading.Thread | None = None
        self._conns: dict[int, _Conn] = {}
        self._next_conn_id = 0

        # -- the state a test pokes ---------------------------------------
        self.session_protocol: int = _SESSION_IDENTIFY["session_protocol"]
        #: Answer `unknown-op` to `session.identify` — roost's own tell for a UI
        #: socket rather than a session socket.
        self.ui_socket: bool = False
        self._session_id: str = _SESSION_IDENTIFY["session_id"]
        self._started_at: str = _SESSION_IDENTIFY["started_at"]
        self._revision: int = _TAB_LIST["revision"]
        self._projects: list[dict] = [copy.deepcopy(_PROJECT)]
        self._writes: dict[int, bytes] = {}
        self._dumps: dict[int, list[str]] = {}
        self._restarts = 0
        self._tab_list_calls = 0
        self._before_tab_list = None
        self._stopped = False
        #: Every `tab.open` this fake served, params verbatim, in order.
        self.opens: list[dict] = []
        #: Every `session.set_agent_hooks` this fake served, params verbatim, in
        #: order — the twin of `testing.rs`'s `agent_hooks_calls`.
        self.agent_hooks_calls: list[dict] = []
        #: What the next `session.set_agent_hooks` answers with, instead of the
        #: vendored vector. A whole result, so a test can seed partial errors.
        self._agent_hooks_result: dict | None = None

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

    def shutdown(self) -> None:
        """Tear the fake DOWN: hang up, stop listening, and take the socket with
        us. (roost's own `session.stopping` is `stop()`, which leaves the daemon
        there to be restarted.)

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
        self.shutdown()

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

    @property
    def tab_list_calls(self) -> int:
        """How many `tab.list` replies have been served since the fake started.

        The observer loop reads the inventory ONCE per cycle and folds everything
        after that off the stream, so this is what proves a pushed change did not
        quietly cost a re-read."""
        with self._lock:
            return self._tab_list_calls

    def set_agent_hooks_result(self, result: dict) -> None:
        """Answer the next `session.set_agent_hooks` with this result.

        The twin of `testing.rs::set_agent_hooks_result`. Seeded whole rather
        than merged, so a partial-failure result (`errors` non-empty) is written
        exactly as roost would send it."""
        with self._lock:
            self._agent_hooks_result = dict(result)

    def stream_count(self) -> int:
        """Registered event streams — one per subscribed connection.

        Unclassified at generation 5: the driver/observer split went with the
        lease, so this is the whole of what a test can assert about who is
        listening."""
        with self._lock:
            return self._count_streams()

    def add_tab(self, tab_id: int, *, cwd: str, title: str, source: str | None = None,
                session_id: str = "", lifecycle: str = "inactive", detail: str = "",
                has_notification: bool = False, shell_state: str = "at_prompt",
                metadata: dict | None = None) -> dict:
        """Add a tab and commit it as roost does: one `tab.opened` batch.

        `source` is roost's open `ownership.source` string (`opencode`, `claude`,
        …). **`None` means a plain shell tab** — no `ownership` key at all, which
        is what roost's wire carries for somebody's terminal, and what makes it
        NOT a session row.

        `metadata` is roost's open extension channel on the ownership
        (`TabAgentReportParams.metadata`) — the adapter's own key/value bag,
        carried verbatim and validated by nobody. It is how an opencode tab
        reports `server_url` (roost R10, plan 015 §3.3), which is what makes the
        row's `agent_lane` stamp appear. Ignored without a `source`, because
        there is no ownership to hang it on.
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
                                              tab.get("last_active", 0), metadata)
            else:
                tab.pop("ownership", None)
            tabs.append(tab)
            opened = copy.deepcopy(_TAB_OPENED)
            opened["data"]["tab"] = copy.deepcopy(tab)
            self._commit([opened])
            return copy.deepcopy(tab)

    def set_axes(self, tab_id: int, *, lifecycle=_UNSET, has_notification=_UNSET,
                 detail=_UNSET, ownership=_UNSET) -> None:
        """Set a tab's agent axes and commit one batch — the whole point of the
        fake for a status test.

        The batch is an `agent_report.changed`, plus a `tab.notification` when
        the sticky bit actually moved (roost's own pairing, and the reason
        `attention` is not folded out of the report envelope).

        `ownership=None` un-owns the tab (the key is REMOVED, as roost's wire has
        it), which drops it from the row set; a dict replaces it wholesale.
        `detail` edits the current ownership's detail in place.
        """
        with self._lock:
            tab = self._tab(tab_id)
            if tab is None:
                raise KeyError(f"no tab {tab_id} to set axes on")
            was_notifying = bool(tab.get("has_notification"))
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

            report = copy.deepcopy(_AGENT_REPORT_CHANGED)
            report["data"]["tab_id"] = str(tab_id)
            report["data"]["agent_lifecycle"] = tab["agent_lifecycle"]
            report["data"]["shell_state"] = tab.get("shell_state", "unknown")
            report["data"]["hook_active"] = bool(tab.get("hook_active", False))
            if "ownership" in tab:
                report["data"]["ownership"] = copy.deepcopy(tab["ownership"])
            else:
                report["data"].pop("ownership", None)
            events = [report]
            if bool(tab.get("has_notification")) != was_notifying:
                fired = copy.deepcopy(_TAB_NOTIFICATION)
                fired["data"]["tab_id"] = str(tab_id)
                fired["data"]["has_pending"] = bool(tab.get("has_notification"))
                events.append(fired)
            self._commit(events)

    def bump_revision(self) -> None:
        """Commit a revision with no visible change — an empty batch, which roost
        pushes too (that is what makes a gap mean loss and nothing else)."""
        with self._lock:
            self._commit([])

    def skip_revision(self) -> None:
        """Advance the commit counter **without pushing a batch**, so the next
        batch arrives with a hole in the sequence.

        The only way to manufacture the loss a resync exists for — roost itself
        never does this, it closes the stream instead."""
        with self._lock:
            self._revision += 1

    def reorder_tabs(self) -> None:
        """Reverse the first project's tab order and commit it as roost does: a
        `tabs.reordered` naming the whole post-reorder sequence.

        The event names the SET, not a member, which is why no client folds it
        into a row. The fake really does reorder its own tabs so a re-list
        observes the new order."""
        with self._lock:
            project = self._projects[0]
            project["tabs"].reverse()
            envelope = copy.deepcopy(_TABS_REORDERED)
            envelope["data"]["project_id"] = project["id"]
            envelope["data"]["tab_ids"] = [t["id"] for t in project["tabs"]]
            self._commit([envelope])

    def reorder_projects(self) -> None:
        """Reverse the project order and commit it as a `projects.reordered`."""
        with self._lock:
            self._projects.reverse()
            envelope = copy.deepcopy(_PROJECTS_REORDERED)
            envelope["data"]["project_ids"] = [p["id"] for p in self._projects]
            self._commit([envelope])

    def stop(self) -> None:
        """roost's `session.stopping`: every stream gets the terminal
        `session.stopping{reason: "stop"}`, every connection is hung up, and the
        fake **latches unavailable** — new dials get nothing — until
        `restart()`.

        A stopped daemon accepts nothing, and a fake that kept answering would
        make "further attempts fail" untestable. This is NOT the teardown; that
        is `shutdown()`.
        """
        with self._lock:
            envelope = copy.deepcopy(_SESSION_STOPPING)
            envelope["data"]["reason"] = "stop"
            # Queued BEFORE the hang-up, so the goodbye is written first: a
            # client that never hears `session.stopping` cannot tell a stop from
            # a backpressure close.
            self._push(envelope)
            self._stopped = True
            self._hangup_all()

    def restart(self) -> None:
        """Restart the daemon: a new `session_id`, `revision` back to 1, **tab
        ids unchanged**, every live connection hung up, and any `stop()` latch
        lifted.

        All of it is real roost behaviour — ids are persisted, the revision
        counter is in-process, and a restart is a new process, so what a client
        sees is an EOF. Together they are why a client fences per connection and
        keys rows by tab id.
        """
        with self._lock:
            self._restarts += 1
            self._session_id = f"{_SESSION_IDENTIFY['session_id']}-restart-{self._restarts}"
            self._revision = 1
            self._stopped = False
            self._hangup_all()

    def close_all(self) -> None:
        """Hang up on every connection that is live right now. Connections
        accepted afterwards are unaffected — this is a hang-up, not a shutdown,
        and not a stop."""
        with self._lock:
            self._hangup_all()

    def before_tab_list(self, hook) -> None:
        """Run `hook(TabListHook)` **once**, under the state lock, just before the
        next `tab.list` reply is built.

        The subscribe-then-list prologue's race in one seam: a mutation that
        commits here lands between the ack a client fenced on and the snapshot it
        is about to take, which is exactly the interleaving a real busy daemon
        produces and the one a naive client turns into a spurious gap."""
        with self._lock:
            self._before_tab_list = hook

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

    # -- connection bookkeeping (all under the lock) -----------------------
    def _accept(self, sock: socket.socket) -> _Conn | None:
        with self._lock:
            if self._stopped:
                return None
            self._next_conn_id += 1
            conn = _Conn(self._next_conn_id, sock)
            self._conns[conn.id] = conn
            return conn

    def _closed(self, conn: _Conn) -> None:
        with self._lock:
            self._conns.pop(conn.id, None)

    def _count_streams(self) -> int:
        return sum(1 for c in self._conns.values() if c.stream is not None)

    def _hangup(self, conn: _Conn) -> None:
        """End one connection. A stream is asked through its queue so anything
        already pushed is written first; anything else is shut down outright."""
        conn.hangup.set()
        if conn.stream is not None:
            try:
                conn.stream.put_nowait(_CLOSE)
                return
            except queue.Full:
                pass
        with contextlib.suppress(OSError):
            conn.sock.shutdown(socket.SHUT_RDWR)

    def _hangup_all(self) -> None:
        for conn in list(self._conns.values()):
            self._hangup(conn)

    # -- the event stream (all under the lock) -----------------------------
    def _push(self, frame: dict) -> None:
        """Push one already-shaped frame to every registered stream."""
        line = json.dumps(frame)
        for conn in list(self._conns.values()):
            if conn.stream is None:
                continue
            try:
                conn.stream.put_nowait(line)
            except queue.Full:
                # roost's rule: **the server closes rather than thins.** A
                # subscriber that fell behind is dropped, and the bare EOF is its
                # resync signal.
                with contextlib.suppress(OSError):
                    conn.sock.shutdown(socket.SHUT_RDWR)

    def _commit(self, events: list[dict]) -> None:
        """Commit one revision and push it as exactly one batch — including an
        empty one, which roost pushes too so a skipped number always means loss.

        The bump and the push are one operation on purpose: both happen under the
        caller's state lock, so a subscriber registered at revision `n` sees
        `n + 1` next and nothing in between."""
        self._revision += 1
        batch = copy.deepcopy(_EVENTS_BATCH)
        batch["revision"] = self._revision
        batch["events"] = events
        self._push(batch)

    def _register_stream(self, conn: _Conn, params: dict) -> dict:
        """Read the ack's revision and register the subscriber **under one lock**.

        Splitting the two is this fake's most tempting bug: a mutation that
        commits between them is delivered to nobody and skipped in the sequence,
        so the client sees a gap the daemon never had — which would flake exactly
        the resync cells this surface exists for.

        The ack carries `session_id` as well as `revision` at generation 5, and it
        is **required** on roost's side — a fake that omitted it would fail to
        decode in every consumer rather than in one test."""
        with self._lock:
            if self.ui_socket:
                raise _Refusal("not-implemented", "events.subscribe is not yet implemented")
            wanted = params.get("tab_id_filter")
            if wanted is not None and (_wire_id(wanted) or 0) != 0:
                # Refused rather than ignored: a filter the server does not apply
                # is a contract lie.
                raise _Refusal("invalid-param", 'tab_id_filter is not implemented; pass "0"')
            conn.stream = queue.Queue(maxsize=FRAME_CAPACITY)
            ack = copy.deepcopy(_EVENTS_SUBSCRIBE)
            ack["revision"] = self._revision
            # The incarnation answering, echoed so a client that identified on
            # one connection and subscribed on another can refuse a mismatched
            # pair. It tracks `restart()`, which is the only thing that moves it.
            ack["session_id"] = self._session_id
            return ack

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
                if self.ui_socket:
                    # Exactly what a roost UI socket answers, and the only way a
                    # client can tell the two sockets apart.
                    raise _Refusal("unknown-op", "no such op: session.identify")
                result = {
                    **copy.deepcopy(_SESSION_IDENTIFY),
                    "session_protocol": self.session_protocol,
                    "session_id": self._session_id,
                    "started_at": self._started_at,
                }
                return result
            if op == "identify":
                return copy.deepcopy(_IDENTIFY)
            if op == "tab.list":
                hook, self._before_tab_list = self._before_tab_list, None
                if hook is not None:
                    hook(TabListHook(self))
                self._tab_list_calls += 1
                # **`revision` only on a session socket.** A UI socket serves no
                # event stream, so it omits the key ENTIRELY — not `null` — and a
                # fence read off one would be a number with nothing to fence
                # against.
                if self.ui_socket:
                    return {"projects": copy.deepcopy(self._projects)}
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
            if op == "session.set_agent_hooks":
                return self._set_agent_hooks(params)
            # Everything else: the fake serves inventory and the one-shots, and
            # an op shed reaches for that roost does not serve here should fail
            # loudly in a test rather than pass.
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
        opened = copy.deepcopy(_TAB_OPENED)
        opened["data"]["tab"] = copy.deepcopy(tab)
        self._commit([opened])
        return {"tab": copy.deepcopy(tab)}

    def _close(self, tab_id: int) -> dict:
        self._require(tab_id)
        for project in self._projects:
            project["tabs"] = [t for t in project["tabs"] if _wire_id(t["id"]) != tab_id]
        closed = copy.deepcopy(_TAB_CLOSED)
        closed["data"]["tab_id"] = str(tab_id)
        self._commit([closed])
        return {}

    def _write(self, tab_id: int, params: dict) -> dict:
        """Decode, then look for the tab. Generation 4 ran roost's own
        `require_lease` between the two — deliberately, so a write to a missing
        tab without authority never leaked that the tab was missing. At 5 there
        is no authority to check and nothing to leak it to: every same-UID
        client may write.
        """
        data = params.get("data")
        if not isinstance(data, str):
            raise _Refusal("invalid-param", "tab.write needs base64 `data`")
        try:
            decoded = base64.b64decode(data, validate=True)
        except ValueError as e:
            raise _Refusal("invalid-param", f"tab.write data: {e}") from e
        self._require(tab_id)
        self._writes[tab_id] = self._writes.get(tab_id, b"") + decoded
        return {}

    def _set_agent_hooks(self, params: dict) -> dict:
        """`session.set_agent_hooks {mode, skip, client}` (plan 019 §3.4).

        roost's own order at generation 5: decode, barrier, handle. The
        `AgentHooksAuthority` check that sat between the first two was deleted
        with the lease, so this op WRITES FILES under the session user's home
        for whichever same-UID client asked last.
        """
        if self.ui_socket:
            raise _Refusal("unknown-op", "no such op: session.set_agent_hooks")
        if params.get("mode") not in ("auto", "off"):
            raise _Refusal("invalid-param",
                           f"session.set_agent_hooks mode: {params.get('mode')!r}")
        if not isinstance(params.get("client"), str):
            raise _Refusal("invalid-param", "session.set_agent_hooks needs a `client`")
        self.agent_hooks_calls.append(copy.deepcopy(params))
        seeded, self._agent_hooks_result = self._agent_hooks_result, None
        return copy.deepcopy(seeded if seeded is not None else _SET_AGENT_HOOKS)


def _ownership(source: str, session_id: str, detail: str, last_event_at: int,
               metadata: dict | None = None) -> dict:
    """A minimal roost `Ownership`, so a test never hand-writes the shape.

    `metadata` is roost's open extension channel — an opaque string map the
    daemon carries verbatim (it validates only `source` and the attention bits).
    Defaults to empty, which is what every adapter that stamps nothing sends.
    """
    return {
        "source": source,
        "session_id": session_id,
        "last_event_at": last_event_at,
        "detail": detail,
        "metadata": dict(metadata or {}),
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
