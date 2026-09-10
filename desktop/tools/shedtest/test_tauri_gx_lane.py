"""The **gx** agent lane in the Tauri app — kind dispatch, the credential seam,
and the capability-gated panel over them (plan 017 §3.5, C4 + C5). `--target
tauri`.

**Hermetic, and every layer under test is the shipped one.** Three fakes and no
network beyond loopback:

* `fake_roost.FakeRoost` on a Unix socket, serving **two agent tabs on one
  machine** — a `grok` tab whose metadata carries `gx.remote` and an `opencode`
  tab whose metadata carries `server_url` — plus a second `grok` tab with
  neither. Two adapters live in one app is the whole point: it is the only shape
  in which "the panel offers what the ADAPTER can do" can fail visibly, and the
  only one where a kind mixed up between two lanes would show.
* `fake_gx.FakeGx` on a loopback port, answering gx's `/v1` API with its request
  ledger and pin guard armed, and writing the `$GROK_HOME` the app's LOCAL
  credential reader reads (`SHED_TAURI_GX_HOME`).
* `fake_opencode.FakeOpencode`, the neighbour — here only so the cross-adapter
  claims (no interject toggle, three permission buttons) are made against a real
  second lane rather than against a memory of one.

Everything between them is production code: the real `RoostWatcher`, shed-core's
own `agent_lane()` stamp, the real `shed_gx` client/fold/watcher, the real
`lane.rs` dispatch, the real panel.

**Every cell opens its own lane** (`_ready`, which is `lane.open` plus a wait for
the seed). What order still matters for is the FAKES and the LANE's live state:
the app, both servers and the lane are module-scoped, so the cells that disturb
the stream (a silent resume, a server reset, a leader rotation, a dead leader)
come last, in that order, and the approval cells clean up after themselves so a
held-pending approval does not leave the next cell's session in
`needs_approval`. Read them top to bottom.

**What this file cannot assert, and where it is asserted instead.** The panel's
buttons are not clickable from here — the harness drives IPC, not a mouse — so a
button's EFFECT is asserted through `lane.answer`/`lane.send` (the same ops the
button calls) and the button's PRESENCE through `lane.dump`, which reports what
was rendered. The one claim that leaves a seam is "an action's refusal renders
inline": `not_accepting` is asserted as an IPC refusal (cell 13), and the inline
banner it would land in is asserted with a refusal the panel raises by itself
(cell 3's failed open).
"""

from __future__ import annotations

import json
import os
import platform
import shutil
import subprocess
import tempfile
import time
from pathlib import Path

import pytest

import ui
from client import ShedError, TauriClient
from fake_gx import (
    SENTINEL_TOKEN, FakeGx, chunk, elicitation_request, hook,
    opaque_permission_request, permission_request, plan_request, question_request,
    tool_call, tool_call_update, turn_completed,
)
from fake_host_agent import FakeHostAgent
from fake_opencode import FakeOpencode
from fake_roost import FakeRoost

pytestmark = pytest.mark.skipif(
    os.environ.get("SHED_TEST_TARGET", "mac") != "tauri",
    reason="tauri-only: the mac app has no machine layer, and so no lanes",
)

FIXTURES = Path(__file__).resolve().parent / "fixtures"

PNG_MAGIC = b"\x89PNG\r\n\x1a\n"

#: Where the panel screenshots are KEPT, when a runner asks for them
#: (`SHED_LANE_SHOTS=<dir>`). Unset by default: the render gate runs in a
#: container with nowhere to put them, and every cell asserts the PNG it captured
#: either way — writing the file is archiving, not the assertion.
SHOTS = os.environ.get("SHED_LANE_SHOTS")

MACHINE = "mini3"
DIRECTORY = "/home/shed/gx-work"

#: The gx session every cell drives. A real gx id shape (a UUIDv7), because an
#: event id is split at its LAST hyphen and a toy id would not exercise that.
GX_SESSION = "01a0fa1e-0000-7000-8000-0000000000ab"
GX_TAB = 11
#: The opencode lane in the same app — the cross-adapter control.
OC_SESSION = "ses_gx_suite_neighbour"
OC_TAB = 12
#: A `grok` tab that reported NO `gx.remote`: status only, no lane. It is what a
#: gx TUI looks like before its remote lane binds (or with `--no-leader`), and
#: the row must still be a `grok` agent row — the kind is a promotion, not a
#: precondition.
BARE_GROK_SESSION = "ses_grok_no_lane"
BARE_GROK_TAB = 13
#: A `grok` tab whose `gx.remote` carries a TRAILING SLASH. `loopback_base_url`
#: rejects it, so the row stays `grok` — a reported URL is slash-free, and one
#: that is not never reached the promotion. It is a different claim from the
#: keyless tab above: this key is PRESENT and still does not promote.
SLASHED_GROK_SESSION = "ses_grok_slashed"
SLASHED_GROK_TAB = 14

#: The adapter's windows, in ms, scaled so no cell waits out a real one.
#:
#: `stall` is deliberately LEFT at its 30 s default. Nothing here needs a stall
#: to fire — every disconnection this file causes is an EOF or a dead listener,
#: which the watcher notices at once — and a scaled-down stall would fire in the
#: quiet part of an unrelated cell and reconnect underneath it, turning the
#: generation assertions into coin flips.
GX_TIMINGS_MS = "resume_window=2000,flush_after=400,down_after=4000"


# ---------------------------------------------------------------------------
# the fakes
# ---------------------------------------------------------------------------


class Feed:
    """Allocates event ids for the gx session and remembers the highest counter
    the lane can have applied.

    Two jobs, and the second is the one a cell cannot do without: a silent resume
    sends `Last-Event-ID: <the MAXIMUM counter seen>`, so a cell asserting the
    cursor has to know what that maximum is at the moment it cuts the stream —
    and the counters deliberately do not arrive in order.
    """

    def __init__(self, fake: FakeGx, session: str):
        self.fake = fake
        self.session = session
        self._next = 20
        self.max_applied = 0

    def adopt(self, envelopes: list[dict]) -> None:
        """Take over from a transcript that is already installed: the highest
        counter in it is what the lane will have applied once it seeds, and the
        allocator starts after it.

        READ, not written — the seed history has to be in place before the app
        launches, and this fixture is built on first use by a cell, long after.
        Reading it back is also what keeps the two from drifting: there is no
        second copy of "the seed's last counter" to update.
        """
        counters = [int(e["eventId"].rsplit("-", 1)[1]) for e in envelopes if e.get("eventId")]
        self.max_applied = max(counters, default=0)
        self._next = max(self._next, self.max_applied + 1)

    def _take(self) -> int:
        n = self._next
        self._next += 1
        self.max_applied = max(self.max_applied, n)
        return n

    @property
    def cursor(self) -> str:
        """The `Last-Event-ID` a silent resume must carry right now."""
        return f"{self.session}-{self.max_applied}"

    # -- pushes ----------------------------------------------------------
    def _emit(self, env: dict, live: bool) -> dict:
        """`live=False` writes the leader's stores WITHOUT broadcasting — see
        `FakeGx.stage_update`. It is how a cell scripts what happened while the
        client was disconnected, before disconnecting it, so the arrival is a
        replay by construction rather than by luck."""
        if live:
            self.fake.push_update(self.session, env)
        else:
            self.fake.stage_update(self.session, env)
        return env

    def chunk(self, kind: str, text: str, prompt: str | None = None,
              live: bool = True) -> dict:
        return self._emit(chunk(self.session, self._take(), kind, text, prompt), live)

    def turn_completed(self, stop_reason: str = "end_turn", live: bool = True) -> dict:
        return self._emit(turn_completed(self.session, self._take(), stop_reason), live)

    def working(self, text: str = "still going") -> None:
        """Put the session back in `Working` the way a real one gets there — a
        chunk. The fold's verdict is what the view reports when it is newer than
        the roster's, so this is the honest way to arrange it."""
        self.chunk("agent_message_chunk", text)


