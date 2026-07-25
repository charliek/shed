"""The desktop `credential.get` differential — the mode-agnostic control-credential
relay that `token.get` cannot express.

A desktop client sends `credential.get{server:"prod", csr:"<base64 DER>"}` (surface A);
the daemon's control-credential provider resolves `prod` from the shed CLI config
(`~/.shed/config.yaml`), runs a CONTROL-scope SSH `_bootstrap` **passing the app's CSR
through verbatim**, and replies `credential.response`. The Go `cmd/shed-host-agent` and
the Rust `crates/shed-host-agent` are asserted to produce equal wire-visible output.

Two things are pinned here that no other cell covers:

1. **The CSR reaches `ssh` unmodified, in both impls.** The private key that will pair
   with the issued certificate never leaves the desktop app, so a CSR that either
   daemon rewrote, re-encoded, or quietly regenerated would yield a certificate the app
   cannot present — and it would fail later, at a TLS handshake, with no trace back to
   here. The shim records the argv, so the `csr=<b64>` element is compared Go-vs-Rust
   AND against the expected vector.

2. **The token-mode answer.** The shim server returns a TOKEN bundle (the same fixed
   bundle `test_token_get.py` uses), which is the everyday case: an app that asks with
   a CSR against a token-mode server must get `auth_mode:"token"` plus the token, with
   the certificate fields ABSENT. Getting the omissions right matters as much as the
   values — the app branches on `auth_mode`, and a response carrying both shapes (or
   neither) is the ambiguity the separate message was introduced to remove.

Both daemons resolve `ssh` from PATH to the SAME committed shim, so the credential
fields are deterministic and compared UNMASKED (see `mask_credential_response`).
"""

from __future__ import annotations

import pytest

from desktop_client import DesktopClient, wait_for_consumer
from normalize import canonical, mask_credential_response
from synthetic_bus import SyntheticBus
from test_token_get import KNOWN_HOSTS, PROD_SHED_CONFIG, SHIM_BUNDLE

# A syntactically plausible standard-base64 PKCS#10 stand-in. Its CONTENT is irrelevant
# — neither daemon parses it (the server does), and this cell's claim is precisely that
# neither daemon touches it — but it must be a single argv-safe token, because the
# bootstrap argv validator rejects whitespace and NUL in every request element.
APP_CSR = "MIIBSGVsbG9DU1IrK3Rlc3QvdmVjdG9yPT0="


def _credential_get_scenario(daemon, single_server_config, csr):
    """Build a `scenario(impl)` for the `differential` fixture: drive one
    `credential.get` for `prod` against `impl`'s daemon and return the canonical masked
    `credential.response` plus the home-normalized `ssh` argv."""

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
                    wait_for_consumer(d, connected=True, timeout=10.0)
                    req = {"type": "credential.get", "id": "q1", "server": "prod"}
                    if csr is not None:
                        req["csr"] = csr
                    app.send(req)
                    resp = app.await_frame("credential.response", timeout=10.0)
                    argv = d.read_ssh_argv(timeout=10.0)
                    argv = [a.replace(d.home, "<HOME>") for a in argv]
                    # Token-never-logged, same discipline as token.get.
                    assert "minted-control-token" not in d.read_log(), (
                        f"{impl}: minted token leaked into the operational log"
                    )
                    return {
                        "credential_response": canonical(mask_credential_response(resp)),
                        "argv": argv,
                    }

    return scenario


@pytest.mark.differential
def test_credential_get_relays_the_csr(daemon, single_server_config, differential):
    result = differential(_credential_get_scenario(daemon, single_server_config, APP_CSR))

    # --- surface A: the credential.response (credential fields compared UNMASKED) ---
    resp = result["credential_response"]
    assert resp["type"] == "credential.response"
    assert resp["v"] == 2
    assert resp["in_reply_to"] == "q1"  # correlation echoes the request id
    assert resp["server"] == "prod"
    assert resp["auth_mode"] == "token", "the shim server issues tokens"
    assert resp["token"] == "minted-control-token"
    assert resp["expires_at"] == "2030-01-01T00:00:00Z"
    # The omissions are the contract: a token answer carries no certificate material,
    # so an app that branches on `auth_mode` can never find both shapes populated.
    assert "client_cert" not in resp
    assert "cert_serial" not in resp
    assert "error" not in resp
    assert resp["id"] == "<id>"
    assert resp["ts"] == "<ts>"

    # --- the relayed CSR: equal Go-vs-Rust (differential) + the expected vector ---
    argv = result["argv"]
    csr_args = [a for a in argv if a.startswith("csr=")]
    assert csr_args == [f"csr={APP_CSR}"], (
        f"the app's CSR must reach ssh verbatim, exactly once; argv={argv!r}"
    )
    # Position: the remote command is `<scope> [<client-kind>] [csr=...]`, so the CSR
    # follows the scope and the kind rather than displacing either.
    i = argv.index("_bootstrap")
    assert argv[i + 2] == "control", f"scope moved: {argv!r}"
    assert argv[i + 3] == "host-agent", f"client kind moved: {argv!r}"
    assert argv[i + 4] == f"csr={APP_CSR}", f"csr is not the last request element: {argv!r}"


@pytest.mark.differential
def test_credential_get_without_a_csr_sends_none(daemon, single_server_config, differential):
    """A `credential.get` with no `csr` is legal — an app that has no use for
    certificates may ask without one — and must send NO `csr=` argument at all rather
    than an empty one. An empty `csr=` is a validation error server-side, so emitting it
    would turn "I did not ask for a certificate" into a hard failure."""
    result = differential(_credential_get_scenario(daemon, single_server_config, None))

    resp = result["credential_response"]
    assert resp["auth_mode"] == "token"
    assert resp["token"] == "minted-control-token"

    argv = result["argv"]
    assert not [a for a in argv if a.startswith("csr")], (
        f"an absent CSR must add no argument: {argv!r}"
    )
    # The legacy request shape is unchanged: `<scope> <client-kind>`, nothing after.
    i = argv.index("_bootstrap")
    assert argv[i + 2 :] == ["control", "host-agent"], f"argv tail changed: {argv!r}"
