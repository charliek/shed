"""Live rc hub activity over the server surfaces (Phase C).

These exercise the two server surfaces the resident rc hub is reached through:

  * the per-shed reverse proxy `GET /api/sheds/{name}/rc/v1/sessions`
    (feature token ``rc-proxy``), which forwards into the guest-local hub and
    returns the live session list WITH the activity dimension; and
  * the aggregate SSE stream `GET /api/rc/events` (feature token ``rc-events``),
    which fans out per-shed hub activity events.

Both are **server-side** features, so like ``test_rc_enrichment.py`` they target
the parallel dev server via ``shed_server_dev`` / ``test_shed_name_dev``.

**Image requirement:** both need the rebuilt guest image whose ``shed-ext-rc``
carries the ``serve`` hub subcommand. On an OLD image (or on Firecracker, where
the loopback-only hub is unreachable from the bridge IP — the documented
FC-degrade) the proxy returns ``503 RC_HUB_UNAVAILABLE`` and these SKIP cleanly:
the hub binary / reachability is a genuine precondition, not a regression.

A ``shell``-kind session is used because it needs no agent auth, so its activity
is driven by the pane-stability engine and is deterministic in CI.
"""

from __future__ import annotations

import json
import time
import urllib.error
import urllib.request

import pytest

from fixtures.server import resolve_server_entry

# See test_rc_enrichment.py — the same image-compatibility signatures gate the
# `shed-ext-rc create` precondition.
_RC_INCOMPAT_SIGNS = (
    "command not found",
    "no such file",
    "executable file not found",
    "unknown kind",
    "unknown flag",
    "unknown shorthand flag",
    "flag provided but not defined",
)


def _api_base(server) -> str:
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


def _require_feature(base: str, token: str) -> None:
    info = _get_json(base, "/api/info")
    features = info.get("features")
    if isinstance(features, list) and token not in features:
        pytest.skip(
            f"server advertises features={features} without {token!r} — it "
            "predates this rc hub surface"
        )


def _require_rc_create(r) -> None:
    if r.returncode == 0:
        return
    text = (r.stderr or "").lower()
    if any(sign in text for sign in _RC_INCOMPAT_SIGNS):
        pytest.skip(
            "shed-ext-rc create --kind shell failed (image predates multi-agent "
            f"RC or omits the binary): exit={r.returncode} stderr={r.stderr!r}"
        )
    pytest.fail(
        "shed-ext-rc create --kind shell failed unexpectedly: "
        f"exit={r.returncode} stderr={r.stderr!r}"
    )


def _proxy_sessions_or_skip(base: str, shed: str) -> list[dict]:
    """GET the proxied hub session list. Skip on 503 RC_HUB_UNAVAILABLE (old
    image without the hub binary, or the FC loopback-unreachable degrade)."""
    url = f"{base}/api/sheds/{shed}/rc/v1/sessions"
    try:
        with urllib.request.urlopen(url, timeout=15) as resp:
            body = json.loads(resp.read().decode("utf-8"))
    except urllib.error.HTTPError as e:
        detail = e.read().decode("utf-8", "replace")
        if e.code == 503 and "RC_HUB_UNAVAILABLE" in detail:
            pytest.skip(
                "rc hub unavailable (image predates `shed-ext-rc serve`, or the "
                f"FC loopback-unreachable degrade): {detail!r}"
            )
        pytest.fail(f"proxy GET {url} -> {e.code}: {detail!r}")
    return body.get("sessions") or []


def _rc_session(sessions: list[dict], slug_or_name: str) -> dict | None:
    for s in sessions:
        if s.get("slug") == slug_or_name or s.get("tmux_session") == slug_or_name:
            return s
    return None


def test_rc_proxy_sessions_carry_activity(shed_server_dev, test_shed_name_dev):
    """`GET /api/sheds/{name}/rc/v1/sessions` (the hub reverse proxy) returns the
    shell session with an `activity` value derived by the pane-stability engine
    within ~15s. Needs the rebuilt image (hub binary); skips on
    RC_HUB_UNAVAILABLE."""
    server = shed_server_dev
    shed = test_shed_name_dev
    base = _api_base(server)

    _require_feature(base, "rc-proxy")
    server.create(shed, image="extensions")
    r = server.exec(shed, ["shed-ext-rc", "create", "--kind", "shell", "--wait=false"])
    _require_rc_create(r)

    # Poll the proxied hub session list for our session carrying an activity.
    deadline = time.monotonic() + 15.0
    sess = None
    while time.monotonic() < deadline:
        sessions = _proxy_sessions_or_skip(base, shed)
        # There is exactly one shell rc session in this shed; take the first rc row.
        for s in sessions:
            if s.get("kind") == "shell":
                sess = s
                break
        if sess is not None and sess.get("activity"):
            break
        time.sleep(0.5)

    assert sess is not None, "no shell rc session returned via the hub proxy"
    activity = sess.get("activity")
    assert activity in ("working", "idle", "needs_input", "unknown"), (
        f"unexpected/absent activity via proxy: {sess!r}"
    )


def test_rc_events_streams_on_activity(shed_server_dev, test_shed_name_dev):
    """`GET /api/rc/events` streams an event once a shell session's pane changes
    (a command is sent to it). Needs the rebuilt image; skips on
    RC_HUB_UNAVAILABLE."""
    server = shed_server_dev
    shed = test_shed_name_dev
    base = _api_base(server)

    _require_feature(base, "rc-events")
    _require_feature(base, "rc-proxy")
    server.create(shed, image="extensions")
    r = server.exec(shed, ["shed-ext-rc", "create", "--kind", "shell", "--wait=false"])
    _require_rc_create(r)

    # Precondition + discover the tmux session name: the proxy must be reachable
    # (else skip), and the aggregator only opens an upstream for a shed that has
    # rc sessions — which this one now does.
    sessions = _proxy_sessions_or_skip(base, shed)
    shell = next((s for s in sessions if s.get("kind") == "shell"), None)
    assert shell is not None, f"no shell rc session to drive: {sessions!r}"
    tmux = shell.get("tmux_session")
    assert tmux, f"session missing tmux_session: {shell!r}"

    # Open the aggregate SSE stream, then drive activity by sending a command into
    # the shell pane; the pane change surfaces as an activity.changed event that
    # the aggregator fans out. Read (bounded) until we see an `event:` line.
    req = urllib.request.Request(
        f"{base}/api/rc/events", headers={"Accept": "text/event-stream"}
    )
    got_event = False
    with urllib.request.urlopen(req, timeout=30) as stream:
        # Give the aggregator a moment to open its upstream reader, then trigger.
        time.sleep(1.0)
        server.exec(shed, ["tmux", "send-keys", "-t", tmux, "sleep 2", "Enter"])
        deadline = time.monotonic() + 20.0
        while time.monotonic() < deadline:
            raw = stream.readline()
            if not raw:
                break
            line = raw.decode("utf-8", "replace")
            if line.startswith("event:"):
                got_event = True
                break

    assert got_event, "no SSE event observed on /api/rc/events after driving pane activity"
