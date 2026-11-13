#!/usr/bin/env bash
# Turn a stock Ubuntu 24.04 GCE VM into a shed remote image (guest layout 1).
#
# Runs as root on the builder VM. scripts/build-gce-image.sh copies this
# directory, the agent binary and a pins file to the builder and runs
# gce/bake.sh, which calls this script, then gce/verify.sh, gce/seal.sh and
# gce/verify.sh again. This script installs; it does not seal (no key or
# machine-id cleanup), so it can be re-run.
#
# What a layout-1 guest carries is fixed by internal/remote/guest (guest.go);
# internal/remote/guest/gce_test.go keeps the paths and names here in step
# with it. Any change to what this script lays down that the server relies on
# is a new layout: raise SHED_LAYOUT here and guest.Layout together.
#
# Carried over from firecracker/Dockerfile (base + extensions): the package
# list, gh, the shed user (uid 1000, passwordless sudo, docker group) and its
# home, tmux.conf, the prompt, strix/prox/roost-session/craze. Docker CE is in
# every variant (the cloud has no `full`). Dropped: the kernel, network-setup,
# the systemd unit wipe, the transient machine-id, the shed-extensions
# credential brokers (no broker host on a cloud shed).
#
# Usage:
#   provision.sh --agent PATH [--variant base|extensions] [--arch amd64|arm64]
#                [--pins PATH]
#     --agent    the linux shed-agent binary to install
#     --variant  extensions (default) adds strix, prox, roost-session, craze
#     --arch     must equal the VM's dpkg architecture (default: the VM's)
#     --pins     KEY=value file from scripts/read-guest-pins.sh; required for
#                extensions. This script holds no pin of its own.
set -euo pipefail

# The guest layout this script lays down (guest.Layout).
SHED_LAYOUT=1

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

die() { echo "provision: $*" >&2; exit 1; }
log() { echo "==> provision: $*"; }

variant=extensions
arch=""
agent=""
pins=""
while [ $# -gt 0 ]; do
  case "$1" in
    --variant) variant="${2:?}"; shift 2 ;;
    --arch) arch="${2:?}"; shift 2 ;;
    --agent) agent="${2:?}"; shift 2 ;;
    --pins) pins="${2:?}"; shift 2 ;;
    -h|--help) sed -n '2,32p' "$0"; exit 0 ;;
    *) die "unknown argument: $1" ;;
  esac
done

[ "$(id -u)" -eq 0 ] || die "must run as root"
case "${variant}" in base|extensions) ;; *) die "--variant must be base or extensions, not '${variant}'" ;; esac
[ -n "${agent}" ] || die "--agent is required"
[ -f "${agent}" ] || die "agent binary ${agent} not found"

# shellcheck source=/dev/null
. /etc/os-release
if [ "${ID:-}" != ubuntu ] || [ "${VERSION_ID:-}" != "24.04" ]; then
  die "needs Ubuntu 24.04, found ${ID:-?} ${VERSION_ID:-?}"
fi
dpkg_arch="$(dpkg --print-architecture)"
[ -n "${arch}" ] || arch="${dpkg_arch}"
[ "${arch}" = "${dpkg_arch}" ] || die "--arch ${arch} but this VM is ${dpkg_arch}"
case "${arch}" in amd64|arm64) ;; *) die "unsupported arch ${arch}" ;; esac
codename="${UBUNTU_CODENAME:-${VERSION_CODENAME:-}}"
[ -n "${codename}" ] || die "no Ubuntu codename in /etc/os-release"

# systemd is PID 1 on the builder. In a plain container (used only to check
# this script's package steps) it is not, and unit enablement runs offline.
live_systemd=no
[ -d /run/systemd/system ] && live_systemd=yes

