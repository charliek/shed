"""Writes to a `--local-dir` mount survive `shed stop` and `shed delete` (#284).

A stop runs the shed's `hooks.shutdown` before the VM goes down, and the
hook's writes only reach the host if the mount is still being served while it
runs. On Firecracker the mount is a 9P server in shed-server; #284 made those
servers close promptly by force-closing their connections, which is only safe
because they now close AFTER the VM is gone. The stop half is the live guard on
that ordering: closing the servers first (where they used to close) loses the
hook's write.

  1. Create a shed whose `--local-dir` carries a `.shed/provision.yaml` with a
     shutdown hook that writes a per-run marker into the mounted directory.
  2. `shed stop` — the marker must be on the HOST.
  3. `shed start`, `shed exec` a write of a second file into the mount (no
     explicit sync), `shed delete --force` — a write made before delete
     survives the delete on the host.

Step 3 does NOT prove delete's pre-kill sync: the FC 9P mount has no writeback
cache, so the exec'd write has already reached the host when the exec
returns. It guards the plainer property that deleting a running shed never
loses, rolls back, or removes data it already wrote to a host mount.

Parameterized over ["vz", "fc"] through `shed_server`. The `--local-dir` must
exist on the shed-server's host, but the CLI also validates it with a LOCAL
stat, so for a remote (FC) server the same absolute path is created on both
sides: empty here, populated on the server's host over ssh, and the markers are
read back over ssh. Skips against a released shed-server older than 0.9.1.
"""

from __future__ import annotations

import os
import shlex
import shutil
import uuid
from pathlib import Path
from typing import Optional

import pytest

from fixtures.server import RemoteServer

MIN_VERSION = "0.9.1"

HOOK_MARKER = "stop-hook-marker"
EXEC_MARKER = "exec-before-delete"


class _HostProject:
    """A `--local-dir` project directory on the shed-server's host.

    `path` is the absolute path handed to `shed create --local-dir`. Files are
    written and read where the SERVER sees them: directly for a local server,
    over ssh for a remote one.

    The directory itself is mode 0777 because the guest's shed user (uid 1000)
    creates the markers in it. Over 9P the server remaps only root-owned host
    files; a non-root owner's uid passes through, so a remote ssh user whose
    uid is not 1000 would otherwise own a directory the guest cannot write.
    0777 works without sudo on any host; the files written here stay 0644 /
    0755, since the guest only reads and runs them.
    """

    def __init__(self, server, path: str) -> None:
        self.server = server
        self.path = path
        self.remote = isinstance(server, RemoteServer)

    @property
    def guest_path(self) -> str:
        return f"/home/shed/{os.path.basename(self.path)}"

    def setup(self, files: dict[str, tuple[str, int]]) -> None:
        """Create the directory and write `files` ({relpath: (content, mode)})."""
        if not self.remote:
            Path(self.path).mkdir(parents=True, exist_ok=True)
            Path(self.path).chmod(0o777)  # see the class docstring
            for rel, (content, mode) in files.items():
                p = Path(self.path) / rel
                p.parent.mkdir(parents=True, exist_ok=True)
                p.write_text(content)
                p.chmod(mode)
            return
        # The CLI's local stat needs the path to exist here too; it stays empty.
        os.makedirs(self.path, exist_ok=False)
        # 0777: the guest's uid 1000 writes here, whatever the ssh user's uid
        # (see the class docstring).
        steps = [f"mkdir -p {shlex.quote(self.path)}", f"chmod 0777 {shlex.quote(self.path)}"]
        for rel, (content, mode) in files.items():
            dst = f"{self.path}/{rel}"
            steps += [
                f"mkdir -p {shlex.quote(os.path.dirname(dst))}",
                f"chmod 0755 {shlex.quote(os.path.dirname(dst))}",
                f"printf %s {shlex.quote(content)} > {shlex.quote(dst)}",
                f"chmod {mode:o} {shlex.quote(dst)}",
            ]
        r = self.server.ssh_run(" && ".join(steps))
        assert r.returncode == 0, (
            f"could not create {self.path} on {self.server.ssh_host}: "
            f"exit={r.returncode} stderr={r.stderr!r}"
        )

    def read(self, rel: str) -> Optional[str]:
        """The host file's contents, or None if it does not exist."""
        if not self.remote:
            p = Path(self.path) / rel
            return p.read_text() if p.exists() else None
        q = shlex.quote(f"{self.path}/{rel}")
        r = self.server.ssh_run(f"if [ -e {q} ]; then cat {q} || sudo -n cat {q}; else exit 3; fi")
        if r.returncode == 3:
            return None
        assert r.returncode == 0, (
            f"could not read {self.path}/{rel} on {self.server.ssh_host}: "
            f"exit={r.returncode} stderr={r.stderr!r}"
        )
        return r.stdout

    def cleanup(self) -> None:
        if not self.remote:
            return  # under tmp_path; pytest owns it
        shutil.rmtree(self.path, ignore_errors=True)
        q = shlex.quote(self.path)
        # Guest-created files are remapped to the directory's owner (the ssh
        # user); sudo is only the fallback for anything left root-owned.
        self.server.ssh_run(f"rm -rf {q} 2>/dev/null || sudo -n rm -rf {q}")


