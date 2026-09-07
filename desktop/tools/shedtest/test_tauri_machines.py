"""Machine targets in the Tauri app — read from each machine's `roost-session`
(plan 013 S3, the Roost Pivot's first milestone; originally plan 012 S4).
`--target tauri`.

**Hermetic, and genuinely exercising the real code.** A machine's roost-session
is reached over roost's SSH client-bridge, which a hermetic harness cannot do —
so the app is launched with the test-mode `SHED_TAURI_ROOST_SOCKETS` seam, which
swaps the bridge for a direct `LocalSession` on a Unix socket, per machine.
Everything above that socket is the production path: the real `shed_core::roost`
client, the real `RoostWatcher`, the real decode, the real row mapping. The far
side is `fake_roost.FakeRoost`, which answers from roost's own vendored wire
vectors — so these tests cannot pass against a shape roost does not publish.

That seam exists because a client has to choose its own transport (mobile
supplies a Dart one). It turning out to be exactly what a hermetic harness needs
is a good sign the cut landed in the right place.

The alternative — injecting rendered rows like `rc.inject_test` does — would test
the renderer and nothing else, leaving the roost client, the watcher, the poll
loop and the unreachable posture uncovered. Those are where the bugs were.

**Ordering is load-bearing.** Each app instance here is module-scoped (the
`machines:` config and the socket map are read at LAUNCH, as in production), and
a self-managed instance replaces the session app — so the tests for one instance
are contiguous, and the read-only assertions about a machine's baseline rows come
BEFORE the tests that mutate the fake's tabs.
"""

from __future__ import annotations

import json
import os
import platform
import shutil
import socket
import subprocess
import tempfile
import time
from pathlib import Path

import pytest

import ui
from client import ShedError, TauriClient, scaled_timeout
from fake_roost import FakeRoost, RoostWireError, roost_call

pytestmark = pytest.mark.skipif(
    os.environ.get("SHED_TEST_TARGET", "mac") != "tauri",
    reason="tauri-only: the mac app has no machine layer",
)

FIXTURES = Path(__file__).resolve().parent / "fixtures"

# The identity token a real hub returns from /v1/health (Go `rc.HubAppID`). Still
# here because the app HOSTS a hub (S6 retires that); it no longer READS one.
HUB_APP_ID = "shed-rc-hub"

# mini3's baseline tabs. An agent tab and a plain shell tab, which is what a real
# roost-session looks like — the shed-recorded vector these are built from
# carries exactly that pair.
AGENT_TAB = 4
SHELL_TAB = 3
OC_CWD = "/home/shed/oc-work"
OC_TITLE = "OC | exact pong reply"
OC_SESSION = "ses_f8510bbf0ffePFCHCY6iyzAieq"


def _free_port() -> int:
    with socket.socket() as s:
        s.bind(("127.0.0.1", 0))
        return s.getsockname()[1]


def _machine_rows(app: TauriClient, machine: str | None = None) -> list[dict]:
    """The machine-sourced rows in `rc.list` (optionally one machine's)."""
    rows = [s for s in app.call("rc.list").get("sessions", []) if s.get("origin_kind") == "machine"]
    return [r for r in rows if machine is None or r.get("machine") == machine]


def _named(rows: list[dict], name: str) -> dict:
    """One row by name, or `{}` — so a wait predicate can ask about a machine
    that has not been registered yet without blowing up mid-poll."""
    return next((r for r in rows if r["name"] == name), {})


# ---------------------------------------------------------------------------
# fakes + apps
# ---------------------------------------------------------------------------


@pytest.fixture(scope="module")
def fake_roost():
    """mini3's session: one opencode tab beside one plain shell tab.

    The opencode tab is the shape the row mapping is really about — an adapter
    has claimed it, so it carries a source, the agent's OWN session id, a
    lifecycle and a detail. The shell tab is somebody's terminal and must never
    become a card.
    """
    fake = FakeRoost().start()
    fake.add_tab(AGENT_TAB, cwd=OC_CWD, title=OC_TITLE, source="opencode",
                 session_id=OC_SESSION, lifecycle="finished", detail="session_idle",
                 shell_state="unknown")
    fake.add_tab(SHELL_TAB, cwd="/home/shed", title="shed@mini3: ~")
    try:
        yield fake
    finally:
        fake.stop()


@pytest.fixture(scope="module")
def flaky_roost():
    """A session that answers, and that a test can then STOP.

    This is the only way to produce the third health band: a machine that
    `connected_once` and is now unreachable — "offline", as opposed to "never
    reached". A fixture with only reachable and never-reached machines cannot
    tell an ordering regression from a correct sort.

    It runs NO tabs, so it is also the "up, and nothing running on it" case —
    reachable with an empty row set.

    Yields `(fake, stop)`; `stop` is idempotent so the teardown is safe after a
    test has already called it.
    """
    fake = FakeRoost().start()
    stopped = False

    def stop() -> None:
        nonlocal stopped
        if not stopped:
            stopped = True
            fake.stop()

    try:
        yield fake, stop
    finally:
        stop()


@pytest.fixture(scope="module")
def machine_app(fake_roost, flaky_roost, mock):
    """A SECOND, self-managed app instance pointed at the fake sessions.

    Self-managed rather than the shared session app because the machine config
    and the socket map must be set at LAUNCH — `machines:` is read once at
    startup to spawn the watchers, exactly as it would be in production. The
    down-host suite uses the same pattern for the same reason.
    """
    # `tempfile.mkdtemp` with a SHORT prefix, not pytest's tmp_path_factory: the
    # IPC socket lives under this dir, and pytest's nested path under macOS's
    # long TMPDIR overruns the Unix-socket limit (SUN_LEN) — the app then aborts
    # at bind. Same reason the session fixture does it this way.
    state_dir = Path(tempfile.mkdtemp(prefix="shed-e2e-mach-"))
    ui.quit("tauri")
    ui.launch(
        "tauri",
        mock_base_url=mock.base_url,
        config_path=FIXTURES / "config-machines.yaml",
        state_dir=state_dir,
        # mini3 gets the session with tabs; `flaky` gets one a test will stop;
        # `sleepy` is deliberately UNMAPPED, which the app treats as permanently
        # unreachable — the everyday asleep case, with no ssh and no real machine
        # involved. One machine per health band (see the fixture config).
        # `localhost` is unmapped too, so the implicit host stays unlisted and
        # this suite's machine set is exactly the configured three.
        roost_sockets={"mini3": fake_roost.socket_path, "flaky": flaky_roost[0].socket_path},
    )
    client = TauriClient(ui.socket_path("tauri"))
    # The WebView mounts AFTER `identify`, so wait for its first snapshot before
    # any test drives a UI op — otherwise `ui.navigate` races the frontend and
    # answers `frontend_not_ready`. Same wait the session fixture does.
    client.wait_until(
        lambda: client.current_pane() is not None,
        timeout=30,
        what="tauri frontend ready",
    )
    try:
        yield client
    finally:
        client.close()
        ui.quit("tauri")


