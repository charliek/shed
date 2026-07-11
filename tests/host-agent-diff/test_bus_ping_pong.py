"""Surface-B bus: subscribe -> ping -> respond-pong.

A single-server (no `discovery:` block) daemon connects to the synthetic shed-server
plugin bus and subscribes to `ssh-agent`. The test pushes a `ping` request Envelope
over that SSE stream and awaits the daemon's response POST. The masked pong is
canonical-equal across the Go and Rust daemons — same correlation, namespace, type,
final flag, `{"status":"ok"}` payload, and echoed `shed`, with only the volatile
`id`/`timestamp` masked (D3, `mask_bus_response`).

Slice asymmetry (a known contract gap, not a diff target): the Go daemon in
single-server mode ALSO GETs `/api/egress/stream` (always-on egress subscriber) and
would subscribe to aws/docker if configured; the Rust slice-1b daemon subscribes to
`ssh-agent` only. The synthetic bus 501s egress and the test compares only the
ssh-agent response, so the asymmetry is tolerated by construction.
"""

import pytest

from normalize import assert_rfc3339, canonical, mask_bus_response
from synthetic_bus import SyntheticBus

# The request Envelope shed-server would deliver to the ssh-agent listener: a
# `ping` op with a fixed id + shed so the correlation (`in_reply_to`) and echoed
# `shed` are pinned constants both impls must reproduce.
PING_REQUEST = {
    "id": "ping-1",
    "namespace": "ssh-agent",
    "type": "request",
    "final": True,
    "timestamp": "2026-07-10T00:00:00Z",
    "payload": {"operation": "ping"},
    "shed": {"name": "web", "backend": "vz", "server": "mini2"},
}


@pytest.mark.differential
def test_bus_ping_pong_masked_canonical_equal(daemon, single_server_config, differential):
    def scenario(impl):
        with SyntheticBus() as bus:
            with daemon(impl, single_server_config(bus.url)) as d:
                # The daemon subscribes to ssh-agent once its bus client connects.
                bus.wait_for_subscribe("ssh-agent", timeout=10.0)
                bus.push_request("ssh-agent", PING_REQUEST)
                response = bus.await_response("ssh-agent", timeout=10.0)

                # Assert timestamp shape on the RAW response BEFORE masking (belt +
                # suspenders: mask_bus_response also shape-asserts it).
                assert_rfc3339(response.get("timestamp"), "pong.timestamp")
                return canonical(mask_bus_response(response))

    pong = differential(scenario)

    # The stable fields survived the mask and carry the expected pong values.
    assert pong["type"] == "response"
    assert pong["final"] is True
    # Correlation is diffed, NOT masked: the response must echo the request id.
    assert pong["in_reply_to"] == "ping-1"
    assert pong["namespace"] == "ssh-agent"
    assert pong["payload"] == {"status": "ok"}
    # `shed` is copied verbatim from the request so shed-server can route the reply.
    assert pong["shed"] == {"name": "web", "backend": "vz", "server": "mini2"}
    # The volatile fields were masked (shape-asserted first inside mask_bus_response).
    assert pong["id"] == "<id>"
    assert pong["timestamp"] == "<ts>"
