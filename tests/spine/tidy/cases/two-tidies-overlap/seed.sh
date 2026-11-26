mkdir -p /srv/o/sub
printf 'x\n' >/srv/o/x.a
printf 'y\n' >/srv/o/sub/y.a
printf 'z\n' >/srv/o/sub/z
printf 'k\n' >/srv/o/keep
find /srv/o -exec touch -h -d '2001-01-01 00:00:00' {} +
