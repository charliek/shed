"""The create-time preseeds as a **RAW-BYTES** surface (plan 009 §3.5).

`~/.claude.json`, `~/.cursor/hooks.json`, the hub's cursor hook script and a
plan file are all merged/rewritten IN PLACE by whichever implementation happens
to run a create — a mixed fleet, on one machine, against one file. Semantic
equality is not enough there: if the two writers disagree on key order,
indentation, HTML escaping or number fidelity, each create rewrites the other's
file and churns content the user never touched. So these cells compare the bytes,
with no canonicalization beyond substituting the leg's isolated HOME (see
`normalize.mask_file_bytes`).

Every create pins `--workdir` at a directory OUTSIDE both HOMEs, so the
`projects` key the claude preseed writes is byte-identical on both legs rather
than differing by the leg's home.
"""

import base64

from normalize import mask_file_bytes, mask_stderr

CLAUDE_KIND = ["--kind", "claude-rc"]
CURSOR_KIND = ["--kind", "cursor"]

CLAUDE_JSON = ".claude.json"
HOOKS_JSON = ".cursor/hooks.json"
HOOK_SCRIPT = ".shed-rc-hub/cursor-hook.sh"

# The refusal both implementations must produce for a file they cannot account
# for. Everything past this marker is the JSON parser's own wording (Go's
# `encoding/json` vs this port's), which is explicitly not contract.
REFUSAL_HEAD = "is not valid JSON; leaving untouched:"


def _create(leg, kind_args, slug, workdir, *extra, stdin=None):
    res = leg.run(
        "create",
        *kind_args,
        "--slug",
        slug,
        "--name",
        f"parity-{slug}",
        "--workdir",
        str(workdir),
        *extra,
        stdin=stdin,
    )
    return res


def _claude_bytes(leg, seed, slug, workdir):
    """Seed `~/.claude.json` (when `seed` is not None), run a claude create, and
    return the resulting file's bytes."""
    if seed is not None:
        (leg.home / CLAUDE_JSON).write_bytes(seed)
    res = _create(leg, CLAUDE_KIND, slug, workdir)
    assert res.returncode == 0, f"{leg.impl}: exit {res.returncode}: {res.stderr}"
    leg.wait_for_session(f"rc-{slug}")
    return leg.read_bytes(CLAUDE_JSON)


def _claude_cell(differential, isolated, tmp_path, seed, slug="cc1111"):
    """The shared shape of every claude.json cell: same seed, same workdir, both
    implementations, compared as bytes."""
    workdir = tmp_path

    def scenario(impl):
        leg = isolated(impl)
        return mask_file_bytes(
            _claude_bytes(leg, seed, slug, workdir), str(leg.home), str(workdir)
        )

    return differential(scenario)


# --- ~/.claude.json ---------------------------------------------------------


def test_claude_json_fresh_write(differential, isolated, tmp_path):
    """No config at all: the document the merge creates from nothing — two-space
    indent, keys sorted, `999` written as an integer."""
    document = _claude_cell(differential, isolated, tmp_path, None)
    assert '"<workdir>": {' in document, document
    assert '"hasTrustDialogAccepted": true' in document
    assert '"fullscreenUpsellSeenCount": 999' in document


def test_claude_json_empty_file(differential, isolated, tmp_path):
    """A zero-byte file is treated as an empty object, not as malformed."""
    _claude_cell(differential, isolated, tmp_path, b"")


def test_claude_json_null_document(differential, isolated, tmp_path):
    """A literal `null` decodes the map to nil in Go; both writers must RE-SEED an
    object rather than crash or write `null`."""
    _claude_cell(differential, isolated, tmp_path, b"null")


def test_claude_json_preserves_unknown_keys(differential, isolated, tmp_path):
    """Merge — never clobber: OAuth/MCP-shaped state, another project's entry, a
    nested object and an array all round-trip verbatim (and come back out in Go's
    sorted-key order at EVERY level)."""
    seed = (
        b'{"oauthAccount":{"emailAddress":"x@y.z","scopes":["a","b"]},'
        b'"mcpServers":{"foo":{"command":"bar","args":["--x","--y"],"env":{}}},'
        b'"projects":{"/other":{"hasTrustDialogAccepted":true,"history":[{"n":1}]}},'
        b'"tipsHistory":{},"emptyList":[]}'
    )
    document = _claude_cell(differential, isolated, tmp_path, seed)
    assert '"emptyList": []' in document
    assert '"tipsHistory": {}' in document


def test_claude_json_number_fidelity(differential, isolated, tmp_path):
    """The §3.1 number set: every literal here is corrupted by a decode through
    `f64` (or an integer), and all of them appear in real configs."""
    seed = (
        b'{"big":9007199254740993,"huge":18446744073709551615,"exp":1e10,'
        b'"trailingZero":0.10,"negZero":-0,"nested":{"floats":[1.0,2.50,3.14159]}}'
    )
    document = _claude_cell(differential, isolated, tmp_path, seed)
    for literal in ("9007199254740993", "18446744073709551615", "1e10", "0.10", "-0"):
        assert literal in document, f"{literal} was corrupted:\n{document}"


