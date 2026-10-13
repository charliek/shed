#!/usr/bin/env bash
# The #109 regression test: /etc/shed/server.yaml is no longer a dpkg conffile,
# so no install, upgrade, reinstall, remove, purge or downgrade of the
# shed-server deb prompts about it or changes an operator's config.
#
# Usage:
#   test-deb-upgrade.sh <shed-server_*_amd64.deb>
#
# Needs Docker and curl on the host, no sudo. Each scenario runs in a fresh
# ubuntu:24.04 container, and every apt-get call runs with stdin closed, as an
# unattended upgrade does. There is no systemd in the container, so the postinst
# stops at its systemd guard; everything this test asserts happens before it.
#
# Baselines, downloaded from the GitHub releases and pinned by sha256:
#   v0.7.10  conffile era, and its packaged template differs from today's, so
#            an edited config made dpkg prompt on the upgrade
#   v0.9.0   the last conffile-era release, for the stuck-host row (T3)
#
# Version order: the deb under test must sort above 0.9.0, or the upgrade rows
# would be downgrades. A goreleaser snapshot is versioned 0.9.0~SNAPSHOT-<sha>,
# which sorts below 0.9.0, so such a deb is re-versioned into a copy
# (<version without its ~suffix>+upgradetest1). Only the Version field of the
# control file changes; the contents and maintainer scripts are the input's.
# A release deb (0.9.1 or later) is tested as it is.
#
# A1 (on the deb itself): conffiles does not list /etc/shed/server.yaml, the
# deb ships /usr/share/shed/server.yaml.default (0644) and owns /etc/shed.
# T1-T10: the rows in row_title below (plan 028 D1). Each row prints every
# apt-get output and one ok/MISS line per assertion; the run ends with a
# PASS/FAIL summary and exits 1 if anything failed.
#
# Env:
#   SHED_DEB_UPGRADE_CACHE  baseline download cache
#                           (default: ${XDG_CACHE_HOME:-$HOME/.cache}/shed/deb-upgrade)
#   SHED_DEB_UPGRADE_IMAGE  container image (default: ubuntu:24.04)
#
# CI: ci.yml's server-deb-upgrade job runs this against the release-snapshot
# job's linux/amd64 deb.

set -euo pipefail

IMAGE="${SHED_DEB_UPGRADE_IMAGE:-ubuntu:24.04}"
RELEASES_URL="https://github.com/charliek/shed/releases/download"
# "<version> <sha256>" for each baseline shed-server_<version>_amd64.deb.
BASELINES=(
  "0.7.10 78ded347c0e5e7e2eb47834119d61d835ae737c2caf327b880d11350c6e8ddb8"
  "0.9.0 fb90307e8f586f80aafbd9befdf4dd911bb87e29c5886db6fbc04f909cd75a0a"
)
ROWS="T1 T2 T3 T4 T5 T6 T7 T8 T9 T10"

# Paths inside the containers.
CFG=/etc/shed/server.yaml
REF=/usr/share/shed/server.yaml.default
OLD=/work/shed-server_0.7.10_amd64.deb
STUCK=/work/shed-server_0.9.0_amd64.deb
NEW=/work/new.deb
NEXT=/work/next.deb

row_title() {
  case "$1" in
    T1) echo "0.7.10, edit the config, upgrade to new" ;;
    T2) echo "T1 with --force-confnew" ;;
    T3) echo "stuck host: 0.7.10, edit, 0.9.0 fails at the prompt, then new" ;;
    T4) echo "reinstall new; a deleted config stays deleted" ;;
    T5) echo "remove new, then install it again" ;;
    T6) echo "purge new" ;;
    T7) echo "purge, then install new again" ;;
    T8) echo "fresh install of new" ;;
    T9) echo "new, then new+1 with a changed template" ;;
    T10) echo "edited new, downgrade to 0.7.10 with --force-confold" ;;
    *) return 1 ;;
  esac
}

die() {
  echo "test-deb-upgrade: $*" >&2
  exit 1
}

# ---------------------------------------------------------------------------
# In-container helpers
# ---------------------------------------------------------------------------

misses=0

