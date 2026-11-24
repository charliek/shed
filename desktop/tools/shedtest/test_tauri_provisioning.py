"""The provisioning UI, driven through the real app (plan 039 A7).

Preferences → Provisioning (each provider on or off, its settings and its `only` rule,
written to `delivery.toml`; the credential source rows; a preview of what a named shed
would get) and each shed row's delivery badge and detail (items, last outcome,
problems, Deliver again). Every cell drives the app's own controls through its `ui.*`
doors and reads back what was RENDERED (`prefs.dump`'s `values.provisioning`,
`shed_delivery.dump`, `dashboard.dump`'s `delivery_badge`), never the backend's view.

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
from client import ShedError, TauriClient
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
        _script(home / "bin" / "gh", f"""case "$1 $2" in
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
    rig.mock.release_provisions()
    yield rig
    rig.mock.release_provisions()


def _shot(c: TauriClient, name: str) -> None:
    if platform.system() == "Darwin":
        return
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
                       ("ui.press_delivery_toggle", {"provider": "nope"}),
                       ("ui.type_delivery_field", {"provider": "github", "field": "gh"}),
                       ("ui.type_delivery_field", {"provider": "github", "field": "gh", "value": 3}),
                       ("ui.press_delivery_button", {"provider": "github"}),
                       ("ui.preview_delivery", {"server": SERVER, "shed": "../x"}),
                       ("ui.prefs_section", {})):
        with pytest.raises(ShedError) as e:
            c.call(op, params)
        assert e.value.code == "bad_request", (op, params)


# -- Preferences → Provisioning ----------------------------------------------

def _prov(c: TauriClient) -> dict:
    snap = (c.prefs_dump().get("prefs") or {}).get("values") or {}
    return snap.get("provisioning") or {}


def _provider(c: TauriClient, name: str) -> dict:
    return next((p for p in _prov(c).get("providers", []) if p["name"] == name), {})


def _prefs(c: TauriClient) -> None:
    c.show_preferences()
    c.wait_until(lambda: len(_prov(c).get("providers", [])) == 5, timeout=20,
                 what="the Provisioning section's five providers")


def test_toggling_a_provider_writes_delivery_toml(r):
    c = r.client
    _prefs(c)
    # The window reads the file again when it is shown.
    c.show_preferences()
    c.wait_until(lambda: _provider(c, "github").get("enabled") is True, timeout=15,
                 what="github on")
    assert "provisioning" in c.prefs_dump()["prefs"]["sections"]
    c.call("ui.press_delivery_toggle", {"provider": "github"})
    c.wait_until(lambda: _provider(c, "github").get("enabled") is False, timeout=15,
                 what="github's switch off")
    text = r.settings.read_text()
    gh = text.split("[github]", 1)[1].split("\n[", 1)[0]
    assert "enabled = false" in gh, text
    view = c.call("delivery.settings")
    assert next(p for p in view["providers"] if p["name"] == "github")["enabled"] is False
    # Every other provider is as it was.
    for name in ("ssh-key", "api-keys", "envsecrets-login", "files"):
        assert _provider(c, name)["enabled"] is True, name
    c.call("ui.press_delivery_toggle", {"provider": "github"})
    c.wait_until(lambda: _provider(c, "github").get("enabled") is True, timeout=15,
                 what="github's switch on again")
    _no_value(json.dumps(c.prefs_dump()), r.key_lines)


def _editor(c: TauriClient) -> dict:
    return _prov(c).get("editor") or {}


def _values(c: TauriClient) -> dict:
    return _editor(c).get("values") or {}


def _press(c: TauriClient, provider: str, button: str) -> None:
    c.call("ui.press_delivery_button", {"provider": provider, "button": button})


def _type(c: TauriClient, provider: str, field: str, value: str, index: int | None = None) -> None:
    c.call("ui.type_delivery_field", {"provider": provider, "field": field, "index": index,
                                      "value": value})
    key = field if index is None else f"{field}[{index}]"
    c.wait_until(lambda: _values(c).get(key) in (value, "redacted"), timeout=10,
                 what=f"{key} typed into the editor")


def _edit(c: TauriClient, provider: str) -> dict:
    _press(c, provider, "edit")
    c.wait_until(lambda: _editor(c).get("provider") == provider, timeout=15,
                 what=f"{provider}'s editor open")
    return _values(c)


