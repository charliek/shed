#!/usr/bin/env bash
# publish-images-local.sh
#
# Run the .github/workflows/publish-images.yaml VZ job's flow locally,
# inside a shed development VM, against a local registry:2 container.
#
# Why this exists:
#   The publish workflow only runs on tag push to GitHub. When the build
#   regresses (wrong --platform, stale --source-ref annotation, layer
#   ingest changes, etc.) the only feedback is a 30+ minute CI failure.
#   This script reproduces the same flow on an Apple Silicon Mac in a
#   shed, so changes can be validated end-to-end in a few minutes.
#
# How to use:
#   1. Boot a validation shed on a Mac with the 'full' image and a
#      generous upper layer (the publish flow needs room for a buildx
#      builder, a registry:2 cache, and ~3 GB of intermediate rootfs
#      layers per variant):
#        shed create publish-test --image full --cpus 4 --memory 8192 \
#                                 --upper-size 40G
#
#   2. Open an SSH session to the shed:
#        shed ssh-config publish-test > /tmp/publish-test-ssh
#        ssh -F /tmp/publish-test-ssh shed-publish-test
#
#   3. Inside the shed, neutralise the docker credential helper. The
#      shed-agent installs a credsStore that talks to the host over
#      vsock; inside a validation shed there is no host to answer, so
#      docker pull/push hangs:
#        echo '{}' > "$HOME/.docker/config.json"
#
#   4. Cross-build the in-VM binaries on the host (or get them onto the
#      shed however you like) and copy the project tree in. The guest
#      extension binaries + /etc overlay are staged by the shared
#      scripts/stage-guest-binaries.sh (into vz/shed-ext-*, vz/docker-
#      credential-shed, vz/ext-etc/), the same script the rootfs builds
#      and the publish workflow use:
#        GOOS=linux GOARCH=arm64 go build -o vz/shed-agent     ./cmd/shed-agent
#        GOOS=linux GOARCH=arm64 go build -o vz/shed-firstboot ./cmd/shed-firstboot
#        ./scripts/stage-guest-binaries.sh vz arm64
#        # On the host, scp the project tree and the linux/arm64 shed CLI:
#        scp -F /tmp/publish-test-ssh -r . shed-publish-test:/home/shed/work
#        GOOS=linux GOARCH=arm64 go build -o /tmp/shed ./cmd/shed
#        scp -F /tmp/publish-test-ssh /tmp/shed shed-publish-test:/tmp/shed
#        ssh -F /tmp/publish-test-ssh shed-publish-test \
#            'sudo install -m 0755 /tmp/shed /usr/local/bin/shed'
#
#   5. Run this script from the project root inside the shed:
#        ssh -F /tmp/publish-test-ssh shed-publish-test \
#            'cd /home/shed/work && ./scripts/publish-images-local.sh'
#
#      Inputs (override via env):
#        VERSION       (default: v0.0.0-local)
#        REGISTRY_HOST (default: localhost:5050)
#        VARIANTS      (default: "base extensions full")
#        WORK_DIR      (default: current dir; must contain vz/ initramfs/
#                       scripts/build-initramfs.sh)
#        STORE_DIR     (default: /tmp/publish-images-local-store)
#        OUT_DIR       (default: /tmp/publish-images-local)
#        SHED_BIN      (default: /usr/local/bin/shed)
#        BACKEND       (default: vz; pass "firecracker" on a Linux/KVM
#                       host to exercise the FC publish path instead)
#
# Behavior:
#   * Creates a docker buildx builder with --driver-opt network=host so
#     BuildKit can reach the in-shed registry over loopback. The default
#     docker-container driver runs in its own netns and cannot.
#   * Starts a registry:2 container with --network host on :5050.
#   * Builds the shed-overlay initramfs via scripts/build-initramfs.sh.
#   * For each variant (base, extensions, full):
#       - shed image build  (writes OCI layout + manifest with
#                            io.shed.source-ref annotation)
#       - shed image push   (streams the OCI layout to the registry)
#       - asserts the manifest in the registry carries the expected
#         source-ref annotation (the bug PR #94 fixed).
#   * Round-trips the first variant through `shed image pull` to a
#     fresh store and re-checks the source-ref annotation, mirroring
#     what a remote shed-server's resolveImage cache check does.
#
# Exit codes:
#   0  PASS - every variant built, pushed, and re-asserted with the
#      expected io.shed.source-ref annotation.
#   1  FAIL - one of the assertions tripped; the log line preceding
#      "FAIL:" identifies which variant and which check.
#
# See: docs/development/in-shed-build-debugging.md for the rationale
# behind --driver-opt network=host and how to interpret a failure.
set -euo pipefail

