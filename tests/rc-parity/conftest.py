"""Fixtures for the Go↔Rust RC HUB differential harness (plan 010).

Each cell runs one scenario against TWO resident hub daemons — Go:
`shed-machine-rc serve --foreground` (the test-only oracle built from
`tests/rc-parity/oracle`, the retired binary's main, still running under the
`shed-machine-rc` identity the goldens were recorded with); Rust:
`shed-host-agent rc-hub` — asserts their wire-visible `/v1` output is identical
under `normalize.py`'s canonicalization, then pins it to a committed golden
recorded from the Go side.

**BOTH legs are stimulated by the Go oracle CLI** since plan 016 (S7) sunset
`sx`: `Leg.cli` is an explicit argv prefix and `hub_leg` hands the oracle to
both legs, so the differential's only controlled variable is the DAEMON. That is
deliberate, not a wiring bug — see `tests/rc-parity/README.md` § Purpose. It is
also asserted rather than assumed: `start_hub` checks the daemon's own basename
per leg and records the argv in `$HOME/hub.log`, and
`test_hub_wiring.py::test_legs_run_distinct_daemons` pins that the two legs run
different binaries.

Hermeticity, in the order the traps bite:

* **The hub.** Every Go `create` otherwise spawns a detached, setsid'd activity
  hub on the FIXED loopback port 1029 — which would survive teardown, cross-
  contaminate cells and poke the developer's real hub. `SHED_RC_NO_HUB=1` (the C2
  oracle seam, honored identically by the Rust engine) neutralizes it on both
  sides; a session-scoped guard asserts no test-spawned process ended up holding
  the port. It gates create-time ensure ONLY, never the explicit `serve` each
  hub leg starts on its own ephemeral port.
* **tmux.** Each LEG gets its own `TMUX_TMPDIR` — a shallow `mkdtemp`, because
  an AF_UNIX path caps at ~104 bytes and pytest's tmp tree blows past that. So
  the two legs run on separate tmux servers, cannot see each other's sessions,
  and use the SAME pinned `--slug`/`--name` — which is what lets the DTOs
  compare with no slug masking.
* **PATH.** `bash -lc` (the installed-agent gate) and `bash -l` (the shell kind)
  REBUILD PATH from `/etc/profile` + macOS `path_helper`, so prepending onto the
  pytest process's PATH vanishes. `_clean_env` therefore writes `.bash_profile`,
  `.bashrc` AND `.profile` into the leg's fresh HOME prepending the shim dir, and
  constructs a MINIMAL PATH rather than inheriting the developer's — which also
  keeps a brew-installed agent (or `shed-machine-rc`) from outranking a shim.
* **Agents.** The four agent binaries are `sh` shims that answer the capability
  probe's `--version`, print a fixed pane, then `exec cat` (so the pane stays
  alive and a delivered prompt echoes into it). Nothing real is ever launched.

No sleeps anywhere: every wait is a deadline poll that reports its last snapshot.
"""

from __future__ import annotations

import dataclasses
import http.client
import json
import os
import re
import shutil
import socket
import subprocess
import tempfile
import time
from pathlib import Path

import pytest

from normalize import canonical

# tests/rc-parity/conftest.py -> tests -> repo root.
REPO_ROOT = Path(__file__).resolve().parents[2]
CRATES_ROOT = REPO_ROOT / "crates"

# The fixed loopback port the Go activity hub binds (`rc.HubAddr`). Never bound by
# this suite — asserted, not used.
HUB_PORT = 1029

# The fake agents installed on the shim PATH. The pane each prints is chosen so the
# kind's classifier lands on a deterministic state with a SHORT line (a real pane
# fixture is 120 columns wide and would wrap in an 80-column tmux pane):
#
#   codex/opencode/cursor -> their ready anchor  -> state "ready"
#   claude                -> neutral text        -> state "starting"
#
# claude stays neutral on purpose: its ready state needs a URL, so a static claude
# shim can only ever be `starting`. (The REACTIVE shim variants that drove its
# ready path retired with the one-shot `--wait` family in plan 016.)
SHIM_PANES = {
    "claude": ["claude fixture pane (rc-parity)"],
    "codex": ["Find and fix a bug in @filename"],
    "opencode": ["Ask anything..."],
    "cursor-agent": ["→ Plan, search, build anything"],
}

# The version a shim answers `--version` with. Capability discovery probes every
# agent binary with `bash -lc "'<bin>' --version"`; without this branch the shim
# would print its pane and block on `cat` until the probe's timeout, and every
# agent would degrade to "installed, version unknown" — testing the budget instead
# of the parse. The value is masked in the differential (shape-asserted only).
SHIM_VERSION = "rc-parity fake agent 1.2.3"

