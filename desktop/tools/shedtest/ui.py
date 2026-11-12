"""Launch / quit a hermetic shed-desktop UI for tests, and resolve its socket.

The Tauri client is a *subprocess* UI — a binary run in test mode, pointed at the
in-process mock, with a throwaway HOME/XDG_RUNTIME_DIR — described by one config
entry in `_SUBPROC` (binary, env-var prefix, socket name, `identify.platform`
stamp). The `target` argument every helper takes is a key into that table; it
defaults to the only entry, `"tauri"`. The harness ALWAYS owns the app it drives.
"""

from __future__ import annotations

import os
import platform
import shutil
import subprocess
import tempfile
import time
from dataclasses import dataclass, replace
from pathlib import Path

from client import IPCClient, ShedError, TauriClient, scaled_timeout

REPO_ROOT = Path(__file__).resolve().parents[2]
DEFAULT_TARGET = "tauri"


@dataclass(frozen=True)
class _Subproc:
    """Per-target config for the subprocess-launched UI (tauri)."""
    binary: Path
    env_prefix: str      # "SHED_TAURI"
    fallback_stem: str   # /tmp/<stem>-<uid> when XDG_RUNTIME_DIR is unset; also the log-file stem
    sock_rel: str        # socket path relative to the runtime dir (matches the app's env resolver)
    platform_id: str     # the identify.platform stamp: "tauri"
    client_cls: type     # the IPC client class for this target


_SUBPROC: dict[str, _Subproc] = {
    # the Tauri crate builds `shed-desktop-tauri` in its standalone workspace; the
    # socket is flat (shorter, so a throwaway XDG_RUNTIME_DIR under macOS's long
    # TMPDIR stays under SUN_LEN).
    "tauri": _Subproc(
        binary=REPO_ROOT / "tauri" / "src-tauri" / "target" / "debug" / "shed-desktop-tauri",
        env_prefix="SHED_TAURI", fallback_stem="shed-tauri",
        sock_rel="shed-tauri.sock", platform_id="tauri", client_cls=TauriClient),
}

# The Linux render gate builds the Tauri app with a relocated CARGO_TARGET_DIR (so
# a container build can't clobber the host's target dir); let it point the harness at
# that binary via SHED_TAURI_BIN.
if os.environ.get("SHED_TAURI_BIN"):
    _SUBPROC["tauri"] = replace(_SUBPROC["tauri"], binary=Path(os.environ["SHED_TAURI_BIN"]))

# the Tauri binary as a module attr (test_tauri.py references `ui.TAURI_BIN`).
TAURI_BIN = _SUBPROC["tauri"].binary


@dataclass
class _ProcState:
    """A harness-launched subprocess UI's process + captured log + throwaway
    HOME/XDG_RUNTIME_DIR + launch env. Retained so `wait_alive` can tell a
    crashed-on-boot UI from a slow one, `quit` can terminate + clean up, and the
    second-instance test can spawn a sibling with the same env."""
    proc: "subprocess.Popen[bytes] | None" = None
    log: Path | None = None
    log_fh: object = None
    runtime_dir: Path | None = None
    env: dict[str, str] | None = None


_state: dict[str, _ProcState] = {t: _ProcState() for t in _SUBPROC}


def _cfg(target: str) -> _Subproc:
    cfg = _SUBPROC.get(target)
    if cfg is None:
        raise ValueError(f"unknown target {target!r} (want {'|'.join(_SUBPROC)})")
    return cfg


def socket_path(target: str = DEFAULT_TARGET) -> Path:
    cfg = _cfg(target)
    runtime = _state[target].runtime_dir or Path(
        os.environ.get("XDG_RUNTIME_DIR") or f"/tmp/{cfg.fallback_stem}-{os.getuid()}"
    )
    return runtime / cfg.sock_rel


def _client_at(target: str, sock: Path) -> IPCClient:
    """The IPC client class for a target, bound to an EXPLICIT socket path — the
    single source of the target→class map. `make_client` resolves the socket
    from session state; a self-managed instance (down-host) passes its own."""
    return _cfg(target).client_cls(sock)


def make_client(target: str = DEFAULT_TARGET) -> IPCClient:
    """The IPC client for a target at its session-resolved socket."""
    return _client_at(target, socket_path(target))


