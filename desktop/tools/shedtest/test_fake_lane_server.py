"""`fake_lane_server.py`'s own suite — the control door, not the fakes' wire.

`fake_gx.py` / `fake_opencode.py` are already exercised (indirectly) by the
Tauri gx-lane / agent-lane cells; this file is the one place that proves the
CONTROL PORT itself works: spawn the process, seed a session over `/_/…`, hit
the agent port and see the seed, prove `hold_seed`/`release_seed` actually
parks and releases a GET rather than being a no-op, and prove the stdin-EOF
watchdog exits the process promptly.

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

    def __init__(self, agent: str, *, home: Path | None = None, extra_args: list[str] | None = None):
        argv = [sys.executable, str(SCRIPT), "--agent", agent]
        if home is not None:
            argv += ["--home", str(home)]
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
def gx_fake(tmp_path):
    fake = _Fake("gx", home=tmp_path / "gx-home")
    yield fake
    fake.close()


@pytest.fixture
def opencode_fake():
    fake = _Fake("opencode")
    yield fake
    fake.close()


def _gx_token(fake: _Fake) -> str:
    home = Path(fake.info["home"])
    return (home / "gx-remote.token").read_text().strip()


# ---------------------------------------------------------------------------
# tests
# ---------------------------------------------------------------------------


def test_gx_startup_line_and_info_agree(gx_fake):
    assert gx_fake.info["agent"] == "gx"
    assert gx_fake.info["port"] > 0
    assert gx_fake.info["control"].startswith("http://127.0.0.1:")
    assert gx_fake.info["reported_url"] == f"http://127.0.0.1:{gx_fake.info['port']}"
    assert gx_fake.info_via_control() == gx_fake.info


def test_gx_seed_over_control_reaches_agent_port(gx_fake):
    gx_fake.call("add_session", args=["s1"], kwargs={"title": "hello"})
    token = _gx_token(gx_fake)
    status, body = _get(f"{gx_fake.info['reported_url']}/v1/sessions",
                        headers={"Authorization": f"Bearer {token}"})
    assert status == 200
    sessions = json.loads(body)["sessions"]
    assert [s["sessionId"] for s in sessions] == ["s1"]
    assert sessions[0]["title"] == "hello"


def test_gx_envelope_route_matches_the_builder(gx_fake):
    """`POST /_/envelope/permission_request` must return exactly what
    `fake_gx.permission_request` itself builds — the whole point of routing
    the builders through the control port instead of re-deriving them."""
    import fake_gx

    expected = fake_gx.permission_request("s1", "ls -la")
    got = gx_fake.envelope("permission_request", session="s1", command="ls -la")
    assert got == expected


def test_gx_unknown_method_is_400(gx_fake):
    status, body = _post(f"{gx_fake.info['control']}/_/no_such_method", {"args": [], "kwargs": {}})
    assert status == 400
    assert "error" in body


def test_gx_add_approval_is_answerable_over_the_control_door(gx_fake):
    """An approval a client can ANSWER, not merely render.

    `add_placeholder_approval` writes `method: null, request: null`, and
    `push_approval_frame` is broadcast-only by design (its docstring: the store
    is scripted separately so a cell can make the frame and the store disagree
    on purpose). So with neither `add_approval` nor this cell, a harness in
    another language could deliver a five-option permission, watch it render,
    and then have the answer 404 on the re-read every gx adapter does before it
    translates a decision — which looks like a client bug and is not one.
    """
    token = _gx_token(gx_fake)
    auth = {"Authorization": f"Bearer {token}"}
    session = "s-approve"
    gx_fake.call("add_session", args=[session], kwargs={"activity": "working"})
    request = gx_fake.envelope("permission_request", session=session, command="ls")
    resource = gx_fake.call(
        "add_approval",
        args=[session, "call_fixture", "permission",
              "session/request_permission", request])
    assert resource["status"] == "pending"
    assert resource["method"] == "session/request_permission"

    # The re-read, which is the step a frame-only approval cannot survive.
    url = f"{gx_fake.info['reported_url']}/v1/sessions/{session}/approvals/call_fixture"
    status, raw = _get(url, headers=auth)
    assert status == 200, raw
    options = json.loads(raw)["request"]["options"]
    assert len(options) == 5
    # The load-bearing wire fact: TWO options declare `allow_once`, so a
    # by-kind answer cannot say which the human meant.
    assert sum(1 for o in options if o["kind"] == "allow_once") == 2

    chosen = {"outcome": {"outcome": "selected", "optionId": "allow-once"}}
    status, body = _post(url, {"response": chosen}, headers=auth)
    assert status == 202, body
    assert gx_fake.call("answered_with", args=[session, "call_fixture"]) == chosen


def test_gx_requests_ledger_is_serialised_per_spec(gx_fake):
    token = _gx_token(gx_fake)
    _get(f"{gx_fake.info['reported_url']}/v1/healthz")
    _get(f"{gx_fake.info['reported_url']}/v1/sessions", headers={"Authorization": f"Bearer {token}"})
    records = gx_fake.call("requests")
    assert isinstance(records, list) and len(records) == 2
    for record in records:
        assert set(record) == {"method", "path", "query", "had_bearer", "bearer_ok"}
    assert records[0]["path"] == "/v1/healthz"
    assert records[0]["had_bearer"] is False
    assert records[1]["path"] == "/v1/sessions"
    assert records[1]["had_bearer"] is True

    gx_fake.call("clear_requests")
    assert gx_fake.call("requests") == []


def test_gx_bodies_to_returns_every_matching_body_in_order(gx_fake):
    """`bodies_to` is a LIST, not `FakeGx.body_of`'s first match.

    The cell that proves `mode: "interject"` posts to `…/messages` twice — a
    queued send first, then the interject — so asserting the first body there
    would assert the wrong one and pass.
    """
    token = _gx_token(gx_fake)
    auth = {"Authorization": f"Bearer {token}"}
    session = "s-bodies"
    gx_fake.call("add_session", args=[session], kwargs={"activity": "working"})
    url = f"{gx_fake.info['reported_url']}/v1/sessions/{session}/messages"
    assert _post(url, {"text": "one", "mode": "queue"}, headers=auth)[0] == 202
    assert _post(url, {"text": "two", "mode": "interject"}, headers=auth)[0] == 202

    bodies = [json.loads(b) for b in gx_fake.call("bodies_to", args=["/messages"])]
    assert [b["mode"] for b in bodies] == ["queue", "interject"]
    assert [b["text"] for b in bodies] == ["one", "two"]

    assert gx_fake.call("bodies_to", args=["/cancel"]) == []

    # A missing suffix is a 400 rather than "every body in the ledger" — and so
    # is an empty one, which `str.endswith` would otherwise match on every path.
    for sent in (None, {"args": [""]}, {"args": [0]}):
        status, body = _post(f"{gx_fake.info['control']}/_/bodies_to", sent)
        assert status == 400, f"{sent!r} -> {status} {body}"
        assert "suffix" in body["error"]


def test_gx_restart_leader_and_rewrite_home_composite(gx_fake):
    """`restart_leader` on its own does not rewrite the discovery record (by
    design — see `fake_gx.py:restart_leader`'s docstring); the composite must
    do both, in that order, and the rewritten record must carry the NEW
    instance id."""
    home = Path(gx_fake.info["home"])
    record_path = home / "gx-remote-0123456789abcdef.json"
    before = json.loads(record_path.read_text())
    assert before["instanceId"] == "facade00facade00facade00facade00"

    result = gx_fake.call("restart_leader_and_rewrite_home", args=["new-instance-id"])
    assert result["instance_id"] == "new-instance-id"

    after = json.loads(record_path.read_text())
    assert after["instanceId"] == "new-instance-id"


def test_gx_hold_seed_blocks_the_history_get_until_released(gx_fake):
    gx_fake.call("add_session", args=["s1"])
    gx_fake.call("push_update", args=["s1", {"eventId": "s1-1", "method": "session/update",
                                             "params": {"sessionId": "s1"}}])
    token = _gx_token(gx_fake)

    gx_fake.call("hold_seed")

    result: dict = {}

    def _do_get():
        status, body = _get(
            f"{gx_fake.info['reported_url']}/v1/sessions/s1/history",
            headers={"Authorization": f"Bearer {token}"}, timeout=10)
        result["status"], result["body"] = status, body

    t = threading.Thread(target=_do_get)
    t.start()
    try:
        # NEGATIVE-CONTROL LINE: while `hold_seed` is in effect, the GET must
        # still be parked half a second later. Flipping `hold_seed`/
        # `release_seed` in fake_gx.py to be a no-op (or swapping this
        # assertion to `is False`) makes this line fail — the assertion this
        # test exists to make, not incidental timing.
        time.sleep(0.3)
        assert t.is_alive() is True, "the seed GET must still be parked"
    finally:
        gx_fake.call("release_seed")
        t.join(timeout=5)
    assert not t.is_alive(), "the seed GET must complete once released"
    assert result["status"] == 200
    assert json.loads(result["body"])["totalCount"] == 1


def test_gx_non_seed_route_is_unaffected_by_hold_seed(gx_fake):
    """`hold_seed` parks only the history route — every other GET must still
    answer immediately, proving the barrier is narrow rather than
    accidentally server-wide."""
    gx_fake.call("hold_seed")
    try:
        status, _ = _get(f"{gx_fake.info['reported_url']}/v1/healthz", timeout=2)
        assert status == 200
    finally:
        gx_fake.call("release_seed")


def test_gx_stdin_eof_exits_within_two_seconds(gx_fake):
    start = time.time()
    gx_fake.proc.stdin.close()
    gx_fake.proc.wait(timeout=2)
    assert time.time() - start < 2.0
    assert gx_fake.proc.returncode == 0


def test_gx_stop_route_terminates_the_process(gx_fake):
    gx_fake.close(via="stop")
    assert gx_fake.proc.returncode == 0


# -- opencode ----------------------------------------------------------------


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
