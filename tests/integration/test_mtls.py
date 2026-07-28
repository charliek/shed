"""mtls auth mode — live integration tests.

`auth.mode: mtls` issues a short-lived client certificate over the SSH
`_bootstrap` channel instead of a bearer token, and the HTTPS listener enforces
`RequireAndVerifyClientCert` with per-request re-validation (see
`internal/config/server.go`, `internal/servertls/ca.go`,
`internal/sshd/bootstrap.go`). This module drives that live, against the
parallel dev server, in `SHED_DEV_AUTH_MODE=mtls` (see the Makefile knob added
alongside this module and documented in `docs/development/testing.md` /
`tests/integration/README.md`).

**Detection.** The suite cannot probe `GET /api/info` to find out whether a
server is in mtls mode — that endpoint is bootstrap-exempt at the HTTP layer,
but mtls enforces `RequireAndVerifyClientCert` on the whole TLS listener, so an
uncredentialed client never gets far enough to receive ANY HTTP response,
including from bootstrap-exempt routes (this is exactly what
`test_bare_tls_probe_without_client_cert_fails_before_http` below asserts).
Instead, this module gates on the `SHED_DEV_AUTH_MODE` environment variable
(set by `make test-integration-dev[-fc]`, forwarded from the same knob that
picked the dev server's config — see the Makefile) — the whole module skips
unless it is exactly `"mtls"`. Individual tests additionally assert against the
*client entry's* cached `auth_mode` in `~/.shed/config.yaml` (or a throwaway
`$HOME`'s copy of it) as their actual pass/fail signal, per
`internal/config/client.go: ServerEntry.AuthMode` — that field is populated by
a real bootstrap exchange, not probed unauthenticated.

**Two-config choreography** (see docs/development/testing.md for the full
version): run the suite once against the dev server in `token` mode (today's
default — the existing modules, unaffected by this one), then flip the dev
server to mtls and run this module:

    make dev-server-restart SHED_DEV_AUTH_MODE=mtls        # (or -fc)
    SHED_DEV_AUTH_MODE=mtls make test-integration-dev       # (or -fc)

**Mode-flip test.** `test_mode_flip_migrates_live` restarts the *actual*
parallel-dev server mid-test (via the Makefile's `SHED_DEV_AUTH_MODE` knob,
never a hand-built override config) to prove the live migration in both
directions. That is more invasive than every other test in this module (and
than the existing `dev_config()`-based auth tests, which restart against a
merged-but-still-static config) — restarting the dev server the *rest of this
module's run* depends on being in mtls mode is a real footgun if the test
fails halfway. It is therefore VZ-only (mirroring `fixtures/devcontrol.py`'s
documented FC-config-mutation-is-out-of-scope stance) and gated behind an
explicit opt-in env var, `SHED_MTLS_FLIP_TEST=1`, so a plain
`SHED_DEV_AUTH_MODE=mtls make test-integration-dev` run never restarts the
server out from under itself.
"""

from __future__ import annotations

import os
import socket
import ssl
import stat
import subprocess
import tempfile
import time
import urllib.error
import urllib.request
from contextlib import contextmanager
from pathlib import Path
from typing import Optional

import pytest
import yaml

from fixtures.devcontrol import DEV_AUTH_MODE, KNOWN_HOSTS_PATH, REPO_ROOT
from fixtures.server import resolve_server_entry

# Single source of truth for SHED_DEV_AUTH_MODE detection lives in
# fixtures/devcontrol.py (DEV_AUTH_MODE) — imported here so this module and
# every mtls-aware skip elsewhere in the suite (skip_mtls_reconfigure /
# skip_mtls_token_semantics) can never disagree about what "mtls mode" means.
SHED_DEV_AUTH_MODE = DEV_AUTH_MODE

pytestmark = pytest.mark.skipif(
    SHED_DEV_AUTH_MODE != "mtls",
    reason=(
        f"SHED_DEV_AUTH_MODE={SHED_DEV_AUTH_MODE!r} (want 'mtls'): this module "
        "only runs against a dev server started with SHED_DEV_AUTH_MODE=mtls. "
        "Run `make dev-server-restart SHED_DEV_AUTH_MODE=mtls` (or `-fc`), then "
        "`SHED_DEV_AUTH_MODE=mtls make test-integration-dev` (or `-fc`). "
        "See docs/development/testing.md."
    ),
)

FLIP_TEST_ENV = "SHED_MTLS_FLIP_TEST"

