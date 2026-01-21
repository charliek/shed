# Phase 9: Supporting Files

## Overview
- **Phase**: 9 of 9
- **Epic**: [setup-epic.md](./setup-epic.md)
- **Status**: NOT STARTED
- **Estimated Effort**: Small-Medium
- **Dependencies**: Phase 6 (Server Binary), Phase 7 (CLI Commands)

## Objective
Create the Docker base image, build scripts, installation script, and smoke test script that support development, deployment, and testing of the Shed project.

## Prerequisites
- Phase 6 complete (shed-server binary functional)
- Phase 7 complete (shed CLI functional)
- Docker installed and running
- Access to GitHub for release API (install script)

## Context for New Engineers

### Docker Base Image
The `shed-base` image is the default environment for new sheds. It includes:
- Common development tools (git, curl, vim, etc.)
- Node.js runtime (required for Claude Code)
- AI coding assistants (Claude Code, OpenCode)
- GitHub CLI for repository operations

This image runs `sleep infinity` to stay alive, allowing console connections via `docker exec`.

### Scripts Overview
```
scripts/
  build-image.sh    # Builds shed-base Docker image
  install-cli.sh    # Downloads and installs shed CLI binary
  smoke-test.sh     # End-to-end test of shed functionality
```

### Image Naming
- **Image**: `shed-base:latest`
- **Tag Override**: Set `TAG` environment variable to use custom tag

---

## Progress Tracker

| Task | Status | Notes |
|------|--------|-------|
| 9.1 Create shed-base Dockerfile | NOT STARTED | |
| 9.2 Create build-image.sh script | NOT STARTED | |
| 9.3 Create install-cli.sh script | NOT STARTED | |
| 9.4 Create smoke-test.sh script | NOT STARTED | |
| 9.5 Test all scripts | NOT STARTED | |

---

## Detailed Tasks

### 9.1 Create Docker Base Image

**File**: `images/shed-base/Dockerfile`

This Dockerfile creates the default development environment for sheds.

```dockerfile
FROM ubuntu:24.04

LABEL maintainer="shed"
LABEL description="Base image for Shed development environments"

# Avoid interactive prompts during package installation
ENV DEBIAN_FRONTEND=noninteractive

# Install base development tools
RUN apt-get update && apt-get install -y \
    curl \
    git \
    tmux \
    vim \
    wget \
    jq \
    openssh-client \
    build-essential \
    ca-certificates \
    gnupg \
    && rm -rf /var/lib/apt/lists/*

# Install Node.js 22.x (required for Claude Code)
RUN curl -fsSL https://deb.nodesource.com/setup_22.x | bash - \
    && apt-get install -y nodejs \
    && rm -rf /var/lib/apt/lists/*

# Install Claude Code globally
RUN npm install -g @anthropic-ai/claude-code

# Install OpenCode
RUN curl -fsSL https://opencode.ai/install | bash

# Install GitHub CLI (gh)
RUN curl -fsSL https://cli.github.com/packages/githubcli-archive-keyring.gpg | dd of=/usr/share/keyrings/githubcli-archive-keyring.gpg \
    && chmod go+r /usr/share/keyrings/githubcli-archive-keyring.gpg \
    && echo "deb [arch=$(dpkg --print-architecture) signed-by=/usr/share/keyrings/githubcli-archive-keyring.gpg] https://cli.github.com/packages stable main" | tee /etc/apt/sources.list.d/github-cli.list > /dev/null \
    && apt-get update \
    && apt-get install -y gh \
    && rm -rf /var/lib/apt/lists/*

# Set working directory
WORKDIR /workspace

# Keep container running
CMD ["sleep", "infinity"]
```

**Implementation Notes**:
- Use `DEBIAN_FRONTEND=noninteractive` to prevent apt prompts
- Clean up apt lists after each RUN to reduce image size
- Node.js 22.x is LTS and required for Claude Code npm package
- OpenCode installer handles architecture detection automatically
- GitHub CLI installed from official repository for updates

### 9.2 Create Build Image Script

**File**: `scripts/build-image.sh`

