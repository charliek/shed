"""Named users (`auth.users`) on one dev server: the API and SSH doors.

Two users and an admin drive one dev server, each as itself (its own key,
its own CLI config directory and client certificate; `fixtures/identity.py`),
through the real `shed` CLI, the system `ssh`, `sftp` and `scp`, and the
HTTPS API over pinned mTLS. What it shows, live:

  - each user sees only their own sheds (`shed list`, `GET /api/overview`);
    an admin sees every user's, with its user;
  - another user's shed does not exist for a user: every API route on it is
    404, and a session, `shed exec`, sftp, scp and a forwarded port are
    refused; a stopped one is not woken;
  - a taken name is the same 409 for everyone and names nobody;
  - an admin may list, stop and delete another user's shed, and is refused
    entry (API, SSH, forward) with a message saying so;
  - a user who is not an admin is refused host directories, server mounts,
    an image that is not an alias, and the image store; snapshots belong to
    their user;
  - a script key cannot take the credentials scope; an expired key opens
    nothing;
  - a credential-bus request from a user's shed reaches only that user's
    listener, and only that user may answer it (a synthetic listener,
    `fixtures/busclient.py`; no real host agent);
  - `shed assign` refuses a non-admin, an unknown user and a shed that has a
    user.

The one-time upgrade step and assigning a shed with no user need a server
restarted with another config: `test_users_upgrade.py` (Mac dev server).
Service clients beside users: `test_service_client.py`.

Backends: `users_env` (conftest) is [vz] (the Mac dev server, restarted with
the itest user list for this module and restored after), [fc] and [remote]
(a dev server STARTED with the user list, `SHED_ITEST_USERS`). On [remote]
every shed is a billed cloud VM and at most two run at once, so only the
cases that prove ownership run there (one shed per user); the rest carry
`local_vm_only`.

The module runs in any order: the one case that deletes a shared shed
takes it out of the module's hands first, and the next case that wants one
gets a new one. Every clean-up is checked: a shed or snapshot that cannot be
shown gone fails the teardown with its name and server.

Run only these cases:

    cd tests/integration && uv run pytest -v test_users.py

with the environment the README's "Named users" section lists.
"""

from __future__ import annotations

import shutil
import subprocess
import threading
import time
import uuid
from typing import Optional

import pytest

from fixtures.busclient import (
    BusListener,
    FakeMachine,
    ensure_source,
    machine_headers,
    payload_has,
    publish_argv,
    response_body,
)
from fixtures.identity import ADMIN, ALICE, BOB, Identity, UsersEnv
from fixtures.tlsclient import error_code, error_message, pinned_request

LOCAL = "the remote run is kept to one shed per user, which proves ownership"
NOT_FOUND = "SHED_NOT_FOUND"
ANOTHER_USER = "belongs to another user"
# What a forwarded port to a shed the caller may not see is refused with: the
# channel open's reason (internal/sshd/server.go, handleDirectTCPIP), which
# OpenSSH prints as "open failed: connect failed: shed not found".
FORWARD_NOT_FOUND = "shed not found"


def _session_not_found(shed: str) -> str:
    """What a session, sftp or scp into a shed the caller may not see writes
    to stderr (internal/sshd/wake.go ensureRunning, as "Error: ..."): the
    same words as for a shed that does not exist."""
    return f"shed '{shed}' not found"


# The credential bus round trip. The guest gives up on a request
# publishTimeout after it sends it (cmd/shed-agent/http.go: 30 s), so every
# step for one request, from starting it in the shed to the owner's answer,
# fits inside that with margin. The clock starts before `shed exec` runs,
# which is earlier than the guest's, so the margin is larger than it looks.
GUEST_REQUEST_LIFETIME = 30
DELIVERY_BUDGET = 14  # started in the shed -> the owner's listener has it
FORGERY_TIMEOUT = 4  # each other user's answer (all sent side by side)
ANSWER_BY = 22  # the owner's answer is sent by then ...
ANSWER_TIMEOUT = 5  # ... and accepted within this: 27 s, inside 30 s
# The server writes a keepalive on every subscription every 25 s
# (internal/api/plugin_handlers.go: sseKeepaliveInterval).
LIVENESS_TIMEOUT = 40


# ----------------------------------------------------------------------------
# Helpers
# ----------------------------------------------------------------------------


def _timeout(env: UsersEnv) -> int:
    return 600 if env.backend == "remote" else 300


def _marker() -> str:
    return f"itest-entered-{uuid.uuid4().hex[:10]}"


def _create(env: UsersEnv, who: Identity, name: str) -> None:
    args = ["create", name, "--no-sync"]
    if env.image:
        args += ["--image", env.image]
    r = who.cli(*args, timeout=_timeout(env))
    assert r.returncode == 0, f"{who.user}: shed create {name} failed (exit {r.returncode}): {r.stdout[-800:]!r} {r.stderr[-800:]!r}"


