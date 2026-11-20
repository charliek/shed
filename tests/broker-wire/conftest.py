"""Fixtures for the hermetic broker **wire** harness.

The harness drives the desktop app's embedded credential broker, hosted in a
test-only process: `crates/shed-broker-harness` (shed-app's `broker` feature, the
app's own loader and bridge, minus the app's Coordinator). The session `binaries`
fixture builds it (`cargo build -p shed-broker-harness`, with `~/.cargo/bin` on PATH)
once and returns its path. `daemon` (the name kept so the cells read as before) is a
context manager that launches it with a config, waits for its control socket, yields a
handle that reads its status over that socket, and on exit sends SIGTERM and asserts
exit 0, the `harness drained` line in its log (the broker drained every server group
before it exited) and the control socket unlinked.

`differential` asserts the broker's wire-visible output against a committed golden
under `goldens/` (see the "Goldens" block below `_shutdown`). Those goldens were
recorded (73755c0-era) from a run where the Go and Rust daemons produced IDENTICAL
values, and have since held for the Rust daemon and now for the app's broker; the
values only the retired daemon produced (its desktop socket's frames, its `status`
text, its start-up key listing) were removed from them, not re-recorded. The `impl`
seam and the fixture's `scenario(impl)` signature survive because the whole harness is
written in terms of it; `"rust"` is the only impl.

Hermetic by construction: every test gets a fresh control-socket dir, `$HOME` (so
no real `~/.ssh` / `~/.shed` is read) and state dir, `SSH_AUTH_SOCK` is
stripped (the broker falls back to an empty local-keys backend), and the only network
is the synthetic bus on loopback.
"""

from __future__ import annotations

import contextlib
import json
import os
import re
import shutil
import signal
import subprocess
import tempfile
import time
from pathlib import Path

import pytest

import control_client
from normalize import canonical

# tests/broker-wire/conftest.py -> tests -> repo root.
REPO_ROOT = Path(__file__).resolve().parents[2]

# Committed, throwaway, passphrase-less OpenSSH ed25519 test key (see
# fixtures/test_ed25519). The `daemon` fixture writes it to a daemon's isolated
# `<HOME>/.ssh/id_ed25519` so the `LocalEd25519Backend` loads a FIXED key — ed25519 is
# deterministic (RFC 8032), so the same challenge always signs to the same 64 bytes,
# which is why the sign blob is golden-pinned UNMASKED. (Its Go twin loaded the same
# committed key, which is how the recorded blob is a two-implementation agreement.)
FIXTURES_DIR = Path(__file__).resolve().parent / "fixtures"
TEST_ED25519_KEY = FIXTURES_DIR / "test_ed25519"

# The harness's control socket, in the per-test short socket dir.
CONTROL_SOCK_NAME = "control.sock"

# What the harness writes to its log once the broker has drained on SIGTERM
# (`crates/shed-broker-harness/src/main.rs`).
DRAINED_LINE = "INFO  harness drained"

# The "watch none" launch config: ssh delegated to shed-desktop, aws approve-all,
# docker deny-all, logging on, discovery watching zero servers. Chosen so the daemon
# reports an EMPTY `servers` list while still exercising a non-trivial policy/gate
# mix. Written in BLOCK style on purpose: the Rust `yaml_lite` parser handles block maps only,
# not inline flow maps like `ssh: { approval: { policy: X } }` — see README "Known
# contract gaps". `{audit_log}` / `{source}` are filled in with per-test temp paths.
WATCH_NONE_CONFIG = """\
ssh:
  approval:
    policy: shed-desktop
aws:
  approval:
    policy: approve-all
docker:
  approval:
    policy: deny-all
logging:
  enabled: true
  path: {audit_log}
discovery:
  servers: []
  watch: off
  source: {source}
"""

# The single-server (NO `discovery:` block) launch config for the surface-B bus
# tests. With no `discovery:` key the daemon runs in single-server mode
# (`is_single_server()`), connects to `server:`, and subscribes to `ssh-agent`.
# `server:` is filled by `single_server_config(bus_url)` (via a plain string replace,
# leaving `{audit_log}` for the `daemon` fixture's `.format()` — an unused `{source}`
# kwarg there is harmless). BLOCK style on purpose (the Rust `yaml_lite` parser is
# block-only; see WATCH_NONE_CONFIG / README "Known contract gaps").
# `ssh.approval.policy: approve-all` is a valid ssh policy — but ping is NOT gated, so
# it's irrelevant to the pong; it just exercises a non-empty gate. No `aws:`/`docker:`
# blocks → those backends stay nil → the daemon subscribes to ssh-agent only (plus the
# always-on egress GET, which the synthetic bus 501s).
SINGLE_SERVER_CONFIG = """\
server: {server}
ssh:
  approval:
    policy: approve-all
logging:
  enabled: true
  path: {audit_log}
"""


def _single_server_config(bus_url: str) -> str:
    """Fill `server:` with `bus_url`, leaving `{audit_log}` for the `daemon`
    fixture. Uses `str.replace` (not `.format`) so the still-unfilled `{audit_log}`
    placeholder survives to the fixture's own `.format(audit_log=..., source=...)`."""
    return SINGLE_SERVER_CONFIG.replace("{server}", bus_url)


