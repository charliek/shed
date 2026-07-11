"""The desktop `token.get` + minter differential — the SSH-bootstrap control-token mint.

A desktop client sends `token.get{server:"prod"}` (surface A); the daemon's control-token
provider resolves `prod` from the shed CLI config (`~/.shed/config.yaml`), mints a CONTROL
token over the server's SSH `_bootstrap` channel by invoking the system `ssh` client, and
replies `token.response`. The Go `cmd/shed-host-agent` and the Rust `crates/shed-host-agent`
are asserted to produce equal wire-visible output.

**Deterministic seam → the minted token is compared UNMASKED.** Both daemons resolve `ssh`
from PATH to the SAME committed shim (`conftest._write_ssh_shim`), which prints one fixed
`sdk.Bundle` and captures its argv. So `mask_token_response` leaves `token`/`expires_at` to
be diffed — pinning that both impls carry the minted token through and format the expiry to
UTC RFC3339 identically — while the volatile `id`/`ts` are masked.

**The `ssh` argv is compared too** (the minter cell's control-scope live check). Each impl
runs with its OWN isolated `$HOME`, so the raw argv differ in the one env-dependent element
`-o UserKnownHostsFile=<HOME>/.shed/known_hosts`; we home-normalize that (replace the impl's
`$HOME` with `<HOME>`, analogous to `mask_live_status` stripping the socket-path prefix) and
then (a) the `differential` fixture asserts the normalized argv are equal Go-vs-Rust — the
byte-for-byte `ssh_args` parity check — and (b) assert them against the expected vector
(control scope, `_bootstrap` user, the 17 `-o` options in order).
"""

from __future__ import annotations

import os
import subprocess
from pathlib import Path

import pytest

from conftest import _write_ssh_shim
from desktop_client import DesktopClient, wait_for_consumer
from normalize import canonical, mask_token_response
from synthetic_bus import SyntheticBus

FIXTURES_DIR = Path(__file__).resolve().parent / "fixtures"

# The committed test key's SSH-wire public-key blob (2nd field of the `.pub`) — used to
# build a syntactically-valid `known_hosts` pin line. The pin's VALUE is irrelevant to
# the presence check (`knownHostsPinned` / `known_hosts_pinned`), but the line must parse.
_PUB_BLOB = (FIXTURES_DIR / "test_ed25519.pub").read_text().split()[1]

# The secure `prod` server the control-token provider resolves (block-style — both yaml
# readers agree): an https `api_url` + an SSH endpoint, so it is mintable.
PROD_SHED_CONFIG = """\
servers:
  prod:
    api_url: https://prod.example:8443
    host: prod.example
    ssh_port: 2222
"""

# The host-key pin for prod's SSH endpoint (`[host]:port` for a non-22 port).
KNOWN_HOSTS = f"[prod.example]:2222 ssh-ed25519 {_PUB_BLOB}\n"

# The fixed `sdk.Bundle` the shim prints: CONTROL scope (must match the token.get scope or
# decode rejects), an https port + fingerprint, a non-empty token, a fixed whole-second
# `Z` expiry (so parse→UTC→render is byte-identical across impls). No single quotes (the
# shim wraps it in `printf '%s' '<bundle>'`).
SHIM_BUNDLE = (
    '{"https_port":8443,"tls_cert_fingerprint":"sha256:deadbeef",'
    '"token":"minted-control-token","scope":"control","token_id":"tk1",'
    '"expires_at":"2030-01-01T00:00:00Z"}'
)

# The exact `ssh` argv the daemon must emit (catalog §7.1), home-normalized. The 17 `-o`
# options in order, `-l _bootstrap`, the host, the CONTROL scope, and the `host-agent`
# client kind.
EXPECTED_ARGV = [
    "-T",
    "-p",
    "2222",
    "-o",
    "BatchMode=yes",
    "-o",
    "StrictHostKeyChecking=yes",
    "-o",
    "UserKnownHostsFile=<HOME>/.shed/known_hosts",
    "-o",
    "GlobalKnownHostsFile=/dev/null",
    "-o",
    "VerifyHostKeyDNS=no",
    "-o",
    "KnownHostsCommand=none",
    "-o",
    "UpdateHostKeys=no",
    "-o",
    "CheckHostIP=no",
    "-o",
    "PreferredAuthentications=publickey",
    "-o",
    "PubkeyAuthentication=yes",
    "-o",
    "PasswordAuthentication=no",
    "-o",
    "KbdInteractiveAuthentication=no",
    "-o",
    "ChallengeResponseAuthentication=no",
    "-o",
    "NumberOfPasswordPrompts=0",
    "-o",
    "ForwardAgent=no",
    "-o",
    "ClearAllForwardings=yes",
    "-o",
    "PermitLocalCommand=no",
    "-l",
    "_bootstrap",
    "prod.example",
    "control",
    "host-agent",
]


