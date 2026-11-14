"""Server-free unit tests for `fixtures/server.py`,
`fixtures/mount_parents.py` and `fixtures/devcontrol.py` helpers, and for the
committed parallel dev configs and `scripts/render-dev-config.sh` they run.

No shed-server, no `shed` CLI, no network: subprocess and urllib are faked.
The real subprocesses are the render script on local files, and `make` and
`scripts/dev-server-probe.sh` with every outward-reaching command stubbed and
loopback sockets standing in for the server's listeners.
Runs in CI via `make test-integration-units`, next to `test_smoke_units.py`.
"""

from __future__ import annotations

import io
import json
import os
import shutil
import socket
import subprocess
import time
import urllib.error
from pathlib import Path

import pytest
import yaml

from fixtures import devcontrol as dc
from fixtures import mount_parents as mp
from fixtures import server as fx

MAC_DEV_CONFIG = dc.REPO_ROOT / "configs" / "server.dev-parallel.mac.yaml"
FC_DEV_CONFIG = dc.REPO_ROOT / "configs" / "server.dev-parallel.linux-fc.yaml"
RENDER = dc.REPO_ROOT / "scripts" / "render-dev-config.sh"


@pytest.mark.parametrize(
    "version, want",
    [
        ("0.9.0", (0, 9, 0)),  # goreleaser release
        ("v0.9.1", (0, 9, 1)),  # `make build` exactly at a clean tag
        ("0.10.0", (0, 10, 0)),
        ("1.2.3\n", (1, 2, 3)),
        ("0.9.1-rc.1", (0, 9, 1)),  # an rc of 0.9.1 carries its fixes
        ("v0.9.0-3-gabc1234", None),  # git describe: commits past the tag
        ("v0.9.0-3-gabc1234-dirty", None),
        ("v0.9.0-dirty", None),  # on the tag, with local changes
        ("0.9.0-SNAPSHOT-abc1234", None),  # goreleaser snapshot
        ("dev", None),  # un-stamped go build
        ("abc1234", None),  # git describe --always with no tag
        ("", None),
        (None, None),
    ],
)
def test_parse_release_version(version, want):
    assert fx.parse_release_version(version) == want


@pytest.mark.parametrize(
    "version, want",
    [
        ("0.9.0", True),
        ("0.8.2", True),
        ("v0.9.0", True),
        ("0.9.1", False),
        ("0.9.2", False),
        ("0.10.0", False),
        ("1.0.0", False),
        ("v0.9.0-3-gabc1234", False),  # a dev build counts as NEW
        ("v0.9.0-dirty", False),
        ("dev", False),
        (None, False),  # unreadable: never skip on it
    ],
)
def test_released_before(version, want):
    assert fx.released_before(version, "0.9.1") is want


def test_released_before_rejects_a_non_release_minimum():
    with pytest.raises(ValueError):
        fx.released_before("0.9.0", "v0.9.1-2-gabc")


class _FakeResponse(io.BytesIO):
    def __enter__(self):
        return self

    def __exit__(self, *exc):
        self.close()
        return False


def _fake_entry(monkeypatch, endpoint, cert_pair=None):
    entry = None if endpoint is None else {"name": "srv", "endpoint": endpoint}
    monkeypatch.setattr(fx, "resolve_server_entry", lambda name, timeout=10: entry)
    # Never read the developer's real ~/.shed/config.yaml from a unit test.
    monkeypatch.setattr(fx, "client_cert_pair", lambda name, config_path=None: cert_pair)


