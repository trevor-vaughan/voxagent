# foo.bar is in the file but is no kernel key; the real binary's `-e` drops it.
sysctl { 'vm.swappiness': value => '20', apply => false }
