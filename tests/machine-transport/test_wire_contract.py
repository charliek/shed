"""The LIVE leg: does the agreed quoting really deliver the intended argv?

Precisely what this leg does and does not measure, because the distinction
matters:

* It sends a line composed by the same RULE as `goldens/wire.json` — but with
  `argv[0]` swapped for the receiver script, since the scenarios' `argv[0]` (`sx`,
  an absolute path on some machine) does not exist on this host. So it does not
  literally transmit the golden bytes; it transmits the same quoting applied to a
  probe.
* What it asserts is the property that actually protects users: the remote
  process received **exactly `scenarios.json`'s argv**. That expectation comes
  from the contract, NOT from either golden and not from the composer — so no
  amount of `UPDATE_GOLDEN=1` can paper over a quoting bug that changes delivered
  argv.

The BYTE-level pin (that Rust still emits always-quoted form rather than drifting
to a conditional quoter, which post-`bash` would be indistinguishable here) lives
in the Rust leg: `crates/shed-core/tests/machine_transport_contract.rs`. The two
legs together cover both layers; neither covers both alone.
"""

from __future__ import annotations

from pathlib import Path

import pytest

from conftest import decode_received, load_golden, run_wire, save_golden, updating


def _wire_line(argv: list[str]) -> str:
    """Compose the wire line the way `shed_core::machine::display_line` does:
    ALWAYS single-quote every element, escaping `'` as `'\\''`.

    Deliberately reimplemented here in five lines rather than shelled out to
    Rust. This leg's job is to be an INDEPENDENT check on the wire golden — if
    it called the Rust implementation, a quoter bug would be invisible because
    both sides would carry it.
    """
    return " ".join("'" + a.replace("'", "'\\''") + "'" for a in argv)


def test_the_wire_golden_matches_an_independent_composition(scenarios):
    """The recorded wire line is what the house quoter's RULE produces.

    This is the cheap guard that the golden was not recorded from a broken
    implementation: the rule ("always single-quote, escape ' as '\\''") is
    restated here from the spec, not imported.
    """
    wire = load_golden("wire.json")
    recorded = {}
    for scenario in scenarios:
        recorded[scenario["id"]] = _wire_line(scenario["argv"])
    if updating():
        save_golden("wire.json", recorded)
        wire = recorded
    for scenario in scenarios:
        sid = scenario["id"]
        assert sid in wire, f"no wire golden for {sid} — record with UPDATE_GOLDEN=1"
        assert wire[sid] == recorded[sid], (
            f"{sid}: the wire golden disagrees with the quoting rule.\n"
            f"  golden: {wire[sid]!r}\n"
            f"  rule:   {recorded[sid]!r}"
        )


@pytest.mark.live
def test_the_wire_line_delivers_the_intended_argv(sshd, receiver, scenarios):
    """**The measurement that matters**: run each wire line over a real sshd and
    assert the remote process received exactly the scenario's argv.

    A quoting bug shows up here as a split, merged, expanded, or vanished
    argument — the failure modes that actually reach a user (a session named
    `it's mine` losing its tail, a `$HOME` expanding on the far side, an
    injected `; rm` running).
    """
    wire = load_golden("wire.json")
    received_golden = load_golden("received.json")
    recorded = {}

    for scenario in scenarios:
        sid = scenario["id"]
        argv = scenario["argv"]
        # argv[0] is replaced by the receiver: we are measuring how the ARGUMENTS
        # arrive, not resolving a binary that does not exist on this host.
        probe_argv = [receiver] + argv[1:]
        line = _wire_line(probe_argv)

        proc = run_wire(sshd, line)
        assert proc.returncode == 0, (
            f"{sid}: ssh failed rc={proc.returncode}\n"
            f"  wire: {line!r}\n  stderr: {proc.stderr}"
        )
        got = decode_received(proc.stdout)
        want = argv[1:]

        recorded[sid] = got
        if not updating():
            assert sid in received_golden, (
                f"no received golden for {sid} — record with UPDATE_GOLDEN=1"
            )
            assert got == received_golden[sid], (
                f"{sid}: the remote received different argv than the golden.\n"
                f"  golden:   {received_golden[sid]!r}\n"
                f"  received: {got!r}"
            )
        # …and independently of the golden: what arrived IS the scenario's argv.
        assert got == want, (
            f"{sid}: the remote process received the WRONG argv.\n"
            f"  wire sent: {line!r}\n"
            f"  expected:  {want!r}\n"
            f"  received:  {got!r}"
        )
        # Cross-check the composed line against the shared wire golden, so this
        # leg cannot drift from the one the other two legs read.
        assert wire[sid] == _wire_line(argv), f"{sid}: wire golden drift"

    if updating():
        save_golden("received.json", recorded)


@pytest.mark.live
def test_injection_attempts_never_execute_remotely(sshd, receiver):
    """The security property stated plainly, separate from the table.

    If quoting were wrong, these would run on the far side. The assertion is
    that the payload arrives as inert TEXT and that its side effect never
    happened.

    The marker lives under the per-session sshd root, NOT a shared `/tmp` path:
    two concurrent runs of this suite (two checkouts, two pytest sessions) would
    otherwise share one filename, and run B's cleanup landing between run A's
    injection and A's check would make a REAL execution look clean.
    """
    marker = str(Path(sshd["root"]) / "pwn-marker")
    subprocess_argv = [
        receiver,
        f"; touch {marker}",
        f"$(touch {marker})",
        f"`touch {marker}`",
        f"&& touch {marker}",
        f"| touch {marker}",
    ]
    line = _wire_line(subprocess_argv)

    # Make sure a stale marker from an earlier run cannot mask a real failure.
    cleanup = run_wire(sshd, _wire_line(["/bin/rm", "-f", marker]))
    assert cleanup.returncode == 0, cleanup.stderr

    proc = run_wire(sshd, line)
    assert proc.returncode == 0, proc.stderr

    check = run_wire(
        sshd,
        _wire_line(["/bin/sh", "-c", f"test -e {marker} && echo PWNED || echo clean"]),
    )
    # The probe must have RUN, and must have said exactly "clean". A substring
    # test would accept a probe that printed both, and skipping the returncode
    # would let a probe that never ran (sshd hiccup, unexpected banner) pass as
    # evidence of safety.
    assert check.returncode == 0, f"the probe itself failed: {check.stderr!r}"
    assert check.stdout.strip() == "clean", (
        f"a quoted payload EXECUTED remotely — quoting is broken.\n"
        f"  wire: {line!r}\n  probe: {check.stdout!r}"
    )

    # And every payload arrived intact as an argument.
    got = decode_received(proc.stdout)
    assert got == subprocess_argv[1:], f"payloads were mangled: {got!r}"


def test_the_contract_is_versioned_and_non_empty(contract):
    """A guard on the shared file itself: shed-mobile's Dart leg pins this
    version, so an unversioned edit here would let the two repos diverge with
    every test green on both sides."""
    assert isinstance(contract.get("version"), int), "scenarios.json needs an integer version"
    assert contract["scenarios"], "the contract has no scenarios"
    ids = [s["id"] for s in contract["scenarios"]]
    assert len(ids) == len(set(ids)), f"duplicate scenario ids: {ids}"
    for s in contract["scenarios"]:
        assert s.get("why"), f"scenario {s['id']!r} must say why it exists"
        assert isinstance(s.get("argv"), list) and s["argv"], f"scenario {s['id']!r} needs argv"