# The single-server config for the gated cross-surface **sign** differential
# (`test_bus_sign_gated.py`). Same single-server shape as SINGLE_SERVER_CONFIG (no
# `discovery:` block → single-server mode, subscribes to `ssh-agent`), but with
# `ssh.mode: local-keys` (so the committed ed25519 key the `daemon` fixture installs
# is loaded) and `ssh.approval.policy: shed-desktop` (so a `sign` is DELEGATED to the
# app through the bridge's approval gate, which the harness's control socket stands in
# for; the resulting audit fans out to it as an `event`). BLOCK style on purpose (the Rust `yaml_lite` parser is block-only; see
# WATCH_NONE_CONFIG / README "Known contract gaps"). `{server}` is filled by
# `sign_config(bus_url)`; `{audit_log}` survives to the `daemon` fixture's `.format`.
SIGN_CONFIG = """\
server: {server}
ssh:
  mode: local-keys
  approval:
    policy: shed-desktop
logging:
  enabled: true
  path: {audit_log}
"""


def _sign_config(bus_url: str) -> str:
    """Fill `server:` with `bus_url`, leaving `{audit_log}` for the `daemon` fixture
    (str.replace, same reason as `_single_server_config`)."""
    return SIGN_CONFIG.replace("{server}", bus_url)


# The discovery `watch: poll` launch config (short poll interval so the convergence
# cell resolves within a test deadline). The daemon builds a supervisor and
# reconciles the desired server set from `{source}` (a `~/.shed/config.yaml`-style
# `servers:` doc) every poll tick — so an appearance/change of `{source}` converges
# `servers[]`. The live cell drives POLL for determinism (production's
# default is event-driven `notify`; that path is unit-covered). BLOCK style (the Rust
# reader is block-only). `{source}`/`{audit_log}` are filled by the `daemon` fixture.
DISCOVERY_POLL_CONFIG = """\
ssh:
  approval:
    policy: approve-all
logging:
  enabled: true
  path: {audit_log}
discovery:
  watch: poll
  poll_interval: 300ms
  source: {source}
"""

# The discovery `watch: off` launch config: reconciles ONCE at startup, then never
# reloads — so a later `{source}` change is NOT picked up (the deterministic half of the
# off/poll convergence cell).
DISCOVERY_OFF_CONFIG = """\
ssh:
  approval:
    policy: approve-all
logging:
  enabled: true
  path: {audit_log}
discovery:
  watch: off
  source: {source}
"""


def discovery_source_doc(servers: dict) -> str:
    """Render a `~/.shed/config.yaml`-style discovery source doc from
    `{name: {field: value, ...}, ...}` (block style — both readers agree). An empty dict
    renders a bare `servers:` (an empty set → the daemon watches nothing)."""
    lines = ["servers:"]
    for name, fields in servers.items():
        lines.append(f"  {name}:")
        for key, value in fields.items():
            lines.append(f"    {key}: {value}")
    return "\n".join(lines) + "\n"


def _clean_env(home, path_prepend=None, ssh_auth_sock=None, state_dir=None) -> dict:
    """A hermetic environment: real PATH (so system tools resolve) but an isolated
    HOME + state dir, and no ambient ssh-agent. `path_prepend` (a dir)
    goes on the FRONT of PATH so a shim binary (e.g. a fake `ssh`) is resolved before
    the real one — the broker uses `look_ssh` on PATH.

    The state dir (`SHED_BROKER_STATE_DIR`: the machine identity file and the
    credential store) defaults to `<home>/state`. It is set explicitly because an
    isolated HOME alone does not isolate it on Linux: the broker prefers
    `$XDG_DATA_HOME`, so a developer with that exported would otherwise have the
    broker write `machine.json` into their real data dir. Pass `state_dir` to share
    one across several harness runs (the machine-identity restart cell).

    `SSH_AUTH_SOCK` is stripped by default (so the local-keys backend is the
    auto-detect result); pass `ssh_auth_sock` to point the agent-forward backend at a
    fake agent (the ssh-backend tests) — it is set AFTER the strip so it wins."""
    env = dict(os.environ)
    env["HOME"] = str(home)
    env["SHED_BROKER_STATE_DIR"] = str(
        state_dir if state_dir is not None else Path(home) / "state"
    )
    env.pop("SSH_AUTH_SOCK", None)
    env.pop("XDG_RUNTIME_DIR", None)
    # Strip DOCKER_CONFIG so `find_docker_config` resolves the isolated
    # `<HOME>/.docker/config.json` (a dev-Mac DOCKER_CONFIG would otherwise leak a
    # real Docker config into a golden-checked cell — non-hermetic).
    env.pop("DOCKER_CONFIG", None)
    if path_prepend is not None:
        env["PATH"] = str(path_prepend) + os.pathsep + env.get("PATH", "")
    if ssh_auth_sock is not None:
        env["SSH_AUTH_SOCK"] = str(ssh_auth_sock)
    return env


def harness_argv(binary: str, config_path, log_path, control_path) -> list:
    """The harness command line (`shed-broker-harness --config --log-file --control`)."""
    return [
        binary,
        "--config",
        str(config_path),
        "--log-file",
        str(log_path),
        "--control",
        str(control_path),
    ]