```bash
#!/usr/bin/env bash
#
# Build the shed-base Docker image
#
# Usage:
#   ./scripts/build-image.sh              # Builds shed-base:latest
#   TAG=v1.0.0 ./scripts/build-image.sh   # Builds shed-base:v1.0.0
#

set -euo pipefail

# Configuration
IMAGE_NAME="shed-base"
TAG="${TAG:-latest}"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
DOCKERFILE_PATH="${PROJECT_ROOT}/images/shed-base/Dockerfile"

# Colors for output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[0;33m'
NC='\033[0m' # No Color

log_info() {
    echo -e "${GREEN}[INFO]${NC} $1"
}

log_warn() {
    echo -e "${YELLOW}[WARN]${NC} $1"
}

log_error() {
    echo -e "${RED}[ERROR]${NC} $1"
}

# Check prerequisites
if ! command -v docker &> /dev/null; then
    log_error "Docker is not installed or not in PATH"
    exit 1
fi

if [ ! -f "${DOCKERFILE_PATH}" ]; then
    log_error "Dockerfile not found at: ${DOCKERFILE_PATH}"
    exit 1
fi

# Build the image
log_info "Building ${IMAGE_NAME}:${TAG}..."
log_info "Dockerfile: ${DOCKERFILE_PATH}"

docker build \
    -t "${IMAGE_NAME}:${TAG}" \
    -f "${DOCKERFILE_PATH}" \
    "${PROJECT_ROOT}/images/shed-base"

if [ $? -eq 0 ]; then
    log_info "Successfully built ${IMAGE_NAME}:${TAG}"

    # Show image size
    SIZE=$(docker images "${IMAGE_NAME}:${TAG}" --format "{{.Size}}")
    log_info "Image size: ${SIZE}"
else
    log_error "Failed to build ${IMAGE_NAME}:${TAG}"
    exit 1
fi
```

**Make executable**:
```bash
chmod +x scripts/build-image.sh
```

### 9.3 Create Install CLI Script

**File**: `scripts/install-cli.sh`

