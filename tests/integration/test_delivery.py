"""Provisioning by delivery, live (plan 039 §6).

A shed-server asks the shed user's credential source (their bound machine)
for a bundle of files and environment values at create and at `shed
provision`, and writes it into the shed in two stages around the clone:
stage 1 (home files and the env file), the clone, stage 2 (project files),
the install hook. These cases drive one dev server with the itest user list
(`users_env`, `fixtures/identity.py`) and play alice's machine with the
contract's reference source, `bin/test/shed-provision-ref` (`make
provision-ref`), run as a subprocess in a throwaway HOME with a throwaway
credentials certificate, on loopback to the server under test
(`fixtures/delivery.py`). Before any case the module makes that machine
alice's source through the routes a person has (`ensure_source`), and at the
end puts her default machine back.

What it shows, live (the plan's case numbers):

  1. a create with a canned bundle: files with their modes and content, the
     env values byte for byte (quotes, `$`, inner spaces) in a new command
     and a new SSH session, the record, and the order (the stage 1 file is
     older than the clone, the stage 2 file is inside it and newer);
  2. the source not connected (create), silent past the deadline (create),
     disconnecting mid-request (provision), and no source bound (create):
     the reasons, the warning text or its absence, the step states;
  3. `shed provision` finishing an offline create, then again (nothing
     changes but timestamps, nothing is removed); and a project directory
     that already exists as something else (`project_dir_exists`, left
     alone);
  4. the contract's refuse fixtures, one per rule (all of them with
     `SHED_DELIVERY_REFUSE_ALL=1`): refused with the rule, nothing written;
  5. answer binding: a second connection and another certificate
     (`not_stream_owner`), a second answer (`already_answered`), a pin move
     while a request is pending (it fails at once; the old machine's answer
     is refused);
  6. the sentinel sweep: no delivered value (raw, or its base64) in any API
     response or CLI output the module kept, the server's log since the
     module began, the audit log or the state directories, after a create,
     provisions, a failed write and a refused answer;
  7. a script key's shed and a service client's shed get no request, and
     neither may provision; another user may not provision alice's shed (bob:
     404, an admin: 403 and the record withheld); a second provision while
     one runs is 409;
  8. `shed start` with an expired item: one warning, no request;
  9. a published image (where the image gate is unset): `image_too_old`, no
     request, and no second agent process in the shed;
 10. activity: two users, a deleted shed, its name reused, paging.

**Gates.**

  - `users_env` (conftest): [vz] the Mac dev server (restarted with the
    itest user list for the module); [fc] and [remote] a dev server started
    with it (`SHED_ITEST_USERS`). Without one every case skips saying what
    to set.
  - `SHED_IMAGE_HAS_DELIVERY` (a comma list of `vz`, `fc`, `remote`): the
    backends whose images were rebuilt from this branch. The cases that need
    the new agent (1 to 8 but the no-source, script-key and service-client
    ones) skip without it; case 9 runs only where it is unset, or with
    `SHED_DELIVERY_OLD_IMAGE` naming an image alias that still boots a
    published image.
  - [remote]: each shed is a billed cloud VM, so only the core cases run
    there (case 1, cases 2 and 3 with an offline source, and the sweep,
    which makes no VM): two VMs one after the other, each deleted. The rest
    carry `local_vm_only`.
  - The service client's shed is [fc] only (its bundle is issued on the FC
    dev host, as in test_service_client.py: `SHED_FC_DEV_BIN_PATH`,
    `SHED_FC_DEV_CONFIG`). The script-key case skips under the directory
    source. The no-source case skips when itest-admin already has a bound
    machine on this server (bindings outlive a restart, and test_users.py
    binds one; the Mac dev server keeps the itest users' bindings only for a
    module).

**What it needs.** `bin/` first on PATH (`make build`: the CLI with `shed
provision`) and the reference source (`make provision-ref`, or
`SHED_PROVISION_REF`). The sweep reads the dev server's files: its log
(the suite's usual log variables), its audit log and state directories
(`fixtures/delivery.py: server_files` lists the defaults and the
variables that move them).

**Runtime, per local backend:** about 12 to 15 minutes and 8 sheds (at most
three at once): alice's main shed (case 1, then reused by cases 2, 4, 5, 6,
7, 8 through provisions), one each for the offline and silent creates, the
admin's no-source shed, the script key's, the service client's ([fc]), and
two for activity; plus about 55 refusals (case 4, 3 to 4 s each) and three
waits of the server's 30 s delivery timeout. [remote]: two VMs, about 8
minutes.

Run only these cases:

    cd tests/integration && uv run pytest -v -rs test_delivery.py
"""

from __future__ import annotations

import hashlib
import json
import os
import shutil
import subprocess
import threading
import time
import uuid
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any, Callable, Optional
from urllib.parse import urlencode, urlparse

import pytest

import test_smoke
from fixtures import delivery as dl
from fixtures import devcontrol
from fixtures.busclient import FakeMachine, bound_id, describe_sources, ensure_known, ensure_source, sources
from fixtures.identity import ALICE, BOB, Identity, UsersEnv
from fixtures.server import resolve_server_entry
from fixtures.tlsclient import Pinned, error_code, pinned_request
from test_users import _delete, _timeout

# The remote run is kept to the core cases (each shed is a billed VM).
CORE_ONLY = "the remote run is kept to the core delivery cases (case 1, an offline create and its provision)"
NO_SCRIPT_KEY_IN_A_LIST = "a list never marks a key script (the server never reads script from it)"

# A public repository (no credential needed: the clone proves nothing about
# delivery, it places the project directory), and one that cannot be
# reached (example.test never resolves): its clone fails at once.
REPO = "https://github.com/octocat/Hello-World.git"
REPO_DIR = "/home/shed/Hello-World"
MISSING_REPO = "https://git.example.test/nobody/itest-missing.git"
MISSING_DIR = "/home/shed/itest-missing"

# The server's delivery.timeout default: how long a request waits for a
# connected source's answer.
DELIVERY_TIMEOUT = 30
# A request that fails at once (a stream that ended, a pin that moved) ends
# well inside this.
PROMPT = DELIVERY_TIMEOUT - 8

VALUE_VAR = "ITEST_DELIVERY_VALUE"
PLAIN_VAR = "ITEST_DELIVERY_PLAIN"
STAGE1_FILE = ".itest-delivery/id_itest"


def _value(sentinel: str) -> str:
    """An env value that a shell would mangle: quotes, `$`, a command
    substitution, backslashes and inner runs of spaces. It must arrive byte
    for byte."""
    return f"it's \"quoted\" $HOME and `true` $(true) a\\nb \\t  two  spaces ;|& {sentinel} end"


def standard_items(sentinel: str) -> list[dict]:
    """The canned bundle: three stage 1 home files (modes 0600, 0644, 0400,
    two under a parent the writer creates 0700), two env values, and a stage
    2 project file. Every content and value carries the sentinel; no name,
    path or fingerprint does (those are in the record by design)."""
    return [
        dl.file_item("itest-key", STAGE1_FILE,
                     f"-----BEGIN ITEST KEY-----\n{sentinel}\n-----END ITEST KEY-----\n".encode(), provider="ssh-key"),
        dl.file_item("itest-key.pub", ".itest-delivery/id_itest.pub",
                     b"ssh-ed25519 AAAAitestnotakey itest@example.test\n", mode="0644", provider="ssh-key"),
        dl.file_item("itest-token", ".config/itest-delivery/token", f"token-{sentinel}\n".encode(),
                     mode="0400", provider="github"),
        dl.env_item("itest-value", VALUE_VAR, _value(sentinel)),
        dl.env_item("itest-plain", PLAIN_VAR, f"plain-{sentinel}"),
        dl.file_item("itest-project", ".itest-delivery-project", f"PROJECT_TOKEN={sentinel}\n".encode(),
                     stage=2, base="project"),
    ]


