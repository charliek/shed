#!/usr/bin/env bash
# Assertions run inside the builder VM before its disk is saved as an image.
# Any failed assertion fails the bake.
#
#   verify.sh --phase live   [--variant V] [--pins PATH] [--link-key PATH]
#     After provision.sh, with shed-identity and shed-agent started from the
#     builder's own metadata: users, sudo, sshd's effective settings, file
#     modes, units, the agent's sockets and version, the host key claim,
#     Docker, and the tools. --link-key is the private half of the builder's
#     shed-link-authorized-keys metadata; with it, a loopback SSH login as
#     shed-link runs the agent's version command.
#
#   verify.sh --phase sealed [--bake-user NAME]
#     After seal.sh: no host key, no authorized_keys anywhere, machine-id
#     empty, no cloud-init instance state, no bake user.
#
# Names come from internal/remote/guest (guest.go).
# The checks pass shell snippets to bash -c, quoted so they expand there.
# shellcheck disable=SC2016
set -uo pipefail

phase=""
variant=extensions
pins=""
link_key=""
bake_user=""
while [ $# -gt 0 ]; do
  case "$1" in
    --phase) phase="${2:?}"; shift 2 ;;
    --variant) variant="${2:?}"; shift 2 ;;
    --pins) pins="${2:?}"; shift 2 ;;
    --link-key) link_key="${2:?}"; shift 2 ;;
    --bake-user) bake_user="${2:?}"; shift 2 ;;
    -h|--help) sed -n '2,20p' "$0"; exit 0 ;;
    *) echo "verify: unknown argument: $1" >&2; exit 2 ;;
  esac
done
case "${phase}" in live|sealed) ;; *) echo "verify: --phase must be live or sealed" >&2; exit 2 ;; esac
[ "$(id -u)" -eq 0 ] || { echo "verify: must run as root" >&2; exit 2; }

failures=0
pass() { echo "ok   $1"; }
fail() { echo "FAIL $1${2:+: $2}"; failures=$((failures + 1)); }
# check "description" command...: passes when the command exits 0.
check() {
  local desc="$1"; shift
  local out
  if out="$("$@" 2>&1)"; then pass "${desc}"; else fail "${desc}" "$(printf '%s' "${out}" | tail -3 | tr '\n' ' ')"; fi
}
# expect "description" want got
expect() {
  if [ "$2" = "$3" ]; then pass "$1"; else fail "$1" "want '$2', got '$3'"; fi
}
# stat_is path owner:group mode
stat_is() {
  local got
  got="$(stat -c '%U:%G %a' "$1" 2>/dev/null || echo missing)"
  expect "$1 is $2 $3" "$2 $3" "${got}"
}

metadata() {
  curl -fsS --max-time 5 -H 'Metadata-Flavor: Google' "http://169.254.169.254/computeMetadata/v1/$1"
}