def _delete(env: UsersEnv, who: Identity, name: str) -> None:
    """Clean-up that cannot pass quietly: the owner deletes the shed; if it
    is still there, the developer does (user `dev`, an admin, who may delete
    any shed); then it must be shown gone (`LocalServer.ensure_deleted`).
    Otherwise this fails, naming the shed and the server: on [remote] a shed
    left behind is a billed VM. A name never created is simply not there."""
    earlier = []
    try:
        r = who.cli("delete", name, "-f", timeout=_timeout(env))
        if r.returncode != 0:
            earlier.append(f"{who.user}'s `shed delete {name}` exited {r.returncode}: "
                           f"{(r.stderr or r.stdout).strip()[-400:]!r}")
    except subprocess.TimeoutExpired:
        earlier.append(f"{who.user}'s `shed delete {name}` timed out after {_timeout(env)} s")
    env.server.ensure_deleted(name, earlier=earlier)


def _shed_fixture(env: UsersEnv, who: Identity, prefix: str):
    name = f"{prefix}-{uuid.uuid4().hex[:8]}"
    try:
        _create(env, who, name)
    except BaseException as e:
        try:
            _delete(env, who, name)
        except AssertionError as cleanup:
            e.add_note(str(cleanup))
        raise
    return name


class _ShedSlot:
    """One user's shed, made when a case first asks for it and shared by the
    module's cases after that. A case that deletes it `take()`s it first: that
    case then owns its clean-up, and the next case that asks gets a new shed.
    So the module runs in any order, with never more than one such shed at a
    time (on [remote], at most two cloud VMs: this and alice's)."""

    def __init__(self, env: UsersEnv, who: Identity, prefix: str) -> None:
        self.env, self.who, self.prefix = env, who, prefix
        self.name: Optional[str] = None

    def get(self) -> str:
        if self.name is None:
            self.name = _shed_fixture(self.env, self.who, self.prefix)
        return self.name

    def take(self) -> str:
        name = self.get()
        self.name = None
        return name

    def close(self) -> None:
        if self.name is not None:
            name, self.name = self.name, None
            _delete(self.env, self.who, name)


def _run_all(steps) -> None:
    """Run every clean-up step even when one fails, then fail with all of
    their failures."""
    failures = []
    for step in steps:
        try:
            step()
        except Exception as e:  # noqa: BLE001 - reported below, with the rest
            failures.append(f"{type(e).__name__}: {e}")
    if failures:
        raise AssertionError("clean-up failed:\n" + "\n".join(failures))


def _get(who: Identity, name: str) -> tuple[int, Optional[dict]]:
    return who.api().request("GET", f"/api/sheds/{name}")


def _status(who: Identity, name: str) -> str:
    status, body = _get(who, name)
    assert status == 200, f"{who.user}: GET {name}: {status} {body!r}"
    return body["status"]


def _ensure_running(env: UsersEnv, who: Identity, name: str) -> None:
    if _status(who, name) != "running":
        r = who.cli("start", name, timeout=_timeout(env))
        assert r.returncode == 0, f"{who.user}: shed start {name}: {r.stderr[-800:]!r}"
    assert _status(who, name) == "running"


def _ensure_stopped(env: UsersEnv, who: Identity, name: str) -> None:
    if _status(who, name) != "stopped":
        r = who.cli("stop", name, timeout=_timeout(env))
        assert r.returncode == 0, f"{who.user}: shed stop {name}: {r.stderr[-800:]!r}"
    assert _status(who, name) == "stopped"


def _names(sheds: list[dict]) -> set[str]:
    return {s["name"] for s in sheds}


# ----------------------------------------------------------------------------
# One shed per user, shared by the module (per backend)
# ----------------------------------------------------------------------------


@pytest.fixture(scope="module")
def alice_shed(users_env):
    name = _shed_fixture(users_env, users_env.alice, "itest-ua")
    yield name
    _delete(users_env, users_env.alice, name)


@pytest.fixture(scope="module")
def bob_slot(users_env):
    slot = _ShedSlot(users_env, users_env.bob, "itest-ub")
    yield slot
    slot.close()


@pytest.fixture
def bob_shed(bob_slot):
    return bob_slot.get()


@pytest.fixture
def enrolled(tmp_path):
    """Where a case enrols certificates (each with its private key) over
    `_bootstrap`. Removed when the case ends, pass or fail: pytest keeps its
    tmp_path directories, and these are live credentials until they expire."""
    d = tmp_path / "enrolled"
    d.mkdir(mode=0o700)
    yield d
    shutil.rmtree(d)


