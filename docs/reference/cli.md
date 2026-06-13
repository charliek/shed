# CLI Reference

Complete reference for the `shed` command-line interface.

!!! warning "Migration from v0.4.x"
    Two image commands were removed in the OCI image rollout:

    - `shed image build --from <ref>` — use [`shed image pull`](#shed-image-pull) instead. Pulls are now registry-direct via `go-containerregistry` and don't require a Docker daemon.
    - `shed image install` — host-side blob install. The same effect is now produced by `shed image build`, `shed image pull`, or `shed image load`.

    The variant lineup also changed. The old `default`, `devtools`, and `experimental` variants are replaced by `base`, `extensions`, and `full`. See [Images](images.md) for the new lineup.

## Global Flags

| Flag | Short | Description |
|------|-------|-------------|
| `--server` | `-s` | Target server (overrides default) |
| `--verbose` | `-v` | Increase output verbosity (`-v` for expanded, `-vv` for full detail) |
| `--config` | `-c` | Config file path (default: `~/.shed/config.yaml`) |
| `--json` | | Emit structured JSON to stdout (suppresses verbose output) |

## Server Management

### shed server add

Adds a new server to the client configuration.

```bash
shed server add <host> [flags]
```

| Flag | Short | Default | Description |
|------|-------|---------|-------------|
| `--name` | `-n` | Derived from host | Friendly name for server |
| `--port` | `-p` | `8080` | HTTP API port |
| `--fingerprint` | | | Expected SSH host-key fingerprint (`SHA256:...`); verified out-of-band and fails on mismatch |
| `--trust-on-first-use` | | `false` | Trust the server's SSH host key without prompting |

The SSH port is automatically discovered from the server's `/api/info` endpoint.

The command pins the server's SSH host key (in `~/.shed/known_hosts`). It prints the SHA256 fingerprint and, on an interactive terminal, asks you to confirm it before trusting — closing the add-time MITM window. Supply `--fingerprint SHA256:...` (read from the server's startup log) to verify out-of-band, or `--trust-on-first-use` to accept without a prompt. When stdin is not a terminal (scripts/CI), the key is trusted on first use unless `--fingerprint` is given.

**Example:**

```bash
shed server add mini-desktop.tailnet.ts.net --name mini
shed server add vps.example.com --name vps --fingerprint SHA256:HtYK...j4Y
```

### shed server list

Lists configured servers.

```bash
shed server list
```

**Output:**

```
NAME            HOST                              HTTP    SSH     STATUS     DEFAULT
mini-desktop    mini-desktop.tailnet.ts.net       8080    2222    online     *
cloud-vps       vps.tailnet.ts.net                8080    2222    offline
```

### shed server remove

Removes a server from configuration.

```bash
shed server remove <name>
```

### shed server set-default

Sets the default server.

```bash
shed server set-default <name>
```

## Shed Lifecycle

### shed create

Creates a new shed.

```bash
shed create <name> [flags]
```

| Flag | Short | Default | Description |
|------|-------|---------|-------------|
| `--repo` | `-r` | None | Repository to clone into `~/<reponame>` (`owner/repo` shorthand or full URL). The login lands in the cloned repo directory. Mutually exclusive with `--local-dir`/`--add-dir`. |
| `--server` | `-s` | Default server | Target server |
| `--image` | `-i` | Server default | Image [variant name](images.md) to use |
| `--backend` | | Server default | Backend to use: `firecracker` or `vz` |
| `--cpus` | | Server default | Number of vCPUs |
| `--memory` | | Server default | Memory in MB |
| `--upper-size` | | Server default (`5G`) | Logical size of the per-shed writable overlay upper layer (e.g. `1G`, `5G`, `10G`, `100G`). Validated range: 1–100 GiB. The overlay grows copy-on-write, so this is the maximum, not the on-disk physical bytes. |
| `--local-dir` | | None | Mount a local host directory into the guest at `~/<basename>`. The login lands there. Mutually exclusive with `--repo`. |
| `--add-dir` | | None | Mount an additional local host directory at `~/<basename>` as a reference sibling. Repeatable. Requires `--local-dir`. |
| `--no-provision` | | `false` | Skip provisioning hooks |
| `--sync-profile` | | `default` | Profile to sync after creation |
| `--no-sync` | | `false` | Skip syncing default profile |
| `--timeout` | | `10m` | Timeout for create operation (e.g., `30s`, `5m`, `2h`) |

**Examples:**

```bash
shed create scratch
shed create codelens --repo charliek/codelens
shed create stbot --repo charliek/stbot --server cloud-vps
shed create myproj --sync-profile full
shed create myproj --no-sync
shed create bigproj --repo org/large-repo --timeout 30m

# Mount a local directory; the login lands in ~/myproj inside the shed
shed create myproj --local-dir ~/projects/myproj

# Mount additional reference directories alongside the primary one
shed create myproj --local-dir ~/projects/myproj --add-dir ~/projects/shared-lib
```