@pytest.fixture
def host_project(shed_server, tmp_path):
    """A fresh `--local-dir` path on the shed-server's host, removed afterwards.

    Requested BEFORE `test_shed_name` in the test signature, so it is torn
    down after the shed is deleted.
    """
    if isinstance(shed_server, RemoteServer):
        path = f"/tmp/shed-it-hook-{uuid.uuid4().hex[:12]}"
    else:
        path = str(tmp_path / "hookproj")
    project = _HostProject(shed_server, path)
    yield project
    project.cleanup()


def test_local_dir_writes_survive_stop_hook_and_delete(shed_server, host_project, test_shed_name):
    shed_server.skip_if_released_before(
        MIN_VERSION,
        "it predates the #284 9P teardown fix (stopping a shed with 9P mounts "
        "drains each mount for 5 s and can outrun the CLI's 30 s timeout)",
    )

    hook_marker = f"hook-{uuid.uuid4().hex}"
    exec_marker = f"exec-{uuid.uuid4().hex}"
    guest_dir = host_project.guest_path
    host_project.setup(
        {
            ".shed/provision.yaml": ("hooks:\n  shutdown: .shed/stop-hook.sh\n", 0o644),
            ".shed/stop-hook.sh": (
                "#!/bin/sh\n"
                f"printf '%s\\n' '{hook_marker}' > '{guest_dir}/{HOOK_MARKER}'\n",
                0o755,
            ),
        }
    )

    shed_server.create(test_shed_name, image="base", local_dir=host_project.path)

    # 1. The shutdown hook runs while the mount is still served.
    shed_server.stop(test_shed_name)
    got = host_project.read(HOOK_MARKER)
    assert got is not None and got.strip() == hook_marker, (
        f"the shutdown hook's write to {guest_dir}/{HOOK_MARKER} never reached "
        f"the host ({host_project.path}): got {got!r}, want {hook_marker!r}. "
        f"The mount was torn down before the hook (or the sync after it) ran."
    )

    # 2. A write made before delete survives the delete on the host. (Not a
    # test of delete's pre-kill sync: 9P writes are synchronous; see the
    # module docstring.)
    shed_server.start(test_shed_name)
    r = shed_server.exec(
        test_shed_name,
        ["sh", "-c", f"printf '%s' '{exec_marker}' > '{guest_dir}/{EXEC_MARKER}'"],
    )
    assert r.returncode == 0, (
        f"exec write into the mount failed: exit={r.returncode} stderr={r.stderr!r}"
    )
    shed_server.delete(test_shed_name)
    got = host_project.read(EXEC_MARKER)
    assert got is not None and got.strip() == exec_marker, (
        f"a write made before delete did not survive the delete on the host: "
        f"{host_project.path}/{EXEC_MARKER} = {got!r}, want {exec_marker!r}."
    )