@pytest.mark.parametrize(
    "endpoint, want_url, want_tls",
    [
        ("http://mini3:18080", "http://mini3:18080/api/info", False),
        ("https://localhost:8443/", "https://localhost:8443/api/info", True),
    ],
)
def test_server_version_reads_api_info(monkeypatch, endpoint, want_url, want_tls):
    _fake_entry(monkeypatch, endpoint)
    seen = {}

    def fake_urlopen(url, timeout=None, context=None):
        seen["url"] = url
        seen["context"] = context
        return _FakeResponse(json.dumps({"name": "srv", "version": "v0.9.0-4-gdeadbee"}).encode())

    monkeypatch.setattr(fx.urllib.request, "urlopen", fake_urlopen)
    assert fx.LocalServer("srv", "firecracker").server_version() == "v0.9.0-4-gdeadbee"
    assert seen["url"] == want_url
    # A pinned (self-signed) token-mode cert must not fail the version read.
    assert (seen["context"] is not None) is want_tls
    if want_tls:
        assert seen["context"].verify_mode == fx.ssl.CERT_NONE


@pytest.mark.parametrize(
    "failure",
    [
        urllib.error.URLError("refused"),
        OSError("handshake failed"),  # e.g. mtls without a client cert
        ValueError("not json"),
    ],
)
def test_server_version_unknown_on_failure(monkeypatch, failure):
    _fake_entry(monkeypatch, "http://mini3:18080")

    def fake_urlopen(url, timeout=None, context=None):
        raise failure

    monkeypatch.setattr(fx.urllib.request, "urlopen", fake_urlopen)
    assert fx.LocalServer("srv", "firecracker").server_version() is None


def test_server_version_unknown_without_an_entry(monkeypatch):
    _fake_entry(monkeypatch, None)
    assert fx.LocalServer("srv", "vz").server_version() is None


def _record_cert_loads(monkeypatch, fail: bool = False) -> list:
    loaded = []

    def fake_load_cert_chain(self, certfile, keyfile=None, password=None):
        loaded.append((certfile, keyfile))
        if fail:
            raise fx.ssl.SSLError("bad pair")

    monkeypatch.setattr(fx.ssl.SSLContext, "load_cert_chain", fake_load_cert_chain)
    return loaded


@pytest.mark.parametrize(
    "endpoint, pair, want_loaded",
    [
        # An mtls entry: its client certificate is presented, or the
        # listener refuses the handshake and the version is unknown.
        ("https://localhost:18443", ("/c/client.pem", "/c/client.key"), [("/c/client.pem", "/c/client.key")]),
        # A token entry has no certificate: nothing to present.
        ("https://localhost:18443", None, []),
        # Plain HTTP never builds a TLS context.
        ("http://mini3:18080", ("/c/client.pem", "/c/client.key"), []),
    ],
    ids=["mtls-entry", "token-entry", "plain-http"],
)
def test_server_version_presents_the_entry_client_cert(monkeypatch, endpoint, pair, want_loaded):
    _fake_entry(monkeypatch, endpoint, cert_pair=pair)
    loaded = _record_cert_loads(monkeypatch)
    seen = {}

    def fake_urlopen(url, timeout=None, context=None):
        seen["context"] = context
        return _FakeResponse(json.dumps({"version": "0.9.0"}).encode())

    monkeypatch.setattr(fx.urllib.request, "urlopen", fake_urlopen)
    assert fx.LocalServer("srv", "vz").server_version() == "0.9.0"
    assert loaded == want_loaded


def test_server_version_survives_an_unloadable_cert(monkeypatch):
    _fake_entry(monkeypatch, "https://localhost:18443", cert_pair=("/gone.pem", "/gone.key"))
    loaded = _record_cert_loads(monkeypatch, fail=True)
    monkeypatch.setattr(
        fx.urllib.request, "urlopen",
        lambda url, timeout=None, context=None: _FakeResponse(json.dumps({"version": "v0.9.0-1-gabc"}).encode()),
    )
    assert fx.LocalServer("srv", "vz").server_version() == "v0.9.0-1-gabc"
    assert loaded == [("/gone.pem", "/gone.key")]


def _write_client_config(tmp_path: Path, servers) -> Path:
    path = tmp_path / "config.yaml"
    path.write_text(yaml.safe_dump({"default_server": "a", "servers": servers}), encoding="utf-8")
    return path