# ---------------------------------------------------------------------------
# Pins: parsed as data, never sourced.
# ---------------------------------------------------------------------------
pin() {
  local key="$1" pattern="$2" value
  value="$(sed -nE "s/^${key}=(.*)\$/\\1/p" "${pins}" | head -1)"
  printf '%s' "${value}" | grep -Eq "${pattern}" || die "pin ${key} missing or malformed in ${pins}"
  printf '%s' "${value}"
}
if [ "${variant}" = extensions ]; then
  [ -n "${pins}" ] || die "--pins is required for --variant extensions"
  [ -f "${pins}" ] || die "pins file ${pins} not found"
  ROOST_SESSION_VERSION="$(pin ROOST_SESSION_VERSION '^[0-9]+\.[0-9]+\.[0-9]+$')"
  ROOST_SESSION_PROTOCOL="$(pin ROOST_SESSION_PROTOCOL '^[0-9]+$')"
  CRAZE_VERSION="$(pin CRAZE_VERSION '^[0-9]+\.[0-9]+\.[0-9]+$')"
  case "${arch}" in
    amd64)
      ROOST_SESSION_SHA256="$(pin ROOST_SESSION_SHA256_AMD64 '^[0-9a-f]{64}$')"
      CRAZE_SHA256="$(pin CRAZE_SHA256_AMD64 '^[0-9a-f]{64}$')" ;;
    arm64)
      ROOST_SESSION_SHA256="$(pin ROOST_SESSION_SHA256_ARM64 '^[0-9a-f]{64}$')"
      CRAZE_SHA256="$(pin CRAZE_SHA256_ARM64 '^[0-9a-f]{64}$')" ;;
  esac
fi

export DEBIAN_FRONTEND=noninteractive
# unattended-upgrades runs on a fresh VM; wait for its dpkg lock, not fail.
# A download from the regional mirror can stall on an open connection (seen
# on a real bake: no byte for minutes, no timeout), so each try is capped
# and retried; apt keeps what it already fetched.
apt_get() {
  local try
  for try in 1 2 3; do
    if timeout --kill-after=10 600 apt-get -o DPkg::Lock::Timeout=600 -o Dpkg::Options::=--force-confold \
      -o Acquire::Retries=3 -o Acquire::http::Timeout=60 -o Acquire::https::Timeout=60 "$@"; then
      return 0
    fi
    echo "provision: apt-get $* failed (try ${try} of 3)" >&2
    # A killed run can leave dpkg half-configured.
    dpkg --configure -a || true
    sleep 5
  done
  return 1
}

if [ "${live_systemd}" = yes ] && command -v cloud-init >/dev/null 2>&1; then
  log "waiting for cloud-init to finish"
  # Exit 2 is "done with recoverable errors"; anything else is a real failure.
  rc=0; cloud-init status --wait >/dev/null || rc=$?
  [ "${rc}" -eq 0 ] || [ "${rc}" -eq 2 ] || die "cloud-init status --wait exited ${rc}"
fi

# ---------------------------------------------------------------------------
# Packages
# ---------------------------------------------------------------------------
log "installing base packages"
apt_get update
# firecracker/Dockerfile's list, minus systemd/dbus (stock Ubuntu has them)
# and systemd-timesyncd (the GCE image keeps its own time sync), plus gnupg
# for the repository keys and unattended-upgrades (kept on; see below).
apt_get install -y --no-install-recommends \
  sudo zip unzip ca-certificates hostname gnupg \
  tmux git curl wget neovim jq ncurses-term ripgrep tree \
  openssh-server openssh-client \
  iproute2 iputils-ping dnsutils netcat-openbsd \
  build-essential bubblewrap \
  unattended-upgrades

install -m 0755 -d /etc/apt/keyrings

log "adding the GitHub CLI repository"
curl -fsSL --retry 5 --retry-delay 2 https://cli.github.com/packages/githubcli-archive-keyring.gpg \
  -o /usr/share/keyrings/githubcli-archive-keyring.gpg
chmod a+r /usr/share/keyrings/githubcli-archive-keyring.gpg
echo "deb [arch=${arch} signed-by=/usr/share/keyrings/githubcli-archive-keyring.gpg] https://cli.github.com/packages stable main" \
  > /etc/apt/sources.list.d/github-cli.list

log "adding Docker's repository"
curl -fsSL --retry 5 --retry-delay 2 https://download.docker.com/linux/ubuntu/gpg -o /etc/apt/keyrings/docker.asc
chmod a+r /etc/apt/keyrings/docker.asc
printf 'Types: deb\nURIs: https://download.docker.com/linux/ubuntu\nSuites: %s\nComponents: stable\nSigned-By: /etc/apt/keyrings/docker.asc\n' \
  "${codename}" > /etc/apt/sources.list.d/docker.sources

