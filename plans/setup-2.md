# Phase 2: Core Types & Configuration

## Overview
- **Phase**: 2 of 9
- **Epic**: [setup-epic.md](./setup-epic.md)
- **Status**: NOT STARTED
- **Estimated Effort**: Medium
- **Dependencies**: Phase 1 complete

## Objective
Define all shared data types used across packages and implement configuration loading for both the server and client components.

## Prerequisites
- Phase 1 complete (project scaffold exists)
- go.mod initialized

## Context for New Engineers
This phase establishes the data structures that flow through the entire system:
- **Types**: Shed, ServerInfo, API request/response structures, error types
- **Server Config**: Loaded from YAML, defines ports, credentials mounts, env file
- **Client Config**: Stores known servers, default server, cached shed locations

Configuration files use YAML format with `gopkg.in/yaml.v3`.

---

## Progress Tracker

| Task | Status | Notes |
|------|--------|-------|
| 2.1 Add yaml.v3 dependency | NOT STARTED | |
| 2.2 Create types.go | NOT STARTED | |
| 2.3 Create server.go | NOT STARTED | |
| 2.4 Create client.go | NOT STARTED | |
| 2.5 Create example configs | NOT STARTED | |
| 2.6 Write unit tests | NOT STARTED | |

---

## Detailed Tasks

### 2.1 Add YAML Dependency

```bash
go get gopkg.in/yaml.v3
```

### 2.2 Create Types Package

**File**: `internal/config/types.go`

Define these types:

```go
package config

import "time"

// Shed represents a development environment container
type Shed struct {
    Name        string    `json:"name" yaml:"name"`
    Status      string    `json:"status" yaml:"status"`
    CreatedAt   time.Time `json:"created_at" yaml:"created_at"`
    Repo        string    `json:"repo,omitempty" yaml:"repo,omitempty"`
    ContainerID string    `json:"container_id" yaml:"container_id"`
}

// Shed status constants
const (
    StatusRunning  = "running"
    StatusStopped  = "stopped"
    StatusStarting = "starting"
    StatusError    = "error"
)

// ServerInfo returned by GET /api/info
type ServerInfo struct {
    Name     string `json:"name"`
    Version  string `json:"version"`
    SSHPort  int    `json:"ssh_port"`
    HTTPPort int    `json:"http_port"`
}

// SSHHostKeyResponse returned by GET /api/ssh-host-key
type SSHHostKeyResponse struct {
    HostKey string `json:"host_key"`
}

// ShedsResponse returned by GET /api/sheds
type ShedsResponse struct {
    Sheds []Shed `json:"sheds"`
}

// CreateShedRequest for POST /api/sheds
type CreateShedRequest struct {
    Name  string `json:"name"`
    Repo  string `json:"repo,omitempty"`
    Image string `json:"image,omitempty"`
}

// APIError for error responses
type APIError struct {
    Error APIErrorDetail `json:"error"`
}

type APIErrorDetail struct {
    Code    string `json:"code"`
    Message string `json:"message"`
}

// Error codes
const (
    ErrShedNotFound       = "SHED_NOT_FOUND"
    ErrShedAlreadyExists  = "SHED_ALREADY_EXISTS"
    ErrShedAlreadyRunning = "SHED_ALREADY_RUNNING"
    ErrShedAlreadyStopped = "SHED_ALREADY_STOPPED"
    ErrInvalidShedName    = "INVALID_SHED_NAME"
    ErrCloneFailed        = "CLONE_FAILED"
    ErrDockerError        = "DOCKER_ERROR"
    ErrInternalError      = "INTERNAL_ERROR"
)
```

### 2.3 Create Server Configuration

**File**: `internal/config/server.go`

Key functionality:
- Load from multiple locations: `./server.yaml`, `~/.config/shed/server.yaml`, `/etc/shed/server.yaml`
- Expand `~` in paths
- Apply defaults (http_port: 8080, ssh_port: 2222)
- Parse credential mounts
- Load environment file

