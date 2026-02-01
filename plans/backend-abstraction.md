# Shed Backend Abstraction - Phase 1 Implementation Plan

> **Status**: Planned (deferred until Docker implementation is mature)
> **Prerequisites**: Complete Docker feature development first
> **Estimated Effort**: ~1 day of refactoring

## Overview

This plan covers Phase 1: Creating a Backend abstraction layer that enables multiple execution backends (Docker now, Firecracker later) while keeping all existing Docker functionality working identically.

## Current Architecture Summary

**Key Components:**
- `internal/docker/` - Docker client and container operations
- `internal/api/server.go` - HTTP API with `DockerClient` interface
- `internal/sshd/server.go` - SSH server with separate `DockerClient` interface
- `cmd/shed-server/serve.go` - Two adapters (`dockerAPIAdapter`, `dockerSSHAdapter`)
- `internal/config/types.go` - Shed, Session types and Docker label constants

**Current Flow:**
```
serve.go creates adapters
    ├── dockerAPIAdapter → api.Server (HTTP handlers)
    └── dockerSSHAdapter → sshd.Server (SSH sessions + exec)
```

## Design Decisions

### 1. Interface Location
Create `internal/backend/` package with a unified `Backend` interface that combines the needs of both API and SSH servers.

### 2. Backend Tracking
Add `shed.backend` Docker label for tracking. Existing sheds without this label default to "docker" for backwards compatibility.

### 3. Exec Complexity
The `ExecInContainer` logic (TTY, resize, I/O streams) currently lives in `dockerSSHAdapter`. This will move into `DockerBackend.Exec()`, keeping the interface clean.

### 4. Minimal Type Duplication
Reuse `config.Shed` and `config.Session` where possible. Add `Backend` field to `config.Shed`. The `sshd` package will use types from `backend` package.

---

## Implementation Steps

### Step 1: Create Backend Package Structure

**New files:**
```
internal/backend/
├── backend.go      # Backend interface
├── types.go        # ExecOptions, TerminalSize (moved from sshd)
└── errors.go       # Common error types
```

**`internal/backend/backend.go`:**
```go
package backend

type Type string

const (
    TypeDocker      Type = "docker"
    TypeFirecracker Type = "firecracker"
)

type Backend interface {
    // Identity
    Type() Type
    Close() error

    // Shed lifecycle
    CreateShed(ctx context.Context, req config.CreateShedRequest) (*config.Shed, error)
    GetShed(ctx context.Context, name string) (*config.Shed, error)
    ListSheds(ctx context.Context) ([]config.Shed, error)
    DeleteShed(ctx context.Context, name string, keepVolume bool) error
    StartShed(ctx context.Context, name string) (*config.Shed, error)
    StopShed(ctx context.Context, name string) (*config.Shed, error)

    // Sessions
    ListSessions(ctx context.Context, shedName string) ([]config.Session, error)
    KillSession(ctx context.Context, shedName, sessionName string) error

    // Execution (for SSH)
    Exec(ctx context.Context, shedName string, opts ExecOptions) error
}
```

**`internal/backend/types.go`:**
Move `ExecOptions`, `TerminalSize`, `ReadCloser`, `WriteCloser` from `sshd/server.go`.

### Step 2: Add Backend Field to Config Types

**Modify `internal/config/types.go`:**
```go
// Add to Shed struct
type Shed struct {
    Name        string    `json:"name"`
    Status      string    `json:"status"`
    CreatedAt   time.Time `json:"created_at"`
    Repo        string    `json:"repo,omitempty"`
    ContainerID string    `json:"container_id"`
    Backend     string    `json:"backend,omitempty"`  // NEW: "docker", "firecracker"
}

// Add label constant
const LabelShedBackend = "shed.backend"
```

**Modify `internal/config/server.go`:**
```go
type ServerConfig struct {
    // ... existing fields ...
    Backend string `yaml:"backend"` // "docker" (default) or "firecracker"
}
```

The `Backend` field defaults to "docker" if not specified in config.

### Step 3: Create Docker Backend Implementation

**New file `internal/docker/backend.go`:**
```go
package docker

type DockerBackend struct {
    client *Client  // existing docker.Client
}

func NewBackend(cfg *config.ServerConfig) (*DockerBackend, error) {
    client, err := NewClient(cfg)
    if err != nil {
        return nil, err
    }
    return &DockerBackend{client: client}, nil
}

func (b *DockerBackend) Type() backend.Type { return backend.TypeDocker }
func (b *DockerBackend) Close() error { return b.client.Close() }

// Lifecycle methods delegate to existing client methods
func (b *DockerBackend) CreateShed(ctx context.Context, req config.CreateShedRequest) (*config.Shed, error) {
    return b.client.CreateShed(ctx, req)
}
// ... other lifecycle methods (GetShed, ListSheds, etc.)

// Exec method - moves logic from dockerSSHAdapter.ExecInContainer
func (b *DockerBackend) Exec(ctx context.Context, shedName string, opts backend.ExecOptions) error {
    shed, err := b.client.GetShed(ctx, shedName)
    if err != nil {
        return err
    }
    // Existing exec logic from serve.go:210-309
}
```

### Step 4: Update Docker Container Creation