# ----------------------------------------------------------------------------
# Ownership (every backend, remote included)
# ----------------------------------------------------------------------------


def test_each_user_sees_only_their_own_sheds(users_env, alice_shed, bob_shed):
    env = users_env
    for who, mine, theirs in ((env.alice, alice_shed, bob_shed), (env.bob, bob_shed, alice_shed)):
        sheds = who.list_sheds()
        assert mine in _names(sheds), f"{who.user} does not see their own shed: {_names(sheds)}"
        assert theirs not in _names(sheds), f"{who.user} sees another user's shed {theirs}"
        assert {s.get("user") for s in sheds} == {who.user}, f"{who.user} sees sheds that are not theirs: {sheds}"

        status, body = who.api().request("GET", "/api/overview")
        assert status == 200, body
        names = {s["name"] for s in body["sheds"]}
        assert mine in names and theirs not in names, names
        assert {s.get("user") for s in body["sheds"]} == {who.user}, body["sheds"]

    # An admin lists every user's sheds, each with its user.
    by_name = {s["name"]: s for s in env.admin.list_sheds()}
    assert by_name[alice_shed].get("user") == ALICE
    assert by_name[bob_shed].get("user") == BOB


def test_another_users_shed_is_not_found_over_the_api(users_env, alice_shed):
    env = users_env
    bob = env.bob.api()
    for method, path, body in (
        ("GET", f"/api/sheds/{alice_shed}", None),
        ("POST", f"/api/sheds/{alice_shed}/stop", None),
        ("POST", f"/api/sheds/{alice_shed}/start", None),
        ("POST", f"/api/sheds/{alice_shed}/exec", {"argv": ["true"]}),
        ("GET", f"/api/sheds/{alice_shed}/sessions", None),
        ("DELETE", f"/api/sheds/{alice_shed}", None),
    ):
        status, resp = bob.request(method, path, body)
        assert (status, error_code(resp)) == (404, NOT_FOUND), (method, path, status, resp)
    # Untouched: still alice's, still running.
    _ensure_running(env, env.alice, alice_shed)
    status, got = _get(env.alice, alice_shed)
    assert status == 200 and got.get("user") == ALICE, got


def test_another_user_cannot_open_a_session_or_exec(users_env, alice_shed, bob_shed):
    env = users_env
    _ensure_running(env, env.alice, alice_shed)
    _ensure_running(env, env.bob, bob_shed)
    # Controls: each user's own session and `shed exec` run, through the same
    # shim, known_hosts and CLI entry, so the refusals below are about whose
    # shed it is.
    for who, mine in ((env.alice, alice_shed), (env.bob, bob_shed)):
        marker = _marker()
        r = who.ssh(mine, f"echo {marker}")
        assert r.returncode == 0 and marker in r.stdout, f"{who.user}'s own session failed: {r.stdout!r} {r.stderr!r}"
        marker = _marker()
        r = who.cli("exec", mine, "--", "echo", marker)
        assert r.returncode == 0 and marker in r.stdout, f"{who.user}'s own shed exec failed: {r.stderr[-800:]!r}"

    marker = _marker()
    r = env.bob.ssh(alice_shed, f"echo {marker}")
    assert r.returncode != 0 and marker not in r.stdout, f"bob ran a command in alice's shed: {r.stdout!r}"
    assert _session_not_found(alice_shed) in r.stderr, r.stderr

    # The CLI looks the shed up first (GET /api/sheds/<name>, a 404 for bob)
    # and says so in its own words.
    marker = _marker()
    r = env.bob.cli("exec", alice_shed, "--", "echo", marker)
    assert r.returncode != 0 and marker not in r.stdout, f"bob's shed exec ran in alice's shed: {r.stdout!r}"
    assert f'shed "{alice_shed}" not found' in (r.stdout + r.stderr), r.stdout + r.stderr


def test_a_taken_name_is_the_same_conflict_for_everyone(users_env, alice_shed):
    env = users_env
    body = {"name": alice_shed}
    if env.image:
        body["image"] = env.image
    seen = {}
    for who in (env.alice, env.bob, env.admin):
        status, resp = who.api().request("POST", "/api/sheds", body, timeout=120)
        assert (status, error_code(resp)) == (409, "SHED_ALREADY_EXISTS"), (who.user, status, resp)
        seen[who.user] = error_message(resp)
    assert len(set(seen.values())) == 1, f"the conflict differs by caller: {seen}"
    msg = seen[ALICE]
    assert ALICE not in msg and "alice" not in msg, f"the conflict names the owner: {msg!r}"


