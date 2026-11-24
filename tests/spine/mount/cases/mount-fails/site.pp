mount { '/mnt/nodir': ensure => mounted, device => 'tmpfs', fstype => 'tmpfs', options => 'defaults' }
