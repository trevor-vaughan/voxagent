ini_subsetting { 'present': path => '/etc/m.ini', section => 'a', setting => 'o', subsetting => '-v', use_exact_match => true }
ini_subsetting { 'add': path => '/etc/m.ini', section => 'a', setting => 'o', subsetting => '-q', use_exact_match => true }
ini_subsetting { 'rm': ensure => absent, path => '/etc/m.ini', section => 'a', setting => 'o', subsetting => '-w', use_exact_match => true }