def _require_image(backend: str) -> None:
    if backend not in dl.delivery_image_backends():
        pytest.skip(f"SHED_IMAGE_HAS_DELIVERY does not list {backend}: its image's agent cannot take items "
                    f"(rebuild it from this branch and list it)")


_TOOLS_CHECKED: list[bool] = []


def _require_tools() -> None:
    """The reference source and a CLI with `shed provision`; checked once a
    server has been found (a run with none skips without asking)."""
    if _TOOLS_CHECKED:
        return
    binary = dl.ref_source_binary()
    if not binary.is_file() or not os.access(binary, os.X_OK):
        pytest.fail(f"{binary} is missing: run `make provision-ref` (or set SHED_PROVISION_REF)")
    try:
        r = subprocess.run(["shed", "provision", "--help"], capture_output=True, text=True, timeout=30)
    except FileNotFoundError:
        pytest.fail("no `shed` on PATH; put the repository's bin/ first (make build)")
    if r.returncode != 0 or "--status" not in r.stdout:
        pytest.fail("the `shed` first on PATH has no `shed provision`: put the repository's bin/ first (make build)")
    _TOOLS_CHECKED.append(True)


# Every case here acts only on a parallel dev server: the guard runs before
# anything is created or enrolled (a test replaces it in test_smoke_units.py).
_assert_dev_target = devcontrol.assert_dev_target


def _ns() -> str:
    return f"itest-dlv-{uuid.uuid4().hex[:8]}"


def _name(tag: str) -> str:
    return f"itest-dlv-{tag}-{uuid.uuid4().hex[:6]}"


# ----------------------------------------------------------------------------
# The module's machine
# ----------------------------------------------------------------------------


@dataclass
class DeliveryEnv:
    """One run's delivery set-up against one dev server: alice's delivery
    machine (`machine`, her source for the module) and her default machine,
    the credentials certificate both present, what the reference source
    connects as (`ident`), the module's sentinel, and the server's files.
    `kept` is every API response body and create/provision output the module
    saw (case 6 sweeps them); `delivered` the sheds a request was made for."""

    users: UsersEnv
    sentinel: str
    forms: list[str]
    machine: FakeMachine
    default_machine: FakeMachine
    cert: Any
    ident: dl.SourceIdentity
    work: Path
    files: dl.ServerFiles
    log_offset: Any
    kept: list[str] = field(default_factory=list)
    delivered: set[str] = field(default_factory=set)
    _n: int = 0

    @property
    def backend(self) -> str:
        return self.users.backend

    @property
    def alice(self) -> Identity:
        return self.users.alice

    def keep(self, *texts: Any) -> None:
        for t in texts:
            if t is None:
                continue
            self.kept.append(t if isinstance(t, str) else json.dumps(t))

    def items(self) -> list[dict]:
        return standard_items(self.sentinel)

    def start(self, items: Optional[list[dict]] = None, *, problems=(), refuse: Optional[Path] = None,
              label: str = "source", wait: bool = True, **kw) -> dl.RefSource:
        """Start the reference source as alice's delivery machine: answering
        with `items` (an empty answer when None), or the refuse fixture
        `refuse`, or misbehaving as `kw` says (silent, delay, twice,
        second_connection, answer_cert). Waits for its stream unless
        `wait=False`."""
        self._n += 1
        lbl = f"{self._n:03d}-{label}"
        items_file = None
        if refuse is None:
            p = self.work / f"{lbl}.items.json"
            p.write_text(json.dumps(dl.canned(items or [], problems)), encoding="utf-8")
            p.chmod(0o600)
            items_file = str(p)
        argv = dl.refsource_argv(dl.ref_source_binary(), self.ident, items_file=items_file,
                                 refuse_fixture=str(refuse) if refuse else None, **kw)
        src = dl.RefSource(argv, self.work, label=lbl)
        if wait:
            try:
                src.wait_stream()
            except BaseException:
                src.stop()
                raise
        return src


@pytest.fixture(scope="module")
def delivery_env(users_env, tmp_path_factory):
    env = users_env
    # users_env reaches dev servers only, and enrolment checks again; this
    # says so before anything else is done.
    _assert_dev_target(env.server.name)
    _require_tools()
    work = tmp_path_factory.mktemp("delivery")
    work.chmod(0o700)
    certs = work / "certs"
    certs.mkdir(mode=0o700)
    ssh_host = getattr(env.server, "ssh_host", None)
    api = env.alice.api()
    cert = env.alice.enrol("credentials", certs / "alice")
    machine = FakeMachine(ALICE, env.alice.host, cert, tag="delivery")
    default = FakeMachine(ALICE, env.alice.host, cert)
    try:
        ensure_source(api, machine, _ns())
        with dl.loopback_endpoint(cert.https_port, cert.pin, ssh_host) as url:
            ca = dl.write_ca(work / "server-ca.pem", env.alice.host, cert.https_port, cert.pin)
            ident = dl.SourceIdentity(url=url, ca_file=str(ca), cert_file=str(cert.cert_path),
                                      key_file=str(cert.key_path), machine_id=machine.id, machine_name=machine.name)
            sentinel = dl.new_sentinel()
            d = DeliveryEnv(users=env, sentinel=sentinel, forms=dl.sentinel_forms(sentinel), machine=machine,
                            default_machine=default, cert=cert, ident=ident, work=work,
                            files=dl.server_files(env.backend, ssh_host=ssh_host),
                            log_offset=env.server._log_offset())
            yield d
    finally:
        try:
            # alice's source back on her default machine, as test_source_pin.py
            # leaves it (test_users.py sets its own and does not rely on it).
            ensure_source(env.alice.api(), default, _ns())
        finally:
            shutil.rmtree(certs, ignore_errors=True)


def _wait_offline(d: DeliveryEnv, timeout: float = 20) -> None:
    """Wait until the server no longer counts the delivery machine as
    connected (a stopped source's stream has ended there)."""
    deadline = time.monotonic() + timeout
    while True:
        view = sources(d.alice.api())
        got = [m for m in view.get("machines") or [] if m.get("id") == d.machine.id]
        if got and not got[0].get("connected"):
            return
        if time.monotonic() > deadline:
            raise AssertionError(f"the delivery machine is still connected after {timeout:.0f} s: {describe_sources(view)}")
        time.sleep(1)


# ----------------------------------------------------------------------------
# The CLI and the API as a user
# ----------------------------------------------------------------------------


def _create(d: DeliveryEnv, who: Identity, name: str, *, repo: Optional[str] = None,
            image: Optional[str] = None) -> subprocess.CompletedProcess:
    args = ["create", name, "--no-sync"]
    image = image or d.users.image
    if image:
        args += ["--image", image]
    if repo:
        args += ["--repo", repo]
    r = who.cli(*args, timeout=_timeout(d.users))
    d.keep(r.stdout, r.stderr)
    assert r.returncode == 0, f"{who.user}: shed create {name} failed (exit {r.returncode}): {r.stdout[-800:]!r} {r.stderr[-800:]!r}"
    return r


def _doc(d: DeliveryEnv, who: Identity, name: str) -> dict:
    status, body = who.api().request("GET", f"/api/sheds/{name}")
    d.keep(body)
    assert status == 200 and isinstance(body, dict), f"{who.user}: GET {name}: {status} {body!r}"
    return body


def _record(d: DeliveryEnv, who: Identity, name: str) -> dict:
    rec = _doc(d, who, name).get("delivery")
    assert isinstance(rec, dict), f"the shed document of {name} carries no delivery record"
    return rec


def _provision(d: DeliveryEnv, who: Identity, name: str, timeout: float = DELIVERY_TIMEOUT + 120) -> tuple[int, dict]:
    """`POST /api/sheds/{name}/provision` (no event stream: the shed
    document comes back when the run is done)."""
    status, body = who.api().request("POST", f"/api/sheds/{name}/provision", timeout=timeout)
    d.keep(body)
    return status, body


