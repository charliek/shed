"""Canonicalization + volatile-field masking for the RC parity harness.

Two disciplines inherited from `tests/host-agent-diff/normalize.py`:

* **D2 — structural canonical JSON, never raw bytes.** `canonical()` recursively
  sorts object keys; lists stay order-sensitive. This is the comparison model plan
  009 §3.5 pins for a `/v1` body: Go's `json.Encoder` HTML-escapes `<`/`>`/`&` and
  appends a newline, serde_json does neither, and every consumer parses — so field
  PRESENCE is contract, byte shape is not.

* **D3 — determinism over blanking.** Mask as little as possible and SHAPE-ASSERT
  before masking, so a mask can never hide a malformed value. Distinct sentinels
  make a cross-field leak obvious when eyeballing a golden.

The sentinels are exactly the axes on which two correct implementations (or two
runs) must differ: a fresh uuid, a wall-clock stamp, a feed row's sequence
number, the isolated HOME each leg runs under, and the pid + version of a
resident daemon. `<prog>` is the odd one out — the binary's own name, pinned as a
masked token rather than a divergence.
"""

from __future__ import annotations

import os
import re
from typing import Any

MASK_ID = "<id>"
MASK_TS = "<ts>"
MASK_SEQ = "<seq>"
MASK_PID = "<pid>"
MASK_HOME = "<home>"
MASK_PROG = "<prog>"
MASK_VERSION = "<version>"

# RFC3339 with second precision. Go stamps `time.Now().UTC().Format(time.RFC3339)`
# and the Rust engine's clock seam formats seconds-precision UTC with a `Z`; an
# offset form is accepted too so the shape check isn't brittle.
_RFC3339 = re.compile(
    r"^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d+)?(?:Z|[+-]\d{2}:\d{2})$"
)

_UUID = re.compile(r"^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$")

# Every program name that can appear in a `created_by` stamp. `shed-ext-rc` is
# masked as belt: pre-C2 Go emitted it from `warnHook`/`EnsureHub` regardless of
# which binary was running (the seam commit fixed that), and the engine's
# library-level `DEFAULT_CREATED_BY` fallback still spells it. (`sx` was a third
# token until plan 016 sunset the crate; both legs now stamp `shed-machine-rc`,
# and the mask stays because the goldens are recorded under it.)
_PROG_TOKENS = ("shed-machine-rc", "shed-ext-rc")


def assert_rfc3339(value: Any, field: str) -> None:
    assert isinstance(value, str), f"{field}: expected an RFC3339 string, got {value!r}"
    assert _RFC3339.match(value), f"{field}: not RFC3339-shaped: {value!r}"


def assert_uuid(value: Any, field: str) -> None:
    assert isinstance(value, str), f"{field}: expected a uuid string, got {value!r}"
    assert _UUID.match(value), f"{field}: not uuid-shaped: {value!r}"


def canonical(obj: Any) -> Any:
    """A structurally-canonical copy: object keys recursively sorted, list order
    preserved (D2)."""
    if isinstance(obj, dict):
        return {k: canonical(obj[k]) for k in sorted(obj)}
    if isinstance(obj, list):
        return [canonical(v) for v in obj]
    return obj


def mask_home(text: str, home: str) -> str:
    """Replace the leg's isolated HOME prefix with `<home>` wherever it appears,
    keeping any suffix — so `<home>/work/repo` still diffs its structure.

    The realpath form is substituted too: macOS resolves /var -> /private/var, and
    an engine reads the path verbatim while tmux may report the resolved one.

    LONGEST candidate first, and never over a set: on macOS one form is a strict
    SUFFIX-bearing prefix of the other (`/var/...` vs `/private/var/...`), so
    replacing the short one first leaves `/private<home>` behind while replacing
    the long one first is clean. Iterating a set made that order depend on
    PYTHONHASHSEED, which differs per process — so the two legs of a differential
    could canonicalize the same path differently and diff on the masking rather
    than on the behavior."""
    if not home:
        return text
    for candidate in sorted({home, os.path.realpath(home)}, key=len, reverse=True):
        text = text.replace(candidate, MASK_HOME)
    return text


