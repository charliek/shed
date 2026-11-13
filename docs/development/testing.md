# Testing

This document covers Shed's testing strategy, how to run each tier of tests, and the conventions used across the codebase.

## Test Tiers

| Tier | Scope | Tool | Requirements | CI |
|------|-------|------|--------------|-----|
| Unit | Pure logic, file-shape, ordering invariants | `go test` | Go toolchain | Yes |
| Integration suite (installed binary) | Live create-cycle via `shed` CLI against the brew/deb-installed shed-server | `pytest` + subprocess (uv-managed) | Reachable shed-server (local or remote); for FC: SSH to the host | No (locally + bare-metal release-validation) |
| Integration suite (dev binary, VZ) | Same, but targets a parallel dev shed-server running from the just-built source on the local Mac | `make dev-server-up` + `make test-integration-dev` | All of the above + a `~/.shed/config.yaml` entry for the dev server | No |
| Integration suite (dev binary, FC) | Same, but targets a parallel dev shed-server on the FC remote (alongside the deb one on a different port) | `make dev-server-up-fc` + `make test-integration-dev-fc` | SSH + sudo NOPASSWD on the remote + a `~/.shed/config.yaml` entry for the FC dev server | No |
| Network namespace (netpolicy) | The `isolated` profile's nftables rules, applied for real inside throwaway network namespaces | `make test-netns-remote` | SSH + sudo NOPASSWD on the FC remote, `nft` and `ip` there | No |
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

### Remote transport suite

The remote backend reaches a shed's agent through a transport link (`internal/remote/transport`). One conformance suite (`internal/remote/transport/conformance`) runs against every link, driving a real `shed-agent` the way the server does. It is part of `go test ./...`:

| Leg | Test | Needs |
|-----|------|-------|
| Local link (agent on unix sockets, no SSH) | `go test ./internal/remote/transport/locallink` | Linux |
| SSH link through an in-process SSH server | `go test ./internal/remote/transport/sshlink -run InProcess` | Linux |
| SSH link through a real OpenSSH `sshd` | `go test ./internal/remote/transport/sshlink -run SSHD` | Docker |
| SSH link against a real machine | `go test ./internal/remote/transport/sshlink/tests -run TestLive` | See below |

The `sshd` leg builds a container from `ubuntu:24.04` with `openssh-server`, the guest's `sshd` policy (`guest.SSHDConfig`) and an agent built from the source tree. It skips without Docker and with `-short`. Set `SHED_REQUIRE_SSHD=1` to turn a skip into a failure; CI does.

```bash
SHED_REQUIRE_SSHD=1 go test ./internal/remote/transport/... -count=1
```

The live check points the same suite at a real machine. It runs only when `SHED_LINK_HOST` is set:

```bash
SHED_LINK_HOST=<address or address:port> \
SHED_LINK_KEY_FILE=<OpenSSH ed25519 private key> \
SHED_LINK_HOST_KEY='ssh-ed25519 AAAA...' \
go test ./internal/remote/transport/sshlink/tests -run TestLive -count=1 -v
```

| Variable | Default | Effect |
|----------|---------|--------|
| `SHED_LINK_HOST` | _unset_ (the check skips) | Address of the machine's `sshd`, with or without a port. |
| `SHED_LINK_KEY_FILE` | _required_ | The client key. Unencrypted ed25519. |
| `SHED_LINK_HOST_KEY` | _required_ | The machine's ed25519 host key. It is pinned, never learned. |
| `SHED_LINK_USER` | `shed-link` | Login name. |
| `SHED_LINK_SOCKET_DIR` | `/run/shed-agent` | The agent's `--listen unix:<dir>`. |
| `SHED_LINK_HTTP_PORT` | `498` | The agent's `--http-port`. |
| `SHED_LINK_STREAM_MIB` | `16` | Size of the large-stream check. |
| `SHED_LINK_RESTART_CMD` | _unset_ | Local shell command that drops the machine's connections and brings `sshd` and the agent back. Without it the restart check is skipped. |

### Integration Suite (pytest)

