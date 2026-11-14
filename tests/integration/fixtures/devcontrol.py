"""Dev-server control primitives for config-mutating integration tests.

The `LocalServer` / `RemoteServer` fixtures (`server.py`) drive a
*running, static-config* shed-server via `shed -s <name>`. Several
auth/transport tests need more than that:

  - Set a server config, restart the dev server, assert the new
    behavior, then restore (bind config, `auth.ssh` mode, TLS names,
    open mode on loopback).
  - Assert the server *refuses to start* with a given config (removed
    keys and modes, a wide-open bind) — a shape the running-server
    fixtures cannot express at all.
  - Obtain a client certificate of a chosen scope over the `_bootstrap`
    SSH channel, the way every client enrols, to drive the HTTPS API
    directly.

Every primitive is **hard-scoped to the `*-dev` servers on offset ports** so
an autonomous run can never touch production. There are two independent
safety gates:

  - `assert_dev_target(name)`  — the restart *target* must be a `*-dev`
    entry on offset ports (never the brew/deb production `8080/2222`).
  - `_assert_config_ports_safe()` — the *config* a server is (re)started
    with must not name the production ports either, so a stray override
    like `{"http_port": 8080}` can't bind prod even if prod is down.

The primitives:

  - `dev_config(overrides, server)` — context manager: write a temp
    config merged from the committed dev-parallel base, restart the VZ
    dev server against it, wait until it is ready, yield, then **always**
    restore the base config (pinned explicitly) and wait for that too.
    Never edits the committed `configs/server.dev-parallel.*.yaml`.
  - `run_preflight(overrides)` — launch `bin/shed-server serve` as a
    throwaway subprocess against a merged temp config, with a timeout,
    and return a `PreflightResult` so a test can assert a non-zero exit
    + stderr (expected-startup-failure).
  - `bootstrap_cert(server, scope, directory)` — a fresh P-256 key and
    CSR, sent over `_bootstrap` exactly as `shed server add` does; returns
    the issued certificate, its key and the server's TLS pin.
  - `api_info(server, config, directory)` — `GET /api/info` from the dev
    server as the config it was started with serves it: plain HTTP in
    open mode, and in mtls mode over the pinned HTTPS listener with a
    freshly bootstrapped certificate.

Temp configs live under `~/.shed/dev/`, never the committed configs.

FC (remote) config mutation is intentionally out of scope here: config
mutation integration is VZ-only, with FC covered by Go unit tests plus a
manual pass. `bootstrap_cert` reaches either dev server.

**Auth modes.** shed-server has two: `mtls`, which both committed dev
configs run, and `open`, which binds loopback only. `token`/`secure` and
`allow_insecure_exposure` fail config load. So the dev server is always
mtls unless a test asks `dev_config()` for `{"auth": {"mode": "open"}}`,
which on the Mac dev server (loopback) is a valid scenario: a plain-HTTP
listener with no client authentication.

**Readiness.** `dev_config()` waits on two things after each restart.
First `scripts/dev-server-probe.sh`, the same probe `make dev-server-up`
polls: the readiness line in this run's log, the process alive, every
logged listener accepting a TCP connection, and no listener error a
moment later. It needs no credential, so it works in every mode. Then one
real API request (`api_info`), so a server that listens but cannot serve
fails here: plain HTTP in open mode, and in mtls mode a request that
presents a certificate the server issued over `_bootstrap` a moment
earlier (an mtls listener refuses a request without one, by design).
"""

from __future__ import annotations

import base64
import copy
import json
import os
import signal
import ssl
import subprocess
import tempfile
import time
import urllib.error
import urllib.request
from contextlib import contextmanager
from dataclasses import dataclass
from pathlib import Path
from typing import Iterator, Optional
from urllib.parse import urlparse

import pytest
import yaml

from .server import resolve_server_entry
from .tlsclient import pinned_request

