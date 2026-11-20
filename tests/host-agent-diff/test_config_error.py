"""A broker started with no config file runs on the desktop app's synthesized default.

The retired daemon exited 1 when its `-config` path did not exist. The app's broker does
not: a missing `extensions.yaml` is a fresh install, and the app's loader
(`shed_app::load_or_synthesize`, which the harness calls) synthesizes the desktop
default — `ssh.approval.policy: shed-desktop` (so `ssh-agent` is the one delegated
namespace) and discovery over the shed CLI config, which the isolated HOME does not
have, so no server is watched. A config that exists but does not load still stops it
(`test_config_validate.py`). Not a golden: the assertion is what the default sets."""

import os


def test_missing_config_runs_on_the_synthesized_default(daemon):
    # `config_text=None`: the fixture writes no config at the path it passes.
    with daemon("rust", None) as d:
        assert not os.path.exists(d.config_path)
        status = d.status()
    assert status["gate_namespaces"] == ["ssh-agent"], status
    assert status["policies"]["ssh-agent"] == "shed-desktop", status
    assert status["servers"] == [], status
