"""The Docker credential backend — surface-B `get`/`list`/`status`/`ping`/`unknown`
over the docker-credentials namespace, golden-pinned.

**Hermetic by construction.** The `daemon` fixture writes the Docker `config.json`
fixture into the daemon's isolated `<HOME>/.docker/config.json` (`install_docker_config`
kwarg), and `_clean_env` strips `DOCKER_CONFIG`, so the SAME config resolves off
`$HOME` with no real `~/.docker` read. The credential-helper exec seam is a fake
`docker-credential-testhelper` (a python3 script, 0755 + shebang) installed into a
PATH-prepended `helper-bin` dir (`docker_helper_bundle` kwarg); it appends a JSONL
transcript of `{argv, stdin}` — argv+stdin ONLY, never PATH/env — and prints a fixed
bundle, so the exec seam is deterministic and its transcript is pinnable.

**The unconfigured crux.** Unlike AWS (which gates itself off when unconfigured), the
Docker backend is non-nil even absent — so `docker-credentials` is subscribed for every
server. Every cell `wait_for_subscribe("docker-credentials")` first (proving the
subscription); the dedicated `test_docker_unconfigured` cell proves the crux directly
(a minimal `docker:` block still subscribes + denies).

**Error-string policy.** Only PREFIXES + codes are pinned live — the base64/JSON/
exit-status runtime SUFFIXES are environment-dependent and excluded (helper-failure
`reason` is unit-owned). The audit `reason`s asserted here (`registry "x" not in
allowlist`, `no credentials found for "x"`, `denied: approval policy is deny-all`) carry
NO runtime suffix, so they ARE pinned byte-for-byte.
"""

from __future__ import annotations

import base64
import json
import subprocess
import sys

import pytest

from normalize import canonical, mask_audit_entry, mask_bus_response
from synthetic_bus import SyntheticBus

NS = "docker-credentials"

# The fixed helper bundle the fake `docker-credential-testhelper` prints (Capitalized
# docker-credential-helper protocol tags → mapped to the snake_case guest wire).
HELPER_BUNDLE = json.dumps(
    {"ServerURL": "ghcr.io", "Username": "helper-user", "Secret": "helper-secret"}
)


def _b64(plain: str) -> str:
    return base64.b64encode(plain.encode()).decode()


# --- config helpers (block-style; `{server}` filled, `{audit_log}` survives) ----------

_HEAD = "server: {server}\nssh:\n  approval:\n    policy: approve-all\n"
_TAIL = "logging:\n  enabled: true\n  path: {audit_log}\n"


def _config(bus_url: str, docker_block: str) -> str:
    """Assemble a launch config: ssh approve-all + the given `docker:` block + logging.
    `{server}` is filled now (str.replace), `{audit_log}` survives to the `daemon`
    fixture's `.format()`."""
    return (_HEAD + docker_block + _TAIL).replace("{server}", bus_url)


# The `shed` block every request carries; echoed onto the response.
SHED = {"name": "web", "backend": "vz", "server": "mini2"}
EXPECTED_SHED = dict(SHED)


def _req(req_id: str, operation: str, **extra) -> dict:
    payload = {"operation": operation}
    payload.update(extra)
    return {
        "id": req_id,
        "namespace": NS,
        "type": "request",
        "final": True,
        "timestamp": "2026-07-10T00:00:00Z",
        "payload": payload,
        "shed": SHED,
    }


# =====================================================================================
# fake-seam self-test (proves the shim honors the contract before it's trusted)
# =====================================================================================


def test_docker_helper_shim_contract(tmp_path):
    """Drive the generated fake helper directly (no daemon): it must capture argv+stdin
    to the transcript and print the bundle verbatim — the contract the cells rely
    on. Uses the conftest writer via a throwaway helper file."""
    from conftest import _write_docker_helper

    transcript = tmp_path / "t.jsonl"
    helper = tmp_path / "docker-credential-testhelper"
    _write_docker_helper(helper, transcript, HELPER_BUNDLE)
    proc = subprocess.run(
        [sys.executable, str(helper), "get"],
        input="ghcr.io",
        capture_output=True,
        text=True,
        timeout=10,
    )
    assert proc.returncode == 0
    assert proc.stdout == HELPER_BUNDLE
    lines = [ln for ln in transcript.read_text().splitlines() if ln.strip()]
    assert len(lines) == 1
    rec = json.loads(lines[0])
    assert rec == {"argv": ["get"], "stdin": "ghcr.io"}
    # The transcript must NOT leak PATH/env.
    assert "PATH" not in transcript.read_text()