def test_an_admin_may_view_but_not_enter_another_users_shed(users_env, alice_shed):
    env = users_env
    _ensure_running(env, env.alice, alice_shed)
    admin = env.admin.api()
    status, got = admin.request("GET", f"/api/sheds/{alice_shed}")
    assert status == 200 and got.get("user") == ALICE, got

    status, out, code, err = admin.exec(alice_shed, ["echo", "admin-was-here"])
    assert status == 403 and error_code(err) == "FORBIDDEN", (status, err)
    assert ANOTHER_USER in error_message(err), err
    assert out == b""

    marker = _marker()
    r = env.admin.ssh(alice_shed, f"echo {marker}")
    assert r.returncode != 0 and marker not in r.stdout, f"the admin entered alice's shed: {r.stdout!r}"
    assert ANOTHER_USER in r.stderr, r.stderr

    marker = _marker()
    r = env.admin.cli("exec", alice_shed, "--", "echo", marker)
    assert r.returncode != 0 and marker not in r.stdout, f"the admin's shed exec ran: {r.stdout!r}"
    assert ANOTHER_USER in (r.stdout + r.stderr), r.stderr

    if env.backend != "remote":
        rc, first, err_text = env.admin.forward(alice_shed)
        assert not first.startswith(b"SSH-"), "the admin's forwarded port reached alice's shed"
        assert ANOTHER_USER in err_text, err_text


# ----------------------------------------------------------------------------
# The rest of the doors (local VM backends)
# ----------------------------------------------------------------------------


@pytest.mark.local_vm_only(LOCAL)
def test_another_user_cannot_sftp_scp_or_forward(users_env, alice_shed, bob_shed, tmp_path):
    env = users_env
    _ensure_running(env, env.alice, alice_shed)
    _ensure_running(env, env.bob, bob_shed)
    owners = ((env.alice, alice_shed), (env.bob, bob_shed))

    # Forwarded port (a direct-tcpip channel, as `ssh -L` opens): the guest's
    # sshd speaks first, so a forward that reaches it reads "SSH-". Controls:
    # each user's forward to their own shed, bob's included, so bob's refusal
    # below is about whose shed it is, not about bob.
    for who, shed in owners:
        rc, first, err = who.forward(shed)
        assert first.startswith(b"SSH-"), f"{who.user}'s own forward did not reach the guest: rc={rc} {first!r} {err!r}"
    rc, first, err = env.bob.forward(alice_shed)
    assert not first.startswith(b"SSH-"), "bob's forwarded port reached alice's shed"
    assert rc != 0 and FORWARD_NOT_FOUND in err, f"not the server's refusal: rc={rc} {err!r}"

    # sftp.
    for who, shed in owners:
        r = who.sftp(shed, "pwd\n")
        assert r.returncode == 0 and "Remote working directory" in r.stdout, \
            f"{who.user}'s own sftp failed: {r.stdout!r} {r.stderr!r}"
    r = env.bob.sftp(alice_shed, "pwd\n")
    assert r.returncode != 0 and "Remote working directory" not in r.stdout, f"bob's sftp reached alice's shed: {r.stdout!r}"
    assert _session_not_found(alice_shed) in r.stderr, f"not the server's refusal: {r.stderr!r}"

    # scp (the sftp protocol in current OpenSSH).
    token = uuid.uuid4().hex
    src = tmp_path / "payload.txt"
    src.write_text(token + "\n")
    mine, theirs = f"/tmp/itest-scp-alice-{token[:8]}", f"/tmp/itest-scp-bob-{token[:8]}"
    r = env.alice.scp_to(alice_shed, src, mine)
    assert r.returncode == 0, f"the owner's scp failed: {r.stderr!r}"
    r = env.bob.scp_to(bob_shed, src, theirs)
    assert r.returncode == 0, f"bob's scp into his own shed failed: {r.stderr!r}"
    r = env.bob.scp_to(alice_shed, src, theirs)
    assert r.returncode != 0, f"bob's scp into alice's shed succeeded: {r.stdout!r} {r.stderr!r}"
    assert _session_not_found(alice_shed) in r.stderr, f"not the server's refusal: {r.stderr!r}"
    r = env.alice.ssh(alice_shed, f"cat {mine}; test -e {theirs} && echo bob-file-present || echo bob-file-absent")
    assert r.returncode == 0, r.stderr
    assert token in r.stdout and "bob-file-absent" in r.stdout, r.stdout


@pytest.mark.local_vm_only(LOCAL)
def test_a_stopped_shed_is_not_woken_by_someone_who_may_not_enter_it(users_env, alice_shed):
    env = users_env
    _ensure_stopped(env, env.alice, alice_shed)
    try:
        marker = _marker()
        r = env.bob.ssh(alice_shed, f"echo {marker}")
        assert r.returncode != 0 and marker not in r.stdout, r.stdout
        assert _session_not_found(alice_shed) in r.stderr, r.stderr

        marker = _marker()
        r = env.admin.ssh(alice_shed, f"echo {marker}")
        assert r.returncode != 0 and marker not in r.stdout, r.stdout
        assert ANOTHER_USER in r.stderr, r.stderr

        status, resp = env.admin.api().request("POST", f"/api/sheds/{alice_shed}/start", timeout=120)
        assert (status, error_code(resp)) == (403, "FORBIDDEN"), (status, resp)

        # Nothing woke it.
        assert _status(env.alice, alice_shed) == "stopped"
    finally:
        _ensure_running(env, env.alice, alice_shed)


