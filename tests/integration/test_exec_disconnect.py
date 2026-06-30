"""Host-disconnect cleanup for the SSH exec channel (issue #222 follow-on).

When the host disconnects mid-command, the in-VM agent must terminate the
command — it SIGHUPs the command's process group — rather than orphaning it.
This matches standard SSH "connection hung up" semantics (use tmux/nohup to
intentionally survive a disconnect). Before the fix, a disconnected command kept
running and the agent leaked the handler goroutine waiting on it.

A silent command (no stdin, no output) can't be detected via an I/O error on the
channel — the guest's vsock read does not deliver EOF on the host's close — so
the agent probes the connection with a periodic keepalive; a failed probe is the
disconnect signal. This is exercised on BOTH the non-PTY exec path and the PTY
path (an idle interactive session whose host sleeps/drops is the canonical
orphan case).

Parameterized over ["vz", "fc"] (the backend) and ["nopty", "pty"] (the channel).
"""

from __future__ import annotations

import subprocess
import time

import pytest


@pytest.mark.parametrize("pty", [False, True], ids=["nopty", "pty"])
def test_disconnect_terminates_command(shed_server, test_shed_name, pty):
    """Killing the SSH client mid-command terminates the remote process.

    Runs a long silent `sleep` over the exec channel, records its PID (via a
    file the command writes), kills the SSH client to simulate a disconnect,
    then polls `/proc/<pid>` from a fresh exec until the process is gone. The
    silent command forces detection through the agent's keepalive probe rather
    than an I/O error, on both the non-PTY and PTY paths.
    """
    shed_server.create(test_shed_name, image="base")

    label = "pty" if pty else "nopty"
    # Unique per shed + mode so a run can never read a stale PID file (the shed
    # is fresh per test, but this is cheap insurance against future test reuse).
    pid_file = f"/tmp/shed-disconnect-{test_shed_name}-{label}.pid"
    shed_server.exec(test_shed_name, ["rm", "-f", pid_file])  # clear any stale file
    # `exec sleep` replaces the shell, so $$ (recorded before exec) is the
    # sleep's PID. The command holds the channel open until we disconnect.
    raw = f"echo $$ > {pid_file}; exec sleep 600"
    argv = shed_server.ssh_argv(test_shed_name, raw, pty=pty)

    # `with` closes the stdin/stdout/stderr pipes and reaps the client on exit
    # (avoids ResourceWarnings that the suite escalates to failures).
    with subprocess.Popen(
        argv,
        stdin=subprocess.PIPE,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
    ) as proc:
        # Poll (from a fresh exec) for the PID the command records — robust to
        # PTY echo/control bytes that would complicate parsing the channel.
        pid = None
        for _ in range(30):
            r = shed_server.exec(test_shed_name, ["cat", pid_file])
            if r.returncode == 0 and r.stdout.strip().isdigit():
                pid = r.stdout.strip()
                break
            time.sleep(0.5)
        assert pid, f"command did not start on the {label} channel"

        r = shed_server.exec(test_shed_name, ["test", "-d", f"/proc/{pid}"])
        assert r.returncode == 0, f"probe process {pid} not alive before disconnect"

        # Simulate a host disconnect: kill the SSH client hard.
        proc.kill()

    # The disconnect should propagate to the agent, which SIGHUPs the command's
    # process group. A silent command is detected via the agent's keepalive
    # probe, so allow comfortably more than one keepalive interval. Poll until
    # /proc/<pid> disappears.
    deadline = time.time() + 40
    alive = True
    while time.time() < deadline:
        r = shed_server.exec(test_shed_name, ["test", "-d", f"/proc/{pid}"])
        if r.returncode != 0:
            alive = False
            break
        time.sleep(1)

    assert not alive, (
        f"command (pid {pid}) survived the host disconnect on the {label} channel "
        f"— the agent did not terminate the process group, orphaning it and "
        f"leaking the handler goroutine"
    )
