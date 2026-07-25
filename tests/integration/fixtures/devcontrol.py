"""Dev-server control primitives for config-mutating integration tests.

The `LocalServer` / `RemoteServer` fixtures (`server.py`) drive a
*running, static-config* shed-server via `shed -s <name>`. Several
auth/transport phases need more than that:

  - Set a server config, restart the dev server, assert the new
    behavior, then restore (Phase 1 bind config, Phase 2 `auth.ssh`
    mode, Phase 4 token, Phase 5 TLS).
  - Assert the server *refuses to start* with an incomplete public
    bundle (Phase 6 preflight) — a shape the running-server fixtures
    cannot express at all.

This module adds three primitives, all **hard-scoped to the `*-dev`
servers on offset ports** so an autonomous run can never touch
production. There are two independent safety gates:

  - `assert_dev_target(name)`  — the restart *target* must be a `*-dev`
    entry on offset ports (never the brew/deb production `8080/2222`).
  - `_assert_config_ports_safe()` — the *config* a server is (re)started
    with must not name the production ports either, so a stray override
    like `{"http_port": 8080}` can't bind prod even if prod is down.

The mutation primitives:

  - `dev_config(overrides, server)` — context manager: write a temp
    config merged from the committed dev-parallel base, restart the VZ
    dev server against it, yield, then **always** restore the base
    config (pinned explicitly). Never edits the committed
    `configs/server.dev-parallel.*.yaml`.
  - `run_preflight(overrides)` — launch `bin/shed-server serve` as a
    throwaway subprocess against a merged temp config, with a timeout,
    and return a `PreflightResult` so a test can assert a non-zero exit
    + stderr (expected-startup-failure).

Temp configs live under `~/.shed/dev/`, never the committed configs.

FC (remote) config mutation is intentionally out of scope here: per the
plan, config-mutation integration is VZ-only, with FC covered by Go unit
tests plus a manual pass.

**Auth-mode awareness.** This module (plus `test_mtls.py`) is what a
config-mutating test actually calls, so it is also the single source of
truth for `SHED_DEV_AUTH_MODE` detection (`DEV_AUTH_MODE` below) — the
exact mechanism `test_mtls.py`'s module docstring documents (env var, never
an unauthenticated `/api/info` probe, which is unreachable once a server is
actually in mtls). Two ready-made `pytest.mark.skipif` marks are exported
for tests elsewhere in the suite that structurally cannot run against an
mtls dev server, expressing the two distinct reasons a test in this suite
runs into that wall:

  - `skip_mtls_reconfigure` — the test calls `dev_config()` (or otherwise
    depends on the dev server running the *committed* base config) to set
    up a scenario unrelated to bearer tokens (TLS pinning, SSH allowlist
    mechanics, the `dev_config()` round-trip itself). Under
    `SHED_DEV_AUTH_MODE=mtls` the running server executes a *generated*
    config (`~/.shed/dev/server.mtls-generated.yaml`, produced by
    `scripts/render-dev-mtls-config.sh` at `dev-server-up`/`-restart`
    time) instead — `dev_config()` refuses to touch it (see the guard at
    the top of `dev_config()` below) rather than silently clobbering the
    live mtls server other work may depend on.
  - `skip_mtls_token_semantics` — the test's actual assertions are about
    bearer-credential semantics (scoped HTTP tokens, TTL expiry,
    allowlist-gated minting, the open-mode single-plain-listener shape)
    that don't exist under mtls at all: the mtls client authenticates with
    a short-lived certificate, never a bearer token (see
    `internal/servertls/ca.go`, `internal/config/server.go`).

Both marks are inert (no-ops) when `SHED_DEV_AUTH_MODE` is unset or
`"token"` (today's default) — see `docs/development/testing.md` and
`tests/integration/README.md` for the full token-vs-mtls run split.
"""

from __future__ import annotations

import copy
import json
import os
import signal
import ssl
import subprocess
import time
import urllib.error
import urllib.request
from contextlib import contextmanager
from dataclasses import dataclass
from pathlib import Path
from typing import Iterator, Optional

import pytest
import yaml

from .server import resolve_server_entry

