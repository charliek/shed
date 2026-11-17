"""Sheds that predate a user list: the one-time upgrade step and `shed assign`.

A shed made before a server had `auth.users` carries no user. On the first
start with a user list:

  - with exactly one user, every such shed becomes that user's;
  - with several, none does: the shed is admin-only until an admin gives it
    to a user with `shed assign <shed> <user>`, which refuses a shed that
    carries host directories or server mounts.

The step runs once (`<state_dir>/users-upgrade.json`), so cutting a list
from two users to one later hands nobody the unassigned sheds. A start with
no user list removes the marker. The Mac dev server's `state_dir` is its own
(`~/.shed/dev/state`, read from the committed config), never the installed
server's `~/.shed`: the cases check that the marker appears and goes there,
and that `~/.shed/users-upgrade.json` is not created, changed or removed.

These cases restart a server with another config, which the suite can do
only to the Mac dev server (`devcontrol.dev_config()`, which always restores
the committed base afterwards). They run on [vz] only, on macOS only (they
skip on any other host before a fixture runs, even with a VZ entry
configured); [fc] and [remote] have no case here (`vz_server_dev` skips when
`SHED_VZ_DEV_SERVER` is empty). Each case checks that its target is a dev
server it may restart (`devcontrol.assert_dev_target`) BEFORE it creates
anything, so a dev entry name that points somewhere else gets no shed.

**Side effect on the dev server.** The one-user restart hands EVERY shed on
the Mac dev server that has no user (not only this module's) to the user
`dev`, which is the developer's own key (`fixtures/identity.py`). That is
harmless there: on the committed base (no user list) every shed is reachable
whatever user it records, and on a later restart with the itest list the
developer is `dev`.
"""

from __future__ import annotations

import json
import sys
import uuid
from pathlib import Path
from typing import Optional

import pytest

from fixtures.devcontrol import USERS_UPGRADE_FILE, assert_dev_target, dev_config, dev_state_dir, installed_state_dirs
from fixtures.identity import ADMIN, ALICE, BOB, DEV_USER, users_environment, with_itest_users
from fixtures.tlsclient import error_code

# Under the directory source (SHED_ITEST_SOURCE=directory) the run started
# the Mac dev server with a directory config; these cases restart it with
# their own auth.users configs and restore the committed base, which would
# replace that config, so they do not run there.
NOT_WITH_A_DIRECTORY = ("restarts the Mac dev server with its own auth.users configs and restores the committed "
                        "base, which would replace the directory config the run started it with")

pytestmark = [
    pytest.mark.vz,
    pytest.mark.slow,
    # A mark, not a fixture, so it skips before any fixture runs.
    pytest.mark.skipif(sys.platform != "darwin", reason="restarts the Mac dev server: macOS only"),
    pytest.mark.not_under_directory_source(NOT_WITH_A_DIRECTORY),
]


def _dev_target(server) -> None:
    """Refuse (raise) unless `server` is a dev server these cases may
    restart. Called first in every case, before its first create: the
    restart's own check (inside dev_config) would come only after sheds were
    made on whatever the entry points at."""
    assert_dev_target(server.name)


def _cleanup(server, *names: str) -> None:
    """Delete every name (each verified gone, `ensure_deleted`), trying them
    all before failing with every failure."""
    failures = []
    for name in names:
        try:
            server.ensure_deleted(name)
        except AssertionError as e:
            failures.append(str(e))
    if failures:
        raise AssertionError("\n".join(failures))


def _create(server, name: str, *, mounts: Optional[str]) -> None:
    args = ["create", name, "--image", "base", "--no-sync"]
    if mounts is not None:
        args += ["--mounts", mounts]
    r = server.cli(*args, timeout=300)
    assert r.returncode == 0, f"shed create {name} failed: {r.stdout[-800:]!r} {r.stderr[-800:]!r}"


def _user_of(server, name: str) -> Optional[str]:
    """The user the developer's CLI reports for `name` (None: no user)."""
    r = server.cli("--json", "list", timeout=60)
    assert r.returncode == 0, r.stderr
    found = [s for s in json.loads(r.stdout or "[]") if s.get("name") == name]
    assert found, f"{name} is not listed: {r.stdout[-800:]!r}"
    return found[0].get("user") or None


def _dev_marker() -> Path:
    """The Mac dev server's upgrade marker, in the state_dir of the config it
    runs (the committed base, and every dev_config() detour, which keeps it)."""
    return dev_state_dir() / USERS_UPGRADE_FILE


def _installed_marker() -> Optional[tuple[int, int, bytes]]:
    """The installed server's marker (`~/.shed/users-upgrade.json`) as it is
    now: (mtime, size, content), or None when there is none. Read only."""
    path = installed_state_dirs()[0] / USERS_UPGRADE_FILE
    try:
        st = path.stat()
        return st.st_mtime_ns, st.st_size, path.read_bytes()
    except FileNotFoundError:
        return None


def _only(names):
    """A dev_config transform: the itest user list with only `names` beside
    the developer's own user `dev`."""
    return lambda cfg: with_itest_users(cfg, names=tuple(names))


