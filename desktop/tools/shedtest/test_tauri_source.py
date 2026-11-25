"""This machine as a provisioning source, driven through the real app (plan 035 §3.4,
re-hosted on the delivery stream in plan 039 PR B), and the app-level cells that ran
beside it: the log file, the per-server credential mode and launch at login.

The app binds to a server through its delivery stream (`GET /api/delivery/requests`
with this machine's id): a pinning server refuses another machine's stream `403
SOURCE_NOT_BOUND`, "Answer from here" (`source.use`, or the Credentials view's Where row's
own button) moves the binding here and reopens the stream at once. There is no broker and
no plugin bus. The Where rows were Preferences' Source machine section until plan 040
§3.9 moved them to the Credentials view; the cells that drove that section drive the
view now.

Every cell launches its OWN hermetic app instance (a throwaway HOME/XDG_RUNTIME_DIR →
its own socket, single-instance lock and machine identity) with its own PRIVATE mock
shed-server, so the cells are isolated from each other and from the session instance.
The mock URL is plain `http://` on loopback, so nothing mints over SSH.
"""

from __future__ import annotations

import json
import os
import platform
import shutil
import stat
import subprocess
import sys
import tempfile
import time
from contextlib import contextmanager
from pathlib import Path

import pytest

import hermetic
import ui
from client import ShedError, TauriClient, scaled_timeout
from mockserver import SOURCE_IS_SERVICE_BODY, MockShedServer

FIXTURES = Path(__file__).resolve().parent / "fixtures"
CONFIG = FIXTURES / "config.yaml"
MTLS_CONFIG = FIXTURES / "config-mtls.yaml"
SOURCES_CONFIG = FIXTURES / "config-sources.yaml"
SERVICE_URL = "https://accounts.example.test"

PNG_MAGIC = b"\x89PNG\r\n\x1a\n"
#: Keeps the Credentials view's screenshots (Linux; TCC-gated on macOS).
SHOTS = os.environ.get("SHED_DELIVERY_SHOTS")


def _shot(client, name: str, anchor: str | None = None) -> None:
    if platform.system() == "Darwin":
        return
    if anchor:
        token = client.call("ui.scroll_to", {"anchor": anchor})["token"]
        client.wait_until(lambda: (client.call("ui.scrolled")["scrolled"] or {}).get("token") == token,
                          timeout=10, what=f"scrolled to {anchor}")
    png, w, h = client.screenshot(scale=1)
    assert png[:8] == PNG_MAGIC and w > 0 and h > 0
    if SHOTS:
        out = Path(SHOTS).expanduser()
        out.mkdir(parents=True, exist_ok=True)
        (out / name).write_bytes(png)


# The machine the armed mock binds by default (`mockserver.NOT_SOURCE_ACTIVE`).
OTHER_MACHINE = "fedcba9876543210fedcba9876543210"
# A third machine that takes the source behind the app's back.
LAB_MACHINE = "abcdefabcdefabcdefabcdefabcdef01"

# The delivery stream's backoff after its fourth refusal (1, 2, 4, then 8 s; no
# jitter): with no wake, the next attempt lands exactly this long after it.
_BACKOFF_AFTER_FOUR_S = 8.0
# How soon after that fourth refusal the WOKEN attempt must come. 2 s inside the
# backoff at scale 1, scaled with SHED_TAURI_TEST_TIMEOUT_SCALE, but never closer than
# 0.5 s to the backoff itself, where a wake and the backoff could no longer be told
# apart.
_WAKE_BUDGET_S = min(scaled_timeout(6.0), _BACKOFF_AFTER_FOUR_S - 0.5)


@contextmanager
def _source_app(
    *,
    source: dict | None = None,
    shed_config_path: Path = CONFIG,
    block_state_dir: bool = False,
    runtime_dir: Path | None = None,
    before_launch=None,
    mock: MockShedServer | None = None,
    mock_base_url: str | None = None,
    source_recheck_ms: int | None = None,
    unreachable_hosts: tuple[str, ...] = (),
):
    """Launch a self-managed tauri instance with a PRIVATE mock shed-server, yield
    `(client, mock, info)`, and tear both down. `info` carries `runtime_dir` and
    `proc` (the app's process, for a cell that quits it itself). Every launch goes
    through `hermetic.validate` (in `ui.subproc_env`) before the spawn.

    * `source` — arm the mock's source binding (plan 035 §3.4) with these
      `MockShedServer.arm_source` kwargs (`{}` binds the default other machine, "desk").
    * `shed_config_path` — the app's `~/.shed/config.yaml` (`SHED_TAURI_SHED_CONFIG`).
    * `block_state_dir` — make the state dir (`SHED_BROKER_STATE_DIR`) a regular FILE,
      so no machine identity can be loaded or created.
    * `runtime_dir` — launch over this (caller-owned, kept) HOME instead of a fresh
      throwaway one, so a cell can relaunch over the same prefs, login item and
      machine identity.
    * `before_launch` — called with the runtime dir just before the spawn.
    * `mock` — a mock the caller made and started (it is stopped here); `mock_base_url`
      — the launch's mock URL instead of that mock's own (per-server overrides,
      `hermetic.mock_url`).
    * `source_recheck_ms` — how often a stream settled on the account service is
      checked again (`SHED_TAURI_SOURCE_RECHECK_MS`).
    * `unreachable_hosts` — config server names pointed at a closed port.
    """
    cfg = ui._SUBPROC["tauri"]
    if not cfg.binary.exists():
        raise RuntimeError(
            f"tauri binary not found at {cfg.binary}; build it first (make tauri-build).")
    if mock is None:
        mock = MockShedServer()
        mock.start()
    base_url = mock_base_url or mock.base_url
    if source is not None:
        mock.arm_source(**source)
    owned = runtime_dir is None
    runtime_dir = Path(tempfile.mkdtemp(prefix="shed-src-")) if owned else Path(runtime_dir)
    info: dict = {"runtime_dir": runtime_dir}
    try:
        if block_state_dir:
            # `hermetic.overrides` points SHED_BROKER_STATE_DIR here.
            (runtime_dir / "broker-state").write_text("not a directory\n")
        env = ui.subproc_env(cfg, runtime_dir=runtime_dir, mock_base_url=base_url,
                             config_path=shed_config_path,
                             source_recheck_ms=source_recheck_ms,
                             unreachable_hosts=unreachable_hosts)
        if before_launch is not None:
            before_launch(runtime_dir)
        sock = runtime_dir / cfg.sock_rel
        log = runtime_dir / "source-ui.log"
        log_fh = open(log, "wb")
        proc = subprocess.Popen([str(cfg.binary)], env=env, stdout=log_fh, stderr=subprocess.STDOUT)
        info["proc"] = proc
        try:
            ui.await_hermetic("tauri", sock=sock, mock_base_url=base_url, proc=proc, log=log)
            client = TauriClient(sock)
            try:
                client.wait_until(lambda: client.current_pane() is not None,
                                  timeout=30, what="source frontend ready")
                yield client, mock, info
            finally:
                client.close()
        finally:
            ui.terminate(proc)
            log_fh.close()
    finally:
        if owned:
            shutil.rmtree(runtime_dir, ignore_errors=True)
        mock.stop()


def _stream(client, host: str = "mock") -> dict:
    """`delivery.status`'s stream for `host` (`{"state": None}` before it is listed)."""
    servers = client.call("delivery.status")["servers"]
    return next((s for s in servers if s["server"] == host), {"state": None})


