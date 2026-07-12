"""Surface-B egress: the always-on egress-audit SSE consumer, asserted equal across the
Go `cmd/shed-host-agent` and the Rust `crates/shed-host-agent`.

Both daemons run a per-server, read-only `GET /api/egress/stream` subscriber (Go's
watcher group; Rust's `egress.rs` side task on `run_single_server_bus`). With egress
wired on both sides the endpoint sets fully converge — the synthetic bus no longer
merely tolerates a Go-only egress GET, it asserts BOTH impls hit the route.

Three cells (open server — the harness runs single-server open mode, so the token path
is `None`/static; the 401-invalidate + control-token scope are unit-owned in
`egress.rs`, Go's `_SendsControlToken`/`_401Invalidates` are httptest+fake):

1. **subscription convergence** — both impls GET `/api/egress/stream` when the bus
   starts (`wait_for_egress`), the endpoint-set-converged proof.
2. **events** — the bus serves a 200 stream and pushes ONE decision frame with a FIXED
   timestamp; both impls write the SAME durable audit JSONL line. The frame ts is fixed
   (`2020-01-01T00:00:00Z`), so Go's `LogEntry` keeps it verbatim (it stamps `now()`
   ONLY for an empty ts) — the whole line is deterministic and is diffed UNMASKED,
   including the always-present `"approval":""` byte.
3. **501 hard-backoff** — the bus 501s egress; each impl backs off the hard 5m (DEBUG-
   quiet), so within a short window its OWN `egress_hits()` stays at 1 (per-impl — each
   impl gets its own `SyntheticBus`), proving no fast reconnect.
"""

from __future__ import annotations

import time

import pytest

from normalize import canonical
from synthetic_bus import SyntheticBus

# A single streamed egress decision (shed-server's `egressDecision` wire shape:
# ts/shed/host/port/resolved_ip/protocol/verdict/reason). The FIXED ts makes the
# resulting audit line deterministic on both impls (see the events cell).
DECISION = {
    "ts": "2020-01-01T00:00:00Z",
    "shed": "web",
    "host": "evil.com",
    "port": 443,
    "resolved_ip": "1.2.3.4",
    "protocol": "https",
    "verdict": "deny",
    "reason": "default-deny",
}

# The durable audit JSONL line `egressAuditEntry(server="", DECISION)` produces on BOTH
# impls: ns=egress, op=protocol, result=verdict, detail=host:port (resolved_ip),
# reason echoed, ts kept from the frame, `approval:""` present, and `server` omitted
# (single-server mode leaves it empty → omitempty on both sides).
EXPECTED_AUDIT = {
    "ts": "2020-01-01T00:00:00Z",
    "shed": "web",
    "ns": "egress",
    "op": "https",
    "result": "deny",
    "detail": "evil.com:443 (1.2.3.4)",
    "reason": "default-deny",
    "approval": "",
}

# The no-reconnect observation window. Comfortably longer than a WRONG (normal exp)
# first backoff (~1s) would take to re-GET, and far shorter than the correct 5m hard
# backoff — so a 501 taking the normal path instead of the hard 5m would trip this.
_NO_RECONNECT_WINDOW_S = 2.0


@pytest.mark.differential
def test_egress_subscription_convergence(daemon, single_server_config, differential):
    """Both impls GET `/api/egress/stream` — the endpoint sets converged (this replaces
    the old harness code that absorbed a Go-only egress subscribe)."""

    def scenario(impl):
        with SyntheticBus() as bus:  # default "unavailable" (501)
            with daemon(impl, single_server_config(bus.url)):
                bus.wait_for_egress(timeout=10.0)
                return True

    assert differential(scenario) is True


@pytest.mark.differential
def test_egress_events_audit_diff(daemon, single_server_config, differential):
    """A fixed-ts decision frame → the SAME durable audit JSONL line on both impls,
    diffed UNMASKED (deterministic frame ts), including the `"approval":""` byte."""

    def scenario(impl):
        with SyntheticBus(egress="events") as bus:
            with daemon(impl, single_server_config(bus.url)) as d:
                bus.wait_for_egress(timeout=10.0)
                bus.push_egress(DECISION)
                audit = d.read_audit_jsonl(expect=1, timeout=10.0)[0]
                return canonical(audit)

    line = differential(scenario)
    assert line == canonical(EXPECTED_AUDIT)
    # The empty-but-PRESENT approval byte (egress leaves approval empty; the wire keeps
    # the key). A dropped key would fail the whole-line compare above too, but pin it.
    assert "approval" in line and line["approval"] == ""
    # ts is DIFFED (not masked): both impls rendered the frame ts identically.
    assert line["ts"] == "2020-01-01T00:00:00Z"


@pytest.mark.differential
def test_egress_501_hard_backoff_no_reconnect(daemon, single_server_config, differential):
    """A 501 egress route → each impl backs off the hard 5m (not the normal exp backoff),
    so within a short window its OWN `egress_hits()` stays at 1 (per-impl counter)."""

    def scenario(impl):
        with SyntheticBus() as bus:  # default "unavailable" (501)
            with daemon(impl, single_server_config(bus.url)):
                bus.wait_for_egress(timeout=10.0)
                time.sleep(_NO_RECONNECT_WINDOW_S)
                hits = bus.egress_hits()
                assert hits == 1, (
                    f"{impl}: egress_hits={hits} within {_NO_RECONNECT_WINDOW_S}s; "
                    "expected 1 (501 → hard 5m backoff, no fast reconnect)"
                )
                return hits

    assert differential(scenario) == 1