**Modify `internal/docker/containers.go` `CreateShed()`:**
- Add `LabelShedBackend: "docker"` to container labels
- Ensure `config.Shed` returned includes `Backend: "docker"`

**Modify `GetShed()` and `ListSheds()`:**
- Read `shed.backend` label, default to "docker" if missing

### Step 5: Update SSH Server to Use Backend

**Modify `internal/sshd/server.go`:**

Replace:
```go
type DockerClient interface {
    GetShed(ctx context.Context, name string) (*ShedInfo, error)
    StartShed(ctx context.Context, name string) error
    ExecInContainer(ctx context.Context, containerID string, opts ExecOptions) error
}
```

With:
```go
import "github.com/charliek/shed/internal/backend"

// Server now takes backend.Backend
type Server struct {
    backend    backend.Backend  // was: docker DockerClient
    // ... rest unchanged
}

func NewServer(b backend.Backend, hostKeyPath string, port int, termConfig *terminal.Config) (*Server, error)
```

**Modify `internal/sshd/session.go`:**
- Use `s.backend.GetShed()` returning `*config.Shed`
- Use `s.backend.StartShed()`
- Use `s.backend.Exec(shedName, opts)` instead of `ExecInContainer(containerID, opts)`

### Step 6: Update API Server to Use Backend

**Modify `internal/api/server.go`:**

Replace `DockerClient` interface with:
```go
import "github.com/charliek/shed/internal/backend"

type Server struct {
    backend    backend.Backend  // was: docker DockerClient
    cfg        *config.ServerConfig
    sshHostKey string
}

func NewServer(b backend.Backend, cfg *config.ServerConfig, sshHostKey string) *Server
```

Update handlers to call `s.backend.CreateShed()`, etc.

### Step 7: Simplify serve.go

**Modify `cmd/shed-server/serve.go`:**

Remove adapter types entirely. Replace with:
```go
func runServe(cmd *cobra.Command, args []string) error {
    cfg, err := loadConfig()
    if err != nil {
        return fmt.Errorf("failed to load config: %w", err)
    }

    // Create Docker backend directly
    dockerBackend, err := docker.NewBackend(cfg)
    if err != nil {
        return fmt.Errorf("failed to create backend: %w", err)
    }
    defer dockerBackend.Close()

    // Pass backend to both servers
    sshServer, err := sshd.NewServer(dockerBackend, DefaultHostKeyPath, cfg.SSHPort, cfg.Terminal)
    apiServer := api.NewServer(dockerBackend, cfg, hostKey)
    // ... rest unchanged
}
```

Delete `dockerAPIAdapter` and `dockerSSHAdapter` structs entirely.

---

## Files to Modify

| File | Change |
|------|--------|
| `internal/backend/backend.go` | NEW - Backend interface |
| `internal/backend/types.go` | NEW - ExecOptions, TerminalSize |
| `internal/backend/errors.go` | NEW - Common errors |
| `internal/docker/backend.go` | NEW - DockerBackend implementation |
| `internal/docker/containers.go` | Add backend label, populate Backend field |
| `internal/config/types.go` | Add Backend field to Shed, LabelShedBackend |
| `internal/config/server.go` | Add Backend field to ServerConfig |
| `internal/api/server.go` | Use backend.Backend instead of DockerClient |
| `internal/sshd/server.go` | Use backend.Backend, remove ShedInfo/ExecOptions types |
| `internal/sshd/session.go` | Update to use backend methods |
| `cmd/shed-server/serve.go` | Remove adapters, create DockerBackend directly |

---

## Verification Plan

### Unit Tests
1. Verify `DockerBackend` implements `backend.Backend` interface (compile-time check)
2. Add test for backend label in container creation

### Integration Tests
1. All existing tests should pass unchanged
2. `shed create` → verify container has `shed.backend=docker` label
3. `shed list` → verify Backend field populated in response

### Manual Testing Checklist

**Core Operations:**
- [ ] `shed create test` creates container with backend label
- [ ] `shed list` shows sheds (Backend field in JSON response)
- [ ] `shed console test` opens interactive shell
- [ ] `shed attach test` opens tmux session
- [ ] `shed exec test -- ls` runs command
- [ ] `shed stop test` stops container
- [ ] `shed start test` starts container
- [ ] `shed delete test` removes container
- [ ] SSH via `ssh test@localhost -p 2222` works
- [ ] Existing sheds without backend label still work (defaults to docker)

**Tunnels:**
- [ ] `shed tunnels config test` shows tunnel configuration
- [ ] `shed tunnels start test -t 8080` starts foreground tunnel
- [ ] `shed tunnels start test -t 8080 -d` starts background tunnel
- [ ] `shed tunnels list` shows active tunnels
- [ ] `shed tunnels stop test` stops tunnel
- [ ] Port forwarding works (curl localhost:8080 reaches container)
- [ ] Tunnel auto-starts stopped shed before connecting

---

## Decisions Made

1. **Backend config in server.yaml**: Add `backend: docker` field (defaults to "docker", ready for Phase 2)

2. **CLI changes**: Wait to add Backend column until Phase 2 when Firecracker exists

3. **Test coverage**: Rely on existing Docker tests + compile-time interface check

---

## Next Steps

After completing Phase 1, proceed to Phase 2: Firecracker Backend Implementation.
See `plans/firecracker-backend.md` for details.
