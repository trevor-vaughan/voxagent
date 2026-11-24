mount { '/mnt/t/': ensure => defined, device => 'tmpfs', fstype => 'tmpfs', options => 'noatime' }
mount { '/mnt/s//': ensure => defined, device => 'tmpfs', fstype => 'tmpfs' }
mount { 'named': name => '/mnt/u/', ensure => defined, device => 'tmpfs', fstype => 'tmpfs' }
notify { 'ref': require => Mount['/mnt/t/'] }
