"""Fixtures for the Go↔Rust RC one-shot parity harness (plan 009 §3.6).

Builds BOTH implementations once per session — `shed-machine-rc` (Go, the oracle)
and `sx` (Rust) — and drives them as black-box subprocesses against a hermetic
tmux server, asserting their wire-visible output is identical under
`normalize.py`'s canonicalization, then pinning it to a committed golden recorded
from the Go side.

Hermeticity, in the order the traps bite:

* **The hub.** Every Go `create` otherwise spawns a detached, setsid'd activity
  hub on the FIXED loopback port 1029 — which would survive teardown, cross-
  contaminate cells and poke the developer's real hub. `SHED_RC_NO_HUB=1` (the C2
  oracle seam, honored identically by the Rust engine) neutralizes it on both
  sides; a session-scoped guard asserts no test-spawned process ended up holding
  the port.
* **tmux.** Each CONTEXT gets its own `TMUX_TMPDIR` — a shallow `mkdtemp`,
  because an AF_UNIX path caps at ~104 bytes and pytest's tmp tree blows past
  that. In the `isolated` flavor a context is one implementation LEG, so the two
  legs run on separate tmux servers, cannot see each other's sessions, and use
  the SAME pinned `--slug`/`--name` — which is what lets the DTOs compare with no
  slug masking. In the `shared` flavor one context serves BOTH implementations
  (interop + preseed-in-place), so coexisting sessions take distinct slugs.
* **PATH.** `bash -lc` (the installed-agent gate) and `bash -l` (the shell kind)
  REBUILD PATH from `/etc/profile` + macOS `path_helper`, so prepending onto the
  pytest process's PATH vanishes. `_clean_env` therefore writes `.bash_profile`,
  `.bashrc` AND `.profile` into the leg's fresh HOME prepending the shim dir, and
  constructs a MINIMAL PATH rather than inheriting the developer's — which also
  keeps a brew-installed agent (or `shed-machine-rc`) from outranking a shim.
* **Agents.** The four agent binaries are `sh` shims that record their argv to
  `$HOME/agent-argv.txt`, print a fixed pane, then `exec cat` (so the pane stays
  alive and a delivered prompt echoes into it). Nothing real is ever launched.

No sleeps anywhere: every wait is a deadline poll that reports its last snapshot.
"""

from __future__ import annotations

import dataclasses
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
# shim can only ever be `starting`. The REACTIVE variants below (a dialog that
# redraws after the engine's keystroke) are what exercise its ready path.
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

# The preamble every shim shares: answer the capability probe, then record the argv
# the engine actually launched us with.
SHIM_PREAMBLE = """\
#!/bin/sh
# rc-parity fake agent — nothing real is ever launched.
case "$1" in
  --version) printf '%s\\n' '{version}'; exit 0 ;;
esac
for a in "$@"; do printf '%s\\n' "$a" >> "$HOME/agent-argv.txt"; done
"""

# The STATIC shim: draw a fixed pane, then hold the pane open on stdin so a
# delivered prompt echoes back for capture-pane assertions.
SHIM_TEMPLATE = (
    SHIM_PREAMBLE
    + """\
{pane}
exec cat
"""
)

# The REACTIVE shim (plan 009 §3.6): draw a dialog, block until the engine sends a
# line-terminating keystroke — recording EVERY byte that arrives, in hex — then
# redraw as ready. It is what proves the `--wait` poller's keystrokes: a static
# pane would eat the whole 20 s timeout and prove nothing.
#
# Two mechanics worth knowing before reading a recorded transcript:
#
#   * the pane's tty is in CANONICAL mode (the shim never puts it in raw mode), so
#     the CR that `tmux send-keys Enter` writes is delivered to the process as LF
#     (ICRNL) and nothing is readable until that line terminator arrives. A `Down`
#     (ESC [ B) therefore shows up in the SAME read burst as the Enter that
#     follows it — which is exactly the ordering assertion we want.
#   * the redraw pushes the dialog out of the engine's CAPTURE WINDOW with blank
#     lines rather than an escape sequence (portable across dash and bash, whose
#     `printf` disagree about `\033`). That window is `capture-pane -S -200` —
#     the visible frame PLUS 200 lines of scrollback (`tmux.go:76`) — so scrolling
#     the dialog off the visible pane is NOT enough: a trust dialog still inside
#     the window keeps classifying as needs-trust, and the poller (whose accept is
#     latched once) would then return needs-trust instead of ready. Hence 220
#     lines. A real TUI redraws on the alternate screen and leaves no scrollback
#     at all; this is the line-oriented equivalent.
REACTIVE_TEMPLATE = (
    SHIM_PREAMBLE
    + """\
{dialog}
while :; do
  b=$(dd bs=1 count=1 2>/dev/null | od -An -tx1 | tr -d ' \\n')
  [ -n "$b" ] || break
  printf '%s\\n' "$b" >> "$HOME/agent-stdin.hex"
  [ "$b" = "0a" ] && break
done
i=0
while [ $i -lt 220 ]; do printf '\\n'; i=$((i+1)); done
{ready}
exec cat
"""
)