VERSION="${VERSION:-v0.0.0-local}"
REGISTRY_HOST="${REGISTRY_HOST:-localhost:5050}"
VARIANTS="${VARIANTS:-base extensions full}"
WORK_DIR="${WORK_DIR:-$(pwd)}"
STORE_DIR="${STORE_DIR:-/tmp/publish-images-local-store}"
OUT_DIR="${OUT_DIR:-/tmp/publish-images-local}"
SHED_BIN="${SHED_BIN:-/usr/local/bin/shed}"
BACKEND="${BACKEND:-vz}"

case "${BACKEND}" in
  vz)
    DEFAULT_PLATFORM="linux/arm64"
    BACKEND_PREFIX="shed-vz"
    BACKEND_DIR="vz"
    ;;
  firecracker)
    DEFAULT_PLATFORM="linux/amd64"
    BACKEND_PREFIX="shed-fc"
    BACKEND_DIR="firecracker"
    ;;
  *)
    echo "FAIL: unknown BACKEND='${BACKEND}' (want 'vz' or 'firecracker')" >&2
    exit 1
    ;;
esac
PLATFORM="${PLATFORM:-${DEFAULT_PLATFORM}}"

BUILDER_NAME="publish-images-local"
REGISTRY_CONTAINER="publish-images-local-registry"
INITRAMFS="${OUT_DIR}/shed-initrd.img"
SERVER_CFG="${OUT_DIR}/server.yaml"
SERVER_CFG_RT="${OUT_DIR}/server-rt.yaml"
STORE_RT="${STORE_DIR}-rt"

log()  { printf '\n=== %s ===\n' "$*"; }
fail() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }

write_server_yaml() {
  # Minimal shed-server config so `shed image push --local` /
  # `shed image pull` know where the OCI store lives.
  local path="$1"
  local images_dir="$2"
  case "${BACKEND}" in
    vz)
      cat > "${path}" <<YAML
name: publish-images-local
http_port: 0
ssh_port: 0
default_backend: vz
vz:
  vfkit_path: vfkit
  images_dir: ${images_dir}
  instance_dir: ${images_dir}/instances
  socket_dir: ${images_dir}/sockets
  default_cpus: 2
  default_memory_mb: 1024
  default_disk_gb: 1
  console_port: 1024
  notify_port: 1026
  tcp_proxy_port: 1028
  start_timeout: 60s
  stop_timeout: 10s
YAML
      ;;
    firecracker)
      cat > "${path}" <<YAML
name: publish-images-local
http_port: 0
ssh_port: 0
default_backend: firecracker
firecracker:
  images_dir: ${images_dir}
  instance_dir: ${images_dir}/instances
  socket_dir: ${images_dir}/sockets
  default_cpus: 2
  default_memory_mb: 1024
  default_disk_gb: 1
  console_port: 1024
  notify_port: 1026
  vsock_base_cid: 100
  bridge_name: shed-br0
  bridge_cidr: 172.30.0.1/24
  tap_prefix: shed-tap
  start_timeout: 60s
  stop_timeout: 10s
YAML
      ;;
  esac
}

cleanup() {
  log "cleanup"
  docker rm -f "${REGISTRY_CONTAINER}" >/dev/null 2>&1 || true
  docker buildx rm "${BUILDER_NAME}" >/dev/null 2>&1 || true
}
trap cleanup EXIT

