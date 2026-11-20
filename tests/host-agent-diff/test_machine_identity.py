"""This machine's identity (plan 035 §3.4): `<state>/host-agent/machine.json`, created on
first start, read unchanged on every later one, and shown in the status.

Not golden cells — the id is random per state dir and the name comes from the host's
hostname, so what these pin is the behaviour (stable across restarts, replaced when
corrupt, a broker that keeps running without one) rather than a recorded value. The
masked shape of the `machine` block is pinned by `test_config_inline_flow.py`'s golden.
The log lines asserted are the broker's own (the bridge logs them with the same
`WARN  `/`ERROR ` prefixes the retired daemon used).
"""

from __future__ import annotations

import json
import os
import re
import stat

import pytest

from normalize import assert_machine_identity, mask_live_status

# Always `fullmatch`: `$` also matches before a final newline.
_ID = re.compile(r"[0-9a-f]{32}")
_KEPT_ID = "0123456789abcdef0123456789abcdef"


def _machine_file(state_dir) -> str:
    return os.path.join(str(state_dir), "host-agent", "machine.json")


def test_machine_id_stable_across_restart(daemon, watch_none_config, tmp_path):
    state_dir = tmp_path / "state"

    with daemon("rust", watch_none_config, state_dir=state_dir) as d:
        first = d.status()
        oplog = d.read_log()
    machine = first.get("machine")
    assert machine is not None, f"no machine block: {first}"
    assert_machine_identity(machine["id"], machine["name"])
    assert "machine_error" not in first, first
    assert "machine identity created" in oplog, oplog

    # The file the broker wrote: 0600 in a 0700 dir, and it says what status said.
    path = _machine_file(state_dir)
    assert stat.S_IMODE(os.stat(path).st_mode) == 0o600
    assert stat.S_IMODE(os.stat(os.path.dirname(path)).st_mode) == 0o700
    with open(path, encoding="utf-8") as f:
        on_disk = json.load(f)
    assert on_disk["schema"] == 1
    assert on_disk["id"] == machine["id"]
    assert on_disk["name"] == machine["name"]

    # A second start on the same state dir (a fresh broker, a fresh HOME and socket
    # dir) is the same machine.
    with daemon("rust", watch_none_config, state_dir=state_dir) as d:
        second = d.status()
        oplog = d.read_log()
    assert second["machine"] == machine
    assert "machine identity created" not in oplog, oplog


def test_corrupt_machine_file_is_replaced_not_fatal(daemon, watch_none_config, tmp_path):
    state_dir = tmp_path / "state"
    path = _machine_file(state_dir)
    os.makedirs(os.path.dirname(path), mode=0o700)
    with open(path, "w", encoding="utf-8") as f:
        f.write("{not json")

    with daemon("rust", watch_none_config, state_dir=state_dir) as d:
        status = d.status()
        oplog = d.read_log()

    machine = status.get("machine")
    assert machine is not None and _ID.fullmatch(machine["id"]), status
    # Loud, not silent: the operator is told the server will see a new machine.
    assert "WARN  machine identity was unusable and has been REPLACED" in oplog, oplog
    # The bad file is kept for inspection, the good one written in its place.
    with open(path + ".corrupt", encoding="utf-8") as f:
        assert f.read() == "{not json"
    with open(path, encoding="utf-8") as f:
        assert json.load(f)["id"] == machine["id"]


def test_daemon_runs_without_an_identity_it_cannot_create(
    daemon, watch_none_config, tmp_path
):
    if os.geteuid() == 0:
        pytest.skip("root writes through any directory mode")
    # A read-only state dir with no `host-agent/` in it: nothing can be created.
    state_dir = tmp_path / "state"
    state_dir.mkdir()
    os.chmod(state_dir, 0o500)
    try:
        with daemon("rust", watch_none_config, state_dir=state_dir) as d:
            status = d.status()
            oplog = d.read_log()
    finally:
        os.chmod(state_dir, 0o700)

    # The broker started and serves status; it just has no identity, and says why.
    assert "machine" not in status, status
    assert status["machine_error"], status
    assert "ERROR machine identity unavailable; continuing without one" in oplog, oplog


def _write_machine_file(state_dir, text: str) -> str:
    path = _machine_file(state_dir)
    os.makedirs(os.path.dirname(path), mode=0o700)
    with open(path, "w", encoding="utf-8") as f:
        f.write(text)
    return path


def test_a_valid_id_with_unreadable_fields_is_kept(daemon, watch_none_config, tmp_path):
    """A valid id is never thrown away because some OTHER field is missing, `null`, of
    the wrong type or malformed: the id is kept and the rest repaired in place."""
    state_dir = tmp_path / "state"
    path = _write_machine_file(
        state_dir,
        json.dumps({"schema": 1, "id": _KEPT_ID, "name": None, "created_at": 17}),
    )

    with daemon("rust", watch_none_config, state_dir=state_dir) as d:
        status = d.status()
        oplog = d.read_log()

    assert status["machine"]["id"] == _KEPT_ID, status
    assert_machine_identity(status["machine"]["id"], status["machine"]["name"])
    assert "WARN  machine identity repaired; the id is kept" in oplog, oplog
    assert "REPLACED" not in oplog, oplog
    assert not os.path.exists(path + ".corrupt")
    with open(path, encoding="utf-8") as f:
        on_disk = json.load(f)
    assert on_disk["schema"] == 1
    assert on_disk["id"] == _KEPT_ID
    assert on_disk["name"] == status["machine"]["name"]
    assert isinstance(on_disk["created_at"], str)


def test_a_newer_schema_without_a_readable_id_is_left_alone(
    daemon, watch_none_config, tmp_path
):
    """A newer build's file is never rewritten, moved or replaced. With no id this build
    can read, the broker runs without an identity and says which schema it found."""
    state_dir = tmp_path / "state"
    text = json.dumps({"schema": 2, "id": {"hex": _KEPT_ID}, "name": "desk"})
    path = _write_machine_file(state_dir, text)

    with daemon("rust", watch_none_config, state_dir=state_dir) as d:
        status = d.status()
        oplog = d.read_log()

    assert "machine" not in status, status
    err = status["machine_error"]
    assert "newer build (schema 2)" in err and "left as it is" in err, status
    assert "ERROR machine identity unavailable; continuing without one" in oplog, oplog
    with open(path, encoding="utf-8") as f:
        assert f.read() == text
    assert not os.path.exists(path + ".corrupt")


def _live_status_with_machine(machine_id: str) -> dict:
    return {
        "schema": 1,
        "pid": 42,
        "version": "v",
        "config_path": "/c.yaml",
        "started_at": "2026-01-01T00:00:00Z",
        "written_at": "2026-01-01T00:00:00Z",
        "approval_channel": {"socket_path": "/s/host-agent.sock"},
        "servers": [],
        "machine": {"id": machine_id, "name": "desk"},
    }


def test_machine_id_mask_requires_a_full_match():
    """The mask asserts the whole id before hiding it: an id with a trailing newline (which
    a `$`-anchored `match` lets through) must fail, not be masked to `<machine_id>`."""
    bad = "a" * 32 + "\n"
    with pytest.raises(AssertionError):
        assert_machine_identity(bad, "desk")
    with pytest.raises(AssertionError):
        mask_live_status(_live_status_with_machine(bad), "/s", "/c.yaml")
    masked = mask_live_status(_live_status_with_machine("a" * 32), "/s", "/c.yaml")
    assert masked["machine"] == {"id": "<machine_id>", "name": "<machine_name>"}
