"""Core E2E: identify, the shed list and the rendered dashboard, lifecycle
(start / stop / delete), create (stream / cancel / error), and a screenshot.

`client.dashboard_rows()` reads what the UI rendered (`dashboard.dump`),
normalized to `[{name, status, host}]`, so these assert on the UI's truth rather
than only the backend's.
"""

from __future__ import annotations

import platform

import pytest


PNG_MAGIC = b"\x89PNG\r\n\x1a\n"


def test_identify_is_hermetic(client, mock):
    info = client.identify()
    assert info["test_mode"] is True
    assert info["mock_base_url"] == mock.base_url
    assert info["core"] == "rust"
    assert info["platform"] == "tauri"
    assert info["socket_path"].endswith("shed-tauri.sock")


def test_sheds_list_matches_fixture(client):
    client.sheds_refresh()  # re-sync to the reset mock
    client.wait_until(lambda: len(client.sheds_list()) >= 2, timeout=10, what="sheds populated")
    names = sorted(s["name"] for s in client.sheds_list())
    assert names == ["callbell", "hello-world"]


def test_dashboard_rows_match_fixture(client):
    client.sheds_refresh()
    client.wait_until(
        lambda: len(client.dashboard_rows()) >= 2, timeout=10, what="dashboard populated")
    rows = {r["name"]: r for r in client.dashboard_rows()}
    assert set(rows) == {"hello-world", "callbell"}
    assert rows["hello-world"]["status"] == "running"
    assert rows["callbell"]["status"] == "stopped"
    # The configured server name is stamped as the host.
    assert all(r["host"] == "mock" for r in rows.values())


def test_null_sheds_is_an_empty_list_not_an_error(client, mock):
    # A real shed-server answers `{"sheds": null}` when it has none. That decodes
    # to an empty list, and the host is still reachable — not a host error.
    mock.set_sheds(None)
    client.sheds_refresh()
    client.wait_until(lambda: client.dashboard_rows() == [], timeout=10,
                      what="dashboard empty")
    assert client.sheds_list() == []
    assert client.dashboard_dump()["host_errors"] == []
    assert client.sheds_host_errors() == []


def test_cloud_lifecycle_rows(client, mock):
    # The remote backend's lifecycle (plan 037 §3.14): a cloud shed that is
    # stopping reads `stopping` (it read `unknown`) with the busy spinner and no
    # Start or Delete, one that is starting the same, and a stopped one with a
    # reason reads as the CLI's
    # `shed list` does. Everything here is read back off the RENDERED row
    # (`dashboard.dump`'s `status_text` and `controls`), not the backend's view.
    # `rebooting` is no status shed-core knows: it still reads `unknown`.
    mock.set_sheds([
        {"name": "cloudy", "status": "stopping", "backend": "remote"},
        {"name": "booting", "status": "starting", "backend": "remote"},
        {"name": "full-zone", "status": "stopped", "status_reason": "capacity",
         "backend": "remote"},
        {"name": "rebooting", "status": "rebooting", "backend": "remote"},
    ])
    client.wait_until(lambda: client.current_pane() is not None, timeout=15,
                      what="frontend ready")
    client.navigate("sheds")
    client.wait_until(lambda: client.current_pane() == "sheds", timeout=15, what="pane=sheds")
    client.sheds_refresh()

    def rendered() -> dict:
        rows = {r["name"]: r for r in client.dashboard_dump()["rows"]}
        if set(rows) != {"cloudy", "booting", "full-zone", "rebooting"}:
            return {}
        # Off-pane (or before the cards mount) the rendered fields are null.
        if any(r.get("status_text") is None or r.get("controls") is None
               for r in rows.values()):
            return {}
        return rows

    client.wait_until(lambda: rendered(), timeout=10, what="cloud rows rendered")
    rows = rendered()

    cloudy = rows["cloudy"]
    assert cloudy["status"] == "stopping", cloudy
    assert cloudy["status_text"] == "stopping", cloudy
    assert cloudy["controls"]["spinner"] is True, cloudy
    assert "Start" not in cloudy["controls"]["buttons"], cloudy
    assert "Delete" not in cloudy["controls"]["buttons"], cloudy
    assert cloudy["controls"]["buttons"] == [], cloudy  # nothing to press while busy

    booting = rows["booting"]
    assert booting["status"] == "starting", booting
    assert booting["status_text"] == "starting", booting
    assert booting["controls"] == {"spinner": True, "buttons": []}, booting

    full = rows["full-zone"]
    assert full["status"] == "stopped", full
    assert full["status_reason"] == "capacity", full
    assert full["status_text"] == "stopped (capacity)", full
    assert full["controls"] == {"spinner": False, "buttons": ["Start", "Delete"]}, full

    # Negative control: a status shed-core does not know is not mistaken for
    # one it does.
    odd = rows["rebooting"]
    assert odd["status"] == "unknown", odd
    assert odd["status_text"] == "unknown", odd
    assert odd["controls"]["spinner"] is False, odd


