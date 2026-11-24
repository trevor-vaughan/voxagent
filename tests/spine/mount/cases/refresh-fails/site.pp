mount { '/mnt/x': ensure => mounted, device => 'tmpfs', fstype => 'tmpfs', options => 'bogus' }
notify { 'after': require => Mount['/mnt/x'] }
