mkdir -p /srv/rd/d1/d2 /srv/rd/d3
printf 'f\n' >/srv/rd/d1/d2/f1
printf 's\n' >/srv/rd/d3/stay
printf 'k\n' >/srv/rd/keep
find /srv/rd -exec touch -h -d '2001-01-01 00:00:00' {} +
