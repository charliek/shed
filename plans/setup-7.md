# Phase 7: CLI Commands

## Overview
- **Phase**: 7 of 9
- **Epic**: [setup-epic.md](./setup-epic.md)
- **Status**: NOT STARTED
- **Estimated Effort**: Large
- **Dependencies**: Phase 2 complete (Core Types & Configuration)

## Objective
Implement the `shed` CLI binary with all commands for server management, shed management, and interactive operations using the cobra framework.

## Prerequisites
- Phase 2 complete (config package with ClientConfig, ServerEntry, ShedCache types)
- `github.com/spf13/cobra` dependency added
- Server API available for testing (Phase 6 complete, or mock server)

## Context for New Engineers

### CLI Architecture
The CLI uses cobra for command organization. All commands share:
- Global flags for server selection, verbosity, and config path
- A shared HTTP client for API calls
- Client configuration loaded from `~/.shed/config.yaml`

### Command Categories

1. **Server Management** - Configure which shed-servers the CLI knows about
2. **Shed Management** - CRUD operations on development environments
3. **Interactive Commands** - Connect to sheds via SSH

### Client Configuration Location
- **Config file**: `~/.shed/config.yaml`
- **Known hosts**: `~/.shed/known_hosts`

### SSH Integration
The `console` and `exec` commands use the system's SSH client via `os/exec`. This ensures:
- SSH agent forwarding works automatically
- User's SSH configuration is respected
- Familiar SSH behavior

---

## Progress Tracker

| Task | Status | Notes |
|------|--------|-------|
| 7.1 Add cobra dependency | NOT STARTED | |
| 7.2 Create root command and global flags | NOT STARTED | |
| 7.3 Create HTTP client wrapper | NOT STARTED | |
| 7.4 Implement server commands | NOT STARTED | |
| 7.5 Implement shed management commands | NOT STARTED | |
| 7.6 Implement interactive commands | NOT STARTED | |
| 7.7 Wire up main.go | NOT STARTED | |
| 7.8 Test CLI commands | NOT STARTED | |

---

## Detailed Tasks

### 7.1 Add Cobra Dependency

```bash
go get github.com/spf13/cobra
```

### 7.2 Create Root Command and Global Flags

**File**: `cmd/shed/root.go`

The root command sets up global flags and initializes shared resources.

```go
package main

import (
    "fmt"
    "os"

    "github.com/spf13/cobra"
    "github.com/charliek/shed/internal/config"
)

var (
    // Global flags
    serverFlag  string
    verboseFlag bool
    configFlag  string

    // Shared resources (initialized in PersistentPreRun)
    clientConfig *config.ClientConfig
)

var rootCmd = &cobra.Command{
    Use:   "shed",
    Short: "Manage containerized development environments",
    Long: `Shed enables developers to create, manage, and connect to
persistent containerized development environments across multiple servers.`,
    PersistentPreRunE: initializeClient,
    SilenceUsage:      true,
}

func init() {
    rootCmd.PersistentFlags().StringVarP(&serverFlag, "server", "s", "", "Target server (overrides default)")
    rootCmd.PersistentFlags().BoolVarP(&verboseFlag, "verbose", "v", false, "Enable debug output")
    rootCmd.PersistentFlags().StringVarP(&configFlag, "config", "c", "", "Config file path (default: ~/.shed/config.yaml)")
}

// initializeClient loads client configuration before any command runs
func initializeClient(cmd *cobra.Command, args []string) error {
    // Skip config loading for commands that don't need it
    if cmd.Name() == "version" || cmd.Name() == "help" {
        return nil
    }

    var err error
    if configFlag != "" {
        // Load from specified path
        clientConfig, err = config.LoadClientConfigFrom(configFlag)
    } else {
        // Load from default path
        clientConfig, err = config.LoadClientConfig()
    }

    // If config doesn't exist, create empty one (for first-time setup)
    if os.IsNotExist(err) {
        clientConfig = &config.ClientConfig{
            Servers: make(map[string]config.ServerEntry),
            Sheds:   make(map[string]config.ShedCache),
        }
        return nil
    }

    return err
}

// getTargetServer returns the server to use for operations
// Priority: --server flag > default server > error
func getTargetServer() (*config.ServerEntry, string, error) {
    var serverName string

    if serverFlag != "" {
        serverName = serverFlag
    } else if clientConfig.DefaultServer != "" {
        serverName = clientConfig.DefaultServer
    } else {
        return nil, "", fmt.Errorf("no server specified and no default server configured\n\nTry:\n  shed server add <host>")
    }

    server, err := clientConfig.GetServer(serverName)
    if err != nil {
        return nil, "", fmt.Errorf("server %q not found\n\nTry:\n  shed server list", serverName)
    }

    return server, serverName, nil
}

// verbose prints message only if --verbose flag is set
func verbose(format string, args ...interface{}) {
    if verboseFlag {
        fmt.Printf(format+"\n", args...)
    }
}

func Execute() {
    if err := rootCmd.Execute(); err != nil {
        os.Exit(1)
    }
}
```

### 7.3 Create HTTP Client Wrapper

**File**: `internal/client/http.go`

A thin wrapper around `net/http` for API calls.

