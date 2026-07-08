"""B2: the RC classifier + launch/list/kill/inject drive the Agents pane, in test
mode against an in-memory session store.

The behavioral tests run cross-target (mac + tauri, `needs_agents`) on the shared
`client` fixture. The mac-only tail (the launch *sheet* — the Tauri pane uses an
inline form instead) stays `mac_only`; the render test is target-aware (the mac
screenshot takes a `surface`, the Rust-core one a `scale`), and asserts the
drivable `agents.dump` truth on tauri."""

from __future__ import annotations

import platform

import pytest

from client import ShedError

from _marks import mac_only, needs_agents

pytestmark = needs_agents


# ---- classifier (the pure rc.classify utility) ---------------------------

def test_classify_agent_ready(client):
    pane = "·✔︎· Connected\nContinue at https://claude.ai/code?environment=env_01ABC"
    r = client.rc_classify("claude-broker", pane)
    assert r["state"] == "ready"
    assert r["url"] == "https://claude.ai/code?environment=env_01ABC"


def test_classify_repl_needs_trust(client):
    r = client.rc_classify("claude-rc", "Quick safety check: Is this a project you trust?")
    assert r["state"] == "needs-trust"


def test_classify_agent_reconnecting(client):
    r = client.rc_classify("claude-broker", "·|· Reconnecting · retrying in 2.5s")
    assert r["state"] == "reconnecting"
    assert "url" not in r


# ---- launch / list / kill ------------------------------------------------

def test_launch_list_kill(client):
    client.sheds_refresh()
    session = client.rc_launch("hello-world", kind="claude-rc", display_name="demo")
    slug = session["slug"]
    assert session["state"] == "ready"
    assert session["url"].startswith("https://claude.ai/code/session_")
    assert session["kind"] == "claude-rc"

    # It shows up in the list with its tmux name.
    listed = {s["slug"]: s for s in client.rc_list()}
    assert slug in listed
    assert listed[slug]["tmux_session"] == f"rc-{slug}"

    # Kill removes it.
    client.rc_kill("hello-world", slug)
    client.wait_until(lambda: slug not in {s["slug"] for s in client.rc_list()},
                      what="rc session gone")


def test_launch_agent_kind_gets_environment_url(client):
    client.sheds_refresh()
    session = client.rc_launch("hello-world", kind="claude-broker")
    try:
        assert session["url"].startswith("https://claude.ai/code?environment=env_")
    finally:
        client.rc_kill("hello-world", session["slug"])


def test_launch_carries_managed_provenance(client):
    """A launched session is managed and stamps SHED_RC_* provenance, which
    survives the round-trip through rc.list (RC Session Convention v2)."""
    client.sheds_refresh()
    session = client.rc_launch("hello-world", kind="claude-rc", display_name="demo")
    slug = session["slug"]
    try:
        assert session["managed"] is True
        assert session["rc_id"]
        assert session["created_by"].startswith("shed-desktop/")
        assert session["created_at"].endswith("Z")
        listed = {s["slug"]: s for s in client.rc_list()}
        assert listed[slug]["managed"] is True
        assert listed[slug]["created_by"] == session["created_by"]
        assert listed[slug]["rc_id"] == session["rc_id"]
    finally:
        client.rc_kill("hello-world", slug)


def test_console_terminal_preview_attaches_session(client):
    """The Agents console button resolves to `ssh … tmux attach -t rc-<slug>`,
    observable (without spawning) via terminal.preview with a session."""
    client.sheds_refresh()
    session = client.rc_launch("hello-world", kind="shell", display_name="dev")
    slug = session["slug"]
    try:
        prev = client.terminal_preview("hello-world", host=session["host"],
                                       session=session["tmux_session"])
        assert prev["argv"][-4:] == ["tmux", "attach", "-t", f"rc-{slug}"]
        assert f"tmux attach -t rc-{slug}" in prev["command"]
    finally:
        client.rc_kill("hello-world", slug)


# ---- initial prompt (the typed kickoff line) -----------------------------

