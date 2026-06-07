"""Project-mount + landing-directory tests (--local-dir / --add-dir).

Exercise the home-rooted workspace model:
  - a bare shed lands in the home directory;
  - --local-dir mounts a host dir at ~/<basename> and becomes the
    landing directory;
  - --add-dir mounts additional host dirs as siblings under the home dir.

Parameterized over ["vz", "fc"] through the `shed_server` fixture. The
mount test needs its host directories to exist on the SAME machine as
the shed-server, so it runs only against a local server (skips on the
remote/FC case).
"""

from __future__ import annotations

import pytest

from fixtures.server import RemoteServer


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
