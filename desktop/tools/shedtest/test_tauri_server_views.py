"""The views that read the servers themselves (plan 039 PR B), driven through the real
app over two mock servers and one that is down:

* the sidebar's four entries (Sheds, Agents, Activity, System);
* **Activity** — every configured server's own feed (`GET /api/activity`), merged
  newest first, paged with its cursor ("Load older"), and a server that could not be
  read named above the list;
* a **shed's detail** — its delivery and its egress: its EFFECTIVE profiles, read from
  the mock's `GET /api/egress/{name}` (a shed created without `--egress` shows the
  server's default, marked as such; "not restricted" only when none applies) and
  its recent blocks, the `deny` decisions pushed through the mock's
  `/api/egress/stream` for THAT incarnation (`shed_created_at`), re-read on
  `egress-changed`; a shed created again under its name shows none of the old
  incarnation's blocks;
* **System** — every server's egress profiles.

One self-managed app per module, over its own throwaway HOME, with two PRIVATE mocks:
`mock` and `mock2` (the per-server mock URL, `hermetic.mock_url`), both streaming
egress decisions (built with `egress_events=[]`, so the app's feed holds a stream
instead of resting on a `501`), and `down`, pointed at a closed port. The server
decides what a user may see; the app shows what it sent.

`SHED_VIEW_SHOTS=<dir>` keeps the screenshots (Linux; the capture is TCC-gated on
macOS, where the cells still run and only the capture is skipped).
"""

from __future__ import annotations

import os
import platform
import shutil
import subprocess
import tempfile
from dataclasses import dataclass
from pathlib import Path

import pytest

import hermetic
import ui
from client import TauriClient
from mockserver import DELIVERED_RECORD, MockShedServer

FIXTURES = Path(__file__).resolve().parent / "fixtures"
PNG_MAGIC = b"\x89PNG\r\n\x1a\n"
SHOTS = os.environ.get("SHED_VIEW_SHOTS")

#: hello-world's first incarnation on `mock`, and the one created after it was deleted.
FIRST = "2026-11-20T09:00:00Z"
SECOND = "2026-11-23T11:00:00Z"

MOCK_SHEDS = [
    {"name": "hello-world", "id": "q6c3g4ymb5m2dkxwe7kyz5hhzu", "status": "running",
     "backend": "firecracker", "cpus": 2, "memory_mb": 4096, "image": "full",
     "created_at": FIRST, "delivery": DELIVERED_RECORD, "egress_profiles": ["default"],
     "egress_port": 20001},
    {"name": "callbell", "status": "stopped", "backend": "vz", "created_at": "2026-11-19T08:00:00Z"},
]
MOCK2_SHEDS = [
    {"name": "api-box", "status": "running", "backend": "vz", "cpus": 4, "memory_mb": 8192,
     "created_at": "2026-11-21T12:00:00Z"},
]


def _ev(prefix: str, n: int, minute: int, kind: str, shed: str, **extra) -> dict:
    return {"id": f"{prefix}{n}", "at": f"2026-11-23T10:{minute:02d}:00Z", "kind": kind,
            "shed": {"name": shed}, "actor": {"kind": "user", "user": "ada"}, **extra}


#: Newest first on each server; the two interleave by time.
MOCK_ACTIVITY = [
    _ev("m", 3, 50, "shed.provision.end", "hello-world", result="ok"),
    _ev("m", 2, 30, "delivery", "hello-world", actor={"kind": "system"},
        delivery={"state": "partial", "outcome": "accepted"}),
    _ev("m", 1, 10, "shed.create", "hello-world", result="ok"),
]
MOCK2_ACTIVITY = [
    _ev("n", 2, 40, "shed.stop", "api-box", result="ok"),
    _ev("n", 1, 20, "shed.create", "api-box", actor={"kind": "client", "client": "ci"},
        result="error"),
]


def _config(path: Path) -> Path:
    path.write_text(
        "servers:\n"
        "    mock:\n        host: 127.0.0.1\n        http_port: 8080\n        ssh_port: 2222\n"
        "    mock2:\n        host: 127.0.0.1\n        http_port: 8081\n        ssh_port: 2222\n"
        "    down:\n        host: 127.0.0.1\n        http_port: 8082\n        ssh_port: 2222\n"
        "default_server: mock\nsheds: {}\n")
    return path


