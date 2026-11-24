mount { '/mnt/f1': ensure => defined, device => 'tmpfs' }
mount { '/mnt/f2': ensure => defined, device => 'tmpfs', fstype => 'tmpfs', require => Mount['/mnt/f1'] }
mount { '/mnt/f3': ensure => defined, device => 'tmpfs', fstype => 'tmpfs', require => Mount['/mnt/f2'] }