# fixtures/ -> integration/ -> tests/ -> repo root
REPO_ROOT = Path(__file__).resolve().parents[3]
DEV_BASE_CONFIG = REPO_ROOT / "configs" / "server.dev-parallel.mac.yaml"
SHED_SERVER_BIN = REPO_ROOT / "bin" / "shed-server"
PROBE_SCRIPT = REPO_ROOT / "scripts" / "dev-server-probe.sh"
DEV_SCRATCH_DIR = Path.home() / ".shed" / "dev"
DEV_CONFIG_TMP = DEV_SCRATCH_DIR / "test-dev-config.yaml"
PREFLIGHT_CONFIG_TMP = DEV_SCRATCH_DIR / "test-preflight-config.yaml"
# The Mac dev server's log and pid file (the Makefile's DEV_LOG_PATH and
# DEV_PID_PATH); dev-server-up empties the log before each launch.
DEV_LOG_PATH = DEV_SCRATCH_DIR / "server.log"
DEV_PID_PATH = DEV_SCRATCH_DIR / "server.pid"
# The pinned known_hosts the CLI bootstrap reads (config.GetKnownHostsPath()).
KNOWN_HOSTS_PATH = Path.home() / ".shed" / "known_hosts"

# The brew/deb production ports. A target/config is only safe if it is
# NOT on these — the structural guarantee of a parallel-dev server (see
# configs/server.dev-parallel.mac.yaml: offset 18080/12222).
PROD_HTTP_PORT = 8080
PROD_SSH_PORT = 2222

# The override that turns the Mac dev server into an open-mode server: the
# committed base binds loopback, keeps http_port 18080 for exactly this, and
# its https_port must go (an open server serves plain HTTP only). The base's
# auth.ssh key sources stay: an open server simply does not enforce them.
OPEN_MODE_OVERRIDES = {"https_port": 0, "auth": {"mode": "open"}}


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


@dataclass
class BootstrapCert:
    """A client certificate issued over `_bootstrap`, on disk.

    `pin` is the server's TLS certificate fingerprint (`sha256:<hex>`) as the
    bundle reported it; `https_port` the port the bundle named.
    """

    cert_path: Path
    key_path: Path
    pin: str
    https_port: int
    scope: str
    bundle: dict

    @property
    def pair(self) -> tuple[str, str]:
        return str(self.cert_path), str(self.key_path)


def _ports(d: dict) -> tuple[int, int]:
    """Extract (http_port, ssh_port) from a config/entry dict, defaulting
    missing/blank values to 0 (which the safety checks treat as invalid).

    A CLIENT ENTRY for an mtls server has no `http_port` at all: mtls runs no
    plain-HTTP listener, so SSH-first `shed server add` records only
    `api_url`. The port that matters for the safety checks — "this is not the
    production server" — is then the one in `api_url`, so fall back to it.
    Server CONFIG dicts still carry `http_port` and are unaffected.
    """
    http_port = int(d.get("http_port", 0) or 0)
    if http_port <= 0:
        # `shed --json server list` reports https_port/endpoint; the raw
        # config file uses api_url. Accept whichever is present.
        http_port = int(d.get("https_port", 0) or 0)
    if http_port <= 0:
        url = str(d.get("api_url", "") or d.get("endpoint", "") or "")
        if url:
            parsed = urlparse(url)
            if parsed.port:
                http_port = int(parsed.port)
    return http_port, int(d.get("ssh_port", 0) or 0)


