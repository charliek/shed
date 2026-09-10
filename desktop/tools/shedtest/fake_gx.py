"""A fake gx remote-lane server for the Tauri gx-lane cells (plan 017 §3.5).

A **port of `crates/shed-gx/src/testing.rs::FakeGx`** — same routes, same error
envelope, same four SSE resume rules, same request ledger, same pin guard — in
the stdlib-only posture `fake_opencode.py` established (`http.server` +
`threading`), so the Tauri harness can drive the shipped `shed_gx` adapter
without a Rust test process.

**Two things here are the subject rather than the scenery.**

* **The credential seam.** gx wants a bearer on every route but `healthz`, and
  the token lives on the host that runs gx — so the fake serves one, and
  `write_home()` lays out the `$GROK_HOME` the app's LOCAL reader reads it from
  (a `gx-remote*.json` record naming this server's URL, and a `0600`
  `gx-remote.token`). Every request is recorded with **whether it carried an
  `Authorization` header at all**, which is what makes "healthz came before the
  first bearer request" an assertion rather than a hope.
* **The resume rules.** `plan_replay` below is gx's own four-rule ladder,
  re-derived from `gx-remote-api/src/routes/events.rs` exactly as the Rust fake
  re-derives it — **and deliberately NOT sharing the adapter's event-id parser**.
  A fake that split ids with the code under test would agree with it by
  construction and the resume cells would pass even if that parser were wrong.
  Do not "fix" the duplication.

The port is faithful rather than minimal: the cells need most of it, and a fake
that answered only the routes today's cells reach would send the next cell
hunting for why a seed hangs. What it does NOT carry over is the Rust fake's
fault injection for transport-level faults (`hangup`, `truncate_body`,
`delay_get`, `inject_on_get`) — those exist to drive `shed-gx`'s own unit tests
at a granularity the IPC door cannot observe, and the two crate suites already
own them.
"""

from __future__ import annotations

import json
import os
import threading
from collections import OrderedDict
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
from urllib.parse import unquote, urlsplit

#: The token every fake serves unless told otherwise. A **sentinel**: 64 lowercase
#: hex (which is the only shape `GxToken::parse` accepts) chosen to be greppable,
#: because several cells' real assertion is that this string appears in NO app
#: log, IPC response or panel dump.
SENTINEL_TOKEN = "5e471e15e471e15e471e15e471e15e471e15e471e15e471e15e471e15e471e15"

#: The leader instance the fake reports until a restart rotates it.
DEFAULT_INSTANCE_ID = "facade00facade00facade00facade00"

#: The gx build the fake claims to be, in `healthz` and in the record it writes.
FAKE_VERSION = "1.0.16+gx.12"

#: How long a stream handler parks between checks for new frames / a hang-up.
#: A latency bound on delivery, never a test's timing assumption.
_TICK = 0.02

#: How long a parked seed GET waits on `release_seed()` before giving up. Long
#: enough that no real test hits it; short enough that a leaked `hold_seed()`
#: (a test that forgot to release) fails fast instead of hanging a suite.
_SEED_TIMEOUT = 5.0

#: How many recent update envelopes one session's ring holds. gx's own is 2,000.
DEFAULT_RING_CAP = 2000

#: The fixture's synthetic wall-clock base, shared by every `agentTimestampMs`
#: and `createdAt` below so the epoch lives in exactly one place.
_BASE_TS_MS = 1_788_931_000_000


def _ts(n: int) -> int:
    """`agentTimestampMs` for envelope `n` — the base plus one synthetic second
    per counter."""
    return _BASE_TS_MS + n * 1_000


# ---------------------------------------------------------------------------
# envelope builders — the wire vocabulary, spelled once
# ---------------------------------------------------------------------------


def eid(session: str, n: int) -> str:
    """An event id: `<session-prefix>-<counter>`, split at the LAST hyphen."""
    return f"{session}-{n}"


def chunk(session: str, n: int, kind: str, text: str,
          prompt: str | None = None) -> dict:
    """One `session/update` chunk.

    `prompt` is `_meta.promptId`; ABSENT when `None`, which the fold reads as "no
    change" rather than as a different prompt — the distinction a `""` would
    destroy.
    """
    meta: dict = {"agentTimestampMs": _ts(n)}
    if prompt is not None:
        meta["promptId"] = prompt
    return {
        "eventId": eid(session, n),
        "method": "session/update",
        "params": {
            "sessionId": session,
            "update": {"sessionUpdate": kind, "content": {"type": "text", "text": text}},
            "_meta": meta,
        },
    }


def turn_completed(session: str, n: int, stop_reason: str = "end_turn") -> dict:
    """gx's own `turn_completed` extension — the frame that closes a streak."""
    return {
        "eventId": eid(session, n),
        "method": "_x.ai/session/update",
        "params": {
            "sessionId": session,
            "update": {"sessionUpdate": "turn_completed", "stop_reason": stop_reason},
            "_meta": {"agentTimestampMs": _ts(n)},
        },
    }