def _machine(client) -> dict:
    return client.call("delivery.status")["machine"]


def _source_row(client, host: str = "mock") -> dict:
    return next(r for r in client.source_status()["rows"] if r["host"] == host)


def _where(client) -> dict:
    """The Credentials view's Where part as it rendered ({} off the view)."""
    return (client.credentials_dump() or {}).get("where") or {}


def _view_row(client, host: str = "mock") -> dict | None:
    for row in _where(client).get("rows") or []:
        if row["host"] == host:
            return row
    return None


def _open_view(client) -> None:
    """Navigate to the Credentials view (each navigation reads every server again) and
    wait until it has reported."""
    client.navigate("credentials")
    client.wait_until(lambda: client.current_pane() == "credentials" and _where(client)
                      and (client.credentials_dump() or {}).get("doors_ready"),
                      timeout=20, what="the Credentials view shown + reported")


def _leave_view(client) -> None:
    client.navigate("sheds")
    client.wait_until(lambda: client.credentials_dump() is None,
                      timeout=10, what="the Credentials view left")


def _holds(pred, seconds: float, what: str) -> None:
    """`pred` stays true for `seconds` (a stale answer must not land at any point)."""
    deadline = time.monotonic() + seconds
    while time.monotonic() < deadline:
        assert pred(), f"no longer holds: {what}"
        time.sleep(0.1)


def _not_bound(client) -> bool:
    return _stream(client)["state"] == "not_bound"


# ---------------------------------------------------------------------------
# The source rows, "Answer from here" over IPC and through the Credentials view
# ---------------------------------------------------------------------------

def test_not_bound_until_use_this_machine_then_connected_at_once():
    """Armed with another machine as the source, the delivery stream reads
    `not_bound` and keeps retrying on its backoff. `source.use` sends THIS machine's
    id (the one `delivery.status` reports and every stream attempt carried), reopens
    the stream, and it connects with no relaunch — sooner than the backoff the
    refusals had built up would have allowed."""
    with _source_app(source={}) as (client, mock, _i):
        client.wait_until(lambda: _not_bound(client), timeout=20, what="the stream not_bound")
        machine = _machine(client)
        assert machine and len(machine["id"]) == 32, machine
        # It keeps retrying: four refusals, so the next backoff is 8 s.
        client.wait_until(lambda: len(mock.delivery_attempts()) >= 4,
                          timeout=30, what="four refused stream attempts")
        for ident in mock.delivery_attempts():
            assert ident == (machine["id"], machine["name"]), ident
        assert mock.delivery_streams() == [], "no stream was admitted"

        status = client.source_status()
        assert status["this_machine"] == {"id": machine["id"], "name": machine["name"]}, status
        row = _source_row(client)
        assert row == {"host": "mock", "supported": True,
                       "source": {"id": OTHER_MACHINE, "name": "desk"},
                       "this_machine": False, "connected": True, "busy": False,
                       "unreachable": False, "error": None}, row

        before = mock.delivery_attempt_times()
        used = client.source_use("mock")
        assert mock.source_use_requests() == [{"machine_id": machine["id"]}]
        assert used["woke_source"] is True, used
        assert used["row"]["this_machine"] is True, used
        assert used["row"]["source"] == {"id": machine["id"], "name": machine["name"]}, used
        assert mock.source_bound() == machine["id"]
        client.wait_until(lambda: _stream(client)["state"] == "connected",
                          timeout=30, what="the stream connected after source.use")
        stream = mock.live_delivery_stream()
        assert stream["machine_id"] == machine["id"], stream
        # The admitted attempt came well inside the 8 s the stream would otherwise have
        # slept after its fourth refusal: the wake, not the backoff, brought it.
        after = mock.delivery_attempt_times()[len(before):]
        assert after, "no stream attempt after the move"
        assert after[0] - before[-1] < _WAKE_BUDGET_S, (_WAKE_BUDGET_S, before, after)
        assert _source_row(client)["this_machine"] is True


def test_answer_from_here_flips_the_where_row():
    """The Credentials view's Where part has one row per configured server: the other
    machine's name with an "Answer from here" button, and a line naming this machine.
    Pressing the REAL button (its `data-source-use` element, clicked by the harness
    door) sends this machine's id; the row then reads "This machine answers here" with
    "Stop answering from here", and the stream connects."""
    with _source_app(source={}) as (client, mock, _i):
        client.wait_until(lambda: _not_bound(client), timeout=20, what="the stream not_bound")
        machine = _machine(client)
        _open_view(client)
        client.wait_until(lambda: (_view_row(client) or {}).get("source_name") == "desk",
                          timeout=20, what="the source row naming desk")
        row = _view_row(client)
        assert row["kind"] == "other_machine" and row["who"] == "Another machine answers here:", row
        assert row["this_machine"] is False and row["can_use"] is True, row
        assert row["action"] == {"id": "use", "label": "Answer from here", "enabled": True}, row
        assert row["busy"] is False and row["error"] is None, row
        where = _where(client)
        assert where["this_machine_name"] == machine["name"], where
        assert where["this_machine_error"] is None, where

        client.press_source_use("mock")
        client.wait_until(lambda: mock.source_use_requests(),
                          timeout=15, what="the button's move request")
        assert mock.source_use_requests() == [{"machine_id": machine["id"]}]
        client.wait_until(
            lambda: (lambda r: r and r["this_machine"] and not r["busy"])(_view_row(client)),
            timeout=15, what="the row flipped to this machine")
        row = _view_row(client)
        assert row["source_name"] == machine["name"], row
        assert row["error"] is None, row
        # No "Answer from here" where this machine already is the source: its action is
        # to stop.
        assert row["can_use"] is False and row["action"]["id"] == "unbind", row
        client.wait_until(lambda: _stream(client)["state"] == "connected",
                          timeout=30, what="the stream connected after the button")
        client.wait_until(lambda: (_view_row(client) or {}).get("detail") == "connected",
                          timeout=20, what="the row saying its stream is connected")


def test_an_older_server_has_a_row_with_no_action():
    """A server older than the feature (the unarmed mock: a plain 404 on
    `/api/sources`) is `supported: false` with no error over IPC, its Where row says so
    and offers no action, and a move is refused as unsupported."""
    with _source_app() as (client, mock, _i):
        mock.live_delivery_stream(timeout=scaled_timeout(20))
        status = client.source_status()
        assert status["rows"] == [{"host": "mock", "supported": False, "source": None,
                                   "this_machine": False, "connected": False,
                                   "busy": False, "unreachable": False,
                                   "error": None}], status
        assert status["this_machine"] is not None, status
        with pytest.raises(ShedError) as e:
            client.source_use("mock")
        assert e.value.code == "unsupported", e.value
        _open_view(client)
        client.wait_until(lambda: _view_row(client) is not None, timeout=20,
                          what="the older server's row")
        row = _view_row(client)
        assert row["kind"] == "unsupported" and row["action"] is None, row
        assert row["who"] == "This server is older than credential sources", row
        assert row["error"] is None, row


