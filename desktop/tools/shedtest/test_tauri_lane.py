"""The opencode **agent lane** in the Tauri app — the `lane.*` ops and the
transcript panel over them (plan 015 §3.4, C5 + C6). `--target tauri`.

**Hermetic, and every layer under test is the shipped one.** Two fakes and no
network beyond loopback:

* `fake_roost.FakeRoost` on a Unix socket, reached through the test-mode
  `SHED_TAURI_ROOST_SOCKETS` seam, serving a tab whose ownership metadata carries
  `server_url` — roost's own plugin's stamp (roost R10, §3.3). That mapping is
  also what makes the machine `ReachKind::Local`, so the lane dials the reported
  URL directly and no `ssh -N -L` is ever spawned.
* `fake_opencode.FakeOpencode` on a loopback port, answering opencode's v1 HTTP
  API with its **pin guard** armed: any POST to a global route, or to a session
  other than the pinned one, is a recorded violation and answers 500. Several
  cells assert `violations == []`, which is the real claim — a panel pinned to
  one session can never mutate another.

Everything between them is production code: the real `RoostWatcher`, the real
`machine_row` stamp, the real `shed_opencode` client/fold/ring/watcher, the real
`lane.rs` staging. Nothing here injects a rendered row.

**Two halves, one file.** The `lane.*` ops are the backend cut (C5); the
transcript PANEL over them is C6, and the cells that carry both say both — a
`lane.messages` assertion is what the backend staged, the `lane.dump` beside it
is what a person is actually looking at, and those are different claims. The
panel is mounted the only way a caller can mount one (`ui.show_lane`, the
show-create/show-launch pattern — a card's Transcript affordance is a click, and
the harness has none) and closed again inside the same cell, so no cell inherits
someone else's open panel.

**Every cell opens its own lane** (`_ready`, which is `lane.open` plus a wait for
the seed — `lane.open` is idempotent precisely so a caller never has to know
whether one is already open). What order still matters for is the FAKES: the app
instance and both of them are module-scoped, so the cells that retire a lane by
moving its `server_url` or taking its tab away come last, after everything that
needs one to still be there. Read them top to bottom.
"""

from __future__ import annotations

import json
import os
import platform
import shutil
import subprocess
import tempfile
import threading
from pathlib import Path

import pytest

import ui
from client import ShedError, TauriClient
from fake_host_agent import FakeHostAgent
from fake_opencode import FakeOpencode
from fake_roost import FakeRoost, roost_call

pytestmark = pytest.mark.skipif(
    os.environ.get("SHED_TEST_TARGET", "mac") != "tauri",
    reason="tauri-only: the mac app has no machine layer, and so no lanes",
)

FIXTURES = Path(__file__).resolve().parent / "fixtures"

PNG_MAGIC = b"\x89PNG\r\n\x1a\n"

#: Where the panel screenshots are KEPT, when a runner asks for them
#: (`SHED_LANE_SHOTS=<dir>`). Unset by default on purpose: the render gate runs
#: in a container with nowhere to put them, and every cell asserts the PNG it
#: captured either way — writing the file is archiving, not the assertion.
SHOTS = os.environ.get("SHED_LANE_SHOTS")

MACHINE = "mini3"
#: The workspace every session in this suite lives in. ONE directory on purpose:
#: opencode scopes `/event`, `/session/status`, `/permission` and `/question` by
#: it, so two sessions sharing one is the case where a leak would actually show
#: (the plan-012 cross-contamination bug, made unrepresentable by scoping on the
#: id instead of searching by directory).
DIRECTORY = "/home/shed/oc-work"

#: The tab a lane opens on, and the agent session it reported.
LANE_TAB = 4
LANE_SESSION = "ses_lane_root"
#: An opencode tab that reported NO server — status-only, no lane.
BARE_TAB = 5
BARE_SESSION = "ses_no_server"
#: A plain shell tab: not a session row at all.
SHELL_TAB = 3

#: A second root on the SAME server and directory, with a tab of its own — the
#: cross-contamination control. Two tabs reporting ONE `server_url` is what a
#: shared `opencode serve` looks like, and it is the shape where a leak would
#: actually show: both lanes read the same `/event` connection's vocabulary.
SIBLING_TAB = 6
SIBLING_SESSION = "ses_sibling_root"
#: A sub-agent of the lane's session: its approvals block the same agent, so they
#: surface on the root's panel; its transcript does not.
CHILD_SESSION = "ses_lane_child"


def _ownership(session_id: str, server_url: str | None) -> dict:
    """A roost `Ownership` for an opencode tab, with or without the plugin's
    `server_url` stamp.

    Spelled here rather than reached for inside `fake_roost` because that is what
    the wire carries: `metadata` is roost's open extension channel, validated by
    nobody, and a test that could only produce the keys a helper knew about would
    not be testing the channel.
    """
    return {
        "source": "opencode",
        "session_id": session_id,
        "last_event_at": 1_700_000_100,
        "detail": "session_status",
        "metadata": {"server_url": server_url} if server_url else {},
    }


@pytest.fixture(scope="module")
def oc():
    """The agent server the lane talks to.

    Three sessions in one directory: the root a lane opens on, a sibling root
    (which must never leak into it), and a child of the root (whose approvals
    must). The pin is the root, so every mutation this app makes is checked
    against it.
    """
    fake = FakeOpencode()
    fake.add_session(LANE_SESSION, title="the lane", directory=DIRECTORY)
    fake.add_session(SIBLING_SESSION, title="a neighbour", directory=DIRECTORY)
    fake.add_session(CHILD_SESSION, title="a sub-agent", directory=DIRECTORY,
                     parent=LANE_SESSION)
    fake.set_simple_transcript(LANE_SESSION, "seeded question", "seeded answer")
    fake.set_simple_transcript(SIBLING_SESSION, "the neighbour asks",
                               "the neighbour answers")
    fake.set_status(LANE_SESSION, "idle")
    fake.pin = LANE_SESSION
    try:
        yield fake
    finally:
        fake.stop()


