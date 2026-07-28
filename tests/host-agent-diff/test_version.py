"""`version` — smoke only. The build string is masked everywhere else in the harness
(it changes every release), so this asserts exit 0 + nonempty stdout and deliberately
pins no value. Style-B (parametrized per impl, no golden): it asserts an absolute
property, not a recorded shape."""

import pytest


@pytest.mark.parametrize("impl", ["rust"])
def test_version_exit0_nonempty(run_cli, impl):
    r = run_cli(impl, "version")
    assert r.returncode == 0, f"{impl}: version exit {r.returncode}\n{r.stderr}"
    assert r.stdout.strip() != "", f"{impl}: version stdout was empty"
