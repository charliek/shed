"""Pytest configuration and session-scoped fixtures.

Conventions:
    - `vz_server` / `fc_server` are session-scoped fixtures that detect
      backend availability and skip the dependent tests cleanly when the
      environment can't run them.
    - `shed_server` is parameterized across `["vz", "fc", "remote"]` via
      `request.getfixturevalue(f"{request.param}_server")` so each
      sub-fixture is only instantiated when actually used.
    - `test_shed_name` allocates a unique per-test name and tears it down
      automatically so a failed test never leaks a shed.

Two environment overrides for non-default setups:
    - `SHED_VZ_SERVER` — entry name in `~/.shed/config.yaml` for the VZ
      server (default: `my-server`). Set empty, the `[vz]` cases skip
      (`make test-integration-dev-fc` does this).
    - `SHED_FC_HOST`   — SSH host for the FC server (default: `mini3`).

The `remote` parameter (a server running `default_backend: remote`, where
each shed is a cloud VM) is opt-in: it skips unless `SHED_REMOTE_SERVER`
names a `~/.shed/config.yaml` entry (`make test-integration-remote` sets it).
A test the remote backend cannot run carries
`@pytest.mark.local_vm_only("<reason>")` and skips on `[remote]` with
that reason, before any server is probed.
"""

from __future__ import annotations

import hashlib
import os
import re
import sys
from pathlib import Path

import pytest

from fixtures.identity import (
    itest_source,
    skip_reason_under_directory_source,
    skip_unless_users_source_ready,
    users_environment,
)
from fixtures.server import CloudServer, LocalServer, RemoteServer


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

# Remote-file log path for the PROD `fc_server` fixture. Defaults to
# unset, in which case `fc_server` reads logs via `ssh + sudo journalctl
# -u shed-server` (the deb-installed shed-server's systemd-captured
# logs). `make test-integration-dev-fc` sets this to the dev server's
# remote log file so the existing fc_server-using tests can read
# PhaseTimer lines from the file (the dev FC server runs via sudo
# nohup, not systemd, so journald has no records for it).
FC_LOG_PATH = os.environ.get("SHED_FC_LOG_PATH", "")

