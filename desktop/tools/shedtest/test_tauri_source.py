"""This machine as a provisioning source, driven through the real app (plan 035 §3.4,
re-hosted on the delivery stream in plan 039 PR B), and the app-level cells that ran
beside it: the log file, the per-server credential mode and launch at login.

The app binds to a server through its delivery stream (`GET /api/delivery/requests`
with this machine's id): a pinning server refuses another machine's stream `403
SOURCE_NOT_BOUND`, "Use this machine" (`source.use`, or the Preferences row's own
button) moves the binding here and reopens the stream at once. There is no broker and
no plugin bus.

Every cell launches its OWN hermetic app instance (a throwaway HOME/XDG_RUNTIME_DIR →
its own socket, single-instance lock and machine identity) with its own PRIVATE mock
shed-server, so the cells are isolated from each other and from the session instance.
The mock URL is plain `http://` on loopback, so nothing mints over SSH.
"""

from __future__ import annotations

import json
import shutil
import stat
import subprocess
import sys
import tempfile
import time
from contextlib import contextmanager
from pathlib import Path

import pytest

import ui
from client import ShedError, TauriClient, scaled_timeout
from mockserver import MockShedServer

FIXTURES = Path(__file__).resolve().parent / "fixtures"
CONFIG = FIXTURES / "config.yaml"
MTLS_CONFIG = FIXTURES / "config-mtls.yaml"

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
    """
    cfg = ui._SUBPROC["tauri"]
    if not cfg.binary.exists():
        raise RuntimeError(
            f"tauri binary not found at {cfg.binary}; build it first (make tauri-build).")
    mock = MockShedServer()
    mock.start()
    if source is not None:
        mock.arm_source(**source)
    owned = runtime_dir is None
    runtime_dir = Path(tempfile.mkdtemp(prefix="shed-src-")) if owned else Path(runtime_dir)
    info: dict = {"runtime_dir": runtime_dir}
    try:
        if block_state_dir:
            # `hermetic.overrides` points SHED_BROKER_STATE_DIR here.
            (runtime_dir / "broker-state").write_text("not a directory\n")
        env = ui.subproc_env(cfg, runtime_dir=runtime_dir, mock_base_url=mock.base_url,
                             config_path=shed_config_path)
        if before_launch is not None:
            before_launch(runtime_dir)
        sock = runtime_dir / cfg.sock_rel
        log = runtime_dir / "source-ui.log"
        log_fh = open(log, "wb")
        proc = subprocess.Popen([str(cfg.binary)], env=env, stdout=log_fh, stderr=subprocess.STDOUT)
        info["proc"] = proc
        try:
            ui.await_hermetic("tauri", sock=sock, mock_base_url=mock.base_url, proc=proc, log=log)
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


def _prefs_report(client) -> dict:
    return client.prefs_dump().get("prefs") or {}


def _prefs_source(client, host: str = "mock") -> dict | None:
    for row in (_prefs_report(client).get("values") or {}).get("sources") or []:
        if row["host"] == host:
            return row
    return None


def _open_prefs(client) -> None:
    client.show_preferences()
    client.wait_until(lambda: client.prefs_dump()["visible"] and _prefs_report(client),
                      timeout=20, what="preferences window visible + reported")


def _close_prefs(client) -> None:
    client.prefs_close()
    client.wait_until(lambda: not client.prefs_dump()["visible"],
                      timeout=10, what="preferences window hidden")


def _holds(pred, seconds: float, what: str) -> None:
    """`pred` stays true for `seconds` (a stale answer must not land at any point)."""
    deadline = time.monotonic() + seconds
    while time.monotonic() < deadline:
        assert pred(), f"no longer holds: {what}"
        time.sleep(0.1)


def _not_bound(client) -> bool:
    return _stream(client)["state"] == "not_bound"


# ---------------------------------------------------------------------------
# The source rows, "Use this machine" over IPC and through the Preferences button
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


def test_preferences_source_row_flips_after_use_this_machine():
    """Preferences shows one row per supporting server: the other machine's name with
    a "Use this machine" button, and a line naming this machine. Pressing the REAL
    button (its `data-source-use` element, clicked by the harness door) sends this
    machine's id; the row then reads "this machine" and the stream connects."""
    with _source_app(source={}) as (client, mock, _i):
        client.wait_until(lambda: _not_bound(client), timeout=20, what="the stream not_bound")
        machine = _machine(client)
        _open_prefs(client)
        client.wait_until(lambda: (_prefs_source(client) or {}).get("source_name") == "desk",
                          timeout=20, what="the source row naming desk")
        row = _prefs_source(client)
        assert row["this_machine"] is False and row["can_use"] is True, row
        assert row["busy"] is False and row["error"] is None, row
        values = _prefs_report(client)["values"]
        assert values["this_machine_name"] == machine["name"], values
        assert values["this_machine_error"] is None, values

        client.press_source_use("mock")
        client.wait_until(lambda: mock.source_use_requests(),
                          timeout=15, what="the button's move request")
        assert mock.source_use_requests() == [{"machine_id": machine["id"]}]
        client.wait_until(
            lambda: (lambda r: r and r["this_machine"] and not r["busy"])(_prefs_source(client)),
            timeout=15, what="the row flipped to this machine")
        row = _prefs_source(client)
        assert row["source_name"] == machine["name"], row
        assert row["error"] is None, row
        # No button where this machine already is the source, and none reported.
        assert row["can_use"] is False, row
        client.wait_until(lambda: _stream(client)["state"] == "connected",
                          timeout=30, what="the stream connected after the button")


