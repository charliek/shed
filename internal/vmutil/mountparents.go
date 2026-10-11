package vmutil

import (
	"path"

	"github.com/charliek/shed/internal/config"
)

// mountParentsScript is the root prelude WithMountParents runs ahead of a
// mount. POSIX sh; its data arrives only as positional parameters
// ($1 = target, $2 = home, $3 = user, then the wrapped mount argv) and is
// never interpolated into code. See WithMountParents for the rules.
const mountParentsScript = `set -f                                   # no globbing on the split components
t=$1 home=$2 u=$3
case "$t" in "$home"/*) ;; *) shift 3; exec "$@";; esac   # outside $HOME: untouched, the mount's own mkdir handles it
if [ -L "$home" ] || [ ! -d "$home" ]; then shift 3; exec "$@"; fi
hd=$(stat -c %d "$home" 2>/dev/null) rd=$(stat -c %d / 2>/dev/null)
[ -n "$rd" ] && [ "$hd" = "$rd" ] || { shift 3; exec "$@"; }   # $HOME inside some other mount (or uninspectable): its subtree is not ours to touch
p=$home; rel=${t#"$home"/}; IFS=/
for c in $rel; do
  case "$c" in ''|.|..) break;; esac     # defensive; Go already path.Clean'd the target
  p="$p/$c"
  [ -L "$p" ] && break                   # never walk through (or chown) a symlink
  if [ ! -e "$p" ]; then
    install -d -o "$u" -g "$u" -m 0755 "$p" || { printf 'shed: could not create %s\n' "$p" >&2; break; }
    continue
  fi
  [ -d "$p" ] || break                   # a file in the way: the mount fails exactly as today
  mountpoint -q "$p"; rc=$?
  [ "$rc" -eq 32 ] || break              # 0 = mounted (its subtree is the host's), else unknown: stop
  if [ "$p" != "$t" ] && [ "$(stat -c %u "$p")" = 0 ]; then
    chown "$u:$u" "$p" || printf 'shed: could not repair %s\n' "$p" >&2   # one-time repair of broken sheds
  fi
done
unset IFS; shift 3; exec "$@"
`

// WithMountParents wraps a guest mount command so the directories the mount
// needs under the shed user's home are owned by that user, not root (#410).
// It returns
//
//	sudo sh -c <prelude> sh <path.Clean(target)> <config.HomePath> <config.ShedUser> <cmd...>
//
// so each mount stays a single agent exec: the prelude runs as root, then
// execs cmd. cmd is the original mount argv WITHOUT sudo (the prelude is
// already root) and must be non-empty; its argv and exit status pass through
// unchanged.
//
// The prelude walks target's components below the home directory:
//   - It never touches the home directory itself or anything above it, and a
//     target outside the home directory (or a home that is a symlink or not a
//     directory) is left entirely to cmd.
//   - It never walks a home directory that lies inside another mount — one at
//     the home directory or above it, such as a server mount at /home/shed or
//     /home: unless home is on the same filesystem as / (equal st_dev), or if
//     either device cannot be read, cmd runs untouched. Below home, the walk
//     itself stops at mounts (see below).
//   - A missing component is created as the shed user (install -d, 0755) —
//     including the leaf, so the mount's own root `mkdir -p` finds nothing
//     left to create.
//   - An existing root-owned intermediate directory is chowned to the shed
//     user: a one-time repair of sheds the root `mkdir -p` already broke, so
//     existing sheds are repaired on their next start. The leaf is never
//     chowned; the mount is about to cover it.
//   - The walk stops at the first symlink, non-directory, mountpoint, or
//     component whose mount status cannot be read: nothing at or below one of
//     those is created or chowned, so the walk never follows a link out of
//     the home directory or into a mounted subtree. The device test above
//     catches a home inside a 9P/virtiofs (host) mount, whose st_dev differs
//     from the guest root's; a bind mount of the guest's own filesystem keeps
//     the root's st_dev and is not detected — it holds guest data, not host
//     data, so the walk acts on the guest only.
//
// Best effort: a prelude failure (a failed install stops the walk; a failed
// chown is skipped) prints a warning to stderr and cmd then runs exactly as
// it did before this prelude existed. It never makes a mount fail that would
// succeed without it.
//
// Assumptions:
//   - The target's ancestors don't change while the prelude runs. The guest
//     user has passwordless sudo, so this is not a privilege boundary; the
//     rules exist to protect host directories from accidents, not to defend
//     against a hostile guest.
//   - The guest's home directory lives on its root filesystem: both images
//     boot one overlay root, so a home whose st_dev differs from /'s is inside
//     a mount.
//   - The guest has GNU `stat -c` (%u, %d), util-linux `mountpoint` (exit 0
//     for a mountpoint, 32 for not one, 1 for an error), and coreutils
//     `install` — the Ubuntu 24.04 images do. A missing `mountpoint` exits
//     127, which stops the walk: the old behavior, never worse.
func WithMountParents(target string, cmd []string) []string {
	argv := []string{"sudo", "sh", "-c", mountParentsScript, "sh", path.Clean(target), config.HomePath, config.ShedUser}
	return append(argv, cmd...)
}
