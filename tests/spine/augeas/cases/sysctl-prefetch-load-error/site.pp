# /etc/sysctl.conf does not parse with the Sysctl lens, so the prefetch fails.
sysctl { 'vm.swappiness': value => '20', apply => false }
sysctl { 'net.ipv4.ip_forward': value => '1', apply => false }