def test_client_cert_pair_reads_the_entry(tmp_path):
    path = _write_client_config(tmp_path, {
        "my-server-dev": {
            "host": "localhost",
            "auth_mode": "mtls",
            "client_cert_file": "/h/.shed/creds/my-server-dev/client.pem",
            "client_key_file": "/h/.shed/creds/my-server-dev/client.key",
        },
        "tok-dev": {"host": "localhost", "control_token": "shed_control_x"},
        "half-dev": {"host": "localhost", "client_cert_file": "/h/c.pem"},
    })
    assert fx.client_cert_pair("my-server-dev", path) == (
        "/h/.shed/creds/my-server-dev/client.pem",
        "/h/.shed/creds/my-server-dev/client.key",
    )
    assert fx.client_cert_pair("tok-dev", path) is None
    assert fx.client_cert_pair("half-dev", path) is None
    assert fx.client_cert_pair("missing", path) is None


def test_client_cert_pair_defaults_to_home(tmp_path, monkeypatch):
    (tmp_path / ".shed").mkdir()
    _write_client_config(tmp_path / ".shed", {"x-dev": {"client_cert_file": "/c", "client_key_file": "/k"}})
    monkeypatch.setenv("HOME", str(tmp_path))
    assert fx.client_cert_pair("x-dev") == ("/c", "/k")


@pytest.mark.parametrize("content", [None, "servers: [unclosed", "just a string\n", "servers: [a, b]\n"])
def test_client_cert_pair_unreadable_config(tmp_path, content):
    path = tmp_path / "config.yaml"
    if content is not None:
        path.write_text(content, encoding="utf-8")
    assert fx.client_cert_pair("x-dev", path) is None


@pytest.mark.parametrize(
    "version, skips",
    [
        ("0.9.0", True),
        ("0.9.1", False),
        ("v0.9.0-7-gabc1234", False),
        (None, False),
    ],
)
def test_skip_if_released_before(monkeypatch, version, skips):
    s = fx.LocalServer("srv", "vz")
    monkeypatch.setattr(s, "server_version", lambda: version)
    if skips:
        with pytest.raises(pytest.skip.Exception, match=r"srv runs released shed-server 0\.9\.0, older than 0\.9\.1"):
            s.skip_if_released_before("0.9.1", "needs the fix")
    else:
        s.skip_if_released_before("0.9.1", "needs the fix")


def test_remote_ssh_run_targets_the_server_host(monkeypatch):
    calls = []

    def fake_run(argv, **kwargs):
        calls.append((argv, kwargs))
        return subprocess.CompletedProcess(argv, 0, stdout="ok\n", stderr="")

    monkeypatch.setattr(fx.subprocess, "run", fake_run)
    r = fx.RemoteServer(ssh_host="mini3", name="mini3-dev", backend="firecracker").ssh_run(
        "cat /tmp/x", timeout=7
    )
    assert r.stdout == "ok\n"
    argv, kwargs = calls[0]
    assert argv == ["ssh", "-o", "BatchMode=yes", "-o", "ConnectTimeout=5", "mini3", "cat /tmp/x"]
    assert kwargs["timeout"] == 7
    assert kwargs["capture_output"] is True and kwargs["text"] is True


# ----------------------------------------------------------------------------
# fixtures/mount_parents.py (#410 classifier)
# ----------------------------------------------------------------------------

_H = mp.HOME
_DEV_MOUNTS = [  # the dev configs' nested mounts, plus a non-9p decoy
    ("/", "overlay"),
    ("/proc", "proc"),
    (f"{_H}/.claude", "9p"),
    (f"{_H}/.local/share/opencode", "9p"),
    (f"{_H}/.local/state/opencode", "9p"),
    (f"{_H}/.config/gh", "9p"),
    (f"{_H}/proj", "virtiofs"),
    ("/tmp/x/y", "9p"),
]


