file { '/mnt/p/c/f': content => "hi\n" }
mount { '/mnt/p/c': ensure => mounted, device => 'tmpfs', fstype => 'tmpfs', options => 'defaults', require => File['/mnt/p/c'] }
file { '/mnt/p/c': ensure => directory }
mount { '/mnt/p': ensure => mounted, device => 'tmpfs', fstype => 'tmpfs', options => 'defaults' }
