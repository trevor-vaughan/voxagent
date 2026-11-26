mkdir -p /srv/mx
for f in a b c d e; do printf '%s\n' "$f" >"/srv/mx/$f"; done
find /srv/mx -exec touch -h -d '2001-01-01 00:00:00' {} +
