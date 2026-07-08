"""M0: the read-only dashboard reflects the mock server, handles the
`{"sheds": null}` empty shape, and drives sidebar navigation."""

from __future__ import annotations

import pytest
from client import ShedError

from _marks import mac_only

pytestmark = mac_only


def test_lists_fixture_sheds(shed):
    shed.refresh()
    sheds = {s["name"]: s for s in shed.sheds_list()}
    assert "hello-world" in sheds
    assert sheds["hello-world"]["status"] == "running"
    assert sheds["hello-world"]["host"] == "mock"
    assert sheds["callbell"]["status"] == "stopped"


def test_host_is_reachable_with_version(shed):
    shed.refresh()
    hosts = {h["name"]: h for h in shed.host_list()}
    assert hosts["mock"]["reachable"] is True
    assert hosts["mock"]["version"] == "0.0.0-mock"


def test_null_sheds_handled(shed, mock):
    mock.set_sheds(None)  # the real server returns {"sheds": null} when empty
    shed.refresh()
    shed.wait_until(lambda: shed.sheds_list() == [], what="empty shed list")
    # And the host stays reachable — null sheds is not an error.
    assert {h["name"]: h["reachable"] for h in shed.host_list()}["mock"] is True


def test_navigate_panes(shed):
    for pane in ("agents", "approvals", "activity", "sheds"):
        shed.navigate(pane)
        assert shed.ui_state()["pane"] == pane


def test_navigate_rejects_unknown_pane(shed):
    with pytest.raises(ShedError) as exc:
        shed.navigate("bogus")
    assert exc.value.code == "invalid-param"