# claude's dialogs and its ready screen, anchored on the real classifier's regexes
# (`internal/ext/rc/rc.go:374-398`, `agents.go:674-681`) — short lines, because a
# detached tmux pane is 80 columns.
TRUST_DIALOG = ["Quick safety check", "Yes, I trust this folder"]
BYPASS_DIALOG = ["WARNING: Bypass Permissions mode", "2. Yes, I accept"]
CLAUDE_READY = ["Remote Control active", "https://claude.ai/code/session_TESTTEST"]


def _printf(lines) -> str:
    return "printf '%s\\n' " + " ".join(f"'{line}'" for line in lines)


def static_shim(pane) -> str:
    """A shim that draws `pane` once and holds it open."""
    return SHIM_TEMPLATE.format(version=SHIM_VERSION, pane=_printf(pane))


def reactive_shim(dialog, ready=CLAUDE_READY) -> str:
    """A shim that draws `dialog`, records the engine's keystrokes, then redraws
    `ready` — the seam the `--wait` trust/bypass scenarios drive."""
    return REACTIVE_TEMPLATE.format(
        version=SHIM_VERSION, dialog=_printf(dialog), ready=_printf(ready)
    )


@dataclasses.dataclass
class RunResult:
    """One CLI invocation's outcome."""

    argv: list
    returncode: int
    stdout: str
    stderr: str

    def json(self) -> dict:
        assert self.returncode == 0, f"{self.argv}: exit {self.returncode}: {self.stderr}"
        try:
            return json.loads(self.stdout)
        except ValueError as exc:  # pragma: no cover - a failure prints both streams
            raise AssertionError(
                f"{self.argv}: stdout is not JSON ({exc})\n"
                f"--- stdout ---\n{self.stdout}\n--- stderr ---\n{self.stderr}"
            ) from exc


def _build(cmd, cwd, env=None) -> None:
    proc = subprocess.run(cmd, cwd=cwd, env=env, capture_output=True, text=True)
    if proc.returncode != 0:
        raise AssertionError(
            f"build failed: {' '.join(cmd)} (cwd={cwd})\n"
            f"--- stdout ---\n{proc.stdout}\n--- stderr ---\n{proc.stderr}"
        )


