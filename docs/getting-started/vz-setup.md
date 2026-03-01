# VZ Setup (macOS Apple Silicon)

This guide covers setting up the VZ backend, which uses Apple's Virtualization.framework to run Linux VMs on macOS via [vfkit](https://github.com/crc-org/vfkit).

## Prerequisites

- macOS 13+ (Ventura) on Apple Silicon (arm64)
- Go 1.24+ (for building from source)
- Docker (for building the rootfs image)
- Homebrew (for installing vfkit)

Intel macOS support is not currently available.

## Installation

### 1. Install vfkit

```bash
brew install vfkit
```

Or build from source:

```bash
git clone https://github.com/crc-org/vfkit.git
cd vfkit
make
sudo cp vfkit /usr/local/bin/
```

### 2. Build shed-server

```bash
git clone https://github.com/charliek/shed.git
cd shed
make build
```

### 3. Build the VZ rootfs

The rootfs build script creates an ext4 disk image, extracts the kernel, and extracts the initrd:

```bash
./scripts/build-vz-rootfs.sh
```

This produces:

- `~/Library/Application Support/shed/vz/base-rootfs.ext4` - Root filesystem
- `~/Library/Application Support/shed/vz/vmlinux` - Decompressed kernel
- `~/Library/Application Support/shed/vz/initrd.img` - Initial RAM disk

You can override the output directory with `OUTPUT_DIR`:

```bash
OUTPUT_DIR=/path/to/output ./scripts/build-vz-rootfs.sh
```

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

enabled_backends:
  - vz
default_backend: vz

vz:
  vfkit_path: vfkit
  kernel_path: ~/Library/Application Support/shed/vz/vmlinux
  initrd_path: ~/Library/Application Support/shed/vz/initrd.img
  base_rootfs: ~/Library/Application Support/shed/vz/base-rootfs.ext4
  instance_dir: ~/Library/Application Support/shed/vz/instances
  socket_dir: ~/.shed/vz/sockets  # Must not contain spaces (vfkit limitation)
  default_cpus: 2
  default_memory_mb: 4096
  default_disk_gb: 20
  start_timeout: 60s
  stop_timeout: 10s

credentials:
  git-ssh:
    source: ~/.ssh
    target: /home/shed/.ssh
    readonly: true
  git-config:
    source: ~/.gitconfig
    target: /home/shed/.gitconfig
    readonly: true
  claude:
    source: ~/.claude
    target: /home/shed/.claude
    readonly: false

env_file: ~/.shed/env
```

### 6. Code signing

The shed-server binary needs the `com.apple.security.virtualization` entitlement to use Virtualization.framework:

```bash
codesign --entitlements internal/vz/entitlements.plist -s - ./bin/shed-server
```

### 7. Start the server

```bash
./bin/shed-server serve
```

### 8. Create a test shed

```bash
shed create test --backend=vz
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
