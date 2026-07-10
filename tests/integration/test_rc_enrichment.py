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
import urllib.request

import pytest

from fixtures.server import resolve_server_entry


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
    if r.returncode != 0:
        pytest.skip(
            "shed-ext-rc create --kind shell failed (image predates multi-agent "
            f"RC or omits the binary): exit={r.returncode} stderr={r.stderr!r}"
        )

    # Default listing: the rc-* row must be present and enriched. rc presence is
    # the feature under test — a missing block here is a FAILURE (if the target
    # is a brew/deb-installed server, rerun via make test-integration-dev).
    resp = _get_json(base, "/api/sessions")
    row = _find_rc_row(resp.get("sessions") or [], shed)
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