def test_refused_moves_show_their_messages():
    """A move the server refuses is reported in fixed words, never the server's: a 403
    under the Where row (pressed through the real button, the row flips back), a
    500 as "could not confirm", an unknown server as a bad request — each one request,
    never retried."""
    with _source_app(source={}) as (client, mock, _i):
        client.wait_until(lambda: _source_row(client)["supported"], timeout=20,
                          what="the source row")
        mock.source_use_script([403, 500])
        _open_view(client)
        client.wait_until(lambda: (_view_row(client) or {}).get("can_use") is True,
                          timeout=20, what="an enabled Use this machine button")
        client.press_source_use("mock")
        client.wait_until(lambda: (_view_row(client) or {}).get("error"),
                          timeout=15, what="the refusal under the row")
        row = _view_row(client)
        assert "does not let this client change its credential source" in row["error"], row
        assert "script key" not in row["error"], row
        assert row["this_machine"] is False and row["busy"] is False, row
        assert len(mock.source_use_requests()) == 1

        with pytest.raises(ShedError) as e:
            client.source_use("mock")
        assert e.value.code == "action_failed", e.value
        assert "could not confirm the move" in e.value.message, e.value
        assert len(mock.source_use_requests()) == 2

        with pytest.raises(ShedError) as e:
            client.source_use("no-such-server")
        assert e.value.code == "bad_request", e.value
        assert len(mock.source_use_requests()) == 2
        assert mock.source_bound() == OTHER_MACHINE


def test_machine_name_with_control_and_bidi_characters_is_shown_cleaned():
    """A machine name is text from another machine: its control and Unicode format
    characters (a bell, a newline, a right-to-left override, a zero-width space) are
    gone wherever it is shown — the source row and the Credentials view."""
    dirty = "desk\N{RIGHT-TO-LEFT OVERRIDE}\u0007\n\N{ZERO WIDTH SPACE}evil"
    with _source_app(source={"machine_name": dirty}) as (client, _m, _i):
        client.wait_until(lambda: _source_row(client)["supported"], timeout=20,
                          what="the source row")
        assert _source_row(client)["source"] == {"id": OTHER_MACHINE, "name": "deskevil"}
        _open_view(client)
        client.wait_until(lambda: (_view_row(client) or {}).get("source_name") == "deskevil",
                          timeout=20, what="the cleaned name in the view")
        assert "deskevil" in _view_row(client)["text"]


def test_no_machine_identity_disables_use_this_machine():
    """With no machine identity (the state dir cannot hold one) no delivery stream
    runs, a move is refused with the reason and nothing is sent, and the Credentials
    view disables the button and says why."""
    with _source_app(source={}, block_state_dir=True) as (client, mock, _i):
        client.wait_until(lambda: _source_row(client)["supported"], timeout=20,
                          what="the source row")
        status = client.source_status()
        assert status["this_machine"] is None, status
        assert status["this_machine_error"], status
        with pytest.raises(ShedError) as e:
            client.source_use("mock")
        assert e.value.code == "machine_identity_unavailable", e.value
        assert e.value.message.startswith("this machine has no identity"), e.value
        assert mock.source_use_requests() == []
        assert mock.delivery_attempts() == [], "no stream without an identity"
        _open_view(client)
        client.wait_until(lambda: _view_row(client) is not None,
                          timeout=20, what="the source row in the view")
        assert _view_row(client)["can_use"] is False
        where = _where(client)
        assert where["this_machine_error"] == status["this_machine_error"], where
        assert "This machine has no identity, so it cannot answer" in \
            client.credentials_dump()["rendered"]


def test_busy_source_read_keeps_the_last_known_source_and_recovers():
    """`503 SOURCE_BUSY` is transient. Over IPC: two busy answers are retried away
    inside one read; busy on every ask is a `busy` row — supported, no error, the
    source unknown rather than absent. In the Credentials view, shown again after
    another machine took the source while the server is busy: the row keeps the source
    it last showed, marked retrying (never "no source"), and the one later read brings
    the new one."""
    with _source_app(source={}) as (client, mock, _i):
        client.wait_until(lambda: _source_row(client)["supported"], timeout=20,
                          what="the source row")
        gets = mock.source_gets()
        mock.source_busy(2, retry_after="0")
        row = _source_row(client)
        assert row["busy"] is False and row["source"]["name"] == "desk", row
        assert mock.source_gets() - gets == 3, "two busy asks, then the answer"

        mock.source_busy(3, retry_after="0")
        row = _source_row(client)
        assert row == {"host": "mock", "supported": True, "source": None,
                       "this_machine": False, "connected": False, "busy": True,
                       "unreachable": False, "error": None}, row

        _open_view(client)
        client.wait_until(lambda: (_view_row(client) or {}).get("source_name") == "desk",
                          timeout=20, what="the source row naming desk")
        _leave_view(client)
        mock.move_source(LAB_MACHINE, "lab")
        mock.source_busy(3, retry_after="0")
        client.navigate("credentials")
        client.wait_until(lambda: (_view_row(client) or {}).get("retrying") is True,
                          timeout=20, what="the row marked busy, retrying")
        row = _view_row(client)
        assert row["source_name"] == "desk", f"the last known source, never none: {row}"
        assert row["error"] is None, row
        client.wait_until(
            lambda: (lambda r: r and r["source_name"] == "lab" and not r["retrying"])(
                _view_row(client)),
            timeout=20, what="the later read naming lab")


def test_source_row_survives_a_server_restart_and_recovers():
    """A read while the server is down (a restart) keeps the Where row the view
    showed — the last known source, noted "cannot reach the server" — instead of
    dropping it, and the view reads again on its own: once the server is back the
    row is current again, with nothing shown, focused or reloaded meanwhile. Over IPC
    the same read is `unreachable`, never `supported: false` with nothing to show."""
    with _source_app(source={}) as (client, mock, _i):
        client.wait_until(lambda: _source_row(client)["supported"], timeout=20,
                          what="the source row")
        _open_view(client)
        client.wait_until(lambda: (_view_row(client) or {}).get("source_name") == "desk",
                          timeout=20, what="the source row naming desk")
        mock.pause()
        try:
            row = _source_row(client)
            assert row["unreachable"] is True and row["error"], row
            # Navigating to the view again reads every server: the read finds it down.
            client.navigate("credentials")
            client.wait_until(lambda: (_view_row(client) or {}).get("unreachable") is True,
                              timeout=20, what="the row kept, marked unreachable")
            row = _view_row(client)
            assert row["source_name"] == "desk", f"the last known source, never gone: {row}"
            assert row["error"] is None, row
            assert "cannot reach the server" in row["detail"], row
            # Another machine takes the source while the server is down; the view must
            # learn it by itself once the server is back.
            mock.move_source(LAB_MACHINE, "lab")
            _holds(lambda: (_view_row(client) or {}).get("source_name") == "desk",
                   1.0, "the row stays while the server is down")
        finally:
            mock.resume()
        client.wait_until(
            lambda: (lambda r: r and r["source_name"] == "lab" and not r["unreachable"])(
                _view_row(client)),
            timeout=30, what="the row current again once the server is back")


