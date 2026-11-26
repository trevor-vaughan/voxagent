mkdir -p /srv/me
printf 'old\n' >/srv/me/by-title
printf 'p\n' >/srv/me/by-path
printf 'x\n' >/srv/me/other
find /srv/me -exec touch -h -d '2001-01-01 00:00:00' {} +