```go
package client

import (
    "bytes"
    "encoding/json"
    "fmt"
    "io"
    "net/http"
    "time"

    "github.com/charliek/shed/internal/config"
)

// Client is an HTTP client for shed-server API
type Client struct {
    baseURL    string
    httpClient *http.Client
}

// NewClient creates a new API client for the given server
func NewClient(server *config.ServerEntry) *Client {
    return &Client{
        baseURL: fmt.Sprintf("http://%s:%d/api", server.Host, server.HTTPPort),
        httpClient: &http.Client{
            Timeout: 30 * time.Second,
        },
    }
}

// NewClientFromHostPort creates a client for discovery (before server is configured)
func NewClientFromHostPort(host string, httpPort int) *Client {
    return &Client{
        baseURL: fmt.Sprintf("http://%s:%d/api", host, httpPort),
        httpClient: &http.Client{
            Timeout: 10 * time.Second,
        },
    }
}

// GetInfo retrieves server information (GET /api/info)
func (c *Client) GetInfo() (*config.ServerInfo, error) {
    resp, err := c.get("/info")
    if err != nil {
        return nil, err
    }
    defer resp.Body.Close()

    if resp.StatusCode != http.StatusOK {
        return nil, c.parseError(resp)
    }

    var info config.ServerInfo
    if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
        return nil, fmt.Errorf("failed to parse response: %w", err)
    }

    return &info, nil
}

// GetSSHHostKey retrieves the server's SSH host key (GET /api/ssh-host-key)
func (c *Client) GetSSHHostKey() (string, error) {
    resp, err := c.get("/ssh-host-key")
    if err != nil {
        return "", err
    }
    defer resp.Body.Close()

    if resp.StatusCode != http.StatusOK {
        return "", c.parseError(resp)
    }

    var result config.SSHHostKeyResponse
    if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
        return "", fmt.Errorf("failed to parse response: %w", err)
    }

    return result.HostKey, nil
}

// ListSheds retrieves all sheds (GET /api/sheds)
func (c *Client) ListSheds() ([]config.Shed, error) {
    resp, err := c.get("/sheds")
    if err != nil {
        return nil, err
    }
    defer resp.Body.Close()

    if resp.StatusCode != http.StatusOK {
        return nil, c.parseError(resp)
    }

    var result config.ShedsResponse
    if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
        return nil, fmt.Errorf("failed to parse response: %w", err)
    }

    return result.Sheds, nil
}

// GetShed retrieves a specific shed (GET /api/sheds/{name})
func (c *Client) GetShed(name string) (*config.Shed, error) {
    resp, err := c.get("/sheds/" + name)
    if err != nil {
        return nil, err
    }
    defer resp.Body.Close()

    if resp.StatusCode == http.StatusNotFound {
        return nil, fmt.Errorf("shed %q not found", name)
    }
    if resp.StatusCode != http.StatusOK {
        return nil, c.parseError(resp)
    }

    var shed config.Shed
    if err := json.NewDecoder(resp.Body).Decode(&shed); err != nil {
        return nil, fmt.Errorf("failed to parse response: %w", err)
    }

    return &shed, nil
}

// CreateShed creates a new shed (POST /api/sheds)
func (c *Client) CreateShed(req *config.CreateShedRequest) (*config.Shed, error) {
    body, err := json.Marshal(req)
    if err != nil {
        return nil, fmt.Errorf("failed to marshal request: %w", err)
    }

    resp, err := c.post("/sheds", bytes.NewReader(body))
    if err != nil {
        return nil, err
    }
    defer resp.Body.Close()

    if resp.StatusCode == http.StatusConflict {
        return nil, fmt.Errorf("shed %q already exists", req.Name)
    }
    if resp.StatusCode != http.StatusCreated {
        return nil, c.parseError(resp)
    }

    var shed config.Shed
    if err := json.NewDecoder(resp.Body).Decode(&shed); err != nil {
        return nil, fmt.Errorf("failed to parse response: %w", err)
    }

    return &shed, nil
}

// DeleteShed deletes a shed (DELETE /api/sheds/{name})
func (c *Client) DeleteShed(name string, keepVolume bool) error {
    url := "/sheds/" + name
    if keepVolume {
        url += "?keep_volume=true"
    }

    resp, err := c.delete(url)
    if err != nil {
        return err
    }
    defer resp.Body.Close()

    if resp.StatusCode == http.StatusNotFound {
        return fmt.Errorf("shed %q not found", name)
    }
    if resp.StatusCode != http.StatusNoContent {
        return c.parseError(resp)
    }

    return nil
}

// StartShed starts a stopped shed (POST /api/sheds/{name}/start)
func (c *Client) StartShed(name string) (*config.Shed, error) {
    resp, err := c.post("/sheds/"+name+"/start", nil)
    if err != nil {
        return nil, err
    }
    defer resp.Body.Close()

    if resp.StatusCode == http.StatusNotFound {
        return nil, fmt.Errorf("shed %q not found", name)
    }
    if resp.StatusCode == http.StatusConflict {
        return nil, fmt.Errorf("shed %q is already running", name)
    }
    if resp.StatusCode != http.StatusOK {
        return nil, c.parseError(resp)
    }

    var shed config.Shed
    if err := json.NewDecoder(resp.Body).Decode(&shed); err != nil {
        return nil, fmt.Errorf("failed to parse response: %w", err)
    }

    return &shed, nil
}

// StopShed stops a running shed (POST /api/sheds/{name}/stop)
func (c *Client) StopShed(name string) (*config.Shed, error) {
    resp, err := c.post("/sheds/"+name+"/stop", nil)
    if err != nil {
        return nil, err
    }
    defer resp.Body.Close()

    if resp.StatusCode == http.StatusNotFound {
        return nil, fmt.Errorf("shed %q not found", name)
    }
    if resp.StatusCode == http.StatusConflict {
        return nil, fmt.Errorf("shed %q is already stopped", name)
    }
    if resp.StatusCode != http.StatusOK {
        return nil, c.parseError(resp)
    }

    var shed config.Shed
    if err := json.NewDecoder(resp.Body).Decode(&shed); err != nil {
        return nil, fmt.Errorf("failed to parse response: %w", err)
    }

    return &shed, nil
}

// Ping checks if the server is reachable
func (c *Client) Ping() error {
    _, err := c.GetInfo()
    return err
}

// Helper methods

func (c *Client) get(path string) (*http.Response, error) {
    return c.httpClient.Get(c.baseURL + path)
}

func (c *Client) post(path string, body io.Reader) (*http.Response, error) {
    req, err := http.NewRequest(http.MethodPost, c.baseURL+path, body)
    if err != nil {
        return nil, err
    }
    if body != nil {
        req.Header.Set("Content-Type", "application/json")
    }
    return c.httpClient.Do(req)
}

func (c *Client) delete(path string) (*http.Response, error) {
    req, err := http.NewRequest(http.MethodDelete, c.baseURL+path, nil)
    if err != nil {
        return nil, err
    }
    return c.httpClient.Do(req)
}

func (c *Client) parseError(resp *http.Response) error {
    body, _ := io.ReadAll(resp.Body)

    // Try to parse as APIError
    var apiErr config.APIError
    if err := json.Unmarshal(body, &apiErr); err == nil && apiErr.Error.Message != "" {
        return fmt.Errorf("%s", apiErr.Error.Message)
    }

    // Fallback to status text
    return fmt.Errorf("server error: %s", resp.Status)
}
```

