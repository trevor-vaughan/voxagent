mount { '/mnt/t': ensure => defined, device => 'tmpfs', fstype => 'tmpfs', atboot => true }
mount { '/mnt/f': ensure => defined, device => 'tmpfs', fstype => 'tmpfs', atboot => false }
mount { '/mnt/n': ensure => defined, device => 'tmpfs', fstype => 'tmpfs' }
