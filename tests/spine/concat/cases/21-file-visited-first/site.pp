# Declared Files related to their concat_file so Ruby's generate finds an edge already present.
file { '/etc/c21a.conf': ensure => file, content => "declared\n", before => Concat_file['/etc/c21a.conf'] }
concat_file { '/etc/c21a.conf': notify => Exec['c21a-refresh'] }
concat_fragment { 'fa': target => '/etc/c21a.conf', content => "from fragment\n" }
exec { 'c21a-refresh': command => '/bin/sh -c "echo r >> /etc/c21a.refreshed"', refreshonly => true }
file { '/etc/c21b.conf': ensure => file }
concat_file { '/etc/c21b.conf': require => File['/etc/c21b.conf'] }
concat_fragment { 'fb': target => '/etc/c21b.conf', content => "from fragment\n" }
file { '/etc/c21c.conf': ensure => file }
concat_file { '/etc/c21c.conf': before => File['/etc/c21c.conf'], notify => Exec['c21c-refresh'] }
concat_fragment { 'fc': target => '/etc/c21c.conf', content => "from fragment\n" }
exec { 'c21c-refresh': command => '/bin/sh -c "echo r >> /etc/c21c.refreshed"', refreshonly => true }