@pytest.fixture(scope="module")
def spare_oc():
    """A SECOND agent server on its own port — the restarted tab.

    A tab that is closed and reopened runs a new opencode process, which binds a
    new ephemeral port, so its `server_url` changes. That is a different server,
    not a reconnect, and the lane has to treat it as one.
    """
    fake = FakeOpencode()
    fake.add_session(LANE_SESSION, title="the lane, restarted", directory=DIRECTORY)
    fake.set_simple_transcript(LANE_SESSION, "after the restart", "a fresh answer")
    fake.set_status(LANE_SESSION, "idle")
    fake.pin = LANE_SESSION
    try:
        yield fake
    finally:
        fake.stop()


@pytest.fixture(scope="module")
def roost(oc):
    """mini3's session: two opencode tabs that reported a server (the lane's and
    its neighbour's), one that did not, and somebody's terminal."""
    fake = FakeRoost().start()
    fake.add_tab(LANE_TAB, cwd=DIRECTORY, title="oc | the lane", source="opencode",
                 session_id=LANE_SESSION, lifecycle="working", detail="session_status",
                 shell_state="unknown", metadata={"server_url": oc.base_url})
    fake.add_tab(SIBLING_TAB, cwd=DIRECTORY, title="oc | the neighbour",
                 source="opencode", session_id=SIBLING_SESSION, lifecycle="working",
                 detail="session_status", shell_state="unknown",
                 metadata={"server_url": oc.base_url})
    fake.add_tab(BARE_TAB, cwd=DIRECTORY, title="oc | status only", source="opencode",
                 session_id=BARE_SESSION, lifecycle="working", detail="session_status",
                 shell_state="unknown")
    fake.add_tab(SHELL_TAB, cwd="/home/shed", title="shed@mini3: ~")
    try:
        yield fake
    finally:
        fake.shutdown()


@pytest.fixture(scope="module")
def app(roost, mock):
    """A SECOND, INDEPENDENT app instance pointed at the fake session.

    Its own instance because the `machines:` config and the socket map are read
    at LAUNCH, exactly as in production — the shared session app is launched from
    `conftest.py` with neither.

    **Independent rather than self-managed**, which is the interesting choice.
    `test_tauri_machines.py` takes over `ui._state` (`ui.quit` + `ui.launch`), and
    that leaves the session app DEAD at teardown: the autouse `_reset_policy` /
    `_reset_mock` fixtures dial `ui.socket_path("tauri")` before every test, so
    the next module that rides the session app dies at setup unless it also
    relaunches. That works there because it is collected last. It would make this
    module's position in the alphabet load-bearing, so this follows
    `test_tauri_downhost.py` instead: its own throwaway HOME / XDG_RUNTIME_DIR
    (hence its own socket and single-instance lock), `ui._state` untouched, the
    session app left running. Ordering stops mattering.

    Its own fake host-agent for the same reason downhost has one — the session
    `fake` tracks a single connection, and a second app connecting to it would
    misroute the frames the approval suites are waiting on.

    Short `mkdtemp` prefixes: the IPC socket lives under the runtime dir, and a
    Unix socket path must stay inside SUN_LEN.
    """
    cfg = ui._SUBPROC["tauri"]
    if not cfg.binary.exists():
        raise RuntimeError(
            f"tauri binary not found at {cfg.binary}; build it first (make tauri-build).")
    agent = FakeHostAgent()
    agent.start()
    runtime_dir = Path(tempfile.mkdtemp(prefix="shed-lane-"))
    sock = runtime_dir / cfg.sock_rel
    log = runtime_dir / "lane-ui.log"
    env = ui.subproc_env(
        cfg,
        runtime_dir=runtime_dir,
        mock_base_url=mock.base_url,
        config_path=FIXTURES / "config-lane.yaml",
        host_agent_socket=agent.socket_path,
        roost_sockets={MACHINE: roost.socket_path},
    )
    log_fh = open(log, "wb")
    proc = subprocess.Popen([str(cfg.binary)], env=env, stdout=log_fh,
                            stderr=subprocess.STDOUT)
    client = None
    try:
        ui.await_hermetic("tauri", sock=sock, mock_base_url=mock.base_url,
                          proc=proc, log=log)
        client = TauriClient(sock)
        client.wait_until(
            lambda: client.current_pane() is not None,
            timeout=30,
            what="tauri frontend ready",
        )
        client.wait_until(
            lambda: _row(client, LANE_SESSION),
            timeout=20,
            what="the machine's rows",
        )
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
    """The machine row whose `agent_lane` names `session_id`, or `None`."""
    for row in _rows(app):
        if (row.get("agent_lane") or {}).get("session_id") == session_id:
            return row
    return None


def _open(app: TauriClient, session_id: str = LANE_SESSION) -> dict:
    return app.call("lane.open", {"machine": MACHINE, "session_id": session_id})


def _ready(app: TauriClient, session_id: str = LANE_SESSION) -> dict:
    """Open the lane (idempotent) and wait for its FIRST generation to be whole.

    Every cell calls this rather than relying on an earlier one having left a
    lane open: a suite whose cells only pass in file order hides which one
    actually establishes what, and `lane.open` is idempotent precisely so a
    caller never has to know.

    The wait is on the seed reaching `Ready` — `generation` moves off 0 when the
    first `Reset` lands and the view swaps at `Ready` — because `lane.open`
    returns as soon as the subscription is STARTED, which is what lets a panel
    render its frame before the transcript arrives.
    """
    _open(app, session_id)
    app.wait_until(
        lambda: _messages(app, session_id)["generation"] >= 1
        and _messages(app, session_id)["messages"],
        timeout=20,
        what=f"the seeded lane on {session_id}",
    )
    return _messages(app, session_id)


def _messages(app: TauriClient, session_id: str = LANE_SESSION) -> dict:
    return app.call("lane.messages", {"machine": MACHINE, "session_id": session_id})