@pytest.fixture(scope="module")
def addable_app(mock):
    """An app over a WRITABLE copy of the machine config.

    Its own instance, and its own copy: the add path really writes, and a test
    that mutated the committed fixture would poison every later run.

    MODULE-scoped like `machine_app`, and for the same non-obvious reason: a
    self-managed instance quits the app on teardown, and the autouse policy
    fixture that runs before the NEXT test would then find nothing listening.
    One launch per module keeps an app up for the whole run.
    """
    state_dir = Path(tempfile.mkdtemp(prefix="shed-e2e-add-"))
    cfg = state_dir / "config.yaml"
    cfg.write_text((FIXTURES / "config-machines.yaml").read_text())
    ui.quit("tauri")
    ui.launch(
        "tauri",
        mock_base_url=mock.base_url,
        config_path=cfg,
        state_dir=state_dir,
        # Nothing mapped: every machine here is an unreachable row, which is all
        # this instance needs — and it keeps the add path off any live session.
        roost_sockets={"nothing": "/nonexistent/roost.sock"},
    )
    client = TauriClient(ui.socket_path("tauri"))
    client.wait_until(
        lambda: client.current_pane() is not None,
        timeout=30,
        what="tauri frontend ready",
    )
    try:
        yield client, cfg
    finally:
        client.close()
        ui.quit("tauri")


@pytest.fixture(scope="module")
def localhost_app(mock):
    """An app whose implicit `localhost` host is mapped at a socket NOTHING is
    listening on yet, over a config with no `machines:` at all.

    Both halves matter: the empty config means every row this app reports is the
    implicit host and nothing else, and the dead mapping means the test owns the
    moment the session appears.
    """
    state_dir = Path(tempfile.mkdtemp(prefix="shed-e2e-lh-"))
    # Its own short dir: the socket has to exist at a path chosen BEFORE launch,
    # and `/tmp` keeps it well under SUN_LEN.
    sock_dir = Path(tempfile.mkdtemp(prefix="fkroost-lh-", dir="/tmp"))
    sock = sock_dir / "roost.sock"
    ui.quit("tauri")
    ui.launch(
        "tauri",
        mock_base_url=mock.base_url,
        config_path=FIXTURES / "config.yaml",
        state_dir=state_dir,
        roost_sockets={"localhost": sock},
    )
    client = TauriClient(ui.socket_path("tauri"))
    client.wait_until(
        lambda: client.current_pane() is not None,
        timeout=30,
        what="tauri frontend ready",
    )
    try:
        yield client, sock
    finally:
        client.close()
        ui.quit("tauri")
        shutil.rmtree(sock_dir, ignore_errors=True)


# ---------------------------------------------------------------------------
# adding a machine
# ---------------------------------------------------------------------------


def test_adding_a_machine_writes_the_config_and_starts_watching_it(addable_app):
    """The whole point of the button: a machine added now is watched now.

    A relaunch-to-see-it would be a worse affordance than editing the config by
    hand, which is what this replaces.
    """
    app, cfg = addable_app
    before = cfg.read_text()
    assert "mini4" not in before

    app.machine_add("mini4", user="charliek", rc_bin="/opt/sx")

    # It is in the file …
    after = cfg.read_text()
    assert "mini4:" in after
    assert "/opt/sx" in after
    # … and INSERT-ONLY: every original line survives, in order. This file is
    # hand-maintained, so an edit that reflowed it would be a bug even if the
    # result parsed.
    remaining = iter(before.splitlines())
    peek = next(remaining, None)
    for line in after.splitlines():
        if line == peek:
            peek = next(remaining, None)
    assert peek is None, "an original config line was dropped or reordered"

    # … and a backup of what was there before sits beside it.
    assert (cfg.parent / "config.yaml.bak").read_text() == before

    # … and it is being watched, without a relaunch.
    app.wait_until(
        lambda: any(m["name"] == "mini4" for m in app.machines_list()),
        timeout=20,
        what="the new machine to be watched",
    )


def test_adding_a_duplicate_machine_is_refused_without_touching_the_file(addable_app):
    """Refused, not shadowed: two rows with one name is a UI nobody can reason
    about — and the config must be left exactly as it was."""
    app, cfg = addable_app
    before = cfg.read_text()

    with pytest.raises(ShedError) as excinfo:
        app.machine_add("mini3")
    assert "already" in str(excinfo.value).lower()
    assert cfg.read_text() == before, "a refused add still wrote to the config"


def test_adding_localhost_is_refused(addable_app):
    """`localhost` is RESERVED for the implicit host — the machine the user is
    sitting at, which needs no config entry (plan 013 §3.4).

    Refused before the write, not after: a `localhost:` entry left in the config
    would silently win over the implicit host on the next launch, and the user
    would have no way to tell which one they were looking at.
    """
    app, cfg = addable_app
    before = cfg.read_text()

    with pytest.raises(ShedError) as excinfo:
        app.machine_add("localhost", host="example.internal")
    assert "always present" in str(excinfo.value), excinfo.value
    assert cfg.read_text() == before, "a refused add still wrote to the config"
    assert not any(m["name"] == "localhost" for m in app.machines_list()), (
        "the refusal registered a watcher anyway"
    )


