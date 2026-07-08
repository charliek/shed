"""M3: the approval gate end-to-end against the fake host agent — policy
matrix, approve/deny round-trip, session grants, the all-namespace event
feed, and fail-closed expiry.

Each test uses a distinct shed name so session grants (4h, app-side state)
don't leak across tests.
"""

from __future__ import annotations

from _marks import needs_approvals
from client import ShedError

# The approval spine is shared (mac + tauri); each test drives the
# target-appropriate `client` fixture.
pytestmark = needs_approvals


def _pending_ids(client) -> set[str]:
    return {a["id"] for a in client.approvals_list()}


def _pane(client, target) -> str | None:
    """The pane the UI currently shows — the truth op differs per target (mac:
    `ui.state.pane`; tauri: `ui.current_pane`)."""
    return client.ui_state().get("pane") if target == "mac" else client.current_pane()


def test_app_connected_to_host_agent(fake):
    assert fake.wait_connected()


def test_prompt_then_approve(client, fake):
    rid = fake.emit_request("ssh-agent", "sign", "appr-shed", "ssh-ed25519")
    client.wait_until(lambda: rid in _pending_ids(client), what="request queued")
    client.approval_decide(rid, "approve")
    resp = fake.wait_response(rid)
    assert resp and resp["decision"] == "approve"
    client.wait_until(lambda: rid not in _pending_ids(client), what="request resolved")
    entry = next((e for e in client.activity_list() if e["id"] == rid), None)
    assert entry and entry["source"] == "app" and entry["result"] == "ok"
    assert entry["approval"] == "shed-desktop"


def test_prompt_then_deny(client, fake):
    rid = fake.emit_request("ssh-agent", "sign", "deny-shed")
    client.wait_until(lambda: rid in _pending_ids(client), what="request queued")
    client.approval_decide(rid, "deny")
    resp = fake.wait_response(rid)
    assert resp and resp["decision"] == "deny"


def test_policy_auto_approve(client, fake):
    client.policy_set([{"scope": "default", "action": "approve", "gate": "none"}])
    rid = fake.emit_request("ssh-agent", "sign", "auto-shed")
    resp = fake.wait_response(rid)
    assert resp and resp["decision"] == "approve" and resp["decided_by"] == "policy"
    assert rid not in _pending_ids(client)  # never queued


def test_policy_auto_deny(client, fake):
    client.policy_set([{"scope": "default", "action": "deny", "gate": "none"}])
    rid = fake.emit_request("ssh-agent", "sign", "block-shed")
    resp = fake.wait_response(rid)
    assert resp and resp["decision"] == "deny" and resp["decided_by"] == "policy"


def test_ssh_policy_always_allow_auto_approves(client, fake):
    # SSH "Always Allow" policy decides every sign with no prompt (a namespace
    # rule), not a per-shed grant — so the request is never queued.
    client.set_ssh_approval(policy="always-allow")
    try:
        rid = fake.emit_request("ssh-agent", "sign", "pol-allow-shed", "ssh-ed25519")
        resp = fake.wait_response(rid)
        assert resp and resp["decision"] == "approve" and resp["decided_by"] == "policy"
        assert rid not in _pending_ids(client)  # never prompted
    finally:
        client.set_ssh_approval(policy="time-based-allow")  # restore the prompting default


def test_ssh_policy_always_deny_auto_denies(client, fake):
    # SSH "Always Deny" policy denies every sign with no prompt.
    client.set_ssh_approval(policy="always-deny")
    try:
        rid = fake.emit_request("ssh-agent", "sign", "pol-deny-shed", "ssh-ed25519")
        resp = fake.wait_response(rid)
        assert resp and resp["decision"] == "deny" and resp["decided_by"] == "policy"
        assert rid not in _pending_ids(client)
    finally:
        client.set_ssh_approval(policy="time-based-allow")


def test_ssh_policy_change_resolves_pending(client, fake):
    # A card queued under a prompting policy must resolve when the policy flips
    # to a non-prompting one (Always Deny) — it can't linger and stay actionable.
    rid = fake.emit_request("ssh-agent", "sign", "flip-shed", "ssh-ed25519")
    client.wait_until(lambda: rid in _pending_ids(client), what="request queued")
    client.set_ssh_approval(policy="always-deny")
    try:
        resp = fake.wait_response(rid)
        assert resp and resp["decision"] == "deny" and resp["decided_by"] == "policy"
        client.wait_until(lambda: rid not in _pending_ids(client), what="pending resolved by policy change")
    finally:
        client.set_ssh_approval(policy="time-based-allow")