def launch_env(target: str = DEFAULT_TARGET) -> dict[str, str]:
    """The env the session's subprocess UI was launched with — so the
    second-instance (single-instance) test can spawn a sibling against the same
    runtime."""
    st = _state.get(target)
    env = st.env if st else None
    if env is None:
        raise RuntimeError(f"no {target} UI launched this session")
    return dict(env)


def _log_path(cfg: _Subproc) -> Path:
    """Where a harness-launched subprocess UI's stdout+stderr are captured. Kept
    OUTSIDE the throwaway runtime dir (which `quit` removes) so a boot-failure log
    survives for CI. Honors `<PREFIX>_E2E_LOG_DIR`; else the system temp dir."""
    base = Path(os.environ.get(f"{cfg.env_prefix}_E2E_LOG_DIR") or tempfile.gettempdir())
    base.mkdir(parents=True, exist_ok=True)
    return base / f"{cfg.fallback_stem}-ui.log"


def is_alive(target: str = DEFAULT_TARGET) -> bool:
    try:
        c = make_client(target)
        try:
            c.identify()
            return True
        finally:
            c.close()
    except (OSError, ShedError):
        return False


def quit(target: str = DEFAULT_TARGET) -> None:
    """Quit the harness-owned UI: terminate/kill the child, then remove its
    throwaway runtime dir."""
    _cfg(target)
    _quit_subproc(target)


def terminate(proc: "subprocess.Popen[bytes] | None") -> None:
    """terminate → wait → SIGKILL a subprocess UI child, tolerating one that has
    already exited. Shared by `_quit_subproc` (the session instance) and a
    self-managed instance's teardown (down-host)."""
    if proc is None or proc.poll() is not None:
        return
    proc.terminate()
    try:
        proc.wait(timeout=scaled_timeout(5))
    except subprocess.TimeoutExpired:
        proc.kill()
        proc.wait()


def _quit_subproc(target: str) -> None:
    st = _state[target]
    terminate(st.proc)
    if st.log_fh is not None:
        st.log_fh.close()
    if st.runtime_dir is not None:
        shutil.rmtree(st.runtime_dir, ignore_errors=True)
    _state[target] = _ProcState()


