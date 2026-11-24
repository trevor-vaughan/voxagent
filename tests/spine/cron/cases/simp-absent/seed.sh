printf '# Puppet Name: puppetagent\n*/30 * * * * /opt/puppetlabs/bin/puppet agent --onetime\n7 * * * * /bin/unmanaged\n' | crontab -
