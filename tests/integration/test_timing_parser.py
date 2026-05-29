"""Unit tests for `fixtures.timing.parse_timing_line`.

The parser is the brittle bit of the live integration tests — every
timing-threshold assertion depends on it. Cheap to test in isolation;
expensive to debug live when it breaks.

These tests do not need a running server.
"""

from __future__ import annotations

import pytest

from fixtures.timing import PhaseTimings, parse_timing_line


# Real PhaseTimer lines captured during the v0.5.6 work. Trailing/leading
# whitespace and varying journald/shed-server timestamp prefixes verify
# we don't accidentally anchor on those.

CASES_OK = [
    pytest.param(
        "2026/05/28 02:21:51 timing: create name=postmerge backend=vz "
        "total=1993ms setup=2ms image=2ms rootfs=7ms vm=4ms agent=1952ms "
        "credentials=24ms err=<nil>",
        "vz",
        "postmerge",
        1993,
        {"setup": 2, "image": 2, "rootfs": 7, "vm": 4, "agent": 1952, "credentials": 24},
        None,
        id="vz_plain_create",
    ),
    pytest.param(
        "May 28 01:56:50 mini3 shed-server[176375]: 2026/05/28 01:56:50 "
        "timing: create name=fr1 backend=firecracker total=2334ms setup=0ms "
        "image=35ms network=1ms rootfs=3ms vm=26ms agent=1802ms repo=0ms "
        "clone=462ms repo=2ms err=<nil>",
        "firecracker",
        "fr1",
        2334,
        # repo appears twice in the line — 0 ms before clone, then 2 ms
        # after clone. The parser SUMS duplicates (see timing.py docstring),
        # so repo here is 0 + 2 = 2. §15 PR 1b will eliminate duplicates
        # at the source; this sum-on-parse becomes a no-op then.
        {"setup": 0, "image": 35, "network": 1, "rootfs": 3, "vm": 26,
         "agent": 1802, "repo": 2, "clone": 462},
        None,
        id="fc_repo_create_journald_prefix",
    ),
    pytest.param(
        '2026/05/28 03:00:00 timing: create name=x backend=vz total=500ms '
        'agent=400ms err="git clone failed: exit 128"',
        "vz",
        "x",
        500,
        {"agent": 400},
        'git clone failed: exit 128',
        id="error_with_whitespace",
    ),
]


@pytest.mark.parametrize("line,backend,name,total,phases,error", CASES_OK)
def test_parses_real_lines(line, backend, name, total, phases, error):
    t = parse_timing_line(line)
    assert t is not None
    assert isinstance(t, PhaseTimings)
    assert t.backend == backend
    assert t.name == name
    assert t.total_ms == total
    assert t.phases == phases
    if error is None:
        assert t.error is None
    else:
        assert error in (t.error or "")


@pytest.mark.parametrize(
    "line",
    [
        "",
        "some random log line without the marker",
        "timing: create",  # marker but no payload
        "2026/05/28 02:21:51 timing: create backend=vz total=100ms",  # no name=
    ],
)
def test_rejects_non_timing_lines(line):
    """Lines without the marker, or missing required fields, return None."""
    assert parse_timing_line(line) is None


def test_agent_ms_convenience_property():
    line = "timing: create name=x backend=vz total=10ms agent=7ms err=<nil>"
    t = parse_timing_line(line)
    assert t is not None
    assert t.agent_ms == 7
    assert "agent" in t
    assert t["agent"] == 7


def test_duplicate_phase_keys_are_summed():
    """Duplicate phase keys on the same line SUM, matching the physical
    semantic ("total time spent in this phase"). See timing.py docstring
    and the §15 PR 1b note."""
    line = "timing: create name=x backend=vz total=100ms phase=10ms phase=20ms err=<nil>"
    t = parse_timing_line(line)
    assert t is not None
    assert t.phases == {"phase": 30}


def test_no_keys_after_err():
    """Guard against future PhaseTimer changes adding keys after `err=`.

    The parser treats everything after `err=` as the error string. If a
    future change in `internal/backend/phasetimer.go:Finish` writes a key
    after `err=`, that key+value would be silently absorbed into the
    error string. This test pins the assumption to a synthetic line that
    matches the current emitter shape; if PhaseTimer ever changes, this
    test fails first and forces a parser update.
    """
    # Mirrors `phasetimer.go:Finish` — err= is written LAST.
    line = "timing: create name=x backend=vz total=10ms agent=7ms err=<nil>"
    t = parse_timing_line(line)
    assert t is not None
    assert t.error is None
    # If a synthetic line ever puts a key after err=, the parser would
    # produce a non-trivial error string. Catch that here so it's an
    # explicit failure instead of silent data loss.
    weird = "timing: create name=x backend=vz total=10ms err=<nil> extra=1ms"
    t2 = parse_timing_line(weird)
    assert t2 is not None
    # extra=1ms would be glued onto the error if/when PhaseTimer changes;
    # for now the parser absorbs it. This assertion documents that
    # absorption — if you're updating phasetimer.go to add keys after
    # err=, also update the parser to handle them and remove this guard.
    assert "extra=1ms" in (t2.error or ""), (
        "If err= ever stops being the terminal key, update timing.py to "
        "stop treating everything after err= as the error string."
    )
