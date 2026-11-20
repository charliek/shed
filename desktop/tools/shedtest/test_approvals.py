"""M3: the approval gate end-to-end through the app's own broker — policy
matrix, approve/deny round-trip, session grants, the all-namespace event feed.

Every request is a real one: pushed on a session mock's plugin bus
(`bus_stimulus`), read by the app's broker, gated through the app, and answered
on the bus. The outcome is read from the bus response (`verdict`) and the
broker's audit file (`audit_line`: `decided_by`, `scope`, `ttl`).

Each test uses a distinct shed name so session grants (app-side state) and the
audit file's per-shed lines don't leak across tests.
"""

from __future__ import annotations

import platform

from bus_stimulus import (
    AWS,
    DOCKER,
    SSH,
    audit_line,
    emit_aws,
    emit_docker,
    emit_sign,
    new_approval,
    pending_ids,
    verdict,
)

PNG_MAGIC = b"\x89PNG\r\n\x1a\n"


def _assert_window_renders(client) -> None:
    """A non-empty PNG of the dashboard. Linux/Xvfb only: on macOS the capture is
    Screen-Recording-TCC-gated for a harness-launched binary."""
    if platform.system() == "Darwin":
        return
    client.show_window()
    shot: dict = {}

    def grab() -> bool:
        png, width, height = client.screenshot(scale=1)
        shot.update(png=png, width=width, height=height)
        return True

    client.wait_until(grab, timeout=20, what="window screenshot")
    assert shot["png"][:8] == PNG_MAGIC and shot["width"] > 0 and shot["height"] > 0


def _prompt(client, mock, shed, broker):
    """Emit one sign request for `shed` and return `(envelope id, approval id)`."""
    before = pending_ids(client)
    eid = emit_sign(mock, shed, broker)
    return eid, new_approval(client, shed, before)


def test_broker_connected(mock, studio):
    for server in (mock, studio):
        assert set(server.subscribed_namespaces()) >= {SSH, AWS, DOCKER}, \
            server.subscribed_namespaces()


def test_prompt_then_approve(client, mock, broker):
    eid, aid = _prompt(client, mock, "appr-shed", broker)
    client.approval_decide(aid, "approve")
    assert verdict(mock, SSH, eid) == "approve"
    client.wait_until(lambda: aid not in pending_ids(client), what="request resolved")
    entry = next((e for e in client.activity_list() if e["id"] == aid), None)
    assert entry and entry["source"] == "app" and entry["result"] == "ok"
    assert entry["approval"] == "shed-desktop"


def test_prompt_then_deny(client, mock, broker):
    eid, aid = _prompt(client, mock, "deny-shed", broker)
    client.approval_decide(aid, "deny")
    assert verdict(mock, SSH, eid) == "deny"
    entry = next((e for e in client.activity_list() if e["id"] == aid), None)
    assert entry and entry["source"] == "app" and entry["result"] == "denied"


def test_policy_auto_approve(client, mock, broker):
    client.policy_set([{"scope": "default", "action": "approve", "gate": "none"}])
    before = pending_ids(client)
    eid = emit_sign(mock, "auto-shed", broker)
    assert verdict(mock, SSH, eid) == "approve"
    assert audit_line(broker, SSH, "auto-shed")["decided_by"] == "policy"
    assert not any(a.get("shed") == "auto-shed" for a in client.approvals_list()
                   if a["id"] not in before)  # never queued


def test_policy_auto_deny(client, mock, broker):
    client.policy_set([{"scope": "default", "action": "deny", "gate": "none"}])
    eid = emit_sign(mock, "block-shed", broker)
    assert verdict(mock, SSH, eid) == "deny"
    assert audit_line(broker, SSH, "block-shed")["decided_by"] == "policy"


def test_ssh_policy_always_allow_auto_approves(client, mock, broker):
    # SSH "Always Allow" policy decides every sign with no prompt (a namespace
    # rule), not a per-shed grant — so the request is never queued.
    client.set_ssh_approval(policy="always-allow")
    try:
        eid = emit_sign(mock, "pol-allow-shed", broker)
        assert verdict(mock, SSH, eid) == "approve"
        assert audit_line(broker, SSH, "pol-allow-shed")["decided_by"] == "policy"
        assert not any(a.get("shed") == "pol-allow-shed" for a in client.approvals_list())
    finally:
        client.set_ssh_approval(policy="time-based-allow")  # restore the prompting default


