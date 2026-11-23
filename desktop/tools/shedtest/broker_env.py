"""The hermetic credential broker every harness-launched app runs.

The app brokers credentials in process: its broker answers ssh, AWS and Docker
requests for every server in its server list, signing with keys and vending
credentials from the HOME it runs under. A harness launch must therefore never
see the developer's agent, keys, credentials or servers. This module writes
everything that broker reads into the launch's own runtime dir and checks the
launch env before the app is spawned:

* `write_hermetic_broker(runtime_dir, servers)` writes `extensions.yaml`
  (`ssh.mode: local-keys`, every namespace delegated to the app with
  `shed-desktop`), a broker server file naming each mock of `servers` on
  loopback, a throwaway ed25519 key, AWS credentials and a Docker `config.json`
  of constants, and returns the env overrides plus a `BrokerInfo`.
* `custom_broker(...)` describes a launch that wrote its own broker files (the
  embedded-broker cells of `test_tauri_broker.py`), for the same checks.
* `apply(env, overrides)` sets or removes each override.
* `validate(env, info)` raises `HermeticityError` unless the env and files are
  hermetic: no `SSH_AUTH_SOCK`; HOME, every `XDG_*_HOME`, `XDG_RUNTIME_DIR`,
  `SHED_BROKER_STATE_DIR`, `SHED_TAURI_EXTENSIONS_CONFIG` and
  `SHED_TAURI_DELIVERY_CONFIG` inside the runtime
  dir; a written `extensions.yaml` with `ssh.mode: local-keys` whose discovery
  source is inside the runtime dir; and every server URL in the broker's files
  `http://127.0.0.1:<port>` of a mock the caller passed.
* `check_running(client, info)` is the post-launch check: `broker.status` lists
  exactly the expected server URLs and (for a written config) `config.source ==
  "loaded"`.

`ui.subproc_env` runs `apply` and `validate` for every launch, so no harness
launch reaches the spawn without them.
"""

from __future__ import annotations

import base64
import json
import os
import re
import shutil
import subprocess
import time
from dataclasses import dataclass, field
from pathlib import Path

from client import scaled_timeout

#: Every namespace the broker can delegate to the app.
ALL_NAMESPACES = ("ssh-agent", "aws-credentials", "docker-credentials")

#: The throwaway AWS profile (constants, as the wire harness's AWS cells use).
AWS_PROFILE = "shed-test"
AWS_KEY = "ASIATESTKEY00000001"
AWS_SECRET = "testSecretAccessKey00000001"
AWS_TOKEN = "testSessionTokenABCDEF00000001"

#: The one registry the broker brokers, and the inline credentials it holds.
DOCKER_REGISTRY = "ghcr.io"
DOCKER_USER = "shedtest-user"
DOCKER_SECRET = "shedtest-secret"

#: Env vars that would point the broker at the developer's own agent or files.
#: Removed from every launch env (and refused by `validate` if present).
_REMOVED = ("SSH_AUTH_SOCK", "AWS_SHARED_CREDENTIALS_FILE", "AWS_CONFIG_FILE",
            "AWS_PROFILE", "DOCKER_CONFIG")

#: The proxy variables the broker's HTTP clients honour (reqwest's system proxy,
#: upper and lower case): an inherited one would route a mock request through a
#: real host. Removed from every launch env, refused by `validate` when non-empty,
#: and every host bypasses any proxy (`NO_PROXY=*`).
_PROXIES = tuple(v for name in ("HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY")
                 for v in (name, name.lower()))

#: The one URL shape a broker server may have: a mock this harness bound.
_MOCK_URL_RE = re.compile(r"http://127\.0\.0\.1:([1-9][0-9]{0,4})")
_SERVER_NAME_RE = re.compile(r"[a-z][a-z0-9-]{0,31}")


class HermeticityError(RuntimeError):
    """A launch env or broker file that could reach something outside the harness."""


@dataclass
class BrokerInfo:
    """What a launch's broker was given, for the checks and the cells."""
    runtime_dir: Path
    #: server name -> mock base URL (`http://127.0.0.1:<port>`).
    servers: dict[str, str]
    #: The broker's server file (its `discovery.source`).
    discovery_path: Path
    #: The `extensions.yaml` the app loads; None when the app synthesizes its default.
    extensions_path: Path | None
    #: The broker's durable audit file (`logging.path`), when one is configured.
    audit_path: Path | None = None
    #: The generated key's SSH wire blob (base64), when a key was generated.
    ssh_blob: str | None = None
    #: True when this module wrote `extensions.yaml` (so it must be byte-equal to
    #: `extensions_text` and `broker.status` must say `loaded`).
    written: bool = False
    #: The `extensions.yaml` this module rendered for the launch.
    extensions_text: str | None = None
    #: Filled by the session fixture: the machine id the session broker sends.
    machine_id: str | None = field(default=None)


