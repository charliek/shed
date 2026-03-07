# Plan: macOS VZ Backend

## Context

Shed has Docker (containers) and Firecracker (microVMs, Linux-only) backends. macOS users can't use Firecracker. Apple's Virtualization.framework supports vsock, which means we can reuse shed-agent + agentproto unchanged. The `vfkit` project provides a Go config library + subprocess model (used by Podman, Minikube, CRC). The existing Firecracker rootfs pipeline (Docker -> ext4) is portable to VZ with minimal changes.

See `docs/mac_vm_discovery.md` for full research and decision rationale.

## Step 1: Add backend constants and config types

**Files to modify:**
- `internal/backend/backend.go` -- add `TypeVZ Type = "vz"`
- `internal/config/types.go` -- add `BackendVZ = "vz"`
- `internal/config/server.go` -- add `VZConfig` struct, update `isValidBackend()`, add `VZ *VZConfig` field to `ServerConfig`

```go
type VZConfig struct {
    VfkitPath    string `yaml:"vfkit_path"`     // path to vfkit binary (default: search PATH)
    KernelPath   string `yaml:"kernel_path"`    // decompressed vmlinuz
    BaseRootfs   string `yaml:"base_rootfs"`    // base ext4 image
    InstanceDir  string `yaml:"instance_dir"`   // per-shed instance storage
    SocketDir    string `yaml:"socket_dir"`     // runtime Unix sockets
    WorkspaceDir string `yaml:"workspace_dir"`  // host-side workspace mounts
    DefaultCPUs  int    `yaml:"default_cpus"`   // default: 2
    DefaultMemMB int    `yaml:"default_mem_mb"` // default: 4096
}
```

## Step 2: Create `internal/vz/` package (macOS-only)

All files tagged `//go:build darwin`.

### `backend.go` (~80 LOC)
- `VZBackend` struct wrapping `*Client`
- `var _ backend.Backend = (*VZBackend)(nil)` compile-time check
- Thin delegation to Client methods (same pattern as Docker and Firecracker)

### `client.go` (~300 LOC)
- `Client` struct holding `VZConfig` + `ServerConfig`
- `CreateShed`: generate vfkit config, copy rootfs, start vfkit process, wait for health, clone repo, provision
- `GetShed/ListSheds`: read metadata from instance directory
- `DeleteShed`: kill vfkit process, remove instance dir (optionally keep workspace)
- `StartShed/StopShed`: start/kill vfkit process, manage metadata

VM configuration uses `github.com/crc-org/vfkit/pkg/config`:
```go
bootloader := config.NewLinuxBootloader(kernelPath, cmdline, "")
vm := config.NewVirtualMachine(cpus, memMB, bootloader)
vm.AddDevice(config.VirtioBlkNew(rootfsPath))
vm.AddDevice(config.VirtioVsockNew(1024, consoleSock, true))
vm.AddDevice(config.VirtioVsockNew(1025, healthSock, true))
vm.AddDevice(config.VirtioFsNew(workspacePath, "workspace"))
cmd, _ := vm.Cmd(vfkitPath)
cmd.Args = append(cmd.Args, "--restful-uri", "unix://"+restSock)
cmd.Start()
```

### `vsock.go` (~100 LOC)
- Adapted from `internal/firecracker/vsock.go`
- Remove Firecracker's `CONNECT port\n` / `OK port\n` handshake
- Direct `net.Dial("unix", socketPath)` -> agentproto framing
- Same `Exec()`, `CheckHealth()`, `WaitForHealth()` methods
- Reuse `internal/agentproto` package unchanged

### `metadata.go` (~100 LOC)
- Same pattern as Firecracker: JSON file per instance
- Fields: name, pid, socket paths, rootfs path, workspace path, created_at, status

### `provisioning.go` (~150 LOC)
- Adapted from `internal/firecracker/provisioning.go`
- Uses vsock exec (via the adapted VsockClient) instead of Firecracker's vsock
- Same provisioning config loading, hook execution, state tracking

### `entitlements.plist`
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

## Step 3: Wire into server

**File: `cmd/shed-server/serve.go`**

Add case to the backend initialization switch:
```go
case config.BackendVZ:
    vzCfg := cfg.VZ
    if vzCfg == nil {
        vzCfg = config.DefaultVZConfig()
    }
    vzClient, err := vz.NewClient(vzCfg, cfg)
    if err != nil {
        return fmt.Errorf("failed to create vz client: %w", err)
    }
    backends[backend.TypeVZ] = vz.NewBackend(vzClient)
```

## Step 4: Image pipeline

### `vz/Dockerfile` (variant of `firecracker/Dockerfile`)
- Same base: Ubuntu 24.04 + systemd + shed-agent + tmux + tools
- Changed: network-setup.sh uses DHCP instead of kernel IP args
- Added: virtiofs mount in fstab (`workspace /workspace virtiofs defaults 0 0`)

### `scripts/build-vz-rootfs.sh`
- Same steps as `build-firecracker-rootfs.sh`
- Additional step: download and decompress Ubuntu kernel for VZ's LinuxBootloader
- Output: `base-rootfs.ext4` + `vmlinuz` (decompressed)

**Alternative (lower effort):** Make `network-setup.sh` detect the hypervisor at runtime, use ONE Dockerfile and ONE rootfs for both Firecracker and VZ. Only extra asset is the decompressed kernel.

## Step 5: Update configs and docs

- `configs/server.example.yaml` -- add VZ config section
- `docs/getting-started/` -- macOS setup guide
- Update CLI to pass `--backend vz`

## Key files to reference/reuse

| Existing file | Reuse for |
|--------------|-----------|
| `internal/firecracker/backend.go` | Pattern for `VZBackend` struct |
| `internal/firecracker/vsock.go` | Adapt for VZ vsock (remove handshake) |
| `internal/firecracker/provisioning.go` | Near-copy for VZ provisioner |
| `internal/firecracker/client.go` | Pattern for VM lifecycle management |
| `internal/agentproto/` | Use unchanged |
| `cmd/shed-agent/` | Use unchanged (same binary in VZ VMs) |
| `scripts/build-firecracker-rootfs.sh` | Adapt for VZ rootfs build |
| `firecracker/Dockerfile` | Adapt for VZ (networking only) |
| `internal/config/server.go` | Pattern for VZConfig |

## Verification

1. **Build:** `go build ./cmd/shed-server` on macOS (verify VZ package compiles)
2. **Build on Linux:** Verify VZ package is excluded, existing backends unaffected
3. **Code sign:** `codesign --entitlements internal/vz/entitlements.plist -s - ./shed-server`
4. **Build rootfs:** `./scripts/build-vz-rootfs.sh` (requires Docker on macOS)
5. **Create shed:** `shed create --backend vz test1` -- verify VM starts, health check passes
6. **Exec:** `ssh test1@localhost -p 2222` -- verify shell, TTY, resize
7. **Provisioning:** Create shed with `.shed/provision.yaml` -- verify hooks run
8. **Sessions:** `shed sessions list test1` -- verify tmux sessions
9. **Lifecycle:** `shed stop test1`, `shed start test1`, `shed delete test1`
10. **Workspace persistence:** Verify `/workspace` contents survive stop/start via virtiofs mount

## Estimated effort

~2-3 weeks total:
- Steps 1-3 (backend skeleton + wiring): 2-3 days
- Step 2 exec/vsock: 2-3 days (highest risk -- vsock adaptation + TTY testing)
- Step 4 (image pipeline): 1-2 days
- Step 5 (config/docs): 1 day
- Testing + debugging on macOS hardware: 3-5 days
