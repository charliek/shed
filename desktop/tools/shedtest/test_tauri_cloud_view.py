"""Cloud sheds on the Sheds pane (plan 041 B2, criterion 14): what the real app renders
for a remote server whose sheds are machines of an account service, read back through
`dashboard.dump` (each row's `tag`, `meta`, `lease` and `controls`, and its `cloud`:
the open cloud detail, the usage lines and the delete confirmation), and driven through
the pane's own controls by the test-mode doors `ui.press_shed_details`,
`ui.press_extend`, `ui.fill_extend_hours`, `ui.press_extend_custom`,
`ui.press_shed_delete` and `ui.press_delete_confirm`.

* A cloud row wears the `Cloud` tag in its own tint, and its meta line says its size,
  its status and its time left, amber under 2 h.
* Its detail shows the image, created, run time, lease end and cap, and Extend: a
  preset and a custom value each move the lease and the row says so; a refusal shows
  its fixed words, never the server's text.
* Restart is never offered on a cloud row, and its Delete asks first.
* The usage line sits under the cloud server's heading.
* A server whose rows have no `cloud` shows them as before.

One self-managed app per module with two PRIVATE mocks: `mock`, a remote server with
leases (`MockShedServer.serve_cloud`), and `mock2`, a server without cloud sheds.

`SHED_CLOUD_SHOTS=<dir>` keeps the screenshots (Linux; the capture is TCC-gated on
macOS, where the cells still run and only the capture is skipped).
"""

from __future__ import annotations

import json
import os
import platform
import shutil
import subprocess
import tempfile
from dataclasses import dataclass
from datetime import timedelta
from pathlib import Path

import pytest

import hermetic
import ui
from client import ShedError, TauriClient
from mockserver import CLOUD_USAGE, EXTEND_REFUSAL_MESSAGE, MockShedServer, _parse_utc, cloud_sheds

PNG_MAGIC = b"\x89PNG\r\n\x1a\n"
SHOTS = os.environ.get("SHED_CLOUD_SHOTS")

PLAIN_SHEDS = [
    {"name": "api-box", "status": "running", "backend": "vz", "cpus": 4, "memory_mb": 8192,
     "created_at": "2026-11-21T12:00:00Z"},
    {"name": "old-box", "status": "stopped", "backend": "firecracker", "cpus": 2,
     "memory_mb": 4096, "created_at": "2026-11-20T12:00:00Z"},
]

#: The usage line of the mock's recorded usage (`CLOUD_USAGE`).
USAGE_WORDS = "2 of 100 machines (1 running, 1 stopped) · organization 2 of 100"


def _config(path: Path) -> Path:
    path.write_text(
        "servers:\n"
        "    mock:\n        host: 127.0.0.1\n        http_port: 8080\n        ssh_port: 2222\n"
        "    mock2:\n        host: 127.0.0.1\n        http_port: 8081\n        ssh_port: 2222\n"
        "default_server: mock\nsheds: {}\n")
    return path


@dataclass
class Rig:
    c: TauriClient
    mock: MockShedServer
    mock2: MockShedServer

    def reset(self) -> None:
        self.mock.reset()
        self.mock.serve_cloud()
        self.mock2.reset()
        self.mock2.set_sheds([dict(s) for s in PLAIN_SHEDS])
        self.c.call("ui.close_shed_delivery")
        self.c.call("ui.press_delete_confirm", {"button": "cancel"})
        _go(self.c, "sheds")
        self.c.sheds_refresh()
        self.c.wait_until(lambda: self.cloud()["delete_confirm"] is None, timeout=10,
                          what="no delete confirmation")

    def dump(self) -> dict:
        return self.c.dashboard_dump()

    def cloud(self) -> dict:
        return self.dump()["cloud"]

    def rows(self) -> dict[tuple[str, str], dict]:
        return {(r["host"], r["name"]): r for r in self.dump()["rows"]}

    def row(self, name: str, host: str = "mock") -> dict:
        return self.rows()[(host, name)]

    def detail(self) -> dict | None:
        return self.cloud()["detail"]

    def open_detail(self, name: str) -> dict:
        self.c.call("ui.press_shed_details", {"server": "mock", "shed": name})
        self.c.wait_until(lambda: (self.detail() or {}).get("shed") == name, timeout=10,
                          what=f"{name}'s cloud detail")
        return self.detail()


