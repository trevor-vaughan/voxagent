mkdir -p /srv/ra/d/e
printf 'a\n' >/srv/ra/a
printf 'b\n' >/srv/ra/d/b
printf 'c\n' >/srv/ra/d/e/c
find /srv/ra -exec touch -h -d '2001-01-01 00:00:00' {} +
