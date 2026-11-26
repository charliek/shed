"""Server-free unit tests for `fixtures/server.py`,
`fixtures/mount_parents.py` and `fixtures/devcontrol.py` helpers, and for the
committed parallel dev configs and `scripts/render-dev-config.sh` they run.

No shed-server, no `shed` CLI, no network: subprocess and urllib are faked.
The real subprocesses are the render script on local files, and `make`,
`scripts/dev-server-probe.sh` and `scripts/dev-server-wait.sh` with every
outward-reaching command stubbed and loopback sockets standing in for the
server's listeners.
Runs in CI via `make test-integration-units`, next to `test_smoke_units.py`.
"""

from __future__ import annotations

import base64
import hashlib
import io
import json
import os
import re
import shutil
import socket
import stat
import subprocess
import threading
import time
import urllib.error
from pathlib import Path
from typing import Optional

import pytest
import yaml

from fixtures import devcontrol as dc
from fixtures import mount_parents as mp
from fixtures import server as fx
from fixtures import tlsclient

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
    monkeypatch.setattr(fx, "resolve_server_entry", lambda name, timeout=10, env=None: entry)
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


def test_full_server_entry_merges_the_raw_entry(tmp_path, monkeypatch):
    """`shed --json server list` has no auth mode, API URL, TLS pin or
    certificate paths; the compute suite's remote_base reads them, so the
    raw config entry is merged under the listed one (the listed keys win)."""
    path = _write_client_config(tmp_path, {
        "remote-dev": {
            "host": "localhost",
            "auth_mode": "mtls",
            "api_url": "https://localhost:18490",
            "tls_cert_fingerprint": "SHA256:abc",
            "client_cert_file": "/h/c.pem",
            "client_key_file": "/h/c.key",
            "user": "raw-user",
        },
    })
    listed = {"name": "remote-dev", "host": "localhost", "https_port": 18490, "security": "mtls", "user": "dev"}
    monkeypatch.setattr(fx, "resolve_server_entry",
                        lambda name, timeout=10, env=None: dict(listed) if name == "remote-dev" else None)
    e = fx.full_server_entry("remote-dev", config_path=path)
    assert e["auth_mode"] == "mtls" and e["api_url"] == "https://localhost:18490"
    assert e["client_cert_file"] == "/h/c.pem" and e["client_key_file"] == "/h/c.key"
    assert e["tls_cert_fingerprint"] == "SHA256:abc"
    assert e["user"] == "dev" and e["https_port"] == 18490
    assert fx.full_server_entry("missing", config_path=path) is None
    # Listed, with no raw entry (or no file): the listed entry alone.
    assert fx.full_server_entry("remote-dev", config_path=tmp_path / "none.yaml") == listed
    # $HOME in env names the file.
    (tmp_path / "home" / ".shed").mkdir(parents=True)
    _write_client_config(tmp_path / "home" / ".shed", {"remote-dev": {"auth_mode": "mtls"}})
    assert fx.full_server_entry("remote-dev", env={"HOME": str(tmp_path / "home")})["auth_mode"] == "mtls"


def test_remote_compute_reads_the_full_entry():
    """remote_base reads the merged entry, never the bare JSON list's."""
    src = (Path(__file__).parent / "test_remote_compute.py").read_text(encoding="utf-8")
    assert "full_server_entry(remote_server.name)" in src
    assert "resolve_server_entry" not in src


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
# fixtures/devcontrol.py's dev_config(): mtls is the only enforced mode, and
# the SHED_DEV_AUTH_MODE knob is gone.
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
    # Nothing that only an open-mode server needed, and which no longer loads.
    assert "allow_insecure_exposure" not in cfg
    # Offset from the production ports (the dev-control safety rails).
    assert cfg.get("ssh_port") == 12222


def test_fc_dev_config_carries_the_service_clients():
    clients = (_load_yaml(FC_DEV_CONFIG).get("auth") or {}).get("clients") or {}
    assert set(clients) == {"itest", "nocturn"}
    assert clients["itest"]["policy"]["name_prefix"] == "itest-svc-"
    assert clients["itest"]["policy"]["images"] == ["itest"]


def test_open_mode_override_is_a_loopback_open_server():
    # What dev_config() runs for the open-mode tests: open, plain HTTP on an
    # offset port, no https_port (an open server serves no TLS), loopback.
    cfg = dc._merge_config(dc.OPEN_MODE_OVERRIDES)
    assert dc.config_auth_mode(cfg) == "open"
    assert not dc._config_enforced(cfg)
    assert not cfg.get("https_port")
    assert cfg.get("http_port") == 18080
    assert cfg.get("bind_address") in (None, "", "127.0.0.1", "localhost", "::1")
    # The committed base, by contrast, is mtls.
    assert dc._config_enforced(dc._merge_config({}))


def _render(tmp_path: Path, base: Path, *args: str) -> subprocess.CompletedProcess:
    return subprocess.run(
        ["bash", str(RENDER), str(base), str(tmp_path / "out.yaml"), *args],
        capture_output=True, text=True, timeout=30,
    )


def test_render_state_dir_relocates_only_the_blob_dirs(tmp_path):
    r = _render(tmp_path, MAC_DEV_CONFIG, "--state-dir", "/Volumes/ext/shed-dev/vz/")
    assert r.returncode == 0, r.stderr
    got = _load_yaml(tmp_path / "out.yaml")
    want = _load_yaml(MAC_DEV_CONFIG)
    assert got["auth"] == want["auth"]
    assert got["vz"]["images_dir"] == "/Volumes/ext/shed-dev/vz"
    assert got["vz"]["instance_dir"] == "/Volumes/ext/shed-dev/vz/instances"


# ----------------------------------------------------------------------------
# The Mac dev server's own identity directory (state_dir). Without one it
# resolves shed-server's default, ~/.shed: the brew-installed server's host
# key, TLS pin, client CA, upgrade marker and pins (C28).
# ----------------------------------------------------------------------------


def test_mac_dev_config_keeps_its_identity_in_its_own_state_dir(monkeypatch, tmp_path):
    monkeypatch.setenv("HOME", str(tmp_path))
    cfg = _load_yaml(MAC_DEV_CONFIG)
    raw = cfg.get("state_dir")
    assert raw, "the Mac dev config names no state_dir: it would share the brew server's ~/.shed"
    # What config load accepts (internal/config/server.go: validateStateDir).
    assert raw.startswith("~/") or raw.startswith("/"), raw
    got = dc.dev_state_dir(cfg, environ={})
    # In the dev targets' scratch area, beside the log and the pid file.
    assert got == tmp_path / ".shed" / "dev" / "state"
    assert got != tmp_path / ".shed"
    # Not inside any blob directory SHED_DEV_STATE_DIR relocates.
    for key in ("images_dir", "instance_dir", "snapshots_dir", "uppers_dir"):
        blob = Path(dc._go_expand(cfg["vz"][key]))
        assert blob != got and blob not in got.parents, (key, blob, got)
    # dev_config() and run_preflight() check the merged base the same way.
    assert dc.dev_state_dir(environ={}) == got


def _render_state_dir_args(tmp_path: Path, which: str, keys_dir: Optional[Path]) -> tuple[list[str], dict]:
    relocated = str(tmp_path / "ext" / "shed-dev" / "vz")
    args = {"state-dir": ["--state-dir", relocated], "users": ["--users", str(keys_dir)],
            "both": ["--state-dir", relocated, "--users", str(keys_dir)]}[which]
    return args, ({"SHED_DEV_STATE_DIR": relocated} if "--state-dir" in args else {})


@pytest.mark.parametrize("which", ["state-dir", "users", "both"])
def test_the_rendered_mac_config_keeps_its_state_dir(monkeypatch, tmp_path, which):
    # Every config the Makefile renders for the Mac dev server (with
    # SHED_DEV_STATE_DIR, SHED_DEV_USERS=itest, or both) keeps the base's
    # identity directory, and it is not moved with the blob directories.
    monkeypatch.setenv("HOME", str(tmp_path))
    keys = None
    if which != "state-dir":
        if shutil.which("uv") is None or shutil.which("ssh-keygen") is None:
            pytest.skip("uv and ssh-keygen are needed for --users")
        keys = ident.ensure_keys(tmp_path / "keys")
    args, environ = _render_state_dir_args(tmp_path, which, keys)
    r = _render(tmp_path, MAC_DEV_CONFIG, *args)
    assert r.returncode == 0, r.stdout + r.stderr
    got = _load_yaml(tmp_path / "out.yaml")
    assert got.get("state_dir") == _load_yaml(MAC_DEV_CONFIG)["state_dir"]
    state = dc.dev_state_dir(got, environ=environ)
    assert state == tmp_path / ".shed" / "dev" / "state"
    if environ:
        assert got["vz"]["images_dir"] == environ["SHED_DEV_STATE_DIR"]  # the blobs did move


@pytest.mark.parametrize(
    "state_dir, environ, match",
    [
        (None, {}, "names no state_dir"),
        ("", {}, "names no state_dir"),
        ("~/.shed", {}, "installed server"),
        ("~/.shed/", {}, "installed server"),
        ("{home}/.shed", {}, "installed server"),
        ("/etc/shed", {}, "installed server"),
        ("dev/state", {}, "not absolute"),
        ("~/Library/Application Support/shed-dev/vz/state", {}, "inside the blob directory"),
        ("/Volumes/ext/shed-dev/vz/state", {"SHED_DEV_STATE_DIR": "/Volumes/ext/shed-dev/vz/"}, "inside the blob"),
        ("/Volumes/ext/shed-dev/vz", {"SHED_DEV_STATE_DIR": "/Volumes/ext/shed-dev/vz"}, "inside the blob"),
    ],
    ids=["missing", "empty", "home-shed", "home-shed-slash", "home-shed-absolute", "etc-shed", "relative",
         "under-images-dir", "under-relocated-blobs", "is-relocated-blobs"],
)
def test_dev_state_dir_refuses_the_installed_servers_and_the_blob_dirs(monkeypatch, tmp_path, state_dir, environ,
                                                                         match):
    monkeypatch.setenv("HOME", str(tmp_path))
    cfg = _load_yaml(MAC_DEV_CONFIG)
    if state_dir is None:
        cfg.pop("state_dir", None)
    else:
        cfg["state_dir"] = state_dir.format(home=tmp_path)
    with pytest.raises(RuntimeError, match=match):
        dc.dev_state_dir(cfg, environ=environ)


def test_dev_state_dir_accepts_a_relocated_store_elsewhere(monkeypatch, tmp_path):
    # The control for the cases above: blobs on another volume, identity at home.
    monkeypatch.setenv("HOME", str(tmp_path))
    cfg = _load_yaml(MAC_DEV_CONFIG)
    assert dc.dev_state_dir(cfg, environ={"SHED_DEV_STATE_DIR": "/Volumes/ext/shed-dev/vz"}) == \
        tmp_path / ".shed" / "dev" / "state"
    # A sibling whose name only starts like a blob directory is not inside it.
    cfg["state_dir"] = "/Volumes/ext/shed-dev/vz-identity"
    assert dc.dev_state_dir(cfg, environ={"SHED_DEV_STATE_DIR": "/Volumes/ext/shed-dev/vz"}) == \
        Path("/Volumes/ext/shed-dev/vz-identity")


@pytest.mark.parametrize(
    "args", [[], ["--token"], ["--mtls"], ["--token", "--state-dir", "/x"]],
    ids=["no-override", "removed-token-flag", "removed-mtls-flag", "token-beside-state-dir"],
)
def test_render_refuses_a_mode_flag_or_nothing_to_do(tmp_path, args):
    # The committed configs are mtls and there is no other mode to render:
    # both mode flags are unknown arguments, and with no override at all the
    # Makefile runs a committed config directly.
    r = _render(tmp_path, MAC_DEV_CONFIG, *args)
    assert r.returncode != 0
    if args:
        assert "unknown argument" in r.stderr
        assert not (tmp_path / "out.yaml").exists()


def test_restart_argv_names_only_the_config():
    # SHED_DEV_USERS is pinned empty: the written config says whether it has
    # a user list, and a value left in the shell must not add a second one.
    assert dc._restart_argv(Path("/x/cfg.yaml")) == [
        "make", "dev-server-restart", "DEV_CONFIG=/x/cfg.yaml", "SHED_DEV_USERS=",
    ]


def _store_state(store: Path) -> Optional[str]:
    """The bindings store's content, or None when there is none."""
    return store.read_text() if store.exists() else None


def _fake_dev_control(monkeypatch, tmp_path) -> dict:
    """Stub every side effect of dev_config(): no server, no make. The
    credential source bindings store is a file under tmp_path (`store`);
    `events` records each stop ("down") and each start ("up", config, the
    store's content as that server starts with it)."""
    state = tmp_path / "vz-state"
    state.mkdir(exist_ok=True)
    store = state / dc.BINDINGS_FILE
    calls = {"restarts": [], "waits": [], "targets": [], "events": [], "store": store}

    def restart(cfg):
        calls["restarts"].append(cfg)
        calls["events"].append(("up", cfg, _store_state(store)))

    monkeypatch.setattr(dc, "DEV_CONFIG_TMP", tmp_path / "test-dev-config.yaml")
    monkeypatch.setattr(dc, "DEV_SCRATCH_DIR", tmp_path)
    monkeypatch.setattr(dc, "assert_dev_target", lambda name: calls["targets"].append(name) or {})
    monkeypatch.setattr(dc, "dev_bindings_store", lambda config, environ=None: store)
    monkeypatch.setattr(dc, "_make_down", lambda: calls["events"].append("down"))
    monkeypatch.setattr(dc, "_make_restart", restart)
    monkeypatch.setattr(dc, "_wait_ready", lambda server, timeout, *, config: calls["waits"].append(config))
    return calls


@pytest.mark.parametrize(
    "overrides, want_mode",
    [
        ({"name": "harness-selfcheck-name"}, "mtls"),
        (dc.OPEN_MODE_OVERRIDES, "open"),
    ],
    ids=["mtls-base", "open-on-loopback"],
)
def test_dev_config_runs_the_override_then_restores_the_base(monkeypatch, tmp_path, overrides, want_mode):
    calls = _fake_dev_control(monkeypatch, tmp_path)
    with dc.dev_config(overrides, "my-server-dev") as name:
        assert name == "my-server-dev"
        written = _load_yaml(tmp_path / "test-dev-config.yaml")
        assert dc.config_auth_mode(written) == want_mode
        # The rest of the committed base survives the merge.
        assert written["auth"]["ssh"] == _load_yaml(MAC_DEV_CONFIG)["auth"]["ssh"]
    # The override runs as written; the restore pins the committed base, and
    # each restart is waited on in its own mode.
    assert calls["restarts"] == [tmp_path / "test-dev-config.yaml", dc.DEV_BASE_CONFIG]
    assert [dc.config_auth_mode(w) for w in calls["waits"]] == [want_mode, "mtls"]
    assert calls["targets"] == ["my-server-dev"]
    assert not (tmp_path / "test-dev-config.yaml").exists()


def test_dev_config_restores_the_base_when_the_body_raises(monkeypatch, tmp_path):
    calls = _fake_dev_control(monkeypatch, tmp_path)
    with pytest.raises(RuntimeError, match="test body"):
        with dc.dev_config(dc.OPEN_MODE_OVERRIDES, "my-server-dev"):
            raise RuntimeError("test body")
    assert calls["restarts"][-1] == dc.DEV_BASE_CONFIG
    assert dc.config_auth_mode(calls["waits"][-1]) == "mtls"


def test_dev_config_refuses_a_production_port_before_any_restart(monkeypatch, tmp_path):
    calls = _fake_dev_control(monkeypatch, tmp_path)
    with pytest.raises(RuntimeError, match="production ports"):
        with dc.dev_config({"ssh_port": 2222}, "my-server-dev"):
            pass
    assert calls["restarts"] == [] and calls["waits"] == []


@pytest.mark.parametrize("override", [{"state_dir": "~/.shed"}, {"state_dir": ""}], ids=["installed", "none"])
def test_dev_config_refuses_the_installed_servers_state_dir_before_any_stop(monkeypatch, tmp_path, override):
    calls = _fake_dev_control(monkeypatch, tmp_path)
    monkeypatch.setenv("HOME", str(tmp_path))
    with pytest.raises(RuntimeError, match="installed server"):
        with dc.dev_config(override, "my-server-dev"):
            pytest.fail("the detour ran")
    assert calls["events"] == [] and calls["waits"] == []


def test_run_preflight_refuses_the_installed_servers_state_dir(monkeypatch, tmp_path):
    # Refused before anything is written or launched.
    monkeypatch.setenv("HOME", str(tmp_path))
    monkeypatch.setattr(dc, "SHED_SERVER_BIN", Path(shutil.which("true")))  # /usr/bin/true on macOS
    monkeypatch.setattr(dc, "PREFLIGHT_CONFIG_TMP", tmp_path / "preflight.yaml")
    launched = []
    monkeypatch.setattr(dc.subprocess, "Popen", lambda *a, **k: launched.append(a) or pytest.fail("launched"))
    with pytest.raises(RuntimeError, match="installed server"):
        dc.run_preflight({"state_dir": "~/.shed"})
    assert launched == [] and not (tmp_path / "preflight.yaml").exists()


# ----------------------------------------------------------------------------
# dev_config() keeps the developer's credential source bindings: moved aside,
# with the server down, for the detour and put back, with the server down,
# before the base starts. Server control stubbed; the store is a temp file.
# This logic runs only on macOS for real: the Mac run proves it there.
# ----------------------------------------------------------------------------


def _leftovers(store: Path) -> list[str]:
    return sorted(p.name for p in store.parent.iterdir() if p.name.startswith(store.name + ".itest"))


def test_dev_config_keeps_the_developers_bindings_across_the_detour(monkeypatch, tmp_path, keys_dir):
    calls = _fake_dev_control(monkeypatch, tmp_path)
    store = calls["store"]
    store.write_text("DEVELOPER")
    with dc.dev_config({}, "my-server-dev", transform=lambda c: ident.with_itest_users(c, keys_dir)):
        # The user-list server writes a store of its own.
        store.write_text("DETOUR")
    temp = tmp_path / "test-dev-config.yaml"
    # Stopped before each move: the detour's server starts with no store,
    # the base's with the developer's, the detour's discarded.
    assert calls["events"] == ["down", ("up", temp, None), "down", ("up", dc.DEV_BASE_CONFIG, "DEVELOPER")]
    assert _store_state(store) == "DEVELOPER" and _leftovers(store) == []


def test_dev_config_keeps_a_missing_store_missing(monkeypatch, tmp_path):
    calls = _fake_dev_control(monkeypatch, tmp_path)
    store = calls["store"]
    with dc.dev_config({}, "my-server-dev"):
        assert _leftovers(store) == [store.name + dc.BINDINGS_ABSENT_SUFFIX]
        store.write_text("DETOUR")
    assert calls["events"][-1] == ("up", dc.DEV_BASE_CONFIG, None)
    assert _store_state(store) is None and _leftovers(store) == []


def test_dev_config_puts_the_bindings_back_when_the_body_raises(monkeypatch, tmp_path):
    calls = _fake_dev_control(monkeypatch, tmp_path)
    store = calls["store"]
    store.write_text("DEVELOPER")
    with pytest.raises(RuntimeError, match="test body"):
        with dc.dev_config({}, "my-server-dev"):
            store.write_text("DETOUR")
            raise RuntimeError("test body")
    assert calls["events"][-2:] == ["down", ("up", dc.DEV_BASE_CONFIG, "DEVELOPER")]
    assert _store_state(store) == "DEVELOPER" and _leftovers(store) == []


@pytest.mark.parametrize(
    "killed_at",
    ["during-the-detour", "between-link-and-unlink", "during-the-detour-with-no-store"],
)
def test_dev_config_recovers_a_run_killed_mid_detour(monkeypatch, tmp_path, killed_at):
    # A run killed mid-detour left the developer's store aside and the
    # detour's in its place. The next dev_config() puts the developer's back
    # first, never overwriting the kept copy with the detour's, and the run
    # ends with it in place.
    calls = _fake_dev_control(monkeypatch, tmp_path)
    store = calls["store"]
    detour = dc.BindingsDetour(store)
    if killed_at == "during-the-detour":
        detour.saved.write_text("DEVELOPER")
        store.write_text("DETOUR OF THE KILLED RUN")
        want = "DEVELOPER"
    elif killed_at == "between-link-and-unlink":
        store.write_text("DEVELOPER")
        os.link(store, detour.saved)
        want = "DEVELOPER"
    else:
        detour.absent.write_text("")
        store.write_text("DETOUR OF THE KILLED RUN")
        want = None
    with dc.dev_config({}, "my-server-dev"):
        assert _store_state(store) is None, "the detour's server started with a store"
        store.write_text("DETOUR")
    assert calls["events"][1] == ("up", tmp_path / "test-dev-config.yaml", None)
    assert _store_state(store) == want and _leftovers(store) == []


def test_bindings_detour_restore_recovers_and_reports(tmp_path):
    store = tmp_path / dc.BINDINGS_FILE
    d = dc.BindingsDetour(store)
    assert d.restore() is False  # nothing kept: nothing to do
    store.write_text("DEVELOPER")
    assert d.save() is False and _store_state(store) is None and d.saved.read_text() == "DEVELOPER"
    store.write_text("DETOUR")
    assert d.restore() is True and _store_state(store) == "DEVELOPER" and _leftovers(store) == []


def test_dev_config_moves_the_store_only_in_the_outermost_call(monkeypatch, tmp_path):
    # test_users_upgrade.py restarts inside a users detour: the inner call
    # neither keeps nor puts back anything.
    calls = _fake_dev_control(monkeypatch, tmp_path)
    store = calls["store"]
    store.write_text("DEVELOPER")
    temp = tmp_path / "test-dev-config.yaml"
    with dc.dev_config({}, "my-server-dev"):
        store.write_text("OUTER")
        with dc.dev_config({"name": "inner"}, "my-server-dev"):
            store.write_text("INNER")
        assert _store_state(store) == "INNER"
    assert calls["events"] == [
        "down", ("up", temp, None),
        ("up", temp, "OUTER"), ("up", dc.DEV_BASE_CONFIG, "INNER"),
        "down", ("up", dc.DEV_BASE_CONFIG, "DEVELOPER"),
    ]
    assert _store_state(store) == "DEVELOPER" and _leftovers(store) == [] and dc._detour_depth == 0


def test_dev_config_moves_nothing_while_a_server_holds_the_store(monkeypatch, tmp_path):
    import fcntl

    calls = _fake_dev_control(monkeypatch, tmp_path)
    store = calls["store"]
    store.write_text("DEVELOPER")
    monkeypatch.setattr(dc, "BINDINGS_UNLOCK_TIMEOUT", 0.3)
    fd = os.open(store.with_name(store.name + dc.BINDINGS_LOCK_SUFFIX), os.O_RDWR | os.O_CREAT, 0o600)
    try:
        fcntl.flock(fd, fcntl.LOCK_EX | fcntl.LOCK_NB)  # a server that did not stop
        with pytest.raises(RuntimeError, match="still holds"):
            with dc.dev_config({}, "my-server-dev"):
                pytest.fail("the detour ran")
    finally:
        os.close(fd)
    assert _store_state(store) == "DEVELOPER" and _leftovers(store) == []
    assert calls["restarts"] == [dc.DEV_BASE_CONFIG]  # the base is started again all the same


def test_dev_bindings_store_follows_the_dev_servers_instance_dir(monkeypatch, tmp_path):
    monkeypatch.setenv("HOME", str(tmp_path))
    base = dc._merge_config({})
    # The committed base: beside ~/Library/Application Support/shed-dev/vz/instances.
    assert dc.dev_bindings_store(base, {}) == tmp_path / "Library/Application Support/shed-dev/vz/source-bindings.json"
    # SHED_DEV_STATE_DIR: the renderer puts instance_dir at <dir>/instances.
    assert dc.dev_bindings_store(base, {"SHED_DEV_STATE_DIR": "/Volumes/ext/shed-dev/vz/"}) == \
        Path("/Volumes/ext/shed-dev/vz/source-bindings.json")
    assert dc.dev_bindings_store(base, {"SHED_DEV_STATE_DIR": "~/elsewhere"}) == tmp_path / "elsewhere/source-bindings.json"
    # An empty SHED_DEV_STATE_DIR is unset (the Makefile renders nothing).
    assert dc.dev_bindings_store(base, {"SHED_DEV_STATE_DIR": ""}) == dc.dev_bindings_store(base, {})


@pytest.mark.parametrize(
    "config, environ, match",
    [
        ({"default_backend": "firecracker", "vz": {"instance_dir": "/x/instances"}}, {}, "not 'vz'"),
        ({"default_backend": "vz", "vz": {}}, {}, "no vz.instance_dir"),
        ({"default_backend": "vz", "vz": {"instance_dir": "rel/instances"}}, {}, "not absolute"),
        ({"default_backend": "vz", "vz": {"instance_dir": "/x/instances"}}, {"SHED_DEV_STATE_DIR": "rel"}, "not absolute"),
        ({"default_backend": "vz", "vz": {"instance_dir": "/x/instances"}}, {"SHED_DEV_STATE_DIR": "/a|b"}, "renderer"),
        ({"default_backend": "vz", "vz": {"instance_dir": "/x/instances"}}, {"SHED_DEV_STATE_DIR": " /a"}, "renderer"),
    ],
    ids=["not-vz", "no-instance-dir", "relative", "relative-state-dir", "unsafe-state-dir", "padded-state-dir"],
)
def test_dev_bindings_store_refuses_to_guess(config, environ, match):
    with pytest.raises(RuntimeError, match=match):
        dc.dev_bindings_store(config, environ)


_REAL_DEV_BINDINGS_STORE = dc.dev_bindings_store


def test_dev_config_refuses_before_stopping_when_the_store_cannot_be_kept(monkeypatch, tmp_path):
    calls = _fake_dev_control(monkeypatch, tmp_path)
    # The real location rule, on a home under tmp_path.
    monkeypatch.setattr(dc, "dev_bindings_store", _REAL_DEV_BINDINGS_STORE)
    monkeypatch.setenv("HOME", str(tmp_path))
    monkeypatch.delenv("SHED_DEV_STATE_DIR", raising=False)
    # The state directory does not exist: where the store is cannot be told.
    with pytest.raises(RuntimeError, match="does not exist"):
        with dc.dev_config({}, "my-server-dev"):
            pytest.fail("the detour ran")
    # An override that moves instance_dir moves the store: refused.
    (tmp_path / "Library/Application Support/shed-dev/vz").mkdir(parents=True)
    with pytest.raises(RuntimeError, match="moves its source bindings"):
        with dc.dev_config({"vz": {"instance_dir": str(tmp_path / "other/instances")}}, "my-server-dev"):
            pytest.fail("the detour ran")
    assert calls["events"] == [] and calls["restarts"] == []


def test_api_info_open_mode_is_plain_http(monkeypatch):
    seen = []

    class _Resp(io.BytesIO):
        status = 200

        def __enter__(self):
            return self

        def __exit__(self, *a):
            return False

    def urlopen(url, timeout=None, context=None):
        seen.append(url)
        return _Resp(b'{"name":"x","auth_mode":"open"}')

    monkeypatch.setattr(dc, "resolve_server_entry", lambda name: {"host": "localhost"})
    monkeypatch.setattr(dc.urllib.request, "urlopen", urlopen)
    monkeypatch.setattr(dc, "bootstrap_cert", lambda *a, **k: pytest.fail("open mode must not enrol"))
    info = dc.api_info("my-server-dev", dc._merge_config(dc.OPEN_MODE_OVERRIDES), Path("/nonexistent"))
    assert info["auth_mode"] == "open"
    assert seen == ["http://localhost:18080/api/info"]


def test_api_info_mtls_presents_a_bootstrapped_certificate(monkeypatch, tmp_path):
    cert = dc.BootstrapCert(
        cert_path=tmp_path / "c.pem", key_path=tmp_path / "c.key", pin="sha256:" + "ab" * 32,
        https_port=18443, scope="control", bundle={},
    )
    seen = []
    monkeypatch.setattr(dc, "resolve_server_entry", lambda name: {"host": "localhost"})
    monkeypatch.setattr(dc, "bootstrap_cert", lambda server, scope, directory: seen.append(("enrol", scope)) or cert)
    monkeypatch.setattr(
        dc, "pinned_request",
        lambda host, port, path, pin, pair: seen.append((host, port, path, pin, pair)) or (200, {"auth_mode": "mtls"}),
    )
    info = dc.api_info("my-server-dev", dc._merge_config({}), tmp_path)
    assert info == {"auth_mode": "mtls"}
    assert seen == [("enrol", "control"), ("localhost", 18443, "/api/info", cert.pin, cert.pair)]


def test_bootstrap_cert_sends_one_csr_and_writes_the_issued_cert(monkeypatch, tmp_path):
    if shutil.which("openssl") is None:
        pytest.skip("openssl is not installed")
    sent = []
    real_run = subprocess.run

    def run(argv, **kw):
        if argv[0] != "ssh":
            return real_run(argv, **kw)
        sent.append(argv)
        bundle = {
            "auth_mode": "mtls", "https_port": 18443, "tls_cert_fingerprint": "sha256:" + "cd" * 32,
            "client_cert": "-----BEGIN CERTIFICATE-----\nAAAA\n-----END CERTIFICATE-----\n",
            "scope": "credentials",
        }
        return subprocess.CompletedProcess(argv, 0, stdout=json.dumps(bundle) + "\n", stderr="")

    monkeypatch.setattr(dc, "assert_dev_target", lambda name: {"host": "localhost", "ssh_port": 12222})
    monkeypatch.setattr(dc.subprocess, "run", run)
    got = dc.bootstrap_cert("my-server-dev", "credentials", tmp_path / "c", "broker")

    assert len(sent) == 1
    argv = sent[0]
    assert argv[argv.index("-p") + 1] == "12222"
    assert "StrictHostKeyChecking=yes" in argv and f"UserKnownHostsFile={dc.KNOWN_HOSTS_PATH}" in argv
    # The request line: scope, kind, exactly one csr= in standard base64 that
    # decodes to a DER CSR openssl verifies.
    target = argv.index("_bootstrap@localhost")
    request = argv[target + 1:]
    assert request[:2] == ["credentials", "broker"]
    csr_args = [a for a in request if a.startswith("csr=")]
    assert len(csr_args) == 1 and len(request) == 3
    der = base64.b64decode(csr_args[0][len("csr="):], validate=True)
    (tmp_path / "check.der").write_bytes(der)
    v = real_run(["openssl", "req", "-inform", "DER", "-in", str(tmp_path / "check.der"), "-noout", "-verify"],
                 capture_output=True, text=True)
    assert v.returncode == 0, v.stderr
    # What came back is on disk, with the pin and port the bundle named.
    assert got.cert_path.read_text().startswith("-----BEGIN CERTIFICATE-----")
    assert got.pin == "sha256:" + "cd" * 32 and got.https_port == 18443
    assert stat.S_IMODE(got.key_path.stat().st_mode) == 0o600


def test_bootstrap_cert_refuses_a_bundle_that_is_not_mtls(monkeypatch, tmp_path):
    if shutil.which("openssl") is None:
        pytest.skip("openssl is not installed")
    real_run = subprocess.run

    def run(argv, **kw):
        if argv[0] != "ssh":
            return real_run(argv, **kw)
        # What a 0.9.x token-mode server would have answered.
        token = {"auth_mode": "token", "https_port": 18443, "tls_cert_fingerprint": "sha256:x", "token": "shed_control_x"}
        return subprocess.CompletedProcess(argv, 0, stdout=json.dumps(token), stderr="")

    monkeypatch.setattr(dc, "assert_dev_target", lambda name: {"host": "localhost", "ssh_port": 12222})
    monkeypatch.setattr(dc.subprocess, "run", run)
    with pytest.raises(AssertionError, match="not an mtls bundle"):
        dc.bootstrap_cert("my-server-dev", "control", tmp_path)


# ----------------------------------------------------------------------------
# fixtures/tlsclient.py: pinned_request against a real local TLS listener that
# requires a client certificate, as an mtls shed-server does.
# ----------------------------------------------------------------------------


def _openssl(*args: str) -> None:
    r = subprocess.run(["openssl", *args], capture_output=True, text=True, timeout=30)
    assert r.returncode == 0, r.stderr


@pytest.fixture
def mtls_listener(tmp_path):
    """A loopback HTTPS server that requires a client certificate signed by
    its own throwaway CA, and answers every GET with a small JSON body."""
    if shutil.which("openssl") is None:
        pytest.skip("openssl is not installed")
    import http.server
    import ssl
    import threading

    d = tmp_path / "pki"
    d.mkdir()
    for name in ("server", "ca", "client", "stranger"):
        _openssl("ecparam", "-name", "prime256v1", "-genkey", "-noout", "-out", str(d / f"{name}.key"))
    for name in ("server", "ca", "stranger"):
        _openssl("req", "-new", "-x509", "-sha256", "-key", str(d / f"{name}.key"), "-subj", f"/CN={name}",
                 "-days", "1", "-out", str(d / f"{name}.pem"))
    _openssl("req", "-new", "-key", str(d / "client.key"), "-subj", "/CN=client", "-out", str(d / "client.csr"))
    _openssl("x509", "-req", "-sha256", "-in", str(d / "client.csr"), "-CA", str(d / "ca.pem"), "-CAkey", str(d / "ca.key"),
             "-CAcreateserial", "-days", "1", "-out", str(d / "client.pem"))

    class Handler(http.server.BaseHTTPRequestHandler):
        def do_GET(self):
            body = b'{"ok":true}'
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(body)))
            self.end_headers()
            self.wfile.write(body)

        def log_message(self, *a):
            pass

    ctx = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
    ctx.load_cert_chain(str(d / "server.pem"), str(d / "server.key"))
    ctx.load_verify_locations(str(d / "ca.pem"))
    ctx.verify_mode = ssl.CERT_REQUIRED
    srv = http.server.HTTPServer(("127.0.0.1", 0), Handler)
    srv.socket = ctx.wrap_socket(srv.socket, server_side=True)
    t = threading.Thread(target=srv.serve_forever, daemon=True)
    t.start()
    pin = tlsclient.fingerprint((d / "server.pem").read_text())
    yield {"port": srv.server_address[1], "pin": pin, "dir": d}
    srv.shutdown()
    srv.server_close()


def test_pinned_request_with_a_client_cert(mtls_listener):
    d = mtls_listener["dir"]
    status, body = tlsclient.pinned_request(
        "127.0.0.1", mtls_listener["port"], "/api/info", mtls_listener["pin"],
        (str(d / "client.pem"), str(d / "client.key")),
    )
    assert (status, body) == (200, {"ok": True})


@pytest.mark.parametrize("who", ["none", "stranger"], ids=["no-client-cert", "cert-from-another-ca"])
def test_pinned_request_refused_without_an_issued_cert_is_no_response(mtls_listener, who):
    d = mtls_listener["dir"]
    cert = None if who == "none" else (str(d / "stranger.pem"), str(d / "stranger.key"))
    assert tlsclient.pinned_request("127.0.0.1", mtls_listener["port"], "/", mtls_listener["pin"], cert) == (None, None)


def test_pinned_request_raises_on_the_wrong_server(mtls_listener):
    d = mtls_listener["dir"]
    with pytest.raises(tlsclient.PinMismatch):
        tlsclient.pinned_request(
            "127.0.0.1", mtls_listener["port"], "/", "sha256:" + "00" * 32,
            (str(d / "client.pem"), str(d / "client.key")),
        )


# ----------------------------------------------------------------------------
# The dev-server targets: settings guards that run before any stop, and
# scripts/dev-server-probe.sh, the readiness probe dev-server-up[-fc|-remote]
# and dev_config() poll. A real `make` and a real probe script; ssh, scp,
# sudo, go and uname are stubs that only record their argv, and the "server"
# is a sleep process plus loopback sockets.
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


def _make_with_stubs(tmp_path: Path, target: str, *assigns: str, env_extra: dict | None = None,
                     dry_run: bool = False) -> tuple[subprocess.CompletedProcess, list[str]]:
    """Run `make -j8 <target>` in the repo with every outward-reaching
    command stubbed. Returns make's result and the stubs' recorded calls."""
    if shutil.which("make") is None:
        pytest.skip("make is not installed")
    stubs = tmp_path / "stubs"
    stubs.mkdir(exist_ok=True)
    (stubs / "stub").write_text(_STUB)
    (stubs / "stub").chmod(0o755)
    for name in ("ssh", "scp", "sudo", "go", "uname"):
        if not (stubs / name).exists():
            (stubs / name).symlink_to("stub")
    calls = tmp_path / "calls.log"
    calls.write_text("")
    env = {k: v for k, v in os.environ.items() if not k.startswith(("SHED_", "MAKE"))}
    env.update(PATH=f"{stubs}:{os.environ['PATH']}", STUB_LOG=str(calls), HOME=str(tmp_path / "home"))
    env.update(env_extra or {})
    r = subprocess.run(
        ["make", *(["-n"] if dry_run else []), "-j8", "--no-print-directory", "-C", str(dc.REPO_ROOT), target,
         "FC_REMOTE_HOST=probe.invalid", "GOARCH=amd64", "RELEASE_BUILD_TOOLS_REF=x", *assigns],
        capture_output=True, text=True, timeout=120, env=env,
    )
    return r, calls.read_text().splitlines()


_REMOVED_KNOB = "SHED_DEV_AUTH_MODE was removed"


@pytest.mark.parametrize("value", ["token", "mtls"])
@pytest.mark.parametrize(
    "target", ["dev-server-restart-fc", "dev-server-up-fc", "test-integration-dev-fc",
               "dev-server-restart", "dev-server-up", "test-integration-dev"],
)
def test_a_leftover_auth_knob_fails_before_anything_runs(tmp_path, target, value):
    r, calls = _make_with_stubs(tmp_path, target, f"SHED_DEV_AUTH_MODE={value}")
    assert r.returncode != 0
    out = r.stdout + r.stderr
    # One line says it.
    assert len([line for line in out.splitlines() if _REMOVED_KNOB in line]) == 1, out
    # Nothing past the guard ran: no stop (ssh kill, or the Mac target's
    # `uname -s`), no upload, no launch. (A sibling build prerequisite may
    # have started beside the guard under -j; it stops nothing.)
    assert not [c for c in calls if "kill" in c or "serve --config" in c or c.startswith(("scp ", "uname -s"))], calls
    if target.startswith(("dev-server-restart", "test-integration-dev-fc")):
        assert calls == [], calls


def test_fc_restart_stops_before_it_builds_or_starts(tmp_path):
    r, calls = _make_with_stubs(tmp_path, "dev-server-restart-fc")
    assert r.returncode == 0, r.stdout + r.stderr
    stop = next(i for i, c in enumerate(calls) if "kill -TERM" in c)
    arch = next(i for i, c in enumerate(calls) if "uname -m" in c)
    launch = next(i for i, c in enumerate(calls) if "serve --config" in c)
    assert stop == 0
    assert stop < arch < launch