# ---------------------------------------------------------------------------
# the implicit localhost host
# ---------------------------------------------------------------------------


def test_the_localhost_row_appears_once_its_socket_exists(localhost_app):
    """**Connect-if-present, in both directions** (plan 013 §3.4).

    A machine the user never configured must not produce a row just because the
    app happens to run there: with no `roost-session` listening, `localhost` is
    not listed AT ALL — not an unreachable row, not an error. Once a session has
    answered, the host is real and is listed with its sessions.

    The two halves are one test on purpose: an "it appears" assertion alone
    passes just as well against a client that lists it unconditionally.
    """
    app, sock = localhost_app

    # Before: nothing is listening, so there is no localhost anywhere.
    assert app.machines_list() == [], "a host that has never run a session is not listed"
    assert _machine_rows(app) == []

    fake = FakeRoost(socket_path=sock).start()
    try:
        fake.add_tab(9, cwd="/home/charliek/projects/shed", title="claude", source="claude",
                     session_id="0199-uuid", lifecycle="working", detail="session_status")
        app.wait_until(
            lambda: any(m["name"] == "localhost" and m["reachable"]
                        for m in app.machines_list()),
            # Generous: the watcher is on the shared 500ms→30s backoff, so the
            # first dial AFTER the socket appears can be a few seconds out.
            timeout=90,
            what="localhost to be listed once its session answers",
        )
        row = _named(app.machines_list(), "localhost")
        assert row["origin"] == "machine:localhost"
        assert row["connected_once"] is True
        assert row["sessions"] == 1
        assert row["detail"] is None

        app.wait_until(
            lambda: [r["slug"] for r in _machine_rows(app, "localhost")] == ["9"],
            timeout=30,
            what="localhost's session row",
        )
        session = _machine_rows(app, "localhost")[0]
        assert session["kind"] == "claude-rc"
        assert session["origin"] == "machine:localhost"
        assert session["activity"] == "working"
    finally:
        fake.stop()


# ---------------------------------------------------------------------------
# reading a machine's sessions (read-only — before anything mutates the fake)
# ---------------------------------------------------------------------------


def test_a_machine_session_is_listed_beside_shed_sessions(machine_app):
    """The core of it: a machine session appears in the SAME `rc.list` payload as
    shed sessions, mapped off roost's tab and stamped with its origin.

    The origin stamp is what the UI keys and labels by — `shed` is empty for
    every machine session by construction, so keying on it would collide two
    machines that happen to share a slug.
    """
    machine_app.wait_until(
        lambda: bool(_machine_rows(machine_app)),
        timeout=30,
        what="the machine session to arrive",
    )
    rows = _machine_rows(machine_app)
    assert len(rows) == 1, f"expected one machine session, got {rows!r}"
    row = rows[0]
    # The slug IS the roost tab id, and `tab_id` carries it as a STRING (roost's
    # own ids are strings on the wire; a JS client cannot round an i64 through a
    # Number).
    assert row["slug"] == str(AGENT_TAB)
    assert row["tab_id"] == str(AGENT_TAB)
    assert row["origin"] == "machine:mini3"
    assert row["machine"] == "mini3"
    assert row["shed"] == "", "a machine session belongs to no shed"
    assert row["host"] == "machine:mini3"
    assert row["stale"] is False, "a reachable machine's rows are not stale"
    assert row["tmux_session"] == "", "roost has no tmux"
    # The four agent axes, READ off roost rather than derived from pane text —
    # the whole point of the pivot.
    assert row["kind"] == "opencode", "ownership.source is the kind"
    assert row["state"] == "ready", "a roost tab is either there or gone"
    assert row["activity"] == "idle", "lifecycle `finished` is idle"
    assert row["attention"] is False, "roost's sticky notification bit"
    assert row["workdir"] == OC_CWD
    assert row["display_name"] == OC_TITLE
    # The AGENT's own session id (opencode's `ses_…`) — the handle a later native
    # verb addresses it by, and proof the row came off the adapter's ownership
    # rather than being synthesized from the tab alone.
    assert row["id"] == OC_SESSION


def test_a_shell_tab_beside_an_agent_tab_yields_one_row(machine_app, fake_roost):
    """**A roost-session is somebody's terminal multiplexer.** A user with
    fifteen shells open has fifteen tabs and zero sessions — only a tab an agent
    adapter has CLAIMED is a card (plan 013 §3.2).

    mini3's session carries exactly the pair the shed-recorded vector does: one
    claimed tab and one plain shell. Both halves are asserted — the claimed tab
    IS a row, the shell tab is NOT — because "no shell rows" alone would also
    pass against a client that listed nothing.
    """
    machine_app.wait_until(
        lambda: bool(_machine_rows(machine_app, "mini3")),
        timeout=30, what="mini3's rows",
    )
    listed = {r["slug"] for r in _machine_rows(machine_app, "mini3")}
    assert str(AGENT_TAB) in listed, "the agent-owned tab is a session"
    assert str(SHELL_TAB) not in listed, "a plain shell tab is not a session"
    # …and the shell tab really is on the wire, so its absence above is the
    # client filtering rather than the fake never serving it.
    assert SHELL_TAB in fake_roost.tab_ids(), "the fake never served the shell tab"


def test_a_reachable_machine_reports_its_health(machine_app):
    """Every configured machine has a status row, whether or not it has
    sessions — the row is how the UI renders a machine group."""
    machine_app.wait_until(
        # BOTH mapped sessions, not "any": mini3 answering first would satisfy a
        # loose predicate and the flaky assertions below would then race its
        # first snapshot.
        lambda: {m["name"] for m in machine_app.machines_list() if m["reachable"]}
        == {"mini3", "flaky"},
        timeout=30,
        what="both mapped machines to report reachable",
    )
    machines = machine_app.machines_list()
    # `machines.list` is the BACKEND's view: every configured machine, in config
    # order, with no health ranking. The healthy-first ordering is a rendering
    # concern and is asserted against the sidebar, not here. `localhost` is
    # absent because it is unmapped in this app — it has never answered.
    assert [m["name"] for m in machines] == ["flaky", "mini3", "sleepy"]
    live = _named(machines, "mini3")
    assert live["reachable"] is True
    assert live["connected_once"] is True
    assert live["sessions"] == 1
    assert live["detail"] is None
    assert live["origin"] == "machine:mini3"
    # A session that is UP with nothing running is reachable with no rows — a
    # different claim from "unreachable", and the one `flaky` is here to make.
    up_and_empty = _named(machines, "flaky")
    assert up_and_empty["reachable"] is True
    assert up_and_empty["sessions"] == 0


