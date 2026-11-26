"""The provisioning UI, driven through the real app (plan 039 A7; plan 040 §3.9).

The Credentials view (what each provider delivers, set up, edited, turned off and on
or removed, its scope and its form written to `delivery.toml`; who answers on each
server; Check, of a shed or of a server, backend and repo; the recent deliveries) and
each shed row's delivery badge and detail (items, last outcome, problems, Deliver
again). Every cell drives the app's own controls through its `ui.*` doors and reads
back what was RENDERED (`credentials.dump`, `shed_delivery.dump`, `dashboard.dump`'s
`delivery_badge`), never the backend's view. The cells that drove Preferences'
Provisioning, Source machine and Preview sections before plan 040 drive the
Credentials view now.

The module launches its OWN app over a throwaway HOME that holds every input the five
providers read (the A6 module's setup: a throwaway key, a fake `gh`, craze and env
files, an envsecrets config), against a PRIVATE mock whose sheds carry one delivery
record of each kind. Nothing a provider reads may appear in anything rendered.

`SHED_DELIVERY_SHOTS=<dir>` keeps the screenshots (Linux; the capture is TCC-gated on
macOS, where the cells still run and only the capture is skipped).
"""

from __future__ import annotations

import json
import os
import platform
import shutil
import subprocess
import tempfile
import tomllib
from dataclasses import dataclass
from pathlib import Path

import pytest

import hermetic
import ui
from client import ShedError, TauriClient, Timeout
from mockserver import DELIVERED_RECORD, MockShedServer
from test_tauri_delivery import (
    CONFIG, CRAZE_KEY, ENV_KEY, GCS, GH_TOKEN, NPMRC, PASSPHRASE, PROJECT_ENV, QUOTED,
    SENTINELS, SERVER, _script, _settings, _write,
)

PNG_MAGIC = b"\x89PNG\r\n\x1a\n"
SHOTS = os.environ.get("SHED_DELIVERY_SHOTS")

#: A partial delivery: one item failed to write, a problem reported, an item
#: expired, the install step owed.
PARTIAL_RECORD = {
    "state": "partial",
    "steps": {"stage1": {"state": "done"}, "clone": {"state": "done"},
              "stage2": {"state": "failed", "reason": "write_failed"},
              "install": {"state": "owed"}},
    "last": {"at": "2026-11-23T10:00:00Z", "reason": "provision", "outcome": "accepted",
             "failure": {"reason": "write_failed", "item": "project-env", "result": "target_is_link"},
             "source": {"machine_id": "5f0c6e2a9b8d4c1e7a3f2b6d8e9c0a1b",
                        "machine_name": "ada-laptop"},
             "problems": [{"provider": "envsecrets-login", "code": "missing_setting",
                           "message": "the envsecrets config names no passphrase_env"}]},
    "items": [
        {"name": "ssh-key", "provider": "ssh-key", "stage": 1, "kind": "file", "base": "home",
         "target": ".ssh/id_ed25519", "fingerprint": "k3y-fp_01",
         "written_at": "2026-11-23T10:00:02Z", "result": "ok"},
        {"name": "github", "provider": "github", "stage": 1, "kind": "file", "base": "home",
         "target": ".config/gh/hosts.yml", "fingerprint": "gh-fp-1",
         "expires_at": "2026-11-22T10:00:00Z", "written_at": "2026-11-21T10:00:02Z",
         "result": "ok"},
        {"name": "example-api-key", "provider": "api-keys", "stage": 1, "kind": "env",
         "target": "EXAMPLE_API_KEY", "fingerprint": "api-fp-1",
         "written_at": "2026-11-23T10:00:02Z", "result": "ok"},
        {"name": "project-env", "provider": "files", "stage": 2, "kind": "file",
         "base": "project", "target": ".envrc", "fingerprint": "proj-fp-1",
         "result": "target_is_link"},
    ],
    "expired": ["github"],
    "owed": ["install"],
}
NOT_DELIVERED_RECORD = {
    "state": "not_delivered",
    "steps": {"stage1": {"state": "failed", "reason": "no_source"},
              "clone": {"state": "failed", "reason": "no_source"},
              "stage2": {"state": "owed"}, "install": {"state": "owed"}},
    "last": {"at": "2026-11-23T09:00:00Z", "reason": "create", "outcome": "no_source",
             "failure": {"reason": "no_source"}},
    "owed": ["stage1", "clone", "stage2", "install"],
}

SHEDS = [
    {"name": "widgets", "id": "w1dg3tsw1dg3tsw1dg3tsw1dg3", "status": "running", "backend": "vz",
     "repo": "git@git.example.test:example/widgets.git", "landing_dir": "/home/shed/widgets",
     "delivery": PARTIAL_RECORD,
     "cpus": 2, "memory_mb": 4096},
    {"name": "folio", "status": "running", "backend": "firecracker", "delivery": DELIVERED_RECORD},
    {"name": "atlas", "status": "stopped", "backend": "vz", "delivery": NOT_DELIVERED_RECORD},
    {"name": "legacy", "status": "running", "backend": "vz", "delivery": {"state": "none"}},
    {"name": "service", "status": "running", "backend": "remote",
     "delivery": {"state": "not_applicable"}},
    # An admin looking at another user's shed is sent the state alone, marked so; the
    # state alone ("none") cannot tell it from the user's own shed.
    {"name": "theirs", "status": "running", "backend": "vz",
     "delivery": {"state": "partial", "withheld": True}},
    {"name": "theirs-new", "status": "running", "backend": "vz",
     "delivery": {"state": "none", "withheld": True}},
    # A server older than delivery sends no record.
    {"name": "older", "status": "running", "backend": "vz"},
]


@dataclass
class Rig:
    client: TauriClient
    mock: MockShedServer
    home: Path
    settings: Path
    key_lines: list[str]