if [ "${variant}" = extensions ]; then
  # stridelabs serves a BINARY key, so it must be named .gpg (see the note in
  # firecracker/Dockerfile).
  log "adding the stridelabs repository"
  curl -fsSL --retry 5 --retry-delay 2 https://apt.stridelabs.ai/pubkey.gpg -o /etc/apt/keyrings/stridelabs.gpg
  chmod a+r /etc/apt/keyrings/stridelabs.gpg
  printf 'Types: deb\nURIs: https://apt.stridelabs.ai\nSuites: noble\nComponents: main\nSigned-By: /etc/apt/keyrings/stridelabs.gpg\n' \
    > /etc/apt/sources.list.d/stridelabs.sources
fi

apt_get update
log "installing gh and Docker"
# Docker keeps its default storage driver: the root disk is ext4, so it
# picks overlayfs (firecracker/daemon.json forces vfs for the microVM disk).
# buildx is added so `docker build` uses BuildKit, as Docker Desktop does.
apt_get install -y --no-install-recommends \
  gh docker-ce docker-ce-cli containerd.io docker-compose-plugin docker-buildx-plugin
install -m 0755 -d /etc/docker
install -m 0644 "${here}/daemon.json" /etc/docker/daemon.json

if [ "${variant}" = extensions ]; then
  log "installing strix and prox"
  apt_get install -y --no-install-recommends strix prox

  tmpd="$(mktemp -d)"
  trap 'rm -rf "${tmpd}"' EXIT

  log "installing roost-session ${ROOST_SESSION_VERSION}"
  curl -fsSL --retry 5 --retry-delay 2 -o "${tmpd}/rs" \
    "https://github.com/charliek/roost/releases/download/v${ROOST_SESSION_VERSION}/roost-session-${ROOST_SESSION_VERSION}-linux-${arch}"
  echo "${ROOST_SESSION_SHA256}  ${tmpd}/rs" | sha256sum -c -
  install -m 0755 "${tmpd}/rs" /usr/bin/roost-session
  if ! /usr/bin/roost-session identify | jq -e ".session_protocol == ${ROOST_SESSION_PROTOCOL} and .app_version == \"${ROOST_SESSION_VERSION}\"" >/dev/null; then
    die "installed roost-session is not ${ROOST_SESSION_VERSION}/protocol ${ROOST_SESSION_PROTOCOL}"
  fi

  log "installing craze ${CRAZE_VERSION}"
  curl -fsSL --retry 5 --retry-delay 2 -o "${tmpd}/craze.tgz" \
    "https://github.com/charliek/craze/releases/download/v${CRAZE_VERSION}/craze_linux_${arch}.tar.gz"
  echo "${CRAZE_SHA256}  ${tmpd}/craze.tgz" | sha256sum -c -
  mkdir "${tmpd}/craze"
  tar -xzf "${tmpd}/craze.tgz" -C "${tmpd}/craze"
  install -m 0755 "${tmpd}/craze/craze" /usr/bin/craze
  got="$(/usr/bin/craze --version)"
  [ "${got}" = "${CRAZE_VERSION}" ] || die "installed craze reports '${got}', want ${CRAZE_VERSION}"
fi

# Security updates stay on: the stock image enables unattended-upgrades for
# the -security pocket. Write the switch explicitly so the image does not
# depend on that default.
printf '%s\n' \
  'APT::Periodic::Update-Package-Lists "1";' \
  'APT::Periodic::Unattended-Upgrade "1";' \
  > /etc/apt/apt.conf.d/20auto-upgrades

# ---------------------------------------------------------------------------
# The shed user (UID 1000, passwordless sudo, docker group)
# ---------------------------------------------------------------------------
log "creating the shed user"
getent group docker >/dev/null 2>&1 || groupadd --system docker
holder="$(getent passwd 1000 | cut -d: -f1 || true)"
case "${holder}" in
  ""|shed) ;;
  ubuntu) userdel -r ubuntu 2>/dev/null || userdel ubuntu ;;
  *) die "UID 1000 belongs to '${holder}'; the shed user needs it" ;;
esac
if ! id shed >/dev/null 2>&1; then
  useradd -m -s /bin/bash -u 1000 -G docker shed
else
  usermod -a -G docker shed
fi
echo 'shed ALL=(ALL) NOPASSWD:ALL' > /etc/sudoers.d/shed
chmod 0440 /etc/sudoers.d/shed
# Login-shell PATH; .bashrc for interactive shells.
# shellcheck disable=SC2016 # literal $PATH, expanded at login
printf '%s\n' \
  'export PATH="/home/shed/.local/bin:$PATH"' \
  '[ -f ~/.bashrc ] && . ~/.bashrc' \
  > /home/shed/.bash_profile