# The shim: answer the capability probe, draw a fixed pane, then hold the pane
# open on stdin so a delivered prompt echoes back into it. (A second, REACTIVE
# variant recorded stdin bytes and redrew on a keystroke; it retired with the
# one-shot `--wait` family in plan 016.)
SHIM_TEMPLATE = """\
#!/bin/sh
# rc-parity fake agent — nothing real is ever launched.
case "$1" in
  --version) printf '%s\\n' '{version}'; exit 0 ;;
esac
{pane}
exec cat
"""


def static_shim(pane) -> str:
    """A shim that draws `pane` once and holds it open."""
    printf = "printf '%s\\n' " + " ".join(f"'{line}'" for line in pane)
    return SHIM_TEMPLATE.format(version=SHIM_VERSION, pane=printf)


@dataclasses.dataclass
class RunResult:
    """One CLI invocation's outcome."""

    argv: list
    returncode: int
    stdout: str
    stderr: str


def _build(cmd, cwd, env=None) -> None:
    proc = subprocess.run(cmd, cwd=cwd, env=env, capture_output=True, text=True)
    if proc.returncode != 0:
        raise AssertionError(
            f"build failed: {' '.join(cmd)} (cwd={cwd})\n"
            f"--- stdout ---\n{proc.stdout}\n--- stderr ---\n{proc.stderr}"
        )


@pytest.fixture(scope="session")
def binaries(tmp_path_factory) -> dict:
    """Build both daemons' binaries once and return `{"go": …, "rust_hub": …}`.

    `go` is the oracle: the Go hub daemon (`serve --foreground`) AND — since
    plan 016 sunset `sx` — the session-creation CLI for BOTH legs. It is built
    into a session tmp dir (the repo's `bin/` is the developer's, and must not be
    clobbered by a test run).

    `rust_hub` is the Rust hub daemon (`shed-host-agent rc-hub`) at cargo's usual
    `debug/shed-host-agent`, honoring `CARGO_TARGET_DIR` the way
    `tests/host-agent-diff` does (a RELATIVE value resolves against `crates/`,
    cargo's cwd here — not pytest's)."""
    out_dir = tmp_path_factory.mktemp("bin")
    go_bin = out_dir / "shed-machine-rc"
    _build(
        ["go", "build", "-o", str(go_bin), "./tests/rc-parity/oracle"],
        cwd=REPO_ROOT,
    )
    assert go_bin.exists(), f"go binary missing: {go_bin}"

    cargo_env = dict(os.environ)
    cargo_env["PATH"] = (
        str(Path.home() / ".cargo" / "bin") + os.pathsep + cargo_env.get("PATH", "")
    )
    _build(
        ["cargo", "build", "-p", "shed-host-agent", "--locked"],
        cwd=CRATES_ROOT,
        env=cargo_env,
    )
    env_target = os.environ.get("CARGO_TARGET_DIR")
    if env_target:
        target_dir = Path(env_target)
        if not target_dir.is_absolute():
            target_dir = CRATES_ROOT / target_dir
    else:
        target_dir = CRATES_ROOT / "target"
    # The hub family's Rust daemon (plan 010 H12): the host-agent binary whose
    # `rc-hub` subcommand is the harness leg. The Go hub daemon needs no extra
    # build — it IS shed-machine-rc (`serve --foreground`).
    rust_hub_bin = target_dir / "debug" / "shed-host-agent"
    assert rust_hub_bin.exists(), f"rust hub binary missing: {rust_hub_bin}"

    return {"go": str(go_bin), "rust_hub": str(rust_hub_bin)}


@pytest.fixture(scope="session")
def tmux_bin() -> str:
    """The `tmux` the harness (and both engines) drive. tmux ≥ 3.2 is an implicit
    floor on both implementations (`new-session -e` is how session metadata is
    stamped); assert it rather than debugging a mangled env surface later."""
    found = shutil.which("tmux")
    if not found:
        pytest.skip("tmux is not installed (the rc engine's hard dependency)")
    out = subprocess.run([found, "-V"], capture_output=True, text=True)
    m = re.search(r"(\d+)\.(\d+)", out.stdout)
    assert m, f"could not read a tmux version from {out.stdout!r}"
    major, minor = int(m.group(1)), int(m.group(2))
    assert (major, minor) >= (3, 2), (
        f"tmux {out.stdout.strip()} is below the 3.2 floor (`new-session -e`)"
    )
    return found


def _port_in_use(port: int) -> bool:
    with socket.socket(socket.AF_INET, socket.SOCK_STREAM) as sock:
        sock.settimeout(0.25)
        return sock.connect_ex(("127.0.0.1", port)) == 0