@pytest.fixture(scope="module")
def rig():
    cfg = ui._SUBPROC["tauri"]
    if not cfg.binary.exists():
        raise RuntimeError(f"tauri binary not found at {cfg.binary}; build it first (make tauri-build).")
    if shutil.which("ssh-keygen") is None:
        pytest.skip("ssh-keygen is needed for the throwaway key")
    mock = MockShedServer()
    mock.set_sheds([dict(s) for s in SHEDS])
    # The source rows show: another machine is the bound source.
    mock.arm_source()
    mock.start()
    home = Path(tempfile.mkdtemp(prefix="shed-prov-"))
    proc = None
    try:
        hermetic.generate_key(home / ".ssh")
        # Each run is logged, so a cell can tell whether a Check ran it.
        _script(home / "bin" / "gh", f"""echo "$1" >> '{home / "gh-runs.log"}'
case "$1 $2" in
  "auth token") echo {GH_TOKEN} ;;
  "api user") echo octo-test ;;
  *) exit 1 ;;
esac""")
        _write(home / ".craze" / "native" / "providers.toml", f'[anthropic]\napi_key = "{CRAZE_KEY}"\n')
        _write(home / ".config" / "shed" / "keys.env", f"EXAMPLE_API_KEY={ENV_KEY}\nQUOTED_VALUE={QUOTED}\n")
        _write(home / ".envsecrets" / "config.yaml",
               f"passphrase_env: ENVSECRETS_PASSPHRASE\ngcs_credentials: {GCS}\nbucket: example\n")
        _write(home / ".config" / "envsecrets" / "passphrase", PASSPHRASE + "\n")
        _write(home / ".npmrc", NPMRC)
        _write(home / "work" / "widgets.env", PROJECT_ENV)
        settings = _write(home / ".config" / "shed" / "delivery.toml", _settings(home))
        key_text = (home / ".ssh" / "id_ed25519").read_text()
        key_lines = [l for l in key_text.splitlines() if l and not l.startswith("-----")]

        env = ui.subproc_env(cfg, runtime_dir=home, mock_base_url=mock.base_url,
                             config_path=CONFIG, delivery_config=settings)
        sock = home / cfg.sock_rel
        log = home / "provisioning-ui.log"
        log_fh = open(log, "wb")
        proc = subprocess.Popen([str(cfg.binary)], env=env, stdout=log_fh, stderr=subprocess.STDOUT)
        ui.await_hermetic("tauri", sock=sock, mock_base_url=mock.base_url, proc=proc, log=log)
        client = TauriClient(sock)
        client.wait_until(lambda: client.call("ui.current_pane")["pane"] is not None,
                          timeout=30, what="the frontend ready")
        client.call("ui.set_appearance", {"mode": "light"})
        try:
            yield Rig(client, mock, home, settings, key_lines)
        finally:
            client.close()
            ui.terminate(proc)
            log_fh.close()
    finally:
        mock.stop()
        shutil.rmtree(home, ignore_errors=True)


@pytest.fixture
def r(rig):
    """Each cell starts from the full settings and the fixture's sheds."""
    _write(rig.settings, _settings(rig.home))
    rig.mock.set_sheds([dict(s) for s in SHEDS])
    rig.mock.provision_record = DELIVERED_RECORD
    rig.mock.provision_warning = None
    rig.mock.provision_should_fail = False
    rig.mock.shed_doc_refusal = None
    rig.mock.release_provisions()
    yield rig
    rig.mock.shed_doc_refusal = None
    rig.mock.release_provisions()


def _shot(c: TauriClient, name: str, anchor: str | None = None) -> None:
    if platform.system() == "Darwin":
        return
    if anchor:
        token = c.call("ui.scroll_to", {"anchor": anchor})["token"]
        c.wait_until(lambda: (c.call("ui.scrolled")["scrolled"] or {}).get("token") == token,
                     timeout=10, what=f"scrolled to {anchor}")
    png, w, h = c.screenshot(scale=1)
    assert png[:8] == PNG_MAGIC and w > 0 and h > 0
    if SHOTS:
        out = Path(SHOTS).expanduser()
        out.mkdir(parents=True, exist_ok=True)
        (out / name).write_bytes(png)


def _no_value(text: str, key_lines: list[str]) -> None:
    for s in SENTINELS:
        assert s not in text, s
    for line in key_lines:
        assert line not in text, "a line of the private key"


# -- the shed rows and their detail ------------------------------------------

def _rows(c: TauriClient) -> dict[str, dict]:
    return {row["name"]: row for row in c.call("dashboard.dump")["rows"]}


def _detail(c: TauriClient) -> dict | None:
    return c.call("shed_delivery.dump")["detail"]


def _open(c: TauriClient, shed: str) -> dict:
    c.call("ui.show_shed_delivery", {"server": SERVER, "shed": shed})
    c.wait_until(lambda: (_detail(c) or {}).get("shed") == shed, timeout=15,
                 what=f"{shed}'s delivery detail open")
    return _detail(c)


def test_each_shed_row_shows_its_delivery_state(r):
    c = r.client
    c.call("ui.close_shed_delivery")
    c.navigate("sheds")
    c.wait_until(lambda: len([x for x in _rows(c).values() if x.get("status_text")]) == len(SHEDS),
                 timeout=20, what="every shed's card rendered")
    badges = {name: row["delivery_badge"] for name, row in _rows(c).items()}
    assert badges["folio"] == "delivered", badges
    assert badges["widgets"].startswith("partial") and "1 expired" in badges["widgets"] \
        and "owed: install" in badges["widgets"], badges
    assert badges["atlas"].startswith("not delivered") \
        and "owed: stage1, clone, stage2, install" in badges["atlas"], badges
    assert badges["legacy"] == "no delivery"
    assert badges["service"] == "not applicable"
    assert badges["theirs"] == "partial"
    assert badges["theirs-new"] == "no delivery"
    assert badges["older"] is None, "a server older than delivery shows no badge"
    rows = _rows(c)
    assert rows["widgets"]["delivery"]["state"] == "partial"
    assert rows["older"]["delivery"] is None


def test_the_detail_lists_items_and_problems(r):
    c = r.client
    d = _open(c, "widgets")
    assert d["server"] == SERVER and d["state"] == "partial" and d["withheld"] is False
    items = {i["name"]: i for i in d["items"]}
    assert list(items) == ["ssh-key", "github", "example-api-key", "project-env"]
    assert items["ssh-key"]["target"] == ".ssh/id_ed25519" and items["ssh-key"]["result"] == "ok"
    assert items["ssh-key"]["provider"] == "ssh-key" and items["ssh-key"]["stage"] == "1"
    assert items["example-api-key"]["target"] == "EXAMPLE_API_KEY"
    assert items["project-env"]["result"] == "target_is_link" and items["project-env"]["stage"] == "2"
    assert items["github"]["expires"].startswith("expired")
    assert items["ssh-key"]["written"], "the written time is shown"
    assert d["problems"] == ["envsecrets-login: the envsecrets config names no passphrase_env "
                             "(missing_setting)"]
    assert "provision · accepted" in d["last"] and "from ada-laptop" in d["last"]
    assert "write_failed" in d["failure"] and "project-env" in d["failure"]
    steps = {s["name"]: s["text"] for s in d["steps"]}
    assert steps == {"stage1": "stage1 done", "clone": "clone done",
                     "stage2": "stage2 failed (write_failed)", "install": "install owed"}
    assert d["deliver_again"] == {"disabled": False, "text": "Deliver again"}
    _no_value(d["rendered"], r.key_lines)
    _shot(c, "shed-detail-partial.png")
    # The badge closes it, as it opened it.
    c.call("ui.close_shed_delivery")
    c.wait_until(lambda: _detail(c) is None, timeout=10, what="the detail closed")


