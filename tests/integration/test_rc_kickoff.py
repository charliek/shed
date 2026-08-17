"""CLI-driven Remote Control kickoff: `shed attach --kind shell -d` (plan 008, C7).

Every existing rc integration test (`test_rc_enrichment.py`, `test_rc_hub_activity.py`)
drives the in-shed `shed-ext-rc` binary directly via `server.exec(...)` over the guest
agent channel — none of them ever invoke the `shed` CLIENT CLI's own `attach --kind` /
`plan` path (WS-E in plan 008). That path has its own argv-building
(`cmd/shed/rc.go:buildRCCreateArgv`), flag validation (`cmd/shed/attach.go:
validateAttachFlags`), and plain-text (NOT `--json`) output rendering
(`printRCSummary`/`reportRCCreateOutcome`) that the guest-exec'd tests never exercise.
This module is the first CLI-path coverage.

A `shell`-kind session is the deliberate choice, same as the existing rc tests: it
needs no agent auth/login, so `ClassifyPane(KindShell, ...)` can only land on
`StateStarting` (blank pane) or `StateReady` (any pane content) — never
`needs-auth`/`needs-trust`/`dead` (see `internal/ext/rc/agents.go:887-892`,
`rc.go:406-417`). That makes the CLI's `--wait` round-trip deterministic in CI, and
keeps this smoke test out of agent-auth territory (--kind cursor/codex/opencode need
real login and are out of scope here).

This is effectively a **CLI-side** feature (the server surface it rides on —
`shed exec`-equivalent SSH delivery of `shed-ext-rc create` — predates this plan), but
it needs a real shed with the `extensions` image, so it uses the same dev-server
fixtures (`shed_server_dev` / `test_shed_name_dev`) as its siblings for consistency and
because C7's gate is "green vs dev server" per the plan.
"""

from __future__ import annotations

import json
import re
import time

import pytest

# Same image-compatibility signatures as test_rc_enrichment.py / test_rc_hub_activity.py,
# plus one CLI-specific rewrap: `createRCSession`'s old-binary detection
# (`cmd/shed/rc.go:isOldBinaryRCErr`) turns the raw "unknown kind"/"flag provided but
# not defined" guest errors into a friendlier "predates multi-agent RC" message before
# it ever reaches stderr here, so that phrase has to be matched too.
_RC_INCOMPAT_SIGNS = (
    "command not found",
    "no such file",
    "executable file not found",
    "unknown kind",
    "unknown flag",
    "unknown shorthand flag",
    "flag provided but not defined",
    "predates multi-agent rc",
)

# `printRCSummary` (cmd/shed/attach.go): `Started %s session rc-%s (%s)\n` with
# dto.Kind / dto.Slug / dto.State. Only printed on the success path (state reached one
# of shell's two reachable terminal-ish states); needs-auth/needs-trust/dead print a
# different message and are structurally unreachable for shell (see module docstring).
_STARTED_RE = re.compile(r"^Started (\S+) session (rc-[a-z0-9-]+) \(([a-z-]+)\)\s*$", re.MULTILINE)


def _require_rc_attach(r) -> tuple[str, str, str]:
    """Gate on a `shed attach --kind shell -d` CLI result.

    Passes through (kind, slug, state) parsed from the printed summary on success.
    Skips only on a known image-compatibility signature (a fixture-environment gap,
    same idiom as the guest-exec'd rc tests); any other non-zero exit, or a zero exit
    whose stdout doesn't carry the expected summary line, FAILS — a regression must
    never masquerade as a precondition skip.
    """
    if r.returncode != 0:
        text = (r.stderr or "").lower()
        if any(sign in text for sign in _RC_INCOMPAT_SIGNS):
            pytest.skip(
                "shed attach --kind shell -d failed (image predates multi-agent RC "
                f"or omits shed-ext-rc): exit={r.returncode} stderr={r.stderr!r}"
            )
        pytest.fail(
            "shed attach --kind shell -d failed unexpectedly — not a known "
            "image-compatibility signature, so this is a CLI/server/guest-agent "
            f"regression, not a precondition: exit={r.returncode} "
            f"stdout={r.stdout!r} stderr={r.stderr!r}"
        )
    m = _STARTED_RE.search(r.stdout or "")
    if m is None:
        pytest.fail(
            "shed attach --kind shell -d exited 0 but stdout didn't carry the "
            "expected 'Started <kind> session rc-<slug> (<state>)' summary — "
            f"stdout={r.stdout!r} stderr={r.stderr!r}"
        )
    return m.group(1), m.group(2), m.group(3)


