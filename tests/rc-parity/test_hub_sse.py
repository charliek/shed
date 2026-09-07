"""The hub family's SSE cells (plan 010 §2.9 family 2): bounded frame reads
with the within-tick ordering AND the frame payload shapes pinned, plus the
stalled-reader survivability cell.

Every read is deadline-bounded (`HubLeg.hub_events_until`); comments (`: ok`,
heartbeats) are liveness, not wire, and are dropped before compare.

Both cells ride an **opencode** session since A6 (charliek/shed#322): the
frame ordering and the fan-out survivability are lane-agnostic hub contracts
that merely happened to be driven by codex and cursor, whose lanes A6 retired.
opencode is the only watchable kind left, so its fake is what drives them now —
the appear/activity ordering through the same pane-stability engine as before,
and the fan-out load through a burst of scripted feed events rather than 300
cursor ingest POSTs.
"""

import time

import pytest

from hub_opencode import OC_SID, lane_session, user_text_frames
from normalize import MASK_TS, assert_rfc3339, mask_hub_session

pytestmark = pytest.mark.hub

SSE_SLUG = "sse111"
STALL_SLUG = "stall11"


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

    The session is opencode (A6, charliek/shed#322 — codex has no watcher any
    more). Deliberately a BARE create, not the full `lane_session` setup: the
    trigger runs inside the subscription's read loop, so anything slower than a
    create leaves frames queueing unread and a subscriber buffer can drop the
    opening `working` tick this cell exists to observe. No fake is bound, so the
    watcher never connects and the activity comes from the pane-stability
    engine — the same mechanism this cell always pinned.
    """

    def scenario(impl):
        leg = hub_leg(impl)

        def create():
            res = leg.run(
                "create", "--kind", "opencode", "--slug", SSE_SLUG, "--name", "hub-sse"
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

    The load generator is a burst of scripted opencode feed events (A6,
    charliek/shed#322 — it was 300 cursor ingest POSTs, and that route is
    gone). Each user text part is a terminal fold row, so the burst becomes
    `message.appended` frames fanning out to every subscriber. The cell asserts
    the rows ACTUALLY LANDED in the ring before drawing conclusions: a
    liveness-only check would pass against a completely dead feed.

    The connection-teardown half (frames dropped on the slow path, the stalled
    stream ENDED once the write deadline fires) is pinned at UNIT level on
    both sides — Go's TestHubEventsWedgedClientUnsubscribes and Rust's
    events_wedged_client_unsubscribes — because the TCP-level close is
    legitimately different plumbing (Go poisons the whole keep-alive
    connection on a deadline; hyper ends the response stream), and because
    kernel send buffers dwarf what a hermetic flood can fill deterministically
    — the differential's contract stops at "the hub survives, nobody else is
    affected"."""

    FLOOD = 300
    fakes = []

    def scenario(impl):
        leg = hub_leg(impl)
        fake = lane_session(leg, fakes, STALL_SLUG, "hub-stall")

        stalled = leg.hub_events_socket()
        try:
            time.sleep(0.3)  # let the subscriber register + first frames queue

            for burst in range(3):
                for frame in user_text_frames(OC_SID, FLOOD // 3, f"flood{burst}"):
                    fake.stream(frame)
                time.sleep(0.4)  # a few reconcile ticks fan the burst out

            # The flood REACHED the ring — without this the whole cell would
            # pass against a feed that produced nothing at all.
            # A page STARTING AFTER seq FLOOD-1: a row there proves at least
            # FLOOD rows landed. (`limit` is capped at 200 by the wire, so the
            # whole ring cannot be asked for in one page.)
            def flooded():
                got = leg.hub_request(
                    "GET",
                    f"/v1/sessions/{STALL_SLUG}/messages?since={FLOOD - 1}&limit=1",
                )
                rows = (got["json"] or {}).get("messages", [])
                return rows[0]["seq"] if rows else None

            delivered = leg.wait_hub(
                "the scripted flood never reached the feed", flooded, timeout=25
            )

            # The hub survived the wedged peer:
            health = leg.hub_request("GET", "/v1/health")
            assert health["status"] == 200, f"{leg.impl}: health {health}"

            # …and a fresh subscriber receives a NEW event promptly.
            def stream_one():
                for frame in user_text_frames(OC_SID, 1, "after"):
                    fake.stream(frame)

            fresh = leg.hub_events_until(
                "a fresh subscriber never received an event",
                lambda evs: len(evs) >= 1,
                timeout=20,
                on_subscribed=stream_one,
            )
            return {
                "hub_healthy": True,
                "flood_delivered": delivered >= FLOOD,
                "fresh_subscriber_receives": len(fresh) >= 1,
            }
        finally:
            stalled.close()

    try:
        hub_differential(scenario)
    finally:
        for fake in fakes:
            fake.stop()