# The remote-backend server (`default_backend: remote`; each shed is a billed
# cloud VM). Unset by default, so `make test-integration` never creates a
# cloud VM: every `[remote]` case skips. `make test-integration-remote` sets
# the entry name (default `remote-dev`) and the dev server's log file.
# `SHED_REMOTE_IMAGE`, when set, is the image alias every create asks for;
# unset, creates boot the server's `remote.default_image`.
REMOTE_SERVER_NAME = os.environ.get("SHED_REMOTE_SERVER", "")
REMOTE_LOG_PATH = os.environ.get(
    "SHED_REMOTE_LOG_PATH", str(Path.home() / ".shed" / "dev-remote" / "server.log")
)
REMOTE_IMAGE = os.environ.get("SHED_REMOTE_IMAGE", "")
# `make test-integration-compute` sets it: the `[remote]` server is a private
# compute world on this host (fixtures/directory.py: compute_world), no cloud.
REMOTE_HERMETIC = os.environ.get("SHED_REMOTE_HERMETIC", "") == "1"


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
    if not VZ_SERVER_NAME:
        # Set empty on purpose (`make test-integration-dev-fc` does): no VZ
        # server for this run. Never probe with an empty name, which `shed
        # -s ""` would resolve to the client's default server.
        pytest.skip("SHED_VZ_SERVER is empty: no VZ server for this run.")
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
        SHED_FC_LOG_PATH = remote file path to read logs from. When set,
                           the fixture uses `ssh + sudo -n tail -c +N`
                           against this file instead of journalctl.
                           `make test-integration-dev-fc` sets this to
                           the dev FC server's log file (the dev server
                           runs via sudo nohup, not systemd, so journald
                           has no records for it).

    Skips if SSH to the host fails or `shed -s <name> list` returns nonzero.
    """
    if not FC_SERVER_NAME:
        # Same as vz_server: an empty name means no FC server, never the
        # client's default server.
        pytest.skip("SHED_FC_SERVER is empty: no FC server for this run.")
    s = RemoteServer(
        ssh_host=FC_SSH_HOST,
        name=FC_SERVER_NAME,
        backend="firecracker",
        remote_log_path=FC_LOG_PATH or None,
    )
    if not s.available():
        pytest.skip(
            f"FC shed-server (`shed -s {FC_SERVER_NAME}`) is not reachable; "
            f"verify SSH access to {FC_SSH_HOST!r} and that shed-server is "
            f"running there, or set SHED_FC_HOST."
        )
    return s


@pytest.fixture(scope="session")
def hermetic_remote_world():
    """The `[remote]` server of a hermetic run (`SHED_REMOTE_HERMETIC=1`,
    `make test-integration-compute`): one compute world for the session (a
    private reference service and shed-server from bin/, the local link, no
    cloud), listing the itest people (`fixtures.identity.directory_users`)."""
    from fixtures.directory import compute_skip_reason, compute_world
    from fixtures.identity import directory_users, ensure_keys

    why = compute_skip_reason()
    if why:
        pytest.skip(why)
    with compute_world() as w:
        for u in directory_users(ensure_keys()):
            w.users[u["name"]] = u
        w.publish()
        w.refresh_ok()
        yield w


@pytest.fixture(scope="session")
def remote_server(request) -> CloudServer:
    """A shed-server running `default_backend: remote` (each shed is a
    cloud VM), reached through the local CLI as its local admin (the
    developer's own entry).

    Skips unless `SHED_REMOTE_SERVER` names a `~/.shed/config.yaml` entry,
    so no run creates a cloud VM by accident. `make test-integration-remote`
    sets it (default `remote-dev`) together with `SHED_REMOTE_LOG_PATH`.
    Under `SHED_REMOTE_HERMETIC=1` it is the hermetic world's server instead,
    as its local admin.
    """
    if REMOTE_HERMETIC:
        from fixtures.directory import ENTRY

        w = request.getfixturevalue("hermetic_remote_world")
        return CloudServer(name=ENTRY, log_path=w.server.log_path, env=w.admin.env(), home=Path(w.admin.home))
    if not REMOTE_SERVER_NAME:
        pytest.skip(
            "SHED_REMOTE_SERVER not set: no remote-backend server for this run "
            "(each remote shed is a billed cloud VM; run "
            "`make test-integration-remote` to opt in)."
        )
    log_path = Path(REMOTE_LOG_PATH) if REMOTE_LOG_PATH else None
    s = CloudServer(
        name=REMOTE_SERVER_NAME,
        log_path=log_path if log_path is not None and log_path.exists() else None,
        image=REMOTE_IMAGE or None,
    )
    if not s.available():
        pytest.skip(
            f"remote-backend shed-server ({REMOTE_SERVER_NAME!r}) is not "
            f"reachable; start it with `make dev-server-up-remote`."
        )
    return s


@pytest.fixture(scope="session")
def remote_compute(remote_server) -> bool:
    """Whether the remote server takes its machines from the account
    service (plan 041: `/api/info` lists `lease`). Its local admins create no
    cloud shed there."""
    from fixtures.compute import FEATURE_LEASE

    return FEATURE_LEASE in remote_server.features()


@pytest.fixture(scope="session")
def remote_person(remote_server, remote_compute):
    """Who the shared `[remote]` cases act as: on a server whose machines
    come from the account service (`remote_compute`), the service's person
    `itest-alice` (an identity of its own, with the itest key; a local admin
    creates no cloud shed); on any other remote server, the developer's
    entry (`remote_server`).

    Against the reference service (`SHED_REMOTE_SERVICE=ref`) alice is put in
    its users.json first; against any other service she must already be on
    its list (the case skips, saying so, otherwise)."""
    if not remote_compute:
        yield remote_server
        return
    from fixtures.compute import list_itest_people, person_identity, unlisted_people
    from fixtures.identity import ALICE
    from fixtures.server import resolve_server_entry

    if not REMOTE_HERMETIC and os.environ.get("SHED_REMOTE_SERVICE", "").strip() == "ref":
        ref_dir = Path(os.environ.get("SHED_COMPUTE_REF_DIR", "") or Path.home() / ".shed" / "dev-remote" / "ref")
        users = ref_dir / "control" / "users.json"
        if users.parent.is_dir():
            list_itest_people(users)
    if unlisted_people(remote_server, [ALICE]):
        pytest.skip(f"{remote_server.name!r} takes its machines from the account service, whose list lacks "
                    f"{ALICE}; the shared [remote] cases act as {ALICE} there (a local admin creates no cloud "
                    f"shed): list the itest people (`uv run python -m fixtures.identity users-yaml`)")
    entry = resolve_server_entry(remote_server.name, env=remote_server.env) or {}
    ident = person_identity(ALICE, remote_server.name, entry.get("host", "localhost"), int(entry.get("ssh_port") or 0))
    try:
        ident.add_server()
        yield remote_server.as_person(ident)
    finally:
        ident.remove()


@pytest.fixture(params=["vz", "fc", "remote"])
def shed_server(request):
    """Parameterized across backends.

    `request.getfixturevalue` lazily instantiates only the requested
    sub-fixture, so each backend's skip-on-unavailable triggers only
    when that backend would actually have run.

    On `[remote]`, a test marked `local_vm_only(reason)` skips with
    that reason before the remote server is probed.
    """
    if request.param == "remote":
        marker = request.node.get_closest_marker("local_vm_only")
        if marker is not None:
            reason = marker.args[0] if marker.args else "not supported"
            pytest.skip(f"not on the remote backend: {reason}")
    if request.param == "remote":
        # On a compute server, the service's person (a local admin creates
        # no cloud shed there).
        return request.getfixturevalue("remote_person")
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

    Fails (not skips) when `SHED_VZ_DEV_SERVER` is set but
    `SHED_VZ_DEV_LOG_PATH` is empty: the suite's PhaseTimer-dependent
    tests need a log path, and silently falling back to journald
    (which Mac launchd-less dev servers don't write to) would produce
    confusing "no PhaseTimer found" skips deep in the suite.
    """
    if not VZ_DEV_SERVER_NAME:
        pytest.skip(
            "SHED_VZ_DEV_SERVER not set; this test targets the parallel "
            "dev VZ server. Run `make dev-server-up` then "
            "`make test-integration-dev`, or set SHED_VZ_DEV_SERVER + "
            "SHED_VZ_DEV_LOG_PATH manually."
        )
    if not VZ_DEV_LOG_PATH:
        pytest.fail(
            "SHED_VZ_DEV_SERVER is set but SHED_VZ_DEV_LOG_PATH is "
            "empty. The parallel dev VZ shed-server writes its log "
            "to a known file (typically ~/.shed/dev/server.log); set "
            "SHED_VZ_DEV_LOG_PATH to that path. Without it, the "
            "PhaseTimer-dependent tests would skip with a confusing "
            "'no PhaseTimer found' reason."
        )
    log_path = Path(VZ_DEV_LOG_PATH)
    s = LocalServer(
        name=VZ_DEV_SERVER_NAME,
        backend="vz",
        log_path=log_path if log_path.exists() else None,
    )
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

    Fails (not skips) when `SHED_FC_DEV_SERVER` is set but
    `SHED_FC_DEV_LOG_PATH` is empty: the dev FC server runs via `sudo
    nohup` (no systemd unit), so journald has no records and the
    fixture's PhaseTimer fetch needs the file path.
    """
    if not FC_DEV_SERVER_NAME:
        pytest.skip(
            "SHED_FC_DEV_SERVER not set; this test targets the parallel "
            "dev FC server. Run `make dev-server-up-fc` then "
            "`make test-integration-dev-fc`, or set SHED_FC_DEV_SERVER + "
            "SHED_FC_DEV_LOG_PATH manually."
        )
    if not FC_DEV_LOG_PATH:
        pytest.fail(
            "SHED_FC_DEV_SERVER is set but SHED_FC_DEV_LOG_PATH is "
            "empty. The parallel dev FC shed-server runs via "
            "`sudo nohup` (not systemd), so journald has no records "
            "for it; the fixture reads its log file via "
            "`ssh + sudo -n tail`. Set SHED_FC_DEV_LOG_PATH to the "
            "remote file path (typically /tmp/shed-server-dev.log)."
        )
    s = RemoteServer(
        ssh_host=FC_SSH_HOST,
        name=FC_DEV_SERVER_NAME,
        backend="firecracker",
        remote_log_path=FC_DEV_LOG_PATH,
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
# Named users (auth.users): several identities against one dev server
# ----------------------------------------------------------------------------
#
# `users_env` is a dev server running with the itest user list plus the test
# identities enrolled against it (fixtures/identity.py): `itest-alice` (user
# A, with a script key and nothing else), `itest-bob` (user B, with an
# expired second key), `itest-admin` (an admin), each with its own key, CLI
# config directory and client certificate. Module-scoped, so one module's
# cases share one enrolment (and, on vz, one restart).
#
#   [vz]     the Mac dev server (`SHED_VZ_DEV_SERVER`), always restarted with
#            the user list through devcontrol.dev_config() and restored to
#            the committed base afterwards; the developer's credential
#            source bindings are moved aside, with the server down, for the
#            detour and put back before the base starts (devcontrol:
#            BindingsDetour). (Other modules' dev_config() calls
#            restore the base too, so a Mac dev server started with the list
#            would not still have it by the time this module runs.)
#   [fc]     the FC dev server (`SHED_FC_DEV_SERVER`). The suite cannot
#            reconfigure it: it must have been STARTED with the user list,
#            and `SHED_ITEST_USERS` must list `fc`; otherwise it skips saying
#            what to set.
#   [remote] the remote dev server (`SHED_REMOTE_SERVER`), likewise with
#            `remote` in `SHED_ITEST_USERS`. Every shed there is a billed VM:
#            a users case the remote backend need not run carries
#            `@pytest.mark.local_vm_only(reason)` and skips on [remote]
#            before any fixture runs (pytest_runtest_setup below).
#
# Under the directory source (SHED_ITEST_SOURCE=directory) the run started
# the server with `python -m fixtures.identity render-directory`'s auth block
# and the reference server with its users.json: no backend is reconfigured or
# asked for SHED_ITEST_USERS, and a case marked
# `not_under_directory_source(reason)` skips before any fixture runs.


def skip_vz_off_macos() -> None:
    """The VZ dev server runs on macOS only, and the [vz] users cases
    restart it (`make dev-server-restart`, macOS-only). On any other host they
    skip, even when a VZ entry is configured and reachable (it would be
    another machine's server, or a production one)."""
    if sys.platform != "darwin":
        pytest.skip(f"[vz] users cases restart the Mac dev server; this host is {sys.platform}, not macOS")


@pytest.fixture(scope="module", params=["vz", "fc", "remote"])
def users_env(request):
    backend = request.param
    try:
        source = itest_source()
    except ValueError as e:
        pytest.fail(str(e))
    if backend == "vz":
        skip_vz_off_macos()
        server = request.getfixturevalue("vz_server_dev")
        # The Mac dev server is restarted with the itest list, except under
        # the directory source, where the run started it so.
        reconfigure, image = source == "config", "base"
    elif backend == "fc":
        server = request.getfixturevalue("fc_server_dev")
        skip_unless_users_source_ready("fc", server.name, source)
        reconfigure, image = False, "base"
    else:
        server = request.getfixturevalue("remote_server")
        if request.getfixturevalue("remote_compute"):
            # Its people come from the account service (auth.directory).
            source = "directory"
        skip_unless_users_source_ready("remote", server.name, source)
        reconfigure, image = False, server.image
    with users_environment(backend, server, reconfigure=reconfigure, image=image, source=source) as env:
        yield env


@pytest.hookimpl(tryfirst=True)
def pytest_runtest_setup(item):
    """Before any fixture of a users case runs: a [vz] case skips on a host
    that is not macOS (`skip_vz_off_macos`), and a case marked
    `local_vm_only` skips on [remote] (a module-scoped shed fixture would
    otherwise create a billed VM first). `shed_server` does the same for its
    own parameter. A case marked `not_under_directory_source(reason)` skips
    under the directory source."""
    marker = item.get_closest_marker("real_guest")
    if marker is not None and REMOTE_HERMETIC:
        callspec = getattr(item, "callspec", None)
        params = callspec.params if callspec is not None else {}
        on_remote = (params.get("shed_server") == "remote" or params.get("users_env") == "remote"
                     or not params)
        if on_remote:
            pytest.skip("needs a real guest; the hermetic compute world's machines are shed-agents on this "
                        f"host: {marker.args[0] if marker.args else 'not supported'}")
    marker = item.get_closest_marker("not_under_directory_source")
    if marker is not None:
        why = skip_reason_under_directory_source(marker.args[0] if marker.args else "not supported")
        if why:
            pytest.skip(why)
    callspec = getattr(item, "callspec", None)
    if callspec is None:
        return
    if callspec.params.get("users_env") == "vz":
        skip_vz_off_macos()
    if callspec.params.get("users_env") != "remote":
        return
    marker = item.get_closest_marker("local_vm_only")
    if marker is not None:
        reason = marker.args[0] if marker.args else "not supported"
        pytest.skip(f"not on the remote backend: {reason}")


# ----------------------------------------------------------------------------
# Per-test fixtures
# ----------------------------------------------------------------------------


# Shed names allow lowercase letters, digits, and dashes (per ValidateShedName
# in internal/config/types.go). Sanitize node ids to that alphabet.
_SHED_NAME_SAFE = re.compile(r"[^a-z0-9-]+")


def _gen_shed_name(request) -> str:
    """Compute the canonical per-test shed name from a pytest request.

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
    return f"itest-{prefix}-{suffix}"


@pytest.fixture
def test_shed_name(shed_server, request):
    """Per-test shed name with automatic cleanup on the PROD shed-server.

    Tests using the `shed_server` fixture (brew/deb-installed
    shed-server) pair with this one. The teardown calls
    `shed_server.delete(name)`.

    Tests targeting the parallel-dev server must use `test_shed_name_dev`
    instead — using `test_shed_name` with `shed_server_dev` would
    silently instantiate BOTH the prod and dev backends AND clean up
    against the wrong server. The two name fixtures are intentionally
    separate so a misconfigured test fails at collection-time rather
    than running half-correctly.
    """
    name = _gen_shed_name(request)
    yield name
    # Teardown is best-effort. A test that died mid-create might leave a
    # half-created shed; ignore_missing=True keeps cleanup from masking
    # the original failure.
    try:
        shed_server.delete(name, ignore_missing=True)
    except Exception:
        pass


@pytest.fixture
def test_shed_name_dev(shed_server_dev, request):
    """Per-test shed name with automatic cleanup on the DEV shed-server.

    Mirror of `test_shed_name` for tests targeting the parallel-dev
    shed-server (the `shed_server_dev` fixture). Keeps the two
    server-target paths cleanly separated — see `test_shed_name`'s
    docstring for the reasoning.
    """
    name = _gen_shed_name(request)
    yield name
    try:
        shed_server_dev.delete(name, ignore_missing=True)
    except Exception:
        pass
