"""M1: lifecycle mutations flip status, create streams to completion, and
terminal.preview builds the ssh command without spawning anything."""

from __future__ import annotations

import pytest
from client import ShedError

from _marks import mac_only

pytestmark = mac_only


def test_stop_then_start(shed):
    shed.refresh()
    assert shed.shed_status("hello-world") == "running"
    shed.shed_action("stop", "hello-world")  # the op refreshes before returning
    assert shed.shed_status("hello-world") == "stopped"
    shed.shed_action("start", "hello-world")
    assert shed.shed_status("hello-world") == "running"


def test_delete_removes_shed(shed):
    shed.refresh()
    assert shed.shed_status("callbell") == "stopped"
    shed.shed_action("delete", "callbell")
    assert shed.shed_status("callbell") is None


def test_create_streams_to_complete(shed):
    cid = shed.create_start("folio", repo="charliek/folio", backend="vz")
    shed.wait_until(lambda: shed.create_status(cid)["state"] == "complete",
                    what="create complete")
    status = shed.create_status(cid)
    assert status["messages"]            # progress messages were streamed
    assert status["shed"]["name"] == "folio"
    # The completed shed shows up in the list.
    shed.refresh()
    assert shed.shed_status("folio") == "running"


def test_create_error_surfaced(shed, mock):
    mock.create_should_fail = True
    cid = shed.create_start("doomed")
    shed.wait_until(lambda: shed.create_status(cid)["state"] == "error",
                    what="create error")
    assert "doomed" in (shed.create_status(cid)["error"] or "")


def test_terminal_preview_builds_ssh(shed):
    shed.refresh()
    cmd = shed.terminal_preview("hello-world")
    # Host "mock" in the fixture config maps to 127.0.0.1:2222 (ssh port).
    argv = cmd["argv"]
    assert argv[:2] == ["ssh", "-t"]
    assert "hello-world@127.0.0.1" in argv
    assert "-p" in argv and argv[argv.index("-p") + 1] == "2222"
    # The SSH host key is pinned: strict checking against the shed known_hosts
    # file. The -o options sit between `-t` and the target; the file path is
    # host-specific, so assert on the structure rather than the exact path.
    assert "StrictHostKeyChecking=yes" in argv
    assert "hello-world@127.0.0.1 -p 2222" in cmd["command"]
    assert "StrictHostKeyChecking=yes" in cmd["command"]
    # Observability: preview also surfaces the active preset + the exact
    # invocation that would run (no spawn). Fresh defaults → Terminal.app.
    assert cmd["preset"] == "terminal-app"
    assert cmd["invocation"]["executable"] == "/usr/bin/osascript"
    assert 'tell application "Terminal"' in cmd["invocation"]["arguments"][1]


def test_terminal_open_disabled_in_test_mode(shed):
    # terminal.open must never spawn a terminal under the harness.
    with pytest.raises(ShedError) as exc:
        shed.call("terminal.open", {"shed": "hello-world"})
    assert exc.value.code == "not-enabled"
