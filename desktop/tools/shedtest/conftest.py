"""pytest fixtures for the shed-desktop E2E suite (the macOS app + the Tauri client).

Parameterized by `--target mac|tauri` (default `$SHED_TEST_TARGET`, else `mac`).
A session fixture starts the shared in-process mock shed-server, then launches a
hermetic UI for the chosen target and drives it over its IPC socket. The mac
target adds a fake host-agent (backing the approval gate) + a per-test policy
reset; the Tauri target runs its binary with a throwaway HOME/XDG_RUNTIME_DIR (it
wires the same approval spine, so it also gets the fake host-agent).

Shared tests take the `client` (target-appropriate) + `target` fixtures; the
mac-only suites gate on `SHED_TEST_TARGET`, and the backend-dependent shared
tests carry `@needs_backend` (see `_marks.py`).
"""

from __future__ import annotations

import os
import sys
import tempfile
from pathlib import Path

import pytest

import ui
from client import ShedDesktop, TauriClient
from mockserver import MockShedServer

# Targets whose UI implements the credential-approval spine (mac + tauri). Kept in
# sync with `_marks._APPROVAL_TARGETS`, but defined here as a plain constant —
# importing `_marks` at conftest load (before `pytest_configure` sets
# $SHED_TEST_TARGET) would freeze its markers at the wrong target.
_APPROVAL_TARGETS = {"mac", "tauri"}

sys.path.insert(0, str(Path(__file__).resolve().parents[1] / "fake-host-agent"))
from fake_host_agent import FakeHostAgent  # noqa: E402

FIXTURES = Path(__file__).resolve().parent / "fixtures"
CONFIG = FIXTURES / "config.yaml"


def pytest_addoption(parser):
    parser.addoption(
        "--target", action="store", default=None, choices=list(ui.TARGETS),
        help="which UI to drive (mac|tauri); default $SHED_TEST_TARGET or mac",
    )


def _resolve_target(config) -> str:
    return config.getoption("--target") or os.environ.get("SHED_TEST_TARGET") or "mac"


def pytest_configure(config):
    # Sync the resolved target back to the env BEFORE collection, so the mac-only
    # suites' module-level `@pytest.mark.skipif(... != "mac")` see the CLI flag
    # too (markers evaluate at import time, before any fixture exists).
    os.environ["SHED_TEST_TARGET"] = _resolve_target(config)


@pytest.fixture(scope="session")
def target(pytestconfig) -> str:
    return _resolve_target(pytestconfig)


@pytest.fixture(scope="session")
def mock():
    server = MockShedServer()
    server.start()
    yield server
    server.stop()


@pytest.fixture(scope="session")
def fake():
    """The fake host-agent backing the approval gate. Session-scoped and lazily
    instantiated — only the mac + tauri sessions (which wire the approval spine)
    request it."""
    agent = FakeHostAgent()
    agent.start()
    yield agent
    agent.stop()


@pytest.fixture(scope="session", autouse=True)
def _app_session(mock, target, request):
    ui.quit(target)  # force-quit any running instance; we own a hermetic one
    # Short prefix: on macOS the socket lives under this dir (which is itself under
    # a long /var/folders TMPDIR), and a Unix socket path must stay under SUN_LEN.
    state_dir = Path(tempfile.mkdtemp(prefix=f"shed-e2e-{target}-"))
    # Both mac + tauri gate approvals → a fake host-agent.
    host_agent_socket = (
        request.getfixturevalue("fake").socket_path if target in _APPROVAL_TARGETS else None
    )
    if target == "mac":
        ui.launch(
            "mac",
            mock_base_url=mock.base_url,
            config_path=CONFIG,
            state_dir=state_dir,
            host_agent_socket=host_agent_socket,
        )
    else:
        # tauri is a subprocess target (a binary launched with a throwaway env).
        ui.launch(target, mock_base_url=mock.base_url, config_path=CONFIG,
                  state_dir=state_dir, host_agent_socket=host_agent_socket)
        if target == "tauri":
            # The WebView mounts AFTER `identify`; wait for its first snapshot so no
            # test drives a backend op before the frontend can echo a refresh — the
            # cold-start window sheds.refresh's fast path only best-effort covers.
            c = TauriClient(ui.socket_path("tauri"))
            try:
                c.wait_until(lambda: c.current_pane() is not None, timeout=30,
                             what="tauri frontend ready")
            finally:
                c.close()
    yield
    ui.quit(target)


@pytest.fixture(autouse=True)
def _reset_policy(_app_session, target):
    """Reset to the default prompt+native-auth policy before each approval-capable
    test (policy is app state that persists across tests). Targets without an
    approval spine no-op."""
    if target not in _APPROVAL_TARGETS:
        yield
        return
    c = ui.make_client(target)
    try:
        c.policy_set([{"scope": "default", "action": "prompt", "gate": "biometrics-or-password"}])
    finally:
        c.close()
    yield


@pytest.fixture(autouse=True)
def _reset_mock(mock, target, _app_session):
    """Each test starts from the default served payload. The Tauri app is
    session-scoped and its dashboard.dump reads last-rendered state, so that run
    must ALSO re-sync the dashboard to the reset mock (a prior test's create/
    lifecycle mutation would otherwise leak into dashboard.dump); mac reads live."""
    mock.reset()
    if target == "tauri":
        c = ui.make_client(target)
        try:
            c.sheds_refresh()
        finally:
            c.close()
    yield


@pytest.fixture
def shed(_app_session):
    """The mac app's full client. Used by the mac-only suites (skipped on tauri)."""
    client = ShedDesktop(ui.socket_path("mac"))
    try:
        yield client
    finally:
        client.close()


@pytest.fixture
def tauri(_app_session):
    """The Tauri client. Used by the tauri-only suite (skipped otherwise)."""
    client = TauriClient(ui.socket_path("tauri"))
    try:
        yield client
    finally:
        client.close()


@pytest.fixture
def client(_app_session, target):
    """The target-appropriate IPC client for the shared (cross-target) tests."""
    c = ui.make_client(target)
    try:
        yield c
    finally:
        c.close()