def assert_dev_target(name: str) -> dict:
    """Raise unless `name` is a parallel-dev server safe to reconfigure.

    The executable form of the suite's safety discipline: every
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


def config_auth_mode(config: dict) -> str:
    """A SERVER CONFIG dict's effective `auth.mode`: `open` when unset, else
    the value as written (`mtls`; anything else does not load)."""
    return str((config.get("auth") or {}).get("mode") or "open")


def _config_enforced(config: dict) -> bool:
    """Whether a SERVER CONFIG dict runs an enforced mode (no plain-HTTP
    listener) — the Python mirror of the Go loader's `AuthEnforced()`
    predicate (`internal/config/server.go`), which is true for `mtls` only.
    Any value but `open` counts here, so an override naming a mode the
    server refuses is never mistaken for an open server."""
    return config_auth_mode(config) != "open"


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
    replaces wholesale. Nested merge is needed so a test can override
    just `auth.ssh.mode` or `vz.default_image` without clobbering
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


def _restart_argv(dev_config: Path) -> list[str]:
    """The `make` command `_make_restart` runs (split out so the unit tests
    can pin it without a server)."""
    return ["make", "dev-server-restart", f"DEV_CONFIG={dev_config}"]


def _make_restart(dev_config: Path) -> None:
    """`make dev-server-restart DEV_CONFIG=<path>` — always with an explicit
    config, so the restart never inherits a stale/foreign value from a parent
    `make`/`MAKEFLAGS`. The config runs as written (its own `auth:` block).

    Command-line variable assignments win over the Makefile's `:=`/`?=`
    defaults. `dev-server-down` keys off the PID file, so it stops whatever
    is running regardless of config. (The restart drags the Makefile's
    `build` prerequisite along; the Go build cache makes that cheap and
    the VM/server spin-up dominates the latency, so it's left as-is.)
    """
    r = subprocess.run(
        _restart_argv(dev_config),
        cwd=REPO_ROOT, capture_output=True, text=True, timeout=300,
    )
    if r.returncode != 0:
        raise AssertionError(
            f"dev-server-restart failed (exit {r.returncode}): "
            f"stdout={r.stdout[-2000:]!r} stderr={r.stderr[-2000:]!r}"
        )


def _probe_once(log: Path, pid_path: Path) -> tuple[int, str]:
    """One run of `scripts/dev-server-probe.sh` against the dev server's log
    and pid file: (exit status, its one-line verdict, or its stderr when it
    printed no verdict). 0 ready, 1 not yet, 2 failed; anything else is a
    broken probe."""
    try:
        pid = pid_path.read_text(encoding="utf-8").strip()
    except OSError:
        pid = ""
    r = subprocess.run(
        ["bash", str(PROBE_SCRIPT), str(log), pid],
        capture_output=True, text=True, timeout=60,
    )
    return r.returncode, r.stdout.strip() or r.stderr.strip()


def _wait_probe(timeout: float, *, log: Path = DEV_LOG_PATH, pid_path: Path = DEV_PID_PATH) -> str:
    """Poll the readiness probe until it says ready; raise when it says the
    server failed or the deadline passes. Mode-independent: the probe only
    reads the log and opens TCP connections. Only status 1 means "not yet":
    any status the probe does not define (64 for arguments it refuses, 127
    for a missing script) raises at once as a broken check, never as a slow
    start."""
    deadline = time.monotonic() + timeout
    rc, verdict = _probe_once(log, pid_path)
    while rc == 1 and time.monotonic() < deadline:
        time.sleep(0.5)
        rc, verdict = _probe_once(log, pid_path)
    if rc == 0:
        return verdict
    try:
        tail = "".join(log.read_text(encoding="utf-8", errors="replace").splitlines(keepends=True)[-20:])
    except OSError:
        tail = ""
    if rc == 1:
        why = f"not ready within {timeout:.0f}s"
    elif rc == 2:
        why = "failed"
    else:
        why = f"failed: the probe exited with unexpected status {rc}"
    raise AssertionError(f"dev server not ready ({why}): {verdict}; last lines of {log}:\n{tail}")


def bootstrap_cert(
    server: str,
    scope: str,
    directory: Path,
    client_kind: str = "cli",
    *,
    timeout: float = 25.0,
) -> BootstrapCert:
    """Enrol a client certificate of `scope` over the reserved `_bootstrap`
    SSH channel, exactly as `shed server add` does (sdk/bootstrap): a fresh
    P-256 key and a CSR for it, then `ssh -T _bootstrap@host` against the
    pinned known_hosts (BatchMode, strict) running
    "<scope> <client_kind> csr=<base64 DER>", and the JSON bundle parsed for
    its `client_cert` and `tls_cert_fingerprint`. The key, CSR and
    certificate are written under `directory` (a pytest tmp_path).

    Dev-only — `assert_dev_target` gates the target so this can only ever
    reach a `*-dev` server on offset ports. The local SSH key (the default
    identity, or the agent's) must be in the server's allowlist, as it is for
    every enrolled dev entry; the server must be mtls (an open server answers
    every bootstrap with its refusal). Needs the `openssl` CLI.
    """
    entry = assert_dev_target(server)
    host = entry.get("host", "localhost")
    ssh_port = int(entry["ssh_port"])
    directory.mkdir(parents=True, exist_ok=True)
    key_path = directory / f"{scope}.key"
    csr_path = directory / f"{scope}.csr.der"
    cert_path = directory / f"{scope}.pem"
    for argv in (
        ["openssl", "ecparam", "-name", "prime256v1", "-genkey", "-noout", "-out", str(key_path)],
        ["openssl", "req", "-new", "-key", str(key_path), "-subj", "/CN=itest",
         "-outform", "DER", "-out", str(csr_path)],
    ):
        r = subprocess.run(argv, capture_output=True, text=True, timeout=30)
        if r.returncode != 0:
            raise AssertionError(f"{argv[0]} {argv[1]} failed: {r.stderr[-500:]!r}")
    key_path.chmod(0o600)
    csr = base64.b64encode(csr_path.read_bytes()).decode("ascii")
    args = [
        "ssh", "-T", "-p", str(ssh_port),
        "-o", "BatchMode=yes",
        "-o", f"UserKnownHostsFile={KNOWN_HOSTS_PATH}",
        "-o", "StrictHostKeyChecking=yes",
        f"_bootstrap@{host}", scope, client_kind, f"csr={csr}",
    ]
    r = subprocess.run(args, capture_output=True, text=True, timeout=timeout)
    if r.returncode != 0:
        raise AssertionError(
            f"bootstrap (scope={scope!r}) failed (exit {r.returncode}): "
            f"stderr={r.stderr[-1000:]!r}"
        )
    try:
        bundle = json.loads(r.stdout)
    except json.JSONDecodeError as e:
        raise AssertionError(f"bootstrap returned invalid JSON: {e}; stdout={r.stdout[-500:]!r}")
    assert bundle.get("auth_mode") == "mtls", f"bootstrap (scope={scope!r}) is not an mtls bundle: {bundle!r}"
    pem = bundle.get("client_cert", "")
    pin = bundle.get("tls_cert_fingerprint", "")
    assert pem and pin, f"bootstrap (scope={scope!r}) returned no certificate or pin: {bundle!r}"
    cert_path.write_text(pem, encoding="utf-8")
    return BootstrapCert(
        cert_path=cert_path, key_path=key_path, pin=pin,
        https_port=int(bundle.get("https_port") or 0), scope=scope, bundle=bundle,
    )


def api_info(server: str, config: dict, directory: Path) -> dict:
    """`GET /api/info` from the dev server, as `config` (the SERVER CONFIG it
    was started with) serves it — never derived from the CLIENT ENTRY, which
    names only the mtls endpoint.

    Open mode: plain HTTP on `http_port`. mtls: the pinned HTTPS listener on
    `https_port`, presenting a control certificate bootstrapped into
    `directory` a moment earlier (an mtls listener refuses a request without
    one, at the TLS layer)."""
    entry = resolve_server_entry(server)
    if entry is None:
        raise AssertionError(f"dev server {server!r} not registered in ~/.shed/config.yaml")
    host = entry.get("host", "localhost")
    if not _config_enforced(config):
        port = int(config.get("http_port") or 8080)
        with urllib.request.urlopen(f"http://{host}:{port}/api/info", timeout=10) as resp:
            return json.loads(resp.read().decode("utf-8"))
    cert = bootstrap_cert(server, "control", directory)
    port = int(config.get("https_port") or cert.https_port or 8443)
    status, body = pinned_request(host, port, "/api/info", cert.pin, cert.pair)
    if status != 200:
        raise AssertionError(f"GET /api/info over mtls: status {status}, body {body!r}")
    return body or {}


def _wait_ready(server: str, timeout: float, *, config: dict) -> None:
    """Wait until the restarted dev server is serving `config`.

    `make dev-server-restart` has already polled the probe for 15 s, but it
    only warns when that runs out, so poll it again here up to `timeout`
    (it fails at once on a server that died). Then one real `GET /api/info`
    in the config's own mode (`api_info`), retried briefly, so a server that
    accepts connections but cannot serve them fails here and not in the
    test."""
    _wait_probe(timeout)
    deadline = time.monotonic() + 30
    while True:
        try:
            with tempfile.TemporaryDirectory() as scratch:
                api_info(server, config, Path(scratch))
            return
        except (AssertionError, urllib.error.URLError, OSError, ssl.SSLError) as e:
            if time.monotonic() >= deadline:
                raise AssertionError(f"dev server {server!r} is listening but did not serve /api/info: {e}")
            time.sleep(1)


@contextmanager
def dev_config(
    overrides: dict,
    server: str,
    *,
    ready_timeout: float = 90.0,
) -> Iterator[str]:
    """Run the VZ dev server with `overrides` merged onto its base config.

    Restarts the dev server against a temp config, waits for it to be
    ready (`_wait_ready`), yields the server name, then **always** restores
    the committed base config on exit (pinned explicitly, even if the test
    body raises) and waits for that too. Never edits the committed
    dev-parallel config.

    Guarded by `assert_dev_target` (the target) and
    `_assert_config_ports_safe` (the merged config's ports), so it can
    only ever restart a `*-dev` server on offset ports. Pass `server`
    from the `vz_server_dev` fixture's `.name`. Works whatever mode the
    override names: the base is mtls, and `OPEN_MODE_OVERRIDES` gives an
    open server on loopback.
    """
    assert_dev_target(server)
    merged = _merge_config(overrides or {})
    _assert_config_ports_safe(merged)
    temp = _write_config(merged, DEV_CONFIG_TMP)
    try:
        _make_restart(temp)
        # The merged config (not the client entry) says which ports/mode the
        # restart just bound.
        _wait_ready(server, ready_timeout, config=merged)
        yield server
    finally:
        try:
            # Restore, base config pinned.
            _make_restart(DEV_BASE_CONFIG)
            _wait_ready(server, ready_timeout, config=_merge_config({}))
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

    Start the server with a given config and observe whether it *refuses to
    start* (non-zero exit + a diagnostic on stderr) rather than binding. Runs
    in its own process group so a server that DID come up can be killed
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


# The identity copied into a throwaway $HOME by `isolated_home_with_identity`.
# It must already be allowlisted by the dev server's `auth.ssh` (today
# `github_users` in both committed configs/server.dev-parallel.*.yaml); nothing
# here sets that allowlisting up.
_REAL_SSH_DIR = Path.home() / ".ssh"
_DEFAULT_IDENTITY = "id_ed25519"


@contextmanager
def isolated_home_with_identity() -> Iterator[str]:
    """A throwaway `$HOME` carrying a copy of the developer's real default SSH
    identity, so system `ssh`'s default identity-file resolution (which honors
    the `HOME` env var for `~/.ssh/id_ed25519`) finds a usable key without
    depending on an ssh-agent being loaded. `shed server add`'s SSH-first
    bootstrap shells out to the real `ssh` binary (`sdk/bootstrap`), so this
    is what lets an add run against a config- and creds-isolated `$HOME`
    (never the developer's real `~/.shed/config.yaml`) while still
    authenticating as a key the dev server allowlists.

    Skips the calling test if no default identity exists to copy."""
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
