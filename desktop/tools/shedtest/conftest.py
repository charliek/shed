"""pytest fixtures for the shed-desktop E2E suite (the Tauri client, macOS + Linux).

A session fixture starts the shared in-process mock shed-server and a second,
bus-only mock, then launches the Tauri binary hermetically — test mode, pointed
at the mock, with a throwaway HOME/XDG_RUNTIME_DIR — and drives it over its IPC
socket. The app brokers credentials in process: its broker (written by
`broker_env.py`) serves the two mocks as `mini3` and `studio`, and the approval
cells push requests on their plugin buses (`bus_stimulus.py`). Tests take the
`client` or `tauri` fixture (the same `TauriClient`, two names) or launch a
self-managed instance of their own.
"""

from __future__ import annotations

import os
import tempfile
import time
from pathlib import Path

import pytest

import broker_env
import ui
from client import ShedError, scaled_timeout
from mockserver import MockShedServer

#: The session broker's names for the two mocks it serves.
SESSION_SERVER = "mini3"
STUDIO_SERVER = "studio"

FIXTURES = Path(__file__).resolve().parent / "fixtures"
CONFIG = FIXTURES / "config.yaml"


def _env_roost_sockets() -> dict[str, str] | None:
    """Parse `SHEDTEST_ROOST_SOCKETS` (`name=path,name=path,...`) into the map
    `ui.launch`'s `roost_sockets` wants, or `None` if unset/empty.

    This is a harness-level opt-in, distinct from the app-level
    `<PREFIX>_ROOST_SOCKETS` env var that `ui.subproc_env` sets on the child: the
    app-level var is always set-or-cleared per launch so a hermetic run never
    inherits a value from the parent shell (see `ui.py`'s `subproc_env`). Driving
    the SESSION app against a real `roost-session` daemon (the
    `.claude/skills/shedtest-linux` recipe) instead exports this var into the
    harness process, and the `_app_session` fixture below reads it once and
    passes it explicitly to `ui.launch` — an opt-in the hermetic default test
    run never sees.
    """
    raw = os.environ.get("SHEDTEST_ROOST_SOCKETS")
    if not raw:
        return None
    sockets: dict[str, str] = {}
    for entry in raw.split(","):
        entry = entry.strip()
        if not entry:
            continue
        name, _, path = entry.partition("=")
        if not name or not path:
            raise ValueError(f"malformed SHEDTEST_ROOST_SOCKETS entry {entry!r} (want name=path)")
        sockets[name] = path
    return sockets or None


def pytest_configure(config):
    # Declared here rather than in pyproject so the harness stays self-contained
    # and a `--strict-markers` run does not fail on it.
    config.addinivalue_line(
        "markers",
        "real_roost: drives a REAL roost-session daemon; runs only when "
        "$SHED_TAURI_ROOST_SESSION_BIN names an executable (CI has none)",
    )


@pytest.fixture(scope="session")
def mock():
    server = MockShedServer()
    server.start()
    yield server
    server.stop()


@pytest.fixture(scope="session")
def studio():
    """The second, bus-only mock: the session broker serves it as `studio`, so a
    request pushed here comes from a different server than one on `mock`. It is
    not in the app's own shed config, so it never appears in the shed list."""
    server = MockShedServer()
    server.start()
    yield server
    server.stop()


@pytest.fixture(scope="session", autouse=True)
def _app_session(mock, studio):
    """Launch the session-scoped hermetic Tauri app and tear it down at session
    end; yields the session broker's `broker_env.BrokerInfo`. Every module's
    default `client`/`tauri` fixtures ride this one instance (a suite that needs
    its own, e.g. a `roost_sockets` map per test, replaces it with a
    self-managed instance instead — see `test_agents.py`).

    The app's broker serves `mock` as `mini3` and `studio` as `studio`, for all
    three namespaces. After launch `broker_env.check_running` holds the session
    to exactly those two URLs, and both mocks must see the three subscribes
    before any test runs.

    `roost_sockets` is sourced from `SHEDTEST_ROOST_SOCKETS`
    (`name=path,name=path,...`, parsed by `_env_roost_sockets`) — an explicit,
    harness-level opt-in for driving this session app against a REAL
    `roost-session` daemon instead of `fake_roost.py` (see
    `.claude/skills/shedtest-linux`'s "Driving against a roost-session" recipe).
    Unset by default, so the default hermetic run never touches a real daemon
    and the app's own `SHED_TAURI_ROOST_SOCKETS` env var stays cleared.
    """
    ui.quit()  # we own a hermetic instance
    # Short prefix: on macOS the socket lives under this dir (which is itself under
    # a long /var/folders TMPDIR), and a Unix socket path must stay under SUN_LEN.
    state_dir = Path(tempfile.mkdtemp(prefix="shed-e2e-tauri-"))
    # `roost_sockets` is the harness-level `SHEDTEST_ROOST_SOCKETS` opt-in (see
    # `_env_roost_sockets`) — unset in the default hermetic run, so the app's
    # `SHED_TAURI_ROOST_SOCKETS` stays cleared.
    broker = broker_env.write_hermetic_broker(
        state_dir, {SESSION_SERVER: mock.base_url, STUDIO_SERVER: studio.base_url})
    info = broker[1]
    ui.launch(mock_base_url=mock.base_url, config_path=CONFIG, state_dir=state_dir,
              broker=broker, roost_sockets=_env_roost_sockets())
    # The WebView mounts AFTER `identify`; wait for its first snapshot so no test
    # drives a backend op before the frontend can echo a refresh — the cold-start
    # window sheds.refresh's fast path only best-effort covers.
    c = ui.make_client()
    try:
        c.wait_until(lambda: c.current_pane() is not None, timeout=30,
                     what="tauri frontend ready")
        status = broker_env.check_running(c, info)
        info.machine_id = (status.get("machine") or {}).get("id")
    finally:
        c.close()
    for server in (mock, studio):
        for ns in broker_env.ALL_NAMESPACES:
            server.wait_for_subscribe(ns, timeout=scaled_timeout(30))
    yield info
    ui.quit()


