mkdir -p /srv/at
printf 'o\n' >/srv/at/old
find /srv/at -exec touch -h -d '2001-01-01 00:00:00' {} +
printf 'f\n' >/srv/at/fresh
