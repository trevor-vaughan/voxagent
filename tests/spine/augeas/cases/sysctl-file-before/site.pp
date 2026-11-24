# The Sysctls set no target, so nothing but the explicit `before` orders the
# File ahead of them, and their prefetch must read the File's content.
sysctl { 'vm.swappiness': value => '20' }
sysctl { 'net.ipv4.ip_forward': value => '1' }
file { '/etc/sysctl.conf':
  content => "vm.swappiness = 5\nnet.ipv4.ip_forward = 1\n",
  before  => [Sysctl['vm.swappiness'], Sysctl['net.ipv4.ip_forward']],
}