def test_mac_restart_reaches_the_stop_without_the_knob(tmp_path):
    # The control for the knob test: unset, the stop does run (the uname stub
    # says Linux, so dev-server-down refuses right there).
    r, calls = _make_with_stubs(tmp_path, "dev-server-restart")
    assert r.returncode != 0
    assert "dev-server-down is macOS-only" in r.stdout + r.stderr
    assert calls == ["uname -s"]


def _remote_home(tmp_path: Path) -> tuple[Path, int]:
    """A $HOME with a remote dev pid file naming a live process that is not
    a shed-server, so a stop that ran would remove the pid file (and send
    no signal: dev-server-down-remote checks the process name first)."""
    run_dir = tmp_path / "home" / ".shed" / "dev-remote"
    run_dir.mkdir(parents=True)
    proc = subprocess.Popen(["sleep", "60"])
    (run_dir / "server.pid").write_text(f"{proc.pid}\n")
    return run_dir / "server.pid", proc


# A remote dev env file that passes the settings guard: the reference
# service on loopback, a local admin key, the local transport.
_REMOTE_ENV_OK = (
    "SHED_REMOTE_DIRECTORY_URL=http://127.0.0.1:18700\n"
    "SHED_REMOTE_ADMIN_KEY='ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOk7m0g4b0dJ7J8cAqXqv8k3m1m2m3m4m5m6m7m8m9m0 dev'\n"
    "SHED_REMOTE_TRANSPORT=local\n"
)


@pytest.mark.parametrize(
    "env_file, want",
    [
        (None, "no env file"),
        ("SHED_REMOTE_SSH_KEY_FILE=x\n", "must set SHED_REMOTE_DIRECTORY_URL"),
        ("SHED_REMOTE_DIRECTORY_URL=http://accounts.example.test\n", "must set SHED_REMOTE_DIRECTORY_URL"),
        (_REMOTE_ENV_OK + "SHED_REMOTE_TRANSPORT=tailscale\n", "want ssh or local"),
        (_REMOTE_ENV_OK + "SHED_REMOTE_FIREWALL_MODE=auto\n", "needs SHED_REMOTE_ECHO_URLS"),
    ],
    ids=["missing-env-file", "no-directory-url", "plain-http-off-loopback", "unknown-transport", "auto-without-echo-urls"],
)
def test_remote_restart_refuses_bad_settings_before_stopping(tmp_path, env_file, want):
    pid_file, proc = _remote_home(tmp_path)
    try:
        path = tmp_path / "remote.env"
        if env_file is not None:
            path.write_text(env_file)
        r, calls = _make_with_stubs(tmp_path, "dev-server-restart-remote", f"SHED_REMOTE_ENV_FILE={path}")
        assert r.returncode != 0
        assert want in r.stdout + r.stderr
        # The stop never ran: the pid file of the "running server" is untouched.
        assert pid_file.exists(), "dev-server-down-remote ran before the settings were checked"
        assert proc.poll() is None
    finally:
        proc.kill()
        proc.wait()


def test_remote_restart_stops_then_starts_in_its_recipe(tmp_path):
    # Dry run: make prints the recipes and recurses with -n, so nothing runs.
    env = tmp_path / "remote.env"
    env.write_text(_REMOTE_ENV_OK)
    r, _ = _make_with_stubs(tmp_path, "dev-server-restart-remote", f"SHED_REMOTE_ENV_FILE={env}", dry_run=True)
    assert r.returncode == 0, r.stdout + r.stderr
    out = r.stdout
    stop = out.index("kill -TERM")
    launch = out.index("nohup bin/shed-server serve")
    assert stop < launch
    # Readiness is the probe over this run's part of the log, not the
    # readiness line alone.
    probe = out.index("scripts/dev-server-probe.sh")
    assert launch < probe
    assert '"$START_LINE"' in out[probe:probe + 300]


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


def test_probe_reads_only_from_the_first_line(fake_server):
    # dev-server-up-remote appends to its log: the probe must not take an
    # earlier run's readiness, or an earlier run's listener error, for this
    # run's. Lines 1-3 are an earlier run that failed; this run starts at 4.
    log = fake_server["log"]
    current = log.read_text()
    log.write_text(
        "2026/11/13 09:00:00 Shed server is ready\n"
        "2026/11/13 09:00:00 Server error: HTTPS server error: listen tcp: address already in use\n"
        "=== dev-server-up-remote 2026-11-14T10:00:00Z ===\n"
        + current
    )
    # The whole file: the earlier run's error fails it.
    assert _probe(log, fake_server["pid"]).returncode == 2
    # From this run's first line: ready.
    r = subprocess.run(["bash", str(PROBE), str(log), str(fake_server["pid"]), "0", "4"],
                       capture_output=True, text=True, timeout=30)
    assert r.returncode == 0, r.stdout
    # And an earlier run's readiness line does not stand in for this run's.
    log.write_text("2026/11/13 09:00:00 Shed server is ready\n" + current.replace("Shed server is ready", "still starting"))
    r = subprocess.run(["bash", str(PROBE), str(log), str(fake_server["pid"]), "0", "2"],
                       capture_output=True, text=True, timeout=30)
    assert r.returncode == 1, r.stdout


@pytest.mark.parametrize("first", ["x", "-1", "1.5"])
def test_probe_refuses_a_malformed_first_line(fake_server, first):
    r = subprocess.run(["bash", str(PROBE), str(fake_server["log"]), str(fake_server["pid"]), "0", first],
                       capture_output=True, text=True, timeout=30)
    assert r.returncode == 64, r.stdout + r.stderr


def test_wait_probe_returns_ready_and_raises_on_a_dead_server(fake_server, tmp_path):
    pid_file = tmp_path / "server.pid"
    pid_file.write_text(f"{fake_server['pid']}\n")
    assert dc._wait_probe(10, log=fake_server["log"], pid_path=pid_file).startswith("ready:")
    fake_server["proc"].kill()
    fake_server["proc"].wait()
    start = time.monotonic()
    with pytest.raises(AssertionError, match="failed"):
        dc._wait_probe(30, log=fake_server["log"], pid_path=pid_file)
    # A dead server fails at once, not at the deadline.
    assert time.monotonic() - start < 10


def test_wait_probe_raises_at_once_on_a_status_the_probe_does_not_define(fake_server, tmp_path, monkeypatch):
    # A probe that refuses its arguments (64) is a broken check: the fixture
    # must fail on it at once, not poll it as "not yet" until the deadline
    # and then blame a slow start.
    broken = tmp_path / "probe.sh"
    broken.write_text("#!/bin/bash\necho 'usage: probe <log> <pid>' >&2\nexit 64\n")
    monkeypatch.setattr(dc, "PROBE_SCRIPT", broken)
    pid_file = tmp_path / "server.pid"
    pid_file.write_text(f"{fake_server['pid']}\n")
    start = time.monotonic()
    with pytest.raises(AssertionError, match="unexpected status 64") as exc:
        dc._wait_probe(30, log=fake_server["log"], pid_path=pid_file)
    assert "usage: probe" in str(exc.value)
    assert time.monotonic() - start < 5


# ----------------------------------------------------------------------------
# scripts/dev-server-wait.sh: the poll loop every dev-server-up target runs
# the probe through. A status the probe does not define is a failure.
# ----------------------------------------------------------------------------

WAIT = dc.REPO_ROOT / "scripts" / "dev-server-wait.sh"


def _status_script(tmp_path: Path, *statuses: int) -> Path:
    """A command that exits with statuses[n] on its n-th run (the last one
    repeating), printing which run it is."""
    counter = tmp_path / "runs"
    counter.write_text("0")
    script = tmp_path / "probe-stub.sh"
    seq = " ".join(str(s) for s in statuses)
    script.write_text(
        "#!/bin/bash\n"
        f"n=$(( $(cat {counter}) + 1 )); echo $n > {counter}\n"
        f"set -- {seq}\n"
        'i=$n; [ "$i" -le "$#" ] || i=$#\n'
        'rc=${!i}\n'
        'echo "run $n status $rc"\n'
        'exit "$rc"\n'
    )
    script.chmod(0o755)
    return script


def _wait(*args, timeout: float = 30) -> subprocess.CompletedProcess:
    return subprocess.run(["bash", str(WAIT), *map(str, args)], capture_output=True, text=True, timeout=timeout)


@pytest.mark.parametrize(
    "statuses, want_rc, want_out",
    [
        ((0,), 0, "run 1 status 0 (try 1 of 3)"),
        ((1, 1, 0), 0, "run 3 status 0 (try 3 of 3)"),
        ((1, 2), 2, "run 2 status 2"),
        ((1,), 1, "run 3 status 1"),
        ((64,), 2, "probe exited with unexpected status 64: run 1 status 64"),
        ((1, 127), 2, "probe exited with unexpected status 127: run 2 status 127"),
        ((255,), 2, "probe exited with unexpected status 255: run 1 status 255"),
    ],
    ids=["ready", "ready-on-the-last-try", "failed", "timed-out", "usage-is-a-failure",
         "missing-command-is-a-failure", "ssh-failure-without-retry"],
)
def test_wait_reads_the_probe_status(tmp_path, statuses, want_rc, want_out):
    r = _wait(3, "--", _status_script(tmp_path, *statuses))
    assert r.returncode == want_rc, r.stdout + r.stderr
    assert r.stdout.strip() == want_out, r.stdout


def test_wait_does_not_retry_an_unexpected_status(tmp_path):
    start = time.monotonic()
    r = _wait(10, "--", _status_script(tmp_path, 64))
    assert r.returncode == 2
    assert (tmp_path / "runs").read_text().strip() == "1"
    assert time.monotonic() - start < 3


def test_wait_retries_a_named_status(tmp_path):
    # dev-server-up-fc's ssh: 255 means the host did not answer this second.
    r = _wait("--retry", 255, 3, "--", _status_script(tmp_path, 255, 0))
    assert r.returncode == 0, r.stdout
    assert r.stdout.strip() == "run 2 status 0 (try 2 of 3)"


@pytest.mark.parametrize("args", [[], ["x", "--", "true"], ["0", "--", "true"], ["3", "true"], ["--retry", "x", "3", "--", "true"]])
def test_wait_refuses_bad_arguments(args):
    assert _wait(*args).returncode == 64


def test_wait_feeds_the_probe_on_stdin_as_the_fc_target_does(fake_server):
    # The FC target pipes the probe to `bash -s` over ssh; this is that
    # without the ssh.
    r = _wait("--stdin", PROBE, 3, "--", "bash", "-s", "--", fake_server["log"], fake_server["pid"], "0")
    assert r.returncode == 0, r.stdout
    assert r.stdout.startswith("ready: listening on")


@pytest.mark.parametrize("first", ["       4", "4 "])
def test_wait_fails_the_real_probe_given_a_padded_first_line(fake_server, first):
    # The review's case: a space-padded count is refused by the probe (64),
    # and the wait must report that as a failure, not wait out its tries.
    start = time.monotonic()
    r = _wait(150, "--", "bash", PROBE, fake_server["log"], fake_server["pid"], "0", first)
    assert r.returncode == 2, r.stdout
    assert "unexpected status 64" in r.stdout and "usage" in r.stdout
    assert time.monotonic() - start < 5


def _makefile_logical_lines() -> list[str]:
    """The Makefile's lines with backslash continuations joined."""
    out, cur = [], ""
    for line in (dc.REPO_ROOT / "Makefile").read_text().splitlines():
        if line.endswith("\\"):
            cur += line[:-1] + " "
            continue
        out.append(cur + line)
        cur = ""
    return out


def test_every_dev_server_up_target_waits_through_the_shared_loop():
    lines = [l for l in _makefile_logical_lines() if not l.lstrip().lstrip("@").startswith("#")]
    probes = [l for l in lines if "dev-server-probe.sh" in l]
    # The Mac, FC and remote targets, each once, each through the wait script.
    assert len(probes) == 3, probes
    for line in probes:
        assert "scripts/dev-server-wait.sh" in line, line
        # 0 ready, 1 timed out, anything else fails the target.
        tail = line[line.index("dev-server-wait.sh"):]
        assert 'case "$$RC" in' in tail, line
        assert "*)" in tail, f"no catch-all failure branch: {line}"
        star = tail.index("*)")
        assert "exit 1" in tail[star:], line
        assert tail.index("0)") < tail.index("1)") < star, line
    fc = next(l for l in probes if "--stdin scripts/dev-server-probe.sh" in l)
    assert "--retry 255" in fc


def test_remote_start_line_survives_a_padded_wc(tmp_path):
    # Take the remote target's own START_LINE assignment from a dry run and
    # run it with a wc that pads its count the way macOS wc does.
    env = tmp_path / "remote.env"
    env.write_text(_REMOTE_ENV_OK)
    r, _ = _make_with_stubs(tmp_path, "dev-server-up-remote", f"SHED_REMOTE_ENV_FILE={env}", dry_run=True)
    assert r.returncode == 0, r.stdout + r.stderr
    m = re.search(r"START_LINE=(\$\([^;]*\));", r.stdout)
    assert m, r.stdout
    log = tmp_path / "home" / ".shed" / "dev-remote" / "server.log"
    log.parent.mkdir(parents=True, exist_ok=True)
    log.write_text("a\nb\nc\nd\n")
    padded = tmp_path / "padded"
    padded.mkdir()
    (padded / "wc").write_text('#!/bin/bash\ncat >/dev/null\nprintf "       4\\n"\n')
    (padded / "wc").chmod(0o755)
    run = subprocess.run(
        ["bash", "-c", f'START_LINE={m.group(1)}; printf "[%s]" "$START_LINE"'],
        capture_output=True, text=True, timeout=30,
        env={**os.environ, "PATH": f"{padded}:{os.environ['PATH']}"},
    )
    assert run.stdout == "[4]", run.stdout + run.stderr


# ----------------------------------------------------------------------------
# Named users: fixtures/identity.py (keys, the user list, an identity's ssh
# shim), fixtures/busclient.py (the synthetic bus listener), render-dev-config
# --users, and the Makefile's SHED_DEV_USERS knob. No server: keys are made
# in tmp_path, configs are checked with `shed-server config-validate`, and the
# bus listener talks to a local TLS server.
# ----------------------------------------------------------------------------

from fixtures import busclient  # noqa: E402
from fixtures import identity as ident  # noqa: E402

REMOTE_TEMPLATE = dc.REPO_ROOT / "configs" / "server.dev-remote.yaml.tmpl"


@pytest.fixture
def keys_dir(tmp_path):
    if shutil.which("ssh-keygen") is None:
        pytest.skip("ssh-keygen is not installed")
    return ident.ensure_keys(tmp_path / "keys")


def test_ensure_keys_is_idempotent_and_never_replaces_a_key(keys_dir):
    before = {p: p.read_bytes() for p in keys_dir.rglob("*_ed25519*")}
    assert len(before) == 2 * len(ident.KEYS)
    assert ident.ensure_keys(keys_dir) == keys_dir
    assert {p: p.read_bytes() for p in keys_dir.rglob("*_ed25519*")} == before
    # Half a pair is refused, never regenerated: a server may hold the other half.
    (keys_dir / ident.BOB / "id_ed25519.pub").unlink()
    with pytest.raises(RuntimeError, match="only one half"):
        ident.ensure_keys(keys_dir)


def test_itest_users_are_two_users_and_an_admin(keys_dir):
    users = ident.itest_users(keys_dir)
    assert set(users) == {ident.ADMIN, ident.ALICE, ident.BOB}
    assert [users[u]["admin"] for u in (ident.ADMIN, ident.ALICE, ident.BOB)] == [True, False, False]
    alice = {k["label"]: k for k in users[ident.ALICE]["keys"]}
    bob = {k["label"]: k for k in users[ident.BOB]["keys"]}
    assert alice["itest-script"]["script"] is True and "script" not in alice["itest"]
    assert bob["itest-expired"]["expires"] == ident.EXPIRED_ON and "expires" not in bob["itest"]
    keys = [k["key"] for u in users.values() for k in u["keys"]]
    assert len(set(keys)) == len(ident.KEYS), "a key is listed twice"
    assert all(k.startswith("ssh-ed25519 ") and len(k.split()) == 2 for k in keys), keys
    assert alice["itest"]["key"] == ident.public_key(ident.ALICE, ident.PRIMARY_KEY, keys_dir)


@pytest.mark.parametrize("path", [MAC_DEV_CONFIG, FC_DEV_CONFIG], ids=["mac", "fc"])
def test_with_itest_users_moves_the_developer_key_sources_under_dev(path, keys_dir):
    base = _load_yaml(path)
    out = ident.with_itest_users(base, keys_dir)
    assert base == _load_yaml(path), "the input config was changed"
    auth = out["auth"]
    assert auth["mode"] == "mtls"
    # Config load refuses auth.ssh key sources beside auth.users.
    assert not {"github_users", "authorized_keys", "authorized_keys_file"} & set(auth.get("ssh") or {})
    users = auth["users"]
    assert set(users) == {ident.DEV_USER, ident.ADMIN, ident.ALICE, ident.BOB}
    dev = users[ident.DEV_USER]
    assert dev["admin"] is True and dev["github"] == base["auth"]["ssh"]["github_users"][0]
    for line in base["auth"]["ssh"].get("authorized_keys") or []:
        assert " ".join(line.split()[:2]) in [k["key"] for k in dev["keys"]], line
    assert auth.get("clients") == base["auth"].get("clients")
    # Everything outside auth is untouched.
    assert {k: v for k, v in out.items() if k != "auth"} == {k: v for k, v in base.items() if k != "auth"}


def test_with_users_keeps_other_ssh_settings_and_refuses_what_it_cannot_do(keys_dir):
    base = {"auth": {"mode": "mtls", "ssh": {"github_users": ["someone"], "max_auth_tries": 3}}}
    out = ident.with_itest_users(base, keys_dir)
    assert out["auth"]["ssh"] == {"max_auth_tries": 3}
    with pytest.raises(ValueError, match="mtls"):
        ident.with_itest_users({"auth": {"mode": "open", "ssh": {"github_users": ["x"]}}}, keys_dir)
    with pytest.raises(ValueError, match="already has auth.users"):
        ident.with_itest_users(out, keys_dir)
    with pytest.raises(ValueError, match="no auth.ssh key source"):
        ident.with_itest_users({"auth": {"mode": "mtls"}}, keys_dir)
    clash = {"auth": {"mode": "mtls", "ssh": {"github_users": ["x"]}, "clients": {ident.ALICE: {}}}}
    with pytest.raises(ValueError, match="service-client"):
        ident.with_itest_users(clash, keys_dir)
    # Only the named itest users beside `dev`.
    assert set(ident.with_itest_users(base, keys_dir, names=())["auth"]["users"]) == {ident.DEV_USER}
    assert set(ident.with_itest_users(base, keys_dir, names=(ident.ALICE,))["auth"]["users"]) == {
        ident.DEV_USER, ident.ALICE,
    }


def _validate(cfg: Path) -> subprocess.CompletedProcess:
    if not dc.SHED_SERVER_BIN.exists():
        pytest.skip(f"{dc.SHED_SERVER_BIN} is not built; run 'make build'")
    return subprocess.run([str(dc.SHED_SERVER_BIN), "config-validate", "--config", str(cfg)],
                          capture_output=True, text=True, timeout=60)


def _rendered_remote_template(tmp_path: Path) -> Path:
    """The remote dev template with its placeholders filled the way
    dev-server-up-remote fills them (dummy values; nothing private)."""
    text = REMOTE_TEMPLATE.read_text(encoding="utf-8")
    for k, v in {
        "@DEV_REMOTE_DIR@": str(tmp_path / "dev-remote"),
        "@ADMIN_KEY@": "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOk7m0g4b0dJ7J8cAqXqv8k3m1m2m3m4m5m6m7m8m9m0",
        "@DIRECTORY_URL@": "http://127.0.0.1:18700", "@DIRECTORY_SETTING@": "allow_plain_http: true",
        "@TRANSPORT@": "local", "@TRANSPORT_SETTING@": "allow_test_drivers: true",
        "@FIREWALL_MODE@": "static", "@FIREWALL_SETTING@": "source_ranges_env: SHED_REMOTE_SOURCE_RANGES",
    }.items():
        text = text.replace(k, v)
    out = tmp_path / "remote.yaml"
    out.write_text(text, encoding="utf-8")
    return out


def test_rendered_remote_template_loads(tmp_path):
    # The remote dev template, rendered as dev-server-up-remote renders it
    # against the reference service: the account service's compute with the
    # local transport, a local admin and the directory.
    v = _validate(_rendered_remote_template(tmp_path))
    assert v.returncode == 0, v.stdout + v.stderr


@pytest.mark.parametrize("which", ["mac", "fc"])
def test_render_users_writes_a_config_the_server_loads(tmp_path, keys_dir, which):
    if shutil.which("uv") is None:
        pytest.skip("uv is not installed")
    base = {"mac": MAC_DEV_CONFIG, "fc": FC_DEV_CONFIG}[which]
    r = _render(tmp_path, base, "--users", str(keys_dir))
    assert r.returncode == 0, r.stdout + r.stderr
    out = tmp_path / "out.yaml"
    got = _load_yaml(out)
    assert set(got["auth"]["users"]) == {ident.DEV_USER, ident.ADMIN, ident.ALICE, ident.BOB}
    assert got["auth"]["users"][ident.ALICE]["keys"][0]["key"] == ident.public_key(ident.ALICE, keys_dir=keys_dir)
    assert not (tmp_path / "out.yaml.users").exists()
    # The server loads it (a vz config loads on macOS only).
    if which == "mac" and os.uname().sysname != "Darwin":
        return
    if which == "fc" and os.uname().sysname != "Linux":
        return
    v = _validate(out)
    assert v.returncode == 0, v.stdout + v.stderr
    # Control: the same config with the developer's github_users put back
    # beside the user list is refused, so the check above can fail.
    got["auth"]["ssh"] = {"github_users": ["someone"]}
    bad = tmp_path / "bad.yaml"
    bad.write_text(yaml.safe_dump(got), encoding="utf-8")
    v = _validate(bad)
    assert v.returncode != 0 and "auth.users cannot be combined" in v.stdout + v.stderr, v.stdout + v.stderr


def test_render_users_and_state_dir_together(tmp_path, keys_dir):
    if shutil.which("uv") is None:
        pytest.skip("uv is not installed")
    r = _render(tmp_path, FC_DEV_CONFIG, "--state-dir", "/var/lib/shed-x/firecracker", "--users", str(keys_dir))
    assert r.returncode == 0, r.stderr
    got = _load_yaml(tmp_path / "out.yaml")
    assert got["firecracker"]["instance_dir"] == "/var/lib/shed-x/firecracker/instances"
    assert ident.ALICE in got["auth"]["users"]


def test_render_users_needs_a_directory(tmp_path):
    r = _render(tmp_path, FC_DEV_CONFIG, "--users")
    assert r.returncode != 0 and "usage" in r.stderr


def test_dev_config_applies_a_transform_then_restores_the_base(monkeypatch, tmp_path, keys_dir):
    calls = _fake_dev_control(monkeypatch, tmp_path)
    with dc.dev_config({}, "my-server-dev", transform=lambda c: ident.with_itest_users(c, keys_dir)):
        written = _load_yaml(tmp_path / "test-dev-config.yaml")
        assert ident.ALICE in written["auth"]["users"] and "ssh" not in written["auth"]
    assert calls["restarts"] == [tmp_path / "test-dev-config.yaml", dc.DEV_BASE_CONFIG]
    assert "users" in calls["waits"][0]["auth"] and "users" not in calls["waits"][1]["auth"]


def test_identity_shim_runs_ssh_with_only_its_key(tmp_path, keys_dir):
    fake = tmp_path / "fake-ssh"
    fake.write_text('#!/bin/sh\nfor a in "$@"; do printf "%s\\n" "$a"; done\necho "AGENT=${SSH_AUTH_SOCK:-none}"\n')
    fake.chmod(0o755)
    key = ident.key_path(ident.ALICE, keys_dir=keys_dir)
    shim = tmp_path / "ssh"
    shim.write_text(ident.shim_text(ident.ALICE, key, str(fake)))
    shim.chmod(0o755)
    r = subprocess.run([str(shim), "-T", "-p", "12222", "x@localhost", "echo hi"], capture_output=True, text=True,
                       env={**os.environ, "SSH_AUTH_SOCK": "/tmp/an-agent"}, timeout=10)
    assert r.returncode == 0, r.stderr
    args = r.stdout.splitlines()
    assert args[-1] == "AGENT=none", "the shim let the developer's agent through"
    argv = args[:-1]
    # Its own options first (the first value ssh sees for an option wins),
    # then the caller's argv untouched.
    assert argv[:4] == ["-F", "/dev/null", "-i", str(key)]
    opts = {argv[i + 1] for i, a in enumerate(argv) if a == "-o"}
    assert {"IdentitiesOnly=yes", "IdentityAgent=none", "BatchMode=yes"} <= opts
    assert argv[-5:] == ["-T", "-p", "12222", "x@localhost", "echo hi"]


def test_identity_home_and_teardown(keys_dir):
    if shutil.which("ssh") is None:
        pytest.skip("ssh is not installed")
    a = ident.Identity.create(ident.ALICE, "box-dev", "localhost", 12222, keys_dir=keys_dir)
    assert a.home == keys_dir / ident.ALICE / "home-box-dev" and a.home.is_dir()
    assert os.access(a.shim, os.X_OK)
    env = a.env()
    assert env["HOME"] == str(a.home) and env["PATH"].startswith(str(a.shim.parent) + os.pathsep)
    assert "SSH_AUTH_SOCK" not in env
    argv = a.ssh_argv("some-shed", "true")
    assert argv[0] == str(a.shim) and f"UserKnownHostsFile={a.known_hosts}" in argv
    assert argv[-2:] == ["some-shed@localhost", "true"]
    assert a.known_hosts == a.home / ".shed" / "known_hosts"

    # Another key of the same user shares the HOME through its own shim.
    s = a.with_key(ident.SCRIPT_KEY)
    assert s.home == a.home and s.shim != a.shim and s.key.name == ident.SCRIPT_KEY
    s.remove()
    assert a.home.exists(), "removing a key variant removed the user's HOME"

    # A leftover HOME from a crashed run is replaced, not reused.
    (a.home / "stale").write_text("x")
    a2 = ident.Identity.create(ident.ALICE, "box-dev", "localhost", 12222, keys_dir=keys_dir)
    assert not (a2.home / "stale").exists()
    a2.remove()
    assert not a2.home.exists()
    assert ident.key_path(ident.ALICE, keys_dir=keys_dir).exists(), "teardown removed the key"


def test_identity_enrols_through_its_shim_and_known_hosts(monkeypatch, tmp_path, keys_dir):
    if shutil.which("openssl") is None or shutil.which("ssh") is None:
        pytest.skip("openssl or ssh is not installed")
    a = ident.Identity.create(ident.BOB, "box-dev", "localhost", 12222, keys_dir=keys_dir)
    sent = []
    real_run = subprocess.run

    def run(argv, **kw):
        if argv[0] != str(a.shim):
            return real_run(argv, **kw)
        sent.append(argv)
        return subprocess.CompletedProcess(argv, 255, stdout="", stderr="Permission denied (publickey).")

    monkeypatch.setattr(dc, "assert_dev_target", lambda name: {"host": "localhost", "ssh_port": 12222})
    monkeypatch.setattr(dc.subprocess, "run", run)
    try:
        with pytest.raises(AssertionError, match="Permission denied"):
            a.enrol("control", tmp_path / "c")
        assert len(sent) == 1 and f"UserKnownHostsFile={a.known_hosts}" in sent[0]
    finally:
        a.remove()


@pytest.mark.parametrize(
    "value, backend, want", [("fc", "fc", True), ("fc, remote", "remote", True), ("fc", "remote", False), ("", "fc", False)],
)
def test_started_with_users_reads_the_backend_list(monkeypatch, value, backend, want):
    monkeypatch.setenv("SHED_ITEST_USERS", value)
    assert ident.started_with_users(backend) is want
    if not want:
        with pytest.raises(pytest.skip.Exception, match=f"SHED_ITEST_USERS={backend}"):
            ident.skip_unless_started_with_users(backend, "box-dev")


def test_local_vm_only_users_cases_skip_on_remote_before_fixtures():
    import conftest

    class Item:
        def __init__(self, param, marked):
            self.callspec = type("C", (), {"params": {"users_env": param}})()
            self._marked = marked

        def get_closest_marker(self, name):
            return pytest.mark.local_vm_only("why").mark if self._marked and name == "local_vm_only" else None

    with pytest.raises(pytest.skip.Exception, match="not on the remote backend: why"):
        conftest.pytest_runtest_setup(Item("remote", True))
    conftest.pytest_runtest_setup(Item("remote", False))
    conftest.pytest_runtest_setup(Item("fc", True))


def test_parse_sse_reads_events_and_skips_keepalives():
    lines = [": keepalive\n", "\n", "event: message\n", 'data: {"id":"1"}\n', "\n",
             "data: a\n", "data: b\n", "\n", "event: message\n", "data: {}\n"]
    # The last event has no blank line after it: the stream ended mid-event.
    assert list(busclient.parse_sse(lines)) == [("message", '{"id":"1"}'), ("message", "a\nb")]


def test_response_body_answers_the_request_by_its_server_id():
    req = {"id": "srv-1", "namespace": "ns", "shed": {"name": "s1"}, "payload": {"x": 1}}
    body = busclient.response_body(req, {"ok": True})
    assert body == {"namespace": "ns", "type": "response", "in_reply_to": "srv-1", "final": True,
                    "payload": {"ok": True}, "shed": {"name": "s1"}}
    assert busclient.payload_has(req, "x")
    argv = busclient.publish_argv("ns", {"itest_probe": "t"})
    assert argv[-1] == "http://127.0.0.1:498/v1/publish"
    assert json.loads(argv[argv.index("--data") + 1]) == {"namespace": "ns", "type": "request", "payload": {"itest_probe": "t"}}


@pytest.fixture
def sse_listener(tmp_path):
    """A loopback HTTPS server requiring a client certificate that serves a
    credential-bus subscribe: on /api/plugins/listeners/ok/messages a
    keepalive, then one event, then it holds the stream until the client
    goes; any other namespace is refused with a 403 JSON error. `hang` never
    answers (for 5 s); `limited` is 429 SOURCE_RATE_LIMITED, Retry-After 7.

    The namespace `pin` answers as a pinning server does, for the machine id
    in `state["bound"]`: no machine id is 403 SOURCE_MACHINE_ID_REQUIRED,
    another machine 403 SOURCE_NOT_BOUND naming the bound one, the bound one
    is served like `ok`. A POST (an answer) is recorded in `posts` and is 204
    from the bound machine, the same refusals otherwise."""
    if shutil.which("openssl") is None:
        pytest.skip("openssl is not installed")
    import http.server
    import ssl
    import threading

    d = tmp_path / "pki"
    d.mkdir()
    for name in ("server", "ca", "client"):
        _openssl("ecparam", "-name", "prime256v1", "-genkey", "-noout", "-out", str(d / f"{name}.key"))
    for name in ("server", "ca"):
        _openssl("req", "-new", "-x509", "-sha256", "-key", str(d / f"{name}.key"), "-subj", f"/CN={name}",
                 "-days", "1", "-out", str(d / f"{name}.pem"))
    _openssl("req", "-new", "-key", str(d / "client.key"), "-subj", "/CN=client", "-out", str(d / "client.csr"))
    _openssl("x509", "-req", "-sha256", "-in", str(d / "client.csr"), "-CA", str(d / "ca.pem"), "-CAkey", str(d / "ca.key"),
             "-CAcreateserial", "-days", "1", "-out", str(d / "client.pem"))
    stop = threading.Event()
    seen_headers: list[dict] = []
    state = {"bound": busclient.machine_id(ident.ALICE), "bound_name": busclient.machine_name(ident.ALICE)}
    posts: list[dict] = []

    class Handler(http.server.BaseHTTPRequestHandler):
        protocol_version = "HTTP/1.1"

        def _json(self, status, obj):
            body = json.dumps(obj).encode()
            self.send_response(status)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(body)))
            self.end_headers()
            self.wfile.write(body)

        def _pin_refusal(self):
            """The pin's refusal for this request's machine, or None for the
            bound machine."""
            mid = self.headers.get(busclient.MACHINE_ID_HEADER)
            if not mid or not re.fullmatch(r"[0-9a-f]{32}", mid) or not self.headers.get(busclient.MACHINE_NAME_HEADER):
                return 403, {"error": {"code": "SOURCE_MACHINE_ID_REQUIRED", "message": "install the current shed desktop app"}}
            if mid != state["bound"]:
                return 403, {"error": {"code": "SOURCE_NOT_BOUND", "message": f"only {state['bound_name']}"},
                             "source": {"machine_id": state["bound"], "machine_name": state["bound_name"]}}
            return None

        def do_POST(self):
            raw = self.rfile.read(int(self.headers.get("Content-Length") or 0))
            posts.append({"path": self.path, "headers": dict(self.headers.items()), "body": json.loads(raw or b"null")})
            refusal = self._pin_refusal()
            if refusal is not None:
                self._json(*refusal)
                return
            self.send_response(204)
            self.send_header("Content-Length", "0")
            self.end_headers()

        def do_GET(self):
            seen_headers.append(dict(self.headers.items()))
            ns = self.path.removeprefix("/api/plugins/listeners/").removesuffix("/messages")
            if ns == "hang":
                stop.wait(5)  # never answers while the client waits
                return
            if ns == "limited":
                body = json.dumps({"error": {"code": "SOURCE_RATE_LIMITED", "message": "retry in 7 s"}}).encode()
                self.send_response(429)
                self.send_header("Retry-After", "7")
                self.send_header("Content-Type", "application/json")
                self.send_header("Content-Length", str(len(body)))
                self.end_headers()
                self.wfile.write(body)
                return
            if ns == "pin":
                refusal = self._pin_refusal()
                if refusal is not None:
                    self._json(*refusal)
                    return
            elif ns not in ("ok", "ends", "ka") or not self.path.endswith("/messages"):
                self._json(403, {"error": {"code": "FORBIDDEN", "message": "no"}})
                return
            self.send_response(200)
            self.send_header("Content-Type", "text/event-stream")
            self.send_header("Connection", "close")
            self.end_headers()
            env = {"id": "srv-1", "namespace": "ok", "payload": {"itest_probe": "tok"}, "shed": {"name": "s1"}}
            try:
                self.wfile.write(b": keepalive\n\n")
                self.wfile.write(b"event: message\ndata: " + json.dumps(env).encode() + b"\n\n")
                self.wfile.flush()
                if ns == "ends":
                    return  # the server ends the stream (Connection: close)
                if ns == "ka":
                    # A keepalive every 0.2 s until the client goes.
                    while not stop.wait(0.2):
                        self.wfile.write(b": keepalive\n\n")
                        self.wfile.flush()
                    return
                stop.wait(10)
            except OSError:
                pass

        def log_message(self, *a):
            pass

    ctx = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
    ctx.load_cert_chain(str(d / "server.pem"), str(d / "server.key"))
    ctx.load_verify_locations(str(d / "ca.pem"))
    ctx.verify_mode = ssl.CERT_REQUIRED
    srv = http.server.ThreadingHTTPServer(("127.0.0.1", 0), Handler)
    srv.daemon_threads = True
    srv.socket = ctx.wrap_socket(srv.socket, server_side=True)
    t = threading.Thread(target=srv.serve_forever, daemon=True)
    t.start()
    pin = tlsclient.fingerprint((d / "server.pem").read_text())
    yield {"port": srv.server_address[1], "pin": pin, "pair": (str(d / "client.pem"), str(d / "client.key")),
           "headers": seen_headers, "state": state, "posts": posts}
    stop.set()
    srv.shutdown()
    srv.server_close()


def test_bus_listener_receives_events_and_closes(sse_listener):
    s = sse_listener
    with busclient.BusListener("127.0.0.1", s["port"], s["pin"], s["pair"], "ok") as listener:
        assert listener.open() == (200, None)
        got = listener.wait_for(lambda e: busclient.payload_has(e, "tok"), 10)
        assert got is not None and got["id"] == "srv-1", listener.events
        assert listener.wait_for(lambda e: busclient.payload_has(e, "nothing-like-it"), 0.2) is None
    assert listener.error is None


def test_bus_listener_counts_a_stream_that_ends_as_broken(sse_listener):
    # A listener whose stream the server ended hears nothing more: it must
    # not pass as a listener that stayed quiet.
    s = sse_listener
    with busclient.BusListener("127.0.0.1", s["port"], s["pin"], s["pair"], "ends") as listener:
        assert listener.open() == (200, None)
        assert listener.wait_for(lambda e: busclient.payload_has(e, "tok"), 10) is not None
        deadline = time.monotonic() + 10
        while listener.error is None and time.monotonic() < deadline:
            time.sleep(0.05)
        assert listener.error is not None and "ended" in listener.error, listener.error
        assert not listener.alive_after(time.monotonic() - 60, 0.5), "an ended stream counted as alive"


def test_bus_listener_alive_after_needs_activity_after_the_point(sse_listener):
    s = sse_listener
    with busclient.BusListener("127.0.0.1", s["port"], s["pin"], s["pair"], "ka") as live:
        assert live.open() == (200, None)
        assert live.wait_for(lambda e: True, 10) is not None
        t = time.monotonic()
        assert live.alive_after(t, 5), "keepalives after the point were not seen"
        assert max(live.keepalive_times) > t
    # The `ok` stream sends one keepalive and one event, then nothing: alive
    # before that point, silent after it.
    with busclient.BusListener("127.0.0.1", s["port"], s["pin"], s["pair"], "ok") as quiet:
        assert quiet.open() == (200, None)
        assert quiet.wait_for(lambda e: True, 10) is not None
        before = min(quiet.keepalive_times) - 1
        assert quiet.alive_after(before, 1)
        assert not quiet.alive_after(time.monotonic(), 0.6)
        assert quiet.error is None


def test_machine_headers_are_stable_per_identity():
    import re
    import urllib.parse
    a, a2, b = busclient.machine_headers("alice"), busclient.machine_headers("alice"), busclient.machine_headers("bob")
    assert a == a2, "an identity's machine changed between calls"
    assert a[busclient.MACHINE_ID_HEADER] != b[busclient.MACHINE_ID_HEADER], "two identities share a machine"
    for h in (a, b):
        assert re.fullmatch(r"[0-9a-f]{32}", h[busclient.MACHINE_ID_HEADER])
        name = urllib.parse.unquote(h[busclient.MACHINE_NAME_HEADER])
        assert 1 <= len(name.encode()) <= 63
        assert all(0x20 <= ord(c) <= 0x7E for c in h[busclient.MACHINE_NAME_HEADER])


def test_bus_listener_names_its_machine(sse_listener):
    s = sse_listener
    with busclient.BusListener("127.0.0.1", s["port"], s["pin"], s["pair"], "ok", name="alice") as listener:
        assert listener.open() == (200, None)
    with busclient.BusListener("127.0.0.1", s["port"], s["pin"], s["pair"], "ok", name="x", machine="bob") as listener:
        assert listener.open() == (200, None)
    sent = [{k.lower(): v for k, v in h.items()} for h in s["headers"][-2:]]
    for got, who in zip(sent, ("alice", "bob")):
        want = busclient.machine_headers(who)
        assert got.get(busclient.MACHINE_ID_HEADER.lower()) == want[busclient.MACHINE_ID_HEADER], got
        assert got.get(busclient.MACHINE_NAME_HEADER.lower()) == want[busclient.MACHINE_NAME_HEADER], got