@dataclass
class Rig:
    c: TauriClient
    mock: MockShedServer
    mock2: MockShedServer

    def reset(self) -> None:
        for m in (self.mock, self.mock2):
            with m._lock:
                m.egress_enabled, m.egress_default = True, []
        self.mock.set_sheds([dict(s) for s in MOCK_SHEDS])
        self.mock2.set_sheds([dict(s) for s in MOCK2_SHEDS])
        with self.mock._lock:
            self.mock.activity_events = [dict(e) for e in MOCK_ACTIVITY]
            self.mock.activity_hidden = set()
            self.mock.activity_scan_budget = None
        with self.mock2._lock:
            self.mock2.activity_events = [dict(e) for e in MOCK2_ACTIVITY]
        self.c.call("ui.close_shed_delivery")
        self.c.sheds_refresh()


@pytest.fixture(scope="module")
def rig():
    cfg = ui._SUBPROC["tauri"]
    if not cfg.binary.exists():
        raise RuntimeError(f"tauri binary not found at {cfg.binary}; build it first (make tauri-build).")
    mock, mock2 = MockShedServer(egress_events=[]), MockShedServer(egress_events=[])
    mock.set_sheds([dict(s) for s in MOCK_SHEDS])
    mock2.set_sheds([dict(s) for s in MOCK2_SHEDS])
    mock.start()
    mock2.start()
    home = Path(tempfile.mkdtemp(prefix="shed-views-"))
    proc = log_fh = None
    try:
        url = hermetic.mock_url(mock.base_url, {"mock2": mock2.base_url})
        env = ui.subproc_env(cfg, runtime_dir=home, mock_base_url=url,
                             config_path=_config(home / "views-config.yaml"),
                             unreachable_hosts=("down",))
        sock = home / cfg.sock_rel
        log = home / "views-ui.log"
        log_fh = open(log, "wb")
        proc = subprocess.Popen([str(cfg.binary)], env=env, stdout=log_fh, stderr=subprocess.STDOUT)
        ui.await_hermetic("tauri", sock=sock, mock_base_url=url, proc=proc, log=log)
        client = TauriClient(sock)
        try:
            client.wait_until(lambda: client.current_pane() is not None, timeout=30,
                              what="frontend ready")
            client.wait_until(
                lambda: {s["server"]: s["state"] for s in client.egress_status()}.get("mock")
                == "connected"
                and {s["server"]: s["state"] for s in client.egress_status()}.get("mock2")
                == "connected",
                timeout=30, what="both egress streams connected")
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


def _shot(c: TauriClient, name: str) -> None:
    if platform.system() == "Darwin":
        return
    png, w, h = c.screenshot(scale=1)
    assert png[:8] == PNG_MAGIC and w > 0 and h > 0
    if SHOTS:
        out = Path(SHOTS).expanduser()
        out.mkdir(parents=True, exist_ok=True)
        (out / name).write_bytes(png)


def _scroll(c: TauriClient, anchor: str) -> None:
    """Scroll the dashboard to `anchor` and wait until it says it did, with no CSS
    animation (a card's fade-in) still running — what a screenshot should wait for."""
    def settled() -> bool:
        token = c.call("ui.scroll_to", {"anchor": anchor})["token"]
        c.wait_until(lambda: (c.call("ui.scrolled").get("scrolled") or {}).get("token") == token,
                     timeout=10, what=f"scrolled to {anchor}")
        done = c.call("ui.scrolled")["scrolled"]
        assert done["found"] is True, anchor
        return done["animations"] == 0
    c.wait_until(settled, timeout=10, what=f"{anchor} in view, animations done")


def _go(c: TauriClient, pane: str) -> None:
    c.navigate(pane)
    c.wait_until(lambda: c.current_pane() == pane, timeout=15, what=f"pane={pane}")


def _decision(shed: str, created: str, host: str, verdict: str, *, port: int = 443,
              reason: str = "default-deny") -> dict:
    return {"ts": "2026-11-23T10:15:00Z", "shed": shed, "shed_created_at": created,
            "host": host, "port": port, "protocol": "https", "verdict": verdict,
            "reason": reason}


# ---------------------------------------------------------------------------
# The sidebar
# ---------------------------------------------------------------------------

