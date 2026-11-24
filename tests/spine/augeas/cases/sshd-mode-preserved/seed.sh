printf 'PermitRootLogin yes\nSubsystem sftp /usr/libexec/openssh/sftp-server\n' > /etc/ssh/sshd_config
chown nobody:nobody /etc/ssh/sshd_config
chmod 0600 /etc/ssh/sshd_config
