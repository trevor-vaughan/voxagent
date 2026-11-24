useradd -m alice; useradd -m bob
for u in alice bob; do mkdir -m 700 /home/$u/.ssh; chown $u:$u /home/$u/.ssh; done
printf 'ssh-rsa AAAAB3NzaC1yc2EAAAADAQABAAABAQC7vbqajDw4o6gJy8UtmsBJKXe7JvPJ5mVGrTxQ1w8aXx1 shared@x\nssh-rsa AAAAB3NzaC1yc2EAAAADAQABAAABAQDKtwEp2Qo0Jn6yS9X0q8yTzrBH0mUL2v8c9aF3dQ2k alice-other@x\n' > /home/alice/.ssh/authorized_keys
echo 'ssh-rsa AAAAC3NzaC1lZDI1NTE5AAAAIOMqqnkVzrm0SdG6UOoqKLsabgH5C9okWi0dh2l9GKJl bobs@x' > /home/bob/.ssh/authorized_keys
chown alice:alice /home/alice/.ssh/authorized_keys; chown bob:bob /home/bob/.ssh/authorized_keys
chmod 600 /home/*/.ssh/authorized_keys