def test_pinned_request_sends_extra_headers(sse_listener):
    s = sse_listener
    status, body = tlsclient.pinned_request("127.0.0.1", s["port"], "/api/plugins/listeners/refused/messages",
                                            s["pin"], s["pair"], headers=busclient.machine_headers("carol"))
    assert status == 403, body
    got = {k.lower(): v for k, v in s["headers"][-1].items()}
    assert got.get(busclient.MACHINE_ID_HEADER.lower()) == busclient.machine_id("carol"), got


def test_bus_listener_reports_a_refused_subscribe(sse_listener):
    s = sse_listener
    listener = busclient.BusListener("127.0.0.1", s["port"], s["pin"], s["pair"], "refused")
    status, body = listener.open()
    listener.close()
    assert status == 403 and tlsclient.error_code(body) == "FORBIDDEN"


def test_bus_listener_and_pinned_refuse_the_wrong_server(sse_listener):
    s = sse_listener
    with pytest.raises(AssertionError, match="pinned"):
        busclient.BusListener("127.0.0.1", s["port"], "sha256:" + "00" * 32, s["pair"], "ok").open()
    with pytest.raises(tlsclient.PinMismatch):
        tlsclient.Pinned("127.0.0.1", s["port"], "sha256:" + "00" * 32, *s["pair"])
    status, body = tlsclient.Pinned("127.0.0.1", s["port"], s["pin"], *s["pair"]).request("GET", "/api/x")
    assert status == 403 and tlsclient.error_code(body) == "FORBIDDEN"


@pytest.mark.parametrize(
    "target", ["dev-server-restart", "dev-server-up-fc", "dev-server-restart-fc", "dev-server-restart-remote"],
)
def test_a_bad_dev_users_value_fails_before_anything_runs(tmp_path, target):
    env = tmp_path / "remote.env"
    env.write_text(_REMOTE_ENV_OK)
    r, calls = _make_with_stubs(tmp_path, target, "SHED_DEV_USERS=alice", f"SHED_REMOTE_ENV_FILE={env}")
    assert r.returncode != 0
    assert "SHED_DEV_USERS is 'alice'" in r.stdout + r.stderr
    assert not [c for c in calls if "kill" in c or "serve --config" in c or c.startswith(("scp ", "uname -s"))], calls


# A stand-in for scripts/render-dev-config.sh (DEV_RENDER_SCRIPT) that records
# its argv, one tab-separated line, and then does what RENDER_MODE says: fail,
# write an empty file, write nothing and exit 0, or write a small config. And a
# stand-in shed-server (DEV_SERVER_BIN) that records its argv and exits, so no
# test here can start a real server whatever the Makefile does.
_RENDER_STUB = """#!/bin/bash
{ printf 'render'; for a in "$@"; do printf '\\t%s' "$a"; done; printf '\\n'; } >> "$STUB_LOG"
case "$RENDER_MODE" in
  fail) echo "stub renderer: failing on purpose" >&2; exit 1 ;;
  empty) mkdir -p "$(dirname "$2")"; : > "$2"; exit 0 ;;
  nothing) exit 0 ;;
  *) mkdir -p "$(dirname "$2")"; printf 'auth:\\n  mode: mtls\\n' > "$2"; exit 0 ;;
esac
"""
_DARWIN_UNAME = '#!/bin/bash\necho "uname $*" >> "$STUB_LOG"\necho Darwin\n'


def _render_stubs(tmp_path: Path, *, darwin: bool = False) -> tuple[str, str]:
    """Write the renderer and server stubs (and, for the Mac targets, a
    `uname` that says Darwin) into the stubs dir `_make_with_stubs` uses.
    Returns the make assignments that put them in place."""
    stubs = tmp_path / "stubs"
    stubs.mkdir(exist_ok=True)
    render = stubs / "render-stub"
    render.write_text(_RENDER_STUB)
    render.chmod(0o755)
    server = stubs / "shed-server"
    server.write_text('#!/bin/bash\necho "shed-server $*" >> "$STUB_LOG"\nexit 0\n')
    server.chmod(0o755)
    if darwin:
        (stubs / "uname").write_text(_DARWIN_UNAME)
        (stubs / "uname").chmod(0o755)
    return f"DEV_RENDER_SCRIPT={render}", f"DEV_SERVER_BIN={server}"


def _renders(calls: list[str]) -> list[list[str]]:
    return [c.split("\t")[1:] for c in calls if c.startswith("render\t")]


def _launches(calls: list[str]) -> list[str]:
    """Every start of a server: the Mac and remote launch (the stub), and the
    FC upload and remote launch (the scp and ssh stubs)."""
    return [c for c in calls if c.startswith("shed-server ") or "serve --config" in c or c.startswith("scp ")]


def _remote_env(tmp_path: Path) -> tuple[str]:
    env = tmp_path / "remote.env"
    env.write_text(_REMOTE_ENV_OK)
    return (f"SHED_REMOTE_ENV_FILE={env}",)


@pytest.mark.parametrize("mode", ["fail", "empty", "nothing"])
@pytest.mark.parametrize(
    "target, assigns",
    [
        ("dev-server-up", ("SHED_DEV_USERS=itest",)),
        # The same hole without --users: a state-dir render that fails.
        ("dev-server-up", ("SHED_DEV_STATE_DIR={tmp}/state",)),
        ("dev-server-up-fc", ("SHED_DEV_USERS=itest",)),
    ],
    ids=["mac-users", "mac-state-dir", "fc-users"],
)
def test_a_render_that_fails_never_starts_a_server(tmp_path, target, assigns, mode):
    # Before the fix the Mac target went on to `serve --config ""` (shed-server's
    # normal config search) when the render failed; the stub server records it.
    stubs = _render_stubs(tmp_path, darwin=target == "dev-server-up")
    r, calls = _make_with_stubs(
        tmp_path, target, *stubs, *(a.format(tmp=tmp_path) for a in assigns), *_remote_env(tmp_path),
        env_extra={"RENDER_MODE": mode},
    )
    out = r.stdout + r.stderr
    assert _renders(calls), (calls, out)
    assert r.returncode != 0, out
    assert not _launches(calls), calls
    assert "not starting shed-server" in out, out


@pytest.mark.parametrize(
    "target, assign",
    [
        ("dev-server-up", "DEV_CONFIG={empty}"),
        ("dev-server-up-fc", "FC_DEV_LOCAL_CONFIG={empty}"),
        ("dev-server-up-remote", "REMOTE_DEV_TEMPLATE={empty}"),
    ],
    ids=["mac", "fc", "remote"],
)
def test_an_empty_config_never_starts_a_server_without_users_either(tmp_path, target, assign):
    empty = tmp_path / "empty.yaml"
    empty.write_text("")
    stubs = _render_stubs(tmp_path, darwin=target == "dev-server-up")
    r, calls = _make_with_stubs(tmp_path, target, *stubs, assign.format(empty=empty), *_remote_env(tmp_path))
    out = r.stdout + r.stderr
    assert r.returncode != 0, out
    assert not _launches(calls), calls
    assert "not starting shed-server" in out, out


def test_the_dev_targets_start_with_the_config_they_rendered(tmp_path):
    # The positive control for the two tests above: a render that works
    # reaches the launch, with the rendered file, after the render.
    home = tmp_path / "home"
    for target, users, want_base, want_out, launch in (
        ("dev-server-up", "itest", str(MAC_DEV_CONFIG), f"{home}/.shed/dev/server.generated.yaml", "shed-server serve"),
        ("dev-server-up", "", None, None, "shed-server serve"),
        ("dev-server-up-fc", "itest", "configs/server.dev-parallel.linux-fc.yaml",
         f"{home}/.shed/dev/server.dev-fc.users.yaml", "scp "),
        ("dev-server-up-fc", "", None, None, "scp "),
        ("dev-server-up-remote", "", None, None, "shed-server serve"),
    ):
        stubs = _render_stubs(tmp_path, darwin=target == "dev-server-up")
        r, calls = _make_with_stubs(
            tmp_path, target, *stubs, f"SHED_DEV_USERS={users}", "SHED_ITEST_USERS_DIR=/keys/here",
            *_remote_env(tmp_path), env_extra={"RENDER_MODE": "ok"},
        )
        renders = _renders(calls)
        if users:
            assert renders == [[want_base, want_out, "--users", "/keys/here"]], (target, calls, r.stdout + r.stderr)
        else:
            assert renders == [], (target, calls)
        started = [i for i, c in enumerate(calls) if c.startswith(launch)]
        assert started, (target, users, calls, r.stdout + r.stderr)
        if users:
            first = next(i for i, c in enumerate(calls) if c.startswith("render\t"))
            assert first < started[0], (target, calls)
        if launch == "shed-server serve":
            config = calls[started[0]].split("--config ", 1)[1]
            want = {"dev-server-up": f"{home}/.shed/dev/server.generated.yaml" if users else str(MAC_DEV_CONFIG),
                    "dev-server-up-remote": f"{home}/.shed/dev-remote/server.yaml"}[target]
            assert config == want, (target, users, calls)
            assert Path(config).stat().st_size > 0


def test_the_remote_dev_server_takes_no_itest_user_list(tmp_path):
    # Its people come from the account service (auth.directory): the itest
    # user list is refused before anything is rendered or started.
    stubs = _render_stubs(tmp_path)
    r, calls = _make_with_stubs(tmp_path, "dev-server-up-remote", *stubs, "SHED_DEV_USERS=itest", *_remote_env(tmp_path),
                                env_extra={"RENDER_MODE": "ok"})
    out = r.stdout + r.stderr
    assert r.returncode != 0, out
    assert "takes its people from the account service" in out, out
    assert not _renders(calls) and not _launches(calls), calls


# A stand-in shed-server that comes up the way the probe wants it (two
# loopback listeners logged, the readiness line) and stays up, logging its
# state directory and, with NEW_HOST_KEY set, the line serve logs when it
# makes a host key (internal/sshd/server.go).
_LISTENING_SERVER_STUB = """#!{python}
import os, socket, sys, time
with open(os.environ["STUB_LOG"], "a") as f:
    f.write("shed-server " + " ".join(sys.argv[1:]) + "\\n")
socks = []
for _ in range(2):
    s = socket.socket()
    s.bind(("127.0.0.1", 0))
    s.listen(8)
    socks.append(s)
state = os.environ["HOME"] + "/.shed/dev/state"
print("State dir: " + state)
if os.environ.get("NEW_HOST_KEY"):
    print("Generated new host key: " + state + "/host_key")
print("HTTPS server listening on 127.0.0.1:%d" % socks[0].getsockname()[1])
print("SSH server listening on 127.0.0.1:%d" % socks[1].getsockname()[1])
print("Shed server is ready")
sys.stdout.flush()
time.sleep(90)
"""


@pytest.mark.parametrize("new_key", [True, False], ids=["new-host-key", "existing-host-key"])
def test_mac_dev_server_up_names_its_identity_and_says_when_to_re_add(tmp_path, new_key):
    import sys

    stubs = _render_stubs(tmp_path, darwin=True)
    server = tmp_path / "stubs" / "shed-server"
    server.write_text(_LISTENING_SERVER_STUB.format(python=sys.executable))
    server.chmod(0o755)
    pid_file = tmp_path / "home" / ".shed" / "dev" / "server.pid"
    # macOS make runs recipes with /bin/sh, which is bash 3.2; Linux's is dash,
    # which accepts lines bash refuses (an apostrophe inside "${VAR:-word}"
    # broke this recipe on the Mac only). Run the recipe under bash here too.
    shell = [f"SHELL={shutil.which('bash')}"] if shutil.which("bash") else []
    try:
        r, calls = _make_with_stubs(tmp_path, "dev-server-up", *stubs, *shell,
                                    env_extra={"NEW_HOST_KEY": "1" if new_key else ""})
    finally:
        if pid_file.exists():
            try:
                os.kill(int(pid_file.read_text().strip()), 9)
            except (ValueError, ProcessLookupError):
                pass
    out = r.stdout + r.stderr
    assert r.returncode == 0, out
    assert [c for c in calls if c.startswith("shed-server serve --config ")], calls
    state = f"{tmp_path}/home/.shed/dev/state"
    assert f"Identity: {state}" in out, out
    hint = f"NOTE: this start made a new SSH host key in {state}"
    if new_key:
        assert hint in out, out
        for line in ("shed server remove my-server-dev", "ssh-keygen -R '[localhost]:12222' -f ~/.shed/known_hosts",
                     "shed server add localhost --ssh-port 12222 --name my-server-dev"):
            assert line in out, (line, out)
    else:
        assert "NOTE:" not in out and "shed server remove" not in out, out


# The remote dev server takes no itest user list (its people are the account
# service's); its own render's failure is test_an_empty_config_never_...'s.
@pytest.mark.parametrize("target", ["dev-server-restart", "dev-server-restart-fc"])
def test_a_render_that_fails_stops_a_restart_before_the_stop(tmp_path, target):
    pid_file, proc = _remote_home(tmp_path)
    try:
        stubs = _render_stubs(tmp_path, darwin=target == "dev-server-restart")
        r, calls = _make_with_stubs(tmp_path, target, *stubs, "SHED_DEV_USERS=itest", *_remote_env(tmp_path),
                                    env_extra={"RENDER_MODE": "fail"})
        out = r.stdout + r.stderr
        assert r.returncode != 0, out
        assert _renders(calls), calls
        # No stop ran: the Mac stop's `uname -s`, the FC stop's ssh kill, and
        # the remote stop, which would have removed this pid file.
        assert not [c for c in calls if c.startswith("uname -s") or "kill" in c], calls
        assert not _launches(calls), calls
        assert pid_file.exists() and proc.poll() is None
    finally:
        proc.kill()
        proc.wait()


@pytest.mark.parametrize("target", ["dev-server-restart", "dev-server-restart-fc", "dev-server-restart-remote"])
def test_a_restart_with_a_render_that_works_does_stop_then_start(tmp_path, target):
    # The control for the test above: the same restart with a working render
    # reaches the stop and then the launch.
    pid_file, proc = _remote_home(tmp_path)
    try:
        stubs = _render_stubs(tmp_path, darwin=target == "dev-server-restart")
        users = "" if target == "dev-server-restart-remote" else "itest"
        r, calls = _make_with_stubs(tmp_path, target, *stubs, f"SHED_DEV_USERS={users}", *_remote_env(tmp_path),
                                    env_extra={"RENDER_MODE": "ok"})
        stop = {"dev-server-restart": lambda c: c.startswith("uname -s"),
                "dev-server-restart-fc": lambda c: "kill -TERM" in c,
                "dev-server-restart-remote": None}[target]
        launches = [i for i, c in enumerate(calls) if c.startswith("shed-server serve") or c.startswith("scp ")]
        assert launches, (calls, r.stdout + r.stderr)
        if stop is not None:
            assert next(i for i, c in enumerate(calls) if stop(c)) < launches[0], calls
        else:
            # dev-server-down-remote removed the pid file of the "running"
            # (not a shed-server, so unsignalled) process before the launch.
            assert not pid_file.exists() or pid_file.read_text().strip() != str(proc.pid)
    finally:
        proc.kill()
        proc.wait()


def _half_pair(keys_dir: Path) -> Path:
    (keys_dir / ident.BOB / "id_ed25519.pub").unlink()
    return keys_dir


def test_the_real_renderer_fails_on_a_half_key_pair_and_leaves_no_config(tmp_path, keys_dir):
    if shutil.which("uv") is None:
        pytest.skip("uv is not installed")
    out = tmp_path / "out.yaml"
    out.write_text("left from an earlier run\n")
    r = _render(tmp_path, FC_DEV_CONFIG, "--users", str(_half_pair(keys_dir)))
    assert r.returncode != 0 and "only one half" in r.stderr, r.stdout + r.stderr
    assert not out.exists(), "a failed render left a config behind"


def test_the_real_renderer_fails_without_uv_and_leaves_no_config(tmp_path, keys_dir):
    path = "/usr/bin:/bin"
    if shutil.which("uv", path=path) is not None:
        pytest.skip("uv is installed system-wide; cannot run the renderer without it")
    out = tmp_path / "out.yaml"
    r = subprocess.run(["bash", str(RENDER), str(FC_DEV_CONFIG), str(out), "--users", str(keys_dir)],
                       capture_output=True, text=True, timeout=30, env={**os.environ, "PATH": path})
    assert r.returncode != 0 and "needs uv" in r.stderr, r.stdout + r.stderr
    assert not out.exists()


def test_the_fc_target_with_the_real_renderer_and_a_half_key_pair_uploads_nothing(tmp_path, keys_dir):
    if shutil.which("uv") is None:
        pytest.skip("uv is not installed")
    r, calls = _make_with_stubs(tmp_path, "dev-server-up-fc", "SHED_DEV_USERS=itest",
                                f"SHED_ITEST_USERS_DIR={_half_pair(keys_dir)}")
    out = r.stdout + r.stderr
    assert r.returncode != 0 and "only one half" in out, out
    assert not _launches(calls), calls
    assert not (tmp_path / "home" / ".shed" / "dev" / "server.dev-fc.users.yaml").exists()


@pytest.mark.parametrize("target", ["dev-server-up", "dev-server-up-fc", "dev-itest-users"])
def test_the_users_dir_is_one_argument_and_never_code(tmp_path, target):
    pwned = tmp_path / "pwned"
    users_dir = f'{tmp_path}/keys dir"; touch {pwned}; echo "`touch {pwned}`'
    stubs = _render_stubs(tmp_path, darwin=target == "dev-server-up")
    extra = ()
    if target == "dev-itest-users":
        # dev-itest-users runs `uv run ... --dir DIR keys`; a uv stub records it.
        uv = tmp_path / "stubs" / "uv"
        uv.write_text('#!/bin/bash\n{ printf uv; for a in "$@"; do printf "\\t%s" "$a"; done; printf "\\n"; } >> "$STUB_LOG"\n')
        uv.chmod(0o755)
    r, calls = _make_with_stubs(tmp_path, target, *stubs, "SHED_DEV_USERS=itest", f"SHED_ITEST_USERS_DIR={users_dir}",
                                *_remote_env(tmp_path), *extra, env_extra={"RENDER_MODE": "ok"})
    assert not pwned.exists(), "a quote in the users dir ran as shell code"
    if target == "dev-itest-users":
        got = [c.split("\t")[1:] for c in calls if c.startswith("uv\t")]
        assert got and got[0][got[0].index("--dir") + 1] == users_dir, calls
    else:
        renders = _renders(calls)
        assert renders and renders[0][renders[0].index("--users") + 1] == users_dir, (calls, r.stdout + r.stderr)


def test_the_test_targets_tell_the_suite_about_the_user_list():
    lines = _makefile_logical_lines()
    for target, backend in (("test-integration-dev", None), ("test-integration-dev-fc", "fc"),
                            ("test-integration-remote", None)):
        start = next(i for i, l in enumerate(lines) if l.startswith(f"{target}:"))
        recipe = []
        for l in lines[start + 1:]:
            if not l.startswith("\t"):
                break
            recipe.append(l)
        body = "\n".join(recipe)
        if backend is None:
            # The [vz] cases restart the Mac dev server with the list
            # themselves, and the remote dev server takes its people from
            # the account service (it refuses SHED_DEV_USERS=itest): nothing
            # to tell them.
            assert "SHED_ITEST_USERS=" not in body, (target, body)
        else:
            assert f"SHED_ITEST_USERS=$(if $(filter itest,$(SHED_DEV_USERS)),{backend},)" in body, (target, body)
        # Quoted, from the environment (the Makefile exports it): a path with
        # a space stays one value.
        assert 'SHED_ITEST_USERS_DIR="$$SHED_ITEST_USERS_DIR"' in body, target
        assert "$(SHED_ITEST_USERS_DIR)" not in body, target


# ----------------------------------------------------------------------------
# The users cases' own helpers (test_users.py, test_users_upgrade.py,
# conftest.py): checked clean-up, order independence, the platform skip, the
# target check before any create, a forward that keeps stdin open, the bus
# exchange's time budget, and the server's own words the refusals are matched
# against. No server: the CLI and the API are fakes.
# ----------------------------------------------------------------------------

import sys  # noqa: E402

import test_users as tu  # noqa: E402
import test_users_upgrade as tuu  # noqa: E402


class _CliServer(fx.LocalServer):
    """A LocalServer whose `shed` CLI is a script: `listed` is what each
    `--json list` says in turn (True, False, or None for a failed list; the
    last value repeats), `delete_rc` what a delete exits with."""

    CLEANUP_GONE_TIMEOUT = 0.3
    CLEANUP_POLL_INTERVAL = 0.05

    def __init__(self, listed, delete_rc=0, name="box-dev", shed="s1"):
        super().__init__(name=name, backend="vz")
        self.shed = shed
        self.listed = list(listed)
        self.delete_rc = delete_rc
        self.calls: list[tuple] = []

    def cli(self, *args, timeout=60):
        self.calls.append(args)
        if args[:2] == ("--json", "list"):
            state = self.listed.pop(0) if len(self.listed) > 1 else self.listed[0]
            if state is None:
                return subprocess.CompletedProcess(args, 1, "", "connection refused")
            return subprocess.CompletedProcess(args, 0, json.dumps([{"name": self.shed}] if state else []), "")
        if args[0] == "delete":
            if self.delete_rc == "timeout":
                raise subprocess.TimeoutExpired(args, timeout)
            return subprocess.CompletedProcess(args, self.delete_rc, "", "" if self.delete_rc == 0 else "boom")
        raise AssertionError(f"unexpected cli {args}")


def test_ensure_deleted_deletes_then_verifies_it_is_gone():
    s = _CliServer([True, False])
    s.ensure_deleted("s1")
    assert ("delete", "s1", "-f") in s.calls
    # A name that is not there: nothing to delete, and nothing fails.
    s = _CliServer([False])
    s.ensure_deleted("s1")
    assert not [c for c in s.calls if c[0] == "delete"]


@pytest.mark.parametrize(
    "listed, delete_rc, want",
    [
        ([True], 1, "is still listed"),            # the delete failed and it is still there
        ([True], 0, "is still listed"),            # the delete "worked" and it is still there
        ([True], "timeout", "timed out"),          # the delete hung
        ([None], 0, "could not be confirmed gone"),  # nobody can say it is gone
    ],
    ids=["delete-failed", "delete-lied", "delete-hung", "list-failed"],
)
def test_ensure_deleted_fails_loudly_naming_the_shed_and_server(listed, delete_rc, want):
    s = _CliServer(listed, delete_rc)
    with pytest.raises(AssertionError) as e:
        s.ensure_deleted("s1", earlier=["the owner's delete exited 1"])
    msg = str(e.value)
    assert "CLEAN-UP FAILED" in msg and "'s1'" in msg and "'box-dev'" in msg and want in msg, msg
    assert "the owner's delete exited 1" in msg


class _Who:
    """An identity whose `shed` CLI is a script (for test_users._delete)."""

    def __init__(self, user, rc):
        self.user, self.rc, self.calls = user, rc, []

    def cli(self, *args, timeout=120):
        self.calls.append(args)
        if self.rc == "timeout":
            raise subprocess.TimeoutExpired(args, timeout)
        return subprocess.CompletedProcess(args, self.rc, "", "" if self.rc == 0 else "owner delete broke")


class _Env:
    def __init__(self, server, backend="fc"):
        self.server, self.backend = server, backend


def test_users_delete_falls_back_to_the_admin_and_verifies():
    # The owner's delete fails; the developer's (an admin) works; it is gone.
    server = _CliServer([True, False])
    owner = _Who(ident.BOB, 1)
    tu._delete(_Env(server), owner, "s1")
    assert owner.calls == [("delete", "s1", "-f")]
    assert ("delete", "s1", "-f") in server.calls


@pytest.mark.parametrize("owner_rc", [1, "timeout"])
def test_users_delete_fails_when_nobody_could_delete_it(owner_rc):
    # Before the fix both deletes' failures were swallowed and the teardown
    # passed with the VM still there.
    server = _CliServer([True], delete_rc=1)
    with pytest.raises(AssertionError) as e:
        tu._delete(_Env(server), _Who(ident.BOB, owner_rc), "s1")
    msg = str(e.value)
    assert "'s1'" in msg and "'box-dev'" in msg and "itest-bob's `shed delete s1`" in msg, msg


def test_a_failed_create_keeps_its_error_and_reports_a_failed_clean_up(monkeypatch):
    monkeypatch.setattr(tu.uuid, "uuid4", lambda: type("U", (), {"hex": "0" * 32})())
    server = _CliServer([True], delete_rc=1, shed="itest-x-00000000")

    def create(env, who, name):
        raise AssertionError("create failed")

    monkeypatch.setattr(tu, "_create", create)
    with pytest.raises(AssertionError, match="create failed") as e:
        tu._shed_fixture(_Env(server), _Who(ident.ALICE, 1), "itest-x")
    assert any("CLEAN-UP FAILED" in n for n in getattr(e.value, "__notes__", [])), e.value


def test_the_shed_slot_lets_the_module_run_in_any_order(monkeypatch):
    made, deleted = [], []
    monkeypatch.setattr(tu, "_shed_fixture", lambda env, who, prefix: made.append(f"{prefix}-{len(made)}") or made[-1])
    monkeypatch.setattr(tu, "_delete", lambda env, who, name: deleted.append(name))
    slot = tu._ShedSlot(object(), object(), "itest-ub")
    # The deleting case first: it gets a shed of its own to delete ...
    taken = slot.take()
    assert taken == "itest-ub-0" and slot.name is None
    # ... and a later case gets a new one, never the deleted one.
    assert slot.get() == "itest-ub-1" and slot.get() == "itest-ub-1"
    slot.close()
    assert deleted == ["itest-ub-1"], "the slot's own shed was not cleaned up, or the taken one was deleted twice"
    slot.close()
    assert deleted == ["itest-ub-1"]


def test_run_all_runs_every_clean_up_step_and_reports_each_failure():
    ran = []

    def boom(msg):
        def step():
            ran.append(msg)
            raise AssertionError(msg)
        return step

    with pytest.raises(AssertionError) as e:
        tu._run_all([boom("snapshot"), lambda: ran.append("shed"), boom("restart")])
    assert ran == ["snapshot", "shed", "restart"]
    assert "snapshot" in str(e.value) and "restart" in str(e.value)


class _Api:
    def __init__(self, answers):
        self.answers, self.calls = list(answers), []

    def request(self, method, path, body=None, timeout=None):
        self.calls.append((method, path))
        a = self.answers.pop(0)
        if isinstance(a, Exception):
            raise a
        return a


class _ApiWho:
    def __init__(self, user, api):
        self.user, self._api = user, api

    def api(self):
        return self._api


_SNAP_GONE = (404, {"error": {"code": "SNAPSHOT_NOT_FOUND", "message": "x"}})


def test_snapshot_clean_up_falls_back_to_the_admin_and_verifies():
    alice = _ApiWho(ident.ALICE, _Api([TimeoutError("slow")]))
    admin = _ApiWho(ident.ADMIN, _Api([(204, None), _SNAP_GONE]))
    env = type("E", (), {"alice": alice, "admin": admin, "server": _CliServer([False])})()
    tu._delete_snapshot(env, "snap1")
    assert admin._api.calls == [("DELETE", "/api/snapshots/snap1"), ("GET", "/api/snapshots/snap1")]


def test_snapshot_clean_up_fails_loudly_when_it_is_still_there():
    alice = _ApiWho(ident.ALICE, _Api([(500, {"error": {"code": "BACKEND_ERROR", "message": "x"}})]))
    admin = _ApiWho(ident.ADMIN, _Api([(500, None), (200, {"name": "snap1"})]))
    env = type("E", (), {"alice": alice, "admin": admin, "server": _CliServer([False])})()
    with pytest.raises(AssertionError, match="CLEAN-UP FAILED: snapshot 'snap1' on server 'box-dev'"):
        tu._delete_snapshot(env, "snap1")


@pytest.mark.parametrize("case", [
    tuu.test_a_one_user_list_gives_that_user_the_sheds_that_had_none,
    tuu.test_cutting_the_list_to_one_user_later_hands_out_nothing,
    tuu.test_an_admin_assigns_a_shed_that_has_no_user,
], ids=["one-user", "cut-to-one", "assign"])
def test_upgrade_cases_check_the_target_before_creating_anything(case):
    # An entry that is not a dev server (here, by name; assert_dev_target
    # also refuses production ports): the case raises before any CLI call,
    # so nothing is created on it.
    class NotDev:
        name = "my-server"

        def cli(self, *args, **kw):
            raise AssertionError(f"the case reached the server before checking it: shed {args}")

        def ensure_deleted(self, name, **kw):
            raise AssertionError(f"the case cleaned up on the server before checking it: {name}")

    with pytest.raises(RuntimeError, match="refusing to reconfigure non-dev server"):
        case(NotDev())


def test_vz_users_cases_skip_off_macos_before_any_fixture(monkeypatch):
    import conftest

    class Item:
        callspec = type("C", (), {"params": {"users_env": "vz"}})()

        def get_closest_marker(self, name):
            return None

    monkeypatch.setattr(sys, "platform", "linux")
    with pytest.raises(pytest.skip.Exception, match="not macOS"):
        conftest.pytest_runtest_setup(Item())
    monkeypatch.setattr(sys, "platform", "darwin")
    conftest.pytest_runtest_setup(Item())


def test_upgrade_module_is_skipped_by_mark_on_this_platform():
    # The mark's condition was evaluated at import, on this host.
    (mark,) = [m for m in tuu.pytestmark if m.name == "skipif"]
    assert mark.args[0] is (sys.platform != "darwin")


# A stand-in for the identity's ssh shim on `ssh -W`: like a forward the far
# side closes as soon as stdin reaches EOF, it prints its banner only if
# stdin is still open after a moment, then copies until stdin closes.
_FORWARD_SHIM = """#!/bin/bash
cat <&0 >/dev/null & c=$!
sleep 1
if ! kill -0 "$c" 2>/dev/null; then echo "forward torn down: stdin reached EOF before the banner" >&2; exit 255; fi
printf 'SSH-2.0-fake\\r\\n'
wait "$c"
exit 0
"""


def test_forward_keeps_stdin_open_until_the_banner_arrives(tmp_path, keys_dir):
    if shutil.which("ssh") is None:
        pytest.skip("ssh is not installed")
    a = ident.Identity.create(ident.ALICE, "box-dev", "localhost", 12222, keys_dir=keys_dir)
    try:
        a.shim.write_text(_FORWARD_SHIM)
        start = time.monotonic()
        rc, first, err = a.forward("some-shed", timeout=10)
        assert first.startswith(b"SSH-"), (rc, first, err)
        assert rc == 0 and time.monotonic() - start < 8, "ssh was not let go once the banner was in"
    finally:
        a.remove()


def test_forward_reports_a_refused_forward(tmp_path, keys_dir):
    if shutil.which("ssh") is None:
        pytest.skip("ssh is not installed")
    a = ident.Identity.create(ident.BOB, "box-dev", "localhost", 12222, keys_dir=keys_dir)
    try:
        a.shim.write_text('#!/bin/bash\necho "channel 0: open failed: connect failed: shed not found" >&2\nexit 255\n')
        rc, first, err = a.forward("some-shed", timeout=10)
        assert (rc, first) == (255, b"") and tu.FORWARD_NOT_FOUND in err, (rc, first, err)
    finally:
        a.remove()


def test_the_bus_exchange_fits_inside_the_guests_request_lifetime():
    # The guest's lifetime, read from the agent's source, so a change there
    # fails here rather than as a flaky live case.
    src = (dc.REPO_ROOT / "cmd" / "shed-agent" / "http.go").read_text()
    m = re.search(r"publishTimeout\s*=\s*(\d+)\s*\*\s*time\.Second", src)
    assert m, "publishTimeout not found in cmd/shed-agent/http.go"
    lifetime = int(m.group(1))
    assert tu.GUEST_REQUEST_LIFETIME == lifetime
    # Delivery, then the forged answers side by side, must leave the owner
    # time to answer by ANSWER_BY, and the answer must land with margin.
    assert tu.DELIVERY_BUDGET + tu.FORGERY_TIMEOUT + 2 <= tu.ANSWER_BY
    assert tu.ANSWER_BY + tu.ANSWER_TIMEOUT <= lifetime - 3
    # The liveness wait covers the server's keepalive interval.
    api = (dc.REPO_ROOT / "internal" / "api" / "plugin_handlers.go").read_text()
    k = re.search(r"sseKeepaliveInterval\s*=\s*(\d+)\s*\*\s*time\.Second", api)
    assert k and tu.LIVENESS_TIMEOUT >= int(k.group(1)) + 10


def test_the_refusals_the_live_cases_match_are_the_servers_own_words():
    # Each string a users case matches a refusal against is the server's
    # (a test that matched words the server never says could not fail for
    # the right reason, and one that matched a general "not found" could
    # pass on any failure).
    root = dc.REPO_ROOT
    sshd = (root / "internal" / "sshd" / "server.go").read_text()
    assert f'newChan.Reject(gossh.ConnectionFailed, "{tu.FORWARD_NOT_FOUND}")' in sshd
    wake = (root / "internal" / "sshd" / "wake.go").read_text()
    assert "wakeErrorf(\"shed '%s' not found\", name)" in wake
    assert tu._session_not_found("x") == "shed 'x' not found"
    ident_go = (root / "internal" / "sshd" / "identity.go").read_text()
    assert tu.ANOTHER_USER in ident_go
    bus = (root / "internal" / "api" / "plugin_handlers.go").read_text()
    assert '"FORBIDDEN", "response does not match a pending request for this namespace"' in bus
    assign = (root / "internal" / "backend" / "assign.go").read_text()
    assert "already belongs to user %s" in assign
    assert '"SHED_ALREADY_ASSIGNED"' in (root / "internal" / "config" / "types.go").read_text()
    cli = (root / "cmd" / "shed" / "shed.go").read_text()
    assert 'fmt.Errorf("shed %q not found on %s", name, serverFlag)' in cli


# ----------------------------------------------------------------------------
# The pin cases' own helpers (test_source_pin.py, fixtures/busclient.py:
# FakeMachine, ensure_source, ensure_known): the machines, what a subscribe
# and an answer report, setting a binding up from any server state, the
# timing budget and the server's own words. No server: a local TLS server
# and a scripted binding store.
# ----------------------------------------------------------------------------

import test_source_pin as tsp  # noqa: E402


class _Cert:
    def __init__(self, s):
        self.https_port, self.pin, self.pair = s["port"], s["pin"], s["pair"]


def test_fake_machines_default_is_the_listeners_machine_and_tagged_ones_differ():
    cert = type("C", (), {"https_port": 1, "pin": "sha256:00", "pair": ("c", "k")})()
    a = busclient.FakeMachine(ident.ALICE, "h", cert)
    b = busclient.FakeMachine(ident.ALICE, "h", cert, tag="b")
    bob = busclient.FakeMachine(ident.BOB, "h", cert)
    # alice's default machine is the one test_users.py's bus case subscribes
    # as (BusListener(name=user)), so the two modules share it.
    assert a.id == busclient.machine_id(ident.ALICE) and a.headers == busclient.machine_headers(ident.ALICE)
    assert b.name == "itest-itest-alice-b" and a.name == "itest-itest-alice"
    for m in (a, b, bob):
        assert re.fullmatch(r"[0-9a-f]{32}", m.id) and 1 <= len(m.name.encode()) <= 63 and m.name.startswith("itest-")
    # Every id prefix the cases name a machine by is that machine's alone.
    for n in (8, 12):
        assert len({m.id[:n] for m in (a, b, bob)}) == 3, n
    assert len({m.name for m in (a, b, bob)}) == 3


def test_bus_listener_sends_the_headers_it_is_given(sse_listener):
    s = sse_listener
    with busclient.BusListener("127.0.0.1", s["port"], s["pin"], s["pair"], "ok", name="alice", headers={}) as listener:
        assert listener.open() == (200, None)
    got = {k.lower() for k in s["headers"][-1]}
    assert busclient.MACHINE_ID_HEADER.lower() not in got and busclient.MACHINE_NAME_HEADER.lower() not in got, got


def test_bus_listener_wait_ended_sees_the_server_end_the_stream(sse_listener):
    s = sse_listener
    with busclient.BusListener("127.0.0.1", s["port"], s["pin"], s["pair"], "ends") as ended:
        assert ended.open() == (200, None)
        assert ended.wait_ended(10) and "ended" in ended.error
    with busclient.BusListener("127.0.0.1", s["port"], s["pin"], s["pair"], "ok") as held:
        assert held.open() == (200, None)
        assert not held.wait_ended(0.3), "a stream the server holds counted as ended"
        held.close()
        assert not held.wait_ended(0.1), "a stream the test closed counted as ended by the server"


def test_fake_machine_reports_how_the_server_answered(sse_listener):
    s = sse_listener
    cert = _Cert(s)
    a = busclient.FakeMachine(ident.ALICE, "127.0.0.1", cert)
    b = busclient.FakeMachine(ident.ALICE, "127.0.0.1", cert, tag="b")

    sub = a.subscribe("pin")
    assert (sub.status, sub.code, sub.source) == (200, None, None) and sub.listener is a.streams[0]
    assert sub.listener.wait_for(lambda e: busclient.payload_has(e, "tok"), 10) is not None

    refused = b.subscribe("pin")
    assert (refused.status, refused.code) == (403, busclient.SOURCE_NOT_BOUND), refused
    assert refused.source == {"machine_id": a.id, "machine_name": a.name}
    assert refused.listener is None and b.streams == [] and a.name in str(refused)

    no_id = a.subscribe("pin", headers={})
    assert (no_id.status, no_id.code, no_id.source) == (403, busclient.SOURCE_MACHINE_ID_REQUIRED, None)
    assert "install the current shed desktop app" in no_id.message and len(a.streams) == 1

    held = a.streams[0]
    a.close()
    assert a.streams == [] and held.error is None


def test_fake_machine_answers_as_itself(sse_listener):
    s = sse_listener
    cert = _Cert(s)
    a = busclient.FakeMachine(ident.ALICE, "127.0.0.1", cert)
    b = busclient.FakeMachine(ident.ALICE, "127.0.0.1", cert, tag="b")
    req = {"id": "srv-9", "namespace": "ns-1", "shed": {"name": "s1"}, "payload": {}}

    assert a.answer(req, {"ok": 1}) == (204, None)
    post = s["posts"][-1]
    assert post["path"] == "/api/plugins/listeners/ns-1/respond"
    assert post["body"] == busclient.response_body(req, {"ok": 1})
    got = {k.lower(): v for k, v in post["headers"].items()}
    assert got[busclient.MACHINE_ID_HEADER.lower()] == a.id
    assert got[busclient.MACHINE_NAME_HEADER.lower()] == a.headers[busclient.MACHINE_NAME_HEADER]

    status, body = b.answer(req, {"ok": 1})
    assert (status, body["error"]["code"], body["source"]["machine_id"]) == (403, busclient.SOURCE_NOT_BOUND, a.id)
    status, body = a.answer(req, {"ok": 1}, headers={})
    assert (status, body["error"]["code"]) == (403, busclient.SOURCE_MACHINE_ID_REQUIRED)
    assert busclient.MACHINE_ID_HEADER.lower() not in {k.lower() for k in s["posts"][-1]["headers"]}


