"""A fake opencode HTTP server for the Tauri agent-lane cells (plan 015 §3.4).

A **port of `tests/rc-parity/fake_opencode.py`** — same stdlib-only posture
(`http.server` + `threading`), same programmable `/event` SSE stream, same canned
REST bodies, same injectable POST statuses, and above all the same **pin guard**:
any POST to a global route, or to a session other than the pinned one, is
recorded as a violation. That guard is the invariant the whole lane rests on — a
client-initiated mutation may only ever address the session the panel is pinned
to — and the grammar it enforces (`_SCOPED_RE`) is carried over VERBATIM so the
two harnesses cannot drift into disagreeing about what "scoped" means.

What this port adds, because the lane speaks routes the RC hub never did:

* the **answer routes** — `POST /permission/{requestID}/reply` and
  `POST /question/{requestID}/reply|reject` — which are id-addressed and global
  on opencode's wire. The guard maps each request id back to the session it was
  ISSUED for and refuses one outside the pinned session's scope (the pin plus its
  descendants), which is how a child session's approval can legitimately be
  answered from the root's panel while a sibling root's cannot. The LEGACY
  session-scoped form `_SCOPED_RE` also matches
  (`POST /session/{id}/permissions/{requestID}`) carries a request id too and
  goes through exactly the same ledger check (`_issuance_violation`) — naming
  the pinned session in the path is not on its own a licence to answer a request
  the fake never issued.
* the REST routes the adapter's seed reads: `/session/{id}`,
  `/session/{id}/message`, `/session/{id}/children`, `/session/status`,
  `/permission`, `/question` — the last three `?directory=`-scoped, as opencode
  serves them. The last two list exactly what is **open**: a streamed
  `permission.asked` / `question.asked` joins the list and its resolution leaves
  it, because that is the invariant the adapter's reseed-and-retire and its
  answer lookup both rest on (`_track_open_requests`).
* stream injectors with names that say what they mean (`stream_part`,
  `stream_permission_asked`/`_replied`, `stream_question_asked`, `stream_idle`),
  `close_streams()` (what makes a watcher reconnect), a live `stream_count()`,
  and a recorded POST **body** list so a cell can assert what a verb sent rather
  than only that it was sent.

Each instance binds its own ephemeral loopback port, so a suite can run two
servers (two machines, or a restarted tab on a new port) at once.
"""

from __future__ import annotations

import json
import re
import threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from urllib.parse import parse_qs, unquote, urlsplit

#: The scoped-mutation grammar the pin guard enforces (the Go double's
#: `ocScopedMutationRe`), verbatim from `tests/rc-parity/fake_opencode.py`.
_SCOPED_RE = re.compile(r"^/session/([^/]+)/(prompt_async|abort|permissions/[^/]+)$")

#: The answer routes the LANE uses, which the hub never did: id-addressed and
#: global, so the guard resolves the id back to its session instead of reading
#: one out of the path.
_ANSWER_RE = re.compile(r"^/(permission|question)/([^/]+)/(reply|reject)$")

#: How long a stream handler parks between checks for new frames / a hang-up.
#: Only a latency bound on delivery, never a test's timing assumption — every
#: cell waits on an observable effect.
_TICK = 0.02

#: Ticks between comment pings on an idle `/event` connection.
#:
#: Two jobs, and the second is the one that is easy to miss. It is the wire's
#: keep-alive (opencode has no `server.heartbeat`, so a comment is all there is,
#: and it is what resets the adapter's 30 s stall window). It is ALSO the only
#: way a handler notices that its peer hung up: a thread parked on a `wait()`
#: with nothing to write never sees the closed socket, so `stream_count()` would
#: keep counting a connection that ended — and a cell asserting "lane.close
#: really closed the stream" would hang on a number that can no longer move.
_PING_EVERY = 5

#: How long a parked seed GET waits on `release_seed()` before giving up. Long
#: enough that no real test hits it; short enough that a leaked `hold_seed()`
#: fails fast instead of hanging a suite.
_SEED_TIMEOUT = 5.0


def _epoch_ms(offset: int = 0) -> int:
    """A fixed epoch-millis base, so a fold's timestamps are deterministic."""
    return 1_700_000_000_000 + offset