verify_live() {
  echo "--- users"
  expect "shed has uid 1000" 1000 "$(id -u shed 2>/dev/null)"
  check "shed is in the docker group" bash -c 'id -nG shed | tr " " "\n" | grep -qx docker'
  check "shed has passwordless sudo" runuser -u shed -- sudo -n true
  expect "shed's login shell is bash" /bin/bash "$(getent passwd shed | cut -d: -f7)"
  check "shed-link is a system user" bash -c '[ "$(id -u shed-link)" -lt 1000 ]'
  expect "shed-link's primary group is shed-link" shed-link "$(id -gn shed-link 2>/dev/null)"
  expect "shed-link's shell is /bin/sh" /bin/sh "$(getent passwd shed-link | cut -d: -f7)"
  expect "shed-link's password field is *" '*' "$(getent shadow shed-link | cut -d: -f2)"
  expect "shed-link is in no other group" shed-link "$(id -nG shed-link 2>/dev/null)"
  expect "shed-link's only sudo right is the installer" \
    "(root) NOPASSWD: /usr/local/sbin/shed-agent-install" \
    "$(sudo -l -U shed-link 2>/dev/null | sed -n '/may run the following/,$p' | sed '1d' | sed 's/^[[:space:]]*//' | grep -v '^$')"
  expect "/etc/sudoers.d/shed-link holds exactly the rule" \
    "shed-link ALL=(root) NOPASSWD: /usr/local/sbin/shed-agent-install" "$(cat /etc/sudoers.d/shed-link 2>/dev/null)"
  stat_is /etc/sudoers.d/shed-link root:root 440
  check "sudoers parses" visudo -c -q

  echo "--- sshd"
  local t
  t="$(/usr/sbin/sshd -T -C user=shed-link,host=bake.invalid,addr=192.0.2.1 2>&1)" || fail "sshd -T" "${t}"
  local kv
  for kv in \
    "permitrootlogin no" \
    "passwordauthentication no" \
    "kbdinteractiveauthentication no" \
    "allowusers shed-link" \
    "hostkey /etc/ssh/ssh_host_ed25519_key" \
    "authorizedkeysfile /etc/ssh/shed-link/authorized_keys" \
    "allowstreamlocalforwarding local" \
    "allowtcpforwarding local" \
    "permitopen [/run/shed-agent/1024.sock]:* [/run/shed-agent/1026.sock]:* [/run/shed-agent/1028.sock]:*" \
    "allowagentforwarding no" \
    "x11forwarding no" \
    "permittunnel no" \
    "permittty no" \
    "permituserrc no"; do
    local key="${kv%% *}"
    expect "sshd ${key}" "${kv}" "$(printf '%s\n' "${t}" | grep -i "^${key} " | tr '\n' '|' | sed 's/|$//')"
  done
  stat_is /etc/ssh/shed-link root:root 755
  stat_is /etc/ssh/shed-link/authorized_keys root:root 644
  stat_is /etc/ssh/ssh_host_ed25519_key root:root 600
  check "sshd listens on port 22" bash -c 'ss -Hltn | awk "{print \$4}" | grep -Eq "(^|:)22$"'
  check "nothing but sshd listens off loopback" bash -c '
    ss -Hltnu | awk "{print \$5}" | grep -Ev "^(127\.|\[::1\]|::1)" | grep -Ev "(^|:)22$" | grep -Ev "^(0\.0\.0\.0|\*|\[::\]):(68|546)$" | grep -Ev "%" | { ! grep .; }'

  echo "--- files and units"
  stat_is /usr/local/bin/shed-agent root:root 755
  stat_is /usr/local/sbin/shed-agent-install root:root 755
  stat_is /usr/local/sbin/shed-identity root:root 755
  stat_is /etc/shed/layout root:root 644
  expect "layout file holds 1" 1 "$(cat /etc/shed/layout 2>/dev/null)"
  local u
  for u in shed-identity.service shed-agent.service docker.service unattended-upgrades.service; do
    expect "${u} is enabled" enabled "$(systemctl is-enabled "${u}" 2>&1)"
  done
  expect "shed-identity.service ran" active "$(systemctl is-active shed-identity.service 2>&1)"
  expect "shed-agent.service is running" active "$(systemctl is-active shed-agent.service 2>&1)"
  expect "shed-agent.service KillMode" process "$(systemctl show -p KillMode --value shed-agent.service)"
  expect "shed-agent.service Restart" always "$(systemctl show -p Restart --value shed-agent.service)"
  check "shed-agent.service requires shed-identity.service" bash -c 'systemctl show -p Requires --value shed-agent.service | grep -qw shed-identity.service'
  check "security updates are on" bash -c 'apt-config dump | grep -q "^APT::Periodic::Unattended-Upgrade \"1\";"'
  check "shutdown inhibitors are capped at 5 s" bash -c 'systemd-analyze cat-config systemd/logind.conf | grep -E "^InhibitDelayMaxSec=" | tail -1 | grep -qx "InhibitDelayMaxSec=5"'
  check "guest agent leaves host keys alone" grep -Eq '^set_host_keys[[:space:]]*=[[:space:]]*false' /etc/default/instance_configs.cfg

  echo "--- identity"
  local md_name md_id
  md_name="$(metadata instance/attributes/shed-name 2>/dev/null)"
  md_id="$(metadata instance/attributes/shed-id 2>/dev/null)"
  if [ -z "${md_name}" ] || [ -z "${md_id}" ]; then
    fail "builder metadata shed-name and shed-id are readable"
  fi
  expect "hostname is the shed name" "${md_name}" "$(hostname)"
  expect "identity.env" "SHED_NAME=${md_name}|SHED_ID=${md_id}" "$(tr '\n' '|' < /run/shed/identity.env 2>/dev/null | sed 's/|$//')"
  check "authorized_keys is not empty" test -s /etc/ssh/shed-link/authorized_keys
  expect "authorized_keys equals metadata" \
    "$(metadata instance/attributes/shed-link-authorized-keys 2>/dev/null | tr -d '\r' | sed '/^[[:space:]]*$/d')" \
    "$(cat /etc/ssh/shed-link/authorized_keys 2>/dev/null)"
  local pub claim
  pub="$(ssh-keygen -y -f /etc/ssh/ssh_host_ed25519_key 2>/dev/null | awk '{print $1" "$2}')"
  claim="$(metadata instance/guest-attributes/shed/host-key 2>/dev/null)"
  expect "guest attribute shed/host-key is the host key" "${pub}" "${claim}"
  check "agent runs with --shed-name" bash -c "tr '\\0' ' ' < /proc/\$(systemctl show -p MainPID --value shed-agent.service)/cmdline | grep -q -- '--shed-name ${md_name}'"

  echo "--- agent"
  local info
  info="$(timeout 10 /usr/local/bin/shed-agent version 2>&1)"
  check "shed-agent version reports layout 1 and a protocol" \
    jq -e '.layout == 1 and (.protocol | type == "number") and .protocol >= 1 and (.version | type == "string")' <<< "${info}"
  stat_is /run/shed-agent root:shed-link 750
  local port
  for port in 1024 1026 1028; do
    check "socket ${port}.sock exists" test -S "/run/shed-agent/${port}.sock"
    stat_is "/run/shed-agent/${port}.sock" root:shed-link 660
    check "shed-link can open ${port}.sock" runuser -u shed-link -- test -w "/run/shed-agent/${port}.sock"
    check "shed cannot open ${port}.sock" bash -c "! runuser -u shed -- test -w /run/shed-agent/${port}.sock"
  done
  if [ -n "${link_key}" ]; then
    local khosts
    khosts="$(mktemp)"
    printf '127.0.0.1 %s\n' "${pub}" > "${khosts}"
    local sshopts=(-i "${link_key}" -o IdentitiesOnly=yes -o BatchMode=yes -o ConnectTimeout=10
      -o StrictHostKeyChecking=yes -o "UserKnownHostsFile=${khosts}")
    local remote
    remote="$(timeout 30 ssh "${sshopts[@]}" shed-link@127.0.0.1 /usr/local/bin/shed-agent version 2>&1)"
    expect "shed-link logs in over SSH and runs the version command" "${info}" "${remote}"
    check "shed-link's SSH session gets no TTY" bash -c "! ssh -tt ${sshopts[*]} shed-link@127.0.0.1 tty 2>/dev/null | grep -q /dev/"
    check "shed cannot log in over SSH" bash -c "! ssh ${sshopts[*]} shed@127.0.0.1 true"
    rm -f "${khosts}"
  fi

  echo "--- docker"
  check "docker uses an overlay storage driver" bash -c 'docker info --format "{{.Driver}}" | grep -Eq "^overlay"'
  check "docker runs hello-world" docker run --rm hello-world
  check "docker compose is installed" docker compose version
  check "shed can use docker" runuser -u shed -- docker ps

  echo "--- tools"
  local tool
  for tool in git gh tmux jq curl rg nvim; do
    check "${tool} is installed" command -v "${tool}"
  done
  if [ "${variant}" = extensions ]; then
    check "strix is installed" command -v strix
    check "prox is installed" command -v prox
    if [ -n "${pins}" ]; then
      local rv rp cv
      rv="$(sed -nE 's/^ROOST_SESSION_VERSION=([0-9.]+)$/\1/p' "${pins}")"
      rp="$(sed -nE 's/^ROOST_SESSION_PROTOCOL=([0-9]+)$/\1/p' "${pins}")"
      cv="$(sed -nE 's/^CRAZE_VERSION=([0-9.]+)$/\1/p' "${pins}")"
      check "roost-session is ${rv}/protocol ${rp}" bash -c \
        "/usr/bin/roost-session identify | jq -e '.session_protocol == ${rp} and .app_version == \"${rv}\"'"
      expect "craze version" "${cv}" "$(/usr/bin/craze --version 2>&1)"
    else
      fail "roost-session and craze pins" "--pins not given"
    fi
  fi
}

