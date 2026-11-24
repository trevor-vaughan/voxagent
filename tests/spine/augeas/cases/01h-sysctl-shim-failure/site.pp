# shim fails only kernel.hostname, so these show the non-silent / silent failure
# of a key that EXISTS (the live value cannot be set) with the rest succeeding
sysctl { 'kernel.hostname': value => 'zz', silent => false }
sysctl { 'kernel.domainname': value => 'zz.example', silent => true }
sysctl { 'vm.swappiness': value => '20' }
