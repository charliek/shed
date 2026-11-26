"""Cloud sheds from the account service (plan 041 §6): the seven cases.

A remote server (`default_backend: remote`, `remote.provider: service`) gets
every machine from the account service it is enrolled with, which holds the
sizes, images, quotas and leases. The cases, each as people of the service
(`itest-alice`, `itest-bob`), the server's local admin and its registered
service client:

  1. create, list (the `cloud` object; `SIZE`, `LEASE` and the usage
     footer), stop, start (the lease unchanged), and, as the module's last
     case, delete;
  2. extend by a preset and by a custom value; past the cap refused
     (`LEASE_AT_CAP`, nothing changes); another person refused; the local
     admin extends alice's; a client's machine refused (the client itself
     by the server, the local admin by the service: `EXTEND_NOT_ALLOWED`);
  3. a lease ending: a running and a stopped shed, each gone within two
     sweeps of its own end (the service's, then the server's), and the
     server's log names `lease_ended`;
  4. quotas: alice's machine past the person limit and bob's past the
     organization's refused with `COMPUTE_LIMIT_REACHED` and their reasons;
     usage counts running and stopped;
  5. the lease banner (criterion 5), the policy's warning time widened so
     the shared shed is inside it: once for `shed console`, an
     interactive `shed exec` and a pty `ssh` login; once for a `shed exec`
     with no terminal but stderr on one; never for `--json`, a non-pty `ssh
     host cmd`, `rsync` or an exec whose stderr is not a terminal; binary
     stdout unchanged;
  6. `lease` on a client's create; above the client's `max_age` refused by
     the server, above the service's client cap by the service; a person's
     `lease` and the local admin's create refused;
  7. the service down (it refuses connections): a create answers
     `COMPUTE_UNAVAILABLE` at once (never delivered, or the firewall check's
     ingress read failing, which is `COMPUTE_UNAVAILABLE` too, not
     `FIREWALL_CHECK_FAILED`), existing sheds stay listed and their
     sessions work, and it recovers (the shed stops and starts through it
     again); a server restarted while the service is down starts degraded
     and recovers. A service that accepts and hangs is not this case: a
     create sent to it may have been acted on, so it is kept `starting
     (cloud_unreadable)` and resumed (docs/reference/remote-backend.md).

The cases share two machines: alice's shed (cases 1, 2, 4, 5, 7 and the
delete) and the client's (cases 2, 4 and 6, made with the client's own
lease). On `[remote]` they live for the module and are deleted at its end,
which prints how many machines the module made: four in a full run (the
two shared ones and case 3's pair with a ten-minute lease; every other
create a case makes is one the service refuses). On `[hermetic]` each
case's world makes its own.

Clean-up on `[remote]` never trusts `shed list` alone (a record can go
before its machine does): a listed shed's delete must succeed, and at the
module's end every machine it made must be gone, by its shed id, from the
service's own inventory (`ServiceControl.machines`), else the module fails
naming each. Every step is tried and every failure reported together. A
case that takes the service down brings it back in its `finally`, and its
fixture again, whatever happened. For a run against a real cloud the
cloud's own inventories before and after stay the authority.

Two parameters:

  - `[remote]`: the remote dev server (`make test-integration-remote`,
    `SHED_REMOTE_SERVER`), its local admin the developer's own entry (`dev`),
    against the service `SHED_REMOTE_SERVICE` names (`fixtures/compute.py`):
      * `ref`, the reference service (`make dev-compute-ref-up`, control
        directory under `SHED_COMPUTE_REF_DIR`, default
        `~/.shed/dev-remote/ref`): the cases list the itest people in its
        `users.json`, register the client, and steer it by `compute.json`
        (`policy.set`, `quota.fill`, `clock.advance`). Case 4 fills the
        quotas at the service's limits (4 per person, 10 per organization)
        and then raises the limits by what it filled, so later cases keep
        their room. Case 7 skips: the reference service keeps its state in
        its process (its hermetic run covers the case).
      * `service`, a local account service: the itest people (and
        `itest-admin`) must be on its list with the keys of
        `SHED_ITEST_USERS_DIR`, the client registered for the server, and
        compute allowed. Case 4 needs the test policy of 1 per person and 2
        per organization (set by `SHED_COMPUTE_CONTROL`'s hook, or already in
        force); cases 3, 5, 6's service cap and 7 need the hook.
    The service client is `SHED_COMPUTE_CLIENT` (default `nocturn`, the dev
    template's), its bundle issued with `bin/shed-server client issue` from
    the rendered config `SHED_REMOTE_CONFIG` (default
    `~/.shed/dev-remote/server.yaml`). Case 7's restart runs
    `SHED_REMOTE_RESTART` (a shell command that restarts the dev server).
    Each create against a real cloud is a billed machine; every shed a case
    makes is deleted at its end.
  - `[hermetic]`: a private server and reference service of the case's own
    (`fixtures/directory.py`'s `compute_world`), the machines shed-agents on
    this host (`make test-integration-compute`, Linux only, no cloud). All
    seven cases run there; the service is reached through a relay the case
    can take down and bring back (case 7), its state kept.

Every case skips, saying what to set, when what it needs is not there.
"""

from __future__ import annotations

import json
import os
import re
import shlex
import subprocess
import time
import uuid
from dataclasses import dataclass, field
from datetime import timedelta
from pathlib import Path
from typing import Callable, Optional
from urllib.parse import urlparse

import pytest
import yaml

from fixtures.compute import (
    BANNER,
    DELETED_BY_SERVICE,
    HookControl,
    NoControl,
    RefControl,
    ServiceControl,
    Unsupported,
    banner_count,
    binary_script,
    client_policy,
    expires_at,
    lease_of,
    list_itest_people,
    parse_time,
    refusal_text,
    run_with_tty,
    service_kind,
    unlisted_people,
)
from fixtures.directory import (
    COMPUTE_CLIENT,
    HOST,
    compute_skip_reason,
    compute_world,
)
from fixtures.identity import (
    ALICE,
    BOB,
    ensure_keys,
    users_environment,
)
from fixtures.server import full_server_entry
from fixtures.tlsclient import Pinned, error_code

