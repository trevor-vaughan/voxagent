ini_setting { 's': ensure => absent, path => '/etc/m.ini', section => 'a', setting => 'k' }
ini_setting { 's2': ensure => absent, path => '/etc/nofile.ini', section => 'a', setting => 'k' }
