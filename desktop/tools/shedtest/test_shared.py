"""Cross-target E2E: the IPC ops the clients implement, driven through the
`client` (target-appropriate) + `target` fixtures so one suite is the
behavioral-parity gate. `identify` + a `screenshot` run on every target; the
backend-dependent tests (sheds / lifecycle / create) carry `@needs_backend` and
run on both `--target mac` + `--target tauri` — a regression on either leg fails
its CI leg.

The dashboard-truth op differs per UI (mac `ui.state.sheds`, tauri
`dashboard.dump.rows`) but is normalized by `client.dashboard_rows(target)` to
`[{name, status, host}]`, so the assertions here are identical across targets.
"""

from __future__ import annotations

import platform

import pytest

from _marks import needs_backend
from client import ShedError

PNG_MAGIC = b"\x89PNG\r\n\x1a\n"


def test_identify_is_hermetic(client, target, mock):
    info = client.identify()
    assert info["test_mode"] is True
    assert info["mock_base_url"] == mock.base_url
    if target == "tauri":
        # The Rust-core client stamps its platform + socket name (shed-tauri.sock).
        assert info["core"] == "rust"
        assert info["platform"] == target
        assert info["socket_path"].endswith(f"shed-{target}.sock")
    else:
        # The mac app also runs a Swift-fallback leg (SHED_DESKTOP_RUST_CORE=0),
        # so don't pin the backend here; the M0 ship-gates own the rust==swift
        # parity assertion. These fields are mac-only.
        assert info["protocol_version"] == 1
        assert info["app_id"] == "ai.stridelabs.ShedDesktop"


@needs_backend
def test_sheds_list_matches_fixture(client):
    client.sheds_refresh()  # re-sync to the reset mock (mac reads rendered state)
    client.wait_until(lambda: len(client.sheds_list()) >= 2, timeout=10, what="sheds populated")
    names = sorted(s["name"] for s in client.sheds_list())
    assert names == ["callbell", "hello-world"]


@needs_backend
def test_dashboard_rows_match_fixture(client, target):
    client.sheds_refresh()
    client.wait_until(
        lambda: len(client.dashboard_rows(target)) >= 2, timeout=10, what="dashboard populated")
    rows = {r["name"]: r for r in client.dashboard_rows(target)}
    assert set(rows) == {"hello-world", "callbell"}
    assert rows["hello-world"]["status"] == "running"
    assert rows["callbell"]["status"] == "stopped"
    # Both backends stamp the configured server name as the host.
    assert all(r["host"] == "mock" for r in rows.values())


@needs_backend
def test_lifecycle_stop_start_delete(client, target):
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
    dump = {r["name"]: r for r in client.dashboard_rows(target)}
    assert dump["hello-world"]["status"] == "stopped"
    assert dump["callbell"]["status"] == "running"

    # delete drops it from the server
    client.shed_action("delete", "callbell")
    client.wait_until(
        lambda: "callbell" not in {s["name"] for s in client.sheds_list()},
        what="callbell deleted",
    )


@needs_backend
def test_create_streams_to_complete(client, target):
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
    assert "shared-created" in {r["name"] for r in client.dashboard_rows(target)}


@needs_backend
def test_create_cancel_drops_it(client):
    cid = client.create_start("cancelme")
    client.create_cancel(cid)
    # After cancel the store entry is gone (whether or not it had completed):
    # the Tauri client's create.status reports {"state": "unknown"}; the mac app
    # raises not-found. Either outcome proves the create was dropped.
    try:
        assert client.create_status(cid).get("state") == "unknown"
    except ShedError as e:
        assert e.code == "not-found"


@needs_backend
def test_create_error_surfaced(client, mock):
    # A server-side create failure surfaces as an `error` state carrying the
    # message — the cross-target parity of the mac-only
    # test_lifecycle::test_create_error_surfaced. The shared shed-app CreateStore
    # folds the SSE error event into state=error the same way the mac backend does.
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


def test_screenshot_returns_non_empty_png(client, target):
    # Deliberately lenient — "a non-empty PNG of expected dimensions" — so the
    # gate isn't coupled to a container's GL stack (dashboard_rows is the truth
    # op). Tauri shells out to a screenshot tool; on macOS that's screencapture,
    # which is Screen-Recording-TCC-gated for a harness-launched binary, so the
    # Linux/Xvfb capture is the real gate and mac-tauri is skipped.
    if target == "tauri" and platform.system() == "Darwin":
        pytest.skip("tauri screenshot on macOS is Screen-Recording-TCC-gated; Linux/Xvfb is the gate")
    # Front the window so the capture has the app on screen (both targets have it).
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