def _provision_ok(d: DeliveryEnv, who: Identity, name: str, **kw) -> dict:
    status, body = _provision(d, who, name, **kw)
    assert status == 200 and isinstance(body, dict), f"{who.user}: provision {name}: {status} {body!r}"
    return body["delivery"]


def _provision_cli(d: DeliveryEnv, who: Identity, name: str) -> subprocess.CompletedProcess:
    r = who.cli("provision", name, timeout=DELIVERY_TIMEOUT + 180)
    d.keep(r.stdout, r.stderr)
    assert r.returncode == 0, f"{who.user}: shed provision {name} exited {r.returncode}: {r.stdout[-800:]!r} {r.stderr[-800:]!r}"
    return r


def _exec(who: Identity, name: str, *argv: str, timeout: float = 90) -> subprocess.CompletedProcess:
    """`shed exec` (argv, never a value: its output is not kept)."""
    r = who.cli("exec", name, "--", *argv, timeout=timeout)
    assert r.returncode == 0, f"{who.user}: shed exec {name} -- {argv[:3]} exited {r.returncode}: {r.stderr[-600:]!r}"
    return r


def _bash(who: Identity, name: str, script: str, *args: str) -> subprocess.CompletedProcess:
    return _exec(who, name, "bash", "-c", script, "_", *args)


def _reap(proc: Optional[subprocess.Popen]) -> None:
    """Kill and reap a background CLI a case did not get to finish."""
    if proc is not None and proc.poll() is None:
        proc.kill()
        proc.communicate()


class _Job:
    """A call run on a thread: `result()` waits for it and returns or raises
    what it did."""

    def __init__(self, fn: Callable[[], Any]) -> None:
        self._value: Any = None
        self._exc: Optional[BaseException] = None
        self._t = threading.Thread(target=self._run, args=(fn,), daemon=True)
        self.started = time.monotonic()
        self.ended: Optional[float] = None
        self._t.start()

    def _run(self, fn) -> None:
        try:
            self._value = fn()
        except BaseException as e:  # noqa: BLE001 - re-raised by result()
            self._exc = e
        finally:
            self.ended = time.monotonic()

    def result(self, timeout: float) -> Any:
        self._t.join(timeout)
        if self._t.is_alive():
            raise AssertionError(f"the call did not end within {timeout:.0f} s")
        if self._exc is not None:
            raise self._exc
        return self._value


# ----------------------------------------------------------------------------
# What a delivery left in the shed
# ----------------------------------------------------------------------------


def _check_bundle(d: DeliveryEnv, who: Identity, name: str, items: list[dict], project_dir: Optional[str]) -> None:
    """Every file item at its place with its mode, owner and content byte for
    byte; the writer's new parent 0700; the env file 0600 holding exactly
    the env items; and each env value, byte for byte, in a new command and a
    new SSH session."""
    files = [i for i in items if i["kind"] == "file" and (i["base"] == "home" or project_dir)]
    env_items = [i for i in items if i["kind"] == "env"]
    paths = [dl.item_shed_path(i, project_dir) for i in files]
    parent = f"{dl.HOME}/.itest-delivery"
    env_file = f"{dl.HOME}/{dl.ENV_FILE}"
    got = dl.parse_paths(_bash(who, name, dl.PATHS_SCRIPT, *paths, parent, env_file).stdout)
    problems = dl.file_problems(got, files, project_dir)
    if any(i["path"].startswith(".itest-delivery/") for i in files):
        p = got.get(parent)
        if p is None or (p.kind, p.mode, p.owner) != ("dir", "700", "shed"):
            problems.append(f"{parent}: {p}, want a directory 700 owned by shed (made by the writer)")
    if env_items:
        e = got.get(env_file)
        if e is None or e.kind != "file" or e.mode != "600" or e.owner != "shed":
            problems.append(f"{env_file}: {e and (e.kind, e.mode, e.owner)}, want a file 600 owned by shed")
        elif e.content != dl.env_file_text(env_items).encode():
            problems.append(f"{env_file} does not hold exactly the delivered env items, in order")
    assert not problems, f"{name}: what the delivery wrote:\n" + "\n".join(problems)
    for i in env_items:
        r = _exec(who, name, "printenv", i["variable"])
        assert r.stdout == i["value"] + "\n", f"{name}: `shed exec printenv {i['variable']}` is not the delivered value byte for byte"
        r = who.ssh(name, f"printenv {i['variable']}")
        assert r.returncode == 0 and r.stdout == i["value"] + "\n", (
            f"{name}: an SSH session's {i['variable']} is not the delivered value byte for byte (exit {r.returncode})"
        )


def _written_at(rec: dict) -> dict:
    return {i["name"]: (i.get("result"), i.get("written_at")) for i in rec.get("items") or []}


def _assert_steps(rec: dict, want: dict, what: str) -> None:
    problems = dl.step_problems(rec, want)
    assert not problems, f"{what}: steps {dl.steps_of(rec)}: " + "; ".join(problems)


def _assert_items(rec: dict, items: list[dict], what: str, **kw) -> None:
    problems = dl.record_item_problems(rec, items, **kw)
    assert not problems, f"{what}: record items:\n" + "\n".join(problems)


# ----------------------------------------------------------------------------
# Alice's main shed (case 1), reused by the cases that provision
# ----------------------------------------------------------------------------


class MainShed:
    """alice's shed made by case 1 (a create with the canned bundle and a
    public repository), shared by the module's provision cases. Made when
    first asked for; deleted at the module's end, or by case 1 itself on
    [remote], where nothing else uses it."""

    def __init__(self, d: DeliveryEnv) -> None:
        self.d = d
        self.name: Optional[str] = None
        self.created: Optional[subprocess.CompletedProcess] = None
        self.requests: list[dict] = []
        self.items: list[dict] = []

    def get(self) -> str:
        if self.name is not None:
            return self.name
        d = self.d
        _require_image(d.backend)
        name = _name("main")
        items = d.items()
        try:
            with d.start(items, label="main-create") as src:
                self.created = _create(d, d.alice, name, repo=REPO)
                self.requests = src.requests(name)
        except BaseException as e:
            try:
                _delete(d.users, d.alice, name)
            except AssertionError as cleanup:
                e.add_note(str(cleanup))
            raise
        d.delivered.add(name)
        self.name, self.items = name, items
        return name

    def close(self) -> None:
        if self.name is not None:
            name, self.name = self.name, None
            _delete(self.d.users, self.d.alice, name)


@pytest.fixture(scope="module")
def main_shed(delivery_env):
    slot = MainShed(delivery_env)
    yield slot
    slot.close()


# ----------------------------------------------------------------------------
# The image gate (and its unit-tested body: test_smoke_units.py)
# ----------------------------------------------------------------------------


@pytest.fixture
def delivery_image_name(shed_server_dev, request):
    """The image test's shed name, armed for cleanup ONLY after the gate and
    the dev-target guard pass: a listed backend on a server the guard
    refuses (an installed one) gets no create and no delete either."""
    key = dl.backend_key(shed_server_dev.backend)
    if key not in dl.delivery_image_backends():
        pytest.skip(f"SHED_IMAGE_HAS_DELIVERY does not list {key}: this backend still boots the published image")
    _assert_dev_target(shed_server_dev.name)
    name = "itest-delivery-image-" + hashlib.sha256(request.node.nodeid.encode()).hexdigest()[:6]
    yield name
    try:
        shed_server_dev.delete(name, ignore_missing=True)
    except Exception:
        pass


# vz and fc only (shed_server_dev): the cloud image's packages are checked when
# it is baked (gce/verify.sh).
@pytest.mark.skipif(not dl.delivery_image_backends(),
                    reason="SHED_IMAGE_HAS_DELIVERY unset: published images predate agent protocol 2, parcel and envsecrets")
