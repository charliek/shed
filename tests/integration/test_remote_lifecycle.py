"""Lifecycle of a `default_backend: remote` server (each shed is a cloud VM).

What the shared `[remote]` cases (create, exec, exec-shell, lifecycle,
delete) do not cover:

  - `shed --json list` reports the shed's permanent `id` and `backend: remote`;
  - a file written in the shed survives `shed stop` and `shed start` (the VM is
    powered off and its disk kept, unlike a reset);
  - a create past `remote.limits.max_running` is refused with
    `SERVER_LIMIT_REACHED` (503, never 409) and leaves no shed.

The module creates billed cloud VMs (two with the default limit of 2, at
most `SHED_REMOTE_MAX_RUNNING` at once) and deletes them in teardown. Runs only
under `make test-integration-remote`; skips otherwise.
"""

from __future__ import annotations

import os
import re
import subprocess
import time
import uuid

import pytest

# config.NewShedID: 16 random bytes, lowercase base32, no padding.
SHED_ID_RE = re.compile(r"^[a-z2-7]{26}$")

# The server's `remote.limits.max_running` (the dev template sets 2).
MAX_RUNNING = int(os.environ.get("SHED_REMOTE_MAX_RUNNING", "2"))

# A deleted shed's VM goes away in the background (about 30 s on the dev
# project). Until it is gone it may still count against max_running, so a
# create that meets SERVER_LIMIT_REACHED right after another test's delete is
# retried for this long before the test fails.
LIMIT_SETTLE_S = 180


@pytest.fixture
def remote_names(remote_server, request):
    """Hands out shed names for one test and deletes every one in teardown."""
    names: list[str] = []
    base = re.sub(r"[^a-z0-9-]+", "-", request.node.name.lower()).strip("-")[:20].rstrip("-")

    def make() -> str:
        name = f"itest-{base}-{uuid.uuid4().hex[:6]}"
        names.append(name)
        return name

    yield make
    for name in names:
        try:
            remote_server.delete(name, ignore_missing=True)
        except Exception:
            pass


def _create(remote_server, name: str) -> subprocess.CompletedProcess:
    cmd = ["create", name]
    if remote_server.image:
        cmd += ["--image", remote_server.image]
    return remote_server.cli(*cmd, timeout=300)


def _create_waiting_for_room(remote_server, name: str) -> None:
    """Create `name`, retrying while an earlier test's VMs still fill the
    running limit."""
    deadline = time.monotonic() + LIMIT_SETTLE_S
    while True:
        r = _create(remote_server, name)
        if r.returncode == 0:
            return
        refused = "SERVER_LIMIT_REACHED" in (r.stdout + r.stderr)
        if not refused or time.monotonic() > deadline:
            raise AssertionError(
                f"shed create {name} failed (exit {r.returncode}): "
                f"stdout={r.stdout!r} stderr={r.stderr!r}"
            )
        time.sleep(10)


def _find(remote_server, name: str) -> dict:
    for shed in remote_server.list_sheds():
        if shed.get("name") == name:
            return shed
    raise AssertionError(f"shed {name!r} not in `shed --json list`")


@pytest.fixture(scope="module")
def module_shed(remote_server):
    """One running shed shared by the list and stop/start tests (one VM
    instead of two)."""
    name = f"itest-remote-life-{uuid.uuid4().hex[:6]}"
    try:
        _create_waiting_for_room(remote_server, name)
        yield name
    finally:
        remote_server.delete(name, ignore_missing=True)


def test_list_json_reports_id_and_backend(remote_server, module_shed):
    shed = _find(remote_server, module_shed)
    assert shed.get("backend") == "remote", shed
    assert SHED_ID_RE.match(shed.get("id") or ""), f"bad or missing id: {shed}"
    assert shed.get("status") == "running", shed


def test_file_survives_stop_and_start(remote_server, module_shed):
    before = _find(remote_server, module_shed)
    marker = uuid.uuid4().hex
    r = remote_server.exec(module_shed, ["bash", "-c", f"echo {marker} > ~/remote-marker && sync"])
    assert r.returncode == 0, f"writing the marker failed: {r.stderr!r}"

    remote_server.stop(module_shed)
    stopped = _find(remote_server, module_shed)
    assert stopped.get("status") == "stopped", stopped

    remote_server.start(module_shed)
    started = _find(remote_server, module_shed)
    assert started.get("status") == "running", started
    # The same shed, not a new one under the old name.
    assert started.get("id") == before.get("id"), (before, started)

    r = remote_server.exec(module_shed, ["cat", "/home/shed/remote-marker"])
    assert r.returncode == 0, f"reading the marker failed: {r.stderr!r}"
    assert r.stdout.strip() == marker, f"marker lost across stop/start: {r.stdout!r}"


def test_create_past_running_limit_is_refused(remote_server, remote_names):
    """With max_running sheds running, one more create is refused with
    SERVER_LIMIT_REACHED, quickly, and leaves no shed.

    Sheds already running (this module's shared shed, while it lives) count
    toward the limit, so only the difference is created.
    """
    running = [
        s.get("name") for s in remote_server.list_sheds()
        if s.get("status") in ("running", "starting", "stopping")
    ]
    if len(running) > MAX_RUNNING:
        pytest.skip(
            f"{len(running)} sheds already run ({running}), more than "
            f"SHED_REMOTE_MAX_RUNNING={MAX_RUNNING}; is the limit set right?"
        )
    for _ in range(MAX_RUNNING - len(running)):
        _create_waiting_for_room(remote_server, remote_names())

    extra = remote_names()
    t0 = time.monotonic()
    r = _create(remote_server, extra)
    elapsed = time.monotonic() - t0
    assert r.returncode != 0, f"create past the limit succeeded: {r.stdout!r}"
    assert "SERVER_LIMIT_REACHED" in (r.stdout + r.stderr), (
        f"want SERVER_LIMIT_REACHED: stdout={r.stdout!r} stderr={r.stderr!r}"
    )
    # Refused before any cloud call (0.3 s on the dev server).
    assert elapsed < 15, f"the refusal took {elapsed:.1f}s"
    assert extra not in remote_server.list_shed_names(), (
        f"the refused create left shed {extra!r} behind"
    )
