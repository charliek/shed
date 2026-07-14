"""Inline flow-style YAML config parses identically in BOTH impls (the config-parse ·
inline-flow parity, closed by the saphyr parser swap).

Before the config-port slice the Rust `yaml_lite` reader was a line/colon reader that
treated an inline flow map like `ssh: { approval: { policy: shed-desktop } }` as an
opaque scalar and fell back to all-`deny-all` with an empty gate list, while the Go
daemon (real YAML) parsed it — a tracked `xfail`. The reader is now backed by
`saphyr-parser`, which parses flow maps + flow sequences natively, so this cell drives a
FULLY flow-style launch config through both daemons and asserts their masked
`LiveStatus` is canonical-equal, with the three approval policies parsed out of the flow
maps (not defaulted to deny-all).

This is the wire-visible half the plan's M3 disposition endorses: `LiveStatus.policies`
is the observable proof that inline-flow `approval.policy` values are parsed. (The Go
daemon has always parsed flow style, so the differential is a real Rust-parity check, not
a tautology — a Rust regression to the old opaque-scalar behavior would surface here as
`policies: {all deny-all}` != Go.) It is the SAME watch-none scenario as
`test_status_running.py`, only written in flow style — so the expected values match that
cell exactly.

The literal `{ }` braces are doubled (`{{ }}`) so the `daemon` fixture's
`.format(audit_log=..., source=...)` leaves them as single braces in the emitted config
while still filling the `{audit_log}` / `{source}` fields.
"""

import json

import pytest

from normalize import canonical, mask_live_status

# A fully inline flow-style launch config (every mapping/sequence in flow form). Escaped
# braces (`{{`/`}}`) survive the `daemon` fixture's `.format()` as single braces; the
# `{audit_log}` / `{source}` fields are filled by the fixture. Equivalent to
# conftest.WATCH_NONE_CONFIG but flow-style — chosen so the Rust parser must handle nested
# flow maps + a flow sequence, and so BOTH impls report an empty `servers` list.
INLINE_FLOW_CONFIG = (
    "ssh: {{ approval: {{ policy: shed-desktop }} }}\n"
    "aws: {{ approval: {{ policy: approve-all }} }}\n"
    "docker: {{ approval: {{ policy: deny-all }} }}\n"
    "logging: {{ enabled: true, path: {audit_log} }}\n"
    "discovery: {{ servers: [], watch: off, source: {source} }}\n"
)


@pytest.mark.differential
def test_inline_flow_config_parses_equal(daemon, differential):
    def scenario(impl):
        with daemon(impl, INLINE_FLOW_CONFIG) as d:
            r = d.status(json=True)
            assert r.returncode == 0, f"{impl}: status --json exit {r.returncode}\n{r.stderr}"
            obj = json.loads(r.stdout)
            return canonical(mask_live_status(obj, d.socket_dir, d.config_path))

    masked = differential(scenario)

    # The flow-map `approval.policy` values were parsed (not defaulted to deny-all) —
    # exactly the block-style expectation from test_status_running.py, proving flow and
    # block style parse identically on BOTH impls.
    assert masked["policies"] == {
        "ssh-agent": "shed-desktop",
        "aws-credentials": "approve-all",
        "docker-credentials": "deny-all",
    }
    assert masked["gate_namespaces"] == ["ssh-agent"]
    # The empty flow sequence `servers: []` parsed as "watch none" on both impls.
    assert masked["servers"] == []