!!! note "Local directory mounts"
    When using `--local-dir`, the specified host directory is mounted into the guest at `~/<basename>` (i.e. `/home/shed/<basename>`), and an interactive login lands there. No volume is created and `--repo` cannot be used. VZ uses VirtioFS; Firecracker uses 9P over TCP.

    `--add-dir` mounts each additional host directory at `~/<basename>` as a reference sibling. It is repeatable and valid only together with `--local-dir`. No two mounted directories may share a basename, and dotfile-style basenames (leading `.`) are rejected.

    Host-mounted directories live outside the writable upper layer, so their contents are **not** captured by `shed snapshot create`. The cloned repo from `--repo` lives in the upper layer and **is** captured.

### shed list

Lists sheds.

```bash
shed list [flags]
```

| Flag | Short | Default | Description |
|------|-------|---------|-------------|
| `--server` | `-s` | Default | List from specific server |
| `--all` | `-a` | false | List from all servers |

**Output (default):**

```text
NAME          BACKEND    STATUS     SSH                              CREATED
codelens      vz         running    codelens@mini-desktop:2222       2026-01-20 10:30
mcp-test      vz         stopped    -                                2026-01-17 14:00
```

**Output with `-v` (expanded):**

```text
NAME          BACKEND    STATUS     SSH                              IP             RESOURCES    SOURCE              UPTIME
codelens      vz         running    codelens@mini-desktop:2222       192.168.64.2   2c/4096MB    charliek/codelens    2h30m
mcp-test      vz         stopped    -                                -              -            -                    -
```

With `-vv`, each shed is displayed as a grouped key-value detail view with network, resources, and runtime sections.

### shed start

Starts a stopped shed.

```bash
shed start <name> [flags]
```

| Flag | Short | Default | Description |
|------|-------|---------|-------------|
| `--timeout` | | `10m` | Timeout for start operation (e.g., `30s`, `5m`, `2h`) |

### shed stop

Stops a running shed.

```bash
shed stop <name>
```

### shed delete

Deletes a shed.

```bash
shed delete <name> [flags]
```

| Flag | Short | Default | Description |
|------|-------|---------|-------------|
| `--keep-volume` | | `false` | Keep the data volume |
| `--force` | `-f` | `false` | Skip confirmation |

**Note:** When using `--json`, the `--force` flag is required (interactive confirmation is not supported in JSON mode).

## Snapshots

A snapshot captures a stopped shed's rootfs as a named, immutable artifact.
New sheds spawned from a snapshot get a fresh identity (machine-id, SSH host
keys, hostname) and their own runtime mounts. See [Snapshots](snapshots.md)
for the full overview.

### shed snapshot create

```bash
shed snapshot create <shed-name> <snapshot-name> [--comment "..."]
```

Captures the source shed's rootfs into a new immutable snapshot. The source
shed must be stopped.

### shed snapshot list

```bash
shed snapshot list
```

Lists snapshots on the current server.

### shed snapshot info

```bash
shed snapshot info <snapshot-name>
```

