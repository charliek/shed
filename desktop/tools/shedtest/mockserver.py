"""A hermetic in-process fake shed-server.

A stdlib ThreadingHTTPServer on 127.0.0.1:<ephemeral> the harness points
the app at (via SHED_TAURI_MOCK_BASE_URL in test mode), so E2E runs
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
import secrets
import select
import socket
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
from urllib.parse import parse_qs, unquote, urlsplit

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
        {"name": "registry.example.test/team/devbox:v1", "docker_ref": "registry.example.test/team/devbox:v1",
         "alias": "devbox", "cached": False, "source": "config", "size_bytes": 0},
        {"name": "sha256:ff8800", "cached": True, "source": "dangling",
         "digest": "sha256:ff8800aa11bb22cc33dd44ee55ff66aa11bb22cc33dd44ee55ff66aa11bb22cc",
         "size_bytes": _GiB // 2},
    ]
}


# GET /api/egress/profiles: a config baseline + a user profile, so the egress
# pane renders a decoded shape.
_EGRESS_FIXTURE = [
    {"name": "default", "source": "config",
     "profile": {"mode": "audit", "allow": ["*.github.com"], "deny": ["evil.example.com"]}},
    {"name": "custom", "source": "user",
     "profile": {"allow": ["api.example.com"], "rule": 'request.host.endsWith(".example.test")'}},
]


DEFAULT_INFO = {
    "name": "mock",
    "version": "0.0.0-mock",
    "ssh_port": 2222,
    "http_port": 8080,
    "backend": "vz",
}

# ---------------------------------------------------------------------------
# Provisioning by delivery (plan 039 A6): the shed document's delivery record, the
# delivery transport, the provision route and the activity feed.
#
# Shapes are the server's, pinned against the provision contract
# (`tests/provision-contract/v2/`, `CONTRACT.md` §3-4) and the Go handlers
# (`internal/api/delivery_handlers.go`, `provision.go`, `activity.go`):
#   * the stream: `GET /api/delivery/requests`, `200` with `Content-Type:
#     text/event-stream`, `Cache-Control: no-store` and `X-Shed-Delivery-Stream:
#     <32 hex>`; each request one `event: request` + `data: <request>` event; a
#     `: keepalive` comment while idle; no replay: a request whose stream ended fails;
#   * the answer: `POST /api/delivery/requests/{id}` with the machine headers and the
#     stream's id; `204`, or the contract's refusals in its order (`unknown_request`
#     404, `not_stream_owner` 403, `already_answered` 409, `deadline_passed` 410,
#     `answer_refused` 400 `not_json`), each as `{"error": {"code", "message"}}`;
#   * provision: `POST /api/sheds/{name}/provision`, the create stream's SSE frames
#     (`progress`, then `complete` with the shed document, or `error`);
#   * activity: `GET /api/activity?since=&shed=&limit=` -> `{events, next}`.
# `test_mockserver_delivery.py` holds the record and stream shapes to the contract's
# own fixtures.
DELIVERY_STREAM_HEADER = "X-Shed-Delivery-Stream"
_DELIVERY_STREAM_PATH = "/api/delivery/requests"
_DELIVERY_ANSWER_RE = re.compile(r"^/api/delivery/requests/([^/]+)$")
_SHED_DOC_RE = re.compile(r"^/api/sheds/([^/]+)$")
_PROVISION_RE = re.compile(r"^/api/sheds/([^/]+)/provision$")
_ACTIVITY_PATH = "/api/activity"
#: `GET /api/egress/{name}` (`shed egress show`): a shed's EFFECTIVE egress. The literal
#: `/api/egress/stream` and `/api/egress/profiles` are matched before it, as chi does.
_EGRESS_SHOW_RE = re.compile(r"^/api/egress/([^/]+)/?$")

#: The contract's `records/accept/delivered.json` record (byte-equal JSON; pinned).
DELIVERED_RECORD = {
    "state": "delivered",
    "steps": {"stage1": {"state": "done"}, "clone": {"state": "done"},
              "stage2": {"state": "done"}, "install": {"state": "done"}},
    "last": {"at": "2026-11-23T10:00:00Z", "reason": "create", "outcome": "accepted",
             "source": {"kind": "machine", "machine_id": "5f0c6e2a9b8d4c1e7a3f2b6d8e9c0a1b",
                        "machine_name": "ada-laptop"}},
    "items": [
        {"name": "ssh-key", "provider": "ssh-key", "stage": 1, "kind": "file", "base": "home",
         "target": ".ssh/id_ed25519", "fingerprint": "k3y-fp_01",
         "written_at": "2026-11-23T10:00:02Z", "result": "ok"},
        {"name": "ssh-key.pub", "provider": "ssh-key", "stage": 1, "kind": "file",
         "base": "home", "target": ".ssh/id_ed25519.pub", "fingerprint": "k3y-fp_01",
         "written_at": "2026-11-23T10:00:02Z", "result": "ok"},
        {"name": "github", "provider": "github", "stage": 1, "kind": "file", "base": "home",
         "target": ".config/gh/hosts.yml", "fingerprint": "gh-fp-1",
         "expires_at": "2026-12-23T10:00:00Z", "written_at": "2026-11-23T10:00:02Z",
         "result": "ok"},
        {"name": "example-api-key", "provider": "api-keys", "stage": 1, "kind": "env",
         "target": "EXAMPLE_API_KEY", "fingerprint": "api-fp-1",
         "written_at": "2026-11-23T10:00:02Z", "result": "ok"},
        {"name": "project-env", "provider": "files", "stage": 2, "kind": "file",
         "base": "project", "target": ".envrc", "fingerprint": "proj-fp-1",
         "written_at": "2026-11-23T10:00:02Z", "result": "ok"},
    ],
}
#: The shapes the source routes answer for a user the account service answers for (plan
#: 040 §3.2): `GET /api/sources` with the service in the delivery slot (and its
#: `not_delivering` form), and the `409 SOURCE_IS_SERVICE` body. Read from the fixtures the
#: server's API tests and shed-core's `SourceDocument` test also read.
_SOURCE_SHAPES = Path(__file__).resolve().parents[3] / "crates" / "fixtures" / "sources"
SERVICE_SOURCE_DOCUMENT = json.loads((_SOURCE_SHAPES / "service.json").read_text())
SERVICE_NOT_DELIVERING_DOCUMENT = json.loads(
    (_SOURCE_SHAPES / "service-not-delivering.json").read_text())
SOURCE_IS_SERVICE_BODY = json.loads((_SOURCE_SHAPES / "source-is-service.json").read_text())

#: The contract's `records/accept/none.json` record: a shed from before delivery.
NONE_RECORD = {"state": "none"}

#: The activity feed served by default (newest first), shaped as `activity.go`'s
#: `ActivityEvent`.
DEFAULT_ACTIVITY = [
    {"id": "c5", "at": "2026-11-23T10:05:00Z", "kind": "shed.provision.end",
     "shed": {"name": "hello-world", "id": "q6c3g4ymb5m2dkxwe7kyz5hhzu"},
     "actor": {"kind": "user", "implicit": True}, "result": "ok"},
    {"id": "c4", "at": "2026-11-23T10:04:00Z", "kind": "delivery",
     "shed": {"name": "hello-world", "id": "q6c3g4ymb5m2dkxwe7kyz5hhzu"},
     "actor": {"kind": "system"}, "delivery": {"state": "delivered"}},
    {"id": "c3", "at": "2026-11-23T10:03:00Z", "kind": "shed.provision.start",
     "shed": {"name": "hello-world", "id": "q6c3g4ymb5m2dkxwe7kyz5hhzu"},
     "actor": {"kind": "user", "implicit": True}},
    {"id": "c2", "at": "2026-11-23T10:02:00Z", "kind": "shed.stop",
     "shed": {"name": "callbell"}, "actor": {"kind": "user", "implicit": True}, "result": "ok"},
    {"id": "c1", "at": "2026-11-23T10:01:00Z", "kind": "shed.create",
     "shed": {"name": "hello-world", "id": "q6c3g4ymb5m2dkxwe7kyz5hhzu"},
     "actor": {"kind": "user", "implicit": True}, "result": "ok"},
]


def _api_error(code: str, message: str, **extra) -> dict:
    body = {"error": {"code": code, "message": message}}
    body.update(extra)
    return body


DEFAULT_SHEDS = [
    {
        "name": "hello-world",
        "id": "q6c3g4ymb5m2dkxwe7kyz5hhzu",
        "delivery": DELIVERED_RECORD,
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
# The egress decision stream (`GET /api/egress/stream`): `501` (egress control not
# enabled) unless the mock was built with `egress_events`, when it is a `200` SSE
# stream of `data: {json}` decisions (`shed_core::models::EgressDecision`), each written
# once, the stream held open; `push_egress` adds one at runtime.
_EGRESS_PATH = "/api/egress/stream"

# The machine pin (plan 035 §3.4): the identity headers a delivery stream and its
# answers carry, and the binding a pinning server judges them against.
MACHINE_ID_HEADER = "X-Shed-Machine-Id"
MACHINE_NAME_HEADER = "X-Shed-Machine-Name"
NOT_SOURCE_ACTIVE = {
    "machine_id": "fedcba9876543210fedcba9876543210",
    "machine_name": "desk",
}

# Credential sources (plan 035 §3.4): `GET /api/sources` and `POST /api/sources/use`.
# Unarmed (the default) the mock is a server older than the feature: both routes answer
# chi's plain 404 for an unrouted path (labelled JSON by the server's middleware, no API
# error body) and every delivery stream is served, so every cell that does not arm it is
# unchanged. Armed (`arm_source`) it holds one binding: a delivery stream (or answer)
# whose machine id is not the bound one is refused 403 SOURCE_NOT_BOUND naming the bound
# machine, a matching one is served, and a move answers the document and ends every
# other machine's stream — the server's contract as the desktop is built against it.
_SOURCES_PATH = "/api/sources"
_SOURCES_USE_PATH = "/api/sources/use"
_PLAIN_404 = "404 page not found\n"
# Used with `fullmatch` only: `$` also matches before a final newline, so an id with
# one appended would pass a `match` (the real server refuses it 400).
_MACHINE_ID_RE = re.compile(r"[0-9a-f]{32}")
# The answers a scripted move (`source_use_script`) gives, by status — the codes the
# server's `handleUseSource` (`internal/api/source_handlers.go`) writes.
_SOURCE_USE_REFUSALS: dict[int, dict] = {
    403: {"error": {"code": "FORBIDDEN",
                    "message": "a script key cannot see or move a credential source"}},
    500: {"error": {"code": "INTERNAL_ERROR",
                    "message": "the move was not confirmed: the server could not save it. The "
                               "current credential source stays in use for now, but the move "
                               "may still take effect when the server restarts; check with "
                               "'shed source ls' before retrying"}},
}


def _not_source_body(machine_id: str, machine_name: str) -> dict:
    """The 403 SOURCE_NOT_BOUND body naming the bound machine (the pinned
    "not-source" shape, with this binding's machine in it)."""
    return {
        "error": {
            "code": "SOURCE_NOT_BOUND",
            "message": f'credential source for this server is "{machine_name}"; '
            "run 'shed source use <this machine>' to move it",
        },
        "source": {"machine_id": machine_id, "machine_name": machine_name},
    }


# How often an idle SSE loop re-checks the shutdown flag while waiting to push.
_SSE_POLL = 0.1

# The longest a held `GET /api/sheds` waits for its release (see
# `hold_sheds_list`) before it answers anyway — under shed-core's 8 s GET timeout,
# so a cell that forgets to release still gets an answer rather than a transport
# error. A held request that hits it is counted (`sheds_hold_expired`), so the
# cell can tell a released list from one that merely timed out.
_SHEDS_HOLD_MAX = 6.0


class MockShedServer:
    def __init__(self, require_auth: bool = False, egress_events: list[dict] | None = None):
        # When set, every /api/* route answers 401 unless the request carried an
        # Authorization header (plan 006 D6): the AUTH-SHAPED refusal a secure
        # shed-server gives a client that presented nothing. It is what lets the
        # harness observe a failed control-credential mint as a per-host failure
        # instead of a silently-successful open-mode request. Constructor-only —
        # `reset()` must not clear it (the instance IS the secure server).
        self.require_auth = require_auth
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
        # ---- the one-shot sheds-list hold (plan 026 §3.4, shed#400) ----
        # Armed → every GET /api/sheds waits on this Event until the release.
        self._sheds_hold: threading.Event | None = None
        self._sheds_waiting = 0  # requests waiting on the hold right now
        self._sheds_hold_expired = 0  # requests that gave up at _SHEDS_HOLD_MAX
        # ---- streams: shutdown and the held SSE handler threads ----
        self._shutdown = threading.Event()
        self._sse_threads: set[threading.Thread] = set()
        self._egress_hits = 0
        # `GET /api/egress/{name}`'s server config (`egress.enabled`, `egress.default`):
        # see `egress_status`. `reset()` restores these.
        self.egress_enabled = True
        self.egress_default: list[str] = []
        # Opt-in egress decisions (`egress_events`): None keeps the route's 501
        # (egress unavailable). A list makes the route a 200 SSE stream that writes
        # each decision once, as a `data: {json}` frame (`EgressDecision`), then holds
        # the stream open; a reconnect gets no repeat. `push_egress` adds more.
        self._egress_queue: queue.Queue | None = None
        if egress_events is not None:
            self._egress_queue = queue.Queue()
            for decision in egress_events:
                self._egress_queue.put(json.dumps(decision, separators=(",", ":")))
        # ---- provisioning by delivery (plan 039 A6) and the source binding; one
        # Condition guards both (the stream handler threads touch them
        # concurrently with the test thread) ----
        self._dlv_cond = threading.Condition()
        # Set while `pause()` has the server down; the port it serves (kept across one).
        self._paused = False
        self._port = 0
        # ---- credential sources (plan 035 §3.4); all under _dlv_cond ----
        self._src_armed = False
        # The account service answers (None: machines do): "" or "not_delivering".
        self._src_service: str | None = None
        self._src_bound: str | None = None  # the bound machine's id
        self._src_generation = 0
        # id -> {"name", "first_seen", "connected" (None: its live slots decide)}
        self._src_machines: dict[str, dict] = {}
        self._src_use_requests: list[dict] = []  # every POST /api/sources/use body
        self._src_use_script: list[int] = []  # statuses the next moves answer first
        self._src_use_delay = 0.0  # seconds every move waits before it is applied
        # The next GETs answer 503 SOURCE_BUSY (with this Retry-After) this many times.
        self._src_busy_left = 0
        self._src_busy_retry_after = "1"
        # The next GETs that take their document on arrival and answer it this many
        # seconds later (a read overtaken in transit), one entry per GET.
        self._src_late_reads: list[float] = []
        self._src_gets = 0  # GET /api/sources answered (busy and late ones included)
        self._src_late_answered = 0  # late GETs whose (old) answer has been written
        # ---- provisioning by delivery (plan 039 A6); all under _dlv_cond ----
        # machine id -> the live stream {"id", "end", "queue"} it holds (one per machine).
        self._dlv_live: dict[str, dict] = {}
        # every stream opened: {"id", "machine_id", "machine_name", "at"}.
        self._dlv_streams: list[dict] = []
        # every GET of the stream route, refused or not (machine id, decoded name).
        self._dlv_attempts: list[tuple[str | None, str | None]] = []
        # the monotonic time of each of those GETs (what proves a woken stream came
        # sooner than its backoff would have allowed).
        self._dlv_attempt_times: list[float] = []
        # request id -> {"stream", "machine_id", "deadline" (epoch), "request"}: sent, unanswered.
        self._dlv_pending: dict[str, dict] = {}
        # request id -> {"stream", "machine_id"}: answered (a repeat is already_answered).
        self._dlv_done: dict[str, dict] = {}
        # request ids written on a stream (what proves one was sent).
        self._dlv_written: list[str] = []
        # every answer POST: {"id", "stream", "machine_id", "machine_name", "status",
        # "code", "body" (the parsed answer, or None)}.
        self._dlv_answers: list[dict] = []
        # the next stream GETs answer these (status, body) first.
        self._dlv_refuse: list[tuple[int, dict]] = []
        # seconds between keepalive comments on an idle stream (the contract's 15).
        self.delivery_keepalive = 15.0
        # provision controls and the activity feed.
        self.provision_progress = ["stage1: delivered", "clone: done", "stage2: delivered",
                                  "install: done"]
        self.provision_warning: str | None = None
        self.provision_should_fail = False
        self.provision_record = DELIVERED_RECORD
        self.provisions: list[str] = []
        # Armed (`hold_provisions`): each provision waits, after its progress, for
        # `release_provisions` (at most 30 s), so a cell can act while one is in flight.
        self._provision_gate: threading.Event | None = None
        self.activity_events: list[dict] = [dict(e) for e in DEFAULT_ACTIVITY]
        self.activity_queries: list[dict] = []
        # The server's view of the feed: ids of events this user may not see (they
        # are scanned but never sent), and how many events one page may scan
        # (`activity.go`'s bound) — past it the page ends where the scan stopped,
        # possibly EMPTY, with a cursor. None: unbounded.
        self.activity_hidden: set[str] = set()
        self.activity_scan_budget: int | None = None

    @property
    def base_url(self) -> str:
        assert self._port, "start() first"
        return f"http://127.0.0.1:{self._port}"

    def snapshot(self) -> tuple[dict, list | None]:
        # Deep-copy the shed dicts under the lock so a GET never encodes a
        # dict that a concurrent lifecycle POST is mutating.
        with self._lock:
            sheds = None if self.sheds is None else [dict(s) for s in self.sheds]
            return dict(self.info), sheds

    def set_sheds(self, sheds: list | None) -> None:
        with self._lock:
            self.sheds = sheds

    def add_shed(self, shed: dict) -> None:
        """Append a shed to the served GET /api/sheds fixture, mimicking a shed
        created OUTSIDE the app (e.g. `shed create` from the CLI). Used by the
        dashboard auto-refresh cell to prove the open UI picks it up on its own."""
        self._add(dict(shed))

    def reset(self) -> None:
        with self._lock:
            self.info = dict(DEFAULT_INFO)
            self.sheds = [dict(s) for s in DEFAULT_SHEDS]
            self.create_should_fail = False
            self.last_create_body = None
            self.provision_progress = ["stage1: delivered", "clone: done",
                                       "stage2: delivered", "install: done"]
            self.provision_warning = None
            self.provision_should_fail = False
            self.provision_record = DELIVERED_RECORD
            self.provisions = []
            self.egress_enabled = True
            self.egress_default = []
            gate, self._provision_gate = self._provision_gate, None
        if gate is not None:
            gate.set()
        with self._lock:
            self.activity_events = [dict(e) for e in DEFAULT_ACTIVITY]
            self.activity_queries = []
            self.activity_hidden = set()
            self.activity_scan_budget = None
        self.release_sheds_list()  # never leave a handler thread parked

    # -- the one-shot sheds-list hold (plan 026 §3.4, shed#400) ------------
    def hold_sheds_list(self) -> None:
        """Arm a ONE-SHOT hold on `GET /api/sheds`: from now until
        `release_sheds_list()`, every sheds list waits for that one release (or
        `_SHEDS_HOLD_MAX`) before it answers. The release consumes the arming,
        so nothing is held after it. Arm it BEFORE the app launches and the app's
        first shed list is held, whichever request is first: the shell's own
        `list_sheds` and a background listing fan out to the same route.

        The cell it exists for (`test_tauri_downhost.py`) holds that first list
        until the UI has already published a real `hosts: 2` report — the exact
        interleaving in which the bridge's initial report used to land a zero
        seed over it."""
        with self._lock:
            if self._sheds_hold is None:
                self._sheds_hold = threading.Event()
            self._sheds_hold_expired = 0

    def release_sheds_list(self) -> None:
        """Answer every held sheds list now, and disarm the hold. Idempotent."""
        with self._lock:
            hold, self._sheds_hold = self._sheds_hold, None
        if hold is not None:
            hold.set()

    def sheds_waiting(self) -> int:
        """How many sheds lists are parked on the hold right now — what proves the
        app's first list really is still in flight."""
        with self._lock:
            return self._sheds_waiting

    def sheds_hold_expired(self) -> int:
        """How many held sheds lists answered at `_SHEDS_HOLD_MAX` instead of at
        the release (since the hold was last armed)."""
        with self._lock:
            return self._sheds_hold_expired

    def _await_sheds_release(self) -> None:
        """Called by the handler before it answers `GET /api/sheds`: park on the
        hold if one is armed. The snapshot is taken AFTER, so the held list
        answers with the state at the release."""
        with self._lock:
            hold = self._sheds_hold
            if hold is None:
                return
            self._sheds_waiting += 1
        released = hold.wait(timeout=_SHEDS_HOLD_MAX)
        with self._lock:
            self._sheds_waiting -= 1
            if not released:
                self._sheds_hold_expired += 1

    def last_create(self) -> dict | None:
        with self._lock:
            return None if self.last_create_body is None else dict(self.last_create_body)

    def hold_provisions(self) -> None:
        """From now on each provision waits, after its progress, for the release."""
        with self._lock:
            self._provision_gate = threading.Event()

    def release_provisions(self) -> None:
        with self._lock:
            gate, self._provision_gate = self._provision_gate, None
        if gate is not None:
            gate.set()

    def egress_status(self, name: str) -> dict | None:
        """`GET /api/egress/{name}` as `internal/api/handlers.go:handleEgressShow`
        resolves it: the shed's explicit `egress_profiles`, and — a shed created with the
        server default has none but does have a listener (`egress_port`) — the server's
        `egress_default` in their place; `rules` from the profile fixture. None: no such
        shed."""
        shed = self.shed(name)
        if shed is None:
            return None
        with self._lock:
            enabled, default = self.egress_enabled, list(self.egress_default)
        profiles = list(shed.get("egress_profiles") or [])
        port = int(shed.get("egress_port") or 0)
        if not profiles and port:
            profiles = default
        defs = {p["name"]: p["profile"] for p in _EGRESS_FIXTURE}
        status: dict = {"shed": name, "enabled": enabled}
        if profiles:
            status["profiles"] = profiles
            rules = {p: defs[p] for p in profiles if p in defs}
            if rules:  # omitempty
                status["rules"] = rules
        if port:
            status["port"] = port
        return status

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
    def arm_source(self, machine_id: str = NOT_SOURCE_ACTIVE["machine_id"],
                   machine_name: str = NOT_SOURCE_ACTIVE["machine_name"],
                   connected: bool = True) -> None:
        """Become a pinning server whose binding names `machine_id` (shown as
        `machine_name`, `connected` as given). Call before the app launches."""
        assert _MACHINE_ID_RE.fullmatch(machine_id), machine_id
        with self._dlv_cond:
            self._src_armed = True
            self._src_bound = machine_id
            self._src_generation = 1
            self._src_machines[machine_id] = {
                "name": machine_name, "first_seen": "2026-11-16T09:00:00Z",
                "connected": connected}

    def serve_service_source(self, state: str = "") -> None:
        """Become a directory server on which the account service answers for this user
        (plan 040 §3.2): `GET /api/sources` answers the fixture's document, with
        `"state": "not_delivering"` when `state` says so. Call before the app launches."""
        assert state in ("", "not_delivering"), state
        with self._dlv_cond:
            self._src_armed = True
            self._src_service = state

    def know_machine(self, machine_id: str, machine_name: str) -> None:
        """Record a machine as seen for this user — what a delivery stream from it
        does on the real server (a move to an unseen machine is 404)."""
        with self._dlv_cond:
            self._src_machines.setdefault(machine_id, {
                "name": machine_name, "first_seen": "2026-11-16T09:30:00Z",
                "connected": None})

    def move_source(self, machine_id: str, machine_name: str) -> None:
        """Another machine takes the source (what its `shed source use` does on the
        real server): the binding moves, and this machine's streams end."""
        assert _MACHINE_ID_RE.fullmatch(machine_id), machine_id
        with self._dlv_cond:
            assert self._src_armed, "arm_source first"
            self._src_machines.setdefault(machine_id, {
                "name": machine_name, "first_seen": "2026-11-16T09:45:00Z",
                "connected": True})
            self._src_move_locked(machine_id)

    def source_busy(self, times: int, retry_after: str = "1") -> None:
        """Answer the next `times` source reads 503 SOURCE_BUSY with `Retry-After`."""
        with self._dlv_cond:
            self._src_busy_left = times
            self._src_busy_retry_after = retry_after

    def source_reads_late(self, *delays: float) -> None:
        """The next source reads each take the document as they arrive and answer it
        `delay` seconds later — a read overtaken by newer ones in transit."""
        with self._dlv_cond:
            self._src_late_reads = list(delays)

    def source_gets(self) -> int:
        with self._dlv_cond:
            return self._src_gets

    def source_late_answered(self) -> int:
        with self._dlv_cond:
            return self._src_late_answered

    def delay_source_use(self, seconds: float) -> None:
        """Every following move waits this long (outside the lock) before it is
        applied — so two can be in flight at once."""
        with self._dlv_cond:
            self._src_use_delay = seconds

    def source_bound(self) -> str | None:
        with self._dlv_cond:
            return self._src_bound

    def source_use_requests(self) -> list[dict]:
        """Every `POST /api/sources/use` body received, refused ones included."""
        with self._dlv_cond:
            return [dict(b) for b in self._src_use_requests]

    def source_use_script(self, statuses: list[int]) -> None:
        """Answer the next moves with these statuses (pinned bodies) first."""
        for status in statuses:
            assert status in _SOURCE_USE_REFUSALS, f"no pinned body for {status}"
        with self._dlv_cond:
            self._src_use_script = list(statuses)

    def _src_document(self) -> dict:
        """The GET/POST answer (caller holds _dlv_cond)."""
        if self._src_service is not None:
            doc = SERVICE_NOT_DELIVERING_DOCUMENT if self._src_service else SERVICE_SOURCE_DOCUMENT
            return json.loads(json.dumps(doc))
        binding = None
        if self._src_bound is not None:
            ref = [{"kind": "machine", "id": self._src_bound}]
            binding = {"delivery": ref, "signing": list(ref),
                       "bound_at": "2026-11-16T10:00:00Z", "bound_by": "mock-user",
                       "generation": self._src_generation}
        machines = []
        for mid, m in self._src_machines.items():
            connected = m["connected"]
            if connected is None:
                connected = mid in self._dlv_live
            machines.append({"id": mid, "name": m["name"], "key_fingerprint": "SHA256:mock",
                             "first_seen": m["first_seen"], "connected": connected,
                             "namespaces": []})
        return {"user": "mock-user", "binding": binding, "machines": machines}

    def _src_move_locked(self, mid: str) -> None:
        """Bind `mid` (caller holds _dlv_cond). As the real server's move, and at
        once rather than when the old stream's handler gets round to closing: every
        other machine's delivery stream is unregistered and told to end, and its
        pending requests fail. So only the new source holds a stream, a request can
        never be pushed onto an ended one, the old machine's later answers are
        refused SOURCE_NOT_BOUND, and after a move away and back its answer on the
        old stream names a request that no longer waits (`unknown_request`)."""
        if mid == self._src_bound:
            return
        self._src_bound = mid
        self._src_generation += 1
        for owner, live in list(self._dlv_live.items()):
            if owner != mid:
                live["end"].set()
                del self._dlv_live[owner]
                for rid in [r for r, p in self._dlv_pending.items() if p["stream"] == live["id"]]:
                    del self._dlv_pending[rid]
        self._dlv_cond.notify_all()

    def _src_get(self) -> tuple[str, object, float] | None:
        """A read: None when unarmed (a plain 404), else `("busy", retry_after, 0)` or
        `("doc", document, delay)` — the document as it stands NOW, to be answered
        after `delay` seconds."""
        with self._dlv_cond:
            if not self._src_armed:
                return None
            self._src_gets += 1
            self._dlv_cond.notify_all()
            if self._src_busy_left > 0:
                self._src_busy_left -= 1
                return "busy", self._src_busy_retry_after, 0.0
            delay = self._src_late_reads.pop(0) if self._src_late_reads else 0.0
            return "doc", self._src_document(), delay

    def _src_late_done(self) -> None:
        with self._dlv_cond:
            self._src_late_answered += 1
            self._dlv_cond.notify_all()

    def _src_use(self, body: dict) -> tuple[int, dict] | None:
        """A move: (status, body), or None when unarmed (a plain 404)."""
        with self._dlv_cond:
            if not self._src_armed:
                return None
            self._src_use_requests.append(dict(body))
            self._dlv_cond.notify_all()
            delay = self._src_use_delay
        if delay:
            time.sleep(delay)
        with self._dlv_cond:
            if self._src_use_script:
                status = self._src_use_script.pop(0)
                return status, _SOURCE_USE_REFUSALS[status]
            mid = body.get("machine_id")
            if not isinstance(mid, str) or not _MACHINE_ID_RE.fullmatch(mid):
                return 400, {"error": {"code": "INVALID_REQUEST",
                                       "message": "machine_id must be a machine's full id, 32 "
                                                  "lowercase hex digits ('shed source ls --json' "
                                                  "lists them)"}}
            if mid not in self._src_machines:
                return 404, {"error": {"code": "SOURCE_MACHINE_NOT_FOUND",
                                       "message": "this server has not seen that machine"}}
            self._src_move_locked(mid)
            return 200, self._src_document()

    def _src_admit(self, identity: tuple[str | None, str | None]) -> tuple[int, object] | None:
        """The pin on a delivery stream (caller holds _dlv_cond): None to serve it,
        else the refusal. Unarmed: always None. The stream route has checked the
        machine headers already."""
        if not self._src_armed:
            return None
        mid, name = identity
        if mid not in self._src_machines:
            self._src_machines[mid] = {"name": name or "", "connected": None,
                                       "first_seen": "2026-11-16T09:30:00Z"}
        if mid != self._src_bound:
            bound = self._src_machines.get(self._src_bound or "", {})
            return 403, _not_source_body(self._src_bound or "", bound.get("name", ""))
        return None

    def egress_hits(self) -> int:
        with self._dlv_cond:
            return self._egress_hits

    def push_egress(self, decision: dict) -> None:
        """Write one egress decision on the egress stream (the mock must have been
        built with `egress_events`, so the route streams instead of answering 501)."""
        assert self._egress_queue is not None, "build the mock with egress_events=[]"
        self._egress_queue.put(json.dumps(decision, separators=(",", ":")))

    # -- provisioning by delivery (plan 039 A6): test-facing helpers -------
    def _dlv_wait(self, what: str, cond, timeout: float):
        """Wait (under _dlv_cond) until `cond()` is truthy; return it."""
        deadline = time.monotonic() + timeout
        with self._dlv_cond:
            while True:
                got = cond()
                if got:
                    return got
                left = deadline - time.monotonic()
                if left <= 0:
                    raise TimeoutError(f"mock: timed out waiting for {what}")
                self._dlv_cond.wait(left)

    def delivery_streams(self) -> list[dict]:
        """Every delivery stream opened, in order: `{id, machine_id, machine_name, at}`."""
        with self._dlv_cond:
            return [dict(s) for s in self._dlv_streams]

    def delivery_attempts(self) -> list[tuple[str | None, str | None]]:
        """Every GET of the stream route, refused ones too."""
        with self._dlv_cond:
            return list(self._dlv_attempts)

    def delivery_attempt_times(self) -> list[float]:
        """The monotonic time of every GET of the stream route, in order."""
        with self._dlv_cond:
            return list(self._dlv_attempt_times)

    def live_delivery_stream(self, timeout: float = 10.0, after: int = 0) -> dict:
        """The live stream once one is open and at least `after` + 1 were opened."""
        def find():
            if len(self._dlv_streams) <= after:
                return None
            for live in self._dlv_live.values():
                return {"id": live["id"], "machine_id": live["machine_id"]}
            return None
        return self._dlv_wait("a live delivery stream", find, timeout)

    def refuse_delivery_stream(self, status: int, code: str, message: str = "refused",
                               times: int = 1) -> None:
        """The next `times` stream GETs answer `status` with the API error `code`."""
        with self._dlv_cond:
            self._dlv_refuse.extend([(status, _api_error(code, message))] * times)

    def push_delivery_request(self, *, shed: str = "widgets", backend: str = "vz",
                              repo: str | None = None, reason: str = "create",
                              user: str = "", local_dir: str | None = None,
                              project_dir: str | None = None, deadline_s: int = 60,
                              timeout: float = 10.0) -> dict:
        """Send a request (CONTRACT.md §1) on the live stream; returns it. `repo` is an
        `owner/name`; its URL is `git@git.example.test:<repo>.git`."""
        req_shed = {"name": shed, "id": "q6c3g4ymb5m2dkxwe7kyz5hhzu", "backend": backend}
        if repo:
            req_shed["repo"] = {"url": f"git@git.example.test:{repo}.git", "name": repo}
            req_shed["project_dir"] = f"/home/shed/{repo.split('/')[1]}"
        if local_dir:
            req_shed["local_dir"] = local_dir
        if project_dir:
            req_shed["project_dir"] = project_dir
        deadline = time.time() + deadline_s
        req = {"version": 2, "id": secrets.token_hex(16), "reason": reason,
               "deadline": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime(deadline)),
               "user": user, "shed": req_shed}
        live = self.live_delivery_stream(timeout)
        with self._dlv_cond:
            stream = next((l for l in self._dlv_live.values() if l["id"] == live["id"]), None)
            if stream is None:
                raise RuntimeError("mock: the delivery stream ended before the push")
            self._dlv_pending[req["id"]] = {"stream": stream["id"],
                                            "machine_id": stream["machine_id"],
                                            "deadline": deadline, "request": req}
            stream["queue"].put(json.dumps(req, separators=(",", ":")))
        return req

    def wait_delivery_written(self, request_id: str, timeout: float = 10.0) -> None:
        """Until `request_id` was written on its stream."""
        self._dlv_wait(f"request {request_id} written",
                       lambda: request_id in self._dlv_written, timeout)

    def end_delivery_stream(self) -> None:
        """End every live delivery stream (the server closes it): its pending requests
        fail at once and are never sent again."""
        with self._dlv_cond:
            for live in list(self._dlv_live.values()):
                live["end"].set()
            self._dlv_cond.notify_all()

    def await_delivery_answer(self, request_id: str, timeout: float = 10.0) -> dict:
        """The answer to `request_id` the mock accepted (`204`)."""
        return self._dlv_wait(
            f"an accepted answer to {request_id}",
            lambda: next((a for a in self._dlv_answers
                          if a["id"] == request_id and a["status"] == 204), None),
            timeout)

    def delivery_answers(self) -> list[dict]:
        with self._dlv_cond:
            return [dict(a) for a in self._dlv_answers]

    def _dlv_open(self, mid: str, name: str) -> tuple[int, dict] | dict:
        """Admit a stream for machine `mid`: a scripted refusal, the duplicate refusal,
        or the new stream's record."""
        with self._dlv_cond:
            self._dlv_attempts.append((mid, name))
            self._dlv_attempt_times.append(time.monotonic())
            self._dlv_cond.notify_all()
            if self._dlv_refuse:
                return self._dlv_refuse.pop(0)
            pinned = self._src_admit((mid, name))
            if pinned is not None:
                return pinned
            if mid in self._dlv_live:
                return (409, _api_error(
                    "SOURCE_DUPLICATE_STREAM",
                    "this machine already holds its delivery stream on this server; it is retried"))
            live = {"id": secrets.token_hex(16), "machine_id": mid, "end": threading.Event(),
                    "queue": queue.Queue()}
            self._dlv_live[mid] = live
            self._dlv_streams.append({"id": live["id"], "machine_id": mid,
                                      "machine_name": name, "at": time.monotonic()})
            self._dlv_cond.notify_all()
            return live

    def _dlv_close(self, live: dict) -> None:
        """The stream ended: unregister it and fail its pending requests."""
        with self._dlv_cond:
            if self._dlv_live.get(live["machine_id"]) is live:
                del self._dlv_live[live["machine_id"]]
            for rid in [r for r, p in self._dlv_pending.items() if p["stream"] == live["id"]]:
                del self._dlv_pending[rid]
            self._dlv_cond.notify_all()

    def _dlv_mark_written(self, frame: str) -> None:
        with self._dlv_cond:
            self._dlv_written.append(json.loads(frame)["id"])
            self._dlv_cond.notify_all()

    def _dlv_answer(self, rid: str, stream: str | None, mid: str, name: str,
                    raw: bytes) -> tuple[int, dict | None]:
        """Judge one answer in the contract's order — the binding first — and record it."""
        with self._dlv_cond:
            known = self._dlv_pending.get(rid) or self._dlv_done.get(rid)
            bound = self._src_bound if self._src_armed else None
            if bound is not None and mid != bound:
                status, body = 403, _not_source_body(
                    bound, self._src_machines.get(bound, {}).get("name", ""))
            elif known is None:
                status, body = 404, _api_error("unknown_request", "no such request is waiting")
            elif known["stream"] != stream or known["machine_id"] != mid:
                status, body = 403, _api_error(
                    "not_stream_owner", "the answer did not come on the request's stream")
            elif rid in self._dlv_done:
                status, body = 409, _api_error("already_answered", "answered already")
            elif time.time() > known["deadline"]:
                del self._dlv_pending[rid]
                self._dlv_done[rid] = known
                status, body = 410, _api_error("deadline_passed", "the deadline passed")
            else:
                del self._dlv_pending[rid]
                self._dlv_done[rid] = known
                try:
                    parsed = json.loads(raw)
                    status, body = 204, None
                except ValueError:
                    parsed = None
                    status, body = 400, _api_error("answer_refused",
                                                   "the answer breaks the rule not_json",
                                                   rule="not_json")
            self._dlv_answers.append({
                "id": rid, "stream": stream, "machine_id": mid, "machine_name": name,
                "status": status, "code": (body or {}).get("error", {}).get("code"),
                "body": parsed if status == 204 else None})
            self._dlv_cond.notify_all()
            return status, body

    def _activity_page(self, since: str, shed: str, limit: int) -> dict:
        with self._lock:
            self.activity_queries.append({"since": since, "shed": shed, "limit": limit})
            events = [dict(e) for e in self.activity_events]
            hidden = set(self.activity_hidden)
            budget = self.activity_scan_budget
        if since:
            ids = [e["id"] for e in events]
            events = events[ids.index(since) + 1:] if since in ids else []
        page: list[dict] = []
        scanned = 0
        for e in events:
            if budget is not None and scanned >= budget:
                # The scan budget ran out: the page ends here, a cursor at the last
                # event scanned, whatever it holds (possibly nothing).
                return {"events": page, "next": events[scanned - 1]["id"]}
            scanned += 1
            if e["id"] in hidden or (shed and e.get("shed", {}).get("name") != shed):
                continue
            page.append(e)
            if len(page) == limit:
                more = scanned < len(events)
                return {"events": page, "next": e["id"] if more else ""}
        return {"events": page, "next": ""}

    def _record_egress(self) -> None:
        with self._dlv_cond:
            self._egress_hits += 1
            self._dlv_cond.notify_all()

    def _register_sse_thread(self) -> None:
        with self._dlv_cond:
            self._sse_threads.add(threading.current_thread())

    def _unregister_sse_thread(self) -> None:
        with self._dlv_cond:
            self._sse_threads.discard(threading.current_thread())

    def start(self) -> None:
        state = self
        assert not self._paused

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

            def _identity(self) -> tuple[str | None, str | None]:
                """The machine id and percent-DECODED name this request carried."""
                mid = self.headers.get(MACHINE_ID_HEADER)
                name = self.headers.get(MACHINE_NAME_HEADER)
                return mid, (unquote(name, errors="strict") if name is not None else None)

            def _send_raw(self, code: int, body: object, headers: dict | None = None):
                """A pinned refusal: a dict as compact JSON, a str as is (under 1024
                bytes, as the server's are)."""
                raw = (json.dumps(body, separators=(",", ":")) if isinstance(body, dict)
                       else str(body)).encode()
                assert len(raw) < 1024, len(raw)
                self.send_response(code)
                self.send_header("Content-Type", "application/json")
                self.send_header("Content-Length", str(len(raw)))
                for k, v in (headers or {}).items():
                    self.send_header(k, v)
                self.end_headers()
                self.wfile.write(raw)

            def _plain_404(self):
                """What a shed-server older than a route answers: chi's NotFound text,
                labelled JSON by the server's middleware — no API error body."""
                raw = _PLAIN_404.encode()
                self.send_response(404)
                self.send_header("Content-Type", "application/json")
                self.send_header("Content-Length", str(len(raw)))
                self.end_headers()
                self.wfile.write(raw)

            def _unauthorized(self) -> bool:
                """A secure-server 401 for an unauthenticated /api request (the
                mock's whole auth model: presented something, or did not)."""
                if not state.require_auth:
                    return False
                if not urlsplit(self.path).path.startswith("/api/"):
                    return False
                if self.headers.get("Authorization"):
                    return False
                self._send(401, {"error": "unauthorized"})
                return True

            def _body(self) -> dict:
                length = int(self.headers.get("Content-Length", 0))
                if not length:
                    return {}
                return json.loads(self.rfile.read(length) or b"{}")

            def do_GET(self):
                if self._unauthorized():
                    return
                path = urlsplit(self.path).path
                if path == _EGRESS_PATH:
                    self._serve_egress()
                    return
                if path == _SOURCES_PATH:
                    read = state._src_get()
                    if read is None:
                        self._plain_404()
                    elif read[0] == "busy":
                        self._send_raw(503, {"error": {
                            "code": "SOURCE_BUSY",
                            "message": "the credential source is being changed or saved "
                                       "right now; retry in a moment"}},
                            {"Retry-After": read[1]})
                    else:
                        _kind, doc, delay = read
                        if delay:
                            time.sleep(delay)
                        self._send(200, doc)
                        if delay:
                            state._src_late_done()
                    return
                # -- provisioning by delivery (plan 039 A6) --
                if path == _DELIVERY_STREAM_PATH:
                    self._serve_delivery_stream()
                    return
                if path == _ACTIVITY_PATH:
                    q = parse_qs(urlsplit(self.path).query)
                    one = lambda k: (q.get(k) or [""])[0]  # noqa: E731
                    try:
                        limit = int(one("limit") or 100)
                    except ValueError:
                        limit = 100
                    self._send(200, state._activity_page(one("since"), one("shed"),
                                                         max(1, min(limit, 500))))
                    return
                m = _SHED_DOC_RE.match(path)
                if m:
                    doc = state.shed(unquote(m.group(1)))
                    if doc is None:
                        self._send(404, _api_error("SHED_NOT_FOUND", "no such shed"))
                    else:
                        self._send(200, doc)
                    return
                m = _EGRESS_SHOW_RE.match(path)
                if m and m.group(1) not in ("stream", "profiles"):
                    status = state.egress_status(unquote(m.group(1)))
                    if status is None:
                        self._send(404, _api_error("SHED_NOT_FOUND", "no such shed"))
                    else:
                        self._send(200, status)
                    return
                if self.path == "/api/sheds":
                    state._await_sheds_release()
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

            # -- provisioning by delivery (plan 039 A6) ------------------
            def _delivery_machine(self) -> tuple[str, str] | None:
                """The machine headers, valid (32 lowercase hex; a name of 1 to 63
                bytes decoded, no control character), or None."""
                mid = self.headers.get(MACHINE_ID_HEADER) or ""
                raw = self.headers.get(MACHINE_NAME_HEADER)
                try:
                    name = unquote(raw, errors="strict") if raw is not None else ""
                except UnicodeDecodeError:
                    return None
                if not _MACHINE_ID_RE.fullmatch(mid):
                    return None
                if not 1 <= len(name.encode()) <= 63 or any(ord(c) < 0x20 or ord(c) == 0x7f
                                                            for c in name):
                    return None
                return mid, name

            def _machine_required(self):
                self._send_raw(403, _api_error(
                    "SOURCE_MACHINE_ID_REQUIRED",
                    "this server needs to know which machine is asking"),
                    {"Cache-Control": "no-store"})

            def _serve_delivery_stream(self):
                machine = self._delivery_machine()
                if machine is None:
                    with state._dlv_cond:
                        state._dlv_attempts.append(self._identity())
                        state._dlv_attempt_times.append(time.monotonic())
                        state._dlv_cond.notify_all()
                    self._machine_required()
                    return
                opened = state._dlv_open(*machine)
                if isinstance(opened, tuple):
                    status, body = opened
                    self._send_raw(status, body, {"Cache-Control": "no-store"})
                    return
                live = opened
                try:
                    self.send_response(200)
                    self.send_header("Content-Type", "text/event-stream")
                    self.send_header("Cache-Control", "no-store")
                    self.send_header(DELIVERY_STREAM_HEADER, live["id"])
                    self.end_headers()
                    self.wfile.flush()
                except OSError:
                    state._dlv_close(live)
                    return
                state._register_sse_thread()
                quiet_since = time.monotonic()
                try:
                    while not state._shutdown.is_set() and not live["end"].is_set():
                        if self._peer_gone():
                            break
                        try:
                            frame = live["queue"].get(timeout=_SSE_POLL)
                        except queue.Empty:
                            if time.monotonic() - quiet_since >= state.delivery_keepalive:
                                self.wfile.write(b": keepalive\n\n")
                                self.wfile.flush()
                                quiet_since = time.monotonic()
                            continue
                        if frame is None:
                            break
                        self.wfile.write(f"event: request\ndata: {frame}\n\n".encode())
                        self.wfile.flush()
                        quiet_since = time.monotonic()
                        state._dlv_mark_written(frame)
                except OSError:
                    pass
                finally:
                    state._dlv_close(live)
                    state._unregister_sse_thread()

            def _serve_delivery_answer(self, rid: str):
                length = int(self.headers.get("Content-Length", "0") or "0")
                raw = self.rfile.read(length) if length > 0 else b""
                machine = self._delivery_machine()
                if machine is None:
                    self._machine_required()
                    return
                status, body = state._dlv_answer(
                    rid, self.headers.get(DELIVERY_STREAM_HEADER), machine[0], machine[1], raw)
                if body is None:
                    self.send_response(204)
                    self.send_header("Cache-Control", "no-store")
                    self.send_header("Content-Length", "0")
                    self.end_headers()
                else:
                    self._send_raw(status, body, {"Cache-Control": "no-store"})

            def _serve_provision(self, name: str):
                doc = state.shed(name)
                with state._lock:
                    state.provisions.append(name)
                    progress = list(state.provision_progress)
                    warning = state.provision_warning
                    fail = state.provision_should_fail
                    record = state.provision_record
                if doc is None:
                    self._send(404, _api_error("SHED_NOT_FOUND", "no such shed"))
                    return
                if doc.get("status") != "running":
                    self._send(409, _api_error("SHED_NOT_RUNNING",
                                               f"shed {name} is not running"))
                    return
                self.send_response(200)
                self.send_header("Content-Type", "text/event-stream")
                self.send_header("Cache-Control", "no-cache")
                self.end_headers()

                def frame(event: str, data: dict):
                    self.wfile.write(f"event: {event}\ndata: {json.dumps(data)}\n\n".encode())
                    self.wfile.flush()

                for msg in progress:
                    frame("progress", {"phase": "provision", "message": msg})
                with state._lock:
                    gate = state._provision_gate
                if gate is not None:
                    gate.wait(30)
                if warning:
                    frame("progress", {"phase": "provision", "message": warning, "warning": True})
                if fail:
                    frame("error", _api_error("BACKEND_ERROR", f"could not provision {name}"))
                    return
                with state._lock:
                    for s in (state.sheds or []):
                        if s["name"] == name:
                            s["delivery"] = record
                            doc = dict(s)
                frame("complete", doc)

            def _serve_egress(self):
                """The egress decision stream. Record the hit, then 501 (egress
                control not enabled) so the app's egress feed rests — unless the mock
                was built with `egress_events`, when it streams them (each once) and
                holds the stream until teardown or the client goes."""
                state._record_egress()
                if state._egress_queue is None:
                    self._send(501, {"error": "egress unavailable"})
                    return
                self._stream_sse(state._egress_queue, threading.Event())

            def _stream_sse(self, q, end: threading.Event):
                """Send the 200 SSE handshake, then hold the connection and drain `q`,
                writing each frame as a `data:` line, until shutdown / a `None` sentinel
                / client disconnect / `end`."""
                try:
                    self.send_response(200)
                    self.send_header("Content-Type", "text/event-stream")
                    self.send_header("Cache-Control", "no-cache")
                    self.end_headers()
                    self.wfile.flush()
                except OSError:
                    return
                state._register_sse_thread()
                try:
                    while not state._shutdown.is_set() and not end.is_set():
                        if self._peer_gone():
                            break  # the client went away: its slot is free again
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
                    state._unregister_sse_thread()

            def _peer_gone(self) -> bool:
                """Whether the client closed its side (EOF readable), consuming nothing."""
                try:
                    readable, _, _ = select.select([self.connection], [], [], 0)
                    if not readable:
                        return False
                    return self.connection.recv(1, socket.MSG_PEEK) == b""
                except (OSError, ValueError):
                    return False

            def do_POST(self):
                if self._unauthorized():
                    return
                path = urlsplit(self.path).path
                m = _DELIVERY_ANSWER_RE.match(path)
                if m:
                    self._serve_delivery_answer(unquote(m.group(1)))
                    return
                m = _PROVISION_RE.match(path)
                if m:
                    self._serve_provision(unquote(m.group(1)))
                    return
                if path == _SOURCES_USE_PATH:
                    try:
                        body = self._body()
                    except ValueError:
                        body = {}
                    answer = state._src_use(body if isinstance(body, dict) else {})
                    if answer is None:
                        self._plain_404()
                    else:
                        self._send(*answer)
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
                if self._unauthorized():
                    return
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

        self._handler = Handler
        self._serve(0)

    def _serve(self, port: int) -> None:
        self._httpd = ThreadingHTTPServer(("127.0.0.1", port), self._handler)
        self._port = self._httpd.server_address[1]
        self._thread = threading.Thread(target=self._httpd.serve_forever, daemon=True)
        self._thread.start()

    def pause(self) -> None:
        """Take the server down — its port refuses connections and every live stream
        ends — keeping what a real server keeps on disk (sheds, the binding) and losing
        what it keeps in memory (the pending requests): what a shed-server restart looks
        like to the app. `resume()` brings it back on the same port."""
        assert self._httpd is not None and not self._paused
        self._paused = True
        self._httpd.shutdown()  # no new connection is served
        self._httpd.server_close()  # the port refuses
        self._shutdown.set()  # every held stream ends
        with self._dlv_cond:
            threads = list(self._sse_threads)
        for t in threads:
            t.join(timeout=2)
        if self._thread is not None:
            self._thread.join(timeout=2)
        self._httpd = None
        self._thread = None
        self._shutdown.clear()

    def resume(self) -> None:
        """Serve again, on the port `pause()` closed."""
        assert self._paused
        self._paused = False
        self._serve(self._port)

    def stop(self) -> None:
        # Unblock + join any live SSE handler threads first (a held stream), so no
        # socket lingers past shutdown (ResourceWarning-safe).
        self._shutdown.set()
        self.release_sheds_list()
        with self._dlv_cond:
            if self._egress_queue is not None:
                self._egress_queue.put(None)
            threads = list(self._sse_threads)
        for t in threads:
            t.join(timeout=2)
        if self._httpd is not None:
            self._httpd.shutdown()
            self._httpd.server_close()
            self._httpd = None
        if self._thread is not None:
            self._thread.join(timeout=2)
            self._thread = None
