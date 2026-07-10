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
MASK_ID = "<id>"

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

    # Assert shape/value BEFORE masking so the mask can't paper over a real
    # regression (a Rust `"pid":"123"` string, a blank version, or a relative /
    # wrong config_path would otherwise compare equal to Go once blanked).
    pid = out.get("pid")
    assert isinstance(pid, int) and not isinstance(pid, bool) and pid > 0, (
        f"pid not a positive int: {pid!r}"
    )
    out["pid"] = MASK_PID

    version = out.get("version")
    assert isinstance(version, str) and version.strip(), (
        f"version not a non-empty string: {version!r}"
    )
    out["version"] = MASK_VERSION

    cp = out.get("config_path")
    assert isinstance(cp, str) and cp, f"config_path missing/empty: {cp!r}"
    if config_path:
        # The daemon must echo back the exact config path it was given (each impl
        # resolves it the same way — Go filepath.Abs, Rust lexical clean).
        assert cp == config_path, f"config_path {cp!r} != expected {config_path!r}"
    else:
        assert os.path.isabs(cp), f"config_path not absolute: {cp!r}"
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


def mask_hello_ack(obj: dict) -> dict:
    """Mask the volatile fields of a desktop `hello_ack` frame, leaving everything
    else to be diffed (D3 normalization for surface A).

    Masked (volatile): `id` (a UUID — v7 in Go, v4 in Rust, so never diffable) and
    `ts` (RFC3339, shape-asserted first). On an **accepted** ack `agent.version` is
    also masked (the build string, shape-asserted **nonempty** first — same discipline
    as `mask_live_status`).

    Diffed (stable): `v`, `type`, `accepted`, `reason`, `agent.approval_method`,
    `namespaces`, `gate_namespaces`, `request_timeout_ms` — and, on a **superseded**
    (`accepted:false`) ack, `agent.version` too: that ack carries the ZERO-value agent
    `{"version":"","approval_method":""}` (Go `desktop_server.go:355` sends a bare
    `helloAckMsg{}`; the Rust `hello_ack` builder emits an empty agent whenever
    `accepted` is false). An empty `version` there is a stable constant, not a volatile
    build string, so it is diffed as-is rather than masked (and masking it would trip
    the nonempty shape-assert).

    Returns a new object; the input is not mutated.
    """
    assert obj.get("type") == "hello_ack", f"not a hello_ack frame: {obj!r}"
    out = dict(obj)

    # id: a nonempty string (UUID). Masked — the two impls use different UUID
    # versions, and it varies per-frame regardless.
    _id = out.get("id")
    assert isinstance(_id, str) and _id, f"hello_ack.id missing/empty: {_id!r}"
    out["id"] = MASK_ID

    # ts: RFC3339 shape asserted BEFORE masking (so the mask can't hide a bad value).
    assert_rfc3339(out.get("ts"), "hello_ack.ts")
    out["ts"] = MASK_TS

    accepted = out.get("accepted")
    assert isinstance(accepted, bool), f"hello_ack.accepted not a bool: {accepted!r}"

    agent = dict(out.get("agent", {}))
    if accepted:
        # An accepted ack carries the live build version — volatile, so mask it
        # (shape-asserted nonempty first). approval_method stays diffed ("shed-desktop").
        version = agent.get("version")
        assert isinstance(version, str) and version.strip(), (
            f"accepted hello_ack agent.version not a nonempty string: {version!r}"
        )
        agent["version"] = MASK_VERSION
    else:
        # A superseded ack's agent is the zero value; leave version ("") to be diffed.
        assert agent.get("version") == "", (
            f"superseded hello_ack agent.version should be zero-value \"\": {agent!r}"
        )
    out["agent"] = agent

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