def test_deliver_again_calls_provision_and_updates_the_state(r):
    c = r.client
    before = list(r.mock.provisions)
    # A stopped shed's button is refused, as a pointer's is.
    atlas = _open(c, "atlas")
    assert atlas["deliver_again"]["disabled"] is True
    c.call("ui.press_deliver_again", {"server": SERVER, "shed": "atlas"})
    _open(c, "widgets")
    r.mock.provision_warning = "delivery: github: the token expires within a day"
    c.call("ui.press_deliver_again", {"server": SERVER, "shed": "widgets"})
    c.wait_until(lambda: (_detail(c) or {}).get("result") == "Delivered again: delivered.",
                 timeout=20, what="the provision's result shown")
    assert r.mock.provisions[len(before):] == ["widgets"], "atlas's press made no call"
    d = _detail(c)
    assert d["warnings"] == ["delivery: github: the token expires within a day"]
    assert d["progress"][:1] == ["stage1: delivered"]
    # The row reads the record the provision ended with.
    c.wait_until(lambda: _rows(c)["widgets"]["delivery_badge"] == "delivered", timeout=20,
                 what="the row's badge updated")
    c.wait_until(lambda: (_detail(c) or {}).get("state") == "delivered", timeout=10,
                 what="the detail updated")
    # A refused provision says so, in the server's words.
    r.mock.provision_should_fail = True
    c.call("ui.press_deliver_again", {"server": SERVER, "shed": "widgets"})
    c.wait_until(lambda: (_detail(c) or {}).get("result", "").startswith("Could not deliver again"),
                 timeout=20, what="the refusal shown")
    assert "could not provision widgets" in _detail(c)["result"]
    c.call("ui.close_shed_delivery")


def test_a_delivery_in_flight_survives_closing_the_detail(r):
    """The run is held by the app, not the detail: closing it, leaving the pane and
    coming back cannot start a second request, and the first one's outcome is shown."""
    c = r.client
    before = len(r.mock.provisions)
    r.mock.hold_provisions()
    _open(c, "widgets")
    c.call("ui.press_deliver_again", {"server": SERVER, "shed": "widgets"})
    c.wait_until(lambda: len(r.mock.provisions) == before + 1, timeout=20,
                 what="the provision in flight")
    c.call("ui.close_shed_delivery")
    c.wait_until(lambda: _detail(c) is None, timeout=10, what="the detail closed")
    d = _open(c, "widgets")
    c.wait_until(lambda: (_detail(c) or {}).get("deliver_again") == {
        "disabled": True, "text": "Delivering…"}, timeout=10, what="the reopened detail in flight")
    c.call("ui.press_deliver_again", {"server": SERVER, "shed": "widgets"})
    # Away and back: the pane unmounts every detail.
    c.navigate("agents")
    c.wait_until(lambda: _detail(c) is None, timeout=10, what="the detail gone with the pane")
    c.navigate("sheds")
    d = _open(c, "widgets")
    assert d["deliver_again"] == {"disabled": True, "text": "Delivering…"}, d
    c.call("ui.press_deliver_again", {"server": SERVER, "shed": "widgets"})
    r.mock.release_provisions()
    c.wait_until(lambda: (_detail(c) or {}).get("result") == "Delivered again: delivered.",
                 timeout=20, what="the first run's outcome shown")
    assert len(r.mock.provisions) == before + 1, r.mock.provisions[before:]
    # Closed and opened again, the outcome is still there.
    c.call("ui.close_shed_delivery")
    c.wait_until(lambda: _detail(c) is None, timeout=10, what="closed")
    assert _open(c, "widgets")["result"] == "Delivered again: delivered."
    c.call("ui.close_shed_delivery")


def test_an_admin_sees_the_state_alone(r):
    c = r.client
    for shed, state in (("theirs", "partial"), ("theirs-new", "none")):
        d = _open(c, shed)
        assert d["state"] == state and d["withheld"] is True, d
        assert d["summary"] == "Only the shed's user can see what was delivered."
        assert d["items"] == [] and d["problems"] == [] and d["steps"] == []
        assert d["last"] is None and d["deliver_again"] is None, d
        c.call("ui.press_deliver_again", {"server": SERVER, "shed": shed})
    # The user's own state-only record ("none": a shed from before delivery) is not
    # withheld, and offers Deliver again.
    d = _open(c, "legacy")
    assert d["withheld"] is False and d["deliver_again"] == {"disabled": False, "text": "Deliver again"}
    assert "theirs" not in r.mock.provisions and "theirs-new" not in r.mock.provisions
    c.call("ui.close_shed_delivery")


def test_ui_doors_refuse_bad_params(r):
    c = r.client
    for op, params in (("ui.show_shed_delivery", {"server": SERVER}),
                       ("ui.show_shed_delivery", {"server": SERVER, "shed": "a/b"}),
                       ("ui.type_delivery_field", {"provider": "github", "field": "gh"}),
                       ("ui.type_delivery_field", {"provider": "github", "field": "gh", "value": 3}),
                       ("ui.type_delivery_field", {"provider": "nope", "field": "gh", "value": "x"}),
                       ("ui.press_delivery_button", {"provider": "github"}),
                       ("ui.fill_check", {"field": "nope", "value": "x"}),
                       ("ui.fill_check", {"field": "repo"}),
                       ("ui.press_check", {"button": "save"}),
                       ("ui.press_source_unbind", {}),
                       ("ui.navigate", {"pane": "provisioning"})):
        with pytest.raises(ShedError) as e:
            c.call(op, params)
        assert e.value.code == "bad_request", (op, params)
    # The switch, the Preferences preview and its section scroll went with the
    # Provisioning pane.
    for op in ("ui.press_delivery_toggle", "ui.preview_delivery", "ui.prefs_section",
               "ui.run_check"):
        with pytest.raises(ShedError) as e:
            c.call(op, {"provider": "github", "server": SERVER, "section": "terminal"})
        assert e.value.code == "unknown_op", op


# -- the Credentials view: What ------------------------------------------------

def _cred(c: TauriClient) -> dict:
    return c.credentials_dump() or {}


def _what(c: TauriClient) -> dict:
    return _cred(c).get("what") or {}