def test_ssh_policy_always_deny_auto_denies(client, mock, broker):
    # SSH "Always Deny" policy denies every sign with no prompt.
    client.set_ssh_approval(policy="always-deny")
    try:
        eid = emit_sign(mock, "pol-deny-shed", broker)
        assert verdict(mock, SSH, eid) == "deny"
        assert audit_line(broker, SSH, "pol-deny-shed")["decided_by"] == "policy"
        assert not any(a.get("shed") == "pol-deny-shed" for a in client.approvals_list())
    finally:
        client.set_ssh_approval(policy="time-based-allow")


def test_ssh_policy_change_resolves_pending(client, mock, broker):
    # A card queued under a prompting policy must resolve when the policy flips
    # to a non-prompting one (Always Deny) — it can't linger and stay actionable.
    eid, aid = _prompt(client, mock, "flip-shed", broker)
    client.set_ssh_approval(policy="always-deny")
    try:
        assert verdict(mock, SSH, eid) == "deny"
        assert audit_line(broker, SSH, "flip-shed")["decided_by"] == "policy"
        client.wait_until(lambda: aid not in pending_ids(client),
                          what="pending resolved by policy change")
    finally:
        client.set_ssh_approval(policy="time-based-allow")


def test_session_grant_auto_approves_next(client, mock, broker):
    eid1, aid1 = _prompt(client, mock, "grant-shed", broker)
    client.approval_decide(aid1, "approve", scope="per-session", ttl="1h")
    assert verdict(mock, SSH, eid1) == "approve"
    first = audit_line(broker, SSH, "grant-shed", 1)
    assert first.get("scope") == "per-session" and first.get("ttl") == "1h", first
    # A second request for the same namespace+shed is auto-approved by the grant.
    before = pending_ids(client)
    eid2 = emit_sign(mock, "grant-shed", broker)
    assert verdict(mock, SSH, eid2) == "approve"
    assert audit_line(broker, SSH, "grant-shed", 2)["decided_by"] == "policy"
    assert pending_ids(client) - before == set()


def test_server_field_propagates(client, mock, broker):
    # #21 multi-server: the broker stamps the request with its server's name,
    # which rides through the queue and into the app-side audit entry.
    eid, aid = _prompt(client, mock, "srv-shed", broker)
    req = next((a for a in client.approvals_list() if a["id"] == aid), None)
    assert req and req.get("server") == "mini3"
    client.approval_decide(aid, "approve")
    assert verdict(mock, SSH, eid) == "approve"
    client.wait_until(lambda: aid not in pending_ids(client), what="request resolved")
    entry = next((e for e in client.activity_list() if e["id"] == aid), None)
    assert entry and entry.get("server") == "mini3"


def test_always_allow_persists_per_shed_rule(client, mock, broker):
    # "Always allow" approves now AND installs a per-shed approve rule, so the
    # next request for that shed is auto-approved by policy (no prompt).
    eid1, aid1 = _prompt(client, mock, "always-shed", broker)
    client.approval_decide(aid1, "approve", persist=True)
    assert verdict(mock, SSH, eid1) == "approve"
    assert audit_line(broker, SSH, "always-shed", 1).get("scope") == "always"
    assert any(r.get("scope") == "shed" and r.get("shed") == "always-shed" and r.get("action") == "approve"
               for r in client.policy_list())
    before = pending_ids(client)
    eid2 = emit_sign(mock, "always-shed", broker)
    assert verdict(mock, SSH, eid2) == "approve"
    assert audit_line(broker, SSH, "always-shed", 2)["decided_by"] == "policy"
    assert pending_ids(client) - before == set()


def test_event_stream_covers_all_namespaces(client, mock, broker):
    # With the AWS and Docker provider modes on deny (their default; set here
    # because the per-test `policy.set` reset replaces the engine's namespace
    # rules until the next rebuild), the Coordinator decides these by policy; the
    # broker's own audit row for each must reach the activity feed carrying its
    # namespace.
    for ns, emit in ((AWS, emit_aws), (DOCKER, emit_docker)):
        client.set_provider_mode(ns, "deny")
        eid = emit(mock, "evt-shed")
        assert verdict(mock, ns, eid) == "deny"
        client.wait_until(
            lambda ns=ns: any(e.get("source") == "broker" and e.get("ns") == ns
                              and e.get("shed") == "evt-shed"
                              for e in client.activity_list()),
            timeout=15, what=f"the broker's {ns} event in the activity feed")


def test_notification_posted_and_invoked(client, mock, broker):
    # A prompt posts an actionable notification; driving its Approve action
    # resolves the request, then the notification is withdrawn.
    eid, aid = _prompt(client, mock, "notif-shed", broker)
    client.wait_until(lambda: any(n["id"] == aid for n in client.notifications_list()),
                      what="notification posted")
    client.notification_invoke(aid, "approve")
    assert verdict(mock, SSH, eid) == "approve"
    client.wait_until(lambda: all(n["id"] != aid for n in client.notifications_list()),
                      what="notification withdrawn")
    assert aid not in pending_ids(client)