def test_an_unreachable_machine_is_a_row_with_a_reason_not_an_error(machine_app):
    """**Unreachable is a first-class state.** `sleepy` is unmapped in the socket
    map, so nothing answers for it — the everyday case of a machine that is
    asleep or off-network.

    It must be LISTED, marked unreachable, and carry a reason. A machine being
    down must never fail the sessions view or surface as an error.
    """
    machine_app.wait_until(
        lambda: _named(machine_app.machines_list(), "sleepy").get("detail") is not None,
        timeout=30,
        what="the unreachable machine to report why",
    )
    sleepy = _named(machine_app.machines_list(), "sleepy")
    assert sleepy["reachable"] is False
    assert sleepy["connected_once"] is False, "it never connected"
    assert sleepy["sessions"] == 0
    assert sleepy["detail"], "an unreachable machine must say WHY"
    # `rc.list` still answers — a down machine does not break the view.
    assert isinstance(machine_app.call("rc.list").get("sessions"), list)


def test_the_agents_pane_renders_machine_rows(machine_app):
    """The rendered truth, not just the payload: the Agents pane reports its
    sessions via `agents.dump`, so the machine row must reach the UI."""
    machine_app.navigate("agents")
    machine_app.wait_until(
        lambda: any(s.get("origin_kind") == "machine" for s in machine_app.agents_dump()),
        timeout=30,
        what="the machine row to render",
    )
    rendered = machine_app.agents_dump()
    row = next(s for s in rendered if s.get("origin_kind") == "machine")
    assert row["display_name"] == OC_TITLE
    assert row["origin"] == "machine:mini3"
    assert row["tab_id"] == str(AGENT_TAB)


def test_the_machines_pane_groups_each_machine_with_its_own_sessions(machine_app):
    """The Machines pane's UI truth: a row per CONFIGURED machine — reachable or
    not — each leading the sessions that belong to it.

    `machines.dump` is deliberately not `machines.list`: the backend view can be
    perfect while nothing reaches the window, which is precisely the bug this
    pane's first cut shipped with.
    """
    machine_app.navigate("machines")
    machine_app.wait_until(
        lambda: (machine_app.machines_dump() or []) and
        any(r["sessions"] for r in machine_app.machines_dump()),
        timeout=30,
        what="the machines pane to render a machine with its session",
    )
    rows = {r["name"]: r for r in machine_app.machines_dump()}
    assert set(rows) == {"mini3", "sleepy", "flaky"}, f"unexpected pane rows: {rows}"

    live = rows["mini3"]
    assert live["reachable"] is True
    assert live["status"] == "reachable"
    assert live["origin"] == "machine:mini3"
    # The session is grouped UNDER its machine, keyed by origin — not left to be
    # found by filtering the Agents list.
    assert live["sessions"] == [str(AGENT_TAB)], f"mini3's session is not grouped under it: {live}"
    # A reachable machine's sub-line is its session COUNT (the reason slot is
    # only used when there is something wrong to report).
    n = len(live["sessions"])
    assert live["detail"] == f"{n} session" + ("" if n == 1 else "s"), live

    # `sleepy` never answered, so it is "connecting", NOT "unreachable" — a
    # distinction the raw boolean cannot make and a user needs (it may yet come up).
    down = rows["sleepy"]
    assert down["reachable"] is False
    assert down["status"] == "connecting", f"unexpected status: {down}"
    assert down["sessions"] == []
    assert down["detail"], "a machine that isn't up must say why"


def test_the_machines_dump_is_null_off_pane_and_fresh_on_return(machine_app):
    """A pane dump reports what is RENDERED, so off-pane it must be null and a
    return to the pane must re-report rather than resurrect a stale snapshot.

    Note what each half proves. The off-pane null is enforced by the Rust gate
    (`machines_dump` answers null unless the reported pane is `machines`), so it
    does NOT by itself prove the frontend cleared — the frontend's
    clear-on-unmount is defence in depth. The RETURN half is the load-bearing
    one: it can only pass if the pane re-reports on mount.
    """
    # Start from somewhere else, so the first wait below is a real transition
    # and not a condition the previous test already satisfied.
    machine_app.navigate("sheds")
    machine_app.wait_until(
        lambda: machine_app.machines_dump() is None,
        timeout=15, what="the machines dump to be null off-pane",
    )

    machine_app.navigate("machines")
    machine_app.wait_until(
        lambda: machine_app.machines_dump() is not None,
        timeout=30, what="the machines pane to re-report on return",
    )
    rows = machine_app.machines_dump()
    assert {r["name"] for r in rows} == {"mini3", "sleepy", "flaky"}, rows

    # A smoke check for the on-pane null flap, and honestly only that: a clear
    # folded into the report effect's cleanup (rather than a mount/unmount effect
    # of its own) re-clears before every re-report, and each parent re-render —
    # the 5s shed poll, an approval, an appearance flip — would then open a
    # window where this reads null with the pane plainly on screen. The window is
    # sub-millisecond, so sampling cannot RELIABLY catch it; the actual defence
    # is the effect split in `MachinesPane`. This only fails loudly if the flap
    # ever becomes wide.
    for _ in range(20):
        assert machine_app.machines_dump() is not None, "machines.dump flapped to null on-pane"