def test_unarmed_server_has_no_source_rows():
    """A server older than the feature (the unarmed mock: a plain 404 on
    `/api/sources`) is `supported: false` with no error, Preferences shows no source
    row and no new section, and a move is refused as unsupported."""
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
        _open_prefs(client)
        client.wait_until(lambda: "provisioning" in (_prefs_report(client).get("sections") or []),
                          timeout=20, what="the Provisioning section")
        report = _prefs_report(client)
        assert report["sections"] == ["general", "terminal", "provisioning"], report
        assert report["values"]["sources"] == [], report
        assert report["values"]["this_machine_name"] is None, report


def test_refused_moves_show_their_messages():
    """A move the server refuses is reported in its words: a 403 under the
    Preferences row (pressed through the real button, the row flips back), a 500 as
    "could not confirm", an unknown server as a bad request — each one request, never
    retried."""
    with _source_app(source={}) as (client, mock, _i):
        client.wait_until(lambda: _source_row(client)["supported"], timeout=20,
                          what="the source row")
        mock.source_use_script([403, 500])
        _open_prefs(client)
        client.wait_until(lambda: (_prefs_source(client) or {}).get("can_use") is True,
                          timeout=20, what="an enabled Use this machine button")
        client.press_source_use("mock")
        client.wait_until(lambda: (_prefs_source(client) or {}).get("error"),
                          timeout=15, what="the refusal under the row")
        row = _prefs_source(client)
        assert "refused the move: a script key cannot see or move a credential source" \
            in row["error"], row
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
    gone wherever it is shown — the source row and Preferences."""
    dirty = "desk\N{RIGHT-TO-LEFT OVERRIDE}\u0007\n\N{ZERO WIDTH SPACE}evil"
    with _source_app(source={"machine_name": dirty}) as (client, _m, _i):
        client.wait_until(lambda: _source_row(client)["supported"], timeout=20,
                          what="the source row")
        assert _source_row(client)["source"] == {"id": OTHER_MACHINE, "name": "deskevil"}
        _open_prefs(client)
        client.wait_until(lambda: (_prefs_source(client) or {}).get("source_name") == "deskevil",
                          timeout=20, what="the cleaned name in Preferences")


def test_no_machine_identity_disables_use_this_machine():
    """With no machine identity (the state dir cannot hold one) no delivery stream
    runs, a move is refused with the reason and nothing is sent, and Preferences
    disables the button and says why."""
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
        _open_prefs(client)
        client.wait_until(lambda: _prefs_source(client) is not None,
                          timeout=20, what="the source row in Preferences")
        assert _prefs_source(client)["can_use"] is False
        values = _prefs_report(client)["values"]
        assert values["this_machine_error"] == status["this_machine_error"], values


def test_busy_source_read_keeps_the_last_known_source_and_recovers():
    """`503 SOURCE_BUSY` is transient. Over IPC: two busy answers are retried away
    inside one read; busy on every ask is a `busy` row — supported, no error, the
    source unknown rather than absent. In Preferences, reopened after another machine
    took the source while the server is busy: the row keeps the source it last showed,
    marked retrying (never "no source"), and the one later read brings the new one."""
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

        _open_prefs(client)
        client.wait_until(lambda: (_prefs_source(client) or {}).get("source_name") == "desk",
                          timeout=20, what="the source row naming desk")
        _close_prefs(client)
        mock.move_source(LAB_MACHINE, "lab")
        mock.source_busy(3, retry_after="0")
        client.show_preferences()
        client.wait_until(lambda: (_prefs_source(client) or {}).get("retrying") is True,
                          timeout=20, what="the row marked busy, retrying")
        row = _prefs_source(client)
        assert row["source_name"] == "desk", f"the last known source, never none: {row}"
        assert row["error"] is None, row
        client.wait_until(
            lambda: (lambda r: r and r["source_name"] == "lab" and not r["retrying"])(
                _prefs_source(client)),
            timeout=20, what="the later read naming lab")


def test_source_row_survives_a_server_restart_and_recovers():
    """A read while the server is down (a restart) keeps the source row Preferences
    showed — the last known source, noted "cannot reach the server" — instead of
    dropping it, and the window reads again on its own: once the server is back the
    row is current again, with nothing shown, focused or reloaded meanwhile. Over IPC
    the same read is `unreachable`, never `supported: false` with nothing to show."""
    with _source_app(source={}) as (client, mock, _i):
        client.wait_until(lambda: _source_row(client)["supported"], timeout=20,
                          what="the source row")
        _open_prefs(client)
        client.wait_until(lambda: (_prefs_source(client) or {}).get("source_name") == "desk",
                          timeout=20, what="the source row naming desk")
        mock.pause()
        try:
            row = _source_row(client)
            assert row["unreachable"] is True and row["error"], row
            # Any prefs change reloads the window: its source read finds the server down.
            prefs = client.prefs_get()
            client.prefs_set_terminal(prefs["terminal_preset"], prefs.get("terminal_template"))
            client.wait_until(lambda: (_prefs_source(client) or {}).get("unreachable") is True,
                              timeout=20, what="the row kept, marked unreachable")
            row = _prefs_source(client)
            assert row["source_name"] == "desk", f"the last known source, never gone: {row}"
            assert row["error"] is None, row
            # Another machine takes the source while the server is down; the window must
            # learn it by itself once the server is back.
            mock.move_source(LAB_MACHINE, "lab")
            _holds(lambda: (_prefs_source(client) or {}).get("source_name") == "desk",
                   1.0, "the row stays while the server is down")
        finally:
            mock.resume()
        client.wait_until(
            lambda: (lambda r: r and r["source_name"] == "lab" and not r["unreachable"])(
                _prefs_source(client)),
            timeout=30, what="the row current again once the server is back")


def test_reopening_preferences_reads_a_source_taken_elsewhere():
    """Closing Preferences only hides it, so a reopen must read the sources again:
    this machine was the source when it closed, another machine took it meanwhile
    (which ends this machine's stream), and the reopened window says so and offers the
    button again."""
    with _source_app(source={}) as (client, mock, _i):
        client.wait_until(lambda: _not_bound(client), timeout=20, what="the stream not_bound")
        client.source_use("mock")
        client.wait_until(lambda: _stream(client)["state"] == "connected",
                          timeout=30, what="the stream connected")
        _open_prefs(client)
        client.wait_until(lambda: (_prefs_source(client) or {}).get("this_machine") is True,
                          timeout=20, what="the row reading this machine")
        assert _prefs_source(client)["can_use"] is False
        _close_prefs(client)
        mock.move_source(OTHER_MACHINE, "desk")
        client.wait_until(lambda: _not_bound(client), timeout=30,
                          what="the stream ended by the move and refused after it")
        client.show_preferences()
        client.wait_until(
            lambda: (lambda r: r and r["source_name"] == "desk" and not r["this_machine"]
                     and r["can_use"])(_prefs_source(client)),
            timeout=20, what="the reopened window naming desk with the button back")


def test_a_read_overtaken_by_a_move_never_lands_over_it():
    """A read the window started BEFORE a move, answered after the move's own answer
    and read-back (the server took its document first), is older: it must not put
    the other machine back on the row."""
    with _source_app(source={}) as (client, mock, _i):
        client.wait_until(lambda: _not_bound(client), timeout=20, what="the stream not_bound")
        machine = _machine(client)
        _open_prefs(client)
        client.wait_until(lambda: (_prefs_source(client) or {}).get("can_use") is True,
                          timeout=20, what="an enabled Use this machine button")
        gets = mock.source_gets()
        mock.source_reads_late(3.0)
        # Any prefs change reloads the window: its source read is the late one.
        prefs = client.prefs_get()
        client.prefs_set_terminal(prefs["terminal_preset"], prefs.get("terminal_template"))
        client.wait_until(lambda: mock.source_gets() > gets, timeout=10,
                          what="the window's read reaching the server (desk)")
        client.press_source_use("mock")
        client.wait_until(
            lambda: (lambda r: r and r["this_machine"] and not r["busy"])(_prefs_source(client)),
            timeout=15, what="the move confirmed")
        client.wait_until(lambda: mock.source_late_answered() >= 1, timeout=15,
                          what="the overtaken read answered")
        _holds(lambda: (_prefs_source(client) or {}).get("source_name") == machine["name"],
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
        _open_prefs(client)
        both = ("mock", "mock2")
        client.wait_until(
            lambda: all((_prefs_source(client, h) or {}).get("can_use") for h in both),
            timeout=20, what="both servers' buttons enabled")
        mock.delay_source_use(1.5)
        client.press_source_use("mock")
        client.press_source_use("mock2")
        client.wait_until(
            lambda: all((_prefs_source(client, h) or {}).get("busy") for h in both),
            timeout=5, what="both moves in flight at once")
        client.press_source_use("mock")  # the row's move is in flight: no button
        client.wait_until(
            lambda: all((lambda r: r and r["this_machine"] and not r["busy"])(
                _prefs_source(client, h)) for h in both),
            timeout=20, what="both rows reading this machine")
        assert len(mock.source_use_requests()) == 2, mock.source_use_requests()
        assert not any(_prefs_source(client, h)["can_use"] for h in both)


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
