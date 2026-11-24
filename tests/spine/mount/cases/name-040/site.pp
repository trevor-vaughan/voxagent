mount { '/mnt/p q': options => 'noatime' }
mount { '/mnt/n m': ensure => defined, device => 'x y', fstype => 'ext4' }