def _build(cmd, cwd, env=None) -> None:
    proc = subprocess.run(cmd, cwd=cwd, env=env, capture_output=True, text=True)
    if proc.returncode != 0:
        raise AssertionError(
            f"build failed: {' '.join(cmd)} (cwd={cwd})\n"
            f"--- stdout ---\n{proc.stdout}\n--- stderr ---\n{proc.stderr}"
        )


@pytest.fixture(scope="session")
def binaries() -> dict:
    """Build the harness binary once and return `{"rust": path}`.

    Still a dict keyed by impl — every `daemon` call site is written as
    `binaries[impl]`, and the `impl` seam is what makes a golden's provenance
    readable ("the value both implementations produced"). `"rust"` is the only key
    since the Go daemon's retirement. The build is `-p shed-broker-harness`: the
    crate is not a default member, and building it alone keeps shed-app's `broker`
    feature out of every other crate's build."""
    cargo_env = dict(os.environ)
    cargo_env["PATH"] = (
        str(Path.home() / ".cargo" / "bin") + os.pathsep + cargo_env.get("PATH", "")
    )
    _build(
        ["cargo", "build", "-p", "shed-broker-harness"],
        cwd=REPO_ROOT / "crates",
        env=cargo_env,
    )
    # Honor CARGO_TARGET_DIR (standard cargo redirection — used e.g. by the
    # rehab's Linux loop container to keep linux artifacts off the bind mount).
    # Cargo resolves a RELATIVE value against ITS cwd (crates/, where we invoke
    # it above) — not pytest's cwd — so anchor relative values there too.
    env_target = os.environ.get("CARGO_TARGET_DIR")
    if env_target:
        target_dir = Path(env_target)
        if not target_dir.is_absolute():
            target_dir = REPO_ROOT / "crates" / target_dir
    else:
        target_dir = REPO_ROOT / "crates" / "target"
    rust_bin = target_dir / "debug" / "shed-broker-harness"

    assert rust_bin.exists(), f"rust binary missing: {rust_bin}"
    return {"rust": str(rust_bin)}


class DaemonHandle:
    """A running harness under test: read its status over the control socket, inspect
    its files."""

    def __init__(
        self,
        impl,
        binary,
        proc,
        socket_dir,
        home,
        config_path,
        log_path,
        audit_log_path,
        ssh_argv_file=None,
        docker_transcript_file=None,
        source_path=None,
    ):
        self.impl = impl
        self.binary = binary
        self.proc = proc
        self.home = str(home)
        self.config_path = str(config_path)
        self.log_path = Path(log_path)
        # The discovery `source:` file (a `~/.shed/config.yaml`-style `servers:` doc). A
        # discovery test writes/rewrites it to drive the poll/off convergence differential;
        # `None` for the non-discovery configs.
        self.source_path = Path(source_path) if source_path else None
        # When the harness was launched with a shim `ssh` (the minter tests), the shim
        # appends its argv (one element per line) to this file — the broker's exact
        # `ssh` invocation, captured for the differential's argv comparison.
        self.ssh_argv_file = Path(ssh_argv_file) if ssh_argv_file else None
        # When launched with a fake `docker-credential-testhelper` (the docker helper
        # cells), the helper appends one JSONL record per invocation — `{"argv":[...],
        # "stdin":"<server_url>"}`, argv+stdin ONLY, never PATH/env — captured for the
        # exec-seam transcript diff.
        self.docker_transcript_file = (
            Path(docker_transcript_file) if docker_transcript_file else None
        )
        # The DURABLE audit JSONL (`logging.path`), distinct from the operational log
        # above. Populated by gated ops (the sign flow); a fan-out `event` is sent
        # AFTER the file line is written (`JsonlAuditSink::log`), so a durable line is
        # readable once the control client has seen the matching `event`.
        self.audit_log_path = Path(audit_log_path)
        self.control_sock = Path(socket_dir) / CONTROL_SOCK_NAME

    def status(self) -> dict:
        """The broker's status JSON (`{"op":"status"}` on a connection of its own)."""
        return control_client.request(self.control_sock, {"op": "status"})

    def poll_status(self, predicate, timeout: float = 12.0) -> dict:
        """Poll the status until `predicate(obj)` holds (a deadline poll, never a fixed
        sleep) and return the parsed status object. A failed request is retried, not
        fatal. Raises on timeout with the last snapshot for a readable failure — the
        shared primitive behind the discovery/servers[] convergence cells."""
        deadline = time.monotonic() + timeout
        last = None
        while time.monotonic() < deadline:
            try:
                last = self.status()
            except (OSError, AssertionError, ValueError):
                last = None
            if last is not None and predicate(last):
                return last
            time.sleep(0.05)
        raise AssertionError(
            f"{self.impl}: status never satisfied the predicate within "
            f"{timeout}s; last={last!r}"
        )

    def read_log(self) -> str:
        return _safe_read(self.log_path)

    def read_ssh_argv(self, timeout: float = 5.0) -> list:
        """Poll the shim's argv-capture file until it is non-empty and return the
        daemon's `ssh` argv (one element per line). A deadline poll, not a sleep —
        the shim writes it during the async token.get round-trip."""
        assert self.ssh_argv_file is not None, "daemon was not launched with a shim ssh"
        deadline = time.monotonic() + timeout
        while time.monotonic() < deadline:
            try:
                raw = self.ssh_argv_file.read_text()
            except OSError:
                raw = ""
            lines = raw.split("\n")
            # The shim appends each arg + a trailing newline, so a complete capture ends
            # with an empty final field; require that so we never read a half-written file.
            if len(lines) >= 2 and lines[-1] == "":
                return lines[:-1]
            time.sleep(0.02)
        raise AssertionError(
            f"{self.impl}: shim ssh argv file {self.ssh_argv_file} empty within {timeout}s"
        )

    def _poll_jsonl(self, path: Path, what: str, expect: int, timeout: float) -> list:
        """Poll `path` until it holds `expect` non-empty JSONL lines (a deadline poll,
        never a fixed sleep) and return them parsed. Raises on timeout or on a line
        that isn't valid JSON. Robust against the small window between the triggering
        event and the file flush landing on disk."""
        deadline = time.monotonic() + timeout
        last: list = []
        while time.monotonic() < deadline:
            try:
                raw = path.read_text()
            except OSError:
                raw = ""
            last = [line for line in raw.splitlines() if line.strip()]
            if len(last) >= expect:
                import json as _json

                return [_json.loads(line) for line in last]
            time.sleep(0.02)
        raise AssertionError(
            f"{self.impl}: {what} {path} held {len(last)} line(s), "
            f"expected {expect} within {timeout}s; contents={last!r}"
        )

    def read_docker_transcript(self, expect: int = 1, timeout: float = 5.0) -> list:
        """Poll the fake docker-helper's transcript until it holds `expect` non-empty
        JSONL lines and return them parsed (each `{"argv":[...], "stdin":"..."}`)."""
        assert self.docker_transcript_file is not None, (
            "daemon was not launched with docker_helper_bundle"
        )
        return self._poll_jsonl(self.docker_transcript_file, "docker transcript", expect, timeout)

    def read_audit_jsonl(self, expect: int = 1, timeout: float = 5.0) -> list:
        """Poll the durable audit file (`logging.path`) until it holds `expect`
        non-empty JSONL lines and return them parsed."""
        return self._poll_jsonl(self.audit_log_path, "audit file", expect, timeout)