def test_the_sidebar_has_four_entries_and_lists_every_server(r: Rig):
    """Sheds, Agents, Activity, System: each navigates; the Sheds pane lists both
    servers' sheds; the sidebar's status foot names the three servers, `down` as
    unreachable."""
    c = r.c
    for pane in ("agents", "activity", "system", "sheds"):
        _go(c, pane)
    c.wait_until(lambda: {(x["host"], x["name"]) for x in c.dashboard_rows()} ==
                 {("mock", "hello-world"), ("mock", "callbell"), ("mock2", "api-box")},
                 timeout=20, what="both servers' sheds")
    c.wait_until(lambda: len((c.sidebar_dump() or {}).get("servers") or []) == 3,
                 timeout=20, what="three servers in the sidebar")
    servers = {s["name"]: s["reachable"] for s in c.sidebar_dump()["servers"]}
    assert servers == {"mock": True, "mock2": True, "down": False}, servers
    assert set(c.badges()) == {"sheds", "agents", "hosts"}
    _scroll(c, "shed:mock/hello-world")
    _shot(c, "b4-sidebar-sheds.png")


# ---------------------------------------------------------------------------
# Activity
# ---------------------------------------------------------------------------

def _activity(c: TauriClient) -> dict:
    return c.activity_dump() or {}


def test_activity_lists_every_servers_events_newest_first(r: Rig):
    """The pane shows both servers' events in one list, newest first, each with its
    server, kind, shed, actor and outcome (a delivery's state in words), and names
    the server it could not read; that server has no events in the list."""
    c = r.c
    _go(c, "activity")
    c.wait_until(lambda: _activity(c).get("state") == "ready" and len(_activity(c)["events"]) == 5,
                 timeout=20, what="five events")
    d = _activity(c)
    assert [e["id"] for e in d["events"]] == ["mock/m3", "mock2/n2", "mock/m2", "mock2/n1", "mock/m1"], d
    first = d["events"][0]
    assert first["server"] == "mock" and first["kind"] == "shed.provision.end", first
    assert first["shed"] == "hello-world" and first["actor"] == "ada" and first["outcome"] == "ok"
    by_id = {e["id"]: e for e in d["events"]}
    assert by_id["mock/m2"]["actor"] == "the server"
    assert by_id["mock/m2"]["outcome"] == "partial · accepted"
    assert by_id["mock2/n1"]["actor"] == "client ci"
    assert by_id["mock2/n1"]["outcome"] == "error"
    assert len(d["errors"]) == 1 and d["errors"][0].startswith("down: "), d["errors"]
    # `down` keeps its place (its events come with a later page), so the page after
    # is still offered.
    assert d["older"] == {"disabled": False, "text": "Load older"}, d["older"]
    _scroll(c, "activity")
    _shot(c, "b4-activity-two-servers.png")


def test_activity_pages_with_load_older(r: Rig):
    """Eighty events across the two servers: the first page shows the newest fifty,
    merged; "Load older" (the pane's own button, pressed through its door) brings the
    other thirty after them, newest first."""
    c = r.c
    with r.mock._lock:
        r.mock.activity_events = [_ev("m", n, 0, "shed.start", "hello-world", result="ok",
                                      at=f"2026-11-23T{10 + n // 60:02d}:{n % 60:02d}:00Z")
                                  for n in range(39, -1, -1)]
    with r.mock2._lock:
        r.mock2.activity_events = [_ev("n", n, 0, "shed.stop", "api-box", result="ok",
                                       at=f"2026-11-23T{10 + n // 60:02d}:{n % 60:02d}:30Z")
                                   for n in range(39, -1, -1)]
    _go(c, "sheds")
    _go(c, "activity")
    c.wait_until(lambda: _activity(c).get("state") == "ready" and len(_activity(c)["events"]) == 50,
                 timeout=20, what="the first page of fifty")
    assert _activity(c)["older"] == {"disabled": False, "text": "Load older"}
    c.press_activity_older()
    c.wait_until(lambda: len(_activity(c)["events"]) == 80, timeout=20, what="all eighty")
    d = _activity(c)
    want = [f"{srv}/{p}{n}" for n in range(39, -1, -1)
            for srv, p in (("mock2", "n"), ("mock", "m"))]
    assert [e["id"] for e in d["events"]] == want
    # Only `down` is left to read, so the page after is still offered.
    assert d["older"] == {"disabled": False, "text": "Load older"}, d["older"]
    assert len(d["errors"]) == 1, d["errors"]


