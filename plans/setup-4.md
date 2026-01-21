# Phase 4: HTTP API Server

## Overview
- **Phase**: 4 of 9
- **Epic**: [setup-epic.md](./setup-epic.md)
- **Status**: NOT STARTED
- **Estimated Effort**: Medium
- **Dependencies**: Phase 3 complete

## Objective
Implement the REST API server using the chi router. This provides the HTTP interface that the CLI uses to communicate with the server for all CRUD operations on sheds.

## Prerequisites
- Phase 3 complete (Docker client wrapper exists)
- chi router dependency added

## Context for New Engineers

### API Base URL
`http://{host}:8080/api`

### Endpoints
| Method | Path | Description |
|--------|------|-------------|
| GET | /api/info | Server metadata |
| GET | /api/ssh-host-key | SSH host public key |
| GET | /api/sheds | List all sheds |
| POST | /api/sheds | Create new shed |
| GET | /api/sheds/{name} | Get shed details |
| DELETE | /api/sheds/{name} | Delete shed |
| POST | /api/sheds/{name}/start | Start shed |
| POST | /api/sheds/{name}/stop | Stop shed |

### Error Response Format
```json
{
  "error": {
    "code": "SHED_NOT_FOUND",
    "message": "Shed 'codelens' not found"
  }
}
```

---

## Progress Tracker

| Task | Status | Notes |
|------|--------|-------|
| 4.1 Add chi dependency | NOT STARTED | |
| 4.2 Create routes.go | NOT STARTED | |
| 4.3 Create handlers.go | NOT STARTED | |
| 4.4 Create middleware.go | NOT STARTED | |
| 4.5 Write handler tests | NOT STARTED | |

---

## Detailed Tasks

### 4.1 Add Chi Dependency

```bash
go get github.com/go-chi/chi/v5
go get github.com/go-chi/chi/v5/middleware
```

### 4.2 Create Routes

**File**: `internal/api/routes.go`

```go
package api

import (
    "github.com/go-chi/chi/v5"
    "github.com/go-chi/chi/v5/middleware"
    "github.com/charliek/shed/internal/config"
    "github.com/charliek/shed/internal/docker"
)

// Server holds API dependencies
type Server struct {
    docker       *docker.Client
    config       *config.ServerConfig
    sshHostKey   string  // Public key for /api/ssh-host-key
}

// NewServer creates an API server
func NewServer(dockerClient *docker.Client, cfg *config.ServerConfig, sshHostKey string) *Server

// Router returns the configured chi router
func (s *Server) Router() chi.Router {
    r := chi.NewRouter()

    // Middleware
    r.Use(middleware.RequestID)
    r.Use(middleware.RealIP)
    r.Use(middleware.Logger)
    r.Use(middleware.Recoverer)
    r.Use(middleware.Timeout(60 * time.Second))

    // Routes
    r.Route("/api", func(r chi.Router) {
        r.Get("/info", s.handleGetInfo)
        r.Get("/ssh-host-key", s.handleGetSSHHostKey)

        r.Route("/sheds", func(r chi.Router) {
            r.Get("/", s.handleListSheds)
            r.Post("/", s.handleCreateShed)

            r.Route("/{name}", func(r chi.Router) {
                r.Get("/", s.handleGetShed)
                r.Delete("/", s.handleDeleteShed)
                r.Post("/start", s.handleStartShed)
                r.Post("/stop", s.handleStopShed)
            })
        })
    })

    return r
}
```

### 4.3 Create Handlers

**File**: `internal/api/handlers.go`

```go
package api

import (
    "encoding/json"
    "net/http"
    "github.com/go-chi/chi/v5"
    "github.com/charliek/shed/internal/config"
    "github.com/charliek/shed/internal/version"
)

// handleGetInfo - GET /api/info
func (s *Server) handleGetInfo(w http.ResponseWriter, r *http.Request) {
    // Return ServerInfo{Name, Version, SSHPort, HTTPPort}
}

// handleGetSSHHostKey - GET /api/ssh-host-key
func (s *Server) handleGetSSHHostKey(w http.ResponseWriter, r *http.Request) {
    // Return SSHHostKeyResponse{HostKey}
}

// handleListSheds - GET /api/sheds
func (s *Server) handleListSheds(w http.ResponseWriter, r *http.Request) {
    // Call docker.ListSheds()
    // Return ShedsResponse
}

// handleCreateShed - POST /api/sheds
func (s *Server) handleCreateShed(w http.ResponseWriter, r *http.Request) {
    // Parse CreateShedRequest from body
    // Validate request
    // Call docker.CreateShed()
    // Return 201 with Shed or error
}

// handleGetShed - GET /api/sheds/{name}
func (s *Server) handleGetShed(w http.ResponseWriter, r *http.Request) {
    name := chi.URLParam(r, "name")
    // Call docker.GetShed(name)
    // Return Shed or 404
}

// handleDeleteShed - DELETE /api/sheds/{name}
func (s *Server) handleDeleteShed(w http.ResponseWriter, r *http.Request) {
    name := chi.URLParam(r, "name")
    keepVolume := r.URL.Query().Get("keep_volume") == "true"
    // Call docker.DeleteShed(name, keepVolume)
    // Return 204 or error
}

// handleStartShed - POST /api/sheds/{name}/start
func (s *Server) handleStartShed(w http.ResponseWriter, r *http.Request) {
    name := chi.URLParam(r, "name")
    // Call docker.StartShed(name)
    // Return Shed or error
}

// handleStopShed - POST /api/sheds/{name}/stop
func (s *Server) handleStopShed(w http.ResponseWriter, r *http.Request) {
    name := chi.URLParam(r, "name")
    // Call docker.StopShed(name)
    // Return Shed or error
}

// Helper functions
func writeJSON(w http.ResponseWriter, status int, data interface{})
func writeError(w http.ResponseWriter, status int, code string, message string)
```

### 4.4 Create Middleware

**File**: `internal/api/middleware.go`

```go
package api

import (
    "context"
    "net/http"
)

// Use chi's built-in middleware for most things
// Add custom middleware as needed:

// ContentTypeJSON sets Content-Type to application/json
func ContentTypeJSON(next http.Handler) http.Handler {
    return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        w.Header().Set("Content-Type", "application/json")
        next.ServeHTTP(w, r)
    })
}
```

### 4.5 Write Handler Tests

**File**: `internal/api/handlers_test.go`

```go
package api

import (
    "net/http"
    "net/http/httptest"
    "testing"
)

func TestHandleGetInfo(t *testing.T) {
    // Create server with mock dependencies
    // Make request to /api/info
    // Verify response
}

func TestHandleListSheds(t *testing.T)
func TestHandleCreateShed(t *testing.T)
func TestHandleCreateShedAlreadyExists(t *testing.T)
func TestHandleGetShedNotFound(t *testing.T)
func TestHandleDeleteShed(t *testing.T)
```

---

## Deliverables Checklist

- [ ] chi dependency added to go.mod
- [ ] `internal/api/routes.go` implemented
- [ ] `internal/api/handlers.go` implemented
- [ ] `internal/api/middleware.go` implemented
- [ ] All endpoints return correct status codes
- [ ] Error responses follow JSON format
- [ ] Handler tests passing

---

## Deviations & Notes

| Item | Description |
|------|-------------|
| | |

---

## Completion Criteria
- All deliverables checked off
- `go test ./internal/api/...` passes
- Can manually test with curl if server is running
- Update epic progress tracker to mark Phase 4 complete