# check <description> <command...>: one ok/MISS line; a miss is counted and
# the row carries on, so one run reports every broken assertion.
check() {
  local what=$1
  shift
  if "$@"; then
    echo "  ok:   ${what}"
  else
    echo "  MISS: ${what}"
    misses=$((misses + 1))
  fi
}

# require <description> <command...>: a setup step the rest of the row
# depends on; a miss ends the row.
require() {
  local what=$1
  shift
  if "$@"; then
    echo "  ok:   ${what}"
  else
    echo "  MISS: ${what} (setup failed; the rest of this row was not run)"
    exit 1
  fi
}

not() { ! "$@"; }

# apt_get <apt-get args...>: apt-get -y with stdin closed. The output is kept
# in $LOG and echoed indented. dpkg runs on a pty, so its lines end in CRLF and
# its "Reading database ... N%" progress is CR-separated: the echo drops the
# line-ending CR, then keeps what follows the last CR, as a terminal shows it.
# The exit code lands in $rc.
LOG=/tmp/apt.log
rc=0
apt_get() {
  echo "+ apt-get -y $*"
  if apt-get -y "$@" </dev/null >"${LOG}" 2>&1; then rc=0; else rc=$?; fi
  sed -e 's/\r$//' -e 's/^.*\r//' -e 's/^/    | /' "${LOG}"
  echo "  (exit ${rc})"
}

exit_is() { [ "${rc}" -eq "$1" ]; }
no_prompt() { not grep -Eq "conffile prompt|What would you like to do about it|Configuration file '${CFG}'" "${LOG}"; }
logged() { grep -Fq -- "$1" "${LOG}"; }
pkg_state() { dpkg-query -W -f='${Version} ${Status}' shed-server 2>/dev/null; }
# purged: dpkg has no record of the package, or only a not-installed one.
purged() {
  local s
  s="$(dpkg-query -W -f='${Status}' shed-server 2>/dev/null)" || return 0
  case "${s}" in *" not-installed") return 0 ;; *) return 1 ;; esac
}
state_is() { [ "$(pkg_state)" = "$1" ]; }
cfg_is() { cmp -s "${CFG}" "$1"; }
absent() { [ ! -e "$1" ] && [ ! -L "$1" ]; }
no_dpkg_leftovers() { not compgen -G '/etc/shed/*.dpkg-*' >/dev/null; }
conffile_obsolete() { dpkg-query -W -f='${Conffiles}\n' shed-server | grep -Eq "^ ${CFG} [0-9a-f]+ obsolete$"; }
mode_is() { [ "$(stat -c '%a %U:%G' "$2")" = "$1" ]; }
# in_deb <mode string> <./path>: dpkg-deb -c lists the path, root-owned, with
# that mode.
in_deb() {
  awk -v m="$1" -v p="$2" '$1 == m && $2 == "root/root" && $NF == p { found = 1 } END { exit !found }' /tmp/a1.contents
}

# edit_cfg: an operator edit; the edited bytes are kept for comparison.
EDITED=/tmp/server.yaml.edited
edit_cfg() {
  echo "# operator edit" >>"${CFG}"
  cp "${CFG}" "${EDITED}"
  echo "  (edited ${CFG})"
}

show_conffiles() {
  echo "  dpkg Conffiles:"
  dpkg-query -W -f='${Conffiles}\n' shed-server | sed '/^$/d; s/^/    /'
}

install_new() {
  apt_get install "${NEW}"
  require "fresh install of new exits 0" exit_is 0
}