# The identity copied into a throwaway $HOME for the SSH-first `server add`
# test (see `_isolated_home_with_identity`). Must already be allowlisted by
# the dev server's `auth.ssh` — today `github_users: [charliek]` on both
# configs/server.dev-parallel.mac.yaml and .linux-fc.yaml (see
# scripts/render-dev-mtls-config.sh) — which is the "allowlisted SSH key"
# precondition this module's docstring and the plan both refer to; nothing
# here sets that allowlisting up.
_REAL_SSH_DIR = Path.home() / ".ssh"
_DEFAULT_IDENTITY = "id_ed25519"

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


@contextmanager
def _isolated_home_with_identity():
    """A throwaway `$HOME` carrying a copy of the developer's real default SSH
    identity, so system `ssh`'s default identity-file resolution (which honors
    the `HOME` env var for `~/.ssh/id_ed25519`) finds a usable key without
    depending on an ssh-agent being loaded. `shed server add`'s SSH-first
    bootstrap shells out to the real `ssh` binary
    (`sdk/bootstrap/bootstrap.go`), so this is what lets the add run against a
    config- and creds-isolated `$HOME` (never the developer's real
    `~/.shed/config.yaml`) while still authenticating as a key the dev server
    allowlists.

    Skips the calling test if no default identity exists to copy — this
    module assumes the same one-time developer setup every other auth test in
    this suite already assumes (see e.g. `test_bootstrap.py`,
    `test_http_auth.py`, which read `~/.ssh/id_ed25519.pub` directly).
    """
    priv = _REAL_SSH_DIR / _DEFAULT_IDENTITY
    pub = _REAL_SSH_DIR / f"{_DEFAULT_IDENTITY}.pub"
    if not priv.exists() or not pub.exists():
        pytest.skip(f"no default SSH identity at {priv} to copy into a throwaway $HOME")
    with tempfile.TemporaryDirectory() as home:
        ssh_dir = Path(home) / ".ssh"
        ssh_dir.mkdir(mode=0o700)
        for src in (priv, pub):
            dst = ssh_dir / src.name
            dst.write_bytes(src.read_bytes())
            dst.chmod(0o600)
        yield home


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


def _wait_tcp_open(host: str, port: int, timeout: float) -> None:
    """Poll a raw TCP connect (no TLS, no HTTP) until it succeeds.

    Used as the post-restart readiness probe for the mode-flip test: a
    connect-only probe works identically for BOTH auth modes, unlike an
    `/api/info` fetch (unreachable under mtls — see the module docstring).
    Proves the process is listening again; the actual functional assertion is
    the `shed -s <name> list` call that follows.
    """
    deadline = time.monotonic() + timeout
    last_err: Optional[Exception] = None
    while time.monotonic() < deadline:
        try:
            with socket.create_connection((host, port), timeout=3):
                return
        except OSError as e:
            last_err = e
            time.sleep(0.5)
    raise AssertionError(f"{host}:{port} not accepting connections within {timeout:.0f}s ({last_err})")


def _shed_list_ok(server: str, timeout: float = 30.0) -> subprocess.CompletedProcess:
    """Retry `shed -s <server> list` for a bit after a restart.

    A single attempt right after `_wait_tcp_open` can still race the Go
    process finishing its own startup (SSH server, allowlist refresh, etc.);
    this absorbs that without masking a genuine failure — it returns the
    LAST attempt's result once the deadline passes, so a real failure still
    reports real stdout/stderr.
    """
    deadline = time.monotonic() + timeout
    r = subprocess.run(["shed", "-s", server, "list"], capture_output=True, text=True, timeout=30)
    while r.returncode != 0 and time.monotonic() < deadline:
        time.sleep(1)
        r = subprocess.run(["shed", "-s", server, "list"], capture_output=True, text=True, timeout=30)
    return r


# ----------------------------------------------------------------------------
# SSH-first `server add` against an mtls server
# ----------------------------------------------------------------------------


def test_ssh_first_add_produces_mtls_entry(shed_server_dev):
    """`shed server add <host> --ssh-port <port> --trust-on-first-use` against
    the mtls dev server, with no manual step beyond the allowlisted SSH key
    (see `_isolated_home_with_identity`): the resulting entry records
    `auth_mode: mtls`, and the issued credential lands on disk exactly per
    `internal/config/clientcreds.go`'s documented layout — a 0700 per-server
    dir containing 0600 `client.pem` / `client.key`.
    """
    server = shed_server_dev
    entry = resolve_server_entry(server.name)
    assert entry is not None, f"{server.name!r} not found in ~/.shed/config.yaml"
    host = entry.get("host", "localhost")
    ssh_port = int(entry["ssh_port"])

    with _isolated_home_with_identity() as home:
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