# fixtures/ -> integration/ -> tests/ -> repo root
REPO_ROOT = Path(__file__).resolve().parents[3]
DEV_BASE_CONFIG = REPO_ROOT / "configs" / "server.dev-parallel.mac.yaml"
SHED_SERVER_BIN = REPO_ROOT / "bin" / "shed-server"
DEV_SCRATCH_DIR = Path.home() / ".shed" / "dev"
DEV_CONFIG_TMP = DEV_SCRATCH_DIR / "test-dev-config.yaml"
PREFLIGHT_CONFIG_TMP = DEV_SCRATCH_DIR / "test-preflight-config.yaml"
# The pinned known_hosts the CLI bootstrap reads (config.GetKnownHostsPath()).
KNOWN_HOSTS_PATH = Path.home() / ".shed" / "known_hosts"

# The brew/deb production ports. A target/config is only safe if it is
# NOT on these — the structural guarantee of a parallel-dev server (see
# configs/server.dev-parallel.mac.yaml: offset 18080/12222).
PROD_HTTP_PORT = 8080
PROD_SSH_PORT = 2222

# ----------------------------------------------------------------------------
# Auth-mode detection (single source of truth — see the module docstring's
# "Auth-mode awareness" section). Matches test_mtls.py's own module-level
# read of the same env var exactly; both must agree on what "mtls mode" means.
# ----------------------------------------------------------------------------

DEV_AUTH_MODE = os.environ.get("SHED_DEV_AUTH_MODE", "token")

_MTLS_RECONFIGURE_REASON = (
    f"SHED_DEV_AUTH_MODE={DEV_AUTH_MODE!r}: this test calls dev_config() (or an "
    "equivalent dev-server-config-mutating helper) to set up a scenario "
    "unrelated to bearer tokens. Under SHED_DEV_AUTH_MODE=mtls the running "
    "dev server executes a GENERATED config "
    "(~/.shed/dev/server.mtls-generated.yaml, produced by "
    "scripts/render-dev-mtls-config.sh), not the committed base config "
    "dev_config() merges onto and restores — reconfiguring would silently "
    "flip the live mtls server other work may depend on. Run this test only "
    "with SHED_DEV_AUTH_MODE=token (the default)."
)

skip_mtls_reconfigure = pytest.mark.skipif(
    DEV_AUTH_MODE == "mtls", reason=_MTLS_RECONFIGURE_REASON
)

_MTLS_TOKEN_SEMANTICS_REASON = (
    f"SHED_DEV_AUTH_MODE={DEV_AUTH_MODE!r}: this test asserts bearer-token/"
    "token-mode semantics (scoped HTTP tokens, TTL expiry, allowlist-gated "
    "minting, the open-mode single-plain-listener shape, ...) that do not "
    "hold under mtls — the mtls client authenticates with a short-lived "
    "certificate, never a bearer token (see internal/servertls/ca.go, "
    "internal/config/server.go). Run this test only with "
    "SHED_DEV_AUTH_MODE=token (the default)."
)

skip_mtls_token_semantics = pytest.mark.skipif(
    DEV_AUTH_MODE == "mtls", reason=_MTLS_TOKEN_SEMANTICS_REASON
)


@dataclass
class PreflightResult:
    """Outcome of launching `shed-server serve` for a start-time check.

    `returncode is None` means the process was still running at the
    timeout (it started successfully and was killed) — for a preflight
    that is *expected to refuse to start*, that means the refusal did NOT
    happen. `timed_out` exposes exactly that condition.
    """

    returncode: Optional[int]
    stdout: str
    stderr: str

    @property
    def timed_out(self) -> bool:
        return self.returncode is None


def _ports(d: dict) -> tuple[int, int]:
    """Extract (http_port, ssh_port) from a config/entry dict, defaulting
    missing/blank values to 0 (which the safety checks treat as invalid)."""
    return int(d.get("http_port", 0) or 0), int(d.get("ssh_port", 0) or 0)


