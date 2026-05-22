# v0.5.1: Flatten + Host-Native Materialize

Status: landed on `release/v0.5.1` (2026-05-22). Documents the v0.5.1
materializer redesign: every shed boots from a single flattened erofs
lower built on the host. Reading time ~15 min.

## What shipped

v0.5.1 replaces the multi-layer-overlay + materialize-VM model with
the **bootc / Podman Machine pattern**:

- Distribution stays layered OCI on `ghcr.io/charliek/shed-*` (and any
  user-derived `FROM` images). Pull/push/storage/annotations unchanged.
- At materialize time on the host, every layer is read in OCI order
  and merged into one tree via `MergeLayersFromManifest`. OCI
  whiteouts (`.wh.foo`, `.wh..wh..opq`) are applied so the result is
  a coherent flattened rootfs.
- The merged tree is fed to `mkfs.erofs --tar=f -z lz4 -E
  force-inode-compact`, producing a single content-addressed erofs
  file at `{imagesDir}/cache/sha256/<manifest-digest>.erofs`.
- The shed VM boots with one writable upper (`/dev/vda`, per-shed
  ext4) and one read-only lower (`/dev/vdb`, the manifest erofs).
  Initramfs assembles a single-lower overlayfs and `switch_root`s.

This is what Lima, Colima, OrbStack, firecracker-containerd, and
Podman Machine v5+ do. Shed had been the odd one out with N-lower
overlays and a per-layer materialize VM.

## What was unwound

- `internal/vz/materializer.go` (the 430-LOC vfkit-driven materialize
  VM).
- `initramfs/init`'s `shed.mode=materialize` branch (~95 LOC of dd /
  gunzip / busybox tar / mkfs.erofs / magic-byte sanity).
- The multi-lower wait/mount loop and `shed.lowers=` cmdline parse
  in `initramfs/init`.
- The cache.go materializerHook dispatcher, RegisterMaterializer,
  ErrMaterializerUnavailable, materializeNativeLinux,
  materializeViaDockerFallback, and the per-layer EnsureLowerFromLayer.
- The legacy `.ext4` cache extension + dual-extension fallback
  bookkeeping (CacheLowerPathLegacy / RemoveCachedExt4 / etc.).
- Per-layer materialize calls in `PullToOCILayout` and `Convert` —
  the flatten happens lazily on the next EnsureImage instead.
- The initramfs's in-VM mkfs.erofs + libgcc_s.so.1 + busybox tar/
  gunzip staging, and the trixie-slim build base (back to
  bookworm-slim).
- `DefaultLayerSize`, `RootfsSize` option, and the `--size` CLI flag
  on `shed image build` (the flattened erofs is tightly-packed).

Net: ~1100 LOC deleted, ~500 added (`flatten.go` + the host-native
`EnsureLowerFromManifest` + tests + the linux-modules-extra-virtual
and systemd-firstboot mask fixes in `vz/Dockerfile`).

## Bug that motivated the redesign

The discovery preceding this redesign noted that `shed-agent.service`
never responded to vsock health checks on freshly-built v0.5.1 base
images, even when the materializer VM produced valid erofs lowers
(6/6) and the boot reached `network-online.target`. The earlier
analysis hypothesized a missing vsock kernel module
(`vmw_vsock_virtio_transport`) and recommended adding
`linux-modules-extra-virtual`.

The console.log preserved by `Metadata.PreserveConsoleLog` (added
this release) showed two separate issues both blocking boot:

1. `linux-image-virtual` does ship a minimal recommended-modules
   tree without the vsock guest-transport modules. The fix is to
   derive the matching `linux-modules-extra-<kvers>-generic` package
   name from the installed `linux-image-*-generic` (Noble doesn't
   expose a `linux-modules-extra-virtual` metapackage).
2. `systemd-firstboot.service` prompts interactively on /dev/console
   for locale / hostname / root password. With `Before=sysinit.target`
   it blocks `multi-user.target`, which `shed-agent.service` is
   `WantedBy=`. The transient machine-id symlink (→ /run/machine-id)
   makes `ConditionFirstBoot=yes` evaluate true on every boot, so the
   interactive wizard fires every boot. Masking the service skips it.

The flatten redesign is independent of these two fixes — both apply
just as cleanly to the multi-layer model. But the redesign is the
right architecture regardless: simpler, faster, fewer moving parts,
shared by every other tool in this space.

## Prerequisites

The host running `shed-server` needs `mkfs.erofs` on PATH:

- macOS: `brew install erofs-utils` (1.9.1 bottled).
- Debian/Ubuntu: `apt install erofs-utils`.

If absent, shed errors at first materialize attempt with a clear
install hint.

## Upgrade from v0.5.0

The cache layout changed from `<layer-digest>.erofs` (or `.ext4`) to
`<manifest-digest>.erofs`. v0.5.0 cache files are stranded — no
backwards-compat shim. The release notes call out a one-time

```
rm -rf ~/Library/Application\ Support/shed/{vz,firecracker}/cache
```

on upgrade, with `shed image prune` cleaning up automatically on the
next run.

## Historical: what the prior session tried

For posterity, the in-flight Phase 2 design (committed and then
unwound on this branch) was:

- `internal/vz/materializer.go`: a one-shot vfkit VM that ran
  `mkfs.erofs` on each layer.tar.gz, producing a per-layer erofs.
- `initramfs/init` had a `shed.mode=materialize` branch dispatched by
  cmdline that gunzipped → busybox tar extract → `mkfs.erofs --quiet`
  to a target block device, then powered off.
- The shed VM kernel insmod'd `erofs.ko` + `libcrc32c.ko` before
  mounting N erofs lowers as a stacked overlayfs.

Mechanically this worked — 6/6 layers materialized in ~30s total on
the VZ host, the shed VM booted to `network-online.target`. The
blocker was the systemd-firstboot interactive prompt described above,
which manifests the same in either the multi-layer or single-lower
design. Once that was diagnosed, the flatten path was both simpler
to debug and the well-known industry default — no reason to keep
shed as the snowflake.

Lessons:
- `mkfs.erofs --tar=f` on erofs-utils 1.8.6 (trixie) failed with
  `[Error 74] Bad message` on Ubuntu base layers (PAX/@LongLink
  records). 1.9.1 (Noble Homebrew bottle) handles them. The
  host-native path uses 1.9.1, so this is no longer a concern.
- `mkfs.erofs --aufs` converts AUFS markers to overlayfs xattrs
  but does NOT flatten — it preserves whiteouts for downstream
  overlay use. To actually merge layers you need to apply whiteouts
  yourself before feeding the tar to mkfs.erofs (or before extracting
  to a tree). That's what `flatten.go` does in Go.
- Preserving the boot log past instance-dir cleanup
  (`Metadata.PreserveConsoleLog`) is essential. Without it every
  failure investigation reduces to "rerun and hope it repeats."