### 7.4 Implement Server Commands

**File**: `cmd/shed/server.go`

```go
package main

import (
    "fmt"
    "os"
    "text/tabwriter"
    "time"

    "github.com/spf13/cobra"
    "github.com/charliek/shed/internal/client"
    "github.com/charliek/shed/internal/config"
)

var serverCmd = &cobra.Command{
    Use:   "server",
    Short: "Manage configured servers",
    Long:  "Commands for adding, removing, and listing shed servers.",
}

// shed server add <host>
var serverAddCmd = &cobra.Command{
    Use:   "add <host>",
    Short: "Add a new server",
    Long: `Discovers and adds a shed server to your configuration.

Connects to the server's HTTP API to retrieve server information
and SSH host key, then saves the configuration locally.

Examples:
  shed server add mini-desktop.tailnet.ts.net
  shed server add 192.168.1.100 --name homelab
  shed server add vps.example.com --http-port 9080`,
    Args: cobra.ExactArgs(1),
    RunE: runServerAdd,
}

var (
    serverAddName     string
    serverAddHTTPPort int
    serverAddSSHPort  int
)

func init() {
    serverAddCmd.Flags().StringVar(&serverAddName, "name", "", "Friendly name for server (default: derived from server info)")
    serverAddCmd.Flags().IntVar(&serverAddHTTPPort, "http-port", 8080, "HTTP API port")
    serverAddCmd.Flags().IntVar(&serverAddSSHPort, "ssh-port", 2222, "SSH port")

    serverCmd.AddCommand(serverAddCmd)
    serverCmd.AddCommand(serverListCmd)
    serverCmd.AddCommand(serverRemoveCmd)
    serverCmd.AddCommand(serverSetDefaultCmd)
    rootCmd.AddCommand(serverCmd)
}

func runServerAdd(cmd *cobra.Command, args []string) error {
    host := args[0]

    // Create client for discovery
    c := client.NewClientFromHostPort(host, serverAddHTTPPort)

    // Step 1: Get server info
    verbose("Connecting to %s:%d...", host, serverAddHTTPPort)
    info, err := c.GetInfo()
    if err != nil {
        return fmt.Errorf("cannot connect to server: %w\n\nCheck:\n  - Server is running: curl http://%s:%d/api/info\n  - Network is reachable: ping %s", host, serverAddHTTPPort, host)
    }

    fmt.Printf("Connected to %s\n", host)
    fmt.Printf("  Server version: %s\n", info.Version)

    // Step 2: Get SSH host key
    verbose("Retrieving SSH host key...")
    hostKey, err := c.GetSSHHostKey()
    if err != nil {
        return fmt.Errorf("failed to get SSH host key: %w", err)
    }
    fmt.Println("  Retrieved SSH host key")

    // Determine server name
    name := serverAddName
    if name == "" {
        name = info.Name
    }
    if name == "" {
        // Extract hostname from host (remove domain)
        name = host
        for i, c := range name {
            if c == '.' {
                name = name[:i]
                break
            }
        }
    }

    // Check if server already exists
    if _, err := clientConfig.GetServer(name); err == nil {
        return fmt.Errorf("server %q already configured\n\nTry:\n  shed server remove %s  # Remove existing\n  shed server add %s --name different-name", name, name, host)
    }

    // Step 3: Add to config
    entry := config.ServerEntry{
        Host:     host,
        HTTPPort: serverAddHTTPPort,
        SSHPort:  info.SSHPort, // Use port from server, not flag (server knows its config)
        AddedAt:  time.Now(),
    }

    // Override SSH port if explicitly set
    if cmd.Flags().Changed("ssh-port") {
        entry.SSHPort = serverAddSSHPort
    }

    if err := clientConfig.AddServer(name, entry); err != nil {
        return fmt.Errorf("failed to add server: %w", err)
    }

    // Step 4: Add host key to known_hosts
    if err := config.AddKnownHost(host, entry.SSHPort, hostKey); err != nil {
        return fmt.Errorf("failed to add known host: %w", err)
    }

    // Step 5: Set as default if first server
    isFirst := len(clientConfig.Servers) == 1
    if isFirst {
        if err := clientConfig.SetDefaultServer(name); err != nil {
            return fmt.Errorf("failed to set default server: %w", err)
        }
    }

    // Step 6: Save config
    if err := clientConfig.Save(); err != nil {
        return fmt.Errorf("failed to save config: %w", err)
    }

    if isFirst {
        fmt.Printf("Server %q added and set as default\n", name)
    } else {
        fmt.Printf("Server %q added\n", name)
    }

    return nil
}

// shed server list
var serverListCmd = &cobra.Command{
    Use:   "list",
    Short: "List configured servers",
    Long:  "Shows all configured servers with their connection status.",
    RunE:  runServerList,
}

func runServerList(cmd *cobra.Command, args []string) error {
    if len(clientConfig.Servers) == 0 {
        fmt.Println("No servers configured.")
        fmt.Println("\nTry:\n  shed server add <host>")
        return nil
    }

    w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
    fmt.Fprintln(w, "NAME\tHOST\tSTATUS\tDEFAULT")

    for name, server := range clientConfig.Servers {
        // Check if server is online
        c := client.NewClient(&server)
        status := "offline"
        if err := c.Ping(); err == nil {
            status = "online"
        }

        // Check if default
        defaultMarker := ""
        if name == clientConfig.DefaultServer {
            defaultMarker = "*"
        }

        fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", name, server.Host, status, defaultMarker)
    }

    w.Flush()
    return nil
}

// shed server remove <name>
var serverRemoveCmd = &cobra.Command{
    Use:   "remove <name>",
    Short: "Remove a server from configuration",
    Long: `Removes a server from your local configuration.