@pytest.fixture(scope="session", autouse=True)
def hub_port_guard():
    """Belt-and-suspenders for the `SHED_RC_NO_HUB` kill-switch: if a create ever
    spawned a hub despite it, port 1029 would be freshly held at session end. A hub
    the DEVELOPER was already running is fine — only a newly-appeared one fails."""
    before = _port_in_use(HUB_PORT)
    yield
    after = _port_in_use(HUB_PORT)
    assert not (after and not before), (
        f"a test-spawned process is holding 127.0.0.1:{HUB_PORT} — the "
        "SHED_RC_NO_HUB kill-switch did not neutralize the create-time hub ensure"
    )


def _write_shims(shim_dir: Path, overrides: dict | None = None) -> None:
    """Install the four fake agents. `overrides` replaces a named agent's script
    wholesale — both legs of a differential must always be given the SAME
    overrides, or the two are not running one scenario."""
    scripts = {name: static_shim(pane) for name, pane in SHIM_PANES.items()}
    scripts.update(overrides or {})
    for name, script in scripts.items():
        path = shim_dir / name
        path.write_text(script)
        os.chmod(path, 0o755)


def _clean_env(home: Path, tmux_tmpdir: Path, shim_dir: Path, tmux_path: str) -> dict:
    """The hermetic environment for one implementation leg (see the module docstring)."""
    env = dict(os.environ)
    env["HOME"] = str(home)
    # AF_UNIX bind limit: the tmux server socket lives at
    # $TMUX_TMPDIR/tmux-<uid>/default, so this dir must be SHALLOW.
    env["TMUX_TMPDIR"] = str(tmux_tmpdir)
    # The C2 oracle seam, honored identically by the Rust engine's hub hook.
    env["SHED_RC_NO_HUB"] = "1"
    # A pytest run started from inside tmux would otherwise leak its session, and a
    # developer's workspace/config would leak into workdir + plan-path resolution.
    for leak in ("TMUX", "TMUX_PANE", "SHED_WORKSPACE", "CLAUDE_CONFIG_DIR"):
        env.pop(leak, None)

    # A CONSTRUCTED minimal PATH — not a prepend onto the developer's. The shims
    # come first; tmux's and bash's own directories follow so the engine can find
    # them; nothing else is visible, so a brew-installed agent or shed-machine-rc
    # cannot outrank a shim.
    parts = [str(shim_dir), str(Path(tmux_path).parent)]
    bash = shutil.which("bash")
    if bash:
        parts.append(str(Path(bash).parent))
    parts += ["/usr/bin", "/bin"]
    seen, path_dirs = set(), []
    for part in parts:
        if part not in seen and Path(part).is_dir():
            seen.add(part)
            path_dirs.append(part)
    env["PATH"] = os.pathsep.join(path_dirs)

    # `bash -lc` / `bash -l` / `bash -ic` rebuild PATH from /etc/profile (+ macOS
    # path_helper), so the shim dir must be re-prepended by the leg's own rc files
    # — all three, because the three shell modes read different ones.
    prepend = f'export PATH="{shim_dir}:$PATH"\n'
    for rc_file in (".bash_profile", ".bashrc", ".profile"):
        (home / rc_file).write_text(prepend)
    return env


