ini_subsetting { 'create': path => '/etc/m.ini', section => 'a', setting => 'opts', subsetting => '-Xmx', value => '512m' }
ini_subsetting { 'change': path => '/etc/m.ini', section => 'a', setting => 'o2', subsetting => '-Xms', value => '1g' }
ini_subsetting { 'remove': ensure => absent, path => '/etc/m.ini', section => 'a', setting => 'o3', subsetting => '-Xss' }