@pytest.fixture(scope="session")
def binaries(tmp_path_factory) -> dict:
    """Build both implementations once and return `{"go": path, "rust": path}`.

    Go's binary is built into a session tmp dir (the repo's `bin/` is the
    developer's, and must not be clobbered by a test run); the Rust binary is
    cargo's usual `debug/sx`, honoring `CARGO_TARGET_DIR` the way
    `tests/host-agent-diff` does (a RELATIVE value resolves against `crates/`,
    cargo's cwd here — not pytest's)."""
    out_dir = tmp_path_factory.mktemp("bin")
    go_bin = out_dir / "shed-machine-rc"
    _build(
        ["go", "build", "-o", str(go_bin), "./cmd/shed-machine-rc"],
        cwd=REPO_ROOT,
    )
    assert go_bin.exists(), f"go binary missing: {go_bin}"

    cargo_env = dict(os.environ)
    cargo_env["PATH"] = (
        str(Path.home() / ".cargo" / "bin") + os.pathsep + cargo_env.get("PATH", "")
    )
    _build(["cargo", "build", "-p", "sx"], cwd=CRATES_ROOT, env=cargo_env)
    env_target = os.environ.get("CARGO_TARGET_DIR")
    if env_target:
        target_dir = Path(env_target)
        if not target_dir.is_absolute():
            target_dir = CRATES_ROOT / target_dir
    else:
        target_dir = CRATES_ROOT / "target"
    rust_bin = target_dir / "debug" / "sx"
    assert rust_bin.exists(), f"rust binary missing: {rust_bin}"

    return {"go": str(go_bin), "rust": str(rust_bin)}


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
    wholesale (the reactive `--wait` variants) — both legs of a differential must
    always be given the SAME overrides, or the two are not running one scenario."""
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


def argv_prefix(impl: str, binary: str) -> list:
    """The implementation's argv prefix: the Go binary takes the verb directly,
    `sx` namespaces it under `rc` (plan 009 §3.2)."""
    assert impl in ("go", "rust"), f"unknown implementation {impl!r}"
    return [binary] if impl == "go" else [binary, "rc"]


class Rig:
    """One hermetic CONTEXT: a fresh HOME, a private tmux server, a shim PATH.

    Everything below the CLI invocation — the environment, the tmux observation
    helpers, the deadline polls, the teardown — is context-level, not
    implementation-level, which is exactly why the two flavors can share it:

    * `Leg` binds the context to ONE implementation (the isolated flavor: two
      contexts, one per impl).
    * `SharedRig` binds ONE context to BOTH implementations (the shared flavor:
      each call names the impl that should run the verb)."""

    def __init__(
        self,
        label: str,
        home: Path,
        tmux_tmpdir: Path,
        tmux_bin: str,
        shims: dict | None = None,
    ):
        self.label = label
        self.home = home
        self.tmux_tmpdir = tmux_tmpdir
        self.tmux_bin = tmux_bin
        self.shim_dir = home / "shims"
        self.shim_dir.mkdir()
        _write_shims(self.shim_dir, shims)
        self.env = _clean_env(home, tmux_tmpdir, self.shim_dir, tmux_bin)

    def uninstall_agents(self, *names: str) -> None:
        """Take agent binaries OFF this context's PATH — how a capability
        differential proves an `installed: false` row rather than asserting only
        the true one."""
        for name in names:
            (self.shim_dir / name).unlink(missing_ok=True)

    # -- the CLI under test -------------------------------------------------

    def _invoke(
        self, argv: list, stdin: str | None = None, timeout: float = 60
    ) -> RunResult:
        proc = subprocess.run(
            argv,
            env=self.env,
            input=(stdin or "").encode(),
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

    def sessions(self) -> list:
        res = self.tmux("ls", "-F", "#{session_name}")
        if res.returncode != 0:
            return []
        return sorted(line for line in res.stdout.split("\n") if line.strip())

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

    def capture(self, name: str) -> str:
        res = self.tmux("capture-pane", "-p", "-t", name)
        return res.stdout if res.returncode == 0 else ""

    def read_bytes(self, relative: str) -> bytes:
        """A file under this context's HOME, as RAW BYTES — the preseed
        artifacts' comparison model (plan 009 §3.5)."""
        return (self.home / relative).read_bytes()

    def _shim_log(self, relative: str) -> str:
        """A file a shim appends to, or `""` when it does not exist yet — every
        reader below is polled while the shim may not have written anything."""
        try:
            return (self.home / relative).read_text()
        except OSError:
            return ""

    def agent_stdin_hex(self) -> list:
        """The bytes the reactive shim received on stdin, one lowercase hex pair
        per element, in arrival order — the proof of WHICH keystrokes the `--wait`
        poller sent (a single Enter for trust; Down then Enter for bypass)."""
        raw = self._shim_log("agent-stdin.hex")
        return [line for line in raw.split("\n") if line.strip()]

    def agent_argv(self) -> list:
        """The argv the PATH-shim agent recorded (one element per line)."""
        lines = self._shim_log("agent-argv.txt").split("\n")
        # A complete capture ends with the trailing newline of its last element.
        return lines[:-1] if lines and lines[-1] == "" else []

    # -- deadline polls (never a sleep) -------------------------------------

    def _poll(self, what: str, predicate, timeout: float):
        deadline = time.monotonic() + timeout
        last = None
        while time.monotonic() < deadline:
            last = predicate()
            if last:
                return last
            time.sleep(0.02)
        raise AssertionError(f"{self.label}: {what} within {timeout}s; last={last!r}")

    def wait_for_session(self, name: str, timeout: float = 10) -> list:
        def listed():
            names = self.sessions()
            return names if name in names else None

        return self._poll(f"session {name} never appeared", listed, timeout)

    def wait_for_pane(self, name: str, needle: str = "", timeout: float = 15) -> str:
        """Poll until the pane has drawn `needle` (or anything at all when it is
        empty). The engine's own settle constants are 750 ms, so a session that
        has not drawn within the budget is a real failure, not a slow machine."""

        def drawn():
            text = self.capture(name)
            return text if (needle in text if needle else text.strip()) else None

        return self._poll(f"pane of {name} never showed {needle!r}", drawn, timeout)

    def wait_for_agent_argv(self, count: int, timeout: float = 15) -> list:
        """Poll until the shim has recorded at least `count` argv elements.

        The count is REQUIRED, mirroring `wait_for_stdin_hex`, because the shim
        writes its argv file element by element: polling for "non-empty" can read
        a half-written file and hand back a short list. That is not theoretical —
        it was observed live, one leg seeing 3 elements where the other saw 4,
        which surfaces as a spurious cross-implementation diff rather than as an
        honest failure. Waiting for the expected LENGTH makes the read
        deterministic.
        """

        def recorded():
            got = self.agent_argv()
            return got if len(got) >= count else None

        return self._poll(
            f"the shim agent never recorded {count} argv element(s)", recorded, timeout
        )

    def wait_for_stdin_hex(self, count: int, timeout: float = 15) -> list:
        """Poll until the reactive shim has recorded at least `count` bytes."""

        def recorded():
            got = self.agent_stdin_hex()
            return got if len(got) >= count else None

        return self._poll(f"the shim never received {count} stdin byte(s)", recorded, timeout)

    # -- teardown -----------------------------------------------------------

    def teardown(self) -> None:
        # kill-server IS the session cleanup: each test gets its own private
        # server, so nothing can leak across tests, and cells may legitimately
        # leave sessions for this reaper. (An earlier "stray rc-*" assert here
        # was dead code — it ran after kill-server, when sessions() can only read
        # [] — and arming it would wrongly fail those cells, so it was removed
        # rather than falsely advertised; C4 review finding.)
        self.tmux("kill-server")
        shutil.rmtree(self.tmux_tmpdir, ignore_errors=True)


