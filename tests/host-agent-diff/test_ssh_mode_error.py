"""A broker started with an unknown `ssh.mode` exits 1 — and does so whether the
config is single-server OR carries a `discovery:` block. The app's broker resolves the
SSH backend UNCONDITIONALLY at spawn (before the harness binds its socket), so the mode
is validated in multi-server mode too (the exact case the unconditional-resolve
relocation protects); the spawn error fails the broker closed, and the harness exits 1.
Only the exit code is asserted — the stderr and the log are excluded.

This drives a readable config whose `ssh.mode` value is bad (`test_config_validate.py`
drives configs that do not load at all).
"""

from __future__ import annotations

import shutil
import subprocess
import tempfile

import pytest

from conftest import CONTROL_SOCK_NAME, _clean_env, harness_argv

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

    # The harness exits before binding its socket, so the dir length doesn't matter; a
    # short mkdtemp keeps it consistent with the rest of the harness anyway.
    socket_dir = tempfile.mkdtemp(prefix="hadiff-")
    try:
        proc = subprocess.run(
            harness_argv(
                binaries[impl], cfg, root / "op.log", f"{socket_dir}/{CONTROL_SOCK_NAME}"
            ),
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