@pytest.fixture(scope="module")
def gx():
    """The gx leader the lane talks to.

    The seeded transcript is the shape the plan's live sample has: chunk streaks
    with `hook_execution` frames INTERLEAVED (15 of 40 real envelopes are hooks),
    and **counters out of transcript order** — gx bumps them from an atomic
    several sessions share, so `12` landing before `11` is ordinary traffic and
    the cursor must still be the maximum.

    It ends WITHOUT a `turn_completed` on purpose: the last streak is then open
    when the seed's history page ends, which is the case the fold has to flush by
    itself ("a standalone page ends there"). The roster says `working` to match.
    """
    fake = FakeGx()
    fake.add_session(GX_SESSION, title="a gx fixture session", cwd=DIRECTORY,
                     activity="working")
    fake.set_history(GX_SESSION, [
        chunk(GX_SESSION, 10, "user_message_chunk", "say pong", "p1"),
        # A tool call and its terminal update — the commonest non-chunk row in a
        # real gx transcript (the plan's live sample is full of them), and the
        # only row type whose display text lives on `tool` rather than on `text`.
        # Their counters are swapped, so the pair is also out of order.
        tool_call(GX_SESSION, 12, "call_seed", title="Execute `ls -la`",
                  command="ls -la"),
        tool_call_update(GX_SESSION, 11, "call_seed", text="total 0"),
        chunk(GX_SESSION, 14, "agent_message_chunk", "po", "p1"),
        hook(GX_SESSION, 13),
        chunk(GX_SESSION, 15, "agent_message_chunk", "ng", "p1"),
    ])
    # Every mutation this app makes is checked against the one session the panel
    # is pinned to.
    fake.pin = GX_SESSION
    try:
        yield fake
    finally:
        fake.stop()


@pytest.fixture(scope="module")
def feed(gx):
    f = Feed(gx, GX_SESSION)
    f.adopt(gx.history[GX_SESSION])
    return f


@pytest.fixture(scope="module")
def oc():
    """The opencode lane in the same app — the control for every cross-adapter
    claim. It has no `interject` capability and exactly three permission
    options, and both facts are asserted THROUGH it rather than assumed."""
    fake = FakeOpencode()
    fake.add_session(OC_SESSION, title="the neighbour", directory=DIRECTORY)
    fake.set_simple_transcript(OC_SESSION, "neighbour asks", "neighbour answers")
    fake.set_status(OC_SESSION, "idle")
    fake.pin = OC_SESSION
    try:
        yield fake
    finally:
        fake.stop()


@pytest.fixture(scope="module")
def gx_home(gx, tmp_path_factory):
    """The `$GROK_HOME` the app's LOCAL credential reader reads.

    Written BEFORE the app launches, because `lane.open`'s very first bearer
    request depends on it. The app never learns the token any other way: there is
    no token in the roost metadata, none in the shed config, and none in an
    environment variable — roost says where the agent is, never how to be let in.
    """
    home = tmp_path_factory.mktemp("grok-home")
    gx.write_home(home)
    return home


@pytest.fixture(scope="module")
def roost(gx, oc):
    """mini3's session: a gx tab, an opencode tab, and a grok tab with no lane.

    Both agent tabs report `source` and a `metadata` bag, and the keys are
    spelled out at the call site rather than reached for through a `fake_roost`
    helper. That is what the wire carries: `metadata` is roost's open extension
    channel, validated by nobody, and a fixture that could only produce the keys
    a helper already knew about would not be testing the channel at all — it
    would be testing the helper.
    """
    fake = FakeRoost().start()
    fake.add_tab(GX_TAB, cwd=DIRECTORY, title="gx | the lane", source="grok",
                 session_id=GX_SESSION, lifecycle="working", detail="session_status",
                 shell_state="unknown", metadata={"gx.remote": gx.reported_url})
    fake.add_tab(OC_TAB, cwd=DIRECTORY, title="oc | the neighbour", source="opencode",
                 session_id=OC_SESSION, lifecycle="working", detail="session_status",
                 shell_state="unknown", metadata={"server_url": oc.base_url})
    fake.add_tab(BARE_GROK_TAB, cwd=DIRECTORY, title="gx | no lane", source="grok",
                 session_id=BARE_GROK_SESSION, lifecycle="working",
                 detail="session_status", shell_state="unknown")
    fake.add_tab(SLASHED_GROK_TAB, cwd=DIRECTORY, title="gx | slashed", source="grok",
                 session_id=SLASHED_GROK_SESSION, lifecycle="working",
                 detail="session_status", shell_state="unknown",
                 metadata={"gx.remote": gx.reported_url + "/"})
    try:
        yield fake
    finally:
        fake.shutdown()


@pytest.fixture(scope="module")
def app(roost, gx_home, mock):
    """A SECOND, INDEPENDENT app instance pointed at the fake session.

    Its own instance because `machines:`, the socket map, `SHED_TAURI_GX_HOME`
    and `SHED_TAURI_GX_TIMINGS_MS` are all read at LAUNCH, exactly as in
    production — and its own throwaway HOME / XDG_RUNTIME_DIR (hence its own IPC
    socket and single-instance lock), so `ui._state` is untouched and this
    module's position in the alphabet is not load-bearing. The pattern, and the
    reasons for each half of it, are `test_tauri_lane.py`'s.

    `pid` and `log` are stashed on the client because two cells ask questions
    about the PROCESS rather than about the app's answers: whether it ever
    spawned a shell, and whether the token reached its log.
    """
    cfg = ui._SUBPROC["tauri"]
    if not cfg.binary.exists():
        raise RuntimeError(
            f"tauri binary not found at {cfg.binary}; build it first (make tauri-build).")
    agent = FakeHostAgent()
    agent.start()
    runtime_dir = Path(tempfile.mkdtemp(prefix="shed-gx-"))
    sock = runtime_dir / cfg.sock_rel
    log = runtime_dir / "gx-lane-ui.log"
    env = ui.subproc_env(
        cfg,
        runtime_dir=runtime_dir,
        mock_base_url=mock.base_url,
        config_path=FIXTURES / "config-gx-lane.yaml",
        host_agent_socket=agent.socket_path,
        roost_sockets={MACHINE: roost.socket_path},
        gx_home=gx_home,
        gx_timings_ms=GX_TIMINGS_MS,
    )
    log_fh = open(log, "wb")
    proc = subprocess.Popen([str(cfg.binary)], env=env, stdout=log_fh,
                            stderr=subprocess.STDOUT)
    client = None
    try:
        ui.await_hermetic("tauri", sock=sock, mock_base_url=mock.base_url,
                          proc=proc, log=log)
        client = TauriClient(sock)
        client.app_pid = proc.pid
        client.app_log = log
        client.wait_until(lambda: client.current_pane() is not None, timeout=30,
                          what="tauri frontend ready")
        client.wait_until(lambda: _row(client, GX_SESSION), timeout=20,
                          what="the machine's rows")
        yield client
    finally:
        if client is not None:
            client.close()
        ui.terminate(proc)
        log_fh.close()
        agent.stop()
        shutil.rmtree(runtime_dir, ignore_errors=True)


# ---------------------------------------------------------------------------
# reading the rows + the lane
# ---------------------------------------------------------------------------


def _rows(app: TauriClient) -> list[dict]:
    return [s for s in app.call("rc.list").get("sessions", [])
            if s.get("origin_kind") == "machine"]


def _row(app: TauriClient, session_id: str) -> dict | None:
    """The machine row for a roost tab that reported `session_id`."""
    for row in _rows(app):
        lane = row.get("agent_lane") or {}
        if lane.get("session_id") == session_id:
            return row
    return None


def _row_by_slug(app: TauriClient, tab: int) -> dict | None:
    for row in _rows(app):
        if row.get("slug") == str(tab):
            return row
    return None


def _open(app: TauriClient, session_id: str = GX_SESSION) -> dict:
    return app.call("lane.open", {"machine": MACHINE, "session_id": session_id})


