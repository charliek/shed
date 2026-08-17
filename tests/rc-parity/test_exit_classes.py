"""The exit-code classes — 2 bad args, 3 duplicate slug, 4 gone session — with
their masked stderr.

The CODE is what an orchestrator maps (exit 3 → `409 RC_SLUG_TAKEN`, exit 4 → a
gone session, exit 2 → a bad request), and the MESSAGE for these classes is the
part of stderr plan 009 §3.5 keeps in the contract. Everything else on stderr is
explicitly not matched.
"""

from normalize import mask_list, mask_stderr

PLAN_MAX_BYTES = 1 << 20


def _err_cell(leg, sub, *args, stdin=None, truncate_after=None):
    res = leg.run(sub, *args, stdin=stdin)
    return {
        "code": res.returncode,
        "stdout": res.stdout,
        "stderr": mask_stderr(res.stderr, str(leg.home), truncate_after=truncate_after),
    }


def test_unknown_kind_is_bad_args(differential, isolated):
    def scenario(impl):
        return _err_cell(isolated(impl), "create", "--kind", "bogus", "--slug", "aa1111")

    assert differential(scenario)["code"] == 2


def test_empty_kind_is_bad_args(differential, isolated):
    """`--kind=` is SET-to-empty, not absent: Go keeps its flag default only for
    an absent flag and rejects the empty kind (exit 2, no session). sx once
    conflated the two and created a claude-rc session Go refuses (C4 review
    HIGH)."""

    def scenario(impl):
        leg = isolated(impl)
        out = _err_cell(leg, "create", "--kind=", "--slug", "aa1111")
        assert leg.sessions() == [], f"{impl}: empty-kind create left a session"
        return out

    assert differential(scenario)["code"] == 2


def test_invalid_slug_is_bad_args(differential, isolated):
    def scenario(impl):
        return _err_cell(isolated(impl), "create", "--kind", "shell", "--slug", "BAD_slug!")

    assert differential(scenario)["code"] == 2


def test_prompt_on_a_broker_kind_is_bad_args(differential, isolated):
    """`claude-broker`'s input is a remote URL, not the pane — so a typed prompt is
    rejected on KIND, before the state check (the order matters: a starting broker
    session must still report the kind reason)."""

    def scenario(impl):
        leg = isolated(impl)
        created = leg.run(
            "create", "--kind", "claude-broker", "--slug", "ee5555", "--name", "parity-broker"
        )
        assert created.returncode == 0, f"{impl}: {created.stderr}"
        leg.wait_for_session("rc-ee5555")
        return _err_cell(leg, "prompt", "--slug", "ee5555", stdin="hello\n")

    assert differential(scenario)["code"] == 2


def test_duplicate_slug_is_exit_3(differential, isolated):
    """The second create of a live slug — within ONE leg's server, since the
    isolated flavor gives each implementation its own."""

    def scenario(impl):
        leg = isolated(impl)
        first = leg.run("create", "--kind", "shell", "--slug", "ff6666", "--name", "parity-dup")
        assert first.returncode == 0, f"{impl}: {first.stderr}"
        leg.wait_for_session("rc-ff6666")
        return _err_cell(leg, "create", "--kind", "shell", "--slug", "ff6666", "--name", "parity-dup")

    assert differential(scenario)["code"] == 3


def test_oversize_plan_is_rejected_at_the_transport_boundary(differential, isolated):
    """A plan one byte over the 1 MiB cap, rejected by the CLI before any tmux
    work — with the byte count in the message, so a cap drift is visible."""

    def scenario(impl):
        oversize = "x" * (PLAN_MAX_BYTES + 1)
        cell = _err_cell(
            isolated(impl), "create", "--kind", "shell", "--slug", "aa1111",
            "--plan-stdin", stdin=oversize,
        )
        assert str(PLAN_MAX_BYTES) in cell["stderr"], cell
        return cell

    assert differential(scenario)["code"] == 2


def test_empty_plan_stdin_is_bad_args(differential, isolated):
    def scenario(impl):
        return _err_cell(
            isolated(impl), "create", "--kind", "shell", "--slug", "aa1111",
            "--plan-stdin", stdin="",
        )

    assert differential(scenario)["code"] == 2


def test_prompt_b64_without_plan_stdin_is_bad_args(differential, isolated):
    def scenario(impl):
        return _err_cell(
            isolated(impl), "create", "--kind", "shell", "--slug", "aa1111",
            "--prompt-b64", "aGVsbG8=",
        )

    assert differential(scenario)["code"] == 2


def test_bad_prompt_b64_is_bad_args(differential, isolated):
    """The one message whose TAIL is a third-party parser's wording (Go's
    `encoding/base64` vs the Rust `base64` crate), so the cell diffs the class,
    the exit code and the message HEAD, with the detail masked."""

    def scenario(impl):
        return _err_cell(
            isolated(impl), "create", "--kind", "shell", "--slug", "aa1111",
            "--plan-stdin", "--prompt-b64", "!!!not-base64!!!",
            stdin="a plan\n",
            truncate_after="--prompt-b64 is not valid base64: ",
        )

    assert differential(scenario)["code"] == 2


def test_unknown_flag_is_a_usage_error(differential, isolated):
    """Only the CODE (and an empty stdout) is contract here: Go's `flag` package
    dumps a per-flag usage block this port deliberately does not reproduce."""

    def scenario(impl):
        leg = isolated(impl)
        res = leg.run("create", "--kind", "shell", "--bogus", "x")
        return {"code": res.returncode, "stdout": res.stdout}

    assert differential(scenario)["code"] == 2


def test_stray_positional_is_bad_args(differential, isolated):
    def scenario(impl):
        return _err_cell(isolated(impl), "create", "--kind", "shell", "oops")

    assert differential(scenario)["code"] == 2


def test_missing_slug_flag_is_bad_args(differential, isolated):
    def scenario(impl):
        return _err_cell(isolated(impl), "probe")

    assert differential(scenario)["code"] == 2


def test_list_on_an_empty_server(differential, isolated):
    """The envelope with no sessions: `rc_sessions` is ALWAYS an array (never null
    or absent), and the `capabilities` block both implementations embed rides
    along (C5 — the C4 strip is gone, so this golden was deliberately
    re-recorded)."""

    def scenario(impl):
        leg = isolated(impl)
        res = leg.run("list")
        assert res.returncode == 0, f"{impl}: exit {res.returncode}: {res.stderr}"
        envelope = res.json()
        assert "capabilities" in envelope, envelope
        return mask_list(envelope, str(leg.home))

    envelope = differential(scenario)
    assert envelope["rc_sessions"] == []
    assert envelope["capabilities"]["rc_version"] == 4