@pytest.fixture
def broker(_app_session):
    """The session broker's `broker_env.BrokerInfo` (its audit file, its key)."""
    return _app_session


@pytest.fixture(autouse=True)
def _session_leak_guard(request, _app_session, mock, studio):
    """After each test, every subscribe the two session mocks saw DURING that test
    came from the session app's own broker. A self-managed launch subscribes on
    a private mock or nowhere (`broker_env.write_hermetic_broker(..., {})`); one
    that subscribed on a session mock would take a listener slot the session
    broker needs and answer the session's requests with its own keys. Only the
    test's own slice of the mocks' history is checked (a mark taken before it
    runs), so a stray subscriber fails its own test, not every later one; the
    session-wide record is the terminal summary's leak-guard line."""
    servers = ((SESSION_SERVER, mock), (STUDIO_SERVER, studio))
    marks = {(name, ns): len(server.subscribe_identities(ns))
             for name, server in servers for ns in broker_env.ALL_NAMESPACES}
    yield
    want = _app_session.machine_id
    for name, server in servers:
        for ns in broker_env.ALL_NAMESPACES:
            during = server.subscribe_identities(ns)[marks[(name, ns)]:]
            others = sorted({str(mid) for mid, _ in during if mid != want})
            if others:
                _LEAKS.append(f"{request.node.nodeid}: {ns} on {name} from {others}")
            assert not others, (
                f"a launch other than the session app subscribed {ns} on the session "
                f"mock {name} during this test: machine ids {others}")


#: Every stray subscribe the leak guard caught this session (the summary line).
_LEAKS: list[str] = []


def pytest_terminal_summary(terminalreporter):
    terminalreporter.write_line(
        f"session leak guard: {len(_LEAKS)} stray subscribe(s) on the session mocks"
        + "".join(f"\n  {leak}" for leak in _LEAKS))


@pytest.fixture(autouse=True)
def _reset_policy(_app_session):
    """Reset to the default prompt+native-auth policy before each test (policy is
    app state that persists across tests), and deny any approval an earlier test
    left pending: the broker answers one request per namespace and server at a
    time, so a leftover would hold the next test's request until it timed out."""
    c = ui.make_client()
    try:
        c.policy_set([{"scope": "default", "action": "prompt", "gate": "biometrics-or-password"}])
        # Denying one leftover lets the broker hand the app the next request it
        # held, so repeat until nothing is pending (or fail, saying what is).
        deadline = time.monotonic() + scaled_timeout(10)
        while leftovers := c.approvals_list():
            if time.monotonic() >= deadline:
                pytest.fail(f"approvals still pending after denying leftovers: {leftovers}")
            for leftover in leftovers:
                try:
                    c.approval_decide(leftover["id"], "deny")
                except ShedError as e:
                    # Only "no such approval" (it expired, or a policy decided it
                    # meanwhile) is harmless; anything else means the cleanup failed.
                    if e.code not in ("not-found", "not_found"):
                        pytest.fail(f"could not deny the leftover approval {leftover}: {e}")
            time.sleep(0.2)
    finally:
        c.close()
    yield


@pytest.fixture(autouse=True)
def _reset_mock(mock, _app_session):
    """Each test starts from the default served payload. The app is
    session-scoped and its dashboard.dump reads last-rendered state, so the
    dashboard is ALSO re-synced to the reset mock (a prior test's create/
    lifecycle mutation would otherwise leak into dashboard.dump)."""
    mock.reset()
    c = ui.make_client()
    try:
        c.sheds_refresh()
    finally:
        c.close()
    yield


@pytest.fixture
def tauri(_app_session):
    """The session app's IPC client."""
    client = ui.make_client()
    try:
        yield client
    finally:
        client.close()


@pytest.fixture
def client(_app_session):
    """The session app's IPC client (the same as `tauri`; the shared suites'
    name for it)."""
    c = ui.make_client()
    try:
        yield c
    finally:
        c.close()
