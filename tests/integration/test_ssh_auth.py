"""Phase 2: SSH public-key allowlist (enforce / warn).

Hermetic: generates throwaway ed25519 keypairs and seeds the on-list one via
`auth.ssh.authorized_keys` (no dependency on the runner holding any particular
private key). The GitHub-seeding path (`github_users`) is covered by Go unit
tests + a manual smoke; here we exercise the allowlist gate itself live.

The auth decision happens during the SSH handshake, before any shed session,
so these probes don't need a real shed — `ssh -v` reports `Authenticated to`
on success and `Permission denied (publickey)` on rejection. VZ-only (config
mutation via the Phase 0.5 harness restarts the local VZ dev server).

The reserved username `git` (no shed may be named git; the user's machine signs
SSH logins for that name by default) is refused at the door in every mode, for a
key that would otherwise authenticate, and a create of a shed named git answers
400 (INVALID_SHED_NAME, "reserved"). A case variant (`Git`) authenticates, as
any username does, and is then answered exactly as an unknown shed is: it is not
a valid shed name, so it is refused before any shed lookup.

SSH enforce is the mtls posture: `auth.ssh.mode: enforce` on an open-mode
server is rejected at startup, and mtls forces enforce itself, so the enforce
test reconfigures the (mtls) dev server's key list. Warn stays an open-mode
setting (mtls forbids an explicit warn override), so the warn test runs the dev
server as an open server on loopback. `dev_config` waits for either mode to be
serving before the SSH probes run.
"""

from __future__ import annotations

import pathlib
import subprocess

import pytest

from fixtures.devcontrol import OPEN_MODE_OVERRIDES, dev_config
from fixtures.server import resolve_server_entry


def _keygen(dir_: pathlib.Path, name: str) -> tuple[str, str]:
    """Generate an ed25519 keypair; return (private_key_path, public_line)."""
    path = dir_ / name
    subprocess.run(
        ["ssh-keygen", "-t", "ed25519", "-N", "", "-f", str(path), "-q"],
        check=True,
    )
    return str(path), (dir_ / f"{name}.pub").read_text().strip()


def _ssh_auth_stderr(port: int, key_path: str, user: str = "probe-shed") -> str:
    """Attempt an SSH connection with `key_path`; return ssh -v stderr.

    Uses a bounded timeout and returns whatever stderr was captured (even on
    timeout), so a post-auth session that stalls can't hide the auth result.
    """
    argv = [
        "ssh", "-v", "-F", "/dev/null", "-i", key_path, "-p", str(port),
        "-o", "IdentitiesOnly=yes", "-o", "IdentityAgent=none",
        "-o", "BatchMode=yes",
        "-o", "StrictHostKeyChecking=no",
        "-o", "UserKnownHostsFile=/dev/null",
        "-o", "ConnectTimeout=10",
        f"{user}@localhost", "true",
    ]
    try:
        r = subprocess.run(argv, capture_output=True, text=True, timeout=25)
        return r.stderr
    except subprocess.TimeoutExpired as e:
        err = e.stderr or ""
        return err.decode() if isinstance(err, bytes) else err


@pytest.mark.vz
@pytest.mark.slow
def test_enforce_denies_offlist_admits_onlist(vz_server_dev, tmp_path):
    server = vz_server_dev.name
    ssh_port = int(resolve_server_entry(server)["ssh_port"])

    on_key, on_pub = _keygen(tmp_path, "onlist")
    off_key, _ = _keygen(tmp_path, "offlist")

    # The base is mtls, which forces auth.ssh.mode to enforce; only the key
    # list changes. Its github_users stay, so the developer's own key (which
    # dev_config's readiness check enrols with) is still allowlisted.
    with dev_config({"auth": {"ssh": {"authorized_keys": [on_pub]}}}, server):
        off = _ssh_auth_stderr(ssh_port, off_key)
        assert "Permission denied" in off, f"off-list key should be denied; stderr={off!r}"

        on = _ssh_auth_stderr(ssh_port, on_key)
        assert "Authenticated to" in on, f"on-list key should authenticate; stderr={on!r}"
        assert "Permission denied" not in on