def _texts(app: TauriClient, session_id: str = LANE_SESSION) -> list[str]:
    return [m.get("text") or "" for m in _messages(app, session_id)["messages"]]


def _approvals(app: TauriClient, session_id: str = LANE_SESSION) -> list[dict]:
    return app.call("lane.approvals",
                    {"machine": MACHINE, "session_id": session_id})["approvals"]


def _close(app: TauriClient, session_id: str = LANE_SESSION) -> None:
    app.call("lane.close", {"machine": MACHINE, "session_id": session_id})


def _error(fn) -> ShedError:
    with pytest.raises(ShedError) as caught:
        fn()
    return caught.value


# ---------------------------------------------------------------------------
# the panel (C6): mounting it, reading what it rendered, screenshotting it
# ---------------------------------------------------------------------------


def _dump(app: TauriClient) -> dict | None:
    """What the transcript PANEL rendered, or `None` when none is mounted.

    Deliberately a different question from `lane.messages`: that is the backend's
    staged view and answers whether or not anything is on screen. This is the
    screen.
    """
    return app.call("lane.dump")["lane"]


def _mounted(app: TauriClient, session_id: str) -> bool:
    """Is the panel mounted on this session AND past its first read?

    A panel reports from its FIRST render — before `lane.open` has answered —
    on purpose: `lane.dump` going non-null is how a caller learns a panel
    exists, and a panel that reports only once it succeeded would be invisible
    in exactly the case worth seeing (an open that failed). So "mounted" is not
    the same question as "showing something", and this asks the second: a
    generation it can render, or an error saying why it cannot.
    """
    d = _dump(app)
    return bool(d and d["session_id"] == session_id
                and (d["generation"] >= 1 or d["error"]))


def _panel(app: TauriClient, session_id: str = LANE_SESSION) -> dict:
    """Mount the transcript panel on a session and wait for its first read.

    `ui.show_lane` is the drivable half of the card's Transcript affordance —
    the panel itself calls `lane.open` on mount and `lane.close` on unmount, so
    this is a UI action, not a second door into the lane layer.
    """
    app.call("ui.show_lane", {"machine": MACHINE, "session_id": session_id})
    app.wait_until(lambda: _mounted(app, session_id), timeout=20,
                   what="the transcript panel to mount and report")
    return _dump(app)


def _panel_approvals(app: TauriClient) -> list[dict]:
    """The approval CARDS on screen — not `lane.approvals`, which is what the
    backend holds. A card carries what was rendered for it: the decision buttons
    by label, and a question's option buttons + free-text flag."""
    return (_dump(app) or {}).get("approvals", [])


def _unmount(app: TauriClient) -> None:
    """Close the panel and prove `lane.dump` goes back to `null`.

    Every cell that mounts one ends here, for two reasons: the module's app and
    fakes are shared, so an inherited panel would hold a lane open behind a cell
    that thinks it closed one — and "null once it unmounts" is itself the pinned
    behavior (§3.4), which is only worth anything if a panel was mounted first.
    """
    app.call("ui.close_lane")
    app.wait_until(lambda: _dump(app) is None, timeout=20,
                   what="the panel to unmount and clear its report")


def _shot(app: TauriClient, name: str) -> None:
    """Capture the window and keep it under `$SHED_LANE_SHOTS` when set.

    macOS screenshots are Screen-Recording-TCC-gated (the tauri app can run
    there too), so the capture is skipped rather than failed — Linux/Xvfb is
    where this is the gate, and where the plan's artifacts are recorded.
    """
    if platform.system() == "Darwin":
        return
    png, w, h = app.screenshot(scale=1)
    assert png[:8] == PNG_MAGIC and w > 0 and h > 0
    if SHOTS:
        out = Path(SHOTS).expanduser()
        out.mkdir(parents=True, exist_ok=True)
        (out / name).write_bytes(png)


# ---------------------------------------------------------------------------
# (1) the capability signal
# ---------------------------------------------------------------------------


def test_only_a_tab_that_reported_a_server_carries_a_lane(app, oc):
    """**`agent_lane`'s presence IS the capability signal** (§3.4).

    A tab whose adapter reported `server_url` gets the stamp and can be opened;
    an opencode tab that reported none is status-only and answers `no_lane`, as
    is a plain shell (which is not a session row at all). Nothing about the rest
    of the row changes — the stamp is additive, beside the DTO every card already
    renders.
    """
    row = _row(app, LANE_SESSION)
    assert row is not None, _rows(app)
    assert row["agent_lane"] == {
        "kind": "opencode",
        "session_id": LANE_SESSION,
        "server_url": oc.base_url,
    }
    # The key is `agent_lane`, NOT `lane` — `lane` is the RC hub's lane token on
    # this same DTO and must keep meaning what it meant.
    assert row.get("lane") is None
    assert row["machine"] == MACHINE
    assert row["origin"] == f"machine:{MACHINE}"

    # The status-only tab: a row, a kind, no lane.
    bare = next(r for r in _rows(app) if r["slug"] == str(BARE_TAB))
    assert "agent_lane" not in bare, bare
    assert bare["kind"] == "opencode", "still an opencode card, just no transcript"
    # …and the shell tab never became a row at all.
    assert all(r["slug"] != str(SHELL_TAB) for r in _rows(app))

    opened = _open(app)
    assert opened["session"]["id"] == LANE_SESSION
    assert opened["session"]["cwd"] == DIRECTORY
    assert opened["capabilities"] == {
        "kind": "opencode",
        "interject": False,
        "create": True,
        "cancel": True,
        "approvals": True,
        "history_cursor": False,
    }

    refused = _error(lambda: _open(app, BARE_SESSION))
    assert refused.code == "no_lane", refused
    assert BARE_SESSION in refused.message


# ---------------------------------------------------------------------------
# (2) the seed
# ---------------------------------------------------------------------------