This does not affect the server itself, only removes it from
the CLI's list of known servers.`,
    Args: cobra.ExactArgs(1),
    RunE: runServerRemove,
}

func runServerRemove(cmd *cobra.Command, args []string) error {
    name := args[0]

    // Check server exists
    if _, err := clientConfig.GetServer(name); err != nil {
        return fmt.Errorf("server %q not found\n\nTry:\n  shed server list", name)
    }

    // Remove server
    if err := clientConfig.RemoveServer(name); err != nil {
        return fmt.Errorf("failed to remove server: %w", err)
    }

    // Save config
    if err := clientConfig.Save(); err != nil {
        return fmt.Errorf("failed to save config: %w", err)
    }

    fmt.Printf("Removed server %q\n", name)

    // Warn if it was the default
    if name == clientConfig.DefaultServer && len(clientConfig.Servers) > 0 {
        fmt.Println("\nNote: This was your default server. Set a new default with:")
        fmt.Println("  shed server set-default <name>")
    }

    return nil
}

// shed server set-default <name>
var serverSetDefaultCmd = &cobra.Command{
    Use:   "set-default <name>",
    Short: "Set the default server",
    Long:  "Sets which server is used when --server is not specified.",
    Args:  cobra.ExactArgs(1),
    RunE:  runServerSetDefault,
}

func runServerSetDefault(cmd *cobra.Command, args []string) error {
    name := args[0]

    // Check server exists
    if _, err := clientConfig.GetServer(name); err != nil {
        return fmt.Errorf("server %q not found\n\nTry:\n  shed server list", name)
    }

    // Set default
    if err := clientConfig.SetDefaultServer(name); err != nil {
        return fmt.Errorf("failed to set default: %w", err)
    }

    // Save config
    if err := clientConfig.Save(); err != nil {
        return fmt.Errorf("failed to save config: %w", err)
    }

    fmt.Printf("Default server set to %q\n", name)
    return nil
}
```

### 7.5 Implement Shed Management Commands

**File**: `cmd/shed/shed.go`

```go
package main

import (
    "bufio"
    "fmt"
    "os"
    "strings"
    "text/tabwriter"
    "time"

    "github.com/spf13/cobra"
    "github.com/charliek/shed/internal/client"
    "github.com/charliek/shed/internal/config"
)

// shed create <name>
var createCmd = &cobra.Command{
    Use:   "create <name>",
    Short: "Create a new shed",
    Long: `Creates a new development environment container.

The shed name must be alphanumeric with optional hyphens.

Examples:
  shed create scratch                           # Empty shed
  shed create myproject --repo owner/repo       # Clone a repository
  shed create test --server cloud-vps           # Create on specific server`,
    Args: cobra.ExactArgs(1),
    RunE: runCreate,
}

