# Phase 5: SSH Server

## Overview
- **Phase**: 5 of 9
- **Epic**: [setup-epic.md](./setup-epic.md)
- **Status**: NOT STARTED
- **Estimated Effort**: Large
- **Dependencies**: Phase 3 complete

## Objective
Implement an SSH server using gliderlabs/ssh that routes connections to the appropriate shed containers. This enables `shed console` and IDE remote connections.

## Prerequisites
- Phase 3 complete (Docker client wrapper exists)
- gliderlabs/ssh dependency added

## Context for New Engineers

### Connection Flow
```
ssh codelens@server:2222
     └─ username determines container (shed-codelens)
```

1. User connects via SSH with username = shed name
2. Server parses username to get container name
3. If container is stopped, auto-start it
4. Create docker exec session
5. Forward I/O bidirectionally

### PTY Handling
- Terminal type passed via `TERM` environment variable
- Window resize events must be forwarded to the container
- Raw mode preserved for full terminal compatibility

### Authentication (MVP)
- Accept all SSH keys (Tailscale network is trust boundary)
- Log connecting key fingerprints for audit

---

## Progress Tracker

| Task | Status | Notes |
|------|--------|-------|
| 5.1 Add gliderlabs/ssh dependency | NOT STARTED | |
| 5.2 Create server.go | NOT STARTED | |
| 5.3 Create session.go | NOT STARTED | |
| 5.4 Implement PTY handling | NOT STARTED | |
| 5.5 Test SSH connections | NOT STARTED | |

---

## Detailed Tasks

### 5.1 Add SSH Dependency

```bash
go get github.com/gliderlabs/ssh
go get golang.org/x/crypto/ssh
```

### 5.2 Create SSH Server

**File**: `internal/sshd/server.go`

```go
package sshd

import (
    "context"
    "crypto/ed25519"
    "crypto/rand"
    "os"

    "github.com/gliderlabs/ssh"
    gossh "golang.org/x/crypto/ssh"
    "github.com/charliek/shed/internal/docker"
)

// Server wraps the SSH server
type Server struct {
    sshServer    *ssh.Server
    dockerClient *docker.Client
    hostKeyPath  string
    port         int
}

// NewServer creates a new SSH server
func NewServer(dockerClient *docker.Client, hostKeyPath string, port int) (*Server, error) {
    s := &Server{
        dockerClient: dockerClient,
        hostKeyPath:  hostKeyPath,
        port:         port,
    }

    // Load or generate host key
    hostKey, err := s.loadOrGenerateHostKey()
    if err != nil {
        return nil, err
    }

    s.sshServer = &ssh.Server{
        Addr:             fmt.Sprintf(":%d", port),
        Handler:          s.handleSession,
        PublicKeyHandler: s.handlePublicKey,
    }
    s.sshServer.AddHostKey(hostKey)

    return s, nil
}

// loadOrGenerateHostKey loads existing key or generates new ED25519 key
func (s *Server) loadOrGenerateHostKey() (ssh.Signer, error) {
    // If file exists, load it
    // Otherwise, generate new ED25519 key and save it
}

// handlePublicKey handles SSH authentication
// For MVP: accept all keys, log fingerprint
func (s *Server) handlePublicKey(ctx ssh.Context, key ssh.PublicKey) bool {
    fingerprint := gossh.FingerprintSHA256(key)
    log.Printf("SSH connection from %s with key %s", ctx.RemoteAddr(), fingerprint)
    return true  // Accept all keys
}

// GetHostPublicKey returns the public key for /api/ssh-host-key
func (s *Server) GetHostPublicKey() string

// Start begins listening for SSH connections
func (s *Server) Start() error {
    return s.sshServer.ListenAndServe()
}

// Shutdown gracefully stops the server
func (s *Server) Shutdown(ctx context.Context) error {
    return s.sshServer.Shutdown(ctx)
}
```

### 5.3 Create Session Handler

**File**: `internal/sshd/session.go`

