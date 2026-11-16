# shed integration test suite (`tests/integration/`)

Live integration tests that drive a running `shed-server` via the `shed`
CLI. Complements the pure file-parsing Go tests under `internal/`
(`make test`), which catch unit-file ordering regressions in CI; this
suite catches **live** create-cycle regressions that need a real VM.

Architecture: pytest + subprocess (+ Fabric later, only when needed for
remote-orchestration tasks). Managed with [`uv`](https://docs.astral.sh/uv/).
Background and decision rationale: see
`docs/discovery/runtime-optimization-backlog.md` §16.

## Running

Server-free unit tests of the suite's own helpers and the baked-image test
bodies (`test_smoke_units.py`, `test_fixture_units.py`) run without any
shed-server, and in CI:

```sh
make test-integration-units
```

The live suite:

```sh
make test-integration
```

That target:

1. Verifies `uv` is installed (install via `brew install uv` or
   https://docs.astral.sh/uv/getting-started/installation/).
2. `uv sync` the Python deps into `tests/integration/.venv` (gitignored).
3. `uv run pytest -v` the suite, plus whatever `PYTEST_ARGS` holds (every
   `test-integration*` target passes it on), for example
   `PYTEST_ARGS="test_smoke.py -k 'not vz'"`.

Each test is parameterized over `["vz", "fc", "remote"]` and skips cleanly
when its target backend isn't reachable from this host. The `remote` cases
are opt-in: they skip unless `SHED_REMOTE_SERVER` is set (see
[Remote backend](#remote-backend-cloud-sheds)), so `make test-integration`
never creates a cloud VM. The session-scoped server
fixtures probe once per pytest run; un-reachable backends skip *only* the
tests they would have run, not the whole session.

## Requirements per backend

### VZ (Apple Silicon mac)

- A running shed-server reachable as `shed -s my-server list`
  (e.g. via `brew services start shed`).
- Server log at `/opt/homebrew/var/log/shed-server.log` (the homebrew
  default). Override the entry name with `SHED_VZ_SERVER`.

### Firecracker (default: `mini3` over SSH)

- SSH access to the host (passwordless or agent-cached). Override the
  hostname with `SHED_FC_HOST`.
- `shed -s mini3 list` succeeds (i.e. the entry exists in your
  `~/.shed/config.yaml` and the remote server responds).
- Passwordless `sudo -n journalctl -u shed-server` on the remote, for
  the PhaseTimer log-line fetch in tests 2 and 4. Without it, those two
  tests skip cleanly with a clear reason.
- The remote shed-server must be **v0.5.4 or newer** for any
  PhaseTimer-dependent assertion (`test_phase_timer_emitted`,
  `test_create_agent_p50`, `test_create_rootfs_template_present`).
  Older servers skip those tests.

## Environment overrides

| Variable               | Default                                            | Effect |
|------------------------|----------------------------------------------------|--------|
| `SHED_VZ_SERVER`       | `my-server`                                        | Entry name in `~/.shed/config.yaml` for the local VZ server. Empty skips every `[vz]` case (`make test-integration-dev-fc` sets it empty). |
| `SHED_VZ_LOG_PATH`     | `/opt/homebrew/var/log/shed-server.log`            | Where the VZ shed-server's log file lives. Override for Intel Macs (`/usr/local/...`) or custom installs. |
| `SHED_FC_HOST`         | `mini3`                                            | SSH hostname for the FC server (used for journald log fetch). |
| `SHED_FC_SERVER`       | same as host                                       | Entry name for the FC server (when it differs from the SSH host). |
| `SHED_VZ_DEV_SERVER`   | _unset_                                            | Entry name for a PARALLEL dev VZ shed-server (alongside the brew one on a different port). When unset, the `vz_server_dev` fixture skips cleanly. Set by `make test-integration-dev`. |
| `SHED_VZ_DEV_LOG_PATH` | _unset_                                            | Where the parallel dev VZ shed-server writes its log file. Required when `SHED_VZ_DEV_SERVER` is set. |
| `SHED_FC_DEV_SERVER`   | _unset_                                            | Entry name for a PARALLEL dev FC shed-server (alongside the deb one on a different port). When unset, the `fc_server_dev` fixture skips cleanly. Set by `make test-integration-dev-fc`. |
| `SHED_FC_DEV_LOG_PATH` | _unset_                                            | Path on the FC remote where the parallel dev shed-server writes its log file. Required when `SHED_FC_DEV_SERVER` is set; the fixture reads it via `ssh + sudo -n tail -c +N` (offset-based) because the dev server runs as root via `sudo nohup` and is not under systemd. |
| `SHED_FC_LOG_PATH`     | _unset_ (uses journald)                            | Remote file path for the prod `fc_server` fixture to read logs from. When set, the fixture uses `ssh + sudo -n tail -c +N` against this file instead of journalctl. `make test-integration-dev-fc` sets this to the dev FC server's log file so the existing `shed_server`-using tests find PhaseTimer lines (the dev server isn't under systemd). |
| `SHED_DEV_STATE_DIR`   | _unset_                                             | **Per-machine, optional.** Relocates the Mac dev server's blob-heavy state dirs (images/instances/snapshots/uppers) under this path, for a boot volume too small for a multi-GB dev image store. Unset = the committed default under `~/Library/Application Support/shed-dev/vz`. `socket_dir` stays local. `dev-server-up` refuses to start if it is unmounted/unwritable. |
| `SHED_IMAGE_HAS_ROOST` | _unset_                                            | Gates `test_smoke.py::test_roost_session_baked` — a comma list of the backends (`vz`, `fc`) whose `extensions` image was rebuilt with roost-session baked in. `test-integration-dev` sets `vz`, `test-integration-dev-fc` sets `fc` (the other backend still boots the published image); unset for plain `test-integration` (images built before 0.9.0 carry no roost-session). Overrides `SHED_ROOST_SESSION_VERSION`/`SHED_ROOST_SESSION_PROTOCOL` (default `0.0.21`/`6`) let the test track a future bump. |
| `SHED_IMAGE_HAS_CRAZE` | _unset_                                            | Gates `test_smoke.py::test_craze_baked` — a comma list of the backends (`vz`, `fc`) whose `extensions` image was rebuilt with craze baked in. `test-integration-dev` sets `vz`, `test-integration-dev-fc` sets `fc` (the other backend still boots the published image); unset for plain `test-integration` (published images bake their release's craze, 0.9.0: 0.1.0, and the test asserts the pinned version). `SHED_CRAZE_VERSION` overrides the default, which the test reads from `craze-pin.env`'s `CRAZE_RELEASE` via `scripts/release/read-craze-pin.sh`. |
| `SHED_REMOTE_SERVER`   | _unset_                                            | Entry name for a shed-server running `default_backend: remote`. When unset every `[remote]` case skips. Set by `make test-integration-remote` (default `remote-dev`). |
| `SHED_REMOTE_LOG_PATH` | `~/.shed/dev-remote/server.log`                    | Local file the remote dev server logs to (its PhaseTimer lines). Set by `make test-integration-remote`. |
| `SHED_REMOTE_IMAGE`    | _unset_                                            | Image alias every `[remote]` create asks for. Unset, creates boot the server's `remote.default_image` (the vz/fc names `base` and `extensions` do not exist on a remote server). |
| `SHED_REMOTE_MAX_RUNNING` | `2`                                             | The server's `remote.limits.max_running`, for `test_remote_lifecycle.py::test_create_past_running_limit_is_refused`. |
| `SHED_IMAGE_HAS_HOME_DIRS` | _unset_                                            | Gates `test_smoke.py::test_home_dirs_owned_by_shed` — a comma list of the backends (`vz`, `fc`) whose `base` image was rebuilt with the shed-owned `~/.local`, `~/.config` and `~/.cache` dirs (#410). `test-integration-dev` sets `vz`, `test-integration-dev-fc` sets `fc` (the other backend still boots the published image); unset for plain `test-integration` (published images carry these dirs only from the 0.9.1 tag). |
| `SHED_ITEST_USERS`     | _unset_                                            | A comma list of the backends (`fc`, `remote`) whose dev server was STARTED with the itest user list (see [Named users](#named-users-authusers)). Gates the users cases on `[fc]` and `[remote]`, which skip saying what to set when it is missing; `[vz]` always restarts the Mac dev server with the list itself. `test-integration-dev-fc` and `-remote` set it from `SHED_DEV_USERS`. |
| `SHED_ITEST_USERS_DIR` | `~/.shed/dev/itest-users`                          | Where the itest users' keys live (generated once; a server is started with their public halves) and where each identity's throwaway `$HOME` is made. Must be the same directory the server's user list was rendered from. |
| `SHED_DEV_USERS`       | _unset_                                            | Makefile only. `itest` starts the dev servers (`dev-server-up`, `-up-fc`, `-up-remote` and their restarts) with the itest user list and tells `test-integration-dev-fc`/`-remote` so; empty means no user list (one implicit user). |

## Fixtures

- `shed_server` (params: `["vz", "fc", "remote"]`) — parameterized across
  the brew/deb-installed shed-server on each backend, plus the opt-in
  remote-backend server (`remote_server`, a `CloudServer`). This is what the
  existing test files use. A test only a local VM backend can run carries
  `@pytest.mark.local_vm_only("<reason>")`; its `[remote]` case skips with
  that reason before any server is probed.
- `shed_server_dev` (params: `["vz", "fc"]`) — parameterized across
  the PARALLEL dev shed-server on each backend. Skips per-backend
  when the corresponding `SHED_*_DEV_SERVER` env var isn't set. New
  tests targeting server-side changes against the developer's source
  tree use this fixture.

- `users_env` (module-scoped, params: `["vz", "fc", "remote"]`) — a dev
  server with the itest user list and three test identities enrolled
  against it (`fixtures/identity.py`; see [Named users](#named-users-authusers)).

The `vz_server_dev` / `fc_server_dev` sub-fixtures are session-scoped
mirrors of `vz_server` / `fc_server`, reading the `_DEV_` env vars.
The parallel-dev workflow (`make dev-server-up`, `test-integration-dev`)
lands in PR 2 (Mac) + PR 3 (FC remote) of the
`lets-defer-remote-mac-deep-brook` plan.

## Framework meta-tests

`test_framework_meta.py` covers the suite's own infra — fixture
availability probing, log-path fall-through, raw-SSH endpoint
resolution, the `template_fallback` per-shed-name marker, the
remote-file-vs-journald log read branch, and a brew-targeted
regression backstop. These don't need a live shed-server (mocked
subprocess), except the backstop which spawns a real
`test_create_delete_lifecycle[vz]` against brew and skips cleanly
when brew isn't reachable. Run them with:

```sh
cd tests/integration && uv run pytest -v test_framework_meta.py
```

## Layout

```
tests/integration/
  pyproject.toml          # uv-managed Python project + pytest config
  uv.lock                 # committed for reproducibility (gitignore exception)
  README.md               # this file
  conftest.py             # vz_server, fc_server, shed_server, test_shed_name
  test_smoke.py           # the MVP smoke tests
  test_http_exec.py       # HTTPS exec (FC dev server, mtls)
  test_service_client.py  # service clients end to end (FC dev server, mtls)
  test_create_mounts.py   # shed create --mounts (FC dev server)
  test_network_profiles.py # the isolated network profile (FC dev server, mtls)
  test_users.py           # named users on the API, SSH and the bus (users_env)
  test_users_upgrade.py   # sheds with no user: the upgrade step, shed assign (Mac dev server)
  fixtures/
    __init__.py
    server.py             # LocalServer + RemoteServer + ShedHandle
    timing.py             # PhaseTimer log-line parser
    devcontrol.py         # dev_config(), run_preflight(), bootstrap_cert()
    tlsclient.py          # pinned HTTPS: pinned_request, Pinned
    identity.py           # itest users: keys, the user list, Identity, users_environment
    busclient.py          # a synthetic credential-bus listener
```

## The MVP smoke tests

| Test                                  | What it asserts                                                    |
|---------------------------------------|--------------------------------------------------------------------|
| `test_create_delete_lifecycle`        | `shed create` succeeds; teardown deletes it cleanly.               |
| `test_phase_timer_emitted`            | Server log contains a `timing: create` line with the expected keys.|
| `test_repo_clone_https`               | `--repo <https url>` + `git log` round-trip in the guest works.    |
| `test_create_agent_p50`               | `agent` phase p50 (5 samples) stays under a per-backend ceiling. Skips when the VZ upper-template fast path was unavailable (in-guest mkfs cost inflates `agent_ms` by ~4 s). |
| `test_create_rootfs_template_present` | VZ-only: the host-side upper-template fast path is active. Skips on FC (no template path) and on VZ dev mode (where the fast path is unavailable by design). |
| `test_shed_exec_smoke`                | `shed exec name -- echo hello` returns `hello`.                    |

### The split timing gate

`test_create_agent_p50` and `test_create_rootfs_template_present`
replaced the single `test_plain_create_timing` gate. Both use the
server log line `[<shed-name>] upper template unavailable (...);
formatting in guest` (emitted from `internal/vz/orchestrator.go:249`
when the VZ host-side template clone is unavailable) as their
"dev-mode active" discriminator, exposed as
`ShedHandle.template_fallback`:

- `test_create_agent_p50` skips when the fallback was seen on any
  sample — the in-guest `mkfs.ext4` lands inside the agent phase and
  inflates `agent_p50` by ~4 s on VZ, which would fire the gate for
  a structural reason that's not a real regression.
- `test_create_rootfs_template_present` skips on FC entirely (no
  host-side template path; FC always uses in-guest mkfs and its
  agent ceiling already accommodates that cost — see
  `internal/firecracker/orchestrator.go:AllocateUpper`) and on VZ
  dev mode. On VZ release mode it asserts `rootfs_ms ≤ 100 ms` as a
  sanity check that the host-side clone actually happened fast.

This lets the suite run against either binary kind on either backend
without false-positive timing failures. The dev-build-isolation
rationale lives in `internal/version/buildtools.go:BuildToolsRefForTag`
(which returns `""` for any non-release version string by design) and
in the module-level comment in `tests/integration/test_smoke.py`.

## Per-backend timing ceilings

Defined in `fixtures/server.py:DEFAULT_AGENT_P50_MS`. Generous enough to
tolerate noise; tight enough to catch >200 ms regressions. Tighten over
time as §15 Phase 1 and Phase 2 land.

Today (post v0.5.6):

| Backend | `agent` p50 ceiling |
|---------|---------------------|
| `vz`    | 2200 ms             |
| `fc`    | 2100 ms             |

## Adding a test

```python
def test_my_thing(shed_server, test_shed_name):
    shed_server.create(test_shed_name, image="base")
    r = shed_server.exec(test_shed_name, ["uname", "-r"])
    assert r.returncode == 0
    assert "Linux" in r.stdout or "linux" in r.stdout
```

For a backend-specific test, mark it explicitly:

```python
@pytest.mark.fc
def test_only_on_fc(fc_server, ...):
    ...
```

(Markers are declared in `pyproject.toml` under
`[tool.pytest.ini_options].markers` — keep them in sync.)

## Validating server-side changes — parallel dev server (Mac VZ)

By default `make test-integration` runs against whichever `shed-server`
binary is currently *installed* on the host (brew on Mac, deb on
Linux). A server-side-only PR — orchestrator change, lifecycle
internals, backend handler refactor — can pass that suite without ever
exercising its own code, because the installed binary is the OLD one.

The mechanism for closing this gap on Mac is a **parallel dev
shed-server** that runs alongside the brew one on a different port
(mtls on HTTPS `18443`, SSH `12222`). The brew server keeps running
undisturbed; the dev server is what the suite targets.

```sh
# One-time setup, after the first `make dev-server-up`: register the dev
# server. First contact is over SSH and enrols an mtls client certificate,
# so there is no hand-written config.yaml entry.
shed server add localhost --ssh-port 12222 --name my-server-dev

# Per dev cycle:
make dev-server-up              # launches dev server (nohup, PID file)
make test-integration-dev       # runs suite against dev server (auto-ups if needed)

# ... edit source ...
make build && make dev-server-restart
make test-integration-dev

make dev-server-down            # when done
```

The dev server's lifecycle is intentionally simple — `nohup` writing
to `~/.shed/dev/server.log` with PID tracked at
`~/.shed/dev/server.pid`. No launchd plist, no auto-restart on crash
(crashes should be visible), no survives-reboot (re-up after reboot is
one command). State-dirs are isolated under
`~/Library/Application Support/shed-dev/vz/` so `shed image prune`
from the brew server never touches dev blobs.

The dev server's `SHED_BUILD_TOOLS_REF` is set inline to the latest
release tag so the dev binary uses release-shaped upper-template
behavior (sub-100 ms rootfs phase). `RELEASE_BUILD_TOOLS_REF`
defaults to the latest `git tag` matching `v*`; override to pin to
an older release if your source has drifted from `main`:

```sh
RELEASE_BUILD_TOOLS_REF=ghcr.io/charliek/shed-build-tools:v0.5.7 \
  make dev-server-restart
```

Lifecycle helpers:

- `make dev-server-up` — refuses to start if a dev server is already
  running (use `dev-server-restart`).
- `make dev-server-down` — graceful TERM with 5 s wait, then KILL.
  Idempotent (no-op with clear message if not running).
- `make dev-server-status` — running / reachable / log / config.
- `make dev-server-logs` — `tail -F ~/.shed/dev/server.log`.
- `make dev-server-restart` — down + up. Use this after `make build`
  to pick up a rebuilt binary.

Why this isn't a `launchd` plist: the dev server is intentionally
ephemeral. A crash should surface to the developer, not be silently
recovered. A reboot should require an explicit `make dev-server-up`,
not auto-start a possibly-stale binary. The PID-file lifecycle gives
us start / stop / restart semantics with zero Apple-specific machinery.

**Coexistence:** brew shed-server on port 8080/2222 + dev shed-server
on port 18443/12222. Both run simultaneously. The SSH host key
(`~/.shed/host_key`) is shared between them; the CLI's
`~/.shed/known_hosts` keys by `host:port` so each entry's
fingerprint is recorded independently.

## Validating server-side changes — parallel dev server (FC remote)

Same shape as the Mac VZ section above, but over SSH to
`$SHED_FC_HOST` (default `mini3`). The deb shed-server keeps running
undisturbed on `mini3:8080/2222`; the parallel dev FC server runs mtls on
`mini3:18443` (HTTPS) and `mini3:12222` (SSH).

```sh
# One-time setup, after the first `make dev-server-up-fc`: register the FC
# dev server (first contact is over SSH and enrols an mtls client
# certificate). An entry from when this server ran open is plain HTTP and
# does not heal itself: run `shed server remove mini3-dev` first.
shed server add mini3 --ssh-port 12222 --name mini3-dev

# Per dev cycle:
make dev-server-up-fc              # launches dev shed-server on mini3:18443
make test-integration-dev-fc       # runs suite against FC dev (auto-ups if needed)

# ... edit source ...
make build && make dev-server-restart-fc
make test-integration-dev-fc

make dev-server-down-fc            # when done
```

The FC dev server:

- Runs from `/tmp/shed-server-dev` via `sudo nohup` on the remote
  (needs root for FC's CAP_NET_ADMIN bridge/TAP operations). PID at
  `/tmp/shed-server-dev.pid`, log at `/tmp/shed-server-dev.log`.
- No systemd unit — same intentionally-ephemeral lifecycle as the
  Mac dev server. Crashes visible (not silently auto-restarted), no
  survives-reboot, re-up after reboot is one command.
- Isolated state-dirs under `/var/lib/shed-dev/firecracker/`: separate
  `images_dir`, `instance_dir`, `snapshots_dir`, `uppers_dir`,
  `socket_dir`. `shed image prune` from the deb server never touches
  dev blobs.

**FC-specific isolation:**

- **Different ports** (mtls HTTPS 18443 + SSH 12222 vs deb's 8080/2222).
- **Its own identity directory**: `state_dir: /var/lib/shed-dev/state` holds
  the dev server's SSH host key, TLS cert, mtls client CA and GitHub key cache,
  so the root dev server never writes to production's `/etc/shed`. The first
  `dev-server-up-fc` with this setting generates a new host key and TLS pin:
  remove only the stale `mini3-dev` entry and re-add it.
- **Its own nftables tables** (`shed_mini3_dev`, from the config's `name:`),
  installed only while an isolated shed exists. `make dev-server-down-fc`
  removes them with `shed-server network teardown` after stopping the server;
  the shared bridge and NAT rules are never touched.
- **Offset `vsock_base_cid: 600`** (deb default is 100). vsock
  allocation is in-memory per-server with no kernel check; two
  servers with the same base would collide on the first VM.
- **SHARED bridge + CIDR + tap_prefix** with the deb server.
  Kernel-level TAP existence check in
  `internal/firecracker/network.go:FindAvailableTAPIndex` coordinates
  cross-server. Known race window: two servers can both pick the
  same TAP index before either calls `LinkAdd`; the second call fails
  loudly with EEXIST. Diagnosable, not silent. For PR-validation
  workloads (one dev creating one shed at a time on the dev server
  while the deb server handles its own creates) this never fires.

**Assumed infrastructure** (same as the existing FC test path):

- Passwordless SSH from this host to `$SHED_FC_HOST`.
- Passwordless `sudo` for the SSH user. The recipes drive `install`,
  `cp`, `rm`, `nohup`, `tail`, `cat`, `stat`, `kill`, `ps`. The
  integration suite's existing journalctl read already requires
  sudo NOPASSWD for `journalctl`, so this is the same bar.
- The remote has FC + KVM available (which the deb shed-server
  already requires).

**Lifecycle helpers** (all mirror the Mac dev-server-* targets):

- `make dev-server-up-fc` — refuses if already running (use
  `dev-server-restart-fc`).
- `make dev-server-down-fc` — graceful TERM with 5 s wait, then
  KILL. Verifies the PID points at a `shed-server` process before
  signaling (so a stale PID file pointing at an unrelated process
  on the remote doesn't get killed). Idempotent.
- `make dev-server-status-fc` — running / reachable / log / config.
- `make dev-server-logs-fc` — `ssh -t mini3 "sudo -n tail -F
  /tmp/shed-server-dev.log"`.
- `make dev-server-restart-fc` — down + up. Use after `make build`
  to pick up a rebuilt binary on the remote.

```sh
# Pin a specific build-tools tag (default: latest git v* tag):
RELEASE_BUILD_TOOLS_REF=ghcr.io/charliek/shed-build-tools:v0.5.7 \
  make dev-server-up-fc

# Target a different remote:
SHED_FC_HOST=other-host make dev-server-up-fc
```

## Auth mode: mtls

Both parallel dev servers run `auth.mode: mtls` (client certificates issued
over the SSH `_bootstrap` channel, `RequireAndVerifyClientCert` on the HTTPS
listener): the committed `configs/server.dev-parallel.mac.yaml` and
`configs/server.dev-parallel.linux-fc.yaml` say so, and the dev targets run
them as written. One run covers everything, `test_mtls.py` and the FC-only
mtls modules included:

```sh
make dev-server-restart && make test-integration-dev          # Mac (VZ)
make dev-server-restart-fc && make test-integration-dev-fc    # Firecracker
```

**Registering the dev servers.** `shed server add` is SSH-first: it pins the
host key and enrols a client certificate over SSH before it touches HTTPS, so
it needs the dev SSH port and nothing else (the bootstrap reports the HTTPS
port):

```sh
shed server add localhost --ssh-port 12222 --name my-server-dev   # Mac
shed server add mini3 --ssh-port 12222 --name mini3-dev           # FC
```

An entry for the FC dev server from when it ran open is plain HTTP and does
not heal itself: `shed server remove mini3-dev`, then add it again. (An entry
written against a token-mode server heals itself on its next request.)

**One mode, no knob.** shed-server has two auth modes, `mtls` and `open`
(loopback only); `token`, its old spelling `secure`, and
`allow_insecure_exposure` no longer load at all. The `SHED_DEV_AUTH_MODE`
knob that used to pick a token-mode dev server is gone; a value left in the
environment makes the dev targets stop with one line saying so.

**Reconfiguring the Mac dev server.** Tests that need a different server
config go through `fixtures/devcontrol.py: dev_config()`, which restarts the
VZ dev server against the committed base merged with the test's overrides,
and always restores the base. After each restart it waits on
`scripts/dev-server-probe.sh` (the same probe `make dev-server-up` polls; it
needs no credential) and then one real `GET /api/info` in the config's own
mode: plain HTTP for an open config, and for an mtls config over the pinned
HTTPS listener with a client certificate enrolled over `_bootstrap` a moment
earlier (`bootstrap_cert`). So these tests run in the default run:
`test_tls.py`, `test_ssh_auth.py`, `test_network_surface.py` and
`test_harness_selfcheck.py`'s live cases. The open-mode ones pass
`devcontrol.OPEN_MODE_OVERRIDES` (`auth.mode: open`, no `https_port`): the
committed Mac config binds loopback and keeps `http_port: 18080` for exactly
this. Config mutation stays VZ-only: an open server is loopback only, and the
FC dev server runs on another host.

**Certificates of a chosen scope.** `bootstrap_cert(server, scope, dir)`
enrols a fresh P-256 key over `_bootstrap` the way every client does and
returns the certificate, its key and the server's TLS pin;
`fixtures/tlsclient.py: pinned_request` drives the HTTPS API with it, pinned
by fingerprint. `test_http_auth.py` (scope enforcement), `test_cred_bus.py`
(a forged `/respond` is dropped) and `test_bootstrap.py` (an off-list key
gets no certificate) use them against the running server without
reconfiguring it; the first two run on either dev server.

**Detection.** `test_mtls.py` cannot probe `GET /api/info` unauthenticated to
find out whether a server is in mtls mode — under mtls that endpoint is
unreachable full stop (the TLS listener refuses the handshake itself before
any HTTP request is even read, `RequireAndVerifyClientCert`), which is
precisely what `test_bare_tls_probe_without_client_cert_fails_before_http`
asserts. Both dev servers run mtls, so the module needs no gate; individual
tests assert against the *client entry's* cached `auth_mode` in
`~/.shed/config.yaml` (`internal/config/client.go: ServerEntry.AuthMode`) as
their real pass/fail signal. `LocalServer.server_version()` reads
`/api/info` with the entry's own client certificate
(`fixtures/server.py: client_cert_pair`), so the released-version gate works
against an mtls server too.

## Named users (`auth.users`)

A server with a user list gives every key to one named user; each user sees
and enters only their own sheds, and an admin may list, stop and delete any
shed but not enter another user's. The users cases drive one dev server as
several people at once, none of them with the developer's own key.

**The identities** (`fixtures/identity.py`). Keys are generated once under
`$SHED_ITEST_USERS_DIR` (default `~/.shed/dev/itest-users`, outside the
repo), because the server must be started with their public halves:

| Server user | Key | Role in the tests |
|---|---|---|
| `itest-alice` | `itest-alice/id_ed25519` | user A |
| `itest-alice` | `itest-alice/script_ed25519` (`script: true`) | user A's script key |
| `itest-bob` | `itest-bob/id_ed25519` | user B |
| `itest-bob` | `itest-bob/expired_ed25519` (`expires: 2000-01-01`) | an expired key |
| `itest-admin` | `itest-admin/id_ed25519` (`admin: true`) | an admin |
| `dev` | the config's own `auth.ssh` key sources | the developer, an admin |

Config load refuses `auth.ssh` key sources beside `auth.users`, so the user
list moves them under the admin user `dev`: the developer's entry, its
certificate and `dev_config()`'s readiness check keep working, as `dev`.

Each identity gets its own `$HOME` at `$SHED_ITEST_USERS_DIR/<user>/home-<entry>`
(its own `~/.shed/config.yaml`, client certificate and pinned `known_hosts`;
created per run, removed at teardown; the keys stay) and enrols with
`shed server add <host> --ssh-port <port> --name <entry> --trust-on-first-use`
under the developer's entry name. An `ssh` shim first on its `PATH` runs the
system ssh with `-F /dev/null -i <key> -o IdentitiesOnly=yes -o
IdentityAgent=none` and no `SSH_AUTH_SOCK`: the CLI runs ssh with no `-i`, so
without it the developer's agent or `~/.ssh/config` would offer the
developer's key first and a refusal could pass as the wrong person. Because
the shim drops `~/.ssh/config`, the dev entry's host must resolve without it
(an identity skips with that reason otherwise). `sftp` and `scp` run through
the shim with `-S`.

**Starting a server with the user list.**

```sh
make dev-itest-users                       # generate the keys (idempotent)
cd tests/integration && uv run python -m fixtures.identity users-yaml   # print the itest users

# Any base config (adds the users, moves auth.ssh's sources under `dev`):
scripts/render-dev-config.sh <base.yaml> <out.yaml> --users ~/.shed/dev/itest-users [--state-dir DIR]

# The dev targets do it with SHED_DEV_USERS=itest, and tell the suite:
SHED_DEV_USERS=itest make dev-server-restart-fc && SHED_DEV_USERS=itest make test-integration-dev-fc
SHED_DEV_USERS=itest make dev-server-restart-remote && SHED_DEV_USERS=itest make test-integration-remote PYTEST_ARGS=test_users.py
```

**Which server each backend uses** (`users_env` in `conftest.py`):

| Backend | Server | Needs |
|---|---|---|
| `[vz]` | the Mac dev server | nothing: the module restarts it with the user list through `dev_config()` and restores the committed base afterwards (always: other modules' `dev_config()` calls restore the base too, so a list the server was started with would be gone by then) |
| `[fc]` | `$SHED_FC_DEV_SERVER` | the server STARTED with the user list and `SHED_ITEST_USERS=fc`; skips saying so otherwise |
| `[remote]` | `$SHED_REMOTE_SERVER` | the server STARTED with the user list and `SHED_ITEST_USERS=remote`; skips saying so otherwise |

To run only the users cases against an FC dev server started by hand (for
example from a config rendered with `--users`):

```sh
cd tests/integration && PATH="$PWD/../../bin:$PATH" \
  SHED_VZ_SERVER= SHED_VZ_DEV_SERVER= SHED_REMOTE_SERVER= \
  SHED_FC_HOST=<host> SHED_FC_DEV_SERVER=<entry>-dev SHED_FC_DEV_LOG_PATH=<remote log> \
  SHED_FC_DEV_BIN_PATH=<remote binary> SHED_FC_DEV_CONFIG=<remote config> \
  SHED_ITEST_USERS=fc \
  uv run pytest -v -rs test_users.py test_service_client.py
```

| Module | Covers | Where it runs |
|---|---|---|
| `test_users.py` | Each user lists and sees only their own sheds (`shed list`, `/api/overview`), an admin sees all with their users; another user's shed is 404 on every API route and refused for a session, `shed exec`, sftp, scp and a forwarded port, and a stopped one is not woken; a taken name is one 409 for everyone, naming nobody; an admin may view, stop and delete another user's shed and is refused entry with the message; a non-admin is refused `local_dir`, server mounts, an image that is not an alias and the image store; snapshots belong to their user; a script key cannot take the credentials scope; an expired key opens nothing; a bus request from a user's shed reaches only that user's listener and only that user may answer it (a synthetic listener, `fixtures/busclient.py`); `shed assign` refuses a non-admin, an unknown user and an assigned shed | `[vz]`, `[fc]`; on `[remote]` only the ownership cases (one shed per user, two VMs): listing, the API 404s, session and exec refusals, the 409, the admin's refusal to enter, and the admin's stop and delete. The rest are `local_vm_only` there. |
| `test_users_upgrade.py` | Sheds made before a user list: with one user they become that user's at start; cutting a list from two users to one later hands out nothing (the one-time marker); with several users an admin assigns one with `shed assign`, a shed carrying server mounts is refused (409 `SHED_HOST_RESOURCES`), the user can then enter it and the admin no longer can; a key taken off the list opens nothing new | `[vz]` only: it restarts the server with other configs, which the suite can do only to the Mac dev server. The one-user restart gives every shed on the Mac dev server that has no user to `dev`, the developer. |
| `test_cred_bus.py::test_a_user_list_outside_its_rules_hard_fails` | A user list with an open server, or beside `auth.ssh` key sources, refuses to start (`run_preflight`, no running server) | Marked `vz` like the other preflight cases (they start `bin/shed-server` from the Mac dev config) |
| `test_service_client.py::test_users_and_the_client_do_not_reach_each_others_sheds` | A user cannot see or enter a service client's shed, an admin can enter it, and the client cannot see the user's shed | `[fc]` with `SHED_ITEST_USERS=fc` |

Not covered live: the server's `env_file` reaching only admins' sheds (it
only feeds a `--repo` clone's environment, which a test cannot observe
without a private repository; the unit tests in `internal/backend`,
`internal/api`, `internal/config` and `internal/vmutil` cover it).

## Remote backend (cloud sheds)

A server with `default_backend: remote` runs each shed as a cloud VM. The
`[remote]` cases run against it only when `SHED_REMOTE_SERVER` names its
`~/.shed/config.yaml` entry, which `make test-integration-remote` does:

```sh
make dev-server-up-remote           # once; see docs/development/remote-backend.md
make test-integration-remote        # every [remote] case; vz/fc variables set empty
make test-integration-remote PYTEST_ARGS="test_remote_refusals.py"   # no VMs
```

**Every case that creates a shed creates a billed VM.** The target is manual
only: CI and `make test-integration` never set the variable. A full run
creates about 25 VMs, one after another, each up for a minute or two, never more than
`remote.limits.max_running` at once, and takes about 42 minutes (measured: 24
VMs, about 90 s a case, most of it the create and the delete, which waits for
the VM to go). Check nothing is left afterwards with
`shed -s remote-dev list`.

| Runs on `[remote]` | Skips on `[remote]` (`local_vm_only`) |
|---|---|
| create and delete (`test_smoke`; `test_delete_fast` with a 120 s ceiling, because a remote `shed delete` returns when the VM is gone), `--repo` clone, exec, exec-shell, exec stderr, exec disconnect, the binary channel, stop/start (`test_lifecycle`), landing directory, background tunnels | host and server mounts (`test_mounts`, `test_stop_hook_mount`), the image store (`test_images`), guest MTU detection, the vz/fc boot-path timing tests, the vz/fc image-bake tests, the extensions image smoke |

Two remote-only modules:

- `test_remote_lifecycle.py`: `shed --json list` reports `id` and
  `backend: remote`; a file survives `shed stop` and `shed start`; a create
  past `max_running` is refused with `SERVER_LIMIT_REACHED` and leaves no shed.
  Two VMs with the default limit of 2.
- `test_remote_refusals.py`: a create asking for `local_dir`, `add_dirs`,
  `mounts`, `from_snapshot` or `egress` is 422 `FEATURE_UNSUPPORTED`; the
  snapshot, reset, image-pull and egress-set routes are 501
  `FEATURE_UNSUPPORTED`. Talks to the API over pinned mTLS (the CLI refuses
  these flags itself) and creates no VM.

Snapshot, reset and egress-profile behaviour has no `shed_server` case to
skip: those modules use the vz/fc dev fixtures and never reach a remote
server.

## Orchestrated sheds: exec, service clients, mounts, network profiles

Four modules cover HTTPS exec, service clients, `--mounts` and the network
profiles. They are Firecracker-only and use the `fc_server_dev` fixture, so they
can only ever reach the dev entry (`$SHED_FC_DEV_SERVER`), never a production
server:

| Module | Covers | Skips unless |
|--------|--------|--------------|
| `test_http_exec.py` | `POST /api/sheds/{name}/exec` on the wire: frames, exit codes, stdin, the 124 timeout and a client disconnect (the guest process is gone afterwards), 8 MiB intact, truncation, a 70 s quiet stream, 404 and 409, validation | the dev entry holds an mtls client certificate |
| `test_service_client.py` | A service client issued with `client issue itest` over SSH: owned sheds with no 9P mounts across stop/start, 403 and 404 refusals, admin visibility, the dev audit log's `api` and `exec.end` lines | the dev entry holds an mtls client certificate |
| `test_create_mounts.py` | `shed create --mounts none`, the `list -v`/`-vv`/`--json` columns, an unknown mount refused | (mode-independent) |
| `test_network_profiles.py` | `--network isolated` probed from inside guests, the client's shed isolated by default, the dev server's nftables tables (rebuild on restart, reapply after a manual delete, `dev-server-down-fc` removing only its two tables) | the dev entry holds an mtls client certificate |

They talk HTTPS themselves (stdlib `http.client`): the admin side with the CLI's
mtls credential for the dev entry (`~/.shed/creds/<entry>/client.pem` and
`client.key`), pinned by the entry's `tls_cert_fingerprint`; the client side
with the bundle from `client issue`, pinned by its `tls_cert_fingerprint`.
The committed FC dev config carries the `itest` client
(prefix `itest-svc-`, `max_sheds: 3`, `max_age: 2h`, images `[itest]`), and a
`nocturn` client the suite does not use (it is a development Nocturn's).

**The `itest` image alias.** Their sheds use the FC dev config's `itest` alias,
`ghcr.io/charliek/shed-fc-base:v0.9.0`, not the dev config's default v0.5.9
images: the exec semantics need a guest agent from about v0.7.10 on (it SIGHUPs
the command's process group when the host goes away, and sends stderr as its
own stream). The guest agent notices a lost host at its 15 s keepalive, so the
cleanup probes wait up to 45 s.

```sh
make build && make dev-server-restart-fc
make test-integration-dev-fc \
  PYTEST_ARGS="test_http_exec.py test_service_client.py test_create_mounts.py test_network_profiles.py"
```

The rule set behind `test_network_profiles.py` is proven first, as root in
network namespaces, by `make test-netns-remote` (see
`docs/development/testing.md`).

## Known-skipped tests

None by mark. The open-mode tests that used to skip unconditionally
(`skip_needs_open_mode_dev_server`) and the reconfiguring and bearer-token
tests that skipped under mtls (`skip_mtls_reconfigure`,
`skip_mtls_token_semantics`) were ported: the open-mode ones run the Mac dev
server as an open server on loopback through `dev_config()`, the rest run
against mtls with certificates. `test_token_ttl.py` became
`test_cert_ttl.py` (VZ-only: `auth.token_ttl` reaches the certificate the
bootstrap issues, which works at once and is refused once expired), and
`test_mtls.py::test_mode_flip_migrates_live` was deleted with the token mode
(there is no token server to flip from). Tests still skip for the usual reasons: a
backend or dev server that is not configured for the run, or a server older
than the feature (`skip_if_released_before`).

## What this suite is *not*

- Not a replacement for the Go unit tests (`make test`) — those run on
  every PR's GHA CI; this suite needs a real VM and runs locally or on
  the bare-metal release-validation host.
- Not the structural unit-ordering tests from PR #127 — those live in
  `internal/vmutil/guest_unit_ordering_test.go` and run on every PR.
  This suite is **additive** to them.
- Not (yet) wired into CI. The MVP runs locally. Hooking into the
  bare-metal half of the `Smoke (Linux)` workflow is a follow-up.
