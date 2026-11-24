"""The launch env every harness-launched app runs under, and its check.

The app reads this machine's files to answer provisioning requests (its delivery
providers read keys, tokens and config under HOME), keeps this machine's identity in
its state dir, and talks HTTP to its servers. A harness launch must therefore never
see the developer's agent, keys, credentials, proxies or servers:

* `overrides(runtime_dir)` is the env every launch gets: HOME, every `XDG_*_HOME`,
  `XDG_RUNTIME_DIR` and `SHED_BROKER_STATE_DIR` inside the runtime dir; the
  developer's agent and credential files unset; every proxy unset and bypassed.
* `apply(env, overrides)` sets each override with a value and removes each None.
* `validate(env, runtime_dir)` raises `HermeticityError` unless the env is
  hermetic: none of the removed variables, no proxy, those directories (and
  `SHED_TAURI_DELIVERY_CONFIG` when set) inside the runtime dir, and every mock URL
  in `SHED_TAURI_MOCK_BASE_URL` a `http://127.0.0.1:<port>`.
* `generate_key(ssh_dir)` makes a throwaway ed25519 key (a relaunch keeps it) and
  returns its SSH wire blob.

`ui.subproc_env` runs `apply` and `validate` for every launch, so no harness launch
reaches the spawn without them.
"""

from __future__ import annotations

import os
import re
import shutil
import subprocess
from pathlib import Path

#: Env vars that would point the app at the developer's own agent or files.
_REMOVED = ("SSH_AUTH_SOCK", "AWS_SHARED_CREDENTIALS_FILE", "AWS_CONFIG_FILE",
            "AWS_PROFILE", "DOCKER_CONFIG")

#: The proxy variables the app's HTTP clients honour (upper and lower case): an
#: inherited one would route a mock request through a real host.
_PROXIES = tuple(v for name in ("HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY")
                 for v in (name, name.lower()))

#: The one URL shape a mock may have.
_MOCK_URL_RE = re.compile(r"http://127\.0\.0\.1:([1-9][0-9]{0,4})")
_SERVER_NAME_RE = re.compile(r"[a-z][a-z0-9-]{0,31}")


class HermeticityError(RuntimeError):
    """A launch env that could reach something outside the harness."""


def mock_url(base: str, per_server: dict[str, str] | None = None) -> str:
    """`SHED_TAURI_MOCK_BASE_URL` for a launch: `base` for every server, then a
    `,<server>=<url>` override for each of `per_server` (`shed_app::mock_url_for`)."""
    parts = [base] + [f"{name}={url}" for name, url in sorted((per_server or {}).items())]
    value = ",".join(parts)
    check_mock_url(value)
    return value


def check_mock_url(value: str) -> None:
    """Refuse a mock URL (or override list) naming anything but a loopback mock."""
    parts = value.split(",")
    for i, part in enumerate(parts):
        name, url = (None, part) if i == 0 else part.split("=", 1) if "=" in part else ("", part)
        if name is not None and not _SERVER_NAME_RE.fullmatch(name):
            raise HermeticityError(f"mock override {part!r} does not name a server")
        m = _MOCK_URL_RE.fullmatch(url)
        if m is None or int(m.group(1)) > 65535:
            raise HermeticityError(
                f"mock URL {url!r}: only a mock on http://127.0.0.1:<port> may be named")


def overrides(runtime_dir: Path) -> dict[str, str | None]:
    runtime_dir = Path(runtime_dir)
    out: dict[str, str | None] = {k: None for k in _REMOVED + _PROXIES}
    out.update({
        "NO_PROXY": "*",
        "no_proxy": "*",
        "HOME": str(runtime_dir),
        "XDG_RUNTIME_DIR": str(runtime_dir),
        "XDG_CONFIG_HOME": str(runtime_dir / "config"),
        "XDG_STATE_HOME": str(runtime_dir / "state"),
        "XDG_DATA_HOME": str(runtime_dir / "data"),
        "XDG_CACHE_HOME": str(runtime_dir / "cache"),
        # This machine's identity and credential store (`shed_source::paths`): set
        # explicitly, since on Linux it otherwise follows `$XDG_DATA_HOME` and two
        # launches would share one identity.
        "SHED_BROKER_STATE_DIR": str(runtime_dir / "broker-state"),
    })
    return out


def apply(env: dict[str, str], values: dict[str, str | None]) -> dict[str, str]:
    """Set each override with a value and remove each that is None; returns `env`."""
    for key, value in values.items():
        if value is None:
            env.pop(key, None)
        else:
            env[key] = value
    return env


def _inside(path: str | os.PathLike, root: Path) -> bool:
    try:
        return Path(path).resolve().is_relative_to(root.resolve())
    except (OSError, ValueError):
        return False


def validate(env: dict[str, str], runtime_dir: Path) -> None:
    """Raise `HermeticityError` unless this launch env is hermetic. Runs before
    every harness spawn (`ui.subproc_env`)."""
    root = Path(runtime_dir)
    present = [k for k in _REMOVED if k in env]
    if present:
        raise HermeticityError(f"launch env carries {present}: the app could reach "
                               "the developer's own agent or files")
    proxies = [k for k in _PROXIES if env.get(k)]
    if proxies:
        raise HermeticityError(f"launch env carries proxy settings {proxies}")
    dirs = {k: v for k, v in env.items()
            if k in ("HOME", "XDG_RUNTIME_DIR", "SHED_BROKER_STATE_DIR")
            or (k.startswith("XDG_") and k.endswith("_HOME"))}
    for key in ("HOME", "XDG_RUNTIME_DIR", "SHED_BROKER_STATE_DIR"):
        if key not in dirs:
            raise HermeticityError(f"launch env has no {key}")
    delivery = env.get("SHED_TAURI_DELIVERY_CONFIG")
    if delivery is not None:
        dirs["SHED_TAURI_DELIVERY_CONFIG"] = delivery
    outside = {k: v for k, v in dirs.items() if not _inside(v, root)}
    if outside:
        raise HermeticityError(f"outside the runtime dir {root}: {outside}")
    mock = env.get("SHED_TAURI_MOCK_BASE_URL")
    if not mock:
        raise HermeticityError("launch env has no SHED_TAURI_MOCK_BASE_URL")
    check_mock_url(mock)


def generate_key(ssh_dir: Path) -> str:
    """A throwaway ed25519 key at `ssh_dir/id_ed25519` (kept across a relaunch);
    returns its SSH wire blob."""
    keygen = shutil.which("ssh-keygen")
    if keygen is None:
        raise RuntimeError("a throwaway key needs ssh-keygen")
    ssh_dir = Path(ssh_dir)
    ssh_dir.mkdir(parents=True, exist_ok=True, mode=0o700)
    key = ssh_dir / "id_ed25519"
    if not (key.exists() and (ssh_dir / "id_ed25519.pub").exists()):
        subprocess.run([keygen, "-t", "ed25519", "-f", str(key), "-N", "", "-q",
                        "-C", "shedtest"], check=True, stdin=subprocess.DEVNULL)
    return (ssh_dir / "id_ed25519.pub").read_text().split()[1]