# A create on a real cloud takes about a minute; a stop or start under one.
CREATE_TIMEOUT = 300
LIFECYCLE_TIMEOUT = 300
# The server's periodic pass (internal/remote: DefaultSweepInterval), and the
# service's lease sweep (plan 041 §3.6: every 60 s).
SERVER_SWEEP_S = 60
SERVICE_SWEEP_S = 60
# What a bound in seconds allows for polling and a slow pass.
MARGIN_S = 30
# How long the server keeps a caller's cloud usage (plan 041 §3.4).
USAGE_CACHE_S = 30
REPO = Path(__file__).resolve().parents[2]


# ----------------------------------------------------------------------------
# The environment of one case
# ----------------------------------------------------------------------------


class EntryAdmin:
    """The remote dev server's local admin: the developer's own CLI entry."""

    def __init__(self, server, entry: dict) -> None:
        self.server, self.entry = server, entry
        self.user = str(entry.get("user") or "dev")

    def cli(self, *args: str, timeout: float = 120) -> subprocess.CompletedProcess:
        return self.server.cli(*args, timeout=int(timeout))

    def list_sheds(self) -> list[dict]:
        return self.server.list_sheds()

    def api(self) -> Pinned:
        url = urlparse(self.entry["api_url"])
        return Pinned(url.hostname, url.port, self.entry["tls_cert_fingerprint"],
                      self.entry["client_cert_file"], self.entry["client_key_file"])


@dataclass
class Shared:
    """The machines the cases share: alice's shed (cases 1, 2, 4, 5, 7 and
    the final delete) and the client's (cases 2, 4 and 6). On [remote] they
    live for the module (a real cloud's machines are counted: the run keeps
    inside the live budget) and are deleted at its end; on [hermetic] each
    case's world makes its own."""

    person: Optional[str] = None
    client: Optional[str] = None
    # Machines this module had made (successful creates).
    created: int = 0
    # The shed id of every machine made, by shed name, to name one that
    # remains at the service.
    ids: dict = field(default_factory=dict)
    # What the service counted before the module made anything (usage:
    # machines, running, stopped), the organization's and alice's, for the
    # quota case; and the machines that case added that are never deleted
    # (the reference service's quota.fill, all running).
    base_org: tuple = (0, 0, 0)
    base_person: tuple = (0, 0, 0)
    filled: int = 0


@dataclass
class Env:
    """Who acts in a case, and how the service and the server are reached."""

    where: str  # "remote" or "hermetic"
    service: str  # "ref" or "service"
    alice: object
    bob: object
    admin: object
    client: Pinned
    client_name: str
    client_policy: dict
    control: ServiceControl
    server_log: Callable[[], str]
    restart: Optional[Callable[[], None]]
    image: Optional[str]
    # Deletes one shed, as the local admin, verifying it is gone.
    cleanup: Callable[[str], None]
    shared: Shared
    made: list[str] = field(default_factory=list)

    def name(self, tag: str) -> str:
        n = f"itest-cc-{tag}-{uuid.uuid4().hex[:6]}"
        self.made.append(n)
        return n

    def client_shed_name(self, tag: str) -> str:
        n = f"{self.client_policy['name_prefix']}cc-{tag}-{uuid.uuid4().hex[:6]}"
        self.made.append(n)
        return n

    def person_shed(self) -> dict:
        """Alice's shared shed, running (made on first use)."""
        if self.shared.person is None:
            name = f"itest-cc-shared-{uuid.uuid4().hex[:6]}"
            self.shared.person = name
            create_ok(self, self.alice, name)
        return wait_status(self.alice, self.shared.person, "running")

    def stop_service(self) -> None:
        """Take the service down. Call it inside the `try` whose `finally`
        calls `start_service`: a stop that fails half-way (a hook that
        times out, an interrupt) is still undone."""
        self.control.stop()

    def start_service(self) -> None:
        """Bring the service back if a stop was attempted."""
        if self.control.stopped:
            self.control.start()

    def client_shed(self) -> dict:
        """The client's shared shed, running, with the lease
        `client_lease_h` asked for at its create (made on first use)."""
        if self.shared.client is None:
            name = f"{self.client_policy['name_prefix']}cc-shared-{uuid.uuid4().hex[:6]}"
            self.shared.client = name
            status, body = client_create(self, name, f"{client_lease_h(self.client_policy)}h")
            assert status in (200, 201), f"the client's create of {name}: {status} {body}"
            self.shared.created += 1
            self.shared.ids[name] = (body or {}).get("id", "?")
        return client_wait_running(self, self.shared.client)

    def ask(self, fn: Callable, *args, **kwargs):
        """A control step; the case skips when this run cannot take it."""
        try:
            return fn(*args, **kwargs)
        except Unsupported as e:
            pytest.skip(str(e))


def client_lease_h(policy: dict) -> int:
    """The lease the shared client shed asks for: 3 hours, or an hour less
    than the client's max_age when that is shorter (at least one)."""
    max_age_h = policy["max_age_s"] // 3600
    return 3 if not max_age_h or max_age_h >= 4 else max(1, max_age_h - 1)


@pytest.fixture(params=["remote", "hermetic"])
def env(request):
    if request.param == "hermetic":
        why = compute_skip_reason()
        if why:
            pytest.skip(why)
        with compute_world() as w:
            e = _hermetic_env(w)
            try:
                yield e
            finally:
                # Whatever happened in the case, the service is up again
                # (the world deletes its sheds through it).
                e.start_service()
        return
    base = request.getfixturevalue("remote_base")
    e = Env(**{**base, "made": []})
    # The shared machines are the module's, deleted when it ends.
    try:
        yield e
    finally:
        problems = []
        try:
            e.start_service()
        except Exception as err:  # noqa: BLE001 - reported with the rest
            problems.append(f"bringing the service back up: {err!r}; start it by hand before deleting anything")
        for n in reversed(e.made):
            try:
                e.cleanup(n)
            except Exception as err:  # noqa: BLE001 - every shed is tried; all are reported
                problems.append(f"{n}: {err!r}")
        if problems:
            raise AssertionError("CLEAN-UP FAILED:\n" + "\n".join(problems))


