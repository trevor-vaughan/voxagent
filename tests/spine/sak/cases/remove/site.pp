ssh_authorized_key { 'alice@laptop': ensure => absent, user => 'alice' }
ssh_authorized_key { '/home/alice/.ssh/authorized_keys:unnamed-1': ensure => absent, user => 'alice' }
