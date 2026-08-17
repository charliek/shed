"""Capability discovery — the payload a client reads INSTEAD of sniffing error
strings, and the copy of it every `list` envelope embeds.

What is diffed here (everything except the version strings): the schema version,
the ordered `kinds` list, the ordered feature-token list, the whole
`kind_features` matrix, and each agent's `installed` boolean — which the PATH
shims pin deterministically, including the FALSE case (a test uninstalls two
agents from both legs, so an absent agent is proven, not assumed).

The version VALUES are masked after a `<major>.<minor>.<patch>` shape assert:
that still proves both implementations parsed the shim's `--version` line rather
than echoing it, without pinning a number the shim owns.
"""

from normalize import mask_capabilities, mask_list, mask_session

# Uninstalled on BOTH legs, so the differential covers `installed: false` as well
# as `true`. cursor's binary is `cursor-agent`, not `cursor` — the tool token and
# the binary name differ, and the capabilities key is the TOOL.
ABSENT_AGENTS = ("opencode", "cursor-agent")


def test_capabilities_payload(differential, isolated):
    """The whole payload, with two agents deliberately off PATH."""

    def scenario(impl):
        leg = isolated(impl)
        leg.uninstall_agents(*ABSENT_AGENTS)
        res = leg.run("capabilities")
        assert res.returncode == 0, f"{impl}: exit {res.returncode}: {res.stderr}"
        return mask_capabilities(res.json())

    caps = differential(scenario)
    assert caps["rc_version"] == 4
    assert caps["kinds"] == [
        "claude-broker",
        "claude-rc",
        "codex",
        "opencode",
        "cursor",
        "shell",
    ]
    assert caps["features"] == [
        "generic-perm",
        "plan-stdin",
        "prompt-b64",
        "serve",
        "activity",
        "messages",
        "contract-v2",
    ]
    # The installed booleans really are driven by the shim PATH.
    assert caps["agents"]["claude"]["installed"] is True
    assert caps["agents"]["codex"]["installed"] is True
    assert caps["agents"]["opencode"] == {"installed": False}
    assert caps["agents"]["cursor"] == {"installed": False}
    # shell has no binary, so it is never probed and never appears.
    assert "shell" not in caps["agents"]
    # claude-broker and shell are omitted from the matrix entirely — an absent row
    # is how the wire says "no feed/input/approval affordances".
    assert set(caps["kind_features"]) == {"claude-rc", "codex", "opencode", "cursor"}
    assert caps["kind_features"]["opencode"] == {
        "post_input": True,
        "approvals": "remote",
        "watch": True,
        "input": "turn",
        "feed": "messages",
        "interrupt": True,
        "attach": "tmux",
    }
    # claude-rc's row omits `watch` (false) and `input` ("") — Go's omitempty,
    # which the client-side absent-field fallbacks depend on.
    assert caps["kind_features"]["claude-rc"] == {
        "post_input": True,
        "approvals": "tui",
        "feed": "activity",
        "interrupt": False,
        "attach": "tmux",
    }


def test_list_embeds_capabilities_beside_a_live_session(differential, isolated):
    """`doList` feeds both halves from one invocation: the sessions AND the
    capabilities block. This is the full envelope (the C4 strip is gone)."""
    slug = "ll1111"

    def scenario(impl):
        leg = isolated(impl)
        res = leg.run(
            "create", "--kind", "codex", "--slug", slug, "--name", "parity-list"
        )
        assert res.returncode == 0, f"{impl}: exit {res.returncode}: {res.stderr}"
        leg.wait_for_session(f"rc-{slug}")
        # Wait for the shim's pane draw too: `state` is otherwise a race between
        # the draw and the list — macOS lost it (pinning "starting"), Linux won
        # it ("ready"), and the golden went platform-dependent (C6 review HIGH).
        leg.wait_for_pane(f"rc-{slug}", "Find and fix a bug")
        listed = leg.run("list")
        assert listed.returncode == 0, f"{impl}: exit {listed.returncode}"
        return mask_list(listed.json(), str(leg.home))

    envelope = differential(scenario)
    assert [s["slug"] for s in envelope["rc_sessions"]] == [slug]
    assert envelope["capabilities"]["rc_version"] == 4


def test_probe_carries_no_capabilities(differential, isolated):
    """The block rides the `list` envelope and the `capabilities` verb ONLY — a
    bare session DTO never carries it (nor an `omitempty` empty object)."""
    slug = "ll2222"

    def scenario(impl):
        leg = isolated(impl)
        res = leg.run(
            "create", "--kind", "codex", "--slug", slug, "--name", "parity-probe"
        )
        assert res.returncode == 0, f"{impl}: exit {res.returncode}: {res.stderr}"
        leg.wait_for_session(f"rc-{slug}")
        leg.wait_for_pane(f"rc-{slug}", "Find and fix a bug")  # same race as the list cell
        dto = leg.run("probe", "--slug", slug).json()
        assert "capabilities" not in dto, dto
        return mask_session(dto, str(leg.home))

    differential(scenario)
