printf '%s\n' 'old ssh-rsa OOOO' >/etc/ssh/ssh_known_hosts
chmod 0600 /etc/ssh/ssh_known_hosts
chown nobody:nobody /etc/ssh/ssh_known_hosts
