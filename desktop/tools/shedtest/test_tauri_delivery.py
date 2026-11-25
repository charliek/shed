"""Provisioning by delivery, driven through the real app (plan 039 A6, criterion 10).

The app is a machine source: it holds a delivery stream on every configured server
and answers each request it is sent with what `delivery.toml`'s providers read from
this machine. These cells launch their OWN app (one per module) over a throwaway
HOME that holds every input the five providers read, all made here at test time:

* a throwaway ed25519 key (`ssh-keygen`, by `hermetic`), never a committed one;
* a fake `gh` that prints a sentinel token and login;
* a craze `providers.toml` and an API-key env file (`0600`);
* an envsecrets config, and its passphrase in a file (or a slow command);
* two plain files for the `files` provider, one of them stage 2.

It is pointed at a PRIVATE mock (`SHED_TAURI_MOCK_BASE_URL`, plain HTTP: nothing is
minted over SSH), so nothing here touches the session app or its mock, and nothing reads the developer's own `~/.ssh`,
`~/.config/gh`, `~/.craze` or `~/.envsecrets`. The settings file is the launch's own
(`SHED_TAURI_DELIVERY_CONFIG`, inside the runtime dir).

The cells check the posted answer (names, targets, modes, env variables and values),
each `only` rule (server, backend, repo), that `delivery.preview`/`delivery.status`
carry no value, that a request whose stream ended is never answered (no replay on the
next stream), and that `delivery.set` round-trips. `shed.provision`, `shed.delivery`
and `activity.server_list` run against the session app's mock.
"""

from __future__ import annotations

import base64
import json
import os
import shutil
import stat
import subprocess
import tempfile
from dataclasses import dataclass
from pathlib import Path

import pytest

import hermetic
import ui
from client import ShedError, TauriClient
from mockserver import DELIVERED_RECORD, MockShedServer

FIXTURES = Path(__file__).resolve().parent / "fixtures"
CONFIG = FIXTURES / "config.yaml"
#: The app's name for the mock (fixtures/config.yaml).
SERVER = "mock"

GH_TOKEN = "gho_SENTINELGHTOKEN0001"
CRAZE_KEY = "sk-SENTINEL-CRAZE-0001"
ENV_KEY = "sk-SENTINEL-ENV-0001"
#: A value with quotes, a dollar sign and inner spaces: delivered byte for byte.
QUOTED = 'a "b" $SENTINEL_Q d'
PASSPHRASE = "SENTINEL-PASSPHRASE-0001"
GCS = base64.b64encode(b"SENTINEL-GCS-0001").decode()
NPMRC = "//registry.example.test/:_authToken=SENTINEL-NPMRC-0001\n"
PROJECT_ENV = "SECRET=SENTINEL-PROJECT-0001\n"
SENTINELS = (GH_TOKEN, CRAZE_KEY, ENV_KEY, "SENTINEL_Q", PASSPHRASE, GCS,
             "SENTINEL-GCS", "SENTINEL-NPMRC", "SENTINEL-PROJECT")


@dataclass
class Ctx:
    client: TauriClient
    mock: MockShedServer
    home: Path
    settings: Path
    machine_id: str
    #: The private key's base64 lines (none may appear in a preview).
    key_lines: list[str]
    #: The app's process (the last cell quits it).
    proc: subprocess.Popen


def _write(path: Path, text: str, mode: int = 0o600) -> Path:
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(text)
    path.chmod(mode)
    return path


def _script(path: Path, body: str) -> Path:
    _write(path, "#!/bin/sh\n" + body + "\n", 0o755)
    return path


def _settings(home: Path, *, github_only: str = "", api_only: str = "",
              ssh_only: str = "", passphrase: str | None = None) -> str:
    passphrase = passphrase or 'passphrase_file = "~/.config/envsecrets/passphrase"'
    return f"""[ssh-key]
enabled = true
key_file = "~/.ssh/id_ed25519"
{ssh_only}
[github]
enabled = true
gh = "{home / 'bin' / 'gh'}"
{github_only}
[api-keys]
enabled = true
env_files = ["~/.config/shed/keys.env"]
{api_only}
[envsecrets-login]
enabled = true
{passphrase}

[files]
enabled = true
[[files.file]]
from = "~/.npmrc"
to = ".npmrc"
[[files.file]]
from = "~/work/widgets.env"
to = ".env"
base = "project"
only = {{ repos = ["example/widgets"] }}
"""


