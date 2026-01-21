# Shed Project Setup - Epic Plan

## Overview

**Project**: Shed - A tool for managing persistent, containerized development environments
**Repository**: github.com/charliek/shed
**Language**: Go 1.24
**Start Date**: 2026-01-20
**Status**: IN PROGRESS

## Project Description

Shed enables developers to spin up isolated coding sessions with AI tools (Claude Code, OpenCode) pre-installed, disconnect, and reconnect later to continue work. It consists of two binaries:

1. **`shed`** - CLI for developer machines (macOS, Linux)
2. **`shed-server`** - Server daemon exposing HTTP + SSH APIs (Linux)

### Key Features
- Simple CLI for creating and managing dev environments
- Session persistence (containers keep running after disconnect)
- Multi-server support (manage sheds across home servers and cloud VPS)
- IDE integration via SSH (Cursor/VS Code Remote-SSH)
- AI-ready with pre-configured Claude Code and OpenCode

### Architecture
- **HTTP API** (port 8080): REST API for CRUD operations
- **SSH Server** (port 2222): Terminal access, IDE remote connections
- **Docker**: Container and volume management

---

## Progress Tracker

| Phase | Name | Status | Started | Completed | Notes |
|-------|------|--------|---------|-----------|-------|
| 1 | Project Scaffold | COMPLETE | 2026-01-20 | 2026-01-20 | All deliverables verified |
| 2 | Core Types & Configuration | COMPLETE | 2026-01-20 | 2026-01-20 | All tests passing |
| 3 | Docker Client Wrapper | COMPLETE | 2026-01-20 | 2026-01-20 | |
| 4 | HTTP API Server | COMPLETE | 2026-01-20 | 2026-01-20 | |
| 5 | SSH Server | COMPLETE | 2026-01-20 | 2026-01-20 | |
| 6 | Server Binary | COMPLETE | 2026-01-20 | 2026-01-20 | |
| 7 | CLI Commands | COMPLETE | 2026-01-20 | 2026-01-20 | |
| 8 | SSH Config Management | COMPLETE | 2026-01-20 | 2026-01-20 | |
| 9 | Supporting Files | COMPLETE | 2026-01-20 | 2026-01-20 | |

**Overall Progress**: 9/9 phases complete

---

## Phase Summary

### Phase 1: Project Scaffold
Initialize Go module, create directory structure, set up Makefile and version package.
- **Deliverables**: go.mod, Makefile, directory structure, version package
- **Dependencies**: None
- **Detailed Plan**: [setup-1.md](./setup-1.md)

### Phase 2: Core Types & Configuration
Define shared data types and implement configuration loading for both server and client.
- **Deliverables**: types.go, server.go, client.go, example configs
- **Dependencies**: Phase 1
- **Detailed Plan**: [setup-2.md](./setup-2.md)

### Phase 3: Docker Client Wrapper
Implement Docker SDK wrapper for container and volume operations.
- **Deliverables**: docker/client.go, docker/volumes.go, docker/containers.go
- **Dependencies**: Phase 2
- **Detailed Plan**: [setup-3.md](./setup-3.md)

### Phase 4: HTTP API Server
Implement REST API endpoints using chi router.
- **Deliverables**: api/routes.go, api/handlers.go, api/middleware.go
- **Dependencies**: Phase 3
- **Detailed Plan**: [setup-4.md](./setup-4.md)

### Phase 5: SSH Server
Implement SSH server using gliderlabs/ssh with PTY support.
- **Deliverables**: sshd/server.go, sshd/session.go
- **Dependencies**: Phase 3
- **Detailed Plan**: [setup-5.md](./setup-5.md)

### Phase 6: Server Binary
Wire up HTTP and SSH servers into shed-server binary with systemd support.
- **Deliverables**: cmd/shed-server/main.go (complete)
- **Dependencies**: Phase 4, Phase 5
- **Detailed Plan**: [setup-6.md](./setup-6.md)

### Phase 7: CLI Commands
Implement shed CLI with all commands for server and shed management.
- **Deliverables**: cmd/shed/*.go files
- **Dependencies**: Phase 2
- **Detailed Plan**: [setup-7.md](./setup-7.md)

### Phase 8: SSH Config Management
Implement SSH config parsing and generation for IDE integration.
- **Deliverables**: sshconfig/parser.go, sshconfig/writer.go
- **Dependencies**: Phase 7
- **Detailed Plan**: [setup-8.md](./setup-8.md)

### Phase 9: Supporting Files
Create Docker base image, build scripts, and test scripts.
- **Deliverables**: Dockerfile, shell scripts
- **Dependencies**: Phase 6, Phase 7
- **Detailed Plan**: [setup-9.md](./setup-9.md)

---

## Key Dependencies (Go Modules)

```
github.com/docker/docker         # Docker SDK
github.com/gliderlabs/ssh        # SSH server library
github.com/go-chi/chi/v5         # HTTP router
github.com/spf13/cobra           # CLI framework
gopkg.in/yaml.v3                 # YAML parsing
```

---

## Key Design Decisions

1. **SSH Host Key**: Auto-generate ED25519 key at `/etc/shed/host_key` on first server start
2. **Console Command**: Use system SSH via os/exec (leverages existing SSH agent)
3. **Clone Failure Handling**: Rollback (delete container + volume) on git clone failure
4. **Config File Atomicity**: Write to temp file, then atomic rename
5. **Authentication**: Accept all SSH keys (Tailscale network is trust boundary for MVP)

---

## Deviations Log

| Date | Phase | Description | Reason | Impact |
|------|-------|-------------|--------|--------|
| 2026-01-20 | 6 | Added /etc/shed dir requirement | SSH host key storage needs a system directory | Requires `sudo mkdir -p /etc/shed && sudo chown $USER /etc/shed` before first run |

---

## Key Notes & Learnings

-

---

## Verification Checklist

- [x] Both binaries build successfully (`make build`)
- [x] Base Docker image builds (`./scripts/build-image.sh`) - 1.27GB
- [x] Server starts and responds to `/api/info`
- [x] CLI can add a server (`shed server add localhost`)
- [x] CLI can create a shed (`shed create test-shed`)
- [x] CLI can list sheds (`shed list`)
- [x] CLI can stop/start sheds (`shed stop/start`)
- [x] CLI can generate SSH config (`shed ssh-config`)
- [x] CLI can delete a shed (`shed delete test-shed --force`)
- [ ] Console interactive session (requires TTY)
- [ ] Full smoke test script

---

## Reference Documents

- **Specification**: `/home/charliek/projects/shed/docs/spec.md`
- **Phase Plans**: `./setup-1.md` through `./setup-9.md`
