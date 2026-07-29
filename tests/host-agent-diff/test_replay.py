"""The event replay ring (A2) — the `replay_events`-on-connect surface.

The desktop server buffers EVERY audit `event` frame into a bounded ring
(`RING_MAX = 100`) **regardless of whether a consumer is connected**
(`desktop.rs:publish_audit`, wired from the audit sink
`audit.rs:JsonlAuditSink::log`). When a fresh consumer connects with
`hello.replay_events: N > 0`, the server replays the last `N` buffered frames
verbatim (each keeps its ORIGINAL `id`/`ts` — the stored serialized bytes are
re-sent) before the live stream continues.

This drives it end-to-end: with NO desktop consumer connected, a non-gated
audit-producing bus op (ssh `list`, `approval: none`, no gate) buffers an `event` in
the ring; THEN a consumer connects requesting replay and the buffered frame is
replayed (golden-pinned via `mask_event`, id/ts masked). A subsequent live `list`
then fans out to the now-connected consumer — the buffered-then-live path.

Single-server mode (`server:` at the synthetic bus), `ssh.mode: local-keys` with the
committed ed25519 key installed so `list` reports `1 keys`. The `list` op is ungated
regardless of policy; `approve-all` just exercises a non-empty gate.
"""

from __future__ import annotations

import time

import pytest

from desktop_client import DesktopClient
from normalize import canonical, mask_event
from synthetic_bus import SyntheticBus

# Block style (the Rust reader is block-only). `{server}` is str.replace-filled below;
# `{audit_log}` survives to the `daemon` fixture's `.format`.
REPLAY_CONFIG = """\
server: {server}
ssh:
  mode: local-keys
  approval:
    policy: approve-all
logging:
  enabled: true
  path: {audit_log}
"""

# The `shed` block every request carries; echoed onto the audit/event (shed name "web").
SHED = {"name": "web", "backend": "vz", "server": "mini2"}


def _replay_config(bus_url: str) -> str:
    return REPLAY_CONFIG.replace("{server}", bus_url)


def _list_req(req_id: str) -> dict:
    return {
        "id": req_id,
        "namespace": "ssh-agent",
        "type": "request",
        "final": True,
        "timestamp": "2026-07-10T00:00:00Z",
        "payload": {"operation": "list"},
        "shed": SHED,
    }


def _connect_and_replay(
    sock_path: str, replay_n: int, timeout: float = 10.0
) -> tuple[DesktopClient, dict]:
    """Connect a desktop consumer requesting replay of the last `replay_n` events and
    return `(client, replayed_event)` once a buffered `event` is replayed. Reconnects
    until the ring append has landed: the audit fan-out appends to the ring on a
    goroutine, so the durable audit line (which we wait for first) can precede the ring
    append by a scheduling hop; replay is one-shot per `hello`, so a connect that raced
    an empty ring is retried on a fresh connection (the ring persists — replay only
    copies it). The winning client is returned OPEN for the live-fan-out half."""
    deadline = time.monotonic() + timeout
    while True:
        app = DesktopClient(sock_path, replay_events=replay_n)
        app.connect()
        app.send_hello()
        try:
            return app, app.await_frame("event", timeout=0.5)
        except AssertionError:
            app.close()
            if time.monotonic() >= deadline:
                raise AssertionError(
                    f"no replayed event within {timeout}s (ring never populated)"
                )


@pytest.mark.differential
def test_event_replay_ring(daemon, differential):
    def scenario(impl):
        with SyntheticBus() as bus:
            with daemon(impl, _replay_config(bus.url), install_ssh_key=True) as d:
                bus.wait_for_subscribe("ssh-agent", timeout=10.0)

                # Buffer an audit event in the ring with NO desktop consumer connected.
                bus.push_request("ssh-agent", _list_req("list-1"))
                bus.await_response("ssh-agent", timeout=10.0)
                # The durable line landing proves the entry was published (Go writes the
                # file + publishes in one locked LogEntry) — the ring append follows.
                d.read_audit_jsonl(expect=1, timeout=10.0)

                app, replayed = _connect_and_replay(str(d.desktop_sock), 10, timeout=10.0)
                try:
                    # A subsequent live audit event fans out to the now-connected consumer.
                    bus.push_request("ssh-agent", _list_req("list-2"))
                    bus.await_response_at("ssh-agent", 1, timeout=10.0)
                    live = app.await_frame("event", timeout=10.0)
                finally:
                    app.close()

                return {
                    "replayed": canonical(mask_event(replayed)),
                    "live": canonical(mask_event(live)),
                }

    result = differential(scenario)

    # The buffered `list` audit event, replayed verbatim (id/ts masked). The ungated
    # `list` audits `approval:"none"`, `1 keys` (one committed ed25519 key installed).
    expected = {
        "v": 2,
        "type": "event",
        "id": "<id>",
        "ts": "<ts>",
        "kind": "audit",
        "shed": "web",
        "ns": "ssh-agent",
        "op": "list",
        "result": "ok",
        "detail": "1 keys",
        "approval": "none",
    }
    assert result["replayed"] == expected, result["replayed"]
    # The live fan-out carries the same shape (a distinct id/ts, masked away).
    assert result["live"] == expected, result["live"]
