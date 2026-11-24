# The target's directory does not exist, so the save fails; apply => false
# keeps `-w` out of it.
sysctl { 'vm.swappiness': value => '20', apply => false, target => '/etc/no-such-dir/sysctl.conf' }