def _provider(c: TauriClient, name: str) -> dict:
    return next((p for p in _what(c).get("providers", []) if p["name"] == name), {})


def _view(c: TauriClient) -> None:
    """Show the Credentials view (each navigation reads everything again) and wait for
    its five providers."""
    c.navigate("credentials")
    c.wait_until(lambda: len(_what(c).get("providers", [])) == 5 and _cred(c).get("doors_ready")
                 and ((_cred(c).get("where") or {}).get("rows") or []), timeout=20,
                 what="the Credentials view's five providers and its Where rows")


def _editor(c: TauriClient) -> dict:
    return _what(c).get("editor") or {}


def _values(c: TauriClient) -> dict:
    return _editor(c).get("values") or {}


def _press(c: TauriClient, provider: str, button: str) -> None:
    c.call("ui.press_delivery_button", {"provider": provider, "button": button})


def _type(c: TauriClient, provider: str, field: str, value: str, index: int | None = None) -> None:
    c.call("ui.type_delivery_field", {"provider": provider, "field": field, "index": index,
                                      "value": value})
    key = field if index is None else f"{field}[{index}]"
    c.wait_until(lambda: _values(c).get(key) in (value, "redacted"), timeout=10,
                 what=f"{key} typed into the form")


def _tick(c: TauriClient, provider: str, button: str, on: bool = True) -> None:
    _press(c, provider, button)
    c.wait_until(lambda: _values(c).get(button) is on, timeout=10, what=f"{button} {on}")


def _edit(c: TauriClient, provider: str, action: str = "edit") -> dict:
    _press(c, provider, action)
    c.wait_until(lambda: _editor(c).get("provider") == provider, timeout=15,
                 what=f"{provider}'s form open")
    return _values(c)


def _save(c: TauriClient, provider: str) -> None:
    _press(c, provider, "save")
    c.wait_until(lambda: not _provider(c, provider).get("editing") or _provider(c, provider).get("error"),
                 timeout=15, what=f"{provider} saved")
    assert not _provider(c, provider).get("error"), _provider(c, provider)


def _stored(r: Rig) -> dict:
    return tomllib.loads(r.settings.read_text())


def _fresh(c: TauriClient, pred, what: str) -> None:
    """Show the view again (it reads the file again) and wait until its rows show it."""
    _view(c)
    c.wait_until(lambda: pred(_what(c)), timeout=15, what=what)


def _cfg(p: dict, name: str) -> dict:
    return next((x for x in p.get("providers", []) if x["name"] == name), {}).get("config") or {}


def test_turning_a_provider_off_and_on_writes_delivery_toml(r):
    c = r.client
    _fresh(c, lambda w: _cfg(w, "github").get("enabled") is True, "github on")
    gh = _provider(c, "github")
    assert gh["state"] == "on" and gh["state_text"] == "On", gh
    assert gh["actions"] == ["edit", "turn_off", "remove"], gh
    assert gh["title"] == "GitHub" and gh["scope"] == "On every server", gh
    assert gh["lands"].startswith("~/.config/gh/hosts.yml"), gh
    _press(c, "github", "turn_off")
    c.wait_until(lambda: _provider(c, "github").get("enabled") is False, timeout=15,
                 what="github off")
    text = r.settings.read_text()
    assert "enabled = false" in text.split("[github]", 1)[1].split("\n[", 1)[0], text
    assert _provider(c, "github")["actions"] == ["turn_on", "edit", "remove"]
    view = c.call("delivery.settings")
    assert next(p for p in view["providers"] if p["name"] == "github")["enabled"] is False
    for name in ("ssh-key", "api-keys", "envsecrets-login", "files"):
        assert _provider(c, name)["enabled"] is True, name
    _press(c, "github", "turn_on")
    c.wait_until(lambda: _provider(c, "github").get("enabled") is True, timeout=15,
                 what="github on again")
    assert _stored(r)["github"]["gh"] == str(r.home / "bin" / "gh"), "turning on keeps the table"
    _no_value(json.dumps(c.credentials_dump()), r.key_lines)


def test_the_ssh_key_form_writes_its_fields_and_scope(r):
    c = r.client
    _fresh(c, lambda w: _cfg(w, "ssh-key").get("enabled") is True and "only" not in _cfg(w, "ssh-key"),
           "the settings read")
    v = _edit(c, "ssh-key")
    assert v["key_file"] == "~/.ssh/id_ed25519" and v["toggle:sign_commits"] is False, v
    assert v["scope:every"] is True and v["scope:only"] is False, v
    assert {b: v[f"backend:{b}"] for b in ("vz", "firecracker", "remote")} == \
        {"vz": False, "firecracker": False, "remote": False}, v
    _type(c, "ssh-key", "key_file", "~/.ssh/id_work,1")
    _press(c, "ssh-key", "toggle:sign_commits")
    c.wait_until(lambda: _values(c).get("toggle:sign_commits") is True, timeout=10, what="the switch on")
    _tick(c, "ssh-key", "backend:vz")
    _press(c, "ssh-key", "add:only.repos")
    _type(c, "ssh-key", "only.repos", "example/*", 0)
    _save(c, "ssh-key")
    assert _provider(c, "ssh-key")["scope"] == "On every server · VZ sheds only · repos matching example/*"
    ssh = _stored(r)["ssh-key"]
    assert ssh["key_file"] == "~/.ssh/id_work,1" and ssh["sign_commits"] is True, ssh
    assert ssh["only"] == {"backends": ["vz"], "repos": ["example/*"]} and ssh["enabled"] is True
    # A change the schema refuses stays under the row, in its fixed words, and the
    # file is left as it was; corrected, it saves, and the refusal goes.
    before = r.settings.read_text()
    v = _edit(c, "ssh-key")
    assert v["only.repos[0]"] == "example/*" and v["backend:vz"] is True, v
    _type(c, "ssh-key", "only.repos", "widgets", 0)
    _press(c, "ssh-key", "save")
    c.wait_until(lambda: _provider(c, "ssh-key").get("error"), timeout=15, what="the refusal shown")
    err = _provider(c, "ssh-key")["error"]
    assert "ssh-key.only.repos" in err and "expected" in err and "widgets" not in err, err
    assert r.settings.read_text() == before
    _type(c, "ssh-key", "only.repos", "example/*", 0)
    _tick(c, "ssh-key", "backend:remote")
    _save(c, "ssh-key")
    assert not _provider(c, "ssh-key")["error"]
    assert _stored(r)["ssh-key"]["only"]["backends"] == ["vz", "remote"]