def hook(session: str, n: int) -> dict:
    """A `hook_execution` — one of the IGNORED kinds.

    15 of the 40 envelopes in the plan's live sample are these, and they
    interleave with chunk streaks, so "transparent" is not a corner case: a fold
    that let one end a streak would split every real answer into fragments.
    """
    return {
        "eventId": eid(session, n),
        "method": "_x.ai/session/update",
        "params": {
            "sessionId": session,
            "update": {"sessionUpdate": "hook_execution", "event_name": "post_tool_use"},
        },
    }


def tool_call(session: str, n: int, call_id: str, *, title: str,
              command: str, name: str = "bash") -> dict:
    return {
        "eventId": eid(session, n),
        "method": "session/update",
        "params": {
            "sessionId": session,
            "update": {
                "sessionUpdate": "tool_call",
                "toolCallId": call_id,
                "title": title,
                "rawInput": {"command": command},
                "_meta": {"x.ai/tool": {"name": name, "kind": "execute",
                                        "label": title, "read_only": False}},
            },
            "_meta": {"agentTimestampMs": _ts(n)},
        },
    }


def tool_call_update(session: str, n: int, call_id: str, *, status: str = "completed",
                     text: str = "ok") -> dict:
    return {
        "eventId": eid(session, n),
        "method": "session/update",
        "params": {
            "sessionId": session,
            "update": {
                "sessionUpdate": "tool_call_update",
                "toolCallId": call_id,
                "status": status,
                "content": [{"type": "content",
                             "content": {"type": "text", "text": text}}],
            },
            "_meta": {"agentTimestampMs": _ts(n)},
        },
    }


def permission_request(session: str, command: str) -> dict:
    """A permission whose five options are the shape a REAL gx serves — opaque
    ids, and **two of them declaring `allow_once`**.

    Copied from the same live `gx 1.0.16+gx.12` permission the crate's
    `tests/common` fixture is copied from. The two `allow_once` options are the
    load-bearing part: one proceeds once, the other turns prompting off for the
    whole session, so a three-valued `{permission: "allow-once"}` genuinely
    cannot say which the human meant — which is why the adapter refuses it and
    why the panel answers `{choice: "<id>"}` instead.
    """
    return {
        "sessionId": session,
        "toolCall": {
            "toolCallId": "call_fixture",
            "kind": "execute",
            "title": f"Execute `{command}`",
            "rawInput": {"command": command, "description": "a fixture command"},
        },
        "options": [
            {"optionId": "enable-always-approve",
             "name": "Yes, and don't ask again", "kind": "allow_once"},
            {"optionId": "allow-always-command",
             "name": f"Always allow: {command}", "kind": "allow_always"},
            {"optionId": "allow-once", "name": "Yes, proceed", "kind": "allow_once"},
            {"optionId": "reject-once", "name": "No", "kind": "reject_once"},
            {"optionId": "reject-always-command",
             "name": f"Never allow: {command}", "kind": "reject_always"},
        ],
    }


def opaque_permission_request(session: str, command: str) -> dict:
    """§3.5 cell 7's synthetic four-option permission: ids `p-1…p-4` that say
    NOTHING about their semantics, one per ACP kind.

    Deliberately unlike `permission_request` above: there the ids are gx's real
    ones (which happen to read like their kinds), here they are opaque. Both
    shapes are covered because they fail differently — the real one breaks a
    by-kind resolver (two `allow_once`), this one breaks an id-sniffing client.
    """
    return {
        "sessionId": session,
        "toolCall": {
            "toolCallId": "call_opaque",
            "kind": "execute",
            "title": f"Execute `{command}`",
            "rawInput": {"command": command},
        },
        "options": [
            {"optionId": "p-1", "name": "Yes, proceed", "kind": "allow_once"},
            {"optionId": "p-2", "name": "Yes, and remember", "kind": "allow_always"},
            {"optionId": "p-3", "name": "No", "kind": "reject_once"},
            {"optionId": "p-4", "name": "Never", "kind": "reject_always"},
        ],
    }


def question_request(session: str, questions: list[dict]) -> dict:
    """`ask_user_question`'s request. Each entry is
    `{question, options: [{label, description?}], multiSelect?}` — and the
    ANSWER is keyed by the question's TEXT, which is why the texts have to be
    distinct and why a cell asserts both keys."""
    return {"sessionId": session, "questions": questions}


def plan_request(session: str, plan: str) -> dict:
    return {"sessionId": session, "toolCallId": "call_plan", "planContent": plan}


def elicitation_request(session: str, message: str) -> dict:
    """An `mcp_elicitation` — a kind the adapter deliberately synthesizes NO
    options for, because inventing buttons for a schema it cannot read would be
    inventing an answer."""
    return {
        "sessionId": session,
        "message": message,
        "requestedSchema": {"type": "object", "properties": {"api_key": {"type": "string"}}},
    }


# ---------------------------------------------------------------------------
# the ledger
# ---------------------------------------------------------------------------


