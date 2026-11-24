mount { '/mnt/x': ensure => mounted, device => 'tmpfs', fstype => 'tmpfs', options => 'defaults' }