def test_scope_ticks_write_only_servers_exactly(r):
    """Criterion 16: "Every server this machine answers on" writes no `only.servers`;
    "Only these" writes exactly the ticked servers. A server the settings already name
    that is not configured is a tick of its own, so it is never dropped unseen."""
    c = r.client
    _fresh(c, lambda w: _cfg(w, "github").get("enabled") is True, "github on")
    v = _edit(c, "github")
    assert v["scope:every"] is True and "server:mock" not in v, v
    _press(c, "github", "scope:only")
    c.wait_until(lambda: _values(c).get("scope:only") is True, timeout=10, what="Only these")
    assert _values(c)["server:mock"] is False
    assert _editor(c)["buttons"]["save"] is False, "only these, with nothing ticked"
    assert "Tick at least one server, or choose every server." in _editor(c)["missing"]
    _tick(c, "github", "server:mock")
    _save(c, "github")
    assert _stored(r)["github"]["only"] == {"servers": ["mock"]}
    assert _provider(c, "github")["scope"] == "Only on mock"
    # Every server again: no `only` at all.
    _edit(c, "github")
    _press(c, "github", "scope:every")
    c.wait_until(lambda: _values(c).get("scope:every") is True, timeout=10, what="every server")
    _save(c, "github")
    assert "only" not in _stored(r)["github"], _stored(r)["github"]
    assert _provider(c, "github")["scope"] == "On every server"
    # A server named in the file but not configured keeps its tick, ticked.
    _write(r.settings, _settings(r.home, github_only='only = { servers = ["lab"] }'))
    _fresh(c, lambda w: (_cfg(w, "github").get("only") or {}).get("servers") == ["lab"], "only lab")
    v = _edit(c, "github")
    assert (v["scope:only"], v["server:mock"], v["server:lab"]) == (True, False, True), v
    _tick(c, "github", "server:mock")
    _save(c, "github")
    assert _stored(r)["github"]["only"] == {"servers": ["mock", "lab"]}


DISCARDED = ("The settings changed while you were editing; your unsaved changes were "
             "discarded.")


def test_a_save_over_a_change_made_elsewhere_is_refused_unwritten(r):
    """Save is a compare-and-swap: the provider's table as the form read it goes with
    the change (`base`), and a table changed elsewhere while the form was open is
    refused `conflict` with nothing written. The view discards the form, says so, and
    shows the settings as they are now. Over IPC: a stale `base` is `conflict` and
    writes nothing; the current one writes; no `base` writes as before."""
    c = r.client
    # The fixture's table (no `only`), so the form is not opened on the last cell's.
    _fresh(c, lambda w: _cfg(w, "github").get("enabled") is True and "only" not in _cfg(w, "github"),
           "github on, on every server")
    _edit(c, "github")
    _press(c, "github", "scope:only")
    c.wait_until(lambda: _values(c).get("scope:only") is True, timeout=10, what="Only these")
    _tick(c, "github", "server:mock")
    # Changed by hand while the form is open (the view does not read it again).
    _write(r.settings, _settings(r.home, github_only='only = { servers = ["lab"] }'))
    elsewhere = r.settings.read_text()
    _press(c, "github", "save")
    c.wait_until(lambda: _provider(c, "github").get("notice"), timeout=15,
                 what="the discard notice")
    gh = _provider(c, "github")
    assert gh["notice"] == DISCARDED and not gh["editing"] and not gh["error"], gh
    assert r.settings.read_text() == elsewhere, "nothing written over the change"
    c.wait_until(lambda: (_cfg(_what(c), "github").get("only") or {}).get("servers") == ["lab"],
                 timeout=15, what="the settings as they are now")

    # The same rule over IPC.
    now = _cfg(c.call("delivery.settings"), "github")
    stale = {**now, "only": {"servers": ["mock"]}}
    with pytest.raises(ShedError) as e:
        c.call("delivery.set", {"provider": "github", "config": {**now, "enabled": False},
                                "base": stale})
    assert e.value.code == "conflict", e.value
    assert r.settings.read_text() == elsewhere
    with pytest.raises(ShedError) as e:
        c.call("delivery.set", {"provider": "github", "config": {**now, "enabled": False},
                                "base": None})
    assert e.value.code == "conflict", "null is a provider not set up: github is"
    out = c.call("delivery.set", {"provider": "github", "config": {**now, "enabled": False},
                                  "base": now})
    assert _cfg(out, "github")["enabled"] is False, out
    c.call("delivery.set", {"provider": "github", "config": now})  # no base: as before
    assert _cfg(c.call("delivery.settings"), "github") == now


def test_a_provider_cannot_be_turned_on_with_a_required_field_empty(r):
    """Criterion 16: there is no bare switch. A provider that is not set up offers Set
    up, whose form cannot be saved (so the provider cannot be turned on) until its
    required fields are filled; saved, it is on. One that is off and incomplete is set
    up through its form, never turned on as it is."""
    c = r.client
    _view(c)
    _press(c, "ssh-key", "remove")
    c.wait_until(lambda: _provider(c, "ssh-key").get("state") == "not_set_up", timeout=15,
                 what="ssh-key removed")
    assert "ssh-key" not in _stored(r)
    assert _provider(c, "ssh-key")["actions"] == ["setup"]
    v = _edit(c, "ssh-key", "setup")
    assert _editor(c)["mode"] == "setup" and v["key_file"] == "", v
    assert _editor(c)["buttons"]["save"] is False
    assert _editor(c)["save"] == "Save and turn on"
    assert _editor(c)["missing"] == ["Key file is required."]
    _press(c, "ssh-key", "save")
    assert "ssh-key" not in _stored(r), "a press on the disabled Save writes nothing"
    _type(c, "ssh-key", "key_file", "~/.ssh/id_ed25519")
    c.wait_until(lambda: _editor(c).get("buttons", {}).get("save") is True, timeout=10,
                 what="Save enabled once the key file is in")
    _save(c, "ssh-key")
    assert _stored(r)["ssh-key"] == {"enabled": True, "key_file": "~/.ssh/id_ed25519",
                                     "sign_commits": False}
    assert _provider(c, "ssh-key")["state"] == "on"
    # Off, and incomplete: no Turn on, only Set up.
    _write(r.settings, _settings(r.home, passphrase="enabled = false").replace(
        "[envsecrets-login]\nenabled = true\n", "[envsecrets-login]\n"))
    _fresh(c, lambda w: _cfg(w, "envsecrets-login").get("enabled") is False, "envsecrets off")
    env = _provider(c, "envsecrets-login")
    assert env["actions"] == ["setup", "remove"] and env["state_text"] == "Off · not complete", env
    assert env["missing"] == "A passphrase file or a passphrase command is required.", env
    _edit(c, "envsecrets-login", "setup")
    assert _editor(c)["buttons"]["save"] is False
    _type(c, "envsecrets-login", "passphrase_file", "~/.config/envsecrets/passphrase")
    _save(c, "envsecrets-login")
    assert _stored(r)["envsecrets-login"]["enabled"] is True


