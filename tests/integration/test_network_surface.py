"""Phase 1: network-surface route split.

Verifies (on the live VZ dev server, via the Phase 0.5 `dev_config` harness):

  - Default (no `internal_http_port`): the credential bus is reachable on
    the public HTTP listener — legacy behavior, unchanged.
  - With `internal_http_port` set: the bus (`/api/plugins/*`) and the
    Connect tunnel (`/api/sheds/*/connect/*`) move to the loopback-only
    internal listener and are gone from the public listener, while the
    control plane (info, sheds, …) stays on the public listener.

Config mutation is VZ-only (the harness restarts the local VZ dev server);
the route-registration logic itself is backend-agnostic Go covered by
`internal/config` + `internal/api` unit tests, so FC needs no separate
live case here.
"""

from __future__ import annotations

import urllib.error
import urllib.request

import pytest

from fixtures.devcontrol import dev_config
from fixtures.server import resolve_server_entry


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
def test_bus_on_public_listener_by_default(vz_server_dev):
    """Default config (split off): the bus is served on the public port."""
    public = int(resolve_server_entry(vz_server_dev.name)["http_port"])
    assert _status(public, "/api/plugins/listeners") == 200


@pytest.mark.vz
@pytest.mark.slow
def test_split_moves_bus_and_connect_to_internal(vz_server_dev):
    """With internal_http_port set, bus + Connect are loopback-only and the
    control plane stays on the public listener."""
    server = vz_server_dev.name
    public = int(resolve_server_entry(server)["http_port"])
    internal = public + 1  # e.g. 18081 alongside the dev server's 18080

    with dev_config({"internal_http_port": internal}, server):
        # Control plane + bootstrap endpoints stay on the public listener.
        assert _status(public, "/api/info") == 200
        assert _status(public, "/api/sheds") == 200
        # Bus + Connect are removed from the public listener. Using port 0:
        # if the Connect route were still registered here, handleConnect would
        # run and return 400 (INVALID_PORT); a 404 proves the route itself is
        # absent (not merely a missing shed, which also 404s on a live route).
        assert _status(public, "/api/plugins/listeners") == 404
        assert _status(public, "/api/sheds/nope/connect/0") == 404
        # ...and present on the internal loopback listener: the bus answers,
        # and the Connect handler runs and rejects port 0 with 400 (proving the
        # route is registered here, not 404'd away).
        assert _status(internal, "/api/plugins/listeners") == 200
        assert _status(internal, "/api/sheds/nope/connect/0") == 400
        # The internal listener does NOT serve the control plane.
        assert _status(internal, "/api/info") == 404
