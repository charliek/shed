# shed integration test suite (`tests/integration/`)

Live integration tests that drive a running `shed-server` via the `shed`
CLI. Complements the pure file-parsing Go tests under `internal/`
(`make test`), which catch unit-file ordering regressions in CI; this
suite catches **live** create-cycle regressions that need a real VM.

Architecture: pytest + subprocess (+ Fabric later, only when needed for
remote-orchestration tasks). Managed with [`uv`](https://docs.astral.sh/uv/).
Background and decision rationale: see
`docs/discovery/platform-runtime-optimization.md` §16.

## Running

```sh
make test-integration
```

That target:

1. Verifies `uv` is installed (install via `brew install uv` or
   https://docs.astral.sh/uv/getting-started/installation/).
2. `uv sync` the Python deps into `tests/integration/.venv` (gitignored).
3. `uv run pytest -v` the suite.

Each test is parameterized over `["vz", "fc"]` and skips cleanly when its
target backend isn't reachable from this host. The session-scoped server
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
| `SHED_VZ_SERVER`       | `my-server`                                        | Entry name in `~/.shed/config.yaml` for the local VZ server. |
| `SHED_VZ_LOG_PATH`     | `/opt/homebrew/var/log/shed-server.log`            | Where the VZ shed-server's log file lives. Override for Intel Macs (`/usr/local/...`) or custom installs. |
| `SHED_FC_HOST`         | `mini3`                                            | SSH hostname for the FC server (used for journald log fetch). |
| `SHED_FC_SERVER`       | same as host                                       | Entry name for the FC server (when it differs from the SSH host). |
| `SHED_VZ_DEV_SERVER`   | _unset_                                            | Entry name for a PARALLEL dev VZ shed-server (alongside the brew one on a different port). When unset, the `vz_server_dev` fixture skips cleanly. Set by `make test-integration-dev`. |
| `SHED_VZ_DEV_LOG_PATH` | _unset_                                            | Where the parallel dev VZ shed-server writes its log file. Required when `SHED_VZ_DEV_SERVER` is set. |
| `SHED_FC_DEV_SERVER`   | _unset_                                            | Entry name for a PARALLEL dev FC shed-server (alongside the deb one on a different port). When unset, the `fc_server_dev` fixture skips cleanly. Set by `make test-integration-dev-fc`. |
| `SHED_FC_DEV_LOG_PATH` | _unset_                                            | Path on the FC remote where the parallel dev shed-server writes its log file. Required when `SHED_FC_DEV_SERVER` is set; the fixture reads it via `ssh + sudo -n tail -c +N` (offset-based) because the dev server runs as root via `sudo nohup` and is not under systemd. |
| `SHED_FC_LOG_PATH`     | _unset_ (uses journald)                            | Remote file path for the prod `fc_server` fixture to read logs from. When set, the fixture uses `ssh + sudo -n tail -c +N` against this file instead of journalctl. `make test-integration-dev-fc` sets this to the dev FC server's log file so the existing `shed_server`-using tests find PhaseTimer lines (the dev server isn't under systemd). |

## Fixtures

- `shed_server` (params: `["vz", "fc"]`) — parameterized across the
  brew/deb-installed shed-server on each backend. This is what the
  existing test files use.
- `shed_server_dev` (params: `["vz", "fc"]`) — parameterized across
  the PARALLEL dev shed-server on each backend. Skips per-backend
  when the corresponding `SHED_*_DEV_SERVER` env var isn't set. New
  tests targeting server-side changes against the developer's source
  tree use this fixture.

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
  fixtures/
    __init__.py
    server.py             # LocalServer + RemoteServer + ShedHandle
    timing.py             # PhaseTimer log-line parser
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
without false-positive timing failures. Background:
`docs/discovery/integration-suite-server-coverage.md` §7.

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
(`18080/12222`). The brew server keeps running undisturbed; the dev
server is what the suite targets.

```sh
# One-time setup: add a ~/.shed/config.yaml entry for the dev server.
# Either copy this snippet:
cat >> ~/.shed/config.yaml <<'EOF'
servers:
  my-server-dev:
    host: localhost
    http_port: 18080
    ssh_port: 12222
EOF
# Or run after the first `make dev-server-up`:
shed server add localhost --port 18080 --name my-server-dev

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
on port 18080/12222. Both run simultaneously. The SSH host key
(`~/.shed/host_key`) is shared between them; the CLI's
`~/.shed/known_hosts` keys by `host:port` so each entry's
fingerprint is recorded independently.

## Validating server-side changes — parallel dev server (FC remote)

Same shape as the Mac VZ section above, but over SSH to
`$SHED_FC_HOST` (default `mini3`). The deb shed-server keeps running
undisturbed on `mini3:8080/2222`; the parallel dev FC server runs on
`mini3:18080/12222`.

```sh
# One-time setup: add a ~/.shed/config.yaml entry for the FC dev server.
# Either copy this snippet:
cat >> ~/.shed/config.yaml <<'EOF'
servers:
  mini3-dev:
    host: mini3
    http_port: 18080
    ssh_port: 12222
EOF
# Or run after the first `make dev-server-up-fc`:
shed server add mini3 --port 18080 --name mini3-dev

# Per dev cycle:
make dev-server-up-fc              # launches dev shed-server on mini3:18080
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

- **Different ports** (18080/12222 vs deb's 8080/2222).
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

## What this suite is *not*

- Not a replacement for the Go unit tests (`make test`) — those run on
  every PR's GHA CI; this suite needs a real VM and runs locally or on
  the bare-metal release-validation host.
- Not the structural unit-ordering tests from PR #127 — those live in
  `internal/vmutil/guest_unit_ordering_test.go` and run on every PR.
  This suite is **additive** to them.
- Not (yet) wired into CI. The MVP runs locally. Hooking into the
  bare-metal half of the `Smoke (Linux)` workflow is a follow-up.
