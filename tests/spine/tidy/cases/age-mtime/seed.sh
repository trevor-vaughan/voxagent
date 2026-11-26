mkdir -p /srv/fw
printf 'a\n' >/srv/fw/fw_a
printf 'k\n' >/srv/fw/keep
find /srv/fw -exec touch -h -d '2001-01-01 00:00:00' {} +
printf 'b\n' >/srv/fw/fw_b
