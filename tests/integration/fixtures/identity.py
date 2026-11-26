"""Several people against one dev server: throwaway test identities.

A server with a user list (`auth.users`, mtls only) gives every key to one
named user. The users cases (`test_users.py`, `test_users_upgrade.py`, the
users cases of `test_service_client.py`) need several of them in one run,
none of which may authenticate with the developer's own key. This module
makes them.

**The keys.** One ed25519 key pair per key, generated once (idempotently)
under `$SHED_ITEST_USERS_DIR` (default `~/.shed/dev/itest-users`), outside the
repository, because a server must be STARTED with their public halves:

| user | key file | what it is |
|---|---|---|
| `itest-admin` | `itest-admin/id_ed25519` | an admin |
| `itest-alice` | `itest-alice/id_ed25519` | user A |
| `itest-alice` | `itest-alice/script_ed25519` | user A's key marked `script: true` |
| `itest-bob` | `itest-bob/id_ed25519` | user B |
| `itest-bob` | `itest-bob/expired_ed25519` | user B's key that expired in 2000 |

**The user list.** `with_itest_users(config)` turns a dev server config into
one with that user list. Config load refuses `auth.ssh` key sources beside
`auth.users`, so the config's own `auth.ssh.github_users` and
`authorized_keys` (the developer's) move under a user named `dev`, an admin:
the developer's CLI entry, its certificate and `dev_config()`'s readiness
check keep working, now as `dev`. The same transform renders a config file
from the command line (`scripts/render-dev-config.sh --users DIR` runs it):

    cd tests/integration
    uv run python -m fixtures.identity keys                  # generate the keys
    uv run python -m fixtures.identity users-yaml            # print the itest users
    uv run python -m fixtures.identity render-users IN OUT   # IN with the user list
    uv run python -m fixtures.identity render-directory URL USERS_JSON \\
        [--admin-key-file PUB]...                            # the directory source

**The directory source** (`SHED_ITEST_SOURCE=directory`): the same users
served by an account service (the in-tree reference server) instead of
`auth.users`. `render-directory` prints the server's `auth` block (one local
admin, `localadmin`, with its own itest key and any `--admin-key-file`, and
the directory at URL) and writes the reference server's `users.json` (the
itest users, less the script and expired keys a list never carries). The run
starts both servers so; `users_environment` then does not reconfigure the
server, and the two cases about script and expired keys skip.

**An identity** is one user acting through the real `shed` CLI and the
system `ssh`, `sftp` and `scp`, with:

  - its own `$HOME` (`<keys dir>/<user>/home-<entry>`): its own
    `~/.shed/config.yaml`, client certificate and pinned `known_hosts`,
    never the developer's. It is created when the identity is made (any
    leftover from a crashed run is removed first) and removed at teardown;
    the keys stay.
  - an `ssh` shim first on its `PATH` (`<home>/bin/ssh`): the system ssh
    with `-F /dev/null -i <key> -o IdentitiesOnly=yes -o IdentityAgent=none`
    and no `SSH_AUTH_SOCK`. The CLI runs the system `ssh` with no `-i` and
    with `IdentitiesOnly` unset (sdk/bootstrap, cmd/shed/console.go), so
    without the shim the developer's agent or `~/.ssh/config` would offer the
    developer's key first, and a refusal test could pass or fail as the wrong
    person. `-F /dev/null` also drops the developer's ssh config, so the dev
    entry's host must resolve without it (checked; the identity skips with
    the reason when it does not).

Enrolment is `shed server add <host> --ssh-port <port> --name <entry>
--trust-on-first-use` in the identity's HOME, under the developer's entry
name, so `identity.cli(...)` runs `shed -s <entry> ...` exactly as the
developer would.
"""

from __future__ import annotations

import argparse
import base64
import binascii
import copy
import json
import os
import shlex
import shutil
import stat
import subprocess
import sys
import time
from contextlib import contextmanager, nullcontext
from dataclasses import dataclass, field
from pathlib import Path
from typing import Iterator, Mapping, Optional, Sequence

import pytest
import yaml

ADMIN = "itest-admin"
ALICE = "itest-alice"
BOB = "itest-bob"
# The user the developer's own key sources (auth.ssh) move under.
DEV_USER = "dev"
ITEST_USERS = (ADMIN, ALICE, BOB)

PRIMARY_KEY = "id_ed25519"
SCRIPT_KEY = "script_ed25519"
EXPIRED_KEY = "expired_ed25519"
# A date long gone: the key loads (an expired key is not a config error) and
# is refused at the SSH and TLS handshakes.
EXPIRED_ON = "2000-01-01"


def _default_keys_dir() -> Path:
    raw = os.environ.get("SHED_ITEST_USERS_DIR", "").strip()
    return Path(raw).expanduser() if raw else Path.home() / ".shed" / "dev" / "itest-users"


@dataclass(frozen=True)
class KeySpec:
    """One key of an itest user, as it is written into `auth.users`."""

    user: str
    file: str
    label: str
    script: bool = False
    expires: Optional[str] = None


KEYS: tuple[KeySpec, ...] = (
    KeySpec(ADMIN, PRIMARY_KEY, "itest"),
    KeySpec(ALICE, PRIMARY_KEY, "itest"),
    KeySpec(ALICE, SCRIPT_KEY, "itest-script", script=True),
    KeySpec(BOB, PRIMARY_KEY, "itest"),
    KeySpec(BOB, EXPIRED_KEY, "itest-expired", expires=EXPIRED_ON),
)
ADMINS = frozenset({ADMIN})

