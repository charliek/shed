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
import io
import json
import os
import re
import shutil
import socket
import stat
import subprocess
import time
import urllib.error
from pathlib import Path

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


def _fake_dev_control(monkeypatch, tmp_path) -> dict:
    """Stub every side effect of dev_config(): no server, no make."""
    calls = {"restarts": [], "waits": [], "targets": []}
    monkeypatch.setattr(dc, "DEV_CONFIG_TMP", tmp_path / "test-dev-config.yaml")
    monkeypatch.setattr(dc, "DEV_SCRATCH_DIR", tmp_path)
    monkeypatch.setattr(dc, "assert_dev_target", lambda name: calls["targets"].append(name) or {})
    monkeypatch.setattr(dc, "_make_restart", lambda cfg: calls["restarts"].append(cfg))
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
    got = dc.bootstrap_cert("my-server-dev", "credentials", tmp_path / "c", "host-agent")

    assert len(sent) == 1
    argv = sent[0]
    assert argv[argv.index("-p") + 1] == "12222"
    assert "StrictHostKeyChecking=yes" in argv and f"UserKnownHostsFile={dc.KNOWN_HOSTS_PATH}" in argv
    # The request line: scope, kind, exactly one csr= in standard base64 that
    # decodes to a DER CSR openssl verifies.
    target = argv.index("_bootstrap@localhost")
    request = argv[target + 1:]
    assert request[:2] == ["credentials", "host-agent"]
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
        _openssl("req", "-new", "-x509", "-key", str(d / f"{name}.key"), "-subj", f"/CN={name}",
                 "-days", "1", "-out", str(d / f"{name}.pem"))
    _openssl("req", "-new", "-key", str(d / "client.key"), "-subj", "/CN=client", "-out", str(d / "client.csr"))
    _openssl("x509", "-req", "-in", str(d / "client.csr"), "-CA", str(d / "ca.pem"), "-CAkey", str(d / "ca.key"),
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


@pytest.mark.parametrize(
    "env_file, want",
    [
        (None, "no env file"),
        ("SHED_GCE_PROJECT=x\n", "does not set SHED_REMOTE_ENV"),
        ("SHED_REMOTE_ENV=dev\nSHED_REMOTE_FIREWALL_MODE=auto\n", "needs SHED_REMOTE_ECHO_URLS"),
    ],
    ids=["missing-env-file", "no-environment-name", "auto-without-echo-urls"],
)
def test_remote_restart_refuses_bad_settings_before_stopping(tmp_path, env_file, want):
    pid_file, proc = _remote_home(tmp_path)
    try:
        path = tmp_path / "remote.env"
        if env_file is not None:
            path.write_text(env_file)
        r, calls = _make_with_stubs(
            tmp_path, "dev-server-restart-remote", f"SHED_REMOTE_ENV_FILE={path}", "SHED_REMOTE_GITHUB_USER=someone",
        )
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
    env.write_text("SHED_REMOTE_ENV=dev\n")
    r, _ = _make_with_stubs(
        tmp_path, "dev-server-restart-remote", f"SHED_REMOTE_ENV_FILE={env}", "SHED_REMOTE_GITHUB_USER=someone",
        dry_run=True,
    )
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
    env.write_text("SHED_REMOTE_ENV=dev\n")
    r, _ = _make_with_stubs(
        tmp_path, "dev-server-up-remote", f"SHED_REMOTE_ENV_FILE={env}", "SHED_REMOTE_GITHUB_USER=someone",
        dry_run=True,
    )
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
        "@DEV_REMOTE_DIR@": str(tmp_path / "dev-remote"), "@SHED_REMOTE_ENV@": "itest",
        "@GITHUB_USER@": "someone", "@FIREWALL_MODE@": "static",
        "@FIREWALL_SETTING@": "source_ranges_env: SHED_REMOTE_SOURCE_RANGES",
    }.items():
        text = text.replace(k, v)
    out = tmp_path / "remote.yaml"
    out.write_text(text, encoding="utf-8")
    return out


