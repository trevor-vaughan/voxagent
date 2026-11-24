mount { '/mnt/x': ensure => mounted, device => 'tmpfs', fstype => 'tmpfs', options => 'noatime', remounts => false }