def launch(target: str = DEFAULT_TARGET, *, mock_base_url: str, config_path: Path, state_dir: Path,
           host_agent_socket: str | None = None, unreachable_hosts: tuple[str, ...] = (),
           roost_sockets: dict[str, object] | None = None,
           ssh_bin: object | None = None,
           roost_jail: bool = False,
           roost_install_bin: object | None = None,
           roost_session_bin: object | None = None) -> None:
    """Launch the UI hermetically and block until it answers `identify`.

    `state_dir` is the throwaway per-session dir; it doubles as HOME +
    XDG_RUNTIME_DIR (so ~/.shed and the runtime socket are both isolated).
    `host_agent_socket` backs the approval gate (the fake host-agent).
    `unreachable_hosts` are config server NAMES the backend points at a closed
    port instead of the mock (the `<PREFIX>_MOCK_UNREACHABLE_HOSTS` down-host
    override) so the per-host error row is exercisable e2e.
    `roost_sockets` maps machine NAME -> the Unix socket its `roost-session`
    answers on, replacing roost's SSH client-bridge with a direct `LocalSession`
    — the test-mode-only `<PREFIX>_ROOST_SOCKETS` seam. Per-machine so a suite
    can serve a session for one and leave another unmapped (which reads as
    unreachable, covering the everyday asleep/off-network state); the implicit
    `localhost` host goes through the same map, so a hermetic run never reads the
    developer's own session. That is what makes the machine path testable
    hermetically: the harness serves a fake `roost-session` there
    (`fake_roost.py`) and the app reaches it through the REAL roost client +
    watcher, with no ssh and no remote host anywhere. Set-or-clear like `unreachable_hosts`: `None` (not given)
    or `{}` clears the env var so a hermetic launch never inherits a value from
    the parent shell; a non-empty map sets it. Driving against a real
    roost-session daemon is a separate, explicit opt-in — see the
    `SHEDTEST_ROOST_SOCKETS` harness var read by conftest.py's `_app_session`
    fixture.
    `ssh_bin` is a fake `ssh` the app's roost transports exec instead of the real
    one (`<PREFIX>_SSH_BIN`, test-mode only) — the plan-019 bootstrap seam. Unlike
    `roost_sockets`, which replaces the transport entirely, this KEEPS the real
    `SshBridge` and `SshExec`: the bootstrap's subject is a host with no session,
    and its answers come from `ssh` exit codes, `ssh` stderr and roost's own
    scripts run through a remote `/bin/sh -s`. A host that is neither mapped nor
    served by a fake `ssh` is unreachable, so a hermetic run still spawns no real
    ssh.
    `roost_jail` sets roost's `jail_fs_root` (`<PREFIX>_ROOST_JAIL=1`, test-mode
    only), which prefixes the candidate ladder's ABSOLUTE rungs with
    `${ROOST_BOOTSTRAP_FS_ROOT}` — expanded by the far side, i.e. by the fake
    `ssh`. Without it a cold-host cell finds the DEVELOPER's own
    `/usr/bin/roost-session` and reads `Mismatch` on a workstation and `Missing`
    in CI's container.
    `roost_install_bin` is roost's OWN override rung, `ROOST_SESSION_INSTALL_BIN`
    (plan 019 §3.5 rung 1) — not a `<PREFIX>_` var, because shed reads roost's
    variable by that name. It names the `roost-session` a bootstrap installs, so a
    cell can install a fake one; unset, the ladder falls through the sibling rung
    to the pinned release asset (`RELEASE_PIN`, roost 0.0.21) — and only reaches
    `NoSource`, the no-button cell, when that rung is taken away too. Set-or-
    cleared like the rest, so a developer with it exported cannot make the
    NoSource cell pass for the wrong reason.
    `roost_session_bin` is roost's `ROOST_SESSION_BIN` — its own "which daemon do
    I run" override, which the source ladder's SIBLING rung borrows to find a
    roost-session beside the client (plan 019 §3.5 rung 2). Pointing it at a path
    that does not exist is what makes "this machine has no current-generation
    roost" deterministic: without it the rung finds whatever `roost-session`
    happens to be on the developer's PATH and the NoSource cell reads
    differently on a workstation than in CI.
    """
    _cfg(target)
    _launch_subproc(target, mock_base_url=mock_base_url, config_path=config_path,
                    runtime_dir=state_dir, host_agent_socket=host_agent_socket,
                    unreachable_hosts=unreachable_hosts,
                    roost_sockets=roost_sockets,
                    ssh_bin=ssh_bin, roost_jail=roost_jail,
                    roost_install_bin=roost_install_bin,
                    roost_session_bin=roost_session_bin)


