concat_file { '/etc/c18exist.conf': ensure => absent }
concat_file { '/etc/c18none.conf': ensure => absent }
concat_file { '/etc/c18present.conf': ensure => present }
concat_fragment { 'f': target => '/etc/c18exist.conf', content => "x\n" }
concat_fragment { 'g': target => '/etc/c18present.conf', content => "x\n" }