# The directory source (SHED_ITEST_SOURCE=directory): the server takes its
# users from an account service (auth.directory), here the in-tree reference
# server, beside one local admin. Not "admin": a service never serves a
# reserved name, so a list entry could never be named like it.
LOCAL_ADMIN = "localadmin"
LOCAL_ADMIN_KEY = KeySpec(LOCAL_ADMIN, PRIMARY_KEY, "itest")
ITEST_SOURCES = ("config", "directory")


# ----------------------------------------------------------------------------
# Keys and the user list
# ----------------------------------------------------------------------------


def key_path(user: str, file: str = PRIMARY_KEY, keys_dir: Optional[Path] = None) -> Path:
    return (keys_dir or _default_keys_dir()) / user / file


def ensure_keys(keys_dir: Optional[Path] = None) -> Path:
    """Generate every itest key that does not exist yet; return the keys dir.

    Idempotent: an existing key is never replaced, because a running server
    may have been started with its public half. A key whose private or public
    half is missing is an error (regenerating it would silently orphan the
    server's copy)."""
    return _ensure(KEYS, keys_dir)


def ensure_local_admin_key(keys_dir: Optional[Path] = None) -> Path:
    """The directory source's local admin key (`localadmin/id_ed25519`),
    generated once like the itest keys; returns its private half's path."""
    base = _ensure((LOCAL_ADMIN_KEY,), keys_dir)
    return key_path(LOCAL_ADMIN, PRIMARY_KEY, base)


def _ensure(specs: Sequence[KeySpec], keys_dir: Optional[Path]) -> Path:
    base = keys_dir or _default_keys_dir()
    base.mkdir(parents=True, exist_ok=True)
    base.chmod(0o700)
    for spec in specs:
        priv = key_path(spec.user, spec.file, base)
        pub = priv.with_name(priv.name + ".pub")
        priv.parent.mkdir(mode=0o700, exist_ok=True)
        if priv.exists() and pub.exists():
            continue
        if priv.exists() or pub.exists():
            raise RuntimeError(
                f"{priv} has only one half of its key pair; remove both halves to regenerate "
                f"(and restart any server whose user list was made from it)"
            )
        r = subprocess.run(
            ["ssh-keygen", "-t", "ed25519", "-N", "", "-q", "-C", f"{spec.user}-{spec.label}", "-f", str(priv)],
            capture_output=True, text=True, timeout=30,
        )
        if r.returncode != 0:
            raise RuntimeError(f"ssh-keygen for {priv} failed: {r.stderr.strip()}")
    return base


def public_key(user: str, file: str = PRIMARY_KEY, keys_dir: Optional[Path] = None) -> str:
    """The `<type> <base64>` of a key (its comment dropped: config load wants
    one key per line and nothing else is needed)."""
    line = key_path(user, file, keys_dir).with_suffix(".pub").read_text(encoding="utf-8").split()
    return " ".join(line[:2])


def itest_users(keys_dir: Optional[Path] = None, names: Sequence[str] = ITEST_USERS) -> dict:
    """The `auth.users` entries for the itest users in `names` (keys must
    exist: `ensure_keys`)."""
    users: dict[str, dict] = {}
    for spec in KEYS:
        if spec.user not in names:
            continue
        u = users.setdefault(spec.user, {"admin": spec.user in ADMINS, "keys": []})
        k: dict = {"key": public_key(spec.user, spec.file, keys_dir), "label": spec.label}
        if spec.script:
            k["script"] = True
        if spec.expires:
            k["expires"] = spec.expires
        u["keys"].append(k)
    return users


def developer_users(config: dict) -> dict:
    """`auth.ssh`'s key sources as `auth.users` entries, all admins: the first
    GitHub login is user `dev` (later ones `dev-2`, ...), and every
    `authorized_keys` line is a key of `dev`, labelled by its comment."""
    ssh = ((config.get("auth") or {}).get("ssh")) or {}
    logins = list(ssh.get("github_users") or [])
    keys = list(ssh.get("authorized_keys") or [])
    if ssh.get("authorized_keys_file"):
        raise ValueError("auth.ssh.authorized_keys_file cannot be moved under a user; list the keys instead")
    if not logins and not keys:
        raise ValueError(
            "the config has no auth.ssh key source to give to the developer's user "
            f"{DEV_USER!r}; without one the developer's own entry would stop working"
        )
    users: dict[str, dict] = {}
    for i, login in enumerate(logins):
        users[DEV_USER if i == 0 else f"{DEV_USER}-{i + 1}"] = {"github": str(login), "admin": True}
    if keys:
        dev = users.setdefault(DEV_USER, {"admin": True})
        dev_keys = dev.setdefault("keys", [])
        seen: set[str] = set()
        for n, line in enumerate(keys, 1):
            fields = str(line).split()
            label = fields[2] if len(fields) > 2 else f"key-{n}"
            if label in seen:
                label = f"{label}-{n}"
            seen.add(label)
            dev_keys.append({"key": " ".join(fields[:2]), "label": label})
    return users


