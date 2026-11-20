"""Canonicalization + volatile-field masking for the broker wire harness.

Two disciplines from the harness plan (D2 comparison model, D3 normalization), both
of which now serve the recorded goldens:

* **D2 — structural canonical-JSON, never raw bytes.** `canonical()` parses have
  already happened (callers pass Python objects from `json.loads`); it recursively
  sorts object keys so serializer field order can never read as a diff. Lists stay
  order-sensitive.

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
MASK_MACHINE_ID = "<machine_id>"
MASK_MACHINE_NAME = "<machine_name>"

# A machine id (plan 035 §3.4): 32 lowercase hex characters, no hyphens. Always used
# with `fullmatch`: `$` also matches before a final newline, which would pass (and then
# mask away) an id with a trailing `\n`.
_MACHINE_ID = re.compile(r"[0-9a-f]{32}")

# RFC3339 with second precision and either a `Z` or a numeric offset. The broker emits
# `...Z` (its std-only formatter; the retired Go twin's `time.RFC3339` on a UTC time),
# but accept an offset too so the shape check isn't brittle.
_RFC3339 = re.compile(
    r"^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d+)?(?:Z|[+-]\d{2}:\d{2})$"
)


def assert_rfc3339(value: Any, field: str) -> None:
    """Assert `value` is an RFC3339 timestamp string (shape only). Raises on a
    non-string or a value that doesn't match, so masking never papers over a
    malformed timestamp."""
    assert isinstance(value, str), f"{field}: expected an RFC3339 string, got {value!r}"
    assert _RFC3339.match(value), f"{field}: not RFC3339-shaped: {value!r}"


def assert_machine_identity(machine_id: Any, name: Any) -> None:
    """Assert a machine identity's shape before it is masked: the id is 32 lowercase
    hex characters, the name 1 to 63 bytes of UTF-8 with no control character (the
    host's name, derived from its hostname, so it varies by machine and is masked)."""
    assert isinstance(machine_id, str) and _MACHINE_ID.fullmatch(machine_id), (
        f"machine id not 32 lowercase hex characters: {machine_id!r}"
    )
    assert isinstance(name, str), f"machine name not a string: {name!r}"
    assert 1 <= len(name.encode("utf-8")) <= 63, f"machine name not 1-63 bytes: {name!r}"
    assert not any(
        ord(c) < 0x20 or 0x7F <= ord(c) <= 0x9F for c in name
    ), f"machine name holds a control character: {name!r}"


def canonical(obj: Any) -> Any:
    """Return a structurally-canonical copy: object keys recursively sorted, list
    order preserved. This is the D2 comparison model — parse (already done by the
    caller) then sort, never a byte compare."""
    if isinstance(obj, dict):
        return {k: canonical(obj[k]) for k in sorted(obj)}
    if isinstance(obj, list):
        return [canonical(v) for v in obj]
    return obj


def mask_live_status(obj: dict, config_path: str) -> dict:
    """Mask the volatile fields of a `LiveStatus` JSON object, leaving everything
    else to be diffed.

    Masked (volatile): `version`, `pid`, `started_at`, `written_at`, `config_path`,
    every `servers[].namespaces[].since`, and `machine.id` / `machine.name` (a fresh
    random id per run, a name from the host's hostname; both shape-asserted first). The
    presence of the `machine` block, and of `machine_error`, is diffed.

    Diffed (stable): `schema`, `policies`, `gate_namespaces`, and the `servers[]`
    structure.

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
        # The broker must echo back the exact config path it was given (made absolute).
        assert cp == config_path, f"config_path {cp!r} != expected {config_path!r}"
    else:
        assert os.path.isabs(cp), f"config_path not absolute: {cp!r}"
    out["config_path"] = MASK_CONFIG_PATH

    for ts_field in ("started_at", "written_at"):
        assert_rfc3339(out.get(ts_field), ts_field)
        out[ts_field] = MASK_TS

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

    if "machine" in out:
        machine = dict(out["machine"])
        assert_machine_identity(machine.get("id"), machine.get("name"))
        machine["id"] = MASK_MACHINE_ID
        machine["name"] = MASK_MACHINE_NAME
        out["machine"] = machine

    return out


def mask_bus_response(obj: dict) -> dict:
    """Mask the volatile fields of a bus **response** Envelope, leaving everything else
    to be diffed (D3 normalization).

    Masked (volatile): `id` (a fresh UUID per response, `new_response`) and
    `timestamp` (the response mint time, RFC3339 shape-asserted first so the mask
    can't paper over a malformed value).

    Diffed (stable — NOT masked): `in_reply_to` (the correlation id, which MUST echo
    the request's `id`), `namespace`, `type`, `final`, `payload`, and `shed`. The
    caller asserts those carry the expected pong values.

    Returns a new object; the input is not mutated.
    """
    assert obj.get("type") == "response", f"not a response envelope: {obj!r}"
    out = dict(obj)

    # id: a nonempty UUID string. Masked — it varies per-response.
    _id = out.get("id")
    assert isinstance(_id, str) and _id, f"bus response id missing/empty: {_id!r}"
    out["id"] = MASK_ID

    # timestamp: RFC3339 shape asserted BEFORE masking.
    assert_rfc3339(out.get("timestamp"), "bus response timestamp")
    out["timestamp"] = MASK_TS

    return out


def mask_audit_entry(obj: dict) -> dict:
    """Mask the volatile fields of a durable audit JSONL record (channel 2, catalog
    §3.1), leaving everything else to be diffed.

    Masked (volatile): `ts` only (RFC3339 UTC stamped by the sink, shape-asserted
    first). Every other field — `server` (omitempty), `shed`, `ns`, `op`, `result`,
    `detail`, `code`, `reason`, `approval`, `decided_by`, `scope`, `ttl` — is pinned;
    the `WireEntry` `skip_serializing_if` set means the present-key set is itself
    golden-pinned (a missing `detail`/`code` on the deny line, a missing `server` in
    single-server mode). Field *order* in the file is normalized away by `canonical()`.

    Returns a new object; the input is not mutated.
    """
    out = dict(obj)
    assert_rfc3339(out.get("ts"), "audit.ts")
    out["ts"] = MASK_TS
    return out
