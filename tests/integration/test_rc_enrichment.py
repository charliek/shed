"""Server-side RC enrichment of session listings (issue #242, spirit-of).

The server execs the in-shed `shed-ext-rc` binary over the guest agent channel
and populates `Session.RC` on `GET /api/sessions` (and the per-shed variant), so
an HTTP-only client sees RC Session Convention metadata without SSH-ing into each
shed. `?rc=0` opts out (zero guest execs).

This is a **server-side** change, so it must be validated against the parallel
dev server (`make test-integration-dev`) — the brew/deb-installed binary predates
the feature. The test therefore uses the `shed_server_dev` / `test_shed_name_dev`
fixtures, and resolves each backend's API endpoint (host + port) from the client
config entry, so the FC leg reaches the remote dev server rather than localhost.

A `shell`-kind rc session is used because it needs no agent auth (no login), so
the assertion is deterministic in CI: the row is present and carries an `rc`
block. Skips are limited to genuine preconditions (the guest image lacks a
multi-agent `shed-ext-rc`, or the server explicitly advertises a feature set
without rc enrichment); once preconditions hold, a missing `rc` block FAILS —
rc presence is the feature under test.
"""

from __future__ import annotations

import json
import time
import urllib.request

import pytest

from fixtures.server import resolve_server_entry

# Substrings that identify a fixture-environment gap — the guest image's
# shed-ext-rc predates multi-agent RC (no `--kind` flag / unknown kind) or the
# binary is absent — as opposed to a real server, guest-agent, argument, or
# permission regression. Only these get a skip; anything else FAILS the test so a
# regression can't masquerade as a precondition skip.
_RC_INCOMPAT_SIGNS = (
    "command not found",
    "not found",
    "no such file",
    "executable file not found",
    "unknown kind",
    "unknown flag",
    "unknown shorthand flag",
    "flag provided but not defined",
)


def _require_rc_create(r) -> None:
    """Gate on a `shed-ext-rc create` result: pass on success, skip only on a
    known image-compatibility signature, fail on any other non-zero exit."""
    if r.returncode == 0:
        return
    text = (r.stderr or "").lower()
    if any(sign in text for sign in _RC_INCOMPAT_SIGNS):
        pytest.skip(
            "shed-ext-rc create --kind shell failed (image predates multi-agent "
            f"RC or omits the binary): exit={r.returncode} stderr={r.stderr!r}"
        )
    pytest.fail(
        "shed-ext-rc create --kind shell failed unexpectedly — not a known "
        "image-compatibility signature, so this is a server/guest-agent/argument "
        f"regression, not a precondition: exit={r.returncode} stderr={r.stderr!r}"
    )


def _poll_rc_row(fetch, *, timeout: float = 10.0, interval: float = 0.5):
    """Poll `fetch()` -> (row, payload) until `row` is non-None or `timeout`
    elapses. `--wait=false` skips readiness polling, so the session may not be
    registered when the first listing is fetched; bound the wait to absorb that
    create-race without masking a genuinely missing row. Returns the last
    (row, payload) either way so the caller's assertion reports real diagnostics.
    """
    deadline = time.monotonic() + timeout
    while True:
        row, payload = fetch()
        if row is not None or time.monotonic() >= deadline:
            return row, payload
        time.sleep(interval)


def _api_base(server) -> str:
    """Return the dev server's HTTP base URL from its client-config entry.

    `shed_server_dev` is parameterized across VZ (localhost) and FC (remote,
    `$SHED_FC_HOST`); the config entry carries the right host + http_port for
    each, the same way the CLI reaches the server. Dev servers run in open mode
    (plain HTTP, no token).
    """
    entry = resolve_server_entry(server.name)
    if not entry:
        pytest.skip(f"no config entry for server {server.name!r}")
    host = entry.get("host") or "localhost"
    port = int(entry.get("http_port") or 0)
    if port <= 0:
        pytest.skip(f"config entry for {server.name!r} has no http_port")
    return f"http://{host}:{port}"


def _get_json(base: str, path: str) -> dict:
    with urllib.request.urlopen(f"{base}{path}", timeout=15) as resp:
        return json.loads(resp.read().decode("utf-8"))