def test_a_native_remote_row_has_no_terminal_action(machine_app):
    """**A roost row's terminal belongs to roost** (plan 013 §3.4).

    roost's synthesized capabilities say `attach: "native-remote"`, so there is
    no tmux session to attach to and both terminal doors refuse a machine row
    server-side — the React card hides `>_ open` off the same capability, and
    this is the half that holds when something asks anyway.

    Paired with the shed row below, which still previews: a blanket "terminal is
    refused" would pass the machine half and break every real user.
    """
    caps = machine_app.call("rc.list")["capabilities"]
    machine_caps = caps["machine:mini3"]
    assert machine_caps["rc_version"] == 2, "roost claims contract v2"
    assert machine_caps["kind_features"]["opencode"]["attach"] == "native-remote"

    # `machine.capabilities` answers the SAME synthesized set, not a second,
    # probed one that could disagree with the payload the cards render from.
    assert machine_app.machine_capabilities("mini3") == machine_caps

    for op in ("terminal.preview", "terminal.open"):
        with pytest.raises(ShedError) as excinfo:
            machine_app.call(op, {"machine": "mini3", "shed": "hello-world"})
        assert excinfo.value.code == "not_enabled", excinfo.value
        assert "attach is native-remote" in str(excinfo.value), excinfo.value

    # The presence half: a SHED row still resolves its ssh command.
    preview = machine_app.terminal_preview("hello-world")
    assert preview["argv"][0] == "ssh", preview


def test_the_sidebar_lists_machines_under_the_shed_servers(machine_app, flaky_roost):
    """The sidebar's status foot carries BOTH kinds of place a session can run,
    in one vocabulary — and unlike a pane dump it answers from anywhere, because
    the sidebar is always mounted.

    Ordering is load-bearing and spans THREE bands: reachable, then never-yet-
    reached ("connecting"), then reached-and-now-gone ("offline"). A status list
    that reshuffles as machines come and go is one you stop trusting at a glance.

    Producing the third band is why `flaky` exists: its session answers, so the
    app records `connected_once`, and then this test STOPS it. Without that, a
    regression that ordered offline above connecting would pass unnoticed.
    """
    _, stop_flaky = flaky_roost
    machine_app.wait_until(
        lambda: any(m["name"] == "flaky" and m["status"] == "reachable"
                    for m in machine_app.sidebar_dump().get("machines") or []),
        timeout=30, what="flaky to connect before it is stopped",
    )
    stop_flaky()
    machine_app.wait_until(
        lambda: any(m["name"] == "flaky" and m["status"] == "unreachable"
                    for m in machine_app.sidebar_dump().get("machines") or []),
        timeout=60, what="flaky to go offline after its session died",
    )

    bar = machine_app.sidebar_dump()
    assert bar["servers"], "the sidebar lost the shed servers"
    names = [m["name"] for m in bar["machines"]]
    assert names == ["mini3", "sleepy", "flaky"], (
        f"expected reachable -> connecting -> offline, got {names}: {bar['machines']}"
    )

    live, connecting, offline = bar["machines"]
    assert live["status"] == "reachable"
    # The NOTE is what a person reads in the list — a clean word or a count,
    # never the raw transport error (which stays on hover).
    assert live["note"].endswith("session") or live["note"].endswith("sessions"), live
    assert connecting["note"] == "connecting", f"raw error text leaked: {connecting}"
    assert offline["note"] == "offline", f"raw error text leaked: {offline}"

    # The reason a stopped session gives NAMES the socket that is gone — "no
    # route to host" and "nothing is listening" are different problems, and the
    # app must not flatten them into "offline".
    #
    # The FIRST `Down` is the held connection dying ("connection reset by peer"),
    # which is true but transient; the SETTLED reason — what the row keeps saying
    # while the session stays gone — is the reach's, and it is the one worth
    # asserting.
    machine_app.wait_until(
        lambda: "no roost-session at" in (
            _named(machine_app.machines_list(), "flaky").get("detail") or ""),
        timeout=60, what="flaky to settle on the socket-gone reason",
    )
    assert str(flaky_roost[0].socket_path) in _named(
        machine_app.machines_list(), "flaky")["detail"]


# ---------------------------------------------------------------------------
# status changes + the write verbs (these MUTATE the fake — keep them last)
# ---------------------------------------------------------------------------


def test_a_lifecycle_flip_reaches_rc_list_within_one_poll(machine_app, fake_roost):
    """**S3's acceptance cell.** An agent's turn changing on the machine reaches
    the sessions view by itself, with no refresh and no relaunch.

    The path is the whole point of the pivot: roost's adapter writes the axes,
    `tab.list` carries them, the watcher polls, the row's `activity` follows. The
    harness turns the cadence down with `SHED_ROOST_POLL_MS=50` (read once, at
    watcher spawn — which is why `ui.launch` sets it), so "within one poll" is
    fast; the wait budget is generous because the render gate runs under Xvfb.

    `has_notification` rides along as `attention` and NOT as activity: roost
    clears it on UI focus and shed never clears it, so folding it into "needs
    you" would leave a card stuck asking for a decision forever.
    """
    fake_roost.set_axes(AGENT_TAB, lifecycle="working", detail="session_status")
    machine_app.wait_until(
        lambda: [r["activity"] for r in _machine_rows(machine_app, "mini3")] == ["working"],
        timeout=10, what="the working flip to reach rc.list",
    )

    started = time.monotonic()
    fake_roost.set_axes(AGENT_TAB, lifecycle="waiting", detail="question_asked",
                        has_notification=True)
    machine_app.wait_until(
        lambda: [r["activity"] for r in _machine_rows(machine_app, "mini3")] == ["needs_input"],
        timeout=10, what="the waiting flip to reach rc.list",
    )
    elapsed = time.monotonic() - started
    row = _machine_rows(machine_app, "mini3")[0]
    assert row["attention"] is True, "the sticky notification bit"
    assert row["slug"] == str(AGENT_TAB), "the same tab, not a new row"
    # `question_asked` is a QUESTION; only opencode's `permission_asked` (and
    # claude's `permission_prompt`) are approvals, matched by equality so a
    # `permission_replied` is never read as a new one.
    assert row["activity"] != "needs_approval"
    # A floor, not a stopwatch: the SHIPPED cadence is 2 s, so on an unscaled
    # local run this only passes when `SHED_ROOST_POLL_MS` actually took. Under
    # the render gate's timeout scale the budget widens with everything else,
    # which is the price of never flaking on a loaded runner.
    assert elapsed < scaled_timeout(1.5), f"the flip took {elapsed:.1f}s"