def with_users(config: dict, extra: Mapping[str, dict]) -> dict:
    """A copy of `config` with a user list: `auth.ssh`'s key sources moved
    under `dev` (an admin; `developer_users`), plus `extra`. `auth.ssh` keeps
    anything else it held (`github_refresh`, `max_auth_tries`) and goes when
    nothing is left. The config must be mtls and have no user list yet."""
    cfg = copy.deepcopy(config)
    auth = cfg.get("auth")
    if not isinstance(auth, dict) or auth.get("mode") != "mtls":
        raise ValueError("a user list needs auth.mode: mtls")
    if auth.get("users"):
        raise ValueError("the config already has auth.users")
    users = developer_users(cfg)
    clash = set(users) & set(extra)
    if clash:
        raise ValueError(f"user names used twice: {sorted(clash)}")
    users.update(copy.deepcopy(dict(extra)))
    clients = set((auth.get("clients") or {}))
    if clients & set(users):
        raise ValueError(f"user names that are also service-client names: {sorted(clients & set(users))}")
    ssh = dict(auth.get("ssh") or {})
    for k in ("github_users", "authorized_keys"):
        ssh.pop(k, None)
    if ssh:
        auth["ssh"] = ssh
    else:
        auth.pop("ssh", None)
    auth["users"] = users
    return cfg


def with_itest_users(config: dict, keys_dir: Optional[Path] = None, names: Sequence[str] = ITEST_USERS) -> dict:
    """`with_users` with the itest users in `names` (generating their keys
    first if needed)."""
    ensure_keys(keys_dir)
    return with_users(config, itest_users(keys_dir, names))


def render_users_file(src: Path, dst: Path, keys_dir: Optional[Path] = None) -> dict:
    """Write `src` (a server config file) to `dst` with the itest user list.
    `dst` keeps `src`'s mode. Returns the user list written."""
    with src.open("r", encoding="utf-8") as f:
        cfg = yaml.safe_load(f) or {}
    out = with_itest_users(cfg, keys_dir)
    mode = stat.S_IMODE(src.stat().st_mode)
    tmp = dst.with_name(dst.name + ".tmp")
    with tmp.open("w", encoding="utf-8") as f:
        f.write(
            "# Rendered with the integration suite's itest user list\n"
            "# (tests/integration/fixtures/identity.py). auth.ssh's key sources\n"
            f"# are under the user {DEV_USER!r}, an admin.\n"
        )
        yaml.safe_dump(out, f, sort_keys=False)
    tmp.chmod(mode)
    tmp.replace(dst)
    return out["auth"]["users"]


# ----------------------------------------------------------------------------
# The directory source (SHED_ITEST_SOURCE=directory)
# ----------------------------------------------------------------------------
#
# The same users, served by an account service instead of auth.users: the
# server's auth block names one local admin (`localadmin`) and the directory,
# and the reference server's users.json lists the itest users. A list holds no
# script key (the server never reads `script` from one) and the reference
# server never serves an expired key, so those two keys are left out and the
# two cases about them skip under this source.


def itest_source(environ: Mapping[str, str] = os.environ) -> str:
    """Where the users cases' server takes its users from: `config`
    (auth.users, the default) or `directory` (SHED_ITEST_SOURCE=directory: an
    account service, started so by the run)."""
    source = environ.get("SHED_ITEST_SOURCE", "").strip() or "config"
    if source not in ITEST_SOURCES:
        raise ValueError(f"SHED_ITEST_SOURCE={source!r}: expected one of {', '.join(ITEST_SOURCES)}")
    return source


def skip_reason_under_directory_source(why: str, environ: Mapping[str, str] = os.environ) -> Optional[str]:
    """The reason a users case does not run under the directory source, or
    None when the source is another."""
    if itest_source(environ) != "directory":
        return None
    return f"skipped under the directory source (SHED_ITEST_SOURCE=directory): {why}"


def skip_unless_users_source_ready(backend: str, server_name: str, source: str) -> None:
    """The gate of a users case on a server the run started (no
    reconfiguring): under the config source the server must have been
    started with the itest user list (`SHED_ITEST_USERS`); under the
    directory source `users_environment` asks the server itself."""
    if source == "config":
        skip_unless_started_with_users(backend, server_name)


def directory_source_problem(r: subprocess.CompletedProcess, server_name: str) -> Optional[str]:
    """Why `shed --json users status` (as the developer's entry, `r`) does
    not show a server whose users come from the directory, or None when it
    does."""
    if r.returncode != 0:
        return (f"`shed -s {server_name} --json users status` failed (exit {r.returncode}): "
                f"{(r.stdout + r.stderr).strip()[-800:]!r}")
    try:
        status = json.loads(r.stdout)
    except json.JSONDecodeError:
        return f"`shed -s {server_name} --json users status` printed no JSON: {r.stdout[-800:]!r}"
    source = status.get("source") if isinstance(status, dict) else None
    if source != "directory":
        return (f"{server_name!r} takes its users from {source!r}, not 'directory': under "
                f"SHED_ITEST_SOURCE=directory the run must start it with render-directory's auth block")
    return None


_PUBLIC_KEY_TYPES = frozenset({
    "ssh-ed25519", "ssh-rsa", "ecdsa-sha2-nistp256", "ecdsa-sha2-nistp384", "ecdsa-sha2-nistp521",
    "sk-ssh-ed25519@openssh.com", "sk-ecdsa-sha2-nistp256@openssh.com",
})


