"""Approval stimuli through a mock's plugin bus, and what to read back.

The app brokers in process, so a credential request reaches it the real way: a
request envelope pushed on a mock's plugin bus, which the app's broker reads,
gates through the app (a pending approval, or a decision by policy) and answers
with a respond POST. These helpers build valid requests, find the approval a
request made, and read the outcome back from the bus and from the broker's
durable audit file.

* `emit_sign(mock, shed, info)` / `emit_docker(mock, shed)` / `emit_aws(mock, shed)`
  push one request and return its envelope id. The request's server is the
  mock's name in the broker's server file: the broker stamps it, so a request
  emitted on `studio` is a request from `studio`.
* `new_approval(client, shed, before)` waits for the one pending approval for
  `shed` that is not in `before` (a `pending_ids` snapshot taken just before the
  emit) and returns its id. The broker answers one request per namespace and
  server at a time, so a cell emits one request and takes its approval before
  the next.
* `verdict(mock, ns, envelope_id)` is "approve" or "deny", read from the bus
  response; any other answer fails.
* `audit_line(info, ns, shed, n)` is the `n`th line of the broker's audit file
  for that namespace and shed, with `decided_by`, `scope`, `ttl`, `server` and
  `result` (the app's own activity entries carry none of the first three).
"""

from __future__ import annotations

import base64
import json
import struct
import time
import uuid

from client import scaled_timeout

SSH = "ssh-agent"
AWS = "aws-credentials"
DOCKER = "docker-credentials"


def _ssh_string(b: bytes) -> bytes:
    """RFC 4251 `string`: a big-endian uint32 length, then the bytes."""
    return struct.pack(">I", len(b)) + b


def git_login(ssh_blob: str) -> bytes:
    """An SSH `publickey` login request for user `git` with the ed25519 key whose
    wire blob is `ssh_blob`. The broker signs only a login as an allowed username
    (default `[git]`) or a `git` SSHSIG, and refuses anything else before any
    prompt, so this is what reaches the approval gate."""
    return (
        _ssh_string(bytes(range(32)))  # session id
        + bytes([50])  # SSH_MSG_USERAUTH_REQUEST
        + _ssh_string(b"git")
        + _ssh_string(b"ssh-connection")
        + _ssh_string(b"publickey")
        + bytes([1])  # a signature follows
        + _ssh_string(b"ssh-ed25519")
        + _ssh_string(base64.b64decode(ssh_blob))
    )


def _envelope(ns: str, payload: dict, shed: str) -> dict:
    return {
        "id": f"req-{uuid.uuid4().hex[:12]}",
        "namespace": ns,
        "type": "request",
        "final": True,
        "timestamp": "2026-07-10T00:00:00Z",
        "payload": payload,
        # The broker ignores the request's own `server` (it stamps the name its
        # server file gives the mock), but the envelope must carry the field.
        "shed": {"name": shed, "backend": "vz", "server": ""},
    }


def _push(mock, ns: str, env: dict) -> str:
    mock.wait_for_subscribe(ns, timeout=scaled_timeout(10))
    mock.push_request(ns, env)
    return env["id"]


def emit_sign(mock, shed: str, info) -> str:
    """Push a `sign` request (a `git` login over the broker's generated key)."""
    payload = {
        "operation": "sign",
        "public_key": info.ssh_blob,
        "data": base64.b64encode(git_login(info.ssh_blob)).decode(),
        "flags": 0,
    }
    return _push(mock, SSH, _envelope(SSH, payload, shed))


def emit_docker(mock, shed: str, registry: str = "ghcr.io") -> str:
    """Push a docker `get` for `registry`."""
    return _push(mock, DOCKER, _envelope(DOCKER, {"operation": "get", "server_url": registry}, shed))


def emit_aws(mock, shed: str) -> str:
    """Push an AWS `get_credentials`."""
    return _push(mock, AWS, _envelope(AWS, {"operation": "get_credentials"}, shed))


def pending_ids(client) -> set[str]:
    return {a["id"] for a in client.approvals_list()}


def new_approval(client, shed: str, before: set[str], timeout: float = 20.0) -> str:
    """The id of the one pending approval for `shed` that is not in `before`."""
    found: list[str] = []

    def one() -> bool:
        found[:] = [a["id"] for a in client.approvals_list()
                    if a.get("shed") == shed and a["id"] not in before]
        return len(found) == 1

    client.wait_until(one, timeout=timeout, what=f"one new pending approval for {shed}")
    return found[0]


def verdict(mock, ns: str, envelope_id: str, timeout: float = 20.0) -> str:
    """'approve' (a signature or credentials came back) or 'deny' (the error
    `approval denied`) for request `envelope_id`; any other answer fails."""
    resp = mock.await_reply(ns, envelope_id, timeout=scaled_timeout(timeout))
    payload = resp.get("payload") or {}
    if payload.get("error") == "approval denied":
        return "deny"
    if "error" in payload:
        raise AssertionError(f"{ns} request {envelope_id} failed: {payload}")
    ok = {SSH: "blob", AWS: "access_key_id", DOCKER: "secret"}[ns]
    if not payload.get(ok):
        raise AssertionError(f"{ns} request {envelope_id}: no {ok} in {payload}")
    return "approve"


def audit_lines(info, ns: str, shed: str) -> list[dict]:
    path = info.audit_path
    if path is None or not path.exists():
        return []
    out = []
    for raw in path.read_text().splitlines():
        if not raw.strip():
            continue
        try:
            e = json.loads(raw)
        except ValueError:
            continue
        if e.get("ns") == ns and e.get("shed") == shed:
            out.append(e)
    return out


def audit_line(info, ns: str, shed: str, n: int = 1, timeout: float = 15.0) -> dict:
    """The `n`th (1-based) broker audit line for `ns` and `shed`, waiting for it."""
    deadline = time.monotonic() + scaled_timeout(timeout)
    while True:
        lines = audit_lines(info, ns, shed)
        if len(lines) >= n:
            return lines[n - 1]
        if time.monotonic() >= deadline:
            raise AssertionError(
                f"no audit line #{n} for {ns}/{shed} in {info.audit_path}: {lines}")
        time.sleep(0.1)
