"""Project-mount + landing-directory tests (--local-dir / --add-dir).

Exercise the home-rooted workspace model:
  - a bare shed lands in the home directory;
  - --local-dir mounts a host dir at ~/<basename> and becomes the
    landing directory;
  - --add-dir mounts additional host dirs as siblings under the home dir.

It also guards #410: the directories a mount needs under the shed user's
home (e.g. ~/.local, ~/.local/share, ~/.config) belong to `shed`, not root,
and a root-owned one is repaired on the next `shed start`.

Parameterized over ["vz", "fc"] through the `shed_server` fixture. The
--local-dir/--add-dir test needs its host directories to exist on the SAME
machine as the shed-server, so it runs only against a local server (skips on
the remote/FC case). The #410 test needs the server's own `mounts:` config to
mount something at least two levels below the home directory (the dev configs
do), and skips otherwise.
"""

from __future__ import annotations

import pytest

from fixtures import mount_parents as mp
from fixtures.server import RemoteServer

SHED_OWNER = "shed:shed"


def test_default_landing_is_home(shed_server, test_shed_name):
    """A bare shed (no --repo / --local-dir) lands in the home directory."""
    shed_server.create(test_shed_name, image="base")
    r = shed_server.exec(test_shed_name, ["pwd"])
    assert r.returncode == 0, f"pwd failed: exit={r.returncode} stderr={r.stderr!r}"
    assert r.stdout.strip() == "/home/shed", (
        f"expected landing dir /home/shed for a bare shed, got {r.stdout!r}"
    )


def test_local_dir_and_add_dir_mounts(shed_server, test_shed_name, tmp_path):
    """--local-dir mounts at ~/<basename> and is the landing dir; --add-dir
    mounts additional dirs as siblings; each surfaces its host contents."""
    if isinstance(shed_server, RemoteServer):
        pytest.skip(
            "--local-dir/--add-dir host paths must exist on the shed-server "
            "host; this mount test only runs against a local server (VZ)."
        )

    primary = tmp_path / "primary"
    primary.mkdir()
    (primary / "MARKER").write_text("primary-ok")
    ref = tmp_path / "reference"
    ref.mkdir()
    (ref / "MARKER").write_text("reference-ok")

    shed_server.create(
        test_shed_name,
        image="base",
        local_dir=str(primary),
        add_dirs=[str(ref)],
    )

    # The landing directory is the --local-dir mount.
    r = shed_server.exec(test_shed_name, ["pwd"])
    assert r.returncode == 0, f"pwd failed: exit={r.returncode} stderr={r.stderr!r}"
    assert r.stdout.strip() == "/home/shed/primary", (
        f"expected landing dir /home/shed/primary, got {r.stdout!r}"
    )

    # Both mounts surface their host contents at ~/<basename>.
    r = shed_server.exec(test_shed_name, ["cat", "/home/shed/primary/MARKER"])
    assert r.returncode == 0 and r.stdout.strip() == "primary-ok", (
        f"--local-dir mount missing at ~/primary: "
        f"exit={r.returncode} stdout={r.stdout!r} stderr={r.stderr!r}"
    )
    r = shed_server.exec(test_shed_name, ["cat", "/home/shed/reference/MARKER"])
    assert r.returncode == 0 and r.stdout.strip() == "reference-ok", (
        f"--add-dir mount missing at ~/reference: "
        f"exit={r.returncode} stdout={r.stdout!r} stderr={r.stderr!r}"
    )


# ----------------------------------------------------------------------------
# #410: mount parents are owned by shed
# ----------------------------------------------------------------------------


def _path_facts(server, name: str, paths: list[str]) -> dict[str, mp.PathFact]:
    """{path: PathFact} for each path, read in the guest in one exec."""
    r = server.exec(name, ["sh", "-c", mp.PATH_FACTS_SH, "sh", *paths])
    assert r.returncode == 0, f"path stat failed: exit={r.returncode} stderr={r.stderr!r}"
    return mp.parse_path_facts(r.stdout)