def _mock_url(name: str, url: str) -> str:
    """`url` itself, if it is exactly `http://127.0.0.1:<port>` and `name` a plain
    server name; anything else (userinfo, a backslash, a path, another spelling
    of the host) is refused, never normalized."""
    if not isinstance(name, str) or not _SERVER_NAME_RE.fullmatch(name):
        raise HermeticityError(f"broker server name {name!r} is not a plain name")
    m = _MOCK_URL_RE.fullmatch(url) if isinstance(url, str) else None
    if m is None or int(m.group(1)) > 65535:
        raise HermeticityError(
            f"broker server {name!r} is {url!r}: only a mock on http://127.0.0.1:<port> may be named")
    return url


def render_server_file(servers: dict[str, str]) -> str:
    """The broker server file for `servers` (name -> mock URL), byte for byte: the
    only server file `validate` accepts."""
    if not servers:
        return "servers: {}\n"
    lines = ["servers:"]
    for name, url in sorted(servers.items()):
        port = int(_MOCK_URL_RE.fullmatch(_mock_url(name, url)).group(1))
        lines += [f"    {name}:", "        host: 127.0.0.1", f"        http_port: {port}"]
    return "\n".join(lines) + "\n"


def _extensions_yaml(info: BrokerInfo, namespaces: tuple[str, ...],
                     approval_timeout: str | None) -> str:
    rt = info.runtime_dir
    text = "ssh:\n  mode: local-keys\n"
    if "ssh-agent" in namespaces:
        text += "  approval:\n    policy: shed-desktop\n"
    if "aws-credentials" in namespaces:
        text += (
            "aws:\n"
            f"  source_profile: {AWS_PROFILE}\n"
            "  mode: passthrough\n"
            "  approval:\n"
            "    policy: shed-desktop\n"
        )
    if "docker-credentials" in namespaces:
        text += (
            "docker:\n"
            "  registries:\n"
            f"    - {DOCKER_REGISTRY}\n"
            f"  config_path: {rt / '.docker' / 'config.json'}\n"
            "  approval:\n"
            "    policy: shed-desktop\n"
        )
    text += (
        "discovery:\n"
        f"  source: {info.discovery_path}\n"
        '  watch: "off"\n'
        "logging:\n"
        "  enabled: true\n"
        f"  path: {info.audit_path}\n"
    )
    if approval_timeout:
        text += f"approval_timeout: {approval_timeout}\n"
    return text


def _generate_key(ssh_dir: Path) -> str:
    keygen = shutil.which("ssh-keygen")
    if keygen is None:
        raise RuntimeError("the hermetic broker needs ssh-keygen to make its throwaway key")
    ssh_dir.mkdir(parents=True, exist_ok=True, mode=0o700)
    key = ssh_dir / "id_ed25519"
    if not (key.exists() and (ssh_dir / "id_ed25519.pub").exists()):  # a relaunch keeps it
        subprocess.run([keygen, "-t", "ed25519", "-f", str(key), "-N", "", "-q",
                        "-C", "shedtest"], check=True, stdin=subprocess.DEVNULL)
    # `ssh-ed25519 <base64 wire blob> <comment>`: the blob is what a sign request names.
    return (ssh_dir / "id_ed25519.pub").read_text().split()[1]


def _base_overrides(runtime_dir: Path) -> dict[str, str | None]:
    overrides: dict[str, str | None] = {k: None for k in _REMOVED + _PROXIES}
    overrides.update({
        "NO_PROXY": "*",
        "no_proxy": "*",
        "HOME": str(runtime_dir),
        "XDG_RUNTIME_DIR": str(runtime_dir),
        "XDG_CONFIG_HOME": str(runtime_dir / "config"),
        "XDG_STATE_HOME": str(runtime_dir / "state"),
        "XDG_DATA_HOME": str(runtime_dir / "data"),
        "XDG_CACHE_HOME": str(runtime_dir / "cache"),
        "SHED_BROKER_STATE_DIR": str(runtime_dir / "broker-state"),
    })
    return overrides


