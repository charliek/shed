"""Surface-B secure bus — TLS-pinned subscribe + credentials-scope self-mint (4b).

A SECURE server (https `api_url`, an SSH endpoint, a `tls_cert_fingerprint` pin) reached
via `discovery:` drives BOTH daemons to:

* self-mint a **credentials-scope** token over the server's SSH `_bootstrap` channel (the
  PATH-shim `ssh` returns a fixed bundle) — the credentials-scope wire the minter + egress
  slices deferred to the supervisor slice, and
* subscribe to the bus over **https**, pinning the synthetic bus's committed leaf cert
  (`tls_cert_fingerprint = "sha256:" + hex(sha256(leaf_DER))` — the SAME derivation Go's
  `sdk.WithTLSPin`/`certFingerprint` and Rust's `shed_core::tls::fingerprint` use), and
  present `Authorization: Bearer <minted>`.

The synthetic bus serves TLS with a **committed** self-signed cert/key pair (Python's
stdlib `ssl` cannot generate one). Cells:

* **credentials-scope live mint** — the subscribe succeeds under the pin and carries the
  minted credentials token (asserted equal Go-vs-Rust and against the fixed bundle).
* **401 → invalidate + re-mint + reconnect** — the bus 401s the first subscribe; both
  daemons invalidate the token, re-mint, and reconnect (a second subscribe, fresh Bearer).
* **wrong-pin fails closed** — a discovery config with a WRONG fingerprint never completes
  the TLS handshake, so the bus records NO subscribe (both impls fail closed).
"""

from __future__ import annotations

import time
from pathlib import Path

import pytest

from conftest import discovery_source_doc

FIXTURES_DIR = Path(__file__).resolve().parent / "fixtures"

# The committed synthetic-bus TLS pair + its leaf-DER pin (regenerate → recompute; see the
# fixtures README note). Both impls pin THIS fingerprint from the discovery config.
TLS_CERT = str(FIXTURES_DIR / "synthetic_bus_cert.pem")
TLS_KEY = str(FIXTURES_DIR / "synthetic_bus_key.pem")
CERT_PIN = "sha256:25eaee3d039ab1ca7251800fda76335d10b4bd15a79301b3303a45d3420aed28"

# The committed test key's SSH-wire public-key blob, for a syntactically-valid known_hosts
# pin line (its VALUE is irrelevant to the presence pre-check; the line must parse).
_PUB_BLOB = (FIXTURES_DIR / "test_ed25519.pub").read_text().split()[1]
# Pin the secure server's SSH endpoint (127.0.0.1:2222 → `[host]:port` for a non-22 port).
KNOWN_HOSTS = f"[127.0.0.1]:2222 ssh-ed25519 {_PUB_BLOB}\n"

# The fixed bundle the shim `ssh` prints: CREDENTIALS scope (the bus token provider's
# scope), a non-empty token + https_port + fingerprint (decode requires an https port to be
# paired with SOME fingerprint; its value is irrelevant — the bus pin comes from the target,
# not the bundle), a fixed whole-second Z expiry.
CREDENTIALS_BUNDLE = (
    '{"https_port":8443,"tls_cert_fingerprint":"sha256:deadbeef",'
    '"token":"minted-credentials-token","scope":"credentials","token_id":"tk1",'
    '"expires_at":"2030-01-01T00:00:00Z"}'
)
EXPECTED_BEARER = "Bearer minted-credentials-token"


def _secure_source(bus_url: str, pin: str) -> str:
    """A discovery source pinning ONE secure server at `bus_url` (its dynamic port is only
    known at runtime, so the source is written live) with SSH endpoint 127.0.0.1:2222."""
    return discovery_source_doc(
        {
            "alpha": {
                "api_url": bus_url,
                "host": "127.0.0.1",
                "ssh_port": 2222,
                "tls_cert_fingerprint": pin,
            }
        }
    )


def _make_secure_daemon(daemon, impl, discovery_poll_config):
    """A daemon context manager wired for the secure mint: the discovery poll config, the
    shim `ssh` returning the credentials bundle, and the known_hosts pin."""
    return daemon(
        impl,
        discovery_poll_config,
        ssh_shim_bundle=CREDENTIALS_BUNDLE,
        known_hosts=KNOWN_HOSTS,
    )


@pytest.mark.differential
def test_secure_bus_credentials_mint(daemon, discovery_poll_config, differential):
    from synthetic_bus import SyntheticBus

    def scenario(impl):
        with SyntheticBus(tls_cert=TLS_CERT, tls_key=TLS_KEY) as bus:
            assert bus.url.startswith("https://"), bus.url
            with _make_secure_daemon(daemon, impl, discovery_poll_config) as d:
                # The secure server appears; poll picks it up, mints, subscribes over TLS.
                d.source_path.write_text(_secure_source(bus.url, CERT_PIN))
                auth = bus.wait_for_subscribe("ssh-agent", timeout=15.0)
                return auth

    auth = differential(scenario)
    # The subscribe succeeded UNDER THE PIN (it wouldn't reach the bus otherwise) and carries
    # the minted credentials-scope token — deterministic, compared UNMASKED + equal Go-vs-Rust.
    assert auth == EXPECTED_BEARER


@pytest.mark.differential
def test_secure_bus_401_remint_reconnect(daemon, discovery_poll_config, differential):
    from synthetic_bus import SyntheticBus

    def scenario(impl):
        # The bus 401s ssh-agent's FIRST subscribe → invalidate + re-mint + reconnect.
        with SyntheticBus(
            tls_cert=TLS_CERT, tls_key=TLS_KEY, unauthorized={"ssh-agent"}
        ) as bus:
            with _make_secure_daemon(daemon, impl, discovery_poll_config) as d:
                d.source_path.write_text(_secure_source(bus.url, CERT_PIN))
                # Wait for the SECOND subscribe (the reconnect after the 401 re-mint).
                auths = bus.wait_for_subscribe_count("ssh-agent", 2, timeout=20.0)
                return auths[:2]

    auths = differential(scenario)
    # Both the initial (401'd) and the reconnect subscribe carry the minted Bearer; the
    # re-mint returns the same fixed bundle, so both are EXPECTED_BEARER on both impls.
    assert auths == [EXPECTED_BEARER, EXPECTED_BEARER]


@pytest.mark.parametrize("impl", ["go", "rust"])
def test_secure_bus_wrong_pin_fails_closed(daemon, discovery_poll_config, impl):
    """A WRONG `tls_cert_fingerprint` fails the pin at the TLS handshake, so the client
    never completes an HTTP request → the bus records NO subscribe. Both impls fail closed
    (per-impl assertion — no cross-impl value to diff, just the absence of a subscribe)."""
    from synthetic_bus import SyntheticBus

    wrong_pin = "sha256:" + ("00" * 32)
    with SyntheticBus(tls_cert=TLS_CERT, tls_key=TLS_KEY) as bus:
        with _make_secure_daemon(daemon, impl, discovery_poll_config) as d:
            d.source_path.write_text(_secure_source(bus.url, wrong_pin))
            # Give the daemon ample time to mint + attempt (and retry) the pinned handshake.
            time.sleep(4.0)
            assert "ssh-agent" not in bus.subscribed_namespaces(), (
                f"{impl}: a wrong-pin handshake must never reach the bus subscribe"
            )
