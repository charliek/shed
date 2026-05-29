"""Pytest configuration and session-scoped fixtures.

Conventions:
    - `vz_server` / `fc_server` are session-scoped fixtures that detect
      backend availability and skip the dependent tests cleanly when the
      environment can't run them.
    - `shed_server` is parameterized across `["vz", "fc"]` via
      `request.getfixturevalue(f"{request.param}_server")` so each
      sub-fixture is only instantiated when actually used.
    - `test_shed_name` allocates a unique per-test name and tears it down
      automatically so a failed test never leaks a shed.

Two environment overrides for non-default setups:
    - `SHED_VZ_SERVER` — entry name in `~/.shed/config.yaml` for the VZ
      server (default: `my-server`).
    - `SHED_FC_HOST`   — SSH host for the FC server (default: `mini3`).
"""

from __future__ import annotations

import os
import re
from pathlib import Path

import pytest

from fixtures.server import LocalServer, RemoteServer


VZ_SERVER_NAME = os.environ.get("SHED_VZ_SERVER", "my-server")
FC_SSH_HOST = os.environ.get("SHED_FC_HOST", "mini3")
FC_SERVER_NAME = os.environ.get("SHED_FC_SERVER", FC_SSH_HOST)

VZ_BREW_LOG = Path("/opt/homebrew/var/log/shed-server.log")


# ----------------------------------------------------------------------------
# Server fixtures (session-scoped — one connection probe per pytest run)
# ----------------------------------------------------------------------------


@pytest.fixture(scope="session")
def vz_server() -> LocalServer:
    """Local VZ shed-server.

    Defaults to the brew-installed `my-server` entry; override with
    `SHED_VZ_SERVER`. Skips if the server can't be reached or if the
    `shed` CLI isn't installed.
    """
    log_path = VZ_BREW_LOG if VZ_BREW_LOG.exists() else None
    s = LocalServer(name=VZ_SERVER_NAME, backend="vz", log_path=log_path)
    if not s.available():
        pytest.skip(
            f"VZ shed-server ({VZ_SERVER_NAME!r}) is not reachable; "
            f"start it with `brew services start shed` or set SHED_VZ_SERVER."
        )
    return s


@pytest.fixture(scope="session")
def fc_server() -> RemoteServer:
    """Remote FC shed-server (default: mini3 over SSH).

    Overrides:
        SHED_FC_HOST   = SSH hostname (default: mini3)
        SHED_FC_SERVER = `~/.shed/config.yaml` entry name (default: same)

    Skips if SSH to the host fails or `shed -s <name> list` returns nonzero.
    """
    s = RemoteServer(
        ssh_host=FC_SSH_HOST,
        name=FC_SERVER_NAME,
        backend="firecracker",
    )
    if not s.available():
        pytest.skip(
            f"FC shed-server (`shed -s {FC_SERVER_NAME}`) is not reachable; "
            f"verify SSH access to {FC_SSH_HOST!r} and that shed-server is "
            f"running there, or set SHED_FC_HOST."
        )
    return s


@pytest.fixture(params=["vz", "fc"])
def shed_server(request):
    """Parameterized across backends.

    `request.getfixturevalue` lazily instantiates only the requested
    sub-fixture, so each backend's skip-on-unavailable triggers only
    when that backend would actually have run.
    """
    return request.getfixturevalue(f"{request.param}_server")


# ----------------------------------------------------------------------------
# Per-test fixtures
# ----------------------------------------------------------------------------


# Shed names allow lowercase letters, digits, and dashes (per ValidateShedName
# in internal/config/types.go). Sanitize node ids to that alphabet.
_SHED_NAME_SAFE = re.compile(r"[^a-z0-9-]+")


@pytest.fixture
def test_shed_name(shed_server, request):
    """Allocate a unique shed name per test, with automatic cleanup.

    Pattern: `itest-<short-test-id>`. The id is sanitized to the shed-name
    alphabet so parameterized variants get distinct names without
    accidentally clashing or producing invalid names.
    """
    raw = request.node.name.lower()
    sanitized = _SHED_NAME_SAFE.sub("-", raw).strip("-")
    name = f"itest-{sanitized}"[:48]
    yield name
    # Teardown is best-effort: a test that died mid-create might leave a
    # half-created shed, but the next run picks up the same name and the
    # server's idempotent delete handles it.
    try:
        shed_server.delete(name, ignore_missing=True)
    except Exception:
        pass