class Leg:
    """One leg of the differential: a fresh HOME, a private tmux server, a shim
    PATH, and the CLI argv prefix that drives them.

    `cli` is that prefix, complete and explicit: `[binary]` for a CLI whose verbs
    are top-level, `[binary, "sub"]` for one that namespaces them. It is passed
    in rather than derived from `impl`, because since plan 016 the impl label and
    the CLI are deliberately decoupled — both legs are stimulated by the Go
    oracle while only the DAEMON differs (see the module docstring). `HubLeg`
    adds that resident daemon.
    """

    def __init__(
        self,
        impl: str,
        cli: list,
        home: Path,
        tmux_tmpdir: Path,
        tmux_bin: str,
        shims: dict | None = None,
    ):
        self.impl = impl
        self.cli = list(cli)
        self.home = home
        self.tmux_tmpdir = tmux_tmpdir
        self.tmux_bin = tmux_bin
        self.shim_dir = home / "shims"
        self.shim_dir.mkdir()
        _write_shims(self.shim_dir, shims)
        self.env = _clean_env(home, tmux_tmpdir, self.shim_dir, tmux_bin)

    # -- the CLI under test -------------------------------------------------

    def run(self, sub: str, *args, timeout: float = 60) -> RunResult:
        argv = self.cli + [sub] + list(args)
        proc = subprocess.run(
            argv,
            env=self.env,
            input=b"",
            capture_output=True,
            timeout=timeout,
        )
        return RunResult(
            argv=argv,
            returncode=proc.returncode,
            stdout=proc.stdout.decode("utf-8", "replace"),
            stderr=proc.stderr.decode("utf-8", "replace"),
        )

    # -- observation (never the code under test) ----------------------------

    def tmux(self, *args, timeout: float = 20) -> subprocess.CompletedProcess:
        return subprocess.run(
            [self.tmux_bin, *args],
            env=self.env,
            capture_output=True,
            text=True,
            timeout=timeout,
        )

    def session_env(self, name: str) -> dict:
        """The session's `SHED_RC_*` / `OPENCODE_*` environment as a mapping.

        Only those keys: the rest of a `show-environment` dump is the leg's own
        inherited environment (HOME, PATH, TMUX_TMPDIR …), which is harness
        plumbing, not the engine's contract."""
        res = self.tmux("show-environment", "-t", name)
        assert res.returncode == 0, f"show-environment {name}: {res.stderr}"
        out = {}
        for line in res.stdout.split("\n"):
            if "=" not in line or line.startswith("-"):
                continue
            key, _, value = line.partition("=")
            if key.startswith("SHED_RC_") or key.startswith("OPENCODE_"):
                out[key] = value
        return out

    # -- deadline polls (never a sleep) -------------------------------------

    def _poll(self, what: str, predicate, timeout: float):
        deadline = time.monotonic() + timeout
        last = None
        while time.monotonic() < deadline:
            last = predicate()
            if last:
                return last
            time.sleep(0.02)
        raise AssertionError(f"{self.impl}: {what} within {timeout}s; last={last!r}")

    # -- teardown -----------------------------------------------------------

    def teardown(self) -> None:
        # kill-server IS the session cleanup: each test gets its own private
        # server, so nothing can leak across tests, and cells may legitimately
        # leave sessions for this reaper. (An earlier "stray rc-*" assert here
        # was dead code — it ran after kill-server, when the server can only
        # report no sessions — and arming it would wrongly fail those cells, so
        # it was removed rather than falsely advertised; C4 review finding.)
        self.tmux("kill-server")
        shutil.rmtree(self.tmux_tmpdir, ignore_errors=True)


def _fresh_context(tmp_path_factory, name: str) -> tuple:
    home = tmp_path_factory.mktemp(f"home-{name}")
    # Shallow (AF_UNIX limit), NOT under pytest's nested tmp tree.
    return home, Path(tempfile.mkdtemp(prefix="rcp-"))


# --- Goldens ---------------------------------------------------------------
#
# Bookkeeping copied from `tests/host-agent-diff/conftest.py` (the template this
# harness restores the TWO-implementation shape of): a golden per
# `hub_differential()` call, keyed by the sanitized nodeid, recorded with
# `UPDATE_GOLDEN=1`, with the one-call-per-test, case-insensitive-collision and
# stale-sweep guards.

GOLDENS_DIR = Path(__file__).resolve().parent / "goldens"

_KEY_UNSAFE = re.compile(r"[^A-Za-z0-9._-]+")

_GOLDEN_SESSION: dict = {
    "claimed": {},
    "visited": set(),
    "expected": set(),
    "enforce_stale": True,
}


def _update_golden() -> bool:
    return os.environ.get("UPDATE_GOLDEN", "") not in ("", "0")


def _golden_key(nodeid: str) -> str:
    key = nodeid.replace(".py::", "__")
    key = _KEY_UNSAFE.sub("_", key).strip("_")
    assert key, f"nodeid sanitized to an empty golden key: {nodeid!r}"
    return key


def _claim_golden_key(nodeid: str) -> str:
    key = _golden_key(nodeid)
    # Case-insensitively: macOS filesystems fold case where CI's does not.
    owner = _GOLDEN_SESSION["claimed"].setdefault(key.lower(), nodeid)
    assert owner == nodeid, (
        f"golden key collision: {nodeid!r} and {owner!r} both sanitize to {key!r} "
        "(compared case-insensitively). Rename one of the tests."
    )
    return key


