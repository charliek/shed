"""`create` — the DTO, the session it leaves behind, its `SHED_RC_*` environment,
and (for a TUI kind) the argv the agent was actually launched with.

Every create pins `--slug` AND `--name`: the two implementations generate slugs
differently (crypto-rand vs uuid-derived) and `slug`/`tmux_session` are
deliberately NOT masked, so a pinned slug is what makes the DTOs comparable.
"""

from normalize import mask_argv, mask_env, mask_session

SHELL_SLUG = "aa1111"
OPENCODE_SLUG = "bb2222"


def _create_shell(leg):
    res = leg.run(
        "create",
        "--kind",
        "shell",
        "--slug",
        SHELL_SLUG,
        "--name",
        "parity-shell",
    )
    assert res.returncode == 0, f"{leg.impl}: exit {res.returncode}: {res.stderr}"
    return res.json()


def _create_opencode(leg):
    res = leg.run(
        "create",
        "--kind",
        "opencode",
        "--slug",
        OPENCODE_SLUG,
        "--name",
        "parity-opencode",
    )
    assert res.returncode == 0, f"{leg.impl}: exit {res.returncode}: {res.stderr}"
    return res.json()


def test_create_shell_dto(differential, isolated):
    """The create DTO of a `shell` session — no `--wait`, so `state` is the
    deterministic `starting` on both sides."""

    def scenario(impl):
        leg = isolated(impl)
        dto = _create_shell(leg)
        # The session really exists in THIS leg's server (a DTO alone would not
        # prove tmux accepted it).
        assert f"rc-{SHELL_SLUG}" in leg.wait_for_session(f"rc-{SHELL_SLUG}")
        return mask_session(dto, str(leg.home))

    differential(scenario)


def test_create_shell_session_environment(differential, isolated):
    """The `SHED_RC_*` environment stamped into the tmux session, canonicalized to
    a sorted key→value mapping — tmux's render ORDER is a tmux-version detail, not
    contract (`BuildEnvArgs` ordering is pinned by Rust unit tests instead)."""

    def scenario(impl):
        leg = isolated(impl)
        _create_shell(leg)
        leg.wait_for_session(f"rc-{SHELL_SLUG}")
        return mask_env(leg.session_env(f"rc-{SHELL_SLUG}"), str(leg.home))

    differential(scenario)


def test_create_opencode_dto(differential, isolated):
    """A TUI kind: same DTO surface, launched through the PATH-shim `opencode`."""

    def scenario(impl):
        leg = isolated(impl)
        dto = _create_opencode(leg)
        leg.wait_for_session(f"rc-{OPENCODE_SLUG}")
        return mask_session(dto, str(leg.home))

    differential(scenario)


def test_create_opencode_environment(differential, isolated):
    """opencode's create additionally stamps the allocated loopback port and an
    ALWAYS-present, always-empty `OPENCODE_SERVER_PASSWORD` (the override that
    stops opencode minting one of its own). The port is masked; its presence,
    ephemerality and the password's emptiness are not."""

    def scenario(impl):
        leg = isolated(impl)
        _create_opencode(leg)
        leg.wait_for_session(f"rc-{OPENCODE_SLUG}")
        env = leg.session_env(f"rc-{OPENCODE_SLUG}")
        assert env.get("OPENCODE_SERVER_PASSWORD") == "", env
        return mask_env(env, str(leg.home))

    differential(scenario)


def test_create_opencode_inner_command_argv(differential, isolated):
    """The argv the agent was ACTUALLY launched with, recorded by the shim through
    whatever tmux did with the inner command string.

    This is the surface that catches a quoting or flag-ordering drift the DTO
    cannot see — opencode's `--port <p> --hostname 127.0.0.1` is appended to the
    command BEFORE any shell wrap, and appending it after would hand the flags to
    the wrapper instead of the agent."""

    def scenario(impl):
        leg = isolated(impl)
        _create_opencode(leg)
        leg.wait_for_session(f"rc-{OPENCODE_SLUG}")
        # 4 elements: `--port <p> --hostname 127.0.0.1`.
        return mask_argv(leg.wait_for_agent_argv(4), str(leg.home))

    differential(scenario)


CURSOR_SLUG = "bb3333"


def test_create_cursor_inner_command_argv(differential, isolated):
    """cursor's spec-owned base flag: the agent must be launched with `--trust`
    (the workspace-trust skip both implementations append BEFORE any permission
    flags). This is the only harness surface that covers the base-flag mirror —
    the DTO and env cells cannot see the inner command, so without this cell a
    one-sided change to the flag would still read 51/51 (patch-cluster review
    finding)."""

    def scenario(impl):
        leg = isolated(impl)
        res = leg.run(
            "create",
            "--kind",
            "cursor",
            "--slug",
            CURSOR_SLUG,
            "--name",
            "parity-cursor",
        )
        assert res.returncode == 0, f"{impl}: exit {res.returncode}: {res.stderr}"
        leg.wait_for_session(f"rc-{CURSOR_SLUG}")
        # 1 element: `--trust` (bare kickoff, no permission mode, no port).
        return mask_argv(leg.wait_for_agent_argv(1), str(leg.home))

    argv = differential(scenario)
    assert argv == ["--trust"], argv
