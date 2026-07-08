"""M0: in-process screenshots of the window and the menu popover."""

from __future__ import annotations

import pytest
from client import ShedError

from _marks import mac_only

pytestmark = mac_only

PNG_MAGIC = b"\x89PNG\r\n\x1a\n"


def test_window_screenshot(shed):
    shed.show_window()
    png, w, h = shed.screenshot(surface="window", scale=2)
    assert png.startswith(PNG_MAGIC)
    assert w > 0 and h > 0
    metrics = shed.window_metrics()
    # 2x scale => pixel dims are ~2x the logical content size.
    assert abs(w - metrics["window_width"] * 2) <= 4
    assert abs(h - metrics["window_height"] * 2) <= 4


def test_menu_screenshot(shed):
    shed.open_menu(True)
    try:
        png, w, h = shed.screenshot(surface="menu", scale=1)
        assert png.startswith(PNG_MAGIC)
        assert w > 0 and h > 0
    finally:
        shed.open_menu(False)


def test_menu_screenshot_requires_open_menu(shed):
    shed.open_menu(False)
    with pytest.raises(ShedError) as exc:
        shed.screenshot(surface="menu")
    assert exc.value.code == "internal"


def test_approvals_pane_renders_request(shed, fake):
    # The Approvals pane renders a real pending request (not just empty state).
    rid = fake.emit_request("ssh-agent", "sign", "shot-shed", "ssh-ed25519", server="mini3")
    shed.wait_until(lambda: rid in {a["id"] for a in shed.approvals_list()}, what="request queued")
    shed.show_window()
    shed.navigate("approvals")
    png, w, h = shed.screenshot(surface="window", scale=2)
    assert png.startswith(PNG_MAGIC) and w > 0 and h > 0
    # Clean up so the queued request doesn't leak into later tests.
    shed.approval_decide(rid, "deny")


def test_activity_pane_renders_events(shed, fake):
    # The Activity pane renders streamed audit events.
    fake.emit_event("aws-credentials", "get_credentials", "shot-shed", result="ok", server="mini3")
    shed.wait_until(lambda: any(e["shed"] == "shot-shed" for e in shed.activity_list()),
                    what="event in feed")
    shed.show_window()
    shed.navigate("activity")
    png, w, h = shed.screenshot(surface="window", scale=2)
    assert png.startswith(PNG_MAGIC) and w > 0 and h > 0
