"""Target-gating pytest markers shared across the functional suites.

`pytest_configure` (conftest.py) syncs the resolved `--target` into
`$SHED_TEST_TARGET` before collection, so this module — imported at collection
time — sees the effective target whether it came from the CLI flag or the env.

- `mac_only`: the Swift app's ops (the surface-based screenshot and other
  Swift-only op surfaces) that the Tauri client doesn't implement.
- `needs_agents`: the Agents-pane suite, which reads roost — tauri only since S6
  (see the constant below).
- `needs_backend`: the shared-suite tests that drive the shed-core backend ops
  (sheds.list/refresh, the lifecycle actions, create + cancel). Both targets
  (mac + tauri) implement them.
"""

from __future__ import annotations

import os

import pytest

_TARGET = os.environ.get("SHED_TEST_TARGET", "mac")

mac_only = pytest.mark.skipif(
    _TARGET != "mac",
    reason="mac-only: drives the Swift app op surface (no tauri analog)",
)

# Targets whose UI implements the shed-core backend ops.
_BACKEND_TARGETS = {"mac", "tauri"}

needs_backend = pytest.mark.skipif(
    _TARGET not in _BACKEND_TARGETS,
    reason="target has no shed-core backend ops",
)

# Targets whose UI implements the credential-approval spine. Tauri gained it in
# Phase B (B3).
_APPROVAL_TARGETS = {"mac", "tauri"}

needs_approvals = pytest.mark.skipif(
    _TARGET not in _APPROVAL_TARGETS,
    reason="target has no approval spine",
)

# Targets whose Agents pane reads ROOST.
#
# **Tauri only, since S6** (`charliek/shed#328`). The suite used to be
# cross-target because both panes read the same hub: sessions listed by ssh'ing
# `shed-ext-rc` into a shed. That guest binary is gone, and the Tauri pane now
# reads each host's `roost-session` instead. The Swift app's pane still shells
# `shed-ext-rc` — it compiles, and is retained pending demolition, but there is
# nothing on a 0.9.0 image for it to shell — so pointing `test_agents.py` at it
# would be testing a client nobody ships against a binary that no longer exists.
_AGENTS_TARGETS = {"tauri"}

needs_agents = pytest.mark.skipif(
    _TARGET not in _AGENTS_TARGETS,
    reason="target has no roost-backed Agents pane",
)
