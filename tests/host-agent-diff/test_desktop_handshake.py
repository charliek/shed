"""Surface-A handshake: a fake shed-desktop app connects to the always-on approval
channel, sends a `hello`, and receives a `hello_ack`. The masked ack is canonical-
equal across the Go and Rust daemons — same `v`/`type`/namespaces/gate/timeout, with
only the volatile `id`/`ts`/`agent.version` masked (D3). The 'watch none' launch
config makes ssh the sole gated namespace (`gate_namespaces:["ssh-agent"]`)."""

import pytest

from desktop_client import DesktopClient
from normalize import canonical, mask_hello_ack


@pytest.mark.differential
def test_desktop_hello_ack_masked_canonical_equal(daemon, watch_none_config, differential):
    def scenario(impl):
        with daemon(impl, watch_none_config) as d:
            with DesktopClient(str(d.desktop_sock)) as app:
                app.send_hello()
                ack = app.await_frame("hello_ack", timeout=5.0)
                return canonical(mask_hello_ack(ack))

    ack = differential(scenario)

    # The stable fields survived the mask and carry the expected values.
    assert ack["v"] == 2
    assert ack["type"] == "hello_ack"
    assert ack["accepted"] is True
    assert ack["agent"] == {"version": "<version>", "approval_method": "shed-desktop"}
    assert ack["namespaces"] == [
        "ssh-agent",
        "aws-credentials",
        "docker-credentials",
        "egress",
    ]
    assert ack["gate_namespaces"] == ["ssh-agent"]
    assert ack["request_timeout_ms"] == 25000
    # An accepted ack omits `reason` (only the superseded ack sets it).
    assert "reason" not in ack