def test_notification_open_navigates_to_approvals(client, mock, broker):
    # Tapping the banner body (not Approve/Deny) opens the dashboard on the
    # Approvals pane and leaves the request pending.
    client.navigate("sheds")
    eid, aid = _prompt(client, mock, "open-shed", broker)
    client.notification_open()
    client.wait_until(lambda: client.current_pane() == "approvals", what="navigated to approvals")
    assert aid in pending_ids(client)  # default tap doesn't decide it
    client.approval_decide(aid, "deny")  # cleanup
    assert verdict(mock, SSH, eid) == "deny"
    client.wait_until(lambda: aid not in pending_ids(client), what="cleanup resolved")


def test_notification_not_posted_for_auto_policy(client, mock, broker):
    # An auto-approve never prompts, so it never posts a notification.
    client.policy_set([{"scope": "default", "action": "approve", "gate": "none"}])
    before = {n["id"] for n in client.notifications_list()}
    eid = emit_sign(mock, "notif-auto-shed", broker)
    assert verdict(mock, SSH, eid) == "approve"
    assert {n["id"] for n in client.notifications_list()} <= before


def test_always_deny_persists_per_shed_rule(client, mock, broker):
    # "Always deny" denies now AND installs a per-shed deny rule, so the next
    # request for that shed is auto-denied by policy (no prompt).
    eid1, aid1 = _prompt(client, mock, "always-deny-shed", broker)
    client.approval_decide(aid1, "deny", persist=True)
    assert verdict(mock, SSH, eid1) == "deny"
    assert audit_line(broker, SSH, "always-deny-shed", 1).get("scope") == "always"
    assert any(r.get("scope") == "shed" and r.get("shed") == "always-deny-shed"
               and r.get("action") == "deny" for r in client.policy_list())
    before = pending_ids(client)
    eid2 = emit_sign(mock, "always-deny-shed", broker)
    assert verdict(mock, SSH, eid2) == "deny"
    assert audit_line(broker, SSH, "always-deny-shed", 2)["decided_by"] == "policy"
    assert pending_ids(client) - before == set()


def test_time_based_allow_invalid_ttl_falls_back_to_default(client, mock, broker):
    # An empty/invalid duration on a Time Based grant falls back to the 2h default,
    # and that applied value (not the raw input) is what's reported to the broker.
    eid, aid = _prompt(client, mock, "ttl-shed", broker)
    client.approval_decide(aid, "approve", scope="per-session", ttl="garbage")
    assert verdict(mock, SSH, eid) == "approve"
    line = audit_line(broker, SSH, "ttl-shed")
    assert line.get("scope") == "per-session" and line.get("ttl") == "2h", line


def test_per_shed_sticky_grant(client, mock, broker):
    # Per Shed: approve once, then auto-approve repeats for that shed (a sticky
    # grant with no TTL); a different shed still prompts.
    eid1, aid1 = _prompt(client, mock, "sticky-shed", broker)
    client.approval_decide(aid1, "approve", scope="per-shed")  # no ttl
    assert verdict(mock, SSH, eid1) == "approve"
    first = audit_line(broker, SSH, "sticky-shed", 1)
    assert first.get("scope") == "per-shed" and not first.get("ttl"), first
    # A second request for the same shed is auto-approved by the sticky grant.
    before = pending_ids(client)
    eid2 = emit_sign(mock, "sticky-shed", broker)
    assert verdict(mock, SSH, eid2) == "approve"
    assert audit_line(broker, SSH, "sticky-shed", 2)["decided_by"] == "policy"
    assert pending_ids(client) - before == set()
    # A different shed still prompts — the grant is per-(server, shed).
    eid3, aid3 = _prompt(client, mock, "other-sticky-shed", broker)
    client.approval_decide(aid3, "deny")  # cleanup
    assert verdict(mock, SSH, eid3) == "deny"
    client.wait_until(lambda: aid3 not in pending_ids(client), what="cleanup denial resolved")


