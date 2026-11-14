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

import pytest

import fixtures.devcontrol as devcontrol


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
    `shed server add` now records for an mtls server,
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


@pytest.mark.vz
@pytest.mark.slow
def test_dev_config_roundtrips_override(vz_server_dev, tmp_path):
    """`dev_config` applies an override through a real restart and restores.

    Overrides the server's reported `name` (a cosmetic field surfaced by
    /api/info, with no effect on the CLI entry which resolves by
    host:port) so the change is observable and harmless. The dev server is
    mtls throughout, so every read presents a freshly enrolled certificate
    (`devcontrol.api_info`) — which is also what `dev_config` itself waits on.
    """
    server = vz_server_dev.name
    base_config = devcontrol._merge_config({})

    base_name = devcontrol.api_info(server, base_config, tmp_path / "before")["name"]
    sentinel = "harness-selfcheck-name"
    assert base_name != sentinel

    with devcontrol.dev_config({"name": sentinel}, server):
        assert devcontrol.api_info(server, devcontrol._merge_config({"name": sentinel}), tmp_path / "during")["name"] == sentinel

    # Restored to the committed base config.
    assert devcontrol.api_info(server, base_config, tmp_path / "after")["name"] == base_name


@pytest.mark.vz
@pytest.mark.slow
def test_dev_config_runs_open_mode_on_loopback(vz_server_dev, tmp_path):
    """`dev_config` can run the dev server as an open server on loopback —
    the one other mode shed-server has — and returns it to mtls.

    In open mode `/api/info` answers plain HTTP with no credential and
    reports `open`; back on the base it reports `mtls`, and only to a
    request that presents a certificate."""
    server = vz_server_dev.name
    open_config = devcontrol._merge_config(devcontrol.OPEN_MODE_OVERRIDES)
    assert open_config.get("bind_address") in (None, "", "127.0.0.1", "localhost"), (
        "the committed base must keep the dev server on loopback for open mode to load"
    )

    with devcontrol.dev_config(devcontrol.OPEN_MODE_OVERRIDES, server):
        info = devcontrol.api_info(server, open_config, tmp_path / "open")
        assert info.get("auth_mode") == "open", f"open-mode /api/info: {info!r}"

    info = devcontrol.api_info(server, devcontrol._merge_config({}), tmp_path / "mtls")
    assert info.get("auth_mode") == "mtls", f"restored /api/info: {info!r}"


@pytest.mark.vz
@pytest.mark.slow
def test_run_preflight_captures_startup_failure(vz_server_dev):
    """`run_preflight` captures a non-zero exit + stderr.

    Forces a deterministic, port-bind startup failure by launching a
    second server on the dev server's own (occupied) port. This exercises
    the exact subprocess-launch-and-capture path Phase 6's public-exposure
    preflight will use to assert a refuse-to-start.
    """
    result = devcontrol.run_preflight({})  # base config -> reuses :18443/:12222 -> bind conflict
    assert not result.timed_out, (
        "expected the second server to fail to bind, but it stayed up; "
        f"stderr={result.stderr[-500:]!r}"
    )
    assert result.returncode not in (0, None), (
        f"expected non-zero exit; stdout={result.stdout[-500:]!r} "
        f"stderr={result.stderr[-500:]!r}"
    )
    assert result.stderr.strip(), "expected a diagnostic on stderr"
