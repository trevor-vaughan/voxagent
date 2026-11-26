mkdir -p /srv/m
printf 'a\n' >/srv/m/a
printf 'b\n' >/srv/m/b
find /srv/m -exec touch -h -d '2001-01-01 00:00:00' {} +
