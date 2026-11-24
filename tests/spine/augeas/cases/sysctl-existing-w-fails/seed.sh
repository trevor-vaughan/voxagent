printf 'kernel.hostname = old\nvm.swappiness = 5\n' > /etc/sysctl.conf
sh /tmp/sysctl-shim.sh
