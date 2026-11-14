#!/usr/bin/env bash
# Bake a shed remote VM image on Google Compute Engine.
#
# Boots a throwaway builder VM from stock Ubuntu 24.04, copies gce/, the
# agent binary and the guest tool pins to it, runs gce/bake.sh there
# (provision, verify, seal, verify the seal), stops it, saves its disk as an
# image and deletes it. A trap deletes the builder if any step fails.
#
# The builder has no service account and no scopes, its own network tag, a
# maximum run duration after which Google deletes it, and a throwaway SSH key
# that only the bake uses. Its host key is learned from the guest attribute
# the stock guest agent publishes, not trusted on first use.
#
# Usage:
#   scripts/build-gce-image.sh [--print] [options]
#
#   --print                 print every command and run nothing
#   --env-file FILE         read KEY=value settings (the variables below)
#                           from FILE before the flags are applied
#   --project P             $SHED_GCE_PROJECT    (required)
#   --zone Z                $SHED_GCE_ZONE       (required)
#   --network N             $SHED_GCE_NETWORK    (required)
#   --subnet S              $SHED_GCE_SUBNET     (required)
#   --tag T                 $SHED_GCE_BAKE_TAG, else $SHED_GCE_TAG (required):
#                           the network tag whose firewall rule admits this
#                           machine on tcp:22
#   --variant V             extensions (default) or base
#   --arch A                amd64 (the only architecture baked today)
#   --agent FILE            linux agent binary (default: make build-agent)
#   --version V             shed version for the name and label
#                           (default: git describe --tags --always --dirty)
#   --timestamp T           UTC build time for names (default: now,
#                           YYYYMMDD-HHMMSS)
#   --family F              image family (default: shed-<variant>-<arch>)
#   --machine-type M        builder machine type (default: e2-standard-4)
#   --disk-gb N             builder and image disk size (default: 20)
#   --max-run-duration D    builder lifetime cap (default: 3h)
#   --label KEY=VALUE       extra builder label (repeatable)
#   --accept-new-host-key   trust the builder's host key on first use when
#                           the guest attribute never appears
#   --keep-builder          on failure, leave the builder for inspection
#
# The image is named shed-remote-<variant>-<version>-<timestamp>, joins the
# family, and carries labels shed_layout, shed_protocol, shed_arch,
# shed_variant and shed_version. Nothing project-specific is in this file:
# every value comes from a flag or the environment.
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "${repo_root}"

die() { echo "build-gce-image: $*" >&2; exit 1; }
log() { echo "==> $*" >&2; }

print=no
env_file=""
project="" zone="" network="" subnet="" tag=""
variant=extensions
arch=amd64
agent=""
version=""
timestamp=""
family=""
machine_type=e2-standard-4
disk_gb=20
max_run=3h
extra_labels=()
accept_new=no
keep_builder=no

while [ $# -gt 0 ]; do
  case "$1" in
    --print) print=yes; shift ;;
    --env-file) env_file="${2:?}"; shift 2 ;;
    --project) project="${2:?}"; shift 2 ;;
    --zone) zone="${2:?}"; shift 2 ;;
    --network) network="${2:?}"; shift 2 ;;
    --subnet) subnet="${2:?}"; shift 2 ;;
    --tag) tag="${2:?}"; shift 2 ;;
    --variant) variant="${2:?}"; shift 2 ;;
    --arch) arch="${2:?}"; shift 2 ;;
    --agent) agent="${2:?}"; shift 2 ;;
    --version) version="${2:?}"; shift 2 ;;
    --timestamp) timestamp="${2:?}"; shift 2 ;;
    --family) family="${2:?}"; shift 2 ;;
    --machine-type) machine_type="${2:?}"; shift 2 ;;
    --disk-gb) disk_gb="${2:?}"; shift 2 ;;
    --max-run-duration) max_run="${2:?}"; shift 2 ;;
    --label) extra_labels+=("${2:?}"); shift 2 ;;
    --accept-new-host-key) accept_new=yes; shift ;;
    --keep-builder) keep_builder=yes; shift ;;
    -h|--help) sed -n '2,46p' "$0"; exit 0 ;;
    *) die "unknown argument: $1 (see --help)" ;;
  esac
done

if [ -n "${env_file}" ]; then
  [ -f "${env_file}" ] || die "env file ${env_file} not found"
  set -a
  # shellcheck source=/dev/null
  . "${env_file}"
  set +a
