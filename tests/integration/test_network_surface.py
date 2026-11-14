"""Network surface: the credential bus + Connect tunnel ride the single listener.

Verifies (on the live VZ dev server, run as an open server on loopback via
`dev_config`) that the credential bus (`/api/plugins/*`) and the Connect tunnel
(`/api/sheds/*/connect/*`) are served on the same listener as the control
plane. v0.7.4 removed `internal_http_port`, so there is no separate loopback
listener: in open mode everything is on the plain-HTTP port; in mtls mode
everything is on the pinned-TLS port, with the bus + Connect gated by the
credentials scope (test_http_auth.py live, `internal/api` unit tests).

The route-registration logic is backend-agnostic Go covered by `internal/config`
+ `internal/api` unit tests, so FC needs no separate live case here.
"""

from __future__ import annotations

import urllib.error
import urllib.request

import pytest

from fixtures.devcontrol import OPEN_MODE_OVERRIDES, _merge_config, dev_config


def _status(port: int, path: str) -> int | None:
    """GET http://localhost:<port><path>; return the HTTP status, or None
    if the listener isn't reachable at all."""
    try:
        with urllib.request.urlopen(
            f"http://localhost:{port}{path}", timeout=10
        ) as resp:
            return resp.status
    except urllib.error.HTTPError as e:
        # HTTPError holds an open response body; close it so the suite's
        # warnings-as-errors doesn't trip on a ResourceWarning at GC.
        e.close()
        return e.code
    except urllib.error.URLError:
        return None


@pytest.mark.vz
@pytest.mark.slow
def test_bus_and_connect_on_single_listener(vz_server_dev):
    """The credential bus and the Connect tunnel are served on the single
    public listener of an open dev server alongside the control plane — there
    is no separate internal listener after v0.7.4."""
    public = int(_merge_config(OPEN_MODE_OVERRIDES)["http_port"])
    with dev_config(OPEN_MODE_OVERRIDES, vz_server_dev.name):
        # Control plane.
        assert _status(public, "/api/info") == 200
        assert _status(public, "/api/sheds") == 200
        # Credential bus.
        assert _status(public, "/api/plugins/listeners") == 200
        # Connect tunnel: the route is registered on this listener —
        # handleConnect runs and rejects port 0 with 400 (INVALID_PORT). A 404
        # would mean the route is absent; 400 proves it is present here.
        assert _status(public, "/api/sheds/nope/connect/0") == 400
