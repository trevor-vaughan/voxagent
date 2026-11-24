printf '192.0.2.3\nface\ndead   beef  # x\n# my comment\n\n   \n127.0.0.1\tlocalhost\nthis is not a host line\n192.0.2.1\th1  a   b   # trailing comment\n  indented line\n\t# tab comment\n# HEADER fake\n192.0.2.2 h2\n' >/etc/hosts
chmod 0640 /etc/hosts
chown nobody:nobody /etc/hosts