# repack <src.deb> <dst.deb> <version> [<template line>]: a copy with a new
# Version and, optionally, a line appended to the shipped template (its md5sums
# entry updated to match).
repack() {
  local dir=/tmp/repack
  rm -rf "${dir}"
  dpkg-deb -R "$1" "${dir}"
  sed -i "s|^Version: .*|Version: $3|" "${dir}/DEBIAN/control"
  if [ -n "${4:-}" ]; then
    echo "$4" >>"${dir}${REF}"
    local sum
    sum="$(cd "${dir}" && md5sum "${REF#/}")"
    sed -i "s|^[0-9a-f]*  ${REF#/}\$|${sum}|" "${dir}/DEBIAN/md5sums"
  fi
  dpkg-deb --root-owner-group -b "${dir}" "$2" >/dev/null
  rm -rf "${dir}"
}

# ---------------------------------------------------------------------------
# __prep (in a container, as the host user): A1 on the input deb, then the
# debs under test: /work/new.deb (sorts above 0.9.0) and /work/next.deb (new+1).
# Exit 0: done; 3: done, but A1 failed; anything else: the debs could not be
# prepared.
# ---------------------------------------------------------------------------
prep() {
  local in=/in/input.deb pkg in_ver new_ver next_ver
  pkg="$(dpkg-deb -f "${in}" Package)" || { echo "not a readable deb"; exit 2; }
  in_ver="$(dpkg-deb -f "${in}" Version)"
  echo "deb under test: ${pkg} ${in_ver}"
  [ "${pkg}" = "shed-server" ] || { echo "not a shed-server deb (Package: ${pkg})"; exit 2; }

  echo "A1: the deb's conffiles and contents"
  dpkg-deb -I "${in}" conffiles >/tmp/a1.conffiles 2>/dev/null || : >/tmp/a1.conffiles
  dpkg-deb -c "${in}" >/tmp/a1.contents
  sed 's/^/    conffiles: /' /tmp/a1.conffiles
  check "conffiles does not list ${CFG}" not grep -qx "${CFG}" /tmp/a1.conffiles
  check "ships ${REF} (0644)" in_deb -rw-r--r-- ".${REF}"
  check "owns /etc/shed (0755)" in_deb drwxr-xr-x ./etc/shed/
  local a1_misses=${misses}

  if dpkg --compare-versions "${in_ver}" gt 0.9.0; then
    cp "${in}" "${NEW}"
    new_ver="${in_ver}"
  else
    new_ver="${in_ver%%~*}+upgradetest1"
    repack "${in}" "${NEW}" "${new_ver}"
    echo "Re-versioned ${in_ver} to ${new_ver} so it sorts above 0.9.0 (a copy; contents and maintainer scripts unchanged)."
  fi
  if ! dpkg --compare-versions "${new_ver}" gt 0.9.0; then
    echo "The deb under test (${new_ver}) does not sort above 0.9.0, so the upgrade rows would be downgrades."
    exit 2
  fi

  # new+1 changes the reference copy, which a deb failing A1 may not ship.
  # Without it the rows still run, so the run shows what the regression breaks;
  # T9 then fails on the missing deb.
  if [ "${a1_misses}" -ne 0 ]; then
    echo "new: ${new_ver}; new+1 not built (A1 failed)"
    exit 3
  fi
  next_ver="${new_ver}+next1"
  repack "${NEW}" "${NEXT}" "${next_ver}" "# test-deb-upgrade: a template change in ${next_ver}"
  dpkg --compare-versions "${next_ver}" gt "${new_ver}" || { echo "new+1 (${next_ver}) does not sort above ${new_ver}"; exit 2; }
  echo "new: ${new_ver}; new+1: ${next_ver} (changed template)"
}

# ---------------------------------------------------------------------------
# __row <T#> (in a fresh container, as root): one scenario from D1's matrix.
# ---------------------------------------------------------------------------
row() {
  export DEBIAN_FRONTEND=noninteractive
  local new_ver
  new_ver="$(dpkg-deb -f "${NEW}" Version)"

  case "$1" in
    T1 | T2)
      local force=()
      if [ "$1" = T2 ]; then force=(-o Dpkg::Options::=--force-confnew); fi
      apt_get install "${OLD}"
      require "install 0.7.10 exits 0" exit_is 0
      edit_cfg
      apt_get install "${force[@]}" "${NEW}"
      show_conffiles
      check "upgrade exits 0" exit_is 0
      check "no conffile prompt" no_prompt
      check "installed and configured" state_is "${new_ver} install ok installed"
      check "config byte-identical to the edit" cfg_is "${EDITED}"
      check "no /etc/shed/*.dpkg-* left" no_dpkg_leftovers
      check "dpkg marks ${CFG} obsolete" conffile_obsolete
      ;;
    T3)
      apt_get install "${OLD}"
      require "install 0.7.10 exits 0" exit_is 0
      edit_cfg
      apt_get install "${STUCK}"
      check "0.9.0 fails at the conffile prompt (exit 100)" exit_is 100
      check "0.9.0 is left unpacked" state_is "0.9.0 install ok unpacked"
      check "0.9.0 left ${CFG}.dpkg-new" test -e "${CFG}.dpkg-new"
      apt_get install "${NEW}"
      check "new exits 0" exit_is 0
      check "no conffile prompt" no_prompt
      check "installed and configured" state_is "${new_ver} install ok installed"
      check "config byte-identical to the edit" cfg_is "${EDITED}"
      check "${CFG}.dpkg-new removed" absent "${CFG}.dpkg-new"
      check "postinst logged the removal" logged "Removed ${CFG}.dpkg-new"
      ;;
    T4)
      install_new
      edit_cfg
      apt_get install --reinstall "${NEW}"
      check "reinstall exits 0" exit_is 0
      check "config unchanged" cfg_is "${EDITED}"
      rm -f "${CFG}"
      echo "  (deleted ${CFG})"
      apt_get install --reinstall "${NEW}"
      check "reinstall exits 0" exit_is 0
      check "the deleted config stays deleted" absent "${CFG}"
      check "postinst names the reference copy" logged "sudo install -m 0644 ${REF} ${CFG}"
      ;;
    T5)
      install_new
      edit_cfg
      apt_get remove shed-server
      check "remove exits 0" exit_is 0
      check "config survives remove" cfg_is "${EDITED}"
      apt_get install "${NEW}"
      check "install after remove exits 0" exit_is 0
      check "installed and configured" state_is "${new_ver} install ok installed"
      check "config survives the reinstall" cfg_is "${EDITED}"
      ;;
    T6)
      install_new
      echo "dummy host key" >/etc/shed/host_key
      apt_get purge shed-server
      check "purge exits 0" exit_is 0
      check "package purged" purged
      check "config gone" absent "${CFG}"
      check "/etc/shed/host_key survives" test -f /etc/shed/host_key
      ;;
    T7)
      install_new
      apt_get purge shed-server
      require "purge exits 0" exit_is 0
      require "config gone" absent "${CFG}"
      check "the emptied /etc/shed is removed" absent /etc/shed
      apt_get install "${NEW}"
      check "install after purge exits 0" exit_is 0
      check "config recreated, identical to the reference copy" cfg_is "${REF}"
      ;;
    T8)
      apt_get install "${NEW}"
      check "fresh install exits 0" exit_is 0
      check "installed and configured" state_is "${new_ver} install ok installed"
      check "config identical to the reference copy" cfg_is "${REF}"
      check "config is 0644 root:root" mode_is "644 root:root" "${CFG}"
      check "no conffiles entry for ${CFG}" not grep -q "${CFG}" <(dpkg-query -W -f='${Conffiles}\n' shed-server)
      ;;
    T9)
      require "new+1 was built" test -f "${NEXT}"
      local next_ver
      next_ver="$(dpkg-deb -f "${NEXT}" Version)"
      install_new
      edit_cfg
      cp "${REF}" /tmp/ref.before
      apt_get install "${NEXT}"
      check "new+1 exits 0" exit_is 0
      check "no conffile prompt" no_prompt
      check "installed and configured" state_is "${next_ver} install ok installed"
      check "config byte-identical to the edit" cfg_is "${EDITED}"
      # Compared byte for byte with new+1's own copy: a bare "differs from
      # before" would also pass on a missing or truncated reference.
      dpkg-deb -x "${NEXT}" /tmp/next-x
      check "the reference copy is new+1's template" cmp -s "${REF}" "/tmp/next-x${REF}"
      check "new+1's template differs from new's" not cmp -s "/tmp/next-x${REF}" /tmp/ref.before
      ;;
    T10)
      install_new
      edit_cfg
      apt_get install --allow-downgrades -o Dpkg::Options::=--force-confold "${OLD}"
      show_conffiles
      check "downgrade exits 0" exit_is 0
      check "0.7.10 installed and configured" state_is "0.7.10 install ok installed"
      check "config byte-identical to the edit" cfg_is "${EDITED}"
      ;;
    *)
      echo "unknown row: $1"
      exit 2
      ;;
  esac
  [ "${misses}" -eq 0 ]
}