def test_delivery_image_baked(shed_server_dev, delivery_image_name):
    """A `full` shed from an image rebuilt from this branch: its agent speaks
    protocol 2 (it takes delivered items) and parcel and envsecrets are on
    the login PATH. A backend listed in `SHED_IMAGE_HAS_DELIVERY` whose store
    has no `full` image FAILS rather than skips. Only ever on a parallel dev
    server (`shed_server_dev`, and the dev-target guard before the create):
    the rebuilt images live in a dev store, never an installed server's."""
    shed_server, test_shed_name = shed_server_dev, delivery_image_name
    key = dl.backend_key(shed_server.backend)
    listed = dl.delivery_image_backends()
    if key not in listed:
        pytest.skip(f"SHED_IMAGE_HAS_DELIVERY does not list {key}: this backend still boots the published image")
    _assert_dev_target(shed_server.name)
    image_present = True
    try:
        shed_server.create(test_shed_name, image="full")
    except AssertionError as e:
        msg = str(e)
        if "full" in msg and ("no image tag" in msg or "not found" in msg or "unknown image" in msg):
            image_present = False
        else:
            raise
    verdict = test_smoke._baked_image_verdict(key, listed, image_present)
    if verdict == "fail":
        pytest.fail(f"SHED_IMAGE_HAS_DELIVERY lists {key!r} but its dev store has no `full` image; build it from this "
                    f"branch (docs/development/testing.md) or take {key} out of SHED_IMAGE_HAS_DELIVERY.")
    assert verdict == "run", verdict
    r = shed_server.exec(test_shed_name, ["/usr/local/bin/shed-agent", "version"])
    assert r.returncode == 0, f"shed-agent version: exit {r.returncode}: {r.stderr!r}"
    info = json.loads(r.stdout)
    assert info.get("protocol", 0) >= 2, f"the image's agent speaks protocol {info.get('protocol')}, want 2 or more: {info!r}"
    r = shed_server.exec(test_shed_name, ["sh", "-c", "command -v parcel && command -v envsecrets"])
    assert r.returncode == 0 and len(r.stdout.split()) == 2, (
        f"parcel and envsecrets are not both on the login PATH of a `full` shed: exit {r.returncode} {r.stdout!r} {r.stderr!r}"
    )


# ----------------------------------------------------------------------------
# The server
# ----------------------------------------------------------------------------


def test_the_server_advertises_delivery_and_activity(delivery_env):
    status, body = delivery_env.alice.api().request("GET", "/api/info")
    assert status == 200, body
    for f in dl.FEATURES:
        assert f in (body or {}).get("features", []), f"{f} is not among the features: {body!r}"


# ----------------------------------------------------------------------------
# 1. A create with a canned bundle
# ----------------------------------------------------------------------------


def test_create_delivers_a_canned_bundle(delivery_env, main_shed):
    d = delivery_env
    alice = d.alice
    name = main_shed.get()
    items = main_shed.items
    try:
        out = main_shed.created.stdout + main_shed.created.stderr
        assert dl.delivery_warnings(out) == [], f"a delivered create warned: {dl.delivery_warnings(out)}"
        reqs = main_shed.requests
        assert len(reqs) == 1, f"want one request for {name}, got {reqs}"
        assert (reqs[0].get("reason"), reqs[0].get("user")) == ("create", ALICE), reqs[0]

        doc = _doc(d, alice, name)
        assert doc.get("id"), f"{name} has no permanent id: {doc!r}"
        assert doc.get("landing_dir") in (REPO_DIR, None), doc.get("landing_dir")
        rec = doc["delivery"]
        assert rec.get("state") == "delivered", rec
        _assert_steps(rec, {"stage1": ("done", None), "clone": ("done", None), "stage2": ("done", None),
                            "install": ("skipped", "no_hook")}, name)
        last = rec.get("last") or {}
        assert (last.get("reason"), last.get("outcome")) == ("create", "accepted"), last
        assert "failure" not in last and not last.get("problems"), last
        assert last.get("source") == {"machine_id": d.machine.id, "machine_name": d.machine.name}, last
        _assert_items(rec, items, name)
        assert not rec.get("owed") and not rec.get("expired"), rec

        _check_bundle(d, alice, name, items, REPO_DIR)

        # The order: stage 1 before the clone, stage 2 inside the clone and
        # after it. `.git/HEAD` is written by the clone and never again.
        stage1 = f"{dl.HOME}/{STAGE1_FILE}"
        head = f"{REPO_DIR}/.git/HEAD"
        stage2 = f"{REPO_DIR}/.itest-delivery-project"
        r = _exec(alice, name, "find", stage1, head, stage2, "-maxdepth", "0", "-printf", "%T@\t%p\n")
        t = {p: float(s) for s, p in (line.split("\t", 1) for line in r.stdout.splitlines() if "\t" in line)}
        assert set(t) == {stage1, head, stage2}, r.stdout
        assert t[stage1] < t[head], f"the stage 1 file ({t[stage1]}) is not older than the clone ({t[head]})"
        assert t[head] <= t[stage2], f"the stage 2 file ({t[stage2]}) is older than the clone ({t[head]})"
        r = _exec(alice, name, "git", "-C", REPO_DIR, "config", "--get", "remote.origin.url")
        assert r.stdout.strip() == REPO, r.stdout

        # The listing shows the state.
        r = alice.cli("list", timeout=60)
        d.keep(r.stdout, r.stderr)
        assert r.returncode == 0 and "DELIVERY" in r.stdout, r.stdout
        row = [line for line in r.stdout.splitlines() if line.split()[:1] == [name]]
        assert row and "delivered" in row[0].split(), f"{name}'s row: {row}"
        r = alice.cli("--json", "list", timeout=60)
        d.keep(r.stdout)
        listed = {s["name"]: s for s in json.loads(r.stdout)}
        assert listed[name]["delivery"]["state"] == "delivered", listed[name].get("delivery")
    finally:
        if d.backend == "remote":
            main_shed.close()


# ----------------------------------------------------------------------------
# 2 and 3. A source that does not answer, and what `shed provision` does
# ----------------------------------------------------------------------------


@pytest.mark.local_vm_only(CORE_ONLY)
def test_no_source_bound_is_not_applicable_and_silent(delivery_env):
    """A person with no bound machine gets the create they always had: no
    request, no warning, the clone, and the record `not_applicable`."""
    d = delivery_env
    admin = d.users.admin
    if bound_id(sources(admin.api())) is not None:
        pytest.skip("itest-admin has a credential source bound on this server (bindings outlive a restart, and "
                    "test_users.py binds one): the no-source case needs a user who never had one. The Mac dev "
                    "server keeps the itest users' bindings only for a module.")
    name = _name("nosrc")
    try:
        r = _create(d, admin, name, repo=REPO)
        assert dl.delivery_warnings(r.stdout + r.stderr) == [], r.stderr
        rec = _record(d, admin, name)
        assert rec.get("state") == "not_applicable", rec
        _assert_steps(rec, {"stage1": ("skipped", "not_applicable"), "clone": ("done", None),
                            "stage2": ("skipped", "not_applicable"), "install": ("skipped", "no_hook")}, name)
        last = rec.get("last") or {}
        assert last.get("outcome") == "not_requested" and "failure" not in last and "source" not in last, last
        assert not rec.get("items") and not rec.get("owed"), rec
        r = _exec(admin, name, "git", "-C", REPO_DIR, "rev-parse", "--is-inside-work-tree")
        assert r.stdout.strip() == "true", r.stdout
    finally:
        _delete(d.users, admin, name)