def _facts():
    """Guest facts for _DEV_MOUNTS: everything a shed-owned dir on device 1."""
    return {
        p: mp.PathFact("dir", "1", "shed:shed")
        for p in ["/", _H, f"{_H}/.local", f"{_H}/.local/share", f"{_H}/.local/state", f"{_H}/.config"]
    }


def test_mount_parents_parse_findmnt_unescapes_targets():
    out = "/ overlay\n/home/shed/my\\x20proj virtiofs\n\n"
    assert mp.parse_findmnt(out) == [("/", "overlay"), ("/home/shed/my proj", "virtiofs")]


def test_mount_parents_parse_path_facts():
    out = "/\tdir\t2049 root:root\n/home/shed/x\tmissing\t\n"
    assert mp.parse_path_facts(out) == {
        "/": mp.PathFact("dir", "2049", "root:root"),
        "/home/shed/x": mp.PathFact("missing", "", ""),
    }


def test_mount_parents_fact_paths_cover_root_home_and_parents():
    deep = mp.deep_mounts(_DEV_MOUNTS)
    assert deep == [f"{_H}/.config/gh", f"{_H}/.local/share/opencode", f"{_H}/.local/state/opencode"]
    assert mp.fact_paths(deep) == [
        "/", _H, f"{_H}/.config", f"{_H}/.local", f"{_H}/.local/share", f"{_H}/.local/state",
    ]


def test_mount_parents_classify_dev_config():
    got = mp.classify(_DEV_MOUNTS, _facts())
    assert got.home_blocker is None
    assert got.eligible == [f"{_H}/.config", f"{_H}/.local", f"{_H}/.local/share", f"{_H}/.local/state"]


@pytest.mark.parametrize(
    "mounts, over",
    [
        # A server mount at HOME, or at /home above it.
        (_DEV_MOUNTS + [(_H, "9p")], {}),
        (_DEV_MOUNTS + [("/home", "9p")], {_H: mp.PathFact("dir", "7", "shed:shed")}),
        # HOME on another device than / (a mount findmnt listed elsewhere).
        (_DEV_MOUNTS, {_H: mp.PathFact("dir", "7", "shed:shed")}),
        # Unreadable devices: two empty strings must not compare equal.
        (_DEV_MOUNTS, {"/": mp.PathFact("dir", "", ""), _H: mp.PathFact("dir", "", "")}),
        (_DEV_MOUNTS, {_H: mp.PathFact("dir", "", "")}),
        # HOME a symlink.
        (_DEV_MOUNTS, {_H: mp.PathFact("link", "1", "shed:shed")}),
    ],
    ids=["mount-at-home", "mount-above-home", "home-other-device", "both-devices-unreadable",
         "home-device-unreadable", "home-symlink"],
)
def test_mount_parents_classify_home_inside_a_mount_owns_nothing(mounts, over):
    got = mp.classify(mounts, _facts() | over)
    assert got.home_blocker is not None
    assert got.eligible == []


def test_mount_parents_classify_stops_below_home():
    mounts = [
        ("/", "overlay"),
        (f"{_H}/mnt", "virtiofs"),  # a mount below HOME ...
        (f"{_H}/mnt/inner/deep", "9p"),  # ... and one nested inside it
        (f"{_H}/link/x", "9p"),
        (f"{_H}/odev/x", "9p"),
        (f"{_H}/file/x", "9p"),
        (f"{_H}/ok/x", "9p"),
    ]
    facts = _facts() | {
        f"{_H}/mnt": mp.PathFact("dir", "9", "shed:shed"),
        f"{_H}/mnt/inner": mp.PathFact("dir", "9", "root:root"),
        f"{_H}/link": mp.PathFact("link", "1", "shed:shed"),
        f"{_H}/odev": mp.PathFact("dir", "8", "shed:shed"),  # on another device
        f"{_H}/file": mp.PathFact("other", "1", "shed:shed"),
        f"{_H}/ok": mp.PathFact("dir", "1", "root:root"),
    }
    got = mp.classify(mounts, facts)
    assert got.home_blocker is None
    assert got.eligible == [f"{_H}/ok"]


