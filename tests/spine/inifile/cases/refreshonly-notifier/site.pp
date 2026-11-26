ini_setting { 's': path => '/etc/m.ini', section => 'a', setting => 'k', value => 'new', refreshonly => true, notify => Exec['n'] }
ini_setting { 'chg': path => '/etc/m.ini', section => 'a', setting => 'k', value => 'v2', notify => Exec['n'] }
exec { 'n': command => '/bin/true', refreshonly => true }
