mount { '/mnt/x': ensure => mounted, device => 'tmpfs', fstype => 'swap', options => 'defaults' }