@pytest.fixture(scope="module")
def rig():
    cfg = ui._SUBPROC["tauri"]
    if not cfg.binary.exists():
        raise RuntimeError(f"tauri binary not found at {cfg.binary}; build it first (make tauri-build).")
    mock, mock2 = MockShedServer(), MockShedServer()
    mock.serve_cloud()
    mock2.set_sheds([dict(s) for s in PLAIN_SHEDS])
    mock.start()
    mock2.start()
    home = Path(tempfile.mkdtemp(prefix="shed-cloud-"))
    proc = log_fh = None
    try:
        url = hermetic.mock_url(mock.base_url, {"mock2": mock2.base_url})
        env = ui.subproc_env(cfg, runtime_dir=home, mock_base_url=url,
                             config_path=_config(home / "cloud-config.yaml"))
        sock = home / cfg.sock_rel
        log = home / "cloud-ui.log"
        log_fh = open(log, "wb")
        proc = subprocess.Popen([str(cfg.binary)], env=env, stdout=log_fh, stderr=subprocess.STDOUT)
        ui.await_hermetic("tauri", sock=sock, mock_base_url=url, proc=proc, log=log)
        client = TauriClient(sock)
        try:
            client.wait_until(lambda: client.current_pane() is not None, timeout=30,
                              what="frontend ready")
            client.set_appearance("light")
            yield Rig(client, mock, mock2)
        finally:
            client.close()
    finally:
        ui.terminate(proc)
        if log_fh:
            log_fh.close()
        mock.stop()
        mock2.stop()
        shutil.rmtree(home, ignore_errors=True)


@pytest.fixture
def r(rig: Rig) -> Rig:
    rig.reset()
    return rig


def _go(c: TauriClient, pane: str) -> None:
    c.navigate(pane)
    c.wait_until(lambda: c.current_pane() == pane, timeout=15, what=f"pane={pane}")


def _shot(c: TauriClient, name: str, anchor: str) -> None:
    """Scroll to `anchor`, wait for a painted frame with no transient animation left,
    and take the window's picture (kept under `SHED_CLOUD_SHOTS`)."""
    if platform.system() == "Darwin":
        return
    def settled() -> bool:
        token = c.call("ui.scroll_to", {"anchor": anchor})["token"]
        c.wait_until(lambda: (c.call("ui.scrolled").get("scrolled") or {}).get("token") == token,
                     timeout=10, what=f"scrolled to {anchor}")
        done = c.call("ui.scrolled")["scrolled"]
        assert done["found"] is True, anchor
        return done["animations"] == 0
    c.wait_until(settled, timeout=10, what=f"{anchor} in view, animations done")
    png, w, h = c.screenshot(scale=1)
    assert png[:8] == PNG_MAGIC and w > 0 and h > 0
    if SHOTS:
        out = Path(SHOTS).expanduser()
        out.mkdir(parents=True, exist_ok=True)
        (out / name).write_bytes(png)


def _end(r: Rig, name: str):
    return _parse_utc(r.mock.shed(name)["cloud"]["lease"]["expires_at"])


def _wait_rows(r: Rig) -> None:
    """Both servers' rows rendered, the cloud ones with their lease."""
    r.c.wait_until(
        lambda: {k for k, v in r.rows().items() if v["meta"] is not None} == {
            ("mock", n) for n in ("cloud-ok", "cloud-soon", "cloud-ended", "cloud-unknown",
                                  "cloud-stopped", "nocturn-cloud")
        } | {("mock2", "api-box"), ("mock2", "old-box")},
        timeout=15, what="both servers' rows rendered")


# ---------------------------------------------------------------------------
# The rows
# ---------------------------------------------------------------------------

