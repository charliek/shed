"""Cross-implementation interop — the MIXED-FLEET property (plan 009 §3.4).

Every other cell in this suite runs the two implementations side by side in
sealed worlds and asks "do they say the same thing?". These cells put them in the
SAME world — one tmux server, one HOME (the `shared` fixture flavor) — and ask
the question that actually decides whether a machine can have both binaries on
it: **can one implementation drive what the other created?**

That is not a corollary of the isolated differentials. A session is a tmux
session plus an env stamp plus a pane, and a preseeded config is a file on disk;
either implementation can be the one that wrote it and the one that reads it, in
any order, on the same machine, forever. The two failure modes this file exists
to catch are:

* a session one binary created that the other cannot see, read, prompt, or kill
  (an env-stamp or session-name drift the isolated differentials cannot expose,
  because there each binary only ever reads its own sessions);
* a config file the second writer REWRITES rather than merges — the same bytes
  question `test_preseed.py` asks within one implementation, asked across the
  boundary that matters.

How the cells stay differentials: the axis being varied is **which
implementation drives**, not which implementation is under test. for the
DRIVE cells (`chain`, `accept-trust`, the preseeds), `scenario(impl)` sets up the
world with the OTHER implementation and drives it with `impl` — so
`differential()` compares "Go drove a Rust-made world" against "Rust drove a
Go-made world" (or the mixed-fleet result against the pure-Go reference), each
leg on its own named shared rig. The READ cells (`go-created`/`rust-created`/
`coexist`) instead share ONE rig and ONE stored world across both legs — there
the fixed world is the point, and the varied axis is only the reader; their
volatile stamp fields are additionally raw-compared across legs since the masks
would otherwise hide a reader that invents them.

Slugs are pinned here as everywhere, and coexisting sessions take DISTINCT slugs
— one server is exactly where a repeated slug is the duplicate-slug error
(`test_exit_classes.test_duplicate_slug_is_exit_3`).
"""

from conftest import CLAUDE_READY, TRUST_DIALOG, reactive_shim
from normalize import mask_file_bytes, mask_list, mask_session, mask_stderr

# The claude-rc ready anchor the reactive shim redraws with (conftest owns the
# fixture text; the URL is what makes `ready` reachable — `agents.go:719-729`).
READY_URL = CLAUDE_READY[1]

# The marker a delivered prompt carries into the pane.
MARKER = "INTEROPMARKER"


def _other(impl: str) -> str:
    return "rust" if impl == "go" else "go"


def _create(rig, impl, kind, slug, *extra, name=None):
    res = rig.run(
        impl,
        "create",
        "--kind",
        kind,
        "--slug",
        slug,
        "--name",
        name or f"interop-{slug}",
        *extra,
    )
    assert res.returncode == 0, f"{impl} create: exit {res.returncode}: {res.stderr}"
    rig.wait_for_session(f"rc-{slug}")
    return res


# --- reading each other's sessions -----------------------------------------


def test_a_go_created_session_reads_identically_from_both(differential, shared):
    """A session created by the GO binary, then `probe`d and `list`ed by each
    implementation in turn against the same server.

    The Rust reads must equal the Go reads field for field — including the ones
    that are reconstructed rather than stored: `kind`, `created_by` and
    `created_at` come back out of the tmux env stamp Go wrote, and `state` comes
    out of the Rust classifier reading a pane a Go-launched agent drew."""
    rig = shared("go-created")
    _create(rig, "go", "codex", "io1111")
    rig.wait_for_pane("rc-io1111")

    raw = {}

    def scenario(impl):
        probed = rig.run(impl, "probe", "--slug", "io1111")
        assert probed.returncode == 0, f"{impl}: exit {probed.returncode}: {probed.stderr}"
        raw[impl] = probed.json()
        listed = rig.run(impl, "list")
        assert listed.returncode == 0, f"{impl}: exit {listed.returncode}: {listed.stderr}"
        return {
            "probe": mask_session(probed.json(), str(rig.home)),
            "list": mask_list(listed.json(), str(rig.home)),
        }

    cell = differential(scenario)
    assert cell["probe"]["state"] == "ready"
    assert cell["probe"]["kind"] == "codex"
    assert [s["slug"] for s in cell["list"]["rc_sessions"]] == ["io1111"]
    # The three fields the differential MASKS (uuid/timestamp can't live in a
    # golden) are exactly the env stamp being round-tripped here, so pin their
    # raw cross-leg equality: a reader that regenerated the id, restamped
    # created_at, or reported ITSELF as created_by would otherwise slip through
    # the shape-only masks (C6 review finding).
    for field in ("id", "created_at", "created_by"):
        assert raw["go"][field] == raw["rust"][field], (field, raw)


