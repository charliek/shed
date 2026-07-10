"""Bad `status` argument handling: an unknown flag and the removed `--live` flag both
exit 2 with a byte-equal stderr message across the impls."""

import pytest


@pytest.mark.differential
def test_status_bogus_arg_exit2_equal(run_cli, differential):
    def scenario(impl):
        r = run_cli(impl, "status", "--bogus")
        assert r.returncode == 2, f"{impl}: exit {r.returncode} (want 2)\n{r.stderr}"
        assert r.stdout == "", f"{impl}: wrote to stdout: {r.stdout!r}"
        return r.stderr

    msg = differential(scenario)
    assert msg.strip() == 'status: unknown argument "--bogus"'


@pytest.mark.differential
def test_status_live_removed_exit2_equal(run_cli, differential):
    def scenario(impl):
        r = run_cli(impl, "status", "--live")
        assert r.returncode == 2, f"{impl}: exit {r.returncode} (want 2)\n{r.stderr}"
        return r.stderr

    msg = differential(scenario)
    assert "--live was removed" in msg
