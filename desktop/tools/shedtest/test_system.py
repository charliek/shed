"""M7: the System pane — per-host disk usage from GET /api/system/df."""

from __future__ import annotations

from _marks import mac_only

pytestmark = mac_only

PNG_MAGIC = b"\x89PNG\r\n\x1a\n"
_GiB = 1024 ** 3


def test_system_df_returns_per_host_usage(shed):
    usage = shed.system_df()
    assert usage, "expected at least one host"
    row = usage[0]
    assert "host" in row
    totals = row["usage"]["totals"]
    # Matches the mock fixture (sheds physical = 1 GiB, all physical = 1.5 GiB).
    assert totals["sheds"]["physical_bytes"] == _GiB
    assert totals["all"]["physical_bytes"] == _GiB + _GiB // 2
    assert row["usage"]["images"][0]["name"] == "base"


def test_system_pane_renders(shed):
    shed.system_df()  # populate + publish state.systemUsage
    shed.show_window()
    shed.navigate("system")
    png, w, h = shed.screenshot(surface="window", scale=2)
    assert png.startswith(PNG_MAGIC) and w > 0 and h > 0
