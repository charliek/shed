"""Smoke: the app is up, hermetic, and answers the IPC protocol."""

from __future__ import annotations

from _marks import mac_only

pytestmark = mac_only


def test_identify_is_hermetic(shed, mock):
    info = shed.identify()
    assert info["test_mode"] is True
    assert info["mock_base_url"] == mock.base_url
    assert info["protocol_version"] == 1
    assert info["app_id"] == "ai.stridelabs.ShedDesktop"
