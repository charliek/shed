# Testing

This document covers Shed's testing strategy, how to run each tier of tests, and the conventions used across the codebase.

## Test Tiers

| Tier | Scope | Tool | Requirements | CI |
|------|-------|------|--------------|-----|
| Unit | Pure logic, file-shape, ordering invariants | `go test` | Go toolchain | Yes |
| Integration suite | Live create-cycle via `shed` CLI (both backends) | `pytest` + subprocess (uv-managed) | Reachable shed-server (local or remote); for FC: SSH to the host | No (locally + bare-metal release-validation) |
| E2E (Firecracker, legacy) | Full VM lifecycle via API directly | `go test -tags=e2e` | KVM, root, Firecracker assets | No |

The **integration suite** (PR #132) is the recommended path for live create-cycle verification on both backends. The Go-tagged FC e2e tests predate it and remain available for low-level API exercise.

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

**Firecracker (default: `mini3` over SSH):**

- SSH access to the host with `BatchMode=yes` (no interactive prompt). Override the hostname with `SHED_FC_HOST`.
- `shed -s mini3 list` succeeds (the entry exists in `~/.shed/config.yaml` and the server responds).
- Passwordless `sudo -n journalctl -u shed-server` on the remote, for the PhaseTimer log-line fetch. Two tests skip cleanly if it's unavailable (you still get the other three FC tests).
- **The remote shed-server must be v0.5.4 or newer** for the PhaseTimer-dependent assertions (`test_phase_timer_emitted`, `test_plain_create_timing`). PhaseTimer was added in PR #118 / v0.5.4; older servers cause those two tests to skip with a clear reason while the rest of the suite still runs.

#### Enabling FC e2e on `mini3` (or any remote host)

The suite picks up FC tests automatically once the remote server emits PhaseTimer. The procedure to enable end-to-end FC verification on `mini3`:

1. **Upgrade the remote shed-server to v0.5.4 or newer.** The simplest path is to install the latest release's `.deb` — every v0.5.4+ release publishes `shed-server_<version>_amd64.deb` and `shed-server_<version>_arm64.deb` to GitHub Releases. On `mini3`:

   ```bash
   # On the remote host
   curl -L -o /tmp/shed-server.deb \
     https://github.com/charliek/shed/releases/download/v<VERSION>/shed-server_<VERSION>_amd64.deb
   sudo dpkg -i /tmp/shed-server.deb
   sudo systemctl restart shed-server
   systemctl is-active shed-server     # active
   /usr/local/bin/shed-server --version
   ```

2. **Verify passwordless sudo for journalctl** (one-time per remote):

   ```bash
   sudo -n journalctl -u shed-server --since "1 minute ago" --no-pager
   ```

   If this prompts for a password, the suite's PhaseTimer-dependent FC tests can't fetch logs. Either keep your sudo cache warm during the run or add a NOPASSWD rule for the `journalctl -u shed-server` invocation.

3. **Run the suite from the dev mac**:

   ```bash
   make test-integration
   ```

   All ten tests (5 × 2 backends) should now run live. Plain-create timing thresholds are in `tests/integration/fixtures/server.py:DEFAULT_AGENT_P50_MS` — tighten as future PRs land.

#### Environment overrides

| Variable | Default | Effect |
|---|---|---|
| `SHED_VZ_SERVER` | `my-server` | `~/.shed/config.yaml` entry for the local VZ server. |
| `SHED_VZ_LOG_PATH` | `/opt/homebrew/var/log/shed-server.log` | Where the local VZ server writes its log file. |
| `SHED_FC_HOST` | `mini3` | SSH hostname for the FC server (used for journald log fetch). |
| `SHED_FC_SERVER` | same as `SHED_FC_HOST` | `~/.shed/config.yaml` entry name when it differs from the SSH host. |

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
  test_smoke.py           # 5 MVP tests × 2 backends
  test_timing_parser.py   # 8 PhaseTimer parser unit tests (no live server)
  fixtures/
    server.py             # LocalServer + RemoteServer + ShedHandle
    timing.py             # PhaseTimer log-line parser
```

The five MVP tests:

| Test | What it asserts |
|---|---|
| `test_create_delete_lifecycle` | `shed create` succeeds; the shed appears in `shed list`; explicit `shed delete` removes it; absence verified. |
| `test_phase_timer_emitted` | Server log contains a `timing: create …` line with the expected phase keys. |
| `test_repo_clone_https` | `--repo` with an HTTPS URL clones the pinned octocat/Hello-World HEAD; `git rev-parse HEAD` in the guest matches. |
| `test_plain_create_timing` | `agent` phase p50 (5 samples) stays under a per-backend ceiling. The **dynamic perf-regression gate** PR-time CI can't be (no `/dev/kvm` on GHA). |
| `test_shed_exec_smoke` | `shed exec <name> -- echo hello` returns `hello`. |

Plus 8 parser unit tests in `test_timing_parser.py` covering the PhaseTimer log shape (real captured lines, duplicate-phase summation, "no keys after err=" guard).

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