def test_ssh_pref_change_resets_session_grant(client, mock, broker):
    # A live SSH grant auto-approves repeats; changing an SSH approval setting
    # clears the in-memory grant so the next request prompts again.
    eid1, aid1 = _prompt(client, mock, "reset-shed", broker)
    client.approval_decide(aid1, "approve", scope="per-session", ttl="1h")
    assert verdict(mock, SSH, eid1) == "approve"
    # Second request for the shed is auto-approved by the grant (no prompt).
    before = pending_ids(client)
    eid2 = emit_sign(mock, "reset-shed", broker)
    assert verdict(mock, SSH, eid2) == "approve"
    assert audit_line(broker, SSH, "reset-shed", 2)["decided_by"] == "policy"
    assert pending_ids(client) - before == set()
    # Change an SSH approval setting → clears the live grant.
    client.set_ssh_approval(policy="always-ask")
    # Now the same shed PROMPTS again (grant gone), rather than auto-approving.
    eid3, aid3 = _prompt(client, mock, "reset-shed", broker)
    client.approval_decide(aid3, "deny")  # cleanup
    assert verdict(mock, SSH, eid3) == "deny"
    client.wait_until(lambda: aid3 not in pending_ids(client), what="cleanup denial resolved")


def test_pending_item_exposes_gate_and_defaults(client, mock, broker):
    # The pending item carries the decided gate (which drives the fingerprint
    # icon) plus the SSH scope/TTL defaults the card pre-fills — observable over
    # IPC, so the no-fingerprint-for-prompt path is assertable without pixels.
    client.policy_set([{"scope": "default", "action": "prompt", "gate": "none"}])
    eid, aid = _prompt(client, mock, "gate-shed", broker)
    item = next(a for a in client.approvals_list() if a["id"] == aid)
    # gate "none" → the card shows a plain Approve (no Touch ID / fingerprint).
    assert item["gate"] == "none"
    assert "default_scope" in item and "default_ttl" in item
    client.approval_decide(aid, "approve")
    assert verdict(mock, SSH, eid) == "approve"

    # A biometric gate is surfaced too.
    client.policy_set([{"scope": "default", "action": "prompt", "gate": "biometrics-or-password"}])
    eid2, aid2 = _prompt(client, mock, "gate-shed2", broker)
    item2 = next(a for a in client.approvals_list() if a["id"] == aid2)
    assert item2["gate"] == "biometrics-or-password"
    client.approval_decide(aid2, "approve")
    assert verdict(mock, SSH, eid2) == "approve"


def test_per_server_shed_isolation(client, mock, studio, broker):
    # #21: a per-(server,shed) rule applies only to its own server, even when
    # the shed name is identical on another server.
    client.policy_set([
        {"scope": "default", "action": "prompt", "gate": "biometrics-or-password"},
        {"scope": "shed", "server": "mini3", "shed": "dual", "action": "approve", "gate": "none"},
    ])
    eid_a = emit_sign(mock, "dual", broker)
    assert verdict(mock, SSH, eid_a) == "approve"
    line = audit_line(broker, SSH, "dual", 1)
    assert line["decided_by"] == "policy" and line["server"] == "mini3", line
    # Same shed name on a different server is NOT covered → it prompts (queues).
    before = pending_ids(client)
    eid_b = emit_sign(studio, "dual", broker)
    aid_b = new_approval(client, "dual", before)
    assert next(a for a in client.approvals_list() if a["id"] == aid_b)["server"] == "studio"
    # Don't leak a pending request (+ its notification) into later tests.
    client.approval_decide(aid_b, "deny")
    assert verdict(studio, SSH, eid_b) == "deny"
    client.wait_until(lambda: aid_b not in pending_ids(client), what="request resolved")


def test_audit_log_path_exposed(client):
    # FR-6: the append-only audit log path is discoverable (the Activity pane's
    # "Reveal log" reveals this file).
    path = client.activity_log_path()
    assert path.endswith("audit.jsonl")


def test_approvals_pane_shows_a_pending_request(client, mock, broker):
    # The Approvals pane renders a real pending request, not just its empty state:
    # the sidebar's pending badge (UI truth) counts it while the pane is up.
    eid, aid = _prompt(client, mock, "shot-shed", broker)
    client.navigate("approvals")
    client.wait_until(lambda: client.current_pane() == "approvals", what="approvals pane up")
    client.wait_until(lambda: (client.badges() or {}).get("pending") == 1, timeout=15,
                      what="pending badge counts the request")
    _assert_window_renders(client)
    client.approval_decide(aid, "deny")
    assert verdict(mock, SSH, eid) == "deny"
    client.wait_until(lambda: (client.badges() or {}).get("pending") == 0, timeout=15,
                      what="pending badge cleared")


def test_activity_pane_renders_streamed_events(client, mock):
    client.set_provider_mode(AWS, "deny")  # decided by policy, never a prompt
    eid = emit_aws(mock, "activity-shed")
    assert verdict(mock, AWS, eid) == "deny"
    client.wait_until(lambda: any(e.get("shed") == "activity-shed" for e in client.activity_list()),
                      what="event in the activity feed")
    client.navigate("activity")
    client.wait_until(lambda: client.current_pane() == "activity", what="activity pane up")
    _assert_window_renders(client)
