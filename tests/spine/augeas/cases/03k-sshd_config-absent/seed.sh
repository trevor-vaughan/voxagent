printf 'PermitRootLogin yes\nUsePrivilegeSeparation sandbox\nMatch User bob\n\tX11Forwarding no\nMatch Host example.net User root\n\tPasswordAuthentication yes\n' > /etc/ssh/sshd_config