def _messages(app: TauriClient, session_id: str = GX_SESSION) -> dict:
    return app.call("lane.messages", {"machine": MACHINE, "session_id": session_id})


def _texts(app: TauriClient, session_id: str = GX_SESSION) -> list[str]:
    return [m.get("text") or "" for m in _messages(app, session_id)["messages"]]


def _approvals(app: TauriClient, session_id: str = GX_SESSION) -> list[dict]:
    return app.call("lane.approvals",
                    {"machine": MACHINE, "session_id": session_id})["approvals"]


def _ready(app: TauriClient, session_id: str = GX_SESSION) -> dict:
    """Open the lane (idempotent) and wait for its first generation to be whole."""
    _open(app, session_id)
    app.wait_until(
        lambda: _messages(app, session_id)["generation"] >= 1
        and _messages(app, session_id)["messages"],
        timeout=25,
        what=f"the seeded lane on {session_id}",
    )
    return _messages(app, session_id)


def _close(app: TauriClient, session_id: str = GX_SESSION) -> None:
    """Close the lane if one is open. Tolerant on purpose: a cell that wants a
    FRESH entry (a new credential source, a new pin epoch) should not have to
    know whether the cell before it left one behind."""
    try:
        app.call("lane.close", {"machine": MACHINE, "session_id": session_id})
    except ShedError:
        pass


def _settled(app: TauriClient, session_id: str = GX_SESSION) -> dict:
    """Wait until the transcript stops growing, then answer with it.

    A chunk streak becomes a row on a TIMER (`flush_after`), so a count taken the
    instant a cell starts can still move underneath it — the previous cell's last
    push is very likely still open in the fold. Any cell that compares counts
    across an event starts here instead of at `_ready`.

    The window is comfortably longer than the scaled `flush_after`; two reads
    100 ms apart would prove nothing.
    """
    state = {"rows": -1, "since": 0.0}

    def _stable() -> bool:
        rows = len(_messages(app, session_id)["messages"])
        now = time.monotonic()
        if rows != state["rows"]:
            state["rows"], state["since"] = rows, now
            return False
        return now - state["since"] >= 0.8

    _ready(app, session_id)
    app.wait_until(_stable, timeout=20, what="the transcript to settle")
    return _messages(app, session_id)


def _answer(app: TauriClient, approval_id: str, answer: dict,
            session_id: str = GX_SESSION) -> dict:
    return app.call("lane.answer", {"machine": MACHINE, "session_id": session_id,
                                    "approval_id": approval_id, "answer": answer})


def _send(app: TauriClient, text: str, mode: str | None = None,
          session_id: str = GX_SESSION) -> dict:
    params: dict = {"machine": MACHINE, "session_id": session_id, "text": text}
    if mode is not None:
        params["mode"] = mode
    return app.call("lane.send", params)


def _error(fn) -> ShedError:
    with pytest.raises(ShedError) as caught:
        fn()
    return caught.value


def _wait_approval(app: TauriClient, approval_id: str, done, what: str) -> dict:
    """Wait for the approval `approval_id` to satisfy `done`, and answer with it."""
    def _find() -> dict | None:
        for a in _approvals(app):
            if a["id"] == approval_id and done(a):
                return a
        return None
    app.wait_until(_find, timeout=20, what=what)
    return _find()


# ---------------------------------------------------------------------------
# the panel
# ---------------------------------------------------------------------------


def _dump(app: TauriClient) -> dict | None:
    """What the transcript PANEL rendered, or `None` when none is mounted."""
    return app.call("lane.dump")["lane"]


def _mounted(app: TauriClient, session_id: str) -> bool:
    """Is the panel mounted on this session AND past its first read?

    A panel reports from its FIRST render — before `lane.open` has answered — on
    purpose, so `lane.dump` going non-null is how a caller learns a panel exists
    even when the open FAILED. So "mounted" is not "showing something", and this
    asks the second.

    `kind` is in the success arm because the panel gets its rows and its
    capabilities from two different awaits: the transcript comes from a read that
    a lane-event can trigger before `lane.open`'s promise resolves, and `kind` /
    `interject` come from the open itself. Without it a cell could legitimately
    see `generation >= 1` beside `kind: ""` and `interject: null` and conclude
    the adapter advertises nothing. The failed-open arm keeps `error` alone —
    there is no `kind` to wait for there, and cell 3 needs exactly that.
    """
    d = _dump(app)
    return bool(d and d["session_id"] == session_id
                and ((d["generation"] >= 1 and d["kind"]) or d["error"]))


def _panel(app: TauriClient, session_id: str = GX_SESSION) -> dict:
    """Mount the transcript panel on a session and wait for its first read."""
    app.call("ui.show_lane", {"machine": MACHINE, "session_id": session_id})
    app.wait_until(lambda: _mounted(app, session_id), timeout=25,
                   what="the transcript panel to mount and report")
    return _dump(app)


def _card(app: TauriClient, approval_id: str) -> dict | None:
    for card in (_dump(app) or {}).get("approvals", []):
        if card["id"] == approval_id:
            return card
    return None


def _unmount(app: TauriClient) -> None:
    """Close the panel and prove `lane.dump` goes back to `null`.

    **This CLOSES the lane too.** The panel calls `lane.open` on mount and
    `lane.close` on unmount — it owns the lifecycle, which is what makes the
    panel the only door a user has — so a cell that keeps working on the lane
    afterwards re-opens it with `_ready`. Learned the hard way: without that, the
    next `lane.answer` is a `no_lane` that `wait_until` swallows into a timeout
    somewhere else entirely.
    """
    app.call("ui.close_lane")
    app.wait_until(lambda: _dump(app) is None, timeout=20,
                   what="the panel to unmount and clear its report")


def _shot(app: TauriClient, name: str) -> None:
    """Capture the window and keep it under `$SHED_LANE_SHOTS` when set."""
    if platform.system() == "Darwin":
        return
    png, w, h = app.screenshot(scale=1)
    assert png[:8] == PNG_MAGIC and w > 0 and h > 0
    if SHOTS:
        out = Path(SHOTS).expanduser()
        out.mkdir(parents=True, exist_ok=True)
        (out / name).write_bytes(png)


def _descendants(pid: int) -> list[tuple[int, str]]:
    """Every descendant of `pid`, as `(pid, comm)`, walked out of `/proc`."""
    children: dict[int, list[int]] = {}
    comm: dict[int, str] = {}
    for entry in Path("/proc").iterdir():
        if not entry.name.isdigit():
            continue
        try:
            stat = (entry / "stat").read_text()
        except OSError:
            continue
        # `comm` may contain spaces and parentheses, so it is delimited by the
        # FIRST '(' and the LAST ')' — splitting on whitespace would misread a
        # process called `(sh )`.
        open_at, close_at = stat.find("("), stat.rfind(")")
        if open_at < 0 or close_at < 0:
            continue
        fields = stat[close_at + 2:].split()
        if len(fields) < 2:
            continue
        this = int(entry.name)
        comm[this] = stat[open_at + 1:close_at]
        children.setdefault(int(fields[1]), []).append(this)
    out: list[tuple[int, str]] = []
    queue = [pid]
    while queue:
        for child in children.get(queue.pop(), []):
            out.append((child, comm.get(child, "")))
            queue.append(child)
    return out


# ---------------------------------------------------------------------------
# (1) kinds: a lane-bearing gx tab, and a lane-less grok one
# ---------------------------------------------------------------------------