log "pre-flight"
command -v docker >/dev/null || fail "docker missing"
command -v "${SHED_BIN}" >/dev/null || fail "shed binary missing at ${SHED_BIN}"
[ -d "${WORK_DIR}/${BACKEND_DIR}" ]              || fail "build context ${WORK_DIR}/${BACKEND_DIR} missing"
[ -d "${WORK_DIR}/initramfs" ]                   || fail "${WORK_DIR}/initramfs missing"
[ -x "${WORK_DIR}/scripts/build-initramfs.sh" ]  || fail "build-initramfs.sh missing/not executable"
[ -f "${WORK_DIR}/${BACKEND_DIR}/shed-agent" ]     || fail "${WORK_DIR}/${BACKEND_DIR}/shed-agent missing; cross-build with GOOS=linux GOARCH=${PLATFORM#linux/}"
[ -f "${WORK_DIR}/${BACKEND_DIR}/shed-firstboot" ] || fail "${WORK_DIR}/${BACKEND_DIR}/shed-firstboot missing; cross-build with GOOS=linux GOARCH=${PLATFORM#linux/}"
# Guest extension binaries + /etc overlay staged by scripts/stage-guest-binaries.sh.
for guest_bin in shed-ext-ssh-agent shed-ext-aws-credentials docker-credential-shed shed-ext-rc; do
  [ -f "${WORK_DIR}/${BACKEND_DIR}/${guest_bin}" ] || fail "${WORK_DIR}/${BACKEND_DIR}/${guest_bin} missing; run scripts/stage-guest-binaries.sh ${BACKEND_DIR} ${PLATFORM#linux/}"
done
[ -d "${WORK_DIR}/${BACKEND_DIR}/ext-etc" ]        || fail "${WORK_DIR}/${BACKEND_DIR}/ext-etc missing; run scripts/stage-guest-binaries.sh ${BACKEND_DIR} ${PLATFORM#linux/}"

mkdir -p "${OUT_DIR}"

log "creating buildx builder with --driver-opt network=host"
# This is the workaround that makes the in-shed publish flow work:
# the default docker-container driver runs BuildKit in its own network
# namespace, so it cannot reach localhost:5050 inside the shed. Using
# network=host puts BuildKit on the shed's loopback. The flag also
# implies --allow-insecure-entitlement=network.host inside BuildKit,
# which is required for any `RUN --network=host` directives.
docker buildx rm "${BUILDER_NAME}" >/dev/null 2>&1 || true
docker buildx create \
  --name "${BUILDER_NAME}" \
  --driver docker-container \
  --driver-opt network=host \
  --use
docker buildx inspect --bootstrap "${BUILDER_NAME}" >/dev/null

log "starting registry:2 with --network host on ${REGISTRY_HOST}"
docker rm -f "${REGISTRY_CONTAINER}" >/dev/null 2>&1 || true
docker run -d --name "${REGISTRY_CONTAINER}" \
  --network host \
  -e REGISTRY_HTTP_ADDR=0.0.0.0:"${REGISTRY_HOST##*:}" \
  registry:2 >/dev/null
for i in $(seq 1 20); do
  if curl -fsS "http://${REGISTRY_HOST}/v2/" >/dev/null 2>&1; then
    echo "registry up after ${i} attempts"
    break
  fi
  sleep 0.5
  [ "${i}" -eq 20 ] && fail "registry did not come up on ${REGISTRY_HOST}"
done

log "building shed-overlay initramfs (${BACKEND}, ${PLATFORM})"
"${WORK_DIR}/scripts/build-initramfs.sh" \
  --backend "${BACKEND}" \
  --platform "${PLATFORM}" \
  --output "${INITRAMFS}"
[ -s "${INITRAMFS}" ] || fail "initramfs not produced"

rm -rf "${STORE_DIR}"
mkdir -p "${STORE_DIR}"
write_server_yaml "${SERVER_CFG}" "${STORE_DIR}"

# SHED_INSTALL_SHA busts BuildKit's content-blind bind-mount cache when the
# staged agent/service files change (#227). The base stage installs them, so
# the value is identical across variants — compute once.
install_sha="$("${WORK_DIR}/scripts/install-input-sha.sh" "${WORK_DIR}/${BACKEND_DIR}/")"
log "install-sha=${install_sha}"

