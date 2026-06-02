# VZ Setup (macOS Apple Silicon)

This guide covers setting up the VZ backend, which uses Apple's Virtualization.framework to run Linux VMs on macOS via [vfkit](https://github.com/crc-org/vfkit).

## Prerequisites

- macOS 13+ (Ventura) on Apple Silicon (arm64)
- Docker (for VM image management)

Intel macOS support is not currently available.

## Homebrew Install (Recommended)

The fastest way to get started. Homebrew handles vfkit, code signing, config generation, and service management.

### 1. Install

```bash
brew install charliek/tap/shed
```

This installs `shed` (CLI) and `shed-server`, installs the vfkit dependency, generates a default server config with version-pinned VZ images, and codesigns the server binary.

For credential brokering (SSH agent forwarding, AWS credentials, Docker registry auth):

```bash
brew install charliek/tap/shed-host-agent
```

### 2. Configure

Edit the server config to enable credential mounts and extensions:

```bash
# Open the config in your editor
$EDITOR $(brew --prefix)/etc/shed/server.yaml
```

Uncomment the `credentials` section to mount tool configs into VMs, and the `extensions` section if you installed `shed-host-agent`.

### 3. Start services

```bash
brew services start shed
brew services start shed-host-agent  # if installed
```

### 4. Create a test shed

```bash
shed server add localhost
shed create test
shed console test
```

The first `shed create` pulls the OCI image registry-direct from
`ghcr.io` and materializes the flattened erofs lower via host-native
`mkfs.erofs --tar=f`. The pull step dominates wallclock on the first
run (registry I/O); subsequent creates from the same manifest mount
the cached erofs in under a second.

### Service management

```bash
brew services list                  # check status
brew services restart shed          # restart after config changes
```

Logs are at `$(brew --prefix)/var/log/shed-server.log` and `$(brew --prefix)/var/log/shed-host-agent.log`.

## Upgrading from v0.4.x

The image store schema changed from v2 to v3 with the OCI image
rollout. Pre-v3 sheds, snapshots, and cached blobs are not migrated
automatically — the in-guest initramfs rejects them.

To upgrade:

```bash
brew services stop shed

# Wipe the legacy store. Backup first if anything in there is precious.
rm -rf ~/Library/Application\ Support/shed/vz/blobs
rm -rf ~/Library/Application\ Support/shed/vz/instances
rm -rf ~/Library/Application\ Support/shed/vz/snapshots
rm -rf ~/Library/Application\ Support/shed/vz/uppers

brew upgrade charliek/tap/shed
brew services start shed
sudo shed-server pull-images
```

Workspace data under `--local-dir` mounts is unaffected. Workspace data
that lived only inside the deleted upper layers is lost, by design.

## Build from Source (Alternative)

Use this if you're contributing to shed or need a custom build.

### 1. Install vfkit

```bash
brew install vfkit
```

### 2. Build shed-server

```bash
git clone https://github.com/charliek/shed.git
cd shed
make build
```

### 3. Set up VZ images

#### Published images (recommended)

Configure your server to use published OCI references. Shed pulls them
registry-direct (no Docker daemon needed) and stacks the layers as
overlayfs lowers on first boot.

```yaml
vz:
  default_image: ghcr.io/charliek/shed-vz-full:v0.5.1
  image_aliases:
    base: ghcr.io/charliek/shed-vz-base:v0.5.1
    extensions: ghcr.io/charliek/shed-vz-extensions:v0.5.1
    full: ghcr.io/charliek/shed-vz-full:v0.5.1
  pull_policy: missing
```

Pin a concrete version that matches the `shed-server` you built.
v0.5.1 published images are live now — check
<https://github.com/charliek/shed/pkgs/container/shed-vz-full> for tags.
Pre-v0.5.0 published images use the legacy flattened layout and will
not work with v0.5.0+ `shed-server`; v0.5.0 cached images use the
per-layer cache layout and need a one-time
`rm -rf {images_dir}/cache` on upgrade to v0.5.1.

The first `shed create` pulls each layer blob, flattens them into a
single erofs lower at `cache/sha256/<manifest-digest>.erofs`, and
boots. Subsequent sheds from the same manifest reuse the cached erofs
directly; other variants that share layer blobs skip the registry pull
but build their own flattened erofs the first time they're used.

#### Build images from source

```bash
./scripts/build-vz-rootfs.sh
```

This builds the `full` variant. Build other variants with `--variant`:

```bash
./scripts/build-vz-rootfs.sh --variant base         # Minimal image
./scripts/build-vz-rootfs.sh --variant extensions   # Base + credential brokering
./scripts/build-vz-rootfs.sh --all                  # All variants
```

The script writes directly into the local blob store and advances the
`base`, `extensions`, and `full` tags. Confirm with `shed image ls`.