def _hermetic_env(w) -> Env:
    people = {}
    for user in (ALICE, BOB):
        k = w.key(user)
        w.set_user(user, [k])
        people[user] = k
    w.publish()
    w.refresh_ok()
    alice, bob = (w.identity(u, people[u]) for u in (ALICE, BOB))
    for who in (alice, bob):
        who.add_server()
    b = w.client_bundle()
    client = Pinned(HOST, b["https_port"], b["tls_cert_fingerprint"], b["cert_file"], b["key_file"])

    def logs() -> str:
        return "".join(p.read_text(encoding="utf-8", errors="replace") for p in sorted(w.logs.glob("server-*.log")))

    def restart() -> None:
        problems = w.server.stop()
        assert not problems, problems
        # A degraded start waits two minutes for the service's list first.
        w.server.start(timeout=200)

    return Env(where="hermetic", service="ref", alice=alice, bob=bob, admin=w.admin, client=client,
               client_name=COMPUTE_CLIENT, client_policy=client_policy(w.config(), COMPUTE_CLIENT),
               control=w.ref_control(), server_log=logs, restart=restart, image=None,
               # The world deletes every shed when it closes.
               cleanup=lambda name: None, shared=Shared())  # a fresh world holds no machine


@pytest.fixture(scope="module")
def remote_base(remote_server, request):
    """The [remote] run's people, client, controls and shared machines,
    shared by its cases. At the end the shared machines are deleted and the
    number of machines the module made is printed."""
    kind, why = service_kind()
    if kind is None:
        pytest.skip(why)
    # The JSON list leaves out the auth mode, the API URL, the TLS pin and the
    # client certificate: they come from the raw config entry.
    entry = full_server_entry(remote_server.name)
    if not entry or entry.get("auth_mode") != "mtls" or not entry.get("client_cert_file"):
        pytest.skip(f"~/.shed/config.yaml has no mtls entry {remote_server.name!r} with a client certificate")
    config_path = Path(os.environ.get("SHED_REMOTE_CONFIG", "") or Path.home() / ".shed" / "dev-remote" / "server.yaml")
    if not config_path.is_file():
        pytest.skip(f"no rendered dev server config at {config_path} (SHED_REMOTE_CONFIG): make dev-server-up-remote")
    cfg = yaml.safe_load(config_path.read_text(encoding="utf-8")) or {}
    if (cfg.get("remote") or {}).get("provider") != "service":
        pytest.skip(f"{config_path}: remote.provider is not service; the compute cases need the account service")
    client = os.environ.get("SHED_COMPUTE_CLIENT", "").strip() or "nocturn"
    try:
        policy = client_policy(cfg, client)
    except KeyError as e:
        pytest.skip(str(e))
    server_bin = Path(os.environ.get("SHED_SERVER_BIN", "") or REPO / "bin" / "shed-server")
    if not os.access(server_bin, os.X_OK):
        pytest.skip(f"no shed-server at {server_bin} to issue the client's bundle (make build-server)")
    enrolled = os.environ.get("SHED_COMPUTE_REF_SERVER", "").strip() or str(cfg.get("name") or "")

    if kind == "ref":
        ref_dir = Path(os.environ.get("SHED_COMPUTE_REF_DIR", "") or Path.home() / ".shed" / "dev-remote" / "ref")
        control_dir = ref_dir / "control"
        if not (control_dir / "compute.json").is_file():
            pytest.skip(f"no reference service control directory at {control_dir}: make dev-compute-ref-up "
                        f"(SHED_COMPUTE_REF_DIR)")
        log = ref_dir / "ref.log"
        control: ServiceControl = RefControl(control_dir, enrolled,
                                             lambda: log.read_text(encoding="utf-8", errors="replace")
                                             if log.exists() else "",
                                             inventory=ref_dir / "inventory.json")
        list_itest_people(control_dir / "users.json")
        control.register_client(client)
    else:
        control = HookControl.from_env() or NoControl()
        missing = unlisted_people(remote_server)
        if missing:
            pytest.skip(f"the account service does not list {', '.join(missing)}: list them with the keys in "
                        f"{ensure_keys()} (`cd tests/integration && uv run python -m fixtures.identity "
                        f"users-yaml` prints them), then `shed -s {remote_server.name} users refresh`")

    log_path = remote_server.log_path

    def server_log() -> str:
        return log_path.read_text(encoding="utf-8", errors="replace") if log_path and log_path.exists() else ""

    restart_cmd = os.environ.get("SHED_REMOTE_RESTART", "").strip()

    def restart() -> None:
        r = subprocess.run(["bash", "-c", restart_cmd], capture_output=True, text=True, timeout=600,
                           stdin=subprocess.DEVNULL)
        assert r.returncode == 0, f"SHED_REMOTE_RESTART: exit {r.returncode}: {(r.stdout + r.stderr)[-1500:]}"
        deadline = time.monotonic() + 300
        while not remote_server.available():
            assert time.monotonic() < deadline, "the dev server does not answer 300 s after its restart"
            time.sleep(5)

    with users_environment("remote", remote_server, reconfigure=False, image=remote_server.image,
                           source="directory") as uenv:
        bundle = _issue_bundle(server_bin, config_path, client, uenv.keys_dir / "compute-client")
        host = urlparse(entry["api_url"]).hostname
        pinned = Pinned(host, bundle["https_port"], bundle["tls_cert_fingerprint"], bundle["cert_file"],
                        bundle["key_file"])
        admin = EntryAdmin(remote_server, entry)
        # The service's inventory before anything is made: read once the
        # server's 30 s usage cache cannot hold an older count.
        time.sleep(USAGE_CACHE_S + 1)
        shared = Shared(base_org=_counts(usage(admin)["org"]), base_person=_counts(usage(uenv.alice)["person"]))

        def cleanup(name: str) -> None:
            delete_confirmed(admin, name, shared.ids.get(name, "?"))

        try:
            yield dict(where="remote", service=kind, alice=uenv.alice, bob=uenv.bob,
                       admin=admin, client=pinned, client_name=client,
                       client_policy=policy, control=control, server_log=server_log,
                       restart=restart if restart_cmd else None, image=remote_server.image,
                       cleanup=cleanup, shared=shared)
        finally:
            # Every step is tried whatever the one before did; every failure
            # is reported at the end.
            problems = []
            for n in (shared.person, shared.client):
                if n:
                    try:
                        cleanup(n)
                    except Exception as e:  # noqa: BLE001
                        problems.append(f"{n}: {e!r}")
            reporter = request.config.pluginmanager.get_plugin("terminalreporter")
            try:
                confirm_gone(control, shared.ids)
            except Unsupported as e:
                line = (f"test_remote_compute: NOT CONFIRMED that the machines made are gone ({e}); "
                        f"check the service's own inventory for: "
                        f"{', '.join(f'{n} (id {i})' for n, i in shared.ids.items()) or 'none'}")
                if reporter is not None:
                    reporter.write_line(line)
                else:
                    print(line)
            except Exception as e:  # noqa: BLE001
                problems.append(f"confirming the machines are gone: {e}")
            line = (f"test_remote_compute [remote, {kind}]: {shared.created} machine(s) made by this module "
                    f"on {remote_server.name}")
            if reporter is not None:
                reporter.write_line(line)
            else:
                print(line)
            if problems:
                raise AssertionError("CLEAN-UP FAILED:\n" + "\n".join(problems))


