mount { '/mnt/x': ensure => defined, device => 'tmpfs', fstype => 'tmpfs', options => 'defaults' }
