printf '# Puppet Name: job\n1 * * * * /bin/root-job\n' | crontab -
printf '# Puppet Name: job\n2 * * * * /bin/old\n' | crontab -u bin -