def test_the_history_seeds_into_the_lane_view(app):
    """The REST seed reaches `lane.messages` as ordered, seq'd feed rows — and
    the PANEL renders those same rows.

    Both halves, because they are different claims. `lane.messages` is what the
    backend staged; `lane.dump` is what a person is looking at, and a panel that
    opened its lane and then rendered nothing would satisfy the first and fail
    the second.
    """
    # The Agents pane first, so the card carrying the Transcript affordance has
    # long finished painting by the time the panel opens beside it — the artifact
    # this cell writes is meant to show both.
    app.navigate("agents")
    _ready(app)
    texts = _texts(app)
    assert texts[:2] == ["seeded question", "seeded answer"]

    view = _messages(app)
    seqs = [m["seq"] for m in view["messages"]]
    assert seqs == sorted(seqs) and len(set(seqs)) == len(seqs), seqs
    assert view["messages"][0]["role"] == "user"
    assert view["messages"][0]["type"] == "text"
    assert view["stale"] is None
    assert view["generation"] >= 1
    # `needs_input`, not `idle`: the lane's fold NEVER says idle — that verdict
    # came from the hub also watching a tmux pane for stability, and a lane has
    # no pane. A session that reached an idle boundary with nothing pending is
    # waiting on the human.
    assert view["activity"] == "needs_input"

    # --- the panel over it (C6) ---
    assert _dump(app) is None, "no panel until one is opened"
    panel = _panel(app)
    assert panel["machine"] == MACHINE
    assert panel["title"] == "the lane", "the header names the SESSION, not the tab"
    # The rendered row, field by field — the treatments the panel applies (a
    # `status` row muted, reasoning collapsed) are part of the row, not of the
    # wire, so this is where they are pinned.
    assert [{k: r[k] for k in ("role", "type", "text", "tool", "muted", "collapsed")}
            for r in panel["rows"][:2]] == [
        {"role": "user", "type": "text", "text": "seeded question",
         "tool": None, "muted": False, "collapsed": False},
        {"role": "assistant", "type": "text", "text": "seeded answer",
         "tool": None, "muted": False, "collapsed": False},
    ], panel["rows"][:2]
    # …and the SAME rows, in the same order, as the view it renders — the panel
    # re-reads the staged view rather than folding frames of its own, which is
    # what makes those two able to disagree impossible.
    assert [r["seq"] for r in panel["rows"]] == [m["seq"] for m in _messages(app)["messages"]]
    assert panel["activity"] == "needs_input", "the badge says what the view says"
    assert panel["stale"] is None, "no stale banner on a live lane"
    assert panel["generation"] >= view["generation"]
    assert panel["approvals"] == [], "nothing is blocking on the human"
    assert panel["can_cancel"] is False, "Cancel is enabled only while Working"
    assert panel["error"] is None

    _shot(app, "tauri-lane-transcript.png")
    _unmount(app)


# ---------------------------------------------------------------------------
# (3) the live stream
# ---------------------------------------------------------------------------


def test_a_streamed_part_appears_without_any_poll(app, oc):
    """A `message.part.updated` folded live lands in the view.

    There is no cadence anywhere on this path — the frame is pushed on the SSE
    connection the watcher already holds — so this is a latency assertion, not a
    "wait one interval" one.
    """
    _ready(app)
    oc.stream_part(LANE_SESSION, message_id="msg_live", part_id="prt_live",
                   text="streamed live")
    app.wait_until(lambda: "streamed live" in _texts(app), timeout=5,
                   what="the streamed row")
    assert _texts(app)[-1] == "streamed live", "it landed at the end of the feed"


# ---------------------------------------------------------------------------
# (4) send
# ---------------------------------------------------------------------------


def test_send_posts_prompt_async_to_the_pinned_session_only(app, oc):
    """`lane.send` is `POST /session/{id}/prompt_async` on the pinned session,
    and the pin guard records no violation.

    The guard is the point: a panel pinned to one session must never mutate
    another, and a fake that answered 200 to anything would prove nothing.
    """
    _ready(app)
    app.call("lane.send", {"machine": MACHINE, "session_id": LANE_SESSION,
                           "text": "do the thing"})
    path = f"/session/{LANE_SESSION}/prompt_async"
    assert path in oc.post_paths, oc.post_paths
    assert json.loads(oc.post_body("/prompt_async")) == {
        "parts": [{"type": "text", "text": "do the thing"}]
    }
    assert oc.violations == []

    # `interject` is advertised false, so it is REFUSED rather than quietly
    # downgraded into a queue.
    refused = _error(lambda: app.call(
        "lane.send", {"machine": MACHINE, "session_id": LANE_SESSION,
                      "text": "now", "mode": "interject"}))
    assert refused.code == "not_accepting", refused
    assert oc.violations == []


# ---------------------------------------------------------------------------
# (5) permissions
# ---------------------------------------------------------------------------