def test_a_one_user_list_gives_that_user_the_sheds_that_had_none(vz_server_dev):
    server = vz_server_dev
    _dev_target(server)
    plain, mounted = f"itest-uu1-{uuid.uuid4().hex[:6]}", f"itest-uu1m-{uuid.uuid4().hex[:6]}"
    try:
        _create(server, plain, mounts="none")
        _create(server, mounted, mounts=None)  # every server mount: an admin's shed
        assert _user_of(server, plain) is None and _user_of(server, mounted) is None
        installed = _installed_marker()
        # The only user is the developer's `dev`, an admin: both become theirs.
        with dev_config({}, server.name, transform=_only([])):
            assert _user_of(server, plain) == DEV_USER
            assert _user_of(server, mounted) == DEV_USER
            # The step is recorded in the dev server's own state directory.
            assert _dev_marker().is_file(), f"no upgrade marker at {_dev_marker()}"
        # Back on the base (no user list): that start removed it, and the
        # installed server's marker was never touched.
        assert not _dev_marker().exists(), f"{_dev_marker()} outlived a start with no user list"
        assert _installed_marker() == installed, "the dev server changed the installed server's upgrade marker"
    finally:
        _cleanup(server, plain, mounted)


def test_cutting_the_list_to_one_user_later_hands_out_nothing(vz_server_dev):
    server = vz_server_dev
    _dev_target(server)
    name = f"itest-uu2-{uuid.uuid4().hex[:6]}"
    try:
        _create(server, name, mounts="none")
        installed = _installed_marker()
        with dev_config({}, server.name, transform=_only([ALICE])):
            # Two users (dev, itest-alice): nothing is stamped, and the step
            # is recorded in the dev server's own state directory.
            assert _user_of(server, name) is None
            assert _dev_marker().is_file(), f"no upgrade marker at {_dev_marker()}"
            # Straight from two users to one (no start without a list in
            # between, which would remove the marker): still nobody's.
            with dev_config({}, server.name, transform=_only([])):
                assert _user_of(server, name) is None
        assert not _dev_marker().exists(), f"{_dev_marker()} outlived a start with no user list"
        assert _installed_marker() == installed, "the dev server changed the installed server's upgrade marker"
    finally:
        _cleanup(server, name)


def test_an_admin_assigns_a_shed_that_has_no_user(vz_server_dev):
    server = vz_server_dev
    _dev_target(server)
    plain, mounted = f"itest-uu3-{uuid.uuid4().hex[:6]}", f"itest-uu3m-{uuid.uuid4().hex[:6]}"
    try:
        _create(server, plain, mounts="none")
        _create(server, mounted, mounts=None)
        with users_environment("vz", server, reconfigure=True, image="base") as env:
            # Several users: neither shed is anyone's. A user does not see
            # them; an admin sees them with no user.
            assert plain not in {s["name"] for s in env.alice.list_sheds()}
            seen = {s["name"]: s for s in env.admin.list_sheds()}
            assert not seen[plain].get("user") and not seen[mounted].get("user"), seen

            # Nobody may enter one; the admin is told how to give it away.
            marker = f"itest-entered-{uuid.uuid4().hex[:8]}"
            r = env.admin.ssh(plain, f"echo {marker}")
            assert r.returncode != 0 and marker not in r.stdout, r.stdout
            assert "has no user" in r.stderr and "shed assign" in r.stderr, r.stderr

            admin = env.admin.api()
            # Refused: a non-admin; a shed that carries the server's mounts.
            status, resp = env.alice.api().request("POST", f"/api/sheds/{plain}/assign", {"user": ALICE})
            assert (status, error_code(resp)) == (403, "FORBIDDEN"), (status, resp)
            status, resp = admin.request("POST", f"/api/sheds/{mounted}/assign", {"user": ALICE})
            assert (status, error_code(resp)) == (409, "SHED_HOST_RESOURCES"), (status, resp)

            # The admin assigns the plain one; alice may then enter it.
            r = env.admin.cli("assign", plain, ALICE)
            assert r.returncode == 0, f"shed assign failed: {r.stdout!r} {r.stderr!r}"
            assert {s["name"]: s.get("user") for s in env.alice.list_sheds()}.get(plain) == ALICE
            marker = f"itest-entered-{uuid.uuid4().hex[:8]}"
            r = env.alice.ssh(plain, f"echo {marker}")
            assert r.returncode == 0 and marker in r.stdout, f"alice cannot enter her assigned shed: {r.stderr!r}"

            # Assigned now: not again, and the admin may no longer enter it.
            status, resp = admin.request("POST", f"/api/sheds/{plain}/assign", {"user": BOB})
            assert (status, error_code(resp)) == (409, "SHED_ALREADY_ASSIGNED"), (status, resp)
            r = env.admin.ssh(plain, "true")
            assert r.returncode != 0 and "belongs to another user" in r.stderr, r.stderr

            # A key taken off the list opens nothing new: restart without bob.
            with dev_config({}, server.name, transform=_only([ADMIN, ALICE])):
                r = env.bob.cli("list", timeout=60)
                assert r.returncode != 0, f"bob still reaches the API after his removal: {r.stdout!r}"
                r = env.bob.ssh(plain, "true")
                assert r.returncode != 0 and "Permission denied" in r.stderr, r.stderr
                # Control: alice, still listed, still does.
                assert plain in {s["name"] for s in env.alice.list_sheds()}
    finally:
        _cleanup(server, plain, mounted)