def test_ssh_shim_self_test(tmp_path):
    """Fake-seam self-test (slice-0 discipline): the committed shim `ssh` records its
    argv one-per-line and prints the fixed bundle to stdout, exit 0 — so a shim bug is
    never misread as a daemon diff."""
    argv_file = tmp_path / "argv.txt"
    shim = tmp_path / "ssh"
    _write_ssh_shim(shim, argv_file, SHIM_BUNDLE)
    proc = subprocess.run(
        [str(shim), "-T", "-p", "2222", "control", "host-agent"],
        capture_output=True,
        text=True,
        timeout=10,
    )
    assert proc.returncode == 0
    assert proc.stdout == SHIM_BUNDLE
    assert argv_file.read_text().splitlines() == ["-T", "-p", "2222", "control", "host-agent"]
    assert os.access(shim, os.X_OK)


def _token_get_scenario(daemon, single_server_config):
    """Build a `scenario(impl)` for the `differential` fixture: drive a `token.get` for
    `prod` against `impl`'s daemon (single-server mode so the control source is
    `~/.shed/config.yaml`) and return the canonical masked `token.response` plus the
    home-normalized `ssh` argv."""

    def scenario(impl):
        with SyntheticBus() as bus:
            with daemon(
                impl,
                single_server_config(bus.url),
                shed_config=PROD_SHED_CONFIG,
                known_hosts=KNOWN_HOSTS,
                ssh_shim_bundle=SHIM_BUNDLE,
            ) as d:
                with DesktopClient(str(d.desktop_sock)) as app:
                    app.send_hello()
                    # Be the active consumer before token.get (deadline poll, not a sleep).
                    wait_for_consumer(d, connected=True, timeout=10.0)
                    app.send({"type": "token.get", "id": "q1", "server": "prod"})
                    resp = app.await_frame("token.response", timeout=10.0)
                    argv = d.read_ssh_argv(timeout=10.0)
                    # Home-normalize the one env-dependent argv element (F8).
                    argv = [a.replace(d.home, "<HOME>") for a in argv]
                    # Token-never-logged: the minted token must not appear in the op log.
                    assert "minted-control-token" not in d.read_log(), (
                        f"{impl}: minted token leaked into the operational log"
                    )
                    return {
                        "token_response": canonical(mask_token_response(resp)),
                        "argv": argv,
                    }

    return scenario


@pytest.mark.differential
def test_token_get(daemon, single_server_config, differential):
    result = differential(_token_get_scenario(daemon, single_server_config))

    # --- surface A: the token.response (token + expiry compared UNMASKED) ---
    resp = result["token_response"]
    assert resp["type"] == "token.response"
    assert resp["v"] == 2
    assert resp["in_reply_to"] == "q1"  # correlation echoes the request id
    assert resp["server"] == "prod"
    assert resp["token"] == "minted-control-token"  # deterministic → compared
    assert resp["expires_at"] == "2030-01-01T00:00:00Z"  # parsed→UTC→rendered identically
    assert "error" not in resp, "success token.response omits `error`"
    # Volatile fields masked (shape-asserted inside mask_token_response).
    assert resp["id"] == "<id>"
    assert resp["ts"] == "<ts>"

    # --- the minter's ssh argv: equal Go-vs-Rust (differential) + the expected vector ---
    argv = result["argv"]
    assert argv == EXPECTED_ARGV
    # The control-scope live check: the remote command's <scope> is `control`.
    scope = argv[argv.index("_bootstrap") + 1 + 1]  # ..._bootstrap <host> <scope>
    assert scope == "control", f"minted with scope {scope!r}, want control"