def test_showing_the_view_again_reads_a_source_taken_elsewhere():
    """The Where rows outlive the view, so showing it again must read the sources
    again: this machine was the source when the view was left, another machine took it
    meanwhile (which ends this machine's stream), and the view says so and offers the
    button again."""
    with _source_app(source={}) as (client, mock, _i):
        client.wait_until(lambda: _not_bound(client), timeout=20, what="the stream not_bound")
        client.source_use("mock")
        client.wait_until(lambda: _stream(client)["state"] == "connected",
                          timeout=30, what="the stream connected")
        _open_view(client)
        client.wait_until(lambda: (_view_row(client) or {}).get("this_machine") is True,
                          timeout=20, what="the row reading this machine")
        assert _view_row(client)["can_use"] is False
        _leave_view(client)
        mock.move_source(OTHER_MACHINE, "desk")
        client.wait_until(lambda: _not_bound(client), timeout=30,
                          what="the stream ended by the move and refused after it")
        client.navigate("credentials")
        client.wait_until(
            lambda: (lambda r: r and r["source_name"] == "desk" and not r["this_machine"]
                     and r["can_use"])(_view_row(client)),
            timeout=20, what="the view naming desk with the button back")


def test_a_read_overtaken_by_a_move_never_lands_over_it():
    """A read the view started BEFORE a move, answered after the move's own answer
    and read-back (the server took its document first), is older: it must not put
    the other machine back on the row."""
    with _source_app(source={}) as (client, mock, _i):
        client.wait_until(lambda: _not_bound(client), timeout=20, what="the stream not_bound")
        machine = _machine(client)
        _open_view(client)
        client.wait_until(lambda: (_view_row(client) or {}).get("can_use") is True,
                          timeout=20, what="an enabled Answer from here button")
        gets = mock.source_gets()
        mock.source_reads_late(3.0)
        # Navigating to the view again reads every server: that read is the late one.
        client.navigate("credentials")
        client.wait_until(lambda: mock.source_gets() > gets, timeout=10,
                          what="the view's read reaching the server (desk)")
        client.press_source_use("mock")
        client.wait_until(
            lambda: (lambda r: r and r["this_machine"] and not r["busy"])(_view_row(client)),
            timeout=15, what="the move confirmed")
        client.wait_until(lambda: mock.source_late_answered() >= 1, timeout=15,
                          what="the overtaken read answered")
        _holds(lambda: (_view_row(client) or {}).get("source_name") == machine["name"],
               1.5, "the row still names this machine after the stale answer")


def test_moves_on_two_servers_at_once_and_a_second_press_lands_nowhere(tmp_path):
    """Two servers' buttons pressed back to back are both in flight at once (a move
    on one server never disables another's); a second press on a row whose move is
    in flight lands nowhere; both settle on this machine."""
    cfg = tmp_path / "config.yaml"
    cfg.write_text(CONFIG.read_text().replace(
        "default_server: mock",
        "    mock2:\n        host: 127.0.0.1\n        http_port: 8081\n"
        "        ssh_port: 2222\ndefault_server: mock"))
    with _source_app(source={}, shed_config_path=cfg) as (client, mock, _i):
        client.wait_until(lambda: _not_bound(client), timeout=20, what="the stream not_bound")
        _open_view(client)
        both = ("mock", "mock2")
        client.wait_until(
            lambda: all((_view_row(client, h) or {}).get("can_use") for h in both),
            timeout=20, what="both servers' buttons enabled")
        mock.delay_source_use(1.5)
        client.press_source_use("mock")
        client.press_source_use("mock2")
        client.wait_until(
            lambda: all((_view_row(client, h) or {}).get("busy") for h in both),
            timeout=5, what="both moves in flight at once")
        client.press_source_use("mock")  # the row's move is in flight: no button
        client.wait_until(
            lambda: all((lambda r: r and r["this_machine"] and not r["busy"])(
                _view_row(client, h)) for h in both),
            timeout=20, what="both rows reading this machine")
        assert len(mock.source_use_requests()) == 2, mock.source_use_requests()
        assert not any(_view_row(client, h)["can_use"] for h in both)


# ---------------------------------------------------------------------------
# Criterion 13's UI half: an existing identity is used as it is
# ---------------------------------------------------------------------------

def test_a_cold_launch_uses_the_existing_machine_identity():
    """`<state>/broker/machine.json` (the retired broker's path, kept) is this
    machine's identity: a cold launch over a state dir that already holds one uses it
    as it is — `source.status` and `delivery.status` report the same machine, its
    delivery stream carries that id, and the file is not rewritten. The bound source
    on a pinning server therefore stays this machine across the relaunch."""
    runtime_dir = Path(tempfile.mkdtemp(prefix="shed-idn-"))
    machine_json = runtime_dir / "broker-state" / "broker" / "machine.json"
    try:
        with _source_app(source={}, runtime_dir=runtime_dir) as (client, mock, _i):
            client.wait_until(lambda: _not_bound(client), timeout=20, what="the stream not_bound")
            first = _machine(client)
            client.source_use("mock")
            client.wait_until(lambda: _stream(client)["state"] == "connected",
                              timeout=30, what="the stream connected")
        assert machine_json.is_file(), machine_json
        before = machine_json.read_bytes()
        assert json.loads(before)["id"] == first["id"]

        # A server that has this machine as its bound source (the move above, now
        # on a fresh mock): the relaunch must be admitted at once, as the same machine.
        with _source_app(source={"machine_id": first["id"], "machine_name": first["name"]},
                         runtime_dir=runtime_dir) as (client, mock, _i):
            client.wait_until(lambda: _stream(client)["state"] == "connected",
                              timeout=30, what="the stream admitted as the bound source")
            assert _machine(client) == first
            assert client.source_status()["this_machine"] == first
            assert all(mid == first["id"] for mid, _ in mock.delivery_attempts())
            assert _source_row(client)["this_machine"] is True
        assert machine_json.read_bytes() == before, "the identity file was rewritten"
    finally:
        shutil.rmtree(runtime_dir, ignore_errors=True)


# ---------------------------------------------------------------------------
# The app's log file (#414): where it is, and what it records
# ---------------------------------------------------------------------------

def _log_lines(path) -> list[str]:
    try:
        return path.read_text(errors="replace").splitlines()
    except FileNotFoundError:
        return []


def test_log_file_records_startup_and_shutdown():
    """The app writes a log file (#414) and says where: `identify.log_path` is the
    platform default under the harness's redirected HOME / `XDG_STATE_HOME`, created
    0600 in 0700 directories the app made. It holds the startup line (version,
    platform, log path) and where the delivery settings are read from — and, after an
    orderly quit, every shutdown phase and the closing line, which an app launched
    from Finder or a desktop launcher (stderr discarded) used to lose."""
    with _source_app() as (client, mock, info):
        log = ui.app_log_path(info["runtime_dir"])
        assert client.identify()["log_path"] == str(log)
        mock.live_delivery_stream(timeout=scaled_timeout(20))
        client.wait_until(
            lambda: any("shed-desktop-tauri: delivery: settings at " in line
                        for line in _log_lines(log)),
            timeout=20, what="the delivery settings line in the log")
        lines = _log_lines(log)
        starting = [line for line in lines if "shed-desktop-tauri: starting: version " in line]
        assert len(starting) == 1, lines
        assert starting[0].endswith(f"log {log}"), starting
        assert not any("broker" in line for line in lines if "broker-state" not in line), lines
        # Every line is stamped `<RFC 3339 UTC> <LEVEL> …`.
        for line in lines:
            ts, _, rest = line.partition(" ")
            assert ts.endswith("Z") and "T" in ts, line
            assert rest.lstrip().split(" ", 1)[0] in {"ERROR", "WARN", "INFO", "DEBUG"}, line
        assert stat.S_IMODE(log.stat().st_mode) == 0o600
        assert stat.S_IMODE(log.parent.stat().st_mode) == 0o700

        # An orderly quit (the tray's Quit, through the test-mode door): the
        # shutdown's phases land in the file before the process exits.
        client.call("app.quit")
        code = info["proc"].wait(timeout=scaled_timeout(20))
        assert code == 0, f"an orderly quit exits 0, not {code}"
        lines = _log_lines(log)
        for n in range(5):
            assert any(f"shed-desktop-tauri: shutdown: phase {n} (" in line for line in lines), \
                f"phase {n} missing: {[l for l in lines if 'shutdown' in l]}"
        assert any("shed-desktop-tauri: shutdown: done in " in line for line in lines), lines


