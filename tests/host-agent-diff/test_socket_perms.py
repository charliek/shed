"""The daemon-created status socket is a socket with owner-only 0600 perms on BOTH
impls (the fixed public-interface socket is explicitly chmod'd by the daemon, not
left at the process umask). Socket-dir 0700 and stale-vs-live rebinding are tracked
for a later slice — see README "Known contract gaps"."""

import os
import stat

import pytest


@pytest.mark.differential
def test_status_socket_is_0600(daemon, watch_none_config, differential):
    def scenario(impl):
        with daemon(impl, watch_none_config) as d:
            st = os.stat(d.status_sock)
            assert stat.S_ISSOCK(st.st_mode), f"{impl}: {d.status_sock} is not a socket"
            return oct(stat.S_IMODE(st.st_mode))

    perms = differential(scenario)
    assert perms == oct(0o600), f"status socket perms {perms} != 0o600"
