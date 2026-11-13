#!/usr/bin/env bash
# shed-identity: the cloud shed's boot unit (shed-identity.service).
#
# Runs on every boot, before sshd and the agent, and in this order:
#   1. reads shed-name, shed-id and shed-link-authorized-keys from the
#      metadata server, retrying for up to 60 s; a missing or empty value
#      fails the unit, and the agent (Requires=) then does not start;
#   2. sets the hostname to the shed name;
#   3. writes /run/shed/identity.env (SHED_NAME, SHED_ID) for the agent;
#   4. creates /etc/ssh/ssh_host_ed25519_key if it is absent (first boot);
#   5. writes /etc/ssh/shed-link/authorized_keys from metadata (temp file and
#      rename);
#   6. publishes the host public key as guest attribute shed/host-key. A
#      failure here is logged and does not fail the unit; the server then
#      fails the create.
#
# Names come from internal/remote/guest (guest.go); gce_test.go there keeps
# them in step. Nothing read here is secret, and nothing written is.
#
# Test seams (unset on a real VM):
#   SHED_METADATA_URL      base URL of the metadata server
#                          (default http://169.254.169.254/computeMetadata/v1)
#   SHED_IDENTITY_ROOT     prefix for every file written (default: none);
#                          when set, the running hostname is not changed
#   SHED_IDENTITY_TIMEOUT  seconds to keep retrying metadata (default 60)
set -euo pipefail

md_url="${SHED_METADATA_URL:-http://169.254.169.254/computeMetadata/v1}"
root="${SHED_IDENTITY_ROOT:-}"
timeout_s="${SHED_IDENTITY_TIMEOUT:-60}"

env_file="${root}/run/shed/identity.env"
host_key="${root}/etc/ssh/ssh_host_ed25519_key"
keys_file="${root}/etc/ssh/shed-link/authorized_keys"
hostname_file="${root}/etc/hostname"
hosts_file="${root}/etc/hosts"

log() { echo "shed-identity: $*" >&2; }
die() { log "$*"; exit 1; }

# Fetch one instance attribute. Prints the value; fails when it is missing.
md_get() {
  curl -fsS --max-time 5 -H 'Metadata-Flavor: Google' "${md_url}/instance/attributes/$1"
}

# Retry until every value is present and non-empty, or the deadline passes.
deadline=$(( $(date +%s) + timeout_s ))
while :; do
  name="$(md_get shed-name 2>/dev/null || true)"
  id="$(md_get shed-id 2>/dev/null || true)"
  keys="$(md_get shed-link-authorized-keys 2>/dev/null || true)"
  if [ -n "${name}" ] && [ -n "${id}" ] && [ -n "${keys}" ]; then
    break
  fi
  if [ "$(date +%s)" -ge "${deadline}" ]; then
    [ -n "${name}" ] || log "metadata shed-name is missing or empty"
    [ -n "${id}" ] || log "metadata shed-id is missing or empty"
    [ -n "${keys}" ] || log "metadata shed-link-authorized-keys is missing or empty"
    die "no shed identity after ${timeout_s}s; the agent will not start"
  fi
  sleep 2
done

# The name becomes the hostname and the agent's --shed-name; the ID goes in
# an environment file. Refuse anything that is not plainly one or the other
# (config.ValidateShedName and the shed_id label rules).
if ! printf '%s' "${name}" | grep -Eq '^[a-z]([a-z0-9-]{0,61}[a-z0-9])?$'; then
  die "metadata shed-name '${name}' is not a valid shed name"
fi
if ! printf '%s' "${id}" | grep -Eq '^[a-z0-9]{8,32}$'; then
  die "metadata shed-id '${id}' is not a valid shed ID"
fi

# Authorized keys: one public key per line, no options. Blank lines and
# carriage returns are dropped.
keys="$(printf '%s\n' "${keys}" | tr -d '\r' | sed '/^[[:space:]]*$/d')"
[ -n "${keys}" ] || die "metadata shed-link-authorized-keys has no key"
while IFS= read -r line; do
  if ! printf '%s' "${line}" | grep -Eq '^(ssh-ed25519|ecdsa-sha2-nistp(256|384|521)|ssh-rsa|sk-ssh-ed25519@openssh\.com|sk-ecdsa-sha2-nistp256@openssh\.com) [A-Za-z0-9+/]+=*( [^[:cntrl:]]*)?$'; then
    die "metadata shed-link-authorized-keys has a line that is not a plain public key"
  fi
done <<< "${keys}"

# Write a file by temp file and rename, so a reader never sees half of it.
write_file() { # path mode content
  local path="$1" mode="$2" content="$3" tmp
  mkdir -p "$(dirname "${path}")"
  tmp="$(mktemp "${path}.XXXXXX")"
  printf '%s\n' "${content}" > "${tmp}"
  chmod "${mode}" "${tmp}"
  mv -f "${tmp}" "${path}"
}

# 2. Hostname.
write_file "${hostname_file}" 0644 "${name}"
if [ -z "${root}" ]; then
  hostname "${name}"
fi
# Resolve our own name, or sudo warns on every call.
if [ -f "${hosts_file}" ]; then
  hosts="$(grep -Ev '# shed-identity$|^127\.0\.1\.1[[:space:]]' "${hosts_file}" || true)"
else
  hosts=""
fi
write_file "${hosts_file}" 0644 "$(printf '%s\n127.0.1.1 %s # shed-identity' "${hosts}" "${name}" | sed '/./,$!d')"
log "hostname ${name}"

# 3. Identity for the agent.
mkdir -p "$(dirname "${env_file}")"
chmod 0755 "$(dirname "${env_file}")"
write_file "${env_file}" 0644 "$(printf 'SHED_NAME=%s\nSHED_ID=%s' "${name}" "${id}")"

# 4. Host key, first boot only. The image bakes none.
if [ ! -s "${host_key}" ]; then
  mkdir -p "$(dirname "${host_key}")"
  tmpdir="$(mktemp -d "$(dirname "${host_key}")/.shed-hostkey.XXXXXX")"
  ssh-keygen -q -t ed25519 -N '' -C '' -f "${tmpdir}/key"
  chmod 0600 "${tmpdir}/key"
  chmod 0644 "${tmpdir}/key.pub"
  mv -f "${tmpdir}/key.pub" "${host_key}.pub"
  mv -f "${tmpdir}/key" "${host_key}"
  rmdir "${tmpdir}"
  log "created host key"
fi
# Derive the public half from the private key, in case the .pub is missing
# or stale.
pub="$(ssh-keygen -y -f "${host_key}" | awk '{print $1" "$2}')"
case "${pub}" in
  "ssh-ed25519 "?*) ;;
  *) die "cannot read the public half of ${host_key}" ;;
esac
write_file "${host_key}.pub" 0644 "${pub}"

# 5. The server's key(s) for shed-link. Directory root 0755, file root 0644.
mkdir -p "$(dirname "${keys_file}")"
chmod 0755 "$(dirname "${keys_file}")"
write_file "${keys_file}" 0644 "${keys}"
log "installed $(printf '%s\n' "${keys}" | wc -l) shed-link key(s)"

# 6. Publish the host key. Guest-writable, so only a claim the server checks
# against what sshd presents at first contact.
if curl -fsS --max-time 10 --retry 5 --retry-delay 2 --retry-connrefused \
     -X PUT -H 'Metadata-Flavor: Google' --data-binary "${pub}" \
     "${md_url}/instance/guest-attributes/shed/host-key" >/dev/null; then
  log "published host key as guest attribute shed/host-key"
else
  log "WARNING: could not publish the host key as guest attribute shed/host-key"
fi