def test_a_cloud_row_has_its_tag_size_and_time_left(r: Rig):
    _wait_rows(r)
    ok = r.row("cloud-ok")
    assert (ok["tag"], ok["tag_tint"]) == ("Cloud", "cloud"), ok
    assert ok["meta"].startswith("2 vCPU · 8 GB · running · deleted in 5h "), ok["meta"]
    assert ok["status_text"] == "running"
    assert ok["lease"]["state"] == "ok" and ok["lease"]["amber"] is False, ok["lease"]
    assert ok["lease"]["text"].startswith("deleted in 5h "), ok["lease"]
    stopped = r.row("cloud-stopped")
    assert stopped["meta"].startswith("2 vCPU · 8 GB · stopped · deleted in 1d "), stopped["meta"]
    ended = r.row("cloud-ended")
    assert ended["meta"] == "2 vCPU · 8 GB · stopped · lease ended"
    assert (ended["lease"]["state"], ended["lease"]["amber"]) == ("ended", False)
    unknown = r.row("cloud-unknown")
    assert unknown["meta"] == "2 vCPU · 8 GB · starting · lease unknown"
    assert unknown["controls"] == {"spinner": True, "buttons": []}
    _shot(r.c, "cloud-rows.png", anchor="host:mock")


def test_the_lease_is_amber_under_two_hours(r: Rig):
    _wait_rows(r)
    soon = r.row("cloud-soon")
    assert soon["meta"].startswith("8 vCPU · 32 GB · running · deleted in 1h "), soon["meta"]
    assert soon["lease"]["state"] == "soon" and soon["lease"]["amber"] is True, soon["lease"]
    for name in ("cloud-ok", "cloud-stopped", "nocturn-cloud", "cloud-ended", "cloud-unknown"):
        assert r.row(name)["lease"]["amber"] is False, name


def test_restart_is_never_offered_on_a_cloud_row(r: Rig):
    _wait_rows(r)
    buttons = {n: r.row(n)["controls"]["buttons"] for n in
               ("cloud-ok", "cloud-soon", "cloud-ended", "cloud-stopped", "nocturn-cloud")}
    assert buttons == {
        "cloud-ok": ["Open in Terminal", "Stop"],
        "cloud-soon": ["Open in Terminal", "Stop"],
        "cloud-ended": ["Delete"],
        "cloud-stopped": ["Start", "Delete"],
        "nocturn-cloud": ["Open in Terminal", "Stop"],
    }, buttons


def test_a_server_without_cloud_sheds_shows_its_rows_as_before(r: Rig):
    _wait_rows(r)
    api, old = r.row("api-box", "mock2"), r.row("old-box", "mock2")
    assert (api["tag"], api["tag_tint"], api["meta"]) == ("vz", "vz", "4 vCPU · 8 GB · running")
    assert (old["tag"], old["tag_tint"], old["meta"]) == ("firecracker", "fc", "2 vCPU · 4 GB · stopped")
    assert api["lease"] is None and old["lease"] is None
    assert api["cloud_row"] is None and old["cloud_row"] is None
    assert api["controls"]["buttons"] == ["Open in Terminal", "Restart", "Stop"]
    assert old["controls"]["buttons"] == ["Start", "Delete"]
    _shot(r.c, "plain-rows.png", anchor="host:mock2")
    # No usage line under its heading; its detail is the egress one, not a cloud one.
    assert [u["server"] for u in r.cloud()["usage"]] == ["mock"]
    r.c.call("ui.press_shed_details", {"server": "mock2", "shed": "api-box"})
    r.c.wait_until(lambda: (r.c.shed_egress_dump() or {}).get("shed") == "api-box",
                   timeout=10, what="api-box's egress detail")
    assert r.detail() is None
    # Its Delete asks nothing: the shed is deleted at once, as before.
    r.c.call("ui.press_shed_delete", {"server": "mock2", "shed": "old-box"})
    r.c.wait_until(lambda: r.mock2.shed("old-box") is None, timeout=10, what="old-box deleted")
    assert r.cloud()["delete_confirm"] is None


