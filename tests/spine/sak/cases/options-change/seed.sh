useradd -m alice
mkdir -m 700 /home/alice/.ssh
cat > /home/alice/.ssh/authorized_keys <<'X'
# my comment

ssh-rsa AAAAB3NzaC1yc2EAAAADAQABAAABAQC7vbqajDw4o6gJy8UtmsBJKXe7JvPJ5mVGrTxQ1w8aXx1 alice@laptop
ssh-rsa AAAAB3NzaC1yc2EAAAADAQABAAABAQDKtwEp2Qo0Jn6yS9X0q8yTzrBH0mUL2v8c9aF3dQ2k
ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOMqqnkVzrm0SdG6UOoqKLsabgH5C9okWi0dh2l9GKJl
command="echo hi",no-pty ssh-rsa AAAAB3NzaC1yc2EAAAADAQABAAABAQDe4y5PtUQ1m0b8Zq3gS1wX7vN2jH6kLpR9sT4uV bob@x
1024 35 123456789012345678901234567890 legacy@host
X
chown -R alice:alice /home/alice/.ssh
chmod 640 /home/alice/.ssh/authorized_keys