def test_a_rust_created_session_reads_identically_from_both(differential, shared):
    """The mirror: the RUST binary creates, and both implementations read it.

    Worth its own cell rather than folding into the one above — the two
    directions exercise different halves of the contract (Rust's env WRITER vs
    Go's env READER, and vice versa), and a drift in either one would be a
    mixed-fleet break in only one direction."""
    rig = shared("rust-created")
    _create(rig, "rust", "codex", "io2222")
    rig.wait_for_pane("rc-io2222")

    raw = {}

    def scenario(impl):
        probed = rig.run(impl, "probe", "--slug", "io2222")
        assert probed.returncode == 0, f"{impl}: exit {probed.returncode}: {probed.stderr}"
        raw[impl] = probed.json()
        listed = rig.run(impl, "list")
        assert listed.returncode == 0, f"{impl}: exit {listed.returncode}: {listed.stderr}"
        return {
            "probe": mask_session(probed.json(), str(rig.home)),
            "list": mask_list(listed.json(), str(rig.home)),
        }

    cell = differential(scenario)
    assert cell["probe"]["state"] == "ready"
    assert cell["probe"]["kind"] == "codex"
    assert [s["slug"] for s in cell["list"]["rc_sessions"]] == ["io2222"]
    # The three fields the differential MASKS (uuid/timestamp can't live in a
    # golden) are exactly the env stamp being round-tripped here, so pin their
    # raw cross-leg equality: a reader that regenerated the id, restamped
    # created_at, or reported ITSELF as created_by would otherwise slip through
    # the shape-only masks (C6 review finding).
    for field in ("id", "created_at", "created_by"):
        assert raw["go"][field] == raw["rust"][field], (field, raw)


def test_sessions_from_both_impls_coexist_in_one_server(differential, shared):
    """Two sessions of different kinds, created by different binaries, alive at
    once in ONE server — the case the isolated flavor structurally cannot reach
    (there, each implementation only ever sees its own sessions).

    Both implementations must enumerate BOTH rows, in the same order, with the
    same fields. Row order is contract here: `list` reads `tmux ls` and the two
    implementations must agree on how they present it, or a client paging a
    mixed-fleet machine sees the list reshuffle depending on which binary it
    happened to call."""
    rig = shared("coexist")
    # io4444 is created FIRST but sorts SECOND: creation order and name order
    # disagree, so the row-order assertion below actually discriminates between
    # "tmux name order" (the contract) and "creation order" (C6 review finding —
    # with the orders aligned, the assert proved nothing).
    _create(rig, "rust", "opencode", "io4444")
    _create(rig, "go", "codex", "io3333")
    rig.wait_for_pane("rc-io3333")
    rig.wait_for_pane("rc-io4444")

    raw = {}

    def scenario(impl):
        listed = rig.run(impl, "list")
        assert listed.returncode == 0, f"{impl}: exit {listed.returncode}: {listed.stderr}"
        raw[impl] = listed.json()
        return mask_list(listed.json(), str(rig.home))

    envelope = differential(scenario)
    rows = envelope["rc_sessions"]
    assert [s["slug"] for s in rows] == ["io3333", "io4444"], rows
    assert [s["kind"] for s in rows] == ["codex", "opencode"], rows
    # The masked golden can't carry created_by (the two rows differ by maker),
    # so pin the RAW rows instead: each row reports its MAKER's provenance
    # identically to both readers — a reader stamping itself would diverge here.
    go_by = [s["created_by"] for s in raw["go"]["rc_sessions"]]
    rust_by = [s["created_by"] for s in raw["rust"]["rc_sessions"]]
    assert go_by == rust_by == ["shed-machine-rc", "sx"], (go_by, rust_by)


# --- driving each other's sessions -----------------------------------------