def test_sheds_list_carries_image_and_digest(client):
    # A shed created from the server's default image carries only `image_digest`
    # (no `image` label); one created from an alias carries both.
    client.sheds_refresh()
    client.wait_until(lambda: len(client.sheds_list()) >= 2, timeout=10, what="sheds populated")
    sheds = {s["name"]: s for s in client.sheds_list()}
    assert not sheds["callbell"].get("image"), sheds["callbell"]
    assert sheds["callbell"]["image_digest"].startswith("sha256:")
    assert sheds["hello-world"]["image"] == "full"
    assert sheds["hello-world"]["image_digest"].startswith("sha256:")


def test_lifecycle_stop_start_delete(client):
    client.sheds_refresh()
    client.wait_until(lambda: len(client.sheds_list()) >= 2, timeout=10, what="sheds populated")

    # stop a running shed → sheds.list reflects it
    client.shed_action("stop", "hello-world")
    client.wait_until(lambda: client.shed_status("hello-world") == "stopped", what="hello-world stopped")

    # start a stopped shed
    client.shed_action("start", "callbell")
    client.wait_until(lambda: client.shed_status("callbell") == "running", what="callbell running")

    # after a refresh, the UI-truth dashboard reflects both
    client.sheds_refresh()
    dump = {r["name"]: r for r in client.dashboard_rows()}
    assert dump["hello-world"]["status"] == "stopped"
    assert dump["callbell"]["status"] == "running"

    # delete drops it from the server
    client.shed_action("delete", "callbell")
    client.wait_until(
        lambda: "callbell" not in {s["name"] for s in client.sheds_list()},
        what="callbell deleted",
    )


def test_create_streams_to_complete(client):
    cid = client.create_start("shared-created", image="base")
    client.wait_until(
        lambda: client.create_status(cid).get("state") == "complete",
        timeout=15,
        what="create complete",
    )
    st = client.create_status(cid)
    assert st["state"] == "complete"
    assert st["shed"]["name"] == "shared-created"
    assert len(st["messages"]) >= 1  # progress messages streamed

    # the new shed shows up in the dashboard after a refresh
    client.sheds_refresh()
    assert "shared-created" in {r["name"] for r in client.dashboard_rows()}


def test_create_sends_the_chosen_image(client, mock):
    cid = client.create_start("picked", image="base")
    client.wait_until(lambda: client.create_status(cid).get("state") == "complete",
                      timeout=15, what="create complete")
    # The image reached the create request...
    assert mock.last_create()["image"] == "base"
    # ...and the created shed reports it.
    client.sheds_refresh()
    client.wait_until(lambda: any(s["name"] == "picked" for s in client.sheds_list()),
                      timeout=10, what="created shed listed")
    created = {s["name"]: s for s in client.sheds_list()}["picked"]
    assert created["image"] == "base"


def test_create_cancel_drops_it(client):
    cid = client.create_start("cancelme")
    client.create_cancel(cid)
    # After cancel the store entry is gone (whether or not it had completed), so
    # create.status reports {"state": "unknown"}.
    assert client.create_status(cid).get("state") == "unknown"


def test_create_error_surfaced(client, mock):
    # A server-side create failure surfaces as an `error` state carrying the
    # message: the shed-app CreateStore folds the SSE error event into
    # state=error.
    mock.create_should_fail = True
    cid = client.create_start("doomed")
    client.wait_until(
        lambda: client.create_status(cid).get("state") == "error",
        timeout=15,
        what="create error",
    )
    st = client.create_status(cid)
    assert st["state"] == "error"
    assert "doomed" in (st.get("error") or "")


def test_screenshot_returns_non_empty_png(client):
    # Deliberately lenient — "a non-empty PNG of expected dimensions" — so the
    # gate isn't coupled to a container's GL stack (dashboard_rows is the truth
    # op). Tauri shells out to a screenshot tool; on macOS that's screencapture,
    # which is Screen-Recording-TCC-gated for a harness-launched binary, so the
    # Linux/Xvfb capture is the real gate and macOS skips.
    if platform.system() == "Darwin":
        pytest.skip("screenshot on macOS is Screen-Recording-TCC-gated; Linux/Xvfb is the gate")
    # Front the window so the capture has the app on screen.
    client.show_window()

    captured: dict = {}

    def grab() -> bool:
        # The client may raise ("window not realized") until the surface is up; retry.
        png, width, height = client.screenshot(scale=1)
        captured.update(png=png, width=width, height=height)
        return True

    client.wait_until(grab, timeout=20, what="window realized + screenshot")
    assert captured["png"][:8] == PNG_MAGIC
    assert captured["width"] > 0 and captured["height"] > 0