# ----------------------------------------------------------------------------
# The parallel dev configs, scripts/render-dev-config.sh and
# fixtures/devcontrol.py's auth-mode handling (plan 035 C1: mtls by default)
# ----------------------------------------------------------------------------


def _load_yaml(path: Path) -> dict:
    return yaml.safe_load(path.read_text(encoding="utf-8")) or {}


@pytest.mark.parametrize("path", [MAC_DEV_CONFIG, FC_DEV_CONFIG], ids=["mac", "fc"])
def test_committed_dev_configs_run_mtls(path):
    cfg = _load_yaml(path)
    auth = cfg.get("auth") or {}
    assert auth.get("mode") == "mtls"
    # mtls serves only the TLS listener; it needs a port and a key source.
    assert cfg.get("https_port") == 18443
    assert (auth.get("ssh") or {}).get("github_users") or (auth.get("ssh") or {}).get("authorized_keys")
    # Nothing that only an open-mode server needs.
    assert "allow_insecure_exposure" not in cfg
    # Offset from the production ports (the dev-control safety rails).
    assert cfg.get("ssh_port") == 12222


def test_fc_dev_config_carries_the_service_clients():
    clients = (_load_yaml(FC_DEV_CONFIG).get("auth") or {}).get("clients") or {}
    assert set(clients) == {"itest", "nocturn"}
    assert clients["itest"]["policy"]["name_prefix"] == "itest-svc-"
    assert clients["itest"]["policy"]["images"] == ["itest"]


def _render(tmp_path: Path, base: Path, *args: str) -> subprocess.CompletedProcess:
    return subprocess.run(
        ["bash", str(RENDER), str(base), str(tmp_path / "out.yaml"), *args],
        capture_output=True, text=True, timeout=30,
    )


def test_render_token_flips_only_the_mode(tmp_path):
    r = _render(tmp_path, MAC_DEV_CONFIG, "--token")
    assert r.returncode == 0, r.stderr
    got = _load_yaml(tmp_path / "out.yaml")
    want = _load_yaml(MAC_DEV_CONFIG)
    assert got["auth"]["mode"] == "token"
    want["auth"]["mode"] = "token"
    assert got == want


def test_render_token_composes_with_state_dir(tmp_path):
    r = _render(tmp_path, MAC_DEV_CONFIG, "--token", "--state-dir", "/Volumes/ext/shed-dev/vz/")
    assert r.returncode == 0, r.stderr
    got = _load_yaml(tmp_path / "out.yaml")
    assert got["auth"]["mode"] == "token"
    assert got["vz"]["images_dir"] == "/Volumes/ext/shed-dev/vz"
    assert got["vz"]["instance_dir"] == "/Volumes/ext/shed-dev/vz/instances"


def test_render_token_refuses_a_config_with_service_clients(tmp_path):
    r = _render(tmp_path, FC_DEV_CONFIG, "--token")
    assert r.returncode != 0
    assert "auth.clients" in r.stderr


@pytest.mark.parametrize("args", [[], ["--mtls"]], ids=["no-override", "removed-mtls-flag"])
def test_render_refuses_nothing_to_do(tmp_path, args):
    # The committed configs are already mtls: there is no --mtls any more,
    # and the Makefile runs a committed config directly when nothing is set.
    r = _render(tmp_path, MAC_DEV_CONFIG, *args)
    assert r.returncode != 0


