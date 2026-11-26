ini_setting { 's': path => '/etc/m.ini', section => 'new' }
ini_setting { 's2': ensure => absent, path => '/etc/m2.ini', section => 'a' }