@pytest.fixture
def daemon(binaries, tmp_path_factory):
    """Return a context-manager factory `make(impl, config_text) -> DaemonHandle`.

    On enter: writes `config_text` (a `.format(audit_log=..., source=...)` template)
    into an isolated dir — or, with `config_text=None`, writes no config at all — then
    launches `shed-broker-harness --config/--log-file/--control` and waits up to 10s
    for the control socket to appear. On exit: SIGTERM, assert exit 0, the
    `harness drained` line in the log and the control socket unlinked.
    """

    @contextlib.contextmanager
    def _make(
        impl,
        config_text,
        install_ssh_key: bool = False,
        install_ssh_keys=None,
        ssh_auth_sock: str | None = None,
        shed_config: str | None = None,
        known_hosts: str | None = None,
        ssh_shim_bundle: str | None = None,
        install_aws_credentials: str | None = None,
        install_docker_config: str | None = None,
        docker_helper_bundle: str | None = None,
        state_dir=None,
    ):
        root = tmp_path_factory.mktemp(f"daemon-{impl}")
        home = root / "home"
        home.mkdir()
        audit_log = root / "audit.log"
        source = root / "nonexistent-discovery.yaml"
        config_path = root / "extensions.yaml"
        log_path = root / "op.log"
        if config_text is not None:
            config = config_text.format(audit_log=audit_log, source=source)
            config_path.write_text(config)

        # Install the committed ed25519 key into this daemon's isolated
        # `<HOME>/.ssh/id_ed25519` (dir 0700, file 0600) BEFORE launch, so a
        # local-keys `sign` finds the SAME key every run (`user_home_dir()` resolves
        # `$HOME`, set by `_clean_env`), which is what makes the golden-pinned sign
        # blob reproducible. `install_ssh_key=True` installs id_ed25519 (unchanged
        # default). The additive `install_ssh_keys` kwarg installs a list of committed
        # fixture STEMS (e.g. "test_ed25519","test_rsa","test_ecdsa") as
        # `<HOME>/.ssh/id_<algo>` — the local-keys backend loads them in
        # STANDARD_KEY_FILES order (ed25519, rsa, ecdsa). A stem `test_<algo>` maps to
        # `id_<algo>`.
        keys_to_install: list[str] = []
        if install_ssh_key:
            keys_to_install.append("test_ed25519")
        if install_ssh_keys:
            keys_to_install.extend(install_ssh_keys)
        if keys_to_install:
            ssh_dir = home / ".ssh"
            ssh_dir.mkdir(mode=0o700, exist_ok=True)
            os.chmod(ssh_dir, 0o700)
            for stem in keys_to_install:
                dst_name = stem.replace("test_", "id_", 1)
                key_dst = ssh_dir / dst_name
                key_dst.write_bytes((FIXTURES_DIR / stem).read_bytes())
                os.chmod(key_dst, 0o600)

        # Install the AWS shared-credentials fixture into this daemon's isolated
        # `<HOME>/.aws/{credentials,config}` BEFORE launch, so a passthrough vend reads
        # the SAME profile every run (`user_home_dir().join(".aws")`, keyed off
        # `$HOME`). The config file is written EMPTY (the shared-config loader merges
        # config + credentials; the credentials file carries the static keys).
        # Hermetic by construction — no
        # AWS_SHARED_CREDENTIALS_FILE env plumbing needed (that route is unit-covered).
        if install_aws_credentials is not None:
            aws_dir = home / ".aws"
            aws_dir.mkdir(exist_ok=True)
            (aws_dir / "credentials").write_text(install_aws_credentials)
            (aws_dir / "config").write_text("")

        # Install the Docker `config.json` fixture into this daemon's isolated
        # `<HOME>/.docker/config.json` BEFORE launch, so a `get`/`list` reads the SAME
        # config every run (`find_docker_config` resolves `$HOME/.docker/config.json`
        # with DOCKER_CONFIG stripped by `_clean_env`). Hermetic by construction — no
        # DOCKER_CONFIG env plumbing. When None (the UNCONFIGURED cell), no file is
        # written, so the default path is absent → the backend denies every registry.
        if install_docker_config is not None:
            docker_dir = home / ".docker"
            docker_dir.mkdir(exist_ok=True)
            (docker_dir / "config.json").write_text(install_docker_config)

        # The minter reads `<HOME>/.shed/{config.yaml,known_hosts}` (the shed CLI
        # config it resolves servers from + the host-key pin). Both impls read the
        # SAME isolated HOME.
        if shed_config is not None or known_hosts is not None:
            shed_dir = home / ".shed"
            shed_dir.mkdir(exist_ok=True)
            if shed_config is not None:
                (shed_dir / "config.yaml").write_text(shed_config)
            if known_hosts is not None:
                (shed_dir / "known_hosts").write_text(known_hosts)

        # Install a shim `ssh` (a PATH-front executable) that appends its argv to a
        # capture file, then prints the fixed bundle — so the daemon mints
        # deterministically over a fake ssh (`look_ssh` resolves it from PATH). See
        # the `fake_seams` self-test.
        ssh_argv_file = None
        path_prepend = None
        if ssh_shim_bundle is not None:
            shim_dir = root / "shim-bin"
            shim_dir.mkdir()
            ssh_argv_file = root / "ssh-argv.txt"
            _write_ssh_shim(shim_dir / "ssh", ssh_argv_file, ssh_shim_bundle)
            path_prepend = shim_dir

        # Install a fake `docker-credential-testhelper` (a python3 script, 0755 + shebang)
        # into a PATH-PREPENDED `helper-bin` dir — `look_helper_path` resolves via PATH
        # (`which`-equivalent) FIRST, so the front-of-PATH shim wins. It captures its
        # argv + stdin (the server_url) to a per-daemon transcript JSONL,
        # then prints the fixed bundle. The two cells are mutually exclusive with the ssh
        # shim (both want `path_prepend`).
        docker_transcript_file = None
        if docker_helper_bundle is not None:
            assert ssh_shim_bundle is None, "ssh shim + docker helper both want path_prepend"
            helper_dir = root / "helper-bin"
            helper_dir.mkdir()
            docker_transcript_file = root / "docker-helper-transcript.jsonl"
            _write_docker_helper(
                helper_dir / "docker-credential-testhelper",
                docker_transcript_file,
                docker_helper_bundle,
            )
            path_prepend = helper_dir

        # The socket dir must be SHORT: an AF_UNIX bind path caps at ~104 bytes
        # (macOS) / ~108 (Linux), and pytest's nested tmp tree blows past that. A
        # shallow mkdtemp under $TMPDIR keeps `<dir>/control.sock` well under the
        # limit. Cleaned up in the finally below.
        socket_dir = Path(tempfile.mkdtemp(prefix="hadiff-"))
        control_sock = socket_dir / CONTROL_SOCK_NAME

        # The harness writes its operational log to --log-file, so stdout/stderr carry
        # nothing worth capturing; DEVNULL avoids leaving un-read pipe file objects
        # (which would trip the suite's warnings-as-errors as ResourceWarnings).
        proc = subprocess.Popen(
            harness_argv(binaries[impl], config_path, log_path, control_sock),
            env=_clean_env(
                home,
                path_prepend=path_prepend,
                ssh_auth_sock=ssh_auth_sock,
                state_dir=state_dir,
            ),
            stdout=subprocess.DEVNULL,
            stderr=subprocess.DEVNULL,
        )

        # Readiness: the control socket exists once the broker has spawned and the
        # harness has bound it.
        deadline = time.monotonic() + 10.0
        while time.monotonic() < deadline:
            if control_sock.exists():
                break
            if proc.poll() is not None:
                shutil.rmtree(socket_dir, ignore_errors=True)
                raise AssertionError(
                    f"{impl} harness exited early rc={proc.returncode}\n"
                    f"oplog={_safe_read(log_path)}"
                )
            time.sleep(0.02)
        else:
            proc.kill()
            proc.wait()
            shutil.rmtree(socket_dir, ignore_errors=True)
            raise AssertionError(
                f"{impl} harness did not bind {control_sock} within 10s\n"
                f"oplog={_safe_read(log_path)}"
            )

        handle = DaemonHandle(
            impl,
            binaries[impl],
            proc,
            socket_dir,
            home,
            config_path,
            log_path,
            audit_log,
            ssh_argv_file=ssh_argv_file,
            docker_transcript_file=docker_transcript_file,
            source_path=source,
        )
        try:
            yield handle
        finally:
            try:
                _shutdown(impl, proc, control_sock, log_path)
            finally:
                shutil.rmtree(socket_dir, ignore_errors=True)

    return _make


