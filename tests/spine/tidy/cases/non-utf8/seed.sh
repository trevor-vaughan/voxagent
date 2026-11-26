mkdir -p /srv/nu
printf 'g\n' >/srv/nu/good
printf 'b\n' >"/srv/nu/$(printf 'bad\377')"
find /srv/nu -exec touch -h -d '2001-01-01 00:00:00' {} +
