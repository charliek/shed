"""Inline flow-style YAML config parses (the config-parse · inline-flow contract,
closed by the saphyr parser swap).

Before the config-port slice the `yaml_lite` reader was a line/colon reader that
treated an inline flow map like `ssh: { approval: { policy: shed-desktop } }` as an
opaque scalar and fell back to all-`deny-all` with an empty gate list — a tracked
`xfail` against the retired Go daemon, which parsed it. The reader is now backed by
`saphyr-parser`, which parses flow maps + flow sequences natively, so this cell drives a
FULLY flow-style launch config through the broker and golden-pins the masked
`LiveStatus`, with the three approval policies parsed out of the flow maps (not
defaulted to deny-all).

This is the wire-visible half the plan's M3 disposition endorses: `LiveStatus.policies`
is the observable proof that inline-flow `approval.policy` values are parsed — a
regression to the old opaque-scalar behavior surfaces here as
`policies: {all deny-all}`. It is the watch-none scenario (`conftest.WATCH_NONE_CONFIG`)
written in flow style. The app's broker lists no keys at spawn, so the status's `ssh`
block carries no key counts (the retired daemon listed local keys at start-up and
reported `0`/`0` here).

The literal `{ }` braces are doubled (`{{ }}`) so the `daemon` fixture's
`.format(audit_log=..., source=...)` leaves them as single braces in the emitted config
while still filling the `{audit_log}` / `{source}` fields.
"""

import pytest

from normalize import canonical, mask_live_status

# A fully inline flow-style launch config (every mapping/sequence in flow form). Escaped
# braces (`{{`/`}}`) survive the `daemon` fixture's `.format()` as single braces; the
# `{audit_log}` / `{source}` fields are filled by the fixture. Equivalent to
# conftest.WATCH_NONE_CONFIG but flow-style — chosen so the Rust parser must handle nested
# flow maps + a flow sequence, and so the broker reports an empty `servers` list.
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
            obj = d.status()
            return canonical(mask_live_status(obj, d.socket_dir, d.config_path))

    masked = differential(scenario)

    # The flow-map `approval.policy` values were parsed (not defaulted to deny-all) —
    # exactly what the block-style WATCH_NONE_CONFIG sets, proving flow and block style
    # parse identically.
    assert masked["policies"] == {
        "ssh-agent": "shed-desktop",
        "aws-credentials": "approve-all",
        "docker-credentials": "deny-all",
    }
    assert masked["gate_namespaces"] == ["ssh-agent"]
    # The empty flow sequence `servers: []` parsed as "watch none".
    assert masked["servers"] == []
