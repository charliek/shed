"""mtls auth mode — live integration tests.

`auth.mode: mtls` issues a short-lived client certificate over the SSH
`_bootstrap` channel instead of a bearer token, and the HTTPS listener enforces
`RequireAndVerifyClientCert` with per-request re-validation (see
`internal/config/server.go`, `internal/servertls/ca.go`,
`internal/sshd/bootstrap.go`). This module drives that live, against the
parallel dev server, which runs mtls by default (both committed
`configs/server.dev-parallel.*.yaml` are mtls; see
`docs/development/testing.md` / `tests/integration/README.md`).

**Detection.** mtls is the only enforced mode shed-server has, and both
committed dev configs run it, so this module needs no mode gate. The suite
cannot probe `GET /api/info` unauthenticated to confirm it — mtls enforces
`RequireAndVerifyClientCert` on the whole TLS listener, so an uncredentialed
client never gets far enough to receive ANY HTTP response (exactly what
`test_bare_tls_probe_without_client_cert_fails_before_http` below asserts).
Individual tests assert against the *client entry's* cached `auth_mode` in
`~/.shed/config.yaml` (or a throwaway `$HOME`'s copy of it), per
`internal/config/client.go: ServerEntry.AuthMode` — that field is populated by
a real bootstrap exchange, not probed unauthenticated.

**One run.** `make test-integration-dev` (or `-dev-fc`) runs this module
with the rest of the suite:

    make dev-server-restart && make test-integration-dev    # (or -fc)

(The live token-to-mtls mode-flip test that used to live here went with the
token mode: a server no longer loads a token config to flip from. A client
entry recorded against a 0.9.x token server still migrates on its next
request; that is client code, covered by `cmd/shed` unit tests.)
"""

from __future__ import annotations

import os
import socket
import ssl
import stat
import subprocess
import time
import urllib.error
import urllib.request
from pathlib import Path
from typing import Optional

import pytest
import yaml

from fixtures.devcontrol import KNOWN_HOSTS_PATH, isolated_home_with_identity
from fixtures.server import resolve_server_entry

# The exact upgrade-error string a pre-mtls-aware client gets from an mtls
# server (internal/sshd/bootstrap.go: errBootstrapNeedsCSR). Part of the wire
# protocol, not incidental wording — kept in lockstep with that Go string.
_UPGRADE_ERROR = "this server requires auth.mode: mtls; upgrade shed (client certificate support)"


# ----------------------------------------------------------------------------
# Small helpers
# ----------------------------------------------------------------------------


def _load_client_entry(config_path: Path, name: str) -> Optional[dict]:
    """Read a single server entry directly out of a `~/.shed/config.yaml`-
    shaped file (real or a throwaway `$HOME`'s copy). Returns None if the file
    or the entry doesn't exist.

    This is the "client entry's auth_mode" detection/assertion signal called
    out in this module's docstring — deliberately a direct YAML read, not a
    `shed --json server list` parse: that command's JSON output (see
    `cmd/shed/server.go: runServerList`) does not expose `auth_mode` at all
    today (it only reports a coarser `security` label), so it can't
    distinguish token from mtls.
    """
    if not config_path.exists():
        return None
    data = yaml.safe_load(config_path.read_text()) or {}
    return (data.get("servers") or {}).get(name)


def _shed(args: list[str], home: str, timeout: float = 60) -> subprocess.CompletedProcess:
    env = dict(os.environ, HOME=home)
    return subprocess.run(["shed", *args], capture_output=True, text=True, timeout=timeout, env=env)


def _free_local_port() -> int:
    s = socket.socket()
    try:
        s.bind(("127.0.0.1", 0))
        return s.getsockname()[1]
    finally:
        s.close()


# ----------------------------------------------------------------------------
# SSH-first `server add` against an mtls server
# ----------------------------------------------------------------------------