def test_session_grant_auto_approves_next(client, fake):
    rid1 = fake.emit_request("ssh-agent", "sign", "grant-shed")
    client.wait_until(lambda: rid1 in _pending_ids(client), what="first request queued")
    client.approval_decide(rid1, "approve", scope="per-session", ttl="1h")
    r1 = fake.wait_response(rid1)
    assert r1["decision"] == "approve" and r1.get("scope") == "per-session" and r1.get("ttl") == "1h"
    # A second request for the same namespace+shed is auto-approved by the grant.
    rid2 = fake.emit_request("ssh-agent", "sign", "grant-shed")
    resp2 = fake.wait_response(rid2)
    assert resp2 and resp2["decision"] == "approve" and resp2["decided_by"] == "policy"
    assert rid2 not in _pending_ids(client)


def test_server_field_propagates(client, fake):
    # #21 multi-server: a server-tagged request carries `server` through the
    # queue and into the app-side audit entry.
    rid = fake.emit_request("ssh-agent", "sign", "srv-shed", "ssh-ed25519", server="mini3")
    client.wait_until(lambda: rid in _pending_ids(client), what="request queued")
    req = next((a for a in client.approvals_list() if a["id"] == rid), None)
    assert req and req.get("server") == "mini3"
    client.approval_decide(rid, "approve")
    assert fake.wait_response(rid)["decision"] == "approve"
    client.wait_until(lambda: rid not in _pending_ids(client), what="request resolved")
    entry = next((e for e in client.activity_list() if e["id"] == rid), None)
    assert entry and entry.get("server") == "mini3"


def test_always_allow_persists_per_shed_rule(client, fake):
    # "Always allow" approves now AND installs a per-shed approve rule, so the
    # next request for that shed is auto-approved by policy (no prompt).
    rid1 = fake.emit_request("ssh-agent", "sign", "always-shed", "ssh-ed25519")
    client.wait_until(lambda: rid1 in _pending_ids(client), what="first request queued")
    client.approval_decide(rid1, "approve", persist=True)
    r1 = fake.wait_response(rid1)
    assert r1["decision"] == "approve" and r1.get("scope") == "always"
    assert any(r.get("scope") == "shed" and r.get("shed") == "always-shed" and r.get("action") == "approve"
               for r in client.policy_list())
    rid2 = fake.emit_request("ssh-agent", "sign", "always-shed")
    resp2 = fake.wait_response(rid2)
    assert resp2 and resp2["decision"] == "approve" and resp2["decided_by"] == "policy"
    assert rid2 not in _pending_ids(client)


def test_event_stream_covers_all_namespaces(client, fake):
    fake.emit_event("aws-credentials", "get_credentials", "evt-shed", result="ok", approval="none")
    client.wait_until(
        lambda: any(e["ns"] == "aws-credentials" and e["shed"] == "evt-shed" for e in client.activity_list()),
        what="aws event in activity feed")


def test_notification_posted_and_invoked(client, fake):
    # A prompt posts an actionable notification; driving its Approve action
    # resolves the request over the UDS, then the notification is withdrawn.
    rid = fake.emit_request("ssh-agent", "sign", "notif-shed", "ssh-ed25519")
    client.wait_until(lambda: rid in _pending_ids(client), what="request queued")
    client.wait_until(lambda: any(n["id"] == rid for n in client.notifications_list()),
                    what="notification posted")
    client.notification_invoke(rid, "approve")
    resp = fake.wait_response(rid)
    assert resp and resp["decision"] == "approve"
    client.wait_until(lambda: all(n["id"] != rid for n in client.notifications_list()),
                    what="notification withdrawn")
    assert rid not in _pending_ids(client)


def test_notification_open_navigates_to_approvals(client, fake, target):
    # Tapping the banner body (not Approve/Deny) opens the dashboard on the
    # Approvals pane and leaves the request pending.
    client.navigate("sheds")
    rid = fake.emit_request("ssh-agent", "sign", "open-shed", "ssh-ed25519")
    client.wait_until(lambda: rid in _pending_ids(client), what="request queued")
    client.notification_open()
    client.wait_until(lambda: _pane(client, target) == "approvals", what="navigated to approvals")
    if target == "mac":
        assert client.window_state().get("visible") is True
    assert rid in _pending_ids(client)  # default tap doesn't decide it
    client.approval_decide(rid, "deny")  # cleanup
    client.wait_until(lambda: rid not in _pending_ids(client), what="cleanup resolved")


def test_notification_not_posted_for_auto_policy(client, fake):
    # An auto-approve never prompts, so it never posts a notification.
    client.policy_set([{"scope": "default", "action": "approve", "gate": "none"}])
    rid = fake.emit_request("ssh-agent", "sign", "notif-auto-shed")
    assert fake.wait_response(rid)["decision"] == "approve"
    assert all(n["id"] != rid for n in client.notifications_list())


