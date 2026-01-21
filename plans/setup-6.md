# Phase 6: Server Binary

## Overview
- **Phase**: 6 of 9
- **Epic**: [setup-epic.md](./setup-epic.md)
- **Status**: NOT STARTED
- **Estimated Effort**: Medium
- **Dependencies**: Phase 4 complete (HTTP API), Phase 5 complete (SSH Server)

## Objective
Wire up the HTTP and SSH servers into the `shed-server` binary with systemd support. This phase creates the main server entrypoint that initializes all components, manages their lifecycle, and supports installation as a systemd service.

## Prerequisites
- Phase 4 complete (`internal/api` package exists)
- Phase 5 complete (`internal/sshd` package exists)
- cobra dependency added (should already be present from Phase 7 planning)

## Context for New Engineers

### Server Binary Purpose
The `shed-server` binary is the daemon that runs on each server machine. It:
1. Loads configuration from multiple possible locations
2. Initializes the Docker client
3. Starts the SSH server (generates host key if needed)
4. Starts the HTTP API server
5. Handles graceful shutdown on signals

### Subcommands
| Command | Description |
|---------|-------------|
| `serve` | Run the server in the foreground (default) |
| `install` | Install as a systemd service |
| `uninstall` | Remove the systemd service |

### Configuration Loading Order
The server checks these locations in order (first found wins):
1. `./server.yaml` (current directory)
2. `~/.config/shed/server.yaml` (user config)
3. `/etc/shed/server.yaml` (system config)

### Signal Handling
- `SIGINT` (Ctrl+C): Graceful shutdown
- `SIGTERM`: Graceful shutdown (systemd sends this)

Graceful shutdown means:
1. Stop accepting new SSH connections
2. Stop accepting new HTTP requests
3. Wait for in-flight requests to complete (with timeout)
4. Close Docker client
5. Exit cleanly

### Host Key Management
On first start, if no SSH host key exists at `/etc/shed/host_key`, the server generates a new ED25519 key and saves it. This key is used for all SSH connections and served via the `/api/ssh-host-key` endpoint.

---

## Progress Tracker

| Task | Status | Notes |
|------|--------|-------|
| 6.1 Add cobra dependency | NOT STARTED | |
| 6.2 Create main.go with cobra commands | NOT STARTED | |
| 6.3 Implement serve command | NOT STARTED | |
| 6.4 Implement install command | NOT STARTED | |
| 6.5 Implement uninstall command | NOT STARTED | |
| 6.6 Test server startup and shutdown | NOT STARTED | |

---

## Detailed Tasks

### 6.1 Add Cobra Dependency

```bash
go get github.com/spf13/cobra
```

### 6.2 Create Main Entry Point

**File**: `cmd/shed-server/main.go`

```go
package main

import (
    "fmt"
    "os"

    "github.com/spf13/cobra"
    "github.com/charliek/shed/internal/version"
)

var (
    configPath string
)

func main() {
    rootCmd := &cobra.Command{
        Use:     "shed-server",
        Short:   "Shed development environment server",
        Version: version.Version,
    }

    // Global flags
    rootCmd.PersistentFlags().StringVarP(&configPath, "config", "c", "", "config file path (default: auto-detect)")

    // Add subcommands
    rootCmd.AddCommand(serveCmd())
    rootCmd.AddCommand(installCmd())
    rootCmd.AddCommand(uninstallCmd())

    // Default to serve if no subcommand specified
    rootCmd.Run = serveCmd().Run

    if err := rootCmd.Execute(); err != nil {
        fmt.Fprintln(os.Stderr, err)
        os.Exit(1)
    }
}
```

### 6.3 Implement Serve Command

**File**: `cmd/shed-server/serve.go`