def _find_rc_row(sessions: list[dict], shed: str) -> dict | None:
    for s in sessions:
        if s.get("shed_name") == shed and str(s.get("name", "")).startswith("rc-"):
            return s
    return None


def _find_overview_shed(sheds: list[dict], shed: str) -> dict | None:
    for s in sheds:
        if s.get("name") == shed:
            return s
    return None


def test_rc_enrichment_populates_and_rc0_opts_out(
    shed_server_dev, test_shed_name_dev
):
    """A shell-kind rc session appears with an `rc` block on GET /api/sessions,
    and `?rc=0` returns the same row without it."""
    server = shed_server_dev
    shed = test_shed_name_dev
    base = _api_base(server)

    # Genuine server precondition: once /api/info advertises a feature list, a
    # server without "rc-enrich" explicitly predates the feature — skip. A dev
    # server without the list at all is asserted against directly below (this
    # suite leg targets the developer's own build; a missing rc block there is
    # a regression, not a skip).
    info = _get_json(base, "/api/info")
    features = info.get("features")
    if isinstance(features, list) and "rc-enrich" not in features:
        pytest.skip(
            f"server {server.name!r} advertises features={features} without "
            "rc-enrich — it predates server-side rc enrichment"
        )

    # The extensions image bakes in shed-ext-rc (base does not).
    server.create(shed, image="extensions")

    # Guest precondition: create a shell-kind rc session in-guest. Shell needs no
    # agent login, so it reaches a stable state immediately; --wait=false returns
    # without polling. A failure here means the image's shed-ext-rc predates
    # multi-agent RC (or is absent) — a fixture-environment gap, not a server
    # regression, so skip.
    r = server.exec(
        shed,
        ["shed-ext-rc", "create", "--kind", "shell", "--wait=false"],
    )
    _require_rc_create(r)

    # Default listing: the rc-* row must be present and enriched. rc presence is
    # the feature under test — a missing block here is a FAILURE (if the target
    # is a brew/deb-installed server, rerun via make test-integration-dev).
    # --wait=false returned before the session registered, so poll for the row.
    def _fetch_global():
        resp = _get_json(base, "/api/sessions")
        return _find_rc_row(resp.get("sessions") or [], shed), resp

    row, resp = _poll_rc_row(_fetch_global)
    assert row is not None, (
        f"no rc-* session row for shed {shed!r} in GET /api/sessions: "
        f"{resp.get('sessions')!r}"
    )
    rc = row.get("rc")
    assert rc is not None, (
        f"Session.RC missing for {row.get('name')!r} — server-side rc enrichment "
        f"did not populate it (warnings={resp.get('warnings')!r}). This is the "
        "feature under test; if the target server is the brew/deb install, rerun "
        "against the dev build via make test-integration-dev."
    )
    assert rc.get("kind") == "shell", f"unexpected rc.kind: {rc!r}"
    assert rc.get("managed") is True, f"managed shell session expected: {rc!r}"
    assert rc.get("state"), f"rc.state should be populated: {rc!r}"

    # ?rc=0 must return the same row WITHOUT the rc block (enrichment skipped).
    resp0 = _get_json(base, "/api/sessions?rc=0")
    row0 = _find_rc_row(resp0.get("sessions") or [], shed)
    assert row0 is not None, (
        f"rc-* row disappeared under ?rc=0 for shed {shed!r}: {resp0.get('sessions')!r}"
    )
    assert row0.get("rc") is None, (
        f"?rc=0 must omit the rc block, got: {row0.get('rc')!r}"
    )

    # Per-shed listing (GET /api/sheds/{name}/sessions) must carry the same
    # contract as the aggregate: enriched by default, rc omitted under ?rc=0.
    per_shed = _get_json(base, f"/api/sheds/{shed}/sessions")
    prow = _find_rc_row(per_shed.get("sessions") or [], shed)
    assert prow is not None, (
        f"no rc-* row for shed {shed!r} in GET /api/sheds/{shed}/sessions: "
        f"{per_shed.get('sessions')!r}"
    )
    prc = prow.get("rc")
    assert prc is not None, (
        f"Session.RC missing for {prow.get('name')!r} on the per-shed route "
        f"(warnings={per_shed.get('warnings')!r}); rerun against the dev build via "
        "make test-integration-dev if targeting a brew/deb server."
    )
    assert prc.get("kind") == "shell", f"unexpected per-shed rc.kind: {prc!r}"

    per_shed0 = _get_json(base, f"/api/sheds/{shed}/sessions?rc=0")
    prow0 = _find_rc_row(per_shed0.get("sessions") or [], shed)
    assert prow0 is not None, (
        f"rc-* row disappeared under ?rc=0 on the per-shed route for shed "
        f"{shed!r}: {per_shed0.get('sessions')!r}"
    )
    assert prow0.get("rc") is None, (
        f"?rc=0 must omit the rc block on the per-shed route, got: {prow0.get('rc')!r}"
    )