class RequestRecord:
    """One served request, as the ledger remembers it."""

    __slots__ = ("method", "path", "query", "had_bearer", "bearer_ok",
                 "last_event_id", "body")

    def __init__(self, method: str, path: str, query: str, had_bearer: bool,
                 bearer_ok: bool, last_event_id: str | None, body: str):
        self.method = method
        #: Query string STRIPPED — `requests_to("/history")` matches either way.
        self.path = path
        self.query = query
        #: Whether an `Authorization` header was present AT ALL. "healthz carried
        #: no token" is about this, not about whether the token was right.
        self.had_bearer = had_bearer
        self.bearer_ok = bearer_ok
        self.last_event_id = last_event_id
        self.body = body

    def __repr__(self) -> str:  # pragma: no cover - debugging aid
        return (f"<{self.method} {self.path}?{self.query} bearer={self.had_bearer}"
                f" resume={self.last_event_id!r}>")


def _error(status: int, code: str, message: str) -> tuple[int, str]:
    """gx's error envelope. `{error, message}`, eight codes, nothing else."""
    return status, json.dumps({"error": code, "message": message})


def _split_event_id(raw: str) -> tuple[str, int] | None:
    """`<session-prefix>-<counter>`, split at the LAST hyphen (the prefix is a
    UUID and carries four of its own).

    **Deliberately not the adapter's parser** — see the module doc.
    """
    raw = (raw or "").strip()
    prefix, sep, counter = raw.rpartition("-")
    if not sep or not prefix or not counter:
        return None
    try:
        return prefix, int(counter)
    except ValueError:
        return None


def _counter_of(env: dict) -> int | None:
    split = _split_event_id(env.get("eventId") or "")
    return split[1] if split else None


def _sse(event: str, data: str, event_id: str | None = None) -> str:
    """One SSE frame. Only `update` ever carries an `id:` — `session`,
    `approval` and `reset` are state invalidations, and giving one an `id:` would
    let a client resume from a cursor that is not an event position at all."""
    out = f"event: {event}\n"
    if event_id:
        out += f"id: {event_id}\n"
    return out + f"data: {data}\n\n"