def test_a_grok_tab_is_gx_when_it_reported_a_remote_and_grok_when_it_did_not(app, gx):
    """**The kind is a promotion off the metadata, made in shed-core** (§3.2).

    roost reports `source: "grok"` for both tabs — it never learns the word "gx".
    What separates them is `metadata["gx.remote"]`: a loopback base URL promotes
    the row to `gx` and stamps an `agent_lane`; its absence leaves the row `grok`,
    which is a first-class creatable kind with a status row and **no transcript**.
    That the keyless tab still gets a row is the part worth pinning — a gx TUI
    before its lane binds is not invisible, it is `grok`.
    """
    lane_row = _row(app, GX_SESSION)
    assert lane_row is not None, _rows(app)
    assert lane_row["kind"] == "gx"
    assert lane_row["agent_lane"] == {
        "kind": "gx",
        "session_id": GX_SESSION,
        "server_url": gx.reported_url,
    }
    # `agent_lane`, NOT `lane` — `lane` is the RC hub's lane token on this same
    # DTO and keeps meaning what it meant.
    assert lane_row.get("lane") is None

    bare = _row_by_slug(app, BARE_GROK_TAB)
    assert bare is not None, _rows(app)
    assert bare["kind"] == "grok", "no gx.remote ⇒ no promotion"
    assert bare.get("agent_lane") is None, "lane-less by design"
    refusal = _error(lambda: _open(app, BARE_GROK_SESSION))
    assert refusal.code == "no_lane", refusal

    # **A trailing slash is not a reported URL.** The key is PRESENT here, and
    # the row is still `grok`: `loopback_base_url` wants http, a loopback host, an
    # explicit port and nothing after it, and a record is matched against the
    # reported URL byte-for-byte. Refusing the near-miss rather than normalising
    # it is deliberate — both sides come from the same producer (gx writes the
    # record, roost forwards its URL), so a difference means something REWROTE a
    # URL, which is the case worth refusing rather than papering over.
    slashed = _row_by_slug(app, SLASHED_GROK_TAB)
    assert slashed is not None, _rows(app)
    assert slashed["kind"] == "grok", "a trailing slash must not promote"
    assert slashed.get("agent_lane") is None
    assert _error(lambda: _open(app, SLASHED_GROK_SESSION)).code == "no_lane"

    # The neighbour is untouched by any of it.
    assert (_row(app, OC_SESSION) or {}).get("kind") == "opencode"


# ---------------------------------------------------------------------------
# (2) the credential seam: healthz before the bearer, files not a shell
# ---------------------------------------------------------------------------


def test_open_reads_the_token_from_the_fixture_home_and_healthz_precedes_the_bearer(app, gx):
    """**The pin comes before the credential leaves** (§3.3), and on a local
    machine the credential is READ, never shelled out for.

    Three claims, in the order they matter:

    1. the lane opens at all — which it can only do by finding the token in
       `$SHED_TAURI_GX_HOME`, because nothing else in the app knows it;
    2. the token-free `GET /v1/healthz` is the FIRST request, before anything
       carrying an `Authorization` header. A client that sent the bearer first
       would hand a credential to whatever process happened to be on that port;
    3. no `sh` was ever spawned. `ReachKind::Local` reads files directly, with
       gx's own eligibility checks; the probe script is the SSH path's business,
       and a local reader that shelled out would be a second implementation to
       keep in step.
    """
    _close(app)
    gx.clear_requests()
    _ready(app)

    ledger = gx.requests()
    assert ledger, "lane.open made no request at all"
    assert ledger[0].path == "/v1/healthz" and not ledger[0].had_bearer, (
        f"the first request was not a token-free healthz: {ledger[:3]}")
    bearer = gx.bearer_requests()
    assert bearer, "no bearer request followed the pin"
    assert ledger.index(bearer[0]) > 0, "a bearer request preceded healthz"
    # The first bearer request is the roster read `lane.open` does before it
    # subscribes — which is exactly where the pin has to have happened already.
    assert bearer[0].path == f"/v1/sessions/{GX_SESSION}", bearer[0]
    assert all(r.bearer_ok for r in bearer), "a bearer request carried the wrong token"

    if platform.system() == "Linux":
        shells = [d for d in _descendants(app.app_pid)
                  if d[1] in {"sh", "bash", "dash", "ssh"}]
        assert shells == [], f"the local reader spawned a shell: {shells}"


# ---------------------------------------------------------------------------
# (3) the pin refuses a moved leader, and says so on screen
# ---------------------------------------------------------------------------


def test_an_instance_id_mismatch_is_unavailable_and_sends_no_token(app, gx, gx_home):
    """**A token is never sent to a leader that is not the one it was read
    beside** (§3.3).

    The record on disk names instance A; `healthz` answers B. `ensure_pinned`
    re-discovers once (the record has not moved, so it still says A), and then
    refuses — with **nothing carrying an `Authorization` header on the wire**.
    That empty ledger IS the security property: a token pinned to the wrong
    leader has already leaked, and no later check can un-leak it.

    The panel half is the inline refusal: `lane.open` failing renders in the
    banner rather than leaving an empty transcript with no explanation.
    """
    _close(app)
    gx.clear_requests()
    gx.set_instance_id("cafe0000cafe0000cafe0000cafe0000")
    try:
        refusal = _error(lambda: _open(app))
        assert refusal.code == "unavailable", refusal
        assert gx.bearer_requests() == [], (
            f"a token went to a leader that failed the pin: {gx.bearer_requests()}")
        assert [r.path for r in gx.requests()].count("/v1/healthz") >= 1

        panel = _panel(app)
        assert panel["error"], "a failed open renders nothing at all"
        assert panel["generation"] == 0 and panel["rows"] == []
        _unmount(app)
    finally:
        # Put the leader back and re-write the record, so the next cell opens a
        # lane rather than inheriting this one's refusal.
        gx.set_instance_id("facade00facade00facade00facade00")
        gx.write_home(gx_home)
    _ready(app)


# ---------------------------------------------------------------------------
# (4) the seed renders
# ---------------------------------------------------------------------------


def test_the_seeded_history_renders_with_hooks_transparent_and_the_open_streak_flushed(app):
    """**Chunks coalesce, hooks do not split them, and the page's last streak is
    one row** (§3.4).

    The seed is `user("say pong")`, a tool call and its result, then `"po"`, a
    `hook_execution`, `"ng"` — with counters out of transcript order. Four rows
    come out: the prompt, the two tool rows, and `"pong"` as ONE assistant row.

    Each is a separate way to get it wrong. A fold that let a hook end a streak
    would render `"po"` and `"ng"` as two rows; one that dropped hooks before
    advancing the cursor would resume from the wrong place; one that discarded
    the streak still open when the page ended would render the answer not at
    all; and a tool row carries its text on `tool` rather than on `text`, so a
    panel that only read `text` would render two blank lines where the command
    and its output should be.
    """
    # `_settled`, not `_ready`: the seed's LAST streak is still open when the
    # subscription reaches `Ready` — that is the whole point of it — and the
    # watcher's flush timer is what turns it into a row a moment later. A cell
    # that read the transcript at `Ready` would see the prompt and no answer,
    # which is exactly the bug this asserts against, only with the wrong cause.
    view = _settled(app)
    assert _texts(app) == ["say pong", "", "", "pong"], view["messages"]
    assert [m["role"] for m in view["messages"]] == ["user", "tool", "tool", "assistant"]
    assert [m["type"] for m in view["messages"]] == [
        "text", "tool_use", "tool_result", "text"]
    assert view["stale"] is None

    panel = _panel(app)
    assert [r["text"] for r in panel["rows"]] == ["say pong", "", "", "pong"]
    # `name · detail` — the tool name off `_meta["x.ai/tool"]`, the detail off
    # `rawInput.command` going in and off the result's content coming back.
    assert [r["tool"] for r in panel["rows"]] == [
        None, "bash · ls -la", "bash · total 0", None]
    assert panel["kind"] == "gx", "the header says which agent this is"
    assert panel["session_id"] == GX_SESSION
    _shot(app, "tauri-gx-transcript.png")
    _unmount(app)


# ---------------------------------------------------------------------------
# (5) live chunks: closed by turn_completed, and flushed by the timer
# ---------------------------------------------------------------------------