@pytest.mark.parametrize(
    "env, want",
    [({}, "mtls"), ({"SHED_DEV_AUTH_MODE": ""}, "mtls"), ({"SHED_DEV_AUTH_MODE": "token"}, "token"),
     ({"SHED_DEV_AUTH_MODE": "mtls"}, "mtls")],
    ids=["unset", "empty", "token", "mtls"],
)
def test_dev_auth_mode_defaults_to_mtls(env, want):
    assert dc.dev_auth_mode(env) == want


def test_restart_argv_names_config_and_mode():
    assert dc._restart_argv(Path("/x/cfg.yaml"), "token") == [
        "make", "dev-server-restart", "DEV_CONFIG=/x/cfg.yaml", "SHED_DEV_AUTH_MODE=token",
    ]


def test_running_base_config_follows_the_knob():
    base = dc._merge_config({})
    assert dc.running_base_config("mtls") == base
    token = dc.running_base_config("token")
    assert token["auth"]["mode"] == "token"
    assert {k: v for k, v in token.items() if k != "auth"} == {k: v for k, v in base.items() if k != "auth"}
    assert token["auth"]["ssh"] == base["auth"]["ssh"]


def _fake_dev_control(monkeypatch, tmp_path, mode: str) -> dict:
    """Stub every side effect of dev_config(): no server, no make."""
    calls = {"restarts": [], "waits": [], "targets": []}
    monkeypatch.setattr(dc, "DEV_AUTH_MODE", mode)
    monkeypatch.setattr(dc, "DEV_CONFIG_TMP", tmp_path / "test-dev-config.yaml")
    monkeypatch.setattr(dc, "DEV_SCRATCH_DIR", tmp_path)
    monkeypatch.setattr(dc, "assert_dev_target", lambda name: calls["targets"].append(name) or {})
    monkeypatch.setattr(dc, "_make_restart", lambda cfg, auth_mode: calls["restarts"].append((cfg, auth_mode)))
    monkeypatch.setattr(dc, "_wait_reachable", lambda server, timeout, *, config: calls["waits"].append(config))
    return calls


def test_dev_config_refuses_under_mtls_before_any_restart(monkeypatch, tmp_path):
    calls = _fake_dev_control(monkeypatch, tmp_path, "mtls")
    with pytest.raises(RuntimeError, match="SHED_DEV_AUTH_MODE=token"):
        with dc.dev_config({"name": "x"}, "my-server-dev"):
            pass
    assert calls == {"restarts": [], "waits": [], "targets": []}


@pytest.mark.parametrize(
    "overrides, want_mode",
    [
        ({"https_port": 18443, "auth": {"mode": "secure"}}, "secure"),
        # An override that names no mode keeps the running token copy's.
        ({"name": "harness-selfcheck-name"}, "token"),
    ],
    ids=["names-a-mode", "inherits-token"],
)
def test_dev_config_under_token_restores_the_token_base(monkeypatch, tmp_path, overrides, want_mode):
    calls = _fake_dev_control(monkeypatch, tmp_path, "token")
    with dc.dev_config(overrides, "my-server-dev") as name:
        assert name == "my-server-dev"
        written = _load_yaml(tmp_path / "test-dev-config.yaml")
        assert written["auth"]["mode"] == want_mode
        # The rest of the committed base survives the merge.
        assert written["auth"]["ssh"] == _load_yaml(MAC_DEV_CONFIG)["auth"]["ssh"]
    # The override runs as written (`mtls` = no token render); the restore
    # returns to the committed base rendered for the run's own mode.
    assert calls["restarts"] == [
        (tmp_path / "test-dev-config.yaml", "mtls"),
        (dc.DEV_BASE_CONFIG, "token"),
    ]
    assert [w["auth"]["mode"] for w in calls["waits"]] == [want_mode, "token"]
    assert calls["targets"] == ["my-server-dev"]
    assert not (tmp_path / "test-dev-config.yaml").exists()

