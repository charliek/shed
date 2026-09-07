"""The shared opencode-lane setup for every hub-family cell that needs a
TRACKED, WATCHED session.

Before A6 (charliek/shed#322) the hub family's workhorse kind was codex: it was
watchable, so the tracked path was exercised, and its three contract-v2 verbs
were deterministically rejected. A6 retired the codex rollout tail and the
cursor hook-ingest lane, leaving **opencode as the only watchable kind** — so
the lane-agnostic hub contracts (`/messages` paging, the SSE frame ordering, the
stalled-reader survivability) moved onto it, and the setup `test_hub_lane.py`
already needed became shared rather than copied.

The setup is: create the session, bind a `FakeOpencode` on the port the engine
allocated, pin it with a directory-matched `session.created`, and wait for the
pin's back-write — the proof the watcher is attached and addressable — then for
the reconcile loop's tracked-ness. Each leg drives its OWN fake with an
identical script, so the two hubs face byte-identical upstreams.
"""

from fake_opencode import FakeOpencode

OC_SID = "ses_hubparity0000000000000001"


def lane_session(leg, fakes, slug: str, name: str, sid: str = OC_SID) -> FakeOpencode:
    """Create `slug` as an opencode session on `leg` and return its bound fake.

    `fakes` is the caller's cleanup list: the fake is appended BEFORE any
    polling, so a pin-wait timeout cannot leak the bound server (and its port)
    for the rest of the session.
    """
    res = leg.run("create", "--kind", "opencode", "--slug", slug, "--name", name)
    assert res.returncode == 0, f"{leg.impl}: create: {res.stderr}"
    env = leg.session_env(f"rc-{slug}")
    port = int(env["SHED_RC_OPENCODE_PORT"])
    workdir = env["SHED_RC_WORKDIR"]

    fake = FakeOpencode(port)
    fakes.append(fake)
    fake.pin = sid
    fake.stream_session_created(sid, workdir)

    def pinned():
        got = leg.session_env(f"rc-{slug}").get("SHED_RC_AGENT_SESSION")
        return got if got == sid else None

    leg.wait_hub("the opencode pin was never back-written", pinned, timeout=20)
    leg.wait_tracked(slug)
    return fake


def user_text_frames(sid: str, n: int, tag: str) -> list:
    """`n` complete user-message event pairs for `sid` — the smallest scripted
    input that makes the opencode fold emit `n` feed rows (a user text part is
    terminal on arrival), and therefore `n` `message.appended` SSE frames out of
    the hub. Used as the fan-out load generator.
    """
    frames = []
    for i in range(n):
        mid = f"msg_{tag}_{i}"
        frames.append(
            {
                "type": "message.updated",
                "properties": {
                    "sessionID": sid,
                    "info": {
                        "id": mid,
                        "role": "user",
                        "sessionID": sid,
                        "time": {"created": 1784613616806 + i},
                    },
                },
            }
        )
        frames.append(
            {
                "type": "message.part.updated",
                "properties": {
                    "sessionID": sid,
                    "part": {
                        "type": "text",
                        "text": f"{tag} {i}",
                        "messageID": mid,
                        "sessionID": sid,
                        "id": f"prt_{tag}_{i}",
                    },
                    "time": 1784613616889 + i,
                },
            }
        )
    return frames