def public_key_line(text: str, where: str) -> str:
    """The `<type> <base64>` of the one public key `text` holds (an
    authorized_keys line or a .pub file, comment dropped). Refused, with a
    message naming `where`: a private key, anything but exactly one line,
    an unknown type, a body that is not base64 or does not open with its
    type."""
    if "PRIVATE KEY" in text:
        raise ValueError(f"{where}: this is a private key; give its public half (the .pub file)")
    lines = text.splitlines()
    if len(lines) != 1 or not lines[0].strip():
        raise ValueError(f"{where}: expected exactly one line holding a public key, found {len(lines)} lines")
    fields = lines[0].split()
    if len(fields) < 2 or fields[0] not in _PUBLIC_KEY_TYPES:
        raise ValueError(f"{where}: not a public key line ('<type> <base64> [comment]'): {lines[0][:60]!r}")
    typ, body = fields[0], fields[1]
    try:
        blob = base64.b64decode(body, validate=True)
    except (ValueError, binascii.Error):
        raise ValueError(f"{where}: the key's body is not base64") from None
    n = int.from_bytes(blob[:4], "big") if len(blob) >= 4 else -1
    if blob[4:4 + n] != typ.encode("ascii") or len(blob) <= 4 + n:
        raise ValueError(f"{where}: the key's body is not a {typ} key")
    return f"{typ} {body}"


def directory_users(keys_dir: Optional[Path] = None, names: Sequence[str] = ITEST_USERS) -> list[dict]:
    """The reference server's users.json for the itest users in `names`: each
    with its admin flag and its keys, less script and expired keys, sorted
    by name (keys must exist: `ensure_keys`)."""
    out: dict[str, dict] = {}
    for spec in KEYS:
        if spec.user not in names or spec.script or spec.expires:
            continue
        u = out.setdefault(spec.user, {"name": spec.user, "admin": spec.user in ADMINS, "keys": []})
        u["keys"].append({"key": public_key(spec.user, spec.file, keys_dir), "label": spec.label})
    return [out[n] for n in sorted(out)]


def _loopback_host(host: str) -> bool:
    import ipaddress

    if host == "localhost":
        return True
    try:
        return ipaddress.ip_address(host).is_loopback
    except ValueError:
        return False


def directory_auth(url: str, admin_keys: Sequence[str], *, refresh: str = "1m", trust: str = "2m") -> dict:
    """The server's `auth` block for the directory source: mtls, one local
    admin (`localadmin`) holding `admin_keys` (each the text of one public
    key, `public_key_line`), and the directory at `url`, used as written:
    nothing is lower-cased, so an upper-case scheme or host is refused.
    Plain HTTP is allowed only to a loopback host (the reference server); an
    https URL never sets it."""
    from urllib.parse import urlsplit

    if not (url.startswith("http://") or url.startswith("https://")):
        raise ValueError(f"{url!r} is not an http(s) origin (the scheme in lower case)")
    parts = urlsplit(url)
    if not parts.hostname:
        raise ValueError(f"{url!r} is not an http(s) origin")
    if parts.path not in ("", "/") or parts.query or parts.fragment or parts.username or parts.password:
        raise ValueError(f"{url!r} is not an origin (no path, query, fragment or user info)")
    netloc = parts.netloc
    host = netloc[:netloc.index("]") + 1] if netloc.startswith("[") else netloc.split(":", 1)[0]
    if host != host.lower():
        raise ValueError(f"{url!r}: write the host in lower case")
    plain = parts.scheme == "http"
    if plain and not _loopback_host(parts.hostname):
        raise ValueError(f"{url!r}: plain HTTP is for a loopback reference server only")
    if not admin_keys:
        raise ValueError("the local admin needs at least one key")
    keys = []
    for n, text in enumerate(admin_keys, 1):
        keys.append({"key": public_key_line(str(text), f"admin key {n}"),
                     "label": "itest" if n == 1 else f"itest-{n}"})
    directory: dict = {"url": url.rstrip("/"), "refresh": refresh, "trust": trust}
    if plain:
        directory["allow_plain_http"] = True
    return {"mode": "mtls", "users": {LOCAL_ADMIN: {"admin": True, "keys": keys}}, "directory": directory}


def render_directory(url: str, users_json: Path, keys_dir: Optional[Path] = None,
                     admin_key_files: Sequence[Path] = (), *, refresh: str = "1m", trust: str = "2m") -> dict:
    """Write the reference server's users.json (mode 0600) to `users_json`
    and return the server's `auth` block. The local admin holds the
    `localadmin` itest key and each public key in `admin_key_files` (the
    developer's own, so their CLI entry is a local admin and can clean up)."""
    base = ensure_keys(keys_dir)
    admin = ensure_local_admin_key(base)
    files = [admin.with_name(admin.name + ".pub"), *(Path(p).expanduser() for p in admin_key_files)]
    lines = [public_key_line(f.read_text(encoding="utf-8"), str(f)) for f in files]
    auth = directory_auth(url, lines, refresh=refresh, trust=trust)
    data = json.dumps(directory_users(base), indent=1).encode() + b"\n"
    tmp = users_json.with_name(f".{users_json.name}.tmp")
    fd = os.open(tmp, os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o600)
    try:
        try:
            os.fchmod(fd, 0o600)
            os.write(fd, data)
        finally:
            os.close(fd)
        tmp.replace(users_json)
    except BaseException:
        tmp.unlink(missing_ok=True)
        raise
    return auth


