# Disk Management

Shed stores cached VM images, per-shed rootfs copies, and (on VZ) console logs on each server. This page explains what lives on disk, how to measure it, and how to reclaim space safely.

The three tools involved:

- `shed system df` — read-only disk usage report.
- `shed system prune` — scoped cleanup with a dry-run-first UX.
- Reflink / clonefile on `shed create` — new sheds share extents with the base image on reflink-capable filesystems, so per-shed disk cost starts at near-zero.

Full flag references live in the [CLI reference](cli.md#system-disk-usage); full API schemas live in the [HTTP API reference](api.md#system). This page focuses on workflows and the reflink behavior that affects every `shed create`. The on-disk layout itself is covered in [Storage Model](storage-model.md).

## What lives on disk

Each server stores four kinds of data in its backend directory:

| Kind | VZ path (macOS) | Firecracker path (Linux) | Created by | Removed by |
|------|-----------------|---------------------------|------------|------------|
| Image blobs | `~/Library/Application Support/shed/vz/blobs/sha256/<digest>/` | `/var/lib/shed/firecracker/images/blobs/sha256/<digest>/` | `shed image build`, `shed image pull`, or `shed-server pull-images` | `shed image prune` (when no shed/snapshot pins the digest) |
| Tags | `~/Library/Application Support/shed/vz/tags/<tag>.json` | `/var/lib/shed/firecracker/images/tags/<tag>.json` | Same as above | `shed image rm <tag>` |
| Kernel / initrd (per blob) | Stored alongside `rootfs.ext4` inside each blob directory | Same | First conversion of an image | Removed with the blob |
| Per-shed rootfs | `~/Library/Application Support/shed/vz/instances/{name}/rootfs.ext4` | `/var/lib/shed/firecracker/instances/{name}/rootfs.ext4` | `shed create` (shares extents with the blob's rootfs when possible) | `shed delete` or `shed system prune --instances` |
| VZ console log | `~/Library/Application Support/shed/vz/instances/{name}/console.log` | _(Firecracker has none — SDK writes to stderr)_ | VM boot | `shed system prune --logs` (truncates to last N bytes) |
| Orphan sidecars | `*.tmp` from a crashed install staging dir | Same | Partial or crashed image conversions | `shed system prune --orphans` |

See [Storage Model](storage-model.md) for the content-addressed layout
and [Image Variants](images.md) for how named tags resolve to digests.

## Measuring usage with `shed system df`

`shed system df` reports what each server currently holds on disk. The default rollup shows one line per category:

```text
SERVER:  prod-mac      BACKEND: vz
GENERATED: 2026-04-21T13:36:15Z

CATEGORY                  FILES  LOGICAL  PHYSICAL
images                    3      20.1 GB  3.3 GB
sheds (0 stopped, 2 run)  5      40.0 GB  6.3 GB
orphans                   1      0 B      0 B
TOTAL                     9      60.1 GB  9.6 GB

Note: physical bytes may overcount shared extents on APFS (clonefile) or hardlinks
```

Two columns matter:

- **LOGICAL** (`stat.Size`) — what tools like `du -k --apparent-size` see. For a 20 GB sparse ext4 rootfs, this is 20 GB regardless of how much data is actually in it.
- **PHYSICAL** (`stat.Blocks * 512`) — how much the filesystem reports as allocated. On non-reflink filesystems this is the real on-disk cost. On APFS, ext4-reflink, btrfs, and xfs, extents shared via clonefile or FICLONE are counted against **every** file that references them, so summed physical bytes can exceed the actual disk consumption.

Add `-v` for per-image and per-shed rows, `--json` for machine-readable output, and `--all` to fan out across every configured server:

```bash
shed system df
shed system df -v
shed system df --json | jq '.totals'
shed system df --all              # Every configured server
shed system df -s mini2           # Specific server
```

The full flag table is in the [CLI reference](cli.md#system-disk-usage). The raw response schema is documented under [`GET /api/system/df`](api.md#get-apisystemdf).

## Reclaiming space with `shed system prune`

`shed system prune` runs a scoped cleanup pass with four scopes that can be combined:

- `--images` — remove cached image variants that aren't referenced by config or any existing shed.
- `--instances` — delete stopped sheds older than `--until` (default 72 h).
- `--logs` — truncate VZ console logs to the last `--log-tail-bytes` (default 5 MiB). No-op on Firecracker.
- `--orphans` — remove `.tmp` / `.source` sidecars whose matching rootfs is absent. Lock files are preserved to avoid an inode-reuse race.

When no scope flags are set, the command applies the default scope: `--images --instances --orphans` (not `--logs`, which is always opt-in).

The command is always dry-run-first. It prints the candidate table, then prompts for confirmation unless `--force` is set:

```text
$ shed system prune
SERVER: prod-mac (dry-run) --until 72h0m0s scope=images+instances+orphans

IMAGES (2, 40.0 GB)
NAME          PATH                                                                          LOGICAL  PHYSICAL
base          /Users/alice/Library/Application Support/shed/vz/base-rootfs.ext4             20.0 GB  1.9 GB
experimental  /Users/alice/Library/Application Support/shed/vz/experimental-rootfs.ext4     20.0 GB  3.2 GB

SKIPPED (3)
KIND      NAME/PATH                                                                 REASON
instance  api-dev                                                                   cannot prune running shed
instance  api-test                                                                  too recent (3h < 72h)
lock      /Users/alice/Library/Application Support/shed/vz/foo-rootfs.ext4.lock     lock file retained (inode-reuse race safety)

TOTAL TO FREE: 40.0 GB logical / 5.1 GB physical (2 items)

Proceed? [y/N]
```

The 72 h age gate filters by `mtime(metadata.json)`, which is refreshed on every state change. `--until 0s` is an explicit "any age" escape hatch that still skips running sheds. `--all` fans out across every configured server; `--json --force` is required to execute non-interactively, and `--json --dry-run` is always allowed.

Full flag table, scope semantics, and the internal deletion ordering are in the [CLI reference](cli.md#shed-system-prune). The raw request/response schema is under [`POST /api/system/prune`](api.md#post-apisystemprune).

### Physical bytes vs. reclaimed disk

The `PHYSICAL` freed total is attributed per file from `stat.Blocks * 512`. When the file being removed shares extents with another file (clonefile, FICLONE, or hardlinks), the bytes the filesystem actually reclaims may be lower than what the report attributes. Compare `shed system df` before and after to measure true reclamation.

## Reflink and copy-on-write on `shed create`

`shed create` produces a per-shed rootfs under `instances/{name}/rootfs.ext4`. On reflink-capable filesystems the rootfs starts out sharing all its extents with `_base`, so the create adds near-zero physical bytes and completes in under a second on a warm cache. Writes diverge on a copy-on-write basis from that point, so long-lived sheds grow gradually with the changes the VM makes.

The server picks a strategy per create from this chain:

| Host | Filesystem | Strategy | Initial physical cost per shed |
|------|------------|----------|--------------------------------|
| macOS | APFS | `clonefile(2)` | ~0 bytes (extents shared with `_base`) |
| Linux | btrfs, xfs-reflink, or ext4 with `reflink=1` | `FICLONE` ioctl | ~0 bytes (extents shared with `_base`) |
| Linux | ext4 without reflink, other filesystems | `copy_file_range(2)` | Full image size (~2–5 GB) |
| Any | Remaining fallback | `io.Copy` | Full image size |

Each `shed create` logs one line to the server journal so operators can confirm which strategy fired:

```text
rootfs strategy=<clonefile|ficlone|copy_file_range|io_copy> src=<base path> dst=<instance rootfs path> logical_bytes=<size>
```

On Linux, check whether the images directory supports reflink:

```bash
sudo tune2fs -l $(findmnt -T /var/lib/shed/firecracker/images -o SOURCE --noheadings) \
  | grep -i 'features' | grep shared_blocks
```

If `shared_blocks` is not in the feature list, `shed create` falls back to `copy_file_range` and each shed will cost the full rootfs size. To enable reflink on ext4 you need kernel 6.7+ and the filesystem must have been created with `mkfs.ext4 -O reflink`.

## Workflows

### "Where did 40 GB go?"

```bash
shed system df -v
```

The verbose view shows per-image and per-shed rows so you can identify the largest consumers. On APFS the PHYSICAL column overcounts shared extents — cross-check against `du -k -s ~/Library/Application\ Support/shed/vz` if the sum looks higher than the actual disk.

### Clean up before a deploy

```bash
shed system prune --dry-run       # Preview
shed system prune                 # Interactive confirm
```

Default scope covers unreferenced images, stopped sheds older than 72 h, and orphan sidecars. Running sheds, recently-stopped sheds, and live conversion locks are all skipped with an explicit reason.

### Trim runaway console logs

```bash
shed system prune --logs --log-tail-bytes 1048576 --force
```

VZ `console.log` files grow unbounded during long-running sheds. This keeps the last 1 MiB of each log and discards the rest. The truncation happens in place so `vfkit` keeps writing past the new EOF; a tiny window of writes can be lost between the read-tail and truncate. On Firecracker this is a no-op (no per-instance console log exists).

### Fleet-wide cleanup

```bash
shed system prune --all --dry-run
shed system prune --all --force
```

Fan-out is client-side: each configured server is queried in parallel. Offline or older-version servers (missing the `/api/system/prune` route) are reported inline and the command still exits 0, so partial fleet upgrades don't block cleanup.