def _check_golden(nodeid: str, value) -> None:
    """Assert (or, under `UPDATE_GOLDEN=1`, record) the golden for `nodeid`."""
    key = _claim_golden_key(nodeid)
    path = GOLDENS_DIR / f"{key}.json"
    recorded = canonical(value)
    _GOLDEN_SESSION["visited"].add(key)

    if _update_golden():
        try:
            roundtrip = json.loads(json.dumps(recorded, allow_nan=False))
        except ValueError:
            roundtrip = None
        assert recorded == roundtrip, (
            f"{nodeid}: the differential value does not survive a JSON round-trip "
            "(a tuple/set/bytes/non-string key/NaN). Return plain dict/list/str/"
            f"int/bool/None from the scenario. value={value!r}"
        )
        text = json.dumps(recorded, indent=2, sort_keys=True) + "\n"
        GOLDENS_DIR.mkdir(exist_ok=True)
        if not path.exists() or path.read_text() != text:
            path.write_text(text)
        return

    assert path.exists(), (
        f"{nodeid}: no golden at {path}. A NEW cell records its first golden via "
        "UPDATE_GOLDEN=1 uv run pytest — for an EXISTING cell a missing golden "
        "means the file was deleted, and re-recording would silently bless "
        "whatever the binaries do today."
    )
    expected = json.loads(path.read_text())
    assert json.dumps(expected, sort_keys=True) == json.dumps(recorded, sort_keys=True), (
        f"golden mismatch for {nodeid} ({path}):\n"
        f"--- golden ---\n{json.dumps(expected, indent=2, sort_keys=True)}\n"
        f"--- actual ---\n{json.dumps(recorded, indent=2, sort_keys=True)}\n"
        "If the new value is correct, re-record with UPDATE_GOLDEN=1."
    )


def pytest_collection_modifyitems(config, items) -> None:
    # `hub_differential` is the ONE golden-pinning fixture left (the one-shot
    # family's `differential` retired with it in plan 016), and `fixturenames`
    # membership is an exact-name test — a cell that pins no golden (the wiring
    # cell) is deliberately not in this set.
    _GOLDEN_SESSION["expected"] = {
        item.nodeid
        for item in items
        if "hub_differential" in set(getattr(item, "fixturenames", ()))
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
    if items:
        _GOLDEN_SESSION["enforce_stale"] = False


def pytest_runtest_logreport(report) -> None:
    if report.nodeid in _GOLDEN_SESSION["expected"] and (report.skipped or report.failed):
        _GOLDEN_SESSION["enforce_stale"] = False


def pytest_sessionfinish(session, exitstatus) -> None:
    """End-of-session accounting, enforced ONLY on a clean full run: every
    collected differential test actually called the fixture, and every committed
    golden was visited."""
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
            [
                f"{len(uncalled)} test(s) request `hub_differential` but never "
                "invoked it — no golden was checked:"
            ]
            + [f"  {n}" for n in uncalled],
        )
        return
    stale = sorted(
        p.name for p in GOLDENS_DIR.glob("*.json") if p.stem not in _GOLDEN_SESSION["visited"]
    )
    if stale:
        _fail(
            "stale goldens",
            [
                f"{len(stale)} committed golden file(s) were not visited by this run "
                "— the owning test was renamed or deleted. Delete them (or restore "
                "the test):"
            ]
            + [f"  {GOLDENS_DIR / name}" for name in stale],
        )


def _dump(value) -> str:
    """The stable rendering a differential compares on and reports with."""
    return json.dumps(value, indent=2, sort_keys=True)


# --- The hub family (plan 010) ---------------------------------------------
#
# Resident-daemon differential: each leg runs a REAL hub daemon — Go:
# `shed-machine-rc serve --foreground`; Rust (from H12): `shed-host-agent
# rc-hub` — on its OWN ephemeral loopback port via the sanctioned
# `SHED_RC_HUB_ADDR` seam, with IDENTICAL fast-tick tuning overrides so no cell
# ever settle-and-compares. The overrides go on the DAEMON subprocess env only,
# never `os.environ`. `_clean_env`'s `SHED_RC_NO_HUB=1` is correct here and kept:
# it gates create-time ensure only, never an explicit `serve`, and the
# `hub_port_guard` stays green because every hub binds an ephemeral port, not
# 1029.
#
# Since plan 016 the two legs share ONE stimulus (the Go oracle CLI), so the
# daemon is the differential's only controlled variable — which is why the leg's
# CLI argv and its DAEMON argv are two independent values, and why the daemon
# identity is asserted in `start_hub` rather than inferred from `impl`.

# Fast ticks for the differential (both legs ALWAYS get the same values):
# active/idle drive reconcile latency; idle-exit is pinned LARGE-FINITE because
# the Go seam cannot express "never" (resolve() maps <=0 back to the 15m default
# — plan 010 §2.5). A sixth knob drove the pane-stability settle; it went with
# that engine in S2 (charliek/shed#324), on both sides — a knob the two hubs read
# differently would be a silent drift point on a differential-gated wire.
HUB_TUNING = {
    "SHED_RC_HUB_ACTIVE_MS": "100",
    "SHED_RC_HUB_IDLE_MS": "250",
    "SHED_RC_HUB_IDLE_EXIT_MS": "86400000",
    "SHED_RC_HUB_HEARTBEAT_MS": "1000",
    "SHED_RC_HUB_WRITE_TIMEOUT_MS": "2000",
}


