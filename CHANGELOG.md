# Changelog

All notable changes to this project will be documented in this file.

## v0.5.0 — 2026-05-18

### Release-readiness fixes

- **`shed image push --local` works from a Linux runner pointing at a
  `default_backend: vz` config.** The publish workflow stages a small
  config file with the relevant backend block; on the `ubuntu-24.04-arm`
  runner that meant `vz:`, which the strict validator rejected
  (`vz backend is only supported on macOS`). Added
  `config.LoadServerConfigForCLI` + `ServerConfig.ValidateNoHostCoupling`
  for the image-only flows that never start a VM. Callers that *do* start
  a VM (shed-server serve) still use the strict path.
- **`shed image build` picks the right tag prefix AND `io.shed.source-ref`
  for cross-builds.** The PR #92 fix corrected `--platform` for
  `--target shed-vz-*` invocations from a Linux runner, but the tag
  prefix (`shed-fc-` vs `shed-vz-`), kernel-extraction flag, and initrd
  flag were still derived from `runtime.GOOS` — so a Linux-built
  `shed-vz-full` image landed on disk tagged `shed-fc-full:latest` and
  its manifest's `io.shed.source-ref` annotation lied accordingly. The
  per-target table is now centralized and driven off `--target`
  uniformly. Added `--source-ref <ref>` so CI publish workflows can pin
  the annotation to the final `ghcr.io/charliek/shed-*-*:<version>` ref
  the image will be pushed to; without this, the server's
  `resolveImage` cache-hit check (which compares the manifest annotation
  against the configured `ref:`) missed on every subsequent `shed create`
  and forced a re-pull.
- **`shed image build` picks the right `--platform` for the target backend.**
  Previously the CLI inferred the platform purely from `runtime.GOOS`, so
  invoking `shed image build --target shed-vz-*` from a Linux runner (e.g.,
  the `ubuntu-24.04-arm` GitHub Actions runner used by the publish workflow)
  silently flipped to `linux/amd64`, which then crashed the per-layer ext4
  materialization step with `exec format error`. The CLI now inspects
  `--target shed-vz-*` / `shed-fc-*` and picks `linux/arm64` / `linux/amd64`
  accordingly; an explicit `--platform <plat>` flag is available as an
  override.
- **`POST /api/images/pull` no longer 405s.** A chi router precedence
  bug routed `POST /api/images/pull` to the parametric
  `DELETE /api/images/{name}` handler, returning `405 Method Not Allowed,
  Allow: DELETE`. Fixed in `internal/api/server.go` by mounting the
  static `/pull` route before the parametric `/{name}` sibling.
- **Legacy bundled-blob layout now surfaces an actionable error.**
  v0.4.x users upgrading without wiping `images_dir` previously hit a
  cryptic `... is a directory` error. `internal/vmimage/ocilayout.go`
  now detects this case and wraps it as `ErrLegacyBundledBlob`; the CLI
  surfaces a message pointing the user at [`docs/UPGRADE.md`](docs/UPGRADE.md).
