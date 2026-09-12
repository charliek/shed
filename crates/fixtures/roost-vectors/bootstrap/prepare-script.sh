set -eu
[ -n "${HOME:-}" ] || { printf '%s\n' 'roost bootstrap: HOME is not set' >&2; exit 1; }
case "${HOME:-}" in /*) ;; *) printf '%s\n' 'roost bootstrap: HOME is not an absolute path' >&2; exit 1;; esac
dest="$HOME/.local/bin/roost-session"
mkdir -p "${dest%/*}"
for stale in "$dest".tmp.*; do suffix="${stale##*.tmp.}"; case "$suffix" in ''|*[!0-9]*) continue;; esac; [ -f "$stale" ] || continue; rm -f -- "$stale"; done
tmp="${dest}.tmp.$$"
[ ! -L "$tmp" ] || { printf '%s\n' 'roost bootstrap: the staged path is a symlink' >&2; exit 1; }
(set -C; : > "$tmp") || { printf '%s\n' 'roost bootstrap: the staged path already exists' >&2; exit 1; }
printf '%s\0%s\0' "$tmp" "$dest"