# ----------------------------------------------------------------------------
# One identity
# ----------------------------------------------------------------------------


_SHIM = """#!/bin/sh
# Integration-suite identity {user!s}: the system ssh with only this key.
unset SSH_AUTH_SOCK
exec {ssh} -F /dev/null -i {key} \\
  -o IdentitiesOnly=yes -o IdentityAgent=none -o BatchMode=yes \\
  -o PasswordAuthentication=no -o KbdInteractiveAuthentication=no \\
  -o ControlMaster=no -o ControlPath=none -o UpdateHostKeys=no \\
  "$@"
"""


def _real_ssh() -> str:
    path = shutil.which("ssh")
    if not path:
        raise RuntimeError("ssh is not on PATH")
    return path


def shim_text(user: str, key: Path, ssh: str) -> str:
    """The `ssh` shim an identity puts first on its PATH (split out so the
    unit tests can run it)."""
    return _SHIM.format(user=user, ssh=shlex.quote(ssh), key=shlex.quote(str(key)))


@dataclass
class Identity:
    """One user of a dev server with a user list, acting through the real CLI
    and the system ssh with exactly one key.

    `entry` is the developer's CLI entry name for the server, used for the
    identity's own entry too; `host` and `ssh_port` come from it."""

    user: str
    key: Path
    home: Path
    entry: str
    host: str
    ssh_port: int
    shim: Path
    # Extra identities that share this one's HOME (another key of the user).
    _owns_home: bool = field(default=True, repr=False)

    # -- construction ------------------------------------------------------

    @classmethod
    def create(cls, user: str, entry: str, host: str, ssh_port: int, *,
               key_file: str = PRIMARY_KEY, keys_dir: Optional[Path] = None) -> "Identity":
        """A new identity with a fresh HOME (a leftover one from a crashed run
        is removed first: the directory is the fixture's own)."""
        key = key_path(user, key_file, keys_dir)
        if not key.exists():
            raise RuntimeError(f"no key at {key}; run ensure_keys() first")
        home = key.parent / f"home-{entry}"
        if home.exists():
            shutil.rmtree(home)
        home.mkdir(mode=0o700)
        ident = cls(user=user, key=key, home=home, entry=entry, host=host, ssh_port=int(ssh_port),
                    shim=home / "bin" / "ssh")
        ident._write_shim()
        return ident

    def with_key(self, key_file: str) -> "Identity":
        """Another key of the same user, sharing this identity's HOME (its
        CLI entry and pinned known_hosts) through its own shim."""
        key = self.key.parent / key_file
        if not key.exists():
            raise RuntimeError(f"no key at {key}")
        other = Identity(user=self.user, key=key, home=self.home, entry=self.entry, host=self.host,
                         ssh_port=self.ssh_port, shim=self.home / f"bin-{key_file}" / "ssh", _owns_home=False)
        other._write_shim()
        return other

    def _write_shim(self) -> None:
        self.shim.parent.mkdir(parents=True, exist_ok=True)
        self.shim.write_text(shim_text(self.user, self.key, _real_ssh()), encoding="utf-8")
        self.shim.chmod(0o755)

    def remove(self) -> None:
        """Remove what the identity added: its HOME (the keys stay)."""
        if self._owns_home and self.home.exists():
            shutil.rmtree(self.home, ignore_errors=True)

    # -- environment -------------------------------------------------------

    @property
    def known_hosts(self) -> Path:
        return self.home / ".shed" / "known_hosts"

    def env(self) -> dict:
        env = {k: v for k, v in os.environ.items() if k != "SSH_AUTH_SOCK"}
        env["HOME"] = str(self.home)
        env["PATH"] = f"{self.shim.parent}{os.pathsep}{os.environ.get('PATH', '')}"
        env["LC_ALL"] = "C"
        return env

    # -- the CLI -----------------------------------------------------------

    def shed(self, *args: str, timeout: float = 120, input: Optional[str] = None) -> subprocess.CompletedProcess:
        """`shed <args>` as this identity (no `-s`)."""
        return subprocess.run(
            ["shed", *args], capture_output=True, text=True, timeout=timeout, env=self.env(),
            input=input, stdin=None if input is not None else subprocess.DEVNULL,
        )

    def cli(self, *args: str, timeout: float = 120) -> subprocess.CompletedProcess:
        """`shed -s <entry> <args>` as this identity."""
        return self.shed("-s", self.entry, *args, timeout=timeout)

    def popen_cli(self, *args: str) -> subprocess.Popen:
        """`shed -s <entry> <args>` as this identity, in the background."""
        return subprocess.Popen(
            ["shed", "-s", self.entry, *args], stdout=subprocess.PIPE, stderr=subprocess.PIPE,
            stdin=subprocess.DEVNULL, text=True, env=self.env(),
        )

    def add_server(self, timeout: float = 90) -> None:
        """Enrol: `shed server add` over SSH in this identity's HOME, trusting
        the server's host key on first use. Raises with the CLI's output and a
        hint when the server refuses the key."""
        r = self.shed(
            "server", "add", self.host, "--ssh-port", str(self.ssh_port),
            "--name", self.entry, "--trust-on-first-use", timeout=timeout,
        )
        if r.returncode != 0 or self.entry_config() is None:
            raise AssertionError(
                f"identity {self.user} could not enrol with {self.entry} "
                f"({self.host}:{self.ssh_port}) using {self.key} (exit {r.returncode}).\n"
                f"stdout={r.stdout[-1500:]!r}\nstderr={r.stderr[-1500:]!r}\n"
                f"The server must have been started with the itest user list made from the keys "
                f"in {self.key.parent.parent} (print it with "
                f"`cd tests/integration && uv run python -m fixtures.identity users-yaml`)."
            )

    def entry_config(self) -> Optional[dict]:
        """This identity's own CLI entry (`<home>/.shed/config.yaml`)."""
        path = self.home / ".shed" / "config.yaml"
        try:
            data = yaml.safe_load(path.read_text(encoding="utf-8")) or {}
        except (OSError, yaml.YAMLError):
            return None
        entry = (data.get("servers") or {}).get(self.entry)
        return entry if isinstance(entry, dict) else None

    def list_sheds(self) -> list[dict]:
        """`shed --json list` as this identity (fail-loud)."""
        r = self.shed("--json", "-s", self.entry, "list", timeout=60)
        if r.returncode != 0:
            raise AssertionError(f"{self.user}: shed --json list failed (exit {r.returncode}): {r.stderr[-800:]!r}")
        sheds = json.loads(r.stdout or "[]")
        assert isinstance(sheds, list), r.stdout
        return sheds

    # -- the HTTPS API -----------------------------------------------------

    def api(self):
        """A pinned client with this identity's CLI credential (control scope)."""
        from urllib.parse import urlparse

        from .tlsclient import Pinned

        e = self.entry_config()
        if not e or not e.get("client_cert_file"):
            raise AssertionError(f"{self.user} has no mtls entry {self.entry!r} with a client certificate")
        url = urlparse(e["api_url"])
        return Pinned(url.hostname, url.port, e["tls_cert_fingerprint"], e["client_cert_file"], e["client_key_file"])

    def enrol(self, scope: str, directory: Path, client_kind: str = "cli"):
        """A certificate of `scope` over `_bootstrap`, as this identity
        (`devcontrol.bootstrap_cert` with this identity's ssh and known_hosts).
        Raises AssertionError with the server's refusal."""
        from .devcontrol import bootstrap_cert

        return bootstrap_cert(self.entry, scope, directory, client_kind, ssh=str(self.shim),
                              known_hosts=self.known_hosts)

    def pinned(self, cert) -> "object":
        """A pinned client presenting `cert` (a `BootstrapCert` from `enrol`)."""
        from .tlsclient import Pinned

        return Pinned(self.host, cert.https_port, cert.pin, str(cert.cert_path), str(cert.key_path))

    # -- raw ssh, sftp, scp -----------------------------------------------

    def _ssh_opts(self) -> list[str]:
        return [
            "-o", f"UserKnownHostsFile={self.known_hosts}",
            "-o", "StrictHostKeyChecking=yes",
            "-o", "ConnectTimeout=10",
        ]

    def ssh_argv(self, shed: str, command: Optional[str] = None, *extra: str) -> list[str]:
        argv = [str(self.shim), "-T", "-p", str(self.ssh_port), *self._ssh_opts(), *extra, f"{shed}@{self.host}"]
        if command is not None:
            argv.append(command)
        return argv

    def ssh(self, shed: str, command: str, timeout: float = 90) -> subprocess.CompletedProcess:
        """A session in `shed` running the raw `command`, as this identity."""
        return subprocess.run(self.ssh_argv(shed, command), capture_output=True, text=True,
                              timeout=timeout, env=self.env(), stdin=subprocess.DEVNULL)

    def forward(self, shed: str, port: int = 22, timeout: float = 20) -> tuple[Optional[int], bytes, str]:
        """Open one forwarded connection (a direct-tcpip channel, `ssh -W`) to
        `localhost:<port>` in `shed`; return (exit code, or None when ssh had
        to be killed, the first line the far side sent (at most 64 bytes),
        stderr). The guest's sshd speaks first, so a forward that reaches it
        reads `SSH-`.

        stdin stays OPEN until that first line arrives (or `timeout`): with
        stdin at EOF from the start, ssh sends EOF down the channel at once
        and the server closes the forward when either direction finishes, so
        it could be torn down before the guest's banner is read. Once the
        line is in, stdin is closed and ssh is given a few seconds to exit."""
        return _first_line_over(
            [str(self.shim), "-p", str(self.ssh_port), *self._ssh_opts(),
             "-W", f"localhost:{port}", f"{shed}@{self.host}"],
            env=self.env(), timeout=timeout,
        )

    def sftp(self, shed: str, batch: str, timeout: float = 60) -> subprocess.CompletedProcess:
        """`sftp -b -` into `shed` with `batch` on stdin, over this identity's shim."""
        argv = ["sftp", "-S", str(self.shim), "-P", str(self.ssh_port), *self._ssh_opts(),
                "-b", "-", f"{shed}@{self.host}"]
        return subprocess.run(argv, capture_output=True, text=True, timeout=timeout, env=self.env(), input=batch)

    def scp_to(self, shed: str, local: Path, remote: str, timeout: float = 60) -> subprocess.CompletedProcess:
        """`scp <local> <shed>@host:<remote>` over this identity's shim."""
        argv = ["scp", "-S", str(self.shim), "-P", str(self.ssh_port), *self._ssh_opts(),
                str(local), f"{shed}@{self.host}:{remote}"]
        return subprocess.run(argv, capture_output=True, text=True, timeout=timeout, env=self.env(),
                              stdin=subprocess.DEVNULL)


