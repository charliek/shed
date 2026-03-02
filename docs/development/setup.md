# Development Setup

This guide covers setting up a development environment for contributing to Shed.

## Prerequisites

- [mise](https://mise.jdx.dev/) (manages Go, golangci-lint, and other tool versions)
- Docker (for building the base image and testing)
- Make
- Git

## Getting Started

### Clone the Repository

```bash
git clone https://github.com/charliek/shed.git
cd shed
```

### Install Tools

```bash
# Install Go, golangci-lint, and other tools defined in .mise.toml
mise install
```

### Build

```bash
# Build both binaries
make build

# Binaries are placed in bin/
ls bin/
# shed  shed-server
```

### Run Tests

```bash
# Run all tests
make test

# Run tests with coverage
make coverage

# Run tests with race detection
go test -race ./...
```

### Code Quality

```bash
# Run linter (requires golangci-lint)
make lint

# Format code
make fmt

# Run all checks
make check
```

## Project Structure

```
shed/
├── cmd/
│   ├── shed/               # CLI binary
│   │   ├── main.go         # Entry point
│   │   ├── client.go       # HTTP client for API
│   │   ├── create.go       # create command
│   │   ├── console.go      # console command
│   │   └── ...
│   ├── shed-server/        # Server binary
│   │   ├── main.go         # Entry point
│   │   ├── serve.go        # serve command
│   │   └── install.go      # systemd install
│   └── shed-agent/         # In-VM agent binary (Firecracker and VZ)
│       └── main.go
├── internal/
│   ├── api/                # HTTP API handlers
│   ├── agentproto/         # Vsock binary protocol
│   ├── backend/            # Backend interface
│   ├── config/             # Configuration types
│   ├── docker/             # Docker client wrapper
│   ├── firecracker/        # Firecracker backend
│   ├── vz/                 # VZ backend (macOS, build tag: darwin)
│   ├── vmutil/             # Shared VM utilities (AgentClient, Dialer)
│   ├── sshd/               # SSH server
│   ├── sshconfig/          # SSH config management
│   ├── provision/          # Provisioning hooks
│   ├── sync/               # File synchronization
│   ├── tunnels/            # SSH tunnel management
│   └── version/            # Version information
├── scripts/
│   ├── build-image.sh              # Build shed-base Docker image
│   ├── build-firecracker-rootfs.sh # Build Firecracker rootfs
│   ├── build-firecracker-kernel.sh # Build custom 6.1 kernel
│   ├── build-vz-rootfs.sh          # Build VZ rootfs, kernel, and initrd
│   └── download-firecracker.sh     # Download Firecracker binary
├── vz/                                # VZ Dockerfile and assets
│   ├── Dockerfile
│   └── ...
├── configs/
│   ├── server.example.yaml
│   ├── server.dev.yaml
│   └── server.localmac.yaml           # macOS VZ development config
├── docs/
├── Makefile
└── go.mod
```

## Running Locally

### Single Machine Development

Run both CLI and server on the same machine:

```bash
# Terminal 1: Start the server
./bin/shed-server serve -c configs/server.dev.yaml

# Terminal 2: Use the CLI
./bin/shed server add localhost
./bin/shed create test-shed
./bin/shed console test-shed
```

## Making Changes

### Adding a New CLI Command

1. Create a new file in `cmd/shed/` (e.g., `newcmd.go`)
2. Define a `cobra.Command` variable
3. Register it in `cmd/shed/main.go`
4. Implement the command logic

```go
// cmd/shed/newcmd.go
package main

import "github.com/spf13/cobra"

var newCmd = &cobra.Command{
    Use:   "newcmd",
    Short: "Description of the new command",
    RunE:  runNewCmd,
}

func runNewCmd(cmd *cobra.Command, args []string) error {
    // Implementation
    return nil
}
```

### Adding a New API Endpoint

1. Add the route in `internal/api/server.go`
2. Add the handler in `internal/api/handlers.go`
3. Add any new types to `internal/config/types.go`

## Testing

### Unit Tests

Place unit tests alongside the code:

```go
// internal/config/types_test.go
package config

import "testing"

func TestValidateShedName(t *testing.T) {
    tests := []struct {
        name    string
        input   string
        wantErr bool
    }{
        {"valid", "my-shed", false},
        {"empty", "", true},
    }

    for _, tt := range tests {
        t.Run(tt.name, func(t *testing.T) {
            err := ValidateShedName(tt.input)
            if (err != nil) != tt.wantErr {
                t.Errorf("ValidateShedName(%q) error = %v", tt.input, err)
            }
        })
    }
}
```

### Integration Tests

Use build tags for Docker-dependent tests:

```go
//go:build integration

package docker

func TestCreateShed_Integration(t *testing.T) {
    // Test with real Docker
}
```

Run integration tests:

```bash
go test -tags=integration ./...
```

## Continuous Integration

GitHub Actions runs on push to `main` or `feature/*` branches:

1. **Test**: Runs all unit tests
2. **Lint**: Runs golangci-lint
3. **Dockerfile Lint**: Runs hadolint

Run checks locally before pushing:

```bash
make check
```

### Installing Tools

```bash
# Go and golangci-lint are managed by mise (see .mise.toml)
mise install

# hadolint (for Dockerfile linting)
# macOS: brew install hadolint
```

## Building Documentation

```bash
# Build documentation
make docs

# Serve locally
make docs-serve
# Visit http://127.0.0.1:7070
```

## Debugging

### Server Logs

```bash
LOG_LEVEL=debug ./bin/shed-server serve
```

### Docker Inspection

```bash
docker ps --filter "label=shed=true"
docker inspect shed-myproject
docker logs shed-myproject
```

### API Testing

```bash
curl http://localhost:8080/api/info
curl http://localhost:8080/api/sheds
```