def test_offline_source_at_create_then_provision_finishes_it(delivery_env):
    """Cases 2 and 3: the bound machine is not connected at create (the shed
    starts; `source_offline` at once; the public clone runs; stage 2 and the
    install hook are owed). `shed provision` with the source connected
    finishes every step; a second provision changes nothing but times and
    removes nothing."""
    d = delivery_env
    _require_image(d.backend)
    alice = d.alice
    _wait_offline(d)
    items = d.items()
    name = _name("offline")
    try:
        d.delivered.add(name)
        r = _create(d, alice, name, repo=REPO)
        if d.backend != "remote":
            # A remote create answers 201 starting and is polled: its warnings
            # are in the record, not the output.
            assert dl.delivery_warnings(r.stdout + r.stderr) == [dl.delivery_warning(name, "source_offline")], r.stderr
        rec = _record(d, alice, name)
        assert rec.get("state") == "not_delivered", rec
        _assert_steps(rec, {"stage1": ("failed", "source_offline"), "clone": ("done", None),
                            "stage2": ("owed", None), "install": ("owed", None)}, name)
        assert rec.get("owed") == ["stage2", "install"], rec
        last = rec.get("last") or {}
        assert (last.get("reason"), last.get("outcome"), last.get("failure")) == (
            "create", "no_answer", {"reason": "source_offline"}), last
        assert last.get("source") == {"machine_id": d.machine.id, "machine_name": d.machine.name}, last
        assert not rec.get("items"), rec

        with d.start(items, label="offline-provision") as src:
            r = _provision_cli(d, alice, name)
            assert dl.delivery_warnings(r.stdout + r.stderr) == [], r.stderr
            assert f"Provisioned shed {name}: delivered" in r.stdout, r.stdout
            reqs = src.requests(name)
            assert [q.get("reason") for q in reqs] == ["provision"], reqs
            first = _record(d, alice, name)
            assert first.get("state") == "delivered", first
            _assert_steps(first, {"stage1": ("done", None), "clone": ("done", None), "stage2": ("done", None),
                                  "install": ("skipped", "no_hook")}, name)
            assert (first["last"].get("reason"), first["last"].get("outcome")) == ("provision", "accepted"), first["last"]
            _assert_items(first, items, name)
            _check_bundle(d, alice, name, items, REPO_DIR)

            again = _provision_ok(d, alice, name)
            assert dl.without_times(again) == dl.without_times(first), (
                f"a second provision changed more than times:\nfirst {first}\nagain {again}")
            assert again["last"]["at"] >= first["last"]["at"]
            _check_bundle(d, alice, name, items, REPO_DIR)
    finally:
        _delete(d.users, alice, name)


@pytest.mark.local_vm_only(CORE_ONLY)
def test_silent_source_times_out_and_provision_leaves_an_existing_directory(delivery_env):
    """A connected source that never answers: the create waits the delivery
    timeout and ends `source_timeout` (the unreachable repository's clone
    fails too). Then something that is not the repository stands at the
    project directory: `shed provision` delivers stage 1, fails the clone
    `project_dir_exists` and leaves the directory as it is."""
    d = delivery_env
    _require_image(d.backend)
    alice = d.alice
    items = d.items()
    name = _name("silent")
    try:
        d.delivered.add(name)
        with d.start(label="silent", silent=True) as src:
            t0 = time.monotonic()
            r = _create(d, alice, name, repo=MISSING_REPO)
            waited = time.monotonic() - t0
            reqs = src.requests(name)
        assert [q.get("reason") for q in reqs] == ["create"], reqs
        assert waited >= DELIVERY_TIMEOUT - 5, f"the create took {waited:.0f} s: it did not wait for the source"
        assert dl.delivery_warnings(r.stdout + r.stderr) == [dl.delivery_warning(name, "source_timeout")], r.stderr
        rec = _record(d, alice, name)
        assert rec.get("state") == "not_delivered", rec
        _assert_steps(rec, {"stage1": ("failed", "source_timeout"), "clone": ("failed", "clone_failed"),
                            "stage2": ("owed", None), "install": ("owed", None)}, name)
        assert (rec["last"].get("outcome"), rec["last"].get("failure")) == ("no_answer", {"reason": "source_timeout"})

        keep = uuid.uuid4().hex
        _bash(alice, name, 'mkdir -p "$1" && printf %s "$2" > "$1/keep"', MISSING_DIR, keep)
        with d.start(items, label="silent-provision"):
            r = _provision_cli(d, alice, name)
        assert dl.delivery_warnings(r.stdout + r.stderr) == [], r.stderr
        rec = _record(d, alice, name)
        _assert_steps(rec, {"stage1": ("done", None), "clone": ("failed", "project_dir_exists"),
                            "stage2": ("owed", None), "install": ("owed", None)}, name)
        assert rec.get("state") == "partial", rec
        _assert_items(rec, items, name, results={"itest-project": "not_written"})
        r = _bash(alice, name, 'cat "$1/keep"; echo; ls -A "$1"', MISSING_DIR)
        lines = r.stdout.splitlines()
        assert lines[:1] == [keep] and lines[1:] == ["keep"], f"{MISSING_DIR} was not left alone: {r.stdout!r}"
    finally:
        _delete(d.users, alice, name)


@pytest.mark.local_vm_only(CORE_ONLY)
def test_source_disconnecting_mid_request_fails_it_at_once(delivery_env, main_shed):
    d = delivery_env
    alice = d.alice
    name = main_shed.get()
    src = d.start(label="disconnect", silent=True)
    proc = None
    try:
        proc = alice.popen_cli("provision", name)
        req = src.wait_request(name)
        t0 = time.monotonic()
        src.stop()
        out, err = proc.communicate(timeout=DELIVERY_TIMEOUT + 120)
        took = time.monotonic() - t0
    finally:
        src.stop()
        _reap(proc)
    d.keep(out, err)
    assert proc.returncode == 0, f"shed provision exited {proc.returncode}: {err[-600:]!r}"
    assert req.get("reason") == "provision", req
    assert took < PROMPT, f"the request ended {took:.0f} s after its stream: it waited for its deadline"
    assert dl.delivery_warnings(out + err) == [dl.delivery_warning(name, "source_disconnected")], err
    rec = _record(d, alice, name)
    _assert_steps(rec, {"stage1": ("failed", "source_disconnected"), "clone": ("done", None),
                        "stage2": ("done", None), "install": ("skipped", "no_hook")}, name)
    assert (rec["last"].get("outcome"), rec["last"].get("failure")) == ("no_answer", {"reason": "source_disconnected"})
    assert rec.get("state") == "partial", rec


# ----------------------------------------------------------------------------
# 4. Refused answers
# ----------------------------------------------------------------------------


@pytest.mark.local_vm_only(CORE_ONLY)
def test_each_refused_answer_is_refused_and_writes_nothing(delivery_env, main_shed):
    """One fixture per rule (every live-runnable fixture with
    SHED_DELIVERY_REFUSE_ALL=1): the reference source sends it as alice's
    answer to a provision of her main shed; the server refuses it with the
    fixture's status and rule, the request is over (`answer_refused`), the
    record's items are untouched and no file in the home or project
    directory is newer than the batch."""
    d = delivery_env
    alice = d.alice
    name = main_shed.get()
    everything = os.environ.get("SHED_DELIVERY_REFUSE_ALL", "").strip() == "1"
    fixtures = dl.select_refuse_fixtures(dl.load_refuse_fixtures(), everything=everything)
    assert any(f.name == "path-dot-segment" for f in fixtures), "the `.` segment fixture must be in the live set"
    before = _written_at(_record(d, alice, name))
    marker = "/tmp/itest-delivery-refuse-marker"
    _bash(alice, name, ': > "$1" && sleep 1', marker)
    failures = []
    for f in fixtures:
        with d.start(refuse=f.path, label=f"refuse-{f.name}") as src:
            status, body = _provision(d, alice, name)
            answers = src.answers()
            info = src.describe()
        what = f"{f.name} ({f.rule})"
        if status != 200 or not isinstance(body, dict):
            failures.append(f"{what}: provision {status} {body!r}")
            continue
        got = [(a.get("status"), a.get("code"), a.get("rule")) for a in answers]
        if got != [(f.status, "answer_refused", f.rule)]:
            failures.append(f"{what}: the answer was judged {got}, want {[(f.status, 'answer_refused', f.rule)]}; {info}")
        rec = body["delivery"]
        if dl.steps_of(rec)["stage1"] != ("failed", "answer_refused"):
            failures.append(f"{what}: stage1 {dl.steps_of(rec)['stage1']}")
        last = rec.get("last") or {}
        if (last.get("outcome"), last.get("failure")) != ("refused", {"reason": "answer_refused", "rule": f.rule}):
            failures.append(f"{what}: last {last}")
        if _written_at(rec) != before:
            failures.append(f"{what}: the record's items changed: {rec.get('items')}")
    r = _bash(alice, name, 'find /home/shed -xdev -newer "$1" -not -path "/home/shed/.cache/*" -print', marker)
    assert not failures, f"{len(failures)} of {len(fixtures)} refusals were wrong:\n" + "\n".join(failures)
    assert r.stdout.strip() == "", f"files changed in the shed during the refusals:\n{r.stdout}"