def test_overview_shape(shed_server_dev, test_shed_name_dev):
    """GET /api/overview is a single-call host snapshot: the server block
    advertises the `overview` feature, the df block is present, and the shed
    carries its rc-enriched sessions (a shell-kind session appears under its shed
    with an `rc` block).

    Like the enrichment test this is a server-side feature, so it targets the
    dev server via the `shed_server_dev` fixture; a server that advertises a
    feature list without `overview` predates the endpoint and skips.
    """
    server = shed_server_dev
    shed = test_shed_name_dev
    base = _api_base(server)

    # Precondition: a features list without "overview" means the server predates
    # the endpoint. A dev server without any list is asserted directly (regression,
    # not skip) — the /api/overview call below 404s loudly if the route is absent.
    info = _get_json(base, "/api/info")
    features = info.get("features")
    if isinstance(features, list) and "overview" not in features:
        pytest.skip(
            f"server {server.name!r} advertises features={features} without "
            "overview — it predates the /api/overview endpoint"
        )

    server.create(shed, image="extensions")

    r = server.exec(
        shed,
        ["shed-ext-rc", "create", "--kind", "shell", "--wait=false"],
    )
    _require_rc_create(r)

    # --wait=false returned before the session registered; poll /api/overview for
    # the enriched shell row under our shed before asserting the payload.
    def _fetch_overview():
        payload = _get_json(base, "/api/overview")
        entry = _find_overview_shed(payload.get("sheds") or [], shed)
        sessions = (entry or {}).get("sessions") or []
        return _find_rc_row(sessions, shed), payload

    row, ov = _poll_rc_row(_fetch_overview)

    # server block: version + the overview/rc-enrich feature tokens.
    srv_block = ov.get("server") or {}
    assert srv_block.get("version"), f"overview server.version missing: {srv_block!r}"
    ov_features = srv_block.get("features") or []
    assert "overview" in ov_features, (
        f"overview server.features must include 'overview': {ov_features!r}"
    )

    # df block present (same shape as /api/system/df).
    df = ov.get("df")
    assert df is not None, f"overview df block missing (warnings={ov.get('warnings')!r})"
    assert "totals" in df, f"df block missing totals: {df!r}"

    # sheds present; our shed carries the enriched shell rc session.
    sheds = ov.get("sheds") or []
    entry = _find_overview_shed(sheds, shed)
    assert entry is not None, f"shed {shed!r} not in overview sheds: {[s.get('name') for s in sheds]!r}"
    sessions = entry.get("sessions")
    assert isinstance(sessions, list), f"shed sessions must be a list: {entry!r}"
    assert row is not None, (
        f"no rc-* session under shed {shed!r} in overview: {sessions!r}"
    )
    rc = row.get("rc")
    assert rc is not None, (
        f"Session.RC missing under overview for {row.get('name')!r} "
        f"(warnings={ov.get('warnings')!r}); rerun against the dev build via "
        "make test-integration-dev if targeting a brew/deb server."
    )
    assert rc.get("kind") == "shell", f"unexpected rc.kind: {rc!r}"
    assert rc.get("state"), f"rc.state should be populated: {rc!r}"
