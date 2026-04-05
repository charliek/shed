#!/bin/bash
# Note: no set -e — this script checks exit codes explicitly via log_pass/log_fail

# End-to-end test for Shed CLI
# Tests full lifecycle with a real repo, provisioning, services, and stop/start cycle.
# Requires a running shed-server and configured backends.
#
# Usage:
#   ./scripts/e2e-test.sh [--backend firecracker|vz] [--repo URL] [--timeout MINUTES]
#
# Defaults: --backend firecracker --repo git@github.com:charliek/sltstodo.git --timeout 25

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
REPO_ROOT="$(dirname "$SCRIPT_DIR")"

# Use local binaries if available
if [ -f "$REPO_ROOT/bin/shed" ]; then
    SHED="$REPO_ROOT/bin/shed"
elif [ -f "$REPO_ROOT/shed" ]; then
    SHED="$REPO_ROOT/shed"
else
    SHED="shed"
fi

# Defaults
BACKEND="firecracker"
REPO="git@github.com:charliek/sltstodo.git"
TIMEOUT_MINUTES=25
TIMESTAMP=$(date +%s)

# Parse arguments
while [[ $# -gt 0 ]]; do
    case $1 in
        --backend)
            if [[ -z "${2:-}" || "$2" == --* ]]; then
                echo "Error: --backend requires a value (firecracker or vz)"
                exit 1
            fi
            BACKEND="$2"
            shift 2
            ;;
        --repo)
            if [[ -z "${2:-}" || "$2" == --* ]]; then
                echo "Error: --repo requires a URL value"
                exit 1
            fi
            REPO="$2"
            shift 2
            ;;
        --timeout)
            if [[ -z "${2:-}" || "$2" == --* ]]; then
                echo "Error: --timeout requires a numeric value (minutes)"
                exit 1
            fi
            TIMEOUT_MINUTES="$2"
            shift 2
            ;;
        -h|--help)
            echo "Usage: $0 [--backend firecracker|vz] [--repo URL] [--timeout MINUTES]"
            exit 0
            ;;
        *)
            echo "Unknown argument: $1"
            exit 1
            ;;
    esac
done

# Colors for output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[0;33m'
NC='\033[0m'

# Test tracking
TOTAL_PASSED=0
TOTAL_FAILED=0
ALL_TESTS=()
CREATED_SHEDS=()

log_pass() {
    echo -e "  ${GREEN}PASS${NC}: $1"
    ALL_TESTS+=("PASS [$CURRENT_BACKEND]: $1")
    TOTAL_PASSED=$((TOTAL_PASSED + 1))
}

log_fail() {
    echo -e "  ${RED}FAIL${NC}: $1"
    if [ -n "${2:-}" ]; then
        echo "    Output: $2"
    fi
    ALL_TESTS+=("FAIL [$CURRENT_BACKEND]: $1")
    TOTAL_FAILED=$((TOTAL_FAILED + 1))
}

log_skip() {
    echo -e "  ${YELLOW}SKIP${NC}: $1"
    ALL_TESTS+=("SKIP [$CURRENT_BACKEND]: $1")
}

cleanup() {
    echo ""
    echo "Cleaning up..."
    for name in "${CREATED_SHEDS[@]}"; do
        "$SHED" delete "$name" --force 2>/dev/null || true
    done
}

trap cleanup EXIT

# Pre-flight checks
echo "=========================================="
echo "Shed E2E Test"
echo "=========================================="
echo "Backend:  $BACKEND"
echo "Repo:     $REPO"
echo "Timeout:  ${TIMEOUT_MINUTES}m"
echo ""

# Check server is running
echo "Checking server status..."
if ! "$SHED" list >/dev/null 2>&1; then
    echo -e "${RED}Error: shed-server is not running. Start it before running e2e tests.${NC}"
    exit 1
fi
echo "Server is running."

# Check SSH known_hosts
if [ ! -f "$HOME/.shed/known_hosts" ]; then
    echo -e "${YELLOW}Warning: ~/.shed/known_hosts does not exist. First SSH connection may prompt.${NC}"
fi

echo ""

