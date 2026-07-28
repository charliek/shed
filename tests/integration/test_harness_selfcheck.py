"""Self-checks for the Phase 0.5 dev-control primitives (`fixtures/devcontrol.py`).

These validate the *mechanism* every later auth/transport phase relies on.
Two kinds, deliberately separated:

  - **Hermetic** guard tests (`assert_dev_target`) — monkeypatched, no
    server, run everywhere. This is the suite's actual write-side safety
    rail, so it gets always-on coverage like `test_framework_meta.py`.
  - **Live** mechanism tests (`dev_config`, `run_preflight`) — these do a
    real VZ dev-server restart, so they gate on the `vz_server_dev`
    fixture (skips cleanly when `SHED_VZ_DEV_SERVER` is unset, exactly
    like every other live dev test) and are marked `vz` + `slow`.
"""

from __future__ import annotations

import json
import ssl
import urllib.request

import pytest

import fixtures.devcontrol as devcontrol


def _api_info(config: dict) -> dict:
    """Fetch `/api/info`, choosing scheme/port the same way
    `devcontrol._wait_reachable` does: an enforced-mode CONFIG (today's
    committed dev-parallel base is `auth.mode: token`) is TLS-only, so read
    `https_port` off the CONFIG the server was actually started with — never
    the CLIENT ENTRY, which carries no `http_port` at all once the server
    enforces token/mtls (see `fixtures/devcontrol.py`'s module docstring)."""
    ctx = None
    if devcontrol._config_enforced(config):
        port = int(config.get("https_port") or 8443)
        scheme = "https"
        ctx = ssl._create_unverified_context()
    else:
        port = int(config.get("http_port") or 8080)
        scheme = "http"
    with urllib.request.urlopen(
        f"{scheme}://localhost:{port}/api/info", timeout=10, context=ctx
    ) as resp:
        return json.loads(resp.read().decode("utf-8"))


# --- Hermetic guard tests (no live server) ---------------------------------


def test_assert_dev_target_rejects_non_dev_name():
    """A name without the '-dev' suffix is refused before any resolve."""
    with pytest.raises(RuntimeError, match="non-dev"):
        devcontrol.assert_dev_target("my-server")


def test_assert_dev_target_rejects_dev_suffixed_production(monkeypatch):
    """A '-dev'-suffixed entry on production ports is caught by the port
    gate — the `localmac-dev`-on-:8080 footgun the suffix alone misses."""
    monkeypatch.setattr(
        devcontrol,
        "resolve_server_entry",
        lambda name, timeout=10: {"name": name, "http_port": 8080, "ssh_port": 2222},
    )
    with pytest.raises(RuntimeError, match="production ports"):
        devcontrol.assert_dev_target("sneaky-dev")


def test_assert_dev_target_accepts_offset_port_dev(monkeypatch):
    """A '-dev' entry on offset ports is accepted and returned."""
    entry = {"name": "x-dev", "http_port": 18080, "ssh_port": 12222}
    monkeypatch.setattr(
        devcontrol, "resolve_server_entry", lambda name, timeout=10: entry
    )
    assert devcontrol.assert_dev_target("x-dev") == entry


def test_assert_dev_target_accepts_https_only_dev_entry(monkeypatch):
    """A '-dev' entry with NO `http_port` at all — the real shape an SSH-first
    `shed server add` now records for an enforced-mode (token/mtls) server,
    per `shed --json server list` (see `fixtures/devcontrol.py`'s module
    docstring and `_ports`'s https_port/endpoint fallback) — is still
    accepted via the `https_port` fallback, not misdiagnosed as malformed."""
    entry = {
        "name": "x-dev",
        "host": "localhost",
        "endpoint": "https://localhost:18443",
        "ssh_port": 12222,
        "https_port": 18443,
        "security": "secure",
    }
    monkeypatch.setattr(
        devcontrol, "resolve_server_entry", lambda name, timeout=10: entry
    )
    assert devcontrol.assert_dev_target("x-dev") == entry


def test_assert_dev_target_rejects_malformed_ports(monkeypatch):
    """A '-dev' entry with missing/zero ports fails closed, not defaulted."""
    monkeypatch.setattr(
        devcontrol,
        "resolve_server_entry",
        lambda name, timeout=10: {"name": name, "http_port": 0, "ssh_port": 0},
    )
    with pytest.raises(RuntimeError, match="malformed"):
        devcontrol.assert_dev_target("broken-dev")


def test_config_ports_safe_rejects_prod_and_nonpositive():
    """The config-ports gate refuses prod ports and non-positive ports,
    so a stray override can't make a server bind production."""
    with pytest.raises(RuntimeError, match="production ports"):
        devcontrol._assert_config_ports_safe({"http_port": 8080, "ssh_port": 2222})
    with pytest.raises(RuntimeError, match="non-positive"):
        devcontrol._assert_config_ports_safe({"http_port": 0, "ssh_port": 12222})
    # Offset dev ports pass.
    devcontrol._assert_config_ports_safe({"http_port": 18080, "ssh_port": 12222})


# --- Live mechanism tests (real dev-server restart) ------------------------


@devcontrol.skip_needs_open_mode_dev_server
@devcontrol.skip_mtls_reconfigure
@pytest.mark.vz
@pytest.mark.slow
def test_dev_config_roundtrips_override(vz_server_dev):
    """`dev_config` applies an override through a real restart and restores.

    Overrides the server's reported `name` (a cosmetic field surfaced by
    /api/info, with no effect on the CLI entry which resolves by
    host:port) so the change is observable and harmless.
    """
    server = vz_server_dev.name
    # The override below only changes `name`; ports/mode are unchanged from
    # the committed base, so the same resolved config is valid for all three
    # fetches (before, during, and after the override).
    base_config = devcontrol._merge_config({})

    base_name = _api_info(base_config)["name"]
    sentinel = "harness-selfcheck-name"
    assert base_name != sentinel

    with devcontrol.dev_config({"name": sentinel}, server):
        assert _api_info(base_config)["name"] == sentinel

    # Restored to the committed base config.
    assert _api_info(base_config)["name"] == base_name


@pytest.mark.vz
@pytest.mark.slow
def test_run_preflight_captures_startup_failure(vz_server_dev):
    """`run_preflight` captures a non-zero exit + stderr.

    Forces a deterministic, port-bind startup failure by launching a
    second server on the dev server's own (occupied) port. This exercises
    the exact subprocess-launch-and-capture path Phase 6's public-exposure
    preflight will use to assert a refuse-to-start.
    """
    result = devcontrol.run_preflight({})  # base config -> reuses :18080 -> bind conflict
    assert not result.timed_out, (
        "expected the second server to fail to bind, but it stayed up; "
        f"stderr={result.stderr[-500:]!r}"
    )
    assert result.returncode not in (0, None), (
        f"expected non-zero exit; stdout={result.stdout[-500:]!r} "
        f"stderr={result.stderr[-500:]!r}"
    )
    assert result.stderr.strip(), "expected a diagnostic on stderr"
