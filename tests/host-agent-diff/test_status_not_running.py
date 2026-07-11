"""`status` with no daemon listening → exit 1 and a three-line stderr message that is
BYTE-equal between the impls after masking the (env-dependent) socket dir."""

import pytest

from normalize import mask_not_running


@pytest.mark.differential
def test_status_not_running_masked_stderr_equal(run_cli, differential):
    def scenario(impl):
        r = run_cli(impl, "status")
        assert r.returncode == 1, f"{impl}: exit {r.returncode} (want 1)\n{r.stderr}"
        assert r.stdout == "", f"{impl}: not-running wrote to stdout: {r.stdout!r}"
        return mask_not_running(r.stderr, r.socket_dir)

    masked = differential(scenario)

    lines = masked.rstrip("\n").split("\n")
    assert lines == [
        "shed-host-agent is not running — nothing is listening at "
        "<DIR>/host-agent-status.sock",
        "Start it (Homebrew): brew services start shed-host-agent",
        "  or run it directly: shed-host-agent -config <path>",
    ]