def _issue_bundle(server_bin: Path, config: Path, client: str, out: Path) -> dict:
    r = subprocess.run([str(server_bin), "client", "issue", client, "--config", str(config)],
                       capture_output=True, text=True, timeout=60, stdin=subprocess.DEVNULL)
    assert r.returncode == 0, f"shed-server client issue {client}: exit {r.returncode}: {r.stdout}{r.stderr}"
    b = json.loads(r.stdout)
    out.mkdir(parents=True, mode=0o700, exist_ok=True)
    for name, key in (("client.pem", "client_cert"), ("client.key", "client_key")):
        p = out / name
        fd = os.open(p, os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o600)
        try:
            os.write(fd, b[key].encode())
        finally:
            os.close(fd)
    b["cert_file"], b["key_file"] = str(out / "client.pem"), str(out / "client.key")
    return b


# ----------------------------------------------------------------------------
# Helpers
# ----------------------------------------------------------------------------


def _out(r: subprocess.CompletedProcess) -> str:
    return (r.stdout or "") + (r.stderr or "")


def create(env: Env, who, name: str) -> subprocess.CompletedProcess:
    args = ["create", name, "--no-provision"]
    if env.image:
        args += ["--image", env.image]
    return who.cli(*args, timeout=CREATE_TIMEOUT)


def create_ok(env: Env, who, name: str) -> dict:
    r = create(env, who, name)
    assert r.returncode == 0, f"{who.user}: shed create {name}: exit {r.returncode}: {_out(r)[-1500:]}"
    env.shared.created += 1
    shed = wait_status(who, name, "running")
    env.shared.ids[name] = shed.get("id", "?")
    return shed


# How long a delete may take to finish, and the service's inventory to show
# every machine made gone.
DELETE_TIMEOUT = 600
INVENTORY_TIMEOUT = 300


def delete_confirmed(admin, name: str, shed_id: str) -> None:
    """Delete `name` as the local admin: when it is listed its delete must
    succeed, else this fails naming the shed and its id. A shed not listed is
    left to `confirm_gone`, which asks the service for the machine by its id:
    a record can be gone while its machine is not."""
    if name not in {s.get("name") for s in admin.list_sheds()}:
        return
    r = admin.cli("delete", name, "-f", timeout=DELETE_TIMEOUT)
    assert r.returncode == 0, (
        f"CLEAN-UP FAILED: deleting shed {name} (id {shed_id}) failed (exit {r.returncode}); its machine may "
        f"still exist at the account service (and be billed): {_out(r)[-800:]}")


def confirm_gone(control: ServiceControl, ids: dict, timeout: float = INVENTORY_TIMEOUT) -> None:
    """Every machine the module made (`ids`: shed name to shed id) is gone
    from the service's own inventory (`control.machines()`), polled until
    `timeout`; else fail naming each that remains. The shed server's records
    are not asked: a record can go before its machine does."""
    want = {i: n for n, i in ids.items() if i and i != "?"}
    deadline = time.monotonic() + timeout
    while True:
        left = sorted(set(want) & control.machines())
        if not left:
            return
        if time.monotonic() >= deadline:
            raise AssertionError(
                "CLEAN-UP FAILED: the account service still holds "
                + ", ".join(f"{want[i]} (id {i})" for i in left)
                + f" {timeout:.0f} s after the clean-up; delete each at the service (they may be billed)")
        time.sleep(5)


def find(who, name: str) -> Optional[dict]:
    for s in who.list_sheds():
        if s.get("name") == name:
            return s
    return None


def wait_status(who, name: str, status: str, timeout: float = LIFECYCLE_TIMEOUT) -> dict:
    deadline = time.monotonic() + timeout
    last = None
    while True:
        last = find(who, name)
        if last is not None and last.get("status") == status:
            return last
        if time.monotonic() >= deadline:
            raise AssertionError(f"{name} is not {status} after {timeout} s: {json.dumps(last)[:800]}")
        time.sleep(2)


def wait_gone(who, names: list[str], timeout: float) -> None:
    deadline = time.monotonic() + timeout
    while True:
        listed = [s["name"] for s in who.list_sheds() if s.get("name") in names]
        if not listed:
            return
        if time.monotonic() >= deadline:
            raise AssertionError(f"{listed} still listed after {timeout} s")
        time.sleep(3)


def cli_ok(who, *args: str, timeout: float = LIFECYCLE_TIMEOUT) -> subprocess.CompletedProcess:
    r = who.cli(*args, timeout=timeout)
    assert r.returncode == 0, f"{who.user}: shed {' '.join(args)}: exit {r.returncode}: {_out(r)[-1500:]}"
    return r


def refused(r: subprocess.CompletedProcess, code: str, reason: str = "") -> None:
    want = refusal_text(code, reason) if reason else code
    assert r.returncode != 0, f"not refused: {_out(r)[-800:]}"
    assert want in _out(r), f"want {want!r}: {_out(r)[-1500:]}"


def usage(who) -> dict:
    status, body = who.api().request("GET", "/api/cloud/usage")
    assert status == 200, f"{who.user}: GET /api/cloud/usage: {status} {body}"
    return body


def wait_usage(who, want: Callable[[dict], bool], what: str) -> dict:
    """`who`'s usage once `want` holds: the server keeps each caller's for
    30 s (USAGE_CACHE_S), so a count is read again until it is current."""
    deadline = time.monotonic() + USAGE_CACHE_S + 15
    while True:
        u = usage(who)
        if want(u):
            return u
        if time.monotonic() >= deadline:
            raise AssertionError(f"{who.user}'s usage is not {what} after {USAGE_CACHE_S + 15} s: {u}")
        time.sleep(3)


