# Testing

This document covers Shed's testing strategy, how to run each tier of tests, and the conventions used across the codebase.

## Test Tiers

| Tier | Scope | Tool | Requirements | CI |
|------|-------|------|--------------|-----|
| Unit | Pure logic, file-shape, ordering invariants | `go test` | Go toolchain | Yes |
| Integration suite (installed binary) | Live create-cycle via `shed` CLI against the brew/deb-installed shed-server | `pytest` + subprocess (uv-managed) | Reachable shed-server (local or remote); for FC: SSH to the host | No (locally + bare-metal release-validation) |
| Integration suite (dev binary, VZ) | Same, but targets a parallel dev shed-server running from the just-built source on the local Mac | `make dev-server-up` + `make test-integration-dev` | All of the above + a `~/.shed/config.yaml` entry for the dev server | No |
| Integration suite (dev binary, FC) | Same, but targets a parallel dev shed-server on the FC remote (alongside the deb one on a different port) | `make dev-server-up-fc` + `make test-integration-dev-fc` | SSH + sudo NOPASSWD on the remote + a `~/.shed/config.yaml` entry for the FC dev server | No |
| E2E (Firecracker, legacy) | Full VM lifecycle via API directly | `go test -tags=e2e` | KVM, root, Firecracker assets | No |

