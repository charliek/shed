"""A broker started with a valid-YAML-but-INVALID (or malformed) `--config` exits 1 —
the live half of the config-validate contract. Each vector is a config
`HostAgentConfig::load` must reject: an unknown/biometric policy, an
`aws.mode`/`aws.sheds` error, a non-positive/invalid `approval_timeout`, a duplicate
map key, or malformed YAML. (The vector set is the one the retired Go
`LoadConfig`/`Validate` rejected; the saphyr parser + `validate()` port matched it,
which is what the shared `config_validate.json` fixture records.) The app's loader
(`shed_app::load_or_synthesize`) refuses each, so the harness exits 1 before binding
its control socket (the app keeps running with its broker failed closed).

Direct-launch-await-exit-1 pattern (like `test_ssh_mode_error.py`), NOT the `daemon`
fixture — a rejected config never binds a socket, so there is nothing to query; only
the exit code is asserted. The operational log is the message channel and is excluded
from the compared surface; per-vector message substrings are pinned language-neutrally
by the `config_validate.json` fixture's in-crate runner, not here.

There is deliberately NO exit-0 positive control: a VALID config boots the broker and
runs until it is signalled, so this exit-code pattern cannot assert exit 0. The
"a valid / deprecated-key config loads OK" case is unit-owned (`config.rs`
`deprecated_desktop_keys_load_ok`) and golden-owned (`config_validate.json` `valid:true`
vectors), per the plan's H3 disposition.
"""

from __future__ import annotations

import shutil
import subprocess
import tempfile

import pytest

from conftest import CONTROL_SOCK_NAME, _clean_env, harness_argv

# (id, config_yaml) — each valid YAML but an invalid config, OR malformed YAML. Mirrors
# the config_validate.json fixture's invalid vectors; the fixture pins the per-vector
# message substring, this cell pins the live exit-1 behavior.
BAD_CONFIGS = [
    ("unknown-ssh-policy", "ssh:\n  approval:\n    policy: maybe\n"),
    ("biometrics-for-aws", "aws:\n  approval:\n    policy: biometrics\n"),
    ("biometrics-or-password-for-docker", "docker:\n  approval:\n    policy: biometrics-or-password\n"),
    ("aws-mode-bogus", "aws:\n  mode: bogus\n"),
    ("per-shed-bad-mode", "aws:\n  servers:\n    mini2:\n      sheds:\n        web:\n          mode: nope\n"),
    ("populated-aws-sheds", "aws:\n  sheds:\n    web:\n      role: arn:aws:iam::123:role/web\n"),
    ("approval-timeout-nonsense", "approval_timeout: nonsense\n"),
    ("approval-timeout-zero", "approval_timeout: 0s\n"),
    ("approval-timeout-negative", "approval_timeout: -5s\n"),
    ("approval-timeout-bare-10", "approval_timeout: 10\n"),
    ("duplicate-key", "server: http://a:8080\nserver: http://b:8080\n"),
    ("map-valued-discovery-servers", "discovery:\n  servers:\n    web: {}\n"),
    ("malformed-top-level-garbage", "{{invalid yaml"),
    ("malformed-unterminated-flow", "ssh:\n  approval:\n    policy: [unterminated\n"),
    # The signing check's machine-wide settings (plan 035 §3.5).
    ("allow-users-not-a-list", "ssh:\n  allow_users: git\n"),
    ("allow-users-reserved-name", "ssh:\n  allow_users: [_bootstrap]\n  offer_keys: []\n"),
    ("allow-users-widened-without-offer-keys", "ssh:\n  allow_users: [git, deploy]\n"),
    ("offer-keys-bad-entry", "ssh:\n  offer_keys: [id_ed25519]\n"),
]


@pytest.mark.parametrize("cfg_id,config_yaml", BAD_CONFIGS, ids=[c[0] for c in BAD_CONFIGS])
@pytest.mark.parametrize("impl", ["rust"])
def test_invalid_config_exits_1(binaries, tmp_path_factory, impl, cfg_id, config_yaml):
    root = tmp_path_factory.mktemp(f"cfgvalidate-{impl}")
    home = root / "home"
    home.mkdir()
    cfg = root / "cfg.yaml"
    cfg.write_text(config_yaml)

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
            f"{impl}/{cfg_id}: exit {proc.returncode} (want 1)\n"
            f"stderr={proc.stderr.decode('utf-8', 'replace')}"
        )
    finally:
        shutil.rmtree(socket_dir, ignore_errors=True)
