# Phase 1: Project Scaffold

## Overview
- **Phase**: 1 of 9
- **Epic**: [setup-epic.md](./setup-epic.md)
- **Status**: NOT STARTED
- **Estimated Effort**: Small

## Objective
Initialize the Go module, create the complete directory structure, set up the Makefile with build targets, and implement the version package for build-time version injection.

## Prerequisites
- Go 1.24 installed (configured via .mise.toml)
- Git repository initialized (already done)

## Context for New Engineers
This phase establishes the foundation for the entire project. The directory structure follows Go best practices with:
- `cmd/` - Main applications (each subdirectory is a binary)
- `internal/` - Private packages (not importable by external projects)
- `configs/` - Configuration file examples
- `images/` - Docker-related files
- `scripts/` - Build and utility scripts

---

## Progress Tracker

| Task | Status | Notes |
|------|--------|-------|
| 1.1 Initialize go.mod | NOT STARTED | |
| 1.2 Create directory structure | NOT STARTED | |
| 1.3 Create Makefile | NOT STARTED | |
| 1.4 Create version package | NOT STARTED | |
| 1.5 Create stub main.go files | NOT STARTED | |
| 1.6 Verify build works | NOT STARTED | |

---

## Detailed Tasks

### 1.1 Initialize Go Module

**Command:**
```bash
go mod init github.com/charliek/shed
```

**Expected Result:**
- `go.mod` file created with module path and Go version

### 1.2 Create Directory Structure

Create the following directories:
```
shed/
├── cmd/
│   ├── shed/
│   └── shed-server/
├── internal/
│   ├── api/
│   ├── config/
│   ├── docker/
│   ├── sshconfig/
│   ├── sshd/
│   └── version/
├── configs/
├── images/
│   └── shed-base/
└── scripts/
```

### 1.3 Create Makefile

**File**: `Makefile`

Must include targets:
- `build` - Build both binaries
- `build-cli` - Build shed CLI only
- `build-server` - Build shed-server only
- `test` - Run all tests
- `test-integration` - Run integration tests
- `release` - Cross-compile for all platforms
- `clean` - Remove build artifacts
- `dev-server` - Run server in dev mode
- `dev-cli` - Run CLI in dev mode

**Version injection via ldflags:**
```makefile
VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
LDFLAGS := -ldflags "-X github.com/charliek/shed/internal/version.Version=$(VERSION)"
```

### 1.4 Create Version Package

**File**: `internal/version/version.go`

```go
package version

var (
    Version   = "dev"
    GitCommit = "unknown"
    BuildDate = "unknown"
)
```

### 1.5 Create Stub Main Files

**File**: `cmd/shed/main.go`
```go
package main

import (
    "fmt"
    "github.com/charliek/shed/internal/version"
)

func main() {
    fmt.Printf("shed version %s\n", version.Version)
}
```

**File**: `cmd/shed-server/main.go`
```go
package main

import (
    "fmt"
    "github.com/charliek/shed/internal/version"
)

func main() {
    fmt.Printf("shed-server version %s\n", version.Version)
}
```

### 1.6 Verify Build

**Commands:**
```bash
make build
./bin/shed
./bin/shed-server
```

**Expected Output:**
Both binaries print their version string.

---

## Deliverables Checklist

- [ ] `go.mod` exists with correct module path
- [ ] All directories created
- [ ] `Makefile` with all required targets
- [ ] `internal/version/version.go` implemented
- [ ] `cmd/shed/main.go` stub created
- [ ] `cmd/shed-server/main.go` stub created
- [ ] `make build` succeeds
- [ ] Both binaries execute and print version

---

## Deviations & Notes

| Item | Description |
|------|-------------|
| | |

---

## Completion Criteria
- All deliverables checked off
- `make build` produces working binaries
- Update epic progress tracker to mark Phase 1 complete