def client_create(env: Env, name: str, lease: Optional[str] = None) -> tuple[int, Optional[dict]]:
    body: dict = {"name": name}
    pol = env.client_policy
    if pol["images"]:
        body["image"] = pol["images"][0]
    if pol["network"]:
        body["network"] = pol["network"]
    if lease is not None:
        body["lease"] = lease
    return env.client.request("POST", "/api/sheds", body, timeout=CREATE_TIMEOUT)


def client_wait_running(env: Env, name: str) -> dict:
    deadline = time.monotonic() + CREATE_TIMEOUT
    while True:
        status, body = env.client.request("GET", f"/api/sheds/{name}")
        if status == 200 and (body or {}).get("status") == "running":
            return body
        if time.monotonic() >= deadline:
            raise AssertionError(f"the client's {name} is not running after {CREATE_TIMEOUT} s: {status} {body}")
        time.sleep(2)


def listed_names(who) -> set[str]:
    return {s.get("name") for s in who.list_sheds()}


# ----------------------------------------------------------------------------
# 1. Create, list, stop, start (the delete is the module's last case)
# ----------------------------------------------------------------------------


def test_create_list_stop_start(env):
    alice = env.alice
    shed = env.person_shed()
    name = shed["name"]
    assert shed.get("backend") == "remote", shed
    cloud = shed.get("cloud")
    assert isinstance(cloud, dict), shed
    size = cloud.get("size") or {}
    assert size.get("name") and size.get("cpus", 0) > 0 and size.get("memory_mb", 0) > 0, cloud
    if env.service == "ref":
        assert (size["name"], size["cpus"], size["memory_mb"]) == ("standard", 2, 8192), cloud
    assert cloud.get("image"), cloud
    lease = lease_of(shed)
    exp, cap = parse_time(lease["expires_at"]), parse_time(lease["max_expires_at"])
    created = parse_time(cloud["created_at"])
    assert created < exp <= cap, lease
    assert lease.get("extendable") is True, lease

    # The table: SIZE and LEASE, and the usage footer.
    r = cli_ok(alice, "list")
    lines = r.stdout.splitlines()
    header = next((ln for ln in lines if ln.split()[:1] == ["NAME"]), "")
    assert "SIZE" in header.split() and "LEASE" in header.split(), r.stdout
    row = next((ln for ln in lines if ln.split()[:1] == [name]), "")
    assert size["name"] in row.split(), r.stdout
    assert re.search(r"\b(\d+d \d+h|\d+h \d+m|\d+m)\b", row), f"no time left in {row!r}"
    assert any(ln.startswith("Cloud: ") and " machines (" in ln and "organization " in ln for ln in lines), r.stdout

    # -vv: the Cloud block.
    r = cli_ok(alice, "list", "-vv")
    assert "Cloud:" in r.stdout and "Deleted at:" in r.stdout and "Cap:" in r.stdout, r.stdout

    cli_ok(alice, "stop", name)
    stopped = wait_status(alice, name, "stopped")
    assert lease_of(stopped)["expires_at"] == lease["expires_at"], (lease, stopped)
    assert lease_of(stopped)["max_expires_at"] == lease["max_expires_at"], (lease, stopped)

    cli_ok(alice, "start", name)
    started = wait_status(alice, name, "running")
    assert lease_of(started)["expires_at"] == lease["expires_at"], (lease, started)
    assert started["id"] == shed["id"], (shed, started)
    assert started["cloud"].get("started_at"), started["cloud"]

    # A cloud shed belongs to the person its machine was made for: assign is
    # 501, even from the local admin.
    status, body = env.admin.api().request("POST", f"/api/sheds/{name}/assign", {"user": BOB})
    assert status == 501 and error_code(body) == "FEATURE_UNSUPPORTED", (status, body)


# ----------------------------------------------------------------------------
# 2. Extend
# ----------------------------------------------------------------------------


