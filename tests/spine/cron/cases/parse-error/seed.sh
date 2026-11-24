printf '# Puppet Name: a\n1 * * * * /bin/a\nthis is garbage\n' >/var/spool/cron/root; chmod 600 /var/spool/cron/root