@pytest.fixture(scope="module")
def ctx():
    cfg = ui._SUBPROC["tauri"]
    if not cfg.binary.exists():
        raise RuntimeError(f"tauri binary not found at {cfg.binary}; build it first (make tauri-build).")
    if shutil.which("ssh-keygen") is None:
        pytest.skip("ssh-keygen is needed for the throwaway key")
    mock = MockShedServer()
    mock.start()
    home = Path(tempfile.mkdtemp(prefix="shed-dlv-"))
    proc = None
    try:
        # Every input the providers read (the throwaway key included) lives in `home`.
        hermetic.generate_key(home / ".ssh")
        _script(home / "bin" / "gh", f"""case "$1 $2" in
  "auth token") echo {GH_TOKEN} ;;
  "api user") echo octo-test ;;
  *) exit 1 ;;
esac""")
        _script(home / "bin" / "slowpass", f"sleep 3; echo {PASSPHRASE}")
        _write(home / ".craze" / "native" / "providers.toml",
               f'[anthropic]\napi_key = "{CRAZE_KEY}"\n')
        _write(home / ".config" / "shed" / "keys.env",
               f"EXAMPLE_API_KEY={ENV_KEY}\nQUOTED_VALUE={QUOTED}\n")
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
        log = home / "delivery-ui.log"
        log_fh = open(log, "wb")
        proc = subprocess.Popen([str(cfg.binary)], env=env, stdout=log_fh,
                                stderr=subprocess.STDOUT)
        ui.await_hermetic("tauri", sock=sock, mock_base_url=mock.base_url, proc=proc, log=log)
        client = TauriClient(sock)
        client.wait_until(lambda: _server(client)["state"] == "connected", timeout=30,
                          what="the delivery stream connected")
        machine = client.call("delivery.status")["machine"]["id"]
        try:
            yield Ctx(client, mock, home, settings, machine, key_lines, proc)
        finally:
            client.close()
            ui.terminate(proc)
            log_fh.close()
    finally:
        mock.stop()
        shutil.rmtree(home, ignore_errors=True)


def _server(client: TauriClient) -> dict:
    servers = client.call("delivery.status")["servers"]
    return next((s for s in servers if s["server"] == SERVER), {"state": None})


def _items(answer: dict) -> dict[str, dict]:
    return {i["name"]: i for i in answer["body"]["items"]}


def _content(item: dict) -> str:
    return base64.b64decode(item["content"]).decode()


@pytest.fixture
def settings(ctx):
    """Each cell starts from the full settings; a cell may rewrite them."""
    _write(ctx.settings, _settings(ctx.home))
    yield ctx


