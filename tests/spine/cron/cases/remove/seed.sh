printf '# Puppet Name: gone\n1 * * * * /bin/gone\n# Puppet Name: kept\n2 * * * * /bin/kept\n' | crontab -