def _delete_snapshot(env: UsersEnv, snap: str) -> None:
    """Clean-up of a snapshot that cannot pass quietly: its owner (alice)
    deletes it; if that fails, the admin (an admin may delete any snapshot);
    then the admin must see it gone (404 SNAPSHOT_NOT_FOUND)."""
    problems = []
    for who in (env.alice, env.admin):
        try:
            status, resp = who.api().request("DELETE", f"/api/snapshots/{snap}", timeout=120)
        except Exception as e:  # noqa: BLE001 - recorded; the admin tries next
            problems.append(f"{who.user}: {type(e).__name__}: {e}")
            continue
        if status in (200, 204) or (status, error_code(resp)) == (404, "SNAPSHOT_NOT_FOUND"):
            break
        problems.append(f"{who.user}: {status} {resp!r}")
    status, resp = env.admin.api().request("GET", f"/api/snapshots/{snap}")
    if (status, error_code(resp)) != (404, "SNAPSHOT_NOT_FOUND"):
        raise AssertionError(
            f"CLEAN-UP FAILED: snapshot {snap!r} on server {env.server.name!r} is not gone "
            f"(GET: {status} {resp!r}); attempts: {'; '.join(problems) or 'none failed'}"
        )


@pytest.mark.local_vm_only("the remote backend has no snapshots")
def test_snapshots_belong_to_their_user(users_env, alice_shed):
    env = users_env
    snap = f"itest-us-{uuid.uuid4().hex[:8]}"
    as_bob, as_admin, as_alice = (f"itest-us{who}-{uuid.uuid4().hex[:6]}" for who in "bda")
    _ensure_stopped(env, env.alice, alice_shed)
    alice, bob, admin = env.alice.api(), env.bob.api(), env.admin.api()
    try:
        status, resp = alice.request("POST", "/api/snapshots", {"name": snap, "source_shed": alice_shed}, timeout=900)
        if status in (422, 501):
            pytest.skip(f"snapshots are not supported here: {resp}")
        assert status == 201, (status, resp)

        # Another user: not there.
        status, resp = bob.request("GET", "/api/snapshots")
        assert status == 200 and snap not in {s["name"] for s in resp.get("snapshots") or []}, resp
        for method, path in (("GET", f"/api/snapshots/{snap}"), ("DELETE", f"/api/snapshots/{snap}")):
            status, resp = bob.request(method, path)
            assert (status, error_code(resp)) == (404, "SNAPSHOT_NOT_FOUND"), (method, status, resp)
        status, resp = bob.request("POST", "/api/sheds", {"name": as_bob, "from_snapshot": snap}, timeout=300)
        assert (status, error_code(resp)) == (404, "SNAPSHOT_NOT_FOUND"), (status, resp)

        # The admin: sees it with its user, may not use it.
        status, resp = admin.request("GET", "/api/snapshots")
        assert status == 200, resp
        mine = [s for s in resp.get("snapshots") or [] if s["name"] == snap]
        assert mine and mine[0].get("user") == ALICE, resp
        status, resp = admin.request("POST", "/api/sheds", {"name": as_admin, "from_snapshot": snap}, timeout=300)
        assert (status, error_code(resp)) == (403, "FORBIDDEN"), (status, resp)

        # Control: its owner may use it, so the refusals above are about
        # whose snapshot it is (a server refusing every restore by a user who
        # is not an admin would fail here). A third VM: local backends only.
        status, resp = alice.request("POST", "/api/sheds", {"name": as_alice, "from_snapshot": snap}, timeout=900)
        assert status == 201, f"alice could not create a shed from her own snapshot: {status} {resp!r}"
        assert resp.get("user") == ALICE and resp.get("name") == as_alice, resp

        status, resp = alice.request("DELETE", f"/api/snapshots/{snap}", timeout=120)
        assert status in (200, 204), (status, resp)
    finally:
        # Every step runs whatever the others do; then all failures together.
        _run_all([
            lambda: _delete(env, env.alice, as_alice),
            lambda: env.server.ensure_deleted(as_bob),
            lambda: env.server.ensure_deleted(as_admin),
            lambda: _delete_snapshot(env, snap),
            lambda: _ensure_running(env, env.alice, alice_shed),
        ])


