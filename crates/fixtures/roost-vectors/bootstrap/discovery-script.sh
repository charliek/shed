printf '%s\0' "$(uname -s 2>/dev/null)"
printf '%s\0' "$(uname -m 2>/dev/null)"
printf '%s\0' "${HOME:-}"
if [ -n "${HOME:-}" ]; then p="$HOME/.local/bin/roost-session"; [ -f "$p" ] && [ -x "$p" ] && printf '%s\0' "$p"; fi
p=$(command -v roost-session 2>/dev/null) || p=; case "$p" in /*) [ -f "$p" ] && [ -x "$p" ] && printf '%s\0' "$p";; esac
p="/usr/bin/roost-session"; [ -f "$p" ] && [ -x "$p" ] && printf '%s\0' "$p"
p="/home/linuxbrew/.linuxbrew/bin/roost-session"; [ -f "$p" ] && [ -x "$p" ] && printf '%s\0' "$p"
if [ -n "${HOME:-}" ]; then p="$HOME/.nix-profile/bin/roost-session"; [ -f "$p" ] && [ -x "$p" ] && printf '%s\0' "$p"; fi
if [ -n "${USER:-}" ]; then p="/etc/profiles/per-user/$USER/bin/roost-session"; [ -f "$p" ] && [ -x "$p" ] && printf '%s\0' "$p"; fi
p="/nix/var/nix/profiles/default/bin/roost-session"; [ -f "$p" ] && [ -x "$p" ] && printf '%s\0' "$p"
p="/run/current-system/sw/bin/roost-session"; [ -f "$p" ] && [ -x "$p" ] && printf '%s\0' "$p"
exit 0
