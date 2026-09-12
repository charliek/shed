set -u
if [ -e /home/shed/.local/bin/roost-session.bak.4242 ] || [ -L /home/shed/.local/bin/roost-session.bak.4242 ]; then mv -f -- /home/shed/.local/bin/roost-session.bak.4242 /home/shed/.local/bin/roost-session && printf '%s\n' restored; fi