def test_a_permission_ask_surfaces_and_is_answered_on_its_own_route(app, oc):
    """`permission.asked` → a pending approval; `lane.answer` → `POST
    /permission/{requestID}/reply {"reply":"once"}`.

    opencode's answer route is id-addressed and GLOBAL — the session is not on
    the wire at all — so the fake resolves the request id back to the session it
    issued it for and refuses one outside the pinned scope. That is what makes
    "answered the right thing" an assertion rather than an assumption.
    """
    _ready(app)
    ask = "per_bash_1"
    oc.stream_permission_asked(LANE_SESSION, ask, command="rm -rf /tmp/x")
    app.wait_until(lambda: any(a["id"] == ask for a in _approvals(app)),
                   timeout=5, what="the permission to surface")

    approval = next(a for a in _approvals(app) if a["id"] == ask)
    assert approval["kind"] == "permission"
    assert approval["status"] == "pending"
    assert approval["session_id"] == LANE_SESSION
    assert "rm -rf /tmp/x" in json.dumps(approval), approval
    # The count the card badges off is the session row's, not the list's length.
    app.wait_until(lambda: _messages(app)["activity"] == "needs_approval",
                   timeout=5, what="the blocked verdict")

    # --- and ON SCREEN (C6): a card with the three fixed decisions ---
    panel = _panel(app)
    app.wait_until(lambda: any(c["id"] == ask for c in _panel_approvals(app)),
                   timeout=10, what="the permission to reach the panel")
    card = next(c for c in _panel_approvals(app) if c["id"] == ask)
    assert card["kind"] == "permission"
    assert card["session_id"] == LANE_SESSION
    # The three fixed choices, in order. Branched off `kind` — a permission
    # leaves `questions` empty, and a panel that keyed off "whichever list is
    # non-empty" would render an approval with no buttons at all (§11.4).
    assert card["buttons"] == ["Allow once", "Always", "Reject"]
    assert card["questions"] == []
    assert "rm -rf /tmp/x" in card["title"] + card["detail"], card
    assert panel["error"] is None
    _shot(app, "tauri-lane-approval.png")

    app.call("lane.answer", {"machine": MACHINE, "session_id": LANE_SESSION,
                             "approval_id": ask,
                             "answer": {"permission": "allow-once"}})
    assert f"/permission/{ask}/reply" in oc.post_paths, oc.post_paths
    assert json.loads(oc.post_body(f"/permission/{ask}/reply")) == {"reply": "once"}
    assert oc.violations == []

    # A second answer is the double-tap gate, not a second POST.
    again = _error(lambda: app.call(
        "lane.answer", {"machine": MACHINE, "session_id": LANE_SESSION,
                        "approval_id": ask, "answer": {"permission": "reject"}}))
    assert again.code == "already_resolved", again
    assert oc.post_paths.count(f"/permission/{ask}/reply") == 1

    # The agent confirms; the ask retires — from the view AND from the panel,
    # which follows the stream rather than its own optimistic guess about what
    # answering did.
    oc.stream_permission_replied(LANE_SESSION, ask, "once")
    app.wait_until(lambda: all(a["id"] != ask for a in _approvals(app)),
                   timeout=5, what="the permission to retire")
    app.wait_until(lambda: all(c["id"] != ask for c in _panel_approvals(app)),
                   timeout=10, what="the card to leave the panel")
    _unmount(app)


# ---------------------------------------------------------------------------
# (6) questions
# ---------------------------------------------------------------------------


def test_a_question_surfaces_with_its_options_and_answers_on_the_question_route(app, oc):
    """`question.asked` → an approval carrying the option list; answering it is
    `POST /question/{requestID}/reply {"answers": [[…]]}`.

    A question's options carry no id of their own on opencode's wire, so the
    contract sets `id = label` — which is also exactly what the reply route wants
    back. Asserting the POSTed body is what pins that.
    """
    _ready(app)
    ask = "que_pick_1"
    # `custom=False` OUT LOUD: an omitted flag is opencode's documented default
    # of TRUE (see the free-text cell below), and this cell is the one-click
    # shape — one question, one choice, NO free text.
    oc.stream_question_asked(LANE_SESSION, ask, header="Which branch?",
                             question="Pick a branch to work on",
                             options=["main", "develop"], custom=False)
    app.wait_until(lambda: any(a["id"] == ask for a in _approvals(app)),
                   timeout=5, what="the question to surface")

    approval = next(a for a in _approvals(app) if a["id"] == ask)
    assert approval["kind"] == "question"
    assert approval["status"] == "pending"
    assert len(approval["questions"]) == 1
    options = approval["questions"][0]["options"]
    assert [o["id"] for o in options] == ["main", "develop"]
    assert [o["label"] for o in options] == ["main", "develop"]

    # --- and ON SCREEN (C6): the option buttons, off `kind` again ---
    panel = _panel(app)
    app.wait_until(lambda: any(c["id"] == ask for c in _panel_approvals(app)),
                   timeout=10, what="the question to reach the panel")
    card = next(c for c in _panel_approvals(app) if c["id"] == ask)
    assert card["kind"] == "question"
    assert card["questions"] == [{
        "header": "Which branch?",
        "question": "Pick a branch to work on",
        "options": ["main", "develop"],
        "custom": False,
    }]
    # A question fills `questions` and leaves the permission trio empty — the
    # mirror image of the permission card, and the reason branching on "which
    # list is non-empty" would render one of the two with nothing to click.
    assert card["buttons"] == [], "one question, one choice, no free text: a click IS the answer"
    _shot(app, "tauri-lane-question.png")

    app.call("lane.answer", {"machine": MACHINE, "session_id": LANE_SESSION,
                             "approval_id": ask,
                             "answer": {"question": [["develop"]]}})
    assert json.loads(oc.post_body(f"/question/{ask}/reply")) == {
        "answers": [["develop"]]
    }
    assert oc.violations == []

    oc.stream({"type": "question.replied",
               "properties": {"sessionID": LANE_SESSION, "requestID": ask}})
    app.wait_until(lambda: all(a["id"] != ask for a in _approvals(app)),
                   timeout=5, what="the question to retire")

    # A `custom` question is the other shape: free text is accepted, so a click
    # is no longer the whole answer and the card stages a selection behind an
    # explicit submit. The ask here OMITS the flag, which is opencode's ORDINARY
    # wire shape — its schema documents `custom` as "Allow typing a custom
    # answer (default: true)" and its TUI draws the freeform row when the ask
    # says nothing. Reading an omitted flag as `false` (what the fold did before
    # plan 018 §3.2) hid the text box on the common case.
    free = "que_free_1"
    oc.stream_question_asked(LANE_SESSION, free, header="Anything else?",
                             question="Name the branch", options=["main"])
    app.wait_until(lambda: any(c["id"] == free for c in _panel_approvals(app)),
                   timeout=10, what="the custom question to reach the panel")
    typed = next(c for c in _panel_approvals(app) if c["id"] == free)
    assert typed["questions"][0]["custom"] is True, typed
    assert typed["buttons"] == ["Send answer"], "a staged answer needs a submit"

    # Free text rides in `custom_text`, positionally — NOT smuggled into the
    # vec-of-vecs. The adapter appends it to that question's answer list because
    # that is opencode's own shape (a custom answer is a label the ask did not
    # offer), so the wire is unchanged while the contract is now able to say
    # which entry the human typed.
    app.call("lane.answer", {"machine": MACHINE, "session_id": LANE_SESSION,
                             "approval_id": free,
                             "answer": {"question": [["main"]],
                                        "custom_text": ["  a-branch-i-typed  "]}})
    assert json.loads(oc.post_body(f"/question/{free}/reply")) == {
        "answers": [["main", "a-branch-i-typed"]]
    }, "the typed text is one more entry on that question's list, TRIMMED"
    assert oc.violations == []
    oc.stream({"type": "question.replied",
               "properties": {"sessionID": LANE_SESSION, "requestID": free}})
    app.wait_until(lambda: all(c["id"] != free for c in _panel_approvals(app)),
                   timeout=10, what="the custom question to retire")

    # …and a question that says `custom: false` REFUSES free text, before
    # anything reaches the wire. The no-traffic half is the point: a refusal
    # that posted first would have filed the typed string as a label the ask
    # never offered.
    strict = "que_strict_1"
    oc.stream_question_asked(LANE_SESSION, strict, header="Which branch?",
                             question="Pick one", options=["main"], custom=False)
    app.wait_until(lambda: any(c["id"] == strict for c in _panel_approvals(app)),
                   timeout=10, what="the strict question to reach the panel")
    before = len(oc.post_paths)
    err = _error(lambda: app.call("lane.answer", {
        "machine": MACHINE, "session_id": LANE_SESSION, "approval_id": strict,
        "answer": {"question": [[]], "custom_text": ["something I typed"]}}))
    assert err.code == "bad_request", err
    assert oc.post_paths[before:] == [], \
        "the refusal is decided against the resolved approval — nothing was posted"
    assert oc.violations == []

    # The other door: `custom_text` beside a form that does not take it never
    # reaches an adapter at all — the IPC grammar refuses it.
    before = len(oc.post_paths)
    err = _error(lambda: app.call("lane.answer", {
        "machine": MACHINE, "session_id": LANE_SESSION, "approval_id": strict,
        "answer": {"choice": "main", "custom_text": ["typed"]}}))
    assert err.code == "bad_request", err
    assert "custom_text" in err.message and "choice" in err.message, err
    assert oc.post_paths[before:] == [], "refused at the door, no wire traffic"

    # Retire it, like the two asks above. `oc` is module-scoped and
    # `_approvals()` returns everything still pending, so a question this cell
    # left open would be visible to every cell that runs after it — and
    # `_unmount()` only closes the panel, it does not answer anything. The two
    # refusals above are the point of the cell and neither of them retires the
    # ask, so this is the only place it can happen.
    oc.stream({"type": "question.replied",
               "properties": {"sessionID": LANE_SESSION, "requestID": strict}})
    app.wait_until(lambda: all(c["id"] != strict for c in _panel_approvals(app)),
                   timeout=10, what="the strict question to retire")
    _unmount(app)