@pytest.mark.local_vm_only(LOCAL)
@pytest.mark.parametrize(
    "payload, field",
    [
        ({"local_dir": "/tmp"}, "local_dir"),
        ({"mounts": ["gh"]}, "mounts"),
        ({"image": "ghcr.io/charliek/shed-itest-no-such-image:v0.0.0"}, "image"),
    ],
    ids=["local-dir", "server-mounts", "image-not-an-alias"],
)
def test_host_resources_are_for_admins_only(users_env, payload, field):
    env = users_env
    name = f"itest-uh-{uuid.uuid4().hex[:8]}"
    try:
        status, resp = env.alice.api().request("POST", "/api/sheds", {"name": name, **payload}, timeout=120)
        assert (status, error_code(resp)) == (403, "FORBIDDEN"), (status, resp)
        assert error_message(resp).startswith("admin only: ") and field in error_message(resp), resp
        assert _get(env.alice, name)[0] == 404, "a refused create left a shed"
    finally:
        env.server.delete(name, ignore_missing=True)


@pytest.mark.local_vm_only(LOCAL)
def test_the_image_store_is_for_admins_only(users_env):
    env = users_env
    alice = env.alice.api()
    for method, path, body in (
        ("POST", "/api/images/pull", {"docker_ref": "ghcr.io/charliek/shed-itest-no-such-image:v0.0.0", "tag": "itest-nope"}),
        # dry_run: should the refusal ever regress, nothing is removed.
        ("POST", "/api/images/prune?dry_run=true", None),
        ("POST", "/api/system/prune?dry_run=true", None),
    ):
        status, resp = alice.request(method, path, body, timeout=120)
        assert (status, error_code(resp)) == (403, "FORBIDDEN"), (path, status, resp)


@pytest.mark.local_vm_only(LOCAL)
def test_a_script_key_never_holds_the_credentials_scope(users_env, alice_shed, bob_shed, enrolled):
    env = users_env
    # Control: alice's normal key may take it.
    env.alice.enrol("credentials", enrolled / "alice-credentials")
    with pytest.raises(AssertionError, match="script key cannot take the credentials scope"):
        env.alice_script.enrol("credentials", enrolled / "script-credentials")

    # The script key is alice: a control certificate, with her sheds only.
    ctl = env.alice_script.enrol("control", enrolled / "script-control")
    client = env.alice_script.pinned(ctl)
    status, body = client.request("GET", "/api/sheds")
    assert status == 200, body
    names = {s["name"] for s in body["sheds"]}
    assert alice_shed in names and bob_shed not in names, names
    # And has no way onto the bus (a control certificate does not reach it).
    status, body = client.request("GET", "/api/plugins/listeners")
    assert status == 403, (status, body)


@pytest.mark.local_vm_only(LOCAL)
def test_an_expired_key_opens_nothing(users_env, bob_shed, enrolled):
    env = users_env
    _ensure_running(env, env.bob, bob_shed)
    marker = _marker()
    r = env.bob.ssh(bob_shed, f"echo {marker}")
    assert r.returncode == 0 and marker in r.stdout, f"bob's own key failed: {r.stderr!r}"

    marker = _marker()
    r = env.bob_expired.ssh(bob_shed, f"echo {marker}")
    assert r.returncode != 0 and marker not in r.stdout, r.stdout
    assert "Permission denied" in r.stderr, r.stderr
    with pytest.raises(AssertionError, match="Permission denied"):
        env.bob_expired.enrol("control", enrolled / "expired")


# ----------------------------------------------------------------------------
# The credential bus (a synthetic listener per user)
# ----------------------------------------------------------------------------