```go
package sshd

import (
    "context"
    "fmt"
    "io"

    "github.com/gliderlabs/ssh"
    "github.com/charliek/shed/internal/config"
)

// handleSession handles an individual SSH session
func (s *Server) handleSession(session ssh.Session) {
    // 1. Extract shed name from username
    shedName := session.User()

    // 2. Handle special usernames
    if shedName == "_api" {
        // Reserved for future use
        fmt.Fprintln(session, "API access not implemented")
        session.Exit(1)
        return
    }

    // 3. Check if shed exists
    ctx := context.Background()
    shed, err := s.dockerClient.GetShed(ctx, shedName)
    if err != nil {
        fmt.Fprintf(session.Stderr(), "Shed '%s' not found\n", shedName)
        session.Exit(1)
        return
    }

    // 4. Auto-start if stopped
    if shed.Status == config.StatusStopped {
        if _, err := s.dockerClient.StartShed(ctx, shedName); err != nil {
            fmt.Fprintf(session.Stderr(), "Failed to start shed: %v\n", err)
            session.Exit(1)
            return
        }
        // Wait for container to be ready (up to 10 seconds)
        if err := s.waitForReady(ctx, shedName); err != nil {
            fmt.Fprintf(session.Stderr(), "Shed failed to start: %v\n", err)
            session.Exit(1)
            return
        }
    }

    // 5. Determine session type
    cmd := session.Command()  // Empty for interactive shell
    ptyReq, winCh, isPty := session.Pty()

    // 6. Execute in container
    exitCode := s.execInContainer(ctx, session, shedName, cmd, isPty, ptyReq, winCh)
    session.Exit(exitCode)
}

// execInContainer runs a command or shell in the container
func (s *Server) execInContainer(
    ctx context.Context,
    session ssh.Session,
    shedName string,
    cmd []string,
    isPty bool,
    ptyReq ssh.Pty,
    winCh <-chan ssh.Window,
) int {
    // Build exec command
    execCmd := cmd
    if len(execCmd) == 0 {
        execCmd = []string{"/bin/bash"}
    }

    // Create exec session with PTY if requested
    execSession, err := s.dockerClient.AttachToShed(ctx, shedName, isPty)
    if err != nil {
        fmt.Fprintf(session.Stderr(), "Failed to attach: %v\n", err)
        return 1
    }
    defer execSession.Close()

    // Set TERM if PTY requested
    if isPty {
        // Set terminal type and size
    }

    // Handle window resize events
    if winCh != nil {
        go s.handleWindowResize(ctx, execSession, winCh)
    }

    // Stream I/O bidirectionally
    return s.streamIO(session, execSession)
}

// handleWindowResize forwards window size changes to container
func (s *Server) handleWindowResize(ctx context.Context, exec *docker.ExecSession, winCh <-chan ssh.Window) {
    for win := range winCh {
        // Resize the exec TTY
    }
}

// streamIO handles bidirectional I/O between SSH session and container
func (s *Server) streamIO(session ssh.Session, exec *docker.ExecSession) int {
    // Use goroutines to copy in both directions
    // Wait for completion, return exit code
}

// waitForReady waits for container to be ready
func (s *Server) waitForReady(ctx context.Context, name string) error {
    // Poll container status with timeout
}
```

### 5.4 Implement PTY Handling

Key PTY requirements:
- Extract terminal type from `session.Pty().Term`
- Set `TERM` env var in docker exec
- Set initial window size
- Forward window change events via `ResizeExecTTY`

### 5.5 Test SSH Connections

Manual testing steps:
1. Start server with SSH enabled
2. Create a test shed
3. Connect: `ssh -p 2222 test-shed@localhost`
4. Verify shell works
5. Test window resize (make terminal bigger/smaller)
6. Test command execution: `ssh -p 2222 test-shed@localhost "ls -la"`

---

## Deliverables Checklist

- [ ] gliderlabs/ssh dependency added
- [ ] `internal/sshd/server.go` implemented
- [ ] `internal/sshd/session.go` implemented
- [ ] Host key generation works
- [ ] Username → container routing works
- [ ] Auto-start stopped containers works
- [ ] PTY sessions work (interactive shell)
- [ ] Non-PTY sessions work (command execution)
- [ ] Window resize forwarding works

---

## Deviations & Notes

| Item | Description |
|------|-------------|
| | |

---

## Completion Criteria
- All deliverables checked off
- Can SSH into a shed and get a working shell
- Update epic progress tracker to mark Phase 5 complete