# ---------------------------------------------------------------------------
# Per-server credential mode (plan 002 C3)
# ---------------------------------------------------------------------------

def test_hosts_auth_surfaces_the_config_mode():
    """`hosts.auth` reports each server's credential mode: one registry seeded from
    config and updated by the core's CredentialObserver.

    `learned` must be False everywhere: the hermetic mock is open (plain http), so
    nothing has been proven, and a surface that echoed the config into `learned`
    would be lying about the one thing it exists to report.
    """
    with _source_app(shed_config_path=MTLS_CONFIG) as (client, _mock, _info):
        hosts = {h["name"]: h for h in client.hosts_auth()}
        assert set(hosts) == {"legacy", "mock"}
        assert hosts["mock"]["auth_mode"] == "mtls"
        # ABSENT MEANS TOKEN — an entry written before certificates existed.
        assert hosts["legacy"]["auth_mode"] == "token"
        assert all(h["learned"] is False for h in hosts.values())


# ---------------------------------------------------------------------------
# Launch at login is on by default, once (plan 037 §3.1.4)
# ---------------------------------------------------------------------------

def _prefs_file(runtime_dir: Path) -> Path:
    return ui.app_config_dir(runtime_dir) / "prefs.json"


def _autostart_entries(runtime_dir: Path) -> list[Path]:
    d = ui.linux_autostart_dir(runtime_dir)
    return sorted(d.glob("*.desktop")) if d.is_dir() else []


def test_login_item_default_once():
    """The first launch turns launch at login on and records that it did; once the
    person switches it off, a relaunch over the same prefs and HOME leaves it off.
    On Linux the login item is a real `.desktop` under the throwaway HOME; on macOS
    test mode keeps it in memory (a fresh launch reads off), so there the relaunch
    shows only that the default did not run again."""
    runtime_dir = Path(tempfile.mkdtemp(prefix="shed-login-"))
    linux = sys.platform.startswith("linux")
    try:
        with _source_app(runtime_dir=runtime_dir) as (client, _mock, _info):
            assert client.login_item_status() is True
            if linux:
                assert _autostart_entries(runtime_dir), "no login item under the throwaway HOME"
            client.login_item_set(False)
            assert client.login_item_status() is False
            if linux:
                assert _autostart_entries(runtime_dir) == []
        with _source_app(runtime_dir=runtime_dir) as (client, _mock, _info):
            assert client.login_item_status() is False, "the default ran again"
            if linux:
                assert _autostart_entries(runtime_dir) == []
        assert json.loads(_prefs_file(runtime_dir).read_text())["login_item_defaulted"] is True
    finally:
        shutil.rmtree(runtime_dir, ignore_errors=True)


def test_login_item_not_turned_on_when_prefs_unwritable():
    """An app that cannot record having applied the default never turns launch at
    login on: not at its first launch, and not at the next. The app log says why.

    The prefs file is made a DIRECTORY rather than its directory read-only: the
    Docker render gate runs as root, which a read-only directory does not stop."""
    runtime_dir = Path(tempfile.mkdtemp(prefix="shed-login-ro-"))
    linux = sys.platform.startswith("linux")

    def block(rt: Path) -> None:
        _prefs_file(rt).mkdir(parents=True, exist_ok=True)

    try:
        for launch in (1, 2):
            with _source_app(runtime_dir=runtime_dir, before_launch=block) as (
                client, _mock, _info,
            ):
                assert client.login_item_status() is False, f"turned on at launch {launch}"
                if linux:
                    assert _autostart_entries(runtime_dir) == []
                log = ui.app_log_path(runtime_dir)
                client.wait_until(
                    lambda: log.exists() and log.read_text(errors="replace").count(
                        "could not record that launch at login was set up") >= launch,
                    timeout=15, what=f"the could-not-record line (launch {launch})")
    finally:
        shutil.rmtree(runtime_dir, ignore_errors=True)



# ---------------------------------------------------------------------------
# The Credentials view's Rust side (plan 040 §3.9, B1): who answers on each server,
# the account service as a settled state, and "Stop answering from here"
# ---------------------------------------------------------------------------

def _list_row(client, host: str) -> dict:
    return next(r for r in client.sources_list()["servers"] if r["server"] == host)