def subproc_env(cfg: _Subproc, *, runtime_dir: Path, mock_base_url: str,
                config_path: Path, host_agent_socket: str | None = None,
                unreachable_hosts: tuple[str, ...] = (),
                roost_sockets: dict[str, object] | None = None,
                ssh_bin: object | None = None,
                roost_jail: bool = False,
                roost_install_bin: object | None = None,
                roost_session_bin: object | None = None,
                craze_path: object | None = None,
                craze_home: object | None = None,
                craze_runtime_dir: object | None = None) -> dict[str, str]:
    """The launch env for a subprocess UI — the single source of the subprocess
    env-var contract, shared by the session launcher and a self-managed instance
    (down-host). HOME/XDG_RUNTIME_DIR/XDG_CONFIG_HOME/XDG_STATE_HOME are
    redirected to the throwaway `runtime_dir` (never the dev's ~/.shed, real
    runtime socket, real prefs, or real log file — see `app_log_path`), config is
    pinned to the fixture, test mode + mock are set, and `<PREFIX>_SOCKET` and
    `<PREFIX>_LOG_FILE` are cleared so the defaults (under runtime_dir) are used.

    `craze_path` is the craze seam (`<PREFIX>_CRAZE_PATH`, plan 025 §3.6.1,
    test-mode and debug-build only): the directory this machine's craze source
    finds `craze` in — the app then dials it through the JAILED ladder with
    `PATH` set to exactly that directory, and passes through only `HOME`,
    `CRAZE_HOME` and `CRAZE_RUNTIME_DIR`. Unset, a hermetic app has no local
    craze source at all. `craze_home` / `craze_runtime_dir` are craze's own
    `CRAZE_HOME` / `CRAZE_RUNTIME_DIR` for that dial — the runtime dir must be
    SHORT, 0700, and under `/tmp` (craze refuses one beneath a group-writable
    ancestor, plan 025 Amendment A3). All three are set-or-cleared: a
    developer's own `CRAZE_HOME` must never reach a hermetic launch."""
    env = dict(os.environ)
    env["HOME"] = str(runtime_dir)
    env["XDG_RUNTIME_DIR"] = str(runtime_dir)
    env["XDG_CONFIG_HOME"] = str(runtime_dir / "config")
    # The app's log file (#414) lands under here on Linux (macOS: under HOME's
    # ~/Library/Logs) — never in the developer's own ~/.local/state.
    env["XDG_STATE_HOME"] = str(runtime_dir / "state")
    env[f"{cfg.env_prefix}_TEST_MODE"] = "1"
    env[f"{cfg.env_prefix}_MOCK_BASE_URL"] = mock_base_url
    env[f"{cfg.env_prefix}_SHED_CONFIG"] = str(config_path)
    # The approval gate's host-agent socket (the fake, in tests).
    if host_agent_socket:
        env[f"{cfg.env_prefix}_HOST_AGENT_SOCKET"] = str(host_agent_socket)
    # **Every app-level knob below is SET-OR-CLEARED**, never merely set: an
    # inherited value from the parent shell must not reach a hermetic launch.
    # One loop rather than a stanza each, so the rule is structural — a knob
    # added to this table cannot forget to clear itself.
    #
    #  * MOCK_UNREACHABLE_HOSTS — down-host override: server NAMES the backend
    #    redirects to a closed port.
    #  * ROOST_SOCKETS — the roost seam: every `machines:` entry (and the
    #    implicit `localhost`) is reached on the named Unix socket directly,
    #    instead of through roost's SSH client-bridge. Driving the session app
    #    against a REAL roost-session daemon is an explicit harness opt-in via
    #    SHEDTEST_ROOST_SOCKETS (see conftest.py's `_app_session` fixture and
    #    `.claude/skills/shedtest-linux`), not env inheritance here.
    #  * SSH_BIN — the fake `ssh` the roost transports exec (plan 019's
    #    bootstrap seam). Load-bearing: an inherited value would point a
    #    hermetic run's execs at a real binary.
    #  * ROOST_JAIL — roost's `jail_fs_root`, so a cold-host cell cannot find
    #    the developer's own /usr/bin/roost-session.
    #  * SOCKET — cleared unconditionally so the XDG default (under
    #    runtime_dir) is used.
    #  * LOG_FILE — cleared unconditionally, like SOCKET (#414): an inherited
    #    one would send a hermetic run's log into the developer's file (or
    #    theirs into this run's), and `app_log_path` would no longer say where
    #    it is.
    #
    # NOTE: there is no roost poll knob to set. Since plan 014 the watcher
    # observes roost's push feed (`events.subscribe`) instead of polling
    # `tab.list`, so a machine-row change arrives when roost commits it — the
    # cadence env var this used to seed was deleted on both sides.
    #  * CRAZE_PATH — the craze seam (plan 025): the directory the LOCAL
    #    craze dial's jailed ladder finds `craze` in. Load-bearing like
    #    SSH_BIN: an inherited one would point a hermetic run at a real craze.
    managed = {
        "MOCK_UNREACHABLE_HOSTS": ",".join(unreachable_hosts) if unreachable_hosts else None,
        "ROOST_SOCKETS": (",".join(f"{n}={p}" for n, p in roost_sockets.items())
                          if roost_sockets else None),
        "SSH_BIN": str(ssh_bin) if ssh_bin else None,
        "ROOST_JAIL": "1" if roost_jail else None,
        "CRAZE_PATH": str(craze_path) if craze_path else None,
        "SOCKET": None,
        "LOG_FILE": None,
    }
    for suffix, value in managed.items():
        key = f"{cfg.env_prefix}_{suffix}"
        if value:
            env[key] = value
        else:
            env.pop(key, None)
    # roost's OWN variable, under roost's own name (not `<PREFIX>_`), so it is
    # spelled out here rather than in the table above. Same set-or-clear rule:
    # an inherited value would silently give the NoSource cell a source.
    for key, value in (
        ("ROOST_SESSION_INSTALL_BIN", roost_install_bin),
        ("ROOST_SESSION_BIN", roost_session_bin),
        # craze's own variables, under craze's own names, for the local
        # craze dial (the app passes them through beside the seam's PATH).
        ("CRAZE_HOME", craze_home),
        ("CRAZE_RUNTIME_DIR", craze_runtime_dir),
    ):
        if value:
            env[key] = str(value)
        else:
            env.pop(key, None)
    return env


