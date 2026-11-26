mkdir -p /srv/sz
head -c 2048 /dev/zero >/srv/sz/big
head -c 1024 /dev/zero >/srv/sz/exact
printf 'small\n' >/srv/sz/small
find /srv/sz -exec touch -h -d '2001-01-01 00:00:00' {} +