```go
package main

import (
    "context"
    "fmt"
    "log"
    "net/http"
    "os"
    "os/signal"
    "syscall"
    "time"

    "github.com/spf13/cobra"
    "github.com/charliek/shed/internal/api"
    "github.com/charliek/shed/internal/config"
    "github.com/charliek/shed/internal/docker"
    "github.com/charliek/shed/internal/sshd"
)

func serveCmd() *cobra.Command {
    return &cobra.Command{
        Use:   "serve",
        Short: "Run the server in the foreground",
        Long:  "Start the shed-server, running both HTTP API and SSH servers.",
        RunE:  runServe,
    }
}

func runServe(cmd *cobra.Command, args []string) error {
    // 1. Load configuration
    cfg, err := loadConfig()
    if err != nil {
        return fmt.Errorf("failed to load config: %w", err)
    }
    log.Printf("Loaded configuration for server: %s", cfg.Name)

    // 2. Initialize Docker client
    dockerClient, err := docker.NewClient()
    if err != nil {
        return fmt.Errorf("failed to connect to Docker: %w", err)
    }
    defer dockerClient.Close()
    log.Println("Connected to Docker")

    // 3. Initialize SSH server (generates host key if needed)
    hostKeyPath := "/etc/shed/host_key"
    sshServer, err := sshd.NewServer(dockerClient, hostKeyPath, cfg.SSHPort)
    if err != nil {
        return fmt.Errorf("failed to initialize SSH server: %w", err)
    }
    log.Printf("SSH server initialized on port %d", cfg.SSHPort)

    // 4. Initialize HTTP API server
    sshHostKey := sshServer.GetHostPublicKey()
    apiServer := api.NewServer(dockerClient, cfg, sshHostKey)
    httpServer := &http.Server{
        Addr:    fmt.Sprintf(":%d", cfg.HTTPPort),
        Handler: apiServer.Router(),
    }
    log.Printf("HTTP server initialized on port %d", cfg.HTTPPort)

    // 5. Start servers in goroutines
    errChan := make(chan error, 2)

    // Start SSH server
    go func() {
        log.Printf("Starting SSH server on :%d", cfg.SSHPort)
        if err := sshServer.Start(); err != nil {
            errChan <- fmt.Errorf("SSH server error: %w", err)
        }
    }()

    // Start HTTP server
    go func() {
        log.Printf("Starting HTTP server on :%d", cfg.HTTPPort)
        if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
            errChan <- fmt.Errorf("HTTP server error: %w", err)
        }
    }()

    // 6. Wait for shutdown signal
    quit := make(chan os.Signal, 1)
    signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)

    select {
    case <-quit:
        log.Println("Shutdown signal received, initiating graceful shutdown...")
    case err := <-errChan:
        log.Printf("Server error: %v", err)
        log.Println("Initiating shutdown...")
    }

    // 7. Graceful shutdown with timeout
    ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
    defer cancel()

    // Shutdown HTTP server
    log.Println("Shutting down HTTP server...")
    if err := httpServer.Shutdown(ctx); err != nil {
        log.Printf("HTTP server shutdown error: %v", err)
    }

    // Shutdown SSH server
    log.Println("Shutting down SSH server...")
    if err := sshServer.Shutdown(ctx); err != nil {
        log.Printf("SSH server shutdown error: %v", err)
    }

    log.Println("Server stopped")
    return nil
}

// loadConfig loads server configuration from standard locations
func loadConfig() (*config.ServerConfig, error) {
    // If explicit config path provided, use it
    if configPath != "" {
        return config.LoadServerConfig(configPath)
    }

    // Check standard locations in order
    searchPaths := []string{
        "./server.yaml",
        expandPath("~/.config/shed/server.yaml"),
        "/etc/shed/server.yaml",
    }

    for _, path := range searchPaths {
        if _, err := os.Stat(path); err == nil {
            log.Printf("Using config file: %s", path)
            return config.LoadServerConfig(path)
        }
    }

    return nil, fmt.Errorf("no configuration file found; checked: %v", searchPaths)
}

// expandPath expands ~ to the user's home directory
func expandPath(path string) string {
    if len(path) > 1 && path[:2] == "~/" {
        home, err := os.UserHomeDir()
        if err != nil {
            return path
        }
        return home + path[1:]
    }
    return path
}
```

### 6.4 Implement Install Command

**File**: `cmd/shed-server/install.go`

```go
package main

import (
    "fmt"
    "os"
    "os/exec"
    "os/user"
    "path/filepath"
    "text/template"

    "github.com/spf13/cobra"
)

const systemdUnitPath = "/etc/systemd/system/shed-server.service"

const systemdUnitTemplate = `[Unit]
Description=Shed Development Environment Server
After=network.target docker.service
Requires=docker.service

[Service]
Type=simple
User={{.User}}
Group={{.Group}}
ExecStart={{.ExecPath}} serve
Restart=on-failure
RestartSec=5

# Environment
Environment=HOME={{.Home}}

[Install]
WantedBy=multi-user.target
`

type unitData struct {
    User     string
    Group    string
    ExecPath string
    Home     string
}

func installCmd() *cobra.Command {
    return &cobra.Command{
        Use:   "install",
        Short: "Install shed-server as a systemd service",
        Long: `Install shed-server as a systemd service.

This command:
1. Creates a systemd unit file at /etc/systemd/system/shed-server.service
2. Reloads the systemd daemon
3. Enables the service to start on boot
4. Starts the service

