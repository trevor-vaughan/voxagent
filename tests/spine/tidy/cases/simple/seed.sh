mkdir -p /srv/s
printf 'one\n' >/srv/s/one
printf 'two\n' >/srv/s/two
find /srv/s -exec touch -h -d '2001-01-01 00:00:00' {} +
