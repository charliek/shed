"""The hub family's SSE cells (plan 010 §2.9 family 2): bounded frame reads
with the within-tick ordering AND the frame payload shapes pinned, plus the
stalled-reader survivability cell.

Every read is deadline-bounded (`HubLeg.hub_events_until`); comments (`: ok`,
heartbeats) are liveness, not wire, and are dropped before compare.
"""

import json
import time

import pytest

from normalize import MASK_TS, assert_rfc3339, mask_hub_session

pytestmark = pytest.mark.hub

SSE_SLUG = "sse111"


VALID_STATES = ("starting", "ready", "reconnecting", "needs-trust", "needs-auth", "dead")


def _mask_frame(frame: dict, home: str, race_state: bool = False) -> dict:
    """One SSE event, masked: session.updated bodies via the session masker,
    activity.changed's wall-clock stamp via the ts mask. `race_state` masks
    the lifecycle `state` (shape-asserted) — the APPEAR frame captures
    whatever the shim pane showed at that instant (starting vs ready races
    real paint timing); the settled frame's state stays diffed."""
    name, data = frame["event"], dict(frame["data"])
    if name == "session.updated" and data.get("session") is not None:
        data["session"] = mask_hub_session(data["session"], home)
        if race_state:
            assert data["session"].get("state") in VALID_STATES, frame
            data["session"]["state"] = "<state>"
    if "activity_at" in data:
        assert_rfc3339(data["activity_at"], "activity.changed.activity_at")
        data["activity_at"] = MASK_TS
    if race_state and "state" in data:
        assert data["state"] in VALID_STATES, frame
        data["state"] = "<state>"
    return {"event": name, "data": data}


def test_sse_appear_then_activity_order(hub_differential, hub_leg):
    """Subscribe FIRST, then create, and read until the activity settles. The
    frame COUNT races real pane transitions (the shim's starting→ready may
    land on the appear tick or its own later tick, emitting an extra
    session.updated), so the cell pins the timing-INVARIANT wire properties:

    - the very first frame is the appear `session.updated` (before any
      activity frame — the within-tick order the aggregator depends on);
    - the activity sequence is exactly working → needs_input (a fresh session
      always ticks working first; the quiet period then settles the anchor);
    - every frame carries the session's slug.
    """

    def scenario(impl):
        leg = hub_leg(impl)

        def create():
            res = leg.run(
                "create", "--kind", "codex", "--slug", SSE_SLUG, "--name", "hub-sse"
            )
            assert res.returncode == 0, f"{leg.impl}: create: {res.stderr}"

        def settled(events):
            return any(
                f["event"] == "activity.changed"
                and f["data"].get("activity") == "needs_input"
                for f in events
            )

        # Subscribe (confirmed by the `: ok` opener) BEFORE the session exists
        # — the appear frames land in THIS subscription by construction.
        frames = leg.hub_events_until(
            "activity never settled", settled, timeout=25, on_subscribed=create
        )

        activity_seq = [
            f["data"]["activity"] for f in frames if f["event"] == "activity.changed"
        ]
        last_activity = [f for f in frames if f["event"] == "activity.changed"][-1]
        return {
            "first_event": frames[0]["event"],
            # The frame BODIES are wire (a separate serialization from the
            # /v1/sessions snapshot): the appear frame is deterministic by
            # construction (first frame on a registered-before-create
            # subscription), and the last activity frame is the settled one.
            "appear_frame": _mask_frame(frames[0], str(leg.home), race_state=True),
            "settled_activity_frame": _mask_frame(last_activity, str(leg.home)),
            "activity_sequence": activity_seq,
            "slugs": sorted({f["data"].get("slug") for f in frames}),
        }

    hub_differential(scenario)


def test_sse_stalled_reader_hub_survives(hub_differential, hub_leg):
    """A subscriber that never reads must not wedge the hub (§2.2's
    write-deadline emulation, DIFFERENTIAL half): with a zero-window stalled
    peer attached and a flood of feed events fanning out, the hub keeps
    answering health and a FRESH subscriber still receives frames promptly.

    The connection-teardown half (frames dropped on the slow path, the stalled
    stream ENDED once the write deadline fires) is pinned at UNIT level on
    both sides — Go's TestHubEventsWedgedClientUnsubscribes and Rust's
    events_wedged_client_unsubscribes — because the TCP-level close is
    legitimately different plumbing (Go poisons the whole keep-alive
    connection on a deadline; hyper ends the response stream), and because
    kernel send buffers dwarf what a hermetic flood can fill deterministically
    — the differential's contract stops at "the hub survives, nobody else is
    affected"."""

    def scenario(impl):
        leg = hub_leg(impl)
        res = leg.run(
            "create", "--kind", "cursor", "--slug", "stall11", "--name", "hub-stall"
        )
        assert res.returncode == 0, f"{leg.impl}: create: {res.stderr}"
        leg.wait_tracked("stall11")

        stalled = leg.hub_events_socket()
        try:
            time.sleep(0.3)  # let the subscriber register + first frames queue

            accepted = 0
            for burst in range(3):
                for i in range(100):
                    payload = json.dumps(
                        {
                            "session_id": "4113a71f-0a42-4a6d-89b9-483e44b74103",
                            "prompt": f"flood {burst}-{i}",
                        }
                    )
                    got = leg.hub_request(
                        "POST",
                        "/v1/ingest/cursor?slug=stall11&event=beforeSubmitPrompt",
                        body=payload,
                    )
                    accepted += 1 if got["status"] == 202 else 0
                time.sleep(0.4)  # a few reconcile ticks fan the burst out
            assert accepted == 300, (
                f"{leg.impl}: the ingest route rejected flood events "
                f"({accepted}/300 accepted)"
            )

            # The hub survived the wedged peer:
            health = leg.hub_request("GET", "/v1/health")
            assert health["status"] == 200, f"{leg.impl}: health {health}"

            # …and a fresh subscriber receives a NEW event promptly.
            def post_one():
                got = leg.hub_request(
                    "POST",
                    "/v1/ingest/cursor?slug=stall11&event=beforeSubmitPrompt",
                    body=json.dumps(
                        {
                            "session_id": "4113a71f-0a42-4a6d-89b9-483e44b74103",
                            "prompt": "after the stall",
                        }
                    ),
                )
                assert got["status"] == 202, f"{leg.impl}: post-stall ingest {got}"

            fresh = leg.hub_events_until(
                "a fresh subscriber never received an event",
                lambda evs: len(evs) >= 1,
                timeout=15,
                on_subscribed=post_one,
            )
            return {
                "hub_healthy": True,
                "fresh_subscriber_receives": len(fresh) >= 1,
            }
        finally:
            stalled.close()

    hub_differential(scenario)
