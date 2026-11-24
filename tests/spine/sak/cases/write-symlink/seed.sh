useradd -m alice
mkdir -m 700 /home/alice/.ssh
echo 'ssh-rsa AAAAB3NzaC1yc2EAAAADAQABAAABAQC7vbqajDw4o6gJy8UtmsBJKXe7JvPJ5mVGrTxQ1w8aXx1 alice@laptop' > /etc/ak_target
chown root:root /etc/ak_target; chmod 600 /etc/ak_target
ln -s /etc/ak_target /home/alice/.ssh/authorized_keys
chown -R alice:alice /home/alice/.ssh