# ---------------------------------------------------------------------------
# (7) cross-contamination
# ---------------------------------------------------------------------------


def test_a_second_session_on_the_same_server_never_leaks_into_the_first(app, oc):
    """Two roots in ONE directory on ONE server: neither sees the other.

    This is the plan-012 hub bug, made unrepresentable. The hub had to SEARCH for
    its session (directory + timing heuristics) and two sessions in one directory
    adopted each other's conversations; the lane is handed the id by
    construction, so it scopes instead — and the sibling's rows, which arrive on
    the very same `/event` connection, are simply not the root's.
    """
    _ready(app)
    oc.stream_part(SIBLING_SESSION, message_id="msg_sib", part_id="prt_sib",
                   text="the neighbour's secret")
    # Something the ROOT can wait on, so the negative below is not a race with a
    # frame that had not arrived yet: both were pushed on the same connection, in
    # this order, so the root's arrival proves the sibling's was seen and dropped.
    oc.stream_part(LANE_SESSION, message_id="msg_after", part_id="prt_after",
                   text="mine, after the neighbour's")
    app.wait_until(lambda: "mine, after the neighbour's" in _texts(app),
                   timeout=5, what="the root's own later row")
    assert "the neighbour's secret" not in _texts(app)

    # And the sibling, opened on its own, has its own transcript and not the
    # root's.
    _ready(app, SIBLING_SESSION)
    app.wait_until(lambda: "the neighbour's secret" in _texts(app, SIBLING_SESSION),
                   timeout=20, what="the sibling's own rows")
    sibling = _texts(app, SIBLING_SESSION)
    assert "seeded question" not in sibling
    assert "mine, after the neighbour's" not in sibling
    _close(app, SIBLING_SESSION)


# ---------------------------------------------------------------------------
# (12) descendants
# ---------------------------------------------------------------------------


def test_a_child_sessions_approval_surfaces_on_the_roots_panel(app, oc):
    """A sub-agent's approval blocks the SAME agent, so it surfaces on the root's
    panel, attributed to the child — and answering it addresses the child's own
    request id. A sibling ROOT's does not surface at all.

    Both halves matter. Without the first, a root sits `Working` forever while a
    child waits on a human nobody can see. Without the second, one panel would
    answer for a session it is not pinned to.
    """
    _ready(app)
    child_ask = "per_child_1"
    sibling_ask = "per_sibling_1"
    oc.stream_permission_asked(CHILD_SESSION, child_ask, command="git push")
    oc.stream_permission_asked(SIBLING_SESSION, sibling_ask, command="not yours")
    app.wait_until(lambda: any(a["id"] == child_ask for a in _approvals(app)),
                   timeout=5, what="the child's approval")

    approval = next(a for a in _approvals(app) if a["id"] == child_ask)
    assert approval["session_id"] == CHILD_SESSION, "attributed to the child"
    assert all(a["id"] != sibling_ask for a in _approvals(app)), _approvals(app)

    app.call("lane.answer", {"machine": MACHINE, "session_id": LANE_SESSION,
                             "approval_id": child_ask,
                             "answer": {"permission": "allow-always"}})
    assert json.loads(oc.post_body(f"/permission/{child_ask}/reply")) == {
        "reply": "always"
    }
    # In scope, because the fake issued it FOR a descendant of the pin.
    assert oc.violations == []

    oc.stream_permission_replied(CHILD_SESSION, child_ask, "always")
    app.wait_until(lambda: all(a["id"] != child_ask for a in _approvals(app)),
                   timeout=5, what="the child's approval to retire")


