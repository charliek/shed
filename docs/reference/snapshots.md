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
correct hostname.

- **machine-id** is handled by the rootfs itself: `/etc/machine-id` is a
  symlink to `/run/machine-id` (tmpfs), and `systemd-machine-id-commit.service`
  is masked. PID 1 generates a fresh UUID into `/run/machine-id` at every VM
  boot; nothing persists to disk. Each shed (fresh-create OR snapshot-spawn)
  gets a unique machine-id, with no host-side ext4 manipulation required.
  *Note:* this means `/etc/machine-id` regenerates on every boot of the same
  shed, not just the first boot. For shed's ephemeral test-environment workflow
  this is fine, but applications that key persistent state on machine-id and
  expect it to be stable across reboots will see a regression.
- **Hostname and SSH host keys** are handled by a one-shot `shed-firstboot`
  service that runs early in boot — before D-Bus, journald, sshd, or
  `shed-agent` cache identity. firstboot writes `/etc/hostname` from the
  kernel cmdline `shed.name=<name>` value, then runs `ssh-keygen -A` so the
  new keys' comment field carries the spawn's hostname. It records the name
  in `/var/lib/shed/identity.json` and is idempotent — re-runs only when the
  recorded name doesn't match the cmdline value.

This makes `ssh known_hosts` and machine-id-based services work correctly
across snapshot-spawned sheds.

## Constraints

| Constraint | Notes |
|---|---|
| Source shed must be stopped | `shed snapshot create` errors with a `stop the shed first` message otherwise. |
| Same backend only | A VZ snapshot can only spawn VZ sheds; a Firecracker snapshot only Firecracker. The CLI surfaces this as a clear error. |
| `--from-snapshot` is mutually exclusive with `--image` and `--repo` | The snapshot rootfs is the source of truth. `--local-dir` and credential mounts are still allowed because they are runtime mounts. |
| Snapshot rootfs is immutable | Stored mode `0444`. Spawned sheds get a writable (`0644`) copy via reflink. |

## `--from-snapshot` combined with `--local-dir`

These compose. `--from-snapshot` selects the rootfs; `--local-dir` is a
runtime VirtioFS / 9P mount that overlays the workspace path inside the VM.
Both can be set at the same time:

```bash
shed create work --from-snapshot baseline-v1 --local-dir /Users/me/proj
```

In that example the rootfs is the snapshot's (so installed tools / dotfiles
are present) but `/workspace` inside the VM is the host's `/Users/me/proj`,
not whatever the snapshot's rootfs had at `/workspace`. If the source shed
also used `--local-dir`, the snapshot's `/workspace` is whatever the rootfs
held *before* the local dir was first mounted — typically empty — so the
overlay behavior matches what you'd intuitively expect.

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

## Known caveats

- **machine-id is not stable across reboots of the same shed.** Because
  `/etc/machine-id` is a tmpfs symlink (see "Identity regeneration"), every
  VM boot generates a fresh value. This is the trade-off for unique-per-VM
  identity without host-side ext4 manipulation. For applications that expect
  a stable machine-id across reboots, recreate the shed instead of
  stop+starting it.
- **A snapshot create that crashes mid-write may leave an "invisible"
  directory.** If the host crashes between writing `rootfs.ext4` and the
  atomic rename of `snapshot.json`, the directory under `snapshots_dir`
  will contain only `rootfs.ext4` and be filtered out of `shed snapshot
  list` (which requires `snapshot.json` to be present). Cleanup is manual
  for v1: remove the directory directly. `shed system prune` does not
  currently scan `snapshots_dir`.

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