chown shed:shed /home/shed/.bash_profile
install -d -o shed -g shed -m 0755 /var/log/shed
install -d -o shed -g shed -m 0700 /home/shed/.ssh
# Shed-owned XDG/user dirs (#410), as firecracker/Dockerfile.
install -d -o shed -g shed -m 0755 /home/shed/.local /home/shed/.local/bin /home/shed/.local/share \
  /home/shed/.local/state /home/shed/.config /home/shed/.cache
if ! grep -q '__git_prompt()' /home/shed/.bashrc 2>/dev/null; then
  cat >> /home/shed/.bashrc <<'PROMPT_EOF'
__git_prompt() {
    local branch
    branch=$(git symbolic-ref --short HEAD 2>/dev/null) || return
    local dirty=""
    [ -n "$(git status --porcelain 2>/dev/null)" ] && dirty=$'\001\033[31m\002✗'
    printf " \001\033[01;32m\002(%s)%s\001\033[00m\002" "$branch" "$dirty"
}
export PS1='\[\033[1;36m\][\[\033[1;32m\]shed:${SHED_NAME:-dev}\[\033[1;36m\]]\[\033[0m\] \[\033[01;34m\]\w\[\033[00m\]$(__git_prompt) \$ '
PROMPT_EOF
  chown shed:shed /home/shed/.bashrc
fi

printf '%s\n' \
  'set -g mouse on' \
  'set -g history-limit 50000' \
  'set -g focus-events on' \
  'set -sg escape-time 10' \
  'set -g default-terminal "tmux-256color"' \
  'set -ga terminal-overrides ",*256col*:Tc"' \
  > /etc/tmux.conf

install -d -m 0755 /etc/ssh/ssh_config.d
printf '%s\n' \
  'Host *' \
  '    IgnoreUnknown UseKeychain,AddKeysToAgent' \
  > /etc/ssh/ssh_config.d/10-macos-compat.conf

# ---------------------------------------------------------------------------
# The shed-link user: the server's only login
# ---------------------------------------------------------------------------
# A system user with a real shell (sshd runs session commands through it), a
# '*' password field (not '!', which sshd may read as a locked account), a
# root-owned home it cannot write, and no sudo beyond the agent installer.
log "creating the shed-link user"
getent group shed-link >/dev/null 2>&1 || groupadd --system shed-link
if ! id shed-link >/dev/null 2>&1; then
  useradd --system --gid shed-link --shell /bin/sh --home-dir /var/lib/shed-link --no-create-home shed-link
fi
usermod --password '*' --shell /bin/sh shed-link
install -d -o root -g shed-link -m 0750 /var/lib/shed-link

# ---------------------------------------------------------------------------
# sshd policy
# ---------------------------------------------------------------------------
# sshd keeps the first value it reads, and Ubuntu's sshd_config includes
# sshd_config.d/*.conf before its own lines, so 10- wins over the stock
# 50-cloud-init and 60-cloudimg drop-ins.
log "writing the sshd policy"
install -d -m 0755 /etc/ssh/sshd_config.d
cat > /etc/ssh/sshd_config.d/10-shed.conf <<'SSHD_EOF'
# shed remote guest, layout 1. Owned by gce/provision.sh; the lines below
# are guest.SSHDConfig verbatim (internal/remote/guest/sshd.go).
PermitRootLogin no
PasswordAuthentication no
KbdInteractiveAuthentication no
AllowUsers shed-link
HostKey /etc/ssh/ssh_host_ed25519_key
AuthorizedKeysFile /etc/ssh/shed-link/authorized_keys
AllowStreamLocalForwarding local
AllowTcpForwarding local
PermitOpen [/run/shed-agent/1024.sock]:* [/run/shed-agent/1026.sock]:* [/run/shed-agent/1028.sock]:*
AllowAgentForwarding no
X11Forwarding no
PermitTunnel no
PermitTTY no
PermitUserRC no
SSHD_EOF
chmod 0644 /etc/ssh/sshd_config.d/10-shed.conf
# Only the boot unit writes the key file.
install -d -o root -g root -m 0755 /etc/ssh/shed-link

# Check the merged config parses, with a throwaway host key (the image bakes
# none; the boot unit makes the real one on first boot).
checkkey="$(mktemp -d)"
ssh-keygen -q -t ed25519 -N '' -C '' -f "${checkkey}/k"
install -d -m 0755 /run/sshd
if ! /usr/sbin/sshd -t -h "${checkkey}/k"; then
  rm -rf "${checkkey}"
  die "sshd rejects the merged configuration"