def app_log_path(runtime_dir: Path) -> Path:
    """Where a subprocess UI launched with `subproc_env(runtime_dir=...)` writes
    its log file (#414) — the app's own platform default under the redirected
    HOME / XDG_STATE_HOME, since `<PREFIX>_LOG_FILE` is cleared."""
    if platform.system() == "Darwin":
        return runtime_dir / "Library" / "Logs" / "ShedDesktop" / "shed-desktop.log"
    return runtime_dir / "state" / "ai.stridelabs.shed-desktop" / "shed-desktop.log"


def _launch_subproc(target: str, *, mock_base_url: str, config_path: Path,
                    runtime_dir: Path, host_agent_socket: str | None = None,
                    unreachable_hosts: tuple[str, ...] = (),
                    roost_sockets: dict[str, object] | None = None,
                    ssh_bin: object | None = None,
                    roost_jail: bool = False,
                    roost_install_bin: object | None = None,
                    roost_session_bin: object | None = None) -> None:
    cfg = _SUBPROC[target]
    if not cfg.binary.exists():
        raise RuntimeError(
            f"{target} binary not found at {cfg.binary}; build it first "
            f"(tauri: `make tauri-build`).")
    env = subproc_env(cfg, runtime_dir=runtime_dir, mock_base_url=mock_base_url,
                      config_path=config_path, host_agent_socket=host_agent_socket,
                      unreachable_hosts=unreachable_hosts,
                      roost_sockets=roost_sockets,
                      ssh_bin=ssh_bin, roost_jail=roost_jail,
                      roost_install_bin=roost_install_bin,
                      roost_session_bin=roost_session_bin)
    st = _state[target]
    st.env = env
    st.runtime_dir = runtime_dir
    sock = socket_path(target)
    sock.parent.mkdir(parents=True, exist_ok=True)
    st.log = _log_path(cfg)
    st.log_fh = open(st.log, "wb")
    st.proc = subprocess.Popen([str(cfg.binary)], env=env, stdout=st.log_fh, stderr=subprocess.STDOUT)
    wait_alive(target, mock_base_url=mock_base_url)


def await_hermetic(target: str, *, sock: Path, mock_base_url: str, timeout: float = 30.0,
                   proc: "subprocess.Popen[bytes] | None" = None,
                   log: Path | None = None) -> None:
    """Block until the UI at `sock` answers `identify` AND confirms it's hermetic
    (test mode + the expected mock base URL + the target's backend). Failing fast
    here stops a misconfigured run from silently hitting a real server. A
    subprocess UI passes its `proc`/`log` so a crashed-on-boot child (already
    exited) is surfaced with its captured log rather than a slow timeout. `_state`-
    free — shared by the session launcher (via `wait_alive`) and a self-managed
    instance (down-host)."""
    timeout = scaled_timeout(timeout)
    deadline = time.monotonic() + timeout
    while True:
        try:
            c = _client_at(target, sock)
            try:
                info = c.identify()
                if _hermetic(target, info, mock_base_url):
                    return
            finally:
                c.close()
        except (OSError, ShedError):
            pass
        if proc is not None and proc.poll() is not None:
            raise RuntimeError(
                f"{target} UI exited early (code {proc.returncode}); see {log}")
        if time.monotonic() >= deadline:
            raise TimeoutError(f"{target} UI not hermetically ready within {timeout}s")
        time.sleep(0.25)


def wait_alive(target: str = DEFAULT_TARGET, *, mock_base_url: str, timeout: float = 30.0) -> None:
    """Session-instance wait: resolve the socket + subprocess bookkeeping from
    `_state` and block until hermetically ready (see `await_hermetic`)."""
    st = _state.get(target)
    await_hermetic(
        target, sock=socket_path(target), mock_base_url=mock_base_url, timeout=timeout,
        proc=st.proc if st else None, log=st.log if st else None,
    )


def _hermetic(target: str, info: dict, mock_base_url: str) -> bool:
    if not (info.get("test_mode") and info.get("mock_base_url") == mock_base_url):
        return False
    return info.get("core") == "rust" and info.get("platform") == _cfg(target).platform_id