def test_the_full_chain_works_in_both_directions(differential, shared):
    """create → prompt → kill → probe, with the create done by ONE binary and
    the prompt/kill by the OTHER, run both ways round and compared.

    This is the lifecycle a mixed fleet actually performs: something created the
    session earlier (a skill, a server, the other binary's release), and the
    thing steering it now is whatever is on this machine's PATH today. Each step
    is a different seam — `prompt` re-reads the env stamp AND the pane
    classification (it refuses a session that is not `ready`), `kill` addresses
    the tmux session by the name the creator chose, and the creator's own `probe`
    afterwards is what proves the kill really removed it rather than the killer
    merely reporting success (exit 4, the gone-session class)."""
    slug = "io5555"

    def scenario(impl):
        maker = _other(impl)
        rig = shared(f"chain-{impl}")
        _create(rig, maker, "codex", slug)
        # The prompt is refused unless the pane classifies `ready`, so the draw
        # is polled to a deadline rather than raced.
        rig.wait_for_pane(f"rc-{slug}", "Find and fix a bug")

        prompted = rig.run(impl, "prompt", "--slug", slug, stdin=f"{MARKER} do the thing\n")
        assert prompted.returncode == 0, f"{impl}: prompt: {prompted.stderr}"
        pane = rig.wait_for_pane(f"rc-{slug}", MARKER)

        killed = rig.run(impl, "kill", "--slug", slug)
        gone = rig.run(maker, "probe", "--slug", slug)
        assert f"rc-{slug}" not in rig.sessions(), "the cross-impl kill left the session"
        return {
            "prompt": {
                "code": prompted.returncode,
                "stdout": prompted.stdout,
                "stderr": mask_stderr(prompted.stderr, str(rig.home)),
            },
            "marker_count": pane.count(MARKER),
            "kill": {
                "code": killed.returncode,
                "stdout": killed.stdout,
                "stderr": mask_stderr(killed.stderr, str(rig.home)),
            },
            "probe_after_kill": gone.returncode,
        }

    cell = differential(scenario)
    assert cell["prompt"]["code"] == 0
    assert cell["prompt"]["stdout"] == "", "prompt writes nothing on success"
    assert cell["marker_count"] >= 1, "the delivered line never reached the pane"
    assert cell["kill"]["code"] == 0
    assert cell["probe_after_kill"] == 4, "the creator must see the session as gone"


def test_accept_trust_works_across_the_boundary(differential, shared):
    """One binary creates a claude session that stops at the first-run trust
    dialog; the OTHER answers it; the first one then sees `ready`.

    Deliberately created WITHOUT `--wait`: the wait poller would auto-accept the
    dialog itself (that path is `test_wait.py`'s), and there would be nothing
    left for the second binary to do. Here the session genuinely sits at the
    dialog until the cross-implementation `accept-trust` arrives, and the
    reactive shim's stdin transcript is the proof of what was sent — one Enter
    (`0a`), nothing before it."""
    shims = {"claude": reactive_shim(TRUST_DIALOG)}
    slug = "io6666"

    def scenario(impl):
        maker = _other(impl)
        rig = shared(f"trust-{impl}", shims)
        _create(rig, maker, "claude-rc", slug)
        rig.wait_for_pane(f"rc-{slug}", TRUST_DIALOG[0])

        accepted = rig.run(impl, "accept-trust", "--slug", slug)
        assert accepted.returncode == 0, f"{impl}: accept-trust: {accepted.stderr}"
        keystrokes = rig.wait_for_stdin_hex(1)
        # The shim redraws its ready screen only after the keystroke lands, so
        # this poll is what makes the probe below deterministic.
        rig.wait_for_pane(f"rc-{slug}", READY_URL)

        probed = rig.run(maker, "probe", "--slug", slug)
        assert probed.returncode == 0, f"{maker}: probe: {probed.stderr}"
        return {
            "accept_trust": {
                "code": accepted.returncode,
                "stdout": accepted.stdout,
                "stderr": mask_stderr(accepted.stderr, str(rig.home)),
            },
            "keystrokes": keystrokes,
            "probe_by_creator": mask_session(probed.json(), str(rig.home)),
        }

    cell = differential(scenario)
    assert cell["keystrokes"] == ["0a"], "an Enter, with nothing sent before it"
    assert cell["probe_by_creator"]["state"] == "ready"
    assert cell["probe_by_creator"]["url"] == READY_URL


