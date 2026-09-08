"""The hub family's SNAPSHOT cells (plan 010 §2.9 family 1): health identity,
the sessions overlay, messages paging, the whole 4xx/409 matrix for the four
POST verbs, and the bare-mux status shapes.

Their goldens were recorded from the Go hub BEFORE any Rust hub existed (the
H1½ Go-only phase) — the frozen `/v1` wire — and since H12 both legs answer
them equality-then-pin under this canonicalization.

Every cell that needs a session uses a pinned slug and polls the hub until the
reconcile loop has tracked it; nothing here sleeps.

Two kinds carry this family, for two different reasons:

* **codex** is the CAPABILITY workhorse: it advertises none of the contract-v2
  verbs, and since A6 (charliek/shed#322) no `feed`/`input` either — so its
  matrix cells pin the kind-based 409s, `input` now among them. Their
  tracked-ness precondition is the `/messages` 404→200 flip (see
  `_tracked_codex`), the one observable that survives S2's removal of the
  activity fallback (charliek/shed#324).
* **opencode** is the only WATCHABLE kind left (A6 retired the codex rollout
  tail and the cursor hook-ingest lane), so the `/messages` cells — which are
  lane-agnostic hub contracts that merely happened to ride a codex session —
  moved onto it, through the shared `hub_opencode.lane_session` setup that
  `test_hub_lane.py` already used.
"""

import json

import pytest

from hub_opencode import lane_session
from normalize import mask_hub_health, mask_hub_sessions

pytestmark = pytest.mark.hub

CODEX_SLUG = "hub111"
OC_SLUG = "hubmsg1"

# A slug no cell ever creates — the unknown-session arm of every matrix.
MISSING_SLUG = "nosuch"

JSON_HEADERS = {"Content-Type": "application/json"}

# Two oversized bodies, both just over hubMaxBodyBytes (16 KiB — keep them
# small: a real 413 sets closeAfterReply, so a megabyte body risks EPIPE).
# The cap is enforced by the STREAM READER, not a Content-Length pre-check, so
# the two pin opposite outcomes: junk fails the JSON parse at its first bytes
# (400 — the cap never trips), while VALID oversized JSON reads past the cap
# and earns the 413. A Rust hub that checks Content-Length up front would
# invert the junk cell — both directions are the frozen wire.
OVERSIZED_JUNK = "x" * (17 * 1024)
OVERSIZED_JSON = json.dumps({"text": "x" * (17 * 1024)})


def _status_body(got: dict) -> dict:
    """A snapshot cell's golden value: the status AND the decoded envelope (the
    bare-mux cells below deliberately pin the status alone)."""
    return {"status": got["status"], "body": got["json"]}


def _tracked_codex(leg):
    """Create the pinned codex session and wait until the hub tracks it.

    Tracked-ness is proved by the MESSAGES 404→200 FLIP, not by the activity
    overlay `wait_tracked` uses. Merely appearing in `GET /v1/sessions` is not
    tracked-ness (that endpoint lists from tmux one-shot, while the verbs read
    the reconcile-built tracked map — a verb fired in the gap earns 404
    `unknown_slug` instead of its kind-based 409), and codex's overlay used to
    come from the pane-stability engine S2 (charliek/shed#324) deleted. The
    reconcile loop tracks EVERY enumerated session, watcher or not, and
    `handleMessages` answers a tracked feedless kind with 200 and an empty page
    — so the flip off 404 IS the tracked-map insertion."""
    res = leg.run(
        "create", "--kind", "codex", "--slug", CODEX_SLUG, "--name", "hub-codex"
    )
    assert res.returncode == 0, f"{leg.impl}: exit {res.returncode}: {res.stderr}"

    def tracked():
        got = leg.hub_request("GET", f"/v1/sessions/{CODEX_SLUG}/messages")
        return got if got["status"] == 200 else None

    return leg.wait_hub(f"hub never tracked slug {CODEX_SLUG}", tracked)


def _matrix_slug(leg, needs_session: bool) -> str:
    """The slug a matrix cell aims at: the pinned TRACKED codex session for the
    capability arms, otherwise a slug that provably does not exist."""
    if not needs_session:
        return MISSING_SLUG
    _tracked_codex(leg)
    return CODEX_SLUG


def test_health_identity(hub_differential, hub_leg):
    """`GET /v1/health` — the identity handshake. `app` is byte-frozen; version
    and pid are shape-asserted then masked (two distinct daemons legitimately
    differ)."""

    def scenario(impl):
        leg = hub_leg(impl)
        got = leg.hub_request("GET", "/v1/health")
        return {"status": got["status"], "body": mask_hub_health(got["json"])}

    hub_differential(scenario)


def test_sessions_empty(hub_differential, hub_leg):
    """`GET /v1/sessions` with no rc sessions at all."""

    def scenario(impl):
        return _status_body(hub_leg(impl).hub_request("GET", "/v1/sessions"))

    hub_differential(scenario)


