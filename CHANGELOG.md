# Changelog

All notable changes to this project will be documented in this file.

## v0.4.4

- **`POST /api/sheds` clone failures now reach the SSE stream (#84):** When `git clone` failed inside a freshly-created shed, the failure was logged to journald via `log.Printf` but never emitted on the SSE stream — the API client saw `event: complete` while `/workspace` was actually empty. Both backends (`internal/vz/client.go`, `internal/firecracker/client.go`) now emit a `progress` event with `warning: true` on clone failure, and a `Repository cloned` `progress` event on success. The `repo` phase always has a terminal event regardless of outcome. Shed creation itself is still considered successful when only the clone fails — the shed VM is healthy, the operator can clone manually after fixing whatever — so the schema-additive `warning: true` field is non-breaking for existing SSE consumers. The journald log is preserved for sysadmins.
- **In-VM `~/.ssh/known_hosts` seeded before SSH `git clone` (#85):** PR #58 (v0.4.0) removed the `git_ssh` credential mount that previously supplied both keys and host trust. The shed-extensions ssh-agent forwarding replaces *keys*, but the agent protocol carries no host trust — so a clone of `git@github.com:...` failed immediately with `Host key verification failed` before key auth even ran. The server now writes `~/.ssh/known_hosts` into the VM via the agent (`umask 077`, owned `shed:shed`) before invoking `git clone`, but only when the URL is SSH-form (`git@host:path` or `ssh://...`) — HTTPS / git:// / http:// URLs skip this step. Built-in defaults bake GitHub's published host keys (ed25519, ecdsa, rsa) into the server binary so the common case works with no operator config. No image rebuild needed; `~/.ssh` already exists in v0.4.x rootfs from the existing Dockerfile `mkdir`.
- **New `git.extra_known_hosts` server config:** Optional list of `known_hosts`-format lines that operators can paste in to trust additional SSH hosts (GitLab, GitHub Enterprise, self-hosted Gitea, etc.). Always *additive* on top of the built-in GitHub defaults — OpenSSH treats multiple lines for the same host as any-match-wins, so there's no `disable_default_hosts` flag. Validation runs at server startup: each entry must have at least three whitespace-separated fields and a recognized SSH key type (`ssh-rsa`, `ssh-ed25519`, `ssh-dss`, `ecdsa-sha2-nistp256/384/521`); malformed entries fail server startup with a clear error pointing at the bad index. Generate entries by running `ssh-keyscan <host>` on a trusted machine and pasting the output. Documented in `docs/reference/configuration.md` (new `## Git` section); commented example in `configs/server.example.yaml`. If GitHub or another host rotates keys, operators can extend trust via config without waiting for a release.
- **URL credentials no longer leak into SSE stream or server log:** Defense-in-depth follow-up applied during PR review. The SSE warning emits a fixed-string message (`Failed to clone repository (see server logs for details)`) — never `req.Repo` and never the wrapped `err`, since either could carry credentials from URLs like `https://user:pw@host/repo.git` (or from git/ssh stderr if a future refactor surfaces it into err). The server-side `log.Printf` line still logs the URL — operators need "which repo failed" to debug — but routes it through the new `config.SanitizeRepoURL` helper which strips just the password component from the URL's userinfo while preserving the username, scheme, host, and path. SSH-form URLs (`git@host:path`) and shorthand pass through unchanged.
- **Tests:** `TestCreateShed_SSE_SurfacesProgressAndWarning` (asserts `warning: true` propagates from backend through SSE handler, and that the warning message contains no URL fragments — `git@`, `://`); `TestGitConfigValidate` (table-driven, 11 cases covering valid ed25519/ecdsa/rsa, empty/whitespace/single-field/two-field/unknown-type rejections); `TestBuildKnownHosts_*` (nil cfg, nil git, additivity, exact-match dedupe, CRLF trimming); `TestIsSSHRepoURL` and `TestSanitizeRepoURL` (table-driven URL classification and password stripping). End-to-end validated on local Firecracker — HTTPS clone path emitted the new success event, SSH clone path wrote a 0600 shed:shed-owned `known_hosts` containing all three GitHub defaults plus a configured `gitlab.com` extra, and a malformed config was rejected at server startup. No Dockerfile / rootfs / agent-protocol changes — server-only release; v0.4.x rootfs images work unchanged.

## v0.4.3

- **Snapshot orphan reclamation:** `shed system prune --orphans` now reclaims partial snapshot directories left behind when a host crashes between `rootfs.ext4` and the atomic `snapshot.json` rename. Pre-fix these dirs were invisible to `shed snapshot list` and unreachable to prune; operators had to `rm -rf` manually. Race-safety is via a `.creating` marker dropped (durably, with `syncDir`) at the start of `CreateSnapshot` and removed via `defer` on every exit path. Stale markers (>24h) are treated as crash residue and the dir is reclaimed. Stat errors are fail-closed: a permission/transient I/O failure on either `snapshot.json` or `.creating` reports a `SkippedItem` rather than enqueuing the dir for deletion. Adds the `snapshot_orphan` kind to `FileEntry` / `PrunedItem`.
- **`shed system df` totals:** `SnapshotDiskEntry` now carries an `OtherFiles` slice (mirroring `ShedDiskEntry`), and both backends stat `snapshot.json` alongside the rootfs and add its bytes into the per-snapshot `Total`. CLI counts replace the hardcoded `len(snapshots) * 2` with a sum over `rootfs + OtherFiles`. The "snapshots and sheds spawned from them share extents via reflink" note is reworded so it no longer implies metadata bytes are shared.
- **`--from-snapshot` validation:** Mutual exclusion against `--image` / `--repo` is now wrapped in a new `ErrInvalidShedRequestSentinel` and routed through `mapBackendError` to a uniform 400 INVALID_REQUEST. The API handler keeps `ValidateSnapshotName` and the mutex check pre-SSE (so the SSE path doesn't surface a 200 + streamed error). Adds backend unit tests verifying the wrapped sentinel is returned.
- **`internal/lockmap` (new internal package):** `NamedMutexMap` consolidates the four duplicated per-name mutex maps (`createMu`/`createLocks` + `snapshotMu`/`snapshotLocks` × 2 backends). Zero-value-safe so existing tests that use `Client{}` continue to work without changes. The field rename `createLocks` → `shedLocks` matches the broader semantics already documented (Create/Start/Stop/Delete/snapshot-source — not just Create). Wrapper methods on `Client` are kept so callsites and lock-order docstrings stay put.
- **`TestHandleConnectSuccess` flake fix:** `handleConnect` now wraps the hijacked `clientConn` with `vmutil.BufferedConn` (mirroring the existing pattern in `internal/tunnels/connect.go`) so any bytes the HTTP server pre-buffered past the request headers aren't stranded. Eliminates the "read from VM side: unexpected EOF" failure that intermittently tripped CI on unrelated PRs.
- **Tests:** New `internal/firecracker/system_prune_test.go` (FC had no prune coverage before); new `TestPrune_SnapshotOrphans` table-driven harnesses in both backends; `internal/lockmap/lockmap_test.go` covers serialization + zero-value usage. No image/Dockerfile/rootfs changes — server-only release.

## v0.4.2

- **Snapshots — machine-id regeneration:** Every shed (fresh-create AND snapshot-spawn) now gets a unique `/etc/machine-id` at every VM boot. Pre-fix, `dbus`'s postinst baked a single UUID into the rootfs at Docker build time, so all sheds inherited it; spawning multiple sheds from one snapshot collided on identity. Fixed in both rootfs Dockerfiles via systemd's "transient machine-id" pattern: `/etc/machine-id` is a symlink to `/run/machine-id` (tmpfs), `/var/lib/dbus/machine-id` symlinks to `/etc/machine-id`, and `systemd-machine-id-commit.service` is masked so systemd doesn't replace the symlink with a regular file. PID 1 generates a fresh UUID per boot; nothing persists to disk. **Behavior change:** machine-id regenerates on every boot of the same shed, not just first boot. For applications that key persistent state on machine-id and expect it stable across reboots, recreate the shed instead of stop+starting it. Documented in `docs/reference/snapshots.md`.
- **Snapshots — SSH host key comment:** `shed-firstboot` now sets `/etc/hostname` BEFORE running `ssh-keygen -A`, so cloned sheds' SSH host keys carry the spawn's hostname (e.g. `root@my-spawn`) in the comment field rather than the source's.
- **Snapshots — internal cleanup:** `shed-firstboot` no longer touches machine-id (the previous `truncate + systemd-machine-id-setup` flow was broken — it pulled the source's value back from `/var/lib/dbus/machine-id`). With the rootfs symlink in place, machine-id is handled cleanly by systemd alone.

## v0.4.1

- **Snapshots:** Drop `ConditionVirtualization=vm` from `shed-firstboot.service`. In v0.4.0 the unit was loaded and enabled but never ran on snapshot-spawned sheds because `systemd-detect-virt` returns `docker` inside the Docker-built rootfs (container-y artifacts in `/` confuse detection), and the condition blocked the boot. The shed-firstboot binary already short-circuits when `/proc/cmdline` has no `shed.name=`, so the systemd-side gate was redundant. After this fix, snapshot-spawned sheds get fresh SSH host keys and the correct hostname on first boot. (The machine-id PID 1 caching caveat from `docs/reference/snapshots.md` still applies — fix is queued for a follow-up.)

## v0.4.0

- **Snapshots (new feature):** `shed snapshot create|list|info|delete` plus `shed create --from-snapshot <name>` on both backends (#81). A snapshot captures a stopped shed's rootfs as a named, immutable artifact (mode `0o444`) under a separate `snapshots_dir`; new sheds spawn from it via reflink (APFS clonefile / FICLONE) with their own writable rootfs. Snapshots survive deletion of the source shed and show up in `shed system df` with a reflink double-count note. Mutually exclusive with `--image` and `--repo`; `--local-dir` and credential mounts compose. Snapshot create of a `--local-dir`-backed shed surfaces a warning that workspace contents are not captured. See [`docs/reference/snapshots.md`](docs/reference/snapshots.md).
- **Snapshots — in-guest identity regeneration:** New `shed-firstboot` oneshot service (sysinit.target, before D-Bus / journald / sshd / shed-agent) regenerates `/etc/machine-id`, SSH host keys, and hostname when a cloned rootfs is detected (recorded shed name in `/var/lib/shed/identity.json` mismatches the boot-time `shed.name=` cmdline arg). Backends now append `shed.name=<name>` to kernel cmdline. Idempotent across normal restarts.
- **Snapshots — lifecycle lock broadening (internal behavior change):** `acquireCreateLock` is now a per-shed-name lifecycle lock taken by `Create`, `Start`, `Stop`, `Delete`, and `CreateSnapshot` of a shed-as-source. This closes TOCTOU races between snapshot-of-stopped-shed and concurrent Start/Delete of the same source. New separate `acquireSnapshotLock` keyspace serializes `CreateSnapshot` / `DeleteSnapshot` / `CreateShed --from-snapshot` for the same snapshot name. Lock-order rule: `snapshotLock -> createLock` (no AB-BA cycle). `DeleteShed` of a running shed uses an internal `stopShedLocked` helper to avoid re-entering the non-reentrant mutex.
- **`shed system df` (new):** New `shed system df` and `shed system df --verbose --all` for per-server disk usage reporting (#80). Categories: images (kernel/initrd/cached variants), sheds (rootfs + console logs + sidecars), snapshots, orphans. Returns both logical (apparent) and physical (block) bytes; `Notes` flag APFS clonefile and reflink double-counting.
- **`shed system prune` (new):** Scoped cleanup pass with `--scope images|instances|logs|orphans`, `--until <duration>`, `--dry-run`, and per-server `--all`. Age-based instance prune uses `mtime(metadata.json)` as the "last touched" proxy.
- **Reflink rootfs copies:** `CopyRootfs` now uses `clonefile(2)` on darwin/APFS and `FICLONE` (with `copy_file_range` and `io.Copy` fallbacks) on linux. `shed create` is near-instant and near-zero physical cost on supported filesystems; falls back transparently otherwise.
- **`CopyRootfs` writable-by-default:** All clone strategies now produce a `0o644` instance rootfs regardless of the source's mode. Required for spawn-from-snapshot (snapshot rootfs is `0o444` immutable) and a defensive no-op everywhere else.
- **Image cache:** `LinkCachedImage` auto-cleans stale `.tmp` orphans before retrying (#78); fixes a class of "ENOSPC after a previous interrupted conversion" failures.
- **Docs:** New [`docs/reference/snapshots.md`](docs/reference/snapshots.md) and [`docs/reference/disk-management.md`](docs/reference/disk-management.md). Snapshot subcommands added to the CLI reference.

## v0.3.5

- **Image cache:** Fix `shed-server pull-images` skipping `_base-rootfs.ext4` when `base_rootfs` shared a Docker ref with an `images:` entry. `pull-images` now hardlinks `_base` to the matching variant (zero extra disk) so the first subsequent `shed create` (no `--image`) is immediate instead of triggering an unexpected ~60s lazy pull. Verified on Firecracker (ext4) and VZ (APFS).
- **Image cache:** Make `shed image prune` source-aware for every Docker-ref entry (variants and `_base`). After a config ref bump (e.g. v0.3.3 → v0.3.4) stale `.ext4` caches whose `.source` sidecar no longer matches the current config ref are now reclaimed; previously they survived prune and had to be `sudo rm`'d manually.
- **Image cache:** Fix local-path exclusion in prune so a configured path like `images.prod: /var/lib/shed/firecracker/images/base-rootfs.ext4` protects the actual on-disk file, not just the map key. Prune derives the protected name from the path when it lives in `images_dir`.
- **Image cache:** `LinkCachedImage` helper uses atomic `os.Link`-to-temp + `os.Rename` so in-flight `CopyRootfs` readers keep valid open FDs on the old inode, and cleans up partially-written state if the sidecar write fails.
- **Docs:** New "`base_rootfs` vs `images:`" and "On-Disk Layout" sections in `docs/reference/images.md`, with per-backend (Firecracker/VZ) layout tables and an upgrade-and-reclaim cookbook. Cross-linked from `docs/getting-started/fc-setup.md`, `docs/getting-started/vz-setup.md`, and the `shed image prune` / `shed-server pull-images` entries in `docs/reference/cli.md`.
- **Config templates:** Bumped `configs/server.localmac.yaml` and `configs/server.localfc.yaml` image refs from `:v0.3.1` to `:v0.3.4`.

## v0.3.4

- **Firecracker:** Fix PS1 prompt showing time instead of shed name (Dockerfile heredoc with single-quoted PS1, matching VZ)
- **Firecracker:** Fix credential mounts (`~/.codex`, `~/.claude`, `~/.config/gh`) appearing as `root:root` inside the VM — `resolvePathOwner` now falls back to UID 1000 (shed user) for root-owned host dirs instead of triggering p9 passthrough
- **Firecracker:** Implement missing 9P ops (`UnlinkAt`, `SetAttr` chmod, `SetAttr` timestamps) on `remappingFile`; p9 library's localfs returned ENOSYS for these
- **Firecracker (security):** Skip `chmod`/`chtimes` on symlinks in 9P `SetAttr` — `os.Chmod` and `os.Chtimes` follow symlinks, which could let a guest modify files outside the shared directory by crafting a symlink (matches the existing `os.Lchown` bypass for ownership changes)
- **Firecracker / VZ:** Add `bubblewrap` to both Dockerfiles (required by Codex CLI sandbox); remove obsolete `// +build` tags
- **Tests:** Stabilize `TestHandleConnectSuccess` CI flake — replace `net.Pipe` mock with real TCP socketpair so `BidirectionalCopy`'s `CloseWrite`-based EOF propagation works correctly, add I/O deadlines, exercise both copy directions (#74, #75)

## v0.3.3

- **Breaking (deb only):** Rename deb package `shed` → `shed-server` and the release artifact to `shed-server_<version>_<arch>.deb` to avoid silent collisions with Ubuntu's `shed` hex editor. Add `Conflicts: shed` so the two packages can never coexist. Hosts on the old v0.3.2 deb need to `sudo apt purge shed && sudo dpkg -i shed-server_0.3.3_*.deb` (the old binary at `/usr/local/bin/shed-server` may have been silently removed by an `apt upgrade` — `readlink /proc/$(pidof shed-server)/exe` showing `(deleted)` confirms this)
- Document the deb as the primary Linux install path (#71)
- Align `configs/server.local.yaml` (localfc) with localmac defaults (extensions, mounts, images)

## v0.3.2

- Add `.deb` package support via GoReleaser nfpm for Linux (Ubuntu/Pop!OS) deployment
- Add `shed-server setup` command for automated Firecracker infrastructure provisioning (Linux-only)
- Add `shed-server pull-images` command to pre-cache VM images from Docker refs (cross-platform)
- Add Homebrew tap automation via GoReleaser
- Add VZ entitlement codesigning in Homebrew post_install
- Improve Homebrew config with platform-specific defaults and extensions guidance
- Update docs for Homebrew install workflow

## v0.3.1

- Add Docker credential helper to experimental images (docker-credential-shed, guest Docker config)
- Enable `docker-credentials` namespace in local dev server config
- Bump shed-extensions to v0.3.1
- Add DialService, Connect API, and vsock TCP proxy (#62)
- Run initial extension health check immediately (#61)

## v0.2.0

- Replace `typescript` image variant with `experimental` (default + shed-extensions credential brokering)
- Publish `shed-vz-experimental` and `shed-fc-experimental` images to ghcr.io
- Add `--shed-ext-version` flag to build scripts for local development
- Add SFTP support and `environment.d` loading in shed-agent
- Consolidate health checks onto message bus with heartbeats
- Upgrade GitHub Actions to Node.js 24 compatible versions
- Fix hardcoded image versions in docs, reorient Quick Start

## v0.1.2

- Fix kernel extraction failing on Firecracker images due to `set -euo pipefail` aborting on glob mismatch before reaching the `/boot/vmlinux` fallback path

## v0.1.1

- Include custom Firecracker kernel in published images — users no longer need to compile a kernel or run `build-firecracker-kernel.sh` when using published images
- Add `GetNeedsInitrd()` to ImageConfig interface to make initrd extraction optional (VZ only)
- Update `extractKernel()` to handle both VZ-style compressed and FC-style uncompressed kernels
- Default Firecracker `kernel_path` to `{images_dir}/vmlinux` (auto-populated from published images)
- Defer Firecracker `kernel_path` validation when Docker refs are configured
- Extract `hasAnyDockerRef()` and `dockerRunScript()` shared helpers
- Update Firecracker setup docs and example configs for kernel-in-image workflow

## v0.1.0

### Features

- Add VZ backend for macOS Apple Silicon using vfkit/Virtualization.framework
- Add `--local-dir` flag for mounting host directories as workspace (VZ with VirtioFS, Firecracker with 9P)
- Add image extensibility with Docker ref support and multiple image variants
- Add image delete and prune commands for managing cached rootfs images
- Add Firecracker image management parity with VZ backend
- Add plugin message bus for extensible VM-host communication
- Add CI workflow to publish VZ and Firecracker base images to ghcr.io on release tags
- Add SSE progress streaming for shed create
- Add enriched shed metadata and tiered CLI verbosity

### Firecracker Backend

- Add 9P filesystem and UID remapping for local-dir mounts
- Add exclude patterns to credential mounts
- Add credential change notifications over persistent vsock channel

### VZ Backend

- Switch to VirtioFS for credential mounts
- Add Docker CE networking in guest VMs
- Add multiple image variants with multi-stage Dockerfile (base, default, typescript)
- Fix DNS resolution and credential transfer

### Fixes

- Fix credential exclude glob matching for dir/* patterns
- Fix SSH config incompatibility causing git clone failures
- Fix console hang after shell exit by closing PTY master promptly
- Fix race condition in Firecracker dialer and exec stdin framing
- Fix shed exec PATH and improve backend error propagation
- Fix VM provisioning failing on first run due to state file check
- Fix credential sync tar failure on ephemeral files

### Documentation

- Unify image reference documentation for both VZ and Firecracker backends
- Restructure Firecracker docs into getting-started and reference sections
- Add comprehensive shed lifecycle documentation across all backends
- Add provisioning, tunnels, and file sync reference pages

### Infrastructure

- Upgrade golangci-lint to v2.10.1 managed via mise
- Add Dockerfile linting to CI

## v0.0.1

Initial release of shed, an SSH-based development environment manager.

### Features

- Docker container backend with bind mounts and Docker exec
- Firecracker microVM backend with vsock communication, TAP networking, and rootfs overlays
- SSH server (port 2222) and HTTP API (port 8080)
- Provisioning and file sync for containers and VMs
- SSH tunnel management for port forwarding
- Tmux session management for persistent CLI sessions
- Credential mounts for CLI tools (Git, SSH, AWS, etc.)
- Bidirectional credential sync for Firecracker VMs
- Graceful shutdown hooks for Firecracker VMs
- JSON output flag for machine-readable CLI output
- Repo URL validation with shorthand expansion (e.g., `owner/repo`)
- Configurable timeouts for create and start operations
- MkDocs-based documentation

### Firecracker Backend

- Run VM commands as non-root shed user
- Kernel build scripts and Firecracker v1.14.1 support
- Config validation with upper-bound constraints
- Version metadata for VM instances

### Infrastructure

- CI with linting and tests
- Release pipeline with GoReleaser and GitHub Actions