# ---------------------------------------------------------------------------
# The usage line
# ---------------------------------------------------------------------------

def test_the_usage_line_is_under_the_cloud_servers_heading(r: Rig):
    _wait_rows(r)
    r.c.wait_until(lambda: r.cloud()["usage"] == [{"server": "mock", "words": USAGE_WORDS}],
                   timeout=10, what="mock's usage line")
    _shot(r.c, "cloud-usage.png", anchor="host:mock")


def test_no_usage_line_when_usage_cannot_be_read(r: Rig):
    """A server whose usage cannot be read has no line; the line is read again when
    the server's cloud rows change (here: one fewer)."""
    _wait_rows(r)
    r.c.wait_until(lambda: r.cloud()["usage"] != [], timeout=10, what="mock's usage line")
    r.mock.serve_cloud(sheds=[s for s in cloud_sheds() if s["name"] != "nocturn-cloud"],
                       usage=None)
    r.c.sheds_refresh()
    r.c.wait_until(lambda: ("mock", "nocturn-cloud") not in r.rows(), timeout=10,
                   what="nocturn-cloud gone")
    r.c.wait_until(lambda: r.cloud()["usage"] == [], timeout=10, what="no usage line")
    assert r.row("cloud-ok")["tag"] == "Cloud"


# ---------------------------------------------------------------------------
# The detail and Extend
# ---------------------------------------------------------------------------

def test_the_cloud_detail_shows_the_machine_and_extend(r: Rig):
    _wait_rows(r)
    d = r.open_detail("cloud-ok")
    facts = d["facts"]
    assert facts["image"] == r.mock.shed("cloud-ok")["cloud"]["image"], facts
    assert facts["created"] and facts["created"].endswith("UTC"), facts
    assert facts["run"].startswith("running for 3h "), facts
    assert facts["lease_end"].endswith("UTC (" + r.row("cloud-ok")["lease"]["text"] + ")"), facts
    assert facts["cap"].endswith("UTC"), facts
    ext = d["extend"]
    assert [(p["hours"], p["label"], p["disabled"]) for p in ext["presets"]] == [
        (6, "+6 h", False), (12, "+12 h", False), (24, "+24 h", False), (48, "+48 h", False)]
    assert ext["custom"] == {"value": "", "max": r.row("cloud-ok")["cloud_row"]["extend_max_h"],
                             "disabled": True}
    _shot(r.c, "cloud-detail.png", anchor="shed:mock/cloud-ok")


def test_a_shed_whose_lease_cannot_be_extended_offers_no_extend(r: Rig):
    _wait_rows(r)
    for name in ("nocturn-cloud", "cloud-ended"):
        d = r.open_detail(name)
        assert d["extend"] is None, (name, d)
        r.c.call("ui.close_shed_delivery")
        r.c.wait_until(lambda: r.detail() is None, timeout=10, what="detail closed")


def test_extend_with_a_preset_moves_the_lease_and_the_row(r: Rig):
    _wait_rows(r)
    before = _end(r, "cloud-ok")
    r.open_detail("cloud-ok")
    r.c.call("ui.press_extend", {"server": "mock", "shed": "cloud-ok", "hours": 6})
    # The shell keeps a shed's last outcome while the app runs (a shed listed again
    # under its name and creation time is the same shed), so wait for THIS one.
    r.c.wait_until(lambda: r.mock.extend_requests() != [], timeout=10, what="the extend sent")
    r.c.wait_until(lambda: (r.detail() or {}).get("extend", {}).get("busy") is False
                   and ((r.detail() or {}).get("result") or {}).get("ok") is True,
                   timeout=10, what="the extend's result")
    words = r.detail()["result"]["words"]
    assert words.startswith("Extended by 6 h; deleted at "), words
    (sent,) = r.mock.extend_requests()
    assert sent["name"] == "cloud-ok" and sent["hours"] == 6, sent
    assert _end(r, "cloud-ok") == before + timedelta(hours=6)
    # The row is refreshed: its lease reads six hours later.
    r.c.wait_until(lambda: r.row("cloud-ok")["lease"]["text"].startswith("deleted in 11h "),
                   timeout=10, what="cloud-ok's row extended")