def _mount_parents(server, name: str) -> mp.MountParents:
    r = server.exec(name, ["findmnt", "-rn", "-o", "TARGET,FSTYPE"])
    assert r.returncode == 0, f"findmnt failed: exit={r.returncode} stderr={r.stderr!r}"
    mounts = mp.parse_findmnt(r.stdout)
    facts = _path_facts(server, name, mp.fact_paths(mp.deep_mounts(mounts)))
    return mp.classify(mounts, facts)


def test_mount_parents_owned_by_shed(shed_server, test_shed_name):
    """Directories a server mount needs under the home directory are created
    as `shed`, not by the mount's root `mkdir -p` (#410), and a root-owned
    one is repaired by the next `shed start`.

    Needs the server's `mounts:` config to place a 9p/virtiofs mount at least
    two levels below the home directory (e.g. ~/.local/share/opencode,
    ~/.config/gh — the dev configs do). Red on a server without the mount
    prelude (pre-0.9.1): `~/.local`, `~/.config` come up `root:root`.

    Only directories on the guest's own root filesystem are inspected or
    chowned (see `fixtures/mount_parents.py:classify`): with the home
    directory inside a host mount the test skips, so its `chown root:root`
    can never reach a host directory.
    """
    shed_server.skip_if_released_before(
        "0.9.1",
        "it predates the #410 fix (a mount's missing parent directories are "
        "created by root, so ~/.local and ~/.config come up root-owned)",
    )

    shed_server.create(test_shed_name, image="base")

    found = _mount_parents(shed_server, test_shed_name)
    if not found.deep:
        pytest.skip(
            f"{shed_server.name} mounts nothing 9p/virtiofs at least two levels "
            f"below {mp.HOME} (its `mounts:` config has no nested target such as "
            f"~/.config/gh), so no mount needs a parent directory created"
        )
    if found.home_blocker:
        pytest.skip(
            f"{mp.HOME} is inside a mount on {shed_server.name} "
            f"({found.home_blocker}); the prelude leaves its subtree to the host"
        )
    if not found.eligible:
        pytest.skip(
            f"every parent of {shed_server.name}'s nested mounts {found.deep} is a "
            f"symlink, a mountpoint or inside one; none is the prelude's to own"
        )

    facts = _path_facts(shed_server, test_shed_name, found.eligible)
    wrong = {p: facts[p].owner for p in found.eligible if facts[p].owner != SHED_OWNER}
    assert not wrong, (
        f"mount parents not owned by {SHED_OWNER} (#410) for mounts {found.deep}: {wrong}"
    )

    home_local = f"{mp.HOME}/.local"
    home_share = f"{home_local}/share"
    if home_local in found.eligible and home_share in found.eligible:
        # What roost-session (`shed attach`, Connect Host) needs first. Only
        # when BOTH ~/.local and ~/.local/share are eligible — real guest
        # directories, not mounts or inside one — so this mkdir can never
        # create anything inside a host share (e.g. a mount at ~/.local/share).
        r = shed_server.exec(
            test_shed_name,
            ["mkdir", "-p", f"{home_share}/roost-session"],
        )
        assert r.returncode == 0, (
            f"shed cannot create ~/.local/share/roost-session: "
            f"exit={r.returncode} stderr={r.stderr!r}"
        )

    # A shed broken before the fix is repaired on its next start. `top` is a
    # depth-1 eligible parent: a real directory on the guest's root
    # filesystem, never a host directory.
    top = home_local if home_local in found.eligible else found.eligible[0]
    r = shed_server.exec(test_shed_name, ["sudo", "chown", "root:root", top])
    assert r.returncode == 0, f"sudo chown failed: exit={r.returncode} stderr={r.stderr!r}"
    assert _path_facts(shed_server, test_shed_name, [top])[top].owner == "root:root"

    shed_server.stop(test_shed_name)
    shed_server.start(test_shed_name)

    got = _path_facts(shed_server, test_shed_name, [top])[top].owner
    assert got == SHED_OWNER, (
        f"{top} is {got} after `shed start`; the mount prelude should have "
        f"repaired the root-owned parent to {SHED_OWNER} (#410)"
    )
