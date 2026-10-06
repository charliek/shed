"""`fake_lane_server.py`'s own suite — the control door, not the fakes' wire.

`fake_opencode.py` is already exercised (indirectly) by the Tauri agent-lane
cells; this file is the one place that proves the CONTROL PORT itself works:
spawn the process, seed a session over `/_/…`, hit the agent port and see the
seed, prove `hold_seed`/`release_seed` actually parks and releases a GET
rather than being a no-op, and prove the stdin-EOF watchdog exits the process
promptly. (It also covered a second agent, `fake_gx.py`, through plan 017;
gx and its lane left shed in plan 025 C1 — shed#390.)

This suite talks stdlib HTTP directly to a `fake_lane_server.py` subprocess and
needs no mac/Tauri app, so the three `_app_session`/`_reset_policy`/
`_reset_mock` overrides below neutralize `conftest.py`'s autouse app-launch
fixtures for the tests in THIS module only (they stay in force, unmodified,
for every other module in the directory — this is the standard pytest
fixture-override pattern, not a global change).
"""

from __future__ import annotations

import json
import subprocess
import sys
import threading
import time
import urllib.error
import urllib.request
from pathlib import Path

import pytest

HERE = Path(__file__).resolve().parent
SCRIPT = HERE / "fake_lane_server.py"


# ---------------------------------------------------------------------------
# neutralize conftest.py's autouse app-launching fixtures for this module
# ---------------------------------------------------------------------------


@pytest.fixture(scope="session")
def _app_session():
    yield


@pytest.fixture
def _reset_policy():
    yield


@pytest.fixture
def _reset_mock():
    yield


# ---------------------------------------------------------------------------
# a minimal stdlib client for the control port + spawn/teardown helper
# ---------------------------------------------------------------------------


def _post(url: str, obj: dict | None = None, *, headers: dict | None = None) -> tuple[int, dict | None]:
    data = json.dumps(obj if obj is not None else {}).encode()
    req = urllib.request.Request(
        url, data=data, method="POST",
        headers={"Content-Type": "application/json", **(headers or {})})
    try:
        with urllib.request.urlopen(req, timeout=10) as resp:
            raw = resp.read()
            return resp.status, json.loads(raw.decode()) if raw else None
    except urllib.error.HTTPError as exc:
        raw = exc.read()
        return exc.code, json.loads(raw.decode()) if raw else None


def _get(url: str, *, headers: dict | None = None, timeout: float = 10) -> tuple[int, str]:
    req = urllib.request.Request(url, headers=headers or {})
    try:
        with urllib.request.urlopen(req, timeout=timeout) as resp:
            return resp.status, resp.read().decode()
    except urllib.error.HTTPError as exc:
        return exc.code, exc.read().decode()


class _Fake:
    """One spawned `fake_lane_server.py` process plus its parsed startup line."""

    def __init__(self, agent: str, *, extra_args: list[str] | None = None):
        argv = [sys.executable, str(SCRIPT), "--agent", agent]
        argv += extra_args or []
        self.proc = subprocess.Popen(
            argv, stdin=subprocess.PIPE, stdout=subprocess.PIPE,
            stderr=subprocess.PIPE, text=True, bufsize=1)
        line = self.proc.stdout.readline()
        assert line, f"fake_lane_server.py produced no stdout (stderr: {self.proc.stderr.read()})"
        self.info = json.loads(line)

    def call(self, method: str, *, args: list | None = None, kwargs: dict | None = None) -> dict:
        status, body = _post(f"{self.info['control']}/_/{method}",
                             {"args": args or [], "kwargs": kwargs or {}})
        assert status == 200, f"{method} -> {status} {body}"
        return body

    def envelope(self, name: str, **kwargs) -> dict:
        status, body = _post(f"{self.info['control']}/_/envelope/{name}", kwargs)
        assert status == 200, f"envelope/{name} -> {status} {body}"
        return body

    def info_via_control(self) -> dict:
        status, body = _get(f"{self.info['control']}/_/info")
        assert status == 200
        return json.loads(body)

    def close(self, *, via: str = "stdin") -> None:
        if via == "stop":
            self.call("stop")
            self.proc.wait(timeout=5)
        self.proc.stdin.close()
        try:
            self.proc.wait(timeout=5)
        except subprocess.TimeoutExpired:
            self.proc.kill()
            self.proc.wait(timeout=5)


@pytest.fixture
def opencode_fake():
    fake = _Fake("opencode")
    yield fake
    fake.close()


# ---------------------------------------------------------------------------
# tests
# ---------------------------------------------------------------------------


def test_opencode_startup_and_seed(opencode_fake):
    assert opencode_fake.info["agent"] == "opencode"
    assert opencode_fake.info["home"] is None
    opencode_fake.call("add_session", args=["s1"], kwargs={"title": "hi"})
    opencode_fake.call("set_simple_transcript", args=["s1", "hello", "hi there"])

    status, body = _get(f"{opencode_fake.info['reported_url']}/session")
    assert status == 200
    sessions = json.loads(body)
    assert [s["id"] for s in sessions] == ["s1"]

    status, body = _get(f"{opencode_fake.info['reported_url']}/session/s1/message")
    assert status == 200
    assert len(json.loads(body)) == 2


def test_opencode_post_paths_and_snapshot(opencode_fake):
    opencode_fake.call("add_session", args=["s1"])
    status, _ = _post(f"{opencode_fake.info['reported_url']}/session/s1/prompt_async", {})
    assert status == 204

    post_paths = opencode_fake.call("post_paths")
    assert post_paths == ["/session/s1/prompt_async"]

    snapshot = opencode_fake.call("snapshot")
    assert snapshot["post_paths"] == post_paths
    assert snapshot["violations"] == []


def test_opencode_hold_seed_blocks_the_message_get_until_released(opencode_fake):
    opencode_fake.call("add_session", args=["s1"])
    opencode_fake.call("set_simple_transcript", args=["s1", "hello", "hi there"])
    opencode_fake.call("hold_seed")

    result: dict = {}

    def _do_get():
        status, body = _get(
            f"{opencode_fake.info['reported_url']}/session/s1/message", timeout=10)
        result["status"], result["body"] = status, body

    t = threading.Thread(target=_do_get)
    t.start()
    try:
        time.sleep(0.3)
        assert t.is_alive() is True, "the seed GET must still be parked"
    finally:
        opencode_fake.call("release_seed")
        t.join(timeout=5)
    assert result["status"] == 200
    assert len(json.loads(result["body"])) == 2


def test_opencode_stdin_eof_exits_within_two_seconds(opencode_fake):
    start = time.time()
    opencode_fake.proc.stdin.close()
    opencode_fake.proc.wait(timeout=2)
    assert time.time() - start < 2.0
    assert opencode_fake.proc.returncode == 0