def test_a_streak_closes_on_turn_completed_and_a_silent_one_flushes_by_itself(app, feed):
    """**Two ways a streak ends, and both are pinned** (§3.4).

    A `turn_completed` closes it at once and makes the session idle. Nothing
    closes the second one — no terminator ever arrives — and it still becomes a
    row, because the watcher flushes a streak that has been silent for
    `flush_after`. Without that timer a paused agent's half-written answer would
    sit invisible in the fold until it happened to say something else.
    """
    before = len(_settled(app)["messages"])

    feed.chunk("agent_message_chunk", "alpha", "p2")
    feed.turn_completed("end_turn")
    app.wait_until(lambda: _texts(app)[before:] == ["alpha"], timeout=15,
                   what="the turn_completed-closed streak")
    app.wait_until(lambda: _messages(app)["activity"] == "idle", timeout=15,
                   what="turn_completed to make the session idle")

    # No terminator this time: the flush timer is the only thing that can emit it.
    feed.chunk("agent_message_chunk", "beta", "p3")
    app.wait_until(lambda: _texts(app)[before:] == ["alpha", "beta"], timeout=15,
                   what="the silent streak to flush by itself")
    assert _messages(app)["activity"] == "working", "a chunk means the turn is live"


# ---------------------------------------------------------------------------
# (6) send modes, and the interject toggle that is gx's alone
# ---------------------------------------------------------------------------


def test_send_modes_reach_the_pinned_session_and_the_toggle_is_gx_only(app, gx, oc, feed):
    """**`interject` is a capability, and the panel reads it** (§3.5).

    The IPC half: `lane.send` posts `mode: queue` by default and `mode:
    interject` when asked, to the pinned session and nowhere else (the fake's
    guard records any other addressee as a violation, and answers 500 so it can
    never look successful).

    The panel half is the one plan 015 could not make: `capabilities` were stored
    and never read, so the panel offered whatever it offered. Now the gx panel
    has an Interject toggle and the opencode panel — in the SAME app, against the
    same code — has none, because opencode's adapter does not advertise one.
    """
    _ready(app)
    feed.working("driving")
    app.wait_until(lambda: _messages(app)["activity"] == "working", timeout=15,
                   what="a working turn to interject into")

    gx.clear_requests()
    _send(app, "queued prompt")
    _send(app, "interjected prompt", mode="interject")
    posts = gx.requests_to("/messages")
    assert len(posts) == 2, [p.path for p in posts]
    assert json.loads(posts[0].body)["mode"] == "queue", posts[0].body
    assert json.loads(posts[1].body)["mode"] == "interject", posts[1].body
    assert all(p.path == f"/v1/sessions/{GX_SESSION}/messages" for p in posts)
    assert gx.violations == [], gx.violations

    # An unknown mode never reaches the wire.
    before = len(gx.requests_to("/messages"))
    assert _error(lambda: _send(app, "later", mode="whenever")).code == "bad_request"
    assert len(gx.requests_to("/messages")) == before

    gx_panel = _panel(app)
    assert gx_panel["interject"] == {"on": False, "enabled": True}, (
        "the gx panel offers an interject toggle, enabled while working")

    # …and it goes DISABLED the moment the turn ends. Both halves are needed:
    # "present" and "usable" are different claims, which is exactly why the
    # report's `interject` is tri-state rather than a bool. gx answers `409
    # not_accepting` to an interject outside a working turn, so a toggle that
    # stayed live past `turn_completed` would be an affordance whose only
    # outcome is an error in the banner above it.
    feed.turn_completed("end_turn")
    app.wait_until(
        lambda: (_dump(app) or {}).get("interject") == {"on": False, "enabled": False},
        timeout=20,
        what="the interject toggle to go disabled when the turn ends",
    )
    _unmount(app)

    _ready(app, OC_SESSION)
    oc_panel = _panel(app, OC_SESSION)
    assert oc_panel["kind"] == "opencode"
    assert oc_panel["interject"] is None, (
        "opencode advertises no interject — an always-on toggle would be an "
        "affordance whose only outcome is an error")
    _unmount(app)
    _close(app, OC_SESSION)


# ---------------------------------------------------------------------------
# (7) a permission with opaque ids
# ---------------------------------------------------------------------------


def test_a_permission_renders_its_offered_options_and_a_choice_answers_by_id(app, gx, feed):
    """**Buttons are the agent's options, by label, in the offered order, and a
    click posts that option's own id** (§3.5).

    The ids here are `p-1…p-4` and say NOTHING about their semantics — which is
    the realistic case, since `optionId` is free text and gx's own fixture pairs
    `optionId: "allow-once"` with `kind: "allow_once"` only by coincidence. A
    panel that derived a decision from an id would post the wrong one here; a
    panel that rendered a fixed three would show four buttons' worth of choice as
    three.

    The scripted `{permission: …}` form still works — it resolves by KIND
    (`p-1` is the only `allow_once`), which is what keeps every existing caller
    working — and a second answer is refused rather than silently re-posted.
    """
    _ready(app)
    approval_id = "call_opaque"
    resource = gx.add_approval(GX_SESSION, approval_id, "permission",
                               "session/request_permission",
                               opaque_permission_request(GX_SESSION, "rm -rf /tmp/x"))
    gx.push_approval_frame(GX_SESSION, resource)

    live = _wait_approval(app, approval_id, lambda a: a["options"],
                          "the permission to reach the lane")
    assert [o["id"] for o in live["options"]] == ["p-1", "p-2", "p-3", "p-4"]
    assert [o["kind"] for o in live["options"]] == [
        "allow_once", "allow_always", "reject_once", "reject_always"]
    assert "rm -rf /tmp/x" in json.dumps(live), live

    # The first sight of a pending approval leaves a trace in the transcript, so
    # a reader scrolling back sees where the agent stopped to ask.
    app.wait_until(
        lambda: any(m["type"] == "approval_request"
                    and (m.get("approval") or {}).get("id") == approval_id
                    for m in _messages(app)["messages"]),
        timeout=15, what="the approval_request row")

    panel = _panel(app)
    card = _card(app, approval_id)
    assert card is not None, panel
    assert card["buttons"] == ["Yes, proceed", "Yes, and remember", "No", "Never"]
    assert [o["id"] for o in card["options"]] == ["p-1", "p-2", "p-3", "p-4"], (
        "a label is what a person reads; the id is what the click sends")
    _shot(app, "tauri-gx-approval.png")
    _unmount(app)
    _ready(app)

    # The panel's own form: the option's id, verbatim.
    _answer(app, approval_id, {"choice": "p-2"})
    assert gx.answered_with(GX_SESSION, approval_id) == {
        "outcome": {"outcome": "selected", "optionId": "p-2"}}

    # A second answer is refused. `resolved` rather than `submitted` because that
    # is the real sequence: the agent proceeded and closed the interaction.
    gx.resolve_approval(GX_SESSION, approval_id)
    assert _error(lambda: _answer(app, approval_id, {"choice": "p-1"})).code == \
        "already_resolved"

    # …and the by-kind form, on a fresh ask: `allow-once` resolves to the only
    # option DECLARING `allow_once`, which is `p-1` and not the one named it.
    second = "call_opaque_2"
    gx.push_approval_frame(GX_SESSION, gx.add_approval(
        GX_SESSION, second, "permission", "session/request_permission",
        opaque_permission_request(GX_SESSION, "rm -rf /tmp/y")))
    _wait_approval(app, second, lambda a: a["options"], "the second permission")
    _answer(app, second, {"permission": "allow-once"})
    assert gx.answered_with(GX_SESSION, second) == {
        "outcome": {"outcome": "selected", "optionId": "p-1"}}
    gx.resolve_approval(GX_SESSION, second)
    feed.working("carrying on")


# ---------------------------------------------------------------------------
# (8) a two-question form, keyed by the question's text
# ---------------------------------------------------------------------------


