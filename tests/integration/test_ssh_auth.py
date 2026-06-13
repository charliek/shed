"""Phase 2: SSH public-key allowlist (enforce / warn).

Hermetic: generates throwaway ed25519 keypairs and seeds the on-list one via
`auth.ssh.authorized_keys` (no dependency on the runner holding any particular
private key). The GitHub-seeding path (`github_users`) is covered by Go unit
tests + a manual smoke; here we exercise the allowlist gate itself live.

The auth decision happens during the SSH handshake, before any shed session,
so these probes don't need a real shed — `ssh -v` reports `Authenticated to`
on success and `Permission denied (publickey)` on rejection. VZ-only (config
mutation via the Phase 0.5 harness restarts the local VZ dev server).
"""

from __future__ import annotations

import pathlib
import subprocess

import pytest

from fixtures.devcontrol import dev_config
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
        "ssh", "-v", "-i", key_path, "-p", str(port),
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

    with dev_config(
        {"auth": {"ssh": {"mode": "enforce", "authorized_keys": [on_pub]}}}, server
    ):
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

    with dev_config(
        {"auth": {"ssh": {"mode": "warn", "authorized_keys": [on_pub]}}}, server
    ):
        off = _ssh_auth_stderr(ssh_port, off_key)
        # warn logs would-deny but still authenticates.
        assert "Authenticated to" in off, f"warn mode should admit off-list; stderr={off!r}"
        assert "Permission denied" not in off
