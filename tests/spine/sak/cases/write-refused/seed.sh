useradd -m alice
mkdir -m 700 /home/alice/.ssh
echo 'ssh-rsa AAAAB3NzaC1yc2EAAAADAQABAAABAQC7vbqajDw4o6gJy8UtmsBJKXe7JvPJ5mVGrTxQ1w8aXx1 alice@laptop' > /home/alice/.ssh/authorized_keys
chown alice:alice /home/alice/.ssh; chown root:root /home/alice/.ssh/authorized_keys; chmod 600 /home/alice/.ssh/authorized_keys