class _PinStore:
    """A user's binding as the server keeps it, behind the two routes
    (`request`) and a machine's subscribe (`admit`). `lie` makes the move
    route answer 200 without moving."""

    def __init__(self, bound=None, known=(), lie=False):
        self.bound, self.lie = bound, lie
        self.known = list(known) + ([bound] if bound and bound not in known else [])
        self.gen = 1 if bound else 0
        self.calls: list[tuple] = []

    def view(self):
        ref = [{"kind": "machine", "id": self.bound}]
        binding = None if self.bound is None else {
            "delivery": ref, "signing": ref, "generation": self.gen, "bound_by": "x", "bound_at": "t"}
        return {"user": ident.ALICE, "binding": binding,
                "machines": [{"id": m, "name": m, "connected": False, "namespaces": []} for m in self.known]}

    def request(self, method, path, body=None, timeout=None):
        self.calls.append((method, path, body))
        if method == "GET":
            return 200, self.view()
        if body["machine_id"] not in self.known:
            return 404, {"error": {"code": busclient.SOURCE_MACHINE_NOT_FOUND, "message": "x"}}
        if not self.lie:
            self.bound, self.gen = body["machine_id"], self.gen + 1
        return 200, self.view()

    def admit(self, mid):
        self.calls.append(("subscribe", mid))
        if mid not in self.known:
            self.known.append(mid)
        if self.bound is None:
            self.bound, self.gen = mid, self.gen + 1
        if mid == self.bound:
            return 200, None
        return 403, {"error": {"code": busclient.SOURCE_NOT_BOUND, "message": "x"},
                     "source": {"machine_id": self.bound, "machine_name": self.bound}}


class _StoreMachine:
    """A machine of a `_PinStore`. `answer` fixes what its subscribes get;
    `answers` is a list of them, used up one per subscribe (then the store
    decides); each is `(status, body)` or `(status, body, retry_after)`."""

    def __init__(self, store, mid, answer=None, answers=None):
        self.store, self.id, self.name = store, mid, mid
        self._answer, self._answers = answer, list(answers or [])

    def __repr__(self):
        return f"machine {self.id}"

    def subscribe(self, ns):
        fixed = self._answers.pop(0) if self._answers else self._answer
        if fixed:
            self.store.calls.append(("subscribe", self.id))
            status, body, *rest = fixed
            return busclient.Subscription(self.name, ns, status, body, None, rest[0] if rest else None)
        status, body = self.store.admit(self.id)
        return busclient.Subscription(self.name, ns, status, body, None)


def _moves(store):
    return [c for c in store.calls if c[0] == "POST"]


def _subscribes(store):
    return [c for c in store.calls if c[0] == "subscribe"]


def test_ensure_source_from_every_state_the_server_can_be_in():
    # Already the source: nothing is asked of the server but a read.
    s = _PinStore(bound="a")
    assert busclient.ensure_source(s, _StoreMachine(s, "a"), "ns") == "already"
    assert not _moves(s) and not _subscribes(s)
    # A fresh server: the machine's one subscribe binds it; no move.
    s = _PinStore()
    assert busclient.ensure_source(s, _StoreMachine(s, "a"), "ns") == "first-connect"
    assert s.bound == "a" and not _moves(s) and len(_subscribes(s)) == 1
    # Another machine bound, this one never seen: it is presented (refused,
    # recorded), then the binding is moved to it.
    s = _PinStore(bound="b")
    assert busclient.ensure_source(s, _StoreMachine(s, "a"), "ns") == "moved"
    assert s.bound == "a" and len(_subscribes(s)) == 1 and _moves(s) == [("POST", "/api/sources/use", {"machine_id": "a"})]
    # Another machine bound, this one known (a later run): moved, no subscribe.
    s = _PinStore(bound="b", known=["a"])
    assert busclient.ensure_source(s, _StoreMachine(s, "a"), "ns") == "moved"
    assert s.bound == "a" and not _subscribes(s)


@pytest.mark.parametrize(
    "store, answer, match",
    [
        # The presenting subscribe was admitted although another machine is bound.
        (lambda: _PinStore(bound="b"), (200, None), "presenting"),
        # It was refused for some other reason.
        (lambda: _PinStore(bound="b"), (403, {"error": {"code": "FORBIDDEN"}}), "presenting"),
        # Refused as not bound, but naming a machine that is not the source.
        (lambda: _PinStore(bound="b"), (403, {"error": {"code": busclient.SOURCE_NOT_BOUND},
                                              "source": {"machine_id": "c", "machine_name": "c"}}), "presenting"),
        # ... or naming the source by another name.
        (lambda: _PinStore(bound="b"), (403, {"error": {"code": busclient.SOURCE_NOT_BOUND},
                                              "source": {"machine_id": "b", "machine_name": "not-b"}}), "presenting"),
        # The move route said it moved and did not.
        (lambda: _PinStore(bound="b", known=["a"], lie=True), None, "is not the source after moved"),
    ],
    ids=["admitted-beside-a-binding", "refused-otherwise", "not-bound-naming-another-machine",
         "not-bound-naming-the-source-by-another-name", "move-did-not-land"],
)
def test_ensure_source_fails_loudly_when_the_server_does_not_do_it(store, answer, match):
    s = store()
    with pytest.raises(AssertionError, match=match):
        busclient.ensure_source(s, _StoreMachine(s, "a", answer), "ns")


def test_ensure_source_reports_a_refused_move():
    s = _PinStore(bound="b")
    # Refused naming the source, but never recorded.
    m = _StoreMachine(s, "a", (403, {"error": {"code": busclient.SOURCE_NOT_BOUND},
                                     "source": {"machine_id": "b", "machine_name": "b"}}))
    with pytest.raises(AssertionError, match="moving the source to machine a: 404"):
        busclient.ensure_source(s, m, "ns")


def test_ensure_known_records_a_machine_without_moving_the_source():
    s = _PinStore(bound="a")
    assert busclient.ensure_known(s, _StoreMachine(s, "b"), "ns") == "presented"
    assert s.bound == "a" and s.gen == 1 and "b" in s.known and not _moves(s)
    assert busclient.ensure_known(s, _StoreMachine(s, "b"), "ns") == "known"
    assert len(_subscribes(s)) == 1
    # With no source yet, presenting it would bind it: refused, nothing sent.
    s = _PinStore()
    with pytest.raises(AssertionError, match="no source yet"):
        busclient.ensure_known(s, _StoreMachine(s, "b"), "ns")
    assert not _subscribes(s) and s.bound is None


@pytest.mark.parametrize(
    "source",
    [None, {"machine_id": "c", "machine_name": "c"}, {"machine_id": "a", "machine_name": "not-a"}],
    ids=["names-nothing", "names-another-machine", "names-the-source-by-another-name"],
)
def test_ensure_known_wants_the_refusal_to_name_the_source(source):
    s = _PinStore(bound="a")
    body = {"error": {"code": busclient.SOURCE_NOT_BOUND}, **({"source": source} if source else {})}
    with pytest.raises(AssertionError, match="naming the bound machine"):
        busclient.ensure_known(s, _StoreMachine(s, "b", (403, body)), "ns")


@pytest.fixture
def waits(monkeypatch):
    """What the busclient helpers wait for, recorded instead of slept."""
    got: list[float] = []
    monkeypatch.setattr(busclient, "_sleep", got.append)
    return got


_LIMITED = (429, {"error": {"code": busclient.SOURCE_RATE_LIMITED, "message": "retry"}}, "7")


def test_a_set_up_subscribe_waits_out_the_rate_limit(waits):
    # Refused twice by the admission limit, then admitted: each Retry-After
    # waited, and the machine is presented.
    s = _PinStore(bound="a")
    assert busclient.ensure_known(s, _StoreMachine(s, "b", answers=[_LIMITED, _LIMITED]), "ns") == "presented"
    assert waits == [7.0, 7.0] and len(_subscribes(s)) == 3 and "b" in s.known
    # A Retry-After that is missing or not a number of seconds: the limit's own 15 s.
    waits.clear()
    s = _PinStore()
    assert busclient.ensure_source(s, _StoreMachine(s, "a", answers=[(*_LIMITED[:2], None)]), "ns") == "first-connect"
    assert waits == [busclient.RATE_LIMIT_WAIT]


def test_a_set_up_subscribe_still_rate_limited_fails_saying_so(waits):
    s = _PinStore(bound="a")
    with pytest.raises(AssertionError, match="still 429 SOURCE_RATE_LIMITED after waiting out its Retry-After 2 times"):
        busclient.ensure_known(s, _StoreMachine(s, "b", _LIMITED), "ns")
    assert waits == [7.0, 7.0] and len(_subscribes(s)) == 3


class _BusyApi:
    """GET /api/sources answering 503 SOURCE_BUSY `busy` times (with
    `retry_after`), then 200; it has `request_full`, as tlsclient.Pinned
    does."""

    def __init__(self, busy, retry_after="1", then=(200, {"user": "u", "binding": None, "machines": []})):
        self.busy, self.retry_after, self.then, self.calls = busy, retry_after, then, 0

    def request_full(self, method, path, body=None):
        self.calls += 1
        if self.calls <= self.busy:
            headers = {} if self.retry_after is None else {"retry-after": self.retry_after}
            return 503, {"error": {"code": busclient.SOURCE_BUSY, "message": "busy"}}, headers
        return (*self.then, {})


def test_sources_waits_out_source_busy(waits):
    api = _BusyApi(busy=2, retry_after="3")
    assert busclient.sources(api)["user"] == "u"
    assert api.calls == 3 and waits == [3.0, 3.0]
    # No Retry-After: one second, what the server sends.
    waits.clear()
    assert busclient.sources(_BusyApi(busy=1, retry_after=None))["user"] == "u" and waits == [1.0]
    # An absurd Retry-After is capped.
    waits.clear()
    busclient.sources(_BusyApi(busy=1, retry_after="9999"))
    assert waits == [busclient.MAX_RETRY_AFTER]


def test_sources_fails_clearly_when_it_stays_busy(waits):
    api = _BusyApi(busy=99)
    with pytest.raises(AssertionError, match="503 SOURCE_BUSY 5 times in a row"):
        busclient.sources(api)
    assert api.calls == busclient.SOURCE_BUSY_ATTEMPTS and len(waits) == busclient.SOURCE_BUSY_ATTEMPTS - 1


def test_sources_does_not_retry_any_other_answer(waits):
    for then in ((503, {"error": {"code": "PLUGINS_DISABLED"}}), (403, {"error": {"code": "FORBIDDEN"}})):
        api = _BusyApi(busy=0, then=then)
        with pytest.raises(AssertionError, match=f"GET /api/sources: {then[0]}"):
            busclient.sources(api)
        assert api.calls == 1 and waits == []


def test_pinned_request_full_returns_the_headers(monkeypatch):
    # sources() reads Retry-After through Pinned.request_full; request() is
    # the same call without them.
    class Resp:
        status = 503

        def read(self):
            return b'{"error":{"code":"SOURCE_BUSY"}}'

        def getheaders(self):
            return [("Retry-After", "1"), ("Content-Type", "application/json")]

        def getheader(self, name, default=None):
            return dict(self.getheaders()).get(name, default)

    class Conn:
        def __init__(self, *a, **k):
            pass

        def request(self, *a, **k):
            pass

        def getresponse(self):
            return Resp()

        def close(self):
            pass

    monkeypatch.setattr(tlsclient.http.client, "HTTPSConnection", Conn)
    p = tlsclient.Pinned.__new__(tlsclient.Pinned)
    p.host, p.port, p.ctx = "h", 1, None
    status, body, headers = p.request_full("GET", "/api/sources")
    assert (status, body["error"]["code"], headers["retry-after"]) == (503, "SOURCE_BUSY", "1")
    assert p.request("GET", "/api/sources") == (status, body)


def _proc(rc, out="", err=""):
    return subprocess.CompletedProcess(["shed"], rc, out, err)


def test_cli_retrying_busy_runs_again_only_on_source_busy(waits):
    runs = iter([_proc(1, err="Error: SOURCE_BUSY: retry in a moment"), _proc(0, out="ok")])
    assert busclient.cli_retrying_busy(lambda: next(runs), "shed source ls").stdout == "ok"
    assert waits == [busclient.SOURCE_BUSY_WAIT]
    # Any other failure is the command's answer, returned at once.
    waits.clear()
    refused = _proc(1, err="Error: FORBIDDEN: only an admin")
    assert busclient.cli_retrying_busy(lambda: refused, "shed source ls") is refused and waits == []


def test_cli_retrying_busy_fails_clearly_when_it_stays_busy(waits):
    calls = []
    with pytest.raises(AssertionError, match="shed source status: the server answered 503 SOURCE_BUSY 5 times"):
        busclient.cli_retrying_busy(lambda: calls.append(1) or _proc(1, err="SOURCE_BUSY: x"), "shed source status")
    assert len(calls) == busclient.SOURCE_BUSY_ATTEMPTS


def test_bus_listener_keeps_a_refusals_retry_after(sse_listener):
    s = sse_listener
    m = busclient.FakeMachine(ident.ALICE, "127.0.0.1", _Cert(s))
    sub = m.subscribe("limited")
    assert (sub.status, sub.code, sub.retry_after) == (429, busclient.SOURCE_RATE_LIMITED, "7") and m.streams == []


def test_bus_listener_open_closes_its_connection_when_it_raises(sse_listener, monkeypatch):
    # A subscribe the server never answers times out inside open(): the
    # connection is closed before the exception leaves, with nothing tracking
    # it (FakeMachine keeps only listeners that opened).
    s = sse_listener
    made = []
    cls = busclient.http.client.HTTPSConnection
    connect = cls.connect

    def recording(self):
        connect(self)
        made.append((self, self.sock))

    monkeypatch.setattr(cls, "connect", recording)
    m = busclient.FakeMachine(ident.ALICE, "127.0.0.1", _Cert(s))
    with pytest.raises(TimeoutError) as raised:
        m.subscribe("hang", timeout=0.5)
    (conn, sock), = made
    assert sock.fileno() == -1, "the subscribe's socket is still open after open() raised"
    assert conn.sock is None and m.streams == []
    del raised


def test_bound_id_reads_one_machine_and_refuses_a_split_binding():
    assert busclient.bound_id({"binding": None}) is None
    one = [{"kind": "machine", "id": "a"}]
    assert busclient.bound_id({"binding": {"delivery": one, "signing": one}}) == "a"
    with pytest.raises(AssertionError, match="not one machine"):
        busclient.bound_id({"binding": {"delivery": one, "signing": [{"kind": "machine", "id": "b"}]}})
    view = _PinStore(bound="a", known=["b"]).view()
    assert busclient.known_ids(view) == {"a", "b"} and "bound a" in busclient.describe_sources(view)


def test_the_pin_module_skips_on_remote_before_any_fixture():
    # The whole module is local_vm_only, so conftest's runtest hook skips each
    # of its [remote] cases before users_env (or any shed) is set up.
    import conftest

    assert tsp.pytestmark.name == "local_vm_only"

    class Item:
        callspec = type("C", (), {"params": {"users_env": "remote"}})()

        def get_closest_marker(self, name):
            return tsp.pytestmark.mark if name == "local_vm_only" else None

    with pytest.raises(pytest.skip.Exception, match="no helper requests"):
        conftest.pytest_runtest_setup(Item())


def test_the_pin_exchanges_fit_inside_the_guests_request_lifetime():
    src = (dc.REPO_ROOT / "cmd" / "shed-agent" / "http.go").read_text()
    m = re.search(r"publishTimeout\s*=\s*(\d+)\s*\*\s*time\.Second", src)
    assert m and tsp.GUEST_REQUEST_LIFETIME == int(m.group(1))
    assert tsp.DELIVERY_BUDGET + tsp.REFUSAL_TIMEOUT + 2 <= tsp.ANSWER_BY
    assert tsp.ANSWER_BY + tsp.ANSWER_TIMEOUT <= tsp.GUEST_REQUEST_LIFETIME - 3
    api = (dc.REPO_ROOT / "internal" / "api" / "plugin_handlers.go").read_text()
    k = re.search(r"sseKeepaliveInterval\s*=\s*(\d+)\s*\*\s*time\.Second", api)
    assert k and tsp.LIVENESS_TIMEOUT >= int(k.group(1)) + 10


def test_the_pin_cases_match_the_servers_and_the_clis_own_words():
    root = dc.REPO_ROOT
    types = (root / "internal" / "config" / "types.go").read_text()
    for code in (busclient.SOURCE_MACHINE_ID_REQUIRED, busclient.SOURCE_NOT_BOUND, busclient.SOURCE_DUPLICATE_STREAM,
                 busclient.SOURCE_RATE_LIMITED, busclient.SOURCE_MACHINE_NOT_FOUND):
        assert f'= "{code}"' in types, code
    handlers = (root / "internal" / "api" / "source_handlers.go").read_text()
    assert f'"{tsp.NOT_ADMIN}"' in handlers and f'"{tsp.SCRIPT_KEY}"' in handlers
    assert 'who := "user " + c.User' in handlers and 'who = "admin " + c.User' in handlers
    assert f'return "{tsp.CONTROL_SCOPE}", false' in (root / "internal" / "api" / "auth.go").read_text()
    machine = (root / "internal" / "api" / "source_machine.go").read_text()
    assert tsp.DUPLICATE in machine
    # The phrase is in the refusal itself, not only in the log line beside it.
    refusal = machine.split("const machineIDRequiredMessage =", 1)[1].split("\n\n", 1)[0]
    assert tsp.INSTALL_APP in refusal, refusal
    assert 'MachineID   string `json:"machine_id"`' in machine and 'MachineName string `json:"machine_name"`' in machine
    assert f'FeatureSourceBinding = "{tsp.FEATURE}"' in (root / "internal" / "api" / "overview.go").read_text()
    assert f'"FORBIDDEN", "{tsp.UNMATCHED}"' in (root / "internal" / "api" / "plugin_handlers.go").read_text()
    assert 'BoundByFirstConnect = "first-connect"' in (root / "internal" / "source" / "source.go").read_text()
    cli = (root / "cmd" / "shed" / "source.go").read_text()
    for words in ('"Machine %s is now the credential source for %s on %s.\\n"',
                  '"Machine %s is already the credential source for %s on %s.\\n"',
                  '"Machine %s no longer answers. "',
                  '"Credential source for %s on %s: %s, connected (%s).\\n"',
                  '"No credential source for %s on %s yet: ',
                  '"%w\\nnothing changed on %s"',
                  'return fmt.Sprintf("\\"%s\\" (%s)", termSafe(m.Name), shortSourceID(m.ID))',
                  'return "user " + sourceUserFlag',
                  '"MACHINE\\tID\\tKEY\\tBOUND\\tCONNECTED\\tFIRST SEEN"'):
        assert words in cli, words
    assert tsp._label(busclient.FakeMachine(ident.ALICE, "h", None)) == f'"itest-itest-alice" ({busclient.machine_id(ident.ALICE)[:8]})'


# ----------------------------------------------------------------------------
# A server whose users come from an account service: fixtures/directory.py
# (the world of test_directory.py), the Makefile's test-integration-directory,
# and the identity fixture's directory source (render-directory,
# SHED_ITEST_SOURCE=directory). No server: the helpers are run on local
# files, local processes and the Go sources they mirror.
# ----------------------------------------------------------------------------

import ssl  # noqa: E402

from fixtures import directory as dfx  # noqa: E402

INTEGRATION_DIR = Path(__file__).resolve().parent


def test_the_directory_module_runs_only_under_its_make_target():
    full = {"SHED_DIRECTORY_ITEST": "1", "SHED_BIN_DIR": "/repo/bin"}
    assert dfx.skip_reason(full, "linux") is None
    # Plain `make test-integration` (installed binaries): skipped, saying what to run.
    assert "run make test-integration-directory" in dfx.skip_reason({}, "linux")
    assert "run make test-integration-directory" in dfx.skip_reason({**full, "SHED_DIRECTORY_ITEST": "0"}, "linux")
    assert "Linux only" in dfx.skip_reason(full, "darwin")
    assert "SHED_BIN_DIR" in dfx.skip_reason({"SHED_DIRECTORY_ITEST": "1"}, "linux")


def test_the_directory_module_skips_under_plain_test_integration():
    # The real module, collected as `make test-integration` collects it.
    env = {k: v for k, v in os.environ.items() if k not in ("SHED_DIRECTORY_ITEST", "SHED_BIN_DIR")}
    r = subprocess.run([sys.executable, "-m", "pytest", "-q", "-rs", "-p", "no:cacheprovider", "test_directory.py"],
                       cwd=INTEGRATION_DIR, capture_output=True, text=True, timeout=120, env=env)
    # A module skipped whole collects no test: pytest exits 5 ("no tests ran").
    assert r.returncode in (0, 5), r.stdout + r.stderr
    assert "1 skipped" in r.stdout and "run make test-integration-directory" in r.stdout, r.stdout
    assert "failed" not in r.stdout and "error" not in r.stdout.lower(), r.stdout


def _make_recipe(target: str) -> tuple[str, str]:
    text = (dc.REPO_ROOT / "Makefile").read_text()
    m = re.search(rf"^{re.escape(target)}:([^\n]*)\n((?:\t[^\n]*\n)+)", text, re.MULTILINE)
    assert m, target
    return m.group(1), m.group(2)


def test_the_directory_target_builds_then_runs_the_module_against_no_other_server():
    deps, recipe = _make_recipe("test-integration-directory")
    # The host check first, then everything the world runs from bin/.
    assert deps.split() == ["check-directory-itest-host", "build", "directory-ref", "provision-ref"]
    assert "uv run pytest -v $(DIRECTORY_ITEST_FILES) $(PYTEST_ARGS)" in recipe
    files = re.search(r"^DIRECTORY_ITEST_FILES \?= (.*)$", (dc.REPO_ROOT / "Makefile").read_text(), re.MULTILINE)
    assert files and files.group(1).split() == ["test_directory.py", "test_delivery_hosted.py"]
    assert 'PATH="$(CURDIR)/bin:$$PATH"' in recipe
    assert "SHED_DIRECTORY_ITEST=1" in recipe and 'SHED_BIN_DIR="$(CURDIR)/bin"' in recipe
    # Every server variable of the rest of the suite is set empty.
    conftest_src = (INTEGRATION_DIR / "conftest.py").read_text()
    names = set(re.findall(r'os\.environ\.get\(\s*"(SHED_(?:VZ|FC)_[A-Z_]+|SHED_REMOTE_SERVER)"', conftest_src))
    assert names >= {"SHED_VZ_SERVER", "SHED_FC_HOST", "SHED_FC_SERVER", "SHED_REMOTE_SERVER"}, names
    for name in names | {"SHED_ITEST_SOURCE", "SHED_ITEST_DIRECTORY_CONTROL", "SHED_ITEST_DIRECTORY_LOG"}:
        assert re.search(rf"^\s*{name}= \\$", recipe, re.MULTILINE), f"{name} is not set empty"
    _, check = _make_recipe("check-directory-itest-host")
    assert '"$$(uname -s)" != "Linux"' in check and "exit 1" in check
    _, ref = _make_recipe("directory-ref")
    assert "-o bin/test/shed-directory-ref ./internal/directory/refserver/cmd/shed-directory-ref" in ref


def test_free_ports_never_picks_a_forbidden_or_repeated_port():
    seq = iter([8000, 12222, 40000, 18443, 40000, 40001, 40002])
    assert dfx.free_ports(2, pick=lambda: next(seq)) == [40000, 40001]
    with pytest.raises(RuntimeError, match="no 2 free loopback ports"):
        dfx.free_ports(2, pick=lambda: 8000, attempts=5)
    got = dfx.free_ports(3)
    assert len(set(got)) == 3 and not set(got) & dfx.FORBIDDEN_PORTS
    assert {8000, 8100, 8114, 5179, 5533, 1126, 8126, 12222, 18443, 12290, 18490, 12936, 18936} <= dfx.FORBIDDEN_PORTS


def test_the_worlds_folder_is_short_enough_for_its_sockets():
    assert dfx.socket_base("/tmp") == "/tmp"
    assert dfx.socket_base("/tmp/x" * 3) == "/tmp/x" * 3
    assert dfx.socket_base("/tmp/" + "d" * 80) == "/tmp"


def test_agent_arch_names_the_go_architecture():
    assert dfx.agent_arch("x86_64") == "amd64"
    assert dfx.agent_arch("aarch64") == "arm64"
    assert dfx.agent_arch("arm64") == "arm64"
    with pytest.raises(RuntimeError):
        dfx.agent_arch("riscv64")


def test_ssh_fingerprint_is_ssh_keygens(tmp_path):
    if shutil.which("ssh-keygen") is None:
        pytest.skip("ssh-keygen is not installed")
    key = tmp_path / "k"
    subprocess.run(["ssh-keygen", "-t", "ed25519", "-N", "", "-q", "-f", str(key)], check=True, timeout=30)
    pub = key.with_suffix(".pub").read_text()
    r = subprocess.run(["ssh-keygen", "-l", "-E", "sha256", "-f", str(key.with_suffix(".pub"))],
                       capture_output=True, text=True, check=True, timeout=30)
    assert dfx.ssh_fingerprint(pub) == r.stdout.split()[1]


def _go_reserved_names() -> set[str]:
    src = (dc.REPO_ROOT / "internal" / "directory" / "refserver" / "directory.go").read_text()
    m = re.search(r"reservedNames\s*=\s*map\[string\]bool\{([^}]*)\}", src)
    assert m
    return set(re.findall(r'"([a-z]+)":\s*true', m.group(1)))


def test_the_server_config_is_a_directory_server_on_the_test_drivers(tmp_path):
    cfg = dfx.server_config(http_port=1, https_port=2, ssh_port=3, state_dir=tmp_path / "state",
                            issuer="http://127.0.0.1:9", admin_key="ssh-ed25519 AAAA", agent_dir=tmp_path / "agent")
    assert cfg["bind_address"] == "127.0.0.1"
    assert cfg["state_dir"] == str(tmp_path / "state")
    assert cfg["default_backend"] == "remote"
    auth = cfg["auth"]
    assert auth["mode"] == "mtls"
    assert auth["directory"] == {"url": "http://127.0.0.1:9", "refresh": "1m", "trust": "2m", "allow_plain_http": True}
    assert auth["users"] == {dfx.LOCAL_ADMIN: {"admin": True, "keys": [{"key": "ssh-ed25519 AAAA", "label": "itest"}]}}
    remote = cfg["remote"]
    assert (remote["provider"], remote["transport"], remote["allow_test_drivers"]) == ("fake", "local", True)
    assert remote["agent_dir"] == str(tmp_path / "agent") and remote["agent_install"] == "never"
    assert remote["firewall"]["source_ranges"]
    assert remote["limits"] == {"max_running": 8, "max_sheds": 12}
    # The local admin can be named in a list: the reference server serves no
    # reserved name (it refuses the whole list), and `localadmin` is not one.
    reserved = _go_reserved_names()
    assert "admin" in reserved and dfx.LOCAL_ADMIN not in reserved


def test_the_server_config_loads(tmp_path):
    binary = dc.REPO_ROOT / "bin" / "shed-server"
    if not binary.exists():
        pytest.skip("bin/shed-server is not built (make build)")
    agent = tmp_path / "agent"
    agent.mkdir()
    cfg = dfx.server_config(http_port=40001, https_port=40002, ssh_port=40003, state_dir=tmp_path / "state",
                            issuer="http://127.0.0.1:40004",
                            admin_key="ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIGuffgNov86IRo5236ko3Fma9npCn7RtJrpSU6FFijOs",
                            agent_dir=agent)
    path = tmp_path / "server.yaml"
    path.write_text(yaml.safe_dump(cfg))
    r = subprocess.run([str(binary), "--config", str(path), "config-validate"], capture_output=True, text=True,
                       timeout=60, env={**os.environ, "HOME": str(tmp_path)})
    assert r.returncode == 0, r.stdout + r.stderr
    assert "local admins: localadmin" in r.stdout, r.stdout


def test_listed_users_and_the_control_files(tmp_path):
    u = dfx.listed_user("pat", [("ssh-ed25519 AAAA", "laptop")], admin=True)
    assert u == {"name": "pat", "admin": True, "keys": [{"key": "ssh-ed25519 AAAA", "label": "laptop"}]}
    path = tmp_path / "users.json"
    path.write_text("old")
    path.chmod(0o644)
    dfx.write_private(path, b"[]")
    assert path.read_bytes() == b"[]"
    assert stat.S_IMODE(path.stat().st_mode) == 0o600
    assert sorted(p.name for p in tmp_path.iterdir()) == ["users.json"]


def test_parse_issuer_reads_the_reference_servers_line():
    assert dfx.parse_issuer("listening on http://127.0.0.1:41234\nenrolled x srv_1\n") == "http://127.0.0.1:41234"
    assert dfx.parse_issuer("enrolled x srv_1\n") is None
    # Only a loopback plain-HTTP issuer, the only kind it serves.
    assert dfx.parse_issuer("listening on http://192.0.2.1:41234\n") is None


def test_cert_user_reads_both_openssl_subject_formats():
    fp = "SHA256:abc+/def"
    assert dfx.cert_user(f"subject=UID=jay,CN={fp},OU=control,O=cli") == "jay"
    assert dfx.cert_user(f'subject=O = cli, OU = control, CN = "{fp}", UID = jay') == "jay"
    assert dfx.cert_user(f"subject=CN={fp},OU=control,O=cli") is None
    assert dfx.cert_user("subject=O = cli, OU = control, CN = itest") is None


def test_a_request_config_makes_the_subject_the_ca_issues(tmp_path):
    if shutil.which("openssl") is None:
        pytest.skip("openssl is not installed")
    fp = "SHA256:Ab+/0123456789abcdefghijklmnopqrstuvwxyzABCDEFG"
    for user in ("jay", None):
        cnf, key, csr = tmp_path / "r.cnf", tmp_path / "k.pem", tmp_path / "c.csr"
        cnf.write_text(dfx.cert_request_config(fp, "control", "cli", user))
        subprocess.run(["openssl", "ecparam", "-name", "prime256v1", "-genkey", "-noout", "-out", str(key)],
                       check=True, capture_output=True, timeout=30)
        subprocess.run(["openssl", "req", "-new", "-key", str(key), "-config", str(cnf), "-out", str(csr)],
                       check=True, capture_output=True, timeout=30)
        subject = subprocess.run(["openssl", "req", "-in", str(csr), "-noout", "-subject", "-nameopt", "RFC2253"],
                                 check=True, capture_output=True, text=True, timeout=30).stdout
        assert dfx.cert_user(subject) == user, subject
        assert "OU=control" in subject and "O=cli" in subject and "CN=SHA256:Ab\\+/0123" in subject, subject
    assert "clientAuth" in dfx.CLIENT_CERT_EXTENSIONS and "CA:FALSE" in dfx.CLIENT_CERT_EXTENSIONS


def test_parse_stamp_reads_the_servers_instants():
    assert dfx.parse_stamp("2026-11-17T03:36:00Z").isoformat() == "2026-11-17T03:36:00+00:00"


@pytest.mark.skipif(not Path("/proc/self/stat").exists(), reason="needs /proc")
def test_owned_processes_find_what_runs_below_a_root():
    proc = subprocess.Popen(["sh", "-c", "sleep 30 & wait"])
    try:
        root = dfx.ProcHandle.pin(proc.pid)
        assert root is not None
        assert dfx._wait_for(lambda: bool(dfx.owned_processes(None, roots=[root])), 5)
        kids = dfx.owned_processes(None, roots=[root])
        assert kids and all("sleep" in h.cmdline for h in kids) and root.pid not in [h.pid for h in kids], kids
        assert len(dfx.stop_survivors(kids, "started by the test")) == len(kids)
        proc.wait(timeout=10)
    finally:
        if proc.poll() is None:
            root = dfx.ProcHandle.pin(proc.pid)
            dfx.stop_survivors(dfx.owned_processes(None, roots=[root]) if root else [], "started by the test")
            proc.kill()
            proc.wait(timeout=10)


def test_a_probe_tells_admitted_from_refused():
    admitted = dfx.Probe(1, "", f"this server requires auth.mode: mtls; {dfx.NEEDS_CSR}\n", 0.1)
    refused = dfx.Probe(255, "", "_bootstrap@127.0.0.1: Permission denied (publickey).\n", 0.1)
    other = dfx.Probe(1, "", "bootstrap: key not authorized\n", 0.1)
    assert admitted.admitted and not admitted.refused
    assert refused.refused and not refused.admitted
    assert not other.admitted and not other.refused


def test_a_session_echoes_and_closes(tmp_path):
    s = dfx.Session(["sh", "-c", "echo ready; exec cat"], os.environ, tmp_path / "err")
    try:
        assert s.read_until(b"ready\n", 10)
        assert s.echo("one") and s.echo("two")
        assert not s.wait_closed(0.2)
    finally:
        s.close()
    assert s.proc.poll() is not None
    # A session whose far side ended: its echo is not answered.
    s = dfx.Session(["sh", "-c", "echo ready"], os.environ, tmp_path / "err2")
    try:
        assert s.read_until(b"ready\n", 10)
        assert s.wait_closed(10)
        assert not s.echo("lost", timeout=1)
    finally:
        s.close()


def test_the_directory_cases_match_the_servers_own_words():
    root = dc.REPO_ROOT
    bootstrap = (root / "internal" / "sshd" / "bootstrap.go").read_text()
    assert f'"this server requires auth.mode: mtls; {dfx.NEEDS_CSR}"' in bootstrap
    assert f'errors.New("{dfx.BOOTSTRAP_NOT_AUTHORIZED}")' in bootstrap
    assert f'"key %s {dfx.KEY_MOVED}"' in (root / "internal" / "sshd" / "identity.go").read_text()
    runtime = (root / "internal" / "directory" / "runtime.go").read_text()
    assert f'r.logf("{dfx.UNKNOWN_KEY_PULL}%s: pulling the list' in runtime
    # A shed name's background pull has its own prefix, which the count of
    # first-login pulls (UNKNOWN_KEY_PULL) does not take in.
    assert f'r.logf("{dfx.SHED_NAME_PULL}%s for an existing shed: pulling the list in the background' in runtime
    assert not dfx.SHED_NAME_PULL.startswith(dfx.UNKNOWN_KEY_PULL)
    assert not dfx.UNKNOWN_KEY_PULL.startswith(dfx.SHED_NAME_PULL)
    assert f'"{dfx.STARTUP_NO_COPY}"' in runtime
    assert f'fmt.Sprintf("{dfx.STARTUP_KEPT_COPY}%s, version' in runtime
    assert "UnknownKeyEvery = time.Minute" in runtime and dfx.UNKNOWN_KEY_EVERY == 60
    assert f"UnknownKeyWait = {dfx.UNKNOWN_KEY_WAIT} * time.Second" in runtime
    lookup = (root / "internal" / "directory" / "lookup.go").read_text()
    assert f'log.Printf("{dfx.TRUST_ENDED}:' in lookup
    assert f"TrustCheckEvery = {dfx.TRUST_TICK} * time.Second" in lookup
    assert f'SkipLocalAdminName SkipReason = "{dfx.SKIPPED_LOCAL_ADMIN_NAME}"' in lookup
    assert f'fmt.Fprintf(w, "{dfx.PULL_GOOD}: %s\\n"' in (root / "cmd" / "shed" / "users.go").read_text()
    https_line, ssh_line = dfx.READY_LINES
    assert f'log.Printf("{https_line} %s"' in (root / "cmd" / "shed-server" / "serve.go").read_text()
    assert f'log.Printf("{ssh_line} %s"' in (root / "internal" / "sshd" / "server.go").read_text()
    # The skipped entry's log line the local-admin cases match, and the
    # reason a list entry with a local admin's key is skipped for.
    assert 'r.logf("directory: list entry %q skipped: %s"' in runtime
    assert dfx.SKIPPED_LOG.replace('"{name}"', "%q") + "%s" == "directory: list entry %q skipped: %s"
    assert f'SkipLocalAdminKey SkipReason = "{dfx.SKIPPED_LOCAL_ADMIN_KEY}"' in lookup
    # An mtls listener refuses a user certificate at the handshake, through
    # VerifyConnection (crypto/tls answers its error with bad_certificate).
    serve = (root / "cmd" / "shed-server" / "serve.go").read_text()
    assert "tlsCfg.VerifyConnection = servertls.ClientConnectionVerifier(authorized, clientKnown)" in serve
    assert "MinVersion:   tls.VersionTLS12," in serve
    # The files of the server's state that are meant to hold a private key.
    assert 'filepath.Join(stateDir, "tls_key.pem")' in serve
    assert 'filepath.Join(stateDir, "ca_cert.pem"), filepath.Join(stateDir, "ca_key.pem")' in serve
    assert 'filepath.Join(stateDir, "host_key")' in serve
    assert {"state/tls_key.pem", "state/ca_key.pem", "state/host_key",
            "state/directory/enrolment.json"} <= set(dfx.WORLD_PRIVATE_KEY_FILES)
    # The CLI keeps an entry's client key at ~/.shed/creds/<entry>/client.key.
    assert 'filepath.Join(GetClientConfigDir(), "creds")' in (root / "internal" / "config" / "clientcreds.go").read_text()
    assert "homes/*/.shed/creds/*/client.key" in dfx.WORLD_PRIVATE_KEY_FILES
    assert dfx.TRUST_SECONDS == 120 and dfx.TRUST == "2m"
    # The world's refresh is the shortest the config allows, and its trust
    # time two refreshes.
    assert "MinDirectoryRefresh = time.Minute" in (root / "internal" / "config" / "types.go").read_text()
    assert (dfx.REFRESH, dfx.TRUST) == ("1m", "2m")


def test_the_directory_source_is_config_by_default():
    assert ident.itest_source({}) == "config"
    assert ident.itest_source({"SHED_ITEST_SOURCE": " "}) == "config"
    assert ident.itest_source({"SHED_ITEST_SOURCE": "directory"}) == "directory"
    with pytest.raises(ValueError, match="SHED_ITEST_SOURCE"):
        ident.itest_source({"SHED_ITEST_SOURCE": "nope"})
    assert ident.skip_reason_under_directory_source("why", {}) is None
    assert ident.skip_reason_under_directory_source("why", {"SHED_ITEST_SOURCE": "directory"}).endswith(": why")


def test_directory_users_leave_out_script_and_expired_keys(keys_dir):
    got = ident.directory_users(keys_dir)
    assert [u["name"] for u in got] == sorted(ident.ITEST_USERS)
    by = {u["name"]: u for u in got}
    assert by[ident.ADMIN]["admin"] is True and by[ident.ALICE]["admin"] is False
    for name in ident.ITEST_USERS:
        assert by[name]["keys"] == [{"key": ident.public_key(name, ident.PRIMARY_KEY, keys_dir), "label": "itest"}]
    assert not {u["name"] for u in got} & _go_reserved_names()


