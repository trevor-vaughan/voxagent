ini_section { 's': path => '/etc/m.ini', section => 'a' }
ini_section { 's2': ensure => absent, path => '/etc/m.ini', section => 'zz' }