```go
package config

// ServerConfig represents server-side configuration
type ServerConfig struct {
    Name         string                `yaml:"name"`
    HTTPPort     int                   `yaml:"http_port"`
    SSHPort      int                   `yaml:"ssh_port"`
    DefaultImage string                `yaml:"default_image"`
    Credentials  map[string]MountConfig `yaml:"credentials"`
    EnvFile      string                `yaml:"env_file"`
    LogLevel     string                `yaml:"log_level"`
}

type MountConfig struct {
    Source   string `yaml:"source"`
    Target   string `yaml:"target"`
    ReadOnly bool   `yaml:"readonly"`
}

// Functions to implement:
// - LoadServerConfig() (*ServerConfig, error)
// - (c *ServerConfig) Validate() error
// - expandPath(path string) string
// - loadEnvFile(path string) (map[string]string, error)
```

### 2.4 Create Client Configuration

**File**: `internal/config/client.go`

Key functionality:
- Load/save `~/.shed/config.yaml`
- Manage servers (add, remove, list, get)
- Set default server
- Cache shed locations
- Manage known_hosts file

```go
package config

import "time"

// ClientConfig represents CLI-side configuration
type ClientConfig struct {
    Servers       map[string]ServerEntry `yaml:"servers"`
    DefaultServer string                 `yaml:"default_server"`
    Sheds         map[string]ShedCache   `yaml:"sheds"`
}

type ServerEntry struct {
    Host     string    `yaml:"host"`
    HTTPPort int       `yaml:"http_port"`
    SSHPort  int       `yaml:"ssh_port"`
    AddedAt  time.Time `yaml:"added_at"`
}

type ShedCache struct {
    Server    string    `yaml:"server"`
    Status    string    `yaml:"status"`
    UpdatedAt time.Time `yaml:"updated_at"`
}

// Functions to implement:
// - LoadClientConfig() (*ClientConfig, error)
// - (c *ClientConfig) Save() error
// - (c *ClientConfig) AddServer(name string, entry ServerEntry) error
// - (c *ClientConfig) RemoveServer(name string) error
// - (c *ClientConfig) GetServer(name string) (*ServerEntry, error)
// - (c *ClientConfig) SetDefaultServer(name string) error
// - (c *ClientConfig) CacheShed(name string, cache ShedCache)
// - (c *ClientConfig) GetShedServer(name string) (string, error)
// - AddKnownHost(host string, port int, hostKey string) error
// - GetClientConfigPath() string
// - GetKnownHostsPath() string
```

### 2.5 Create Example Configs

**File**: `configs/server.example.yaml`
```yaml
# Shed Server Configuration

# Server identity
name: my-server

# Network configuration
http_port: 8080
ssh_port: 2222

# Docker settings
default_image: shed-base:latest

# Credentials to mount into containers
credentials:
  git_ssh:
    source: ~/.ssh
    target: /root/.ssh
    readonly: true
  git_config:
    source: ~/.gitconfig
    target: /root/.gitconfig
    readonly: true
  claude:
    source: ~/.claude
    target: /root/.claude
    readonly: false
  gh:
    source: ~/.config/gh
    target: /root/.config/gh
    readonly: true

# Environment file path
env_file: ~/.shed/env

# Logging level: debug, info, warn, error
log_level: info
```

**File**: `configs/server.dev.yaml`
```yaml
name: localhost-dev
http_port: 8080
ssh_port: 2222
default_image: shed-base:latest
credentials:
  git_ssh:
    source: ~/.ssh
    target: /root/.ssh
    readonly: true
  git_config:
    source: ~/.gitconfig
    target: /root/.gitconfig
    readonly: true
env_file: ""
log_level: debug
```

### 2.6 Write Unit Tests

**File**: `internal/config/server_test.go`
- Test config loading from file
- Test path expansion
- Test default values
- Test validation

**File**: `internal/config/client_test.go`
- Test config load/save cycle
- Test server add/remove
- Test shed cache operations

---

## Deliverables Checklist

- [ ] `internal/config/types.go` with all types
- [ ] `internal/config/server.go` with full implementation
- [ ] `internal/config/client.go` with full implementation
- [ ] `configs/server.example.yaml`
- [ ] `configs/server.dev.yaml`
- [ ] Unit tests passing

---

## Deviations & Notes

| Item | Description |
|------|-------------|
| | |

---

## Completion Criteria
- All deliverables checked off
- `go test ./internal/config/...` passes
- Update epic progress tracker to mark Phase 2 complete