def test_extend(env):
    alice, bob, admin = env.alice, env.bob, env.admin
    shed = env.person_shed()
    name = shed["name"]
    lease = lease_of(shed)
    presets = lease.get("presets_h") or []
    assert presets, lease
    exp0, cap = expires_at(shed), parse_time(lease["max_expires_at"])

    # A preset.
    r = cli_ok(alice, "extend", name, "--hours", str(presets[0]))
    assert f"Extended shed {name}" in r.stdout and "the cap is" in r.stdout, r.stdout
    exp1 = expires_at(find(alice, name))
    assert exp1 - exp0 == timedelta(hours=presets[0]), (exp0, exp1)

    # A custom value.
    cli_ok(alice, "extend", name, "--by", "5h")
    exp2 = expires_at(find(alice, name))
    assert exp2 - exp1 == timedelta(hours=5), (exp1, exp2)

    # Past the cap: refused, nothing changes.
    over = int((cap - exp2).total_seconds() // 3600) + 1
    r = alice.cli("extend", name, "--hours", str(over), timeout=120)
    refused(r, "LEASE_AT_CAP")
    assert expires_at(find(alice, name)) == exp2

    # Another person: refused (the shed is not theirs to see or extend).
    status, body = bob.api().request("POST", f"/api/sheds/{name}/extend", {"hours": 1})
    assert status in (403, 404), f"bob's extend of alice's shed: {status} {body}"
    assert expires_at(find(alice, name)) == exp2

    # The local admin extends alice's.
    cli_ok(admin, "extend", name, "--hours", "1")
    assert expires_at(find(alice, name)) - exp2 == timedelta(hours=1)

    # A client's machine: the client itself is refused by the server, the
    # local admin by the service.
    cshed = env.client_shed()
    cname = cshed["name"]
    assert lease_of(cshed).get("extendable") is False, cshed["cloud"]
    status, body = env.client.request("POST", f"/api/sheds/{cname}/extend", {"hours": 1})
    assert status == 403 and error_code(body) == "FORBIDDEN", (status, body)
    r = admin.cli("extend", cname, "--hours", "1", timeout=120)
    refused(r, "EXTEND_NOT_ALLOWED")
    assert lease_of(find(admin, cname))["expires_at"] == lease_of(cshed)["expires_at"]


# ----------------------------------------------------------------------------
# 3. A lease ending
# ----------------------------------------------------------------------------


def test_lease_end(env):
    """Two machines with a ten-minute lease, one running and one stopped,
    each gone within two sweeps of its own end: the service deletes it at its
    next sweep after the end (at most a minute), and the server drops the
    record at its next pass after that (at most a minute), MARGIN_S allowed
    for polling. Against the reference service the end is the moment its
    clock is moved past it, which its sweep then deletes at once, so only the
    server's pass is allowed. The server's log names lease_ended."""
    alice = env.alice
    lease_s = 600
    # Room for the pair under the service's limits (a test policy of 1 per
    # person and 2 per organization has none beside the shared machines),
    # raised with the lease and put back once both are made.
    u = usage(alice)
    known_person = env.shared.base_person[0] + (1 if env.shared.person else 0)
    known_org = (env.shared.base_org[0] + env.shared.filled + (1 if env.shared.person else 0)
                 + (1 if env.shared.client else 0))
    need_p, need_o = known_person + 2, known_org + 2
    raise_limits = u["person"]["max"] < need_p or u["org"]["max"] < need_o
    values = {"initial_lease_s": lease_s}
    if raise_limits:
        values.update(person_max=max(u["person"]["max"], need_p), org_max=max(u["org"]["max"], need_o))
    env.ask(env.control.policy, **values)
    try:
        running, stopped = env.name("end-run"), env.name("end-stop")
        create_ok(env, alice, running)
        create_ok(env, alice, stopped)
    finally:
        env.control.policy_reset("initial_lease_s")
        if raise_limits:
            env.control.restore_limits(u["person"]["max"], u["org"]["max"])
    cli_ok(alice, "stop", stopped)
    wait_status(alice, stopped, "stopped")
    ends = {}
    for n in (running, stopped):
        shed = find(alice, n)
        left = (expires_at(shed) - parse_time(shed["cloud"]["created_at"])).total_seconds()
        assert left == lease_s, f"{n}: a lease of {left} s, not the test policy's {lease_s} s"
        ends[n] = expires_at(shed).timestamp()

    if env.service == "ref":
        # The reference service's clock past the lease: its sweep deletes
        # both at once; the server drops them at its next pass.
        env.control.clock_advance(lease_s + 1)
        moved = time.time()
        bound = {n: moved + SERVER_SWEEP_S + MARGIN_S for n in ends}
    else:
        bound = {n: end + SERVICE_SWEEP_S + SERVER_SWEEP_S + MARGIN_S for n, end in ends.items()}
    # Each observation is timed when the list has returned: a machine first
    # seen gone then went no later than that, and must be within its own
    # bound; one still listed past its bound fails at once.
    gone_at = {}
    while len(gone_at) < len(ends):
        listed = {s.get("name") for s in alice.list_sheds()}
        seen = time.time()
        for n in ends:
            if n in gone_at:
                continue
            if n not in listed:
                gone_at[n] = seen
                assert seen <= bound[n], (f"{n} was first seen gone {seen - ends[n]:.0f} s after its lease's end, "
                                          f"past its bound ({bound[n] - ends[n]:.0f} s)")
            else:
                assert seen <= bound[n], (f"{n} still listed {seen - ends[n]:.0f} s after its lease's end, past "
                                          f"its bound ({bound[n] - ends[n]:.0f} s: two sweeps of "
                                          f"{SERVICE_SWEEP_S} s and {MARGIN_S} s)")
        time.sleep(3)
    log = env.server_log().splitlines()
    want = DELETED_BY_SERVICE.format(cause="lease_ended")
    for n in (running, stopped):
        assert any(f"shed {n} (" in ln and want in ln for ln in log), (
            f"the server's log has no line for {n} ending {want!r}")


def _now_rfc3339() -> str:
    return time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime())


# ----------------------------------------------------------------------------
# 4. Quotas
# ----------------------------------------------------------------------------


def _counts(c: dict) -> tuple[int, int, int]:
    return c["machines"], c["running"], c["stopped"]


def test_quotas(env):
    """Usage counts running and stopped (alice's shared shed stopped and
    started again); a person past their limit and the organization past its
    own are refused with their reasons. No machine is made beyond the shared
    two: the refused creates make none."""
    alice, bob = env.alice, env.bob
    shed = env.person_shed()
    env.client_shed()
    set_here = False
    if env.service == "service":
        u = usage(alice)
        if (u["person"]["max"], u["org"]["max"]) != (1, 2):
            env.ask(env.control.policy, person_max=1, org_max=2)
            set_here = True
    filled_person = filled_org = 0
    u0 = None
    try:
        # Before the module made anything the service held base_org machines
        # (base_person of them alice's), and quota.fill's: since then the
        # shared two. Exactly those counts, polled past the usage cache; a
        # shortage (a machine the service does not count) is a failure.
        # Both shared machines run: alice's counts one running machine more
        # for her and the organization, the client's one more for the
        # organization (quota.fill's are all running).
        bp, bo, filled = env.shared.base_person, env.shared.base_org, env.shared.filled
        want_person = (bp[0] + 1, bp[1] + 1, bp[2])
        want_org = (bo[0] + filled + 2, bo[1] + filled + 2, bo[2])
        if env.service == "service":
            if bo[0]:
                pytest.skip(f"the organization held {bo[0]} other machine(s) before the module; the "
                            f"test policy of 1 per person and 2 per organization needs it empty")
            wait_usage(alice, lambda u: (u["person"]["max"], u["org"]["max"]) == (1, 2),
                       "under the test policy (1 per person, 2 per organization)")
        u0 = wait_usage(alice, lambda u: (_counts(u["person"]), _counts(u["org"])) == (want_person, want_org),
                        f"person {want_person} and organization {want_org} (machines, running, stopped: the "
                        f"shared two running)")
        p0, o0 = _counts(u0["person"]), _counts(u0["org"])

        # Running and stopped.
        cli_ok(alice, "stop", shed["name"])
        wait_status(alice, shed["name"], "stopped")
        want_p = (p0[0], p0[1] - 1, p0[2] + 1)
        want_o = (o0[0], o0[1] - 1, o0[2] + 1)
        wait_usage(alice, lambda u: (_counts(u["person"]), _counts(u["org"])) == (want_p, want_o),
                   f"person {want_p} and organization {want_o} (machines, running, stopped)")

        if env.service == "ref":
            # Alice at her limit (the rest are machines of other servers, all
            # running), then the organization at its own.
            filled_person = u0["person"]["max"] - p0[0]
            env.control.quota_fill_person(ALICE)
            env.shared.filled += filled_person
            u2 = wait_usage(alice, lambda u: u["person"]["machines"] == u["person"]["max"], "at the person limit")
        over = env.name("q-person")
        refused(create(env, alice, over), "COMPUTE_LIMIT_REACHED", "person")
        assert over not in listed_names(alice)

        if env.service == "ref":
            filled_org = u2["org"]["max"] - u2["org"]["machines"]
            env.control.quota_fill_org()
            env.shared.filled += filled_org
            u3 = wait_usage(bob, lambda u: u["org"]["machines"] == u["org"]["max"], "at the organization limit")
            assert u3["person"]["machines"] < u3["person"]["max"], u3
            if env.where == "hermetic":
                # The plan's numbers: alice's fifth, the organization's eleventh.
                assert (u2["person"]["max"], u3["org"]["max"]) == (4, 10), (u2, u3)
        over = env.name("q-org")
        refused(create(env, bob, over), "COMPUTE_LIMIT_REACHED", "organization")
        assert over not in listed_names(bob)
    finally:
        if set_here:
            env.control.policy_reset("person_max", "org_max")
        if env.where == "remote" and (filled_person or filled_org) and u0 is not None:
            # The fills cannot be undone: raise the limits by them, so the
            # rest of the run has the room it had.
            env.control.policy(person_max=u0["person"]["max"] + filled_person,
                               org_max=u0["org"]["max"] + filled_person + filled_org)
        # The shared shed runs again for the cases after this one.
        if (find(alice, shed["name"]) or {}).get("status") == "stopped":
            alice.cli("start", shed["name"], timeout=LIFECYCLE_TIMEOUT)