def assert_dev_target(name: str) -> dict:
    """Raise unless `name` is a parallel-dev server safe to reconfigure.

    The executable form of the plan's safety discipline: every
    config-mutating / restarting primitive calls it first, so an
    autonomous run physically cannot point a restart at the brew/deb
    production server. It *raises* (RuntimeError) rather than skips — a
    mis-targeted destructive test is a bug to surface loudly.

    Fail-closed gates, all of which must pass: the `-dev` name suffix
    (intent); the entry must resolve; its ports must be present and
    positive (a malformed entry is rejected, not defaulted through); and
    — the load-bearing one — the ports must not be the production ones
    (catches a `-dev`-suffixed entry pointing at the brew server, e.g.
    `localmac-dev` on :8080). Returns the resolved entry on success.
    """
    if not name.endswith("-dev"):
        raise RuntimeError(
            f"refusing to reconfigure non-dev server {name!r}: "
            f"config-mutating primitives only target a '*-dev' entry"
        )
    entry = resolve_server_entry(name)
    if entry is None:
        raise RuntimeError(
            f"dev server {name!r} not found in ~/.shed/config.yaml; "
            f"run 'make dev-server-up' and register it (see plan §4 P0)"
        )
    http_port, ssh_port = _ports(entry)
    if http_port <= 0 or ssh_port <= 0:
        raise RuntimeError(
            f"refusing to reconfigure {name!r}: malformed entry with "
            f"missing/invalid ports ({http_port}/{ssh_port})"
        )
    if http_port == PROD_HTTP_PORT or ssh_port == PROD_SSH_PORT:
        raise RuntimeError(
            f"refusing to reconfigure {name!r}: it is on production ports "
            f"({http_port}/{ssh_port}); a dev server must use offset ports "
            f"(e.g. 18080/12222)"
        )
    return entry


def _assert_config_ports_safe(config: dict) -> None:
    """Raise unless `config`'s ports are positive and non-production.

    Guards the *config a server is started with* (vs. `assert_dev_target`,
    which guards *which* entry is restarted). Without this, a stray
    override such as `{"http_port": 8080}` would have a dev restart — or a
    `run_preflight` subprocess — bind the production port whenever prod
    happens to be down.
    """
    http_port, ssh_port = _ports(config)
    if http_port <= 0 or ssh_port <= 0:
        raise RuntimeError(
            f"refusing to start a dev server with non-positive ports "
            f"({http_port}/{ssh_port})"
        )
    if http_port == PROD_HTTP_PORT or ssh_port == PROD_SSH_PORT:
        raise RuntimeError(
            f"refusing to start a dev server on production ports "
            f"({http_port}/{ssh_port}); use offset ports (e.g. 18080/12222)"
        )


def _deep_merge(base: dict, overrides: dict) -> dict:
    """Recursively merge `overrides` onto a deep copy of `base`.

    Dict values merge key-by-key; every other value (scalars, lists)
    replaces wholesale. Nested merge is needed so a later phase can
    override just `auth.ssh.mode` or `vz.default_image` without clobbering
    sibling keys in the committed base config.
    """
    out = copy.deepcopy(base)
    for k, v in overrides.items():
        if isinstance(v, dict) and isinstance(out.get(k), dict):
            out[k] = _deep_merge(out[k], v)
        else:
            out[k] = copy.deepcopy(v)
    return out


def _merge_config(overrides: dict) -> dict:
    """Merge `overrides` onto the committed dev-parallel base config."""
    with DEV_BASE_CONFIG.open("r", encoding="utf-8") as f:
        base = yaml.safe_load(f) or {}
    return _deep_merge(base, overrides or {})


def _write_config(config: dict, path: Path) -> Path:
    """Write `config` to `path` under `~/.shed/dev/` (never the committed
    base config)."""
    DEV_SCRATCH_DIR.mkdir(parents=True, exist_ok=True)
    with path.open("w", encoding="utf-8") as f:
        yaml.safe_dump(config, f, sort_keys=False)
    return path


def _make_restart(dev_config: Path) -> None:
    """`make dev-server-restart DEV_CONFIG=<path>` — always with an
    explicit config so the restart never inherits a stale/foreign
    `DEV_CONFIG` from a parent `make`/`MAKEFLAGS`.

    Command-line variable assignments win over the Makefile's `:=`
    default. `dev-server-down` keys off the PID file, so it stops whatever
    is running regardless of config. (The restart drags the Makefile's
    `build` prerequisite along; the Go build cache makes that cheap and
    the VM/server spin-up dominates the latency, so it's left as-is.)
    """
    r = subprocess.run(
        ["make", "dev-server-restart", f"DEV_CONFIG={dev_config}"],
        cwd=REPO_ROOT, capture_output=True, text=True, timeout=300,
    )
    if r.returncode != 0:
        raise AssertionError(
            f"dev-server-restart failed (exit {r.returncode}): "
            f"stdout={r.stdout[-2000:]!r} stderr={r.stderr[-2000:]!r}"
        )