# Per-backend test function
run_backend_tests() {
    local backend_name="$1"
    local test_name="e2e-${backend_name}-${TIMESTAMP}"
    CURRENT_BACKEND="$backend_name"
    CREATED_SHEDS+=("$test_name")

    echo "=========================================="
    echo "Testing backend: $backend_name"
    echo "Shed name: $test_name"
    echo "=========================================="

    # 1. CREATE
    echo ""
    echo "Step 1: Create shed with repo"
    local create_output
    if create_output=$(timeout $((TIMEOUT_MINUTES * 60)) "$SHED" create "$test_name" --repo "$REPO" --backend "$backend_name" --timeout "${TIMEOUT_MINUTES}m" 2>&1); then
        log_pass "Create shed"
    else
        log_fail "Create shed" "$create_output"
        echo -e "${RED}Cannot continue tests for $backend_name without a running shed.${NC}"
        return
    fi

    # Verify in list
    if "$SHED" list 2>&1 | grep -q "$test_name"; then
        log_pass "Shed visible in list"
    else
        log_fail "Shed visible in list"
    fi

    # 2. EXEC BASIC
    echo ""
    echo "Step 2: Basic exec"
    local exec_output
    exec_output=$("$SHED" exec "$test_name" -- echo hello 2>&1)
    if echo "$exec_output" | grep -q "hello"; then
        log_pass "Basic exec (echo hello)"
    else
        log_fail "Basic exec (echo hello)" "$exec_output"
    fi

    # 2b. NON-ROOT USER
    echo ""
    echo "Step 2b: Verify non-root user context"

    # whoami should return shed
    local whoami_output
    whoami_output=$("$SHED" exec "$test_name" -- whoami 2>&1)
    if echo "$whoami_output" | grep -q "shed"; then
        log_pass "Commands run as shed user"
    else
        log_fail "Commands run as shed user" "$whoami_output"
    fi

    # /workspace should be owned by shed
    local owner_output
    owner_output=$("$SHED" exec "$test_name" -- stat -c '%U' /workspace 2>&1)
    if echo "$owner_output" | grep -q "shed"; then
        log_pass "Workspace owned by shed"
    else
        log_fail "Workspace owned by shed" "$owner_output"
    fi

    # Passwordless sudo should work
    local sudo_output
    sudo_output=$("$SHED" exec "$test_name" -- sudo -n whoami 2>&1)
    if echo "$sudo_output" | grep -q "root"; then
        log_pass "Passwordless sudo works"
    else
        log_fail "Passwordless sudo works" "$sudo_output"
    fi

    # 3. SERVICES: Verify provisioning worked
    echo ""
    echo "Step 3: Verify provisioned services"

    # PostgreSQL
    local pg_output
    pg_output=$("$SHED" exec "$test_name" -- pg_isready 2>&1)
    if [ $? -eq 0 ]; then
        log_pass "PostgreSQL is ready"
    else
        log_fail "PostgreSQL is ready" "$pg_output"
    fi

    # Redis
    local redis_output
    redis_output=$("$SHED" exec "$test_name" -- redis-cli ping 2>&1)
    if echo "$redis_output" | grep -qi "PONG"; then
        log_pass "Redis is ready"
    else
        log_fail "Redis is ready" "$redis_output"
    fi

    # Bun
    local bun_output
    bun_output=$("$SHED" exec "$test_name" -- bash -c "command -v bun || ls \$HOME/.bun/bin/bun" 2>&1)
    if [ $? -eq 0 ]; then
        log_pass "Bun is installed"
    else
        log_fail "Bun is installed" "$bun_output"
    fi

    # 4. DB SCHEMA
    echo ""
    echo "Step 4: Push database schema"
    local db_output
    db_output=$("$SHED" exec "$test_name" -- bash -c "export PATH=\$HOME/.bun/bin:\$PATH && cd /workspace/apps/api && DATABASE_URL=postgresql://dev:dev@localhost:5432/sltstodo bun run db:push:ci" 2>&1)
    if [ $? -eq 0 ]; then
        log_pass "Database schema push"
    else
        log_fail "Database schema push" "$db_output"
    fi

    # 5. TESTS
    echo ""
    echo "Step 5: Run test suite"
    local test_output
    test_output=$("$SHED" exec "$test_name" -- bash -c "set -o pipefail && export PATH=\$HOME/.bun/bin:\$PATH && cd /workspace && DATABASE_URL=postgresql://dev:dev@localhost:5432/sltstodo REDIS_URL=redis://localhost:6379 bun test 2>&1 | tail -20" 2>&1)
    if [ $? -eq 0 ]; then
        log_pass "Test suite"
    else
        log_fail "Test suite" "$test_output"
    fi

    # 6. STOP
    echo ""
    echo "Step 6: Stop shed"
    local stop_output
    if stop_output=$("$SHED" stop "$test_name" 2>&1); then
        log_pass "Stop shed"
    else
        log_fail "Stop shed" "$stop_output"
    fi

    sleep 2

    # Verify stopped
    local status_output
    status_output=$("$SHED" list 2>&1)
    if echo "$status_output" | grep "$test_name" | grep -qi "stopped\|exited"; then
        log_pass "Shed is stopped"
    else
        log_fail "Shed is stopped" "$status_output"
    fi

    # 7. START
    echo ""
    echo "Step 7: Start shed"
    local start_output
    if start_output=$(timeout $((TIMEOUT_MINUTES * 60)) "$SHED" start "$test_name" --timeout 10m 2>&1); then
        log_pass "Start shed"
    else
        log_fail "Start shed" "$start_output"
        echo -e "${RED}Cannot continue post-restart tests for $backend_name.${NC}"
        return
    fi

    # Verify running
    status_output=$("$SHED" list 2>&1)
    if echo "$status_output" | grep "$test_name" | grep -qi "running"; then
        log_pass "Shed is running after restart"
    else
        log_fail "Shed is running after restart" "$status_output"
    fi

    # 8. SERVICES AFTER RESTART
    echo ""
    echo "Step 8: Verify services after restart"

    pg_output=$("$SHED" exec "$test_name" -- pg_isready 2>&1)
    if [ $? -eq 0 ]; then
        log_pass "PostgreSQL after restart"
    else
        log_fail "PostgreSQL after restart" "$pg_output"
    fi

    redis_output=$("$SHED" exec "$test_name" -- redis-cli ping 2>&1)
    if echo "$redis_output" | grep -qi "PONG"; then
        log_pass "Redis after restart"
    else
        log_fail "Redis after restart" "$redis_output"
    fi

    # 9. TESTS AFTER RESTART
    echo ""
    echo "Step 9: Run tests after restart"
    test_output=$("$SHED" exec "$test_name" -- bash -c "set -o pipefail && export PATH=\$HOME/.bun/bin:\$PATH && cd /workspace && DATABASE_URL=postgresql://dev:dev@localhost:5432/sltstodo REDIS_URL=redis://localhost:6379 bun test 2>&1 | tail -20" 2>&1)
    if [ $? -eq 0 ]; then
        log_pass "Test suite after restart"
    else
        log_fail "Test suite after restart" "$test_output"
    fi

    # 10. DELETE
    echo ""
    echo "Step 10: Delete shed"
    if "$SHED" delete "$test_name" --force 2>&1; then
        log_pass "Delete shed"
    else
        log_fail "Delete shed"
    fi

    # Verify deleted
    if ! "$SHED" list 2>&1 | grep -q "$test_name"; then
        log_pass "Shed deleted from list"
    else
        log_fail "Shed deleted from list"
    fi

    echo ""
}

# Run tests for selected backends
case "$BACKEND" in
    firecracker)
        run_backend_tests firecracker
        ;;
    vz)
        run_backend_tests vz
        ;;
    *)
        echo "Unknown backend: $BACKEND (use firecracker or vz)"
        exit 1
        ;;
esac

# Summary
echo "=========================================="
echo "E2E Test Summary"
echo "=========================================="
for test in "${ALL_TESTS[@]}"; do
    if [[ "$test" == PASS* ]]; then
        echo -e "  ${GREEN}$test${NC}"
    elif [[ "$test" == FAIL* ]]; then
        echo -e "  ${RED}$test${NC}"
    else
        echo -e "  ${YELLOW}$test${NC}"
    fi
done
echo ""
echo -e "Passed: ${GREEN}${TOTAL_PASSED}${NC}"
echo -e "Failed: ${RED}${TOTAL_FAILED}${NC}"
echo ""

if [ $TOTAL_FAILED -gt 0 ]; then
    echo -e "${RED}E2E TEST FAILED${NC}"
    exit 1
else
    echo -e "${GREEN}E2E TEST PASSED${NC}"
    exit 0
fi
