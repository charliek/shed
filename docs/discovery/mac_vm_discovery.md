# macOS VM Backend Discovery

Research and analysis for adding a macOS VM backend to shed using Apple's Virtualization.framework.

## Problem

Shed's two existing backends are Linux-only or Linux-centric:
- **Docker**: containers, works anywhere Docker runs but not a VM
- **Firecracker**: microVMs with vsock + shed-agent, Linux-only (needs KVM)

macOS users have no native VM backend. The goal is to evaluate approaches for running shed environments inside Linux VMs on macOS.

## Options Evaluated

### Option A: Lima (CLI wrapping + SSH exec)

[Lima](https://github.com/lima-vm/lima) is a CNCF-incubating VM manager that runs Linux VMs on macOS (Apple Virtualization.framework or QEMU) and Linux (QEMU+KVM). It's used by Colima, Rancher Desktop, and others.

**Approach:** Wrap `limactl` CLI for VM lifecycle, use direct SSH (Go's `x/crypto/ssh`) for command execution.

| Dimension | Detail |
|-----------|--------|
| Effort | ~1-2 weeks, ~1,000 LOC |
| Platform | macOS + Linux |
| Exec model | SSH (new pattern, different from Firecracker's vsock) |
| Agent reuse | None -- SSH replaces shed-agent |
| External deps | `limactl` binary must be installed |
| VM startup | 15-60s (cloud-init overhead) |
| Build complexity | Simple (no CGo, no entitlements) |

**Pros:** Less code, works on Linux too, Lima handles VM plumbing (disk images, networking, mounts, cloud-init).

**Cons:** Different communication model than Firecracker (SSH instead of vsock), external dependency, slower startup, less control over VM behavior.

### Option B: Direct Apple VZ via vfkit (recommended)

Use Apple's Virtualization.framework directly, following the same architectural pattern as Firecracker: one VM per shed, vsock communication with shed-agent.

| Dimension | Detail |
|-----------|--------|
| Effort | ~2-3 weeks, ~800-1,000 LOC |
| Platform | macOS only (`//go:build darwin`) |
| Exec model | vsock + shed-agent (reused from Firecracker) |
| Agent reuse | 100% -- same binary |
| Protocol reuse | 100% -- same agentproto |
| External deps | `vfkit` binary (can be bundled) |
| VM startup | ~5-15s (no cloud-init, direct boot) |
| Build complexity | CGo required, macOS code signing with entitlements |

**Pros:** Reuses shed-agent/agentproto unchanged, consistent architecture across VM backends, no `limactl` dependency, faster startup.

**Cons:** macOS only, CGo + entitlements complexity, must build on macOS.

### Option C: Lima as Docker host (zero code)

Use Lima to run a single Linux VM with Docker inside (like Colima), then use the existing Docker backend unchanged.

```bash
limactl create --name shed-docker template://docker
limactl start shed-docker
# Point DOCKER_HOST at the Lima VM's Docker socket
```

Zero code changes, just documentation. But no per-shed VM isolation -- all sheds share one VM. Worth documenting as a quick-start for macOS users regardless.

## Recommendation: Direct Apple VZ (Option B)

The effort gap between Lima and Direct VZ narrowed significantly once we discovered the vfkit library. Direct VZ provides better architecture consistency (one exec model for all VM backends) and reuses tested code (shed-agent, agentproto) rather than introducing a second communication path.

## Key Technical Findings

### vfkit: High-level Go library for Apple VZ

[vfkit](https://github.com/crc-org/vfkit) (`github.com/crc-org/vfkit/pkg/config`) is a Go config library + CLI hypervisor for Apple's Virtualization.framework. It's used in production by Podman (5.0+), Minikube (1.35.0+), and CRC.

**Architecture:** vfkit is a subprocess model. The Go `pkg/config` package builds a VM configuration and generates an `exec.Cmd` that launches a `vfkit` process. Each VM runs in its own `vfkit` process -- the same pattern as Firecracker where each VM is a separate `firecracker` process.

**What vfkit provides:**
- `VirtualMachine` config with `NewVirtualMachine(vcpus, memoryMiB, bootloader)`
- `VirtioBlk` -- disk image attachment
- `VirtioVsock` -- vsock port mapped to host Unix socket
- `VirtioFs` -- virtiofs shared directories between host and guest
- `LinuxBootloader` -- direct kernel boot (needs uncompressed kernel on Apple Silicon)
- `EFIBootloader` -- EFI boot from disk image with GRUB
- REST API for VM state queries (`--restful-uri unix:///path/to/sock`)
- Cloud-init / Ignition support

**VM configuration example:**

```go
import "github.com/crc-org/vfkit/pkg/config"

bootloader := config.NewLinuxBootloader(
    "/var/lib/shed/vz/vmlinuz",        // uncompressed kernel
    "console=hvc0 root=/dev/vda rw",   // kernel args
    "",                                 // initrd (optional)
)

vm := config.NewVirtualMachine(2, 4096, bootloader)

blk, _ := config.VirtioBlkNew("/var/lib/shed/vz/instances/myshed/rootfs.ext4")
vm.AddDevice(blk)

vsock, _ := config.VirtioVsockNew(1024, "/run/shed/vz/myshed-console.sock", true)
vm.AddDevice(vsock)

fs, _ := config.VirtioFsNew("/Users/dev/.shed/workspaces/myshed", "workspace")
vm.AddDevice(fs)
// Guest mounts: mount -t virtiofs workspace /workspace

cmd, _ := vm.Cmd("/usr/local/bin/vfkit")
cmd.Args = append(cmd.Args, "--restful-uri", "unix:///run/shed/vz/myshed-rest.sock")
cmd.Start()
```

### vsock: shed-agent reuse

Apple VZ supports vsock via `VZVirtioSocketDeviceConfiguration`. vfkit maps each vsock port to a dedicated Unix socket on the host. This eliminates Firecracker's handshake protocol:

```
FIRECRACKER                        APPLE VZ (via vfkit)
shed-server                        shed-server
    |                                  |
    | connect to <name>.vsock          | connect to <name>-console.sock
    | send "CONNECT 1024\n"            | (direct connection, no handshake)
    | read "OK 1024\n"                 |
    |                                  |
    v                                  v
shed-agent (vsock port 1024)       shed-agent (vsock port 1024)
    [identical binary]                 [identical binary]
```

- **shed-agent** runs unchanged in the VZ VM -- same Linux binary, same vsock listeners on ports 1024/1026
- **agentproto** framing protocol works unchanged -- same message types, same encoding
- **Host-side vsock client** needs minor adaptation: remove the Firecracker `CONNECT/OK` handshake, connect directly to the per-port Unix socket

### Image pipeline: portable from Firecracker

The existing Firecracker rootfs build pipeline (`scripts/build-firecracker-rootfs.sh`) does:
1. Build shed-agent binary (linux/amd64)
2. Build Docker image from `firecracker/Dockerfile` (Ubuntu 24.04 + systemd + shed-agent + tools)
3. `docker create` + `docker export` -> tar
4. tar -> ext4 disk image (20GB sparse)

**This pipeline is directly portable to VZ.** The ext4 rootfs content (systemd, shed-agent, tmux, tools) is hypervisor-agnostic. Only two things differ:

1. **Networking:** Firecracker's `network-setup.sh` reads static IP from kernel command line args. VZ uses NAT with DHCP. The script needs a DHCP variant, or can detect the hypervisor at runtime:
   ```bash
   if grep -q "firecracker" /sys/class/dmi/id/board_vendor 2>/dev/null; then
       configure_static_ip  # Firecracker: IP from kernel args
   else
       dhclient $(ip -o link show | awk -F: '/^[0-9]+: e/{print $2; exit}')  # VZ: DHCP
   fi
   ```

2. **Kernel:** VZ with `LinuxBootloader` needs a decompressed kernel binary (Apple Silicon can't decompress). This is a build-script addition, not a Dockerfile change.

**Possible to share one rootfs for both backends** with runtime hypervisor detection. The only extra build artifact for VZ is the decompressed kernel.

### Workspace storage via virtiofs

VZ supports virtiofs for high-performance host-guest filesystem sharing. The workspace lives on the host at `~/.shed/workspaces/<name>/` and is mounted inside the VM at `/workspace`. This gives:
- Persistence independent of VM lifecycle
- Easy `keepVolume` support (just don't delete the host directory)
- Host-side access to workspace files

### Networking

VZ provides NAT networking by default. The guest gets an IP via DHCP but it's not directly routable from the host. This is fine for shed because:
- Primary access path (SSH -> shed-server -> vsock exec) doesn't use networking
- Lima-style port forwarding can expose guest services on localhost
- Bridged networking available with `com.apple.vm.networking` entitlement (optional)

`GetNetworkEndpoint` returns `127.0.0.1` and relies on port forwarding for service access.

## CGo, Entitlements, and Build

### Build requirements

```bash
# Standard go build -- CGo is handled internally by vz package
GOOS=darwin GOARCH=arm64 go build -o shed-server ./cmd/shed-server

# Must build ON macOS (cannot cross-compile from Linux due to CGo/Obj-C runtime)
# CAN cross between Mac architectures:
GOOS=darwin GOARCH=amd64 go build -o shed-server-amd64 ./cmd/shed-server
```

### Entitlements

Only `com.apple.security.virtualization` is required:

```xml
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN"
  "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>com.apple.security.virtualization</key>
  <true/>
</dict>
</plist>
```

### Code signing

```bash
# Ad-hoc signing (no Apple Developer account needed, works for local dev)
codesign --entitlements entitlements.plist -s - ./shed-server
```

Without signing: Apple Silicon Macs won't run the binary at all. Intel Macs get auto-signed by the linker but lack entitlements, so VZ API calls fail.

For distribution (Homebrew, etc.), a Developer ID certificate + notarization would be needed.

### Build tags

All VZ backend files use `//go:build darwin`. On Linux builds, the VZ package is simply absent -- the router won't have it as an option. No stub files needed. Same pattern as Firecracker's `//go:build linux`.

### CI/CD

- Must use macOS GitHub Actions runners (`macos-14` for ARM, `macos-13` for Intel)
- Universal binary via `lipo -create`
- Linux builds continue unchanged (VZ excluded by build tags)

### macOS version requirement

macOS 13+ (Ventura) for full feature set (virtiofs, EFI boot). macOS 11 (Big Sur) is the absolute minimum for Virtualization.framework but lacks virtiofs.

## Side-by-side Comparison

| Dimension | Lima | Direct VZ |
|-----------|------|-----------|
| Effort | ~1-2 weeks, ~1,000 LOC | ~2-3 weeks, ~800-1,000 LOC |
| Platform | macOS + Linux | macOS only |
| Architecture consistency | New pattern (SSH exec) | Same pattern as Firecracker (vsock + agent) |
| shed-agent reuse | None | 100% |
| External deps | `limactl` must be installed | `vfkit` binary (can bundle) |
| VM startup | 15-60s | ~5-15s |
| Image building | Lima handles it | Reuse Firecracker's Docker->ext4 pipeline |
| Build on macOS | No CGo needed | CGo + entitlements required |
| Maintenance | Two exec models to maintain | One exec model for all VM backends |

## References

- [vfkit](https://github.com/crc-org/vfkit) -- Go config library + macOS hypervisor (used by Podman, Minikube, CRC)
- [Code-Hex/vz](https://github.com/Code-Hex/vz) -- Low-level Go bindings for Apple Virtualization.framework
- [Lima](https://github.com/lima-vm/lima) -- Linux VM manager (CNCF incubating)
- [Apple Virtualization.framework docs](https://developer.apple.com/documentation/virtualization)
- [vfkit pkg/config API](https://pkg.go.dev/github.com/crc-org/vfkit/pkg/config)
