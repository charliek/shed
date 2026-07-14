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
carries the ``serve`` hub subcommand. ``create`` succeeding is NOT proof of that
(multi-agent ``create`` shipped before the hub did), so hub capability is probed
explicitly via the binary's usage text (`_require_hub_capable`); an image from
before the hub SKIPS cleanly — the hub binary is a genuine precondition, not a
regression.

**Both backends are hub-capable.** ``DialService`` routes through the guest
agent's vsock TCP proxy on VZ *and* Firecracker, so the loopback hub is reachable
on either. Once the image is proven hub-capable and ``shed-ext-rc create`` has
succeeded, a lingering ``503 RC_HUB_UNAVAILABLE`` from the proxy is a FAILURE —
the hub or the dial path is broken — not a skip. A brief startup grace is
allowed for the on-demand hub to come up on first create.

A ``shell``-kind session is used because it needs no agent auth, so its activity
is driven by the pane-stability engine and is deterministic in CI.
"""

from __future__ import annotations

import json
import time
import urllib.error

import pytest

from fixtures.apiendpoint import resolve_api_endpoint

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


def _require_feature(ep, token: str) -> None:
    info = ep.get_json("/api/info")
    features = info.get("features")
    if isinstance(features, list) and token not in features:
        pytest.skip(
            f"server advertises features={features} without {token!r} — it "
            "predates this rc hub surface"
        )


def _require_hub_capable(server, shed: str) -> None:
    """Skip when the image's ``shed-ext-rc`` predates the ``serve`` hub subcommand.

    ``create --kind shell`` succeeding is not enough to enter the fail-not-skip
    regime: multi-agent ``create`` shipped before the hub, so an image from that
    window creates sessions fine but can never serve the hub — an environment
    gap, not a dial-path regression. The usage text is the version-agnostic
    probe: a hub-capable binary lists the ``serve`` verb (an older one prints a
    usage without it, whatever the exit code)."""
    r = server.exec(shed, ["shed-ext-rc", "help"])
    text = f"{r.stdout or ''}\n{r.stderr or ''}"
    if any(sign in text.lower() for sign in _RC_INCOMPAT_SIGNS):
        pytest.skip(
            f"image predates multi-agent RC or omits shed-ext-rc: {text!r}"
        )
    if "serve" not in text:
        pytest.skip(
            "image's shed-ext-rc lacks the `serve` hub subcommand (predates the "
            "rc hub) — rebuild the extensions image to run the hub tests"
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


class _HubUnavailable(Exception):
    """Raised when the proxy returns 503 RC_HUB_UNAVAILABLE, so the caller can
    allow a brief on-demand-startup grace before deciding it is a failure."""

    def __init__(self, detail: str):
        super().__init__(detail)
        self.detail = detail


def _proxy_sessions(ep, shed: str) -> list[dict]:
    """GET the proxied hub session list.

    There is NO skip path here. Image/server preconditions are gated earlier
    (`_require_feature` + `_require_hub_capable` + `_require_rc_create`); the
    image is proven hub-capable and the hub is reachable on both VZ and
    Firecracker, so a 503 RC_HUB_UNAVAILABLE is a real regression — it is
    raised as `_HubUnavailable` for the caller's startup-grace loop, and any
    other HTTPError is a hard failure. (A 401 is caught earlier by `ep.open`.)"""
    path = f"/api/sheds/{shed}/rc/v1/sessions"
    try:
        with ep.open(path, timeout=15) as resp:
            body = json.loads(resp.read().decode("utf-8"))
    except urllib.error.HTTPError as e:
        detail = e.read().decode("utf-8", "replace")
        if e.code == 503 and "RC_HUB_UNAVAILABLE" in detail:
            raise _HubUnavailable(detail)
        pytest.fail(f"proxy GET {ep.base}{path} -> {e.code}: {detail!r}")
    return body.get("sessions") or []


def _proxy_sessions_wait(ep, shed: str, grace: float = 8.0) -> list[dict]:
    """Fetch the proxied session list, tolerating a brief RC_HUB_UNAVAILABLE
    window while the on-demand hub starts on first create, then FAILING (never
    skipping) if the 503 persists past the grace budget."""
    deadline = time.monotonic() + grace
    last = None
    while True:
        try:
            return _proxy_sessions(ep, shed)
        except _HubUnavailable as e:
            last = e.detail
            if time.monotonic() >= deadline:
                pytest.fail(
                    "rc hub still 503 RC_HUB_UNAVAILABLE after `shed-ext-rc "
                    f"create` succeeded and a {grace:.0f}s startup grace — the hub "
                    "or the DialService dial path is broken. This is a parity "
                    f"regression, not a skip: {last!r}"
                )
            time.sleep(0.5)


def _rc_session(sessions: list[dict], slug_or_name: str) -> dict | None:
    for s in sessions:
        if s.get("slug") == slug_or_name or s.get("tmux_session") == slug_or_name:
            return s
    return None


def test_rc_proxy_sessions_carry_activity(shed_server_dev, test_shed_name_dev):
    """`GET /api/sheds/{name}/rc/v1/sessions` (the hub reverse proxy) returns the
    shell session with an `activity` value derived by the pane-stability engine
    within ~15s. Needs the rebuilt image (hub binary); once create succeeds a
    lingering RC_HUB_UNAVAILABLE is a failure, not a skip (both backends reach
    the loopback hub through the guest tcpproxy)."""
    server = shed_server_dev
    shed = test_shed_name_dev
    ep = resolve_api_endpoint(server.name)

    _require_feature(ep, "rc-proxy")
    server.create(shed, image="extensions")
    _require_hub_capable(server, shed)
    r = server.exec(shed, ["shed-ext-rc", "create", "--kind", "shell", "--wait=false"])
    _require_rc_create(r)

    # Poll the proxied hub session list for our session carrying an activity.
    deadline = time.monotonic() + 15.0
    sess = None
    while time.monotonic() < deadline:
        sessions = _proxy_sessions_wait(ep, shed)
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
    (a command is sent to it). Needs the rebuilt image; once create succeeds a
    lingering RC_HUB_UNAVAILABLE is a failure, not a skip."""
    server = shed_server_dev
    shed = test_shed_name_dev
    ep = resolve_api_endpoint(server.name)

    _require_feature(ep, "rc-events")
    _require_feature(ep, "rc-proxy")
    server.create(shed, image="extensions")
    _require_hub_capable(server, shed)
    r = server.exec(shed, ["shed-ext-rc", "create", "--kind", "shell", "--wait=false"])
    _require_rc_create(r)

    # Precondition + discover the tmux session name: the proxy must be reachable
    # (create already succeeded, so a persistent 503 fails), and the aggregator
    # only opens an upstream for a shed that has rc sessions — which this one now
    # does.
    sessions = _proxy_sessions_wait(ep, shed)
    shell = next((s for s in sessions if s.get("kind") == "shell"), None)
    assert shell is not None, f"no shell rc session to drive: {sessions!r}"
    tmux = shell.get("tmux_session")
    assert tmux, f"session missing tmux_session: {shell!r}"

    # Open the aggregate SSE stream, then drive activity by sending a command into
    # the shell pane; the pane change surfaces as an activity.changed event that
    # the aggregator fans out. Read (bounded) until we see that specific event —
    # NOT any `event:` line: the aggregator also emits synthetic
    # hub.unavailable/shed.stopped frames, and a transient upstream reconnect
    # would satisfy a bare `event:` check without the pane-driven change.
    got_event = False
    with ep.open("/api/rc/events", timeout=30, accept="text/event-stream") as stream:
        # Give the aggregator a moment to open its upstream reader, then trigger.
        time.sleep(1.0)
        server.exec(shed, ["tmux", "send-keys", "-t", tmux, "sleep 2", "Enter"])
        deadline = time.monotonic() + 20.0
        while time.monotonic() < deadline:
            raw = stream.readline()
            if not raw:
                break
            line = raw.decode("utf-8", "replace")
            if line.startswith("event: activity.changed"):
                got_event = True
                break

    assert got_event, "no activity.changed SSE event observed on /api/rc/events after driving pane activity"