def _first_line_over(argv: Sequence[str], *, env: Mapping[str, str], timeout: float,
                     max_bytes: int = 64) -> tuple[Optional[int], bytes, str]:
    """Run `argv` with its stdin held open; read its stdout until a newline,
    `max_bytes`, EOF or `timeout`; then close stdin and reap it (killing it
    after 5 s more). Returns (exit code or None when it was killed, what was
    read, stderr). Split out of `Identity.forward` for its unit test."""
    import selectors

    proc = subprocess.Popen(list(argv), stdin=subprocess.PIPE, stdout=subprocess.PIPE,
                            stderr=subprocess.PIPE, env=dict(env))
    got = b""
    deadline = time.monotonic() + timeout
    try:
        sel = selectors.DefaultSelector()
        sel.register(proc.stdout, selectors.EVENT_READ)
        try:
            while b"\n" not in got and len(got) < max_bytes:
                left = deadline - time.monotonic()
                if left <= 0 or not sel.select(left):
                    break
                chunk = os.read(proc.stdout.fileno(), max_bytes)
                if not chunk:
                    break  # EOF: the forward was refused or closed
                got += chunk
        finally:
            sel.close()
    finally:
        try:
            proc.stdin.close()
        except OSError:
            pass
        killed = False
        try:
            _, err = proc.communicate(timeout=5)
        except subprocess.TimeoutExpired:
            killed = True
            proc.kill()
            _, err = proc.communicate()
    line = got.split(b"\n", 1)[0][:max_bytes]
    return (None if killed else proc.returncode), line, err.decode("utf-8", errors="replace")


