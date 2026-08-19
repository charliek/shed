"""The hub family's SIDE-EFFECT cells (plan 010 §2.9 family 3): the
`SHED_RC_AGENT_SESSION` correlation back-write into the tmux session env, and
`/input` delivery onto the pane — the two places the hub WRITES into the
world rather than answering reads.
"""

import json

import pytest

pytestmark = pytest.mark.hub

BW_SLUG = "bwr111"
BW_SID = "019f0000-0000-7000-8000-00000000ab12"
IN_SLUG = "inp111"


def test_codex_correlation_back_writes_agent_session(hub_differential, hub_leg):
    """An unambiguous codex rollout correlation back-writes the discovered id
    into `SHED_RC_AGENT_SESSION` (so a hub restart re-correlates exactly). The
    rollout is written into the leg's hermetic HOME with the session's own
    created_at + workdir, making the window match deterministic."""

    def scenario(impl):
        leg = hub_leg(impl)
        res = leg.run(
            "create", "--kind", "codex", "--slug", BW_SLUG, "--name", "hub-bw"
        )
        assert res.returncode == 0, f"{leg.impl}: create: {res.stderr}"
        env = leg.session_env(f"rc-{BW_SLUG}")
        created_at = env.get("SHED_RC_CREATED_AT")
        workdir = env.get("SHED_RC_WORKDIR")
        assert created_at and workdir, f"{leg.impl}: metadata missing: {env}"

        # One rollout file inside the correlation window, cwd-matched: the
        # unambiguous-pick arm, which back-writes IMMEDIATELY at watcher build.
        # The dated path is cosmetic (real codex nests by date): correlation
        # WALKS the whole sessions root and the window match keys off the
        # file's payload.timestamp, never the path.
        rollout_dir = leg.home / ".codex" / "sessions" / "2026" / "08" / "18"
        rollout_dir.mkdir(parents=True)
        meta = {
            "timestamp": created_at,
            "type": "session_meta",
            "payload": {"id": BW_SID, "timestamp": created_at, "cwd": workdir},
        }
        (rollout_dir / f"rollout-{BW_SID}.jsonl").write_text(
            json.dumps(meta) + "\n"
        )

        def back_written():
            got = leg.session_env(f"rc-{BW_SLUG}").get("SHED_RC_AGENT_SESSION")
            return got or None

        got = leg.wait_hub("the agent session was never back-written", back_written)
        return {"agent_session": got}

    hub_differential(scenario)


def test_input_delivery_reaches_the_pane(hub_differential, hub_leg):
    """POST /input on a settled codex composer: 200 `{"delivered":true}` and
    the text lands on the tmux pane (typed via the shared bracketed-paste
    path). The one-shot suite already proves the shim echoes typed keys."""

    TEXT = "hello from the hub differential"

    def scenario(impl):
        leg = hub_leg(impl)
        res = leg.run(
            "create", "--kind", "codex", "--slug", IN_SLUG, "--name", "hub-input"
        )
        assert res.returncode == 0, f"{leg.impl}: create: {res.stderr}"
        leg.wait_tracked(IN_SLUG)

        # The gate needs the SETTLED needs_input verdict (the overlay cell
        # pinned it): poll the overlay, then post.
        def settled():
            got = leg.hub_request("GET", "/v1/sessions")
            for entry in (got["json"] or {}).get("sessions", []):
                if entry.get("slug") == IN_SLUG and entry.get("activity") == "needs_input":
                    return entry
            return None

        leg.wait_hub("codex never settled to needs_input", settled)
        got = leg.hub_request(
            "POST",
            f"/v1/sessions/{IN_SLUG}/input",
            body=json.dumps({"text": TEXT}),
            headers={"Content-Type": "application/json"},
        )

        def echoed():
            pane = leg.capture(f"rc-{IN_SLUG}")
            return True if TEXT in pane else None

        leg.wait_hub("the delivered text never reached the pane", echoed)
        return {"status": got["status"], "body": got["json"], "pane_echoed": True}

    hub_differential(scenario)