class FakeOpencode:
    """One fake opencode server on its own loopback port."""

    def __init__(self, port: int = 0):
        self._lock = threading.RLock()
        self._stopped = threading.Event()
        #: Bumped by `close_streams()`; every live handler exits when its own
        #: generation is no longer the current one.
        self._generation = 0
        self._streams = 0

        # -- REST state (the Rust `FakeOpencode`'s, one for one) -------------
        self.sessions: dict[str, dict] = {}
        self.order: list[str] = []
        self.messages: dict[str, list] = {}
        self.status: dict[str, str] = {}
        self.permissions: list[dict] = []
        self.questions: list[dict] = []
        #: request id → the session the fake issued it for (the answer guard's
        #: ledger).
        self.issued: dict[str, str] = {}

        # -- the stream -----------------------------------------------------
        #: Every SSE payload broadcast so far, with the session it belongs to.
        #: A handler replays from its own cursor, so a frame pushed before a
        #: reconnect is still delivered to the new connection — which is what
        #: makes a reconnect's reseed observable.
        self._frames: list[tuple[str, str | None]] = []

        # -- recording + injection ------------------------------------------
        self.get_paths: list[str] = []
        self.post_paths: list[str] = []
        #: `(path, body)` in order — what a verb actually SENT.
        self.posts: list[tuple[str, str]] = []
        self.violations: list[str] = []
        self.pin = ""
        #: POST status overrides keyed by route suffix.
        self.post_status: dict[str, int] = {}
        #: GET status overrides keyed by route suffix.
        self.get_status: dict[str, int] = {}

        #: Parks the messages-seed GET (`/session/{id}/message`) until
        #: `release_seed()` — set (open) by default.
        self._seed_gate = threading.Event()
        self._seed_gate.set()

        outer = self

        class Handler(BaseHTTPRequestHandler):
            protocol_version = "HTTP/1.1"
            # Bounds the keep-alive read after a handled request, so no handler
            # thread parks in readline() past stop().
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
                split = urlsplit(self.path)
                path = unquote(split.path)
                directory = (parse_qs(split.query).get("directory") or [None])[0]
                with outer._lock:
                    outer.get_paths.append(self.path)
                    override = next(
                        (c for s, c in outer.get_status.items() if path.endswith(s)),
                        None,
                    )
                if override is not None:
                    return self._json(override, '{"message":"injected failure"}')
                if path == "/event":
                    return self._event(directory)
                if outer._is_seed_route(path):
                    # Parked OUTSIDE the lock: `release_seed()` (called from the
                    # control port's own thread) needs the lock to set the
                    # event, and a wait held under it would deadlock the two.
                    #
                    # A timed-out wait must FAIL the read, not fall through and
                    # serve the seed anyway: a test that parks the seed and then
                    # forgets to release it would otherwise get an ordinary 200
                    # five seconds late and pass, which is exactly the vacuous
                    # green this barrier exists to prevent.
                    if not outer._seed_gate.wait(_SEED_TIMEOUT):
                        return self._json(504, json.dumps({
                            "message": "the seed route was parked by hold_seed"
                                       " and never released"}))
                status, body = outer._serve_get(path, directory)
                return self._json(status, body)

            def _event(self, directory: str | None):
                """The SSE feed. Close-delimited, so the handler thread ends
                with the connection."""
                self.send_response(200)
                self.send_header("Content-Type", "text/event-stream")
                self.send_header("Connection", "close")
                self.end_headers()
                with outer._lock:
                    generation = outer._generation
                    outer._streams += 1
                cursor = 0
                ticks = 0
                try:
                    self.wfile.write(
                        b'data: {"type":"server.connected","properties":{}}\n\n'
                    )
                    self.wfile.flush()
                    while not outer._stopped.is_set():
                        with outer._lock:
                            if outer._generation != generation:
                                return
                            pending = outer._frames[cursor:]
                            cursor = len(outer._frames)
                        for payload, session_id in pending:
                            if not outer._delivers(session_id, directory):
                                continue
                            self.wfile.write(b"data: " + payload.encode() + b"\n\n")
                            self.wfile.flush()
                        ticks += 1
                        if ticks % _PING_EVERY == 0:
                            self.wfile.write(b": ping\n\n")
                            self.wfile.flush()
                        outer._stopped.wait(_TICK)
                except (BrokenPipeError, ConnectionResetError, OSError):
                    pass
                finally:
                    with outer._lock:
                        outer._streams -= 1

            def do_POST(self):  # noqa: N802 - http.server API
                length = int(self.headers.get("Content-Length") or 0)
                body = self.rfile.read(length).decode("utf-8", "replace")
                path = unquote(urlsplit(self.path).path)
                status, body_out = outer._serve_mutation(path, body)
                if status == 204:
                    self.send_response(204)
                    self.send_header("Content-Length", "0")
                    self.end_headers()
                    return None
                return self._json(status, body_out)

        self._server = ThreadingHTTPServer(("127.0.0.1", port), Handler)
        self._thread = threading.Thread(target=self._server.serve_forever, daemon=True)
        self._thread.start()

    # -- identity ----------------------------------------------------------

    @property
    def port(self) -> int:
        return self._server.server_address[1]

    @property
    def base_url(self) -> str:
        """The loopback URL a roost tab would report as `server_url`."""
        return f"http://127.0.0.1:{self.port}"

    # -- REST scripting ----------------------------------------------------

    def add_session(self, session_id: str, *, title: str = "", directory: str = "/work",
                    parent: str | None = None) -> None:
        """Add a session. `parent` makes it a child — its approvals surface on
        the parent's panel, its transcript does not."""
        with self._lock:
            self.sessions[session_id] = {
                "id": session_id,
                "title": title,
                "directory": directory,
                "parentID": parent or "",
                "time": {"created": _epoch_ms(), "updated": _epoch_ms(1000)},
            }
            if session_id not in self.order:
                self.order.append(session_id)

    def remove_session(self, session_id: str) -> None:
        """Delete a session — every id-addressed route then answers 404, which
        is what turns a reseed into `Down{"unknown_session"}`."""
        with self._lock:
            self.sessions.pop(session_id, None)
            self.messages.pop(session_id, None)
            self.status.pop(session_id, None)
            self.order = [s for s in self.order if s != session_id]

    def set_status(self, session_id: str, status: str) -> None:
        """`"busy" | "retry" | "idle"`; `""` removes the entry (idle)."""
        with self._lock:
            if status:
                self.status[session_id] = status
            else:
                self.status.pop(session_id, None)

    def set_messages(self, session_id: str, messages: list) -> None:
        """The raw `GET /session/{id}/message` body — `[{info, parts}, …]`."""
        with self._lock:
            self.messages[session_id] = messages

    def set_simple_transcript(self, session_id: str, user: str, assistant: str) -> None:
        """The common seed: one user turn and one assistant turn, both complete
        (an assistant part only becomes a feed row once it is terminal)."""
        self.set_messages(session_id, [
            {
                "info": {"id": "msg_u", "role": "user",
                         "time": {"created": _epoch_ms(), "completed": _epoch_ms()}},
                "parts": [{"id": "prt_u", "messageID": "msg_u",
                           "type": "text", "text": user}],
            },
            {
                "info": {"id": "msg_a", "role": "assistant",
                         "time": {"created": _epoch_ms(500), "completed": _epoch_ms(1000)}},
                "parts": [{"id": "prt_a", "messageID": "msg_a",
                           "type": "text", "text": assistant}],
            },
        ])

    def add_permission(self, session_id: str, request_id: str, *,
                       permission: str = "bash", command: str = "ls -la") -> None:
        """Add an OPEN permission request and record that the fake issued it for
        `session_id` (the answer guard's ledger)."""
        with self._lock:
            self.permissions.append({
                "id": request_id,
                "sessionID": session_id,
                "permission": permission,
                "patterns": [command],
                "metadata": {"command": command},
            })
            self.issued[request_id] = session_id

    def add_question(self, session_id: str, request_id: str, *, header: str,
                     question: str, options: list[str]) -> None:
        """Add an OPEN, single-choice question request."""
        with self._lock:
            self.questions.append({
                "id": request_id,
                "sessionID": session_id,
                "questions": [{
                    "header": header,
                    "question": question,
                    "options": [{"label": o, "description": ""} for o in options],
                }],
            })
            self.issued[request_id] = session_id

    def remove_request(self, request_id: str) -> None:
        """Retire an open request — what a reseed sees after the TUI answered
        it. The ledger entry stays, so answering it is still in-scope (and a
        late answer is a 200, not a guard violation)."""
        with self._lock:
            self.permissions = [p for p in self.permissions if p["id"] != request_id]
            self.questions = [q for q in self.questions if q["id"] != request_id]

    # -- the stream --------------------------------------------------------

    def stream(self, payload: dict) -> None:
        """Broadcast one `/event` payload to every current AND future
        connection — the replay is what makes a reconnect's reseed observable.

        It also keeps the REST lists in step: an `*.asked` frame LISTS the
        request in `GET /permission` / `GET /question`, and a `*.replied` /
        `*.rejected` frame unlists it. See `_track_open_requests`.
        """
        session_id = ((payload.get("properties") or {}).get("sessionID")) or None
        with self._lock:
            self._track_open_requests(payload)
            self._frames.append((json.dumps(payload), session_id))

    def _track_open_requests(self, payload: dict) -> None:
        """Mirror an approval frame into the REST lists. Caller holds the lock.

        **A real opencode server lists exactly what is OPEN**: an ask announced
        on `/event` is simultaneously in `GET /permission` / `GET /question`, and
        it leaves that list when it is answered — by this client, by another, or
        in the agent's own TUI. The adapter DEPENDS on that in two places, so a
        fake that streamed an ask without listing it would be modelling a state
        the real server cannot be in:

        * every reconnect reseeds and RETIRES anything the fold holds open that
          the list no longer carries (`OpencodeFold::seed_approvals`) — an
          unlisted-but-open ask would silently vanish on the next reconnect;
        * `answer` resolves the addressed approval THROUGH those lists, inside
          the pinned session's scope, so that one panel cannot answer a sibling
          session's request (charliek/shed#345) — an unlisted ask is not
          answerable at all.

        This is the fake's REST visibility only. The `issued` ledger is
        untouched and stays the guard's own record of who a request belongs to:
        `remove_request` deliberately unlists WITHOUT forgetting the issue, so a
        late answer to a retired ask is a 200 rather than a guard violation, and
        that asymmetry is the point of keeping the two separate.
        """
        props = payload.get("properties") or {}
        kind = payload.get("type")
        if kind in ("permission.asked", "question.asked"):
            request_id = props.get("id") or ""
            target = self.permissions if kind == "permission.asked" else self.questions
            # Re-announcing an open ask is not a second ask. (opencode does not
            # do this; a cell replaying a frame might.)
            if request_id and all(r["id"] != request_id for r in target):
                target.append(dict(props))
        elif kind in ("permission.replied", "question.replied", "question.rejected"):
            # `requestID` on the resolution frames, `id` on the asks — opencode's
            # own asymmetry, not ours.
            request_id = props.get("requestID") or props.get("id") or ""
            if request_id:
                self.remove_request(request_id)

    def stream_part(self, session_id: str, *, message_id: str, part_id: str,
                    text: str, role: str = "assistant") -> None:
        """A COMPLETE transcript row: the message (so the fold learns the role)
        and then a terminal text part.

        Both halves are required. A part whose owning message's role is unknown
        stays cached and emits nothing, and an assistant part with no `time.end`
        is still streaming — so a cell that pushed only the part would wait
        forever for a row that is correctly not there yet.
        """
        self.stream({
            "type": "message.updated",
            "properties": {
                "sessionID": session_id,
                "info": {
                    "id": message_id,
                    "role": role,
                    "time": {"created": _epoch_ms(2000), "completed": _epoch_ms(2500)},
                },
            },
        })
        self.stream({
            "type": "message.part.updated",
            "properties": {
                "sessionID": session_id,
                "time": _epoch_ms(2500),
                "part": {
                    "id": part_id,
                    "messageID": message_id,
                    "type": "text",
                    "text": text,
                    "time": {"start": _epoch_ms(2000), "end": _epoch_ms(2500)},
                },
            },
        })

    def stream_permission_asked(self, session_id: str, request_id: str, *,
                                permission: str = "bash",
                                command: str = "ls -la") -> None:
        """A live `permission.asked`.

        Two side effects, and both are what a real server does. The issue is
        recorded in the guard's ledger, so answering it is IN SCOPE; and the
        request joins `GET /permission` until it is replied to, because opencode
        lists what is open (`_track_open_requests`) — which is what makes it
        answerable at all, since `answer` resolves the addressed approval
        through that list.
        """
        with self._lock:
            self.issued[request_id] = session_id
        self.stream({
            "type": "permission.asked",
            "properties": {
                "id": request_id,
                "sessionID": session_id,
                "permission": permission,
                "patterns": [command],
                "metadata": {"command": command},
            },
        })

    def stream_permission_replied(self, session_id: str, request_id: str,
                                  reply: str = "once") -> None:
        """The resolution of an ask — by this client, by another, or by the
        agent's own TUI. The fold cannot tell, which is the point.

        It also unlists the request from `GET /permission`, because a server
        that has replied no longer lists it. A raw
        `stream({"type": "question.replied", …})` gets the same treatment —
        the bookkeeping is in `stream`, not in this helper.
        """
        self.stream({
            "type": "permission.replied",
            "properties": {
                "sessionID": session_id,
                "requestID": request_id,
                "reply": reply,
            },
        })

    def stream_question_asked(self, session_id: str, request_id: str, *, header: str,
                              question: str, options: list[str],
                              custom: bool | None = None, multiple: bool = False) -> None:
        """A live `question.asked` with its options. Same two side effects as
        `stream_permission_asked`: the ledger entry, and `GET /question` listing
        it while it is open.

        **`custom=None` OMITS the key**, which is opencode's ordinary wire shape
        — its own schema documents the field as "Allow typing a custom answer
        (default: true)" and its TUI draws the freeform row when the ask says
        nothing. So an omitted flag means free text IS accepted, and a cell that
        wants the other answer has to say `custom=False` out loud.
        """
        with self._lock:
            self.issued[request_id] = session_id
        q = {
            "header": header,
            "question": question,
            "options": [{"label": o, "description": ""} for o in options],
            "multiple": multiple,
        }
        if custom is not None:
            q["custom"] = custom
        self.stream({
            "type": "question.asked",
            "properties": {
                "id": request_id,
                "sessionID": session_id,
                "questions": [q],
            },
        })

    def stream_idle(self, session_id: str) -> None:
        """`session.idle` — the boundary the activity verdict turns on."""
        self.stream({"type": "session.idle", "properties": {"sessionID": session_id}})

    def stream_session_created(self, session_id: str, directory: str,
                               parent: str | None = None) -> None:
        """A `session.created`. With a `parent` in the watcher's scope it GROWS
        that scope, which is how a child spawned mid-stream gets its approvals
        surfaced on the root's panel."""
        self.add_session(session_id, directory=directory, parent=parent)
        self.stream({
            "type": "session.created",
            "properties": {
                "info": {
                    "id": session_id,
                    "directory": directory,
                    "parentID": parent or "",
                },
            },
        })

    def close_streams(self) -> None:
        """Hang up on every live `/event` connection — what makes a watcher
        reconnect. Future connections are served normally."""
        with self._lock:
            self._generation += 1

    def stream_count(self) -> int:
        """How many `/event` connections are live right now."""
        with self._lock:
            return self._streams

    # -- failure injection -------------------------------------------------

    def fail_post(self, suffix: str, status: int) -> None:
        with self._lock:
            self.post_status[suffix] = status

    def fail_get(self, suffix: str, status: int) -> None:
        with self._lock:
            self.get_status[suffix] = status

    # -- the seed barrier ----------------------------------------------------

    def hold_seed(self) -> None:
        """Park the messages-seed GET (`/session/{id}/message`) until
        `release_seed()`. Makes "no partial transcript before the seed read
        completes" an assertion a cell can make, rather than a timing hope."""
        self._seed_gate.clear()

    def release_seed(self) -> None:
        """Let every GET parked by `hold_seed()` proceed."""
        self._seed_gate.set()

    @staticmethod
    def _is_seed_route(path: str) -> bool:
        if not path.startswith("/session/"):
            return False
        _, _, tail = path[len("/session/"):].partition("/")
        return tail == "message"

    # -- reading back ------------------------------------------------------

    def post_body(self, suffix: str) -> str | None:
        """The body of the FIRST recorded POST whose path ends with `suffix`."""
        with self._lock:
            for path, body in self.posts:
                if path.endswith(suffix):
                    return body
        return None

    def snapshot(self) -> dict:
        with self._lock:
            return {
                "get_paths": list(self.get_paths),
                "post_paths": list(self.post_paths),
                "violations": list(self.violations),
            }

    def stop(self) -> None:
        self._stopped.set()
        self._server.shutdown()
        self._server.server_close()
        self._thread.join(timeout=5)

    def __enter__(self) -> "FakeOpencode":
        return self

    def __exit__(self, *_exc) -> None:
        self.stop()

    # -- internals ---------------------------------------------------------

    def _directory_of(self, session_id: str) -> str | None:
        session = self.sessions.get(session_id)
        return session["directory"] if session else None

    def _delivers(self, session_id: str | None, directory: str | None) -> bool:
        """opencode's `/event` is instance-scoped: a connection that named a
        directory sees that directory's sessions.

        A frame that names NO session (`server.connected`, a ping) reaches
        everybody — that is also how the wire's liveness arrives. A frame for a
        session the fake does not know is delivered too, so a `session.created`
        for a brand-new child is not filtered out before it can be learned.
        """
        if not session_id or directory is None:
            return True
        with self._lock:
            known = self._directory_of(session_id)
        return known is None or known == directory

    def _serve_get(self, path: str, directory: str | None) -> tuple[int, str]:
        with self._lock:
            if path == "/session":
                return 200, json.dumps([self.sessions[i] for i in self.order
                                        if i in self.sessions])
            if path == "/session/status":
                return 200, json.dumps({
                    sid: {"type": status}
                    for sid, status in self.status.items()
                    if directory is None or self._directory_of(sid) == directory
                })
            if path in ("/permission", "/question"):
                source = self.permissions if path == "/permission" else self.questions
                return 200, json.dumps([
                    r for r in source
                    if directory is None
                    or self._directory_of(r.get("sessionID", "")) == directory
                ])
            if path.startswith("/session/"):
                rest = path[len("/session/"):]
                session_id, _, tail = rest.partition("/")
                if session_id not in self.sessions:
                    return 404, json.dumps(
                        {"name": "NotFoundError",
                         "data": {"message": "no such session"}})
                if tail == "":
                    return 200, json.dumps(self.sessions[session_id])
                if tail == "message":
                    return 200, json.dumps(self.messages.get(session_id, []))
                if tail == "children":
                    return 200, json.dumps([
                        self.sessions[i] for i in self.order
                        if i in self.sessions
                        and self.sessions[i].get("parentID") == session_id
                    ])
                return 404, "{}"
        return 404, "{}"

    def _pin_scope(self) -> set[str]:
        """The pinned session and its descendants, transitively — the ids whose
        approvals the panel may legitimately answer."""
        if not self.pin:
            return set()
        scope = {self.pin}
        while True:
            grown = {
                sid for sid, s in self.sessions.items()
                if s.get("parentID") in scope
            }
            if grown <= scope:
                return scope
            scope |= grown

    def _issuance_violation(self, request_id: str) -> str | None:
        """The ANSWER guard, by request id: an answer may only ever name a
        request the fake actually issued, for a session inside the pin's scope.

        Shared by both answer shapes, which is the point. The lane's global
        `/permission/{id}/reply` always went through it; the legacy
        session-scoped `/session/{id}/permissions/{id}` was checked on its
        SESSION alone, so with the pin set, answering a request the fake had
        never issued returned `200 true` and recorded no violation as long as
        the path named the pinned session. Called with the lock held.
        """
        owner = self.issued.get(request_id)
        if owner is None:
            return f"answered {request_id}, which the fake never issued"
        if self.pin and owner not in self._pin_scope():
            return (f"answered {request_id}, issued for {owner}, "
                    f"outside the pinned {self.pin}'s scope")
        return None

    def _serve_mutation(self, path: str, body: str) -> tuple[int, str]:
        with self._lock:
            self.post_paths.append(path)
            self.posts.append((path, body))

            violation: str | None = None
            unknown_session = False
            scoped = _SCOPED_RE.match(path)
            answer = _ANSWER_RE.match(path)
            if scoped:
                # The hub's grammar, unchanged: a session-scoped mutation may
                # only ever address the pinned session.
                if self.pin and scoped.group(1) != self.pin:
                    violation = (f"addressed session {scoped.group(1)}, "
                                 f"not the pinned {self.pin}")
                else:
                    unknown_session = scoped.group(1) not in self.sessions
                    verb = scoped.group(2)
                    # `permissions/{id}` is the LEGACY answer form: it carries a
                    # request id, so it gets the same ledger check the global
                    # answer routes below get. Naming the pinned session is not
                    # on its own a licence to answer anything.
                    if not unknown_session and verb.startswith("permissions/"):
                        violation = self._issuance_violation(
                            verb.split("/", 1)[1])
            elif answer:
                # The lane's answer routes are id-addressed and GLOBAL, so the
                # guard resolves the request id back to the session it was
                # issued for. A descendant's is in scope (its approval blocks
                # the same agent); a sibling root's is not.
                violation = self._issuance_violation(answer.group(2))
            else:
                violation = f"not a session-scoped mutation route: POST {path}"

            if violation:
                self.violations.append(violation)
                # A violation can never look successful.
                return 500, json.dumps({"message": "pin guard violation"})
            if unknown_session:
                return 404, json.dumps(
                    {"name": "NotFoundError", "data": {"message": "no such session"}})

            override = next(
                (c for s, c in self.post_status.items() if path.endswith(s)), None)
            if override is not None:
                return override, json.dumps(
                    {"_tag": "InvalidRequestError", "message": "injected failure"})

        if path.endswith("/prompt_async"):
            return 204, ""
        return 200, "true"
