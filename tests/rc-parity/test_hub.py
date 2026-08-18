"""The hub family's SNAPSHOT cells (plan 010 §2.9 family 1): health identity,
the sessions overlay, messages paging, the whole 4xx/409 matrix for the four
POST verbs, and the bare-mux status shapes.

Recorded from the Go hub BEFORE any Rust hub exists (`hub_differential` is
Go-only until H12) — these goldens ARE the frozen `/v1` wire the Rust port must
answer byte-for-byte under this canonicalization.

Every cell that needs a session uses a pinned slug and polls the hub until the
reconcile loop has tracked it; nothing here sleeps. The codex kind is the
snapshot workhorse: watchable (so the tracked path is exercised), the three
contract-v2 verbs deterministically rejected (`not_supported` — codex
advertises none), and its static shim pane settles to a stable activity under
the fast-tick tuning. `input` is NOT in this family's tracked-session cells on
purpose: codex input is `gated` (it would type into the live shim pane) — the
delivery path is the side-effect family's territory (H12).
"""

import json

import pytest

from normalize import mask_hub_health, mask_hub_sessions

pytestmark = pytest.mark.hub

CODEX_SLUG = "hub111"

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
    """Create the pinned codex session and wait until the hub tracks it."""
    res = leg.run(
        "create", "--kind", "codex", "--slug", CODEX_SLUG, "--name", "hub-codex"
    )
    assert res.returncode == 0, f"{leg.impl}: exit {res.returncode}: {res.stderr}"
    return leg.wait_tracked(CODEX_SLUG)


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


def test_sessions_overlay_codex_settled(hub_differential, hub_leg):
    """A tracked codex session's list entry once its activity has SETTLED.

    The static shim pane never changes, so under the fast ticks the stability
    engine settles within ~1s: `working` on first capture, then the quiet
    period expires and the verdict lands on the kind's anchor answer. The cell
    polls for a settled value and pins WHICH one the Go hub derives — that
    choice (needs_input vs idle for this pane) is part of the frozen wire."""

    def scenario(impl):
        leg = hub_leg(impl)
        _tracked_codex(leg)

        def settled():
            got = leg.hub_request("GET", "/v1/sessions")
            for entry in (got["json"] or {}).get("sessions", []):
                if entry.get("slug") == CODEX_SLUG and entry.get("activity") in (
                    "idle",
                    "needs_input",
                ):
                    return got
            return None

        got = leg.wait_hub("codex activity never settled", settled)
        return {
            "status": got["status"],
            "body": mask_hub_sessions(got["json"], str(leg.home)),
        }

    hub_differential(scenario)


def test_messages_empty_ring(hub_differential, hub_leg):
    """`GET /messages` on a tracked codex session with an empty ring (no JSONL
    was ever correlated — the hermetic HOME has no rollout files)."""

    def scenario(impl):
        leg = hub_leg(impl)
        _tracked_codex(leg)
        return _status_body(
            leg.hub_request("GET", f"/v1/sessions/{CODEX_SLUG}/messages")
        )

    hub_differential(scenario)


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
    """The `since`/`limit` validation matrix on a REAL tracked session (so the
    rejection is provably about the query, not the slug)."""

    def scenario(impl):
        leg = hub_leg(impl)
        _tracked_codex(leg)
        return _status_body(
            leg.hub_request("GET", f"/v1/sessions/{CODEX_SLUG}/messages{query}")
        )

    hub_differential(scenario)


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