# ----------------------------------------------------------------------------
# 5. Answer binding
# ----------------------------------------------------------------------------


@pytest.mark.local_vm_only(CORE_ONLY)
@pytest.mark.parametrize("how", ["second-connection", "another-certificate"])
def test_an_answer_not_from_the_requests_stream_is_refused(delivery_env, main_shed, how, tmp_path):
    """An answer from a connection that did not open the stream (a stream id
    not its own), or with another of alice's credentials certificates: 403
    `not_stream_owner`, nothing accepted, and the request runs to its
    deadline (`source_timeout`)."""
    d = delivery_env
    alice = d.alice
    name = main_shed.get()
    if how == "second-connection":
        kw = {"second_connection": True}
    else:
        tmp_path.chmod(0o700)
        other = alice.enrol("credentials", tmp_path / "other")
        kw = {"answer_cert": other.pair}
    before = _written_at(_record(d, alice, name))
    try:
        with d.start(d.items(), label=how, **kw) as src:
            status, body = _provision(d, alice, name)
            answers = src.answers()
    finally:
        shutil.rmtree(tmp_path / "other", ignore_errors=True)
    assert status == 200, (status, body)
    assert answers and all((a.get("status"), a.get("code")) == (403, "not_stream_owner") for a in answers), answers
    rec = body["delivery"]
    _assert_steps(rec, {"stage1": ("failed", "source_timeout")}, name)
    assert _written_at(rec) == before, "an answer that was refused wrote items"


@pytest.mark.local_vm_only(CORE_ONLY)
def test_a_second_answer_is_already_answered(delivery_env, main_shed):
    d = delivery_env
    alice = d.alice
    name = main_shed.get()
    with d.start(d.items(), label="twice", twice=True) as src:
        rec = _provision_ok(d, alice, name)
        req = src.wait_request(name, timeout=5)
        # The second answer is posted after the first's 204, which may be
        # after the provision returned.
        src.wait_for(lambda e: e["event"] == "answer" and e.get("id") == req["id"] and e.get("attempt") == 2, 30)
        answers = src.answers(req["id"])
    got = sorted((a.get("attempt"), a.get("status"), a.get("code")) for a in answers)
    assert got == [(1, 204, None), (2, 409, "already_answered")], answers
    assert rec.get("state") == "delivered", rec


@pytest.mark.local_vm_only(CORE_ONLY)
def test_a_pin_move_fails_the_pending_request_and_the_old_machines_answer(delivery_env, main_shed):
    """While a request waits on alice's delivery machine, she moves her
    source to her default machine: the request fails at once
    (`source_disconnected`), the old machine's stream ends, and its answer
    is refused (403 SOURCE_NOT_BOUND)."""
    d = delivery_env
    alice = d.alice
    api = alice.api()
    name = main_shed.get()
    ensure_known(api, d.default_machine, _ns())
    src = d.start(label="pin-move", silent=True)
    proc = None
    try:
        proc = alice.popen_cli("provision", name)
        req = src.wait_request(name)
        t0 = time.monotonic()
        status, body = api.request("POST", "/api/sources/use", {"machine_id": d.default_machine.id})
        assert status == 200, f"moving alice's source: {status} {body!r}"
        out, err = proc.communicate(timeout=DELIVERY_TIMEOUT + 120)
        took = time.monotonic() - t0
        d.keep(out, err)
        ended = src.wait_for(lambda e: e["event"] == "end", 15)
        answer = {"version": 1, "id": req["id"], "items": [], "problems": []}
        late = pinned_request(alice.host, d.cert.https_port, f"/api/delivery/requests/{req['id']}", d.cert.pin,
                              d.cert.pair, method="POST", body=answer,
                              headers={**d.machine.headers, "X-Shed-Delivery-Stream": req["stream"]})
    finally:
        src.stop()
        _reap(proc)
        ensure_source(alice.api(), d.machine, _ns())
    assert proc.returncode == 0, f"shed provision exited {proc.returncode}: {err[-600:]!r}"
    assert took < PROMPT, f"the request ended {took:.0f} s after the move: it waited for its deadline"
    assert dl.delivery_warnings(out + err) == [dl.delivery_warning(name, "source_disconnected")], err
    assert ended is not None, f"the old machine's stream did not end at the move: {src.describe()}"
    assert (late[0], error_code(late[1])) == (403, "SOURCE_NOT_BOUND"), f"the old machine's answer: {late}"
    rec = _record(d, alice, name)
    _assert_steps(rec, {"stage1": ("failed", "source_disconnected")}, name)


# ----------------------------------------------------------------------------
# 7. Who is asked, and who may ask
# ----------------------------------------------------------------------------


@pytest.mark.local_vm_only(CORE_ONLY)
@pytest.mark.not_under_directory_source(NO_SCRIPT_KEY_IN_A_LIST)
def test_a_script_keys_shed_gets_no_request_and_the_key_cannot_provision(delivery_env, tmp_path):
    d = delivery_env
    alice = d.alice
    tmp_path.chmod(0o700)
    ctl = d.users.alice_script.enrol("control", tmp_path / "script")
    script = d.users.alice_script.pinned(ctl)
    name = _name("script")
    try:
        with d.start(d.items(), label="script") as src:
            status, body = script.request("POST", "/api/sheds", {"name": name, "image": d.users.image}, timeout=300)
            d.keep(body)
            assert status == 201, f"a create with alice's script key: {status} {body!r}"
            rec = _record(d, alice, name)
            assert rec.get("state") == "not_applicable", rec
            _assert_steps(rec, {"stage1": ("skipped", "not_applicable"), "clone": ("skipped", "no_repo"),
                                "stage2": ("skipped", "not_applicable")}, name)
            assert src.requests(name) == [], src.requests(name)
            status, body = script.request("POST", f"/api/sheds/{name}/provision", timeout=60)
            d.keep(body)
            assert (status, error_code(body)) == (403, "FORBIDDEN"), (status, body)
            assert src.requests(name) == []
            # Control: alice's own key may provision it, and her source is asked.
            if d.backend in dl.delivery_image_backends():
                rec = _provision_ok(d, alice, name)
                assert rec.get("state") == "delivered", rec
                assert [q.get("reason") for q in src.requests(name)] == ["provision"]
    finally:
        shutil.rmtree(tmp_path / "script", ignore_errors=True)
        _delete(d.users, alice, name)


SVC_CLIENT = "itest"
SVC_PREFIX = "itest-svc-"
SVC_IMAGE = "itest"