# =====================================================================================
# cells
# =====================================================================================


@pytest.mark.differential
def test_docker_get_inline_auth(daemon, differential):
    """get via inline `auths` (base64 in config.json → fully deterministic, no helper):
    payload + ok audit diffed."""
    config_json = json.dumps({"auths": {"ghcr.io": {"auth": _b64("inline-user:inline-secret")}}})
    docker_block = "docker:\n  allow_all: true\n  approval:\n    policy: approve-all\n"

    def scenario(impl):
        with SyntheticBus() as bus:
            with daemon(impl, _config(bus.url, docker_block), install_docker_config=config_json) as d:
                bus.wait_for_subscribe(NS, timeout=10.0)
                bus.push_request(NS, _req("inl-1", "get", server_url="ghcr.io"))
                resp = bus.await_response(NS, timeout=10.0)
                audit = d.read_audit_jsonl(expect=1, timeout=10.0)[0]
                return {
                    "response": canonical(mask_bus_response(resp)),
                    "audit": canonical(mask_audit_entry(audit)),
                }

    result = differential(scenario)
    resp = result["response"]
    assert resp["in_reply_to"] == "inl-1"
    assert resp["shed"] == EXPECTED_SHED
    assert resp["payload"] == {
        "server_url": "ghcr.io",
        "username": "inline-user",
        "secret": "inline-secret",
    }
    # ok audit (LogEntry form): detail=server_url, NO code (asserted absent).
    assert result["audit"] == {
        "ts": "<ts>",
        "shed": "web",
        "ns": NS,
        "op": "get",
        "result": "ok",
        "detail": "ghcr.io",
        "approval": "approve-all",
    }
    assert "code" not in result["audit"], "ok get audit must carry NO code"


@pytest.mark.differential
def test_docker_get_helper(daemon, differential):
    """get via a credHelper (the live exec seam): the helper transcript golden-pinned,
    the response payload diffed, and the ok audit diffed."""
    config_json = json.dumps({"credHelpers": {"ghcr.io": "testhelper"}})
    docker_block = "docker:\n  allow_all: true\n  approval:\n    policy: approve-all\n"

    def scenario(impl):
        with SyntheticBus() as bus:
            with daemon(
                impl,
                _config(bus.url, docker_block),
                install_docker_config=config_json,
                docker_helper_bundle=HELPER_BUNDLE,
            ) as d:
                bus.wait_for_subscribe(NS, timeout=10.0)
                bus.push_request(NS, _req("hlp-1", "get", server_url="ghcr.io"))
                resp = bus.await_response(NS, timeout=10.0)
                audit = d.read_audit_jsonl(expect=1, timeout=10.0)[0]
                transcript = d.read_docker_transcript(expect=1, timeout=10.0)
                return {
                    "response": canonical(mask_bus_response(resp)),
                    "audit": canonical(mask_audit_entry(audit)),
                    "transcript": transcript,
                }

    result = differential(scenario)
    # The helper's bundle → the snake_case guest wire.
    assert result["response"]["payload"] == {
        "server_url": "ghcr.io",
        "username": "helper-user",
        "secret": "helper-secret",
    }
    # The exec seam: argv `["get"]`, stdin = the raw server_url.
    assert result["transcript"] == [{"argv": ["get"], "stdin": "ghcr.io"}]
    assert result["audit"]["result"] == "ok"
    assert result["audit"]["detail"] == "ghcr.io"


@pytest.mark.differential
def test_docker_not_allowed(daemon, differential):
    """A registry outside the allowlist → guest `REGISTRY_NOT_ALLOWED` (a BACKEND deny,
    distinct from the approval deny below) + audit result:error. The allowlist is checked
    BEFORE config.json is read, so a blank config still denies."""
    config_json = "{}"
    docker_block = "docker:\n  registries: [allowed.io]\n  approval:\n    policy: approve-all\n"

    def scenario(impl):
        with SyntheticBus() as bus:
            with daemon(impl, _config(bus.url, docker_block), install_docker_config=config_json) as d:
                bus.wait_for_subscribe(NS, timeout=10.0)
                bus.push_request(NS, _req("na-1", "get", server_url="blocked.io"))
                resp = bus.await_response(NS, timeout=10.0)
                audit = d.read_audit_jsonl(expect=1, timeout=10.0)[0]
                return {
                    "response": canonical(mask_bus_response(resp)),
                    "audit": canonical(mask_audit_entry(audit)),
                }

    result = differential(scenario)
    assert result["response"]["payload"] == {
        "error": "credential request failed",
        "code": "REGISTRY_NOT_ALLOWED",
    }
    audit = result["audit"]
    assert audit["result"] == "error"
    assert audit["code"] == "REGISTRY_NOT_ALLOWED"
    assert audit["detail"] == "blocked.io"
    # The backend reason carries no runtime suffix (Go `%q` == Rust `{:?}`), so it's diffed.
    assert audit["reason"] == 'registry "blocked.io" not in allowlist'


