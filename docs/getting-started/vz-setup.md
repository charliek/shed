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
`ghcr.io` and materializes each layer's ext4 cache. This takes a
minute on the first run; subsequent creates that share layers are
fast.

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
  base_rootfs: ghcr.io/charliek/shed-vz-full:v{version}
  images:
    base: ghcr.io/charliek/shed-vz-base:v{version}
    extensions: ghcr.io/charliek/shed-vz-extensions:v{version}
    full: ghcr.io/charliek/shed-vz-full:v{version}
```

Replace `{version}` with the version matching your `shed` binary — run `shed version` to check.

The first `shed create` pulls each layer blob, materializes the layer
ext4 cache, and boots. Subsequent variants reuse the shared layers.

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

See [Image Variants](../reference/images.md) for details. The OCI image
store lives under `~/Library/Application Support/shed/vz/` — see
[Storage Model](../reference/storage-model.md) and the
[upgrade-and-reclaim cookbook](../reference/images.md#cookbook-upgrading-image-versions) for how to manage disk space.

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
  kernel_path: ~/Library/Application Support/shed/vz/vmlinux
  initrd_path: ~/Library/Application Support/shed/vz/initrd.img
  base_rootfs: ghcr.io/charliek/shed-vz-full:v{version}
  images:
    base: ghcr.io/charliek/shed-vz-base:v{version}
    extensions: ghcr.io/charliek/shed-vz-extensions:v{version}
    full: ghcr.io/charliek/shed-vz-full:v{version}
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

```bash
./bin/shed-server serve
```

### 8. Create a test shed

```bash
shed server add localhost
shed create test
shed console test
```

## Configuration Reference

See [VZ Configuration](../reference/configuration.md#vz-configuration) for all available fields.

## How It Works

The VZ backend launches each VM as a `vfkit` subprocess. Communication with the guest uses vsock over per-port Unix sockets (one socket per port, named `<name>-<port>.sock`). This differs from Firecracker, which uses a single multiplexed socket with a CONNECT/OK handshake.

Networking uses NAT provided by Virtualization.framework. The guest obtains an IP via DHCP through `systemd-networkd`. From the host's perspective, `GetNetworkEndpoint` always returns `127.0.0.1`.

The rootfs is a standard ext4 image, same as Firecracker. Each instance gets its own copy.

## Troubleshooting

**"vfkit not found"**
: Install vfkit with `brew install vfkit` or add it to your PATH.

**Code signing errors**
: Re-sign the binary: `codesign --entitlements internal/vz/entitlements.plist -s - ./bin/shed-server`

**"Virtualization.framework not available"**
: Check that you're running macOS 13+ (Ventura or later).

**VM fails to boot**
: Verify `kernel_path`, `initrd_path`, and `base_rootfs` point to valid files. Check that the rootfs was built successfully. Check the console log at `<instance_dir>/<name>/console.log` for boot messages.

**Health check timeout**
: Check that vsock socket files exist in `~/.shed/vz/sockets/`. Verify vfkit is running with `ps aux | grep vfkit`. Check the console log for systemd boot errors.

**Permission denied**
: Ensure the entitlements plist is applied to the binary via code signing.