def test_claude_json_escaping(differential, isolated, tmp_path):
    """Go's encoder HTML-escapes `<`, `>`, `&` and the two JavaScript line
    terminators, and leaves every other non-ASCII rune raw. This is where a naive
    writer diverges first.

    The control bytes are the subtle half. Go gives 0x08 and 0x0c the SHORT forms
    `\\b` and `\\f` — alongside the better-known `\\t`/`\\n`/`\\r` — while every
    other sub-0x20 byte (0x0b, sitting right between them, is the trap) goes out
    as generic `\\u00xx`. A writer that spells 0x08/0x0c the generic way produces
    a file the other implementation rewrites wholesale on its next create."""
    seed = (
        '{"note":"a<b>c&d","js":"x\u2028y\u2029z","uni":"h\u00e9llo \u4e16\u754c \U0001f389",'
        '"ctl":"tab\\tnl\\nquote\\"back\\\\bs\\bff\\f","del":"\\u007f",'
        '"vt":"\\u000b"}'
    ).encode("utf-8")
    document = _claude_cell(differential, isolated, tmp_path, seed)
    assert r"a\u003cb\u003ec\u0026d" in document
    assert r"x\u2028y\u2029z" in document
    assert "h\u00e9llo \u4e16\u754c \U0001f389" in document
    # The short forms, and the generic form for the byte that sits between them.
    assert r"bs\bff\f" in document
    assert r"tab\tnl\nquote\"back\\" in document
    assert r"\u000b" in document


def test_claude_json_trailing_garbage_is_refused(differential, isolated, tmp_path):
    """A file with content after the top-level value cannot be accounted for, so
    the preseed DECLINES: the file is left byte-identical, the create still
    succeeds (a preseed is best-effort), and the reason lands on stderr."""
    original = b'{"theme":"dark"} garbage after'
    workdir = tmp_path

    def scenario(impl):
        leg = isolated(impl)
        (leg.home / CLAUDE_JSON).write_bytes(original)
        res = _create(leg, CLAUDE_KIND, "cc2222", workdir)
        assert res.returncode == 0, f"{impl}: a preseed must never fail a create"
        leg.wait_for_session("rc-cc2222")
        return {
            "bytes": mask_file_bytes(
                leg.read_bytes(CLAUDE_JSON), str(leg.home), str(workdir)
            ),
            "warning": mask_stderr(
                res.stderr, str(leg.home), truncate_after=REFUSAL_HEAD
            ),
        }

    cell = differential(scenario)
    assert cell["bytes"] == original.decode()
    assert "claude preseed skipped" in cell["warning"]


def test_claude_json_merge_is_idempotent(differential, isolated, tmp_path):
    """Two creates of the SAME workdir leave the file byte-identical — the
    property that keeps a mixed fleet from rewriting the file on every create."""
    workdir = tmp_path

    def scenario(impl):
        leg = isolated(impl)
        first = _claude_bytes(leg, None, "cc3333", workdir)
        second = _claude_bytes(leg, None, "cc4444", workdir)
        assert first == second, f"{impl}: the second create rewrote the file"
        return mask_file_bytes(second, str(leg.home), str(workdir))

    differential(scenario)


# --- ~/.cursor/hooks.json + the hub script ----------------------------------


def test_cursor_hook_script_bytes(differential, isolated, tmp_path):
    """The hub-owned script: byte-identical (it is rewritten on EVERY create by
    whichever binary runs it) and 0755, because cursor execs it directly."""

    def scenario(impl):
        leg = isolated(impl)
        res = _create(leg, CURSOR_KIND, "cu1111", tmp_path)
        assert res.returncode == 0, f"{impl}: exit {res.returncode}: {res.stderr}"
        leg.wait_for_session("rc-cu1111")
        path = leg.home / HOOK_SCRIPT
        return {
            "script": mask_file_bytes(path.read_bytes(), str(leg.home)),
            "mode": oct(path.stat().st_mode & 0o777),
        }

    cell = differential(scenario)
    assert cell["mode"] == "0o755"
    # The confinement controls the script's doc calls load-bearing.
    assert "--noproxy '*'" in cell["script"]
    assert "127.0.0.1:1029/v1/ingest/cursor" in cell["script"]


def test_cursor_hooks_fresh_write(differential, isolated, tmp_path):
    """A fresh `hooks.json`: every wired event gets exactly our entry, the schema
    `version` is supplied, and the command is `shellQuote(script) + " " + event` —
    the ALWAYS-quoted form, which is what a Go-written entry looks like and what
    the idempotent match keys on."""

    def scenario(impl):
        leg = isolated(impl)
        res = _create(leg, CURSOR_KIND, "cu2222", tmp_path)
        assert res.returncode == 0, f"{impl}: exit {res.returncode}: {res.stderr}"
        leg.wait_for_session("rc-cu2222")
        return mask_file_bytes(leg.read_bytes(HOOKS_JSON), str(leg.home))

    document = differential(scenario)
    assert '"version": 1' in document
    assert "'<home>/.shed-rc-hub/cursor-hook.sh' sessionStart" in document
    # Deliberately unwired events must not appear at all.
    assert "afterAgentThought" not in document


