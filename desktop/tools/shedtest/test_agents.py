"""The Agents pane: the roost tabs on every listed shed, and the ops that
list and inject them.

**Roost-backed since S6** (`charliek/shed#328`). A shed's agent
sessions are the tabs its `roost-session` reports. `rc.list` returns them
(`hosts`, not a machines list) and `rc.inject_test` puts a row into the roost
snapshot. Launch, kill and the machines pane answer `unknown_op`.

**Hermetic, and genuinely exercising the real code.** The app is launched with
the test-mode `SHED_TAURI_ROOST_SOCKETS` seam — which swaps roost's SSH
client-bridge for a direct `LocalSession` on a Unix socket — pointed at
`fake_roost.FakeRoost`, which answers from roost's own vendored wire vectors.
Everything above the socket is the production path: the real `shed_core::roost`
client, the real watcher, the real decode, the real row mapping. A shed's socket
is keyed by its host TOKEN (`roost:<server>/<shed>`), which is how the app
addresses a shed's roost-session.
"""

from __future__ import annotations

import platform
import shutil
import subprocess
import tempfile
from pathlib import Path

import pytest

import ui
from client import ShedError, TauriClient
from fake_roost import FakeRoost


FIXTURES = Path(__file__).resolve().parent / "fixtures"

#: The server the harness config names, and the shed the mock reports running.
SERVER = "mock"
SHED = "hello-world"
#: The host token the app addresses that shed's roost-session by — and so the
#: key its socket is mapped under.
TARGET = f"roost:{SERVER}/{SHED}"

#: The fake's baseline tabs: an agent-owned one (a session row) beside a plain
#: shell (somebody's terminal, and never a card).
AGENT_TAB = 21
SHELL_TAB = 22


# ---------------------------------------------------------------------------
# the app + its far side
# ---------------------------------------------------------------------------


@pytest.fixture(scope="module")
def shed_roost():
    """The `roost-session` running INSIDE the harness's shed."""
    fake = FakeRoost().start()
    fake.add_tab(AGENT_TAB, cwd="/home/shed/work", title="OC | demo",
                 source="opencode", session_id="ses_agents", lifecycle="finished",
                 detail="session_idle", shell_state="unknown")
    fake.add_tab(SHELL_TAB, cwd="/home/shed", title="shed@hello-world: ~")
    try:
        yield fake
    finally:
        fake.shutdown()


@pytest.fixture(scope="module")
def agents_app(shed_roost, mock):
    """An app instance whose shed has a `roost-session`, pointed at the fake.

    Its own instance because the socket map is read at LAUNCH, exactly as the
    transport choice would be in production — the shared session app is launched
    from `conftest.py` with none.

    **Independent rather than self-managed**, the `test_tauri_downhost.py`
    pattern: its own throwaway HOME / XDG_RUNTIME_DIR (hence
    its own socket and single-instance lock), `ui._state` untouched, the session
    app left running. `ui.quit` + `ui.launch` would leave the session app dead at
    teardown, and the autouse `_reset_policy` / `_reset_mock` fixtures dial its
    socket before every test — so every module collected after this one would die
    at setup. That would make this file's position in the alphabet load-bearing,
    and this file sorts early.

    Its broker serves no server (`ui.subproc_env`'s default hermetic broker), so
    it never takes a listener slot on the session mock.

    Short `mkdtemp` prefixes: the IPC socket lives under the runtime dir, and a
    Unix socket path must stay inside SUN_LEN.
    """
    cfg = ui._SUBPROC["tauri"]
    if not cfg.binary.exists():
        raise RuntimeError(
            f"tauri binary not found at {cfg.binary}; build it first (make tauri-build).")
    runtime_dir = Path(tempfile.mkdtemp(prefix="shed-agents-"))
    sock = runtime_dir / cfg.sock_rel
    log = runtime_dir / "agents-ui.log"
    env = ui.subproc_env(
        cfg,
        runtime_dir=runtime_dir,
        mock_base_url=mock.base_url,
        config_path=FIXTURES / "config.yaml",
        # Only the shed. `localhost` is deliberately unmapped, so the implicit
        # host stays unlisted and every row this suite sees is the shed's.
        roost_sockets={TARGET: shed_roost.socket_path},
    )
    log_fh = open(log, "wb")
    proc = subprocess.Popen([str(cfg.binary)], env=env, stdout=log_fh,
                            stderr=subprocess.STDOUT)
    client = None
    try:
        ui.await_hermetic("tauri", sock=sock, mock_base_url=mock.base_url,
                          proc=proc, log=log)
        client = TauriClient(sock)
        client.wait_until(lambda: client.current_pane() is not None, timeout=30,
                          what="tauri frontend ready")
        yield client
    finally:
        if client is not None:
            client.close()
        ui.terminate(proc)
        log_fh.close()
        shutil.rmtree(runtime_dir, ignore_errors=True)


