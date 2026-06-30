#!/usr/bin/env bash
# release-validation.sh — pre-release end-to-end validation.
#
# Drives the full shed image lifecycle on both backends before tagging
# a release:
#
#   1. Build the CLI + server binaries locally (Mac, VZ, arm64).
#   2. Rsync sources to mini2 (Linux, FC, amd64) and build there too.
#   3. On each machine: spin up a local registry:2 container, build all
#      three variants (base, extensions, full), push, wipe the images
#      store, pull back, validate the OCI layout with crane, create a
#      shed against each variant, exec a sanity command, delete.
#   4. Cross-validate: pull the arm64 manifest on mini2 and expect a
#      platform-mismatch refusal.
#   5. Emit dist/release-validation.json with the pass/fail summary.
#
# Required env:
#   MINI2_HOST     hostname or IP of the Linux/FC test box (default: mini2)
#   MINI2_USER     ssh user on mini2 (default: $USER)
#
# Optional env:
#   SKIP_REMOTE=1  skip mini2 steps (useful when iterating locally)
#   SKIP_BUILDS=1  skip image builds — only validate push/pull/boot of
#                  already-locally-published images
#
# This script intentionally does NOT push to ghcr.io. It runs against a
# local `registry:2` Docker container on each machine so we don't pollute
# the public registry. After the script exits the registries are torn down.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
REPO_ROOT="$(dirname "$SCRIPT_DIR")"
DIST_DIR="${REPO_ROOT}/dist"
REPORT_PATH="${DIST_DIR}/release-validation.json"

MINI2_HOST="${MINI2_HOST:-mini2}"
MINI2_USER="${MINI2_USER:-$USER}"
SKIP_REMOTE="${SKIP_REMOTE:-0}"
SKIP_BUILDS="${SKIP_BUILDS:-0}"

REGISTRY_NAME="shed-test-registry"
REGISTRY_PORT="5050"
LOCAL_REGISTRY="localhost:${REGISTRY_PORT}"
VARIANTS=("base" "extensions" "full")

RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[0;33m'
NC='\033[0m'

PASS_COUNT=0
FAIL_COUNT=0
declare -a RESULTS=()
declare -a STEPS=()

mkdir -p "${DIST_DIR}"

log()  { echo -e "${YELLOW}==>${NC} $*"; }
ok()   { echo -e "  ${GREEN}PASS${NC} $*"; PASS_COUNT=$((PASS_COUNT+1)); RESULTS+=("\"PASS: $*\""); }
fail() { echo -e "  ${RED}FAIL${NC} $*"; FAIL_COUNT=$((FAIL_COUNT+1)); RESULTS+=("\"FAIL: $*\""); }
step() { STEPS+=("\"$*\""); log "$*"; }

cleanup() {
    local exit_code=$?
    log "Cleanup: stopping local registry container ${REGISTRY_NAME}"
    docker rm -f "${REGISTRY_NAME}" >/dev/null 2>&1 || true
    if [[ "${SKIP_REMOTE}" != "1" ]]; then
        ssh "${MINI2_USER}@${MINI2_HOST}" "docker rm -f ${REGISTRY_NAME} >/dev/null 2>&1 || true" || true
    fi
    write_report "${exit_code}"
    exit "${exit_code}"
}
trap cleanup EXIT

write_report() {
    local exit_code="$1"
    local results_csv
    results_csv=$(IFS=,; echo "${RESULTS[*]}")
    local steps_csv
    steps_csv=$(IFS=,; echo "${STEPS[*]}")
    cat > "${REPORT_PATH}" <<EOF
{
  "passed": ${PASS_COUNT},
  "failed": ${FAIL_COUNT},
  "exit_code": ${exit_code},
  "steps": [${steps_csv:-}],
  "results": [${results_csv:-}]
}
EOF
    log "Report: ${REPORT_PATH}"
}

require_cmd() {
    if ! command -v "$1" >/dev/null 2>&1; then
        fail "missing required command: $1"
        return 1
    fi
}

run_local() { eval "$@"; }
run_remote() {
    if [[ "${SKIP_REMOTE}" == "1" ]]; then
        return 0
    fi
    ssh -o ConnectTimeout=5 "${MINI2_USER}@${MINI2_HOST}" "$@"
}

# -------------------------------------------------------------------- #
# Preflight                                                            #
# -------------------------------------------------------------------- #
step "Preflight"
require_cmd go
require_cmd docker
require_cmd ssh
require_cmd rsync
if ! command -v crane >/dev/null 2>&1; then
    log "crane not found locally — install with: brew install crane"
    fail "crane not installed (Mac)"
else
    ok "crane installed (local)"
fi
if [[ "${SKIP_REMOTE}" != "1" ]]; then
    if run_remote "command -v docker >/dev/null 2>&1"; then
        ok "docker present on mini2"
    else
        fail "docker missing on mini2"
    fi
    if run_remote "command -v crane >/dev/null 2>&1"; then
        ok "crane installed (mini2)"
    else
        log "crane missing on mini2 — installing via Go"
        run_remote "go install github.com/google/go-containerregistry/cmd/crane@latest" || fail "installing crane on mini2"
    fi
fi

# -------------------------------------------------------------------- #
# Build CLI                                                            #
# -------------------------------------------------------------------- #
step "Build CLI (local Mac)"
(cd "${REPO_ROOT}" && go build -o bin/shed ./cmd/shed && go build -o bin/shed-server ./cmd/shed-server) \
    && ok "local CLI build" \
    || fail "local CLI build"

if [[ "${SKIP_REMOTE}" != "1" ]]; then
    step "Sync sources to mini2"
    rsync -az --delete \
        --exclude .git --exclude bin --exclude dist --exclude '*.ext4' \
        "${REPO_ROOT}/" "${MINI2_USER}@${MINI2_HOST}:/tmp/shed-validation/" \
        && ok "rsync to mini2" \
        || fail "rsync to mini2"

    step "Build CLI (mini2)"
    run_remote "cd /tmp/shed-validation && go build -o bin/shed ./cmd/shed && go build -o bin/shed-server ./cmd/shed-server" \
        && ok "remote CLI build" \
        || fail "remote CLI build"
fi

# -------------------------------------------------------------------- #
# Local registry (Mac)                                                 #
# -------------------------------------------------------------------- #
step "Start local registry:2 on Mac"
docker rm -f "${REGISTRY_NAME}" >/dev/null 2>&1 || true
docker run -d --rm --name "${REGISTRY_NAME}" -p "${REGISTRY_PORT}:5000" registry:2 >/dev/null \
    && ok "registry:2 up on ${LOCAL_REGISTRY}" \
    || fail "starting registry"

# -------------------------------------------------------------------- #
# Build + push each variant on Mac                                     #
# -------------------------------------------------------------------- #
if [[ "${SKIP_BUILDS}" != "1" ]]; then
    # Content hash of the build context so a changed agent busts BuildKit's
    # bind-mount cache (#227); without it this pre-release gate could validate
    # a stale-cached agent. Same value across variants.
    vz_install_sha="$("${REPO_ROOT}/scripts/install-input-sha.sh" "${REPO_ROOT}/vz")"
    for variant in "${VARIANTS[@]}"; do
        step "Build shed-vz-${variant}"
        docker buildx build --platform linux/arm64 \
            --target "shed-vz-${variant}" \
            -t "${LOCAL_REGISTRY}/shed-vz-${variant}:test" \
            --build-arg "SHED_INSTALL_SHA=${vz_install_sha}" \
            --load -f "${REPO_ROOT}/vz/Dockerfile" "${REPO_ROOT}/vz" \
            && ok "build shed-vz-${variant}" \
            || { fail "build shed-vz-${variant}"; continue; }

        step "Push shed-vz-${variant} to local registry"
        docker push "${LOCAL_REGISTRY}/shed-vz-${variant}:test" >/dev/null \
            && ok "push shed-vz-${variant}" \
            || fail "push shed-vz-${variant}"
    done
fi

# -------------------------------------------------------------------- #
# crane validation                                                     #
# -------------------------------------------------------------------- #
step "crane validate against local registry (Mac)"
for variant in "${VARIANTS[@]}"; do
    if crane manifest "${LOCAL_REGISTRY}/shed-vz-${variant}:test" >/dev/null 2>&1; then
        ok "crane manifest ${variant}"
    else
        fail "crane manifest ${variant}"
    fi
done

# -------------------------------------------------------------------- #
# Pull + create + exec + delete (Mac, VZ)                              #
# -------------------------------------------------------------------- #
SHED="${REPO_ROOT}/bin/shed"

step "Wipe local images_dir + instances (VZ)"
rm -rf "${HOME}/Library/Application Support/shed/vz/blobs" \
       "${HOME}/Library/Application Support/shed/vz/cache" \
       "${HOME}/Library/Application Support/shed/vz/tags" \
       "${HOME}/Library/Application Support/shed/vz/instances" \
       "${HOME}/Library/Application Support/shed/vz/snapshots" \
       "${HOME}/Library/Application Support/shed/vz/uppers" 2>/dev/null || true
ok "wiped images_dir"

step "Pull + create + exec + delete each variant (VZ)"
for variant in "${VARIANTS[@]}"; do
    test_name="rv-${variant}-$$"
    if "${SHED}" image pull "${LOCAL_REGISTRY}/shed-vz-${variant}:test" -t "${variant}" >/dev/null 2>&1; then
        ok "pull ${variant}"
    else
        fail "pull ${variant}"; continue
    fi
    if "${SHED}" create "${test_name}" --image "${variant}" >/dev/null 2>&1; then
        ok "create ${test_name}"
    else
        fail "create ${test_name}"; continue
    fi
    if "${SHED}" exec "${test_name}" -- uname -a >/dev/null 2>&1; then
        ok "exec ${test_name}"
    else
        fail "exec ${test_name}"
    fi
    "${SHED}" delete "${test_name}" --force >/dev/null 2>&1 \
        && ok "delete ${test_name}" \
        || fail "delete ${test_name}"
done

# -------------------------------------------------------------------- #
# Remote validation on mini2 (Linux, FC, amd64)                        #
# -------------------------------------------------------------------- #
if [[ "${SKIP_REMOTE}" != "1" ]]; then
    step "Start local registry:2 on mini2"
    run_remote "docker rm -f ${REGISTRY_NAME} >/dev/null 2>&1 || true; docker run -d --rm --name ${REGISTRY_NAME} -p ${REGISTRY_PORT}:5000 registry:2 >/dev/null" \
        && ok "registry up on mini2:${REGISTRY_PORT}" \
        || fail "remote registry"

    if [[ "${SKIP_BUILDS}" != "1" ]]; then
        for variant in "${VARIANTS[@]}"; do
            step "Build shed-fc-${variant} on mini2"
            run_remote "cd /tmp/shed-validation && docker buildx build --platform linux/amd64 --target shed-fc-${variant} -t ${LOCAL_REGISTRY}/shed-fc-${variant}:test --build-arg SHED_INSTALL_SHA=\$(./scripts/install-input-sha.sh firecracker) --load -f firecracker/Dockerfile firecracker" \
                && ok "build shed-fc-${variant}" \
                || { fail "build shed-fc-${variant}"; continue; }
            run_remote "docker push ${LOCAL_REGISTRY}/shed-fc-${variant}:test >/dev/null" \
                && ok "push shed-fc-${variant}" \
                || fail "push shed-fc-${variant}"
        done
    fi

    step "Stop shed-server + wipe images_dir on mini2"
    run_remote "sudo systemctl stop shed-server; sudo rm -rf /var/lib/shed/firecracker/blobs /var/lib/shed/firecracker/cache /var/lib/shed/firecracker/tags /var/lib/shed/firecracker/instances /var/lib/shed/firecracker/snapshots /var/lib/shed/firecracker/uppers; sudo systemctl start shed-server" \
        && ok "mini2 shed-server restart with wiped images_dir" \
        || fail "mini2 wipe+restart"

    for variant in "${VARIANTS[@]}"; do
        step "Pull + create + exec + delete shed-fc-${variant} on mini2"
        test_name="rv-${variant}-$$"
        run_remote "/tmp/shed-validation/bin/shed image pull ${LOCAL_REGISTRY}/shed-fc-${variant}:test -t ${variant}" \
            && ok "remote pull ${variant}" \
            || { fail "remote pull ${variant}"; continue; }
        run_remote "/tmp/shed-validation/bin/shed create ${test_name} --image ${variant}" \
            && ok "remote create ${test_name}" \
            || { fail "remote create ${test_name}"; continue; }
        run_remote "/tmp/shed-validation/bin/shed exec ${test_name} -- uname -a" \
            && ok "remote exec ${test_name}" \
            || fail "remote exec ${test_name}"
        run_remote "/tmp/shed-validation/bin/shed delete ${test_name} --force" \
            && ok "remote delete ${test_name}" \
            || fail "remote delete ${test_name}"
    done

    # Cross-arch sanity: pulling an arm64 image on amd64 mini2 must
    # refuse with a platform mismatch. We push the Mac-built arm64
    # image directly to mini2's registry (the Mac flow above only
    # pushes to the Mac's registry) so the pull resolves to bytes that
    # actually exist, surfacing the real architecture error rather
    # than a generic "manifest unknown".
    step "Cross-arch refusal test (arm64 image on mini2)"
    if crane copy \
        "${LOCAL_REGISTRY}/shed-vz-base:test" \
        "${MINI2_USER}@${MINI2_HOST}/shed-vz-base:test" \
        --insecure 2>/dev/null; then
        : # rare; only used to seed the mini2 registry for this test
    fi
    if run_remote "/tmp/shed-validation/bin/shed image pull ${LOCAL_REGISTRY}/shed-vz-base:test -t crossarch --platform linux/arm64 2>&1 | grep -qi 'platform\\|arm64\\|architecture'"; then
        ok "cross-arch pull refused with clear platform error"
    else
        fail "cross-arch pull did not surface a platform mismatch"
    fi
fi

# -------------------------------------------------------------------- #
# Summary                                                              #
# -------------------------------------------------------------------- #
echo ""
echo "=========================================="
echo "Release validation summary"
echo "=========================================="
echo "Passed: ${PASS_COUNT}"
echo "Failed: ${FAIL_COUNT}"
echo ""
if [[ ${FAIL_COUNT} -gt 0 ]]; then
    echo -e "${RED}VALIDATION FAILED${NC}"
    exit 1
fi
echo -e "${GREEN}VALIDATION PASSED${NC}"
exit 0
