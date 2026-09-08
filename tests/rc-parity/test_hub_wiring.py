"""The wiring cell: prove the two legs really run two DIFFERENT hub daemons.

Every other cell in this suite is a golden differential, and since plan 016 (S7)
sunset `sx` both legs are stimulated by the same Go oracle CLI — which means the
38 goldens would ALSO pass if the "rust" leg had quietly started
`shed-machine-rc serve` instead of `shed-host-agent rc-hub`. A suite that can be
mis-wired into comparing a daemon with itself and stay green proves nothing, so
the wiring is asserted directly, here, at the source.

This cell pins NO golden (it never touches `hub_differential`): its subject is
the harness's own construction, not the hubs' wire. `conftest.start_hub` carries
the same assertion per leg — this is the pair-wise half, the one that catches a
future edit collapsing the two argvs onto one binary.
"""

from pathlib import Path

import pytest

pytestmark = pytest.mark.hub


def test_legs_run_distinct_daemons(hub_leg):
    """The two legs share a CLI stimulus and differ ONLY in their daemon."""
    go, rust = hub_leg("go"), hub_leg("rust")

    assert go.hub_argv != rust.hub_argv, (
        "both hub legs launched the same daemon argv — the differential would be "
        f"comparing a hub with itself: {go.hub_argv!r}"
    )
    assert Path(go.hub_argv[0]).name == "shed-machine-rc", go.hub_argv
    assert Path(rust.hub_argv[0]).name == "shed-host-agent", rust.hub_argv

    # ...and the shared stimulus is the deliberate half of the arrangement (see
    # the module docstring): identical CLI argv, so the daemon is the only
    # controlled variable.
    assert go.cli == rust.cli, (go.cli, rust.cli)
    assert Path(go.cli[0]).name == "shed-machine-rc", go.cli
