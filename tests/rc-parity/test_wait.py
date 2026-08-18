"""`create --wait` — the poller's keystrokes and its kickoff delivery, driven by
REACTIVE PATH shims (plan 009 §3.6).

A static pane cannot prove any of this: the poller would classify the same screen
every tick until the 20 s timeout and the cell would assert only that nothing
happened. The reactive shims instead draw a dialog, BLOCK on stdin recording every
byte that arrives, and redraw as ready — so a cell finishes in about one poll tick
and the recorded transcript is direct evidence of which keys the engine sent:

| scenario | what the engine must send |
|---|---|
| trust dialog | a single `Enter` |
| bypass dialog (`--skip` on a claude kind) | `Down`, then `Enter` |

The transcript is hex because it is a BYTE surface: the pane's tty is in canonical
mode, so `Enter` (a CR on the wire) is delivered to the process as LF and the
`Down` escape sequence arrives in the same read burst as the Enter that completes
the line. `1b 5b 42` is `ESC [ B`.
"""

import base64

from conftest import BYPASS_DIALOG, CLAUDE_READY, TRUST_DIALOG, reactive_shim, static_shim
from normalize import mask_session

# The URL the reactive claude shim prints. `claude-rc` reaches `ready` only with a
# session URL present (`agents.go:719-729`), so this is what makes the ready
# transition observable at all — and it is deliberately NOT masked: a synthetic,
# pinned URL is a stronger assertion than `<url>`.
READY_URL = "https://claude.ai/code/session_TESTTEST"

# The marker a delivery cell counts in the pane. The kickoff itself embeds the
# leg's HOME (the plan path), so the cells compare a COUNT, not the pane text.
MARKER = "PARITYMARKER"


def test_trust_dialog_is_auto_accepted(differential, isolated):
    """claude's first-run workspace-trust prompt: the poller classifies
    `needs_trust`, sends an Enter, and the session reaches `ready`.

    What this cell actually pins is the exact PREFIX of the byte stream, up to and
    including that first Enter: a wrong keystroke, a wrong order, or any extra
    byte sent BEFORE the Enter fails it. What it does not pin is a trailing
    duplicate — the reactive shim stops recording once the Enter completes its
    line, so a second Enter on a later tick would be invisible here. The latch
    that prevents one is covered by the engine's own unit tests."""
    shims = {"claude": reactive_shim(TRUST_DIALOG)}
    slug = "tt1111"

    def scenario(impl):
        leg = isolated(impl, shims)
        res = leg.run(
            "create",
            "--kind",
            "claude-rc",
            "--slug",
            slug,
            "--name",
            "parity-trust",
            "--wait",
        )
        assert res.returncode == 0, f"{impl}: exit {res.returncode}: {res.stderr}"
        return {
            "dto": mask_session(res.json(), str(leg.home)),
            "keystrokes": leg.wait_for_stdin_hex(1),
        }

    cell = differential(scenario)
    assert cell["dto"]["state"] == "ready"
    assert cell["dto"]["url"] == READY_URL
    assert cell["keystrokes"] == ["0a"], "an Enter, with nothing sent before it"


def test_bypass_dialog_gets_down_then_enter(differential, isolated):
    """The one-time "Bypass Permissions mode" dialog pre-selects "1. No, exit", so
    a bare Enter would EXIT the session. The poller must move down first — and only
    for a claude kind whose resolved posture is full bypass, which `--skip` is."""
    shims = {"claude": reactive_shim(BYPASS_DIALOG)}
    slug = "bb1111"

    def scenario(impl):
        leg = isolated(impl, shims)
        res = leg.run(
            "create",
            "--kind",
            "claude-rc",
            "--slug",
            slug,
            "--name",
            "parity-bypass",
            "--skip",
            "--wait",
        )
        assert res.returncode == 0, f"{impl}: exit {res.returncode}: {res.stderr}"
        return {
            "dto": mask_session(res.json(), str(leg.home)),
            "keystrokes": leg.wait_for_stdin_hex(4),
            # 5 elements: `--remote-control --name <n> --permission-mode <m>`.
            "argv": leg.wait_for_agent_argv(5),
        }

    cell = differential(scenario)
    assert cell["dto"]["state"] == "ready"
    assert cell["keystrokes"] == ["1b", "5b", "42", "0a"], "Down (ESC [ B) then Enter"
    # …and the posture that gates the dialog really was on the command line: a
    # claude-rc create WITH a posture takes the `--remote-control` flag form (the
    # bare `/rc` slash command takes no flags), `agents.go:616-624`.
    assert cell["argv"] == [
        "--remote-control",
        "--name",
        "parity-bypass",
        "--permission-mode",
        "bypassPermissions",
    ]


def test_single_line_kickoff_is_delivered_once(differential, isolated):
    """A single-line kickoff goes out as `send-keys -l` + `Enter`. The shim holds
    the pane open on `cat`, so the delivered line lands in the pane — the same
    number of times on both implementations (the tty echoes it as typed AND `cat`
    writes it back; what is contract is that it is delivered ONCE, not the echo
    count)."""
    shims = {"claude": static_shim(CLAUDE_READY)}
    slug = "kk1111"

    def scenario(impl):
        leg = isolated(impl, shims)
        res = leg.run(
            "create",
            "--kind",
            "claude-rc",
            "--slug",
            slug,
            "--name",
            "parity-kickoff",
            "--prompt-stdin",
            stdin=f"{MARKER} do the thing\n",
        )
        assert res.returncode == 0, f"{impl}: exit {res.returncode}: {res.stderr}"
        pane = leg.wait_for_pane(f"rc-{slug}", MARKER)
        return {
            "dto": mask_session(res.json(), str(leg.home)),
            "marker_count": pane.count(MARKER),
            "tail": pane.strip().split("\n")[-1].strip(),
        }

    cell = differential(scenario)
    assert cell["dto"]["state"] == "ready"
    assert cell["marker_count"] >= 1
    assert MARKER in cell["tail"]


def test_multiline_kickoff_is_delivered(differential, isolated):
    """A MULTI-line kickoff takes the other delivery path: `set-buffer` +
    `paste-buffer -p -d` (bracketed paste) instead of `send-keys -l`. The
    assertion is deliberately coarse — the delivered text arrives in the pane, the
    same way on both implementations — because the composed kickoff embeds the
    leg's own plan path.

    The multi-line-ness comes from `--prompt-b64` framing, which is prepended to
    the composed "read the plan" sentence."""
    shims = {"claude": static_shim(CLAUDE_READY)}
    slug = "kk2222"
    framing = base64.b64encode(f"{MARKER} first line\n{MARKER} second line".encode()).decode()

    def scenario(impl):
        leg = isolated(impl, shims)
        res = leg.run(
            "create",
            "--kind",
            "claude-rc",
            "--slug",
            slug,
            "--name",
            "parity-paste",
            "--plan-stdin",
            "--prompt-b64",
            framing,
            stdin="# parity plan\n\n- do the thing\n",
        )
        assert res.returncode == 0, f"{impl}: exit {res.returncode}: {res.stderr}"
        pane = leg.wait_for_pane(f"rc-{slug}", MARKER)
        return {
            "dto": mask_session(res.json(), str(leg.home)),
            "marker_count": pane.count(MARKER),
            "mentions_the_plan": "Read the plan at" in pane,
        }

    cell = differential(scenario)
    assert cell["dto"]["state"] == "ready"
    assert cell["mentions_the_plan"], "the composed kickoff must reference the plan"
    assert cell["marker_count"] >= 2, "both framing lines arrived"