var (
    createRepo   string
    createImage  string
)

func init() {
    createCmd.Flags().StringVarP(&createRepo, "repo", "r", "", "GitHub repo to clone (owner/repo)")
    createCmd.Flags().StringVar(&createImage, "image", "", "Base Docker image (default: server's default)")

    rootCmd.AddCommand(createCmd)
    rootCmd.AddCommand(listCmd)
    rootCmd.AddCommand(deleteCmd)
    rootCmd.AddCommand(stopCmd)
    rootCmd.AddCommand(startCmd)
}

func runCreate(cmd *cobra.Command, args []string) error {
    name := args[0]

    // Validate name
    if !isValidShedName(name) {
        return fmt.Errorf("invalid shed name %q\n\nShed names must be alphanumeric with optional hyphens.", name)
    }

    // Get target server
    server, serverName, err := getTargetServer()
    if err != nil {
        return err
    }

    // Create API client
    c := client.NewClient(server)

    // Build request
    req := &config.CreateShedRequest{
        Name:  name,
        Repo:  createRepo,
        Image: createImage,
    }

    // Create shed
    fmt.Printf("Creating shed %q on %s...\n", name, serverName)
    if createRepo != "" {
        fmt.Printf("Cloning %s...\n", createRepo)
    }

    shed, err := c.CreateShed(req)
    if err != nil {
        return fmt.Errorf("failed to create shed: %w", err)
    }

    // Cache shed location
    clientConfig.CacheShed(name, config.ShedCache{
        Server:    serverName,
        Status:    shed.Status,
        UpdatedAt: time.Now(),
    })
    if err := clientConfig.Save(); err != nil {
        verbose("Warning: failed to update shed cache: %v", err)
    }

    fmt.Println("Shed ready")
    fmt.Printf("\nConnect with: shed console %s\n", name)

    return nil
}

// isValidShedName checks if a name is valid (alphanumeric + hyphens)
func isValidShedName(name string) bool {
    if name == "" {
        return false
    }
    for _, c := range name {
        if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '-') {
            return false
        }
    }
    // Cannot start or end with hyphen
    if name[0] == '-' || name[len(name)-1] == '-' {
        return false
    }
    return true
}

// shed list
var listCmd = &cobra.Command{
    Use:   "list",
    Short: "List sheds",
    Long: `Lists all sheds on the default server, or all servers with --all.

Examples:
  shed list                  # List sheds on default server
  shed list --all            # List sheds on all servers
  shed list --server cloud   # List sheds on specific server`,
    RunE: runList,
}

var listAll bool

func init() {
    listCmd.Flags().BoolVarP(&listAll, "all", "a", false, "List from all servers")
}

func runList(cmd *cobra.Command, args []string) error {
    type shedWithServer struct {
        config.Shed
        ServerName string
    }
    var allSheds []shedWithServer

    if listAll {
        // Query all servers
        for name, server := range clientConfig.Servers {
            c := client.NewClient(&server)
            sheds, err := c.ListSheds()
            if err != nil {
                verbose("Warning: cannot reach %s: %v", name, err)
                continue
            }
            for _, shed := range sheds {
                allSheds = append(allSheds, shedWithServer{shed, name})
                // Update cache
                clientConfig.CacheShed(shed.Name, config.ShedCache{
                    Server:    name,
                    Status:    shed.Status,
                    UpdatedAt: time.Now(),
                })
            }
        }
    } else {
        // Query single server
        server, serverName, err := getTargetServer()
        if err != nil {
            return err
        }

        c := client.NewClient(server)
        sheds, err := c.ListSheds()
        if err != nil {
            return fmt.Errorf("cannot reach server %s: %w", serverName, err)
        }

        for _, shed := range sheds {
            allSheds = append(allSheds, shedWithServer{shed, serverName})
            // Update cache
            clientConfig.CacheShed(shed.Name, config.ShedCache{
                Server:    serverName,
                Status:    shed.Status,
                UpdatedAt: time.Now(),
            })
        }
    }

    // Save updated cache
    if err := clientConfig.Save(); err != nil {
        verbose("Warning: failed to update shed cache: %v", err)
    }

    if len(allSheds) == 0 {
        fmt.Println("No sheds found.")
        fmt.Println("\nTry:\n  shed create <name>")
        return nil
    }

    w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
    fmt.Fprintln(w, "SERVER\tNAME\tSTATUS\tCREATED\tREPO")

    for _, shed := range allSheds {
        repo := shed.Repo
        if repo == "" {
            repo = "-"
        }
        created := formatRelativeTime(shed.CreatedAt)
        fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", shed.ServerName, shed.Name, shed.Status, created, repo)
    }

    w.Flush()
    return nil
}

// formatRelativeTime formats a time as relative (e.g., "2 hours ago")
func formatRelativeTime(t time.Time) string {
    d := time.Since(t)

    switch {
    case d < time.Minute:
        return "just now"
    case d < time.Hour:
        mins := int(d.Minutes())
        if mins == 1 {
            return "1 minute ago"
        }
        return fmt.Sprintf("%d minutes ago", mins)
    case d < 24*time.Hour:
        hours := int(d.Hours())
        if hours == 1 {
            return "1 hour ago"
        }
        return fmt.Sprintf("%d hours ago", hours)
    default:
        days := int(d.Hours() / 24)
        if days == 1 {
            return "1 day ago"
        }
        return fmt.Sprintf("%d days ago", days)
    }
}

