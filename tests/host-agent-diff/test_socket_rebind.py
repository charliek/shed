"""A1 · socket-dir 0700 + stale-vs-live rebinding, driven LIVE.

The bind ceremony (`bind_unix_socket`) is a three-way gate shared by BOTH the status
and desktop sockets:

  - the parent dir is (re-)chmod'd to 0700 **even when it already existed** — an
    owner-only parent dir is the real protection, covering the window before the
    0600 socket chmod;
  - a STALE socket left behind by an unclean exit (a socket file with nothing
    accepting) is unlinked and rebound;
  - a LIVE socket held by another process, or a NON-socket regular file, is REFUSED
    (never steal a running agent's channel; never delete an unrelated file).

The dir-0700 re-chmod and the stale-rebind halves have a clean wire consequence —
the daemon comes up and answers `status` — so they are driven live here. The
live-refuse and non-socket-refuse halves are op-log-only (the daemon just refuses
that one listener), with no clean wire consequence to assert, so they are pinned as
`sockets.rs` units (`prepare_refuses_live_socket`,
`prepare_refuses_non_socket_file`) — see the README per-cell table.
"""

import os
import socket
import stat
from pathlib import Path

import pytest

from conftest import DESKTOP_SOCK_NAME, STATUS_SOCK_NAME


@pytest.mark.differential
def test_socket_dir_rechmod_0700_when_preexisting(daemon, watch_none_config, differential):
    """A pre-existing world-open (0777) socket dir is tightened to 0700 by the bind
    ceremony (`set_permissions(0o700)`)."""

    def scenario(impl):
        def widen(socket_dir):
            os.chmod(socket_dir, 0o777)

        with daemon(impl, watch_none_config, pre_launch=widen) as d:
            # Fully up (status answers) → the dir chmod, which runs before the socket
            # bind, has definitely happened.
            d.poll_status(lambda _o: True)
            mode = stat.S_IMODE(os.stat(d.socket_dir).st_mode)
            return oct(mode)

    perms = differential(scenario)
    assert perms == oct(0o700), f"socket dir perms {perms} != 0o700"


@pytest.mark.differential
def test_stale_sockets_are_rebound(daemon, watch_none_config, differential):
    """A stale AF_UNIX socket (bound, no listener) pre-planted at BOTH fixed socket
    paths is detected as not-live, unlinked, and rebound — the daemon
    comes up and answers `status`, and both paths are live sockets afterward."""

    def scenario(impl):
        def plant_stale(socket_dir):
            for name in (STATUS_SOCK_NAME, DESKTOP_SOCK_NAME):
                # A REAL AF_UNIX socket file (not a regular file) with NO listener —
                # bind then close, leaving the file behind → genuinely stale.
                s = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
                s.bind(str(Path(socket_dir) / name))
                s.close()

        with daemon(impl, watch_none_config, pre_launch=plant_stale) as d:
            obj = d.poll_status(lambda _o: True)
            # The rebound paths are live sockets (the stale files were replaced).
            for sock_path in (d.status_sock, d.desktop_sock):
                st = os.stat(sock_path)
                assert stat.S_ISSOCK(st.st_mode), f"{impl}: {sock_path} is not a socket"
            # A stable field proving the daemon actually served a report post-rebind.
            return obj["schema"]

    schema = differential(scenario)
    assert schema == 1