def _free_loopback_port() -> int:
    with socket.socket(socket.AF_INET, socket.SOCK_STREAM) as sock:
        sock.bind(("127.0.0.1", 0))
        return sock.getsockname()[1]


# The daemon each leg MUST be running, by binary basename. This is the wiring
# assertion's ground truth: since both legs share one CLI stimulus, a Rust leg
# that silently started the Go daemon would still pass all 38 goldens.
HUB_DAEMON_NAMES = {"go": "shed-machine-rc", "rust": "shed-host-agent"}


class HubLeg(Leg):
    """A `Leg` plus a resident hub daemon bound to an ephemeral loopback port.

    The leg carries TWO complete, independent argv values: the inherited `cli`
    prefix (the session-creation stimulus — the Go oracle on BOTH legs since plan
    016) and `hub_argv` (the daemon this leg's `/v1` answers come from, which is
    what the differential actually varies). Neither is derived from the other or
    from `impl` — `impl` names the leg (in messages and its context dir) and is
    the key `start_hub` CHECKS `hub_argv` against, never a value it builds from.

    The daemon shares the leg's hermetic env (HOME, TMUX_TMPDIR, constructed
    PATH) so it observes exactly the sessions this leg's CLI creates. Its
    stdout/stderr go to `$HOME/hub.log` for post-mortems, under a first line
    recording the argv it was actually launched with."""

    def __init__(self, *args, hub_argv: list, **kwargs):
        super().__init__(*args, **kwargs)
        self.hub_port = _free_loopback_port()
        self.hub_addr = f"127.0.0.1:{self.hub_port}"
        self.hub_argv = list(hub_argv)
        self._hub_proc: subprocess.Popen | None = None
        self._hub_log = None

    def start_hub(self) -> None:
        assert self._hub_proc is None, f"{self.impl}: hub already started"
        # ASSERT the wiring, never infer it. The goldens cannot see which daemon
        # answered — both legs are stimulated by the same CLI — so this is the
        # only place a mis-wired leg is caught at the source.
        expected = HUB_DAEMON_NAMES[self.impl]
        assert Path(self.hub_argv[0]).name == expected, (
            f"{self.impl}: the {self.impl} leg must run the {expected} daemon, "
            f"not {self.hub_argv!r}"
        )
        env = dict(self.env)
        env["SHED_RC_HUB_ADDR"] = self.hub_addr
        env.update(HUB_TUNING)
        self._hub_log = open(self.home / "hub.log", "wb")
        # The argv header: a saved run log then shows WHICH daemon each leg ran,
        # which pass output never prints (labels appear only on divergence).
        self._hub_log.write(f"# daemon argv: {self.hub_argv}\n".encode())
        self._hub_log.flush()
        self._hub_proc = subprocess.Popen(
            self.hub_argv,
            env=env,
            stdout=self._hub_log,
            stderr=self._hub_log,
            stdin=subprocess.DEVNULL,
        )
        # Readiness = the identity handshake, not a bare open port.

        def healthy():
            if self._hub_proc.poll() is not None:
                raise AssertionError(
                    f"{self.impl}: hub exited {self._hub_proc.returncode} before "
                    f"ready — see {self.home / 'hub.log'}:\n"
                    f"{(self.home / 'hub.log').read_text()}"
                )
            try:
                got = self.hub_request("GET", "/v1/health")
            except OSError:
                return None
            body = got["json"] or {}
            return got if body.get("app") == "shed-rc-hub" else None

        self._poll("hub never answered its health identity", healthy, 10)

    def hub_request(
        self,
        method: str,
        path: str,
        body: bytes | str | None = None,
        headers: dict | None = None,
        timeout: float = 10,
    ) -> dict:
        """One HTTP exchange with this leg's hub: `{"status": int, "json": ...}`.
        `json` is None for an empty or non-JSON body — cells that assert an
        envelope get a readable failure from the golden compare."""
        if isinstance(body, str):
            body = body.encode()
        conn = http.client.HTTPConnection("127.0.0.1", self.hub_port, timeout=timeout)
        try:
            conn.request(method, path, body=body, headers=headers or {})
            resp = conn.getresponse()
            raw = resp.read()
            status = resp.status
        finally:
            conn.close()
        parsed = None
        if raw:
            try:
                parsed = json.loads(raw.decode("utf-8"))
            except ValueError:
                parsed = None
        return {"status": status, "json": parsed}

    def hub_events_until(
        self, what: str, predicate, timeout: float = 20, on_subscribed=None
    ) -> list:
        """Read SSE EVENTS (comments dropped) from ONE `/v1/events`
        subscription until `predicate(events)` is truthy; returns every event
        read, in arrival order (the within-tick ordering is wire). Bounded:
        raises on the deadline rather than blocking forever. Reads go through
        `HTTPResponse.read1`, which de-chunks (both hubs stream chunked).

        `on_subscribed` (if given) runs after the literal `: ok` opener is
        read — the server-side proof the subscriber is REGISTERED — so a cell
        can trigger the events it wants to observe with no subscription race
        (subscribe → act → read, never act-then-hope)."""
        conn = http.client.HTTPConnection("127.0.0.1", self.hub_port, timeout=5)
        events: list = []
        buf = b""
        try:
            conn.request("GET", "/v1/events")
            resp = conn.getresponse()
            assert resp.status == 200, f"{self.impl}: /v1/events status {resp.status}"
            deadline = time.monotonic() + timeout

            def read_line() -> str:
                nonlocal buf
                while b"\n" not in buf:
                    assert time.monotonic() < deadline, (
                        f"{self.impl}: {what} — read {len(events)} events before "
                        f"the deadline: {events!r}"
                    )
                    try:
                        chunk = resp.read1(4096)
                    except (TimeoutError, OSError):
                        # A heartbeat gap longer than the socket timeout: let
                        # the deadline assert above be the one that speaks.
                        continue
                    if not chunk:
                        raise AssertionError(f"{self.impl}: SSE stream ended early")
                    buf += chunk
                line, _, buf = buf.partition(b"\n")
                return line.decode().rstrip("\r")

            if on_subscribed is not None:
                opener = read_line()
                assert opener == ": ok", (
                    f"{self.impl}: the stream must open with the literal "
                    f"`: ok` comment, got {opener!r}"
                )
                read_line()  # its trailing blank line
                on_subscribed()
                # The trigger (a full create, on the appear cell) has its own
                # timeout; restart the deadline so `timeout` bounds WAITING
                # FOR FRAMES, not the trigger.
                deadline = time.monotonic() + timeout

            name, data_lines = None, []
            while not predicate(events):
                assert time.monotonic() < deadline, (
                    f"{self.impl}: {what} — read {len(events)} events before the "
                    f"deadline: {events!r}"
                )
                text = read_line()
                if text.startswith(":"):
                    continue  # `: ok` opener + heartbeats — liveness, not wire
                if text.startswith("event: "):
                    name = text[len("event: ") :]
                elif text.startswith("data: "):
                    data_lines.append(text[len("data: ") :])
                elif text == "" and (name or data_lines):
                    events.append(
                        {"event": name, "data": json.loads("\n".join(data_lines))}
                    )
                    name, data_lines = None, []
        finally:
            conn.close()
        return events

    def hub_events_socket(self):
        """A RAW subscribed socket for the stalled-reader cell: the request is
        written and the response is never read. Caller closes it."""
        # A tiny receive buffer closes the TCP window once the hub has written
        # a few KB — what makes the write deadline observable without megabytes
        # of traffic. It must be set BEFORE connect (the window is negotiated
        # at the handshake; shrinking afterwards has no reliable effect).
        sock = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
        sock.setsockopt(socket.SOL_SOCKET, socket.SO_RCVBUF, 1)
        sock.settimeout(10)
        sock.connect(("127.0.0.1", self.hub_port))
        sock.sendall(b"GET /v1/events HTTP/1.1\r\nHost: hub\r\n\r\n")
        return sock

    def wait_hub(self, what: str, predicate, timeout: float = 15):
        """Deadline-poll an observable through the hub API (never a sleep)."""
        return self._poll(what, predicate, timeout)

    def wait_tracked(self, slug: str, timeout: float = 15) -> dict:
        """Poll until the RECONCILE loop has tracked `slug`, returning its entry.

        Merely appearing in `GET /v1/sessions` is NOT tracked-ness: that
        endpoint lists from tmux one-shot, while `/messages` and the verbs use
        the reconcile-built tracked map — a verb fired in the gap earns a 404
        `unknown_slug` instead of its kind-based 409 (observed live while
        recording the first goldens). The ACTIVITY OVERLAY is the observable
        proof the tracked session exists: only a tracked entry carries it.

        OPENCODE ONLY, since S2 (charliek/shed#324) removed the pane-stability
        fallback that gave every kind an overlay — a feedless kind now carries
        no activity at all. The codex-tracked cells use the `/messages` 404→200
        flip instead (`test_hub.py::_tracked_codex`)."""

        def tracked():
            got = self.hub_request("GET", "/v1/sessions")
            for entry in (got["json"] or {}).get("sessions", []):
                if entry.get("slug") == slug and "activity" in entry:
                    return entry
            return None

        return self.wait_hub(f"hub never tracked slug {slug}", tracked, timeout)

    def stop_hub(self) -> None:
        """Stop the daemon and close its log. Reap failures must not skip the
        log close (an unclosed file is a ResourceWarning, which
        `filterwarnings = error` pins on some LATER unrelated test), and the
        port-free assert runs LAST so it can never leave the daemon running."""
        proc, self._hub_proc = self._hub_proc, None
        try:
            if proc is not None:
                proc.terminate()
                try:
                    proc.wait(timeout=5)
                except subprocess.TimeoutExpired:
                    proc.kill()
                    proc.wait(timeout=5)
        finally:
            if self._hub_log is not None:
                self._hub_log.close()
                self._hub_log = None
        # The port must actually be free — a lingering holder would poison the
        # next cell's bind (and mirrors the session-scoped 1029 guard).
        self._poll(
            f"nothing released {self.hub_addr} after the hub stopped",
            lambda: not _port_in_use(self.hub_port),
            5,
        )

    def teardown(self) -> None:
        # finally: a wedged hub reap must not leak the tmux server + TMUX_TMPDIR.
        try:
            self.stop_hub()
        finally:
            super().teardown()


