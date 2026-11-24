sshkey { 'before.example@ssh-rsa': key => 'BBBB', target => '/home/alice/.ssh/authorized_keys' }
-> ssh_authorized_key { 'alice@desktop': user => 'alice', type => 'ssh-ed25519', key => 'AAAAC3NzaC1lZDI1NTE5AAAAIOMqqnkVzrm0SdG6UOoqKLsabgH5C9okWi0dh2l9GKJl' }
-> sshkey { 'after.example@ssh-ed25519': key => 'CCCC', target => '/home/alice/.ssh/authorized_keys' }
