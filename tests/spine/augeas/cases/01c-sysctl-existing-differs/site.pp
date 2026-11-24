# A: file 5, live 5, want 20  -> config and live change
# B: file 1, live 0, want 1   -> live only
# C: file 0, live 1, want 1   -> config only
sysctl { 'vm.swappiness': value => '20' }
sysctl { 'net.ipv4.ip_forward': value => '1' }
sysctl { 'net.ipv4.conf.all.rp_filter': value => 1 }