def test_a_request_is_answered_with_every_providers_items(settings):
    c = settings
    stream = c.mock.live_delivery_stream()
    req = c.mock.push_delivery_request(repo="example/widgets", backend="vz")
    ans = c.mock.await_delivery_answer(req["id"], timeout=30)
    assert ans["stream"] == stream["id"]
    assert ans["machine_id"] == c.machine_id
    assert ans["body"]["version"] == 2 and ans["body"]["id"] == req["id"]
    assert ans["body"]["problems"] == [], ans["body"]["problems"]
    items = _items(ans)
    want = {
        "ssh-key": ("file", ".ssh/id_ed25519", "0600", 1),
        "ssh-key.pub": ("file", ".ssh/id_ed25519.pub", "0644", 1),
        "github": ("file", ".config/gh/hosts.yml", "0600", 1),
        "git-config": ("file", ".config/git/config", "0600", 1),
        "api-keys.craze": ("file", ".craze/native/providers.toml", "0600", 1),
        "envsecrets-login.config": ("file", ".envsecrets/config.yaml", "0600", 1),
        "files..npmrc": ("file", ".npmrc", "0600", 1),
        "files..env": ("file", ".env", "0600", 2),
    }
    for name, (kind, path, mode, stage) in want.items():
        it = items.get(name)
        assert it is not None, f"{name} missing from {sorted(items)}"
        assert (it["kind"], it["path"], it["mode"], it["stage"]) == (kind, path, mode, stage), it
    assert items["files..env"]["base"] == "project"
    env = {i["variable"]: i["value"] for i in ans["body"]["items"] if i["kind"] == "env"}
    assert env == {"EXAMPLE_API_KEY": ENV_KEY, "QUOTED_VALUE": QUOTED,
                   "ENVSECRETS_PASSPHRASE": PASSPHRASE}
    assert GH_TOKEN in _content(items["github"]) and "octo-test" in _content(items["github"])
    assert "gh auth git-credential" in _content(items["git-config"])
    assert CRAZE_KEY in _content(items["api-keys.craze"])
    assert _content(items["envsecrets-login.config"]).startswith("passphrase_env: ")
    assert _content(items["files..npmrc"]) == NPMRC
    assert _content(items["ssh-key"]) == (c.home / ".ssh" / "id_ed25519").read_text()
    for it in ans["body"]["items"]:
        assert len(it["fingerprint"]) == 24, it["name"]
    # What the app keeps of it is redacted.
    status = c.client.call("delivery.status")
    shown = json.dumps(status)
    for s in SENTINELS:
        assert s not in shown, s

    def recent():
        rows = c.client.call("delivery.status")["recent"]
        return next((r for r in rows if r["request"] == req["id"]), None)

    c.client.wait_until(lambda: (recent() or {}).get("outcome") == {"outcome": "accepted"},
                        timeout=10, what="the answer's outcome recorded")
    assert {i["target"] for i in recent()["items"]} >= {".ssh/id_ed25519",
                                                         "ENVSECRETS_PASSPHRASE"}


def test_delivery_recent_lists_the_answers_with_no_value(settings):
    """`delivery.recent` is the answers this machine gave, newest first: when, the
    server, the shed, each item by name, target, size and fingerprint, the problems,
    and what the server made of it. No value or content reaches it."""
    c = settings
    first = c.mock.push_delivery_request(shed="first", repo="example/widgets")
    c.mock.await_delivery_answer(first["id"], timeout=30)
    second = c.mock.push_delivery_request(shed="second", backend="firecracker")
    ans = c.mock.await_delivery_answer(second["id"], timeout=30)

    def newest() -> list[dict]:
        return c.client.call("delivery.recent")["recent"]

    c.client.wait_until(lambda: newest()[0]["outcome"] == {"outcome": "accepted"},
                        timeout=10, what="the newest answer's outcome")
    rows = newest()
    assert [r["request"] for r in rows[:2]] == [second["id"], first["id"]], rows[:2]
    top = rows[0]
    assert (top["server"], top["shed"], top["reason"]) == (SERVER, "second", "create"), top
    assert top["at"].endswith("Z"), top
    assert top["problems"] == [], top
    posted = {i["name"]: i for i in ans["body"]["items"]}
    assert {i["name"] for i in top["items"]} == set(posted), top["items"]
    for it in top["items"]:
        assert set(it) <= {"name", "provider", "kind", "stage", "base", "target", "mode",
                           "size", "fingerprint", "expires_at"}, it
        assert it["fingerprint"] == posted[it["name"]]["fingerprint"], it
    shown = json.dumps(rows)
    for s in SENTINELS:
        assert s not in shown, s
    assert rows == c.client.call("delivery.status")["recent"][:len(rows)]


