"""The AWS credential backend differential — surface-B `get_credentials`/`status`/
`ping`/unknown over the aws-credentials namespace, in **passthrough** mode, asserted
equal across the Go `cmd/shed-host-agent` and the Rust `crates/shed-host-agent`.

**Passthrough only, by design.** The hand-rolled INI reader + expiry-hint scan is the
only differentially-tested path (both impls read the same `<HOME>/.aws/credentials`
profile with no SDK involved). The **assume-role** path is owned by unit tests (the
`AssumeRoler` fake) + the `aws_expiry`/`aws_resolve` goldens — there is no Go STS seam
to drive live, so it is reclassified out-of-scope for the live diff (README table).

**Hermetic by construction.** The `daemon` fixture writes the fixture profile into each
impl's isolated `<HOME>/.aws/credentials` (+ an empty `<HOME>/.aws/config`), so no real
`~/.aws` is read and the two impls resolve the SAME profile off `$HOME`. The env-var
resolution route (`AWS_SHARED_CREDENTIALS_FILE`) is covered by Rust unit tests instead.

**Subscription-set.** Each scenario `wait_for_subscribe("aws-credentials")` before
pushing, so BOTH impls are proven to subscribe the aws-credentials namespace when the
AWS backend is configured (the Rust side now wires it; docker + egress asymmetry
remains — see the README).
"""

from __future__ import annotations

import json
import os
from pathlib import Path

import pytest

from normalize import canonical, mask_audit_entry, mask_bus_response
from synthetic_bus import SyntheticBus

# The fixed passthrough profile the fixtures carry (throwaway, non-secret constants).
PROFILE = "shed-test"
AWS_KEY = "ASIATESTKEY00000001"
AWS_SECRET = "testSecretAccessKey00000001"
AWS_TOKEN = "testSessionTokenABCDEF00000001"
# The re-login rewrite carries a distinct key so the pickup is observable.
AWS_KEY_2 = "ASIATESTKEY00000002"
AWS_SECRET_2 = "testSecretAccessKey00000002"
AWS_TOKEN_2 = "testSessionTokenABCDEF00000002"

# A fixed expiry hint → a deterministic wire `expiration` + `cached_until` on both impls.
EXPIRY = "2030-01-01T00:00:00Z"

# --- fixture profiles (INI text written to <HOME>/.aws/credentials) -------------------

CREDS_WITH_EXPIRY = (
    f"[{PROFILE}]\n"
    f"aws_access_key_id = {AWS_KEY}\n"
    f"aws_secret_access_key = {AWS_SECRET}\n"
    f"aws_session_token = {AWS_TOKEN}\n"
    f"aws_session_expiration = {EXPIRY}\n"
)

CREDS_NO_EXPIRY = (
    f"[{PROFILE}]\n"
    f"aws_access_key_id = {AWS_KEY}\n"
    f"aws_secret_access_key = {AWS_SECRET}\n"
    f"aws_session_token = {AWS_TOKEN}\n"
)

# A profile present but with NO static credentials (region only) → the exact
# no-static-credentials error, which embeds the shared-credentials PATH. Chosen for the
# error cell because it (a) is an EXACT Go string ported byte-for-byte to Rust and (b)
# carries the per-impl `<HOME>/.aws/credentials` path, so it genuinely exercises the
# home-normalization the plan requires (the no-session-token branch carries no path).
CREDS_NO_STATIC = f"[{PROFILE}]\nregion = us-east-1\n"

CREDS_RELOGIN = (
    f"[{PROFILE}]\n"
    f"aws_access_key_id = {AWS_KEY_2}\n"
    f"aws_secret_access_key = {AWS_SECRET_2}\n"
    f"aws_session_token = {AWS_TOKEN_2}\n"
    f"aws_session_expiration = {EXPIRY}\n"
)

# --- config (block style; `{server}` filled, `{audit_log}` survives to the fixture) ---

AWS_CONFIG = """\
server: {server}
ssh:
  approval:
    policy: approve-all
aws:
  source_profile: shed-test
  mode: passthrough
  approval:
    policy: approve-all
logging:
  enabled: true
  path: {audit_log}
"""


def _aws_config(bus_url: str) -> str:
    return AWS_CONFIG.replace("{server}", bus_url)


# The `shed` block every request carries; echoed onto the response.
SHED = {"name": "web", "backend": "vz", "server": "mini2"}
EXPECTED_SHED = dict(SHED)


def _req(req_id: str, operation: str) -> dict:
    return {
        "id": req_id,
        "namespace": "aws-credentials",
        "type": "request",
        "final": True,
        "timestamp": "2026-07-10T00:00:00Z",
        "payload": {"operation": operation},
        "shed": SHED,
    }


# --- D3.6 introspection: RAW captured pairs, saved ONLY when a reviewer opts in -------
#
# During per-slice development the orchestrator manually diffs the RAW (unmasked)
# Go/Rust wire pairs; setting SHED_HOST_AGENT_DIFF_INTROSPECTION to a writable file
# path re-enables that capture. It MUST default off: an earlier revision hard-coded
# a developer-machine absolute path here, which made these cells unrunnable on any
# other host (caught by the first Linux CI execution of this suite).