# ----------------------------------------------------------------------------
# The identities of one run against one server
# ----------------------------------------------------------------------------


def _env_backends(name: str) -> set[str]:
    return {b.strip() for b in os.environ.get(name, "").split(",") if b.strip()}


def started_with_users(backend: str) -> bool:
    """Whether `SHED_ITEST_USERS` (a comma list of backends, `fc`) says the dev
    server of `backend` was started with the itest user list. (The Mac dev
    server is always restarted with it by the suite itself; a remote server
    whose machines come from the account service takes its people from it.)"""
    return backend in _env_backends("SHED_ITEST_USERS")


def skip_unless_started_with_users(backend: str, server_name: str) -> None:
    if started_with_users(backend):
        return
    how = {
        "fc": "SHED_DEV_USERS=itest make dev-server-restart-fc (or render a config with "
              "`scripts/render-dev-config.sh <base> <out> --users <keys dir>`)",
    }.get(backend, "scripts/render-dev-config.sh <base> <out> --users <keys dir>")
    pytest.skip(
        f"the users cases on [{backend}] need {server_name!r} STARTED with the itest user list "
        f"(keys in {_default_keys_dir()}): start it with {how}, then set SHED_ITEST_USERS={backend}"
    )


@dataclass
class UsersEnv:
    """The identities of one run against one dev server with the itest user
    list. `server` is the developer's own view of it (user `dev`, an admin),
    for clean-up only; `image` is what every user's create asks for (an image
    alias, so a non-admin may use it; None = the server default)."""

    backend: str
    server: object
    alice: Identity
    bob: Identity
    admin: Identity
    alice_script: Identity
    bob_expired: Identity
    image: Optional[str]
    keys_dir: Path

    @property
    def people(self) -> tuple[Identity, Identity, Identity]:
        return self.alice, self.bob, self.admin


def _check_host_resolves_without_ssh_config(host: str) -> None:
    """The shims run ssh with `-F /dev/null`: a host that only the developer's
    ssh config resolves (a `Host` alias with another `HostName`) would not be
    reached, and the host key the CLI pinned would not match. Skip with the
    reason instead of failing for it."""
    try:
        r = subprocess.run(["ssh", "-G", host], capture_output=True, text=True, timeout=10)
    except (OSError, subprocess.TimeoutExpired):
        return
    for line in r.stdout.splitlines():
        k, _, v = line.partition(" ")
        if k == "hostname" and v.strip() and v.strip() != host:
            pytest.skip(
                f"the dev entry's host {host!r} is an ssh-config alias for {v.strip()!r}; the test identities "
                f"run ssh without the developer's ssh config, so register the dev server by a name that resolves "
                f"on its own"
            )