def _wait_reachable(
    server: str, timeout: float, *, secure: bool = False, https_port: Optional[int] = None
) -> None:
    """Poll the server's bootstrap `/api/info` endpoint until it answers.

    `/api/info` is unauthenticated (bootstrap-exempt), so this readiness check
    works even when the server is restarted with HTTP-auth enforcement on —
    unlike a `shed list` probe, a control-plane call that would 401 under
    enforce. `make dev-server-up` returns as soon as it has launched the
    process, so the caller must wait for actual readiness here.

    In secure mode the server serves NO plain-HTTP listener (TLS-only), so the
    probe targets `https://<host>:<https_port>/api/info`. The cert is the
    server's self-signed one; this readiness probe skips verification — it only
    needs a 200 from a bootstrap-exempt endpoint, and the real tests pin the
    cert for their actual assertions.
    """
    entry = resolve_server_entry(server)
    if entry is None:
        raise AssertionError(f"dev server {server!r} not registered in ~/.shed/config.yaml")
    host = entry.get("host", "localhost")
    ctx = None
    if secure:
        port = int(https_port or 8443)  # secure mode defaults https_port to 8443
        url = f"https://{host}:{port}/api/info"
        ctx = ssl._create_unverified_context()
    else:
        url = f"http://{host}:{int(entry['http_port'])}/api/info"
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        try:
            with urllib.request.urlopen(url, timeout=5, context=ctx) as resp:
                if resp.status == 200:
                    return
        except (urllib.error.URLError, OSError):
            pass
        time.sleep(0.5)
    raise AssertionError(f"dev server {server!r} not reachable within {timeout:.0f}s (url={url})")


@contextmanager
def dev_config(
    overrides: dict,
    server: str,
    *,
    ready_timeout: float = 90.0,
) -> Iterator[str]:
    """Run the VZ dev server with `overrides` merged onto its base config.

    Restarts the dev server against a temp config, waits for it to come
    back up, yields the server name, then **always** restores the base
    config on exit (pinned explicitly, even if the test body raises).
    Never edits the committed dev-parallel config.

    Guarded by `assert_dev_target` (the target) and
    `_assert_config_ports_safe` (the merged config's ports), so it can
    only ever restart a `*-dev` server on offset ports. Pass `server`
    from the `vz_server_dev` fixture's `.name`.

    Raises (does not skip) when `SHED_DEV_AUTH_MODE=mtls`: this is a
    defense-in-depth rail, not the primary mechanism — callers should
    prefer skipping up front via `skip_mtls_reconfigure` (see the module
    docstring) so a test never even attempts the restart under mtls. This
    guard exists for any caller that doesn't.
    """
    if DEV_AUTH_MODE == "mtls":
        raise RuntimeError(
            f"refusing to reconfigure {server!r}: SHED_DEV_AUTH_MODE=mtls means "
            f"it is running a GENERATED config "
            f"(~/.shed/dev/server.mtls-generated.yaml, produced by "
            f"scripts/render-dev-mtls-config.sh), not the committed base config "
            f"{DEV_BASE_CONFIG} that dev_config() merges onto and restores; "
            f"tests that reconfigure the dev server must skip under mtls "
            f"(see fixtures.devcontrol.skip_mtls_reconfigure)"
        )
    assert_dev_target(server)
    merged = _merge_config(overrides)
    _assert_config_ports_safe(merged)
    # In secure mode the server is TLS-only (no plain-HTTP listener), so the
    # readiness probe must use https. The base config (restored in finally) is
    # open, so its probe stays plain-http.
    secure = (merged.get("auth") or {}).get("mode") == "secure"
    https_port = merged.get("https_port")
    temp = _write_config(merged, DEV_CONFIG_TMP)
    try:
        _make_restart(temp)
        _wait_reachable(server, ready_timeout, secure=secure, https_port=https_port)
        yield server
    finally:
        try:
            _make_restart(DEV_BASE_CONFIG)  # restore, base config pinned
            _wait_reachable(server, ready_timeout)
        finally:
            try:
                temp.unlink()
            except OSError:
                pass