The **integration suite** (PR #132) is the recommended path for live create-cycle verification on both backends. The Go-tagged FC e2e tests predate it and remain available for low-level API exercise.

**Server-side changes require the dev-binary variant.** The plain `make test-integration` runs whatever binary is currently *installed* on the host, not your source — so a server-side-only change can pass the installed-binary tier without ever exercising the new code path. See [Validating server-side changes — parallel dev server](#validating-server-side-changes-parallel-dev-server) below for the workflow.

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
- For the parallel-dev variant (`make dev-server-up-fc` / `test-integration-dev-fc`): the deb shed-server keeps running under systemd at `/usr/local/bin/shed-server`; the dev shed-server runs from `/tmp/shed-server-dev` via `sudo nohup` (no systemd unit), with PID at `/tmp/shed-server-dev.pid` and log at `/tmp/shed-server-dev.log`. The SSH user needs passwordless `sudo` for the operations the dev-server-* recipes drive (`bash` — the launcher wraps in `sudo bash -c '...'` so the log redirect runs as root; plus `install`, `rm`, `tail`, `cat`, `stat`, `kill`). `sudo -n true` succeeding from the SSH session is a reasonable smoke test.

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

   If this prompts for a password, the suite's PhaseTimer-dependent FC tests can't fetch logs. Either keep your sudo cache warm during the run or add a NOPASSWD rule for `/usr/bin/journalctl` (and `/usr/bin/bash` + `/usr/bin/install` + `/usr/bin/cat` + `/usr/bin/rm` + `/usr/bin/kill` + `/usr/bin/tail` + `/usr/bin/stat` if you intend to use `make dev-server-up-fc`).

3. **Add the entry to `~/.shed/config.yaml`** on the dev workstation (or point at an existing entry with `SHED_FC_SERVER=<entry-name>`), then run the suite:

   ```bash
   SHED_FC_HOST=<your-host> make test-integration
   ```

   FC tests now run live against the installed binary. To validate against your source tree instead, see [Validating server-side changes](#validating-server-side-changes-required-for-server-side-prs) below.

#### Environment overrides

| Variable | Default | Effect |
|---|---|---|
| `SHED_VZ_SERVER` | `my-server` | `~/.shed/config.yaml` entry for the brew-installed VZ server. The `dev-server-*` and `test-integration-dev` Makefile targets use `SHED_VZ_DEV_SERVER` instead. |
| `SHED_VZ_LOG_PATH` | `/opt/homebrew/var/log/shed-server.log` | Where the local VZ server writes its log file. Override for Intel-Mac Homebrew (`/usr/local/...`) or custom installs. Set automatically by `make test-integration-dev` to the dev server's log file. |
| `SHED_VZ_DEV_SERVER` | `my-server-dev` | `~/.shed/config.yaml` entry for the parallel dev VZ server. Honored by the `dev-server-*` Makefile targets. |
| `SHED_FC_HOST` | `mini3` | SSH hostname for the FC server. Also honored by `make dev-server-up-fc` / `dev-server-status-fc` / `test-integration-dev-fc`. |
| `SHED_FC_SERVER` | same as `$SHED_FC_HOST` | `~/.shed/config.yaml` entry name for the deb FC server. |
| `SHED_FC_DEV_SERVER` | `$(SHED_FC_HOST)-dev` | `~/.shed/config.yaml` entry name for the parallel dev FC server. |
| `SHED_FC_LOG_PATH` | _unset_ (uses journald) | Remote file path for `fc_server` fixture to read logs from. `test-integration-dev-fc` sets this to the dev server's log file so the existing tests find PhaseTimer lines (the dev server runs via `sudo nohup`, not systemd). |
| `RELEASE_BUILD_TOOLS_REF` | latest `git tag` matching `v*` | shed-build-tools image ref injected into the dev binary so it uses release-shaped upper-template behavior. Pin to an older release if your source has drifted: `RELEASE_BUILD_TOOLS_REF=ghcr.io/charliek/shed-build-tools:v0.5.7`. |

#### Validating server-side changes — parallel dev server

`make test-integration` runs against whichever `shed-server` binary is currently *installed* on the host (brew on Mac, deb on Linux), not the source tree you're editing. A server-side-only change (orchestrator, lifecycle internals, backend handlers with no CLI-visible surface) can pass that suite without ever exercising its own code, because the installed binary is the OLD one.

The mechanism for closing this gap is a **parallel dev shed-server** that runs alongside the brew/deb one on a different port. The production server keeps running undisturbed; the dev server is what the suite targets when you run the dev-targeted Makefile chain.

**VZ (local Mac):**

```sh
# One-time setup per developer:
#   Add a ~/.shed/config.yaml entry for the dev server (snippet at the
#   end of this section), OR run `shed server add localhost --port 18080
#   --name my-server-dev` after the first `make dev-server-up`.

# Per dev cycle:
make dev-server-up              # launches dev shed-server on 18080/12222
make test-integration-dev       # runs suite against dev server (auto-ups if needed)
# ... edit source ...
make build && make dev-server-restart
make test-integration-dev
make dev-server-down            # when done
```

The dev server:

- Runs from `bin/shed-server` (the just-built dev binary) via `nohup` — no launchd plist, no auto-restart, no survives-reboot. Crashes are visible (not silently recovered); re-up after a reboot is one command.
- Listens on ports `18080` (HTTP) + `12222` (SSH), separate from the brew server's `8080` + `2222`. Both servers run simultaneously without conflict.
- Uses isolated state-dirs under `~/Library/Application Support/shed-dev/vz/` — separate `images_dir`, `instance_dir`, `snapshots_dir`, `uppers_dir`, `socket_dir`. `shed image prune` from the brew server never touches dev blobs, and vice versa.
- Has `SHED_BUILD_TOOLS_REF` set inline to the latest release tag, so the dev binary uses the release-shaped upper-template fast path (sub-100 ms rootfs phase) — the suite's `test_create_rootfs_template_present` test passes against the dev server when the env var is wired correctly.

**FC (remote Linux over SSH; default `$SHED_FC_HOST=mini3`):**

```sh
# One-time setup per developer:
#   shed server add mini3 --port 18080 --name mini3-dev  # after first dev-server-up-fc
#   (or manually add the entry to ~/.shed/config.yaml)

# Per dev cycle:
make dev-server-up-fc           # launches dev shed-server on mini3:18080 via sudo nohup
make test-integration-dev-fc    # runs suite against FC dev (auto-ups if needed)
# ... edit source ...
make build && make dev-server-restart-fc
make test-integration-dev-fc
make dev-server-down-fc         # when done
```

The FC dev server runs via `sudo nohup` on the remote (needs root for FC's CAP_NET_ADMIN bridge/TAP operations). No systemd unit — same intentionally-ephemeral lifecycle as the Mac dev server (crashes visible, no survives-reboot). Listens on `mini3:18080/12222` alongside the deb shed-server's `mini3:8080/2222`. Isolated state-dirs under `/var/lib/shed-dev/firecracker/`.

FC-specific isolation:
- **Offset `vsock_base_cid: 600`** (deb default is 100) so the two servers' vsock CIDs don't collide. vsock allocation is in-memory per-server with no kernel check.
- **Shared bridge + CIDR + tap_prefix.** Kernel-level TAP existence check in `internal/firecracker/network.go:FindAvailableTAPIndex` coordinates cross-server. Known race window: two servers can both pick the same TAP index before either calls `LinkAdd`; the second call fails loudly with EEXIST. Diagnosable, not silent. For PR-validation workloads (one dev creating one shed at a time on the dev server while the deb server handles its own creates) this never fires; for production-style concurrent stress it would need a server-side `retry-on-EEXIST` fix.

A server-side PR should open with **"`make test-integration-dev`: N/N pass against dev-build at commit `<sha>`"** (or `make test-integration-dev-fc` for FC changes). That sentence is then true and meaningful — not a brew/deb-binary alibi.

**`~/.shed/config.yaml` entry** to add for the parallel-dev VZ server:

```yaml
servers:
  my-server-dev:
    host: localhost
    http_port: 18080
    ssh_port: 12222
```

#### Validating pre-release: build-tools image changes

The parallel dev server uses `SHED_BUILD_TOOLS_REF` to pick which
`shed-build-tools` image mints the pre-formatted upper template ext4
on first VM create. By default that's the latest release tag
(resolved from `git tag --list 'v*'` at recipe time). To validate a
change to the `build-tools/` tree before cutting a release:

```sh
# Setup
# -----
# 1. Build the local shed-build-tools image. Tags it as
#    shed-build-tools:dev in the local Docker daemon.
make build-tools

# 2. Restart the dev server with the local image override.
#    Works on either platform (Mac VZ or FC remote).
RELEASE_BUILD_TOOLS_REF=shed-build-tools:dev make dev-server-restart
# or:
RELEASE_BUILD_TOOLS_REF=shed-build-tools:dev make dev-server-restart-fc

# 3. Run the suite (or a focused subset). The dev shed-server will
#    invoke shed-build-tools:dev for upper-template formatting.
make test-integration-dev   # or test-integration-dev-fc

# Teardown
# --------
# 4. Restart WITHOUT the override so the dev server goes back to the
#    latest-release tag for its next start (the env var is per-process,
#    not persisted; this is just to leave a clean dev state).
make dev-server-restart      # or dev-server-restart-fc

# 5. Optionally remove the local shed-build-tools:dev image to
#    reclaim ~145 MB:
docker image rm shed-build-tools:dev
```

How to confirm the override took effect: after a `shed create` the
PhaseTimer line should show `rootfs=` sub-100 ms (fast template
clone), AND the server log should NOT contain a
`[<shed-name>] upper template unavailable` line for the created
shed. If you see `rootfs=` in the multi-second range, or the
"unavailable" log line, the override didn't take and the dev binary
fell back to in-guest mkfs.

For an FC remote validation you'll need `shed-build-tools:dev`
loaded into the REMOTE's Docker daemon (the build-tools image is
invoked by shed-server on the remote host, not by the dev
workstation). Either push `shed-build-tools:dev` to a registry the
remote can pull from, or `docker save` on the dev workstation +
`docker load` on the remote across the SSH boundary.

#### Validating pre-release: base image changes (vz/firecracker Dockerfiles)

Base images (`shed-vz-base`, `shed-vz-extensions`, `shed-vz-full`,
and the FC counterparts) are built by
`scripts/build-vz-rootfs.sh` / `scripts/build-firecracker-rootfs.sh`,
which write OCI blobs + a tag pointer into an `OUTPUT_DIR`. The
default `OUTPUT_DIR` is the BREW/deb server's `images_dir`
(`~/Library/Application Support/shed/vz` for Mac VZ;
`/var/lib/shed/firecracker/images` for Linux FC) — which means a
naive local build lands in the brew/deb server's blob store, NOT the
parallel dev server's.

To make the local-built image visible to the dev server, override
`OUTPUT_DIR` to point at the dev server's `images_dir`:

**Mac VZ:**

```sh
# Setup
# -----
# 1. Build the variant you changed. OUTPUT_DIR is the key override —
#    it lands the blobs in the dev server's images_dir so `shed -s
#    my-server-dev create --image base` picks them up.
OUTPUT_DIR="$HOME/Library/Application Support/shed-dev/vz" \
  ./scripts/build-vz-rootfs.sh --variant base

# If your change is in build-tools too, pass --build-tools-version
# dev so the local shed-build-tools:dev mints the rootfs erofs:
OUTPUT_DIR="$HOME/Library/Application Support/shed-dev/vz" \
  ./scripts/build-vz-rootfs.sh --variant base --build-tools-version dev

# Note on shed-extensions changes: the build-vz-rootfs.sh script
# does NOT pass --shed-ext-version through to the docker build
# (its `--shed-ext-version` flag is informational only, per
# scripts/build-vz-rootfs.sh:201-211). To validate a shed-extensions
# bump, edit `ARG SHED_EXT_VERSION` in `vz/Dockerfile` directly
# before running the script, or invoke `docker buildx build
# --build-arg SHED_EXT_VERSION=<tag> ...` manually.

# 2. Restart the dev server (it picks up the new blobs automatically
#    — the OCI store is content-addressed so a `shed image ls` will
#    show the new tag pointing at the new manifest digest).
make dev-server-restart

# 3. Run the suite, or just a focused test that uses the variant.
make test-integration-dev
# or for a focused single test:
cd tests/integration && \
  SHED_VZ_SERVER=my-server-dev \
  SHED_VZ_LOG_PATH="$HOME/.shed/dev/server.log" \
  uv run pytest -v test_smoke.py::test_create_delete_lifecycle

# Teardown
# --------
# 4. Stop the dev server.
make dev-server-down

# 5. Optionally remove the locally-built blobs to reclaim disk (~250 MB
#    to ~750 MB per variant). The shed-dev/vz tree is the dev server's
#    images_dir; deleting it is safe because the brew server uses a
#    DIFFERENT directory (~/Library/Application Support/shed/vz).
rm -rf "$HOME/Library/Application Support/shed-dev/vz/blobs"
rm -rf "$HOME/Library/Application Support/shed-dev/vz/tags"
# Or: `shed -s my-server-dev image rm <variant>` for one variant
# at a time + `shed -s my-server-dev image prune` to GC orphaned
# blobs (the dev server's prune is isolated from the brew server's).
```

**FC remote:**

The build script needs to run on Linux. The cleanest path is to
build on the remote itself (or, if you have Docker buildx with
cross-build set up, build on Mac and ship the OCI tarball). No
one-command target for this yet; the manual sequence is:

```sh
# Setup
# -----
# 1. Build on the remote, writing blobs directly to the dev
#    images_dir. `sudo env OUTPUT_DIR=...` (not `OUTPUT_DIR=... sudo`)
#    is the load-bearing detail — sudo strips environment variables
#    by default; `sudo env VAR=value ...` is the standard way to
#    pass an env var through.
ssh $SHED_FC_HOST "cd /path/to/shed && \
  sudo env OUTPUT_DIR=/var/lib/shed-dev/firecracker/images \
    ./scripts/build-firecracker-rootfs.sh --variant base"

# 2. Restart the dev FC server so it sees the new blobs.
make dev-server-restart-fc

# 3. Run the suite.
make test-integration-dev-fc

# Teardown
# --------
# 4. Stop the dev FC server.
make dev-server-down-fc

# 5. Optionally remove the locally-built blobs on the remote.
ssh $SHED_FC_HOST "sudo rm -rf \
  /var/lib/shed-dev/firecracker/images/blobs \
  /var/lib/shed-dev/firecracker/images/tags"
```

A `make build-dev-image` / `build-dev-image-fc` Makefile helper that
wraps these flows is worth adding if these workflows become
frequent — for now the explicit `OUTPUT_DIR` override is the
load-bearing piece to know.

How to confirm the override took effect: `shed -s my-server-dev
image ls` (Mac) or `shed -s mini3-dev image ls` (FC) will list the
locally-built image with its new manifest digest. If you don't see
your change, `OUTPUT_DIR` didn't land the blobs in the dev server's
images_dir.

#### Performance validation against the released version

For changes that touch the boot path, agent dial, healthPoll, upper-allocation, mount, image-resolution, or any other hot path: **measure the impact on each platform the change affects, against the most recent release binary, before merging.**

The split timing gate (`test_create_agent_p50` + `test_create_rootfs_template_present`) is the floor — it fires on regressions around 500 ms or more (see `tests/integration/fixtures/server.py:DEFAULT_AGENT_P50_MS` for the per-backend ceiling; VZ has ~500 ms regression budget over its ~1550 ms median, FC has ~500 ms over its ~2400 ms median) — but a sub-threshold regression (or worse, a "no regression" that masks an actual gain that didn't materialise) won't trip it.

The workflow uses the parallel-dev pair on both sides of the comparison — no service interruption needed:

1. **Baseline against release.** `make test-integration` (it targets the brew/deb install). Record the agent_p50 and rootfs_ms (and total wall-clock) from the PhaseTimer line for each backend you're changing.
2. **Compare against your branch.** `make build && make dev-server-restart && make test-integration-dev` (it targets the parallel dev server running your source). Same measurements.
3. **Repeat on every affected backend.** A change shipping for both VZ and FC needs both backends measured — Apple Silicon vfkit and Linux KVM Firecracker have different floors, and the same code can be faster on one and slower on the other.
4. **Record the measurements in the PR body.** Hypothesised gains that don't show up are worth investigating before merge.

The v0.5.4 build-tools-ref regression (caught by a user noticing slow creates after `brew upgrade`, not by the suite) is the canonical example of "the binary built correctly but a config knob silently disabled the fast path." Measuring on the actual dev binary is what catches that class of bug before users do.

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
| `test_extensions_image_smoke` | The `extensions` image carries the shed-extensions binaries at the documented paths with the executable bit. Skips when no `extensions` alias is configured. |

`test_create_agent_p50` + `test_create_rootfs_template_present` are the **dynamic perf-regression gates** PR-time CI can't be (no `/dev/kvm` on GHA). The split replaced the single-gate `test_plain_create_timing` (renamed during PR #157) so the suite runs against either a brew/deb release binary or a `make build` dev binary without false-positive failures from the dev-build in-guest `mkfs.ext4` fallback. See the module-level comment in `tests/integration/test_smoke.py` for the split rationale, and `tests/integration/README.md` for the workflow guide.

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
