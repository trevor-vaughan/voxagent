mount { '/mnt/r': ensure => defined, device => 'tmpfs', fstype => 'tmpfs', options => 'defaults' }
