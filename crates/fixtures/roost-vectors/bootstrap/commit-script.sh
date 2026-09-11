set -eu
[ -f /home/shed/.local/bin/roost-session.tmp.4242 ]
[ ! -d /home/shed/.local/bin/roost-session ]
chmod -- 755 /home/shed/.local/bin/roost-session.tmp.4242
if [ -e /home/shed/.local/bin/roost-session ] || [ -L /home/shed/.local/bin/roost-session ]; then mv -- /home/shed/.local/bin/roost-session /home/shed/.local/bin/roost-session.bak.4242; fi
mv -- /home/shed/.local/bin/roost-session.tmp.4242 /home/shed/.local/bin/roost-session