@contextmanager
def users_environment(
    backend: str,
    server,
    *,
    reconfigure: bool,
    image: Optional[str],
    keys_dir: Optional[Path] = None,
    source: str = "config",
) -> Iterator[UsersEnv]:
    """Enrol the itest identities against `server` (a LocalServer,
    RemoteServer or CloudServer for a dev entry), yield them, then remove
    their HOMEs.

    `reconfigure=True` (the Mac dev server only) restarts the server with the
    itest user list through `devcontrol.dev_config()` and restores the
    committed base afterwards. Otherwise the server must already run with the
    user list (`SHED_ITEST_USERS`).

    `source` is the caller's choice, never read from the environment here:
    `config` (the default, the behaviour above) or `directory`, which a
    caller that follows SHED_ITEST_SOURCE (`itest_source()`) passes. Under the
    directory source the run started the server with `render-directory`'s
    auth block and the reference server with its users.json: the server is
    never reconfigured (asking for both is an error), the developer's entry
    (a local admin) must see `shed users status` say the users come from the
    directory, and it runs `shed users refresh` first, so the list is current
    and no identity's first login goes through the unknown-key pull (at most
    one a minute)."""
    from .devcontrol import dev_config
    from .server import resolve_server_entry

    if source not in ITEST_SOURCES:
        raise ValueError(f"source {source!r}: expected one of {', '.join(ITEST_SOURCES)}")
    if reconfigure and source == "directory":
        raise ValueError("users_environment: reconfigure=True restarts the server with auth.users, "
                         "which the directory source never does: a caller that needs its own "
                         "configuration skips under the directory source")
    base = ensure_keys(keys_dir)
    entry = resolve_server_entry(server.name, env=getattr(server, "env", None))
    if entry is None:
        pytest.skip(f"{server.name!r} is not in ~/.shed/config.yaml")
    host = entry.get("host", "localhost")
    ssh_port = int(entry.get("ssh_port") or 0)
    if ssh_port <= 0:
        pytest.skip(f"{server.name!r} has no ssh_port in ~/.shed/config.yaml")
    _check_host_resolves_without_ssh_config(host)

    if source == "directory":
        # The server's own word first: a server that takes its users from its
        # config would let these identities in too, and prove nothing here.
        problem = directory_source_problem(server.cli("--json", "users", "status", timeout=60), server.name)
        if problem:
            pytest.fail(f"directory source: {problem}")
        r = server.cli("users", "refresh", timeout=120)
        if r.returncode != 0:
            pytest.fail(
                f"directory source: `shed -s {server.name} users refresh` failed (exit {r.returncode}): "
                f"{(r.stdout + r.stderr).strip()[-800:]!r}. The entry must be a local admin "
                f"(render-directory --admin-key-file <its public key>) and the server must reach the "
                f"reference server serving render-directory's users.json"
            )
    ctx = (
        dev_config({}, server.name, transform=lambda c: with_itest_users(c, base))
        if reconfigure else nullcontext()
    )
    with ctx:
        made: list[Identity] = []
        try:
            for user in ITEST_USERS:
                made.append(Identity.create(user, server.name, host, ssh_port, keys_dir=base))
            alice, bob, admin = (next(i for i in made if i.user == u) for u in (ALICE, BOB, ADMIN))
            for ident in (alice, bob, admin):
                ident.add_server()
            yield UsersEnv(
                backend=backend, server=server, alice=alice, bob=bob, admin=admin,
                alice_script=alice.with_key(SCRIPT_KEY), bob_expired=bob.with_key(EXPIRED_KEY),
                image=image, keys_dir=base,
            )
        finally:
            for ident in made:
                ident.remove()


# ----------------------------------------------------------------------------
# Command line (scripts/render-dev-config.sh --users runs render-users)
# ----------------------------------------------------------------------------


def main(argv: Optional[Sequence[str]] = None) -> int:
    p = argparse.ArgumentParser(prog="python -m fixtures.identity", description=__doc__.split("\n\n")[0])
    p.add_argument("--dir", type=Path, default=None,
                   help="keys dir (default $SHED_ITEST_USERS_DIR or ~/.shed/dev/itest-users)")
    sub = p.add_subparsers(dest="cmd", required=True)
    sub.add_parser("keys", help="generate the itest keys that do not exist yet")
    sub.add_parser("users-yaml", help="print the itest users as an auth.users block")
    r = sub.add_parser("render-users", help="write a server config with the itest user list")
    r.add_argument("src", type=Path)
    r.add_argument("dst", type=Path)
    d = sub.add_parser(
        "render-directory",
        help="print the server's auth block for the directory source and write the reference server's users.json",
    )
    d.add_argument("url", help="the account service's origin (the reference server's 'listening on' URL)")
    d.add_argument("users_json", type=Path, help="where to write users.json (the reference server's control dir)")
    d.add_argument("--admin-key-file", dest="admin_key_files", type=Path, action="append", default=[],
                   help="a public key the local admin also holds (repeatable; the developer's, for their CLI entry)")
    d.add_argument("--refresh", default="1m")
    d.add_argument("--trust", default="2m")
    a = p.parse_args(argv)
    try:
        base = ensure_keys(a.dir.expanduser() if a.dir else None)
        if a.cmd == "keys":
            print(f"itest keys in {base}")
        elif a.cmd == "users-yaml":
            sys.stdout.write("# under auth: (beside mode: mtls); add the developer's own user too\n")
            yaml.safe_dump({"users": itest_users(base)}, sys.stdout, sort_keys=False)
        elif a.cmd == "render-users":
            users = render_users_file(a.src, a.dst, base)
            print(f"wrote {a.dst} with users {', '.join(users)} (keys in {base})")
        elif a.cmd == "render-directory":
            auth = render_directory(a.url, a.users_json, base, a.admin_key_files, refresh=a.refresh, trust=a.trust)
            sys.stdout.write(
                f"# The server's auth block for the directory source (SHED_ITEST_SOURCE=directory).\n"
                f"# The local admin {LOCAL_ADMIN!r} holds {key_path(LOCAL_ADMIN, PRIMARY_KEY, base)}.pub.\n"
                f"# Wrote {a.users_json} (the itest users less script and expired keys).\n"
            )
            yaml.safe_dump({"auth": auth}, sys.stdout, sort_keys=False)
    except (OSError, ValueError, RuntimeError, yaml.YAMLError) as e:
        print(f"fixtures.identity: {e}", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
