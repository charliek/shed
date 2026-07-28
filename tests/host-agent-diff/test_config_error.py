"""A daemon started with an unreadable `-config` path exits 1. Only the exit code is
asserted: the stderr here is the operational log (`tracing`-style lines), which the
harness plan explicitly excludes from the compared surface."""

import pytest


@pytest.mark.parametrize("impl", ["rust"])
def test_config_load_error_exit1(run_cli, impl):
    # No subcommand + a bogus -config path => daemon mode, which fails to load the
    # config and exits 1 before binding any socket.
    r = run_cli(impl, "-config", "/no/such/host-agent-config.yaml")
    assert r.returncode == 1, f"{impl}: exit {r.returncode} (want 1)\n{r.stderr}"
