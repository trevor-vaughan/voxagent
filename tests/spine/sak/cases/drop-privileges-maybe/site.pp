ssh_authorized_key { 'k1': user => 'alice', target => '/home/alice/.ssh/authorized_keys', type => 'ssh-ed25519', key => 'AAAAC3NzaC1lZDI1NTE5AAAAIOMqqnkVzrm0SdG6UOoqKLsabgH5C9okWi0dh2l9GKJl', drop_privileges => 'maybe' }
notify { 'after': }