class Leg(Rig):
    """One implementation running in its own HOME + tmux server (the ISOLATED
    flavor). `run()` needs no impl argument — the leg IS the implementation."""

    def __init__(
        self,
        impl: str,
        binary: str,
        home: Path,
        tmux_tmpdir: Path,
        tmux_bin: str,
        shims: dict | None = None,
    ):
        super().__init__(impl, home, tmux_tmpdir, tmux_bin, shims)
        self.impl = impl
        self.binary = binary

    def argv_for(self, sub: str, args) -> list:
        return argv_prefix(self.impl, self.binary) + [sub] + list(args)

    def run(self, sub: str, *args, stdin: str | None = None, timeout: float = 60) -> RunResult:
        return self._invoke(self.argv_for(sub, args), stdin=stdin, timeout=timeout)


class SharedRig(Rig):
    """BOTH implementations against ONE tmux server and ONE HOME (the SHARED
    flavor). `run(impl, verb, …)` picks which binary executes the verb.

    Why sharing is not an optional convenience here:

    * **Interop** is the mixed-fleet property itself — a session one binary
      created is a session the other must be able to read, prompt and kill. Two
      isolated servers cannot express it: each implementation would only ever see
      its own sessions, and the cell would prove nothing beyond what the isolated
      differentials already prove.
    * **Preseed-in-place** is about ONE file on ONE machine that both binaries
      merge into, in sequence. The byte-exactness that matters is what the SECOND
      writer does to the FIRST writer's document, which only exists when the two
      share a HOME.

    Because both implementations see one server, coexisting sessions must carry
    DISTINCT pinned slugs (a shared server is exactly where a duplicate slug is
    an error — see `test_exit_classes.test_duplicate_slug_is_exit_3`)."""

    def __init__(
        self,
        label: str,
        binaries: dict,
        home: Path,
        tmux_tmpdir: Path,
        tmux_bin: str,
        shims: dict | None = None,
    ):
        super().__init__(label, home, tmux_tmpdir, tmux_bin, shims)
        self.binaries = dict(binaries)

    def argv_for(self, impl: str, sub: str, args) -> list:
        return argv_prefix(impl, self.binaries[impl]) + [sub] + list(args)

    def run(
        self, impl: str, sub: str, *args, stdin: str | None = None, timeout: float = 60
    ) -> RunResult:
        return self._invoke(self.argv_for(impl, sub, args), stdin=stdin, timeout=timeout)


