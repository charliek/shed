"""Surface-A handshake guard: a connection whose FIRST frame is not a `hello`
(here a `pong`) is dropped by the server with NO `hello_ack` — the handshake must
open with a hello or the connection is refused. Both impls close the socket and send
nothing (Go `desktop_server.go:222` `probe.Type != "hello"` -> return; Rust
`handle_conn` bails on a non-`Hello` first frame)."""

import pytest

from desktop_client import DesktopClient


@pytest.mark.differential
def test_first_non_hello_line_drops_connection(daemon, watch_none_config, differential):
    def scenario(impl):
        with daemon(impl, watch_none_config) as d:
            with DesktopClient(str(d.desktop_sock)) as app:
                # First line is a pong, not a hello -> the server must drop us.
                app.send({"type": "pong"})
                closed = app.wait_closed(timeout=3.0)
                return {"closed": closed, "frames": app.frames()}

    result = differential(scenario)
    assert result["closed"] is True, "server did not drop the non-hello connection"
    assert result["frames"] == [], f"server sent frames on a non-hello open: {result['frames']!r}"
