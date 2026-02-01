# Firecracker Backend Implementation Guide - Phase 2

> **Status**: Planned (after Phase 1 Backend Abstraction)
> **Prerequisites**: Complete `plans/backend-abstraction.md` first
> **Estimated Effort**: Multiple days

## Overview

This document outlines how to implement the Firecracker backend using the `backend.Backend` interface established in Phase 1. The Firecracker backend provides VM-level isolation for shed environments.

## Why Firecracker

Firecracker is a lightweight VMM (Virtual Machine Monitor) built by AWS that powers Lambda and Fargate:

- **~125ms boot time** — fast enough for interactive use
- **~5MB memory overhead** per VM — efficient for running multiple environments
- **Strong isolation** — full VM boundary, not just Linux namespaces
- **Safe privileged containers** — users can run `--privileged` Docker containers inside the VM without risking the host
- **Linux + KVM only** — acceptable since shed-server runs on Linux

## Prerequisites

After Phase 1 is complete, you have:
- `internal/backend/backend.go` - Backend interface definition
- `internal/backend/types.go` - ExecOptions, TerminalSize types
- `internal/docker/backend.go` - Reference implementation (DockerBackend)

## Architecture

```
┌──────────┐     SSH (2222)      ┌─────────────┐
│  Client  │ ──────────────────▶ │ shed-server │
└──────────┘   user=shedname     └─────────────┘
                                       │
                               ┌───────────────┐
                               │    Backend    │
                               │   Selection   │
                               └───────────────┘
                                 │           │
                    ┌────────────┘           └────────────┐
                    ▼                                     ▼
            ┌───────────────┐                    ┌───────────────┐
            │ Docker Backend│                    │  Firecracker  │
            │               │                    │    Backend    │
            │ docker exec   │                    │    vsock      │
            └───────────────┘                    └───────────────┘
                    │                                     │
                    ▼                                     ▼
            ┌───────────────┐                    ┌───────────────┐
            │   Container   │                    │  Firecracker  │
            │               │                    │      VM       │
            │               │                    │ ┌───────────┐ │
            │               │                    │ │shed-agent │ │
            │               │                    │ │  Docker   │ │
            └───────────────┘                    └─┴───────────┴─┘
```

## New Package Structure

```
internal/
  firecracker/
    backend.go       # FirecrackerBackend implementing backend.Backend
    vm.go            # VM lifecycle (create, start, stop)
    vsock.go         # vsock connection handling for Exec
    config.go        # Firecracker-specific configuration
    rootfs.go        # Rootfs image management
  agent/             # shed-agent binary (runs inside VMs)
    main.go
    vsock_server.go
    pty.go
```

## Implementation Mapping

### Backend Interface → Firecracker Implementation

| Interface Method | Firecracker Implementation |
|------------------|---------------------------|
| `Type()` | Returns `backend.TypeFirecracker` |
| `CreateShed()` | Copy base rootfs, create VM config, store metadata |
| `GetShed()` | Read metadata from state file |
| `ListSheds()` | Scan instance directory for metadata files |
| `DeleteShed()` | Stop VM if running, remove rootfs and metadata |
| `StartShed()` | Launch Firecracker process via firecracker-go-sdk |
| `StopShed()` | Send shutdown via vsock, then terminate process |
| `ListSessions()` | Exec `tmux list-sessions` via vsock |
| `KillSession()` | Exec `tmux kill-session` via vsock |
| `Exec()` | Connect to shed-agent via vsock, bridge PTY |

### Key Differences from Docker

1. **Container ID → VM Metadata**: Docker uses container IDs; Firecracker uses instance directory with metadata.json

2. **Docker Exec → vsock**: Docker uses ContainerExecCreate/Attach; Firecracker connects to vsock and communicates with shed-agent

3. **Docker Labels → State Files**: Metadata stored in `instances/{name}/metadata.json` instead of container labels

4. **Volumes → Rootfs Images**: Workspace is part of rootfs or separate virtio-fs mount

## shed-agent (New Component)

The shed-agent runs inside Firecracker VMs and handles:

1. **vsock Listener** (port 1024): Accepts console connections
2. **Health Check** (port 1025): Returns "ready" when VM is initialized
3. **PTY Management**: Spawns shell and bridges to vsock connection
4. **Graceful Shutdown**: Handles shutdown signals

**Protocol:**
```
Host: Connect to vsock CID:1024
Agent: Accept, allocate PTY, spawn /bin/bash
Host: Forward I/O bidirectionally
Agent: On disconnect, cleanup PTY
```

**Behavior Flow:**
```
VM Boot
   │
   ▼
systemd starts shed-agent.service
   │
   ▼
shed-agent listens on vsock port 1024
   │
   ├──── Health check (port 1025): responds "ready"
   │
   └──── Console connection (port 1024):
            │
            ▼
         Accept connection
            │
            ▼
         Allocate PTY
            │
            ▼
         Spawn /bin/bash (or configured shell)
            │
            ▼
         Bridge PTY ↔ vsock connection
            │
            ▼
         On disconnect: cleanup PTY
```

## Configuration

```yaml
# server.yaml
backend: firecracker  # or docker (default)

firecracker:
  kernel_path: /var/lib/shed/firecracker/vmlinux.bin
  base_rootfs: /var/lib/shed/firecracker/base-rootfs.ext4
  instance_dir: /var/lib/shed/firecracker/instances
  default_cpus: 2
  default_memory_mb: 4096
  default_disk_gb: 20
  vsock_base_cid: 100
  console_port: 1024
  health_port: 1025
```

## Backend Selection

Phase 1 adds `backend` field to ServerConfig. For Phase 2:

