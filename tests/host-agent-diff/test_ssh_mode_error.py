"""A daemon started with an unknown `ssh.mode` exits 1 — and does so whether the
config is single-server OR carries a `discovery:` block. Resolution runs
UNCONDITIONALLY at startup (before any socket binds), so the mode is validated even in
multi-server mode where the single-server bus stays off (the exact case the
unconditional-resolve relocation protects). Only the exit code is asserted — the
stderr is the operational log (`tracing` lines), which the plan excludes.

Distinct from `test_config_error.py` (that is a file-not-found → exit 1); this drives a
readable config whose `ssh.mode` value is bad.
"""

from __future__ import annotations

import shutil
import subprocess
import tempfile

import pytest

from conftest import _clean_env

# Single-server (no `discovery:`) with a bogus ssh mode.
BOGUS_SINGLE = "ssh:\n  mode: bogus\n"

# Multi-server: the SAME bogus mode plus a valid `discovery:` block (the block shape
# the parser accepts — `has_key("discovery")`). Resolution still runs first and exits 1
# before the discovery source is ever read.
BOGUS_DISCOVERY = "ssh:\n  mode: bogus\ndiscovery:\n  servers: []\n  watch: off\n  source: {source}\n"


@pytest.mark.parametrize("shape", ["single-server", "discovery"])
@pytest.mark.parametrize("impl", ["rust"])
def test_ssh_unknown_mode_exits_1(binaries, tmp_path_factory, impl, shape):
    root = tmp_path_factory.mktemp(f"modeerr-{impl}")
    home = root / "home"
    home.mkdir()
    cfg = root / "cfg.yaml"
    if shape == "single-server":
        cfg.write_text(BOGUS_SINGLE)
    else:
        cfg.write_text(BOGUS_DISCOVERY.format(source=root / "nonexistent-discovery.yaml"))

    # The daemon exits before binding any socket, so the dir length doesn't matter; a
    # short mkdtemp keeps it consistent with the rest of the harness anyway.
    socket_dir = tempfile.mkdtemp(prefix="hadiff-")
    try:
        proc = subprocess.run(
            [binaries[impl], "-config", str(cfg), "-log-file", str(root / "op.log")],
            env=_clean_env(socket_dir, home),
            capture_output=True,
            timeout=30,
        )
        assert proc.returncode == 1, (
            f"{impl}/{shape}: exit {proc.returncode} (want 1)\n"
            f"stderr={proc.stderr.decode('utf-8', 'replace')}"
        )
    finally:
        shutil.rmtree(socket_dir, ignore_errors=True)