# --- merging into each other's files ---------------------------------------
#
# The preseed cells below are differentials in a slightly different key: the
# scenario's `impl` names ONE of the two sequential writers, so the `go` leg is
# always the PURE-GO reference (Go wrote it, Go merged on top) and the `rust` leg
# is the mixed-fleet sequence. `differential()` therefore asserts exactly the
# property that matters — "the mixed sequence leaves the same bytes as the
# all-Go sequence" — and pins the all-Go bytes as the golden.
#
# Both writers share ONE HOME (that is the whole point) and both creates pin the
# same `--workdir`, outside the HOME, so the `projects` key the claude preseed
# writes is a masked constant rather than a per-leg path.


def _second_writer_bytes(rig, first, second, artifact, kind, workdir):
    """`first` creates, then `second` creates in the same HOME; return the
    artifact's bytes afterwards.

    Also asserts the second create left the first writer's file BYTE-UNTOUCHED —
    without that, a second writer that REWROTE the file wholesale would still
    produce the reference bytes (both creates pin the same workdir, so a
    from-scratch rewrite equals the merge result) and the cell could not fail on
    the very failure mode it exists for (C6 review finding). This is the same
    `first == second` discipline `test_preseed.py`'s within-impl idempotence
    cell pins."""
    _create(rig, first, kind, "pi1111", "--workdir", str(workdir))
    before = rig.read_bytes(artifact)
    _create(rig, second, kind, "pi2222", "--workdir", str(workdir))
    after = rig.read_bytes(artifact)
    assert before == after, (
        f"{second} did not leave {first}'s {artifact} byte-identical"
    )
    return after


def test_claude_config_merge_survives_a_rust_writer_on_top_of_go(
    differential, shared, tmp_path
):
    """Go writes `~/.claude.json`, then the second create merges on top: with Go
    (the reference) and with Rust (the mixed fleet). The bytes must be the same
    file either way — if they are not, every create on a mixed machine rewrites
    the user's config and churns state nobody touched."""

    def scenario(impl):
        rig = shared(f"claude-second-{impl}")
        return mask_file_bytes(
            _second_writer_bytes(rig, "go", impl, ".claude.json", "claude-rc", tmp_path),
            str(rig.home),
            str(tmp_path),
        )

    document = differential(scenario)
    assert '"hasTrustDialogAccepted": true' in document
    assert '"<workdir>": {' in document


def test_claude_config_merge_survives_go_on_top_of_a_rust_writer(
    differential, shared, tmp_path
):
    """The mirror sequence: the FIRST writer varies, Go always merges on top. A
    Rust-written document must be one Go reads back and leaves byte-identical —
    the direction that matters when `sx` runs first and the older Go binary (or a
    shed skill still calling it) runs next."""

    def scenario(impl):
        rig = shared(f"claude-first-{impl}")
        return mask_file_bytes(
            _second_writer_bytes(rig, impl, "go", ".claude.json", "claude-rc", tmp_path),
            str(rig.home),
            str(tmp_path),
        )

    assert '"fullscreenUpsellSeenCount": 999' in differential(scenario)


def test_cursor_hooks_merge_survives_a_rust_writer_on_top_of_go(
    differential, shared, tmp_path
):
    """`~/.cursor/hooks.json` across the boundary: the idempotent entry match is
    keyed on `shellQuote(scriptPath)`, so a quoting drift between the two writers
    would show up here as a DUPLICATED hook entry (ten entries become twenty) —
    which is why the always-quote form was ported verbatim rather than reusing a
    conditional quoter."""

    def scenario(impl):
        rig = shared(f"hooks-second-{impl}")
        return mask_file_bytes(
            _second_writer_bytes(rig, "go", impl, ".cursor/hooks.json", "cursor", tmp_path),
            str(rig.home),
        )

    document = differential(scenario)
    assert document.count("cursor-hook.sh") == 10, document


def test_cursor_hooks_merge_survives_go_on_top_of_a_rust_writer(
    differential, shared, tmp_path
):
    """The mirror: Rust writes the hooks file first, Go merges on top."""

    def scenario(impl):
        rig = shared(f"hooks-first-{impl}")
        return mask_file_bytes(
            _second_writer_bytes(rig, impl, "go", ".cursor/hooks.json", "cursor", tmp_path),
            str(rig.home),
        )

    document = differential(scenario)
    assert document.count("cursor-hook.sh") == 10, document
