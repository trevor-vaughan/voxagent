exec { 'trigger': command => '/usr/bin/true' }
mount { '/mnt/x': ensure => mounted, device => 'tmpfs', fstype => 'tmpfs', options => 'defaults', subscribe => Exec['trigger'] }
mount { '/mnt/y': ensure => unmounted, device => 'tmpfs', fstype => 'tmpfs', options => 'defaults', subscribe => Exec['trigger'] }
mount { '/mnt/z': ensure => mounted, device => 'tmpfs', fstype => 'swap', options => 'defaults', subscribe => Exec['trigger'] }
