# Shed Development Guide

This guide covers setting up a development environment for contributing to Shed.

## Prerequisites

- Go 1.24 or later
- Docker (for building the base image and testing)
- Make
- Git

## Getting Started

### Clone the Repository

```bash
git clone https://github.com/charliek/shed.git
cd shed
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
│   │   ├── list.go         # list command
│   │   ├── console.go      # console command
│   │   ├── exec.go         # exec command
│   │   ├── delete.go       # delete command
│   │   ├── start.go        # start command
│   │   ├── stop.go         # stop command
│   │   └── server.go       # server subcommand (add/list/remove)
│   └── shed-server/        # Server binary
│       ├── main.go         # Entry point
│       ├── serve.go        # serve command
│       ├── install.go      # systemd install
│       └── uninstall.go    # systemd uninstall
├── internal/
│   ├── api/                # HTTP API handlers
│   │   ├── server.go       # Router and middleware
│   │   └── handlers.go     # Request handlers
│   ├── config/             # Configuration types and loading
│   │   ├── types.go        # Shared types
│   │   ├── server.go       # Server config
│   │   └── client.go       # Client config
│   ├── docker/             # Docker client wrapper
│   │   ├── client.go       # Docker connection
│   │   ├── containers.go   # Container operations
│   │   └── volumes.go      # Volume operations
│   ├── sshd/               # SSH server
│   │   ├── server.go       # SSH server
│   │   └── session.go      # Session handling
│   ├── sshconfig/          # SSH config file management
│   │   ├── parser.go       # Parse SSH config
│   │   └── writer.go       # Write SSH config
│   └── version/            # Version information
│       └── version.go
├── scripts/
│   └── build-image.sh      # Build shed-base Docker image
├── configs/
│   ├── server.example.yaml # Example server config
│   └── server.dev.yaml     # Development config
├── docs/
│   ├── spec.md             # Technical specification
│   ├── SERVER_SETUP.md     # Server setup guide
│   └── DEVELOPMENT.md      # This file
├── Dockerfile              # shed-base image definition
├── Makefile                # Build automation
├── go.mod                  # Go module definition
└── README.md               # Project overview
```

## Running Locally

### Single Machine Development

You can run both the CLI and server on the same machine for development:

```bash
# Terminal 1: Start the server
./bin/shed-server serve

# Terminal 2: Use the CLI
./bin/shed server add localhost
./bin/shed create test-shed
./bin/shed console test-shed
```

### Development Configuration

Use the development config for local testing:

```bash
./bin/shed-server serve -c configs/server.dev.yaml
```

## Making Changes

### Adding a New CLI Command

1. Create a new file in `cmd/shed/` (e.g., `newcmd.go`)
2. Define a `cobra.Command` variable
3. Register it in `cmd/shed/main.go`'s `init()` function
4. Implement the command logic

Example:

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

### Adding Docker Operations

1. Add the method to `internal/docker/client.go` or `internal/docker/containers.go`
2. Add corresponding tests

## Testing

### Unit Tests

Place unit tests in `*_test.go` files alongside the code:

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
        {"too-long", "a" + strings.Repeat("b", 63), true},
    }

    for _, tt := range tests {
        t.Run(tt.name, func(t *testing.T) {
            err := ValidateShedName(tt.input)
            if (err != nil) != tt.wantErr {
                t.Errorf("ValidateShedName(%q) error = %v, wantErr %v", tt.input, err, tt.wantErr)
            }
        })
    }
}
```

### Integration Tests

For tests requiring Docker, use build tags:

```go
//go:build integration

package docker

import "testing"

func TestCreateShed_Integration(t *testing.T) {
    // Test with real Docker
}
```

Run integration tests:

```bash
go test -tags=integration ./...
```

## Code Style

- Follow standard Go conventions
- Use `gofmt` for formatting
- Use meaningful variable and function names
- Add comments for exported functions
- Keep functions focused and small
- Prefer returning errors over panicking

## Continuous Integration

This project uses GitHub Actions for CI. The workflow runs on:
- Push to `main` or `feature/*` branches
- Pull requests to `main` or `feature/*` branches

### CI Jobs

1. **Test**: Runs all unit tests
2. **Lint**: Runs golangci-lint with project configuration
3. **Dockerfile Lint**: Runs hadolint on the shed-base Dockerfile

### Running Checks Locally

Before pushing, run the same checks that CI runs:

```bash
make check           # Run lint + test
make lint            # Go linting only
make lint-dockerfile # Dockerfile linting (requires hadolint)
make coverage        # Tests with coverage report
```

### Installing Tools

```bash
# golangci-lint
go install github.com/golangci/golangci-lint/cmd/golangci-lint@v1.64.5

# hadolint (for Dockerfile linting)
# macOS: brew install hadolint
# Linux: see https://github.com/hadolint/hadolint#install
```

## Submitting Changes

1. Fork the repository
2. Create a feature branch: `git checkout -b feature/my-feature`
3. Make your changes
4. Run tests: `make test`
5. Run linter: `make lint`
6. Commit with a clear message
7. Push and create a pull request

## Common Tasks

### Updating Dependencies

```bash
go get -u ./...
go mod tidy
```

### Adding a New Dependency

```bash
go get github.com/example/package
```

### Regenerating Mocks

If using mockgen:

```bash
go generate ./...
```

## Debugging

### Server Logs

```bash
# Run server with debug logging
LOG_LEVEL=debug ./bin/shed-server serve
```

### Docker Inspection

```bash
# List shed containers
docker ps --filter "label=shed=true"

# Inspect a container
docker inspect shed-myproject

# View container logs
docker logs shed-myproject
```

### API Testing

```bash
# Test API endpoints
curl http://localhost:8080/api/info
curl http://localhost:8080/api/sheds
```
