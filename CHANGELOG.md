# Changelog

All notable changes to this project will be documented in this file.

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