def _shutdown(impl, proc, control_sock, log_path) -> None:
    """SIGTERM the harness and assert it drained: exit 0, the `harness drained` line in
    its log (written only after the broker's every server group was drained) and the
    control socket unlinked.

    The harness MUST still be running at teardown: an early exit (a crash after
    serving a request, say) would otherwise false-pass this "clean SIGTERM
    shutdown" check, since `proc.wait()` would just return the prior code."""
    assert proc.poll() is None, (
        f"{impl} harness already exited (rc={proc.returncode}) before SIGTERM — "
        "it must stay up until teardown; an early exit is a bug"
    )
    proc.send_signal(signal.SIGTERM)
    try:
        # The harness waits at most 5s for the drain, then exits 3.
        rc = proc.wait(timeout=10)
    except subprocess.TimeoutExpired:
        proc.kill()
        proc.wait()
        raise AssertionError(f"{impl} harness did not exit within 10s of SIGTERM")
    oplog = _safe_read(log_path)
    assert rc == 0, f"{impl} harness exit={rc} on SIGTERM (want 0)\noplog={oplog}"
    assert DRAINED_LINE in oplog.splitlines(), (
        f"{impl}: no {DRAINED_LINE!r} in the harness log: the broker did not drain "
        f"before exit\noplog={oplog}"
    )
    assert not control_sock.exists(), (
        f"{impl}: control socket not unlinked on shutdown: {control_sock}"
    )