- **`image has N layers (max 16)` rejection now includes a recovery
  hint.** `internal/vmimage/registry.go` extends the pull-time
  `MaxLayers` error with concrete next steps (wait for the v0.5.0+
  published image, or rebuild locally with the backend's build script).
- **`SHED-INIT-04` panic cross-references the upgrade guide.** The
  initramfs overlay-mount panic in `initramfs/init` now points at
  [`docs/UPGRADE.md#shed-init-04-panic-during-vm-boot`](docs/UPGRADE.md#shed-init-04-panic-during-vm-boot)
  so operators know to refresh the cached initramfs by re-running the
  build script.
- **New top-level [`docs/UPGRADE.md`](docs/UPGRADE.md).** Walks v0.4.x →
  v0.5.0 with a "what's new", a keep/lose table, links into the
  backend-specific wipe steps in `vz-setup.md` / `fc-setup.md`, and a
  recovery-scenarios index keyed off the error strings users actually
  see (cryptic "is a directory", `>MaxLayers`, `SHED-INIT-04`, the
  now-fixed 405 on `/api/images/pull`). Added to the MkDocs nav under
  "Getting Started".
- **Getting-started doc gap repairs.**
  [`quick-start.md`](docs/getting-started/quick-start.md) now frames the
  published-vs-source install paths explicitly, routes source builders
  into the backend setup guides instead of dead-ending at `make build`,
  and drops the stale `VERSION=0.3.3` pin in favor of a
  `gh release view`-driven lookup.
  [`vz-setup.md`](docs/getting-started/vz-setup.md) and
  [`fc-setup.md`](docs/getting-started/fc-setup.md) gain a working
  local-build `server.yaml` example (omit `base_rootfs` + `images:`,
  rely on tag auto-discovery, pass `--image <tag>`), demote
  `kernel_path` / `initrd_path` to optional fallbacks for OCI images,
  spell out `shed-server serve --config <path>` (the binary doesn't
  read `~/.config/shed/server.yaml` by default), document `uppers_dir`
  as an optional FC config field that should track non-default
  `instance_dir`, restructure the FC manual-setup section so source
  builders see it as required rather than optional reference material,
  call out the Go-on-host requirement for the FC rootfs build script,
  and reframe `shed server add localhost` so users know it registers
  under the server's `name:` field (with `shed server list` to confirm).
  Concrete `:v0.5.0` placeholders replace the unresolvable `:v{version}`
  pseudo-syntax.

### Storage rewrite (Phase A / B / C)

- **`shed image build` preserves the docker layer structure (multi-layer per variant).** The previous flow flattened every variant to a single layer via `docker create` + `docker export`, defeating cross-variant sharing on disk. The new flow uses `docker buildx --output type=oci,dest=<tar>` and ingests each layer blob into the local store, so `base`, `extensions`, and `full` share their common parent layers byte-for-byte. The `vz/` and `firecracker/` Dockerfiles are consolidated (BuildKit 1.7 `--mount=type=bind` for context staging) to keep each variant under the 16-layer `MaxLayers` cap: VZ ships 5 / 7 / 9 layers and FC ships 6 / 8 / 10. Measured on an arm64 Mac with all three VZ variants built locally: **~5.0 GB total** (1.7 GB blobs + 3.3 GB cache) versus **~12 GB** for the same three under the old flattened model — about **60% less disk** for users who keep multiple variants installed. Single-variant footprint is roughly unchanged; the gain is from cross-variant blob dedup.
- **Multi-layer boot fixes surfaced during validation.** Three latent bugs only triggered once a variant carried more than one lower:
  - **vfkit bootloader cmdline:** `--bootloader linux,kernel=,initrd=,cmdline=…` is comma-separated key=value, and `shed.lowers=/dev/vdb,/dev/vdc,…` embeds commas vfkit interprets as bogus options (`unknown option /dev/vdc`). Switched to the dedicated `--kernel` / `--initrd` / `--kernel-cmdline` flags.
  - **`/proc` mounted after cmdline parse:** the initramfs read `/proc/cmdline` before mounting `/proc`, silently fell through to the legacy `LOWER_DEVS=/dev/vdb` fallback — accidentally correct for one lower, wrong for N. Fixed by mounting `/proc`, `/sys`, `/dev` first.
  - **`overlay.ko` module probe path:** only looked under `/lib/modules/…`, but on Ubuntu 24.04 the `/lib → /usr/lib` symlink lives in the bare-distro layer while the kernel module ships in the APT layer; inside an isolated layer ext4 only `/usr/lib/modules/…` resolves. Now probes both paths in every lower.
- **BREAKING — content-addressed image store + tag indirection (storage rewrite, Phase A):** The flat-file image cache (`{name}-rootfs.ext4` + `.source` sidecar + `.lock`) is gone, replaced by a Docker-style blob store at `{images_dir}/blobs/sha256/<digest>/` with tags at `{images_dir}/tags/<tag>.json`. Image identity is now the sha256 of the produced ext4 bytes. Tags name digests; multiple tags can point at one digest with no extra disk cost. **All existing images and sheds must be discarded on upgrade** — `shed-server` refuses to load v1 instance metadata with a clear delete-and-recreate message. Tag → blob resolution is atomic (tmp + rename), and concurrent EnsureImage/InstallBlob calls serialize per-tag and per-digest via flock. See [`docs/reference/storage-model.md`](docs/reference/storage-model.md) for the layout and lifecycle commands.
- **BREAKING — overlay-in-guest boot (storage rewrite, Phase B):** Each shed now boots from a writable per-shed *upper* (sparse `uppers/<name>/upper.ext4`, default 5 GB) mounted on top of the shared read-only *lower* (the cached blob's `rootfs.ext4`) via a new busybox-based initramfs that builds the overlay and `pivot_root`s into the merged tree. Both backends now pass the initrd from the per-image blob dir and append a second virtio-blk drive for the lower; the kernel cmdline drops `root=` and adds `shed.upper=` / `shed.lower=`. `CopyRootfs` is gone from the create path; new `EnsureUpper(uppersDir, name, sizeBytes)` allocates the sparse upper and the in-guest initramfs `mkfs.ext4`-formats it on first boot. Per-shed disk cost drops from a 2-5 GB rootfs clone to a few hundred MB of written upper, *independent of host filesystem reflink support*.
- **`shed create --upper-size <N>G` + `upper_size_default` config (storage rewrite, Phase B).** New per-shed override (1G-100G validated; integer-overflow guarded) plus per-backend default `upper_size_default: 5G`. Stored as `upper_path` + `upper_size_bytes` in instance metadata.
- **`shed reset <name>` (new, storage rewrite, Phase C).** Wipes and recreates the per-shed writable upper while leaving the shared lower image and `/workspace` (mounted post-boot, outside the overlay) untouched. Requires the shed to be stopped. `Backend.ResetShed` + `POST /api/sheds/{name}/reset` + `SHED_NOT_STOPPED` (409) sentinel.
- **Snapshots capture only the upper (storage rewrite, Phase C).** `shed snapshot create` now clones the per-shed upper instead of the merged rootfs, so snapshots are typically a few hundred MB rather than a full image. Snapshot metadata gains `lower_digest` pinning the underlying-blob digest; spawning from a snapshot inherits the pin. `shed snapshot info` warns when the pinned lower digest is no longer cached (`Lower digest: sha256:... (MISSING — pull or rebuild the image before spawning)`), and `shed create --from-snapshot` fail-fasts with an actionable error pointing at the original image tag.
- **`shed system df` per-shed accounting (storage rewrite, Phase C).** The per-shed row now reports only the writable upper (typically hundreds of MB), and the shared lower image is reported once under `images` rather than duplicated under every shed pinning it. The "physical bytes may overcount shared extents on APFS / reflink" caveat is dropped — the upper/lower split removes the double-counting. CLI numbers should now match `du -k` to within a few KiB.
- **Initramfs build pipeline (storage rewrite, Phase B).** New top-level `initramfs/` directory with a busybox-based init script and a dedicated `initramfs/Dockerfile` for the cpio.gz build. `scripts/build-initramfs.sh` wraps the Dockerfile stage. `scripts/build-{vz,firecracker}-rootfs.sh` stage the initramfs into a tempfile and call the new `shed image install` Go subcommand to atomically install the rootfs + kernel + initrd into the content-addressed blob store, advancing the variant tag. The legacy `scripts/install-blob.sh` (a partial bash re-implementation of the install protocol — no fsync, no flock, no JSON escaping) is deleted; the build pipeline now goes through the same code path as the runtime EnsureImage flow.
- **Metadata schema v2 (FC + VZ).** Instance metadata gains `lower_digest` + `lower_image_tag` + `upper_path` + `upper_size_bytes`, recorded at create time and inherited from snapshots when spawning. Snapshot metadata gains `lower_digest` so spawn-from-snapshot inherits the underlying-blob pin. Pre-v2 metadata loads now error with an actionable message pointing operators at manual `rm -rf` cleanup of the instance directory (since `shed delete` itself goes through `LoadMetadata` and would hit the same error).
- **Refcount-protected prune.** `shed image prune` walks `instances/*/metadata.json` and `snapshots/*/snapshot.json` and removes any blob whose digest has zero shed/snapshot references. Tags do **not** protect a digest (Docker model). `shed image rm <tag>` removes the tag only; the blob persists for `prune` to GC. `shed system prune --images` is wired through the same refcount path. The ref scanner fail-closes on snapshot-load errors so a transient I/O failure can't trick prune into deleting a still-pinned blob.
- **CLI:** `shed image ls` (alias `list`), `shed image rm` (alias `delete`), `shed image inspect <tag-or-digest>`, `shed image tag <src> <new>`, `shed image pull <docker-ref> [-t <tag>]`, `shed image install --rootfs <path> [...]`, `shed reset <name>`. `shed image build` now writes into the blob store and advances a tag — no `.source` sidecar, no `LinkCachedImage` hop. `shed image ls` output gains DIGEST and IN USE columns. `shed image rm` no longer claims `(freed X)` — only the tag is removed; `shed image prune` reclaims the blob. `shed create` gains `--upper-size`.
- **API:** New `POST /api/images/tag`, `POST /api/images/pull`, `GET /api/images/inspect/{name}`, `POST /api/sheds/{name}/reset`. `ImagesResponse.images[]` gains `digest`, `tag`, and `in_use` fields. `Snapshot` wire format gains a transient `lower_cached` bool (recomputed on each read; never persisted). `CreateShedRequest` accepts `upper_size_bytes`. Sentinel error mapping: `ErrShedNotStoppedSentinel` → 409 `SHED_NOT_STOPPED`; `ErrNotSupportedSentinel` → 501 (VZ-on-Linux + FC-on-darwin stubs both map here). Tag and pull request handlers return 400 `INVALID_REQUEST` for malformed JSON / empty fields / unsafe tag names.
- **Server config resolution.** `config.ResolveImage` and `config.ResolveBaseRootfs` look up cached images through the blob-store tag layer. A blob is treated as fresh when its `manifest.source_ref` matches the configured Docker ref; otherwise the resolver returns the Docker ref so `EnsureImage` can pull a new digest and advance the tag.
- **`shed-server pull-images`.** When `base_rootfs` and an `images:` entry share a Docker ref, `_base` is now realized as a tag pointing at the same digest — no hardlink dance needed (the blob is shared by definition).
- **Removed:** `vmimage.{CheckCache,WriteSource,SourceFilename,LinkCachedImage}` and the legacy `*-rootfs.ext4` flat-file layout. `inUseImageNames` closure plumbing replaced by a `vmimage.RefScanner` interface that returns shed + snapshot digest references.
- **Tests:** New `internal/vmimage/blobstore.go`, `refs.go`, plus rewritten manager/convert tests covering digest determinism, install atomicity, refs scanning, tag resolution, and config ResolveImage's tag-aware fast path. FC + VZ test fixtures install fake blobs through the public `vmimage` API rather than seeding flat files.

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