def test_launching_on_a_machine_routes_over_the_machine_not_a_server(machine_app, fake_roost):
    """**Starting a session on a machine is a first-class create**, and on roost
    it is a `tab.open` of the kind's agent — addressed by machine name over that
    machine's own session, never by `(host, shed)` through a server.

    M1's kickoff is deliberately minimal (plan 013 §4): the agent binary and a
    cwd. No prompt, no permission mode, no title — roost owns the tab's title
    (it follows the foreground process) and prompts need the provider script that
    is a later slice.
    """
    before = len(fake_roost.opens)
    before_tabs = set(fake_roost.tab_ids())
    row = machine_app.machine_launch("mini3", kind="opencode", workdir="  /home/shed/new  ")

    assert len(fake_roost.opens) == before + 1, "the launch never reached the session"
    opened = fake_roost.opens[-1]
    assert opened["argv"] == ["opencode"], f"the kind's argv, and nothing else: {opened}"
    assert opened["cwd"] == "/home/shed/new", "the workdir, trimmed"
    assert opened["title"] == "", "the title is roost's"

    # The answer is a ROW, in the same shape the list serves — a caller that keys
    # off `origin`/`machine` must get the same thing from both doors.
    assert row["origin"] == "machine:mini3"
    assert row["machine"] == "mini3"
    assert row["workdir"] == "/home/shed/new"
    # The kind the caller ASKED for: `tab.open` answers before any adapter has
    # claimed the tab, so roost would report it as a plain shell for a second.
    assert row["kind"] == "opencode"
    opened_ids = set(fake_roost.tab_ids()) - before_tabs
    assert row["slug"] in {str(i) for i in opened_ids}, (
        f"the answered row is not the tab that was opened: {row['slug']} vs {opened_ids}"
    )


def test_launching_an_unknown_kind_on_a_machine_is_refused(machine_app, fake_roost):
    """Refused BEFORE the transport, twice over: a kind this build has never
    heard of never reaches roost, and neither does a known kind roost has no
    launch recipe for. Either way nothing is opened on somebody's machine."""
    before = len(fake_roost.opens)

    with pytest.raises(ShedError) as excinfo:
        machine_app.machine_launch("mini3", kind="wat")
    assert "wat" in str(excinfo.value), excinfo.value
    assert "machine:mini3" not in str(excinfo.value), (
        f"an unknown kind must be refused BEFORE the transport: {excinfo.value}"
    )

    # `shell` IS a known kind — it is the machine layer, not the kind gate, that
    # refuses it: roost has no agent to start for a bare shell.
    with pytest.raises(ShedError) as excinfo:
        machine_app.machine_launch("mini3", kind="shell")
    assert "shell" in str(excinfo.value), excinfo.value

    assert len(fake_roost.opens) == before, "a refused launch still opened a tab"


def test_probing_an_unknown_machine_is_rejected(machine_app):
    """A capability probe for a machine that is not configured names the ones
    that are, rather than failing as a transport error."""
    with pytest.raises(ShedError) as excinfo:
        machine_app.machine_capabilities("ghost")
    message = str(excinfo.value)
    assert "ghost" in message
    assert "mini3" in message or "sleepy" in message


def test_killing_an_unknown_machine_is_rejected(machine_app):
    """A slug on a machine that is not configured fails with the configured
    names, rather than a confusing transport error."""
    with pytest.raises(ShedError) as excinfo:
        machine_app.machine_kill("ghost", "x")
    message = str(excinfo.value)
    assert "ghost" in message
    assert "mini3" in message or "sleepy" in message, (
        f"an unknown machine should name the configured ones: {message!r}"
    )


def test_killing_a_machine_session_routes_over_the_machine_not_a_server(machine_app, fake_roost):
    """`machine.kill` is addressed by (machine, slug), not (host, shed, slug),
    and on roost it is a `tab.close`: **the tab really leaves the session.**

    Asserting the far side, not just the payload, is the point — an optimistic
    drop that never reached the machine would look identical in `rc.list` until
    the next poll put the row back.
    """
    assert AGENT_TAB in fake_roost.tab_ids(), "the tab to close is there to begin with"

    machine_app.machine_kill("mini3", str(AGENT_TAB))

    assert AGENT_TAB not in fake_roost.tab_ids(), "the tab is still open on the machine"
    with pytest.raises(RoostWireError) as wire:
        roost_call(fake_roost.socket_path, "tab.dump", {"tab_id": str(AGENT_TAB)})
    assert wire.value.code == "not-found", wire.value

    # …and the row is gone from the view, optimistically — the watcher polls, so
    # waiting for the next snapshot would read as "the kill didn't work".
    assert str(AGENT_TAB) not in {r["slug"] for r in _machine_rows(machine_app, "mini3")}

    # A slug that is not a roost tab id is refused by name rather than sent to
    # roost as a zero.
    with pytest.raises(ShedError) as excinfo:
        machine_app.machine_kill("mini3", "rc-abc123")
    assert "not a roost tab id" in str(excinfo.value), excinfo.value