def _terminate(proc: subprocess.Popen) -> None:
    """Best-effort kill of `proc` and its process group (it was started
    with `start_new_session=True`)."""
    for killer in (
        lambda: os.killpg(os.getpgid(proc.pid), signal.SIGKILL),
        proc.kill,
    ):
        try:
            killer()
        except (ProcessLookupError, PermissionError, OSError):
            pass


def run_preflight(overrides: dict, *, timeout: float = 20.0) -> PreflightResult:
    """Launch `bin/shed-server serve` against a temp config and capture it.

    The primitive Phase 6's public-exposure preflight needs: start the
    server with a given config and observe whether it *refuses to start*
    (non-zero exit + a diagnostic on stderr) rather than binding. Runs in
    its own process group so a server that DID come up can be killed
    cleanly on timeout, and the merged config's ports are validated so a
    throwaway server can never bind a production port.
    """
    if not SHED_SERVER_BIN.exists():
        raise AssertionError(f"{SHED_SERVER_BIN} not built; run 'make build' first")
    merged = _merge_config(overrides)
    _assert_config_ports_safe(merged)
    temp = _write_config(merged, PREFLIGHT_CONFIG_TMP)
    proc: Optional[subprocess.Popen] = None
    try:
        proc = subprocess.Popen(
            [str(SHED_SERVER_BIN), "serve", "--config", str(temp)],
            cwd=REPO_ROOT,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            text=True,
            start_new_session=True,
            env=os.environ.copy(),
        )
        try:
            out, err = proc.communicate(timeout=timeout)
            return PreflightResult(proc.returncode, out, err)
        except subprocess.TimeoutExpired:
            # Still running -> it did NOT refuse to start. Kill + drain
            # with a bounded wait so a wedged child can't hang the suite.
            _terminate(proc)
            try:
                out, err = proc.communicate(timeout=5)
            except subprocess.TimeoutExpired:
                out, err = "", ""
            return PreflightResult(None, out, err)
    finally:
        if proc is not None and proc.poll() is None:
            _terminate(proc)
        try:
            temp.unlink()
        except OSError:
            pass


def bootstrap_mint(server: str, scope: str, client_kind: str = "cli", *, timeout: float = 25.0) -> str:
    """Mint a scoped HTTP token over the reserved `_bootstrap` SSH channel,
    exactly as `shed server add` does (cmd/shed/bootstrap.go): `ssh -T` as
    `_bootstrap@host` against the pinned known_hosts (BatchMode, strict), run
    "<scope> <client_kind>", and parse the JSON bundle for its `token`.

    Dev-only — `assert_dev_target` gates the target so this can only ever reach a
    `*-dev` server on offset ports. The local SSH key (default identity) must be
    in the server's allowlist for the mint to succeed; the server must be in
    `auth.mode: secure` (the bootstrap channel requires `auth.ssh.mode: enforce`).
    """
    entry = assert_dev_target(server)
    host = entry.get("host", "localhost")
    ssh_port = int(entry["ssh_port"])
    args = [
        "ssh", "-T", "-p", str(ssh_port),
        "-o", "BatchMode=yes",
        "-o", f"UserKnownHostsFile={KNOWN_HOSTS_PATH}",
        "-o", "StrictHostKeyChecking=yes",
        f"_bootstrap@{host}", scope, client_kind,
    ]
    r = subprocess.run(args, capture_output=True, text=True, timeout=timeout)
    if r.returncode != 0:
        raise AssertionError(
            f"bootstrap mint (scope={scope!r}) failed (exit {r.returncode}): "
            f"stderr={r.stderr[-1000:]!r}"
        )
    try:
        bundle = json.loads(r.stdout)
    except json.JSONDecodeError as e:
        raise AssertionError(f"bootstrap returned invalid JSON: {e}; stdout={r.stdout[-500:]!r}")
    token = bundle.get("token", "")
    assert token, f"bootstrap (scope={scope!r}) returned no token: {bundle!r}"
    return token