def _write_ssh_shim(path: Path, argv_file: Path, bundle_json: str) -> None:
    """Write an executable `#!/bin/sh` shim `ssh` that records its argv (one element
    per line, so a multi-arg invocation is unambiguous) and prints the fixed bundle to
    stdout, exit 0 — the deterministic seam the daemon mints over. `printf '%s'` (no
    trailing newline) keeps stdout a single JSON object (trailing whitespace is fine)."""
    script = (
        "#!/bin/sh\n"
        f'for a in "$@"; do printf \'%s\\n\' "$a" >> "{argv_file}"; done\n'
        f"printf '%s' '{bundle_json}'\n"
        "exit 0\n"
    )
    path.write_text(script)
    os.chmod(path, 0o755)


def _write_docker_helper(path: Path, transcript_file: Path, bundle_json: str) -> None:
    """Write an executable `#!/usr/bin/env python3` fake `docker-credential-testhelper`
    that (a) reads stdin (the raw `server_url`), (b) appends ONE JSONL record of its
    argv + stdin — argv+stdin ONLY, NEVER the augmented PATH/env (host-dependent;
    APPEND parity is the `augment_path` golden's job) — to `transcript_file`, and (c)
    prints the fixed `bundle_json` to stdout, exit 0. The deterministic exec seam the
    daemon runs over."""
    script = (
        "#!/usr/bin/env python3\n"
        "import sys, json\n"
        "stdin = sys.stdin.read()\n"
        f"with open({str(transcript_file)!r}, 'a') as f:\n"
        "    f.write(json.dumps({'argv': sys.argv[1:], 'stdin': stdin}) + '\\n')\n"
        f"sys.stdout.write({bundle_json!r})\n"
    )
    path.write_text(script)
    os.chmod(path, 0o755)


def _safe_read(path) -> str:
    try:
        return Path(path).read_text()
    except OSError:
        return "(no operational log)"


@pytest.fixture
def watch_none_config() -> str:
    """The block-style 'watch none' launch-config template (see WATCH_NONE_CONFIG)."""
    return WATCH_NONE_CONFIG


@pytest.fixture
def single_server_config():
    """Return `make(bus_url) -> config_text`: the block-style single-server (no
    `discovery:`) launch config with `server:` pointed at the synthetic bus. The
    returned text still carries the `{audit_log}` placeholder the `daemon` fixture
    fills. See SINGLE_SERVER_CONFIG."""
    return _single_server_config


@pytest.fixture
def sign_config():
    """Return `make(bus_url) -> config_text`: the block-style single-server config for
    the gated cross-surface **sign** differential — `ssh.mode: local-keys` +
    `ssh.approval.policy: shed-desktop`, `server:` pointed at the synthetic bus. Pair
    with `daemon(..., install_ssh_key=True)` so the committed ed25519 key is loaded.
    See SIGN_CONFIG."""
    return _sign_config


@pytest.fixture
def discovery_poll_config() -> str:
    """The block-style `watch: poll` discovery launch-config template (see
    DISCOVERY_POLL_CONFIG). The `daemon` fixture fills `{source}`/`{audit_log}`."""
    return DISCOVERY_POLL_CONFIG


@pytest.fixture
def discovery_off_config() -> str:
    """The block-style `watch: off` discovery launch-config template (see
    DISCOVERY_OFF_CONFIG)."""
    return DISCOVERY_OFF_CONFIG