def test_a_two_question_form_posts_answers_keyed_by_the_question_text(app, gx, feed):
    """**gx files an answer under the question's TEXT** (§3.1 #4).

    `ask_user_question` does `answers.insert(q.question.clone(), labels)`, so an
    answer filed under `q1` or under a position is an answer the agent never
    reads. The contract's `Question` answers stay POSITIONAL — the panel has no
    business knowing gx's keying — and the adapter maps position to key. Two
    questions, one of them `multiSelect`, is the shape where a positional bug
    survives: with one question you cannot tell a key from an index.
    """
    _ready(app)
    approval_id = "call_questions"
    first, second = "Which branch?", "Which checks should run?"
    gx.push_approval_frame(GX_SESSION, gx.add_approval(
        GX_SESSION, approval_id, "question", "session/ask_user_question",
        question_request(GX_SESSION, [
            {"question": first,
             "options": [{"label": "main"}, {"label": "release"}]},
            {"question": second, "multiSelect": True,
             "options": [{"label": "lint"}, {"label": "test"}, {"label": "docs"}]},
        ])))

    live = _wait_approval(app, approval_id, lambda a: len(a["questions"]) == 2,
                          "the two-question form")
    assert [q["question"] for q in live["questions"]] == [first, second]
    assert [q["id"] for q in live["questions"]] == [first, second], (
        "the key an answer is filed under rides on the DTO")
    assert [q["multiple"] for q in live["questions"]] == [False, True]
    assert all(q["custom"] is False for q in live["questions"]), (
        "free text on gx needs an annotations channel the contract cannot carry")

    panel = _panel(app)
    card = _card(app, approval_id)
    assert card is not None, panel
    assert [q["options"] for q in card["questions"]] == [
        ["main", "release"], ["lint", "test", "docs"]]
    assert card["buttons"] == ["Send answer"], "two questions cannot be one click"
    _unmount(app)
    _ready(app)

    _answer(app, approval_id, {"question": [["release"], ["lint", "docs"]]})
    assert gx.answered_with(GX_SESSION, approval_id) == {
        "outcome": "accepted",
        "answers": {first: ["release"], second: ["lint", "docs"]},
    }
    gx.resolve_approval(GX_SESSION, approval_id)
    feed.working("carrying on")


# ---------------------------------------------------------------------------
# (9) a plan approval, through the same renderer
# ---------------------------------------------------------------------------


def test_a_plan_approval_offers_two_synthesized_options(app, gx, feed):
    """**One generic renderer covers plan approvals too** (§3.5).

    gx sends no options for a plan — it takes a bare string outcome — so the
    adapter synthesizes the two, which is the one place an option id is not
    arbitrary: `approved` and `cancelled` ARE the outcomes. They are still
    round-tripped rather than parsed, and the panel renders them exactly as it
    renders a permission's five: no second code path, no second way to be wrong.
    """
    _ready(app)
    approval_id = "call_plan"
    gx.push_approval_frame(GX_SESSION, gx.add_approval(
        GX_SESSION, approval_id, "plan_approval", "session/request_plan_approval",
        plan_request(GX_SESSION, "1. read the file\n2. change one line")))

    live = _wait_approval(app, approval_id, lambda a: a["options"],
                          "the plan approval")
    assert live["title"] == "Approve plan"
    assert [(o["id"], o["label"], o["kind"]) for o in live["options"]] == [
        ("approved", "Approve", "allow_once"),
        ("cancelled", "Cancel", "reject_once"),
    ]
    assert "read the file" in (live["detail"] or "")

    panel = _panel(app)
    card = _card(app, approval_id)
    assert card is not None, panel
    assert card["buttons"] == ["Approve", "Cancel"], card
    _unmount(app)
    _ready(app)

    _answer(app, approval_id, {"choice": "approved"})
    assert gx.answered_with(GX_SESSION, approval_id) == {"outcome": "approved"}
    gx.resolve_approval(GX_SESSION, approval_id)
    feed.working("carrying on")


# ---------------------------------------------------------------------------
# (10) an elicitation the build cannot render buttons for
# ---------------------------------------------------------------------------


def test_an_mcp_elicitation_renders_its_request_and_reject_declines(app, gx, feed):
    """**A kind this build cannot read gets the agent's own request, and one
    honest answer** (§3.4, §3.5).

    Synthesizing buttons for a schema the adapter cannot interpret would be
    inventing an answer, so it offers none — and the panel shows `request_json`
    verbatim with a Reject beside it. Reject is the only thing that is always
    meaningful, and offering it is what keeps the card from being a dead end that
    leaves the agent blocked for ever.
    """
    _ready(app)
    approval_id = "call_elicit"
    gx.push_approval_frame(GX_SESSION, gx.add_approval(
        GX_SESSION, approval_id, "mcp_elicitation", "session/elicit",
        elicitation_request(GX_SESSION, "the deploy key, please")))

    live = _wait_approval(app, approval_id, lambda a: a["kind"] == "mcp_elicitation",
                          "the elicitation")
    assert live["options"] == [] and live["questions"] == []
    assert "requestedSchema" in live["request_json"]

    panel = _panel(app)
    card = _card(app, approval_id)
    assert card is not None, panel
    assert card["buttons"] == ["Reject"], card
    assert card["options"] == [], "no invented buttons for a schema it cannot read"
    _unmount(app)
    _ready(app)

    _answer(app, approval_id, {"reject": True})
    assert gx.answered_with(GX_SESSION, approval_id) == {"outcome": "decline"}
    gx.resolve_approval(GX_SESSION, approval_id)
    feed.working("carrying on")


# ---------------------------------------------------------------------------
# (17) the two-phase approval a live gx really sends
# ---------------------------------------------------------------------------


def test_a_placeholder_approval_is_replaced_by_the_real_request(app, gx, feed):
    """**A live gx announces every approval TWICE, and the second one is the one
    with the buttons.**

    Not in §3.5's list, and found only against a real leader: the first frame is
    a `pending_interaction` placeholder whose `method` and `request` are both
    null — therefore no options — and the real request arrives in a second
    `approval` frame carrying the SAME id. A panel that rendered the first and
    stopped would show a human a permission with no buttons and no way to
    proceed.

    Two things have to hold for the second frame to win, and both are asserted
    here: the fold's status ranking must let a SAME-rank frame refresh an entry
    (both are `pending`; a strict "newer only" rule would drop it), and the panel
    must fall back to the raw-request card while the options are missing rather
    than rendering an empty button row.
    """
    _ready(app)
    approval_id = "call_two_phase"
    placeholder = gx.add_placeholder_approval(GX_SESSION, approval_id)
    gx.push_approval_frame(GX_SESSION, placeholder)

    live = _wait_approval(app, approval_id, lambda a: a["kind"] == "placeholder",
                          "the pending_interaction placeholder")
    assert live["options"] == [], "a placeholder carries nothing to offer"

    panel = _panel(app)
    card = _card(app, approval_id)
    assert card is not None, panel
    assert card["buttons"] == ["Reject"], (
        f"a placeholder must not render as a permission with no buttons: {card}")

    # …and now the real request, same id.
    real = gx.add_approval(GX_SESSION, approval_id, "permission",
                           "session/request_permission",
                           permission_request(GX_SESSION, "git push --dry-run"))
    gx.push_approval_frame(GX_SESSION, real)

    live = _wait_approval(app, approval_id, lambda a: a["kind"] == "permission",
                          "the real request behind the placeholder")
    assert len(live["options"]) == 5, live["options"]
    app.wait_until(lambda: len((_card(app, approval_id) or {}).get("options", [])) == 5,
                   timeout=15, what="the panel to pick up the real options")
    real_card = _card(app, approval_id)
    assert real_card is not None, _dump(app)
    assert real_card["buttons"][0] == "Yes, and don't ask again"
    _unmount(app)
    _ready(app)

    _answer(app, approval_id, {"choice": "reject-once"})
    gx.resolve_approval(GX_SESSION, approval_id)
    feed.working("carrying on")


# ---------------------------------------------------------------------------
# (18) the real five-option permission, and the decision it cannot express
# ---------------------------------------------------------------------------