Shows snapshot metadata, including provenance and size. The output includes a
`Lower digest: sha256:... (cached)` line — the digest of the lower image the
snapshot was captured against. If that blob is no longer in the local image
store, the status reads `(MISSING — pull or rebuild the image before
spawning)` and a warning block points at the original image tag. See
[Snapshots → Missing lower digest](snapshots.md#missing-lower-digest).

### shed snapshot delete

```bash
shed snapshot delete <snapshot-name> [-f]
```

Removes a snapshot. Spawned sheds remain independent (each has its own
rootfs copy).

### shed create --from-snapshot

```bash
shed create <new-name> --from-snapshot <snapshot-name> [--local-dir ...]
```

Spawns a new shed from a snapshot. `--from-snapshot` is mutually exclusive
with `--image` and `--repo` (the snapshot rootfs is the source of truth).
`--local-dir`, `--cpus`, and `--memory` remain valid since they describe
runtime configuration, not rootfs source.

If the snapshot's pinned `lower_digest` is no longer cached, the command
fails fast with `BACKEND_ERROR: snapshot ... references lower digest
sha256:... which is no longer cached; pull the original image (<tag>)
first`. See [Snapshots → Missing lower digest](snapshots.md#missing-lower-digest).

## Reset

### shed reset

Wipes and recreates a stopped shed's per-shed writable upper layer.

```bash
shed reset <name> [flags]
```

| Flag | Short | Default | Description |
|------|-------|---------|-------------|
| `--force` | `-f` | `false` | Skip confirmation prompt |

The writable upper holds the shed user's home directory (`/home/shed`),
including any repo cloned via `--repo` — `shed reset` deletes it and re-creates
it empty. The shared, read-only lower image is untouched, and host-mounted
directories from `--local-dir`/`--add-dir` survive (they live outside the
overlay). This is the equivalent of "throw away anything I wrote inside the VM
and start fresh from the same image."

The shed must be stopped. Otherwise the API returns `SHED_NOT_STOPPED` (HTTP
409). When using `--json`, `--force` is required.

Common uses:

- Recovering from the upper-corruption panic (`shed-initramfs PANIC: ... no
  ext4 magic at offset 1080 ...`) emitted by the in-guest initramfs.
- Throwing away accumulated experiment state without losing host-mounted `--local-dir`/`--add-dir` contents.

**Example:**

```bash
shed reset my-broken-shed --force
```

## Image Management

### shed image build

Builds an OCI image from a Dockerfile and installs it into the local
image store. The target platform is auto-detected from the host OS
(`linux/amd64` for Firecracker, `linux/arm64` for VZ).

```bash
shed image build [flags] [context]
```

| Flag | Short | Default | Description |
|------|-------|---------|-------------|
| `--file` | `-f` | `./Dockerfile.shed` or `./Dockerfile` | Dockerfile path |
| `--name` | `-n` | *required* | Tag to advance after install |
| `--target` | | | Build target stage |
| `--from-oci-archive` | | | Skip `docker buildx` and ingest a pre-built OCI image-layout tar (e.g. produced by `podman build --output type=oci,dest=...` or `buildah bud --output oci-archive,...`). Mutually exclusive with `--file`/`--target`/`--platform`/`--force`. |
| `--initramfs` | | | Pre-built shed-overlay initramfs (produced by `scripts/build-initramfs.sh`). Required for images that need shed's overlayfs assembly at boot — i.e. anything you intend to `shed create` against. |
| `--output-dir` | | `images_dir` from server config | Override the OCI store root |
| `--force` | | `false` | Skip base-image validation warning |

```bash
# Build the shed-overlay initramfs once.
./scripts/build-initramfs.sh --backend vz --platform linux/arm64 --output /tmp/shed-initrd.img

# Then drive the OCI conversion (Dockerfile path; runs docker buildx).
shed image build \
    --target shed-vz-full \
    -n my-image \
    --initramfs /tmp/shed-initrd.img \
    -f vz/Dockerfile vz/

# Or, ingest an OCI archive built externally (no Docker required):
podman build --platform linux/arm64 \
    --output type=oci,dest=my-image.tar \
    -f Dockerfile.shed .
shed image build \
    --from-oci-archive my-image.tar \
    -n my-image \
    --initramfs /tmp/shed-initrd.img
```

The `scripts/build-{vz,firecracker}-rootfs.sh` helpers wrap the
Dockerfile flow for the standard variants — use them if you don't
need a custom target. See [Build your own image § 2a](../tutorials/build-your-own-image.md#2a-alternative-building-without-docker)
for the daemon-free workflow.

To convert an existing registry image into the local store, use
[`shed image pull`](#shed-image-pull) — `shed image build --from` was
removed in the OCI rollout.

### shed image ls

Lists installed images with their resolved Docker ref in the `IMAGE`
column. The `SOURCE` column is `config` (the ref is the configured
`default_image` or an `image_aliases` value), `user` (pulled or labeled
ad-hoc), or `dangling` (an untagged, unconfigured leftover). `shed image
list` is a deprecated alias.

```bash
shed image ls
```

**Output:**

```text
IMAGE                                       DIGEST          SOURCE    SIZE     IN USE
ghcr.io/charliek/shed-vz-base:v0.6.0        sha256:abc123…  config    2.1 GB   yes
ghcr.io/charliek/shed-vz-extensions:v0.6.0  sha256:7c2e5d…  config    2.3 GB   no
ghcr.io/charliek/shed-vz-full:v0.6.0        sha256:def456…  config    3.8 GB   no
ghcr.io/test/legacy:v1                      sha256:ff8800…  dangling  2.0 GB   no
```

### shed image history

Lists the layers of an image, top-down (latest layer first).

```bash
shed image history <tag-or-digest>
```

| Column | Description |
|------|---------|
| `LAYER` | Layer ordinal — `1` is the bottom-most overlay lower, `N` is just below the writable upper |
| `DIGEST` | Layer blob digest |
| `SIZE` | Layer size in the OCI manifest |
| `CREATED` | Relative timestamp from the image config history |
| `CREATED BY` | Matching history entry (typically the Dockerfile line that produced the layer) |

**Example:**

```bash
shed image history shed-vz-full
```

```text
LAYER  DIGEST                                                                   SIZE      CREATED         CREATED BY
9      sha256:6214c050b2d46d711a9878da53f2ae1f1c2cc2644d1d30f9116d346c59d06ab2   493.4 MB  2 hours ago     RUN runuser -l shed -c '… mise use -g node@lts; uv python install 3.13; …'
8      sha256:4f4fb700ef54461cfa02571ae0db9a0dc1e0cdb5577484a6d75e68dc38e8acc1     32 B    2 hours ago     ENV CLAUDE_CONFIG_DIR=/home/shed/.claude
7      sha256:5c61939d1edf11daa570fcfe8ea24b56a60a89403f5ce91c4354cd400cad2591   6.92 MB   2 hours ago     RUN --mount=type=bind,from=shed-extensions … (extensions binaries)
…
2      sha256:a3e89a578b079f684c28e09084737b3ff22914ab234c60ae0064c6f4d218be54   1.18 GB   2 hours ago     RUN apt-get install systemd docker-ce …
1      sha256:818154cda96df8bbb276b4f4339124da55756620a1037af15570bc95312850fa     28 MB   2 hours ago     ubuntu:24.04 base
```

The big shared layers — `ubuntu:24.04` (ordinal 1) and `apt-get install`
(ordinal 2) — appear with the same digest in `base`, `extensions`, and
`full`, so on disk they cost once across all three variants.

### shed image inspect

Shows full details (manifest + annotations + cached path + in-use status)
for a tag or a digest. Accepts either a tag name (`full`) or a
`sha256:...` digest (full or truncated).

```bash
shed image inspect <tag-or-digest>
```

### shed image tag

Points a new tag at the digest currently held by another tag (or by a specified digest). Equivalent to `docker tag`.

```bash
shed image tag <src-tag-or-digest> <new-tag>
```

### shed image pull

Pulls an OCI reference registry-direct (no Docker daemon needed),
installs every blob into the local store, and advances a tag.

```bash
shed image pull <ref> [-t <tag>] [--platform <os/arch>] [--with-layers]
```

| Flag | Short | Default | Description |
|------|-------|---------|-------------|
| `--tag` | `-t` | derived from the ref | Tag to advance |
| `--platform` | | host (`linux/arm64` on VZ, `linux/amd64` on Firecracker) | Override the manifest selection for multi-arch indexes (e.g. `linux/amd64`, `linux/arm64`) |
| `--with-layers` | | off | Pull the full image including layer tarballs (see below) |

If `--tag` is omitted, the tag is derived from the last path segment of
the ref minus the `shed-{vz,fc}-` prefix and the version suffix (e.g.
`ghcr.io/charliek/shed-vz-extensions:v0.5.1` → `extensions`).

#### Boot-only by default

A shed host **boots from the prebuilt rootfs erofs blob and never reads
the OCI layer tarballs**. So `pull` (and the implicit pull during
`shed create`) is **boot-only** by default: it fetches the config,
kernel, initrd, erofs, and manifest, but **skips the layer tarballs** —
roughly half the bytes of a typical image. The image boots normally;
`shed image ls` shows it under `LAYERS` as `boot-only`.

The layers are only needed to **re-push a *pulled* image** byte-perfect
(`shed image push`). That's rare — building and pushing your *own* image
is unaffected, because `shed image build` produces its layers locally
(see [Build Your Own Image](../tutorials/build-your-own-image.md)). When
you do need to push a pulled image:

```bash
shed image pull <ref> --with-layers   # full pull (or hydrate a boot-only image)
shed image push <ref> <dst>
```

`--with-layers` on an already-boot-only image fetches just the missing
layer blobs (everything already present is a content-addressed no-op).
Pre-v0.5.2 images (no erofs annotation) can't be pulled boot-only and
require `--with-layers`.

`pull` writes the image's blobs into `blobs/sha256/` and records the
ref→digest mapping in the ref-index.

### shed image push

Pushes a local tag or manifest digest to an OCI registry. The upload is
byte-perfect — the manifest digest at the destination equals the local
manifest digest.

```bash
shed image push <src> <dst>
```

| Argument | Description |
|------|---------|
| `<src>` | Local tag (`full`) or `sha256:...` manifest digest |
| `<dst>` | Destination OCI reference (e.g. `ghcr.io/myorg/my-shed-image:v1`) |

| Flag | Default | Description |
|------|---------|-------------|
| `--local` | `false` | Read the OCI store directly instead of routing through a running shed-server's HTTP API. Implied when `-c <config>` is passed without `-s <server>`. Useful for CI publish flows. |

```bash
# Push against a running shed-server:
shed image push full ghcr.io/myorg/shed-vz-full:v0.5.1

# Local mode (no shed-server needed):
shed image push --local --output-dir ./store full ghcr.io/myorg/shed-vz-full:v0.5.1
shed -c ./publish-server.yaml image push full ghcr.io/myorg/shed-vz-full:v0.5.1
```

A byte-perfect push streams the layer tarballs from disk, so they must
be present. An image that was **pulled boot-only** has none — push fails
the layer preflight with an actionable error; re-pull it with
`shed image pull <ref> --with-layers` first. Images you **built**
locally (`shed image build`) already have their layers, so building and
pushing your own image needs no extra step.

Authentication uses the standard Docker credential resolution chain
(`~/.docker/config.json` and any installed credential helpers — `docker
login ghcr.io` works).

### shed image save

Writes a tag and every layer it references to a single OCI archive,
suitable for air-gap transport or backups.

```bash
shed image save <tag-or-digest> -o <file>
```

| Flag | Short | Default | Description |
|------|-------|---------|-------------|
| `--output` | `-o` | *required* | Output archive path |

The archive is a standard OCI image layout — `crane manifest
--from-archive`, `skopeo copy oci-archive:`, and similar tools work on
it unmodified.

```bash
shed image save shed-vz-full -o shed-vz-full.tar
crane manifest --from-archive shed-vz-full.tar
```

### shed image load

Loads an OCI archive into the local store. Inverse of `shed image save`.

```bash
shed image load -i <file>
```

| Flag | Short | Default | Description |
|------|-------|---------|-------------|
| `--input` | `-i` | *required* | OCI archive path |

```bash
shed image load -i shed-vz-full.tar
```

Layers already present locally are skipped (deduplicated by blob
digest). Tag pointers in the archive are advanced on the local side.

### shed image rm

Removes a tag from the image store. `shed image delete` is a deprecated alias.

```bash
shed image rm <name> [flags]
```

| Flag | Short | Default | Description |
|------|-------|---------|-------------|
| `--force` | | `false` | Skip confirmation prompt |

Accepts a Docker ref, a digest, or a cosmetic tag label. Following the Docker model, this drops the image's addressability (tags + ref-index entry) but leaves the underlying blob for `shed image prune` to garbage-collect once nothing references it. Removal is hard-blocked only when a live shed or snapshot still pins the manifest (matching `docker rmi`); removing an image referenced by server config (`default_image` / `image_aliases`) prompts for confirmation and means the next `shed create` for it re-pulls.

**Note:** When using `--json`, the `--force` flag is required.

**Examples:**

```bash
shed image rm myimage
shed image rm myimage --force
```

### shed image prune

Removes blobs that have no protective tag, shed, snapshot, or in-flight create reference.

```bash
shed image prune [flags]
```

| Flag | Short | Default | Description |
|------|-------|---------|-------------|
| `--force` | | `false` | Skip confirmation prompt |
| `--dry-run` | | `false` | List candidates without deleting |

A blob is preserved if its digest is referenced by a tag, an existing shed's `metadata.json` (`lower_digest`), or a snapshot's `snapshot.json` (`lower_digest`). To delete a tagged blob the workflow is `shed image rm <tag>` first, then `shed image prune` (Docker model — `docker rmi` then `docker image prune`).

**Upgrade note:** pre-v0.5.8 tags were informational and did NOT protect blobs, so `shed image pull <tag> && shed image prune` would delete the manifest just pulled. The v0.5.8 fix makes tags protective. After bumping image refs in server config and re-running `shed-server pull-images`, drop any stale tag with `shed image rm` before `shed image prune` to actually reclaim the previous digest.

**Fail-closed on malformed metadata:** `image prune` aborts (returns an error naming the broken shed and its directory) if any instance's `metadata.json` cannot be parsed for `lower_digest` — pruning while a refcount source is unreadable could orphan a digest still in use. Fix or `rm -rf` the broken instance directory before re-running. Lenient read paths (`shed list`, `shed image ls`, `shed system df`) warn-and-skip on the same fault.

**Note:** When using `--json`, the `--force` flag is required.

**Examples:**

```bash
shed image prune --dry-run       # Preview what would be removed
shed image prune                 # Interactive confirmation
shed image prune --force         # No confirmation
```

## System (Disk Usage)

### shed system df

Shows disk usage for shed servers: image cache, per-instance rootfs copies, kernel/initrd, and orphan sidecar files.

```bash
shed system df [flags]
```

| Flag | Short | Default | Description |
|------|-------|---------|-------------|
| `--all` | | `false` | Query every configured server (client-side fan-out) |
| `--server` | `-s` | (default server) | Target a specific server (global flag) |
| `--verbose` | `-v` | | Show per-image, per-shed, kernel, and orphan rows (global flag) |
| `--json` | | `false` | Emit machine-readable JSON (global flag) |

The rollup view groups files into four categories — images (including kernel and initrd), sheds (per-instance rootfs + console logs), orphans (stale `.lock`/`.tmp`/`.source` files), and totals. `-v` breaks each category into per-file rows.

Both **logical** (apparent) and **physical** (allocated) bytes are reported. Physical bytes come from `stat.Blocks * 512`. On filesystems that support extent sharing (APFS clonefile, ext4/xfs reflink, hardlinks), the same bytes may be attributed to multiple files — so summed physical totals may overcount actual on-disk usage. A note on every report calls this out.

**Multi-server (`--all`):** runs concurrently across every configured server. Offline or older servers (returning errors or 404) are reported inline without aborting; the command exits 0. In `--json` mode the response is `{servers: [{server_name, usage?, error?}, ...]}`.

**Examples:**

```bash
shed system df                 # Rollup for the active server
shed system df -v              # Per-image and per-shed detail
shed system df --json | jq     # Pipe the raw wire type to jq
shed system df --all           # Fan out across all configured servers
shed system df -s mini2 --json # Query one specific server in JSON
```

### shed system prune

Reclaims disk space by deleting unused images, stopped instances past an age threshold, and orphaned sidecar files. Optionally truncates VZ `console.log` files.

```bash
shed system prune [flags]
```

| Flag | Default | Description |
|------|---------|-------------|
| `--images` | `false` | Prune unreferenced cached images |
| `--instances` | `false` | Prune stopped instances older than `--until` |
| `--logs` | `false` | Truncate VZ `console.log` files (opt-in; no-op on Firecracker) |
| `--orphans` | `false` | Remove `.tmp`/`.source` sidecars whose parent rootfs is gone |
| `--dry-run` | `false` | Show candidates without mutating |
| `--force` | `false` | Skip confirmation prompt; required with `--json` for the execute path |
| `--until` | `72h` | Stopped-instance age threshold via `mtime(metadata.json)`. `0s` = any age |
| `--log-tail-bytes` | `0` | Console log truncation target (`0` = server default, 5 MiB) |
| `--all` | `false` | Prune on every configured server (client-side fan-out) |
| `--server` (`-s`) | (default server) | Target a specific server (global flag) |
| `--json` | `false` | Emit machine-readable JSON (global flag) |

**Scope semantics:** flags are additive. If none of `--images/--instances/--logs/--orphans` is set, the server applies its default scope: images + instances + orphans (NOT logs, which is always opt-in).

**Dry-run-first UX:** the command always previews candidates before mutating. Without `--force` the CLI prints the candidate table and prompts for confirmation. With `--force` it executes immediately. With `--dry-run` it previews and exits.

**Age filtering:** stopped instances are pruned only when `mtime(metadata.json) < now - --until`. The mtime snapshot is captured before the staleness re-check that `shed list` would otherwise trigger, so transient running→stopped transitions don't reset the clock. `--until 0s` disables the age gate.

**Orphan safety:** a sidecar (`.tmp`, `.source`, or `.lock`) is treated as an orphan only when its parent `{name}-rootfs.ext4` is absent **and** a non-blocking `flock()` on `{name}-rootfs.ext4.lock` succeeds (i.e., no conversion is in flight). `.tmp` files younger than 1 hour are skipped unconditionally. The canonical `.lock` file is never removed even when abandoned, to avoid the inode-reuse race documented in `shed image prune`.

**Console log truncation (VZ only):** when `--logs` is set, each surviving VZ shed's `console.log` is truncated to its last `--log-tail-bytes` in place (preserves inode so `vfkit`'s `O_APPEND` file descriptor keeps writing past the new EOF). Firecracker does not produce a per-instance console log, so `--logs` is a silent no-op for FC sheds.

**Freed bytes caveat:** reported `physical_bytes` come from `stat.Blocks * 512` and are attributed to each file — they reflect how much the filesystem said each file occupied, not how much disk is actually reclaimed. Files that share extents via clonefile/FICLONE clones or hardlinks may report bytes that won't be reclaimed until the last reference is removed. Compare `shed system df` before and after for true reclamation.

**JSON + destructive:** `--json` without `--force` blocks the execute path. `--dry-run --json` is always allowed.

**Examples:**

```bash
shed system prune --dry-run            # Preview default scope
shed system prune                      # Preview + interactive confirm
shed system prune --force              # Execute without prompt
shed system prune --instances --until 1h --force
shed system prune --logs --log-tail-bytes 1048576 --force
shed system prune --all --force        # Prune every configured server
shed system prune --json --dry-run     # Machine-readable preview
```

## Interactive Access

### shed console

Opens an interactive shell in a shed.

```bash
shed console <name>
```

Opens `/bin/bash` in the shed, landing in the shed's landing directory — the shed user's home (`/home/shed`) by default, or the project directory when the shed was created with `--repo` (`~/<reponame>`) or `--local-dir` (`~/<basename>`). If the shed is stopped, it will be started automatically.

### shed exec

Executes a command in a shed.

```bash
shed exec <name> <command...>
```

| Flag | Short | Default | Description |
|------|-------|---------|-------------|
| `--session` | `-S` | None | Run in tmux session context |

**Working directory.** Commands run in the shed's landing directory — the shed user's home (`/home/shed`) by default, or the project directory when the shed was created with `--repo` (`~/<reponame>`) or `--local-dir` (`~/<basename>`).

**Environment.** Sessions (`shed exec` and interactive logins) export `SHED_NAME` (the shed name), `SHED_WORKSPACE` (the landing/project directory), and — when the shed was created with `--add-dir` — `SHED_ADD_DIRS` (a colon-separated list of the additional mount paths, e.g. `/home/shed/lib-a:/home/shed/lib-b`). These mirror the variables provisioning hooks receive.

**Execution model.** `shed exec` ships argv literally. The CLI single-quote-wraps each argv element before transmission so nested quotes, spaces, and shell metacharacters in *your* data survive the SSH wire intact and reach the guest as argv, not as shell code. The server-side SSH command channel does run those quoted tokens through `bash -lc` (so login PATH adjustments — `/etc/profile.d/*.sh`, `~/.profile`, mise, nvm, rustup — are in effect), but bash treats single-quoted text as literal data, so argv stays argv. End result: `shed exec name -- mytool` just works for tools installed via login-shell PATH managers, and `shed exec name -- echo '$HOME'` still prints the literal `$HOME` — no expansion, no command splitting.

This is the same model Docker, devcontainers, Codespaces, and Coder follow on their `exec` path. Pipes, redirects, semicolons, `$VAR` expansion, command substitution, and other shell metacharacters only take effect when *you* explicitly invoke a shell as part of the command (e.g. `bash -c '...'`).

**Raw SSH gets the full shell.** If you connect with raw `ssh shed-name 'cmd | pipe'` (the path Zed Remote-SSH, VS Code Remote-SSH, JetBrains Gateway, and `rsync` take), the SSH server runs your command string through `bash -lc <raw>` — so the pipe runs as a shell pipeline on the shed, exactly like a normal dev VM. The `shed exec` CLI is the path that preserves argv literally; raw SSH is the path that runs a shell.

**Examples:**

```bash
# Direct argv — bash on the server side, but argv stays argv because of
# the single-quote wrap. Tools installed via login PATH (mise, nvm, rustup,
# etc.) just work because `bash -lc` sources profile scripts.
shed exec codelens git status
shed exec codelens ls -la /home/shed
shed exec codelens rustc --version
shed exec codelens mise current

# Explicit shell when YOU want pipes/redirects/multi-statements
shed exec codelens -- bash -c 'echo hello | wc -c'
shed exec codelens -- bash -c 'cd ~/codelens && npm test'
shed exec codelens -- bash -c 'for f in *.go; do gofmt -l $f; done'

# Nested quotes survive intact
shed exec codelens -- bash -c 'bun -e "console.log(1+1)"'

# Variables in argv are LITERAL — no expansion (security gate)
shed exec codelens -- echo '$HOME'      # prints literal '$HOME'
shed exec codelens -- echo 'a;b;c'      # prints literal 'a;b;c'

# Raw SSH from any other client gets the full shell (Zed, VS Code, etc.)
ssh codelens 'echo $HOME'               # prints /home/shed
ssh codelens 'cat file | wc -l'         # pipe runs server-side

# Run in an existing tmux session
shed exec codelens --session default -- git status
```

### shed attach

Attaches to a tmux session. Creates the session if it doesn't exist.

```bash
shed attach <name> [flags]
```

| Flag | Short | Default | Description |
|------|-------|---------|-------------|
| `--session` | `-S` | `default` | Session name |
| `--new` | | `false` | Force create new session |

**Examples:**

```bash
shed attach codelens
shed attach codelens --session debug
shed attach codelens --new --session experiment
```

Detach with `Ctrl-B D`.

## Session Management

### shed sessions

Lists tmux sessions.

```bash
shed sessions [shed-name] [flags]
```

| Flag | Short | Default | Description |
|------|-------|---------|-------------|
| `--all` | `-a` | `false` | List from all servers |

**Examples:**

```bash
shed sessions                  # All sessions on default server
shed sessions myproj           # Sessions in specific shed
shed sessions --all            # Across all servers
```

**Output:**

```
SHED        SESSION      STATUS      CREATED      WINDOWS
codelens    default      attached    2h ago       1
codelens    debug        detached    30m ago      2
```

### shed sessions kill

Terminates a tmux session.

```bash
shed sessions kill <shed-name> <session-name>
```

## Port Forwarding

### shed tunnels start

Starts SSH tunnels for port forwarding.

```bash
shed tunnels start <shed> [flags]
```

| Flag | Short | Default | Description |
|------|-------|---------|-------------|
| `--profile` | `-p` | None | Use tunnel profile |
| `--tunnel` | `-t` | None | Port mapping (local:remote) |
| `--background` | `-d` | `false` | Run in background |
| `--replace` | | `false` | Replace existing tunnel without prompting |

**Examples:**

```bash
shed tunnels start myproj -t 3000:3000
shed tunnels start myproj -t 3000:3000 -t 5432:5432
shed tunnels start myproj -p webdev -d
shed tunnels start myproj -p webdev --replace
```

### shed tunnels stop

Stops tunnels.

```bash
shed tunnels stop [shed] [flags]
```

| Flag | Default | Description |
|------|---------|-------------|
| `--all` | | `false` | Stop all tunnels |

### shed tunnels list

Lists active tunnels.

```bash
shed tunnels list [flags]
```

| Flag | Short | Default | Description |
|------|-------|---------|-------------|
| `--verbose` | `-v` | `false` | Show detailed info |

### shed tunnels config

Previews tunnel configuration.

```bash
shed tunnels config <shed> [flags]
```

| Flag | Short | Default | Description |
|------|-------|---------|-------------|
| `--profile` | `-p` | None | Profile to preview |
| `--tunnel` | `-t` | None | Additional tunnels to include |

## File Sync

### shed sync

Syncs local files to a shed.

```bash
shed sync <name> [flags]
```

| Flag | Short | Default | Description |
|------|-------|---------|-------------|
| `--profile` | `-p` | `default` | Sync profile to use |
| `--feature` | `-f` | None | Sync single feature |
| `--dry-run` | | `false` | Preview without syncing |

**Examples:**

```bash
shed sync myproj
shed sync myproj -p full
shed sync myproj -f devproxy
shed sync myproj --dry-run
```

## IDE Integration

### shed ssh-config

Generates or installs SSH config for IDE integration.

```bash
shed ssh-config [name] [flags]
```

| Flag | Short | Default | Description |
|------|-------|---------|-------------|
| `--all` | | `false` | Generate for all known sheds |
| `--install` | | `false` | Write to `~/.ssh/config` |
| `--dry-run` | | `false` | Show changes without applying |
| `--uninstall` | | `false` | Remove entries |

**Examples:**

```bash
shed ssh-config codelens              # Print config for one shed
shed ssh-config --all --install --dry-run  # Preview changes
shed ssh-config --all --install       # Apply changes
shed ssh-config --uninstall           # Remove managed block
```

## Server Commands

These commands are part of `shed-server`, the server daemon binary.

### shed-server setup

Sets up Firecracker infrastructure on a Linux host. Idempotent and safe to re-run.

```bash
sudo shed-server setup
```

| Step | Description |
|------|-------------|
| KVM check | Verifies `/dev/kvm` is available |
| Docker check | Verifies Docker CE is installed |
| Firecracker | Downloads and installs Firecracker and jailer binaries |
| Kernel | Downloads fallback CI kernel if none exists |
| Directories | Creates `/var/lib/shed/firecracker/` and `/var/run/shed/firecracker/` |
| Bridge | Creates `shed-br0` bridge network |
| NAT | Enables IP forwarding and iptables masquerade rules |
| Capabilities | Sets `CAP_NET_ADMIN` on `shed-server` and `firecracker` binaries |

Linux only. Not available on macOS.

### shed-server doctor

Runs a one-pass health report against the local shed-server install. Designed as the first diagnostic step when `shed create` or `shed-server pull-images` fail unexpectedly.

```bash
sudo shed-server doctor
```

Each check reports `PASS` / `WARN` / `FAIL`. The command exits non-zero if any `FAIL` fires.

| Check | What it verifies |
|---|---|
| `/dev/kvm` | KVM is readable (hardware virtualization on, nested-virt enabled) |
| `docker` | Present on PATH (used by `shed image build`, not by `shed create`) |
| `firecracker` | `firecracker` binary installed at `/usr/local/bin/firecracker` and reports a version |
| `server.yaml` | Config parses cleanly via `loadConfig` (same parser as `shed-server serve`) |
| `kernel_path` | The configured kernel exists and is larger than 10 MB (sanity floor against truncated downloads) |
| `bridge` | The bridge interface named in `firecracker.bridge_name` exists and is up |
| `images` | Every installed tag points at a manifest with the v0.5.2+ `io.shed.rootfs.erofs.digest` annotation and the referenced blob exists locally |
| `extensions` | Every extension named in `extensions.enabled` has its manifest under `/etc/shed-extensions.d/` |
| `shed-server.service` | systemd unit is active |

Linux only. Not available on macOS.

### shed-server pull-images

Pre-caches VM images by pulling Docker refs and converting to ext4.

```bash
shed-server pull-images [flags]
```

| Flag | Default | Description |
|------|---------|-------------|
| `--variant` | All | Pre-pull only one selector (`default_image`, or an `image_aliases` name) |
| `--config` | Auto-detect | Path to server config file |

**Examples:**

```bash
sudo shed-server pull-images                  # Pull default_image + all aliases
sudo shed-server pull-images --variant base   # Pull only the "base" alias
```

Works on both macOS (VZ) and Linux (Firecracker). Pre-pulls the `default_image` and every `image_aliases` ref configured in `server.yaml` for the active backend. Refs that resolve to the same content-addressed manifest are pulled once. See [Storage Model](storage-model.md) for the on-disk layout and the [upgrade cookbook](images.md#cookbook-upgrading-image-versions) in the images reference.

### shed-server install

Installs shed-server as a systemd service. Alternative to the deb package for manual binary installs.

```bash
sudo shed-server install
```

Creates a systemd unit file at `/etc/systemd/system/shed-server.service` and enables it. Does not start the service.

### shed-server uninstall

Removes the shed-server systemd service.

```bash
sudo shed-server uninstall
```

Stops the service, disables it, and removes the unit file.

## Utility

### shed version

Shows version information.

```bash
shed version [flags]
```

| Flag | Short | Default | Description |
|------|-------|---------|-------------|
| `--verbose` | `-v` | `false` | Show full version info including dependencies |
