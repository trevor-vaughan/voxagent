ini_section { 'c': path => '/etc/m.ini', section => 'c', section_prefix => '<', section_suffix => '>' }
ini_section { 'r': ensure => absent, path => '/etc/m.ini', section => 'a' }
