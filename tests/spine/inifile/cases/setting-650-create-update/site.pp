ini_setting { 'c': path => '/etc/m.ini', section => 'a', setting => 'k', value => 'v' }
ini_setting { 'u': path => '/etc/m.ini', section => 'a', setting => 'u', value => 'new' }
ini_setting { 'a': ensure => absent, path => '/etc/m.ini', section => 'a', setting => 'z' }
