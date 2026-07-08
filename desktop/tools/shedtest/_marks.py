"""Target-gating pytest markers shared across the functional suites.

`pytest_configure` (conftest.py) syncs the resolved `--target` into
`$SHED_TEST_TARGET` before collection, so this module — imported at collection
time — sees the effective target whether it came from the CLI flag or the env.

- `mac_only`: the Swift app's ops (the surface-based screenshot and other
  Swift-only op surfaces) that the Tauri client doesn't implement.
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

# Targets whose UI implements the Agents / remote-control pane. Tauri gained it in
# Phase C (B2).
_AGENTS_TARGETS = {"mac", "tauri"}

needs_agents = pytest.mark.skipif(
    _TARGET not in _AGENTS_TARGETS,
    reason="target has no Agents/RC pane",
)