fi
project="${project:-${SHED_GCE_PROJECT:-}}"
zone="${zone:-${SHED_GCE_ZONE:-}}"
network="${network:-${SHED_GCE_NETWORK:-}}"
subnet="${subnet:-${SHED_GCE_SUBNET:-}}"
tag="${tag:-${SHED_GCE_BAKE_TAG:-${SHED_GCE_TAG:-}}}"

[ -n "${project}" ] || die "no project: pass --project or set SHED_GCE_PROJECT"
[ -n "${zone}" ] || die "no zone: pass --zone or set SHED_GCE_ZONE"
[ -n "${network}" ] || die "no network: pass --network or set SHED_GCE_NETWORK"
[ -n "${subnet}" ] || die "no subnet: pass --subnet or set SHED_GCE_SUBNET"
[ -n "${tag}" ] || die "no network tag: pass --tag or set SHED_GCE_BAKE_TAG (or SHED_GCE_TAG)"
case "${variant}" in base|extensions) ;; *) die "--variant must be base or extensions" ;; esac
case "${arch}" in amd64) ;; *) die "--arch: only amd64 is baked today" ;; esac
printf '%s' "${disk_gb}" | grep -Eq '^[0-9]+$' || die "--disk-gb must be a number"
for l in ${extra_labels[@]+"${extra_labels[@]}"}; do
  printf '%s' "${l}" | grep -Eq '^[a-z][a-z0-9_-]{0,62}=[a-z0-9_-]{0,63}$' || die "--label ${l}: want key=value in GCE label syntax"
done

# --- names --------------------------------------------------------------

# GCE names and label values: lowercase letters, digits and hyphens.
sanitize() {
  printf '%s' "$1" | tr '[:upper:]' '[:lower:]' | sed -E 's/[^a-z0-9]+/-/g; s/^-+//; s/-+$//'
}

[ -n "${version}" ] || version="$(git describe --tags --always --dirty 2>/dev/null || echo dev)"
[ -n "${timestamp}" ] || timestamp="$(date -u +%Y%m%d-%H%M%S)"
printf '%s' "${timestamp}" | grep -Eq '^[0-9]{8}-[0-9]{6}$' || die "--timestamp must be YYYYMMDD-HHMMSS"
version_s="$(sanitize "${version}")"
[ -n "${version_s}" ] || die "version '${version}' has no usable characters"
[ -n "${family}" ] || family="shed-${variant}-${arch}"

