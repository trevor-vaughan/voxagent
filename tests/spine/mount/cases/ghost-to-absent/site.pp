mount { '/mnt/x': ensure => absent, device => 'tmpfs', fstype => 'tmpfs', options => 'defaults' }
