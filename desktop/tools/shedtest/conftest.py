"""pytest fixtures for the shed-desktop E2E suite (the Tauri client, macOS + Linux).

A session fixture starts the shared in-process mock shed-server, then launches
the Tauri binary hermetically — test mode, pointed at the mock, with a throwaway
HOME/XDG_RUNTIME_DIR (`hermetic.py`) — and drives it over its IPC socket. The app
is this machine's provisioning source: it holds a delivery stream on the mock.
Tests take the `client` or `tauri` fixture (the same `TauriClient`, two names) or
launch a self-managed instance of their own.
"""

from __future__ import annotations

import os
import tempfile
from pathlib import Path

import pytest

import ui
from client import scaled_timeout
from mockserver import MockShedServer

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


@pytest.fixture(scope="session", autouse=True)
def _app_session(mock):
    """Launch the session-scoped hermetic Tauri app and tear it down at session
    end; yields this machine's id (the one its delivery stream carries). Every module's
    default `client`/`tauri` fixtures ride this one instance (a suite that needs
    its own, e.g. a `roost_sockets` map per test, replaces it with a
    self-managed instance instead — see `test_agents.py`).

    After launch the app's delivery stream must be open on the mock before any
    test runs.

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
    ui.launch(mock_base_url=mock.base_url, config_path=CONFIG, state_dir=state_dir,
              roost_sockets=_env_roost_sockets())
    # The WebView mounts AFTER `identify`; wait for its first snapshot so no test
    # drives a backend op before the frontend can echo a refresh — the cold-start
    # window sheds.refresh's fast path only best-effort covers.
    c = ui.make_client()
    try:
        c.wait_until(lambda: c.current_pane() is not None, timeout=30,
                     what="tauri frontend ready")
        machine_id = (c.call("delivery.status").get("machine") or {}).get("id")
        assert machine_id, "the session app has no machine identity"
    finally:
        c.close()
    mock.live_delivery_stream(timeout=scaled_timeout(30))
    yield machine_id
    ui.quit()


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