_introspection: dict = {}


def _record_introspection(section: str, data: dict) -> None:
    """Merge a RAW captured pair (response envelope + audit) into the opt-in
    introspection file and rewrite it (tests run serially, so accumulation is
    safe). No-op unless SHED_HOST_AGENT_DIFF_INTROSPECTION names a target file."""
    target = os.environ.get("SHED_HOST_AGENT_DIFF_INTROSPECTION")
    if not target:
        return
    _introspection[section] = data
    Path(target).write_text(json.dumps(_introspection, indent=2, sort_keys=True))


# =====================================================================================
# passthrough cells
# =====================================================================================


@pytest.mark.differential
def test_aws_passthrough_get_credentials(daemon, differential):
    captured: dict = {}

    def scenario(impl):
        with SyntheticBus() as bus:
            with daemon(impl, _aws_config(bus.url), install_aws_credentials=CREDS_WITH_EXPIRY) as d:
                bus.wait_for_subscribe("aws-credentials", timeout=10.0)
                bus.push_request("aws-credentials", _req("cred-1", "get_credentials"))
                resp = bus.await_response("aws-credentials", timeout=10.0)
                audit = d.read_audit_jsonl(expect=1, timeout=10.0)[0]
                captured[impl] = {"response": resp, "audit": dict(audit)}
                return {
                    "response": canonical(mask_bus_response(resp)),
                    "audit": canonical(mask_audit_entry(audit)),
                }

    result = differential(scenario)
    resp = result["response"]
    assert resp["in_reply_to"] == "cred-1"
    assert resp["shed"] == EXPECTED_SHED
    # The full response payload is deterministic → diffed byte-for-byte incl. expiration.
    assert resp["payload"] == {
        "access_key_id": AWS_KEY,
        "secret_access_key": AWS_SECRET,
        "session_token": AWS_TOKEN,
        "expiration": EXPIRY,
    }
    # The gated get_credentials audit (LogEntry form): approve-all → no decided_by/scope/
    # ttl; detail = awsExpiryDetail (HH:MM UTC); NO code (asserted absent below).
    assert result["audit"] == {
        "ts": "<ts>",
        "shed": "web",
        "ns": "aws-credentials",
        "op": "get_credentials",
        "result": "ok",
        "detail": "expires:00:00",
        "approval": "approve-all",
    }
    assert "code" not in result["audit"], "get_credentials audit must carry NO code"
    _record_introspection("get_credentials", captured["rust"])


@pytest.mark.differential
def test_aws_passthrough_no_expiry_hint(daemon, differential):
    def scenario(impl):
        with SyntheticBus() as bus:
            with daemon(impl, _aws_config(bus.url), install_aws_credentials=CREDS_NO_EXPIRY) as d:
                bus.wait_for_subscribe("aws-credentials", timeout=10.0)
                bus.push_request("aws-credentials", _req("cred-2", "get_credentials"))
                resp = bus.await_response("aws-credentials", timeout=10.0)
                audit = d.read_audit_jsonl(expect=1, timeout=10.0)[0]
                return {
                    "response": canonical(mask_bus_response(resp)),
                    "audit": canonical(mask_audit_entry(audit)),
                }

    result = differential(scenario)
    # No expiry hint → the expiration key is ABSENT on the wire (Go omitempty == Rust
    # skip_serializing_if), and the audit detail is expires:none.
    assert "expiration" not in result["response"]["payload"], (
        f"expiration must be absent without a hint: {result['response']['payload']}"
    )
    assert result["response"]["payload"] == {
        "access_key_id": AWS_KEY,
        "secret_access_key": AWS_SECRET,
        "session_token": AWS_TOKEN,
    }
    assert result["audit"]["result"] == "ok"
    assert result["audit"]["detail"] == "expires:none"


