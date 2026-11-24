printf 'AllowUsers root\nAuthorizedKeysFile\t.ssh/authorized_keys\nCiphers aes128-ctr,aes192-ctr\nSubsystem\tsftp\t/usr/libexec/openssh/sftp-server\n' > /etc/ssh/sshd_config
