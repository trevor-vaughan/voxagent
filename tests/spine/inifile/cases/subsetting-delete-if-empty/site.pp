ini_subsetting { 'd': ensure => absent, path => '/etc/m.ini', section => 'a', setting => 'o', subsetting => 'A', delete_if_empty => true }
ini_subsetting { 'n': ensure => absent, path => '/etc/m.ini', section => 'a', setting => 'p', subsetting => 'A' }
