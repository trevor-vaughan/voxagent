ini_setting { 's': path => '/etc/m.ini', section => 'a', setting => 'k', value => 'v' }
ini_setting { 's2': ensure => absent, path => '/etc/m2.ini', section => 'a', setting => 'k' }