@pytest.mark.local_vm_only(CORE_ONLY)
def test_a_service_clients_shed_gets_no_request_and_it_cannot_provision(delivery_env, tmp_path):
    d = delivery_env
    if d.backend != "fc":
        pytest.skip("a service client's bundle is issued on the FC dev host (as test_service_client.py does)")
    server = d.users.server
    dev_bin = os.environ.get("SHED_FC_DEV_BIN_PATH", "/tmp/shed-server-dev")
    dev_config = os.environ.get("SHED_FC_DEV_CONFIG", "/tmp/shed-server-dev.yaml")
    r = server.ssh_run(f"sudo -n {dev_bin} client issue {SVC_CLIENT} --config {dev_config}", timeout=60)
    if r.returncode != 0:
        pytest.skip(f"cannot issue the {SVC_CLIENT!r} service client's bundle on {server.ssh_host} "
                    f"(SHED_FC_DEV_BIN_PATH, SHED_FC_DEV_CONFIG): {r.stderr.strip()[-300:]!r}")
    bundle = json.loads(r.stdout)
    tmp_path.chmod(0o700)
    cert, key = tmp_path / "client.pem", tmp_path / "client.key"
    cert.write_text(bundle["client_cert"])
    key.write_text(bundle["client_key"])
    key.chmod(0o600)
    entry = resolve_server_entry(server.name) or {}
    host = urlparse(entry.get("api_url", "")).hostname or d.alice.host
    svc = Pinned(host, bundle["https_port"], bundle["tls_cert_fingerprint"], str(cert), str(key))
    name = f"{SVC_PREFIX}{uuid.uuid4().hex[:8]}"
    try:
        with d.start(d.items(), label="svc") as src:
            status, body = svc.request("POST", "/api/sheds", {"name": name, "image": SVC_IMAGE}, timeout=300)
            d.keep(body)
            assert status == 201, f"the service client's create: {status} {body!r}"
            status, doc = svc.request("GET", f"/api/sheds/{name}")
            d.keep(doc)
            assert status == 200 and doc["delivery"].get("state") == "not_applicable", doc.get("delivery")
            assert src.requests(name) == []
            status, body = svc.request("POST", f"/api/sheds/{name}/provision", timeout=60)
            d.keep(body)
            assert (status, error_code(body)) == (403, "FORBIDDEN"), (status, body)
            assert src.requests(name) == []
    finally:
        try:
            svc.request("DELETE", f"/api/sheds/{name}", timeout=300)
        except Exception:  # noqa: BLE001 - the checked clean-up below decides
            pass
        server.ensure_deleted(name)
        shutil.rmtree(tmp_path, ignore_errors=True)


@pytest.mark.local_vm_only(CORE_ONLY)
def test_only_the_sheds_own_user_may_provision_it(delivery_env, main_shed):
    """bob cannot see alice's shed (404, as for a shed that does not exist);
    an admin sees it but may not enter it, so may not provision it (403),
    and reads its delivery state alone."""
    d = delivery_env
    name = main_shed.get()
    bob, admin = d.users.bob, d.users.admin
    status, body = bob.api().request("POST", f"/api/sheds/{name}/provision")
    d.keep(body)
    assert (status, error_code(body)) == (404, "SHED_NOT_FOUND"), (status, body)
    r = bob.cli("provision", name, timeout=60)
    d.keep(r.stdout, r.stderr)
    assert r.returncode != 0, f"bob's `shed provision {name}` succeeded: {r.stdout!r}"
    status, body = admin.api().request("POST", f"/api/sheds/{name}/provision")
    d.keep(body)
    assert (status, error_code(body)) == (403, "FORBIDDEN"), (status, body)
    doc = _doc(d, admin, name)
    rec = doc.get("delivery") or {}
    assert set(rec) == {"state", "withheld"} and rec["withheld"] is True, f"an admin reads more than the state: {rec}"


@pytest.mark.local_vm_only(CORE_ONLY)
def test_a_second_provision_while_one_runs_is_refused(delivery_env, main_shed):
    d = delivery_env
    alice = d.alice
    name = main_shed.get()
    with d.start(d.items(), label="busy", delay=8) as src:
        job = _Job(lambda: _provision(d, alice, name))
        src.wait_request(name, timeout=60)
        status, body = alice.api().request("POST", f"/api/sheds/{name}/provision")
        d.keep(body)
        first = job.result(DELIVERY_TIMEOUT + 120)
    assert (status, error_code(body)) == (409, "PROVISION_IN_PROGRESS"), (status, body)
    assert first[0] == 200 and first[1]["delivery"].get("state") == "delivered", first
    assert len(src.requests(name)) == 1, src.requests(name)


# ----------------------------------------------------------------------------
# 8. Start with an expired item
# ----------------------------------------------------------------------------


@pytest.mark.local_vm_only(CORE_ONLY)
def test_start_warns_once_per_expired_item_and_asks_no_source(delivery_env, main_shed):
    d = delivery_env
    alice = d.alice
    name = main_shed.get()
    items = d.items() + [
        dl.file_item("itest-expired", ".itest-delivery/expired", b"expired\n", expires_at="2020-01-01T00:00:00Z"),
        dl.file_item("itest-future", ".itest-delivery/future", b"future\n", expires_at="2099-01-01T00:00:00Z"),
    ]
    with d.start(items, label="expired") as src:
        rec = _provision_ok(d, alice, name)
        assert rec.get("state") == "delivered", rec
        _assert_items(rec, items, name)
        assert _doc(d, alice, name)["delivery"].get("expired") == ["itest-expired"]
        r = alice.cli("stop", name, timeout=_timeout(d.users))
        assert r.returncode == 0, r.stderr
        asked = len(src.requests())
        t0 = time.monotonic()
        r = alice.cli("start", name, timeout=_timeout(d.users))
        d.keep(r.stdout, r.stderr)
        assert r.returncode == 0, r.stderr
        # Anything the start caused would have reached the source by now; a
        # few seconds more for a request on its way.
        late = src.wait_for(lambda e: e["event"] == "request" and e["_t"] >= t0, 5)
    out = r.stdout + r.stderr
    assert dl.expired_warnings(out) == [dl.expired_warning(name, "itest-expired")], out
    assert dl.delivery_warnings(out) == [], out
    assert late is None and len(src.requests()) == asked, f"`shed start` asked the source: {src.requests()}"


# ----------------------------------------------------------------------------
# 9. A published image
# ----------------------------------------------------------------------------


@pytest.mark.local_vm_only(CORE_ONLY)
def test_an_image_whose_agent_cannot_take_items_is_image_too_old(delivery_env):
    """The server reads the agent's protocol before it asks anyone: an image
    whose agent is older ends `image_too_old`, makes no request, and never
    runs the subcommand (which an old agent would take for a second
    daemon)."""
    d = delivery_env
    alice = d.alice
    old = os.environ.get("SHED_DELIVERY_OLD_IMAGE", "").strip()
    if d.backend in dl.delivery_image_backends() and not old:
        pytest.skip(f"SHED_IMAGE_HAS_DELIVERY lists {d.backend}: its image's agent takes items. Set "
                    f"SHED_DELIVERY_OLD_IMAGE to an image alias that boots a published image to run this here.")
    name = _name("old")
    try:
        with d.start(d.items(), label="old-image") as src:
            r = _create(d, alice, name, image=old or None)
            reqs = src.requests(name)
        assert reqs == [], f"a request was made for an image whose agent cannot take items: {reqs}"
        assert dl.delivery_warnings(r.stdout + r.stderr) == [dl.delivery_warning(name, "image_too_old")], r.stderr
        rec = _record(d, alice, name)
        _assert_steps(rec, {"stage1": ("failed", "image_too_old"), "clone": ("skipped", "no_repo"),
                            "stage2": ("owed", None), "install": ("owed", None)}, name)
        last = rec.get("last") or {}
        assert (last.get("outcome"), last.get("failure")) == ("not_requested", {"reason": "image_too_old"}), last
        assert "source" not in last, last
        r = _exec(alice, name, "sh", "-c",
                  'n=0; for f in /proc/[0-9]*/comm; do [ "$(cat "$f" 2>/dev/null)" = shed-agent ] && n=$((n+1)); done; echo $n')
        assert r.stdout.strip() == "1", f"{r.stdout.strip()} shed-agent processes in the shed, want 1"
    finally:
        _delete(d.users, alice, name)