@pytest.mark.vz
@pytest.mark.slow
def test_warn_admits_offlist(vz_server_dev, tmp_path):
    server = vz_server_dev.name
    ssh_port = int(resolve_server_entry(server)["ssh_port"])

    _, on_pub = _keygen(tmp_path, "onlist")
    off_key, _ = _keygen(tmp_path, "offlist")

    # warn is an open-mode setting: an open server on loopback.
    warn = {**OPEN_MODE_OVERRIDES, "auth": {"mode": "open", "ssh": {"mode": "warn", "authorized_keys": [on_pub]}}}
    with dev_config(warn, server):
        off = _ssh_auth_stderr(ssh_port, off_key)
        # warn logs would-deny but still authenticates.
        assert "Authenticated to" in off, f"warn mode should admit off-list; stderr={off!r}"
        assert "Permission denied" not in off


@pytest.mark.vz
@pytest.mark.slow
def test_enforce_refuses_reserved_username_for_onlist_key(vz_server_dev, tmp_path):
    server = vz_server_dev.name
    ssh_port = int(resolve_server_entry(server)["ssh_port"])

    on_key, on_pub = _keygen(tmp_path, "onlist")

    with dev_config({"auth": {"ssh": {"authorized_keys": [on_pub]}}}, server):
        # Control: the same on-list key authenticates as another username.
        ok = _ssh_auth_stderr(ssh_port, on_key)
        assert "Authenticated to" in ok, f"on-list key should authenticate; stderr={ok!r}"

        git = _ssh_auth_stderr(ssh_port, on_key, user="git")
        assert "Permission denied (publickey)" in git, f"git@ must be refused; stderr={git!r}"
        assert "Authenticated to" not in git


@pytest.mark.vz
@pytest.mark.slow
def test_open_mode_refuses_reserved_username(vz_server_dev, tmp_path):
    server = vz_server_dev.name
    ssh_port = int(resolve_server_entry(server)["ssh_port"])

    key, _ = _keygen(tmp_path, "anykey")

    # An open server on loopback accepts every key, except for git.
    with dev_config(OPEN_MODE_OVERRIDES, server):
        ok = _ssh_auth_stderr(ssh_port, key)
        assert "Authenticated to" in ok, f"open mode should admit any key; stderr={ok!r}"

        git = _ssh_auth_stderr(ssh_port, key, user="git")
        assert "Permission denied (publickey)" in git, f"git@ must be refused; stderr={git!r}"
        assert "Authenticated to" not in git


@pytest.mark.vz
@pytest.mark.slow
def test_case_variant_of_reserved_username_reaches_no_shed(vz_server_dev, tmp_path):
    server = vz_server_dev.name
    ssh_port = int(resolve_server_entry(server)["ssh_port"])

    on_key, on_pub = _keygen(tmp_path, "onlist")

    # `Git` is not the reserved name (the exact match is refused at the
    # door), but it is not a valid shed name either: on a case-insensitive
    # volume a lookup of it would find a shed stored as `git`. The server
    # refuses it before any shed lookup, with exactly the answer an unknown
    # shed gets, so it reveals nothing.
    with dev_config({"auth": {"ssh": {"authorized_keys": [on_pub]}}}, server):
        unknown = _ssh_auth_stderr(ssh_port, on_key, user="no-such-shed-zz")
        assert "shed 'no-such-shed-zz' not found" in unknown, f"control: stderr={unknown!r}"

        cap = _ssh_auth_stderr(ssh_port, on_key, user="Git")
        assert "Error: shed 'Git' not found" in cap, f"Git@ must reach no shed; stderr={cap!r}"


@pytest.mark.vz
@pytest.mark.slow
def test_create_of_reserved_name_is_refused(vz_server_dev):
    r = subprocess.run(
        ["shed", "-s", vz_server_dev.name, "create", "git", "--no-provision"],
        capture_output=True, text=True, timeout=60,
    )
    out = (r.stdout or "") + (r.stderr or "")
    assert r.returncode != 0, f"create of git should fail; output={out!r}"
    assert "reserved" in out, f"the refusal should say the name is reserved; output={out!r}"