// shed delete <name>
var deleteCmd = &cobra.Command{
    Use:   "delete <name>",
    Short: "Delete a shed",
    Long: `Deletes a shed and its data.

By default, prompts for confirmation. Use --force to skip.
Use --keep-volume to preserve the workspace data.`,
    Args: cobra.ExactArgs(1),
    RunE: runDelete,
}

var (
    deleteKeepVolume bool
    deleteForce      bool
)

func init() {
    deleteCmd.Flags().BoolVar(&deleteKeepVolume, "keep-volume", false, "Preserve workspace data")
    deleteCmd.Flags().BoolVarP(&deleteForce, "force", "f", false, "Skip confirmation")
}

func runDelete(cmd *cobra.Command, args []string) error {
    name := args[0]

    // Resolve server from cache or flag
    server, serverName, err := resolveShedServer(name)
    if err != nil {
        return err
    }

    // Confirmation prompt
    if !deleteForce {
        message := fmt.Sprintf("Delete shed %q and all its data?", name)
        if deleteKeepVolume {
            message = fmt.Sprintf("Delete shed %q (keeping workspace data)?", name)
        }
        if !confirm(message) {
            fmt.Println("Aborted.")
            return nil
        }
    }

    // Create API client and delete
    c := client.NewClient(server)
    if err := c.DeleteShed(name, deleteKeepVolume); err != nil {
        return fmt.Errorf("failed to delete shed: %w", err)
    }

    // Remove from cache
    delete(clientConfig.Sheds, name)
    if err := clientConfig.Save(); err != nil {
        verbose("Warning: failed to update cache: %v", err)
    }

    fmt.Printf("Deleted shed %q from %s\n", name, serverName)
    return nil
}

// confirm asks user for yes/no confirmation
func confirm(message string) bool {
    fmt.Printf("%s [y/N]: ", message)
    reader := bufio.NewReader(os.Stdin)
    input, _ := reader.ReadString('\n')
    input = strings.TrimSpace(strings.ToLower(input))
    return input == "y" || input == "yes"
}

// shed stop <name>
var stopCmd = &cobra.Command{
    Use:   "stop <name>",
    Short: "Stop a running shed",
    Args:  cobra.ExactArgs(1),
    RunE:  runStop,
}

func runStop(cmd *cobra.Command, args []string) error {
    name := args[0]

    server, serverName, err := resolveShedServer(name)
    if err != nil {
        return err
    }

    c := client.NewClient(server)
    _, err = c.StopShed(name)
    if err != nil {
        return fmt.Errorf("failed to stop shed: %w", err)
    }

    // Update cache
    if cache, ok := clientConfig.Sheds[name]; ok {
        cache.Status = config.StatusStopped
        cache.UpdatedAt = time.Now()
        clientConfig.Sheds[name] = cache
        clientConfig.Save()
    }

    fmt.Printf("Stopped shed %q on %s\n", name, serverName)
    return nil
}

// shed start <name>
var startCmd = &cobra.Command{
    Use:   "start <name>",
    Short: "Start a stopped shed",
    Args:  cobra.ExactArgs(1),
    RunE:  runStart,
}

func runStart(cmd *cobra.Command, args []string) error {
    name := args[0]

    server, serverName, err := resolveShedServer(name)
    if err != nil {
        return err
    }

    c := client.NewClient(server)
    _, err = c.StartShed(name)
    if err != nil {
        return fmt.Errorf("failed to start shed: %w", err)
    }

    // Update cache
    if cache, ok := clientConfig.Sheds[name]; ok {
        cache.Status = config.StatusRunning
        cache.UpdatedAt = time.Now()
        clientConfig.Sheds[name] = cache
        clientConfig.Save()
    }

    fmt.Printf("Started shed %q on %s\n", name, serverName)
    return nil
}

// resolveShedServer finds which server a shed is on
// Priority: --server flag > cache > query default server > query all servers
func resolveShedServer(shedName string) (*config.ServerEntry, string, error) {
    // If --server flag specified, use that
    if serverFlag != "" {
        server, err := clientConfig.GetServer(serverFlag)
        if err != nil {
            return nil, "", fmt.Errorf("server %q not found", serverFlag)
        }
        return server, serverFlag, nil
    }

    // Check cache
    if cached, ok := clientConfig.Sheds[shedName]; ok {
        server, err := clientConfig.GetServer(cached.Server)
        if err == nil {
            verbose("Found %s in cache on %s", shedName, cached.Server)
            return server, cached.Server, nil
        }
    }

    // Query default server
    if clientConfig.DefaultServer != "" {
        server, err := clientConfig.GetServer(clientConfig.DefaultServer)
        if err == nil {
            c := client.NewClient(server)
            if _, err := c.GetShed(shedName); err == nil {
                // Found it, update cache
                clientConfig.CacheShed(shedName, config.ShedCache{
                    Server:    clientConfig.DefaultServer,
                    UpdatedAt: time.Now(),
                })
                clientConfig.Save()
                return server, clientConfig.DefaultServer, nil
            }
        }
    }

    // Try all servers
    for name, server := range clientConfig.Servers {
        c := client.NewClient(&server)
        if _, err := c.GetShed(shedName); err == nil {
            // Found it, update cache
            serverCopy := server
            clientConfig.CacheShed(shedName, config.ShedCache{
                Server:    name,
                UpdatedAt: time.Now(),
            })
            clientConfig.Save()
            return &serverCopy, name, nil
        }
    }

    return nil, "", fmt.Errorf("shed %q not found\n\nTry:\n  shed list --all       # Find which server has it\n  shed create %s  # Create a new shed", shedName, shedName)
}
```

### 7.6 Implement Interactive Commands

**File**: `cmd/shed/console.go`

```go
package main

