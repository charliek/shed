"""Resolve a dev-server API endpoint for the rc integration tests, honoring
both an OPEN-mode and a SECURE-mode `~/.shed/config.yaml` entry.

The rc hub tests (`test_rc_enrichment.py`, `test_rc_hub_activity.py`) drive the
parallel dev server's HTTP surfaces directly, resolving host+port from the
client config the same way the CLI reaches the server. Historically their
endpoint helpers assumed the dev entry was OPEN mode — plain HTTP on
`http_port`, no auth — and silently SKIPPED against a SECURE entry. But the
maintainer's real dev server (`my-server-dev`) runs SECURE: a pinned-TLS
listener on `https_port` (api_url `https://127.0.0.1:18443`), reached with a
Bearer `control_token`. This module teaches both modules to speak either
dialect through one shared `ApiEndpoint`.

Endpoint resolution (`resolve_api_endpoint`):

  * SECURE entry — detected by `security == "secure"` or a present
    `https_port` / `tls_cert_fingerprint`. The base URL is the entry's
    `api_url` (== the `endpoint` the CLI reports over `server list`); the
    `control_token` is read from the RAW `~/.shed/config.yaml`, because the CLI
    deliberately never emits the token over `shed --json server list`, so
    `resolve_server_entry` alone can't see it. Requests carry
    `Authorization: Bearer <control_token>`.
  * OPEN entry — plain `http://<host>:<http_port>`, no auth (the prior shape).

TLS: the dev cert is self-signed and the CLI trusts it by PINNING its
fingerprint (`tls_cert_fingerprint`), not via a CA chain. Neither `urllib` nor
`requests` can pin a fingerprint, so we connect with an UNVERIFIED TLS context
(the `verify=False` equivalent). That is acceptable here and only here: the
target is a localhost (or SSH-reachable, dev-only) endpoint, TLS trust is not
what these rc tests are exercising, and the pin itself is already covered by
`test_tls.py`. We are testing rc surfaces, not the TLS trust path.

Token staleness: a SECURE control_token has a short TTL. If a request comes
back 401 the token in config.yaml has expired; the endpoint SKIPS with a clear
"re-mint by running any shed CLI command against this entry" message rather
than failing — an expired local token is an environment gap, not an rc
regression. Genuinely-absent-feature skips (503, missing feature token) are
left to the callers, unchanged.
"""

from __future__ import annotations

import json
import ssl
import urllib.error
import urllib.request
from dataclasses import dataclass
from pathlib import Path
from typing import Optional

import pytest
import yaml

from fixtures.server import resolve_server_entry

# The CLI's client config. Fixed at `~/.shed/config.yaml` (see
# internal/config/client.go:GetClientConfigPath) — the same location
# fixtures/devcontrol.py assumes. Source of the `control_token`, which the CLI
# never emits over `shed --json server list`.
_CONFIG_PATH = Path.home() / ".shed" / "config.yaml"


def _raw_config_servers() -> dict:
    """The raw `servers:` map from `~/.shed/config.yaml`.

    Returns `{}` if the file is missing/unreadable/malformed — the caller then
    degrades to only what `resolve_server_entry` (the `server list` view)
    provides, which is enough for an OPEN entry.
    """
    try:
        with _CONFIG_PATH.open() as f:
            data = yaml.safe_load(f) or {}
    except (OSError, yaml.YAMLError):
        return {}
    servers = data.get("servers")
    return servers if isinstance(servers, dict) else {}


def _unverified_ctx() -> ssl.SSLContext:
    """A TLS context that skips chain + hostname verification (the `verify=False`
    equivalent). See the module docstring for why this is acceptable for the
    pinned-but-unchainable self-signed dev cert."""
    ctx = ssl.create_default_context()
    ctx.check_hostname = False
    ctx.verify_mode = ssl.CERT_NONE
    return ctx


@dataclass
class ApiEndpoint:
    """A resolved dev-server API base plus the auth + TLS a request needs.

    `base` is scheme+host+port with no trailing slash. `token`, when set, is
    sent as a Bearer header (SECURE mode). `ssl_context`, when set, is the
    unverified TLS context for the self-signed dev cert; it is None for OPEN
    (http) endpoints, where `urllib` ignores it anyway.
    """

    base: str
    server_name: str
    token: Optional[str] = None
    ssl_context: Optional[ssl.SSLContext] = None

    def request(
        self, path: str, *, accept: Optional[str] = None
    ) -> urllib.request.Request:
        req = urllib.request.Request(f"{self.base}{path}")
        if self.token:
            req.add_header("Authorization", f"Bearer {self.token}")
        if accept:
            req.add_header("Accept", accept)
        return req

    def open(
        self, path: str, *, timeout: float = 15.0, accept: Optional[str] = None
    ):
        """Open `path` and return the live response (a context manager).

        A 401 is treated as an expired control_token and SKIPS (an environment
        gap, not a regression). Every other `HTTPError` propagates with its body
        intact so callers can classify it themselves — e.g. the 503
        RC_HUB_UNAVAILABLE skip, or a genuine failure.
        """
        req = self.request(path, accept=accept)
        try:
            return urllib.request.urlopen(
                req, timeout=timeout, context=self.ssl_context
            )
        except urllib.error.HTTPError as e:
            if e.code == 401:
                e.close()
                pytest.skip(
                    f"control token for {self.server_name!r} expired (401) — run "
                    f"any shed CLI command against this entry (e.g. `shed -s "
                    f"{self.server_name} list`) to re-mint it, then re-run"
                )
            raise

    def get_json(self, path: str, *, timeout: float = 15.0) -> dict:
        with self.open(path, timeout=timeout) as resp:
            return json.loads(resp.read().decode("utf-8"))


def resolve_api_endpoint(server_name: str) -> ApiEndpoint:
    """Resolve `server_name` to an `ApiEndpoint`, SECURE or OPEN.

    Skips cleanly (never fails) when the entry is absent, or when a SECURE entry
    lacks the material needed to reach it (no base URL, or no control_token in
    config.yaml — both signalling "run a shed CLI command against this entry").
    """
    entry = resolve_server_entry(server_name)
    if not entry:
        pytest.skip(f"no config entry for server {server_name!r}")

    # SECURE signal: any of security=="secure", an https_port, or a pinned cert
    # fingerprint. `server list` surfaces these; api_url + control_token live
    # only in the raw config, read below.
    secure = (
        entry.get("security") == "secure"
        or entry.get("https_port")
        or entry.get("tls_cert_fingerprint")
    )
    if secure:
        raw = _raw_config_servers().get(server_name) or {}
        base = raw.get("api_url") or entry.get("endpoint")
        if not base:
            host = entry.get("host") or "localhost"
            port = entry.get("https_port")
            if not port:
                pytest.skip(
                    f"secure entry {server_name!r} has no api_url/endpoint/"
                    "https_port to reach it"
                )
            base = f"https://{host}:{port}"
        token = raw.get("control_token")
        if not token:
            pytest.skip(
                f"secure entry {server_name!r} has no control_token in "
                "~/.shed/config.yaml — run any shed CLI command against this "
                "entry to mint one, then re-run"
            )
        return ApiEndpoint(
            base=str(base).rstrip("/"),
            server_name=server_name,
            token=token,
            ssl_context=_unverified_ctx(),
        )

    # OPEN: plain HTTP on http_port, no auth (the prior shape).
    host = entry.get("host") or "localhost"
    port = int(entry.get("http_port") or 0)
    if port <= 0:
        pytest.skip(f"config entry for {server_name!r} has no http_port")
    return ApiEndpoint(base=f"http://{host}:{port}", server_name=server_name)