def test_ssh_first_add_produces_mtls_entry(shed_server_dev):
    """`shed server add <host> --ssh-port <port> --trust-on-first-use` against
    the mtls dev server, with no manual step beyond the allowlisted SSH key
    (see `fixtures.devcontrol.isolated_home_with_identity`): the resulting entry records
    `auth_mode: mtls`, and the issued credential lands on disk exactly per
    `internal/config/clientcreds.go`'s documented layout — a 0700 per-server
    dir containing 0600 `client.pem` / `client.key`.
    """
    server = shed_server_dev
    entry = resolve_server_entry(server.name)
    assert entry is not None, f"{server.name!r} not found in ~/.shed/config.yaml"
    host = entry.get("host", "localhost")
    ssh_port = int(entry["ssh_port"])

    with isolated_home_with_identity() as home:
        name = "itest-mtls-add"
        r = _shed(
            ["server", "add", host, "--ssh-port", str(ssh_port),
             "--trust-on-first-use", "--name", name],
            home,
        )
        assert r.returncode == 0, f"server add failed: {r.stdout!r} {r.stderr!r}"

        cfg_path = Path(home) / ".shed" / "config.yaml"
        added = _load_client_entry(cfg_path, name)
        assert added is not None, f"no {name!r} entry written to {cfg_path}"
        assert added.get("auth_mode") == "mtls", (
            f"auth_mode = {added.get('auth_mode')!r}, want 'mtls' "
            f"(entry: {added!r})"
        )

        creds_dir = Path(home) / ".shed" / "creds" / name
        assert creds_dir.is_dir(), f"no creds dir at {creds_dir}"
        dir_mode = stat.S_IMODE(creds_dir.stat().st_mode)
        assert dir_mode == 0o700, f"creds dir {creds_dir} perm = {oct(dir_mode)}, want 0700"

        cert_path = creds_dir / "client.pem"
        key_path = creds_dir / "client.key"
        assert cert_path.is_file(), f"missing {cert_path}"
        assert key_path.is_file(), f"missing {key_path}"
        for p in (cert_path, key_path):
            mode = stat.S_IMODE(p.stat().st_mode)
            assert mode == 0o600, f"{p} perm = {oct(mode)}, want 0600"

        # The freshly issued certificate actually authenticates.
        r = _shed(["-s", name, "list"], home)
        assert r.returncode == 0, (
            f"control plane over the fresh mtls entry failed: "
            f"{r.stdout!r} {r.stderr!r}"
        )


# ----------------------------------------------------------------------------
# create -> list -> exec, and a tunnel, over mtls
# ----------------------------------------------------------------------------


@pytest.mark.slow
def test_create_list_exec_over_mtls(shed_server_dev, test_shed_name_dev):
    """One `shed create` (to stay inside the suite's timing budget), then
    `list` + `exec` over the same mtls-authenticated control plane —
    the create-cycle smoke test, reusing `LocalServer`/`RemoteServer` exactly
    as the rest of the suite does (`fixtures/server.py`)."""
    server = shed_server_dev
    name = test_shed_name_dev
    server.create(name, image="base")

    assert name in server.list_shed_names(), f"{name!r} missing from list: {server.list_shed_names()}"

    r = server.exec(name, ["echo", "hello-mtls"])
    assert r.returncode == 0, f"exec failed: {r.stdout!r} {r.stderr!r}"
    assert "hello-mtls" in r.stdout, f"unexpected exec output: {r.stdout!r}"