@pytest.fixture
def hub_leg(binaries, tmux_bin, tmp_path_factory):
    """`make(impl, shims=None) -> HubLeg` — one leg (its own HOME + tmux server)
    with its resident hub already healthy. At most one leg per impl.

    The two legs' argvs, spelled out in full rather than derived:

    * **cli** — the Go oracle on BOTH legs. `sx rc` was the Rust leg's stimulus
      until plan 016 (S7) sunset the crate; the surviving hub cells only ever
      invoke `create`, and the one-shot family proved the two engines wire-
      identical there, so a single stimulus costs the hub differential nothing
      and isolates it on the daemon.
    * **hub** — the daemon under test: `shed-machine-rc serve --foreground` vs
      `shed-host-agent rc-hub`. `start_hub` asserts each leg got the right one."""
    made: dict = {}
    oracle = binaries["go"]
    hub_argvs = {
        "go": [oracle, "serve", "--foreground"],
        "rust": [binaries["rust_hub"], "rc-hub"],
    }

    def _leg(impl: str, shims: dict | None = None) -> HubLeg:
        if impl not in made:
            home, tmux_tmpdir = _fresh_context(tmp_path_factory, f"hub-{impl}")
            # Register BEFORE start_hub: a failed/slow health handshake raises,
            # and an unregistered leg's daemon (idle-exit pinned to 24h) would
            # outlive the pytest process on its port, log handle open.
            leg = HubLeg(
                impl,
                [oracle],
                home,
                tmux_tmpdir,
                tmux_bin,
                shims,
                hub_argv=hub_argvs[impl],
            )
            made[impl] = leg
            leg.start_hub()
        return made[impl]

    yield _leg
    # One leg's teardown failure must not leak the other leg's daemon.
    errors = []
    for leg in made.values():
        try:
            leg.teardown()
        except Exception as exc:  # noqa: BLE001 - reported below, never swallowed
            errors.append(f"{leg.impl}: {exc}")
    assert not errors, "hub leg teardown failed: " + "; ".join(errors)


