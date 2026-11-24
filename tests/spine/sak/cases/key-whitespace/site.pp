ssh_authorized_key { 'k1': user => 'alice', target => '/home/alice/.ssh/authorized_keys', type => 'ssh-ed25519', key => 'a b' }
notify { 'sibling': }