# --- Goldens (plan 006 D1) -------------------------------------------------
#
# Every `differential()` value is pinned to a committed golden file — that is how the
# agreed wire shape survived the Go daemon's retirement. The goldens were recorded
# (73755c0) from a run where BOTH implementations produced the same value, the
# strongest provenance available; the Rust daemon is now asserted against them.
#
# The golden key is the pytest nodeid, sanitized to a safe-but-readable filename —
# so parametrized cases get distinct files for free and the 55 `differential()` call
# sites stay untouched (the check lives in the fixture body).
GOLDENS_DIR = Path(__file__).resolve().parent / "goldens"

# Anything outside this set is collapsed to `_` in a golden key (`::` separators,
# parametrize-id brackets, spaces, ...).
_KEY_UNSAFE = re.compile(r"[^A-Za-z0-9._-]+")

# Session-wide golden bookkeeping (no state survives across pytest runs):
#   claimed  — sanitized key -> the nodeid that claimed it (collision detection)
#   visited  — keys checked/recorded this session (stale-golden detection)
#   expected — nodeids of collected tests that request `differential`
#   enforce_stale — whether the stale check may run at all (see the hooks below)
_GOLDEN_SESSION: dict = {
    "claimed": {},
    "visited": set(),
    "expected": set(),
    "enforce_stale": True,
}


def _update_golden() -> bool:
    """True when the run is in RECORD mode (`UPDATE_GOLDEN=1`). Any non-empty value
    other than `0` records, so `UPDATE_GOLDEN=true` works too."""
    return os.environ.get("UPDATE_GOLDEN", "") not in ("", "0")


def _golden_key(nodeid: str) -> str:
    """Sanitize a pytest nodeid into a golden filename stem: readable (test file +
    test name + params), safe on every filesystem. `test_ssh_backend.py::
    test_ssh_local_sign_rsa[0-ssh-rsa]` -> `test_ssh_backend__test_ssh_local_sign_rsa_0-ssh-rsa`."""
    key = nodeid.replace(".py::", "__")
    key = _KEY_UNSAFE.sub("_", key).strip("_")
    assert key, f"nodeid sanitized to an empty golden key: {nodeid!r}"
    return key


def _claim_golden_key(nodeid: str) -> str:
    """Return the golden key for `nodeid`, failing if a DIFFERENT nodeid already
    claimed it this session. Sanitization is lossy (`[a/b]` and `[a_b]` collapse to
    the same stem), and a silent collision would mean two cells sharing — and
    overwriting — one golden file. Keys are collision-free by construction today;
    this makes that a checked property rather than an assumption."""
    key = _golden_key(nodeid)
    # Claimed case-insensitively: common macOS filesystems are case-insensitive, so
    # two keys differing only in case would share one file there while CI (Linux,
    # case-sensitive) saw two — catch that class at claim time.
    owner = _GOLDEN_SESSION["claimed"].setdefault(key.lower(), nodeid)
    assert owner == nodeid, (
        f"golden key collision: {nodeid!r} and {owner!r} both sanitize to {key!r} "
        "(compared case-insensitively). Rename one of the tests (or its parametrize "
        "id) so the two cells get distinct golden files."
    )
    return key


def _check_golden(nodeid: str, value) -> None:
    """Assert (or, under `UPDATE_GOLDEN=1`, record) the golden for `nodeid`.

    Both sides are compared through one canonical serialization
    (`json.dumps(..., sort_keys=True)`), so key order and pretty-print details can
    never read as a diff, while bool/int stay distinct — a bare parsed-object
    compare would let Python's `1 == True` silently accept a type drift. Record
    mode asserts JSON round-trip fidelity FIRST: a value carrying tuples, bytes,
    sets, non-string keys or NaN/Infinity changes shape (or emits non-JSON) on the
    way to disk and would then never compare equal in assert mode, so it must fail
    loudly here."""
    key = _claim_golden_key(nodeid)
    path = GOLDENS_DIR / f"{key}.json"
    recorded = canonical(value)
    # Visited in BOTH modes: a recorded golden is not stale either.
    _GOLDEN_SESSION["visited"].add(key)

    if _update_golden():
        try:
            roundtrip = json.loads(json.dumps(recorded, allow_nan=False))
        except ValueError:
            roundtrip = None
        assert recorded == roundtrip, (
            f"{nodeid}: differential value does not survive a JSON round-trip — it "
            "carries a non-JSON type (tuple/set/bytes/non-string key/NaN/Infinity). "
            f"Return plain dict/list/str/int/bool/None from the scenario. value={value!r}"
        )
        text = json.dumps(recorded, indent=2, sort_keys=True) + "\n"
        GOLDENS_DIR.mkdir(exist_ok=True)
        # Idempotent by CONTENT: an unchanged golden is not rewritten, so a re-record
        # leaves a clean `git status` (stable mtimes are not promised, nor needed).
        if not path.exists() or path.read_text() != text:
            path.write_text(text)
        return

    assert path.exists(), (
        f"{nodeid}: no golden at {path}. A NEW cell records its first golden via: "
        "UPDATE_GOLDEN=1 uv run pytest — for an EXISTING cell, a missing golden means "
        "the file was deleted, and re-recording would silently bless whatever the "
        "daemon does today. See README 'Recording and updating goldens'."
    )
    expected = json.loads(path.read_text())
    assert json.dumps(expected, sort_keys=True) == json.dumps(recorded, sort_keys=True), (
        f"golden mismatch for {nodeid} ({path}):\n"
        f"--- golden ---\n{json.dumps(expected, indent=2, sort_keys=True)}\n"
        f"--- actual ---\n{json.dumps(recorded, indent=2, sort_keys=True)}\n"
        "If the new value is correct, re-record with UPDATE_GOLDEN=1."
    )


