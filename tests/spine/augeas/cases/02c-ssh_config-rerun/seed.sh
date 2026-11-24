printf 'Host *\n\tCompression yes\n\tForwardX11 yes\nHost foo\n\tCompression yes\n' > /etc/ssh/ssh_config
/tmp/papply >/tmp/seed-apply.out 2>&1 || true