def test_five_options_refuse_the_ambiguous_decision_and_take_the_choice(app, gx, feed):
    """**A real gx permission offers five options, TWO of them `allow_once` —
    so `{permission: "allow-once"}` is refused, by design.**

    The two are "Yes, proceed" and "Yes, and don't ask again for anything", and
    the second turns prompting off for the whole session. A three-valued decision
    cannot say which the human meant, and guessing would silently disable every
    future prompt. So the adapter refuses with a message naming both, and
    `{choice: "<id>"}` — what the panel's buttons send — is what can say it.

    §3.5's cell 7 uses a synthetic four-option set with exactly one `allow_once`,
    and that stays valid: it catches an id-sniffing client. This catches a
    by-kind resolver that picks the first match. They fail differently, so both
    are here.
    """
    _ready(app)
    approval_id = "call_five"
    gx.push_approval_frame(GX_SESSION, gx.add_approval(
        GX_SESSION, approval_id, "permission", "session/request_permission",
        permission_request(GX_SESSION, "curl https://example.com")))

    live = _wait_approval(app, approval_id, lambda a: len(a["options"]) == 5,
                          "the five-option permission")
    allow_once = [o["id"] for o in live["options"] if o["kind"] == "allow_once"]
    assert allow_once == ["enable-always-approve", "allow-once"], live["options"]

    refusal = _error(lambda: _answer(app, approval_id, {"permission": "allow-once"}))
    assert refusal.code == "bad_request", refusal
    assert "enable-always-approve" in refusal.message and "allow-once" in refusal.message, (
        f"the refusal must name both candidates: {refusal.message}")
    assert gx.answered_with(GX_SESSION, approval_id) is None, (
        "an ambiguous decision must not post anything at all")

    panel_before = _panel(app)
    card = _card(app, approval_id)
    assert card is not None, panel_before
    assert len(card["options"]) == 5, panel_before
    _unmount(app)
    _ready(app)

    _answer(app, approval_id, {"choice": "allow-once"})
    assert gx.answered_with(GX_SESSION, approval_id) == {
        "outcome": {"outcome": "selected", "optionId": "allow-once"}}
    gx.resolve_approval(GX_SESSION, approval_id)
    feed.working("carrying on")


# ---------------------------------------------------------------------------
# (11) a silent resume
# ---------------------------------------------------------------------------


def test_a_closed_stream_resumes_silently_and_reconciles_what_it_missed(app, gx, feed):
    """**A cursor-capable adapter resumes without rebuilding anything** (§3.1 #1).

    The stream is cut. The reconnect carries `Last-Event-ID` = the MAXIMUM
    counter applied — not the last one to arrive, because gx's counters come out
    of order — and the transcript, the generation and the view all survive: no
    `Reset`, no reseed, nothing flashing empty on screen.

    Then the part a cursor cannot cover: **approval frames are not resumable**,
    so an approval raised while the stream was down would simply not exist for
    this client. Every reconnect, silent or not, re-fetches what the stream does
    not replay — and the approval appears anyway.
    """
    view = _settled(app)
    generation, before = view["generation"], len(view["messages"])
    cursor = feed.cursor
    gx.clear_requests()

    # **The gap is scripted BEFORE the stream is cut**, into the leader's stores
    # only (`live=False`). The app reconnects on its own ~100 ms backoff, so
    # "cut, then push" is a race the harness loses often enough to matter — and
    # when it loses, the frame arrives LIVE and the cell passes without ever
    # exercising a replay. Staged, the only way it can arrive is the resume.
    gap_approval = "call_in_the_gap"
    gx.add_approval(GX_SESSION, gap_approval, "permission",
                    "session/request_permission",
                    opaque_permission_request(GX_SESSION, "cat /etc/hosts"))
    feed.chunk("agent_message_chunk", "after the gap", "p9", live=False)
    feed.turn_completed("end_turn", live=False)

    gx.close_streams()

    app.wait_until(lambda: "after the gap" in _texts(app), timeout=20,
                   what="the gap frame to arrive on the resumed stream")
    _wait_approval(app, gap_approval, lambda a: a["options"],
                   "the gap approval to be reconciled")

    view = _messages(app)
    assert view["generation"] == generation, "a silent resume must not reseed"
    assert view["stale"] is None
    # ONCE. The ring replays it and the seed's history contains it, so a fold
    # without a `seen` set would render the same frame twice.
    assert _texts(app).count("after the gap") == 1, _texts(app)
    assert len(view["messages"]) > before

    resumes = [r for r in gx.requests_to("/events") if r.last_event_id]
    assert resumes, [r.path for r in gx.requests_to("/events")]
    assert resumes[0].last_event_id == cursor, (
        f"resumed from {resumes[0].last_event_id}, not the maximum counter {cursor}")

    _answer(app, gap_approval, {"choice": "p-3"})
    gx.resolve_approval(GX_SESSION, gap_approval)


# ---------------------------------------------------------------------------
# (12) a server reset reseeds
# ---------------------------------------------------------------------------


def test_a_server_reset_reseeds_with_a_new_generation_and_never_a_partial_view(app, gx, feed):
    """**`reset` is the server saying "your view is not resumable"**, and the
    answer is a whole new generation — staged, then swapped (§3.1 #1).

    The generation moves, the transcript survives (it is re-read from history),
    and **every observation is of one whole generation** — the old one exactly as
    it was, or the new one complete. Never a half-built anything.

    That last claim is the reason the view is staged at all, and it is asserted
    by polling THROUGH the reseed, not after it: a reseed that swapped rows in as
    they folded would blank the panel for as long as the seed took, on a
    transcript somebody is reading.

    It is deliberately NOT "the row count never falls". A new generation can
    legitimately be SHORTER: the `approval_request` rows in the old one were
    emitted on first sight of approvals that are all resolved by now, so the
    rebuild does not re-emit them. Asserting a floor on the count would have
    pinned that incidental arithmetic instead of the staging property.
    """
    view = _settled(app)
    generation = view["generation"]
    rows = _texts(app)

    gx.push_reset(GX_SESSION, "slow_consumer")

    def _reseeded() -> bool:
        now = _messages(app)
        if now["generation"] > generation:
            return True
        assert [m.get("text") or "" for m in now["messages"]] == rows, (
            "the OLD generation changed while the new one was being staged")
        return False

    app.wait_until(_reseeded, timeout=25, what="the reseed to complete")
    after = _messages(app)
    assert after["generation"] == generation + 1
    assert after["stale"] is None
    assert _texts(app)[:4] == ["say pong", "", "", "pong"], (
        "the reseed rebuilt the transcript")
    feed.working("carrying on")


# ---------------------------------------------------------------------------
# (13) cancel, and a refusal
# ---------------------------------------------------------------------------


def test_cancel_posts_and_a_refusal_surfaces_as_not_accepting(app, gx, feed):
    """**Cancel may be refused, and the refusal is not swallowed** (§3.1 #8).

    gx answers `409 not_accepting` to a cancel on a session that is not working.
    Mapping that to `Ok(())` would make "the session moved on" indistinguishable
    from success, so it surfaces — and clients gate the affordance on `Working`
    instead, which is what the panel's `can_cancel` reports.
    """
    _ready(app)
    feed.working("a long turn")
    app.wait_until(lambda: _messages(app)["activity"] == "working", timeout=15,
                   what="a working turn to cancel")

    gx.clear_requests()
    app.call("lane.cancel", {"machine": MACHINE, "session_id": GX_SESSION})
    posts = gx.requests_to("/cancel")
    assert len(posts) == 1 and posts[0].method == "POST", [p.path for p in posts]
    assert posts[0].path == f"/v1/sessions/{GX_SESSION}/cancel"

    panel = _panel(app)
    assert panel["can_cancel"] is True, "Cancel is enabled while working"
    _unmount(app)
    _ready(app)

    gx.fail("/cancel", 409, "not_accepting", "the session is not accepting that")
    try:
        refusal = _error(
            lambda: app.call("lane.cancel", {"machine": MACHINE,
                                             "session_id": GX_SESSION}))
        assert refusal.code == "not_accepting", refusal
    finally:
        gx.clear_failures()