```bash
#!/usr/bin/env bash
#
# Install the shed CLI binary
#
# Usage:
#   curl -fsSL https://raw.githubusercontent.com/charliek/shed/main/scripts/install-cli.sh | bash
#   ./scripts/install-cli.sh
#
# Environment variables:
#   SHED_VERSION    - Specific version to install (default: latest)
#   INSTALL_DIR     - Installation directory (default: /usr/local/bin)
#

set -euo pipefail

# Configuration
GITHUB_REPO="charliek/shed"
BINARY_NAME="shed"
INSTALL_DIR="${INSTALL_DIR:-/usr/local/bin}"
VERSION="${SHED_VERSION:-}"

# Colors for output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[0;33m'
NC='\033[0m' # No Color

log_info() {
    echo -e "${GREEN}[INFO]${NC} $1"
}

log_warn() {
    echo -e "${YELLOW}[WARN]${NC} $1"
}

log_error() {
    echo -e "${RED}[ERROR]${NC} $1"
}

# Detect OS
detect_os() {
    local os
    os="$(uname -s | tr '[:upper:]' '[:lower:]')"
    case "${os}" in
        linux)
            echo "linux"
            ;;
        darwin)
            echo "darwin"
            ;;
        *)
            log_error "Unsupported operating system: ${os}"
            exit 1
            ;;
    esac
}

# Detect architecture
detect_arch() {
    local arch
    arch="$(uname -m)"
    case "${arch}" in
        x86_64|amd64)
            echo "amd64"
            ;;
        aarch64|arm64)
            echo "arm64"
            ;;
        *)
            log_error "Unsupported architecture: ${arch}"
            exit 1
            ;;
    esac
}

# Get latest release version from GitHub API
get_latest_version() {
    local latest
    latest=$(curl -fsSL "https://api.github.com/repos/${GITHUB_REPO}/releases/latest" | grep '"tag_name":' | sed -E 's/.*"([^"]+)".*/\1/')

    if [ -z "${latest}" ]; then
        log_error "Failed to fetch latest version from GitHub"
        exit 1
    fi

    echo "${latest}"
}

# Download and install binary
install_binary() {
    local os="$1"
    local arch="$2"
    local version="$3"

    # Construct download URL
    # Expected format: shed_<version>_<os>_<arch>.tar.gz
    local filename="shed_${version#v}_${os}_${arch}.tar.gz"
    local download_url="https://github.com/${GITHUB_REPO}/releases/download/${version}/${filename}"

    log_info "Downloading ${filename}..."

    # Create temp directory
    local tmp_dir
    tmp_dir=$(mktemp -d)
    trap "rm -rf ${tmp_dir}" EXIT

    # Download archive
    if ! curl -fsSL "${download_url}" -o "${tmp_dir}/${filename}"; then
        log_error "Failed to download ${download_url}"
        exit 1
    fi

    # Extract binary
    log_info "Extracting ${filename}..."
    tar -xzf "${tmp_dir}/${filename}" -C "${tmp_dir}"

    # Find the binary (might be in root or in a subdirectory)
    local binary_path
    binary_path=$(find "${tmp_dir}" -name "${BINARY_NAME}" -type f | head -1)

    if [ -z "${binary_path}" ]; then
        log_error "Binary '${BINARY_NAME}' not found in archive"
        exit 1
    fi

    # Install binary
    log_info "Installing to ${INSTALL_DIR}/${BINARY_NAME}..."

    # Check if we need sudo
    if [ -w "${INSTALL_DIR}" ]; then
        mv "${binary_path}" "${INSTALL_DIR}/${BINARY_NAME}"
        chmod +x "${INSTALL_DIR}/${BINARY_NAME}"
    else
        log_warn "Elevated permissions required to install to ${INSTALL_DIR}"
        sudo mv "${binary_path}" "${INSTALL_DIR}/${BINARY_NAME}"
        sudo chmod +x "${INSTALL_DIR}/${BINARY_NAME}"
    fi
}

# Verify installation
verify_installation() {
    if command -v "${BINARY_NAME}" &> /dev/null; then
        local installed_version
        installed_version=$("${BINARY_NAME}" version 2>/dev/null || echo "unknown")
        log_info "Successfully installed ${BINARY_NAME} (${installed_version})"
        return 0
    else
        log_error "Installation verification failed"
        return 1
    fi
}

# Main
main() {
    log_info "Installing shed CLI..."

    # Check for curl
    if ! command -v curl &> /dev/null; then
        log_error "curl is required but not installed"
        exit 1
    fi

    # Detect platform
    local os
    local arch
    os=$(detect_os)
    arch=$(detect_arch)
    log_info "Detected platform: ${os}/${arch}"

    # Determine version
    if [ -z "${VERSION}" ]; then
        log_info "Fetching latest version..."
        VERSION=$(get_latest_version)
    fi
    log_info "Version: ${VERSION}"

    # Install
    install_binary "${os}" "${arch}" "${VERSION}"

    # Verify
    verify_installation

    log_info "Installation complete!"
    log_info "Run 'shed --help' to get started"
}

main "$@"
```

**Make executable**:
```bash
chmod +x scripts/install-cli.sh
```

### 9.4 Create Smoke Test Script

**File**: `scripts/smoke-test.sh`

