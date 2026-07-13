"""Discovery reload — the `off`/`poll` convergence differential.

Both daemons build ONE supervisor and reconcile the desired server set from a discovery
`source:` (`~/.shed/config.yaml`-style `servers:` doc) per the `watch:` mode. This drives
BOTH impls with a source that starts absent and then appears, asserting they converge to
the SAME `servers[]` set (by name):

* **poll** (short interval) — picks up the appearance within a deadline.
* **off** — reconciles ONCE at startup and never reloads, so the later appearance is NOT
  picked up (`servers[]` stays empty).

The live differential drives POLL for determinism (production's default watch mode is
event-driven `notify`; that path is covered by the `watcher.rs` `notify`-backed unit
smoke). The `since` value is masked; only the server NAME set is compared, so the
per-namespace connection state (an unreachable target reconnects with an impl-specific
`last_error`) is not a diff target here — the `servers[]` state shape is owned by
`test_servers.py`.
"""

from __future__ import annotations

import json
import time

import pytest

from conftest import discovery_source_doc

# One discovery server that appears mid-run. An unreachable http endpoint (discard port
# 9) — the group starts + reconnects, but this cell asserts only that the NAME converges.
ALPHA = {"alpha": {"host": "127.0.0.1", "http_port": 9}}


def _names_of(obj) -> list:
    """The sorted `servers[].name` set from a parsed `status --json` object."""
    return sorted(sv["name"] for sv in obj.get("servers", []))


def _server_names(d) -> list:
    """The sorted `servers[].name` set from `impl`'s `status --json` (once, no poll)."""
    r = d.status(json=True)
    assert r.returncode == 0, f"{d.impl}: status --json exit {r.returncode}\n{r.stderr}"
    return _names_of(json.loads(r.stdout))


def _poll_names(d, expected: list, timeout: float = 12.0) -> list:
    """Poll `impl`'s `servers[]` names until they equal `expected` (a deadline poll)."""
    d.poll_status(lambda obj: _names_of(obj) == expected, timeout=timeout)
    return expected


@pytest.mark.differential
def test_discovery_poll_convergence(daemon, discovery_poll_config, differential):
    def scenario(impl):
        with daemon(impl, discovery_poll_config) as d:
            # Source absent at launch → an empty desired set.
            assert _server_names(d) == []
            # The source appears; poll picks it up within the deadline.
            d.source_path.write_text(discovery_source_doc(ALPHA))
            return _poll_names(d, ["alpha"])

    names = differential(scenario)
    assert names == ["alpha"]


@pytest.mark.differential
def test_discovery_off_no_reload(daemon, discovery_off_config, differential):
    def scenario(impl):
        with daemon(impl, discovery_off_config) as d:
            # Off reconciles once at startup (source absent) → empty.
            assert _server_names(d) == []
            # The source appears AFTER startup — off never reloads, so it is NOT picked up.
            d.source_path.write_text(discovery_source_doc(ALPHA))
            # Wait well beyond any poll cadence, then assert it is STILL empty (no reload).
            time.sleep(1.5)
            return _server_names(d)

    names = differential(scenario)
    assert names == []