def test_always_deny_persists_per_shed_rule(client, fake):
    # "Always deny" denies now AND installs a per-shed deny rule, so the next
    # request for that shed is auto-denied by policy (no prompt).
    rid1 = fake.emit_request("ssh-agent", "sign", "deny-shed", "ssh-ed25519")
    client.wait_until(lambda: rid1 in _pending_ids(client), what="first request queued")
    client.approval_decide(rid1, "deny", persist=True)
    assert fake.wait_response(rid1)["decision"] == "deny"
    assert any(r.get("scope") == "shed" and r.get("shed") == "deny-shed" and r.get("action") == "deny"
               for r in client.policy_list())
    rid2 = fake.emit_request("ssh-agent", "sign", "deny-shed")
    resp2 = fake.wait_response(rid2)
    assert resp2 and resp2["decision"] == "deny" and resp2["decided_by"] == "policy"
    assert rid2 not in _pending_ids(client)


def test_time_based_allow_invalid_ttl_falls_back_to_default(client, fake):
    # An empty/invalid duration on a Time Based grant falls back to the 2h default,
    # and that applied value (not the raw input) is what's reported to the host.
    rid = fake.emit_request("ssh-agent", "sign", "ttl-shed", "ssh-ed25519")
    client.wait_until(lambda: rid in _pending_ids(client), what="request queued")
    client.approval_decide(rid, "approve", scope="per-session", ttl="garbage")
    r = fake.wait_response(rid)
    assert r["decision"] == "approve" and r.get("scope") == "per-session" and r.get("ttl") == "2h"


def test_per_shed_sticky_grant(client, fake):
    # Per Shed: approve once, then auto-approve repeats for that shed (a sticky
    # grant with no TTL); a different shed still prompts.
    rid1 = fake.emit_request("ssh-agent", "sign", "sticky-shed", "ssh-ed25519")
    client.wait_until(lambda: rid1 in _pending_ids(client), what="first request queued")
    client.approval_decide(rid1, "approve", scope="per-shed")  # no ttl
    r1 = fake.wait_response(rid1)
    assert r1["decision"] == "approve" and r1.get("scope") == "per-shed" and not r1.get("ttl")
    # A second request for the same shed is auto-approved by the sticky grant.
    rid2 = fake.emit_request("ssh-agent", "sign", "sticky-shed")
    r2 = fake.wait_response(rid2)
    assert r2 and r2["decision"] == "approve" and r2["decided_by"] == "policy"
    assert rid2 not in _pending_ids(client)
    # A different shed still prompts — the grant is per-(server, shed).
    rid3 = fake.emit_request("ssh-agent", "sign", "other-sticky-shed", "ssh-ed25519")
    client.wait_until(lambda: rid3 in _pending_ids(client), what="a different shed still prompts")
    client.approval_decide(rid3, "deny")  # cleanup
    client.wait_until(lambda: rid3 not in _pending_ids(client), what="cleanup denial resolved")


def test_ssh_pref_change_resets_session_grant(client, fake):
    # A live SSH grant auto-approves repeats; changing an SSH approval setting
    # clears the in-memory grant so the next request prompts again.
    rid1 = fake.emit_request("ssh-agent", "sign", "reset-shed", "ssh-ed25519")
    client.wait_until(lambda: rid1 in _pending_ids(client), what="first request queued")
    client.approval_decide(rid1, "approve", scope="per-session", ttl="1h")
    assert fake.wait_response(rid1)["decision"] == "approve"
    # Second request for the shed is auto-approved by the grant (no prompt).
    rid2 = fake.emit_request("ssh-agent", "sign", "reset-shed")
    r2 = fake.wait_response(rid2)
    assert r2 and r2["decision"] == "approve" and r2["decided_by"] == "policy"
    assert rid2 not in _pending_ids(client)
    # Change an SSH approval setting → clears the live grant.
    client.set_ssh_approval(policy="always-ask")
    # Now the same shed PROMPTS again (grant gone), rather than auto-approving.
    rid3 = fake.emit_request("ssh-agent", "sign", "reset-shed", "ssh-ed25519")
    client.wait_until(lambda: rid3 in _pending_ids(client), what="re-prompts after pref change")
    client.approval_decide(rid3, "deny")  # cleanup
    client.wait_until(lambda: rid3 not in _pending_ids(client), what="cleanup denial resolved")


def test_deny_evicts_live_session_grant(client, fake):
    # A deny must supersede a live "approve for this session" grant — otherwise
    # the grant (highest precedence) would keep auto-approving past the deny.
    rid1 = fake.emit_request("ssh-agent", "sign", "evict-shed", "ssh-ed25519")
    rid2 = fake.emit_request("ssh-agent", "sign", "evict-shed", "ssh-ed25519")
    client.wait_until(lambda: rid1 in _pending_ids(client) and rid2 in _pending_ids(client),
                    what="both requests queued")
    # Approve rid1 with a session grant; rid2 stays pending (queued before the grant).
    client.approval_decide(rid1, "approve", scope="per-session", ttl="1h")
    assert fake.wait_response(rid1)["decision"] == "approve"
    # Always-deny rid2 → installs a deny rule AND evicts the grant.
    client.approval_decide(rid2, "deny", persist=True)
    assert fake.wait_response(rid2)["decision"] == "deny"
    # A fresh request for the shed is now DENIED by policy (grant gone, deny rule wins).
    rid3 = fake.emit_request("ssh-agent", "sign", "evict-shed")
    resp3 = fake.wait_response(rid3)
    assert resp3 and resp3["decision"] == "deny" and resp3["decided_by"] == "policy"