# `test_sessions_overlay_codex_settled` pinned the pane-stability engine's SETTLE
# — a codex row whose static shim pane went quiet past the hub's quiet period and
# landed on the kind's anchor answer. Both the engine and the anchor were deleted
# in S2 (charliek/shed#324): a shed row's activity now comes from a lane watcher
# or not at all, and codex has none. Removed with its golden, and replaced by the
# cell below, which keeps the differential it also carried.


def test_sessions_tracked_feedless_row(hub_differential, hub_leg):
    """`GET /v1/sessions` for a TRACKED session of a kind with no feed.

    **What this replaces, and why.** `test_sessions_overlay_codex_settled` pinned
    the pane-stability SETTLE, and went with the engine in S2
    (charliek/shed#324) — correctly. But it was also the only cell that compared
    the two hubs' `/v1/sessions` for a *tracked* session of a feedless kind:
    `_tracked_codex` now reads `/messages` purely as a readiness probe, so
    nothing else in this family looks at what such a row actually says. Without
    this, Go and Rust could drift on whether a tracked feedless row omits
    `activity`, `activity_at`, `last_message` and `pending_approvals` — or on any
    other field of it — and every remaining cell in the gate would still pass.

    It pins the row's SHAPE and its OMISSIONS, and deliberately pins no derived
    verdict: there is no producer left to derive one for codex, and re-pinning a
    value here would restore exactly what S2 deleted.
    """

    def scenario(impl):
        leg = hub_leg(impl)
        _tracked_codex(leg)
        got = leg.hub_request("GET", "/v1/sessions")
        body = mask_hub_sessions(got["json"], str(leg.home))
        rows = [s for s in body["sessions"] if s.get("slug") == CODEX_SLUG]
        assert len(rows) == 1, f"{impl}: expected one {CODEX_SLUG} row: {body!r}"
        # The claim, asserted before it is pinned (the D3 discipline): a tracked
        # row of a kind with no producer carries no derived status at all.
        for absent in ("activity", "activity_at", "last_message", "pending_approvals"):
            assert absent not in rows[0], (
                f"{impl}: a feedless kind's row carries {absent}: {rows[0]!r}"
            )
        return {"status": got["status"], "body": body}

    hub_differential(scenario)


def test_messages_empty_ring(hub_differential, hub_leg):
    """`GET /messages` on a tracked, WATCHED opencode session whose lane has
    produced nothing yet — the empty page a live feed answers before its first
    row. (It rode a codex session until A6 — charliek/shed#322 — retired that
    kind's tail; opencode is the only watchable kind left, and the contract
    under test was never codex-specific.)"""

    fakes = []

    def scenario(impl):
        leg = hub_leg(impl)
        lane_session(leg, fakes, OC_SLUG, "hub-msg")
        return _status_body(leg.hub_request("GET", f"/v1/sessions/{OC_SLUG}/messages"))

    try:
        hub_differential(scenario)
    finally:
        for fake in fakes:
            fake.stop()


# Explicit `pytest.param` ids on every cell below: the golden filename is the
# sanitized nodeid, and a default id embedding a 17 KiB body is not a filename.
PAGING_CELLS = [
    pytest.param("?since=abc", id="bad_since"),
    pytest.param("?since=-1", id="negative_since"),
    pytest.param("?limit=abc", id="bad_limit"),
    pytest.param("?limit=0", id="zero_limit"),
    pytest.param("?limit=999", id="over_max_limit"),
]

# The one paging behavior an EMPTY ring can still observe: a beyond-tail cursor
# (seq restarts on hub restart) must answer truncated=true — the refetch signal
# a poll-only client depends on. Without this cell a hub that never sets
# `truncated` would pass the whole family.
BEYOND_TAIL = pytest.param("?since=1", id="beyond_tail_truncated")


@pytest.mark.parametrize("query", PAGING_CELLS + [BEYOND_TAIL])
def test_messages_paging_rejections(hub_differential, hub_leg, query):
    """The `since`/`limit` validation matrix on a REAL tracked, WATCHED session
    (so the rejection is provably about the query, not the slug — and reaches a
    handler a feed-bearing kind actually uses). On opencode since A6
    (charliek/shed#322); the validation itself is lane-agnostic."""

    fakes = []

    def scenario(impl):
        leg = hub_leg(impl)
        lane_session(leg, fakes, OC_SLUG, "hub-msg")
        return _status_body(
            leg.hub_request("GET", f"/v1/sessions/{OC_SLUG}/messages{query}")
        )

    try:
        hub_differential(scenario)
    finally:
        for fake in fakes:
            fake.stop()


def test_messages_unknown_slug(hub_differential, hub_leg):
    def scenario(impl):
        got = hub_leg(impl).hub_request("GET", f"/v1/sessions/{MISSING_SLUG}/messages")
        return _status_body(got)

    hub_differential(scenario)


# --- The 4xx/409 verb matrix ------------------------------------------------
#
# Handler precedence is contract (rc-helper.md): body size (413) -> body
# validation (400) -> tracked lookup (404) -> capability (409 not_supported) —
# with the stream-cap nuance the OVERSIZED_JUNK comment pins. Cells are grouped
# by which precedence edge they pin. `needs_session=False` cells prove the
# EARLIER stage wins while the later one would also fail.