@pytest.mark.differential
def test_docker_approval_deny(daemon, differential):
    """The approval gate (docker.approval.policy: deny-all) rejects: the guest still gets
    `REGISTRY_NOT_ALLOWED` (back-compat) while the audit carries `code:APPROVAL_DENIED,
    result:denied` — the two-code disambiguation."""
    config_json = json.dumps({"auths": {"ghcr.io": {"auth": _b64("u:s")}}})
    docker_block = "docker:\n  allow_all: true\n  approval:\n    policy: deny-all\n"

    def scenario(impl):
        with SyntheticBus() as bus:
            with daemon(impl, _config(bus.url, docker_block), install_docker_config=config_json) as d:
                bus.wait_for_subscribe(NS, timeout=10.0)
                bus.push_request(NS, _req("deny-1", "get", server_url="ghcr.io"))
                resp = bus.await_response(NS, timeout=10.0)
                audit = d.read_audit_jsonl(expect=1, timeout=10.0)[0]
                return {
                    "response": canonical(mask_bus_response(resp)),
                    "audit": canonical(mask_audit_entry(audit)),
                }

    result = differential(scenario)
    # Guest: REGISTRY_NOT_ALLOWED (unchanged) — NOT the audit's APPROVAL_DENIED.
    assert result["response"]["payload"] == {
        "error": "approval denied",
        "code": "REGISTRY_NOT_ALLOWED",
    }
    audit = result["audit"]
    assert audit["result"] == "denied"
    assert audit["code"] == "APPROVAL_DENIED"
    assert audit["detail"] == "ghcr.io"
    assert audit["approval"] == "deny-all"
    # The deny reason is the gate's error string (Go `err.Error()` == Rust outcome.reason).
    assert audit["reason"] == "denied: approval policy is deny-all"


@pytest.mark.differential
def test_docker_not_found_anonymous(daemon, differential):
    """An ALLOWED registry with no credential → guest `CREDENTIALS_NOT_FOUND` but audit
    result:`anonymous` (the guest pulls anonymously). Assert BOTH the guest code AND the
    anonymous audit — the load-bearing subtlety."""
    config_json = "{}"  # allowed (allow_all) but empty → nothing to serve
    docker_block = "docker:\n  allow_all: true\n  approval:\n    policy: approve-all\n"

    def scenario(impl):
        with SyntheticBus() as bus:
            with daemon(impl, _config(bus.url, docker_block), install_docker_config=config_json) as d:
                bus.wait_for_subscribe(NS, timeout=10.0)
                bus.push_request(NS, _req("nf-1", "get", server_url="ghcr.io"))
                resp = bus.await_response(NS, timeout=10.0)
                audit = d.read_audit_jsonl(expect=1, timeout=10.0)[0]
                return {
                    "response": canonical(mask_bus_response(resp)),
                    "audit": canonical(mask_audit_entry(audit)),
                }

    result = differential(scenario)
    # Guest receives CREDENTIALS_NOT_FOUND (it turns this into an anonymous pull).
    assert result["response"]["payload"] == {
        "error": "credential request failed",
        "code": "CREDENTIALS_NOT_FOUND",
    }
    audit = result["audit"]
    assert audit["result"] == "anonymous", "an allowed-but-no-cred get audits as anonymous"
    assert audit["code"] == "CREDENTIALS_NOT_FOUND"
    assert audit["reason"] == 'no credentials found for "ghcr.io"'