def write_hermetic_broker(runtime_dir: Path, servers: dict[str, str], *,
                          namespaces: tuple[str, ...] = ALL_NAMESPACES,
                          approval_timeout: str | None = None
                          ) -> tuple[dict[str, str | None], BrokerInfo]:
    """Write the broker's files into `runtime_dir` and return `(overrides, info)`.

    `servers` maps each broker server name to a mock's base URL; `{}` subscribes
    nowhere (a self-managed launch never takes a listener slot on a session mock).
    `namespaces` are the ones delegated to the app; the others get no block (AWS
    and Docker then deny everything)."""
    runtime_dir = Path(runtime_dir)
    servers = {name: _mock_url(name, url) for name, url in servers.items()}
    info = BrokerInfo(
        runtime_dir=runtime_dir, servers=servers,
        discovery_path=runtime_dir / "broker-servers.yaml",
        extensions_path=runtime_dir / "broker-extensions.yaml",
        audit_path=runtime_dir / "broker-audit.jsonl", written=True)
    info.discovery_path.write_text(render_server_file(servers))
    info.extensions_text = _extensions_yaml(info, namespaces, approval_timeout)
    info.extensions_path.write_text(info.extensions_text)
    info.ssh_blob = _generate_key(runtime_dir / ".ssh")
    aws_dir = runtime_dir / ".aws"
    aws_dir.mkdir(exist_ok=True)
    (aws_dir / "credentials").write_text(
        f"[{AWS_PROFILE}]\n"
        f"aws_access_key_id = {AWS_KEY}\n"
        f"aws_secret_access_key = {AWS_SECRET}\n"
        f"aws_session_token = {AWS_TOKEN}\n")
    (aws_dir / "config").write_text("")
    docker_dir = runtime_dir / ".docker"
    docker_dir.mkdir(exist_ok=True)
    auth = base64.b64encode(f"{DOCKER_USER}:{DOCKER_SECRET}".encode()).decode()
    (docker_dir / "config.json").write_text(
        json.dumps({"auths": {DOCKER_REGISTRY: {"auth": auth}}}))
    overrides = _base_overrides(runtime_dir)
    overrides["SHED_TAURI_EXTENSIONS_CONFIG"] = str(info.extensions_path)
    return overrides, info


def custom_broker(runtime_dir: Path, servers: dict[str, str], *,
                  extensions_path: Path | None, audit_path: Path | None = None,
                  ssh_blob: str | None = None) -> tuple[dict[str, str | None], BrokerInfo]:
    """`(overrides, info)` for a launch that wrote its own broker files: an
    `extensions.yaml` at `extensions_path` (None: the app synthesizes its
    default), and the broker's default server file `~/.shed/config.yaml` under
    the runtime HOME, absent or written with `render_server_file`. `validate`
    accepts a custom `extensions.yaml` only in the narrow form `_custom_ok`
    reads: no server, discovery, AWS or Docker key."""
    runtime_dir = Path(runtime_dir)
    servers = {name: _mock_url(name, url) for name, url in servers.items()}
    discovery = runtime_dir / ".shed" / "config.yaml"
    info = BrokerInfo(runtime_dir=runtime_dir, servers=servers, discovery_path=discovery,
                      extensions_path=extensions_path, audit_path=audit_path,
                      ssh_blob=ssh_blob)
    overrides = _base_overrides(runtime_dir)
    overrides["SHED_TAURI_EXTENSIONS_CONFIG"] = (
        str(extensions_path) if extensions_path is not None else None)
    return overrides, info


def apply(env: dict[str, str], overrides: dict[str, str | None]) -> dict[str, str]:
    """Set each override with a value and remove each that is None; returns `env`."""
    for key, value in overrides.items():
        if value is None:
            env.pop(key, None)
        else:
            env[key] = value
    return env


# ---------------------------------------------------------------------------
# The checks
# ---------------------------------------------------------------------------

def _inside(path: str | os.PathLike, root: Path) -> bool:
    try:
        return Path(path).resolve().is_relative_to(root.resolve())
    except (OSError, ValueError):
        return False


#: One line of a custom `extensions.yaml`: a plain key at an even indent, with a
#: plain scalar or nothing after it. Anything else is not understood.
_CUSTOM_LINE_RE = re.compile(r"((?:  )*)([a-z_]+):(?: ([A-Za-z0-9_.-]+))?")
#: The only top-level keys a custom `extensions.yaml` may hold: none of them names
#: a server, a server file or a credential file.
_CUSTOM_TOP = ("ssh",)