def test_extend_with_a_custom_value(r: Rig):
    _wait_rows(r)
    before = _end(r, "cloud-ok")
    r.open_detail("cloud-ok")
    custom = lambda: r.detail()["extend"]["custom"]  # noqa: E731
    # Out of range or not a whole number: the Extend is not pressable, and a press of
    # it sends nothing.
    for bad in ("0", "1.5", "abc", str(r.row("cloud-ok")["cloud_row"]["extend_max_h"] + 1)):
        r.c.call("ui.fill_extend_hours", {"server": "mock", "shed": "cloud-ok", "value": bad})
        r.c.wait_until(lambda: custom()["value"] == bad, timeout=10, what=f"typed {bad!r}")
        assert custom()["disabled"] is True, bad
        r.c.call("ui.press_extend_custom", {"server": "mock", "shed": "cloud-ok"})
    r.c.call("ui.fill_extend_hours", {"server": "mock", "shed": "cloud-ok", "value": "30"})
    r.c.wait_until(lambda: custom() == {**custom(), "value": "30", "disabled": False},
                   timeout=10, what="30 typed")
    r.c.call("ui.press_extend_custom", {"server": "mock", "shed": "cloud-ok"})
    r.c.wait_until(lambda: (((r.detail() or {}).get("result") or {}).get("words") or "")
                   .startswith("Extended by 30 h; "), timeout=10, what="the custom extend's result")
    assert r.detail()["result"]["words"].startswith("Extended by 30 h; "), r.detail()
    assert [x["hours"] for x in r.mock.extend_requests()] == [30], "only the valid press was sent"
    assert _end(r, "cloud-ok") == before + timedelta(hours=30)
    # The field is cleared, and the row reads 30 hours later.
    assert custom()["value"] == ""
    r.c.wait_until(lambda: r.row("cloud-ok")["lease"]["text"].startswith("deleted in 1d 11h"),
                   timeout=10, what="cloud-ok's row extended by 30 h")


@pytest.mark.parametrize("code,reason,words", [
    ("LEASE_AT_CAP", "", "the extend would take the lease past its cap; extend by fewer hours"),
    ("PERSON_INACTIVE", "person", "the shed's person is no longer active at the account service"),
])
def test_a_refusal_shows_its_words(r: Rig, code: str, reason: str, words: str):
    _wait_rows(r)
    before = _end(r, "cloud-ok")
    r.open_detail("cloud-ok")
    r.mock.refuse_extend(code, reason)
    r.c.call("ui.press_extend", {"server": "mock", "shed": "cloud-ok", "hours": 12})
    r.c.wait_until(lambda: ((r.detail() or {}).get("result") or {}).get("words") == words,
                   timeout=10, what="the refusal")
    shown = r.detail()["result"]["words"]
    assert shown == words, shown
    assert EXTEND_REFUSAL_MESSAGE not in r.detail()["rendered"]
    assert _end(r, "cloud-ok") == before, "a refusal moves nothing"
    if code == "LEASE_AT_CAP":
        _shot(r.c, "cloud-refusal.png", anchor="shed:mock/cloud-ok")


# ---------------------------------------------------------------------------
# Delete asks
# ---------------------------------------------------------------------------