def test_list_and_files_entries_are_edited_one_by_one(r):
    """The plain lists, entry by entry; and the files form, which round-trips each
    file's mode and its own scope (criterion 16)."""
    c = r.client
    _fresh(c, lambda w: _cfg(w, "api-keys").get("env_files") == ["~/.config/shed/keys.env"],
           "the settings read")
    v = _edit(c, "api-keys")
    assert v["env_files[0]"] == "~/.config/shed/keys.env" and v["toggle:craze_providers"] is True, v
    _press(c, "api-keys", "add:env_files")
    _type(c, "api-keys", "env_files", "~/keys,work.env", 1)
    _save(c, "api-keys")
    assert _stored(r)["api-keys"]["env_files"] == ["~/.config/shed/keys.env", "~/keys,work.env"]
    _edit(c, "api-keys")
    _press(c, "api-keys", "remove:env_files:0")
    c.wait_until(lambda: "env_files[1]" not in _values(c), timeout=10, what="the entry removed")
    assert _values(c)["env_files[0]"] == "~/keys,work.env"
    _save(c, "api-keys")
    assert _stored(r)["api-keys"]["env_files"] == ["~/keys,work.env"]

    v = _edit(c, "files")
    assert v["file.from[0]"] == "~/.npmrc" and v["file.base[1]"] == "project", v
    assert v["file.mode[0]"] == "" and v["file.1.scope:every"] is True, v
    _press(c, "files", "add:file")
    _type(c, "files", "file.from", "~/notes, todo.md", 2)
    _type(c, "files", "file.to", "notes.md", 2)
    _type(c, "files", "file.base", "project", 2)
    _type(c, "files", "file.mode", "0400", 2)
    _press(c, "files", "file.2.scope:only")
    c.wait_until(lambda: _values(c).get("file.2.scope:only") is True, timeout=10, what="only these")
    _tick(c, "files", "file.2.server:mock")
    _tick(c, "files", "file.2.backend:vz")
    _press(c, "files", "add:file.2.only.repos")
    _type(c, "files", "file.2.only.repos", "example/widgets", 0)
    _save(c, "files")
    entries = _stored(r)["files"]["file"]
    assert entries[2] == {"from": "~/notes, todo.md", "to": "notes.md", "base": "project",
                          "mode": "0400", "only": {"servers": ["mock"], "backends": ["vz"],
                                                   "repos": ["example/widgets"]}}, entries
    assert entries[1]["only"] == {"repos": ["example/widgets"]}, "an entry's own rule is kept"
    assert _provider(c, "files")["lands"] == "~/.npmrc, the project's .env, the project's notes.md"
    # Read back into the form: the same mode and scope.
    v = _edit(c, "files")
    assert (v["file.mode[2]"], v["file.2.scope:only"], v["file.2.server:mock"],
            v["file.2.backend:vz"], v["file.2.only.repos[0]"]) == \
        ("0400", True, True, True, "example/widgets"), v
    _type(c, "files", "file.mode", "", 2)
    _press(c, "files", "file.2.scope:every")
    c.wait_until(lambda: _values(c).get("file.2.scope:every") is True, timeout=10, what="every")
    _save(c, "files")
    assert _stored(r)["files"]["file"][2] == {
        "from": "~/notes, todo.md", "to": "notes.md", "base": "project",
        "only": {"backends": ["vz"], "repos": ["example/widgets"]}}


def test_a_passphrase_command_is_never_shown_and_is_kept(r):
    """Criterion 16: a stored passphrase command never reaches the page or a dump."""
    c = r.client
    cmd = ["printf", "%s", "SENTINEL-CMD-0001", ""]
    _write(r.settings, _settings(r.home, passphrase=f"passphrase_command = {json.dumps(cmd)}"))
    _fresh(c, lambda w: _cfg(w, "envsecrets-login").get("passphrase_command") == "set",
           "the command shown as set")
    for shown in (json.dumps(c.call("delivery.settings")), json.dumps(c.credentials_dump())):
        assert "SENTINEL-CMD" not in shown and "printf" not in shown
    v = _edit(c, "envsecrets-login")
    assert v["passphrase_command"] == "set" and "passphrase_command[0]" not in v, v
    assert _editor(c)["buttons"]["replace:passphrase_command"] is True
    # An unrelated edit saves the command back exactly.
    _tick(c, "envsecrets-login", "backend:vz")
    _save(c, "envsecrets-login")
    stored = _stored(r)["envsecrets-login"]
    assert stored["passphrase_command"] == cmd and stored["only"] == {"backends": ["vz"]}, stored
    # Replaced: each argument as typed, never dumped.
    _edit(c, "envsecrets-login")
    _press(c, "envsecrets-login", "replace:passphrase_command")
    c.wait_until(lambda: _values(c).get("passphrase_command[0]") == "redacted", timeout=10,
                 what="the new command's first argument")
    _type(c, "envsecrets-login", "passphrase_command", "printf", 0)
    _press(c, "envsecrets-login", "add:passphrase_command")
    _type(c, "envsecrets-login", "passphrase_command", "SENTINEL-CMD-0002, b", 1)
    _press(c, "envsecrets-login", "add:passphrase_command")
    c.wait_until(lambda: (_what(c).get("draft") or {}).get("passphrase_command") == "replaced (3 arguments)",
                 timeout=10, what="the replaced command, redacted")
    for shown in (json.dumps(c.credentials_dump()), json.dumps(c.call("delivery.settings"))):
        assert "SENTINEL-CMD" not in shown
    _save(c, "envsecrets-login")
    assert _stored(r)["envsecrets-login"]["passphrase_command"] == ["printf", "SENTINEL-CMD-0002, b", ""]
    assert "SENTINEL-CMD" not in json.dumps(c.credentials_dump())
    # Check says it will run the command, and the command's words never reach it.
    note = _cred(c)["check"]["note"]
    assert note.startswith("Check runs gh and your passphrase command on this machine"), note
    # Cleared, for a passphrase file instead (the two are one or the other).
    _edit(c, "envsecrets-login")
    _press(c, "envsecrets-login", "clear:passphrase_command")
    c.wait_until(lambda: "passphrase_command" not in _values(c), timeout=10, what="the command cleared")
    _type(c, "envsecrets-login", "passphrase_file", "~/pp")
    _save(c, "envsecrets-login")
    stored = _stored(r)["envsecrets-login"]
    assert "passphrase_command" not in stored and stored["passphrase_file"] == "~/pp", stored


