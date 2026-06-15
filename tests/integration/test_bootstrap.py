"""Bootstrap-channel allowlist gating (live).

The reserved `_bootstrap` SSH channel mints HTTP tokens, but only for keys in the
server's SSH allowlist — it re-verifies the connecting key against `auth.ssh`
before minting. This drives the dev server in `auth.mode: secure` with ONLY the
local key allowlisted and proves: an off-list key is denied the mint, while the
allowlisted key still mints. VZ-only (config mutation restarts the dev server);
the mint re-verify logic is backend-agnostic Go covered by internal/sshd unit
tests.
"""

from __future__ import annotations

import subprocess
import tempfile
from pathlib import Path

import pytest

from fixtures.devcontrol import bootstrap_mint, dev_config
from fixtures.server import resolve_server_entry

KNOWN_HOSTS = Path.home() / ".shed" / "known_hosts"


def _bootstrap_with_key(ssh_port: int, key_path: Path, scope: str = "control") -> subprocess.CompletedProcess:
    """Attempt a `_bootstrap` exchange offering ONLY `key_path` (IdentitiesOnly).
    A non-zero returncode means the mint was refused at SSH auth."""
    args = [
        "ssh", "-T", "-p", str(ssh_port),
        "-i", str(key_path),
        "-o", "IdentitiesOnly=yes",
        "-o", "BatchMode=yes",
        "-o", f"UserKnownHostsFile={KNOWN_HOSTS}",
        "-o", "StrictHostKeyChecking=yes",
        "_bootstrap@localhost", scope, "cli",
    ]
    return subprocess.run(args, capture_output=True, text=True, timeout=20)


@pytest.mark.vz
@pytest.mark.slow
def test_bootstrap_mint_is_allowlist_gated(vz_server_dev):
    server = vz_server_dev.name
    ssh_port = int(resolve_server_entry(server)["ssh_port"])
    pubkey = (Path.home() / ".ssh" / "id_ed25519.pub").read_text().strip()
    overrides = {"auth": {"mode": "secure", "ssh": {"authorized_keys": [pubkey]}}}
    with dev_config(overrides, server):
        # Positive control: the allowlisted local key mints a control token.
        assert bootstrap_mint(server, "control").startswith("shed_control_")

        # An off-list key is refused: generate a throwaway ed25519 not in the
        # allowlist and offer only it.
        with tempfile.TemporaryDirectory() as d:
            offkey = Path(d) / "offkey"
            kg = subprocess.run(
                ["ssh-keygen", "-t", "ed25519", "-f", str(offkey), "-N", "", "-q"],
                capture_output=True, text=True, timeout=15,
            )
            assert kg.returncode == 0, f"ssh-keygen failed: {kg.stderr!r}"
            r = _bootstrap_with_key(ssh_port, offkey, "control")
            assert r.returncode != 0, (
                "an off-list key must be refused the bootstrap mint, got exit 0; "
                f"stdout={r.stdout[-300:]!r}"
            )
            # Nothing token-shaped leaked onto stdout.
            assert "shed_control_" not in r.stdout