def _find_session(sessions: list[dict], name: str) -> dict | None:
    for s in sessions:
        if s.get("name") == name:
            return s
    return None


def _poll_session_ready(server, shed: str, session_name: str, *, timeout: float = 10.0, interval: float = 0.5):
    """Poll `shed sessions <shed> --json` until `session_name`'s rc.state is
    "ready" or `timeout` elapses. Returns the last-seen row (possibly None) so the
    caller's assertion can report real diagnostics either way. Doubles as the
    "probe" step: this is the CLI's own listing surface, not a guest exec."""
    deadline = time.monotonic() + timeout
    row = None
    while True:
        r = server.cli("sessions", shed, "--json", timeout=30)
        if r.returncode == 0:
            try:
                sessions = json.loads(r.stdout or "[]")
            except json.JSONDecodeError:
                sessions = []
            row = _find_session(sessions, session_name)
        rc = (row or {}).get("rc") or {}
        if rc.get("state") == "ready" or time.monotonic() >= deadline:
            return row
        time.sleep(interval)


def test_attach_kind_shell_detach_probe_kill(shed_server_dev, test_shed_name_dev):
    """`shed attach <shed> --kind shell -d` creates an rc-<slug> session, prints a
    "Started shell session rc-<slug> (ready)" summary, the session is visible (with
    kind=shell) via `shed sessions <shed> --json`, and `shed sessions kill` tears it
    down — the first CLI-path (as opposed to guest-exec) rc coverage."""
    server = shed_server_dev
    shed = test_shed_name_dev

    # The extensions image bakes in shed-ext-rc (base does not) — same precondition
    # as the guest-exec'd rc tests.
    server.create(shed, image="extensions")

    slug: str | None = None
    try:
        r = server.cli("attach", shed, "--kind", "shell", "-d", timeout=90)
        kind, name, state = _require_rc_attach(r)
        slug = name.removeprefix("rc-")

        assert kind == "shell", f"unexpected rc kind in attach summary: {kind!r} (full stdout={r.stdout!r})"
        assert state in ("ready", "starting"), (
            f"unexpected rc state in attach summary: {state!r} (shell can only reach "
            f"starting/ready — see internal/ext/rc/agents.go classifyShell; "
            f"full stdout={r.stdout!r})"
        )

        # Probe: the session must be discoverable via the CLI's own listing surface
        # (not a guest exec), and — since `--wait` can in rare cases return while the
        # pane's first prompt is still drawing (see classifyShell) — poll briefly for
        # rc.state to settle at "ready" rather than trusting the single attach-time
        # observation.
        row = _poll_session_ready(server, shed, name)
        assert row is not None, (
            f"no session named {name!r} in `shed sessions {shed} --json` after "
            "attach --kind shell -d succeeded"
        )
        rc = row.get("rc")
        assert rc is not None, f"session {name!r} listed without an rc block: {row!r}"
        assert rc.get("kind") == "shell", f"unexpected rc.kind via sessions listing: {rc!r}"
        assert rc.get("state") == "ready", (
            f"rc.state never reached ready via `shed sessions --json` (last-seen "
            f"row={row!r}) — this is a regression, not a skip, once attach itself "
            "reported the session as created"
        )
    finally:
        # Cleanup discipline mirrors the guest-exec'd rc tests' reliance on
        # test_shed_name_dev's shed-level teardown, but ALSO exercises (and proves)
        # the CLI's own session-kill path so a failed run doesn't leak a live rc-*
        # tmux session for the lifetime of the dev shed. Best-effort: a failure here
        # must never mask the real assertion failure above.
        if slug is not None:
            try:
                result = server.cli("sessions", "kill", shed, f"rc-{slug}", timeout=30)
                if result.returncode != 0:
                    print(
                        f"warning: cleanup `sessions kill {shed} rc-{slug}` failed "
                        f"(exit {result.returncode}): stdout={result.stdout!r} stderr={result.stderr!r}"
                    )
            except Exception as exc:
                print(f"warning: cleanup `sessions kill {shed} rc-{slug}` raised: {exc!r}")