# ----------------------------------------------------------------------------
# Mode-flip migration, live, both directions (opt-in — see module docstring)
# ----------------------------------------------------------------------------


@pytest.mark.vz
@pytest.mark.slow
@pytest.mark.skipif(
    os.environ.get(FLIP_TEST_ENV) != "1",
    reason=(
        f"opt-in only: set {FLIP_TEST_ENV}=1 to run. This test restarts the "
        "real parallel-dev VZ server via the Makefile's SHED_DEV_AUTH_MODE "
        "knob, twice (token, then back to mtls) — more invasive than every "
        "other test in this module, which only ever read the already-running "
        "dev server. See the module docstring for why this is VZ-only and "
        "opt-in rather than always-on."
    ),
)
def test_mode_flip_migrates_live(vz_server_dev):
    server = vz_server_dev.name
    entry = resolve_server_entry(server)
    assert entry is not None, f"{server!r} not found in ~/.shed/config.yaml"
    https_port = int(entry.get("https_port") or 18443)
    real_cfg = Path.home() / ".shed" / "config.yaml"

    def auth_mode() -> str:
        e = _load_client_entry(real_cfg, server)
        assert e is not None, f"{server!r} missing from {real_cfg}"
        # ABSENT MEANS TOKEN (internal/config/client.go: ServerEntry.AuthMode).
        return e.get("auth_mode") or "token"

    def restart(mode: str) -> None:
        r = subprocess.run(
            ["make", "dev-server-restart", f"SHED_DEV_AUTH_MODE={mode}"],
            cwd=REPO_ROOT, capture_output=True, text=True, timeout=300,
        )
        assert r.returncode == 0, (
            f"dev-server-restart SHED_DEV_AUTH_MODE={mode} failed: "
            f"stdout={r.stdout[-2000:]!r} stderr={r.stderr[-2000:]!r}"
        )
        # TCP-connect-only probe: valid under EITHER auth mode, unlike an
        # /api/info fetch (unreachable under mtls — see module docstring).
        _wait_tcp_open("localhost", https_port, timeout=90)

    assert auth_mode() == "mtls", (
        "this module only runs with SHED_DEV_AUTH_MODE=mtls; the dev server "
        "should already be in mtls mode at the start of this test"
    )

    try:
        # --- mtls -> token ---
        restart("token")
        r = _shed_list_ok(server)
        assert r.returncode == 0, (
            f"shed -s {server} list failed after the server flipped to token "
            f"(no manual re-add should be needed): {r.stdout!r} {r.stderr!r}"
        )
        assert auth_mode() == "token", (
            f"entry auth_mode did not migrate to 'token' after the server "
            f"flipped modes (still {auth_mode()!r})"
        )

        # --- token -> mtls ---
        restart("mtls")
        r = _shed_list_ok(server)
        assert r.returncode == 0, (
            f"shed -s {server} list failed after the server flipped back to "
            f"mtls (no manual re-add should be needed): {r.stdout!r} {r.stderr!r}"
        )
        assert auth_mode() == "mtls", (
            f"entry auth_mode did not migrate back to 'mtls' after the "
            f"server flipped back (still {auth_mode()!r})"
        )
    finally:
        # Leave the dev server in mtls: the rest of this module (and a
        # developer's next SHED_DEV_AUTH_MODE=mtls test-integration-dev run)
        # assumes that's what's actually running. Best-effort — a failure
        # here is reported but doesn't mask the real assertion above.
        r = subprocess.run(
            ["make", "dev-server-restart", "SHED_DEV_AUTH_MODE=mtls"],
            cwd=REPO_ROOT, capture_output=True, text=True, timeout=300,
        )
        if r.returncode == 0:
            try:
                _wait_tcp_open("localhost", https_port, timeout=90)
            except AssertionError as e:
                print(f"[test_mtls] WARNING: dev server not reachable after final mtls restore: {e}")
        else:
            print(
                f"[test_mtls] WARNING: failed to restore dev server to mtls: "
                f"stdout={r.stdout[-1000:]!r} stderr={r.stderr[-1000:]!r}"
            )