def test_pending_item_exposes_gate_and_defaults(client, fake):
    # The pending item carries the decided gate (which drives the fingerprint
    # icon) plus the SSH scope/TTL defaults the card pre-fills — observable over
    # IPC, so the no-fingerprint-for-prompt path is assertable without pixels.
    client.policy_set([{"scope": "default", "action": "prompt", "gate": "none"}])
    rid = fake.emit_request("ssh-agent", "sign", "gate-shed", "ssh-ed25519")
    client.wait_until(lambda: rid in _pending_ids(client), what="request queued")
    item = next(a for a in client.approvals_list() if a["id"] == rid)
    # gate "none" → the card shows a plain Approve (no Touch ID / fingerprint).
    assert item["gate"] == "none"
    assert "default_scope" in item and "default_ttl" in item
    client.approval_decide(rid, "approve")

    # A biometric gate is surfaced too.
    client.policy_set([{"scope": "default", "action": "prompt", "gate": "biometrics-or-password"}])
    rid2 = fake.emit_request("ssh-agent", "sign", "gate-shed2", "ssh-ed25519")
    client.wait_until(lambda: rid2 in _pending_ids(client), what="request queued")
    item2 = next(a for a in client.approvals_list() if a["id"] == rid2)
    assert item2["gate"] == "biometrics-or-password"
    client.approval_decide(rid2, "approve")


def test_per_server_shed_isolation(client, fake):
    # #21: a per-(server,shed) rule applies only to its own server, even when
    # the shed name is identical on another server.
    client.policy_set([
        {"scope": "default", "action": "prompt", "gate": "biometrics-or-password"},
        {"scope": "shed", "server": "mini3", "shed": "dual", "action": "approve", "gate": "none"},
    ])
    rid_a = fake.emit_request("ssh-agent", "sign", "dual", server="mini3")
    resp_a = fake.wait_response(rid_a)
    assert resp_a and resp_a["decision"] == "approve" and resp_a["decided_by"] == "policy"
    # Same shed name on a different server is NOT covered → it prompts (queues).
    rid_b = fake.emit_request("ssh-agent", "sign", "dual", server="studio")
    client.wait_until(lambda: rid_b in _pending_ids(client), what="other-server request queued")
    # Don't leak a pending request (+ its notification) into later tests.
    client.approval_decide(rid_b, "deny")
    client.wait_until(lambda: rid_b not in _pending_ids(client), what="request resolved")


def test_audit_log_path_exposed(client):
    # FR-6: the append-only audit log path is discoverable (the Activity pane's
    # "Reveal log" reveals this file).
    path = client.activity_log_path()
    assert path.endswith("audit.jsonl")


def test_expiry_fails_closed(client, fake):
    rid = fake.emit_request("ssh-agent", "sign", "expire-shed", expires_in_s=1.0)
    client.wait_until(lambda: rid in _pending_ids(client), what="request queued")
    resp = fake.wait_response(rid, timeout=8)  # left undecided → auto-deny on expiry
    assert resp and resp["decision"] == "deny" and resp["decided_by"] == "timeout"
    assert rid not in _pending_ids(client)


def test_disconnect_drops_pending_fail_closed(client, fake):
    # F3 (coverage add over the mac suite): a host-agent disconnect drops ALL
    # pending — the user can't act on, or persist a rule from, a stale prompt — and
    # a late decide is a no-op (the agent fails closed on its side regardless).
    hellos = fake.hello_count
    rid = fake.emit_request("ssh-agent", "sign", "drop-shed", "ssh-ed25519")
    client.wait_until(lambda: rid in _pending_ids(client), what="request queued")
    fake.drop_connection()
    client.wait_until(lambda: rid not in _pending_ids(client), what="pending dropped on disconnect")
    # Wait for the client's backoff-reconnect (so later tests have a live channel).
    assert fake.wait_hello_count(hellos + 1), "client did not reconnect after the drop"
    # A decide for the now-gone request never sends an approve to the host. The app
    # either rejects it (mac: not-found) or silently no-ops (tauri) — both fail closed.
    try:
        client.approval_decide(rid, "approve")
    except ShedError as e:
        assert e.code in ("not-found", "not_found")
    assert fake.wait_response(rid, timeout=2) is None  # no late approve
