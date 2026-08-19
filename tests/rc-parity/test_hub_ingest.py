"""The hub family's INGEST cells (plan 010 §2.9 family 5): the cursor hook
route via direct POST (the §2.9 contract surface — the preseeded hook script
targets the fixed production port and is one-shot-suite territory), its
precedence matrix, the no-lost-kickoff property, and the fold-mapping smoke.
"""

import json

import pytest

from normalize import masked_feed_rows

pytestmark = pytest.mark.hub

CUR_SLUG = "ing111"
CUR_SID = "4113a71f-0a42-4a6d-89b9-483e44b74103"


def _tracked_cursor(leg, slug=CUR_SLUG):
    res = leg.run("create", "--kind", "cursor", "--slug", slug, "--name", "hub-ing")
    assert res.returncode == 0, f"{leg.impl}: create: {res.stderr}"
    return leg.wait_tracked(slug)


def _post_hook(leg, slug, event, payload):
    return leg.hub_request(
        "POST",
        f"/v1/ingest/cursor?slug={slug}&event={event}",
        body=payload,
        headers={"Content-Type": "application/json"},
    )


INGEST_CELLS = [
    pytest.param("nosuch", "stop", "{}", id="unknown_slug"),
    pytest.param("not_a_slug!", "stop", "{}", id="malformed_slug"),
    pytest.param("", "stop", "{}", id="missing_slug"),
    pytest.param(CUR_SLUG, "st%20op%21", "{}", id="malformed_event"),
    pytest.param(CUR_SLUG, "", "{}", id="missing_event"),
    # Oversize: the route's OWN 256 KiB cap (not the shared 16 KiB one).
    pytest.param(
        CUR_SLUG,
        "afterShellExecution",
        json.dumps({"session_id": CUR_SID, "output": "x" * (257 * 1024)}),
        id="oversized_413",
    ),
]


@pytest.mark.parametrize("slug,event,payload", INGEST_CELLS)
def test_ingest_rejections(hub_differential, hub_leg, slug, event, payload):
    def scenario(impl):
        leg = hub_leg(impl)
        _tracked_cursor(leg)
        got = _post_hook(leg, slug, event, payload)
        return {"status": got["status"], "body": got["json"]}

    hub_differential(scenario)


def test_ingest_non_cursor_kind_409(hub_differential, hub_leg):
    """A tracked session of another kind: 409 not_supported (this payload
    shape is cursor's; folding it anywhere else is a category error)."""

    def scenario(impl):
        leg = hub_leg(impl)
        res = leg.run(
            "create", "--kind", "codex", "--slug", "ingcdx", "--name", "hub-ing-cdx"
        )
        assert res.returncode == 0, f"{leg.impl}: create: {res.stderr}"
        leg.wait_tracked("ingcdx")
        got = _post_hook(leg, "ingcdx", "stop", "{}")
        return {"status": got["status"], "body": got["json"]}

    hub_differential(scenario)


def test_ingest_kickoff_is_never_lost(hub_differential, hub_leg):
    """The no-lost-kickoff property: the FIRST accepted hook event reaches the
    feed whether it rode the pre-watcher queue or a live push — `shed attach
    --kind cursor --prompt` delivers within the create→first-tick window, and
    that beforeSubmitPrompt must be the feed's first row."""

    def scenario(impl):
        leg = hub_leg(impl)
        res = leg.run(
            "create", "--kind", "cursor", "--slug", "kick11", "--name", "hub-kick"
        )
        assert res.returncode == 0, f"{leg.impl}: create: {res.stderr}"
        payload = json.dumps({"session_id": CUR_SID, "prompt": "the kickoff prompt"})

        # Post immediately and keep retrying: 404 until the slug is tracked
        # (the ingest route never re-derives from tmux), then 202 — possibly
        # into the pre-watcher queue, possibly into the live watcher. Either
        # way the row must land.
        def accepted():
            got = _post_hook(leg, "kick11", "beforeSubmitPrompt", payload)
            return got if got["status"] == 202 else None

        leg.wait_hub("the kickoff hook was never accepted", accepted, timeout=20)

        def in_feed():
            got = leg.hub_request("GET", "/v1/sessions/kick11/messages")
            for row in (got["json"] or {}).get("messages", []):
                if row.get("text") == "the kickoff prompt":
                    return row
            return None

        row = leg.wait_hub("the kickoff prompt never reached the feed", in_feed)
        return masked_feed_rows([row])[0]

    hub_differential(scenario)


def test_ingest_fold_smoke_and_activity(hub_differential, hub_leg):
    """Fold-mapping smoke: a submitted prompt folds to a user row and flips
    the session to `working`; a stop settles it. Read through /messages +
    the sessions overlay (both legs poll the same observables)."""

    def scenario(impl):
        leg = hub_leg(impl)
        _tracked_cursor(leg, "fold11")
        p1 = json.dumps({"session_id": CUR_SID, "prompt": "build the thing"})
        got = _post_hook(leg, "fold11", "beforeSubmitPrompt", p1)
        assert got["status"] == 202, f"{leg.impl}: {got}"

        def working():
            got = leg.hub_request("GET", "/v1/sessions")
            for entry in (got["json"] or {}).get("sessions", []):
                if entry.get("slug") == "fold11" and entry.get("activity") == "working":
                    return entry
            return None

        leg.wait_hub("the prompt never flipped activity to working", working)

        stop = json.dumps({"session_id": CUR_SID, "status": "completed"})
        got = _post_hook(leg, "fold11", "stop", stop)
        assert got["status"] == 202, f"{leg.impl}: {got}"

        def settled():
            got = leg.hub_request("GET", "/v1/sessions")
            for entry in (got["json"] or {}).get("sessions", []):
                if entry.get("slug") == "fold11" and entry.get("activity") in (
                    "idle",
                    "needs_input",
                ):
                    return entry["activity"]
            return None

        activity_after_stop = leg.wait_hub("stop never settled the session", settled)

        def rows():
            got = leg.hub_request("GET", "/v1/sessions/fold11/messages")
            msgs = (got["json"] or {}).get("messages", [])
            return msgs if msgs else None

        msgs = leg.wait_hub("no feed rows folded", rows)
        return {
            "rows": masked_feed_rows(msgs),
            "activity_after_stop": activity_after_stop,
        }

    hub_differential(scenario)