def test_cursor_hooks_merge_is_idempotent(differential, isolated, tmp_path):
    """Two creates leave the file byte-identical — no duplicated entries."""

    def scenario(impl):
        leg = isolated(impl)
        for slug in ("cu3333", "cu4444"):
            res = _create(leg, CURSOR_KIND, slug, tmp_path)
            assert res.returncode == 0, f"{impl}: exit {res.returncode}: {res.stderr}"
            leg.wait_for_session(f"rc-{slug}")
        return mask_file_bytes(leg.read_bytes(HOOKS_JSON), str(leg.home))

    document = differential(scenario)
    assert document.count("cursor-hook.sh") == 10, document


def test_cursor_hooks_preserve_a_users_own_hooks(differential, isolated, tmp_path):
    """A user's existing entries keep their position and their own unknown fields,
    their declared `version` is never overwritten, an unknown top-level key
    survives, and an event we do not wire gains nothing."""
    seed = (
        b'{"version":2,'
        b'"hooks":{"beforeSubmitPrompt":[{"command":"/usr/local/bin/audit.sh","failClosed":true}],'
        b'"afterAgentThought":[{"command":"/usr/local/bin/thoughts.sh"}]},'
        b'"somethingElse":{"keep":"me"}}'
    )

    def scenario(impl):
        leg = isolated(impl)
        (leg.home / ".cursor").mkdir(parents=True, exist_ok=True)
        (leg.home / HOOKS_JSON).write_bytes(seed)
        res = _create(leg, CURSOR_KIND, "cu5555", tmp_path)
        assert res.returncode == 0, f"{impl}: exit {res.returncode}: {res.stderr}"
        leg.wait_for_session("rc-cu5555")
        return mask_file_bytes(leg.read_bytes(HOOKS_JSON), str(leg.home))

    document = differential(scenario)
    assert '"version": 2' in document
    assert '"failClosed": true' in document
    assert "thoughts.sh" in document
    # Ours is APPENDED after the user's — within that event's array (the events
    # themselves come out in Go's sorted-key order, so the comparison has to be
    # scoped to the array, not to the whole document).
    submit = document.split('"beforeSubmitPrompt": [')[1].split("]")[0]
    assert submit.index("audit.sh") < submit.index("cursor-hook.sh"), submit


# --- plan files -------------------------------------------------------------


def test_plan_file_bytes_and_mode(differential, isolated, tmp_path):
    """A `--plan-stdin` create writes the plan verbatim (NOT newline-trimmed — a
    plan is a document, not a line) at 0600, and composes the kickoff that
    references it.

    Driven with `codex`, whose static shim draws its ready anchor immediately, so
    the wait poller settles in one tick instead of the 20 s timeout."""
    plan = "# parity plan\n\n- step one\n- step two\n\ntrailing structure\n\n"

    def scenario(impl):
        leg = isolated(impl)
        res = leg.run(
            "create",
            "--kind",
            "codex",
            "--slug",
            "pp1111",
            "--name",
            "parity-plan",
            "--workdir",
            str(tmp_path),
            "--plan-stdin",
            stdin=plan,
        )
        assert res.returncode == 0, f"{impl}: exit {res.returncode}: {res.stderr}"
        path = leg.home / ".shed-plans/plan-pp1111.md"
        # The DTO is not part of THIS cell: `--workdir` deliberately points
        # outside both HOMEs (so the preseed's `projects` key is byte-identical
        # on the two legs), and `mask_session` requires a HOME-rooted workdir.
        # The create DTO surface is pinned by test_create.py.
        return {
            "plan": mask_file_bytes(path.read_bytes(), str(leg.home)),
            "mode": oct(path.stat().st_mode & 0o777),
            "state": res.json()["state"],
        }

    cell = differential(scenario)
    assert cell["plan"] == plan, "the plan must be written verbatim"
    assert cell["mode"] == "0o600"


def test_plan_file_carries_base64_framing(differential, isolated, tmp_path):
    """`--prompt-b64` framing rides alongside the plan (stdin stays reserved for
    the plan itself): the FILE is unchanged by it — the framing lands in the
    composed kickoff, not on disk."""
    plan = "# framed plan\n"
    framing = base64.b64encode(b"context line\n").decode()

    def scenario(impl):
        leg = isolated(impl)
        res = leg.run(
            "create",
            "--kind",
            "codex",
            "--slug",
            "pp2222",
            "--name",
            "parity-framed",
            "--workdir",
            str(tmp_path),
            "--plan-stdin",
            "--prompt-b64",
            framing,
            stdin=plan,
        )
        assert res.returncode == 0, f"{impl}: exit {res.returncode}: {res.stderr}"
        return mask_file_bytes(
            leg.read_bytes(".shed-plans/plan-pp2222.md"), str(leg.home)
        )

    assert differential(scenario) == plan