prefix="shed-remote-${variant}-"
room=$(( 63 - ${#prefix} - 1 - ${#timestamp} ))
version_n="$(printf '%s' "${version_s}" | cut -c1-"${room}" | sed -E 's/-+$//')"
image="${prefix}${version_n}-${timestamp}"
builder="shed-image-builder-${timestamp}"
bake_user=shedbake
bake_id="bake${timestamp//-/}"

layout="$(sed -nE 's/^SHED_LAYOUT=([0-9]+)$/\1/p' gce/provision.sh | head -1)"
protocol="$(sed -nE 's/^[[:space:]]*AgentProtocol[[:space:]]*=[[:space:]]*([0-9]+).*/\1/p' internal/remote/guest/guest.go | head -1)"
[ -n "${layout}" ] || die "cannot read SHED_LAYOUT from gce/provision.sh"
[ -n "${protocol}" ] || die "cannot read AgentProtocol from internal/remote/guest/guest.go"

builder_labels="shed_bake=true"
for l in ${extra_labels[@]+"${extra_labels[@]}"}; do builder_labels+=",${l}"; done
image_labels="shed_layout=${layout},shed_protocol=${protocol},shed_arch=${arch},shed_variant=${variant},shed_version=${version_s:0:63}"

# --- running or printing ----------------------------------------------------

# Print a command the way a shell would read it back.
show() {
  local out="" a
  for a in "$@"; do
    if [[ "${a}" =~ ^[A-Za-z0-9_@%+=:,./{}\$-]+$ ]]; then
      out+="${a} "
    else
      out+="'${a//\'/\'\\\'\'}' "
    fi
  done
  printf '%s\n' "${out% }"
}
run() {
  if [ "${print}" = yes ]; then show "$@"; else "$@"; fi
}

if [ "${print}" = yes ]; then
  # shellcheck disable=SC2016 # printed literally, for the reader's shell
  work='${WORK}'
else
  work="$(mktemp -d)"
fi
stage="${work}/stage"
gc=(--project "${project}" --zone "${zone}")
ssh_opts=(-i "${work}/bake_key" -o IdentitiesOnly=yes -o BatchMode=yes -o ConnectTimeout=10
  -o ServerAliveInterval=30 -o "UserKnownHostsFile=${work}/known_hosts")
if [ "${accept_new}" = yes ]; then
  ssh_opts+=(-o StrictHostKeyChecking=accept-new)
else
  ssh_opts+=(-o StrictHostKeyChecking=yes)
fi

builder_created=no
cleanup() {
  local rc=$?
  if [ "${print}" != yes ]; then
    if [ "${builder_created}" = yes ]; then
      if [ "${keep_builder}" = yes ] && [ "${rc}" -ne 0 ]; then
        log "leaving builder ${builder} for inspection (--keep-builder); delete it with:"
        show gcloud compute instances delete "${builder}" "${gc[@]}" --quiet >&2
      else
        log "deleting builder ${builder}"
        gcloud compute instances delete "${builder}" "${gc[@]}" --quiet >&2 || \
          log "WARNING: could not delete builder ${builder}; delete it by hand"
      fi
    fi
    rm -rf "${work}"
  fi
  exit "${rc}"
}
trap cleanup EXIT

if [ "${print}" = yes ]; then
  echo "# build-gce-image: image ${image} (family ${family}) from builder ${builder}"
  echo "WORK=\$(mktemp -d)"
fi

# --- 1. stage -----------------------------------------------------------

if [ -z "${agent}" ]; then
  log "building the agent for linux/${arch}"
  run make build-agent GOARCH="${arch}"
  agent=bin/shed-agent
elif [ "${print}" != yes ]; then
  [ -f "${agent}" ] || die "agent ${agent} not found"
fi

log "staging the bake files"
run mkdir -p "${stage}"
run cp gce/bake.sh gce/provision.sh gce/verify.sh gce/seal.sh gce/shed-identity.sh \
  gce/shed-agent-install.sh gce/shed-identity.service gce/shed-agent.service gce/daemon.json "${stage}/"
run install -m 0755 "${agent}" "${stage}/shed-agent"
if [ "${print}" = yes ]; then
  echo "scripts/read-guest-pins.sh > ${stage}/pins.env"
else
  scripts/read-guest-pins.sh > "${stage}/pins.env"
fi
run ssh-keygen -q -t ed25519 -N '' -C shed-bake -f "${work}/bake_key"
run ssh-keygen -q -t ed25519 -N '' -C shed-bake-link -f "${stage}/link_key"

write_user_data() {
  cat <<EOF
#cloud-config
users:
  - name: ${bake_user}
    uid: 2999
    shell: /bin/bash
    lock_passwd: true
    sudo: "ALL=(ALL) NOPASSWD:ALL"
    ssh_authorized_keys:
      - $1
EOF
}
if [ "${print}" = yes ]; then
  echo "# ${work}/user-data: cloud-config creating ${bake_user} (uid 2999, sudo) with ${work}/bake_key.pub"
  write_user_data '<bake_key.pub>' | sed 's/^/#   /'
else
  write_user_data "$(cat "${work}/bake_key.pub")" > "${work}/user-data"
fi

# --- 2. builder ---------------------------------------------------------

source_image='<newest ubuntu-2404-lts-amd64>'
if [ "${print}" = yes ]; then
  show gcloud compute images describe-from-family ubuntu-2404-lts-amd64 --project ubuntu-os-cloud --format='value(name)'
else
  source_image="$(gcloud compute images describe-from-family ubuntu-2404-lts-amd64 --project ubuntu-os-cloud --format='value(name)')"
  [ -n "${source_image}" ] || die "cannot resolve the ubuntu-2404-lts-amd64 family"
  log "source image ${source_image}"
fi

log "creating builder ${builder}"
builder_created=yes
run gcloud compute instances create "${builder}" "${gc[@]}" \
  --machine-type "${machine_type}" \
  --image "${source_image}" --image-project ubuntu-os-cloud \
  --boot-disk-size "${disk_gb}GB" --boot-disk-type pd-balanced \
  --network "${network}" --subnet "${subnet}" --tags "${tag}" \
  --no-service-account --no-scopes \
  --max-run-duration "${max_run}" --instance-termination-action DELETE \
  --labels "${builder_labels}" \
  --metadata "enable-guest-attributes=TRUE,block-project-ssh-keys=TRUE,enable-oslogin=FALSE,shed-name=shed-bake,shed-id=${bake_id}" \
  --metadata-from-file "user-data=${work}/user-data,shed-link-authorized-keys=${stage}/link_key.pub"

ip='<builder-ip>'
if [ "${print}" = yes ]; then
  show gcloud compute instances describe "${builder}" "${gc[@]}" --format='get(networkInterfaces[0].accessConfigs[0].natIP)'
else
  ip="$(gcloud compute instances describe "${builder}" "${gc[@]}" --format='get(networkInterfaces[0].accessConfigs[0].natIP)')"
  [ -n "${ip}" ] || die "builder ${builder} has no external address"
  log "builder address ${ip}"
fi

# --- 3. host key and SSH ---------------------------------------------------

log "learning the builder's host key from its guest attributes"
if [ "${print}" = yes ]; then
  echo "# poll until set (up to 180 s), then write '<builder-ip> <key>' to ${work}/known_hosts:"
  show gcloud compute instances get-guest-attributes "${builder}" "${gc[@]}" --query-path=hostkeys/ssh-ed25519 --format='value(value)'
else
  : > "${work}/known_hosts"
  hostkey=""
  for _ in $(seq 1 36); do
    hostkey="$(gcloud compute instances get-guest-attributes "${builder}" "${gc[@]}" \
      --query-path=hostkeys/ssh-ed25519 --format='value(value)' 2>/dev/null || true)"
    [ -n "${hostkey}" ] && break
    sleep 5
  done
  if [ -n "${hostkey}" ]; then
    case "${hostkey}" in ssh-ed25519\ *) ;; *) hostkey="ssh-ed25519 ${hostkey}" ;; esac
    printf '%s %s\n' "${ip}" "${hostkey}" > "${work}/known_hosts"
  elif [ "${accept_new}" = yes ]; then
    log "WARNING: no hostkeys/ssh-ed25519 guest attribute; trusting the first key seen (--accept-new-host-key)"
  else
    die "no hostkeys/ssh-ed25519 guest attribute after 180 s; rerun with --accept-new-host-key to trust on first use"
  fi
fi

log "waiting for SSH as ${bake_user}"
if [ "${print}" = yes ]; then
  echo "# retry until it succeeds (up to 300 s):"
  show ssh "${ssh_opts[@]}" "${bake_user}@${ip}" true
else
  ok=no
  for _ in $(seq 1 60); do
    if ssh "${ssh_opts[@]}" "${bake_user}@${ip}" true 2>/dev/null; then ok=yes; break; fi
    sleep 5
  done
  [ "${ok}" = yes ] || die "cannot log in to ${bake_user}@${ip} (firewall for tag ${tag}? host key changed?)"
fi

# --- 4. bake ----------------------------------------------------------------

log "copying the bake files"
run scp -p -r "${ssh_opts[@]}" "${stage}" "${bake_user}@${ip}:/tmp/shed-bake"

log "running gce/bake.sh on the builder"
remote_cmd="sudo /tmp/shed-bake/bake.sh --variant ${variant} --arch ${arch} --bake-user ${bake_user}"
if [ "${print}" = yes ]; then
  show ssh "${ssh_opts[@]}" "${bake_user}@${ip}" "${remote_cmd}"
  echo "# the last line of its output must be BAKE OK"
else
  # shellcheck disable=SC2029 # expanded here on purpose; every part is validated
  ssh "${ssh_opts[@]}" "${bake_user}@${ip}" "${remote_cmd}" 2>&1 | tee "${work}/bake.log"
  [ "$(tail -n 1 "${work}/bake.log" | tr -d '\r')" = "BAKE OK" ] || die "the bake did not finish (see the output above)"
fi

# --- 5. image ---------------------------------------------------------------

log "stopping the builder"
run gcloud compute instances stop "${builder}" "${gc[@]}"

log "creating image ${image} in family ${family}"
run gcloud compute images create "${image}" --project "${project}" \
  --source-disk "${builder}" --source-disk-zone "${zone}" \
  --family "${family}" \
  --labels "${image_labels}" \
  --description "shed remote guest layout ${layout}, agent protocol ${protocol}, variant ${variant}, ${arch}, shed ${version}, from ${source_image}"

log "deleting the builder"
run gcloud compute instances delete "${builder}" "${gc[@]}" --quiet
builder_created=no

if [ "${print}" = yes ]; then
  echo "rm -rf \"\${WORK}\""
fi
log "image ${image} (family ${family})"
if [ "${print}" != yes ]; then
  echo "${image}"
fi
