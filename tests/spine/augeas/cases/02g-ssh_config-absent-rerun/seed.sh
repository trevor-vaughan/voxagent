printf 'Host *\n\tCompression yes\n\tCompressionLevel 6\n\tForwardX11 yes\n' > /etc/ssh/ssh_config
/tmp/papply >/tmp/seed-apply.out 2>&1 || true
