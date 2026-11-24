printf 'vm.swappiness = 30\n' > /etc/sysctl.conf
sh /tmp/sysctl-shim.sh
sysctl -w net.ipv4.ip_forward=1 >/dev/null
rm -f /tmp/sysctl.calls
