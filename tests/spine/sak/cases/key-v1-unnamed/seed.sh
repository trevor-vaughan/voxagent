useradd -m alice
mkdir -m 700 /home/alice/.ssh
printf '%s\n' '1024 35 777' 'ssh-rsa AAAAB3NzaC1yc2EAAAADAQABAAABAQC7vbqajDw4o6gJy8UtmsBJKXe7JvPJ5mVGrTxQ1w8aXx1 alice@laptop' >/home/alice/.ssh/authorized_keys
chown -R alice:alice /home/alice/.ssh
