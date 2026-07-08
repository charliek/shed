"""Non-PTY exec stderr/stdout separation (issue #222 follow-on).

Zed Remote-SSH, SFTP, and any raw `ssh -T` client read the exec channel's stdout
and stderr as SEPARATE streams. The in-VM agent used to fold the remote process's
stderr into stdout (a single MsgTypeData stream), which injected diagnostic/log
output into a binary stdout protocol — corrupting Zed's length-prefixed protobuf
pipe so its readiness handshake never parsed ("client did not become ready within
the timeout"). The agent now frames stderr as its own MsgTypeStderr and the host
demuxes it onto the SSH channel's extended-data stderr.

These live tests drive the real wire path (host framing → vsock → agent copiers →
process and back). They assert on raw `ssh -T` (the channel shape Zed/SFTP/rsync
use), NOT the `shed exec` CLI, which requests a PTY (`ssh -t`, allocated when
stdin is a terminal) that merges the two streams into one.

Parameterized over ["vz", "fc"] like the rest of the suite.

NOTE: the agent is baked into the rootfs IMAGE, so these assert the NEW agent —
they pass only against an image rebuilt with the stderr-separation change (a bare
`make test-integration-dev`, which restarts only the dev *server*, runs the old
baked agent and will fail here until the rootfs is rebuilt).
"""

from __future__ import annotations

import subprocess


def test_nopty_stderr_separated(shed_server, test_shed_name):
    """A non-PTY command's stdout and stderr arrive on their own SSH streams."""
    shed_server.create(test_shed_name, image="base")

    r = shed_server.ssh_exec(test_shed_name, "echo OUT; echo ERR 1>&2")
    assert r.returncode == 0, f"exit={r.returncode} stderr={r.stderr!r}"
    assert "OUT" in r.stdout, f"stdout missing OUT: stdout={r.stdout!r}"
    assert "ERR" not in r.stdout, (
        f"stderr leaked into stdout (fold regression): stdout={r.stdout!r}"
    )
    assert "ERR" in r.stderr, f"stderr missing ERR: stderr={r.stderr!r}"
    assert "OUT" not in r.stderr, f"stdout leaked into stderr: stderr={r.stderr!r}"


def test_pty_merges_streams(shed_server, test_shed_name):
    """The PTY channel is a single stream: stderr merges onto stdout (correct).

    A pty master merges stdout and stderr the way any terminal does, so the agent
    keeps the PTY path on one MsgTypeData stream. This locks that intended
    behavior so a future change can't accidentally split a PTY session.
    """
    shed_server.create(test_shed_name, image="base")

    argv = shed_server.ssh_argv(test_shed_name, "echo OUT; echo ERR 1>&2", pty=True)
    r = subprocess.run(argv, capture_output=True, text=True, timeout=60)
    # Over a PTY both lines land on the client's stdout (the tty stream).
    assert "OUT" in r.stdout, f"PTY stdout missing OUT: stdout={r.stdout!r}"
    assert "ERR" in r.stdout, (
        f"PTY must merge stderr onto stdout: stdout={r.stdout!r} stderr={r.stderr!r}"
    )
