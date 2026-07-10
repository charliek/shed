"""Canonicalization + volatile-field masking for the host-agent differential.

Two disciplines from the harness plan (D2 comparison model, D3 normalization):

* **D2 — structural canonical-JSON, never raw bytes.** `canonical()` parses have
  already happened (callers pass Python objects from `json.loads`); it recursively
  sorts object keys so a Go `map` and a Rust `BTreeMap` (or field-declaration order)
  can never read as a diff. Lists stay order-sensitive.

* **D3 — determinism over blanking.** Mask as *little* as possible: only the fields
  that genuinely vary run-to-run (`pid`, `version`, timestamps, absolute paths). Every
  masked timestamp has its **RFC3339 shape asserted before** it is replaced, so the
  mask can't hide a malformed value. Structure is preserved — e.g. the approval-channel
  socket path keeps its `host-agent.sock` basename; only the volatile dir prefix is
  masked.
"""

from __future__ import annotations

import os
import re
from typing import Any

# A masked value's placeholder. Distinct sentinels make an accidental cross-field
# leak obvious when eyeballing a canonical transcript (the D3.6 manual ritual).
MASK_TS = "<ts>"
MASK_PID = "<pid>"
MASK_VERSION = "<version>"
MASK_CONFIG_PATH = "<config_path>"

# RFC3339 with second precision and either a `Z` or a numeric offset. Both daemons
# emit `...Z` (Go `time.RFC3339` on a UTC time; Rust's std-only formatter), but accept
# an offset too so the shape check isn't brittle.
_RFC3339 = re.compile(
    r"^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d+)?(?:Z|[+-]\d{2}:\d{2})$"
)


def assert_rfc3339(value: Any, field: str) -> None:
    """Assert `value` is an RFC3339 timestamp string (shape only). Raises on a
    non-string or a value that doesn't match, so masking never papers over a
    malformed timestamp."""
    assert isinstance(value, str), f"{field}: expected an RFC3339 string, got {value!r}"
    assert _RFC3339.match(value), f"{field}: not RFC3339-shaped: {value!r}"


def canonical(obj: Any) -> Any:
    """Return a structurally-canonical copy: object keys recursively sorted, list
    order preserved. This is the D2 comparison model — parse (already done by the
    caller) then sort, never a byte compare."""
    if isinstance(obj, dict):
        return {k: canonical(obj[k]) for k in sorted(obj)}
    if isinstance(obj, list):
        return [canonical(v) for v in obj]
    return obj


def mask_live_status(obj: dict, socket_dir: str, config_path: str) -> dict:
    """Mask the volatile fields of a `LiveStatus` JSON object, leaving everything
    else to be diffed.

    Masked (volatile): `version`, `pid`, `started_at`, `written_at`, `config_path`,
    `approval_channel.socket_path` (dir prefix only — the `host-agent.sock` basename
    is kept), and every `servers[].namespaces[].since`.

    Diffed (stable): `schema`, `policies`, `gate_namespaces`,
    `approval_channel.consumer_connected`, and the `servers[]` structure.

    `started_at`/`written_at`/`since` have their RFC3339 shape asserted before the
    mask. Returns a new object; the input is not mutated.
    """
    out = dict(obj)

    out["version"] = MASK_VERSION
    out["pid"] = MASK_PID
    out["config_path"] = MASK_CONFIG_PATH

    for ts_field in ("started_at", "written_at"):
        assert_rfc3339(out.get(ts_field), ts_field)
        out[ts_field] = MASK_TS

    ac = dict(out.get("approval_channel", {}))
    sp = ac.get("socket_path")
    assert isinstance(sp, str), f"approval_channel.socket_path missing: {ac!r}"
    # Structure-preserving mask: keep the fixed public basename, blank only the
    # env-dependent directory (the plan: "mask the dir prefix, KEEP host-agent.sock").
    if socket_dir:
        assert os.path.dirname(sp) == socket_dir, (
            f"approval_channel.socket_path parent {os.path.dirname(sp)!r} "
            f"!= socket_dir {socket_dir!r}"
        )
    ac["socket_path"] = "<dir>/" + os.path.basename(sp)
    out["approval_channel"] = ac

    masked_servers = []
    for sv in out.get("servers", []):
        sv = dict(sv)
        namespaces = []
        for ns in sv.get("namespaces", []):
            ns = dict(ns)
            if "since" in ns:
                assert_rfc3339(ns["since"], "servers[].namespaces[].since")
                ns["since"] = MASK_TS
            namespaces.append(ns)
        sv["namespaces"] = namespaces
        masked_servers.append(sv)
    out["servers"] = masked_servers

    return out


def mask_not_running(stderr: str, socket_dir: str) -> str:
    """Mask the socket directory in the `status`-not-running stderr so the two
    impls' three lines are byte-equal. Only the first line embeds the path
    (`<dir>/host-agent-status.sock`); the dir prefix is replaced with `<DIR>`."""
    return stderr.replace(socket_dir, "<DIR>")


def mask_status_text(text: str, socket_dir: str, config_path: str) -> str:
    """Mask the volatile lines of the human-readable `status` render so the two
    impls' text is equal: the `pid`/`version` header line, the `config:` line, the
    `started:` line, and the approval-channel `socket` line. Everything else (the
    policy table, gate annotations, consumer line, `Servers (N):` block) is diffed.
    """
    lines = text.split("\n")
    masked = []
    for line in lines:
        if line.startswith("shed-host-agent status (pid "):
            masked.append("shed-host-agent status (pid <pid>, <version>)")
        elif line.startswith("config:"):
            masked.append("config:   <config_path>")
        elif line.startswith("started:"):
            masked.append("started:  <ts>")
        elif line.startswith("  socket    "):
            masked.append("  socket    <dir>/" + os.path.basename(line.strip().split()[-1]))
        else:
            masked.append(line)
    return "\n".join(masked)