def test_delivery_describe_names_every_provider(client):
    """`delivery.describe` is the providers' descriptors, in the order they are asked:
    what a form is drawn from (a title, a summary, where the items land, the fields)."""
    providers = client.call("delivery.describe")["providers"]
    assert [p["name"] for p in providers] == ["ssh-key", "github", "api-keys",
                                              "envsecrets-login", "files"]
    for p in providers:
        assert p["title"] and p["summary"] and p["lands"], p
        assert p["fields"], p
        for f in p["fields"]:
            assert f["kind"] in ("text", "secret", "file", "path", "switch", "list"), f
            assert f["place"] in ("machine", "service", "both"), f
    ssh = providers[0]
    key_file = next(f for f in ssh["fields"] if f["key"] == "key_file")
    assert key_file["required"] is True and key_file["place"] == "machine", key_file
    files = next(p for p in providers if p["name"] == "files")
    entry = next(f for f in files["fields"] if f["entry"])
    assert entry["scoped"] is True, entry
    assert {f["key"] for f in entry["entry"]} >= {"from", "to", "mode"}, entry


def test_each_only_rule_is_honoured(settings):
    c = settings
    # Server, backend and repo rules, each on its own provider, set through the op.
    view = c.client.call("delivery.settings")
    cfgs = {p["name"]: p["config"] for p in view["providers"]}
    c.client.call("delivery.set", {"provider": "github", "config": {
        **cfgs["github"], "only": {"servers": ["elsewhere"]}}})
    c.client.call("delivery.set", {"provider": "api-keys", "config": {
        **cfgs["api-keys"], "only": {"backends": ["remote"]}}})
    c.client.call("delivery.set", {"provider": "ssh-key", "config": {
        **cfgs["ssh-key"], "only": {"repos": ["example/*"], "servers": [SERVER]}}})

    def answer(**req) -> set[str]:
        r = c.mock.push_delivery_request(**req)
        return set(_items(c.mock.await_delivery_answer(r["id"], timeout=30)))

    got = answer(repo="example/widgets", backend="vz")
    assert "github" not in got and "git-config" not in got, "server rule"
    assert "api-keys.craze" not in got, "backend rule"
    assert {"ssh-key", "files..env", "files..npmrc"} <= got
    got = answer(repo="example/widgets", backend="remote")
    assert "api-keys.craze" in got and "api-keys.example_api_key" in got
    got = answer(repo="other/thing", backend="vz")
    assert "ssh-key" not in got, "repo rule"
    assert "files..env" not in got, "a files entry's own repo rule"
    assert "files..npmrc" in got
    got = answer(backend="vz")
    assert "ssh-key" not in got, "a shed with no repository matches no repo rule"


def test_the_preview_shows_no_values(settings):
    c = settings
    for params in ({"server": SERVER}, {"server": SERVER, "shed": "hello-world"}):
        p = c.client.call("delivery.preview", params)
        shown = json.dumps(p)
        for s in SENTINELS:
            assert s not in shown, (params, s)
        for line in c.key_lines:
            assert line not in shown, "a line of the private key"
        targets = {i["target"] for i in p["preview"]["items"]}
        assert {".ssh/id_ed25519", ".config/gh/hosts.yml", "EXAMPLE_API_KEY",
                "ENVSECRETS_PASSPHRASE", ".npmrc"} <= targets, targets
        for it in p["preview"]["items"]:
            assert len(it["fingerprint"]) == 24 and it["size"] > 0, it
            assert set(it) <= {"name", "provider", "kind", "stage", "base", "target", "mode",
                               "size", "fingerprint", "expires_at"}, it
    p = c.client.call("delivery.preview", {"server": SERVER, "shed": "hello-world"})
    assert p["shed"] == "hello-world" and p["request"]["backend"] == "firecracker"
    # The settings view names files, never their contents.
    shown = json.dumps(c.client.call("delivery.settings"))
    for s in SENTINELS:
        assert s not in shown, s
    for bad in ({"server": "nope"}, {"server": SERVER, "shed": "../x"}, {}):
        with pytest.raises(ShedError) as e:
            c.client.call("delivery.preview", bad)
        assert e.value.code == "bad_request", bad
    with pytest.raises(ShedError) as e:
        c.client.call("delivery.preview", {"server": SERVER, "shed": "no-such-shed"})
    assert e.value.code == "not_found"