def test_a_daemon_restart_replaces_the_row_set(machine_app, fake_roost):
    """**A restarted daemon resyncs wholesale.**

    roost's `revision` is an in-process counter that RESETS on restart, while tab
    ids are persisted and do not. So a client must compare
    `(session_id, revision)` with `!=` rather than watching the number climb: a
    `>` test would stop emitting the moment the counter went backwards and every
    row would sit frozen for the rest of the session's life.

    The restart happens FIRST here, so every change after it lands on a revision
    LOWER than the last one emitted — which is exactly the state a `>` client
    cannot see.
    """
    fake_roost.add_tab(6, cwd="/home/shed/before", title="OC | before", source="opencode",
                       session_id="ses_before", lifecycle="working", detail="session_status")
    machine_app.wait_until(
        lambda: [r["slug"] for r in _machine_rows(machine_app, "mini3")] == ["6"],
        timeout=15, what="the pre-restart row set",
    )
    before_id = fake_roost.session_id

    fake_roost.restart()
    assert fake_roost.session_id != before_id, "a restart is a new daemon instance"
    assert fake_roost.revision == 1, "the revision counter resets"
    # A different tab set on the other side of the restart: the old row is
    # released by its adapter, a new one is claimed.
    fake_roost.set_axes(6, ownership=None, lifecycle="inactive", has_notification=False)
    fake_roost.add_tab(7, cwd="/home/shed/after", title="OC | after", source="opencode",
                       session_id="ses_after", lifecycle="working", detail="session_status")

    machine_app.wait_until(
        lambda: [r["slug"] for r in _machine_rows(machine_app, "mini3")] == ["7"],
        timeout=30, what="the row set to be replaced after the restart",
    )
    row = _machine_rows(machine_app, "mini3")[0]
    assert row["id"] == "ses_after"
    assert row["workdir"] == "/home/shed/after"
    assert row["activity"] == "working"
    # Tab 6 is still IN the session (ids persist across a restart) — it is gone
    # from the view because nobody owns it, not because roost forgot it.
    assert 6 in fake_roost.tab_ids()


# ---------------------------------------------------------------------------
# The app HOSTING the rc hub (plan 012 S4 / roadmap R4's hub-home graduation)
# ---------------------------------------------------------------------------


def test_the_app_hosts_the_rc_hub_when_it_brokers_in_process(mock, tmp_path):
    """**R4's hub-home graduation, proven end to end.**

    The rc-hub role moved out of the `shed-host-agent` bin into
    `shed_broker::rc_hub::role` so a second consumer could host it. This is that
    consumer: with no daemon to broker for it, the app runs the broker
    in-process — and now the hub with it, from the same code the daemon runs.

    Hosting, not reading: since plan 013 the app's own machine rows come from
    roost, and this hub is what `sx watch` and the phone read FROM it. S6 retires
    it.

    Hermetic on two axes: `SHED_TAURI_HOST_AGENT_SOCKET` is cleared so the app
    resolves EMBEDDED mode (rather than dialling a real daemon), and
    `SHED_RC_HUB_ADDR` pins the hub to an ephemeral port instead of the
    production 1029 — otherwise this test would fight a real daemon on the
    developer's machine and two concurrent runs would fight each other.
    """
    import http.client

    cfg = ui._SUBPROC["tauri"]
    runtime_dir = Path(tempfile.mkdtemp(prefix="shed-e2e-hub-"))
    hub_port = _free_port()
    try:
        shed_config = runtime_dir / "config.yaml"
        shutil.copyfile(FIXTURES / "config.yaml", shed_config)

        env = ui.subproc_env(
            cfg,
            runtime_dir=runtime_dir,
            mock_base_url=mock.base_url,
            config_path=shed_config,
            host_agent_socket=None,
        )
        # No desktop socket => no daemon => EMBEDDED mode, which is the mode that
        # hosts the hub. (subproc_env only sets the key when given one; clear an
        # inherited value so None really means "no daemon".)
        env.pop("SHED_TAURI_HOST_AGENT_SOCKET", None)
        env.pop("SHED_HOST_AGENT_SOCKET_DIR", None)
        # Pin the hub off the production port — see the docstring.
        env["SHED_RC_HUB_ADDR"] = f"127.0.0.1:{hub_port}"

        sock = runtime_dir / cfg.sock_rel
        log = runtime_dir / "hub-ui.log"
        log_fh = open(log, "wb")
        proc = subprocess.Popen(
            [str(cfg.binary)], env=env, stdout=log_fh, stderr=subprocess.STDOUT
        )
        try:
            ui.await_hermetic(
                "tauri", sock=sock, mock_base_url=mock.base_url, proc=proc, log=log
            )
            client = TauriClient(sock)
            try:
                # The app must SERVE the hub wire: identity, and a snapshot. This
                # is the same `/v1` surface `sx watch` and the phone read, so a
                # pass here means the app is a real hub host, not a stub.
                def hub_answers() -> bool:
                    try:
                        conn = http.client.HTTPConnection("127.0.0.1", hub_port, timeout=2)
                        conn.request("GET", "/v1/health")
                        body = json.loads(conn.getresponse().read())
                        conn.close()
                        return body.get("app") == HUB_APP_ID
                    except OSError:
                        return False

                client.wait_until(
                    hub_answers, timeout=60, what="the app's hub to answer /v1/health"
                )

                conn = http.client.HTTPConnection("127.0.0.1", hub_port, timeout=5)
                conn.request("GET", "/v1/sessions")
                snapshot = json.loads(conn.getresponse().read())
                conn.close()
                assert "sessions" in snapshot, f"the hub served no snapshot: {snapshot!r}"
            finally:
                client.close()
        finally:
            ui.terminate(proc)
            log_fh.close()
            # The hub must go away with the app — a leaked listener would hold the
            # port for every later run.
            deadline = time.monotonic() + 10
            while time.monotonic() < deadline:
                try:
                    with socket.create_connection(("127.0.0.1", hub_port), timeout=0.25):
                        time.sleep(0.1)
                except OSError:
                    break
            else:
                pytest.fail("the hub still answers after the app exited")
    finally:
        shutil.rmtree(runtime_dir, ignore_errors=True)


# ---------------------------------------------------------------------------
# Against a REAL roost-session (opt-in — CI has no roost binary)
# ---------------------------------------------------------------------------


def _real_session_binary() -> Path | None:
    """The `roost-session` daemon to smoke against, or None to skip.

    `SHED_TAURI_ROOST_SESSION_BIN` is the same shape roost's own harness uses for
    `ROOST_SESSION_BIN`: an explicit path to a built daemon. Nothing is built
    here — roost's tree is not this repo's to compile.
    """
    raw = os.environ.get("SHED_TAURI_ROOST_SESSION_BIN")
    if not raw:
        return None
    path = Path(raw).expanduser()
    return path if os.access(path, os.X_OK) else None