# ---------------------------------------------------------------------------
# (9) cancel
# ---------------------------------------------------------------------------


def test_cancel_aborts_the_pinned_session(app, oc):
    """`lane.cancel` is `POST /session/{id}/abort` — on the pin, and nowhere
    else."""
    _ready(app)
    before = oc.post_paths.count(f"/session/{LANE_SESSION}/abort")
    app.call("lane.cancel", {"machine": MACHINE, "session_id": LANE_SESSION})
    assert oc.post_paths.count(f"/session/{LANE_SESSION}/abort") == before + 1
    assert oc.violations == []


# ---------------------------------------------------------------------------
# (8) the reconnect
# ---------------------------------------------------------------------------


def test_a_dropped_stream_reseeds_without_ever_showing_a_partial_view(app, oc):
    """The fake hangs up; the adapter reconnects; the view NEVER goes empty.

    The contract brackets a reconnect with `Reset` … `Ready` and requires the
    client to stage everything between them and swap atomically — this is that
    requirement, asserted from outside. The loop reads `lane.messages` as fast as
    it can while the reseed runs: every sample must still carry the whole
    previous generation. When the swap lands, the rows are back with HIGHER seqs
    (the ring outlives the fold's per-generation reset) and a higher generation.
    """
    before = _ready(app)
    assert before["messages"], "nothing to lose"
    kept = [m.get("text") for m in before["messages"]]
    top_seq = before["messages"][-1]["seq"]
    generation = before["generation"]

    oc.close_streams()

    samples = 0

    def reseeded() -> bool:
        nonlocal samples
        samples += 1
        view = _messages(app)
        # The invariant, checked on EVERY sample, not just at the end.
        assert [m.get("text") for m in view["messages"]][:len(kept)] == kept, (
            "a partial or empty view was visible mid-reseed"
        )
        return view["generation"] > generation

    app.wait_until(reseeded, timeout=30, what="the reseeded generation")
    assert samples > 1, "the reseed finished before a single sample — no proof"

    after = _messages(app)
    assert [m.get("text") for m in after["messages"]] == kept, "the same rows"
    assert after["messages"][0]["seq"] > top_seq, (
        "a reseed must re-mint seqs from the SAME ring, not restart it"
    )
    assert after["stale"] is None, "a completed reseed is not stale"


# ---------------------------------------------------------------------------
# (10) close
# ---------------------------------------------------------------------------


def test_close_ends_the_subscription_and_two_opens_make_one(app, oc):
    """`lane.close` really closes the `/event` socket; two concurrent
    `lane.open`s build ONE subscription; `lane.dump` is `null` with no panel.

    The connection count is the assertion that matters. A `close` that only
    forgot the entry would leave the adapter's pump — and, on a remote machine,
    an `ssh -N` child — running behind a panel that is gone.
    """
    _ready(app)
    _close(app)
    app.wait_until(lambda: oc.stream_count() == 0, timeout=15,
                   what="the /event socket to close")

    # A verb on a closed lane is refused rather than silently re-opening one.
    stale = _error(lambda: _messages(app))
    assert stale.code == "no_lane", stale

    # Two opens at once, on two INDEPENDENT IPC connections (one client is one
    # socket, and calls on it are serialized — which would test nothing).
    results: list = []
    errors: list = []

    def open_once() -> None:
        # `app.path` — this module's OWN instance, not the session app's socket.
        client = TauriClient(app.path)
        try:
            results.append(_open(client))
        except Exception as e:  # noqa: BLE001 - reported below
            errors.append(e)
        finally:
            client.close()

    threads = [threading.Thread(target=open_once) for _ in range(2)]
    for t in threads:
        t.start()
    for t in threads:
        t.join(timeout=60)
    assert not errors, errors
    assert len(results) == 2
    assert results[0]["session"]["id"] == results[1]["session"]["id"]

    app.wait_until(lambda: oc.stream_count() == 1, timeout=15,
                   what="exactly one subscription")
    assert oc.stream_count() == 1, "a second lane.open opened a second stream"

    # And the panel's own lifecycle, which is the same lifecycle: it opens the
    # lane on mount and closes it on unmount, so an unmounted panel leaves NO
    # subscription behind and `lane.dump` answers `null` — the honest answer to
    # "is anyone looking at this", not a stale copy of the last thing rendered.
    assert _dump(app) is None, "no panel is mounted yet"
    _close(app)
    _panel(app)
    app.wait_until(lambda: oc.stream_count() == 1, timeout=15,
                   what="the panel's own subscription")
    _unmount(app)
    app.wait_until(lambda: oc.stream_count() == 0, timeout=15,
                   what="the unmounted panel to take its lane with it")
    assert app.call("lane.dump") == {"lane": None}


# ---------------------------------------------------------------------------
# (11) a restarted tab
# ---------------------------------------------------------------------------