def pytest_collection_modifyitems(config, items) -> None:
    """Record which collected tests use `differential`, and decide whether the
    stale-golden check may run at all.

    Stale detection compares committed golden files against the keys visited this
    session, so it is only meaningful on a FULL, unfiltered run: `-k`/`-m`, explicit
    file/nodeid arguments, `--last-failed`-style reruns and `--collect-only` all
    leave goldens unvisited for reasons that are not staleness. `pytest_deselected`
    and `pytest_runtest_logreport` below disable it for the remaining cases (a
    deselected, skipped or failing differential cell)."""
    _GOLDEN_SESSION["expected"] = {
        item.nodeid for item in items if "differential" in getattr(item, "fixturenames", ())
    }
    opt = config.option
    filtered = bool(
        getattr(opt, "keyword", "")
        or getattr(opt, "markexpr", "")
        or getattr(opt, "file_or_dir", None)
        or getattr(opt, "lf", False)
        or getattr(opt, "failedfirst", False)
        or getattr(opt, "collectonly", False)
    )
    if filtered:
        _GOLDEN_SESSION["enforce_stale"] = False


def pytest_deselected(items) -> None:
    """A deselected cell leaves its golden unvisited — not stale."""
    if items:
        _GOLDEN_SESSION["enforce_stale"] = False


def pytest_runtest_logreport(report) -> None:
    """A differential cell that skipped or failed never reached its golden — so the
    stale check would false-fire. Non-differential outcomes (the style-B per-impl
    tests) are irrelevant to it and don't disable it."""
    if report.nodeid in _GOLDEN_SESSION["expected"] and (report.skipped or report.failed):
        _GOLDEN_SESSION["enforce_stale"] = False


def pytest_sessionfinish(session, exitstatus) -> None:
    """End-of-session golden accounting, enforced ONLY on a clean full run.

    The `exitstatus == 0` gate is load-bearing: an early abort (`-x`, `--maxfail`,
    a collection error, Ctrl-C) exits non-zero with cells legitimately unrun, so
    enforcing there would both false-flag stale goldens and clobber pytest's own
    exit code (2/3) with a generic 1. On a clean run two things must hold:
    1. every collected differential test actually CALLED the fixture — requesting
       `differential` and never invoking it would otherwise pass silently with no
       golden check at all;
    2. every committed golden was visited — a renamed or deleted test must not
       leave phantom coverage behind."""
    if exitstatus != 0 or not _GOLDEN_SESSION["enforce_stale"] or not GOLDENS_DIR.is_dir():
        return
    reporter = session.config.pluginmanager.get_plugin("terminalreporter")

    def _fail(title: str, lines: list) -> None:
        if reporter is not None:
            reporter.write_sep("=", title, red=True, bold=True)
            for line in lines:
                reporter.write_line(line)
        session.exitstatus = 1

    claimed_nodeids = set(_GOLDEN_SESSION["claimed"].values())
    uncalled = sorted(_GOLDEN_SESSION["expected"] - claimed_nodeids)
    if uncalled:
        _fail(
            "differential fixture never called",
            [f"{len(uncalled)} test(s) request `differential` but never invoked it — "
             "no golden was checked:"] + [f"  {n}" for n in uncalled],
        )
        return
    stale = sorted(
        p.name for p in GOLDENS_DIR.glob("*.json") if p.stem not in _GOLDEN_SESSION["visited"]
    )
    if stale:
        _fail(
            "stale goldens",
            [f"{len(stale)} committed golden file(s) were not visited by this run — "
             "the owning test was renamed or deleted. Delete them (or restore the test):"]
            + [f"  {GOLDENS_DIR / name}" for name in stale],
        )


@pytest.fixture
def differential(request):
    """Return `run(scenario) -> value`, where `scenario(impl) -> normalized value`.

    Runs the scenario against the `rust` daemon, asserts the normalized value against
    this test's recorded golden (see "Goldens" above), and returns it. The goldens
    were recorded while the Go daemon still existed, from a run in which the two
    implementations produced identical values — so the assertion each cell makes is
    still "the wire shape both implementations agreed on", now enforced against one
    runner instead of two.

    `scenario` keeps its `impl` parameter: the name of the compared implementation is
    what makes the harness readable, and it is the seam a second runner would slot
    back into."""

    calls = {"n": 0}

    def _run(scenario):
        calls["n"] += 1
        assert calls["n"] == 1, (
            f"{request.node.nodeid}: differential() called twice in one test. The "
            "golden key is the nodeid, so a second call would overwrite the first "
            "call's golden — split the test (or parametrize it) so each cell makes "
            "exactly one call."
        )
        rust = scenario("rust")
        _check_golden(request.node.nodeid, rust)
        return rust

    return _run