def _ed25519_pub(seed: int) -> str:
    """A well-formed ed25519 public key line (`<type> <base64>`), made from
    `seed` (no key pair behind it: its shape is all that is checked)."""
    blob = b"\x00\x00\x00\x0bssh-ed25519\x00\x00\x00\x20" + bytes([seed]) * 32
    return "ssh-ed25519 " + base64.b64encode(blob).decode("ascii")


ED_A, ED_B = _ed25519_pub(1), _ed25519_pub(2)


def test_directory_auth_allows_plain_http_to_loopback_only():
    auth = ident.directory_auth("http://127.0.0.1:41234/", [f"{ED_A} me@example.test", ED_B])
    assert auth["mode"] == "mtls"
    assert auth["directory"] == {"url": "http://127.0.0.1:41234", "refresh": "1m", "trust": "2m",
                                 "allow_plain_http": True}
    assert auth["users"] == {ident.LOCAL_ADMIN: {"admin": True, "keys": [
        {"key": ED_A, "label": "itest"}, {"key": ED_B, "label": "itest-2"}]}}
    https = ident.directory_auth("https://accounts.example.test", [ED_A], refresh="1h", trust="72h")
    assert https["directory"] == {"url": "https://accounts.example.test", "refresh": "1h", "trust": "72h"}
    assert ident.directory_auth("http://[::1]:9", [ED_A])["directory"]["allow_plain_http"] is True
    for bad, match in (("http://accounts.example.test", "loopback"), ("https://accounts.example.test/v1", "origin"),
                       ("ftp://127.0.0.1", "origin"), ("http://u@127.0.0.1:9", "origin"),
                       # Nothing is lower-cased: an upper-case scheme or host is refused.
                       ("HTTP://127.0.0.1:9", "lower case"), ("Https://accounts.example.test", "lower case"),
                       ("http://LOCALHOST:9", "lower case"), ("https://Accounts.example.test", "lower case")):
        with pytest.raises(ValueError, match=match):
            ident.directory_auth(bad, [ED_A])
    with pytest.raises(ValueError, match="at least one key"):
        ident.directory_auth("http://127.0.0.1:9", [])
    with pytest.raises(ValueError, match="admin key 2: not a public key"):
        ident.directory_auth("http://127.0.0.1:9", [ED_A, "ssh-ed25519"])


def test_public_key_line_takes_exactly_one_public_key(tmp_path):
    assert ident.public_key_line(f"{ED_A} someone@example.test\n", "k") == ED_A
    assert ident.public_key_line(ED_B, "k") == ED_B
    private = ("-----BEGIN OPENSSH PRIVATE KEY-----\nb3BlbnNzaC1rZXktdjEAAAAABG5vbmUAAAAEbm9uZQAAAAAAAAABAAAAMwAAAAtzc2gtZW\n"
               "-----END OPENSSH PRIVATE KEY-----\n")
    for text, match in (
        (private, "private key"),
        (f"{ED_A}\n{ED_B}\n", "exactly one line.*found 2"),
        (f"{ED_A}\n\n", "exactly one line"),
        ("", "exactly one line.*found 0"),
        ("\n", "exactly one line"),
        ("ssh-dss AAAA", "not a public key line"),
        ("ssh-ed25519", "not a public key line"),
        ("ssh-ed25519 not*base64", "not base64"),
        # Base64, but not a key of the type it names.
        (f"ssh-rsa {ED_A.split()[1]}", "not a ssh-rsa key"),
        ("ssh-ed25519 " + base64.b64encode(b"\x00\x00\x00\x0bssh-ed25519").decode(), "not a ssh-ed25519 key"),
    ):
        with pytest.raises(ValueError, match=match) as e:
            ident.public_key_line(text, "/where/k.pub")
        assert str(e.value).startswith("/where/k.pub: "), e.value
    if shutil.which("ssh-keygen") is None:
        return
    # A real key pair: its .pub is taken, its private half refused.
    key = tmp_path / "k"
    subprocess.run(["ssh-keygen", "-t", "ed25519", "-N", "", "-q", "-C", "x@example.test", "-f", str(key)],
                   check=True, timeout=30)
    pub = key.with_name("k.pub").read_text()
    assert ident.public_key_line(pub, "k.pub") == " ".join(pub.split()[:2])
    with pytest.raises(ValueError, match="private key"):
        ident.public_key_line(key.read_text(), "k")


def test_render_directory_refuses_what_is_not_one_public_key(tmp_path, keys_dir):
    control = tmp_path / "control"
    control.mkdir()
    users_json = control / "users.json"
    key = tmp_path / "dev"
    subprocess.run(["ssh-keygen", "-t", "ed25519", "-N", "", "-q", "-f", str(key)], check=True, timeout=30)
    two = tmp_path / "two.pub"
    two.write_text(f"{ED_A}\n{ED_B}\n")
    for bad, words in ((key, "private key"), (two, "exactly one line")):
        r = subprocess.run(
            [sys.executable, "-m", "fixtures.identity", "--dir", str(keys_dir), "render-directory",
             "http://127.0.0.1:41234", str(users_json), "--admin-key-file", str(bad)],
            cwd=INTEGRATION_DIR, capture_output=True, text=True, timeout=60,
        )
        assert r.returncode == 1, r.stdout + r.stderr
        assert str(bad) in r.stderr and words in r.stderr, r.stderr
        assert r.stdout == "" and "PRIVATE" not in r.stderr, (r.stdout, r.stderr)
    r = subprocess.run(
        [sys.executable, "-m", "fixtures.identity", "--dir", str(keys_dir), "render-directory",
         "HTTP://127.0.0.1:41234", str(users_json)],
        cwd=INTEGRATION_DIR, capture_output=True, text=True, timeout=60,
    )
    assert r.returncode == 1 and "lower case" in r.stderr, r.stderr
    assert list(control.iterdir()) == []


def test_render_directory_leaves_no_temporary_file_when_the_write_fails(tmp_path, keys_dir):
    control = tmp_path / "control"
    control.mkdir()
    # users.json is a directory: the final rename fails.
    (control / "users.json").mkdir()
    with pytest.raises(OSError):
        ident.render_directory("http://127.0.0.1:41234", control / "users.json", keys_dir)
    assert sorted(p.name for p in control.iterdir()) == ["users.json"]


def test_render_directory_prints_the_auth_block_and_writes_the_list(tmp_path, keys_dir):
    dev = tmp_path / "dev.pub"
    dev.write_text(f"{ED_A} developer@example.test\n")
    users_json = tmp_path / "control" / "users.json"
    users_json.parent.mkdir()
    r = subprocess.run(
        [sys.executable, "-m", "fixtures.identity", "--dir", str(keys_dir), "render-directory",
         "http://127.0.0.1:41234", str(users_json), "--admin-key-file", str(dev)],
        cwd=INTEGRATION_DIR, capture_output=True, text=True, timeout=60,
    )
    assert r.returncode == 0, r.stderr
    auth = yaml.safe_load(r.stdout)["auth"]
    admin_pub = ident.public_key(ident.LOCAL_ADMIN, ident.PRIMARY_KEY, keys_dir)
    assert auth["users"][ident.LOCAL_ADMIN]["keys"] == [{"key": admin_pub, "label": "itest"},
                                                         {"key": ED_A, "label": "itest-2"}]
    assert auth["directory"]["url"] == "http://127.0.0.1:41234" and auth["directory"]["allow_plain_http"] is True
    assert json.loads(users_json.read_text()) == ident.directory_users(keys_dir)
    assert stat.S_IMODE(users_json.stat().st_mode) == 0o600
    # The local admin key is generated once and kept.
    before = ident.key_path(ident.LOCAL_ADMIN, ident.PRIMARY_KEY, keys_dir).read_bytes()
    assert ident.ensure_local_admin_key(keys_dir).read_bytes() == before


def test_the_two_users_cases_skip_under_the_directory_source_before_any_fixture(monkeypatch):
    import conftest

    skipped = {name for name, fn in vars(tu).items() if name.startswith("test_")
               and any(m.name == "not_under_directory_source" for m in getattr(fn, "pytestmark", []))}
    assert skipped == {"test_a_script_key_never_holds_the_credentials_scope", "test_an_expired_key_opens_nothing"}
    runs = {name for name in vars(tu) if name.startswith("test_")} - skipped
    assert len(runs) == 13, sorted(runs)

    marker = next(m for m in tu.test_an_expired_key_opens_nothing.pytestmark
                  if m.name == "not_under_directory_source")

    class Item:
        callspec = type("C", (), {"params": {"users_env": "fc"}})()

        def get_closest_marker(self, name):
            return marker if name == "not_under_directory_source" else None

    monkeypatch.setenv("SHED_ITEST_SOURCE", "directory")
    with pytest.raises(pytest.skip.Exception, match="skipped under the directory source.*expired key"):
        conftest.pytest_runtest_setup(Item())
    monkeypatch.delenv("SHED_ITEST_SOURCE")
    conftest.pytest_runtest_setup(Item())


# -- The directory source: what the server says, and callers that chose ------


class _FakeUsersServer:
    """A dev server entry for users_environment: `users status` says
    `source`, `users refresh` exits `refresh_rc`; every call is recorded."""

    def __init__(self, source: str = "directory", refresh_rc: int = 0) -> None:
        self.name, self.source, self.refresh_rc = "box-dev", source, refresh_rc
        self.calls: list[tuple] = []

    def cli(self, *args: str, timeout: float = 60) -> subprocess.CompletedProcess:
        self.calls.append(args)
        if args == ("--json", "users", "status"):
            return subprocess.CompletedProcess(args, 0, json.dumps({"source": self.source, "users": []}), "")
        if args == ("users", "refresh"):
            return subprocess.CompletedProcess(args, self.refresh_rc, "", "refresh failed" if self.refresh_rc else "")
        raise AssertionError(f"unexpected cli call {args}")


@pytest.fixture
def users_env_fakes(monkeypatch, keys_dir):
    """users_environment with no server and no ssh: what it asks of the
    server, whether it reconfigures, and which identities it enrols."""
    from contextlib import contextmanager

    events: list[tuple] = []

    @contextmanager
    def fake_dev_config(overrides, name, transform=None):
        events.append(("dev_config", name))
        yield

    monkeypatch.setattr(fx, "resolve_server_entry", lambda name, env=None: {"host": "127.0.0.1", "ssh_port": 2222})
    monkeypatch.setattr(dc, "dev_config", fake_dev_config)
    monkeypatch.setattr(ident, "_check_host_resolves_without_ssh_config", lambda host: None)
    monkeypatch.setattr(ident.Identity, "add_server", lambda self, timeout=90: events.append(("add_server", self.user)))
    return events, keys_dir


def test_users_environment_does_as_its_caller_asks_whatever_the_ambient_source(monkeypatch, users_env_fakes):
    events, keys_dir = users_env_fakes
    monkeypatch.setenv("SHED_ITEST_SOURCE", "directory")
    # A caller that asks to reconfigure (test_users_upgrade) reconfigures,
    # and the server is not asked about the directory.
    server = _FakeUsersServer(source="config")
    with ident.users_environment("vz", server, reconfigure=True, image="base", keys_dir=keys_dir) as env:
        assert env.alice.user == ident.ALICE
    assert events[0] == ("dev_config", "box-dev"), events
    assert [e for e in events if e[0] == "add_server"] == [("add_server", u) for u in (ident.ALICE, ident.BOB, ident.ADMIN)]
    assert server.calls == []
    # One that does not (test_service_client before this source): neither.
    events.clear()
    with ident.users_environment("fc", server, reconfigure=False, image="itest", keys_dir=keys_dir):
        pass
    assert ("dev_config", "box-dev") not in events and server.calls == []


def test_users_environment_under_the_directory_source_asks_the_server_first(users_env_fakes):
    events, keys_dir = users_env_fakes
    # A server whose users come from its config: refused before anything.
    server = _FakeUsersServer(source="config")
    with pytest.raises(pytest.fail.Exception, match="takes its users from 'config', not 'directory'"):
        with ident.users_environment("fc", server, reconfigure=False, image="base", keys_dir=keys_dir, source="directory"):
            pass
    assert server.calls == [("--json", "users", "status")] and events == []
    # The directory's: refreshed, never reconfigured, then the identities.
    server = _FakeUsersServer(source="directory")
    with ident.users_environment("fc", server, reconfigure=False, image="base", keys_dir=keys_dir, source="directory"):
        pass
    assert server.calls == [("--json", "users", "status"), ("users", "refresh")]
    assert ("dev_config", "box-dev") not in events and len(events) == 3
    # A refresh that fails stops it.
    events.clear()
    server = _FakeUsersServer(source="directory", refresh_rc=1)
    with pytest.raises(pytest.fail.Exception, match="users refresh` failed"):
        with ident.users_environment("fc", server, reconfigure=False, image="base", keys_dir=keys_dir, source="directory"):
            pass
    assert events == []
    # Asking to reconfigure under the directory source is a caller's error.
    server = _FakeUsersServer()
    with pytest.raises(ValueError, match="reconfigure=True"):
        with ident.users_environment("vz", server, reconfigure=True, image="base", keys_dir=keys_dir, source="directory"):
            pass
    with pytest.raises(ValueError, match="source 'nope'"):
        with ident.users_environment("vz", server, reconfigure=False, image="base", keys_dir=keys_dir, source="nope"):
            pass
    assert server.calls == [] and events == []


def test_directory_source_problem_reads_the_servers_own_answer():
    def done(rc, out):
        return subprocess.CompletedProcess([], rc, out, "boom" if rc else "")

    assert ident.directory_source_problem(done(0, '{"source": "directory", "users": []}'), "s") is None
    assert "'config', not 'directory'" in ident.directory_source_problem(done(0, '{"source": "config"}'), "s")
    assert "None, not 'directory'" in ident.directory_source_problem(done(0, "[]"), "s")
    assert "printed no JSON" in ident.directory_source_problem(done(0, "this server has no status route"), "s")
    assert "failed (exit 1)" in ident.directory_source_problem(done(1, ""), "s")


def test_the_users_gate_follows_the_source(monkeypatch):
    monkeypatch.delenv("SHED_ITEST_USERS", raising=False)
    with pytest.raises(pytest.skip.Exception, match="SHED_ITEST_USERS=fc"):
        ident.skip_unless_users_source_ready("fc", "box-dev", "config")
    ident.skip_unless_users_source_ready("fc", "box-dev", "directory")
    monkeypatch.setenv("SHED_ITEST_USERS", "fc")
    ident.skip_unless_users_source_ready("fc", "box-dev", "config")


def test_the_upgrade_cases_skip_under_the_directory_source(monkeypatch):
    import conftest

    (mark,) = [m for m in tuu.pytestmark if m.name == "not_under_directory_source"]

    class Item:
        callspec = None

        def get_closest_marker(self, name):
            return mark if name == "not_under_directory_source" else None

    monkeypatch.setenv("SHED_ITEST_SOURCE", "directory")
    with pytest.raises(pytest.skip.Exception, match="directory source.*restarts the Mac dev server"):
        conftest.pytest_runtest_setup(Item())
    monkeypatch.delenv("SHED_ITEST_SOURCE")
    conftest.pytest_runtest_setup(Item())


def test_the_service_client_cases_users_follow_the_source(monkeypatch, users_env_fakes):
    import test_service_client as tsc

    events, keys_dir = users_env_fakes
    real = ident.users_environment
    monkeypatch.setattr(tsc, "users_environment", lambda *a, **kw: real(*a, **{**kw, "keys_dir": keys_dir}))
    fixture = tsc.fc_users._get_wrapped_function()
    # Under the directory source: no SHED_ITEST_USERS gate; the server is asked.
    monkeypatch.delenv("SHED_ITEST_USERS", raising=False)
    monkeypatch.setenv("SHED_ITEST_SOURCE", "directory")
    server = _FakeUsersServer(source="directory")
    gen = fixture(server)
    env = next(gen)
    assert env.backend == "fc" and server.calls == [("--json", "users", "status"), ("users", "refresh")]
    gen.close()
    # Under the config source: the SHED_ITEST_USERS gate, as before.
    monkeypatch.delenv("SHED_ITEST_SOURCE")
    with pytest.raises(pytest.skip.Exception, match="SHED_ITEST_USERS=fc"):
        next(fixture(_FakeUsersServer()))


# -- Processes the world did not start itself ---------------------------------


def _fake_stat(proc: Path, pid: int, name: str, state: str, start: int, ppid: int = 4) -> None:
    rest = [state] + [str(i) for i in range(4, 53)]
    rest[1] = str(ppid)
    rest[19] = str(start)
    (proc / str(pid)).mkdir(parents=True, exist_ok=True)
    (proc / str(pid) / "stat").write_text(f"{pid} ({name}) " + " ".join(rest) + "\n")


def _fake_proc(proc: Path, pid: int, start: int, cmdline: str, ppid: int = 1) -> None:
    _fake_stat(proc, pid, cmdline.split(" ")[0] or "x", "S", start, ppid)
    (proc / str(pid) / "cmdline").write_bytes(cmdline.replace(" ", "\0").encode() + b"\0")


def _reuse_on_read(monkeypatch, at: int, proc: Path, start: int, cmdline: str, before: bool) -> None:
    """Make pid `at` another process (`start`, `cmdline`) at the first
    reading of its command line: just before it (`before`) or just after."""
    real = dfx._cmdline
    done = []

    def reading(pid: int, p: Path = Path("/proc")) -> str:
        if pid == at and not done and before:
            done.append(1)
            _fake_proc(proc, at, start, cmdline)
        out = real(pid, p)
        if pid == at and not done and not before:
            done.append(1)
            _fake_proc(proc, at, start, cmdline)
        return out

    monkeypatch.setattr(dfx, "_cmdline", reading)


@pytest.mark.parametrize("before", [False, True], ids=["just after the ownership reading", "just after the handle"])
def test_owned_processes_never_select_a_reused_pid(tmp_path, monkeypatch, before):
    marker = f"{tmp_path}/world/"
    if before:
        # The handle pins an unrelated process; the pid is then the world's
        # (another start time) when its command line is read.
        _fake_proc(tmp_path, 100, 500, "unrelated-program")
        _reuse_on_read(monkeypatch, 100, tmp_path, 900, f"agent {marker}t/x", before)
    else:
        # The world's process is pinned and its command line read; it exits
        # and an unrelated process takes its pid at once.
        _fake_proc(tmp_path, 100, 500, f"agent {marker}t/x")
        _reuse_on_read(monkeypatch, 100, tmp_path, 900, "unrelated-program", before)
    assert dfx.owned_processes(marker, tmp_path, os.getuid(), 0) == []


def test_owned_processes_select_by_command_line_or_folder_and_follow_children(tmp_path):
    marker = f"{tmp_path}/world/"
    (tmp_path / "world").mkdir()
    _fake_proc(tmp_path, 100, 500, f"agent {marker}t/x")
    _fake_proc(tmp_path, 101, 600, "child-program", ppid=100)
    _fake_proc(tmp_path, 102, 700, "grandchild-program", ppid=101)
    _fake_proc(tmp_path, 103, 400, "older-than-its-parent", ppid=100)  # its parent's pid, reused
    _fake_proc(tmp_path, 104, 650, "worker")  # no marker; its folder is the world's
    os.symlink(tmp_path / "world", tmp_path / "104" / "cwd")
    _fake_proc(tmp_path, 105, 650, "unrelated-program")
    _fake_proc(tmp_path, 106, 660, "a-zombie's-child", ppid=107)
    _fake_stat(tmp_path, 107, "z", "Z", 610, 100)
    found = {h.pid: h for h in dfx.owned_processes(marker, tmp_path, os.getuid(), 0)}
    assert sorted(found) == [100, 101, 102, 104], found
    assert found[101].cmdline == "child-program " and found[100].start == 500 and found[102].ppid == 101


def test_owned_processes_follow_a_child_of_another_user(tmp_path, monkeypatch):
    marker = f"{tmp_path}/world/"
    me = os.getuid()
    other = me + 1
    _fake_proc(tmp_path, 100, 500, f"agent {marker}t/x")
    _fake_proc(tmp_path, 101, 600, "helper-run-as-another-user", ppid=100)
    _fake_proc(tmp_path, 102, 700, "its-child", ppid=101)
    _fake_proc(tmp_path, 103, 650, f"another-users-program {marker}t/y")  # names the world, not ours
    uids = {101: other, 102: other, 103: other}
    monkeypatch.setattr(dfx, "_proc_uid", lambda pid, proc=Path("/proc"): uids.get(pid, me))
    found = {h.pid: h for h in dfx.owned_processes(marker, tmp_path, me, 0)}
    assert sorted(found) == [100, 101, 102], found
    assert found[101].cmdline == "helper-run-as-another-user "


def _fake_popen(pid: int):
    class P:
        def __init__(self) -> None:
            self.pid = pid

        def poll(self):
            return None

    return P()


@pytest.mark.parametrize("before", [False, True], ids=["just after the ownership reading", "just after the handle"])
def test_a_server_stop_never_selects_a_reused_pid(tmp_path, monkeypatch, before):
    marker = f"{tmp_path}/world/t/"
    _fake_proc(tmp_path, 50, 100, "shed-server serve")
    _fake_proc(tmp_path, 100, 500, f"agent {marker}x", ppid=50)
    # The server's child exits and an unrelated process takes its pid, around
    # the first reading of its command line.
    _reuse_on_read(monkeypatch, 100, tmp_path, 900, "unrelated-program", before)
    s = dfx.ShedServer(Path("shed-server"), tmp_path / "c.yaml", tmp_path, tmp_path, tmp_path, marker)
    s.proc, s.proc_root = _fake_popen(50), tmp_path
    handed: list = []
    monkeypatch.setattr(dfx, "_stop", lambda proc, timeout=15: True)
    monkeypatch.setattr(dfx, "stop_survivors", lambda hs, what: handed.extend(hs) or [])
    assert s.stop() == []
    assert handed == [], handed


def test_a_server_stop_follows_its_children(tmp_path, monkeypatch):
    marker = f"{tmp_path}/world/t/"
    _fake_proc(tmp_path, 50, 100, "shed-server serve")
    _fake_proc(tmp_path, 100, 500, "worker-without-the-marker", ppid=50)
    _fake_proc(tmp_path, 101, 600, "its-child", ppid=100)
    _fake_proc(tmp_path, 102, 650, "unrelated-program")
    s = dfx.ShedServer(Path("shed-server"), tmp_path / "c.yaml", tmp_path, tmp_path, tmp_path, marker)
    s.proc, s.proc_root = _fake_popen(50), tmp_path
    handed: list = []
    monkeypatch.setattr(dfx, "_stop", lambda proc, timeout=15: True)
    monkeypatch.setattr(dfx, "stop_survivors", lambda hs, what: handed.extend(hs) or [])
    assert s.stop() == []
    assert sorted(h.pid for h in handed) == [100, 101], handed  # not the server itself


@pytest.mark.skipif(not Path("/proc/self/stat").exists(), reason="needs /proc")
def test_a_process_that_cannot_be_signalled_is_reported(monkeypatch):
    proc = subprocess.Popen(["sleep", "30"])
    try:
        h = dfx.ProcHandle.of(proc.pid)
        assert h is not None

        def refuse(fd, sig, *a):
            raise PermissionError(1, "Operation not permitted")

        monkeypatch.setattr(dfx.signal, "pidfd_send_signal", refuse)
        problems = dfx.stop_survivors([h], "started in the world")
        assert problems == [f"process {proc.pid} started in the world outlived it and could not be signalled "
                            f"(Operation not permitted), and is still running: {h.cmdline!r}"], problems
        assert proc.poll() is None
    finally:
        proc.kill()
        proc.wait(timeout=10)


def test_proc_stat_reads_the_start_time_after_any_name(tmp_path):
    _fake_stat(tmp_path, 7, "a) b (c", "S", 4242)
    assert dfx.proc_stat(7, tmp_path) == ("S", 4242)
    assert dfx.proc_stat(8, tmp_path) is None
    _fake_stat(tmp_path, 9, "z", "Z", 5)
    assert dfx.ProcHandle.of(9, tmp_path) is None  # a zombie is not a process to stop
    h = dfx.ProcHandle(7, 4242)
    assert h.alive(tmp_path)
    assert not dfx.ProcHandle(7, 4243).alive(tmp_path)  # the pid, reused


@pytest.mark.skipif(not Path("/proc/self/stat").exists(), reason="needs /proc")
def test_proc_stat_reads_this_process_from_the_real_proc():
    st = dfx.proc_stat(os.getpid())
    assert st is not None and st[1] > 0


@pytest.mark.skipif(not Path("/proc/self/stat").exists(), reason="needs /proc")
def test_a_proc_handle_signals_only_its_own_process():
    proc = subprocess.Popen(["sleep", "30"])
    try:
        h = dfx.ProcHandle.of(proc.pid)
        assert h is not None and h.alive()
        # The same pid with another start time is another process: not signalled.
        other = dfx.ProcHandle(proc.pid, h.start + 1)
        assert not other.alive() and other.signal(15) is False and other.stop() is None
        assert proc.poll() is None
        assert h.stop() == "stopped on SIGTERM"
        assert proc.wait(timeout=10) == -15
        # Gone: no error, nothing sent.
        assert h.signal(15) is False and h.stop() is None and not h.alive()
    finally:
        if proc.poll() is None:
            proc.kill()
            proc.wait(timeout=10)


@pytest.mark.skipif(not Path("/proc/self/stat").exists(), reason="needs /proc")
def test_owned_processes_find_an_orphan_by_its_folder_whoever_its_parent(tmp_path):
    marker = f"{tmp_path}/world/"
    # The orphan's parent exits at once (as a server that died would):
    # it is found by the folder its command line names.
    r = subprocess.run(["sh", "-c", f'{sys.executable} -c "import time; time.sleep(60)" {marker}agent '
                                    f'</dev/null >/dev/null 2>&1 & echo $!'],
                       capture_output=True, text=True, timeout=30)
    pid = int(r.stdout.strip())
    try:
        assert dfx._wait_for(lambda: any(h.pid == pid for h in dfx.owned_processes(marker)), 10)
        unrelated = subprocess.Popen(["sleep", "30"])
        try:
            found = dfx.owned_processes(marker)
            assert [h.pid for h in found] == [pid] and marker in found[0].cmdline
            problems = dfx.stop_survivors(found + found, "started in the world")
            assert problems == [f"process {pid} started in the world outlived it and was stopped on SIGTERM: "
                                f"{found[0].cmdline!r}"], problems
            assert unrelated.poll() is None
        finally:
            unrelated.kill()
            unrelated.wait(timeout=10)
        assert dfx._wait_for(lambda: dfx.owned_processes(marker) == [], 10)
        # What has gone already is passed over.
        assert dfx.stop_survivors(found, "started in the world") == []
    finally:
        h = dfx.ProcHandle.of(pid)
        if h is not None and marker in h.cmdline:
            h.stop()


# -- What the world keeps --------------------------------------------------------


_PEM = "-----BEGIN OPENSSH PRIVATE KEY-----\nb3BlbnNzaC1rZXktdjEAAAAABG5vbmUAAAAEbm9uZQAAAAAAAAABAAAAMwAAAAtzc2gtZW\n"


def test_secrets_in_reads_the_whole_world_but_the_files_meant_for_them(tmp_path):
    code = "0f" * 16

    def put(rel: str, text: str) -> None:
        p = tmp_path / rel
        p.parent.mkdir(parents=True, exist_ok=True)
        p.write_text(text)

    # Meant to hold them.
    put("control/codes", f"itest-dir {code}\n")
    put("keys/dana/id_ed25519", _PEM)
    put("certs/c3/client.key", _PEM.replace("OPENSSH", "EC"))
    put("homes/dana-2/.shed/creds/dir-itest/client.key", _PEM)
    put("state/directory/enrolment.json", json.dumps({"private_key": _PEM.replace("OPENSSH ", "")}))
    put("state/ca_key.pem", _PEM)
    put("state/tls_key.pem", _PEM)
    put("state/host_key", _PEM)
    # Clean.
    put("keys/dana/id_ed25519.pub", "ssh-ed25519 AAAA")
    put("logs/server-1.log", "a log naming -----BEGIN OPENSSH PRIVATE KEY----- in words only\n")
    assert dfx.secrets_in(tmp_path, code) == []
    # Anywhere else: the server's home and TMPDIR, its config, every log (the
    # reference server's too), identity homes, a key under another name.
    put("homes/server/.cache/x", f"code={code}")
    put("t/agent-1/agent.log", code)
    put("server.yaml", f"note: {code}\n")
    put("logs/ref.err", json.dumps({"k": _PEM}))
    put("homes/dana-2/.shed/config.yaml", _PEM)
    put("state/directory/users.json", f"{code}")
    put("control/users.json", code)
    (tmp_path / "control" / "link").symlink_to(tmp_path / "control" / "codes")
    assert dfx.secrets_in(tmp_path, code) == [
        "control/users.json: holds the enrolment code",
        "homes/dana-2/.shed/config.yaml: holds a private key",
        "homes/server/.cache/x: holds the enrolment code",
        "logs/ref.err: holds a private key",
        "server.yaml: holds the enrolment code",
        "state/directory/users.json: holds the enrolment code",
        "t/agent-1/agent.log: holds the enrolment code",
    ]


_JWT = "eyJhbGciOiJSUzI1NiIsInR5cCI6ImF0K2p3dCJ9.eyJzdWIiOiJzcnZfMCJ9.c2lnbmF0dXJl"


def test_secrets_in_finds_a_jwt_shaped_string_outside_the_key_files(tmp_path):
    code = "0f" * 16

    def put(rel: str, text: str) -> None:
        p = tmp_path / rel
        p.parent.mkdir(parents=True, exist_ok=True)
        p.write_text(text)

    put("control/codes", f"itest-dir {code}\n")
    put("state/directory/enrolment.json", json.dumps({"note": _JWT}))  # a file meant to hold a key
    put("logs/server-1.log", "directory: good pull: version abc; eyJshort.x.y; a.b.c\n")  # not JWT-shaped
    assert dfx.secrets_in(tmp_path, code) == []
    put("logs/server-2.log", f"refused the assertion {_JWT} as sent\n")
    put("state/directory/users.json", json.dumps({"body": base64.b64encode(b"{}").decode(), "note": _JWT}))
    put("homes/server/.cache/x", _JWT.rsplit(".", 1)[0] + ".")  # header and claims, no signature
    assert dfx.secrets_in(tmp_path, code) == [
        "homes/server/.cache/x: holds a JWT-shaped string",
        "logs/server-2.log: holds a JWT-shaped string",
        "state/directory/users.json: holds a JWT-shaped string",
    ]


def test_secrets_in_searches_the_kept_copys_body_decoded(tmp_path):
    code = "0f" * 16

    def put(rel: str, text: str) -> None:
        p = tmp_path / rel
        p.parent.mkdir(parents=True, exist_ok=True)
        p.write_text(text)

    def kept(answer: dict) -> str:
        body = base64.b64encode(json.dumps(answer).encode()).decode()
        return json.dumps({"format": 1, "version": "a" * 64, "body": body})

    put("control/codes", f"itest-dir {code}\n")
    put("state/directory/users.json", kept({"contract": 1, "users": []}))
    assert dfx.secrets_in(tmp_path, code) == []
    # The token only inside the encoded body: its raw bytes are not in the file.
    put("state/directory/users.json", kept({"contract": 1, "users": [], "seen": _JWT}))
    assert _JWT.encode() not in (tmp_path / "state/directory/users.json").read_bytes()
    assert dfx.secrets_in(tmp_path, code) == ["state/directory/users.json: its kept body holds a JWT-shaped string"]
    put("state/directory/users.json", kept({"contract": 1, "users": [{"name": code}]}))
    assert dfx.secrets_in(tmp_path, code) == ["state/directory/users.json: its kept body holds the enrolment code"]


def test_the_files_meant_for_a_private_key_are_named_one_folder_at_a_time():
    assert dfx.path_matches("keys/dana/id_ed25519", "keys/*/id_ed25519")
    assert not dfx.path_matches("keys/a/b/id_ed25519", "keys/*/id_ed25519")
    assert not dfx.path_matches("keys/dana/id_ed25519.bak", "keys/*/id_ed25519")
    assert not dfx.path_matches("state/directory/users.json", "state/directory/enrolment.json")


def test_trust_deadline_is_the_last_good_pull_plus_the_trust_time():
    d = dfx.trust_deadline({"last_good_pull": "2026-11-17T03:36:00.123456789Z", "trusted_until": "ignored"})
    assert d.isoformat() == "2026-11-17T03:38:00.123456+00:00"
    assert dfx.trust_deadline({"last_good_pull": "2026-11-17T03:36:00Z"}, 60).isoformat() == "2026-11-17T03:37:00+00:00"


# -- The API: answered, refused by the server, or a failure ---------------------


def test_a_refusal_is_only_the_servers_own():
    assert dfx.http_refusal(401, {"error": {"code": "UNAUTHORIZED", "message": "x"}}) == "HTTP 401 UNAUTHORIZED"
    assert dfx.http_refusal(403, {"error": {"code": "FORBIDDEN"}}) == "HTTP 403 FORBIDDEN"
    for status, body in ((401, None), (403, {"raw": "<html>"}), (403, [1]), (500, {"error": {"code": "X"}}),
                         (200, {"sheds": []}), (None, None)):
        assert dfx.http_refusal(status, body) == "", (status, body)

    def ssl_error(reason):
        e = ssl.SSLError(1, f"[SSL: {reason}] alert")
        e.reason = reason
        return e

    assert dfx.tls_refusal(ssl_error("SSLV3_ALERT_BAD_CERTIFICATE")) == "TLS alert SSLV3_ALERT_BAD_CERTIFICATE"
    for e in (ssl_error("TLSV1_ALERT_UNKNOWN_CA"), ssl_error("CERTIFICATE_VERIFY_FAILED"), ssl.SSLEOFError(8, "eof"),
              ConnectionResetError(104, "reset"), ConnectionRefusedError(111, "refused"), OSError("x")):
        assert dfx.tls_refusal(e) == "", e
    assert dfx.ApiAnswer(None, None, "TLS alert X").refused
    assert not dfx.ApiAnswer(200, {}).refused and not dfx.ApiAnswer(500, None).refused


def test_an_api_request_that_gets_no_answer_is_a_failure_not_a_refusal(tmp_path):
    # Nothing listens.
    with socket.socket() as s:
        s.bind(("127.0.0.1", 0))
        port = s.getsockname()[1]
    with pytest.raises(dfx.TransportFailure, match="no TLS connection"):
        dfx.api_request("127.0.0.1", port, "sha256:00", ("c", "k"), "/api/sheds")
    # A listener that closes every connection at once (a server going down).
    srv = socket.socket()
    srv.bind(("127.0.0.1", 0))
    srv.listen(8)
    import threading

    def serve():
        while True:
            try:
                c, _ = srv.accept()
            except OSError:
                return
            c.close()

    t = threading.Thread(target=serve, daemon=True)
    t.start()
    try:
        with pytest.raises(dfx.TransportFailure):
            dfx.api_request("127.0.0.1", srv.getsockname()[1], "sha256:00", ("c", "k"), "/api/sheds", timeout=5)
    finally:
        srv.close()
        t.join(timeout=5)
    assert issubclass(dfx.TransportFailure, AssertionError)


# -- The world's own clean-up ----------------------------------------------------


def _fake_bin(tmp_path: Path) -> Path:
    b = tmp_path / "bin"
    (b / "test").mkdir(parents=True)
    for p in dfx.required_binaries(b):
        p.write_text("#!/bin/sh\nexit 1\n")
        p.chmod(0o755)
    return b


def test_the_world_leaves_no_folder_when_a_prerequisite_is_missing(tmp_path, monkeypatch):
    base = tmp_path / "w"
    base.mkdir()
    monkeypatch.setenv("SHED_BIN_DIR", str(_fake_bin(tmp_path)))
    monkeypatch.setattr(dfx, "socket_base", lambda tmp: str(base))
    monkeypatch.setattr(dfx.shutil, "which", lambda name: None)
    with pytest.raises(AssertionError, match="ssh and false must be on PATH"):
        with dfx.directory_world():
            pass
    assert list(base.iterdir()) == []


def test_the_world_removes_its_folder_when_it_cannot_start(tmp_path, monkeypatch):
    if shutil.which("ssh-keygen") is None or shutil.which("ssh") is None:
        pytest.skip("ssh and ssh-keygen are needed")
    base = tmp_path / "w"
    base.mkdir()
    monkeypatch.setenv("SHED_BIN_DIR", str(_fake_bin(tmp_path)))
    monkeypatch.setattr(dfx, "socket_base", lambda tmp: str(base))
    # The reference server (a script that exits at once) never comes up.
    with pytest.raises(AssertionError, match="the reference server exited"):
        with dfx.directory_world():
            pass
    assert list(base.iterdir()) == []


def test_a_held_connection_is_registered_for_clean_up_before_it_starts(tmp_path):
    from types import SimpleNamespace

    if shutil.which("ssh") is None or shutil.which("ssh-keygen") is None:
        pytest.skip("ssh and ssh-keygen are needed")
    with socket.socket() as s:
        s.bind(("127.0.0.1", 0))
        port = s.getsockname()[1]
    key = tmp_path / "k"
    subprocess.run(["ssh-keygen", "-t", "ed25519", "-N", "", "-q", "-f", str(key)], check=True, timeout=30)
    world = SimpleNamespace(ssh=shutil.which("ssh"), false=shutil.which("false"), sockets=tmp_path, logs=tmp_path,
                            known_hosts=tmp_path / "known_hosts", ssh_port=port, _n=0, _helpers=[])
    who = dfx.DirIdentity(user="u", key=key, home=tmp_path, entry="e", host="127.0.0.1", ssh_port=port,
                          shim=tmp_path / "bin" / "ssh")
    # Nothing listens: the start fails, and the world already holds it.
    with pytest.raises(AssertionError, match="was refused"):
        dfx.World.held(world, who)
    (h,) = world._helpers
    assert isinstance(h, dfx.Held) and h.proc is not None and h.proc.poll() is not None
    h.close()
    # One that never started closes too.
    dfx.Held(world, who, "_bootstrap", 9).close()


# The entry cases of test_directory.py (an entry held to its user, its own
# SSH key, the shed record on the remote backend): their helpers, and the
# CLI's and the server's words they match.


def _as_go(template: str) -> str:
    """A fixture template as the Go format string that writes it: each
    quoted placeholder is %q, each bare one %s, a line break is `\\n`."""
    return re.sub(r"\{[a-z]+\}", "%s", re.sub(r'"\{[a-z]+\}"', "%q", template)).replace("\n", "\\n")