# ---------------------------------------------------------------------------
# (14) a leader restart: rediscovery, and a token that goes nowhere
# ---------------------------------------------------------------------------


def test_a_leader_restart_rediscovers_and_the_token_never_appears_anywhere(app, gx, gx_home, feed):
    """**A restarted leader keeps its token and changes its instance** (§3.3),
    and the transcript comes back off the persisted history.

    That asymmetry is the whole reason the pin exists: the token is per
    `$GROK_HOME` and survives, the `instanceId` does not — so a client that
    trusted the token alone would happily talk to a different leader on the same
    port. Here the record is rewritten, the ring is empty (a restart loses it),
    and the resume therefore falls through to gx's fourth replay rule: the
    persisted transcript.

    The second half is the audit. The token is a sentinel precisely so it can be
    grepped for, and it appears in **nothing the app hands out**: not the IPC
    responses, not the rendered panel, not the app's own log.
    """
    before = len(_settled(app)["messages"])

    # Staged into the stores first — see cell 11. After `restart_leader` the ring
    # is EMPTY, so the resume cursor falls past it and gx's fourth replay rule
    # serves these off the persisted transcript, which is the path a restart is
    # the only way to reach.
    feed.chunk("agent_message_chunk", "after the restart", "p10", live=False)
    feed.turn_completed("end_turn", live=False)

    gx.restart_leader("beefbeefbeefbeefbeefbeefbeefbeef")
    gx.write_home(gx_home)
    gx.close_streams()

    app.wait_until(lambda: "after the restart" in _texts(app), timeout=30,
                   what="the lane to rediscover the restarted leader")
    assert len(_messages(app)["messages"]) >= before + 1
    assert _messages(app)["stale"] is None
    assert all(r.bearer_ok for r in gx.bearer_requests()), (
        "the token is per-$GROK_HOME and must not have changed")

    # The audit. Everything the app hands a caller, plus what it wrote down.
    panel = _panel(app)
    transcript = json.dumps([
        _rows(app), _open(app), _messages(app), _approvals(app), panel,
        app.call("lane.dump"),
    ])
    _unmount(app)
    log = Path(app.app_log).read_bytes().decode("utf-8", "replace")
    assert SENTINEL_TOKEN not in transcript, "the token reached an IPC response"
    assert SENTINEL_TOKEN not in log, "the token reached the app log"

    # **And the two greps above are not vacuous.** A search for a string that
    # was never anywhere in the first place passes trivially, which in the one
    # cell whose whole job is proving a credential never leaks would be the
    # worst possible false green. So: the sentinel really IS the credential in
    # play — it is the byte string sitting in the fixture home the app read it
    # out of, and every bearer request above was accepted with it.
    on_disk = (Path(gx_home) / "gx-remote.token").read_text()
    assert SENTINEL_TOKEN in on_disk, (
        "the fixture home does not hold the sentinel, so the greps above "
        "searched for a string nothing was ever going to contain")
    assert gx.bearer_requests(), "no bearer request was made with it either"


# ---------------------------------------------------------------------------
# (15) a dead leader: the ladder, and the recovery
# ---------------------------------------------------------------------------


def test_a_dead_leader_walks_the_ladder_to_down_and_recovers_when_it_returns(app, gx, feed):
    """**A dead leader ends in `Down`, with the last good generation still on
    screen** (§3.4).

    The ladder is three silent resumes, then a `cursor_lost` reseed, then — once
    reseeds have been failing for `down_after` — `Down`. What a caller sees is
    `stale` going non-null while the transcript stays exactly where it was: the
    contract's "keep rendering, say so, don't blank" posture, which is the right
    one for a machine that went to sleep.

    Then the leader comes back on the same port and the lane recovers by itself,
    with no `lane.open`, no click and no relaunch.

    The panel is mounted for the WHOLE cell rather than opened at the interesting
    moment. Not tidiness: a panel unmount closes the lane, and closing it would
    reset the generation counter this ends by comparing — as well as hiding the
    thing worth seeing, which is one panel living through a leader dying and
    coming back.
    """
    view = _settled(app)
    rows_before = _texts(app)
    assert view["stale"] is None
    panel = _panel(app)
    assert panel["stale"] is None and [r["text"] for r in panel["rows"]] == rows_before

    gx.stop_listening()
    try:
        app.wait_until(lambda: _messages(app)["stale"], timeout=60,
                       what="the ladder to reach Down")
        down = _messages(app)
        assert down["stale"], down
        assert [m.get("text") or "" for m in down["messages"]] == rows_before, (
            "a Down must keep the last good generation, not blank it")

        app.wait_until(lambda: (_dump(app) or {}).get("stale"), timeout=30,
                       what="the panel's stale banner")
        assert [r["text"] for r in _dump(app)["rows"]] == rows_before
    finally:
        gx.start_listening()

    feed.chunk("agent_message_chunk", "the leader is back", "p11")
    feed.turn_completed("end_turn")
    app.wait_until(lambda: _messages(app)["stale"] is None
                   and "the leader is back" in _texts(app),
                   timeout=60, what="the lane to recover by itself")
    assert _messages(app)["generation"] > view["generation"], (
        "recovery from Down is a reseed, and a reseed is a new generation")
    # WAITED on, not asserted. The wait above is on the BACKEND's view; the
    # banner is the PANEL's, and the panel re-reads on its own frame/40 ms
    # timer — so there is a window where `lane.messages` is live again and the
    # last report still carries the stale reason. Asserting instantaneously
    # across those two clocks is a flake, and was one: it failed roughly one run
    # in three with `activity: "working"` already in the dump, i.e. the panel
    # had picked the recovery up in every respect except having re-reported yet.
    app.wait_until(lambda: (_dump(app) or {}).get("stale") is None, timeout=30,
                   what="the panel's stale banner to clear with the lane")
    _unmount(app)


# ---------------------------------------------------------------------------
# (16) launching gx
# ---------------------------------------------------------------------------


def test_gx_is_offered_by_the_launch_picker_and_opens_with_its_own_argv(app, roost):
    """**`gx` is a creatable kind, and starting one is a `tab.open` of `["gx"]`**
    (§3.2).

    The picker's offer is capability-derived — `offeredKinds` keeps a creatable
    kind whose backing agent the machine reports installed — so the assertion is
    made against exactly what the form reads, rather than against the form's
    rendering of it. `grok` is offered on the same terms: it is creatable and
    lane-less, which is a deliberate pair, not an oversight.

    Last in the file because it opens a tab on the shared fake roost.
    """
    caps = app.machine_capabilities(MACHINE)
    assert "gx" in caps["kinds"] and "grok" in caps["kinds"], caps["kinds"]
    assert caps["agents"]["gx"]["installed"] is True, caps["agents"]
    assert caps["agents"]["grok"]["installed"] is True, caps["agents"]

    before = len(roost.opens)
    row = app.machine_launch(MACHINE, kind="gx", workdir=DIRECTORY)
    assert len(roost.opens) == before + 1, "the launch never reached the session"
    assert roost.opens[-1]["argv"] == ["gx"], roost.opens[-1]
    assert roost.opens[-1]["cwd"] == DIRECTORY
    assert row["machine"] == MACHINE

    # **And the answered row is `grok`, not `gx`** — which is right, and is the
    # promotion rule seen from the other end. `tab.open` answers before any
    # adapter has claimed the tab, so roost reports `source: "grok"` and there is
    # no `gx.remote` yet; the row becomes `gx` the moment the lane binds and
    # roost forwards the URL. A launch that answered `gx` would be deriving a
    # capability from an intention, which is exactly what §3.2 forbids: the kind
    # is a hint about a lane that exists, not a promise about one that might.
    assert row["kind"] == "grok", row
    assert row.get("agent_lane") is None, "a tab with no lane yet has no stamp"
