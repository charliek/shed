if out=$(/home/shed/.local/bin/roost-session identify 2>/dev/null) && [ -n "$out" ]; then printf '%s\0%s\0' /home/shed/.local/bin/roost-session "$out"; exit 0; fi
printf '%s\0%s\0' /home/shed/.local/bin/roost-session ''
exit 0
