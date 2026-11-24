printf '#PermitRootLogin prohibit-password\nUsePAM no\nAuthorizedKeysFile\t.ssh/authorized_keys\nSubsystem\tsftp\t/usr/libexec/openssh/sftp-server\n' > /etc/ssh/sshd_config
/tmp/papply >/tmp/seed-apply.out 2>&1 || true