def test_sources_list_says_who_answers_on_each_server():
    """`sources.list` has one row per configured server, saying who answers there:
    this machine (with its stream's state), another machine by name, the account
    service with its URL (and `not_delivering` when the server says so), nobody, or a
    server older than sources. Where the account service answers, this machine's
    stream is settled on it (`service`), not retrying."""
    mocks = {name: MockShedServer() for name in
             ("here", "desk", "account", "paused", "fresh", "old")}
    for m in mocks.values():
        m.start()
    try:
        mocks["here"].arm_source_unbound()
        mocks["desk"].arm_source()
        mocks["account"].serve_service_source()
        mocks["paused"].serve_service_source("not_delivering")
        mocks["fresh"].arm_source_unbound()
        mocks["fresh"].refuse_delivery_stream(503, "DELIVERY_DISABLED", times=1000)
        url = hermetic.mock_url(mocks["here"].base_url,
                                {n: m.base_url for n, m in mocks.items() if n != "here"})
        with _source_app(mock=mocks.pop("here"), mock_base_url=url,
                         shed_config_path=SOURCES_CONFIG) as (client, here, _i):
            mocks["here"] = here
            client.wait_until(lambda: _stream(client, "here")["state"] == "connected",
                              timeout=30, what="this machine bound on here")
            client.wait_until(lambda: _stream(client, "desk")["state"] == "not_bound",
                              timeout=30, what="desk's stream refused")
            for name in ("account", "paused"):
                client.wait_until(lambda n=name: _stream(client, n)["state"] == "service",
                                  timeout=30, what=f"{name}'s stream settled on the service")
            machine = _machine(client)
            listed = client.sources_list()
            assert listed["this_machine"] == {"id": machine["id"], "name": machine["name"]}
            assert listed["this_machine_error"] is None
            rows = {r["server"]: r for r in listed["servers"]}
            assert list(rows) == sorted(rows) == ["account", "desk", "fresh", "here", "old",
                                                  "paused"], rows
            kinds = {n: r["kind"] for n, r in rows.items()}
            assert kinds == {"here": "this_machine", "desk": "other_machine",
                             "account": "service", "paused": "service",
                             "fresh": "nobody", "old": "unsupported"}, kinds

            assert rows["here"]["machine"] == {"id": machine["id"], "name": machine["name"]}
            assert rows["here"]["stream"]["state"] == "connected", rows["here"]
            assert rows["desk"]["machine"] == {"id": OTHER_MACHINE, "name": "desk"}
            assert rows["desk"]["stream"]["state"] == "not_bound", rows["desk"]
            for name, state in (("account", None), ("paused", "not_delivering")):
                r = rows[name]
                assert (r["url"], r["state"], r["machine"]) == (SERVICE_URL, state, None), r
                assert r["stream"]["state"] == "service", r
                assert r["stream"]["last_error"] == "source_is_service", r
            for name in ("here", "desk", "fresh", "old"):
                assert (rows[name]["url"], rows[name]["state"]) == (None, None), rows[name]
            assert rows["fresh"]["machine"] is None and rows["fresh"]["error"] is None
            assert rows["old"]["error"] is None
            for r in rows.values():
                assert (r["busy"], r["unreachable"]) == (False, False), r
            # Settled, not retrying: the one refused attempt, and no more, held.
            for name in ("account", "paused"):
                client.wait_until(lambda n=name: len(mocks[n].delivery_attempts()) >= 1,
                                  timeout=10, what=f"{name}'s refused attempt")
            _holds(lambda: all(len(mocks[n].delivery_attempts()) == 1
                               for n in ("account", "paused")), 1.5,
                   "one refused attempt each, and no reconnect")
            # `source.status` carries the same: the service on its rows, nothing else.
            status = {r["host"]: r for r in client.source_status()["rows"]}
            assert status["account"]["service"] == {"url": SERVICE_URL, "state": None}
            assert status["paused"]["service"]["state"] == "not_delivering"
            assert "service" not in status["here"], status["here"]
            # A move or an unbind there is refused in fixed words. The move reaches the
            # server; the unbind is refused before anything is paused or sent, so the
            # settled stream is not stopped and started again.
            for call in (client.source_use, client.source_unbind):
                with pytest.raises(ShedError) as e:
                    call("account")
                assert e.value.code == "source_is_service", e.value
                assert e.value.message == ("the account service answers for you on "
                                           "account; no machine does there"), e.value
                assert SOURCE_IS_SERVICE_BODY["error"]["message"] not in e.value.message
            assert len(mocks["account"].source_use_requests()) == 1
            assert mocks["account"].source_unbind_requests() == []
            assert _stream(client, "account")["state"] == "service"
            assert client.call("delivery.status")["paused"] == []
            _holds(lambda: len(mocks["account"].delivery_attempts()) == 1, 1.5,
                   "no stream restarted by the refused unbind")

            # Criterion 15: the Credentials view's Where part draws each state, says
            # "not delivering" where the server says so, and offers no action where the
            # account service answers.
            client.navigate("credentials")
            client.wait_until(
                lambda: len(_where(client).get("rows") or []) == 6
                and all(r["kind"] != "unknown" for r in _where(client)["rows"])
                and (_view_row(client, "here") or {}).get("detail") == "connected",
                timeout=20, what="six Where rows")
            shown = {r["host"]: r for r in _where(client)["rows"]}
            assert list(shown) == ["account", "desk", "fresh", "here", "old", "paused"], shown
            assert {h: r["kind"] for h, r in shown.items()} == kinds
            assert shown["here"]["who"] == "This machine answers here"
            assert shown["here"]["action"]["label"] == "Stop answering from here"
            assert shown["desk"]["who"] == "Another machine answers here:"
            assert shown["desk"]["source_name"] == "desk" and "desk" in shown["desk"]["text"]
            assert shown["desk"]["action"]["label"] == "Answer from here"
            for name in ("account", "paused"):
                r = shown[name]
                assert r["who"] == "The account service answers here", r
                assert r["url"] == SERVICE_URL and r["action"] is None, r
            assert shown["account"]["not_delivering"] is False
            assert shown["paused"]["not_delivering"] is True
            assert shown["paused"]["detail"] == "It is not delivering to this server."
            assert shown["fresh"]["who"] == "Nobody answers here"
            assert shown["fresh"]["action"]["label"] == "Answer from here"
            assert shown["old"]["who"] == "This server is older than credential sources"
            assert shown["old"]["action"] is None
            assert SOURCE_IS_SERVICE_BODY["error"]["message"] not in json.dumps(
                client.credentials_dump())
            _shot(client, "where.png", "credentials-where")
            client.call("ui.set_appearance", {"mode": "dark"})
            client.wait_until(lambda: (client.computed_style() or {}).get("mode") == "dark",
                              timeout=10, what="dark")
            _shot(client, "where-dark.png", "credentials-where")
            client.call("ui.set_appearance", {"mode": "light"})
    finally:
        for m in mocks.values():
            m.stop()


def test_the_account_service_stops_the_stream_and_a_recheck_reopens_it():
    """Criterion 15: on `409 SOURCE_IS_SERVICE` the stream stops reconnecting. It
    re-checks `GET /api/sources` on its timer (shortened here) and, while the service
    still answers, opens no stream at all. When the service stops answering (the name
    moved to the server's local admins), the next re-check opens the stream, which is
    bound and connected."""
    mock = MockShedServer()
    mock.start()
    mock.serve_service_source()
    with _source_app(mock=mock, source_recheck_ms=400) as (client, mock, _i):
        client.wait_until(lambda: _stream(client)["state"] == "service",
                          timeout=30, what="the stream settled on the service")
        stream = _stream(client)
        assert stream["last_error"] == "source_is_service", stream
        gets = mock.source_gets()
        # Re-checks come on the timer; each is a sources read, and none is a stream.
        client.wait_until(lambda: _stream(client)["service_checks"] >= 3,
                          timeout=30, what="three re-checks")
        client.wait_until(lambda: mock.source_gets() >= gets + 2, timeout=10,
                          what="the re-checks' sources reads")
        assert len(mock.delivery_attempts()) == 1, mock.delivery_attempts()
        assert mock.delivery_streams() == []
        assert _stream(client)["state"] == "service"

        mock.stop_service_source()
        client.wait_until(lambda: _stream(client)["state"] == "connected",
                          timeout=30, what="the stream reopened after the service stopped")
        assert len(mock.delivery_attempts()) == 2, mock.delivery_attempts()
        machine = _machine(client)
        assert mock.source_bound() == machine["id"]
        assert _list_row(client, "mock")["kind"] == "this_machine"