@pytest.fixture
def app(agents_app):
    """The app with its shed listed and its roost-session watched.

    `sheds.list` is the AUTHORITATIVE refresh — the one that carries which
    servers answered — and so the one that drives `observe_sheds`: a probe of
    each running shed, and a watcher for every one a session answers on. Until
    that has happened the shed is not a roost host and has no rows.
    """
    agents_app.call("sheds.list")
    agents_app.wait_until(
        lambda: any(h.get("origin") == TARGET and h.get("reachable")
                    for h in agents_app.call("rc.list")["hosts"]),
        timeout=30, what="the shed's roost-session to be probed and watched",
    )
    return agents_app


def _rows(app: TauriClient, **params) -> dict[str, dict]:
    """The shed's session rows from `rc.list`, by slug."""
    return {s["slug"]: s for s in app.call("rc.list", params)["sessions"]}


def _wait_for(app: TauriClient, what: str, probe, timeout: float = 20.0):
    """`wait_until`, but it hands BACK what the probe found — so a cell asserts
    on the value it waited for rather than re-reading it and racing itself."""
    found: list = []
    app.wait_until(lambda: bool(found.append(probe()) or found[-1]),
                   timeout=timeout, what=what)
    return found[-1]


# ---- list ----------------------------------------------------------------

def test_list_returns_the_roost_tabs_and_only_the_owned_ones(app):
    """The whole of what the pane shows: the agent-owned tabs its shed's
    roost-session reports, stamped as a shed's.

    The shell tab is the control — somebody's terminal is not an agent session,
    and a fake that listed it would let fifteen terminals become fifteen cards.
    """
    rows = _rows(app)
    assert str(AGENT_TAB) in rows, rows
    assert str(SHELL_TAB) not in rows, "a plain shell tab is not a session"

    row = rows[str(AGENT_TAB)]
    assert row["source"] == "roost"
    assert row["origin"] == TARGET
    assert row["origin_kind"] == "shed"
    assert row["host"] == SERVER, "the server, as a shed row spells it"
    assert row["shed"] == SHED
    assert row["target"] == TARGET, "the shed address, roost:<server>/<shed>"
    assert row["display_name"] == "OC | demo"
    assert row["workdir"] == "/home/shed/work"
    assert row["stale"] is False


def test_the_sheds_contract_is_keyed_by_its_roost_origin(app):
    """`capabilities` is keyed by ORIGIN — the same string a row carries — which
    is what gives the launch form and the card a data path from a row to the
    contract behind it.

    The pre-S6 `host/shed` composite was the HUB's key and is gone: there is one
    contract per host now, roost's synthesized one, and it says the terminal
    affordance is `native-remote` (no tmux pane to attach to).
    """
    caps = app.call("rc.list")["capabilities"]
    assert TARGET in caps, list(caps)
    assert f"{SERVER}/{SHED}" not in caps, "the hub's key is gone"
    assert caps[TARGET]["kind_features"]["opencode"]["attach"] == "native-remote"


def test_a_filtered_list_narrows_to_that_shed(app):
    """The shed card's own query. It is a SHED filter: server + name."""
    assert str(AGENT_TAB) in _rows(app, host=SERVER, shed=SHED)
    assert _rows(app, host=SERVER, shed="not-a-shed") == {}


# ---- removed ops ---------------------------------------------------------

@pytest.mark.parametrize("op,params", [
    ("machines.list", None),
    ("machines.dump", None),
    ("machine.add", {"name": "mini4"}),
    ("machine.kill", {"machine": "mini4", "slug": "1"}),
    ("machine.capabilities", {"machine": "mini4"}),
    ("machine.launch", {"target": TARGET, "kind": "opencode"}),
    ("roost.launch", {"target": TARGET, "kind": "opencode"}),
    ("roost.run", {"target": TARGET, "command": "true"}),
    ("rc.kill", {"shed": SHED, "slug": "1"}),
    ("ui.show_launch", None),
    ("launch.dump", None),
])
def test_removed_session_ops_are_unknown(app, op, params):
    """Launch, kill and the machines pane are gone. Each door answers the
    IPC's unknown-op error."""
    with pytest.raises(ShedError) as exc:
        app.call(op, params)
    assert exc.value.code == "unknown_op", exc.value
    assert exc.value.message == f"unknown op: {op}", exc.value


def test_rc_launch_is_an_unknown_op(app, shed_roost):
    """The 0.9.x alias `rc.launch {shed}` was removed in 0.10.0: it answers the
    IPC's unknown-op error and opens nothing."""
    opens = len(shed_roost.opens)
    with pytest.raises(ShedError) as exc:
        app.call("rc.launch", {"shed": SHED, "kind": "opencode"})
    assert exc.value.code == "unknown_op", exc.value
    assert exc.value.message == "unknown op: rc.launch", exc.value
    assert len(shed_roost.opens) == opens, "a removed op opened a tab"