@pytest.mark.slow
def test_tunnel_over_mtls(shed_server_dev, test_shed_name_dev):
    """`shed tunnels start -d` (the `shed forward` tunnel machinery) over an
    mtls-authenticated control plane, with a REAL byte round-trip through the
    tunnel: the guest's SSH daemon sends its banner first, so reading it back
    off the local forwarded port proves bytes actually flowed shed->guest,
    not just that the local listener accepted a TCP connection."""
    server = shed_server_dev
    name = test_shed_name_dev
    server.create(name, image="base")

    local_port = _free_local_port()
    try:
        r = subprocess.run(
            ["shed", "-s", server.name, "tunnels", "start", name,
             "-t", f"{local_port}:22", "-d"],
            capture_output=True, text=True, timeout=30,
        )
        assert r.returncode == 0, f"tunnels start failed: {r.stdout!r} {r.stderr!r}"

        deadline = time.monotonic() + 15.0
        banner = b""
        last_err: Optional[Exception] = None
        while time.monotonic() < deadline:
            try:
                with socket.create_connection(("127.0.0.1", local_port), timeout=5) as s:
                    s.settimeout(5)
                    banner = s.recv(64)
                break
            except OSError as e:
                last_err = e
                time.sleep(0.3)
        assert banner.startswith(b"SSH-"), (
            f"no SSH banner through the mtls-authenticated tunnel "
            f"(last connect error: {last_err}); got {banner!r}"
        )
    finally:
        subprocess.run(
            ["shed", "-s", server.name, "tunnels", "stop", name],
            capture_output=True, text=True, timeout=15,
        )


# ----------------------------------------------------------------------------
# Bare TLS probe with no client certificate
# ----------------------------------------------------------------------------


def test_bare_tls_probe_without_client_cert_fails_before_http(shed_server_dev):
    """A TLS connection that never presents a client certificate must be
    refused during the handshake itself (`RequireAndVerifyClientCert`), not
    merely answered with a non-200 — proving `/api/info`'s bootstrap-exempt
    status at the HTTP layer does not leak through an mtls listener at the
    transport layer. `ssl._create_unverified_context()` skips SERVER-cert
    verification (this probe isn't testing that) and, critically, never loads
    a client cert either."""
    server = shed_server_dev
    entry = resolve_server_entry(server.name)
    assert entry is not None, f"{server.name!r} not found in ~/.shed/config.yaml"
    host = entry.get("host", "localhost")
    https_port = entry.get("https_port")
    assert https_port, f"no https_port on {server.name!r} entry: {entry!r}"

    ctx = ssl._create_unverified_context()
    got_status = None
    raised: Optional[Exception] = None
    try:
        with urllib.request.urlopen(
            f"https://{host}:{https_port}/api/info", timeout=10, context=ctx
        ) as resp:
            got_status = resp.status
    except (urllib.error.URLError, OSError) as e:
        raised = e

    assert got_status is None, (
        f"expected the TLS handshake to fail before any HTTP response; "
        f"instead got HTTP {got_status} without presenting a client certificate"
    )
    assert raised is not None, (
        "expected a TLS/connection failure with no client certificate, "
        "got neither an HTTP status nor an exception"
    )


# ----------------------------------------------------------------------------
# Old-style bootstrap (no CSR) against an mtls server
# ----------------------------------------------------------------------------


def test_old_style_bootstrap_without_csr_gets_upgrade_error(shed_server_dev):
    """Drives the `_bootstrap` SSH channel directly with a pre-mtls-shaped
    request line (`"<scope> <kind>"`, no `csr=...` argument) — what any client
    built before client-certificate support existed sends. The mtls server
    must refuse with the exact upgrade message
    (`internal/sshd/bootstrap.go: errBootstrapNeedsCSR`), not a generic error,
    since that string is what an old-client user pastes into an issue."""
    server = shed_server_dev
    entry = resolve_server_entry(server.name)
    assert entry is not None, f"{server.name!r} not found in ~/.shed/config.yaml"
    host = entry.get("host", "localhost")
    ssh_port = int(entry["ssh_port"])

    r = subprocess.run(
        [
            "ssh", "-T", "-p", str(ssh_port),
            "-o", "BatchMode=yes",
            "-o", f"UserKnownHostsFile={KNOWN_HOSTS_PATH}",
            "-o", "StrictHostKeyChecking=yes",
            f"_bootstrap@{host}", "control", "cli",
        ],
        capture_output=True, text=True, timeout=20,
    )
    assert r.returncode == 1, (
        f"expected exit 1 for a CSR-less bootstrap against an mtls server, "
        f"got {r.returncode}: stdout={r.stdout!r} stderr={r.stderr!r}"
    )
    assert _UPGRADE_ERROR in r.stderr, f"unexpected bootstrap error: {r.stderr!r}"