def test_a_draft_changed_underneath_is_discarded_and_said(r):
    """A reload that finds the provider changed elsewhere discards the open form, so
    Save can never write back an `enabled` the row no longer shows."""
    c = r.client
    _fresh(c, lambda w: _cfg(w, "github").get("enabled") is True, "github on")
    _edit(c, "github")
    gh = next(p for p in c.call("delivery.settings")["providers"] if p["name"] == "github")
    c.call("delivery.set", {"provider": "github", "config": {**gh["config"], "enabled": False}})
    c.navigate("credentials")
    c.wait_until(lambda: "settings changed" in (_provider(c, "github").get("notice") or ""),
                 timeout=15, what="the draft discarded, and said")
    p = _provider(c, "github")
    assert p["editing"] is False and p["enabled"] is False, p
    assert _stored(r)["github"]["enabled"] is False


def test_a_settings_file_error_is_shown_with_its_place(r):
    c = r.client
    _view(c)
    _write(r.settings, '[github]\nenabled = true\nbogus = "SENTINEL-SETTINGS-0001"\n')
    c.navigate("credentials")
    c.wait_until(lambda: _what(c).get("error"), timeout=15, what="the settings error shown")
    w = _what(c)
    assert w["error"].startswith("delivery.toml line 3, column 1: "), w["error"]
    assert "SENTINEL" not in json.dumps(c.credentials_dump())
    assert w["providers"] == [], "no provider row while the file does not read"
    _write(r.settings, _settings(r.home))
    _view(c)
    c.wait_until(lambda: not _what(c).get("error"), timeout=15, what="the settings read again")


# -- the Credentials view: Check -----------------------------------------------

def _check(c: TauriClient) -> dict:
    return _cred(c).get("check") or {}


def _fill(c: TauriClient, field: str, value: str) -> None:
    """Fill one of Check's own controls (the segmented choice, a picker, the repo)."""
    c.call("ui.fill_check", {"field": field, "value": value})
    c.wait_until(lambda: _check(c).get(field) == value, timeout=10,
                 what=f"Check's {field} filled with {value!r}")


def _press_check(c: TauriClient) -> dict:
    """Press the real Check button and wait for what it rendered."""
    started = _check(c).get("started") or 0
    c.wait_until(lambda: (_check(c).get("run") or {}).get("enabled"), timeout=15,
                 what="the Check button enabled")
    c.call("ui.press_check", {"button": "run"})
    # A Check started (not just a press taken): what is read next is its own outcome,
    # never the one before it.
    c.wait_until(lambda: (_check(c).get("started") or 0) > started, timeout=15,
                 what="the press started a Check")
    c.wait_until(lambda: not _check(c).get("busy")
                 and (_check(c).get("items") or _check(c).get("error") or _check(c).get("about")),
                 timeout=30, what="the check rendered")
    return _check(c)


def _run(c: TauriClient, *, shed: str | None = None, backend: str | None = None,
         repo: str = "", details: bool = False) -> dict:
    """Fill Check's controls as a person does (a shed from the list, or a server, a
    backend and a repo), press Check, and, with `details`, press Details."""
    if shed is not None:
        _fill(c, "by", "shed")
        _fill(c, "shed", f"{SERVER}/{shed}")
    else:
        _fill(c, "by", "target")
        _fill(c, "server", SERVER)
        _fill(c, "backend", backend or "vz")
        _fill(c, "repo", repo)
    pv = _press_check(c)
    if details and pv.get("items"):
        c.call("ui.press_check", {"button": "details"})
        c.wait_until(lambda: _check(c).get("details") is True
                     and "target" in (_check(c).get("items") or [{}])[0], timeout=10,
                     what="the details shown")
        pv = _check(c)
    return pv


def _gh_runs(r: Rig) -> int:
    log = r.home / "gh-runs.log"
    return len(log.read_text().splitlines()) if log.exists() else 0


def test_check_from_a_shed_renders_items_and_no_value(r):
    """Criterion 17: Check from a shed picked from the list. It says before it runs
    which commands it runs; items are in words, with paths, modes, sizes and
    fingerprints behind Details."""
    c = r.client
    _view(c)
    c.wait_until(lambda: _check(c).get("shed") == "mock/widgets", timeout=20,
                 what="the shed picker filled from the sheds list")
    note = _check(c)["note"]
    assert note.startswith("Check runs gh on this machine"), note
    pv = _run(c, shed="widgets", details=True)
    assert pv["about"] == "widgets on mock · VZ · example/widgets" and pv["error"] is None, pv
    assert pv["shed"] == "mock/widgets" and pv["by"] == "shed", pv
    items = {i["name"]: i for i in pv["items"]}
    assert items["ssh-key"]["words"] == "SSH key: ~/.ssh/id_ed25519"
    assert items["ssh-key"]["target"] == ".ssh/id_ed25519" and items["ssh-key"]["mode"] == "0600"
    assert items["ssh-key.pub"]["mode"] == "0644"
    assert items["files..env"]["stage"] == "2"
    assert items["files..env"]["words"] == "Files: the project's .env"
    assert {"EXAMPLE_API_KEY", "ENVSECRETS_PASSPHRASE", ".config/gh/hosts.yml"} <= {
        i["target"] for i in pv["items"]}
    for it in pv["items"]:
        assert len(it["fingerprint"]) == 24 and it["size"].endswith("B"), it
    _no_value(pv["rendered"], r.key_lines)
    _no_value(json.dumps(c.credentials_dump()), r.key_lines)
    _shot(c, "check-shed.png", "credentials-check")
    # Without Details: the words alone.
    pv = _run(c, shed="widgets")
    assert pv["details"] is False and "target" not in pv["items"][0], pv["items"][0]
    # A rule that leaves a provider out: it is named, and its items are gone.
    c.call("delivery.set", {"provider": "github", "config": {
        "enabled": True, "gh": str(r.home / "bin" / "gh"), "only": {"backends": ["remote"]}}})
    pv = _run(c, shed="folio")
    assert pv["left_out"] == "Left out by their scope: GitHub", pv
    assert "github" not in {i["name"] for i in pv["items"]}
    # A shed the server will not send is fixed words by code: never the server's.
    r.mock.shed_doc_refusal = (404, "SHED_NOT_FOUND", "SERVER SENTINEL-DOC")
    pv = _run(c, shed="widgets")
    assert pv["error"] == "mock has no shed by that name.", pv
    r.mock.shed_doc_refusal = (500, "INTERNAL", "SERVER SENTINEL-DOC")
    pv = _run(c, shed="widgets")
    assert pv["error"] == "mock refused to send the shed.", pv
    assert "SENTINEL-DOC" not in json.dumps(c.credentials_dump())
    with pytest.raises(ShedError) as e:
        c.call("delivery.preview", {"server": SERVER, "shed": "widgets"})
    assert "SENTINEL" not in e.value.message, e.value