def test_stop_answering_from_here_sticks_until_answer_from_here():
    """`source.unbind` ("Stop answering from here") clears the binding on that server
    and stops this machine answering there: the server ends the stream and, though it
    would bind the first machine to connect, this one does not come back — not on its
    backoff, not after a relaunch. `sources.list` says nobody answers and that this
    machine is paused there. `source.use` ("Answer from here") takes the pause away: the
    stream returns and this machine is the source again. A refused unbind pauses
    nothing; an unknown server is a bad request."""
    home = Path(tempfile.mkdtemp(prefix="shed-src-pause-"))
    try:
        with _source_app(source={}, runtime_dir=home) as (client, mock, _i):
            client.wait_until(lambda: _not_bound(client), timeout=20, what="the stream not_bound")
            machine = _machine(client)
            client.source_use("mock")
            client.wait_until(lambda: _stream(client)["state"] == "connected",
                              timeout=30, what="this machine the source")
            done = client.source_unbind("mock")
            assert done == {"server": "mock", "unbound": True,
                            "previous": {"id": machine["id"], "name": machine["name"]},
                            "previous_was_this_machine": True, "forgotten_machines": 2,
                            "paused": True, "verified": True}, done
            assert mock.source_unbind_requests() == [{}]
            client.wait_until(lambda: _stream(client)["state"] is None, timeout=10,
                              what="no stream runs for the server")
            attempts = len(mock.delivery_attempts())
            # Several backoff floors (1 s): no reconnect, so nothing binds this machine.
            _holds(lambda: len(mock.delivery_attempts()) == attempts
                   and mock.source_bound() is None, 3.5, "no reconnect after the unbind")
            row = _list_row(client, "mock")
            assert (row["kind"], row["paused"], row["stream"]) == ("nobody", True, None), row
            assert client.call("delivery.status")["paused"] == ["mock"]

            with pytest.raises(ShedError) as e:
                client.source_unbind("no-such-server")
            assert e.value.code == "bad_request", e.value
            with pytest.raises(ShedError) as e:
                client.call("source.unbind", {})
            assert e.value.code == "bad_request", e.value
            assert len(mock.source_unbind_requests()) == 1

        # Across a relaunch over the same HOME: still no stream for it.
        fresh = MockShedServer()
        fresh.start()
        fresh.arm_source_unbound()
        with _source_app(mock=fresh, runtime_dir=home) as (client, mock, _i):
            assert _machine(client)["id"] == machine["id"]
            _holds(lambda: mock.delivery_attempts() == [], 2.5, "no stream after the relaunch")
            assert _list_row(client, "mock")["paused"] is True
            # "Answer from here": the stream returns, and this machine is the source.
            used = client.source_use("mock")
            assert used["row"]["this_machine"] is True, used
            client.wait_until(lambda: _stream(client)["state"] == "connected",
                              timeout=30, what="the stream back after source.use")
            assert mock.source_bound() == machine["id"]
            row = _list_row(client, "mock")
            assert (row["kind"], row["paused"]) == ("this_machine", False), row
            assert client.call("delivery.status")["paused"] == []
    finally:
        shutil.rmtree(home, ignore_errors=True)

    # A server older than sources refuses the unbind: nothing is paused.
    with _source_app() as (client, mock, _i):
        mock.live_delivery_stream(timeout=scaled_timeout(20))
        with pytest.raises(ShedError) as e:
            client.source_unbind("mock")
        assert e.value.code == "unsupported", e.value
        row = _list_row(client, "mock")
        assert (row["kind"], row["paused"]) == ("unsupported", False), row
        client.wait_until(lambda: _stream(client)["state"] == "connected", timeout=30,
                          what="the stream still runs")



def test_stop_answering_from_here_through_the_view():
    """The Where row's own "Stop answering from here" (pressed through the real
    button) unbinds and pauses this machine there: the row reads nobody, this machine
    stopped, with "Answer from here", which brings it back. A refused stop puts the row
    back with fixed words."""
    with _source_app(source={}) as (client, mock, _i):
        client.wait_until(lambda: _not_bound(client), timeout=20, what="the stream not_bound")
        machine = _machine(client)
        client.source_use("mock")
        client.wait_until(lambda: _stream(client)["state"] == "connected",
                          timeout=30, what="this machine the source")
        _open_view(client)
        client.wait_until(lambda: (_view_row(client) or {}).get("can_stop") is True,
                          timeout=20, what="Stop answering from here")
        client.press_source_unbind("mock")
        client.wait_until(
            lambda: (lambda r: r and r["kind"] == "nobody" and r["paused"] and not r["busy"])(
                _view_row(client)),
            timeout=20, what="the row reading nobody, this machine stopped")
        assert mock.source_unbind_requests() == [{}]
        row = _view_row(client)
        assert row["detail"] == "This machine stopped answering here.", row
        assert row["action"] == {"id": "use", "label": "Answer from here", "enabled": True}, row
        assert row["error"] is None, row
        client.wait_until(lambda: _stream(client)["state"] is None, timeout=10,
                          what="no stream runs for the server")

        client.press_source_use("mock")
        client.wait_until(
            lambda: (lambda r: r and r["this_machine"] and not r["busy"])(_view_row(client)),
            timeout=20, what="this machine answering again")
        assert mock.source_bound() == machine["id"]
        client.wait_until(lambda: _stream(client)["state"] == "connected",
                          timeout=30, what="the stream back")
        # A stop the server undoes at once (a late admission, twice) is refused: the row
        # is put back, with fixed words under it.
        client.wait_until(lambda: (_view_row(client) or {}).get("can_stop") is True,
                          timeout=20, what="Stop answering from here again")
        mock.admit_after_unbind(machine["id"], machine["name"], times=2)
        client.press_source_unbind("mock")
        client.wait_until(lambda: (_view_row(client) or {}).get("error"), timeout=30,
                          what="the refusal under the row")
        row = _view_row(client)
        assert row["error"] == ("mock bound this machine again after the stop; it stays "
                                "stopped here. Try again."), row
        # Bound here and yet stopped (the uncertain stop): the row offers Answer from
        # here, which undoes the pause, beside Stop answering from here.
        client.navigate("credentials")
        client.wait_until(
            lambda: (lambda r: r and r["kind"] == "this_machine" and r["paused"])(
                _view_row(client)), timeout=20, what="this machine bound and paused")
        row = _view_row(client)
        assert [(a["id"], a["label"], a["enabled"]) for a in row["actions"]] == [
            ("use", "Answer from here", True),
            ("unbind", "Stop answering from here", True)], row
        client.press_source_use("mock")
        client.wait_until(lambda: _stream(client)["state"] == "connected", timeout=30,
                          what="the stream back after Answer from here")
        client.navigate("credentials")
        client.wait_until(
            lambda: (lambda r: r and r["kind"] == "this_machine" and not r["paused"]
                     and [a["id"] for a in r["actions"]] == ["unbind"])(_view_row(client)),
            timeout=20, what="answering here again, with only the stop offered")


def test_a_server_added_while_the_app_runs_is_listed_and_actionable(tmp_path):
    """The sources are the shed config's as it reads now, not as it read at start: a
    server added while the app runs is listed (IPC and the view) and can be stopped on;
    removed again, it is gone and an unbind naming it is a bad request."""
    cfg = tmp_path / "config.yaml"
    base = CONFIG.read_text()
    cfg.write_text(base)
    with _source_app(source={}, shed_config_path=cfg) as (client, mock, _i):
        hosts = lambda: [r["server"] for r in client.sources_list()["servers"]]
        assert hosts() == ["mock"]
        with pytest.raises(ShedError) as e:
            client.source_unbind("mock2")
        assert e.value.code == "bad_request", e.value
        cfg.write_text(base.replace(
            "default_server: mock",
            "    mock2:\n        host: 127.0.0.1\n        http_port: 8081\n"
            "        ssh_port: 2222\ndefault_server: mock"))
        assert hosts() == ["mock", "mock2"]
        _open_view(client)
        client.wait_until(lambda: _view_row(client, "mock2") is not None, timeout=20,
                          what="the added server's row")
        done = client.source_unbind("mock2")
        assert done["server"] == "mock2" and done["unbound"] is True, done
        assert mock.source_unbind_requests() == [{}]
        # Gone from the config: gone from the list, and refused again.
        cfg.write_text(base)
        assert hosts() == ["mock"]
        with pytest.raises(ShedError) as e:
            client.source_unbind("mock2")
        assert e.value.code == "bad_request", e.value
        assert len(mock.source_unbind_requests()) == 1