@pytest.fixture
def hub_differential(request):
    """The hub family's differential (see the section comment above):
    `run(scenario) -> value` where `scenario(impl) -> normalized value`.

    Run the scenario against the Go oracle's hub, assert the Rust hub agrees
    (with a readable diff naming both DAEMONS — what differs), then pin the GO
    value to this test's golden, so a golden always records "the wire shape the
    implementations agreed on", exactly as `tests/host-agent-diff`'s did before
    its Go twin was retired.

    BOTH legs ALWAYS run (equality-then-pin) since H12, and there is deliberately
    no switch back to the Go-only mode the H1½ phase froze the wire under: with
    one shared stimulus, a suite that can silently drop the Rust comparison is a
    suite that stays green while proving nothing (plan 016 §3.4)."""
    go_label, rust_label = "shed-machine-rc serve", "shed-host-agent rc-hub"
    calls = {"n": 0}

    def run(scenario):
        calls["n"] += 1
        assert calls["n"] == 1, (
            f"{request.node.nodeid}: hub_differential() called twice in one test. "
            "The golden key is the nodeid, so the second call would overwrite the "
            "first's golden — split (or parametrize) the test."
        )
        go = canonical(scenario("go"))
        rust = canonical(scenario("rust"))
        assert _dump(go) == _dump(rust), (
            f"Go↔Rust divergence in {request.node.nodeid}:\n"
            f"--- go ({go_label}) ---\n{_dump(go)}\n"
            f"--- rust ({rust_label}) ---\n{_dump(rust)}"
        )
        _check_golden(request.node.nodeid, go)
        return go

    return run