See [Image Variants](../reference/images.md) for details. The OCI image
store lives under `~/Library/Application Support/shed/vz/` — see
[Storage Model](../reference/storage-model.md) and the
[upgrade-and-reclaim cookbook](../reference/images.md#cookbook-upgrading-image-versions) for how to manage disk space.

#### Configure server for local builds

When you build images locally, label them with `shed image build/pull -t
<label>` and reference the label. You can either set `default_image` to a
label (used when no `--image` is passed) or omit it and pass `--image
<label>` on every create:

```yaml
vz:
  vfkit_path: vfkit
  instance_dir: ~/Library/Application Support/shed/vz/instances
  socket_dir: ~/.shed/vz/sockets
  default_image: full   # a local label, or a ghcr.io/...:vX ref
```

Then create (omit `--image` to use `default_image`, or override per shed):

```bash
shed create test            # uses default_image
shed create dev --image base
```

`default_image` may be a Docker ref (`ghcr.io/...:vX`), an `image_aliases`
name, a local label set with `-t`, or an absolute path to an ext4/erofs
image. Docker refs are pulled on first use per `pull_policy`.

### 4. Create directories

```bash
mkdir -p ~/Library/Application\ Support/shed/vz/instances
mkdir -p ~/.shed/vz/sockets
```

### 5. Configure the server

Create `~/.config/shed/server.yaml`:

```yaml
name: my-mac
http_port: 8080
ssh_port: 2222
default_backend: vz

vz:
  vfkit_path: vfkit
  # kernel_path and initrd_path are optional fallbacks for OCI images.
  # The published / locally-built image blobs include their own kernel
  # and initramfs via manifest annotations; these fields only apply if
  # you're booting a legacy raw-rootfs image (rare).
  # kernel_path: ~/Library/Application Support/shed/vz/vmlinux
  # initrd_path: ~/Library/Application Support/shed/vz/initrd.img
  default_image: ghcr.io/charliek/shed-vz-full:v0.5.1
  image_aliases:
    base: ghcr.io/charliek/shed-vz-base:v0.5.1
    extensions: ghcr.io/charliek/shed-vz-extensions:v0.5.1
    full: ghcr.io/charliek/shed-vz-full:v0.5.1
  pull_policy: missing
  instance_dir: ~/Library/Application Support/shed/vz/instances
  socket_dir: ~/.shed/vz/sockets
  default_cpus: 2
  default_memory_mb: 4096
  default_disk_gb: 20
  console_port: 1024
  notify_port: 1026
  tcp_proxy_port: 1028
  start_timeout: 60s
  stop_timeout: 10s

credentials:
  claude:
    source: ~/.shed/mounts/claude
    target: /home/shed/.claude
    readonly: false

env_file: ~/.shed/env
```

### 6. Code signing

The shed-server binary needs the `com.apple.security.virtualization` entitlement:

```bash
codesign --entitlements internal/vz/entitlements.plist -s - ./bin/shed-server
```

### 7. Start the server

Pass `--config` explicitly when your config lives outside the current
working directory. `shed-server` without `--config` only looks at
`./server.yaml` (CWD), **not** `~/.config/shed/server.yaml`.

```bash
./bin/shed-server serve --config ~/.config/shed/server.yaml
```

### 8. Create a test shed

`shed server add` registers the server under the `name:` field from
`server.yaml`, not literally as `localhost`. Confirm with
`shed server list` before creating a shed.

```bash
shed server add localhost
shed server list                  # shows the registered name (e.g. "my-mac")
shed create test --image full     # required when default_image is omitted
shed console test
```

## Configuration Reference

See [VZ Configuration](../reference/configuration.md#vz-configuration) for all available fields.

## How It Works

The VZ backend launches each VM as a `vfkit` subprocess. Communication with the guest uses vsock over per-port Unix sockets (one socket per port, named `<name>-<port>.sock`). This differs from Firecracker, which uses a single multiplexed socket with a CONNECT/OK handshake.

Networking uses NAT provided by Virtualization.framework. The guest obtains an IP via DHCP through `systemd-networkd`. From the host's perspective, the guest IP isn't routable; service traffic from the host goes through `DialService`'s vsock TCP-proxy hop. `shed list` and the API report `127.0.0.1` as the shed's IP so the field has a sensible value, but no host code dials that address.

The rootfs is a standard ext4 image, same as Firecracker. Each instance gets its own copy.

## Troubleshooting

**"vfkit not found"**
: Install vfkit with `brew install vfkit` or add it to your PATH.

**Code signing errors**
: Re-sign the binary: `codesign --entitlements internal/vz/entitlements.plist -s - ./bin/shed-server`

**"Virtualization.framework not available"**
: Check that you're running macOS 13+ (Ventura or later).

**VM fails to boot**
: Verify `kernel_path`, `initrd_path`, and `default_image` point to valid files (or a pullable ref). Check that the rootfs was built successfully. Check the console log at `<instance_dir>/<name>/console.log` for boot messages.

**Health check timeout**
: Check that vsock socket files exist in `~/.shed/vz/sockets/`. Verify vfkit is running with `ps aux | grep vfkit`. Check the console log for systemd boot errors.

**Permission denied**
: Ensure the entitlements plist is applied to the binary via code signing.