def _jailed_session_env(root: Path) -> tuple[dict[str, str], Path]:
    """A throwaway profile for a real `roost-session`: its environment, and the
    directory its socket will appear UNDER. Mirrors roost's
    `tools/roosttest/session.py::make_env`.

    The daemon resolves its socket / state / log from the ENVIRONMENT, so a
    shared profile would let this test contend with the developer's own session
    on one socket, one `state.json`, and one lock pair. It also creates only the
    LEAF of its runtime directory (non-recursively, 0700), which is right on a
    real machine where the OS provides `$XDG_RUNTIME_DIR` / `~/Library/Caches` —
    so this fixture plays the OS and pre-creates the parent.

    The socket's own directory name splits on the daemon's build profile
    (`roost-session-dev` / `RoostSessionDev` for a debug build) and is not
    observable from outside the binary. roost's harness predicts it from cargo's
    layout (`target/<profile>/<bin>`), which stops being true the moment the
    binary is copied or bind-mounted somewhere else — so this returns the PARENT
    and the caller discovers the socket under it. The profile root holds exactly
    one session, so there is never a second one to confuse it with.
    """
    home = root / "home"
    env = {k: v for k, v in os.environ.items()
           if not k.startswith(("XDG_", "ROOST_")) and k not in ("HOME", "SHELL")}
    env["HOME"] = str(home)
    # `/bin/sh` in an empty HOME emits no OSC marks, so a restored tab cannot
    # rewrite the title or cwd this test reads.
    env["SHELL"] = "/bin/sh"
    env["ROOST_SHELL_FEATURES"] = ""
    home.mkdir(parents=True, exist_ok=True)

    if platform.system() == "Darwin":
        runtime_parent = home / "Library/Caches"
    else:
        for name in ("run", "data", "state", "cache"):
            (root / name).mkdir(parents=True, exist_ok=True)
        env["XDG_RUNTIME_DIR"] = str(root / "run")
        env["XDG_DATA_HOME"] = str(root / "data")
        env["XDG_STATE_HOME"] = str(root / "state")
        env["XDG_CACHE_HOME"] = str(root / "cache")
        runtime_parent = root / "run"
    runtime_parent.mkdir(parents=True, exist_ok=True)
    return env, runtime_parent


@pytest.mark.real_roost
@pytest.mark.skipif(_real_session_binary() is None,
                    reason="set SHED_TAURI_ROOST_SESSION_BIN to a built roost-session")
def test_a_real_roost_session_answers_the_client(mock):
    """The fake's fidelity claim, checked against the real thing.

    Everything else in this module talks to `fake_roost.py`. That fake is built
    from roost's vendored vectors, but a vector is a snapshot: the assertion that
    actually binds is this one, where a REAL `roost-session` — jailed into a
    throwaway profile so it cannot touch the developer's own — answers
    `session.identify`, takes a `tab.open`, and shows up in the app as a
    reachable `localhost`.

    Opt-in because CI has no roost binary and building one needs libghostty-vt
    and roost's tree (plan 013 §10: declined to build it inside the Docker gate).
    """
    binary = _real_session_binary()
    assert binary is not None  # the skipif above
    root = Path(tempfile.mkdtemp(prefix="real-roost-", dir="/tmp")).resolve()
    launch_cwd = root / "launch"
    launch_cwd.mkdir(parents=True, exist_ok=True)
    env, runtime_parent = _jailed_session_env(root)
    state_dir = Path(tempfile.mkdtemp(prefix="shed-e2e-real-"))
    daemon = None
    errlog = None
    app = None
    try:
        # Readiness is detected by polling for the socket + a live `identify`
        # below, never by reading a line from stdout — so stdout is drained to
        # DEVNULL rather than left as an unread PIPE (a chatty daemon would
        # otherwise fill the pipe buffer and block). errlog is opened inside
        # the try so a failed Popen still gets it closed in `finally`.
        errlog = open(root / "session.stderr", "wb")
        daemon = subprocess.Popen([str(binary), "start", "--foreground"], cwd=str(launch_cwd),
                                  env=env, stdout=subprocess.DEVNULL, stderr=errlog)
        # Poll for a session that ANSWERS, never a sleep: the daemon prints one
        # readiness line, but a socket that exists is not yet a socket that
        # serves. The path is discovered rather than predicted — see
        # `_jailed_session_env`.
        deadline = time.monotonic() + 60
        identify = None
        sock = None
        while identify is None:
            if daemon.poll() is not None:
                pytest.fail(f"roost-session exited {daemon.returncode}; see {root}/session.stderr")
            if time.monotonic() > deadline:
                pytest.fail(f"no roost-session answering under {runtime_parent} "
                            f"(saw {sorted(runtime_parent.glob('*'))})")
            found = sorted(runtime_parent.glob("*/roost.sock"))
            if not found:
                time.sleep(0.2)
                continue
            sock = found[0]
            try:
                identify = roost_call(sock, "session.identify")
            except (OSError, RoostWireError):
                time.sleep(0.2)
        assert identify["session_id"], identify
        assert isinstance(identify["session_protocol"], int), identify

        # A plain shell tab: it proves the wire end to end, and it must NOT
        # become a session card — only an agent-claimed tab is one.
        opened = roost_call(sock, "tab.open", {"project_id": "0", "cwd": str(launch_cwd),
                                               "argv": ["/bin/sh"], "cols": 80, "rows": 24,
                                               "title": ""})
        assert opened["tab"]["id"], opened

        ui.quit("tauri")
        ui.launch("tauri", mock_base_url=mock.base_url, config_path=FIXTURES / "config.yaml",
                  state_dir=state_dir, roost_sockets={"localhost": sock})
        app = TauriClient(ui.socket_path("tauri"))
        app.wait_until(
            lambda: any(m["name"] == "localhost" and m["reachable"] for m in app.machines_list()),
            timeout=90,
            what=f"a real roost-session at {sock} to read as reachable",
        )
        assert _machine_rows(app, "localhost") == [], "a plain shell tab is not a session"
    finally:
        if app is not None:
            app.close()
        ui.quit("tauri")
        if daemon is not None:
            ui.terminate(daemon)
        if errlog is not None:
            errlog.close()
        shutil.rmtree(root, ignore_errors=True)
        shutil.rmtree(state_dir, ignore_errors=True)