def test_an_empty_page_with_a_cursor_still_offers_load_older(r: Rig):
    """A server's page can be empty and still have older events (its scan budget ran
    out among events this user may not see): the pane says so and offers "Load older",
    which reaches them."""
    c = r.c
    with r.mock._lock:
        r.mock.activity_events = [_ev("m", n, n, "shed.start", "hello-world", result="ok")
                                  for n in range(9, -1, -1)]
        r.mock.activity_hidden = {"m9", "m8", "m7"}
        r.mock.activity_scan_budget = 3
    with r.mock2._lock:
        r.mock2.activity_events = []
    _go(c, "sheds")
    _go(c, "activity")
    c.wait_until(lambda: _activity(c).get("state") == "ready", timeout=20, what="the first page")
    d = _activity(c)
    assert d["events"] == [], d
    assert d["empty"] and "No activity on this page" in d["empty"], d["empty"]
    assert d["older"] == {"disabled": False, "text": "Load older"}, d["older"]
    c.press_activity_older()
    c.wait_until(lambda: [e["id"] for e in _activity(c)["events"]] == ["mock/m6", "mock/m5", "mock/m4"],
                 timeout=20, what="the events past the scan budget")


# ---------------------------------------------------------------------------
# A shed's detail: delivery and egress
# ---------------------------------------------------------------------------

def _egress(c: TauriClient) -> dict:
    return c.shed_egress_dump() or {}


def test_shed_detail_shows_delivery_and_recent_blocks(r: Rig):
    """`egress.show` opens hello-world's detail on the Sheds pane: its delivery, its
    egress profile, and its recent blocks. A `deny` decision pushed on the egress
    stream for this incarnation appears (newest first, `host:port` and the reason); an
    `allow` one does not, nor one for another incarnation or another shed."""
    c = r.c
    c.egress_show(server="mock", shed="hello-world")
    c.wait_until(lambda: c.current_pane() == "sheds", timeout=15, what="pane=sheds")
    c.wait_until(lambda: _egress(c).get("profiles") == "Egress profile: default (audit)",
                 timeout=20, what="the egress profile")
    c.wait_until(lambda: (c.call("shed_delivery.dump").get("detail") or {}).get("state") == "delivered",
                 timeout=20, what="the delivery detail beside it")
    assert _egress(c)["blocks"] == [], _egress(c)
    r.mock.push_egress(_decision("hello-world", FIRST, "evil.example.test", "deny"))
    r.mock.push_egress(_decision("hello-world", FIRST, "ok.example.test", "allow", reason="allowed"))
    r.mock.push_egress(_decision("hello-world", "2026-01-01T00:00:00Z", "stale.example.test", "deny"))
    r.mock.push_egress(_decision("callbell", "2026-11-19T08:00:00Z", "other.example.test", "deny"))
    r.mock.push_egress(_decision("hello-world", FIRST, "198.51.100.7", "deny", port=22,
                                 reason="not-allowed"))
    c.wait_until(lambda: len(_egress(c).get("blocks") or []) == 2, timeout=20,
                 what="the two blocks of this incarnation")
    blocks = _egress(c)["blocks"]
    assert [b["target"] for b in blocks] == ["198.51.100.7:22", "evil.example.test:443"], blocks
    assert [b["reason"] for b in blocks] == ["not-allowed", "default-deny"], blocks
    # The op agrees: this incarnation's blocks, and its allowed decision is kept apart.
    recent = c.egress_recent("mock", "hello-world", FIRST)
    assert {e["verdict"] for e in recent} == {"allow", "deny"} and len(recent) == 3, recent
    assert len(c.egress_recent("mock", "hello-world", FIRST, blocks=True)) == 2
    _scroll(c, "shed:mock/hello-world")
    _shot(c, "b4-shed-detail-delivery-and-blocks.png")