@pytest.mark.differential
def test_docker_list(daemon, differential):
    """list → the allowed registry→username map + a positional audit (`count:N`,
    `approval:none`)."""
    config_json = json.dumps(
        {"credHelpers": {"gcr.io": "gcloud"}, "auths": {"ghcr.io": {"auth": _b64("user:token")}}}
    )
    docker_block = "docker:\n  allow_all: true\n  approval:\n    policy: approve-all\n"

    def scenario(impl):
        with SyntheticBus() as bus:
            with daemon(impl, _config(bus.url, docker_block), install_docker_config=config_json) as d:
                bus.wait_for_subscribe(NS, timeout=10.0)
                bus.push_request(NS, _req("lst-1", "list"))
                resp = bus.await_response(NS, timeout=10.0)
                audit = d.read_audit_jsonl(expect=1, timeout=10.0)[0]
                return {
                    "response": canonical(mask_bus_response(resp)),
                    "audit": canonical(mask_audit_entry(audit)),
                }

    result = differential(scenario)
    assert result["response"]["payload"] == {
        "registries": {"gcr.io": "(credential helper)", "ghcr.io": "user"}
    }
    # Positional audit: op=list, ok, detail=count:2, approval=none, NO outcome/code.
    assert result["audit"] == {
        "ts": "<ts>",
        "shed": "web",
        "ns": NS,
        "op": "list",
        "result": "ok",
        "detail": "count:2",
        "approval": "none",
    }


@pytest.mark.differential
def test_docker_status(daemon, differential):
    """status → `{connected:true, allow_all, registry_count}` for the resolved policy."""
    docker_block = "docker:\n  registries: [a.io, b.io]\n  approval:\n    policy: approve-all\n"

    def scenario(impl):
        with SyntheticBus() as bus:
            with daemon(impl, _config(bus.url, docker_block), install_docker_config="{}") as d:
                bus.wait_for_subscribe(NS, timeout=10.0)
                bus.push_request(NS, _req("st-1", "status"))
                resp = bus.await_response(NS, timeout=10.0)
                return canonical(mask_bus_response(resp))

    result = differential(scenario)
    assert result["in_reply_to"] == "st-1"
    assert result["payload"] == {"connected": True, "allow_all": False, "registry_count": 2}


@pytest.mark.differential
def test_docker_ping(daemon, differential):
    docker_block = "docker:\n  allow_all: true\n  approval:\n    policy: approve-all\n"

    def scenario(impl):
        with SyntheticBus() as bus:
            with daemon(impl, _config(bus.url, docker_block), install_docker_config="{}") as d:
                bus.wait_for_subscribe(NS, timeout=10.0)
                bus.push_request(NS, _req("png-1", "ping"))
                resp = bus.await_response(NS, timeout=10.0)
                return canonical(mask_bus_response(resp))

    result = differential(scenario)
    assert result["in_reply_to"] == "png-1"
    assert result["payload"] == {"status": "ok"}


@pytest.mark.differential
def test_docker_unknown_op(daemon, differential):
    docker_block = "docker:\n  allow_all: true\n  approval:\n    policy: approve-all\n"

    def scenario(impl):
        with SyntheticBus() as bus:
            with daemon(impl, _config(bus.url, docker_block), install_docker_config="{}") as d:
                bus.wait_for_subscribe(NS, timeout=10.0)
                bus.push_request(NS, _req("unk-1", "delete"))
                resp = bus.await_response(NS, timeout=10.0)
                return canonical(mask_bus_response(resp))

    result = differential(scenario)
    assert result["in_reply_to"] == "unk-1"
    assert result["payload"] == {"error": "unknown operation: delete", "code": "INTERNAL_ERROR"}


@pytest.mark.differential
def test_docker_unconfigured(daemon, differential):
    """The unconfigured crux: a minimal `docker:` block (ONLY approval.policy, NO
    registries/allow_all/config_path) and NO `~/.docker/config.json` under the isolated
    `$HOME`. The daemon STILL subscribes `docker-credentials` (proven by
    `wait_for_subscribe`) AND a `get` returns `REGISTRY_NOT_ALLOWED` (empty allowlist,
    allow_all false) — the live proof of the non-nil-when-unconfigured constructor."""
    docker_block = "docker:\n  approval:\n    policy: approve-all\n"

    def scenario(impl):
        with SyntheticBus() as bus:
            # NO install_docker_config → default ~/.docker/config.json is absent.
            with daemon(impl, _config(bus.url, docker_block)) as d:
                bus.wait_for_subscribe(NS, timeout=10.0)  # subscription proven on both
                bus.push_request(NS, _req("unc-1", "get", server_url="ghcr.io"))
                resp = bus.await_response(NS, timeout=10.0)
                audit = d.read_audit_jsonl(expect=1, timeout=10.0)[0]
                return {
                    "response": canonical(mask_bus_response(resp)),
                    "audit": canonical(mask_audit_entry(audit)),
                }

    result = differential(scenario)
    assert result["response"]["payload"] == {
        "error": "credential request failed",
        "code": "REGISTRY_NOT_ALLOWED",
    }
    assert result["audit"]["result"] == "error"
    assert result["audit"]["code"] == "REGISTRY_NOT_ALLOWED"