fi
rm -rf "${checkkey}"

# ---------------------------------------------------------------------------
# Agent, boot unit, install helper, layout
# ---------------------------------------------------------------------------
log "installing the agent and its units"
install -o root -g root -m 0755 "${agent}" /usr/local/bin/shed-agent
install -o root -g root -m 0755 "${here}/shed-identity.sh" /usr/local/sbin/shed-identity
install -o root -g root -m 0755 "${here}/shed-agent-install.sh" /usr/local/sbin/shed-agent-install
install -o root -g root -m 0644 "${here}/shed-identity.service" /etc/systemd/system/shed-identity.service
install -o root -g root -m 0644 "${here}/shed-agent.service" /etc/systemd/system/shed-agent.service

printf '%s\n' 'shed-link ALL=(root) NOPASSWD: /usr/local/sbin/shed-agent-install' > /etc/sudoers.d/shed-link
chmod 0440 /etc/sudoers.d/shed-link
visudo -c -q -f /etc/sudoers.d/shed-link || die "sudoers rule for shed-link does not parse"
visudo -c -q || die "sudoers does not parse"

install -d -o root -g root -m 0755 /etc/shed
printf '%s\n' "${SHED_LAYOUT}" > /etc/shed/layout
chmod 0644 /etc/shed/layout

# ---------------------------------------------------------------------------
# Cloud boot agents
# ---------------------------------------------------------------------------
# cloud-init: the boot unit owns the hostname and the one host key, and no
# default user is wanted (the shed user is baked; the server logs in as
# shed-link). Without this, a fresh instance's cloud-init would make an
# `ubuntu` user and generate rsa/ecdsa/ed25519 host keys of its own.
if [ -d /etc/cloud/cloud.cfg.d ]; then
  log "configuring cloud-init"
  cat > /etc/cloud/cloud.cfg.d/99-shed.cfg <<'CLOUD_EOF'
# shed remote guest: gce/provision.sh. shed-identity.service sets the
# hostname and creates the only host key on every boot.
preserve_hostname: true
manage_etc_hosts: false
ssh_deletekeys: false
ssh_genkeytypes: []
users: []
CLOUD_EOF
fi

# Google guest agent: kept, with one feature turned off. On a new instance it
# would regenerate every host key after the boot unit created and published
# the ed25519 one, and the server would then see a key that does not match
# the claim (guest layout 1: the boot unit owns the host key). Its account and
# startup-script features are left on: AllowUsers shed-link stops any account
# it makes from logging in, and the server never sets ssh-keys or
# startup-script metadata.
log "configuring the Google guest agent"
cfg=/etc/default/instance_configs.cfg
if [ ! -f "${cfg}" ]; then
  printf '[InstanceSetup]\nset_host_keys = false\n' > "${cfg}"
elif grep -q '^set_host_keys' "${cfg}"; then
  sed -i 's/^set_host_keys.*/set_host_keys = false/' "${cfg}"
elif grep -q '^\[InstanceSetup\]' "${cfg}"; then
  sed -i 's/^\[InstanceSetup\]$/[InstanceSetup]\nset_host_keys = false/' "${cfg}"
else
  printf '\n[InstanceSetup]\nset_host_keys = false\n' >> "${cfg}"
fi
chmod 0644 "${cfg}"

# Port 22 is the only listener off loopback (guest layout 1): turn off
# resolved's LLMNR and mDNS responders, which would listen on 5355 and 5353.
install -d -m 0755 /etc/systemd/resolved.conf.d
printf '[Resolve]\nLLMNR=no\nMulticastDNS=no\n' > /etc/systemd/resolved.conf.d/10-shed.conf
chmod 0644 /etc/systemd/resolved.conf.d/10-shed.conf

# ---------------------------------------------------------------------------
# Units
# ---------------------------------------------------------------------------
log "enabling units"
units=(shed-identity.service shed-agent.service docker.service containerd.service unattended-upgrades.service)
if [ "${live_systemd}" = yes ]; then
  systemctl daemon-reload
  systemctl enable "${units[@]}"
  systemctl try-restart systemd-resolved.service
else
  SYSTEMD_OFFLINE=1 systemctl enable "${units[@]}"
fi

log "done (variant ${variant}, arch ${arch}, layout ${SHED_LAYOUT})"
