"""Surface-B `LiveStatus.servers[]` — the supervisor's per-server health snapshot.

A single-server (no `discovery:` block) daemon runs ONE supervisor with a single unnamed
target (`name:""`). It connects to the synthetic shed-server plugin bus and subscribes to
`ssh-agent` + `docker-credentials` (the docker backend is non-nil even unconfigured). The
supervisor's `health()` surfaces each subscription's connection state into
`status --json`'s `servers[]`, which the Go and Rust daemons must render identically
(masking only the volatile per-namespace `since`, RFC3339 shape-asserted first).

Two cells:

* **connected** — the bus holds both SSE streams open, so both namespaces reach
  `connected` on both impls. `servers[]` is one entry (`name:""`) with two namespaces.
* **409-rejected** (the terminal-state cell) — the bus answers 409 for `ssh-agent`'s
  subscribe. The SDK stops that namespace terminally (`rejected`, no hot-retry) and
  surfaces it observably; `docker-credentials` stays `connected`. Both impls report the
  SAME `state:"rejected"` (asserted UNMASKED) and the SAME `last_error`
  (`"namespace already has an active subscriber: "` — Go's `errSubscribeConflict` + the
  trimmed empty body == Rust's `format!` with an empty body).
"""

from __future__ import annotations

import pytest

from normalize import canonical, mask_live_status
from synthetic_bus import SyntheticBus

# The namespaces a single-server open daemon subscribes (ssh always; docker-credentials
# because the docker backend constructs even unconfigured). Sorted — Go's
# `HostClient.Status()` sorts by namespace and the Rust `health()` now matches.
_SSH = "ssh-agent"
_DOCKER = "docker-credentials"


def _poll_servers(d, predicate, timeout: float = 12.0):
    """Poll `impl`'s `status --json` until `predicate(servers)` holds over the `servers[]`
    array (a deadline poll, never a fixed sleep) and return the full status object. Thin
    wrapper over the shared `DaemonHandle.poll_status`."""
    return d.poll_status(lambda obj: predicate(obj.get("servers", [])), timeout=timeout)


def _state_of(servers, ns):
    """The state string of namespace `ns` on the single (unnamed) server, or None."""
    for sv in servers:
        for n in sv.get("namespaces", []):
            if n.get("namespace") == ns:
                return n.get("state")
    return None


def _normalize_url(servers, bus_url):
    """Replace the (per-impl, dynamic-port) synthetic-bus URL with a stable `<bus>`
    placeholder so the two impls' `servers[].url` compare equal — each impl runs its OWN
    `SyntheticBus` on a fresh port (analogous to home-normalizing the ssh argv). Returns a
    new list; asserts every url matched the bus so a real url divergence still surfaces."""
    out = []
    for sv in servers:
        sv = dict(sv)
        assert sv["url"] == bus_url, f"unexpected server url {sv['url']!r} != {bus_url!r}"
        sv["url"] = "<bus>"
        out.append(sv)
    return out


@pytest.mark.differential
def test_servers_connected_masked_equal(daemon, single_server_config, differential):
    def scenario(impl):
        with SyntheticBus() as bus:
            with daemon(impl, single_server_config(bus.url)) as d:
                bus.wait_for_subscribe(_SSH, timeout=10.0)
                bus.wait_for_subscribe(_DOCKER, timeout=10.0)
                # Both namespaces must reach `connected` (the bus holds both streams open).
                obj = _poll_servers(
                    d,
                    lambda s: _state_of(s, _SSH) == "connected"
                    and _state_of(s, _DOCKER) == "connected",
                )
                masked = mask_live_status(obj, d.socket_dir, d.config_path)
                return canonical(_normalize_url(masked["servers"], bus.url))

    servers = differential(scenario)

    # One unnamed single-server entry with the two subscribed namespaces (sorted), both
    # connected, `since` masked (shape-asserted inside mask_live_status).
    assert len(servers) == 1
    sv = servers[0]
    assert sv["name"] == ""
    assert sv["url"] == "<bus>"
    names = [n["namespace"] for n in sv["namespaces"]]
    assert names == sorted([_DOCKER, _SSH])
    for n in sv["namespaces"]:
        assert n["state"] == "connected"
        assert n["since"] == "<ts>"
        assert n.get("last_error", "") == ""


@pytest.mark.differential
def test_servers_409_rejected_masked_equal(daemon, single_server_config, differential):
    def scenario(impl):
        with SyntheticBus(conflict={_SSH}) as bus:
            with daemon(impl, single_server_config(bus.url)) as d:
                bus.wait_for_subscribe(_SSH, timeout=10.0)
                bus.wait_for_subscribe(_DOCKER, timeout=10.0)
                # ssh-agent goes terminally `rejected` (the 409); docker stays connected.
                obj = _poll_servers(
                    d,
                    lambda s: _state_of(s, _SSH) == "rejected"
                    and _state_of(s, _DOCKER) == "connected",
                )
                masked = mask_live_status(obj, d.socket_dir, d.config_path)
                return canonical(_normalize_url(masked["servers"], bus.url))

    servers = differential(scenario)

    assert len(servers) == 1
    sv = servers[0]
    assert sv["name"] == ""
    assert sv["url"] == "<bus>"
    by_ns = {n["namespace"]: n for n in sv["namespaces"]}
    # The 409 → terminal rejected, asserted UNMASKED; last_error is diffed (equal on both
    # impls: the conflict error + an empty body).
    assert by_ns[_SSH]["state"] == "rejected"
    assert by_ns[_SSH]["last_error"] == "namespace already has an active subscriber: "
    assert by_ns[_SSH]["since"] == "<ts>"
    # docker-credentials is unaffected — still connected.
    assert by_ns[_DOCKER]["state"] == "connected"
