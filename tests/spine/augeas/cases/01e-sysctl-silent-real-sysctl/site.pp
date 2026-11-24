# real sysctl: every -w fails "permission denied"; foo.* do not exist
sysctl { 'net.ipv4.ip_forward': value => '1', silent => false }
sysctl { 'net.ipv4.conf.all.rp_filter': value => '1', silent => true }
sysctl { 'foo.bar': value => '1', silent => false }
sysctl { 'foo.qux': value => '1', silent => true }
sysctl { 'vm.swappiness': value => '20', apply => false }