import (
    "fmt"
    "os"
    "os/exec"
    "syscall"

    "github.com/spf13/cobra"
    "github.com/charliek/shed/internal/client"
    "github.com/charliek/shed/internal/config"
)

// shed console <name>
var consoleCmd = &cobra.Command{
    Use:   "console <name>",
    Short: "Open an interactive shell in a shed",
    Long: `Opens an SSH connection to a shed's container.

If the shed is stopped, it will be started automatically.

Examples:
  shed console myproject      # Connect to shed
  shed console myproject -s cloud   # Connect to shed on specific server`,
    Args: cobra.ExactArgs(1),
    RunE: runConsole,
}

func init() {
    rootCmd.AddCommand(consoleCmd)
    rootCmd.AddCommand(execCmd)
}

func runConsole(cmd *cobra.Command, args []string) error {
    name := args[0]

    // Resolve server
    server, serverName, err := resolveShedServer(name)
    if err != nil {
        return err
    }

    // Check if shed needs to be started
    c := client.NewClient(server)
    shed, err := c.GetShed(name)
    if err != nil {
        return fmt.Errorf("shed %q not found on %s", name, serverName)
    }

    if shed.Status == config.StatusStopped {
        fmt.Printf("Starting shed %q...\n", name)
        if _, err := c.StartShed(name); err != nil {
            return fmt.Errorf("failed to start shed: %w", err)
        }
    }

    // Build SSH command
    sshArgs := buildSSHArgs(server, name)
    verbose("Executing: ssh %v", sshArgs)

    // Execute SSH, replacing current process
    return execSSH(sshArgs)
}

// shed exec <name> <command...>
var execCmd = &cobra.Command{
    Use:   "exec <name> <command...>",
    Short: "Execute a command in a shed",
    Long: `Executes a command in a shed and returns the output.

Examples:
  shed exec myproject git status
  shed exec myproject "cd /workspace && npm test"`,
    Args: cobra.MinimumNArgs(2),
    RunE: runExec,
}

func runExec(cmd *cobra.Command, args []string) error {
    name := args[0]
    command := args[1:]

    // Resolve server
    server, serverName, err := resolveShedServer(name)
    if err != nil {
        return err
    }

    // Check if shed needs to be started
    c := client.NewClient(server)
    shed, err := c.GetShed(name)
    if err != nil {
        return fmt.Errorf("shed %q not found on %s", name, serverName)
    }

    if shed.Status == config.StatusStopped {
        verbose("Starting stopped shed %q...", name)
        if _, err := c.StartShed(name); err != nil {
            return fmt.Errorf("failed to start shed: %w", err)
        }
    }

    // Build SSH command with remote command
    sshArgs := buildSSHArgs(server, name)
    sshArgs = append(sshArgs, command...)
    verbose("Executing: ssh %v", sshArgs)

    // Execute SSH, replacing current process
    return execSSH(sshArgs)
}

// buildSSHArgs constructs SSH arguments for connecting to a shed
func buildSSHArgs(server *config.ServerEntry, shedName string) []string {
    knownHostsPath := config.GetKnownHostsPath()

    return []string{
        "-t",                                          // Force pseudo-terminal allocation
        "-p", fmt.Sprintf("%d", server.SSHPort),       // Port
        "-o", fmt.Sprintf("UserKnownHostsFile=%s", knownHostsPath), // Our known_hosts
        "-o", "StrictHostKeyChecking=yes",             // Require known host
        fmt.Sprintf("%s@%s", shedName, server.Host),   // user@host
    }
}

// execSSH executes the SSH command, replacing the current process
func execSSH(args []string) error {
    // Find ssh binary
    sshPath, err := exec.LookPath("ssh")
    if err != nil {
        return fmt.Errorf("ssh not found in PATH: %w", err)
    }

    // Prepare full args (ssh as argv[0])
    fullArgs := append([]string{"ssh"}, args...)

    // Replace current process with ssh
    // This ensures proper signal handling and terminal behavior
    err = syscall.Exec(sshPath, fullArgs, os.Environ())
    if err != nil {
        return fmt.Errorf("failed to execute ssh: %w", err)
    }

    // Should never reach here
    return nil
}
```

### 7.7 Wire Up main.go

**File**: `cmd/shed/main.go`

```go
package main

import (
    "fmt"

    "github.com/charliek/shed/internal/version"
)

func main() {
    Execute()
}

func init() {
    // Add version command
    rootCmd.AddCommand(versionCmd)
}