def test_a_reused_shed_name_shows_none_of_the_old_blocks(r: Rig):
    """hello-world deleted and created again (a new `created_at`) is another shed:
    its detail shows no blocks though the old incarnation had some, until one is
    pushed for the new one."""
    c = r.c
    r.mock.push_egress(_decision("hello-world", FIRST, "old.example.test", "deny"))
    c.wait_until(lambda: len(c.egress_recent("mock", "hello-world", FIRST, blocks=True)) >= 1,
                 timeout=20, what="a block for the first incarnation")
    sheds = [dict(s) for s in MOCK_SHEDS]
    sheds[0]["created_at"] = SECOND
    r.mock.set_sheds(sheds)
    c.sheds_refresh()
    c.egress_show(server="mock", shed="hello-world")
    c.wait_until(lambda: _egress(c).get("created_at") == SECOND and _egress(c).get("blocks") == [],
                 timeout=20, what="the new incarnation's detail, with no blocks")
    assert "No blocked connections" in (_egress(c).get("note") or ""), _egress(c)
    assert c.egress_recent("mock", "hello-world", SECOND, blocks=True) == []
    r.mock.push_egress(_decision("hello-world", SECOND, "new.example.test", "deny"))
    c.wait_until(lambda: [b["target"] for b in _egress(c).get("blocks") or []] ==
                 ["new.example.test:443"], timeout=20, what="the new incarnation's block")


def test_a_shed_without_a_delivery_record_opens_its_egress_detail(r: Rig):
    """api-box (on mock2, no delivery record) has a details toggle instead of a
    delivery badge; its detail shows its egress alone: no profile assigned."""
    c = r.c
    c.egress_show(server="mock2", shed="api-box")
    c.wait_until(lambda: _egress(c).get("shed") == "api-box"
                 and _egress(c).get("profiles") == "Egress profile: none (egress is not restricted)",
                 timeout=20, what="api-box's egress detail")
    assert c.call("shed_delivery.dump").get("detail") is None
    rows = {(x["host"], x["name"]): x for x in c.dashboard_dump()["rows"]}
    assert rows[("mock2", "api-box")]["delivery_badge"] is None


def test_a_shed_on_the_server_default_shows_the_default_profile(r: Rig):
    """A shed created without `--egress` on a server whose `egress.default` is
    `[default]` has no explicit selection in its document, yet runs behind the
    default profile: its detail shows that profile, marked as the server's default,
    never "not restricted". The same shed on a server with egress control off is
    unrestricted, and says why."""
    c = r.c
    with r.mock._lock:
        r.mock.egress_default = ["default"]
    inherits = {"name": "inherits", "status": "running", "backend": "firecracker",
                "created_at": "2026-11-22T08:00:00Z", "egress_port": 20003}
    r.mock.set_sheds([dict(s) for s in MOCK_SHEDS] + [inherits])
    c.sheds_refresh()
    assert "egress_profiles" not in (r.mock.shed("inherits") or {})
    c.egress_show(server="mock", shed="inherits")
    c.wait_until(lambda: _egress(c).get("shed") == "inherits"
                 and _egress(c).get("profiles")
                 == "Egress profile: default (audit), the server's default",
                 timeout=20, what="the inherited default profile")
    assert "not restricted" not in _egress(c)["rendered"], _egress(c)
    _scroll(c, "shed:mock/inherits")
    _shot(c, "b4-shed-detail-server-default-egress.png")

    with r.mock._lock:
        r.mock.egress_enabled = False
    c.call("ui.close_shed_delivery")
    c.wait_until(lambda: not _egress(c), timeout=10, what="the detail closed")
    c.egress_show(server="mock", shed="inherits")
    c.wait_until(lambda: _egress(c).get("profiles")
                 == "Egress profile: none (egress control is off on this server)",
                 timeout=20, what="egress control off on the server")


# ---------------------------------------------------------------------------
# System
# ---------------------------------------------------------------------------

def test_system_shows_every_servers_egress_profiles(r: Rig):
    """The System pane lists each server's egress profiles, and the server it could
    not reach as an error row."""
    c = r.c
    _go(c, "system")
    c.wait_until(lambda: len((c.egress_dump() or {}).get("profiles") or []) == 4,
                 timeout=20, what="both servers' profiles")
    d = c.egress_dump()
    assert {(p["host"], p["name"]) for p in d["profiles"]} == {
        ("mock", "default"), ("mock", "custom"), ("mock2", "default"), ("mock2", "custom")}, d
    assert [e["host"] for e in d["errors"]] == ["down"], d["errors"]
    rules = {(p["host"], p["name"]): p["rule"] for p in d["profiles"]}
    assert rules[("mock2", "custom")] == 'request.host.endsWith(".example.test")', rules
    assert rules[("mock", "default")] is None
    _scroll(c, "system-egress")
    _shot(c, "b4-system-profiles.png")
