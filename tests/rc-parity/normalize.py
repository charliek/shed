"""Canonicalization + volatile-field masking for the RC parity harness.

Two disciplines inherited from `tests/host-agent-diff/normalize.py`:

* **D2 — structural canonical JSON, never raw bytes.** `canonical()` recursively
  sorts object keys; lists stay order-sensitive. This is the comparison model plan
  009 §3.5 pins for DTO stdout: Go's `json.Encoder` HTML-escapes `<`/`>`/`&` and
  appends a newline, serde_json does neither, and no consumer byte-compares
  stdout — so field PRESENCE is contract, byte shape is not. Preseed artifacts,
  which a mixed fleet rewrites IN PLACE, are the exception: `mask_file_bytes`
  compares them as raw bytes, with no canonicalization at all.

* **D3 — determinism over blanking.** Mask as little as possible and SHAPE-ASSERT
  before masking, so a mask can never hide a malformed value. Distinct sentinels
  make a cross-field leak obvious when eyeballing a golden.

The sentinels are exactly the axes on which two correct implementations (or two
runs) must differ: a fresh uuid, a wall-clock stamp, the isolated HOME each leg
runs under, the per-run pytest workdir a preseed records as a `projects` key, the
loopback port opencode was handed, the pid of a resident daemon, and the agent
version a probe read. `<prog>` is the odd one out — the binary's own name, which
is deliberately NOT the same on both sides (`shed-machine-rc` vs `sx`) and which
the plan pins as a masked token rather than a divergence.
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
MASK_PORT = "<port>"
MASK_PROG = "<prog>"
MASK_DETAIL = "<detail>"
MASK_VERSION = "<version>"
MASK_WORKDIR = "<workdir>"

# An agent version as `ParseAgentVersion` returns it for the shims' `--version`
# line. Shape-asserted before masking so an implementation that failed to PARSE
# (and echoed the whole line, or reported "") is a failure, not a mask.
_VERSION = re.compile(r"^\d+\.\d+\.\d+$")

# RFC3339 with second precision. Go stamps `time.Now().UTC().Format(time.RFC3339)`
# and the Rust engine's clock seam formats seconds-precision UTC with a `Z`; an
# offset form is accepted too so the shape check isn't brittle.
_RFC3339 = re.compile(
    r"^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d+)?(?:Z|[+-]\d{2}:\d{2})$"
)

_UUID = re.compile(r"^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$")

# Every program name that can appear in a `created_by` stamp or an error line.
# `shed-ext-rc` is masked as belt: pre-C2 Go emitted it from `warnHook`/`EnsureHub`
# regardless of which binary was running (the seam commit fixed that), and the
# engine's library-level `DEFAULT_CREATED_BY` fallback still spells it.
_PROG_TOKENS = ("shed-machine-rc", "shed-ext-rc", "sx")
# The two-letter "sx" is masked ONLY as a stderr line prefix ("sx: ") — a bare
# \b-bounded "sx" would also hit path components (target/debug/sx) and any
# hyphen-delimited word containing it, and an over-broad mask can hide a real
# divergence (C4 review finding). The longer tokens keep word-boundary breadth
# (they appear mid-sentence in Go's diagnostics).
_PROG_RE = re.compile(
    r"\b(?:shed-machine-rc|shed-ext-rc)\b|(?m:^sx(?=: ))"
)

# A port number in an argv token or an env value (opencode's allocated loopback
# port). Anchored to the two places it appears so a stray number is never masked.
_PORT_VALUE = re.compile(r"^\d{2,5}$")


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


def mask_prog(text: str) -> str:
    """Replace the program-name token (`shed-machine-rc` / `sx` / `shed-ext-rc`)
    with `<prog>`. Word-anchored so a path or a display name containing the letters
    is untouched."""
    return _PROG_RE.sub(MASK_PROG, text)


def _mask_path(text: str, path: str, mask: str) -> str:
    """Replace `path` with `mask` wherever it appears, keeping any suffix — so
    `<home>/.shed-plans/plan-aa1111.md` still diffs its structure.

    The realpath form is substituted too: macOS resolves /var -> /private/var, and
    an engine reads the path verbatim while tmux (or a preseed's own `os.Getwd`)
    may report the resolved one.

    LONGEST candidate first, and never over a set: on macOS one form is a strict
    SUFFIX-bearing prefix of the other (`/var/...` vs `/private/var/...`), so
    replacing the short one first leaves `/private<mask>` behind while replacing
    the long one first is clean. Iterating a set made that order depend on
    PYTHONHASHSEED, which differs per process — so the two legs of a differential
    could canonicalize the same path differently and diff on the masking rather
    than on the behavior."""
    if not path:
        return text
    candidates = sorted({path, os.path.realpath(path)}, key=len, reverse=True)
    for candidate in candidates:
        text = text.replace(candidate, mask)
    return text


def mask_home(text: str, home: str) -> str:
    """Replace the leg's isolated HOME prefix with `<home>`."""
    return _mask_path(text, home, MASK_HOME)


def mask_text(text: str, home: str) -> str:
    """Both textual masks, in the order a stderr line needs them."""
    return mask_prog(mask_home(text, home))


def mask_version(stdout: str) -> str:
    """The `version` verb's line, fully masked after a shape assert.

    The two implementations report genuinely different things (`shed-machine-rc
    dev (commit: ..., built: ...)` vs `sx <crate version>`), so the SHAPE — a
    program name, a space, a non-empty version — is the contract, not the text."""
    line = stdout.rstrip("\n")
    assert "\n" not in line, f"version wrote more than one line: {stdout!r}"
    m = re.match(r"^(\S+) (\S.*)$", line)
    assert m, f"version output is not `<prog> <version>`-shaped: {line!r}"
    prog, version = m.group(1), m.group(2)
    assert prog in _PROG_TOKENS, f"version prog token {prog!r} not a known binary"
    assert version.strip(), f"version is blank: {line!r}"
    return f"{MASK_PROG} <version>"


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


def mask_capabilities(caps: dict) -> dict:
    """Mask a `capabilities` payload: only the agent VERSION strings, and only
    after a shape assert.

    Everything else is diffed: `rc_version`, the ordered `kinds` list, the ordered
    `features` token list, the whole `kind_features` matrix, and — load-bearing —
    each agent's `installed` boolean, which the PATH shims pin deterministically
    (an uninstalled agent is one the test removed from the shim dir).

    The version values are masked because they are the one thing a real
    installation would change under the harness's feet; the shape assert
    (`<major>.<minor>.<patch>`) still proves BOTH implementations parsed the shim's
    `--version` output rather than reporting an empty string."""
    assert isinstance(caps, dict), f"not a capabilities payload: {caps!r}"
    out = dict(caps)
    agents = out.get("agents")
    assert isinstance(agents, dict), f"capabilities.agents missing: {caps!r}"
    masked = {}
    for tool, info in agents.items():
        assert isinstance(info, dict), f"agents[{tool}] is not an object: {info!r}"
        row = dict(info)
        assert isinstance(row.get("installed"), bool), f"agents[{tool}].installed: {info!r}"
        if "version" in row:
            assert row["installed"], f"agents[{tool}] reports a version while not installed"
            assert _VERSION.match(str(row["version"])), (
                f"agents[{tool}].version is not version-shaped: {row['version']!r} — "
                "both implementations must PARSE the probe output, not echo it"
            )
            row["version"] = MASK_VERSION
        masked[tool] = row
    out["agents"] = masked
    return out


def mask_list(envelope: dict, home: str) -> dict:
    """Mask a `list` envelope — the sessions AND the embedded capabilities block.

    Go's `doList` always embeds capabilities (`clirc.go:356`), and since C5 so does
    `sx rc list`, so the FULL envelope is compared here. (C4 stripped the block
    from both sides while the Rust engine had no `capabilities.rs`; that carve-out
    is gone.)"""
    assert isinstance(envelope, dict), f"not a list envelope: {envelope!r}"
    out = dict(envelope)
    sessions = out.get("rc_sessions")
    assert isinstance(sessions, list), f"rc_sessions missing/not a list: {envelope!r}"
    out["rc_sessions"] = [mask_session(s, home) for s in sessions]
    caps = out.get("capabilities")
    assert isinstance(caps, dict), (
        f"the list envelope must embed a capabilities block: {envelope!r}"
    )
    out["capabilities"] = mask_capabilities(caps)
    return out


def mask_file_bytes(raw: bytes, home: str, workdir: str | None = None) -> str:
    """A preseed artifact as a RAW-BYTES surface (plan 009 §3.5).

    The only transformation is the leg's isolated HOME prefix — a genuine per-leg
    difference, exactly like `<home>` everywhere else — and it is applied to the
    decoded text, never to the structure: no JSON parse, no key sorting, no
    whitespace normalization. Two implementations whose writers disagree on key
    order, indentation, HTML escaping or number fidelity fail HERE, which is the
    entire point of the byte-exact writer.

    `workdir` masks the create's `--workdir`, which the claude preseed writes as
    a `projects` KEY. Both legs of a cell pass the same directory (that is what
    makes the two documents byte-identical), so masking it costs the differential
    nothing — but the path is a per-RUN pytest tmp dir, and an unmasked one would
    make every golden here stale on the next run.

    The bytes must be UTF-8 (both writers emit UTF-8 JSON or a POSIX shell
    script); a decode failure is a real finding, not something to paper over."""
    text = _mask_path(raw.decode("utf-8"), workdir or "", MASK_WORKDIR)
    return mask_home(text, home)


def mask_env(dump: dict, home: str) -> dict:
    """Mask a session's `SHED_RC_*` environment (already parsed to a mapping).

    Masked: `SHED_RC_ID`, `SHED_RC_CREATED_AT`, `SHED_RC_CREATED_BY`,
    `SHED_RC_WORKDIR`, `SHED_RC_OPENCODE_PORT`. Diffed: `SHED_RC_V` (the schema
    version — 2), `SHED_RC_KIND`, `SHED_RC_DISPLAY_NAME`, `SHED_RC_SLUG`,
    `SHED_RC_TARGET`, and `OPENCODE_SERVER_PASSWORD` (always set, always empty).

    The mapping is sorted by `canonical()` at compare time on purpose: tmux's
    `show-environment` render ORDER is a tmux-version detail, not our contract
    (`BuildEnvArgs` ordering is pinned by Rust unit tests instead — §3.6)."""
    out = dict(dump)
    if "SHED_RC_ID" in out:
        assert_uuid(out["SHED_RC_ID"], "SHED_RC_ID")
        out["SHED_RC_ID"] = MASK_ID
    if "SHED_RC_CREATED_AT" in out:
        assert_rfc3339(out["SHED_RC_CREATED_AT"], "SHED_RC_CREATED_AT")
        out["SHED_RC_CREATED_AT"] = MASK_TS
    if "SHED_RC_CREATED_BY" in out:
        assert out["SHED_RC_CREATED_BY"] in _PROG_TOKENS, out["SHED_RC_CREATED_BY"]
        out["SHED_RC_CREATED_BY"] = MASK_PROG
    if "SHED_RC_WORKDIR" in out:
        out["SHED_RC_WORKDIR"] = mask_home(out["SHED_RC_WORKDIR"], home)
    if "SHED_RC_OPENCODE_PORT" in out:
        port = out["SHED_RC_OPENCODE_PORT"]
        assert _PORT_VALUE.match(port), f"SHED_RC_OPENCODE_PORT not a port: {port!r}"
        assert int(port) > 1024, f"opencode port not ephemeral: {port!r}"
        out["SHED_RC_OPENCODE_PORT"] = MASK_PORT
    return out


def mask_argv(tokens: list, home: str) -> list:
    """Mask an inner-command argv transcript (recorded by a PATH-shim agent):
    the allocated port becomes `<port>`, the leg HOME becomes `<home>`. Order is
    contract — the argv the agent actually received."""
    out = []
    port_next = False
    for token in tokens:
        if port_next:
            assert _PORT_VALUE.match(token), f"--port value not a port: {token!r}"
            out.append(MASK_PORT)
            port_next = False
            continue
        port_next = token == "--port"
        out.append(mask_home(token, home))
    return out


def mask_stderr(text: str, home: str, *, truncate_after: str | None = None) -> str:
    """Mask a stderr transcript: the prog-name prefix and any HOME path.

    `truncate_after` replaces everything past a marker with `<detail>` — used for
    the ONE class whose tail is a third-party parser's wording (Go's
    `encoding/base64` vs Rust's `base64` crate both report "not valid base64",
    with different detail). The contract there is the class and the message head,
    not the library's phrasing."""
    out = mask_text(text, home).strip()
    if truncate_after is not None:
        idx = out.find(truncate_after)
        assert idx >= 0, f"stderr {out!r} does not contain {truncate_after!r}"
        out = out[: idx + len(truncate_after)] + MASK_DETAIL
    return out


# --- Hub payloads (plan 010) ------------------------------------------------
#
# The hub family compares HTTP payloads from resident daemons rather than CLI
# stdout; the D2/D3 disciplines are identical.


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
    """Mask a hub `/v1/sessions` entry: the one-shot DTO masks, plus the activity
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