# ----------------------------------------------------------------------------
# The dev-server targets: settings guards that run before any stop, and
# scripts/dev-server-probe.sh, the readiness probe dev-server-up[-fc] polls.
# A real `make` and a real probe script; ssh, scp, sudo, go and uname are
# stubs that only record their argv, and the "server" is a sleep process
# plus loopback sockets.
# ----------------------------------------------------------------------------

PROBE = dc.REPO_ROOT / "scripts" / "dev-server-probe.sh"

_STUB = """#!/bin/bash
echo "$(basename "$0") $*" >> "$STUB_LOG"
case "$*" in
  *"uname -m"*) echo x86_64 ;;
  *"test -f"*) exit 1 ;;
esac
[ "$(basename "$0")" = uname ] && echo Linux
exit 0
"""


def _make_with_stubs(tmp_path: Path, target: str, *assigns: str) -> tuple[subprocess.CompletedProcess, list[str]]:
    """Run `make -j8 <target>` in the repo with every outward-reaching
    command stubbed. Returns make's result and the stubs' recorded calls."""
    if shutil.which("make") is None:
        pytest.skip("make is not installed")
    stubs = tmp_path / "stubs"
    stubs.mkdir()
    (stubs / "stub").write_text(_STUB)
    (stubs / "stub").chmod(0o755)
    for name in ("ssh", "scp", "sudo", "go", "uname"):
        (stubs / name).symlink_to("stub")
    calls = tmp_path / "calls.log"
    calls.touch()
    env = {k: v for k, v in os.environ.items() if not k.startswith(("SHED_", "MAKE"))}
    env.update(PATH=f"{stubs}:{os.environ['PATH']}", STUB_LOG=str(calls), HOME=str(tmp_path / "home"))
    r = subprocess.run(
        ["make", "-j8", "--no-print-directory", "-C", str(dc.REPO_ROOT), target,
         "FC_REMOTE_HOST=probe.invalid", "GOARCH=amd64", "RELEASE_BUILD_TOOLS_REF=x", *assigns],
        capture_output=True, text=True, timeout=120, env=env,
    )
    return r, calls.read_text().splitlines()


def test_fc_restart_refuses_a_token_mode_before_stopping(tmp_path):
    r, calls = _make_with_stubs(tmp_path, "dev-server-restart-fc", "SHED_DEV_AUTH_MODE=token")
    assert r.returncode != 0
    assert "runs mtls only" in r.stdout + r.stderr
    # Nothing ran past the guard: no ssh (so no stop), no build.
    assert calls == []


def test_fc_restart_stops_before_it_builds_or_starts(tmp_path):
    r, calls = _make_with_stubs(tmp_path, "dev-server-restart-fc", "SHED_DEV_AUTH_MODE=mtls")
    assert r.returncode == 0, r.stdout + r.stderr
    stop = next(i for i, c in enumerate(calls) if "kill -TERM" in c)
    arch = next(i for i, c in enumerate(calls) if "uname -m" in c)
    launch = next(i for i, c in enumerate(calls) if "serve --config" in c)
    assert stop == 0
    assert stop < arch < launch


@pytest.mark.parametrize("mode", ["bogus", ""], ids=["unknown", "empty"])
def test_mac_restart_refuses_a_bad_mode_before_stopping(tmp_path, mode):
    r, calls = _make_with_stubs(tmp_path, "dev-server-restart", f"SHED_DEV_AUTH_MODE={mode}")
    assert r.returncode != 0
    assert "SHED_DEV_AUTH_MODE must be 'mtls' or 'token'" in r.stdout + r.stderr
    # dev-server-down's first command is `uname -s`; it never ran.
    assert calls == []


def test_mac_restart_reaches_the_stop_with_a_good_mode(tmp_path):
    # The control for the test above: with a valid mode the stop does run
    # (the uname stub says Linux, so dev-server-down refuses right there).
    r, calls = _make_with_stubs(tmp_path, "dev-server-restart", "SHED_DEV_AUTH_MODE=token")
    assert r.returncode != 0
    assert "dev-server-down is macOS-only" in r.stdout + r.stderr
    assert calls == ["uname -s"]


