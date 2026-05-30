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

| Variable          | Default                                            | Effect |
|-------------------|----------------------------------------------------|--------|
| `SHED_VZ_SERVER`  | `my-server`                                        | Entry name in `~/.shed/config.yaml` for the local VZ server. |
| `SHED_VZ_LOG_PATH`| `/opt/homebrew/var/log/shed-server.log`            | Where the VZ shed-server's log file lives. Override for Intel Macs (`/usr/local/...`) or custom installs. |
| `SHED_FC_HOST`    | `mini3`                                            | SSH hostname for the FC server (used for journald log fetch). |
| `SHED_FC_SERVER`  | same as host                                       | Entry name for the FC server (when it differs from the SSH host). |

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

## Validating server-side changes (VZ local)

By default `make test-integration` runs against whichever `shed-server`
binary is currently *installed* on the host (typically the brew
release). A server-side-only PR — orchestrator change, lifecycle
internals, backend handler refactor — can pass that suite without ever
exercising its own code, because the installed binary is the OLD one.

`make test-integration-local` closes that gap on the local Mac (VZ):

```sh
make test-integration-local
```

That target:

1. `install-local-server` — builds `bin/shed-server`, backs up the
   brew binary to `/tmp/shed-server-vN.M.K.bak`, swaps the dev
   binary into the brew Cellar, ad-hoc-codesigns it (launchd
   SIGKILLs unsigned binaries), sets `SHED_BUILD_TOOLS_REF` to the
   latest release tag (so the dev binary uses the release-shaped
   pre-formatted-template fast path instead of the in-guest mkfs
   fallback — see the split-gate explanation above), and restarts
   the brew service.
2. `test-integration` — runs the full suite against the dev binary.
3. `restore-brew-server` — restores the brew binary from the backup,
   clears the env var, restarts the brew service, removes the
   backup. Runs unconditionally (even if the suite fails).

Run `make install-local-server` and `make restore-brew-server`
standalone if you want to drive the suite manually between the swap
and restore (e.g. for an ad-hoc `shed create` repro against the dev
binary). Both are macOS-only and idempotent:

- `install-local-server` refuses to overwrite an existing backup
  without `FORCE=1` (so a developer who runs it twice doesn't lose
  the original).
- `restore-brew-server` is a no-op when no backup exists.

`RELEASE_BUILD_TOOLS_REF` defaults to the latest `git tag` matching
`v*`. Override to pin to an older release if your source has drifted
from `main`:

```sh
RELEASE_BUILD_TOOLS_REF=ghcr.io/charliek/shed-build-tools:v0.5.7 \
  make install-local-server
```

The FC remote (mini3 over SSH) sibling — `make
test-integration-local-fc` — will close the same gap on the
Firecracker backend. **Planned for a follow-up PR** (not yet in the
repo); tracked in `docs/discovery/integration-suite-server-coverage.md`
§5 Option 1b. Until it lands, validate FC server-side changes via the
manual swap on `$SHED_FC_HOST` (cross-compile +
`scp` + systemd unit override + restore — see the discovery doc for
the full sequence).

## What this suite is *not*

- Not a replacement for the Go unit tests (`make test`) — those run on
  every PR's GHA CI; this suite needs a real VM and runs locally or on
  the bare-metal release-validation host.
- Not the structural unit-ordering tests from PR #127 — those live in
  `internal/vmutil/guest_unit_ordering_test.go` and run on every PR.
  This suite is **additive** to them.
- Not (yet) wired into CI. The MVP runs locally. Hooking into the
  bare-metal half of the `Smoke (Linux)` workflow is a follow-up.
