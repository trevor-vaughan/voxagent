mkdir -p /srv/ms
printf 'a\n' >/srv/ms/a
find /srv/ms -exec touch -h -d '2001-01-01 00:00:00' {} +
