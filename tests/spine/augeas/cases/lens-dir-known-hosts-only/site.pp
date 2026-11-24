# The lens directory holds only augeasproviders_ssh's known_hosts.aug, as
# merging that module's whole lib/ leaves it: the loadpath branch with no
# override of a stock lens.
sysctl { 'vm.swappiness': value => '20' }
ssh_config { 'Compression': host => '*', value => 'no' }
sshd_config { 'PermitRootLogin': value => 'no' }