def test_a_request_whose_stream_ends_is_never_answered(settings):
    c = settings
    # A slow passphrase command keeps the answer in the making past the stream's end.
    _write(c.settings, _settings(
        c.home, passphrase=f'passphrase_command = ["{c.home / "bin" / "slowpass"}"]'))
    before = _server(c.client)
    first = c.mock.live_delivery_stream()
    opened = len(c.mock.delivery_streams())
    a = c.mock.push_delivery_request(repo="example/widgets")
    c.mock.wait_delivery_written(a["id"])
    c.mock.end_delivery_stream()
    second = c.mock.live_delivery_stream(timeout=30, after=opened)
    assert second["id"] != first["id"]
    c.client.wait_until(lambda: _server(c.client)["dropped"] > before["dropped"], timeout=30,
                        what="the ended stream's answer dropped")
    b = c.mock.push_delivery_request(repo="example/widgets")
    ans = c.mock.await_delivery_answer(b["id"], timeout=30)
    assert ans["stream"] == second["id"]
    assert all(x["id"] != a["id"] for x in c.mock.delivery_answers()), "never posted"
    recent = c.client.call("delivery.status")["recent"]
    dropped = next(r for r in recent if r["request"] == a["id"])
    assert dropped["outcome"] == {"outcome": "dropped_stream_ended"}


def test_delivery_set_round_trips_and_refuses_without_writing(settings):
    c = settings
    view = c.client.call("delivery.settings")
    assert view["path"] == str(c.settings) and view["exists"] and view["error"] is None
    gh = next(p for p in view["providers"] if p["name"] == "github")
    assert gh["configured"] and gh["enabled"] and gh["config"]["gh"].endswith("/bin/gh")
    new = c.client.call("delivery.set", {"provider": "github",
                                         "config": {**gh["config"], "enabled": False}})
    gh2 = next(p for p in new["providers"] if p["name"] == "github")
    assert gh2["enabled"] is False
    assert stat.S_IMODE(os.stat(c.settings).st_mode) == 0o600
    again = c.client.call("delivery.settings")
    assert again == new
    # Every other provider is as it was.
    for p in view["providers"]:
        if p["name"] != "github":
            assert next(q for q in again["providers"] if q["name"] == p["name"]) == p
    before = c.settings.read_text()
    for params in ({"provider": "github", "config": {"enabled": "SENTINEL-X"}},
                   {"provider": "github", "config": {"enabled": True, "SENTINEL-X": 1}},
                   {"provider": "nope", "config": {"enabled": True}},
                   {"provider": "github", "config": ["x"]},
                   {"provider": "github"}):
        with pytest.raises(ShedError) as e:
            c.client.call("delivery.set", params)
        assert e.value.code == "bad_request", params
        assert "SENTINEL" not in str(e.value), str(e.value)
    assert c.settings.read_text() == before
    # A provider cannot be written on without what it needs (plan 040 §3.9): the
    # fixed class `incomplete`, and the file is left as it was.
    for provider, config in (("ssh-key", {"enabled": True}),
                             ("envsecrets-login", {"enabled": True}),
                             ("files", {"enabled": True, "file": []})):
        with pytest.raises(ShedError) as e:
            c.client.call("delivery.set", {"provider": provider, "config": config})
        assert e.value.code == "incomplete", (provider, e.value)
    assert c.settings.read_text() == before
    # null removes the provider.
    out = c.client.call("delivery.set", {"provider": "files", "config": None})
    assert not next(p for p in out["providers"] if p["name"] == "files")["configured"]


# -- the server-side ops, against the session app's mock --------------------------

