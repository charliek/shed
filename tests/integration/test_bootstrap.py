"""Bootstrap-channel allowlist gating (live).

The reserved `_bootstrap` SSH channel issues client certificates, but only for
keys in the server's SSH allowlist: an off-list key never reaches a session,
so it never gets a certificate. This drives the running mtls dev server (no
reconfiguration: its committed config allowlists the developer's key, as every
enrolled dev entry already assumes) and proves both halves: the allowlisted key
is issued a certificate, and an off-list key is refused at SSH auth with
nothing certificate-shaped on stdout.

It replaces the bearer-token form of this test, which ran against the removed
token mode. VZ-only because the off-list probe ignores the user's ssh config
(see `_bootstrap_with_key`) and so must name a host that resolves without it;
the issue/refuse logic is backend-agnostic Go covered by `internal/sshd` unit
tests.
"""

from __future__ import annotations

import base64
import subprocess
from pathlib import Path

import pytest

from fixtures.devcontrol import KNOWN_HOSTS_PATH, bootstrap_cert
from fixtures.server import resolve_server_entry


def _bootstrap_with_key(ssh_port: int, key_path: Path, request: str) -> subprocess.CompletedProcess:
    """Attempt a `_bootstrap` exchange offering ONLY `key_path`.

    `IdentitiesOnly=yes` alone is NOT enough to isolate `key_path`: a user
    `~/.ssh/config` with `Host * IdentityFile ~/.ssh/id_ed25519` (the macOS
    default) makes that key an explicit identity, and a key loaded in the agent
    that matches a configured IdentityFile is then offered too — so an
    allowlisted local key would be tried first and authenticate, masking the
    off-list rejection this probe exists to assert. `-F /dev/null` ignores the
    user/system ssh_config and `IdentityAgent=none` ignores the agent, so the
    only key offered is `key_path`."""
    args = [
        "ssh", "-T", "-p", str(ssh_port),
        "-F", "/dev/null",
        "-i", str(key_path),
        "-o", "IdentitiesOnly=yes",
        "-o", "IdentityAgent=none",
        "-o", "BatchMode=yes",
        "-o", f"UserKnownHostsFile={KNOWN_HOSTS_PATH}",
        "-o", "StrictHostKeyChecking=yes",
        "_bootstrap@localhost", *request.split(),
    ]
    return subprocess.run(args, capture_output=True, text=True, timeout=20)


@pytest.mark.vz
def test_bootstrap_cert_is_allowlist_gated(vz_server_dev, tmp_path):
    server = vz_server_dev.name
    ssh_port = int(resolve_server_entry(server)["ssh_port"])

    # Positive control: the allowlisted local key is issued a certificate.
    issued = bootstrap_cert(server, "control", tmp_path / "issued")
    assert "BEGIN CERTIFICATE" in issued.cert_path.read_text()

    # An off-list key is refused: a throwaway ed25519 not in the allowlist,
    # offered alone, with a request line that is otherwise exactly the one
    # that was just honoured.
    offkey = tmp_path / "offkey"
    kg = subprocess.run(
        ["ssh-keygen", "-t", "ed25519", "-f", str(offkey), "-N", "", "-q"],
        capture_output=True, text=True, timeout=15,
    )
    assert kg.returncode == 0, f"ssh-keygen failed: {kg.stderr!r}"
    csr = (tmp_path / "issued" / "control.csr.der").read_bytes()
    request = f"control cli csr={base64.b64encode(csr).decode('ascii')}"
    r = _bootstrap_with_key(ssh_port, offkey, request)
    assert r.returncode != 0, (
        "an off-list key must be refused a certificate, got exit 0; "
        f"stdout={r.stdout[-300:]!r}"
    )
    # Refused at SSH auth, and not for some other reason (an unknown host key
    # would also exit non-zero, and prove nothing).
    assert "Permission denied" in r.stderr, f"expected a publickey refusal: {r.stderr[-500:]!r}"
    # Nothing certificate-shaped leaked onto stdout.
    assert "client_cert" not in r.stdout and "BEGIN CERTIFICATE" not in r.stdout
