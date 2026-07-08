"""M4: the preferences window opens and is screenshot-able."""

from __future__ import annotations

from _marks import mac_only

pytestmark = mac_only

PNG_MAGIC = b"\x89PNG\r\n\x1a\n"


def test_open_preferences_and_screenshot(shed):
    shed.open_preferences()
    png, w, h = shed.screenshot(surface="preferences", scale=1)
    assert png.startswith(PNG_MAGIC)
    assert w > 0 and h > 0
