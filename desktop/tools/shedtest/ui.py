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


def make_client(target: str) -> IPCClient:
    """The IPC client for a target — the single source of the target→class map
    (subprocess targets carry their `client_cls` in `_SUBPROC`)."""
    sock = socket_path(target)
    if target == "mac":
        return ShedDesktop(sock)
    cfg = _SUBPROC.get(target)
    if cfg is None:
        raise ValueError(f"unknown target {target!r} (want {'|'.join(TARGETS)})")
    return cfg.client_cls(sock)


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


def _quit_subproc(target: str) -> None:
    st = _state[target]
    if st.proc is not None and st.proc.poll() is None:
        st.proc.terminate()
        try:
            st.proc.wait(timeout=scaled_timeout(5))
        except subprocess.TimeoutExpired:
            st.proc.kill()
    if st.log_fh is not None:
        st.log_fh.close()
    if st.runtime_dir is not None:
        shutil.rmtree(st.runtime_dir, ignore_errors=True)
    _state[target] = _ProcState()


def launch(target: str = "mac", *, mock_base_url: str, config_path: Path, state_dir: Path,
           host_agent_socket: str | None = None) -> None:
    """Launch the UI hermetically and block until it answers `identify`.

    `state_dir` is the throwaway per-session dir: on mac SHED_DESKTOP_STATE_DIR;
    on a subprocess target it doubles as HOME + XDG_RUNTIME_DIR (so ~/.shed and
    the runtime socket are both isolated). `host_agent_socket` backs the approval
    gate — set for both mac + tauri (the fake host-agent).
    """
    if target == "mac":
        _launch_mac(mock_base_url=mock_base_url, config_path=config_path,
                    state_dir=state_dir, host_agent_socket=host_agent_socket)
    elif target in _SUBPROC:
        _launch_subproc(target, mock_base_url=mock_base_url, config_path=config_path,
                        runtime_dir=state_dir, host_agent_socket=host_agent_socket)
    else:
        raise ValueError(f"unknown target {target!r} (want {'|'.join(TARGETS)})")


def _launch_mac(*, mock_base_url: str, config_path: Path, state_dir: Path,
                host_agent_socket: str | None) -> None:
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


def _launch_subproc(target: str, *, mock_base_url: str, config_path: Path,
                    runtime_dir: Path, host_agent_socket: str | None = None) -> None:
    cfg = _SUBPROC[target]
    if not cfg.binary.exists():
        raise RuntimeError(
            f"{target} binary not found at {cfg.binary}; build it first "
            f"(tauri: `make tauri-build`).")
    # HOME/XDG_RUNTIME_DIR redirected to the temp dir (never the dev's ~/.shed or
    # real runtime socket), config pinned to the fixture, test mode + mock set.
    # <PREFIX>_SOCKET is cleared so the XDG default (under runtime_dir) is used.
    env = dict(os.environ)
    env["HOME"] = str(runtime_dir)
    env["XDG_RUNTIME_DIR"] = str(runtime_dir)
    # Redirect the config dir too so tauri's app_config_dir (persisted prefs) is
    # hermetic — else a dev's real $XDG_CONFIG_HOME would be written under test.
    env["XDG_CONFIG_HOME"] = str(runtime_dir / "config")
    env[f"{cfg.env_prefix}_TEST_MODE"] = "1"
    env[f"{cfg.env_prefix}_MOCK_BASE_URL"] = mock_base_url
    env[f"{cfg.env_prefix}_SHED_CONFIG"] = str(config_path)
    # The approval gate's host-agent socket (the fake, in tests).
    if host_agent_socket:
        env[f"{cfg.env_prefix}_HOST_AGENT_SOCKET"] = str(host_agent_socket)
    env.pop(f"{cfg.env_prefix}_SOCKET", None)
    st = _state[target]
    st.env = env
    st.runtime_dir = runtime_dir
    sock = socket_path(target)
    sock.parent.mkdir(parents=True, exist_ok=True)
    st.log = _log_path(cfg)
    st.log_fh = open(st.log, "wb")
    st.proc = subprocess.Popen([str(cfg.binary)], env=env, stdout=st.log_fh, stderr=subprocess.STDOUT)
    wait_alive(target, mock_base_url=mock_base_url)


def wait_alive(target: str = "mac", *, mock_base_url: str, timeout: float = 30.0) -> None:
    """Block until the app answers `identify` AND confirms it's hermetic (test
    mode + the expected mock base URL + the target's backend). Failing fast here
    stops a misconfigured run from silently hitting a real server."""
    timeout = scaled_timeout(timeout)
    deadline = time.monotonic() + timeout
    while True:
        try:
            c = make_client(target)
            try:
                info = c.identify()
                if _hermetic(target, info, mock_base_url):
                    return
            finally:
                c.close()
        except (OSError, ShedError):
            pass
        # A crashed-on-boot subprocess child (already exited) is a hard failure,
        # not a slow boot — surface it (with the captured log) rather than time out.
        st = _state.get(target)
        if st is not None and st.proc is not None and st.proc.poll() is not None:
            raise RuntimeError(
                f"{target} UI exited early (code {st.proc.returncode}); see {st.log}")
        if time.monotonic() >= deadline:
            raise TimeoutError(f"{target} UI not hermetically ready within {timeout}s")
        time.sleep(0.25)


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