verify_sealed() {
  echo "--- sealed"
  expect "no host key is left" "" "$(find /etc/ssh -maxdepth 1 -name 'ssh_host_*' 2>/dev/null)"
  local keys
  keys="$(find /root /home /etc/ssh /var/lib/shed-link -name 'authorized_keys*' 2>/dev/null)"
  expect "no authorized_keys file is left" "" "${keys}"
  check "machine-id is empty" bash -c '[ -f /etc/machine-id ] && [ ! -s /etc/machine-id ]'
  check "dbus machine-id is not a copy" bash -c '[ ! -s /var/lib/dbus/machine-id ] || [ -L /var/lib/dbus/machine-id ]'
  check "no cloud-init instance state" bash -c '[ ! -e /var/lib/cloud/instance ] && [ -z "$(ls -A /var/lib/cloud/instances 2>/dev/null)" ]'
  check "no cloud-init sudoers file" test ! -e /etc/sudoers.d/90-cloud-init-users
  if [ -n "${bake_user}" ]; then
    check "bake user ${bake_user} is gone" bash -c "! getent passwd ${bake_user} && [ ! -e /home/${bake_user} ]"
  fi
  expect "only shed has a login uid" shed \
    "$(getent passwd | awk -F: '$3 >= 1000 && $3 < 60000 {print $1}' | tr '\n' ' ' | sed 's/ $//')"
  check "no guest-agent hosts lines" bash -c '! grep -q "Added by Google" /etc/hosts'
  check "no shed-identity hosts line" bash -c '! grep -q "# shed-identity$" /etc/hosts'
  check "no apt lists" bash -c '[ -z "$(find /var/lib/apt/lists -maxdepth 1 -type f ! -name lock 2>/dev/null)" ]'
  check "layout file still holds 1" grep -qx 1 /etc/shed/layout
}

case "${phase}" in
  live) verify_live ;;
  sealed) verify_sealed ;;
esac

if [ "${failures}" -gt 0 ]; then
  echo "verify (${phase}): ${failures} check(s) failed"
  exit 1
fi
echo "verify (${phase}): all checks passed"