VERB_CELLS = [
    # Oversized junk: the parse fails before the cap trips -> 400, NOT 413
    # (stream-enforced cap — see the comment on OVERSIZED_JUNK).
    pytest.param("input", OVERSIZED_JUNK, False, id="input_oversized_junk_parse_wins"),
    pytest.param("turn", OVERSIZED_JUNK, False, id="turn_oversized_junk_parse_wins"),
    # Oversized VALID JSON: the decoder reads past the cap -> 413.
    pytest.param("input", OVERSIZED_JSON, False, id="input_oversized_json_413"),
    pytest.param("turn", OVERSIZED_JSON, False, id="turn_oversized_json_413"),
    # 400 body validation wins before the 404 the unknown slug would earn.
    pytest.param("input", "{nope", False, id="input_invalid_json_before_404"),
    pytest.param(
        "input", json.dumps({"text": ""}), False, id="input_empty_text_before_404"
    ),
    pytest.param(
        "turn", json.dumps({"text": "  "}), False, id="turn_empty_text_before_404"
    ),
    # 404 unknown slug (valid body, no session).
    pytest.param("input", json.dumps({"text": "hi"}), False, id="input_unknown_slug"),
    pytest.param("turn", json.dumps({"text": "hi"}), False, id="turn_unknown_slug"),
    pytest.param("interrupt", "", False, id="interrupt_unknown_slug"),
    # 409 not_supported — codex advertises none of the three verbs.
    pytest.param("turn", json.dumps({"text": "hi"}), True, id="turn_codex_not_supported"),
    pytest.param("interrupt", "", True, id="interrupt_codex_not_supported"),
    # 409 not_accepting — the behavior A6 (charliek/shed#322) left on /input.
    # No kind is `gated` any more, so a VALID body on a LIVE, tracked session
    # is refused by the kind gate rather than delivered to the pane; the 400/
    # 404/413 arms above are untouched. This cell is what keeps the surviving
    # `/input` contract pinned now that the delivery cell is gone.
    pytest.param(
        "input", json.dumps({"text": "hi"}), True, id="input_codex_not_accepting"
    ),
]


@pytest.mark.parametrize("verb,body,needs_session", VERB_CELLS)
def test_verb_matrix(hub_differential, hub_leg, verb, body, needs_session):
    def scenario(impl):
        leg = hub_leg(impl)
        slug = _matrix_slug(leg, needs_session)
        return _status_body(
            leg.hub_request(
                "POST", f"/v1/sessions/{slug}/{verb}", body=body, headers=JSON_HEADERS
            )
        )

    hub_differential(scenario)


ALLOW = json.dumps({"decision": "allow"})

APPROVAL_CELLS = [
    # Grammar failure direct-to-hub is a 400 (not 404): ".bad" starts
    # non-alphanumeric, so it can never be a real id.
    pytest.param(".bad", ALLOW, False, id="bad_grammar_id"),
    # Body validation precedes the slug lookup.
    pytest.param(
        "call1", json.dumps({"decision": "maybe"}), False, id="invalid_decision_before_404"
    ),
    # Valid id + decision, unknown slug -> 404 unknown_slug.
    pytest.param("call1", ALLOW, False, id="unknown_slug"),
    # codex approvals are `tui`: capability check rejects BEFORE any id lookup,
    # for a plausible lane id and a pane-anchor id alike.
    pytest.param("call1", ALLOW, True, id="codex_not_supported"),
    pytest.param("pane-1", ALLOW, True, id="codex_pane_id_not_supported"),
]


@pytest.mark.parametrize("approval_id,body,needs_session", APPROVAL_CELLS)
def test_approvals_matrix(hub_differential, hub_leg, approval_id, body, needs_session):
    def scenario(impl):
        leg = hub_leg(impl)
        slug = _matrix_slug(leg, needs_session)
        return _status_body(
            leg.hub_request(
                "POST",
                f"/v1/sessions/{slug}/approvals/{approval_id}",
                body=body,
                headers=JSON_HEADERS,
            )
        )

    hub_differential(scenario)


MUX_CELLS = [
    pytest.param("GET", "/v1/nope", id="unknown_path"),
    pytest.param("POST", "/v1/sessions", id="post_on_sessions"),
    pytest.param("GET", f"/v1/sessions/{MISSING_SLUG}/input", id="get_on_input"),
    pytest.param("POST", "/v1/health", id="post_on_health"),
    pytest.param("DELETE", "/v1/sessions", id="delete_on_sessions"),
]


@pytest.mark.parametrize("method,path", MUX_CELLS)
def test_bare_mux_status_only(hub_differential, hub_leg, method, path):
    """Unmatched path/method shapes: STATUS ONLY (plan 010 §2.2 — rc-helper.md
    forbids clients from interpreting the bare mux 404, and the Rust shell's
    fallback bodies are deliberately not chased)."""

    def scenario(impl):
        return {"status": hub_leg(impl).hub_request(method, path)["status"]}

    hub_differential(scenario)