def test_a_config_that_cannot_be_read_keeps_the_rows_it_last_listed(tmp_path):
    """A shed config the reader refuses is not an empty one: the servers it last listed
    stay (IPC and the view), with a fixed line saying the config cannot be read, and an
    action is refused `config_unreadable` with nothing sent. Read again, all is as it
    was."""
    cfg = tmp_path / "config.yaml"
    base = CONFIG.read_text()
    cfg.write_text(base)
    with _source_app(source={}, shed_config_path=cfg) as (client, mock, _i):
        assert [r["server"] for r in client.sources_list()["servers"]] == ["mock"]
        cfg.write_text(base + "servers:\n")  # a second `servers:` key: refused whole
        listed = client.sources_list()
        assert listed["config_error"] == "unreadable", listed
        assert [r["server"] for r in listed["servers"]] == ["mock"], "never dropped"
        for call in (client.source_unbind, client.source_use):
            with pytest.raises(ShedError) as e:
                call("mock")
            assert e.value.code == "config_unreadable", e.value
        assert mock.source_unbind_requests() == [] and mock.source_use_requests() == []
        _open_view(client)
        client.wait_until(lambda: (_where(client).get("config_error") or "").startswith(
            "Your shed config cannot be read"), timeout=20, what="the config line")
        assert _view_row(client, "mock") is not None
        # Negative control: read fine again, no line and no refusal.
        cfg.write_text(base)
        assert client.sources_list()["config_error"] is None
        client.navigate("credentials")
        client.wait_until(lambda: _where(client).get("config_error") is None, timeout=20,
                          what="the config line gone")
        client.source_unbind("mock")
        assert mock.source_unbind_requests() == [{}]


def test_a_server_unreachable_from_the_start_has_a_row_and_a_tick(tmp_path):
    """A configured server that cannot be reached at the first read still has its
    Where row (fixed words, no "Reading your servers…" once the read has landed) and
    its tick in a scope; with every server offline the view still says so."""
    cfg = tmp_path / "config.yaml"
    cfg.write_text(CONFIG.read_text().replace(
        "default_server: mock",
        "    mock2:\n        host: 127.0.0.1\n        http_port: 8081\n"
        "        ssh_port: 2222\ndefault_server: mock"))
    with _source_app(source={}, shed_config_path=cfg, unreachable_hosts=("mock2",)) as (
            client, _mock, _i):
        _open_view(client)
        client.wait_until(lambda: _view_row(client, "mock2") is not None
                          and _view_row(client, "mock") is not None, timeout=30,
                          what="both servers' rows")
        row = _view_row(client, "mock2")
        assert row["who"] == "This server cannot be reached" and row["unreachable"], row
        assert row["kind"] == "unknown" and row["error"], row
        assert "Reading your servers" not in client.credentials_dump()["rendered"]
        # Its tick is offered in a scope.
        client.call("ui.press_delivery_button", {"provider": "github", "button": "setup"})
        client.wait_until(lambda: ((client.credentials_dump() or {}).get("what") or {})
                          .get("editor"), timeout=15, what="github's form")
        client.call("ui.press_delivery_button", {"provider": "github", "button": "scope:only"})
        client.wait_until(
            lambda: "server:mock2" in (client.credentials_dump()["what"]["editor"] or {})
            .get("values", {}), timeout=10, what="mock2's tick")
    with _source_app(unreachable_hosts=("mock",)) as (client, _mock, _i):
        _open_view(client)
        client.wait_until(lambda: (_view_row(client) or {}).get("unreachable"), timeout=30,
                          what="the offline server's row")
        assert "Reading your servers" not in client.credentials_dump()["rendered"]


def test_a_pause_shows_whoever_answers():
    """This machine stopped answering on a server, and another machine answers there
    now: the row names that machine and still says this machine stopped answering."""
    with _source_app(source={}) as (client, mock, _i):
        client.wait_until(lambda: _not_bound(client), timeout=20, what="the stream not_bound")
        client.source_use("mock")
        client.wait_until(lambda: _stream(client)["state"] == "connected",
                          timeout=30, what="this machine the source")
        client.source_unbind("mock")
        mock.move_source(OTHER_MACHINE, "desk")
        _open_view(client)
        client.wait_until(lambda: (_view_row(client) or {}).get("kind") == "other_machine",
                          timeout=20, what="desk answering")
        row = _view_row(client)
        assert row["paused"] is True and row["paused_shown"] is True, row
        assert "this machine stopped answering here" in row["detail"], row
        assert row["source_name"] == "desk", row


def test_an_unusable_paused_file_starts_no_stream():
    """Finding 1: a paused-servers file that exists and cannot be parsed fails closed —
    no stream starts, every server reads as paused, and `sources.list` says why with a
    class. "Answer from here" writes it afresh and the stream starts."""
    def corrupt(home: Path) -> None:
        broker = home / "broker-state" / "broker"
        broker.mkdir(parents=True, exist_ok=True)
        (broker / "paused-servers.json").write_text("{not json")

    with _source_app(source={}, before_launch=corrupt) as (client, mock, _i):
        client.wait_until(lambda: client.sources_list()["paused_error"] == "unparseable",
                          timeout=20, what="the paused file's error")
        _holds(lambda: mock.delivery_attempts() == [], 2.0, "no stream while it fails closed")
        row = _list_row(client, "mock")
        assert (row["paused"], row["stream"]) == (True, None), row
        assert client.call("delivery.status")["paused_error"] == "unparseable"
        client.source_use("mock")
        client.wait_until(lambda: _stream(client)["state"] == "connected", timeout=30,
                          what="the stream after Answer from here")
        assert client.sources_list()["paused_error"] is None
        assert _list_row(client, "mock")["paused"] is False



def test_a_late_admission_after_an_unbind_is_unbound_again_or_reported():
    """A stream attempt the server received before the unbind can be admitted after it
    and bind this machine again. `source.unbind` reads the sources once afterwards: bound
    again, it unbinds once more (and says `verified`); still bound after that, it is the
    fixed refusal `rebound`, and this machine stays paused there."""
    with _source_app(source={}) as (client, mock, _i):
        client.wait_until(lambda: _not_bound(client), timeout=20, what="the stream not_bound")
        machine = _machine(client)
        client.source_use("mock")
        client.wait_until(lambda: _stream(client)["state"] == "connected",
                          timeout=30, what="this machine the source")

        mock.admit_after_unbind(machine["id"], machine["name"])
        done = client.source_unbind("mock")
        assert done["unbound"] is True and done["verified"] is True, done
        assert len(mock.source_unbind_requests()) == 2, "the late admission unbound again"
        assert mock.source_bound() is None

        client.source_use("mock")
        client.wait_until(lambda: _stream(client)["state"] == "connected",
                          timeout=30, what="this machine the source again")
        mock.admit_after_unbind(machine["id"], machine["name"], times=2)
        with pytest.raises(ShedError) as e:
            client.source_unbind("mock")
        assert e.value.code == "rebound", e.value
        assert e.value.message == ("mock bound this machine again after the unbind; it "
                                   "stays stopped here: try again"), e.value
        assert len(mock.source_unbind_requests()) == 4
        assert mock.source_bound() == machine["id"]
        row = _list_row(client, "mock")
        assert (row["paused"], row["stream"]) == (True, None), row
        _holds(lambda: _stream(client)["state"] is None, 2.0, "no stream after rebound")
