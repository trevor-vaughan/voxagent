ini_setting { 'other': path => '/etc/o.ini', section => 'a', setting => 'k', value => 'v' , notify => Ini_setting['s'] }
ini_setting { 's': ensure => absent, path => '/etc/m.ini', section => 'a', setting => 'k', refreshonly => true }