```go
// cmd/shed-server/serve.go
func createBackend(cfg *config.ServerConfig) (backend.Backend, error) {
    switch cfg.Backend {
    case "firecracker":
        return firecracker.NewBackend(cfg)
    case "docker", "":
        return docker.NewBackend(cfg)
    default:
        return nil, fmt.Errorf("unknown backend: %s", cfg.Backend)
    }
}
```

## CLI Changes for Phase 2

```bash
# New flags
shed create myproject --backend=firecracker
shed create myproject --isolated  # alias for --backend=firecracker
shed create myproject --cpus=4 --memory=8192  # resource allocation

# Modified output
shed list
# NAME        BACKEND      STATUS    CPUS  MEMORY
# myproject   firecracker  running   2     4096MB
# oldproject  docker       stopped   -     -
```

## Key Dependencies

```go
// go.mod additions
github.com/firecracker-microvm/firecracker-go-sdk
github.com/mdlayher/vsock
```

## Host Requirements

```bash
# KVM support
sudo apt install -y qemu-kvm
sudo modprobe kvm
sudo modprobe kvm_intel  # or kvm_amd
sudo usermod -aG kvm $USER

# Firecracker binary
ARCH="$(uname -m)"
LATEST=$(curl -fsSLI -o /dev/null -w %{url_effective} \
  https://github.com/firecracker-microvm/firecracker/releases/latest | xargs basename)
curl -fsSL "https://github.com/firecracker-microvm/firecracker/releases/download/${LATEST}/firecracker-${LATEST}-${ARCH}.tgz" | tar xz
sudo mv release-*/firecracker-* /usr/local/bin/firecracker
sudo mv release-*/jailer-* /usr/local/bin/jailer

# Verify
firecracker --version
```

## Rootfs Image

The root filesystem is an ext4 image containing a bootable Linux system with Docker pre-installed.

### Base Image Contents

- Ubuntu 24.04 LTS (or similar)
- systemd init system
- Docker CE
- shed-agent binary
- Basic development tools (git, curl, etc.)
- SSH server (optional, for debugging)

### Build Process

1. Create Dockerfile defining the rootfs contents
2. Build Docker image
3. Export container filesystem to ext4 image
4. Add any final configurations (fstab, systemd units)

### Image Management

```
/var/lib/shed/firecracker/
├── vmlinux.bin              # Linux kernel (downloaded or built)
├── base-rootfs.ext4         # Base image (built from Dockerfile)
└── instances/
    ├── myproject/
    │   ├── rootfs.ext4      # Copy-on-write or full copy of base
    │   ├── workspace.ext4   # Optional separate workspace volume
    │   └── metadata.json    # VM config, status, etc.
    └── another-project/
        └── ...
```

### Kernel

Use pre-built kernels from Firecracker's CI artifacts for MVP:

```bash
# Download kernel
curl -fsSL -o vmlinux.bin \
  "https://s3.amazonaws.com/spec.ccfc.min/firecracker-ci/v1.10/x86_64/vmlinux-5.10.217"
```

## Implementation Order

1. **Firecracker skeleton**: Implement interface with stubs
2. **VM lifecycle**: Create/Start/Stop/Delete with firecracker-go-sdk
3. **shed-agent**: Basic vsock server + PTY handling
4. **Exec via vsock**: Connect Exec() to shed-agent
5. **Session management**: tmux operations via Exec()
6. **Rootfs builder**: Dockerfile → ext4 image pipeline
7. **CLI flags**: --backend, --isolated, resource options

## Testing Strategy

1. **Unit tests**: Mock firecracker-go-sdk calls
2. **Integration tests**: Require KVM access, test full lifecycle
3. **Agent tests**: Test vsock + PTY in isolation

### Manual Testing Checklist

- [ ] `shed create test --backend=firecracker` creates VM
- [ ] `shed start test` boots VM, shed-agent becomes ready
- [ ] `shed console test` opens shell via vsock
- [ ] `shed exec test -- docker ps` runs command and returns
- [ ] `shed stop test` gracefully shuts down VM
- [ ] `shed delete test` removes VM and cleans up
- [ ] SSH via Cursor: connect to `test@server:2222`, get shell in VM
- [ ] Docker inside VM: `docker run hello-world` works
- [ ] Privileged container: `docker run --privileged ...` works safely

## Open Design Questions

1. **CID allocation**: Incremental from base, or random with collision detection?

2. **Rootfs strategy**: Full copy per instance vs copy-on-write (device-mapper, overlay)?

3. **Resource limits**: Should shed enforce CPU/memory limits, or leave to Firecracker defaults?

4. **Startup timeout**: How long to wait for shed-agent health check before declaring failure?

5. **Graceful vs forceful stop**: Always try graceful first with timeout, then force? Or make it configurable?

6. **Jailer**: Should we use Firecracker's jailer for additional isolation? Adds complexity but improves security.

7. **Networking**: None for MVP, but options for later include:
   - TAP devices with bridge networking
   - Tailscale installed inside the VM
   - Port forwarding via shed-server

8. **Filesystem mounting**: Options include:
   - virtio-fs / 9p mount (live file sync, requires virtiofsd)
   - Block device (separate ext4 volume)
   - Sync on create (simpler but requires rebuild)

## References

- [Firecracker GitHub](https://github.com/firecracker-microvm/firecracker)
- [Firecracker Getting Started](https://github.com/firecracker-microvm/firecracker/blob/main/docs/getting-started.md)
- [firecracker-go-sdk](https://github.com/firecracker-microvm/firecracker-go-sdk)
- [vsock documentation](https://man7.org/linux/man-pages/man7/vsock.7.html)
- [Julia Evans - Firecracker](https://jvns.ca/blog/2021/01/23/firecracker--start-a-vm-in-less-than-a-second/)