def mask_session(dto: dict, home: str) -> dict:
    """Mask an `RcSessionDto`'s volatile fields, diffing everything else.

    Masked (volatile, shape-asserted first): `id` (a fresh uuid), `created_at`
    (RFC3339), `workdir` (the leg's HOME), `created_by` (the binary's own name).

    Diffed (stable — the whole point of pinning `--slug`/`--name` on every
    harness create): `slug`, `tmux_session`, `kind`, `state`, `managed`, `lane`,
    `display_name`, `target_label`, `url`, and — load-bearing — the PRESENT KEY
    SET, which is Go's `omitempty` contract."""
    assert isinstance(dto, dict), f"not a session DTO: {dto!r}"
    out = dict(dto)

    assert_uuid(out.get("id"), "session.id")
    out["id"] = MASK_ID

    assert_rfc3339(out.get("created_at"), "session.created_at")
    out["created_at"] = MASK_TS

    workdir = out.get("workdir")
    assert isinstance(workdir, str) and workdir, f"session.workdir missing: {dto!r}"
    out["workdir"] = mask_home(workdir, home)
    assert out["workdir"].startswith(MASK_HOME), (
        f"session.workdir {workdir!r} is not under the leg HOME {home!r}"
    )

    created_by = out.get("created_by")
    assert created_by in _PROG_TOKENS, (
        f"session.created_by {created_by!r} is not a known program token — the "
        "bare prog-name default is the contract (plan 009 §3.2)"
    )
    out["created_by"] = MASK_PROG

    return out


# --- Hub payloads (plan 010) ------------------------------------------------


def mask_hub_health(payload: dict) -> dict:
    """Mask `/v1/health`: `app` is the byte-frozen identity token and is DIFFED;
    `version` and `pid` are legitimately different per leg (the Go binary's
    ldflags version vs the host-agent's crate version; two distinct daemons) and
    are masked AFTER shape asserts, so a hub that reports an empty version or a
    bogus pid fails rather than masks."""
    assert isinstance(payload, dict), f"not a health payload: {payload!r}"
    out = dict(payload)
    assert out.get("app") == "shed-rc-hub", (
        f"health.app is the byte-frozen identity token: {payload!r}"
    )
    version = out.get("version")
    assert isinstance(version, str) and version.strip(), (
        f"health.version must be a non-empty string: {payload!r}"
    )
    out["version"] = MASK_VERSION
    pid = out.get("pid")
    assert isinstance(pid, int) and pid > 0, (
        f"health.pid must be a positive integer: {payload!r}"
    )
    out["pid"] = MASK_PID
    return out


def mask_hub_session(dto: dict, home: str) -> dict:
    """Mask a hub `/v1/sessions` entry: the session DTO masks, plus the activity
    overlay's timestamp.

    Diffed: `activity`, `last_message` and `pending_approvals` — a cell polls
    until the activity has SETTLED, so the overlay is deterministic. Masked:
    `activity_at`, a wall-clock stamp, after its shape assert — and the overlay
    arrives or is absent as a WHOLE, so `activity_at` without `activity` fails
    rather than masks."""
    out = mask_session(dto, home)
    if "activity_at" in out:
        assert_rfc3339(out["activity_at"], "session.activity_at")
        out["activity_at"] = MASK_TS
        assert "activity" in out, (
            f"activity_at without activity — the overlay must drop the whole "
            f"dimension together: {dto!r}"
        )
    return out


def mask_hub_sessions(payload: dict, home: str) -> dict:
    """Mask a `/v1/sessions` envelope, ordering preserved (the hub lists in its
    own order; the cells create sessions with pinned slugs so order is
    deterministic)."""
    assert isinstance(payload, dict), f"not a sessions envelope: {payload!r}"
    out = dict(payload)
    sessions = out.get("sessions")
    assert isinstance(sessions, list), f"sessions missing/not a list: {payload!r}"
    out["sessions"] = [mask_hub_session(s, home) for s in sessions]
    return out


def masked_feed_rows(rows: list) -> list:
    """Mask a /messages feed row list for the hub cells: `seq` and `ts` are
    SHAPE-ASSERTED before masking (the D3 discipline — a mask must never
    invent the key it hides; these cells are the only place a non-empty feed
    row is observed, so this is the wire's only seq/ts shape pin)."""
    out = []
    for row in rows:
        masked = dict(row)
        assert isinstance(masked.get("seq"), int) and masked["seq"] >= 1, (
            f"feed row seq must be a positive integer: {row!r}"
        )
        assert_rfc3339(masked.get("ts"), "message.ts")
        masked["seq"] = MASK_SEQ
        masked["ts"] = MASK_TS
        out.append(masked)
    return out