# ----------------------------------------------------------------------------
# 5. The lease banner
# ----------------------------------------------------------------------------


def _tty_env(who) -> dict:
    e = who.env()
    e["TERM"] = "dumb"
    return e


def _ssh(who, shed: str, *opts: str) -> list[str]:
    return [str(who.shim), *opts, "-p", str(who.ssh_port), "-o", f"UserKnownHostsFile={who.known_hosts}",
            "-o", "StrictHostKeyChecking=yes", "-o", "ConnectTimeout=10", f"{shed}@{who.host}"]


def _wait_warned(who, name: str, warn_s: int) -> dict:
    """The shed once its record carries the policy's new warning time (the
    server reads the catalog at every pass) and its lease is inside it."""
    deadline = time.monotonic() + SERVER_SWEEP_S + 45
    while True:
        shed = find(who, name) or {}
        lease = ((shed.get("cloud") or {}).get("lease") or {})
        if lease.get("warn_s") == warn_s:
            return shed
        if time.monotonic() >= deadline:
            raise AssertionError(f"{name}'s lease does not carry warn_s {warn_s} after a pass: {lease}")
        time.sleep(3)


def test_lease_banner(env):
    """The banner warns of a lease ending within the policy's warning time:
    the case widens that time to a week (past any lease of the shared shed)
    rather than making a machine with a short lease."""
    alice = env.alice
    name = env.person_shed()["name"]
    warn_s = 7 * 24 * 3600
    env.ask(env.control.policy, warn_s=warn_s)
    try:
        shed = _wait_warned(alice, name, warn_s)
        left = (expires_at(shed) - parse_time(_now_rfc3339())).total_seconds()
        assert 0 < left < warn_s, f"{name} has {left:.0f} s left, not inside the warning time"
        _banner_counts(alice, name)
    finally:
        env.control.policy_reset("warn_s")


def _banner_counts(alice, name: str) -> None:
    shed_cmd = ["shed", "-s", alice.entry]
    e = _tty_env(alice)
    exit_line = b"exit\n"

    def once(what: str, run) -> None:
        assert run.code == 0, f"{what}: {run}"
        assert banner_count(run.text()) == 1, f"{what}: want the banner once: {run}"

    def never(what: str, run) -> None:
        assert run.code == 0, f"{what}: {run}"
        assert banner_count(run.text()) == 0, f"{what}: want no banner: {run}"

    # Once: a session with a terminal (the server's banner).
    once("shed console", run_with_tty([*shed_cmd, "console", name], e, input=exit_line, resend_every=5))
    r = run_with_tty([*shed_cmd, "exec", name, "--", "echo", "hi"], e)
    once("an interactive shed exec", r)
    assert b"hi" in r.tty, r
    once("a pty ssh login", run_with_tty(_ssh(alice, name, "-tt"), e, input=exit_line, resend_every=5))

    # Once: an exec with no terminal, stderr on one (the CLI's banner).
    r = run_with_tty([*shed_cmd, "exec", name, "--", "echo", "hi"], e, on_tty=("stderr",))
    once("a shed exec with only stderr on a terminal", r)
    assert r.stdout == b"hi\n", r
    assert BANNER.encode() in r.tty, r

    # Binary stdout unchanged, the banner on stderr only.
    r = run_with_tty([*shed_cmd, "exec", name, "--", "bash", "-c", binary_script()], e, on_tty=("stderr",))
    once("a binary shed exec", r)
    assert r.stdout == bytes(range(256)), f"binary stdout changed: {r.stdout[:80]!r}... ({len(r.stdout)} bytes)"

    # Never.
    r = run_with_tty(["shed", "--json", "-s", alice.entry, "exec", name, "--", "echo", "hi"], e)
    never("shed --json exec", r)
    r = run_with_tty(_ssh(alice, name, "-T") + ["echo hi"], e, on_tty=("stderr",))
    never("a non-pty ssh command", r)
    assert r.stdout == b"hi\n", r
    r = run_with_tty([*shed_cmd, "exec", name, "--", "echo", "hi"], e, on_tty=("stdin", "stdout"))
    never("a shed exec whose stderr is not a terminal", r)
    assert b"hi" in r.tty, r
    r = run_with_tty([*shed_cmd, "exec", name, "--", "echo", "hi"], e, on_tty=())
    never("a shed exec with no terminal at all", r)
    assert r.stdout == b"hi\n", r
    # rsync is one of criterion 5's channels: required, never skipped.
    assert _which("rsync"), "rsync is not installed here; the banner case needs it (criterion 5)"
    dest = Path(alice.home) / f"rsync-{uuid.uuid4().hex[:6]}"
    dest.mkdir()
    rsh = " ".join(shlex.quote(a) for a in _ssh(alice, "x")[:-1])
    r = run_with_tty(["rsync", "-e", rsh, f"{name}@{alice.host}:/etc/hostname", str(dest) + "/"], e,
                     on_tty=("stderr",))
    never("rsync", r)
    assert (dest / "hostname").is_file(), r


