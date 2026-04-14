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

The first `shed create` pulls the VM image from the container registry and converts it to ext4. This takes a minute on the first run.

### Service management

```bash
brew services list                  # check status
brew services log shed              # view server logs
brew services log shed-host-agent   # view host agent logs
brew services restart shed          # restart after config changes
```

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

Configure your server to use published Docker image references. Shed auto-pulls and converts them to ext4 on first use:

```yaml
vz:
  base_rootfs: ghcr.io/charliek/shed-vz-experimental:{version}
  images:
    base: ghcr.io/charliek/shed-vz-base:{version}
    experimental: ghcr.io/charliek/shed-vz-experimental:{version}
```

Replace `{version}` with the version matching your `shed` binary — run `shed version` to check.

The first `shed create` will pull the image, convert it to ext4, and extract the kernel and initrd automatically.

#### Build images from source

```bash
./scripts/build-vz-rootfs.sh
```

This builds the `default` variant. Build other variants with `--variant`:

```bash
./scripts/build-vz-rootfs.sh --variant base           # Minimal image
./scripts/build-vz-rootfs.sh --variant experimental   # Default + credential brokering
./scripts/build-vz-rootfs.sh --all                    # All variants
```

See [Image Variants](../reference/images.md) for details.

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
  base_rootfs: ghcr.io/charliek/shed-vz-experimental:{version}
  images:
    base: ghcr.io/charliek/shed-vz-base:{version}
    experimental: ghcr.io/charliek/shed-vz-experimental:{version}
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