for variant in ${VARIANTS}; do
  target="${BACKEND_PREFIX}-${variant}"
  source_ref="${REGISTRY_HOST}/charliek/${target}:${VERSION}"

  log "building ${target} (source-ref=${source_ref})"
  "${SHED_BIN}" -c "${SERVER_CFG}" image build \
    --target "${target}" \
    --platform "${PLATFORM}" \
    --source-ref "${source_ref}" \
    -n "${variant}" \
    --initramfs "${INITRAMFS}" \
    --output-dir "${STORE_DIR}" \
    --build-arg "SHED_INSTALL_SHA=${install_sha}" \
    -f "${WORK_DIR}/${BACKEND_DIR}/Dockerfile" \
    "${WORK_DIR}/${BACKEND_DIR}/"

  log "asserting local manifest source-ref for ${variant}"
  "${SHED_BIN}" -c "${SERVER_CFG}" --json image inspect "${variant}" \
    > "${OUT_DIR}/inspect-${variant}.json"
  local_sr=$(jq -r '
    .source_ref //
    (.manifest.annotations // {})["io.shed.source-ref"] //
    (.annotations // {})["io.shed.source-ref"] //
    ""
  ' < "${OUT_DIR}/inspect-${variant}.json")
  echo "local source-ref (${variant}): ${local_sr}"
  [ "${local_sr}" = "${source_ref}" ] || fail "${variant}: local manifest source-ref mismatch (got '${local_sr}', want '${source_ref}')"
  case "${local_sr}" in
    *"-${variant}:latest"*) fail "${variant}: manifest carries legacy :latest source-ref" ;;
  esac

  log "pushing ${variant} -> ${source_ref}"
  "${SHED_BIN}" -c "${SERVER_CFG}" image push --local "${variant}" "${source_ref}"

  log "asserting registry-side source-ref for ${variant}"
  manifest=$(curl -fsS \
    -H 'Accept: application/vnd.oci.image.manifest.v1+json' \
    -H 'Accept: application/vnd.oci.image.index.v1+json' \
    -H 'Accept: application/vnd.docker.distribution.manifest.v2+json' \
    "http://${REGISTRY_HOST}/v2/charliek/${target}/manifests/${VERSION}")
  remote_sr=$(printf '%s' "${manifest}" | jq -r '(.annotations // {})["io.shed.source-ref"] // ""')
  echo "registry source-ref (${variant}): ${remote_sr}"
  [ "${remote_sr}" = "${source_ref}" ] || fail "${variant}: registry-side source-ref mismatch (got '${remote_sr}', want '${source_ref}')"
done

# Round-trip the first variant through `shed image pull` into a fresh
# store. This is what a remote shed-server would do when serving
# `shed create foo --image base` after a publish; the source-ref on
# the pulled manifest needs to match the registry ref so the
# resolveImage cache check hits.
first_variant=$(echo "${VARIANTS}" | awk '{print $1}')
first_target="${BACKEND_PREFIX}-${first_variant}"
first_ref="${REGISTRY_HOST}/charliek/${first_target}:${VERSION}"

log "round-trip pull of ${first_variant} into fresh store"
rm -rf "${STORE_RT}"
mkdir -p "${STORE_RT}"
write_server_yaml "${SERVER_CFG_RT}" "${STORE_RT}"
"${SHED_BIN}" -c "${SERVER_CFG_RT}" image pull "${first_ref}" \
  --platform "${PLATFORM}" \
  --tag "${first_variant}-rt"

"${SHED_BIN}" -c "${SERVER_CFG_RT}" --json image inspect "${first_variant}-rt" \
  > "${OUT_DIR}/inspect-rt-${first_variant}.json"
rt_sr=$(jq -r '
  .source_ref //
  (.manifest.annotations // {})["io.shed.source-ref"] //
  (.annotations // {})["io.shed.source-ref"] //
  ""
' < "${OUT_DIR}/inspect-rt-${first_variant}.json")
echo "round-trip source-ref (${first_variant}): ${rt_sr}"
[ "${rt_sr}" = "${first_ref}" ] || fail "${first_variant}: round-trip source-ref mismatch (got '${rt_sr}', want '${first_ref}')"

printf '\nPASS: built, pushed, and round-tripped %d variant(s) at %s\n' \
  "$(echo "${VARIANTS}" | wc -w | tr -d ' ')" \
  "${VERSION}"
