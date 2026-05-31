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

import hashlib
import os
import re
from pathlib import Path

import pytest

from fixtures.server import LocalServer, RemoteServer


VZ_SERVER_NAME = os.environ.get("SHED_VZ_SERVER", "my-server")
FC_SSH_HOST = os.environ.get("SHED_FC_HOST", "mini3")
FC_SERVER_NAME = os.environ.get("SHED_FC_SERVER", FC_SSH_HOST)

# Where the brew-installed mac shed-server writes its log file. Override
# for non-default Homebrew prefixes (Intel Macs at /usr/local, custom
# installs, etc.) or to point at a different file entirely.
VZ_BREW_LOG = Path(
    os.environ.get("SHED_VZ_LOG_PATH", "/opt/homebrew/var/log/shed-server.log")
)

# Parallel-dev server overrides. Set by `make test-integration-dev` /
# `make test-integration-dev-fc` (or manually) to point the suite at a
# second shed-server running alongside the brew/deb one on a different
# port. When unset, the `vz_server_dev` / `fc_server_dev` / `shed_server_dev`
# fixtures skip cleanly so today's `make test-integration` flow against
# the brew/deb server is unaffected.
VZ_DEV_SERVER_NAME = os.environ.get("SHED_VZ_DEV_SERVER", "")
VZ_DEV_LOG_PATH = os.environ.get("SHED_VZ_DEV_LOG_PATH", "")
FC_DEV_SERVER_NAME = os.environ.get("SHED_FC_DEV_SERVER", "")
FC_DEV_LOG_PATH = os.environ.get("SHED_FC_DEV_LOG_PATH", "")


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
# Parallel-dev server fixtures
# ----------------------------------------------------------------------------
#
# These mirror the brew/deb-targeted fixtures above but read the dev-
# specific env vars (`SHED_VZ_DEV_SERVER`, `SHED_VZ_DEV_LOG_PATH`,
# `SHED_FC_DEV_SERVER`, `SHED_FC_DEV_LOG_PATH`). When those env vars
# aren't set, the dev fixtures skip cleanly — so a developer running
# `make test-integration` (the brew/deb-targeted default) doesn't get
# spurious "dev server unreachable" failures.
#
# Tests that want to run against the dev server use the `shed_server_dev`
# fixture instead of (or in addition to) `shed_server`. The Makefile's
# `test-integration-dev` / `test-integration-dev-fc` targets set both
# the prod env vars (`SHED_VZ_SERVER`, `SHED_FC_SERVER`) AND the
# matching `_DEV_` env vars so a test file can use either fixture
# depending on what it's exercising.


@pytest.fixture(scope="session")
def vz_server_dev() -> LocalServer:
    """Parallel-dev VZ shed-server (Mac, launched via `make dev-server-up`).

    Skips when `SHED_VZ_DEV_SERVER` is unset (the default — no dev
    server expected). When set, behaves like `vz_server` but targets
    the dev server entry name and reads the dev log file path.
    """
    if not VZ_DEV_SERVER_NAME:
        pytest.skip(
            "SHED_VZ_DEV_SERVER not set; this test targets the parallel "
            "dev VZ server. Run `make dev-server-up` then "
            "`make test-integration-dev`, or set SHED_VZ_DEV_SERVER + "
            "SHED_VZ_DEV_LOG_PATH manually."
        )
    log_path = Path(VZ_DEV_LOG_PATH) if VZ_DEV_LOG_PATH else None
    if log_path is not None and not log_path.exists():
        log_path = None
    s = LocalServer(name=VZ_DEV_SERVER_NAME, backend="vz", log_path=log_path)
    if not s.available():
        pytest.skip(
            f"VZ dev shed-server ({VZ_DEV_SERVER_NAME!r}) is not reachable; "
            f"start it with `make dev-server-up`."
        )
    return s


@pytest.fixture(scope="session")
def fc_server_dev() -> RemoteServer:
    """Parallel-dev FC shed-server (remote, launched via `make dev-server-up-fc`).

    Skips when `SHED_FC_DEV_SERVER` is unset (the default — no dev
    server expected). When set, behaves like `fc_server` but targets
    the dev server entry name and reads logs from the remote dev log
    file via `ssh + sudo cat` (not journald — the dev server isn't
    under systemd).
    """
    if not FC_DEV_SERVER_NAME:
        pytest.skip(
            "SHED_FC_DEV_SERVER not set; this test targets the parallel "
            "dev FC server. Run `make dev-server-up-fc` then "
            "`make test-integration-dev-fc`, or set SHED_FC_DEV_SERVER + "
            "SHED_FC_DEV_LOG_PATH manually."
        )
    s = RemoteServer(
        ssh_host=FC_SSH_HOST,
        name=FC_DEV_SERVER_NAME,
        backend="firecracker",
        remote_log_path=FC_DEV_LOG_PATH or None,
    )
    if not s.available():
        pytest.skip(
            f"FC dev shed-server ({FC_DEV_SERVER_NAME!r} on "
            f"{FC_SSH_HOST!r}) is not reachable; start it with "
            f"`make dev-server-up-fc`."
        )
    return s


@pytest.fixture(params=["vz", "fc"])
def shed_server_dev(request):
    """Parallel-dev counterpart of `shed_server`. Parameterized across
    backends; lazily instantiates only the requested sub-fixture and
    skips cleanly when the corresponding dev server isn't configured.
    """
    return request.getfixturevalue(f"{request.param}_server_dev")


# ----------------------------------------------------------------------------
# Per-test fixtures
# ----------------------------------------------------------------------------


# Shed names allow lowercase letters, digits, and dashes (per ValidateShedName
# in internal/config/types.go). Sanitize node ids to that alphabet.
_SHED_NAME_SAFE = re.compile(r"[^a-z0-9-]+")


@pytest.fixture
def test_shed_name(shed_server, request):
    """Allocate a unique shed name per test, with automatic cleanup.

    Pattern: `itest-<sanitized-prefix>-<6-char-hash>`. The hash is over
    the full pytest nodeid (including parameterization), so two tests
    whose names collapse to the same sanitized prefix get distinct
    names — and a single test always gets the same name across runs
    (helpful when a previous run leaked state).
    """
    raw = request.node.nodeid
    sanitized = _SHED_NAME_SAFE.sub("-", raw.lower()).strip("-")
    suffix = hashlib.sha256(raw.encode()).hexdigest()[:6]
    # Budget: "itest-" (6) + dashes (2) + suffix (6) = 14 chars of overhead
    # against the 48-char shed-name ceiling, leaving 34 for the prefix.
    prefix = sanitized[:34].rstrip("-")
    name = f"itest-{prefix}-{suffix}"
    yield name
    # Teardown is best-effort. A test that died mid-create might leave a
    # half-created shed; ignore_missing=True keeps cleanup from masking
    # the original failure.
    try:
        shed_server.delete(name, ignore_missing=True)
    except Exception:
        pass
