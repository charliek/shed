"""The hub family's LANE cells (plan 010 §2.9 family 4): the contract-v2
verbs round-tripped against a live opencode lane — each leg's hub drives its
OWN `fake_opencode` instance (identical scripts), and the fake's pinGuard
fails the cell on any POST outside the pinned session's three scoped routes.
"""

import json

import pytest

from fake_opencode import FakeOpencode
from normalize import masked_feed_rows

pytestmark = pytest.mark.hub

OC_SLUG = "lane11"
OC_SID = "ses_hubparity0000000000000001"
JSON_HEADERS = {"Content-Type": "application/json"}


def _lane(leg, fakes):
    """Create the opencode session, bind the fake on the engine-allocated
    port, pin via a directory-matched session.created, and wait for the pin's
    back-write — the proof the watcher is attached and addressable."""
    res = leg.run(
        "create", "--kind", "opencode", "--slug", OC_SLUG, "--name", "hub-lane"
    )
    assert res.returncode == 0, f"{leg.impl}: create: {res.stderr}"
    env = leg.session_env(f"rc-{OC_SLUG}")
    port = int(env["SHED_RC_OPENCODE_PORT"])
    workdir = env["SHED_RC_WORKDIR"]

    fake = FakeOpencode(port)
    # Registered BEFORE any polling: a pin-wait timeout must not leak the
    # bound server (and its port) for the rest of the session.
    fakes.append(fake)
    fake.pin = OC_SID
    fake.stream_session_created(OC_SID, workdir)

    def pinned():
        got = leg.session_env(f"rc-{OC_SLUG}").get("SHED_RC_AGENT_SESSION")
        return got if got == OC_SID else None

    leg.wait_hub("the opencode pin was never back-written", pinned, timeout=20)
    leg.wait_tracked(OC_SLUG)
    return fake


def test_lane_verbs_round_trip(hub_differential, hub_leg, request):
    """turn → 202 (opaque handle, masked), interrupt → 202, and the approval
    lifecycle: pending ask → resolve (200) → same-decision replay (200,
    idempotent, NO second upstream POST) → different decision (409
    already_resolved). The fake records exactly one scoped POST per verb and
    zero pinGuard violations — the WS-B invariant on the live path."""

    fakes = []

    def scenario(impl):
        leg = hub_leg(impl)
        fake = _lane(leg, fakes)
        base = f"/v1/sessions/{OC_SLUG}"

        # --- turn ---
        turn = leg.hub_request(
            "POST", f"{base}/turn", body=json.dumps({"text": "run the tests"}),
            headers=JSON_HEADERS,
        )
        turn_body = dict(turn["json"] or {})
        turn_id = turn_body.pop("turn_id", None)
        assert isinstance(turn_id, str) and turn_id, f"{leg.impl}: turn body {turn}"

        # --- interrupt ---
        intr = leg.hub_request("POST", f"{base}/interrupt")

        # --- approvals ---
        fake.stream_ask(OC_SID, "per_1")

        def pending():
            got = leg.hub_request("GET", "/v1/sessions")
            for entry in (got["json"] or {}).get("sessions", []):
                for ask in entry.get("pending_approvals") or []:
                    if ask.get("id") == "per_1":
                        return ask
            return None

        leg.wait_hub("the ask never reached pending_approvals", pending, timeout=20)
        allow = json.dumps({"decision": "allow"})
        first = leg.hub_request(
            "POST", f"{base}/approvals/per_1", body=allow, headers=JSON_HEADERS
        )
        posts_after_first = len(fake.snapshot()["post_paths"])
        replay = leg.hub_request(
            "POST", f"{base}/approvals/per_1", body=allow, headers=JSON_HEADERS
        )
        posts_after_replay = len(fake.snapshot()["post_paths"])
        conflict = leg.hub_request(
            "POST",
            f"{base}/approvals/per_1",
            body=json.dumps({"decision": "deny"}),
            headers=JSON_HEADERS,
        )
        snap = fake.snapshot()
        # The upstream surface: exactly three scoped POSTs (turn, abort,
        # permissions) with their WIRE BODIES pinned (the lane's payload
        # contract), no replay POST, no violations.
        suffixes = sorted(p.rsplit("/", 1)[-1] for p in snap["post_paths"])
        bodies = {
            p.rsplit("/", 1)[-1]: (json.loads(b) if b else None)
            for p, b in fake.post_bodies.items()
        }
        return {
            "turn": {"status": turn["status"], "body": turn_body, "handle": "<turn_id>"},
            "interrupt": {"status": intr["status"], "body": intr["json"]},
            "resolve": {"status": first["status"], "body": first["json"]},
            "replay": {"status": replay["status"], "body": replay["json"]},
            "conflict": {"status": conflict["status"], "body": conflict["json"]},
            "upstream_posts": suffixes,
            "upstream_bodies": bodies,
            "replay_posted_again": posts_after_replay != posts_after_first,
            "violations": snap["violations"],
        }

    try:
        hub_differential(scenario)
    finally:
        for fake in fakes:
            fake.stop()


def test_lane_approval_feed_rows(hub_differential, hub_leg):
    """The approval lifecycle's FEED rows: a pending `approval_request` row on
    the ask, and the resolved row after the hub-side resolve — read through
    /messages with seq/ts masked."""

    fakes = []

    def scenario(impl):
        leg = hub_leg(impl)
        fake = _lane(leg, fakes)
        fake.stream_ask(OC_SID, "per_9")

        def has_pending_row():
            got = leg.hub_request("GET", f"/v1/sessions/{OC_SLUG}/messages")
            for row in (got["json"] or {}).get("messages", []):
                ap = row.get("approval") or {}
                if ap.get("id") == "per_9" and ap.get("status") == "pending":
                    return got
            return None

        leg.wait_hub("no pending approval row", has_pending_row, timeout=20)
        leg.hub_request(
            "POST",
            f"/v1/sessions/{OC_SLUG}/approvals/per_9",
            body=json.dumps({"decision": "allow_always"}),
            headers=JSON_HEADERS,
        )

        def has_resolved_row():
            got = leg.hub_request("GET", f"/v1/sessions/{OC_SLUG}/messages")
            rows = [
                r
                for r in (got["json"] or {}).get("messages", [])
                if (r.get("approval") or {}).get("id") == "per_9"
            ]
            for row in rows:
                if row["approval"].get("status") == "resolved":
                    return rows
            return None

        rows = leg.wait_hub("no resolved approval row", has_resolved_row, timeout=20)
        # violations ride the golden so an unscoped resolve (e.g. the global
        # /permission/{id}/reply route) fails THIS cell too, not only the
        # round-trip one.
        return {
            "rows": masked_feed_rows(rows),
            "violations": fake.snapshot()["violations"],
        }

    try:
        hub_differential(scenario)
    finally:
        for fake in fakes:
            fake.stop()
