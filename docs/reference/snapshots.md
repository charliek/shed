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

A snapshot is an offline (Tier 1) clone of the rootfs only, which includes the
per-shed writable upper (the `/home/shed` home directory):

- ✅ Captured: everything in the rootfs and writable upper — installed packages,
  system config, the home directory under `/home/shed` (including any cloned
  `--repo` and home-directory dotfiles).
- ❌ Not captured: in-memory state (running processes, tmux sessions, page
  cache); host-backed mounts (`--local-dir`/`--add-dir` directory contents,
  configured `mounts:` syncs). These live outside the overlay, under
  `/home/shed/<basename>`.
- ⚠️  If the source shed used `--local-dir`/`--add-dir`, each host-backed
  directory is mounted from the host, so its contents are not in the rootfs.
  The CLI surfaces this as a warning at create time.

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
| `--from-snapshot` is mutually exclusive with `--image` and `--repo` | The snapshot rootfs is the source of truth. `--local-dir`/`--add-dir` and configured `mounts:` are still allowed because they are runtime mounts. |
| Snapshot rootfs is immutable | Stored mode `0444`. Spawned sheds get a writable (`0644`) copy via reflink. |

## `--from-snapshot` combined with `--local-dir`

These compose. `--from-snapshot` selects the rootfs; `--local-dir` is a
runtime VirtioFS / 9P mount bound at `/home/shed/<basename>` inside the VM.
Both can be set at the same time:

```bash
shed create work --from-snapshot baseline-v1 --local-dir /Users/me/proj
```

In that example the rootfs is the snapshot's (so installed tools / dotfiles
are present) but `/home/shed/proj` inside the VM is the host's `/Users/me/proj`,
not whatever the snapshot's rootfs held at that path. The host mount overlays
the captured home directory at that one basename; the rest of `/home/shed`
comes from the snapshot.

## Storage layout

```
{snapshots_dir}/
  {snapshot-name}/
    snapshot.json    # metadata (records lower_digest)
    rootfs.ext4      # rootfs (mode 0444)
```

`snapshots_dir` defaults to `~/Library/Application Support/shed/vz/snapshots`
(VZ) or `/var/lib/shed/firecracker/snapshots` (Firecracker). Override via the
`snapshots_dir` field in `vz` or `firecracker` server config blocks.

Snapshots show up in `shed system df` under their own section. They are
**not** removed by `shed system prune` — deletion is always explicit via
`shed snapshot delete`.

### Lower-digest pinning

Each snapshot records the `lower_digest` of the underlying image its
source shed was created from. This counts as a protective reference
against `shed image prune`, so a snapshot guarantees its source image
stays cached for as long as the snapshot exists. Spawning a shed from
a snapshot inherits that pin: the new shed's metadata also records
`lower_digest`, keeping the blob alive for the new shed's lifetime.

Snapshots and sheds created before this storage rewrite (schema v1)
are not loadable: pre-v2 metadata is rejected at runtime with an
explicit "delete and recreate" error. There is no in-place migration —
delete the old snapshot/shed and recapture from a freshly created
shed on the new layout.

When the host filesystem supports reflink (APFS clonefile, XFS/Btrfs/ext4
FICLONE), the snapshot's stored upper and any spawned shed's upper
share extents until they diverge. `shed system df` notes this so you
don't overcount on-disk usage.

### Missing lower digest

A snapshot only **pins** its source's `lower_digest` — it doesn't carry a
copy of the lower bytes. `shed image prune` refuses to delete a digest a
snapshot pins, so in normal operation the lower stays available. If an
operator removes a blob directory by hand (or moves the image store between
hosts), the pin's protection is bypassed and the lower may go missing.

`shed snapshot info` warns when this happens:

```text
Lower digest:   sha256:abc123... (MISSING — pull or rebuild the image before spawning)

Warning: this snapshot's lower image is no longer cached.
  shed create --from-snapshot <snap> will fail until you pull/rebuild the original image <ref>.
```

`shed create --from-snapshot` then fails fast with
`BACKEND_ERROR: snapshot ... references lower digest sha256:... which is no
longer cached; pull the original image (<ref>) first`. Recover by pulling
the original image (the digest is what the snapshot pins; the tag/label is
optional):

```bash
shed image pull <docker-ref>
shed create my-new-shed --from-snapshot <snap>
```

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
shed create base --image full
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