Requires root privileges (use sudo).`,
        RunE: runInstall,
    }
}

func runInstall(cmd *cobra.Command, args []string) error {
    // Check for root
    if os.Geteuid() != 0 {
        return fmt.Errorf("this command requires root privileges; use: sudo shed-server install")
    }

    // Get current user info (the user who invoked sudo)
    sudoUser := os.Getenv("SUDO_USER")
    if sudoUser == "" {
        return fmt.Errorf("SUDO_USER not set; run with sudo")
    }

    u, err := user.Lookup(sudoUser)
    if err != nil {
        return fmt.Errorf("failed to lookup user %s: %w", sudoUser, err)
    }

    // Get the path to the current executable
    execPath, err := os.Executable()
    if err != nil {
        return fmt.Errorf("failed to get executable path: %w", err)
    }
    execPath, err = filepath.Abs(execPath)
    if err != nil {
        return fmt.Errorf("failed to resolve executable path: %w", err)
    }

    // Ensure executable is in a suitable location
    // Recommend /usr/local/bin for production
    if execPath != "/usr/local/bin/shed-server" {
        fmt.Printf("Warning: executable is at %s\n", execPath)
        fmt.Println("Consider copying to /usr/local/bin/shed-server for production use.")
    }

    // Generate unit file
    data := unitData{
        User:     u.Username,
        Group:    u.Username, // Primary group same as username
        ExecPath: execPath,
        Home:     u.HomeDir,
    }

    tmpl, err := template.New("unit").Parse(systemdUnitTemplate)
    if err != nil {
        return fmt.Errorf("failed to parse unit template: %w", err)
    }

    f, err := os.Create(systemdUnitPath)
    if err != nil {
        return fmt.Errorf("failed to create unit file: %w", err)
    }
    defer f.Close()

    if err := tmpl.Execute(f, data); err != nil {
        return fmt.Errorf("failed to write unit file: %w", err)
    }

    fmt.Printf("Created systemd unit file: %s\n", systemdUnitPath)

    // Reload systemd daemon
    fmt.Println("Reloading systemd daemon...")
    if err := runCommand("systemctl", "daemon-reload"); err != nil {
        return fmt.Errorf("failed to reload systemd: %w", err)
    }

    // Enable the service
    fmt.Println("Enabling shed-server service...")
    if err := runCommand("systemctl", "enable", "shed-server"); err != nil {
        return fmt.Errorf("failed to enable service: %w", err)
    }

    // Start the service
    fmt.Println("Starting shed-server service...")
    if err := runCommand("systemctl", "start", "shed-server"); err != nil {
        return fmt.Errorf("failed to start service: %w", err)
    }

    fmt.Println("")
    fmt.Println("shed-server installed and started successfully!")
    fmt.Println("")
    fmt.Println("Useful commands:")
    fmt.Println("  systemctl status shed-server   # Check status")
    fmt.Println("  journalctl -u shed-server -f   # View logs")
    fmt.Println("  systemctl restart shed-server  # Restart service")

    return nil
}

func runCommand(name string, args ...string) error {
    cmd := exec.Command(name, args...)
    cmd.Stdout = os.Stdout
    cmd.Stderr = os.Stderr
    return cmd.Run()
}
```

### 6.5 Implement Uninstall Command

**File**: `cmd/shed-server/uninstall.go`

```go
package main

import (
    "fmt"
    "os"

    "github.com/spf13/cobra"
)

func uninstallCmd() *cobra.Command {
    var keepConfig bool

    cmd := &cobra.Command{
        Use:   "uninstall",
        Short: "Remove shed-server systemd service",
        Long: `Remove the shed-server systemd service.

This command:
1. Stops the running service
2. Disables the service from starting on boot
3. Removes the systemd unit file
4. Reloads the systemd daemon

Requires root privileges (use sudo).