def test_delete_on_a_cloud_row_asks_first(r: Rig):
    _wait_rows(r)
    r.c.call("ui.press_shed_delete", {"server": "mock", "shed": "cloud-stopped"})
    r.c.wait_until(lambda: r.cloud()["delete_confirm"] is not None, timeout=10,
                   what="the delete confirmation")
    ask = r.cloud()["delete_confirm"]
    assert ask == {"server": "mock", "shed": "cloud-stopped", "title": "Delete cloud-stopped?",
                   "words": "Deletes the machine and its disk.", "buttons": ["Cancel", "Delete"]}, ask
    _shot(r.c, "cloud-delete-confirm.png", anchor="host:mock")
    assert r.mock.shed("cloud-stopped") is not None, "nothing is deleted before the answer"
    # Cancel keeps the shed.
    r.c.call("ui.press_delete_confirm", {"button": "cancel"})
    r.c.wait_until(lambda: r.cloud()["delete_confirm"] is None, timeout=10, what="cancelled")
    assert r.mock.shed("cloud-stopped") is not None
    # Delete deletes it, and its row goes.
    r.c.call("ui.press_shed_delete", {"server": "mock", "shed": "cloud-stopped"})
    r.c.wait_until(lambda: r.cloud()["delete_confirm"] is not None, timeout=10, what="asked again")
    r.c.call("ui.press_delete_confirm", {"button": "delete"})
    r.c.wait_until(lambda: r.mock.shed("cloud-stopped") is None, timeout=10, what="deleted")
    r.c.wait_until(lambda: ("mock", "cloud-stopped") not in r.rows(), timeout=10,
                   what="its row gone")
    assert r.cloud()["delete_confirm"] is None


def _ask_delete(r: Rig, name: str) -> None:
    r.c.call("ui.press_shed_delete", {"server": "mock", "shed": name})
    r.c.wait_until(lambda: (r.cloud()["delete_confirm"] or {}).get("shed") == name, timeout=10,
                   what=f"Delete asks about {name}")


def test_the_delete_question_closes_when_its_row_no_longer_offers_delete(r: Rig):
    """The confirmation stands only while the shed's CURRENT row offers Delete: another
    client starting the shed, or the shed going, closes it, and a press of its Delete
    then sends nothing."""
    _wait_rows(r)
    _ask_delete(r, "cloud-stopped")
    assert r.mock._set_status("cloud-stopped", "running")  # started elsewhere
    r.c.sheds_refresh()
    r.c.wait_until(lambda: r.cloud()["delete_confirm"] is None, timeout=10,
                   what="the question closed once the row offers no Delete")
    assert "Delete" not in r.row("cloud-stopped")["controls"]["buttons"]
    r.c.call("ui.press_delete_confirm", {"button": "delete"})
    r.c.sheds_refresh()  # a round trip through the dashboard after the press
    assert r.mock.shed("cloud-stopped") is not None, "a closed question deletes nothing"

    _ask_delete(r, "cloud-ended")
    names = {s["name"] for s in r.mock.snapshot()[1]}
    r.mock.set_sheds([s for s in r.mock.snapshot()[1] if s["name"] != "cloud-ended"])
    r.c.sheds_refresh()
    r.c.wait_until(lambda: r.cloud()["delete_confirm"] is None, timeout=10,
                   what="the question closed once the shed is gone")
    r.c.call("ui.press_delete_confirm", {"button": "delete"})
    r.c.sheds_refresh()
    assert {s["name"] for s in r.mock.snapshot()[1]} == names - {"cloud-ended"}, \
        "nothing else was deleted"


def test_an_outstanding_extend_holds_across_reopening_the_detail(r: Rig):
    """An extend in flight belongs to the shed, not to its open detail: closing and
    reopening the detail shows it still running, offers no second press, and shows its
    outcome when it ends; the lease moves once."""
    _wait_rows(r)
    before = _end(r, "cloud-ok")
    gate = r.mock.hold_extend()
    try:
        r.open_detail("cloud-ok")
        r.c.call("ui.press_extend", {"server": "mock", "shed": "cloud-ok", "hours": 6})
        r.c.wait_until(lambda: r.mock.cloud_counts()["extend_arrived"] == 1, timeout=10,
                       what="the extend held at the mock")
        r.c.wait_until(lambda: r.detail()["extend"]["busy"] is True, timeout=10, what="busy")
        r.c.call("ui.close_shed_delivery")
        r.c.wait_until(lambda: r.detail() is None, timeout=10, what="detail closed")
        d = r.open_detail("cloud-ok")
        r.c.wait_until(lambda: r.detail()["extend"]["busy"] is True, timeout=10,
                       what="still busy after reopening")
        d = r.detail()
        assert all(p["disabled"] for p in d["extend"]["presets"]), d
        assert d["extend"]["custom"]["disabled"] is True, d
        # Every way to extend again lands nowhere.
        r.c.call("ui.press_extend", {"server": "mock", "shed": "cloud-ok", "hours": 12})
        r.c.call("ui.fill_extend_hours", {"server": "mock", "shed": "cloud-ok", "value": "5"})
        r.c.call("ui.press_extend_custom", {"server": "mock", "shed": "cloud-ok"})
        r.c.sheds_refresh()  # a round trip through the dashboard after the presses
        assert r.mock.cloud_counts()["extend_arrived"] == 1, "no second extend was sent"
    finally:
        gate.set()
    r.c.wait_until(lambda: r.detail()["extend"]["busy"] is False
                   and ((r.detail() or {}).get("result") or {}).get("ok") is True,
                   timeout=10, what="the first extend's outcome, in the reopened detail")
    assert r.detail()["result"]["words"].startswith("Extended by 6 h; "), r.detail()
    assert [x["hours"] for x in r.mock.extend_requests()] == [6]
    assert _end(r, "cloud-ok") == before + timedelta(hours=6)