# ---------------------------------------------------------------------------
# Host side
# ---------------------------------------------------------------------------

sha256_of() {
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$1" | cut -d' ' -f1
  else
    shasum -a 256 "$1" | cut -d' ' -f1
  fi
}

host() {
  local input=$1 self cache work entry ver sum f got prc rc a1 r res failed=0
  [ -f "${input}" ] || die "no such file: ${input}"
  command -v docker >/dev/null 2>&1 || die "docker is required"
  command -v curl >/dev/null 2>&1 || die "curl is required"
  input="$(cd "$(dirname "${input}")" && pwd)/$(basename "${input}")"
  self="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/$(basename "${BASH_SOURCE[0]}")"

  cache="${SHED_DEB_UPGRADE_CACHE:-${XDG_CACHE_HOME:-$HOME/.cache}/shed/deb-upgrade}"
  mkdir -p "${cache}"
  work="$(mktemp -d "${TMPDIR:-/tmp}/shed-deb-upgrade.XXXXXX")"
  # shellcheck disable=SC2064  # expand $work now, while it is set
  trap "rm -rf '${work}'" EXIT

  for entry in "${BASELINES[@]}"; do
    read -r ver sum <<<"${entry}"
    f="${cache}/shed-server_${ver}_amd64.deb"
    if [ ! -f "${f}" ] || [ "$(sha256_of "${f}")" != "${sum}" ]; then
      echo "Downloading the v${ver} baseline"
      curl -fsSL --retry 3 -o "${f}.part" "${RELEASES_URL}/v${ver}/shed-server_${ver}_amd64.deb"
      mv "${f}.part" "${f}"
    fi
    got="$(sha256_of "${f}")"
    [ "${got}" = "${sum}" ] || die "v${ver} baseline sha256 is ${got}, pinned ${sum}"
    cp "${f}" "${work}/"
    echo "baseline v${ver}: sha256 ${got} (pinned)"
  done

  docker image inspect "${IMAGE}" >/dev/null 2>&1 || docker pull "${IMAGE}"

  echo
  echo "== prep: A1 and the debs under test"
  if docker run --rm --user "$(id -u):$(id -g)" \
    -v "${self}:/t/test-deb-upgrade.sh:ro" \
    -v "${input}:/in/input.deb:ro" \
    -v "${work}:/work" \
    "${IMAGE}" bash /t/test-deb-upgrade.sh __prep </dev/null; then
    prc=0
  else
    prc=$?
  fi
  case "${prc}" in
    0) a1=PASS ;;
    3) a1=FAIL ;;
    *) die "could not prepare the debs under test (exit ${prc})" ;;
  esac
  # The row containers run apt as root, whose download sandbox user (_apt)
  # must be able to read the debs.
  chmod -R a+rX "${work}"

  local results=""
  for r in ${ROWS}; do
    echo
    echo "== ${r}: $(row_title "${r}")"
    if docker run --rm \
      -v "${self}:/t/test-deb-upgrade.sh:ro" \
      -v "${work}:/work:ro" \
      "${IMAGE}" bash /t/test-deb-upgrade.sh __row "${r}" </dev/null; then
      rc=0
    else
      rc=$?
    fi
    if [ "${rc}" -eq 0 ]; then
      results="${results}${r} PASS"$'\n'
    else
      results="${results}${r} FAIL"$'\n'
      failed=1
    fi
  done

  echo
  echo "== summary ($(basename "${input}"))"
  printf '%-4s %s  %s\n' A1 "${a1}" "conffiles and contents of the deb under test"
  [ "${a1}" = PASS ] || failed=1
  while read -r r res; do
    [ -n "${r}" ] || continue
    printf '%-4s %s  %s\n' "${r}" "${res}" "$(row_title "${r}")"
  done <<<"${results}"

  if [ "${failed}" -ne 0 ]; then
    echo "FAIL: the shed-server deb broke the config contract (#109); see the MISS lines above." >&2
    exit 1
  fi
  echo "PASS: A1 and T1-T10"
}

case "${1:-}" in
  __prep) prep ;;
  __row) row "${2:-}" ;;
  -h | --help) sed -n '2,/^$/s/^# \{0,1\}//p' "$0" ;;
  "") die "usage: $0 <shed-server_*_amd64.deb>" ;;
  *)
    [ "$#" -eq 1 ] || die "usage: $0 <shed-server_*_amd64.deb>"
    host "$1"
    ;;
esac