def _round_trip(env: UsersEnv, sender: Identity, shed: str, ns: str, certs: dict, listeners: dict) -> str:
    """Send one bus request from `shed` (as its owner `sender`): it reaches
    `sender`'s listener; every other user's answer is refused; the owner's
    answer reaches the shed. All inside the guest's request lifetime (the
    budget above). Returns the request's token, for the check that no other
    listener ever got it (made by the caller, once every stream has shown it
    is still alive after the last round trip)."""
    token = uuid.uuid4().hex
    proc: Optional[subprocess.Popen] = None
    request_env = None
    try:
        for _attempt in range(4):
            started = time.monotonic()
            proc = sender.popen_cli("exec", shed, "--", *publish_argv(ns, {"itest_probe": token}))
            while request_env is None and time.monotonic() - started < DELIVERY_BUDGET:
                request_env = listeners[sender.user].wait_for(lambda e: payload_has(e, token), 0.5)
                if request_env is None and proc.poll() is not None:
                    break  # the request ended in the shed without reaching anyone
            if request_env is not None:
                break
            if proc.poll() is None:
                raise AssertionError(
                    f"a request from {shed} did not reach {sender.user}'s listener within {DELIVERY_BUDGET} s "
                    f"(listener error: {listeners[sender.user].error}); this case must answer inside the guest's "
                    f"{GUEST_REQUEST_LIFETIME} s request lifetime, so a server this slow fails it"
                )
            out, err = proc.communicate(timeout=90)
            # The shed's message channel may not be up yet right after a
            # create or start: the agent answers NO_CONNECTION at once.
            # Anything else is the failure.
            assert "NO_CONNECTION" in out + err, (
                f"a request from {shed} never reached {sender.user}'s listener "
                f"(listener error: {listeners[sender.user].error}); in the shed: {out!r} {err!r}"
            )
            time.sleep(5)
        assert request_env is not None, f"{shed}'s bus connection never came up"
        assert (request_env.get("shed") or {}).get("name") == shed, request_env

        # Every other user answers it at once, side by side; each is refused.
        forged: dict = {}

        def forge(user: str, cert) -> None:
            try:
                # Each user answers from their own machine (the one their
                # listener subscribed as, so it is their bound source).
                forged[user] = pinned_request(
                    sender.host, cert.https_port, f"/api/plugins/listeners/{ns}/respond", cert.pin, cert.pair,
                    method="POST", body=response_body(request_env, {"itest_answer": f"forged-by-{user}"}),
                    timeout=FORGERY_TIMEOUT, headers=machine_headers(user),
                )
            except Exception as e:  # noqa: BLE001 - reported by the assertion below
                forged[user] = e

        threads = [threading.Thread(target=forge, args=(u, c), daemon=True)
                   for u, c in certs.items() if u != sender.user]
        for t in threads:
            t.start()
        for t in threads:
            t.join(FORGERY_TIMEOUT + 2)
        for user in (u for u in certs if u != sender.user):
            got = forged.get(user)
            assert isinstance(got, tuple), f"{user}'s answer to {sender.user}'s request got no reply: {got!r}"
            status, body = got
            assert (status, error_code(body)) == (403, "FORBIDDEN"), \
                f"{user} answered {sender.user}'s request: {status} {body!r}"
            assert "does not match a pending request" in error_message(body), body

        elapsed = time.monotonic() - started
        assert elapsed < ANSWER_BY, (
            f"{elapsed:.1f} s into the exchange before the owner could answer; the guest gives up at "
            f"{GUEST_REQUEST_LIFETIME} s, so the answer could not be shown to arrive"
        )
        answer = uuid.uuid4().hex
        cert = certs[sender.user]
        status, body = pinned_request(
            sender.host, cert.https_port, f"/api/plugins/listeners/{ns}/respond", cert.pin, cert.pair,
            method="POST", body=response_body(request_env, {"itest_answer": answer}), timeout=ANSWER_TIMEOUT,
            headers=machine_headers(sender.user),
        )
        assert status == 204, f"{sender.user}'s own answer was refused: {status} {body!r}"
        out, err = proc.communicate(timeout=90)
        assert answer in out, f"the answer did not reach {shed}: {out!r} {err!r}"
        assert "forged-by-" not in out, out
        return token
    finally:
        if proc is not None and proc.poll() is None:
            proc.kill()
            proc.communicate()