```bash
#!/usr/bin/env bash
#
# Smoke test for shed functionality
#
# This script tests the core shed operations:
# - Creating a shed
# - Listing sheds
# - Executing commands in a shed
# - Stopping and starting a shed
# - Deleting a shed
#
# Prerequisites:
# - shed CLI installed
# - shed-server running (or will be started)
# - shed-base image available
#
# Usage:
#   ./scripts/smoke-test.sh
#

set -euo pipefail

# Configuration
TEST_SHED_NAME="smoke-test-$$"  # Use PID for uniqueness
SERVER_URL="${SHED_SERVER_URL:-http://localhost:8080}"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# Colors for output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[0;33m'
BLUE='\033[0;34m'
NC='\033[0m' # No Color

# Test counters
TESTS_PASSED=0
TESTS_FAILED=0

log_info() {
    echo -e "${GREEN}[INFO]${NC} $1"
}

log_warn() {
    echo -e "${YELLOW}[WARN]${NC} $1"
}

log_error() {
    echo -e "${RED}[ERROR]${NC} $1"
}

log_test() {
    echo -e "${BLUE}[TEST]${NC} $1"
}

# Run a test and track result
run_test() {
    local test_name="$1"
    local test_cmd="$2"

    log_test "Running: ${test_name}"

    if eval "${test_cmd}"; then
        log_info "PASSED: ${test_name}"
        ((TESTS_PASSED++))
        return 0
    else
        log_error "FAILED: ${test_name}"
        ((TESTS_FAILED++))
        return 1
    fi
}

# Cleanup function
cleanup() {
    log_info "Cleaning up test shed..."
    shed delete "${TEST_SHED_NAME}" --force 2>/dev/null || true
}

# Check prerequisites
check_prerequisites() {
    log_info "Checking prerequisites..."

    # Check shed CLI
    if ! command -v shed &> /dev/null; then
        log_error "shed CLI not found in PATH"
        exit 1
    fi
    log_info "shed CLI found: $(which shed)"

    # Check Docker
    if ! command -v docker &> /dev/null; then
        log_error "Docker not found in PATH"
        exit 1
    fi
    log_info "Docker found: $(which docker)"

    # Check if shed-base image exists
    if ! docker image inspect shed-base:latest &> /dev/null; then
        log_warn "shed-base:latest image not found, building..."
        "${SCRIPT_DIR}/build-image.sh"
    fi
    log_info "shed-base:latest image available"
}

# Check if server is running, start if not
check_server() {
    log_info "Checking server status..."

    if curl -sf "${SERVER_URL}/api/info" > /dev/null 2>&1; then
        log_info "Server is running at ${SERVER_URL}"
        return 0
    fi

    log_warn "Server not responding at ${SERVER_URL}"
    log_info "Attempting to start server..."

    # Try to start the server in background
    if command -v shed-server &> /dev/null; then
        shed-server &
        SERVER_PID=$!
        sleep 2  # Give server time to start

        if curl -sf "${SERVER_URL}/api/info" > /dev/null 2>&1; then
            log_info "Server started successfully (PID: ${SERVER_PID})"
            return 0
        else
            log_error "Failed to start server"
            return 1
        fi
    else
        log_error "shed-server not found in PATH"
        return 1
    fi
}

# Test: Create a shed
test_create_shed() {
    shed create "${TEST_SHED_NAME}"
}

# Test: List sheds and verify test shed exists
test_list_sheds() {
    shed list | grep -q "${TEST_SHED_NAME}"
}

# Test: Execute command in shed
test_exec_command() {
    local output
    output=$(shed exec "${TEST_SHED_NAME}" -- echo "hello from shed")
    [ "${output}" = "hello from shed" ]
}

# Test: Stop shed
test_stop_shed() {
    shed stop "${TEST_SHED_NAME}"
    # Verify shed is stopped
    local status
    status=$(shed list --json | jq -r ".[] | select(.name == \"${TEST_SHED_NAME}\") | .status")
    [ "${status}" = "stopped" ]
}

# Test: Start shed
test_start_shed() {
    shed start "${TEST_SHED_NAME}"
    # Verify shed is running
    local status
    status=$(shed list --json | jq -r ".[] | select(.name == \"${TEST_SHED_NAME}\") | .status")
    [ "${status}" = "running" ]
}

# Test: Delete shed
test_delete_shed() {
    shed delete "${TEST_SHED_NAME}" --force
    # Verify shed is gone
    ! shed list | grep -q "${TEST_SHED_NAME}"
}

# Print test summary
print_summary() {
    echo ""
    echo "============================================"
    echo "           SMOKE TEST SUMMARY"
    echo "============================================"
    echo -e "Tests passed: ${GREEN}${TESTS_PASSED}${NC}"
    echo -e "Tests failed: ${RED}${TESTS_FAILED}${NC}"
    echo "============================================"

    if [ "${TESTS_FAILED}" -eq 0 ]; then
        log_info "All smoke tests passed!"
        return 0
    else
        log_error "Some smoke tests failed"
        return 1
    fi
}

# Main
main() {
    echo ""
    echo "============================================"
    echo "        SHED SMOKE TEST"
    echo "============================================"
    echo "Test shed name: ${TEST_SHED_NAME}"
    echo "Server URL: ${SERVER_URL}"
    echo "============================================"
    echo ""

    # Set up cleanup trap
    trap cleanup EXIT

    # Prerequisites
    check_prerequisites
    check_server

    echo ""
    log_info "Starting smoke tests..."
    echo ""

    # Run tests in sequence
    run_test "Create shed" "test_create_shed"
    run_test "List sheds" "test_list_sheds"
    run_test "Execute command" "test_exec_command"
    run_test "Stop shed" "test_stop_shed"
    run_test "Start shed" "test_start_shed"
    run_test "Delete shed" "test_delete_shed"

    # Print summary
    print_summary
}

main "$@"
```

