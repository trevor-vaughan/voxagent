mkdir -p /srv/r1/sub
printf 'a\n' >/srv/r1/a.log
printf 'k\n' >/srv/r1/keep.txt
printf 'd\n' >/srv/r1/sub/deep.log
find /srv/r1 -exec touch -h -d '2001-01-01 00:00:00' {} +