Note: This does NOT remove:
- Configuration files (/etc/shed/, ~/.config/shed/)
- SSH host keys
- Docker containers/volumes managed by shed
- The shed-server binary itself`,
        RunE: func(cmd *cobra.Command, args []string) error {
            return runUninstall(keepConfig)
        },
    }

    cmd.Flags().BoolVar(&keepConfig, "keep-config", true, "keep configuration files (default: true)")

    return cmd
}

func runUninstall(keepConfig bool) error {
    // Check for root
    if os.Geteuid() != 0 {
        return fmt.Errorf("this command requires root privileges; use: sudo shed-server uninstall")
    }

    // Check if service exists
    if _, err := os.Stat(systemdUnitPath); os.IsNotExist(err) {
        fmt.Println("shed-server service is not installed.")
        return nil
    }

    // Stop the service (ignore errors - might not be running)
    fmt.Println("Stopping shed-server service...")
    _ = runCommand("systemctl", "stop", "shed-server")

    // Disable the service
    fmt.Println("Disabling shed-server service...")
    if err := runCommand("systemctl", "disable", "shed-server"); err != nil {
        fmt.Printf("Warning: failed to disable service: %v\n", err)
    }

    // Remove the unit file
    fmt.Println("Removing systemd unit file...")
    if err := os.Remove(systemdUnitPath); err != nil {
        return fmt.Errorf("failed to remove unit file: %w", err)
    }

    // Reload systemd daemon
    fmt.Println("Reloading systemd daemon...")
    if err := runCommand("systemctl", "daemon-reload"); err != nil {
        return fmt.Errorf("failed to reload systemd: %w", err)
    }

    fmt.Println("")
    fmt.Println("shed-server service has been uninstalled.")
    fmt.Println("")
    fmt.Println("Note: The following were NOT removed:")
    fmt.Println("  - Configuration files in /etc/shed/ and ~/.config/shed/")
    fmt.Println("  - SSH host keys in /etc/shed/host_key")
    fmt.Println("  - Docker containers and volumes created by shed")
    fmt.Println("  - The shed-server binary")

    return nil
}
```

### 6.6 Test Server Startup and Shutdown

**Manual Testing Steps:**

1. **Basic startup test:**
   ```bash
   # Create a test config
   cat > /tmp/test-server.yaml << 'EOF'
   name: test-server
   http_port: 18080
   ssh_port: 12222
   default_image: ubuntu:24.04
   credentials: {}
   log_level: debug
   EOF

   # Run the server
   go run ./cmd/shed-server serve --config /tmp/test-server.yaml
   ```

2. **Verify HTTP endpoint:**
   ```bash
   curl http://localhost:18080/api/info
   # Should return: {"name":"test-server","version":"...","ssh_port":12222,"http_port":18080}
   ```

3. **Test graceful shutdown:**
   - Press Ctrl+C while server is running
   - Verify clean shutdown messages appear
   - Verify exit code is 0

4. **Test systemd install (requires root):**
   ```bash
   # Build the binary
   go build -o bin/shed-server ./cmd/shed-server

   # Copy to standard location
   sudo cp bin/shed-server /usr/local/bin/

   # Create config
   sudo mkdir -p /etc/shed
   sudo cp /tmp/test-server.yaml /etc/shed/server.yaml

   # Install service
   sudo /usr/local/bin/shed-server install

   # Verify it's running
   systemctl status shed-server
   curl http://localhost:18080/api/info

   # View logs
   journalctl -u shed-server -f

   # Uninstall
   sudo shed-server uninstall
   ```

**Automated Test File**: `cmd/shed-server/main_test.go`

```go
package main

import (
    "os"
    "testing"
)

func TestExpandPath(t *testing.T) {
    home, _ := os.UserHomeDir()

    tests := []struct {
        input    string
        expected string
    }{
        {"~/test", home + "/test"},
        {"~/.config/shed", home + "/.config/shed"},
        {"/etc/shed/server.yaml", "/etc/shed/server.yaml"},
        {"./server.yaml", "./server.yaml"},
    }

    for _, tt := range tests {
        result := expandPath(tt.input)
        if result != tt.expected {
            t.Errorf("expandPath(%q) = %q, want %q", tt.input, result, tt.expected)
        }
    }
}
```

---

## Deliverables Checklist

- [ ] cobra dependency added to go.mod
- [ ] `cmd/shed-server/main.go` implements root command with version flag
- [ ] `cmd/shed-server/serve.go` implements serve subcommand
- [ ] `cmd/shed-server/install.go` implements install subcommand
- [ ] `cmd/shed-server/uninstall.go` implements uninstall subcommand
- [ ] Configuration loading from multiple locations works
- [ ] Both HTTP and SSH servers start concurrently
- [ ] Graceful shutdown on SIGINT/SIGTERM works
- [ ] Systemd unit file correctly references user and executable path
- [ ] Install command creates, enables, and starts the service
- [ ] Uninstall command stops, disables, and removes the service

---

## Deviations & Notes

| Item | Description |
|------|-------------|
| | |

---

## Completion Criteria
- All deliverables checked off
- `go build ./cmd/shed-server` succeeds
- Server starts and responds to `/api/info`
- Server handles Ctrl+C gracefully
- Systemd install/uninstall works on a Linux system
- Update epic progress tracker to mark Phase 6 complete