def test_a_stale_usage_answer_never_replaces_a_newer_one(r: Rig):
    """A usage read still in flight when a server's cloud rows went away and came back
    is an old read: the line shows the newer answer, never the older one's."""
    plain = [{"name": "plain-a", "status": "stopped", "backend": "vz", "cpus": 2,
              "memory_mb": 4096, "created_at": "2026-11-20T12:00:00Z"}]
    r.mock.set_sheds([dict(s) for s in plain])
    r.c.sheds_refresh()
    r.c.wait_until(lambda: r.cloud()["usage"] == [], timeout=10, what="no cloud rows, no line")
    base = r.mock.cloud_counts()["usage_arrived"]
    gate = r.mock.hold_usage()
    try:
        # The rows come back: the first read (the old answer) is held at the mock.
        r.mock.serve_cloud()
        r.c.sheds_refresh()
        r.c.wait_until(lambda: r.mock.cloud_counts()["usage_arrived"] == base + 1, timeout=10,
                       what="the first usage read held")
        # They go away, and come back with other counts: a second read, answered.
        r.mock.set_sheds([dict(s) for s in plain])
        r.c.sheds_refresh()
        r.c.wait_until(lambda: ("mock", "cloud-ok") not in r.rows(), timeout=10,
                       what="cloud rows gone")
        newer = json.loads(json.dumps(CLOUD_USAGE))
        newer["person"].update({"machines": 3, "running": 2})
        newer["org"]["machines"] = 3
        r.mock.serve_cloud(usage=newer)
        r.c.sheds_refresh()
        words = "3 of 100 machines (2 running, 1 stopped) · organization 3 of 100"
        r.c.wait_until(lambda: r.cloud()["usage"] == [{"server": "mock", "words": words}],
                       timeout=10, what="the newer usage line")
    finally:
        gate.set()
    r.c.wait_until(lambda: r.mock.cloud_counts()["usage_answered"]
                   == r.mock.cloud_counts()["usage_arrived"], timeout=10,
                   what="the held read answered")
    r.c.sheds_refresh()  # a round trip through the dashboard after the old answer
    assert r.cloud()["usage"] == [{"server": "mock", "words": words}], r.cloud()["usage"]
    assert USAGE_WORDS not in str(r.cloud()["usage"])


def test_the_cloud_doors_check_their_params(r: Rig):
    """A door with a bad param is refused before anything is pressed."""
    for op, params in [
        ("ui.press_extend", {"server": "mock", "shed": "cloud-ok"}),
        ("ui.press_extend", {"server": "mock", "shed": "cloud-ok", "hours": "6"}),
        ("ui.fill_extend_hours", {"server": "mock", "shed": "cloud-ok"}),
        ("ui.press_delete_confirm", {"button": "yes"}),
        ("ui.press_shed_details", {"server": "mock"}),
    ]:
        with pytest.raises(ShedError) as e:
            r.c.call(op, params)
        assert e.value.code == "bad_request", (op, params)