# ---- inject (the render fixture) -----------------------------------------

def test_inject_puts_a_row_into_the_roost_snapshot(app):
    """`rc.inject_test` lands in the ROOST snapshot — the only row source there
    is — so the injected row is stamped and keyed exactly like one a session
    reported."""
    app.call("rc.inject_test", {"shed": SHED, "slug": "77", "kind": "claude-rc",
                                "display_name": "injected", "workdir": "/home/shed/inj"})
    row = _rows(app)["77"]
    assert row["origin"] == TARGET
    assert row["source"] == "roost"
    assert row["display_name"] == "injected"
    assert row["workdir"] == "/home/shed/inj"
    assert row["stale"] is False, "an injected row is not the last-known state"


def test_inject_refuses_a_slug_that_is_not_a_tab_id(app):
    """A row whose slug is not a tab id could never be closed, so it is refused
    rather than injected."""
    with pytest.raises(ShedError) as exc:
        app.call("rc.inject_test", {"shed": SHED, "slug": "legacy1"})
    assert exc.value.code == "bad_request"


@pytest.mark.parametrize("kind", ["shell", "claude-broker"])
def test_inject_refuses_a_kind_roost_has_no_adapter_for(app, kind):
    """The fixture op goes through the SNAPSHOT's own ownership filter.

    A real inventory lists the agent-owned tabs and sets the rest aside — an
    unowned tab is somebody's terminal. A kind roost has no adapter for makes
    exactly such a tab, and injecting one straight into the row set would let
    this harness photograph a card production can never draw. Refused instead,
    and nothing lands.
    """
    with pytest.raises(ShedError) as exc:
        app.call("rc.inject_test", {"shed": SHED, "slug": "79", "kind": kind})
    assert exc.value.code == "bad_request"
    assert "not a session row" in exc.value.message, exc.value.message
    assert "79" not in _rows(app), "a refused injection left a row behind"


# ---- render (the pane's own truth) ---------------------------------------

def test_agents_pane_renders_an_injected_session(app):
    """The Agents pane renders a row: the drivable `agents.dump` truth (which
    needs no display) + a window screenshot where the capture is available —
    macOS-tauri shells out to a Screen-Recording-TCC-gated tool, so the
    Linux/Xvfb render gate covers that pixel."""
    app.call("rc.inject_test", {"shed": SHED, "slug": "78", "kind": "opencode",
                                "display_name": "shot1"})
    app.navigate("agents")
    app.show_window()
    app.wait_until(lambda: "78" in {s["slug"] for s in app.agents_dump()},
                   timeout=20, what="agents.dump shows the injected session")
    if platform.system() != "Darwin":
        png, w, h = app.screenshot(scale=2)
        assert png[:8] == b"\x89PNG\r\n\x1a\n", "expected a PNG"
        assert w > 0 and h > 0


def test_the_empty_pane_says_why_and_offers_the_bootstrap(agents_app, mock):
    """**A shed with no roost-session lists no agent sessions** — and the pane
    has to say so.

    That is the whole behavioural change of S6 for this pane: an empty list used
    to mean "nothing launched yet" and now means "there is no session here to
    list". A pane that just said "no agents running" would leave a user unable
    to tell an empty list from a broken one, so the copy names the cause and the
    offer points at where a roost-session gets installed and started.

    Driven on a shed the socket map does NOT cover, so nothing answers on it —
    which is exactly the everyday case (a shed nobody has bootstrapped).

    **`state` is what makes this cell about the right blank.** Four things wear
    an empty pane — still loading, the load failed, every shed is unreachable,
    and a genuine "nothing here" — and only the last offers the bootstrap. Waiting
    on `state == "empty"` rather than on `sessions == []` is what keeps this from
    passing against the first frame, which is `loading`.
    """
    mock.set_sheds([{"name": "unbootstrapped", "status": "running",
                     "backend": "firecracker"}])
    try:
        agents_app.call("sheds.list")
        agents_app.navigate("agents")
        agents_app.wait_until(lambda: agents_app.agents_dump() == [], timeout=20,
                              what="the pane to render no sessions")
        empty = _wait_for(
            agents_app, "the pane to report a genuinely-empty state",
            lambda: (agents_app.agents_empty() or {}).get("state") == "empty"
            and agents_app.agents_empty())
        assert empty["title"] == "No agent sessions", empty
        assert "roost-session" in empty["body"], empty
        assert empty["action"] == "Set up a roost-session", empty
    finally:
        mock.reset()
        agents_app.call("sheds.list")