def test_the_entry_cases_match_the_clis_own_words():
    root = dc.REPO_ROOT
    entrycert = (root / "internal" / "config" / "entrycert.go").read_text()
    assert f'"{_as_go(dfx.USER_CHANGE_REFUSAL)}\\n"' in entrycert
    stored, _, tail = dfx.STORED_UNRECORDED_REFUSAL.partition(" records no user")
    assert tail == "" and f'"{_as_go(stored)} %s; "' in entrycert
    assert 'records := "records no user (' in entrycert
    assert f'"{dfx.NO_REQUEST_SENT}. Enrol it again: %s"' in entrycert
    add = (root / "cmd" / "shed" / "server_add.go").read_text()
    # Printed by `shed server add` and by a refetch.
    assert add.count(f'fmt.Printf("{_as_go(dfx.ENTRY_USER_LINE)}"') == 2
    assert f'fmt.Printf("{_as_go(dfx.ENTRY_USER_CHANGED_LINE)}", previousUser, updated.User)' in add
    # The rendered words.
    assert dfx.USER_CHANGE_REFUSAL.format(cert="admin", entry="shed", held="pat") == (
        'the server issued a certificate for user "admin", but entry "shed" is held to user "pat"; nothing was saved.')
    assert dfx.ENTRY_USER_CHANGED_LINE.format(was="pat", now="admin") == '  User changed from "pat" to "admin"\n'
    # `shed users status` refused: the CLI's wrapping, its rendering of an API
    # error, the server's code and message.
    wrap, rest = dfx.USERS_STATUS_FORBIDDEN.split(": ", 1)
    assert wrap == "users status on {entry}"
    users = (root / "cmd" / "shed" / "users.go").read_text()
    assert 'fmt.Errorf("users status on %s: %w", name, err)' in users
    client = (root / "cmd" / "shed" / "client.go").read_text()
    assert 'fmt.Errorf("%s: %s", apiErr.Error.Code, apiErr.Error.Message)' in client
    code, message = rest.split(": ", 1)
    assert re.search(rf'ErrForbidden\s*=\s*"{code}"', (root / "internal" / "config" / "types.go").read_text())
    what = message.removeprefix("admin only: ")
    assert what != message
    assert 'writeError(w, http.StatusForbidden, config.ErrForbidden, "admin only: "+what)' in (
        root / "internal" / "api" / "policy.go").read_text()
    assert f'requireAdmin(w, r, "{what}")' in (root / "internal" / "api" / "users_handlers.go").read_text()


def test_the_token_request_lines_match_the_reference_servers(tmp_path):
    token = (dc.REPO_ROOT / "internal" / "directory" / "refserver" / "token.go").read_text()
    prefix, _, value = dfx.TOKEN_REQUEST_DIRECTORY_SCOPE.partition("scope=")
    assert prefix == dfx.TOKEN_REQUEST
    assert f'fmt.Sprintf("{dfx.TOKEN_REQUEST}scope=%q", scope[0])' in token
    assert f'"{dfx.TOKEN_REQUEST}no scope"' in token
    assert value == '"directory:read"'
    assert 's.logf("%s", tokenRequestLine(form["scope"]))' in token
    # What the fixture reads back from the reference server's log (its
    # standard error, each line after the logger's date and time).
    ref = dfx.RefServer(tmp_path / "bin", tmp_path / "control", tmp_path, tmp_path)
    assert ref.token_requests() == []
    (tmp_path / "ref.err").write_text("\n".join([
        "2026/11/19 10:00:00 token request: scope=\"directory:read\"",
        "2026/11/19 10:00:01 directory: a pull",
        "2026/11/19 10:00:02 token request: no scope",
        "2026/11/19 10:00:03 token refused: the assertion has expired",
    ]) + "\n")
    assert ref.token_requests() == [dfx.TOKEN_REQUEST_DIRECTORY_SCOPE, "token request: no scope"]


def test_the_record_case_matches_the_servers_members_and_tokens():
    root = dc.REPO_ROOT
    types = (root / "internal" / "config" / "types.go").read_text()
    m = re.search(r"^type ExecLimits struct \{\n(.*?)^\}", types, re.MULTILINE | re.DOTALL)
    assert m
    assert set(re.findall(r'json:"([a-z_]+)"', m.group(1))) == dfx.EXEC_LIMIT_MEMBERS
    assert len(dfx.EXEC_LIMIT_MEMBERS) == 9
    overview = (root / "internal" / "api" / "overview.go").read_text()
    assert f'FeatureExecLimits = "{dfx.FEATURE_EXEC_LIMITS}"' in overview
    assert f'FeatureUniformShedRecord = "{dfx.FEATURE_UNIFORM_SHED_RECORD}"' in overview
    features = re.search(r"features := \[\]string\{([^}]*)\}", overview)
    assert features and {"FeatureExecLimits", "FeatureUniformShedRecord"} <= set(re.findall(r"\w+", features.group(1)))
    caps = (root / "internal" / "backend" / "capabilities.go").read_text()
    for name, value in (("IsolationNone", "none"), ("IsolationTopology", "topology")):
        assert re.search(rf'{name}\s*=\s*"{value}"', caps), name


def _shed_folder(home: Path) -> Path:
    """A `~/.shed` holding each kind of entry the CLI may leave."""
    shed = home / ".shed"
    for d in ("creds/e", "creds/%staging/attempt-1", "creds/%staging/empty", "empty"):
        (shed / d).mkdir(parents=True)
    (shed / "config.yaml").write_text("servers: {}\n")
    (shed / "known_hosts").write_text("[127.0.0.1]:2222 ssh-ed25519 AAAA\n")
    (shed / "%config.lock").write_text("")
    (shed / "config.yaml.tmp.123").write_text("half")
    (shed / "creds" / "e" / "client.pem").write_text("cert")
    (shed / "creds" / "e" / "client.key").write_text("key")
    (shed / "creds" / "e" / "client.key").chmod(0o600)
    (shed / "creds" / "%staging" / "attempt-1" / "client.pem").write_text("staged")
    (shed / "creds" / "current").symlink_to("e")
    os.mkfifo(shed / "fifo")
    return shed


def test_shed_tree_records_every_entry_of_the_shed_folder(tmp_path):
    shed = _shed_folder(tmp_path)
    got = dfx.shed_tree(tmp_path)
    assert sorted(got) == sorted([
        ".shed", ".shed/%config.lock", ".shed/config.yaml", ".shed/config.yaml.tmp.123", ".shed/creds",
        ".shed/creds/%staging", ".shed/creds/%staging/attempt-1", ".shed/creds/%staging/attempt-1/client.pem",
        ".shed/creds/%staging/empty", ".shed/creds/current", ".shed/creds/e", ".shed/creds/e/client.key",
        ".shed/creds/e/client.pem", ".shed/empty", ".shed/fifo", ".shed/known_hosts",
    ]), sorted(got)
    assert got[".shed/creds/e/client.pem"] == ("file", 0o644 & ~_umask(), hashlib.sha256(b"cert").hexdigest())
    assert got[".shed/creds/e/client.key"][:2] == ("file", 0o600)
    assert got[".shed/creds/current"] == ("symlink", stat.S_IMODE(os.lstat(shed / "creds" / "current").st_mode), "e")
    assert got[".shed/empty"][0] == got[".shed/creds/%staging/empty"][0] == "dir"
    assert got[".shed/fifo"][0] == "other"
    # A link is never followed: the folder it names is listed once, as itself.
    assert not any(p.startswith(".shed/creds/current/") for p in got)

    # Each kind of change is seen.
    def changed(change, undo) -> bool:
        change()
        try:
            return dfx.shed_tree(tmp_path) != got
        finally:
            undo()
            assert dfx.shed_tree(tmp_path) == got

    key = shed / "creds" / "e" / "client.key"
    assert changed(lambda: (shed / "known_hosts").write_text("[127.0.0.1]:2222 ssh-ed25519 AAAB\n"),
                   lambda: (shed / "known_hosts").write_text("[127.0.0.1]:2222 ssh-ed25519 AAAA\n"))
    assert changed(lambda: key.chmod(0o644), lambda: key.chmod(0o600))
    assert changed(lambda: (shed / "creds" / "%staging" / "attempt-2").mkdir(),
                   lambda: (shed / "creds" / "%staging" / "attempt-2").rmdir())
    assert changed(lambda: (shed / "config.yaml.tmp.456").write_text(""),
                   lambda: (shed / "config.yaml.tmp.456").unlink())
    assert changed(lambda: (shed / "%config.lock").unlink(), lambda: (shed / "%config.lock").write_text(""))
    assert changed(lambda: (shed / "creds" / "current").unlink() or (shed / "creds" / "current").symlink_to("x"),
                   lambda: (shed / "creds" / "current").unlink() or (shed / "creds" / "current").symlink_to("e"))
    assert changed(lambda: (shed / "creds" / "other").symlink_to("e"), lambda: (shed / "creds" / "other").unlink())
    assert changed(lambda: (shed / "empty").chmod(got[".shed/empty"][1] ^ 0o010),
                   lambda: (shed / "empty").chmod(got[".shed/empty"][1]))
    # A time is not content: a file written again with its own bytes is no
    # change.
    st = os.stat(shed / "%config.lock")
    os.utime(shed / "%config.lock", ns=(st.st_atime_ns + 10**9, st.st_mtime_ns + 10**9))
    assert dfx.shed_tree(tmp_path) == got
    # No `.shed` at all is an empty tree.
    assert dfx.shed_tree(tmp_path / "nobody") == {}


def _umask() -> int:
    mask = os.umask(0)
    os.umask(mask)
    return mask


_TWO_ENTRIES = """\
default_server: dir-itest
servers:
    dir-itest:
        host: 127.0.0.1
        user: pat
        ssh_port: 2222
        tls_cert_fingerprint: "sha256:ab:cd"  # quoted, with a comment
        api_url: 'https://127.0.0.1:4443'
    other:
        host: 127.0.0.1
        user: bob
        extra:
            user: nested
"""


@pytest.mark.parametrize("text", [_TWO_ENTRIES, _TWO_ENTRIES.replace("    ", "  "),
                                  _TWO_ENTRIES.replace("\n", "\r\n")])
def test_drop_entry_field_removes_one_line_of_one_entry(text):
    got = dfx.drop_entry_field(text, "dir-itest", "user")
    # Exactly that line is gone; every other byte stays.
    removed = [line for line in text.splitlines(keepends=True) if line not in got.splitlines(keepends=True)]
    assert [line.strip() for line in removed] == ["user: pat"], removed
    assert len(got) == len(text) - len(removed[0])
    want = yaml.safe_load(text)
    del want["servers"]["dir-itest"]["user"]
    assert yaml.safe_load(got) == want
    # No such line, or a name that is no entry: refused.
    with pytest.raises(ValueError, match="0 'user' lines"):
        dfx.drop_entry_field(got, "dir-itest", "user")
    with pytest.raises(ValueError):
        dfx.drop_entry_field(text, "missing", "user")
    with pytest.raises(ValueError):
        dfx.drop_entry_field(text, "dir-itest", "use")


@pytest.mark.parametrize("text", [
    # The review's input: a flow mapping whose inner `user:` sits at the
    # entry's member indentation.
    "servers:\n  dir-itest:\n    extra: {\n    user: nested\n    }\n",
    "servers:\n  dir-itest:\n    host: 127.0.0.1\n    extra: [a,\n    user: x]\n",
    # A nested block member, and a key with no value on its line.
    "servers:\n  dir-itest:\n    user: pat\n    extra:\n      user: nested\n",
    "servers:\n  dir-itest:\n    user: pat\n    extra:\n",
    # Block scalars, an alias, an anchor, a tag.
    "servers:\n  dir-itest:\n    user: |\n      pat\n",
    "servers:\n  dir-itest:\n    note: >-\n      x\n    user: pat\n",
    "servers:\n  dir-itest:\n    user: *who\n",
    "servers:\n  dir-itest:\n    user: &who pat\n",
    "servers:\n  dir-itest:\n    user: !!str pat\n",
    # A quoted value that does not end on its line.
    'servers:\n  dir-itest:\n    note: "a\n      b"\n    user: pat\n',
    "servers:\n  dir-itest:\n    note: 'a\n      b'\n    user: pat\n",
    # A plain value continued on the next line.
    "servers:\n  dir-itest:\n    note: a\n      b\n    user: pat\n",
    # A sequence item, a member deeper or shallower than the first.
    "servers:\n  dir-itest:\n    - user: pat\n",
    "servers:\n  dir-itest:\n    host: 127.0.0.1\n     user: pat\n",
    "servers:\n  dir-itest:\n      host: 127.0.0.1\n    user: pat\n",
    # Tab indentation.
    "servers:\n  dir-itest:\n\tuser: pat\n",
    "servers:\n  dir-itest:\n    host: x\n  \tuser: pat\n",
])
def test_drop_entry_field_refuses_an_entry_it_cannot_edit_line_by_line(text):
    with pytest.raises(ValueError):
        dfx.drop_entry_field(text, "dir-itest", "user")


def test_drop_entry_field_judges_only_the_named_entry():
    # The other entry's nested member does not stop an edit of this one, and
    # an edit of the other one is refused for it.
    got = dfx.drop_entry_field(_TWO_ENTRIES, "dir-itest", "user")
    assert "    user: pat\n" not in got and "            user: nested\n" in got
    with pytest.raises(ValueError, match="not a one-line"):
        dfx.drop_entry_field(_TWO_ENTRIES, "other", "user")


def test_the_agent_shim_offers_the_agents_keys_and_no_key_file(tmp_path):
    ssh = dfx.real_ssh()
    if ssh is None:
        pytest.skip("ssh is not installed")
    sock, no_key, key = tmp_path / "agent.sock", tmp_path / "none" / "absent", tmp_path / "id"
    argv_dir = tmp_path / "argv"
    argv_dir.mkdir()
    key.write_text("not read by ssh -G")
    shim = tmp_path / "ssh"
    shim.write_text(dfx.agent_shim_text("pat", sock, no_key, ssh, argv_dir))
    shim.chmod(0o755)
    runs: list[list[str]] = []

    def config(*args: str, command: tuple = ()) -> dict:
        # ssh -G prints the configuration it would use and connects to nothing.
        argv = ["-G", *args, "127.0.0.1", *command]
        r = subprocess.run([str(shim), *argv], capture_output=True, text=True, timeout=30,
                           env={**os.environ, "SSH_AUTH_SOCK": str(tmp_path / "other.sock")}, stdin=subprocess.DEVNULL)
        assert r.returncode == 0, r.stderr
        runs.append(argv)
        got: dict = {}
        for line in r.stdout.splitlines():
            name, _, value = line.partition(" ")
            got.setdefault(name, []).append(value)
        return got

    plain = config()
    # The world's agent, never the caller's; no key file but the absent one
    # (so not the default key files of the account); IdentitiesOnly unset.
    assert plain["identityagent"] == [str(sock)], plain.get("identityagent")
    assert plain["identityfile"] == [str(no_key)], plain["identityfile"]
    assert plain["identitiesonly"] == ["no"]
    # Nothing the CLI is meant to set: ssh's own defaults, which the CLI's
    # options replace (an option the shim set first would win over the CLI's).
    assert {k: plain[k] for k in ("batchmode", "controlmaster", "passwordauthentication",
                                  "kbdinteractiveauthentication", "updatehostkeys")} == {
        "batchmode": ["no"], "controlmaster": ["false"], "passwordauthentication": ["yes"],
        "kbdinteractiveauthentication": ["yes"], "updatehostkeys": ["true"]}, plain
    assert "controlpath" not in plain
    cli = config("-o", "BatchMode=yes", "-o", "PasswordAuthentication=no", "-o", "KbdInteractiveAuthentication=no",
                 "-o", "UpdateHostKeys=no", "-o", "ControlMaster=auto", "-o", "ControlPath=/run/x")
    assert {k: cli[k] for k in ("batchmode", "controlmaster", "controlpath", "passwordauthentication",
                                "kbdinteractiveauthentication", "updatehostkeys")} == {
        "batchmode": ["yes"], "controlmaster": ["auto"], "controlpath": ["/run/x"], "passwordauthentication": ["no"],
        "kbdinteractiveauthentication": ["no"], "updatehostkeys": ["false"]}, cli
    # What the CLI adds for an entry with an identity file narrows the offer.
    narrowed = config("-i", str(key), "-o", "IdentitiesOnly=yes")
    assert narrowed["identitiesonly"] == ["yes"]
    assert narrowed["identityfile"] == [str(no_key), str(key)], narrowed["identityfile"]
    # Each run's arguments, as given and in order, before the shim's own; an
    # argument with a space or a newline is kept whole.
    config(command=("control", "a b\nc", ""))
    assert dfx.recorded_argv(argv_dir) == runs
    assert dfx.has_option(runs[2], "-i", str(key)) and not dfx.has_option(runs[2], "-o", "IdentityAgent=" + str(sock))
    assert not dfx.has_option(runs[2], "-i", str(no_key))


@pytest.mark.parametrize("bad", [" ", "%", "~", "$", '"', "'", "\\", "#", "\t", "\n"])
def test_the_agent_shim_refuses_a_path_ssh_would_read_as_more(tmp_path, bad):
    good = {"sock": tmp_path / "s", "no_key": tmp_path / "k", "ssh": "/usr/bin/ssh", "argv_dir": tmp_path / "a"}
    dfx.agent_shim_text("pat", **good)
    for name in good:
        args = dict(good)
        args[name] = type(good[name])(f"{good[name]}x{bad}y")
        with pytest.raises(ValueError, match="cannot hand ssh the path"):
            dfx.agent_shim_text("pat", **args)
    with pytest.raises(ValueError, match="not absolute"):
        dfx.agent_shim_text("pat", **{**good, "sock": Path("relative/s")})


def test_a_relay_counts_every_accept_and_forwards_both_ways():
    # A plain echo server as the target.
    echo = socket.socket()
    echo.bind(("127.0.0.1", 0))
    echo.listen(8)
    echo.settimeout(0.2)
    stop = threading.Event()

    def serve() -> None:
        while not stop.is_set():
            try:
                c, _ = echo.accept()
            except (socket.timeout, OSError):
                continue
            with c:
                while data := c.recv(4096):
                    c.sendall(data)

    t = threading.Thread(target=serve, daemon=True)
    t.start()
    relay = dfx.Relay(echo.getsockname()[1])
    try:
        assert relay.accepts == 0
        relay.start()
        assert relay.settle() == 0
        # Bytes both ways.
        with socket.create_connection(("127.0.0.1", relay.port), timeout=5) as c:
            c.sendall(b"ping")
            got = b""
            while len(got) < 4:
                got += c.recv(16)
            assert got == b"ping"
            c.shutdown(socket.SHUT_WR)
            assert c.recv(16) == b""
        assert relay.settle() == 1
        # A connection opened and closed with nothing sent is counted.
        socket.create_connection(("127.0.0.1", relay.port), timeout=5).close()
        assert relay.settle() == 2
        # Several at once, each counted, none lost.
        conns = [socket.create_connection(("127.0.0.1", relay.port), timeout=5) for _ in range(5)]
        for c in conns:
            c.close()
        assert relay.settle() == 7
        # A connection still open when the relay closes is ended by it.
        held = socket.create_connection(("127.0.0.1", relay.port), timeout=5)
        assert relay.settle() == 8
    finally:
        relay.close()
        stop.set()
        t.join(5)
        echo.close()
    held.settimeout(5)
    assert held.recv(16) == b""
    held.close()
    with pytest.raises(OSError):
        socket.create_connection(("127.0.0.1", relay.port), timeout=2)
    assert not any(th.is_alive() for th in relay._threads), relay._threads
    # One that never started closes too.
    dfx.Relay(1).close()


def test_a_relay_of_the_world_is_registered_for_clean_up_before_it_starts(monkeypatch):
    from types import SimpleNamespace

    world = SimpleNamespace(_helpers=[])

    def refuse(self):
        assert world._helpers == [self], "started before it was registered"
        raise OSError("no port")

    monkeypatch.setattr(dfx.Relay, "start", refuse)
    with pytest.raises(OSError):
        dfx.World.relay(world, 1)
    (r,) = world._helpers
    assert isinstance(r, dfx.Relay) and r.target == 1


def test_a_key_agent_holds_its_keys_in_order_and_is_registered_before_it_starts(tmp_path):
    from types import SimpleNamespace

    if not all(shutil.which(t) for t in ("ssh-agent", "ssh-add", "ssh-keygen")):
        pytest.skip("ssh-agent, ssh-add and ssh-keygen are needed")
    keys = []
    for label in ("first", "second"):
        path = tmp_path / label
        subprocess.run(["ssh-keygen", "-t", "ed25519", "-N", "", "-q", "-C", label, "-f", str(path)], check=True,
                       timeout=30)
        keys.append(dfx.Key(label, path, " ".join(path.with_suffix(".pub").read_text().split()[:2])))
    world = SimpleNamespace(sockets=tmp_path, logs=tmp_path, homes=tmp_path, _n=0, _helpers=[])

    def close_all() -> None:
        # The world's clean-up, here: whatever World.agent registered.
        for h in reversed(world._helpers):
            h.close()

    # The clean-up is in place before anything starts (as the live world's
    # is), so a failure inside World.agent leaves nothing running.
    try:
        agent = dfx.World.agent(world, [keys[1], keys[0]])
        (h,) = world._helpers
        assert h is agent and agent.proc.poll() is None and agent.sock.exists()
        # The order they were added, which is the order ssh offers them.
        assert agent.listed() == [keys[1].pub, keys[0].pub]
        # The agent's command line names its socket, inside the world's folder.
        assert str(tmp_path) in " ".join(agent.proc.args)
    finally:
        close_all()
    assert agent.proc.poll() is not None and not agent.sock.exists()

    # An agent whose setup fails after it started (a key ssh-add cannot
    # read) was registered first, so the clean-up stops it.
    world._helpers.clear()
    missing = dfx.Key("missing", tmp_path / "missing", keys[0].pub)
    try:
        with pytest.raises(AssertionError, match="ssh-add missing"):
            dfx.World.agent(world, [missing])
        (failed,) = world._helpers
        assert failed.proc is not None and failed.proc.poll() is None, "the agent should still run until clean-up"
    finally:
        close_all()
    assert failed.proc.poll() is not None and not failed.sock.exists()
    # One that never started closes too.
    dfx.KeyAgent("ssh-agent", "ssh-add", tmp_path / "never", tmp_path / "never.log", tmp_path).close()


# The audit log's identity shapes: fixtures/auditlog.py, which
# test_service_client.py uses on the dev server's audit.jsonl.

from fixtures import auditlog  # noqa: E402

# What the server writes (internal/audit TestIdentityShapes pins the same).
_PERSON_IDS = [
    {"kind": "user", "user": "pat", "admin": False, "script": False, "subject": "SHA256:abc", "serial": "1f"},
    {"kind": "user", "user": "admin", "admin": True, "script": False, "subject": "SHA256:abc", "serial": "1f"},
    {"kind": "user", "user": "ci", "admin": False, "script": True, "subject": "SHA256:abc"},
    {"kind": "user", "implicit": True, "admin": True, "script": False},
    {"kind": "user", "implicit": True, "admin": True, "script": False, "subject": "SHA256:abc", "serial": "1f"},
]


@pytest.mark.parametrize("identity", _PERSON_IDS)
def test_audit_person_identity_accepts_what_the_server_writes(identity):
    assert auditlog.person_identity_problems(identity) == []
    assert auditlog.person_identity_problems(identity, admin=identity["admin"]) == []
    assert auditlog.person_identity_problems(identity, admin=not identity["admin"]) != []
    # A person is never a client.
    assert auditlog.client_identity_problems(identity) != []


@pytest.mark.parametrize(
    "identity, problem",
    [
        # The retired shape, for anyone who was not a client.
        ({"kind": "admin", "subject": "SHA256:abc"}, "not 'user'"),
        ({"kind": "admin"}, "not 'user'"),
        ({"kind": "client", "name": "build-1", "serial": "3fa2"}, "not 'user'"),
        ({"kind": "system"}, "not 'user'"),
        # Both flags are always present, as booleans.
        ({"kind": "user", "user": "pat", "script": False}, "admin is None"),
        ({"kind": "user", "user": "pat", "admin": True}, "script is None"),
        ({"kind": "user", "user": "pat", "admin": "true", "script": False}, "admin is 'true'"),
        ({"kind": "user", "user": "pat", "admin": 1, "script": False}, "admin is 1"),
        # A named person has a name; the implicit user has none and is an admin.
        ({"kind": "user", "admin": True, "script": False}, "no user name"),
        ({"kind": "user", "user": "", "admin": True, "script": False}, "no user name"),
        ({"kind": "user", "implicit": True, "user": "pat", "admin": True, "script": False}, "carries a user name"),
        ({"kind": "user", "implicit": True, "admin": False, "script": False}, "not an admin"),
        ({"kind": "user", "implicit": False, "user": "pat", "admin": True, "script": False}, "true or absent"),
        ({"kind": "user", "user": "pat", "admin": True, "script": False, "subject": ""}, "subject is ''"),
        ({"kind": "user", "user": "pat", "admin": True, "script": False, "name": "x"}, "carries ['name']"),
        ("admin", "not an object"),
    ],
)
def test_audit_person_identity_refuses(identity, problem):
    problems = auditlog.person_identity_problems(identity)
    assert any(problem in p for p in problems), problems


def test_audit_client_identity():
    assert auditlog.client_identity_problems({"kind": "client", "name": "build-1", "serial": "3fa2"}) == []
    assert auditlog.client_identity_problems({"kind": "client", "name": "build-1"}) == []
    for bad, problem in (
        ({"kind": "client", "name": "build-1", "serial": "3fa2", "admin": False}, "carries ['admin']"),
        ({"kind": "client", "name": "build-1", "user": "pat", "script": False}, "carries ['script', 'user']"),
        ({"kind": "client", "serial": "3fa2"}, "no client name"),
        ({"kind": "admin", "subject": "SHA256:abc"}, "not 'client'"),
        (None, "not an object"),
    ):
        problems = auditlog.client_identity_problems(bad)
        assert any(problem in p for p in problems), (bad, problems)


def test_audit_identity_examples_in_the_configuration_reference():
    """The identity examples the configuration reference prints are shapes
    the helpers accept, one per kind, and none is the retired kind admin."""
    doc = (dc.REPO_ROOT / "docs" / "reference" / "configuration.md").read_text()
    start = doc.index("The `identity` member says who acted")
    block = doc[start:].split("```json\n", 1)[1].split("```", 1)[0]
    examples = [json.loads(line) for line in block.splitlines() if line.strip()]
    assert [e["kind"] for e in examples] == ["client", "user", "user", "system"], examples
    assert auditlog.client_identity_problems(examples[0]) == []
    assert auditlog.person_identity_problems(examples[1], admin=False) == []
    assert auditlog.person_identity_problems(examples[2], admin=True) == []
    assert examples[3] == {"kind": "system"}


# The developer's entry on the dev server test_service_client.py runs
# against: a named admin on a server with a user list, the implicit user on
# one without.
_DEV_NAMED = {"kind": "user", "user": "dev", "admin": True, "script": False, "subject": "SHA256:abc", "serial": "1f"}
_DEV_IMPLICIT = {"kind": "user", "implicit": True, "admin": True, "script": False, "subject": "SHA256:abc", "serial": "1f"}


@pytest.mark.parametrize(
    "source, with_user_list, want",
    [
        ("config", True, "dev"),
        ("config", False, None),
        ("directory", True, "localadmin"),
        ("directory", False, "localadmin"),
    ],
)
def test_developer_audit_user(source, with_user_list, want):
    assert auditlog.developer_audit_user(source, with_user_list) == want


def test_developer_audit_user_refuses_an_unknown_source():
    with pytest.raises(ValueError, match="github"):
        auditlog.developer_audit_user("github", True)


def test_audit_person_identity_expects_the_named_user():
    assert auditlog.person_identity_problems(_DEV_NAMED, admin=True, user="dev") == []
    # A server with a user list that wrote the developer as the implicit user
    # is wrong, though the line is a well-formed person and an admin.
    assert auditlog.person_identity_problems(_DEV_IMPLICIT, admin=True) == []
    problems = auditlog.person_identity_problems(_DEV_IMPLICIT, admin=True, user="dev")
    assert any("got the implicit user" in p for p in problems), problems
    problems = auditlog.person_identity_problems(_DEV_NAMED, admin=True, user="localadmin")
    assert any("user is 'dev', want 'localadmin'" in p for p in problems), problems
    # What test_service_client.py asks for under the itest user list.
    want = auditlog.developer_audit_user("config", True)
    assert auditlog.person_identity_problems(_DEV_NAMED, admin=True, user=want) == []
    assert auditlog.person_identity_problems(_DEV_IMPLICIT, admin=True, user=want) != []


def test_audit_person_identity_expects_the_implicit_user():
    assert auditlog.person_identity_problems(_DEV_IMPLICIT, admin=True, user=None) == []
    problems = auditlog.person_identity_problems(_DEV_NAMED, admin=True, user=None)
    assert any("want the implicit user" in p for p in problems), problems
    # What test_service_client.py asks for on a server with no user list.
    want = auditlog.developer_audit_user("config", False)
    assert auditlog.person_identity_problems(_DEV_IMPLICIT, admin=True, user=want) == []
    assert auditlog.person_identity_problems(_DEV_NAMED, admin=True, user=want) != []


@pytest.mark.parametrize("bad", ["", 7])
def test_audit_person_identity_refuses_a_bad_expected_user(bad):
    with pytest.raises(ValueError, match="a user name, or None"):
        auditlog.person_identity_problems(_DEV_NAMED, user=bad)


# The ssh line: a person's entry into a service client's shed over SSH
# (internal/audit TestSSHEventShape pins the same shape).

_SSH_ID = {"kind": "user", "user": "dev", "admin": True, "script": False, "subject": "SHA256:abc"}
_SSH_EXEC = {
    "ts": "2026-11-19T10:04:05.123456789Z", "event": "ssh", "identity": _SSH_ID,
    "remote": "192.0.2.10:51234", "shed": "itest-svc-1", "owner": "itest", "channel": "exec",
}


def _ssh_line(**changes):
    line = json.loads(json.dumps(_SSH_EXEC))
    for k, v in changes.items():
        if v is _DROP:
            line.pop(k, None)
        else:
            line[k] = v
    return line


_DROP = object()


def test_ssh_entry_accepts_what_the_server_writes():
    assert auditlog.ssh_entry_problems(_SSH_EXEC, shed="itest-svc-1", owner="itest", channel="exec") == []
    assert auditlog.ssh_entry_problems(
        _SSH_EXEC, shed="itest-svc-1", owner="itest", channel="exec", admin=True, user="dev") == []
    for channel in ("shell", "sftp"):
        line = _ssh_line(channel=channel)
        assert auditlog.ssh_entry_problems(line, shed="itest-svc-1", owner="itest", channel=channel) == []
    tunnel = _ssh_line(channel="tunnel", port=8080)
    assert auditlog.ssh_entry_problems(tunnel, shed="itest-svc-1", owner="itest", channel="tunnel") == []
    assert auditlog.ssh_entry_problems(tunnel, shed="itest-svc-1", owner="itest", channel="tunnel", port=8080) == []
    implicit = _ssh_line(identity={"kind": "user", "implicit": True, "admin": True, "script": False, "subject": "SHA256:abc"})
    assert auditlog.ssh_entry_problems(implicit, shed="itest-svc-1", owner="itest", channel="exec", admin=True, user=None) == []


@pytest.mark.parametrize(
    "changes, kwargs, problem",
    [
        ({"event": "exec.end"}, {}, "not 'ssh'"),
        ({"shed": "other"}, {}, "shed is 'other'"),
        ({"owner": "build-1"}, {}, "owner is 'build-1'"),
        ({"channel": "shell"}, {}, "channel is 'shell'"),
        ({"remote": _DROP}, {}, "remote is None"),
        ({"remote": ""}, {}, "remote is ''"),
        # The command never reaches the line, in any member.
        ({"cmd": "echo hi"}, {}, "carries ['cmd']"),
        ({"argv0": "echo", "argc": 2}, {}, "carries ['argc', 'argv0']"),
        ({"port": 22}, {}, "the exec line carries port"),
        ({"identity": {"kind": "client", "name": "itest", "serial": "3fa2"}}, {}, "not 'user'"),
        ({"identity": {"kind": "admin", "subject": "SHA256:abc"}}, {}, "not 'user'"),
        ({"identity": {**_SSH_ID, "serial": "1f"}}, {}, "an SSH login has none"),
        ({"identity": {k: v for k, v in _SSH_ID.items() if k != "subject"}}, {}, "no subject"),
        ({"identity": {**_SSH_ID, "admin": False}}, {"admin": True}, "admin is False, want True"),
        ({}, {"user": "localadmin"}, "user is 'dev', want 'localadmin'"),
        ({}, {"user": None}, "want the implicit user"),
    ],
)
def test_ssh_entry_refuses(changes, kwargs, problem):
    problems = auditlog.ssh_entry_problems(_ssh_line(**changes), shed="itest-svc-1", owner="itest", channel="exec", **kwargs)
    assert any(problem in p for p in problems), problems


@pytest.mark.parametrize(
    "port, want, problem",
    [
        (_DROP, None, "the line names the shed's port"),
        (0, None, "port is 0"),
        (70000, None, "port is 70000"),
        ("8080", None, "port is '8080'"),
        (True, None, "port is True"),
        (8080, 3000, "port is 8080, want 3000"),
    ],
)
def test_ssh_entry_tunnel_port(port, want, problem):
    line = _ssh_line(channel="tunnel", port=port)
    problems = auditlog.ssh_entry_problems(line, shed="itest-svc-1", owner="itest", channel="tunnel", port=want)
    assert any(problem in p for p in problems), problems


def test_ssh_entry_refuses_a_line_that_is_not_an_object_and_a_bad_channel():
    assert any("not an object" in p for p in auditlog.ssh_entry_problems(None, shed="s", owner="o", channel="exec"))
    with pytest.raises(ValueError, match="channel='session'"):
        auditlog.ssh_entry_problems(_SSH_EXEC, shed="itest-svc-1", owner="itest", channel="session")


def test_ssh_lines_by_picks_one_persons_lines_about_one_shed():
    other_admin = _ssh_line(identity={**_SSH_ID, "user": "itest-admin"})
    implicit = _ssh_line(identity={"kind": "user", "implicit": True, "admin": True, "script": False, "subject": "SHA256:abc"})
    lines = [
        _SSH_EXEC,
        other_admin,
        implicit,
        _ssh_line(shed="itest-svc-2"),
        {"event": "exec.end", "shed": "itest-svc-1", "identity": _SSH_ID},
        "not a line",
    ]
    assert auditlog.ssh_lines_by(lines, shed="itest-svc-1", user="dev") == [_SSH_EXEC]
    assert auditlog.ssh_lines_by(lines, shed="itest-svc-1", user="itest-admin") == [other_admin]
    assert auditlog.ssh_lines_by(lines, shed="itest-svc-1", user=None) == [implicit]
    assert auditlog.ssh_lines_by(lines, shed="itest-svc-3", user="dev") == []
    with pytest.raises(ValueError, match="a user name, or None"):
        auditlog.ssh_lines_by(lines, shed="itest-svc-1", user="")


def test_ssh_line_example_in_the_configuration_reference():
    """The ssh line the configuration reference prints is one the helper
    accepts, with no command in it."""
    doc = (dc.REPO_ROOT / "docs" / "reference" / "configuration.md").read_text()
    start = doc.index("An `ssh` line, for an admin's `shed exec`")
    block = doc[start:].split("```json\n", 1)[1].split("```", 1)[0]
    examples = [json.loads(line) for line in block.splitlines() if line.strip()]
    assert len(examples) == 1, examples
    ex = examples[0]
    assert auditlog.ssh_entry_problems(ex, shed=ex["shed"], owner="build-1", channel="exec", admin=True, user="pat") == []


@pytest.mark.parametrize(
    "ts, problem",
    [
        (_DROP, "every line carries its time"),
        (42, "ts is 42"),
        ("", "ts is ''"),
        ("yesterday", "not the RFC 3339 UTC time the server writes"),
        ("2026-11-19T10:04:05.123+01:00", "not the RFC 3339 UTC time the server writes"),
        # Only the first 19 characters and the final Z of these look right:
        # the whole string is matched.
        ("2026-11-19T10:04:05secretZ", "not the RFC 3339 UTC time the server writes"),
        ("2026-11-19T10:04:05.Z", "not the RFC 3339 UTC time the server writes"),
        ("2026-11-19T10:04:05.1234567891Z", "not the RFC 3339 UTC time the server writes"),
        ("2026-11-19T10:04:05.120Z", "not the RFC 3339 UTC time the server writes"),
        ("2026-11-19T10:04:05Z\n", "not the RFC 3339 UTC time the server writes"),
        (" 2026-11-19T10:04:05Z", "not the RFC 3339 UTC time the server writes"),
        ("2026-11-19 10:04:05Z", "not the RFC 3339 UTC time the server writes"),
        ("2026-13-19T10:04:05Z", "not a real date and time"),
    ],
)
def test_ssh_entry_needs_its_time(ts, problem):
    problems = auditlog.ssh_entry_problems(_ssh_line(ts=ts), shed="itest-svc-1", owner="itest", channel="exec")
    assert any(problem in p for p in problems), problems
    # The connect line's time is checked the same way.
    problems = auditlog.connect_entry_problems(_connect_line(ts=ts), shed="itest-svc-1", owner="itest")
    assert any(problem in p for p in problems), problems


@pytest.mark.parametrize(
    "ts",
    [
        "2026-11-19T10:04:05Z",  # a zero fraction is left out
        "2026-11-19T10:04:05.1Z",
        "2026-11-19T10:04:05.12Z",
        "2026-11-19T10:04:05.123456789Z",
        "2026-11-19T10:04:05.000000001Z",
    ],
)
def test_entry_lines_accept_every_time_the_server_writes(ts):
    """Go's time.RFC3339Nano of a UTC time: one to nine fraction digits with
    trailing zeros cut, or none."""
    assert auditlog.ssh_entry_problems(_ssh_line(ts=ts), shed="itest-svc-1", owner="itest", channel="exec") == []
    assert auditlog.connect_entry_problems(_connect_line(ts=ts), shed="itest-svc-1", owner="itest") == []


# The connect line: a person's tunnel into a service client's shed through
# the HTTPS Connect API (internal/audit TestConnectEventShape pins the same
# shape).

_CONNECT_ID = {"kind": "user", "user": "dev", "admin": True, "script": False, "subject": "SHA256:abc", "serial": "1f"}
_CONNECT = {
    "ts": "2026-11-19T10:06:01.234567891Z", "event": "connect",
    "identity": _CONNECT_ID, "remote": "192.0.2.10:51240", "shed": "itest-svc-1", "owner": "itest", "port": 8080,
}


def _connect_line(**changes):
    line = json.loads(json.dumps(_CONNECT))
    for k, v in changes.items():
        if v is _DROP:
            line.pop(k, None)
        else:
            line[k] = v
    return line


def test_connect_entry_accepts_what_the_server_writes():
    assert auditlog.connect_entry_problems(_CONNECT, shed="itest-svc-1", owner="itest") == []
    assert auditlog.connect_entry_problems(_CONNECT, shed="itest-svc-1", owner="itest", admin=True, user="dev", port=8080) == []
    # Open mode: the implicit user, with no certificate (no subject, no serial).
    implicit = _connect_line(identity={"kind": "user", "implicit": True, "admin": True, "script": False})
    assert auditlog.connect_entry_problems(implicit, shed="itest-svc-1", owner="itest", admin=True, user=None) == []