Live create-cycle tests that drive a running `shed-server` via the `shed` CLI. The suite lives at `tests/integration/` and is the recommended path for verifying that the host-side and guest-side pieces actually fit together — boot timing, SSE events, `--repo` clone, `shed exec` round-trip, etc.

Architecture: pytest + subprocess (+ Fabric reserved for remote-orchestration tasks like deploying a dev binary), managed with [`uv`](https://docs.astral.sh/uv/). This guide is the canonical reference for the suite's design and operation.

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

   FC tests now run live against the installed binary. To validate against your source tree instead, see [Validating server-side changes](#validating-server-side-changes-parallel-dev-server) below.

#### Environment overrides

| Variable | Default | Effect |
|---|---|---|
| `SHED_VZ_SERVER` | `my-server` | `~/.shed/config.yaml` entry for the brew-installed VZ server. The `dev-server-*` and `test-integration-dev` Makefile targets use `SHED_VZ_DEV_SERVER` instead. Empty skips every `[vz]` case; `test-integration-dev-fc` sets it empty. |
| `SHED_VZ_LOG_PATH` | `/opt/homebrew/var/log/shed-server.log` | Where the local VZ server writes its log file. Override for Intel-Mac Homebrew (`/usr/local/...`) or custom installs. Set automatically by `make test-integration-dev` to the dev server's log file. |
| `SHED_VZ_DEV_SERVER` | `my-server-dev` | `~/.shed/config.yaml` entry for the parallel dev VZ server. Honored by the `dev-server-*` Makefile targets. |
| `SHED_FC_HOST` | `mini3` | SSH hostname for the FC server. Also honored by `make dev-server-up-fc` / `dev-server-status-fc` / `test-integration-dev-fc`. |
| `SHED_FC_SERVER` | same as `$SHED_FC_HOST` | `~/.shed/config.yaml` entry name for the deb FC server. |
| `SHED_FC_DEV_SERVER` | `$(SHED_FC_HOST)-dev` | `~/.shed/config.yaml` entry name for the parallel dev FC server. |
| `SHED_FC_LOG_PATH` | _unset_ (uses journald) | Remote file path for `fc_server` fixture to read logs from. `test-integration-dev-fc` sets this to the dev server's log file so the existing tests find PhaseTimer lines (the dev server runs via `sudo nohup`, not systemd). |
| `RELEASE_BUILD_TOOLS_REF` | latest `git tag` matching `v*` | shed-build-tools image ref injected into the dev binary so it uses release-shaped upper-template behavior. Pin to an older release if your source has drifted: `RELEASE_BUILD_TOOLS_REF=ghcr.io/charliek/shed-build-tools:v0.5.7`. |
| `SHED_DEV_STATE_DIR` | _unset_ | **Per-machine, optional.** Relocates the Mac dev server's blob-heavy state (`images_dir`, `instance_dir`, `snapshots_dir`, `uppers_dir`) under this directory — for a boot volume that cannot host a multi-GB dev image store. Unset means the committed default (`~/Library/Application Support/shed-dev/vz`), so no other developer is affected; set it in your shell profile (e.g. `export SHED_DEV_STATE_DIR=/Volumes/ext/shed-dev/vz`). `socket_dir` is deliberately NOT relocated (unix socket path length limits). `dev-server-up` refuses to start if the path is unmounted or unwritable, rather than silently building a second empty store on the mount point. |
| `SHED_DEV_AUTH_MODE` | `token` | `auth.mode` the parallel dev server (Mac or FC) is (re)started with — `token` (byte-identical to today's dev config) or `mtls` (renders a throwaway variant via `scripts/render-dev-config.sh`, never editing the committed base). Honored by `dev-server-up`/`-restart` and the `-fc` variants; forwarded by `test-integration-dev[-fc]` so `tests/integration/test_mtls.py` can tell which mode is running. See "Validating the mtls auth mode" below. |
| `SHED_MTLS_FLIP_TEST` | _unset_ | Opt-in for `test_mtls.py::test_mode_flip_migrates_live`, which restarts the real Mac dev server twice to prove the live auth-mode migration in both directions. Unset skips it. |

#### Validating server-side changes — parallel dev server

`make test-integration` runs against whichever `shed-server` binary is currently *installed* on the host (brew on Mac, deb on Linux), not the source tree you're editing. A server-side-only change (orchestrator, lifecycle internals, backend handlers with no CLI-visible surface) can pass that suite without ever exercising its own code, because the installed binary is the OLD one.

The mechanism for closing this gap is a **parallel dev shed-server** that runs alongside the brew/deb one on a different port. The production server keeps running undisturbed; the dev server is what the suite targets when you run the dev-targeted Makefile chain.

**VZ (local Mac):**

```sh
# One-time setup per developer:
#   Add a ~/.shed/config.yaml entry for the dev server (snippet at the
#   end of this section), OR run `shed server add localhost --port 18080
#   --ssh-port 12222 --name my-server-dev` after the first `make dev-server-up`.

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
#   shed server add mini3 --port 18080 --ssh-port 12222 --name mini3-dev  # after first dev-server-up-fc
#   (or manually add the entry to ~/.shed/config.yaml)

# Per dev cycle:
make dev-server-up-fc           # launches dev shed-server on mini3:18080 via sudo nohup
make test-integration-dev-fc    # runs suite against FC dev (auto-ups if needed)
# ... edit source ...
make build && make dev-server-restart-fc
make test-integration-dev-fc
make dev-server-down-fc         # when done
```

`PYTEST_ARGS` passes extra arguments to pytest in every `test-integration*` target, for example a subset of modules:

```sh
make test-integration-dev-fc SHED_DEV_AUTH_MODE=mtls PYTEST_ARGS="test_smoke.py test_mounts.py test_mtls.py"
```

`test-integration-dev-fc` is FC-only: it sets `SHED_VZ_SERVER` and `SHED_VZ_DEV_SERVER` empty, so every `[vz]` case skips. Without that the `[vz]` cases would reach the `my-server` entry, which on a Linux workstation can be a production FC server (mini3's deb server), and create and delete sheds there. An empty `SHED_VZ_SERVER` (or `SHED_FC_SERVER`) always means "no server for that backend", never the client's default server.

The FC dev server runs via `sudo nohup` on the remote (needs root for FC's CAP_NET_ADMIN bridge/TAP operations). No systemd unit — same intentionally-ephemeral lifecycle as the Mac dev server (crashes visible, no survives-reboot). Listens on `mini3:18080/12222` alongside the deb shed-server's `mini3:8080/2222`. Isolated state-dirs under `/var/lib/shed-dev/firecracker/`.

FC-specific isolation:
- **Its own identity directory.** The FC dev config sets `state_dir: /var/lib/shed-dev/state`, so the root dev server keeps its SSH host key, TLS cert, mtls client CA and GitHub key cache there instead of in production's `/etc/shed`. The first `make dev-server-up-fc` after this change generates a new host key and TLS pin: remove only the stale `mini3-dev` entry from `~/.shed/config.yaml` and re-add it with `shed server add mini3 --port 18080 --ssh-port 12222 --name mini3-dev` (leave the `mini3` entry and `default_server` alone).
- **An `itest` image alias** (`ghcr.io/charliek/shed-fc-base:v0.9.0`) for tests that need a newer guest agent than the v0.5.9 images (exec process-group cleanup, a separate stderr stream).
- **Five throwaway writable 9P mounts.** The FC dev config carries `claude`, `codex`, `opencode_data`, `opencode_state` and `gh` mounts mirroring a typical production mount set (same keys and targets). Their sources live under `/var/lib/shed-dev/mounts/` on the remote and are created by `make dev-server-up-fc` (derived from the uploaded config's `source:` lines). They hold no real credentials; they exist so the suite exercises the 9P credential-mount path (stop latency, nested mount targets).
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

Both dev configs already run with auth enforced (`auth.mode: token`, or `mtls`
per the knob below), so the entry above only registers the *connection*
details — it carries no credential. `shed server add localhost --ssh-port
12222 --name my-server-dev` (SSH-first: see the callout below) is what
actually bootstraps one; prefer it over hand-editing the file.

#### Remote backend (cloud sheds)

A dev server with `default_backend: remote` runs on your machine and creates one cloud VM per shed. Its setup (the private env file, `make dev-server-up-remote` and its siblings, the image bake) is on [Remote Backend Development](remote-backend.md). The suite's `[remote]` cases are opt-in and billed:

```bash
make test-integration-remote
```

#### Service clients, HTTPS exec and network profiles (FC, mtls)

Four modules cover the orchestrated-sheds work against the FC dev server. All
of them use the `fc_server_dev` fixture, so they can only reach the dev entry,
and they create their sheds from the dev config's `itest` alias
(`shed-fc-base:v0.9.0`, whose agent has the exec process-group cleanup and the
separate stderr stream):

| Module | Covers | Needs |
|--------|--------|-------|
| `test_http_exec.py` | `POST /api/sheds/{name}/exec`: frames, exit codes, stdin, the 124 timeout and the disconnect (both followed by the guest process being gone), 8 MiB intact, truncation, a 70 s quiet stream, 404, validation, 409 | mtls |
| `test_service_client.py` | A service client end to end: the bundle from `client issue` over SSH, owned sheds with no 9P mounts across stop/start, 403 and 404 refusals, admin visibility, the audit log's `api` and `exec.end` lines | mtls |
| `test_create_mounts.py` | `shed create --mounts none` and the `list -v`/`-vv`/`--json` columns, with a default create as the positive control | any mode |
| `test_network_profiles.py` | `--network isolated` probed from inside real guests, the service client's shed isolated by default, the dev server's tables in `nft`, rebuild at restart, reapply within 40 s of a manual delete, `dev-server-down-fc` removing only its two tables | mtls |

`scripts/render-dev-config.sh --mtls` gives the FC dev config an `itest` service
client (prefix `itest-svc-`, `max_sheds: 3`, `max_age: 2h`, images `[itest]`, no
server mounts, network isolated by default) and a `nocturn` client for a development
Nocturn's shed-craze Agent Provider (prefix `nocturn-`, `max_sheds: 4`, `max_age: 30h`,
network isolated, no server mounts, images `[itest]`; issue its bundle on the dev host
with `sudo -n /tmp/shed-server-dev client issue nocturn --config /tmp/shed-server-dev.yaml`).
Run them with:

```sh
make build && make dev-server-restart-fc SHED_DEV_AUTH_MODE=mtls
make test-integration-dev-fc SHED_DEV_AUTH_MODE=mtls \
  PYTEST_ARGS="test_http_exec.py test_service_client.py test_create_mounts.py test_network_profiles.py"
```

The audit log the service-client module reads is the dev server's
`/var/lib/shed-dev/firecracker/audit.jsonl` (read with `sudo -n cat`).

#### Proving the network rules in a namespace: `make test-netns-remote`

The `isolated` profile's rule set is tested for real, as root, by
`internal/netpolicy/netns_linux_test.go` (build tags `linux && netns`; it skips
unless it runs as root with `nft` and `ip`). It builds a bridge, veth pairs
standing in for TAPs, an "internet" and a "LAN" entirely inside
`shedtest-<pid>-*` network namespaces, runs every `nft` call through
`ip netns exec` (the test asserts it), and probes with its own binary
re-executed inside a namespace, so it never touches the host's ruleset or
links. It covers internet reachability, the blocked ranges with and without
`allow_cidrs`, host input except the shed's own ports, guest to guest,
a second source address, IP and MAC spoofing (each with a no-rules control),
IPv6, removing a guarded shed, and a teardown that leaves an unrelated table
alone.

```sh
make test-netns-remote      # cross-compiles, copies to $SHED_FC_HOST:/tmp, runs ^TestNetns under sudo, deletes it
```

Run it before any change to `internal/netpolicy`'s rendering reaches a real
host. The golden-file tests for the rendered script run in `make check`.

#### Applying new firewall rules to a remote host for the first time

A mistake in the rules can cut the remote host off (including SSH, when SSH
arrives over a network the rules match). The first time a changed rule set
reaches the FC remote's dev server, arm an automatic rollback that stops the
dev server (its 30 s presence check would otherwise put the tables back),
waits for it to exit (up to 35 s, its 30 s graceful shutdown included, then
SIGKILL), and deletes **only its own tables**, and only once the server is
gone (if it is somehow still alive, the tables are left alone). It signals
the PID only while that process is still the dev server: its command name must
be exactly `shed-server-dev` (the binary is `/tmp/shed-server-dev`), so a
stale PID reused by the production `shed-server`, or by anything else, is
never signalled and is treated as a server already gone. It refuses to
arm while another rollback is armed, so a cancel always reaches the live one:

```sh
ssh mini3 "sudo -n sh -c 'if [ -s /tmp/shed-dev-nft-rollback.pid ] && kill -0 -\$(cat /tmp/shed-dev-nft-rollback.pid) 2>/dev/null; then echo \"a rollback is already armed; cancel it first\" >&2; exit 1; fi; setsid nohup sh -c \"sleep 120; P=\\\$(cat /tmp/shed-server-dev.pid); case \\\$(ps -p \\\$P -o comm= 2>/dev/null) in shed-server-dev) kill -TERM \\\$P;; *) P=;; esac; i=0; while [ -n \\\"\\\$P\\\" ] && kill -0 \\\$P 2>/dev/null && [ \\\$i -lt 35 ]; do sleep 1; i=\\\$((i+1)); done; [ -n \\\"\\\$P\\\" ] && case \\\$(ps -p \\\$P -o comm= 2>/dev/null) in shed-server-dev) kill -KILL \\\$P;; esac; sleep 2; if [ -z \\\"\\\$P\\\" ] || ! kill -0 \\\$P 2>/dev/null; then nft delete table bridge shed_mini3_dev; nft delete table inet shed_mini3_dev; fi\" </dev/null >/dev/null 2>&1 & echo \$! > /tmp/shed-dev-nft-rollback.pid'"
```

Then, within the window: create one isolated shed on the dev server
(`shed -s mini3-dev create netiso --image itest --network isolated`), confirm
from your workstation that a **new** `ssh -o ConnectTimeout=5 mini3 true` works
and that the production and dev HTTPS ports still accept TCP, and cancel the
rollback by killing its whole process group (the `sleep` included):

```sh
ssh mini3 'sudo -n sh -c "kill -TERM -\$(cat /tmp/shed-dev-nft-rollback.pid) && rm -f /tmp/shed-dev-nft-rollback.pid"'
ssh mini3 'ps -ef | grep -v grep | grep "sleep 120"'   # expect nothing
```

If anything looks wrong, let the rollback fire. The table names come from the
dev config's `name:` (`mini3-dev` gives `shed_mini3_dev`). Never delete another
table, the bridge or the NAT rules: the dev server shares `shed-br0` with the
production server on the same host.

If the rollback fired, the test shed's VM is still running (stopping
shed-server never stops a Firecracker VM) and is **unguarded** until the dev
server starts again. Once the host is reachable, stop that shed's
`firecracker` process; or, once the rule change is fixed, start the dev server
(`make dev-server-up-fc SHED_DEV_AUTH_MODE=mtls`, which reinstalls the tables
from the sheds' metadata before it serves) and delete the shed.

`make dev-server-down-fc` runs `shed-server network teardown` for the dev
server after stopping it, so its tables never outlive it.

#### Validating the mtls auth mode

`auth.mode: mtls` — client certificates minted over the SSH `_bootstrap`
channel, `RequireAndVerifyClientCert` enforced on the HTTPS listener with
per-request re-validation — is exercised live by
`tests/integration/test_mtls.py`, against the parallel dev server, gated by a
Makefile knob:

```sh
SHED_DEV_AUTH_MODE ?= token   # default: byte-identical to today's dev config
```

Honored by `dev-server-up` / `dev-server-restart` (and the `-fc` variants):
`token` runs the committed `configs/server.dev-parallel.*.yaml` unmodified —
no generation step at all; `mtls` first renders a throwaway variant
(`scripts/render-dev-config.sh`, never editing the committed base) with
`auth.mode: mtls`. The mac config already carries the `https_port` +
`auth.ssh` key source mtls needs (only the mode line flips); the FC config
defaults to open mode with no `auth:` block at all, so the mtls render also
appends `https_port` + a `github_users` key source — the two invariants mtls
mode requires (`internal/config/server.go`).

**Two-config choreography:**

```sh
# 1. Regression pass in the default (token) mode — every other module.
make test-integration-dev                                # or -dev-fc

# 2. Flip the dev server to mtls, run just the mtls module.
make dev-server-restart SHED_DEV_AUTH_MODE=mtls           # or -restart-fc
SHED_DEV_AUTH_MODE=mtls make test-integration-dev         # or -dev-fc

# 3. Flip back (also the default):
make dev-server-restart                                   # or -restart-fc
```

**`shed server add` needs `--ssh-port` for the dev servers** — it is SSH-first
(host-key pin + credential bootstrap happen over SSH before HTTP is ever
touched), so:

```sh
shed server add localhost --ssh-port 12222 --name my-server-dev        # Mac
shed server add mini3 --ssh-port 12222 --port 18080 --name mini3-dev   # FC
```

**Detection.** The test module cannot probe `GET /api/info` to tell whether a
server is in mtls mode — under mtls that endpoint is unreachable outright
(the TLS listener refuses an uncertificated handshake before any HTTP request
is even parsed), which is exactly what
`test_bare_tls_probe_without_client_cert_fails_before_http` asserts. Instead
the whole module skips unless `SHED_DEV_AUTH_MODE` is exactly `mtls`
(forwarded by `test-integration-dev[-fc]`); individual tests additionally
assert against the *client entry's* cached `auth_mode` in
`~/.shed/config.yaml` (`internal/config/client.go: ServerEntry.AuthMode`) as
their real pass/fail signal.

**The mode-flip test is opt-in and VZ-only.**
`test_mode_flip_migrates_live` restarts the real Mac dev server twice
(token, then back to mtls) mid-test to prove the live migration in both
directions — more invasive than every other test in the module, which only
ever read the already-running dev server. Gated behind
`SHED_MTLS_FLIP_TEST=1`:

```sh
SHED_MTLS_FLIP_TEST=1 SHED_DEV_AUTH_MODE=mtls make test-integration-dev
```

**The full suite (`make test-integration-dev[-fc]` with no
`SHED_DEV_AUTH_MODE` override) is a TOKEN-mode regression run.** Nearly
every module outside `test_mtls.py` assumes the dev server's token-mode
invariants — bearer tokens exist, a plain-HTTP listener exists where
expected, `dev_config()`'s config-mutation round-trip works against the
committed base config — and doesn't read `SHED_DEV_AUTH_MODE` at all.
Running the suite against a server actually in `mtls` mode therefore mixes
two incompatible postures on purpose only in the sense that it's meant to
degrade gracefully: a documented set of tests **skip cleanly** instead of
failing, via two shared `pytest.mark.skipif` marks exported from
`tests/integration/fixtures/devcontrol.py`:

- `skip_mtls_reconfigure` — the test mutates the dev server's config via
  `dev_config()`/`bootstrap_mint()` for a scenario unrelated to bearer
  tokens (TLS pinning, SSH allowlist mechanics, the `dev_config()`
  round-trip test itself). Under mtls the server runs a *generated* config
  (`~/.shed/dev/server.mtls-generated.yaml`) rather than the committed base
  `dev_config()` expects, so reconfiguring would silently knock the live
  server out of mtls — `dev_config()` itself raises `RuntimeError` if
  called under mtls as a defense-in-depth rail, but the mark is what
  actually keeps the test from calling it in the first place.
- `skip_mtls_token_semantics` — the test's assertions are inherently about
  bearer-credential behavior (scoped HTTP tokens, TTL expiry,
  allowlist-gated minting, the open-mode single-plain-listener shape) that
  mtls mode has no equivalent of (short-lived client certs instead).

Guarded tests: `test_tls.py` (all four live tests), `test_ssh_auth.py`
(both), `test_harness_selfcheck.py::test_dev_config_roundtrips_override`,
`test_token_ttl.py::test_token_ttl_expiry`,
`test_http_auth.py::test_secure_mode_enforces_scoped_tokens`,
`test_bootstrap.py::test_bootstrap_mint_is_allowlist_gated`,
`test_cred_bus.py::test_cred_bus_forged_respond_dropped`, and
`test_network_surface.py::test_bus_and_connect_on_single_listener`. Running
`SHED_DEV_AUTH_MODE=mtls make test-integration-dev[-fc]` is thus **not** a
second full regression pass — it runs `test_mtls.py` plus whatever else in
the suite is mode-independent, with the tests above skipping by design; the
two-config choreography above (run once in each mode) is what gets full
coverage.

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
# SHED_SOURCE_REF must equal the dev config's image_aliases.<variant>
# (configs/server.dev-parallel.mac.yaml) — the build writes the ref-index
# entry for THAT ref, which is what `create --image <variant>` resolves.
# Without it the build lands under `...:dev`, the alias still names the
# published tag, and `create` quietly PULLS the published image instead
# (plan 023 hit exactly this). --build-tools-version <released tag> is
# needed on a host with no local shed-build-tools:dev.
SHED_SOURCE_REF=ghcr.io/charliek/shed-vz-base:v0.8.0 \
OUTPUT_DIR="$HOME/Library/Application Support/shed-dev/vz" \
  ./scripts/build-vz-rootfs.sh --variant base --build-tools-version v0.8.2

# If your change is in build-tools too, pass --build-tools-version
# dev so the local shed-build-tools:dev mints the rootfs erofs:
OUTPUT_DIR="$HOME/Library/Application Support/shed-dev/vz" \
  ./scripts/build-vz-rootfs.sh --variant base --build-tools-version dev

# Note on guest extension changes: the three guest binaries
# (shed-ext-ssh-agent, shed-ext-aws-credentials, docker-credential-shed)
# and the guest/extensions/etc/ overlay are built in-tree and
# staged into the build context by scripts/stage-guest-binaries.sh, which
# the build script calls automatically. There is no SHED_EXT_VERSION /
# --shed-ext-version any more — edit cmd/shed-ext-* or guest/extensions/etc/
# and rebuild the extensions (or full) variant to pick the change up:
SHED_SOURCE_REF=ghcr.io/charliek/shed-vz-extensions:v0.8.0 \
OUTPUT_DIR="$HOME/Library/Application Support/shed-dev/vz" \
  ./scripts/build-vz-rootfs.sh --variant extensions --build-tools-version v0.8.2

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
#    images_dir — as the NORMAL USER, not under sudo. Root's Docker has
#    no buildx builder that supports the OCI exporter the script needs
#    ("OCI exporter is not supported for the docker driver"), and mise
#    shims (Go for the guest-binary staging) are per-user. Make the dev
#    store user-writable once; the root-run dev server (sudo nohup)
#    reads user-owned blobs fine. (The .claude/skills/testing-vm-agent-
#    changes skill records the same trap.)
ssh $SHED_FC_HOST 'sudo chown -R $USER:$USER /var/lib/shed-dev/firecracker'
#    SHED_SOURCE_REF must equal image_aliases.<variant> in
#    configs/server.dev-parallel.linux-fc.yaml, for the same reason as on
#    the Mac (the ref-index entry is what `create` resolves).
ssh $SHED_FC_HOST 'cd /path/to/shed && export PATH="$HOME/.local/share/mise/shims:$PATH" && \
  SHED_SOURCE_REF=ghcr.io/charliek/shed-fc-base:v0.5.9 \
  OUTPUT_DIR=/var/lib/shed-dev/firecracker/images \
    ./scripts/build-firecracker-rootfs.sh --variant base --build-tools-version v0.8.2'

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
  test_http_exec.py       # HTTPS exec (FC dev server, mtls)
  test_service_client.py  # service clients end to end (FC dev server, mtls)
  test_create_mounts.py   # shed create --mounts (FC dev server)
  test_network_profiles.py # the isolated network profile (FC dev server, mtls)
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
| `test_extensions_image_smoke` | The `extensions` image carries the guest extension binaries at the documented paths with the executable bit. Skips when no `extensions` alias is configured. |

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