def test_check_never_runs_under_a_stale_warning(r):
    """Check's warning names the commands it runs; turned on behind the view's back,
    a provider's command is not run under the old warning: the press reads the
    settings again, shows the new warning and runs nothing, and the next press runs."""
    c = r.client
    gh = {"enabled": False, "gh": str(r.home / "bin" / "gh")}
    c.call("delivery.set", {"provider": "github", "config": gh})
    _view(c)
    c.wait_until(lambda: "runs no command" in (_check(c).get("note") or ""), timeout=15,
                 what="the warning naming no command")
    c.call("delivery.set", {"provider": "github", "config": {**gh, "enabled": True}})
    before = _gh_runs(r)
    pv = _run(c, shed="widgets")
    assert pv["error"].startswith("The settings changed since this view read them"), pv
    assert pv["note"].startswith("Check runs gh on this machine"), pv
    assert _gh_runs(r) == before, "gh ran under a warning that did not name it"
    pv = _press_check(c)
    assert pv["error"] is None and "github" in {i["name"] for i in pv["items"]}, pv
    assert _gh_runs(r) > before
    # Held to the commands it was told: the preview itself refuses another list.
    with pytest.raises(ShedError) as e:
        c.call("delivery.preview", {"server": SERVER, "shed": "widgets", "runs": []})
    assert e.value.code == "settings_changed", e.value


def test_check_from_a_server_backend_and_repo(r):
    """Criterion 17: Check from a server, a backend and a repo, with no shed: each
    scope judged against a new shed of that kind."""
    c = r.client
    _view(c)
    pv = _run(c, backend="remote", repo="example/widgets", details=True)
    assert pv["about"] == "A new Cloud shed on mock with example/widgets", pv
    assert (pv["by"], pv["server"], pv["backend"], pv["repo"]) == \
        ("target", SERVER, "remote", "example/widgets"), pv
    items = {i["name"]: i for i in pv["items"]}
    assert items["files..env"]["stage"] == "2", "a repo-scoped project file is in"
    assert {"ssh-key", "github", "files..npmrc"} <= set(items)
    _no_value(json.dumps(c.credentials_dump()), r.key_lines)
    _shot(c, "check-target.png", "credentials-check")
    # Switching to "A shed" clears the result: it was about another Check.
    _fill(c, "by", "shed")
    c.wait_until(lambda: not _check(c).get("items") and _check(c).get("about") is None
                 and _check(c).get("error") is None and _check(c).get("details") is False,
                 timeout=10, what="the result cleared on the switch")
    # No repo: the repo-scoped file is left out.
    pv = _run(c, backend="vz")
    assert pv["about"] == "A new VZ shed on mock with no repo", pv
    assert "files..env" not in {i["name"] for i in pv["items"]}
    # A repo that is not owner/name is refused before anything runs.
    pv = _run(c, backend="vz", repo="widgets")
    assert pv["error"] == "A repo is owner/name.", pv


def test_the_credentials_view_in_light_and_dark(r):
    c = r.client
    _view(c)
    c.wait_until(lambda: any(row["host"] == SERVER for row in (_cred(c).get("where") or {}).get("rows", [])),
                 timeout=30, what="the Where row")
    row = next(x for x in _cred(c)["where"]["rows"] if x["host"] == SERVER)
    assert row["kind"] == "other_machine" and row["source_name"] == "desk", row
    assert row["action"]["label"] == "Answer from here", row
    for mode in ("light", "dark"):
        c.call("ui.set_appearance", {"mode": mode})
        c.wait_until(lambda: (c.computed_style() or {}).get("mode") == mode, timeout=10,
                     what=f"the dashboard in {mode}")
        _shot(c, f"credentials-{mode}.png", "credentials")
        _shot(c, f"what-{mode}.png", "credentials-what")
        _edit(c, "files")
        _shot(c, f"what-files-form-{mode}.png", "credentials-form")
        _press(c, "files", "cancel")
        c.wait_until(lambda: not _editor(c), timeout=10, what="the form closed")
    c.call("ui.set_appearance", {"mode": "light"})


# -- the Credentials view: Recent deliveries ----------------------------------

def test_recent_deliveries_list_the_answers_with_no_value(r):
    """Criterion 17: the answers this machine gave, newest first: when, the server,
    the shed, the items by name and what became of each. No value. (Last: it makes
    this machine the source.)"""
    c = r.client
    _view(c)
    assert _cred(c)["recent"]["empty"] == "This machine has answered no request since the app started."
    c.source_use(SERVER)
    c.wait_until(lambda: next((s for s in c.call("delivery.status")["servers"]
                               if s["server"] == SERVER), {}).get("state") == "connected",
                 timeout=30, what="the stream connected")
    first = r.mock.push_delivery_request(shed="widgets", repo="example/widgets", backend="vz")
    r.mock.await_delivery_answer(first["id"], timeout=30)
    second = r.mock.push_delivery_request(shed="folio", backend="firecracker")
    r.mock.await_delivery_answer(second["id"], timeout=30)

    def rows() -> list[dict]:
        return (_cred(c).get("recent") or {}).get("rows") or []

    c.wait_until(lambda: (c.navigate("credentials"), len(rows()) >= 2
                          and rows()[0].get("outcome") == "delivered")[1],
                 timeout=30, what="the two answers listed")
    top, nxt = rows()[0], rows()[1]
    assert (top["key"], nxt["key"]) == (second["id"], first["id"]), rows()
    assert (top["shed"], top["server"], top["reason"]) == ("folio", SERVER, "create"), top
    assert top["when"].endswith(" UTC"), top
    assert top["items"].split(":")[0].endswith("items"), top
    assert "ssh-key" in top["items"] and "files..npmrc" in top["items"], top
    assert "files..env" in nxt["items"], nxt
    _no_value(json.dumps(c.credentials_dump()), r.key_lines)
    where = next(x for x in _cred(c)["where"]["rows"] if x["host"] == SERVER)
    assert where["kind"] == "this_machine", where
    _shot(c, "recent.png", "credentials-recent")
    _shot(c, "where-this-machine.png", "credentials-where")