@pytest.mark.parametrize(
    "changes, kwargs, problem",
    [
        ({"event": "ssh"}, {}, "not 'connect'"),
        ({"ts": _DROP}, {}, "every line carries its time"),
        ({"shed": "other"}, {}, "shed is 'other'"),
        ({"owner": "build-1"}, {}, "owner is 'build-1'"),
        ({"remote": _DROP}, {}, "remote is None"),
        # The request id can come from a caller's X-Request-Id header: the
        # connect line carries none.
        ({"request_id": "host/Ab3dEf-000042"}, {}, "carries ['request_id']"),
        ({"request_id": ""}, {}, "carries ['request_id']"),
        ({"channel": "tunnel"}, {}, "carries ['channel']"),
        ({"argv0": "nc"}, {}, "carries ['argv0']"),
        ({"port": _DROP}, {}, "the line names the shed's port"),
        ({"port": 0}, {}, "port is 0"),
        ({"port": 70000}, {}, "port is 70000"),
        ({"port": "8080"}, {}, "port is '8080'"),
        ({"port": True}, {}, "port is True"),
        ({}, {"port": 3000}, "port is 8080, want 3000"),
        ({"identity": {"kind": "client", "name": "itest", "serial": "3fa2"}}, {}, "not 'user'"),
        ({"identity": {"kind": "admin", "subject": "SHA256:abc"}}, {}, "not 'user'"),
        ({"identity": {**_CONNECT_ID, "admin": False}}, {"admin": True}, "admin is False, want True"),
        ({}, {"user": "localadmin"}, "user is 'dev', want 'localadmin'"),
        ({}, {"user": None}, "want the implicit user"),
    ],
)
def test_connect_entry_refuses(changes, kwargs, problem):
    problems = auditlog.connect_entry_problems(_connect_line(**changes), shed="itest-svc-1", owner="itest", **kwargs)
    assert any(problem in p for p in problems), problems


def test_connect_entry_refuses_a_line_that_is_not_an_object():
    assert any("not an object" in p for p in auditlog.connect_entry_problems([], shed="s", owner="o"))


def test_connect_lines_by_picks_one_persons_lines_about_one_shed():
    other_admin = _connect_line(identity={**_CONNECT_ID, "user": "itest-admin"})
    lines = [
        _CONNECT,
        other_admin,
        _connect_line(shed="itest-svc-2"),
        _ssh_line(),  # an ssh line about the same shed, by the same person
        None,
    ]
    assert auditlog.connect_lines_by(lines, shed="itest-svc-1", user="dev") == [_CONNECT]
    assert auditlog.connect_lines_by(lines, shed="itest-svc-1", user="itest-admin") == [other_admin]
    assert auditlog.connect_lines_by(lines, shed="itest-svc-1", user=None) == []
    with pytest.raises(ValueError, match="a user name, or None"):
        auditlog.connect_lines_by(lines, shed="itest-svc-1", user="")


def test_connect_line_example_in_the_configuration_reference():
    """The connect line the configuration reference prints is one the helper
    accepts."""
    doc = (dc.REPO_ROOT / "docs" / "reference" / "configuration.md").read_text()
    start = doc.index("A `connect` line, for an admin's `shed tunnels`")
    block = doc[start:].split("```json\n", 1)[1].split("```", 1)[0]
    examples = [json.loads(line) for line in block.splitlines() if line.strip()]
    assert len(examples) == 1, examples
    ex = examples[0]
    assert auditlog.connect_entry_problems(ex, shed=ex["shed"], owner="build-1", admin=True, user="pat", port=8080) == []


@pytest.mark.parametrize("seq", [_DROP, 1, 42, 10**12])
def test_audit_lines_take_seq_or_none(seq):
    """A server that writes the lifecycle and delivery events puts the log's
    counter, `seq`, on every line; an older one writes none. Either is what
    the server writes."""
    line = _ssh_line(seq=seq)
    assert auditlog.ssh_entry_problems(line, shed="itest-svc-1", owner="itest", channel="exec") == []
    line = _connect_line(seq=seq)
    assert auditlog.connect_entry_problems(line, shed="itest-svc-1", owner="itest") == []


@pytest.mark.parametrize("seq", [0, -1, True, "3", 1.5, None])
def test_audit_lines_refuse_a_bad_seq(seq):
    problems = auditlog.ssh_entry_problems(_ssh_line(seq=seq), shed="itest-svc-1", owner="itest", channel="exec")
    assert any("seq is" in p for p in problems), problems
    problems = auditlog.connect_entry_problems(_connect_line(seq=seq), shed="itest-svc-1", owner="itest")
    assert any("seq is" in p for p in problems), problems


# ----------------------------------------------------------------------------
# fixtures/delivery.py: provisioning by delivery (test_delivery.py)
# ----------------------------------------------------------------------------

import random  # noqa: E402
import sys as _sys  # noqa: E402

from fixtures import delivery as dlv  # noqa: E402

_GO_LIFECYCLE = dc.REPO_ROOT / "internal" / "delivery" / "lifecycle.go"
_GO_CLI_PROVISION = dc.REPO_ROOT / "cmd" / "shed" / "provision.go"
_GO_WRITER = dc.REPO_ROOT / "internal" / "delivery" / "writer.go"
_GO_OVERVIEW = dc.REPO_ROOT / "internal" / "api" / "overview.go"
_GO_REF_MAIN = dc.REPO_ROOT / "internal" / "delivery" / "refsource" / "cmd" / "shed-provision-ref" / "main.go"
_GO_REF_SOURCE = dc.REPO_ROOT / "internal" / "delivery" / "refsource" / "source.go"


def test_delivery_texts_are_the_servers_own():
    """The warning, the expired-item line, the env file and the feature
    tokens the cases match are the ones the Go source writes."""
    life = _GO_LIFECYCLE.read_text()
    assert 'fmt.Sprintf("Credentials were not delivered (%s). Run: shed provision %s", FailureText(f), shed)' in life
    assert dlv.WARNING_FORMAT.replace("{failure}", "%s").replace("{shed}", "%s") in life
    assert ('fmt.Sprintf("Credentials were not delivered (%s). Recreate the shed from a current image.", FailureText(f))'
            in life)
    assert dlv.IMAGE_TOO_OLD_FORMAT.replace("{failure}", "%s") in life
    for form in ('"%s: %s", f.Reason, f.Rule', '"%s: %s %s", f.Reason, f.Item, f.Result', '"%s: the env file %s"'):
        assert form in life, form
    cli = _GO_CLI_PROVISION.read_text()
    assert ('"Warning: delivered item %s has expired. Run: shed provision %s\\n"' in cli)
    assert dlv.EXPIRED_FORMAT.replace("{item}", "%s").replace("{shed}", "%s") in cli
    writer = _GO_WRITER.read_text()
    env_dir, env_name = dlv.ENV_FILE.rsplit("/", 1)
    assert f'EnvFileName = "{env_name}"' in writer
    assert re.search(r"UserEnvDir\s*=\s*refusedHomeTree", writer)
    text = (dc.REPO_ROOT / "internal" / "delivery" / "text.go").read_text()
    assert f'const refusedHomeTree = "{env_dir}"' in text, env_dir
    overview = _GO_OVERVIEW.read_text()
    for f in dlv.FEATURES:
        assert f'= "{f}"' in overview, f


@pytest.mark.parametrize("reason, kw, want", [
    ("source_offline", {}, "source_offline"),
    ("answer_refused", {"rule": "refused_variable"}, "answer_refused: refused_variable"),
    ("write_failed", {"item": "itest-is-dir", "result": "is_directory"}, "write_failed: itest-is-dir is_directory"),
    ("write_failed", {"result": "io"}, "write_failed: the env file io"),
])
def test_delivery_warning_text(reason, kw, want):
    assert dlv.failure_text(reason, **kw) == want
    assert dlv.delivery_warning("s1", reason, **kw) == f"Credentials were not delivered ({want}). Run: shed provision s1"


def test_image_too_old_warning_names_a_recreate():
    """An image too old to take a delivery is never finished by `shed
    provision`, so its warning does not send the person there."""
    assert dlv.delivery_warning("s1", "image_too_old") == (
        "Credentials were not delivered (image_too_old). Recreate the shed from a current image.")


def test_delivery_warnings_are_picked_from_cli_output():
    out = ("  Requesting credentials from your source...\n"
           "  Warning: Credentials were not delivered (source_offline). Run: shed provision s1\n"
           "  Warning: Failed to clone repository (see server logs for details)\n"
           "Created shed s1\n"
           "Warning: delivered item gh has expired. Run: shed provision s1\n")
    assert dlv.delivery_warnings(out) == ["Credentials were not delivered (source_offline). Run: shed provision s1"]
    assert dlv.expired_warnings(out) == [dlv.expired_warning("s1", "gh")]
    assert dlv.delivery_warnings("Created shed s1\n") == []


def test_delivery_canned_items_follow_the_contract():
    f = dlv.file_item("k", ".ssh/id_x", b"key\n", mode="0400")
    assert f == {"name": "k", "provider": "files", "kind": "file", "stage": 1, "base": "home", "path": ".ssh/id_x",
                 "mode": "0400", "content": "a2V5Cg==", "fingerprint": "fp-k"}
    e = dlv.env_item("api.key", "EXAMPLE_KEY", "a b", expires_at="2020-01-01T00:00:00Z")
    assert e["fingerprint"] == "fp-api-key" and e["expires_at"] == "2020-01-01T00:00:00Z" and "stage" not in e
    p = dlv.file_item("p", ".env", b"x", stage=2, base="project")
    assert dlv.canned([f, e, p]) == {"items": [f, e, p], "problems": []}
    assert dlv.item_target(e) == "EXAMPLE_KEY" and dlv.item_target(p) == ".env"
    assert dlv.item_shed_path(f, None) == "/home/shed/.ssh/id_x"
    assert dlv.item_shed_path(p, "/home/shed/repo/") == "/home/shed/repo/.env"
    with pytest.raises(ValueError):
        dlv.item_shed_path(p, None)
    assert dlv.env_file_text([f, e, dlv.env_item("b", "B", "x$y")]) == "EXAMPLE_KEY=a b\nB=x$y\n"


@pytest.mark.parametrize("make", [
    lambda: dlv.file_item("Bad", "a", b"x"),
    lambda: dlv.file_item("a", ".ssh/./authorized_keys", b"x"),
    lambda: dlv.file_item("a", "a/../b", b"x"),
    lambda: dlv.file_item("a", "/abs", b"x"),
    lambda: dlv.file_item("a", "trailing/", b"x"),
    lambda: dlv.file_item("a", ".ssh/authorized_keys", b"x"),
    lambda: dlv.file_item("a", ".config/environment.d/x.conf", b"x"),
    lambda: dlv.file_item("a", "a", b"x", mode="0700"),
    lambda: dlv.file_item("a", "a", b"x", base="project"),
    lambda: dlv.file_item("a", "a", b"x" * (dlv.MAX_FILE_BYTES + 1)),
    lambda: dlv.env_item("a", "PATH", "x"),
    lambda: dlv.env_item("a", "LD_PRELOAD", "x"),
    lambda: dlv.env_item("a", "SHED_X", "x"),
    lambda: dlv.env_item("a", "lower", "x"),
    lambda: dlv.env_item("a", "A", " lead"),
    lambda: dlv.env_item("a", "A", "trail "),
    lambda: dlv.env_item("a", "A", ""),
    lambda: dlv.env_item("a", "A", "tab\there"),
    lambda: dlv.env_item("a", "A", "x" * 8193),
])
def test_delivery_canned_items_refuse_what_the_contract_refuses(make):
    with pytest.raises(ValueError):
        make()


def test_delivery_standard_bundle_carries_the_sentinel_in_values_only():
    import test_delivery as td

    s = dlv.new_sentinel()
    assert re.fullmatch(r"itestsntl[0-9a-f]{24}", s)
    items = td.standard_items(s)
    assert all(not dlv.item_problems(i) for i in items)
    for i in items:
        assert s not in i["name"] + i.get("path", "") + i.get("variable", "") + i["fingerprint"], i["name"]
    carriers = [i for i in items if (i["kind"] == "env" and s in i["value"])
                or (i["kind"] == "file" and s.encode() in dlv.item_content(i))]
    assert len(carriers) == len(items) - 1, "every content and value but the public key's carries the sentinel"
    value = next(i["value"] for i in items if i["variable" if i["kind"] == "env" else "name"] == td.VALUE_VAR)
    for ch in ("'", '"', "$", "  ", "`", "\\"):
        assert ch in value, ch
    modes = {i["mode"] for i in items if i["kind"] == "file"}
    assert modes == {"0400", "0600", "0644"}
    assert {i["stage"] for i in items if i["kind"] == "file"} == {1, 2}


def test_delivery_refsource_argv_uses_only_its_flags():
    flags = set(re.findall(r'fs\.(?:String|Bool|Duration)\("([a-z-]+)"', _GO_REF_MAIN.read_text()))
    assert {"server", "cert", "key", "ca", "machine-id", "machine-name", "items", "refuse-fixture", "silent",
            "delay", "twice", "second-connection", "answer-machine-id", "answer-cert", "answer-key", "once"} <= flags
    ident = dlv.SourceIdentity("https://127.0.0.1:1", "/ca", "/c", "/k", "a" * 32, "m")
    argv = dlv.refsource_argv("/bin/ref", ident, items_file="/i.json", silent=True, delay=8, twice=True,
                              second_connection=True, answer_machine_id="b" * 32, answer_cert=("/c2", "/k2"), once=True)
    used = {a[2:] for a in argv if a.startswith("--")}
    assert used <= flags, used - flags
    assert argv[:3] == ["/bin/ref", "--server", "https://127.0.0.1:1"]
    assert argv[argv.index("--delay") + 1] == "8s"
    assert argv[argv.index("--answer-cert") + 1:argv.index("--answer-cert") + 4] == ["/c2", "--answer-key", "/k2"]
    plain = dlv.refsource_argv("/bin/ref", ident, refuse_fixture="/f.json")
    assert "--refuse-fixture" in plain and "--items" not in plain and "--silent" not in plain
    with pytest.raises(ValueError):
        dlv.refsource_argv("/bin/ref", ident, items_file="/i", refuse_fixture="/f")


def test_delivery_event_lines():
    kinds = set(re.findall(r'Kind: "([a-z_]+)"', _GO_REF_SOURCE.read_text()))
    assert kinds == set(dlv.EVENT_KINDS), kinds
    assert dlv.parse_event_line('{"event":"request","id":"x","shed":"s"}\n') == {"event": "request", "id": "x", "shed": "s"}
    for bad in ("", "not json", "[]", '{"event":"other"}', '{"id":"x"}'):
        assert dlv.parse_event_line(bad) is None, bad


def test_delivery_source_env_carries_nothing_of_the_developers(tmp_path, monkeypatch):
    monkeypatch.setenv("SSH_AUTH_SOCK", "/tmp/agent")
    monkeypatch.setenv("HTTPS_PROXY", "http://proxy.example.test")
    env = dlv.source_env(tmp_path)
    assert set(env) == {"HOME", "PATH", "LC_ALL"} and env["HOME"] == str(tmp_path)


_FAKE_REF = """import json, sys, time
print(json.dumps({"event": "refused", "status": 409, "code": "SOURCE_DUPLICATE_STREAM"}), flush=True)
print(json.dumps({"event": "stream", "stream": "s1"}), flush=True)
print("not an event", flush=True)
print(json.dumps({"event": "request", "stream": "s1", "id": "r1", "reason": "create", "shed": "a", "user": "u"}), flush=True)
print(json.dumps({"event": "answer", "stream": "s1", "id": "r1", "attempt": 1, "status": 204}), flush=True)
time.sleep(60)
"""


def test_delivery_refsource_process_reads_events_and_stops(tmp_path):
    script = tmp_path / "fake_ref.py"
    script.write_text(_FAKE_REF)
    src = dlv.RefSource([_sys.executable, str(script)], tmp_path, label="fake")
    try:
        assert src.wait_stream(timeout=20)["stream"] == "s1"
        assert src.wait_request("a", timeout=20)["id"] == "r1"
        assert src.wait_for(lambda e: e["event"] == "answer", 20)["status"] == 204
        assert [e["id"] for e in src.requests("a")] == ["r1"] and src.requests("b") == []
        assert src.answers("r1")[0]["attempt"] == 1 and src.stream_id() == "s1"
        assert src.wait_for(lambda e: e["event"] == "end", 0.2) is None
    finally:
        t0 = time.monotonic()
        src.stop()
    assert src.proc.poll() is not None and time.monotonic() - t0 < 15
    src.stop()  # idempotent
    with pytest.raises(AssertionError, match="did not open its stream"):
        quiet = tmp_path / "quiet.py"
        quiet.write_text("import time; time.sleep(30)\n")
        q = dlv.RefSource([_sys.executable, str(quiet)], tmp_path, label="quiet")
        try:
            q.wait_stream(timeout=0.5)
        finally:
            q.stop()


def _delivery_record(**steps):
    rec = {"state": "delivered", "steps": {n: {"state": "done"} for n in dlv.STEP_NAMES},
           "last": {"at": "2026-11-23T10:00:00Z", "reason": "create", "outcome": "accepted"}, "items": []}
    for k, v in steps.items():
        rec["steps"][k] = v
    return rec


def test_delivery_record_steps_and_items():
    f = dlv.file_item("k", "a/k", b"x")
    e = dlv.env_item("v", "V", "x", expires_at="2099-01-01T00:00:00Z")
    p = dlv.file_item("p", ".env", b"x", stage=2, base="project")
    rec = _delivery_record(install={"state": "skipped", "reason": "no_hook"})
    rec["items"] = [
        {**dlv.expected_record_item(f), "written_at": "2026-11-23T10:00:01Z"},
        {**dlv.expected_record_item(e), "written_at": "2026-11-23T10:00:01Z"},
        dlv.expected_record_item(p, result="not_written"),
    ]
    assert rec["items"][1]["stage"] == 1 and "base" not in rec["items"][1]
    assert rec["items"][2]["base"] == "project" and rec["items"][2]["target"] == ".env"
    assert dlv.steps_of(rec)["install"] == ("skipped", "no_hook")
    assert dlv.steps_of({})["stage1"] == (None, None)
    assert dlv.step_problems(rec, {"stage1": ("done", None), "install": ("skipped", "no_hook")}) == []
    assert dlv.step_problems(rec, {"install": ("owed", None)}) != []
    assert dlv.record_item_problems(rec, [f, e, p], results={"p": "not_written"}) == []
    # A result, a written_at, a member that would carry a value, a missing item.
    assert dlv.record_item_problems(rec, [f, e, p]) != []
    bad = json.loads(json.dumps(rec))
    bad["items"][0]["content"] = "eA=="
    assert any("carries ['content']" in x for x in dlv.record_item_problems(bad, [f], results={}))
    bad = json.loads(json.dumps(rec))
    del bad["items"][0]["written_at"]
    assert any("written_at" in x for x in dlv.record_item_problems(bad, [f]))
    assert any("not in the record" in x for x in dlv.record_item_problems(rec, [dlv.file_item("z", "z", b"x")]))
    again = json.loads(json.dumps(rec))
    again["last"]["at"] = "2026-11-23T11:00:00Z"
    again["items"][0]["written_at"] = "2026-11-23T11:00:01Z"
    assert dlv.without_times(again) == dlv.without_times(rec) and again != rec
    again["items"][0]["fingerprint"] = "other"
    assert dlv.without_times(again) != dlv.without_times(rec)


@pytest.mark.parametrize("prefix_len", range(8))
def test_delivery_sentinel_forms_find_it_in_any_base64(prefix_len):
    s = dlv.new_sentinel()
    forms = dlv.sentinel_forms(s)
    assert forms[0] == s and all(len(f) >= 12 for f in forms) and len(forms) == 4
    rnd = random.Random(prefix_len)
    for suffix_len in range(4):
        blob = bytes(rnd.randrange(256) for _ in range(prefix_len)) + s.encode() + b"tail"[:suffix_len]
        enc = base64.b64encode(blob).decode()
        assert dlv.found_forms(enc, forms), (prefix_len, suffix_len)
        assert dlv.found_forms(json.dumps({"content": enc}).encode(), forms)
    assert dlv.found_forms(f'{{"value":"x {s} y"}}', forms) == [s]
    assert dlv.found_forms("nothing here " + base64.b64encode(b"other").decode(), forms) == []
    assert dlv.found_forms(None, forms) == []


def _scan_tree_fixture(tmp_path: Path, s: str) -> Path:
    root = tmp_path / "state"
    (root / "instances" / "a").mkdir(parents=True)
    (root / "blobs" / "sha256").mkdir(parents=True)
    (root / "instances" / "a" / "meta.json").write_text(f'{{"x": "{s}"}}')
    (root / "instances" / "a" / "clean.json").write_text("{}")
    (root / "instances" / "a" / "disk.img").write_bytes(s.encode() + b"\0" * 4096)
    (root / "blobs" / "sha256" / "blob").write_text(s)
    (root / "audit.jsonl").write_text(base64.b64encode(b"ab" + s.encode()).decode() + "\n")
    (tmp_path / "outside").write_text(s)
    (root / "link").symlink_to(tmp_path / "outside")
    return root


def test_delivery_scan_tree(tmp_path):
    s = dlv.new_sentinel()
    root = _scan_tree_fixture(tmp_path, s)
    hits = dlv.scan_tree([root], dlv.sentinel_forms(s), max_bytes=1024)
    assert sorted({Path(p).relative_to(root).as_posix() for p, _ in hits}) == ["audit.jsonl", "instances/a/meta.json"]
    assert dlv.scan_tree([root], ["absent-needle"], max_bytes=1024) == []
    with pytest.raises(dlv.ScanError, match="not a directory"):
        dlv.scan_tree([tmp_path / "missing"], [s])


_ROOT = hasattr(os, "geteuid") and os.geteuid() == 0


@pytest.mark.skipif(_ROOT, reason="root reads a mode-000 file or directory")
@pytest.mark.parametrize("what", ["file", "dir"])
def test_delivery_scan_tree_fails_on_what_it_cannot_read(tmp_path, what):
    """A file or directory the sweep cannot read fails it, naming the path:
    a sweep that skipped it would prove nothing."""
    s = dlv.new_sentinel()
    root = _scan_tree_fixture(tmp_path, s)
    target = root / "instances" / ("a" if what == "dir" else "a/clean.json")
    target.chmod(0)
    try:
        with pytest.raises(dlv.ScanError, match="could not read 1 paths") as e:
            dlv.scan_tree([root], dlv.sentinel_forms(s), max_bytes=1024)
        assert str(target) in str(e.value)
    finally:
        target.chmod(0o700)


def test_delivery_scan_tree_skips_only_a_vanished_file(tmp_path, monkeypatch):
    s = dlv.new_sentinel()
    root = _scan_tree_fixture(tmp_path, s)
    real = dlv._read_for_scan
    gone = str(root / "instances" / "a" / "meta.json")

    def vanish(path):
        if path == gone:
            raise FileNotFoundError(2, "No such file or directory", path)
        return real(path)

    monkeypatch.setattr(dlv, "_read_for_scan", vanish)
    hits = dlv.scan_tree([root], dlv.sentinel_forms(s), max_bytes=1024)
    assert sorted({Path(p).name for p, _ in hits}) == ["audit.jsonl"]

    def refuse(path):
        if path == gone:
            raise PermissionError(13, "Permission denied", path)
        return real(path)

    monkeypatch.setattr(dlv, "_read_for_scan", refuse)
    with pytest.raises(dlv.ScanError, match="cannot be read: Permission denied"):
        dlv.scan_tree([root], dlv.sentinel_forms(s), max_bytes=1024)


def test_delivery_remote_scan_command_scans_as_scan_tree(tmp_path):
    """The command run on the server's host, with `sudo -n` standing in as a
    shim that runs its arguments: the same files as `scan_tree`."""
    if shutil.which("bash") is None or shutil.which("xargs") is None:
        pytest.skip("needs bash and xargs")
    r = subprocess.run(["find", "--version"], capture_output=True, text=True)
    if r.returncode != 0:
        pytest.skip("needs GNU find (-xdev, -print0, -size -Nk)")
    s = dlv.new_sentinel()
    root = _scan_tree_fixture(tmp_path, s)
    shim = tmp_path / "bin"
    shim.mkdir()
    (shim / "sudo").write_text('#!/bin/sh\n[ "$1" = "-n" ] && shift\nexec "$@"\n')
    (shim / "sudo").chmod(0o755)
    env = {**os.environ, "PATH": f"{shim}:{os.environ['PATH']}"}
    cmd = dlv.remote_scan_command([str(root)], dlv.sentinel_forms(s), max_bytes=4096)
    r = subprocess.run(["bash", "-c", cmd], capture_output=True, text=True, env=env, timeout=60)
    found = sorted(Path(p).relative_to(root).as_posix() for p in dlv.parse_remote_scan(r.stdout))
    assert found == ["audit.jsonl", "instances/a/meta.json"], (r.stdout, r.stderr)
    r = subprocess.run(["bash", "-c", dlv.remote_scan_command([str(root)], ["absent-needle"])],
                       capture_output=True, text=True, env=env, timeout=60)
    assert dlv.parse_remote_scan(r.stdout) == []
    r = subprocess.run(["bash", "-c", dlv.remote_scan_command([str(root), str(tmp_path / "missing")], [s])],
                       capture_output=True, text=True, env=env, timeout=60)
    assert r.returncode == dlv.SCAN_MISSING_ROOT and "missing" in r.stderr

    # A file that vanishes between find and grep is skipped (a grep shim
    # removes it first); one that cannot be read fails the scan (grep's exit 2
    # is an error, never "no match"), and so does a directory find cannot
    # list.
    real_grep = shutil.which("grep")
    (shim / "grep").write_text('#!/bin/sh\nfor a in "$@"; do case "$a" in *vanish) rm -f "$a";; esac; done\n'
                               f'exec {real_grep} "$@"\n')
    (shim / "grep").chmod(0o755)
    (root / "instances" / "a" / "vanish").write_text(s)
    r = subprocess.run(["bash", "-c", cmd], capture_output=True, text=True, env=env, timeout=60)
    assert r.returncode == 0, r.stderr
    assert sorted(Path(p).name for p in dlv.parse_remote_scan(r.stdout)) == ["audit.jsonl", "meta.json"]
    (shim / "grep").unlink()
    if _ROOT:
        return  # root reads a mode-000 file or directory
    unreadable = root / "instances" / "a" / "clean.json"
    unreadable.chmod(0)
    try:
        r = subprocess.run(["bash", "-c", cmd], capture_output=True, text=True, env=env, timeout=60)
        assert r.returncode == dlv.SCAN_GREP_FAILED and str(unreadable) in r.stderr, (r.returncode, r.stderr)
        assert dlv.SCAN_DONE not in r.stdout
    finally:
        unreadable.chmod(0o600)
    locked = root / "instances" / "a"
    locked.chmod(0)
    try:
        r = subprocess.run(["bash", "-c", cmd], capture_output=True, text=True, env=env, timeout=60)
        assert r.returncode == dlv.SCAN_FIND_FAILED and "find failed" in r.stderr, (r.returncode, r.stderr)
    finally:
        locked.chmod(0o700)
    with pytest.raises(AssertionError, match="did not finish"):
        dlv.parse_remote_scan("partial output")
    with pytest.raises(ValueError):
        dlv.remote_scan_command([], [s])


def test_delivery_audit_events_skip_torn_lines():
    text = '{"event":"delivery","shed":"a"}\n{"event":"shed.cre\n\n[1]\n{"event":"shed.delete","shed":"a"}\n'
    assert [e["event"] for e in dlv.audit_events(text)] == ["delivery", "shed.delete"]


def test_delivery_server_files(monkeypatch):
    fc = dlv.server_files("fc", ssh_host="h", environ={})
    assert fc == dlv.ServerFiles("/var/lib/shed-dev/firecracker/audit.jsonl",
                                 ("/var/lib/shed-dev/firecracker", "/var/lib/shed-dev/state"), "h")
    third = dlv.server_files("fc", ssh_host="h", environ={"SHED_FC_DEV_AUDIT_LOG": "/var/lib/shed-third/firecracker/audit.jsonl"})
    assert third.state_dirs == ("/var/lib/shed-third/firecracker", "/var/lib/shed-third/state")
    moved = dlv.server_files("fc", environ={"SHED_FC_DEV_STATE_DIRS": "/a, /b"})
    assert moved.state_dirs == ("/a", "/b")
    remote = dlv.server_files("remote", environ={"SHED_REMOTE_DEV_DIR": "/r"})
    assert remote == dlv.ServerFiles("/r/remote/audit.jsonl", ("/r/remote", "/r/state"))
    vz = dlv.server_files("vz", environ={"SHED_VZ_DEV_AUDIT_LOG": "/v/audit.jsonl", "SHED_VZ_DEV_STATE_DIRS": "/v,/s"})
    assert vz == dlv.ServerFiles("/v/audit.jsonl", ("/v", "/s"))
    monkeypatch.setenv("HOME", "/Users/dev")
    vz = dlv.server_files("vz", environ={})
    assert vz.audit_log == "/Users/dev/Library/Application Support/shed-dev/vz/audit.jsonl"
    assert vz.state_dirs == ("/Users/dev/Library/Application Support/shed-dev/vz", "/Users/dev/.shed/dev/state")
    assert vz.ssh_host is None
    with pytest.raises(ValueError):
        dlv.server_files("docker", environ={})


def test_delivery_refuse_fixture_selection():
    fixtures = dlv.load_refuse_fixtures()
    assert len(fixtures) > 100
    live = [f for f in fixtures if dlv.runs_live(f)]
    left_out = {f.name for f in fixtures} - {f.name for f in live}
    assert left_out == {f.name for f in fixtures if f.request != "create-repo" or f.name.startswith("cross-base-")}
    assert all(f.request == "create-repo" and not f.name.startswith("cross-base-") for f in live)
    chosen = dlv.select_refuse_fixtures(fixtures)
    assert [f.rule for f in chosen] == sorted({f.rule for f in live}), "one fixture per rule, every rule"
    by_rule = {f.rule: f.name for f in chosen}
    for rule, name in dlv.PREFERRED.items():
        assert by_rule[rule] == name, rule
    assert all(f.status in (400, 413, 422) for f in chosen)
    everything = dlv.select_refuse_fixtures(fixtures, everything=True)
    assert {f.name for f in everything} == {f.name for f in live}
    # Every family of rules the contract names keeps a live fixture.
    rules = {f.rule for f in chosen}
    for family in ("not_json", "duplicate_member", "null_value", "unknown_member", "path_dot_segment",
                   "path_dot_dot_segment", "refused_target", "refused_variable", "nested_target",
                   "duplicate_target", "value_not_printable", "file_too_large", "total_too_large", "env_too_large"):
        assert family in rules, family


def test_delivery_paths_script_reads_files_as_the_shed_does(tmp_path):
    """PATHS_SCRIPT, run here with bash, GNU stat and base64 as in a shed."""
    for tool, arg in (("stat", ["-c", "%a", str(tmp_path)]), ("base64", ["-w0", "/dev/null"])):
        if subprocess.run([tool, *arg], capture_output=True).returncode != 0:
            pytest.skip(f"needs GNU {tool}")
    owner = subprocess.run(["stat", "-c", "%U", str(tmp_path)], capture_output=True, text=True).stdout.strip()
    d = tmp_path / ".itest-delivery"
    d.mkdir(mode=0o700)
    d.chmod(0o700)
    key = d / "id"
    key.write_bytes(b"key \x00\xff\n")
    key.chmod(0o400)
    (tmp_path / "link").symlink_to(key)
    paths = [str(key), str(d), str(tmp_path / "link"), str(tmp_path / "missing")]
    r = subprocess.run(["bash", "-c", dlv.PATHS_SCRIPT, "_", *paths], capture_output=True, text=True, timeout=30)
    got = dlv.parse_paths(r.stdout)
    assert got[str(key)] == dlv.ShedPath(str(key), "file", "400", owner, b"key \x00\xff\n")
    assert (got[str(d)].kind, got[str(d)].mode) == ("dir", "700")
    assert got[str(tmp_path / "link")].kind == "link" and got[str(tmp_path / "missing")].kind == "missing"
    item = dlv.file_item("k", ".itest-delivery/id", b"key \x00\xff\n", mode="0400")
    moved = {str(Path(dlv.item_shed_path(item, None))): dlv.ShedPath("p", "file", "400", "shed", b"key \x00\xff\n")}
    assert dlv.file_problems(moved, [item], None) == []
    wrong = {k: dlv.ShedPath("p", "file", "600", "root", b"other") for k in moved}
    assert len(dlv.file_problems(wrong, [item], None)) == 3
    assert dlv.file_problems({}, [item], None) != []


def test_delivery_image_gate():
    assert dlv.delivery_image_backends({}) == set()
    assert dlv.delivery_image_backends({"SHED_IMAGE_HAS_DELIVERY": " vz, fc ,,remote"}) == {"vz", "fc", "remote"}
    assert dlv.backend_key("firecracker") == "fc" and dlv.backend_key("vz") == "vz"


def test_delivery_activity_paging():
    events = [{"id": str(i)} for i in range(5)]

    def fetch(since):
        start = int(since) if since else 0
        return {"events": events[start:start + 2], "next": str(start + 2) if start + 2 < len(events) else ""}

    assert dlv.page_all(fetch) == events
    with pytest.raises(AssertionError, match="came twice"):
        dlv.page_all(lambda since: {"events": [{"id": "x"}], "next": "c" + str(since)})
    with pytest.raises(AssertionError, match="cursor"):
        dlv.page_all(lambda since: {"events": [{"id": since or "first"}], "next": "same"})


# ----------------------------------------------------------------------------
# The account service's deliveries (fixtures/directory.py; plan 040 §6)
# ----------------------------------------------------------------------------


def test_the_delivery_words_match_the_servers():
    root = dc.REPO_ROOT
    dlv_go = (root / "internal" / "directory" / "refserver" / "deliveries.go").read_text()
    assert f'DeliveryCallLine = "{dfx.DELIVERY_CALL}"' in dlv_go
    assert 's.logf("%s", DeliveryCallLine)' in dlv_go
    modes = re.findall(r'^\tMode[A-Za-z]+ = "([a-z]*)"$', dlv_go, re.MULTILINE)
    assert sorted(modes) == sorted(dfx.DELIVERY_MODES)
    m = re.search(r"^type Delivery struct \{\n(.*?)^\}", dlv_go, re.MULTILINE | re.DOTALL)
    assert m and set(re.findall(r'json:"([a-z_0-9]+)', m.group(1))) == dfx.DELIVERY_MEMBERS
    assert f'"{dfx.NO_SOURCE}"' in dlv_go
    _, _, scope = dfx.TOKEN_REQUEST_DELIVERY_SCOPE.partition("scope=")
    assert dfx.TOKEN_REQUEST_DELIVERY_SCOPE.startswith(dfx.TOKEN_REQUEST)
    assert f'scopeDeliveries = {scope}' in dlv_go
    hosted = (root / "internal" / "delivery" / "hosted.go").read_text()
    assert f'%q%s: {dfx.ASKING_THE_SERVICE}, deadline %s"' in hosted
    assert 'for shed %q%s: ' + dfx.ASKING_THE_SERVICE in hosted
    serve = (root / "cmd" / "shed-server" / "serve.go").read_text()
    assert dfx.BINDING_REMOVED.replace('"{user}"', "%q") in serve


def test_the_hosted_cases_words_match_the_servers():
    import test_delivery_hosted as tdh

    types = (dc.REPO_ROOT / "internal" / "config" / "types.go").read_text()
    assert f'SourceIsServiceMessage = "{tdh.SOURCE_IS_SERVICE_TEXT}"' in types
    source = (dc.REPO_ROOT / "cmd" / "shed" / "source.go").read_text()
    assert tdh.NOT_DELIVERING_TEXT + ":" in source
    provision = (dc.REPO_ROOT / "cmd" / "shed" / "provision.go").read_text()
    assert '" from the account service"' in provision
    directory = (dc.REPO_ROOT / "internal" / "config" / "directory.go").read_text()
    assert tdh.SCRIPT_KEY_REFUSED + "; remove script: true" in directory


def test_delivery_entries_and_the_document():
    items = [{"name": "k"}]
    assert dfx.delivery_answer(items, [{"code": "x"}]) == {"items": items, "problems": [{"code": "x"}]}
    assert dfx.delivery_status(500, "INTERNAL", "boom") == {
        "mode": "status", "status": 500, "body": {"error": {"code": "INTERNAL", "message": "boom"}}}
    assert dfx.delivery_status(418, text="") == {"mode": "status", "status": 418, "text": ""}
    assert dfx.delivery_status(302, headers={"Location": "x"}) == {"mode": "status", "status": 302,
                                                                   "headers": {"Location": "x"}}
    b = dfx.delivery_body(b'{"id":"0123"}', 200)
    assert b["mode"] == "body" and base64.b64decode(b["body_base64"]) == b'{"id":"0123"}'
    doc = json.loads(dfx.deliveries_document({"ada": dfx.NO_SOURCE, "bob": {"mode": "silent"}}))
    assert doc == {"ada": "NO_SOURCE", "bob": {"mode": "silent"}}
    for bad in ("no_source", 3, {"mode": "loud"}, {"items": {}}, {"mode": "status"}, {"mode": "status", "status": 99},
                {"extra": 1}, {"mode": "body", "body_base64": "!!"}):
        assert dfx.delivery_problems(bad), bad
        with pytest.raises(ValueError, match="would refuse"):
            dfx.deliveries_document({"ada": bad})


def test_what_could_be_accepted():
    safe = [dfx.NO_SOURCE, {"mode": "silent"}, dfx.delivery_status(500, "INTERNAL"),
            dfx.delivery_status(404, "NO_SOURCE"), dfx.delivery_status(302, headers={"Location": "x"}),
            dfx.delivery_status(599), dfx.delivery_body(b"{}", 422)]
    risky = [dfx.delivery_answer([]), {"mode": "late"}, {"mode": "echo"}, {"mode": "oversize"},
             {"mode": "truncated", "items": [{"name": "k"}]}, dfx.delivery_body(b"{}"),
             {"mode": "body", "body_base64": ""}, dfx.delivery_status(200, text="{}"),
             {"mode": "status", "status": 200}, dfx.delivery_status(204), dfx.delivery_status(103),
             dfx.delivery_status(100, text="{}"), dfx.delivery_body(b"{}", 101)]
    assert [dfx.may_be_accepted(e) for e in safe] == [False] * len(safe)
    assert [dfx.may_be_accepted(e) for e in risky] == [True] * len(risky)


