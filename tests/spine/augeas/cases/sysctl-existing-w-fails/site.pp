# kernel.hostname is in the file with another value and the shim fails its
# `-w`, so its edit stays pending in the shared handle; vm.swappiness, applied
# next, saves and writes it (I-36).
sysctl { 'kernel.hostname': value => 'zz' }
sysctl { 'vm.swappiness': value => '20' }
