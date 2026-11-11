"""`shed create --mounts` against the parallel FC dev server.

The admin-facing mount selection through the CLI: `--mounts none` sends
`"mounts": []`, the shed boots with no 9P server mounts, `shed list -v` shows
`none` in its MOUNTS column (and `-vv` its `Server Mounts:` key), and the
stored selection holds across stop/start. A default create next to it is the
positive control (it gets every server mount and lists as `all`).

Firecracker only (the `fc_server_dev` fixture, so it can only reach the dev
entry). Mode-independent, but run in mtls mode like the rest of plan 034:
`make build && make dev-server-restart-fc SHED_DEV_AUTH_MODE=mtls &&
make test-integration-dev-fc SHED_DEV_AUTH_MODE=mtls PYTEST_ARGS="test_create_mounts.py"`.
"""

from __future__ import annotations

import json
import subprocess
import uuid

import pytest

CREATE_TIMEOUT = 300


def shed(server, *args: str, timeout: int = 60) -> subprocess.CompletedProcess:
    return subprocess.run(["shed", "-s", server.name, *args], capture_output=True, text=True, timeout=timeout)


def ninep_mounts(server, name: str) -> list[str]:
    r = server.exec(name, ["cat", "/proc/mounts"])
    assert r.returncode == 0, f"cat /proc/mounts failed: {r.stderr!r}"
    return [line for line in r.stdout.splitlines() if " 9p " in line]


def list_v_row(server, name: str) -> dict[str, str]:
    """The `shed list -v` row for name, keyed by column header."""
    r = shed(server, "-v", "list")
    assert r.returncode == 0, r.stderr
    lines = [l for l in r.stdout.splitlines() if l.strip()]
    header = lines[0].split()
    for line in lines[1:]:
        fields = line.split()
        if fields and fields[0] == name:
            # Columns are single words except UPTIME ("1h 2m"), which is last.
            return dict(zip(header, fields))
    raise AssertionError(f"{name} not in `shed list -v`:\n{r.stdout}")


@pytest.fixture
def created(fc_server_dev):
    names: list[str] = []
    yield names
    for n in names:
        fc_server_dev.delete(n, ignore_missing=True)


def test_create_mounts_none_survives_stop_start(fc_server_dev, created):
    name = f"itest-mnt-none-{uuid.uuid4().hex[:8]}"
    control = f"itest-mnt-all-{uuid.uuid4().hex[:8]}"
    created += [name, control]

    r = shed(fc_server_dev, "create", name, "--image", "itest", "--mounts", "none", "--no-sync", timeout=CREATE_TIMEOUT)
    assert r.returncode == 0, f"create --mounts none failed: {r.stdout!r} {r.stderr!r}"
    fc_server_dev.create(control, image="itest", timeout=CREATE_TIMEOUT)

    # Positive control: a default create gets the server's mounts.
    assert ninep_mounts(fc_server_dev, control), "a default shed has no 9P mounts; the check below would prove nothing"
    assert list_v_row(fc_server_dev, control)["MOUNTS"] == "all"

    assert ninep_mounts(fc_server_dev, name) == []
    row = list_v_row(fc_server_dev, name)
    assert row["MOUNTS"] == "none" and row["OWNER"] == "-", row

    r = shed(fc_server_dev, "-vv", "list")
    assert r.returncode == 0, r.stderr
    block = next(b for b in r.stdout.split("---") if f"Name:           {name}\n" in b)
    assert "Server Mounts:  none\n" in block and "Owner:          -\n" in block, block

    r = shed(fc_server_dev, "--json", "list")
    assert r.returncode == 0, r.stderr
    mine = [s for s in json.loads(r.stdout) if s["name"] == name]
    assert mine and mine[0]["mounts"] == {"mode": "selected"} and "owner" not in mine[0], mine

    fc_server_dev.stop(name)
    fc_server_dev.start(name, timeout=CREATE_TIMEOUT)
    assert ninep_mounts(fc_server_dev, name) == []
    assert list_v_row(fc_server_dev, name)["MOUNTS"] == "none"


def test_create_mounts_unknown_name_is_refused(fc_server_dev, created):
    name = f"itest-mnt-bad-{uuid.uuid4().hex[:8]}"
    created.append(name)
    r = shed(fc_server_dev, "create", name, "--image", "itest", "--mounts", "no-such-mount", "--no-sync", timeout=60)
    assert r.returncode != 0
    assert "no-such-mount" in (r.stdout + r.stderr) and "not a configured server mount" in (r.stdout + r.stderr), r.stderr
    assert name not in fc_server_dev.list_shed_names()