@pytest.fixture
def fake_server(tmp_path):
    """A live pid, two accepting loopback listeners and a log naming them."""
    proc = subprocess.Popen(["sleep", "60"])
    socks = []
    for _ in range(2):
        s = socket.socket()
        s.bind(("127.0.0.1", 0))
        s.listen(8)
        socks.append(s)
    https_port, ssh_port = (s.getsockname()[1] for s in socks)
    log = tmp_path / "server.log"
    log.write_text(
        f"2026/11/14 10:00:00 HTTPS server listening on 0.0.0.0:{https_port}\n"
        f"2026/11/14 10:00:00 SSH server listening on 127.0.0.1:{ssh_port}\n"
        "2026/11/14 10:00:00 Shed server is ready\n"
    )
    yield {"pid": proc.pid, "log": log, "proc": proc, "socks": socks}
    proc.kill()
    proc.wait()
    for s in socks:
        s.close()


def _probe(log: Path, pid, settle: str = "0") -> subprocess.CompletedProcess:
    return subprocess.run(
        ["bash", str(PROBE), str(log), str(pid), settle],
        capture_output=True, text=True, timeout=30,
    )


def test_probe_ready_when_alive_and_every_listener_accepts(fake_server):
    r = _probe(fake_server["log"], fake_server["pid"])
    assert r.returncode == 0, r.stdout
    assert r.stdout.startswith("ready:")


def test_probe_fails_a_dead_process(fake_server):
    fake_server["proc"].kill()
    fake_server["proc"].wait()
    r = _probe(fake_server["log"], fake_server["pid"])
    assert r.returncode == 2, r.stdout
    assert "not running" in r.stdout


def test_probe_fails_a_logged_listener_error(fake_server):
    # The finding: "ready" is logged before the listener errors are read, so
    # a port another process holds yields "ready" then this line, then exit.
    with fake_server["log"].open("a") as f:
        f.write("2026/11/14 10:00:00 Server error: HTTPS server error: listen tcp: address already in use\n")
    r = _probe(fake_server["log"], fake_server["pid"])
    assert r.returncode == 2, r.stdout
    assert "address already in use" in r.stdout


def test_probe_settle_catches_an_error_logged_after_the_connect(fake_server):
    # The race the settle covers: the port accepts (held by someone else),
    # the readiness line is there, and the error lands a moment later.
    p = subprocess.Popen(
        ["bash", str(PROBE), str(fake_server["log"]), str(fake_server["pid"]), "2"],
        stdout=subprocess.PIPE, text=True,
    )
    time.sleep(0.5)
    with fake_server["log"].open("a") as f:
        f.write("2026/11/14 10:00:01 Server error: SSH server error: listen tcp: address already in use\n")
    out, _ = p.communicate(timeout=30)
    assert p.returncode == 2, out


def test_probe_not_yet_when_a_listener_does_not_accept(fake_server):
    fake_server["socks"][0].close()
    r = _probe(fake_server["log"], fake_server["pid"])
    assert r.returncode == 1, r.stdout
    assert "nothing accepts" in r.stdout


@pytest.mark.parametrize(
    "drop", ["Shed server is ready", "SSH server listening on", "HTTPS server listening on"],
    ids=["no-ready-line", "no-ssh-listener", "no-http-listener"],
)
def test_probe_not_yet_until_the_log_has_it_all(fake_server, drop):
    log = fake_server["log"]
    log.write_text("".join(l for l in log.read_text().splitlines(keepends=True) if drop not in l))
    r = _probe(log, fake_server["pid"])
    assert r.returncode == 1, r.stdout


def test_probe_fails_an_empty_pid(fake_server):
    # The FC target passes the pid file's contents; a missing file is "".
    r = _probe(fake_server["log"], "")
    assert r.returncode == 2, r.stdout