@pytest.mark.differential
def test_aws_passthrough_error(daemon, differential):
    captured: dict = {}

    def scenario(impl):
        with SyntheticBus() as bus:
            with daemon(impl, _aws_config(bus.url), install_aws_credentials=CREDS_NO_STATIC) as d:
                bus.wait_for_subscribe("aws-credentials", timeout=10.0)
                bus.push_request("aws-credentials", _req("err-1", "get_credentials"))
                resp = bus.await_response("aws-credentials", timeout=10.0)
                audit = d.read_audit_jsonl(expect=1, timeout=10.0)[0]
                captured[impl] = {"response": resp, "audit": dict(audit)}
                # Home-normalize the per-impl `<HOME>/.aws/credentials` path embedded in
                # the error detail before diffing (each impl has its own isolated $HOME;
                # follow the minter argv home-normalize precedent).
                norm = dict(audit)
                norm["detail"] = norm["detail"].replace(d.home, "<HOME>")
                return {
                    "response": canonical(mask_bus_response(resp)),
                    "audit": canonical(mask_audit_entry(norm)),
                }

    result = differential(scenario)
    resp = result["response"]
    assert resp["in_reply_to"] == "err-1"
    # A backend error maps to the generic guest-facing payload (ASSUME_ROLE_FAILED is
    # scoped to get_credentials failures — gate-deny + backend-error).
    assert resp["payload"] == {"error": "credential request failed", "code": "ASSUME_ROLE_FAILED"}
    audit = result["audit"]
    assert audit["result"] == "error"
    assert audit["ns"] == "aws-credentials"
    assert audit["op"] == "get_credentials"
    assert audit["approval"] == "approve-all"
    assert "code" not in audit, "error audit sets NO code"
    # The detail is the EXACT Go no-static-credentials string, home-normalized.
    assert audit["detail"] == (
        'passthrough: profile "shed-test" in <HOME>/.aws/credentials has no static '
        "credentials; run your SSO/SAML login (e.g. `aws sso login`) to refresh"
    )
    _record_introspection("error", captured["rust"])


@pytest.mark.differential
def test_aws_status(daemon, differential):
    captured: dict = {}

    def scenario(impl):
        with SyntheticBus() as bus:
            with daemon(impl, _aws_config(bus.url), install_aws_credentials=CREDS_WITH_EXPIRY) as d:
                bus.wait_for_subscribe("aws-credentials", timeout=10.0)
                bus.push_request("aws-credentials", _req("st-1", "status"))
                resp = bus.await_response("aws-credentials", timeout=10.0)
                captured[impl] = {"response": resp}
                return canonical(mask_bus_response(resp))

    result = differential(scenario)
    assert result["in_reply_to"] == "st-1"
    # role = passthrough:<source_profile>; cached_until = the scanned expiry hint (UTC).
    assert result["payload"] == {
        "connected": True,
        "role": f"passthrough:{PROFILE}",
        "cached_until": EXPIRY,
    }
    _record_introspection("status", captured["rust"])


@pytest.mark.differential
def test_aws_ping(daemon, differential):
    def scenario(impl):
        with SyntheticBus() as bus:
            with daemon(impl, _aws_config(bus.url), install_aws_credentials=CREDS_WITH_EXPIRY) as d:
                bus.wait_for_subscribe("aws-credentials", timeout=10.0)
                bus.push_request("aws-credentials", _req("ping-1", "ping"))
                resp = bus.await_response("aws-credentials", timeout=10.0)
                return canonical(mask_bus_response(resp))

    result = differential(scenario)
    assert result["in_reply_to"] == "ping-1"
    assert result["payload"] == {"status": "ok"}


@pytest.mark.differential
def test_aws_unknown_op(daemon, differential):
    def scenario(impl):
        with SyntheticBus() as bus:
            with daemon(impl, _aws_config(bus.url), install_aws_credentials=CREDS_WITH_EXPIRY) as d:
                bus.wait_for_subscribe("aws-credentials", timeout=10.0)
                bus.push_request("aws-credentials", _req("unk-1", "delete"))
                resp = bus.await_response("aws-credentials", timeout=10.0)
                return canonical(mask_bus_response(resp))

    result = differential(scenario)
    assert result["in_reply_to"] == "unk-1"
    # Go's exact `unknown operation: <op>` INTERNAL_ERROR (AWSCodeInternal).
    assert result["payload"] == {"error": "unknown operation: delete", "code": "INTERNAL_ERROR"}


@pytest.mark.differential
def test_aws_relogin_pickup(daemon, differential):
    """A fresh `aws sso login` (an atomic tmp+rename rewrite of the credentials file
    between two get_credentials) is picked up immediately: passthrough re-reads the file
    every call (no cache), so the second vend reflects the new keys on BOTH impls."""

    def scenario(impl):
        with SyntheticBus() as bus:
            with daemon(impl, _aws_config(bus.url), install_aws_credentials=CREDS_WITH_EXPIRY) as d:
                bus.wait_for_subscribe("aws-credentials", timeout=10.0)
                bus.push_request("aws-credentials", _req("relogin-1", "get_credentials"))
                first = bus.await_response_at("aws-credentials", 0, timeout=10.0)

                # Simulate `aws sso login` rewriting the file ATOMICALLY (tmp + os.replace).
                creds_path = Path(d.home) / ".aws" / "credentials"
                tmp = creds_path.with_name("credentials.tmp")
                tmp.write_text(CREDS_RELOGIN)
                os.replace(tmp, creds_path)

                bus.push_request("aws-credentials", _req("relogin-2", "get_credentials"))
                second = bus.await_response_at("aws-credentials", 1, timeout=10.0)
                return {
                    "first": mask_bus_response(first)["payload"]["access_key_id"],
                    "second": mask_bus_response(second)["payload"]["access_key_id"],
                }

    result = differential(scenario)
    assert result == {"first": AWS_KEY, "second": AWS_KEY_2}