def _save(c: TauriClient, provider: str) -> None:
    _press(c, provider, "save")
    c.wait_until(lambda: not _provider(c, provider).get("editing") or _provider(c, provider).get("error"),
                 timeout=15, what=f"{provider} saved")
    assert not _provider(c, provider).get("error"), _provider(c, provider)


def _stored(r: Rig) -> dict:
    return tomllib.loads(r.settings.read_text())


def _fresh(c: TauriClient, pred, what: str) -> None:
    """Show Preferences (it reads the file again) and wait until its rows show it."""
    _prefs(c)
    c.show_preferences()
    c.wait_until(lambda: pred(_prov(c)), timeout=15, what=what)


def _cfg(p: dict, name: str) -> dict:
    return next((x for x in p.get("providers", []) if x["name"] == name), {}).get("config") or {}


def test_the_ssh_key_editor_writes_its_fields_and_only_rule(r):
    c = r.client
    _fresh(c, lambda p: _cfg(p, "ssh-key").get("enabled") is True and "only" not in _cfg(p, "ssh-key"),
           "the settings read")
    v = _edit(c, "ssh-key")
    assert v["key_file"] == "~/.ssh/id_ed25519" and v["toggle:sign_commits"] is False, v
    _type(c, "ssh-key", "key_file", "~/.ssh/id_work,1")
    _press(c, "ssh-key", "toggle:sign_commits")
    c.wait_until(lambda: _values(c).get("toggle:sign_commits") is True, timeout=10, what="the switch on")
    _press(c, "ssh-key", "add:only.backends")
    _type(c, "ssh-key", "only.backends", "vz", 0)
    _press(c, "ssh-key", "add:only.repos")
    _type(c, "ssh-key", "only.repos", "example/*", 0)
    _save(c, "ssh-key")
    assert _provider(c, "ssh-key")["only"] == "only backends: vz · repos: example/*"
    ssh = _stored(r)["ssh-key"]
    assert ssh["key_file"] == "~/.ssh/id_work,1" and ssh["sign_commits"] is True, ssh
    assert ssh["only"] == {"backends": ["vz"], "repos": ["example/*"]} and ssh["enabled"] is True
    # A change the schema refuses stays under the row, in its fixed words, and the
    # file is left as it was; corrected, it saves, and the refusal goes.
    before = r.settings.read_text()
    v = _edit(c, "ssh-key")
    assert v["only.backends[0]"] == "vz" and v["key_file"] == "~/.ssh/id_work,1", v
    _type(c, "ssh-key", "only.backends", "docker", 0)
    _press(c, "ssh-key", "save")
    c.wait_until(lambda: _provider(c, "ssh-key").get("error"), timeout=15, what="the refusal shown")
    err = _provider(c, "ssh-key")["error"]
    assert "ssh-key.only.backends" in err and "expected" in err and "docker" not in err, err
    assert r.settings.read_text() == before
    _type(c, "ssh-key", "only.backends", "vz", 0)
    _press(c, "ssh-key", "add:only.backends")
    _type(c, "ssh-key", "only.backends", "remote", 1)
    _save(c, "ssh-key")
    assert not _provider(c, "ssh-key")["error"]
    assert _stored(r)["ssh-key"]["only"]["backends"] == ["vz", "remote"]


def test_list_and_files_entries_are_edited_one_by_one(r):
    c = r.client
    _fresh(c, lambda p: _cfg(p, "api-keys").get("env_files") == ["~/.config/shed/keys.env"],
           "the settings read")
    v = _edit(c, "api-keys")
    assert v["env_files[0]"] == "~/.config/shed/keys.env" and v["toggle:craze_providers"] is True, v
    _press(c, "api-keys", "add:env_files")
    _type(c, "api-keys", "env_files", "~/keys,work.env", 1)
    _save(c, "api-keys")
    assert _stored(r)["api-keys"]["env_files"] == ["~/.config/shed/keys.env", "~/keys,work.env"]
    # Removing one entry leaves the other exactly as it was.
    _edit(c, "api-keys")
    _press(c, "api-keys", "remove:env_files:0")
    c.wait_until(lambda: "env_files[1]" not in _values(c), timeout=10, what="the entry removed")
    assert _values(c)["env_files[0]"] == "~/keys,work.env"
    _save(c, "api-keys")
    assert _stored(r)["api-keys"]["env_files"] == ["~/keys,work.env"]

    v = _edit(c, "files")
    assert v["files.from[0]"] == "~/.npmrc" and v["files.base[1]"] == "project", v
    _press(c, "files", "add:files")
    _type(c, "files", "files.from", "~/notes, todo.md", 2)
    _type(c, "files", "files.to", "notes.md", 2)
    _type(c, "files", "files.base", "project", 2)
    _save(c, "files")
    entries = _stored(r)["files"]["file"]
    assert entries[2] == {"from": "~/notes, todo.md", "to": "notes.md", "base": "project"}, entries
    assert entries[1]["only"] == {"repos": ["example/widgets"]}, "an entry's own rule is kept"