@pytest.mark.parametrize("which", ["mac", "fc", "remote"])
def test_render_users_writes_a_config_the_server_loads(tmp_path, keys_dir, which):
    if shutil.which("uv") is None:
        pytest.skip("uv is not installed")
    base = {"mac": MAC_DEV_CONFIG, "fc": FC_DEV_CONFIG}.get(which) or _rendered_remote_template(tmp_path)
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
    goes; any other namespace is refused with a 403 JSON error."""
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
        _openssl("req", "-new", "-x509", "-key", str(d / f"{name}.key"), "-subj", f"/CN={name}",
                 "-days", "1", "-out", str(d / f"{name}.pem"))
    _openssl("req", "-new", "-key", str(d / "client.key"), "-subj", "/CN=client", "-out", str(d / "client.csr"))
    _openssl("x509", "-req", "-in", str(d / "client.csr"), "-CA", str(d / "ca.pem"), "-CAkey", str(d / "ca.key"),
             "-CAcreateserial", "-days", "1", "-out", str(d / "client.pem"))
    stop = threading.Event()

    class Handler(http.server.BaseHTTPRequestHandler):
        protocol_version = "HTTP/1.1"

        def do_GET(self):
            if self.path != "/api/plugins/listeners/ok/messages":
                body = b'{"error":{"code":"FORBIDDEN","message":"no"}}'
                self.send_response(403)
                self.send_header("Content-Type", "application/json")
                self.send_header("Content-Length", str(len(body)))
                self.end_headers()
                self.wfile.write(body)
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
    yield {"port": srv.server_address[1], "pin": pin, "pair": (str(d / "client.pem"), str(d / "client.key"))}
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
    env.write_text("SHED_REMOTE_ENV=dev\n")
    r, calls = _make_with_stubs(tmp_path, target, "SHED_DEV_USERS=alice", f"SHED_REMOTE_ENV_FILE={env}",
                                "SHED_REMOTE_GITHUB_USER=someone")
    assert r.returncode != 0
    assert "SHED_DEV_USERS is 'alice'" in r.stdout + r.stderr
    assert not [c for c in calls if "kill" in c or "serve --config" in c or c.startswith(("scp ", "uname -s"))], calls


def test_dev_users_renders_the_user_list_on_each_dev_target(tmp_path):
    env = tmp_path / "remote.env"
    env.write_text("SHED_REMOTE_ENV=dev\n")
    common = ("SHED_DEV_USERS=itest", "SHED_ITEST_USERS_DIR=/keys/here", f"SHED_REMOTE_ENV_FILE={env}",
              "SHED_REMOTE_GITHUB_USER=someone")
    for target in ("dev-server-up", "dev-server-up-fc", "dev-server-up-remote"):
        r, _ = _make_with_stubs(tmp_path, target, *common, dry_run=True)
        assert r.returncode == 0, (target, r.stdout + r.stderr)
        out = r.stdout
        m = re.search(r'render-dev-config\.sh [^\n]*--users "?/keys/here', out)
        assert m, (target, out)
        assert m.start() < out.index("serve --config"), target
    # Unset, the render is skipped: its guard reads empty (the FC and remote
    # recipes), or the Mac's render arguments carry no --users.
    for target in ("dev-server-up-fc", "dev-server-up-remote"):
        r, _ = _make_with_stubs(tmp_path, target, f"SHED_REMOTE_ENV_FILE={env}", "SHED_REMOTE_GITHUB_USER=someone",
                                dry_run=True)
        assert r.returncode == 0, r.stdout + r.stderr
        assert 'if [ "" = itest ]; then' in r.stdout and 'if [ "itest" = itest ]' not in r.stdout, (target, r.stdout)
    r, _ = _make_with_stubs(tmp_path, "dev-server-up", dry_run=True)
    assert r.returncode == 0, r.stdout + r.stderr
    assert "--users" not in r.stdout, r.stdout


def test_the_test_targets_tell_the_suite_about_the_user_list():
    lines = _makefile_logical_lines()
    for target, backend in (("test-integration-dev", None), ("test-integration-dev-fc", "fc"),
                            ("test-integration-remote", "remote")):
        start = next(i for i, l in enumerate(lines) if l.startswith(f"{target}:"))
        recipe = []
        for l in lines[start + 1:]:
            if not l.startswith("\t"):
                break
            recipe.append(l)
        body = "\n".join(recipe)
        if backend is None:
            # The [vz] cases restart the Mac dev server with the list
            # themselves: nothing to tell them.
            assert "SHED_ITEST_USERS=" not in body, (target, body)
        else:
            assert f"SHED_ITEST_USERS=$(if $(filter itest,$(SHED_DEV_USERS)),{backend},)" in body, (target, body)
        assert "SHED_ITEST_USERS_DIR=$(SHED_ITEST_USERS_DIR)" in body, target
