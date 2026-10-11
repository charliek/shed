"""Server-free unit tests for `fixtures/server.py` helpers.

No shed-server, no `shed` CLI, no network: subprocess and urllib are faked.
Runs in CI via `make test-integration-units`, next to `test_smoke_units.py`.
"""

from __future__ import annotations

import io
import json
import subprocess
import urllib.error

import pytest

from fixtures import server as fx


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


def _fake_entry(monkeypatch, endpoint):
    entry = None if endpoint is None else {"name": "srv", "endpoint": endpoint}
    monkeypatch.setattr(fx, "resolve_server_entry", lambda name, timeout=10: entry)


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