def _fresh_context(tmp_path_factory, name: str) -> tuple:
    home = tmp_path_factory.mktemp(f"home-{name}")
    # Shallow (AF_UNIX limit), NOT under pytest's nested tmp tree.
    return home, Path(tempfile.mkdtemp(prefix="rcp-"))


@pytest.fixture
def isolated(binaries, tmux_bin, tmp_path_factory):
    """The ISOLATED flavor (plan 009 §3.6): `make(impl) -> Leg`, where each
    implementation gets its OWN tmux server and HOME.

    Independent differentials use this — both legs pass the SAME pinned
    `--slug`/`--name`, so the two DTOs compare with no slug masking at all. The
    cross-impl interop and preseed-in-place cells use the `shared` flavor below
    instead, where one server + one HOME serve both implementations.

    `shims` (the reactive `--wait` variants) applies when the leg is first built;
    a scenario must pass the same value on both legs, which it does by
    construction when it is a constant in the test body."""
    made: dict = {}

    def _leg(impl: str, shims: dict | None = None) -> Leg:
        if impl not in made:
            home, tmux_tmpdir = _fresh_context(tmp_path_factory, impl)
            made[impl] = Leg(impl, binaries[impl], home, tmux_tmpdir, tmux_bin, shims)
        return made[impl]

    yield _leg
    for leg in made.values():
        leg.teardown()


@pytest.fixture
def shared(binaries, tmux_bin, tmp_path_factory):
    """The SHARED flavor (plan 009 §3.6): `make(name, shims=None) -> SharedRig`
    — ONE tmux server + ONE HOME that BOTH implementations drive.

    `name` identifies the context, not the implementation: a test that needs two
    independent shared worlds (a cross-impl chain compared against its mirror; a
    Go→Rust preseed compared against the pure-Go reference) asks for two names
    and gets two servers + two HOMEs. Asking for the same name twice returns the
    same rig, so a scenario can be written straight-line.

    Teardown is the same discipline as `isolated`: every rig built here gets its
    server killed and its `TMUX_TMPDIR` removed, whatever the test did."""
    made: dict = {}

    def _rig(name: str = "shared", shims: dict | None = None) -> SharedRig:
        if name not in made:
            home, tmux_tmpdir = _fresh_context(tmp_path_factory, name)
            made[name] = SharedRig(name, binaries, home, tmux_tmpdir, tmux_bin, shims)
        return made[name]

    yield _rig
    for rig in made.values():
        rig.teardown()


# --- Goldens ---------------------------------------------------------------
#
# Bookkeeping copied from `tests/host-agent-diff/conftest.py` (the template this
# harness restores the TWO-implementation shape of): a golden per `differential()`
# call, keyed by the sanitized nodeid, recorded with `UPDATE_GOLDEN=1`, with the
# one-call-per-test, case-insensitive-collision and stale-sweep guards.

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
                f"{len(uncalled)} test(s) request `differential` but never invoked "
                "it — no golden was checked:"
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


@pytest.fixture
def differential(request):
    """Return `run(scenario) -> value`, where `scenario(impl) -> normalized value`.

    Runs the scenario against BOTH implementations, asserts the two normalized
    values are equal (with a readable diff), then pins the GO value — the oracle —
    to this test's golden. The golden therefore records "the wire shape the two
    implementations agreed on", exactly as `tests/host-agent-diff`'s did before
    its Go twin was retired."""
    calls = {"n": 0}

    def _run(scenario):
        calls["n"] += 1
        assert calls["n"] == 1, (
            f"{request.node.nodeid}: differential() called twice in one test. The "
            "golden key is the nodeid, so the second call would overwrite the "
            "first's golden — split (or parametrize) the test."
        )
        go = canonical(scenario("go"))
        rust = canonical(scenario("rust"))
        assert json.dumps(go, indent=2, sort_keys=True) == json.dumps(
            rust, indent=2, sort_keys=True
        ), (
            f"Go↔Rust divergence in {request.node.nodeid}:\n"
            f"--- go (shed-machine-rc) ---\n{json.dumps(go, indent=2, sort_keys=True)}\n"
            f"--- rust (sx rc) ---\n{json.dumps(rust, indent=2, sort_keys=True)}"
        )
        _check_golden(request.node.nodeid, go)
        return go

    return _run
