"""PhaseTimer log-line parser.

A `shed-server` create emits one line per `CreateShed` that looks like:

    2026/05/28 02:21:51 timing: create name=foo backend=vz total=1993ms \
        setup=2ms image=2ms rootfs=7ms vm=4ms agent=1952ms credentials=24ms \
        err=<nil>

(`shed-server` >= v0.5.4 / PR #118 emits PhaseTimer; older servers do not.)

This module parses such a line into a typed `PhaseTimings` struct. The
suite uses it for the timing-threshold tests in `test_smoke.py` and for
assertions on which phase keys appear.

Brittleness assumptions (acceptable today; flagged for future maintainers):

  - `err=` is treated as the terminal field — everything after `err=` on
    the line becomes the error string. This is safe because
    `internal/backend/phasetimer.go:Finish` writes `err=` last. If a
    future change adds a new key after `err=`, that key+value would be
    silently absorbed into the error string. The
    `test_no_keys_after_err` parser test guards against that drift.
  - Duplicate phase keys (e.g. `repo=0ms ... clone=462ms repo=2ms` as
    emitted today by `shed create --repo`) are SUMMED. This matches the
    physical semantic ("total time spent in this phase across all
    spans"). §15 PR 1b (`backend.Phase` / `backend.Status` split) will
    eliminate duplicates at the source; this sum-on-parse becomes a
    no-op then.
"""

from __future__ import annotations

from dataclasses import dataclass, field
from typing import Optional


@dataclass
class PhaseTimings:
    """Parsed PhaseTimer line. All durations are integer milliseconds."""

    name: str
    backend: str
    total_ms: int
    phases: dict[str, int] = field(default_factory=dict)
    error: Optional[str] = None  # None == server reported err=<nil>

    @property
    def agent_ms(self) -> Optional[int]:
        return self.phases.get("agent")

    def __getitem__(self, key: str) -> int:
        return self.phases[key]

    def __contains__(self, key: str) -> bool:
        return key in self.phases


def parse_timing_line(line: str) -> Optional[PhaseTimings]:
    """Parse a single PhaseTimer log line into PhaseTimings, or None if the
    line doesn't carry a 'timing: create' marker.

    Robust to extra prefix/suffix content on the line (e.g. journald or
    Go-log timestamps) and to phase-key order changes. The trailing `err=…`
    value may contain whitespace; everything after `err=` is treated as the
    error string until end-of-line.
    """
    marker = "timing: create"
    idx = line.find(marker)
    if idx < 0:
        return None
    rest = line[idx + len(marker):].strip()

    name: Optional[str] = None
    backend: Optional[str] = None
    total_ms = 0
    phases: dict[str, int] = {}
    error: Optional[str] = None

    tokens = rest.split()
    i = 0
    while i < len(tokens):
        tok = tokens[i]
        if "=" not in tok:
            i += 1
            continue
        key, _, val = tok.partition("=")
        if key == "name":
            name = val
        elif key == "backend":
            backend = val
        elif key == "err":
            # `err=` value extends to end of line; rejoin remaining tokens.
            tail = " ".join([val] + tokens[i + 1:]).strip()
            error = None if tail == "<nil>" else tail
            break
        elif val.endswith("ms"):
            try:
                ms = int(val[:-2])
            except ValueError:
                i += 1
                continue
            if key == "total":
                total_ms = ms
            else:
                # Sum duplicates rather than last-write-wins so the
                # parsed `agent` (or `repo`) value reflects total time
                # spent in that phase, not just the last span. See the
                # module docstring for the §15 PR 1b note.
                phases[key] = phases.get(key, 0) + ms
        i += 1

    if name is None or backend is None:
        return None
    return PhaseTimings(
        name=name, backend=backend, total_ms=total_ms, phases=phases, error=error,
    )
