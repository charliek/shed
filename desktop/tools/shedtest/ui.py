"""Launch / quit a hermetic shed-desktop UI for tests, and resolve its socket.

Both UIs speak the same IPC, so the test driver is one client parameterized by
`--target`; only launch/quit/socket differ per UI. mac launches the Swift
`ShedDesktop.app` via `open --env`. The Tauri client is a *subprocess* UI — a
binary run in test mode, pointed at the in-process mock, with a throwaway
HOME/XDG_RUNTIME_DIR — driven through one config-driven path (`_SUBPROC`),
differing from mac in binary, env-var prefix, socket name, and the
`identify.platform` stamp. The harness ALWAYS owns the app it drives.
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

from client import IPCClient, ShedDesktop, ShedError, TauriClient, scaled_timeout

REPO_ROOT = Path(__file__).resolve().parents[2]
TARGETS = ("mac", "tauri")

# mac: the ad-hoc-signed bundle the harness drives.
APP = REPO_ROOT / "build" / "ShedDesktop.app"
DEFAULTS_SUITE = "ai.stridelabs.ShedDesktop.e2e"


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
# a container build can't clobber the mac target dir); let it point the harness at
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


def socket_path(target: str = "mac") -> Path:
    if target == "mac":
        return Path.home() / "Library/Caches/ShedDesktop/shed-desktop.sock"
    cfg = _SUBPROC.get(target)
    if cfg is None:
        raise ValueError(f"unknown target {target!r} (want {'|'.join(TARGETS)})")
    runtime = _state[target].runtime_dir or Path(
        os.environ.get("XDG_RUNTIME_DIR") or f"/tmp/{cfg.fallback_stem}-{os.getuid()}"
    )
    return runtime / cfg.sock_rel


def _client_at(target: str, sock: Path) -> IPCClient:
    """The IPC client class for a target, bound to an EXPLICIT socket path — the
    single source of the target→class map (subprocess targets carry their
    `client_cls` in `_SUBPROC`). `make_client` resolves the socket from session
    state; a self-managed instance (down-host) passes its own."""
    if target == "mac":
        return ShedDesktop(sock)
    cfg = _SUBPROC.get(target)
    if cfg is None:
        raise ValueError(f"unknown target {target!r} (want {'|'.join(TARGETS)})")
    return cfg.client_cls(sock)


def make_client(target: str) -> IPCClient:
    """The IPC client for a target at its session-resolved socket."""
    return _client_at(target, socket_path(target))


def launch_env(target: str) -> dict[str, str]:
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


# -- mac process helpers (unchanged behavior) ----------------------------------
def _running() -> bool:
    return subprocess.run(
        ["pgrep", "-x", "ShedDesktop"],
        stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL,
    ).returncode == 0


def is_alive(target: str = "mac") -> bool:
    try:
        c = make_client(target)
        try:
            c.identify()
            return True
        finally:
            c.close()
    except (OSError, ShedError):
        return False


def _wait_gone(timeout: float) -> bool:
    deadline = time.monotonic() + timeout
    while _running():
        if time.monotonic() >= deadline:
            return False
        time.sleep(0.1)
    return True


def quit(target: str = "mac") -> None:
    """Quit the harness-owned UI. A switch, never flattened: the mac path
    (osascript → pkill → `defaults delete` → unlink sock/lock) and the subprocess
    path (terminate/kill the child → remove its temp dir) share nothing."""
    if target == "mac":
        _quit_mac()
    elif target in _SUBPROC:
        _quit_subproc(target)
    else:
        raise ValueError(f"unknown target {target!r} (want {'|'.join(TARGETS)})")


def _quit_mac() -> None:
    """Force-quit any running ShedDesktop (the harness always owns a hermetic
    instance, so a developer's running app would be force-quit — that is
    intended). Only unlink the socket/lock once the process is CONFIRMED gone:
    unlinking out from under a still-live (wedged) process frees the path and a
    fresh launch would create a second instance."""
    if _running():
        try:
            subprocess.run(
                ["osascript", "-e", 'tell application "ShedDesktop" to quit'],
                check=False, timeout=5,
            )
        except subprocess.TimeoutExpired:
            pass
        if not _wait_gone(3.0):
            subprocess.run(["pkill", "-x", "ShedDesktop"], check=False)         # SIGTERM
            if not _wait_gone(3.0):
                subprocess.run(["pkill", "-9", "-x", "ShedDesktop"], check=False)  # SIGKILL
                if not _wait_gone(5.0):
                    raise RuntimeError(
                        "ShedDesktop survived SIGKILL — refusing to unlink its socket/lock "
                        "(would risk a second instance)")
    cache = Path.home() / "Library/Caches/ShedDesktop"
    (cache / "shed-desktop.sock").unlink(missing_ok=True)
    (cache / "shed-desktop.lock").unlink(missing_ok=True)
    # Drop the throwaway preferences suite so a run never leaves dev defaults.
    subprocess.run(["defaults", "delete", DEFAULTS_SUITE],
                   stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, check=False)


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


def launch(target: str = "mac", *, mock_base_url: str, config_path: Path, state_dir: Path,
           host_agent_socket: str | None = None, unreachable_hosts: tuple[str, ...] = (),
           credential_hosts: tuple[str, ...] = (),
           roost_sockets: dict[str, object] | None = None,
           ssh_bin: object | None = None,
           roost_jail: bool = False,
           roost_install_bin: object | None = None,
           roost_session_bin: object | None = None,
           gx_home: object | None = None,
           gx_timings_ms: str | None = None) -> None:
    """Launch the UI hermetically and block until it answers `identify`.

    `state_dir` is the throwaway per-session dir: on mac SHED_DESKTOP_STATE_DIR;
    on a subprocess target it doubles as HOME + XDG_RUNTIME_DIR (so ~/.shed and
    the runtime socket are both isolated). `host_agent_socket` backs the approval
    gate — set for both mac + tauri (the fake host-agent). `unreachable_hosts` are
    config server NAMES the backend points at a closed port instead of the mock
    (the `<PREFIX>_MOCK_UNREACHABLE_HOSTS` down-host override) so the per-host error
    row is exercisable e2e; wired for both targets, though only tauri drives it now.
    `roost_sockets` maps machine NAME -> the Unix socket its `roost-session`
    answers on, replacing roost's SSH client-bridge with a direct `LocalSession`
    — the test-mode-only `<PREFIX>_ROOST_SOCKETS` seam. Per-machine so a suite
    can serve a session for one and leave another unmapped (which reads as
    unreachable, covering the everyday asleep/off-network state); the implicit
    `localhost` host goes through the same map, so a hermetic run never reads the
    developer's own session. That is what makes the machine path testable
    hermetically: the harness serves a fake `roost-session` there
    (`fake_roost.py`) and the app reaches it through the REAL roost client +
    watcher, with no ssh and no remote host anywhere. Tauri-only (the mac app has
    no machine layer). Set-or-clear like `unreachable_hosts`: `None` (not given)
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
    to `NoSource` (no roost release speaks session protocol 4 yet), which is the
    no-button cell. Set-or-cleared like the rest, so a developer with it exported
    cannot make the NoSource cell pass for the wrong reason.
    `roost_session_bin` is roost's `ROOST_SESSION_BIN` — its own "which daemon do
    I run" override, which the source ladder's SIBLING rung borrows to find a
    roost-session beside the client (plan 019 §3.5 rung 2). Pointing it at a path
    that does not exist is what makes "this machine has no protocol-4 roost"
    deterministic: without it the rung finds whatever `roost-session` happens to
    be on the developer's PATH and the NoSource cell reads differently on a
    workstation than in CI.
    `gx_home` is the fixture `$GROK_HOME` the gx lane's LOCAL credential reader
    looks in (`<PREFIX>_GX_HOME`) — a directory holding a fake `gx-remote*.json`
    record and a `0600` `gx-remote.token`, so the SHIPPED reader (checks and all)
    finds them without the run touching the developer's real `~/.grok`.
    `gx_timings_ms` shrinks the gx adapter's windows
    (`<PREFIX>_GX_TIMINGS_MS`, e.g. `stall=2000,flush_after=300,down_after=6000`)
    so a cell exercises the reconnect ladder in seconds instead of minutes. Both
    are test-mode-only on the app side and set-or-clear here, for the same reason
    `roost_sockets` is: an inherited `GX_HOME` reaching a hermetic launch would
    point the app at a REAL token. Tauri-only.
    `credential_hosts` are server NAMES that keep their REAL control-credential
    wiring against the mock (host agent + the config's auth_mode) instead of the
    tokenless open-mode shortcut — the agent-upgrade scenario's override, mac-only
    for now (the Tauri backend has no counterpart yet).
    """
    if target == "mac":
        _launch_mac(mock_base_url=mock_base_url, config_path=config_path,
                    state_dir=state_dir, host_agent_socket=host_agent_socket,
                    unreachable_hosts=unreachable_hosts,
                    credential_hosts=credential_hosts)
    elif target in _SUBPROC:
        _launch_subproc(target, mock_base_url=mock_base_url, config_path=config_path,
                        runtime_dir=state_dir, host_agent_socket=host_agent_socket,
                        unreachable_hosts=unreachable_hosts,
                        roost_sockets=roost_sockets,
                        ssh_bin=ssh_bin, roost_jail=roost_jail,
                        roost_install_bin=roost_install_bin,
                        roost_session_bin=roost_session_bin,
                        gx_home=gx_home, gx_timings_ms=gx_timings_ms)
    else:
        raise ValueError(f"unknown target {target!r} (want {'|'.join(TARGETS)})")


def _launch_mac(*, mock_base_url: str, config_path: Path, state_dir: Path,
                host_agent_socket: str | None,
                unreachable_hosts: tuple[str, ...] = (),
                credential_hosts: tuple[str, ...] = ()) -> None:
    if platform.system() != "Darwin":
        raise RuntimeError("the mac target requires macOS")
    if not APP.is_dir():
        subprocess.run(["./scripts/bundle.sh", "debug"], cwd=REPO_ROOT, check=True)
    argv = [
        "open", "--env", "SHED_DESKTOP_TEST_MODE=1",
        "--env", f"SHED_DESKTOP_MOCK_BASE_URL={mock_base_url}",
        "--env", f"SHED_DESKTOP_SHED_CONFIG={config_path}",
        "--env", f"SHED_DESKTOP_STATE_DIR={state_dir}",
    ]
    if host_agent_socket:
        argv += ["--env", f"SHED_DESKTOP_HOST_AGENT_SOCKET={host_agent_socket}"]
    if unreachable_hosts:
        argv += ["--env", f"SHED_DESKTOP_MOCK_UNREACHABLE_HOSTS={','.join(unreachable_hosts)}"]
    if credential_hosts:
        argv += ["--env", f"SHED_DESKTOP_MOCK_CREDENTIAL_HOSTS={','.join(credential_hosts)}"]
    # Throwaway UserDefaults suite so preferences never touch the dev's real
    # defaults (the SHED_DESKTOP_STATE_DIR analog for UserDefaults).
    argv += ["--env", f"SHED_DESKTOP_DEFAULTS_SUITE={DEFAULTS_SUITE}"]
    # The Rust core is the default (M0). Forward SHED_DESKTOP_RUST_CORE verbatim
    # whenever the harness set it, so the `=0` Swift-fallback leg is exercised
    # (and an explicit `=1` still works). Unset ⇒ the app defaults to rust.
    rust_core = os.environ.get("SHED_DESKTOP_RUST_CORE")
    if rust_core is not None:
        argv += ["--env", f"SHED_DESKTOP_RUST_CORE={rust_core}"]
    argv += [str(APP)]
    subprocess.run(argv, check=True)
    wait_alive("mac", mock_base_url=mock_base_url)


def subproc_env(cfg: _Subproc, *, runtime_dir: Path, mock_base_url: str,
                config_path: Path, host_agent_socket: str | None = None,
                unreachable_hosts: tuple[str, ...] = (),
                roost_sockets: dict[str, object] | None = None,
                ssh_bin: object | None = None,
                roost_jail: bool = False,
                roost_install_bin: object | None = None,
                roost_session_bin: object | None = None,
                gx_home: object | None = None,
                gx_timings_ms: str | None = None) -> dict[str, str]:
    """The launch env for a subprocess UI — the single source of the subprocess
    env-var contract, shared by the session launcher and a self-managed instance
    (down-host). HOME/XDG_RUNTIME_DIR/XDG_CONFIG_HOME are redirected to the
    throwaway `runtime_dir` (never the dev's ~/.shed, real runtime socket, or real
    prefs), config is pinned to the fixture, test mode + mock are set, and
    `<PREFIX>_SOCKET` is cleared so the XDG default (under runtime_dir) is used."""
    env = dict(os.environ)
    env["HOME"] = str(runtime_dir)
    env["XDG_RUNTIME_DIR"] = str(runtime_dir)
    env["XDG_CONFIG_HOME"] = str(runtime_dir / "config")
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
    #    bootstrap seam). Load-bearing for the same reason GX_HOME is: an
    #    inherited value would point a hermetic run's execs at a real binary.
    #  * ROOST_JAIL — roost's `jail_fs_root`, so a cold-host cell cannot find
    #    the developer's own /usr/bin/roost-session.
    #  * GX_HOME — points the gx lane's LOCAL credential reader at a fixture
    #    $GROK_HOME (a fake record + a 0600 token) instead of the developer's
    #    real ~/.grok. This is the one where the rule is load-bearing rather
    #    than tidy: an inherited value would point the app at a REAL token.
    #  * GX_TIMINGS_MS — shrinks the gx adapter's windows
    #    (`stall=…,resume_window=…,flush_after=…,down_after=…`, in ms) so a cell
    #    does not wait out a thirty-second stall.
    #  * SOCKET — cleared unconditionally so the XDG default (under
    #    runtime_dir) is used.
    #
    # NOTE: there is no roost poll knob to set. Since plan 014 the watcher
    # observes roost's push feed (`events.subscribe`) instead of polling
    # `tab.list`, so a machine-row change arrives when roost commits it — the
    # cadence env var this used to seed was deleted on both sides.
    managed = {
        "MOCK_UNREACHABLE_HOSTS": ",".join(unreachable_hosts) if unreachable_hosts else None,
        "ROOST_SOCKETS": (",".join(f"{n}={p}" for n, p in roost_sockets.items())
                          if roost_sockets else None),
        "SSH_BIN": str(ssh_bin) if ssh_bin else None,
        "ROOST_JAIL": "1" if roost_jail else None,
        "GX_HOME": str(gx_home) if gx_home else None,
        "GX_TIMINGS_MS": gx_timings_ms or None,
        "SOCKET": None,
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
    ):
        if value:
            env[key] = str(value)
        else:
            env.pop(key, None)
    return env


def _launch_subproc(target: str, *, mock_base_url: str, config_path: Path,
                    runtime_dir: Path, host_agent_socket: str | None = None,
                    unreachable_hosts: tuple[str, ...] = (),
                    roost_sockets: dict[str, object] | None = None,
                    ssh_bin: object | None = None,
                    roost_jail: bool = False,
                    roost_install_bin: object | None = None,
                    roost_session_bin: object | None = None,
                    gx_home: object | None = None,
                    gx_timings_ms: str | None = None) -> None:
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
                      roost_session_bin=roost_session_bin,
                      gx_home=gx_home, gx_timings_ms=gx_timings_ms)
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


def wait_alive(target: str = "mac", *, mock_base_url: str, timeout: float = 30.0) -> None:
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
    cfg = _SUBPROC.get(target)
    if cfg is not None:
        return info.get("core") == "rust" and info.get("platform") == cfg.platform_id
    # mac: confirm the active backend matches the flag so a silent Rust->Swift
    # downgrade can't make a run falsely green. Default-on (M0): unset ⇒ rust;
    # only an explicit `=0` selects swift.
    want_core = "swift" if os.environ.get("SHED_DESKTOP_RUST_CORE") == "0" else "rust"
    return info.get("core") == want_core