def test_shed_delivery_and_provision(client, mock):
    rec = client.call("shed.delivery", {"server": SERVER, "shed": "hello-world"})
    assert rec["delivery"] == DELIVERED_RECORD
    mock.provision_record = {"state": "none"}
    mock.provision_warning = "delivery: no source is connected; run shed provision"
    out = client.call("shed.provision", {"server": SERVER, "shed": "hello-world"})
    assert out["ok"] is True and out["error"] is None
    assert out["delivery"] == {"state": "none"}
    assert [p["message"] for p in out["progress"]][:1] == ["stage1: delivered"]
    assert out["progress"][-1] == {"message": mock.provision_warning, "warning": True}
    assert mock.provisions == ["hello-world"]
    assert client.call("shed.delivery", {"server": SERVER, "shed": "hello-world"})[
        "delivery"] == {"state": "none"}
    refused = client.call("shed.provision", {"server": SERVER, "shed": "callbell"})
    assert refused["ok"] is False and refused["error"]["status"] == 409
    assert refused["error"]["code"] == "SHED_NOT_RUNNING"
    mock.provision_should_fail = True
    failed = client.call("shed.provision", {"server": SERVER, "shed": "hello-world"})
    assert failed["ok"] is False and failed["error"]["code"] == "BACKEND_ERROR"
    for bad in ({"server": "nope", "shed": "hello-world"}, {"server": SERVER, "shed": "a/b"},
                {"server": SERVER}):
        with pytest.raises(ShedError) as e:
            client.call("shed.provision", bad)
        assert e.value.code == "bad_request", bad
    with pytest.raises(ShedError) as e:
        client.call("shed.delivery", {"server": SERVER, "shed": "gone"})
    assert e.value.code == "not_found"


def test_activity_pages_newest_first(client, mock):
    seen, cursor, pages = [], None, 0
    while True:
        page = client.call("activity.server_list", {"limit": 2, "cursor": cursor})
        assert page["errors"] == []
        seen += [(e["server"], e["id"]) for e in page["events"]]
        pages += 1
        cursor = page["next"]
        if cursor is None:
            break
        assert pages < 10
    assert seen == [(SERVER, i) for i in ("c5", "c4", "c3", "c2", "c1")]
    assert pages == 3
    only = client.call("activity.server_list", {"shed": "callbell"})
    assert [e["id"] for e in only["events"]] == ["c2"] and only["next"] is None
    for bad in ({"limit": 0}, {"limit": 501}, {"limit": "x"}, {"cursor": "nope"},
                {"cursor": json.dumps({"elsewhere": "c1"})}, {"shed": "../x"}):
        with pytest.raises(ShedError) as e:
            client.call("activity.server_list", bad)
        assert e.value.code == "bad_request", bad


# -- last: quits the module's app -----------------------------------------------

def _pids(path: Path) -> list[int]:
    try:
        return [int(l) for l in path.read_text().split() if l.strip().isdigit()]
    except FileNotFoundError:
        return []


def _alive(pid: int) -> bool:
    """A live process (a zombie is not)."""
    try:
        os.kill(pid, 0)
    except ProcessLookupError:
        return False
    except PermissionError:
        return True
    try:
        stat = Path(f"/proc/{pid}/stat").read_text()
    except OSError:
        return True
    return not stat.rsplit(") ", 1)[1].startswith("Z")


def test_a_quit_during_a_slow_command_leaves_no_child(settings):
    """The app quits while a provider command (and a process it started) runs: the
    orderly shutdown's source drain kills and reaps their process group, so none of
    them outlives the app. The command would run 300 s; its own timeout is 10 s."""
    c = settings
    pids = c.home / "hang.pids"
    hang = _script(c.home / "bin" / "hangpass",
                   f"sleep 300 & echo $! >> {pids}; echo $$ >> {pids}; wait")
    _write(c.settings, _settings(c.home, passphrase=f'passphrase_command = ["{hang}"]'))
    c.mock.push_delivery_request(repo="example/widgets")
    c.client.wait_until(lambda: len(_pids(pids)) >= 2, timeout=30, what="the command running")
    try:
        c.client.call("app.quit")
    except ShedError:
        pass  # the socket may close with the app
    c.proc.wait(timeout=30)
    alive = [p for p in _pids(pids) if _alive(p)]
    assert alive == [], f"provider processes outlived the app: {alive}"