def test_a_passphrase_command_is_never_shown_and_is_kept(r):
    c = r.client
    cmd = ["printf", "%s", "SENTINEL-CMD-0001", ""]
    _write(r.settings, _settings(r.home, passphrase=f"passphrase_command = {json.dumps(cmd)}"))
    _fresh(c, lambda p: _cfg(p, "envsecrets-login").get("passphrase_command") == "set",
           "the command shown as set")
    for shown in (json.dumps(c.call("delivery.settings")), json.dumps(c.prefs_dump())):
        assert "SENTINEL-CMD" not in shown and "printf" not in shown
    v = _edit(c, "envsecrets-login")
    assert v["passphrase_command"] == "set" and "passphrase_command[0]" not in v, v
    assert _editor(c)["buttons"]["replace:passphrase_command"] is True
    # An unrelated edit saves the command back exactly.
    _press(c, "envsecrets-login", "add:only.backends")
    _type(c, "envsecrets-login", "only.backends", "vz", 0)
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
    c.wait_until(lambda: (_prov(c).get("draft") or {}).get("passphrase_command") == "replaced (3 arguments)",
                 timeout=10, what="the replaced command, redacted")
    for shown in (json.dumps(c.prefs_dump()), json.dumps(c.call("delivery.settings"))):
        assert "SENTINEL-CMD" not in shown
    _save(c, "envsecrets-login")
    assert _stored(r)["envsecrets-login"]["passphrase_command"] == ["printf", "SENTINEL-CMD-0002, b", ""]
    assert "SENTINEL-CMD" not in json.dumps(c.prefs_dump())
    # Cleared, for a passphrase file instead (the two are one or the other).
    _edit(c, "envsecrets-login")
    _press(c, "envsecrets-login", "clear:passphrase_command")
    c.wait_until(lambda: "passphrase_command" not in _values(c), timeout=10, what="the command cleared")
    _type(c, "envsecrets-login", "passphrase_file", "~/pp")
    _save(c, "envsecrets-login")
    stored = _stored(r)["envsecrets-login"]
    assert "passphrase_command" not in stored and stored["passphrase_file"] == "~/pp", stored


def test_a_draft_changed_underneath_is_discarded_and_said(r):
    """A reload that finds the provider changed elsewhere discards the open draft, so
    Save can never write back an `enabled` the switch no longer shows."""
    c = r.client
    _fresh(c, lambda p: _cfg(p, "github").get("enabled") is True, "github on")
    _edit(c, "github")
    gh = next(p for p in c.call("delivery.settings")["providers"] if p["name"] == "github")
    c.call("delivery.set", {"provider": "github", "config": {**gh["config"], "enabled": False}})
    c.show_preferences()
    c.wait_until(lambda: "settings changed" in (_provider(c, "github").get("notice") or ""),
                 timeout=15, what="the draft discarded, and said")
    p = _provider(c, "github")
    assert p["editing"] is False and p["enabled"] is False, p
    assert _stored(r)["github"]["enabled"] is False


def test_a_settings_file_error_is_shown_with_its_place(r):
    c = r.client
    _prefs(c)
    _write(r.settings, '[github]\nenabled = true\nbogus = "SENTINEL-SETTINGS-0001"\n')
    c.show_preferences()
    c.wait_until(lambda: _prov(c).get("error"), timeout=15, what="the settings error shown")
    p = _prov(c)
    assert p["error"].startswith("delivery.toml line 3, column 1: "), p["error"]
    assert "SENTINEL" not in json.dumps(c.prefs_dump())
    assert p["providers"] == [], "no switch while the file does not read"
    _write(r.settings, _settings(r.home))
    c.show_preferences()
    c.wait_until(lambda: not _prov(c).get("error") and len(_prov(c).get("providers", [])) == 5,
                 timeout=15, what="the settings read again")


