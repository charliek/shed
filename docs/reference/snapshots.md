# Snapshots

A snapshot captures a stopped shed's rootfs as a named, immutable artifact. New
sheds can be spawned from a snapshot, getting an independent copy of the
captured rootfs and their own runtime mounts. Snapshots survive deletion of the
source shed and are stored separately on disk.

Snapshots are useful for:

- Creating a configured baseline (e.g. an installed agent + dependencies) and
  spawning multiple experiment sheds from it.
- Capturing a known-good state before an experiment, then discarding the
  experiment shed and re-spawning from the snapshot.

## What is captured

A snapshot is an offline (Tier 1) clone of the rootfs only:

- ✅ Captured: everything in the rootfs — installed packages, system config,
  home-directory dotfiles, files outside the workspace mount.
- ❌ Not captured: in-memory state (running processes, tmux sessions, page
  cache); host-side mounts (`--local-dir` workspace contents, credential
  syncs).
- ⚠️  If the source shed used `--local-dir`, the workspace is mounted from
  the host, so its contents are not in the rootfs. The CLI surfaces this as a
  warning at create time.

## Commands

```bash
# Create a snapshot from a stopped shed
shed snapshot create <shed-name> <snapshot-name> [--comment "..."]

# List snapshots on the current server
shed snapshot list

# Show details of a snapshot
shed snapshot info <snapshot-name>

# Delete a snapshot
shed snapshot delete <snapshot-name>

# Spawn a new shed from a snapshot (mutually exclusive with --image, --repo)
shed create <new-name> --from-snapshot <snapshot-name> [--local-dir ...]
```

## Identity regeneration

Each spawned shed gets a fresh `/etc/machine-id`, fresh SSH host keys, and the
correct hostname. This is handled by a one-shot `shed-firstboot` service that
runs early in boot — before D-Bus, journald, sshd, or `shed-agent` cache the
old identity. The service is idempotent: it re-runs only when the recorded
shed name in `/var/lib/shed/identity.json` does not match the
`shed.name=<name>` value passed on the kernel cmdline.

This makes `ssh known_hosts` and machine-id-based services work correctly
across snapshot-spawned sheds.

## Constraints

| Constraint | Notes |
|---|---|
| Source shed must be stopped | `shed snapshot create` errors with a `stop the shed first` message otherwise. |
| Same backend only | A VZ snapshot can only spawn VZ sheds; a Firecracker snapshot only Firecracker. The CLI surfaces this as a clear error. |
| `--from-snapshot` is mutually exclusive with `--image` and `--repo` | The snapshot rootfs is the source of truth. `--local-dir` and credential mounts are still allowed because they are runtime mounts. |
| Snapshot rootfs is immutable | Stored mode `0444`. Spawned sheds get a writable copy via reflink. |

## Storage layout

```
{snapshots_dir}/
  {snapshot-name}/
    snapshot.json    # metadata
    rootfs.ext4      # rootfs (mode 0444)
```

`snapshots_dir` defaults to `~/Library/Application Support/shed/vz/snapshots`
(VZ) or `/var/lib/shed/firecracker/snapshots` (Firecracker). Override via the
`snapshots_dir` field in `vz` or `firecracker` server config blocks.

Snapshots show up in `shed system df` under their own section. They are
**not** removed by `shed system prune` — deletion is always explicit via
`shed snapshot delete`.

When the host filesystem supports reflink (APFS clonefile, XFS/Btrfs/ext4
FICLONE), the snapshot's `rootfs.ext4` and any spawned shed's `rootfs.ext4`
share extents until they diverge. `shed system df` notes this so you don't
overcount on-disk usage.

## Out of scope

- Live (memory state) snapshots — Tier 1 captures rootfs only.
- Snapshot export/import / multi-host transfer.
- Snapshot lineage chains — only the immediate `source_shed` is recorded.

## Example: bootstrap, snapshot, experiment

```bash
# Set up a baseline shed
shed create base --image experimental
shed ssh base
# ...install your tools, customize the rootfs...
exit
shed stop base

# Snapshot
shed snapshot create base baseline-v1 --comment "agent + deps"

# Spawn an experiment shed from the snapshot
shed create experiment --from-snapshot baseline-v1
shed ssh experiment
# ...experiment freely; this shed is independent of the baseline...
exit

# Discard and try again
shed delete experiment
shed create experiment --from-snapshot baseline-v1
```