def test_launch_with_initial_prompt(client):
    """rc.launch accepts an optional initial_prompt for the kinds that take typed
    input (claude-rc → prompt, shell → command)."""
    client.sheds_refresh()
    rc = client.rc_launch("hello-world", kind="claude-rc", display_name="demo",
                          initial_prompt="summarize this repo")
    try:
        assert rc["state"] == "ready"
        assert rc["kind"] == "claude-rc"
    finally:
        client.rc_kill("hello-world", rc["slug"])

    sh = client.rc_launch("hello-world", kind="shell", display_name="dev",
                          initial_prompt="npm install && npm test")
    try:
        assert sh["state"] == "ready"
        assert sh["kind"] == "shell"
    finally:
        client.rc_kill("hello-world", sh["slug"])


def test_launch_rejects_control_char_prompt(client):
    """A control char in initial_prompt is rejected before the test-mode branch,
    surfacing the shared `invalid-param` code on both targets."""
    client.sheds_refresh()
    with pytest.raises(ShedError) as exc:
        client.rc_launch("hello-world", kind="claude-rc", initial_prompt="bad\nvalue")
    assert exc.value.code == "invalid-param"


def test_launch_rejects_overlong_prompt(client):
    client.sheds_refresh()
    with pytest.raises(ShedError) as exc:
        client.rc_launch("hello-world", kind="shell", initial_prompt="a" * 2001)
    assert exc.value.code == "invalid-param"


def test_launch_rejects_prompt_for_broker(client):
    """claude-broker has no pane to type into, so a prompt is rejected (the guest
    rejects it too)."""
    client.sheds_refresh()
    with pytest.raises(ShedError) as exc:
        client.rc_launch("hello-world", kind="claude-broker", initial_prompt="nope")
    assert exc.value.code == "invalid-param"


# ---- inject a legacy session (cross-target: assert the IPC truth) --------

def test_inject_legacy_session_lists(client):
    """A legacy/unmanaged rc-* session injected into the store lists with
    managed=false (host-less → the default server) — the IPC truth, every target."""
    client.sheds_refresh()
    client.rc_inject_test("hello-world", "legacy1", kind="claude-broker", state="ready")
    try:
        listed = {s["slug"]: s for s in client.rc_list()}
        assert "legacy1" in listed
        assert listed["legacy1"]["managed"] is False
    finally:
        client.rc_kill("hello-world", "legacy1")


# ---- render (target-aware: the pane draws an injected session) -----------

def test_agents_pane_renders_injected_session(client, target):
    """The Agents pane renders an injected session: the drivable `agents.dump`
    truth on tauri (needs no display) + a window screenshot where the capture is
    available — the mac Swift app grabs its NSWindow in-process, while macOS-tauri
    shells out to a Screen-Recording-TCC-gated tool, so the Linux/Xvfb render gate
    covers that pixel (mirrors `test_screenshot_returns_non_empty_png`)."""
    client.sheds_refresh()
    client.rc_inject_test("hello-world", "shot1", kind="claude-rc", state="ready",
                          display_name="demo", managed=True)
    try:
        client.navigate("agents")
        client.show_window()
        # Tauri publishes the rendered sessions for agents.dump — the logical render
        # proof, which needs no display.
        if target == "tauri":
            client.wait_until(
                lambda: "shot1" in {s["slug"] for s in client.agents_dump()},
                what="agents.dump shows the injected session",
            )
        # Screenshot proof — skipped only where capture is TCC-gated (macOS-tauri).
        if not (target == "tauri" and platform.system() == "Darwin"):
            png, w, h = (client.screenshot(surface="window", scale=2) if target == "mac"
                         else client.screenshot(scale=2))
            assert png[:8] == b"\x89PNG\r\n\x1a\n", "expected a PNG"
            assert w > 0 and h > 0
    finally:
        client.rc_kill("hello-world", "shot1")


# ---- mac-only tail (mac op shapes; no tauri analog) ----------------------

@mac_only
def test_launch_sheet_is_screenshot_driveable(shed):
    """The mac launch SHEET (ui.show_launch) — the Tauri pane uses an inline form
    instead, so this stays mac-only."""
    shed.show_launch()
    png, w, h = shed.screenshot(surface="window", scale=2)
    assert png[:8] == b"\x89PNG\r\n\x1a\n", "expected a PNG"
    assert w > 0 and h > 0
