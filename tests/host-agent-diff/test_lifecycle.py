"""Process lifecycle: a daemon with a valid config binds its status socket, serves
it, and on SIGTERM exits 0 and unlinks its socket(s). The `daemon` teardown already
enforces exit-0 + both-sockets-unlinked; this asserts the observable states
explicitly, per the deliverable."""

import pytest


@pytest.mark.parametrize("impl", ["go", "rust"])
def test_sigterm_clean_exit_and_sockets_unlinked(daemon, watch_none_config, impl):
    with daemon(impl, watch_none_config) as d:
        # While running: the status socket exists and is serving.
        assert d.status_sock.exists(), f"{impl}: status socket absent while running"
        r = d.status(json=True)
        assert r.returncode == 0, f"{impl}: status exit {r.returncode}\n{r.stderr}"
        status_sock = d.status_sock
        desktop_sock = d.desktop_sock

    # After the context exits (SIGTERM sent + clean-exit/unlink asserted in teardown),
    # both socket paths are gone. (The Rust slice-0 daemon has no desktop server, so
    # its desktop socket was never created — 'unlinked' holds trivially there.)
    assert not status_sock.exists(), f"{impl}: status socket lingered after shutdown"
    assert not desktop_sock.exists(), f"{impl}: desktop socket lingered after shutdown"