**Make executable**:
```bash
chmod +x scripts/smoke-test.sh
```

### 9.5 Test All Scripts

**Testing build-image.sh**:
```bash
# Build with default tag
./scripts/build-image.sh

# Build with custom tag
TAG=v1.0.0-test ./scripts/build-image.sh

# Verify image exists
docker images shed-base
```

**Testing install-cli.sh** (after creating a release):
```bash
# Test detection functions
./scripts/install-cli.sh
# Should detect OS/arch and attempt download

# Test with specific version
SHED_VERSION=v1.0.0 ./scripts/install-cli.sh
```

**Testing smoke-test.sh**:
```bash
# Ensure server is running first
./scripts/smoke-test.sh

# Check exit code
echo $?  # Should be 0 for success
```

---

## Deliverables Checklist

- [ ] `images/shed-base/Dockerfile` created
- [ ] Dockerfile includes all required packages (curl, git, tmux, vim, wget, jq, openssh-client, build-essential, ca-certificates)
- [ ] Dockerfile installs Node.js 22.x
- [ ] Dockerfile installs Claude Code via npm
- [ ] Dockerfile installs OpenCode via curl installer
- [ ] Dockerfile installs GitHub CLI (gh)
- [ ] Dockerfile sets WORKDIR to /workspace
- [ ] Dockerfile CMD is ["sleep", "infinity"]
- [ ] `scripts/build-image.sh` created and executable
- [ ] build-image.sh accepts TAG environment variable
- [ ] `scripts/install-cli.sh` created and executable
- [ ] install-cli.sh detects OS (linux, darwin)
- [ ] install-cli.sh detects architecture (amd64, arm64)
- [ ] install-cli.sh fetches latest release from GitHub API
- [ ] install-cli.sh installs to /usr/local/bin by default
- [ ] `scripts/smoke-test.sh` created and executable
- [ ] smoke-test.sh tests create shed
- [ ] smoke-test.sh tests list sheds
- [ ] smoke-test.sh tests exec command
- [ ] smoke-test.sh tests stop/start shed
- [ ] smoke-test.sh tests delete shed
- [ ] smoke-test.sh prints success/failure summary
- [ ] All scripts have proper shebang lines (#!/usr/bin/env bash)
- [ ] All scripts use set -euo pipefail for safety
- [ ] Docker image builds successfully
- [ ] Smoke test passes end-to-end

---

## Directory Structure

After completing this phase, the following files should exist:

```
shed/
  images/
    shed-base/
      Dockerfile
  scripts/
    build-image.sh
    install-cli.sh
    smoke-test.sh
```

---

## Deviations & Notes

| Item | Description |
|------|-------------|
| | |

---

## Completion Criteria
- All deliverables checked off
- `./scripts/build-image.sh` builds image without errors
- Docker image includes all required tools (verify with `docker run --rm shed-base:latest which claude`)
- `./scripts/smoke-test.sh` passes all tests
- Scripts work on both Linux and macOS (where applicable)
- Update epic progress tracker to mark Phase 9 complete
- Update epic verification checklist items related to Phase 9