class FakeGx:
    """One fake gx leader on its own loopback port."""

    def __init__(self, port: int = 0, *, token: str = SENTINEL_TOKEN,
                 instance_id: str = DEFAULT_INSTANCE_ID):
        self._lock = threading.RLock()
        self._stopped = threading.Event()
        #: Bumped by `close_streams()` / `stop_listening()`; a handler whose own
        #: generation is stale returns, which is an EOF to its client.
        self._generation = 0
        self._streams = 0

        self.token = token
        self.instance_id = instance_id
        self.version = FAKE_VERSION

        # -- REST state (the Rust FakeGx's, one for one) ---------------------
        self.sessions: dict[str, dict] = {}
        self.order: list[str] = []
        #: session id → its PERSISTED envelopes, oldest first (what a seed reads
        #: and what a cold resume falls back to).
        self.history: dict[str, list[dict]] = {}
        #: session id → the recent envelopes a live stream can replay from
        #: memory, oldest first and capped at `ring_cap`.
        self.ring: dict[str, list[dict]] = {}
        self.ring_cap = DEFAULT_RING_CAP
        #: session id → approval id → `{resource, status, answered_with}`.
        self.approvals: dict[str, "OrderedDict[str, dict]"] = {}

        # -- the stream ------------------------------------------------------
        #: `(wire, session)` for every frame broadcast so far. A connection
        #: delivers from its own cursor, so nothing pushed before it connected is
        #: re-sent — the replay above is what covers that window.
        self._frames: list[tuple[str, str]] = []

        # -- recording + injection -------------------------------------------
        self._requests: list[RequestRecord] = []
        #: `(suffix, status, code, message)` answered to every request whose path
        #: ends with `suffix`.
        self._failures: list[tuple[str, int, str, str]] = []
        #: The one session this fake may be addressed about. Any session-scoped
        #: request naming another is a recorded violation and a 500 — a
        #: mutation that "worked" could never be caught later.
        self.pin = ""
        self.violations: list[str] = []
        self._next_id = 0

        #: Parks the history-seed GET (`/v1/sessions/{id}/history`) until
        #: `release_seed()` is called — set (open) by default. Lets a cell make
        #: "no partial view before Ready" an observed fact rather than an
        #: assumption about scheduling.
        self._seed_gate = threading.Event()
        self._seed_gate.set()

        outer = self

        class Handler(BaseHTTPRequestHandler):
            protocol_version = "HTTP/1.1"
            timeout = 1

            def log_message(self, *_args):  # quiet
                pass

            # -- the shared front half: record, gate, dispatch ---------------
            def _auth(self) -> tuple[bool, bool]:
                header = self.headers.get("Authorization")
                with outer._lock:
                    ok = header == f"Bearer {outer.token}"
                return header is not None, ok

            def _record(self, method: str, path: str, query: str, body: str) -> tuple[bool, bool]:
                had, ok = self._auth()
                with outer._lock:
                    outer._requests.append(RequestRecord(
                        method, path, query, had, ok,
                        self.headers.get("Last-Event-ID"), body))
                return had, ok

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
                _had, ok = self._record("GET", path, split.query, "")
                if path.endswith("/events"):
                    return self._events(path, ok)
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
                        return self._json(*_error(
                            504, "seed_timeout",
                            "the seed route was parked by hold_seed and never released"))
                with outer._lock:
                    status, body = outer._route("GET", path, split.query, "", ok)
                return self._json(status, body)

            def do_POST(self):  # noqa: N802 - http.server API
                length = int(self.headers.get("Content-Length") or 0)
                body = self.rfile.read(length).decode("utf-8", "replace")
                split = urlsplit(self.path)
                path = unquote(split.path)
                _had, ok = self._record("POST", path, split.query, body)
                with outer._lock:
                    status, out = outer._route("POST", path, split.query, body, ok)
                return self._json(status, out)

            # -- the stream --------------------------------------------------
            def _events(self, path: str, bearer_ok: bool):
                """gx's `/events`, in gx's own gate order: the token, then an
                injected failure, then the pin guard, then the session — and only
                then the replay plan."""
                cursor_header = self.headers.get("Last-Event-ID")
                with outer._lock:
                    refusal = outer._global_gate(path, bearer_ok)
                    session = outer._events_session(path)
                    if refusal is None and session is None:
                        refusal = _error(404, "unknown_session", "no such route")
                    if refusal is None:
                        refusal = outer._session_gate("GET", path, session or "")
                    if refusal is not None:
                        return self._json(*refusal)
                    opening = outer._opening(session or "", cursor_header)
                    # The live cursor is taken UNDER the same lock as the replay
                    # plan, so a frame pushed between the two can be neither lost
                    # nor delivered twice.
                    cursor = len(outer._frames)
                    generation = outer._generation
                    outer._streams += 1

                try:
                    self.send_response(200)
                    self.send_header("Content-Type", "text/event-stream")
                    self.send_header("Cache-Control", "no-cache")
                    self.send_header("Connection", "close")
                    self.end_headers()
                    if opening:
                        self.wfile.write(opening.encode())
                    self.wfile.flush()
                    while not outer._stopped.is_set():
                        with outer._lock:
                            if outer._generation != generation:
                                return
                            pending = outer._frames[cursor:]
                            cursor = len(outer._frames)
                        for wire, frame_session in pending:
                            # A frame naming no session is the keep-alive: it
                            # reaches everybody, and is what a stall timer must
                            # count as liveness.
                            if frame_session and frame_session != session:
                                continue
                            self.wfile.write(wire.encode())
                            self.wfile.flush()
                        outer._stopped.wait(_TICK)
                except (BrokenPipeError, ConnectionResetError, OSError):
                    pass
                finally:
                    with outer._lock:
                        outer._streams -= 1

        self._handler = Handler
        self._server: ThreadingHTTPServer | None = None
        self._thread: threading.Thread | None = None
        self._port = port
        self._listen()

    # -- lifecycle ---------------------------------------------------------

    def _listen(self) -> None:
        server = ThreadingHTTPServer(("127.0.0.1", self._port), self._handler)
        # Pin the port on the FIRST bind, so `stop_listening()` /
        # `start_listening()` can come back on the same one — the URL is in a
        # roost tab's metadata and in a `$GROK_HOME` record by then, and a
        # leader that reappeared somewhere else would be a different leader.
        self._port = server.server_address[1]
        self._server = server
        self._thread = threading.Thread(target=server.serve_forever, daemon=True)
        self._thread.start()

    def stop_listening(self) -> None:
        """Take the port away: stop accepting, and end every live stream.

        A dead leader, which is what the escalation ladder is about. Returns once
        the listener is genuinely released, so a caller is not racing it.
        """
        with self._lock:
            self._generation += 1
            server, thread = self._server, self._thread
            self._server = self._thread = None
        if server is None:
            return
        server.shutdown()
        server.server_close()
        if thread is not None:
            thread.join(timeout=5)

    def start_listening(self) -> None:
        """Bind the same port again — the leader came back."""
        with self._lock:
            listening = self._server is not None
        if not listening:
            self._listen()

    def stop(self) -> None:
        self._stopped.set()
        self.stop_listening()

    def __enter__(self) -> "FakeGx":
        return self

    def __exit__(self, *_exc) -> None:
        self.stop()

    # -- identity ----------------------------------------------------------

    @property
    def port(self) -> int:
        return self._port

    @property
    def reported_url(self) -> str:
        """The URL a discovery record REPORTS and a roost tab forwards as
        `gx.remote`: **slash-free**, and therefore `loopback_base_url`-clean. A
        trailing slash makes it a dial URL, which is a different thing and which
        the stamp deliberately refuses."""
        return f"http://127.0.0.1:{self._port}"

    def set_instance_id(self, instance_id: str) -> None:
        """Rotate the leader instance WITHOUT touching the stores — the narrow
        "the id moved" fixture the pin exists to notice."""
        with self._lock:
            self.instance_id = instance_id

    def restart_leader(self, instance_id: str) -> None:
        """A leader RESTART as a client sees one: a new `instanceId`, the **same
        token** (it is per-`$GROK_HOME`, not per-leader), an **empty ring**, and
        the persisted transcript untouched.

        That combination is the point — a cursor that was inside the ring a
        moment ago now falls through to the disk path, which is the only way to
        exercise gx's fourth replay rule and the reason a lane can recover a
        transcript across a restart at all.
        """
        with self._lock:
            self.instance_id = instance_id
            self.ring.clear()

    def write_home(self, home) -> Path:
        """Lay out the `$GROK_HOME` the app's LOCAL credential reader reads:
        one discovery record naming this server, and a `0600` token file.

        The record's filename is SUFFIXED (`gx-remote-<hex>.json`, gx's shape for
        a leader on a non-default socket) while the token's is not — which is
        exactly why the reader takes the token's path off the record's
        `tokenFile` rather than off the record's own name.

        Re-callable: a leader that restarted rewrites its record, and this is how
        a cell makes the app rediscover a rotated `instanceId`.
        """
        home = Path(home)
        home.mkdir(parents=True, exist_ok=True)
        token_path = home / "gx-remote.token"
        with self._lock:
            token, instance = self.token, self.instance_id
        # A trailing newline, as real gx writes it — the reader has to tolerate
        # one, and a fixture that omitted it would not prove that.
        token_path.write_text(token + "\n")
        token_path.chmod(0o600)
        (home / "gx-remote-0123456789abcdef.json").write_text(json.dumps({
            "url": self.reported_url,
            "pid": os.getpid(),
            "instanceId": instance,
            "socketPath": "/tmp/p017-fake-leader.sock",
            "tokenFile": str(token_path),
            "version": FAKE_VERSION,
            "startedAt": 1_788_931_000,
        }))
        return home

    # -- REST scripting ----------------------------------------------------

    def add_session(self, session_id: str, *, title: str | None = None,
                    cwd: str = "/scratch", activity: str = "idle",
                    pending_approvals: int = 0, approximate: bool = False) -> None:
        """Add a roster row. `activity` is one of gx's six
        (`working|idle|needs_input|dormant|completed|dead`)."""
        with self._lock:
            self.sessions[session_id] = {
                "sessionId": session_id,
                # Nullable on the real wire, and the fake carries that: an
                # untitled session is `null`, not `""`.
                "title": title,
                "cwd": cwd,
                "activity": activity,
                "resident": True,
                "modelId": "fixture-model",
                "lastChangeUnixMs": 1_788_931_056_811,
                "attached": True,
                "pendingApprovals": pending_approvals,
                "approximate": approximate,
            }
            if session_id not in self.order:
                self.order.append(session_id)

    def set_activity(self, session_id: str, activity: str,
                     pending_approvals: int | None = None) -> None:
        """Move a roster row's activity. Does NOT broadcast — pair it with
        `push_session_frame` when the change should reach a live stream."""
        with self._lock:
            row = self.sessions.get(session_id)
            if row is None:
                return
            row["activity"] = activity
            if pending_approvals is not None:
                row["pendingApprovals"] = pending_approvals

    def session_row(self, session_id: str) -> dict:
        with self._lock:
            return json.loads(json.dumps(self.sessions[session_id]))

    def set_history(self, session_id: str, updates: list[dict]) -> None:
        """Replace a session's PERSISTED transcript (oldest first).

        The ring is untouched: this is "what was on disk before the client
        connected", which is what a seed reads.
        """
        with self._lock:
            self.history[session_id] = list(updates)

    def set_ring_cap(self, cap: int) -> None:
        with self._lock:
            self.ring_cap = max(1, cap)
            for ring in self.ring.values():
                self._trim(ring)

    # -- the stream --------------------------------------------------------

    def _append_stores(self, session_id: str, envelope: dict) -> None:
        """Append to the persisted transcript and the ring, trimmed. Both
        `push_update` and `stage_update` write exactly this; only whether the
        result is also broadcast differs. Caller holds `self._lock`."""
        self.history.setdefault(session_id, []).append(envelope)
        ring = self.ring.setdefault(session_id, [])
        ring.append(envelope)
        self._trim(ring)

    def push_update(self, session_id: str, envelope: dict) -> None:
        """The leader's own path for a live event: append it to the persisted
        transcript AND the ring, then broadcast it as `event: update` carrying
        the whole opaque `eventId` as the `id:` line.

        Both stores, because that is what a leader does — a fake that only
        broadcast would let a reseed silently pass on a session whose history it
        never wrote.
        """
        with self._lock:
            self._append_stores(session_id, envelope)
            wire = _sse("update", json.dumps(envelope), envelope.get("eventId"))
            self._frames.append((wire, session_id))

    def stage_update(self, session_id: str, envelope: dict) -> None:
        """Write an envelope to the persisted transcript and the ring **without
        broadcasting it** — what a leader's own stores look like after something
        happened while this client was not connected.

        Not in the Rust fake, and needed here because the two harnesses cut a
        stream differently. There, a test drives the client directly and can wait
        for the socket to be released before scripting the gap. Here the app is a
        separate process reconnecting on its own ~100 ms backoff, so "close the
        stream, then push" is a race the harness loses about as often as it wins —
        and when it loses, the frame is delivered LIVE and the cell passes for
        entirely the wrong reason.

        Staging removes the race instead of widening a window around it: the
        frame is in the stores before the stream is ever cut, so the reconnect's
        replay is the only way it can arrive.
        """
        with self._lock:
            self._append_stores(session_id, envelope)

    def push_session_frame(self, session_id: str, row: dict | None = None) -> None:
        """`event: session` — the roster row changed. A state INVALIDATION: no
        `id:` line, because a roster change is not a position in the session's
        event history."""
        with self._lock:
            body = row if row is not None else self.sessions.get(session_id, {})
            self._frames.append((_sse("session", json.dumps(body)), session_id))

    def push_session_removed(self, session_id: str) -> None:
        with self._lock:
            body = json.dumps({"sessionId": session_id, "removed": True})
            self._frames.append((_sse("session", body), session_id))

    def push_approval_frame(self, session_id: str, resource: dict) -> None:
        """`event: approval` — the resource as the GET routes serve it. Broadcast
        only; the store is scripted separately, so a cell can make the frame and
        the store disagree on purpose."""
        with self._lock:
            self._frames.append((_sse("approval", json.dumps(resource)), session_id))

    def approval_resource(self, session_id: str, approval_id: str) -> dict | None:
        with self._lock:
            entry = (self.approvals.get(session_id) or {}).get(approval_id)
            return json.loads(json.dumps(entry["resource"])) if entry else None

    def push_reset(self, session_id: str, reason: str) -> None:
        """`event: reset {reason}` — gx telling a client its view is not
        resumable. `cursor_unresolvable` and `slow_consumer` are the two a real
        leader sends."""
        with self._lock:
            body = json.dumps({"reason": reason})
            self._frames.append((_sse("reset", body), session_id))

    def push_keepalive(self) -> None:
        """The keep-alive: a comment line carrying no event at all. What a stall
        timer must count as liveness — a timer counting EVENTS would tear down
        every healthy idle stream."""
        with self._lock:
            self._frames.append((":keepalive\n\n", ""))

    def close_streams(self) -> None:
        """Hang up on every live stream — an EOF, which is what makes a watcher
        reconnect. Future connections are served normally."""
        with self._lock:
            self._generation += 1

    def stream_count(self) -> int:
        with self._lock:
            return self._streams

    # -- approvals ---------------------------------------------------------

    def add_approval(self, session_id: str, approval_id: str, kind: str,
                     method: str, request: dict) -> dict:
        """Add an approval in `pending`. Returns the resource, which is also what
        `push_approval_frame` is normally given."""
        resource = {
            "id": approval_id,
            "sessionId": session_id,
            "kind": kind,
            "method": method,
            "status": "pending",
            "request": request,
            "createdAt": _BASE_TS_MS,
            "submittedAt": None,
            "resolvedAt": None,
        }
        return self._insert_pending(session_id, approval_id, resource)

    def add_placeholder_approval(self, session_id: str, approval_id: str) -> dict:
        """The placeholder a `pending_interaction` creates: **`method` and
        `request` both null**, so there is nothing to render but the raw body.

        This is not a contrived shape. A live gx announces every approval TWICE —
        this first, then the real request in a second `approval` frame carrying
        the SAME id — so a panel that rendered the first one's (empty) option
        list and stopped would show a human a permission with no buttons.
        """
        resource = {
            "id": approval_id,
            "sessionId": session_id,
            "kind": "permission",
            "method": None,
            "status": "pending",
            "request": None,
            "createdAt": _BASE_TS_MS,
        }
        return self._insert_pending(session_id, approval_id, resource)

    def _insert_pending(self, session_id: str, approval_id: str, resource: dict) -> dict:
        with self._lock:
            self.approvals.setdefault(session_id, OrderedDict())[approval_id] = {
                "resource": resource, "status": "pending", "answered_with": None,
            }
            return json.loads(json.dumps(resource))

    def resolve_approval(self, session_id: str, approval_id: str) -> None:
        """Move an approval to `resolved` — the TUI answered first, or the agent
        moved on. A later answer is then `409 already_resolved`."""
        with self._lock:
            entry = (self.approvals.get(session_id) or {}).get(approval_id)
            if entry:
                entry["status"] = "resolved"
                entry["resource"]["status"] = "resolved"

    def answered_with(self, session_id: str, approval_id: str) -> dict | None:
        """The `response` value the fake ACCEPTED for an approval — the exact
        body a decision produced."""
        with self._lock:
            entry = (self.approvals.get(session_id) or {}).get(approval_id)
            return entry["answered_with"] if entry else None

    # -- failure injection -------------------------------------------------

    def fail(self, suffix: str, status: int, code: str, message: str) -> None:
        """Answer `status` with gx's `{error, message}` envelope to every request
        whose path ends with `suffix`.

        On a guarded route it is applied AFTER the token gate and BEFORE the pin
        guard, so a scripted `not_accepting` is not also recorded as a violation
        and a bad token still 401s first. **`/v1/healthz` is covered too**, even
        though it never reaches the token gate — it is the one route the pin
        rests on, so a leader that health-checks 503 has to be scriptable.
        """
        with self._lock:
            self._failures.append((suffix, status, code, message))

    def clear_failures(self) -> None:
        with self._lock:
            self._failures.clear()

    # -- reading back ------------------------------------------------------

    def requests(self) -> list[RequestRecord]:
        with self._lock:
            return list(self._requests)

    def requests_to(self, suffix: str) -> list[RequestRecord]:
        return [r for r in self.requests() if r.path.endswith(suffix)]

    def paths(self) -> list[str]:
        return [r.path for r in self.requests()]

    def bearer_requests(self) -> list[RequestRecord]:
        """Every request that carried an `Authorization` header AT ALL.

        The pinning rule is about this list: `healthz` must come before its first
        entry, and a client that could not pin must never add one.
        """
        return [r for r in self.requests() if r.had_bearer]

    def body_of(self, suffix: str) -> str | None:
        """The body of the FIRST recorded request whose path ends with
        `suffix`."""
        for record in self.requests():
            if record.path.endswith(suffix):
                return record.body
        return None

    def clear_requests(self) -> None:
        with self._lock:
            self._requests.clear()

    # -- the seed barrier ----------------------------------------------------

    def hold_seed(self) -> None:
        """Park the history-seed GET (`/v1/sessions/{id}/history`) until
        `release_seed()`. Makes "a client shows no partial transcript before
        its seed read completes" an assertion a cell can make, rather than a
        timing hope."""
        self._seed_gate.clear()

    def release_seed(self) -> None:
        """Let every GET parked by `hold_seed()` proceed."""
        self._seed_gate.set()

    @staticmethod
    def _is_seed_route(path: str) -> bool:
        if not path.startswith("/v1/sessions/"):
            return False
        parts = [p for p in path[len("/v1/sessions/"):].split("/") if p]
        return len(parts) == 2 and parts[1] == "history"

    # -- internals: the gates ----------------------------------------------

    def _trim(self, ring: list[dict]) -> None:
        if len(ring) > self.ring_cap:
            del ring[:len(ring) - self.ring_cap]

    def _injected(self, path: str) -> tuple[int, str] | None:
        """A scripted failure for this path, if one matches. Lock held.

        Its own method because it applies to `healthz` too, which never reaches
        the token gate — see [`FakeGx.fail`].
        """
        for suffix, status, code, message in self._failures:
            if path.endswith(suffix):
                return _error(status, code, message)
        return None

    def _global_gate(self, path: str, bearer_ok: bool) -> tuple[int, str] | None:
        """The token, then an injected failure. Called with the lock held."""
        if not bearer_ok:
            # gx deliberately says nothing about WHICH part was wrong.
            return _error(401, "unauthorized", "missing or invalid token")
        return self._injected(path)

    def _session_gate(self, method: str, path: str, session_id: str) -> tuple[int, str] | None:
        """The pin guard, then the session's existence. Called with the lock
        held."""
        if self.pin and session_id != self.pin:
            self.violations.append(
                f"{method} {path} addressed session {session_id}, not the pinned {self.pin}")
            # A violation can never look successful.
            return 500, json.dumps({"error": "pin_guard", "message": "violation"})
        if session_id not in self.sessions:
            return _error(404, "unknown_session", "no such session")
        return None

    @staticmethod
    def _events_session(path: str) -> str | None:
        rest = path[len("/v1/sessions/"):] if path.startswith("/v1/sessions/") else ""
        head, sep, tail = rest.partition("/")
        return head if sep and tail == "events" and head else None

    # -- internals: routing ------------------------------------------------

    def _route(self, method: str, path: str, query: str, body: str,
               bearer_ok: bool) -> tuple[int, str]:
        """Everything but `/events`. Called with the lock held."""
        # The one unauthenticated route, answered BEFORE the token gate so a
        # client that health-checks first never needs a credential to do it.
        #
        # A scripted failure still reaches it. This route is the pin's entire
        # foundation, and a leader whose `healthz` answers 503 is a real case the
        # escalation ladder has to survive — so `fail("/v1/healthz", …)` has to
        # mean something. It is checked HERE rather than in `_global_gate`
        # because that gate starts with the token, which this route does not have.
        if method == "GET" and path == "/v1/healthz":
            refusal = self._injected(path)
            if refusal is not None:
                return refusal
            return 200, json.dumps({
                "ok": True, "version": self.version, "leaderPid": 4242,
                "instanceId": self.instance_id, "build": "gx",
            })

        refusal = self._global_gate(path, bearer_ok)
        if refusal is not None:
            return refusal

        if method == "GET" and path == "/v1/sessions":
            rows = [self.sessions[i] for i in self.order if i in self.sessions]
            return 200, json.dumps({"sessions": rows})
        if method == "POST" and path == "/v1/sessions":
            # Global by construction — a create names no session yet, so the pin
            # guard does not apply to it.
            self._next_id += 1
            new_id = f"01a0fake-0000-7000-8000-00000000{self._next_id:04d}"
            self.sessions[new_id] = {
                "sessionId": new_id, "title": None,
                "cwd": (json.loads(body) if body else {}).get("cwd", ""),
                "activity": "working", "resident": True, "modelId": "fixture-model",
                "lastChangeUnixMs": 1_788_931_060_000, "attached": False,
                "pendingApprovals": 0, "approximate": False,
            }
            self.order.append(new_id)
            if self.pin:
                self.pin = new_id
            return 201, json.dumps({"sessionId": new_id})

        if not path.startswith("/v1/sessions/"):
            return _error(404, "unknown_session", "no such route")
        rest = path[len("/v1/sessions/"):]
        parts = [p for p in rest.split("/") if p != ""]
        session_id, tail = (parts[0] if parts else ""), parts[1:]

        refusal = self._session_gate(method, path, session_id)
        if refusal is not None:
            return refusal

        if method == "GET" and not tail:
            return 200, json.dumps(self.sessions[session_id])
        if method == "GET" and tail == ["history"]:
            return 200, self._history_page(session_id, query)
        if method == "POST" and tail == ["messages"]:
            mode = (json.loads(body) if body else {}).get("mode") or "queue"
            return 202, json.dumps({"accepted": True, "mode": mode})
        if method == "POST" and tail == ["cancel"]:
            return 202, json.dumps({"accepted": True})
        if method == "GET" and tail == ["approvals"]:
            held = self.approvals.get(session_id) or {}
            return 200, json.dumps({"approvals": [e["resource"] for e in held.values()]})
        if tail[:1] == ["approvals"] and len(tail) == 2:
            entry = (self.approvals.get(session_id) or {}).get(tail[1])
            if entry is None:
                return _error(404, "unknown_approval", "no such approval")
            if method == "GET":
                return 200, json.dumps(entry["resource"])
            if method == "POST":
                return self._answer(entry, body)
        return _error(404, "unknown_session", "no such route")

    @staticmethod
    def _answer(entry: dict, body: str) -> tuple[int, str]:
        if entry["status"] == "submitted":
            return _error(409, "already_submitted", "an answer is already on the wire")
        if entry["status"] == "resolved":
            return _error(409, "already_resolved", "the interaction closed")
        try:
            parsed = json.loads(body) if body else None
        except ValueError:
            parsed = None
        response = (parsed or {}).get("response") if isinstance(parsed, dict) else None
        if not isinstance(response, dict):
            return _error(400, "bad_request", "response is missing or not an object")
        entry["answered_with"] = response
        entry["status"] = "submitted"
        entry["resource"]["status"] = "submitted"
        return 202, json.dumps({"status": "submitted"})

    def _history_page(self, session_id: str, query: str) -> str:
        """gx's negative-`offset` paging: `offset` may count back from the end,
        and `hasMore` says whether anything precedes the returned page."""
        updates = self.history.get(session_id, [])
        n = len(updates)
        params = {}
        for pair in query.split("&"):
            key, sep, value = pair.partition("=")
            if sep:
                params[unquote(key)] = unquote(value)

        def _int(name: str, default: int) -> int:
            try:
                return int(params.get(name, default))
            except ValueError:
                return default

        offset, limit = _int("offset", 0), _int("limit", 50)
        start = max(0, n + offset) if offset < 0 else min(offset, n)
        end = min(start + max(0, limit), n)
        page = updates[start:end]
        # "the newest id IN THE RETURNED PAGE, found by reverse-scanning it" —
        # null when no line in the page carried one.
        last_event_id = next(
            (u["eventId"] for u in reversed(page) if u.get("eventId")), None)
        return json.dumps({
            "updates": page, "totalCount": n,
            "hasMore": start > 0, "lastEventId": last_event_id,
        })

    # -- internals: the resume rules ---------------------------------------

    def _opening(self, session_id: str, cursor: str | None) -> str:
        """What a fresh `/events` connection is sent before it goes live."""
        reset, replay = self._plan_replay(session_id, cursor)
        out = ""
        if reset:
            out += _sse("reset", json.dumps({"reason": reset}))
        for env in replay:
            out += _sse("update", json.dumps(env), env.get("eventId"))
        return out

    def _plan_replay(self, session_id: str,
                     cursor: str | None) -> tuple[str | None, list[dict]]:
        """gx's four resume rules, **in gx's order**, because they overlap: a
        cursor can be both newer than everything known and older than the ring's
        oldest, and only the order says which answer wins.

        1. malformed, or another session's prefix → `cursor_unresolvable`;
        2. newer than anything known → `cursor_unresolvable` (resuming would mean
           skipping events that do not exist yet);
        3. inside the ring → replay from memory;
        4. older than the ring → the persisted transcript, then the ring's tail,
           deduplicated by `eventId`.

        No cursor at all is NOT a reset — a fresh subscription starts live.
        """
        raw = (cursor or "").strip()
        if not raw:
            return None, []

        split = _split_event_id(raw)                                    # (1)
        if split is None or split[0] != session_id:
            return "cursor_unresolvable", []
        counter = split[1]

        ring = self.ring.get(session_id, [])
        history = self.history.get(session_id, [])
        ring_counters = [c for c in (_counter_of(e) for e in ring) if c is not None]

        newest = (max(ring_counters) if ring_counters else                # (2)
                  max((c for c in (_counter_of(e) for e in history) if c is not None),
                      default=None))
        if newest is None or counter > newest:
            return "cursor_unresolvable", []

        if ring_counters and counter >= min(ring_counters):              # (3)
            return None, self._after(ring, counter)

        seen: set[str] = set()                                           # (4)
        last = counter
        frames: list[dict] = []
        for env in history:
            k = _counter_of(env)
            if k is None or k <= counter:
                continue
            last = max(last, k)
            if env.get("eventId"):
                seen.add(env["eventId"])
            frames.append(env)
        frames.extend(e for e in self._after(ring, last)
                      if not e.get("eventId") or e["eventId"] not in seen)
        return None, frames

    @staticmethod
    def _after(page: list[dict], counter: int) -> list[dict]:
        return [e for e in page
                if (_counter_of(e) or -1) > counter]