def test_a_new_server_url_retires_the_old_entry_and_redials(app, oc, roost, spare_oc):
    """A tab that restarted reports a NEW `server_url`, and the lane follows it.

    A new ephemeral port is a different server, not a reconnect: the old entry
    addresses a socket nobody is listening on. So it is evicted the moment the
    roost snapshot says the row moved, and the next `lane.open` dials the new
    port — and the OLD server sees its subscription go away, which is what stops
    a restart from leaking one connection (and one `ssh -N` child) per restart.
    """
    _ready(app)
    app.wait_until(lambda: oc.stream_count() >= 1, timeout=15,
                   what="the lane on the old server")

    roost.set_axes(LANE_TAB, ownership=_ownership(LANE_SESSION, spare_oc.base_url))
    app.wait_until(
        lambda: (_row(app, LANE_SESSION) or {}).get("agent_lane", {}).get("server_url")
        == spare_oc.base_url,
        timeout=15,
        what="the row to report the new server",
    )
    # Evicted on the snapshot, without anyone asking.
    app.wait_until(lambda: oc.stream_count() == 0, timeout=15,
                   what="the old subscription to end")
    gone = _error(lambda: _messages(app))
    assert gone.code == "no_lane", gone

    # And re-opening lands on the NEW port, with the new server's transcript.
    _open(app)
    app.wait_until(lambda: "after the restart" in _texts(app), timeout=20,
                   what="the new server's rows")
    assert "seeded question" not in _texts(app), "the old server's rows survived"
    assert spare_oc.stream_count() == 1
    assert spare_oc.violations == []


# ---------------------------------------------------------------------------
# eviction: the tab goes away
# ---------------------------------------------------------------------------


def test_a_tab_that_goes_away_takes_its_lane_with_it(app, roost, spare_oc):
    """**The tab-gone signal is what retires a lane** (§3.4).

    Two shapes, both of which reach the app as a fresh roost snapshot that no
    longer names the session — and both of which must end the subscription,
    because an entry behind a row nobody can see holds an `/event` connection and,
    on a remote machine, an `ssh -N` child that nothing will ever reap:

    1. the agent exits and its adapter RELEASES the tab (roost's wire drops
       `ownership`, so the row leaves the agent-owned inventory), and
    2. the tab itself closes — which, after a release, is a change in the
       inventory's HIDDEN half. Publishing that at all is plan 014's ghost-row
       fix; before it, a tab that died before anything claimed it left a row
       forever and this signal never arrived.
    """
    _ready(app)
    app.wait_until(lambda: spare_oc.stream_count() == 1, timeout=15,
                   what="a live lane to lose")

    # (1) the agent exits: the adapter releases the tab.
    roost.set_axes(LANE_TAB, lifecycle="inactive", ownership=None)
    app.wait_until(lambda: _row(app, LANE_SESSION) is None, timeout=15,
                   what="the released row to leave")
    app.wait_until(lambda: spare_oc.stream_count() == 0, timeout=15,
                   what="the lane to be evicted with its row")
    orphaned = _error(lambda: _messages(app))
    assert orphaned.code == "no_lane", orphaned

    # (2) the now-hidden tab closes. Out of band, the way a dead process's shell
    # does — NOT through `machine.kill`, whose own optimistic drop would hide
    # whether the snapshot signal works.
    roost_call(roost.socket_path, "tab.close", {"tab_id": str(LANE_TAB)})
    app.wait_until(
        lambda: all(r["slug"] != str(LANE_TAB) for r in _rows(app)),
        timeout=15,
        what="the closed tab to leave the rows",
    )
    assert spare_oc.stream_count() == 0, "a closed tab resurrected its lane"
    assert _error(lambda: _open(app)).code == "no_lane"


# ---------------------------------------------------------------------------
# the guard itself
# ---------------------------------------------------------------------------


def test_the_pin_guard_catches_an_unissued_answer_on_either_route():
    """**The guard is non-vacuous** — `shed-opencode`'s
    `the_pin_guard_catches_an_off_pin_mutation`, in Python and one route wider.

    Every cell above asserts `violations == []`, which is only worth anything if
    a wrong mutation would actually be recorded. It would not be, on one route:
    the legacy session-scoped answer form (`/session/{id}/permissions/{rid}`)
    checked its SESSION alone, so with the pin set, answering a request the fake
    had never issued returned `200 true` and recorded nothing.

    Its own fake, and `_serve_mutation` directly: this is about the guard, not
    about anything the app does — and the module's shared fake must not end up
    carrying deliberate violations that another cell then reads.
    """
    fake = FakeOpencode()
    try:
        fake.add_session("ses_root", title="the pin", directory=DIRECTORY)
        fake.add_session("ses_sib", title="a sibling root", directory=DIRECTORY)
        fake.add_permission("ses_root", "per_real")
        fake.add_permission("ses_sib", "per_sibling")
        fake.pin = "ses_root"

        # The legacy route, naming the PINNED session, answering a request id
        # the fake never issued — the bypass.
        assert fake._serve_mutation(
            "/session/ses_root/permissions/per_ghost", '{"response":"once"}',
        )[0] == 500
        # …and one that exists but was issued for a sibling root, which the
        # pin's scope does not cover.
        assert fake._serve_mutation(
            "/session/ses_root/permissions/per_sibling", '{"response":"once"}',
        )[0] == 500
        # The global answer route, which always had the check.
        assert fake._serve_mutation(
            "/permission/per_ghost/reply", '{"reply":"once"}',
        )[0] == 500
        # And the original grammar: a mutation addressing another session.
        assert fake._serve_mutation("/session/ses_sib/abort", "")[0] == 500

        assert len(fake.violations) == 4, fake.violations
        assert "per_ghost" in fake.violations[0], fake.violations
        assert "per_sibling" in fake.violations[1], fake.violations
        assert "per_ghost" in fake.violations[2], fake.violations
        assert "ses_sib" in fake.violations[3], fake.violations

        # Non-vacuous the OTHER way too: the answers a pinned panel is entitled
        # to make still succeed, on both shapes, and record nothing.
        fake.violations.clear()
        assert fake._serve_mutation(
            "/session/ses_root/permissions/per_real", '{"response":"once"}',
        ) == (200, "true")
        assert fake._serve_mutation(
            "/permission/per_real/reply", '{"reply":"once"}',
        ) == (200, "true")
        assert fake._serve_mutation("/session/ses_root/abort", "") == (200, "true")
        assert fake.violations == []
    finally:
        fake.stop()
