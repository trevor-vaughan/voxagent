mount { '/mnt/x': ensure => unmounted, device => 'tmpfs', fstype => 'tmpfs', options => 'defaults' }
