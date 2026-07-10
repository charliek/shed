"""A daemon started with an unreadable `-config` path exits 1 in BOTH impls. Only the
exit code is compared: the stderr here is the operational log (Go `slog` vs Rust
`tracing`-style lines), which the plan explicitly excludes from the differential."""

import pytest


@pytest.mark.parametrize("impl", ["go", "rust"])
def test_config_load_error_exit1(run_cli, impl):
    # No subcommand + a bogus -config path => daemon mode, which fails to load the
    # config and exits 1 before binding any socket.
    r = run_cli(impl, "-config", "/no/such/host-agent-config.yaml")
    assert r.returncode == 1, f"{impl}: exit {r.returncode} (want 1)\n{r.stderr}"
