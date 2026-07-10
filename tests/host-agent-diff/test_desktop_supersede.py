"""Surface-A single-consumer / last-writer-wins: a second `hello` supersedes the
first connection. Client A connects + registers (accepted); once A is the active
consumer, client B connects + registers. A then receives a
`hello_ack{accepted:false, reason:"superseded"}` whose `agent` is the ZERO value
(`version:"" , approval_method:""`, Go `desktop_server.go:355` / Rust `hello_ack`
with `accepted=false`) and its socket is closed; B becomes the active consumer
(accepted). Both impls' masked acks are canonical-equal.

Readiness is a deadline poll on the daemon's own `approval_channel.consumer_connected`
(via `status --json`), never a fixed sleep — A must be promoted before B connects or
there is nothing to supersede."""

import pytest

from desktop_client import DesktopClient, wait_for_consumer
from normalize import canonical, mask_hello_ack


@pytest.mark.differential
def test_second_consumer_supersedes_first(daemon, watch_none_config, differential):
    def scenario(impl):
        with daemon(impl, watch_none_config) as d:
            with DesktopClient(str(d.desktop_sock), name="A", version="app-a") as a:
                a.send_hello()
                a_ack = a.await_frame("hello_ack", timeout=5.0)
                assert a_ack["accepted"] is True, f"{impl}: A's first ack not accepted"
                # A must be the ACTIVE consumer before B connects (else no supersede).
                wait_for_consumer(d, connected=True, timeout=5.0)

                with DesktopClient(str(d.desktop_sock), name="B", version="app-b") as b:
                    b.send_hello()
                    b_ack = b.await_frame("hello_ack", timeout=5.0)
                    # A gets a superseded ack, then A's socket is closed.
                    a_super = a.await_frame("hello_ack", timeout=5.0)
                    a_closed = a.wait_closed(timeout=5.0)
                    return {
                        "a_superseded": canonical(mask_hello_ack(a_super)),
                        "b_accepted": canonical(mask_hello_ack(b_ack)),
                        "a_closed": a_closed,
                    }

    result = differential(scenario)

    # A's superseded ack: zero-value agent, null slices, timeout 0, reason set.
    a_super = result["a_superseded"]
    assert a_super["type"] == "hello_ack"
    assert a_super["accepted"] is False
    assert a_super["reason"] == "superseded"
    assert a_super["agent"] == {"version": "", "approval_method": ""}
    assert a_super["namespaces"] is None
    assert a_super["gate_namespaces"] is None
    assert a_super["request_timeout_ms"] == 0
    # A's socket was closed after the superseded ack.
    assert result["a_closed"] is True

    # B is the new active consumer: a normal accepted ack.
    b_ack = result["b_accepted"]
    assert b_ack["accepted"] is True
    assert b_ack["agent"] == {"version": "<version>", "approval_method": "shed-desktop"}
    assert b_ack["gate_namespaces"] == ["ssh-agent"]