def test_the_preview_renders_items_and_no_value(r):
    c = r.client
    _prefs(c)
    c.call("ui.preview_delivery", {"server": SERVER, "shed": "widgets"})
    c.wait_until(lambda: ((_prov(c).get("preview") or {}).get("items") or [])
                 and not _prov(c)["preview"]["busy"], timeout=30, what="the preview rendered")
    pv = _prov(c)["preview"]
    assert pv["server"] == SERVER and pv["shed"] == "widgets" and pv["error"] is None
    items = {i["name"]: i for i in pv["items"]}
    assert items["ssh-key"]["target"] == ".ssh/id_ed25519" and items["ssh-key"]["mode"] == "0600"
    assert items["ssh-key.pub"]["mode"] == "0644"
    assert items["files..env"]["stage"] == "2"
    assert {"EXAMPLE_API_KEY", "ENVSECRETS_PASSPHRASE", ".config/gh/hosts.yml"} <= {
        i["target"] for i in pv["items"]}
    for it in pv["items"]:
        assert len(it["fingerprint"]) == 24 and it["size"].endswith("B"), it
    assert "widgets on mock · vz · example/widgets" in pv["rendered"]
    _no_value(pv["rendered"], r.key_lines)
    _no_value(json.dumps(c.prefs_dump()), r.key_lines)
    c.call("ui.prefs_section", {"section": "delivery-preview"})
    c.wait_until(lambda: _prov(c).get("shown_section") == "delivery-preview", timeout=10,
                 what="the preview scrolled into view")
    _shot(c, "preview.png")
    # A rule that leaves a provider out: it is named, and its items are gone.
    c.call("delivery.set", {"provider": "github", "config": {
        "enabled": True, "gh": str(r.home / "bin" / "gh"), "only": {"backends": ["remote"]}}})
    c.call("ui.preview_delivery", {"server": SERVER, "shed": "widgets"})
    c.wait_until(lambda: "Left out by their only rule: github" in
                 ((_prov(c).get("preview") or {}).get("rendered") or ""),
                 timeout=30, what="github left out")
    assert "github" not in {i["name"] for i in _prov(c)["preview"]["items"]}
    # No shed: everything the providers have. An unknown shed is an error, shown.
    c.call("ui.preview_delivery", {"server": SERVER})
    c.wait_until(lambda: (_prov(c).get("preview") or {}).get("shed") is None
                 and "before any only rule" in ((_prov(c).get("preview") or {}).get("rendered") or ""),
                 timeout=30, what="the shed-less preview")
    c.call("ui.preview_delivery", {"server": SERVER, "shed": "no-such-shed"})
    c.wait_until(lambda: (_prov(c).get("preview") or {}).get("error"), timeout=30,
                 what="the unknown shed's error")


def test_the_provisioning_section_in_light_and_dark(r):
    c = r.client
    _prefs(c)
    c.show_preferences()
    c.wait_until(lambda: any(s.get("server") == SERVER for s in _prov(c).get("streams", [])),
                 timeout=30, what="the delivery stream line")
    # The source rows moved into this section and still work as they did.
    c.wait_until(lambda: "Use this machine" in (_prov(c).get("rendered") or ""), timeout=20,
                 what="the source row in the section")
    assert [s["host"] for s in c.prefs_dump()["prefs"]["values"]["sources"]] == [SERVER]
    for mode in ("light", "dark"):
        c.call("ui.set_appearance", {"mode": mode})
        c.wait_until(lambda: c.prefs_dump()["prefs"]["mode"] == mode, timeout=10,
                     what=f"preferences in {mode}")
        c.call("ui.prefs_section", {"section": "provisioning"})
        c.wait_until(lambda: _prov(c).get("shown_section") == "provisioning", timeout=10,
                     what="the section scrolled into view")
        _shot(c, f"provisioning-{mode}.png")
        # Scroll away so the next pass scrolls again.
        c.call("ui.prefs_section", {"section": "delivery-preview"})
        c.wait_until(lambda: _prov(c).get("shown_section") == "delivery-preview", timeout=10,
                     what="scrolled away")
    c.call("ui.set_appearance", {"mode": "light"})
    c.prefs_close()
