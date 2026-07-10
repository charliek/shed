"""`version` — smoke only. Go prints build metadata, Rust prints its cargo version;
the plan masks the version everywhere, so we assert exit 0 + nonempty stdout for each
impl and deliberately do NOT byte-compare the two."""

import pytest


@pytest.mark.parametrize("impl", ["go", "rust"])
def test_version_exit0_nonempty(run_cli, impl):
    r = run_cli(impl, "version")
    assert r.returncode == 0, f"{impl}: version exit {r.returncode}\n{r.stderr}"
    assert r.stdout.strip() != "", f"{impl}: version stdout was empty"