def _which(cmd: str) -> bool:
    from shutil import which

    return which(cmd) is not None


# ----------------------------------------------------------------------------
# 6. A client's lease; the local admin's create
# ----------------------------------------------------------------------------


def test_client_lease_and_local_admin_create(env):
    pol = env.client_policy
    max_age = pol["max_age_s"]

    # The client's own lease (the shared client shed asked for it): the end
    # and the cap both at create + lease, and no extend.
    shed = env.client_shed()
    lease_h = client_lease_h(pol)
    lease = lease_of(shed)
    created = parse_time(shed["cloud"]["created_at"])
    assert parse_time(lease["expires_at"]) - created == timedelta(hours=lease_h), lease
    assert lease["max_expires_at"] == lease["expires_at"], lease
    assert lease.get("extendable") is False, lease

    # Above its max_age: refused by the server, nothing made.
    if max_age:
        name = env.client_shed_name("long")
        status, body = client_create(env, name, lease=f"{max_age // 3600 + 1}h")
        assert status == 403 and error_code(body) == "POLICY_VIOLATION", (status, body)
        assert name not in listed_names(env.admin)

    # A person's create carries no lease; the local admin creates nothing.
    status, body = env.alice.api().request("POST", "/api/sheds", {"name": env.name("person-lease"),
                                                                   "lease": "1h"})
    assert status == 422 and error_code(body) == "FEATURE_UNSUPPORTED", (status, body)
    name = env.name("admin")
    r = create(env, env.admin, name)
    refused(r, "COMPUTE_POLICY_REFUSED", "person")
    assert name not in listed_names(env.admin)


def test_client_lease_above_the_service_cap(env):
    # A cap under the lease the client asks for, which its max_age allows:
    # the service refuses it at the create's judgement, before any machine.
    env.ask(env.control.policy, client_max_lease_s=1800)
    try:
        name = env.client_shed_name("cap")
        status, body = client_create(env, name, lease="1h")
    finally:
        env.control.policy_reset("client_max_lease_s")
    assert status == 422 and error_code(body) == "COMPUTE_POLICY_REFUSED", (status, body)
    assert ((body or {}).get("error") or {}).get("reason") == "lease", body
    assert name not in listed_names(env.admin)


# ----------------------------------------------------------------------------
# 7. The service down
# ----------------------------------------------------------------------------


def test_service_down(env):
    """The service refuses connections: a create is answered 503
    `COMPUTE_UNAVAILABLE` at once with no shed left (the request never
    reached the service, or the firewall check could not read the ingress),
    the existing shed stays listed and running and its session works, and
    once the service is back the shed stops and starts through it again."""
    alice = env.alice
    # Before any shed is made, so a run that cannot stop the service skips
    # without one.
    env.ask(env.control.check_stop)
    name = env.person_shed()["name"]
    try:
        env.stop_service()
        r = create(env, alice, env.name("down-new"))
        refused(r, "COMPUTE_UNAVAILABLE")
        shed = find(alice, name)
        assert shed is not None and shed.get("status") == "running", shed
        r = alice.ssh(name, "echo still-here")
        assert r.returncode == 0 and "still-here" in r.stdout, r
    finally:
        env.start_service()
    _recovered(env, alice, name)


def test_server_restarted_while_the_service_is_down(env):
    alice = env.alice
    if env.restart is None:
        pytest.skip("set SHED_REMOTE_RESTART to a command that restarts the dev server "
                    "(e.g. `make -C <repo> dev-server-restart-remote`)")
    env.ask(env.control.check_stop)
    name = env.person_shed()["name"]
    try:
        env.stop_service()
        env.restart()
        shed = find(alice, name)
        assert shed is not None, f"{name} is not listed after a degraded start"
        r = alice.cli("stop", name, timeout=120)
        refused(r, "COMPUTE_UNAVAILABLE")
        r = alice.ssh(name, "echo still-here")
        assert r.returncode == 0 and "still-here" in r.stdout, r
    finally:
        env.start_service()
    _recovered(env, alice, name)


# The refusals a lifecycle call may meet while the server catches up with a
# service that is back: its record of the machines (degraded) and its
# ingress check (refusing starts until a pass reads the ingress again).
_CATCHING_UP = ("COMPUTE_UNAVAILABLE", "FIREWALL_CHECK_FAILED")


def _retry(who, deadline: float, *args: str) -> None:
    while True:
        r = who.cli(*args, timeout=LIFECYCLE_TIMEOUT)
        if r.returncode == 0:
            return
        assert any(c in _out(r) for c in _CATCHING_UP), f"shed {' '.join(args)} after the service came back: {_out(r)[-1500:]}"
        assert time.monotonic() < deadline, f"shed {' '.join(args)} still refused: {_out(r)[-800:]}"
        time.sleep(5)


def _recovered(env: Env, who, existing: str) -> None:
    """The service is back: within two of the server's passes the shed made
    before stops and starts through it again (a real cloud makes no new
    machine for this); on [hermetic] a create succeeds too."""
    deadline = time.monotonic() + 2 * SERVER_SWEEP_S + 60
    _retry(who, deadline, "stop", existing)
    wait_status(who, existing, "stopped")
    _retry(who, deadline, "start", existing)
    wait_status(who, existing, "running")
    if env.where == "hermetic":
        name = env.name("back")
        cli_ok(who, "create", name, "--no-provision", timeout=CREATE_TIMEOUT)
        wait_status(who, name, "running")


# ----------------------------------------------------------------------------
# Last: the delete
# ----------------------------------------------------------------------------


def test_delete(env):
    """Alice deletes her shed (the shared one, last): it is gone from her
    list, and its machine from the service's own inventory, by its id."""
    alice = env.alice
    shed = env.person_shed()
    name = shed["name"]
    cli_ok(alice, "delete", name, "-f")
    wait_gone(alice, [name], LIFECYCLE_TIMEOUT)
    env.shared.person = None
    try:
        confirm_gone(env.control, {name: shed["id"]}, timeout=60)
    except Unsupported:
        pass  # the module's clean-up says it could not confirm