@pytest.mark.local_vm_only("the remote backend's sheds carry no helper requests")
def test_bus_requests_reach_only_their_users_listener(users_env, alice_shed, bob_shed, enrolled):
    env = users_env
    _ensure_running(env, env.alice, alice_shed)
    _ensure_running(env, env.bob, bob_shed)
    ns = f"itest-users-{uuid.uuid4().hex[:8]}"
    certs = {who.user: who.enrol("credentials", enrolled / who.user) for who in env.people}
    # Each listener subscribes as its user's default synthetic machine, which
    # must be that user's credential source (the pin). Bindings outlive a
    # restart and test_source_pin.py moves them, so set them here, from
    # whatever state the server is in (a run killed mid-move included).
    for who in env.people:
        ensure_source(who.api(), FakeMachine(who.user, who.host, certs[who.user]),
                      f"itest-users-setup-{uuid.uuid4().hex[:8]}")
    listeners: dict[str, BusListener] = {}
    try:
        for who in env.people:
            c = certs[who.user]
            listener = BusListener(who.host, c.https_port, c.pin, c.pair, ns, name=who.user)
            status, body = listener.open()
            assert status == 200, f"{who.user} could not subscribe to {ns}: {status} {body!r}"
            listeners[who.user] = listener

        # The listings follow the user: alice sees her listener, the admin
        # sees all three.
        def listener_users(who: Identity) -> list:
            status, body = who.pinned(certs[who.user]).request("GET", "/api/plugins/listeners")
            assert status == 200, body
            return sorted(l.get("user") for l in body["listeners"] if l["namespace"] == ns)

        assert listener_users(env.alice) == [ALICE]
        assert listener_users(env.admin) == sorted([ADMIN, ALICE, BOB])

        # Each shed is routed to its user (its message channel may still be
        # connecting just after a start).
        deadline = time.monotonic() + 60
        while True:
            status, body = env.alice.pinned(certs[ALICE]).request("GET", "/api/plugins/sheds")
            assert status == 200, body
            routed = {s["name"]: s.get("user") for s in body["sheds"]}
            if alice_shed in routed or time.monotonic() > deadline:
                break
            time.sleep(2)
        assert routed.get(alice_shed) == ALICE and bob_shed not in routed, routed
        assert set(routed.values()) == {ALICE}, routed

        sent = {
            _round_trip(env, env.alice, alice_shed, ns, certs, listeners): ALICE,
            _round_trip(env, env.bob, bob_shed, ns, certs, listeners): BOB,
        }

        # Every listener stays under watch until it has shown, after both
        # round trips, that its stream still delivers (a keepalive or an
        # event read after that point). Anything misrouted to it earlier
        # has been read by then; a stream that broke or ended fails here
        # rather than passing as a listener that stayed quiet.
        done = time.monotonic()
        for user, listener in listeners.items():
            assert listener.alive_after(done, LIVENESS_TIMEOUT), (
                f"{user}'s stream showed no sign of life within {LIVENESS_TIMEOUT} s after the round trips "
                f"({listener.error or 'no keepalive and no event'}), so its silence proves nothing"
            )
        for token, sender in sent.items():
            for user, listener in listeners.items():
                got = listener.matching(lambda e: payload_has(e, token))
                if user == sender:
                    assert got, f"{user}'s own request is missing from their listener"
                else:
                    assert not got, f"{sender}'s request reached {user}'s listener: {got}"
    finally:
        for listener in listeners.values():
            listener.close()


# ----------------------------------------------------------------------------
# shed assign: the refusals a server with users can show without a restart
# (assigning a shed with no user: test_users_upgrade.py)
# ----------------------------------------------------------------------------


@pytest.mark.local_vm_only(LOCAL)
def test_assign_refuses_a_non_admin_an_unknown_user_and_an_assigned_shed(users_env, alice_shed):
    env = users_env
    path = f"/api/sheds/{alice_shed}/assign"
    status, resp = env.alice.api().request("POST", path, {"user": BOB})
    assert (status, error_code(resp)) == (403, "FORBIDDEN"), (status, resp)

    admin = env.admin.api()
    status, resp = admin.request("POST", path, {"user": BOB})
    assert (status, error_code(resp)) == (409, "SHED_ALREADY_ASSIGNED"), (status, resp)
    status, resp = admin.request("POST", path, {"user": "itest-nobody"})
    assert (status, error_code(resp)) == (400, "INVALID_REQUEST"), (status, resp)
    status, resp = admin.request("POST", f"/api/sheds/itest-no-such-{uuid.uuid4().hex[:6]}/assign", {"user": BOB})
    assert (status, error_code(resp)) == (404, NOT_FOUND), (status, resp)

    # The CLI says the server's refusal (internal/backend/assign.go): not a
    # usage error, a missing command or a connection failure.
    r = env.admin.cli("assign", alice_shed, BOB)
    out = r.stdout + r.stderr
    assert r.returncode != 0, f"shed assign moved alice's shed: {r.stdout!r}"
    assert "SHED_ALREADY_ASSIGNED" in out and f"already belongs to user {ALICE}" in out, out

    # Still alice's.
    status, got = _get(env.alice, alice_shed)
    assert status == 200 and got.get("user") == ALICE, got


# ----------------------------------------------------------------------------
# An admin stops and deletes another user's shed (every backend)
# ----------------------------------------------------------------------------


def test_an_admin_stops_and_deletes_another_users_shed(users_env, bob_slot):
    env = users_env
    # This case deletes bob's shed, so it takes it out of the module's hands:
    # its clean-up is this case's, and a later case gets a new one.
    name = bob_slot.take()
    try:
        _ensure_running(env, env.bob, name)
        r = env.admin.cli("stop", name, timeout=_timeout(env))
        assert r.returncode == 0, f"the admin could not stop bob's shed: {r.stderr[-800:]!r}"
        assert _status(env.bob, name) == "stopped"

        r = env.admin.cli("delete", name, "-f", timeout=_timeout(env))
        assert r.returncode == 0, f"the admin could not delete bob's shed: {r.stderr[-800:]!r}"
        assert name not in _names(env.bob.list_sheds())
        status, resp = env.admin.api().request("GET", f"/api/sheds/{name}")
        assert (status, error_code(resp)) == (404, NOT_FOUND), (status, resp)
    finally:
        _delete(env, env.bob, name)
