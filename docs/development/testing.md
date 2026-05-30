# Testing

This document covers Shed's testing strategy, how to run each tier of tests, and the conventions used across the codebase.

## Test Tiers

| Tier | Scope | Tool | Requirements | CI |
|------|-------|------|--------------|-----|
| Unit | Pure logic, file-shape, ordering invariants | `go test` | Go toolchain | Yes |
| Integration suite (installed binary) | Live create-cycle via `shed` CLI (both backends) | `pytest` + subprocess (uv-managed) | Reachable shed-server (local or remote); for FC: SSH to the host | No (locally + bare-metal release-validation) |
| Integration suite (dev binary) | Same, but runs the suite against the just-built source | `make test-integration-local` / `make test-integration-local-fc` | All of the above + brew shed on Mac (VZ), or SSH + sudo NOPASSWD on the remote (FC) | No |
| E2E (Firecracker, legacy) | Full VM lifecycle via API directly | `go test -tags=e2e` | KVM, root, Firecracker assets | No |

The **integration suite** (PR #132) is the recommended path for live create-cycle verification on both backends. The Go-tagged FC e2e tests predate it and remain available for low-level API exercise.

**Server-side changes require the dev-binary variant** (`make test-integration-local` / `-local-fc`). The plain `make test-integration` runs whatever binary is currently *installed* on the host, not your source — so a server-side-only change can pass the installed-binary tier without ever exercising the new code path. See [Validating server-side changes](#validating-server-side-changes-required-for-server-side-prs) below for the workflow.

## Running Tests

### Unit Tests

```bash
# All unit tests (skips linux-only tests on macOS)
make test

# Single package
go test ./internal/config/...

# With race detection
go test -race ./...
```

Firecracker unit tests (metadata, rootfs, networking) use the `linux` build tag and run automatically on Linux but are skipped on macOS:

```bash
# Firecracker unit tests (Linux only)
go test ./internal/firecracker/...
```

### Integration Suite (pytest)

Live create-cycle tests that drive a running `shed-server` via the `shed` CLI. The suite lives at `tests/integration/` and is the recommended path for verifying that the host-side and guest-side pieces actually fit together — boot timing, SSE events, `--repo` clone, `shed exec` round-trip, etc.

Architecture: pytest + subprocess (+ Fabric reserved for remote-orchestration tasks like deploying a dev binary), managed with [`uv`](https://docs.astral.sh/uv/). The decision rationale + the full architecture is captured in `docs/discovery/platform-runtime-optimization.md` §16.

#### Running the suite

```bash
# One-time: install uv (or `brew install uv`)
# https://docs.astral.sh/uv/getting-started/installation/

# From repo root:
make test-integration
```

That target verifies `uv` is on `PATH`, runs `uv sync` into a managed venv (gitignored), and invokes pytest with `-v`. Each test is parameterized over `["vz", "fc"]` and skips cleanly when its target backend isn't reachable from this host.

#### Per-backend requirements

**VZ (Apple Silicon mac):**

- A reachable shed-server, e.g., `brew services start shed`.
- `shed -s my-server list` succeeds. The entry comes from `~/.shed/config.yaml`.
- Server log at `/opt/homebrew/var/log/shed-server.log` (Homebrew default). Override with `SHED_VZ_LOG_PATH` for Intel-Mac (`/usr/local/...`) or custom install paths.

**Firecracker (default: `mini3` over SSH; override with `SHED_FC_HOST`):**

- SSH access to the host with `BatchMode=yes` (no interactive prompt). The default `mini3` is one example; any reachable Linux host with shed-server + KVM + Firecracker works.
- `shed -s <host> list` succeeds (the entry exists in `~/.shed/config.yaml` and the server responds).
- Passwordless `sudo -n journalctl -u shed-server` on the remote, for the PhaseTimer log-line fetch. Two tests skip cleanly if it's unavailable (you still get the others).
- **The remote shed-server must be v0.5.4 or newer** for the PhaseTimer-dependent assertions. PhaseTimer was added in PR #118 / v0.5.4; older servers cause those tests to skip with a clear reason while the rest of the suite still runs.
- For the dev-binary variant (`make test-integration-local-fc`): the remote shed-server runs under systemd as `shed-server.service`, the binary lives at `/usr/local/bin/shed-server` (the deb default; override via `FC_REMOTE_BIN_PATH=...`), and the SSH user has passwordless `sudo` for the operations the install/restore recipes drive (`systemctl`, `cp`, `install`, `mkdir`, `tee`, `rm`). `sudo -n true` succeeding from the SSH session is a reasonable smoke test. These are the same assumptions the deb-installed shed-server already makes.

#### Enabling FC tests on a fresh remote host (first-time setup)

The suite picks up FC tests automatically once the remote server emits PhaseTimer. To enable end-to-end FC verification on a new remote (`mini3` is the default; substitute your host wherever it appears):

1. **Install shed-server v0.5.4 or newer.** Every v0.5.4+ release publishes `shed-server_<version>_amd64.deb` and `shed-server_<version>_arm64.deb` to GitHub Releases. On the remote host:

   ```bash
   # Pick the arch matching the host (uname -m): x86_64 → amd64, aarch64 → arm64.
   ARCH=$(dpkg --print-architecture)
   # Fetch the latest release tag from GitHub (strips the leading `v`).
   VERSION=$(curl -fsSL https://api.github.com/repos/charliek/shed/releases/latest \
     | grep -m1 '"tag_name"' | sed -E 's/.*"v?([^"]+)".*/\1/')
   curl -L -o /tmp/shed-server.deb \
     "https://github.com/charliek/shed/releases/download/v${VERSION}/shed-server_${VERSION}_${ARCH}.deb"
   sudo dpkg -i /tmp/shed-server.deb
   sudo systemctl restart shed-server
   systemctl is-active shed-server     # active
   /usr/local/bin/shed-server --version
   ```

2. **Verify passwordless sudo for journalctl** (one-time per remote):

   ```bash
   sudo -n journalctl -u shed-server --since "1 minute ago" --no-pager
   ```

   If this prompts for a password, the suite's PhaseTimer-dependent FC tests can't fetch logs. Either keep your sudo cache warm during the run or add a NOPASSWD rule for `/usr/bin/journalctl` (and `/usr/bin/systemctl` + `/usr/bin/cp` + `/usr/bin/install` if you intend to use `make test-integration-local-fc`).

3. **Add the entry to `~/.shed/config.yaml`** on the dev workstation (or point at an existing entry with `SHED_FC_SERVER=<entry-name>`), then run the suite:

   ```bash
   SHED_FC_HOST=<your-host> make test-integration
   ```

   FC tests now run live against the installed binary. To validate against your source tree instead, see [Validating server-side changes](#validating-server-side-changes-required-for-server-side-prs) below.

#### Environment overrides

| Variable | Default | Effect |
|---|---|---|
| `SHED_VZ_SERVER` | `my-server` | `~/.shed/config.yaml` entry for the local VZ server. Also honored by `make install-local-server` / `make test-integration-local`. |
| `SHED_VZ_LOG_PATH` | `/opt/homebrew/var/log/shed-server.log` | Where the local VZ server writes its log file. Override for Intel-Mac Homebrew (`/usr/local/...`) or custom installs. |
| `SHED_FC_HOST` | `mini3` | SSH hostname for the FC server. Also honored by `make install-remote-server` / `make test-integration-local-fc`. |
| `SHED_FC_SERVER` | same as `$SHED_FC_HOST` | `~/.shed/config.yaml` entry name when it differs from the SSH host. |
| `FC_REMOTE_BIN_PATH` | `/usr/local/bin/shed-server` | Path to the shed-server binary on the FC remote. Override if the deb's install location moves or you've installed elsewhere. |
| `RELEASE_BUILD_TOOLS_REF` | latest `git tag` matching `v*` | shed-build-tools image ref injected into the dev binary so it uses release-shaped upper-template behavior. Pin to an older release if your source has drifted: `RELEASE_BUILD_TOOLS_REF=ghcr.io/charliek/shed-build-tools:v0.5.7`. |

#### Validating server-side changes (required for server-side PRs)

`make test-integration` runs against whichever `shed-server` binary is currently *installed* on the host (brew on Mac, deb on Linux), not the source tree you're editing. A server-side-only change (orchestrator, lifecycle internals, backend handlers with no CLI-visible surface) can pass that suite without ever exercising its own code, because the installed binary is the OLD one. This pattern silently masked coverage on PRs #151-156 — see [`docs/discovery/integration-suite-server-coverage.md`](../discovery/integration-suite-server-coverage.md) for the full motivation.

Two one-command targets close the gap:

**VZ (local Mac):**

```sh
make test-integration-local
```

That target builds the dev `bin/shed-server`, backs up the brew binary at `/tmp/shed-server-v<VERSION>.bak`, swaps the dev binary into the Cellar (codesigned ad-hoc so launchd will exec it), sets `SHED_BUILD_TOOLS_REF` via `launchctl`, restarts the brew service, runs the suite, then restores the brew binary. The restore runs unconditionally — a suite failure can't strand the host on the dev binary.

**FC (remote Linux over SSH; default `$SHED_FC_HOST=mini3`):**

```sh
make test-integration-local-fc
```

Cross-compiles `shed-server` for the remote's GOARCH (detected at recipe time via `ssh <host> uname -m` — `x86_64` → `amd64`, `aarch64` → `arm64`), `scp`s the dev binary, backs up the deb binary on the remote at `/tmp/shed-server-deb.bak`, swaps via `install -m 755`, writes a systemd drop-in at `/etc/systemd/system/shed-server.service.d/dev-override.conf` setting `Environment=SHED_BUILD_TOOLS_REF=<release-tag>`, `daemon-reload`s, restarts the service, runs the suite, then restores. The backup lives on the **remote** so a workstation reboot mid-test doesn't strand the remote in dev-binary state.

Both chained targets:

- Capture BOTH the suite exit code and the restore exit code, and report each separately if either fails. A restore failure is at least as serious as a suite failure (a stranded host vs. a normal red test).
- Snapshot pre/post state around the install step. If install fails AND your invocation didn't create new state (e.g., the preflight rejected because a prior manual install left a backup), the chain does NOT auto-restore — it leaves your existing dev install alone. If install fails AND it had started mutating state, auto-restore runs to clean up.
- Refuse to clobber an existing backup without `FORCE=1`, so `make install-X-server` followed accidentally by a second `make install-X-server` doesn't lose the original.

A server-side PR should open with **"`make test-integration-local`: N/N pass against dev-build at commit `<sha>`"** (or its `-fc` sibling) in the body. That sentence is then true and meaningful — not a brew/deb-binary alibi.

Standalone `make install-local-server` / `make install-remote-server` and `make restore-brew-server` / `make restore-remote-server` exist as targets too, for manual repro flows (e.g. running `shed create` against the dev binary by hand between the swap and the restore).

#### Performance validation against the released version

For changes that touch the boot path, agent dial, healthPoll, upper-allocation, mount, image-resolution, or any other hot path: **measure the impact on each platform the change affects, against the most recent release binary, before merging.**

The split timing gate (`test_create_agent_p50` + `test_create_rootfs_template_present`) is the floor — it fires on regressions around 500 ms or more (see `tests/integration/fixtures/server.py:DEFAULT_AGENT_P50_MS` for the per-backend ceiling; VZ has ~500 ms regression budget over its ~1550 ms median, FC has ~500 ms over its ~2400 ms median) — but a sub-threshold regression (or worse, a "no regression" that masks an actual gain that didn't materialise) won't trip it. The dynamic timing gate complements the unit tests; this manual per-platform measurement is the only safety net for changes whose value-add IS the timing characteristic.

The workflow:

1. **Baseline against release.** Run `make test-integration` (it picks up the brew/deb install). Record the agent_p50 and rootfs_ms (and total wall-clock) from the PhaseTimer line for each backend you're changing.
2. **Compare against your branch.** Run `make test-integration-local` and/or `make test-integration-local-fc` (it swaps in your dev binary with `SHED_BUILD_TOOLS_REF=<latest-tag>` so the comparison is apples-to-apples). Same measurements.
3. **Repeat on every affected backend.** A change shipping for both VZ and FC needs both backends measured — Apple Silicon vfkit and Linux KVM Firecracker have different floors, and the same code can be faster on one and slower on the other.
4. **Record the measurements in the PR body.** Hypothesised gains that don't show up are worth investigating before merge.

The v0.5.4 build-tools-ref regression (caught by a user noticing slow creates after `brew upgrade`, not by the suite) is the canonical example of "the binary built correctly but a config knob silently disabled the fast path." Measuring on the actual swapped-in dev binary is what catches that class of bug before users do.

#### Adding a test

Tests live in `test_smoke.py`. Use the `shed_server` fixture (parameterized over `["vz", "fc"]`) and `test_shed_name` (unique per-test name with autoteardown):

```python
def test_my_thing(shed_server, test_shed_name):
    shed_server.create(test_shed_name, image="base")
    r = shed_server.exec(test_shed_name, ["uname", "-r"])
    assert r.returncode == 0
    assert "Linux" in r.stdout
```

For a backend-specific test, mark it explicitly:

```python
@pytest.mark.fc
def test_only_on_fc(fc_server, ...):
    ...
```

(Markers are declared in `pyproject.toml` under `[tool.pytest.ini_options].markers` — keep them in sync.)

#### Suite layout

```
tests/integration/
  pyproject.toml          # uv-managed Python project + pytest config
  uv.lock                 # committed for reproducibility (gitignore exception)
  README.md               # operator notes; same content as this section, in-tree
  conftest.py             # vz_server, fc_server, shed_server, test_shed_name
  test_smoke.py           # core MVP tests × 2 backends
  test_lifecycle.py       # create → stop → start → exec → delete
  test_exec_shell.py      # ssh shell semantics + shed exec direct-argv
  test_timing_parser.py   # PhaseTimer parser unit tests (no live server)
  fixtures/
    server.py             # LocalServer + RemoteServer + ShedHandle
    timing.py             # PhaseTimer log-line parser
```

The core smoke tests in `test_smoke.py`:

| Test | What it asserts |
|---|---|
| `test_create_delete_lifecycle` | `shed create` succeeds; the shed appears in `shed list`; explicit `shed delete` removes it; absence verified. |
| `test_phase_timer_emitted` | Server log contains a `timing: create …` line with the expected phase keys. |
| `test_repo_clone_https` | `--repo` with an HTTPS URL clones the pinned octocat/Hello-World HEAD; `git rev-parse HEAD` in the guest matches. |
| `test_create_agent_p50` | `agent` phase p50 (5 samples) stays under a per-backend ceiling. Skips cleanly when the VZ upper-template fast path was unavailable (in-guest mkfs cost would inflate `agent_ms` for a structural reason that's not a real regression). |
| `test_create_rootfs_template_present` | VZ-only: the host-side upper-template fast path is active (`rootfs_ms ≤ 100 ms`). Skips on FC (no host-side template path) and on VZ dev mode (where the fast path is unavailable by design). |
| `test_shed_exec_smoke` | `shed exec <name> -- echo hello` returns `hello`. |
| `test_extensions_image_smoke` | The `extensions` image variant carries the shed-extensions binaries at the documented paths with the executable bit. Skips when no `extensions` tag is configured. |

`test_create_agent_p50` + `test_create_rootfs_template_present` are the **dynamic perf-regression gates** PR-time CI can't be (no `/dev/kvm` on GHA). The split replaced the single-gate `test_plain_create_timing` (renamed during PR #157) so the suite runs against either a brew/deb release binary or a `make build` dev binary without false-positive failures from the dev-build in-guest `mkfs.ext4` fallback. See the split-gate explanation in `tests/integration/README.md` and `docs/discovery/integration-suite-server-coverage.md` §7.

`test_lifecycle.py` adds a create → stop → start → exec → delete round-trip that catches `StartShed`-after-`StopShed` regressions the plain create/delete cycle can't see.

`test_exec_shell.py` (8 tests × 2 backends) exercises the SSH command channel: raw `ssh shed-name 'cmd | pipe'` gets the full shell via the server's `bash -lc` wrap, while `shed exec name -- cmd` ships argv literally without metacharacter expansion. Together they audit the [`shed exec` semantics](../reference/cli.md#shed-exec) end-to-end.

Plus parser unit tests in `test_timing_parser.py` covering the PhaseTimer log shape (real captured lines, duplicate-phase summation, "no keys after err=" guard, convenience properties).

### E2E (Firecracker, legacy)

Firecracker e2e tests exercise the full VM lifecycle: create, start, exec, stop, delete. They require KVM access, root privileges, and pre-built Firecracker assets.

```bash
# Build prerequisites
make build
sudo scripts/build-firecracker-rootfs.sh
scripts/download-firecracker.sh

# Run e2e tests
sudo go test -v -tags=e2e ./e2e/firecracker/...
```

!!! note "Why Firecracker e2e can't run in CI"
    GitHub Actions runners do not support KVM (no nested virtualization). Firecracker requires `/dev/kvm` and root privileges to launch microVMs. These tests must be run manually on a bare-metal Linux host or a VM with nested virt enabled.

## Build Tag Conventions

| Tag | Purpose | Example |
|-----|---------|---------|
| `//go:build linux` | Code that uses Linux-only APIs (vsock, TAP, etc.) | `internal/firecracker/*.go` |
| `//go:build e2e` | Tests requiring KVM + Firecracker | `e2e/firecracker/*_test.go` |

The `integration` build tag was previously used for Docker backend tests and may still appear in older branches.

All `internal/firecracker/` source and test files carry the `linux` build tag because they depend on Linux-specific syscalls (vsock, netlink).

## Test Patterns

### Table-Driven Tests

All test files use Go's standard table-driven pattern:

```go
tests := []struct {
    name    string
    input   string
    wantErr bool
}{
    {"valid", "my-shed", false},
    {"empty", "", true},
}

for _, tt := range tests {
    t.Run(tt.name, func(t *testing.T) {
        err := ValidateShedName(tt.input)
        if (err != nil) != tt.wantErr {
            t.Errorf("error = %v, wantErr %v", err, tt.wantErr)
        }
    })
}
```

### Modify Pattern for Config Validation

Config validation tests use a `modify func(*)` pattern to test individual field changes against a known-good baseline:

```go
cfg := validFirecrackerConfig()
tt.modify(cfg)
err := cfg.Validate()
```

### Test Helpers

The `internal/firecracker` package provides shared helpers in `testutil_test.go`:

| Helper | Purpose |
|--------|---------|
| `mustTempDir(t, prefix)` | Creates a temp directory with automatic cleanup |
| `testMetadata(name)` | Returns a valid `Metadata` with sensible defaults |
| `testFirecrackerConfig(tmpDir)` | Returns a valid `FirecrackerConfig` for testing |
| `createTestInstance(t, dir, name)` | Creates a complete test instance on disk |

### Conventions

- Place test files alongside the code they test (`foo.go` / `foo_test.go`)
- Use `t.Helper()` in all test helper functions
- Use `t.Cleanup()` for resource teardown instead of `defer` where possible
- Prefer `t.Fatalf` for setup failures, `t.Errorf` for assertion failures
- Use `os.MkdirTemp` with `t.Cleanup` for temporary directories

## Code Quality

```bash
# Run linter
make lint

# Format code
make fmt

# All checks (test + lint + fmt)
make check
```
