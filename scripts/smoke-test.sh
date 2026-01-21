#!/bin/bash
set -e

# Smoke test for Shed CLI
# Tests basic functionality: create, list, exec, stop, start, delete

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
REPO_ROOT="$(dirname "$SCRIPT_DIR")"

# Use local binaries if available
if [ -f "$REPO_ROOT/shed" ]; then
    SHED="$REPO_ROOT/shed"
    SHED_SERVER="$REPO_ROOT/shed-server"
else
    SHED="shed"
    SHED_SERVER="shed-server"
fi

# Generate unique test name
TEST_NAME="smoke-test-$(date +%s)-$$"
PASSED=0
FAILED=0
TESTS=()

# Colors for output
RED='\033[0;31m'
GREEN='\033[0;32m'
NC='\033[0m' # No Color

log_pass() {
    echo -e "${GREEN}✓ PASS${NC}: $1"
    TESTS+=("PASS: $1")
    ((PASSED++))
}

log_fail() {
    echo -e "${RED}✗ FAIL${NC}: $1"
    TESTS+=("FAIL: $1")
    ((FAILED++))
}

cleanup() {
    echo ""
    echo "Cleaning up..."
    "$SHED" delete "$TEST_NAME" --force 2>/dev/null || true
}

trap cleanup EXIT

echo "=========================================="
echo "Shed Smoke Test"
echo "=========================================="
echo "Test shed name: $TEST_NAME"
echo ""

# Check if server is running, start if not
echo "Checking server status..."
if ! "$SHED" list >/dev/null 2>&1; then
    echo "Starting shed-server..."
    "$SHED_SERVER" &
    SERVER_PID=$!
    sleep 2

    if ! "$SHED" list >/dev/null 2>&1; then
        echo "Error: Failed to start server"
        exit 1
    fi
    echo "Server started (PID: $SERVER_PID)"
fi

# Test 1: Create shed
echo ""
echo "Test 1: Create shed"
if "$SHED" create "$TEST_NAME" --image ubuntu:24.04 2>&1; then
    log_pass "Create shed"
else
    log_fail "Create shed"
fi

# Test 2: List sheds
echo ""
echo "Test 2: List sheds"
if "$SHED" list 2>&1 | grep -q "$TEST_NAME"; then
    log_pass "List sheds (shed visible)"
else
    log_fail "List sheds (shed not visible)"
fi

# Test 3: Exec command
echo ""
echo "Test 3: Exec command"
EXEC_OUTPUT=$("$SHED" exec "$TEST_NAME" -- echo "hello from shed" 2>&1)
if echo "$EXEC_OUTPUT" | grep -q "hello from shed"; then
    log_pass "Exec command"
else
    log_fail "Exec command"
    echo "  Output: $EXEC_OUTPUT"
fi

# Test 4: Stop shed
echo ""
echo "Test 4: Stop shed"
if "$SHED" stop "$TEST_NAME" 2>&1; then
    log_pass "Stop shed"
else
    log_fail "Stop shed"
fi

# Wait for stop
sleep 1

# Test 5: Verify stopped
echo ""
echo "Test 5: Verify shed is stopped"
STATUS=$("$SHED" list 2>&1)
if echo "$STATUS" | grep "$TEST_NAME" | grep -qi "stopped\|exited"; then
    log_pass "Shed is stopped"
else
    log_fail "Shed is not stopped"
    echo "  Status: $STATUS"
fi

# Test 6: Start shed
echo ""
echo "Test 6: Start shed"
if "$SHED" start "$TEST_NAME" 2>&1; then
    log_pass "Start shed"
else
    log_fail "Start shed"
fi

# Wait for start
sleep 1

# Test 7: Verify started
echo ""
echo "Test 7: Verify shed is running"
STATUS=$("$SHED" list 2>&1)
if echo "$STATUS" | grep "$TEST_NAME" | grep -qi "running"; then
    log_pass "Shed is running"
else
    log_fail "Shed is not running"
    echo "  Status: $STATUS"
fi

# Test 8: Delete shed
echo ""
echo "Test 8: Delete shed"
if "$SHED" delete "$TEST_NAME" --force 2>&1; then
    log_pass "Delete shed"
else
    log_fail "Delete shed"
fi

# Test 9: Verify deleted
echo ""
echo "Test 9: Verify shed is deleted"
if ! "$SHED" list 2>&1 | grep -q "$TEST_NAME"; then
    log_pass "Shed is deleted"
else
    log_fail "Shed still exists"
fi

# Summary
echo ""
echo "=========================================="
echo "Smoke Test Summary"
echo "=========================================="
for test in "${TESTS[@]}"; do
    echo "  $test"
done
echo ""
echo -e "Passed: ${GREEN}${PASSED}${NC}"
echo -e "Failed: ${RED}${FAILED}${NC}"
echo ""

if [ $FAILED -gt 0 ]; then
    echo -e "${RED}SMOKE TEST FAILED${NC}"
    exit 1
else
    echo -e "${GREEN}SMOKE TEST PASSED${NC}"
    exit 0
fi
