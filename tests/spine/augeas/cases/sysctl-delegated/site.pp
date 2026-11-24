# A comment is a shape the native sysctl does not implement, so voxagent sends
# the type to the extension host.
sysctl { 'vm.swappiness': value => '20', comment => 'gate' }