def test_the_deliveries_control_files_and_the_local_guard(tmp_path):
    log = ["2026/11/25 10:00:00 delivery call",
           '2026/11/25 10:00:00 token request: scope="deliveries:request"',
           '2026/11/25 10:00:01 token request: scope="directory:read"',
           "2026/11/25 10:00:02 delivery call", "2026/11/25 10:00:02 deliveries refused: x"]
    local = dfx.DeliveriesControl(tmp_path, lambda: "\n".join(log), local=True)
    assert local.counts() == (2, 1)
    with pytest.raises(ValueError, match="local harness"):
        local.set({"ada": dfx.NO_SOURCE, "bob": dfx.delivery_answer([])})
    assert not (tmp_path / "deliveries.json").exists(), "a refused set wrote the file"
    local.set({"ada": dfx.NO_SOURCE, "bob": {"mode": "silent"}})
    p = tmp_path / "deliveries.json"
    assert stat.S_IMODE(p.stat().st_mode) == 0o600 and json.loads(p.read_text())["ada"] == "NO_SOURCE"
    local.clear()
    assert not p.exists()
    real = dfx.DeliveriesControl(tmp_path, lambda: "", local=False)
    real.set({"ada": dfx.delivery_answer([{"name": "k"}])})
    assert json.loads(p.read_text()) == {"ada": {"items": [{"name": "k"}], "problems": []}}
    real.deny(["srv_a", "srv_b"])
    assert (tmp_path / "no-deliveries").read_text() == "srv_a\nsrv_b\n"
    real.deny([])
    assert not (tmp_path / "no-deliveries").exists()
    real.outage(True)
    assert (tmp_path / "outage").exists()
    real.outage(False)
    assert not (tmp_path / "outage").exists()
    # The world's reference server holds the guard.
    ref = dfx.RefServer(tmp_path / "bin", tmp_path, tmp_path, tmp_path)
    assert ref.deliveries.local is True
    assert dfx.count_lines("a\nx delivery call y\ndelivery call\n", dfx.DELIVERY_CALL) == 2


def test_hosted_skip_reason_and_control(tmp_path):
    full = {"SHED_ITEST_SOURCE": "directory", "SHED_ITEST_DIRECTORY_CONTROL": str(tmp_path),
            "SHED_ITEST_DIRECTORY_LOG": str(tmp_path / "ref.err"), "SHED_IMAGE_HAS_DELIVERY": "fc, remote"}
    assert dfx.hosted_skip_reason("fc", full) is None
    assert dfx.hosted_skip_reason("remote", full) is None
    assert dfx.hosted_skip_reason(None, full) is None
    assert "SHED_IMAGE_HAS_DELIVERY does not list vz" in dfx.hosted_skip_reason("vz", full)
    assert "SHED_ITEST_SOURCE=directory" in dfx.hosted_skip_reason("fc", {**full, "SHED_ITEST_SOURCE": ""})
    assert "real backends only" in dfx.hosted_skip_reason(None, {**full, "SHED_DIRECTORY_ITEST": "1"})
    for var in ("SHED_ITEST_DIRECTORY_CONTROL", "SHED_ITEST_DIRECTORY_LOG"):
        assert var in dfx.hosted_skip_reason("fc", {**full, var: " "})
    ctl = dfx.hosted_control(full)
    assert ctl.local is False and ctl.control == tmp_path
    with pytest.raises(AssertionError, match="cannot be read"):
        ctl.calls()
    (tmp_path / "ref.err").write_text("2026/11/25 10:00:00 delivery call\n")
    assert ctl.calls() == 1
    with pytest.raises(AssertionError, match="not a directory"):
        dfx.hosted_control({**full, "SHED_ITEST_DIRECTORY_CONTROL": str(tmp_path / "none")})


def test_the_world_config_edits():
    cfg = dfx.server_config(http_port=1, https_port=2, ssh_port=3, state_dir=Path("/s"), issuer="http://127.0.0.1:9",
                            admin_key="ssh-ed25519 AAAA", agent_dir=Path("/a"))
    before = json.dumps(cfg, sort_keys=True)
    two = dfx.with_local_admin(cfg, "pat", "ssh-ed25519 BBBB")
    assert two["auth"]["users"]["pat"] == {"admin": True, "keys": [{"key": "ssh-ed25519 BBBB", "label": "itest"}]}
    script = dfx.with_local_admin(two, dfx.LOCAL_ADMIN, "ssh-ed25519 CCCC", label="s", script=True)
    assert script["auth"]["users"][dfx.LOCAL_ADMIN]["keys"][-1] == {"key": "ssh-ed25519 CCCC", "label": "s",
                                                                    "script": True}
    assert len(script["auth"]["users"][dfx.LOCAL_ADMIN]["keys"]) == 2
    assert "pat" not in dfx.without_local_admin(two, "pat")["auth"]["users"]
    with pytest.raises(KeyError):
        dfx.without_local_admin(cfg, "pat")
    svc = dfx.with_service_client(cfg, "itest", "itest-svc-")
    assert svc["auth"]["clients"]["itest"]["policy"]["name_prefix"] == "itest-svc-"
    assert json.dumps(cfg, sort_keys=True) == before, "an edit changed the config it was given"


def test_the_hosted_module_skips_everything_under_plain_test_integration():
    # Collected as `make test-integration` collects it: the local cases skip
    # (no SHED_DIRECTORY_ITEST) and the real-backend ones skip saying what to
    # set, before any fixture runs.
    env = {k: v for k, v in os.environ.items()
           if k not in ("SHED_DIRECTORY_ITEST", "SHED_BIN_DIR", "SHED_ITEST_SOURCE")}
    r = subprocess.run([sys.executable, "-m", "pytest", "-q", "-rs", "-p", "no:cacheprovider",
                        "test_delivery_hosted.py"], cwd=INTEGRATION_DIR, capture_output=True, text=True, timeout=120,
                       env=env)
    assert r.returncode == 0, r.stdout + r.stderr
    assert "passed" not in r.stdout and "failed" not in r.stdout and "error" not in r.stdout.lower(), r.stdout
    assert "run make test-integration-directory" in r.stdout and "SHED_ITEST_SOURCE=directory" in r.stdout, r.stdout


def test_problem_messages_are_taken_out_only_where_a_record_shows_them():
    s = "sntl"
    probs = [{"provider": "github", "code": "signed_out", "message": f"m {s}"}]
    rec = {"name": "x", "delivery": {"state": "delivered", "last": {"outcome": "accepted", "problems": probs}}}
    out = dlv.without_problem_messages(json.dumps(rec), probs)
    assert s not in out and '"code": "signed_out"' in out and '"state": "delivered"' in out
    # The same text anywhere else stays and is found.
    for elsewhere in ({**rec, "note": s}, {"delivery": {"last": {"problems": [], "failure": {"reason": s}}}},
                      {"delivery": {"problems": [{"message": s}]}}, [rec, {"message": s}]):
        assert s in dlv.without_problem_messages(json.dumps(elsewhere), probs), elsewhere
    text = ("Delivery:  delivered\nProblems the source reported:\n  github: signed_out: m sntl\n"
            "Owed:      none sntl\n")
    assert dlv.without_problem_messages(text, probs) == (
        "Delivery:  delivered\nProblems the source reported:\n  github: signed_out: \nOwed:      none sntl\n")
    # Only the expected rows' messages: another row in the block, a row
    # outside the block, and a changed row keep their text.
    for leak in ("Problems the source reported:\n  github: signed_out: m sntl\n  Debug: raw response: sntl\n",
                 "Problems the source reported:\n  github: signed_out: m sntl x\n",
                 "Problems the source reported:\n  gh: signed_out: m sntl\n",
                 "  github: signed_out: m sntl\n",
                 "Problems the source reported:\nother\n  github: signed_out: m sntl\n"):
        assert s in dlv.without_problem_messages(leak, probs), leak
    assert dlv.without_problem_messages('{"a": 1}\n{"b": "sntl"}\n', probs).count("sntl") == 1  # JSON lines: kept
    cli = (dc.REPO_ROOT / "cmd" / "shed" / "provision.go").read_text()
    assert f'fmt.Fprintln(out, "{dlv.PROBLEMS_HEADER}")' in cli
    assert 'fmt.Fprintf(out, "  %s: %s: %s\\n", safe(p.Provider), safe(p.Code), safe(p.Message))' in cli


def test_the_live_refuse_selection_needs_the_whole_fixture_language():
    sel = dlv.select_refuse_fixtures(dlv.load_refuse_fixtures())
    assert sel, "no live refuse fixture"
    forms = {k for f in sel for k in json.loads(f.path.read_text()) if k in ("answer_text", "answer_base64", "fill")}
    assert forms >= {"answer_text", "fill"}, forms


def test_render_answer_fixture_is_the_reference_sources(tmp_path):
    binary = dlv.ref_source_binary()
    if not binary.is_file():
        pytest.skip(f"{binary} is not built (make provision-ref)")
    refuse = dlv.REFUSE_DIR
    text = dlv.render_answer_fixture(refuse / "duplicate-member-escaped.json")
    assert text == json.loads((refuse / "duplicate-member-escaped.json").read_text())["answer_text"].encode()
    big = dlv.render_answer_fixture(refuse / "file-too-large.json")
    doc = json.loads(big)
    assert any(len(base64.b64decode(i.get("content", ""))) > 1 << 20 for i in doc["items"])
    assert dlv.render_answer_fixture(refuse / "not-json-empty.json") == b""
    with pytest.raises(AssertionError, match="rendering"):
        dlv.render_answer_fixture(tmp_path / "none.json")
    with pytest.raises(AssertionError, match="missing"):
        dlv.render_answer_fixture(refuse / "bad-provider.json", binary=tmp_path / "absent")


# ----------------------------------------------------------------------------
# Cloud sheds from the account service (plan 041 §6): fixtures/compute.py,
# the compute world of fixtures/directory.py, test_remote_compute.py and the
# Makefile's test-integration-compute. No server: the helpers are run on
# local files and local processes, and the words they match are checked
# against the Go sources that write them.
# ----------------------------------------------------------------------------

from fixtures import compute as cfx  # noqa: E402


def test_service_kind_reads_shed_remote_service():
    assert cfx.service_kind({"SHED_REMOTE_SERVICE": "ref"}) == ("ref", "")
    assert cfx.service_kind({"SHED_REMOTE_SERVICE": " service "}) == ("service", "")
    kind, why = cfx.service_kind({})
    assert kind is None and "SHED_REMOTE_SERVICE is not set" in why and "make dev-compute-ref-up" in why
    kind, why = cfx.service_kind({"SHED_REMOTE_SERVICE": "other"})
    assert kind is None and "expected one of ref, service" in why


@pytest.mark.parametrize("text, want", [
    ("2026-11-26T09:38:08Z", "2026-11-26T09:38:08+00:00"),
    ("2026-11-26T09:38:08.123456789Z", "2026-11-26T09:38:08.123456+00:00"),
    ("2026-11-26T10:38:08.5+01:00", "2026-11-26T09:38:08.500000+00:00"),
])
def test_parse_time_reads_go_rfc3339(text, want):
    assert cfx.parse_time(text).isoformat() == want


def test_parse_time_refuses_what_is_not_rfc3339():
    for bad in ("2026-11-26", "2026-11-26 09:38:08Z", "yesterday"):
        with pytest.raises(ValueError):
            cfx.parse_time(bad)


def test_lease_helpers_read_the_cloud_object():
    shed = {"name": "a", "cloud": {"lease": {"expires_at": "2026-11-26T10:00:00Z", "max_expires_at": "x"}}}
    assert cfx.expires_at(shed).isoformat() == "2026-11-26T10:00:00+00:00"
    with pytest.raises(AssertionError, match="no cloud object"):
        cfx.lease_of({"name": "a"})
    with pytest.raises(AssertionError, match="no lease yet"):
        cfx.lease_of({"name": "a", "cloud": {"size": {}}})


def test_the_banner_words_are_the_servers():
    src = (dc.REPO_ROOT / "internal" / "config" / "cloud.go").read_text()
    assert 'fmt.Sprintf("shed: this cloud shed is deleted at %s (in %s)"' in src
    assert cfx.BANNER == "shed: this cloud shed is deleted at "
    line = "shed: this cloud shed is deleted at 14:30 UTC (in 1h 20m); extend with: shed extend a --hours N\n"
    assert cfx.banner_count(line) == 1 and cfx.banner_count(line * 2) == 2 and cfx.banner_count("") == 0


def test_the_deletion_line_is_the_sweeps():
    src = (dc.REPO_ROOT / "internal" / "remote" / "sweep.go").read_text()
    assert 'the account service deleted its machine (cause %s); dropping its record"' in src
    assert cfx.DELETED_BY_SERVICE.format(cause="lease_ended") == (
        "the account service deleted its machine (cause lease_ended); dropping its record")


def test_the_refusal_words_are_the_clis():
    src = (dc.REPO_ROOT / "cmd" / "shed" / "cloud.go").read_text()
    assert 'fmt.Sprintf("%s (%s): %s", code, sdkbootstrap.SafeText(d.Reason), words)' in src
    assert cfx.refusal_text("COMPUTE_LIMIT_REACHED", "person") == "COMPUTE_LIMIT_REACHED (person):"


def test_policy_args_are_the_hooks_and_the_reference_services():
    assert cfx.policy_args({"org_max": 2, "person_max": 1}) == ["person_max=1", "org_max=2"]
    assert cfx.policy_args({}) == []
    for bad in ({"sizes": 1}, {"person_max": -1}, {"person_max": "1"}, {"person_max": True}):
        with pytest.raises(ValueError):
            cfx.policy_args(bad)
    # Every key a case sets is one the reference service's policy.set takes.
    src = (dc.REPO_ROOT / "internal" / "directory" / "refserver" / "compute_control.go").read_text()
    for k in cfx.POLICY_KEYS:
        assert f'`json:"{k}"`' in src, k
    start = cfx.world_policy()
    assert set(cfx.POLICY_KEYS) <= set(start)
    assert (start["person_max"], start["org_max"], start["warn_s"]) == (4, 10, 7200)


def test_with_steps_appends_and_keeps_every_other_member():
    doc = {"steps": [{"id": "old", "step": {"compute.allow": "s"}}], "auto_settle_s": 3, "replies": {}}
    out = cfx.with_steps(doc, [("quota.fill", ["org"]), ("clock.advance", 601)], "p")
    assert out["auto_settle_s"] == 3 and out["replies"] == {}
    assert out["steps"] == [{"id": "old", "step": {"compute.allow": "s"}},
                            {"id": "p-0", "step": {"quota.fill": ["org"]}},
                            {"id": "p-1", "step": {"clock.advance": 601}}]
    assert doc["steps"] == [{"id": "old", "step": {"compute.allow": "s"}}], "the input was changed"
    assert cfx.with_steps(None, [("x", 1)], "q") == {"steps": [{"id": "q-0", "step": {"x": 1}}]}
    with pytest.raises(ValueError):
        cfx.with_steps({"steps": {}}, [], "q")


def test_ref_control_writes_compute_json_privately(tmp_path):
    (tmp_path / "compute.json").write_text('{"steps": [], "auto_settle_s": 3}')
    ctl = cfx.RefControl(tmp_path, "remote-dev")
    ctl.steps(("compute.allow", "remote-dev"), wait=False)
    ctl.settings(auto_settle_s=1)
    doc = json.loads((tmp_path / "compute.json").read_text())
    assert doc["auto_settle_s"] == 1
    assert [list(s["step"]) for s in doc["steps"]] == [["compute.allow"]]
    assert stat.S_IMODE((tmp_path / "compute.json").stat().st_mode) == 0o600
    assert not list(tmp_path.glob(".*.tmp")), "a temporary file was left behind"
    # The service-down case: only through a gate (the hermetic world's),
    # never on the dev server's reference service (its state is in its
    # process).
    with pytest.raises(cfx.Unsupported, match="keeps its state"):
        ctl.check_stop()
    gate = _FakeGate()
    down = cfx.RefControl(tmp_path, "s", gate=gate)
    down.check_stop()
    assert not down.stopped
    down.stop()
    assert down.stopped, "a stop is recorded before it is attempted"
    down.start()
    assert gate.calls == ["down", "up"] and not down.stopped
    # A stop that fails half-way still leaves `stopped` set for clean-up.
    gate.down = lambda: (_ for _ in ()).throw(KeyboardInterrupt())
    with pytest.raises(KeyboardInterrupt):
        down.stop()
    assert down.stopped
    # The reference service's limits go back to the values read before.
    down.restore_limits(4, 10)
    doc = json.loads((tmp_path / "compute.json").read_text())
    assert doc["steps"][-1]["step"] == {"policy.set": {"person_max": 4, "org_max": 10}}


class _FakeGate:
    def __init__(self) -> None:
        self.calls: list[str] = []

    def down(self) -> None:
        self.calls.append("down")

    def up(self) -> None:
        self.calls.append("up")


def test_service_gate_refuses_while_down_and_relays_on_the_same_port_after():
    echo = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
    echo.bind(("127.0.0.1", 0))
    echo.listen(8)
    stop = threading.Event()

    def serve() -> None:
        echo.settimeout(0.2)
        while not stop.is_set():
            try:
                c, _ = echo.accept()
            except (socket.timeout, OSError):
                continue
            with c:
                c.sendall(c.recv(64).upper())

    t = threading.Thread(target=serve, daemon=True)
    t.start()
    port = dfx.free_ports(1)[0]
    gate = cfx.ServiceGate(port, echo.getsockname()[1]).up()

    def ask() -> bytes:
        with socket.create_connection(("127.0.0.1", port), timeout=5) as c:
            c.sendall(b"ping")
            return c.recv(64)

    try:
        assert gate.is_up and ask() == b"PING"
        gate.down()
        assert not gate.is_up
        with pytest.raises(ConnectionRefusedError):
            socket.create_connection(("127.0.0.1", port), timeout=5).close()
        gate.up()
        assert ask() == b"PING"
    finally:
        gate.close()
        stop.set()
        t.join(5)
        echo.close()


def test_ref_control_fails_a_step_the_service_logged_it_could_not_apply(tmp_path, monkeypatch):
    log = ["started\n"]
    ctl = cfx.RefControl(tmp_path, "s", lambda: "".join(log))
    monkeypatch.setattr(cfx.time, "sleep", lambda s: log.append("compute: compute.json: policy.set: bad\n"))
    with pytest.raises(AssertionError, match="could not apply"):
        ctl.policy(person_max=1)


def test_hook_control_runs_the_hook_with_its_verbs(tmp_path):
    out = tmp_path / "calls"
    hook = tmp_path / "hook"
    hook.write_text(f'#!/bin/sh\necho "$@" >> {out}\n[ "$1" != fail ] || exit 3\n')
    hook.chmod(0o755)
    h = cfx.HookControl.from_env({"SHED_COMPUTE_CONTROL": f"{hook}"})
    h.policy(person_max=1, org_max=2)
    h.policy_reset("person_max")
    h.check_stop()
    h.stop()
    assert h.stopped
    h.start()
    assert not h.stopped
    h.restore_limits(1, 2)
    assert out.read_text().splitlines() == ["policy person_max=1 org_max=2", "policy-reset", "stop", "start",
                                            "policy-reset"]
    lister = tmp_path / "lister"
    lister.write_text('#!/bin/sh\n[ "$1" = machines ] && printf "id1\\n\\nid2\\n"\n')
    lister.chmod(0o755)
    assert cfx.HookControl([str(lister)]).machines() == {"id1", "id2"}
    with pytest.raises(ValueError):
        h.run("fail")
    with pytest.raises(cfx.Unsupported):
        h.quota_fill_person("a")
    bad = cfx.HookControl([str(tmp_path / "missing")])
    with pytest.raises(OSError):
        bad.stop()
    failing = tmp_path / "failing"
    failing.write_text("#!/bin/sh\necho nope >&2\nexit 3\n")
    failing.chmod(0o755)
    with pytest.raises(AssertionError, match="exit 3"):
        cfx.HookControl([str(failing)]).start()
    f = cfx.HookControl([str(failing)])
    with pytest.raises(AssertionError, match="exit 3"):
        f.stop()
    assert f.stopped, "a failed stop is still started again at clean-up"
    assert cfx.HookControl.from_env({}) is None


def test_no_control_skips_with_what_to_set():
    n = cfx.NoControl()
    for call in (lambda: n.policy(person_max=1), n.policy_reset, n.check_stop, n.stop, n.start,
                 lambda: n.clock_advance(1), lambda: n.quota_fill_org()):
        with pytest.raises(cfx.Unsupported):
            call()
    with pytest.raises(cfx.Unsupported, match="SHED_COMPUTE_CONTROL"):
        n.stop()


def test_client_policy_reads_the_servers_config(tmp_path):
    cfg = {"auth": {"clients": {"nocturn": {"policy": {"name_prefix": "nocturn-", "max_age": "30h",
                                                       "images": ["cloud"], "network": "isolated",
                                                       "max_sheds": 4}}}}}
    assert cfx.client_policy(cfg, "nocturn") == {"name_prefix": "nocturn-", "max_age_s": 30 * 3600,
                                                "images": ["cloud"], "network": "isolated", "max_sheds": 4}
    with pytest.raises(KeyError, match="no service client"):
        cfx.client_policy(cfg, "other")
    # The dev template's client is the default SHED_COMPUTE_CLIENT.
    rendered = yaml.safe_load(_rendered_remote_template(tmp_path).read_text())
    assert rendered["remote"]["provider"] == "service"
    assert cfx.client_policy(rendered, "nocturn")["name_prefix"] == "nocturn-"


@pytest.mark.parametrize("text, want", [("30h", 108000), ("1h30m", 5400), ("90s", 90), ("0s", 0), ("2h", 7200)])
def test_duration_seconds(text, want):
    assert cfx.duration_seconds(text) == want


def test_duration_seconds_refuses_other_units():
    for bad in ("1d", "1.5h", "h", "1h 2m"):
        with pytest.raises(ValueError):
            cfx.duration_seconds(bad)


def test_binary_script_writes_every_byte_once():
    r = subprocess.run(["bash", "-c", cfx.binary_script()], capture_output=True, timeout=30)
    assert r.returncode == 0 and r.stdout == bytes(range(256)), r.stdout[:40]


@pytest.mark.parametrize("on_tty, want_tty, want_stdout, want_stderr", [
    (("stdin", "stdout", "stderr"), b"in\r\nout\r\nerr\r\nplain\r\n", b"", b""),
    (("stderr",), b"err\r\n", b"plain\n", b""),
    (("stdin", "stdout"), b"in\r\nout\r\nplain\r\n", b"", b"e\n"),
    ((), b"", b"plain\n", b"e\n"),
])
def test_run_with_tty_puts_exactly_the_named_streams_on_the_terminal(on_tty, want_tty, want_stdout, want_stderr):
    script = ("test -t 0 && echo in; test -t 1 && echo out; test -t 2 && echo err >&2; "
              "test -t 2 || echo e >&2; echo plain")
    r = cfx.run_with_tty(["sh", "-c", script], dict(os.environ), on_tty=on_tty, timeout=30)
    assert r.code == 0, r
    assert (r.tty, r.stdout, r.stderr) == (want_tty, want_stdout, want_stderr), r


def test_run_with_tty_types_its_input_and_kills_at_its_timeout():
    r = cfx.run_with_tty(["bash", "--norc", "--noprofile", "-i"], {**os.environ, "TERM": "dumb"},
                         input=b"echo got-$((40+2))\nexit\n", timeout=30)
    assert r.code == 0 and b"got-42" in r.tty, r
    t0 = time.monotonic()
    r = cfx.run_with_tty(["sleep", "60"], dict(os.environ), timeout=1)
    assert r.code is None and time.monotonic() - t0 < 15, r
    with pytest.raises(ValueError):
        cfx.run_with_tty(["true"], dict(os.environ), on_tty=("stderr",), input=b"x")
    with pytest.raises(ValueError):
        cfx.run_with_tty(["true"], dict(os.environ), on_tty=("tty",))


def test_the_compute_module_runs_only_under_its_make_target():
    full = {"SHED_COMPUTE_ITEST": "1", "SHED_BIN_DIR": "/repo/bin"}
    assert dfx.compute_skip_reason(full, "linux") is None
    assert "run make test-integration-compute" in dfx.compute_skip_reason({}, "linux")
    assert "Linux only" in dfx.compute_skip_reason(full, "darwin")
    assert "SHED_BIN_DIR" in dfx.compute_skip_reason({"SHED_COMPUTE_ITEST": "1"}, "linux")


def test_the_compute_cases_skip_under_plain_test_integration():
    env = {k: v for k, v in os.environ.items()
           if k not in ("SHED_COMPUTE_ITEST", "SHED_BIN_DIR", "SHED_REMOTE_SERVER", "SHED_REMOTE_SERVICE")}
    r = subprocess.run([sys.executable, "-m", "pytest", "-q", "-rs", "-p", "no:cacheprovider",
                        "test_remote_compute.py"], cwd=INTEGRATION_DIR, capture_output=True, text=True,
                       timeout=120, env=env)
    assert r.returncode == 0, r.stdout + r.stderr
    assert "passed" not in r.stdout and "failed" not in r.stdout, r.stdout
    assert "run make test-integration-compute" in r.stdout and "SHED_REMOTE_SERVER not set" in r.stdout, r.stdout


def test_the_compute_server_config_takes_its_machines_from_the_service(tmp_path):
    cfg = dfx.server_config(http_port=1, https_port=2, ssh_port=3, state_dir=tmp_path / "state",
                            issuer="http://127.0.0.1:9", admin_key="ssh-ed25519 AAAA", agent_dir=tmp_path / "agent",
                            compute=True)
    remote = cfg["remote"]
    assert (remote["provider"], remote["transport"], remote["allow_test_drivers"]) == ("service", "local", True)
    assert "limits" not in remote, "the service holds the quotas"
    client = cfg["auth"]["clients"][dfx.COMPUTE_CLIENT]
    assert client["policy"]["name_prefix"] == dfx.COMPUTE_CLIENT_PREFIX
    # The directory world itself is unchanged.
    plain = dfx.server_config(http_port=1, https_port=2, ssh_port=3, state_dir=tmp_path / "state",
                              issuer="http://127.0.0.1:9", admin_key="ssh-ed25519 AAAA", agent_dir=tmp_path / "agent")
    assert plain["remote"]["provider"] == "fake" and "clients" not in plain["auth"]


def test_the_compute_server_config_loads(tmp_path):
    binary = dc.REPO_ROOT / "bin" / "shed-server"
    if not os.access(binary, os.X_OK):
        pytest.skip(f"{binary} is not built")
    (tmp_path / "agent").mkdir()
    cfg = dfx.server_config(http_port=40001, https_port=40002, ssh_port=40003, state_dir=tmp_path / "state",
                            issuer="http://127.0.0.1:40009", admin_key=ED_A, agent_dir=tmp_path / "agent",
                            compute=True)
    path = tmp_path / "server.yaml"
    path.write_text(yaml.safe_dump(cfg, sort_keys=False))
    r = subprocess.run([str(binary), "--config", str(path), "config-validate"], capture_output=True, text=True,
                       timeout=60, env={**os.environ, "HOME": str(tmp_path)})
    assert r.returncode == 0, r.stdout + r.stderr


def test_the_compute_target_builds_then_runs_the_module_against_no_other_server():
    deps, recipe = _make_recipe("test-integration-compute")
    assert deps.split() == ["check-directory-itest-host", "build", "directory-ref"]
    assert "uv run pytest -v -rs $(COMPUTE_ITEST_FILES) $(PYTEST_ARGS)" in recipe
    assert 'PATH="$(CURDIR)/bin:$$PATH"' in recipe
    assert "SHED_COMPUTE_ITEST=1" in recipe and 'SHED_BIN_DIR="$(CURDIR)/bin"' in recipe
    # The shared [remote] cases against the session's hermetic world.
    assert "SHED_REMOTE_HERMETIC=1" in recipe
    text = (dc.REPO_ROOT / "Makefile").read_text()
    files = re.search(r"^COMPUTE_ITEST_FILES \?= ((?:[^\n]*\\\n)*[^\n]*)$", text, re.MULTILINE)
    assert files, "no COMPUTE_ITEST_FILES"
    names = files.group(1).replace("\\\n", " ").split()
    assert names[0] == "test_remote_compute.py" and {"test_remote_lifecycle.py", "test_smoke.py"} <= set(names)
    for n in names:
        assert (INTEGRATION_DIR / n).is_file(), n
    conftest_src = (INTEGRATION_DIR / "conftest.py").read_text()
    names = set(re.findall(r'os\.environ\.get\(\s*"(SHED_(?:VZ|FC)_[A-Z_]+|SHED_REMOTE_SERVER)"', conftest_src))
    for name in names | {"SHED_REMOTE_SERVICE", "SHED_ITEST_SOURCE"}:
        assert re.search(rf"^\s*{name}= \\$", recipe, re.MULTILINE), f"{name} is not set empty"
    phony = (dc.REPO_ROOT / "Makefile").read_text().splitlines()[0]
    assert " test-integration-compute " in phony


def test_the_local_links_stand_in_key_is_a_public_key_line():
    # A local-link server names this key in every create; a service holding
    # to the contract refuses anything that is not one public key line.
    src = (dc.REPO_ROOT / "internal" / "remote" / "backend.go").read_text()
    m = re.search(r'localAuthorizedKey = "([^"]+)"', src)
    assert m, "no localAuthorizedKey in internal/remote/backend.go"
    assert re.match(r"^[a-z0-9@.-]{1,64} [A-Za-z0-9+/]+={0,2}( [\x20-\x7e]*)?$", m.group(1)), m.group(1)
    assert m.group(1).startswith("ssh-ed25519 ")


# The shared [remote] cases on a compute server act as the service's person
# (conftest: remote_person, through CloudServer.as_person); the hermetic
# world skips the cases that need a real guest (`real_guest`).


def test_a_local_server_runs_every_command_as_its_identity(monkeypatch, tmp_path):
    seen = []

    def fake_run(argv, **kw):
        seen.append((argv, kw.get("env")))
        return subprocess.CompletedProcess(argv, 0, "", "")

    monkeypatch.setattr(fx.subprocess, "run", fake_run)
    monkeypatch.setattr(fx, "resolve_server_entry", lambda name, timeout=10, env=None: {"host": "h", "ssh_port": 7})
    env = {"HOME": str(tmp_path), "PATH": "/x"}
    s = fx.CloudServer(name="e", env=env, home=tmp_path)
    s.create("n")
    s.exec("n", ["true"])
    s.stop("n")
    s.start("n")
    s.cli("list")
    s.ssh_exec("n", "true")
    s.delete("n", ignore_missing=True)
    assert seen and all(e is env for _, e in seen), seen
    (tmp_path / ".shed").mkdir()
    (tmp_path / ".shed" / "known_hosts").write_text("")
    argv = s.ssh_argv("n", "true")
    assert f"UserKnownHostsFile={tmp_path / '.shed' / 'known_hosts'}" in argv
    # The developer's own view runs with this process's environment.
    seen.clear()
    fx.CloudServer(name="e").cli("list")
    assert seen[0][1] is None


def test_as_person_takes_the_identitys_environment_and_home(tmp_path):
    person = ident.Identity(user="itest-alice", key=tmp_path / "k", home=tmp_path / "h", entry="remote-dev",
                            host="localhost", ssh_port=12290, shim=tmp_path / "h" / "bin" / "ssh")
    admin = fx.CloudServer(name="remote-dev", log_path=tmp_path / "log", image="cloud")
    p = admin.as_person(person)
    assert (p.name, p.home, p.image, p.log_path) == ("remote-dev", tmp_path / "h", "cloud", tmp_path / "log")
    assert p.env["HOME"] == str(tmp_path / "h") and p.env["PATH"].startswith(str(tmp_path / "h" / "bin"))


def test_person_identity_has_a_home_apart_from_users_environments(tmp_path):
    keys = tmp_path / "keys"
    a = cfx.person_identity("itest-alice", "remote-dev", "localhost", 12290, keys_dir=keys)
    assert a.home == keys / "itest-alice" / "home-remote-dev-shared"
    assert a.home != keys / "itest-alice" / "home-remote-dev"  # users_environment's
    assert a.shim.is_file() and a.key == keys / "itest-alice" / "id_ed25519"
    (a.home / "leftover").write_text("x")
    again = cfx.person_identity("itest-alice", "remote-dev", "localhost", 12290, keys_dir=keys)
    assert not (again.home / "leftover").exists(), "a leftover HOME is made fresh"


def test_list_itest_people_keeps_everyone_else(tmp_path):
    users = tmp_path / "users.json"
    users.write_text(json.dumps([{"name": "ada", "admin": False, "keys": []},
                                 {"name": "itest-alice", "admin": False, "keys": []}]))
    cfx.list_itest_people(users, keys_dir=tmp_path / "keys")
    got = json.loads(users.read_text())
    names = [u["name"] for u in got]
    assert names == sorted(names) and "ada" in names and {"itest-alice", "itest-bob", "itest-admin"} <= set(names)
    alice = next(u for u in got if u["name"] == "itest-alice")
    assert alice["keys"], "itest-alice's entry is the one with her key"
    assert stat.S_IMODE(users.stat().st_mode) == 0o600


def test_unlisted_people_reads_the_users_status():
    class Server:
        def __init__(self, users, code=0):
            self.users, self.code, self.calls = users, code, []

        def cli(self, *args, timeout=60):
            self.calls.append(args)
            out = json.dumps({"users": [{"name": n} for n in self.users]})
            return subprocess.CompletedProcess(args, self.code, out, "")

    s = Server(["itest-alice"])
    assert cfx.unlisted_people(s) == ["itest-bob"]
    assert s.calls[0] == ("users", "refresh")
    assert cfx.unlisted_people(Server(["itest-alice", "itest-bob"])) == []
    assert cfx.unlisted_people(Server([], code=1), ["x"]) == ["x"]


def test_real_guest_cases_skip_only_on_the_hermetic_remote(monkeypatch):
    import conftest

    class Item:
        def __init__(self, params):
            self.callspec = None if params is None else type("C", (), {"params": params})()

        def get_closest_marker(self, name):
            return pytest.mark.real_guest("why").mark if name == "real_guest" else None

    monkeypatch.setattr(conftest, "REMOTE_HERMETIC", True)
    for params in ({"shed_server": "remote"}, {"users_env": "remote"}, None):
        with pytest.raises(pytest.skip.Exception, match="needs a real guest.*: why"):
            conftest.pytest_runtest_setup(Item(params))
    conftest.pytest_runtest_setup(Item({"shed_server": "fc"}))
    monkeypatch.setattr(conftest, "REMOTE_HERMETIC", False)
    conftest.pytest_runtest_setup(Item({"shed_server": "remote"}))


# test_remote_compute.py's clean-up never trusts `shed list` alone.


class _FakeAdmin:
    def __init__(self, listed, delete_code=0):
        self.listed, self.delete_code = set(listed), delete_code
        self.deleted = []

    def list_sheds(self):
        return [{"name": n} for n in sorted(self.listed)]

    def cli(self, *args, timeout=60):
        self.deleted.append(args[1])
        return subprocess.CompletedProcess(args, self.delete_code, "", "boom")


def test_delete_confirmed_needs_a_successful_delete_of_a_listed_shed():
    import test_remote_compute as trc

    ok = _FakeAdmin(["a"])
    trc.delete_confirmed(ok, "a", "id-a")
    assert ok.deleted == ["a"]
    with pytest.raises(AssertionError, match=r"CLEAN-UP FAILED: deleting shed a \(id id-a\)"):
        trc.delete_confirmed(_FakeAdmin(["a"], delete_code=1), "a", "id-a")
    gone = _FakeAdmin([])
    trc.delete_confirmed(gone, "a", "id-a")
    assert gone.deleted == [], "a shed not listed is left to the inventory check"


def test_confirm_gone_asks_the_service_for_each_shed_id(monkeypatch):
    import test_remote_compute as trc

    monkeypatch.setattr(trc.time, "sleep", lambda s: None)

    class Control:
        def __init__(self, *answers):
            self.answers = list(answers)

        def machines(self):
            return self.answers.pop(0) if len(self.answers) > 1 else self.answers[0]

    ids = {"itest-cc-a": "ida", "itest-cc-b": "idb", "never": "?"}
    trc.confirm_gone(Control({"other-server-machine"}), ids, timeout=0)
    trc.confirm_gone(Control({"ida"}, {"ida"}, set()), ids, timeout=60)
    with pytest.raises(AssertionError, match=r"still holds itest-cc-b \(id idb\)"):
        trc.confirm_gone(Control({"idb", "unrelated"}), ids, timeout=0)
    with pytest.raises(cfx.Unsupported):
        trc.confirm_gone(cfx.NoControl(), ids, timeout=0)


def test_ref_control_reads_the_services_inventory(tmp_path):
    inv = tmp_path / "inventory.json"
    ctl = cfx.RefControl(tmp_path, "remote-dev", inventory=inv)
    with pytest.raises(cfx.Unsupported, match="--inventory"):
        ctl.machines()
    inv.write_text(json.dumps({"servers": {"remote-dev": ["a", "b"], "other": ["c"]}}))
    assert ctl.machines() == {"a", "b"}
    inv.write_text(json.dumps({"servers": {}}))
    assert ctl.machines() == set()
    # The dev script starts the service with the file.
    script = (dc.REPO_ROOT / "scripts" / "dev-compute-ref.sh").read_text()
    assert '--inventory "$DIR/inventory.json"' in script


# A shared shed's name is recorded only once its create succeeded: a refused
# create must not leave later cases polling a shed that does not exist.


def _shared_env():
    import test_remote_compute as trc

    return trc.Env(where="hermetic", service="ref", alice=None, bob=None, admin=None, client=None,
                   client_name="c", client_policy={"name_prefix": "c-", "max_age_s": 0, "images": [],
                                                   "network": ""},
                   control=None, server_log=lambda: "", restart=None, image=None,
                   cleanup=lambda name: None, shared=trc.Shared())


def test_client_shed_records_its_name_only_after_the_create_succeeds(monkeypatch):
    import test_remote_compute as trc

    answers = [(409, {"error": {"code": "CONFLICT"}}), (201, {"id": "id-1"})]
    monkeypatch.setattr(trc, "client_create", lambda env, name, lease=None: answers.pop(0))
    waited = []
    monkeypatch.setattr(trc, "client_wait_running",
                        lambda env, name: waited.append(name) or {"name": name, "status": "running"})
    env = _shared_env()
    with pytest.raises(AssertionError, match="the client's create of c-cc-shared-.*: 409"):
        env.client_shed()
    assert env.shared.client is None and waited == [], "a refused create shares nothing and waits on nothing"
    assert len(env.made) == 1 and env.shared.created == 0, "the failing case's clean-up owns the name"
    env.made.clear()
    shed = env.client_shed()
    assert env.shared.client == shed["name"] == waited[0] and env.made == []
    assert env.shared.created == 1 and env.shared.ids == {shed["name"]: "id-1"}
    # A later case reuses it without another create.
    assert env.client_shed()["name"] == shed["name"] and answers == []


def test_person_shed_records_its_name_only_after_the_create_succeeds(monkeypatch):
    import test_remote_compute as trc

    codes = [1, 0]
    monkeypatch.setattr(trc, "create",
                        lambda env, who, name: subprocess.CompletedProcess([], codes.pop(0), "", "refused"))
    waited = []
    monkeypatch.setattr(trc, "wait_status",
                        lambda who, name, status: waited.append(name) or {"name": name, "id": "id-p"})

    class Alice:
        user = "alice"

    env = _shared_env()
    env.alice = Alice()
    with pytest.raises(AssertionError, match="alice: shed create itest-cc-shared-.*: exit 1"):
        env.person_shed()
    assert env.shared.person is None and waited == [] and len(env.made) == 1
    env.made.clear()
    shed = env.person_shed()
    assert env.shared.person == shed["name"] and env.made == []
    assert env.shared.created == 1 and env.shared.ids == {shed["name"]: "id-p"}