var versionCmd = &cobra.Command{
    Use:   "version",
    Short: "Print version information",
    Run: func(cmd *cobra.Command, args []string) {
        fmt.Printf("shed %s\n", version.Version)
    },
}
```

**Note**: The imports need `github.com/spf13/cobra` in main.go.

### 7.8 Test CLI Commands

**File**: `cmd/shed/root_test.go`

```go
package main

import (
    "testing"
)

func TestIsValidShedName(t *testing.T) {
    tests := []struct {
        name  string
        valid bool
    }{
        {"myproject", true},
        {"my-project", true},
        {"my-project-123", true},
        {"MyProject", true},
        {"123", true},
        {"", false},
        {"-myproject", false},
        {"myproject-", false},
        {"my_project", false},
        {"my project", false},
        {"my.project", false},
    }

    for _, tt := range tests {
        t.Run(tt.name, func(t *testing.T) {
            if got := isValidShedName(tt.name); got != tt.valid {
                t.Errorf("isValidShedName(%q) = %v, want %v", tt.name, got, tt.valid)
            }
        })
    }
}
```

**File**: `internal/client/http_test.go`

```go
package client

import (
    "encoding/json"
    "net/http"
    "net/http/httptest"
    "testing"

    "github.com/charliek/shed/internal/config"
)

func TestClientGetInfo(t *testing.T) {
    // Create mock server
    server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        if r.URL.Path != "/api/info" {
            t.Errorf("unexpected path: %s", r.URL.Path)
        }
        json.NewEncoder(w).Encode(config.ServerInfo{
            Name:     "test-server",
            Version:  "1.0.0",
            SSHPort:  2222,
            HTTPPort: 8080,
        })
    }))
    defer server.Close()

    // Create client (extract host and port from test server)
    c := &Client{
        baseURL:    server.URL + "/api",
        httpClient: server.Client(),
    }

    info, err := c.GetInfo()
    if err != nil {
        t.Fatalf("GetInfo failed: %v", err)
    }

    if info.Name != "test-server" {
        t.Errorf("expected name 'test-server', got %q", info.Name)
    }
}

func TestClientListSheds(t *testing.T) {
    server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        json.NewEncoder(w).Encode(config.ShedsResponse{
            Sheds: []config.Shed{
                {Name: "shed1", Status: "running"},
                {Name: "shed2", Status: "stopped"},
            },
        })
    }))
    defer server.Close()

    c := &Client{
        baseURL:    server.URL + "/api",
        httpClient: server.Client(),
    }

    sheds, err := c.ListSheds()
    if err != nil {
        t.Fatalf("ListSheds failed: %v", err)
    }

    if len(sheds) != 2 {
        t.Errorf("expected 2 sheds, got %d", len(sheds))
    }
}

func TestClientCreateShed(t *testing.T) {
    server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        if r.Method != http.MethodPost {
            t.Errorf("expected POST, got %s", r.Method)
        }

        var req config.CreateShedRequest
        json.NewDecoder(r.Body).Decode(&req)

        w.WriteHeader(http.StatusCreated)
        json.NewEncoder(w).Encode(config.Shed{
            Name:   req.Name,
            Status: "running",
            Repo:   req.Repo,
        })
    }))
    defer server.Close()

    c := &Client{
        baseURL:    server.URL + "/api",
        httpClient: server.Client(),
    }

    shed, err := c.CreateShed(&config.CreateShedRequest{
        Name: "test-shed",
        Repo: "owner/repo",
    })
    if err != nil {
        t.Fatalf("CreateShed failed: %v", err)
    }

    if shed.Name != "test-shed" {
        t.Errorf("expected name 'test-shed', got %q", shed.Name)
    }
}
```

---

## Deliverables Checklist

- [ ] `github.com/spf13/cobra` dependency added
- [ ] `cmd/shed/main.go` implemented
- [ ] `cmd/shed/root.go` with global flags implemented
- [ ] `internal/client/http.go` HTTP client wrapper implemented
- [ ] `cmd/shed/server.go` with server commands:
  - [ ] `shed server add <host>` discovers and adds server
  - [ ] `shed server list` shows servers with online/offline status
  - [ ] `shed server remove <name>` removes server
  - [ ] `shed server set-default <name>` sets default
- [ ] `cmd/shed/shed.go` with shed management commands:
  - [ ] `shed create <name>` creates shed
  - [ ] `shed list` lists sheds (with --all flag)
  - [ ] `shed delete <name>` deletes shed (with --keep-volume, --force)
  - [ ] `shed stop <name>` stops shed
  - [ ] `shed start <name>` starts shed
- [ ] `cmd/shed/console.go` with interactive commands:
  - [ ] `shed console <name>` opens SSH session
  - [ ] `shed exec <name> <command...>` runs command
- [ ] `cmd/shed/root_test.go` with validation tests
- [ ] `internal/client/http_test.go` with HTTP client tests
- [ ] CLI builds successfully: `go build ./cmd/shed`

---

## Deviations & Notes

| Item | Description |
|------|-------------|
| | |

---

## Completion Criteria
- All deliverables checked off
- `go build ./cmd/shed` succeeds
- `./bin/shed --help` shows all commands
- `go test ./cmd/shed/... ./internal/client/...` passes
- Manual test: `shed server add localhost` works against running shed-server
- Manual test: `shed create test && shed console test` works
- Update epic progress tracker to mark Phase 7 complete