# ----------------------------------------------------------------------------
# 10. Activity
# ----------------------------------------------------------------------------


def _activity(d: DeliveryEnv, who: Identity, **q) -> dict:
    status, body = who.api().request("GET", "/api/activity?" + urlencode({k: v for k, v in q.items() if v is not None}))
    d.keep(body)
    assert status == 200 and isinstance(body, dict), f"{who.user}: GET /api/activity {q}: {status} {body!r}"
    return body


def _all_activity(d: DeliveryEnv, who: Identity, shed: str, limit: int) -> list[dict]:
    return dl.page_all(lambda since: _activity(d, who, shed=shed, limit=limit, since=since))


@pytest.mark.local_vm_only(CORE_ONLY)
def test_activity_follows_whose_the_shed_was(delivery_env):
    """alice creates and deletes a shed; bob takes its name for his own. Each
    sees only the events of the shed that was theirs, by name and by id,
    with the delivery's detail for their own; the admin sees both, the
    newer first, with alice's delivery state alone; paging one event at a
    time gives the same list as one page."""
    d = delivery_env
    alice, bob, admin = d.alice, d.users.bob, d.users.admin
    name = _name("act")
    ids = {}
    for who in (alice, bob):
        try:
            _create(d, who, name)
            ids[who.user] = _doc(d, who, name)["id"]
        finally:
            _delete(d.users, who, name)
    assert ids[ALICE] and ids[BOB] and ids[ALICE] != ids[BOB], ids

    mine = _all_activity(d, alice, name, 500)
    kinds = {e["kind"] for e in mine}
    assert {"shed.create", "shed.delete", "delivery"} <= kinds, f"alice's events for {name}: {kinds}"
    for e in mine:
        assert e["shed"].get("name") == name and e["shed"].get("user") == ALICE and e["shed"].get("id") == ids[ALICE], e
    assert all("steps" in (e.get("delivery") or {}) for e in mine if e["kind"] == "delivery"), \
        "alice reads her own delivery's detail"
    assert [e["id"] for e in _activity(d, alice, shed=ids[ALICE], limit=500)["events"]] == [e["id"] for e in mine]
    assert _activity(d, alice, shed=ids[BOB], limit=500)["events"] == [], "alice sees bob's shed by its id"

    his = _all_activity(d, bob, name, 500)
    assert {"shed.create", "shed.delete"} <= {e["kind"] for e in his}, his
    for e in his:
        assert e["shed"].get("user") == BOB and e["shed"].get("id") == ids[BOB], e

    full = _all_activity(d, admin, name, 500)
    by_id = [e["shed"].get("id") for e in full if e["shed"].get("id") in ids.values()]
    assert {e["id"] for e in mine} | {e["id"] for e in his} <= {e["id"] for e in full}, "the admin misses events"
    first_alice = by_id.index(ids[ALICE])
    assert ids[BOB] not in by_id[first_alice:], f"not newest first: {by_id}"
    for e in full:
        if e["kind"] == "delivery" and e["shed"].get("id") == ids[ALICE]:
            assert set(e.get("delivery") or {}) <= {"state"}, f"the admin reads alice's delivery detail: {e}"

    paged = _all_activity(d, admin, name, 1)
    assert [e["id"] for e in paged] == [e["id"] for e in full], "paging one at a time differs from one page"
    assert [e["id"] for e in _all_activity(d, alice, name, 1)] == [e["id"] for e in mine]


# ----------------------------------------------------------------------------
# 6. The sentinel sweep (last: it sweeps what the module did)
# ----------------------------------------------------------------------------


def test_no_delivered_value_leaves_the_shed(delivery_env, main_shed):
    """Every content and value the module delivered carries one sentinel. On
    a local backend this case first sends it through a failed write (a target
    that is a directory) and a refused answer. Then none of its forms (raw,
    or base64 at any alignment) may be in the API responses and create or
    provision outputs the module kept, the server's log since the module
    began, the audit log, or any file in the server's state directories
    under 16 MiB. Each place has a positive control, so a sweep of the wrong
    file cannot pass."""
    d = delivery_env
    _require_image(d.backend)
    alice, admin = d.alice, d.users.admin
    s = d.sentinel
    if d.backend != "remote":
        name = main_shed.get()
        _bash(alice, name, 'mkdir -p "$1"', f"{dl.HOME}/.itest-delivery-dir")
        items = [
            dl.file_item("itest-ok-first", ".itest-delivery/ok-first", f"first-{s}\n".encode()),
            dl.file_item("itest-is-dir", ".itest-delivery-dir", f"dir-{s}\n".encode()),
            dl.env_item("itest-after", "ITEST_DELIVERY_AFTER", f"after-{s}"),
        ]
        with d.start(items, label="write-failed"):
            r = _provision_cli(d, alice, name)
        want = dl.delivery_warning(name, "write_failed", item="itest-is-dir", result="is_directory")
        assert dl.delivery_warnings(r.stdout + r.stderr) == [want], r.stderr
        rec = _record(d, alice, name)
        _assert_steps(rec, {"stage1": ("failed", "write_failed")}, name)
        assert rec["last"].get("failure") == {"reason": "write_failed", "item": "itest-is-dir", "result": "is_directory"}
        _assert_items(rec, items, name, results={"itest-is-dir": "is_directory", "itest-after": "not_written"})

        refused = [{"name": "itest-path", "provider": "api-keys", "kind": "env", "variable": "PATH",
                    "value": f"/opt/{s}", "fingerprint": "fp-itest-path"}]
        with d.start(refused, label="refused-sentinel") as src:
            rec = _provision_ok(d, alice, name)
            answers = src.answers()
        assert [(a.get("status"), a.get("rule")) for a in answers] == [(422, "refused_variable")], answers
        assert rec["last"].get("failure") == {"reason": "answer_refused", "rule": "refused_variable"}

        _doc(d, admin, name)
        for who in (alice, admin):
            r = who.cli("--json", "list", timeout=60)
            d.keep(r.stdout, r.stderr)
            _activity(d, who, shed=name, limit=500)
        r = alice.cli("--json", "provision", name, "--status", timeout=60)
        d.keep(r.stdout, r.stderr)
        assert r.returncode == 0, r.stderr
    if not d.delivered:
        pytest.skip("no delivery ran in this module on this backend: nothing to sweep")

    leaks = []
    for i, text in enumerate(d.kept):
        got = dl.found_forms(text, d.forms)
        if got:
            leaks.append(f"kept output #{i} holds {got}: {text[:200]!r}")
    assert any(n in t for t in d.kept for n in d.delivered), "the kept outputs name none of the module's sheds"

    log = d.users.server._read_log_since(d.log_offset)
    assert any(f'for shed "{n}"' in log for n in d.delivered), (
        "the server log read since the module began holds no delivery line for its sheds: the wrong log, or none "
        "(set the backend's log path variable)")
    if dl.found_forms(log, d.forms):
        leaks.append(f"the server log holds {dl.found_forms(log, d.forms)}")

    audit = dl.read_audit_log(d.files)
    events = dl.audit_events(audit)
    assert any(e.get("event") == "delivery" and e.get("shed") in d.delivered for e in events), (
        f"the audit log {d.files.audit_log} holds no delivery event for the module's sheds: the wrong file")
    if dl.found_forms(audit, d.forms):
        leaks.append(f"the audit log holds {dl.found_forms(audit, d.forms)}")

    control = dl.scan_state_dirs(d.files, [d.machine.id])
    assert control, (f"no file in the state directories {d.files.state_dirs} names the delivery machine (the source "
                     f"bindings do): the wrong directories")
    hits = dl.scan_state_dirs(d.files, d.forms)
    if hits:
        leaks.append(f"files in the state directories hold a delivered value: {hits}")
    assert not leaks, "a delivered value left the shed:\n" + "\n".join(leaks)