def _custom_ok(text: str) -> bool:
    for line in text.splitlines():
        m = _CUSTOM_LINE_RE.fullmatch(line)
        if m is None:
            return False
        if not m.group(1) and m.group(2) not in _CUSTOM_TOP:
            return False
    return True


def validate(env: dict[str, str], info: BrokerInfo) -> None:
    """Raise `HermeticityError` unless this launch env and its broker files are
    hermetic. Runs before every harness spawn (`ui.subproc_env`)."""
    root = info.runtime_dir
    present = [k for k in _REMOVED if k in env]
    if present:
        raise HermeticityError(f"launch env carries {present}: the broker could reach "
                               "the developer's own agent or files")
    proxies = [k for k in _PROXIES if env.get(k)]
    if proxies:
        raise HermeticityError(f"launch env carries proxy settings {proxies}: the broker "
                               "would send its requests through them")
    dirs = {k: v for k, v in env.items()
            if k in ("HOME", "XDG_RUNTIME_DIR", "SHED_BROKER_STATE_DIR")
            or (k.startswith("XDG_") and k.endswith("_HOME"))}
    for key in ("HOME", "XDG_RUNTIME_DIR", "SHED_BROKER_STATE_DIR"):
        if key not in dirs:
            raise HermeticityError(f"launch env has no {key}")
    ext = env.get("SHED_TAURI_EXTENSIONS_CONFIG")
    if ext is not None:
        dirs["SHED_TAURI_EXTENSIONS_CONFIG"] = ext
    delivery = env.get("SHED_TAURI_DELIVERY_CONFIG")
    if delivery is not None:
        dirs["SHED_TAURI_DELIVERY_CONFIG"] = delivery
    outside = {k: v for k, v in dirs.items() if not _inside(v, root)}
    if outside:
        raise HermeticityError(f"outside the runtime dir {root}: {outside}")

    # The broker's files: exactly what this module renders for the launch's own
    # mocks, compared byte for byte. What cannot be proven equal is refused.
    if info.written:
        if ext is None or Path(ext) != info.extensions_path:
            raise HermeticityError(
                f"SHED_TAURI_EXTENSIONS_CONFIG is {ext!r}, not the written {info.extensions_path}")
        if info.extensions_path.read_text() != info.extensions_text:
            raise HermeticityError(
                f"{info.extensions_path} is not the extensions.yaml rendered for this launch")
    elif ext is not None and Path(ext).exists() and not _custom_ok(Path(ext).read_text()):
        raise HermeticityError(
            f"{ext}: a custom extensions.yaml may hold only plain `ssh:` settings")
    want = render_server_file(info.servers)
    if not _inside(info.discovery_path, root):
        raise HermeticityError(f"server file {info.discovery_path} is outside {root}")
    if info.written or info.discovery_path.exists():
        try:
            got = info.discovery_path.read_text()
        except OSError as e:
            raise HermeticityError(f"server file {info.discovery_path}: {e}") from None
        if got != want:
            raise HermeticityError(
                f"{info.discovery_path} is not the server file rendered for this launch's "
                f"mocks {sorted(info.servers.values())}")


def check_running(client, info: BrokerInfo, timeout: float = 30.0) -> dict:
    """The post-launch check: wait until `broker.status` lists exactly the
    expected server URLs (and, for a written config, `config.source == "loaded"`),
    raising at once on any URL that is not one of the launch's mocks. Returns the
    status. It DETECTS a connection to a listener outside the launch's mocks after
    the fact; preventing one is `validate`'s job, before the spawn."""
    expected = set(info.servers.values())
    deadline = time.monotonic() + scaled_timeout(timeout)
    while True:
        status = client.broker_status()
        source = (status.get("config") or {}).get("source")
        urls = {s.get("url") for s in status.get("servers") or []}
        stray = urls - expected
        if stray:
            raise HermeticityError(
                f"the broker serves {sorted(stray)}, which are not this launch's mocks")
        if source == "error":
            raise HermeticityError(f"the broker config failed: {status['config']}")
        if info.written and source not in (None, "loaded"):
            raise HermeticityError(f"broker config source is {source!r}, want 'loaded'")
        if urls == expected and (source == "loaded" or not info.written):
            return status
        if time.monotonic() >= deadline:
            raise HermeticityError(
                f"broker.status did not settle on {sorted(expected)}: "
                f"servers={sorted(u for u in urls if u)} config={status.get('config')}")
        time.sleep(0.25)
