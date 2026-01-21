# Phase 3: Docker Client Wrapper

## Overview
- **Phase**: 3 of 9
- **Epic**: [setup-epic.md](./setup-epic.md)
- **Status**: NOT STARTED
- **Estimated Effort**: Medium-Large
- **Dependencies**: Phase 2 complete

## Objective
Implement a Docker SDK wrapper that handles all container and volume operations for shed management. This is the core engine that creates, manages, and destroys development environments.

## Prerequisites
- Phase 2 complete (config types exist)
- Docker installed and running on the machine
- Docker SDK dependency added

## Context for New Engineers

### Naming Conventions
- **Container**: `shed-{name}` (e.g., `shed-codelens`)
- **Volume**: `shed-{name}-workspace` (e.g., `shed-codelens-workspace`)

### Labels
All shed containers are tagged with:
```
shed=true
shed.name={name}
shed.created={ISO8601 timestamp}
shed.repo={owner/repo}  # if created with --repo
```

### Container Lifecycle
1. Create volume
2. Create container with mounts and env vars
3. Start container
4. (Optional) Clone repo via docker exec
5. Container runs `sleep infinity` to stay alive
6. Console connections use `docker exec`

---

## Progress Tracker

| Task | Status | Notes |
|------|--------|-------|
| 3.1 Add Docker SDK dependency | NOT STARTED | |
| 3.2 Create client.go | NOT STARTED | |
| 3.3 Create volumes.go | NOT STARTED | |
| 3.4 Create containers.go | NOT STARTED | |
| 3.5 Write unit tests | NOT STARTED | |

---

## Detailed Tasks

### 3.1 Add Docker SDK Dependency

```bash
go get github.com/docker/docker/client
go get github.com/docker/docker/api/types
go get github.com/docker/docker/api/types/container
go get github.com/docker/docker/api/types/filters
go get github.com/docker/docker/api/types/mount
go get github.com/docker/docker/api/types/volume
```

### 3.2 Create Docker Client

**File**: `internal/docker/client.go`

```go
package docker

import (
    "context"
    "github.com/charliek/shed/internal/config"
    "github.com/docker/docker/client"
)

// Client wraps the Docker client with shed-specific functionality
type Client struct {
    docker *client.Client
    config *config.ServerConfig
    envVars map[string]string  // Loaded from env_file
}

// NewClient creates a new Docker client wrapper
func NewClient(cfg *config.ServerConfig) (*Client, error) {
    // Use client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
    // Load env vars from cfg.EnvFile if specified
}

// Close releases Docker client resources
func (c *Client) Close() error

// Helper functions
func containerName(shedName string) string  // returns "shed-{name}"
func volumeName(shedName string) string     // returns "shed-{name}-workspace"
func (c *Client) buildMounts() []mount.Mount  // Build from config credentials
func (c *Client) buildEnvList() []string      // Build from loaded env vars
```

### 3.3 Create Volume Operations

**File**: `internal/docker/volumes.go`

```go
package docker

import "context"

// CreateVolume creates a workspace volume for a shed
func (c *Client) CreateVolume(ctx context.Context, shedName string) error {
    // Volume name: shed-{name}-workspace
    // Check if exists first, return nil if already exists
}

// DeleteVolume removes a shed's workspace volume
func (c *Client) DeleteVolume(ctx context.Context, shedName string) error {
    // Ignore "not found" errors
}

// VolumeExists checks if a volume exists
func (c *Client) VolumeExists(ctx context.Context, shedName string) (bool, error)
```

### 3.4 Create Container Operations

**File**: `internal/docker/containers.go`

```go
package docker

import (
    "context"
    "time"
    "github.com/charliek/shed/internal/config"
)

// CreateShed creates a new shed container
func (c *Client) CreateShed(ctx context.Context, req config.CreateShedRequest) (*config.Shed, error) {
    // 1. Validate name (alphanumeric + hyphens only)
    // 2. Check if container already exists -> return error
    // 3. Create volume
    // 4. Create container with:
    //    - Image from req.Image or c.config.DefaultImage
    //    - Volume mounted at /workspace
    //    - Credential mounts from config
    //    - Environment variables
    //    - Labels: shed=true, shed.name, shed.created, shed.repo
    //    - Command: ["sleep", "infinity"]
    // 5. Start container
    // 6. If req.Repo specified:
    //    - docker exec: git clone git@github.com:{repo}.git /workspace/{repo-name}
    //    - If clone fails, rollback (stop + remove container + volume)
    // 7. Return Shed struct
}

// ListSheds returns all shed containers on this server
func (c *Client) ListSheds(ctx context.Context) ([]config.Shed, error) {
    // Use ContainerList with filter: label=shed=true
    // Map container state to status (running, stopped, etc.)
    // Extract labels for name, created, repo
}

// GetShed returns a specific shed by name
func (c *Client) GetShed(ctx context.Context, name string) (*config.Shed, error) {
    // Use ContainerList with filter: label=shed.name={name}
    // Return ErrShedNotFound if not found
}

// DeleteShed removes a shed container and optionally its volume
func (c *Client) DeleteShed(ctx context.Context, name string, keepVolume bool) error {
    // 1. Get container (error if not found)
    // 2. Stop container if running
    // 3. Remove container
    // 4. If !keepVolume, remove volume
}

// StartShed starts a stopped shed
func (c *Client) StartShed(ctx context.Context, name string) (*config.Shed, error) {
    // Get shed, verify stopped, start it
}

// StopShed stops a running shed
func (c *Client) StopShed(ctx context.Context, name string) (*config.Shed, error) {
    // Get shed, verify running, stop it (with timeout)
}

// ExecInShed executes a command in a shed container
func (c *Client) ExecInShed(ctx context.Context, name string, cmd []string, tty bool) (int, error) {
    // Create exec, attach, stream output, return exit code
}

// AttachToShed creates an interactive session
// Returns streams for stdin/stdout/stderr
func (c *Client) AttachToShed(ctx context.Context, name string, tty bool) (*ExecSession, error)

// ExecSession holds an active exec session
type ExecSession struct {
    Conn   net.Conn  // Hijacked connection
    Reader io.Reader
    Writer io.Writer
}

// Helper to validate shed name
func ValidateShedName(name string) error {
    // Must be alphanumeric + hyphens
    // Cannot start/end with hyphen
    // Max length 63 chars (DNS label limit)
}
```

### 3.5 Write Unit Tests

**File**: `internal/docker/client_test.go`
- Test container/volume naming functions
- Test mount building from config
- Test env var building

**File**: `internal/docker/containers_test.go`
- Test name validation
- Test with mock Docker client if feasible

---

## Deliverables Checklist

- [ ] Docker SDK dependencies added to go.mod
- [ ] `internal/docker/client.go` implemented
- [ ] `internal/docker/volumes.go` implemented
- [ ] `internal/docker/containers.go` implemented
- [ ] `ValidateShedName()` function works correctly
- [ ] Container naming follows convention
- [ ] Labels are properly set
- [ ] Credential mounts work
- [ ] Unit tests passing

---

## Deviations & Notes

| Item | Description |
|------|-------------|
| | |

---

## Completion Criteria
- All deliverables checked off
- `go test ./internal/docker/...` passes
- Can manually test with Docker if available
- Update epic progress tracker to mark Phase 3 complete
